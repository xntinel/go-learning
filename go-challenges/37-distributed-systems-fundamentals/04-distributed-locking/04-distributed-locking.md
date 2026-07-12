# 4. Distributed Locking with Leases

Distributed locking is harder than it looks. A local `sync.Mutex` relies on the
kernel: if the goroutine holding the lock crashes, the process dies and the mutex
disappears. Across machines the crash is silent to other nodes. Lease-based locking
handles this by attaching an expiry time to every granted lock, so ownership lapses
automatically when the holder goes quiet. Fencing tokens then solve the remaining
problem: a slow holder that resumes after its lease has expired must not be allowed
to corrupt shared state.

This lesson builds a self-contained in-process lock service that demonstrates both
mechanisms. The same design is what etcd's `concurrency` package and Redis Redlock
implement at distributed scale.

```text
distlock/
  go.mod
  lock.go
  lock_test.go
  cmd/demo/main.go
```

## Concepts

### The Lease Model

A lease is a time-bounded grant of exclusive ownership. The service records, for
each named resource, who holds it and when ownership expires. Every Acquire call
must inspect whether any existing lock is still live before granting a new one.

```
state:
  locks: map[name] -> {owner, token, expiresAt}
  nextToken: monotonic counter (int64, starts at 1)

Acquire(name, owner, ttl):
  lock already held AND not expired? -> return 0, false
  otherwise: nextToken++; record owner+token+expiresAt; return token, true

Release(name, owner, token):
  held by this owner with this token AND not expired? -> delete entry; return true
  otherwise: return false

Renew(name, owner, token, ttl):
  held by this owner with this token AND not expired? -> extend expiresAt; return true
  otherwise: return false
```

Guard every mutation with a `sync.Mutex` — this service will be called from
multiple goroutines concurrently.

### Fencing Tokens

The token is the lease's safety net. Consider the sequence:

1. Client A acquires the lock, gets token 5.
2. Client A pauses (GC, preemption, slow network).
3. The lease expires. Client B acquires the lock, gets token 6.
4. Client A resumes and tries to write to the resource server.

Without fencing, A's write lands and corrupts state because B already owns it.
With fencing, the resource server rejects any write whose token is not strictly
greater than the last accepted token. A arrives with 5; the resource server has
already seen 6 and rejects it.

The resource server's rule is simple and stateful:

```
lastToken: int64 (starts at 0)

Execute(token, op):
  token <= lastToken? -> return false  (stale; reject)
  lastToken = token
  op()
  return true
```

The token must be monotonically increasing across the lifetime of the service, not
just per-client. A simple atomic counter on the lock service satisfies this.

### Lease Renewal

A holder that expects to hold the lock longer than a single TTL must renew before
expiry. The canonical pattern is to renew at TTL/3 intervals so two missed renewals
still leave time for one more attempt:

```
TTL = 30s
renew every 10s
first miss: 20s remain
second miss: 10s remain -> trigger alert
third miss: lock expires -> safe for reacquisition
```

Renewal is only valid while the original token is still live. Renewal cannot
extend a lock that has already expired and been re-granted to someone else.

### Failure Modes

- **Holder crash**: lease expires on schedule; another caller acquires. Safety is
  preserved. Liveness is reduced by one full TTL window.
- **Network partition**: if the holder is alive but unreachable, it should detect
  the partition and stop renewing. Systems with mandatory coordination (etcd, ZooKeeper)
  enforce this via session health checks.
- **Clock drift**: `time.Now()` on two machines may differ by seconds. In
  production, use a coordination service that provides a single authoritative clock
  rather than comparing wall clocks across hosts.
- **Thundering herd on expiry**: all waiting clients poll the service simultaneously
  when a lock expires. Mitigation: exponential back-off with jitter in callers.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/04-distributed-locking/04-distributed-locking/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/04-distributed-locking/04-distributed-locking
```

This is a library: there is no `main` in the package itself. Verify it with
`go test`.

### Exercise 1: The Lock Service

Create `lock.go`:

```go
package distlock

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrLockHeld is returned by Acquire when the named lock is held by another owner.
	ErrLockHeld = errors.New("lock is held by another owner")
	// ErrNotOwner is returned by Release or Renew when the caller is not the current owner.
	ErrNotOwner = errors.New("caller is not the lock owner")
	// ErrExpired is returned by Release or Renew when the lock has already expired.
	ErrExpired = errors.New("lock has expired")
	// ErrBadTTL is returned when a non-positive TTL is supplied.
	ErrBadTTL = errors.New("ttl must be positive")
)

// entry holds the state of one named lock.
type entry struct {
	owner     string
	token     int64
	expiresAt time.Time
}

