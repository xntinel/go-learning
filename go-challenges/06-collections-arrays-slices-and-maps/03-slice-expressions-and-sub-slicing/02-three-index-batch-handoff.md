# Exercise 2: Batch Flusher That Hands Out Sub-Batches Without Clobbering the Buffer

A metrics/event batcher accumulates items in one reused backing buffer and hands
consecutive chunks of it to a flush sink. The naive `buf[i:j]` handoff lets the
sink's `append` overwrite later, not-yet-flushed items still living in the shared
array. The fix is the three-index expression `buf[i:j:j]`, which makes `len == cap`
so any `append` in the sink is forced to a fresh allocation. This exercise makes
the bug reproduce, then fixes it, teaching capacity — not length — as the real
isolation boundary.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
batcher/                   independent module: example.com/batcher
  go.mod                   go 1.24
  batcher.go               FlushInChunks[T](buf, size, sink); SafeCut[T](buf, i, j)
  cmd/
    demo/
      main.go              runnable demo comparing leaky vs safe handoff
  batcher_test.go          leak-reproduction test, isolation test, cap==len assertion
```

- Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
- Implement: `FlushInChunks`, which cuts each `size`-element chunk with the
  three-index expression and passes it to `sink`; `SafeCut`, a helper that returns
  `buf[i:j:j]` via `slices.Clip`.
- Test: hand out a sub-batch, `append` to it in the consumer, and assert later
  buffer items are untouched; contrast a two-index cut whose `append` leaks into
  the buffer; assert `cap(subBatch) == len(subBatch)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/02-three-index-batch-handoff/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/02-three-index-batch-handoff
go mod edit -go=1.24
```

## Why length does not isolate a sub-batch

Picture a batcher holding `buf = [e0 e1 e2 e3]` with spare capacity behind it, and a
flush sink that, for each chunk it receives, appends a trailing checksum sentinel
before shipping it. You flush chunk 0 as `buf[0:2]`. That two-index cut has length 2
but capacity `cap(buf) - 0`, so it still owns the whole array. When the sink does
`append(chunk, checksum)`, `append` sees spare capacity and writes the checksum into
`buf[2]` in place — corrupting `e2`, an item you have not flushed yet. Under load,
with the sink running concurrently or the buffer being refilled, this is a
maddening "events change value after I batched them" bug.

The cut `buf[0:2:2]` fixes it. The three-index form sets capacity to `2 - 0 == 2`,
so the chunk has `len == cap`. The sink's `append` now finds no spare room and is
forced to allocate a fresh array; the checksum lands there, and `buf[2]` is never
touched. Capacity, not length, was the isolation boundary all along.
`slices.Clip(buf[i:j])` is the same thing spelled with a helper — it returns
`buf[i:j : j]` by reducing capacity to length.

Note the distinction from Exercise 1: here we deliberately hand out a *view* into
the shared buffer (zero copy, for throughput) rather than a clone. We do not want a
copy — we want a bounded window whose `append` cannot escape. That is exactly what
the three-index expression buys.

Create `batcher.go`:

```go
package batcher

import "slices"

// FlushInChunks passes consecutive size-element sub-batches of buf to sink. Each
// sub-batch is cut with the three-index expression buf[i:j:j] so that len == cap:
// if sink appends to the sub-batch, append is forced to allocate a fresh array
// and cannot overwrite later, not-yet-flushed elements still living in buf.
func FlushInChunks[T any](buf []T, size int, sink func([]T)) {
	if size <= 0 {
		return
	}
	for i := 0; i < len(buf); i += size {
		j := min(i+size, len(buf))
		sink(buf[i:j:j])
	}
}

// SafeCut returns the sub-batch buf[i:j] with its capacity clipped to its length,
// so an append by the consumer reallocates instead of writing into buf. It is
// equivalent to buf[i:j:j].
func SafeCut[T any](buf []T, i, j int) []T {
	return slices.Clip(buf[i:j])
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batcher"
)

func main() {
	// A buffer of four events with spare capacity behind it.
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 20, 30, 40})

	// Leaky: a two-index cut inherits buf's spare capacity.
	leaky := buf[0:2]
	_ = append(leaky, 999) // writes into buf[2] in place
	fmt.Println("after leaky append, buf:", buf)

	// Reset and flush safely in chunks of two.
	copy(buf, []int{10, 20, 30, 40})
	batcher.FlushInChunks(buf, 2, func(chunk []int) {
		fmt.Printf("chunk len=%d cap=%d %v\n", len(chunk), cap(chunk), chunk)
		_ = append(chunk, -1) // cannot reach buf: len == cap forces a new array
	})
	fmt.Println("after safe flush, buf:", buf)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after leaky append, buf: [10 20 999 40]
