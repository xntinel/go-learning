# Exercise 3: A Draining, Instrumented Event Pipeline

A log or event processor cannot drop the last second of buffered work when it shuts down — losing events on a deploy is a real outage. This exercise builds an instrumented worker pipeline that does the opposite of a hard cancel: on shutdown it stops accepting new events, drains every event already accepted, and returns only once every worker has exited, with atomic counters reporting what happened and no goroutine left behind.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
logpipe.go           Event, Metrics, Processor, New, Submit, Shutdown, Metrics, worker
logpipe_test.go      drain-all, reject-after-close, idempotent, timeout, concurrent, leak
cmd/
  demo/
    main.go          submit 100 events, drain, print metrics
```

- Files: `logpipe.go`, `logpipe_test.go`, `cmd/demo/main.go`.
- Implement: `Event`, `Metrics`, the `Processor` with `New`, `Submit`, `Shutdown`, a `Metrics()` snapshot, and the worker loop, using a mutex-guarded close flag and atomic counters.
- Test: that shutdown drains every accepted event, that submitting after shutdown is rejected, that shutdown is idempotent and timeout-bounded, that concurrent submitters are race-free, and that no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p log-event-pipeline/cmd/demo && cd log-event-pipeline
go mod init example.com/log-event-pipeline
```

### Drain, do not drop: the shutdown protocol

The processor is a pool of worker goroutines, each running `for e := range p.in`. Ranging over a channel is what makes draining trivial: when the input channel is closed, the range delivers every element still buffered before the loop ends, so closing `in` and letting the workers run their loops to completion processes exactly the events already accepted and not one more. A `sync.WaitGroup` counts the workers, and `Shutdown` blocks on `wg.Wait()`, so the moment `Shutdown` returns is the moment the last worker has exited — that is the structural guarantee of no leak, stronger than any goroutine count.

The hard part is the race between a `Submit` still sending and a `Shutdown` closing the same channel. A send on a closed channel panics, so the two must be mutually exclusive. The tool is a `sync.RWMutex` used against its usual intuition: `Submit` takes the read lock, and `Shutdown` takes the write lock. Many `Submit`s may run concurrently (reads do not exclude each other), but `Shutdown`'s write lock cannot be acquired while any `Submit` holds the read lock, so the `close(p.in)` can never overlap a `p.in <- e`. A `Submit` that arrives after `Shutdown` has set the `closed` flag sees it under the read lock and returns `ErrClosed` instead of sending.

The detail that looks alarming but is correct: `Submit` holds the read lock across a potentially blocking channel send. If the input buffer is full, the send blocks (backpressure), and `Shutdown` waiting for the write lock blocks behind it. There is no deadlock, because the workers are still draining `in` — `Shutdown` has not closed it yet, precisely because it is still waiting for the write lock — so the blocked send eventually completes, the read lock releases, and `Shutdown` proceeds. The invariant this buys is worth the staring: a `Submit` that returns nil has provably enqueued its event before any close, so every accepted event is drained, and `Processed + Failed == Submitted` after a clean shutdown.

Instrumentation is lock-free. `Submitted`, `Processed`, and `Failed` are `atomic.Int64` counters: `Submit` does `submitted.Add(1)` after passing the closed check, and each worker does `processed.Add(1)` or `failed.Add(1)` per event. `Metrics()` reads them with `Load()`. Atomics are the right tool because the counters are touched from many goroutines on the hot path and a mutex there would serialize the workers; a plain `int++` would be a data race the detector flags and an undercount in production.

`Shutdown` is also deadline-aware and idempotent. It closes `in` once (guarded by the `closed` flag so a second call is a no-op that just returns the latest snapshot), then waits for the workers in a goroutine that closes a `done` channel, and selects between `done` and `ctx.Done()`. If the context deadline fires first, `Shutdown` returns `ctx.Err()`; the workers are not abandoned, because `in` is already closed and they terminate on their own once their in-flight handler returns — the timeout bounds how long the caller waits, not whether the workers eventually exit.

Create `logpipe.go`:

```go
// Package logpipe implements an instrumented, gracefully-draining event
// processor: a pool of worker goroutines consume submitted events, atomic
// counters instrument the work, and Shutdown drains every accepted event before
// the workers exit, with no goroutine leak.
package logpipe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by Submit once Shutdown has begun.
var ErrClosed = errors.New("logpipe: processor is closed")

// Event is one unit of work.
type Event struct {
	ID      int
	Payload string
}

// Metrics is a snapshot of the processor's instrumentation.
type Metrics struct {
	Submitted int64
	Processed int64
	Failed    int64
}

// Processor is a draining, instrumented event pipeline.
type Processor struct {
	in      chan Event
	wg      sync.WaitGroup
	handler func(Event) error

	mu     sync.RWMutex // guards closed and the close(in)
	closed bool

	submitted atomic.Int64
	processed atomic.Int64
	failed    atomic.Int64
}

// New starts a processor with the given number of worker goroutines and input
// buffer. handler processes one event; a non-nil return marks the event failed.
func New(workers, buffer int, handler func(Event) error) *Processor {
	if workers < 1 {
		workers = 1
	}
	if buffer < 0 {
		buffer = 0
	}
	p := &Processor{
		in:      make(chan Event, buffer),
		handler: handler,
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *Processor) worker() {
	defer p.wg.Done()
	for e := range p.in {
		if err := p.handler(e); err != nil {
			p.failed.Add(1)
		} else {
			p.processed.Add(1)
		}
	}
}

// Submit enqueues an event. It blocks if the input buffer is full (backpressure)
// and returns ErrClosed once Shutdown has begun. A nil return guarantees the
// event will be processed before Shutdown returns.
func (p *Processor) Submit(e Event) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	p.submitted.Add(1)
	p.in <- e
	return nil
}

// Shutdown stops accepting new events, drains every already-accepted event, and
// returns once all workers have exited. It is idempotent. If ctx is cancelled
// before the drain completes, Shutdown returns ctx.Err(); the workers still
// terminate on their own because the input channel is already closed.
func (p *Processor) Shutdown(ctx context.Context) (Metrics, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.snapshot(), nil
	}
	p.closed = true
	close(p.in)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return p.snapshot(), nil
	case <-ctx.Done():
		return p.snapshot(), ctx.Err()
	}
}

// Metrics returns a current snapshot of the instrumentation.
func (p *Processor) Metrics() Metrics { return p.snapshot() }

func (p *Processor) snapshot() Metrics {
	return Metrics{
		Submitted: p.submitted.Load(),
		Processed: p.processed.Load(),
		Failed:    p.failed.Load(),
	}
}
```

### The runnable demo

The demo submits 100 events through four workers, fails every tenth event in the handler, then drains and prints the metrics. The counts are deterministic regardless of how the work is scheduled across the workers, because each event's outcome depends only on its own ID.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/log-event-pipeline"
)

