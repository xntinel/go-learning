# 3. Hazard Pointer Memory Reclamation

Hazard pointer reclamation solves a fundamental problem in lock-free programming: when is it safe to reuse a node that has been unlinked from a shared data structure? Goroutines still traversing the structure may hold pointers to the unlinked node, so recycling it immediately causes use-after-recycle bugs. Hazard pointers solve this by requiring each goroutine to *publish* the addresses it is currently dereferencing. A node is only reclaimed when no goroutine's published pointer refers to it.

Go's garbage collector eliminates dangling pointers, but it does not eliminate use-after-recycle bugs when nodes are explicitly pooled to reduce allocation pressure. This lesson builds a complete hazard pointer domain, integrates it with a lock-free Treiber stack, and demonstrates that the protocol prevents use-after-recycle under high contention.

```text
hazptr/
  go.mod
  hazptr.go
  stack.go
  hazptr_test.go
  cmd/demo/main.go
```

## Concepts

### The Memory Reclamation Problem

Consider a lock-free stack with two goroutines both reading the top pointer:

1. A loads `top → N`.
2. B pops N, adds N to a free list.
3. C pops N from the free list and sets `N.Val = newValue`.
4. A reads `N.Val` and sees `newValue` instead of the expected value.

This is a use-after-recycle bug. The ABA problem is a related race: A loads `top → N`, B pops N and pushes it back, A's CAS succeeds even though the stack changed underneath. Both problems share the root cause: a goroutine holds a pointer to a node that another goroutine has already logically removed and potentially recycled.

### The Hazard Pointer Protocol

Each goroutine maintains K hazard pointer slots — atomic pointers visible to all goroutines. The invariant is:

**A node N may only be reclaimed when no goroutine's hazard pointer slot points to N.**

Three operations implement this:

**Protect(slot, ptr):** Store ptr in the calling goroutine's hazard slot. This signals to the reclamation scan that ptr is currently in use.

**Release(slot):** Clear hazard pointer slot, signalling that the goroutine no longer needs the pointer.

**Retire(ptr):** Add ptr to the goroutine's local retired list. When the list reaches a threshold, trigger Scan.

**Scan:** Collect all hazard pointers from all goroutines into a set H. For each retired node n:
- If n is in H: keep in the retired list (still protected by some goroutine).
- If n is not in H: push to the free list (safe to reclaim).

### The Protect-Validate Loop

A single `top.Load()` is not safe before accessing `top.next`. Consider this interleaving:

```
goroutine A:  Load(top→N)                  [gap]  Protect(0, N)  Load(next)
goroutine B:                Pop(N) Retire(N) Scan pushFree(N)
goroutine C:                                               Alloc(N) N.Val=new
```

Between Load and Protect, B could retire and C could reuse N. A's Protect arrives too late to block the Scan. The fix is the protect-validate loop:

```go
for {
	ptr := s.top.Load()
	if ptr == nil {
		return 0, false
	}
	r.Protect(0, ptr)
	// Validate: did the head change between our Load and our Protect?
	if s.top.Load() == ptr {
		break // ptr is now safely protected
	}
	r.Release(0) // ptr was removed before we protected it; retry
}
// ptr is protected: any subsequent Scan will keep it in the retired list.
```

After protecting ptr and re-validating, any Scan that runs will see ptr in the hazard set and skip it. The window between the original Load and the Protect is closed by the re-read.

### Reclamation Threshold and Amortisation

Scan is O(P + R) where P is the total number of published hazard pointers and R is the size of the retired list. Triggering Scan on every Retire wastes work when R is small. The standard threshold is:

```text
threshold = multiplier * numRecords * hazardSlotsPerRecord
```

With multiplier = 2, numRecords = 32, hazardSlots = 2 the threshold is 128. Scan runs at most every 128 retires per goroutine and is expected to reclaim roughly half the retired list each time because the protected set typically covers far fewer nodes than the retired list. A lower multiplier increases scan frequency; a higher multiplier increases peak memory retention.

### Goroutine Registration and the Record Pool

Go does not expose goroutine-local storage. Each goroutine calls `Domain.Register()` at startup and holds its own `*Record`. The record is returned to the pool via `Unregister()` when the goroutine exits. Reusing record slots bounds the memory overhead of the hazard pointer array and avoids per-goroutine allocations in the hot path.

An unregistered record must have all hazard pointer slots cleared before it is returned to the pool. Otherwise a later Scan may see stale pointers and incorrectly block reclamation of already-retired nodes.

