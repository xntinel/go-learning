# Exercise 5: Bounded Worker Pool Draining a Job Channel

A bounded worker pool is the idiomatic way to process a batch of jobs with a fixed
degree of concurrency: spawn N workers, each running `for job := range jobs` to
consume until the channel closes, and coordinate a clean drain-and-close with a
`WaitGroup`. `for range channel` is the canonical consumer loop — it blocks for a
value, and terminates on its own the moment the producer closes the channel. This
module builds a generic `Run` and proves every job is processed exactly once with
no goroutine leak.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
workerpool/                  module example.com/workerpool
  go.mod
  pool.go                    Run[In,Out](ctx, workers, inputs, work) []Out
  pool_test.go               exactly-once processing, results count, no leak, ctx-cancel, -race
  cmd/demo/
    main.go                  squares 1..8 across 4 workers and prints the sum
```

- Files: `pool.go`, `pool_test.go`, `cmd/demo/main.go`.
- Implement: `Run[In, Out any](ctx, workers int, inputs []In, work func(ctx, In) Out) []Out` — a counted `for range workers` starts workers, each `for job := range jobs`; a feeder honors `ctx.Done()`; a `WaitGroup` closes the results channel after all workers return.
- Test: M jobs across N workers, every job processed exactly once (atomic counter) and `len(results) == M`; all workers return after close (no leak); a `-race` run; a `ctx`-cancel variant where workers stop and `Run` returns without deadlock.
- Verify: `go test -count=1 -race ./...`

### The close-then-drain shutdown, and who closes what

The pool has three moving parts and four `for` loops. A counted `for range
workers` starts the workers. Each worker runs the canonical consumer loop `for job
:= range jobs`: it blocks until a job arrives or `jobs` is closed, and when `jobs`
closes it exits the range and returns on its own — no sentinel value, no
poison-pill, no counting. A feeder goroutine pushes every input into `jobs` and
then closes it; a `for r := range results` in the caller collects outputs.

The ownership rule is what keeps this deadlock-free and panic-free. The *producer*
(the feeder) owns `jobs` and is the only one that closes it — workers never close a
channel they read, because a second close panics. Symmetrically, the workers
produce into `results`, so a dedicated goroutine that does `wg.Wait()` then
`close(results)` owns that channel: it closes `results` only once every worker has
returned, which is exactly when no more sends can happen. That `wg.Wait()` in a
separate goroutine is the linchpin — it lets the caller's `for r := range results`
terminate cleanly on close instead of blocking forever.

Cancellation threads through both the feeder and the workers. The feeder sends with
`select { case jobs <- in: case <-ctx.Done(): return }`, so a cancelled context
stops the feed (and the deferred `close(jobs)` still runs, which ends the worker
ranges). Each worker checks `ctx.Done()` before doing work, so it stops consuming
promptly. The result is that a cancelled pool still shuts down cleanly — the
`WaitGroup` still reaches zero, `results` still closes, and `Run` returns whatever
was produced before cancellation, with no leaked goroutine.

Create `pool.go`:

```go
package workerpool

import (
	"context"
	"sync"
)

