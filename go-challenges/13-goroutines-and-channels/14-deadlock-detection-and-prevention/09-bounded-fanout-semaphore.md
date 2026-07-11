# Exercise 9: Bounded Fan-Out That Cannot Deadlock on Acquire

Fanning out a batch of I/O tasks without a bound floods a downstream (too many open connections,
exhausted file descriptors). Bounding it with a semaphore fixes that — but introduces a new
deadlock risk: if a slot is acquired and then leaked on an error or panic path, the pool starves
until nothing can acquire and the whole batch wedges. This exercise builds a bounded fan-out with
`golang.org/x/sync/errgroup` and `SetLimit`, and shows the discipline that keeps a slot from
leaking on any path.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo and
tests. It depends on the external module `golang.org/x/sync`, so the gate runs with
`GOFLAGS=-mod=mod`.

## What you'll build

```text
fanout/                    independent module: example.com/fanout
  go.mod                   go 1.25; requires golang.org/x/sync
  fanout.go                Process: errgroup with SetLimit(K); observed-peak tracking
  cmd/
    demo/
      main.go              fan out over a batch; report peak concurrency <= K
  fanout_test.go           peak <= K; error cancels group; no slot leak (-race)
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `Process(ctx, tasks, limit, work)` using `errgroup.WithContext` + `SetLimit(limit)`, where each task runs `work` and a slot is bounded so at most `limit` run concurrently, releasing on every path including error.
- Test: assert observed max concurrency never exceeds `limit` via an atomic peak counter; assert a task error cancels the group and `Wait` returns it; a test that would deadlock if a slot were never released.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout/cmd/demo
cd ~/go-exercises/fanout
go mod init example.com/fanout
go mod edit -go=1.25
go get golang.org/x/sync/errgroup
```

### Bounded concurrency and the slot-leak deadlock

An unbounded fan-out — `for _, t := range tasks { go work(t) }` — starts one goroutine per task.
For a thousand tasks each opening a database connection, that is a thousand simultaneous
connections, which either exhausts the pool or the remote's limit, and the excess goroutines block
waiting for a connection that will not free up until the running ones finish. That is itself a
resource-exhaustion deadlock. The fix is to bound concurrency to K: at most K tasks run at once, the
rest wait for a slot.

`errgroup.Group.SetLimit(K)` implements exactly this. After `SetLimit(K)`, `g.Go(fn)` blocks until
fewer than K goroutines are active, then starts `fn`; when `fn` returns, its slot is freed. The slot
accounting is handled by errgroup itself, which is the safe part: because `g.Go` frees the slot when
the function returns *by any path* — normal return or a returned error — a task that fails does not
leak its slot. This is the crucial property. The anti-pattern that leaks is a hand-rolled semaphore
where you `acquire()` then `defer release()` — but forget the `defer` and put `release()` only on the
success path:

```go
// LEAKY — release only on success.
sem.Acquire(ctx, 1)
if err := work(t); err != nil {
	return err // slot never released -> pool starves -> eventual deadlock
}
sem.Release(1)
```

With `errgroup.SetLimit` you do not manage the slot yourself, so you cannot make that mistake — the
release is structural. `errgroup.WithContext` adds the second half: the first task to return a
non-nil error cancels the group's context, so the other tasks (which should be watching `ctx`) stop
early, and `g.Wait()` returns that first error. We pass that context into `work` so tasks are
cancellable and the batch fails fast instead of running to completion after the first error.

To make the bound observable and testable, `Process` tracks peak concurrency with an atomic counter:
each task increments a live counter on entry, records the max, and decrements on exit. The test
asserts that peak never exceeds `limit`.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
)

// Stats reports what happened during a Process run.
type Stats struct {
	// Peak is the maximum number of tasks that ran concurrently.
	Peak int
	// Completed is the number of tasks whose work returned nil.
	Completed int
}