// LockService is a thread-safe in-process distributed lock service.
// It simulates the server side of a distributed locking protocol:
// multiple goroutines (representing distributed clients) compete to
// acquire named locks with a finite lease duration.
type LockService struct {
	mu        sync.Mutex
	locks     map[string]*entry
	nextToken int64
}

// NewLockService creates a ready-to-use LockService.
func NewLockService() *LockService {
	return &LockService{
		locks:     make(map[string]*entry),
		nextToken: 0,
	}
}

// Acquire attempts to take the named lock for owner with the given TTL.
// On success it returns the fencing token (a monotonically increasing int64)
// and nil. On failure it returns 0 and a wrapped ErrLockHeld or ErrBadTTL.
func (ls *LockService) Acquire(name, owner string, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, fmt.Errorf("distlock: Acquire %q: %w", name, ErrBadTTL)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if e, ok := ls.locks[name]; ok && time.Now().Before(e.expiresAt) {
		return 0, fmt.Errorf("distlock: Acquire %q: %w (held by %q until %s)",
			name, ErrLockHeld, e.owner, e.expiresAt.Format(time.RFC3339))
	}
	ls.nextToken++
	ls.locks[name] = &entry{
		owner:     owner,
		token:     ls.nextToken,
		expiresAt: time.Now().Add(ttl),
	}
	return ls.nextToken, nil
}

// Release relinquishes the named lock.
// The caller must supply the token that was returned by Acquire.
// Returns ErrNotOwner if the owner or token does not match, ErrExpired if the
// lease has already lapsed.
func (ls *LockService) Release(name, owner string, token int64) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	e, ok := ls.locks[name]
	if !ok {
		return fmt.Errorf("distlock: Release %q: %w", name, ErrExpired)
	}
	if time.Now().After(e.expiresAt) {
		delete(ls.locks, name)
		return fmt.Errorf("distlock: Release %q: %w", name, ErrExpired)
	}
	if e.owner != owner || e.token != token {
		return fmt.Errorf("distlock: Release %q: %w", name, ErrNotOwner)
	}
	delete(ls.locks, name)
	return nil
}

// Renew extends the lease by ttl from now, provided the caller still holds the lock.
// Returns ErrNotOwner or ErrExpired on failure.
func (ls *LockService) Renew(name, owner string, token int64, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("distlock: Renew %q: %w", name, ErrBadTTL)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()

	e, ok := ls.locks[name]
	if !ok {
		return fmt.Errorf("distlock: Renew %q: %w", name, ErrExpired)
	}
	if time.Now().After(e.expiresAt) {
		delete(ls.locks, name)
		return fmt.Errorf("distlock: Renew %q: %w", name, ErrExpired)
	}
	if e.owner != owner || e.token != token {
		return fmt.Errorf("distlock: Renew %q: %w", name, ErrNotOwner)
	}
	e.expiresAt = time.Now().Add(ttl)
	return nil
}

// ResourceServer enforces the fencing-token protocol on the consumer side.
// It remembers the highest token it has executed and rejects older tokens.
type ResourceServer struct {
	mu        sync.Mutex
	lastToken int64
	log       []string
}

// NewResourceServer creates a ResourceServer ready to accept operations.
func NewResourceServer() *ResourceServer {
	return &ResourceServer{}
}

// Execute runs op if token is strictly greater than the last accepted token.
// Returns nil on success, a descriptive error if the token is stale.
func (rs *ResourceServer) Execute(token int64, op func()) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if token <= rs.lastToken {
		return fmt.Errorf("distlock: Execute: stale token %d (last accepted %d)",
			token, rs.lastToken)
	}
	rs.lastToken = token
	op()
	return nil
}

// LastToken returns the highest fencing token accepted so far.
func (rs *ResourceServer) LastToken() int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.lastToken
}
```

The `LockService` exposes error variables wrapped with `%w` so callers can use
`errors.Is`. The `ResourceServer` is separate because in real systems the resource
owner is a different process from the lock service.

### Exercise 2: Tests

Create `lock_test.go`:

```go
package distlock

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAcquireBasic(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	token, err := svc.Acquire("res", "alice", time.Second)
	if err != nil {
		t.Fatalf("Acquire: unexpected error: %v", err)
	}
	if token != 1 {
		t.Fatalf("first token = %d, want 1", token)
	}
}

func TestAcquireBlockedWhileHeld(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	if _, err := svc.Acquire("res", "alice", time.Second); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_, err := svc.Acquire("res", "bob", time.Second)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second Acquire err = %v, want ErrLockHeld", err)
	}
}

func TestAcquireAfterExpiry(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	if _, err := svc.Acquire("res", "alice", time.Millisecond); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	token, err := svc.Acquire("res", "bob", time.Second)
	if err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
	if token != 2 {
		t.Fatalf("token after re-acquire = %d, want 2", token)
	}
}

