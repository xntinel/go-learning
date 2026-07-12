# Exercise 9: Exact top-K heavy hitters with a map plus a heap

When the number of distinct items is small enough to count exactly and you need
exact numbers — offline telemetry aggregation, a nightly report of the top error
codes — the right tool is not a sketch but a frequency map paired with a bounded
min-heap. This module builds that, and it is the deliberate counterpoint to the
Count-Min Sketch from modules 1 and 2: exact counts with O(n) memory, versus
approximate counts with fixed memory. Knowing which to reach for is the point.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
topk/                      independent module: example.com/topk
  go.mod
  topk.go                  Count, TopK; a bounded min-heap (container/heap)
  cmd/
    demo/
      main.go              top error codes in a telemetry batch
  topk_test.go             matches brute-force, deterministic ties, k > distinct
```

- Files: `topk.go`, `cmd/demo/main.go`, `topk_test.go`.
- Implement: an exact frequency `map[string]int` (`Count`) feeding a bounded min-heap (`container/heap`) to extract the top-K most frequent keys in O(n log k) (`TopK`).
- Test: `TopK` matches a brute-force sorted frequency list; ties broken deterministically by key; K larger than the distinct-key count returns all.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/09-topk-heavy-hitters/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/09-topk-heavy-hitters
```

### Why a bounded min-heap, and how ties stay deterministic

Counting is the easy half: a `map[string]int` where each occurrence does
`m[key]++`. The interesting half is extracting the K most frequent without sorting
all N distinct keys (O(N log N)) when K is small. A bounded **min**-heap does it in
O(N log K): push each item; whenever the heap exceeds K, pop the *smallest*. What
survives is the K largest, because every item smaller than the current K-th has
already been evicted. The counter-intuitive choice is that a *min*-heap gives you
the *max*-K — the min-heap's root is the weakest survivor, the first to go when a
stronger contender arrives.

Determinism comes from the tie-break. Two keys with equal counts must have a
stable order, or the output — and its test — depends on map iteration order. The
final order we want is count descending, then key ascending. So the heap's `Less`
treats "smaller" (more evictable) as: lower count, or *equal* count with a *larger*
key. That way, when counts tie at the K boundary, the larger keys are the ones
dropped, leaving the smaller keys — exactly what "key ascending" then requires.
After the heap holds the K survivors, pop them (smallest first) into a slice filled
back-to-front, so index 0 ends up most-frequent.

Contrast with the sketch: this structure stores one map entry per distinct key
(O(N) memory) and returns exact counts; the Count-Min Sketch stores a fixed table
regardless of cardinality but only estimates (and never under-counts). Choose this
when N fits in memory and you need exact answers; choose the sketch when N is too
large to count exactly and an over-estimate is acceptable.

Create `topk.go`:

```go
package topk

import "container/heap"

// Item pairs a key with its exact frequency.
type Item struct {
	Key   string
	Count int
}

// Count tallies exact frequencies of the items.
func Count(items []string) map[string]int {
	m := make(map[string]int, len(items))
	for _, it := range items {
		m[it]++
	}
	return m
}

// minHeap orders Items so the root is the most evictable: lowest count, and among
// equal counts the larger key. heap.Pop removes that root.
type minHeap []Item

func (h minHeap) Len() int { return len(h) }

func (h minHeap) Less(i, j int) bool {
	if h[i].Count != h[j].Count {
		return h[i].Count < h[j].Count
	}
	return h[i].Key > h[j].Key
}

func (h minHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *minHeap) Push(x any) { *h = append(*h, x.(Item)) }

func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// TopK returns the k most frequent keys, most-frequent first, ties broken by key
// ascending. It keeps a bounded heap of size k, so the cost is O(n log k). k <= 0
// returns nil; k larger than the number of distinct keys returns all of them.
func TopK(counts map[string]int, k int) []Item {
	if k <= 0 {
		return nil
	}
	h := &minHeap{}
	heap.Init(h)
	for key, count := range counts {
		heap.Push(h, Item{Key: key, Count: count})
		if h.Len() > k {
			heap.Pop(h) // evict the weakest survivor so far
		}
	}
	out := make([]Item, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(Item)
	}
	return out
}
```

