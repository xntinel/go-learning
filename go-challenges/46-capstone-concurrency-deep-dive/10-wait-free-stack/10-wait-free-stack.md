# 10. Wait-Free Stack

A wait-free algorithm guarantees that every thread completes its operation in a bounded number of steps, regardless of how other threads are scheduled. This is a strictly stronger promise than lock-freedom, which only guarantees system-wide progress (at least one thread advances, but any individual thread can retry indefinitely). Building a practical wait-free stack requires understanding the gap between the two guarantees and the techniques used to close it. This lesson implements a lock-free Treiber stack and extends it with an elimination backoff array that achieves wait-free progress for matched Push/Pop pairs. The full Kogan-Petrank helping mechanism that guarantees wait-freedom for every operation is described in the Concepts section; its implementation serves as the "your turn" extension.

```text
wfstack/
  go.mod
  wfstack.go
  wfstack_test.go
  cmd/demo/main.go
```

## Concepts

### The Progress Hierarchy

Four levels of progress guarantee appear in the concurrent-programming literature, from weakest to strongest:

**Blocking** (mutex): a thread holding a lock can be preempted indefinitely. Every other thread waiting for that lock makes no progress.

**Obstruction-free**: a thread makes progress if it runs in isolation. Concurrent threads may interfere and stall each other permanently.

**Lock-free**: system-wide progress is guaranteed. At least one thread completes its operation in every finite sequence of steps. Individual threads may retry indefinitely (livelock-free, but not starvation-free).

**Wait-free**: per-thread progress is guaranteed. Every thread completes its operation within a bounded number of its own steps, regardless of other threads' behavior. This is the strongest guarantee.

The Treiber stack is lock-free: its retry loop can spin indefinitely if contention is high enough. The elimination-backoff extension provides wait-free progress for matched Push/Pop pairs; the remaining unpaired operations remain lock-free. Full wait-freedom for every operation requires a helping mechanism.

### The Treiber Stack

The Treiber stack (R. K. Treiber, 1986) is the canonical lock-free stack. It stores elements as a linked list. Push allocates a new node, sets its `next` to the current top, and CAS-swaps the top atomically. Pop loads the top, reads its successor, and CAS-swaps the top to the successor. If the CAS fails, both operations retry.

The ABA problem — a thread reads top=A, another thread pops A and pushes it back, the first thread's CAS succeeds even though the list changed — is less dangerous in Go than in C++ because Go's garbage collector prevents a freed node from being reallocated at the same address while any goroutine holds a reference to it. Pointers in an `atomic.Pointer` are scanned by the GC, so a node remains alive until no goroutine can observe it. In languages without GC, Treiber stacks require hazard pointers or epoch-based reclamation to address ABA.

### The Elimination Array

Under high contention, many goroutines fail their CAS on the stack top repeatedly. The elimination-backoff technique (Hendler, Shavit, and Yerushalmi, 2004) observes that a Push and a Pop arriving simultaneously can exchange values directly, bypassing the central stack. From the outside this is indistinguishable from the Push completing first and the Pop removing it immediately after — the net effect on the stack is zero.

The elimination array is a fixed-size array of slots. Each slot has a state machine:

```
EMPTY   --[pusher CAS]-->  WAITING  (pusher deposits value and waits)
WAITING --[popper CAS]-->  BUSY     (popper claims the exchange)
BUSY    --[pusher CAS]-->  EMPTY    (pusher acknowledges and resets the slot)
```

When a Push fails its stack CAS, it picks a random slot and tries to deposit its value there. When a Pop fails its stack CAS, it picks a random slot and tries to claim a waiting pusher's value. If the exchange succeeds, both operations complete without touching the central stack.

The value must be stored behind an `atomic.Pointer`, not as a plain struct field. Go's race detector will flag a plain field write in the pusher concurrent with a plain field read in the popper as a data race, because no synchronization exists between those two instructions. Storing via `atomic.Pointer.Store` and loading via `atomic.Pointer.Load` provides the sequentially consistent ordering required for a correct handoff.

### Memory Ordering in sync/atomic

All operations in Go's `sync/atomic` package (and the `atomic.Pointer`, `atomic.Int64` types in `sync/atomic`) are sequentially consistent. No explicit memory fence or `sync.Mutex` is needed to order atomic stores against atomic loads in Go, because the operations themselves act as full barriers.