func TestReleaseHappyPath(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	tok, _ := svc.Acquire("res", "alice", time.Second)
	if err := svc.Release("res", "alice", tok); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Should be acquirable again immediately.
	if _, err := svc.Acquire("res", "bob", time.Second); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
}

func TestReleaseWrongOwner(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	tok, _ := svc.Acquire("res", "alice", time.Second)
	err := svc.Release("res", "eve", tok)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Release with wrong owner: err = %v, want ErrNotOwner", err)
	}
}

func TestReleaseExpiredLock(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	tok, _ := svc.Acquire("res", "alice", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	err := svc.Release("res", "alice", tok)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("Release expired lock: err = %v, want ErrExpired", err)
	}
}

func TestRenewExtendsTTL(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	tok, _ := svc.Acquire("res", "alice", 20*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if err := svc.Renew("res", "alice", tok, 200*time.Millisecond); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	time.Sleep(30 * time.Millisecond) // would have expired without renewal
	_, err := svc.Acquire("res", "bob", time.Second)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("lock should still be held after renewal; Acquire err = %v", err)
	}
}

func TestRenewAfterExpiry(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	tok, _ := svc.Acquire("res", "alice", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	err := svc.Renew("res", "alice", tok, time.Second)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("Renew expired lock: err = %v, want ErrExpired", err)
	}
}

func TestTokenIsMonotonicallyIncreasing(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	var prev int64
	for i := 0; i < 5; i++ {
		tok, err := svc.Acquire("r", "owner", time.Millisecond)
		if err != nil {
			t.Fatalf("Acquire iter %d: %v", i, err)
		}
		if tok <= prev {
			t.Fatalf("token %d is not > previous token %d", tok, prev)
		}
		prev = tok
		time.Sleep(5 * time.Millisecond) // let lease expire before re-acquire
	}
}

func TestFencingTokenRejectsStale(t *testing.T) {
	t.Parallel()

	svc := NewLockService()
	rs := NewResourceServer()

	// Client A acquires token 1.
	tokA, _ := svc.Acquire("res", "A", time.Millisecond)
	// Lease expires; client B acquires token 2.
	time.Sleep(5 * time.Millisecond)
	tokB, _ := svc.Acquire("res", "B", time.Second)

	// B executes first with the newer token.
	if err := rs.Execute(tokB, func() {}); err != nil {
		t.Fatalf("B Execute: %v", err)
	}
	// A tries to execute with the stale token — must be rejected.
	err := rs.Execute(tokA, func() {})
	if err == nil {
		t.Fatal("stale token was accepted; want rejection")
	}
}

func TestConcurrentContention(t *testing.T) {
	t.Parallel()

	const n = 20
	svc := NewLockService()
	wins := make([]int64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			tok, err := svc.Acquire("shared", "client", 5*time.Second)
			if err == nil {
				wins[i] = tok
				_ = svc.Release("shared", "client", tok)
			}
		}()
	}
	wg.Wait()

	winners := 0
	for _, tok := range wins {
		if tok > 0 {
			winners++
		}
	}
	// Each release opens the lock for the next; at minimum one goroutine wins.
	if winners < 1 {
		t.Fatalf("expected at least one winner, got %d", winners)
	}
}

// ExampleLockService_Acquire shows a basic acquire-use-release cycle.
func ExampleLockService_Acquire() {
	svc := NewLockService()
	tok, err := svc.Acquire("db-migration", "worker-1", time.Minute)
	if err != nil {
		// Lock is held by another worker.
		return
	}
	// ... do critical work ...
	_ = svc.Release("db-migration", "worker-1", tok)
	// Output:
}
```

Your turn: add `TestAcquireBadTTL` that calls `svc.Acquire("r", "x", 0)` and
asserts `errors.Is(err, ErrBadTTL)`.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/distlock"
)

func main() {
	svc := distlock.NewLockService()
	rs := distlock.NewResourceServer()

	// Scenario 1: basic acquire and release.
	tok1, err := svc.Acquire("resource", "client-A", 500*time.Millisecond)
	if err != nil {
		fmt.Println("unexpected error:", err)
		return
	}
	fmt.Printf("client-A acquired lock, fencing token = %d\n", tok1)

	// client-B cannot acquire while A holds it.
	_, err = svc.Acquire("resource", "client-B", 500*time.Millisecond)
	fmt.Println("client-B attempt while A holds:", err)

	// A finishes; release.
	_ = svc.Release("resource", "client-A", tok1)
	fmt.Println("client-A released")

	// Scenario 2: fencing tokens reject a stale operation.
	// A acquires with a very short TTL.
	tokA, _ := svc.Acquire("resource", "client-A", 50*time.Millisecond)
	fmt.Printf("client-A acquired again, token = %d\n", tokA)

	// Simulate A pausing; lease expires.
	time.Sleep(100 * time.Millisecond)

	// B acquires; gets a higher token.
	tokB, _ := svc.Acquire("resource", "client-B", 5*time.Second)
	fmt.Printf("client-B acquired, token = %d\n", tokB)

	// B writes to the resource server first (accepted).
	if err := rs.Execute(tokB, func() {
		fmt.Println("client-B: operation accepted")
	}); err != nil {
		fmt.Println("client-B rejected:", err)
	}

	// A resumes and tries to write with its stale token (rejected).
	if err := rs.Execute(tokA, func() {
		fmt.Println("client-A: operation accepted (WRONG: should not happen)")
	}); err != nil {
		fmt.Printf("client-A stale token %d rejected: %v\n", tokA, err)
	}

	// Scenario 3: renewal keeps the lock alive.
	tokR, _ := svc.Acquire("heartbeat", "worker", 50*time.Millisecond)
	fmt.Printf("worker acquired heartbeat lock, token = %d\n", tokR)
	time.Sleep(30 * time.Millisecond)
	if err := svc.Renew("heartbeat", "worker", tokR, 500*time.Millisecond); err != nil {
		fmt.Println("renew failed:", err)
	} else {
		fmt.Println("worker renewed the lease")
	}
	_ = svc.Release("heartbeat", "worker", tokR)
	fmt.Println("worker released after successful renewal")
	fmt.Printf("resource server last accepted token: %d\n", rs.LastToken())
}
```

