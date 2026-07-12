# Exercise 12: An Outbox Relay That Acks Only The Rows It Published

**Level: Intermediate**

A transactional-outbox relay sweeps a batch of pending events and publishes each
to a broker. Fan out one goroutine per row and a thousand-row batch opens a
thousand broker connections at once; the naive fix of a first-error-abort batch
throws away every row after the first failure even though most of them would have
published cleanly. This exercise builds the correct middle: a bounded worker pool
that runs to completion and reports per-event outcomes, so the relay acks (deletes)
exactly the rows it delivered and leaves the failures pending for the next sweep.

This module is self-contained: its own module, an `outbox` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26
  outbox.go                  Dispatch: bounded, run-to-completion publish with per-event acks
  cmd/demo/main.go           runnable demo: publish a 5-row batch through 3 workers, print acks and failures
  outbox_test.go             partial-ack, concurrency-bound, cancellation, empty, all-success tests
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `Dispatch(ctx context.Context, workers int, batch []Event, pub Publisher) (acked []int, err error)`, plus `type Event struct{ ID int; Payload []byte }` and `type Publisher func(ctx context.Context, e Event) error`.
- Test: acked holds exactly the successful ids in input order; `err` is an `errors.Join` findable per failure via `errors.Is`; in-flight publishes never exceed `workers`; cancellation leaves pending rows unpublished and unacked and reports `context.Canceled`; empty batch returns `nil, nil`; a clean sweep returns all ids and a nil error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/13-goroutine-pools/12-outbox-relay-partial-ack/cmd/demo
cd go-solutions/13-goroutines-and-channels/13-goroutine-pools/12-outbox-relay-partial-ack
```

### Run-to-completion, not first-error-abort

The transactional outbox pattern decouples "commit the state change" from "publish
the event": the write transaction inserts a row into an `outbox` table in the same
commit, and a separate relay later reads pending rows and pushes them to the
broker. Because the relay is a separate step, delivery is at-least-once — a row can
be published, then the relay crashes before recording that fact, so the next sweep
publishes it again. The consumers dedupe. What the relay must never do is the
inverse: delete (ack) a row it did not actually publish, because that row is then
lost forever.

That single requirement drives the whole design and rules out the batch primitive
from the errgroup exercise. `errgroup` with `SetLimit` cancels the shared context
on the first error and returns that one error; it is built to abandon the rest of
the batch the moment one unit fails. Here the opposite is correct. If row 2 fails
because the broker briefly rejected it, rows 3 through 1000 should still publish —
they are independent events, and leaving 998 rows pending because one row failed
turns a transient hiccup into a growing backlog. So the pool is **run to
completion**: every event is attempted, and the result is a per-event verdict, not
a single pass/fail.

The mechanism is fan-out with index correlation. A shared jobs channel carries
input indices; a fixed set of `workers` goroutines drain it, and each writes its
outcome into `errs[i]`, its own preallocated slot. Distinct indices are touched by
distinct goroutines, so there is no shared element and the race detector stays
quiet — the trap the concepts file warns about is several goroutines appending to
one slice, which this design avoids entirely. After the workers drain, the
dispatcher walks the slots in input order: a nil slot means published, so its id
joins `acked`; a non-nil slot means still pending, so its error joins the
aggregate. `errors.Join` returns nil when there are zero failures, which is exactly
the "clean sweep returns a nil error" contract, and it wraps each failure so the
caller can `errors.Is` its way back to any specific broker error.

Cancellation has a precise contract here. A `SIGTERM` mid-sweep must not publish
rows it has not started, and must not ack them either — they stay pending and the
next relay picks them up. The dispatcher owns the jobs channel, so it is the one
that stops feeding: its send is a `select` on `ctx.Done()` versus `jobs <- i`, and
when cancellation wins it marks the untouched tail with `context.Cause(ctx)` and
closes the channel. A worker that already holds an index re-checks `ctx.Err()`
before calling the publisher, so a row that reached a worker after cancellation is
skipped rather than delivered. Either way a cancelled row gets a non-nil slot, so
it is absent from `acked` and its `context.Canceled` surfaces through the joined
error.

Create `outbox.go`:

```go
// Package outbox implements a transactional-outbox relay that publishes a batch
// of pending events through a bounded worker pool and acks only the events that
// published successfully. Failures stay pending for the next sweep.
package outbox

import (
	"context"
	"errors"
	"sync"
)

// Event is one pending row in the outbox table.
type Event struct {
	ID      int
	Payload []byte
}

// Publisher delivers a single event to the broker. A non-nil error means the
// event was not delivered and must remain pending.
type Publisher func(ctx context.Context, e Event) error

