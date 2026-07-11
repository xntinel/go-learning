# Exercise 4: A Bounded Worker-Pool Dispatcher

The integer range form is the natural way to say "launch exactly N workers." This exercise builds a `pool` package whose `Dispatch` function spins up a fixed number of goroutines with `for range workers`, feeds them a jobs channel, fans the results back in input order, and shuts down cleanly. The accompanying `-race` test asserts that every job is processed exactly once.

This module is fully self-contained. It has its own `go mod init`, defines every type and function it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pool.go              Dispatch (bounded fan-out/fan-in), ErrNoWorkers
cmd/
  demo/
    main.go          dispatch squaring jobs over a small pool; show reject + empty
pool_test.go         exactly-once under -race, order preservation, zero-workers, empty input
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Dispatch[T, R any](workers int, jobs []T, fn func(T) R) ([]R, error)` and the sentinel `ErrNoWorkers`.
- Test: run thousands of jobs through a small pool under `-race`, assert each job is processed exactly once and results are aligned with input order; reject a non-positive worker count; return an empty result for empty input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p worker-pool-dispatcher/cmd/demo && cd worker-pool-dispatcher
go mod init example.com/pool
```

### Why the pool is bounded, and how the parts fit together

A "bounded" pool is the whole point. Spawning one goroutine per job is easy but unbounded: ten million jobs become ten million goroutines, and the program competes with itself for CPU, memory, and any downstream resource (sockets, file handles, a database connection limit). A worker pool fixes the concurrency at a chosen width: `Dispatch` launches exactly `workers` goroutines regardless of how many jobs arrive. This is precisely the place `for range workers` reads as its own intent — the loop's only job is to start that many workers, and the iteration number is never used, so the index is dropped.

The data flow is the classic fan-out/fan-in. One unbuffered channel `in` carries `(index, job)` items out to the workers (fan-out: many workers receive from one channel). Each worker loops with `for it := range in`, which drains items until the channel is closed, and writes its computed value into `results[it.idx]`. The results are collected not through a second channel but by having each worker write to a distinct slot of a preallocated `results` slice — that is the fan-in, and it preserves input order for free because the slot index travels with the job.

Two properties make the slice writes safe under the race detector. First, every job is sent exactly once, so no two workers ever hold the same index; each `results[it.idx]` is written by exactly one goroutine. Concurrent writes to *different* elements of a slice are not a data race — the race detector flags concurrent access to the *same* memory location, and distinct indices are distinct locations. Second, the main goroutine only reads `results` after `wg.Wait()` returns, and `Wait` establishes a happens-before edge with every worker's final `Done`, so all writes are visible before the read. Without that barrier the read could race the writes even though the writes do not race each other.

Clean shutdown is the part naive pools get wrong. After the producer has sent every job it closes `in`. Closing a channel does not lose buffered values and it broadcasts to every receiver: each worker's `for range in` loop sees the channel drained and closed, exits, and runs its deferred `wg.Done`. The producer then calls `wg.Wait`, which blocks until all `workers` goroutines have returned. No worker is leaked, no goroutine is left blocked on a receive, and the function does not return until the pool has fully drained. The two edge cases are handled before any goroutine starts: a non-positive `workers` is a programming error and returns `ErrNoWorkers`; an empty `jobs` slice returns an empty, non-nil result with no work done. Capping `workers` to `len(jobs)` avoids starting goroutines that would only ever see an immediate close.

Create `pool.go`:

```go
package pool

import (
	"errors"
	"sync"
)

// ErrNoWorkers is returned when Dispatch is asked to run with a non-positive
// number of workers, which is always a programming error rather than a
// legitimate "do nothing" request.
var ErrNoWorkers = errors.New("workers must be positive")

// Dispatch runs fn over every element of jobs using exactly workers goroutines
// (capped to len(jobs)), then returns the results aligned with the input order:
// results[i] is fn(jobs[i]). It fans jobs out over one channel, fans results
// back by having each worker write its own result slot, and shuts the pool down
// cleanly by closing the channel and waiting for every worker to finish.
func Dispatch[T, R any](workers int, jobs []T, fn func(T) R) ([]R, error) {
	if workers <= 0 {
		return nil, ErrNoWorkers
	}

	n := len(jobs)
	if n == 0 {
		return []R{}, nil
	}
	if workers > n {
		workers = n
	}

	type item struct {
		idx int
		job T
	}

	in := make(chan item)
	results := make([]R, n)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range in {
				// Each index is delivered to exactly one worker, so this write
				// targets a slot no other goroutine touches: no data race.
				results[it.idx] = fn(it.job)
			}
		}()
	}

	for i, job := range jobs {
		in <- item{idx: i, job: job}
	}
	close(in)
	wg.Wait()

	return results, nil
}
```

The producer loop runs on the calling goroutine and blocks on `in <- item{...}` until a worker is ready to receive, so the unbuffered channel paces the hand-off without a buffer. When the last job is sent, `close(in)` ends every worker's range loop; `wg.Wait` then blocks until all of them have returned. The `for range workers` loop launches the goroutines; the `for i, job := range jobs` loop feeds them. Returning `results` only after `Wait` is what makes the collected slice safe to read.

### The runnable demo

