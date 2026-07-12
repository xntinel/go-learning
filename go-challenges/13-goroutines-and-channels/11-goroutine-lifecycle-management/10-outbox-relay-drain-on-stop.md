# Exercise 10: An Outbox Relay That Flushes Pending Events on Stop

**Level: Intermediate**

The transactional-outbox pattern writes a domain event to the same database as
the business change, then a relay ships those rows to a message bus. The relay
runs one background goroutine that batches pending rows and dispatches them on a
ticker. The naive lifecycle loses data on every deploy: when the pod receives
SIGTERM the goroutine is told to leave and it leaves, dropping whatever it had
buffered but not yet dispatched. The fix is to make `Stop` do teardown *work* —
a final, deadline-bounded flush of everything still pending — rather than merely
returning. This exercise builds that relay.

This module is self-contained: its own module, an `outbox` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26
  outbox.go                  type Relay; New, Start, Enqueue, Stop, Dispatched; drain-on-stop
  cmd/demo/main.go           runnable demo: enqueue, stop, watch the final flush deliver everything
  outbox_test.go             no-loss-on-stop, batch-size, idempotent-stop, closed/started guards, goleak
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `New(sink Sink, batchSize int, interval time.Duration) *Relay`; `Start(ctx) error` guarding double start with `ErrAlreadyStarted`; `Enqueue(e Event) error` returning `ErrClosed` after stop; `Stop(ctx) error` that signals, waits, then does a deadline-bounded final flush, returning `ErrNotStarted` when idle; `Dispatched() int64`.
- Test: every enqueued event reaches the sink including those pending at `Stop`; no batch exceeds `batchSize`; `Stop` is signal-then-wait and leaves no goroutine behind; a second `Stop` returns `ErrNotStarted`; `Enqueue` after `Stop` returns `ErrClosed`; a second `Start` returns `ErrAlreadyStarted`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/10-outbox-relay-drain-on-stop/cmd/demo
cd go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/10-outbox-relay-drain-on-stop
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### Stop that flushes: signal, wait, then drain alone

A background loop with a bare `close(stop); <-done` shutdown is correct for a
worker that owns no unflushed state. An outbox relay does own unflushed state:
between two ticks it accumulates rows in a buffer, and a stop signal that arrives
in that window discards them. Those rows were already committed to the database
as "pending dispatch," so dropping them is silent data loss that only shows up
downstream as missing messages. The lifecycle contract therefore has to be:
`Stop` guarantees delivery of everything enqueued before it was called.

The mechanism sequences three steps, and the ordering is what makes it race-free:

1. **Signal.** Under the mutex, flip `started` to false and `closed` to true,
   capture the `stop` and `done` channels, then `close(stop)`. Setting `closed`
   first means any `Enqueue` that has not yet acquired the mutex will observe the
   closed state and be rejected with `ErrClosed` — so the set of events the final
   flush must deliver is frozen at this instant.
2. **Wait.** Block on `<-done` (bounded by `ctx.Done()`). The run goroutine
   returns as soon as it observes the closed `stop` channel, closing `done` on
   the way out. Waiting here is what makes the next step safe: once `done` is
   closed, the run goroutine is provably gone, so the final flush has *exclusive*
   access to the buffer. There is no lock dance between two flushers because
   there is only ever one flusher at a time.
3. **Drain.** Loop, pulling up to `batchSize` events per batch and dispatching
   each, until the buffer is empty. Before every batch, check `ctx.Done()`: this
   is the deadline bound. A graceful drain that would otherwise hang on a slow
   sink upgrades to abrupt once the shutdown deadline expires, returning
   `ctx.Err()` — the graceful-versus-abrupt trade-off sequenced by a timeout.

`Stop` on a relay that is not running returns `ErrNotStarted`. That single guard
does double duty: it rejects a stop-before-start, and it makes the *second* `Stop`
an error rather than a panic on `close` of an already-closed channel — idempotency
without a `sync.Once`, because the `started` flag already encodes the state.

The run loop dispatches on the ticker for the steady-state path, but it never
owns the shutdown flush. Ownership of the final flush belongs to `Stop`, which
runs it after the loop has exited. Dispatch batching lives in one `flushOne`
helper called by both paths; because the two callers are serialized by the
signal-then-wait ordering, that helper needs no extra coordination.

