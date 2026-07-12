# Exercise 9: Build a Mini errgroup — First Error Cancels the Rest

`golang.org/x/sync/errgroup` is the workhorse of parallel backend work: run N
tasks, and if any one fails, cancel the rest and return the first error. It feels
like magic until you build it from primitives, at which point it is obviously just
`context.WithCancelCause` + a `sync.WaitGroup` + a `sync.Once`. This module builds
that coordinator so the mechanism is fully understood — and so the cancellation of
the siblings carries the failing error as its diagnosable cause.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
group/                     independent module: example.com/group
  go.mod                   module example.com/group
  group.go                 Task; Run(ctx, tasks...) error
  cmd/
    demo/
      main.go              one failing task cancels two blocked siblings
  group_test.go            all-succeed, first-error-cancels, only-first-reported
```

- Files: `group.go`, `cmd/demo/main.go`, `group_test.go`.
- Implement: `Run(ctx, tasks...) error` that launches each task on a shared child context, cancels every sibling with `WithCancelCause` the instant one task errors, waits for all, and returns the first error.
- Test: all tasks succeed returns nil; one error cancels blocked siblings which observe `context.Cause == the failing error`; when two tasks error, only the first-recorded error is returned.
- Verify: `go test -count=1 -race ./...`

### The three primitives that are errgroup

Strip `errgroup` down and this is what remains:

- A **shared child context** from `context.WithCancelCause(ctx)`. Every task runs
  with this one context, so a single `cancel` reaches all of them at once — the
  concurrency-safety of contexts is what lets N goroutines share it with no extra
  locking.
- A **`sync.WaitGroup`** to join. `Run` returns only after every task has
  finished, so a caller that gets control back knows nothing is still running.
- A **`sync.Once`** to elect the first error. Several tasks may fail nearly
  simultaneously; the `Once` guarantees exactly one of them is recorded as "the"
  error and triggers exactly one cancel.

The coordination reads directly: launch each task on the shared child; when a task
returns a non-nil error, `once.Do` records it into `firstErr` and calls
`cancel(err)`, making that error the cancellation *cause* every sibling can read
via `context.Cause(ctx)`. `Wait` for all tasks, then return `firstErr`. Passing
the failing error as the cause is the upgrade over a plain `WithCancel`: a sibling
that observes `ctx.Done()` and inspects `context.Cause(ctx)` learns *why* the
group is tearing down — "the export task failed with ErrDiskFull" — instead of the
opaque `context.Canceled`.

Reading `firstErr` after `wg.Wait()` is race-free without a lock on the read: the
write happens inside `once.Do` on a task goroutine, which happens-before that
goroutine's deferred `wg.Done()`, which happens-before `wg.Wait()` returns. The
`-race` build confirms this ordering holds. The deferred `cancel(context.Canceled)`
is the mandatory cleanup for the all-succeed path; because a second cancel never
overwrites the cause, it does not clobber a failing task's cause on the error path.

Create `group.go`:

```go
package group

import (
	"context"
	"sync"
)

// Task is one unit of parallel work. A well-behaved task honors ctx: it selects
// on ctx.Done() so a sibling's failure tears it down promptly.
type Task func(ctx context.Context) error

// Run executes every task concurrently on a shared child context. The instant a
// task returns a non-nil error, Run cancels the child with that error as the
// cause, so every sibling can read the reason via context.Cause. Run waits for
// all tasks and returns the first error recorded (nil if all succeed).
func Run(ctx context.Context, tasks ...Task) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, task := range tasks {
		wg.Add(1)
		go func(task Task) {
			defer wg.Done()
			if err := task(ctx); err != nil {
				once.Do(func() {
					firstErr = err
					cancel(err)
				})
			}
		}(task)
	}

	wg.Wait()
	return firstErr
}
```

### The runnable demo

The demo runs one task that fails immediately alongside two that block on
`ctx.Done()`. The failure cancels the siblings, which read the cause. `main` prints
the run error, then drains the two sibling causes, so the output order is fixed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/group"
)

func main() {
	errDiskFull := errors.New("disk full")
	causes := make(chan error, 2)

	failing := func(ctx context.Context) error {
		return errDiskFull
	}
	blocking := func(ctx context.Context) error {
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return ctx.Err()
	}

	err := group.Run(context.Background(), failing, blocking, blocking)
	fmt.Println("run error:", err)
	for i := 0; i < 2; i++ {
		fmt.Println("sibling cancelled by:", <-causes)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
run error: disk full
sibling cancelled by: disk full
sibling cancelled by: disk full
```

