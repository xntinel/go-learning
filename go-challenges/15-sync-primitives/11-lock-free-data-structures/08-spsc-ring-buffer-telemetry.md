# Exercise 8: SPSC Ring Buffer for Non-Blocking Telemetry Events

The strongest lock-free move is proving you need no CAS at all. A telemetry
pipeline with one producer (the request handler's event hook) and one consumer
(the flusher goroutine) can run on a bounded ring buffer whose only
synchronization is atomic loads and stores of two indexes — because each index
has exactly one writer. This exercise builds that buffer with an explicit drop
policy, so the hot path never blocks no matter how far behind the flusher falls.

## What you'll build

```text
spscring/                        independent module: example.com/spscring
  go.mod
  ring.go                        Event; Ring: New (power-of-two check, sentinel
                                 error), TryPush, TryPop, Len, Dropped
  ring_test.go                   wrap-around FIFO, drop accounting, 100k-event
                                 producer/consumer ordering test, Example
  cmd/
    demo/
      main.go                    10 events into a capacity-8 ring: 2 dropped, FIFO drain
```

- Files: `ring.go`, `ring_test.go`, `cmd/demo/main.go`.
- Implement: a bounded SPSC ring with monotonically increasing `atomic.Uint64` head/tail, index masking, `TryPush` that drops (and counts) when full, `TryPop` that reports empty.
- Test: sequential wrap-around crossing the capacity boundary repeatedly; a concurrent test where one producer pushes 100000 sequenced events and one consumer drains, asserting strict order and exact drop accounting; `errors.Is` on the capacity sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/spscring/cmd/demo
cd ~/go-exercises/spscring
go mod init example.com/spscring
```

### One writer per index means no CAS

The ring holds `capacity` slots (a power of two). Two `uint64` cursors only ever
increase: `tail` counts events ever pushed, `head` counts events ever popped.
The element for logical position p lives at `buf[p & (capacity-1)]` — masking
replaces modulo because the capacity is a power of two. Emptiness and fullness
fall out of arithmetic on the *unmasked* cursors: empty when `head == tail`,
full when `tail - head == capacity`. Never compare masked values: after a wrap,
a full ring and an empty ring have identical masked cursors, and that
conflation is the classic ring-buffer bug. Unsigned monotone cursors sidestep
it entirely (and even wrap-around of the `uint64` itself is harmless: at one
event per nanosecond that is 584 years in, and unsigned subtraction still
yields the right difference).

Why is this safe with no CAS? Because every piece of state has exactly one
writer. Only the producer writes `tail` and the slots it publishes; only the
consumer writes `head`. What remains is *visibility*, and the Go memory model's
sequentially consistent atomics provide it:

- Producer: write `buf[tail&mask]`, *then* `tail.Store(tail+1)`. A consumer
  that loads the new tail is therefore guaranteed to see the completed element
  write — the store is the publication point.
- Consumer: read the element, *then* `head.Store(head+1)`. A producer that
  loads the new head (deciding the slot is free to reuse) is guaranteed the
  consumer has finished reading it.

Those two edges are the entire correctness argument, and they are exactly what
the race detector validates: reorder either store before its data access and
`-race` reports the conflicting slot access.

The drop policy is a production decision, not a shortcut. A telemetry hook on
the request path must never block a request because the flusher stalled;
bounded-plus-drop turns backpressure into a counted, alertable signal
(`Dropped()`) instead of latency. The alternative — blocking or unbounded
buffering — moves the failure into request latency or memory. Note what SPSC
buys over a buffered channel: `TryPush` is two atomic loads, one slot write,
one atomic store — no mutex, no scheduler interaction, and a non-blocking
failure mode you control. (When you *want* blocking semantics, use the channel;
this structure exists for the path where blocking is forbidden.)

Create `ring.go`:

```go
package spscring

import (
	"fmt"
	"sync/atomic"
)

// ErrCapacityNotPowerOfTwo is returned by New for capacities that
// cannot be index-masked.
var ErrCapacityNotPowerOfTwo = fmt.Errorf("capacity must be a power of two")

// Event is one telemetry record.
type Event struct {
	Seq int64
	Msg string
}

// Ring is a bounded single-producer/single-consumer queue. Exactly
// one goroutine may call TryPush and exactly one may call TryPop;
// that contract is what makes the index-only synchronization sound.
type Ring struct {
	buf     []Event
	mask    uint64
	head    atomic.Uint64 // consumer cursor: events ever popped
	tail    atomic.Uint64 // producer cursor: events ever pushed
	dropped atomic.Uint64
}

// New returns a ring with the given power-of-two capacity.
func New(capacity int) (*Ring, error) {
	if capacity <= 0 || capacity&(capacity-1) != 0 {
		return nil, fmt.Errorf("spscring: capacity %d: %w", capacity, ErrCapacityNotPowerOfTwo)
	}
	return &Ring{
		buf:  make([]Event, capacity),
		mask: uint64(capacity - 1),
	}, nil
}

// TryPush appends e if there is room. When the ring is full it counts
// a drop and returns false; it never blocks. Producer-only.
func (r *Ring) TryPush(e Event) bool {
	tail := r.tail.Load()
	if tail-r.head.Load() == uint64(len(r.buf)) {
		r.dropped.Add(1)
		return false
	}
	r.buf[tail&r.mask] = e
	r.tail.Store(tail + 1) // publication point: element write above is visible
	return true
}

// TryPop removes the oldest event, reporting false when the ring is
// empty. Consumer-only.
func (r *Ring) TryPop() (Event, bool) {
	head := r.head.Load()
	if head == r.tail.Load() {
		return Event{}, false
	}
	e := r.buf[head&r.mask]
	r.head.Store(head + 1) // frees the slot: read above is complete
	return e, true
}

// Len reports how many events are buffered right now.
func (r *Ring) Len() int {
	return int(r.tail.Load() - r.head.Load())
}

// Dropped reports how many events TryPush rejected.
func (r *Ring) Dropped() uint64 {
	return r.dropped.Load()
}
```

### Tests

`TestWrapAround` is sequential and surgical: on a capacity-4 ring it pushes and
pops in a pattern that crosses the wrap boundary five times, asserting FIFO
order throughout — the test that fails if empty/full detection compares masked
indexes. `TestFullDrops` pins the drop policy and its accounting.
`TestSPSCConcurrent` is the real thing: a producer goroutine pushes 100000
sequenced events as fast as it can (counting its own rejected pushes), the
consumer drains concurrently; the consumer must see sequence numbers in
*strictly increasing* order (gaps are dropped events, regressions are
corruption), and consumed plus dropped must equal exactly 100000. Under `-race`
this test also proves the two happens-before edges described above.

Create `ring_test.go`:

```go
package spscring

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNewRejectsBadCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1, 3, 6, 100} {
		_, err := New(capacity)
		if !errors.Is(err, ErrCapacityNotPowerOfTwo) {
			t.Errorf("New(%d) error = %v, want ErrCapacityNotPowerOfTwo", capacity, err)
		}
	}
	if _, err := New(8); err != nil {
		t.Fatalf("New(8) error = %v, want nil", err)
	}
}

