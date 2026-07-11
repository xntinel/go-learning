# 11. Double Buffering for Concurrent Read/Write

Double buffering allows a single writer to replace an entire data structure atomically while hundreds of reader goroutines access the current version without locks, blocking, or copying. The hard part is what happens after the atomic swap: the writer must not touch the old buffer until every reader that started on it has finished. Getting that drain correct, at low latency, requires sharded reference counts and a careful ordering of atomic operations that most introductions skip.

## Concepts

### The Model

Two slots hold independent copies of the data. An atomic index (`front`) records which slot is current. Readers announce themselves by incrementing a per-slot sharded counter, re-check that `front` has not moved since they loaded it, read, then decrement. The writer modifies the back slot (the one not pointed to by `front`), atomically promotes it, then spins on the old front's counters until all announced readers are gone. Only then is the old slot available for the next write.

```
slot[0]                        slot[1]
+-----------------------+      +-----------------------+
| data                  |      | data  (draft)         |
| readers[0..63]        |      | readers[0..63]        |
+-----------------------+      +-----------------------+
        ^                               ^
        | front = 0               back = 1
    readers use                 writer modifies

After atomic Store(front, 1):

slot[0] (draining)            slot[1] (new front)
writer waits here             readers use
```

### Why Sharded Counters

A single `atomic.Int64` for the reader count is correct but creates cache-line contention: every Read and ReadDone writes the same 64-byte cache line. With 64 concurrent readers, all goroutines fight for ownership of that line and throughput degrades to roughly single-threaded — the same bottleneck as a mutex.

The fix is to stripe the count across N padded counters, one per cache line. Each reader picks a stripe with a fast mod operation. Readers in different stripes never contend. The writer sums N values once per swap, which is rare relative to the read rate.

### The Swap-Check Loop

The critical window is between a reader loading `front` and incrementing the counter for that slot. If a swap occurs inside that window, the reader registers on a slot the writer already considers free:

```
Reader                          Writer
------                          ------
i := front.Load()   // = 0
                                front.Store(1)   // swap
                                drainSlot(0)     // counts all zero: OK to reuse
slots[0].count.Add(+1)          // reader increments old slot AFTER writer cleared it
```

The fix is to re-check `front` after incrementing. If it has changed, undo and retry:

```
i := front.Load()
slots[i].count.Add(+1)
if front.Load() != i {
    slots[i].count.Add(-1)
    retry
}
// safe to read
```

Because the writer calls `Write` sequentially (one swap per call) and must drain before returning, a reader retries at most once in practice.

### Failure Modes

**Writer modifies the back slot while a reader holds a pointer to it.** The contract is that Write has exclusive access to the back slot because no reader can be on it: the writer drained the back slot during the previous Write call before returning. If Write is called concurrently from two goroutines, both goroutines call fn on the same back slot — a data race. Write must be serialized at the call site.

**Reader retains the pointer after fn returns.** The callback contract requires the pointer to be used only inside fn. A reader that stores the pointer and uses it after fn returns may observe data being modified by the writer. The Go race detector catches this.

**Slow readers cause writer spin.** If a reader performs I/O inside Read, the writer spins on the drain loop for the duration. Keep Read callbacks short: load the values you need, copy them, return. Never perform blocking operations inside fn.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/doublebuf/cmd/demo
cd ~/go-exercises/doublebuf
go mod init example.com/doublebuf
```

### Exercise 1: Core Types and Constructor

Create `doublebuf.go`:

```go
package doublebuf

import (
	"runtime"
	"sync/atomic"
)

// numShards is the number of per-slot reader-count stripes.
// 64 stripes eliminate false sharing across GOMAXPROCS up to 64.
const numShards = 64

// paddedInt64 wraps atomic.Int64 with enough padding to occupy at least
// one 64-byte cache line, preventing false sharing between adjacent stripes.
// A generous pad is used because the exact size of atomic.Int64 is
// implementation-defined; production code would compute the pad with
// unsafe.Sizeof.
type paddedInt64 struct {
	n atomic.Int64
	_ [64]byte
}

// slot holds one copy of T and the sharded reader-count stripes for that slot.
type slot[T any] struct {
	data    T
	readers [numShards]paddedInt64
}

