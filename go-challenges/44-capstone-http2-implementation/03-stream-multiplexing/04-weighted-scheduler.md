# Exercise 4: Weighted Stream Scheduler

When several streams are ready to send and the connection window allows only a finite budget this round, something must decide who sends and how much. RFC 7540 specified an elaborate priority tree; RFC 9113 deprecated it because almost nobody implemented it correctly. This exercise keeps the durable idea — split a fixed byte budget across ready streams in proportion to their weights, and skip any stream that is blocked or closed — in a small, exact weighted scheduler.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
scheduler/
  go.mod
  scheduler.go           Scheduler, NewScheduler, Add, Remove, SetReady, Allocate
  scheduler_test.go      proportional split, largest-remainder rounding, ready-only, sum==budget
  cmd/demo/main.go        allocate a budget, block a stream, allocate an awkward budget
```

- Files: `scheduler.go`, `scheduler_test.go`, `cmd/demo/main.go`.
- Implement: `Scheduler` with `Add(id, weight) error`, `Remove(id)`, `SetReady(id, ready)`, and `Allocate(budget) map[uint32]int`.
- Test: the budget splits in proportion to weight; rounding remainder is distributed so the allocation sums to exactly the budget; not-ready streams receive nothing and do not dilute the weight total.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/03-stream-multiplexing/04-weighted-scheduler/cmd/demo && cd go-solutions/44-capstone-http2-implementation/03-stream-multiplexing/04-weighted-scheduler
go mod edit -go=1.26
```

### Why proportional, and why rounding is the hard part

A weight expresses relative importance: a stream of weight 2 should get twice the bandwidth of a stream of weight 1 when both are ready. The natural allocation for a budget `B` across ready streams with weights `w_i` and total weight `W` is `B · w_i / W` for each. The catch is that bytes are integers and that formula almost never divides evenly: split 10 bytes across three equal-weight streams and each "wants" 3.33, but you can only hand out whole bytes. Truncating every share to `floor(B · w_i / W)` gives 3 + 3 + 3 = 9 and quietly loses a byte every round; over a long transfer those lost bytes are real throughput left on the floor, and the allocation no longer sums to the budget the connection window actually granted.

The fix is the largest-remainder method (the same apportionment rule used to assign legislative seats). Each stream first gets its floor share; the leftover — `budget` minus the sum of the floors — is then handed out one unit at a time to the streams with the largest fractional remainder, ties broken deterministically by insertion order. Because the fractional remainders sum to exactly the leftover, this distributes every last byte and the allocation provably sums to the budget. Computing the remainder as `(B · w_i) mod W` keeps everything in integers, so there is no floating-point rounding to reason about.

### Ready-only allocation

A stream that is blocked on flow control, half-closed in the send direction, or closed must not receive any of the budget, and — just as importantly — must not be counted in the weight total `W`. If a blocked stream still contributed its weight, the ready streams would be under-allocated and the budget would not be fully used. `Allocate` therefore filters to ready streams first, sums only their weights, and distributes only among them. When no stream is ready it returns an empty map, signalling the caller that this round produces no DATA frames.

Create `scheduler.go`:

```go
// Package scheduler distributes a connection's per-round byte budget across
// ready HTTP/2 streams in proportion to their weights. It replaces the
// deprecated RFC 7540 priority tree (RFC 9113 §5.3) with a simple, exact
// weighted allocator using the largest-remainder method.
package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrInvalidWeight is returned by Add for a weight below 1.
var ErrInvalidWeight = errors.New("scheduler: weight must be >= 1")

type entry struct {
	weight int
	ready  bool
}

// Scheduler holds the weighted, ready set of streams. It is safe for concurrent
// use.
type Scheduler struct {
	mu      sync.Mutex
	entries map[uint32]*entry
	order   []uint32 // insertion order, for deterministic tie-breaking
}

// NewScheduler creates an empty scheduler.
func NewScheduler() *Scheduler {
	return &Scheduler{entries: make(map[uint32]*entry)}
}

// Add registers a stream with the given weight (1..256 in HTTP/2 terms, though
// only weight >= 1 is enforced here). A stream starts ready. Re-adding an
// existing stream updates its weight and leaves its ready flag unchanged.
func (s *Scheduler) Add(id uint32, weight int) error {
	if weight < 1 {
		return fmt.Errorf("%w: stream %d weight %d", ErrInvalidWeight, id, weight)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok {
		e.weight = weight
		return nil
	}
	s.entries[id] = &entry{weight: weight, ready: true}
	s.order = append(s.order, id)
	return nil
}

// Remove drops a stream from the scheduler. Unknown IDs are ignored.
func (s *Scheduler) Remove(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return
	}
	delete(s.entries, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// SetReady marks whether a stream can currently send. A stream blocked on flow
// control or one that is half-closed locally should be set not-ready so it is
// excluded from allocation. Unknown IDs are ignored.
func (s *Scheduler) SetReady(id uint32, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok {
		e.ready = ready
	}
}

// Allocate distributes budget bytes across the currently ready streams in
// proportion to their weights, using the largest-remainder method so the
// returned allocation sums to exactly budget. A non-positive budget, or no
// ready streams, yields an empty map.
func (s *Scheduler) Allocate(budget int) map[uint32]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	alloc := make(map[uint32]int)
	if budget <= 0 {
		return alloc
	}

	type rdy struct {
		id     uint32
		weight int
		rem    int
		idx    int // position in order, for stable tie-breaking
	}
	var ready []rdy
	totalW := 0
	for i, id := range s.order {
		e := s.entries[id]
		if e == nil || !e.ready {
			continue
		}
		totalW += e.weight
		ready = append(ready, rdy{id: id, weight: e.weight, idx: i})
	}
	if totalW == 0 {
		return alloc
	}

	assigned := 0
	for k := range ready {
		exact := budget * ready[k].weight
		base := exact / totalW
		ready[k].rem = exact % totalW
		alloc[ready[k].id] = base
		assigned += base
	}

	leftover := budget - assigned
	if leftover > 0 {
		// Hand the leftover to the largest fractional remainders first, ties
		// broken by insertion order (lower idx wins).
		idxs := make([]int, len(ready))
		for k := range idxs {
			idxs[k] = k
		}
		sort.SliceStable(idxs, func(a, b int) bool {
			ra, rb := ready[idxs[a]], ready[idxs[b]]
			if ra.rem != rb.rem {
				return ra.rem > rb.rem
			}
			return ra.idx < rb.idx
		})
		for k := 0; k < leftover && k < len(idxs); k++ {
			alloc[ready[idxs[k]].id]++
		}
	}
	return alloc
}
```

