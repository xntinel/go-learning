# Exercise 27: Request Coalescing: Shared Panic Boundary for Concurrent Requesters

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cache-miss stampede — a hundred requests for the same cold cache key
arriving within milliseconds of each other — is exactly the scenario
`golang.org/x/sync/singleflight` was built to solve: coalesce concurrent
callers asking for the same key into a single upstream call, and hand every
caller the same result. The subtle failure mode a from-scratch
implementation must get right is what happens when the one goroutine
actually doing the work panics instead of returning cleanly: every other
goroutine blocked waiting for that result must still be released with a
consistent, well-formed error, not left hanging forever because the
goroutine they were counting on to wake them up never got the chance. This
module builds `Group.Do`, the coalescing primitive itself, entirely from
`sync.Mutex` and `sync.Cond`. It is fully self-contained: its own module,
demo, and tests.

## What you'll build

```text
coalesce/                   independent module: example.com/coalesce
  go.mod                     go 1.24
  coalesce.go                 Group, NewGroup, Do, Waiting, runFn
  cmd/
    demo/
      main.go                runnable demo: 5 concurrent callers, the computing one panics
  coalesce_test.go             fn runs once, panic reaches every waiter, key reusable after failure
```

Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
Implement: `Group.Do(key string, fn func() (any, error)) (any, error)` that, under a shared mutex, either joins an in-flight call for key via a `sync.Cond` or starts a fresh one; the fresh call's `fn` runs outside the lock and its panic is isolated and converted into an error shared by every joined waiter.
Test: two goroutines requesting the same key where the computing goroutine blocks until a waiter has actually joined, asserting `fn` runs exactly once; four waiters all receiving the identical panic-derived error with no deadlock; the key being usable again immediately after a failed call.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/27-request-coalescing-singleflight/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/27-request-coalescing-singleflight
go mod edit -go=1.24
```

### Why the check-and-join must be one critical section, and why a Waiting hook beats a sleep

`Do` locks `g.mu`, then checks `g.calls[key]` and either joins the existing
call or registers a new one — all before ever unlocking. If the check ("is
there already a call for this key?") and the act ("join it" or "register a
fresh one") were split across two separate lock acquisitions, two goroutines
racing on the same key could both see no in-flight call and both start
their own `fn`, defeating coalescing entirely and, worse, doing the
expensive work twice concurrently. Keeping check-then-act inside one
critical section is what makes "at most one `fn` running per key" an actual
guarantee rather than a probabilistic one. The fresh call's `fn` itself runs
*outside* the lock — an expensive fetch must never hold `g.mu` for its
whole duration, or every other key's callers would be blocked behind it too
- and the result is written back and broadcast to waiters under the lock
again once `fn` returns.

Building a demo or a test that *proves* the panic reaches every waiter
without deadlocking, deterministically, is harder than it looks: a caller
cannot observe from the outside whether a waiting goroutine has actually
reached `cond.Wait()` yet. Releasing the computing goroutine's blocking gate
the instant the waiter goroutines are merely *launched* (rather than
*registered as waiters*) is a real race — if the computing goroutine
finishes and deletes the key before a slow-to-schedule waiter even calls
`Do`, that waiter finds no in-flight call and wrongly starts a duplicate
one. `Group` exposes `Waiting(key string) int`, which reports how many
callers are currently blocked on the in-flight call for a key purely so
tests and demos can poll until every expected waiter has actually joined
before triggering the next step — a correctness-preserving synchronization
tool, not a workaround for a race that should not exist in `Do` itself.

Create `coalesce.go`:

```go
package coalesce

import (
	"fmt"
	"sync"
)

// call is one in-flight (or just-finished) invocation shared by every
// caller requesting the same key.
type call struct {
	cond    *sync.Cond
	done    bool
	value   any
	err     error
	waiters int // callers currently blocked in cond.Wait for this call
}

