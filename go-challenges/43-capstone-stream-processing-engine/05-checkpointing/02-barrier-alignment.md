# Exercise 2: Barrier Alignment

An operator with one input lane forwards a checkpoint barrier the instant it arrives. An operator that fans in several parallel lanes cannot: it must wait until the same barrier has arrived on every lane, buffering the records that race ahead on the fast lanes so the snapshot it triggers captures one consistent cut across all inputs. This module builds that aligner.

This module is fully self-contained. It defines the stream event types it needs — `Barrier`, `Record`, `StreamEvent`, `CheckpointID` — and ships its own demo and tests, importing nothing from any other exercise.

## What you'll build

```text
aligner.go             CheckpointID, Barrier, Record, StreamEvent,
                       BarrierAligner, Push, Out
cmd/
  demo/
    main.go            two lanes, a record buffered after a barrier, aligned output
aligner_test.go        pass-through, buffering, reset across checkpoints, mismatch, race
```

- Files: `aligner.go`, `cmd/demo/main.go`, `aligner_test.go`.
- Implement: `BarrierAligner` with `Push(lane int, ev StreamEvent) error` and `Out() <-chan StreamEvent`, safe for concurrent callers.
- Test: `aligner_test.go` proves records pass through before a barrier, that records after a lane's barrier are buffered and flushed in lane order, that alignment state resets so the next checkpoint works, that a mismatched or duplicate barrier is rejected, and that concurrent lane pushes are race-free.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why alignment is necessary, and what it buffers

Consider a fan-in operator with two input lanes. The coordinator injects barrier 1 at the sources; it reaches the operator on lane 0 first because that source is faster or its path is shorter. The moment lane 0's barrier arrives, two things become true at once. First, any record that now arrives on lane 0 logically belongs to the *next* checkpoint — it was produced after the barrier — so it must not be counted in this snapshot. Second, lane 1 is still delivering records that belong to *this* checkpoint, because its barrier has not arrived yet. The operator cannot snapshot until lane 1's barrier shows up, and in the meantime it must keep accepting lane 1's records while holding back lane 0's.

That is exactly what the aligner does. When a lane delivers its barrier, the aligner marks that lane `delivered` and starts buffering its subsequent records instead of emitting them. Records from lanes that have not yet delivered their barrier pass straight through. When the arrived count reaches the number of lanes, every lane has delivered the same barrier: the aligner flushes the buffered records in lane order, emits the barrier itself, and resets all of its per-lane state for the next checkpoint. If a second barrier with a different ID arrives while alignment is in progress, or the same lane delivers its barrier twice, that is a protocol violation and `Push` returns an error.

### The lock-then-send discipline

`Push` is safe to call from several goroutines, one per lane, so its internal state lives under a mutex. The trap is the output channel. If `Push` sent to `a.out` while still holding `a.mu`, and the goroutine draining `a.out` ever called `Push` before reading, the send would block with the lock held while the drain's `Push` blocked waiting for the lock — a classic self-inflicted deadlock. The aligner avoids it by computing the list of events to emit *under* the lock, releasing the lock, and only then ranging over that list to send. The lock protects the state machine; the channel send happens outside it. This is why `collectBarrier` and `collectRecord` return a slice of events rather than sending directly.

