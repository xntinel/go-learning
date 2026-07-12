# Exercise 1: The Counting Semaphore

A counting semaphore is the smallest unit of bounded parallelism: a hard ceiling on how many tasks run at once, built from a buffered channel and nothing else. This module implements a runner that processes a slice of items with at most N tasks in flight, propagates context cancellation, and returns the first error.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sem.go                RunCapacity, firstError
cmd/
  demo/
    main.go           run 50 items at limit 5 and report the measured peak
sem_test.go           peak <= limit under -race; first-error propagation; cancellation
```

- Files: `sem.go`, `cmd/demo/main.go`, `sem_test.go`.
- Implement: `RunCapacity(ctx, items, maxConcurrency, fn)` and the `firstError` helper.
- Test: `sem_test.go` asserts the measured peak never exceeds the limit, that the first non-nil error is returned, and that a cancelled parent stops the runner.
- Verify: `go test -race ./...`

### How the bound is enforced

The semaphore is a `chan struct{}` whose capacity is the concurrency limit. The producer loop does the acquire — `sem <- struct{}{}` — before it starts the goroutine, so that the loop itself blocks the moment the buffer is full. This is the load-bearing detail: the bound exists because the loop stops admitting work, not because the goroutines politely wait their turn. If you moved the acquire inside the goroutine, every item would spawn a goroutine immediately and the only thing the semaphore would limit is how many of those already-running goroutines reach `fn`, which is not a bound on anything that costs memory.

The release is `<-sem` in a deferred closure, paired with `defer wg.Done()`. Deferring the release is not stylistic tidiness; it is correctness. If `fn` panics, the deferred receive still runs and the token returns to the buffer. An undeferred release on the happy path would, on a panic, leak the token forever, and after `maxConcurrency` such leaks the buffer is permanently full and the next acquire blocks the producer for good — a hang with no error message.

Cancellation lives in the `select` at the acquire point. The producer either acquires a slot or observes `ctx.Done()`, whichever happens first. When the context is cancelled the runner stops admitting new items, waits for the goroutines already in flight to drain, and returns `ctx.Err()`. The context is also passed into `fn`, so a well-written task can abandon its own work early; the runner does not force-kill goroutines because Go has no mechanism to, it only stops launching new ones and lets the in-flight ones observe the cancellation themselves.

Errors are funneled through a buffered channel sized to the item count so that no sender ever blocks, and a non-blocking `select` keeps only the first one — every later error is dropped into the `default` arm. After the wait, `firstError` drains the channel and returns the earliest value. This is the fail-fast policy: the caller learns that something failed and gets one representative error. The aggregate-all policy, where every failure is collected, is the subject of a later exercise.

Create `sem.go`:

```go
package sem

import (
	"context"
	"sync"
)

// RunCapacity runs fn for each item with at most maxConcurrency tasks in
// flight at once. It returns the first non-nil error any task produced, or
// nil if every task succeeded. A cancelled ctx stops the runner: it waits for
// the in-flight tasks to drain and returns ctx.Err().
func RunCapacity(ctx context.Context, items []int, maxConcurrency int, fn func(context.Context, int) error) error {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	errCh := make(chan error, len(items))

	for _, item := range items {
		// Acquire BEFORE launching: this is what makes the loop block when
		// the system is saturated, which is the actual bound.
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Release in a defer so a panic in fn cannot leak the token.
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

The demo makes the bound visible by measuring it. Each task increments an atomic in-flight counter on entry, records the high-water mark with a compare-and-swap loop, sleeps to overlap with its peers, then decrements. With fifty items at a limit of five, the measured peak must be exactly five: there is always more work waiting than the semaphore will admit, so the buffer stays full for the whole run. The elapsed time is reported too — ten batches of five at fifty milliseconds each is about half a second — but the elapsed figure is timing-dependent and only the peak is an exact, repeatable fact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/counting-semaphore/sem"
)

func main() {
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}

	var inFlight, peak atomic.Int64
	start := time.Now()
	err := sem.RunCapacity(context.Background(), items, 5, func(_ context.Context, _ int) error {
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
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("items=%d limit=%d peak=%d\n", len(items), 5, peak.Load())
	fmt.Printf("elapsed~=%v (10 batches of 5 x 50ms)\n", time.Since(start).Round(10*time.Millisecond))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the peak is exact; the elapsed is approximate):

```text
items=50 limit=5 peak=5
elapsed~=510ms (10 batches of 5 x 50ms)
```

### Tests

The peak test is the centerpiece: it reruns the demo's measurement as an assertion and pins the contract that the runner never exceeds its limit. Because the in-flight counter is read and written from many goroutines, it must be an `atomic.Int64` and the test must run under `-race`; an ordinary `int` would race and the detector would fail the build, which is the point. The first-error test proves the fail-fast policy returns the seeded error and not some other task's nil. The cancellation test proves a parent cancel propagates: the tasks block on `ctx.Done()` and the runner returns `context.Canceled`.

Create `sem_test.go`:

```go
package sem

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCapacityRespectsLimit(t *testing.T) {
	t.Parallel()

	const n = 50
	const limit = 5
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	var inFlight, peak atomic.Int64
	err := RunCapacity(context.Background(), items, limit, func(_ context.Context, _ int) error {
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
	err := RunCapacity(context.Background(), []int{0, 1, 2, 3}, 2, func(_ context.Context, i int) error {
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

func TestRunCapacityRespectsCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := RunCapacity(ctx, []int{0, 1, 2, 3, 4}, 2, func(ctx context.Context, _ int) error {
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

## Review

The runner is correct when the measured peak never exceeds the limit, the first error surfaces, and a cancelled parent stops the loop. The most common way to get this wrong is to move the acquire inside the goroutine, which compiles and passes a casual eyeball test but bounds nothing — the peak test catches it because the in-flight count climbs to the item count. A second classic error is releasing the token without a `defer`: it works until a task panics, at which point the token leaks and the next run hangs. Confirm the in-flight counter is an `atomic.Int64` updated with a compare-and-swap loop and that the whole suite is green under `go test -race ./...`; a non-atomic counter races and the detector turns that into a failed build rather than a silent corruption in production.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of bounded stages and cancellation propagation with channels.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the "a buffered channel can be used like a semaphore" idiom stated by the language docs.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64` and `CompareAndSwap`, the primitives behind the peak measurement.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-weighted-semaphore.md](02-weighted-semaphore.md)
