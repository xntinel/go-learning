# Exercise 17: Connection Pool — Resource Lease Iterator with Goroutine-Safe Cleanup and Early Return

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A database connection pool caps how many connections exist at once, and every
borrower must give its connection back -- on success, on error, or if the
caller just stops asking for more work partway through a batch. Modeling the
pool's slots as an `iter.Seq[int]` that borrowers range over turns "give the
connection back" into a property of the iterator itself instead of a
discipline borrowers have to remember, and a buffered channel does the
concurrency-safety work that would otherwise need a `sync.Mutex`. This
exercise is an independent module with its own `go mod init`.

## What you'll build

```text
pool/                     independent module: example.com/connection-pool-lease-iterator
  go.mod                   module example.com/connection-pool-lease-iterator
  pool.go                  Pool, New, Borrow
  cmd/
    demo/
      main.go              runnable demo: 5 sequential borrows from a pool of 2
  pool_test.go             sequential, early-break returns slot, concurrency bound, panic
```

Implement: `New(size int) *Pool` and `(*Pool) Borrow(count int) iter.Seq[int]` yielding up to `count` exclusive slot leases, blocking when the pool is exhausted; `New` panics if `size < 1`.
Test: borrowing 5 times from a pool of 2 yields 5 leases; breaking after 3 borrows from a pool of 1 still leaves the slot available afterward; 8 concurrent goroutines each borrowing 4 times from a pool of 3 never exceed 3 concurrent leases; `New(0)` panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

The pool holds its slots as integers in a buffered channel of size `size`,
pre-filled at construction. `Borrow` receives from that channel to acquire a
slot -- which blocks exactly when every slot is checked out, giving the pool
its backpressure for free -- and then calls `yield(slot)`. The load-bearing
line is the one right after: `p.slots <- slot` runs unconditionally, before
checking whether `yield` returned `true` or `false`. That ordering is what
makes cleanup correct on every exit path without an explicit `Release`
method and without a `sync.Mutex`: `yield` does not return control to
`Borrow` until the consumer's loop body for that iteration has completely
finished running, whether it fell through to the next iteration or executed
a `break`. So by the time `Borrow` regains control, the borrower is
provably done with the slot, and returning it to the channel is always
safe -- there is no window where the borrower could still be using it. The
channel itself supplies the goroutine safety: concurrent sends and receives
on a channel are safe without any additional lock.

Create `pool.go`:

```go
package pool

import "iter"

// Pool is a bounded set of resource slots (for example, database
// connections) identified by index. It is safe for concurrent use by
// multiple goroutines without a sync.Mutex because ownership of a slot
// transfers through a buffered channel: a receive hands a slot to exactly
// one goroutine, and a send hands it back.
type Pool struct {
	slots chan int
}

// New creates a Pool with size interchangeable slots, all initially free.
func New(size int) *Pool {
	if size < 1 {
		panic("pool: size must be >= 1")
	}
	p := &Pool{slots: make(chan int, size)}
	for i := range size {
		p.slots <- i
	}
	return p
}

// Borrow returns an iter.Seq[int] that yields up to count exclusive slot
// leases. Acquiring a slot blocks on the channel receive when the pool is
// exhausted, which is the backpressure: a caller cannot outrun the pool's
// capacity. The slot is returned to the pool immediately after the
// consumer's loop body for that iteration finishes running -- whether the
// body falls through to the next iteration or executes a break -- because
// yield does not return control to this function until the body has
// finished. That is what makes cleanup correct on early return without an
// explicit Release call and without a mutex: the channel send after yield
// always runs, on every exit path, including the one taken when the
// consumer stops early.
func (p *Pool) Borrow(count int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for range count {
			slot := <-p.slots
			cont := yield(slot)
			p.slots <- slot
			if !cont {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connection-pool-lease-iterator"
)

func main() {
	p := pool.New(2)

	n := 0
	for slot := range p.Borrow(5) {
		fmt.Printf("borrow %d: slot=%d\n", n, slot)
		n++
	}
	fmt.Println("done")
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
borrow 0: slot=0
borrow 1: slot=1
borrow 2: slot=0
borrow 3: slot=1
borrow 4: slot=0
done
```

With a pool of 2 slots pre-filled `[0, 1]`, each `Borrow` call takes the
front of the channel and pushes it back onto the tail immediately after use,
so a single sequential borrower cycles through slots `0, 1, 0, 1, 0` -- the
channel's FIFO order makes the cycling deterministic even though nothing
about the pool's contract guarantees a particular slot order under
concurrent use.

### Tests

The concurrency test is the one that actually exercises "goroutine-safe...
without `sync.Mutex`": 8 goroutines hammer a pool of 3 at once, and the test
tracks the peak number of leases held simultaneously with `sync/atomic`
rather than a mutex, asserting it never exceeds capacity.

Create `pool_test.go`:

```go
package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBorrowSequentialYieldsAllLeases(t *testing.T) {
	t.Parallel()

	p := New(2)
	var got []int
	for slot := range p.Borrow(5) {
		got = append(got, slot)
	}
	if len(got) != 5 {
		t.Fatalf("got %d leases, want 5", len(got))
	}
}

func TestBorrowReturnsSlotOnEarlyBreak(t *testing.T) {
	t.Parallel()

	p := New(1)
	count := 0
	for range p.Borrow(10) {
		count++
		if count == 3 {
			break
		}
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}

	select {
	case slot := <-p.slots:
		p.slots <- slot
	default:
		t.Fatal("slot was not returned to the pool after early break")
	}
}

func TestBorrowNeverExceedsCapacityConcurrently(t *testing.T) {
	t.Parallel()

	const capacity = 3
	p := New(capacity)
	var current, peak int64
	var wg sync.WaitGroup

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range p.Borrow(4) {
				n := atomic.AddInt64(&current, 1)
				for {
					pk := atomic.LoadInt64(&peak)
					if n <= pk || atomic.CompareAndSwapInt64(&peak, pk, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt64(&current, -1)
			}
		}()
	}
	wg.Wait()

	if peak > capacity {
		t.Fatalf("peak concurrent leases = %d, want <= %d", peak, capacity)
	}
}

func TestNewPanicsOnInvalidSize(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for size < 1")
		}
	}()
	New(0)
}
```

## Review

The property this design actually proves is that returning a slot does not
need to be the borrower's responsibility: it is a structural consequence of
how `yield` blocks until the loop body finishes. The common mistake in a
hand-rolled version of this pattern is to return the slot inside a `defer`
in the *consumer's* loop body, which works until someone forgets it, or an
`Acquire`/`Release` pair that leaks the moment a code path returns early
without calling `Release`. Moving the release into the iterator itself, right
after `yield`, removes that whole class of bug -- there is no `Release` call
to forget because there is no `Release` method. The channel-based
implementation also sidesteps a `sync.Mutex` entirely: a buffered channel of
capacity `size` already is the mutual-exclusion primitive, and `select`
against it (as the early-break test does) is a lock-free way to check
whether a slot is currently free.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Effective Go: channels](https://go.dev/doc/effective_go#channels)
- [The Go Memory Model](https://go.dev/ref/mem)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-token-bucket-rate-limiter.md](16-token-bucket-rate-limiter.md) | Next: [18-circuit-breaker-state-machine.md](18-circuit-breaker-state-machine.md)
