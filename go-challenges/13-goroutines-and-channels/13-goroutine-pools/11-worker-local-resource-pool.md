# Exercise 11: A Pool Where Each Worker Owns A Reusable Resource

**Level: Intermediate**

A log-shipping sidecar compresses batches of records and forwards them to an ingest
endpoint. Allocating a fresh gzip writer and a large scratch buffer for every batch
churns the heap and hammers the GC under load. The naive fix — a global pool guarded
by a mutex — trades allocation for contention. This exercise builds the pattern that
does neither: a fixed worker pool where each worker constructs its expensive resource
once at startup and reuses it across every job it runs, so the resource is never
shared and never re-allocated, while the worker count still bounds concurrency to
protect the downstream.

This module is self-contained: its own module, a `respool` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
respool/                     independent module: example.com/respool
  go.mod                     go 1.26
  respool.go                 generic Pool[R]: New, Submit, Size, Close
  cmd/demo/main.go           runnable demo: gzip-ship batches on worker-local writers
  respool_test.go            amortization, ownership, concurrency-bound, drain tests
```

- Files: `respool.go`, `cmd/demo/main.go`, `respool_test.go`.
- Implement: `New[R any](workers int, newResource func() R) *Pool[R]`, `Submit(job func(R) error) bool`, `Size() int`, `Close()`.
- Test: `newResource` runs exactly `workers` times regardless of job count; every job runs with a non-nil resource; no resource is held by two jobs at once; global concurrency never exceeds `workers`; `Submit` returns false after `Close`; `Close` blocks until every queued job finishes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/13-goroutine-pools/11-worker-local-resource-pool/cmd/demo
cd go-solutions/13-goroutines-and-channels/13-goroutine-pools/11-worker-local-resource-pool
```

### Amortization is the pool value beyond fan-out capping

Most pool exercises justify the pool by one property: it caps concurrency so a burst
of work does not become a burst of simultaneous operations that knock over a
downstream. That is real, and this pool keeps it — `workers` goroutines is a hard
ceiling on how many jobs run at once. But a fixed, long-lived pool buys a second
property that a spawn-per-job design cannot: it amortizes expensive per-worker state.

The resource here is a gzip writer plus a reusable scratch buffer. Constructing one
allocates internal Huffman tables and a window buffer; doing that per batch is pure
garbage. The pool's move is to construct it once, inside each worker goroutine, and
reuse it for every job that worker ever runs. With `workers` workers and a million
jobs, `newResource` is called `workers` times, not a million. The reuse is safe
without any lock because of one invariant:

1. Each worker calls `newResource()` exactly once, immediately after it starts.
2. That value is a local variable in the worker goroutine — no other goroutine has a
   reference to it.
3. The worker runs jobs one at a time, in a `for range` over the shared queue, handing
   its own resource into each job.

Because the resource is worker-local and the worker is single-threaded through its
loop, a resource is never touched by two jobs at the same instant. There is no shared
mutable state to race on, so there is nothing to lock. Contrast the tempting
alternative — one `sync.Pool` or one mutex-guarded writer shared by every job — which
reintroduces exactly the contention the worker-local design removes. The failure mode
this prevents is the subtle one: a shared writer whose `Reset` from job A races the
`Write` from job B, corrupting the compressed stream with no panic and no error, just
malformed bytes arriving at the ingest endpoint.

The channel-ownership rules still apply. `Submit` sends on the job channel under a
mutex that also guards a `closed` flag, so a send can never race a `close`; `Close`
sets the flag, closes the channel exactly once (idempotent on a second call), and then
`wg.Wait()`s so it blocks until every worker has drained the queue and returned.

Create `respool.go`:

```go
package respool

import "sync"

// queueDepth is the bounded backlog behind the workers. Jobs wait here when every
// worker is busy; Submit blocks (backpressure) once it is full.
const queueDepth = 1024

// Pool is a fixed-size worker pool where each worker constructs one resource of
// type R at startup and reuses it for every job it runs. The worker count bounds
// concurrency; the per-worker resource amortizes an expensive per-job allocation
// (a gzip writer, a scratch buffer) down to one construction per worker.
type Pool[R any] struct {
	workers int
	jobs    chan func(R) error
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

// New starts workers goroutines. Each worker calls newResource exactly once and
// reuses that value for every job it runs, so newResource runs workers times in
// total no matter how many jobs are submitted. The resource is worker-local: it is
// never shared with another worker, so a job never needs to lock it.
func New[R any](workers int, newResource func() R) *Pool[R] {
	if workers < 1 {
		workers = 1
	}
	p := &Pool[R]{
		workers: workers,
		jobs:    make(chan func(R) error, queueDepth),
	}
	for range workers {
		p.wg.Go(func() {
			res := newResource() // constructed once per worker, then reused below
			for job := range p.jobs {
				_ = job(res)
			}
		})
	}
	return p
}

// Submit enqueues a job that will run on some worker's owned resource. It returns
// false once the pool is closed; otherwise it blocks if the backlog is full.
func (p *Pool[R]) Submit(job func(R) error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job
	return true
}

// Size reports the fixed worker count.
func (p *Pool[R]) Size() int { return p.workers }

// Close stops accepting work and blocks until every in-flight and queued job has
// finished. It is idempotent: a second call is a no-op, never a double-close panic.
func (p *Pool[R]) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.jobs)
	p.mu.Unlock()
	p.wg.Wait()
}
```

`p.wg.Go` (Go 1.25+) launches the worker and pairs the `Add`/`Done` internally, so
the WaitGroup can never under-count from an `Add` that races `Wait`.

### The runnable demo

The demo is the sidecar in miniature: four workers, each owning a gzip writer over a
reusable buffer, compress twelve batches of five records each. Every batch writes its
compressed size into its own slice slot, so the aggregate is deterministic no matter
which worker ran which batch. It prints that `newResource` ran exactly four times
(one per worker) even though twelve batches were shipped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/respool"
)

// shipper is a worker-local resource: one gzip writer over one reusable scratch
// buffer. Constructing it is the expensive step we want to pay once per worker,
// not once per batch.
type shipper struct {
	id  int
	buf bytes.Buffer
	gz  *gzip.Writer
}

// compress gzips one batch of records into the reused buffer and reports the
// compressed size. Reset lets the same writer and buffer serve every batch.
func (s *shipper) compress(records []string) int {
	s.buf.Reset()
	s.gz.Reset(&s.buf)
	for _, r := range records {
		_, _ = s.gz.Write([]byte(r))
	}
	_ = s.gz.Close()
	return s.buf.Len()
}

