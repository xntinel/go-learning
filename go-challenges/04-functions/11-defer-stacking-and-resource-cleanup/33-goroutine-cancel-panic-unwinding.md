# Exercise 33: Spawned Goroutine Cleanup — Cancel All on Panic

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Spawning a batch of child goroutines, each with its own cancellable
context, creates a cleanup obligation that grows one entry at a time: every
`context.CancelFunc` returned by `context.WithCancel` needs to actually be
called, or that context's resources leak. This module pushes each cancel
func onto a slice as its child is spawned, and if preparing a later child
panics before it is even spawned, a single deferred closure cancels every
already-spawned child, waits for them to exit, and only then lets the
panic continue.

## What you'll build

```text
spawner/                     independent module: example.com/spawner
  go.mod
  spawner/spawner.go           Task; RunChildren (per-child cancel stack; panic-safe cancel-all)
  cmd/demo/main.go              two blocking children, third task nil -> panic; watch both cancelled
  spawner/spawner_test.go       all succeed; panic mid-spawn cancels already-spawned children; errors joined
```

- Files: `spawner/spawner.go`, `cmd/demo/main.go`, `spawner/spawner_test.go`.
- Implement: a `Task func(ctx context.Context) error`; and `RunChildren(parent context.Context, tasks []Task) (err error)`, which validates and spawns one goroutine per task with its own `context.WithCancel(parent)`, pushing each `CancelFunc` onto a slice as it spawns, and defers a closure that -- on a recovered panic -- cancels every pushed `CancelFunc` in reverse, waits for all spawned goroutines via a `sync.WaitGroup`, and re-panics.
- Test: every task succeeds and `RunChildren` returns their joined (nil) errors; a nil task partway through the list panics before being spawned, and the two already-spawned blocking children are both cancelled and observed to exit before the panic propagates; multiple task errors are collected via `errors.Join`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/spawner/spawner ~/go-exercises/spawner/cmd/demo
cd ~/go-exercises/spawner
go mod init example.com/spawner
go mod edit -go=1.24
```

### The cancel stack has to grow before the panic can happen

`RunChildren` validates each task immediately before spawning it — in this
exercise, rejecting a `nil` task with a `panic`, standing in for any
setup step that might fail partway through preparing a batch of children.
Crucially, the loop spawns the goroutine (and pushes its `cancel` onto
`cancels`) *before* moving on to validate the next task, so by the time a
later validation panics, every child spawned so far already has a
live cancellable context and its `CancelFunc` is already sitting in
`cancels`. The deferred closure at the top of `RunChildren` does not need
to guess which children exist; it just walks `cancels` in reverse — same
LIFO discipline as everywhere else in this chapter — and calls every one of
them.

Canceling is not enough on its own, though: a goroutine takes some
unspecified amount of time to actually notice `ctx.Done()` and return. If
`RunChildren` re-panicked immediately after calling every `cancel()`, its
own goroutine would unwind and (in a real program) possibly let the process
exit while those children were still mid-shutdown. That is why the panic
branch also calls `wg.Wait()` before `panic(p)` — it blocks until every
spawned child has actually finished responding to cancellation, so nothing
is orphaned by the time the panic keeps propagating.

Create `spawner/spawner.go`:

```go
package spawner

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Task is one unit of child work: it must return promptly once ctx is
// cancelled.
type Task func(ctx context.Context) error

