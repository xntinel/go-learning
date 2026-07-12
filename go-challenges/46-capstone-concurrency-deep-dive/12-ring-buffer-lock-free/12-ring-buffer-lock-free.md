# 12. Lock-Free Ring Buffer

A lock-free ring buffer achieves inter-goroutine communication without mutexes by exploiting the constraint on who reads and who writes each index. The SPSC (single-producer, single-consumer) variant needs only `atomic.Store`/`atomic.Load`; the MPSC (multi-producer, single-consumer) variant adds a CAS loop and a per-slot committed flag to handle the commit-gap problem — the case where producer A claims slot 5, producer B claims slot 6, and B commits before A. Both variants require cache-line padding to prevent false sharing from destroying the throughput gains.

```text
ringbuf/
  go.mod
  ringbuf.go
  ringbuf_test.go
  cmd/demo/main.go
```

## Concepts

### The Ring Buffer Model

A ring buffer (circular buffer) is a fixed-size array of slots indexed by a `head` (read cursor) and a `tail` (write cursor). Both indices increase monotonically and wrap around with a bitmask (`index & mask`) rather than a modulo, which requires the capacity to be a power of two. When `tail - head == capacity` the buffer is full; when `head == tail` it is empty.

Because the indices never reset, a 64-bit counter overflows after 2^64 operations — roughly 500 years at one billion operations per second — so overflow is not a practical concern. The bitmask approach also eliminates a branch that would be needed if indices were reset to zero on wrap-around.

### SPSC: Producer and Consumer Own Separate Indices

In the SPSC model, the producer is the only writer of `tail` and the only reader of `head`; the consumer is the only writer of `head` and the only reader of `tail`. Because no two goroutines write the same index, no compare-and-swap is needed. A plain `atomic.Store` by the writer paired with a plain `atomic.Load` by the reader is sufficient.

The memory ordering guarantee is: the store to `buf[tail&mask]` is sequenced before the `atomic.Store` on `tail`; the consumer's `atomic.Load` on `tail` is sequenced before the read of `buf[head&mask]`. Go's memory model guarantees that if the consumer's load observes the updated tail, the consumer also observes the value stored in the slot — the atomic store and load create a synchronization edge.

### Cache-Line False Sharing and Padding

Modern CPUs transfer memory in 64-byte cache lines. If `head` and `tail` share a cache line, every write to `tail` by the producer invalidates the consumer's cache entry for `head` (and vice versa), even though the two goroutines never write the same variable. This is false sharing. The fix is to place each counter on its own cache line with 56 bytes of padding:

```go
type paddedUint64 struct {
	val atomic.Uint64
	_   [56]byte // atomic.Uint64 is 8 bytes; pad to 64 (one cache line)
}
```

Without padding, false sharing can cut SPSC throughput by 10x on hardware with a strict cache-coherence protocol such as MESI.

### MPSC: CAS-Based Slot Claiming and the Commit-Gap Problem

When multiple producers compete, the SPSC invariant breaks: two goroutines write `tail`. The fix is a CAS loop: each producer atomically increments `tail` to claim a slot, then writes the value and sets a per-slot `committed` flag. Because the CAS is atomic, only one producer wins each increment.

The commit-gap problem: producer A claims slot 5 and producer B claims slot 6. B finishes writing before A. If the consumer advanced past slot 5 to read slot 6, it would observe the wrong order or read an uncommitted value. The consumer avoids this by spinning on the `committed` flag of the head slot; it advances head only after that flag is set. Items are always delivered in claim order.

Slot reuse is safe because the consumer resets `committed = false` before advancing head. By the time the ring wraps around and a new producer claims the same physical position, `tail - head < capacity` guarantees the consumer has already passed that slot and reset its flag.

### Go's Memory Model and Atomic Synchronization

Go's memory model (go.dev/ref/mem) says: if an atomic store of value V to address A is observed by an atomic load of address A, then the store is synchronized before the load. Everything sequenced before the store in the storing goroutine is visible to the loading goroutine after the load.

