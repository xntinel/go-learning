# Exercise 25: Request Coalescing — Multiple Goroutines Defer Broadcast of Shared Result

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache stampede happens when a hot key expires and dozens of goroutines
notice at the same instant, each deciding independently that it must be
the one to recompute the value: the backend that was serving one request
per second is suddenly hit with fifty concurrent, identical, expensive
calls. The fix is request coalescing: the first caller for a key becomes
the leader and actually does the work, every other concurrent caller for
that same key becomes a follower and simply waits for the leader's result
instead of duplicating it. This module builds that coalescing `Group`
around a single deferred closure that runs once the leader's work
completes and hands the shared result to every follower that joined while
it was running. The module is fully self-contained: its own `go mod
init`, all code inline, its own demo and tests.

## What you'll build

```text
coalesce/                   independent module: example.com/request-coalescing-singleflight-deferred
  go.mod                     go 1.24
  coalesce.go                Result, Group (Do, Waiting)
  cmd/
    demo/
      main.go                runnable demo: 5 concurrent callers, one execution of fn
  coalesce_test.go            coalescing under concurrency; sequential calls are not coalesced
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: `Result` (`Value`, `Err`) and `Group` with `Do(key string, fn func() (string, error)) Result` and `Waiting(key string) int`.
- Test: a concurrency case proving `fn` runs exactly once for many simultaneous callers, plus a sequential case proving calls that do not overlap are not coalesced.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/25-request-coalescing-singleflight-deferred/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/25-request-coalescing-singleflight-deferred
go mod edit -go=1.24
```

### Why the broadcast has to be a deferred closure, not a deferred argument

`Do` registers every caller — leader and followers alike — on a shared
wait list for `key` before it knows whether it is the leader. If it is a
follower, it simply blocks on its own channel and returns whatever the
leader eventually sends. If it is the leader, it defers exactly one
closure and then calls `fn`. That closure has to be a closure, not a
plain `defer broadcast(g.pending[key])` argument form: the argument form
would evaluate `g.pending[key]` immediately, at the point the `defer`
statement runs — which is the instant *before* `fn` has even started —
and would freeze the wait list at whatever it contained then (just the
leader's own entry). Every follower that joins afterward, while `fn` is
still running, would register on a list nobody is holding a reference to
anymore, and would block forever waiting for a broadcast that already
happened without them. The closure form defers reading `g.pending[key]`
until the moment it actually runs — after `fn` has returned — which is
exactly when the wait list is complete.

The broadcast loop walks that list back-to-front: LIFO order, the same
ordering `defer` itself uses. This has no effect on which followers wake
up or when a *given* follower notices its result — each one owns a
dedicated buffered channel, so the sends do not block on each other and
do not need to happen in any particular order for correctness. It does,
however, mean the goroutine that joined most recently — the one that
waited the shortest amount of wall-clock time for the leader to finish —
is the first one the closure's loop actually touches, mirroring how a
stack of deferred calls always unwinds starting from the most recently
registered one.

Create `coalesce.go`:

```go
package coalesce

import "sync"

// Result is the shared outcome of a coalesced call: every goroutine that
// asked for the same key while a call was in flight receives an identical
// copy of it.
type Result struct {
	Value string
	Err   error
}

// Group coalesces concurrent requests for the same key into a single
// execution of fn.
type Group struct {
	mu      sync.Mutex
	pending map[string][]chan Result
}

// Do runs fn at most once among all concurrent callers sharing key. The
// first caller for a key becomes the leader and actually invokes fn; every
// caller that arrives before the leader's fn returns becomes a follower
// and is registered on the same wait list instead of calling fn itself.
func (g *Group) Do(key string, fn func() (string, error)) (result Result) {
	g.mu.Lock()
	if g.pending == nil {
		g.pending = make(map[string][]chan Result)
	}
	existing, inFlight := g.pending[key]
	ch := make(chan Result, 1)
	g.pending[key] = append(existing, ch)
	isLeader := !inFlight
	g.mu.Unlock()

	if !isLeader {
		return <-ch
	}

	// The leader's deferred closure must read g.pending[key] at return
	// time, not now: followers keep appending to that slice for as long
	// as fn (below) is still running. Capturing the slice as a plain
	// defer argument right here would freeze it at "1 waiter" -- the
	// leader's own channel -- before any follower has had a chance to
	// join, and every follower would then hang forever on a channel
	// nobody sends to.
	defer func() {
		g.mu.Lock()
		joined := g.pending[key]
		delete(g.pending, key)
		g.mu.Unlock()

		// Broadcast in LIFO order: the most recently joined follower --
		// the one that arrived closest to fn actually finishing, and so
		// waited the shortest -- is notified first. Each follower reads
		// from its own dedicated buffered channel, so send order here
		// does not change who wakes up, only the order these particular
		// send statements execute in.
		for i := len(joined) - 1; i >= 0; i-- {
			joined[i] <- result
		}
	}()

	result.Value, result.Err = fn()
	return result
}

// Waiting reports how many callers are currently coalesced onto key --
// useful both as a production stampede-depth metric and, in tests and
// demos, as a way to wait for every expected caller to have joined an
// in-flight call before proceeding, without guessing at scheduling delays.
func (g *Group) Waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending[key])
}
```

### The runnable demo

Five goroutines all ask for the same report at once. `fetch` blocks on a
`release` channel so the demo can wait — via `Waiting`, not a guess about
scheduling — until all five have actually joined the same in-flight call
before letting it complete. That is what makes the "executed 1 time" line
below a guarantee instead of a race.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"example.com/request-coalescing-singleflight-deferred"
)