func main() {
	p := logpipe.New(4, 16, func(e logpipe.Event) error {
		if e.ID%10 == 0 {
			return fmt.Errorf("bad event %d", e.ID)
		}
		return nil
	})

	for i := 0; i < 100; i++ {
		if err := p.Submit(logpipe.Event{ID: i, Payload: "log line"}); err != nil {
			fmt.Println("submit rejected:", err)
		}
	}

	m, err := p.Shutdown(context.Background())
	if err != nil {
		fmt.Println("shutdown error:", err)
		return
	}
	fmt.Printf("submitted=%d processed=%d failed=%d\n", m.Submitted, m.Processed, m.Failed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submitted=100 processed=90 failed=10
```

### Tests

The tests pin the drain-and-no-leak contract. `TestProcessorDrainsAll` submits many events and asserts every one was processed and `Processed == Submitted`. `TestSubmitAfterShutdownRejected` proves a late submit returns `ErrClosed` instead of panicking. `TestShutdownIdempotent` calls `Shutdown` twice and asserts identical metrics and no panic. `TestShutdownTimeout` blocks the handlers, asserts `Shutdown` returns `context.DeadlineExceeded`, then releases them and confirms the workers still drain and exit. `TestConcurrentSubmitters` hammers `Submit` from many goroutines under `-race`. `TestNoGoroutineLeak` confirms the goroutine count returns to baseline after shutdown.

Create `logpipe_test.go`:

```go
package logpipe

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessorDrainsAll(t *testing.T) {
	t.Parallel()

	const n = 500
	var seen sync.Map
	p := New(4, 8, func(e Event) error {
		seen.Store(e.ID, struct{}{})
		return nil
	})
	for i := 0; i < n; i++ {
		if err := p.Submit(Event{ID: i}); err != nil {
			t.Fatalf("Submit(%d): %v", i, err)
		}
	}

	m, err := p.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if m.Submitted != n || m.Processed != n || m.Failed != 0 {
		t.Fatalf("metrics = %+v, want submitted/processed=%d failed=0", m, n)
	}
	count := 0
	seen.Range(func(_, _ any) bool { count++; return true })
	if count != n {
		t.Fatalf("processed %d distinct events, want %d", count, n)
	}
}

func TestSubmitAfterShutdownRejected(t *testing.T) {
	t.Parallel()

	p := New(2, 4, func(e Event) error { return nil })
	if _, err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := p.Submit(Event{ID: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after shutdown: err = %v, want ErrClosed", err)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	t.Parallel()

	p := New(2, 4, func(e Event) error { return nil })
	for i := 0; i < 10; i++ {
		if err := p.Submit(Event{ID: i}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	m1, err := p.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	m2, err := p.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if m1 != m2 {
		t.Fatalf("metrics differ between shutdowns: %+v vs %+v", m1, m2)
	}
}

func TestShutdownTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	p := New(2, 8, func(e Event) error {
		<-release // block until the test releases the workers
		return nil
	})
	for i := 0; i < 4; i++ {
		if err := p.Submit(Event{ID: i}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want DeadlineExceeded", err)
	}

	// Release the workers; they must finish draining the buffered events and exit.
	close(release)
	for i := 0; i < 200; i++ {
		m := p.Metrics()
		if m.Processed+m.Failed == m.Submitted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("workers did not drain after release")
}

func TestConcurrentSubmitters(t *testing.T) {
	t.Parallel()

	const submitters = 8
	const each = 100
	var ran atomic.Int64
	p := New(4, 16, func(e Event) error {
		ran.Add(1)
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(submitters)
	for s := 0; s < submitters; s++ {
		go func(base int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := p.Submit(Event{ID: base*each + i}); err != nil {
					t.Errorf("Submit: %v", err)
					return
				}
			}
		}(s)
	}
	wg.Wait()

	m, err := p.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := int64(submitters * each)
	if m.Submitted != want || m.Processed != want {
		t.Fatalf("metrics = %+v, want submitted/processed=%d", m, want)
	}
	if ran.Load() != want {
		t.Fatalf("handler ran %d times, want %d", ran.Load(), want)
	}
}

// TestNoGoroutineLeak is intentionally NOT parallel so it runs alone in the
// sequential phase; NumGoroutine is process-global.
func TestNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	p := New(4, 16, func(e Event) error { return nil })
	for i := 0; i < 1000; i++ {
		if err := p.Submit(Event{ID: i}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	if _, err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline %d: now %d", base, runtime.NumGoroutine())
}
```

## Review

The processor is correct when draining is total and shutdown is leak-free. Confirm the workers `range` over `in` so closing it delivers every buffered event before the loops end, and that `Shutdown` blocks on `wg.Wait()` so its return is proof the workers exited. The race that must not exist is a send on a closed channel: `Submit` holds the read lock around its send and `Shutdown` takes the write lock around `close(in)`, so the two are mutually exclusive, and a `Submit` that loses the race sees `closed` and returns `ErrClosed`. The invariant `Processed + Failed == Submitted` after a clean shutdown is the observable proof that no accepted event was dropped, and the whole thing passing under `go test -race` is what establishes the counters and the close flag are properly synchronized.

Common mistakes for this feature. The first is closing `in` from `Submit` or from a second goroutine without the mutex, which lets a concurrent send panic — only `Shutdown` closes, and only under the write lock. The second is using a hard `done`/cancel signal that makes workers abandon buffered events on shutdown, which silently drops the in-flight work the service exists to preserve; draining means closing the input and letting the range finish, not returning early. The third is instrumenting with a plain `int++` from multiple workers, a data race that undercounts; use `atomic.Int64`. The fourth is treating a `Shutdown` timeout as a leak: the workers still exit because `in` is closed, so the timeout bounds the wait, not the teardown — but a handler that never returns is a genuine leak the timeout cannot fix, which is why handlers must themselves honor cancellation in production.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — the typed atomic counters (`Int64.Add`, `Int64.Load`) used for lock-free instrumentation on the hot path.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock that makes concurrent `Submit`s exclusive with the single `close` in `Shutdown`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the goroutine-leak failure mode and the explicit-shutdown discipline that prevents it.

---

Back to [02-etl-pipeline-service.md](02-etl-pipeline-service.md) | Next: [../02-fan-out-pattern/02-fan-out-pattern.md](../02-fan-out-pattern/02-fan-out-pattern.md)
