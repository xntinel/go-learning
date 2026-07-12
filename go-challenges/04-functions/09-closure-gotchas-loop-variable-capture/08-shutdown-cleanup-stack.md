# Exercise 8: Graceful Shutdown Stack: Capturing CancelFuncs in a Loop

A supervisor starts several subsystems in a loop, each returning a cleanup or
`context.CancelFunc`, and collects them into a shutdown stack it runs LIFO on
`Stop`. Building that stack by capturing the loop variable is a subtle
loop-capture bug in graceful-shutdown code: every stack entry ends up cancelling
the same last subsystem. You build the correct append-the-value pattern, tear
down in exact reverse order, and make double-`Stop` safe with `sync.Once`.

## What you'll build

```text
shutdown/                    independent module: example.com/shutdown
  go.mod                     go 1.26
  shutdown.go                Stack; Push, Stop (LIFO via slices.Reverse), sync.Once guard
  cmd/
    demo/
      main.go                runnable demo: start subsystems, Stop, print teardown order
  shutdown_test.go           reverse-order teardown, once-per-cleanup, double-Stop safe
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: `Stack.Push(cleanup)` appending the cleanup value; `Stop` running cleanups LIFO with `slices.Reverse`, guarded by `sync.Once` so it runs at most once; a `Start` helper that wires a subsystem's own `CancelFunc` into the stack.
- Test: start N fake subsystems recording start/stop order; assert `Stop` tears down in exact reverse and calls each subsystem's own cleanup exactly once (a loop-var capture would cancel the last one N times); assert double-`Stop` is safe; `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/08-shutdown-cleanup-stack/cmd/demo
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/08-shutdown-cleanup-stack
go mod edit -go=1.26
```

### Why you append the value, not capture the variable

A shutdown stack collects one cleanup func per subsystem. Each subsystem's
cleanup must close over THAT subsystem's resource. The safe pattern is to append
the cleanup value to a slice: `stack = append(stack, cleanup)`. The slice holds
distinct func values, each already bound to its own subsystem, so there is no
shared variable to alias. This is the version-independent form.

The trap is capturing the loop variable inside a cleanup closure you build in the
loop, for example collecting `func(){ sub.Close() }` where `sub` is the loop
variable on a pre-1.22 module — every collected closure would then close the last
subsystem, so `Stop` cancels one resource N times and leaks the other N-1. This
exercise builds the stack the safe way and pins reverse-order teardown so the bug
cannot slip back in.

Teardown order matters in real systems: you start the database, then the cache
that depends on it, then the HTTP server that depends on both; on shutdown you
stop them in REVERSE so nothing calls into an already-closed dependency.
`slices.Reverse` on a copy of the collected cleanups gives LIFO order. `Stop`
wraps the teardown in `sync.Once` so a double `Stop` (a real hazard when both a
signal handler and a `defer` call it) runs cleanups exactly once. `context.
WithCancel` supplies a realistic `CancelFunc` for the `Start` helper, so the
stack holds genuine cancel functions, not just test doubles.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"slices"
	"sync"
)

// Stack collects cleanup funcs and runs them LIFO exactly once on Stop.
type Stack struct {
	mu       sync.Mutex
	once     sync.Once
	cleanups []func()
}

// New returns an empty Stack.
func New() *Stack {
	return &Stack{}
}

// Push adds a cleanup to the stack. The cleanup value is appended, so it stays
// bound to whatever it already closed over; there is no shared loop variable.
func (s *Stack) Push(cleanup func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanups = append(s.cleanups, cleanup)
}

// Start runs a subsystem's start function, wiring a cancellable context, and
// pushes its cancel plus the subsystem's own stop func onto the stack.
func (s *Stack) Start(parent context.Context, start func(ctx context.Context) (stop func())) {
	ctx, cancel := context.WithCancel(parent)
	stop := start(ctx)
	s.Push(func() {
		stop()
		cancel()
	})
}

// Stop runs every pushed cleanup in reverse (LIFO) order, at most once.
func (s *Stack) Stop() {
	s.once.Do(func() {
		s.mu.Lock()
		order := slices.Clone(s.cleanups)
		s.mu.Unlock()

		slices.Reverse(order)
		for _, cleanup := range order {
			cleanup()
		}
	})
}
```

