# Exercise 1: errgroup Basics

`golang.org/x/sync/errgroup` is the idiomatic way to run N goroutines, wait for all of them, get the first error, and cancel the rest the moment that error appears. This exercise uses the real library to build one small reusable helper, `RunAll`, and proves its three contracts: all-succeed, first-error-wins, and cancellation-on-failure.

This module is fully self-contained. It has its own `go mod init`, imports `errgroup` directly, and ships its own demo and tests. Because `errgroup` is an external module, the gate fetches it; locally you run `go mod tidy` once.

## What you'll build

```text
runner.go              Task type, RunAll(ctx, tasks...) using errgroup.WithContext
cmd/
  demo/
    main.go            run three tasks, one of which fails fast, and observe cancellation
runner_test.go         all-succeed, first-error-wins, sibling-cancellation, context-cancelled-after-Wait
```

- Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: `type Task func(ctx context.Context) error` and `RunAll(ctx context.Context, tasks ...Task) error` over `errgroup.WithContext`.
- Test: all tasks succeed returns nil and leaves the group context cancelled; a failing task returns its error; a slow sibling observes cancellation and stops early.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/06-errgroup-basic-usage/01-errgroup-basics/cmd/demo && cd go-solutions/16-concurrency-patterns/06-errgroup-basic-usage/01-errgroup-basics
go get golang.org/x/sync/errgroup
```

### Why a wrapper, and what the wrapper guarantees

`errgroup` is already small, so the point of wrapping it in `RunAll` is not to hide it but to fix one ergonomic shape: callers almost always have a slice of "things to run" and want a single call that runs them, waits, and returns the first failure. `RunAll` takes that slice of `Task` values, where a `Task` is any function that accepts the group's context and returns an error, and collapses the three-line `WithContext` / `Go` / `Wait` dance into one.

The body is worth reading line by line because every line carries a contract. `errgroup.WithContext(ctx)` derives a cancellable context from the caller's and hands it back alongside the group; the helper shadows `ctx` with that derived context deliberately, so the context every task receives is the one the group will cancel. Each task is launched with `g.Go`, which starts a goroutine and arranges for the first task that returns a non-nil error to record that error and cancel the derived context. Note that there is no `t := t` shadow before the closure: under Go 1.22 and later — and this module declares `go 1.26` — each loop iteration has its own `t`, so capturing it in the closure is already safe. Finally `g.Wait()` blocks until every task has returned and yields the first error, or `nil`. The whole function is a faithful, named version of the canonical errgroup idiom.

The cancellation contract is the part that makes this "fail fast" rather than "wait for everyone." When one task returns an error, the derived context's `Done` channel closes; any task that is watching that channel — by selecting on `ctx.Done()` or by passing `ctx` into a context-aware blocking call — returns promptly instead of running to completion. Tasks that ignore the context are not stopped, because Go cannot forcibly stop a goroutine; the group only supplies the signal. The contract holds in the other direction too: cancelling the context you pass into `RunAll` propagates to the derived context, so an upstream timeout reaches every task.

Create `runner.go`:

```go
package runner

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Task is a unit of concurrent work. It receives the group's derived context
// and must return promptly once that context is cancelled, either by selecting
// on ctx.Done() or by threading ctx into the blocking calls it makes.
type Task func(ctx context.Context) error

// RunAll runs every task concurrently and returns the first non-nil error in
// completion order, or nil if all succeed. When any task fails, the context
// passed to the remaining tasks is cancelled so cooperative tasks can stop
// early instead of running to completion. The context is also cancelled once
// RunAll returns, so no task is left blocked on ctx.Done().
func RunAll(ctx context.Context, tasks ...Task) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, t := range tasks {
		g.Go(func() error {
			return t(ctx)
		})
	}
	return g.Wait()
}
```

### The runnable demo

The demo schedules three tasks. One fails almost immediately; the other two are slow but cooperative — they race their work against `ctx.Done()`. Because the fast failure cancels the group context, the slow tasks return early with the cancellation error rather than after their full sleep, and `RunAll` returns the failure without waiting for them. The printed lines show which tasks were cancelled and what error came back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/errgroup-basics"
)

func main() {
	var mu sync.Mutex
	outcome := map[string]string{}
	record := func(name, state string) {
		mu.Lock()
		outcome[name] = state
		mu.Unlock()
	}

	slow := func(name string) runner.Task {
		return func(ctx context.Context) error {
			select {
			case <-time.After(500 * time.Millisecond):
				record(name, "completed")
				return nil
			case <-ctx.Done():
				record(name, "cancelled")
				return ctx.Err()
			}
		}
	}
	failer := func(ctx context.Context) error {
		record("b", "failed")
		return errors.New("task b failed")
	}

	err := runner.RunAll(context.Background(), slow("a"), failer, slow("c"))

	for _, name := range []string{"a", "b", "c"} {
		fmt.Printf("%s: %s\n", name, outcome[name])
	}
	fmt.Printf("RunAll error: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a: cancelled
b: failed
c: cancelled
RunAll error: task b failed
```

