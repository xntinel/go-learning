# Exercise 17: Lock-Free Single-Producer/Single-Consumer Ring with Atomics

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every ring buffer earlier in this lesson pays for correctness with a mutex or
a `sync.Cond`: whichever goroutine gets there first blocks every other one
for the duration of the critical section. When exactly one goroutine ever
produces and exactly one other goroutine ever consumes -- a metrics sampler
feeding an exporter, a NIC's receive path feeding a decoder, the shape behind
DPDK's single-producer rings and the per-consumer sequence tracking inside
the LMAX Disruptor -- that lock is provably unnecessary. The producer only
ever writes `head`; the consumer only ever writes `tail`. Neither side needs
a compare-and-swap loop, because neither side ever has to retry against a
concurrent writer of the same field. Two atomic loads and one atomic store
per operation are enough, and that is the fourth rung of this lesson's
concurrency ladder, one step past the `RWMutex` wrapper from earlier.

Getting there requires a second decision that looks cosmetic and is not: how
`head` and `tail` represent position. The single-goroutine `Ring[T]` from
this lesson's first module wraps them into `[0,capacity)` immediately and
tells empty from full with a `size` counter. Port that representation
naively into an atomics-based ring and the empty/full ambiguity this lesson
opens with -- `head == tail` meaning either "just emptied" or "just filled
an exact multiple of capacity" -- comes back, because a third atomic field
for size defeats the point of avoiding extra synchronized state. The fix
used here, and in every production lock-free ring worth the name, is to
never wrap `head` and `tail` at all: store them as ever-increasing sequence
counts, and take the modulus only when indexing into the backing array.
`Len` is then `head - tail`, correct at every value, with no separate
counter to keep in sync.

This module builds `SPSCRing[T]`, a generic ring restricted by contract to
one producer and one consumer, with `head` and `tail` padded onto separate
cache lines so the two goroutines' hot writes do not ping-pong the same
cache line between cores -- the same false-sharing concern that motivates
padding in the Disruptor and in DPDK's ring implementation. The wrapped-index
ambiguity is never part of the API; it exists only as an unexported helper in
the test file, isolated and pinned against the monotonic counters that avoid
it.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
spscring/                 module example.com/spscring
  go.mod                  go 1.24
  spscring.go             SPSCRing[T]; New, TryPush, TryPop, Len, Cap; one sentinel error
  spscring_test.go        sequential FIFO table, full/empty edges, evicted-slot zeroing,
                           wrapped-index-ambiguity contrast, concurrent producer/consumer,
                           ExampleSPSCRing
