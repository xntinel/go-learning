# Exercise 11: Bounded Log Buffer With sync.Cond That Never Loses a Wakeup

**Level: Intermediate**

A log shipper keeps a fixed-capacity in-memory buffer between the application goroutines that emit
log lines and a single shipper goroutine that drains and flushes them to a remote sink. Producers
must block when the buffer is full so memory stays bounded, and the consumer must block when it is
empty, coordinated by a `*sync.Cond`. The failure mode is the lost wakeup: a `Signal`/`Broadcast`
omitted on a state transition, or a `Cond.Wait` not wrapped in a predicate loop, parks a producer
and the consumer on each other forever — a partial deadlock the runtime never reports because the
HTTP server and tickers keep the process runnable.

This module is self-contained: its own module, a `boundedbuf` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
boundedbuf/                  independent module: example.com/boundedbuf
  go.mod                     go 1.26
  boundedbuf.go              generic bounded FIFO on sync.Mutex + two sync.Cond
  cmd/demo/main.go           runnable demo: FIFO order, fan-in conservation, Put after Close
  boundedbuf_test.go         conservation, block/release, Close wakes all, drain-then-closed
```

- Files: `boundedbuf.go`, `cmd/demo/main.go`, `boundedbuf_test.go`.
- Implement: `New[T](capacity int) *Buffer[T]`, `(*Buffer[T]).Put(item T) error`, `(*Buffer[T]).Get() (T, bool)`, `(*Buffer[T]).Len() int`, `(*Buffer[T]).Close()`, and `var ErrClosed`.
- Test: N producers and M consumers move every item exactly once; a Put on a full buffer blocks and is released precisely by a Get; Close wakes every parked producer and consumer; a closed buffer drains remaining items then reports closed; Put after Close returns `ErrClosed`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/boundedbuf/cmd/demo
cd ~/go-exercises/boundedbuf
go mod init example.com/boundedbuf
go mod edit -go=1.26
```

### Two conditions, one mutex, and a predicate loop

A `sync.Cond` couples a boolean predicate to the mutex that guards it. A bounded buffer has two
predicates that block: "the buffer is full" (producers wait) and "the buffer is empty" (the consumer
waits). Use one mutex and two `*sync.Cond` built on it — `notFull` and `notEmpty` — so a waking
producer and a waking consumer are woken by different signals and do not thundering-herd each other.

Three rules make this correct, and each maps directly to a lost-wakeup bug when violated:

1. **Wait inside a for-loop over the predicate, never an `if`.** `Cond.Wait` atomically unlocks the
   mutex, parks, and re-locks on wake — but a wake does not prove the predicate changed. Another
   goroutine may have raced in and re-filled the slot, or the wake may be a `Broadcast` meant for a
   different waiter. `for b.count == len(b.ring) && !b.closed { b.notFull.Wait() }` re-checks after
   every wake; an `if` would proceed on a stale premise and overflow the ring or read from an empty
   one.
2. **Signal on every state transition that could satisfy a waiter.** When `Put` moves the buffer from
   empty to non-empty, a consumer might be parked on `notEmpty`, so it must `notEmpty.Signal()`.
   When `Get` moves it from full to non-full, it must `notFull.Signal()`. Omitting either is the
   canonical lost wakeup: the state changed but nobody was told, so a producer and the consumer each
   wait for a signal the other will never send.
3. **Hold the mutex across the predicate check and the `Wait`.** The check and the park must be one
   critical section, which is exactly what `Cond` guarantees by requiring the lock held on entry and
   releasing it atomically inside `Wait`. Checking the predicate, releasing the lock, then calling
   `Wait` would open a window where the signal fires between the check and the park and is lost.

`Close` is where a `Signal` is not enough. Any number of producers and the consumer may be parked at
shutdown, and every one of them must observe `closed == true` and return rather than leak. A single
`Signal` wakes one waiter; the rest stay parked forever. `Close` must `Broadcast` on both conditions
so all waiters wake, re-test their predicate — which now includes `!b.closed` — and return. `Close`
is idempotent: a second call is a no-op, never a double-broadcast or a panic.

