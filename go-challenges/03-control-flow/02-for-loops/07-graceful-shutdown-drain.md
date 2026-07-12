# Exercise 7: For-Select Event Loop with Graceful Shutdown

The `for { select { ... } }` loop is the shape of every long-lived service: a
consumer that multiplexes an input channel, a periodic ticker, and a cancellation
signal, and whose only correct exits are `ctx.Done()` and a closed input. What
separates a correct service loop from a lossy one is the *bounded drain on
shutdown*: when the context is cancelled, buffered in-flight events are flushed —
up to a bound — before the loop returns `ctx.Err()`. This module builds that loop
and tests it deterministically with an injected tick channel, no real sleeping.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
worker/                      module example.com/worker
  go.mod
  worker.go                  Loop: Run(ctx, events) (processed int, err error); bounded drain
  worker_test.go             clean close, cancel-then-drain (bounded), injected ticker fires, -race
  cmd/demo/
    main.go                  runs the loop over a closed channel and prints the count
```

- Files: `worker.go`, `worker_test.go`, `cmd/demo/main.go`.
- Implement: `(*Loop).Run(ctx, events <-chan Event) (int, error)` — the core `for { select { events / tick / ctx.Done } }`, a top-of-loop `ctx.Err()` check that triggers a bounded drain (at most `DrainMax` buffered events) before returning `ctx.Err()`, and a nil-safe injected `Tick` channel.
- Test: feed events on a closed channel and assert `processed == N` with `nil`; pre-cancel the context and assert a bounded drain (exactly `DrainMax` when more are buffered, all when fewer) returning `ctx.Err()`; drive an injected tick channel and assert the tick handler fires the expected number of times; a `-race` run with concurrent producers.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/07-graceful-shutdown-drain/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/07-graceful-shutdown-drain
```

### The two exits, and why the drain must be bounded

The loop's body is a `select` over three cases: an event arrived (process it), a
tick fired (run the periodic handler), or the context is done. Its two correct
exits are a *closed input channel* (`ev, ok := <-events` with `ok == false` means
the producer is finished — return `nil`) and a *cancelled context*. Everything
else is a bug: a `for {}` with no `ctx.Done()` case is a loop you cannot stop, and
one that returns on the first event is not a loop at all.

Shutdown is the subtle part. When the context is cancelled, there may still be
events buffered in the channel that the producer already sent. Dropping them
silently is a data-loss bug; draining them *without a bound* is a different bug —
against a fast producer the "drain" never ends and the service never shuts down. So
the correct shape is a *bounded* drain: on cancellation, pull up to `DrainMax`
already-buffered events (non-blocking, via a `select` with a `default`), process
them, then return `ctx.Err()`. The bound guarantees shutdown completes; the drain
guarantees in-flight work is not silently lost.

One design decision makes the whole thing deterministically testable: the loop
checks `ctx.Err()` at the *top* of each iteration, before the `select`. That means
a context cancelled before `Run` is even called goes straight to the bounded drain,
so a test can assert exactly how many buffered events are flushed without racing
the scheduler. The `ctx.Done()` case inside the `select` is still needed — it wakes
the loop when it is blocked waiting on an idle `events` channel — but the top-of-loop
check is what makes the drain count exact. The injected `Tick` channel is nil-safe:
a `nil` channel blocks forever in a `select`, so a `Loop` with no ticker simply
never takes the tick case.

Create `worker.go`:

```go
package worker

import (
	"context"
	"time"
)

// Event is a unit of work delivered to the loop.
type Event struct {
	ID int
}

// Loop is a long-lived consumer. Process handles each event; OnTick, if set,
// runs on each value from Tick; DrainMax bounds how many buffered events are
// flushed on shutdown.
type Loop struct {
	Process  func(Event)
	OnTick   func()
	Tick     <-chan time.Time
	DrainMax int
}

// Run consumes events until the channel is closed (returns processed, nil) or
// ctx is cancelled (drains up to DrainMax buffered events, then returns
// processed and ctx.Err()).
func (l *Loop) Run(ctx context.Context, events <-chan Event) (int, error) {
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			// Bounded drain of already-buffered events, then stop.
			for range l.DrainMax {
				select {
				case ev, ok := <-events:
					if !ok {
						return processed, err
					}
					l.Process(ev)
					processed++
				default:
					return processed, err
				}
			}
			return processed, err
		}

		select {
		case ev, ok := <-events:
			if !ok {
				return processed, nil
			}
			l.Process(ev)
			processed++
		case <-l.Tick:
			if l.OnTick != nil {
				l.OnTick()
			}
		case <-ctx.Done():
			// Loop back; the top-of-loop check runs the bounded drain.
		}
	}
}
```

### The runnable demo

The demo feeds four events on a channel it then closes, so the loop drains them and
exits cleanly with `nil`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/worker"
)

