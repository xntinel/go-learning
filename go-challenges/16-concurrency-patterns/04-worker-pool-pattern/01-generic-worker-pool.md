# Exercise 1: The Generic Worker Pool

The reusable core of every later exercise is a small generic pool: N worker
goroutines that apply a function to every job read from an input channel and
merge the results onto one output channel the caller can range over. This
exercise builds that core — `New`, `Run`, and a `Workers` accessor — and proves
with the race detector that it bounds concurrency, never duplicates or drops a
job, and closes its output exactly once.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
workerpool.go            Pool[T, U], New, Run (fan-out + WaitGroup closer), Workers
cmd/
  demo/
    main.go              square 1..20 through a 4-worker pool, print sorted results
workerpool_test.go       squares + strings, bounded-concurrency probe, no-dup/no-drop
                         race sweep, zero-worker normalisation, pre-closed input drain
```

- Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
- Implement: `Pool[T, U any]` with `New[T, U any](workers int, fn func(T) U) *Pool[T, U]`, `(*Pool[T, U]).Run(jobs <-chan T) <-chan U`, and `(*Pool[T, U]).Workers() int`.
- Test: `workerpool_test.go` checks the pool transforms every job, bounds in-flight work to the worker count, never duplicates or drops a job under a 1000-job race sweep, normalises a sub-1 worker count to 1, and drains a pre-closed empty input to a clean output close.
- Verify: `go test -race ./...`

### Why a closer goroutine, and why generics

The whole design turns on one question: how does the caller know the results are
complete? The pool fans out N workers that each loop `for j := range jobs` and
send `fn(j)` onto a shared output channel. The workers finish at different times,
so no single worker can close the output — the first one to finish would close a
channel the others are still sending on, and they would panic. The answer is a
dedicated closer goroutine. Each worker signals a `sync.WaitGroup` with
`defer wg.Done()`; one closer goroutine calls `wg.Wait()` and then `close(out)`,
exactly once, after the last worker has returned. That single close is what lets
the caller write `for v := range pool.Run(jobs)` and have the loop end on its own
when every result is in. This is the canonical fan-out/fan-in: many producers,
one merged stream, one close.

The input side has a matching rule the pool depends on but does not control: the
caller must close the input channel when it has no more jobs. The workers' range
loops only end once the input is closed and drained, so an unclosed input hangs
every worker. The pool's close of the output and the caller's close of the input
are two different closes with two different owners; keeping them straight is the
difference between a pool that drains cleanly and one that deadlocks.

The type is `Pool[T, U any]` so the dangerous part — the goroutines, the
WaitGroup, the single close — is written and race-checked once and reused for any
job shape. The same `Run` squares `int -> int`, uppercases `string -> string`,
or parses `string -> *url.URL`; only the function the caller passes changes. A
worker count below one is normalised to one rather than rejected, because a pool
with zero workers would silently drain nothing; there is no useful zero-worker
behaviour, so the constructor picks the smallest sensible value. A `nil` function
is left to panic at the first job — there is no meaningful default transform, and
the caller is expected to supply a real one.

Create `workerpool.go`:

```go
package workerpool

import "sync"

// Pool runs a fixed number of worker goroutines, each applying fn to every job
// it reads from an input channel and emitting the result on a single shared
// output channel. The zero value is not usable; construct a Pool with New.
type Pool[T, U any] struct {
	workers int
	fn      func(T) U
}

// New returns a Pool that runs workers goroutines, each applying fn to a job.
// A workers count below 1 is normalised to 1, since a zero-worker pool would
// drain nothing.
func New[T, U any](workers int, fn func(T) U) *Pool[T, U] {
	if workers < 1 {
		workers = 1
	}
	return &Pool[T, U]{workers: workers, fn: fn}
}

