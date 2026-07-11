# Exercise 23: Multi-Stream Merge-Join — Coordinating N Sorted Inputs with `iter.Pull` State Machines

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

The two-stream merge-join earlier in this lesson only needs to peek one
value from each side and advance the smaller. Merging N sharded, pre-sorted
inputs -- log files from N replicas, N partition readers that each promise
ascending keys -- needs the same idea generalized: peek all N fronts at once
and always advance whichever one is smallest. A single push loop cannot
do that because it only ever sees the value one stream is currently
yielding, so this exercise converts every input stream to a pull-based
cursor with `iter.Pull` and coordinates them with a min-heap. This exercise
is an independent module with its own `go mod init`.

## What you'll build

```text
mergejoin/                 independent module: example.com/merge-join-multiple-sorted-streams
  go.mod                   module example.com/merge-join-multiple-sorted-streams
  mergejoin.go             Merge
  cmd/
    demo/
      main.go              runnable demo: merging 3 sorted streams into one
  mergejoin_test.go        three-stream merge, empty stream, early-stop closes every cursor
```

Implement: `Merge(streams []iter.Seq[int]) iter.Seq[int]` yielding every value across all streams in ascending order.
Test: three sorted streams of varying lengths merge into one fully sorted sequence; a stream that is empty is handled without error; a consumer break after two values stops the merge and every stream's cleanup still runs.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/merge-join-multiple-sorted-streams/cmd/demo
cd ~/go-exercises/merge-join-multiple-sorted-streams
go mod init example.com/merge-join-multiple-sorted-streams
go mod edit -go=1.24
```

Every stream is converted to a `next, stop := iter.Pull(s)` cursor up front,
before any merging happens, and every `stop` is collected into a slice so a
single `defer` can tear all of them down together -- that is the detail that
generalizes correctly from 2 streams to N: with 2 streams it is tempting to
just `defer stop1()` and `defer stop2()` as two separate lines, but that does
not scale, and forgetting even one of N deferred `stop` calls leaks that
stream's goroutine. A `container/heap` min-heap holds one peeked `(value,
streamIndex)` pair per still-live stream; popping the minimum, yielding it,
and then pulling one more value from *that same stream* (pushing it back
onto the heap if there was one) is the entire merge algorithm. Because the
heap only ever holds one pending value per stream, memory is `O(N)`
regardless of how long any individual stream is, and because popping the
minimum is `O(log N)`, merging is efficient even with many input streams.

Create `mergejoin.go`:

```go
package mergejoin

import (
	"container/heap"
	"iter"
)

// item is one pulled value paired with the index of the stream it came from,
// so that once it is popped off the heap the merge knows which cursor to
// advance next.
type item struct {
	val    int
	stream int
}

