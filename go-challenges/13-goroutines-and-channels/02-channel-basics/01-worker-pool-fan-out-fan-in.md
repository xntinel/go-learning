# Exercise 1: Worker Pool with Unbuffered Fan-Out / Fan-In

The worker pool is the first channel topology every backend engineer reaches for:
a bounded set of goroutines chews through a slice of work concurrently, and the
caller collects the results. This exercise builds it with unbuffered channels so
the synchronization is explicit, and proves under the race detector that no job is
lost, duplicated, or deadlocked.

This module is fully self-contained: its own module, its own `internal/pool`
package, its own demo, and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
workerpool/                  independent module: example.com/workerpool
  go.mod                     go 1.26
  internal/pool/pool.go      type Pool; New(workers), Run(jobs) results
  internal/pool/pool_test.go table tests + 100-job uniqueness + Example
  cmd/demo/main.go           runnable demo: doubles a batch of ints
```

- Files: `internal/pool/pool.go`, `internal/pool/pool_test.go`, `cmd/demo/main.go`.
- Implement: `New(workers int) *Pool` and `(*Pool).Run(jobs []int) []int` that fans jobs out to N workers over an unbuffered channel and fans results back over another.
- Test: all jobs processed, empty input, more workers than jobs, one worker, and a 100-job run asserting 100 unique doubled values.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/workerpool/internal/pool ~/go-exercises/workerpool/cmd/demo
cd ~/go-exercises/workerpool
go mod init example.com/workerpool
```

### How the three channels and the WaitGroup fit together

`Run` wires four moving parts. The `jobsCh` channel carries work *into* the
workers; the `resultsCh` channel carries answers *out*. Both are unbuffered, so a
worker receiving a job blocks until a job is sent, and a worker sending a result
blocks until the collector receives it — the pool is paced by whichever side is
slower, with no hidden buffering.

The lifecycle is the subtle part, and it is where most worker pools have bugs.
Three things must happen in the right order:

1. A feeder goroutine sends every job onto `jobsCh`, then `close(jobsCh)`. Closing
   is what lets each worker's `for j := range jobsCh` loop terminate; without the
   close, the workers block forever on an empty channel and never exit.
2. Each worker, after its range loop ends, calls `wg.Done()`. The `WaitGroup` was
   `Add`ed to once per worker *before* the workers launched — adding before launch
   is essential, because a separate goroutine is going to `Wait()` on it, and
   `Add` racing `Wait` is a bug.
3. A closer goroutine calls `wg.Wait()` and then `close(resultsCh)`. This is the
   ownership discipline: the results channel has many producers (all the workers),
   so no single worker may close it; instead one goroutine closes it exactly once,
   after every worker has finished. That close is what lets the collector's
   `for r := range resultsCh` loop terminate and return.

The main goroutine ranges `resultsCh` and appends. Because the workers finish in
nondeterministic order, the results come back in arbitrary order — that is fine
here (Exercise 9 shows how to restore input order when a contract requires it).

Create `internal/pool/pool.go`:

```go
package pool

import "sync"

// Pool runs a fixed number of worker goroutines that process integer jobs
// concurrently. It is the canonical unbuffered fan-out / fan-in topology.
type Pool struct {
	workers int
}

// New returns a Pool that will run with the given number of workers. A
// non-positive count is normalized to a single worker so Run never spawns zero
// workers (which would deadlock on the jobs channel).
func New(workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{workers: workers}
}

// Run fans jobs out to the workers, collects every result, and returns them.
// Results are returned in arbitrary (completion) order.
func (p *Pool) Run(jobs []int) []int {
	jobsCh := make(chan int)
	resultsCh := make(chan int)
	var wg sync.WaitGroup

	// Fan-out: start the workers. Add to the WaitGroup before launching.
	for range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				resultsCh <- j * 2
			}
		}()
	}

	// Feeder: send every job, then close so the workers' ranges terminate.
	go func() {
		for _, j := range jobs {
			jobsCh <- j
		}
		close(jobsCh)
	}()

	// Owner of resultsCh closes it exactly once, after all workers finish.
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	// Fan-in: collect until resultsCh is closed and drained.
	var out []int
	for r := range resultsCh {
		out = append(out, r)
	}
	return out
}
```