// Dispatch publishes every event in batch through a pool of `workers`, returning
// the ids that published successfully (in original input order) and an
// errors.Join wrapping every failure. It is run-to-completion: a single failure
// does NOT abort the remaining events, because at-least-once delivery means the
// relay must decide per event whether to ack (delete) it or leave it pending.
//
// If ctx is cancelled, events that have not yet started are neither published
// nor acked; their slot records the cancellation cause, so the joined error
// reports context.Canceled and those ids are absent from the returned acks.
func Dispatch(ctx context.Context, workers int, batch []Event, pub Publisher) (acked []int, err error) {
	if len(batch) == 0 {
		return nil, nil
	}
	if workers < 1 {
		workers = 1
	}

	// One result slot per input index. Distinct indices are written by distinct
	// goroutines, so there is no shared element and no data race.
	errs := make([]error, len(batch))
	jobs := make(chan int)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				// Re-check cancellation at the last moment: a job that reaches a
				// worker after cancellation is skipped, not published.
				if cerr := ctx.Err(); cerr != nil {
					errs[i] = cerr
					continue
				}
				errs[i] = pub(ctx, batch[i])
			}
		})
	}

	// The dispatcher owns the jobs channel and is the only closer. On
	// cancellation it stops feeding and marks the not-yet-dispatched tail, so
	// those events are never sent to a worker at all.
