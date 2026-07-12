# Exercise 13: Cache-Stampede Coalescing That Strands No Waiter

**Level: Advanced**

Under a cache-miss storm, thousands of requests for the same hot key hit the origin
at once. A call-coalescing group collapses them into a single in-flight origin call
and shares one result — but the naive version is a leak-and-correctness minefield: a
waiter whose request context is cancelled must stop waiting without leaking and
without aborting the work the others still need; the leader must publish a result
even when the origin panics, or every waiter strands on the result channel forever;
and the per-key entry must be deleted or the map grows without bound. This exercise
builds `Do` with all three guarantees and proves them under `goleak`.

This module is self-contained: its own module, a `coalesce` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
coalesce/                    independent module: example.com/coalesce
  go.mod                     go 1.26
  coalesce.go                Group[V], NewGroup, Do (leader/waiter), InFlight, ErrPanic
  cmd/demo/main.go           runnable demo: no-contention, panic, cancel-safe, concurrency
  coalesce_test.go           exactly-once, waiter cancel, leader cancel, panic, distinct keys
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: `NewGroup[V any]() *Group[V]`, `(*Group[V]).Do(ctx, key, fn) (V, bool, error)`, `(*Group[V]).InFlight() int`, and `ErrPanic`.
- Test: N callers invoke `fn` once and share the value; a cancelled waiter returns `ctx.Err()` and disturbs no one; a cancelled *leader* does not abort the shared call; a panic wakes every waiter with `ErrPanic`; distinct keys run concurrently; `InFlight()==0` after every call.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/13-request-coalescing-cancel-safe-no-leak/cmd/demo
cd go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/13-request-coalescing-cancel-safe-no-leak
go get go.uber.org/goleak
go mod tidy
```

### One origin call, three exit guarantees

A coalescing group keeps a map `key -> *call`, where a `call` is the shared state
for one in-flight origin invocation. The first caller for a key becomes the *leader*:
it inserts the `call` and launches the origin `fn` in its own goroutine. Every later
caller for the same key is a *waiter*: it finds the existing `call`, records that it
joined, and blocks. When the origin returns, the leader closes the `call`'s `done`
channel and every parked caller reads the result. That is the easy half.

The hard half is the exit contract, and it has three parts, each a leak or a
stranding bug if you get it wrong:

1. **A cancelled caller must leave without touching the shared call.** Every caller —
   leader and waiter alike — blocks in `select { case <-c.done: ...; case
   <-ctx.Done(): return ctx.Err() }`. Cancellation returns immediately; it does not
   close `done`, does not delete the key, and does not signal the origin. The other
   callers never notice. A caller that instead blocked on a bare `<-c.done` would be
   unkillable: cancelling its context would do nothing, and it would leak until the
   origin happened to finish.

2. **The leader's own cancellation must not kill the shared work.** The origin runs
   under `context.WithoutCancel(ctx)` — a context that carries the leader's values but
   is detached from its cancellation. So when the leader's request is cancelled, the
   leader returns `ctx.Err()` to *its* caller, but the origin call keeps running for
   the waiters who still need it. Passing the leader's raw `ctx` to `fn` would be the
   classic stampede bug: the first client to hang up cancels the origin for everyone.

3. **The leader must publish a result on every path, including panic.** The origin
   goroutine `defer`s a `recover` that converts a panic into `ErrPanic` and then —
   unconditionally, under the mutex — sets the result, deletes the key, and closes
   `done`. Because `close(done)` is in the `defer`, it runs whether `fn` returned
   normally, returned an error, or panicked. Skip it on the panic path and every
   waiter blocks on `done` forever: a single bad origin response leaks N goroutines.
   Deleting the key in the same `defer` is what keeps the map bounded under churn.

The `done` channel is the happens-before edge that makes the shared fields safe to
read without a lock: the leader writes `val`, `err`, and `shared` before `close(done)`,
and every caller reads them only after receiving from `done`. The join counter
`dups` is the one field mutated by waiters, so it is guarded by the group mutex.

Create `coalesce.go`:

```go
// Package coalesce collapses a stampede of concurrent requests for the same key
// into a single in-flight origin call whose result is shared with every waiter.
package coalesce

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrPanic is returned to every caller of a coalesced group whose origin fn
// panicked. The panic value is wrapped so errors.Is(err, ErrPanic) is true.
var ErrPanic = errors.New("coalesce: origin call panicked")