### The runnable demo

The demo doubles a small batch and sorts before printing so the output is
deterministic despite the nondeterministic completion order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/workerpool/internal/pool"
)

func main() {
	got := pool.New(3).Run([]int{1, 2, 3, 4, 5})
	sort.Ints(got)
	fmt.Println("doubled:", got)
	fmt.Println("count:", len(got))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
doubled: [2 4 6 8 10]
count: 5
```

### Tests

The first four tests are the original suite: they pin the core contract across the
cases that break naive pools — the happy path, empty input (the feeder closes
`jobsCh` immediately and every range terminates with zero iterations), more
workers than jobs (some workers receive nothing and still exit cleanly), and a
single worker (serialized, still correct). `TestRunPreservesAllResults` is the
property test that matters most under `-race`: 100 distinct jobs must produce 100
distinct doubled values, proving nothing is dropped and nothing is duplicated by
the fan-out. Collecting into a set catches both failure modes at once.

Create `internal/pool/pool_test.go`:

```go
package pool

import (
	"fmt"
	"sort"
	"testing"
)

func TestRunProcessesAllJobs(t *testing.T) {
	t.Parallel()

	got := New(2).Run([]int{1, 2, 3, 4, 5})
	sort.Ints(got)
	want := []int{2, 4, 6, 8, 10}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], w)
		}
	}
}

func TestRunWithNoJobs(t *testing.T) {
	t.Parallel()

	got := New(2).Run(nil)
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestRunWithMoreWorkersThanJobs(t *testing.T) {
	t.Parallel()

	got := New(10).Run([]int{1, 2, 3})
	sort.Ints(got)
	want := []int{2, 4, 6}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], w)
		}
	}
}

func TestRunWithOneWorker(t *testing.T) {
	t.Parallel()

	got := New(1).Run([]int{1, 2, 3, 4})
	sort.Ints(got)
	want := []int{2, 4, 6, 8}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], w)
		}
	}
}

func TestRunPreservesAllResults(t *testing.T) {
	t.Parallel()

	jobs := make([]int, 100)
	for i := range jobs {
		jobs[i] = i
	}
	got := New(8).Run(jobs)
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100", len(got))
	}
	seen := make(map[int]bool, 100)
	for _, v := range got {
		if seen[v] {
			t.Fatalf("duplicate result %d", v)
		}
		seen[v] = true
	}
	for i := range jobs {
		if !seen[i*2] {
			t.Fatalf("missing result for job %d (want %d)", i, i*2)
		}
	}
}

func ExamplePool_Run() {
	got := New(4).Run([]int{10, 20, 30})
	sort.Ints(got)
	fmt.Println(got)
	// Output: [20 40 60]
}
```

## Review

The pool is correct when three invariants hold. Every job produces exactly one
result: `TestRunPreservesAllResults` proves this with a 100-element uniqueness set
that catches both a dropped job (missing value) and a double-processed job
(duplicate value). Every goroutine terminates: the feeder closes `jobsCh` so the
workers' ranges end, and the closer closes `resultsCh` after `wg.Wait()` so the
collector's range ends — run under `-race` with the default `go test` timeout,
which turns any missing close into a reported hang rather than a silent leak. And
the results channel is closed exactly once by its single owner, never by a worker.
The most common regressions here are calling `wg.Add` inside the worker (racing
`Wait`), and having a worker close `resultsCh` (a second close panics as soon as
another worker also finishes). Keep `Add` before launch and the close in the lone
closer goroutine.

## Resources

- [Go Language Spec: Channel types](https://go.dev/ref/spec#Channel_types) — the semantics of `make(chan T)`, send, receive, and `close`.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the worker-pool and semaphore idioms in the language's own words.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — `Add`/`Done`/`Wait` and the rule that `Add` must not race `Wait`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-request-reply-command-channel.md](02-request-reply-command-channel.md)