feed:
	for i := range batch {
		select {
		case <-ctx.Done():
			for j := i; j < len(batch); j++ {
				errs[j] = context.Cause(ctx)
			}
			break feed
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	var failures []error
	for i, e := range batch {
		if errs[i] == nil {
			acked = append(acked, e.ID)
		} else {
			failures = append(failures, errs[i])
		}
	}
	// errors.Join returns nil when failures is empty, so a fully successful
	// sweep returns a nil error.
	return acked, errors.Join(failures...)
}
```

### The runnable demo

The demo sweeps a fixed 5-row batch through 3 workers with a fake broker that
rejects even-numbered ids. The output is deterministic because acks and failures
are both reported in input order, independent of which worker happened to run
which row.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/outbox"
)

// errBroker is the failure a flaky broker returns for the rows it rejects.
var errBroker = errors.New("broker unavailable")

func main() {
	batch := []outbox.Event{
		{ID: 1, Payload: []byte("order.created")},
		{ID: 2, Payload: []byte("order.paid")},
		{ID: 3, Payload: []byte("order.shipped")},
		{ID: 4, Payload: []byte("order.refunded")},
		{ID: 5, Payload: []byte("order.closed")},
	}

	// Deterministic fake broker: even ids are rejected and stay pending.
	pub := func(_ context.Context, e outbox.Event) error {
		if e.ID%2 == 0 {
			return fmt.Errorf("publish id=%d: %w", e.ID, errBroker)
		}
		return nil
	}

	acked, err := outbox.Dispatch(context.Background(), 3, batch, pub)

	fmt.Printf("workers=3 batch=%d\n", len(batch))
	fmt.Printf("acked=%v\n", acked)
	fmt.Printf("pending=%d\n", len(batch)-len(acked))
	fmt.Println("errors:")
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=3 batch=5
acked=[1 3 5]
pending=2
errors:
publish id=2: broker unavailable
publish id=4: broker unavailable
```

### Tests

`TestPartialAckOnlySuccesses` publishes a 6-row batch where ids 2, 3, and 5 are
rejected with a per-id sentinel; it asserts `acked` is exactly `[1 4 6]` in input
order and that the joined error is `errors.Is`-findable against every sentinel.
`TestConcurrencyNeverExceedsWorkers` uses a publisher that records peak in-flight
count via atomics and a barrier that only releases once `workers` publishes are
simultaneously live, proving the peak equals `workers` and never exceeds it — no
sleep involved. `TestCancellationSkipsPendingRows` runs a single worker so
processing is strictly ordered, cancels inside the first row's publish, and
asserts only id 1 is published and acked while the joined error reports
`context.Canceled`. `TestEmptyBatch` asserts a nil batch returns `nil, nil` and
never calls the publisher. `TestAllSucceed` asserts a clean sweep returns every id
in order and a nil error.

Create `outbox_test.go`:

```go
package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
)

func mkBatch(n int) []Event {
	b := make([]Event, n)
	for i := range b {
		b[i] = Event{ID: i + 1, Payload: []byte(fmt.Sprintf("evt-%d", i+1))}
	}
	return b
}

// TestPartialAckOnlySuccesses pins down the core contract: with a Publisher that
// rejects a known subset of ids, acked holds exactly the successful ids in input
// order, and err is an errors.Join in which each failure is findable via
// errors.Is against a per-id sentinel.
func TestPartialAckOnlySuccesses(t *testing.T) {
	t.Parallel()

	batch := mkBatch(6)
	// Fail ids 2, 3, 5; the rest succeed.
	fail := map[int]bool{2: true, 3: true, 5: true}
	sentinels := map[int]error{}
	for id := range fail {
		sentinels[id] = fmt.Errorf("reject id=%d", id)
	}

	pub := func(_ context.Context, e Event) error {
		if fail[e.ID] {
			return fmt.Errorf("publish id=%d: %w", e.ID, sentinels[e.ID])
		}
		return nil
	}

	acked, err := Dispatch(context.Background(), 3, batch, pub)

	want := []int{1, 4, 6}
	if !slices.Equal(acked, want) {
		t.Fatalf("acked = %v, want %v", acked, want)
	}
	if err == nil {
		t.Fatal("err = nil, want joined failures")
	}
	for id, s := range sentinels {
		if !errors.Is(err, s) {
			t.Fatalf("err does not wrap sentinel for id %d: %v", id, err)
		}
	}
}

// TestConcurrencyNeverExceedsWorkers proves the pool bounds fan-out: the number
// of publishes in flight at once never passes `workers`, and with a batch at
// least `workers` wide it actually reaches `workers`.
func TestConcurrencyNeverExceedsWorkers(t *testing.T) {
	t.Parallel()

	const workers = 4
	batch := mkBatch(workers * 2)

	var inflight, peak, arrived atomic.Int64
	// ready is closed once `workers` publishes are simultaneously in flight, so
	// the first wave is provably concurrent without any sleep.
	ready := make(chan struct{})

	pub := func(_ context.Context, _ Event) error {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if arrived.Add(1) == workers {
			close(ready)
		}
		<-ready
		inflight.Add(-1)
		return nil
	}

	acked, err := Dispatch(context.Background(), workers, batch, pub)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(acked) != len(batch) {
		t.Fatalf("acked count = %d, want %d", len(acked), len(batch))
	}
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrency = %d, want %d", got, workers)
	}
}

// TestCancellationSkipsPendingRows pins the cancel contract: an event in flight
// when ctx is cancelled aborts, not-yet-started events are never published nor
// acked, and the joined error reports context.Canceled.
func TestCancellationSkipsPendingRows(t *testing.T) {
	t.Parallel()

	batch := mkBatch(5)
	ctx, cancel := context.WithCancel(context.Background())

	var published []int
	// workers=1 makes processing strictly ordered: id 1 runs first and cancels
	// the context, so every later id is skipped before it can be published.
	pub := func(_ context.Context, e Event) error {
		published = append(published, e.ID)
		if e.ID == batch[0].ID {
			cancel()
		}
		return nil
	}

	acked, err := Dispatch(ctx, 1, batch, pub)

	if !slices.Equal(acked, []int{1}) {
		t.Fatalf("acked = %v, want [1]", acked)
	}
	if !slices.Equal(published, []int{1}) {
		t.Fatalf("published = %v, want [1]; later rows must not be published", published)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestEmptyBatch: no events means no work and no error.
func TestEmptyBatch(t *testing.T) {
	t.Parallel()

	acked, err := Dispatch(context.Background(), 4, nil, func(context.Context, Event) error {
		t.Fatal("publisher must not be called for an empty batch")
		return nil
	})
	if acked != nil {
		t.Fatalf("acked = %v, want nil", acked)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestAllSucceed: a clean sweep acks every id in order and returns a nil error.
func TestAllSucceed(t *testing.T) {
	t.Parallel()

	batch := mkBatch(20)
	acked, err := Dispatch(context.Background(), 5, batch, func(context.Context, Event) error {
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := make([]int, len(batch))
	for i := range want {
		want[i] = i + 1
	}
	if !slices.Equal(acked, want) {
		t.Fatalf("acked = %v, want %v", acked, want)
	}
}
```

## Review

"Correct" here means the set of acked ids equals the set of rows the broker
actually accepted — never a superset, or the outbox silently drops events. The
invariant that guarantees it is the per-index result slot: a row's id joins
`acked` only when its slot is nil, and a slot is nil only after `pub` returned nil
for that exact row, so an ack cannot outrun a delivery. `TestPartialAckOnlySuccesses`
proves the partition is exact and order-preserving and that each failure survives
in the joined error; `TestConcurrencyNeverExceedsWorkers` proves the pool actually
bounds fan-out to `workers` connections rather than one per row; and
`TestCancellationSkipsPendingRows` proves a mid-sweep shutdown leaves untouched
rows pending instead of publishing or acking them. The production bug this pattern
prevents is the data-loss inverse of at-least-once delivery: a first-error-abort
batch, or a pool that acks the whole batch after a partial publish, deletes rows
that were never delivered, and those events are gone with no retry left to recover
them.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating independent failures into one value that `errors.Is` can still traverse.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — reading the reason a context was cancelled, richer than `ctx.Err()`.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25+ helper that pairs `Add` and `go` correctly so workers cannot be under-counted.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in with a shared jobs channel and clean shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-worker-local-resource-pool.md](11-worker-local-resource-pool.md) | Next: [13-per-key-ordered-worker-pool.md](13-per-key-ordered-worker-pool.md)
