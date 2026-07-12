# Exercise 2: A Job-Processing Service With Graceful Shutdown

A reusable pool is a building block; a service is what you deploy. This exercise
turns the pool into a long-lived `Service` that accepts jobs from many callers,
processes them on a bounded set of workers, reports each job's result or error on
a results channel, and tracks lifetime metrics. The senior concern it solves is
shutdown: when the service is told to stop, it must reject new work but finish
every job it already accepted — queued or in-flight — before it closes its
results channel and returns the final counts. Getting that drain correct without
a "send on closed channel" panic is the heart of the exercise.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
service.go               Job, Result, Metrics, Snapshot, Service, New, Submit,
                         Results, Shutdown (RWMutex-guarded drain)
cmd/
  demo/
    main.go              submit 6 jobs (half fail), collect sorted results + metrics
service_test.go          processes all jobs, counts success/failure, drains in-flight,
                         rejects post-shutdown submits, race-clean concurrent submit,
                         idempotent shutdown
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `Job`, `Result`, `Metrics`, `Snapshot`, `Service`, `New(workers, queue int) *Service`, `(*Service).Submit(Job) bool`, `(*Service).Results() <-chan Result`, and `(*Service).Shutdown() Snapshot`.
- Test: `service_test.go` asserts every accepted job is processed, that the metrics count successes and failures correctly, that `Shutdown` drains in-flight work rather than dropping it, that `Submit` returns false after shutdown without bumping the counter, that concurrent submits are race-clean with no lost jobs, and that `Shutdown` is idempotent.
- Verify: `go test -race ./...`

### The shutdown race, and the RWMutex that closes it

The dangerous moment in any job service is shutdown. Workers read from an input
channel with `for job := range s.jobs`, so the way to make them stop is to close
that channel: each worker drains whatever is queued and then its loop ends. But
`Submit` sends on the same channel, and a `Submit` blocked on a full queue is
sitting on a send when shutdown wants to close. Close a channel out from under a
pending send and the program panics with "send on closed channel"; do the check
and the send without synchronisation and the race detector flags it even when no
panic happens to fire.

The fix is a reader-writer lock used asymmetrically. `Submit` takes a read lock
across its entire check-and-send: it verifies the service is not closed, bumps
the submitted counter, and sends — all while holding the read lock. `Shutdown`
takes the write lock before it sets the closed flag and closes the channel.
Because a write lock cannot be acquired while any read lock is held, the close
can never interleave with a send: any `Submit` already past its closed check is
holding the read lock, so `Shutdown` blocks until that send completes and the
lock is released, and any `Submit` that arrives after the flag is set sees
`closed == true` and returns false without ever touching the channel. A `Submit`
blocked on a full queue still holds only a read lock, and the workers keep
draining the queue, so that send eventually completes and shutdown proceeds —
there is no deadlock, only correct ordering.

After the channel is closed, `Shutdown` waits on the worker WaitGroup so every
in-flight handler runs to completion, then closes the results channel so the
caller's collector loop ends, then returns the final metrics snapshot. The
ordering is the contract: reject, drain, join, close, report.

Create `service.go`:

```go
package jobservice

import (
	"sync"
	"sync/atomic"
)

// Job is a unit of work submitted to the Service. Handler runs the work and
// returns a result string or an error; ID labels the job in its Result.
type Job struct {
	ID      string
	Handler func() (string, error)
}

// Result reports the outcome of one Job. Err is nil on success.
type Result struct {
	JobID string
	Value string
	Err   error
}

// Metrics holds lifetime counters. Workers update them concurrently through
// atomic operations; read a consistent view with Snapshot rather than touching
// the fields directly.
type Metrics struct {
	submitted atomic.Int64
	succeeded atomic.Int64
	failed    atomic.Int64
}

// Snapshot is an immutable copy of the counters at one instant.
type Snapshot struct {
	Submitted int64
	Succeeded int64
	Failed    int64
}

// Snapshot loads each counter and returns a consistent copy.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		Submitted: m.submitted.Load(),
		Succeeded: m.succeeded.Load(),
		Failed:    m.failed.Load(),
	}
}

// Service is a bounded worker pool that processes Jobs and reports each outcome
// on its Results channel. Submit is safe for concurrent use. Shutdown drains
// every accepted job before the Results channel is closed.
type Service struct {
	jobs    chan Job
	results chan Result
	wg      sync.WaitGroup
	metrics Metrics

	mu     sync.RWMutex
	closed bool
}

// New starts a Service with the given number of worker goroutines and an input
// queue of the given capacity. A workers count below 1 is normalised to 1 and a
// negative queue to 0.
func New(workers, queue int) *Service {
	if workers < 1 {
		workers = 1
	}
	if queue < 0 {
		queue = 0
	}
	s := &Service{
		jobs:    make(chan Job, queue),
		results: make(chan Result, queue),
	}
	s.wg.Add(workers)
	for range workers {
		go s.worker()
	}
	return s
}

func (s *Service) worker() {
	defer s.wg.Done()
	for job := range s.jobs {
		value, err := job.Handler()
		if err != nil {
			s.metrics.failed.Add(1)
		} else {
			s.metrics.succeeded.Add(1)
		}
		s.results <- Result{JobID: job.ID, Value: value, Err: err}
	}
}

// Results returns the channel on which each job's Result is delivered. The
// caller must drain it concurrently; it is closed once Shutdown has drained
// every accepted job.
func (s *Service) Results() <-chan Result {
	return s.results
}

// Submit enqueues a job and returns true once it is accepted. It returns false
// without enqueuing if the Service is shutting down. Submit blocks while the
// input queue is full, providing backpressure.
func (s *Service) Submit(j Job) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	s.metrics.submitted.Add(1)
	s.jobs <- j
	return true
}

// Shutdown stops accepting new jobs, waits for every accepted job (queued and
// in-flight) to finish, closes the Results channel, and returns the final
// metrics. It is idempotent: a second call waits for the drain already in
// progress and returns the same final snapshot without closing anything twice.
func (s *Service) Shutdown() Snapshot {
	s.mu.Lock()
	first := !s.closed
	s.closed = true
	if first {
		close(s.jobs)
	}
	s.mu.Unlock()

	s.wg.Wait()
	if first {
		close(s.results)
	}
	return s.metrics.Snapshot()
}
```