Create `aligner.go`:
```go
// Package aligner implements Chandy-Lamport barrier alignment for a stream
// operator that fans in records from several parallel input lanes.
package aligner

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCheckpointMismatch is returned when a lane delivers a barrier for a
// different checkpoint than the one currently being aligned.
var ErrCheckpointMismatch = errors.New("checkpoint ID mismatch")

// CheckpointID is a monotonically increasing checkpoint identifier.
type CheckpointID uint64

// Barrier is a control marker injected at the sources. Operators snapshot on
// receipt and forward it downstream.
type Barrier struct {
	ID        CheckpointID
	Timestamp time.Time
}

// Record is a data element flowing through the operator graph.
type Record struct {
	Data []byte
}

// StreamEvent carries either a Record or a Barrier on one input lane.
// Exactly one field is non-nil per event.
type StreamEvent struct {
	Record  *Record
	Barrier *Barrier
}

// BarrierAligner aligns barriers across numIn input lanes. Once a lane delivers
// its barrier, later records on that lane are buffered; the barrier is not
// forwarded until every lane has delivered the same barrier. This guarantees
// the downstream snapshot captures one consistent cut across all inputs.
//
// Concurrency: Push is safe to call from multiple goroutines. Channel sends
// happen after the internal lock is released, so a send that blocks on a full
// output channel never holds the lock and never deadlocks a concurrent Push.
type BarrierAligner struct {
	mu        sync.Mutex
	numIn     int
	buffered  [][]Record
	delivered []bool       // per-lane: has this lane delivered the current barrier?
	currentID CheckpointID // 0 means no alignment in progress
	arrived   int          // count of lanes that have delivered the current barrier
	out       chan StreamEvent
}

// NewBarrierAligner creates an aligner for numIn input lanes (numIn >= 1).
// outBuf is the capacity of the output channel.
func NewBarrierAligner(numIn int, outBuf int) *BarrierAligner {
	if numIn < 1 {
		numIn = 1
	}
	return &BarrierAligner{
		numIn:     numIn,
		buffered:  make([][]Record, numIn),
		delivered: make([]bool, numIn),
		out:       make(chan StreamEvent, outBuf),
	}
}

// Push delivers a StreamEvent from input lane. Exactly one field of ev must be
// non-nil. It returns an error for an out-of-range lane or a duplicate barrier
// on the same lane.
func (a *BarrierAligner) Push(lane int, ev StreamEvent) error {
	if lane < 0 || lane >= a.numIn {
		return fmt.Errorf("aligner: lane %d out of range [0,%d)", lane, a.numIn)
	}

	a.mu.Lock()
	var toEmit []StreamEvent
	var err error
	switch {
	case ev.Barrier != nil:
		toEmit, err = a.collectBarrier(lane, ev.Barrier)
	case ev.Record != nil:
		toEmit, err = a.collectRecord(lane, ev.Record)
	}
	a.mu.Unlock()

	if err != nil {
		return err
	}
	for i := range toEmit {
		a.out <- toEmit[i]
	}
	return nil
}

// collectBarrier is called with a.mu held.
func (a *BarrierAligner) collectBarrier(lane int, b *Barrier) ([]StreamEvent, error) {
	if a.currentID != 0 && a.currentID != b.ID {
		return nil, fmt.Errorf("aligner: %w: aligning on %d, received %d",
			ErrCheckpointMismatch, a.currentID, b.ID)
	}
	if a.delivered[lane] {
		return nil, fmt.Errorf("aligner: lane %d already delivered barrier %d", lane, b.ID)
	}
	if a.currentID == 0 {
		a.currentID = b.ID
	}
	a.delivered[lane] = true
	a.arrived++

	if a.arrived < a.numIn {
		return nil, nil
	}
	// All lanes delivered the barrier: flush buffers in lane order, then barrier.
	var events []StreamEvent
	for i := range a.buffered {
		for j := range a.buffered[i] {
			r := a.buffered[i][j]
			events = append(events, StreamEvent{Record: &r})
		}
		a.buffered[i] = a.buffered[i][:0]
	}
	events = append(events, StreamEvent{Barrier: b})
	// Reset alignment state for the next checkpoint.
	for i := range a.delivered {
		a.delivered[i] = false
	}
	a.currentID = 0
	a.arrived = 0
	return events, nil
}

// collectRecord is called with a.mu held.
func (a *BarrierAligner) collectRecord(lane int, r *Record) ([]StreamEvent, error) {
	rc := *r
	if a.delivered[lane] {
		// This lane already delivered its barrier; buffer the record.
		a.buffered[lane] = append(a.buffered[lane], rc)
		return nil, nil
	}
	return []StreamEvent{{Record: &rc}}, nil
}

// Out returns the merged, aligned output channel. Drain it continuously; size
// outBuf for the maximum number of in-flight events.
func (a *BarrierAligner) Out() <-chan StreamEvent {
	return a.out
}
```
### The runnable demo

The demo drives two lanes through one checkpoint. Lane 0 delivers its barrier early and then a record that belongs to the next checkpoint, so that record is buffered. Lane 1 still has a current-checkpoint record before its own barrier. When lane 1's barrier completes the alignment, the output is the two pass-through records, then the buffered record flushed in lane order, then the barrier.

