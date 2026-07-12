# Exercise 18: Webhook Batch Queue — Drain With Context Deadline

**Nivel: Intermedio** — validacion rapida (un test corto).

A request handler that queues outbound webhooks — "order created", "payment
captured" — usually wants to flush them all when it finishes, on every exit
path. But "flush" means calling out to a webhook target, which can be slow
or unreachable, and cleanup code that blocks indefinitely on a network call
is its own outage. This module defers a drain bounded by a `context`
deadline, so the flush either finishes or gives up, and reports exactly what
did and did not make it out.

## What you'll build

```text
webhookqueue/                 independent module: example.com/webhookqueue
  go.mod
  webhookqueue/webhookqueue.go       Webhook, Queue, DrainResult; Drain; HandleBatch (defer bounded drain)
  webhookqueue/webhookqueue_test.go  full drain; already-expired deadline; deadline mid-batch; drains on error too
  cmd/demo/main.go                   runnable demo: queue three webhooks, drain them, print the report
```

- Files: `webhookqueue/webhookqueue.go`, `webhookqueue/webhookqueue_test.go`, `cmd/demo/main.go`.
- Implement: a `Queue` with `Enqueue(w Webhook)` and `Pending() []Webhook`; `Drain(ctx context.Context, deliver func(context.Context, Webhook) error) DrainResult` that checks `ctx.Done()` before every delivery and stops the instant it fires, returning whatever was delivered and whatever is left over; and `HandleBatch(ctx, q, drainTimeout, deliver, report *DrainResult, work func(*Queue) error) (err error)` that runs `work` and defers a closure that wraps `ctx` in a timeout and drains into `*report`.
- Test: a drain where nothing cancels (everything delivered); a drain against an already-cancelled context (nothing delivered, everything remaining); a drain where the fake `deliver` cancels the context itself right after the second call (first two delivered, rest remaining); and `HandleBatch` draining the same queued webhooks whether `work` returns `nil` or an error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/18-pending-webhook-drain-timeout/webhookqueue go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/18-pending-webhook-drain-timeout/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/18-pending-webhook-drain-timeout
go mod edit -go=1.24
```

### Bounding cleanup with a context, not just the request

The interesting `defer` here does not just call one cleanup function — it
first *creates* the context that bounds it: `context.WithTimeout(ctx,
drainTimeout)` inside the deferred closure, then `q.Drain(drainCtx, deliver)`.
That timeout is deliberately independent of whatever deadline the original
request's `ctx` carried: a request can have already timed out (that may be
why `work` returned an error) while the drain still deserves its own bounded
window to flush whatever was queued before giving up, rather than either
blocking forever or being killed instantly by an already-expired parent
deadline. `context.WithTimeout`'s derived context still respects the
parent's deadline if the parent expires sooner — it takes the earlier of the
two — so this is strictly additive: the drain gets at most `drainTimeout`,
and never more than the parent allows.

### Testing a deadline without waiting for one

Real deadlines fire on a wall clock, but a test that actually sleeps to prove
a timeout cuts a loop short is slow and flaky under load. Two tricks avoid
that entirely here. First, `context.WithCancel` followed immediately by
`cancel()` produces a context whose `Done()` channel is already closed — the
deterministic stand-in for "the deadline already passed," with no waiting at
all. Second, the fake `deliver` function passed to `Drain` in
`TestDrainStopsMidBatchWhenDeadlineFiresDuringDelivery` calls `cancel()`
itself after its second invocation — simulating "the deadline fires exactly
here" by triggering the same `ctx.Done()` signal a real timer would, on
command, so the test is both instant and exact about which delivery is the
last one to succeed.

Create `webhookqueue/webhookqueue.go`:

```go
package webhookqueue

import (
	"context"
	"sync"
	"time"
)

// Webhook is one outbound delivery accumulated during a request.
type Webhook struct {
	ID string
}

// DrainResult reports what a Drain call actually accomplished: everything
// delivered before ctx ended, and everything left over because it didn't.
type DrainResult struct {
	Delivered []Webhook
	Remaining []Webhook
}