The consumer contract is "closed and drained": `Get` on a closed-but-non-empty buffer still returns
buffered items in FIFO order, and only reports `ok == false` once the buffer is truly empty. That
keeps shutdown lossless — the shipper flushes what it already accepted before it stops.

Create `boundedbuf.go`:

```go
// Package boundedbuf is a fixed-capacity FIFO buffer coordinated by two
// sync.Cond variables. Producers block while it is full, a consumer blocks
// while it is empty, and Close wakes every waiter so nothing leaks on shutdown.
package boundedbuf

import (
	"errors"
	"sync"
)

// ErrClosed is returned by Put once the buffer has been closed.
var ErrClosed = errors.New("boundedbuf: closed")

// Buffer is a bounded FIFO queue. The zero value is not usable; call New.
type Buffer[T any] struct {
	mu       sync.Mutex
	notFull  *sync.Cond // signalled when a slot frees or the buffer closes
	notEmpty *sync.Cond // signalled when an item arrives or the buffer closes
	ring     []T
	head     int
	count    int
	closed   bool
}

// New returns a Buffer that holds at most capacity items. capacity must be >= 1.
func New[T any](capacity int) *Buffer[T] {
	if capacity < 1 {
		panic("boundedbuf: capacity must be >= 1")
	}
	b := &Buffer[T]{ring: make([]T, capacity)}
	b.notFull = sync.NewCond(&b.mu)
	b.notEmpty = sync.NewCond(&b.mu)
	return b
}

// Put appends item, blocking while the buffer is full. It returns ErrClosed if
// the buffer is (or becomes) closed. The Wait sits inside a predicate loop, so a
// spurious or shared wakeup that does not actually free a slot re-checks and
// parks again rather than proceeding on a false premise.
func (b *Buffer[T]) Put(item T) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for b.count == len(b.ring) && !b.closed {
		b.notFull.Wait()
	}
	if b.closed {
		return ErrClosed
	}

	b.ring[(b.head+b.count)%len(b.ring)] = item
	b.count++
	// A slot went from empty to non-empty: wake one waiting consumer. Omitting
	// this Signal is the lost wakeup the lesson is about.
	b.notEmpty.Signal()
	return nil
}

// Get removes and returns the oldest item, blocking while the buffer is empty.
// ok is false only once the buffer is closed AND fully drained; a closed buffer
// still hands back every buffered item first.
func (b *Buffer[T]) Get() (T, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for b.count == 0 && !b.closed {
		b.notEmpty.Wait()
	}
	if b.count == 0 { // closed and drained
		var zero T
		return zero, false
	}

	item := b.ring[b.head]
	var zero T
	b.ring[b.head] = zero // drop the reference so a large T can be collected
	b.head = (b.head + 1) % len(b.ring)
	b.count--
	// A slot went from full to non-full: wake one waiting producer.
	b.notFull.Signal()
	return item, true
}

// Len reports the number of buffered items.
func (b *Buffer[T]) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// Close marks the buffer closed and broadcasts on both conditions so every
// parked producer and consumer wakes and re-evaluates its predicate. It is
// idempotent. Broadcast (not Signal) is mandatory: any number of goroutines may
// be parked, and each must observe closed==true to return instead of leaking.
func (b *Buffer[T]) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.notFull.Broadcast()
	b.notEmpty.Broadcast()
}
```

### The runnable demo

The demo runs three scenarios whose output is deterministic. Scenario A pushes ten lines through a
capacity-2 buffer with one producer and one consumer, forcing repeated block-on-full and
block-on-empty, and confirms FIFO order survives. Scenario B fans four producers into four consumers
and asserts only on conserved aggregates (count and sum), since cross-producer interleaving is not
deterministic. Scenario C shows `Put` after `Close` returns `ErrClosed`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/boundedbuf"
)

