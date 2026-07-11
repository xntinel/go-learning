# Exercise 12: An Outbox Relay That Joins Every In-Flight Dispatch On Stop

**Level: Intermediate**

A transactional-outbox relay polls unsent rows and hands each to a downstream sink
in its own goroutine, so one slow sink never stalls the poll loop. The naive Stop
cancels the poller and returns, which leaks every dispatch goroutine still talking
to the sink and silently drops their at-least-once delivery during a burst. This
exercise builds Start/Stop so Stop cancels the poller, joins all outstanding
dispatch goroutines within a deadline, and honestly reports how many were still in
flight when the deadline fired.

This module is self-contained: its own module, an `outbox` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26
  outbox.go                  Relay with Start/Stop, WithMaxInFlight, Dispatched, ErrDrainTimeout
  cmd/demo/main.go           runnable demo: a clean drain and a wedged-sink Stop timeout
  outbox_test.go             clean drain, burst join, drain-timeout + re-Stop, high-water bound
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `New(f Fetcher, s Sink, opts ...Option) *Relay`, `WithMaxInFlight(n int) Option`, `(*Relay).Start(ctx)`, `(*Relay).Stop(ctx) error`, `(*Relay).Dispatched() int64`, and `ErrDrainTimeout`.
- Test: every fetched row is dispatched once on a clean drain; Stop joins an in-flight burst; a too-short deadline returns `ErrDrainTimeout` carrying the in-flight count and re-Stopping is clean; in-flight never exceeds `WithMaxInFlight`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/outbox/cmd/demo
cd ~/go-exercises/outbox
go mod init example.com/outbox
go get go.uber.org/goleak
go mod tidy
```

### Cancelling the poller is not the same as stopping the work

The relay has two kinds of goroutine, and Stop must treat them differently. There
is exactly one *poll* goroutine: it loops calling the `Fetcher`, and for each row it
launches a *dispatch* child that runs the `Sink`. The reason for the fan-out is
throughput — a sink that takes 200ms must not hold up the next poll — but the fan-out
is precisely what leaks if Stop is careless.

The naive Stop signals the poller to quit and returns. That is the shape-6 leak from
the concepts file: signalling stop is not joining. The dispatch children are still
running the sink; the process now has N orphaned goroutines whose deliveries no one
is waiting for, and if the caller tears the process down they are dropped mid-flight,
violating the at-least-once contract the outbox exists to provide. The fix is a Stop
that does three things in order:

1. Cancel the poll loop so no *new* dispatch is started. Un-fetched rows are left
   unsent in the outbox table; that is correct — they will be picked up on the next
   Start. Cancelling the poller drops nothing that was already promised.
2. Join every dispatch child that is already in flight, bounded by the Stop context.
   The children must run to completion, so they watch the *caller's* context, not the
   poller's cancelled one — cancelling the poller must never abort a delivery that has
   already begun.
3. If the join deadline fires before the children finish, return `ErrDrainTimeout`
   naming how many are still running, rather than pretending shutdown succeeded.

The ownership rule that makes this leak-proof: the poll goroutine is the *only*
producer of dispatch children, so once it has returned (its `pollDone` is closed) no
further `wg.Go` can happen, and a plain `wg.Wait()` is guaranteed to observe the
final count. Joining before the poller has stopped would race an `Add` against the
`Wait`. Joining after is safe.

`WithMaxInFlight(n)` bounds the fan-out with a counting semaphore — a buffered
channel of `n` slots. The poll loop acquires a slot before launching a child and the
child releases it when the sink returns, so at most `n` dispatches run at once. That
protects the downstream from an unbounded burst, and the test verifies it with an
atomic high-water gauge that must never cross `n`.

Create `outbox.go`:

```go
// Package outbox implements a transactional-outbox relay that polls unsent rows
// and dispatches each to a downstream sink in its own goroutine, so a slow sink
// never stalls the poll loop. Stop cancels the poller AND joins every in-flight
// dispatch goroutine within a deadline, reporting how many were still running.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrDrainTimeout is returned by Stop when dispatch goroutines are still running
// when the Stop deadline fires. The wrapped message names the in-flight count.
var ErrDrainTimeout = errors.New("outbox: dispatches still in flight at Stop deadline")