// Queue accumulates webhooks to deliver. Enqueue is called any number of
// times while a request is handled; Drain is called once, afterward, to
// flush them.
type Queue struct {
	mu      sync.Mutex
	pending []Webhook
}

// Enqueue adds one webhook to the pending batch.
func (q *Queue) Enqueue(w Webhook) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, w)
}

// Pending returns a defensive copy of what has not yet been drained.
func (q *Queue) Pending() []Webhook {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Webhook, len(q.pending))
	copy(out, q.pending)
	return out
}

// Drain delivers every pending webhook via deliver, checking ctx before each
// delivery so a deadline (or an explicit cancellation) stops the loop
// immediately instead of letting a slow or unreachable target block cleanup
// forever. Anything not yet delivered when ctx ends is returned as Remaining
// rather than silently dropped.
func (q *Queue) Drain(ctx context.Context, deliver func(context.Context, Webhook) error) DrainResult {
	q.mu.Lock()
	items := q.pending
	q.pending = nil
	q.mu.Unlock()

	var result DrainResult
	for i, w := range items {
		select {
		case <-ctx.Done():
			result.Remaining = append(result.Remaining, items[i:]...)
			return result
		default:
		}

		if err := deliver(ctx, w); err != nil {
			result.Remaining = append(result.Remaining, items[i:]...)
			return result
		}
		result.Delivered = append(result.Delivered, w)
	}
	return result
}

// HandleBatch runs work to accumulate webhooks onto q, then -- in a deferred
// closure that runs on every exit path, success or error -- drains q with a
// timeout-bounded context so the drain itself cannot block the caller
// indefinitely on a wedged delivery. The drain's outcome is written to
// report so a caller can inspect exactly what got flushed before the
// deadline cut it off.
func HandleBatch(
	ctx context.Context,
	q *Queue,
	drainTimeout time.Duration,
	deliver func(context.Context, Webhook) error,
	report *DrainResult,
	work func(*Queue) error,
) (err error) {
	defer func() {
		drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
		defer cancel()
		*report = q.Drain(drainCtx, deliver)
	}()
	return work(q)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/webhookqueue/webhookqueue"
)

