# Exercise 14: Cache-Stampede Suppression via a Completion-Broadcast Done Channel

**Level: Advanced**

When a hot cache key expires, hundreds of in-flight requests can miss at once and stampede the backing database with identical loads -- the same expensive query, run N times, for one answer. The fix is to coalesce concurrent calls for the same key into a single backend fetch: the first caller becomes the leader and runs the load, while every later caller blocks on a shared per-key completion channel that the leader closes when it finishes. Because a closed channel is always ready to every receiver, one close broadcasts the single result to all N waiters at once. This exercise builds that suppressor, including the harder detail: a waiter whose own request is cancelled must stop waiting and return without leaking and without disturbing the leader.

This module is self-contained: its own module, a `singleflight` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
singleflight/                independent module: example.com/singleflight
  go.mod                     go 1.26
  singleflight.go            type Group with Do(done, key, fn) (v, err, shared)
  cmd/demo/main.go           runnable demo: N callers coalesce onto one leader; fn runs once
  singleflight_test.go       broadcast-once, canceled-waiter, distinct-keys, goleak TestMain
```

- Files: `singleflight.go`, `cmd/demo/main.go`, `singleflight_test.go`.
- Implement: `type Group struct{ ... }` and `func (g *Group) Do(done <-chan struct{}, key string, fn func() (any, error)) (v any, err error, shared bool)`.
- Test: N concurrent calls invoke `fn` exactly once and all receive the identical value; a canceled waiter returns `ErrCanceled`, is not shared, and does not block the leader; distinct keys run concurrently; no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak@v1.3.0
```

### One close, N wakeups: the leader/waiter protocol

The whole design turns on the property from the lesson: a receive on a closed channel never blocks and yields the zero value to *every* receiver, forever. So if all N waiters `select` on the same `c.done` channel, one `close(c.done)` wakes all N at once -- there is no value to deliver once, only a latch every observer sees. That is what makes a single leader's completion fan out to an unbounded crowd of waiters without a per-waiter send.

The protocol, all serialized by one mutex, is:

1. A caller locks `g.mu` and looks up `key` in `g.calls`.
2. If no call is in flight, this caller is the **leader**: it creates a `*call` holding a fresh `done` channel, stores it under `key`, unlocks, and runs `fn`. It is the only goroutine that will run `fn` for this key.
3. If a call *is* in flight, this caller is a **waiter**: it marks the call `shared`, unlocks, and blocks in `select { case <-c.done: ...; case <-done: ... }`. The first branch is the broadcast; the second is its own caller's cancellation.
4. When `fn` returns, the leader writes `c.val`/`c.err`, then under the lock deletes `key` (so the next caller starts a fresh load) and reads the `shared` flag, then `close(c.done)`.

The ownership rule is strict and it is why the pattern is safe: the leader is the sole creator and sole closer of `c.done`, and waiters receive it only as a read (`<-c.done`). No waiter can close a channel it did not create, and there is exactly one close, so there is no double-close panic. The memory ordering is equally deliberate: the leader's writes to `c.val`/`c.err` happen-before `close(c.done)`, which happens-before each waiter's receive, so every waiter reads the leader's result without a data race even though many goroutines touch the same fields.

The cancellation branch is the leak-avoidance rule from the lesson applied to a *receive* instead of a send. A waiter that only did `<-c.done` would be parked forever if its caller walked away and the leader were slow; selecting the caller's `done` against `c.done` lets the waiter return `ErrCanceled` immediately and reclaim its goroutine. Crucially, leaving is a purely local act: the waiter never touches `c`, never signals the leader, so the leader keeps running and still broadcasts to everyone who stayed. A canceled waiter is not marked shared on its own return, though it did join the call, so the leader still reports `shared == true`.

Create `singleflight.go`:

```go
// Package singleflight coalesces concurrent calls for the same key into a
// single execution, broadcasting the one result to every caller by closing a
// shared per-key completion channel.
package singleflight

import (
	"errors"
	"sync"
)

// ErrCanceled is returned to a waiter whose own done channel closes before the
// in-flight call it joined completes.
var ErrCanceled = errors.New("singleflight: caller canceled before result was ready")

// call is one in-flight execution. The leader writes val and err, then closes
// done to broadcast completion to every waiter. done carries no value: a closed
// channel is always ready to every receiver, so a single close wakes all N.
type call struct {
	done   chan struct{} // closed by the leader when fn returns
	val    any
	err    error
	shared bool // set under Group.mu whenever a waiter coalesces onto this call
}

// Group coalesces concurrent Do calls that share a key.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call

	// onJoin, when non-nil, is invoked once for each caller that coalesces onto
	// an in-flight call (never for the leader). It is a test/observability hook
	// that lets a test synchronize on registration instead of sleeping; it is
	// nil in production.
	onJoin func()
}

// Do coalesces concurrent calls for the same key. The first caller becomes the
// leader and runs fn; every later caller for that key blocks on the leader's
// shared completion channel, which the leader closes when fn returns, so one
// close broadcasts the single (value, err) to all waiters at once. A waiter
// whose own done closes first returns ErrCanceled immediately without waiting
// for fn and without disturbing the leader. shared is true for coalesced
// waiters and for the leader when at least one waiter joined; a canceled waiter
// is not marked shared.
func (g *Group) Do(done <-chan struct{}, key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*call)
	}
	if c, ok := g.calls[key]; ok {
		// Waiter: a call for this key is already in flight. Coalesce onto it.
		c.shared = true
		hook := g.onJoin
		g.mu.Unlock()
		if hook != nil {
			hook()
		}
		// Block until the leader broadcasts (closes c.done) or our own caller
		// gives up (done closes). The closed-channel broadcast is what lets one
		// close wake every waiter; the done branch is what stops a canceled
		// waiter from leaking a goroutine parked forever on c.done.
		select {
		case <-c.done:
			return c.val, c.err, true
		case <-done:
			return nil, ErrCanceled, false
		}
	}

	// Leader: no call in flight for this key. Register ours, run fn, broadcast.
	c := &call{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()

	// Remove the key before broadcasting so the next caller starts a fresh call,
	// and read the shared flag under the same lock that waiters wrote it under.
	g.mu.Lock()
	delete(g.calls, key)
	shared = c.shared
	g.mu.Unlock()

	// One close broadcasts c.val/c.err to every waiter blocked on c.done. The
	// writes above happen-before this close, which happens-before each waiter's
	// receive, so every waiter observes the leader's result without a data race.
	close(c.done)
	return c.val, c.err, shared
}
```

### The runnable demo

The demo is deterministic by construction. The leader's `fn` blocks on `release`, so the hot key stays in flight the whole time and every caller that arrives during that window coalesces onto the one leader. The waiters here arrive with an *already-closed* done, so each returns `ErrCanceled` the instant it coalesces -- and returning `ErrCanceled` is itself proof it coalesced, because a caller that found no in-flight call would have become a second leader and run `fn`. Only after every waiter has reported back does the demo release the leader, so print order never varies.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/singleflight"
)

// This demo is deterministic. The leader's fn blocks on release, so the hot key
// stays in flight the whole time; every caller that arrives during that window
// coalesces onto the one leader. The waiters here arrive with an already-closed
// done, so each returns ErrCanceled the instant it coalesces -- and returning
// ErrCanceled proves it coalesced (a caller that found no in-flight call would
// have become a second leader and run fn instead). We only release the leader
// after all waiters have reported back, so print order never varies.
func main() {
	var g singleflight.Group
	var calls atomic.Int64

	const waiters = 4
	const key = "user:42"

	started := make(chan struct{})
	release := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		close(started)  // leader is registered and about to block
		<-release       // hold the key in flight until every waiter reports
		return "row(user:42)", nil
	}

	// Leader: nil done means the leader never cancels; it runs fn to completion.
	leaderCh := make(chan struct {
		v      any
		err    error
		shared bool
	}, 1)
	go func() {
		v, err, shared := g.Do(nil, key, fn)
		leaderCh <- struct {
			v      any
			err    error
			shared bool
		}{v, err, shared}
	}()

	<-started

	// Fan in the coalesced waiters. Each brings a pre-closed done, so it joins
	// the leader and then immediately returns ErrCanceled.
	canceled := make(chan bool, waiters)
	for range waiters {
		go func() {
			dead := make(chan struct{})
			close(dead)
			_, err, shared := g.Do(dead, key, fn)
			canceled <- (err == singleflight.ErrCanceled && !shared)
		}()
	}

	allCanceled := true
	for range waiters {
		if !<-canceled {
			allCanceled = false
		}
	}

	// Every waiter has coalesced and returned; now let the leader finish.
	close(release)
	res := <-leaderCh

	fmt.Println("fn invocations:      ", calls.Load())
	fmt.Println("leader value:        ", res.v)
	fmt.Println("leader err:          ", res.err)
	fmt.Println("leader shared:       ", res.shared)
	fmt.Printf("waiters coalesced:    %d/%d\n", waiters, waiters)
	fmt.Println("all waiters canceled:", allCanceled)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fn invocations:       1