### Val as atomic.Int64 and the Race Detector

The hazard pointer protocol establishes the correct happens-before ordering at the semantic level: by the time a goroutine reads a recycled node's Val, the chain of atomic CAS operations on `freeHead` and `s.top` ensures that any prior writes to Val are visible. However, Go's race detector (ThreadSanitizer) tracks happens-before per-variable: it does not propagate ordering from a CAS on `freeHead` to plain reads or writes of `node.Val`. Making `Node.Val` an `atomic.Int64` so that all reads and writes go through `Val.Load()` and `Val.Store()` is the correct way to satisfy the detector and make the synchronization explicit and verifiable by tooling.

## Exercises

### Exercise 1: The Domain, Record, and Node Types

Create `hazptr.go`:

```go
// Package hazptr implements hazard pointer memory reclamation for
// lock-free data structures, following Maged M. Michael (2004).
package hazptr

import (
	"sync"
	"sync/atomic"
)

const (
	slotsPerRecord      = 2
	retireThresholdMult = 2
	// recycledSentinel is written to Node.Val when a node is retired.
	// Any concurrent read of this value indicates a use-after-recycle bug.
	recycledSentinel int64 = 0xDEADC0DE
)

// Node is an element managed by the Domain.  Val carries the payload;
// next links nodes in the Treiber stack and the free list.
//
// Val is an atomic.Int64 (not a plain int64) because Retire writes the
// recycledSentinel to it while another goroutine may concurrently write a
// new value via Alloc.  The hazard-pointer protocol establishes the correct
// ordering at the semantic level, but Go's race detector (TSan) requires
// per-variable atomicity and does not track happens-before through atomic
// operations on a different variable (freeHead, s.top).
//
// A Node must never be copied: it contains atomic fields.
type Node struct {
	Val  atomic.Int64
	next atomic.Pointer[Node]
}

// Record is a per-goroutine hazard pointer record.
// Obtain one via Domain.Register and release it via Unregister.
// A Record must not be shared between goroutines.
type Record struct {
	hp      [slotsPerRecord]atomic.Pointer[Node]
	retired []*Node
	d       *Domain
	active  atomic.Bool
}

// Protect stores ptr in hazard slot s, making it visible to all goroutines
// that call Scan.  The caller must follow with a validate step (re-read the
// shared pointer and compare) before dereferencing ptr.
func (r *Record) Protect(slot int, ptr *Node) {
	r.hp[slot].Store(ptr)
}

// Release clears hazard pointer slot s.
func (r *Record) Release(slot int) {
	r.hp[slot].Store(nil)
}

// Retire adds ptr to the goroutine's local retired list.  ptr.Val is
// poisoned with recycledSentinel so tests can detect use-after-recycle.
// When the retired list reaches the reclamation threshold, Scan is triggered.
func (r *Record) Retire(ptr *Node) {
	ptr.Val.Store(recycledSentinel)
	r.retired = append(r.retired, ptr)

	n := r.d.recordCount()
	threshold := retireThresholdMult * n * slotsPerRecord
	if threshold < 1 {
		threshold = 1
	}
	if len(r.retired) >= threshold {
		r.d.Scan(r)
	}
}

// Unregister clears all hazard pointer slots and marks this record as
// inactive so another goroutine can reuse it.  Call via defer immediately
// after Register.
func (r *Record) Unregister() {
	for i := range r.hp {
		r.hp[i].Store(nil)
	}
	r.active.Store(false)
}

// Domain manages the set of hazard pointer records and the free list.
// All goroutines sharing a lock-free data structure must share one Domain.
type Domain struct {
	mu      sync.Mutex
	records []*Record

	// freeHead is the head of a lock-free free list (a Treiber stack of
	// reclaimed nodes).
	freeHead atomic.Pointer[Node]

	allocNew  atomic.Int64
	allocFree atomic.Int64
}

// NewDomain returns a Domain pre-populated with initialCap inactive records.
// Additional records are allocated on demand as more goroutines register.
func NewDomain(initialCap int) *Domain {
	if initialCap < 1 {
		initialCap = 1
	}
	d := &Domain{}
	d.records = make([]*Record, initialCap)
	for i := range d.records {
		d.records[i] = &Record{d: d}
	}
	return d
}

// Register returns an active Record for the calling goroutine.
// If no inactive record is available, a new one is appended to the pool.
func (d *Domain) Register() *Record {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.records {
		if r.active.CompareAndSwap(false, true) {
			return r
		}
	}
	r := &Record{d: d}
	r.active.Store(true)
	d.records = append(d.records, r)
	return r
}

// recordCount returns the current number of records in the pool.
// Used to compute the retire threshold.
func (d *Domain) recordCount() int {
	d.mu.Lock()
	n := len(d.records)
	d.mu.Unlock()
	return n
}

// Scan collects all published hazard pointers from all records, then
// partitions r's retired list: unprotected nodes go to the free list;
// protected nodes remain in r.retired for the next Scan.
func (d *Domain) Scan(r *Record) {
	// Snapshot the record slice under the lock.  Reading individual hp slots
	// is done outside the lock via atomic loads.
	d.mu.Lock()
	snapshot := make([]*Record, len(d.records))
	copy(snapshot, d.records)
	d.mu.Unlock()

	protected := make(map[*Node]struct{}, len(snapshot)*slotsPerRecord)
	for _, rec := range snapshot {
		for s := 0; s < slotsPerRecord; s++ {
			if p := rec.hp[s].Load(); p != nil {
				protected[p] = struct{}{}
			}
		}
	}

	// Partition r.retired in place to avoid a separate allocation.
	keep := r.retired[:0]
	for _, n := range r.retired {
		if _, ok := protected[n]; ok {
			keep = append(keep, n)
		} else {
			d.pushFree(n)
		}
	}
	r.retired = keep
}

// pushFree adds n to the lock-free free list.
func (d *Domain) pushFree(n *Node) {
	for {
		head := d.freeHead.Load()
		n.next.Store(head)
		if d.freeHead.CompareAndSwap(head, n) {
			return
		}
	}
}

// Alloc returns a Node from the free list when available, or allocates a
// new one.  The returned node has Val set to val and next cleared.
func (d *Domain) Alloc(val int64) *Node {
	for {
		head := d.freeHead.Load()
		if head == nil {
			d.allocNew.Add(1)
			n := &Node{}
			n.Val.Store(val)
			return n
		}
		next := head.next.Load()
		if d.freeHead.CompareAndSwap(head, next) {
			head.Val.Store(val)
			head.next.Store(nil)
			d.allocFree.Add(1)
			return head
		}
	}
}

// AllocStats returns (allocatedNew, allocatedFromFreeList).
func (d *Domain) AllocStats() (int64, int64) {
	return d.allocNew.Load(), d.allocFree.Load()
}
```