The whole correctness argument rests on one fact: the fractional remainders `(budget · w_i) mod W` sum to `leftover · W`-equivalent units such that exactly `leftover` of them round up. Distributing precisely `leftover` increments, each to a distinct ready stream, guarantees the floors-plus-increments equal the budget. The `sort.SliceStable` plus the `idx` tie-break makes the choice of which streams round up fully deterministic, so the allocation is reproducible across runs — which is what makes it testable.

### The runnable demo

The demo registers three streams with weights 1, 2, 1 and allocates a round budget of 100, which splits cleanly as 25/50/25. It then marks the weight-2 stream as blocked on flow control: the budget redistributes across the two remaining ready streams as 50/50, and the blocked stream gets nothing. Finally it allocates an awkward budget of 9 with all three ready, where the largest-remainder rounding is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	sched "example.com/scheduler"
)

func main() {
	s := sched.NewScheduler()
	must(s.Add(1, 1))
	must(s.Add(3, 2))
	must(s.Add(5, 1))

	ids := []uint32{1, 3, 5}

	fmt.Println("=== budget 100, all ready (weights 1:2:1) ===")
	printAlloc(s.Allocate(100), ids)

	fmt.Println("\n=== stream 3 blocked on flow control ===")
	s.SetReady(3, false)
	printAlloc(s.Allocate(100), ids)

	fmt.Println("\n=== all ready, budget 9 (largest-remainder rounding) ===")
	s.SetReady(3, true)
	printAlloc(s.Allocate(9), ids)
}

