# Exercise 1: Bounded Worker Pool from Goroutine Function Literals

A worker pool is the first place a backend engineer meets anonymous functions at
scale: one goroutine per job, launched from a function literal, with concurrency
bounded so a burst of work does not spawn tens of thousands of goroutines and
exhaust memory or downstream connections. This module builds that pool and proves,
under the race detector, that it runs every job exactly once and never exceeds its
worker limit.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
workerpool/                        module example.com/workerpool
  go.mod
  internal/pool/pool.go            type Pool; New, Run, RunWithErrors; Job, Result
  internal/pool/pool_test.go       runs-once, worker-limit, error, join, index, serial
  cmd/workerpool/main.go           runnable demo over N workers
```

- Files: `internal/pool/pool.go`, `internal/pool/pool_test.go`, `cmd/workerpool/main.go`.
- Implement: `Pool.Run` launching one goroutine per job via a function literal, a buffered-channel semaphore bounding concurrency to N, `sync.WaitGroup` join, per-index result writes, and `RunWithErrors` aggregating with `errors.Join`.
- Test: runs-each-job-once, respects-worker-limit (atomic peak tracking), reports-each-error, `RunWithErrors`-joins, serializes-results-to-index, single-worker-is-serial. Under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/workerpool/internal/pool ~/go-exercises/workerpool/cmd/workerpool
cd ~/go-exercises/workerpool
go mod init example.com/workerpool
```

### How the pool uses anonymous functions

`Run` launches one goroutine per job. The goroutine body is a function literal, and
two decisions inside it are the whole lesson. First, the loop index and the job are
passed as *arguments* — `go func(idx int, j Job) { ... }(i, job)` — not captured.
Passing them evaluates `i` and `job` at the call site so each goroutine gets its
own copy; the goroutine then writes only to `results[idx]`, its own slot, which is
what makes the concurrent writes race-free. Go 1.22 fixed per-iteration loop
variables so a bare capture of `i` would also be correct now, but passing the
argument states the intent explicitly and works for any derived value, not just a
loop variable.

Second, the goroutine is bounded by a buffered-channel semaphore. `sem` has
capacity `p.workers`; sending into it before launching blocks once `p.workers`
sends are outstanding, so at most `p.workers` goroutines are past the send at any
moment. Each goroutine returns its token with `defer func() { <-sem }()`. The
`sync.WaitGroup` is separate bookkeeping: `wg.Add(1)` runs *before* the `go`
statement so `Wait` cannot observe a zero counter early, and `defer wg.Done()` is
the *first* deferred statement so the counter is decremented even if the job
panics.

Create `internal/pool/pool.go`:

```go
package pool

import (
	"errors"
	"sync"
)

// Job is a unit of work that reports success or failure.
type Job func() error

// Result pairs a job's original index with its error.
type Result struct {
	Index int
	Err   error
}

// Pool runs jobs concurrently, bounded to a fixed number of workers.
type Pool struct {
	workers int
}

// New returns a Pool bounded to workers concurrent goroutines (at least one).
func New(workers int) *Pool {
	if workers <= 0 {
		workers = 1
	}
	return &Pool{workers: workers}
}

// Run executes every job, at most p.workers at a time, and returns one Result
// per job in the original order. Each goroutine writes only its own index.
func (p *Pool) Run(jobs []Job) []Result {
	results := make([]Result, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, p.workers)

	for i, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, j Job) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = Result{Index: idx, Err: j()}
		}(i, job)
	}
	wg.Wait()
	return results
}

// RunWithErrors executes every job and joins all non-nil errors into one.
func (p *Pool) RunWithErrors(jobs []Job) error {
	results := p.Run(jobs)
	var errs []error
	for _, r := range results {
		if r.Err != nil {
			errs = append(errs, r.Err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo submits twenty jobs across N workers (from the command line, default 4).
It sets the header once with a fixed job set so the final line is deterministic; the
per-job lines print in scheduling order, which varies.

Create `cmd/workerpool/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strconv"

	"example.com/workerpool/internal/pool"
)