`Record.Protect` is a plain `atomic.Pointer.Store`. Go's memory model guarantees that the store is visible to all goroutines that subsequently call `Load` on the same variable. There is no need for an additional fence: `sync/atomic` operations establish happens-before relationships.

### Exercise 2: The Treiber Stack

Create `stack.go`:

```go
package hazptr

import "sync/atomic"

// Stack is a lock-free LIFO stack.  All goroutines sharing one Stack must
// share the same Domain.
type Stack struct {
	top atomic.Pointer[Node]
}

// NewStack returns an empty Stack.
func NewStack() *Stack {
	return &Stack{}
}

// Push allocates a node from d and prepends val to the stack.
func (s *Stack) Push(d *Domain, val int64) {
	n := d.Alloc(val)
	for {
		top := s.top.Load()
		n.next.Store(top)
		if s.top.CompareAndSwap(top, n) {
			return
		}
	}
}

// Pop removes and returns the top value using the protect-validate loop on
// hazard slot 0.  Returns (value, true) if the stack is non-empty, (0, false)
// otherwise.
func (s *Stack) Pop(d *Domain, r *Record) (int64, bool) {
	for {
		top := s.top.Load()
		if top == nil {
			return 0, false
		}
		// Step 1: protect.
		r.Protect(0, top)
		// Step 2: validate — did the head change between the Load and Protect?
		if s.top.Load() != top {
			r.Release(0)
			continue
		}
		// top is now protected: any Scan that runs will keep it in the
		// retired list until we call Release.
		//
		// Read Val while the hazard pointer is still set and the node is
		// still on the stack.  Reading after the CAS also works in theory
		// (the happens-before chain through s.top guarantees visibility),
		// but in practice the Go race detector (TSan) observes a stale
		// write from a prior Retire when the CAS acts as the only fence.
		// Reading before the CAS, while hp[0] = top, ensures we observe
		// the value written by the most recent Alloc with no ambiguity.
		// If the CAS then fails we discard val and retry; Retire can only
		// run after our CAS succeeds, so no sentinel is ever returned.
		val := top.Val.Load()
		next := top.next.Load()
		if s.top.CompareAndSwap(top, next) {
			r.Release(0)
			r.Retire(top) // poisons top.Val with recycledSentinel
			return val, true
		}
		// CAS failed: another goroutine changed top concurrently.  Loop.
	}
}

// Len counts nodes by traversal.  Not linearizable with concurrent Push/Pop;
// use only after all goroutines have stopped.
func (s *Stack) Len() int {
	n := 0
	cur := s.top.Load()
	for cur != nil {
		n++
		cur = cur.next.Load()
	}
	return n
}
```

