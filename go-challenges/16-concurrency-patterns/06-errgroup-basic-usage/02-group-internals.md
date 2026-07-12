# Exercise 2: Rebuilding errgroup From sync Primitives

The fastest way to trust `errgroup` is to rebuild its core and watch it pass the same tests. This exercise implements `Group`, a stdlib-only equivalent of `errgroup.Group` built from exactly three pieces — `sync.WaitGroup`, `sync.Once`, and `context.WithCancel` — and proves it has the same all-succeed, first-error, and cancellation behavior you used through the library in Exercise 1.

This module is fully self-contained and uses the standard library only. It has its own `go mod init`, its own demo, and its own tests; nothing here imports Exercise 1 or `errgroup`.

## What you'll build

```text
group.go               Group{wg, errOnce, err, cancel}, New(ctx), Go(func() error), Wait()
cmd/
  demo/
    main.go            schedule five tasks (some failing), print per-task status
group_test.go          all-succeed, returns-an-error-from-the-set, cancels-on-error, parent-cancel
```

- Files: `group.go`, `cmd/demo/main.go`, `group_test.go`.
- Implement: `New(ctx) (*Group, context.Context)`, `(*Group).Go(func() error)`, and `(*Group).Wait() error`, built on `WaitGroup`, `Once`, and `WithCancel`.
- Test: all-succeed leaves the derived context cancelled; a failing set returns one of its errors; the first error cancels siblings; a parent cancel propagates.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/06-errgroup-basic-usage/02-group-internals/cmd/demo && cd go-solutions/16-concurrency-patterns/06-errgroup-basic-usage/02-group-internals
```

### How the three primitives combine

`Group` holds four fields and they map one-to-one onto the contracts from the concepts file. The `sync.WaitGroup` counts the in-flight goroutines so `Wait` knows when all have returned. The `sync.Once` guards the two things that must happen exactly once and together: recording the first error and cancelling the context. The `err` field is the captured first error, written only inside the `Once`. The `cancel` is the `context.CancelFunc` for the derived context, also called only inside the `Once` (and once more in `Wait`).

`New` does the derivation: `context.WithCancel(ctx)` produces a child context and its cancel function, and `New` returns the `Group` holding that cancel alongside the same child context so the caller can hand it to the tasks. `Go` increments the WaitGroup, launches the goroutine, and on a non-nil result enters `errOnce.Do`, where it stores the error and calls `cancel`. The `Once` is what makes "first error wins" precise and race-free: the first failing goroutine runs the closure, every later failure finds the `Once` already spent and skips it, so `err` is written exactly once and never overwritten by a slower failure. The `cancel()` call inside the same closure is what turns a single failure into a stop signal for the whole group.

`Wait` blocks on the WaitGroup until every goroutine has called `Done`, then calls `cancel` itself and returns `err`. Calling `cancel` in `Wait` is not redundant with the cancel inside `errOnce.Do`: on a fully successful run no goroutine ever entered the `Once`, so the derived context would leak if `Wait` did not cancel it. This is exactly the errgroup contract that the context is cancelled when `Wait` returns even on success, which is why a goroutine parked on `ctx.Done()` is always released. `context.CancelFunc` is safe to call more than once, so the belt-and-suspenders cancel on the failure path costs nothing.

The happens-before reasoning is what makes the unsynchronized read of `err` in `Wait` correct. Each goroutine writes `err` (if at all) before calling `wg.Done`; `wg.Wait` returns only after every `Done`; so the write of `err` happens-before `Wait` reads it. No mutex is needed around `err` because the `Once` serializes the writes and the WaitGroup orders the write before the read.

Create `group.go`:

```go
package group

import (
	"context"
	"sync"
)

// Group runs a collection of goroutines and collects the first error any of
// them returns, cancelling a derived context as soon as that error appears.
// It is a stdlib-only equivalent of golang.org/x/sync/errgroup.Group.
type Group struct {
	wg      sync.WaitGroup
	errOnce sync.Once
	err     error
	cancel  context.CancelFunc
}

// New returns a Group and a derived context. The derived context is cancelled
// when the first goroutine in the group returns a non-nil error, when the
// parent context is cancelled, or when Wait returns — whichever happens first.
func New(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{cancel: cancel}, ctx
}

// Go runs f in a new goroutine. The first f to return a non-nil error has its
// error recorded and triggers cancellation of the derived context; later
// errors are dropped.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.errOnce.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	}()
}

