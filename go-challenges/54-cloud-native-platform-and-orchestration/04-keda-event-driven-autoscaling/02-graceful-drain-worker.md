# Exercise 2: A Scale-to-Zero Worker That Drains on SIGTERM

Autoscaling is only half a system; the other half is a workload that survives
being scaled. Every scale-down sends a pod `SIGTERM` and then `SIGKILL` after
`terminationGracePeriodSeconds`, so a queue consumer that ignores the signal loses
or double-processes in-flight messages. This exercise builds the drainable worker:
on shutdown it stops pulling, lets in-flight handlers finish inside a deadline,
cleanly abandons anything that overruns so it redelivers, and flips its readiness
probe to not-ready.

This module is fully self-contained and pure standard library: it defines the
worker, an in-memory fake queue for the demo and tests, and drives shutdown by
cancelling a context rather than sending real signals, so the tests are
deterministic. Nothing here imports another exercise.

## What you'll build

```text
drainworker/                 independent module: example.com/drainworker
  go.mod                     go 1.24
  worker.go                  Message, Queue, Handler, Worker: Run, ReadyHandler, drain
  cmd/
    demo/
      main.go                signal.NotifyContext wiring; self-SIGTERM to show a clean drain
  worker_test.go             fake queue; clean drain, deadline-abandon, no-pull-after-shutdown
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: a `Worker` bounded pool whose `Run(ctx)` consumes until `ctx` is cancelled, then drains in-flight handlers within a deadline, abandons (Nacks) overruns, and serves a readiness probe that fails while draining.
- Test: cancel the context to simulate `SIGTERM`; assert a fast in-flight handler is Acked, a handler that overruns the deadline is Nacked (redelivered), no `Pull` happens after shutdown, and the probe returns 503 once draining.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/workers/drainworker/cmd/demo
cd ~/workers/drainworker
go mod init example.com/drainworker
go mod edit -go=1.24
```

### Two clocks of shutdown: stop pulling, but let work finish

The subtlety that makes graceful drain correct is that shutdown is not one event
but two overlapping lifecycles. When `SIGTERM` arrives you must *immediately* stop
pulling new messages — otherwise you pull work you cannot possibly finish before
`SIGKILL`. But the messages already in flight should be allowed to *finish*, up to
a bounded grace window that mirrors `terminationGracePeriodSeconds`. If you tie
the handlers to the same context that signals shutdown, they are cancelled the
instant `SIGTERM` lands and nothing drains.

The design separates the two. `Run` receives the shutdown context `ctx` (in
production, from `signal.NotifyContext`). Worker goroutines use `ctx` for
*pulling*: once it is cancelled they stop reading the queue. Handlers, however,
run under a *different* context, `workCtx`, built with
`context.WithCancel(context.WithoutCancel(ctx))`. `context.WithoutCancel` returns
a context that carries `ctx`'s values but is not cancelled when `ctx` is — so a
`SIGTERM` does not reach in-flight handlers. `workCtx` is cancelled only by us,
and only when the drain deadline elapses. That is the mechanism that lets a
handler keep running for a few seconds after `SIGTERM` yet still be force-stopped
if it overruns.

### At-least-once means abandon, not drop

When the drain deadline hits and a handler is still running, cancelling `workCtx`
makes that handler observe `ctx.Done()` and return. The worker treats a handler
error (including a context error) as an *abandon*: it calls `Nack`, which under
at-least-once delivery hands the message back to the broker for redelivery to
another replica. The alternative — silently dropping it, or letting the process
die mid-handler — either loses the message or, if the broker's visibility timeout
expires, redelivers a message the dead pod may have half-applied. Abandon-plus-
idempotency is the only combination that is safe: the message is guaranteed to be
retried, and because the handler is idempotent, retrying a partially-applied
message converges to the same result.

Note the `Ack`/`Nack` calls use `context.WithoutCancel(ctx)` as their own
context. A message dequeued before shutdown must be *resolved* one way or the
other even after the drain deadline has cancelled `workCtx`; using a cancel-free
context guarantees the acknowledgement itself is never skipped because time ran
out.

### Readiness is a routing signal, not a liveness signal

The moment drain begins, `Run` flips an `atomic.Bool` so the readiness handler
returns 503. Kubernetes reacts by removing the pod from Service endpoints, so
load balancers and any HTTP surface stop routing to a pod that is on its way out.
Readiness is distinct from liveness: a draining pod is *healthy* (do not restart
it), it is simply *not ready for new traffic*. Failing readiness on `SIGTERM` is
the standard way to bleed traffic off before the process exits.

Create `worker.go`:

```go
package drainworker

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Message is a unit of work pulled from the queue.
type Message struct {
	ID   string
	Body []byte
}

// Queue is the minimal broker surface the worker needs. A real implementation
// wraps SQS, a Redis list, or a Kafka consumer. Ack confirms processing; Nack
// abandons a message so the broker redelivers it (at-least-once delivery).
type Queue interface {
	Pull(ctx context.Context) (Message, bool, error)
	Ack(ctx context.Context, m Message) error
	Nack(ctx context.Context, m Message) error
}

// Handler processes one message. A non-nil error abandons the message so it
// redelivers; handlers must therefore be idempotent.
type Handler func(ctx context.Context, m Message) error

// pullBackoff is how long a worker waits after a failed Pull before retrying,
// so a persistently erroring broker (a flapping SQS/Redis/Kafka connection)
// cannot spin the loop into a CPU-pegging busy-retry.
const pullBackoff = 100 * time.Millisecond

// Worker is a bounded pool that consumes from a Queue and drains cleanly on
// shutdown: it stops pulling, lets in-flight handlers finish within a deadline,
// abandons whatever overruns the deadline, and reports not-ready while draining.
type Worker struct {
	queue        Queue
	handler      Handler
	workers      int
	drainTimeout time.Duration

	inflight atomic.Int64
	ready    atomic.Bool
}

// Option configures a Worker.
type Option func(*Worker)

// WithWorkers sets the pool size (default 1).
func WithWorkers(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.workers = n
		}
	}
}

// WithDrainTimeout sets how long in-flight handlers may keep running after
// shutdown begins before they are abandoned. It mirrors the pod's
// terminationGracePeriodSeconds and should be a little shorter than it.
func WithDrainTimeout(d time.Duration) Option {
	return func(w *Worker) { w.drainTimeout = d }
}

// NewWorker builds a Worker for the given queue and handler.
func NewWorker(q Queue, h Handler, opts ...Option) *Worker {
	w := &Worker{queue: q, handler: h, workers: 1, drainTimeout: 25 * time.Second}
	for _, o := range opts {
		o(w)
	}
	return w
}

// InFlight reports how many handlers are currently executing.
func (w *Worker) InFlight() int64 { return w.inflight.Load() }

// ReadyHandler is a Kubernetes readiness probe: 200 while serving, 503 once
// draining, so the control plane stops routing new traffic to a departing pod.
func (w *Worker) ReadyHandler(rw http.ResponseWriter, _ *http.Request) {
	if w.ready.Load() {
		rw.WriteHeader(http.StatusOK)
		return
	}
	rw.WriteHeader(http.StatusServiceUnavailable)
}

// Run consumes until ctx is cancelled (SIGTERM), then drains and returns
// ctx.Err() once the pool has stopped.
func (w *Worker) Run(ctx context.Context) error {
	w.ready.Store(true)

	// Handlers run under workCtx, which is independent of ctx: a SIGTERM does
	// not instantly cancel in-flight work. workCtx is cancelled only when the
	// drain deadline elapses, forcing overrunning handlers to abandon.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	var wg sync.WaitGroup
	for range w.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, workCtx)
		}()
	}

	<-ctx.Done()         // shutdown requested
	w.ready.Store(false) // fail readiness immediately so traffic drains off

	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	timer := time.NewTimer(w.drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
		// every in-flight handler finished within the grace window
	case <-timer.C:
		cancelWork() // force remaining handlers to abandon and redeliver
		<-drained
	}
	return ctx.Err()
}

// loop is one worker goroutine. It pulls with pullCtx (so it stops pulling on
// shutdown) and handles with workCtx (so in-flight work survives until drain).
// A failed Pull backs off for pullBackoff before retrying so a broken broker
// cannot turn the loop into a busy-spin, while still returning promptly on
// shutdown.
func (w *Worker) loop(pullCtx, workCtx context.Context) {
	for {
		if pullCtx.Err() != nil {
			return
		}
		m, ok, err := w.queue.Pull(pullCtx)
		if err != nil || !ok {
			if pullCtx.Err() != nil {
				return
			}
			if err != nil {
				// A persistent Pull error (a flapping broker, a network
				// blip) must not spin: back off briefly before retrying,
				// but stay responsive to shutdown so drain is never delayed.
				select {
				case <-pullCtx.Done():
					return
				case <-time.After(pullBackoff):
				}
			}
			continue
		}
		w.handle(workCtx, m)
	}
}

// handle runs one message and acks on success or abandons on error/timeout.
// Ack/Nack use a cancel-free context so a message dequeued before shutdown is
// always resolved, even after the drain deadline cancels workCtx.
func (w *Worker) handle(ctx context.Context, m Message) {
	w.inflight.Add(1)
	defer w.inflight.Add(-1)

	resolve := context.WithoutCancel(ctx)
	if err := w.handler(ctx, m); err != nil {
		_ = w.queue.Nack(resolve, m)
		return
	}
	_ = w.queue.Ack(resolve, m)
}
```

