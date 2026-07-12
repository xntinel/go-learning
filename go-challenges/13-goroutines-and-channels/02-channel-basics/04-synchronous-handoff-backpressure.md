# Exercise 4: Guarantee Persist-Before-Ack with an Unbuffered Handoff

When a service accepts a message and must acknowledge it upstream, the ack is a
promise: "I have this, you can stop retrying." Acking before the message is durably
recorded is how systems lose data on a crash. This exercise builds the enqueue path
that makes the promise honest — `Enqueue` returns only after a persister goroutine
has accepted *and* recorded the item — using an unbuffered channel for the handoff
and its happens-before guarantee.

This module is self-contained: its own module, a `durable` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
durable/                     independent module: example.com/durable
  go.mod                     go 1.26
  durable.go                 type Item, Queue; New, Enqueue, Close, Persisted
  cmd/demo/main.go           runnable demo: enqueue a few items, read them back
  durable_test.go            persist-before-ack, backpressure-blocks-producer
```

- Files: `durable.go`, `cmd/demo/main.go`, `durable_test.go`.
- Implement: `New() *Queue`, `(*Queue).Enqueue(Item)` that returns only after the item is recorded, `(*Queue).Close()`, `(*Queue).Persisted() []Item`.
- Test: the store contains the item the instant `Enqueue` returns; `Enqueue` does not return while the persister is artificially blocked.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/02-channel-basics/04-synchronous-handoff-backpressure/cmd/demo
cd go-solutions/13-goroutines-and-channels/02-channel-basics/04-synchronous-handoff-backpressure
```

### Why unbuffered, and why an explicit ack

An unbuffered send `q.in <- e` blocks until the persister receives it — that is the
first synchronization edge, and it is what gives *backpressure*: if the persister
is slow, `Enqueue` blocks instead of piling items into an unbounded buffer that
eventually exhausts memory. A buffered channel would let `Enqueue` return
immediately, having only *queued* the item, not persisted it; the caller would ack
upstream while the item sits in a buffer that a crash would lose. That is precisely
the bug this exercise avoids.

But "the persister received the item" is not the same as "the persister recorded
it". After the unbuffered receive unblocks the sender, the persister still has to
run its append. So the handoff carries a second channel — a per-item `ack` — that
the persister closes *after* it has recorded the item. `Enqueue` blocks on `<-ack`,
so it returns only once the record is in the store. Two rendezvous, in order: send
(persister has it), then ack (persister has stored it). The Go memory model chains
the happens-before edges — the append happens-before the `close(ack)`, which
happens-before `Enqueue`'s receive returns — so the value is safely visible to any
later `Persisted` read.

The persister goroutine is the sole writer of the store slice. `Persisted` takes a
short mutex only so a test can read a consistent snapshot concurrently with a
future write; the persist-before-ack ordering is what makes the read see the item,
not the mutex.

Create `durable.go`:

```go
package durable

import "sync"

// Item is a unit of work to be durably recorded before it is acknowledged.
type Item struct {
	ID string
}

type entry struct {
	item Item
	ack  chan struct{}
}

// Queue hands items to a single persister goroutine over an unbuffered channel.
// Enqueue returns only after the item has been recorded, so the caller may safely
// acknowledge upstream.
type Queue struct {
	in   chan entry
	gate <-chan struct{} // test hook: if non-nil, persister waits on it per item
	wg   sync.WaitGroup

	mu    sync.Mutex
	store []Item
}

// New returns a running Queue with a live persister.
func New() *Queue {
	return newWithGate(nil)
}

// newWithGate builds a Queue whose persister blocks on gate before recording each
// item. A nil gate means no blocking. Used by tests to prove backpressure.
func newWithGate(gate <-chan struct{}) *Queue {
	q := &Queue{
		in:   make(chan entry),
		gate: gate,
	}
	q.wg.Add(1)
	go q.persistLoop()
	return q
}

func (q *Queue) persistLoop() {
	defer q.wg.Done()
	for e := range q.in {
		if q.gate != nil {
			<-q.gate // blocked until the test releases it
		}
		q.mu.Lock()
		q.store = append(q.store, e.item)
		q.mu.Unlock()
		close(e.ack) // signal: recorded
	}
}

// Enqueue hands item to the persister and returns only after it is recorded.
func (q *Queue) Enqueue(item Item) {
	ack := make(chan struct{})
	q.in <- entry{item: item, ack: ack} // blocks until persister receives
	<-ack                               // blocks until persister has recorded
}

// Close stops the persister after all in-flight items are recorded.
func (q *Queue) Close() {
	close(q.in)
	q.wg.Wait()
}

// Persisted returns a snapshot copy of the recorded items.
func (q *Queue) Persisted() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Item(nil), q.store...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/durable"
)

func main() {
	q := durable.New()
	for _, id := range []string{"a", "b", "c"} {
		q.Enqueue(durable.Item{ID: id})
		// Safe to ack upstream here: the item is recorded.
		fmt.Println("acked:", id)
	}
	q.Close()
	fmt.Println("persisted count:", len(q.Persisted()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acked: a
acked: b
acked: c
persisted count: 3
```