```

- Files: `spscring.go`, `spscring_test.go`.
- Implement: `New[T any](capacity int) (*SPSCRing[T], error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*SPSCRing[T]).TryPush(v T) bool` and `(*SPSCRing[T]).TryPop() (T, bool)`, both non-blocking and using only atomic load/store; `Len() int` and `Cap() int`.
- Test: sequential push/pop FIFO across single-slot, exact-capacity, and multi-wrap cases; a full ring rejects `TryPush` without clobbering existing slots; a popped pointer slot is zeroed; the wrapped-index empty/full ambiguity reproduced against an unexported helper and contrasted with `SPSCRing`'s monotonic `Len`; a real producer/consumer goroutine pair checked under `-race`; and `ExampleSPSCRing` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Monotonic counters, cache-line padding, and why no CAS is needed

A mutex-guarded ring serializes every operation, producer and consumer
alike, behind one lock. An SPSC ring does not need that: `TryPush` is the
only code that ever writes `head`, and `TryPop` is the only code that ever
writes `tail`. Each side only ever *reads* the other side's counter. That
single-writer property is what makes a plain `atomic.Uint64` load and store
sufficient -- there is no lost-update race to resolve with
compare-and-swap, because there is never more than one writer per field to
begin with. This is weaker, and faster, than the general-purpose MPMC
lock-free rings that do need CAS loops; the type's doc comment says so
explicitly, because using `TryPush` from two goroutines at once silently
corrupts the ring instead of failing loudly.

The representation of `head` and `tail` matters as much as the
synchronization primitive. A first attempt at "just make the existing ring
atomic" keeps the wrapped-index style from the single-goroutine `Ring[T]`:
`head, tail atomic.Int64`, both still reduced into `[0,capacity)`, with
`full()` computed as `head.Load()%cap == tail.Load()%cap`. That reintroduces
exactly the bug 00-concepts.md calls out first: the wrapped equality is true
both when the ring just emptied and when it just filled an exact multiple
of capacity. Adding a third atomic `size` field would resolve it, at the
cost of a second synchronized write per operation -- defeating the reason
to go lock-free at all. `SPSCRing` instead never wraps `head` and `tail`:
they count every push and pop ever made, so `Len()` is `head - tail` before
any modulus is taken, unambiguous at every value, with the array index
`head % r.capacity` computed only at the moment of the read or write.

The struct also pads `head` and `tail` onto separate byte ranges (`head
atomic.Uint64` followed by `_ [56]byte`, then `tail` the same way). Go does
not guarantee the struct itself lands on a 64-byte-aligned address, so this
does not *guarantee* each field owns a full cache line. What it does
guarantee is that `head` and `tail` never share one *with each other*: on
real hardware, the producer's every write to `head` and the consumer's
every write to `tail` would otherwise invalidate the same cache line on
both cores, a stall invisible in either goroutine's code and visible only
in a profiler -- the same padding discipline used in DPDK's ring buffer and
the Disruptor's `Sequence` type.

Create `spscring.go`:

```go
// Package spscring implements a single-producer/single-consumer ring buffer
// synchronized with plain atomic loads and stores, no mutex and no CAS loop.
//
// It exists to demonstrate the fourth rung of the concurrency ladder above a
// mutex-guarded ring: when exactly one goroutine ever pushes and exactly one
// other goroutine ever pops, the producer only ever writes head and the
// consumer only ever writes tail, so each side only needs an atomic load of
// the other side's counter, never a compare-and-swap. This is the same shape
// used by DPDK's single-producer rings and the core of the LMAX Disruptor's
// per-consumer sequence tracking.
package spscring

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// ErrInvalidCapacity is returned by New when capacity is not positive.
var ErrInvalidCapacity = errors.New("spscring: capacity must be positive")

// SPSCRing is a fixed-capacity ring buffer for exactly one producer
// goroutine and exactly one consumer goroutine.
//
// Concurrency contract: safe for exactly one goroutine calling TryPush and
// exactly one other goroutine calling TryPop concurrently. It is NOT safe
// for two producers to call TryPush concurrently, nor for two consumers to
// call TryPop concurrently -- both sides use plain atomic load/store, not
// compare-and-swap, which only tolerates a single writer per counter. For
// multiple producers or multiple consumers, use a mutex-guarded ring.
//
// head and tail are monotonically increasing sequence counts, never wrapped:
// the physical slot is head%capacity or tail%capacity. This is what lets
// Len() distinguish empty (head==tail) from full (head-tail==capacity)
// without a separate size field or a reserved sentinel slot -- the two
// classic fixes from 00-concepts.md -- while still costing only two atomic
// loads.
type SPSCRing[T any] struct {
	buf      []T
	capacity uint64

	// head is the sequence number of the next slot the producer will write.
	// Only TryPush ever stores it; TryPop only loads it.
	head atomic.Uint64
	_    [56]byte // pad head onto its own cache line, separate from tail

	// tail is the sequence number of the next slot the consumer will read.
	// Only TryPop ever stores it; TryPush only loads it.
	tail atomic.Uint64
	_    [56]byte

	// True cache-line placement additionally depends on the allocator
	// putting *SPSCRing at a 64-byte-aligned address, which Go does not
	// guarantee. What the padding above does guarantee is that head and
	// tail never share a line with each other, which is the property that
	// matters: without it, the producer's every write to head and the
	// consumer's every write to tail invalidate the same cache line on
	// both cores, a false-sharing stall neither goroutine's code shows any
	// sign of causing.
}

// New returns an SPSCRing with room for capacity elements. It returns
// ErrInvalidCapacity if capacity is not positive.
func New[T any](capacity int) (*SPSCRing[T], error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &SPSCRing[T]{
		buf:      make([]T, capacity),
		capacity: uint64(capacity),
	}, nil
}