The ordering within the protect-validate-read-CAS loop is load-bearing:

1. `r.Protect(0, top)` — publish the hazard pointer before reading any field.
2. validate (`s.top.Load() != top`) — confirm the node is still the stack top.
3. `val := top.Val.Load()` — read Val while hp[0] = top AND the node is on the stack. Reading after the CAS is tempting (the CAS's happens-before chain theoretically covers the Val write), but in practice the Go race detector (TSan) observes a stale sentinel write from a prior Retire when the CAS is the only fence. Reading before the CAS, while hp[0] protects the node from reclamation, eliminates that ambiguity.
4. `s.top.CompareAndSwap(top, next)` — if the CAS fails, discard val and retry. Retire can only run after a successful CAS, so a discarded val is never the sentinel.
5. `r.Release(0)` — clear the hazard pointer after the CAS succeeds.
6. `r.Retire(top)` — poison Val with recycledSentinel, add to retired list.

### Exercise 3: Tests and the Example Function

Create `hazptr_test.go`:

```go
package hazptr

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// ExampleStack demonstrates LIFO ordering in a single goroutine.
func ExampleStack() {
	d := NewDomain(2)
	r := d.Register()
	defer r.Unregister()

	s := NewStack()
	s.Push(d, 10)
	s.Push(d, 20)
	s.Push(d, 30)

	for {
		v, ok := s.Pop(d, r)
		if !ok {
			break
		}
		fmt.Println(v)
	}
	// Output:
	// 30
	// 20
	// 10
}

func TestBasicPushPop(t *testing.T) {
	t.Parallel()

	d := NewDomain(2)
	r := d.Register()
	defer r.Unregister()

	s := NewStack()

	if _, ok := s.Pop(d, r); ok {
		t.Fatal("Pop on empty stack returned true")
	}

	for _, v := range []int64{1, 2, 3} {
		s.Push(d, v)
	}
	for _, want := range []int64{3, 2, 1} {
		got, ok := s.Pop(d, r)
		if !ok {
			t.Fatal("Pop returned false on non-empty stack")
		}
		if got != want {
			t.Errorf("Pop = %d, want %d", got, want)
		}
	}
	if _, ok := s.Pop(d, r); ok {
		t.Fatal("Pop on drained stack returned true")
	}
}

// TestNoUseAfterRecycle runs concurrent Push/Pop operations and asserts that
// no Pop ever reads the recycledSentinel value written by Retire.
// If hazard pointer protection fails, a goroutine may read a poisoned node.
func TestNoUseAfterRecycle(t *testing.T) {
	t.Parallel()

	const numG = 8
	const opsPerG = 20000

	d := NewDomain(numG)
	s := NewStack()

	var wg sync.WaitGroup
	var bad atomic.Int64

	for i := 0; i < numG; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := d.Register()
			defer r.Unregister()
			for j := 0; j < opsPerG; j++ {
				// Push values that are never equal to recycledSentinel.
				s.Push(d, int64(id*opsPerG+j+1))
				if val, ok := s.Pop(d, r); ok {
					if val == recycledSentinel {
						bad.Add(1)
					}
				}
			}
		}(i)
	}
	wg.Wait()

	if n := bad.Load(); n != 0 {
		t.Errorf("read recycledSentinel %d time(s): hazard pointer protection failed", n)
	}
}

// TestScanReclaims verifies that Scan pushes unprotected retired nodes to the
// free list, and that subsequent Alloc calls draw from it.
func TestScanReclaims(t *testing.T) {
	t.Parallel()

	d := NewDomain(1)
	r := d.Register()
	defer r.Unregister()

	s := NewStack()

	// Phase 1: push then pop enough nodes to exceed the retire threshold and
	// trigger a Scan, populating the free list.
	threshold := retireThresholdMult * 1 * slotsPerRecord // = 4
	for i := 0; i < threshold+1; i++ {
		s.Push(d, int64(i))
	}
	for i := 0; i < threshold+1; i++ {
		s.Pop(d, r)
	}

	// Phase 2: push more nodes; Alloc should draw from the free list.
	for i := 0; i < 4; i++ {
		s.Push(d, int64(100+i))
	}

	_, fromFree := d.AllocStats()
	if fromFree == 0 {
		t.Error("expected allocations from the free list after Scan; got 0")
	}
}

// TestFreeListReuse verifies that after a warm-up phase, most allocations
// come from the free list rather than from new(Node).
func TestFreeListReuse(t *testing.T) {
	t.Parallel()

	const numG = 4
	const opsPerG = 5000

	d := NewDomain(numG)
	s := NewStack()

	var wg sync.WaitGroup
	for i := 0; i < numG; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := d.Register()
			defer r.Unregister()
			for j := 0; j < opsPerG; j++ {
				s.Push(d, int64(j))
				s.Pop(d, r)
			}
		}()
	}
	wg.Wait()

	fromNew, fromFree := d.AllocStats()
	total := fromNew + fromFree
	if total == 0 {
		t.Fatal("no allocations recorded")
	}
	freePct := float64(fromFree) * 100.0 / float64(total)
	// After warm-up, the free list should supply at least half of allocations.
	if freePct < 50.0 {
		t.Errorf("free list reuse = %.1f%%, want >= 50%%; fromNew=%d fromFree=%d",
			freePct, fromNew, fromFree)
	}
}

// TestConcurrentStress runs 16 goroutines interleaving Push and Pop for a
// combined total of 200,000 operations and expects no data races under -race.
func TestConcurrentStress(t *testing.T) {
	t.Parallel()

	const numG = 16
	const opsPerG = 12500

	d := NewDomain(numG)
	s := NewStack()

	var wg sync.WaitGroup
	for i := 0; i < numG; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := d.Register()
			defer r.Unregister()
			for j := 0; j < opsPerG; j++ {
				s.Push(d, int64(id*opsPerG+j))
				s.Pop(d, r)
			}
		}(i)
	}
	wg.Wait()
}

// TestUnregisterReleasesSlot verifies that Unregister returns the record to
// the pool, so the next Register call reuses it instead of growing the pool.
func TestUnregisterReleasesSlot(t *testing.T) {
	t.Parallel()

	d := NewDomain(1)

	r1 := d.Register()
	r1.Unregister()

	before := d.recordCount()
	r2 := d.Register()
	defer r2.Unregister()

	if d.recordCount() != before {
		t.Errorf("record pool grew from %d to %d; Unregister did not release the slot",
			before, d.recordCount())
	}
}

// Your turn: add TestProtectValidatePreventsStaleRead that starts a goroutine
// continuously pushing and popping values, then verifies that a concurrent
// Pop from the main goroutine never returns recycledSentinel.  Run it with
// -race and confirm no races are detected.
```

### Exercise 4: The Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/hazptr"
)

