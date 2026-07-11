# Exercise 27: Request Coalescing: Collapse Duplicate Concurrent Calls to One Execution

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache-miss stampede happens when a hot key expires and a burst of
concurrent requests all miss the cache at once, each launching its own call
to a slow origin — a database, a downstream service, a recomputation — for
the exact same key. Request coalescing collapses that burst into a single
execution: the first caller in actually runs the work, and every other
concurrent caller for the same key waits for that one result instead of
launching a duplicate. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
coalesce/                   independent module: example.com/request-coalescing-singleflight
  go.mod                    go 1.24
  coalesce.go               Group (mutex-protected in-flight map), Do(key, fn)
  cmd/
    demo/
      main.go               20 concurrent callers for one key collapse to a single execution
  coalesce_test.go          sequential re-execution; error propagation; concurrent coalescing -race
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: a `Group` struct guarded by a `sync.Mutex` with `Do(key string, fn func() (string, error)) (val string, err error, shared bool)`, where the map lookup deciding "already in flight?" and the map write registering this call as the one in flight happen inside the same lock acquisition, and every waiter blocks on a `chan struct{}` that the executing goroutine closes once to broadcast the result.
- Test: a sequential test proving each call with no concurrent duplicate re-runs `fn`; an error-propagation test; a concurrency test firing many goroutines at the same key at once and asserting `fn` ran exactly once, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/coalesce/cmd/demo
cd ~/go-exercises/coalesce
go mod init example.com/request-coalescing-singleflight
go mod edit -go=1.24
```

### Why the lookup and the registration must share one lock

The tempting-but-wrong design checks the map for an in-flight call, and —
only if none is found — separately locks again to register this call as the
one in flight. Between those two lock acquisitions, another goroutine can
make the exact same "no one is in flight yet" observation from the exact
same stale map state, and both goroutines proceed to launch `fn`, defeating
the entire point of coalescing during the one moment it matters most: the
instant a stampede begins. `Do` avoids this by holding the lock across the
lookup and, on a miss, the registration — whichever goroutine acquires the
mutex first is guaranteed to see any registration a concurrent goroutine
just made, so exactly one goroutine ever believes it is the first for a
given key at a given moment. The `chan struct{}` that broadcasts the result
is closed rather than sent on for the same reason a `context.Context`'s
`Done()` channel is closed: closing a channel is the one channel operation
that every current and future receiver observes, without needing to know in
advance how many waiters there will be.

Create `coalesce.go`:

```go
// Package coalesce collapses duplicate concurrent calls for the same key
// into a single execution, so a cache-miss stampede for one hot key never
// turns into N identical calls to the origin.
package coalesce

import "sync"

// call tracks one in-flight execution for a key. done is closed exactly once,
// after val and err are written, so every waiter's receive unblocks at the
// same instant with the same result.
type call struct {
	done chan struct{}
	val  string
	err  error
}

