# Exercise 9: Fan-Out Worker Pool: Range Index Discard and Per-Scope Aggregation

A bounded worker pool fans jobs across goroutines, collects results, and
aggregates the first error, cancelling the rest. It uses `for _, job := range
jobs` — discarding the index deliberately — and shows that although Go 1.22+
per-iteration loop variables removed the classic capture bug, a shadowed `err`
inside the aggregation still hides failures. Correct aggregation assigns a
mutex-guarded outer error with `=`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
workerpool/                     module: example.com/workerpool
  go.mod
  pool.go                       Job, Result, Pool.Run (fan-out, first-error, cancel)
  cmd/
    demo/
      main.go                   square three jobs across three workers
  pool_test.go                  exactly-once execution, results collected, first error cancels, -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Pool.Run(ctx, jobs, work)` that fans jobs across `Workers` goroutines, buffers results, records the first error under a mutex (assigned with `=`), and cancels remaining work.
- Test: all jobs execute exactly once on the happy path (atomic counter); results collected regardless of order; a failing job surfaces a wrapped error and stops further work; run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/09-worker-pool-index-discard/cmd/demo
cd go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/09-worker-pool-index-discard
```

### The index discard, and the aggregation that must not shadow

The feeder ranges the job slice with `for _, job := range jobs`: the index is
genuinely unused, so `_` is the correct, self-documenting discard. Reading only
`job` is the whole intent.

The aggregation is where a shadow bites. Multiple workers can fail concurrently;
the pool wants the *first* error and then cancels the rest. The shared `firstErr`
lives in the function scope and is guarded by a mutex. Inside the worker's loop,
on failure, the correct code assigns that outer variable:

```go
mu.Lock()
if firstErr == nil {
	firstErr = fmt.Errorf("job %d: %w", job.ID, err) // = assigns the OUTER firstErr
}
mu.Unlock()
```

Write `firstErr := ...` there instead and you declare a fresh local that dies at
the end of the block; the outer `firstErr` stays `nil`, the pool returns success,
and the failure vanishes — even though the mutex, the counter, and the happy-path
test all look fine. Go 1.22's per-iteration loop variables do nothing to prevent
this; that change fixed loop-variable *capture*, not lexical shadowing of an
arbitrary outer variable. The results channel is buffered to `len(jobs)` so a
worker's send never blocks, and cancellation propagates through a derived context
so the feeder stops handing out work once someone fails.

Create `pool.go`:

```go
package workerpool

import (
	"context"
	"fmt"
	"sync"
)

// Job is a unit of work.
type Job struct {
	ID    int
	Input int
}

// Result is the output of a Job.
type Result struct {
	JobID  int
	Output int
}

// Pool runs jobs across a fixed number of workers.
type Pool struct {
	Workers int
}

// Run fans jobs across workers, collects results, and returns the first error,
// cancelling remaining work when one occurs.
func (p *Pool) Run(ctx context.Context, jobs []Job, work func(context.Context, Job) (Result, error)) ([]Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := p.Workers
	if workers < 1 {
		workers = 1
	}

	jobCh := make(chan Job)
	resCh := make(chan Result, len(jobs)) // buffered: a worker send never blocks

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				r, err := work(ctx, job)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("job %d: %w", job.ID, err)
					}
					mu.Unlock()
					cancel()
					return
				}
				resCh <- r
			}
		}()
	}

	go func() {
		defer close(jobCh)
		for _, job := range jobs { // index deliberately discarded
			select {
			case jobCh <- job:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(resCh)

	var results []Result
	for r := range resCh {
		results = append(results, r)
	}

	mu.Lock()
	err := firstErr
	mu.Unlock()
	return results, err
}
```

### The runnable demo

The demo squares three inputs across three workers and sorts the results for a
deterministic print (the pool itself makes no ordering promise).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	"example.com/workerpool"
)