### Tests

`TestAllSucceed` runs three succeeding tasks and asserts nil plus all ran.
`TestFirstErrorCancelsOthers` has one task error and two block on `ctx.Done()`,
then asserts `Run` returns the error and both siblings observed
`context.Cause == the failing error`. `TestOnlyFirstErrorReported` makes the second
task's error strictly follow the first's cancel, so the first is deterministically
recorded, and asserts `Run` returns it.

Create `group_test.go`:

```go
package group

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllSucceed(t *testing.T) {
	t.Parallel()

	var ran atomic.Int64
	ok := func(ctx context.Context) error {
		ran.Add(1)
		return nil
	}

	if err := Run(context.Background(), ok, ok, ok); err != nil {
		t.Fatalf("Run err = %v, want nil", err)
	}
	if got := ran.Load(); got != 3 {
		t.Fatalf("ran %d tasks, want 3", got)
	}
}

func TestFirstErrorCancelsOthers(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	causes := make(chan error, 2)

	failing := func(ctx context.Context) error {
		return errBoom
	}
	blocking := func(ctx context.Context) error {
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return ctx.Err()
	}

	err := Run(context.Background(), failing, blocking, blocking)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Run err = %v, want errBoom", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case cause := <-causes:
			if !errors.Is(cause, errBoom) {
				t.Fatalf("sibling cause = %v, want errBoom", cause)
			}
		case <-time.After(time.Second):
			t.Fatal("sibling did not observe cancellation")
		}
	}
}

func TestOnlyFirstErrorReported(t *testing.T) {
	t.Parallel()

	errFast := errors.New("fast failure")
	errSlow := errors.New("slow failure")

	fast := func(ctx context.Context) error {
		return errFast
	}
	// slow only errors after the group is already cancelling, so fast wins.
	slow := func(ctx context.Context) error {
		<-ctx.Done()
		return errSlow
	}

	err := Run(context.Background(), fast, slow)
	if !errors.Is(err, errFast) {
		t.Fatalf("Run err = %v, want errFast (first recorded)", err)
	}
	if errors.Is(err, errSlow) {
		t.Fatalf("Run err = %v, should not be errSlow", err)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	errBoom := errors.New("boom")
	failing := func(ctx context.Context) error { return errBoom }
	blocking := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

	_ = Run(context.Background(), failing, blocking, blocking)

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: NumGoroutine=%d, base=%d",
				runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}
}
```

## Review

The coordinator is correct when all-success returns nil, one failure returns that
error and cancels the siblings with the failing error as their
`context.Cause`, and a near-simultaneous second failure is discarded in favor of
the first — the property `sync.Once` guarantees. The determinism trick in
`TestOnlyFirstErrorReported` is worth internalizing: the slow task cannot error
until the fast task's cancel has fired, so "first" is not a race. Reading
`firstErr` after `wg.Wait()` is safe because the write happens-before the
matching `wg.Done()`; run `go test -race` to confirm. The `TestNoGoroutineLeak`
check verifies `Run` truly joins every task before returning — an errgroup that
returned while a sibling was still blocked would leak. Having built this from
three primitives, `errgroup.WithContext` should now read as exactly this code.

## Resources

- [context.WithCancelCause](https://pkg.go.dev/context#WithCancelCause) and [context.Cause](https://pkg.go.dev/context#Cause) — the shared cancel and its diagnosable reason.
- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — the production version this reconstructs.
- [sync.Once](https://pkg.go.dev/sync#Once) and [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — electing the first error and joining.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../05-context-withtimeout-withdeadline/00-concepts.md](../05-context-withtimeout-withdeadline/00-concepts.md)
