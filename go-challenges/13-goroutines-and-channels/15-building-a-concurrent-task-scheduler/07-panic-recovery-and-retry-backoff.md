# Exercise 7: Resilient Workers — Recover from Panicking Jobs and Retry with Backoff

A job runner must survive one poison task without killing every worker, and it
must retry transient failures without stampeding a recovering dependency. This
module wraps each task in `recover`, adds capped exponential backoff with jitter
and a max-attempts bound, and routes terminal failures to a dead-letter channel.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
resilient-scheduler/           module example.com/resilient-scheduler
  go.mod                       go 1.25
  scheduler.go                 recover wrapper; Policy backoff+jitter; dead-letter channel
  cmd/
    demo/
      main.go                  demo: recovered panic, retry-then-succeed, dead-letter
  scheduler_test.go            panic-recovered, retry-succeeds, dead-letter, ctx-bounded, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: a per-task `defer`/`recover` wrapper turning a panic into an error `Result`; a `Policy` with capped exponential backoff, jitter, and `MaxAttempts`; a dead-letter channel for terminal failures.
Test: a panicking task is recovered (error mentions the recovered value) and the pool keeps working; a task failing N-1 times then succeeding returns success with attempt count N; an always-failing task lands on the dead-letter channel after max attempts; backoff respects the context deadline.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/07-panic-recovery-and-retry-backoff/cmd/demo
cd go-solutions/13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/07-panic-recovery-and-retry-backoff
go mod edit -go=1.25
```

### recover on the worker, backoff between attempts

`recover` only works when called directly from a deferred function on the
goroutine that panicked. So the recover must live in the worker's per-task
invocation, not somewhere up the call stack. `runOnce` wraps the task in a
`defer`/`recover` that converts a panic into an error wrapping the sentinel
`ErrPanic` and *mentioning the recovered value* — a recovered panic becomes an
error result, never a silent success. Because the recover is on the worker
goroutine, one poison task cannot take down the pool: the worker records the error
and picks up the next job.

Retry is capped exponential backoff with full jitter and a hard attempt bound.
The delay for attempt `k` is `BaseDelay · 2^(k-1)` capped at `MaxDelay`, then
randomized uniformly in `[0, delay]` (full jitter) so retries de-synchronize
instead of stampeding. The backoff sleep is a `select` over `time.After(delay)`
and `ctx.Done()`, so a retry never blocks past the caller's deadline; and each
attempt checks `ctx.Err()` first, so an expired context ends the loop
deterministically. When attempts are exhausted, the terminal failure is delivered
on the result channel *and* pushed to a dead-letter channel for inspection — the
production pattern for "we could not process this; hold it for a human".

Create `scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

var (
	// ErrPanic wraps a recovered panic value.
	ErrPanic = errors.New("task panicked")
	// ErrShuttingDown is delivered when Submit races Stop.
	ErrShuttingDown = errors.New("scheduler shutting down")
)

type Task func() (any, error)

// Result carries the outcome and the number of attempts made.
type Result struct {
	Value    any
	Err      error
	Attempts int
}

// DeadLetter is a task that exhausted its attempts.
type DeadLetter struct {
	Err      error
	Attempts int
}

// Policy controls retry behavior.
type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

type task struct {
	ctx  context.Context
	fn   Task
	done chan Result
}

// Scheduler runs tasks with panic recovery and retry-with-backoff.
type Scheduler struct {
	policy     Policy
	tasks      chan task
	deadLetter chan DeadLetter
	quit       chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// New starts a Scheduler with the given workers and retry policy.
func New(workers int, p Policy) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	s := &Scheduler{
		policy:     p,
		tasks:      make(chan task, workers*2),
		deadLetter: make(chan DeadLetter, 1024),
		quit:       make(chan struct{}),
	}
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

// runOnce invokes fn with a recover, so a panic becomes an error result.
func (s *Scheduler) runOnce(fn Task) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			v = nil
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	return fn()
}

