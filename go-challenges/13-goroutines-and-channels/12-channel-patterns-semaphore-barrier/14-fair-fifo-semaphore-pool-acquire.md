# Exercise 14: Fair FIFO Semaphore to Prevent Connection-Pool Starvation

**Level: Advanced**

A saturated database pool serialized by a plain buffered-channel semaphore gives
no ordering guarantee: when the pool stays full, the runtime is free to keep
favoring newly-arriving acquirers, so one unlucky waiter can sit blocked while
newer callers stream past it — unbounded tail latency that looks like a hang for
a single request while the p50 stays healthy. This module builds a fair
semaphore that grants each freed slot strictly in arrival order using a FIFO
queue of per-waiter signal channels, and lets a queued waiter whose context is
cancelled remove itself without stalling the waiters behind it.

This module is self-contained: its own module, a `fairsem` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
fairsem/                     independent module: example.com/fairsem
  go.mod                     go 1.26; requires go.uber.org/goleak (test only)
  fairsem.go                 type Sem: New, Acquire(ctx) (release, err), Waiters
  cmd/demo/main.go           runnable demo: strict FIFO service order + mid-queue cancel
  fairsem_test.go            FIFO ordering, cancel-without-stall, balance; goleak TestMain
```

- Files: `fairsem.go`, `cmd/demo/main.go`, `fairsem_test.go`.
- Implement: `New(n int) *Sem`, `(*Sem).Acquire(ctx context.Context) (release func(), err error)`, and `(*Sem).Waiters() int`.
- Test: freed slots are granted in strict arrival order; a cancelled middle waiter dequeues itself and never blocks its successors; every release restores one slot; no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
```

### Why a buffered channel is not fair, and how to make one that is

A `chan struct{}` of capacity `n` is a counting semaphore, but its wakeup order
is whatever the runtime decides. Go does not promise FIFO among goroutines
blocked on the same channel send, and under sustained saturation the scheduler
can repeatedly hand the just-freed slot to a goroutine that arrived a microsecond
ago rather than the one that has been parked for seconds. For a connection pool
that is starvation: the victim's request times out while the pool never actually
went idle.

Fairness requires giving each waiter an identity and serving those identities in
order. The design here is a mutex-guarded FIFO queue of per-waiter channels plus
a `free` counter:

1. `Acquire` locks. If `free > 0`, it decrements `free`, unlocks, and returns a
   releaser — the fast path, no queue.
2. Otherwise it appends a fresh `chan struct{}` (its ticket) to the tail of the
   queue, unlocks, and blocks in a `select` on that ticket and `ctx.Done()`.
3. `release` locks. If the queue is non-empty it pops the head ticket and
   `close`s it — a *direct handoff*. The slot count is never returned to `free`;
   ownership transfers straight from the releaser to the head waiter, so no
   newcomer can slip in and grab it first. If the queue is empty, `release`
   increments `free`.

The invariant is that a slot is always in exactly one place: held by a caller,
promised to the head of the queue (mid-handoff), or counted in `free`. That is
what preserves balance and what makes ordering strict — the head of the queue is
always served next, by construction, not by scheduler luck.

Cancellation is the subtle part. When a queued waiter's context fires, it must
remove *its own* ticket so the waiters behind it shift forward. But a release may
be closing that same ticket at the same instant. Both `release` and the
cancellation path take the mutex, so they serialize: the cancelling waiter locks,
scans the queue for its ticket, and either finds it (removes it, returns
`ctx.Err()`) or does not. Not finding it means a release already popped and
closed the ticket under the lock — a slot was handed to us at the very moment we
were leaving. Dropping it would leak a slot forever, so we drain the closed
ticket and call `release` to pass the slot to the next waiter, then still return
`ctx.Err()`. Either way balance holds and the successors are never stalled.

Create `fairsem.go`:

```go
package fairsem

import (
	"context"
	"sync"
)

// Sem is a counting semaphore that grants freed slots strictly in arrival
// order. Unlike a bare buffered-channel semaphore, whose fairness is at the
// mercy of the runtime scheduler, Sem hands each freed slot directly to the
// oldest waiter through a per-waiter signal channel, so no waiter can be
// starved by a steady stream of newcomers.
type Sem struct {
	mu    sync.Mutex
	free  int             // slots not currently held or promised to a waiter
	queue []chan struct{} // FIFO of waiters; head is the next to be served
}

// New returns a semaphore with n slots free.
func New(n int) *Sem {
	return &Sem{free: n}
}

// Acquire takes a slot immediately if one is free, otherwise enqueues the
// caller and blocks until a slot is handed to it in FIFO order or ctx is done.
// On success it returns a release func that must be called exactly once to
// return the slot; on cancellation it returns a nil func and ctx.Err().
func (s *Sem) Acquire(ctx context.Context) (release func(), err error) {
	s.mu.Lock()
	if s.free > 0 {
		s.free--
		s.mu.Unlock()
		return s.releaser(), nil
	}
	// No slot free: take a numbered ticket at the tail of the queue. The slot,
	// when it frees, is handed to us by closing this channel.
	ch := make(chan struct{})
	s.queue = append(s.queue, ch)
	s.mu.Unlock()

	select {
	case <-ch:
		// A releaser popped us and closed ch, transferring a slot directly to
		// us without ever returning it to free. We now own that slot.
		return s.releaser(), nil
	case <-ctx.Done():
		s.mu.Lock()
		for i, c := range s.queue {
			if c == ch {
				// Still queued: remove ourselves so the waiters behind us shift
				// forward and are served without stalling on our ticket.
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		// Not in the queue: a concurrent release already popped us and closed
		// ch under the lock, so a slot was handed to us the instant we were
		// cancelling. Balance demands we pass it on rather than drop it.
		s.mu.Unlock()
		<-ch
		s.release()
		return nil, ctx.Err()
	}
}

// Waiters reports the number of goroutines currently queued for a slot.
func (s *Sem) Waiters() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// release returns one slot: it is handed to the oldest waiter if any is
// queued, otherwise it goes back to the free pool.
func (s *Sem) release() {
	s.mu.Lock()
	if len(s.queue) > 0 {
		ch := s.queue[0]
		s.queue = s.queue[1:]
		close(ch) // direct handoff: the slot never re-enters free
		s.mu.Unlock()
		return
	}
	s.free++
	s.mu.Unlock()
}

// releaser returns a release func guarded so a double call cannot corrupt the
// slot count.
func (s *Sem) releaser() func() {
	var once sync.Once
	return func() { once.Do(s.release) }
}
```

### The runnable demo

The demo pins ordering deterministically by starting each waiter only after
`Waiters()` confirms the previous one is enqueued, so start order equals enqueue
order. It then drains the queue one slot at a time and prints the service order,
and separately cancels the middle waiter of an A/B/C queue to show B leaves and C
is still served.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"runtime"
	"slices"

	"example.com/fairsem"
)

// waitWaiters spins until exactly k goroutines are queued. Polling Waiters()
// is a real synchronization signal, not a timing guess: it lets us pin the
// enqueue order deterministically before starting the next waiter.
func waitWaiters(s *fairsem.Sem, k int) {
	for s.Waiters() != k {
		runtime.Gosched()
	}
}

func main() {
	fairnessDemo()
	fmt.Println("---")
	cancelDemo()
}

func fairnessDemo() {
	s := fairsem.New(1)
	hold, _ := s.Acquire(context.Background()) // occupy the single slot

	names := []string{"A", "B", "C", "D"}
	done := make(chan string)
	rels := make(chan func())

	for i, name := range names {
		go func() {
			r, err := s.Acquire(context.Background())
			if err != nil {
				return
			}
			done <- name
			rels <- r
		}()
		waitWaiters(s, i+1) // enqueue strictly in start order
	}

	fmt.Println("pool size: 1")
	fmt.Printf("enqueued in start order: %v\n", names)

	var served []string
	hold() // release the held slot; it flows to the head of the queue
	for range names {
		served = append(served, <-done)
		r := <-rels
		r() // hand the slot to the next waiter, FIFO
	}
	fmt.Printf("served order:            %v\n", served)
	fmt.Printf("strict FIFO: %v\n", slices.Equal(served, names))
	fmt.Printf("waiters after: %d\n", s.Waiters())
}