func main() {
	// Scenario A: FIFO through a buffer far smaller than the workload, so the
	// single producer repeatedly blocks on full and the consumer repeatedly
	// blocks on empty. Order must be preserved end to end.
	fifoOK := func() bool {
		const n = 10
		buf := boundedbuf.New[int](2)
		got := make([]int, 0, n)

		var wg sync.WaitGroup
		wg.Go(func() {
			for {
				v, ok := buf.Get()
				if !ok {
					return
				}
				got = append(got, v)
			}
		})
		for i := range n {
			_ = buf.Put(i)
		}
		buf.Close()
		wg.Wait()

		if len(got) != n {
			return false
		}
		for i := range n {
			if got[i] != i {
				return false
			}
		}
		return true
	}()
	fmt.Printf("scenarioA fifo through capacity-2 buffer preserved order: %v\n", fifoOK)

	// Scenario B: 4 producers x 250 items into a capacity-8 buffer, drained by 4
	// consumers. Ordering across producers is nondeterministic, so we assert only
	// on the conserved aggregates: every item is delivered exactly once.
	const (
		producers   = 4
		perProducer = 250
		total       = producers * perProducer
	)
	buf := boundedbuf.New[int](8)

	var got, sum atomic.Int64
	var consumers sync.WaitGroup
	for range 4 {
		consumers.Go(func() {
			for {
				v, ok := buf.Get()
				if !ok {
					return
				}
				got.Add(1)
				sum.Add(int64(v))
			}
		})
	}

	var prod sync.WaitGroup
	for range producers {
		prod.Go(func() {
			for i := 1; i <= perProducer; i++ {
				_ = buf.Put(i)
			}
		})
	}
	prod.Wait()
	buf.Close() // single closer, after all producers have finished
	consumers.Wait()

	// Each producer contributes 1+2+...+250 = 31375; four of them sum to 125500.
	wantSum := int64(producers) * perProducer * (perProducer + 1) / 2
	fmt.Printf("scenarioB delivered %d/%d items, sum %d (want %d), none lost: %v\n",
		got.Load(), total, sum.Load(), wantSum, got.Load() == total && sum.Load() == wantSum)

	// Scenario C: Put after Close is a clean ErrClosed, not a panic or a block.
	err := buf.Put(1)
	fmt.Printf("scenarioC put after close: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scenarioA fifo through capacity-2 buffer preserved order: true
scenarioB delivered 1000/1000 items, sum 125500 (want 125500), none lost: true
scenarioC put after close: boundedbuf: closed
```

### Tests

Every hang-prone test runs under `guard`, a watchdog that dumps all goroutine stacks and fails the
test if the body does not return in time — a lost wakeup becomes a stack trace pointing at the parked
`Wait`, not a silent timeout. `TestConcurrentConservation` pushes a universe of distinct integers
through six producers and four consumers and asserts each was delivered exactly once, catching both
loss (a missed `notEmpty` signal) and duplication. `TestPutBlocksUntilReleasedByGet` fills a
capacity-1 buffer, starts a second `Put` that must park, uses a negative timeout to confirm it is
genuinely blocked, then shows a single `Get` releases it. `TestCloseWakesParkedProducersAndConsumers`
parks five producers on a full buffer and five consumers on an empty one and proves `Close` wakes
every one — the `Broadcast` test. `TestCloseDrainsRemainingThenReportsClosed` shows a closed buffer
still hands back its contents in FIFO order before reporting closed, and the two remaining tests pin
`ErrClosed` after `Close` and idempotent `Close`.

Create `boundedbuf_test.go`:

```go
package boundedbuf

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// guard runs fn and, if it does not return within d, dumps every goroutine's
// stack and fails the test. A lost wakeup is a partial deadlock the Go runtime
// never reports, so without this a broken buffer would hang until the package
// timeout instead of failing here with the exact parked frame.
func guard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s did not finish within %s: likely lost-wakeup deadlock\n\n=== goroutine dump ===\n%s",
			name, d, buf[:n])
	}
}

