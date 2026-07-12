# 5. Work-Stealing Deque

The Chase-Lev work-stealing deque is the core primitive behind Go's own scheduler, Java's ForkJoinPool, and Tokio's thread-per-core executor. The hard part is not the data structure itself — it is making the single-producer / multi-consumer separation work correctly with only a handful of atomics: one CAS on `top` separates the thief from the owner; getting the memory ordering right without over-synchronizing is the exercise.

```text
worksteal/
  go.mod
  deque.go
  deque_test.go
  cmd/demo/main.go
```

## Concepts

### The Asymmetric Producer-Consumer Split

A work-stealing deque is a double-ended queue with an asymmetric ownership rule:

- **Owner** (one goroutine): pushes new tasks to the bottom and pops them back from the bottom (LIFO — maximizes cache reuse of recently pushed data).
- **Thieves** (any number of goroutines): steal from the top (FIFO — steals older, likely larger tasks, balancing load).

Because only one goroutine pushes and pops from the bottom, the bottom counter is modified without CAS. Only the top counter requires CAS, because multiple thieves compete for the same slot.

### The Chase-Lev Algorithm (2005)

The algorithm stores three shared values:

- `bottom int64` — index of the next empty slot from the owner's side; only the owner writes it.
- `top int64` — index of the oldest occupied slot; only incremented, by CAS from thieves (and occasionally by the owner resolving a race).
- `buf *circularBuf[T]` — an atomic pointer to a growable circular buffer indexed by `i & mask` where `mask = capacity - 1`.

**Push** (owner only):

1. If `bottom - top > mask` (buffer is full), grow the buffer and atomically swap the pointer.
2. Write the value at `buf.slots[bottom & mask]`.
3. Atomically store `bottom + 1` to make the slot visible to thieves.

**Pop** (owner only):

1. Decrement `bottom` to tentatively claim the slot.
2. Load `top`. If `bottom > top`, the slot is uncontested: return it.
3. If `bottom < top`, the deque was already empty; restore `bottom = top` and return false.
4. If `bottom == top` (exactly one item), race with thieves: CAS `top` from `t` to `t+1`. Winner takes the item; loser restores `bottom = top + 1`.

**Steal** (any goroutine):

1. Load `top`, then `bottom`. If `top >= bottom`, return false.
2. Read the value at `buf.slots[top & mask]`.
3. CAS `top` from `t` to `t+1`. If the CAS fails, another goroutine took the slot; return false.

### Why Go's Memory Model Makes This Safe

Go's `sync/atomic` operations are sequentially consistent: they observe a single total order across all goroutines, equivalent to Java's `volatile` and C++'s `memory_order_seq_cst`. The Chase-Lev algorithm relies on two ordering guarantees:

- The owner's `bottom.Store(b+1)` in Push synchronizes-before any thief's `bottom.Load()` that observes the new value. The thief therefore also observes the value written at `buf.slots[b & mask]` before that store.
- The owner's `bottom.Store(b-1)` in Pop synchronizes-before the thief's `bottom.Load()`, so the thief sees the reduced claim area.

