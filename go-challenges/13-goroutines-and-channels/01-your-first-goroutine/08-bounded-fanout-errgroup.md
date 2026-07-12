# Exercise 8: Fan Out to an External API With a Concurrency Cap and First-Error Cancel

The hand-rolled `FanOut` was a good teacher, but production fan-out against a real
dependency needs two more things a raw `WaitGroup` does not give you: a cap on how
many calls are in flight at once (a rate-limited third-party API or a bounded
connection pool will not tolerate unbounded concurrency), and first-error
cancellation (when one call fails, stop the rest instead of burning quota on work
whose result you will discard). `golang.org/x/sync/errgroup` provides both. This
exercise is the production-grade evolution of the earlier fan-out.

This module imports an external package (`golang.org/x/sync/errgroup`), so the
gate must fetch it (`GOFLAGS=-mod=mod`). It is otherwise fully self-contained.

## What you'll build

```text
batch/                       independent module: example.com/batch
  go.mod                     requires golang.org/x/sync
  batch.go                   ProcessBatch(ctx, items, limit, worker) error
  cmd/
    demo/
      main.go                process a batch with a concurrency cap
  batch_test.go              success, first-error+cancel, peak-in-flight <= limit
```

Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
Implement: `ProcessBatch[T any](ctx, items []T, limit int, worker func(ctx, T) error) error` using `errgroup.WithContext` and `SetLimit`.
Test: success path returns nil; error path returns the first error and remaining workers observe `ctx` cancellation; a peak-in-flight gauge never exceeds `limit`.
Verify: `GOFLAGS=-mod=mod go test -race -count=1 ./...`

Set up the module:

```bash
go get golang.org/x/sync/errgroup
```

### What errgroup buys over a raw WaitGroup

`errgroup.Group` is a `WaitGroup` that also carries an error and, via
`WithContext`, a cancellation. `errgroup.WithContext(ctx)` returns a group and a
*derived* context. Each `g.Go(func() error { ... })` runs a task; the first task
to return a non-nil error causes the group to cancel that derived context, and
`g.Wait()` returns that first error. Every other task receives the cancelled
context and can bail out of its own blocking operations by watching `ctx.Done()`.
That is the first-error-cancel behavior: one failure tears down the peers instead
of letting them run to completion.

`g.SetLimit(n)` caps concurrency. After `SetLimit`, `g.Go` blocks the caller until
the number of active goroutines drops below `n` before it launches the next one.
So the launch loop naturally paces itself: at most `n` workers run at once,
regardless of how many items you feed it. This is what keeps a batch of ten
thousand items from opening ten thousand simultaneous connections to a downstream
that allows sixteen.

Two contracts your worker must honor to make this real. First, it must take the
derived `ctx` and watch `ctx.Done()` around any blocking call, so cancellation
actually stops it early rather than after its current call finishes. Second, it
must return an error, not panic — a panic in an `errgroup` task crashes the process
just like any other goroutine (combine with the `SafeGo` pattern if a task can
panic). `ProcessBatch` is generic over the item type so it works for any batch.

Create `batch.go`:

```go
package batch

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// ProcessBatch runs worker over every item with at most limit goroutines in
// flight. It returns the first non-nil error a worker produces; when a worker
// fails, the context passed to the remaining workers is cancelled so they can
// stop early. A nil error means every item was processed successfully.
func ProcessBatch[T any](ctx context.Context, items []T, limit int, worker func(context.Context, T) error) error {
	g, ctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}
	for _, it := range items {
		g.Go(func() error {
			return worker(ctx, it)
		})
	}
	return g.Wait()
}
```

### The runnable demo

The demo processes eight items with a cap of three in flight, all succeeding, and
prints the outcome. The worker here does trivial work; in production it would be
the rate-limited API call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/batch"
)

