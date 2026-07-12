# Exercise 3: Add Monotonic Fencing Tokens to a Redis Lock

Redlock guarantees mutual exclusion only under bounded pauses, drift, and network
delay. A long GC pause can leave holder A believing it still holds a lock that
already expired and was handed to B — and redsync cannot stop A from acting. This
exercise builds the correctness layer redsync does not give you: a monotonic
fencing token minted at acquire time and enforced by the guarded resource, so a
stale holder's late write becomes a harmless no-op.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fencing/                   independent module: example.com/fencing
  go.mod                   go 1.26; requires redsync, go-redis, miniredis
  fence.go                 Locker, Acquire (INCR token via WithGenValueFunc), Held; FencedResource; ErrStaleToken
  cmd/
    demo/
      main.go              embedded miniredis; A writes, lease expires, B takes over, A's late write rejected
  fence_test.go            miniredis tests: monotonic tokens, resource rejects stale, stale holder cannot overwrite
```

Files: `fence.go`, `cmd/demo/main.go`, `fence_test.go`.
Implement: a `Locker` whose `Acquire` mints a strictly increasing token with Redis `INCR` (wired via `WithGenValueFunc` so `Mutex.Value()` equals the token) and returns a `Held` carrying that token; a `FencedResource` that tracks the highest token served and rejects any write with a token `<=` it via `ErrStaleToken`.
Test: with `miniredis`, assert two sequential acquisitions mint strictly increasing tokens and `Value()` matches; that the resource accepts the current token then rejects an older one; and that a stale holder that resumed after its lease expired cannot overwrite the newer holder's state.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get github.com/go-redsync/redsync/v4@latest
go get github.com/redis/go-redis/v9@latest
go get github.com/alicebob/miniredis/v2@latest
```

### Why Value() is not a fencing token

redsync mints a random value per acquisition to make Unlock and Extend safe:
the Lua scripts check the stored value equals this client's value before acting,
so no client can release or renew a lock a different owner now holds. That value
is *unique* but not *ordered*: given two random base64 strings there is no way to
say which holder is newer. Fencing needs exactly that ordering. So instead of
letting redsync generate a random value, `WithGenValueFunc` supplies one that is
the result of Redis `INCR` on a per-lock counter key. `INCR` is atomic and
monotonic, so every acquisition — even ones that overlap in wall-clock time
because a lease expired — gets a strictly larger number than the last. After
`LockContext` succeeds, `Mutex.Value()` returns that number as a string; parse it
back with `strconv.ParseInt`.

The token alone changes nothing until the *resource* enforces it. `FencedResource`
records the highest token it has ever accepted and rejects any write carrying a
token less than or equal to that high-water mark. Now the dangerous sequence is
defanged: A acquires token 1 and writes; A pauses; A's lease expires; B acquires
token 2 and writes; A wakes up still believing it holds the lock and tries to
write with token 1; the resource sees `1 <= 2` and returns `ErrStaleToken`. A's
belief about the lock is irrelevant — the token ordering, not the lock, is what
guarantees correctness. This is the difference between "the lock as an efficiency
optimization" and "correctness that survives a pause".

Create `fence.go`:

```go
package fencing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-redsync/redsync/v4"
	goredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"
	goredislib "github.com/redis/go-redis/v9"
)

// ErrStaleToken is returned by a fenced resource when a write carries a token
// that is not strictly greater than the highest token already served.
var ErrStaleToken = errors.New("fencing: stale token rejected")

// Locker mints monotonic fencing tokens with Redis INCR and wires each token as
// the mutex value, so Mutex.Value() returns the token that guards the resource.
type Locker struct {
	rs     *redsync.Redsync
	client goredislib.UniversalClient
	expiry time.Duration
}

// NewLocker builds a Locker over a go-redis client with the given lease TTL.
func NewLocker(client goredislib.UniversalClient, expiry time.Duration) *Locker {
	return &Locker{
		rs:     redsync.New(goredis.NewPool(client)),
		client: client,
		expiry: expiry,
	}
}

// Held is an acquired lock carrying its monotonic fencing token.
type Held struct {
	mu    *redsync.Mutex
	token int64
}

// Token returns the monotonic fencing token minted for this acquisition.
func (h *Held) Token() int64 { return h.token }

// Value returns the mutex value; for a fenced lock it is the token as a string.
func (h *Held) Value() string { return h.mu.Value() }

// Unlock best-effort releases the lock. A false result is legitimate when the
// lease already expired or a newer holder superseded it.
func (h *Held) Unlock(ctx context.Context) (bool, error) { return h.mu.UnlockContext(ctx) }

// Acquire locks name and mints a strictly increasing fencing token via INCR,
// wiring it as the mutex value so Mutex.Value() equals the token.
func (l *Locker) Acquire(ctx context.Context, name string) (*Held, error) {
	tokenKey := "fence:" + name
	var token int64
	m := l.rs.NewMutex(name,
		redsync.WithExpiry(l.expiry),
		redsync.WithTries(1),
		redsync.WithGenValueFunc(func() (string, error) {
			n, err := l.client.Incr(ctx, tokenKey).Result()
			if err != nil {
				return "", err
			}
			token = n
			return strconv.FormatInt(n, 10), nil
		}),
	)
	if err := m.LockContext(ctx); err != nil {
		return nil, fmt.Errorf("fencing: acquire %q: %w", name, err)
	}
	return &Held{mu: m, token: token}, nil
}

// FencedResource is a guarded resource that accepts writes only in fencing-token
// order. It rejects any write whose token is <= the highest token it has served,
// so a stale holder that resumed after its lease expired cannot corrupt state.
type FencedResource struct {
	mu      sync.Mutex
	highest int64
	state   string
}

// NewFencedResource returns an empty fenced resource (high-water mark 0).
func NewFencedResource() *FencedResource { return &FencedResource{} }

// Write applies value only if token is strictly greater than every token seen so
// far; otherwise it returns ErrStaleToken and leaves the state unchanged.
func (r *FencedResource) Write(token int64, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token <= r.highest {
		return fmt.Errorf("%w: token %d <= last accepted %d", ErrStaleToken, token, r.highest)
	}
	r.highest = token
	r.state = value
	return nil
}

// State returns the last accepted value and the token that wrote it.
func (r *FencedResource) State() (string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.highest
}
```

### The runnable demo

The demo runs an embedded `miniredis` so it needs no external Redis. Holder A
acquires token 1 and writes; A's lease is then expired with `FastForward`
(standing in for a long GC pause); holder B acquires token 2 and writes; A resumes
and its stale-token write is rejected, leaving B's value in place.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/fencing"
	"github.com/alicebob/miniredis/v2"
	goredislib "github.com/redis/go-redis/v9"
)

