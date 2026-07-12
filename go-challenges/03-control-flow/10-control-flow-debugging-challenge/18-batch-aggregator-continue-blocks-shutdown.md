# Exercise 18: Batch Aggregator Loop Continues After Context Cancellation, Blocking Graceful Shutdown

**Nivel: Intermedio** — validacion rapida (un test corto).

A background aggregator that buffers items and flushes them in batches has
to react to `ctx.Done()` exactly like every other well-behaved worker
loop: flush whatever is still buffered, then actually leave the loop. A
`continue` in place of a `return` inside that `case` runs the flush once
and then loops straight back into a `select` where the *same* already-fired
`ctx.Done()` channel is still ready — spinning on it forever instead of
exiting, so the goroutine that graceful shutdown is waiting on never signals
it is done. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
batchagg/                   independent module: example.com/batch-aggregator-continue-blocks-shutdown
  go.mod
  batchagg.go                Aggregator, New, Run
  cmd/
    demo/
      main.go                runnable demo: buffer items, cancel, observe the flush and stop
  batchagg_test.go            shutdown flushes the remainder and stops promptly, plus a full-batch flush
```

- Files: `batchagg.go`, `cmd/demo/main.go`, `batchagg_test.go`.
- Implement: `Aggregator` (`Items`, `Stopped`) and `Run(ctx context.Context)` that flushes full batches as they fill and flushes any remainder on shutdown before returning.
- Test: cancel the context after sending a partial batch and assert `Stopped()` closes within a short bound with the remainder flushed exactly once; a second test asserts a full batch flushes without any shutdown signal.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/18-batch-aggregator-continue-blocks-shutdown/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/18-batch-aggregator-continue-blocks-shutdown
```

### Why continue is not a smaller version of return here

`Run`'s `ctx.Done()` case has to do two things in order: flush whatever is
still buffered, and stop the loop. The bug swaps the second half for a
`continue`, which reads at a glance like "handle shutdown, then keep the
loop going a little more defensively":

```go
case <-ctx.Done():
	if len(buf) > 0 {
		a.onFlush(buf)
	}
	continue // BUG: should be return
```

`continue` in a `select` case does exactly what it does anywhere else in a
`for` loop: it jumps to the top of the *enclosing* `for`, which here goes
straight back into another `select`. But `ctx.Done()`'s channel was closed
by the cancellation, and a closed channel stays permanently ready — every
future receive on it returns immediately. So the very next iteration's
`select` sees `ctx.Done()` ready again (this time with `len(buf) == 0`,
since it was already flushed), takes the same case again, and loops again,
forever. The loop never blocks on anything again; it busy-spins pinning a
CPU core, and because `Run` never returns, the `defer close(a.stopped)` at
its top never fires — so any code waiting on `Stopped()` to orchestrate a
graceful shutdown waits forever alongside it. The fix is `return` after the
shutdown flush: the loop's job is done, and unlike `continue`, `return`
actually leaves the function, letting the deferred close run and the
goroutine exit.

Create `batchagg.go`:

```go
package batchagg

import "context"

// Aggregator buffers items and flushes them in batches, either when the
// buffer reaches batchSize or when the run loop is asked to shut down.
type Aggregator struct {
	items     chan int
	batchSize int
	onFlush   func([]int)
	stopped   chan struct{}
}

// New creates an Aggregator that flushes every batchSize items via onFlush.
func New(batchSize int, onFlush func([]int)) *Aggregator {
	return &Aggregator{
		items:     make(chan int),
		batchSize: batchSize,
		onFlush:   onFlush,
		stopped:   make(chan struct{}),
	}
}

// Items returns the channel callers send values on.
func (a *Aggregator) Items() chan<- int { return a.items }

// Stopped is closed once Run has flushed any remaining buffer and returned.
func (a *Aggregator) Stopped() <-chan struct{} { return a.stopped }

// Run consumes items until ctx is cancelled, flushing full batches as they
// fill and flushing whatever remains buffered on shutdown before returning.
func (a *Aggregator) Run(ctx context.Context) {
	defer close(a.stopped)

	var buf []int
	for {
		select {
		case v := <-a.items:
			buf = append(buf, v)
			if len(buf) >= a.batchSize {
				a.onFlush(buf)
				buf = nil
			}
		case <-ctx.Done():
			if len(buf) > 0 {
				a.onFlush(buf)
			}
			return
		}
	}
}
```