func main() {
	items := []int{1, 2, 3, 4, 5, 6, 7, 8}
	var processed atomic.Int64

	err := batch.ProcessBatch(context.Background(), items, 3, func(ctx context.Context, n int) error {
		processed.Add(1)
		return nil
	})

	fmt.Printf("processed=%d err=%v\n", processed.Load(), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed=8 err=nil
```

### Tests

`TestProcessBatchSuccess` asserts every item is processed and `Wait` returns nil.
`TestProcessBatchFirstErrorCancels` feeds one poison item that returns a sentinel
error while the other workers block on `ctx.Done`; it asserts `Wait` returns the
sentinel (via `errors.Is`) and that every other worker observed the cancellation.
`TestProcessBatchRespectsLimit` uses a gate to hold workers in flight and a peak
gauge to assert concurrency reaches — and never exceeds — the limit.

Create `batch_test.go`:

```go
package batch

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestProcessBatchSuccess(t *testing.T) {
	t.Parallel()

	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	var processed atomic.Int64
	err := ProcessBatch(context.Background(), items, 8, func(ctx context.Context, n int) error {
		processed.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("Wait err = %v, want nil", err)
	}
	if got := processed.Load(); got != int64(len(items)) {
		t.Fatalf("processed = %d, want %d", got, len(items))
	}
}

var errPoison = errors.New("poison item")

func TestProcessBatchFirstErrorCancels(t *testing.T) {
	t.Parallel()

	const n = 8
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	const poison = 0

	var cancelled atomic.Int64
	// limit >= n so every worker starts and can observe the cancellation.
	err := ProcessBatch(context.Background(), items, n, func(ctx context.Context, item int) error {
		if item == poison {
			return errPoison
		}
		<-ctx.Done() // block until the poison item cancels the group
		cancelled.Add(1)
		return ctx.Err()
	})

	if !errors.Is(err, errPoison) {
		t.Fatalf("Wait err = %v, want errPoison", err)
	}
	if got := cancelled.Load(); got != n-1 {
		t.Fatalf("workers that observed cancellation = %d, want %d", got, n-1)
	}
}

func TestProcessBatchRespectsLimit(t *testing.T) {
	t.Parallel()

	const (
		n     = 30
		limit = 3
	)
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}

	gate := make(chan struct{})
	var inFlight, peak atomic.Int64

	errCh := make(chan error, 1)
	go func() {
		errCh <- ProcessBatch(context.Background(), items, limit, func(ctx context.Context, item int) error {
			cur := inFlight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			<-gate // hold the worker in flight
			inFlight.Add(-1)
			return nil
		})
	}()

	// Wait until the cap is saturated, then release everyone.
	for peak.Load() < int64(limit) {
		runtime.Gosched()
	}
	close(gate)

	if err := <-errCh; err != nil {
		t.Fatalf("Wait err = %v, want nil", err)
	}
	if got := peak.Load(); got != int64(limit) {
		t.Fatalf("peak in-flight = %d, want exactly %d", got, limit)
	}
}
```

## Review

`ProcessBatch` is correct when the success path processes every item and returns
nil, the error path returns the first worker error and cancels the derived context
so peers stop, and the peak concurrency never exceeds the limit. The cancellation
contract only works if the worker actually watches `ctx.Done()` — a worker that
ignores the context will run to completion regardless, which is why the error test
blocks each non-poison worker on `<-ctx.Done()`. The cap test proves both
directions: `peak == limit` shows the cap is reached, and because `SetLimit`
launches at most `limit` goroutines, it is also never exceeded. This is the shape
real backend batch code uses against databases, replicas, and third-party APIs;
`FanOut` was the training version of it.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup)
- [errgroup.Group.SetLimit](https://pkg.go.dev/golang.org/x/sync/errgroup#Group.SetLimit)
- [context package](https://pkg.go.dev/context)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-scatter-gather-results-channel.md](07-scatter-gather-results-channel.md) | Next: [09-no-leak-goroutine-count-guard.md](09-no-leak-goroutine-count-guard.md)