// TestConcurrentConservation moves a fixed universe of distinct items through
// the buffer with many producers and many consumers, then asserts every item
// was delivered exactly once: no loss (a missed notEmpty signal) and no
// duplication. The watchdog turns any lost wakeup into a stack dump.
func TestConcurrentConservation(t *testing.T) {
	t.Parallel()

	const (
		producers   = 6
		consumers   = 4
		perProducer = 500
		total       = producers * perProducer
	)

	guard(t, 20*time.Second, "conservation", func() {
		b := New[int](8)
		seen := make([]atomic.Int32, total)
		var delivered atomic.Int64

		var cons sync.WaitGroup
		for range consumers {
			cons.Go(func() {
				for {
					v, ok := b.Get()
					if !ok {
						return
					}
					seen[v].Add(1)
					delivered.Add(1)
				}
			})
		}

		var prod sync.WaitGroup
		for p := range producers {
			prod.Go(func() {
				base := p * perProducer
				for i := range perProducer {
					if err := b.Put(base + i); err != nil {
						t.Errorf("Put returned %v before Close", err)
						return
					}
				}
			})
		}
		prod.Wait()
		b.Close() // single closer, after every producer has finished
		cons.Wait()

		if delivered.Load() != total {
			t.Errorf("delivered %d items, want %d", delivered.Load(), total)
		}
		for v := range total {
			if got := seen[v].Load(); got != 1 {
				t.Errorf("item %d delivered %d times, want exactly 1", v, got)
			}
		}
	})
}