func cancelDemo() {
	s := fairsem.New(1)
	hold, _ := s.Acquire(context.Background())

	type result struct {
		name string
		err  error
	}
	done := make(chan string)
	rels := make(chan func())
	cancelled := make(chan result)

	// A and C use a live context; B will be cancelled out of the middle.
	ctxB, cancelB := context.WithCancel(context.Background())

	start := func(name string, ctx context.Context) {
		go func() {
			r, err := s.Acquire(ctx)
			if err != nil {
				cancelled <- result{name, err}
				return
			}
			done <- name
			rels <- r
		}()
	}

	start("A", context.Background())
	waitWaiters(s, 1)
	start("B", ctxB)
	waitWaiters(s, 2)
	start("C", context.Background())
	waitWaiters(s, 3)

	fmt.Println("pool size: 1")
	fmt.Println("enqueued in start order: [A B C]")

	cancelB()
	res := <-cancelled
	fmt.Printf("cancel B -> %s err: %v\n", res.name, res.err)
	waitWaiters(s, 2) // B has removed itself; A and C remain

	var served []string
	hold()
	for range 2 {
		served = append(served, <-done)
		r := <-rels
		r()
	}
	fmt.Printf("served order:            %v\n", served)
	fmt.Printf("B never served: %v\n", !slices.Contains(served, "B"))
	fmt.Printf("waiters after: %d\n", s.Waiters())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pool size: 1
enqueued in start order: [A B C D]
served order:            [A B C D]
strict FIFO: true
waiters after: 0
---
pool size: 1
enqueued in start order: [A B C]
cancel B -> B err: context canceled
served order:            [A C]
B never served: true
waiters after: 0
```

### Tests

`TestFairFIFO` holds the single slot, starts M waiters one at a time (each only
after `Waiters()` reaches the expected depth, so enqueue order equals start
order), then releases the slot repeatedly and asserts the completion order equals
the start order exactly — the property a plain buffered-channel semaphore cannot
guarantee. `TestCancelNoHeadOfLineStall` enqueues A, B, C behind a held slot,
cancels B, asserts B's `Acquire` returns `context.Canceled` and dequeues itself,
then drains and asserts A and C are served in order while B never is.
`TestBalance` runs many concurrent acquire/release cycles and then proves the
pool can still grant its full capacity at once with zero waiters left, so every
release restored exactly one slot. The `goleak` `TestMain` proves cancelled and
served waiters all exit with no goroutine parked on a ticket.

Create `fairsem_test.go`:

```go
package fairsem

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Proves cancelled and served waiters all exit; a leaked goroutine parked
	// on a signal channel would fail here.
	goleak.VerifyTestMain(m)
}

// waitWaiters spins until exactly k goroutines are queued. Polling Waiters()
// is a synchronization signal on real state, so it pins enqueue order without
// a timing guess.
func waitWaiters(t *testing.T, s *Sem, k int) {
	t.Helper()
	for s.Waiters() != k {
		runtime.Gosched()
	}
}

// TestFairFIFO holds the one slot, enqueues M waiters one at a time (each only
// after Waiters() has reached the expected depth, so enqueue order equals start
// order), then releases the slot repeatedly and asserts the completion order
// matches the start order exactly. A buffered-channel semaphore cannot make
// this pass deterministically.
func TestFairFIFO(t *testing.T) {
	const m = 8
	s := New(1)
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("initial Acquire: %v", err)
	}

	done := make(chan int)
	rels := make(chan func())
	for i := range m {
		go func() {
			r, err := s.Acquire(context.Background())
			if err != nil {
				return
			}
			done <- i
			rels <- r
		}()
		waitWaiters(t, s, i+1)
	}

	got := make([]int, 0, m)
	hold() // the freed slot flows to the head of the queue
	for range m {
		got = append(got, <-done)
		r := <-rels
		r() // hand the slot to the next queued waiter
	}

	want := make([]int, m)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("service order = %v, want strict FIFO %v", got, want)
	}
	if w := s.Waiters(); w != 0 {
		t.Fatalf("Waiters() = %d after drain, want 0", w)
	}
}

