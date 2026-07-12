# Exercise 2: A Buffered-Channel Worker Pool (RunConcurrent)

A fixed-size pool draining a job queue is the workhorse of backend systems: N
workers pull from one buffered `jobs` channel, each writes to one buffered `results`
channel, a single producer closes `jobs` when the queue is exhausted, and a dedicated
closer goroutine closes `results` after every worker has finished so the fan-in loop
terminates. This exercise builds that pool and hardens it against the classic
"who closes results" deadlock.

This module is fully self-contained.

## What you'll build

```text
workerpool/                  module: example.com/workerpool
  go.mod                     go 1.26
  workerpool.go              RunConcurrent(n, workers) []int
  cmd/
    demo/
      main.go                runs RunConcurrent(10, 3) and prints count + sorted set
  workerpool_test.go         count assertion, multiset once-each, repeated-run deadlock guard
```

- Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
- Implement: `RunConcurrent(n, workers int) []int` — N workers square jobs 0..n-1 through buffered channels.
- Test: `RunConcurrent(100, 4)` returns exactly 100 results; every squared value appears once; a repeated-run loop that a missing `close(results)` would hang.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/03-buffered-vs-unbuffered-channels/02-buffered-worker-pool/cmd/demo
cd go-solutions/13-goroutines-and-channels/03-buffered-vs-unbuffered-channels/02-buffered-worker-pool
go mod edit -go=1.26
```

### The three roles and the one deadlock they defend against

A worker pool over channels has exactly three roles, and getting the ownership right
is the whole exercise.

The *producer* owns `jobs`: it sends every job and then closes `jobs`. Closing is
what tells the workers "no more work" — each worker's `for j := range jobs` loop ends
when `jobs` is closed and drained, and the worker returns. There is exactly one
producer, so exactly one goroutine closes `jobs`; that satisfies the "sole sender
closes" rule.

The *workers* are the senders on `results`. Here is the trap: there are N of them, so
no single worker can own the close of `results` — if a worker closed `results` on its
way out, the other workers would panic on their next send ("send on closed channel").
No worker may close `results`.

The *closer* resolves it. A dedicated goroutine does `wg.Wait()` then
`close(results)`. `wg.Wait()` blocks until all N workers have called `wg.Done()` (via
`defer`) and returned, which means no worker will ever send again, so closing
`results` is now safe and exactly-once. Closing `results` is what lets the main
goroutine's `for r := range results` fan-in loop terminate. Forget this closer and
the fan-in loop blocks forever with no more senders and no close — a deadlock the Go
runtime reports as "all goroutines are asleep".

Why buffered channels here? `jobs` is buffered to `n` so the producer can enqueue the
whole batch without a worker attached yet — the producer never blocks in this bounded
batch. `results` is buffered to `n` so a worker never blocks handing back a result
even if the fan-in loop is momentarily behind; in a bounded batch this guarantees no
worker stalls on a full results buffer. In a long-running server you would instead
bound these to a burst size and let backpressure do its job (later exercises), but for
a finite batch, sizing to `n` keeps every goroutine moving.

The result order is *not* deterministic — workers race to pull jobs and race to push
results — so the tests assert on the *count* and on a *multiset* (every squared value
present exactly once), never on order.

Create `workerpool.go`:

```go
package workerpool

import "sync"

// RunConcurrent squares the integers 0..n-1 using a pool of `workers` goroutines.
// jobs and results are buffered; the producer closes jobs, and a dedicated closer
// closes results after every worker has finished so the fan-in loop terminates.
func RunConcurrent(n, workers int) []int {
	jobs := make(chan int, n)
	results := make(chan int, n)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results <- j * j
			}
		}()
	}

	// Sole producer: enqueue every job, then close jobs to signal completion.
	go func() {
		for i := range n {
			jobs <- i
		}
		close(jobs)
	}()

	// Sole closer: wait for all workers to finish, then close results.
	go func() {
		wg.Wait()
		close(results)
	}()

	var out []int
	for r := range results {
		out = append(out, r)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/workerpool"
)

func main() {
	got := workerpool.RunConcurrent(10, 3)
	slices.Sort(got)
	fmt.Printf("processed %d jobs\n", len(got))
	fmt.Printf("squares: %v\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 10 jobs
squares: [0 1 4 9 16 25 36 49 64 81]
```

### Tests

`TestRunConcurrentProcessesAllJobs` pins the count contract: 100 jobs across 4
workers yields exactly 100 results. `TestEverySquareAppearsOnce` builds a multiset
and asserts each `i*i` for `i` in `0..n-1` appears exactly once — proving no job is
dropped or double-processed regardless of scheduling. `TestRepeatedRunsDoNotDeadlock`
runs the pool many times in a loop; if the closer were missing, the fan-in `range`
would hang and the whole test would blow past `go test`'s timeout — so a green run is
positive evidence the close protocol is intact. All of it runs under `-race`.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"fmt"
	"slices"
	"testing"
)

func TestRunConcurrentProcessesAllJobs(t *testing.T) {
	t.Parallel()

	got := RunConcurrent(100, 4)
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100", len(got))
	}
}

func TestEverySquareAppearsOnce(t *testing.T) {
	t.Parallel()

	const n = 50
	got := RunConcurrent(n, 8)

	seen := make(map[int]int, n)
	for _, v := range got {
		seen[v]++
	}
	for i := range n {
		if seen[i*i] != 1 {
			t.Fatalf("square %d appeared %d times, want exactly 1", i*i, seen[i*i])
		}
	}
	if len(seen) != n {
		t.Fatalf("distinct results = %d, want %d", len(seen), n)
	}
}

func TestRepeatedRunsDoNotDeadlock(t *testing.T) {
	t.Parallel()

	// A missing close(results) would hang here; the go test timeout catches it.
	for range 200 {
		got := RunConcurrent(20, 4)
		if len(got) != 20 {
			t.Fatalf("len = %d, want 20", len(got))
		}
	}
}

func ExampleRunConcurrent() {
	got := RunConcurrent(10, 3)
	slices.Sort(got) // results race in; sort makes the printed set deterministic
	fmt.Println(len(got), slices.Equal(got, []int{0, 1, 4, 9, 16, 25, 36, 49, 64, 81}))
	// Output: 10 true
}
```

## Review

The pool is correct when ownership is unambiguous: one producer closes `jobs`, no
worker closes `results`, and one closer does `wg.Wait()` then `close(results)`. The
`wg.Add(1)` must happen before the worker goroutine starts (it does, in the loop body,
before `go`), or `Wait` could race a late `Add`. Assert on count and multiset, never
order — the result sequence is inherently nondeterministic. The repeated-run test is
the practical guard against the "forgot to close results" deadlock: with the closer in
place it stays green; delete the closer and it hangs, which `go test`'s timeout turns
into a failure. Run `-race` to confirm the shared `results` channel and the WaitGroup
are used correctly.

## Resources

- [Go spec: close](https://go.dev/ref/spec#Close) — closing semantics, the panic on send-after-close and double-close.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — `Add`/`Done`/`Wait` and the ordering rules for `Add`.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in and the closer goroutine.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-bounded-stage-pipeline.md](01-bounded-stage-pipeline.md) | Next: [03-queue-depth-saturation-metric.md](03-queue-depth-saturation-metric.md)
