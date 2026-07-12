# Exercise 5: K-Way Merge of Sorted Streams with a Heap

A storage engine that keeps many already-sorted segments — log shards, LSM-tree SSTables, time-ordered partitions — needs to read them back as one globally sorted stream without materializing them all in memory. This is the k-way merge: walk every source by one pull cursor each, and always emit the smallest current front. A binary heap keyed on the cursor fronts makes "which source is smallest" an O(log k) question, and `iter.Pull` is what lets the merge hold one live front per source at the same time.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
merge.go             FromSlice, cursor[T], minHeap[T], Merge (iter.Pull + heap)
cmd/
  demo/
    main.go          merge three sorted log-timestamp segments into one timeline
merge_test.go        total-order union, duplicates, empty/no sources, all stops fire
```

- Files: `merge.go`, `cmd/demo/main.go`, `merge_test.go`.
- Implement: `FromSlice[T]`, the unexported `cursor[T]` and `minHeap[T]`, and `Merge[T cmp.Ordered](seqs ...iter.Seq[T]) iter.Seq[T]`.
- Test: `merge_test.go` checks the merged output is the fully sorted union (duplicates kept), the empty and no-source cases, and that every source's producer is stopped on both full drain and early break.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/04-range-over-func-pull-iterators/05-k-way-merge-with-heap/cmd/demo && cd go-solutions/25-iterators-and-modern-go/04-range-over-func-pull-iterators/05-k-way-merge-with-heap
```

### Why this needs k live cursors, and why a heap

A two-way merge advances whichever of two cursors has the smaller front. The k-way generalization is the same idea, but the question "which of the k fronts is smallest" is no longer a single comparison — done naively it is an O(k) scan on every emitted element, giving O(n·k) overall. A min-heap keyed on the cursor fronts answers it in O(log k): the smallest front is always at the root, and after you advance that cursor you push its new front back down the heap. Total cost is O(n·log k), which is the standard external-merge bound.

The structural reason this must be a pull algorithm is the same one the earlier exercises hit, scaled up: the merge has to hold the current front of *every* source simultaneously and choose among them. A push iterator owns its own loop and can only walk itself to the end, so k push iterators cannot be interleaved by an outside decision. `iter.Pull` converts each source into a `next`/`stop` pair, and the merge keeps k of those `next` functions live at once — one per heap entry — pulling exactly one element from a source each time that source wins the root.

Each cursor bundles a source's `next` function with its current front value. The heap orders cursors by that front. The core loop is: peek the root (smallest front), `yield` it, then pull the next value from that same source. If the source still has values, overwrite the root's front and call `heap.Fix(h, 0)` to restore the heap in place; if it is exhausted, `heap.Pop` removes it. The loop ends when the heap is empty, meaning every source has been drained.

The leak discipline is the part that scales dangerously. Every source gets its own `iter.Pull`, and so every source needs its own `stop`. Because all the pulls happen inside one closure, a single `defer stop()` per source — registered as the source is set up — covers every exit path: normal completion, an exhausted source, and a consumer that breaks early. If the consumer abandons the merge after three elements, the `yield` returning `false` triggers `return`, and the stack of deferred `stop`s unwinds *all* k producers. Forgetting even one would leak that producer's goroutine.

Create `merge.go`:

```go
package kwaymerge

import (
	"cmp"
	"container/heap"
	"iter"
)

// FromSlice returns a push iterator that yields each element of values in order.
// For use with Merge the values must be sorted ascending.
func FromSlice[T any](values []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// cursor is one source's live pull state: its next function and current front.
type cursor[T cmp.Ordered] struct {
	next func() (T, bool)
	val  T
}

// minHeap orders cursors by their current front value, smallest at the root.
type minHeap[T cmp.Ordered] []*cursor[T]

func (h minHeap[T]) Len() int { return len(h) }

func (h minHeap[T]) Less(i, j int) bool { return h[i].val < h[j].val }

func (h minHeap[T]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *minHeap[T]) Push(x any) { *h = append(*h, x.(*cursor[T])) }

func (h *minHeap[T]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// Merge consumes any number of ascending-sorted sequences and yields their union
// in one globally sorted order, duplicates preserved. Each source is walked with
// its own iter.Pull cursor; every cursor's stop is deferred, so no producer
// goroutine leaks even when the consumer breaks out early.
func Merge[T cmp.Ordered](seqs ...iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		h := &minHeap[T]{}
		for _, seq := range seqs {
			next, stop := iter.Pull(seq)
			defer stop()
			if v, ok := next(); ok {
				heap.Push(h, &cursor[T]{next: next, val: v})
			}
		}
		for h.Len() > 0 {
			top := (*h)[0]
			if !yield(top.val) {
				return
			}
			if v, ok := top.next(); ok {
				top.val = v
				heap.Fix(h, 0)
			} else {
				heap.Pop(h)
			}
		}
	}
}
```

The output is an ordinary push iterator (`iter.Seq[T]`), so callers consume it with `for v := range Merge(...)`; the heap and the k pull cursors are entirely internal. Note an empty source is never pushed onto the heap — its single initial `next()` returns `ok == false` — but its `stop` is still deferred, so it is released along with the rest. The contract is the same precondition every merge relies on: each input is sorted ascending. Feed it an unsorted source and the output is no longer globally sorted, but nothing panics — the algorithm faithfully merges whatever order it is given.