Create `cmd/demo/main.go`:
```go
package main

import (
	"fmt"

	"example.com/barrier-alignment"
)

func main() {
	// Two parallel input lanes feeding one fan-in operator.
	a := aligner.NewBarrierAligner(2, 16)

	// Lane 0 races ahead: it delivers its barrier and then a record that
	// belongs to the NEXT checkpoint, so that record must be buffered.
	_ = a.Push(0, aligner.StreamEvent{Record: &aligner.Record{Data: []byte("a0")}})
	_ = a.Push(1, aligner.StreamEvent{Record: &aligner.Record{Data: []byte("b0")}})

	b1 := &aligner.Barrier{ID: 1}
	_ = a.Push(0, aligner.StreamEvent{Barrier: b1})
	_ = a.Push(0, aligner.StreamEvent{Record: &aligner.Record{Data: []byte("a1-buffered")}})

	// Lane 1 still has a current-checkpoint record before its barrier.
	_ = a.Push(1, aligner.StreamEvent{Record: &aligner.Record{Data: []byte("b0-late")}})
	_ = a.Push(1, aligner.StreamEvent{Barrier: b1}) // alignment completes here

	out := a.Out()
	for i := 0; i < 5; i++ {
		ev := <-out
		if ev.Barrier != nil {
			fmt.Printf("barrier:%d\n", ev.Barrier.ID)
		} else {
			fmt.Println(string(ev.Record.Data))
		}
	}
}
```
Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a0
b0
b0-late
a1-buffered
barrier:1
```

### Tests

`TestPassThroughBeforeBarrier` checks a record emitted before any barrier flows straight out. `TestBuffersAfterBarrier` is the core case: a record pushed on lane 0 after its barrier is held until lane 1's barrier arrives, then flushed before the barrier. `TestAlignmentResetsAcrossCheckpoints` runs two checkpoints back to back to prove the per-lane state is reset. `TestMismatchedBarrierRejected` and `TestDuplicateBarrierRejected` cover the two protocol errors, and `TestConcurrentLanes` pushes from both lanes at once so `go test -race` exercises the lock-then-send path.

Create `aligner_test.go`:
```go
package aligner

import (
	"errors"
	"sync"
	"testing"
)

func TestPassThroughBeforeBarrier(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 8)
	if err := a.Push(0, StreamEvent{Record: &Record{Data: []byte("hello")}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	ev := <-a.Out()
	if ev.Record == nil || string(ev.Record.Data) != "hello" {
		t.Errorf("got %+v, want record data='hello'", ev)
	}
}

func TestBuffersAfterBarrier(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 16)

	push := func(lane int, ev StreamEvent) {
		t.Helper()
		if err := a.Push(lane, ev); err != nil {
			t.Fatalf("Push(lane=%d): %v", lane, err)
		}
	}

	push(0, StreamEvent{Record: &Record{Data: []byte("a")}})
	push(1, StreamEvent{Record: &Record{Data: []byte("b")}})

	b1 := &Barrier{ID: 1}
	push(0, StreamEvent{Barrier: b1})
	push(0, StreamEvent{Record: &Record{Data: []byte("c")}}) // buffered after barrier
	push(1, StreamEvent{Barrier: b1})                        // triggers alignment flush

	out := a.Out()
	type want struct {
		data    string
		barrier bool
		barrID  CheckpointID
	}
	wantSeq := []want{
		{data: "a"},
		{data: "b"},
		{data: "c"},
		{barrier: true, barrID: 1},
	}
	for i, w := range wantSeq {
		ev := <-out
		if w.barrier {
			if ev.Barrier == nil || ev.Barrier.ID != w.barrID {
				t.Errorf("event %d: got %+v, want barrier ID=%d", i, ev, w.barrID)
			}
		} else {
			if ev.Record == nil || string(ev.Record.Data) != w.data {
				t.Errorf("event %d: got %+v, want record data=%q", i, ev, w.data)
			}
		}
	}
}