In MPSC `Poll`, the spin on `slot.committed.Load()` returning `true` establishes a happens-before edge from the producer's `slot.committed.Store(true)` (which is sequenced after `slot.value = v`) to the consumer's subsequent read of `slot.value`. The non-atomic `slot.value` access is therefore free of data races — the atomic bool acts as the synchronization point, which is why the race detector reports no issues under `-race`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/46-capstone-concurrency-deep-dive/12-ring-buffer-lock-free/12-ring-buffer-lock-free/cmd/demo
cd go-solutions/46-capstone-concurrency-deep-dive/12-ring-buffer-lock-free/12-ring-buffer-lock-free
```

### Exercise 1: The paddedUint64 Helper, SPSC Type, and Batch Operations

Create `ringbuf.go`. This is a library, not a program: verify it with `go test`, not `go run`.

```go
// Package ringbuf provides lock-free SPSC and MPSC ring buffers backed by
// sync/atomic operations. Both types are generic and require a power-of-two
// capacity so that index masking replaces modulo arithmetic.
package ringbuf

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
)

// paddedUint64 places an atomic counter on its own 64-byte cache line.
// Without padding, head and tail share a cache line and writes by the producer
// invalidate the consumer's cached copy of the line on every enqueue —
// "false sharing" that can cut throughput by 10x on modern hardware.
type paddedUint64 struct {
	val atomic.Uint64
	_   [56]byte // atomic.Uint64 is 8 bytes; pad to 64 (one cache line)
}

// ErrCapacity is returned when the requested capacity is zero or greater than
// 1<<30. Wrap it with fmt.Errorf("…%w", ErrCapacity) to carry extra context.
var ErrCapacity = errors.New("ringbuf: capacity must be a power of two between 1 and 1<<30")

