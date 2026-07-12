# 1. Lock-Free MPMC Queue

A lock-free multi-producer multi-consumer (MPMC) bounded queue is one of the hardest artifacts to get right in Go. It requires reasoning at the hardware memory-model level: you must choose the exact sequence of atomic loads, CAS operations, and stores that remain correct across all interleavings of goroutines across all cores, without any mutex to serialize them. The reward is a data structure with no lock contention, no convoy effect, and throughput that scales linearly with core count until the memory subsystem saturates.

This lesson implements Dmitry Vyukov's bounded MPMC queue algorithm (2008) in Go using `sync/atomic`, generics, and cache-line padding. The result is a production-grade `Queue[T]` type that you can benchmark against Go channels.

```text
mpmc/
  go.mod
  queue.go
  queue_test.go
  cmd/demo/main.go
```

## Concepts

### The ABA Problem And Why Mutex-Free Does Not Mean Trivially Correct

In a mutex-protected queue, the head or tail index is modified only while the lock is held, so no other goroutine observes a partial update. Without a mutex, two goroutines can simultaneously read the same index value, both decide the slot is available, and both try to use it. The naive fix — "check, then act" — is not atomic: between the check and the act, another goroutine can change the world.

The ABA problem is the classic concrete failure: goroutine A reads index 0, is preempted; goroutine B enqueues and dequeues many items so index 0 cycles back to "available"; goroutine A wakes, sees index 0 still "available", and proceeds — but the slot has been recycled. The stale read corrupts data.

### Per-Slot Sequence Numbers: Vyukov's Insight

Vyukov's algorithm attaches a *generation counter* (`seq`) to each slot. The counter is a monotonically increasing `uint64` (never decreasing, never repeating within the lifetime of the process). Each slot's sequence encodes which lap of the ring it is valid for:

- `seq == pos`: the slot is free; a producer claiming `pos` may write here.
- `seq == pos + 1`: a producer has written; a consumer claiming `pos` may read here.
- `seq == pos + capacity`: the slot has been consumed and is free for the next producer at `pos + capacity`.

The difference `int64(seq) - int64(pos)` tells a goroutine whether to proceed, to back off (the queue is full or empty), or to retry (a concurrent goroutine is in the middle of an operation):

| `diff` for Enqueue (`seq - pos`) | Meaning |
|---|---|
| `== 0` | Slot is free; attempt CAS to claim `pos`. |
| `< 0` | Slot is occupied from a previous lap; queue is full. |
| `> 0` | Another producer already claimed this slot; reload and retry. |

| `diff` for Dequeue (`seq - (pos+1)`) | Meaning |
|---|---|
| `== 0` | Slot is filled; attempt CAS to claim `pos`. |
| `< 0` | Slot not yet written; queue is empty. |
| `> 0` | Another consumer already claimed this slot; reload and retry. |

Because `seq` is a monotonically increasing counter, the ABA problem is eliminated: a recycled slot always has a higher sequence value than the old one, and the signed-difference test correctly distinguishes them.

### Compare-And-Swap As A Claim Mechanism

`CompareAndSwap(old, new)` atomically: reads the current value; if it equals `old`, stores `new` and returns true; otherwise returns false. In the MPMC queue, CAS is used solely to "claim" an index position. Only one of N racing goroutines will CAS the index from `pos` to `pos+1`; the others see `false` and retry. The winning goroutine then writes the value and updates the slot's `seq`, using a plain `Store` because the CAS already established exclusive ownership.

This claim-then-write ordering is the key: the CAS creates a happens-before edge between the claimant and any subsequent observer of `seq`. Go's memory model (go.dev/ref/mem, section "Atomic Values") guarantees that an atomic Store by goroutine A is observed by any goroutine that later reads the same location with a Load — so consumers are certain to see the value written after the sequence-number store.

### False Sharing And Cache-Line Padding

The enqueue position and the dequeue position are modified by different goroutines (producers vs. consumers). If they sit on the same 64-byte cache line, every write to one invalidates the other in every CPU's L1 cache, causing a "false sharing" penalty that serializes otherwise independent operations.

The fix is to separate them by at least one cache line:

```go
type Queue[T any] struct {
	enqueuePos atomic.Uint64
	_          [56]byte // pad to 64-byte cache line
	dequeuePos atomic.Uint64
	_          [56]byte // nolint:unused
	// ...
}
```

`atomic.Uint64` is 8 bytes; 56 bytes of padding makes each field occupy exactly one cache line. This is measurable: on a 16-core machine the padding can double throughput at high contention.

