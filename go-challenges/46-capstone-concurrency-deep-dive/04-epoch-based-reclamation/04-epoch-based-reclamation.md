# 4. Epoch-Based Memory Reclamation

Epoch-based reclamation (EBR) solves a fundamental problem in lock-free programming: a goroutine may load a pointer to a node and be preempted before it reads the node's fields. If a concurrent goroutine retires and recycles that node, the first goroutine dereferences stale memory. EBR prevents this by deferring reclamation until every registered goroutine has passed through at least one quiescent point (an `Unpin`) since the node was retired. The protocol is cheaper per operation than hazard pointers because there is no per-access pointer publication, but it requires that goroutines do not block inside a critical section.

```text
ebr/
  go.mod
  ebr.go
  list.go
  ebr_test.go
  cmd/demo/main.go
```

## Concepts

### The Memory Reclamation Problem

In a lock-free linked list, deletion is two-phased: first mark the node logically deleted (so concurrent readers skip it), then physically unlink and reclaim it. The timing hazard: goroutine A loads a node's `next` pointer and is descheduled; goroutine B logically deletes and physically reclaims the node; goroutine A resumes and dereferences freed memory.

The Go garbage collector eliminates use-after-free for heap objects reachable through Go pointers, but the problem applies whenever nodes are returned to a pool, zeroed, or have a destructor invoked. Without a safe-reclamation protocol, a pool's `Get` may return an object another goroutine is still reading.

### Three-Epoch Protocol

EBR divides time into epochs — a monotonically increasing counter that wraps at three values (0, 1, 2). Three values suffice because of the following invariant:

When the global epoch is E, every active goroutine recorded local epoch E at the time of its most recent `Pin`. Therefore no active goroutine can be reading an object retired in epoch E-2: that object was retired at least two full epoch advances ago, and every goroutine has since passed through epochs E-1 and E.

A goroutine enters a critical section with `Pin()`, which:
1. Records the current global epoch as its local epoch.
2. Sets `active = true` with release semantics.

It exits with `Unpin()`, which clears `active`.

`Retire(ptr, reclaim)` appends a pointer to the retirement list for the current global epoch.

`TryAdvance()` inspects all registered goroutines. If every active goroutine has a local epoch equal to the global epoch, the global epoch advances by one (mod 3) and all items in the retirement list from epoch (cur-1) mod 3 are reclaimed — those items were retired at least two advances ago.

### The Advancement Invariant

Let `cur` be the global epoch before an advance call. The condition "all active goroutines have local epoch == cur" guarantees no goroutine is still in a critical section that started in epoch `cur-1` or earlier. After advancing from `cur` to `(cur+1)%3`, items retired in epoch `(cur-1+3)%3` are safe to reclaim.

```
epoch sequence:   ... 1  →  2  →  0  →  1  ...
retire node X:             here
can reclaim X:                        here (two full advances later)
```

A concrete trace:
- Global epoch 0. Goroutine A retires node X to lists[0]. Global advances to 1. Lists[2] reclaimed (empty). Global advances to 2. Lists[0] (containing X) reclaimed. X is safe.

### EBR vs Hazard Pointers

| Property             | Hazard pointers             | EBR                          |
|----------------------|-----------------------------|------------------------------|
| Per-access cost      | 1 store + 1 load barrier    | 0 (epoch recorded at pin)    |
| Bounded reclamation  | Yes (O(threads) per retire) | No (stalled goroutine blocks)|
| Stall sensitivity    | Tolerant                    | Blocked by one stalled goroutine |
| Implementation cost  | More complex (per-slot scan)| Simpler (epoch comparison)   |

### Failure Modes

**Stalled goroutines.** A goroutine that calls `Pin()` and then blocks on a channel, a slow syscall, or a long loop holds the epoch fixed and prevents `TryAdvance()` from succeeding, so retired nodes accumulate. Keep critical sections short and never block while pinned.

**Epoch starvation under low load.** `TryAdvance()` only runs when called explicitly. A background `Collector` goroutine that calls `TryAdvance()` on a timer prevents unbounded accumulation.

**Goroutine-local state sharing.** Each goroutine must own its `*ThreadState`. Sharing one `ThreadState` across goroutines produces racing writes to `active` and `localEpoch`, breaking the epoch tracking invariant.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/ebr/cmd/demo
cd ~/go-exercises/ebr
go mod init example.com/ebr
```

This is a library; verification is through `go test`.

### Exercise 1: Core EBR Types and the Pin/Unpin Protocol

Create `ebr.go`:

```go
package ebr

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

