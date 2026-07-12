# Exercise 6: Closure Capture: Escapes in a Worker-Pool Callback

A closure that runs on another goroutine outlives the frame that created it, so
every variable it captures is forced onto the heap. This module builds a bounded
worker pool two ways — dispatching closures versus passing jobs as values — and
measures the per-dispatch closure allocation, while proving the modern
per-iteration loop variable prevents the classic capture-aliasing bug.

This module is fully self-contained.

## What you'll build

```text
workerpool/                   independent module: example.com/workerpool
  go.mod                      go 1.26
  pool.go                     Job, Result, process; Run (value dispatch),
                              RunClosures (closure dispatch); MakeTask, HandleJob
  cmd/
    demo/
      main.go                 runs the pool; shows the closure alloc
  pool_test.go                equality, -race correctness, no-aliasing, AllocsPerRun
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: a bounded pool `Run(jobs, workers)` that passes each job by value, a
`RunClosures` variant that dispatches per-job closures, and `MakeTask`/`HandleJob`
to isolate the closure allocation.
Test: both pools produce identical results; a `-race` correctness test; a
no-loop-var-aliasing test (each job processed exactly once with its own data); and
an `AllocsPerRun` test showing the closure dispatch allocates while value dispatch
does not.
Verify: `go test -count=1 -race ./...`, then observe the closure escape with
`go build -gcflags=-m ./... 2>&1 | grep 'func literal escapes to heap'`.

### Why a dispatched closure escapes, and why the loop variable is safe now

When you write `go func() { ... captured ... }()` or send a `func()` down a channel
for a worker to run, the closure's lifetime is no longer bounded by the frame that
created it — it runs later, on another goroutine. The compiler cannot prove the
captured variables die with the frame, so it heap-allocates the closure and the
variables it closes over. `MakeTask` shows this in isolation: it returns a
`func() Result` capturing a `Job`, and because the closure is returned (outlives
the call), it escapes — one allocation per dispatch. `HandleJob` takes the same
`Job` by value and computes inline; nothing is captured, nothing escapes.

`RunClosures` pays that cost once per job: it sends `len(jobs)` closures down a
channel. `Run` avoids it entirely by sending an index and letting the worker call
`process(jobs[i])` directly — the job data travels as a value the worker reads, no
closure is built. Both are correct and both are legitimate designs; the point is
to *see* the allocation the closure pattern adds so you can choose it deliberately
rather than by accident on a hot dispatch path.

There is a second, historically nasty trap hiding here: capturing a loop variable.
Before Go 1.22, `for i := range jobs { tasks <- func(){ use(i) } }` captured a
*single* `i` shared by every iteration, so all closures observed the final value —
a data-corruption classic. Since Go 1.22 each iteration gets its own `i`, so every
closure captures a distinct variable and the code is correct as written, with no
`i := i` shadowing needed. `TestNoLoopVarAliasing` proves it: each job is processed
with its own data, exactly once.

`Run` writes results at distinct indices from different workers, which is race-free
(disjoint memory), and `sync.WaitGroup` establishes the happens-before edge so the
caller reads a fully-populated slice.

Create `pool.go`:

```go
package pool

import "sync"

// Job is a unit of work; N drives the computation.
type Job struct {
	ID int
	N  int
}

// Result is the output for one Job.
type Result struct {
	ID  int
	Sum int
}

// process sums 1..N. Pure and allocation-free.
func process(j Job) Result {
	sum := 0
	for i := 1; i <= j.N; i++ {
		sum += i
	}
	return Result{ID: j.ID, Sum: sum}
}

// Run processes jobs with a bounded pool of workers. Each worker receives an
// index and reads the job VALUE directly, so no per-job closure is allocated.
func Run(jobs []Job, workers int) []Result {
	results := make([]Result, len(jobs))
	idx := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				results[i] = process(jobs[i])
			}
		}()
	}
	for i := range jobs {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return results
}

// RunClosures dispatches each job as a closure sent to the workers. Every closure
// captures its per-iteration index and escapes to the heap because it runs on
// another goroutine.
func RunClosures(jobs []Job, workers int) []Result {
	results := make([]Result, len(jobs))
	tasks := make(chan func())
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				task()
			}
		}()
	}
	for i := range jobs {
		tasks <- func() { results[i] = process(jobs[i]) }
	}
	close(tasks)
	wg.Wait()
	return results
}

// MakeTask returns a closure capturing j; it escapes because it is returned.
//
//go:noinline
func MakeTask(j Job) func() Result {
	return func() Result { return process(j) }
}