### Tests

`TestEnqueueReturnsOnlyAfterPersisted` asserts the core promise: immediately after
`Enqueue` returns, `Persisted` already contains the item — no polling, no sleep.
`TestBackpressureBlocksProducer` proves the flip side using the gated persister: it
launches `Enqueue` in a goroutine while the persister is blocked on the gate, checks
that `Enqueue` has *not* returned, then releases the gate and confirms it returns.
That demonstrates a slow persister backpressures the producer rather than letting it
race ahead. The negative check uses a short timeout window; it runs cleanly under
`-race`.

Create `durable_test.go`:

```go
package durable

import (
	"testing"
	"time"
)

func TestEnqueueReturnsOnlyAfterPersisted(t *testing.T) {
	t.Parallel()
	q := New()
	defer q.Close()

	q.Enqueue(Item{ID: "x"})

	got := q.Persisted()
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("Persisted() = %+v right after Enqueue; want one item {x}", got)
	}
}

func TestEnqueueRecordsInOrder(t *testing.T) {
	t.Parallel()
	q := New()
	defer q.Close()

	ids := []string{"a", "b", "c", "d"}
	for _, id := range ids {
		q.Enqueue(Item{ID: id})
	}

	got := q.Persisted()
	if len(got) != len(ids) {
		t.Fatalf("persisted %d items, want %d", len(got), len(ids))
	}
	for i, id := range ids {
		if got[i].ID != id {
			t.Fatalf("item %d = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestBackpressureBlocksProducer(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	q := newWithGate(gate)

	done := make(chan struct{})
	go func() {
		q.Enqueue(Item{ID: "blocked"})
		close(done)
	}()

	// The persister is blocked on the gate, so Enqueue must not have returned.
	select {
	case <-done:
		t.Fatal("Enqueue returned while the persister was blocked")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	close(gate) // release the persister

	select {
	case <-done:
		// expected: Enqueue returns now that the item is recorded
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue did not return after the persister was released")
	}

	if got := q.Persisted(); len(got) != 1 || got[0].ID != "blocked" {
		t.Fatalf("Persisted() = %+v, want one item {blocked}", got)
	}
	q.Close()
}
```

## Review

The path is correct when the ack is a truthful promise: the store holds the item
the moment `Enqueue` returns, which `TestEnqueueReturnsOnlyAfterPersisted` checks
with no sleep at all — if it flaked you would know the ack fired before the record.
The unbuffered channel plus the per-item `ack` give two ordered rendezvous, and the
Go memory model turns "the persister ran its append" into "the caller can see the
append". The backpressure test proves the design's operational virtue: a stalled
persister stalls the producer instead of growing memory without bound. The mistake
to avoid is swapping in a buffered channel "for throughput" — it would let `Enqueue`
return on a mere enqueue, breaking persist-before-ack and reintroducing the data
loss the whole exercise exists to prevent.

## Resources

- [The Go Memory Model: channel communication](https://go.dev/ref/mem#chan) — the happens-before edges that make persist-before-ack sound.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — unbuffered synchronization and backpressure.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — draining the persister cleanly on `Close`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-cursor-stream-close-comma-ok.md](03-cursor-stream-close-comma-ok.md) | Next: [05-pipeline-stage-transform.md](05-pipeline-stage-transform.md)