// nextPow2 rounds n up to the next power of two and validates the range.
func nextPow2(n uint64) (uint64, error) {
	if n == 0 || n > (1<<30) {
		return 0, fmt.Errorf("%w: got %d", ErrCapacity, n)
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	n++
	return n, nil
}

// SPSC is a lock-free single-producer single-consumer ring buffer.
//
// Ownership rule: exactly one goroutine calls Offer* (the producer) and
// exactly one goroutine calls Poll* (the consumer). The producer owns tail;
// the consumer owns head. Because each party only reads the other's index
// (never writes it), no CAS is needed — a plain atomic.Store on the writer's
// side paired with atomic.Load on the reader's side is sufficient.
//
// Capacity is always a power of two so that buf[index & mask] replaces the
// slower buf[index % cap] and the index never needs to be reset to zero.
type SPSC[T any] struct {
	head paddedUint64 // consumer reads here; producer reads (never writes) this
	tail paddedUint64 // producer writes here; consumer reads (never writes) this
	mask uint64
	buf  []T
}

// NewSPSC returns a new SPSC buffer. capacity is rounded up to the next power
// of two. Returns ErrCapacity if capacity is 0 or greater than 1<<30.
func NewSPSC[T any](capacity uint64) (*SPSC[T], error) {
	c, err := nextPow2(capacity)
	if err != nil {
		return nil, err
	}
	return &SPSC[T]{
		mask: c - 1,
		buf:  make([]T, c),
	}, nil
}

// Offer enqueues v without blocking. It returns false if the buffer is full.
//
// Memory ordering: the store to buf[tail&mask] is sequenced before the
// atomic.Store on tail, which synchronizes with the consumer's atomic.Load on
// tail before it reads the value. This establishes a happens-before edge
// without CAS.
func (q *SPSC[T]) Offer(v T) bool {
	tail := q.tail.val.Load()
	head := q.head.val.Load()
	if tail-head > q.mask { // tail - head == capacity means full
		return false
	}
	q.buf[tail&q.mask] = v
	q.tail.val.Store(tail + 1)
	return true
}

// Poll dequeues one item without blocking. It returns (zero, false) if the
// buffer is empty.
func (q *SPSC[T]) Poll() (T, bool) {
	head := q.head.val.Load()
	tail := q.tail.val.Load()
	if head == tail {
		var zero T
		return zero, false
	}
	v := q.buf[head&q.mask]
	q.head.val.Store(head + 1)
	return v, true
}

// OfferBatch enqueues as many items from vs as fit in one call. It returns the
// count actually enqueued, which may be less than len(vs) if the buffer is
// nearly full.
func (q *SPSC[T]) OfferBatch(vs []T) int {
	tail := q.tail.val.Load()
	head := q.head.val.Load()
	free := int(q.mask+1) - int(tail-head)
	n := len(vs)
	if n > free {
		n = free
	}
	for i := range n {
		q.buf[(tail+uint64(i))&q.mask] = vs[i]
	}
	q.tail.val.Store(tail + uint64(n))
	return n
}

// PollBatch dequeues up to len(dst) items into dst in one call. It returns the
// count actually dequeued.
func (q *SPSC[T]) PollBatch(dst []T) int {
	head := q.head.val.Load()
	tail := q.tail.val.Load()
	avail := int(tail - head)
	n := len(dst)
	if n > avail {
		n = avail
	}
	for i := range n {
		dst[i] = q.buf[(head+uint64(i))&q.mask]
	}
	q.head.val.Store(head + uint64(n))
	return n
}

// Len returns a snapshot of the number of items in the buffer. Because the
// producer and consumer run concurrently, this value may be stale by the time
// the caller uses it.
func (q *SPSC[T]) Len() int {
	return int(q.tail.val.Load() - q.head.val.Load())
}

// Cap returns the buffer capacity (always a power of two).
func (q *SPSC[T]) Cap() int { return int(q.mask + 1) }
```

`nextPow2` uses a standard bit-smearing trick: after decrementing n, it OR-shifts to fill all bits below the highest set bit, then increments to the next power. The six shifts cover all 64 bit positions.

`OfferBatch` reads both cursors once, computes how many slots are free, caps the transfer, writes the range without re-reading the cursors, and advances `tail` in a single store. This avoids the per-item overhead of loading `head` on each iteration.

### Exercise 2: MPSC with CAS and Per-Slot Committed Flags

Add the MPSC types to `ringbuf.go`:

```go
// mpscSlot is one position in the MPSC buffer. The committed flag is the
// synchronization point between a producer and the consumer: the producer
// writes value first, then sets committed = true; the consumer spins on
// committed, reads value, then resets committed = false for the next lap.
type mpscSlot[T any] struct {
	committed atomic.Bool
	value     T
}

// MPSC is a lock-free multi-producer single-consumer ring buffer.
//
// Any number of goroutines may call Offer concurrently. Exactly one goroutine
// must call Poll* at a time.
//
// Producers claim a slot by atomically incrementing tail with CompareAndSwap.
// After writing to the claimed slot, the producer sets committed = true.
// The consumer reads only from the slot at head and waits (with Gosched) until
// that slot is committed, so items are always delivered in claim order even
// when producers commit out of order (the commit-gap problem).
//
// Slot reuse: when the ring wraps around, the consumer has already reset
// committed = false before advancing head past the slot, so a new producer
// always finds a clean committed flag.
type MPSC[T any] struct {
	head paddedUint64
	tail paddedUint64
	mask uint64
	buf  []mpscSlot[T]
}

// NewMPSC returns a new MPSC buffer. capacity is rounded up to the next power
// of two. Returns ErrCapacity if capacity is 0 or greater than 1<<30.
func NewMPSC[T any](capacity uint64) (*MPSC[T], error) {
	c, err := nextPow2(capacity)
	if err != nil {
		return nil, err
	}
	return &MPSC[T]{
		mask: c - 1,
		buf:  make([]mpscSlot[T], c),
	}, nil
}

// Offer enqueues v. Multiple goroutines may call Offer concurrently.
// Returns false if the buffer is full; the caller should retry or back off.
//
// The CAS loop is the only contention point. When many producers compete,
// runtime.Gosched() yields the processor so that the winning producer can
// commit quickly before losing producers retry.
func (q *MPSC[T]) Offer(v T) bool {
	for {
		tail := q.tail.val.Load()
		head := q.head.val.Load()
		if tail-head > q.mask {
			return false // full
		}
		if q.tail.val.CompareAndSwap(tail, tail+1) {
			slot := &q.buf[tail&q.mask]
			slot.value = v
			slot.committed.Store(true) // release: value is visible before this store
			return true
		}
		runtime.Gosched() // back off before retrying the CAS
	}
}

// Poll dequeues one item. It spin-waits with Gosched if the head slot has been
// claimed by a producer that has not yet committed. Returns (zero, false) only
// when the buffer is empty (no producer has claimed the head slot).
//
// The spin is bounded in practice: a producer that claimed a slot finishes
// writing within nanoseconds on modern hardware; the consumer yields the CPU
// rather than burning a full quantum in a tight loop.
func (q *MPSC[T]) Poll() (T, bool) {
	head := q.head.val.Load()
	if head == q.tail.val.Load() {
		var zero T
		return zero, false
	}
	slot := &q.buf[head&q.mask]
	// Wait for the producer that claimed this slot to finish writing.
	for !slot.committed.Load() {
		runtime.Gosched()
	}
	v := slot.value
	var zero T
	slot.value = zero           // clear reference so GC can reclaim T if it holds pointers
	slot.committed.Store(false) // reset for the next lap around the ring
	q.head.val.Store(head + 1)
	return v, true
}

// PollBatch dequeues up to len(dst) items. It stops at the first uncommitted
// slot to preserve order; items are never reordered or skipped.
func (q *MPSC[T]) PollBatch(dst []T) int {
	head := q.head.val.Load()
	tail := q.tail.val.Load()
	avail := int(tail - head)
	n := 0
	for n < len(dst) && n < avail {
		slot := &q.buf[(head+uint64(n))&q.mask]
		if !slot.committed.Load() {
			break // gap: producer claimed but has not committed yet
		}
		dst[n] = slot.value
		var zero T
		slot.value = zero
		slot.committed.Store(false)
		n++
	}
	if n > 0 {
		q.head.val.Store(head + uint64(n))
	}
	return n
}

// Len returns a snapshot of the number of claimed (committed or not) slots.
func (q *MPSC[T]) Len() int {
	return int(q.tail.val.Load() - q.head.val.Load())
}

// Cap returns the buffer capacity.
func (q *MPSC[T]) Cap() int { return int(q.mask + 1) }
```

The GC comment on `slot.value = zero` matters for pointer-bearing types: if T is `*MyData` or a slice, clearing the slot lets the GC collect the referent after the consumer reads it. Without the clear, the slot holds a live reference until the ring wraps around.

### Exercise 3: Tests, Example Functions, and cmd/demo

Create `ringbuf_test.go`:

```go
package ringbuf

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// ---- SPSC tests -------------------------------------------------------

func TestNewSPSCRoundsCapacityToPowerOfTwo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   uint64
		want int
	}{
		{1, 1},
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
		{1000, 1024},
		{1024, 1024},
		{1025, 2048},
	}
	for _, tc := range cases {
		q, err := NewSPSC[int](tc.in)
		if err != nil {
			t.Fatalf("NewSPSC(%d): %v", tc.in, err)
		}
		if q.Cap() != tc.want {
			t.Errorf("NewSPSC(%d).Cap() = %d, want %d", tc.in, q.Cap(), tc.want)
		}
	}
}

