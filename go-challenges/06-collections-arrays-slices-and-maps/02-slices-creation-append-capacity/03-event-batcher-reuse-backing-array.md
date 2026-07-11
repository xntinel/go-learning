# Exercise 3: Batch Flush Buffer That Reuses Its Backing Array

A batcher accumulates events and flushes them in fixed-size groups — the shape of
a metrics shipper, a bulk-insert writer, or a log forwarder. The performance-
critical detail is what happens *after* a flush: resetting with `buf = buf[:0]`
keeps the same backing array so the next batch fills in place, instead of
allocating a fresh array on every cycle. This is the buffer-reuse idiom, and it
comes with one sharp edge the exercise makes explicit.

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
batcher/                   independent module: example.com/batcher
  go.mod                   go 1.26
  batcher.go               Event; Batcher; New, Add, Len, Flush
  cmd/
    demo/
      main.go              feed events, observe batch boundaries
  batcher_test.go          boundary + contents + backing-array-reuse proof, Example
```

Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
Implement: a `Batcher` that appends events, calls a flush handler when `len == batchSize`, and resets with `buf = buf[:0]` to reuse the backing array.
Test: feed N events and assert batch boundaries and contents; capture the address of `&buf[0]` and the capacity across two flush cycles to prove the backing array is reused (no reallocation) while old elements are logically dropped.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batcher/cmd/demo
cd ~/go-exercises/batcher
go mod init example.com/batcher
go mod edit -go=1.26
```

### Why buf[:0] reuses the array, and the retention trap it creates

`New` reserves capacity once with `make([]Event, 0, batchSize)`. Each `Add`
appends into that reserved capacity, so no reallocation happens as a batch fills.
When `len(buf) == batchSize`, the batcher hands the full slice to the flush
handler and then does `buf = buf[:0]`. That reslice sets length back to zero while
keeping the *same pointer, same capacity, same backing array* — it does not
allocate. The next batch's appends overwrite the same slots the previous batch
used. Across a million batches, the backing array is allocated exactly once.

The sharp edge: because the backing array is reused, the slice passed to the flush
handler is only valid *during* the flush call. The moment `Add` starts filling the
next batch, it overwrites the elements the previous batch's slice still points at.
If the flush handler retains that slice — stores it, sends it to another goroutine,
appends it to a longer-lived collection — it is holding a window that the next
batch will corrupt. This is the same class of bug as retaining a pooled read
buffer (Exercise 9): a handler that needs to keep the data must copy it
(`slices.Clone`) before returning. The synchronous handler in this exercise
consumes each batch immediately, which is why reuse is safe here.

Create `batcher.go`:

```go
package batcher

// Event is one unit of work to be flushed in a batch.
type Event struct {
	ID      int
	Payload string
}

// Batcher accumulates events and invokes flush when a full batch is ready. It
// reuses a single backing array across batches via buf = buf[:0].
//
// The slice passed to flush is valid only for the duration of the call; the
// next Add overwrites its elements. A handler that retains the data must copy
// it first.
type Batcher struct {
	buf       []Event
	batchSize int
	flush     func([]Event)
}

// New returns a Batcher that calls flush every batchSize events. The backing
// array is allocated once, sized to batchSize.
func New(batchSize int, flush func([]Event)) *Batcher {
	if batchSize < 1 {
		batchSize = 1
	}
	return &Batcher{
		buf:       make([]Event, 0, batchSize),
		batchSize: batchSize,
		flush:     flush,
	}
}

// Add appends e and flushes when the batch is full, then resets the buffer to
// zero length while retaining its backing array.
func (b *Batcher) Add(e Event) {
	b.buf = append(b.buf, e)
	if len(b.buf) == b.batchSize {
		b.flush(b.buf)
		b.buf = b.buf[:0]
	}
}

// Len reports how many events are buffered but not yet flushed.
func (b *Batcher) Len() int { return len(b.buf) }

// Flush forces a flush of the pending partial batch, if any, and resets.
func (b *Batcher) Flush() {
	if len(b.buf) > 0 {
		b.flush(b.buf)
		b.buf = b.buf[:0]
	}
}
```

### The runnable demo

The demo flushes every two events. Feeding five events produces two full batches
and leaves one pending, which the explicit `Flush` at the end emits.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batcher"
)

