# Exercise 11: Outbox Relay: Advance The Cursor Only Past The Contiguous Delivered Prefix

**Level: Intermediate**

A transactional-outbox relay polls a batch of unpublished rows and must ship them
to a broker before advancing its durable cursor. Dispatching the batch
concurrently is the easy part; the trap is the cursor. At-least-once delivery
forbids moving it past a gap: if record k fails while k+1 succeeds out of order,
advancing to k+1 silently drops k forever. This exercise builds `PumpBatch`, which
fans out one launched goroutine per record under a semaphore, joins every one of
them, and only then advances the cursor to the last contiguously-delivered ID.

This module is self-contained: its own module, an `outbox` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26
  outbox.go                  Record; Relay; NewRelay; PumpBatch -> (newCursor, delivered, err)
  cmd/demo/main.go           runnable demo: a mid-batch failure holds the cursor at the gap
  outbox_test.go             prefix advance, gap stop, empty batch, disjoint slots, peak cap, no fail-fast
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `type Record struct { ID int64; Payload string }`, `type Relay`, `NewRelay(publish func(context.Context, Record) error, concurrency int) *Relay`, and `(*Relay).PumpBatch(ctx, afterID int64, batch []Record) (newCursor int64, delivered int, err error)`.
- Test: an all-success batch advances to the last ID; a failure at position k holds the cursor at `batch[k-1].ID` even when later records succeed; empty batch is a no-op; per-record outcomes written by index are race-free; peak in flight never exceeds the cap; every record is attempted even after a failure.
- Verify: `go test -count=1 -race ./...`

### Join before you trust the side effect, then advance only across the gap-free prefix

The relay's job is a two-phase contract. Phase one dispatches every record in the
batch concurrently: one goroutine per record, launched with `WaitGroup.Go`, and a
buffered `sem` channel used as a counting semaphore so that at most `concurrency`
dispatchers run at once. A token is acquired (`sem <- struct{}{}`) in the launching
goroutine *before* the `go`, and released with a `defer` inside the dispatcher.
Acquiring before launch is what actually bounds peak concurrency: the loop blocks
on a full semaphore instead of spawning a goroutine that would immediately block
anyway, so the number of *executing* dispatchers is capped, not just the number
that have started their real work.

Phase two is where the correctness lives, and it is gated on a single `wg.Wait()`.
The cursor computation must not read a single per-record outcome until every
launched goroutine has been joined -- this is the "join before you depend on the
side effect" rule from the concepts file made concrete. Each dispatcher writes only
its own slot `errs[i]`; because the indices are disjoint, those writes never
overlap and need no mutex, but they are only safe to *read* after the join
establishes the happens-before edge.

The advance rule itself is deliberately conservative. `newCursor` starts at
`afterID` and walks the batch in ID order. It moves forward only while the success
prefix is unbroken:

1. Walk `batch` from index 0 (the batch is sorted ascending by ID).
2. On the first record whose `errs[i] != nil`, close the prefix: the cursor freezes
   at wherever it was.
3. A later record that succeeded out of order still increments `delivered` (it was
   genuinely published, at-least-once), but it does **not** move the cursor, because
   a record before it is still undelivered.

So `delivered` is the total actually shipped, while `newCursor` is the largest ID N
such that *every* record with ID <= N was delivered. There is no first-error
cancellation: a failure never tears down its peers, because at-least-once delivery
wants every record attempted this round, and the failed one simply gets re-polled
next round when the cursor did not pass it.

Create `outbox.go`:

