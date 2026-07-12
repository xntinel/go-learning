# Exercise 6: Stop One Goroutine's Panic From Crashing the Whole Server

A panic in a goroutine does not stay local: it unwinds that goroutine's stack and,
if unrecovered, crashes the entire process — every other goroutine dies with it.
A `recover` in the goroutine that *launched* the panicking one does not catch it.
So a single bad task, in a fleet of request-handling workers, can take the whole
server down. This exercise builds `SafeGo`, a supervisor helper that gives each
task its own recovery boundary and routes any panic to an error handler.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
safego/                      independent module: example.com/safego
  go.mod                     go 1.25 (WaitGroup.Go)
  safego.go                  SafeGo(wg, work func() error, onErr func(error))
  cmd/
    demo/
      main.go                run a panicking task and a failing task; server survives
  safego_test.go             panic -> onErr with sentinel text; nil work -> no onErr
```

Files: `safego.go`, `cmd/demo/main.go`, `safego_test.go`.
Implement: `SafeGo(wg *sync.WaitGroup, work func() error, onErr func(error))` that runs `work` in a goroutine with a deferred `recover`, converting any panic into an error routed to `onErr`, and returning a normal error the same way.
Test: a panicking `work` makes `onErr` receive a non-nil error whose message contains the panic value, and the test process does not crash; a `nil`-returning `work` never calls `onErr`; the `WaitGroup` still reaches zero even on panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The recovery boundary must live inside the goroutine

The asymmetry is the whole lesson. This does not work:

```go
// WRONG: the recover never fires; the process crashes.
defer func() { recover() }()
go func() { panic("boom") }()
```

`recover` only catches a panic propagating through *its own* goroutine's stack.
The deferred func above belongs to the launching goroutine; the panic is in a
different goroutine and never touches this stack. When that goroutine's stack
unwinds with no `recover` of its own, the runtime terminates the process.

So the boundary must be the first thing the spawned goroutine sets up. `SafeGo`
wraps `work` in a goroutine whose `defer` calls `recover`; if `recover` returns
non-nil, the panic value is converted into an error with `fmt.Errorf("panic: %v",
r)` and handed to `onErr`. This works for any panic value — a string, an `error`,
a `runtime.Error` from a nil dereference or an out-of-bounds index — because `%v`
formats whatever was passed to `panic`. A task that returns a normal `error`
routes it to `onErr` too; a task that returns `nil` calls nothing. The result is a
supervisor: one task panicking becomes one routed error, not a dead server.

`SafeGo` uses `wg.Go`, so the `Add`/`Done` accounting is correct even when `work`
panics — the `recover` is inside the function `wg.Go` runs, so the function
returns normally (the panic is swallowed there), and `wg.Go`'s own `defer Done`
fires. There is no leaked `WaitGroup` count. Placing `recover` *outside* `wg.Go`'s
function would be too late: `wg.Go` would see the panic propagate and re-panic.

Create `safego.go`:

```go
package safego

import (
	"fmt"
	"sync"
)

// SafeGo runs work in a goroutine tracked by wg, converting any panic into an
// error routed to onErr so a single failing task cannot crash the process. A
// non-nil error returned by work is routed to onErr as well; a nil return calls
// nothing.
func SafeGo(wg *sync.WaitGroup, work func() error, onErr func(error)) {
	wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				onErr(fmt.Errorf("panic: %v", r))
			}
		}()
		if err := work(); err != nil {
			onErr(err)
		}
	})
}
```

### The runnable demo

The demo launches three tasks: one that panics, one that returns an error, and one
that succeeds. All errors are collected under a mutex. The key observation is that
the program prints a summary and exits cleanly — the panic did not take it down.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"example.com/safego"
)

func main() {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string

	onErr := func(err error) {
		mu.Lock()
		errs = append(errs, err.Error())
		mu.Unlock()
	}

	safego.SafeGo(&wg, func() error { panic("bad task") }, onErr)
	safego.SafeGo(&wg, func() error { return errors.New("timeout") }, onErr)
	safego.SafeGo(&wg, func() error { return nil }, onErr)

	wg.Wait()

	sort.Strings(errs)
	fmt.Printf("server survived; %d task error(s): %v\n", len(errs), errs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
server survived; 2 task error(s): [panic: bad task timeout]
```

### Tests

`TestSafeGoRecoversPanic` runs a panicking task and asserts `onErr` received a
non-nil error whose message contains the panic value — and, by the test process
not crashing, that the panic was contained. `TestSafeGoRoutesReturnedError` uses a
sentinel error wrapped with `%w` and asserts `errors.Is`. `TestSafeGoNilIsSilent`
proves a successful task never calls `onErr`. `TestSafeGoWaitGroupNotLeaked` runs a
panicking task and asserts `wg.Wait()` returns (the count reached zero despite the
panic) by simply completing.

Create `safego_test.go`:

```go
package safego

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var errTask = errors.New("task failed")

func TestSafeGoRecoversPanic(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	var got atomic.Pointer[error]
	SafeGo(&wg, func() error { panic("sentinel-boom") }, func(err error) {
		got.Store(&err)
	})
	wg.Wait()

	p := got.Load()
	if p == nil {
		t.Fatal("onErr was never called for a panicking task")
	}
	if !strings.Contains((*p).Error(), "sentinel-boom") {
		t.Fatalf("error %q does not contain the panic value", (*p).Error())
	}
}

func TestSafeGoRoutesReturnedError(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	var got atomic.Pointer[error]
	SafeGo(&wg, func() error { return errTask }, func(err error) {
		got.Store(&err)
	})
	wg.Wait()

	p := got.Load()
	if p == nil || !errors.Is(*p, errTask) {
		t.Fatalf("onErr err = %v, want errors.Is(..., errTask)", p)
	}
}

func TestSafeGoNilIsSilent(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	var calls atomic.Int64
	SafeGo(&wg, func() error { return nil }, func(err error) {
		calls.Add(1)
	})
	wg.Wait()

	if got := calls.Load(); got != 0 {
		t.Fatalf("onErr called %d times for a successful task, want 0", got)
	}
}

func TestSafeGoWaitGroupNotLeaked(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	SafeGo(&wg, func() error { panic("x") }, func(err error) {})
	// If SafeGo leaked the WaitGroup count on panic, this would block forever
	// and the test would time out.
	wg.Wait()
}
```

## Review

`SafeGo` is correct when a panicking task becomes exactly one routed error and the
process keeps running, when a normal error is routed unchanged (assertable with
`errors.Is` against a sentinel), and when a successful task is silent. The one
non-negotiable is that the `recover` lives *inside* the goroutine that runs
`work`, not in the caller — a `recover` in the launcher cannot catch a child's
panic, and the process would crash. Because the boundary is inside `wg.Go`'s
function, the `WaitGroup` count is decremented normally even on panic, so there is
no leaked count; the last test proves `Wait` still returns. Run `-race`: `onErr`
is called from worker goroutines, so any shared state it touches must be
synchronized (the tests use atomics and a mutex).

## Resources

- [Go Language Specification: Handling panics](https://go.dev/ref/spec#Handling_panics)
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover)
- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-shutdown-flush-fire-and-forget-trap.md](05-shutdown-flush-fire-and-forget-trap.md) | Next: [07-scatter-gather-results-channel.md](07-scatter-gather-results-channel.md)