### The runnable demo

The demo starts three named subsystems and stops the stack, printing the teardown
order to show LIFO: last started is first stopped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/shutdown"
)

func main() {
	s := shutdown.New()
	for _, name := range []string{"database", "cache", "http-server"} {
		s.Push(func() {
			fmt.Println("stopping", name)
		})
	}
	s.Stop()
	s.Stop() // safe: runs at most once
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stopping http-server
stopping cache
stopping database
```

### Tests

`TestTeardownIsReverseOrder` starts N subsystems and asserts `Stop` runs cleanups
LIFO. `TestEachCleanupRunsExactlyOnce` counts invocations per subsystem to prove
each own-cleanup fires once (a loop-var capture would fire the last one N times
and the rest zero). `TestDoubleStopIsSafe` calls `Stop` twice and asserts one
teardown.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestTeardownIsReverseOrder(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string

	s := New()
	for _, name := range []string{"db", "cache", "server"} {
		s.Push(func() {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		})
	}
	s.Stop()

	want := []string{"server", "cache", "db"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (LIFO)", order, want)
		}
	}
}

func TestEachCleanupRunsExactlyOnce(t *testing.T) {
	t.Parallel()

	counts := map[string]int{}
	var mu sync.Mutex

	s := New()
	names := []string{"a", "b", "c", "d"}
	for _, name := range names {
		s.Push(func() {
			mu.Lock()
			counts[name]++
			mu.Unlock()
		})
	}
	s.Stop()

	for _, name := range names {
		if counts[name] != 1 {
			t.Fatalf("cleanup %q ran %d times, want 1 (loop-capture cancels last one N times)", name, counts[name])
		}
	}
}

func TestDoubleStopIsSafe(t *testing.T) {
	t.Parallel()

	var n int
	s := New()
	s.Push(func() { n++ })
	s.Stop()
	s.Stop()
	if n != 1 {
		t.Fatalf("cleanup ran %d times across two Stop calls, want 1", n)
	}
}

func TestStartWiresCancel(t *testing.T) {
	t.Parallel()

	s := New()
	var stopped bool
	cancelled := make(chan struct{})

	s.Start(context.Background(), func(ctx context.Context) func() {
		go func() {
			<-ctx.Done()
			close(cancelled)
		}()
		return func() { stopped = true }
	})
	s.Stop()

	<-cancelled // channel receive establishes happens-before; no shared-bool race
	if !stopped {
		t.Fatal("subsystem stop func was not called")
	}
}

func ExampleStack_Stop() {
	s := New()
	for _, name := range []string{"a", "b"} {
		s.Push(func() { fmt.Println("stop", name) })
	}
	s.Stop()
	// Output:
	// stop b
	// stop a
}
```

## Review

The stack is correct when `Stop` runs each pushed cleanup exactly once in reverse
order and is idempotent under repeated calls. `TestEachCleanupRunsExactlyOnce` is
the capture guard: appending the cleanup value binds each closure to its own
subsystem, so a loop-var capture that ran the last cleanup four times and the rest
zero would fail it. Two mechanisms carry the design: `slices.Reverse` on a clone
gives deterministic LIFO teardown (start db, cache, server; stop server, cache,
db), and `sync.Once` makes a double `Stop` — from a signal handler and a `defer` —
safe. Run `go test -race`; `Push` and `Stop` share the cleanup slice under a
mutex.

## Resources

- [`slices.Reverse`](https://pkg.go.dev/slices#Reverse) and [`slices.Clone`](https://pkg.go.dev/slices#Clone) — LIFO teardown on a copy.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — run shutdown at most once.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the `CancelFunc` each subsystem contributes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-per-key-memoizer.md](09-per-key-memoizer.md)