### The runnable demo

The demo models a storage engine reading three sorted segments of log timestamps and merging them into one ordered timeline. The duplicate timestamp `250` appears in two segments and survives in the output, showing the merge keeps duplicates rather than deduplicating.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/kway-merge"
)

func main() {
	seg1 := kwaymerge.FromSlice([]int{100, 250, 400})
	seg2 := kwaymerge.FromSlice([]int{120, 250, 500})
	seg3 := kwaymerge.FromSlice([]int{110, 300})

	timeline := []int{}
	for ts := range kwaymerge.Merge(seg1, seg2, seg3) {
		timeline = append(timeline, ts)
	}
	fmt.Println("timeline:", timeline)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeline: [100 110 120 250 250 300 400 500]
```

### Tests

`TestMergeTotalOrder` merges three overlapping sorted slices and asserts the exact sorted union, including the duplicate, pinning both the heap ordering and the duplicate-keeping behavior. `TestMergeSingleAndEmpty` covers a lone source, a mix that includes empty sources, and the zero-source call (an immediately empty result). `TestMergeStopsAllSources` uses tracked producers that flip a flag in a deferred cleanup, and asserts every source is stopped both after a full drain and after an early `break` — the no-leak guarantee at k sources.

Create `merge_test.go`:

```go
package kwaymerge

import (
	"iter"
	"reflect"
	"testing"
)

func collect(seq iter.Seq[int]) []int {
	out := []int{}
	for v := range seq {
		out = append(out, v)
	}
	return out
}

// tracked yields values and flips done in a deferred cleanup, so a test can see
// whether the producer was unwound (by exhaustion or by stop).
func tracked(values []int, done *bool) iter.Seq[int] {
	return func(yield func(int) bool) {
		defer func() { *done = true }()
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

func TestMergeTotalOrder(t *testing.T) {
	t.Parallel()

	got := collect(Merge(
		FromSlice([]int{100, 250, 400}),
		FromSlice([]int{120, 250, 500}),
		FromSlice([]int{110, 300}),
	))
	want := []int{100, 110, 120, 250, 250, 300, 400, 500}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}
}

func TestMergeSingleAndEmpty(t *testing.T) {
	t.Parallel()

	if got := collect(Merge(FromSlice([]int{1, 2, 3}))); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("single-source Merge = %v, want [1 2 3]", got)
	}
	mixed := collect(Merge(FromSlice([]int(nil)), FromSlice([]int{2, 4}), FromSlice([]int(nil)), FromSlice([]int{1, 3})))
	if !reflect.DeepEqual(mixed, []int{1, 2, 3, 4}) {
		t.Fatalf("mixed-empty Merge = %v, want [1 2 3 4]", mixed)
	}
	if got := collect(Merge[int]()); len(got) != 0 {
		t.Fatalf("no-source Merge = %v, want empty", got)
	}
}

func TestMergeStopsAllSources(t *testing.T) {
	t.Parallel()

	drained := make([]bool, 3)
	all := func() []iter.Seq[int] {
		return []iter.Seq[int]{
			tracked([]int{1, 4, 7}, &drained[0]),
			tracked([]int{2, 5, 8}, &drained[1]),
			tracked([]int{3, 6, 9}, &drained[2]),
		}
	}

	for range Merge(all()...) {
	}
	for i, d := range drained {
		if !d {
			t.Fatalf("after full drain source %d not stopped", i)
		}
	}

	for i := range drained {
		drained[i] = false
	}
	count := 0
	for range Merge(all()...) {
		count++
		if count == 2 {
			break
		}
	}
	for i, d := range drained {
		if !d {
			t.Fatalf("after early break source %d not stopped (leak)", i)
		}
	}
}
```

## Review

The merge is correct when the heap root is always the global minimum of the live fronts and every emitted element is immediately followed by advancing exactly the source it came from. `heap.Fix(h, 0)` after overwriting the root's `val`, and `heap.Pop` when that source is exhausted, are the two operations that keep the heap an accurate index of the current fronts. The total-order test pins the ordering and the duplicate handling; the single/empty/zero-source test pins the boundaries, including that empty sources are released without ever entering the heap.

The traps are about the heap and the leak. Re-scanning all cursors for the minimum instead of using the heap is correct but O(n·k) — the point of the exercise is the O(log k) root. Calling `heap.Push` with a new value rather than `heap.Fix` after advancing the root grows the heap with stale entries and corrupts the order. The leak trap is the sharp one: every source needs its own deferred `stop`, and the only safe place to register them is at setup inside the closure, so that an early consumer `break` — which makes `yield` return false and triggers `return` — unwinds all k producers. The all-stops test fails loudly if even one source is left running after an early break.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the per-source conversion that gives each cursor its `next` and `stop`.
- [`container/heap`](https://pkg.go.dev/container/heap) — the `heap.Interface`, `Push`/`Pop`/`Fix` operations the merge drives.
- [`cmp.Ordered`](https://pkg.go.dev/cmp#Ordered) — the constraint that lets `Merge` compare fronts of any ordered element type.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — the push/pull iterator model these cursors are built on.

---

Back to [04-peekable-lookahead.md](04-peekable-lookahead.md) | Next: [06-reconcile-diff-with-pull2.md](06-reconcile-diff-with-pull2.md)
