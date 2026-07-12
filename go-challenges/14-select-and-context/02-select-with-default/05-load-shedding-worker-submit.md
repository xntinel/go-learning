# Exercise 5: Bounded Worker Pool With Fail-Fast Submit (503 on Saturation)

When a request handler dispatches work to a bounded pool and the pool is saturated,
the right answer is to reject the request immediately — the caller gets a 503 and
can retry or degrade — not to grow an unbounded backlog that eventually OOMs the
process and takes down the in-flight work too. Fail-fast admission is a non-blocking
try-send: if the queue is full, `default` fires and `TrySubmit` returns false.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
pool/                       independent module: example.com/pool
  go.mod                    go 1.26
  pool.go                   type Pool; TrySubmit (try-send + counters), Shutdown
  cmd/
    demo/
      main.go               saturates the pool, shows accepted vs rejected
  pool_test.go              exact saturation accounting, run-once, -race counters
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `New(workers, queue int)` (starts workers), `TrySubmit(Job) bool` (non-blocking send; increments `accepted`/`rejected`), `Accepted()`/`Rejected()`, and `Shutdown()` (closes the queue, waits for workers).
- Test: saturate the pool and assert the accepted count is exactly `workers + queue` before rejections begin, with `Rejected()` counting the overflow; assert every accepted job runs exactly once; a `-race` run on the counters.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Fail fast, and count both sides

The pool is `workers` goroutines ranging over a bounded jobs channel of capacity
`queue`. `TrySubmit` is a non-blocking send: it succeeds while there is room (a
worker is idle, or the buffer has space), and the instant the queue is full it takes
`default`, increments `rejected`, and returns false. The caller maps that false to
HTTP 503. Both counters are `atomic.Int64` because `TrySubmit` runs from many
handler goroutines at once.

The saturation math is worth internalizing, because it is what the test pins. With
`workers` workers and a `queue`-capacity buffer, the number of jobs the pool can
*hold* before rejecting is `workers + queue`: each worker can be busy executing one
job (that is `workers` jobs "in flight" and out of the buffer), and the buffer can
hold `queue` more waiting. So if every job blocks, exactly `workers + queue`
`TrySubmit`s succeed and the next fails. To observe that number deterministically in
a test you must ensure all `workers` workers have actually *picked up* a job before
you count the buffer — otherwise you race the workers' startup. The test does this
by submitting `workers` blocking jobs first and waiting for each to signal that it
is running; only then is the buffer empty and the remaining `queue` slots countable
exactly.

`Shutdown` closes the jobs channel and waits on a `sync.WaitGroup`. Closing the
channel ends every worker's `for job := range p.jobs` loop after the buffer drains,
so every accepted job runs exactly once before the workers exit — the pool does not
drop accepted work on shutdown, it finishes it.

Create `pool.go`:

```go
package pool

import (
	"sync"
	"sync/atomic"
)

// Job is a unit of work run by a pool worker.
type Job func()

// Pool is a bounded worker pool. TrySubmit admits a job if a worker is free or the
// queue has room, and fails fast otherwise so the caller can shed load (e.g. 503).
type Pool struct {
	jobs     chan Job
	accepted atomic.Int64
	rejected atomic.Int64
	wg       sync.WaitGroup
}

// New starts a pool of `workers` goroutines draining a `queue`-capacity channel.
func New(workers, queue int) *Pool {
	p := &Pool{jobs: make(chan Job, queue)}
	p.wg.Add(workers)
	for range workers {
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		job()
	}
}

// TrySubmit admits job without blocking. It returns true if the job was queued
// (accepted), or false if the pool is saturated (rejected). The false return is
// the caller's cue to shed load.
func (p *Pool) TrySubmit(job Job) bool {
	select {
	case p.jobs <- job:
		p.accepted.Add(1)
		return true
	default:
		p.rejected.Add(1)
		return false
	}
}

// Accepted reports how many jobs were admitted.
func (p *Pool) Accepted() int64 { return p.accepted.Load() }

// Rejected reports how many jobs were shed because the pool was saturated.
func (p *Pool) Rejected() int64 { return p.rejected.Load() }

// Shutdown stops accepting work and waits for all accepted jobs to finish. It must
// not be called concurrently with TrySubmit.
func (p *Pool) Shutdown() {
	close(p.jobs)
	p.wg.Wait()
}
```

### The runnable demo

The demo starts a pool whose jobs all block on a gate, saturates it, and prints the
accepted/rejected split, then releases the gate and shuts down. With 2 workers, a
queue of 3, and 8 submissions, exactly 5 (`2 + 3`) are accepted and 3 rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/pool"
)