// Group coalesces concurrent callers requesting the same key so an
// expensive operation (a cache-miss fetch, a cold computation) runs at most
// once at a time per key, no matter how many goroutines ask for it
// simultaneously.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// NewGroup returns a ready-to-use Group. Share it by pointer - never copy a
// Group by value once it has been used, since its embedded sync.Mutex must
// not be copied after first use.
func NewGroup() *Group {
	return &Group{calls: make(map[string]*call)}
}

// Do executes fn for key at most once concurrently. If a call for key is
// already in flight, the caller blocks on that call's eventual result via a
// condition variable instead of starting a duplicate, expensive fn. The
// check ("is a call for key already running?") and the wait both happen
// under the same Group.mu critical section, so no two callers can ever both
// decide to start a fresh call for the same key.
//
// If fn panics, the single goroutine actually running it recovers the
// panic and converts it into an error; every caller waiting on that
// key - not just whichever happened to be first - receives that same
// error, and the key is removed so a later call can retry instead of being
// stuck replaying a permanently poisoned result.
func (g *Group) Do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		c.waiters++
		for !c.done {
			c.cond.Wait()
		}
		c.waiters--
		g.mu.Unlock()
		return c.value, c.err
	}

	c := &call{}
	c.cond = sync.NewCond(&g.mu)
	g.calls[key] = c
	g.mu.Unlock()

	value, err := runFn(fn)

	g.mu.Lock()
	c.value, c.err, c.done = value, err, true
	delete(g.calls, key)
	c.cond.Broadcast()
	g.mu.Unlock()

	return value, err
}

// Waiting reports how many callers are currently blocked waiting on the
// in-flight call for key. Do's own correctness never depends on this - it
// exists so callers driving concurrent scenarios deterministically (tests,
// demos) can poll until every expected waiter has actually joined the
// in-flight call before triggering the next step, instead of guessing with
// a sleep and hoping the scheduler cooperates.
func (g *Group) Waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.waiters
	}
	return 0
}

// runFn is the recover boundary: exactly one in-flight call's untrusted fn.
func runFn(fn func() (any, error)) (value any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("call panicked: %w", e)
				return
			}
			err = fmt.Errorf("call panicked: %v", r)
		}
	}()
	return fn()
}
```

### The runnable demo

Five goroutines request `"weather:nyc"` at once. Goroutine 0 wins the race
to start the call and signals `started` once it has registered, guaranteeing
the other four find it already in flight. Nothing releases the computing
goroutine's gate until all four have actually joined as waiters (via
`Waiting`), so the panic it eventually raises is guaranteed to reach every
one of them.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"

	"example.com/coalesce"
)

func main() {
	group := coalesce.NewGroup()

	started := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]error, 5)

	// Goroutine 0 starts the in-flight call for the key and signals once it
	// has begun, so the other 4 goroutines are guaranteed to find the call
	// already in flight instead of racing to start their own.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := group.Do("weather:nyc", func() (any, error) {
			close(started)
			<-release
			var resp *struct{ TempC int }
			return resp.TempC, nil // nil pointer dereference
		})
		results[0] = err
	}()

	<-started
	for i := 1; i < 5; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := group.Do("weather:nyc", func() (any, error) {
				panic("must never run: duplicate fetch for a coalesced key")
			})
			results[i] = err
		}()
	}

	// Do not release the first caller until all 4 others have actually
	// joined its in-flight call - otherwise the first caller could finish
	// and clean up the key before a slow-to-schedule waiter ever looks for
	// it, and that waiter would wrongly start its own duplicate call.
	for group.Waiting("weather:nyc") < 4 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	for i, err := range results {
		fmt.Printf("caller %d error: %v\n", i, err)
	}

	same := true
	for _, err := range results[1:] {
		if err.Error() != results[0].Error() {
			same = false
		}
	}
	fmt.Println("all callers received the same error:", same)

	// The key is cleaned up after a failed call, so a retry works normally.
	value, err := group.Do("weather:nyc", func() (any, error) {
		return 21, nil
	})
	fmt.Printf("retry: value=%v err=%v\n", value, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
caller 0 error: call panicked: runtime error: invalid memory address or nil pointer dereference
caller 1 error: call panicked: runtime error: invalid memory address or nil pointer dereference
caller 2 error: call panicked: runtime error: invalid memory address or nil pointer dereference
caller 3 error: call panicked: runtime error: invalid memory address or nil pointer dereference
caller 4 error: call panicked: runtime error: invalid memory address or nil pointer dereference
all callers received the same error: true
retry: value=21 err=<nil>
```