When the buffer grows, the owner stores the new pointer atomically BEFORE updating `bottom`. Any thief observing the new `bottom` value also observes the new buffer. Thieves that observed the old `bottom` read from the old buffer, which still holds valid data (the grow copies all existing items before swapping the pointer, and Go's GC keeps the old buffer alive as long as any goroutine holds a reference to it).

### Dynamic Resizing

The circular buffer uses `slots []T` of power-of-two length. The index formula `i & mask` wraps without a modulo. When `bottom - top > mask` (the buffer is full), `grow` allocates a double-size buffer, copies all live items at their same logical indices, and the owner atomically swaps the pointer. Slot 7 in the old buffer (stored at `7 & old_mask`) is copied to slot 7 in the new buffer (stored at `7 & new_mask`); the masking changes but the logical index stays the same, so `get` and `put` are consistent before and after the swap.

### When to Use This

Use a Chase-Lev deque when:

- You have a fork-join workload: tasks spawn sub-tasks, which spawn more sub-tasks.
- You want cache-local task execution (owner pops in LIFO order).
- You need load balancing without a central queue (no single bottleneck mutex).

Do not reach for a work-stealing deque for a simple producer-consumer pipeline — a `sync.Mutex`-protected slice or a buffered channel is far simpler and correct for that case.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/46-capstone-concurrency-deep-dive/05-work-stealing-deque/05-work-stealing-deque/cmd/demo
cd go-solutions/46-capstone-concurrency-deep-dive/05-work-stealing-deque/05-work-stealing-deque
```

This is a library; verify it with `go test`.

### Exercise 1: The Circular Buffer and Deque Type

Create `deque.go`:

```go
package worksteal

import (
	"sync/atomic"
)

// circularBuf is a fixed-size circular buffer indexed by i & mask.
// Capacity is always a power of two; mask = capacity - 1.
type circularBuf[T any] struct {
	slots []T
	mask  int64
}

func newCircularBuf[T any](cap int64) *circularBuf[T] {
	return &circularBuf[T]{
		slots: make([]T, cap),
		mask:  cap - 1,
	}
}

func (c *circularBuf[T]) get(i int64) T    { return c.slots[i&c.mask] }
func (c *circularBuf[T]) put(i int64, v T) { c.slots[i&c.mask] = v }

// grow returns a new buffer of double the capacity with all items from
// [top, bottom) copied at their same logical indices.
func (c *circularBuf[T]) grow(bottom, top int64) *circularBuf[T] {
	next := newCircularBuf[T]((c.mask + 1) * 2)
	for i := top; i < bottom; i++ {
		next.put(i, c.get(i))
	}
	return next
}

// nextPow2 returns the smallest power of two >= n, minimum 2.
func nextPow2(n int64) int64 {
	if n < 2 {
		return 2
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}

// Deque is a Chase-Lev work-stealing deque.
//
// Ownership rule: exactly one goroutine (the owner) may call Push and Pop.
// Any number of goroutines may call Steal concurrently with each other
// and with the owner's Push/Pop calls.
type Deque[T any] struct {
	bottom atomic.Int64
	top    atomic.Int64
	buf    atomic.Pointer[circularBuf[T]]
}

// NewDeque creates a Deque with an initial capacity rounded up to the next
// power of two (minimum 2).
func NewDeque[T any](initialCap int64) *Deque[T] {
	d := &Deque[T]{}
	d.buf.Store(newCircularBuf[T](nextPow2(initialCap)))
	return d
}

// Len returns an instantaneous snapshot of the number of items.
// Under concurrent access the result is an estimate only.
func (d *Deque[T]) Len() int64 {
	b := d.bottom.Load()
	t := d.top.Load()
	if delta := b - t; delta > 0 {
		return delta
	}
	return 0
}
```

`bottom` and `top` are monotonically increasing int64 counters; the slot is `i & mask`. The counters never wrap for any realistic workload (int64 overflows after roughly 9.2 × 10^18 operations).

### Exercise 2: Push, Pop, and Steal

Append to `deque.go`:

```go
// Push adds v to the bottom of the deque.
// Must be called only by the owner goroutine.
func (d *Deque[T]) Push(v T) {
	b := d.bottom.Load()
	t := d.top.Load()
	buf := d.buf.Load()

	if b-t > buf.mask {
		// Buffer is full: grow, copy all existing items, swap the pointer.
		// The new pointer is visible to thieves before bottom is updated,
		// so any thief observing the new bottom also sees the new buffer.
		buf = buf.grow(b, t)
		d.buf.Store(buf)
	}
	buf.put(b, v)
	// Store bottom last: this is the publication fence that makes the new
	// slot visible to thieves.
	d.bottom.Store(b + 1)
}

// Pop removes and returns the value at the bottom of the deque.
// Must be called only by the owner goroutine.
// Returns (value, true) when an item is available; (zero, false) when empty.
func (d *Deque[T]) Pop() (T, bool) {
	b := d.bottom.Load() - 1
	buf := d.buf.Load()
	// Decrement bottom first to claim the slot from thieves.
	d.bottom.Store(b)
	t := d.top.Load()

	var zero T
	switch {
	case b > t:
		// Uncontested: more than one item remains.
		return buf.get(b), true
	case b < t:
		// The deque was empty (a thief concurrently drained it).
		// Restore bottom to top so the deque reads as empty.
		d.bottom.Store(t)
		return zero, false
	default:
		// b == t: exactly one item; race with concurrent Steal calls.
		v := buf.get(b)
		if d.top.CompareAndSwap(t, t+1) {
			// We won: advance bottom past the contested slot.
			d.bottom.Store(t + 1)
			return v, true
		}
		// A thief won the CAS; restore bottom.
		d.bottom.Store(t + 1)
		return zero, false
	}
}

// Steal removes and returns the value at the top of the deque.
// Safe to call from any goroutine concurrently with the owner's Push/Pop
// and with other concurrent Steal calls.
// Returns (value, true) when an item is successfully stolen; (zero, false)
// when the deque is empty or the CAS loses to another stealer.
func (d *Deque[T]) Steal() (T, bool) {
	t := d.top.Load()
	// Load bottom after top to avoid claiming an item that the owner
	// has already re-claimed via a concurrent Pop.
	b := d.bottom.Load()

	var zero T
	if t >= b {
		return zero, false
	}
	v := d.buf.Load().get(t)
	// CAS top: only one goroutine increments top per slot.
	if !d.top.CompareAndSwap(t, t+1) {
		return zero, false
	}
	return v, true
}
```

### Exercise 3: Tests

Create `deque_test.go`:

```go
package worksteal

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDequePopEmpty(t *testing.T) {
	t.Parallel()
	d := NewDeque[int](8)
	if _, ok := d.Pop(); ok {
		t.Fatal("Pop on empty deque returned true")
	}
}

func TestDequeStealEmpty(t *testing.T) {
	t.Parallel()
	d := NewDeque[int](8)
	if _, ok := d.Steal(); ok {
		t.Fatal("Steal on empty deque returned true")
	}
}

func TestDequePushPopLIFO(t *testing.T) {
	t.Parallel()

	d := NewDeque[int](8)
	for i := range 5 {
		d.Push(i)
	}
	for want := 4; want >= 0; want-- {
		v, ok := d.Pop()
		if !ok {
			t.Fatalf("Pop() ok=false, want v=%d", want)
		}
		if v != want {
			t.Fatalf("Pop() = %d, want %d (LIFO order)", v, want)
		}
	}
	if _, ok := d.Pop(); ok {
		t.Fatal("Pop on now-empty deque returned true")
	}
}

func TestDequeStealFIFO(t *testing.T) {
	t.Parallel()

	d := NewDeque[int](8)
	for i := range 5 {
		d.Push(i)
	}
	for want := 0; want < 5; want++ {
		v, ok := d.Steal()
		if !ok {
			t.Fatalf("Steal() ok=false, want v=%d", want)
		}
		if v != want {
			t.Fatalf("Steal() = %d, want %d (FIFO order)", v, want)
		}
	}
}

func TestDequeGrows(t *testing.T) {
	t.Parallel()

	d := NewDeque[int](4)
	const n = 1000
	for i := range n {
		d.Push(i)
	}
	if got := d.Len(); got != n {
		t.Fatalf("Len() = %d, want %d", got, n)
	}
	for want := n - 1; want >= 0; want-- {
		v, ok := d.Pop()
		if !ok {
			t.Fatalf("Pop() ok=false at want=%d", want)
		}
		if v != want {
			t.Fatalf("Pop() = %d, want %d", v, want)
		}
	}
}

func TestDequeOwnerAndThieves(t *testing.T) {
	t.Parallel()

	const total = 10_000
	const numThieves = 4

	d := NewDeque[int](64)
	for i := range total {
		d.Push(i)
	}

	var remaining atomic.Int64
	remaining.Store(total)

	seen := make([]atomic.Int32, total)
	var wg sync.WaitGroup

	mark := func(v int) {
		if !seen[v].CompareAndSwap(0, 1) {
			t.Errorf("duplicate item %d", v)
		}
		remaining.Add(-1)
	}

	for range numThieves {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for remaining.Load() > 0 {
				v, ok := d.Steal()
				if ok {
					mark(v)
				}
			}
		}()
	}

	for remaining.Load() > 0 {
		v, ok := d.Pop()
		if ok {
			mark(v)
		}
	}

	wg.Wait()
}

func ExampleDeque() {
	d := NewDeque[int](8)
	d.Push(10)
	d.Push(20)
	d.Push(30)
	// Pop returns the most recently pushed item (LIFO, from the bottom).
	v, _ := d.Pop()
	fmt.Println(v)
	// Steal returns the oldest item (FIFO, from the top).
	v, _ = d.Steal()
	fmt.Println(v)
	// Output:
	// 30
	// 10
}
```

Your turn: add `TestDequePopThenSteal` — push one item, pop it (Pop must return true), then call Steal (must return false because the deque is now empty). This pins the invariant that a Pop and a Steal cannot both succeed on a deque that contained one item.

### Exercise 4: cmd/demo

Create `cmd/demo/main.go` — a parallel sum using one owner and multiple thieves to demonstrate the asymmetric access pattern:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"example.com/worksteal"
)

func main() {
	const n = 100_000

	// Build the dataset: integers 1..n.
	data := make([]int64, n)
	for i := range data {
		data[i] = int64(i + 1)
	}
	wantSum := int64(n) * (int64(n) + 1) / 2

	// Fill one deque with all indices; the owner goroutine pops them
	// while thieves steal from the other end.
	d := worksteal.NewDeque[int](128)
	for i := range n {
		d.Push(i)
	}

	workers := runtime.NumCPU()
	var total atomic.Int64
	var remaining atomic.Int64
	remaining.Store(n)
	var wg sync.WaitGroup

	// Thieves steal from the top.
	for range workers - 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for remaining.Load() > 0 {
				idx, ok := d.Steal()
				if !ok {
					continue
				}
				total.Add(data[idx])
				remaining.Add(-1)
			}
		}()
	}

	// Owner pops from the bottom.
	for remaining.Load() > 0 {
		idx, ok := d.Pop()
		if !ok {
			continue
		}
		total.Add(data[idx])
		remaining.Add(-1)
	}

	wg.Wait()

	fmt.Printf("workers:  %d\n", workers)
	fmt.Printf("expected: %d\n", wantSum)
	fmt.Printf("got:      %d\n", total.Load())
	fmt.Printf("correct:  %v\n", total.Load() == wantSum)
}
```

Run it:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Calling Pop From Multiple Goroutines

Wrong: two goroutines both call `d.Pop()` on the same deque.

What happens: `Pop` modifies `bottom` without CAS (it is the owner's exclusive counter). Two goroutines decrement it simultaneously, producing a data race that the race detector flags. Both goroutines may return the same item or neither may.

Fix: one goroutine owns the deque. Other goroutines call `Steal`. If you need a multi-consumer queue, use a `sync.Mutex`-protected slice or a buffered channel.

### Using `Add` Instead of `CompareAndSwap` in Steal

Wrong:

```go
func (d *Deque[T]) Steal() (T, bool) {
	t := d.top.Load()
	if t >= d.bottom.Load() {
		var zero T
		return zero, false
	}
	v := d.buf.Load().get(t)
	d.top.Add(1) // wrong: not atomic with the preceding read
	return v, true
}
```

What happens: two thieves both read the same `t`, both call `Add(1)`, and both return the same item `v`. One item is returned twice; the adjacent item is skipped.

Fix: use `CompareAndSwap(t, t+1)`. Only the goroutine that wins the CAS may return the item.

### Using `>=` Instead of `>` in the Grow Check

Wrong:

```go
if b-t >= buf.mask { // triggers one item too early
```

What happens: with `mask = 3` (capacity 4), the deque grows when it has 3 items instead of 4. The buffer is always one slot under-utilized, and the grow condition no longer matches the paper's invariant `bottom - top > size - 1`.

Fix:

```go
if b-t > buf.mask { // grow only when the buffer is truly full
```

### Calling Steal From the Owner for LIFO Work

Wrong: the owner calls `Steal` on its own deque expecting the same LIFO ordering as `Pop`.

What happens: `Steal` takes from the top (FIFO order), not the bottom. The owner processes the oldest tasks first, destroying temporal locality.

Fix: the owner always uses `Pop`. Only non-owner goroutines call `Steal`.

## Verification

From `~/go-exercises/worksteal`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All four compile/test commands must be clean. The `-race` flag is mandatory: `TestDequeOwnerAndThieves` runs one owner goroutine (Pop) and four thief goroutines (Steal) concurrently; the race detector instruments every memory access and will flag any unsynchronized read-write pair.

## Summary

- The Chase-Lev deque uses three atomics: `bottom` (owner-only write), `top` (CAS by thieves and owner), and `buf` (atomic pointer, swapped on grow).
- The ownership asymmetry is the key invariant: `Push` and `Pop` are single-goroutine; `Steal` is multi-goroutine. Violating this is a data race.
- `Pop` uses CAS on `top` only for the single-item edge case (`bottom == top`), avoiding unnecessary synchronization in the common path.
- Go's sequentially consistent atomics provide the cross-goroutine visibility guarantees: a `Store` synchronizes-before any `Load` that observes its value.
- Dynamic resizing doubles the buffer, copies items at their logical indices, and atomically swaps the pointer before updating `bottom`, so thieves always read from a consistent buffer.

## What's Next

Next: [Software Transactional Memory](../06-software-transactional-memory/06-software-transactional-memory.md).

## Resources

- David Chase and Yossi Lev, "Dynamic Circular Work-Stealing Deque" (2005): https://citeseerx.ist.psu.edu/document?repid=rep1&type=pdf&doi=1f8944f4adb2bc0c96be06ddb1a6a7bcb0e59ba7
- Go memory model (sequentially consistent atomics): https://go.dev/ref/mem
- `sync/atomic` package reference: https://pkg.go.dev/sync/atomic
- Go scheduler internals (uses work-stealing): https://go.dev/src/runtime/HACKING.md
- "The Art of Multiprocessor Programming" (Herlihy and Shavit, 2012), Chapter 16: canonical coverage of lock-free work-stealing
