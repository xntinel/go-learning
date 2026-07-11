# Exercise 12: Single-Flight Barrier: Collapse A Cache Stampede With One Close

**Level: Advanced**

When a hot cache key expires, thousands of in-flight requests can each miss and
stampede the origin at the same instant, turning one expensive recompute into
thousands and often knocking the backend over. The fix is to elect one leader per
key to compute the value while every other caller parks on that key's completion
channel; the leader publishes its result and closes the channel, and that single
close broadcasts the shared value to all followers at once. This exercise builds
that barrier and pins down its load-bearing subtlety: the followers read the
leader's writes with no lock on the read path, relying only on the happens-before
edge that `close(done)` then `<-done` establishes.

This module is self-contained: its own module, a `sflight` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
sflight/                     independent module: example.com/sflight
  go.mod                     go 1.26
  sflight.go                 generic Group[K,V] with Do: leader/follower single-flight
  cmd/demo/main.go           runnable demo: stampede collapse, concurrent keys, error, panic re-arm
  sflight_test.go            one-flight, distinct keys, error fan-out, panic re-arm, ctx-cancel, goleak
```

- Files: `sflight.go`, `cmd/demo/main.go`, `sflight_test.go`.
- Implement: `type Group[K comparable, V any]`, `New[K comparable, V any]() *Group[K, V]`, and `func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(context.Context) (V, error)) (val V, err error, shared bool)`.
- Test: N callers for one key invoke `fn` once and all receive the identical value (`shared=true` for followers); distinct keys overlap; a leader error and a recovered leader panic both reach every follower; a cancelled follower returns `ctx.Err()` without disturbing the leader; no goroutine or map-entry leak.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sflight/cmd/demo
cd ~/go-exercises/sflight
go mod init example.com/sflight
go get go.uber.org/goleak
```

### The completion barrier and the memory-model edge

Single-flight has two jobs: elect exactly one leader per in-flight key, and hand
the leader's result to everyone else. Both hinge on the same closed-channel
broadcast from this lesson, but the second job is where the subtlety lives.

The protocol per `Do` call is:

1. Lock the map. If the key already has a `call`, this caller is a *follower*:
   unlock and block on the leader's `done` channel (or its own `ctx.Done()`).
2. Otherwise this caller is the *leader*: create a fresh `call` with an open
   `done` channel, store it under the key, and unlock.
3. The leader runs `fn`, recording `val` and `err` into the `call` (a panic is
   recovered and turned into an error, so no follower is left parked forever).
4. The leader `close(done)`. This is the publish: it broadcasts to every parked
   follower at once, with no value to hand out and no count of followers to know.
5. The leader deletes the key under the lock, so the next `Do` for it starts a
   fresh flight (a stale cache entry does not pin the old result forever).

The memory-model point is step 4 paired with the follower's receive. The leader
writes `c.val` and `c.err` *before* `close(c.done)`. A follower reads them *after*
`<-c.done` returns. The Go memory model guarantees that a receive from a closed
channel happens-after the `close`, and the `close` happens-after every write the
leader made before it. That transitive edge is the *only* synchronization on the
follower's read path: there is no mutex held, no atomic load, nothing else. Remove
the close-then-receive ordering and the followers would be reading fields racily
while the leader writes them, which is exactly what `-race` would flag. The close
is not just a wakeup; it is the memory barrier that makes the shared result
visible.

Note the ownership discipline from the concept file: exactly one goroutine (the
leader) owns and closes `done`, it closes it exactly once, and followers only
receive. Followers never close it, so there is no double-close panic and no
send-on-closed.

Create `sflight.go`:

```go
// Package sflight collapses a cache stampede: for a given key, only the first
// caller (the leader) runs the expensive function; every concurrent caller for
// the same key parks on that key's completion channel and shares the leader's
// result. The completion signal is a single close(done), and the close-then-
// receive edge is the ONLY synchronization the followers rely on to read the
// leader's writes.
package sflight

import (
	"context"
	"fmt"
	"sync"
)

// call is one in-flight computation for a key. done is closed exactly once, by
// the leader, after it has written val and err. A follower must read val and err
// only after <-done has returned: that receive is the happens-before edge that
// makes the leader's writes visible without any other synchronization.
type call[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// Group deduplicates concurrent calls for the same key. The zero value is not
// ready; use New.
type Group[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]*call[V]
}

// New returns a ready Group.
func New[K comparable, V any]() *Group[K, V] {
	return &Group[K, V]{m: make(map[K]*call[V])}
}

// Do runs fn at most once per in-flight key. The first caller for a key is the
// leader: it registers the key, runs fn (with a panic converted to an error),
// publishes the result by closing done, then deletes the key so the next Do
// re-fetches. Concurrent callers for the same key are followers: they block on
// the leader's done channel and reuse its (val, err), returning shared=true. A
// follower whose ctx is cancelled before done closes returns ctx.Err() with
// shared=false and does not disturb the leader or the other followers.
func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(context.Context) (V, error)) (val V, err error, shared bool) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		// Follower: a leader is already computing this key. Release the lock and
		// park on the leader's completion barrier.
		g.mu.Unlock()
		select {
		case <-c.done:
			// close(done) happened-before this receive returns, so the leader's
			// writes to c.val and c.err are visible here with no further locking.
			return c.val, c.err, true
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err(), false
		}
	}

	// Leader: register a fresh call under the lock so later callers find it.
	c := &call[V]{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()

	// Run fn and record the outcome, converting a panic into an error so no
	// follower is left parked on a done that never closes.
	func() {
		defer func() {
			if r := recover(); r != nil {
				c.err = fmt.Errorf("sflight: fn panicked for key: %v", r)
			}
		}()
		c.val, c.err = fn(ctx)
	}()

	// Publish: the writes to c.val and c.err above are ordered before this close,
	// which is the sole edge every follower's receive synchronizes with.
	close(c.done)

	// Re-arm: drop the key so the next Do for it starts a fresh computation.
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err, false
}
```

### The runnable demo

The demo holds each leader in flight on a `release` channel so the followers
provably park before the leader publishes; that is what makes `origin calls: 1`
deterministic rather than timing-dependent. Output is printed in a fixed order
after every goroutine has joined, so it never depends on scheduling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/sflight"
)

func main() {
	stampede()
	distinctKeys()
	errorBroadcast()
	panicReArm()
}

// stampede: N concurrent callers for one hot key collapse to a single origin
// call; the leader computes, and every follower reuses the shared result.
func stampede() {
	g := sflight.New[string, int]()
	var origin atomic.Int64

	release := make(chan struct{})
	var entered atomic.Int64
	load := func(context.Context) (int, error) {
		origin.Add(1)
		<-release // hold the key in flight until every follower has parked
		return 42, nil
	}

	const n = 100
	shared := make([]bool, n)
	vals := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			entered.Add(1)
			v, _, sh := g.Do(context.Background(), "user:1", load)
			vals[i], shared[i] = v, sh
		})
	}

	// Wait until all callers are running, then give the followers a generous
	// margin to reach their <-done park before releasing the leader.
	for entered.Load() < n {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	sharedCount, sameValue := 0, true
	for i := range n {
		if shared[i] {
			sharedCount++
		}
		if vals[i] != 42 {
			sameValue = false
		}
	}
	fmt.Printf("stampede: %d callers for one key\n", n)
	fmt.Printf("  origin calls: %d\n", origin.Load())
	fmt.Printf("  every caller saw the same value: %v\n", sameValue)
	fmt.Printf("  followers that shared the leader result: %d\n", sharedCount)
}

// distinctKeys: different keys are independent, so their leaders run at the same
// time. A three-way barrier proves all three were in flight simultaneously.
func distinctKeys() {
	g := sflight.New[string, int]()
	const k = 3
	var arrived sync.WaitGroup
	arrived.Add(k)
	proceed := make(chan struct{})
	fn := func(context.Context) (int, error) {
		arrived.Done() // announce this leader is in flight
		<-proceed      // block until all k leaders are in flight
		return 0, nil
	}

	var wg sync.WaitGroup
	for _, key := range []string{"a", "b", "c"} {
		wg.Go(func() { g.Do(context.Background(), key, fn) })
	}
	arrived.Wait() // returns only once all k leaders overlap
	peak := k
	close(proceed)
	wg.Wait()
	fmt.Printf("distinct keys: peak concurrent leaders = %d\n", peak)
}

// errorBroadcast: a leader error is delivered to every follower.
func errorBroadcast() {
	g := sflight.New[string, int]()
	origin := errors.New("origin unavailable")

	release := make(chan struct{})
	var entered atomic.Int64
	fn := func(context.Context) (int, error) {
		<-release
		return 0, origin
	}

	const n = 50
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			entered.Add(1)
			_, err, _ := g.Do(context.Background(), "k", fn)
			errs[i] = err
		})
	}
	for entered.Load() < n {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	all := 0
	for _, err := range errs {
		if errors.Is(err, origin) {
			all++
		}
	}
	fmt.Printf("error broadcast: %d/%d callers received the leader error\n", all, n)
}