leader value:         row(user:42)
leader err:           <nil>
leader shared:        true
waiters coalesced:    4/4
all waiters canceled: true
```

### Tests

`TestBroadcastsOneResultToAllWaiters` launches N concurrent `Do` calls for one key whose `fn` blocks on a gate, and pins the core guarantee: `fn` runs exactly once (`calls == 1`) and, once the leader closes the completion channel, every one of the N callers receives the identical value with `shared == true`. It synchronizes on the `onJoin` hook -- not a sleep -- so it opens the gate only after all N-1 waiters have provably coalesced. `TestCanceledWaiterReturnsWithoutBlockingLeader` gives a waiter an already-closed done and asserts it returns `ErrCanceled`, is not marked shared, and that the leader still completes with its value for everyone else. `TestDistinctKeysRunConcurrently` proves keys do not coalesce across one another: two keys' `fn`s are in flight simultaneously and both run. `TestMain` installs `goleak.VerifyTestMain`, so any leaked leader or waiter goroutine fails the package.

Create `singleflight_test.go`:

```go
package singleflight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaks a goroutine, proving that neither
// the leader nor any waiter (canceled or not) is left parked.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestBroadcastsOneResultToAllWaiters pins the core guarantee: N concurrent Do
// calls for one key invoke fn exactly once, and closing the leader's completion
// channel broadcasts the identical (value, shared) to every caller at once.
func TestBroadcastsOneResultToAllWaiters(t *testing.T) {
	const n = 64
	const key = "user:42"

	var g Group
	var calls atomic.Int64

	var joined sync.WaitGroup
	joined.Add(n - 1) // exactly one caller leads; the other n-1 coalesce
	g.onJoin = func() { joined.Done() }

	started := make(chan struct{})
	release := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		close(started) // leader is registered and about to block
		<-release
		return 42, nil
	}

	type res struct {
		v      any
		err    error
		shared bool
	}
	results := make(chan res, n)
	for range n {
		go func() {
			v, err, shared := g.Do(nil, key, fn)
			results <- res{v, err, shared}
		}()
	}

	waitClosed(t, started, "leader to enter fn")
	joined.Wait()  // every waiter has coalesced onto the leader's call
	close(release) // one close broadcasts to all n callers

	got := make([]res, 0, n)
	for range n {
		got = append(got, <-results)
	}

	if calls.Load() != 1 {
		t.Fatalf("fn invoked %d times, want exactly 1 (calls were not coalesced)", calls.Load())
	}
	for i, r := range got {
		if r.err != nil {
			t.Fatalf("caller %d: err = %v, want nil", i, r.err)
		}
		if r.v != 42 {
			t.Fatalf("caller %d: value = %v, want 42 (broadcast delivered a different value)", i, r.v)
		}
		if !r.shared {
			t.Fatalf("caller %d: shared = false, want true (leader and waiters are all shared here)", i)
		}
	}
}