func TestNewSPSCRejectsZeroCapacity(t *testing.T) {
	t.Parallel()

	_, err := NewSPSC[int](0)
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("err = %v, want ErrCapacity", err)
	}
}

func TestNewSPSCRejectsOversizeCapacity(t *testing.T) {
	t.Parallel()

	_, err := NewSPSC[int](1 << 31)
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("err = %v, want ErrCapacity", err)
	}
}

func TestSPSCEmptyPollReturnsFalse(t *testing.T) {
	t.Parallel()

	q, _ := NewSPSC[int](4)
	_, ok := q.Poll()
	if ok {
		t.Fatal("Poll on empty buffer should return false")
	}
}

func TestSPSCOfferAndPoll(t *testing.T) {
	t.Parallel()

	q, _ := NewSPSC[int](4)
	if !q.Offer(10) {
		t.Fatal("Offer into empty buffer returned false")
	}
	v, ok := q.Poll()
	if !ok || v != 10 {
		t.Fatalf("Poll = (%d, %v), want (10, true)", v, ok)
	}
	// buffer is empty again
	_, ok = q.Poll()
	if ok {
		t.Fatal("Poll on now-empty buffer should return false")
	}
}

func TestSPSCOfferReturnsFalseWhenFull(t *testing.T) {
	t.Parallel()

	q, _ := NewSPSC[int](2)
	q.Offer(1)
	q.Offer(2)
	if q.Offer(3) {
		t.Fatal("Offer into full buffer should return false")
	}
	if q.Len() != 2 {
		t.Fatalf("Len = %d after filling 2-slot buffer, want 2", q.Len())
	}
}

func TestSPSCBatchRoundTrip(t *testing.T) {
	t.Parallel()

	q, _ := NewSPSC[int](8)
	src := []int{10, 20, 30, 40, 50}
	n := q.OfferBatch(src)
	if n != 5 {
		t.Fatalf("OfferBatch = %d, want 5", n)
	}
	dst := make([]int, 8)
	got := q.PollBatch(dst)
	if got != 5 {
		t.Fatalf("PollBatch = %d, want 5", got)
	}
	for i, want := range src {
		if dst[i] != want {
			t.Fatalf("dst[%d] = %d, want %d", i, dst[i], want)
		}
	}
}

