# Exercise 9: Observability — Queue Depth, In-Flight, Throughput, and Latency

Operators cannot run a scheduler they cannot see. This module gives the scheduler
a production observability surface: race-free counters (submitted, completed,
failed, retried), an in-flight gauge derived as started minus finished, queue
depth, and per-task latency — exposed both as a `Stats` snapshot and via `expvar`
for scraping. It upgrades the preserved `Stats` into an operator-grade view whose
gauge stays coherent under `-race`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
scheduler-metrics/             module example.com/scheduler-metrics
  go.mod                       go 1.25
  scheduler.go                 atomic counters; Stats snapshot; expvar.Publish
  cmd/
    demo/
      main.go                  demo: run a workload, print the counters
  scheduler_test.go            exact counters, gauge to zero, expvar, concurrent Stats, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: atomic counters (`submitted`/`completed`/`failed`/`retried`), an in-flight gauge as `started − finished`, `queueDepth`, per-task latency via `time.Since`, a `Stats` snapshot from atomic loads, and an `expvar` publication.
Test: a known workload (50 tasks, 5 forced failures, some retried) yields exact counters (submitted=50, completed=45, failed=5); the in-flight gauge returns to 0 after drain and is > 0 while tasks are gated; latency is positive; concurrent `Stats()` reads during submission are consistent under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/scheduler-metrics/cmd/demo
cd ~/go-exercises/scheduler-metrics
go mod init example.com/scheduler-metrics
go mod edit -go=1.25
```

### Coherent counters, a derived gauge, and expvar

Two rules make metrics trustworthy under concurrency. First, every counter is a
`sync/atomic.Int64`, so a concurrent `Stats()` read never observes a torn value.
Second, the in-flight gauge is *derived* — `started − finished` from two monotonic
atomics — not tracked as a single counter that is incremented at start and
decremented "somewhere". A single mutable gauge drifts under races and early
returns and can even read negative; a difference of two monotonic counters cannot.
Latency is captured with `time.Since(submittedAt)` at completion, recording the
last sample and the running total.

Note the ordering that keeps the gauge honest: `finished` is incremented *before*
the result is delivered on the task's channel. If it were deferred until after the
send, a caller that read its result and immediately called `Stats()` could see the
task still counted as in-flight. Incrementing `finished` before the send makes
"the caller has the result" imply "the gauge no longer counts this task".

`expvar` publishes the snapshot under a unique per-scheduler name (an atomic
sequence avoids the "reuse of exported var name" panic when a process builds
several schedulers). The published `expvar.Func` calls `Stats()` at scrape time,
so a metrics agent hitting `/debug/vars` always reads live values.

Create `scheduler.go`:

```go
package scheduler

import (
	"errors"
	"expvar"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrShuttingDown is delivered when Submit races Stop.
var ErrShuttingDown = errors.New("scheduler shutting down")

var schedulerSeq atomic.Int64

type Task func() (any, error)

type Result struct {
	Value any
	Err   error
}

type task struct {
	fn          Task
	done        chan Result
	submittedAt time.Time
}

// Stats is a coherent snapshot of the scheduler's counters.
type Stats struct {
	Submitted   int64
	Completed   int64
	Failed      int64
	Retried     int64
	InFlight    int64
	QueueDepth  int64
	LastLatency time.Duration
}

// Scheduler runs tasks and exports operator-grade metrics.
type Scheduler struct {
	name        string
	maxAttempts int

	tasks    chan task
	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool

	submitted    atomic.Int64
	completed    atomic.Int64
	failed       atomic.Int64
	retried      atomic.Int64
	started      atomic.Int64
	finished     atomic.Int64
	queueDepth   atomic.Int64
	lastLatency  atomic.Int64 // nanoseconds
	totalLatency atomic.Int64 // nanoseconds
}

// New starts a Scheduler with the given workers and per-task attempt limit, and
// publishes its metrics via expvar under a unique name.
func New(workers, maxAttempts int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	s := &Scheduler{
		name:        fmt.Sprintf("scheduler_%d", schedulerSeq.Add(1)),
		maxAttempts: maxAttempts,
		tasks:       make(chan task, workers*2),
		quit:        make(chan struct{}),
	}
	expvar.Publish(s.name, expvar.Func(func() any { return s.Stats() }))
	for range workers {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		select {
		case t := <-s.tasks:
			s.process(t)
		case <-s.quit:
			return
		}
	}
}

func (s *Scheduler) process(t task) {
	s.queueDepth.Add(-1)
	s.started.Add(1)

	var (
		v   any
		err error
	)
	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		if attempt > 1 {
			s.retried.Add(1)
		}
		v, err = t.fn()
		if err == nil {
			break
		}
	}

	lat := time.Since(t.submittedAt).Nanoseconds()
	s.lastLatency.Store(lat)
	s.totalLatency.Add(lat)
	if err == nil {
		s.completed.Add(1)
	} else {
		s.failed.Add(1)
	}

	// Increment finished BEFORE delivering, so a caller that reads its result and
	// then calls Stats() never sees this task still counted as in-flight.
	s.finished.Add(1)
	t.done <- Result{Value: v, Err: err}
}

