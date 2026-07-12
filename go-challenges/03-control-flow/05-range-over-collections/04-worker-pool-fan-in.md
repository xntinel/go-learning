# Exercise 4: Fan-in a Worker Pool by Ranging the Results Channel

A bounded-concurrency job processor is the workhorse of backend systems: N workers
pull jobs off an input channel and push results onto a shared results channel, and
a single collector drains that results channel with `for r := range results`. The
subtle part is termination — the collector's `range` ends only when someone closes
`results`, and the canonical way to close it exactly once is a
`go func(){ wg.Wait(); close(results) }()` closer goroutine. This module builds that
pattern and proves it terminates.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
workerpool/                 independent module: example.com/workerpool
  go.mod                    go 1.24
  workerpool.go             Job, Result, Process(jobs, workers, work) []Result
  cmd/
    demo/
      main.go               runnable demo: square 8 jobs through 3 workers, print sum
  workerpool_test.go        100 jobs x 4 workers (-race) exact count+sum; timeout deadlock guard
```

- Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
- Implement: `Process(jobs []Job, workers int, work func(Job) Result) []Result` fanning jobs to N workers and fanning results back in by ranging the results channel until a WaitGroup-driven closer closes it.
- Test: 100 jobs through 4 workers under `-race`, asserting exactly 100 results and the correct aggregate; a timeout guard proving the range terminates.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/04-worker-pool-fan-in/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/04-worker-pool-fan-in
go mod edit -go=1.24
```

### The close-on-WaitGroup pattern

The pipeline has three stages. A feeder pushes every job into a `jobs` channel and
closes it, so each worker's `for j := range jobs` drains its share and then exits
when the channel is closed and empty. Each of the N workers, on exit, calls
`wg.Done()`. A collector ranges a shared `results` channel and accumulates every
result. The question that makes or breaks the pattern is: who closes `results`, and
when?

It cannot be a worker — several workers write to `results`, so no single worker
knows when the last result has been sent, and if two workers both closed it you get
a panic. The answer is a dedicated closer goroutine:

```go
go func() {
	wg.Wait()      // block until every worker has returned
	close(results) // now, and only now, no more sends can happen
}()
```

`wg.Wait()` returns only after all workers have called `wg.Done()`, which they do
only after their `for j := range jobs` loop has drained. At that instant no worker
will ever send again, so closing `results` is safe, and it is closed exactly once.
That close is what lets the collector's `for r := range results` terminate — without
it, the collector blocks forever on an empty-but-open channel and the whole program
hangs. This is the single most common concurrency bug in fan-in code, and the
timeout test below exists to prove this implementation does not have it.

Ordering note: the collector must run *concurrently* with the workers (results is
unbuffered here), and the closer must be a separate goroutine so `wg.Wait()` does
not block the workers from sending. The result slice comes out in nondeterministic
order, so callers that need order must sort; here we return whatever order the
results arrived and let tests assert on order-independent aggregates.

Create `workerpool.go`:

```go
package workerpool

import "sync"

// Job is a unit of work.
type Job struct {
	ID      int
	Payload int
}

// Result is the outcome of processing one Job.
type Result struct {
	ID    int
	Value int
}

// Process runs work over jobs using the given number of workers and returns all
// results. Results arrive in nondeterministic order. It panics if workers < 1.
func Process(jobs []Job, workers int, work func(Job) Result) []Result {
	if workers < 1 {
		panic("workerpool: workers must be >= 1")
	}

	jobCh := make(chan Job)
	resultCh := make(chan Result)

	// Feeder: push all jobs then close so workers' range loops end.
	go func() {
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
	}()

	// Workers: each drains jobCh until closed, pushing results.
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				resultCh <- work(j)
			}
		}()
	}

	// Closer: when every worker has exited, close resultCh so the collector's
	// range below terminates. Closing here (not in a worker) closes it once.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collector: fan-in by ranging the results channel until it is closed.
	out := make([]Result, 0, len(jobs))
	for r := range resultCh {
		out = append(out, r)
	}
	return out
}
```

### The runnable demo

The demo squares eight jobs through three workers and prints the count and sum of
outputs — order-independent so the output is stable despite concurrency.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/workerpool"
)

func main() {
	jobs := make([]workerpool.Job, 8)
	for i := range jobs {
		jobs[i] = workerpool.Job{ID: i, Payload: i + 1}
	}

	results := workerpool.Process(jobs, 3, func(j workerpool.Job) workerpool.Result {
		return workerpool.Result{ID: j.ID, Value: j.Payload * j.Payload}
	})

	sum := 0
	for _, r := range results {
		sum += r.Value
	}
	fmt.Printf("results=%d sum=%d\n", len(results), sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
results=8 sum=204
```

### Tests

The load test pushes 100 jobs through 4 workers under `-race` and asserts exactly
100 results plus the correct aggregate sum — a count that only holds if no result
is dropped or duplicated and the pool is race-free. The termination test runs
`Process` inside a goroutine and fails if it does not finish before the test
deadline, proving the close-on-WaitGroup pattern actually ends the collector's
range rather than hanging.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"testing"
	"time"
)

func TestProcessExactCountAndSum(t *testing.T) {
	t.Parallel()
	const n = 100
	jobs := make([]Job, n)
	wantSum := 0
	for i := range jobs {
		jobs[i] = Job{ID: i, Payload: i}
		wantSum += i * 2
	}

	results := Process(jobs, 4, func(j Job) Result {
		return Result{ID: j.ID, Value: j.Payload * 2}
	})

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	gotSum := 0
	seen := make(map[int]bool, n)
	for _, r := range results {
		gotSum += r.Value
		if seen[r.ID] {
			t.Fatalf("duplicate result for job %d", r.ID)
		}
		seen[r.ID] = true
	}
	if gotSum != wantSum {
		t.Fatalf("sum = %d, want %d", gotSum, wantSum)
	}
}

func TestProcessTerminates(t *testing.T) {
	t.Parallel()
	jobs := []Job{{ID: 0, Payload: 1}, {ID: 1, Payload: 2}}

	done := make(chan []Result, 1)
	go func() {
		done <- Process(jobs, 2, func(j Job) Result {
			return Result{ID: j.ID, Value: j.Payload}
		})
	}()

	select {
	case got := <-done:
		if len(got) != 2 {
			t.Fatalf("got %d results, want 2", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not terminate: results channel was never closed")
	}
}

func TestProcessPanicsOnZeroWorkers(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for workers < 1")
		}
	}()
	Process([]Job{{ID: 0}}, 0, func(j Job) Result { return Result{} })
}
```

## Review

The pool is correct when the collector receives exactly one result per job and the
program terminates. Two failure modes dominate. First, closing `results` from
inside a worker: with multiple workers this either double-closes (panic) or closes
while another worker is mid-send (send-on-closed panic) — the `wg.Wait(); close`
closer avoids both by closing exactly once after all workers exit. Second, forgetting
to close `results` at all: the collector's `range` never ends and the program hangs,
which `TestProcessTerminates` catches with a deadline. Run `go test -race` to
confirm the two channels and the WaitGroup coordinate without a data race.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go Specification: Close and receive on a closed channel](https://go.dev/ref/spec#Close)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-deterministic-config-diff.md](03-deterministic-config-diff.md) | Next: [05-ttl-cache-sweep.md](05-ttl-cache-sweep.md)