The consequence for the elimination array: the pusher's `slot.item.Store(item)` is sequentially consistent with the popper's subsequent `slot.item.Load()`. If the popper's load returns non-nil, it is guaranteed to see the pusher's stored value, not stale memory. The popper checks for nil explicitly because the pusher CASes the slot to WAITING before storing the item (a brief window where state=WAITING but item=nil exists); the nil check skips the slot in that window.

### Cache-Line Padding and False Sharing

When multiple goroutines access adjacent elimination slots, a single 64-byte CPU cache line may contain several slots. When any goroutine writes to its slot, the CPU invalidates the entire cache line across all cores, forcing other goroutines to reload it even if they access different slots. This false sharing can eliminate the contention-reduction benefit of the array itself.

Padding each slot to 64 bytes ensures each slot occupies its own cache line. The `exchSlot` type uses a `[48]byte` pad field: `atomic.Uint32` (4 bytes) + `atomic.Pointer` (8 bytes) + 48 bytes of padding = 60 bytes plus alignment, approximating one cache line. For production use, measure with `go test -bench -cpuprofile` to confirm the padding is effective.

### The Kogan-Petrank Helping Mechanism

The elimination-backoff extension described above is still lock-free for unpaired operations: a pusher that finds no popper in the elimination array retries the stack, which may retry indefinitely under extreme contention.

True wait-freedom requires that slow operations be completed by fast threads on their behalf. The Kogan-Petrank algorithm (2011) adds per-thread operation descriptors: each thread announces its pending operation (type, value, a monotonically increasing phase number, and a `completed` flag) in a shared array before attempting it. After completing its own operation, a thread scans the descriptor array and helps any incomplete operation from the current phase. The announcing thread returns as soon as it observes `completed == true`, regardless of whether it or a helper performed the actual work. This bounds the number of steps any thread can be stalled to O(P) where P is the number of threads, because the helping scan touches at most P descriptors per phase. Implementing this mechanism is the extension exercise at the end of this lesson.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/wfstack/cmd/demo
cd ~/go-exercises/wfstack
go mod init example.com/wfstack
```

This is a library, not a program. Verification uses `go test`.

### Exercise 1: The Stack Type and Treiber Core

Create `wfstack.go`. Start with the node and the basic Treiber stack without elimination; you will add the elimination array in Exercise 2.

```go
package wfstack

import (
	"math/rand/v2"
	"runtime"
	"sync/atomic"
)

const (
	slotEmpty   = uint32(0)
	slotWaiting = uint32(1)
	slotBusy    = uint32(2)

	// spinCount is the number of Gosched iterations a pusher waits
	// for a matching popper in the elimination array before retracting.
	spinCount = 64
)

// exchItem carries the value a pusher deposits into an elimination slot.
// Using a pointer avoids a data race: atomic.Pointer operations provide the
// memory ordering required for the pusher-popper handoff.
type exchItem[T any] struct {
	val T
}

// exchSlot is one slot in the elimination array.
// The padding field approximates a 64-byte cache line to reduce false sharing
// between adjacent slots when multiple goroutines access them simultaneously.
type exchSlot[T any] struct {
	state atomic.Uint32
	item  atomic.Pointer[exchItem[T]]
	_     [48]byte // pad to ~64 bytes: state(4) + ptr(8) + pad(48) + alignment
}

// node is one link in the Treiber linked list.
type node[T any] struct {
	val  T
	next *node[T]
}

// Stack is a generic lock-free stack with elimination backoff.
//
// Under low contention each Push and Pop performs a single atomic
// CompareAndSwap on the stack top (the classic Treiber algorithm).
// Under high contention, matched Push/Pop pairs exchange values directly
// through a fixed-size elimination array, bypassing the central stack and
// reducing CAS retries.
//
// Progress guarantee: individual operations are lock-free. Matched pairs
// through the elimination array achieve bounded-step progress. A full
// Kogan-Petrank wait-free implementation additionally requires a helping
// mechanism (see Concepts); that layer is the extension exercise.
//
// The zero value is not usable; call New.
type Stack[T any] struct {
	top  atomic.Pointer[node[T]]
	size atomic.Int64
	elim []exchSlot[T]
}

