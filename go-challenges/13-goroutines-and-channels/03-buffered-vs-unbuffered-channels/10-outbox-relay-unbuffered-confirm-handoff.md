# Exercise 10: Outbox Relay: Unbuffered Confirm-Handoff So the Cursor Never Runs Ahead of Delivery

**Level: Intermediate**

A transactional-outbox relay reads unsent rows in ID order and hands each to a dispatcher that publishes it to a broker. The durable cursor is the recovery point: on restart the relay re-polls everything after it. The trap is advancing the cursor when the row has merely been *queued* for dispatch rather than *delivered* — a crash then loses every row the cursor already claimed as sent. This exercise builds the relay on an UNBUFFERED request channel plus an unbuffered ack channel, so the poller and dispatcher rendezvous once per row and the send-then-ack round trip is the happens-before barrier that keeps `delivered == cursor` an invariant.

This module is self-contained: its own module, an `outbox` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26, require go.uber.org/goleak (test only)
  outbox.go                  Relay over an unbuffered req/ack pair; Run(ctx) (delivered, cursor, error)
  cmd/demo/main.go           runnable demo: a full drain and a dispatch failure mid-stream
  outbox_test.go             barrier invariant, full drain, mid-stream cancel, dispatch-error stop, goleak
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `type Row struct { ID int; Payload string }`, `NewRelay(rows []Row, dispatch func(context.Context, Row) error) *Relay`, and `(*Relay).Run(ctx context.Context) (delivered int, cursor int, err error)` over an unbuffered `chan Row` and an unbuffered `chan error`.
- Test: `delivered == cursor` always (barrier); a full run reaches the last row's ID with `delivered == len(rows)`; a mid-stream cancel leaves `cursor == delivered` with no gap; a dispatch error at row k stops with the cursor at row k-1 and `err` wrapping both `ErrDispatch` and the dispatch error; goleak confirms both goroutines exit.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/outbox/cmd/demo
cd ~/go-exercises/outbox
go mod init example.com/outbox
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### Why the round trip has to be unbuffered

The cursor is a promise: "everything with ID at or below this value is durably delivered." Break that promise and a restart silently drops rows. So the only safe rule is that the cursor advances to `row.ID` strictly *after* the dispatcher confirms the row went out — never before, never concurrently. An unbuffered channel is exactly the tool for "after," because the Go memory model makes an unbuffered handoff a two-way barrier: the receive completes only once the send has met it, and the send completes only once the receive has taken the value. There is no slack, no queue, nowhere for a row to sit unaccounted.

The protocol per row is a two-step rendezvous:

1. The poller sends the row on the unbuffered `req` channel. This completes only when the dispatcher has *taken* the row — so a taken row is never sitting in a buffer while the cursor moves on.
2. The dispatcher runs `dispatch(ctx, row)` and sends the result (nil or an error) on the unbuffered `ack` channel. The poller receives that ack. Because the receive happens-after the ack send, and the ack send happens-after `dispatch` returned, the poller *knows* the dispatch has finished before it touches the cursor.
3. Only now, on a nil ack, does the poller do `delivered++; cursor = row.ID`. On a non-nil ack it returns with the cursor left at the previous row's ID.

Contrast the wrong design. Give `req` a buffer and drop the ack, and the poller's send returns the instant the value lands in the buffer — the row is still undispatched, yet nothing stops the poller from advancing the cursor over rows that only *might* get delivered later. That is the data-loss bug in one line of `make`. The ack channel is what turns "I handed it off" into "it was delivered"; making both channels unbuffered is what forbids any gap between those two facts.

One honest consequence of choosing safety: on cancel or a dispatch failure a row may have been dispatched without its ack being consumed, so a restart can re-deliver it. That is at-least-once, the correct bias for an outbox — a duplicate is a downstream idempotency concern; a lost row is unrecoverable. The invariant we hold is `cursor == delivered` with no gap, never `dispatched == delivered` exactly.

Create `outbox.go`:

```go
package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Row is one unsent record drained from the outbox, in ascending ID order.
type Row struct {
	ID      int
	Payload string
}

// ErrDispatch marks any failure that stopped the relay at a dispatch call.
// The underlying dispatch error is wrapped alongside it, so both
// errors.Is(err, ErrDispatch) and errors.Is(err, <dispatch error>) hold.
var ErrDispatch = errors.New("dispatch failed")

// Relay drains rows from an in-memory outbox and hands each to a dispatcher
// goroutine over an UNBUFFERED request channel, confirmed by an unbuffered ack
// channel. The send-then-ack round trip is a happens-before barrier: the cursor
// advances to a row's ID only after that row's dispatch has actually returned.
type Relay struct {
	rows     []Row
	dispatch func(context.Context, Row) error
	req      chan Row   // unbuffered: poller and dispatcher rendezvous per row
	ack      chan error // unbuffered: the confirm; receiving it happens-after dispatch
}

// NewRelay builds a relay over rows (assumed sorted by ID) using dispatch as the
// delivery function. Both channels are unbuffered on purpose: a buffer would let
// the poller advance the cursor while rows still sat undispatched in the buffer.
func NewRelay(rows []Row, dispatch func(context.Context, Row) error) *Relay {
	return &Relay{
		rows:     rows,
		dispatch: dispatch,
		req:      make(chan Row),
		ack:      make(chan error),
	}
}

// Run pairs a poller (this goroutine) and a dispatcher (a spawned goroutine)
// connected by the unbuffered req and ack channels. The poller advances cursor
// to row.ID only after the dispatcher acks a successful dispatch. Run returns on
// full drain (err nil), ctx cancel (err ctx.Err()), or the first dispatch error
// (err wraps ErrDispatch and the dispatch error). On any early exit the cursor is
// left at the last successfully delivered ID, and delivered == cursor's rank.
func (r *Relay) Run(ctx context.Context) (delivered int, cursor int, err error) {
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case row, ok := <-r.req:
				if !ok {
					return // poller closed req on its way out; drain is complete
				}
				derr := r.dispatch(ctx, row)
				select {
				case <-ctx.Done():
					return
				case r.ack <- derr:
				}
			}
		}
	})

	// LIFO: close(req) runs first (releasing the dispatcher's receive), then
	// wg.Wait blocks until the dispatcher goroutine has actually exited.
	defer wg.Wait()
	defer close(r.req)

	for _, row := range r.rows {
		// Barrier step 1: hand the row across. Unbuffered, so this completes
		// only once the dispatcher has taken the row -- no undispatched backlog.
		select {
		case <-ctx.Done():
			return delivered, cursor, ctx.Err()
		case r.req <- row:
		}
		// Barrier step 2: wait for the confirm. Receiving the ack happens-after
		// the dispatch call returned, so the cursor advance below cannot run
		// ahead of a real delivery.
		select {
		case <-ctx.Done():
			return delivered, cursor, ctx.Err()
		case derr := <-r.ack:
			if derr != nil {
				// Leave cursor at the last delivered ID; the failed row stays unsent.
				return delivered, cursor, fmt.Errorf("outbox: %w at row %d: %w", ErrDispatch, row.ID, derr)
			}
			delivered++
			cursor = row.ID
		}
	}
	return delivered, cursor, nil
}
```

### The runnable demo

The demo runs two relays over the same five rows: a clean full drain, then a run whose dispatcher fails at row 3. It prints the delivered count, the cursor, the list of IDs actually dispatched, and the error, and confirms the wrapped-error identities.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/outbox"
)