// Wait blocks until all goroutines have returned, cancels the derived context
// (even on success, so it never leaks), and returns the first non-nil error.
func (g *Group) Wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}
```

### The runnable demo

The demo schedules five named tasks, two of which fail after a short sleep; the rest sleep longer and are cooperative, returning early if the context is cancelled. The task with the smallest delay among the failers fails first, cancels the group, and the others record `SKIP` as they observe the cancellation. The status slice is written one index per goroutine and read only after `Wait`, so the print loop sees every status with no race.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"example.com/group-internals"
)

func main() {
	g, gctx := group.New(context.Background())

	type task struct {
		name    string
		delay   time.Duration
		wantErr bool
	}
	tasks := []task{
		{"a", 30 * time.Millisecond, false},
		{"b", 10 * time.Millisecond, true},
		{"c", 50 * time.Millisecond, false},
		{"d", 5 * time.Millisecond, true},
		{"e", 20 * time.Millisecond, false},
	}
	status := make([]string, len(tasks))
	for i, t := range tasks {
		g.Go(func() error {
			select {
			case <-time.After(t.delay):
				if t.wantErr {
					status[i] = "FAIL " + t.name
					return errors.New("task " + t.name + " failed")
				}
				status[i] = "OK   " + t.name
				return nil
			case <-gctx.Done():
				status[i] = "SKIP " + t.name
				return gctx.Err()
			}
		})
	}

	err := g.Wait()
	for _, line := range status {
		fmt.Println(line)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "group returned error:", err)
		os.Exit(1)
	}
	fmt.Println("all tasks completed cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a normally loaded machine):

```
SKIP a
SKIP b
SKIP c
FAIL d
SKIP e
group returned error: task d failed
exit status 1
```

`d` has the smallest delay (5ms), so it fails first and cancels the group; `b`, whose 10ms timer would also fire as a failure, instead observes the already-closed `gctx.Done()` first and records `SKIP`. The output is printed in task-definition order because the loop ranges over `status` in index order; there is no sort. The status lines are deterministic in which task `FAIL`s only because `d`'s delay is strictly the smallest — change the delays and the first failure can change.

### Tests

The tests are the same contracts you asserted against the library in Exercise 1, now against your own `Group`. `TestGroupAllSucceed` runs five succeeding tasks and asserts `Wait` returns `nil` and the derived context is `context.Canceled` afterward. `TestGroupReturnsAnErrorFromTheSet` runs two tasks that both fail instantly and asserts the result is one of the two — membership, not identity, because completion order is not fixed. `TestGroupCancelsOnError` is the central test: one task fails immediately and a sibling parks on `gctx.Done()` for up to two seconds, and the test asserts the sibling was released by the cancellation rather than timing out. `TestGroupRespectsParentCancel` proves the inverse: cancelling the parent cancels the derived context.

Create `group_test.go`:

```go
package group

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGroupAllSucceed(t *testing.T) {
	t.Parallel()

	g, gctx := New(context.Background())
	for i := 0; i < 5; i++ {
		g.Go(func() error {
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if err := gctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("gctx.Err = %v, want context.Canceled after Wait", err)
	}
}

func TestGroupReturnsAnErrorFromTheSet(t *testing.T) {
	t.Parallel()

	errA := errors.New("a failed")
	errB := errors.New("b failed")
	g, _ := New(context.Background())
	g.Go(func() error { return errA })
	g.Go(func() error { return errB })
	got := g.Wait()
	if !errors.Is(got, errA) && !errors.Is(got, errB) {
		t.Fatalf("Wait = %v, want errA or errB", got)
	}
}

func TestGroupCancelsOnError(t *testing.T) {
	t.Parallel()

	g, gctx := New(context.Background())
	released := make(chan struct{})
	g.Go(func() error {
		select {
		case <-gctx.Done():
			close(released)
			return gctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("did not cancel in time")
		}
	})
	g.Go(func() error {
		return errors.New("first")
	})
	if err := g.Wait(); err == nil {
		t.Fatal("expected error")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled when the group saw an error")
	}
}

func TestGroupRespectsParentCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := New(ctx)
	done := make(chan struct{})
	g.Go(func() error {
		<-gctx.Done()
		close(done)
		return gctx.Err()
	})
	cancel()
	if err := g.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not exit on parent cancel")
	}
}
```

## Review

`Group` is correct when the three primitives are wired exactly as described and the same four tests pass under `-race`. The common mistake the `Once` prevents is the mutex-guarded shared error, where a slow failure overwrites a fast one and the "first error" is a lie; the `Once` makes the first writer the only writer. The mistake the `cancel()` inside `errOnce.Do` prevents is a group that records the error but never signals siblings, silently turning fail-fast into wait-for-everyone — `TestGroupCancelsOnError` would hang to its inner two-second timeout if that cancel were missing. The mistake the `cancel()` in `Wait` prevents is leaking the derived context on success, which `TestGroupAllSucceed` catches by asserting `context.Canceled` after a clean run. And the reason `TestGroupReturnsAnErrorFromTheSet` checks membership rather than identity is that two instantly-failing tasks have no fixed completion order; pinning one is flaky. Having rebuilt this, you can read the real `errgroup` source and recognize every line.

## Resources

- [`golang.org/x/sync/errgroup` source](https://cs.opensource.google/go/x/sync/+/master:errgroup/errgroup.go) — the real implementation, which uses the same `WaitGroup` + `Once` + cancel structure you just built.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — the once-and-only-once guard that makes "first error wins" race-free.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the derived-context and `CancelFunc` contract, including that calling cancel more than once is safe.

---

Back to [01-errgroup-basics.md](01-errgroup-basics.md) | Next: [03-scatter-gather-dashboard.md](03-scatter-gather-dashboard.md)