func main() {
	events := make(chan worker.Event, 4)
	for i := range 4 {
		events <- worker.Event{ID: i}
	}
	close(events)

	var handled []int
	loop := &worker.Loop{
		Process:  func(e worker.Event) { handled = append(handled, e.ID) },
		DrainMax: 16,
	}

	n, err := loop.Run(context.Background(), events)
	fmt.Printf("processed %d events %v, err=%v\n", n, handled, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
processed 4 events [0 1 2 3], err=<nil>
```

### Tests

Every test is deterministic because the input is either a closed channel or a
pre-cancelled context, and the ticker is an injected channel the test fills itself.
`TestCleanClose` proves the closed-channel exit. `TestBoundedDrainOnCancel` fills
more events than `DrainMax`, pre-cancels the context, and asserts exactly `DrainMax`
are flushed — the proof the drain is bounded. `TestDrainsAllWhenUnderCap` checks the
symmetric case. `TestTickerFires` drives an injected tick channel and cancels from
inside the tick handler once the expected number of ticks have fired.

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"testing"
	"time"
)

func TestCleanClose(t *testing.T) {
	t.Parallel()

	events := make(chan Event, 5)
	for i := range 5 {
		events <- Event{ID: i}
	}
	close(events)

	count := 0
	loop := &Loop{Process: func(Event) { count++ }, DrainMax: 8}

	n, err := loop.Run(context.Background(), events)
	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	if n != 5 || count != 5 {
		t.Fatalf("processed n=%d count=%d, want 5,5", n, count)
	}
}

func TestBoundedDrainOnCancel(t *testing.T) {
	t.Parallel()

	events := make(chan Event, 10)
	for i := range 10 {
		events <- Event{ID: i}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run: goes straight to the bounded drain

	count := 0
	loop := &Loop{Process: func(Event) { count++ }, DrainMax: 3}

	n, err := loop.Run(ctx, events)
	if err == nil {
		t.Fatal("Run() err = nil, want ctx.Err()")
	}
	if n != 3 || count != 3 {
		t.Fatalf("drained n=%d count=%d, want exactly 3 (DrainMax)", n, count)
	}
}

func TestDrainsAllWhenUnderCap(t *testing.T) {
	t.Parallel()

	events := make(chan Event, 2)
	events <- Event{ID: 1}
	events <- Event{ID: 2}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count := 0
	loop := &Loop{Process: func(Event) { count++ }, DrainMax: 8}

	n, _ := loop.Run(ctx, events)
	if n != 2 || count != 2 {
		t.Fatalf("drained n=%d count=%d, want 2 (all buffered, under cap)", n, count)
	}
}

func TestTickerFires(t *testing.T) {
	t.Parallel()

	ticks := make(chan time.Time, 3)
	for range 3 {
		ticks <- time.Unix(0, 0)
	}

	ctx, cancel := context.WithCancel(context.Background())

	fired := 0
	loop := &Loop{
		Tick: ticks,
		OnTick: func() {
			fired++
			if fired == 3 {
				cancel() // stop after the third tick
			}
		},
		DrainMax: 1,
	}

	// A nil events channel never delivers, so only ticks and cancellation drive
	// the loop.
	n, err := loop.Run(ctx, nil)
	if err == nil {
		t.Fatal("Run() err = nil, want ctx.Err()")
	}
	if fired != 3 {
		t.Fatalf("OnTick fired %d times, want 3", fired)
	}
	if n != 0 {
		t.Fatalf("processed %d events, want 0", n)
	}
}

func TestConcurrentProducers(t *testing.T) {
	t.Parallel()

	events := make(chan Event)
	loop := &Loop{Process: func(Event) {}, DrainMax: 4}

	go func() {
		for i := range 100 {
			events <- Event{ID: i}
		}
		close(events)
	}()

	n, err := loop.Run(context.Background(), events)
	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	if n != 100 {
		t.Fatalf("processed %d, want 100", n)
	}
}
```

## Review

The loop is correct when its two exits are exactly a closed `events` channel
(return `nil`) and a cancelled context (bounded drain, then `ctx.Err()`), and the
`ctx.Err()` check sits at the top of the iteration so a pre-cancelled context goes
straight to the drain. The drain must be *both* present and bounded: present, so
buffered in-flight events are not silently dropped on shutdown;
bounded by `DrainMax`, so a fast producer cannot keep the "shutting-down" loop
alive forever. `TestBoundedDrainOnCancel` proves exactly `DrainMax` are flushed
when more are buffered, and `TestDrainsAllWhenUnderCap` the symmetric case. The nil
`Tick` channel is safe because receiving from a nil channel blocks forever, so a
loop without a ticker never selects that case. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — how `select` chooses a ready case, and `default`.
- [context package](https://pkg.go.dev/context) — `Context.Done` and `Context.Err`, the loop's cancellation exit.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — cancellation-aware channel loops.
- [time.NewTicker](https://pkg.go.dev/time#NewTicker) — the real ticker a production `Tick` channel comes from.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-labeled-break-slot-search.md](06-labeled-break-slot-search.md) | Next: [08-readiness-poll.md](08-readiness-poll.md)
