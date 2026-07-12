# Exercise 5: A Background-Worker Fixture — Cleanup Ordering and t.Context

A fixture that starts a background goroutine must guarantee the goroutine stops
when the test ends, or it hangs the whole test binary. This module builds a
worker fixture that wires shutdown to `t.Context()` cancellation and joins the
goroutine in a `t.Cleanup`, then demonstrates the LIFO ordering of cleanups that
makes composed fixtures tear down in the right sequence.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
workerfixture/               independent module: example.com/workerfixture
  go.mod                     go 1.26
  worker.go                  Worker: a goroutine that doubles submitted jobs, ctx-cancellable
  cmd/
    demo/
      main.go                starts a worker, submits jobs, cancels
  worker_test.go             startWorker fixture (t.Context + t.Cleanup join); LIFO ordering test
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: a `Worker` with a `Submit(ctx, n)` that hands a job to a background goroutine and waits for the result, and a `Run(ctx)` loop that exits on `ctx.Done()`.
- Test: `startWorker(t)` launches the loop with `t.Context()`, registers a `t.Cleanup` that joins the goroutine within a deadline (failing if it hangs); a second test pins the LIFO ordering of cleanups.
- Verify: `go test -count=1 -race ./...`

### Why `t.Context()` is the correct shutdown signal

Since Go 1.24, `t.Context()` returns a context that "is canceled just before
Cleanup-registered functions are called." That timing is exactly what a
background-goroutine fixture needs. The fixture starts the worker loop with
`ctx := t.Context()`; the loop watches `<-ctx.Done()` and returns when it fires.
The fixture also registers a `t.Cleanup` that joins the goroutine (`wg.Wait()`).
Because the context is canceled *before* any cleanup runs, by the time the join
executes the loop has already been told to stop — so `wg.Wait()` returns promptly
instead of blocking forever.

Get this wrong and the failure mode is severe: a fixture that starts a goroutine
with no stop signal, or that joins a goroutine it never cancelled, blocks in
`Cleanup` and the test binary hangs until the CI timeout kills it. To make the
fixture *prove* it does not hang, the cleanup joins with a deadline: it waits on a
`done` channel or a `time.After`, and calls `t.Error` if the worker did not stop
in time. Note the cleanup uses `t.Error`, not `t.Fatal` — a cleanup runs after the
test function has returned, and calling `Fatal`'s `Goexit` from there is at best
pointless.

### The worker

`Worker` owns an unbuffered `jobs` channel. `Run(ctx)` loops on a `select`:
receive a job and reply with its result, or return on `ctx.Done()`. `Submit(ctx,
n)` sends a job carrying a private reply channel and waits for the answer, also
honoring `ctx` so a caller is never wedged if the worker is gone. The job doubles
its input — a stand-in for any real per-item processing.

Create `worker.go`:

```go
package workerfixture

import (
	"context"
	"sync"
)

type job struct {
	n     int
	reply chan int
}

// Worker processes submitted jobs on a background goroutine.
type Worker struct {
	jobs chan job
	wg   sync.WaitGroup
}

// NewWorker returns a Worker whose loop is not yet running.
func NewWorker() *Worker {
	return &Worker{jobs: make(chan job)}
}

// Start launches the processing loop, which runs until ctx is canceled.
func (w *Worker) Start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case j := <-w.jobs:
				j.reply <- j.n * 2
			}
		}
	}()
}

// Wait blocks until the processing loop has exited.
func (w *Worker) Wait() {
	w.wg.Wait()
}

// Submit sends n to the worker and returns its doubled result, honoring ctx.
func (w *Worker) Submit(ctx context.Context, n int) (int, error) {
	j := job{n: n, reply: make(chan int, 1)}
	select {
	case w.jobs <- j:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case r := <-j.reply:
		return r, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
```

### The runnable demo

The demo starts a worker under a cancellable context, submits a couple of jobs,
then cancels and waits for the loop to exit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/workerfixture"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	w := workerfixture.NewWorker()
	w.Start(ctx)

	for _, n := range []int{21, 50} {
		r, _ := w.Submit(ctx, n)
		fmt.Printf("submit %d -> %d\n", n, r)
	}

	cancel()
	w.Wait()
	fmt.Println("worker stopped")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submit 21 -> 42
submit 50 -> 100
worker stopped
```

### The tests

`startWorker(t)` is the fixture: it creates a worker, starts it with `t.Context()`,
and registers a `t.Cleanup` that joins the goroutine within a one-second deadline,
failing the test if it hangs. `TestWorkerProcesses` submits jobs through the
fixture and asserts the results; when the test ends, the context cancellation and
the cleanup join prove the goroutine actually stopped. `TestCleanupLIFO` pins the
last-added-first-called ordering: a checker cleanup registered *first* runs
*last*, so it can observe the order the later cleanups appended in.

Create `worker_test.go`:

```go
package workerfixture

import (
	"slices"
	"testing"
	"time"
)

// startWorker launches a worker bound to the test's context and joins it in a
// cleanup with a deadline, so a stuck goroutine fails the test instead of
// hanging the binary.
func startWorker(t *testing.T) *Worker {
	t.Helper()
	w := NewWorker()
	w.Start(t.Context()) // canceled just before cleanups run
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			w.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker did not shut down within 1s")
		}
	})
	return w
}

func TestWorkerProcesses(t *testing.T) {
	w := startWorker(t)
	for _, tc := range []struct{ in, want int }{{21, 42}, {0, 0}, {-5, -10}} {
		got, err := w.Submit(t.Context(), tc.in)
		if err != nil {
			t.Fatalf("Submit(%d): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("Submit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestCleanupLIFO proves cleanups run last-added-first-called: the checker is
// registered first so it runs last and sees the order the others appended in.
func TestCleanupLIFO(t *testing.T) {
	var order []string
	t.Cleanup(func() {
		want := []string{"second", "first"}
		if !slices.Equal(order, want) {
			t.Errorf("cleanup order = %v, want %v (LIFO)", order, want)
		}
	})
	t.Cleanup(func() { order = append(order, "first") })
	t.Cleanup(func() { order = append(order, "second") })
}
```

## Review

The fixture is correct when the worker goroutine stops on its own once the test
ends — driven by `t.Context()` cancellation — and the cleanup's deadline join
turns any failure to stop into an explicit test failure rather than a hung binary.
The proof that shutdown works is simply that `go test` returns: a leaked goroutine
would either block the join and trip the one-second deadline, or, without the
deadline, hang forever. `TestCleanupLIFO` is the ordering specification: registered
checker-first, appender-first, appender-second; executed second, first, checker;
so the checker sees `[second, first]`. Run `go test -race`: the race detector
catches a worker that touches shared state without synchronization on the shutdown
path. The mistake to avoid is joining a goroutine you never signalled — wire the
stop to `t.Context()` so cancellation precedes the join.

## Resources

- [testing.T.Context](https://pkg.go.dev/testing#T.Context) — canceled just before cleanup functions run.
- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — last-added-first-called teardown.
- [context.WithCancel](https://pkg.go.dev/context#WithCancel) — the cancellation model the worker loop watches.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-goroutine-safe-fatal-helper.md](06-goroutine-safe-fatal-helper.md)