const numEpochs = 3

// EBR manages an epoch-based reclamation domain. Create one with New.
type EBR struct {
	epoch   atomic.Uint64
	mu      sync.Mutex
	threads []*ThreadState
}

// New returns a ready-to-use EBR domain with epoch 0.
func New() *EBR { return &EBR{} }

// Epoch returns the current global epoch (0, 1, or 2).
func (e *EBR) Epoch() uint64 { return e.epoch.Load() }

// retiredItem is a pointer deferred for reclamation along with its cleanup func.
type retiredItem struct {
	ptr     unsafe.Pointer
	reclaim func(unsafe.Pointer)
}

// ThreadState is the per-goroutine state for EBR participation. Each goroutine
// must call EBR.Register once and hold the returned *ThreadState for the
// lifetime of its participation in the domain. ThreadState must not be
// shared across goroutines.
type ThreadState struct {
	ebr        *EBR
	localEpoch atomic.Uint64
	active     atomic.Bool
	mu         sync.Mutex
	lists      [numEpochs][]retiredItem
}

// Register adds a new goroutine to the EBR domain and returns its per-goroutine
// state. Call this once per goroutine before the first Pin.
func (e *EBR) Register() *ThreadState {
	ts := &ThreadState{ebr: e}
	e.mu.Lock()
	e.threads = append(e.threads, ts)
	e.mu.Unlock()
	return ts
}

// Pin enters a critical section. The returned Guard must be released with
// Unpin (typically via defer) before the goroutine blocks or exits the
// operation. Nested pins are not supported.
func (ts *ThreadState) Pin() *Guard {
	cur := ts.ebr.epoch.Load()
	ts.localEpoch.Store(cur)
	ts.active.Store(true)
	return &Guard{ts: ts}
}

// Guard is active for the duration of one critical section.
// Obtain one with ThreadState.Pin; release it with Unpin.
type Guard struct {
	ts *ThreadState
}

// Unpin exits the critical section. After Unpin the goroutine must not
// access any pointer it loaded while pinned.
func (g *Guard) Unpin() {
	g.ts.active.Store(false)
}

// Retire schedules ptr for reclamation. reclaim is called with ptr once
// it is safe to do so (after two epoch advances). ptr must not be accessed
// after Retire returns.
func (g *Guard) Retire(ptr unsafe.Pointer, reclaim func(unsafe.Pointer)) {
	e := g.ts.ebr.epoch.Load() % numEpochs
	g.ts.mu.Lock()
	g.ts.lists[e] = append(g.ts.lists[e], retiredItem{ptr, reclaim})
	g.ts.mu.Unlock()
}
```

`localEpoch` is stored before `active` is set to true. `TryAdvance` reads `active` first (acquire) then `localEpoch`, so the ordering guarantees `TryAdvance` always sees a valid local epoch for any goroutine it observes as active.

### Exercise 2: Epoch Advancement and Reclamation

Add to `ebr.go`:

```go
// TryAdvance attempts to advance the global epoch. It succeeds when every
// registered goroutine is either inactive or has its local epoch equal to
// the current global epoch. On success it reclaims all items retired two
// epochs ago and returns true.
func (e *EBR) TryAdvance() bool {
	cur := e.epoch.Load()

	e.mu.Lock()
	for _, ts := range e.threads {
		if ts.active.Load() && ts.localEpoch.Load() != cur {
			e.mu.Unlock()
			return false
		}
	}
	threads := e.threads
	e.epoch.Store((cur + 1) % numEpochs)
	e.mu.Unlock()

	// Reclaim items retired two epochs back. When advancing from cur to
	// cur+1, every goroutine that was active in epoch (cur-1) has since
	// been observed at epoch cur, so those nodes are unreachable.
	reclaimEpoch := (cur + numEpochs - 1) % numEpochs
	for _, ts := range threads {
		ts.mu.Lock()
		items := ts.lists[reclaimEpoch]
		ts.lists[reclaimEpoch] = ts.lists[reclaimEpoch][:0]
		ts.mu.Unlock()
		for _, item := range items {
			item.reclaim(item.ptr)
		}
	}
	return true
}
```

The epoch is stored while `e.mu` is held so no goroutine can observe the new epoch before all threads have been validated. Reclamation runs outside the lock to avoid calling user code while holding it.

### Exercise 3: Lock-Free List Backed by EBR

The list uses logical deletion: a node is marked with an `atomic.Bool` before being retired to EBR. Physical unlinking happens lazily during the next traversal. Goroutines pin the epoch on entry and retire logically-deleted nodes from `Delete`.

Create `list.go`:

```go
package ebr

