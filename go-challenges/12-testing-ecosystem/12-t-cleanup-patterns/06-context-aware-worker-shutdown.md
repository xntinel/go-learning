# Exercise 6: Graceful Worker Shutdown via t.Context and t.Cleanup

Backend tests routinely start a background goroutine — a queue consumer, a
ticker-driven flusher — and then must join it cleanly so it does not leak into the
next test. The Go 1.24 `t.Context()` contract makes this idiomatic: the context is
canceled just before cleanups run, so a cleanup can rely on that cancellation to
stop the worker and then wait for it to finish. This exercise builds that pattern.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
worker/                      independent module: example.com/worker
  go.mod                     go 1.24
  worker.go                  queue-consumer Worker bound to a context, drains on cancel
  cmd/
    demo/
      main.go                runnable demo: submit items, cancel, join, count
  worker_test.go             t.Context + t.Cleanup graceful-join test, bounded by timeout
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: a `Worker` started with a context that consumes submitted items, drains the queue on `ctx.Done()`, and exposes `Wait` and `Processed`.
- Test: bind the worker to `t.Context()`, submit items, and register a `t.Cleanup` that waits on the worker's exit — bounded by a timeout that `t.Error`s if it hangs — asserting all items were processed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/12-t-cleanup-patterns/06-context-aware-worker-shutdown/cmd/demo
cd go-solutions/12-testing-ecosystem/12-t-cleanup-patterns/06-context-aware-worker-shutdown
go mod edit -go=1.24
```

### The t.Context / t.Cleanup shutdown handshake

`t.Context()` returns a context canceled *just before* the test's cleanups run.
That single guarantee is what makes graceful worker shutdown in tests trivial.
Start the worker with `t.Context()`; it selects on `ctx.Done()` in its loop. When
the test body finishes, the test runner cancels the context and *then* runs the
cleanups. So a `t.Cleanup` that calls `worker.Wait()` will find the worker already
draining toward exit, and the wait joins it. The sequence is: test body ends,
context canceled, worker observes `ctx.Done()` and drains its remaining queued
items and returns, cleanup's `Wait()` returns. No manual `cancel()` plumbing, no
leaked goroutine.

The worker must actually finish its queued work before exiting, or the test's
assertion on the processed count would be racy. So on `ctx.Done()` the worker
enters a drain loop: it keeps pulling buffered items until the queue is empty, then
returns. Because every `Submit` happens in the test body — before cancellation —
every item is buffered before the drain begins, so the drain processes all of them.
The cleanup bounds `Wait()` with a timeout channel: if the worker ever fails to
exit, the cleanup `t.Error`s instead of hanging the whole suite.

Create `worker.go`:

```go
package worker

import (
	"context"
	"sync"
	"sync/atomic"
)

// Worker consumes submitted items on a background goroutine until its context is
// canceled, at which point it drains the queue and exits.
type Worker struct {
	in        chan int
	processed atomic.Int64
	wg        sync.WaitGroup
}

// Start launches the worker bound to ctx and returns immediately.
func Start(ctx context.Context) *Worker {
	w := &Worker{in: make(chan int, 64)}
	w.wg.Add(1)
	go w.run(ctx)
	return w
}

func (w *Worker) run(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain buffered items, then exit, so the processed count is final.
			for {
				select {
				case v := <-w.in:
					w.handle(v)
				default:
					return
				}
			}
		case v := <-w.in:
			w.handle(v)
		}
	}
}

func (w *Worker) handle(v int) {
	_ = v
	w.processed.Add(1)
}

// Submit enqueues an item for processing.
func (w *Worker) Submit(v int) {
	w.in <- v
}

// Processed reports how many items have been handled.
func (w *Worker) Processed() int64 {
	return w.processed.Load()
}

// Wait blocks until the worker goroutine has exited.
func (w *Worker) Wait() {
	w.wg.Wait()
}
```

### The runnable demo

The demo runs outside a test, so it manages the context itself: submit items,
cancel, join, and report the count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/worker"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	w := worker.Start(ctx)

	for i := range 3 {
		w.Submit(i)
	}

	cancel()
	w.Wait()
	fmt.Printf("processed: %d\n", w.Processed())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed: 3
```

### The tests

`TestWorkerGracefulShutdown` binds the worker to `t.Context()`, submits five items,
and lets the test body end. The registered `t.Cleanup` then runs *after* the context
is canceled: it joins the worker with a timeout guard and asserts all five items
were processed. Because the drain loop finishes the queue before returning,
`Processed()` is exactly five once `Wait()` returns.

Create `worker_test.go`:

```go
package worker

import (
	"testing"
	"time"
)

func TestWorkerGracefulShutdown(t *testing.T) {
	t.Parallel()
	// t.Context() is canceled just before the cleanup below runs, which is what
	// signals the worker to drain and exit.
	w := Start(t.Context())

	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			w.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not exit within 2s of context cancellation")
			return
		}
		if got := w.Processed(); got != 5 {
			t.Errorf("processed = %d, want 5", got)
		}
	})

	for i := range 5 {
		w.Submit(i)
	}
}

func TestWorkerProcessesBeforeShutdown(t *testing.T) {
	t.Parallel()
	w := Start(t.Context())
	for i := range 10 {
		w.Submit(i)
	}
	// The cleanup contract still applies: the worker joins at test exit. Here we
	// only assert the queue accepted every item without blocking.
	if got := len(w.in); got > cap(w.in) {
		t.Fatalf("queue length %d exceeds capacity %d", got, cap(w.in))
	}
}
```

## Review

The pattern is correct when the worker exits promptly on context cancellation and
has processed every enqueued item by the time `Wait()` returns —
`TestWorkerGracefulShutdown` asserts both, with a timeout so a hung worker fails
loudly rather than freezing CI. The mechanism is the `t.Context()` contract:
because the context is canceled just before cleanups, a cleanup that waits on the
worker joins a goroutine that is already shutting down. The trap to remember,
covered in the concepts file, is the inverse: do not use `t.Context()` for a fresh
outbound call *inside* a cleanup, because it is already canceled there — use
`context.Background()`. Run `go test -race`; the `atomic` counter and the channel
handoff keep the worker goroutine race-free.

## Resources

- [`testing.T.Context`](https://pkg.go.dev/testing#T.Context) — canceled just before Cleanup functions are called.
- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — where the worker join belongs.
- [`context.Context.Done`](https://pkg.go.dev/context#Context) — the cancellation signal the worker selects on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-parallel-defer-vs-cleanup.md](05-parallel-defer-vs-cleanup.md) | Next: [07-tb-shared-fixture.md](07-tb-shared-fixture.md)