// New creates a Stack whose elimination array is sized to
// runtime.GOMAXPROCS(0) slots, with a minimum of two.
func New[T any]() *Stack[T] {
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		n = 2
	}
	return &Stack[T]{
		elim: make([]exchSlot[T], n),
	}
}
```

The generic constraint `[T any]` allows the stack to hold any type without reflection or unsafe casts. The elimination array is allocated once in `New` and never resized.

### Exercise 2: Push, Pop, and the Elimination Array

Add the public operations and the two unexported helpers to `wfstack.go`:

```go
// Push adds val to the top of the stack.
// It first attempts a direct CAS on the stack top; on contention it tries
// to exchange through the elimination array before retrying.
func (s *Stack[T]) Push(val T) {
	n := &node[T]{val: val}
	for {
		old := s.top.Load()
		n.next = old
		if s.top.CompareAndSwap(old, n) {
			s.size.Add(1)
			return
		}
		// CAS failed: contention detected. Attempt elimination.
		if s.tryPushElim(val) {
			s.size.Add(1)
			return
		}
		runtime.Gosched()
	}
}

// Pop removes and returns the top element.
// Returns the zero value of T and false if the stack is empty.
func (s *Stack[T]) Pop() (T, bool) {
	for {
		old := s.top.Load()
		if old == nil {
			var zero T
			return zero, false
		}
		if s.top.CompareAndSwap(old, old.next) {
			s.size.Add(-1)
			return old.val, true
		}
		// CAS failed: contention detected. Attempt elimination.
		if val, ok := s.tryPopElim(); ok {
			s.size.Add(-1)
			return val, true
		}
		runtime.Gosched()
	}
}

// Size returns an approximate count of elements in the stack.
// The count may transiently differ from the true element count under
// concurrent modification, but converges once all operations complete.
func (s *Stack[T]) Size() int {
	return int(s.size.Load())
}

// IsEmpty returns true if the stack top is nil at the moment of the call.
// This is a snapshot; a concurrent Push may arrive immediately after.
func (s *Stack[T]) IsEmpty() bool {
	return s.top.Load() == nil
}

// tryPushElim attempts to exchange val with a waiting popper via the
// elimination array. Returns true if the exchange succeeded.
//
// Slot state machine:
//
//	EMPTY   --[pusher CAS]-->  WAITING  (pusher announces and waits)
//	WAITING --[popper CAS]-->  BUSY     (popper claims the exchange)
//	BUSY    --[pusher CAS]-->  EMPTY    (pusher resets; exchange complete)
//
// The item pointer is stored after the WAITING CAS so that a popper
// observing WAITING is not guaranteed to see a non-nil item immediately.
// tryPopElim checks for nil and skips the slot in that window.
func (s *Stack[T]) tryPushElim(val T) bool {
	idx := rand.IntN(len(s.elim))
	slot := &s.elim[idx]

	if !slot.state.CompareAndSwap(slotEmpty, slotWaiting) {
		return false // slot is in use; caller retries the stack
	}

	// Store item after CASing to WAITING. All atomic operations provide
	// sequential consistency in Go, so no explicit fence is needed.
	item := &exchItem[T]{val: val}
	slot.item.Store(item)

	for i := 0; i < spinCount; i++ {
		if slot.state.Load() == slotBusy {
			// A popper claimed the exchange. Reset the slot for reuse.
			slot.state.CompareAndSwap(slotBusy, slotEmpty)
			slot.item.Store(nil)
			return true
		}
		runtime.Gosched()
	}

	// Spin timeout: try to retract before any popper can claim.
	if slot.state.CompareAndSwap(slotWaiting, slotEmpty) {
		slot.item.Store(nil)
		return false // no exchange; caller retries the stack
	}
	// A popper won the race between our timeout check and our retract CAS.
	// Complete the exchange on the pusher side.
	slot.state.CompareAndSwap(slotBusy, slotEmpty)
	slot.item.Store(nil)
	return true
}