Run it:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Releasing with a mismatched token

Wrong: the caller stores the owner name but discards the fencing token, then passes
0 or a wrong value to Release.

```go
// Wrong
tok, _ := svc.Acquire("r", "me", time.Second)
_ = tok // discarded
svc.Release("r", "me", 0) // ErrNotOwner — token 0 != token 1
```

Fix: keep the token in the same variable scope as the Acquire call. Pass it intact
to both Release and Renew.

```go
// Fix
tok, err := svc.Acquire("r", "me", time.Second)
if err != nil { /* handle */ }
defer svc.Release("r", "me", tok)
```

### Trusting Release when the lease may have expired

Wrong: checking only the Release error to decide whether the critical section
succeeded.

```go
// Wrong
tok, _ := svc.Acquire("r", "me", ttl)
// ... work that might take longer than ttl ...
if err := svc.Release("r", "me", tok); err != nil {
	log.Println("release failed") // too late: another owner may have acted
}
```

Fix: treat an expired lease as a potential data race. Log a warning and trigger
reconciliation before assuming the critical section's side effects are valid.

### Skipping the resource-server token check

Wrong: the application performs the fencing-token dance at the lock service but
never propagates the token to the actual storage operation.

```go
// Wrong: token acquired, operation executed without passing token to storage
tok, _ := svc.Acquire("db", "writer", ttl)
db.Write(data) // no token check — a stale writer can still corrupt state
svc.Release("db", "writer", tok)
```

Fix: the fencing token must travel with the write request all the way to the
resource owner. The resource owner enforces the monotonicity check.

### Setting TTL to zero or negative

Wrong: passing `0` as the TTL because it "means no expiry".

```go
// Wrong
svc.Acquire("r", "me", 0) // returns ErrBadTTL; does not grant an infinite lease
```

Fix: choose a realistic TTL based on the expected critical-section duration plus
a safety margin. Use Renew for operations that legitimately need longer.

## Verification

From `~/go-exercises/distlock`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must produce no output (or exit 0). `go test` is the verification
— there is no program output to eyeball.

## Summary

- Leases (time-bounded grants) handle holder crashes without manual cleanup.
- The fencing token is a monotonically increasing integer returned by Acquire; it
  lets the resource owner reject operations from stale holders.
- Renewal extends the lease while the holder is still active; it cannot revive an
  already-expired lock.
- Concurrent safety requires a `sync.Mutex` around all map operations.
- In production, use etcd or ZooKeeper to replace wall-clock expiry with
  consensus-backed session management and to eliminate single-point-of-failure.

## What's Next

[Vector Clocks and Causality](../05-vector-clocks/05-vector-clocks.md).

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) — the formal rules for `sync.Mutex` guarantees that underpin this lesson.
- [sync package documentation](https://pkg.go.dev/sync) — `sync.Mutex`, `sync.WaitGroup` signatures and semantics.
- [How to Do Distributed Locking — Martin Kleppmann](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) — the original fencing-token article; basis for the Concepts section.
- [etcd concurrency package](https://pkg.go.dev/go.etcd.io/etcd/client/v3/concurrency) — production implementation of lease-based locking in Go.
- [Designing Data-Intensive Applications, Chapter 8](https://dataintensive.net/) — clock drift, fencing tokens, and the limits of lease-based locking in wide-area systems.