// TestPutBlocksUntilReleasedByGet pins the core coupling: a Put on a full buffer
// parks and is released by exactly one Get freeing exactly one slot. The
// negative timeout confirms the Put is genuinely blocked; the watchdog confirms
// the Get releases it.
func TestPutBlocksUntilReleasedByGet(t *testing.T) {
	t.Parallel()

	b := New[string](1)
	if err := b.Put("A"); err != nil { // buffer now full
		t.Fatalf("first Put: %v", err)
	}

	putB := make(chan error, 1)
	go func() { putB <- b.Put("B") }() // must block: buffer is full

	// Negative check: with a full buffer the second Put cannot complete. A
	// generous window makes a false "completed" reliable to catch.
	select {
	case err := <-putB:
		t.Fatalf("Put on a full buffer returned early with %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if got := b.Len(); got != 1 {
		t.Fatalf("Len while full = %d, want 1", got)
	}

	guard(t, 5*time.Second, "put-release", func() {
		if v, ok := b.Get(); !ok || v != "A" {
			t.Fatalf("Get = (%q, %v), want (A, true)", v, ok)
		}
		if err := <-putB; err != nil { // the freed slot must release Put("B")
			t.Fatalf("released Put returned %v", err)
		}
		if v, ok := b.Get(); !ok || v != "B" {
			t.Fatalf("second Get = (%q, %v), want (B, true)", v, ok)
		}
	})
}

// TestCloseWakesParkedProducersAndConsumers parks several producers on a full
// buffer and several consumers on an empty buffer, then closes both. Every
// waiter must wake and return; the watchdog proves none is left parked on a
// condition that will never be signalled again. Broadcast (not Signal) in Close
// is what makes all of them wake.
func TestCloseWakesParkedProducersAndConsumers(t *testing.T) {
	t.Parallel()

	guard(t, 5*time.Second, "close-wakes-waiters", func() {
		// Producers parked on a full buffer with no consumer draining it.
		const capacity, extraProducers = 2, 5
		full := New[int](capacity)
		for i := range capacity {
			if err := full.Put(i); err != nil {
				t.Errorf("prefill Put: %v", err)
			}
		}
		var succeeded, closedErr atomic.Int64
		var producers sync.WaitGroup
		for range extraProducers {
			producers.Go(func() {
				switch err := full.Put(99); {
				case err == nil:
					succeeded.Add(1)
				case errors.Is(err, ErrClosed):
					closedErr.Add(1)
				default:
					t.Errorf("parked Put woke with unexpected error %v", err)
				}
			})
		}

		// Consumers parked on an empty buffer with no producer.
		const parkedConsumers = 5
		empty := New[int](4)
		var gotFalse atomic.Int64
		var consumers sync.WaitGroup
		for range parkedConsumers {
			consumers.Go(func() {
				if _, ok := empty.Get(); ok {
					t.Errorf("Get on closed empty buffer returned ok=true")
					return
				}
				gotFalse.Add(1)
			})
		}

		full.Close()
		empty.Close()
		producers.Wait()
		consumers.Wait()

		// The buffer stayed full until Close, so no extra Put could succeed.
		if succeeded.Load() != 0 || closedErr.Load() != extraProducers {
			t.Errorf("parked producers: succeeded=%d closedErr=%d, want 0 and %d",
				succeeded.Load(), closedErr.Load(), extraProducers)
		}
		if gotFalse.Load() != parkedConsumers {
			t.Errorf("parked consumers woke with ok=false %d times, want %d",
				gotFalse.Load(), parkedConsumers)
		}
	})
}

// TestCloseDrainsRemainingThenReportsClosed shows Close does not discard
// buffered items: a closed buffer still hands back everything queued, in FIFO
// order, and only reports closed once empty.
func TestCloseDrainsRemainingThenReportsClosed(t *testing.T) {
	t.Parallel()

	b := New[int](4)
	for i := range 3 {
		if err := b.Put(i); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	b.Close()

	for i := range 3 {
		v, ok := b.Get()
		if !ok || v != i {
			t.Fatalf("drain Get = (%d, %v), want (%d, true)", v, ok, i)
		}
	}
	if v, ok := b.Get(); ok {
		t.Fatalf("Get after drain = (%d, %v), want (_, false)", v, ok)
	}
	if got := b.Len(); got != 0 {
		t.Fatalf("Len after drain = %d, want 0", got)
	}
}

func TestPutAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	b := New[int](2)
	b.Close()
	if err := b.Put(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close = %v, want ErrClosed", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	b := New[int](2)
	b.Close()
	b.Close() // must not panic on a second close
	if err := b.Put(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after double Close = %v, want ErrClosed", err)
	}
}
```

## Review

Correct here means bounded and lossless: memory never exceeds capacity because `Put` parks on a full
ring, and no item and no waiter is ever dropped because every state transition signals and `Close`
broadcasts. The invariant that guarantees it is the pairing of a for-predicate `Wait` with a
`Signal` on the matching transition — empty-to-nonempty wakes `notEmpty`, full-to-nonfull wakes
`notFull` — so a woken goroutine re-checks a predicate that is now actually true, and `Close` flips a
predicate every waiter tests, then broadcasts so all of them see it. `TestConcurrentConservation`
proves the signal path never loses an item across ten thousand hand-offs, `TestPutBlocksUntilReleasedByGet`
proves the block-and-release coupling is exact, and `TestCloseWakesParkedProducersAndConsumers`
proves `Broadcast` reaches every parked goroutine — all under a watchdog so a regression to a missing
signal or an `if`-instead-of-`for` fails as a stack dump instead of a silent hang. The production bug
this prevents is the log shipper that wedges at 3 a.m.: a producer parked on `notFull` and the
consumer parked on `notEmpty`, each waiting for a wakeup the other forgot to send, while the health
check keeps returning 200 and log lines pile up in memory until the process is OOM-killed.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) -- the `Wait`/`Signal`/`Broadcast` contract and the requirement to hold the lock across the predicate check.
- [The Go Memory Model](https://go.dev/ref/mem) -- why the mutex that guards the predicate also establishes the happens-before edge a `Cond` relies on.
- [Effective Go: concurrency](https://go.dev/doc/effective_go#concurrency) -- share-by-communicating context for when a `Cond`-based buffer is the right tool versus a channel.
- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) -- the watchdog primitive that turns a lost-wakeup hang into an actionable goroutine dump.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-deadlock-watchdog-test-harness.md](10-deadlock-watchdog-test-harness.md) | Next: [12-connection-pool-acquire-no-capacity-loss.md](12-connection-pool-acquire-no-capacity-loss.md)