// TestCanceledWaiterReturnsWithoutBlockingLeader pins the cancellation contract:
// a waiter whose own done closes first returns ErrCanceled immediately, is not
// marked shared, and does not stop the leader from completing for everyone else.
func TestCanceledWaiterReturnsWithoutBlockingLeader(t *testing.T) {
	const key = "user:7"

	var g Group
	var calls atomic.Int64

	started := make(chan struct{})
	release := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		close(started)
		<-release
		return "row", nil
	}

	leaderDone := make(chan struct{})
	var lv any
	var lerr error
	var lshared bool
	go func() {
		lv, lerr, lshared = g.Do(nil, key, fn)
		close(leaderDone)
	}()

	waitClosed(t, started, "leader to enter fn")

	// A waiter whose caller has already given up.
	dead := make(chan struct{})
	close(dead)
	v, err, shared := g.Do(dead, key, fn)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled waiter err = %v, want ErrCanceled", err)
	}
	if shared {
		t.Fatal("canceled waiter marked shared = true, want false")
	}
	if v != nil {
		t.Fatalf("canceled waiter value = %v, want nil", v)
	}

	// The leader was never disturbed and still completes for the others.
	close(release)
	waitClosed(t, leaderDone, "leader to finish")
	if lerr != nil {
		t.Fatalf("leader err = %v, want nil", lerr)
	}
	if lv != "row" {
		t.Fatalf("leader value = %v, want \"row\"", lv)
	}
	if calls.Load() != 1 {
		t.Fatalf("fn invoked %d times, want exactly 1", calls.Load())
	}
	if !lshared {
		t.Fatal("leader shared = false, want true (a waiter joined the call)")
	}
}

// TestDistinctKeysRunConcurrently proves keys do not coalesce across one
// another: two keys' fns are in flight at the same time and both run.
func TestDistinctKeysRunConcurrently(t *testing.T) {
	var g Group
	var calls atomic.Int64

	release := make(chan struct{})
	mkFn := func(started chan struct{}, ret any) func() (any, error) {
		return func() (any, error) {
			calls.Add(1)
			close(started)
			<-release
			return ret, nil
		}
	}

	startedA := make(chan struct{})
	startedB := make(chan struct{})
	resA := make(chan any, 1)
	resB := make(chan any, 1)
	go func() {
		v, _, _ := g.Do(nil, "A", mkFn(startedA, "a"))
		resA <- v
	}()
	go func() {
		v, _, _ := g.Do(nil, "B", mkFn(startedB, "b"))
		resB <- v
	}()

	// Both fns must be in flight simultaneously; if distinct keys serialized,
	// one of these would never fire and the test would time out.
	waitClosed(t, startedA, "key A to enter fn")
	waitClosed(t, startedB, "key B to enter fn")
	close(release)

	if got := <-resA; got != "a" {
		t.Fatalf("key A value = %v, want \"a\"", got)
	}
	if got := <-resB; got != "b" {
		t.Fatalf("key B value = %v, want \"b\"", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("fn invoked %d times, want 2 (one per distinct key)", calls.Load())
	}
}
```

## Review

Correct here means a single load per key under any amount of concurrency, and it is guaranteed by two invariants working together: the mutex makes exactly one caller the leader (the first to find no entry under `key`), and the leader is the sole closer of a per-key `done` channel that every waiter only reads. Closing that channel once is a broadcast -- a closed channel is permanently ready to every receiver -- so the leader's single result reaches all N waiters without a per-waiter handoff, and the close ordering (write result, delete key, then close) gives every waiter a race-free view of `val`/`err`. The broadcast test proves the coalescing directly: `fn` runs exactly once while N callers each come away with the identical value. The cancellation test proves the leak-avoidance half: a waiter selects its caller's `done` against `c.done`, so it returns `ErrCanceled` and reclaims its goroutine the moment its request is abandoned, and because leaving never touches the shared call the leader keeps serving everyone who stayed -- which `goleak` confirms leaves nothing parked. The production bug this prevents is the cache stampede: without suppression, one popular key expiring turns a burst of reads into a burst of identical, expensive backend loads that can topple the very database the cache exists to protect.

## Resources

- [pkg.go.dev: golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the production-grade version of this exercise; compare its `Do`/`DoChan`/`Forget` surface to the minimal one built here.
- [Go Blog: Go Concurrency Patterns -- Pipelines and cancellation](https://go.dev/blog/pipelines) -- the source of the select-on-done leak-avoidance rule the waiter uses.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector the TestMain uses to prove no leader or waiter goroutine is left behind.
- [The Go Memory Model](https://go.dev/ref/mem) -- why the leader's writes before `close(c.done)` are visible to each waiter's receive.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-two-replica-request-hedging-cancel-loser.md](13-two-replica-request-hedging-cancel-loser.md) | Next: [../08-goroutine-leak-detection/00-concepts.md](../08-goroutine-leak-detection/00-concepts.md)