// Buffer is a lock-free double buffer for a single writer and multiple readers.
// The zero value is not usable; construct with New.
type Buffer[T any] struct {
	slots [2]slot[T]
	front atomic.Uint32 // 0 or 1: index of the current read slot
	seq   atomic.Uint64 // monotonic counter for stripe selection
}

// New returns a Buffer with both slots initialized to a copy of init.
// Initializing both slots means the very first Write only needs to update
// the back slot, not copy the initial value forward.
func New[T any](init T) *Buffer[T] {
	b := &Buffer[T]{}
	b.slots[0].data = init
	b.slots[1].data = init
	return b
}
```

### Exercise 2: Lock-Free Read

Add to `doublebuf.go`:

```go
// Read calls fn with a pointer to the current read buffer.
// Multiple goroutines may call Read concurrently without blocking each other.
// fn must not retain the pointer after it returns.
func (b *Buffer[T]) Read(fn func(*T)) {
	stripe := int(b.seq.Add(1) % numShards)
	for {
		i := b.front.Load()
		b.slots[i].readers[stripe].n.Add(1)
		if b.front.Load() == i {
			// Announced ourselves on slot i before any swap: safe to read.
			fn(&b.slots[i].data)
			b.slots[i].readers[stripe].n.Add(-1)
			return
		}
		// A swap occurred between our Load and our Add.
		// Undo and retry so we register on the new front slot.
		b.slots[i].readers[stripe].n.Add(-1)
		runtime.Gosched()
	}
}
```

### Exercise 3: Exclusive Write and Atomic Swap

Add to `doublebuf.go`:

```go
// Write calls fn with a pointer to the back buffer for exclusive modification.
// After fn returns, Write atomically promotes the back buffer to front and
// waits until all readers of the former front have finished before returning.
// Write must not be called concurrently with itself.
func (b *Buffer[T]) Write(fn func(*T)) {
	front := b.front.Load()
	back := 1 - front

	fn(&b.slots[back].data)

	// Promote the modified back buffer.
	b.front.Store(back)

	// Drain all readers that were announced on the old front slot.
	// Until their counts reach zero the old slot must not be modified.
	b.drainSlot(int(front))
}

// drainSlot spins until the sum of all stripe counts for slot i is zero.
func (b *Buffer[T]) drainSlot(i int) {
	for {
		var total int64
		for s := 0; s < numShards; s++ {
			total += b.slots[i].readers[s].n.Load()
		}
		if total == 0 {
			return
		}
		runtime.Gosched()
	}
}

// ActiveReaders returns the total instantaneous reader count across both slots.
// Not atomic across slots; use for tests and monitoring only.
func (b *Buffer[T]) ActiveReaders() int64 {
	var total int64
	for i := 0; i < 2; i++ {
		for s := 0; s < numShards; s++ {
			total += b.slots[i].readers[s].n.Load()
		}
	}
	return total
}
```

### Exercise 4: Tests

Create `doublebuf_test.go`:

```go
package doublebuf

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewInitializesBothSlots(t *testing.T) {
	t.Parallel()

	buf := New("hello")
	if buf.slots[0].data != "hello" || buf.slots[1].data != "hello" {
		t.Fatalf("slots = %q %q, want both \"hello\"",
			buf.slots[0].data, buf.slots[1].data)
	}
	if buf.front.Load() != 0 {
		t.Fatalf("initial front = %d, want 0", buf.front.Load())
	}
}

func TestReadSeesInitialValue(t *testing.T) {
	t.Parallel()

	buf := New(42)
	var got int
	buf.Read(func(v *int) { got = *v })
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
}

func TestWriteUpdatesValue(t *testing.T) {
	t.Parallel()

	buf := New(0)
	buf.Write(func(v *int) { *v = 99 })
	var got int
	buf.Read(func(v *int) { got = *v })
	if got != 99 {
		t.Fatalf("got %d, want 99", got)
	}
}

func TestMultipleWritesAreVisible(t *testing.T) {
	t.Parallel()

	buf := New(0)
	for i := 1; i <= 10; i++ {
		buf.Write(func(v *int) { *v = i })
		var got int
		buf.Read(func(v *int) { got = *v })
		if got != i {
			t.Fatalf("after write %d: got %d", i, got)
		}
	}
}