// call is the shared state for one in-flight key. Every field except done is
// written only under Group.mu or before done is closed; readers read after they
// receive from done, so the close is the happens-before edge.
type call[V any] struct {
	done   chan struct{} // closed exactly once when val/err/shared are final
	val    V
	err    error
	dups   int  // waiters that joined this call; guarded by Group.mu
	shared bool // dups > 0 at completion; set under Group.mu before close(done)
}

// Group coalesces concurrent Do calls that share a key.
type Group[V any] struct {
	mu    sync.Mutex
	calls map[string]*call[V]
}

// NewGroup returns an empty Group.
func NewGroup[V any]() *Group[V] {
	return &Group[V]{calls: make(map[string]*call[V])}
}

// Do coalesces concurrent calls for key. The first caller (the leader) launches
// fn once in its own goroutine under context.WithoutCancel of the leader's ctx,
// so no single caller's cancellation can abort the work the others still need.
// Every caller then selects on the shared done channel and its own ctx.Done, so a
// cancelled caller returns ctx.Err() immediately without leaking and without
// disturbing the shared call. shared reports whether more than one caller shared
// this result.
func (g *Group[V]) Do(ctx context.Context, key string, fn func(ctx context.Context) (V, error)) (v V, shared bool, err error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		c.dups++
		g.mu.Unlock()
		return g.wait(ctx, c)
	}
	c := &call[V]{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	// The shared work runs under a context detached from the leader's ctx, so the
	// leader cancelling below does not kill work the waiters depend on.
	go g.exec(context.WithoutCancel(ctx), key, c, fn)

	return g.wait(ctx, c)
}

// wait blocks until the shared call publishes a result or the caller's own ctx is
// cancelled. Cancellation returns immediately; it neither closes done nor removes
// the key, so the shared call and the other waiters are untouched.
func (g *Group[V]) wait(ctx context.Context, c *call[V]) (v V, shared bool, err error) {
	select {
	case <-c.done:
		return c.val, c.shared, c.err
	case <-ctx.Done():
		var zero V
		return zero, false, ctx.Err()
	}
}

