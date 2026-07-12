# Exercise 14: Single-Flight Dedup That Never Wedges the Herd on Error or Panic

**Level: Advanced**

When a hot cache key expires under load, a stampede of concurrent requests all miss it at once and each would run the same expensive backend load. Single-flight collapses them: one goroutine (the leader) computes while the rest (followers) wait and share the result. The deadlock this exercise targets lives on the failure paths -- if the leader returns on an error path, or panics inside the loader, without closing the shared done channel and deleting its in-flight map entry, every follower blocks forever and future callers attach to a dead entry. This module builds a generic `Group` whose leader releases the herd on every path -- success, error, and panic -- and whose followers each keep a `ctx.Done` exit so one slow caller cannot wedge them.

This module is self-contained: its own module, a `singleflight` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
singleflight/                independent module: example.com/singleflight
  go.mod                     go 1.26
  singleflight.go            Group[K,V].Do dedups concurrent loads; releases followers on every path
  cmd/demo/main.go           runnable demo: stampede collapse, error fan-out, panic safety, cancel exit
  singleflight_test.go       exactly-once, error-to-all + re-run, panic-to-error, follower cancel
```

- Files: `singleflight.go`, `cmd/demo/main.go`, `singleflight_test.go`.
- Implement: `func (g *Group[K, V]) Do(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error)`, plus a `NumWaiters(key K) int` observability helper and a `PanicError` type.
- Test: N concurrent callers run `load` exactly once and all get the value; an error reaches every follower and the next call re-runs; a panicking loader yields an error to all callers with no hang; a cancelled follower returns `context.Canceled` promptly while the leader and its peers still succeed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### The invariant: the done channel is closed on every exit the leader can take

A follower's only way to learn the result is `<-c.done`, an unbuffered channel it blocks on. That receive returns exactly once, when the leader closes `done`. So the entire correctness of the pattern rests on one invariant: **the leader closes `done` on every path out of the loader, and it deletes the map entry in the same step.** Two failure modes follow directly from breaking it. If the leader returns early on an error without closing `done`, every follower is parked forever -- a partial deadlock the runtime never reports, because the HTTP server and the rest of the process keep running. If the leader forgets to `delete` the entry, `done` may be closed but the stale `*call` lingers in the map, and the next caller for that key attaches to a call that will never fire again.

The leader path is therefore built around a single deferred cleanup that runs no matter how the loader exits:

1. The leader publishes `c` in the in-flight map under the mutex, then unlocks and runs `load`.
2. A `defer` recovers any panic from `load`, converting it into a `*PanicError` stored in `c.err`, so a panicking loader becomes an error for the whole herd instead of a crash or a wedge.
3. The same `defer`, after recovery, deletes the map entry under the mutex and closes `done`.

Because delete-and-close live in a `defer`, they execute on the success return, on an error return, and on a panic unwind alike -- the three exits a loader can take. `close(done)` also supplies the happens-before edge: every field the leader wrote (`val`, `err`) is written before the close, and each follower reads them after `<-done`, so there is no data race on the shared result even without a per-field lock.

Followers must not trade one wedge for another. A follower blocks on `c.done`, but a leader can be arbitrarily slow, so the follower's blocking receive is a `select` that also watches `ctx.Done()`. A caller whose context is cancelled returns `ctx.Err()` immediately and leaves the leader and the other followers untouched -- the leader keeps computing and still releases everyone else when it finishes.

Create `singleflight.go`:

```go
// Package singleflight collapses concurrent calls for the same key into one
// execution of an expensive loader, sharing the result with every caller. The
// design goal is that no path -- success, error, or panic -- can ever leave a
// follower blocked forever on the shared done channel.
package singleflight

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// call is one in-flight execution. The leader publishes it in the group's map;
// followers find it and block on done. Every field except waiters is written
// once by the leader before close(done) and read by followers after <-done, so
// close(done) provides the happens-before edge. waiters is guarded by the
// group mutex.
type call[V any] struct {
	done    chan struct{}
	val     V
	err     error
	waiters int
}

// Group deduplicates concurrent Do calls per key. The zero value is ready to
// use; K must be comparable to serve as a map key.
type Group[K comparable, V any] struct {
	mu       sync.Mutex
	inflight map[K]*call[V]
}

