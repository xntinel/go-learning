# Exercise 6: Pausable worker pool (incident-time pause/resume of consumption)

A downstream dependency is melting and the incident runbook says: stop
consuming from the queue NOW, without dropping what is already buffered, and
resume when the dependency recovers. This module builds that ops control — a
worker pool whose workers park on a `Cond` while a `paused` flag is set — and
uses it to demonstrate the sharpest `Signal`-vs-`Broadcast` trap in the whole
chapter: `Resume` with `Signal` strands N-1 workers.

## What you'll build

```text
pauser/                     independent module: example.com/pauser
  go.mod                    module path example.com/pauser
  pool.go                   type Pool: New, Submit, Pause, Resume, Stop, QueueLen; ErrStopped
  cmd/
    demo/
      main.go               pause, buffer 6 jobs with zero progress, resume, drain
  pool_test.go              paused = zero progress, Resume wakes ALL workers, Stop leak-check
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Pool` of N worker goroutines consuming a FIFO job queue; `Pause()` makes workers park between jobs, `Resume()` Broadcasts them all awake, `Stop()` is terminal — it wakes even paused workers so they exit, then joins them; `Submit` after `Stop` fails with a wrapped `ErrStopped`.
- Test: after `Pause`, submitted jobs make zero progress with every worker durably parked; `Resume` releases all N workers at once; `Stop` while paused terminates every worker with no goroutine leak; `-race` churn with interleaved pause/resume.
- Verify: `go test -count=1 -race ./...`

### One Cond, one waiter class, three-part predicate

Every parked goroutine here is a worker, and every worker waits for the same
compound condition: "there is a job AND we are not paused, OR we are stopped".
Because the waiter class is homogeneous, ONE `Cond` is correct (contrast the
bounded buffer, where producers and consumers wait on different predicates and
need two). The worker loop is the standard shape — take the lock, `for` over
the negated predicate calling `Wait`, then act:

```
for !p.stopped && (p.paused || len(p.queue) == 0) {
	p.cond.Wait()
}
```

The ordering inside matters. `stopped` is checked first and short-circuits
everything: a stopped pool must release workers even if it is also paused and
even if jobs remain queued — `Stop` is an incident-time abort, and the
remaining queue is deliberately discarded (documented behavior; if you need
drain-then-stop semantics, call `Resume`, wait for `QueueLen() == 0`, then
`Stop`). Each worker pops one job under the lock, then runs it UNLOCKED —
holding the mutex across the handler would serialize the pool to one effective
worker and block `Pause` behind the slowest job.

### Where Signal is right and where it is a production bug

`Submit` uses `Signal`: one new job can satisfy exactly one interchangeable
idle worker, so waking all N to fight over one job is pure wasted scheduling.
It also skips the signal entirely while paused — no worker can proceed, so the
wakeup would be a no-op re-park; the jobs simply accumulate.

`Resume` and `Stop` MUST use `Broadcast`. After a pause, all N workers are
parked and the queue may hold hundreds of jobs; a `Resume` that `Signal`s
wakes exactly one worker, and the other N-1 sleep until some future `Submit`
happens to signal again — your pool silently runs at 1/N capacity for the rest
of the incident, which is precisely the kind of degradation that takes hours
to notice. `Stop` with `Signal` is worse: one worker exits and `wg.Wait`
blocks forever on the rest — a deadlocked shutdown. The test
`TestResumeWakesAllWorkers` is constructed so a `Signal` implementation fails
it deterministically: N jobs whose handlers all block on a channel can only
reach N concurrent starts if all N workers woke.

Create `pool.go`:

```go
package pauser

import (
	"errors"
	"fmt"
	"sync"
)

// ErrStopped is returned by Submit after Stop: the pool is terminal.
var ErrStopped = errors.New("pool stopped")

// Job is one unit of queue work.
type Job func()

// Pool runs jobs on a fixed set of workers that can be paused, resumed, and
// stopped at runtime.
type Pool struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []Job
	paused  bool
	stopped bool
	wg      sync.WaitGroup
}

// New starts a pool with the given number of workers (minimum 1).
func New(workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	p := &Pool{}
	p.cond = sync.NewCond(&p.mu)
	for range workers {
		p.wg.Go(p.worker)
	}
	return p
}

func (p *Pool) worker() {
	for {
		p.mu.Lock()
		for !p.stopped && (p.paused || len(p.queue) == 0) {
			p.cond.Wait()
		}
		if p.stopped {
			p.mu.Unlock()
			return
		}
		job := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		job() // run outside the lock
	}
}

// Submit enqueues a job. While paused the job is buffered; after Stop it is
// rejected with a wrapped ErrStopped.
func (p *Pool) Submit(job Job) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return fmt.Errorf("submit job: %w", ErrStopped)
	}
	p.queue = append(p.queue, job)
	if !p.paused {
		p.cond.Signal() // one job satisfies one interchangeable worker
	}
	return nil
}

// Pause makes workers park after their current job. Buffered and newly
// submitted jobs wait for Resume.
func (p *Pool) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = true
}

// Resume releases every paused worker.
func (p *Pool) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
	p.cond.Broadcast() // ALL workers must wake; Signal strands N-1
}

// Stop terminates the pool, waking even paused workers so they exit, and
// waits for all of them. Jobs still queued are discarded.
func (p *Pool) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.cond.Broadcast() // terminal state releases everyone
	p.mu.Unlock()
	p.wg.Wait()
}

// QueueLen reports the number of buffered, not-yet-started jobs.
func (p *Pool) QueueLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}
```

### The runnable demo

The demo pauses the pool, buffers six jobs while proving zero progress, then
resumes and drains — the exact sequence of an incident runbook.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/pauser"
)