The demo dispatches eight squaring jobs over a three-worker pool, then shows the two boundaries: a rejected worker count and an empty input. Because results are aligned with input order, the output is deterministic even though the work runs concurrently.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/pool"
)

func main() {
	jobs := []int{1, 2, 3, 4, 5, 6, 7, 8}
	squares, err := pool.Dispatch(3, jobs, func(n int) int {
		return n * n
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("squares:", squares)

	if _, err := pool.Dispatch(0, jobs, func(n int) int { return n }); err != nil {
		fmt.Println("rejected:", err)
	}

	empty, err := pool.Dispatch(4, nil, func(n int) int { return n })
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("empty: %v (len %d)\n", empty, len(empty))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
squares: [1 4 9 16 25 36 49 64]
rejected: workers must be positive
empty: [] (len 0)
```

### Tests

The central test runs a large batch of distinct jobs through a small pool under `-race`. Each job's value doubles as its identity, so the worker can record `atomic.AddInt64(&processed[x], 1)`; after the pool drains, every counter must be exactly `1`. That single assertion proves three things at once: no job was dropped (would be `0`), no job was handled twice (would be `2`), and the dispatcher genuinely fanned the work out. The same test pins the result alignment, which can only hold if each result landed in the slot of the job that produced it. `TestDispatchOrderSmall` re-checks alignment on a tiny input. `TestNoWorkers` and `TestEmptyJobs` pin the two boundaries.

The `atomic` package is used for the counters because, although each `processed[x]` is written by exactly one worker in a correct implementation, a *buggy* dispatcher that delivered an index twice would create a genuine concurrent write; the atomic increment keeps the test itself race-free so the detector reports the dispatcher's bug rather than the test's.

Create `pool_test.go`:

```go
package pool

import (
	"sync/atomic"
	"testing"
)

func TestDispatchExactlyOnce(t *testing.T) {
	t.Parallel()

	const n = 2000
	jobs := make([]int, n)
	for i := range n {
		jobs[i] = i
	}

	processed := make([]int64, n)
	got, err := Dispatch(8, jobs, func(x int) int {
		atomic.AddInt64(&processed[x], 1)
		return x * 2
	})
	if err != nil {
		t.Fatalf("Dispatch error = %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(results) = %d, want %d", len(got), n)
	}
	for i := range n {
		if c := atomic.LoadInt64(&processed[i]); c != 1 {
			t.Fatalf("job %d processed %d times, want exactly 1", i, c)
		}
		if got[i] != i*2 {
			t.Fatalf("results[%d] = %d, want %d", i, got[i], i*2)
		}
	}
}

func TestDispatchOrderSmall(t *testing.T) {
	t.Parallel()

	got, err := Dispatch(3, []int{10, 20, 30, 40, 50}, func(x int) int { return x + 1 })
	if err != nil {
		t.Fatalf("Dispatch error = %v", err)
	}
	want := []int{11, 21, 31, 41, 51}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("results = %v, want %v", got, want)
		}
	}
}

func TestNoWorkers(t *testing.T) {
	t.Parallel()

	if _, err := Dispatch(0, []int{1, 2}, func(x int) int { return x }); err != ErrNoWorkers {
		t.Fatalf("err = %v, want ErrNoWorkers", err)
	}
	if _, err := Dispatch(-1, []int{1, 2}, func(x int) int { return x }); err != ErrNoWorkers {
		t.Fatalf("err = %v, want ErrNoWorkers", err)
	}
}

func TestEmptyJobs(t *testing.T) {
	t.Parallel()

	got, err := Dispatch(4, []int{}, func(x int) int { return x })
	if err != nil {
		t.Fatalf("Dispatch error = %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("empty dispatch = %v, want empty non-nil slice", got)
	}
}
```

## Review

The dispatcher is correct when it launches a fixed number of workers, processes each job once, and shuts down without leaking a goroutine. `for range workers` starts exactly the requested width; the unbuffered `in` channel paces the hand-off; each worker writes to its own `results` slot so the concurrent writes target distinct memory and never race; and `close(in)` followed by `wg.Wait` drains every worker before the result slice is read. Reading `results` only after `Wait` is the happens-before barrier that makes the collected values visible and the `-race` run clean.

The traps this code avoids: spawning one goroutine per job instead of a bounded pool (unbounded concurrency); forgetting to `close(in)`, which leaves every worker blocked on a receive and `Wait` blocked forever; reading `results` before `Wait` returns, which races the workers' writes even though those writes do not race each other; and collecting results into a shared slice with `append` from multiple goroutines, which *is* a race on the slice header. The exactly-once test under `-race` — every per-job counter equal to one, every result aligned — establishes that the fan-out, the fan-in, and the shutdown all hold together.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the rule that `for range n` runs the body n times, used here to launch a fixed number of workers.
- [Go by Example: Worker Pools](https://gobyexample.com/worker-pools) — a minimal worker-pool over goroutines and a jobs channel, the pattern this exercise bounds and generalizes.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out/fan-in model and why closing a channel broadcasts a clean shutdown signal to all receivers.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — `Add`/`Done`/`Wait` and the happens-before guarantee that makes the collected results safe to read.

---

Back to [03-closures-and-retries.md](03-closures-and-retries.md) | Next: [05-batch-pagination.md](05-batch-pagination.md)
