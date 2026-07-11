# Exercise 2: The Weighted Semaphore

When tasks are not equal in cost, a counting semaphore is the wrong tool: it would let one giant task and one tiny task each consume a single slot of a shared budget. A weighted semaphore fixes that by letting each task declare a weight and consume that many units of a total capacity. This module builds one from a buffered channel of tokens and proves the real invariant — that in-flight weight never exceeds the capacity.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sem.go                RunWeighted, firstError
cmd/
  demo/
    main.go           mixed-weight workload; report peak in-flight WEIGHT
sem_test.go           peak weight <= capacity under -race; clamp; cancellation
```

- Files: `sem.go`, `cmd/demo/main.go`, `sem_test.go`.
- Implement: `RunWeighted(ctx, items, capacity, weights, fn)` and the `firstError` helper.
- Test: `sem_test.go` asserts the in-flight weight never exceeds the capacity, that an oversized weight is clamped rather than deadlocking, and that a cancelled parent stops the runner.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p weighted-semaphore/cmd/demo && cd weighted-semaphore
go mod init example.com/weighted-semaphore
```

### What the invariant actually is

The unit that is bounded by a weighted semaphore is total weight, not task count, and getting that distinction right is the whole exercise. With a capacity of ten and tasks of weight three, the number of tasks in flight is whatever the scheduler happens to produce — it might be three, it might briefly be more once light tasks interleave — but the sum of their weights is, by construction, never more than ten. The earlier version of this lesson reported a task-count "peak" of six and then tried to explain it with arithmetic that contradicted itself, because task count under a weighted budget is genuinely non-deterministic and not the thing the semaphore controls. The honest, repeatable measurement is the peak in-flight weight, and this module measures and asserts exactly that.

The channel is a `chan struct{}` of capacity equal to the budget; each token is one unit of weight. A task of weight w acquires by sending w tokens and releases by receiving w tokens. The buffer occupancy is the running sum of in-flight weight, and the channel's own accounting enforces the ceiling: the producer cannot send a token into a full buffer, so it cannot push the in-flight weight past the capacity.

### The partial-acquisition edge, and why we clamp

Acquiring w tokens is not one atomic operation; it is w separate sends in a loop. A producer partway through — say it wanted three tokens, got two, and is now blocked on the third — is holding a partial acquisition that stalls every later task, because this runner has a single producer loop and that loop is stuck mid-acquire. That is not a bug; it is the price of the invariant, and it resolves the instant a running task releases enough tokens for the third send to complete. The acquire loop is interruptible: each send sits in a `select` alongside `ctx.Done()`, and on cancellation the producer gives back the tokens it has already taken before returning, so no capacity is stranded.

There is one input that turns the partial acquisition from a stall into a permanent deadlock: a single weight larger than the entire capacity. The producer can never gather more tokens than the buffer can hold, so it would wait forever for a slot that can never open. The robust fix is to clamp each weight to the capacity, so the largest conceivable task still fits in the budget exactly once. This module clamps; it is the difference between a runner that degrades gracefully on bad input and one that hangs. `golang.org/x/sync/semaphore.Weighted` is the packaged equivalent of this construction — its `Acquire(ctx, n)` is the token loop plus a wait queue — and reaching for it is the right call in production when you do not want to hand-roll the loop; building it here once is how you understand what that package does.

Create `sem.go`:

```go
package sem

import (
	"context"
	"sync"
)

// RunWeighted runs fn for each item against a shared weight budget. Each
// item's weight is looked up in weights (missing or non-positive defaults to
// 1); a weight larger than capacity is clamped to capacity so the largest
// task still fits. The total weight of in-flight tasks never exceeds capacity.
// It returns the first non-nil error, or ctx.Err() if the parent is cancelled.
func RunWeighted(ctx context.Context, items []int, capacity int, weights map[int]int, fn func(context.Context, int) error) error {
	if capacity <= 0 {
		capacity = 1
	}
	sem := make(chan struct{}, capacity)
	var wg sync.WaitGroup
	errCh := make(chan error, len(items))

	for _, item := range items {
		weight := 1
		if w, ok := weights[item]; ok && w > 0 {
			weight = w
		}
		if weight > capacity {
			weight = capacity // clamp: an oversized weight would deadlock.
		}

		// Acquire weight tokens one at a time; the loop is interruptible.
		acquired := 0
	acquire:
		for acquired < weight {
			select {
			case <-ctx.Done():
				for i := 0; i < acquired; i++ {
					<-sem // give back the partial acquisition.
				}
				wg.Wait()
				return ctx.Err()
			case sem <- struct{}{}:
				acquired++
			}
		}
		_ = acquire

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

// firstError closes ch and returns the earliest non-nil error it holds.
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

### The runnable demo

The demo runs a mixed workload — some heavy tasks of weight three, some light tasks of weight one — against a capacity of ten, and measures the peak in-flight weight rather than the peak task count. Each task adds its weight to an atomic on entry, records the high-water mark with a compare-and-swap loop, sleeps, then subtracts its weight. The reported peak must be at most the capacity, and in this workload it reaches the capacity exactly, because there is always more weight queued than the budget will admit. This is the deterministic, defensible number; task count is deliberately not reported because it is not what the semaphore bounds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/weighted-semaphore/sem"
)

func main() {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	weights := map[int]int{0: 3, 1: 3, 2: 3, 3: 3, 4: 3} // 5-9 default to weight 1
	const capacity = 10

	weightOf := func(i int) int64 {
		if w, ok := weights[i]; ok {
			return int64(w)
		}
		return 1
	}

	var inFlightWeight, peakWeight atomic.Int64
	err := sem.RunWeighted(context.Background(), items, capacity, weights, func(_ context.Context, i int) error {
		c := inFlightWeight.Add(weightOf(i))
		for {
			old := peakWeight.Load()
			if c <= old || peakWeight.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlightWeight.Add(-weightOf(i))
		return nil
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("capacity=%d peak_in_flight_weight=%d\n", capacity, peakWeight.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (peak weight is bounded by capacity and reaches it here):

```text
capacity=10 peak_in_flight_weight=10
```

### Tests

The peak-weight test pins the real invariant: it sums in-flight weights and asserts the maximum never exceeds the capacity, under `-race` because the counter is shared. The clamp test feeds a single weight larger than the capacity and requires the runner to finish instead of hanging — without the clamp it would deadlock, so this test is the regression guard for that edge. The cancellation test proves the interruptible acquire loop gives back its partial tokens and returns `context.Canceled`.

Create `sem_test.go`:

```go
package sem

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunWeightedRespectsCapacity(t *testing.T) {
	t.Parallel()

	const capacity = 10
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	weights := map[int]int{0: 3, 1: 3, 2: 3, 3: 3, 4: 3}
	weightOf := func(i int) int64 {
		if w, ok := weights[i]; ok {
			return int64(w)
		}
		return 1
	}

	var inFlight, peak atomic.Int64
	err := RunWeighted(context.Background(), items, capacity, weights, func(_ context.Context, i int) error {
		c := inFlight.Add(weightOf(i))
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-weightOf(i))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > int64(capacity) {
		t.Fatalf("peak in-flight weight = %d, want <= %d", got, capacity)
	}
}

func TestRunWeightedClampsOversizedWeight(t *testing.T) {
	t.Parallel()

	// Weight 100 exceeds capacity 4; without clamping the runner would
	// deadlock. The clamp lets it run, so this must complete.
	done := make(chan error, 1)
	go func() {
		done <- RunWeighted(context.Background(), []int{0, 1}, 4, map[int]int{0: 100, 1: 100},
			func(_ context.Context, _ int) error {
				time.Sleep(2 * time.Millisecond)
				return nil
			})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWeighted deadlocked on an oversized weight")
	}
}

func TestRunWeightedRespectsCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := RunWeighted(ctx, []int{0, 1, 2, 3, 4}, 5, nil, func(ctx context.Context, _ int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

## Review

The weighted runner is correct when the in-flight weight — not the task count — stays within the capacity, when an oversized weight is clamped rather than deadlocking, and when cancellation returns the partial acquisition. The trap that the original version of this lesson fell into is asserting on task count, which is non-deterministic under a weighted budget; assert on summed weight instead, which is the invariant the channel actually enforces. Confirm the clamp is present by reading the test that feeds a weight larger than capacity: remove the clamp and that test hangs, which is exactly the production failure it guards against. Keep the weight counter atomic and run under `-race`.

## Resources

- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — the canonical weighted semaphore; `Acquire(ctx, n)` is the token loop plus a wait queue.
- [Go Blog: errgroup and bounded concurrency](https://go.dev/blog/pipelines) — bounding stages with channels, the foundation the weighted variant extends.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — the atomic counter and compare-and-swap used to measure peak weight.

---

Back to [01-counting-semaphore.md](01-counting-semaphore.md) | Next: [03-bounded-http-fanout.md](03-bounded-http-fanout.md)