// tryPopElim looks for a pusher waiting in the elimination array.
// Returns the exchanged value and true if a match was found.
//
// The popper loads the item pointer before its own CAS so the local
// reference remains valid even after the pusher later resets slot.item to nil.
// Go's garbage collector keeps exchItem alive as long as any reference to it
// exists, regardless of what the atomic.Pointer in the slot holds.
func (s *Stack[T]) tryPopElim() (T, bool) {
	idx := rand.IntN(len(s.elim))
	slot := &s.elim[idx]

	if slot.state.Load() != slotWaiting {
		var zero T
		return zero, false
	}
	item := slot.item.Load()
	if item == nil {
		// Pusher has CAS'd to WAITING but not yet stored the item.
		// Skip this slot; the next retry may find it populated.
		var zero T
		return zero, false
	}
	if !slot.state.CompareAndSwap(slotWaiting, slotBusy) {
		// Another popper or the pusher's timeout retraction won the race.
		var zero T
		return zero, false
	}
	// CAS succeeded: we own the exchange. item.val is safe to read because
	// item was loaded before our CAS; the pusher cannot reset the pointer
	// until after it observes BUSY and performs its own cleanup CAS.
	return item.val, true
}
```

The Gosched calls in `tryPushElim` yield to the Go scheduler during the spin, which lets the matching popper run on the same OS thread. Without them, the spin is a busy-wait that blocks other goroutines on the same P.

### Exercise 3: Test Suite

Create `wfstack_test.go`. The tests are the verification — there is no program to eyeball.

```go
package wfstack

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPopEmptyReturnsFalse(t *testing.T) {
	t.Parallel()

	s := New[int]()
	v, ok := s.Pop()
	if ok {
		t.Fatalf("Pop on empty stack: ok = true, v = %d; want ok = false", v)
	}
	if v != 0 {
		t.Fatalf("Pop on empty stack: v = %d, want zero value", v)
	}
}

func TestLIFOOrdering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		pushes []int
		want   []int
	}{
		{
			name:   "single element",
			pushes: []int{42},
			want:   []int{42},
		},
		{
			name:   "three elements LIFO",
			pushes: []int{1, 2, 3},
			want:   []int{3, 2, 1},
		},
		{
			name:   "five elements",
			pushes: []int{10, 20, 30, 40, 50},
			want:   []int{50, 40, 30, 20, 10},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := New[int]()
			for _, v := range tc.pushes {
				s.Push(v)
			}
			got := make([]int, 0, len(tc.want))
			for {
				v, ok := s.Pop()
				if !ok {
					break
				}
				got = append(got, v)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("popped %d elements, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("position %d: got %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSizeAndIsEmpty(t *testing.T) {
	t.Parallel()

	s := New[string]()
	if !s.IsEmpty() {
		t.Fatal("new stack should be empty")
	}
	if s.Size() != 0 {
		t.Fatalf("new stack Size = %d, want 0", s.Size())
	}
	s.Push("a")
	s.Push("b")
	if s.Size() != 2 {
		t.Fatalf("after 2 pushes: Size = %d, want 2", s.Size())
	}
	if s.IsEmpty() {
		t.Fatal("after 2 pushes: IsEmpty should be false")
	}
	s.Pop()
	if s.Size() != 1 {
		t.Fatalf("after 1 pop: Size = %d, want 1", s.Size())
	}
	s.Pop()
	if s.Size() != 0 {
		t.Fatalf("after 2 pops: Size = %d, want 0", s.Size())
	}
	if !s.IsEmpty() {
		t.Fatal("after draining: IsEmpty should be true")
	}
}

// TestConcurrentPushThenPop pushes N values concurrently, waits for all
// pushes to complete, then pops them all concurrently. Verifies that no
// elements are lost or duplicated.
func TestConcurrentPushThenPop(t *testing.T) {
	t.Parallel()

	const goroutines = 32
	const perGoroutine = 500
	const total = goroutines * perGoroutine

	s := New[int]()

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				s.Push(base*perGoroutine + j)
			}
		}(i)
	}
	wg.Wait()

	if got := s.Size(); got != total {
		t.Fatalf("after %d concurrent pushes: Size = %d, want %d", total, got, total)
	}

	var popped atomic.Int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, ok := s.Pop(); !ok {
					return
				}
				popped.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := popped.Load(); got != int64(total) {
		t.Fatalf("popped %d elements, want %d", got, total)
	}
	if !s.IsEmpty() {
		t.Fatalf("stack not empty after draining: Size = %d", s.Size())
	}
}

// TestConcurrentMixedOps runs pushers and poppers simultaneously to stress
// the elimination array and the race detector.
func TestConcurrentMixedOps(t *testing.T) {
	t.Parallel()

	const goroutines = 16
	const opsPerGoroutine = 1000

	s := New[int]()
	var pushed, popped atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(2)

		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				s.Push(id*opsPerGoroutine + j)
				pushed.Add(1)
			}
		}(i)

		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				if _, ok := s.Pop(); ok {
					popped.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// Drain any elements that the poppers missed.
	for {
		if _, ok := s.Pop(); !ok {
			break
		}
		popped.Add(1)
	}

	if p, po := pushed.Load(), popped.Load(); p != po {
		t.Fatalf("pushed %d, popped %d: mismatch (lost or duplicate elements)", p, po)
	}
}