// TryPush writes v into the ring and reports true, or reports false without
// blocking if the ring is full. Only the single producer goroutine may call
// TryPush.
func (r *SPSCRing[T]) TryPush(v T) bool {
	head := r.head.Load()
	tail := r.tail.Load()
	if head-tail >= r.capacity {
		return false
	}
	r.buf[head%r.capacity] = v
	r.head.Store(head + 1)
	return true
}

// TryPop removes and returns the oldest element, or reports false without
// blocking if the ring is empty. Only the single consumer goroutine may call
// TryPop.
//
// The vacated slot is overwritten with T's zero value before tail advances,
// so a ring of pointers or slices does not keep a popped element's referent
// reachable from the backing array for the rest of the ring's lifetime.
func (r *SPSCRing[T]) TryPop() (T, bool) {
	var zero T
	tail := r.tail.Load()
	head := r.head.Load()
	if tail == head {
		return zero, false
	}
	idx := tail % r.capacity
	v := r.buf[idx]
	r.buf[idx] = zero
	r.tail.Store(tail + 1)
	return v, true
}

// Len reports the number of elements currently in the ring. Under
// concurrent use by the producer and consumer, the value is a hint valid at
// the instant of the two loads that compute it; either side may have
// already changed it by the time the caller reads the result.
func (r *SPSCRing[T]) Len() int {
	return int(r.head.Load() - r.tail.Load())
}

// Cap reports the ring's fixed capacity.
func (r *SPSCRing[T]) Cap() int { return int(r.capacity) }
```

### Using it

`New` is the only constructor and it validates its argument, so every
`*SPSCRing[T]` a caller holds has at least one slot. `TryPush` and `TryPop`
never block: a full ring rejects the push, an empty ring reports nothing to
pop, and the caller decides whether to spin, back off, or drop the value.
The concurrency contract is the sharpest edge of this component and it is
stated on the type, not discovered by reading the implementation: exactly
one goroutine may call `TryPush`, exactly one other may call `TryPop`, and
using either method from two goroutines each violates that contract
silently rather than with a detectable error.

Most examples in this lesson call their component from one goroutine; this
one is only interesting under two, so `ExampleSPSCRing`, shown in full in
the Tests section below, spawns a producer and a consumer goroutine, waits
for both with a `sync.WaitGroup`, and prints the aggregate count and sum.
That aggregate is deterministic regardless of how the scheduler interleaves
the two goroutines -- the total count and sum come out the same no matter
which one runs first on any given tick -- which is what keeps it a safe,
timing-independent `Example`; `go test` runs it and compares its stdout
against the `// Output:` comment, so it cannot drift from the code.

### Tests

`TestTryPushTryPopSequentialFIFO` is the table: single-slot, exact-capacity,
and multi-wrap cases, each pushing and popping one at a time so every case
exercises the modulo indexing across several laps of the backing array.
`TestTryPushFullDoesNotOverwrite` is the full-ring edge: a rejected push
must leave every existing slot untouched. `TestEvictedSlotIsZeroed` is a
white-box check -- the test file is `package spscring`, so it can read the
unexported `buf` directly -- confirming a popped pointer does not linger in
a slot the ring still physically owns.

`TestWrappedIndexAmbiguityVersusMonotonicCounters` is the module's center of
gravity: it fills the ring to exact capacity, reduces `head` and `tail` to
`[0,capacity)` the way the naive wrapped-index design would, and shows
`wrappedFull` reports `true` for that state -- and also reports `true` for a
freshly constructed, genuinely empty ring. The wrapped representation cannot
tell the two apart; `SPSCRing`'s own `Len()`, computed from the unwrapped
counters, already did, one line above. `wrappedFull` is unexported and
unreachable from the package's API; it exists solely so this test can pin
the ambiguity SPSCRing's design avoids.

`TestConcurrentSPSCProducerConsumerNoLoss` drives the ring exactly as its
contract permits -- one producer, one consumer -- pushing and popping 5,000
values and asserting every one arrives exactly once, in order. It relies on
`-race` to confirm the atomic load/store pairing actually synchronizes:
without correct ordering, the race detector would catch the consumer
observing a torn or stale write.

