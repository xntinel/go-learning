# 2. Fan-Out Pattern: Multiple Workers On One Channel

Fan-out is the practice of starting multiple goroutines that read from the same
inbound channel until that channel is closed. The Go blog calls it "a way to
distribute work amongst a group of workers to parallelize CPU use and I/O". A
single producer feeds N workers; the workers compute independently; their
outputs are usually merged back into one channel by fan-in.

```text
fanout/
  go.mod
  internal/fanout/fanout.go
  internal/fanout/fanout_test.go
  cmd/fanoutdemo/main.go
```

The package exposes `Workers` that fan out a `[]int` job stream across N
goroutines and emit squared results on a single output channel. Each worker is
identically shaped so the dispatcher and the collector are decoupled.

## Concepts

### Fan-Out Is Sharing One Inbound Channel

Multiple goroutines reading from the same channel is fan-out. The Go runtime
hands each value to exactly one receiver, so the work is partitioned by arrival
order, not by content. The values themselves are immutable from the workers'
point of view.

### Workers Run The Same Function

In Ajmani's pipeline article, fan-out is described as "a group of goroutines
running the same function". The dispatcher does not know about the workers'
internals; it only spawns them and feeds them. This is what makes the pattern
composable: any stage in lesson 01 can be fanned out by simply starting more
copies of it.

### Results Must Be Merged

Two workers writing to two separate output channels means the consumer has to
read from both. That is fragile and racy. The standard fix is fan-in: a single
output channel that every worker writes to, closed by a `WaitGroup` once
every worker has finished. Lesson 03 covers fan-in in detail; this lesson's
`Workers` returns the merged channel directly.

### Order Is Not Preserved

Two workers racing on the same input channel will finish in arrival order,
which is not source order. If the consumer needs source order, fan-out is the
wrong tool. If the consumer only needs every result exactly once, fan-out is
correct.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/02-fan-out-pattern/02-fan-out-pattern/internal/fanout go-solutions/16-concurrency-patterns/02-fan-out-pattern/02-fan-out-pattern/cmd/fanoutdemo
cd go-solutions/16-concurrency-patterns/02-fan-out-pattern/02-fan-out-pattern
```

### Exercise 1: The Worker Function

A worker takes one inbound channel, runs a transformation, and writes to a
shared outbound channel. It does not close the outbound channel — that is the
fan-in coordinator's job (lesson 03).

Create `internal/fanout/fanout.go`:

```go
package fanout

import "sync"

func worker(jobs <-chan int, out chan<- int, wg *sync.WaitGroup) {
	defer wg.Done()
	for n := range jobs {
		out <- n * n
	}
}