### The runnable demo

The demo wires the real production signal path with `signal.NotifyContext`, then
simulates the orchestrator by sending `SIGTERM` to its own process once all work
is done — so you can watch a clean drain end to end without an external kill. A
single worker keeps the output order deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"example.com/drainworker"
)

// memQueue is a tiny in-memory Queue for the demo.
type memQueue struct {
	ch            chan drainworker.Message
	acked, nacked atomic.Int64
}

func newMemQueue(capacity int) *memQueue {
	return &memQueue{ch: make(chan drainworker.Message, capacity)}
}

func (q *memQueue) add(m drainworker.Message) { q.ch <- m }

func (q *memQueue) Pull(ctx context.Context) (drainworker.Message, bool, error) {
	if err := ctx.Err(); err != nil {
		return drainworker.Message{}, false, err
	}
	select {
	case <-ctx.Done():
		return drainworker.Message{}, false, ctx.Err()
	case m := <-q.ch:
		return m, true, nil
	}
}

func (q *memQueue) Ack(context.Context, drainworker.Message) error {
	q.acked.Add(1)
	return nil
}

func (q *memQueue) Nack(context.Context, drainworker.Message) error {
	q.nacked.Add(1)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	q := newMemQueue(3)
	for i := 1; i <= 3; i++ {
		q.add(drainworker.Message{ID: fmt.Sprintf("msg-%d", i)})
	}

	done := make(chan struct{})
	var processed atomic.Int64
	h := func(_ context.Context, m drainworker.Message) error {
		fmt.Println("processed", m.ID)
		if processed.Add(1) == 3 {
			close(done)
		}
		return nil
	}

	w := drainworker.NewWorker(q, h, drainworker.WithWorkers(1), drainworker.WithDrainTimeout(2*time.Second))
	runDone := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(runDone)
	}()

	<-done
	// A Kubernetes scale-down sends SIGTERM; simulate it here so the demo ends.
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGTERM)

	<-runDone
	fmt.Printf("drained: acked=%d nacked=%d\n", q.acked.Load(), q.nacked.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed msg-1
processed msg-2
processed msg-3
drained: acked=3 nacked=0
```

### Tests

The tests replace real signals with a cancellable context, which is what makes
them deterministic: `cancel()` is the `SIGTERM`. A handler that signals a
`started` channel on entry lets each test know precisely when a message is in
flight, removing every race from the assertions. `TestDrainAllowsInFlightToFinish`
cancels while a handler is mid-flight and then releases it before the deadline,
asserting an `Ack` and a 503 probe. `TestDrainDeadlineAbandonsOverrun` uses a
handler that never returns on its own and a tiny drain timeout, asserting the
overrun is `Nack`ed for redelivery. `TestNoPullAfterShutdown` proves the worker
stops reading the queue on shutdown: with one worker and five queued messages,
exactly one is pulled and the other four remain for another replica.

Create `worker_test.go`:

```go
package drainworker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// testQueue is an in-memory Queue that records ack/nack outcomes and pulls.
type testQueue struct {
	ch     chan Message
	mu     sync.Mutex
	acked  []string
	nacked []string
	pulls  int
}

func newTestQueue(capacity int) *testQueue {
	return &testQueue{ch: make(chan Message, capacity)}
}

func (q *testQueue) add(m Message) { q.ch <- m }

func (q *testQueue) Pull(ctx context.Context) (Message, bool, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, false, err
	}
	select {
	case <-ctx.Done():
		return Message{}, false, ctx.Err()
	case m := <-q.ch:
		q.mu.Lock()
		q.pulls++
		q.mu.Unlock()
		return m, true, nil
	}
}

func (q *testQueue) Ack(_ context.Context, m Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.acked = append(q.acked, m.ID)
	return nil
}

func (q *testQueue) Nack(_ context.Context, m Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nacked = append(q.nacked, m.ID)
	return nil
}

func (q *testQueue) stats() (acked, nacked, pulls, remaining int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.acked), len(q.nacked), q.pulls, len(q.ch)
}