func main() {
	const numG = 8
	const opsPerG = 10000

	d := hazptr.NewDomain(numG)
	s := hazptr.NewStack()

	var wg sync.WaitGroup
	var totalPops atomic.Int64

	for i := 0; i < numG; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := d.Register()
			defer r.Unregister()
			for j := 0; j < opsPerG; j++ {
				s.Push(d, int64(id*opsPerG+j))
				if _, ok := s.Pop(d, r); ok {
					totalPops.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	fromNew, fromFree := d.AllocStats()
	total := fromNew + fromFree
	fmt.Printf("goroutines:        %d\n", numG)
	fmt.Printf("ops per goroutine: %d\n", opsPerG)
	fmt.Printf("total pops:        %d\n", totalPops.Load())
	fmt.Printf("alloc from new:    %d\n", fromNew)
	fmt.Printf("alloc from free:   %d\n", fromFree)
	if total > 0 {
		pct := float64(fromFree) * 100.0 / float64(total)
		fmt.Printf("free list reuse:   %.1f%%\n", pct)
	}
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Missing the Re-Validation Step

Wrong:
```go
top := s.top.Load()
r.Protect(0, top)
next := top.next.Load() // top may have been recycled between Load and Protect
```

What happens: another goroutine may retire and recycle `top` between the Load and the Protect. The atomic store of the hazard pointer does not retroactively prevent a Scan that already ran.

Fix: re-read the shared pointer after Protect and compare:
```go
r.Protect(0, top)
if s.top.Load() != top {
    r.Release(0)
    continue
}
```

### Reading Val After Releasing Protection or After the CAS Without Protection

Wrong (reads after Retire):
```go
if s.top.CompareAndSwap(top, next) {
    r.Release(0)
    r.Retire(top)               // poisons top.Val with recycledSentinel
    return top.Val.Load(), true // reads the sentinel, not the original value
}
```

Also wrong (reads after CAS with hp still set, but -race sees stale sentinel):
```go
if s.top.CompareAndSwap(top, next) {
    val := top.Val.Load()       // theoretically safe, but TSan observes stale write
    r.Release(0)
    r.Retire(top)
    return val, true
}
```

What happens: in the first form, `Retire` poisons `Val` before the read, returning the sentinel. In the second form, the CAS is the sole memory fence; with the Go race detector (TSan) active, TSan may observe a sentinel written by a prior Retire on a recycled node instead of the value written by the most recent Alloc.

Fix: read `Val` while the hazard pointer is still set and the node is still on the stack, before the CAS. If the CAS fails, discard and retry. Retire only runs after a successful CAS, so a discarded read can never return the sentinel.
```go
val := top.Val.Load()               // read while hp[0] = top, node on stack
next := top.next.Load()
if s.top.CompareAndSwap(top, next) {
    r.Release(0)
    r.Retire(top)
    return val, true
}
```

### Sharing a `*Record` Between Goroutines

Wrong: two goroutines call `Push` and `Pop` using the same `*Record`.

What happens: `r.retired` is a plain slice with no synchronization. Concurrent appends race; the race detector reports a data race on the slice header.

Fix: each goroutine calls `d.Register()` and holds its own `*Record`. The Domain coordinates Scan across all records without sharing the record internals.

### Omitting `Unregister` on Goroutine Exit

Wrong: a goroutine calls `Register` and exits without `Unregister`.

What happens: the record's `active` flag stays true and its hazard pointer slots may point to addresses that are no longer valid. Future Scans see those stale pointers and never reclaim the corresponding retired nodes. Memory retention grows unboundedly as goroutines create and exit.

Fix: `defer r.Unregister()` immediately after `Register`.

## Verification

From `~/go-exercises/hazptr`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

The race detector is the primary correctness check: any missing atomic operation or incorrect ordering is detected. The demo shows the free-list reuse percentage; expect above 50% after warm-up with 8 goroutines and 10,000 operations each.

## Summary

- Hazard pointers require each goroutine to publish the addresses it is currently dereferencing; those addresses are visible to all goroutines during Scan.
- The protect-validate loop is mandatory: protect the pointer, re-read the shared location, compare before dereferencing.
- `Retire` poisons `Val` (via `Val.Store(recycledSentinel)`) so concurrent tests detect use-after-recycle if protection fails.
- `Node.Val` must be `atomic.Int64`: the protocol is semantically correct with plain reads and writes, but Go's race detector requires per-variable atomicity for any field accessed by multiple goroutines.
- `Scan` is O(P + R); it runs infrequently at the retire threshold and is expected to reclaim most of the retired list each time.
- The free list is a lock-free Treiber stack; `AllocStats` confirms reuse after warm-up.
- Each goroutine holds its own `*Record`; `defer r.Unregister()` returns it to the pool on exit.

## What's Next

Next: [Epoch-Based Memory Reclamation](../04-epoch-based-reclamation/04-epoch-based-reclamation.md).

## Resources

- [Maged M. Michael, "Hazard Pointers: Safe Memory Reclamation for Lock-Free Objects", IEEE TPDS 2004](https://ieeexplore.ieee.org/document/1291819)
- [Go Memory Model](https://go.dev/ref/mem) — formal rules governing atomic operations and the happens-before relation in Go
- [sync/atomic package](https://pkg.go.dev/sync/atomic) — `atomic.Pointer[T]` (Go 1.19+), CompareAndSwap, load/store semantics
- [Treiber, R.K., "Systems Programming: Coping with Parallelism", IBM RJ5118, 1986](https://dominoweb.draco.res.ibm.com/reports/rj5118.pdf) — the original lock-free stack algorithm
- [The Art of Multiprocessor Programming, Herlihy and Shavit, 2nd ed. 2020](https://www.sciencedirect.com/book/9780124159501/the-art-of-multiprocessor-programming) — Chapter 10 covers memory reclamation in depth
