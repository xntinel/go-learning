# Exercise 4: Fan-in worker that drains remaining jobs before stopping

Graceful shutdown of a queue consumer is not just "stop looping." A worker that
abandons the jobs already buffered in its channel drops in-flight work on every
deploy. The correct behavior is: on cancellation, drain what is already buffered
with a non-blocking receive, process each of those, and only then leave the loop —
via a labeled `break`, because the drain runs inside a nested `select`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
drain/                     independent module: example.com/drain
  go.mod                   go 1.24
  drain.go                 Job, Worker; Run(ctx, jobs) drains buffered jobs on cancel
  cmd/
    demo/
      main.go              runnable demo: buffer jobs, cancel, watch them drain
  drain_test.go            drains on cancel; stops on close; -race with a producer
```

- Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
- Implement: `Worker.Run(ctx, <-chan Job)` that processes jobs in a `for`-`select`; on `ctx.Done()` it drains the already-buffered jobs with a non-blocking receive (`select` with `default`) and then leaves via a labeled `break`.
- Test: buffer M jobs, cancel the context, assert all M are processed exactly once and the worker returns; stop cleanly on a closed channel; run under `-race` with a real producer goroutine.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the drain needs a non-blocking receive and a labeled break

When `ctx.Done()` fires, jobs may still sit buffered in the channel — they were
accepted before shutdown and represent work the system already promised to do.
Simply returning would drop them. So the `ctx.Done()` branch enters a small inner
loop that pulls jobs with a *non-blocking* receive: `select { case j := <-jobs:
process; default: done }`. The `default` case fires the instant the buffer is
empty, so the drain never blocks waiting for a job that will never come. That is
the difference between draining what is present and hanging forever.

Leaving the loop is where the label earns its place. The drain runs inside a
`select`, which is inside another `select`, which is inside the `for`. A bare
`break` anywhere in there leaves only the innermost `select`. The only statement
that leaves the whole worker loop is `break loop`, naming the outer `for`. It is
reached from two places: the inner `default` when the buffer is empty, and the
closed-channel check (`ok == false`) in both the outer and inner receives.

Create `drain.go`:

```go
package drain

import "context"

// Job is a unit of work identified by ID.
type Job struct {
	ID int
}

// Worker consumes jobs until its context is cancelled, then drains any jobs
// already buffered before returning.
type Worker struct {
	done []int
}

// Run processes jobs from the channel. On ctx cancellation it drains the jobs
// already buffered (non-blocking) and then stops. It also stops cleanly if the
// channel is closed. The labeled break is the only statement that leaves the
// worker loop; every bare break inside would leave just a select.
func (w *Worker) Run(ctx context.Context, jobs <-chan Job) {
loop:
	for {
		select {
		case <-ctx.Done():
			// Drain whatever is already buffered, then stop.
			for {
				select {
				case j, ok := <-jobs:
					if !ok {
						break loop
					}
					w.process(j)
				default:
					break loop
				}
			}
		case j, ok := <-jobs:
			if !ok {
				break loop
			}
			w.process(j)
		}
	}
}

func (w *Worker) process(j Job) {
	w.done = append(w.done, j.ID)
}

// Done returns the IDs of the jobs processed, in processing order.
func (w *Worker) Done() []int {
	return w.done
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"slices"

	"example.com/drain"
)

func main() {
	jobs := make(chan drain.Job, 5)
	for i := range 5 {
		jobs <- drain.Job{ID: i}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown requested with 5 jobs still buffered

	var w drain.Worker
	w.Run(ctx, jobs)

	got := slices.Clone(w.Done())
	slices.Sort(got)
	fmt.Printf("drained %d buffered jobs: %v\n", len(got), got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained 5 buffered jobs: [0 1 2 3 4]
```

Even though the context was cancelled before `Run` started, all five buffered
jobs are processed before the worker stops.

### Tests

`TestDrainsBufferedJobsOnCancel` buffers M jobs, cancels the context, and asserts
every job is processed exactly once (as a set — the interleaving between the main
receive and the drain receive is not fixed, but the set is). `TestStopsOnClose`
buffers jobs, closes the channel, and asserts a clean stop with all jobs
processed and no send-on-closed panic. `TestConcurrentProducer` feeds jobs from a
separate goroutine to exercise the channel under `-race`.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"slices"
	"testing"
)

func TestDrainsBufferedJobsOnCancel(t *testing.T) {
	t.Parallel()

	const m = 8
	jobs := make(chan Job, m)
	for i := range m {
		jobs <- Job{ID: i}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel with all m jobs buffered

	var w Worker
	w.Run(ctx, jobs)

	got := slices.Clone(w.Done())
	slices.Sort(got)
	want := make([]int, m)
	for i := range m {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("processed = %v, want %v (all buffered jobs must drain exactly once)", got, want)
	}
}

func TestStopsOnClose(t *testing.T) {
	t.Parallel()

	const m = 5
	jobs := make(chan Job, m)
	for i := range m {
		jobs <- Job{ID: i}
	}
	close(jobs)

	var w Worker
	w.Run(context.Background(), jobs)

	if len(w.Done()) != m {
		t.Fatalf("processed %d jobs, want %d", len(w.Done()), m)
	}
}

func TestConcurrentProducer(t *testing.T) {
	t.Parallel()

	const m = 100
	jobs := make(chan Job)
	go func() {
		for i := range m {
			jobs <- Job{ID: i}
		}
		close(jobs)
	}()

	var w Worker
	w.Run(context.Background(), jobs)

	if len(w.Done()) != m {
		t.Fatalf("processed %d jobs, want %d", len(w.Done()), m)
	}
}
```

## Review

The worker is correct when a cancellation drains every already-buffered job
exactly once, a closed channel stops it cleanly, and neither path panics or double-
processes. The interleaving between the outer receive and the drain receive is not
deterministic, so tests assert on the *set* of processed jobs, not the order —
asserting a fixed order would flake. The mistakes to avoid: returning on
`ctx.Done()` without draining (drops in-flight work), and a blocking receive in
the drain instead of `select`-with-`default` (hangs when the buffer empties). Run
`go test -race`; `TestConcurrentProducer` drives the channel from a second
goroutine, which is what catches a mishandled receive.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the `default` case makes a receive non-blocking.
- [context package](https://pkg.go.dev/context) — cancellation via `Done`.
- [Go by Example: Non-Blocking Channel Operations](https://gobyexample.com/non-blocking-channel-operations) — the `select`-with-`default` drain idiom.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-broker-reconnect-backoff-loop.md](03-broker-reconnect-backoff-loop.md) | Next: [05-batch-import-labeled-continue.md](05-batch-import-labeled-continue.md)