func (s *Scheduler) backoff(attempt int) time.Duration {
	d := s.policy.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= s.policy.MaxDelay {
			d = s.policy.MaxDelay
			break
		}
	}
	if d > s.policy.MaxDelay {
		d = s.policy.MaxDelay
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1)) // full jitter in [0, d]
}

func (s *Scheduler) process(t task) {
	var (
		lastVal any
		lastErr error
		attempt int
	)
	for attempt = 1; attempt <= s.policy.MaxAttempts; attempt++ {
		if err := t.ctx.Err(); err != nil {
			t.done <- Result{Err: err, Attempts: attempt - 1}
			return
		}
		v, err := s.runOnce(t.fn)
		if err == nil {
			t.done <- Result{Value: v, Attempts: attempt}
			return
		}
		lastVal, lastErr = v, err
		if attempt == s.policy.MaxAttempts {
			break
		}
		select {
		case <-time.After(s.backoff(attempt)):
		case <-t.ctx.Done():
			t.done <- Result{Err: t.ctx.Err(), Attempts: attempt}
			return
		}
	}

	res := Result{Value: lastVal, Err: lastErr, Attempts: attempt}
	t.done <- res
	select {
	case s.deadLetter <- DeadLetter{Err: lastErr, Attempts: attempt}:
	default: // dead-letter buffer full: drop rather than block a worker
	}
}

// Submit enqueues fn and returns a capacity-1 result channel.
func (s *Scheduler) Submit(ctx context.Context, fn Task) <-chan Result {
	done := make(chan Result, 1)
	if err := ctx.Err(); err != nil {
		done <- Result{Err: err}
		return done
	}
	t := task{ctx: ctx, fn: fn, done: done}
	select {
	case s.tasks <- t:
	case <-ctx.Done():
		done <- Result{Err: ctx.Err()}
	case <-s.quit:
		done <- Result{Err: ErrShuttingDown}
	}
	return done
}

// DeadLetters exposes terminal failures for inspection.
func (s *Scheduler) DeadLetters() <-chan DeadLetter { return s.deadLetter }

// Stop signals the workers and joins them.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.quit) })
	s.wg.Wait()
}
```

### The runnable demo

The demo uses two schedulers so the output is clean: one shows the pool surviving a
panic, the other shows retry-then-succeed and a dead letter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/resilient-scheduler"
)

func main() {
	// The pool survives a panicking task.
	pool := scheduler.New(2, scheduler.Policy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	defer pool.Stop()

	pr := <-pool.Submit(context.Background(), func() (any, error) { panic("bad payload") })
	fmt.Println("panic recovered:", pr.Err)
	alive := <-pool.Submit(context.Background(), func() (any, error) { return "still alive", nil })
	fmt.Println("pool:", alive.Value)

	// Retry with backoff, then dead-letter.
	retry := scheduler.New(2, scheduler.Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond})
	defer retry.Stop()

	var n int
	ok := <-retry.Submit(context.Background(), func() (any, error) {
		n++
		if n < 3 {
			return nil, fmt.Errorf("attempt %d failed", n)
		}
		return "success", nil
	})
	fmt.Printf("succeeded after %d attempts: %v\n", ok.Attempts, ok.Value)

	<-retry.Submit(context.Background(), func() (any, error) { return nil, errors.New("permanent") })
	dl := <-retry.DeadLetters()
	fmt.Printf("dead letter after %d attempts: %v\n", dl.Attempts, dl.Err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
panic recovered: task panicked: bad payload
pool: still alive
succeeded after 3 attempts: success
dead letter after 3 attempts: permanent
```

### Tests