Per-slot false sharing is harder to eliminate generically because the slot holds a `T` whose size is unknown at compile time. For hot paths, a concrete `Queue[uint64]` where each slot is 16 bytes benefits from a fixed-size padded wrapper.

### Capacity Must Be A Power Of Two

The ring-buffer index wrap uses bit-AND: `pos & mask` where `mask = capacity - 1`. This is equivalent to `pos % capacity` only when capacity is a power of two, and is a single instruction instead of a division. The constructor enforces the invariant with a panic rather than silently rounding up, so callers catch the mistake immediately.

### Linearizability And "Non-Blocking" Semantics

The queue is *linearizable*: each Enqueue and Dequeue appears to take effect atomically at some point between its invocation and return (the point of the successful CAS). It is *non-blocking* — neither Enqueue nor Dequeue ever blocks waiting for another goroutine to complete — but it is not *wait-free*: in theory, a single goroutine can retry its CAS indefinitely if it is always preempted at the wrong moment. In practice, CAS contention is extremely brief (microseconds), and `TryEnqueueTimeout` provides a bounded wait for full-queue backpressure.

## Exercises

This is a library. You verify it with `go test`, not by running a `main`.

### Exercise 1: The Queue Type And Its Constructor

Create `queue.go`:

```go
// Package mpmc implements a bounded, lock-free multi-producer multi-consumer
// queue using per-slot sequence numbers. The algorithm is due to Dmitry Vyukov
// (https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue).
package mpmc

import (
	"runtime"
	"sync/atomic"
	"time"
)

// slot is one entry in the ring buffer. seq is a generation counter: its
// value tells producers and consumers whether the slot is available for
// writing or reading at a given lap of the ring.
type slot[T any] struct {
	seq atomic.Uint64
	val T
}

// Queue is a bounded, lock-free MPMC queue.
//
// Zero value is not usable; create with New.
// All exported methods are safe to call from multiple goroutines concurrently.
type Queue[T any] struct {
	// enqueuePos and dequeuePos are in separate cache lines to avoid false
	// sharing. Each atomic.Uint64 is 8 bytes; 56 bytes of padding fills the
	// 64-byte cache line.
	enqueuePos atomic.Uint64
	_          [56]byte
	dequeuePos atomic.Uint64
	_          [56]byte // nolint:unused

	mask  uint64    // capacity - 1; used for fast modulo by bit-AND
	slots []slot[T] // ring buffer; length == capacity
}

// New returns a Queue with the given capacity.
// capacity must be a power of two and at least 2; New panics otherwise.
func New[T any](capacity uint64) *Queue[T] {
	if capacity < 2 || capacity&(capacity-1) != 0 {
		panic("mpmc: capacity must be a power of two and at least 2")
	}
	q := &Queue[T]{
		mask:  capacity - 1,
		slots: make([]slot[T], capacity),
	}
	// Initialize each slot's sequence to its index. This marks every slot as
	// writable for the first lap (enqueuePos == 0 means slot 0 is next, and
	// slot[0].seq == 0 signals "ready to write").
	for i := uint64(0); i < capacity; i++ {
		q.slots[i].seq.Store(i)
	}
	return q
}

// Cap returns the queue's fixed capacity.
func (q *Queue[T]) Cap() int {
	return int(q.mask + 1)
}

// Len returns an approximate item count. Because producers and consumers
// advance their indices concurrently, the result may be stale by the time
// it is read.
func (q *Queue[T]) Len() int {
	head := q.dequeuePos.Load()
	tail := q.enqueuePos.Load()
	if tail >= head {
		n := int(tail - head)
		cap := int(q.mask + 1)
		if n > cap {
			return cap
		}
		return n
	}
	return 0
}
```

The two padding fields (`[56]byte`) push `enqueuePos` and `dequeuePos` onto separate cache lines. `mask` and `slots` are read-only after construction and can share a cache line with `dequeuePos` without harm.

### Exercise 2: Enqueue And Dequeue

Add the two core operations to `queue.go`:

```go
// Enqueue tries to add val to the queue. It returns true on success and
// false if the queue is full. Enqueue is non-blocking and lock-free.
func (q *Queue[T]) Enqueue(val T) bool {
	for {
		pos := q.enqueuePos.Load()
		cell := &q.slots[pos&q.mask]
		seq := cell.seq.Load()
		diff := int64(seq) - int64(pos)

		switch {
		case diff == 0:
			// seq == pos: this slot is free for writing. Claim it.
			if q.enqueuePos.CompareAndSwap(pos, pos+1) {
				cell.val = val
				// Signal consumers: seq = pos+1 means "readable at pos".
				cell.seq.Store(pos + 1)
				return true
			}
			// Another producer claimed pos; re-read and retry.
		case diff < 0:
			// seq < pos: the slot is still occupied from a previous lap.
			// The queue is full from our perspective.
			return false
		default:
			// diff > 0: a concurrent producer is advancing past us.
			// Re-read enqueuePos and try again.
		}
	}
}

// Dequeue removes and returns the oldest value in the queue.
// It returns (value, true) on success and (zero, false) if the queue is empty.
// Dequeue is non-blocking and lock-free.
func (q *Queue[T]) Dequeue() (T, bool) {
	var zero T
	for {
		pos := q.dequeuePos.Load()
		cell := &q.slots[pos&q.mask]
		seq := cell.seq.Load()
		diff := int64(seq) - int64(pos+1)

		switch {
		case diff == 0:
			// seq == pos+1: this slot has been written and is ready to read.
			// Claim it.
			if q.dequeuePos.CompareAndSwap(pos, pos+1) {
				val := cell.val
				// Recycle the slot: seq = pos + capacity signals that it is
				// writable again on the next lap of the ring.
				cell.seq.Store(pos + q.mask + 1)
				return val, true
			}
			// Another consumer claimed pos; re-read and retry.
		case diff < 0:
			// seq < pos+1: the slot has not been written yet.
			// The queue is empty from our perspective.
			return zero, false
		default:
			// diff > 0: a concurrent consumer is advancing past us.
			// Re-read dequeuePos and try again.
		}
	}
}

// TryEnqueueTimeout retries Enqueue in a spin loop with exponential backoff
// until success or the timeout elapses. It returns false if the deadline is
// exceeded without a successful enqueue.
func (q *Queue[T]) TryEnqueueTimeout(val T, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	backoff := 1
	for {
		if q.Enqueue(val) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		// Exponential backoff via scheduler yields to avoid burning CPU while
		// waiting for consumers to drain a full queue.
		for i := 0; i < backoff; i++ {
			runtime.Gosched()
		}
		if backoff < 64 {
			backoff <<= 1
		}
	}
}
```

The recycling formula `pos + q.mask + 1` equals `pos + capacity`. When `enqueuePos` later reaches `pos + capacity`, the slot's sequence number will again equal `enqueuePos`, signalling "ready to write" for the next lap.

### Exercise 3: Test the Contract

Create `queue_test.go`. Tests are the verification; there is no eyeballed `main`:

```go
package mpmc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewPanicsBadCapacity(t *testing.T) {
	t.Parallel()

	for _, cap := range []uint64{0, 1, 3, 5, 6, 7, 9} {
		c := cap
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("New(%d) did not panic", c)
				}
			}()
			New[int](c)
		}()
	}
}

func TestEnqueueReturnsFalseWhenFull(t *testing.T) {
	t.Parallel()

	q := New[int](2)
	if !q.Enqueue(1) {
		t.Fatal("first Enqueue failed")
	}
	if !q.Enqueue(2) {
		t.Fatal("second Enqueue failed")
	}
	if q.Enqueue(3) {
		t.Fatal("Enqueue on full queue returned true")
	}
}

func TestDequeueReturnsFalseWhenEmpty(t *testing.T) {
	t.Parallel()

	q := New[int](4)
	_, ok := q.Dequeue()
	if ok {
		t.Fatal("Dequeue on empty queue returned true")
	}
}

func TestFIFOOrdering(t *testing.T) {
	t.Parallel()

	const n = 8
	q := New[int](n)
	for i := 0; i < n; i++ {
		if !q.Enqueue(i) {
			t.Fatalf("Enqueue(%d) failed", i)
		}
	}
	for i := 0; i < n; i++ {
		val, ok := q.Dequeue()
		if !ok {
			t.Fatalf("Dequeue at position %d returned false", i)
		}
		if val != i {
			t.Fatalf("position %d: got %d, want %d", i, val, i)
		}
	}
}

func TestRingWrap(t *testing.T) {
	t.Parallel()

	// Fill and drain many times to exercise ring-buffer wrap and recycled slots.
	q := New[int](4)
	for round := 0; round < 1000; round++ {
		for i := 0; i < 4; i++ {
			if !q.Enqueue(i) {
				t.Fatalf("round %d: Enqueue(%d) failed", round, i)
			}
		}
		for i := 0; i < 4; i++ {
			val, ok := q.Dequeue()
			if !ok || val != i {
				t.Fatalf("round %d: Dequeue = (%d, %v), want (%d, true)", round, val, ok, i)
			}
		}
	}
}

func TestConcurrentMPMC(t *testing.T) {
	t.Parallel()

	const (
		producers = 8
		consumers = 8
		perWorker = 100_000
		total     = int64(producers * perWorker)
	)
	q := New[int](1024)

	var produced, consumed atomic.Int64
	var wg sync.WaitGroup

	for c := 0; c < consumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for consumed.Load() < total {
				if _, ok := q.Dequeue(); ok {
					consumed.Add(1)
				}
			}
		}()
	}

	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				for !q.Enqueue(1) {
					// Queue full; tight-spin to stay non-blocking.
				}
				produced.Add(1)
			}
		}()
	}

	wg.Wait()

	if produced.Load() != total {
		t.Fatalf("produced = %d, want %d", produced.Load(), total)
	}
	if consumed.Load() != total {
		t.Fatalf("consumed = %d, want %d", consumed.Load(), total)
	}
}

func TestTryEnqueueTimeoutSucceedsWhenSlotFreed(t *testing.T) {
	t.Parallel()

	q := New[int](2)
	q.Enqueue(1)
	q.Enqueue(2)

	go func() {
		time.Sleep(10 * time.Millisecond)
		q.Dequeue()
	}()

	ok := q.TryEnqueueTimeout(3, 200*time.Millisecond)
	if !ok {
		t.Fatal("TryEnqueueTimeout returned false but a slot was freed")
	}
}

func TestTryEnqueueTimeoutFailsWhenAlwaysFull(t *testing.T) {
	t.Parallel()

	q := New[int](2)
	q.Enqueue(1)
	q.Enqueue(2)

	ok := q.TryEnqueueTimeout(3, 20*time.Millisecond)
	if ok {
		t.Fatal("TryEnqueueTimeout returned true on a perpetually full queue")
	}
}

func TestLenAndCap(t *testing.T) {
	t.Parallel()

	q := New[int](4)
	if q.Cap() != 4 {
		t.Fatalf("Cap = %d, want 4", q.Cap())
	}
	if q.Len() != 0 {
		t.Fatalf("Len on empty queue = %d, want 0", q.Len())
	}
	q.Enqueue(1)
	q.Enqueue(2)
	if q.Len() != 2 {
		t.Fatalf("Len = %d, want 2", q.Len())
	}
	q.Dequeue()
	if q.Len() != 1 {
		t.Fatalf("Len after one Dequeue = %d, want 1", q.Len())
	}
}

// ExampleQueue_Enqueue demonstrates the basic enqueue/dequeue cycle.
func ExampleQueue_Enqueue() {
	q := New[string](4)
	q.Enqueue("hello")
	q.Enqueue("world")
	a, _ := q.Dequeue()
	b, _ := q.Dequeue()
	fmt.Println(a, b)
	// Output: hello world
}
```

Your turn: add `TestNoDataRace` that runs 4 producers and 4 consumers for 10,000 items each and verifies that `produced == consumed == 40000`. Run it with `go test -race` to confirm no race detector warnings.

### Exercise 4: Demo Program

Create `cmd/demo/main.go` to benchmark throughput across configurations:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/mpmc"
)

func bench(label string, producers, consumers, capacity, perWorker int) {
	q := mpmc.New[uint64](uint64(capacity))
	total := int64(producers * perWorker)

	var consumed atomic.Int64
	var wg sync.WaitGroup

	// Start consumers before producers so no items are dropped.
	for c := 0; c < consumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for consumed.Load() < total {
				if _, ok := q.Dequeue(); ok {
					consumed.Add(1)
				}
			}
		}()
	}

	start := time.Now()

	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				for !q.Enqueue(1) {
					// Queue full; retry immediately.
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	fmt.Printf("%-14s  items=%d  elapsed=%s  throughput=%.0f ops/sec\n",
		label, consumed.Load(),
		elapsed.Truncate(time.Millisecond),
		float64(total)/elapsed.Seconds())
}

