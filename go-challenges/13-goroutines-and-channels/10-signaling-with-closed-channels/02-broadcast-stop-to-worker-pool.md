# Exercise 2: One Close, N Workers: Broadcasting Stop To A Pool

This is the exercise that proves the defining property of closed-channel
signaling. A pool of `N` worker goroutines all drain a shared jobs channel, and a
single `close`, done exactly once, unblocks every one of them simultaneously. A
value send would wake exactly one worker; the close wakes all `N` — which is why
`close` is the only correct fan-out stop.

The pool ships two distinct stops, each a single broadcasting close with opposite
semantics: `Shutdown()` closes the `jobs` channel so every buffered job still
runs to completion (graceful drain), and `Cancel()` closes a `quit` channel so
workers abandon buffered work and exit now (forced cancel). Confusing the two is
a classic production bug — dropping accepted jobs on a "clean" shutdown — so this
module keeps them separate and provable.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
workerpool/                  independent module: example.com/workerpool
  go.mod                     go mod init example.com/workerpool
  pool.go                    type Pool; New(n), Submit, Shutdown (drain), Cancel (forced)
  cmd/
    demo/
      main.go                runnable demo: 8 workers, submit 100 jobs, drain
  pool_test.go               forced close reaches all workers; drain loses nothing
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: a `Pool` of `n` worker goroutines each selecting over a shared `jobs` channel and a single `quit chan struct{}`; `Shutdown()` closes `jobs` once and waits so every buffered job drains, while `Cancel()` closes `quit` once to force an immediate stop.
Test: spin up 50 workers, `Cancel()`, assert every worker exits (the `WaitGroup` completes); submit 100 jobs then `Shutdown()` and assert all 100 processed with no drops; assert both stops are safe to call twice.
Verify: `go test -count=1 -race ./...`

### Two closes, two meanings: drain vs. forced cancel

Each worker runs the same loop: `select` over `jobs` and `quit`. Closing a
channel makes every receive on it immediately ready for *every* worker at once —
that is the broadcast. The subtlety is *which* channel you close, because the two
closes mean opposite things.

`Shutdown()` is graceful drain. It stops accepting new work and closes the `jobs`
channel. `quit` stays open, so in the worker's `select` the `quit` arm never
becomes ready; the only selectable arm is the receive from `jobs`, which keeps
delivering buffered jobs until the channel is drained and then reports
`ok == false`. Because the `quit` arm is never ready during a drain, the `select`
is *not* choosing pseudo-randomly between "quit now" and "process a buffered job" —
it is deterministically processing every remaining job before returning. Every
accepted job runs. That is the contract a shutdown must honor: work you already
acknowledged is not silently discarded.

`Cancel()` is forced stop. It closes `quit`. Now the `quit` arm is ready for all
`N` workers simultaneously, and — this is the point learners must internalize — if
there are still buffered jobs, each worker's `select` picks pseudo-randomly
between the ready `quit` and the ready `jobs`. Buffered jobs may be dropped. That
is *correct* for a forced cancel: the caller asked to stop now, not to finish.
Trying to assert "all jobs processed" after a `Cancel()` is a category error, and
it is exactly the nondeterminism you must never build a shutdown on.

Contrast both with the anti-pattern of `quit <- struct{}{}`. A send delivers to
exactly one receiver. To stop `N` workers you would have to send `N` times, which
means you must *know* `N` at cancel time and must send in a loop while workers are
still racing to pull jobs. Miss a send and a worker leaks. The close has none of
that fragility: it is `O(1)`, it needs no count, and it cannot under-signal. Both
`Shutdown` and `Cancel` exploit that property; they differ only in which channel
they broadcast on.

Each stop guards its close with a `sync.Once` so it is safe to call from several
paths, then blocks on `wg.Wait()`. The `WaitGroup` is the contract that every
worker actually exited — not `runtime.NumGoroutine`, which lags the scheduler.
`Submit` must never send to a closed `jobs` channel (that panics), so a
`sync.RWMutex` orders it against `Shutdown`'s close: submitters hold the read
lock across the send, `Shutdown` takes the write lock before closing, so no send
is ever in flight at the instant `jobs` closes. Concurrent submits still run in
parallel under the shared read lock; only the close is exclusive.

Set up the module:

```bash
mkdir -p ~/go-exercises/workerpool/cmd/demo
cd ~/go-exercises/workerpool
go mod init example.com/workerpool
```

Create `pool.go`:

```go
package workerpool

import (
	"sync"
	"sync/atomic"
)

// Pool runs n worker goroutines that share one jobs channel. It offers two
// broadcasting stops: Shutdown closes jobs to drain every accepted job, and
// Cancel closes quit to abandon buffered work immediately. Both use a single
// close to reach all n workers at once.
type Pool struct {
	jobs      chan func()
	quit      chan struct{}
	wg        sync.WaitGroup
	mu        sync.RWMutex
	closed    bool
	closeJobs sync.Once
	closeQuit sync.Once
	processed atomic.Int64
}

// New starts a pool of n workers. Each worker selects over jobs and quit, so a
// single close of either channel unblocks every worker simultaneously.
func New(n int) *Pool {
	p := &Pool{
		jobs: make(chan func(), n),
		quit: make(chan struct{}),
	}
	for range n {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.quit:
			// Forced cancel: abandon whatever is still buffered.
			return
		case job, ok := <-p.jobs:
			if !ok {
				// Graceful drain: jobs is closed and empty.
				return
			}
			job()
			p.processed.Add(1)
		}
	}
}

// Submit enqueues a job. It reports false once a stop has begun rather than
// panicking on a send to a closed channel. The read lock is held across the send
// so it cannot race Shutdown's close of jobs.
func (p *Pool) Submit(job func()) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case <-p.quit:
		return false
	case p.jobs <- job:
		return true
	}
}

// Shutdown is a graceful drain: it stops accepting new work, closes jobs so
// every buffered job still runs, and blocks until all workers exit. Idempotent.
func (p *Pool) Shutdown() {
	p.closeJobs.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.jobs)
		p.mu.Unlock()
	})
	p.wg.Wait()
}

// Cancel is a forced stop: it closes quit so workers abandon buffered jobs and
// exit now, then blocks until they do. Buffered jobs may be dropped. Idempotent.
func (p *Pool) Cancel() {
	p.closeQuit.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.quit)
		p.mu.Unlock()
	})
	p.wg.Wait()
}

// Processed reports how many jobs ran to completion.
func (p *Pool) Processed() int64 { return p.processed.Load() }
```

