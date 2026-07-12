# Exercise 12: Outbox Relay Batch Dispatch With Outcome Partition

**Level: Intermediate**

A transactional-outbox relay wakes on a tick, reads a batch of pending rows, and
dispatches each to a downstream sink. The naive version dispatches sequentially and
crawls; the tempting fix — fan out with goroutines and `append` acked IDs to a shared
slice — races the slice header and, worse, silently drops rows when the partition
runs before the goroutines finish. This module builds the relay's dispatch step
correctly: one goroutine per message writing its own disjoint outcome slot, a
WaitGroup join that publishes those slots, then a partition into acked and
still-pending IDs. Because the contract is at-least-once, a failed row must stay
pending for the next tick rather than vanish.

This module is self-contained: its own module, an `outbox` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26
  outbox.go                  RelayBatch: concurrent dispatch, WaitGroup join, outcome partition
  cmd/demo/main.go           runnable demo: a 3-message mixed run, sorted partition printed
  outbox_test.go             all-success, mixed partition, exactly-once, empty, context-cancel
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `RelayBatch(ctx context.Context, batch []Message, dispatch Dispatch) Result`, over `type Message struct { ID int; Payload string }`, `type Dispatch func(ctx context.Context, m Message) error`, and `type Result struct { Dispatched []int; Pending []int }`.
- Test: all-success fills `Dispatched` and leaves `Pending` empty; a mixed run partitions exactly with both slices sorted ascending; each dispatch runs exactly once (atomic per-ID counter); an empty batch returns two empty slices; a cancelled context leaves respecting messages `Pending`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/12-outbox-relay-batch-dispatch-partition/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/12-outbox-relay-batch-dispatch-partition
go mod edit -go=1.26
```

### The join is what makes the outcome slice safe to read

The relay's job is to turn a batch of pending rows into two sets: IDs the sink acked
(delete or mark them done) and IDs that failed (leave them pending, retry next tick).
Getting that partition wrong in either direction is a production incident. Drop a
failed ID and you lose a message — the at-least-once contract is broken. Ack a
message you never actually delivered and you also lose it. So the partition must read
a *complete and correct* per-message outcome, and it must read it only after every
dispatch has finished.

The design uses the disjoint-index idiom. Allocate `outcomes := make([]error, len(batch))`
up front, one slot per message. Fan out one goroutine per message; goroutine `i`
writes only `outcomes[i]`. Distinct indices are distinct memory locations, so there is
no data race and no lock on the dispatch path — the race detector stays quiet even
though every goroutine writes the same backing array. This is the idiom to reach for
whenever a fan-out needs a result per input.

The WaitGroup is the linchpin. `wg.Go` (Go 1.25+) does `Add(1)`, runs the function,
and `Done`s on return, with the `Add` placed correctly so it can never drift into the
new goroutine. After the loop, `wg.Wait()` blocks until every dispatch has returned.
The Go memory model guarantees that every write a goroutine did before its `Done` is
happens-before-ordered ahead of the `Wait` that the `Done` unblocks. In plain terms:
once `Wait` returns, every `outcomes[i]` is visible to the partitioning code, with no
extra synchronization. Read the slice before the join and you would see zero-value
`nil` errors for goroutines that had not run yet — every unfinished row would be
mis-counted as dispatched. The join is not decoration; it is the correctness boundary.

Only after the join does the partition run: walk the batch, send `ID` to `Dispatched`
if its outcome is `nil` and to `Pending` otherwise, then sort both ascending so the
result is deterministic no matter what order the goroutines finished in. A
context-cancelled dispatch that returns `ctx.Err()` produces a non-nil outcome, so
those rows land in `Pending` and are retried — exactly what at-least-once demands.

Create `outbox.go`:

```go
// Package outbox implements a transactional-outbox relay's batch dispatch step:
// fan out every pending message to a downstream sink concurrently, join with a
// WaitGroup, then partition message IDs by delivery outcome.
package outbox

