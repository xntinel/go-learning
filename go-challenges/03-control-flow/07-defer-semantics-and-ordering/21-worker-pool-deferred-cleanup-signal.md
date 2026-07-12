# Exercise 21: Worker Pool Shutdown — Deferred Goroutine Cleanup and WaitGroup

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A worker pool that closes its job channel and returns from `Shutdown`
without waiting for every worker to actually finish hands its caller a false
guarantee: code that runs immediately after `Shutdown()` and expects all
worker-owned resources released can now race against a worker still mid
cleanup. This module builds a pool where each worker's own cleanup is
proven to complete before it is counted as done, and where `Shutdown` itself
defers the `sync.WaitGroup.Wait()` call so the guarantee holds on every exit
path out of `Shutdown`, not just the one line visible today. The module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
workerpool/                 independent module: example.com/worker-pool-deferred-cleanup-signal
  go.mod                     go 1.24
  workerpool.go              Pool (NewPool, Submit, Shutdown, Results, CleanupLog)
  cmd/
    demo/
      main.go                runnable demo: 3 workers process 9 jobs, print results and cleanup count
  workerpool_test.go         table over worker/job counts; concurrent-submitters case under -race
```

- Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
- Implement: `Pool` with `NewPool(workers int, work func(int) int) *Pool`, `Submit(job int)`, `Shutdown()`, `Results() []int`, `CleanupLog() []string`.
- Test: a table over worker/job count combinations, plus a concurrent-submitters case run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two LIFO guarantees stacked on top of each other

There are two separate defer-ordering guarantees at work here, one inside
each worker and one inside `Shutdown`. Inside `runWorker`, `defer
p.wg.Done()` is registered first and `defer p.recordCleanup(id)` second;
LIFO means `recordCleanup` runs first, at return, and `wg.Done()` runs
after it. That ordering is the whole point: a worker is never counted as
"done" by any goroutine blocked on `wg.Wait()` until its own cleanup record
has already been filed, so there is no window where the wait group thinks a
worker is gone but its cleanup has not actually happened yet. Inside
`Shutdown`, `defer p.wg.Wait()` is registered before `close(p.jobs)` even
runs, so it fires after everything else in the function body — today that
body is a single line, but writing the wait as a defer means any later
addition above it (an idempotency check that returns early on a
double-Shutdown, a panic from a misbehaving job) keeps the exact same
guarantee: `Shutdown` never returns while a worker might still be mid
cleanup.

Create `workerpool.go`:

```go
package workerpool

import (
	"fmt"
	"sync"
)

// Pool runs a fixed number of worker goroutines pulling jobs off a shared
// channel until Shutdown closes it.
type Pool struct {
	jobs chan int
	wg   sync.WaitGroup

	mu         sync.Mutex
	results    []int
	cleanupLog []string
}

// NewPool starts workers goroutines, each running work over jobs pulled
// from the pool's internal channel.
func NewPool(workers int, work func(job int) int) *Pool {
	p := &Pool{jobs: make(chan int)}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.runWorker(i, work)
	}
	return p
}

// runWorker drains jobs until the channel is closed. Two defers registered
// here, in this order, run in reverse at return: recordCleanup (registered
// second) fires first, filing this worker's own cleanup record; wg.Done
// (registered first) fires last, telling any Shutdown call blocked on
// wg.Wait that one more worker is gone. That order guarantees a worker
// never gets counted as "done" before its own cleanup has actually run --
// which is exactly the property Shutdown depends on.
func (p *Pool) runWorker(id int, work func(job int) int) {
	defer p.wg.Done()
	defer p.recordCleanup(id)

	for job := range p.jobs {
		result := work(job)
		p.mu.Lock()
		p.results = append(p.results, result)
		p.mu.Unlock()
	}
}

// recordCleanup simulates a worker releasing its own local resources (a
// scratch buffer, a per-worker connection) on the way out.
func (p *Pool) recordCleanup(id int) {
	p.mu.Lock()
	p.cleanupLog = append(p.cleanupLog, fmt.Sprintf("worker-%d-cleaned", id))
	p.mu.Unlock()
}

// Submit pushes one job into the pool. It blocks if every worker is busy.
func (p *Pool) Submit(job int) { p.jobs <- job }

// Shutdown closes the jobs channel, so every worker's range loop ends, and
// waits for all of them to finish. The wait is written as a defer -- even
// though the body above it is a single close(p.jobs) today -- so that any
// step added later above it (an early return for a pool that is already
// shutting down, a panic from a misbehaving job) keeps the same guarantee:
// Shutdown never returns while a worker might still be mid-cleanup.
func (p *Pool) Shutdown() {
	defer p.wg.Wait()
	close(p.jobs)
}

