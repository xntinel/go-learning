# Exercise 16: Background Job Runner with Goroutine Literals and Heartbeat Tracking

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A background job runner needs more than "did the jobs finish" — an operator wants
proof that every job actually started and stopped, and when. This module builds a
bounded runner that launches one goroutine literal per job, passes the job's data as
an argument, and records a heartbeat (start time, finish time, error) for every job
so exactly-once execution is provable, not assumed.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
heartbeat/                    module example.com/heartbeat
  go.mod
  heartbeat.go                 Job, Beat, Runner; New, Run
  heartbeat_test.go            exactly-once heartbeats, worker limit, per-job errors, default workers
  cmd/demo/main.go             10 jobs across 3 workers
```

- Files: `heartbeat.go`, `heartbeat_test.go`, `cmd/demo/main.go`.
- Implement: `Runner.Run` launching one goroutine per job via a function literal that takes the job's index and value as arguments, a buffered-channel semaphore bounding concurrency, a mutex-guarded `map[int]Beat` recording each job's start time, finish time, and error, and an injectable clock so heartbeats are deterministic in tests.
- Test: every job gets exactly one heartbeat; concurrency never exceeds the worker limit (atomic peak tracking); each job's error is recorded correctly; an invalid worker count defaults to one. Under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Passing data by argument, tracking liveness by clock

`Run` launches one goroutine per job, and the goroutine literal takes both the job's
index and its `Job` value as arguments — `go func(idx int, j Job) { ... }(i, job)` —
rather than capturing the loop variables. Go 1.22 made a bare capture of `i` and
`job` safe too, but passing them as arguments keeps the goroutine's inputs explicit
regardless of where they came from, which matters once a caller wants to derive
`idx` and `job` from something other than a simple `for range`.

The heartbeat itself is the new piece: each goroutine stamps a start time before
running the job and a finish time after, then writes both — along with the job's
error — into a shared map. That map is guarded by a `sync.Mutex` because concurrent
writes to a Go map are unsafe even when every goroutine touches a different key; the
map itself carries no per-key locking. The clock used to stamp times is injected
(`now func() time.Time`) rather than called as `time.Now()` directly, so tests can
supply a fake, deterministically increasing clock and assert `Finished` never
precedes `Started` without depending on real wall-clock time.

Create `heartbeat.go`:

```go
package heartbeat

import (
	"sync"
	"time"
)

// Job is a unit of background work that reports success or failure.
type Job func() error

// Beat records when a job started and finished, and how it ended.
type Beat struct {
	Started  time.Time
	Finished time.Time
	Err      error
}

// Runner executes jobs concurrently, bounded to a fixed number of workers,
// and tracks a heartbeat for every job so a caller can prove each one ran
// exactly once.
type Runner struct {
	workers int
	now     func() time.Time
}

// New returns a Runner bounded to workers concurrent goroutines (at least
// one). now is the clock used to stamp heartbeats; pass time.Now in
// production and a fake, incrementing clock in tests so results stay
// deterministic.
func New(workers int, now func() time.Time) *Runner {
	if workers <= 0 {
		workers = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Runner{workers: workers, now: now}
}

// Run launches one goroutine per job via a function literal, passing the
// job's index and Job value as arguments so each goroutine owns its own
// copy. A buffered-channel semaphore bounds concurrency to r.workers. Every
// job's heartbeat — start time, finish time, and error — is recorded in the
// returned map exactly once, guarded by a mutex because concurrent map
// writes are unsafe even to distinct keys.
func (r *Runner) Run(jobs []Job) map[int]Beat {
	beats := make(map[int]Beat, len(jobs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, r.workers)

	for i, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, j Job) {
			defer wg.Done()
			defer func() { <-sem }()

			start := r.now()
			err := j()
			finish := r.now()

			mu.Lock()
			beats[idx] = Beat{Started: start, Finished: finish, Err: err}
			mu.Unlock()
		}(i, job)
	}
	wg.Wait()
	return beats
}
```

### The runnable demo

The demo runs ten no-op jobs across three workers using the real clock, then checks
that every job produced a heartbeat with a finish time no earlier than its start
time. Only the summary is printed, so the output is deterministic even though the
real timestamps are not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/heartbeat"
)

func main() {
	jobs := make([]heartbeat.Job, 10)
	for i := range jobs {
		jobs[i] = func() error {
			return nil
		}
	}

	r := heartbeat.New(3, time.Now)
	beats := r.Run(jobs)

	allRecorded := len(beats) == len(jobs)
	for _, b := range beats {
		if b.Finished.Before(b.Started) {
			allRecorded = false
		}
	}

	fmt.Printf("processed %d jobs with %d workers\n", len(jobs), 3)
	fmt.Println("all heartbeats recorded:", allRecorded)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 10 jobs with 3 workers
all heartbeats recorded: true
```