const defaultMaxInFlight = 64

// Row is one unsent outbox record.
type Row struct {
	ID      int
	Payload string
}

// Fetcher returns the next batch of unsent rows. An empty slice means drained:
// the poll loop stops. A non-nil error also stops the poll loop.
type Fetcher func(ctx context.Context) ([]Row, error)

// Sink delivers one row downstream. It receives a context that is NOT cancelled
// by Stop, so an in-flight delivery runs to completion (at-least-once).
type Sink func(ctx context.Context, r Row) error

// Relay owns a single poll goroutine and a fan-out of dispatch children. The wg
// joins the children; the semaphore bounds concurrency; cancelPoll stops the
// poller without cancelling in-flight dispatches.
type Relay struct {
	fetch       Fetcher
	sink        Sink
	maxInFlight int
	sem         chan struct{} // capacity == maxInFlight

	cancelPoll context.CancelFunc
	pollDone   chan struct{}

	wg       sync.WaitGroup // dispatch children
	waitOnce sync.Once
	drained  chan struct{} // closed when wg reaches zero

	dispatched atomic.Int64
	inFlight   atomic.Int64
	highWater  atomic.Int64
}

// Option configures a Relay at construction.
type Option func(*Relay)

// WithMaxInFlight caps how many dispatch goroutines run concurrently. Values
// below 1 are ignored.
func WithMaxInFlight(n int) Option {
	return func(r *Relay) {
		if n >= 1 {
			r.maxInFlight = n
		}
	}
}

