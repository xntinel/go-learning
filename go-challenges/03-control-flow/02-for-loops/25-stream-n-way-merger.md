# Exercise 25: Merging N Sorted Input Streams into One Output

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Merging two sorted lists is a single condition-only pass — Exercise 15
already built that. Merging *N* sorted streams (a common shape when
combining sorted output from several shards, or several sorted index
segments) needs a different structure: a min-heap over each stream's current
head, an outer loop that pops the overall minimum, and a nested loop that
collapses duplicate values appearing in more than one stream into a single
output value. This module builds that classical N-way merge with a labeled
`break` that lets an output cap interrupt cleanly from inside the nested
dedup pass.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
merge/                         module example.com/merge
  go.mod                       go 1.24
  merge.go                     minHeap (container/heap.Interface); Merge(streams, maxOutput) []int
  merge_test.go                  three-stream table, duplicates, cap mid-merge, cap on a duplicate boundary
  cmd/demo/
    main.go                     three streams with overlapping values, full merge and a capped merge
```

- Files: `merge.go`, `merge_test.go`, `cmd/demo/main.go`.
- Implement: a `minHeap` implementing `container/heap.Interface` over each stream's current head value; `Merge(streams [][]int, maxOutput int) []int` — an outer `for h.Len() > 0` loop popping the minimum, with a nested `for h.Len() > 0 && (*h)[0].value == val` loop draining duplicates of that same value from other streams, and a `break merge` the instant `maxOutput` is reached.
- Test: three interleaved streams merge in full sorted order; a value duplicated across streams collapses to one output; the cap stops mid-merge; the cap lands exactly on a duplicate boundary; one empty stream among several; a single stream; every stream empty; no streams at all.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/25-stream-n-way-merger/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/25-stream-n-way-merger
go mod edit -go=1.24
```

### Why the cap needs a labeled break inside a nested loop

The heap gives `Merge` its termination proof for free: `h.Len() > 0` shrinks
by exactly one every time a stream is fully consumed and never replenished,
so the outer loop is a plain condition-only pass with no separate counter
needed. The nested loop exists for a reason that has nothing to do with the
heap's mechanics — it exists because "the same value shows up in two
different streams" is a real occurrence (think two sharded indexes that
both contain the same key), and the caller wants one merged output, not one
per source stream. Popping every head equal to the just-emitted value,
replenishing each stream it came from, is what makes that collapse happen
in the same pass rather than as a second dedup step over the whole output.

The cap is where the nesting stops being just an implementation detail: once
`maxOutput` values have been emitted, the merge must stop touching the heap
entirely, even mid-dedup. A plain `break` written inside the dedup `for`
would only end *that* duplicate-draining pass — the outer loop would then
pop its next minimum and keep going, emitting one value past the cap. The
labeled `break merge` is what guarantees the cap is exact: the instant
`len(out) >= maxOutput`, both loops end together, regardless of whether the
value that hit the cap happened to be a duplicate or not.
`TestMergeCapExactlyAtDuplicateBoundary` is the test built specifically to
land the cap on that seam.

Create `merge.go`:

```go
package merge

import "container/heap"

// streamItem is one candidate value sitting at the head of one input stream.
type streamItem struct {
	value     int
	streamIdx int
}

// minHeap is a container/heap.Interface over the current head of every
// stream, ordered by value so the smallest head is always at index 0.
type minHeap []streamItem

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].value < h[j].value }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(streamItem)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// Merge combines N already-sorted input streams into one sorted, duplicate-
// free output, stopping after at most maxOutput values. It keeps one pointer
// per stream (via positions) and a min-heap of the current head of every
// stream that still has values left -- the classic multi-way merge.
//
// The outer for loop pops the overall minimum and is bounded by a condition
// only: keep going while the heap is non-empty. Nested inside it, an inner
// for loop drains every other stream whose head currently equals that same
// minimum, so a value present in several streams is only emitted once. The
// labeled break is what lets the cap apply cleanly from inside that nested
// dedup loop: the instant maxOutput is reached, both loops must stop in the
// same statement, because a plain break there would only end the dedup pass
// and leave the outer merge loop popping values nobody asked for.
func Merge(streams [][]int, maxOutput int) []int {
	positions := make([]int, len(streams))
	h := &minHeap{}
	heap.Init(h)
	for i, s := range streams {
		if len(s) > 0 {
			heap.Push(h, streamItem{value: s[0], streamIdx: i})
			positions[i] = 1
		}
	}

	var out []int

merge:
	for h.Len() > 0 {
		item := heap.Pop(h).(streamItem)
		val := item.value
		advance(h, positions, streams, item)

		// Drain every other stream's head that equals val, so duplicates
		// across streams collapse into a single output value.
		for h.Len() > 0 && (*h)[0].value == val {
			dup := heap.Pop(h).(streamItem)
			advance(h, positions, streams, dup)
		}

		out = append(out, val)
		if len(out) >= maxOutput {
			break merge
		}
	}

	return out
}

// advance pushes the next value from popped's stream onto the heap, if that
// stream has one, and records the new read position for it.
func advance(h *minHeap, positions []int, streams [][]int, popped streamItem) {
	pos := positions[popped.streamIdx]
	if pos < len(streams[popped.streamIdx]) {
		heap.Push(h, streamItem{value: streams[popped.streamIdx][pos], streamIdx: popped.streamIdx})
		positions[popped.streamIdx] = pos + 1
	}
}
```

