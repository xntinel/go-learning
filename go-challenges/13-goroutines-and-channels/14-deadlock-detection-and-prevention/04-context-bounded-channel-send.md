# Exercise 4: Cancellable Enqueue onto a Bounded Job Queue

A producer that sends on a channel with no consumer blocks forever — a partial deadlock the
runtime never reports, because the rest of the service keeps running. This exercise builds a
dispatcher whose `Submit` sends onto a bounded job channel using a `select` over the send and
`ctx.Done()`, so a slow or absent consumer yields a clean `context.Canceled` /
`context.DeadlineExceeded` and the producer goroutine exits instead of leaking.

This module is fully self-contained: its own `go mod init`, all types inline, its own demo
and tests.

## What you'll build

```text
dispatch/                  independent module: example.com/dispatch
  go.mod                   go 1.25
  dispatch.go              Dispatcher, Job; Submit (select over send + ctx.Done)
  cmd/
    demo/
      main.go              submit with a consumer (ok) and without one (times out)
  dispatch_test.go         timeout returns ctx err + producer exits; happy handoff
```

- Files: `dispatch.go`, `cmd/demo/main.go`, `dispatch_test.go`.
- Implement: `Dispatcher.Submit(ctx, job)` that sends onto a bounded channel via `select`, returning `context.Cause(ctx)` if the deadline/cancellation fires before a consumer accepts the job.
- Test: assert `Submit` returns the context error when no consumer reads within the deadline and that the producer goroutine actually exits (a `done` signal); assert a successful handoff when a consumer is present.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dispatch/cmd/demo
cd ~/go-exercises/dispatch
go mod init example.com/dispatch
go mod edit -go=1.25
```

### The invisible producer wedge

A bounded (or unbuffered) job channel provides backpressure: when the queue is full, the
producer waits until a worker frees a slot. That is the correct behavior right up until the
consumer stops — a worker pool that crashed, a downstream that is wedged, a shutdown that ran
in the wrong order. Now `ch <- job` blocks with no one to unblock it. The producer goroutine
is parked forever. If that producer is an HTTP handler goroutine, the request never completes;
the client eventually times out but the goroutine stays stuck, holding its request context,
its buffers, its slot in the server's connection pool. Repeat under load and the service
leaks goroutines until it falls over — and the runtime's deadlock detector never fires,
because the accept loop and the health check are still runnable.

The fix is to make the send cancellable. A bare `ch <- job` has no escape; a `select` gives
it one:

```go
select {
case ch <- job:
	return nil
case <-ctx.Done():
	return context.Cause(ctx)
}
```

Now the send has two ways to complete: a consumer accepts the job (success), or the context
is cancelled or times out (the producer returns a context error and exits). The goroutine can
never be stuck permanently as long as the caller supplies a context with a deadline, which a
request-scoped handler always has. `context.Cause` returns the specific cancellation reason
when one was supplied via `context.WithTimeoutCause` or a manual `cancel(err)`, and falls back
to the standard `context.Canceled` / `context.DeadlineExceeded` otherwise — strictly more
informative than `ctx.Err()`.

Create `dispatch.go`:

```go
package dispatch

import "context"

// Job is a unit of work carried over the queue.
type Job struct {
	ID      int
	Payload string
}

// Dispatcher hands jobs to workers over a bounded channel with cancellable
// submission, so a slow or absent consumer never wedges the producer.
type Dispatcher struct {
	jobs chan Job
}

// New returns a Dispatcher whose queue holds up to capacity buffered jobs.
// A capacity of 0 makes submission a synchronous handoff.
func New(capacity int) *Dispatcher {
	return &Dispatcher{jobs: make(chan Job, capacity)}
}

// Jobs returns the receive end of the queue for consumers to range or select.
func (d *Dispatcher) Jobs() <-chan Job { return d.jobs }

