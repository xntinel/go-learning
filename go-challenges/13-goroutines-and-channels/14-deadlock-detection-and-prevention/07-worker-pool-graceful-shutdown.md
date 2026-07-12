# Exercise 7: Worker Pool Shutdown Without Send-on-Closed or Hung Waiter

Graceful shutdown of a worker pool is where channel-close and `WaitGroup` mistakes concentrate:
close the jobs channel from the wrong place and you panic with send-on-closed; leave the results
channel with no reader and the workers wedge while `Wait` never returns. This exercise builds a
bounded pool whose producer closes the jobs channel exactly once, whose workers drain and exit,
and whose results always have somewhere to go.

This module is fully self-contained: its own `go mod init`, all types inline, its own demo and
tests.

## What you'll build

```text
pool/                      independent module: example.com/pool
  go.mod                   go 1.25
  pool.go                  Pool; Run submits jobs, closes once, collects all results
  cmd/
    demo/
      main.go              run N jobs, collect N results, clean shutdown
  pool_test.go             all-results-collected + cancel-mid-run leak-free (-race -count)
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Pool` of K workers where the producer closes the jobs channel exactly once, workers drain remaining jobs then exit, results go to a buffered channel drained concurrently, and `Run` blocks on a `WaitGroup` with no chance of a hung waiter.
- Test: submit N jobs and assert all N results are collected and every worker returns within a watchdog window; a test that cancels context mid-run and asserts prompt, leak-free shutdown; run under `-race` with high `-count`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The three shutdown hazards

A worker pool has one producer feeding a jobs channel, K workers reading it and writing results,
and a collector reading results. Shutdown wedges in three classic ways:

1. **Send on closed / double close.** If a worker (a consumer) closes the jobs channel, or two
   producers both close it, the program panics: `send on closed channel` or
   `close of closed channel`. The rule is one closer — the sole producer — closing exactly once.
   When ownership is genuinely shared, `sync.Once` enforces the "exactly once."
2. **Hung waiter from unread results.** If workers write into a results channel that nobody is
   reading (the collector exited, or `Run` is blocked in `Wait` before starting a collector),
   the workers block on the send, never call `wg.Done()`, and `Wait` blocks forever. Results must
   always have somewhere to go: either a buffer large enough, or a drain goroutine reading
   concurrently while the workers run.
3. **Ordering.** `Wait` must run only after the results have a consumer, and the jobs channel must
   be closed only after the producer is done. The canonical order: start workers, start a collector
   goroutine, feed jobs, close jobs, `wg.Wait()` for workers, then close results so the collector
   finishes.

This design uses a **drain goroutine**: `Run` starts the collector reading `results` before it
feeds any jobs, so workers never block on the results send. Workers `range` over `jobs`, so they
exit automatically when the producer closes it. A `context` lets a caller cancel mid-run: workers
check `ctx.Done()` in their select and stop pulling new jobs, and the producer stops feeding — but
critically, the channels are still closed in the correct order so nothing wedges on the way out.

Create `pool.go`:

```go
package pool

import (
	"context"
	"sync"
)

// Job is a unit of work; Result pairs a job id with its computed output.
type Job struct {
	ID    int
	Value int
}

// Result is the output of processing a Job.
type Result struct {
	ID     int
	Output int
}

// Pool runs jobs across a fixed number of workers with a clean shutdown: the
// jobs channel is closed exactly once by the producer, workers drain then exit,
// and results are drained concurrently so no worker wedges on a full channel.
type Pool struct {
	workers int
	process func(Job) Result
}

// New returns a pool of the given size that maps each Job to a Result via
// process.
func New(workers int, process func(Job) Result) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{workers: workers, process: process}
}

// Run processes every job in jobs and returns the results. It honors ctx: if ctx
// is cancelled, workers stop pulling new jobs and Run returns what was completed.
// Ordering is: start workers -> start collector -> feed jobs -> close jobs ->
// wait for workers -> close results -> collect. No path leaves a goroutine
// blocked or the jobs channel double-closed.
func (p *Pool) Run(ctx context.Context, jobs []Job) []Result {
	jobCh := make(chan Job)
	// Buffer results to the job count so a worker never blocks on the send even
	// before the collector reads; the collector drains it fully regardless.
	resultCh := make(chan Result, len(jobs))

	var wg sync.WaitGroup
	for range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobCh:
					if !ok {
						return // jobs channel closed and drained
					}
					select {
					case resultCh <- p.process(job):
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	// Producer: the sole closer of jobCh, closing exactly once.
	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for workers, then close results so the collector range terminates.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collector: drains results until closed. Because resultCh is buffered to
	// len(jobs) and closed only after all workers exit, this always terminates.
	var out []Result
	for r := range resultCh {
		out = append(out, r)
	}
	return out
}
```

