# Exercise 7: Blocking Bounded Queue with sync.Cond for a Worker Pool

A worker pool needs a bounded queue that pushes back: when it is full, producers
wait; when it is empty, consumers wait. This module builds `BlockingRing[T]` with
one `sync.Mutex` and two `sync.Cond` (notEmpty / notFull), the mandatory
`for`-loop wait guard, and context-aware `PutCtx` / `TakeCtx` for cancellation. It
is the backpressure primitive behind a producer/consumer pool — and a chance to get
`sync.Cond` exactly right.

Self-contained: its own module, the blocking queue, a demo, and `-race` tests.

## What you'll build

```text
blockingring/              independent module: example.com/blockingring
  go.mod                   go 1.24
  blockingring.go          BlockingRing[T]: Put, Take, PutCtx, TakeCtx, Len
  cmd/
    demo/
      main.go              producers + consumers, exact FIFO delivery
  blockingring_test.go     -race: FIFO no drops, Put blocks then unblocks, ctx cancel
```

Files: `blockingring.go`, `cmd/demo/main.go`, `blockingring_test.go`.
Implement: `BlockingRing[T]` with blocking `Put`/`Take` and cancellable `PutCtx`/`TakeCtx` returning `ctx.Err()`.
Test: producers exceed capacity, consumers drain, exact FIFO with no drops; `Put` blocks on a full queue then unblocks after a `Take`; context cancellation unblocks a waiter.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two conditions on one mutex

The queue has two predicates that block goroutines: "the buffer is not full" (what
a `Put` waits for) and "the buffer is not empty" (what a `Take` waits for). Both are
guarded by the same `sync.Mutex`, and each has its own `sync.Cond` created with
`sync.NewCond(&mu)`. The protocol per operation:

- `Put`: lock; `for size == cap { notFull.Wait() }`; push; `notEmpty.Signal()`;
  unlock. It waited on `notFull` and, having added an item, signals `notEmpty` to
  wake a consumer.
- `Take`: lock; `for size == 0 { notEmpty.Wait() }`; pop; `notFull.Signal()`;
  unlock. It waited on `notEmpty` and, having freed a slot, signals `notFull` to
  wake a producer.

The direction of the signal is the part people invert. After a `Take` you signal
`notFull` — the condition that a *producer* is waiting on — not `notEmpty`, because
`Take` did not add anything for a consumer to want. Signal the wrong `Cond` and a
producer or consumer strands forever even though there is work to do.

### Wait must be inside a for loop

`cond.Wait()` atomically releases the mutex, blocks, and re-acquires the mutex on
wake. Crucially, when it returns, the predicate it was waiting for is *not
guaranteed* to hold: a spurious wakeup can occur, and more importantly another
goroutine may have won the race and consumed the condition between the signal and
this goroutine re-acquiring the lock. So the wait is always `for !condition
{ cond.Wait() }`, never `if`. With an `if`, a woken `Take` could proceed to pop an
empty buffer. The `for` re-checks and goes back to sleep if the condition is still
false.

### Context cancellation over a Cond

`sync.Cond` has no built-in timeout or cancellation — `Wait` blocks until signaled.
To make `PutCtx` / `TakeCtx` cancellable, spawn a watcher goroutine that
`Broadcast`s the relevant `Cond` when `ctx.Done()` fires, and have the wait loop
also check `ctx.Err()` each time it wakes. The watcher must be torn down when the
operation completes (via a local done channel) so it does not linger. `Broadcast`
(not `Signal`) is used for the wakeup because the cancelled goroutine must be woken
even though no slot/item actually changed; broadcasting wakes all waiters, each
re-checks its predicate and its context, and only the affected ones act. On
cancellation the method returns `ctx.Err()` (`context.Canceled` or
`context.DeadlineExceeded`).

Create `blockingring.go`:

```go
package blockingring

import (
	"context"
	"sync"
)

// BlockingRing is a bounded FIFO queue. Put blocks while full and Take blocks
// while empty, providing backpressure for a producer/consumer worker pool.
type BlockingRing[T any] struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	data     []T
	head     int
	tail     int
	size     int
}

// New returns a BlockingRing with the given capacity (clamped to >= 1).
func New[T any](capacity int) *BlockingRing[T] {
	if capacity <= 0 {
		capacity = 1
	}
	b := &BlockingRing[T]{data: make([]T, capacity)}
	b.notEmpty = sync.NewCond(&b.mu)
	b.notFull = sync.NewCond(&b.mu)
	return b
}

// pushLocked and popLocked assume the mutex is held.
func (b *BlockingRing[T]) pushLocked(v T) {
	b.data[b.head] = v
	b.head = (b.head + 1) % len(b.data)
	b.size++
}

func (b *BlockingRing[T]) popLocked() T {
	var zero T
	v := b.data[b.tail]
	b.data[b.tail] = zero
	b.tail = (b.tail + 1) % len(b.data)
	b.size--
	return v
}

// Put blocks until there is room, then appends v.
func (b *BlockingRing[T]) Put(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.size == len(b.data) {
		b.notFull.Wait()
	}
	b.pushLocked(v)
	b.notEmpty.Signal()
}

// Take blocks until an item is available, then removes and returns the oldest.
func (b *BlockingRing[T]) Take() T {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.size == 0 {
		b.notEmpty.Wait()
	}
	v := b.popLocked()
	b.notFull.Signal()
	return v
}

// PutCtx blocks until there is room or ctx is done. On cancellation it returns
// ctx.Err() without enqueuing.
func (b *BlockingRing[T]) PutCtx(ctx context.Context, v T) error {
	stop := b.watch(ctx, b.notFull)
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.size == len(b.data) {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.notFull.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.pushLocked(v)
	b.notEmpty.Signal()
	return nil
}

// TakeCtx blocks until an item is available or ctx is done. On cancellation it
// returns the zero value and ctx.Err().
func (b *BlockingRing[T]) TakeCtx(ctx context.Context) (T, error) {
	var zero T
	stop := b.watch(ctx, b.notEmpty)
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.size == 0 {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		b.notEmpty.Wait()
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return b.popLocked(), nil
}

// Len reports the current item count (a hint under concurrency).
func (b *BlockingRing[T]) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// watch broadcasts c when ctx is done, so a waiting goroutine wakes to re-check
// its context. It returns a stop func to tear the watcher down.
func (b *BlockingRing[T]) watch(ctx context.Context, c *sync.Cond) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.Broadcast()
		case <-done:
		}
	}()
	return func() { close(done) }
}
```