// Process runs work over every task with at most limit running concurrently. It
// uses errgroup.SetLimit so a slot is freed whenever a task returns, by any path,
// which is what prevents a slot leak from starving the batch into a deadlock. The
// first task error cancels ctx for the rest and is returned by Process.
func Process[T any](ctx context.Context, tasks []T, limit int, work func(context.Context, T) error) (Stats, error) {
	if limit < 1 {
		limit = 1
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)

	var live, peak, completed atomic.Int64
	for _, task := range tasks {
		g.Go(func() error {
			n := live.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer live.Add(-1)

			if err := work(ctx, task); err != nil {
				return err
			}
			completed.Add(1)
			return nil
		})
	}
	err := g.Wait()
	return Stats{Peak: int(peak.Load()), Completed: int(completed.Load())}, err
}
```

### The runnable demo

The demo fans out 20 tasks with a limit of 4 and reports that peak concurrency stayed at or below 4.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/fanout"
)

func main() {
	tasks := make([]int, 20)
	for i := range tasks {
		tasks[i] = i
	}

	stats, err := fanout.Process(context.Background(), tasks, 4, func(ctx context.Context, t int) error {
		time.Sleep(5 * time.Millisecond) // simulate I/O
		return nil
	})
	if err != nil {
		fmt.Println("process:", err)
		return
	}
	fmt.Printf("completed=%d peak<=4: %v\n", stats.Completed, stats.Peak <= 4)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
completed=20 peak<=4: true
```

### Tests

`TestPeakBounded` runs many slow tasks with a limit and asserts the observed peak never exceeds it —
the concurrency bound holds. `TestErrorCancelsGroup` makes one task fail and asserts `Process`
returns that error and the remaining tasks observe cancellation (so `Completed` is less than the
total). `TestNoSlotLeak` runs a batch where half the tasks return errors and asserts the run still
*terminates* (a leaked slot would starve the pool and hang) — guarded by a watchdog. All under
`-race`.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"testing"
	"time"
)

func runWithWatchdog(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not complete within %s: starved pool / leaked slot", what, d)
	}
}

func TestPeakBounded(t *testing.T) {
	t.Parallel()

	tasks := make([]int, 100)
	const limit = 5
	stats, err := Process(t.Context(), tasks, limit, func(ctx context.Context, _ int) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("Process err = %v, want nil", err)
	}
	if stats.Peak > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", stats.Peak, limit)
	}
	if stats.Completed != len(tasks) {
		t.Fatalf("completed = %d, want %d", stats.Completed, len(tasks))
	}
}

func TestErrorCancelsGroup(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	tasks := make([]int, 50)
	for i := range tasks {
		tasks[i] = i
	}

	stats, err := Process(t.Context(), tasks, 4, func(ctx context.Context, id int) error {
		if id == 0 {
			return errBoom
		}
		// Others watch ctx and stop when the group is cancelled.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
			return nil
		}
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Process err = %v, want errBoom", err)
	}
	if stats.Completed == len(tasks) {
		t.Fatalf("all %d tasks completed; expected cancellation to stop some", len(tasks))
	}
}

func TestNoSlotLeak(t *testing.T) {
	t.Parallel()

	// Half the tasks fail. If a failed task leaked its slot, the pool would starve
	// and Process would hang; the watchdog turns that into a fast failure.
	tasks := make([]int, 200)
	for i := range tasks {
		tasks[i] = i
	}
	runWithWatchdog(t, 5*time.Second, "Process with failures", func() {
		_, _ = Process(context.Background(), tasks, 8, func(ctx context.Context, id int) error {
			if id%2 == 0 {
				return errors.New("fail")
			}
			return nil
		})
	})
}
```

## Review

The fan-out is correct when concurrency stays at or below the limit and the batch always terminates,
even when tasks fail. The bound is enforced by `SetLimit`, which `TestPeakBounded` verifies via the
atomic peak counter. Termination-under-failure is the deadlock-relevant property: because `errgroup`
frees a task's slot whenever the task function returns — success or error — no failing task can leak a
slot, so `TestNoSlotLeak` completes instead of starving. `errgroup.WithContext` gives fail-fast:
`TestErrorCancelsGroup` shows the first error cancels the rest and surfaces from `Wait`.

The mistake this exercise exists to prevent is a hand-rolled semaphore that releases only on the
success path — leak one slot per error and the pool deadlocks after `limit` failures. Whether you use
`errgroup.SetLimit` or a `semaphore.Weighted`, the rule is the same: the release must be structural
(`errgroup` does it for you, or `defer sem.Release(1)` immediately after acquire), never conditional
on the outcome. Run `-race` since the peak counter and the tasks share state.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `SetLimit`, and `Go`/`Wait`.
- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — `Weighted.Acquire`/`Release` for weighted bounds.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — the peak-concurrency counter's `Add`/`CompareAndSwap`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-deadlock-watchdog-test-harness.md](10-deadlock-watchdog-test-harness.md)