// Submit enqueues fn and returns a capacity-1 result channel.
func (s *Scheduler) Submit(fn Task) <-chan Result {
	done := make(chan Result, 1)

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		done <- Result{Err: ErrShuttingDown}
		return done
	}

	s.queueDepth.Add(1)
	select {
	case s.tasks <- task{fn: fn, done: done, submittedAt: time.Now()}:
		s.submitted.Add(1)
	case <-s.quit:
		s.queueDepth.Add(-1)
		done <- Result{Err: ErrShuttingDown}
	}
	return done
}

// Stats returns a coherent snapshot built from atomic loads.
func (s *Scheduler) Stats() Stats {
	started := s.started.Load()
	finished := s.finished.Load()
	return Stats{
		Submitted:   s.submitted.Load(),
		Completed:   s.completed.Load(),
		Failed:      s.failed.Load(),
		Retried:     s.retried.Load(),
		InFlight:    started - finished,
		QueueDepth:  s.queueDepth.Load(),
		LastLatency: time.Duration(s.lastLatency.Load()),
	}
}

// ExpvarName is the name under which this scheduler's metrics are published.
func (s *Scheduler) ExpvarName() string { return s.name }

// Stop signals the workers and joins them.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.quit) })
	s.wg.Wait()
}
```

### The runnable demo

The demo runs ten tasks, two of which fail (and retry), and prints the final
counters.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/scheduler-metrics"
)

func main() {
	s := scheduler.New(4, 2)
	defer s.Stop()

	var dones []<-chan scheduler.Result
	for i := range 10 {
		dones = append(dones, s.Submit(func() (any, error) {
			if i%5 == 0 {
				return nil, fmt.Errorf("task %d failed", i)
			}
			return i, nil
		}))
	}
	for _, d := range dones {
		<-d
	}

	st := s.Stats()
	fmt.Printf("submitted=%d completed=%d failed=%d retried=%d inflight=%d\n",
		st.Submitted, st.Completed, st.Failed, st.Retried, st.InFlight)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submitted=10 completed=8 failed=2 retried=2 inflight=0
```

### Tests

`TestCountersExact` runs 50 tasks with 5 forced failures and asserts every counter
exactly. `TestInFlightGauge` asserts the gauge is 3 while three tasks are gated
open and 0 after drain. `TestExpvarPublished` asserts the metrics are scrapeable.
`TestConcurrentStats` reads `Stats()` in a tight loop during active submission to
prove atomic reads stay consistent under `-race`.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"errors"
	"expvar"
	"fmt"
	"strings"
	"testing"
)

