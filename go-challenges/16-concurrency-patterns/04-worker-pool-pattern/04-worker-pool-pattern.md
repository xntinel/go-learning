# 4. Worker Pool Pattern: Bounded Concurrency Over A Job Stream

A worker pool runs a fixed number of goroutines that pull jobs from a shared
channel and emit results. It bounds memory and OS resource use, which makes
it the safe default for "process N items concurrently" code. The Go blog's
"bounded parallelism" example uses exactly this shape: a fixed-size group of
digester goroutines reading from a `paths` channel.

```text
workerpool/
  go.mod
  internal/workerpool/workerpool.go
  internal/workerpool/workerpool_test.go
  cmd/workerpooldemo/main.go
```

The package exposes a `Pool` that fans out N workers, runs a user-supplied
function on every job, and returns a single result channel. The pool accepts
any `func(T) U` so the same machinery squares integers, uppercases strings, or
parses URLs.

## Concepts

### Bounded Parallelism Is The Default

The unbounded version (one goroutine per job) is a footgun: a million-job
queue opens a million goroutines and the runtime runs out of stack and heap.
A pool of, say, 16 workers serves the same throughput for CPU-bound work and
keeps memory bounded.

### The Pool's Job Type Is Generic

Since Go 1.18, the natural way to write a pool is `Pool[T, U any]`. The
worker function is `func(T) U`. The pool owns the dispatcher, the workers, and
the closer; the caller owns the job channel and the result channel.

### Results Have To Be Merged Into One Channel

Each worker writes to a shared output channel. The pool closes the channel
once every worker is done, using `sync.WaitGroup` exactly as in lesson 03.
This is what makes the result channel composable with downstream stages.

### The Job Channel Must Be Closed

The pool reads `for j := range jobs`. If the caller never closes `jobs`, the
pool hangs. The pattern is: the producer closes its channel when it has no
more jobs to send. The pool's closer is independent.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/workerpool/internal/workerpool ~/go-exercises/workerpool/cmd/workerpooldemo
cd ~/go-exercises/workerpool
go mod init example.com/workerpool
```

### Exercise 1: The Pool Type

Create `internal/workerpool/workerpool.go`:

```go
package workerpool

import "sync"

type Pool[T, U any] struct {
	workers int
	fn      func(T) U
}

func New[T, U any](workers int, fn func(T) U) *Pool[T, U] {
	if workers < 1 {
		workers = 1
	}
	return &Pool[T, U]{workers: workers, fn: fn}
}