func Workers(numW int, jobs <-chan int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup
	wg.Add(numW)
	for i := 0; i < numW; i++ {
		go worker(jobs, out, &wg)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

`Workers` returns a read-only channel that closes once every worker is done.
The two goroutines that matter are the workers and the closer. There is no
need for a `done` channel because the producer owns `jobs` and closes it when
the input is exhausted; that close cascades through `range jobs`.

### Exercise 2: A Real Job Generator

Jobs are produced by a separate goroutine that closes the channel when done:

```go
package fanout

func GenerateJobs(done <-chan struct{}, nums ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, n := range nums {
			select {
			case out <- n:
			case <-done:
				return
			}
		}
	}()
	return out
}
```

This is the same `Generate` from lesson 01, kept here so the package is
self-contained for the test.

### Exercise 3: Test The Contract

Create `internal/fanout/fanout_test.go`:

```go
package fanout

import (
	"sort"
	"sync"
	"testing"
)

func TestWorkersSquareEveryInput(t *testing.T) {
	t.Parallel()

	jobs := GenerateJobs(neverClose(), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	out := Workers(4, jobs)

	var got []int
	for v := range out {
		got = append(got, v)
	}

	want := []int{1, 4, 9, 16, 25, 36, 49, 64, 81, 100}
	sort.Ints(got)
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWorkersHandlesZeroWorkers(t *testing.T) {
	t.Parallel()

	jobs := GenerateJobs(neverClose(), 1, 2, 3)
	out := Workers(0, jobs)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want []", got)
	}
}

func TestWorkersOneWorkerIsSequential(t *testing.T) {
	t.Parallel()

	jobs := GenerateJobs(neverClose(), 5, 6, 7, 8)
	out := Workers(1, jobs)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	want := []int{25, 36, 49, 64}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWorkersIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 1000
	nums := make([]int, n)
	for i := range nums {
		nums[i] = i
	}
	jobs := GenerateJobs(neverClose(), nums...)
	out := Workers(8, jobs)

	seen := make(map[int]bool, n)
	for v := range out {
		if seen[v] {
			t.Fatalf("duplicate %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique values, want %d", len(seen), n)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// neverClose returns a channel that is never closed, so the producer can run
// without the test having to plumb a done signal through every test.
func neverClose() <-chan struct{} { return make(chan struct{}) }

// guard against "forgot WaitGroup.Add before go" regressions.
func TestWorkersClosesAfterAllWorkers(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	wg.Add(1)
	jobs := make(chan int)
	go func() {
		defer wg.Done()
		defer close(jobs)
		for i := 0; i < 100; i++ {
			jobs <- i
		}
	}()

	out := Workers(3, jobs)
	count := 0
	for range out {
		count++
	}
	wg.Wait()
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
}
```

`TestWorkersIsRaceFree` is the main correctness test: every input must appear
exactly once on the output channel, regardless of how the runtime scheduled the
workers. The race detector finds any unsynchronised access to shared state.

Your turn: add `TestWorkersEvenCount` that fans out 4 workers across
`GenerateJobs(neverClose(), 1..100)` and asserts the result has 100 entries
whose sorted sum is `338350`.

### Exercise 4: Runnable Demo

Create `cmd/fanoutdemo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fanout/internal/fanout"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	jobs := fanout.GenerateJobs(done, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	out := fanout.Workers(4, jobs)
	for v := range out {
		fmt.Println(v)
	}
}
```

## Common Mistakes

### Closing The Shared Output Channel From A Worker

Wrong: `defer close(out)` inside `worker`.

What happens: the first worker to finish closes `out`; the next worker's send
panics with `send on closed channel`.

Fix: only the dispatcher closes, after `wg.Wait`. `wg.Add` must run before any
`go worker`, otherwise `wg.Wait` returns zero and `out` is closed before the
workers have started.

### Writing To The Output Channel Without Coordinating Close

Wrong: returning the channel from `Workers` without the closer goroutine.

What happens: the consumer's `for v := range out` hangs forever.

Fix: spawn `go func() { wg.Wait(); close(out) }()`. This is the canonical
recipe from the Go blog.

### Adding Buffers "Just In Case"

Wrong: `out := make(chan int, 1024)` to mask slow consumers.

What happens: a slow consumer plus a fast producer fills the buffer; new
workers block on `out <- v` and the parallelism you wanted disappears.

Fix: size the buffer based on the actual backlog you are willing to hold, and
back-pressure upstream with `done` if necessary.

## Verification

From `~/go-exercises/fanout`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is non-negotiable for any lesson that
shares a channel between goroutines.

## Summary

- Fan-out is N goroutines reading from one inbound channel.
- Workers run the same function; the dispatcher is a `for i := 0; i < n; i++ { go worker }`.
- Outputs are merged into a single channel that closes after `wg.Wait`.
- Order is not preserved; results are deduplicated by content, not position.
- Always pair the lesson with `go test -race`.

## What's Next

Next: [Fan-In Pattern](../03-fan-in-pattern/03-fan-in-pattern.md).

## Resources

- [Go Blog: Pipelines and cancellation - Fan-out, fan-in](https://go.dev/blog/pipelines)
- [Go talks: Advanced Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)