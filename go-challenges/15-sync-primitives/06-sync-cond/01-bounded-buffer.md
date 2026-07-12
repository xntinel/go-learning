# Exercise 1: Bounded blocking queue with two condition variables

The bounded buffer is the canonical `sync.Cond` artifact and a real backend
primitive: a fixed-capacity FIFO where producers block when it is full and
consumers block when it is empty. It is the shape of every hand-rolled work queue
that must apply backpressure — enqueue must wait rather than grow without bound.
This module builds it with two `Cond` instances sharing one mutex, and pins its
contract under `-race` and under Go 1.25 `testing/synctest` so the blocking and
wakeup are asserted deterministically with no sleeps.

## What you'll build

```text
bbuf/                       independent module: example.com/bbuf
  go.mod                    module path example.com/bbuf
  buffer.go                 type Buffer: New, Put, Get, Len, Cap (two Conds, one mutex)
  cmd/
    demo/
      main.go               N producers / M consumers exercising the buffer
  buffer_test.go            FIFO, concurrent stress, saturation, synctest block/wakeup tests
```

- Files: `buffer.go`, `cmd/demo/main.go`, `buffer_test.go`.
- Implement: a `Buffer` of `int` with `New(capacity)`, `Put(item)`, `Get() int`, `Len()`, `Cap()`, where `Put` blocks on a `notFull` condition and `Get` blocks on a `notEmpty` condition.
- Test: FIFO order, concurrent N-producer/M-consumer flow-through, never-exceeds-cap under saturation, plus `synctest` tests that assert `Get` blocks when empty and `Put` blocks when full.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/06-sync-cond/01-bounded-buffer/cmd/demo
cd go-solutions/15-sync-primitives/06-sync-cond/01-bounded-buffer
```

### Why two Conds over one mutex

The buffer has two distinct predicates. A producer waits for "not full"
(`len(buf) < cap`); a consumer waits for "not empty" (`len(buf) > 0`). These are
different conditions over the same shared slice, so they want two `Cond`
instances that both lock the one mutex guarding `buf`. `Put` appends and then
signals `notEmpty` — a consumer may be parked waiting for exactly that. `Get`
removes and then signals `notFull` — a producer may be parked waiting for a slot.
Each operation frees the OTHER side's condition, so each signals the OTHER's
`Cond`.

`Signal` (not `Broadcast`) is correct here: one appended item satisfies exactly
one consumer, one freed slot satisfies exactly one producer, and all producers
(and all consumers) are interchangeable. Waking more than one would only make the
extras re-check the `for` predicate, find nothing, and re-park — harmless but
wasteful.

The `for` loop around each `Wait` is mandatory, not defensive. When `Get` signals
`notFull` and a parked producer wakes, another producer may have raced in and
refilled the slot before the woken one re-acquires the lock; the loop re-checks
`len(b.buf) == b.cap` and re-parks if so. Replace the `for` with an `if` and that
second producer overflows the buffer past its capacity.

Create `buffer.go`:

```go
package bbuf

import "sync"

// Buffer is a fixed-capacity FIFO queue of ints. Put blocks while the buffer is
// full; Get blocks while it is empty. Both conditions share one mutex.
type Buffer struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	notEmpty *sync.Cond
	buf      []int
	cap      int
}

// New returns a Buffer holding at most capacity items (minimum 1).
func New(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	b := &Buffer{cap: capacity, buf: make([]int, 0, capacity)}
	b.notFull = sync.NewCond(&b.mu)
	b.notEmpty = sync.NewCond(&b.mu)
	return b
}

// Put appends item, blocking until a slot is free. It signals a waiting Get.
func (b *Buffer) Put(item int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == b.cap {
		b.notFull.Wait()
	}
	b.buf = append(b.buf, item)
	b.notEmpty.Signal()
}

// Get removes and returns the oldest item, blocking until one exists. It signals
// a waiting Put.
func (b *Buffer) Get() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 {
		b.notEmpty.Wait()
	}
	item := b.buf[0]
	b.buf = b.buf[1:]
	b.notFull.Signal()
	return item
}

// Len reports the current number of buffered items.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// Cap reports the fixed capacity.
func (b *Buffer) Cap() int {
	return b.cap
}
```

### The runnable demo

The demo runs three producers and three consumers against a capacity-4 buffer,
with the item counts arranged so the buffer fully drains. It uses the modern
`for i := range n` loop form and captures no loop variable by alias (Go 1.22+
gives each iteration its own variable).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/bbuf"
)

func main() {
	b := bbuf.New(4)

	const producers, perProducer = 3, 5
	const consumers, perConsumer = 3, 5 // producers*perProducer == consumers*perConsumer

	var wg sync.WaitGroup

	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perProducer {
				b.Put(p*100 + i)
			}
		}()
	}
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perConsumer {
				_ = b.Get()
			}
		}()
	}

	wg.Wait()
	fmt.Println("done; final len =", b.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
done; final len = 0
```

### Tests

The concurrency tests run outside a bubble under `-race`: `TestFIFOOrder` pins
ordering, and `TestConcurrentProducersConsumers` asserts every item flows through
and the buffer fully drains. `TestBlockingUnderSaturation` is the hard one — with
`cap=2` and a saturating producer, it asserts the observed length never exceeds
capacity, proving the `for`-loop backpressure actually holds.

The two blocking tests are rewritten with `testing/synctest`. Instead of
sleeping and hoping a goroutine reached `Wait`, `synctest.Wait()` returns only
once the goroutine is durably blocked on `Cond.Wait`, so "it is blocked" becomes a
deterministic fact. `TestGetBlocksWhenEmpty` confirms a `Get` on an empty buffer
parks and then wakes exactly when a `Put` arrives. `TestProducerBlocksWhenFull`
(the requested producer-side pin) confirms a `Put` on a full buffer parks and
then wakes exactly when a `Get` frees a slot.