func printAlloc(alloc map[uint32]int, ids []uint32) {
	total := 0
	for _, id := range ids {
		fmt.Printf("  stream %d -> %d bytes\n", id, alloc[id])
		total += alloc[id]
	}
	fmt.Printf("  total allocated = %d\n", total)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== budget 100, all ready (weights 1:2:1) ===
  stream 1 -> 25 bytes
  stream 3 -> 50 bytes
  stream 5 -> 25 bytes
  total allocated = 100

=== stream 3 blocked on flow control ===
  stream 1 -> 50 bytes
  stream 3 -> 0 bytes
  stream 5 -> 50 bytes
  total allocated = 100

=== all ready, budget 9 (largest-remainder rounding) ===
  stream 1 -> 2 bytes
  stream 3 -> 5 bytes
  stream 5 -> 2 bytes
  total allocated = 9
```

In the last round the floor shares are 2, 4, 2 (sum 8) with remainders 1, 2, 1; the single leftover byte goes to stream 3, which has the largest remainder, giving 2/5/2.

### Tests

The tests pin proportionality, exact summation, ready-only allocation, and the rounding rule. `TestProportionalSplit` checks the clean 25/50/25 case. `TestLargestRemainderRounding` checks an awkward budget lands on the expected streams. `TestAllocationSumsToBudget` sweeps many budgets and asserts the allocation always sums to the budget. `TestSkipsNotReadyStreams` blocks a stream and asserts it gets nothing and does not dilute the others. `TestEmptyWhenNoneReady` and `TestZeroBudget` cover the empty cases.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"errors"
	"testing"
)

func sum(alloc map[uint32]int) int {
	total := 0
	for _, v := range alloc {
		total += v
	}
	return total
}

func TestProportionalSplit(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	mustAdd(t, s, 3, 2)
	mustAdd(t, s, 5, 1)

	alloc := s.Allocate(100)
	want := map[uint32]int{1: 25, 3: 50, 5: 25}
	for id, w := range want {
		if alloc[id] != w {
			t.Errorf("stream %d = %d, want %d", id, alloc[id], w)
		}
	}
	if got := sum(alloc); got != 100 {
		t.Fatalf("sum = %d, want 100", got)
	}
}

func TestLargestRemainderRounding(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	mustAdd(t, s, 3, 2)
	mustAdd(t, s, 5, 1)

	alloc := s.Allocate(9)
	want := map[uint32]int{1: 2, 3: 5, 5: 2}
	for id, w := range want {
		if alloc[id] != w {
			t.Errorf("stream %d = %d, want %d", id, alloc[id], w)
		}
	}
	if got := sum(alloc); got != 9 {
		t.Fatalf("sum = %d, want 9", got)
	}
}

func TestEqualWeightTieBreakByInsertionOrder(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	mustAdd(t, s, 3, 1)
	mustAdd(t, s, 5, 1)

	// 10 / 3 = 3 each (sum 9); the single leftover byte goes to the
	// first-inserted stream on the all-equal remainder tie.
	alloc := s.Allocate(10)
	want := map[uint32]int{1: 4, 3: 3, 5: 3}
	for id, w := range want {
		if alloc[id] != w {
			t.Errorf("stream %d = %d, want %d", id, alloc[id], w)
		}
	}
	if got := sum(alloc); got != 10 {
		t.Fatalf("sum = %d, want 10", got)
	}
}

func TestAllocationSumsToBudget(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 3)
	mustAdd(t, s, 3, 5)
	mustAdd(t, s, 5, 7)
	mustAdd(t, s, 7, 11)

	for budget := 1; budget <= 1000; budget++ {
		alloc := s.Allocate(budget)
		if got := sum(alloc); got != budget {
			t.Fatalf("budget %d: sum = %d, want %d", budget, got, budget)
		}
	}
}

func TestSkipsNotReadyStreams(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	mustAdd(t, s, 3, 2)
	mustAdd(t, s, 5, 1)

	s.SetReady(3, false)
	alloc := s.Allocate(100)

	if alloc[3] != 0 {
		t.Errorf("blocked stream 3 = %d, want 0", alloc[3])
	}
	// The blocked stream's weight must not dilute the ready streams: 1 and 5
	// have equal weight, so they split the whole budget.
	if alloc[1] != 50 || alloc[5] != 50 {
		t.Errorf("ready split = {1:%d, 5:%d}, want {1:50, 5:50}", alloc[1], alloc[5])
	}
	if got := sum(alloc); got != 100 {
		t.Fatalf("sum = %d, want 100", got)
	}
}

func TestEmptyWhenNoneReady(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	mustAdd(t, s, 3, 1)
	s.SetReady(1, false)
	s.SetReady(3, false)

	if alloc := s.Allocate(100); len(alloc) != 0 {
		t.Fatalf("alloc = %v, want empty", alloc)
	}
}

func TestZeroBudget(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	if alloc := s.Allocate(0); len(alloc) != 0 {
		t.Fatalf("alloc = %v, want empty", alloc)
	}
}

func TestRemoveExcludesStream(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	mustAdd(t, s, 1, 1)
	mustAdd(t, s, 3, 1)
	s.Remove(3)

	alloc := s.Allocate(100)
	if alloc[3] != 0 {
		t.Errorf("removed stream 3 = %d, want 0", alloc[3])
	}
	if alloc[1] != 100 {
		t.Errorf("stream 1 = %d, want 100 (sole ready stream)", alloc[1])
	}
}

func TestInvalidWeight(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	if err := s.Add(1, 0); !errors.Is(err, ErrInvalidWeight) {
		t.Fatalf("Add weight 0 = %v, want ErrInvalidWeight", err)
	}
}

func mustAdd(t *testing.T, s *Scheduler, id uint32, weight int) {
	t.Helper()
	if err := s.Add(id, weight); err != nil {
		t.Fatalf("Add(%d, %d): %v", id, weight, err)
	}
}
```

## Review

The scheduler is correct when every byte of the budget is allocated, the split tracks the weights, and a not-ready stream is fully excluded — both from its share and from the weight total. The most common errors are truncating each share with integer division and silently dropping the leftover bytes (the budget then no longer sums and bandwidth is wasted), counting blocked streams in the weight total (which under-allocates the ready ones), and using a non-deterministic tie-break so the same inputs allocate differently across runs. Confirm `Allocate` sums to exactly the budget across a wide sweep, confirm the largest-remainder increments land on the right streams, and confirm blocking a stream redistributes its share. The `-race` run matters once `SetReady` is driven concurrently from per-stream goroutines while a scheduler goroutine calls `Allocate`.

## Resources

- [RFC 9113 §5.3 — Prioritization](https://httpwg.org/specs/rfc9113.html#priority): how and why RFC 9113 deprecated the RFC 7540 priority scheme, and what an endpoint may do instead.
- [RFC 7540 §5.3 — Stream Priority](https://httpwg.org/specs/rfc7540.html#StreamPriority): the original dependency-tree and weight model this exercise distills.
- [Largest remainder method](https://en.wikipedia.org/wiki/Largest_remainder_method): the apportionment rule that makes the allocation sum exactly to the budget.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-inbound-stream-limiter.md](03-inbound-stream-limiter.md)
