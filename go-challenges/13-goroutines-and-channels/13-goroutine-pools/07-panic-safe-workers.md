# Exercise 7: One Panicking Job Must Not Kill A Worker

A panic in a worker goroutine is not caught by whoever submitted the job — it
crashes the whole process, taking every other in-flight job with it. One malformed
input should not be a fleet-wide outage. This exercise builds a resilient worker
loop that wraps each job in a deferred `recover`, converting a panic into an error
(with a stack trace) reported through an `onErr` hook, so the worker survives and
the pool keeps its full concurrency.

This module is fully self-contained.

## What you'll build

```text
safepool/                  independent module: example.com/safepool
  go.mod                   go 1.25
  pool.go                  type Pool; New(workers, onErr), Submit, Size, Close;
                           safeRun with recover; ErrJobPanic sentinel
  cmd/
    demo/
      main.go              runnable demo: normal + panicking jobs, pool survives
  pool_test.go             panic-becomes-error, workers-survive tests, -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a pool whose worker runs each job under a deferred `recover`, turning a panic into an `error` wrapping `ErrJobPanic` (with the recovered value and a stack) delivered to an `onErr` callback, keeping the worker alive.
- Test: a panicking job is reported as an error containing the recovered value, normal jobs still complete, and after a panic the pool still reaches full concurrency (no worker was lost).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/safepool/cmd/demo
cd ~/go-exercises/safepool
go mod init example.com/safepool
```

### Recover belongs in the worker loop, once per job

`recover` only works inside a deferred function, and only stops a panic unwinding
*that* goroutine. Since the job runs on the worker's goroutine — not the
submitter's — the recover must live in the worker, wrapped around each individual
job call. The `safeRun` helper is that wrapper: it defers a function that calls
`recover()`, and if the recovered value is non-nil it converts it into an `error`
using a named return value, so the panic becomes an ordinary error the worker can
report. Because the deferred recover is scoped to `safeRun` and `safeRun` is
called once per job, a panic in job N is contained to job N; the loop moves on to
job N+1 and the worker never dies.

The recovered value is surfaced two ways at once: it is formatted into the error
message so a human sees what happened, and the error wraps a package sentinel
`ErrJobPanic` so code can classify it with `errors.Is`. A stack trace from
`runtime/debug.Stack()` is included because a panic's value alone rarely tells you
where it came from — the stack is what makes the report actionable. The finished
error flows to an `onErr` hook the caller supplies, which in production is where
you would emit a metric and a structured log line.

The failure this prevents is subtle. Without the recover, the panic crashes the
process. With a *careless* recover — say, one that also lets the worker goroutine
exit — the process survives but the pool silently loses a worker, and its
effective concurrency shrinks with no signal until, after enough bad inputs, the
pool has zero workers and quietly stops making progress. The correct structure
recovers, reports, and *continues the loop*, so `Size` workers stay alive.

Create `pool.go`:

```go
package safepool

import (
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// ErrJobPanic wraps a recovered panic so callers can classify it with errors.Is.
var ErrJobPanic = errors.New("job panicked")

// Job is a unit of work.
type Job func() error

// Pool runs workers that survive a panicking job: each job runs under recover,
// and a recovered panic is reported to onErr instead of crashing the process.
type Pool struct {
	workers int
	jobs    chan Job
	onErr   func(error)
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

// New starts workers goroutines. onErr, if non-nil, receives each job's error,
// including a wrapped ErrJobPanic for a recovered panic.
func New(workers int, onErr func(error)) *Pool {
	p := &Pool{
		workers: workers,
		jobs:    make(chan Job, workers*2),
		onErr:   onErr,
	}
	for range workers {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// safeRun runs job, converting a panic into an error wrapping ErrJobPanic with
// the recovered value and a stack trace. The named return lets the deferred
// recover set the error.
func safeRun(job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v\n%s", ErrJobPanic, r, debug.Stack())
		}
	}()
	return job()
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		if err := safeRun(job); err != nil && p.onErr != nil {
			p.onErr(err)
		}
	}
}

// Submit enqueues job, returning false after Close.
func (p *Pool) Submit(job Job) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job
	return true
}

// Size reports the worker count.
func (p *Pool) Size() int { return p.workers }

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

The demo submits three normal jobs and one that panics, collecting reported errors,
and prints that the process survived with the panic reported as an error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/safepool"
)

func main() {
	var panics atomic.Int64
	p := safepool.New(2, func(err error) {
		if errors.Is(err, safepool.ErrJobPanic) {
			panics.Add(1)
		}
	})

	var ok atomic.Int64
	var wg sync.WaitGroup
	jobs := []safepool.Job{
		func() error { ok.Add(1); return nil },
		func() error { panic("bad input") },
		func() error { ok.Add(1); return nil },
		func() error { ok.Add(1); return nil },
	}
	for _, j := range jobs {
		wg.Add(1)
		p.Submit(func() error {
			defer wg.Done()
			return j()
		})
	}
	wg.Wait()
	p.Close()

	fmt.Printf("normal ok: %d, panics recovered: %d\n", ok.Load(), panics.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
normal ok: 3, panics recovered: 1
```