```go
package outbox

import (
	"context"
	"errors"
	"sync"
)

// Record is one unpublished row read from the transactional outbox table.
type Record struct {
	ID      int64
	Payload string
}

// Relay ships a batch of outbox records to a broker and reports how far the
// durable cursor may safely advance.
type Relay struct {
	publish     func(ctx context.Context, r Record) error
	concurrency int
}

// NewRelay builds a Relay that dispatches with publish, at most concurrency
// records in flight at once. concurrency is clamped up to 1.
func NewRelay(publish func(ctx context.Context, r Record) error, concurrency int) *Relay {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Relay{publish: publish, concurrency: concurrency}
}

// PumpBatch dispatches every record in batch concurrently, at most
// r.concurrency in flight, joins all of them, and only then computes how far the
// cursor may move. batch is assumed sorted ascending by ID.
//
// newCursor is the largest ID N such that every record with ID <= N was
// delivered: the contiguous success prefix. The first failure stops the cursor,
// so any record at or after it is left for the next poll even if it happened to
// succeed out of order. An empty batch returns (afterID, 0, nil).
//
// delivered is the count actually published (including out-of-order successes
// past a gap). err is errors.Join of every per-record failure; there is no
// first-error cancellation, so every record is attempted even after one fails.
func (r *Relay) PumpBatch(ctx context.Context, afterID int64, batch []Record) (newCursor int64, delivered int, err error) {
	if len(batch) == 0 {
		return afterID, 0, nil
	}

	// Disjoint per-index slots: each dispatcher writes only errs[i], so the
	// writes never overlap and need no mutex. The join is what makes the reads
	// below safe.
	errs := make([]error, len(batch))
	sem := make(chan struct{}, r.concurrency) // semaphore: caps records in flight

	var wg sync.WaitGroup
	for i, rec := range batch {
		sem <- struct{}{} // acquire before launch: bounds peak concurrency
		wg.Go(func() {
			defer func() { <-sem }()
			errs[i] = r.publish(ctx, rec)
		})
	}
	wg.Wait() // join every dispatcher before touching the cursor or the slots

	newCursor = afterID
	prefixOpen := true
	for i := range batch {
		if errs[i] != nil {
			prefixOpen = false // first gap: the cursor stops here
		}
		if errs[i] == nil {
			delivered++
			if prefixOpen {
				newCursor = batch[i].ID
			}
		}
	}
	return newCursor, delivered, errors.Join(errs...)
}
```

### The runnable demo

The demo pumps a five-record batch through a broker that rejects the middle record
(ID 103). Records 104 and 105 still publish successfully -- there is no fail-fast --
but the cursor must freeze at 102, so 103, 104, and 105 are all re-polled next
round. The output is deterministic because every line is printed after `PumpBatch`
has joined all dispatchers and returned; nothing is printed from inside a goroutine.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/outbox"
)

func main() {
	batch := []outbox.Record{
		{ID: 101, Payload: "order.created"},
		{ID: 102, Payload: "order.paid"},
		{ID: 103, Payload: "order.shipped"},
		{ID: 104, Payload: "order.delivered"},
		{ID: 105, Payload: "order.closed"},
	}

	// A broker that rejects one record in the middle. 104 and 105 will still be
	// published (no fail-fast), but the cursor must not jump past the 103 gap.
	broker := func(_ context.Context, r outbox.Record) error {
		if r.ID == 103 {
			return fmt.Errorf("broker rejected %d", r.ID)
		}
		return nil
	}

	relay := outbox.NewRelay(broker, 3)
	afterID := int64(100)

	cursor, delivered, err := relay.PumpBatch(context.Background(), afterID, batch)

	fmt.Printf("afterID=%d\n", afterID)
	fmt.Printf("delivered=%d\n", delivered)
	fmt.Printf("newCursor=%d\n", cursor)
	fmt.Printf("err=%v\n", err)

	next := make([]int64, 0, len(batch))
	for _, r := range batch {
		if r.ID > cursor {
			next = append(next, r.ID)
		}
	}
	fmt.Printf("re-polled next round=%v\n", next)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
afterID=100
delivered=4
newCursor=102
err=broker rejected 103
re-polled next round=[103 104 105]
```

Note `delivered=4` (104 and 105 shipped) while `newCursor=102`: four records were
published, but the cursor still stops at the gap.

### Tests

`TestAllSuccessAdvancesToLast` pins that an all-success batch advances the cursor to
the last ID and counts every record. `TestFailureStopsAtContiguousPrefix` is a table
over each failure position k: it asserts `newCursor == batch[k-1].ID` (or `afterID`
when k==0) even though the later records succeed out of order, and that the joined
error wraps the injected sentinel via `errors.Is`. `TestEmptyBatch` pins the no-op
contract. `TestDisjointOutcomeSlotsMatchLog` has each dispatcher write its own index
in two slices with no mutex and asserts the recorded payloads and outcomes match --
under `-race` this proves the writes are disjoint. `TestPeakInFlightBounded` gates
every dispatcher behind a `release` channel, saturates the cap, and asserts an
atomic peak gauge never exceeds `concurrency` and actually reaches it.
`TestNoFailFastEveryRecordAttempted` counts attempts and asserts every record ran
despite an early failure.

Create `outbox_test.go`:

```go
package outbox

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
)