Create `buffer_test.go`:

```go
package bbuf

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestSinglePutGet(t *testing.T) {
	t.Parallel()

	b := New(4)
	b.Put(42)
	if got := b.Get(); got != 42 {
		t.Fatalf("Get = %d, want 42", got)
	}
	if got := b.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
}

func TestFIFOOrder(t *testing.T) {
	t.Parallel()

	b := New(8)
	for i := range 8 {
		b.Put(i)
	}
	for i := range 8 {
		if got := b.Get(); got != i {
			t.Fatalf("Get[%d] = %d, want %d", i, got, i)
		}
	}
}

func TestCap(t *testing.T) {
	t.Parallel()

	b := New(5)
	if got := b.Cap(); got != 5 {
		t.Fatalf("Cap = %d, want 5", got)
	}
	if got := New(0).Cap(); got != 1 {
		t.Fatalf("Cap of New(0) = %d, want clamped to 1", got)
	}
}

func TestConcurrentProducersConsumers(t *testing.T) {
	t.Parallel()

	const producers, perProducer = 4, 250
	const consumers, perConsumer = 4, 250

	b := New(8)

	var produced, consumed atomic.Int64
	var wg sync.WaitGroup

	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perProducer {
				b.Put(p*1000 + i)
				produced.Add(1)
			}
		}()
	}
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perConsumer {
				_ = b.Get()
				consumed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got, want := produced.Load(), int64(producers*perProducer); got != want {
		t.Fatalf("produced = %d, want %d", got, want)
	}
	if got, want := consumed.Load(), int64(consumers*perConsumer); got != want {
		t.Fatalf("consumed = %d, want %d", got, want)
	}
	if got := b.Len(); got != 0 {
		t.Fatalf("Len after drain = %d, want 0", got)
	}
}

func TestBlockingUnderSaturation(t *testing.T) {
	t.Parallel()

	const cap = 2
	b := New(cap)

	var maxObserved atomic.Int64
	consumed := make(chan struct{})

	go func() {
		for i := range 200 {
			b.Put(i)
			if l := int64(b.Len()); l > maxObserved.Load() {
				maxObserved.Store(l)
			}
		}
	}()
	go func() {
		for range 200 {
			_ = b.Get()
		}
		close(consumed)
	}()

	<-consumed

	if got := maxObserved.Load(); got > int64(cap) {
		t.Fatalf("buffer overflowed: max observed len=%d, cap=%d", got, cap)
	}
}

func TestGetBlocksWhenEmpty(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := New(4)
		got := make(chan int, 1)
		go func() { got <- b.Get() }()

		synctest.Wait() // Get is now durably blocked on notEmpty.Wait

		select {
		case v := <-got:
			t.Fatalf("Get returned %d on an empty buffer", v)
		default:
		}

		b.Put(99)
		synctest.Wait() // Get has woken and returned

		select {
		case v := <-got:
			if v != 99 {
				t.Fatalf("Get returned %d, want 99", v)
			}
		default:
			t.Fatal("Get did not return after Put freed an item")
		}
	})
}

func TestProducerBlocksWhenFull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := New(2)
		b.Put(1)
		b.Put(2) // buffer full

		done := make(chan struct{})
		go func() {
			b.Put(3)
			close(done)
		}()

		synctest.Wait() // the third Put is durably blocked on notFull.Wait

		select {
		case <-done:
			t.Fatal("Put returned while the buffer was full")
		default:
		}

		if got := b.Get(); got != 1 {
			t.Fatalf("Get = %d, want 1", got)
		}
		synctest.Wait() // the blocked Put has woken and stored its item

		select {
		case <-done:
		default:
			t.Fatal("Put did not return after Get freed a slot")
		}
		if got := b.Len(); got != 2 {
			t.Fatalf("Len = %d, want 2 (items 2 and 3)", got)
		}
	})
}

func ExampleBuffer() {
	b := New(2)
	b.Put(1)
	b.Put(2)
	fmt.Println(b.Get())
	fmt.Println(b.Get())
	// Output:
	// 1
	// 2
}
```

## Review

The buffer is correct when three invariants hold. Length never exceeds capacity
under any interleaving — this is what `TestBlockingUnderSaturation` pins, and it
depends entirely on the `for len(b.buf) == b.cap` loop rather than an `if`.
Ordering is FIFO because `Get` always removes `buf[0]` and `Put` always appends.
And no goroutine sleeps forever: every `Put` signals `notEmpty` and every `Get`
signals `notFull`, so a parked waiter on either side is always kicked when its
condition can hold. The `synctest` tests prove the block-and-wakeup timing
deterministically: `synctest.Wait()` confirms the goroutine is durably parked on
`Cond.Wait` before the assertion, with no sleep, so these tests are neither slow
nor flaky. Run `go test -race` to confirm the two conditions and the shared mutex
actually serialize every access to `buf`.

The mistakes to avoid are the classic ones: an `if` instead of a `for` (a second
producer overflows the buffer), forgetting to signal after a state change (a
consumer sleeps on a non-empty buffer), and reusing one `Cond` for both
predicates (a producer's signal wakes a consumer who re-sleeps). Each is a real
regression this test suite catches.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — `NewCond`, `Wait`, `Signal`, and the required for-loop pattern.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — durably-blocked goroutines and `synctest.Wait`.
- [The Go Memory Model: sync](https://go.dev/ref/mem) — `Broadcast`/`Signal` synchronizes-before `Wait`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-countdown-latch.md](02-countdown-latch.md)