// New builds a Relay. The Fetcher and Sink are required; options tune concurrency.
func New(f Fetcher, s Sink, opts ...Option) *Relay {
	r := &Relay{
		fetch:       f,
		sink:        s,
		maxInFlight: defaultMaxInFlight,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.sem = make(chan struct{}, r.maxInFlight)
	return r
}

// Start launches the single poll goroutine. Each fetched row is dispatched via a
// wg-tracked child running under the semaphore. The poll loop watches its own
// derived context; the dispatch children watch the caller's ctx, so Stop's
// poll-cancel does not abort a delivery already in flight.
func (r *Relay) Start(ctx context.Context) {
	pctx, cancel := context.WithCancel(ctx)
	r.cancelPoll = cancel
	r.pollDone = make(chan struct{})

	go func() {
		defer close(r.pollDone)
		for {
			rows, err := r.fetch(pctx)
			if err != nil || len(rows) == 0 {
				return
			}
			for _, row := range rows {
				// Acquire a slot before spawning; block until one frees or the
				// poller is cancelled. This bounds fan-out at maxInFlight.
				select {
				case r.sem <- struct{}{}:
				case <-pctx.Done():
					return
				}
				r.wg.Go(func() {
					defer func() { <-r.sem }()
					r.dispatchOne(ctx, row)
				})
			}
		}
	}()
}

// dispatchOne delivers one row and maintains the in-flight gauge and high-water
// mark. inFlight is incremented for the whole life of the delivery, so it never
// exceeds the number of held semaphore slots (== maxInFlight).
func (r *Relay) dispatchOne(ctx context.Context, row Row) {
	cur := r.inFlight.Add(1)
	for {
		hw := r.highWater.Load()
		if cur <= hw || r.highWater.CompareAndSwap(hw, cur) {
			break
		}
	}
	defer r.inFlight.Add(-1)

	_ = r.sink(ctx, row)
	r.dispatched.Add(1)
}

// Stop cancels the poll loop and joins every outstanding dispatch child, bounded
// by ctx. It returns nil on a clean drain, or ErrDrainTimeout naming the count
// still in flight when ctx fired. Stop is safe to call more than once: after a
// timeout, release the wedged sink and call Stop again to confirm a clean join.
func (r *Relay) Stop(ctx context.Context) error {
	if r.cancelPoll != nil {
		r.cancelPoll()
		<-r.pollDone // poller has returned: no further wg.Go calls happen.
	}
	// One persistent waiter drains the WaitGroup; repeated Stops share it, so a
	// timeout never spawns a goroutine that outlives the eventual drain.
	r.waitOnce.Do(func() {
		r.drained = make(chan struct{})
		go func() {
			r.wg.Wait()
			close(r.drained)
		}()
	})

	select {
	case <-r.drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w (%d in flight)", ErrDrainTimeout, r.inFlight.Load())
	}
}

// Dispatched returns the number of rows whose delivery has completed.
func (r *Relay) Dispatched() int64 {
	return r.dispatched.Load()
}
```

### The runnable demo

The demo runs two scenarios. The first drains a small batch cleanly and shows every
row dispatched. The second wedges a sink with `WithMaxInFlight(1)`, so a 100ms Stop
deadline reports exactly one dispatch still in flight; releasing the sink and
re-Stopping then joins cleanly. Both are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/outbox"
)

// batchFetcher hands out rows in fixed-size batches, then reports drained. It
// closes the returned channel the first time it drains.
func batchFetcher(rows []outbox.Row, batch int) (outbox.Fetcher, <-chan struct{}) {
	drained := make(chan struct{})
	var mu sync.Mutex
	var once sync.Once
	i := 0
	f := func(ctx context.Context) ([]outbox.Row, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(rows) {
			once.Do(func() { close(drained) })
			return nil, nil
		}
		end := min(i+batch, len(rows))
		out := rows[i:end]
		i = end
		return out, nil
	}
	return f, drained
}

func rowsUpTo(n int) []outbox.Row {
	out := make([]outbox.Row, n)
	for i := range n {
		out[i] = outbox.Row{ID: i + 1, Payload: fmt.Sprintf("evt-%d", i+1)}
	}
	return out
}

func main() {
	// Scenario A: clean drain. Every fetched row is dispatched exactly once.
	{
		sink := func(ctx context.Context, r outbox.Row) error { return nil }
		f, drained := batchFetcher(rowsUpTo(5), 2)
		r := outbox.New(f, sink)
		r.Start(context.Background())
		<-drained // let the poll loop dispatch every row before shutting down

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := r.Stop(ctx)
		cancel()
		fmt.Printf("clean drain: dispatched=%d err=%v\n", r.Dispatched(), err)
	}

	// Scenario B: a wedged sink. maxInFlight=1 pins the in-flight count at 1, so
	// a too-short Stop deadline reports exactly one dispatch still running.
	{
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		sink := func(ctx context.Context, r outbox.Row) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return nil
		}
		f, _ := batchFetcher(rowsUpTo(100), 10)
		r := outbox.New(f, sink, outbox.WithMaxInFlight(1))
		r.Start(context.Background())

		<-started // one dispatch is now wedged in the sink

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := r.Stop(ctx)
		cancel()
		fmt.Printf("wedged sink: is ErrDrainTimeout=%v\n", errors.Is(err, outbox.ErrDrainTimeout))
		fmt.Printf("wedged sink: %v\n", err)

		close(release) // let the in-flight dispatch finish

		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		err2 := r.Stop(ctx2)
		cancel2()
		fmt.Printf("after release: dispatched=%d err=%v\n", r.Dispatched(), err2)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean drain: dispatched=5 err=<nil>
wedged sink: is ErrDrainTimeout=true
wedged sink: outbox: dispatches still in flight at Stop deadline (1 in flight)
after release: dispatched=1 err=<nil>
```

### Tests

`TestMain` installs `goleak.VerifyTestMain(m)`, the parallel-safe leak check that
runs once after every test: a relay that forgets to join a dispatch child fails
here, named. `TestCleanDrain` waits for the fetcher to drain naturally, then asserts
`Dispatched()` equals the row count and `Stop` returns nil. `TestStopJoinsBurstOfSlowSinks`
wedges a full burst of dispatches in the sink, calls `Stop` concurrently, and
confirms it joins every child (nil error, zero in flight). `TestStopTimeoutReportsInFlightThenReStopClean`
pins the honest-report behavior: a 50ms deadline against a wedged sink returns
`ErrDrainTimeout` carrying an in-flight count of 1, and re-Stopping after release is
clean with zero survivors. `TestHighWaterNeverExceedsMax` proves the semaphore bound —
the high-water gauge reaches the cap and never crosses it.

Create `outbox_test.go`:

```go
package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// VerifyTestMain runs one goroutine-leak check after every test in the package
// has finished. It is the parallel-safe entry point; a relay that forgets to
// join its dispatch children fails here, naming the surviving goroutine.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// sliceFetcher hands out rows in fixed-size batches, then reports drained. It
// closes the returned channel the first time it returns the empty (drained)
// batch, so a test can wait for a natural drain before calling Stop.
func sliceFetcher(rows []Row, batch int) (Fetcher, <-chan struct{}) {
	drained := make(chan struct{})
	var mu sync.Mutex
	var once sync.Once
	i := 0
	f := func(ctx context.Context) ([]Row, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(rows) {
			once.Do(func() { close(drained) })
			return nil, nil
		}
		end := min(i+batch, len(rows))
		out := rows[i:end]
		i = end
		return out, nil
	}
	return f, drained
}

func rowsN(n int) []Row {
	out := make([]Row, n)
	for i := range n {
		out[i] = Row{ID: i + 1, Payload: "p"}
	}
	return out
}

func generousCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestCleanDrain: after the fetcher drains naturally, every fetched row is
// dispatched exactly once, Stop returns nil, and goleak (via TestMain) confirms
// no dispatch goroutine survives.
func TestCleanDrain(t *testing.T) {
	const total = 20
	sink := func(ctx context.Context, r Row) error { return nil }
	f, drained := sliceFetcher(rowsN(total), 3)
	r := New(f, sink)
	r.Start(context.Background())

	<-drained // the poll loop has fetched and dispatched every row

	if err := r.Stop(generousCtx(t)); err != nil {
		t.Fatalf("Stop returned %v, want nil", err)
	}
	if got := r.Dispatched(); got != total {
		t.Fatalf("Dispatched()=%d, want %d", got, total)
	}
	if n := r.inFlight.Load(); n != 0 {
		t.Fatalf("in flight after clean Stop = %d, want 0", n)
	}
}

// TestStopJoinsBurstOfSlowSinks: with a full burst of dispatches wedged in the
// sink, Stop is invoked concurrently and must join every in-flight child before
// returning. Synchronization is by channels, not sleeps.
func TestStopJoinsBurstOfSlowSinks(t *testing.T) {
	const total, maxIF = 40, 8
	started := make(chan struct{}, total)
	release := make(chan struct{})
	sink := func(ctx context.Context, r Row) error {
		started <- struct{}{}
		<-release
		return nil
	}
	f, _ := sliceFetcher(rowsN(total), 5)
	r := New(f, sink, WithMaxInFlight(maxIF))
	r.Start(context.Background())

	for range maxIF { // a full burst is now wedged in the sink
		<-started
	}

	errCh := make(chan error, 1)
	go func() { errCh <- r.Stop(generousCtx(t)) }()
	close(release) // let the wedged children finish so the join can complete
	err := <-errCh

	if err != nil {
		t.Fatalf("Stop returned %v, want nil", err)
	}
	if n := r.inFlight.Load(); n != 0 {
		t.Fatalf("in flight after Stop = %d, want 0", n)
	}
	if got := r.Dispatched(); got < maxIF || got > total {
		t.Fatalf("Dispatched()=%d, want in [%d,%d]", got, maxIF, total)
	}
}

// TestStopTimeoutReportsInFlightThenReStopClean: a wedged sink and a too-short
// deadline make Stop return ErrDrainTimeout carrying a positive in-flight count;
// releasing the sink and re-Stopping then joins cleanly with zero survivors.
func TestStopTimeoutReportsInFlightThenReStopClean(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	sink := func(ctx context.Context, r Row) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	f, _ := sliceFetcher(rowsN(100), 10)
	r := New(f, sink, WithMaxInFlight(1))
	r.Start(context.Background())

	<-started // exactly one dispatch is wedged (maxInFlight == 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := r.Stop(ctx)
	cancel()
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("Stop returned %v, want ErrDrainTimeout", err)
	}
	if n := r.inFlight.Load(); n != 1 {
		t.Fatalf("in flight at deadline = %d, want 1", n)
	}

	close(release) // let the wedged dispatch finish

	if err := r.Stop(generousCtx(t)); err != nil {
		t.Fatalf("re-Stop returned %v, want nil", err)
	}
	if n := r.inFlight.Load(); n != 0 {
		t.Fatalf("in flight after re-Stop = %d, want 0", n)
	}
}

// TestHighWaterNeverExceedsMax: concurrency reaches WithMaxInFlight and never
// crosses it. The gauge equals the cap exactly once a full burst is confirmed.
func TestHighWaterNeverExceedsMax(t *testing.T) {
	const total, maxIF = 64, 4
	started := make(chan struct{}, total)
	release := make(chan struct{})
	sink := func(ctx context.Context, r Row) error {
		started <- struct{}{}
		<-release
		return nil
	}
	f, _ := sliceFetcher(rowsN(total), 8)
	r := New(f, sink, WithMaxInFlight(maxIF))
	r.Start(context.Background())

	for range maxIF { // all maxIF children have incremented the gauge
		<-started
	}
	if hw := r.highWater.Load(); hw != maxIF {
		t.Fatalf("high-water = %d, want %d", hw, maxIF)
	}

	close(release)
	if err := r.Stop(generousCtx(t)); err != nil {
		t.Fatalf("Stop returned %v, want nil", err)
	}
	if hw := r.highWater.Load(); hw > maxIF {
		t.Fatalf("high-water = %d exceeded max %d", hw, maxIF)
	}
}
```

## Review

Correct here means Stop satisfies both halves of shutdown: it cancels the poll loop
so no new dispatch starts, and it *joins* every dispatch child that was already in
flight instead of orphaning them. The invariant that makes the join sound is that the
single poll goroutine is the only producer of children, so once its `pollDone` is
closed a plain `wg.Wait()` observes the final, stable count with no Add/Wait race; the
children run under the caller's context, not the poller's cancelled one, so a delivery
in progress finishes rather than aborting. `TestStopJoinsBurstOfSlowSinks` and
`goleak.VerifyTestMain` together prove nothing survives, `TestStopTimeoutReportsInFlightThenReStopClean`
proves the honest `ErrDrainTimeout` report and that a second Stop after release is
clean, and the high-water gauge proves the semaphore actually bounds the fan-out. The
production bug this prevents is the classic outbox relay that "shuts down" by
cancelling its poller and returning, leaking every in-flight sender and dropping their
at-least-once deliveries the instant the process exits.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 launch-and-join primitive that removes the Add/Done race the poll loop would otherwise risk.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) -- how the poller gets a cancellable context that Stop trips without touching the dispatch children.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector whose `VerifyTestMain` fails the package if a dispatch child is not joined.
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the foundational treatment of fan-out, cancellation, and joining goroutines cleanly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-lease-renewal-keepalive-leak.md](11-lease-renewal-keepalive-leak.md) | Next: [13-request-coalescing-cancel-safe-no-leak.md](13-request-coalescing-cancel-safe-no-leak.md)