### The runnable demo

The demo runs eight jobs across three workers, collects all eight results, and exits cleanly with
no leaked goroutine.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	"example.com/pool"
)

func main() {
	p := pool.New(3, func(j pool.Job) pool.Result {
		return pool.Result{ID: j.ID, Output: j.Value * j.Value}
	})

	jobs := make([]pool.Job, 8)
	for i := range jobs {
		jobs[i] = pool.Job{ID: i, Value: i}
	}

	results := p.Run(context.Background(), jobs)
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })

	fmt.Printf("collected %d results\n", len(results))
	for _, r := range results {
		fmt.Printf("job %d -> %d\n", r.ID, r.Output)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
collected 8 results
job 0 -> 0
job 1 -> 1
job 2 -> 4
job 3 -> 9
job 4 -> 16
job 5 -> 25
job 6 -> 36
job 7 -> 49
```

### Tests

`TestRunCollectsAll` submits N jobs and asserts exactly N results come back and every value is
correct — proof that no worker wedged and the collector terminated. It runs under a watchdog so a
shutdown-ordering bug (a hung waiter) fails with a diagnostic instead of hanging. `TestCancelMidRun`
cancels the context and asserts `Run` returns promptly with no leaked goroutine (checked by
comparing `runtime.NumGoroutine` before and after, allowing the scheduler a moment to settle). Run
with high `-count` to shake out ordering races.

Create `pool_test.go`:

```go
package pool

import (
	"context"
	"runtime"
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
		t.Fatalf("%s did not complete within %s: hung waiter or wedged worker", what, d)
	}
}

func TestRunCollectsAll(t *testing.T) {
	t.Parallel()

	const n = 200
	p := New(4, func(j Job) Result {
		return Result{ID: j.ID, Output: j.Value + 1}
	})
	jobs := make([]Job, n)
	for i := range jobs {
		jobs[i] = Job{ID: i, Value: i}
	}

	var results []Result
	runWithWatchdog(t, 5*time.Second, "Run", func() {
		results = p.Run(t.Context(), jobs)
	})

	if len(results) != n {
		t.Fatalf("collected %d results, want %d (wedged worker?)", len(results), n)
	}
	seen := make(map[int]int, n)
	for _, r := range results {
		seen[r.ID] = r.Output
	}
	for i := range n {
		if seen[i] != i+1 {
			t.Fatalf("job %d output = %d, want %d", i, seen[i], i+1)
		}
	}
}

func TestCancelMidRun(t *testing.T) {
	t.Parallel()

	before := runtime.NumGoroutine()

	p := New(4, func(j Job) Result {
		time.Sleep(time.Millisecond) // slow enough that cancel lands mid-run
		return Result{ID: j.ID, Output: j.Value}
	})
	jobs := make([]Job, 1000)
	for i := range jobs {
		jobs[i] = Job{ID: i, Value: i}
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()

	runWithWatchdog(t, 5*time.Second, "cancelled Run", func() {
		_ = p.Run(ctx, jobs) // returns partial results; must not hang or leak
	})

	// Give the runtime a moment to reap the pool's goroutines, then assert no leak.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - before; leaked > 0 {
		t.Fatalf("leaked %d goroutines after cancelled Run", leaked)
	}
}
```

## Review

The pool is correct when it collects exactly one result per job and every goroutine exits after
`Run` returns. The ordering is what guarantees it: the producer is the sole closer of `jobCh`, so
no send-on-closed panic; the collector drains `resultCh` concurrently and it is buffered to the job
count, so a worker never blocks on the send; and `resultCh` is closed only after `wg.Wait()`, so
the collector's `range` always terminates. `TestRunCollectsAll` under the watchdog is the proof that
no path wedges; `TestCancelMidRun` proves cancellation is prompt and leak-free.

The mistakes to avoid: closing `jobCh` from a worker (panic), closing it twice, and — the subtle
one — writing results into a channel with no reader during shutdown, which hangs `Wait`. The
`runtime.NumGoroutine` comparison in the cancel test is a lightweight goleak check; in a larger
codebase reach for `go.uber.org/goleak` for a rigorous version. Run with `-race -count=10` so an
ordering bug that passes once fails on a later iteration.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — close semantics and the sole-closer discipline.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the fan-in barrier and its Add/Done/Wait contract.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — rigorous goroutine-leak detection for shutdown tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-channel-request-response-rpc.md](08-channel-request-response-rpc.md)