// panicReArm: a leader panic becomes an error (no follower hangs) and the key is
// deleted, so a later Do re-invokes fn and succeeds.
func panicReArm() {
	g := sflight.New[string, int]()

	_, err, _ := g.Do(context.Background(), "k", func(context.Context) (int, error) {
		panic("origin driver crashed")
	})
	fmt.Printf("panic recovered as error: %v\n", err != nil)

	calls := 0
	v, err2, _ := g.Do(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 7, nil
	})
	fmt.Printf("key re-armed after panic: re-invoked=%v value=%d err=%v\n", calls == 1, v, err2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stampede: 100 callers for one key
  origin calls: 1
  every caller saw the same value: true
  followers that shared the leader result: 99
distinct keys: peak concurrent leaders = 3
error broadcast: 50/50 callers received the leader error
panic recovered as error: true
key re-armed after panic: re-invoked=true value=7 err=<nil>
```

### Tests

`TestOneKeyOneFlightSharedResult` holds the leader in flight until all 200 callers
park, then asserts `fn` ran exactly once, every caller saw the same value, and
exactly `n-1` followers reported `shared=true` (the leader reports `false`). Run
under `-race`, this is also the data-race proof: 199 followers read `c.val` while
the leader wrote it, and the test is clean only because the close-then-receive
edge orders those accesses. `TestDistinctKeysRunConcurrently` uses a `k`-way
`WaitGroup` barrier that can only unblock if all `k` leaders overlap, proving
distinct keys are independent. `TestLeaderErrorReachesEveryFollower` checks a
leader error fans out to all followers. `TestLeaderPanicRecoveredAndKeyReArmed`
shows a panicking leader becomes an error for every follower (no hang) and then a
fresh `Do` re-invokes `fn`, proving the key was deleted. `TestFollowerCancelLeaves
LeaderAndOthersIntact` cancels half the followers before release and asserts they
return `context.Canceled` with `shared=false` while the leader still completes and
the healthy followers still share its value. `TestMain` wraps everything in
`goleak.VerifyTestMain`, which fails if any goroutine or the map entry survives.

Create `sflight_test.go`:

```go
package sflight

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// goleak proves no goroutine (and, transitively, no lingering map entry that
	// would keep one parked) survives any test.
	goleak.VerifyTestMain(m)
}

// waitEntered spins until n callers have entered Do, then adds a generous margin
// so the followers reach their <-done park. The leader is held in flight the
// whole time, so only that margin (not correctness of the barrier) depends on it.
func waitEntered(entered *atomic.Int64, n int64) {
	for entered.Load() < n {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
}

func TestOneKeyOneFlightSharedResult(t *testing.T) {
	g := New[string, int]()
	var calls atomic.Int64
	release := make(chan struct{})
	var entered atomic.Int64
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		<-release // hold the key in flight until every follower has parked
		return 42, nil
	}

	const n = 200
	vals := make([]int, n)
	shared := make([]bool, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			entered.Add(1)
			vals[i], _, shared[i] = g.Do(context.Background(), "k", fn)
		})
	}
	waitEntered(&entered, n)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn must run exactly once, ran %d times", got)
	}
	sharedCount := 0
	for i := range n {
		if vals[i] != 42 {
			t.Fatalf("caller %d saw %d, want 42", i, vals[i])
		}
		if shared[i] {
			sharedCount++
		}
	}
	if sharedCount != n-1 {
		t.Fatalf("want exactly one leader and %d shared followers, got %d shared", n-1, sharedCount)
	}
}

func TestDistinctKeysRunConcurrently(t *testing.T) {
	g := New[string, int]()
	const k = 8
	var arrived sync.WaitGroup
	arrived.Add(k)
	proceed := make(chan struct{})
	fn := func(context.Context) (int, error) {
		arrived.Done() // announce this key's leader is in flight
		<-proceed      // and block until every key's leader is in flight
		return 0, nil
	}

	var wg sync.WaitGroup
	for i := range k {
		key := string(rune('a' + i))
		wg.Go(func() { g.Do(context.Background(), key, fn) })
	}
	// arrived.Wait returns only if all k leaders overlapped; if distinct keys
	// serialized, fewer than k would ever be in flight and this would hang.
	arrived.Wait()
	close(proceed)
	wg.Wait()
}

func TestLeaderErrorReachesEveryFollower(t *testing.T) {
	g := New[string, int]()
	sentinel := errors.New("origin down")
	release := make(chan struct{})
	var entered atomic.Int64
	fn := func(context.Context) (int, error) {
		<-release
		return 0, sentinel
	}

	const n = 64
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			entered.Add(1)
			_, errs[i], _ = g.Do(context.Background(), "k", fn)
		})
	}
	waitEntered(&entered, n)
	close(release)
	wg.Wait()

	for i := range n {
		if !errors.Is(errs[i], sentinel) {
			t.Fatalf("caller %d got %v, want the leader error", i, errs[i])
		}
	}
}