func main() {
	workers := 4
	if len(os.Args) > 1 {
		if v, err := strconv.Atoi(os.Args[1]); err == nil {
			workers = v
		}
	}

	jobs := make([]pool.Job, 20)
	for i := range jobs {
		jobs[i] = func() error {
			return nil
		}
	}

	err := pool.New(workers).RunWithErrors(jobs)
	fmt.Printf("processed %d jobs with %d workers, err=%v\n", len(jobs), workers, err)
}
```

Run it:

```bash
go run ./cmd/workerpool 4
```

Expected output:

```
processed 20 jobs with 4 workers, err=<nil>
```

### Tests

`TestPoolRespectsWorkerLimit` is the load-bearing test: each job bumps an atomic
counter on entry, records the peak with a compare-and-swap loop, and decrements on
exit; the assertion is that the peak never exceeds `workers`. `TestPoolWithSingleWorkerIsSerial`
is the same instrument with `New(1)`, asserting the peak is exactly 1 — the
semaphore must serialize even a single worker. The remaining tests prove each job
runs exactly once, that per-index errors are reported, and that `RunWithErrors`
joins multiple failures.

Create `internal/pool/pool_test.go`:

```go
package pool

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestPoolRunsAllJobsExactlyOnce(t *testing.T) {
	t.Parallel()

	const jobCount = 100
	var counter int64
	jobs := make([]Job, jobCount)
	for i := range jobs {
		jobs[i] = func() error {
			atomic.AddInt64(&counter, 1)
			return nil
		}
	}

	results := New(8).Run(jobs)
	if len(results) != jobCount {
		t.Fatalf("results = %d, want %d", len(results), jobCount)
	}
	if got := atomic.LoadInt64(&counter); got != jobCount {
		t.Fatalf("job invocations = %d, want %d", got, jobCount)
	}
	for i, r := range results {
		if r.Index != i {
			t.Fatalf("results[%d].Index = %d", i, r.Index)
		}
		if r.Err != nil {
			t.Fatalf("results[%d].Err = %v", i, r.Err)
		}
	}
}

func peakConcurrency(t *testing.T, workers, jobCount int) int64 {
	t.Helper()
	var concurrent, peak int64
	jobs := make([]Job, jobCount)
	for i := range jobs {
		jobs[i] = func() error {
			cur := atomic.AddInt64(&concurrent, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			atomic.AddInt64(&concurrent, -1)
			return nil
		}
	}
	New(workers).Run(jobs)
	return atomic.LoadInt64(&peak)
}

func TestPoolRespectsWorkerLimit(t *testing.T) {
	t.Parallel()
	if got := peakConcurrency(t, 4, 40); got > 4 {
		t.Fatalf("peak concurrency = %d, want <= 4", got)
	}
}

func TestPoolWithSingleWorkerIsSerial(t *testing.T) {
	t.Parallel()
	if got := peakConcurrency(t, 1, 25); got != 1 {
		t.Fatalf("peak concurrency = %d, want exactly 1", got)
	}
}

func TestPoolReportsEachError(t *testing.T) {
	t.Parallel()

	want := errors.New("kaboom")
	jobs := []Job{
		func() error { return nil },
		func() error { return want },
		func() error { return errors.New("other") },
	}

	results := New(2).Run(jobs)
	if results[0].Err != nil {
		t.Fatalf("results[0] = %v", results[0].Err)
	}
	if !errors.Is(results[1].Err, want) {
		t.Fatalf("results[1] = %v, want %v", results[1].Err, want)
	}
	if results[2].Err == nil {
		t.Fatalf("results[2] should be non-nil")
	}
}

func TestRunWithErrorsJoinsAllErrors(t *testing.T) {
	t.Parallel()

	errA := errors.New("a")
	errB := errors.New("b")
	jobs := []Job{
		func() error { return errA },
		func() error { return errB },
		func() error { return nil },
	}

	err := New(2).RunWithErrors(jobs)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined error %v does not wrap both a and b", err)
	}
}

func TestPoolSerializesResultsToOriginalIndex(t *testing.T) {
	t.Parallel()

	const jobCount = 50
	jobs := make([]Job, jobCount)
	for i := range jobs {
		want := i
		jobs[i] = func() error {
			if want%7 == 0 {
				return fmt.Errorf("job %d failed", want)
			}
			return nil
		}
	}

	results := New(8).Run(jobs)
	for i, r := range results {
		if r.Index != i {
			t.Fatalf("results[%d].Index = %d, want %d", i, r.Index, i)
		}
		wantErr := i%7 == 0
		if gotErr := r.Err != nil; gotErr != wantErr {
			t.Fatalf("results[%d].Err = %v, wantErr=%v", i, r.Err, wantErr)
		}
	}
}

func ExamplePool_Run() {
	jobs := []Job{
		func() error { return nil },
		func() error { return nil },
	}
	ok := true
	for _, r := range New(2).Run(jobs) {
		if r.Err != nil {
			ok = false
		}
	}
	fmt.Println(len(jobs), ok)
	// Output: 2 true
}
```

## Review

The pool is correct when three invariants hold under `-race`. Every job runs
exactly once — proved by the atomic invocation counter in
`TestPoolRunsAllJobsExactlyOnce`. Concurrency never exceeds the worker count —
proved by the peak-tracking tests, including the single-worker case that must stay
at exactly 1. And each `Result` lands at its job's original index — proved by
`TestPoolSerializesResultsToOriginalIndex`, which ties each index to a predictable
error. The subtle traps are the ones the concepts warned about: `wg.Add(1)` must
sit before the `go` statement or `Wait` can return early and drop work; `defer
wg.Done()` must be the first deferred line so a panicking job cannot deadlock
`Wait`; and each goroutine must write only its own index — passing `i` and `job` as
arguments is what guarantees that, and the race detector is the arbiter.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [errors.Join](https://pkg.go.dev/errors#Join)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-iife-immediate-invocation-scoped-init.md](02-iife-immediate-invocation-scoped-init.md)