### The runnable demo

The demo runs three producers pushing 10 items each into a capacity-4 queue while
two consumers drain, and reports the total consumed. Backpressure is exercised
because 30 items pass through a 4-slot buffer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/blockingring"
)

func main() {
	q := blockingring.New[int](4)
	const producers, perProd = 3, 10

	var pw sync.WaitGroup
	for p := range producers {
		pw.Add(1)
		go func() {
			defer pw.Done()
			for i := range perProd {
				q.Put(p*perProd + i)
			}
		}()
	}

	var consumed atomic.Int64
	var cw sync.WaitGroup
	for range 2 {
		cw.Add(1)
		go func() {
			defer cw.Done()
			for {
				if v := q.Take(); v < 0 {
					return // sentinel: stop this consumer
				}
				consumed.Add(1)
			}
		}()
	}

	pw.Wait()
	// One negative sentinel per consumer, delivered after all real items.
	q.Put(-1)
	q.Put(-1)
	cw.Wait()

	fmt.Printf("produced=%d consumed=%d\n", producers*perProd, consumed.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
produced=30 consumed=30
```

The two `-1` sentinels stop the two consumers and are not counted (each consumer
returns on seeing one), so exactly 30 real items are delivered.

### Tests

The tests pin FIFO-with-no-drops (a single producer and single consumer so order is
deterministic), the blocking behavior (a `Put` into a full queue does not return
until a `Take` frees a slot, timed via a channel), and context cancellation (a
`TakeCtx` on an empty queue returns `context.DeadlineExceeded` when its context
expires).

Create `blockingring_test.go`:

```go
package blockingring

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFIFONoDrops(t *testing.T) {
	t.Parallel()
	q := New[int](4)
	const n = 1000
	go func() {
		for i := range n {
			q.Put(i)
		}
	}()
	for i := range n {
		if got := q.Take(); got != i {
			t.Fatalf("Take = %d, want %d (order violated or item dropped)", got, i)
		}
	}
}

func TestPutBlocksUntilTake(t *testing.T) {
	t.Parallel()
	q := New[int](1)
	q.Put(1) // queue now full

	unblocked := make(chan struct{})
	go func() {
		q.Put(2) // must block until the Take below frees the slot
		close(unblocked)
	}()

	select {
	case <-unblocked:
		t.Fatal("Put returned while queue was full")
	case <-time.After(20 * time.Millisecond):
		// still blocked, as expected
	}

	if got := q.Take(); got != 1 {
		t.Fatalf("Take = %d, want 1", got)
	}
	select {
	case <-unblocked:
		// Put completed after the slot freed
	case <-time.After(time.Second):
		t.Fatal("Put did not unblock after Take freed a slot")
	}
	if got := q.Take(); got != 2 {
		t.Fatalf("Take = %d, want 2", got)
	}
}

func TestTakeCtxCancels(t *testing.T) {
	t.Parallel()
	q := New[int](2) // empty
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := q.TakeCtx(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TakeCtx err = %v, want context.DeadlineExceeded", err)
	}
}

func TestPutCtxCancels(t *testing.T) {
	t.Parallel()
	q := New[int](1)
	q.Put(1) // full
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- q.PutCtx(ctx, 2) }()

	time.Sleep(10 * time.Millisecond) // let PutCtx reach the Wait
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PutCtx err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PutCtx did not return after cancel")
	}
}
```

## Review

The queue is correct when it delivers every item in FIFO order with no drops, blocks
producers on a full buffer and consumers on an empty one, and honors context
cancellation. The two `sync.Cond` disciplines are the whole game: `Wait` lives inside
a `for` (never an `if`), so a spuriously or racily woken goroutine re-checks before
proceeding; and each operation signals the *opposite* condition it waited on — `Put`
signals `notEmpty`, `Take` signals `notFull`. `TestPutBlocksUntilTake` proves the
backpressure is real, and `TestTakeCtxCancels` proves a waiter can be released
without an item ever arriving. Remember the framing: a buffered channel already is a
blocking bounded queue, and is the right default; build this only when you need an
operation a channel lacks, such as a non-destructive `Len` or a snapshot.

## Resources

- [`sync` package: Cond](https://pkg.go.dev/sync#Cond) — `NewCond`, `Wait`, `Signal`, `Broadcast` and the for-loop rule.
- [`context` package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancel`, `ctx.Err()`, `Canceled`, `DeadlineExceeded`.
- [Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees a mutex and Cond provide.

---

Back to [06-byte-ring-io-reader-writer.md](06-byte-ring-io-reader-writer.md) | Next: [08-flight-recorder-crash-dump.md](08-flight-recorder-crash-dump.md)
