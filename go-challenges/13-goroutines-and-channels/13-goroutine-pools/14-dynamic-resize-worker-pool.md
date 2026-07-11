# Exercise 14: Resize A Running Pool Without Dropping Or Leaking

**Level: Advanced**

A queue-consumer autoscaler adjusts worker count at runtime: scale up when queue
depth climbs, scale down when it drains, so the service tracks load without
permanently over-provisioning connections to a downstream. Resizing a *live* pool
is where the subtle bugs hide -- growing must add workers without racing the
shutdown wait, and shrinking must retire exactly the surplus workers, letting each
finish its current job, without discarding queued work and without leaking the
retired goroutines. This exercise builds a pool whose worker set grows and shrinks
safely on demand.

This module is self-contained: its own module, an `elasticpool` package, a demo,
and tests. Nothing here imports another exercise.

## What you'll build

```text
elasticpool/                 independent module: example.com/elasticpool
  go.mod                     go 1.26
  elasticpool.go             type Pool; New, Submit, Resize, Size, Close
  cmd/demo/main.go           runnable demo: grow then shrink a live pool, count jobs
  elasticpool_test.go        resize-exactness, gauge-settling, in-flight completion, peak, leak, closed tests
```

- Files: `elasticpool.go`, `cmd/demo/main.go`, `elasticpool_test.go`.
- Implement: `New(workers, queue int) *Pool`, `(*Pool) Submit(job func()) bool`, `(*Pool) Resize(workers int)`, `(*Pool) Size() int`, `(*Pool) Close()`.
- Test: after Resize up then down Size is exact and no job is dropped; the start/stop gauge settles to Size; an in-flight job finishes before its worker retires; observed concurrency never exceeds Size; goleak confirms no leaked workers; Submit returns false after Close.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/elasticpool/cmd/demo
cd ~/go-exercises/elasticpool
go mod init example.com/elasticpool
go get go.uber.org/goleak
go mod tidy
```

### Retire by targeting a worker, not by closing the queue

The classic fixed pool has one shutdown lever: close the shared job channel and
every worker sees the closed channel and exits. That is all-or-nothing -- it
cannot retire *one* worker out of eight. To shrink a live pool you need to target
individual workers, so each worker gets its own control block: a `quit` channel to
retire it and a `done` channel it closes when it has fully exited. The shared job
queue stays open the whole time; only the worker set behind it changes.

The invariant that makes shrinking safe is retirement order inside the worker
loop. A worker must (1) finish the job it is currently running, (2) then notice it
has been retired, and (3) exit *without* grabbing another job. If the retire check
had equal footing with the job receive in a single `select`, Go's random case
choice could let a retiring worker pull one more job off the queue -- harmless for
correctness but it muddies "retire exactly the surplus." The fix is a two-stage
select: a non-blocking `quit`-first check at the top of each iteration gives
retirement priority, so once `quit` is closed the worker returns the moment its
current job returns, before touching the queue again. Crucially, retiring a worker
never drops queued work: the jobs it would have run are simply drained by the
workers that remain.

The second invariant is that Resize is *synchronous with respect to the gauge*.
Each worker bumps a start/stop gauge (`+1` when it starts, `-1` when it exits) and
acks the transition on a channel. Resize-up waits for every new worker's `started`
ack before returning; Resize-down waits for every retired worker's `done` ack. So
the instant Resize returns, `Size` and the gauge already reflect the new target --
no sleeps, no polling, and a shrink that retires a busy worker blocks precisely
until that worker's in-flight job completes. The `done` wait happens *outside* the
lock, so a slow in-flight job cannot block a concurrent `Submit` or `Size`.

Create `elasticpool.go`:

```go
package elasticpool

import (
	"sync"
	"sync/atomic"
)

// worker is one goroutine's control block. Closing quit retires it after its
// current job; the worker closes done once it has fully exited.
type worker struct {
	quit    chan struct{}
	done    chan struct{}
	started chan struct{}
}

// Pool is a worker pool whose live worker set can grow and shrink on demand.
// The queue is shared; workers are individually retirable.
type Pool struct {
	mu      sync.Mutex
	jobs    chan func()
	workers map[int]*worker
	nextID  int
	closed  bool

	// running is a start/stop gauge: +1 when a worker begins, -1 when it exits.
	// After any Resize returns it equals Size, proving exactly the surplus left.
	running atomic.Int64
}

