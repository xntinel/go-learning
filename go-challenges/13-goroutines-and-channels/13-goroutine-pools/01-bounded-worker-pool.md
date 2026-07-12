# Exercise 1: A Fixed-Size Worker Pool For A Job Service

The workhorse of load control in a Go backend is a fixed number of worker
goroutines draining a shared job queue. This exercise builds that pool as a
reusable type — `New`, `Submit`, `Size`, `Close` — with the channel-ownership and
`WaitGroup` discipline that keep it from panicking or leaking, and pins its
behavioral contract, including the drain-on-close guarantee, with race-clean
tests.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
pool/                      independent module: example.com/pool
  go.mod                   go 1.25
  pool.go                  type Pool; New, Submit, Size, Close; Job = func() error
  cmd/
    demo/
      main.go              runnable demo: submit 8 jobs to a pool of 3, drain on Close
  pool_test.go             processes-all, size, reject-after-close, idempotent-close,
                           bounded-concurrency, and drain tests, all -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Pool` running a fixed number of workers over a buffered job channel, with `Submit` returning `false` after `Close`, an idempotent `Close` that drains, and `Size` reporting the worker count.
- Test: all jobs run, `Size` is exact, `Submit` rejects after `Close`, double `Close` is safe, concurrency never exceeds the worker count, and `Close` blocks until in-flight jobs finish.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/13-goroutine-pools/01-bounded-worker-pool/cmd/demo
cd go-solutions/13-goroutines-and-channels/13-goroutine-pools/01-bounded-worker-pool
```

### The shape of the pool

A `Job` is the unit of work: `func() error`. Making it a closure rather than a
data struct means the pool never has to know what a job *is* — the caller captures
whatever state it needs and the pool just runs the function. The error return is
there so a job can report failure; this first pool ignores it (a later exercise
collects it), but keeping it in the signature now means callers do not have to
change later.

`New(workers)` starts exactly `workers` goroutines, each running the same
`worker` loop, and hands them a buffered job channel. The buffer (here
`workers*2`) is a small shock absorber: it lets a burst of `Submit` calls enqueue
without each one blocking until a worker is free, without turning the queue into
an unbounded backlog. The concurrency ceiling is the *number of workers*, not the
buffer size — at most `workers` jobs ever run at once regardless of how full the
channel is.

The worker loop is `for job := range p.jobs`. Ranging over a channel receives
until the channel is closed *and* drained, then the loop ends and the goroutine
returns. That single line encodes the drain: when `Close` closes `p.jobs`, each
worker finishes the jobs still buffered, sees the channel closed, and exits.

### Why Submit needs a guard and Close needs a flag

Sending on a closed channel panics, and closing a channel twice panics. Both are
real risks here: a caller might `Submit` after `Close`, and something might call
`Close` twice. The guard is a `sync.Mutex` plus a `closed` bool. `Submit` takes
the lock, and if `closed` is set it returns `false` without touching the channel —
so it never sends on a closed channel and the caller learns the pool is gone.
`Close` takes the lock, returns immediately if `closed` is already set (making it
idempotent), otherwise sets the flag, closes the channel, releases the lock, and
finally `wg.Wait()`s.

The ordering in `Close` matters. The channel close and flag set happen under the
lock so they are atomic with respect to `Submit`. But `wg.Wait()` is called
*after* releasing the lock: waiting while holding the lock would block any
concurrent `Submit` for the whole drain, and worse, a worker that needed the lock
would deadlock against `Close`. Release first, then wait.

Create `pool.go`:

```go
package pool

import "sync"

// Job is a unit of work. It returns an error so a job can report failure; this
// pool runs jobs for their effect and ignores the error.
type Job func() error

// Pool runs a fixed number of worker goroutines that drain a shared job queue.
// The number of workers is the concurrency ceiling.
type Pool struct {
	workers int
	jobs    chan Job
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

// New starts workers goroutines draining a buffered job channel.
func New(workers int) *Pool {
	p := &Pool{
		workers: workers,
		jobs:    make(chan Job, workers*2),
	}
	for range workers {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		_ = job()
	}
}

// Submit enqueues job. It returns false if the pool is closed, so the caller
// never sends on a closed channel.
func (p *Pool) Submit(job Job) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job
	return true
}

// Size reports the number of workers, which is the concurrency ceiling.
func (p *Pool) Size() int {
	return p.workers
}

// Close stops accepting work and blocks until every in-flight and queued job has
// finished. It is idempotent.
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

Note `Submit` sends on `p.jobs` while holding `p.mu`. Because the channel is
buffered, that send does not block until the buffer is full; if the pool is
saturated, `Submit` blocks on a full channel while holding the lock, which is the
simple backpressure this pool provides. Exercise 6 replaces it with a
non-blocking `TrySubmit`.

### The runnable demo

The demo submits eight jobs to a pool of three workers, each job sleeping briefly
and recording its id, then closes the pool and prints how many completed. The
`Close` call blocks until all eight are done, so the count is always eight.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"example.com/pool"
)

func main() {
	p := pool.New(3)
	fmt.Printf("pool size: %d\n", p.Size())

	var done atomic.Int64
	for range 8 {
		p.Submit(func() error {
			time.Sleep(5 * time.Millisecond)
			done.Add(1)
			return nil
		})
	}

	p.Close() // blocks until all submitted jobs finish
	fmt.Printf("completed: %d\n", done.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pool size: 3
completed: 8
```