import (
	"context"
	"slices"
	"sync"
)

// Message is one row read from the outbox table.
type Message struct {
	ID      int
	Payload string
}

// Dispatch delivers a single message to the downstream sink. A nil return means
// the row was acked (delivered at least once); a non-nil return means the row
// must stay pending and be retried on the next tick.
type Dispatch func(ctx context.Context, m Message) error

// Result partitions a batch's IDs by outcome. Both slices are sorted ascending.
// Dispatched holds acked IDs; Pending holds IDs whose dispatch failed (including
// context-cancelled ones), which the relay must retry next tick.
type Result struct {
	Dispatched []int
	Pending    []int
}

// RelayBatch dispatches every message in batch concurrently, one goroutine per
// message, and joins with a WaitGroup. Each goroutine writes its own disjoint
// index in outcomes, so there is no data race and no lock on the hot path; the
// wg.Wait inside wg.Go's join happens-before this function reads outcomes, which
// is what makes the read safe. It then partitions IDs by outcome. The delivery
// contract is at-least-once: a failed row stays Pending rather than being dropped.
func RelayBatch(ctx context.Context, batch []Message, dispatch Dispatch) Result {
	// Pre-size to len(batch): empty batch yields two empty (non-nil) slices.
	outcomes := make([]error, len(batch))

	var wg sync.WaitGroup
	for i, m := range batch {
		wg.Go(func() {
			outcomes[i] = dispatch(ctx, m) // disjoint index write: race-free
		})
	}
	wg.Wait() // join: publishes every outcomes[i] to this goroutine

	res := Result{Dispatched: []int{}, Pending: []int{}}
	for i, m := range batch {
		if outcomes[i] == nil {
			res.Dispatched = append(res.Dispatched, m.ID)
		} else {
			res.Pending = append(res.Pending, m.ID)
		}
	}
	slices.Sort(res.Dispatched)
	slices.Sort(res.Pending)
	return res
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/outbox"
)