func main() {
	var g coalesce.Group
	var calls int32
	release := make(chan struct{})

	const n = 5

	// fetch blocks on release until every one of the n callers below has
	// registered on the same key, so the demo deterministically shows all
	// of them coalescing onto the single in-flight call instead of racing
	// against how fast the scheduler happens to run each one.
	fetch := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return "expensive-report-v1", nil
	}

	results := make([]coalesce.Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = g.Do("report", fetch)
		}(i)
	}

	for g.Waiting("report") != n {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	fmt.Printf("fn executed %d time(s) for %d concurrent callers\n", calls, n)
	for i, r := range results {
		fmt.Printf("caller %d got value=%q err=%v\n", i, r.Value, r.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
fn executed 1 time(s) for 5 concurrent callers
caller 0 got value="expensive-report-v1" err=<nil>
caller 1 got value="expensive-report-v1" err=<nil>
caller 2 got value="expensive-report-v1" err=<nil>
caller 3 got value="expensive-report-v1" err=<nil>
caller 4 got value="expensive-report-v1" err=<nil>
```

### Tests

`TestGroupCoalescesConcurrentCallers` uses the same `release`-plus-`Waiting`
technique as the demo to deterministically confirm 20 concurrent callers
produce exactly one execution of `fn`, no matter how the scheduler
interleaves them. `TestGroupRunsAgainAfterPreviousCallCompletes` is the
contrasting edge case: two calls that do not overlap in time are two
separate executions, proving `Do` coalesces *concurrent* callers only —
it is not a cache.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestGroupCoalescesConcurrentCallers proves fn runs once no matter how
// many goroutines ask for the same key while it is in flight. fn blocks on
// release, so the leader cannot finish (and delete the pending entry)
// until the test says so. Rather than guessing how long to wait for every
// follower to register, the test polls Waiting -- the exact state Do
// itself mutates under its mutex -- so it only closes release once it has
// directly confirmed all n callers have joined the same in-flight call.
func TestGroupCoalescesConcurrentCallers(t *testing.T) {
	var g Group
	var calls int32
	release := make(chan struct{})

	fn := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return "v", nil
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := g.Do("k", fn)
			if r.Value != "v" || r.Err != nil {
				t.Errorf("got %+v, want value=v err=nil", r)
			}
		}()
	}

	for g.Waiting("k") != n {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fn called %d times, want 1", got)
	}
}

func TestGroupRunsAgainAfterPreviousCallCompletes(t *testing.T) {
	var g Group
	var calls int32
	fn := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		return "v", nil
	}

	g.Do("k", fn)
	g.Do("k", fn)

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fn called %d times, want 2 (sequential calls are not coalesced)", got)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`Do` is correct when every follower that genuinely overlapped with the
leader's `fn` call receives that exact result, and never blocks forever.
The deferred closure is what makes this possible: reading the wait list
at return time, not at the point the `defer` statement was reached,
means it always sees the complete set of joiners, including ones that
arrived while `fn` was still running. The mistake this design avoids is
using a plain argument-form defer for the broadcast — `defer
broadcast(g.pending[key])` — which would snapshot an incomplete wait list
before any follower had joined and leave every one of them hanging on a
channel nobody sends to.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — function value, receiver, and arguments are evaluated when the defer statement executes; only the call is postponed.
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the standard library-adjacent package this exercise's `Group` is a minimal model of.
- [Cloudflare Blog: Sometimes a service becomes so popular...](https://blog.cloudflare.com/a-question-of-timing/) — real-world discussion of cache stampede failure modes coalescing is designed to prevent.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-wal-deferred-flush-named-return.md](24-wal-deferred-flush-named-return.md) | Next: [26-semaphore-bounded-resource-acquire.md](26-semaphore-bounded-resource-acquire.md)
