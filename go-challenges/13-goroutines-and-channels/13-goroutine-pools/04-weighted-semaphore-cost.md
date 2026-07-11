# Exercise 4: Weight Concurrency By Job Cost With A Weighted Semaphore

Not all jobs cost the same. A thumbnail is cheap; a full-resolution render holds
far more memory. Bounding by job *count* either starves the machine (count low
enough for the big jobs, so small jobs queue needlessly) or blows memory (count
high enough for small jobs, so a burst of big ones OOMs). This exercise builds a
processing gate that bounds total *cost* with `semaphore.Weighted`: each job
acquires a weight, and the sum of in-flight weights stays under a budget.

This module is fully self-contained. It uses `golang.org/x/sync/semaphore`.

## What you'll build

```text
mediagate/                 independent module: example.com/mediagate
  go.mod                   go 1.25; require golang.org/x/sync
  gate.go                  type Gate; NewGate, Do, TryDo; ErrTooHeavy sentinel
  cmd/
    demo/
      main.go              runnable demo: mix of weight-1 and weight-4 jobs, budget 4
  gate_test.go             budget-never-exceeded, canceled-ctx, try-when-full,
                           overweight-fails-fast tests, -race
```

- Files: `gate.go`, `cmd/demo/main.go`, `gate_test.go`.
- Implement: a `Gate` over a `semaphore.Weighted` budget, with a blocking `Do(ctx, weight, fn)` and a non-blocking `TryDo(weight, fn)`, both rejecting a job heavier than the whole budget with `ErrTooHeavy`.
- Test: summed in-flight weight never exceeds the budget, `Do` honors a canceled context and does not run the job, `TryDo` returns `false` when full without blocking, and an over-budget job fails fast instead of deadlocking.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mediagate/cmd/demo
cd ~/go-exercises/mediagate
go mod init example.com/mediagate
go get golang.org/x/sync/semaphore
```

### Cost, not count, is the resource

`semaphore.NewWeighted(budget)` creates a semaphore whose total capacity is
`budget` — think of it as MB of memory, or IO credits, or any additive resource.
`Acquire(ctx, w)` takes `w` units, blocking until `w` are free or `ctx` is done;
`Release(w)` returns them. Because acquisition is by weight, the invariant the
semaphore maintains is "sum of held weights <= budget," which is exactly "total
in-flight cost is capped" — independent of how many jobs that is. A budget of 4
admits four weight-1 thumbnails, or one weight-4 render, or two weight-1 plus
nothing else until one finishes. The number of concurrent jobs floats; the cost
does not exceed the budget.

The gate wraps acquire/run/release so callers cannot forget the `Release`. `Do`
acquires, defers the release, and runs the function; the defer guarantees the
weight is returned even if the function panics or errors. That acquire-defer-run
shape is the whole discipline of using a semaphore correctly.

### The two rejection modes: over-budget and full

Two situations need explicit handling rather than a naive `Acquire`. First, a job
whose weight *exceeds the entire budget* can never be admitted. The semaphore's
`Acquire` does not fail fast on its own for this case: given a non-cancellable
context it blocks until the context is done, which for a `context.Background()`
means forever. So the gate checks `weight > budget` up front and returns
`ErrTooHeavy` immediately — failing fast is the correct behavior, because no
amount of waiting will free enough budget for a job bigger than the budget itself.

Second, under load a caller may want to *not wait*: a request that cannot get a
slot right now should be shed, not queued. `TryAcquire(w)` attempts the
acquisition without blocking and returns `false` if the budget cannot satisfy it
at that instant. `TryDo` builds on it to offer non-blocking admission: it returns
`(false, nil)` when the gate is full, so the caller can shed load. (`TryAcquire`
never blocks, so `TryDo` needs no context.)

Create `gate.go`:

```go
package mediagate

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/semaphore"
)

// ErrTooHeavy is returned when a job's weight exceeds the entire budget, so it
// could never be admitted and must be rejected rather than waited on.
var ErrTooHeavy = errors.New("job weight exceeds budget")

// Gate bounds the total in-flight COST of jobs, not their count. Each job
// acquires a weight against a fixed budget.
type Gate struct {
	sem    *semaphore.Weighted
	budget int64
}

// NewGate creates a gate admitting jobs whose combined weight is at most budget.
func NewGate(budget int64) *Gate {
	return &Gate{sem: semaphore.NewWeighted(budget), budget: budget}
}

// Do acquires weight units, runs fn, and releases them. It blocks until the
// weight is available or ctx is done. A job heavier than the whole budget is
// rejected immediately with ErrTooHeavy rather than blocking forever.
func (g *Gate) Do(ctx context.Context, weight int64, fn func() error) error {
	if weight > g.budget {
		return fmt.Errorf("weight %d over budget %d: %w", weight, g.budget, ErrTooHeavy)
	}
	if err := g.sem.Acquire(ctx, weight); err != nil {
		return err
	}
	defer g.sem.Release(weight)
	return fn()
}

