# Exercise 3: Shut Down a Fan-Out Worker Pool With One Closed Channel

A worker pool dispatches jobs from one queue to N goroutines running in parallel. The
production requirement is that closing a single shared `done` channel stops all N of them
at once — the broadcast property of `close` — and that the results channel is closed only
after every worker has drained and returned, so a consumer ranging over results terminates
cleanly instead of racing a live send. This is the exact machinery behind a
thumbnail-generation pool or a webhook-delivery pool that must stop cleanly on SIGTERM.

## What you'll build

```text
fanoutpool/                        independent module: example.com/fanoutpool
  go.mod
  pool.go                          Job, Result; RunPool(done, jobs, results, n) with WaitGroup drain
  cmd/
    demo/
      main.go                      runnable demo: dispatch jobs to a pool, collect results
  pool_test.go                     all-jobs, stop-all-on-done, results-closed-after-drain; -race
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: `RunPool(done <-chan struct{}, jobs <-chan Job, results chan<- Result, n int)` that launches `n` workers each selecting on `{jobs, done}`, and closes `results` only after a `sync.WaitGroup` confirms all workers returned.
Test: M jobs across N workers produce M results (order-independent); closing `done` stops every worker (proven by `results` closing); ranging over `results` terminates.
Verify: `go test -count=1 -race ./...`

### One close broadcasts to N workers

Every worker runs the same loop: `select { case <-done: return; case job, ok := <-jobs: ... }`.
All N workers share the *same* `done` channel. When the coordinator calls `close(done)`,
the closed channel becomes permanently selectable in every worker simultaneously — that is
the broadcast property. A send-based signal (`done <- struct{}{}`) would wake exactly one
worker and leave N-1 running; only `close` reaches all of them. This is why the pattern
scales from one worker to a pool without changing the signaling mechanism.

### The drain order is the subtle part

Workers write to `results`. Multiple writers means no single worker may close `results` —
whoever closed it could race another worker's in-flight send and panic with "send on closed
channel". The correct construction: a `sync.WaitGroup` counts the workers, each worker calls
`wg.Done()` on exit, and a separate closer goroutine does `wg.Wait(); close(results)`. The
results channel is closed exactly once, and only after the last writer has provably returned.
The consumer ranging over `results` then sees a clean close.

Two exit conditions matter for each worker: the jobs channel being closed (normal drain — no
more work) and `done` being closed (cancellation). And the worker's *send* to `results` is
itself guarded with a select on `done`, so if the consumer walks away while a worker holds a
finished result, the worker does not block forever on the send — it observes `done` and
exits. Without that inner guard, a cancelled pool whose consumer stopped reading would leak
every worker that had a result in hand.

Create `pool.go`:

```go
package fanoutpool

import "sync"

// Job is a unit of work dispatched to the pool.
type Job struct {
	ID int
	N  int
}

// Result is the outcome of processing a Job.
type Result struct {
	ID     int
	Square int
}

// process is the pool's per-job work. Real pools would resize an image or
// deliver a webhook here; this squares N so results are checkable.
func process(j Job) Result {
	return Result{ID: j.ID, Square: j.N * j.N}
}

// RunPool launches n workers that pull from jobs and push to results. Closing
// done stops every worker; closing jobs drains them normally. results is closed
// exactly once, after every worker has returned, so a consumer can range over it.
// RunPool returns immediately; the pool runs in the background.
func RunPool(done <-chan struct{}, jobs <-chan Job, results chan<- Result, n int) {
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					r := process(job)
					select {
					case results <- r:
					case <-done:
						return
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/fanoutpool"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	jobs := make(chan fanoutpool.Job)
	results := make(chan fanoutpool.Result)

	fanoutpool.RunPool(done, jobs, results, 4)

	go func() {
		for i := 1; i <= 5; i++ {
			jobs <- fanoutpool.Job{ID: i, N: i}
		}
		close(jobs)
	}()

	var squares []int
	for r := range results {
		squares = append(squares, r.Square)
	}
	sort.Ints(squares)
	fmt.Println("squares:", squares)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
squares: [1 4 9 16 25]
```

### Tests

`TestPoolProcessesAllJobs` dispatches M jobs across N workers and collects results into a map
keyed by job ID, because the completion order across parallel workers is nondeterministic —
you assert the *set* of results, never their order. `TestPoolStopsAllWorkersOnDone` never
closes `jobs`; instead it closes `done` and then proves every worker returned by observing
that `results` closes (the closer goroutine only closes it after `wg.Wait()` unblocks, which
happens only when all N workers have called `wg.Done()`). `TestResultsChannelClosedAfterDrain`
confirms the normal-path close: after all jobs are processed, ranging over `results`
terminates.

Create `pool_test.go`:

```go
package fanoutpool

import "testing"

func TestPoolProcessesAllJobs(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	jobs := make(chan Job)
	results := make(chan Result)

	const m = 20
	RunPool(done, jobs, results, 4)

	go func() {
		for i := 1; i <= m; i++ {
			jobs <- Job{ID: i, N: i}
		}
		close(jobs)
	}()

	got := make(map[int]int)
	for r := range results {
		got[r.ID] = r.Square
	}
	if len(got) != m {
		t.Fatalf("got %d results, want %d", len(got), m)
	}
	for i := 1; i <= m; i++ {
		if got[i] != i*i {
			t.Fatalf("result for job %d = %d, want %d", i, got[i], i*i)
		}
	}
}

func TestPoolStopsAllWorkersOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	jobs := make(chan Job) // never closed: workers can only exit via done
	results := make(chan Result)

	RunPool(done, jobs, results, 8)

	close(done)

	// results is closed only after all 8 workers return. If any worker ignored
	// done, wg.Wait never unblocks and this range hangs (test times out).
	for range results {
		t.Fatal("no jobs were dispatched; results must be empty")
	}
}

func TestResultsChannelClosedAfterDrain(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	jobs := make(chan Job)
	results := make(chan Result)

	RunPool(done, jobs, results, 3)

	go func() {
		jobs <- Job{ID: 1, N: 3}
		close(jobs)
	}()

	count := 0
	for range results { // terminates iff results is closed after the drain
		count++
	}
	if count != 1 {
		t.Fatalf("drained %d results, want 1", count)
	}
}
```

## Review

The pool is correct when one `close(done)` stops every worker and when `results` is closed
exactly once, only after the last worker returns. The two properties are tested independently:
the all-jobs test proves throughput and correctness with an order-independent set assertion,
and the stop-all-on-done test proves the broadcast and the WaitGroup-gated close together —
if a worker ignored `done`, `results` would never close and the ranging loop would hang. The
inner `select` on the send to `results` is what keeps a worker from leaking when the consumer
abandons the pool mid-shutdown. Run `go test -race` to catch any results-close-versus-send
race; the WaitGroup ordering is precisely what makes it race-free. The classic error here is
closing `results` from inside a worker — with N writers that panics; the closer goroutine
after `wg.Wait()` is the only safe closer.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines)
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-spawn-result-channel.md](02-spawn-result-channel.md) | Next: [04-merge-fan-in-cancel.md](04-merge-fan-in-cancel.md)