func main() {
	const workers = 4
	const batches = 12
	const recordsPerBatch = 5

	var built atomic.Int64
	pool := respool.New(workers, func() *shipper {
		id := int(built.Add(1))
		s := &shipper{id: id}
		s.gz = gzip.NewWriter(&s.buf)
		return s
	})

	// Each batch writes its compressed size into its own slot: no shared mutation,
	// so the aggregate below is deterministic regardless of which worker ran it.
	sizes := make([]int, batches)
	var wg sync.WaitGroup
	for b := range batches {
		wg.Add(1)
		records := make([]string, recordsPerBatch)
		for i := range records {
			records[i] = fmt.Sprintf("evt-batch%02d-rec%d\n", b, i)
		}
		pool.Submit(func(s *shipper) error {
			defer wg.Done()
			sizes[b] = s.compress(records)
			return nil
		})
	}
	wg.Wait()
	pool.Close()

	total := 0
	for _, n := range sizes {
		total += n
	}
	fmt.Printf("workers=%d\n", pool.Size())
	fmt.Printf("resources constructed=%d\n", built.Load())
	fmt.Printf("batches shipped=%d\n", batches)
	fmt.Printf("records forwarded=%d\n", batches*recordsPerBatch)
	fmt.Printf("compressed bytes=%d\n", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=4
resources constructed=4
batches shipped=12
records forwarded=60
compressed bytes=648
```

### Tests

`TestResourceConstructedOncePerWorker` submits 500 jobs to a 4-worker pool and asserts
`newResource` ran exactly 4 times — the amortization proof, not one resource per job.
`TestEveryJobRunsWithNonNilResource` asserts all submitted jobs execute and none see a
nil resource. `TestResourceNeverSharedConcurrently` instruments each resource with a
per-resource peak in-use counter and, under a barrier that forces every worker to hold
a job simultaneously, asserts each resource's peak stays at 1 — a shared resource would
push some peak to 2. `TestGlobalConcurrencyBounded` tracks an atomic peak of
concurrently running jobs and asserts it never exceeds `workers` and does reach
`workers`. `TestSubmitFalseAfterClose` asserts `Submit` returns false after `Close` and
that a second `Close` does not panic. `TestCloseDrainsQueuedJobs` submits 400 jobs with
no external WaitGroup and asserts that after `Close` returns, all 400 have finished —
the drain contract.

Create `respool_test.go`:

```go
package respool

import (
	"sync"
	"sync/atomic"
	"testing"
)

// resource is an instrumented worker-local resource. inUse flips 0->1 while a job
// holds it and back; peakInUse records the highest value ever seen for THIS
// resource, so a value above 1 proves two jobs touched the same resource at once.
type resource struct {
	id        int
	inUse     atomic.Int32
	peakInUse atomic.Int32
}

func (r *resource) enter() {
	n := r.inUse.Add(1)
	for {
		peak := r.peakInUse.Load()
		if n <= peak || r.peakInUse.CompareAndSwap(peak, n) {
			break
		}
	}
}

func (r *resource) leave() { r.inUse.Add(-1) }

// TestResourceConstructedOncePerWorker pins amortization: newResource runs exactly
// `workers` times no matter how many jobs are submitted.
func TestResourceConstructedOncePerWorker(t *testing.T) {
	t.Parallel()

	const workers = 4
	const jobs = 500
	var built atomic.Int64
	p := New(workers, func() *resource {
		id := int(built.Add(1))
		return &resource{id: id}
	})

	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		p.Submit(func(r *resource) error {
			defer wg.Done()
			return nil
		})
	}
	wg.Wait()
	p.Close()

	if got := built.Load(); got != workers {
		t.Fatalf("newResource called %d times, want exactly %d (one per worker)", got, workers)
	}
}

// TestEveryJobRunsWithNonNilResource pins that all submitted jobs execute and each
// receives a live resource.
func TestEveryJobRunsWithNonNilResource(t *testing.T) {
	t.Parallel()

	const workers = 3
	const jobs = 300
	p := New(workers, func() *resource { return &resource{} })

	var executed atomic.Int64
	var nilSeen atomic.Int64
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		ok := p.Submit(func(r *resource) error {
			defer wg.Done()
			if r == nil {
				nilSeen.Add(1)
			}
			executed.Add(1)
			return nil
		})
		if !ok {
			t.Fatal("Submit returned false before Close")
		}
	}
	wg.Wait()
	p.Close()

	if got := executed.Load(); got != jobs {
		t.Fatalf("executed %d jobs, want %d", got, jobs)
	}
	if got := nilSeen.Load(); got != 0 {
		t.Fatalf("%d jobs saw a nil resource, want 0", got)
	}
}

// TestResourceNeverSharedConcurrently pins the ownership invariant: no resource is
// ever held by two jobs at the same instant. Each resource records its own peak
// in-use count; every peak must stay at 1. A shared-resource bug would push some
// peak to 2 under the barrier below.
func TestResourceNeverSharedConcurrently(t *testing.T) {
	t.Parallel()

	const workers = 8
	const jobs = 800 // stays under queueDepth so Submit never blocks while workers park at the barrier
	var mu sync.Mutex
	var all []*resource
	p := New(workers, func() *resource {
		r := &resource{}
		mu.Lock()
		all = append(all, r)
		mu.Unlock()
		return r
	})

	// A barrier makes all workers run a job simultaneously at least once, so if a
	// resource were shared the concurrent-hold would actually happen.
	var ready sync.WaitGroup
	ready.Add(workers)
	release := make(chan struct{})
	var barrierOnce atomic.Int64
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		p.Submit(func(r *resource) error {
			defer wg.Done()
			r.enter()
			if barrierOnce.Add(1) <= workers {
				ready.Done()
				<-release
			}
			r.leave()
			return nil
		})
	}
	ready.Wait()
	close(release)
	wg.Wait()
	p.Close()

	for _, r := range all {
		if peak := r.peakInUse.Load(); peak != 1 {
			t.Fatalf("resource peak in-use = %d, want 1 (resource shared across jobs)", peak)
		}
	}
	if len(all) != workers {
		t.Fatalf("constructed %d resources, want %d", len(all), workers)
	}
}