### The runnable demo

The demo sends three items into a batch size of ten (never fills), cancels
the context, and waits on `Stopped()` to show the remainder is flushed
exactly once and the goroutine actually exits.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/batch-aggregator-continue-blocks-shutdown"
)

func main() {
	var flushed [][]int
	agg := batchagg.New(10, func(batch []int) {
		cp := append([]int(nil), batch...)
		flushed = append(flushed, cp)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agg.Run(ctx)
		close(done)
	}()

	agg.Items() <- 1
	agg.Items() <- 2
	agg.Items() <- 3

	cancel()
	<-agg.Stopped()
	<-done

	fmt.Println("flushed batches:", flushed)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
flushed batches: [[1 2 3]]
```

### Tests

`TestShutdownFlushesRemainderAndStopsPromptly` races `Stopped()` and the
goroutine's own completion signal against a `time.After` bound — proving
`Run` actually returns after cancellation rather than spinning forever —
and then asserts the remainder was flushed exactly once.
`TestFullBatchFlushesWithoutWaitingForShutdown` confirms a full batch
flushes on its own, with no cancellation involved.

Create `batchagg_test.go`:

```go
package batchagg

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestShutdownFlushesRemainderAndStopsPromptly(t *testing.T) {
	var flushed [][]int
	agg := New(10, func(batch []int) {
		cp := append([]int(nil), batch...)
		flushed = append(flushed, cp)
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		agg.Run(ctx)
		close(runDone)
	}()

	agg.Items() <- 1
	agg.Items() <- 2
	agg.Items() <- 3

	cancel()

	select {
	case <-agg.Stopped():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Stopped() did not close promptly after cancellation")
	}

	select {
	case <-runDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return promptly after cancellation")
	}

	want := [][]int{{1, 2, 3}}
	if !reflect.DeepEqual(flushed, want) {
		t.Fatalf("flushed = %v, want %v", flushed, want)
	}
}

func TestFullBatchFlushesWithoutWaitingForShutdown(t *testing.T) {
	flushedCh := make(chan []int, 1)
	agg := New(2, func(batch []int) {
		cp := append([]int(nil), batch...)
		flushedCh <- cp
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agg.Run(ctx)

	agg.Items() <- 1
	agg.Items() <- 2

	// The batch is full, so it must flush without any shutdown signal.
	select {
	case got := <-flushedCh:
		want := []int{1, 2}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("flushed = %v, want %v", got, want)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("full batch was never flushed")
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Run` is correct when cancellation always leads to exactly one flush of
the remaining buffer followed by the goroutine actually exiting — proven
by racing `Stopped()` against a timeout, not by merely checking the flush
happened. The mistake this design avoids is treating `continue` as a
cautious version of `return` inside a `select` case: they are not
interchangeable, because a `continue` re-enters the same `select`, and a
context's `Done()` channel stays permanently ready once closed, so any
`case <-ctx.Done()` arm that does not end the loop with `return` (or a
labeled `break`) turns "shut down" into "spin on the shutdown signal
forever."

## Resources

- [context package](https://pkg.go.dev/context) — `Done()` returns a channel that is closed once, and reading from a closed channel never blocks again.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — `continue` terminates the current iteration of the innermost enclosing `for` loop; a `select` block does not change which loop it targets.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the standard `select`-with-`ctx.Done()` shutdown shape this module implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-polling-retry-unhandled-error-case.md](17-polling-retry-unhandled-error-case.md) | Next: [19-log-pipeline-drain-skipped-by-continue.md](19-log-pipeline-drain-skipped-by-continue.md)