The two slow tasks print `cancelled`, not `completed`, because the failure of `b` cancelled the context they were selecting on long before their 500ms timers fired. `RunAll` returns `task b failed`, which is the first — and here only — error.

### Tests

The tests pin the three contracts. `TestAllSucceed` runs five tasks that all return `nil`, asserts `RunAll` returns `nil`, and asserts the group context is cancelled afterward by checking that the context a captured task observed reports `context.Canceled` once `RunAll` has returned. `TestFirstErrorWins` runs one task that fails immediately and one slow cooperative sibling, asserts the returned error `errors.Is` the sentinel, and asserts the sibling actually observed the cancellation rather than timing out. `TestParentCancel` cancels the caller's context and asserts the failure propagates down to a task blocked on `ctx.Done()`.

Create `runner_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllSucceed(t *testing.T) {
	t.Parallel()

	var ran atomic.Int32
	tasks := make([]Task, 5)
	for i := range tasks {
		tasks[i] = func(ctx context.Context) error {
			ran.Add(1)
			return nil
		}
	}
	if err := RunAll(context.Background(), tasks...); err != nil {
		t.Fatalf("RunAll = %v, want nil", err)
	}
	if got := ran.Load(); got != 5 {
		t.Fatalf("ran %d tasks, want 5", got)
	}
}

func TestFirstErrorWins(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	cancelled := make(chan struct{})

	slow := func(ctx context.Context) error {
		select {
		case <-time.After(2 * time.Second):
			return errors.New("slow task was not cancelled in time")
		case <-ctx.Done():
			close(cancelled)
			return ctx.Err()
		}
	}
	fail := func(ctx context.Context) error {
		return sentinel
	}

	err := RunAll(context.Background(), slow, fail)
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunAll = %v, want %v", err, sentinel)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("slow sibling was never cancelled when the group saw an error")
	}
}

func TestParentCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})

	task := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(done)
		return ctx.Err()
	}

	errCh := make(chan error, 1)
	go func() { errCh <- RunAll(ctx, task) }()

	<-started
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll = %v, want context.Canceled", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task did not exit on parent cancel")
	}
}
```

## Review

`RunAll` is correct when its three contracts hold under `-race`. All-succeed returns `nil` and every task runs, which the atomic counter confirms. A failure returns the first error in completion order and cancels the derived context, which the slow sibling proves by returning through its `ctx.Done()` arm rather than its timeout arm — if cancellation were broken, that test would hang to its two-second inner timeout and fail. Parent cancellation propagates, which the third test confirms by cancelling the caller's context and watching the task exit. The common mistake this exercise inoculates against is expecting the group to stop a task that ignores the context: every cooperative task here either selects on `ctx.Done()` or returns immediately, and a task that did neither would run to completion no matter how early `b` failed. The second mistake is asserting a specific error when several can race; `TestFirstErrorWins` is safe only because the failing task returns instantly while the sibling is parked for two seconds, making the sentinel provably first.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — the package reference: `WithContext`, `Go`, `Wait`, `SetLimit`, `TryGo`, and the exact cancellation contract.
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the cancellation discipline that `errgroup` formalizes, including why work must watch `ctx.Done()` to stop.
- [`context` package](https://pkg.go.dev/context) — `WithCancel` and the `Done`/`Err` contract that the derived context is built on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-group-internals.md](02-group-internals.md)
