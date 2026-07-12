# Exercise 5: make vs new: a Bounded Worker Dispatcher

`new` and `make` look interchangeable and are not. `make(chan Job, n)` gives a
usable buffered channel; `new(chan Job)` gives a pointer to a nil channel that
blocks forever. This exercise builds a bounded worker dispatcher backed by
`make(chan Job, size)` and `make(map[ID]Result)`, and demonstrates — safely, with
a guarded helper — why the `new` forms are the wrong tool.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
workerqueue/                  independent module: example.com/workerqueue
  go.mod                      go 1.26
  dispatcher.go               Dispatcher (make(chan)+make(map)), Submit, Run, Wait, nilChannelBlocks helper
  cmd/
    demo/
      main.go                 runnable demo: N jobs across W workers, collect results
  dispatcher_test.go          N-results test + nil-channel-blocks proof + no-goroutine-leak test
```

Files: `dispatcher.go`, `cmd/demo/main.go`, `dispatcher_test.go`.
Implement: a `Dispatcher` built with `make(chan Job, size)` and
`make(map[ID]Result)`, with `Submit`/`Run`/`Wait` and a fixed worker count;
a guarded `nilChannelBlocks` helper documenting the `new(chan)` trap.
Test: N jobs across W workers produce N results; the nil-channel helper proves a
`new(chan)` send is never ready; closing the channel drains `Wait` cleanly with no
goroutine leak (count before/after).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/03-new-vs-composite-literal/05-make-vs-new-worker-queue/cmd/demo
cd go-solutions/09-pointers/03-new-vs-composite-literal/05-make-vs-new-worker-queue
```

### Why make, not new

A dispatcher needs three things that are usable, not zero: a buffered channel to
queue jobs, a map to collect results, and a `WaitGroup` to join the workers. For
the channel and the map, `new` is the wrong constructor. `new(chan Job)` returns a
`*chan Job` pointing at a nil channel; a send or receive on a nil channel blocks
forever, so a dispatcher built that way would deadlock the moment a worker tried
to read. `new(map[ID]Result)` returns a `*map` pointing at a nil map; writing a
result through it panics with "assignment to entry in nil map." `make` is the
constructor that returns an initialized, usable value: `make(chan Job, size)` is a
buffered channel ready to send and receive, and `make(map[ID]Result)` is an empty
map ready to write. The `Dispatcher` struct itself is fine with `new`/`&Dispatcher{}`
because it is a struct — the trap is specifically its channel and map fields,
which must be built with `make`.

The worker model is the standard fan-out: `Run` starts `workers` goroutines, each
looping with `for job := range d.jobs` (which drains until the channel is closed),
computing a result, and storing it under a mutex. `Submit` sends onto the buffered
channel. `Wait` closes the channel — signalling the workers to finish the drain
and exit — and then `WaitGroup.Wait`s for them, so when `Wait` returns every
worker goroutine has exited. That clean shutdown is what the no-leak test checks:
the goroutine count after `Wait` returns to its baseline.

To make the nil-channel trap concrete without deadlocking the test, the helper
`nilChannelBlocks` uses a `select` with a `default`: a send on a nil channel is
never ready, so the `default` is always taken, which is exactly the observable
symptom of "would block forever."

Create `dispatcher.go`:

```go
package workerqueue

import "sync"

// ID identifies a job.
type ID int

// Job is a unit of work: square the input value.
type Job struct {
	ID    ID
	Value int
}

// Result is the outcome of a Job.
type Result struct {
	ID     ID
	Square int
}

// Dispatcher fans jobs out to a fixed pool of workers. The jobs channel and the
// results map MUST be built with make, not new: new(chan)/new(map) yield a nil
// channel (blocks forever) and a nil map (panics on write).
type Dispatcher struct {
	jobs    chan Job
	workers int

	mu      sync.Mutex
	results map[ID]Result
	wg      sync.WaitGroup
}

// NewDispatcher builds a Dispatcher with a buffered job channel and an
// initialized results map. size buffers the channel; workers sets the pool size.
func NewDispatcher(size, workers int) *Dispatcher {
	return &Dispatcher{
		jobs:    make(chan Job, size), // make: usable buffered channel
		workers: workers,
		results: make(map[ID]Result), // make: usable empty map
	}
}

// Run starts the worker goroutines. Each drains the jobs channel until it closes.
func (d *Dispatcher) Run() {
	for range d.workers {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for job := range d.jobs {
				r := Result{ID: job.ID, Square: job.Value * job.Value}
				d.mu.Lock()
				d.results[job.ID] = r
				d.mu.Unlock()
			}
		}()
	}
}

// Submit enqueues a job.
func (d *Dispatcher) Submit(j Job) {
	d.jobs <- j
}

// Wait closes the job channel and blocks until every worker has drained and
// exited. After Wait returns, no dispatcher goroutine is still running.
func (d *Dispatcher) Wait() map[ID]Result {
	close(d.jobs)
	d.wg.Wait()
	return d.results
}

// nilChannelBlocks demonstrates the new(chan) trap without deadlocking: a send on
// the nil channel from new(chan int) is never ready, so the select default fires.
// It returns true, meaning "the send would block forever."
func nilChannelBlocks() bool {
	p := new(chan int) // pointer to a NIL channel
	ch := *p
	select {
	case ch <- 1:
		return false // unreachable: a nil channel is never ready
	default:
		return true
	}
}
```