func main() {
	rows := []outbox.Row{
		{ID: 1, Payload: "order-created:1001"},
		{ID: 2, Payload: "order-paid:1001"},
		{ID: 3, Payload: "order-shipped:1001"},
		{ID: 4, Payload: "order-created:1002"},
		{ID: 5, Payload: "order-paid:1002"},
	}

	// Full drain: every row delivered, cursor reaches the last ID, and the
	// dispatched list equals the row order because each handoff is a rendezvous.
	var sent []int
	deliver := func(_ context.Context, row outbox.Row) error {
		sent = append(sent, row.ID)
		return nil
	}
	delivered, cursor, err := outbox.NewRelay(rows, deliver).Run(context.Background())
	fmt.Printf("drain:   delivered=%d cursor=%d dispatched=%v err=%v\n", delivered, cursor, sent, err)

	// Dispatch error at row 3: the cursor stops at row 2 and delivered==2, so a
	// crash-and-restart would re-poll from row 3 -- nothing the cursor claims is
	// lost, because the cursor never ran ahead of an ack.
	brokerDown := errors.New("broker unreachable")
	var sent2 []int
	deliver2 := func(_ context.Context, row outbox.Row) error {
		if row.ID == 3 {
			return brokerDown
		}
		sent2 = append(sent2, row.ID)
		return nil
	}
	delivered, cursor, err = outbox.NewRelay(rows, deliver2).Run(context.Background())
	fmt.Printf("failure: delivered=%d cursor=%d dispatched=%v err=%v\n", delivered, cursor, sent2, err)
	fmt.Printf("failure: isErrDispatch=%v isBrokerErr=%v\n", errors.Is(err, outbox.ErrDispatch), errors.Is(err, brokerDown))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the drain reaches cursor 5; the failure holds the cursor at 2 and the error wraps both identities):

```
drain:   delivered=5 cursor=5 dispatched=[1 2 3 4 5] err=<nil>
failure: delivered=2 cursor=2 dispatched=[1 2] err=outbox: dispatch failed at row 3: broker unreachable
failure: isErrDispatch=true isBrokerErr=true
```

### Tests

`TestBarrierDeliveredEqualsCursor` is the core property. With rows numbered 1..n the cursor's value equals the count of rows delivered, so an instrumented `atomic.Int64` dispatch counter, the `delivered` return, and `cursor` must all coincide — proving the cursor never ran ahead of a real dispatch. `TestFullRunDrainsAllAndReachesLastID` asserts a clean run delivers every row and lands the cursor on the last ID. `TestCancelMidStreamLeavesNoGap` cancels the context from *inside* the dispatch of row 3 (deterministic, no sleep); it asserts the error is `context.Canceled`, that `delivered == cursor` with no gap, that the run stopped before the end, and that the cursor never exceeds the number of rows actually dispatched. `TestDispatchErrorStopsAtPriorID` fails dispatch at row 4 and asserts the cursor holds at 3, `delivered == 3`, and the error wraps both `ErrDispatch` and the injected error via `errors.Is`. `TestMain` wraps every test in `goleak.VerifyTestMain`, so a leaked poller or dispatcher goroutine fails the suite.

Create `outbox_test.go`:

```go
package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// seqRows builds n rows with IDs 1..n so that the cursor's numeric value equals
// the count of rows delivered so far -- which is what makes delivered==cursor a
// checkable barrier invariant.
func seqRows(n int) []Row {
	rows := make([]Row, n)
	for i := range n {
		rows[i] = Row{ID: i + 1, Payload: "p"}
	}
	return rows
}

func TestBarrierDeliveredEqualsCursor(t *testing.T) {
	t.Parallel()

	rows := seqRows(8)
	var dispatched atomic.Int64 // successful dispatch calls that actually returned
	deliver := func(_ context.Context, _ Row) error {
		dispatched.Add(1)
		return nil
	}

	delivered, cursor, err := NewRelay(rows, deliver).Run(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	// The barrier: the cursor never runs ahead of a real delivery. With the
	// unbuffered send-then-ack round trip, the count of successful dispatches,
	// the delivered count, and the cursor rank are all equal.
	if int64(delivered) != dispatched.Load() {
		t.Fatalf("delivered=%d but dispatched=%d; cursor ran ahead of delivery", delivered, dispatched.Load())
	}
	if delivered != cursor {
		t.Fatalf("delivered=%d cursor=%d; want equal (barrier violated)", delivered, cursor)
	}
}

func TestFullRunDrainsAllAndReachesLastID(t *testing.T) {
	t.Parallel()

	rows := seqRows(6)
	deliver := func(_ context.Context, _ Row) error { return nil }

	delivered, cursor, err := NewRelay(rows, deliver).Run(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if delivered != len(rows) {
		t.Fatalf("delivered=%d, want %d", delivered, len(rows))
	}
	if cursor != rows[len(rows)-1].ID {
		t.Fatalf("cursor=%d, want last row ID %d", cursor, rows[len(rows)-1].ID)
	}
}

func TestCancelMidStreamLeavesNoGap(t *testing.T) {
	t.Parallel()

	rows := seqRows(6)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const cancelAt = 3 // < len(rows), so the run cannot finish normally
	var dispatched atomic.Int64
	deliver := func(_ context.Context, row Row) error {
		dispatched.Add(1)
		if row.ID == cancelAt {
			cancel() // cancel from inside dispatch: deterministic, no sleep
		}
		return nil
	}

	delivered, cursor, err := NewRelay(rows, deliver).Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// cursor==delivered means no gap: the cursor points at exactly the rank of
	// rows confirmed delivered, never at a row still in flight.
	if delivered != cursor {
		t.Fatalf("delivered=%d cursor=%d; cancel opened a gap", delivered, cursor)
	}
	if delivered >= len(rows) {
		t.Fatalf("delivered=%d; want a mid-stream stop (< %d)", delivered, len(rows))
	}
	// The cursor is monotonic and never ahead of what was actually dispatched.
	if int64(cursor) > dispatched.Load() {
		t.Fatalf("cursor=%d exceeds dispatched=%d", cursor, dispatched.Load())
	}
}

func TestDispatchErrorStopsAtPriorID(t *testing.T) {
	t.Parallel()

	rows := seqRows(6)
	brokerDown := errors.New("broker unreachable")
	const failAt = 4 // IDs are 1..6; fail at ID 4, expect cursor to hold at 3
	deliver := func(_ context.Context, row Row) error {
		if row.ID == failAt {
			return brokerDown
		}
		return nil
	}

	delivered, cursor, err := NewRelay(rows, deliver).Run(context.Background())

	if !errors.Is(err, ErrDispatch) {
		t.Fatalf("err = %v, want to wrap ErrDispatch", err)
	}
	if !errors.Is(err, brokerDown) {
		t.Fatalf("err = %v, want to wrap the dispatch error", err)
	}
	if cursor != failAt-1 {
		t.Fatalf("cursor=%d, want %d (row before the failed one)", cursor, failAt-1)
	}
	if delivered != failAt-1 {
		t.Fatalf("delivered=%d, want %d", delivered, failAt-1)
	}
}
```

## Review

"Correct" here is a single durability invariant: the cursor names only rows that were actually delivered, so `delivered == cursor` (with rows numbered 1..n) at every observable point and after every exit path. The unbuffered `req`/`ack` pair is what guarantees it — the send-then-ack round trip is a happens-before barrier, so the poller advances the cursor strictly after `dispatch` returned, never while a row is still queued. The barrier test proves it directly by pinning the instrumented dispatch count to `delivered` and `cursor`; the cancel and error tests prove the invariant survives both early-exit paths, holding the cursor at the last confirmed ID with no gap; goleak proves neither goroutine leaks. The production bug this prevents is the classic outbox data loss: buffer the handoff (or advance the cursor on enqueue) and a crash discards every row the cursor claimed as sent. The unbuffered barrier makes that gap unrepresentable, and biases the relay to at-least-once — the safe direction, since a duplicate is an idempotency concern while a lost row is gone for good.

## Resources

- [The Go Memory Model: Channel communication](https://go.dev/ref/mem#chan) — the happens-before rules that make the unbuffered send/receive a two-way barrier.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — unbuffered channels as synchronization points versus buffered channels as queues.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain` for asserting both goroutines exit on every path.
- [The Go Programming Language Specification: Channel types](https://go.dev/ref/spec#Channel_types) — send and receive on an unbuffered channel is the synchronizing rendezvous the relay depends on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-errgroup-bounded-pipeline.md](09-errgroup-bounded-pipeline.md) | Next: [11-state-push-conflation-cap-one-mailbox.md](11-state-push-conflation-cap-one-mailbox.md)