`TestPanicRecovered` asserts a panic becomes an `ErrPanic` result mentioning the
recovered value and that the pool keeps working. `TestRetryEventuallySucceeds`
proves the attempt count. `TestDeadLetterAfterMaxAttempts` proves terminal
failures reach the dead-letter channel. `TestBackoffRespectsContext` proves a
retrying task stops at the context deadline.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPanicRecovered(t *testing.T) {
	t.Parallel()

	s := New(2, Policy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	defer s.Stop()

	r := <-s.Submit(context.Background(), func() (any, error) { panic("boom") })
	if !errors.Is(r.Err, ErrPanic) {
		t.Fatalf("Err = %v, want wrapped ErrPanic", r.Err)
	}
	if !strings.Contains(r.Err.Error(), "boom") {
		t.Fatalf("Err = %q, want it to mention the recovered value", r.Err)
	}

	// The pool must still process the next task.
	alive := <-s.Submit(context.Background(), func() (any, error) { return "alive", nil })
	if alive.Err != nil || alive.Value != "alive" {
		t.Fatalf("after panic, next task = (%v, %v), want (alive, nil)", alive.Value, alive.Err)
	}
}

func TestRetryEventuallySucceeds(t *testing.T) {
	t.Parallel()

	s := New(2, Policy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond})
	defer s.Stop()

	var calls atomic.Int64
	r := <-s.Submit(context.Background(), func() (any, error) {
		if calls.Add(1) < 3 {
			return nil, errors.New("transient")
		}
		return "ok", nil
	})
	if r.Err != nil || r.Value != "ok" {
		t.Fatalf("result = (%v, %v), want (ok, nil)", r.Value, r.Err)
	}
	if r.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", r.Attempts)
	}
}

func TestDeadLetterAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	s := New(2, Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond})
	defer s.Stop()

	want := errors.New("always fails")
	r := <-s.Submit(context.Background(), func() (any, error) { return nil, want })
	if !errors.Is(r.Err, want) {
		t.Fatalf("Err = %v, want %v", r.Err, want)
	}
	if r.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", r.Attempts)
	}

	select {
	case dl := <-s.DeadLetters():
		if !errors.Is(dl.Err, want) {
			t.Fatalf("dead letter Err = %v, want %v", dl.Err, want)
		}
		if dl.Attempts != 3 {
			t.Fatalf("dead letter Attempts = %d, want 3", dl.Attempts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no dead letter after exhausting attempts")
	}
}

func TestBackoffRespectsContext(t *testing.T) {
	t.Parallel()

	s := New(1, Policy{MaxAttempts: 50, BaseDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond})
	defer s.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	r := <-s.Submit(ctx, func() (any, error) { return nil, errors.New("fail") })
	if !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want context.DeadlineExceeded (retry ignored the deadline)", r.Err)
	}
}

func Example() {
	s := New(1, Policy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	defer s.Stop()

	r := <-s.Submit(context.Background(), func() (any, error) { return "done", nil })
	fmt.Println(r.Value, r.Attempts)
	// Output: done 1
}
```

## Review

Resilience has two halves. Recovery: the `defer`/`recover` lives on the worker
goroutine and converts a panic into an error that wraps `ErrPanic` and names the
recovered value, so one poison task never crashes the pool and never masquerades
as success. Retry: capped exponential backoff with full jitter de-synchronizes
retries, `MaxAttempts` bounds them, the backoff `select` and the per-attempt
`ctx.Err()` check keep them inside the caller's deadline, and terminal failures go
to a dead-letter channel. The classic mistakes are calling `recover` outside a
deferred function (it returns nil and does nothing) and retrying without jitter or
a bound (a thundering herd). Run `go test -race -count=1 ./...`.

## Resources

- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — how `recover` interacts with `defer` and panics.
- [AWS Architecture Blog: Exponential backoff and jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter beats plain exponential backoff.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `rand.Int64N` for the jitter draw.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-per-task-timeout-and-cancellation.md](06-per-task-timeout-and-cancellation.md) | Next: [08-weighted-concurrency-semaphore.md](08-weighted-concurrency-semaphore.md)
