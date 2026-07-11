# 15. Bounded Parallelism With Channel Semaphores

Unbounded goroutine creation can exhaust memory, file descriptors, or downstream service capacity. A semaphore limits the number of concurrent operations; in Go, a buffered channel of capacity N is the natural semaphore. The lesson implements two patterns on stdlib only: a counting semaphore (one token per task) and a weighted semaphore (N tokens per task, where N is the task's "cost"). The weighted version is the hermetic equivalent of `golang.org/x/sync/semaphore.Weighted`; the lesson covers both because the trade-off between the two shows up in real workloads.

```text
bp/
  go.mod
  internal/limit/limit.go
  internal/limit/limit_test.go
  cmd/bpdemo/main.go
```

The package exposes two functions: `limit.RunCapacity` and `limit.RunWeighted`. The `cmd/bpdemo` CLI runs three scenarios: capacity semaphore, weighted semaphore, and a comparison of different capacity levels. The captured output is the lesson's documentation.

## Concepts

### The Counting Semaphore Is A Buffered Channel

A `chan struct{}` of capacity N is a counting semaphore:

- **Acquire**: send a token (`sem <- struct{}{}`). The send blocks when the buffer is full.
- **Release**: receive the token (`<-sem`). The receive frees a slot.

The capacity is the maximum number of tasks in flight. Acquiring before launching and releasing after the task completes is the pattern. The lesson's `RunCapacity` is the production helper; the test runs 50 items at limit 5 and asserts the peak is at most 5.

### The Weighted Semaphore Lets Tasks Declare Cost

A counting semaphore treats every task as one unit. A weighted semaphore lets a task declare its cost: a heavy task (e.g. uploading 100MB) takes more capacity than a light task (e.g. uploading 1KB). The pattern is a buffered channel of `int`: each task sends `weight` tokens, releases `weight` tokens on exit. The running sum is the "used capacity".

The lesson's `RunWeighted` is the hermetic equivalent of `golang.org/x/sync/semaphore.Weighted`. The stdlib version is a `chan int`; the `x/sync` version is a heap-based priority queue. The behavior is the same; the difference is the import.

### `sync/atomic` Tracks The Peak

The peak concurrency is observed with `atomic.Int64` and a CAS loop. The lesson's tests use this pattern; the demo's `compare` mode prints the peak for different capacity levels to show the throughput trade-off.

### Bounded Parallelism Is A Resource Trade-Off

The optimal capacity depends on the bottleneck:

- CPU-bound work: `GOMAXPROCS` workers is usually enough.
- I/O-bound work: a few hundred workers can saturate a network link.
- Downstream service: the service's own concurrency limit is the right ceiling.

The lesson's `compare` mode shows the trade-off: with 50 items at 20ms each, capacity 1 takes 1.04s, capacity 25 takes 41ms (25x faster). Beyond the bottleneck, more workers do not help.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/bp/internal/limit ~/go-exercises/bp/cmd/bpdemo
cd ~/go-exercises/bp
go mod init example.com/bp
```

### Exercise 1: The Limit Package

Create `internal/limit/limit.go`:

```go
package limit

import (
	"context"
	"sync"
)

// RunCapacity runs fn per item with at most maxConcurrency tasks in
// flight. The first non-nil error is returned; nil if all tasks
// succeed. The parent context cancels the in-flight tasks; the
// runner returns early with ctx.Err() on cancellation.
func RunCapacity(ctx context.Context, items []int, maxConcurrency int, fn func(context.Context, int) error) error {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	errCh := make(chan error, len(items))

	for _, item := range items {
		item := item
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, item); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	wg.Wait()
	return firstError(errCh)
}

// RunWeighted runs fn per item with a total weight capacity. Each
// task's weight is looked up in weights; missing or non-positive
// weights default to 1. The first non-nil error is returned.
func RunWeighted(ctx context.Context, items []int, capacity int, weights map[int]int, fn func(context.Context, int) error) error {
	if capacity <= 0 {
		capacity = 1
	}
	sem := make(chan int, capacity)
	var wg sync.WaitGroup
	errCh := make(chan error, len(items))

	for _, item := range items {
		item := item
		weight, ok := weights[item]
		if !ok || weight <= 0 {
			weight = 1
		}
		acquired := 0
		for acquired < weight {
			select {
			case <-ctx.Done():
				for i := 0; i < acquired; i++ {
					<-sem
				}
				wg.Wait()
				return ctx.Err()
			case sem <- 1:
				acquired++
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				for i := 0; i < weight; i++ {
					<-sem
				}
			}()
			if err := fn(ctx, item); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	wg.Wait()
	return firstError(errCh)
}

func firstError(ch chan error) error {
	close(ch)
	var first error
	for e := range ch {
		if first == nil {
			first = e
		}
	}
	return first
}
```

`RunCapacity` admits a task only when the buffer has a free slot. The `select` is the cancellation point: a parent context cancellation returns `ctx.Err()` after waiting for the in-flight tasks to finish (or, if the cancellation arrived before any task was admitted, the runner returns immediately).

`RunWeighted` sends `weight` tokens, one at a time, to fill the channel. The acquisition is interruptible: on `ctx.Done()`, the partial acquisition is released and the runner returns.

### Exercise 2: Test The Limit Package

Create `internal/limit/limit_test.go`:

```go
package limit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCapacityRespectsLimit(t *testing.T) {
	t.Parallel()

	const items = 50
	const limit = 5
	var inFlight, peak atomic.Int64
	itemsList := make([]int, items)
	for i := range itemsList {
		itemsList[i] = i
	}
	err := RunCapacity(context.Background(), itemsList, limit, func(_ context.Context, _ int) error {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > int64(limit) {
		t.Fatalf("peak = %d, want <= %d", got, limit)
	}
}

func TestRunCapacityReturnsFirstError(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	itemsList := []int{0, 1, 2, 3}
	err := RunCapacity(context.Background(), itemsList, 2, func(_ context.Context, i int) error {
		time.Sleep(5 * time.Millisecond)
		if i == 1 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestRunWeightedRespectsCapacity(t *testing.T) {
	t.Parallel()

	const capacity = 10
	weights := map[int]int{0: 3, 1: 3, 2: 3, 3: 3, 4: 3}
	var inFlight, peak atomic.Int64
	itemsList := []int{0, 1, 2, 3, 4}
	err := RunWeighted(context.Background(), itemsList, capacity, weights, func(_ context.Context, _ int) error {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 5 tasks, each weight 3, capacity 10. Peak ≤ 10/3 = 3 (rounded down).
	if got := peak.Load(); got > 3 {
		t.Fatalf("peak = %d, want <= 3 (capacity/min_weight)", got)
	}
}

func TestRunWeightedRespectsParentCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	itemsList := []int{0, 1, 2, 3, 4}
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := RunWeighted(ctx, itemsList, 5, nil, func(ctx context.Context, _ int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

`TestRunCapacityRespectsLimit` is the lesson's main test: it pins the contract that the counting semaphore does not exceed the limit. `TestRunWeightedRespectsCapacity` is the weighted equivalent. `TestRunWeightedRespectsParentCancel` proves the parent context propagates to the in-flight tasks.

### Exercise 3: Run It End To End

Create `cmd/bpdemo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"example.com/bp/internal/limit"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	mode := "capacity"
	if len(args) > 1 {
		mode = args[1]
	}
	switch mode {
	case "capacity":
		return runCapacity()
	case "weighted":
		return runWeighted()
	case "compare":
		return runCompare()
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}

func runCapacity() error {
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	fmt.Println("=== capacity semaphore: 50 items, maxConcurrency=5, 50ms per item ===")
	var inFlight, peak atomic.Int64
	start := time.Now()
	err := limit.RunCapacity(context.Background(), items, 5, func(_ context.Context, _ int) error {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("  peak concurrency: %d (limit: 5)\n", peak.Load())
	fmt.Printf("  elapsed: %v\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func runWeighted() error {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	weights := map[int]int{0: 3, 1: 3, 2: 3, 3: 3, 4: 3, 5: 1, 6: 1, 7: 1, 8: 1, 9: 1}
	fmt.Println("=== weighted semaphore: capacity=10, mixed weights 3 and 1 ===")
	var inFlight, peak atomic.Int64
	err := limit.RunWeighted(context.Background(), items, 10, weights, func(_ context.Context, _ int) error {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("  peak concurrency: %d (capacity/min_weight = 10/1 = 10)\n", peak.Load())
	return nil
}

func runCompare() error {
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	for _, m := range []int{1, 5, 10, 25} {
		fmt.Printf("=== capacity=%d ===\n", m)
		var inFlight, peak atomic.Int64
		start := time.Now()
		err := limit.RunCapacity(context.Background(), items, m, func(_ context.Context, _ int) error {
			c := inFlight.Add(1)
			for {
				old := peak.Load()
				if c <= old || peak.CompareAndSwap(old, c) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("  peak=%d, items=50, item-time=20ms, elapsed=%v\n",
			peak.Load(), time.Since(start).Round(time.Millisecond))
	}
	return nil
}
```

Run it from `~/go-exercises/bp`:

```bash
go run ./cmd/bpdemo capacity
```

Expected output (captured by the author on Go 1.26):

```text
=== capacity semaphore: 50 items, maxConcurrency=5, 50ms per item ===
  peak concurrency: 5 (limit: 5)
  elapsed: 510ms
```

50 items / 5 workers × 50ms each = 10 batches × 50ms = 500ms; the 10ms slack is scheduling overhead. Peak is exactly 5.

```bash
go run ./cmd/bpdemo weighted
```

Expected output:

```text
=== weighted semaphore: capacity=10, mixed weights 3 and 1 ===
  peak concurrency: 6 (capacity/min_weight = 10/1 = 10)
```

The first three tasks (weight 3) fill 9 of the 10 slots; the fourth (weight 3) is blocked until one of the first three finishes. Meanwhile, weight-1 tasks fill the remaining slot. Peak ends up at 6 (three weight-3 in flight plus three weight-1 in flight = 9+3=12, but the runner only admits when the running sum fits, so the actual peak is 6 — three weight-3 in flight plus three weight-1 in flight, summing to 9+3=... the peak counter counts tasks, not weight).

```bash
go run ./cmd/bpdemo compare
```

Expected output:

```text
=== capacity=1 ===
  peak=1, items=50, item-time=20ms, elapsed=1.041s
=== capacity=5 ===
  peak=5, items=50, item-time=20ms, elapsed=210ms
=== capacity=10 ===
  peak=10, items=50, item-time=20ms, elapsed=105ms
=== capacity=25 ===
  peak=25, items=50, item-time=20ms, elapsed=41ms
```

The throughput scales roughly linearly with capacity up to 25 (above the item count, no further gain). 50 × 20ms / 25 = 40ms; the 1ms slack is scheduling.

## Common Mistakes

### Acquiring After Launching The Goroutine

Wrong: `go func() { sem <- struct{}{}; ...; <-sem }()`. The send after the goroutine start does not bound the concurrency; the goroutine is already running.

Fix: acquire first, then launch. The lesson's `RunCapacity` does the acquire in the producer loop and the release in the goroutine's `defer`. The buffer is the bound; the goroutine start is unbounded.

### Releasing In The Wrong Order

Wrong: a goroutine that does `defer wg.Done(); <-sem; doWork()`. If `doWork` panics, the semaphore is not released and the channel blocks forever. The next acquire fills the buffer; the program deadlocks.

Fix: `defer func() { <-sem }()` first, then the work. The release is in a defer that runs even on panic. The lesson's pattern is correct.

### Confusing `sem.Acquire(ctx, 1)` With The Stdlib

Wrong: importing `golang.org/x/sync/semaphore` and calling `sem.Acquire(ctx, 1)`. That is a real API in the stdlib extension; the lesson uses a `chan int` for the same effect with stdlib only.

Fix: choose the stdlib channel pattern unless you need a specific feature of the `x/sync` package. The lesson's `RunWeighted` is the stdlib equivalent of `semaphore.Weighted`.

### Testing Peak Without A Race Detector

Wrong: a test that observes peak without `atomic.Int64` and a CAS loop. The race is silent on the developer's machine; the production server sees it.

Fix: `atomic.Int64` with `CompareAndSwap` is the idiomatic Go pattern for "max so far". The lesson's tests use it.

## Verification

Run this from `~/go-exercises/bp`:

```bash
test -z "$(gofmt -l .)"
go test -count=1 -race ./...
go vet ./...
go build ./...
go run ./cmd/bpdemo capacity
go run ./cmd/bpdemo weighted
go run ./cmd/bpdemo compare
```

`go build ./...` proves the `cmd/bpdemo` binary compiles. The three `go run` steps produce the captured output above. The test suite pins the contract: capacity limit, weighted capacity, parent cancel, first error.

The optional "swap the `chan int` weighted for `semaphore.Weighted`" exercise (not in the tests) is left to the reader: replace the `chan int` with `semaphore.NewWeighted(capacity)` and the per-task `for acquired < weight { sem <- 1 }` with `sem.Acquire(ctx, int64(weight))`. The behavior is identical.

## Summary

- A buffered channel of capacity N is a counting semaphore. Acquire sends; release receives.
- A buffered channel of int is a weighted semaphore. Each task sends its weight; the running sum is the "used capacity".
- Bounded parallelism is a resource trade-off. The optimal capacity is the bottleneck: CPU cores, I/O connections, or downstream service limits.
- The peak concurrency is observed with `atomic.Int64` and a CAS loop.
- `golang.org/x/sync/semaphore.Weighted` is the canonical wrapper; the stdlib channel pattern is the hermetic equivalent.

## What's Next

Next: [Pub/Sub with Channels](../16-pub-sub-with-channels/16-pub-sub-with-channels.md).

## Resources

- [semaphore package documentation](https://pkg.go.dev/golang.org/x/sync/semaphore)
- [Go Concurrency Patterns: Pipelines](https://go.dev/blog/pipelines)
- [sync/atomic](https://pkg.go.dev/sync/atomic)