func ExampleNew() {
	s := New[string]()
	s.Push("first")
	s.Push("second")
	s.Push("third")
	v1, _ := s.Pop()
	v2, _ := s.Pop()
	v3, _ := s.Pop()
	fmt.Println(v1, v2, v3)
	// Output:
	// third second first
}
```

Your turn: add `TestPopReturnsZeroValueOnEmpty` that calls `New[[]byte]()` and asserts that `Pop` returns a nil slice and `ok == false`. This pins the zero-value contract for reference types.

### Exercise 4: Demo Program

Create `cmd/demo/main.go`. This file is `package main` and can only use exported API.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/wfstack"
)

func main() {
	const goroutines = 16
	const perGoroutine = 10_000

	s := wfstack.New[int]()
	var wg sync.WaitGroup

	// Push phase: goroutines * perGoroutine elements pushed concurrently.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				s.Push(id*perGoroutine + j)
			}
		}(i)
	}
	wg.Wait()

	pushed := goroutines * perGoroutine
	fmt.Printf("push phase complete: pushed=%d size=%d\n", pushed, s.Size())

	// Pop phase: drain the stack concurrently.
	var popped atomic.Int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, ok := s.Pop(); !ok {
					return
				}
				popped.Add(1)
			}
		}()
	}
	wg.Wait()

	fmt.Printf("pop phase complete: popped=%d empty=%v\n", popped.Load(), s.IsEmpty())
}
```

Run with:

```bash
go run ./cmd/demo
```

Expected output (sizes are deterministic because the push phase completes before the pop phase starts):

```
push phase complete: pushed=160000 size=160000
pop phase complete: popped=160000 empty=true
```

### Extension: Add the Kogan-Petrank Helping Mechanism

Add a per-goroutine descriptor array to make every operation truly wait-free:

1. Define `opDesc[T any]` with fields: `opType` (push/pop), `val T`, `phase atomic.Uint64`, and `done atomic.Bool`.
2. Allocate a `[maxProcs]opDesc[T]` array in `Stack`. Each goroutine uses a slot indexed by `runtime_procPin()` or a goroutine-local ID.
3. Before attempting the stack CAS, a goroutine stores its descriptor and phase in its slot.
4. After completing its own operation, a goroutine scans the descriptor array and helps any incomplete descriptor from the current phase by performing the announced operation.
5. The announcing goroutine returns as soon as it observes `done == true`, regardless of who did the work.

This bounds completion time to O(P) steps per goroutine where P is the goroutine count.

## Common Mistakes

### Storing the Value as a Plain Struct Field

Wrong: the `exchSlot` value field is a plain `T`, written by the pusher and read by the popper without synchronization.

```go
// Wrong: data race between pusher write and popper read.
type exchSlot[T any] struct {
	state atomic.Uint32
	val   T // plain field — race detector will flag concurrent access
}
```

What happens: `go test -race` reports a data race between the pusher's `slot.val = v` and the popper's `_ = slot.val`. Plain reads and writes are not atomic and have no memory ordering guarantee.

Fix: store the value behind an `atomic.Pointer`. The pusher stores a pointer to a heap-allocated item; the popper loads the pointer and reads `item.val` through its local reference:

```go
type exchSlot[T any] struct {
	state atomic.Uint32
	item  atomic.Pointer[exchItem[T]]
	_     [48]byte
}
```

### Skipping the Nil Check in tryPopElim

Wrong: the popper CASes to BUSY before confirming the item is non-nil.

```go
// Wrong: item may be nil if pusher has not yet stored it.
if !slot.state.CompareAndSwap(slotWaiting, slotBusy) {
	return zero, false
}
item := slot.item.Load() // may be nil
return item.val, true    // nil dereference
```

What happens: the pusher CASes EMPTY→WAITING before storing the item. If the popper CASes WAITING→BUSY before the pusher stores, `slot.item.Load()` returns nil and `item.val` panics.

Fix: load the item before the CAS and check for nil. If nil, skip the slot:

```go
item := slot.item.Load()
if item == nil {
	return zero, false
}
if !slot.state.CompareAndSwap(slotWaiting, slotBusy) {
	return zero, false
}
return item.val, true
```

### Not Resetting the Slot After a Timeout Race

Wrong: when the pusher's timeout CAS (WAITING→EMPTY) fails, the pusher returns false without resetting the slot.