func readyCode(w *Worker) int {
	rec := httptest.NewRecorder()
	w.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

func TestDrainAllowsInFlightToFinish(t *testing.T) {
	t.Parallel()
	q := newTestQueue(4)
	q.add(Message{ID: "m1"})

	started := make(chan string, 1)
	release := make(chan struct{})
	h := func(ctx context.Context, m Message) error {
		started <- m.ID
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	w := NewWorker(q, h, WithWorkers(1), WithDrainTimeout(2*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-started // m1 is in flight
	if code := readyCode(w); code != http.StatusOK {
		t.Fatalf("ready code while serving = %d, want 200", code)
	}

	cancel()       // SIGTERM
	close(release) // let the in-flight handler finish inside the grace window
	<-done

	acked, nacked, _, _ := q.stats()
	if acked != 1 || nacked != 0 {
		t.Fatalf("acked=%d nacked=%d, want 1/0 (clean drain)", acked, nacked)
	}
	if code := readyCode(w); code != http.StatusServiceUnavailable {
		t.Fatalf("ready code after drain = %d, want 503", code)
	}
}

func TestDrainDeadlineAbandonsOverrun(t *testing.T) {
	t.Parallel()
	q := newTestQueue(4)
	q.add(Message{ID: "slow"})

	started := make(chan string, 1)
	h := func(ctx context.Context, m Message) error {
		started <- m.ID
		<-ctx.Done() // never finishes on its own
		return ctx.Err()
	}

	w := NewWorker(q, h, WithWorkers(1), WithDrainTimeout(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-started
	cancel()
	<-done

	acked, nacked, _, _ := q.stats()
	if acked != 0 || nacked != 1 {
		t.Fatalf("acked=%d nacked=%d, want 0/1 (abandon on deadline)", acked, nacked)
	}
}

func TestNoPullAfterShutdown(t *testing.T) {
	t.Parallel()
	q := newTestQueue(8)
	for i := range 5 {
		q.add(Message{ID: fmt.Sprintf("m%d", i)})
	}

	started := make(chan string, 1)
	release := make(chan struct{})
	h := func(_ context.Context, m Message) error {
		started <- m.ID
		<-release
		return nil
	}

	w := NewWorker(q, h, WithWorkers(1), WithDrainTimeout(2*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-started      // exactly one message pulled and in flight
	cancel()       // shutdown before the worker can pull again
	close(release) // finish the in-flight one
	<-done

	acked, nacked, pulls, remaining := q.stats()
	if pulls != 1 {
		t.Fatalf("pulls = %d, want 1 (no pull after shutdown)", pulls)
	}
	if acked != 1 || nacked != 0 {
		t.Fatalf("acked=%d nacked=%d, want 1/0", acked, nacked)
	}
	if remaining != 4 {
		t.Fatalf("remaining = %d, want 4 left for another replica", remaining)
	}
}

func Example() {
	q := newTestQueue(1)
	q.add(Message{ID: "job-1"})

	handled := make(chan struct{})
	h := func(_ context.Context, m Message) error {
		fmt.Println("handled", m.ID)
		close(handled)
		return nil
	}

	w := NewWorker(q, h, WithWorkers(1))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	<-handled // wait until the message is processed
	cancel()  // in production a SIGTERM triggers this
	<-done
	fmt.Println("drained cleanly")
	// Output:
	// handled job-1
	// drained cleanly
}
```

## Review

The worker is correct when shutdown separates pulling from finishing. On
cancellation, `loop` stops calling `Pull` (it checks `pullCtx.Err()` at the top
and `Pull` short-circuits on a cancelled context), while in-flight handlers keep
running under `workCtx` until either they finish or the drain timer fires and
`cancelWork` abandons them. `TestNoPullAfterShutdown` proves the first property,
`TestDrainAllowsInFlightToFinish` the clean-finish path, and
`TestDrainDeadlineAbandonsOverrun` the forced-abandon path.

The mistakes to avoid: do not run handlers under the shutdown context — that
cancels them the instant `SIGTERM` lands and nothing drains; derive a separate
context with `context.WithoutCancel` and cancel it only at the deadline. Do not
let an overrunning handler be dropped or the process die mid-handler; abandon it
with `Nack` so at-least-once redelivery retries it on another replica, and keep
every handler idempotent so a retry is safe. Do not forget to fail readiness at
the start of drain, or Kubernetes keeps routing new requests to a departing pod.
Finally, resolve every dequeued message with a cancel-free context so the
acknowledgement itself is never skipped because the deadline expired. And do not
`continue` straight back into `Pull` after an error: the in-memory fakes here
only ever fail via context cancellation, but a real SQS, Redis, or Kafka client
can return a persistent error against a flapping broker, and a bare retry loop
would peg a CPU — back off for `pullBackoff` (staying responsive to shutdown)
before retrying. Run
`go test -race` to confirm the `inflight` counter, `ready` flag, and queue are
free of data races under the concurrent pool.

## Resources

- [`os/signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) — turning `SIGTERM` into a cancellable context.
- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — deriving a context that survives its parent's cancellation.
- [Kubernetes: Pod termination and terminationGracePeriodSeconds](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) — the SIGTERM-then-SIGKILL sequence a drain must fit inside.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-external-scaler-grpc.md](01-external-scaler-grpc.md) | Next: [03-prometheus-exporter-scaledobject.md](03-prometheus-exporter-scaledobject.md)
