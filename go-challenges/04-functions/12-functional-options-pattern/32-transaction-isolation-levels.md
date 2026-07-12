# Exercise 32: Transaction Manager With ACID Isolation Level and Conflict Resolution

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Serializable isolation promises transactions behave as if they ran one at a
time, in some order — and retrying a transaction that lost a deadlock is
exactly the kind of thing that can quietly break that promise, because the
retry sees a database state a purely serial schedule never would have
produced. This module builds a lock manager with functional options for
isolation level and conflict resolution, and it refuses to let serializable
isolation combine with deadlock retries at all.

## What you'll build

```text
txmanager/                        independent module: example.com/transaction-isolation-levels
  go.mod                          go 1.24
  txmanager.go                    IsolationLevel, ConflictResolver, Manager, Option, New,
                                   WithIsolationLevel, WithConflictResolver,
                                   WithMaxDeadlockRetries, Acquire, AcquireWithRetry,
                                   Release, Holder
  cmd/
    demo/
      main.go                     a priority conflict, an exhausted retry, then a release
  txmanager_test.go                 option-validation table, resolver, retry, and -race tests
```

- Files: `txmanager.go`, `cmd/demo/main.go`, `txmanager_test.go`.
- Implement: a `Manager` built by `New(opts ...Option) (*Manager, error)` whose `Acquire` grants an exclusive per-resource lock or resolves contention through a pluggable `ConflictResolver`, validating that serializable isolation is never combined with a nonzero deadlock-retry count.
- Test: every option-validation case including the exact boundary of zero retries under serializable isolation, both resolver outcomes, retry exhaustion, a retry that succeeds once the resolver's answer changes, release ownership, and a `-race` concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why serializable isolation forbids deadlock retries

`WithIsolationLevel` and `WithMaxDeadlockRetries` are independent options —
either can be set without the other, in any order. Serializable isolation's
guarantee is that the observed outcome is equivalent to *some* serial
execution of every transaction; retrying a transaction after it loses a
conflict changes what that transaction sees on its second attempt, which
can produce an outcome no serial ordering of the *original* transactions
would have produced. Weaker isolation levels tolerate that because they
never promised full serializability in the first place, but serializable
isolation cannot: this module treats "serializable plus retries" as a
configuration error rather than a runtime risk, catching it once, in `New`,
after every option has applied — the same constructor-boundary pattern used
for every cross-field invariant in this chapter.

### A pluggable resolver, not a fixed policy

`Acquire` never decides who wins a conflict itself — it always defers to
the configured `ConflictResolver`, a two-argument function that takes the
current holder and the requester and returns whichever one should keep the
lock. The default, `holderWins`, models a simple non-preemptive lock
manager; the demo's `priorityResolver` models a fixed priority order
instead. Neither `Acquire` nor `AcquireWithRetry` needs to change to support
a different conflict policy — only the injected function does, which is the
same value functional options bring to every pluggable-behavior exercise in
this chapter.

### Retrying is just calling Acquire again

`AcquireWithRetry` adds no new locking logic of its own — it calls `Acquire`
in a loop, up to `1 + maxDeadlockRetries` times, and returns the moment one
attempt succeeds. Whether a retry can ever succeed depends entirely on
whether the resolver's answer — or the lock's holder — has changed between
attempts; `TestAcquireWithRetryExhaustsConfiguredAttempts` proves retries
run out when nothing changes, and `TestAcquireWithRetrySucceedsWhenResolverFlips`
proves a retry succeeds the instant the resolver's decision does.

Create `txmanager.go`:

```go
package txmanager

import (
	"fmt"
	"sync"
)

// IsolationLevel is one of the four standard ACID isolation levels.
type IsolationLevel string

const (
	ReadUncommitted IsolationLevel = "read-uncommitted"
	ReadCommitted   IsolationLevel = "read-committed"
	RepeatableRead  IsolationLevel = "repeatable-read"
	Serializable    IsolationLevel = "serializable"
)

var validIsolationLevels = map[IsolationLevel]bool{
	ReadUncommitted: true,
	ReadCommitted:   true,
	RepeatableRead:  true,
	Serializable:    true,
}

// ConflictResolver decides which transaction keeps a resource's lock when
// two transactions contend for it, returning the winning transaction ID
// (either holder or requester).
type ConflictResolver func(holder, requester string) string

// holderWins is the default resolver: whichever transaction already holds
// the lock keeps it.
func holderWins(holder, requester string) string { return holder }

// Manager grants exclusive per-resource locks to transactions, resolving
// contention with a pluggable ConflictResolver and rejecting configurations
// where serializable isolation is combined with deadlock retries.
type Manager struct {
	isolation          IsolationLevel
	resolver           ConflictResolver
	maxDeadlockRetries int

	mu    sync.Mutex
	locks map[string]string // resource -> holding transaction ID
}

// Option configures a Manager and may reject invalid input.
type Option func(*Manager) error

// New builds a Manager, seeding read-committed isolation, a holder-wins
// resolver, and 3 deadlock retries, then applies opts. It is the single
// validation boundary for the cross-field rule no single option can see on
// its own: serializable isolation must never be combined with deadlock
// retries, because retrying after an abort under serializable isolation can
// itself produce an interleaving the isolation level is meant to forbid.
func New(opts ...Option) (*Manager, error) {
	m := &Manager{
		isolation:          ReadCommitted,
		resolver:           holderWins,
		maxDeadlockRetries: 3,
		locks:              make(map[string]string),
	}
	for _, opt := range opts {
		if err := opt(m); err != nil {
			return nil, err
		}
	}

	if m.isolation == Serializable && m.maxDeadlockRetries > 0 {
		return nil, fmt.Errorf("serializable isolation does not allow deadlock retries, got %d", m.maxDeadlockRetries)
	}
	return m, nil
}

// WithIsolationLevel sets the transaction isolation level.
func WithIsolationLevel(level IsolationLevel) Option {
	return func(m *Manager) error {
		if !validIsolationLevels[level] {
			return fmt.Errorf("unsupported isolation level: %q", level)
		}
		m.isolation = level
		return nil
	}
}

// WithConflictResolver replaces the resolver used to decide which
// transaction keeps a contended resource.
func WithConflictResolver(fn ConflictResolver) Option {
	return func(m *Manager) error {
		if fn == nil {
			return fmt.Errorf("conflict resolver is nil")
		}
		m.resolver = fn
		return nil
	}
}

// WithMaxDeadlockRetries sets how many times AcquireWithRetry will retry a
// contended acquisition (>= 0).
func WithMaxDeadlockRetries(n int) Option {
	return func(m *Manager) error {
		if n < 0 {
			return fmt.Errorf("max deadlock retries must not be negative, got %d", n)
		}
		m.maxDeadlockRetries = n
		return nil
	}
}

// Acquire attempts to grant resource's exclusive lock to txID. If the
// resource is free or already held by txID, it grants immediately. If it is
// held by another transaction, the configured resolver decides the winner;
// Acquire grants the lock only if txID wins, and returns an error
// otherwise.
func (m *Manager) Acquire(txID, resource string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	holder, held := m.locks[resource]
	if !held || holder == txID {
		m.locks[resource] = txID
		return nil
	}

	if m.resolver(holder, txID) == txID {
		m.locks[resource] = txID
		return nil
	}
	return fmt.Errorf("resource %q held by tx %q, conflict resolved against tx %q", resource, holder, txID)
}

// AcquireWithRetry calls Acquire up to 1+maxDeadlockRetries times, returning
// as soon as one attempt succeeds. It returns the number of attempts made
// and the last error if every attempt failed.
func (m *Manager) AcquireWithRetry(txID, resource string) (attempts int, err error) {
	maxAttempts := m.maxDeadlockRetries + 1
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		if err = m.Acquire(txID, resource); err == nil {
			return attempts, nil
		}
	}
	return attempts - 1, err
}

// Release drops txID's lock on resource. It is an error to release a lock
// txID does not hold.
func (m *Manager) Release(txID, resource string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	holder, held := m.locks[resource]
	if !held || holder != txID {
		return fmt.Errorf("tx %q does not hold resource %q", txID, resource)
	}
	delete(m.locks, resource)
	return nil
}

// Holder reports which transaction, if any, currently holds resource.
func (m *Manager) Holder(resource string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	holder, held := m.locks[resource]
	return holder, held
}
```