func TestAlignmentResetsAcrossCheckpoints(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 32)

	drain := func(n int) []StreamEvent {
		t.Helper()
		out := a.Out()
		got := make([]StreamEvent, 0, n)
		for i := 0; i < n; i++ {
			got = append(got, <-out)
		}
		return got
	}

	// Checkpoint 1.
	_ = a.Push(0, StreamEvent{Barrier: &Barrier{ID: 1}})
	_ = a.Push(1, StreamEvent{Barrier: &Barrier{ID: 1}})
	ev := drain(1)
	if ev[0].Barrier == nil || ev[0].Barrier.ID != 1 {
		t.Fatalf("checkpoint 1: got %+v, want barrier 1", ev[0])
	}

	// Checkpoint 2 must succeed; state was reset after checkpoint 1.
	if err := a.Push(0, StreamEvent{Barrier: &Barrier{ID: 2}}); err != nil {
		t.Fatalf("checkpoint 2 lane 0: %v", err)
	}
	if err := a.Push(1, StreamEvent{Barrier: &Barrier{ID: 2}}); err != nil {
		t.Fatalf("checkpoint 2 lane 1: %v", err)
	}
	ev = drain(1)
	if ev[0].Barrier == nil || ev[0].Barrier.ID != 2 {
		t.Fatalf("checkpoint 2: got %+v, want barrier 2", ev[0])
	}
}

func TestMismatchedBarrierRejected(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 4)
	if err := a.Push(0, StreamEvent{Barrier: &Barrier{ID: 1}}); err != nil {
		t.Fatalf("first barrier: %v", err)
	}
	err := a.Push(1, StreamEvent{Barrier: &Barrier{ID: 2}})
	if !errors.Is(err, ErrCheckpointMismatch) {
		t.Errorf("mismatched barrier: err = %v, want ErrCheckpointMismatch", err)
	}
}

func TestDuplicateBarrierRejected(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 4)
	if err := a.Push(0, StreamEvent{Barrier: &Barrier{ID: 1}}); err != nil {
		t.Fatalf("first barrier: %v", err)
	}
	if err := a.Push(0, StreamEvent{Barrier: &Barrier{ID: 1}}); err == nil {
		t.Error("duplicate barrier on lane 0: want error, got nil")
	}
}

func TestOutOfRangeLane(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 4)
	if err := a.Push(5, StreamEvent{Record: &Record{Data: []byte("x")}}); err == nil {
		t.Fatal("Push with invalid lane: want error, got nil")
	}
}

// TestConcurrentLanes pushes records from both lanes at once; -race must report
// no data race and a final aligned barrier must still appear.
func TestConcurrentLanes(t *testing.T) {
	t.Parallel()
	a := NewBarrierAligner(2, 256)

	var wg sync.WaitGroup
	for lane := 0; lane < 2; lane++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = a.Push(lane, StreamEvent{Record: &Record{Data: []byte("r")}})
			}
		}(lane)
	}
	wg.Wait()

	// Drain the 100 records, then align a barrier.
	out := a.Out()
	for i := 0; i < 100; i++ {
		<-out
	}
	_ = a.Push(0, StreamEvent{Barrier: &Barrier{ID: 7}})
	_ = a.Push(1, StreamEvent{Barrier: &Barrier{ID: 7}})
	ev := <-out
	if ev.Barrier == nil || ev.Barrier.ID != 7 {
		t.Errorf("final event = %+v, want barrier 7", ev)
	}
}
```
## Review

The aligner is correct when the output is always one consistent cut: every record produced before the barrier on its lane appears before the barrier in the output, and every record produced after it appears after. The most common error is forgetting to reset `delivered`, `arrived`, and `currentID` once a checkpoint completes — the next barrier is then rejected as a duplicate and alignment wedges forever. The second is buffering records from a lane that has not yet delivered its barrier; those records belong to the current checkpoint and must pass straight through. The third is sending on the output channel under the lock, which deadlocks the moment the consumer calls back into `Push`; the fix is to collect events under the lock and send after releasing it, which `go test -race` confirms is race-free.

## Resources

- [Carbone et al., "Lightweight Asynchronous Snapshots for Distributed Dataflows" (2015)](https://arxiv.org/abs/1506.08603) — the paper behind Flink's barrier alignment.
- [Apache Flink: barrier alignment and checkpointing](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/stateful-stream-processing/) — how a production engine aligns barriers across parallel inputs.
- [Go memory model](https://go.dev/ref/mem) — why the lock must guard every access and why the channel send is the synchronisation point for the consumer.

---

Back to [01-atomic-state-backend.md](01-atomic-state-backend.md) | Next: [03-coordinator-recovery.md](03-coordinator-recovery.md)