// seq builds a batch with IDs 1..n so tests can index disjoint slots by ID-1.
func seq(n int) []Record {
	b := make([]Record, n)
	for i := range n {
		b[i] = Record{ID: int64(i + 1), Payload: fmt.Sprintf("evt-%d", i+1)}
	}
	return b
}

// TestAllSuccessAdvancesToLast pins property (1): when every publish succeeds the
// cursor advances to the last ID and delivered counts the whole batch.
func TestAllSuccessAdvancesToLast(t *testing.T) {
	t.Parallel()

	batch := seq(8)
	r := NewRelay(func(context.Context, Record) error { return nil }, 4)

	cursor, delivered, err := r.PumpBatch(context.Background(), 0, batch)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cursor != 8 {
		t.Fatalf("newCursor = %d, want 8", cursor)
	}
	if delivered != len(batch) {
		t.Fatalf("delivered = %d, want %d", delivered, len(batch))
	}
}

// TestFailureStopsAtContiguousPrefix pins property (2) and (6): a failure at
// position k stops the cursor at batch[k-1].ID (or afterID when k==0) even though
// every later record succeeds out of order, and the error wraps the failure.
func TestFailureStopsAtContiguousPrefix(t *testing.T) {
	t.Parallel()

	const n = 6
	const afterID = 100

	for k := 0; k < n; k++ {
		t.Run(fmt.Sprintf("fail_at_%d", k), func(t *testing.T) {
			t.Parallel()

			batch := make([]Record, n)
			for i := range n {
				batch[i] = Record{ID: int64(afterID + i + 1), Payload: "x"}
			}
			failID := batch[k].ID
			sentinel := errors.New("broker rejected")

			// Only the record at position k fails; all others succeed. Because
			// dispatch is concurrent, the later records genuinely finish out of
			// order relative to k, yet the cursor must still stop at the gap.
			r := NewRelay(func(_ context.Context, rec Record) error {
				if rec.ID == failID {
					return sentinel
				}
				return nil
			}, 4)

			cursor, delivered, err := r.PumpBatch(context.Background(), afterID, batch)

			wantCursor := int64(afterID)
			if k > 0 {
				wantCursor = batch[k-1].ID
			}
			if cursor != wantCursor {
				t.Fatalf("newCursor = %d, want %d", cursor, wantCursor)
			}
			if delivered != n-1 {
				t.Fatalf("delivered = %d, want %d", delivered, n-1)
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want it to wrap sentinel", err)
			}
		})
	}
}

// TestEmptyBatch pins property (3): an empty batch leaves the cursor unchanged.
func TestEmptyBatch(t *testing.T) {
	t.Parallel()

	r := NewRelay(func(context.Context, Record) error {
		t.Fatal("publish must not be called for an empty batch")
		return nil
	}, 4)

	cursor, delivered, err := r.PumpBatch(context.Background(), 42, nil)
	if cursor != 42 || delivered != 0 || err != nil {
		t.Fatalf("got (%d, %d, %v), want (42, 0, nil)", cursor, delivered, err)
	}
}

// TestDisjointOutcomeSlotsMatchLog pins property (4): each dispatcher writes its
// own index in two slices with no mutex; under -race this proves the writes are
// disjoint, and the recorded outcomes match what was published.
func TestDisjointOutcomeSlotsMatchLog(t *testing.T) {
	t.Parallel()

	const n = 32
	batch := seq(n)

	seenPayload := make([]string, n) // slot i written only by record i+1
	ok := make([]bool, n)

	r := NewRelay(func(_ context.Context, rec Record) error {
		idx := rec.ID - 1
		seenPayload[idx] = rec.Payload // disjoint slot: no shared write
		if rec.ID%5 == 0 {
			ok[idx] = false
			return fmt.Errorf("reject %d", rec.ID)
		}
		ok[idx] = true
		return nil
	}, 8)

	_, delivered, _ := r.PumpBatch(context.Background(), 0, batch)

	wantDelivered := 0
	for i := range n {
		if batch[i].Payload != seenPayload[i] {
			t.Fatalf("slot %d = %q, want %q", i, seenPayload[i], batch[i].Payload)
		}
		if ok[i] {
			wantDelivered++
		}
	}
	if delivered != wantDelivered {
		t.Fatalf("delivered = %d, want %d", delivered, wantDelivered)
	}
}