// PanicError wraps a value recovered from a panicking loader so a panic on the
// leader path becomes an error delivered to every caller instead of a wedge or
// a crash.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("singleflight: loader panicked: %v", e.Value)
}

// Do runs load at most once per in-flight key. The first caller for a key is
// the leader and runs load; concurrent callers are followers that block on the
// leader's result. The leader releases followers on every path: a deferred
// close(done) plus delete(inflight, key) run on success, on error, and on
// panic, and a recovered panic is turned into a *PanicError so a panicking
// loader cannot wedge the herd. A follower whose ctx is cancelled returns
// ctx.Err() promptly without disturbing the leader or the other followers.
func (g *Group[K, V]) Do(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	g.mu.Lock()
	if c, ok := g.inflight[key]; ok {
		c.waiters++
		g.mu.Unlock()
		return g.wait(ctx, c)
	}
	c := &call[V]{done: make(chan struct{})}
	if g.inflight == nil {
		g.inflight = make(map[K]*call[V])
	}
	g.inflight[key] = c
	g.mu.Unlock()

	g.lead(ctx, key, c, load)
	return c.val, c.err
}

// wait blocks a follower on the leader's result, but always keeps the ctx.Done
// exit so an individual caller can bail out of a slow leader.
func (g *Group[K, V]) wait(ctx context.Context, c *call[V]) (V, error) {
	select {
	case <-c.done:
		g.release(c)
		return c.val, c.err
	case <-ctx.Done():
		g.release(c)
		var zero V
		return zero, ctx.Err()
	}
}

func (g *Group[K, V]) release(c *call[V]) {
	g.mu.Lock()
	c.waiters--
	g.mu.Unlock()
}