The metrics discipline is visible in the worker: every outcome goes through an
atomic `Add`, never a bare `++`, and the only way to read the counters is the
`Snapshot` method that `Load`s each field. That is what keeps the counts correct
and race-free while many workers update them at once. Note also that `Submit`
increments `submitted` only on the accepted path, so once the pool has drained,
`submitted` equals `succeeded + failed` exactly — an invariant the tests assert.

### The runnable demo

This demo submits six jobs to a four-worker service. The even-numbered jobs
succeed and the odd-numbered ones return an error, so the metrics show three
successes and three failures. A collector goroutine drains the results channel
concurrently with submission; after `Shutdown` returns and the collector has
finished, the results are sorted by job ID for a stable printout. As in exercise
1, the sort is what makes the output deterministic despite concurrent workers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"

	"example.com/job-processing-service"
)

func main() {
	svc := jobservice.New(4, 8)

	var mu sync.Mutex
	var results []jobservice.Result
	done := make(chan struct{})
	go func() {
		for r := range svc.Results() {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}
		close(done)
	}()

	for i := range 6 {
		id := fmt.Sprintf("job-%d", i)
		even := i%2 == 0
		svc.Submit(jobservice.Job{
			ID: id,
			Handler: func() (string, error) {
				if !even {
					return "", fmt.Errorf("handler failed")
				}
				return "processed " + id, nil
			},
		})
	}

	metrics := svc.Shutdown()
	<-done

	sort.Slice(results, func(i, j int) bool { return results[i].JobID < results[j].JobID })
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("%s: error: %v\n", r.JobID, r.Err)
		} else {
			fmt.Printf("%s: %s\n", r.JobID, r.Value)
		}
	}
	fmt.Printf("submitted=%d succeeded=%d failed=%d\n",
		metrics.Submitted, metrics.Succeeded, metrics.Failed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job-0: processed job-0
job-1: error: handler failed
job-2: processed job-2
job-3: error: handler failed
job-4: processed job-4
job-5: error: handler failed
submitted=6 succeeded=3 failed=3
```

### Tests

The tests exercise each guarantee directly. `TestProcessesAllSubmittedJobs`
submits a batch and checks every result comes back with the submitted count
equal to successes plus failures. `TestMetricsCountSuccessAndFailure` makes a
known fraction fail and checks the split. `TestShutdownDrainsInFlight` submits
slow jobs and asserts none are dropped by the shutdown. `TestSubmitAfterShutdown`
checks a post-shutdown submit returns false and does not bump the counter.
`TestConcurrentSubmitNoLoss` floods the service from many goroutines and verifies
every job is processed under `-race`. `TestShutdownIdempotent` calls `Shutdown`
twice and confirms the second call neither panics nor changes the result. The
`collect` helper drains the results channel in the background and delivers the
gathered slice once it closes.

Create `service_test.go`:

```go
package jobservice

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func collect(s *Service) <-chan []Result {
	out := make(chan []Result, 1)
	go func() {
		var got []Result
		for r := range s.Results() {
			got = append(got, r)
		}
		out <- got
	}()
	return out
}

func TestProcessesAllSubmittedJobs(t *testing.T) {
	t.Parallel()

	svc := New(4, 8)
	results := collect(svc)

	const n = 50
	for i := range n {
		id := fmt.Sprintf("job-%d", i)
		svc.Submit(Job{ID: id, Handler: func() (string, error) {
			return "ok:" + id, nil
		}})
	}

	snap := svc.Shutdown()
	got := <-results

	if len(got) != n {
		t.Fatalf("got %d results, want %d", len(got), n)
	}
	if snap.Submitted != int64(n) {
		t.Fatalf("submitted = %d, want %d", snap.Submitted, n)
	}
	if snap.Succeeded+snap.Failed != snap.Submitted {
		t.Fatalf("succeeded+failed = %d, want %d", snap.Succeeded+snap.Failed, snap.Submitted)
	}
}

func TestMetricsCountSuccessAndFailure(t *testing.T) {
	t.Parallel()

	svc := New(3, 4)
	results := collect(svc)

	const n = 20
	for i := range n {
		fail := i%2 == 0
		svc.Submit(Job{ID: fmt.Sprintf("j-%d", i), Handler: func() (string, error) {
			if fail {
				return "", fmt.Errorf("boom")
			}
			return "ok", nil
		}})
	}

	snap := svc.Shutdown()
	<-results

	if snap.Succeeded != n/2 || snap.Failed != n/2 {
		t.Fatalf("succeeded=%d failed=%d, want %d each", snap.Succeeded, snap.Failed, n/2)
	}
}

func TestShutdownDrainsInFlight(t *testing.T) {
	t.Parallel()

	svc := New(2, 16)
	results := collect(svc)

	const n = 16
	for i := range n {
		svc.Submit(Job{ID: fmt.Sprintf("slow-%d", i), Handler: func() (string, error) {
			time.Sleep(2 * time.Millisecond)
			return "done", nil
		}})
	}

	snap := svc.Shutdown()
	got := <-results

	if len(got) != n {
		t.Fatalf("drained %d results, want %d (in-flight work was dropped)", len(got), n)
	}
	if snap.Succeeded != int64(n) {
		t.Fatalf("succeeded = %d, want %d", snap.Succeeded, n)
	}
}

func TestSubmitAfterShutdown(t *testing.T) {
	t.Parallel()

	svc := New(2, 2)
	results := collect(svc)

	svc.Submit(Job{ID: "a", Handler: func() (string, error) { return "ok", nil }})
	snap := svc.Shutdown()
	<-results

	if svc.Submit(Job{ID: "late", Handler: func() (string, error) { return "ok", nil }}) {
		t.Fatal("Submit after Shutdown returned true, want false")
	}
	if after := svc.metrics.Snapshot(); after.Submitted != snap.Submitted {
		t.Fatalf("rejected submit changed counter: %d -> %d", snap.Submitted, after.Submitted)
	}
}

func TestConcurrentSubmitNoLoss(t *testing.T) {
	t.Parallel()

	svc := New(8, 32)
	results := collect(svc)

	const goroutines = 16
	const perG = 25
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perG {
				svc.Submit(Job{
					ID:      fmt.Sprintf("g%d-%d", g, i),
					Handler: func() (string, error) { return "ok", nil },
				})
			}
		}()
	}
	wg.Wait()

	snap := svc.Shutdown()
	got := <-results

	want := goroutines * perG
	if len(got) != want {
		t.Fatalf("processed %d jobs, want %d", len(got), want)
	}
	if snap.Submitted != int64(want) {
		t.Fatalf("submitted = %d, want %d", snap.Submitted, want)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	t.Parallel()

	svc := New(2, 4)
	results := collect(svc)

	svc.Submit(Job{ID: "x", Handler: func() (string, error) { return "ok", nil }})
	first := svc.Shutdown()
	second := svc.Shutdown()
	<-results

	if first != second {
		t.Fatalf("idempotent Shutdown returned different snapshots: %+v vs %+v", first, second)
	}
}
```

`TestShutdownDrainsInFlight` is the one that would catch a shutdown that cancels
rather than drains: each handler sleeps, so at the moment `Shutdown` is called
several jobs are mid-flight and several are still queued. A correct shutdown
waits for all sixteen; a shutdown that abandoned in-flight work would return
fewer results and a lower success count.

## Review

The service is correct when the shutdown sequence holds: reject new submits,
close the input so workers drain the queue, `Wait` for the workers, close the
results, report. The RWMutex is what makes "reject" and "close" safe against an
in-progress `Submit` — the write lock cannot be taken while a send holds the read
lock, so the channel is never closed under a pending send, which the concurrent
submit test confirms under `-race`. Metrics are correct because every outcome is
an atomic `Add` and every read is a `Snapshot`; the submitted-equals-succeeded-
plus-failed invariant holds precisely because `Submit` counts only the accepted
path. Idempotence comes from the `first` flag: only the first `Shutdown` closes
the two channels, while a second call still waits on the already-completed group
and returns the same snapshot.

Common mistakes for this feature. Closing `s.jobs` without the write lock races a
pending `Submit` and panics; the lock asymmetry is the whole point. Forgetting to
drain `Results()` makes the workers block on their sends, so `wg.Wait()` in
`Shutdown` never returns and the program deadlocks — the `collect` helper exists
to model the required consumer. Incrementing a counter with `++` instead of
`atomic.Add` is a data race the detector catches immediately. And cancelling
in-flight work on shutdown instead of draining it silently drops accepted jobs,
which the slow-handler test is designed to expose.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock whose asymmetry makes the shutdown-versus-submit race safe.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` and its `Add`/`Load` operations, the basis for race-free metrics.
- [Go Memory Model](https://go.dev/ref/mem) — why unsynchronised counter increments are a data race and what `atomic` and mutexes guarantee instead.

---

Back to [01-generic-worker-pool.md](01-generic-worker-pool.md) | Next: [03-notification-dispatch-pool.md](03-notification-dispatch-pool.md)