// HandleJob computes the result inline, capturing nothing.
//
//go:noinline
func HandleJob(j Job) Result {
	return process(j)
}
```

### The runnable demo

The demo runs the value-dispatch pool over three jobs (each `Sum` is the triangular
number `N(N+1)/2`), then prints the per-dispatch closure allocation from
`MakeTask`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/workerpool"
)

func main() {
	jobs := []pool.Job{{ID: 0, N: 10}, {ID: 1, N: 100}, {ID: 2, N: 4}}
	for _, r := range pool.Run(jobs, 2) {
		fmt.Printf("job %d sum %d\n", r.ID, r.Sum)
	}

	var sinkFn func() pool.Result
	clo := testing.AllocsPerRun(1000, func() {
		sinkFn = pool.MakeTask(pool.Job{ID: 0, N: 10})
	})
	_ = sinkFn
	fmt.Printf("closure dispatch allocs/op: %.0f\n", clo)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job 0 sum 55
job 1 sum 5050
job 2 sum 10
closure dispatch allocs/op: 1
```

### Tests

`TestPoolsAgree` proves the two dispatch strategies compute the same results.
`TestConcurrentCorrect` runs a larger batch and checks every result under `-race`.
`TestNoLoopVarAliasing` is the aliasing proof: each job must be processed with its
own `N`, which fails if closures shared one loop variable. `TestClosureAllocates`
compares `MakeTask` (captures, escapes) against `HandleJob` (captures nothing).

Create `pool_test.go`:

```go
package pool

import "testing"

func triangular(n int) int { return n * (n + 1) / 2 }

func makeJobs(n int) []Job {
	jobs := make([]Job, n)
	for i := range jobs {
		jobs[i] = Job{ID: i, N: i + 1}
	}
	return jobs
}

func TestPoolsAgree(t *testing.T) {
	t.Parallel()
	jobs := makeJobs(50)
	a := Run(jobs, 4)
	b := RunClosures(jobs, 4)
	for i := range jobs {
		if a[i] != b[i] {
			t.Fatalf("job %d: value=%+v closure=%+v", i, a[i], b[i])
		}
	}
}

func TestConcurrentCorrect(t *testing.T) {
	t.Parallel()
	jobs := makeJobs(500)
	got := Run(jobs, 8)
	for i, r := range got {
		if r.ID != i {
			t.Fatalf("result %d has ID %d", i, r.ID)
		}
		if want := triangular(jobs[i].N); r.Sum != want {
			t.Fatalf("job %d sum = %d, want %d", i, r.Sum, want)
		}
	}
}

func TestNoLoopVarAliasing(t *testing.T) {
	t.Parallel()
	jobs := makeJobs(100)
	got := RunClosures(jobs, 4)
	for i := range jobs {
		if want := triangular(jobs[i].N); got[i].Sum != want {
			t.Fatalf("job %d processed with wrong data: sum=%d want=%d (loop-var aliasing?)", i, got[i].Sum, want)
		}
	}
}

var (
	sinkFn  func() Result
	sinkRes Result
)

func TestClosureAllocates(t *testing.T) {
	j := Job{ID: 0, N: 10}
	clo := testing.AllocsPerRun(1000, func() { sinkFn = MakeTask(j) })
	arg := testing.AllocsPerRun(1000, func() { sinkRes = HandleJob(j) })
	if clo < 1 {
		t.Errorf("MakeTask allocs/op = %.1f, want >= 1 (closure escapes)", clo)
	}
	if arg != 0 {
		t.Errorf("HandleJob allocs/op = %.1f, want 0 (nothing captured)", arg)
	}
}

func BenchmarkRun(b *testing.B) {
	jobs := makeJobs(100)
	b.ReportAllocs()
	for b.Loop() {
		_ = Run(jobs, 4)
	}
}

func BenchmarkRunClosures(b *testing.B) {
	jobs := makeJobs(100)
	b.ReportAllocs()
	for b.Loop() {
		_ = RunClosures(jobs, 4)
	}
}
```

## Review

Both pools are correct when they agree job-for-job and every result carries its
own data; `TestNoLoopVarAliasing` is what makes the closure dispatch trustworthy on
Go 1.22+, where the per-iteration loop variable removes the old shared-capture bug.
The allocation lesson is that a dispatched closure escapes — confirm it with
`go build -gcflags=-m` and look for `func literal escapes to heap` on
`RunClosures` and `MakeTask` — while passing the job as a value keeps the dispatch
allocation-free. The mistake to avoid is reaching for a `chan func()` "task queue"
on a high-rate dispatch path without realizing each submission allocates a closure;
when the work is uniform, send the data and let the worker call the function.

## Resources

- [Go 1.22 release notes: loop variable scoping](https://go.dev/blog/loopvar-preview) — per-iteration variables end the capture-aliasing bug.
- [Go Blog: Escape analysis](https://go.dev/blog/escape-analysis) — why closures that outlive the frame escape.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the happens-before edge the pool relies on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-slice-preallocation-and-growth.md](05-slice-preallocation-and-growth.md) | Next: [07-large-struct-value-vs-pointer.md](07-large-struct-value-vs-pointer.md)