// TryDo runs fn only if weight can be acquired without blocking. It returns
// (false, nil) when the gate is full, and (false, ErrTooHeavy) for an
// over-budget job. On admission it returns (true, fn()'s error).
func (g *Gate) TryDo(weight int64, fn func() error) (bool, error) {
	if weight > g.budget {
		return false, fmt.Errorf("weight %d over budget %d: %w", weight, g.budget, ErrTooHeavy)
	}
	if !g.sem.TryAcquire(weight) {
		return false, nil
	}
	defer g.sem.Release(weight)
	return true, fn()
}
```

### The runnable demo

The demo submits a mix of weight-1 and weight-4 jobs against a budget of 4 and
prints how many completed. Because the weight-4 job monopolizes the budget while
it runs, the others wait for it, but all complete.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/mediagate"
)

func main() {
	g := mediagate.NewGate(4)
	weights := []int64{1, 1, 4, 1, 1}

	var done atomic.Int64
	var wg sync.WaitGroup
	for _, w := range weights {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Do(context.Background(), w, func() error {
				time.Sleep(5 * time.Millisecond)
				done.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()
	fmt.Printf("completed: %d\n", done.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
completed: 5
```

### Tests

`TestBudgetNeverExceeded` runs many jobs of mixed weight concurrently, tracks the
summed running cost with atomics, and asserts the peak never exceeds the budget —
the core invariant. `TestCanceledContext` passes an already-cancelled context to
`Do` and asserts it returns `context.Canceled` and never runs the job.
`TestTryDoWhenFull` fills the budget with a job parked on a channel, then asserts
`TryDo` returns `false` immediately without blocking, and succeeds again after the
parked job releases. `TestOverweightFailsFast` asks for more weight than the whole
budget against `context.Background()` and asserts it returns `ErrTooHeavy`
promptly rather than deadlocking.

Create `gate_test.go`:

```go
package mediagate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetNeverExceeded(t *testing.T) {
	t.Parallel()

	const budget = 4
	g := NewGate(budget)
	weights := []int64{1, 4, 1, 1, 4, 1, 1, 1, 4, 1}

	var running, peak atomic.Int64
	var wg sync.WaitGroup
	for _, w := range weights {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Do(context.Background(), w, func() error {
				cur := running.Add(w)
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				running.Add(-w)
				return nil
			})
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > budget {
		t.Fatalf("peak running cost = %d, want <= %d", got, budget)
	}
}

func TestCanceledContext(t *testing.T) {
	t.Parallel()

	g := NewGate(4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran atomic.Bool
	err := g.Do(ctx, 1, func() error {
		ran.Store(true)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do err = %v, want context.Canceled", err)
	}
	if ran.Load() {
		t.Fatal("job ran despite canceled context")
	}
}

func TestTryDoWhenFull(t *testing.T) {
	t.Parallel()

	g := NewGate(4)
	release := make(chan struct{})
	acquired := make(chan struct{})

	go func() {
		_ = g.Do(context.Background(), 4, func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired // budget is now fully held

	ok, err := g.TryDo(1, func() error { return nil })
	if err != nil {
		t.Fatalf("TryDo err = %v, want nil", err)
	}
	if ok {
		t.Fatal("TryDo admitted a job while the gate was full")
	}

	close(release) // let the big job finish and free the budget
	// TryDo should eventually succeed once the budget is free.
	deadline := time.After(time.Second)
	for {
		ok, _ := g.TryDo(1, func() error { return nil })
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("TryDo never succeeded after budget freed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestOverweightFailsFast(t *testing.T) {
	t.Parallel()

	g := NewGate(4)
	done := make(chan error, 1)
	go func() {
		done <- g.Do(context.Background(), 10, func() error { return nil })
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrTooHeavy) {
			t.Fatalf("Do err = %v, want ErrTooHeavy", err)
		}
	case <-time.After(time.Second):
		t.Fatal("over-budget Do blocked instead of failing fast")
	}
}
```

## Review

The gate is correct when the summed weight of concurrently running jobs never
exceeds the budget — `TestBudgetNeverExceeded` proves it by watching the running
cost gauge peak at or below the budget while weight-4 jobs force the smaller ones
to wait. The two rejection paths are the senior details: an over-budget job must
fail fast (`ErrTooHeavy`) instead of blocking forever on an `Acquire` that can
never succeed, and a full gate under `TryDo` must return `false` immediately
instead of queueing. `TestCanceledContext` confirms `Do` honors cancellation and
does not run the job, matching the semaphore's contract that a done context
returns its error and leaves the semaphore unchanged.

The mistakes to avoid: forgetting `defer g.sem.Release(weight)` (the budget leaks
a little on every job until the gate wedges); calling `Acquire` for an over-budget
weight without the up-front check (a `Background` context makes that a permanent
hang); and confusing this cost bound with a *count* bound — if every job weighs 1,
`Weighted` degenerates to a counting semaphore, which is the whole point only when
weights are uniform. Run `-race` to confirm the cost gauge and the semaphore
interact cleanly.

## Resources

- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — `NewWeighted`, `Acquire`, `TryAcquire`, `Release`.
- [`context`](https://pkg.go.dev/context) — the cancellation `Acquire` observes.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounding concurrent work against a resource.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-errgroup-bounded-enrichment.md](03-errgroup-bounded-enrichment.md) | Next: [05-context-cancelable-pool.md](05-context-cancelable-pool.md)