// Results returns a copy of every result produced so far. Safe to call
// after Shutdown has returned.
func (p *Pool) Results() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, len(p.results))
	copy(out, p.results)
	return out
}

// CleanupLog returns a copy of the recorded per-worker cleanup entries.
func (p *Pool) CleanupLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.cleanupLog))
	copy(out, p.cleanupLog)
	return out
}
```

### The runnable demo

The demo runs 9 jobs through 3 workers, then prints the result count, sum,
and how many per-worker cleanup entries were recorded by the time `Shutdown`
returned.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/worker-pool-deferred-cleanup-signal"
)

func main() {
	pool := workerpool.NewPool(3, func(job int) int { return job * job })

	for i := 1; i <= 9; i++ {
		pool.Submit(i)
	}
	pool.Shutdown()

	sum := 0
	for _, r := range pool.Results() {
		sum += r
	}
	fmt.Printf("results=%d sum=%d\n", len(pool.Results()), sum)
	fmt.Printf("cleanup entries=%d\n", len(pool.CleanupLog()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
results=9 sum=285
cleanup entries=3
```

### Tests

`TestShutdownWaitsForAllWorkerCleanup` is a table over worker/job count
combinations, including more workers than jobs and vice versa, asserting
the result count matches the job count and the cleanup log has exactly one
entry per worker every time. `TestShutdownConcurrentSubmitAndShutdown` races
ten concurrent submitter goroutines against a six-worker pool to prove the
shared `results` and `cleanupLog` slices — guarded by the pool's mutex — see
no data race, and that `Shutdown`'s deferred `wg.Wait()` still waits out
every worker regardless of how job delivery interleaves. Run with `-race` to
actually exercise that guarantee.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"sync"
	"testing"
)

func TestShutdownWaitsForAllWorkerCleanup(t *testing.T) {
	tests := []struct {
		name    string
		workers int
		jobs    int
	}{
		{"one worker, few jobs", 1, 5},
		{"several workers, more jobs than workers", 4, 20},
		{"more workers than jobs", 8, 3},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pool := NewPool(tc.workers, func(job int) int { return job * 2 })

			for i := 0; i < tc.jobs; i++ {
				pool.Submit(i)
			}
			pool.Shutdown()

			if got := len(pool.Results()); got != tc.jobs {
				t.Fatalf("results = %d, want %d", got, tc.jobs)
			}
			if got := len(pool.CleanupLog()); got != tc.workers {
				t.Fatalf("cleanup entries = %d, want %d (one per worker)", got, tc.workers)
			}
		})
	}
}

// TestShutdownConcurrentSubmitAndShutdown races many submitters against a
// pool under -race to confirm the shared results and cleanup-log slices,
// guarded by the pool's mutex, never see a data race, and that Shutdown's
// deferred wg.Wait still waits out every worker regardless of how job
// delivery is interleaved.
func TestShutdownConcurrentSubmitAndShutdown(t *testing.T) {
	const workers = 6
	const submitters = 10
	const jobsPerSubmitter = 50

	pool := NewPool(workers, func(job int) int { return job + 1 })

	var submitWG sync.WaitGroup
	for s := 0; s < submitters; s++ {
		submitWG.Add(1)
		go func(base int) {
			defer submitWG.Done()
			for i := 0; i < jobsPerSubmitter; i++ {
				pool.Submit(base*jobsPerSubmitter + i)
			}
		}(s)
	}
	submitWG.Wait()
	pool.Shutdown()

	want := submitters * jobsPerSubmitter
	if got := len(pool.Results()); got != want {
		t.Fatalf("results = %d, want %d", got, want)
	}
	if got := len(pool.CleanupLog()); got != workers {
		t.Fatalf("cleanup entries = %d, want %d (one per worker)", got, workers)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The pool is correct when `Shutdown()` returning is a real guarantee that
every worker has both stopped and finished its own cleanup — not merely
that the job channel is closed. Two ordering decisions produce that
guarantee together: LIFO inside each worker means its cleanup record is
always filed before `wg.Done()` fires, and LIFO inside `Shutdown` means
`wg.Wait()` runs after everything else in the function, on every exit path.
The mistake this avoids is calling `wg.Done()` before running per-worker
cleanup — which would let a concurrent `Shutdown()` return while that
cleanup is still in flight — or calling `wg.Wait()` as a plain statement
positioned before some later-added early return, which would silently stop
protecting the exit paths added after it.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — `Add`/`Done`/`Wait` and the happens-before guarantee `Wait` provides.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions execute in LIFO order at return.
- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the close-channel-to-signal-done shape this pool's shutdown builds on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-read-replica-failover-deferred-cleanup.md](20-read-replica-failover-deferred-cleanup.md) | Next: [22-panic-recovery-deferred-cleanup.md](22-panic-recovery-deferred-cleanup.md)