### Tests

The tests pin every clause of the contract. `TestPoolProcessesAllJobs` submits
100 jobs and asserts an atomic counter reaches 100 — proof nothing is dropped.
`TestPoolSize` asserts `Size` echoes the configured count. `TestPoolCloseRejectsSubmits`
asserts `Submit` returns `false` after `Close`. `TestPoolCloseIsIdempotent` calls
`Close` twice and asserts no panic. `TestPoolProcessesJobsConcurrently` runs ten
slow jobs and asserts the observed maximum concurrency never exceeds the worker
count, using an atomic compare-and-swap to record the peak. `TestPoolWaitsForJobsToFinish`
is the drain proof: ten jobs each sleep 10ms, `Close` is called immediately, and
the test asserts every job's effect is visible after `Close` returns — so `Close`
must have blocked until they finished.

Create `pool_test.go`:

```go
package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolProcessesAllJobs(t *testing.T) {
	t.Parallel()

	p := New(4)
	defer p.Close()

	var counter atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		if !p.Submit(func() error {
			defer wg.Done()
			counter.Add(1)
			return nil
		}) {
			t.Fatal("Submit returned false")
		}
	}
	wg.Wait()
	if got := counter.Load(); got != 100 {
		t.Fatalf("counter = %d, want 100", got)
	}
}

func TestPoolSize(t *testing.T) {
	t.Parallel()

	p := New(8)
	defer p.Close()
	if got := p.Size(); got != 8 {
		t.Fatalf("Size = %d, want 8", got)
	}
}

func TestPoolCloseRejectsSubmits(t *testing.T) {
	t.Parallel()

	p := New(2)
	p.Close()
	if p.Submit(func() error { return nil }) {
		t.Fatal("Submit should return false after Close")
	}
}

func TestPoolCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p := New(2)
	p.Close()
	p.Close() // must not panic
}

func TestPoolProcessesJobsConcurrently(t *testing.T) {
	t.Parallel()

	p := New(4)
	defer p.Close()

	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		if !p.Submit(func() error {
			defer wg.Done()
			cur := concurrent.Add(1)
			for {
				m := maxConcurrent.Load()
				if cur <= m {
					break
				}
				if maxConcurrent.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			concurrent.Add(-1)
			return nil
		}) {
			t.Fatal("Submit returned false")
		}
	}
	wg.Wait()
	if got := maxConcurrent.Load(); got > 4 {
		t.Fatalf("max concurrent = %d, want <= 4", got)
	}
}

func TestPoolWaitsForJobsToFinish(t *testing.T) {
	t.Parallel()

	p := New(4)

	var done atomic.Int64
	for range 10 {
		if !p.Submit(func() error {
			time.Sleep(10 * time.Millisecond)
			done.Add(1)
			return nil
		}) {
			t.Fatal("Submit returned false")
		}
	}
	p.Close() // must block until all 10 finish
	if got := done.Load(); got != 10 {
		t.Fatalf("done = %d after Close, want 10", got)
	}
}
```

## Review

The pool is correct when three invariants hold together. Fan-out is bounded: at
most `Size` jobs run at once, which `TestPoolProcessesJobsConcurrently` checks by
recording the peak observed concurrency. Nothing is lost: `Close` closes the job
channel and `wg.Wait()`s, so it blocks until every queued and in-flight job has
returned — `TestPoolWaitsForJobsToFinish` proves the drain by reading each job's
effect after `Close`. And the channel is never misused: `Submit` returns `false`
instead of sending on a closed channel, and `Close`'s `closed` flag makes a double
`Close` a no-op instead of a double-close panic.

The mistakes to avoid are the channel-ownership ones. Do not close `p.jobs` from a
worker — only `Close`, the owner, closes it, exactly once. Do not call
`p.wg.Add(1)` inside `worker`; it must run before `go p.worker()` in `New`, or
`Wait` can return before a worker has started. Do not hold `p.mu` across
`wg.Wait()` — release the lock first, or a worker that needs the lock deadlocks
the drain. Run `go test -race` to confirm the mutex actually serializes the
`closed` flag and the channel operations under concurrent `Submit` and `Close`.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the channel model, ranging over a channel, and closing.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the `Add`-before-`go`, `Done`-via-`defer`, `Wait`-after discipline.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in and who owns closing a channel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fan-out-fan-in-results.md](02-fan-out-fan-in-results.md)