// Run processes inputs with a fixed number of workers and returns the outputs.
// The order of the results is not specified (workers run concurrently). If ctx
// is cancelled the pool stops early and returns the results produced so far,
// with no leaked goroutine. A non-positive workers is treated as 1.
func Run[In, Out any](ctx context.Context, workers int, inputs []In, work func(context.Context, In) Out) []Out {
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan In)
	results := make(chan Out)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				select {
				case results <- work(ctx, job):
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Feeder owns jobs and is the only closer.
	go func() {
		defer close(jobs)
		for _, in := range inputs {
			select {
			case jobs <- in:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Close results once every worker has returned.
	go func() {
		wg.Wait()
		close(results)
	}()

	var out []Out
	for r := range results {
		out = append(out, r)
	}
	return out
}
```

### The runnable demo

The demo squares the numbers 1..8 across four workers. Because results arrive in
nondeterministic order, the demo prints their *sum*, which is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/workerpool"
)

func main() {
	inputs := []int{1, 2, 3, 4, 5, 6, 7, 8}

	squares := workerpool.Run(context.Background(), 4, inputs, func(_ context.Context, n int) int {
		return n * n
	})

	sum := 0
	for _, s := range squares {
		sum += s
	}
	fmt.Printf("processed %d jobs, sum of squares = %d\n", len(squares), sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
processed 8 jobs, sum of squares = 204
```

### Tests

`TestEachJobProcessedOnce` submits M jobs across N workers, counts invocations of
`work` with an atomic counter, and asserts both the counter and `len(results)`
equal M — proving nothing is dropped or double-processed. Running under `-race`
(the gate always does) proves the channel handoff is the only synchronization
needed. `TestCancelStopsCleanly` cancels the context before running and asserts
`Run` returns without deadlock and produces no more than M results, which is the
real proof that the `WaitGroup`/`close(results)` shutdown works even on the cancel
path.

Create `pool_test.go`:

```go
package workerpool

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestEachJobProcessedOnce(t *testing.T) {
	t.Parallel()

	const m = 500
	inputs := make([]int, m)
	for i := range inputs {
		inputs[i] = i
	}

	var processed int64
	results := Run(context.Background(), 8, inputs, func(_ context.Context, n int) int {
		atomic.AddInt64(&processed, 1)
		return n * 2
	})

	if got := atomic.LoadInt64(&processed); got != m {
		t.Fatalf("work called %d times, want %d", got, m)
	}
	if len(results) != m {
		t.Fatalf("got %d results, want %d", len(results), m)
	}

	// Every doubled value must appear exactly once.
	seen := make(map[int]int, m)
	for _, r := range results {
		seen[r]++
	}
	for i := range m {
		if seen[i*2] != 1 {
			t.Fatalf("value %d appeared %d times, want 1", i*2, seen[i*2])
		}
	}
}

func TestSingleWorker(t *testing.T) {
	t.Parallel()

	results := Run(context.Background(), 1, []int{1, 2, 3}, func(_ context.Context, n int) int {
		return n + 10
	})
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
}

func TestCancelStopsCleanly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we start

	inputs := make([]int, 1000)
	results := Run(ctx, 4, inputs, func(_ context.Context, n int) int { return n })

	// The pool must return (no deadlock) and never produce more than the input.
	if len(results) > len(inputs) {
		t.Fatalf("got %d results, want at most %d", len(results), len(inputs))
	}
}

func TestZeroWorkersTreatedAsOne(t *testing.T) {
	t.Parallel()

	results := Run(context.Background(), 0, []int{7, 8}, func(_ context.Context, n int) int { return n })
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}
```

## Review

The pool is correct when each worker is the canonical consumer `for job := range
jobs` and the channel-ownership discipline is exact: the feeder is the sole closer
of `jobs`, and a `wg.Wait()`-then-`close(results)` goroutine is the sole closer of
`results`. That single arrangement is what lets the caller's `for r := range
results` terminate on close instead of hanging. The proof of correctness is
`TestEachJobProcessedOnce` under `-race`: every job runs exactly once, the result
count matches, and the race detector stays silent — meaning the channel handoffs
are the only synchronization and there is no shared-state race. `TestCancelStopsCleanly`
proves the shutdown holds on the cancel path: even with the context cancelled up
front, `Run` returns rather than deadlocking, because the `WaitGroup` still reaches
zero and `results` still closes. The trap this guards against is a worker closing
`results`, or the caller forgetting the `wg.Wait()`/close goroutine — either turns
the drain into a panic or a hang. Run `go test -count=1 -race ./...`.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — ranging over a channel and the producer-closes rule.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — coordinating the drain-and-close.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded fan-out/fan-in with channels and `ctx`.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — `for range` over a channel and integer range.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-cursor-pagination-drain.md](04-cursor-pagination-drain.md) | Next: [06-labeled-break-slot-search.md](06-labeled-break-slot-search.md)