func TestSPSCBatchDoesNotExceedCapacity(t *testing.T) {
	t.Parallel()

	q, _ := NewSPSC[int](4)                    // capacity 4
	n := q.OfferBatch([]int{1, 2, 3, 4, 5, 6}) // only 4 fit
	if n != 4 {
		t.Fatalf("OfferBatch into 4-capacity buffer = %d, want 4", n)
	}
}

func TestSPSCConcurrentOrderPreservation(t *testing.T) {
	t.Parallel()

	const N = 1 << 14
	q, _ := NewSPSC[int](N)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range N {
			for !q.Offer(i) {
				// producer spins while consumer is slow
			}
		}
	}()

	received := make([]int, N)
	go func() {
		defer wg.Done()
		for i := range N {
			for {
				v, ok := q.Poll()
				if ok {
					received[i] = v
					break
				}
			}
		}
	}()

	wg.Wait()

	for i, v := range received {
		if v != i {
			t.Fatalf("received[%d] = %d, want %d (FIFO order violated)", i, v, i)
		}
	}
}

func TestSPSCWrapAround(t *testing.T) {
	t.Parallel()

	// Fill and drain three full laps to exercise index wrap-around.
	q, _ := NewSPSC[int](4)
	for lap := range 3 {
		for i := range 4 {
			if !q.Offer(lap*100 + i) {
				t.Fatalf("lap %d: Offer(%d) returned false", lap, i)
			}
		}
		for i := range 4 {
			v, ok := q.Poll()
			if !ok || v != lap*100+i {
				t.Fatalf("lap %d: Poll() = (%d, %v), want (%d, true)", lap, v, ok, lap*100+i)
			}
		}
	}
}

// ---- MPSC tests -------------------------------------------------------

func TestNewMPSCRejectsZeroCapacity(t *testing.T) {
	t.Parallel()

	if _, err := NewMPSC[int](0); !errors.Is(err, ErrCapacity) {
		t.Fatalf("err = %v, want ErrCapacity", err)
	}
}

func TestMPSCOfferAndPoll(t *testing.T) {
	t.Parallel()

	q, _ := NewMPSC[int](4)
	if !q.Offer(42) {
		t.Fatal("Offer into empty MPSC buffer returned false")
	}
	v, ok := q.Poll()
	if !ok || v != 42 {
		t.Fatalf("Poll = (%d, %v), want (42, true)", v, ok)
	}
	_, ok = q.Poll()
	if ok {
		t.Fatal("Poll on empty MPSC buffer should return false")
	}
}

func TestMPSCFullReturnsFalse(t *testing.T) {
	t.Parallel()

	q, _ := NewMPSC[int](2)
	q.Offer(1)
	q.Offer(2)
	if q.Offer(3) {
		t.Fatal("Offer into full MPSC buffer should return false")
	}
}

func TestMPSCMultiProducerNoDataLoss(t *testing.T) {
	t.Parallel()

	const (
		producers = 8
		perProd   = 512
		total     = producers * perProd
	)
	q, _ := NewMPSC[int](total * 2)

	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := range perProd {
				v := p*perProd + i
				for !q.Offer(v) {
					// back off if full
				}
			}
		}(p)
	}

	seen := make([]bool, total)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for count := 0; count < total; {
			v, ok := q.Poll()
			if !ok {
				continue
			}
			if v < 0 || v >= total {
				t.Errorf("value %d out of expected range [0,%d)", v, total)
				return
			}
			if seen[v] {
				t.Errorf("value %d received twice", v)
				return
			}
			seen[v] = true
			count++
		}
	}()

	wg.Wait()
	<-done

	for i, got := range seen {
		if !got {
			t.Errorf("value %d was never received", i)
		}
	}
}

func TestMPSCBatchDrainsInOrder(t *testing.T) {
	t.Parallel()

	q, _ := NewMPSC[int](8)
	for i := range 5 {
		q.Offer(i)
	}
	dst := make([]int, 8)
	n := q.PollBatch(dst)
	if n != 5 {
		t.Fatalf("PollBatch = %d, want 5", n)
	}
	for i, want := range []int{0, 1, 2, 3, 4} {
		if dst[i] != want {
			t.Fatalf("dst[%d] = %d, want %d", i, dst[i], want)
		}
	}
}