Create `outbox.go`:

```go
package outbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyStarted = errors.New("outbox: already started")
	ErrNotStarted     = errors.New("outbox: not started")
	ErrClosed         = errors.New("outbox: closed")
)

// Event is one pending outbox row: an identifier and an opaque payload.
type Event struct {
	ID      string
	Payload []byte
}

// Sink dispatches one batch. It receives the Stop context during the final
// flush, so a slow sink is bounded by the shutdown deadline.
type Sink func(ctx context.Context, batch []Event) error

// Relay owns one background goroutine that batches pending events and hands
// each batch to sink on a ticker. On Stop it performs a final, deadline-bounded
// flush of everything still buffered before the goroutine's owner returns, so
// no enqueued event is lost across a deploy.
type Relay struct {
	sink      Sink
	batchSize int
	interval  time.Duration

	mu      sync.Mutex
	buffer  []Event
	started bool
	closed  bool
	stop    chan struct{}
	done    chan struct{}

	dispatched atomic.Int64
}

// New builds an idle Relay. batchSize is clamped to at least 1.
func New(sink Sink, batchSize int, interval time.Duration) *Relay {
	if batchSize < 1 {
		batchSize = 1
	}
	return &Relay{sink: sink, batchSize: batchSize, interval: interval}
}

// Start launches the run loop. A second Start without an intervening Stop
// returns ErrAlreadyStarted rather than orphaning the first goroutine.
func (r *Relay) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.started {
		return ErrAlreadyStarted
	}
	r.started = true
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	go r.run(ctx, r.stop, r.done)
	return nil
}

// Enqueue buffers one event. After Stop it returns ErrClosed, so no event is
// accepted that the relay can no longer flush.
func (r *Relay) Enqueue(e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	r.buffer = append(r.buffer, e)
	return nil
}

// Stop signals the run loop, waits for it to exit, then performs a final flush
// of every buffered event. Both the wait and the flush are bounded by ctx: if
// the deadline hits, Stop returns ctx.Err() and stops draining. A Stop on a
// relay that is not running returns ErrNotStarted, which also makes a second
// Stop a clear error rather than a double close.
func (r *Relay) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return ErrNotStarted
	}
	r.started = false
	r.closed = true
	stop, done := r.stop, r.done
	r.mu.Unlock()

	close(stop) // signal: leave the loop
	select {    // wait: the loop has returned, so the final flush runs alone
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	// The run goroutine is gone; flushOne now has exclusive access to buffer.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := r.flushOne(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// Dispatched reports how many events have been handed to the sink.
func (r *Relay) Dispatched() int64 {
	return r.dispatched.Load()
}

func (r *Relay) run(ctx context.Context, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			for {
				n, err := r.flushOne(ctx)
				if n == 0 || err != nil {
					break
				}
			}
		}
	}
}

// flushOne removes up to batchSize events under the lock, then dispatches them
// outside the lock so Enqueue is never blocked by a slow sink. It is only ever
// called by one goroutine at a time (the run loop, or Stop after the loop has
// exited), so no batch overlaps another.
func (r *Relay) flushOne(ctx context.Context) (int, error) {
	r.mu.Lock()
	if len(r.buffer) == 0 {
		r.mu.Unlock()
		return 0, nil
	}
	n := min(r.batchSize, len(r.buffer))
	batch := make([]Event, n)
	copy(batch, r.buffer[:n])
	r.buffer = r.buffer[n:]
	r.mu.Unlock()

	if err := r.sink(ctx, batch); err != nil {
		return n, err
	}
	r.dispatched.Add(int64(n))
	return n, nil
}
```

### The runnable demo

The demo sets the interval an hour into the future so the ticker never fires:
every enqueued event is still pending when `Stop` is called, which is exactly the
shutdown-loss scenario. `Stop`'s final flush must deliver all five events, split
into batches of at most two. Buffer order is FIFO, so the batching is fully
deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/outbox"
)