// exec runs the origin fn exactly once for a key. Its deferred recover converts a
// panic into ErrPanic and ALWAYS deletes the key and closes done under the mutex,
// so no waiter can strand on done and the map cannot grow unbounded.
func (g *Group[V]) exec(ctx context.Context, key string, c *call[V], fn func(ctx context.Context) (V, error)) {
	defer func() {
		r := recover()
		g.mu.Lock()
		defer g.mu.Unlock()
		if r != nil {
			c.err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
		c.shared = c.dups > 0
		delete(g.calls, key)
		close(c.done)
	}()
	c.val, c.err = fn(ctx)
}

// InFlight reports how many distinct keys are currently executing. It returns to
// zero once every coalesced call has completed.
func (g *Group[V]) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}
```

### The runnable demo

The demo drives four scenarios and prints only facts that are invariant once each
step's precondition holds: it advances by polling a real state (`InFlight` reaching
a known value), never by a tuned sleep. Section 3 cancels a waiter and then releases
the origin, so the waiter's cancellation is observed *before* the value exists — proof
the shared work survived it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/coalesce"
)

// pollUntil spins until cond holds or a generous deadline passes. It waits on a
// real state (InFlight reaching a known value), not on a tuned delay: each step
// of the demo blocks until the condition it depends on is actually true, so the
// printed output is deterministic.
func pollUntil(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

type result struct {
	val    int
	shared bool
	err    error
}

func main() {
	bg := context.Background()

	// 1. No contention: a lone caller runs fn once and is not shared.
	g := coalesce.NewGroup[int]()
	v, shared, err := g.Do(bg, "solo", func(context.Context) (int, error) { return 42, nil })
	fmt.Printf("no contention:    value=%d shared=%v err=%v inflight=%d\n", v, shared, err, g.InFlight())

	// 2. Panic: the origin panics, yet the caller wakes with ErrPanic and the key
	//    is drained rather than left in the map.
	_, _, err = g.Do(bg, "boom", func(context.Context) (int, error) { panic("origin exploded") })
	fmt.Printf("panic:            isErrPanic=%v inflight=%d\n", errors.Is(err, coalesce.ErrPanic), g.InFlight())

	// 3. A waiter cancels; the shared work is not aborted and the leader is served.
	release := make(chan struct{})
	var calls atomic.Int64
	block := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}
	leaderCh := make(chan result, 1)
	go func() {
		lv, ls, le := g.Do(bg, "hot", block)
		leaderCh <- result{lv, ls, le}
	}()
	// The leader is in flight once its key appears; the key stays until release.
	pollUntil(func() bool { return g.InFlight() == 1 })

	wctx, wcancel := context.WithCancel(bg)
	defer wcancel()
	waiterCh := make(chan result, 1)
	go func() {
		// A waiter never runs fn; the leader owns the single origin call.
		wv, ws, we := g.Do(wctx, "hot", func(context.Context) (int, error) { panic("waiter must not run fn") })
		waiterCh <- result{wv, ws, we}
	}()
	wcancel()               // cancel the WAITER's ctx
	waiter := <-waiterCh    // returns before release: the value is not ready yet
	fmt.Printf("waiter cancelled: err=%v shared=%v\n", waiter.err, waiter.shared)

	close(release)        // let the shared origin call finish
	leader := <-leaderCh  // the leader was never cancelled, so it is served
	fmt.Printf("leader served:    value=%d shared=%v origin_calls=%d\n", leader.val, leader.shared, calls.Load())
	pollUntil(func() bool { return g.InFlight() == 0 })
	fmt.Printf("after coalesce:   inflight=%d\n", g.InFlight())

	// 4. Distinct keys run concurrently: all K leaders sit in fn at once, so
	//    InFlight reaches K. Serial execution could never reach K here.
	const K = 4
	g2 := coalesce.NewGroup[int]()
	gate := make(chan struct{})
	for i := range K {
		go func() {
			_, _, _ = g2.Do(bg, fmt.Sprintf("key-%d", i), func(context.Context) (int, error) {
				<-gate
				return i, nil
			})
		}()
	}
	reachedK := pollUntil(func() bool { return g2.InFlight() == K })
	close(gate)
	pollUntil(func() bool { return g2.InFlight() == 0 })
	fmt.Printf("distinct keys:    concurrent=%v inflight=%d\n", reachedK, g2.InFlight())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
no contention:    value=42 shared=false err=<nil> inflight=0
panic:            isErrPanic=true inflight=0
waiter cancelled: err=context canceled shared=false
leader served:    value=7 shared=true origin_calls=1
after coalesce:   inflight=0
distinct keys:    concurrent=true inflight=0
```

### Tests

`TestMain` wraps the whole package in `goleak.VerifyTestMain`, so any goroutine that
outlives a test fails the package. `TestCoalescesExactlyOnce` is the headline: 100
callers pile onto one key, and a white-box probe of the join counter (`dupsOf`) lets
the barrier release only after all 99 waiters have coalesced, so `fn` running exactly
once is deterministic. `TestCancelledWaiterLeavesOthersUnharmed` cancels one waiter
and confirms it returns `context.Canceled` while the leader and the surviving waiter
still receive the shared value. `TestLeaderCancelDoesNotAbortSharedWork` is the
`WithoutCancel` proof: the origin honours its context, so if the leader's cancel
reached it the waiter would get an error instead of the value.
`TestPanicWakesEveryWaiter` confirms a panicking origin wakes all callers with
`ErrPanic` and drains the key. `TestDistinctKeysRunConcurrently` shows different keys
do not serialize.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain runs every test and then fails the package if any goroutine survived
// it. This is what turns "no leak" from a claim into a gate.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type res struct {
	val    int
	shared bool
	err    error
}

// dupsOf reads a call's join counter under the group mutex. It is a white-box
// probe so a test can release a barrier only once every waiter has coalesced,
// making pile-up deterministic instead of timing-tuned.
func dupsOf[V any](g *Group[V], key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.dups
	}
	return -1
}

// waitFor polls cond until it holds or a generous deadline passes.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// mustNotRun is an origin fn a waiter must never execute; if a waiter ever leads
// by mistake it panics and the test fails loudly.
func mustNotRun(context.Context) (int, error) { panic("waiter ran the origin fn") }