// RunChildren spawns one goroutine per task, each with its own
// cancellable context derived from parent. Every cancel func is pushed onto
// a slice as its child is spawned; if preparing (validating) a later task
// panics before it is spawned, the deferred recovery cancels every
// already-spawned child's context in reverse spawn order, waits for them to
// exit, and then re-panics -- so a mid-loop panic never leaves orphaned,
// un-cancelled goroutines running.
func RunChildren(parent context.Context, tasks []Task) (err error) {
	var cancels []context.CancelFunc
	var wg sync.WaitGroup
	errs := make([]error, len(tasks))

	defer func() {
		if p := recover(); p != nil {
			for i := len(cancels) - 1; i >= 0; i-- {
				cancels[i]()
			}
			wg.Wait()
			panic(p)
		}
	}()

	for i, task := range tasks {
		if task == nil {
			panic(fmt.Sprintf("spawner: nil task at index %d", i))
		}

		ctx, cancel := context.WithCancel(parent)
		cancels = append(cancels, cancel)

		wg.Add(1)
		go func(i int, ctx context.Context, task Task) {
			defer wg.Done()
			errs[i] = task(ctx)
		}(i, ctx, task)
	}

	wg.Wait()
	for i := len(cancels) - 1; i >= 0; i-- {
		cancels[i]()
	}
	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/spawner/spawner"
)

func main() {
	var cancelled int64

	blocking := func(ctx context.Context) error {
		<-ctx.Done()
		atomic.AddInt64(&cancelled, 1)
		return ctx.Err()
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("recovered panic:", r)
			}
		}()
		_ = spawner.RunChildren(context.Background(), []spawner.Task{
			blocking,
			blocking,
			nil, // triggers a validation panic before this index is spawned
		})
	}()

	fmt.Println("children cancelled and exited:", atomic.LoadInt64(&cancelled))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered panic: spawner: nil task at index 2
children cancelled and exited: 2
```

### Tests

Create `spawner/spawner_test.go`:

```go
package spawner

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestRunChildrenAllSucceed(t *testing.T) {
	t.Parallel()

	var ran int64
	task := func(ctx context.Context) error {
		atomic.AddInt64(&ran, 1)
		return nil
	}

	err := RunChildren(context.Background(), []Task{task, task, task})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := atomic.LoadInt64(&ran); got != 3 {
		t.Fatalf("ran = %d, want 3", got)
	}
}

func TestRunChildrenPanicCancelsAlreadySpawnedChildren(t *testing.T) {
	t.Parallel()

	var cancelled int64
	blocking := Task(func(ctx context.Context) error {
		<-ctx.Done()
		atomic.AddInt64(&cancelled, 1)
		return ctx.Err()
	})

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = RunChildren(context.Background(), []Task{blocking, blocking, nil})
	}()

	if got := atomic.LoadInt64(&cancelled); got != 2 {
		t.Fatalf("cancelled = %d, want 2: both already-spawned children must be cancelled and awaited", got)
	}
}

func TestRunChildrenCollectsErrorsFromAllChildren(t *testing.T) {
	t.Parallel()

	failing := func(ctx context.Context) error {
		return context.Canceled
	}
	ok := func(ctx context.Context) error { return nil }

	err := RunChildren(context.Background(), []Task{ok, failing, failing})
	if err == nil {
		t.Fatal("err = nil, want a joined error from the two failing children")
	}
}
```

## Review

`RunChildren` is correct when every spawned child eventually gets its
context cancelled, in every outcome — full success, a mid-loop panic, or
plain task failures — and when a panic path specifically waits for
already-spawned children to actually exit before propagating further. The
mistake this pattern exists to prevent is calling every `cancel()` but
returning (or re-panicking) immediately afterward without a `wg.Wait()`:
cancellation is a request, not a guarantee of immediate termination, and a
function that "cleaned up" by firing cancel signals and moving on can still
leave goroutines running past the point their owner believed they were
gone. Run with `-race` — the shared `errs` slice is written at disjoint
indices from each goroutine, and the race detector is what actually proves
that sharing is safe.

## Resources

- [context package](https://pkg.go.dev/context)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [errors.Join](https://pkg.go.dev/errors#Join)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-queue-item-requeue-on-error.md](32-queue-item-requeue-on-error.md) | Next: [34-memory-arena-checkpoint-restore.md](34-memory-arena-checkpoint-restore.md)