// Group deduplicates calls to Do by key. The zero value is ready to use.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// Do runs fn for key, unless another goroutine is already running fn for the
// same key — in which case Do waits for that call's result instead of
// running fn again. shared reports whether this caller waited on someone
// else's execution rather than running fn itself.
//
// The map lookup that decides "already in flight?" and the map write that
// registers this call as the one in flight happen inside the same lock
// acquisition, so two goroutines racing to be first for the same key can
// never both conclude they are the first and both launch fn.
func (g *Group) Do(key string, fn func() (string, error)) (val string, err error, shared bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}

	c := &call{done: make(chan struct{})}
	if g.calls == nil {
		g.calls = make(map[string]*call)
	}
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	close(c.done) // broadcast the result to every goroutine waiting on this key

	g.mu.Lock()
	delete(g.calls, key) // the next call for this key must run fn again, not replay this result
	g.mu.Unlock()

	return c.val, c.err, false
}
```

### The runnable demo

Twenty goroutines all call `Do` for the same key at the same instant,
simulating a cache-miss stampede against a slow origin. Only one of them
actually executes the origin call; the other nineteen wait for its result. A
final call after the first one has completed proves the key is free to run
`fn` again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	coalesce "example.com/request-coalescing-singleflight"
)

func main() {
	var g coalesce.Group
	var executions atomic.Int32
	var sharedWaiters atomic.Int32

	const callers = 20
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait() // release every goroutine at once to force the stampede
			val, _, shared := g.Do("product:42", func() (string, error) {
				executions.Add(1)
				time.Sleep(20 * time.Millisecond) // simulate a slow origin call
				return "product-42-payload", nil
			})
			if shared {
				sharedWaiters.Add(1)
			}
			_ = val
		}()
	}
	start.Done()
	wg.Wait()

	fmt.Printf("callers:          %d\n", callers)
	fmt.Printf("actual executions: %d\n", executions.Load())
	fmt.Printf("callers that waited on a shared result: %d\n", sharedWaiters.Load())

	// A second, sequential call for the same key after the first finished
	// runs fn again — the entry was removed once the in-flight call completed.
	val, _, shared := g.Do("product:42", func() (string, error) {
		executions.Add(1)
		return "product-42-payload-v2", nil
	})
	fmt.Printf("post-completion call: val=%q shared=%v total executions=%d\n", val, shared, executions.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
callers:          20
actual executions: 1
callers that waited on a shared result: 19
post-completion call: val="product-42-payload-v2" shared=false total executions=2
```

### Tests

A sequential test proves each call with no concurrent duplicate re-runs
`fn`. An error test proves the origin's error propagates to the caller
unchanged. The concurrency test fires 50 goroutines at one key at once and
asserts `fn` ran exactly once, under `-race`.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoSequentialCallsEachRunFn(t *testing.T) {
	t.Parallel()

	var g Group
	var calls atomic.Int32

	for i := 0; i < 3; i++ {
		val, err, shared := g.Do("key", func() (string, error) {
			calls.Add(1)
			return "value", nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if val != "value" {
			t.Fatalf("val = %q, want %q", val, "value")
		}
		if shared {
			t.Fatal("a call with no concurrent duplicate must never report shared")
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("fn ran %d times, want 3 — each sequential call must re-run fn", got)
	}
}

func TestDoPropagatesError(t *testing.T) {
	t.Parallel()

	var g Group
	wantErr := errors.New("origin unavailable")

	_, err, _ := g.Do("key", func() (string, error) {
		return "", wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestDoCoalescesConcurrentCalls(t *testing.T) {
	t.Parallel()

	var g Group
	var executions atomic.Int32
	var sharedCount atomic.Int32

	const callers = 50
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			_, _, shared := g.Do("hot-key", func() (string, error) {
				executions.Add(1)
				time.Sleep(20 * time.Millisecond)
				return "payload", nil
			})
			if shared {
				sharedCount.Add(1)
			}
		}()
	}
	start.Done()
	wg.Wait()

	if got := executions.Load(); got != 1 {
		t.Fatalf("executions = %d, want exactly 1 for a coalesced stampede", got)
	}
	if got := sharedCount.Load(); got != callers-1 {
		t.Fatalf("shared waiters = %d, want %d", got, callers-1)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The `delete(g.calls, key)` after the result is broadcast is what makes the
next, unrelated call for the same key actually re-run `fn` instead of
silently replaying a stale result forever — without it, the first execution
would become a permanent cache entry the `Group` was never meant to be. Carry
this forward: any "check if already in flight, else register and run" shape
shared across goroutines needs the check and the registration inside one
critical section, and needs an explicit cleanup step once the in-flight work
finishes, or the data structure meant to prevent duplicate work quietly turns
into a data structure that serves only the first caller ever.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production package this module's `Do` mirrors the shape of.
- [Thundering herd problem](https://en.wikipedia.org/wiki/Thundering_herd_problem) — the failure mode request coalescing exists to prevent.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive keeping the in-flight map's check-then-register atomic.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-admission-control-load-shedding.md](26-admission-control-load-shedding.md) | Next: [28-distributed-trace-span-propagation.md](28-distributed-trace-span-propagation.md)