func main() {
	batch := []outbox.Message{
		{ID: 101, Payload: "order.created"},
		{ID: 102, Payload: "order.paid"},
		{ID: 103, Payload: "order.shipped"},
	}

	// The sink acks 101 and 103; 102 fails and must stay pending for the next tick.
	dispatch := func(ctx context.Context, m outbox.Message) error {
		if m.ID == 102 {
			return fmt.Errorf("sink rejected %d", m.ID)
		}
		return nil
	}

	res := outbox.RelayBatch(context.Background(), batch, dispatch)
	fmt.Println("dispatched:", res.Dispatched)
	fmt.Println("pending:   ", res.Pending)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dispatched: [101 103]
pending:    [102]
```

### Tests

`TestAllSuccess` dispatches an unsorted batch through an always-ok sink and asserts
every ID appears in `Dispatched` sorted ascending while `Pending` is empty.
`TestMixedPartition` fails a known subset and asserts the exact partition with both
slices sorted, pinning down that outcome order does not depend on goroutine finish
order. `TestExactlyOncePerMessage` runs 64 delayed dispatches under an atomic per-ID
counter and asserts each ID was dispatched exactly once — no duplicate, no dropped
send — and that the partition covers all IDs; the deliberate delay forces real overlap
so the run exercises the join under `-race`. `TestEmptyBatch` asserts an empty input
returns two empty, non-nil slices and never calls dispatch. `TestContextCancelLeavesPending`
cancels the context before dispatch and asserts every respecting message stays
`Pending`. `TestPendingCarriesCancelCause` uses `context.WithCancelCause` and confirms
via `errors.Is` that the cancellation reason reaches the dispatch while the row stays
pending. `ExampleRelayBatch` pins the 3-message mixed run.

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
	"time"
)

func makeBatch(ids ...int) []Message {
	batch := make([]Message, 0, len(ids))
	for _, id := range ids {
		batch = append(batch, Message{ID: id, Payload: fmt.Sprintf("evt-%d", id)})
	}
	return batch
}

// TestAllSuccess: every ID lands in Dispatched, Pending is empty.
func TestAllSuccess(t *testing.T) {
	t.Parallel()

	batch := makeBatch(3, 1, 2)
	ok := func(ctx context.Context, m Message) error { return nil }

	res := RelayBatch(context.Background(), batch, ok)

	if want := []int{1, 2, 3}; !slices.Equal(res.Dispatched, want) {
		t.Fatalf("Dispatched = %v, want %v (sorted ascending)", res.Dispatched, want)
	}
	if len(res.Pending) != 0 {
		t.Fatalf("Pending = %v, want empty", res.Pending)
	}
}

// TestMixedPartition: some dispatch funcs fail; the partition is exact and both
// slices are sorted ascending regardless of goroutine completion order.
func TestMixedPartition(t *testing.T) {
	t.Parallel()

	batch := makeBatch(50, 40, 30, 20, 10)
	fails := map[int]bool{40: true, 20: true}
	dispatch := func(ctx context.Context, m Message) error {
		if fails[m.ID] {
			return fmt.Errorf("sink rejected %d", m.ID)
		}
		return nil
	}

	res := RelayBatch(context.Background(), batch, dispatch)

	if want := []int{10, 30, 50}; !slices.Equal(res.Dispatched, want) {
		t.Fatalf("Dispatched = %v, want %v", res.Dispatched, want)
	}
	if want := []int{20, 40}; !slices.Equal(res.Pending, want) {
		t.Fatalf("Pending = %v, want %v", res.Pending, want)
	}
}

// TestExactlyOncePerMessage: each message's dispatch func is invoked exactly once
// (no duplicate, no dropped send), verified by an atomic per-ID counter. Delayed
// dispatch forces real concurrency so the join is what publishes the outcomes; the
// whole test runs clean under -race.
func TestExactlyOncePerMessage(t *testing.T) {
	t.Parallel()

	const n = 64
	ids := make([]int, 0, n)
	for i := range n {
		ids = append(ids, i)
	}
	batch := makeBatch(ids...)

	var calls [n]atomic.Int32
	dispatch := func(ctx context.Context, m Message) error {
		time.Sleep(time.Millisecond) // delay so goroutines overlap; not a correctness crutch
		calls[m.ID].Add(1)
		if m.ID%2 == 0 {
			return nil
		}
		return fmt.Errorf("odd id %d", m.ID)
	}

	res := RelayBatch(context.Background(), batch, dispatch)

	for id := range n {
		if got := calls[id].Load(); got != 1 {
			t.Fatalf("message %d dispatched %d times, want exactly 1", id, got)
		}
	}

	if len(res.Dispatched)+len(res.Pending) != n {
		t.Fatalf("partition covers %d IDs, want %d", len(res.Dispatched)+len(res.Pending), n)
	}
	if !slices.IsSorted(res.Dispatched) || !slices.IsSorted(res.Pending) {
		t.Fatalf("partition not sorted ascending: %v / %v", res.Dispatched, res.Pending)
	}
	for _, id := range res.Dispatched {
		if id%2 != 0 {
			t.Fatalf("odd id %d in Dispatched", id)
		}
	}
	for _, id := range res.Pending {
		if id%2 == 0 {
			t.Fatalf("even id %d in Pending", id)
		}
	}
}

// TestEmptyBatch: an empty batch returns two empty, non-nil slices.
func TestEmptyBatch(t *testing.T) {
	t.Parallel()

	res := RelayBatch(context.Background(), nil, func(ctx context.Context, m Message) error {
		t.Fatal("dispatch called on empty batch")
		return nil
	})

	if res.Dispatched == nil || len(res.Dispatched) != 0 {
		t.Fatalf("Dispatched = %v, want empty non-nil", res.Dispatched)
	}
	if res.Pending == nil || len(res.Pending) != 0 {
		t.Fatalf("Pending = %v, want empty non-nil", res.Pending)
	}
}

// TestContextCancelLeavesPending: with the context already cancelled, a dispatch
// that respects ctx fails, so those messages stay Pending (they must be retried).
func TestContextCancelLeavesPending(t *testing.T) {
	t.Parallel()

	batch := makeBatch(1, 2, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before dispatch runs

	dispatch := func(ctx context.Context, m Message) error {
		if err := ctx.Err(); err != nil {
			return err // respect cancellation: the row stays pending
		}
		return nil
	}

	res := RelayBatch(ctx, batch, dispatch)

	if len(res.Dispatched) != 0 {
		t.Fatalf("Dispatched = %v, want empty (all cancelled)", res.Dispatched)
	}
	if want := []int{1, 2, 3}; !slices.Equal(res.Pending, want) {
		t.Fatalf("Pending = %v, want %v", res.Pending, want)
	}
}

// TestPendingCarriesCancelCause: a dispatch that returns the context error keeps
// the message pending; errors.Is confirms the cancellation reason propagates.
func TestPendingCarriesCancelCause(t *testing.T) {
	t.Parallel()

	batch := makeBatch(7)
	sentinel := errors.New("relay draining")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(sentinel)

	var got error
	dispatch := func(ctx context.Context, m Message) error {
		got = context.Cause(ctx)
		return got
	}

	res := RelayBatch(ctx, batch, dispatch)

	if len(res.Pending) != 1 || res.Pending[0] != 7 {
		t.Fatalf("Pending = %v, want [7]", res.Pending)
	}
	if !errors.Is(got, sentinel) {
		t.Fatalf("cause = %v, want %v", got, sentinel)
	}
}

func ExampleRelayBatch() {
	batch := []Message{
		{ID: 101, Payload: "order.created"},
		{ID: 102, Payload: "order.paid"},
		{ID: 103, Payload: "order.shipped"},
	}
	dispatch := func(ctx context.Context, m Message) error {
		if m.ID == 102 {
			return fmt.Errorf("sink rejected %d", m.ID)
		}
		return nil
	}

	res := RelayBatch(context.Background(), batch, dispatch)
	fmt.Println("dispatched:", res.Dispatched)
	fmt.Println("pending:", res.Pending)
	// Output:
	// dispatched: [101 103]
	// pending: [102]
}
```

## Review

Correct here means the partition is total and truthful: every batch ID appears in
exactly one of `Dispatched` or `Pending`, an acked outcome (`nil` error) puts the ID in
`Dispatched`, any failure — including a context cancellation — puts it in `Pending`, and
both slices come back sorted ascending so the result is deterministic. The guaranteeing
invariant is the WaitGroup join: each goroutine writes its own disjoint `outcomes[i]`,
and `wg.Wait` establishes the happens-before edge that publishes every slot before the
partition reads it, so the read needs no lock and can never observe a half-finished
batch. The exactly-once test proves no message is dispatched twice or skipped, and the
mixed and cancel tests prove the partition tracks outcomes rather than finish order.
The production bug this prevents is the silent dropped message: read the outcomes
before the join, or `append` acked IDs from many goroutines into one shared slice, and
a failed row can be mis-acked and deleted — breaking the at-least-once delivery
guarantee that the entire outbox pattern exists to provide.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- the happens-before rule that makes `outcomes[i]` safe to read after `wg.Wait`.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- `Go`, `Add`, `Done`, and `Wait` as a join counter.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) -- attaching and reading a cancellation reason, used in the cancel tests.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) -- fanning out a batch of work and propagating cancellation so failed rows are retried, not lost.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-hierarchical-region-shard-read-nested-waitgroups.md](11-hierarchical-region-shard-read-nested-waitgroups.md) | Next: [13-transitive-closure-dynamic-add-traversal.md](13-transitive-closure-dynamic-add-traversal.md)