func TestWrapAround(t *testing.T) {
	t.Parallel()

	r, err := New(4)
	if err != nil {
		t.Fatal(err)
	}

	next := int64(0)   // next sequence to push
	expect := int64(0) // next sequence the consumer must see

	// Repeatedly fill by 3 and drain by 3: 4 does not divide 30, so
	// the cursors cross the wrap boundary at varying offsets.
	for range 10 {
		for range 3 {
			if !r.TryPush(Event{Seq: next}) {
				t.Fatalf("TryPush(%d) = false with room available", next)
			}
			next++
		}
		for range 3 {
			e, ok := r.TryPop()
			if !ok {
				t.Fatalf("TryPop = false, want event %d", expect)
			}
			if e.Seq != expect {
				t.Fatalf("popped seq %d, want %d (FIFO broken at wrap)", e.Seq, expect)
			}
			expect++
		}
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
}

func TestFullDrops(t *testing.T) {
	t.Parallel()

	r, err := New(2)
	if err != nil {
		t.Fatal(err)
	}
	if !r.TryPush(Event{Seq: 1}) || !r.TryPush(Event{Seq: 2}) {
		t.Fatal("pushes within capacity failed")
	}
	if r.TryPush(Event{Seq: 3}) {
		t.Fatal("TryPush succeeded on a full ring")
	}
	if got := r.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
	}
	if e, ok := r.TryPop(); !ok || e.Seq != 1 {
		t.Fatalf("TryPop = %+v,%v, want seq 1", e, ok)
	}
	if !r.TryPush(Event{Seq: 4}) {
		t.Fatal("TryPush failed after a pop freed a slot")
	}
}