### The runnable demo

The demo submits ten jobs across four workers, waits, and prints the collected
results in ID order so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/workerqueue"
)

func main() {
	d := workerqueue.NewDispatcher(4, 4)
	d.Run()

	const n = 10
	for i := range n {
		d.Submit(workerqueue.Job{ID: workerqueue.ID(i), Value: i})
	}
	results := d.Wait()

	ids := make([]int, 0, len(results))
	for id := range results {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)

	fmt.Printf("collected %d results\n", len(results))
	for _, id := range ids {
		r := results[workerqueue.ID(id)]
		fmt.Printf("job %d -> %d\n", r.ID, r.Square)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
collected 10 results
job 0 -> 0
job 1 -> 1
job 2 -> 4
job 3 -> 9
job 4 -> 16
job 5 -> 25
job 6 -> 36
job 7 -> 49
job 8 -> 64
job 9 -> 81
```

### Tests

`TestAllJobsProduceResults` submits N jobs across W workers and asserts N correct
results. `TestNilChannelBlocks` pins the trap: the `new(chan)` send is never
ready. `TestNoGoroutineLeak` captures the goroutine count before and after a full
Run/Submit/Wait cycle and asserts it returns to baseline, proving `Wait` shuts the
workers down cleanly.

Create `dispatcher_test.go`:

```go
package workerqueue

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestAllJobsProduceResults(t *testing.T) {
	t.Parallel()

	const n, workers = 100, 8
	d := NewDispatcher(16, workers)
	d.Run()
	for i := range n {
		d.Submit(Job{ID: ID(i), Value: i})
	}
	results := d.Wait()

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for i := range n {
		r, ok := results[ID(i)]
		if !ok {
			t.Fatalf("missing result for job %d", i)
		}
		if r.Square != i*i {
			t.Fatalf("job %d square = %d, want %d", i, r.Square, i*i)
		}
	}
}

func TestNilChannelBlocks(t *testing.T) {
	t.Parallel()

	if !nilChannelBlocks() {
		t.Fatal("new(chan) send should never be ready; a nil channel blocks forever")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	// Not parallel: it reads the process-wide goroutine count.
	before := runtime.NumGoroutine()

	d := NewDispatcher(4, 4)
	d.Run()
	for i := range 20 {
		d.Submit(Job{ID: ID(i), Value: i})
	}
	d.Wait()

	// After Wait, workers have exited. Poll briefly to let the scheduler settle.
	var after int
	for range 100 {
		after = runtime.NumGoroutine()
		if after <= before {
			return
		}
		time.Sleep(time.Millisecond)
		runtime.Gosched()
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, after)
}

// ExampleDispatcher shows a full submit/collect cycle. Reading results back in ID
// order keeps the output deterministic despite the concurrent workers.
func ExampleDispatcher() {
	d := NewDispatcher(4, 2)
	d.Run()
	for i := range 4 {
		d.Submit(Job{ID: ID(i), Value: i})
	}
	results := d.Wait()

	for i := range 4 {
		r := results[ID(i)]
		fmt.Printf("job %d -> %d\n", r.ID, r.Square)
	}
	// Output:
	// job 0 -> 0
	// job 1 -> 1
	// job 2 -> 4
	// job 3 -> 9
}
```

## Review

The dispatcher is correct when N submitted jobs yield N correct results and when
`Wait` returns only after every worker goroutine has exited — the no-leak test is
the proof of the latter, and it passes only because `Wait` closes the channel
(ending each worker's `range`) before joining the `WaitGroup`. The lesson's spine
is the `make` calls in `NewDispatcher`: swap either for `new` and the dispatcher
breaks in a characteristic way — `new(chan Job)` deadlocks every worker on a nil
channel, `new(map[ID]Result)` panics on the first result write. `TestNilChannelBlocks`
captures the channel half of that without hanging the suite. The goroutine-count
test is deliberately not parallel and polls briefly, because the count is a
process-wide, slightly racy signal; the poll tolerates scheduler lag while still
catching a genuine leak.

## Resources

- [Effective Go: Allocation with new and make](https://go.dev/doc/effective_go#allocation_new) — the exact distinction between the two.
- [Go Specification: Making slices, maps and channels](https://go.dev/ref/spec#Making_slices_maps_and_channels) — what `make` returns and why.
- [Dave Cheney: Channel axioms](https://dave.cheney.net/2014/03/19/channel-axioms) — a send/receive on a nil channel blocks forever.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-nested-composite-route-table.md](06-nested-composite-route-table.md)