// TestCoalescesExactlyOnce pins the headline: N concurrent callers for one key
// pile onto a single leader, fn runs exactly once, and every caller receives the
// same value marked shared. The barrier releases only after all N-1 waiters have
// joined, so the exactly-once assertion is deterministic under -count and -race.
func TestCoalescesExactlyOnce(t *testing.T) {
	g := NewGroup[int]()
	const N = 100
	var calls atomic.Int64
	release := make(chan struct{})
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 99, nil
	}

	vals := make([]int, N)
	shareds := make([]bool, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			vals[i], shareds[i], errs[i] = g.Do(context.Background(), "hot", fn)
		})
	}

	if !waitFor(func() bool { return dupsOf(g, "hot") == N-1 }) {
		t.Fatalf("waiters never fully coalesced: dups=%d, want %d", dupsOf(g, "hot"), N-1)
	}
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("origin fn ran %d times, want 1", got)
	}
	for i := range N {
		if errs[i] != nil {
			t.Fatalf("caller %d: err=%v, want nil", i, errs[i])
		}
		if vals[i] != 99 {
			t.Fatalf("caller %d: value=%d, want 99", i, vals[i])
		}
		if !shareds[i] {
			t.Fatalf("caller %d: shared=false, want true", i)
		}
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight=%d after return, want 0", g.InFlight())
	}
}

// TestCancelledWaiterLeavesOthersUnharmed proves a waiter whose ctx is cancelled
// returns ctx.Err() and vanishes without leaking or aborting the shared call: the
// leader and the surviving waiters still receive the one shared result.
func TestCancelledWaiterLeavesOthersUnharmed(t *testing.T) {
	g := NewGroup[int]()
	var calls atomic.Int64
	release := make(chan struct{})
	block := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}

	leaderCh := make(chan res, 1)
	go func() {
		v, s, e := g.Do(context.Background(), "k", block)
		leaderCh <- res{v, s, e}
	}()
	if !waitFor(func() bool { return g.InFlight() == 1 }) {
		t.Fatal("leader never took the key")
	}

	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelledCh := make(chan error, 1)
	go func() {
		_, _, e := g.Do(wctx, "k", mustNotRun)
		cancelledCh <- e
	}()
	survivorCh := make(chan res, 1)
	go func() {
		v, s, e := g.Do(context.Background(), "k", mustNotRun)
		survivorCh <- res{v, s, e}
	}()

	// Release only after both waiters have joined the single leader.
	if !waitFor(func() bool { return dupsOf(g, "k") == 2 }) {
		t.Fatalf("waiters did not join: dups=%d, want 2", dupsOf(g, "k"))
	}
	cancel()
	if e := <-cancelledCh; !errors.Is(e, context.Canceled) {
		t.Fatalf("cancelled waiter err=%v, want context.Canceled", e)
	}

	close(release)
	survivor := <-survivorCh
	leader := <-leaderCh
	for _, r := range []res{survivor, leader} {
		if r.err != nil || r.val != 7 {
			t.Fatalf("survivor got value=%d err=%v, want 7,<nil>", r.val, r.err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("origin fn ran %d times, want 1", got)
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight=%d, want 0", g.InFlight())
	}
}

// TestLeaderCancelDoesNotAbortSharedWork is the WithoutCancel guarantee: even the
// leader cancelling its own ctx must not kill the origin call the waiters depend
// on. The origin fn honours its ctx, so if the leader's cancel reached it the
// waiter would receive an error instead of the value.
func TestLeaderCancelDoesNotAbortSharedWork(t *testing.T) {
	g := NewGroup[int]()
	var calls atomic.Int64
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		select {
		case <-release:
			return 5, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	leaderCh := make(chan res, 1)
	go func() {
		v, s, e := g.Do(lctx, "k", fn)
		leaderCh <- res{v, s, e}
	}()
	if !waitFor(func() bool { return g.InFlight() == 1 }) {
		t.Fatal("leader never took the key")
	}

	waiterCh := make(chan res, 1)
	go func() {
		v, s, e := g.Do(context.Background(), "k", mustNotRun)
		waiterCh <- res{v, s, e}
	}()
	if !waitFor(func() bool { return dupsOf(g, "k") == 1 }) {
		t.Fatalf("waiter did not join: dups=%d, want 1", dupsOf(g, "k"))
	}

	lcancel() // cancel the LEADER
	leader := <-leaderCh
	if !errors.Is(leader.err, context.Canceled) {
		t.Fatalf("leader err=%v, want context.Canceled", leader.err)
	}

	close(release) // the detached origin call is still alive and now finishes
	waiter := <-waiterCh
	if waiter.err != nil || waiter.val != 5 {
		t.Fatalf("waiter got value=%d err=%v, want 5,<nil> (shared work was aborted)", waiter.val, waiter.err)
	}
	if !waiter.shared {
		t.Fatalf("waiter shared=false, want true")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("origin fn ran %d times, want 1", got)
	}
	if !waitFor(func() bool { return g.InFlight() == 0 }) {
		t.Fatalf("InFlight=%d, want 0", g.InFlight())
	}
}

// TestPanicWakesEveryWaiter proves a panicking origin wakes every caller with
// ErrPanic (never strands them on done) and drains the key.
func TestPanicWakesEveryWaiter(t *testing.T) {
	g := NewGroup[int]()
	var calls atomic.Int64
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		panic("origin exploded")
	}

	const N = 50
	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			_, _, errs[i] = g.Do(context.Background(), "k", fn)
		})
	}
	wg.Wait()

	for i := range N {
		if !errors.Is(errs[i], ErrPanic) {
			t.Fatalf("caller %d: err=%v, want ErrPanic", i, errs[i])
		}
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight=%d after panic, want 0", g.InFlight())
	}
}

