# Exercise 6: First-Error Abort: Cancelling Siblings On The First Failure

When you fan out `N` parallel upstream calls and any one fails, continuing the
rest is wasted work — the caller is going to get an error anyway. The fix is a
`close`-once abort broadcast: the first task to fail closes an `abort chan
struct{}`, every in-flight sibling observes it and bails, and the first error is
returned. This is a hand-rolled miniature `errgroup`; the exercise also shows the
idiomatic `context.WithCancelCause` equivalent.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
fanout/                      independent module: example.com/fanout
  go.mod                     go mod init example.com/fanout
  group.go                   type Group; NewGroup, Go(task), Wait; type Task
  cmd/
    demo/
      main.go                runnable demo: one failure aborts the siblings
  group_test.go              all-succeed, first-error-aborts, concurrent-failures
```

Files: `group.go`, `cmd/demo/main.go`, `group_test.go`.
Implement: a `Group` running `N` tasks; on the first error, an `abort chan struct{}` is closed once via `sync.Once` to signal all siblings; the first error is captured under a mutex and returned by `Wait`. Tasks select over `abort` to bail.
Test: all succeed then `Wait` returns nil and every task ran; inject one failure and that error is returned while siblings observe abort and exit early (a counter proves not all completed full work); concurrent failures record exactly one and close exactly once (no double-close panic).
Verify: `go test -count=1 -race ./...`

### The abort broadcast

Each task receives the `abort` channel and is expected to watch it:

```go
select {
case <-abort:
	return nil // a sibling already failed; stop early
case result := <-doWork():
	...
}
```

When a task returns a non-nil error, the group does two things under
synchronization: it records the error if none is recorded yet (mutex-guarded, so
the *first* error wins), and it closes `abort` through `sync.Once`. The `Once` is
essential — many tasks can fail nearly simultaneously, and a bare `close(abort)`
in each would double-close and panic. `Once.Do(close)` makes the broadcast happen
exactly once no matter how many failures race into it.

`Wait` blocks on the `WaitGroup` until every task goroutine has returned, then
returns the recorded first error. The `WaitGroup` is the termination contract:
`Wait` returning means every sibling either finished or observed the abort and
exited — no goroutine is left running past the group's lifetime.

### The idiomatic equivalent

The standard-library version of this pattern is `context.WithCancelCause`, whose
cancellation *is* a closed done channel with an attached reason:

```go
ctx, cancel := context.WithCancelCause(parent)
// on first failure: cancel(err)
// siblings watch ctx.Done(); context.Cause(ctx) reports the failure
```

`golang.org/x/sync/errgroup` builds exactly on that. This exercise hand-rolls the
mechanism so the closed-channel broadcast underneath is visible; in production,
prefer `errgroup` (or `WithCancelCause`) so cancellation, error capture, and
propagation come for free.

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/06-first-error-abort-fanout/cmd/demo
cd go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/06-first-error-abort-fanout
```

Create `group.go`:

```go
package fanout

import (
	"sync"
)

// Task is a unit of parallel work. It must watch abort and return promptly once
// abort is closed (a sibling has failed).
type Task func(abort <-chan struct{}) error

// Group runs tasks in parallel and aborts the rest on the first error, returning
// that first error from Wait.
type Group struct {
	abort    chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
	mu       sync.Mutex
	firstErr error
}

// NewGroup returns a ready Group.
func NewGroup() *Group {
	return &Group{abort: make(chan struct{})}
}

// Go launches task in its own goroutine. On the task's first non-nil error the
// group records it (first error wins) and closes abort exactly once.
func (g *Group) Go(task Task) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := task(g.abort); err != nil {
			g.mu.Lock()
			if g.firstErr == nil {
				g.firstErr = err
			}
			g.mu.Unlock()
			g.once.Do(func() { close(g.abort) })
		}
	}()
}

// Wait blocks until all tasks have returned and reports the first error, or nil
// if every task succeeded.
func (g *Group) Wait() error {
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.firstErr
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/fanout"
)

func main() {
	g := fanout.NewGroup()
	var completed atomic.Int64

	// One task fails immediately.
	g.Go(func(abort <-chan struct{}) error {
		return errors.New("upstream 2 failed")
	})

	// Four siblings that would take a second, but bail on abort.
	for range 4 {
		g.Go(func(abort <-chan struct{}) error {
			select {
			case <-abort:
				return nil
			case <-time.After(time.Second):
				completed.Add(1)
				return nil
			}
		})
	}

	err := g.Wait()
	fmt.Printf("group error: %v\n", err)
	fmt.Printf("siblings that completed full work: %d\n", completed.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
group error: upstream 2 failed
siblings that completed full work: 0
```

### Tests

Create `group_test.go`:

```go
package fanout

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllSucceed(t *testing.T) {
	t.Parallel()

	g := NewGroup()
	var completed atomic.Int64
	for range 10 {
		g.Go(func(_ <-chan struct{}) error {
			completed.Add(1)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if got := completed.Load(); got != 10 {
		t.Fatalf("completed = %d, want 10", got)
	}
}

func TestFirstErrorAbortsSiblings(t *testing.T) {
	t.Parallel()

	g := NewGroup()
	failErr := errors.New("boom")
	var completed atomic.Int64

	g.Go(func(_ <-chan struct{}) error { return failErr })

	for range 10 {
		g.Go(func(abort <-chan struct{}) error {
			select {
			case <-abort:
				return nil // bailed early
			case <-time.After(5 * time.Second):
				completed.Add(1)
				return nil
			}
		})
	}

	err := g.Wait()
	if !errors.Is(err, failErr) {
		t.Fatalf("Wait = %v, want failErr", err)
	}
	if got := completed.Load(); got != 0 {
		t.Fatalf("completed = %d, want 0 (siblings should abort early)", got)
	}
}

func TestConcurrentFailuresSingleClose(t *testing.T) {
	t.Parallel()

	// Many tasks fail nearly simultaneously. A bare close in each would
	// double-close and panic; sync.Once makes exactly one close happen. Run
	// under -race to catch an unsynchronized close.
	g := NewGroup()
	for range 50 {
		g.Go(func(_ <-chan struct{}) error {
			return errors.New("concurrent failure")
		})
	}
	if err := g.Wait(); err == nil {
		t.Fatal("Wait = nil, want a non-nil error")
	}
}
```

## Review

The group is correct when a single failure aborts every sibling and `Wait`
returns that error: `TestFirstErrorAbortsSiblings` proves both by asserting the
error identity and that no sibling completed its long work. The property that
saves you from a 3am panic is `sync.Once` around the close — `TestConcurrentFailuresSingleClose`
fires 50 simultaneous failures, and without the guard the second close crashes;
`-race` turns any unsynchronized close into a hard failure. For production, reach
for `context.WithCancelCause` or `golang.org/x/sync/errgroup`, which are this
pattern with error propagation and cancellation already wired.

## Resources

- [pkg.go.dev: context.WithCancelCause](https://pkg.go.dev/context#WithCancelCause) — cancellation with an attached cause; `context.Cause` retrieves it.
- [pkg.go.dev: golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — the production version of first-error abort.
- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) — the single-close guard under concurrent failures.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-drain-vs-cancel.md](07-drain-vs-cancel.md)