chunk len=2 cap=2 [10 20]
chunk len=2 cap=2 [30 40]
after safe flush, buf: [10 20 30 40]
```

## Tests

The first test documents the bug: a two-index cut's `append` leaks into the buffer.
The second proves the fix: the three-index handoff leaves later items untouched even
when the sink appends. The third pins the mechanism — `cap == len` on the handed-out
sub-batch.

Create `batcher_test.go`:

```go
package batcher

import (
	"slices"
	"testing"
)

// TestTwoIndexCutLeaks documents the bug the three-index form prevents: a
// two-index sub-batch inherits spare capacity, so the consumer's append writes
// into a later, not-yet-flushed element of the shared buffer.
func TestTwoIndexCutLeaks(t *testing.T) {
	t.Parallel()
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 20, 30, 40})

	sub := buf[0:2] // len 2, cap 8 (inherits spare capacity)
	_ = append(sub, 999)

	if buf[2] != 999 {
		t.Fatalf("expected two-index append to leak into buf[2]; buf = %v", buf)
	}
}

// TestThreeIndexCutIsolates proves the fix: the sink appends to its sub-batch and
// the later, not-yet-flushed items in buf are untouched.
func TestThreeIndexCutIsolates(t *testing.T) {
	t.Parallel()
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 20, 30, 40})

	var flushed [][]int
	FlushInChunks(buf, 2, func(chunk []int) {
		if cap(chunk) != len(chunk) {
			t.Errorf("chunk cap %d != len %d; not isolated", cap(chunk), len(chunk))
		}
		got := append(chunk, -1) // must reallocate, not touch buf
		flushed = append(flushed, got)
	})

	if !slices.Equal(buf, []int{10, 20, 30, 40}) {
		t.Fatalf("safe flush corrupted buf: %v", buf)
	}
	if len(flushed) != 2 {
		t.Fatalf("got %d chunks, want 2", len(flushed))
	}
}

func TestSafeCutClipsCapacity(t *testing.T) {
	t.Parallel()
	buf := make([]int, 4, 16)
	copy(buf, []int{1, 2, 3, 4})

	sub := SafeCut(buf, 1, 3)
	if len(sub) != 2 || cap(sub) != 2 {
		t.Fatalf("SafeCut len=%d cap=%d; want 2,2", len(sub), cap(sub))
	}
	if !slices.Equal(sub, []int{2, 3}) {
		t.Fatalf("SafeCut = %v; want [2 3]", sub)
	}
	_ = append(sub, 99)
	if buf[3] != 4 {
		t.Fatalf("SafeCut append leaked into buf[3]: %v", buf)
	}
}

func TestFlushInChunksCoversEverything(t *testing.T) {
	t.Parallel()
	buf := []int{1, 2, 3, 4, 5} // 5 elements, chunk size 2 -> 2,2,1
	var seen []int
	var sizes []int
	FlushInChunks(buf, 2, func(chunk []int) {
		sizes = append(sizes, len(chunk))
		seen = append(seen, chunk...)
	})
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Fatalf("chunk sizes = %v; want [2 2 1]", sizes)
	}
	if !slices.Equal(seen, buf) {
		t.Fatalf("flushed %v; want %v", seen, buf)
	}
}
```

## Review

The batcher is correct when every element is flushed exactly once, in order, and no
sink `append` is ever observable back in `buf`. The leak test and the isolation test
are two sides of the same coin: the two-index cut has capacity 8 and clobbers
`buf[2]`; the three-index cut has capacity 2 and cannot. If `TestThreeIndexCutIsolates`
ever fails, the handoff has reverted to a two-index cut somewhere. The trap to avoid
is reasoning about the sub-batch's *length* (2, looks safe) instead of its
*capacity* (8, welded to the buffer). When you deliberately share a buffer window
for throughput, bound its capacity so `append` cannot escape; when you must retain
data past the buffer's reuse, copy it instead (Exercise 1). Run `go test -race`.

## Resources

- [Go Specification: Slice expressions (full slice expression)](https://go.dev/ref/spec#Slice_expressions)
- [`slices.Clip`](https://pkg.go.dev/slices#Clip)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-log-window-pagination.md](01-log-window-pagination.md) | Next: [03-request-header-slice-aliasing.md](03-request-header-slice-aliasing.md)