### The runnable demo

The demo aggregates a small batch of error-code events and reports the three most
frequent.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/topk"
)

func main() {
	events := []string{
		"500", "404", "500", "503", "500", "404", "500", "429", "404", "503",
	}
	counts := topk.Count(events)
	for _, it := range topk.TopK(counts, 3) {
		fmt.Printf("%s: %d\n", it.Key, it.Count)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
500: 4
404: 3
503: 2
```

### Tests

The main test cross-checks `TopK` against an independent brute-force (sort every
distinct key, take the first k) for every k from 1 to 5, so the heap logic is
validated against an obviously-correct reference. A dedicated tie test pins the
deterministic key ordering, and a `k > distinct` test confirms it returns all
without padding or panicking.

Create `topk_test.go`:

```go
package topk

import (
	"fmt"
	"slices"
	"sort"
	"testing"
)

// brute is an independent reference top-k for cross-checking.
func brute(counts map[string]int, k int) []Item {
	items := make([]Item, 0, len(counts))
	for key, c := range counts {
		items = append(items, Item{Key: key, Count: c})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Key < items[j].Key
	})
	if k < len(items) {
		items = items[:k]
	}
	return items
}

func TestTopKMatchesBruteForce(t *testing.T) {
	t.Parallel()

	counts := Count([]string{
		"a", "a", "a", "b", "b", "c", "d", "d", "d", "d", "e",
	})
	for k := 1; k <= 5; k++ {
		got := TopK(counts, k)
		want := brute(counts, k)
		if !slices.Equal(got, want) {
			t.Fatalf("TopK(k=%d) = %v, want %v", k, got, want)
		}
	}
}

func TestTiesBrokenByKey(t *testing.T) {
	t.Parallel()

	counts := map[string]int{"x": 2, "y": 2, "z": 2, "w": 1}
	got := TopK(counts, 2)
	want := []Item{{Key: "x", Count: 2}, {Key: "y", Count: 2}}
	if !slices.Equal(got, want) {
		t.Fatalf("TopK = %v, want %v (ties by key ascending)", got, want)
	}
}

func TestKLargerThanDistinctReturnsAll(t *testing.T) {
	t.Parallel()

	counts := map[string]int{"a": 3, "b": 1}
	if got := TopK(counts, 10); len(got) != 2 {
		t.Fatalf("TopK(k=10) returned %d items, want 2", len(got))
	}
}

func TestKZeroReturnsNil(t *testing.T) {
	t.Parallel()

	if got := TopK(map[string]int{"a": 1}, 0); got != nil {
		t.Fatalf("TopK(k=0) = %v, want nil", got)
	}
}

func Example() {
	counts := Count([]string{"go", "go", "rust", "go", "rust", "python"})
	for _, it := range TopK(counts, 2) {
		fmt.Printf("%s=%d\n", it.Key, it.Count)
	}
	// Output:
	// go=3
	// rust=2
}
```

## Review

`TopK` is correct when it agrees with the brute-force reference for every k and
its tie ordering is stable (count descending, key ascending). The subtle mistakes
are using a *max*-heap and evicting the wrong end (you must keep a *min*-heap and
pop the root to shed the weakest of the current top-k), and leaving ties to map
order (non-deterministic output that flakes) — the `Less` tie-break on key fixes
it. Remember the design trade-off this module exists to teach: this is the exact,
O(n)-memory tool; when cardinality outgrows memory you switch to the Count-Min
Sketch from modules 1-2, which bounds memory but only estimates and never
under-counts, so a heavy key's true count is always at least its sketch estimate's
floor. Run `go test -count=1 -race ./...`.

## Resources

- [`container/heap` package](https://pkg.go.dev/container/heap) — the `heap.Interface`, `Init`, `Push`, `Pop`.
- [`sort` package](https://pkg.go.dev/sort) — `sort.Slice` for the brute-force reference.
- [`slices` package](https://pkg.go.dev/slices) — `slices.Equal` for comparing results.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-idempotency-store.md](08-idempotency-store.md) | Next: [10-maphash-comparable-index.md](10-maphash-comparable-index.md)