import (
	"sync/atomic"
	"unsafe"
)

// node is one element of the lock-free sorted linked list.
type node struct {
	key     int
	val     any
	deleted atomic.Bool
	next    atomic.Pointer[node]
}

// List is a sorted, lock-free singly-linked list backed by EBR.
// Keys are unique integers; negative keys are valid.
type List struct {
	head *node // sentinel; key is min-int; never retired
	ebr  *EBR
}

// NewList returns an empty List using the given EBR domain.
func NewList(e *EBR) *List {
	head := &node{key: -(1 << 62)}
	return &List{head: head, ebr: e}
}

// find returns the predecessor and current node for key, physically
// removing logically-deleted nodes encountered during traversal.
// The caller must be pinned for the duration of the call.
func (l *List) find(key int) (pred *node, curr *node) {
retry:
	pred = l.head
	curr = pred.next.Load()
	for curr != nil {
		succ := curr.next.Load()
		if curr.deleted.Load() {
			if !pred.next.CompareAndSwap(curr, succ) {
				goto retry
			}
			curr = succ
			continue
		}
		if curr.key >= key {
			return pred, curr
		}
		pred = curr
		curr = succ
	}
	return pred, nil
}

// Insert adds key/val to the list. Returns true if the key was inserted,
// false if it already existed.
func (l *List) Insert(ts *ThreadState, key int, val any) bool {
	g := ts.Pin()
	defer g.Unpin()

	for {
		pred, curr := l.find(key)
		if curr != nil && curr.key == key {
			return false
		}
		n := &node{key: key, val: val}
		n.next.Store(curr)
		if pred.next.CompareAndSwap(curr, n) {
			return true
		}
	}
}

// Delete removes key from the list and retires the node to EBR.
// Returns true if the key existed.
func (l *List) Delete(ts *ThreadState, key int) bool {
	g := ts.Pin()
	defer g.Unpin()

	for {
		_, curr := l.find(key)
		if curr == nil || curr.key != key {
			return false
		}
		// Logical deletion: mark the node. If another goroutine beats us, retry.
		if !curr.deleted.CompareAndSwap(false, true) {
			continue
		}
		// Retire the node. reclaim is a no-op here because the Go GC manages
		// the heap; replace it with a pool.Put to enable node reuse.
		g.Retire(unsafe.Pointer(curr), func(_ unsafe.Pointer) {})
		// Drive physical removal; if the CAS races, find will clean up later.
		l.find(key)
		return true
	}
}

// Find returns the value for key and true if the key exists.
func (l *List) Find(ts *ThreadState, key int) (any, bool) {
	g := ts.Pin()
	defer g.Unpin()

	_, curr := l.find(key)
	if curr != nil && curr.key == key {
		return curr.val, true
	}
	return nil, false
}

// Len returns the count of live (non-deleted) nodes.
func (l *List) Len(ts *ThreadState) int {
	g := ts.Pin()
	defer g.Unpin()

	n := 0
	curr := l.head.next.Load()
	for curr != nil {
		if !curr.deleted.Load() {
			n++
		}
		curr = curr.next.Load()
	}
	return n
}
```

### Exercise 4: Tests and Example

Create `ebr_test.go`:

```go
package ebr

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestEpochStartsAtZero(t *testing.T) {
	t.Parallel()
	e := New()
	if got := e.Epoch(); got != 0 {
		t.Fatalf("initial epoch = %d, want 0", got)
	}
}

func TestTryAdvanceNoThreads(t *testing.T) {
	t.Parallel()
	e := New()
	if !e.TryAdvance() {
		t.Fatal("TryAdvance with no threads should succeed")
	}
	if got := e.Epoch(); got != 1 {
		t.Fatalf("epoch after one advance = %d, want 1", got)
	}
}

func TestTryAdvanceWithInactiveThread(t *testing.T) {
	t.Parallel()
	e := New()
	ts := e.Register()
	g := ts.Pin()
	g.Unpin()
	if !e.TryAdvance() {
		t.Fatal("TryAdvance with inactive thread should succeed")
	}
	if got := e.Epoch(); got != 1 {
		t.Fatalf("epoch = %d, want 1", got)
	}
}