func TestCountersExact(t *testing.T) {
	t.Parallel()

	s := New(4, 2) // maxAttempts 2, so a failing task retries once
	defer s.Stop()

	const n = 50
	var dones []<-chan Result
	for i := range n {
		dones = append(dones, s.Submit(func() (any, error) {
			if i%10 == 0 { // i = 0,10,20,30,40 -> exactly 5 failures
				return nil, errors.New("forced failure")
			}
			return i, nil
		}))
	}
	for _, d := range dones {
		<-d
	}

	st := s.Stats()
	if st.Submitted != 50 {
		t.Fatalf("Submitted = %d, want 50", st.Submitted)
	}
	if st.Completed != 45 {
		t.Fatalf("Completed = %d, want 45", st.Completed)
	}
	if st.Failed != 5 {
		t.Fatalf("Failed = %d, want 5", st.Failed)
	}
	if st.Retried != 5 { // each of the 5 failing tasks retried once
		t.Fatalf("Retried = %d, want 5", st.Retried)
	}
	if st.InFlight != 0 {
		t.Fatalf("InFlight after drain = %d, want 0", st.InFlight)
	}
	if st.QueueDepth != 0 {
		t.Fatalf("QueueDepth after drain = %d, want 0", st.QueueDepth)
	}
	if st.LastLatency <= 0 {
		t.Fatalf("LastLatency = %v, want > 0", st.LastLatency)
	}
}

func TestInFlightGauge(t *testing.T) {
	t.Parallel()

	s := New(4, 1)
	defer s.Stop()

	gate := make(chan struct{})
	started := make(chan struct{}, 3)
	var dones []<-chan Result
	for range 3 {
		dones = append(dones, s.Submit(func() (any, error) {
			started <- struct{}{}
			<-gate
			return nil, nil
		}))
	}
	for range 3 {
		<-started
	}

	if g := s.Stats().InFlight; g != 3 {
		t.Fatalf("InFlight while gated = %d, want 3", g)
	}

	close(gate)
	for _, d := range dones {
		<-d
	}
	if g := s.Stats().InFlight; g != 0 {
		t.Fatalf("InFlight after drain = %d, want 0", g)
	}
}

func TestExpvarPublished(t *testing.T) {
	t.Parallel()

	s := New(2, 1)
	defer s.Stop()

	<-s.Submit(func() (any, error) { return "x", nil })

	v := expvar.Get(s.ExpvarName())
	if v == nil {
		t.Fatalf("expvar var %q not published", s.ExpvarName())
	}
	if got := v.String(); !strings.Contains(got, `"Submitted":1`) {
		t.Fatalf("expvar payload = %s, want it to contain Submitted:1", got)
	}
}

func TestConcurrentStats(t *testing.T) {
	t.Parallel()

	s := New(8, 1)
	defer s.Stop()

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Stats() // must never read a torn snapshot
			}
		}
	}()

	var dones []<-chan Result
	for i := range 200 {
		dones = append(dones, s.Submit(func() (any, error) { return i, nil }))
	}
	for _, d := range dones {
		<-d
	}
	close(stop)

	if st := s.Stats(); st.Submitted != 200 || st.Completed != 200 {
		t.Fatalf("Submitted=%d Completed=%d, want 200/200", st.Submitted, st.Completed)
	}
}

func Example() {
	s := New(1, 1)
	defer s.Stop()

	<-s.Submit(func() (any, error) { return "ok", nil })
	st := s.Stats()
	fmt.Println(st.Submitted, st.Completed, st.Failed, st.InFlight)
	// Output: 1 1 0 0
}
```

## Review

The metrics are correct when they are both race-free and semantically coherent.
Race-free comes from `sync/atomic.Int64` counters and a `Stats()` that only does
atomic loads, so a concurrent observer — including the tight-loop reader in
`TestConcurrentStats` — never sees a torn value. Coherent comes from deriving the
in-flight gauge as `started − finished`, which cannot drift or go negative, and
from incrementing `finished` before delivering the result so a gauge read after a
result read never double-counts. Latency is a real `time.Since` sample, and
`expvar` exposes the live snapshot for scraping. The failure this design rules out
is a single mutable gauge that disagrees with itself under load — worse than no
metric, because it sends the operator chasing a phantom. Run `go test -race
-count=1 ./...`.

## Resources

- [`expvar`](https://pkg.go.dev/expvar) — publishing metrics at `/debug/vars`, including `expvar.Func`.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` for torn-read-free counters.
- [`time.Since`](https://pkg.go.dev/time#Since) — measuring per-task latency.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-weighted-concurrency-semaphore.md](08-weighted-concurrency-semaphore.md) | Next: [10-outbox-microbatch-size-or-linger-flush.md](10-outbox-microbatch-size-or-linger-flush.md)