// New starts `workers` workers draining a queue of the given capacity.
func New(workers, queue int) *Pool {
	p := &Pool{
		jobs:    make(chan func(), queue),
		workers: make(map[int]*worker),
	}
	p.grow(workers)
	return p
}

// worker drains the shared queue until retired or the queue is closed. The quit
// check has priority over the job receive so a retired worker never grabs a new
// job after finishing its current one.
func (p *Pool) worker(w *worker) {
	p.running.Add(1)
	close(w.started)
	defer func() {
		p.running.Add(-1)
		close(w.done)
	}()
	for {
		select {
		case <-w.quit:
			return
		default:
		}
		select {
		case <-w.quit:
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			job()
		}
	}
}

// grow launches n new workers and blocks until each has incremented the gauge,
// so Size and running are consistent the instant grow returns. Caller holds mu.
func (p *Pool) grow(n int) {
	started := make([]chan struct{}, 0, n)
	for range n {
		w := &worker{
			quit:    make(chan struct{}),
			done:    make(chan struct{}),
			started: make(chan struct{}),
		}
		p.workers[p.nextID] = w
		p.nextID++
		started = append(started, w.started)
		go p.worker(w)
	}
	for _, s := range started {
		<-s
	}
}

// Submit enqueues job; returns false after Close.
func (p *Pool) Submit(job func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job
	return true
}

// Resize grows or shrinks the live worker set to `workers`. Retired workers stop
// after finishing any in-flight job; queued jobs are never dropped.
func (p *Pool) Resize(workers int) {
	if workers < 0 {
		workers = 0
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	cur := len(p.workers)
	switch {
	case workers > cur:
		p.grow(workers - cur)
		p.mu.Unlock()
	case workers < cur:
		surplus := cur - workers
		retired := make([]chan struct{}, 0, surplus)
		for id, w := range p.workers {
			if surplus == 0 {
				break
			}
			close(w.quit)
			delete(p.workers, id)
			retired = append(retired, w.done)
			surplus--
		}
		p.mu.Unlock()
		// Wait outside the lock: a retiring worker may still be finishing an
		// in-flight job, and Submit/Size must not block behind that.
		for _, d := range retired {
			<-d
		}
	default:
		p.mu.Unlock()
	}
}

// Size reports the current number of live workers.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}

// Close retires all workers and drains the queue. Queued jobs run before the
// workers exit; a second Close is a no-op.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.jobs)
	done := make([]chan struct{}, 0, len(p.workers))
	for id, w := range p.workers {
		done = append(done, w.done)
		delete(p.workers, id)
	}
	p.mu.Unlock()
	// Workers exit via the closed-queue path (ok == false) after draining every
	// remaining job, so nothing queued is lost.
	for _, d := range done {
		<-d
	}
}
```

Note the two different shutdown paths. `Resize`-down retires targeted workers via
their `quit` channels while the queue stays open. `Close` instead closes the
*queue*, so the remaining workers drain every buffered job and then exit on the
closed-channel receive (`ok == false`) -- that is the drain contract, and it is
why nothing queued is lost on shutdown.

### The runnable demo

The demo grows a pool from 2 to 4 workers, submits work across the resize, shrinks
to 1, then closes and confirms every submitted job ran and Submit is refused after
Close. Output is deterministic: the job counter is atomic and every print happens
after the relevant barrier.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/elasticpool"
)

func main() {
	p := elasticpool.New(2, 128)
	fmt.Printf("initial size: %d\n", p.Size())

	var completed atomic.Int64
	var wg sync.WaitGroup

	submit := func(n int) {
		for range n {
			wg.Add(1)
			p.Submit(func() {
				defer wg.Done()
				completed.Add(1)
			})
		}
	}

	const total = 40
	submit(total / 2)
	p.Resize(4)
	fmt.Printf("after resize up: %d\n", p.Size())
	submit(total / 2)
	p.Resize(1)
	fmt.Printf("after resize down: %d\n", p.Size())

	wg.Wait()
	p.Close()

	fmt.Printf("jobs submitted: %d\n", total)
	fmt.Printf("jobs completed: %d\n", completed.Load())
	fmt.Printf("final size after close: %d\n", p.Size())
	fmt.Printf("submit after close: %v\n", p.Submit(func() {}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial size: 2
after resize up: 4
after resize down: 1
jobs submitted: 40
jobs completed: 40
final size after close: 0
submit after close: false
```