func main() {
	pool := workerpool.Pool{Workers: 3}
	jobs := []workerpool.Job{{ID: 1, Input: 2}, {ID: 2, Input: 3}, {ID: 3, Input: 4}}

	results, err := pool.Run(context.Background(), jobs,
		func(ctx context.Context, j workerpool.Job) (workerpool.Result, error) {
			return workerpool.Result{JobID: j.ID, Output: j.Input * j.Input}, nil
		})
	if err != nil {
		fmt.Println("pool error:", err)
		return
	}

	sort.Slice(results, func(i, j int) bool { return results[i].JobID < results[j].JobID })
	for _, r := range results {
		fmt.Printf("job %d -> %d\n", r.JobID, r.Output)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job 1 -> 4
job 2 -> 9
job 3 -> 16
```

### Tests

`TestRunExecutesEachJobOnce` proves the happy path runs every job exactly once via
an atomic counter and collects all results. `TestRunFirstErrorStops` uses one
worker for determinism: a mid-stream failure surfaces a wrapped error and later
jobs never run — the assertion a shadowed `firstErr` would break.

Create `pool_test.go`:

```go
package workerpool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestRunExecutesEachJobOnce(t *testing.T) {
	t.Parallel()

	var executed atomic.Int64
	jobs := make([]Job, 50)
	for i := range jobs {
		jobs[i] = Job{ID: i, Input: i}
	}

	pool := Pool{Workers: 4}
	results, err := pool.Run(context.Background(), jobs,
		func(ctx context.Context, j Job) (Result, error) {
			executed.Add(1)
			return Result{JobID: j.ID, Output: j.Input * 2}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != int64(len(jobs)) {
		t.Fatalf("executed %d jobs, want %d", executed.Load(), len(jobs))
	}

	got := make(map[int]int, len(results))
	for _, r := range results {
		got[r.JobID] = r.Output
	}
	if len(got) != len(jobs) {
		t.Fatalf("collected %d results, want %d", len(got), len(jobs))
	}
	for _, j := range jobs {
		if got[j.ID] != j.Input*2 {
			t.Fatalf("job %d output = %d, want %d", j.ID, got[j.ID], j.Input*2)
		}
	}
}

func TestRunFirstErrorStops(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	jobs := []Job{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}}

	var executed atomic.Int64
	pool := Pool{Workers: 1} // single worker: deterministic order
	results, err := pool.Run(context.Background(), jobs,
		func(ctx context.Context, j Job) (Result, error) {
			executed.Add(1)
			if j.ID == 3 {
				return Result{}, errBoom
			}
			return Result{JobID: j.ID}, nil
		})

	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want wrap of errBoom (shadowed firstErr?)", err)
	}
	if len(results) != 2 {
		t.Fatalf("collected %d results, want 2 (jobs 1 and 2)", len(results))
	}
	if executed.Load() != 3 {
		t.Fatalf("executed %d jobs, want 3 (stopped after the failure)", executed.Load())
	}
}

func ExamplePool_Run() {
	pool := Pool{Workers: 2}
	results, _ := pool.Run(context.Background(), []Job{{ID: 1, Input: 5}},
		func(ctx context.Context, j Job) (Result, error) {
			return Result{JobID: j.ID, Output: j.Input * j.Input}, nil
		})
	fmt.Println(results[0].Output)
	// Output: 25
}
```

## Review

The pool is correct when every job on the happy path runs exactly once and a
failure both surfaces and stops further work. `TestRunFirstErrorStops` is the one a
shadow breaks: change `firstErr = ...` to `firstErr := ...` inside the worker and
the pool returns `nil`, failing the `errors.Is` assertion — exactly the silent bug
this module makes visible. The index discard (`for _, job := range jobs`) reads
only the value, and Go 1.22 loop scoping means the goroutines need no `job := job`
copy. Run `go test -race` to confirm the mutex guards `firstErr` and the buffered
results channel is race-free.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) and [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the aggregation and completion primitives.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — propagating cancellation to remaining work.
- [Go Wiki: LoopvarExperiment](https://go.dev/wiki/LoopvarExperiment) — the Go 1.22 per-iteration loop variable change and what it does (and does not) fix.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../10-type-inference-deep-dive/00-concepts.md](../10-type-inference-deep-dive/00-concepts.md)