// TestPeakInFlightBounded pins property (5): the semaphore holds the number of
// concurrently executing dispatchers at or below the configured cap. A gate makes
// every admitted dispatcher pile up so the peak is actually reached.
func TestPeakInFlightBounded(t *testing.T) {
	const n = 50
	const cap = 5

	var inFlight, peak atomic.Int64
	release := make(chan struct{})

	r := NewRelay(func(context.Context, Record) error {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-release // hold until the test lets everyone drain
		inFlight.Add(-1)
		return nil
	}, cap)

	type result struct {
		cursor    int64
		delivered int
	}
	done := make(chan result, 1)
	go func() {
		c, d, _ := r.PumpBatch(context.Background(), 0, seq(n))
		done <- result{c, d}
	}()

	// Wait until the cap is saturated, then release. Bounded poll, no sleep.
	saturated := false
	for range 10_000_000 {
		if inFlight.Load() == cap {
			saturated = true
			break
		}
		runtime.Gosched()
	}
	close(release)
	res := <-done

	if !saturated {
		t.Fatalf("never reached cap=%d in flight", cap)
	}
	if peak.Load() > cap {
		t.Fatalf("peak in flight = %d, exceeds cap %d", peak.Load(), cap)
	}
	if peak.Load() != cap {
		t.Fatalf("peak in flight = %d, want exactly %d", peak.Load(), cap)
	}
	if res.cursor != n || res.delivered != n {
		t.Fatalf("got cursor=%d delivered=%d, want %d/%d", res.cursor, res.delivered, n, n)
	}
}

// TestNoFailFastEveryRecordAttempted pins property (6) directly: one record fails
// but every record is still attempted; there is no first-error cancellation.
func TestNoFailFastEveryRecordAttempted(t *testing.T) {
	t.Parallel()

	const n = 20
	var attempts atomic.Int64

	r := NewRelay(func(_ context.Context, rec Record) error {
		attempts.Add(1)
		if rec.ID == 3 {
			return errors.New("reject 3")
		}
		return nil
	}, 4)

	cursor, delivered, err := r.PumpBatch(context.Background(), 0, seq(n))

	if attempts.Load() != n {
		t.Fatalf("attempts = %d, want %d (every record must be attempted)", attempts.Load(), n)
	}
	if cursor != 2 {
		t.Fatalf("newCursor = %d, want 2 (stops before the ID 3 gap)", cursor)
	}
	if delivered != n-1 {
		t.Fatalf("delivered = %d, want %d", delivered, n-1)
	}
	if err == nil {
		t.Fatal("err = nil, want the failure joined")
	}
}
```

## Review

Correct here means one thing: the cursor never advances past an undelivered record,
no matter how the concurrent dispatch interleaves. The mechanism is the two-phase
shape -- fan out under a semaphore, `wg.Wait()` to join every launched goroutine,
then compute the cursor from the joined per-record outcomes. The join is what makes
the disjoint `errs[i]` writes safe to read and what makes "the cursor moved" mean
"every record it moved across actually shipped." The advance walk stops at the first
gap, so an out-of-order success past a failure raises `delivered` but not
`newCursor`, and the failed record is re-polled next round instead of being silently
skipped. `TestFailureStopsAtContiguousPrefix` proves it for every failure position,
`TestPeakInFlightBounded` proves the semaphore actually caps concurrency, and
`TestNoFailFastEveryRecordAttempted` proves at-least-once (no cancellation). The
production bug this prevents is the classic outbox data-loss incident: a relay that
advances its cursor to the batch maximum after a partial failure, permanently
dropping every record that failed in that round.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) -- how the per-record failures are aggregated without losing any of them to `errors.Is`.
- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 launch-and-join idiom used for each dispatcher.
- [Go concurrency patterns: pipelines](https://go.dev/blog/pipelines) -- fan-out under a bounded semaphore and joining every stage before depending on its result.
- [Go Memory Model](https://go.dev/ref/mem) -- the happens-before edge `wg.Wait()` establishes before the cursor reads the outcome slots.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-context-scoped-background-task.md](10-context-scoped-background-task.md) | Next: [12-singleflight-cache-stampede-single-launch.md](12-singleflight-cache-stampede-single-launch.md)