```go
if slot.state.CompareAndSwap(slotWaiting, slotEmpty) {
	slot.item.Store(nil)
	return false
}
return false // Wrong: slot is stuck in BUSY state permanently
```

What happens: a popper set the slot to BUSY between the timeout check and the CAS. The slot stays in BUSY forever, blocking future pushers from using it.

Fix: if the retract CAS fails, a popper has won. Complete the exchange on the pusher's side:

```go
if slot.state.CompareAndSwap(slotWaiting, slotEmpty) {
	slot.item.Store(nil)
	return false
}
slot.state.CompareAndSwap(slotBusy, slotEmpty)
slot.item.Store(nil)
return true
```

### Treating Size() as an Exact Count Under Concurrency

Wrong: asserting `s.Size() == expected` immediately after concurrent pushes complete in tests that also use elimination.

What happens: the `size` counter is updated by individual atomic `Add` calls inside each Push and Pop. During an elimination exchange, the pusher increments size and the popper decrements size at independent points in time. Between those two additions, the size may be transiently one higher or one lower than the true element count. In `TestConcurrentPushThenPop`, the push phase waits for all goroutines to finish (`wg.Wait()`) before checking size; at that point all `Add(1)` calls have completed and the count is exact.

Fix: only assert exact size after a `wg.Wait()` that covers all pushes. During concurrent mixed operations, treat `Size()` as approximate.

### Conflating Lock-Free With Wait-Free

Wrong: describing the elimination-backoff stack as fully wait-free in documentation or in comments.

What happens: a pusher that consistently fails to find a matching popper in the elimination array retries the Treiber CAS indefinitely. Under pathological scheduling (all goroutines are pushers), no individual operation is bounded.

Fix: state the actual guarantee. The implementation in this lesson is lock-free overall and wait-free for matched Push/Pop pairs. Full per-operation wait-freedom requires the Kogan-Petrank helping mechanism described in the extension exercise.

## Verification

From `~/go-exercises/wfstack`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector with 32 goroutines (`TestConcurrentPushThenPop`) is the primary correctness check. Add one more test of your own: `TestPopReturnsZeroValueOnEmpty` that calls `New[[]byte]()`, pops, and asserts a nil slice and `ok == false`.

## Summary

- Wait-free is strictly stronger than lock-free: every thread completes in bounded steps, not just the system as a whole.
- The Treiber stack is lock-free: a failed CAS causes a retry loop that can spin indefinitely under contention.
- The elimination array lets matched Push/Pop pairs bypass the central stack; each slot's state machine (EMPTY → WAITING → BUSY → EMPTY) is driven by atomic CAS operations.
- Values must be stored behind `atomic.Pointer`, not as plain struct fields; the race detector will catch plain concurrent reads/writes.
- False sharing between elimination slots is reduced by padding each slot to approximately one cache line (64 bytes).
- Full wait-freedom requires a helping mechanism: fast threads complete the announced operations of stalled threads. The Kogan-Petrank algorithm bounds per-thread completion to O(P) steps.
- `Size()` and `IsEmpty()` are approximate under concurrent access; assert exact values only after all writers have finished.

## What's Next

Next: [Double Buffering for Concurrent Read/Write](../11-double-buffering/11-double-buffering.md).

## Resources

- [R. K. Treiber, "Systems Programming: Coping with Parallelism" (IBM Research Report RJ 5118, 1986)](https://dominoweb.draco.res.ibm.com/reports/rj5118.pdf) — original lock-free stack algorithm
- [Danny Hendler, Nir Shavit, Lena Yerushalmi, "A Scalable Lock-Free Stack Algorithm" (SPAA 2004)](https://dl.acm.org/doi/10.1145/1007912.1007944) — elimination backoff stack
- [Alex Kogan and Erez Petrank, "A Methodology for Creating Fast Wait-Free Data Structures" (PPoPP 2012)](https://dl.acm.org/doi/10.1145/2145816.2145835) — wait-free transformation with helping
- [pkg.go.dev/sync/atomic](https://pkg.go.dev/sync/atomic) — atomic.Pointer, atomic.Int64, CompareAndSwap semantics in Go
- [Maurice Herlihy and Nir Shavit, "The Art of Multiprocessor Programming", Chapter 11](https://www.elsevier.com/books/the-art-of-multiprocessor-programming/herlihy/978-0-12-415950-1) — concurrent stacks and elimination arrays