func main() {
	b := batcher.New(2, func(batch []batcher.Event) {
		ids := make([]int, len(batch))
		for i, e := range batch {
			ids[i] = e.ID
		}
		fmt.Printf("flush batch %v\n", ids)
	})

	for i := 1; i <= 5; i++ {
		b.Add(batcher.Event{ID: i, Payload: "p"})
	}
	fmt.Printf("pending before final flush: %d\n", b.Len())
	b.Flush()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flush batch [1 2]
flush batch [3 4]
pending before final flush: 1
flush batch [5]
```

### Tests

`TestBatchBoundaries` feeds seven events with a batch size of three and asserts
the handler saw `[1 2 3]`, `[4 5 6]`, then `[7]` after the final `Flush`.
`TestReusesBackingArray` is the core proof: inside the flush handler it captures
`&batch[0]` and `cap(batch)` on the first two full batches and asserts they are
identical, demonstrating the second batch wrote into the very same backing array.
Because the handler must copy to safely record contents, `TestBatchBoundaries`
clones each batch — modeling the correct retention discipline.

Create `batcher_test.go`:

```go
package batcher

import (
	"fmt"
	"slices"
	"testing"
)

func idsOf(batch []Event) []int {
	ids := make([]int, len(batch))
	for i, e := range batch {
		ids[i] = e.ID
	}
	return ids
}

func TestBatchBoundaries(t *testing.T) {
	t.Parallel()
	var got [][]int
	b := New(3, func(batch []Event) {
		got = append(got, idsOf(batch)) // copy out; batch is reused
	})
	for i := 1; i <= 7; i++ {
		b.Add(Event{ID: i})
	}
	b.Flush()

	want := [][]int{{1, 2, 3}, {4, 5, 6}, {7}}
	if len(got) != len(want) {
		t.Fatalf("got %d batches, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("batch %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestReusesBackingArray(t *testing.T) {
	t.Parallel()
	var firstPtr, secondPtr *Event
	var firstCap, secondCap int
	n := 0
	b := New(2, func(batch []Event) {
		n++
		switch n {
		case 1:
			firstPtr, firstCap = &batch[0], cap(batch)
		case 2:
			secondPtr, secondCap = &batch[0], cap(batch)
		}
	})
	// Two full batches of size 2.
	for i := range 4 {
		b.Add(Event{ID: i})
	}
	if firstPtr != secondPtr {
		t.Fatalf("backing array changed between batches: %p != %p", firstPtr, secondPtr)
	}
	if firstCap != secondCap {
		t.Fatalf("capacity changed: %d != %d", firstCap, secondCap)
	}
}

func TestResetDropsOldElementsLogically(t *testing.T) {
	t.Parallel()
	b := New(3, func([]Event) {})
	for i := range 3 {
		b.Add(Event{ID: i})
	}
	// After the flush the length is zero even though the array still holds data.
	if b.Len() != 0 {
		t.Fatalf("Len after full batch = %d, want 0", b.Len())
	}
	b.Add(Event{ID: 99})
	if b.Len() != 1 {
		t.Fatalf("Len after one more Add = %d, want 1", b.Len())
	}
}

func ExampleBatcher() {
	b := New(2, func(batch []Event) {
		fmt.Println(idsOf(batch))
	})
	for i := 1; i <= 4; i++ {
		b.Add(Event{ID: i})
	}
	// Output:
	// [1 2]
	// [3 4]
}
```

## Review

The batcher is correct when a full batch is delivered exactly at the size
boundary, the partial tail is delivered only by `Flush`, and the reset reuses the
backing array rather than allocating a new one each cycle.
`TestReusesBackingArray` proves the reuse by pointer identity of `&batch[0]` across
two batches; if that assertion fails, something replaced the backing array (for
example, a stray `append` after the reset that exceeded capacity, or a `make` in
the reset path). The trap this exercise drills is retention: the flush handler in
`TestBatchBoundaries` copies each batch out with `idsOf` before the next batch
overwrites it, which is the only safe way to keep the data. A handler that stored
the raw `[]Event` would see it mutate under it — the same failure as retaining a
pooled read buffer. Run `-race` to confirm the single-goroutine reuse is sound.

## Resources

- [Go Wiki: SliceTricks (reuse and reset idioms)](https://go.dev/wiki/SliceTricks)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [`slices.Clone`](https://pkg.go.dev/slices#Clone)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-preallocate-repository-result-slice.md](02-preallocate-repository-result-slice.md) | Next: [04-cache-aliasing-append-bug.md](04-cache-aliasing-append-bug.md)