### Tests

`TestResizeUpThenDownNothingDropped` grows to 8, shrinks to 2, and submits work in
each phase, asserting Size is exact after each Resize and that all 300 jobs ran --
the shrink dropped nothing. `TestGaugeSettlesToSize` checks the start/stop gauge
equals Size after New, after a grow, after a shrink, and reads 0 after Close,
proving exactly the surplus retired each time. `TestInFlightJobCompletesOnShrink`
retires the only busy worker and uses an event channel (never a sleep) to prove
the in-flight job's `job-done` is observed before Resize's `resize-done`.
`TestPeakConcurrencyNeverExceedsSize` measures peak concurrency per phase across a
resize sequence, asserting it reaches Size and never exceeds it.
`TestNoLeakAfterResizeDownAndClose` uses `goleak.VerifyNone` plus the gauge to
prove retired workers actually exit after both a shrink and Close.
`TestSubmitAfterCloseReturnsFalse` pins the closed-pool contract. A package-level
`TestMain` runs `goleak.VerifyTestMain` so any leaked worker fails the suite.

Create `elasticpool_test.go`:

```go
package elasticpool

import (
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestResizeUpThenDownNothingDropped pins the two headline guarantees: after
// growing then shrinking, Size reports the exact target, and every submitted
// job still ran even though the pool shrank while work was outstanding.
func TestResizeUpThenDownNothingDropped(t *testing.T) {
	p := New(2, 512)

	var completed atomic.Int64
	var wg sync.WaitGroup
	submit := func(n int) {
		for range n {
			wg.Add(1)
			p.Submit(func() {
				defer wg.Done()
				completed.Add(1)
			})
		}
	}

	submit(100)
	p.Resize(8)
	if got := p.Size(); got != 8 {
		t.Fatalf("Size after grow = %d, want 8", got)
	}
	submit(100)
	p.Resize(2)
	if got := p.Size(); got != 2 {
		t.Fatalf("Size after shrink = %d, want 2", got)
	}
	submit(100)

	wg.Wait()
	p.Close()

	if got := completed.Load(); got != 300 {
		t.Fatalf("completed = %d, want 300 (jobs dropped on resize)", got)
	}
	if got := p.Size(); got != 0 {
		t.Fatalf("Size after Close = %d, want 0", got)
	}
}

// TestGaugeSettlesToSize proves the start/stop gauge equals Size after every
// Resize, so exactly the surplus workers were retired -- not one more, not one
// fewer. The gauge settling is synchronous: Resize waits for each worker's ack.
func TestGaugeSettlesToSize(t *testing.T) {
	p := New(3, 16)
	if got, want := p.running.Load(), int64(p.Size()); got != want {
		t.Fatalf("running = %d, want %d after New", got, want)
	}

	p.Resize(7)
	if got := p.running.Load(); got != 7 || p.Size() != 7 {
		t.Fatalf("running = %d, Size = %d, want 7 after grow", got, p.Size())
	}

	p.Resize(2)
	if got := p.running.Load(); got != 2 || p.Size() != 2 {
		t.Fatalf("running = %d, Size = %d, want 2 after shrink", got, p.Size())
	}

	p.Close()
	if got := p.running.Load(); got != 0 {
		t.Fatalf("running = %d, want 0 after Close", got)
	}
}

// TestInFlightJobCompletesOnShrink shows that retiring a busy worker lets its
// current job run to completion. The ordering is proven by a signal, not a
// sleep: the job emits "job-done" as its last act, and the worker only acks its
// retirement (unblocking Resize) after the job returns, so "resize-done" can
// never be observed before "job-done".
func TestInFlightJobCompletesOnShrink(t *testing.T) {
	p := New(1, 8)
	defer p.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	events := make(chan string, 2)

	p.Submit(func() {
		close(started)
		<-release
		events <- "job-done"
	})
	<-started // the single worker is now busy

	go func() {
		p.Resize(0) // retire the busy worker
		events <- "resize-done"
	}()

	close(release) // allow the in-flight job to finish

	if e := <-events; e != "job-done" {
		t.Fatalf("first event = %q, want job-done", e)
	}
	if e := <-events; e != "resize-done" {
		t.Fatalf("second event = %q, want resize-done", e)
	}
	if got := p.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0 after shrink to zero", got)
	}
}

// TestPeakConcurrencyNeverExceedsSize measures observed concurrency in each
// phase of a resize sequence. The peak is reset per phase and must equal the
// current Size (all workers busy at once) and never exceed it.
func TestPeakConcurrencyNeverExceedsSize(t *testing.T) {
	p := New(2, 256)
	defer p.Close()

	var inFlight, peak atomic.Int64

	phase := func(size int) {
		peak.Store(0)
		release := make(chan struct{})
		started := make(chan struct{}, size)
		var wg sync.WaitGroup
		for range size {
			wg.Add(1)
			p.Submit(func() {
				defer wg.Done()
				n := inFlight.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				<-release
				inFlight.Add(-1)
			})
		}
		for range size {
			<-started // all `size` workers are concurrently in-flight
		}
		if got := peak.Load(); got != int64(size) {
			t.Fatalf("peak = %d, want %d (want all workers busy)", got, size)
		}
		if got := peak.Load(); got > int64(p.Size()) {
			t.Fatalf("peak %d exceeds Size %d", got, p.Size())
		}
		close(release)
		wg.Wait()
	}

	phase(2)
	p.Resize(5)
	phase(5)
	p.Resize(1)
	phase(1)
}

// TestNoLeakAfterResizeDownAndClose confirms retired workers actually exit.
// The gauge dropping to the target proves the surplus goroutines stopped on
// Resize-down; the deferred goleak.VerifyNone proves none of them leaked after
// Close.
func TestNoLeakAfterResizeDownAndClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := New(6, 32)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		p.Submit(func() { wg.Done() })
	}
	wg.Wait()

	p.Resize(2)
	if got := p.running.Load(); got != 2 {
		t.Fatalf("running = %d after Resize down, want 2", got)
	}
	p.Close()
	if got := p.running.Load(); got != 0 {
		t.Fatalf("running = %d after Close, want 0", got)
	}
}

// TestSubmitAfterCloseReturnsFalse pins the closed-pool contract.
func TestSubmitAfterCloseReturnsFalse(t *testing.T) {
	p := New(2, 8)
	if !p.Submit(func() {}) {
		t.Fatal("Submit before Close = false, want true")
	}
	p.Close()
	if p.Submit(func() {}) {
		t.Fatal("Submit after Close = true, want false")
	}
}
```

