# 3. Work Stealing

When one P's local run queue empties while others are full, the scheduler steals roughly half the victim's queue. This load balancing is automatic and silent. This lesson builds a user-space work-stealing deque — the classic Chase-Lev deque — to make the push/pop/steal contract testable.

## Concepts

### Local Run Queue Per P

Each P in the Go runtime holds a local circular run queue with capacity 256. When a goroutine is created with `go`, it goes into the current P's local queue first. If the local queue is full, half of it is moved to the global run queue. The local queue avoids lock contention because only the owning P normally accesses it.

### Work Stealing

When a P's local queue is empty, it first checks the global run queue, then it attempts to steal from another P's local queue. The stealing P takes approximately half of the victim P's queue items. This amortizes the cost of load imbalance without requiring every goroutine creation to touch a global lock.

### Global Run Queue and Starvation Prevention

The global run queue is checked every 61 scheduling ticks to prevent starvation of goroutines that were placed there. A goroutine sitting in the global queue gets a chance to run even when all local queues are busy.

### The Chase-Lev Deque

The Chase-Lev deque (1999) is the standard algorithm underlying most work-stealing runtimes. It has two ends:

- Bottom (owner side): `Push` adds items here; `Pop` removes items here. Only the owning goroutine calls these.
- Top (thief side): `Steal` removes items from here. Any goroutine may call `Steal` concurrently.

The single-producer property of Push/Pop allows lock-free operation on the bottom. Steal uses a mutex in this implementation to protect against concurrent thieves competing with each other.

### Why User-Space Deques Matter

Understanding the deque interface makes it easier to design your own worker pools and task schedulers. The same push/pop/steal contract appears in thread pool libraries, parallel scanners, and recursive divide-and-conquer algorithms.

## Exercises

### Exercise 1: Push and Pop Round-Trip

Push three items onto a Deque and verify that Pop returns them in LIFO order (last in, first out from the bottom). Then verify that a Pop on an empty deque returns false.

### Exercise 2: ErrFull Behavior

Create a Deque with capacity 3. Push 3 items successfully, then push a fourth and confirm that `ErrFull` is returned. Use `errors.Is` for the check.

### Exercise 3: Concurrent Stealing

Push 100 items, then have 4 goroutines race to Steal all of them. After all goroutines finish, confirm that the total number of stolen items equals exactly 100 with no duplicates.

## Common Mistakes

Wrong: Calling Steal from the owner goroutine interleaved with Pop. What happens: data race on the bottom index (the mutex in Steal does not help if Pop also runs concurrently). Fix: Pop is always owner-only; never call Steal from the goroutine that calls Push/Pop.

Wrong: Sizing the deque at exactly the number of items you plan to push. What happens: any burst larger than capacity returns ErrFull and items are dropped. Fix: Size generously or handle ErrFull by retrying after a Pop.

Wrong: Checking `Len() == 0` from a thief to decide to stop stealing. What happens: TOCTOU race — Len may read 0 but the owner is about to push. Fix: Use the bool return from Steal; false means empty at that instant.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

Work stealing balances load across P's without global locking on the hot path. The Chase-Lev deque separates owner-only operations (Push/Pop) from concurrent thief operations (Steal). The split between bottom and top pointers is what makes the algorithm efficient. Understanding this structure helps when designing worker pools and parallel task schedulers.

## What's Next

[04. Cooperative vs Preemptive Scheduling](../04-cooperative-vs-preemptive/04-cooperative-vs-preemptive.md)

## Resources

- https://pkg.go.dev/runtime#hdr-Environment_Variables
- https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part2.html
- https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw
- https://research.tableau.com/sites/default/files/ipdps10_multicore.pdf

---

Create `go.mod`

```go
// go.mod
module example.com/wsdeque

go 1.26
```

Create `deque.go`

```go
package wsdeque

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrFull is returned by Push when the deque is at capacity.
var ErrFull = errors.New("wsdeque: deque is full")

// Deque is a bounded work-stealing deque. Push and Pop are called
// by the owning goroutine only. Steal may be called by any goroutine.
type Deque struct {
	buf []any
	cap int64
	top atomic.Int64 // thieves read and increment
	bot atomic.Int64 // owner reads and modifies
	mu  sync.Mutex   // protects Steal against concurrent thieves
}

// NewDeque returns a Deque with the given capacity (must be >= 1).
func NewDeque(capacity int) *Deque {
	if capacity < 1 {
		capacity = 1
	}
	return &Deque{
		buf: make([]any, capacity),
		cap: int64(capacity),
	}
}

// Push adds an item to the bottom (owner side). Returns ErrFull if at capacity.
func (d *Deque) Push(item any) error {
	b := d.bot.Load()
	t := d.top.Load()
	if b-t >= d.cap {
		return ErrFull
	}
	d.buf[b%d.cap] = item
	d.bot.Store(b + 1)
	return nil
}

// Pop removes an item from the bottom (owner side).
// Returns (item, true) or (nil, false) if empty.
func (d *Deque) Pop() (any, bool) {
	b := d.bot.Load() - 1
	d.bot.Store(b)
	t := d.top.Load()
	if t <= b {
		return d.buf[b%d.cap], true
	}
	// Empty: restore bot
	d.bot.Store(b + 1)
	return nil, false
}

// Steal removes an item from the top (thief side).
// Returns (item, true) or (nil, false) if empty.
func (d *Deque) Steal() (any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.top.Load()
	b := d.bot.Load()
	if t >= b {
		return nil, false
	}
	item := d.buf[t%d.cap]
	d.top.Store(t + 1)
	return item, true
}

// Len returns the number of items currently in the deque.
func (d *Deque) Len() int {
	b := d.bot.Load()
	t := d.top.Load()
	n := b - t
	if n < 0 {
		return 0
	}
	return int(n)
}
```