func (p *Pool[T, U]) Run(jobs <-chan T) <-chan U {
	out := make(chan U)
	var wg sync.WaitGroup
	wg.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				out <- p.fn(j)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (p *Pool[T, U]) Workers() int {
	return p.workers
}
```

The zero-worker case is normalised to one. A `nil` `fn` would panic at the
first job; the constructor does not guard against that, because there is no
sensible default and the caller should supply a real function.

### Exercise 2: Test The Contract

Create `internal/workerpool/workerpool_test.go`:

```go
package workerpool

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolSquaresAllJobs(t *testing.T) {
	t.Parallel()

	jobs := make(chan int, 10)
	for i := 1; i <= 10; i++ {
		jobs <- i
	}
	close(jobs)

	p := New[int, int](4, func(n int) int { return n * n })
	out := p.Run(jobs)

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

func TestPoolUppercasesStrings(t *testing.T) {
	t.Parallel()

	jobs := make(chan string, 3)
	jobs <- "go"
	jobs <- "pipelines"
	jobs <- "fan-in"
	close(jobs)

	p := New[string, string](2, strings.ToUpper)
	out := p.Run(jobs)

	var got []string
	for v := range out {
		got = append(got, v)
	}
	sort.Strings(got)
	want := []string{"FAN-IN", "GO", "PIPELINES"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPoolBoundsConcurrency(t *testing.T) {
	t.Parallel()

	var inFlight atomic.Int32
	var maxObserved atomic.Int32

	p := New[int, int](4, func(n int) int {
		cur := inFlight.Add(1)
		for {
			prev := maxObserved.Load()
			if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return n
	})

	jobs := make(chan int, 50)
	for i := 0; i < 50; i++ {
		jobs <- i
	}
	close(jobs)

	out := p.Run(jobs)
	for range out {
	}

	if got := maxObserved.Load(); got > 4 {
		t.Fatalf("max in-flight = %d, want <= 4", got)
	}
}

func TestPoolHandlesZeroWorkers(t *testing.T) {
	t.Parallel()

	jobs := make(chan int, 3)
	jobs <- 1
	jobs <- 2
	jobs <- 3
	close(jobs)

	p := New[int, int](0, func(n int) int { return n })
	if p.Workers() != 1 {
		t.Fatalf("Workers() = %d, want 1", p.Workers())
	}
	out := p.Run(jobs)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	sort.Ints(got)
	want := []int{1, 2, 3}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPoolIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 1000
	jobs := make(chan int, n)
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)

	p := New[int, int](8, func(n int) int { return n * 2 })
	out := p.Run(jobs)

	seen := make(map[int]bool, n)
	for v := range out {
		if seen[v] {
			t.Fatalf("duplicate %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique, want %d", len(seen), n)
	}
}

func TestPoolClosesAfterAllWorkers(t *testing.T) {
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

	p := New[int, int](3, func(n int) int { return n })
	out := p.Run(jobs)
	count := 0
	for range out {
		count++
	}
	wg.Wait()
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
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

func equalStrings(a, b []string) bool {
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
```

`TestPoolBoundsConcurrency` proves the pool actually bounds work. It tracks
the in-flight count and asserts it never exceeds the configured worker count.
This is the test that catches an unbounded implementation.

Your turn: add `TestPoolDrainsEmptyJobs` that runs a pool with a pre-closed
`jobs` channel containing zero items and asserts the output channel closes
without producing any values.

### Exercise 3: Runnable Demo

Create `cmd/workerpooldemo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/workerpool/internal/workerpool"
)

func main() {
	jobs := make(chan int, 20)
	for i := 1; i <= 20; i++ {
		jobs <- i
	}
	close(jobs)

	p := workerpool.New[int, int](4, func(n int) int { return n * n })
	for v := range p.Run(jobs) {
		fmt.Println(v)
	}
}
```

## Common Mistakes

### One Goroutine Per Job

Wrong: spawning `go handleJob(job)` for every job.

What happens: a million jobs allocate a million goroutines; the runtime
starves and the program OOMs.

Fix: a pool with `min(N, runtime.NumCPU())` workers, or `runtime.GOMAXPROCS`.

### Closing The Output Channel From A Worker

Wrong: `defer close(out)` inside the worker goroutine.

What happens: first worker to finish closes the channel; later workers panic.

Fix: only the closer goroutine calls `close`, after `wg.Wait`.

### Forgetting To Close The Job Channel

Wrong: the producer never closes `jobs`.

What happens: the pool's `for j := range jobs` hangs forever.

Fix: the producer closes `jobs` after the last send. This is independent of
the pool's closer.

### Returning A Pool Without Starting It

Wrong: returning a `*Pool[T, U]` and asking the caller to "start it manually".

What happens: the caller forgets, or starts it twice.

Fix: `Run(jobs)` is the only entry point that starts goroutines. The pool
itself is just data.

## Verification

From `~/go-exercises/workerpool`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is the test that catches missing
WaitGroup.Add or premature close.

## Summary

- A worker pool runs a fixed number of goroutines over a shared job channel.
- The job channel must be closed by the producer for the pool to drain.
- The pool merges results into one channel that closes after `wg.Wait`.
- Bounded concurrency is the safe default; unbounded goroutine-per-job is a
  footgun.
- Generics let one `Pool[T, U]` work for `int -> int`, `string -> string`, or
  any other shape.

## What's Next

Next: [Generator Pattern](../05-generator-pattern/05-generator-pattern.md).

## Resources

- [Go Blog: Bounded parallelism](https://go.dev/blog/pipelines)
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)