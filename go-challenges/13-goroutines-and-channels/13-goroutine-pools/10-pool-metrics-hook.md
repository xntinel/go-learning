# Exercise 10: Instrument A Pool: Queue Depth, Active Workers, Job Latency

A saturated pool does not fail loudly — it gets slow, because work waits in the
queue before a worker picks it up. To see that in production you need three
signals: how deep the queue is, how many workers are busy, and how long jobs spend
waiting versus executing. This exercise wraps the pool with those metrics, recorded
through atomics and exposed as a race-free `Stats()` snapshot — the numbers a senior
engineer watches to detect saturation and tune the worker count.

This module is fully self-contained.

## What you'll build

```text
metricpool/                independent module: example.com/metricpool
  go.mod                   go 1.25
  pool.go                  type Pool, Stats; New, Submit, Size, Stats, Close
  cmd/
    demo/
      main.go              runnable demo: run known jobs, print a Stats snapshot
  pool_test.go             exec-duration, active-workers, queue-depth tests, -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a pool that records, per job, submit-to-start wait time and execution duration, plus gauges for active workers and queue depth, exposed via a `Stats()` snapshot read from atomics.
- Test: recorded execution duration matches a known sleep within tolerance and the processed count is exact; the active-workers gauge peaks at `Size` and returns to 0 after drain; queue depth reflects pending work under a slow consumer.
- Verify: `go test -count=1 -race ./...`

### Measure the queueing, not just the work

The insight that makes pool metrics useful is that a job's total latency splits
into two parts with different meanings. The *wait* — from when it was submitted to
when a worker started it — is pure queueing delay; it grows when the pool is
saturated and there are more jobs than workers. The *execution* — from start to
finish — is the work itself; it grows when the downstream is slow. Watching only
end-to-end latency conflates the two, so you cannot tell whether to add workers
(wait is high, pool-bound) or fix the dependency (exec is high, downstream-bound).
Recording them separately is the whole point.

So each job carries its submit timestamp. When a worker dequeues it, it records
`time.Since(submittedAt)` as wait, increments the active-workers gauge, times the
execution, then decrements the gauge and bumps the processed count. The two gauges
are live: active workers (how many are mid-job right now) tells you utilization
against `Size` — pegged at `Size` means saturated — and queue depth (`len(jobs)`)
tells you the backpressure building behind them.

Every counter is an atomic, so `Stats()` can read a consistent-enough snapshot
without a lock and without racing the workers that update them. Atomics are the
right tool here: the writes are single-word increments on hot paths, and a
snapshot that is a few nanoseconds stale is perfectly fine for a metric. Using a
mutex would serialize every job's bookkeeping against every `Stats()` read for no
benefit. (The reads are independent atomics, so the snapshot is not a single
atomic instant — that is an acceptable and standard trade-off for gauges.)

Create `pool.go`:

```go
package metricpool

import (
	"sync"
	"sync/atomic"
	"time"
)

// Job is a unit of work.
type Job func() error

// Stats is a point-in-time snapshot of pool metrics.
type Stats struct {
	Processed     uint64        // jobs completed
	ActiveWorkers int64         // jobs executing right now
	QueueDepth    int           // jobs waiting in the queue right now
	AvgWait       time.Duration // mean submit-to-start delay
	AvgExec       time.Duration // mean execution duration
}

type job struct {
	fn          Job
	submittedAt time.Time
}

// Pool is a worker pool instrumented with latency and utilization metrics.
type Pool struct {
	workers   int
	jobs      chan job
	active    atomic.Int64
	processed atomic.Uint64
	totalWait atomic.Uint64 // sum of wait nanoseconds
	totalExec atomic.Uint64 // sum of execution nanoseconds
	mu        sync.Mutex
	closed    bool
	wg        sync.WaitGroup
}

// New starts workers goroutines draining a queue of the given capacity.
func New(workers, queue int) *Pool {
	p := &Pool{
		workers: workers,
		jobs:    make(chan job, queue),
	}
	for range workers {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for j := range p.jobs {
		p.totalWait.Add(uint64(time.Since(j.submittedAt)))
		p.active.Add(1)
		start := time.Now()
		_ = j.fn()
		p.totalExec.Add(uint64(time.Since(start)))
		p.active.Add(-1)
		p.processed.Add(1)
	}
}

// Submit enqueues fn, stamping its submit time. It returns false after Close.
func (p *Pool) Submit(fn Job) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job{fn: fn, submittedAt: time.Now()}
	return true
}

// Size reports the worker count.
func (p *Pool) Size() int { return p.workers }

// Stats returns a snapshot of the current metrics.
func (p *Pool) Stats() Stats {
	processed := p.processed.Load()
	var avgWait, avgExec time.Duration
	if processed > 0 {
		avgWait = time.Duration(p.totalWait.Load() / processed)
		avgExec = time.Duration(p.totalExec.Load() / processed)
	}
	return Stats{
		Processed:     processed,
		ActiveWorkers: p.active.Load(),
		QueueDepth:    len(p.jobs),
		AvgWait:       avgWait,
		AvgExec:       avgExec,
	}
}