### The runnable demo

The demo submits 100 jobs into an 8-slot buffer and then calls `Shutdown()`, the
graceful drain. Because `Shutdown` closes `jobs` (not `quit`), every accepted job
runs before the workers exit, so the processed count and the sum are exact and
reproducible on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/workerpool"
)

func main() {
	p := workerpool.New(8)

	var counter int64
	var mu sync.Mutex
	for i := 1; i <= 100; i++ {
		if !p.Submit(func() {
			mu.Lock()
			counter += int64(i)
			mu.Unlock()
		}) {
			panic("submit rejected before shutdown")
		}
	}

	p.Shutdown() // graceful drain: finishes every accepted job

	fmt.Printf("workers drained, jobs processed: %d\n", p.Processed())
	fmt.Printf("sum of 1..100: %d\n", counter)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers drained, jobs processed: 100
sum of 1..100: 5050
```

### Tests

The forced-stop test starts 50 workers, submits nothing, calls `Cancel()`, and
asserts the `WaitGroup` completes — proving one `close(quit)` reached all 50. The
drain test submits 100 jobs into an 8-slot buffer, calls `Shutdown()`, and asserts
every job was processed, which is deterministic precisely because a drain never
picks the `quit` arm. Two more assert that both stops are idempotent and that
`Submit` is rejected after a stop. An `Example` pins the drain's exact count.

Create `pool_test.go`:

```go
package workerpool

import (
	"fmt"
	"sync"
	"testing"
)

func TestForcedCancelStopsAllWorkers(t *testing.T) {
	t.Parallel()

	p := New(50)
	// No jobs: every worker is parked on the select. A single close(quit)
	// must unblock all 50. If close only reached one, Cancel's wg.Wait would
	// block forever and the test would time out.
	p.Cancel()
	// Reaching here means all 50 workers returned.
}

func TestShutdownDrainsEveryJob(t *testing.T) {
	t.Parallel()

	p := New(8)
	var mu sync.Mutex
	var total int
	for i := 1; i <= 100; i++ {
		if !p.Submit(func() {
			mu.Lock()
			total += i
			mu.Unlock()
		}) {
			t.Fatalf("Submit rejected job %d before shutdown", i)
		}
	}
	p.Shutdown()

	// Deterministic: Shutdown closes jobs, never quit, so the worker select
	// never picks a "stop now" arm while jobs remain. Every accepted job runs.
	if got := p.Processed(); got != 100 {
		t.Fatalf("processed = %d, want 100 (a drain must not drop accepted jobs)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if total != 5050 {
		t.Fatalf("total = %d, want 5050", total)
	}
}

func TestStopsAreIdempotent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stop func(*Pool)
	}{
		{"shutdown twice", func(p *Pool) { p.Shutdown(); p.Shutdown() }},
		{"cancel twice", func(p *Pool) { p.Cancel(); p.Cancel() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := New(4)
			tc.stop(p) // second close must not panic
		})
	}
}

func TestSubmitAfterStopRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stop func(*Pool)
	}{
		{"after shutdown", (*Pool).Shutdown},
		{"after cancel", (*Pool).Cancel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := New(4)
			tc.stop(p)
			if p.Submit(func() {}) {
				t.Fatal("Submit returned true after stop; want false")
			}
		})
	}
}

func ExamplePool_drain() {
	p := New(4)
	for range 10 {
		p.Submit(func() {})
	}
	p.Shutdown()
	fmt.Println(p.Processed())
	// Output: 10
}
```

## Review

The pool is correct when a single close provably terminates all `N` workers and
each stop honors its stated semantics. `Cancel()` is proven by the 50-worker test:
it would hang if `close(quit)` reached fewer than all of them, so its completion
is the proof of broadcast. `Shutdown()` is proven by the drain test: because it
closes `jobs` and leaves `quit` open, the worker `select` never takes a stop arm
while jobs remain, so `Processed() == 100` is deterministic — a drain that dropped
jobs would be the bug. `Submit` is correct when it never panics; sending to a
closed channel would, which is why the `RWMutex` orders every send against
`Shutdown`'s close and `Submit` re-checks `closed` under the lock. The
anti-pattern to remember is `quit <- struct{}{}` in a loop: it forces you to know
`N`, races the workers, and leaks any worker you fail to signal. Run
`go test -race` to confirm the shared counter and the two closes are properly
synchronized.

## Resources

- [The Go Programming Language Specification: Close](https://go.dev/ref/spec#Close) — receiving from a closed channel is always ready, which is what broadcasts to all workers.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the reliable proof that every worker exited.
- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) — the idempotent-close guard.
- [pkg.go.dev: sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — orders concurrent Submits against the exclusive close of the jobs channel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-readiness-gate.md](03-readiness-gate.md)