### Tests

`TestDoRunsFnExactlyOnce` uses the same `started`/`Waiting`-gated pattern as
the demo to prove `fn` runs exactly once even with a second caller racing
in. `TestDoPropagatesPanicToAllWaiters` drives four waiters against one
panicking call and asserts every one of them receives the identical wrapped
error within a bounded timeout, so a real deadlock fails the test instead of
hanging the whole suite. `TestDoCleansUpKeyAfterCompletion` confirms a
failed call's key is immediately reusable.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRunsFnExactlyOnce(t *testing.T) {
	g := NewGroup()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32

	var wg sync.WaitGroup
	results := make([]int, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		v, _ := g.Do("k", func() (any, error) {
			atomic.AddInt32(&calls, 1)
			close(started)
			<-release
			return 42, nil
		})
		results[0] = v.(int)
	}()

	<-started
	for i := 1; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := g.Do("k", func() (any, error) {
				atomic.AddInt32(&calls, 1)
				return -1, nil
			})
			results[i] = v.(int)
		}()
	}

	for g.Waiting("k") < 2 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fn was called %d times, want exactly 1", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("results[%d] = %d, want 42 (the coalesced call's value)", i, v)
		}
	}
}

func TestDoPropagatesPanicToAllWaiters(t *testing.T) {
	g := NewGroup()
	started := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	errs := make([]error, 4)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := g.Do("k", func() (any, error) {
			close(started)
			<-release
			panic(errors.New("upstream unavailable"))
		})
		errs[0] = err
	}()

	<-started
	for i := 1; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.Do("k", func() (any, error) {
				t.Error("waiter must never run its own fn for an in-flight key")
				return nil, nil
			})
			errs[i] = err
		}()
	}

	done := make(chan struct{})
	go func() {
		for g.Waiting("k") < 3 {
			runtime.Gosched()
		}
		close(release)
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters deadlocked instead of being released by the panicking call")
	}

	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "upstream unavailable") {
			t.Fatalf("errs[%d] = %v, want it to wrap upstream unavailable", i, err)
		}
	}
}

func TestDoCleansUpKeyAfterCompletion(t *testing.T) {
	g := NewGroup()

	_, err := g.Do("k", func() (any, error) { panic("boom") })
	if err == nil {
		t.Fatal("first Do err = nil, want a panic error")
	}

	v, err := g.Do("k", func() (any, error) { return "fresh", nil })
	if err != nil || v.(string) != "fresh" {
		t.Fatalf("second Do = (%v, %v), want a clean retry to run fn again", v, err)
	}
}
```

## Review

`Do` is correct when starting a fresh call and joining an in-flight one are
mutually exclusive decisions made atomically under one lock, when the
expensive `fn` never runs while holding that lock, and when a panic in `fn`
is guaranteed to reach every joined waiter through `Broadcast` rather than
`Signal` (which would wake only one waiter, leaving the rest hanging). The
`Waiting` hook is the detail most likely to be skipped under time pressure,
and skipping it does not make the demo wrong today - it makes it flaky
tomorrow, on a slower machine or a busier CI runner, which is precisely the
kind of concurrency bug that is nearly impossible to reproduce once it
finally does show up.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production package this module reimplements the core mechanism of.
- [sync.Cond](https://pkg.go.dev/sync#Cond) — the condition variable coordinating waiters and the one goroutine actually computing the result.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the recover boundary in `runFn` that converts a computing goroutine's panic into a shared error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-multi-phase-rollback-transaction.md](26-multi-phase-rollback-transaction.md) | Next: [28-lease-renewal-background-panic.md](28-lease-renewal-background-panic.md)