// Close drains and waits.
func (p *Pool) Close() {
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

### The runnable demo

The demo runs six jobs with a known sleep, then prints the deterministic parts of
the snapshot after draining: the processed count, and the two gauges both back to
zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/metricpool"
)

func main() {
	p := metricpool.New(3, 16)

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		p.Submit(func() error {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)
			return nil
		})
	}
	wg.Wait()
	p.Close()

	s := p.Stats()
	fmt.Printf("processed=%d active=%d queue=%d\n", s.Processed, s.ActiveWorkers, s.QueueDepth)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed=6 active=0 queue=0
```

### Tests

`TestExecutionDurationRecorded` runs jobs with a known 10ms sleep and asserts the
average execution duration lands in a tolerant window around it and the processed
count is exact. `TestActiveWorkersGauge` blocks `Size` jobs, asserts the gauge
reads `Size` while they run, then asserts it returns to 0 after `Close`.
`TestQueueDepthGauge` occupies the single worker and queues extra jobs, asserting
`QueueDepth` reflects the pending count and never exceeds capacity.

Create `pool_test.go`:

```go
package metricpool

import (
	"sync"
	"testing"
	"time"
)

func TestExecutionDurationRecorded(t *testing.T) {
	t.Parallel()

	p := New(2, 16)
	var wg sync.WaitGroup
	const n = 6
	for range n {
		wg.Add(1)
		p.Submit(func() error {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)
			return nil
		})
	}
	wg.Wait()
	p.Close()

	s := p.Stats()
	if s.Processed != n {
		t.Fatalf("Processed = %d, want %d", s.Processed, n)
	}
	if s.AvgExec < 8*time.Millisecond || s.AvgExec > 60*time.Millisecond {
		t.Fatalf("AvgExec = %v, want ~10ms", s.AvgExec)
	}
}

func TestActiveWorkersGauge(t *testing.T) {
	t.Parallel()

	const workers = 4
	p := New(workers, 16)
	release := make(chan struct{})
	started := make(chan struct{}, workers)
	for range workers {
		p.Submit(func() error {
			started <- struct{}{}
			<-release
			return nil
		})
	}
	for range workers {
		<-started // all workers are now mid-job
	}

	if got := p.Stats().ActiveWorkers; got != workers {
		t.Fatalf("ActiveWorkers = %d, want %d", got, workers)
	}
	if got := int(p.Stats().ActiveWorkers); got > p.Size() {
		t.Fatalf("ActiveWorkers %d exceeds Size %d", got, p.Size())
	}

	close(release)
	p.Close()
	if got := p.Stats().ActiveWorkers; got != 0 {
		t.Fatalf("ActiveWorkers = %d after Close, want 0", got)
	}
}

func TestQueueDepthGauge(t *testing.T) {
	t.Parallel()

	const queue = 8
	p := New(1, queue)
	defer p.Close()

	release := make(chan struct{})
	started := make(chan struct{})
	p.Submit(func() error {
		close(started)
		<-release
		return nil
	})
	<-started // the one worker is busy; queue is empty

	const pending = 3
	for range pending {
		p.Submit(func() error { return nil })
	}
	s := p.Stats()
	if s.QueueDepth != pending {
		t.Fatalf("QueueDepth = %d, want %d", s.QueueDepth, pending)
	}
	if s.QueueDepth > queue {
		t.Fatalf("QueueDepth %d exceeds capacity %d", s.QueueDepth, queue)
	}
	close(release)
}
```

## Review

The instrumentation is correct when the metrics match reality. `TestExecutionDurationRecorded`
confirms the recorded average execution tracks the known sleep and the processed
count is exact — the timing bookkeeping is right. `TestActiveWorkersGauge` confirms
the utilization gauge reads `Size` when the pool is saturated and returns to 0
after drain, and `TestQueueDepthGauge` confirms queue depth reflects pending work
without exceeding capacity. Together those are the signals that distinguish a
pool-bound slowdown (high wait, high queue depth, active pegged at `Size`) from a
downstream-bound one (high exec, low queue depth).

The mistakes to avoid: guarding the counters with a mutex instead of atomics
(needless contention on the hot path); forgetting to decrement the active gauge on
every job exit (it drifts upward and lies about utilization — the `Add(-1)` must be
unconditional); and treating the multi-field snapshot as a single atomic instant
(it is not; each field is read independently, which is fine for gauges). Run
`-race` to confirm the concurrent counter updates and `Stats()` reads are clean.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `Int64`/`Uint64` for lock-free gauges and counters.
- [`time.Since`](https://pkg.go.dev/time#Since) — measuring wait and execution durations.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the pool structure being instrumented.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-graceful-shutdown-drain.md](09-graceful-shutdown-drain.md) | Next: [11-worker-local-resource-pool.md](11-worker-local-resource-pool.md)