type minHeap []item

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].val < h[j].val }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// Merge merges N already-ascending-sorted streams into a single ascending
// stream, which is the core of a multi-way merge-join. Each input stream is
// converted to a pull-based cursor with iter.Pull so the merge can hold one
// peeked value from every stream at once and pick the smallest -- something
// a single push range over one stream cannot do, since a push iterator only
// exposes the value it is currently yielding. A min-heap keyed on the peeked
// values keeps picking the smallest peek an O(log N) operation instead of an
// O(N) scan on every output value. Every cursor's stop function is deferred
// up front, before the heap loop runs, so an early consumer break or a panic
// anywhere in the loop tears down every still-open stream -- not just the
// one whose value was yielded last -- which is what "coordinating N sorted
// inputs" has to guarantee to avoid leaking the other N-1 goroutines that
// iter.Pull started.
func Merge(streams []iter.Seq[int]) iter.Seq[int] {
	return func(yield func(int) bool) {
		nexts := make([]func() (int, bool), len(streams))
		stops := make([]func(), len(streams))
		for i, s := range streams {
			next, stop := iter.Pull(s)
			nexts[i] = next
			stops[i] = stop
		}
		defer func() {
			for _, stop := range stops {
				stop()
			}
		}()

		h := &minHeap{}
		heap.Init(h)
		for i := range nexts {
			if v, ok := nexts[i](); ok {
				heap.Push(h, item{val: v, stream: i})
			}
		}

		for h.Len() > 0 {
			top := heap.Pop(h).(item)
			if !yield(top.val) {
				return
			}
			if v, ok := nexts[top.stream](); ok {
				heap.Push(h, item{val: v, stream: top.stream})
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"iter"
	"slices"

	"example.com/merge-join-multiple-sorted-streams"
)

func main() {
	streams := []iter.Seq[int]{
		slices.Values([]int{1, 4, 9}),
		slices.Values([]int{2, 3, 10}),
		slices.Values([]int{0, 5, 6, 7}),
	}

	for v := range mergejoin.Merge(streams) {
		fmt.Println(v)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
0
1
2
3
4
5
6
7
9
10
```

Three sorted streams of lengths 3, 3, and 4 -- ten values total -- merge into
one fully ascending sequence, with `8` correctly absent because it was never
in any input stream.

### Tests

The early-stop test uses a helper stream that records whether it ran its own
`defer` cleanup, which is what proves `Merge`'s single deferred loop over
every `stop` actually tears down all three cursors, not just the one whose
value happened to be yielded last.

Create `mergejoin_test.go`:

```go
package mergejoin

import (
	"iter"
	"slices"
	"testing"
)

func trackedStream(vals []int, closed *bool) iter.Seq[int] {
	return func(yield func(int) bool) {
		defer func() { *closed = true }()
		for _, v := range vals {
			if !yield(v) {
				return
			}
		}
	}
}

func TestMergeThreeSortedStreams(t *testing.T) {
	t.Parallel()

	streams := []iter.Seq[int]{
		slices.Values([]int{1, 4, 9}),
		slices.Values([]int{2, 3, 10}),
		slices.Values([]int{0, 5, 6, 7}),
	}

	var got []int
	for v := range Merge(streams) {
		got = append(got, v)
	}

	want := []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got = %v, want %v", got, want)
		}
	}
}

func TestMergeHandlesEmptyStreams(t *testing.T) {
	t.Parallel()

	streams := []iter.Seq[int]{
		slices.Values([]int{1, 2}),
		slices.Values([]int{}),
		slices.Values([]int{3}),
	}

	var got []int
	for v := range Merge(streams) {
		got = append(got, v)
	}
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got = %v, want %v", got, want)
		}
	}
}

func TestMergeStopsEarlyAndClosesEveryStream(t *testing.T) {
	t.Parallel()

	closed := make([]bool, 3)
	streams := []iter.Seq[int]{
		trackedStream([]int{1, 4, 9}, &closed[0]),
		trackedStream([]int{2, 3, 10}, &closed[1]),
		trackedStream([]int{0, 5, 6}, &closed[2]),
	}

	count := 0
	for range Merge(streams) {
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	for i, c := range closed {
		if !c {
			t.Fatalf("stream %d was not closed after early break", i)
		}
	}
}
```

## Review

The property that generalizes this from a two-stream merge to an N-stream
merge is treating "the set of streams" as a single collection whose cursors
and stop functions are all managed together, rather than by name. Collecting
`stops` into a slice and deferring one loop over all of them, instead of one
`defer` per named stream, is what makes the code correct at any `N` without
having to touch the cleanup logic when a fourth or fifth stream is added.
The other detail worth calling out: pulling exactly one replacement value
from the stream that was just popped -- never from any other stream -- is
what preserves the sortedness invariant; pulling from a different stream
would mean advancing past a value that had not actually been consumed yet.

## Resources

- [`iter.Pull` documentation](https://pkg.go.dev/iter#Pull)
- [`container/heap` documentation](https://pkg.go.dev/container/heap)
- [Knuth, TAOCP Vol. 3: k-way merging](https://www.pearson.com/en-us/subject-catalog/p/art-of-computer-programming-the-volume-3-sorting-and-searching/P200000004148)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-pub-sub-fanout-broadcast.md](22-pub-sub-fanout-broadcast.md) | Next: [24-graceful-drain-ordered-shutdown.md](24-graceful-drain-ordered-shutdown.md)