// TestTryAdvanceBlockedByStaleThread verifies that a goroutine pinned at
// an old epoch blocks subsequent advances beyond the first.
func TestTryAdvanceBlockedByStaleThread(t *testing.T) {
	t.Parallel()
	e := New()
	ts := e.Register()

	// Pin at epoch 0. First advance checks localEpoch(0) == cur(0): passes.
	g := ts.Pin()
	defer g.Unpin()

	if !e.TryAdvance() {
		t.Fatal("first TryAdvance should succeed: localEpoch(0) == cur(0)")
	}
	// Global is now 1. Thread is still pinned with localEpoch 0 != cur 1.
	if e.TryAdvance() {
		t.Fatal("second TryAdvance should fail: pinned thread has stale localEpoch")
	}
}

func TestEpochWrapsAtThree(t *testing.T) {
	t.Parallel()
	e := New()
	for range 9 {
		e.TryAdvance()
	}
	if got := e.Epoch(); got != 0 {
		t.Fatalf("epoch after 9 advances = %d, want 0 (wraps mod 3)", got)
	}
}

func TestRetireAndReclaim(t *testing.T) {
	t.Parallel()
	e := New()
	ts := e.Register()

	var reclaimCount atomic.Int64
	sentinel := new(int)

	func() {
		g := ts.Pin()
		defer g.Unpin()
		g.Retire(unsafe.Pointer(sentinel), func(_ unsafe.Pointer) {
			reclaimCount.Add(1)
		})
	}()

	// Advance 1: epoch 0 → 1; reclaimEpoch = (0+2)%3 = 2; nothing to reclaim.
	if !e.TryAdvance() {
		t.Fatal("first TryAdvance failed")
	}
	if n := reclaimCount.Load(); n != 0 {
		t.Fatalf("item reclaimed too early: count = %d after 1 advance", n)
	}

	// Advance 2: epoch 1 → 2; reclaimEpoch = (1+2)%3 = 0; reclaims our item.
	if !e.TryAdvance() {
		t.Fatal("second TryAdvance failed")
	}
	if n := reclaimCount.Load(); n != 1 {
		t.Fatalf("reclaimCount = %d after 2 advances, want 1", n)
	}
}

func TestListInsertFindDelete(t *testing.T) {
	t.Parallel()
	e := New()
	ts := e.Register()
	l := NewList(e)

	if !l.Insert(ts, 10, "ten") {
		t.Fatal("Insert(10) should return true")
	}
	if l.Insert(ts, 10, "dup") {
		t.Fatal("duplicate Insert(10) should return false")
	}
	if v, ok := l.Find(ts, 10); !ok || v != "ten" {
		t.Fatalf("Find(10) = %v, %v; want ten, true", v, ok)
	}
	if !l.Delete(ts, 10) {
		t.Fatal("Delete(10) should return true")
	}
	if _, ok := l.Find(ts, 10); ok {
		t.Fatal("Find(10) after Delete should return false")
	}
	if l.Delete(ts, 10) {
		t.Fatal("second Delete(10) should return false")
	}
}

func TestListLen(t *testing.T) {
	t.Parallel()
	e := New()
	ts := e.Register()
	l := NewList(e)

	for i := range 10 {
		l.Insert(ts, i, i)
	}
	if n := l.Len(ts); n != 10 {
		t.Fatalf("Len = %d after 10 inserts, want 10", n)
	}
	for i := range 5 {
		l.Delete(ts, i)
	}
	if n := l.Len(ts); n != 5 {
		t.Fatalf("Len = %d after 5 deletes, want 5", n)
	}
}

func TestListConcurrent(t *testing.T) {
	t.Parallel()
	const goroutines = 16
	const opsPerGoroutine = 200

	e := New()
	l := NewList(e)

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				e.TryAdvance()
				runtime.Gosched()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ts := e.Register()
			for j := range opsPerGoroutine {
				key := (id*opsPerGoroutine + j) % 64
				switch j % 3 {
				case 0:
					l.Insert(ts, key, id)
				case 1:
					l.Find(ts, key)
				case 2:
					l.Delete(ts, key)
				}
				runtime.Gosched()
			}
		}(i)
	}
	wg.Wait()
	close(stop)
}