func TestSPSCConcurrent(t *testing.T) {
	t.Parallel()

	const total = 100000
	r, err := New(1024)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() { // the single producer
		defer wg.Done()
		defer close(done)
		for seq := int64(0); seq < total; seq++ {
			r.TryPush(Event{Seq: seq}) // full ring drops; Dropped counts it
		}
	}()

	var consumed uint64
	lastSeq := int64(-1)
	wg.Add(1)
	go func() { // the single consumer
		defer wg.Done()
		for {
			e, ok := r.TryPop()
			if !ok {
				select {
				case <-done:
					// Producer finished: drain what remains, then stop.
					for {
						e, ok := r.TryPop()
						if !ok {
							return
						}
						if e.Seq <= lastSeq {
							t.Errorf("seq %d after %d: order broken", e.Seq, lastSeq)
							return
						}
						lastSeq = e.Seq
						consumed++
					}
				default:
					continue
				}
			}
			if e.Seq <= lastSeq {
				t.Errorf("seq %d after %d: order broken", e.Seq, lastSeq)
				return
			}
			lastSeq = e.Seq
			consumed++
		}
	}()

	wg.Wait()

	if got := consumed + r.Dropped(); got != total {
		t.Fatalf("consumed(%d) + dropped(%d) = %d, want %d",
			consumed, r.Dropped(), got, total)
	}
	if consumed == 0 {
		t.Fatal("consumer saw nothing; scheduling starved the test")
	}
}

func ExampleRing() {
	r, _ := New(4)
	r.TryPush(Event{Seq: 1, Msg: "request.start"})
	r.TryPush(Event{Seq: 2, Msg: "request.end"})
	for {
		e, ok := r.TryPop()
		if !ok {
			break
		}
		fmt.Println(e.Seq, e.Msg)
	}
	// Output:
	// 1 request.start
	// 2 request.end
}
```

### The demo

Ten events arrive at a capacity-8 ring with no consumer running — two are
dropped, and the drain that follows yields the first eight in FIFO order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/spscring"
)

func main() {
	r, err := spscring.New(8)
	if err != nil {
		panic(err)
	}

	for seq := int64(1); seq <= 10; seq++ {
		r.TryPush(spscring.Event{Seq: seq, Msg: "telemetry"})
	}
	fmt.Printf("buffered=%d dropped=%d\n", r.Len(), r.Dropped())

	first, last := int64(0), int64(0)
	for {
		e, ok := r.TryPop()
		if !ok {
			break
		}
		if first == 0 {
			first = e.Seq
		}
		last = e.Seq
	}
	fmt.Printf("drained %d..%d in order\n", first, last)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buffered=8 dropped=2
drained 1..8 in order
```

## Review

The contract line in the doc comment — exactly one pusher, exactly one popper —
is not advice, it is the algorithm: add a second producer and two `TryPush`
calls can read the same `tail`, write the same slot, and publish one event
twice while losing another (module 10 is the multi-producer answer). Review the
store ordering the way the comments mark it: the element write must precede the
`tail.Store` and the element read must precede the `head.Store`; swapping
either is a real bug that `-race` catches in the concurrent test. Keep the
cursors monotone and compare differences, never masked values. And treat
`Dropped` as an SLO signal: a nonzero rate means the flusher is undersized or
the ring too small — resize capacity (still a power of two), do not "fix" it by
making `TryPush` wait.

## Resources

- [Circular buffer](https://en.wikipedia.org/wiki/Circular_buffer) — the structure, and the empty-vs-full ambiguity monotone cursors avoid.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees of `sync/atomic` that the two publication points rely on.
- [LMAX Disruptor](https://lmax-exchange.github.io/disruptor/disruptor.html) — the high-performance ring-buffer design that popularized this approach.
- [sync/atomic: Uint64](https://pkg.go.dev/sync/atomic#Uint64) — Load/Store/Add on the cursor type.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-cow-subscriber-registry.md](07-cow-subscriber-registry.md) | Next: [09-versioned-peak-gauge.md](09-versioned-peak-gauge.md)