// Run starts the workers and returns a channel of results. The workers read from
// jobs until it is closed and drained; the caller is responsible for closing
// jobs. Run closes the returned channel once every worker has finished, so the
// caller can range over it to completion.
func (p *Pool[T, U]) Run(jobs <-chan T) <-chan U {
	out := make(chan U)
	var wg sync.WaitGroup
	wg.Add(p.workers)
	for range p.workers {
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

// Workers reports the worker count after the sub-1 normalisation applied by New.
func (p *Pool[T, U]) Workers() int {
	return p.workers
}
```

The body is deliberately small: `wg.Add(p.workers)` is called once before any
worker starts (adding inside the goroutine would race the closer's `Wait`), each
worker drains the input and decrements the group on exit, and the lone closer
goroutine waits and closes. Everything that makes the pool correct is in the
ordering of those three facts.

### The runnable demo

A demo makes the pool concrete. This one pushes the integers 1 through 20 through
a four-worker pool that squares each, then collects and sorts the results before
printing. The sort matters: results arrive in whatever order the workers finish,
which is nondeterministic, so a demo that printed them as they came could not
have a stable expected output. Sorting recovers determinism without hiding the
concurrency.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/generic-worker-pool"
)

func main() {
	jobs := make(chan int, 20)
	for i := 1; i <= 20; i++ {
		jobs <- i
	}
	close(jobs)

	pool := workerpool.New(4, func(n int) int { return n * n })
	fmt.Printf("workers: %d\n", pool.Workers())

	var results []int
	for v := range pool.Run(jobs) {
		results = append(results, v)
	}
	sort.Ints(results)
	fmt.Printf("results: %v\n", results)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers: 4
results: [1 4 9 16 25 36 49 64 81 100 121 144 169 196 225 256 289 324 361 400]
```

### Tests

The tests pin the properties the pool must have. `TestPoolSquaresAllJobs` and
`TestPoolUppercasesStrings` show the same `Run` working for two element types.
`TestPoolBoundsConcurrency` is the load-bearing one: it tracks the in-flight
worker count with atomics and asserts it never exceeds the configured size, which
is the test that fails against an unbounded goroutine-per-job implementation.
`TestPoolNoDuplicateNoDrop` runs a thousand jobs through eight workers and proves
every result appears exactly once. `TestPoolNormalisesZeroWorkers` checks the
sub-1 normalisation, and `TestPoolDrainsEmptyJobs` confirms a pre-closed empty
input produces a cleanly closed, empty output.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"sort"
	"strings"
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

	pool := New(4, func(n int) int { return n * n })

	var got []int
	for v := range pool.Run(jobs) {
		got = append(got, v)
	}
	sort.Ints(got)

	want := []int{1, 4, 9, 16, 25, 36, 49, 64, 81, 100}
	if !equalInts(got, want) {
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

	pool := New(2, strings.ToUpper)

	var got []string
	for v := range pool.Run(jobs) {
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

	const workers = 4
	var inFlight atomic.Int32
	var maxObserved atomic.Int32

	pool := New(workers, func(n int) int {
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
	for i := range 50 {
		jobs <- i
	}
	close(jobs)

	for range pool.Run(jobs) {
	}

	if got := maxObserved.Load(); got > workers {
		t.Fatalf("max in-flight = %d, want <= %d", got, workers)
	}
}

func TestPoolNoDuplicateNoDrop(t *testing.T) {
	t.Parallel()

	const n = 1000
	jobs := make(chan int, n)
	for i := range n {
		jobs <- i
	}
	close(jobs)

	pool := New(8, func(x int) int { return x * 2 })

	seen := make(map[int]bool, n)
	for v := range pool.Run(jobs) {
		if seen[v] {
			t.Fatalf("duplicate result %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique results, want %d", len(seen), n)
	}
}

func TestPoolNormalisesZeroWorkers(t *testing.T) {
	t.Parallel()

	pool := New(0, func(n int) int { return n })
	if pool.Workers() != 1 {
		t.Fatalf("Workers() = %d, want 1", pool.Workers())
	}

	jobs := make(chan int, 3)
	jobs <- 1
	jobs <- 2
	jobs <- 3
	close(jobs)

	var got []int
	for v := range pool.Run(jobs) {
		got = append(got, v)
	}
	sort.Ints(got)
	if want := []int{1, 2, 3}; !equalInts(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPoolDrainsEmptyJobs(t *testing.T) {
	t.Parallel()

	jobs := make(chan int)
	close(jobs)

	pool := New(4, func(n int) int { return n })
	count := 0
	for range pool.Run(jobs) {
		count++
	}
	if count != 0 {
		t.Fatalf("got %d results from an empty input, want 0", count)
	}
}

func equalInts(a, b []int) bool {
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

`TestPoolBoundsConcurrency` is worth reading closely. Each invocation bumps an
in-flight counter, records the running maximum with a compare-and-swap loop,
sleeps long enough that workers overlap, then decrements. If the pool truly caps
concurrency at four, the observed maximum can never exceed four; an unbounded
implementation would push it toward fifty. That single assertion is what
separates a real worker pool from a fan-out that forgot to bound itself.

## Review

The pool is correct when three facts hold together. `wg.Add(p.workers)` runs once
before the workers start, so the closer's `Wait` can never observe a count that a
not-yet-started worker will later raise; moving the `Add` inside the goroutine is
the classic bug that makes the closer race ahead and close the output early.
Only the closer goroutine calls `close(out)`, after `Wait`, so no worker ever
sends on a closed channel — the no-duplicate/no-drop sweep passing under `-race`
is the evidence. And the workers end their loops only because the caller closed
the input; the empty-input test confirms a pre-closed channel drains to a clean
output close rather than hanging.

Common mistakes for this feature. Closing the output from inside a worker panics
the moment a second worker finishes; the lone closer is mandatory. Forgetting to
close the input leaves every worker parked in its range loop forever, which looks
like a hang with no error. Adding to the WaitGroup inside the goroutine instead
of before the loop lets the closer win the race and close the output before the
last result is sent. And printing results in arrival order — as the demo
deliberately does not — produces output that changes run to run, because worker
completion order is not defined.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the bounded-parallelism example this pool generalises, with the fan-out/fan-in and WaitGroup-closer pattern.
- [Go by Example: Worker Pools](https://gobyexample.com/worker-pools) — a minimal worker-pool walkthrough over jobs and results channels.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the exact `Add`/`Done`/`Wait` contract the closer relies on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-job-processing-service.md](02-job-processing-service.md)