## Review

"Correct" here means a resize changes the worker set without ever losing work,
leaking a goroutine, or exceeding the target concurrency. Two invariants deliver
it. First, retirement priority in the worker's two-stage select: a retired worker
finishes its current job, then exits before pulling another, so shrinking retires
exactly the surplus and the jobs it would have run are drained by the survivors --
`TestResizeUpThenDownNothingDropped` proves all 300 jobs ran and
`TestInFlightJobCompletesOnShrink` proves the in-flight job completes, both via
signals rather than sleeps. Second, Resize is synchronous with the start/stop
gauge: it waits for each new worker's `started` ack on grow and each retired
worker's `done` ack on shrink, so `Size` and the gauge agree the instant Resize
returns -- `TestGaugeSettlesToSize` reads the exact target every time and
`TestPeakConcurrencyNeverExceedsSize` confirms concurrency tracks Size across the
sequence. `goleak` then proves the retired goroutines truly exit. The production
bug this prevents is the autoscaler that "shrinks" by dropping references to
workers that keep running: concurrency never actually falls, retired goroutines
leak, and under repeated scale-down the process slowly exhausts the very
downstream connections the resize was meant to release.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the channel-ownership and worker-lifecycle patterns this pool builds on.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- the `Int64` gauge that makes "exactly the surplus retired" checkable without a lock.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- goroutine-leak detection that turns "the retired workers exit" into a test assertion.
- [Effective Go: concurrency](https://go.dev/doc/effective_go#concurrency) -- the select and channel idioms behind the two-stage retire check.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-per-key-ordered-worker-pool.md](13-per-key-ordered-worker-pool.md) | Next: [../14-deadlock-detection-and-prevention/00-concepts.md](../14-deadlock-detection-and-prevention/00-concepts.md)