func ExampleEBR_TryAdvance() {
	e := New()
	ts := e.Register()

	// Pin and immediately unpin: goroutine is quiescent.
	g := ts.Pin()
	g.Unpin()

	advanced := e.TryAdvance()
	fmt.Println("advanced:", advanced)
	fmt.Println("epoch:", e.Epoch())
	// Output:
	// advanced: true
	// epoch: 1
}
```

Your turn: add `TestListInsertOrder` that inserts keys 5, 3, 7, 1, 9 (out of order) and then calls `Find` for each, asserting all are found. Verify that `Len` returns 5 after all inserts.

### Exercise 5: Command-Line Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"

	"example.com/ebr"
)

func main() {
	e := ebr.New()
	l := ebr.NewList(e)

	// Background collector advances epochs and reclaims retired nodes.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				e.TryAdvance()
				runtime.Gosched()
			}
		}
	}()

	const workers = 4
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ts := e.Register()
			for j := range 100 {
				key := (id*100 + j) % 20
				if j%2 == 0 {
					l.Insert(ts, key, id)
				} else {
					l.Delete(ts, key)
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)

	ts := e.Register()
	fmt.Printf("final epoch: %d\n", e.Epoch())
	fmt.Printf("final list length: %d\n", l.Len(ts))
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

**Wrong: blocking inside a critical section.**

```go
g := ts.Pin()
result := <-ch  // blocks indefinitely; active flag stays true
g.Unpin()
```

What happens: a blocked goroutine with `active = true` and a stale `localEpoch` prevents `TryAdvance()` from succeeding. Retired nodes accumulate without bound.

Fix: receive from the channel before pinning, or copy the needed data out and unpin before blocking.

**Wrong: reading a pointer after Unpin.**

```go
g := ts.Pin()
n := list.head.next.Load()
g.Unpin()
use(n.val) // n may have been reclaimed
```

What happens: a concurrent `Delete` may retire `n` and invoke its reclaim function (e.g., `pool.Put(n)`) after `Unpin`; a subsequent `pool.Get` returns the same node to another goroutine. Both goroutines now read the same node, which the race detector catches.

Fix: copy `n.val` into a local variable before calling `Unpin`.

**Wrong: sharing one ThreadState across goroutines.**

```go
ts := e.Register()
go func() { g := ts.Pin(); defer g.Unpin(); ... }()
go func() { g := ts.Pin(); defer g.Unpin(); ... }()
```

What happens: two goroutines race on `ts.active` and `ts.localEpoch`. One goroutine's `Unpin` clears `active` while the other is still in a critical section, making the epoch tracking incorrect and the protection unsound.

Fix: each goroutine calls `e.Register()` and holds its own `*ThreadState`.

**Wrong: never calling TryAdvance.**

Retiring nodes without calling `TryAdvance()` means retirement lists grow forever; the domain never reclaims memory.

Fix: run a background goroutine that calls `e.TryAdvance()` on a short timer (1-10 ms is typical).

## Verification

From `~/go-exercises/ebr`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Add `TestListInsertOrder` (the "your turn" exercise) before running the suite. The race detector is mandatory for lock-free code: any unsafe concurrent access produces a finding even when tests otherwise pass.

## Summary

- EBR defers reclamation until every registered goroutine has passed through a quiescent point since the node was retired.
- Three epochs suffice: when advancing from E to E+1, items retired in epoch (E-1) mod 3 are safe because every goroutine active in that epoch has since been observed at epoch E.
- `Pin` records the global epoch and marks the goroutine active; `Unpin` clears the flag. Keep critical sections short and never block while pinned.
- `TryAdvance` checks all registered goroutines for a current-epoch observation, advances the epoch, and reclaims the two-epoch-old retirement list.
- EBR trades bounded reclamation (hazard pointers guarantee O(threads) reclamation per retire) for lower per-access overhead and a simpler implementation.

## What's Next

Next: [Work-Stealing Deque](../05-work-stealing-deque/05-work-stealing-deque.md).

## Resources

- Keir Fraser, "Practical Lock-Freedom" (PhD thesis, Cambridge, 2004), Chapter 5 -- defines the three-epoch EBR protocol. https://www.cl.cam.ac.uk/techreports/UCAM-CL-TR-579.pdf
- Hart, T., McKenney, P., Brown, A., Walpole, J., "Performance of Memory Reclamation for Lockless Synchronization" (2007) -- empirical comparison of EBR, hazard pointers, and reference counting.
- Go `sync/atomic` package -- canonical reference for `atomic.Uint64`, `atomic.Bool`, `atomic.Pointer[T]`. https://pkg.go.dev/sync/atomic
- Timothy L. Harris, "A Pragmatic Implementation of Non-Blocking Linked-Lists" (DISC 2001) -- the logical-deletion traversal pattern used in Exercise 3.
- crossbeam-epoch crate -- https://docs.rs/crossbeam-epoch/latest/crossbeam_epoch/ -- a production EBR implementation; the Guard/Pin/Retire API in this lesson mirrors its design.