Create `spscring_test.go`:

```go
package spscring

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1, -10} {
		if _, err := New[int](capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) err = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestTryPushTryPopSequentialFIFO(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		pushes   []int
	}{
		{name: "single slot", capacity: 1, pushes: []int{7}},
		{name: "exact capacity", capacity: 4, pushes: []int{1, 2, 3, 4}},
		{name: "wraps twice", capacity: 3, pushes: []int{1, 2, 3, 4, 5, 6, 7}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, err := New[int](tc.capacity)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Push and pop one at a time so wraps happen without ever
			// exceeding capacity, exercising the modulo indexing across
			// several laps around the backing array.
			for _, v := range tc.pushes {
				if !r.TryPush(v) {
					t.Fatalf("TryPush(%d): unexpected full", v)
				}
				got, ok := r.TryPop()
				if !ok {
					t.Fatalf("TryPop after pushing %d: unexpected empty", v)
				}
				if got != v {
					t.Fatalf("TryPop = %d, want %d", got, v)
				}
			}
			if _, ok := r.TryPop(); ok {
				t.Fatal("TryPop on drained ring: want empty")
			}
		})
	}
}

func TestTryPushFullDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	r, err := New[int](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, v := range []int{10, 20, 30} {
		if !r.TryPush(v) {
			t.Fatalf("TryPush(%d): unexpected full", v)
		}
	}
	if r.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", r.Len())
	}
	if r.TryPush(999) {
		t.Fatal("TryPush on full ring: want false")
	}
	if r.Len() != 3 {
		t.Fatalf("Len() after rejected push = %d, want 3", r.Len())
	}

	// The rejected push must not have clobbered any existing slot.
	for _, want := range []int{10, 20, 30} {
		got, ok := r.TryPop()
		if !ok || got != want {
			t.Fatalf("TryPop = (%d, %v), want (%d, true)", got, ok, want)
		}
	}
}

func TestEvictedSlotIsZeroed(t *testing.T) {
	t.Parallel()

	r, err := New[*int](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v := 42
	if !r.TryPush(&v) {
		t.Fatal("TryPush: unexpected full")
	}
	if _, ok := r.TryPop(); !ok {
		t.Fatal("TryPop: unexpected empty")
	}
	// White-box: this test lives in package spscring, so it can inspect the
	// unexported backing array directly to confirm the popped pointer was
	// cleared rather than left dangling in a slot the ring still owns.
	if r.buf[0] != nil {
		t.Fatalf("buf[0] = %v after pop, want nil (leaked reference)", r.buf[0])
	}
}

// wrappedFull is the empty/full check a naive port of the single-goroutine
// Ring from earlier in this lesson would use if head and tail were changed
// to atomics but left as wrapped indices in [0,capacity) instead of
// monotonic counts. It is never reachable from SPSCRing's API; it exists
// only to pin what breaks when "make it atomic" targets the wrong shape.
func wrappedFull(head, tail int) bool {
	return head == tail
}

// TestWrappedIndexAmbiguityVersusMonotonicCounters shows why SPSCRing
// stores head/tail as ever-increasing counts, not indices already reduced
// mod capacity: a wrapped design cannot tell "just emptied" from "just
// filled an exact multiple of capacity" -- both leave head==tail.
// Monotonic counters sidestep it: Len is head-tail before any modulo.
func TestWrappedIndexAmbiguityVersusMonotonicCounters(t *testing.T) {
	t.Parallel()

	const capacity = 4

	r, err := New[int](capacity)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < capacity; i++ {
		if !r.TryPush(i) {
			t.Fatalf("TryPush(%d): unexpected full", i)
		}
	}

	// The ring is genuinely full: capacity pushes landed, none popped.
	if r.Len() != capacity {
		t.Fatalf("Len() = %d, want %d (full)", r.Len(), capacity)
	}

	// A wrapped-index view of the same state (head, tail both mod capacity)
	// is 0 == 0, which is exactly what an empty ring also produces.
	wrappedHead := int(r.head.Load() % capacity)
	wrappedTail := int(r.tail.Load() % capacity)
	if !wrappedFull(wrappedHead, wrappedTail) {
		t.Fatalf("wrappedFull(%d, %d) = false, want true (the ambiguity itself)", wrappedHead, wrappedTail)
	}
	empty, err := New[int](capacity)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !wrappedFull(int(empty.head.Load()%capacity), int(empty.tail.Load()%capacity)) {
		t.Fatal("wrappedFull on a brand-new empty ring: want true, proving the ambiguity")
	}
	if empty.Len() != 0 {
		t.Fatalf("Len() on empty ring = %d, want 0", empty.Len())
	}
}

// TestConcurrentSPSCProducerConsumerNoLoss drives SPSCRing the way its
// contract permits: one producer, one consumer. It asserts every pushed
// value is popped once, in order, and relies on -race to confirm the
// atomic load/store pairing synchronizes correctly.
func TestConcurrentSPSCProducerConsumerNoLoss(t *testing.T) {
	t.Parallel()

	const n = 5000
	r, err := New[int](8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			for !r.TryPush(i) {
				runtime.Gosched()
			}
		}
	}()

	got := make([]int, 0, n)
	go func() {
		defer wg.Done()
		for len(got) < n {
			v, ok := r.TryPop()
			if !ok {
				runtime.Gosched()
				continue
			}
			got = append(got, v)
		}
	}()

	wg.Wait()

	if len(got) != n {
		t.Fatalf("received %d values, want %d", len(got), n)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("got[%d] = %d, want %d (order or loss)", i, v, i)
		}
	}
}

// ExampleSPSCRing runs a producer goroutine and a consumer goroutine over a
// small ring and prints the aggregate once both finish. The sum and count
// are deterministic regardless of how the scheduler interleaves the two
// goroutines, which is what makes this a safe Example: the assertion never
// depends on timing.
func ExampleSPSCRing() {
	ring, err := New[int](4)
	if err != nil {
		panic(err)
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			for !ring.TryPush(i) {
				runtime.Gosched()
			}
		}
	}()

	var sum, count int
	go func() {
		defer wg.Done()
		for count < n {
			if v, ok := ring.TryPop(); ok {
				sum += v
				count++
			} else {
				runtime.Gosched()
			}
		}
	}()

	wg.Wait()
	fmt.Printf("count=%d sum=%d cap=%d\n", count, sum, ring.Cap())
	// Output:
	// count=20 sum=190 cap=4
}
```