func main() {
	q := &webhookqueue.Queue{}
	var report webhookqueue.DrainResult

	err := webhookqueue.HandleBatch(context.Background(), q, time.Second,
		func(context.Context, webhookqueue.Webhook) error { return nil },
		&report,
		func(q *webhookqueue.Queue) error {
			q.Enqueue(webhookqueue.Webhook{ID: "order-created"})
			q.Enqueue(webhookqueue.Webhook{ID: "payment-captured"})
			q.Enqueue(webhookqueue.Webhook{ID: "shipment-queued"})
			return nil
		})

	fmt.Println("handler error:", err)
	fmt.Println("delivered:", report.Delivered)
	fmt.Println("remaining:", report.Remaining)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handler error: <nil>
delivered: [{order-created} {payment-captured} {shipment-queued}]
remaining: []
```

### Tests

Create `webhookqueue/webhookqueue_test.go`:

```go
package webhookqueue

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func webhooks(ids ...string) []Webhook {
	out := make([]Webhook, len(ids))
	for i, id := range ids {
		out[i] = Webhook{ID: id}
	}
	return out
}

func TestDrainDeliversEverythingWhenNothingCancels(t *testing.T) {
	t.Parallel()

	q := &Queue{}
	for _, w := range webhooks("a", "b", "c") {
		q.Enqueue(w)
	}

	result := q.Drain(context.Background(), func(context.Context, Webhook) error { return nil })

	if !slices.Equal(result.Delivered, webhooks("a", "b", "c")) {
		t.Fatalf("Delivered = %v, want all three", result.Delivered)
	}
	if result.Remaining != nil {
		t.Fatalf("Remaining = %v, want nil", result.Remaining)
	}
}

func TestDrainStopsAtAlreadyExpiredDeadline(t *testing.T) {
	t.Parallel()

	q := &Queue{}
	for _, w := range webhooks("a", "b", "c") {
		q.Enqueue(w)
	}

	// A deadline that has already passed closes ctx.Done() immediately, with
	// no real waiting -- this is the deterministic stand-in for "the drain
	// timeout already fired" that this test needs.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	deliverCalls := 0
	result := q.Drain(ctx, func(context.Context, Webhook) error {
		deliverCalls++
		return nil
	})

	if deliverCalls != 0 {
		t.Fatalf("deliver was called %d times, want 0", deliverCalls)
	}
	if result.Delivered != nil {
		t.Fatalf("Delivered = %v, want nil", result.Delivered)
	}
	if !slices.Equal(result.Remaining, webhooks("a", "b", "c")) {
		t.Fatalf("Remaining = %v, want all three", result.Remaining)
	}
}

func TestDrainStopsMidBatchWhenDeadlineFiresDuringDelivery(t *testing.T) {
	t.Parallel()

	q := &Queue{}
	for _, w := range webhooks("a", "b", "c", "d") {
		q.Enqueue(w)
	}

	ctx, cancel := context.WithCancel(context.Background())
	delivered := 0
	// Simulate the drain's deadline firing right after the second delivery,
	// without any real clock or sleep: the fake deliver calls cancel itself.
	deliver := func(context.Context, Webhook) error {
		delivered++
		if delivered == 2 {
			cancel()
		}
		return nil
	}

	result := q.Drain(ctx, deliver)

	if !slices.Equal(result.Delivered, webhooks("a", "b")) {
		t.Fatalf("Delivered = %v, want [a b]", result.Delivered)
	}
	if !slices.Equal(result.Remaining, webhooks("c", "d")) {
		t.Fatalf("Remaining = %v, want [c d]", result.Remaining)
	}
}

func TestHandleBatchDrainsOnEveryExitPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		workErr error
	}{
		{name: "work succeeds", workErr: nil},
		{name: "work fails", workErr: errors.New("downstream unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &Queue{}
			var report DrainResult

			err := HandleBatch(context.Background(), q, time.Second,
				func(context.Context, Webhook) error { return nil },
				&report,
				func(q *Queue) error {
					q.Enqueue(Webhook{ID: "hook-1"})
					q.Enqueue(Webhook{ID: "hook-2"})
					return tt.workErr
				})

			if !errors.Is(err, tt.workErr) {
				t.Fatalf("err = %v, want %v", err, tt.workErr)
			}
			// The drain runs on every exit path, so both webhooks queued
			// during work are delivered whether work succeeded or failed.
			if !slices.Equal(report.Delivered, webhooks("hook-1", "hook-2")) {
				t.Fatalf("Delivered = %v, want [hook-1 hook-2]", report.Delivered)
			}
			if report.Remaining != nil {
				t.Fatalf("Remaining = %v, want nil", report.Remaining)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

`Drain` never blocks past the point its `ctx` ends: the `select` with a
`default` case is a non-blocking poll, checked before every single delivery,
not just once at the top of the function — a batch of a thousand webhooks
against a deadline that fires after the third one stops at the third, not
after grinding through the rest. Nothing queued is ever silently dropped:
whatever did not get delivered before `ctx.Done()` fired is returned in
`Remaining`, which a real system would persist and retry rather than
discard. And `HandleBatch`'s defer runs the drain on both branches of
`TestHandleBatchDrainsOnEveryExitPath` — the same "cleanup happens whether
the handler succeeded or failed" guarantee every other exercise in this
chapter relies on, here applied to a batch of pending network calls instead
of a single resource.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancel`, `Done`.
- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Blog: Context](https://go.dev/blog/context) — bounding work with deadlines and cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [17-flag-flip-panic-restore.md](17-flag-flip-panic-restore.md) | Next: [19-event-subscriber-cleanup-list.md](19-event-subscriber-cleanup-list.md)