func main() {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	defer client.Close()

	ctx := context.Background()
	l := fencing.NewLocker(client, time.Second)
	res := fencing.NewFencedResource()

	a, err := l.Acquire(ctx, "account:42")
	if err != nil {
		panic(err)
	}
	fmt.Printf("holder A token=%d\n", a.Token())
	if err := res.Write(a.Token(), "A"); err == nil {
		fmt.Println("A write accepted")
	}

	// A's lease expires during a long pause; B legitimately takes over.
	mr.FastForward(2 * time.Second)
	b, err := l.Acquire(ctx, "account:42")
	if err != nil {
		panic(err)
	}
	fmt.Printf("holder B token=%d\n", b.Token())
	if err := res.Write(b.Token(), "B"); err == nil {
		fmt.Println("B write accepted")
	}

	// A resumes, unaware it lost the lease, and tries to write.
	if err := res.Write(a.Token(), "A-late"); errors.Is(err, fencing.ErrStaleToken) {
		fmt.Println("A late write rejected: stale fencing token")
	}

	v, tok := res.State()
	fmt.Printf("final state=%q at token %d\n", v, tok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
holder A token=1
A write accepted
holder B token=2
B write accepted
A late write rejected: stale fencing token
final state="B" at token 2
```

### Tests

The tests use `miniredis`. `TestTokensAreMonotonic` acquires the same lock name
twice in sequence and asserts the second token is strictly larger and that
`Value()` parses back to `Token()`. `TestFencedResourceRejectsStale` exercises the
resource in isolation: accept token 5, reject token 3 with `ErrStaleToken`, and
confirm the stored state did not change. `TestStaleHolderCannotOverwrite` runs the
full scenario end to end — A writes with token 1, A's lease is fast-forwarded past
expiry, B takes over with token 2 and writes, then A's late write with token 1 is
rejected and A's `Unlock` returns `false` because B already superseded it.

Create `fence_test.go`:

```go
package fencing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredislib "github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T) (*miniredis.Miniredis, *goredislib.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestTokensAreMonotonic(t *testing.T) {
	t.Parallel()
	_, client := newTestClient(t)
	ctx := context.Background()
	l := NewLocker(client, 2*time.Second)

	h1, err := l.Acquire(ctx, "ledger")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	got, err := strconv.ParseInt(h1.Value(), 10, 64)
	if err != nil || got != h1.Token() {
		t.Fatalf("Value()=%q parsed=%d err=%v; want token=%d", h1.Value(), got, err, h1.Token())
	}
	if _, err := h1.Unlock(ctx); err != nil {
		t.Fatalf("unlock 1: %v", err)
	}

	h2, err := l.Acquire(ctx, "ledger")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	defer h2.Unlock(ctx)
	if h2.Token() <= h1.Token() {
		t.Fatalf("token2=%d not strictly greater than token1=%d", h2.Token(), h1.Token())
	}
}

func TestFencedResourceRejectsStale(t *testing.T) {
	t.Parallel()
	r := NewFencedResource()

	if err := r.Write(5, "new"); err != nil {
		t.Fatalf("write token 5: %v", err)
	}
	if err := r.Write(3, "stale"); !errors.Is(err, ErrStaleToken) {
		t.Fatalf("write token 3 = %v; want ErrStaleToken", err)
	}
	if v, tok := r.State(); v != "new" || tok != 5 {
		t.Fatalf("state = %q,%d; want new,5", v, tok)
	}
}

func TestStaleHolderCannotOverwrite(t *testing.T) {
	t.Parallel()
	mr, client := newTestClient(t)
	ctx := context.Background()
	l := NewLocker(client, time.Second)
	res := NewFencedResource()

	a, err := l.Acquire(ctx, "acct:42")
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	if err := res.Write(a.Token(), "A-write"); err != nil {
		t.Fatalf("A write: %v", err)
	}

	// A's lease expires (a long pause); B legitimately takes over.
	mr.FastForward(2 * time.Second)
	b, err := l.Acquire(ctx, "acct:42")
	if err != nil {
		t.Fatalf("B acquire: %v", err)
	}
	defer b.Unlock(ctx)
	if err := res.Write(b.Token(), "B-write"); err != nil {
		t.Fatalf("B write: %v", err)
	}

	// A resumes and tries to write with its stale token.
	if err := res.Write(a.Token(), "A-late"); !errors.Is(err, ErrStaleToken) {
		t.Fatalf("A late write = %v; want ErrStaleToken", err)
	}

	// A's Unlock is best-effort false: B already superseded the lock value.
	if ok, _ := a.Unlock(ctx); ok {
		t.Fatal("A.Unlock = true; expected false (superseded by B)")
	}

	if v, tok := res.State(); v != "B-write" || tok != b.Token() {
		t.Fatalf("state = %q,%d; want B-write,%d", v, tok, b.Token())
	}
}

func Example() {
	mr, _ := miniredis.Run()
	defer mr.Close()
	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	defer client.Close()

	ctx := context.Background()
	l := NewLocker(client, time.Second)
	res := NewFencedResource()

	h1, _ := l.Acquire(ctx, "k")
	_ = res.Write(h1.Token(), "first")
	_, _ = h1.Unlock(ctx)

	h2, _ := l.Acquire(ctx, "k")
	_ = res.Write(h2.Token(), "second")

	err := res.Write(h1.Token(), "stale") // a late write from the old token
	v, tok := res.State()
	fmt.Printf("token1=%d token2=%d state=%q@%d staleErr=%v\n",
		h1.Token(), h2.Token(), v, tok, errors.Is(err, ErrStaleToken))
	// Output: token1=1 token2=2 state="second"@2 staleErr=true
}
```

## Review

The fencing layer is correct when monotonicity is sourced entirely from `INCR` and
the resource enforces strict ordering. Confirm it by acquiring twice and asserting
the second token is strictly greater; if two acquisitions ever share a token the
counter is not the single source of truth. The end-to-end test is the one that
matters most: it reproduces the exact failure Redlock cannot prevent — a stale
holder acting after its lease expired — and shows the resource rejecting the late
write with `ErrStaleToken`, so state reflects only the newer writer. The mistakes
to avoid are using `Mutex.Value()`'s random default as if it ordered holders (it
does not; that is why `WithGenValueFunc` replaces it with `INCR`) and putting the
ordering check anywhere but the resource (the lock cannot enforce it after a
pause). Run `go test -race` to confirm `FencedResource` is safe under concurrent
writers.

## Resources

- [Is Redlock safe? — antirez](http://antirez.com/news/101)
- [How to do distributed locking — Martin Kleppmann](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)
- [redsync WithGenValueFunc — package reference](https://pkg.go.dev/github.com/go-redsync/redsync/v4#WithGenValueFunc)
- [go-redis INCR / IntCmd](https://pkg.go.dev/github.com/redis/go-redis/v9#Client.Incr)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-lease-renewal-watchdog.md](02-lease-renewal-watchdog.md) | Next: [../10-redis-rate-limiting/00-concepts.md](../10-redis-rate-limiting/00-concepts.md)