// lead runs the loader for the leader. The deferred cleanup is the whole point:
// it deletes the map entry and closes done on every exit, and recovers a panic
// into c.err, so followers are guaranteed to be released and future callers can
// never attach to a dead entry.
func (g *Group[K, V]) lead(ctx context.Context, key K, c *call[V], load func(context.Context) (V, error)) {
	defer func() {
		if r := recover(); r != nil {
			c.err = &PanicError{Value: r, Stack: debug.Stack()}
		}
		g.mu.Lock()
		delete(g.inflight, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = load(ctx)
}

// NumWaiters reports how many followers are currently attached to the in-flight
// call for key, or 0 if no call is in flight. It exists for observability and
// for tests that need to rendezvous with attached followers deterministically.
func (g *Group[K, V]) NumWaiters(key K) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.inflight[key]; ok {
		return c.waiters
	}
	return 0
}
```

### The runnable demo

The demo shows all four behaviors, and it is deterministic by construction: instead of sleeping to let goroutines line up, it spins on `NumWaiters` and an atomic call counter, so each phase only advances once the exact rendezvous condition holds. The leader parks in `load` until every follower has attached, which makes "the load ran once" a fact about program logic, not about timing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"example.com/singleflight"
)

// spinUntil busy-waits (yielding the P) until cond holds. It replaces a sleep so
// the demo's output is a function of program logic, not of timing.
func spinUntil(cond func() bool) {
	for !cond() {
		runtime.Gosched()
	}
}

func main() {
	ctx := context.Background()

	// 1. Stampede collapse: N concurrent callers for one key, load runs once.
	stampede(ctx)

	// 2. Error path: the error reaches every follower and the entry is deleted,
	//    so a later call re-runs load instead of attaching to a poisoned entry.
	errorPath(ctx)

	// 3. Panic path: a panicking loader becomes an error for every caller and
	//    the group stays usable.
	panicPath(ctx)

	// 4. Cancellation: one follower bails on ctx while the leader keeps running
	//    and the other follower still shares the result.
	cancelPath()
}

func stampede(ctx context.Context) {
	const followers = 8
	var g singleflight.Group[string, int]
	var calls atomic.Int64

	load := func(context.Context) (int, error) {
		calls.Add(1)
		// Do not finish until every follower has attached, so the single flight
		// provably serves all of them.
		spinUntil(func() bool { return g.NumWaiters("k") == followers })
		return 42, nil
	}

	vals := make([]int, followers+1)
	var wg sync.WaitGroup
	wg.Go(func() { vals[0], _ = g.Do(ctx, "k", load) }) // leader
	spinUntil(func() bool { return calls.Load() == 1 })  // leader is in load
	for i := range followers {
		wg.Go(func() { vals[i+1], _ = g.Do(ctx, "k", load) })
	}
	wg.Wait()

	same := true
	for _, v := range vals {
		if v != 42 {
			same = false
		}
	}
	fmt.Printf("stampede: callers=%d loadRuns=%d allEqual42=%v\n", len(vals), calls.Load(), same)
}

var errBackend = errors.New("backend unavailable")

func errorPath(ctx context.Context) {
	const followers = 4
	var g singleflight.Group[string, int]
	var calls atomic.Int64

	load := func(context.Context) (int, error) {
		calls.Add(1)
		spinUntil(func() bool { return g.NumWaiters("e") == followers })
		return 0, errBackend
	}

	errs := make([]error, followers+1)
	var wg sync.WaitGroup
	wg.Go(func() { _, errs[0] = g.Do(ctx, "e", load) })
	spinUntil(func() bool { return calls.Load() == 1 })
	for i := range followers {
		wg.Go(func() { _, errs[i+1] = g.Do(ctx, "e", load) })
	}
	wg.Wait()

	allBackend := true
	for _, e := range errs {
		if !errors.Is(e, errBackend) {
			allBackend = false
		}
	}
	// Entry was deleted, not poisoned: a fresh call re-runs load.
	_, again := g.Do(ctx, "e", func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errBackend
	})
	fmt.Printf("error: allGotBackendErr=%v reran=%v totalLoadRuns=%d\n",
		allBackend, errors.Is(again, errBackend), calls.Load())
}

func panicPath(ctx context.Context) {
	const followers = 4
	var g singleflight.Group[string, int]
	var calls atomic.Int64

	load := func(context.Context) (int, error) {
		calls.Add(1)
		spinUntil(func() bool { return g.NumWaiters("p") == followers })
		panic("loader exploded")
	}

	errs := make([]error, followers+1)
	var wg sync.WaitGroup
	wg.Go(func() { _, errs[0] = g.Do(ctx, "p", load) })
	spinUntil(func() bool { return calls.Load() == 1 })
	for i := range followers {
		wg.Go(func() { _, errs[i+1] = g.Do(ctx, "p", load) })
	}
	wg.Wait()

	allPanicErr := true
	for _, e := range errs {
		var pe *singleflight.PanicError
		if !errors.As(e, &pe) {
			allPanicErr = false
		}
	}
	// The group is not wedged by the panic: a later key loads normally.
	next, _ := g.Do(ctx, "ok", func(context.Context) (int, error) { return 7, nil })
	fmt.Printf("panic: allGotPanicErr=%v groupStillUsable=%v nextValue=%d\n",
		allPanicErr, next == 7, next)
}

func cancelPath() {
	var g singleflight.Group[string, int]
	var calls atomic.Int64
	release := make(chan struct{})

	load := func(context.Context) (int, error) {
		calls.Add(1)
		<-release // leader parks here until the demo lets it finish
		return 99, nil
	}

	type res struct {
		v   int
		err error
	}
	leaderCh := make(chan res, 1)
	goodCh := make(chan res, 1)
	badCh := make(chan res, 1)

	go func() { v, e := g.Do(context.Background(), "c", load); leaderCh <- res{v, e} }()
	spinUntil(func() bool { return calls.Load() == 1 }) // leader in load

	go func() { v, e := g.Do(context.Background(), "c", load); goodCh <- res{v, e} }()
	spinUntil(func() bool { return g.NumWaiters("c") == 1 }) // good follower attached

	cctx, cancel := context.WithCancel(context.Background())
	go func() { v, e := g.Do(cctx, "c", load); badCh <- res{v, e} }()
	spinUntil(func() bool { return g.NumWaiters("c") == 2 }) // bad follower attached

	cancel()
	bad := <-badCh // returns promptly while the leader is still parked

	close(release)
	leader := <-leaderCh
	good := <-goodCh

	fmt.Printf("cancel: cancelledErr=%v leaderVal=%d goodFollowerVal=%d loadRuns=%d\n",
		bad.err, leader.v, good.v, calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stampede: callers=9 loadRuns=1 allEqual42=true
error: allGotBackendErr=true reran=true totalLoadRuns=2
panic: allGotPanicErr=true groupStillUsable=true nextValue=7
cancel: cancelledErr=context canceled leaderVal=99 goodFollowerVal=99 loadRuns=1
```

### Tests

`TestDoRunsLoadExactlyOnce` launches 100 goroutines on one key; the loader holds the flight open until all 99 followers attach (via `NumWaiters`), so a second load could only occur if dedup were broken -- it asserts the atomic counter is 1 and every caller got the value. `TestErrorReachesEveryFollowerAndReRuns` proves the error path releases the herd (every follower gets `errBackend`) and that the entry was deleted, not poisoned, by running a second call and checking the loader ran twice. `TestPanicYieldsErrorAndGroupStaysUsable` confirms a panicking loader delivers a `*PanicError` to all callers with no hang and leaves the group usable for a later key. `TestFollowerCancelDoesNotWedgeLeaderOrPeers` parks the leader, attaches two followers, cancels one, and asserts it returns `context.Canceled` promptly while the leader and the other follower still complete with the shared value. Every test runs under `guard`, a per-test watchdog that dumps all goroutine stacks on timeout, so a missed close fails with a stack trace instead of hanging; `TestMain` wraps the run in `goleak` to catch a leaked follower.

Create `singleflight_test.go`:

```go
package singleflight

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// guard runs fn and fails with a full goroutine dump if it does not return in d,
// turning a partial deadlock (invisible to the runtime) into an actionable stack
// trace instead of a silent hang.
func guard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s did not finish within %s: likely wedged herd.\n\n%s", name, d, buf[:n])
	}
}

// spinUntil busy-waits until cond holds, yielding the P. It rendezvouses the
// test with attached followers without a sleep-based race.
func spinUntil(cond func() bool) {
	for !cond() {
		runtime.Gosched()
	}
}

func TestDoRunsLoadExactlyOnce(t *testing.T) {
	guard(t, 5*time.Second, "exactly-once", func() {
		const n = 100
		var g Group[string, int]
		var calls atomic.Int64

		load := func(context.Context) (int, error) {
			calls.Add(1)
			// Hold the flight open until every other caller has attached, so a
			// second load can only happen if dedup is broken.
			spinUntil(func() bool { return g.NumWaiters("k") == n-1 })
			return 42, nil
		}

		vals := make([]int, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				vals[i], errs[i] = g.Do(context.Background(), "k", load)
			})
		}
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Fatalf("load ran %d times, want exactly 1", got)
		}
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("caller %d got error %v, want nil", i, errs[i])
			}
			if vals[i] != 42 {
				t.Fatalf("caller %d got value %d, want 42", i, vals[i])
			}
		}
		if w := g.NumWaiters("k"); w != 0 {
			t.Fatalf("NumWaiters=%d after completion, want 0", w)
		}
	})
}

var errBackend = errors.New("backend down")

func TestErrorReachesEveryFollowerAndReRuns(t *testing.T) {
	guard(t, 5*time.Second, "error-fanout", func() {
		const n = 32
		var g Group[string, int]
		var calls atomic.Int64

		load := func(context.Context) (int, error) {
			calls.Add(1)
			spinUntil(func() bool { return g.NumWaiters("e") == n-1 })
			return 0, errBackend
		}

		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = g.Do(context.Background(), "e", load) })
		}
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Fatalf("load ran %d times, want exactly 1", got)
		}
		for i := range n {
			if !errors.Is(errs[i], errBackend) {
				t.Fatalf("caller %d got %v, want errBackend", i, errs[i])
			}
		}

		// The failed entry must be deleted, not poisoned: a fresh call re-runs.
		_, err := g.Do(context.Background(), "e", func(context.Context) (int, error) {
			calls.Add(1)
			return 0, errBackend
		})
		if !errors.Is(err, errBackend) {
			t.Fatalf("re-run got %v, want errBackend", err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("load ran %d times total, want 2 (entry was poisoned)", got)
		}
	})
}

func TestPanicYieldsErrorAndGroupStaysUsable(t *testing.T) {
	guard(t, 5*time.Second, "panic-safe", func() {
		const n = 32
		var g Group[string, int]
		var calls atomic.Int64

		load := func(context.Context) (int, error) {
			calls.Add(1)
			spinUntil(func() bool { return g.NumWaiters("p") == n-1 })
			panic("loader exploded")
		}

		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = g.Do(context.Background(), "p", load) })
		}
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Fatalf("load ran %d times, want exactly 1", got)
		}
		for i := range n {
			var pe *PanicError
			if !errors.As(errs[i], &pe) {
				t.Fatalf("caller %d got %v, want *PanicError", i, errs[i])
			}
		}

		// A panic must not wedge the group: a later key loads normally.
		v, err := g.Do(context.Background(), "ok", func(context.Context) (int, error) {
			return 7, nil
		})
		if err != nil || v != 7 {
			t.Fatalf("after panic got (%d, %v), want (7, nil)", v, err)
		}
	})
}

func TestFollowerCancelDoesNotWedgeLeaderOrPeers(t *testing.T) {
	guard(t, 5*time.Second, "cancel-exit", func() {
		var g Group[string, int]
		var calls atomic.Int64
		release := make(chan struct{})

		load := func(context.Context) (int, error) {
			calls.Add(1)
			<-release // leader parks until the test releases it
			return 99, nil
		}

		type res struct {
			v   int
			err error
		}
		leaderCh := make(chan res, 1)
		goodCh := make(chan res, 1)
		badCh := make(chan res, 1)

		go func() { v, e := g.Do(context.Background(), "c", load); leaderCh <- res{v, e} }()
		spinUntil(func() bool { return calls.Load() == 1 })

		go func() { v, e := g.Do(context.Background(), "c", load); goodCh <- res{v, e} }()
		spinUntil(func() bool { return g.NumWaiters("c") == 1 })

		cctx, cancel := context.WithCancel(context.Background())
		go func() { v, e := g.Do(cctx, "c", load); badCh <- res{v, e} }()
		spinUntil(func() bool { return g.NumWaiters("c") == 2 })

		cancel()
		bad := <-badCh // must return without waiting on the parked leader
		if !errors.Is(bad.err, context.Canceled) {
			t.Fatalf("cancelled follower got %v, want context.Canceled", bad.err)
		}

		close(release)
		if leader := <-leaderCh; leader.err != nil || leader.v != 99 {
			t.Fatalf("leader got (%d, %v), want (99, nil)", leader.v, leader.err)
		}
		if good := <-goodCh; good.err != nil || good.v != 99 {
			t.Fatalf("good follower got (%d, %v), want (99, nil)", good.v, good.err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("load ran %d times, want exactly 1", got)
		}
	})
}
```

## Review

Correct here means the leader releases the herd on every exit and no caller can be left blocked. The guarantee is the deferred `delete`-then-`close(done)` in `lead`: it runs on the success return, on an error return, and on a recovered panic, so `done` is always closed and the map entry is always removed -- followers are released and future callers never attach to a dead call. `close(done)` doubles as the happens-before edge for the shared `val`/`err`, which is why the tests are `-race` clean without per-field locking. The four tests pin the four ways this can go wrong: exactly-once dedup (counter is 1 with all followers proven attached), error fan-out plus re-run (deletion, not poisoning), panic-to-error with the group still usable, and a follower's `ctx.Done` exit that leaves the leader and its peers intact. The production bug this prevents is the silent one -- a loader that returns `err` on a backend timeout, or panics on a malformed row, without closing the channel, so every request that stampeded the expired key hangs until the load balancer times out and someone is paged for latency with nothing in the logs. The watchdog and `goleak` turn that hang into a failed test with a stack trace instead of a 3 a.m. page.

## Resources

- [singleflight package](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the canonical implementation this exercise reconstructs and hardens; compare its `doCall` panic handling.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) -- why a deferred recover is the tool for making a panicking callback safe on the leader path.
- [context package](https://pkg.go.dev/context) -- the cancellation contract behind the follower's `ctx.Done` exit.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- detecting a leaked follower goroutine that a missed `close` would leave behind.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-dead-letter-feedback-cycle-break.md](13-dead-letter-feedback-cycle-break.md) | Next: [../15-building-a-concurrent-task-scheduler/00-concepts.md](../15-building-a-concurrent-task-scheduler/00-concepts.md)