// TestGlobalConcurrencyBounded pins that at most `workers` jobs ever run at once,
// and that the pool genuinely reaches that ceiling under a barrier.
func TestGlobalConcurrencyBounded(t *testing.T) {
	t.Parallel()

	const workers = 6
	const jobs = 800 // stays under queueDepth so Submit never blocks while workers park at the barrier
	p := New(workers, func() *resource { return &resource{} })

	var active atomic.Int64
	var peak atomic.Int64
	var ready sync.WaitGroup
	ready.Add(workers)
	release := make(chan struct{})
	var barrier atomic.Int64
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		p.Submit(func(r *resource) error {
			defer wg.Done()
			n := active.Add(1)
			for {
				pk := peak.Load()
				if n <= pk || peak.CompareAndSwap(pk, n) {
					break
				}
			}
			if barrier.Add(1) <= workers {
				ready.Done()
				<-release
			}
			active.Add(-1)
			return nil
		})
	}
	ready.Wait()
	close(release)
	wg.Wait()
	p.Close()

	if got := peak.Load(); got > workers {
		t.Fatalf("peak concurrency = %d, exceeds workers = %d", got, workers)
	}
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrency = %d, want it to reach workers = %d", got, workers)
	}
}

// TestSubmitFalseAfterClose pins that Submit refuses work once the pool is closed.
func TestSubmitFalseAfterClose(t *testing.T) {
	t.Parallel()

	p := New(2, func() *resource { return &resource{} })
	p.Close()

	if p.Submit(func(r *resource) error { return nil }) {
		t.Fatal("Submit returned true after Close, want false")
	}
	p.Close() // idempotent: must not panic
}

// TestCloseDrainsQueuedJobs pins the drain contract: Close blocks until every job
// already accepted has finished. If Close returned early, done would be < jobs.
func TestCloseDrainsQueuedJobs(t *testing.T) {
	t.Parallel()

	const workers = 3
	const jobs = 400
	p := New(workers, func() *resource { return &resource{} })

	var done atomic.Int64
	for range jobs {
		p.Submit(func(r *resource) error {
			done.Add(1)
			return nil
		})
	}
	p.Close() // no external WaitGroup: the drain itself must account for every job

	if got := done.Load(); got != jobs {
		t.Fatalf("after Close, %d jobs finished, want all %d", got, jobs)
	}
}
```

## Review

Correct here means four invariants hold at once: amortization (`newResource` runs once
per worker, proven by `TestResourceConstructedOncePerWorker` counting exactly `workers`
calls under 500 jobs), ownership (a resource is never concurrently held, proven by the
per-resource peak-in-use staying at 1 under a barrier that would otherwise expose
sharing), bounding (global concurrency never exceeds and does reach `workers`, proven
by the atomic peak), and the drain contract (`Close` accounts for every accepted job,
proven by the no-external-WaitGroup count matching after `Close` returns). The guarantee
comes from the worker-local variable `res`: it is captured once per worker goroutine and
handed into every job that single-threaded loop runs, so there is no shared mutable state
and therefore nothing to lock. The production bug this pattern prevents is a shared
compressor whose `Reset` on one job races a `Write` on another — no panic, no error, just
a corrupted gzip stream landing at the ingest endpoint. Run `-race` to confirm the
resource reuse and the counters are clean; a shared-writer design would light up the
detector immediately.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) -- launch a worker with Add/Done paired, avoiding the Add-racing-Wait bug.
- [`compress/gzip`](https://pkg.go.dev/compress/gzip) -- `Writer.Reset` is what makes reusing one writer across batches cheap and correct.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the channel-owned worker-pool structure this exercise specializes.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- sender-owns-and-closes discipline behind the Submit/Close guard.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-pool-metrics-hook.md](10-pool-metrics-hook.md) | Next: [12-outbox-relay-partial-ack.md](12-outbox-relay-partial-ack.md)