### The runnable demo

The demo merges three overlapping streams (values 5 and 9 each appear
twice), once in full and once capped at 4 values.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/merge"
)

func main() {
	streams := [][]int{
		{1, 5, 9, 20},
		{2, 5, 10},
		{3, 4, 9, 15},
	}

	full := merge.Merge(streams, 100)
	fmt.Printf("full merge: %v\n", full)

	capped := merge.Merge(streams, 4)
	fmt.Printf("capped at 4: %v\n", capped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
full merge: [1 2 3 4 5 9 10 15 20]
capped at 4: [1 2 3 4]
```

### Tests

`TestMerge` is a table over the shapes the multi-way merge has to get right:
plain interleaving, cross-stream duplicates, an empty stream mixed in with
non-empty ones, a single stream, and both empty-input variants.
`TestMergeStopsAtMaxOutput` checks the ordinary cap case, and
`TestMergeCapExactlyAtDuplicateBoundary` is the sharpest one — it sizes the
cap so it lands exactly on a value that is duplicated across two streams,
which is the only scenario that can catch a `break` that exits the wrong
loop.

Create `merge_test.go`:

```go
package merge

import (
	"slices"
	"testing"
)

func TestMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		streams   [][]int
		maxOutput int
		want      []int
	}{
		{
			name: "three sorted streams merge in order",
			streams: [][]int{
				{1, 4, 7},
				{2, 5, 8},
				{3, 6, 9},
			},
			maxOutput: 100,
			want:      []int{1, 2, 3, 4, 5, 6, 7, 8, 9},
		},
		{
			name: "duplicate values across streams collapse to one",
			streams: [][]int{
				{1, 2, 5},
				{2, 3, 5},
				{2, 4},
			},
			maxOutput: 100,
			want:      []int{1, 2, 3, 4, 5},
		},
		{
			name:      "one empty stream among several",
			streams:   [][]int{{1, 3}, {}, {2, 4}},
			maxOutput: 100,
			want:      []int{1, 2, 3, 4},
		},
		{
			name:      "single stream",
			streams:   [][]int{{1, 2, 3}},
			maxOutput: 100,
			want:      []int{1, 2, 3},
		},
		{
			name:      "all streams empty",
			streams:   [][]int{{}, {}},
			maxOutput: 100,
			want:      nil,
		},
		{
			name:      "no streams at all",
			streams:   nil,
			maxOutput: 100,
			want:      nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Merge(tc.streams, tc.maxOutput)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Merge() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeStopsAtMaxOutput(t *testing.T) {
	t.Parallel()

	streams := [][]int{
		{1, 4, 7, 10},
		{2, 5, 8, 11},
		{3, 6, 9, 12},
	}

	got := Merge(streams, 5)
	want := []int{1, 2, 3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("Merge() = %v, want %v", got, want)
	}
}

func TestMergeCapExactlyAtDuplicateBoundary(t *testing.T) {
	t.Parallel()

	// The cap of 2 lands exactly on a value ("2") that is duplicated across
	// two streams -- the dedup pass and the cap check must interact cleanly.
	streams := [][]int{
		{1, 2, 9},
		{2, 3, 9},
	}

	got := Merge(streams, 2)
	want := []int{1, 2}
	if !slices.Equal(got, want) {
		t.Fatalf("Merge() = %v, want %v", got, want)
	}
}
```

## Review

`Merge` is correct when its output is sorted, contains no duplicate values
across streams, and never exceeds `maxOutput` entries — and each of those
three properties traces back to one specific piece of the control flow: the
heap gives sortedness, the nested dedup loop gives uniqueness, and the
labeled break gives the exact cap. The common mistake this design avoids is
checking the cap only at the top of the outer loop (`if len(out) >=
maxOutput { break }` before popping) — that looks equivalent for every
non-duplicate case, but it lets a full run of duplicate-draining happen
*after* the cap should have already stopped things, and it can also emit one
value past the cap if the check is placed after emitting instead of right
where this implementation puts it. Run `go test -count=1 ./...`.

## Resources

- [container/heap package](https://pkg.go.dev/container/heap) — the `heap.Interface` this module implements for the min-heap.
- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — the labeled form used to exit the nested merge/dedup loops together.
- [External sorting and k-way merge (Wikipedia)](https://en.wikipedia.org/wiki/External_sorting#External_merge_sort) — the classical algorithm this module implements over in-memory streams.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-shard-leader-detector.md](24-shard-leader-detector.md) | Next: [26-consistent-hash-ring-shard-lookup.md](26-consistent-hash-ring-shard-lookup.md)