// TestDistinctKeysRunConcurrently proves different keys do not serialize: all K
// leaders sit in fn at once, so InFlight reaches K. Serial execution could never
// reach K here because each fn blocks on the shared gate.
func TestDistinctKeysRunConcurrently(t *testing.T) {
	g := NewGroup[int]()
	const K = 8
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range K {
		wg.Go(func() {
			_, _, _ = g.Do(context.Background(), fmt.Sprintf("k%d", i), func(context.Context) (int, error) {
				<-gate
				return i, nil
			})
		})
	}
	if !waitFor(func() bool { return g.InFlight() == K }) {
		t.Fatalf("only %d/%d keys ran concurrently", g.InFlight(), K)
	}
	close(gate)
	wg.Wait()
	if g.InFlight() != 0 {
		t.Fatalf("InFlight=%d, want 0", g.InFlight())
	}
}

// TestLoneCallerNotShared pins that a caller with no company is not marked shared.
func TestLoneCallerNotShared(t *testing.T) {
	g := NewGroup[int]()
	v, shared, err := g.Do(context.Background(), "k", func(context.Context) (int, error) { return 1, nil })
	if err != nil || v != 1 {
		t.Fatalf("got value=%d err=%v, want 1,<nil>", v, err)
	}
	if shared {
		t.Fatal("shared=true for a lone caller, want false")
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight=%d, want 0", g.InFlight())
	}
}
```

## Review

"Correct" here means three exit contracts hold simultaneously under a stampede: a
cancelled caller returns `ctx.Err()` and leaks nothing, the shared origin call is
never aborted by any single caller's cancellation, and every caller — however the
origin ends — either receives the result or its own cancellation error, with the key
always deleted afterward. The guaranteeing invariants are the `select` on `done`
versus `ctx.Done()` that every caller shares (cancellation without side effects), the
`context.WithoutCancel` under which the origin runs (the leader's cancel cannot reach
the work), and the deferred `recover` that unconditionally sets the result, deletes
the key, and closes `done` under the mutex (no strand, bounded map, even on panic).
The tests prove it: `dupsOf` polling makes the exactly-once and cancellation cases
deterministic rather than racy, the panicking-origin test asserts `errors.Is(err,
ErrPanic)` for all 50 callers, and `goleak.VerifyTestMain` fails the package if any
goroutine outlives it. The production bug this prevents is the coalescer that hangs
on the first origin panic or the first client that hangs up — where one bad request
strands a thousand waiting goroutines and the pod OOM-kills with no crash to blame.

## Resources

- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the standard call-coalescer this exercise reconstructs, including its `shared` return and `DoChan` cancellation story.
- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) -- the detach primitive that lets shared work outlive the caller that started it.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector and `VerifyTestMain` used to gate the package.
- [Go Memory Model](https://go.dev/ref/mem) -- why closing `done` after the writes, and reading after the receive, is a data-race-free publish.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-outbox-relay-join-inflight-dispatch.md](12-outbox-relay-join-inflight-dispatch.md) | Next: [14-rpc-response-demux-pending-call-leak.md](14-rpc-response-demux-pending-call-leak.md)