// TestMPSCRaceDetector exercises the MPSC buffer under -race to confirm that
// no unsynchronized accesses are reported.
func TestMPSCRaceDetector(t *testing.T) {
	t.Parallel()

	const (
		producers = 4
		perProd   = 256
		total     = producers * perProd
	)
	q, _ := NewMPSC[int](total * 2)

	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := range perProd {
				for !q.Offer(p*perProd + i) {
				}
			}
		}(p)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for count := 0; count < total; {
			if _, ok := q.Poll(); ok {
				count++
			}
		}
	}()

	wg.Wait()
	<-done
}

// Your turn: add TestSPSCBatchPollEmptyBuffer that calls PollBatch on an empty
// SPSC buffer and asserts that 0 items are returned and the dst slice is
// unchanged. This pins the boundary behaviour of PollBatch.

// ---- Example functions (auto-verified by go test) ----------------------

func ExampleNewSPSC() {
	q, err := NewSPSC[string](4)
	if err != nil {
		panic(err)
	}
	q.Offer("hello")
	q.Offer("world")
	v1, _ := q.Poll()
	v2, _ := q.Poll()
	fmt.Println(v1, v2)
	// Output: hello world
}

func ExampleNewMPSC() {
	q, err := NewMPSC[int](4)
	if err != nil {
		panic(err)
	}
	q.Offer(1)
	q.Offer(2)
	q.Offer(3)
	dst := make([]int, 4)
	n := q.PollBatch(dst)
	fmt.Println(n, dst[:n])
	// Output: 3 [1 2 3]
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo runs a small demonstration of the ringbuf package.
// It does not assert correctness; run "go test -race ./..." for that.
package main

import (
	"fmt"
	"sync"

	"example.com/ringbuf"
)

func main() {
	demoSPSC()
	demoMPSC()
}

func demoSPSC() {
	fmt.Println("--- SPSC ---")
	q, err := ringbuf.NewSPSC[int](8)
	if err != nil {
		panic(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 5 {
			for !q.Offer(i) {
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 5 {
			for {
				v, ok := q.Poll()
				if ok {
					fmt.Println("received", v)
					break
				}
			}
		}
	}()

	wg.Wait()
}

func demoMPSC() {
	fmt.Println("--- MPSC ---")
	const numProducers = 3
	q, err := ringbuf.NewMPSC[string](16)
	if err != nil {
		panic(err)
	}

	var wg sync.WaitGroup
	for p := range numProducers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			msg := fmt.Sprintf("producer-%d", p)
			for !q.Offer(msg) {
			}
		}(p)
	}

	received := make(chan string, numProducers)
	go func() {
		count := 0
		for count < numProducers {
			v, ok := q.Poll()
			if ok {
				received <- v
				count++
			}
		}
		close(received)
	}()

	wg.Wait()
	for msg := range received {
		fmt.Println("received", msg)
	}
}
```

Run `go run ./cmd/demo` to see live SPSC and MPSC output. The MPSC output order is non-deterministic because three producers race; only the consumer order (delivery order matches claim order) is guaranteed.

## Common Mistakes

### Wrong: Treating Uint64 Underflow as Overflow in the Full Check

Wrong:

```go
if tail-head >= capacity { // fails when head > tail (impossible with uint64 but misleading)
```

The indices are `uint64` and increase monotonically. `tail - head` wraps around if head somehow exceeded tail, which the SPSC invariant prevents. The correct check is `tail-head > mask`, which equals `tail-head >= capacity`. Using `>` against `mask` rather than `>=` against `capacity` avoids introducing a second variable.

Fix: use `tail-head > q.mask` exactly as shown. The mask is `capacity - 1`, so `> mask` is mathematically identical to `>= capacity`.

### Wrong: No Cache-Line Padding

Wrong:

```go
type queue struct {
	head atomic.Uint64
	tail atomic.Uint64
	// ...
}
```

On a shared-memory multiprocessor, the producer's stores to `tail` and the consumer's stores to `head` land on the same 64-byte cache line. Every producer write forces the CPU to request ownership of that line from the consumer's cache, and vice versa — even though neither goroutine touches the other's field. This is false sharing. On an Apple M-series chip it costs around 70 ns per operation instead of 3 ns.

Fix: use `paddedUint64` to put each counter on its own cache line.

### Wrong: Polling MPSC Without Waiting for the Committed Flag

Wrong:

```go
func (q *MPSC[T]) Poll() (T, bool) {
	head := q.head.val.Load()
	if head == q.tail.val.Load() {
		var zero T
		return zero, false
	}
	v := q.buf[head&q.mask].value // reads before the producer commits
	q.head.val.Store(head + 1)
	return v, true
}
```

Producer A claims slot 5 (tail goes from 5 to 6) but has not yet written `slot.value` or set `committed`. The consumer sees `tail == 6`, reads slot 5, and gets the zero value or the previous lap's stale value.

Fix: spin on `slot.committed.Load()` before reading `slot.value`. The `committed` flag is the release-acquire synchronization point between the producer and the consumer.

### Wrong: Forgetting to Reset the Committed Flag Before Advancing Head

Wrong:

```go
v := slot.value
q.head.val.Store(head + 1) // slot.committed is still true
```

When the ring wraps around, the new producer for that slot finds `committed == true` and the consumer reads it prematurely (before the new producer writes), producing either garbage or the previous lap's value.

Fix: always `slot.committed.Store(false)` before `q.head.val.Store(head + 1)`.

### Wrong: Calling Both Offer and Poll From Multiple Goroutines on SPSC

The SPSC invariant is: one goroutine calls `Offer*`, one goroutine calls `Poll*`. If two goroutines call `Poll` concurrently, both read the same `head`, both see a non-empty buffer, both read `buf[head&mask]`, and both increment head — the second increment advances head past an unconsumed slot, silently losing an item. The race detector flags this.

Fix: use MPSC if multiple consumers are required, or serialize consumer calls behind a mutex.

## Verification

From `~/go-exercises/ringbuf`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Run the demo:

```bash
go run ./cmd/demo
```

The SPSC section prints 0–4 in order. The MPSC section prints three `producer-N` lines in an arbitrary order (producers race for the CAS).

Add `TestSPSCBatchPollEmptyBuffer` that calls `PollBatch` on a freshly created SPSC buffer and asserts that zero items are returned and the dst slice is unchanged. This pins the boundary behavior of `PollBatch` when the buffer is empty.

## Summary

- SPSC requires only `atomic.Store`/`atomic.Load` because the producer owns `tail` and the consumer owns `head`; neither writes the other's index.
- MPSC adds a CAS loop so multiple producers can compete for `tail`, plus a per-slot `committed` flag to solve the commit-gap problem.
- Cache-line padding (`paddedUint64`) eliminates false sharing between the producer's tail and the consumer's head; without it, throughput can drop by an order of magnitude.
- Go's memory model guarantees that a non-atomic write before an `atomic.Store` is visible to a goroutine after its corresponding `atomic.Load` returns the stored value, making the `committed` flag a safe synchronization point for the non-atomic `slot.value` field.
- Power-of-two capacity enables index masking (`index & mask`) instead of modulo, and lets the 64-bit indices count up indefinitely without resetting.
- The `Len` snapshot is always stale in a concurrent setting; treat it as advisory, not authoritative.

## What's Next

Next: [Lock-Free Hash Map](../13-lock-free-hash-map/13-lock-free-hash-map.md).

## Resources

- [sync/atomic package documentation](https://pkg.go.dev/sync/atomic) — method signatures for `Uint64`, `Bool`, `CompareAndSwap`, and the guarantee that atomic operations are sequentially consistent within a goroutine.
- [The Go Memory Model](https://go.dev/ref/mem) — the formal definition of the synchronization edges created by atomic operations; Section "Atomic Values" explains why `atomic.Store`/`atomic.Load` pairs establish happens-before without CAS.
- [Dmitry Vyukov, "Bounded MPMC Queue"](https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue) — the canonical reference for the per-slot sequence-number technique on which MPSC committed flags are based.
- [The Go Race Detector](https://go.dev/blog/race-detector) — how `-race` instruments memory accesses and uses the happens-before graph to detect unsynchronized reads and writes; essential reading for understanding why atomic operations suppress race reports on adjacent non-atomic fields.
- [LMAX Disruptor: High Performance Alternative to Bounded Queues](https://lmax-exchange.github.io/disruptor/disruptor.html) — the production system that popularized ring buffers with cache-line padding and batch-committed sequences for inter-thread messaging at sub-100-ns latency.