### Tests

`TestRunRecordsEveryJobExactlyOnce` uses a fake, strictly increasing clock and
checks the returned map has one entry per job with `Finished >= Started` and no
error. `TestRunRespectsWorkerLimit` is the load-bearing concurrency test: each job
bumps an atomic counter on entry, tracks the peak with a compare-and-swap loop, and
decrements on exit, asserting the peak never exceeds the worker count.
`TestRunRecordsPerJobErrors` checks each job's own error lands in its own heartbeat.
`TestNewDefaultsInvalidWorkersToOne` guards the zero-worker fallback.

Create `heartbeat_test.go`:

```go
package heartbeat

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock returns a strictly increasing time on every call, backed by an
// atomic counter. It lets tests assert ordering (finish >= start) without
// depending on real wall-clock time or sleeping.
func fakeClock() func() time.Time {
	var tick int64
	epoch := time.Unix(0, 0)
	return func() time.Time {
		n := atomic.AddInt64(&tick, 1)
		return epoch.Add(time.Duration(n) * time.Millisecond)
	}
}

func TestRunRecordsEveryJobExactlyOnce(t *testing.T) {
	t.Parallel()

	const jobCount = 50
	jobs := make([]Job, jobCount)
	for i := range jobs {
		jobs[i] = func() error { return nil }
	}

	beats := New(6, fakeClock()).Run(jobs)
	if len(beats) != jobCount {
		t.Fatalf("beats = %d, want %d", len(beats), jobCount)
	}
	for idx, b := range beats {
		if b.Finished.Before(b.Started) {
			t.Fatalf("job %d: finished %v before started %v", idx, b.Finished, b.Started)
		}
		if b.Err != nil {
			t.Fatalf("job %d: err = %v, want nil", idx, b.Err)
		}
	}
}

func TestRunRespectsWorkerLimit(t *testing.T) {
	t.Parallel()

	const workers = 4
	const jobCount = 40
	var concurrent, peak int64
	jobs := make([]Job, jobCount)
	for i := range jobs {
		jobs[i] = func() error {
			cur := atomic.AddInt64(&concurrent, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			atomic.AddInt64(&concurrent, -1)
			return nil
		}
	}

	New(workers, fakeClock()).Run(jobs)
	if got := atomic.LoadInt64(&peak); got > workers {
		t.Fatalf("peak concurrency = %d, want <= %d", got, workers)
	}
}

func TestRunRecordsPerJobErrors(t *testing.T) {
	t.Parallel()

	want := errors.New("job failed")
	jobs := []Job{
		func() error { return nil },
		func() error { return want },
	}

	beats := New(2, fakeClock()).Run(jobs)
	if beats[0].Err != nil {
		t.Fatalf("beats[0].Err = %v, want nil", beats[0].Err)
	}
	if !errors.Is(beats[1].Err, want) {
		t.Fatalf("beats[1].Err = %v, want %v", beats[1].Err, want)
	}
}

func TestNewDefaultsInvalidWorkersToOne(t *testing.T) {
	t.Parallel()
	r := New(0, fakeClock())
	if r.workers != 1 {
		t.Fatalf("workers = %d, want 1", r.workers)
	}
}
```

## Review

The runner is correct when three things hold under `-race`: every job produces
exactly one heartbeat, concurrency never exceeds the worker count, and each
heartbeat's own error matches its own job. The heartbeat map is where a real bug
would first show up — writing to it without the mutex would race even though every
goroutine writes a distinct key, because Go maps carry no internal synchronization
at all. The injected clock is what keeps the ordering assertion (`Finished >=
Started`) deterministic; a version that called `time.Now()` directly would still be
correct in production but would make the test's pass/fail depend on real scheduling
jitter instead of the logic being tested.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [sync/atomic](https://pkg.go.dev/sync/atomic)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-functional-filter-map-reduce-pipeline.md](15-functional-filter-map-reduce-pipeline.md) | Next: [17-iife-quota-inline.md](17-iife-quota-inline.md)