Create `deque_test.go`

```go
package wsdeque

import (
	"errors"
	"sync"
	"testing"
)

func TestPushPopRoundTrip(t *testing.T) {
	t.Parallel()
	d := NewDeque(8)
	items := []int{10, 20, 30}
	for _, v := range items {
		if err := d.Push(v); err != nil {
			t.Fatalf("Push(%d): %v", v, err)
		}
	}
	if d.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", d.Len())
	}
	// Pop is LIFO from bottom
	for i := len(items) - 1; i >= 0; i-- {
		got, ok := d.Pop()
		if !ok {
			t.Fatalf("Pop() returned false, want item %d", items[i])
		}
		if got != items[i] {
			t.Errorf("Pop() = %v, want %d", got, items[i])
		}
	}
	_, ok := d.Pop()
	if ok {
		t.Error("Pop() on empty deque returned true")
	}
}

func TestPushReturnsErrFull(t *testing.T) {
	t.Parallel()
	d := NewDeque(3)
	for i := 0; i < 3; i++ {
		if err := d.Push(i); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	err := d.Push(99)
	if !errors.Is(err, ErrFull) {
		t.Errorf("Push on full deque: err = %v, want ErrFull", err)
	}
}

func TestStealFromEmpty(t *testing.T) {
	t.Parallel()
	d := NewDeque(4)
	_, ok := d.Steal()
	if ok {
		t.Error("Steal() on empty deque returned true")
	}
}

func TestStealConcurrent(t *testing.T) {
	t.Parallel()
	const N = 100
	d := NewDeque(N)
	for i := 0; i < N; i++ {
		if err := d.Push(i); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		stolen []any
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				v, ok := d.Steal()
				if !ok {
					return
				}
				mu.Lock()
				stolen = append(stolen, v)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(stolen) != N {
		t.Errorf("stolen %d items, want %d", len(stolen), N)
	}
}

func TestLenTracksItems(t *testing.T) {
	t.Parallel()
	d := NewDeque(10)
	if d.Len() != 0 {
		t.Errorf("Len() = %d, want 0", d.Len())
	}
	_ = d.Push(1)
	_ = d.Push(2)
	if d.Len() != 2 {
		t.Errorf("Len() = %d, want 2", d.Len())
	}
	d.Pop()
	if d.Len() != 1 {
		t.Errorf("Len() = %d, want 1", d.Len())
	}
}
```

Create `example_test.go`

```go
package wsdeque_test

import (
	"fmt"

	"example.com/wsdeque"
)

func ExampleDeque_Len() {
	d := wsdeque.NewDeque(4)
	_ = d.Push("a")
	_ = d.Push("b")
	fmt.Println(d.Len())
	// Output:
	// 2
}
```

Create `cmd/demo/main.go`

```go
package main

import (
	"fmt"
	"sync"

	"example.com/wsdeque"
)

func main() {
	d := wsdeque.NewDeque(64)

	// Owner pushes 20 items.
	for i := 0; i < 20; i++ {
		if err := d.Push(i); err != nil {
			fmt.Printf("push %d: %v\n", i, err)
		}
	}
	fmt.Printf("After 20 pushes: Len=%d\n", d.Len())

	// One thief goroutine steals half.
	var wg sync.WaitGroup
	var mu sync.Mutex
	stolen := make([]any, 0, 10)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			v, ok := d.Steal()
			if !ok {
				return
			}
			mu.Lock()
			stolen = append(stolen, v)
			mu.Unlock()
		}
	}()

	// Owner pops remaining.
	popped := make([]any, 0, 10)
	for {
		v, ok := d.Pop()
		if !ok {
			break
		}
		popped = append(popped, v)
	}

	wg.Wait()
	fmt.Printf("Owner popped: %d items\n", len(popped))
	fmt.Printf("Thief stole: %d items\n", len(stolen))
	fmt.Printf("Total: %d (expected 20)\n", len(popped)+len(stolen))
}
```