func main() {
	fmt.Println("lock-free MPMC queue throughput")
	fmt.Println()
	bench("1P/1C", 1, 1, 1024, 2_000_000)
	bench("4P/4C", 4, 4, 1024, 500_000)
	bench("8P/8C", 8, 8, 1024, 250_000)
	bench("16P/16C", 16, 16, 4096, 125_000)
}
```

Run it with:

```bash
go run ./cmd/demo
```

On an 8-core machine you should see throughput above 10 million ops/sec for the 8P/8C configuration. If you do not, the most common cause is missing cache-line padding on the `enqueuePos`/`dequeuePos` fields.

## Common Mistakes

### Using Spaces Instead Of Tabs

Wrong: copying Go code from a web page that uses 4-space indentation. `gofmt` treats space-indented code as a formatting error and rewrites it.

Fix: always run `gofmt -w .` immediately after creating a `.go` file. Make it part of your editor's save hook.

### Using `!=` Instead Of Signed-Difference To Detect Full/Empty

Wrong:

```go
if seq != pos {
	return false // full?
}
```

This conflates "full" (`diff < 0`) with "concurrent producer advancing" (`diff > 0`). On a quiet queue you accidentally return false when a slower goroutine holds up the index.

Fix: compute `diff := int64(seq) - int64(pos)` and branch on `diff == 0`, `diff < 0`, and `diff > 0` separately. The signed cast is necessary because `seq` and `pos` are `uint64`; subtracting them as unsigned values would wrap to a large positive number instead of a small negative one.

### Reading `cell.val` Before The Sequence Check

Wrong:

```go
val := cell.val        // read first
seq := cell.seq.Load() // then check
if seq != pos+1 { ... }
```

If a producer is in the middle of writing `cell.val` when the consumer reads it, the consumer gets garbage. The sequence check must happen *before* reading the value, and only after the successful CAS confirms exclusive ownership of the slot.

Fix: load `seq`, do the diff check, CAS on `dequeuePos`, and only then read `cell.val`.

### Forgetting `runtime.Gosched()` In Backoff Loops

Wrong: a tight `for !q.Enqueue(val) {}` loop in a backpressure scenario. If the queue is full and all consumers are descheduled, the producers spin, consuming 100% CPU, and can prevent consumers from being scheduled (especially relevant under `GOMAXPROCS=1`).

Fix: use `TryEnqueueTimeout` with a short deadline and a `runtime.Gosched()` backoff, or add an explicit `runtime.Gosched()` in the retry loop. For production use, consider a separate token-bucket rate limiter upstream.

### Non-Power-Of-Two Capacity

Wrong: `New[int](100)` for a queue that will hold "about 100 items".

Fix: choose the next power of two: `New[int](128)`. The panic from `New` catches this at startup rather than silently producing incorrect index wrapping.

## Verification

From `~/go-exercises/mpmc`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

The race detector (`-race`) is mandatory for concurrent code. A lesson that passes all tests without `-race` but has a data race is not verified. Run with `-count=1` to disable result caching so the race detector runs every time.

To confirm no mutex, channel, or `sync.Mutex` is present in the implementation:

```bash
grep -n "sync\.Mutex\|sync\.RWMutex\|make(chan" queue.go
```

This must produce no output.

For the throughput demo:

```bash
go run ./cmd/demo
```

Target: 8P/8C throughput above 10 million ops/sec on a machine with 8 or more cores.

## Summary

- Vyukov's MPMC queue attaches a monotonically increasing sequence number to each slot; the sequence number encodes which "lap" the slot belongs to, eliminating the ABA problem without any additional bookkeeping.
- `int64(seq) - int64(pos)` is the fundamental test: zero means proceed, negative means full/empty, positive means retry.
- Cache-line padding between `enqueuePos` and `dequeuePos` is not optional: without it, throughput collapses under contention due to false sharing.
- Capacity must be a power of two; the `&mask` index wrap is a single AND instruction versus a division.
- `Enqueue` and `Dequeue` are non-blocking: they return immediately if the queue is full or empty. `TryEnqueueTimeout` adds bounded waiting with exponential backoff for back-pressure scenarios.
- The race detector (`go test -race`) is the minimum correctness check for any lock-free code; it catches missing happens-before edges that the human eye cannot.

## What's Next

Next: [Concurrent Skip List](../02-concurrent-skip-list/02-concurrent-skip-list.md).

## Resources

- [Dmitry Vyukov: Bounded MPMC Queue](https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue) — the original algorithm with C++ source and correctness argument.
- [The Go Memory Model](https://go.dev/ref/mem) — the authoritative source for happens-before rules and atomic operation semantics in Go.
- [sync/atomic package](https://pkg.go.dev/sync/atomic) — exact signatures for `Uint64`, `CompareAndSwap`, `Load`, and `Store`.
- [Go Blog: The Go Memory Model (2022 revision)](https://go.dev/blog/memory-model) — updated explanation of synchronization primitives and their ordering guarantees.
- [False Sharing and Cache Line Padding](https://en.wikipedia.org/wiki/False_sharing) — hardware-level explanation of why adjacent atomic variables need padding.