func main() {
	p := pauser.New(3)
	var processed atomic.Int64

	p.Pause() // incident: downstream is unhealthy, stop consuming

	var jobs sync.WaitGroup
	for range 6 {
		jobs.Add(1)
		if err := p.Submit(func() {
			processed.Add(1)
			jobs.Done()
		}); err != nil {
			panic(err)
		}
	}
	fmt.Printf("paused: queued=%d processed=%d\n", p.QueueLen(), processed.Load())

	p.Resume() // downstream recovered
	jobs.Wait()
	fmt.Printf("resumed: queued=%d processed=%d\n", p.QueueLen(), processed.Load())

	p.Stop()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
paused: queued=6 processed=0
resumed: queued=0 processed=6
```

Both lines are deterministic. The first prints before `Resume`, when nothing
can have been popped. For the second, `jobs.Wait()` returns only after every
handler ran, a handler only runs after its job was popped, and no submission
can arrive afterwards — so the queue is exactly empty and the count exactly 6.

### Tests

`TestPausedMakesNoProgress` uses `synctest.Wait` to prove all workers are
durably parked with a non-empty queue. `TestResumeWakesAllWorkers` is the
Signal-vs-Broadcast trap made executable. `TestStopWhilePaused` relies on
`synctest.Test` failing on leaked goroutines: if `Stop` fails to wake paused
workers, the bubble deadlocks and the test fails. `TestChurn` interleaves
pause/resume with concurrent submits under `-race`.

Create `pool_test.go`:

```go
package pauser

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestPausedMakesNoProgress(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := New(3)
		var processed atomic.Int64

		p.Pause()
		for range 5 {
			if err := p.Submit(func() { processed.Add(1) }); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}

		synctest.Wait() // every worker durably parked on the Cond
		if got := processed.Load(); got != 0 {
			t.Fatalf("processed %d job(s) while paused, want 0", got)
		}
		if got := p.QueueLen(); got != 5 {
			t.Fatalf("QueueLen = %d, want 5", got)
		}

		p.Resume()
		synctest.Wait() // workers drained the queue and re-parked idle
		if got := processed.Load(); got != 5 {
			t.Fatalf("processed = %d after Resume, want 5", got)
		}
		if got := p.QueueLen(); got != 0 {
			t.Fatalf("QueueLen = %d after Resume, want 0", got)
		}
		p.Stop()
	})
}

func TestResumeWakesAllWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const workers = 4
		p := New(workers)
		p.Pause()

		started := make(chan struct{}, workers)
		release := make(chan struct{})
		for range workers {
			if err := p.Submit(func() {
				started <- struct{}{}
				<-release // hold the worker inside the job
			}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}

		synctest.Wait()
		if got := len(started); got != 0 {
			t.Fatalf("%d job(s) started while paused, want 0", got)
		}

		p.Resume()
		synctest.Wait() // all workers now blocked inside their job
		if got := len(started); got != workers {
			t.Fatalf("only %d of %d workers picked up work after Resume; Signal instead of Broadcast?", got, workers)
		}

		close(release)
		p.Stop()
	})
}

func TestStopWhilePaused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := New(3)
		p.Pause()
		if err := p.Submit(func() {}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		synctest.Wait()

		p.Stop() // must wake paused workers so they exit; queue is discarded

		if err := p.Submit(func() {}); !errors.Is(err, ErrStopped) {
			t.Fatalf("Submit after Stop = %v, want ErrStopped", err)
		}
	})
}

func TestChurn(t *testing.T) {
	t.Parallel()

	p := New(8)
	var processed atomic.Int64
	const jobs = 200

	var jwg sync.WaitGroup
	jwg.Add(jobs)
	var swg sync.WaitGroup
	for range jobs {
		swg.Go(func() {
			if err := p.Submit(func() {
				processed.Add(1)
				jwg.Done()
			}); err != nil {
				t.Errorf("Submit: %v", err)
				jwg.Done()
			}
		})
	}
	for range 10 {
		p.Pause()
		p.Resume()
	}
	swg.Wait()
	jwg.Wait()
	p.Stop()

	if got := processed.Load(); got != jobs {
		t.Fatalf("processed = %d, want %d", got, jobs)
	}
}

func Example() {
	p := New(2)
	var done sync.WaitGroup
	done.Add(1)
	_ = p.Submit(func() {
		fmt.Println("job processed")
		done.Done()
	})
	done.Wait()
	p.Stop()
	fmt.Println(errors.Is(p.Submit(func() {}), ErrStopped))
	// Output:
	// job processed
	// true
}
```

## Review

The pool is correct when three statements hold under `-race`: a paused pool
makes zero progress while buffering submissions; `Resume` restores FULL
concurrency, not one worker's worth; and `Stop` terminates every worker from
any state. The second is where real systems rot — a `Signal` in `Resume`
compiles, passes a single-worker smoke test, and quietly caps throughput at
one worker in production. `TestResumeWakesAllWorkers` makes that bug loud: the
job handlers park inside their jobs, so the count of started jobs equals the
count of woken workers exactly.

Watch for two structural mistakes when you vary this design. Running `job()`
under the mutex serializes the pool and blocks `Pause` behind the current job
— pop under the lock, run outside it. And checking `paused` before `stopped`
in the predicate makes `Stop` hang on a paused pool: the worker re-parks on
`paused` and never observes the terminal flag. `synctest.Test` converts that
hang into an immediate deadlock failure instead of a CI timeout.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — Signal vs Broadcast semantics.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 way to start tracked workers.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — durable blocking and goroutine-leak detection.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the design rationale behind the bubble model.

---

Back to [05-drain-barrier.md](05-drain-barrier.md) | Next: [07-job-status-waiter.md](07-job-status-waiter.md)