### The runnable demo

The demo has `tx-a` acquire a resource, has `tx-b` lose the conflict to
`tx-a`'s priority, exhausts every configured retry with the same losing
outcome, then releases `tx-a`'s lock and shows `tx-b` acquiring it on the
very next attempt.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/transaction-isolation-levels"
)

// priorityResolver favors whichever transaction ID sorts first
// lexicographically, simulating a fixed priority order.
func priorityResolver(holder, requester string) string {
	if holder < requester {
		return holder
	}
	return requester
}

func main() {
	m, err := txmanager.New(
		txmanager.WithIsolationLevel(txmanager.RepeatableRead),
		txmanager.WithConflictResolver(priorityResolver),
		txmanager.WithMaxDeadlockRetries(2),
	)
	if err != nil {
		panic(err)
	}

	if err := m.Acquire("tx-a", "account:1"); err != nil {
		panic(err)
	}
	holder, _ := m.Holder("account:1")
	fmt.Printf("account:1 holder: %s\n", holder)

	err = m.Acquire("tx-b", "account:1")
	fmt.Printf("tx-b acquire account:1 (tx-a has priority): %v\n", err)

	attempts, err := m.AcquireWithRetry("tx-b", "account:1")
	fmt.Printf("tx-b retried %d times, still held by higher-priority tx: %v\n", attempts, err)

	if err := m.Release("tx-a", "account:1"); err != nil {
		panic(err)
	}
	attempts, err = m.AcquireWithRetry("tx-b", "account:1")
	fmt.Printf("tx-b acquired after tx-a released, attempts=%d, err=%v\n", attempts, err)

	_, err = txmanager.New(
		txmanager.WithIsolationLevel(txmanager.Serializable),
		txmanager.WithMaxDeadlockRetries(1),
	)
	fmt.Printf("serializable with retries rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
account:1 holder: tx-a
tx-b acquire account:1 (tx-a has priority): resource "account:1" held by tx "tx-a", conflict resolved against tx "tx-b"
tx-b retried 3 times, still held by higher-priority tx: resource "account:1" held by tx "tx-a", conflict resolved against tx "tx-b"
tx-b acquired after tx-a released, attempts=1, err=<nil>
serializable with retries rejected: true
```

### Tests

`TestNewValidation` tables construction failures, including the exact
boundary where serializable isolation is paired with zero retries (allowed)
versus any positive count (rejected). `TestAcquireGrantsFreeResource` and
the two resolver-outcome tests cover the base locking behavior in both
directions. `TestAcquireWithRetryExhaustsConfiguredAttempts` proves the
attempt count matches `1 + maxDeadlockRetries` exactly.
`TestAcquireWithRetrySucceedsWhenResolverFlips` proves a retry can still
succeed once conditions change between attempts. `TestReleaseRejectsNonHolder`
guards lock ownership. `TestConcurrentAcquireOnDistinctResourcesAllSucceed`
and `TestConcurrentAcquireOnSameResourceHasExactlyOneWinner` run under
`-race`, the second asserting the invariant that survives concurrent
contention (exactly one holder) rather than which specific goroutine won.

Create `txmanager_test.go`:

```go
package txmanager

import (
	"sync"
	"testing"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "unsupported isolation level", opts: []Option{WithIsolationLevel("snapshot")}, wantErr: true},
		{name: "nil conflict resolver", opts: []Option{WithConflictResolver(nil)}, wantErr: true},
		{name: "negative retries", opts: []Option{WithMaxDeadlockRetries(-1)}, wantErr: true},
		{
			name:    "serializable with retries",
			opts:    []Option{WithIsolationLevel(Serializable), WithMaxDeadlockRetries(1)},
			wantErr: true,
		},
		{
			name: "serializable with zero retries is allowed",
			opts: []Option{WithIsolationLevel(Serializable), WithMaxDeadlockRetries(0)},
		},
		{
			name: "non-serializable with retries is allowed",
			opts: []Option{WithIsolationLevel(RepeatableRead), WithMaxDeadlockRetries(5)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAcquireGrantsFreeResource(t *testing.T) {
	t.Parallel()

	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-a", "r1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	holder, ok := m.Holder("r1")
	if !ok || holder != "tx-a" {
		t.Fatalf("Holder(r1) = (%q, %t), want (tx-a, true)", holder, ok)
	}
}

func TestAcquireResolvesConflictAgainstLoser(t *testing.T) {
	t.Parallel()

	m, err := New(WithConflictResolver(func(holder, requester string) string { return holder }))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-a", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-b", "r1"); err == nil {
		t.Fatal("expected an error: the resolver always favors the holder")
	}
	holder, _ := m.Holder("r1")
	if holder != "tx-a" {
		t.Fatalf("Holder(r1) = %q, want tx-a (lock must not move to the loser)", holder)
	}
}

func TestAcquireResolvesConflictInFavorOfRequester(t *testing.T) {
	t.Parallel()

	m, err := New(WithConflictResolver(func(holder, requester string) string { return requester }))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-a", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-b", "r1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	holder, _ := m.Holder("r1")
	if holder != "tx-b" {
		t.Fatalf("Holder(r1) = %q, want tx-b (the resolver always favors the requester)", holder)
	}
}

func TestAcquireWithRetryExhaustsConfiguredAttempts(t *testing.T) {
	t.Parallel()

	m, err := New(
		WithConflictResolver(func(holder, requester string) string { return holder }),
		WithMaxDeadlockRetries(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-a", "r1"); err != nil {
		t.Fatal(err)
	}

	attempts, err := m.AcquireWithRetry("tx-b", "r1")
	if err == nil {
		t.Fatal("expected an error: the resolver never favors tx-b")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (1 initial + 2 retries)", attempts)
	}
}

func TestAcquireWithRetrySucceedsWhenResolverFlips(t *testing.T) {
	t.Parallel()

	var calls int
	resolver := func(holder, requester string) string {
		calls++
		if calls >= 2 {
			return requester
		}
		return holder
	}

	m, err := New(WithConflictResolver(resolver), WithMaxDeadlockRetries(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-a", "r1"); err != nil {
		t.Fatal(err)
	}

	attempts, err := m.AcquireWithRetry("tx-b", "r1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestReleaseRejectsNonHolder(t *testing.T) {
	t.Parallel()

	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("tx-a", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("tx-b", "r1"); err == nil {
		t.Fatal("expected an error: tx-b does not hold r1")
	}
	if err := m.Release("tx-a", "r1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := m.Holder("r1"); ok {
		t.Fatal("r1 should have no holder after release")
	}
}

func TestConcurrentAcquireOnDistinctResourcesAllSucceed(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.Acquire("tx", resourceName(i))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Acquire(resource %d) unexpected error: %v", i, err)
		}
	}
}

func TestConcurrentAcquireOnSameResourceHasExactlyOneWinner(t *testing.T) {
	m, err := New(WithConflictResolver(func(holder, requester string) string { return requester }))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.Acquire("tx", "shared")
		}(i)
	}
	wg.Wait()

	holder, ok := m.Holder("shared")
	if !ok {
		t.Fatal("expected exactly one transaction to hold the shared resource")
	}
	if holder != "tx" {
		t.Fatalf("Holder(shared) = %q, want tx", holder)
	}
}

func resourceName(i int) string {
	return "resource-" + string(rune('a'+i))
}
```

## Review

The lock manager is correct when a conflict is always resolved through the
one pluggable decision point (`ConflictResolver`), never through ad hoc
logic scattered across `Acquire` and `AcquireWithRetry`, and when the
combination this module refuses to allow at all — serializable isolation
plus retries — is caught at construction rather than left to manifest as a
subtle correctness bug much later, under load, in production. The
`-race`-checked concurrency tests intentionally assert an invariant
(exactly one resource, exactly one holder) rather than a specific winner,
because which goroutine's `Acquire` call reaches the mutex first is
legitimately nondeterministic — what must never be nondeterministic is
whether the lock map itself stays consistent under contention.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [PostgreSQL: transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html)
- [Jepsen: a friendly introduction to serializability](https://jepsen.io/consistency/models/serializable)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-feature-flag-evaluator-context.md](31-feature-flag-evaluator-context.md) | Next: [33-api-request-quota-manager.md](33-api-request-quota-manager.md)
