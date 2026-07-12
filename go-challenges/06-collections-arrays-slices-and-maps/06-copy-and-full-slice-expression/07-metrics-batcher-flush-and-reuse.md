# Exercise 7: Metrics Batcher — Copy on Flush, Truncate to Reuse

A metrics batcher accumulates samples in a buffer and, on flush, hands the batch
to a sink and truncates with `buf = buf[:0]` to reuse the backing array with no
reallocation. The flushed batch must be a *copy*: `buf[:0]` keeps the same array,
so the next round of appends overwrites the not-yet-sent batch. This exercise
builds the batcher, proves the buffer is reused without realloc, and reproduces the
corruption of handing `buf` directly to the sink.

Self-contained module: own `go mod init`, own demo, own tests.

## What you'll build

```text
batcher/                   independent module: example.com/batcher
  go.mod                   go 1.26
  batcher.go               type Batcher; Add, Flush (clone), flushDirect (bug), Len, Cap
  cmd/
    demo/
      main.go              accumulate, flush, accumulate again; batches independent
  batcher_test.go          copy-stability, cap-preserved-on-reuse, direct-handoff corruption
```

Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
Implement: `Add`, and `Flush` that clones the buffer, sends it, then truncates with `buf[:0]`; a buggy `flushDirect` that sends `buf` itself.
Test: accumulate, flush capturing the batch, append new events, assert the earlier batch is unchanged; assert `cap` is preserved across `buf[:0]`; a negative sub-test shows direct hand-off overwrite.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/07-metrics-batcher-flush-and-reuse/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/07-metrics-batcher-flush-and-reuse
```

### Why `buf[:0]` reuse forces a copy on flush

`buf = buf[:0]` is the standard buffer-reuse move: it sets the length to zero while
keeping the capacity and the *same backing array*, so the next batch's appends
write into already-allocated memory with no garbage and no reallocation. That is
exactly what you want for a high-throughput batcher flushing thousands of times a
second. The catch is the flip side of "same backing array": whatever you handed
out before truncating still points at that array, and your next appends overwrite
it. If `Flush` sends `buf` itself to the sink and then does `buf = buf[:0]`, the
sink is holding a slice over the very memory the next `Add` calls overwrite. With a
synchronous sink that copies immediately you might get away with it; with an async
sink (a channel to a writer goroutine, a batched network send) the sink reads the
batch *after* the next round of appends has already corrupted it.

`slices.Clone(buf)` breaks the tie: the sink receives an independent batch on its
own array, and `buf[:0]` can safely reuse the original. You copy once per flush —
cheap relative to the batch — and keep the zero-allocation reuse for the hot
append path. This is the general shape of every "reuse a buffer but hand out its
contents" pattern: reuse is free only for data you also copy out.

Create `batcher.go`:

```go
package batcher

import "slices"

// Sample is one metric observation.
type Sample struct {
	Metric string
	Value  int
}

// Sink consumes a flushed batch. A real sink may retain the slice (async send),
// so it must receive memory the batcher will not overwrite.
type Sink func([]Sample)

// Batcher accumulates samples and flushes them in batches, reusing its buffer.
type Batcher struct {
	buf  []Sample
	sink Sink
}

// New returns a batcher that pre-allocates capHint slots for its reusable buffer.
func New(sink Sink, capHint int) *Batcher {
	return &Batcher{buf: make([]Sample, 0, capHint), sink: sink}
}

// Add appends a sample to the current batch.
func (b *Batcher) Add(s Sample) { b.buf = append(b.buf, s) }

// Flush sends an independent copy of the current batch to the sink, then
// truncates the buffer to reuse its backing array.
func (b *Batcher) Flush() {
	if len(b.buf) == 0 {
		return
	}
	batch := slices.Clone(b.buf)
	b.sink(batch)
	b.buf = b.buf[:0]
}

// flushDirect is the buggy variant used only in tests: it hands the live buffer
// to the sink and then reuses it, so subsequent appends overwrite the batch.
func (b *Batcher) flushDirect() {
	if len(b.buf) == 0 {
		return
	}
	b.sink(b.buf)
	b.buf = b.buf[:0]
}

// Len reports the current unflushed sample count.
func (b *Batcher) Len() int { return len(b.buf) }