## Review

The ring is correct when `Len()` -- computed as `head - tail` before either
counter is ever reduced modulo capacity -- always matches the number of
elements actually pushed and not yet popped, at every point in the ring's
lifetime including the exact boundary where a wrapped-index design would go
ambiguous. Monotonic sequence counters are what buy that: `TestWrappedIndexAmbiguityVersusMonotonicCounters`
proves the wrapped alternative reports the same state for "just emptied"
and "just filled to exact capacity," while `SPSCRing`'s own `Len` never
confuses the two. Around that core, `New` rejects a non-positive capacity
with `ErrInvalidCapacity`, `TryPush` never overwrites a slot the consumer
has not yet read, `TryPop` zeroes the slot it vacates so a ring of pointers
does not leak, and the whole type is safe for exactly one producer goroutine
and one consumer goroutine concurrently -- proven under `-race` by
`TestConcurrentSPSCProducerConsumerNoLoss` -- and unsafe for anything wider,
by contract rather than by a runtime check. `ExampleSPSCRing` is the
executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Uint64`'s `Load` and `Store`, the only synchronization primitive this ring needs.
- [The LMAX Disruptor technical paper](https://lmax-exchange.github.io/disruptor/disruptor.html) — the multi-consumer lock-free ring this module's single-producer/single-consumer case is a slice of, including its cache-line padding rationale.
- [Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees an atomic store/load pair provides, which is what makes TryPush/TryPop safe without a mutex.
- [DPDK Ring Library](https://doc.dpdk.org/guides/prog_guide/ring_lib.html) — a production single-producer ring using the same monotonic-counter, no-CAS design for the SPSC case.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-connection-pool-health-checked-release.md](16-connection-pool-health-checked-release.md) | Next: [18-hashed-wheel-timer-slotted-ring.md](18-hashed-wheel-timer-slotted-ring.md)