func TestWriterWaitsForSlotToDrain(t *testing.T) {
	t.Parallel()

	buf := New(0)
	readHeld := make(chan struct{})
	readRelease := make(chan struct{})
	writerDone := make(chan struct{})

	// Reader acquires the front slot and holds it until signaled.
	go func() {
		buf.Read(func(_ *int) {
			close(readHeld)
			<-readRelease
		})
	}()

	// Wait for the reader to be inside fn (slot announced).
	<-readHeld

	// Writer must drain the old front before Write returns.
	go func() {
		buf.Write(func(v *int) { *v = 1 })
		close(writerDone)
	}()

	// The writer swaps immediately but must block on drain.
	// writerDone must NOT fire while the reader holds the slot.
	select {
	case <-writerDone:
		t.Fatal("writer returned before reader released the slot")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the reader; drain completes; writer should finish shortly.
	close(readRelease)
	select {
	case <-writerDone:
		// OK
	case <-time.After(500 * time.Millisecond):
		t.Fatal("writer did not return after reader released slot")
	}

	var got int
	buf.Read(func(v *int) { got = *v })
	if got != 1 {
		t.Fatalf("after write: got %d, want 1", got)
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()

	type state struct {
		gen   int
		value int
	}

	buf := New(state{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64

	// 32 concurrent readers verify the gen==value invariant on every read.
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				buf.Read(func(s *state) {
					if s.gen != s.value {
						t.Errorf("invariant broken: gen=%d value=%d", s.gen, s.value)
					}
					reads.Add(1)
				})
			}
		}()
	}

	// Single writer performs 200 swaps.
	for i := 1; i <= 200; i++ {
		buf.Write(func(s *state) {
			s.gen = i
			s.value = i
		})
	}
	close(stop)
	wg.Wait()

	t.Logf("total reads: %d", reads.Load())
}

func ExampleNew() {
	buf := New("initial")
	buf.Write(func(s *string) { *s = "updated" })
	buf.Read(func(s *string) { fmt.Println(*s) })
	// Output: updated
}
```

Your turn: add `TestReadDoesNotBlockOtherReaders` that starts 10 goroutines each calling `Read` concurrently and asserts all 10 complete within 200 ms (readers must never block each other).

### Exercise 5: Demo Program

Create `cmd/demo/main.go`. Because `cmd/demo` is a separate `package main`, it uses only the exported API (`New`, `Read`, `Write`, `ActiveReaders`):

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/doublebuf"
)

// RoutingTable maps CIDR prefixes to next-hop addresses.
type RoutingTable struct {
	Routes map[string]string
	Gen    int
}

func main() {
	initial := RoutingTable{
		Routes: map[string]string{
			"10.0.0.0/8":     "gw-a",
			"172.16.0.0/12":  "gw-b",
			"192.168.0.0/16": "gw-c",
		},
		Gen: 0,
	}

	buf := doublebuf.New(initial)

	var lookups atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 8 reader goroutines perform continuous prefix lookups.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				buf.Read(func(rt *RoutingTable) {
					_ = rt.Routes["10.0.0.0/8"]
					lookups.Add(1)
				})
			}
		}()
	}

	// Writer rebuilds the routing table from scratch every 100 ms.
	// Rebuilding from scratch (not mutating the live table) is the
	// correct pattern: the back slot is the writer's exclusive workspace.
	for gen := 1; gen <= 5; gen++ {
		time.Sleep(100 * time.Millisecond)
		g := gen
		buf.Write(func(rt *RoutingTable) {
			rt.Gen = g
			rt.Routes = map[string]string{
				"10.0.0.0/8":     fmt.Sprintf("gw-gen%d", g),
				"172.16.0.0/12":  "gw-b",
				"192.168.0.0/16": "gw-c",
			}
		})
		fmt.Printf("generation %d swapped in (active readers: %d)\n",
			gen, buf.ActiveReaders())
	}

	close(stop)
	wg.Wait()

	var finalGen int
	buf.Read(func(rt *RoutingTable) { finalGen = rt.Gen })
	fmt.Printf("total lookups: %d  final generation: %d\n", lookups.Load(), finalGen)
}
```

Run the demo:

```bash
go run ./cmd/demo
```

Expected shape of output (lookup count varies by CPU speed):

```
generation 1 swapped in (active readers: 0)
generation 2 swapped in (active readers: 0)
generation 3 swapped in (active readers: 0)
generation 4 swapped in (active readers: 0)
generation 5 swapped in (active readers: 0)
total lookups: <N>  final generation: 5
```

Active readers at the point `ActiveReaders` is called is typically 0 because the writer has already completed the drain. The lookup count reflects throughput: on a 4-core machine with 8 reader goroutines and 500 ms of reading time, expect tens of millions of lookups.

## Common Mistakes

**Wrong: announce after reading.**

```go
// Wrong: data is read before the reader count is registered.
i := front.Load()
fn(&slots[i].data)       // writer may modify this concurrently
slots[i].count.Add(+1)   // too late
slots[i].count.Add(-1)
```

What happens: the writer may swap and start draining between the Load and the unregistered read. It sees all counts at zero, considers the slot free, and writes to it while fn is still executing — a data race. Fix: Add(+1), then re-check front, then read (Exercise 2).

**Wrong: one global counter for all readers.**

```go
var globalReaders atomic.Int64 // one cache line, all goroutines

func (b *Buffer[T]) Read(fn func(*T)) {
    globalReaders.Add(1)
    fn(...)
    globalReaders.Add(-1)
}
```

What happens: every Read and ReadDone is a write to the same 64-byte cache line. At 32+ readers on separate cores, the cache line bounces between cores on every operation. Throughput degrades nearly to single-threaded. Fix: stripe across `numShards` padded counters (Exercise 1 + 2).

**Wrong: copying from the front slot inside Write.**

```go
buf.Write(func(rt *RoutingTable) {
    // Wrong: front slot is being read concurrently.
    *rt = *currentFront  // data race if any reader is active
    modify(rt)
})
```

What happens: the front slot is live during Write (readers are using it). Copying it inside Write introduces a race between the read of `currentFront` and an active reader. Fix: rebuild the back slot from scratch or from an independent source, never by copying from the front slot during Write.

**Wrong: calling Write from multiple goroutines.**

```go
go buf.Write(func(t *T) { /* goroutine A */ })
go buf.Write(func(t *T) { /* goroutine B */ }) // data race on back slot
```

What happens: both goroutines compute `back = 1 - front` identically and call fn on the same slot simultaneously — a data race on the slot data. Buffer supports exactly one concurrent writer. Use a `sync.Mutex` around the Write call site if multiple goroutines produce updates.

## Verification

From `~/go-exercises/doublebuf`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

The race detector is the critical check: `TestConcurrentReadersAndWriter` runs 32 goroutines calling Read while a single writer swaps 200 times. If the swap-check loop in Read is absent or ordered incorrectly, the race detector reports a data race on slot data within seconds.

To isolate the drain-blocking behavior:

```bash
go test -count=1 -run TestWriterWaitsForSlotToDrain -v ./...
```

All four commands must pass before the lesson is complete. Add at least one test of your own before running the final suite.

## Summary

- Two slots hold independent copies of T. An atomic uint32 (`front`) names the current read slot.
- Readers increment a sharded per-slot counter before reading, re-check `front` to handle the swap-in-window race, call fn, then decrement.
- The writer modifies the back slot exclusively, atomically swaps `front`, then spins until all shards of the old front's counter sum to zero.
- Sharded, cache-line-padded counters eliminate false sharing so that N readers in N different stripes never contend with each other.
- Write serializes itself: it waits for drain before returning, so the next Write always finds the back slot fully available.
- The Go race detector catches every violation of the no-concurrent-write contract and all escaped pointer uses.

## What's Next

Next: [Lock-Free Ring Buffer](../12-ring-buffer-lock-free/12-ring-buffer-lock-free.md).

## Resources

- pkg.go.dev/sync/atomic — `atomic.Pointer`, `atomic.Uint32`, `atomic.Int64`, `atomic.Uint64` type and method reference for Go 1.19+
- go.dev/blog/race-detector — how the Go race detector works, what it catches, and why `-race` is mandatory for concurrent code
- "Left-Right: A Concurrency Control Technique with Wait-Free Population Oblivious Reads" (Ramalhete & Correia, 2015) — the formal treatment of the reader-drain pattern this lesson implements
- go.dev/ref/spec#Order_of_evaluation — the Go memory model and why atomic operations are necessary for cross-goroutine visibility
- kernel.org/doc/html/latest/RCU/whatisRCU.html — Read-Copy-Update in the Linux kernel, the production precursor to the pattern here