func main() {
	p := pool.New(2, 3)

	gate := make(chan struct{})
	running := make(chan struct{}, 2)
	var ran sync.WaitGroup

	// First saturate the two workers so they are provably busy.
	for range 2 {
		ran.Add(1)
		p.TrySubmit(func() {
			running <- struct{}{}
			<-gate
			ran.Done()
		})
	}
	<-running
	<-running // both workers are now blocked on the gate; buffer is empty

	// Fill the queue of 3, then overflow by 3.
	submitted := 0
	for range 6 {
		if p.TrySubmit(func() {}) {
			submitted++
		}
	}

	fmt.Println("accepted:", p.Accepted())
	fmt.Println("rejected:", p.Rejected())

	close(gate) // release the blockers
	ran.Wait()
	p.Shutdown()
	fmt.Println("shutdown complete")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
accepted: 5
rejected: 3
shutdown complete
```

### Tests

`TestSaturationIsExact` is the core: it submits `workers` blocking jobs, waits for
each to signal it is running (so the buffer is provably empty and both workers are
in flight), then fills the `queue` slots and overflows, asserting the accepted count
is exactly `workers + queue` and the rest are rejected. It then releases the gate,
shuts down, and asserts every accepted job ran exactly once — no accepted work is
dropped. The counters are exercised under `-race`.

Create `pool_test.go`:

```go
package pool

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSaturationIsExact(t *testing.T) {
	t.Parallel()

	const workers, queue = 3, 4
	const capacity = workers + queue
	p := New(workers, queue)

	gate := make(chan struct{})
	running := make(chan struct{}, workers)
	var executed atomic.Int64

	// Occupy every worker with a blocking job so the buffer is the only slack.
	for range workers {
		if !p.TrySubmit(func() {
			running <- struct{}{}
			<-gate
			executed.Add(1)
		}) {
			t.Fatal("a worker-occupying submit was rejected")
		}
	}
	for range workers {
		<-running // wait until this worker has actually taken a job
	}

	// Now all workers are busy and the buffer is empty. Fill the queue exactly.
	for i := range queue {
		if !p.TrySubmit(func() { executed.Add(1) }) {
			t.Fatalf("queue submit %d rejected before the buffer was full", i)
		}
	}

	// The next submits must all be rejected: pool is saturated.
	for i := range 5 {
		if p.TrySubmit(func() { executed.Add(1) }) {
			t.Fatalf("overflow submit %d accepted past capacity %d", i, capacity)
		}
	}

	if got := p.Accepted(); got != capacity {
		t.Fatalf("Accepted() = %d, want %d", got, capacity)
	}
	if got := p.Rejected(); got != 5 {
		t.Fatalf("Rejected() = %d, want 5", got)
	}

	close(gate) // release the blocking jobs
	p.Shutdown()

	// Every accepted job (workers blockers + queue fillers) ran exactly once.
	if got := executed.Load(); got != int64(capacity) {
		t.Fatalf("executed %d jobs, want %d (each accepted job runs once)", got, capacity)
	}
}

func TestConcurrentSubmitAccounting(t *testing.T) {
	t.Parallel()

	p := New(4, 16)
	var done sync.WaitGroup
	var submitted atomic.Int64

	for range 8 {
		done.Add(1)
		go func() {
			defer done.Done()
			for range 100 {
				submitted.Add(1)
				p.TrySubmit(func() {}) // fast jobs; some accepted, some maybe rejected
			}
		}()
	}
	done.Wait()
	p.Shutdown()

	if p.Accepted()+p.Rejected() != submitted.Load() {
		t.Fatalf("accepted %d + rejected %d != submitted %d",
			p.Accepted(), p.Rejected(), submitted.Load())
	}
}
```

## Review

The pool is correct when saturation is exact and no accepted job is lost: precisely
`workers + queue` submissions succeed while every worker is busy, and every accepted
job runs once before `Shutdown` returns. The subtle test-design point — and the
reason `TestSaturationIsExact` waits for the `running` signals — is that counting
the buffer before the workers have picked up their jobs races the scheduler and
yields a flaky, off-by-`workers` count; the signal makes "all workers in flight" an
established fact. The production mistake this exercise inoculates against is the
*blocking* submit: swapping the `select`/`default` for a bare `p.jobs <- job` turns
a saturated pool into a place where handler goroutines pile up blocked, growing
memory and latency without bound until the process dies — the opposite of shedding
load. `TestConcurrentSubmitAccounting` under `-race` proves the two counters plus
the channel account for every submission with nothing lost or double-counted.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the non-blocking send that powers fail-fast admission.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the clean-shutdown barrier for the workers.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded work queues and backpressure.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-wait-for-dependency-readiness.md](06-wait-for-dependency-readiness.md)
