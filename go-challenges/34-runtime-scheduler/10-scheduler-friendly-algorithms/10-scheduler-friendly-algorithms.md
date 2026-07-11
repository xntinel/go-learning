# 10. Scheduler-Friendly Algorithms

Writing algorithms that cooperate with the Go scheduler means keeping goroutines short-lived and bounded, breaking work into chunks the runtime can redistribute, and avoiding patterns that pin a goroutine to one P for long stretches. The two canonical examples are parallel divide-and-conquer (merge sort) and channel-based batch processing. Both patterns create natural scheduling points and keep all P's fed with work.

## Concepts

### Divide and Conquer with Goroutines

A parallel merge sort splits data recursively, spawning two goroutines at each level. Each goroutine is independent and can be scheduled on any available P. The key trade-off is goroutine creation overhead vs. parallelism gain: spawning goroutines for tiny sub-problems costs more than the work itself.

The standard approach is a threshold: below a certain slice length (e.g., 2048 elements), sort sequentially. Above it, recurse in parallel. This bounds goroutine count at O(n/threshold) and keeps each goroutine's lifetime proportional to work done.

### Channel-Based Batching

Sending items one at a time through a channel is expensive -- each send is a scheduling point and may park the sender if the channel is full. Sending in batches of N amortizes the scheduling cost across N items:

- Throughput increases because fewer context switches occur per item.
- Memory grows slightly because batch slices are allocated, but the GC handles this.
- Downstream consumers process whole batches, enabling SIMD or cache-friendly loops.

Batch size is a tuning parameter: too small negates the benefit, too large stalls consumers waiting for a full batch.

### Work Granularity and Scheduling Points

A goroutine that runs without a scheduling point (no channel ops, no syscalls, no function calls at preemption checkpoints) holds its P until it completes or the async preemption signal fires (every 10ms). For most programs, async preemption (added in Go 1.14) is sufficient. For work-stealing to balance load efficiently, each goroutine should complete in well under 10ms -- otherwise idle P's spin waiting while one P holds all the work.

### Failure Modes

- Too many goroutines: spawning one goroutine per element causes excessive GC pressure and scheduling overhead. Use thresholds.
- Too few goroutines: no parallelism. Sequential fallback must still be correct.
- Channel deadlock: `BatchSend` must close the channel after sending all batches; `BatchConsume` ranges over the channel and exits only after close.
- Race on shared state: parallel sort must not share a backing array between goroutines. Each recursive call must operate on a fresh slice.

## Exercises

Module path: `example.com/friendly`. Set up the module:

```go
// go.mod
module example.com/friendly

go 1.26
```

### Exercise 1: Implement the friendly package

Create `friendly.go`:

```go
// friendly.go
package friendly

import (
	"sort"
	"sync"
)

const threshold = 2048

// MergeSort returns a sorted copy of data using parallel merge sort.
// Slices larger than threshold are sorted in parallel; smaller slices
// are sorted sequentially.
func MergeSort(data []int) []int {
	if len(data) <= 1 {
		if data == nil {
			return nil
		}
		cp := make([]int, len(data))
		copy(cp, data)
		return cp
	}
	if len(data) <= threshold {
		cp := make([]int, len(data))
		copy(cp, data)
		sort.Ints(cp)
		return cp
	}
	mid := len(data) / 2
	var left, right []int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		left = MergeSort(data[:mid])
	}()
	go func() {
		defer wg.Done()
		right = MergeSort(data[mid:])
	}()
	wg.Wait()
	return merge(left, right)
}

// merge combines two sorted slices into one sorted slice.
func merge(a, b []int) []int {
	result := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// BatchSend sends items in batches of batchSize to out, then closes out.
func BatchSend[T any](items []T, batchSize int, out chan<- []T) {
	if batchSize <= 0 {
		batchSize = 1
	}
	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := make([]T, end-i)
		copy(batch, items[i:end])
		out <- batch
	}
	close(out)
}

// BatchConsume reads batches from in and calls process for each item.
func BatchConsume[T any](in <-chan []T, process func(T)) {
	for batch := range in {
		for _, item := range batch {
			process(item)
		}
	}
}
```

### Exercise 2: Write the test file

Create `friendly_test.go`:

```go
// friendly_test.go
package friendly

import (
	"sort"
	"testing"
)

func TestMergeSortEmpty(t *testing.T) {
	t.Parallel()
	got := MergeSort(nil)
	if len(got) != 0 {
		t.Errorf("MergeSort(nil) = %v, want empty", got)
	}
}

func TestMergeSortSingle(t *testing.T) {
	t.Parallel()
	got := MergeSort([]int{42})
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("MergeSort([42]) = %v, want [42]", got)
	}
}

func TestMergeSortSorted(t *testing.T) {
	t.Parallel()
	input := []int{5, 2, 8, 1, 9, 3, 7, 4, 6, 0}
	got := MergeSort(input)
	if len(got) != len(input) {
		t.Fatalf("MergeSort returned %d elements, want %d", len(got), len(input))
	}
	want := make([]int, len(input))
	copy(want, input)
	sort.Ints(want)
	for i, v := range got {
		if v != want[i] {
			t.Errorf("result[%d] = %d, want %d", i, v, want[i])
		}
	}
}

func TestMergeSortLarge(t *testing.T) {
	t.Parallel()
	const n = 10000
	data := make([]int, n)
	for i := range data {
		data[i] = n - i
	}
	got := MergeSort(data)
	if len(got) != n {
		t.Fatalf("MergeSort returned %d elements, want %d", len(got), n)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("result not sorted at index %d: %d > %d", i, got[i-1], got[i])
		}
	}
}

func TestBatchSendAndConsume(t *testing.T) {
	t.Parallel()
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	out := make(chan []int, 20)
	go BatchSend(items, 10, out)

	var received []int
	BatchConsume(out, func(v int) {
		received = append(received, v)
	})
	if len(received) != 100 {
		t.Errorf("received %d items, want 100", len(received))
	}
}
```

### Exercise 3: Example and demo

Create `example_test.go`:

```go
// example_test.go
package friendly_test

import (
	"example.com/friendly"
	"fmt"
)

func ExampleMergeSort() {
	data := []int{5, 2, 8, 1, 9, 3}
	sorted := friendly.MergeSort(data)
	fmt.Println(sorted)
	// Output:
	// [1 2 3 5 8 9]
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"example.com/friendly"
	"fmt"
	"math/rand"
)

func main() {
	const n = 100000
	data := make([]int, n)
	for i := range data {
		data[i] = rand.Intn(n)
	}
	sorted := friendly.MergeSort(data)
	fmt.Printf("sorted %d elements\n", n)
	fmt.Printf("first 10: %v\n", sorted[:10])
	fmt.Printf("last 10:  %v\n", sorted[n-10:])
}
```

## Common Mistakes

**Wrong**: Spawning a goroutine for every recursive call regardless of slice size.

What happens: For a 100000-element slice, this creates O(n) goroutines -- roughly 100000 goroutines, each with a tiny slice. Goroutine creation (even at ~1us each) dominates over the actual sort work. Memory for goroutine stacks far exceeds the data size.

**Fix**: Use a threshold (e.g., 2048 elements). Below the threshold, sort sequentially with `sort.Ints`. Above it, recurse in parallel.

---

**Wrong**: Closing `out` inside `BatchConsume` instead of inside `BatchSend`.

What happens: `BatchConsume` ranges over the channel. If the channel is never closed, the range loop blocks forever and the goroutine leaks.

**Fix**: `BatchSend` must `close(out)` after sending all batches. `BatchConsume` then exits naturally when the range loop exhausts all remaining batches.

---

**Wrong**: Sorting `data[:mid]` and `data[mid:]` in place (modifying the input slice) and relying on the caller not using the original slice.

What happens: The caller's slice is mutated, which breaks the principle of least surprise for a function named `MergeSort`. Under `-race`, concurrent writes to overlapping slices may also trigger data-race detection.

**Fix**: Always copy the slice before sorting: `cp := make([]int, len(data)); copy(cp, data)`. Return the sorted copy.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo:

```bash
go run ./cmd/demo
```

## Summary

- Use a threshold for parallel divide-and-conquer: below threshold, sort sequentially; above it, spawn goroutines.
- Each goroutine in merge sort is independent -- natural fit for work stealing across all P's.
- `BatchSend` and `BatchConsume` amortize scheduling overhead by grouping items into slices.
- `BatchSend` owns the channel close; `BatchConsume` exits naturally via range.
- Return sorted copies, not in-place modifications, to avoid surprises and data races.
- Goroutine lifetime should be proportional to work: avoid goroutines that do microseconds of work but take milliseconds to create.

## What's Next

Next: [Tri-Color Mark and Sweep](../../35-runtime-garbage-collector/01-tri-color-mark-and-sweep/01-tri-color-mark-and-sweep.md).

## Resources

- [sort package](https://pkg.go.dev/sort)
- [sync package -- WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go blog: Concurrency patterns](https://go.dev/blog/pipelines)
- [runtime package -- GOMAXPROCS](https://pkg.go.dev/runtime#GOMAXPROCS)
- [Scheduling in Go Part I: OS Scheduler (Ardan Labs)](https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part1.html)