func main() {
	// A deterministic sink that records each batch it receives.
	var batches [][]string
	sink := func(_ context.Context, batch []outbox.Event) error {
		ids := make([]string, len(batch))
		for i, e := range batch {
			ids[i] = e.ID
		}
		batches = append(batches, ids)
		return nil
	}

	// Interval is far in the future so the ticker never fires: every event is
	// still pending when Stop is called, exercising the drain-on-stop path.
	r := outbox.New(sink, 2, time.Hour)
	if err := r.Start(context.Background()); err != nil {
		panic(err)
	}

	for _, id := range []string{"e1", "e2", "e3", "e4", "e5"} {
		if err := r.Enqueue(outbox.Event{ID: id, Payload: []byte(id)}); err != nil {
			panic(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		panic(err)
	}

	fmt.Println("dispatched:", r.Dispatched())
	fmt.Println("batches:", len(batches))
	max := 0
	for _, b := range batches {
		if len(b) > max {
			max = len(b)
		}
	}
	fmt.Println("max batch size:", max)
	for i, b := range batches {
		fmt.Printf("batch %d: %v\n", i, b)
	}

	// Post-stop guarantees: Enqueue is rejected, a second Stop is not started.
	fmt.Println("enqueue after stop:", r.Enqueue(outbox.Event{ID: "e6"}))
	fmt.Println("second stop:", r.Stop(ctx))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dispatched: 5
batches: 3
max batch size: 2
batch 0: [e1 e2]
batch 1: [e3 e4]
batch 2: [e5]
enqueue after stop: outbox: closed
second stop: outbox: not started
```

### Tests

A `goleak.VerifyTestMain(m)` in `TestMain` fails the run if any run goroutine
survives a `Stop` — the objective proof that `Stop` is signal-then-wait and not
signal-then-hope. `TestFlushesPendingOnStop` is the core assertion: with a
one-hour interval nothing is dispatched on a tick, so every event is pending, and
`Stop` must deliver all ten in order with no batch larger than three.
`TestPeriodicFlush` covers the ticker path with a short interval, synchronizing
on the sink's signal channel instead of sleeping. `TestConcurrentEnqueueNoLoss`
enqueues 200 events from eight goroutines and asserts the sink saw exactly that
set — a `-race` magnet. `TestStopIsIdempotent`, `TestEnqueueAfterStop`,
`TestDoubleStart`, and `TestStopWithoutStart` pin the four state-guard errors.

Create `outbox_test.go`:

```go
package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// recorder is a deterministic in-memory sink: it records every event it
// receives and the length of each batch, under its own mutex.
type recorder struct {
	mu         sync.Mutex
	ids        []string
	batchSizes []int
	signal     chan struct{} // optional: pulsed once per batch, non-blocking
}

func (r *recorder) sink(_ context.Context, batch []Event) error {
	r.mu.Lock()
	for _, e := range batch {
		r.ids = append(r.ids, e.ID)
	}
	r.batchSizes = append(r.batchSizes, len(batch))
	r.mu.Unlock()
	if r.signal != nil {
		select {
		case r.signal <- struct{}{}:
		default:
		}
	}
	return nil
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

func (r *recorder) maxBatch() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := 0
	for _, s := range r.batchSizes {
		if s > m {
			m = s
		}
	}
	return m
}

func longStop() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// TestFlushesPendingOnStop pins the core invariant: with an interval that never
// fires, every event is pending when Stop is called, and Stop's final flush
// must deliver all of them in order, with no batch larger than batchSize.
func TestFlushesPendingOnStop(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	r := New(rec.sink, 3, time.Hour)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var want []string
	for i := range 10 {
		id := fmt.Sprintf("e%02d", i)
		want = append(want, id)
		if err := r.Enqueue(Event{ID: id}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := rec.seen(); !slices.Equal(got, want) {
		t.Fatalf("seen = %v, want %v", got, want)
	}
	if m := rec.maxBatch(); m > 3 {
		t.Fatalf("max batch = %d, want <= 3", m)
	}
	if n := r.Dispatched(); n != 10 {
		t.Fatalf("Dispatched = %d, want 10", n)
	}
}

// TestPeriodicFlush pins the ticker path: with a short interval the loop flushes
// on its own. The test synchronizes on the sink's signal channel rather than
// sleeping, then stops.
func TestPeriodicFlush(t *testing.T) {
	t.Parallel()

	rec := &recorder{signal: make(chan struct{}, 1)}
	r := New(rec.sink, 4, time.Millisecond)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Enqueue(Event{ID: "x"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case <-rec.signal:
	case <-time.After(5 * time.Second):
		t.Fatal("ticker never flushed the pending event")
	}

	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := rec.seen(); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("seen = %v, want [x]", got)
	}
}

// TestConcurrentEnqueueNoLoss enqueues from many goroutines, then stops, and
// asserts the sink saw exactly the enqueued set (order-independent). It is a
// -race magnet by design.
func TestConcurrentEnqueueNoLoss(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	r := New(rec.sink, 5, time.Millisecond)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const g, k = 8, 25
	var wg sync.WaitGroup
	for gi := range g {
		wg.Go(func() {
			for ki := range k {
				_ = r.Enqueue(Event{ID: fmt.Sprintf("g%d-k%d", gi, ki)})
			}
		})
	}
	wg.Wait()

	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := rec.seen()
	if len(got) != g*k {
		t.Fatalf("saw %d events, want %d", len(got), g*k)
	}
	if m := rec.maxBatch(); m > 5 {
		t.Fatalf("max batch = %d, want <= 5", m)
	}
	if n := r.Dispatched(); n != g*k {
		t.Fatalf("Dispatched = %d, want %d", n, g*k)
	}
}

// TestStopIsIdempotent pins that the second Stop returns ErrNotStarted instead
// of panicking on a double close of the stop channel.
func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	r := New((&recorder{}).sink, 2, time.Hour)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := r.Stop(ctx); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("second Stop = %v, want ErrNotStarted", err)
	}
}

// TestEnqueueAfterStop pins that Enqueue is rejected once the relay is stopped.
func TestEnqueueAfterStop(t *testing.T) {
	t.Parallel()

	r := New((&recorder{}).sink, 2, time.Hour)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := r.Enqueue(Event{ID: "late"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after Stop = %v, want ErrClosed", err)
	}
}

// TestDoubleStart pins that a second Start without a Stop is rejected rather
// than orphaning the first run goroutine.
func TestDoubleStart(t *testing.T) {
	t.Parallel()

	r := New((&recorder{}).sink, 2, time.Hour)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStopWithoutStart pins that stopping a relay that never started is a clear
// error, not a nil-channel panic.
func TestStopWithoutStart(t *testing.T) {
	t.Parallel()

	r := New((&recorder{}).sink, 2, time.Hour)
	ctx, cancel := longStop()
	defer cancel()
	if err := r.Stop(ctx); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Stop without Start = %v, want ErrNotStarted", err)
	}
}
```

## Review

The relay is correct when `Stop` delivers every event enqueued before it was
called, and does so without leaving a goroutine behind. The delivery guarantee
rests on one ordering: `Stop` sets `closed` under the mutex (freezing the event
set, since later `Enqueue` calls now fail with `ErrClosed`), signals the loop to
leave, and *waits* for `done` before draining — so the final flush runs with the
run goroutine provably gone and exclusive access to the buffer, needing no extra
lock coordination between the two flush paths. `TestFlushesPendingOnStop` proves
no loss by forcing every event to be pending (a one-hour interval never ticks)
and asserting all of them arrive, while `maxBatch` proves no batch exceeds
`batchSize`. `goleak` in `TestMain` proves the signal-then-wait discipline: a
`Stop` that returned before the loop exited would leave a goroutine and fail the
run. The single `started` flag encoding idempotency turns the classic
double-close panic into a clean `ErrNotStarted`. The production bug this pattern
prevents is the one every outbox eventually hits without it: a rolling deploy
that quietly drops the events buffered in each terminating pod, surfacing days
later as messages that were committed but never sent.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) -- the deadline that upgrades a graceful drain to an abrupt one during shutdown.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 launch-and-track helper used to fan out concurrent enqueues in the test.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- fails the test run if Stop leaves the run goroutine alive.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the close-as-broadcast signal and signal-then-wait shutdown discipline.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-once-guarded-closer.md](09-once-guarded-closer.md) | Next: [11-restartable-poller-start-stop-cycles.md](11-restartable-poller-start-stop-cycles.md)