### Tests

`TestPanicBecomesError` submits a job that panics with a known value and asserts
the reported error both matches `ErrJobPanic` via `errors.Is` and contains the
recovered value's text. `TestNormalJobsStillComplete` mixes normal and panicking
jobs and asserts every normal job ran. `TestWorkersSurvivePanic` first triggers a
panic per worker, then submits `Size` slow jobs and asserts the observed peak
concurrency still reaches `Size` — proof no worker goroutine was lost.

Create `pool_test.go`:

```go
package safepool

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPanicBecomesError(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got error
	done := make(chan struct{})
	p := New(1, func(err error) {
		mu.Lock()
		got = err
		mu.Unlock()
		close(done)
	})
	defer p.Close()

	p.Submit(func() error { panic("boom-42") })
	<-done

	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(got, ErrJobPanic) {
		t.Fatalf("err = %v, want wrapping ErrJobPanic", got)
	}
	if !strings.Contains(got.Error(), "boom-42") {
		t.Fatalf("err = %q, want it to contain recovered value boom-42", got.Error())
	}
}

func TestNormalJobsStillComplete(t *testing.T) {
	t.Parallel()

	p := New(4, func(error) {})
	defer p.Close()

	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		p.Submit(func() error {
			defer wg.Done()
			if i%5 == 0 {
				panic("periodic panic")
			}
			ok.Add(1)
			return nil
		})
	}
	wg.Wait()
	if got := ok.Load(); got != 16 { // 20 jobs, 4 panic (i=0,5,10,15)
		t.Fatalf("ok = %d, want 16", got)
	}
}

func TestWorkersSurvivePanic(t *testing.T) {
	t.Parallel()

	const workers = 4
	p := New(workers, func(error) {})
	defer p.Close()

	// Make every worker handle a panic.
	var panicWG sync.WaitGroup
	for range workers {
		panicWG.Add(1)
		p.Submit(func() error {
			defer panicWG.Done()
			panic("die")
		})
	}
	panicWG.Wait()

	// Now confirm all workers are still alive by reaching full concurrency.
	var concurrent, peak atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		p.Submit(func() error {
			defer wg.Done()
			cur := concurrent.Add(1)
			for {
				m := peak.Load()
				if cur <= m || peak.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
			return nil
		})
	}
	wg.Wait()
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrency after panics = %d, want %d (a worker died)", got, workers)
	}
}
```

## Review

The pool is correct when a panic is fully contained. `safeRun`'s deferred recover
converts the panic into an error that wraps `ErrJobPanic` and carries the recovered
value — `TestPanicBecomesError` checks both the `errors.Is` classification and the
value text. Because the recover is scoped per job inside the still-running loop,
the worker survives: `TestWorkersSurvivePanic` forces a panic on every worker and
then still reaches full concurrency, which only holds if no worker goroutine
exited. `TestNormalJobsStillComplete` confirms the panics did not disturb the
neighboring work.

The mistakes to avoid: no recover at all (the panic crashes the process); a recover
that lets the worker goroutine return (the pool silently shrinks, undetectable
until it stalls); and a recover that swallows the value without reporting it (you
lose the only signal that inputs are panicking). Note the panic count logic in the
tests: with 20 jobs, indices 0, 5, 10, 15 panic, so 16 complete. Run `-race` to
confirm the `onErr` callback and the shared counters are accessed cleanly.

## Resources

- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — how a deferred `recover` stops a panic and the value it returns.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — capturing the stack trace included in the error.
- [Go Blog: Defer, panic, and recover](https://go.dev/blog/defer-panic-and-recover) — the panic/recover model.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-backpressure-load-shedding.md](06-backpressure-load-shedding.md) | Next: [08-rate-limited-outbound.md](08-rate-limited-outbound.md)