// TestCancelNoHeadOfLineStall enqueues A, B, C behind a held slot, cancels B's
// context, and asserts B's Acquire returns its ctx error and removes itself.
// Releasing then serves A and C in order; B, the cancelled middle waiter, is
// never served and never stalls C behind it.
func TestCancelNoHeadOfLineStall(t *testing.T) {
	s := New(1)
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("initial Acquire: %v", err)
	}

	done := make(chan string)
	rels := make(chan func())
	bErr := make(chan error)
	ctxB, cancelB := context.WithCancel(context.Background())

	start := func(name string, ctx context.Context) {
		go func() {
			r, err := s.Acquire(ctx)
			if err != nil {
				if name == "B" {
					bErr <- err
				}
				return
			}
			done <- name
			rels <- r
		}()
	}

	start("A", context.Background())
	waitWaiters(t, s, 1)
	start("B", ctxB)
	waitWaiters(t, s, 2)
	start("C", context.Background())
	waitWaiters(t, s, 3)

	cancelB()
	if e := <-bErr; !errors.Is(e, context.Canceled) {
		t.Fatalf("B Acquire err = %v, want context.Canceled", e)
	}
	waitWaiters(t, s, 2) // B dequeued itself; A and C remain

	var served []string
	hold()
	for range 2 {
		served = append(served, <-done)
		r := <-rels
		r()
	}
	if want := []string{"A", "C"}; !slices.Equal(served, want) {
		t.Fatalf("service order = %v, want %v (B skipped)", served, want)
	}
	if slices.Contains(served, "B") {
		t.Fatalf("cancelled waiter B was served: %v", served)
	}
	if w := s.Waiters(); w != 0 {
		t.Fatalf("Waiters() = %d after drain, want 0", w)
	}
}

// TestBalance checks that every returned release restores exactly one free slot:
// after N concurrent acquire/release cycles on a pool of cap, the pool can still
// grant cap slots at once and no waiter remains queued.
func TestBalance(t *testing.T) {
	const cap, rounds = 3, 200
	s := New(cap)

	var wg sync.WaitGroup
	for range rounds {
		wg.Go(func() {
			r, err := s.Acquire(context.Background())
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			runtime.Gosched()
			r()
		})
	}
	wg.Wait()

	// All slots must be back: cap acquisitions succeed without blocking.
	rs := make([]func(), cap)
	for i := range cap {
		r, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("post-balance Acquire %d: %v", i, err)
		}
		rs[i] = r
	}
	if w := s.Waiters(); w != 0 {
		t.Fatalf("Waiters() = %d, want 0", w)
	}
	for _, r := range rs {
		r()
	}
}
```

## Review

Correctness here means three things at once: freed slots are granted in strict
arrival order, a cancelled waiter neither leaks a slot nor blocks its successors,
and the count is always balanced so a pool of `n` can perpetually grant exactly
`n`. The one invariant that guarantees all three is that every slot lives in
exactly one place — held, promised to the head of the queue mid-handoff, or in
`free` — enforced by doing every queue mutation and every `close` under the same
mutex, and by transferring ownership directly on release instead of bouncing the
count through `free` where a newcomer could intercept it. `TestFairFIFO` pins the
ordering by using `Waiters()` to make enqueue order deterministic and then
asserting the exact service order under `-race`; `TestCancelNoHeadOfLineStall`
proves the middle-waiter removal path; `TestBalance` and the `goleak` `TestMain`
prove the count and goroutine bookkeeping. This is the pattern that prevents the
production bug where a saturated connection pool, gated by a naive buffered
channel, starves one request into an unbounded tail-latency timeout while the
pool itself never actually went idle.

## Resources

- [sync.Once and sync.Mutex](https://pkg.go.dev/sync) — the guard used for exactly-once release and the lock that serializes the queue and the handoff.
- [Go Blog: Go Concurrency Patterns — Context](https://go.dev/blog/context) — cancellation propagation and returning `ctx.Err()` from a blocked operation.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — the `TestMain` verifier that proves cancelled and served waiters all exit.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels as semaphores, the baseline this exercise makes fair.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-quorum-barrier-multi-region-ack.md](13-quorum-barrier-multi-region-ack.md) | Next: [../13-goroutine-pools/00-concepts.md](../13-goroutine-pools/00-concepts.md)