func TestLeaderPanicRecoveredAndKeyReArmed(t *testing.T) {
	g := New[string, int]()

	// A panicking leader must not leave followers hung: it is converted to an
	// error and the key is still deleted.
	release := make(chan struct{})
	var entered atomic.Int64
	panicFn := func(context.Context) (int, error) {
		<-release
		panic("driver blew up")
	}

	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			entered.Add(1)
			_, errs[i], _ = g.Do(context.Background(), "k", panicFn)
		})
	}
	waitEntered(&entered, n)
	close(release)
	wg.Wait()

	for i := range n {
		if errs[i] == nil {
			t.Fatalf("caller %d got nil error, want the recovered panic", i)
		}
	}

	// Re-arm proof: a later Do for the same key re-invokes fn and succeeds.
	var calls atomic.Int64
	v, err, shared := g.Do(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 7, nil
	})
	if err != nil || v != 7 {
		t.Fatalf("re-fetch after panic: got (%d, %v), want (7, nil)", v, err)
	}
	if shared {
		t.Fatal("a fresh flight after re-arm must not report shared")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("re-fetch must invoke fn once, invoked %d", got)
	}
}

func TestFollowerCancelLeavesLeaderAndOthersIntact(t *testing.T) {
	g := New[string, int]()
	var calls atomic.Int64
	leaderStarted := make(chan struct{})
	release := make(chan struct{})
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		close(leaderStarted) // fn runs only after the key is registered
		<-release
		return 99, nil
	}

	// Elect the leader explicitly and wait until it is in flight.
	var leaderVal int
	var leaderErr error
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		leaderVal, leaderErr, _ = g.Do(context.Background(), "k", fn)
	}()
	<-leaderStarted

	// Followers: half will have their ctx cancelled before release, half wait.
	const half = 10
	var entered atomic.Int64
	cancelErrs := make([]error, half)
	cancelShared := make([]bool, half)
	cancels := make([]context.CancelFunc, half)
	healthyVals := make([]int, half)
	healthyShared := make([]bool, half)

	var wg sync.WaitGroup
	for i := range half {
		cctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		wg.Go(func() {
			entered.Add(1)
			_, cancelErrs[i], cancelShared[i] = g.Do(cctx, "k", fn)
		})
	}
	for i := range half {
		wg.Go(func() {
			entered.Add(1)
			healthyVals[i], _, healthyShared[i] = g.Do(context.Background(), "k", fn)
		})
	}
	waitEntered(&entered, 2*half)

	// Cancel one group; those followers unblock via ctx.Done, not via the leader.
	for _, cancel := range cancels {
		cancel()
	}

	// Release the leader; the healthy followers share its result.
	close(release)
	<-leaderDone
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn must run exactly once despite a cancelled follower, ran %d", got)
	}
	if leaderErr != nil || leaderVal != 99 {
		t.Fatalf("leader unaffected: got (%d, %v), want (99, nil)", leaderVal, leaderErr)
	}
	for i := range half {
		if !errors.Is(cancelErrs[i], context.Canceled) {
			t.Fatalf("cancelled follower %d got %v, want context.Canceled", i, cancelErrs[i])
		}
		if cancelShared[i] {
			t.Fatalf("cancelled follower %d reported shared, want false", i)
		}
		if healthyVals[i] != 99 || !healthyShared[i] {
			t.Fatalf("healthy follower %d got (%d, shared=%v), want (99, true)", i, healthyVals[i], healthyShared[i])
		}
	}
}
```

## Review

Correct here means three things hold together: exactly one `fn` runs per in-flight
key, every follower observes the identical result, and nothing leaks. The single
`close(done)` delivers all three at once. It broadcasts to an unbounded set of
followers without the leader knowing how many there are, and it is the memory
barrier that makes the leader's `val`/`err` writes visible to a follower's
lock-free read: the receive from the closed channel happens-after the close, which
happens-after those writes, so `-race` stays clean where an unsynchronized read
would not. `TestOneKeyOneFlightSharedResult` pins the exactly-once and shared-value
invariants under `-race`; the panic and error tests prove the barrier still
releases followers on the failure paths (a panic that was not recovered would
leave the channel unclosed and hang every follower, which `goleak` would catch);
and the cancel test shows a follower can abandon the wait via its own
`ctx.Done()` without owning or closing the leader's channel, so it never disturbs
the leader or its peers. The production bug this prevents is the cache stampede: a
popular key expiring under load and every request recomputing it in parallel,
which single-flight collapses to one origin call plus a cheap parked wait for
everyone else.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- defines the close-then-receive happens-before edge the follower read depends on.
- [pkg.go.dev: golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the production-grade version of this pattern, with the same leader/follower and `shared` semantics.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the goroutine-leak detector that proves no follower stays parked.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) -- the recover mechanics behind converting a leader panic into a follower-visible error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-config-reload-watch-broadcast.md](11-config-reload-watch-broadcast.md) | Next: [13-request-hedging-first-success-cancel.md](13-request-hedging-first-success-cancel.md)