// Submit enqueues job, blocking until a slot is free OR ctx is done. It returns
// nil once the job is queued, or context.Cause(ctx) if the context is cancelled
// or its deadline fires first. On the error path nothing is enqueued and the
// calling goroutine returns rather than leaking on a full queue.
func (d *Dispatcher) Submit(ctx context.Context, job Job) error {
	select {
	case d.jobs <- job:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Close signals that no more jobs will be submitted. Consumers ranging over
// Jobs() will observe the channel close and stop.
func (d *Dispatcher) Close() { close(d.jobs) }
```

### The runnable demo

The demo submits one job with a consumer present (succeeds) and one with no consumer under a
short deadline (returns `DeadlineExceeded`), showing the producer is freed rather than wedged.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/dispatch"
)

func main() {
	d := dispatch.New(0) // unbuffered: submission needs a live consumer

	// A consumer is present: the handoff succeeds. It closes consumed after
	// printing so main can order its own line deterministically after it.
	consumed := make(chan struct{})
	go func() {
		job := <-d.Jobs()
		fmt.Printf("consumed job %d: %s\n", job.ID, job.Payload)
		close(consumed)
	}()
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	defer cancel1()
	if err := d.Submit(ctx1, dispatch.Job{ID: 1, Payload: "process"}); err == nil {
		<-consumed // wait for the consumer's line before printing ours
		fmt.Println("submit 1: ok")
	}

	// No consumer: submission times out instead of blocking forever.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := d.Submit(ctx2, dispatch.Job{ID: 2, Payload: "orphan"}); errors.Is(err, context.DeadlineExceeded) {
		fmt.Println("submit 2: deadline exceeded (producer freed)")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
consumed job 1: process
submit 1: ok
submit 2: deadline exceeded (producer freed)
```

### Tests

`TestSubmitTimeoutFreesProducer` is the important one: it submits to an unbuffered queue with
no consumer under a short deadline, asserts the returned error is `DeadlineExceeded`, and
asserts the producer goroutine actually *returned* by closing a `done` channel it waits on
with its own timeout — a goleak-style check that the goroutine is not merely unblocked in
theory but has exited. `TestSubmitHandoff` covers the success path with a consumer present.
`TestSubmitCancel` covers explicit cancellation via `cancel()`.

Create `dispatch_test.go`:

```go
package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSubmitHandoff(t *testing.T) {
	t.Parallel()

	d := New(0)
	go func() { <-d.Jobs() }() // consumer

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := d.Submit(ctx, Job{ID: 1}); err != nil {
		t.Fatalf("Submit err = %v, want nil", err)
	}
}

func TestSubmitTimeoutFreesProducer(t *testing.T) {
	t.Parallel()

	d := New(0) // no buffer, and we start no consumer

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		done <- d.Submit(ctx, Job{ID: 1})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Submit err = %v, want DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer goroutine did not exit: it is wedged on the send")
	}
}

func TestSubmitCancel(t *testing.T) {
	t.Parallel()

	d := New(0)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- d.Submit(ctx, Job{ID: 1}) }()

	cancel() // no consumer; cancellation must free the producer
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Submit err = %v, want Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not observe cancellation")
	}
}
```

## Review

`Submit` is correct when it has exactly two outcomes and no third: the job is enqueued (nil),
or the context ends (its cause), and in the latter case nothing is enqueued and the goroutine
returns. `TestSubmitTimeoutFreesProducer` proves the goroutine truly exits, not just that
`Submit` "should" return — the `done` channel with its own 2-second ceiling is the difference
between testing behavior and testing wishful thinking. A test that only asserted the returned
error would pass even if the producer were leaked, because it would read `done` from a
goroutine that had unblocked but, in a buggy design, could still be stuck.

The mistake to avoid is the bare `ch <- job` with no `select`, which is a latent deadlock the
moment the consumer stalls. The second is forgetting that on the error path the job is *not*
enqueued — callers must treat a context error as "this work was dropped" and decide whether to
retry or shed it. Run `-race` with a per-test timeout so a regression to the wedging version
fails fast instead of hanging CI.

## Resources

- [`context.Cause`](https://pkg.go.dev/context#Cause) — the specific cancellation reason, richer than `ctx.Err()`.
- [Go statements and `select`](https://go.dev/ref/spec#Select_statements) — how a multi-way communication chooses a ready case.
- [`context.Context`](https://pkg.go.dev/context#Context) — deadlines and cancellation propagation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-rwmutex-readthrough-cache.md](05-rwmutex-readthrough-cache.md)