// Cap reports the buffer capacity (stable across flushes that reuse it).
func (b *Batcher) Cap() int { return cap(b.buf) }
```

### The runnable demo

The demo collects flushed batches, accumulates one batch and flushes, then
accumulates a second and flushes, printing both to show they are independent even
though the buffer was reused.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batcher"
)

func main() {
	var batches [][]batcher.Sample
	b := batcher.New(func(batch []batcher.Sample) {
		batches = append(batches, batch)
	}, 8)

	capStart := b.Cap()

	b.Add(batcher.Sample{Metric: "cpu", Value: 10})
	b.Add(batcher.Sample{Metric: "cpu", Value: 20})
	b.Flush()

	b.Add(batcher.Sample{Metric: "mem", Value: 30})
	b.Flush()

	fmt.Println("batch 0:", batches[0])
	fmt.Println("batch 1:", batches[1])
	fmt.Println("cap reused unchanged:", b.Cap() == capStart)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch 0: [{cpu 10} {cpu 20}]
batch 1: [{mem 30}]
cap reused unchanged: true
```

### Tests

`TestFlushBatchesAreStable` flushes one batch, appends new samples, flushes again,
and asserts the first captured batch is unchanged. `TestBufferReusePreservesCap`
asserts `cap` is identical before and after a flush (the reuse did not
reallocate). `TestDirectHandoffCorrupts` uses `flushDirect`, appends after the
flush, and asserts the captured batch was overwritten — the aliasing bug.

Create `batcher_test.go`:

```go
package batcher

import (
	"slices"
	"testing"
)

func collector() (Sink, *[][]Sample) {
	var batches [][]Sample
	return func(b []Sample) { batches = append(batches, b) }, &batches
}

func TestFlushBatchesAreStable(t *testing.T) {
	t.Parallel()

	sink, batches := collector()
	b := New(sink, 8)

	b.Add(Sample{"cpu", 10})
	b.Add(Sample{"cpu", 20})
	b.Flush()

	b.Add(Sample{"mem", 30})
	b.Flush()

	want0 := []Sample{{"cpu", 10}, {"cpu", 20}}
	if !slices.Equal((*batches)[0], want0) {
		t.Fatalf("batch 0 = %v, want %v (copy not stable)", (*batches)[0], want0)
	}
	want1 := []Sample{{"mem", 30}}
	if !slices.Equal((*batches)[1], want1) {
		t.Fatalf("batch 1 = %v, want %v", (*batches)[1], want1)
	}
}

func TestBufferReusePreservesCap(t *testing.T) {
	t.Parallel()

	sink, _ := collector()
	b := New(sink, 8)
	b.Add(Sample{"cpu", 1})
	b.Add(Sample{"cpu", 2})

	before := b.Cap()
	b.Flush()
	after := b.Cap()
	if before != after {
		t.Fatalf("cap changed across flush: before %d, after %d", before, after)
	}
	if b.Len() != 0 {
		t.Fatalf("len after flush = %d, want 0", b.Len())
	}
}

func TestDirectHandoffCorrupts(t *testing.T) {
	t.Parallel()

	sink, batches := collector()
	b := New(sink, 8)

	b.Add(Sample{"cpu", 10})
	b.Add(Sample{"cpu", 20})
	b.flushDirect() // hands the live buffer to the sink

	// Next appends reuse the same backing array and overwrite the batch.
	b.Add(Sample{"mem", 99})
	b.Add(Sample{"mem", 88})

	got := (*batches)[0]
	corrupted := []Sample{{"mem", 99}, {"mem", 88}}
	if !slices.Equal(got, corrupted) {
		t.Fatalf("expected direct hand-off to corrupt batch to %v, got %v", corrupted, got)
	}
}
```

## Review

The batcher is correct when a flushed batch is immune to later appends:
`TestFlushBatchesAreStable` proves the first batch survives a second round, and
`TestBufferReusePreservesCap` proves the buffer was reused in place rather than
reallocated (the performance point of `buf[:0]`). The negative
`TestDirectHandoffCorrupts` shows the failure the copy prevents — the sink's batch
is overwritten by the next appends because it aliased the reused buffer. The rule:
`buf[:0]` reuse is free, but anything you hand out before reusing must be a copy;
with an async sink, skipping the copy is a corruption that surfaces only under
load.

## Resources

- [slices package (`Clone`, `Equal`)](https://pkg.go.dev/slices)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [Go wiki: SliceTricks (reuse / truncate)](https://go.dev/wiki/SliceTricks)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-in-place-filter-with-delete-and-zeroing.md](06-in-place-filter-with-delete-and-zeroing.md) | Next: [08-length-prefixed-frame-reader.md](08-length-prefixed-frame-reader.md)
