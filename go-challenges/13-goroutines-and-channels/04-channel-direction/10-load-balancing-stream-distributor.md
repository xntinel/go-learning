# Exercise 10: Distribute One Ingest Stream Across N Owned Lane Channels

**Level: Intermediate**

A high-volume ingest service pulls records off a single upstream channel and must
spread them across N downstream lane-writers, each lane owning its own database
connection. A tee copies every value to all outputs; that is the wrong shape here,
because writing the same record to every connection would multiply the load N-fold.
A distributor sends each value to exactly one lane, and whichever lane is free next
takes the next record, so a briefly-slow lane does not stall the others. This
module builds `Split`, which returns N receive-only lane views a consumer can never
send back on or close, while the distributor keeps the bidirectional ends and
closes every lane exactly once when the input drains.

This module is self-contained: its own module, a `distributor` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
distributor/                 independent module: example.com/distributor
  go.mod                     go 1.26
  distributor.go             Split[T any](in <-chan T, n int) []<-chan T
  cmd/demo/main.go           runnable demo: one stream fanned across 3 lanes
  distributor_test.go        union==input, exactly-once delivery, all lanes closed,
                             n==1 pass-through, empty input, slow lane no-stall
```

- Files: `distributor.go`, `cmd/demo/main.go`, `distributor_test.go`.
- Implement: `func Split[T any](in <-chan T, n int) []<-chan T` — fan one input across n competing lanes, one value per lane, closing every lane once when `in` drains.
- Test: the multiset union across lanes equals the input multiset, each value lands in exactly one lane, every lane is closed after drain, `n==1` is a pass-through, empty input closes all lanes, and a stalled lane does not gate the others.
- Verify: `go test -count=1 -race ./...`

### One value per lane, and why a single owner per lane makes close trivial

A distributor is a fan-out, but not a broadcast. In a tee, every output must see
every value, which forces the "send to each output exactly once" `nil`-channel
dance. A load-balancing distributor is the opposite contract: each input value goes
to *exactly one* lane, and the choice of lane is whichever one is ready to accept
work right now. That single difference removes all the per-value coordination and
replaces it with a much simpler structure.

The construction is one forwarder goroutine per lane, all competing to receive from
the same `in`:

1. `Split` makes `n` bidirectional lane channels and, alongside them, an `[]<-chan T`
   of narrowed views. Assigning `lanes[i]` (a `chan T`) into a `<-chan T` slot is the
   implicit one-way narrowing: the caller receives receive-only ends and has no legal
   expression to send on or close a lane.
2. For each lane it launches a forwarder: `for v := range in { lane <- v }`. A channel
   receive delivers each value to exactly one receiver, so across the `n` forwarders
   every value from `in` is taken once and forwarded to one lane. No value is lost and
   none is duplicated — that is the union-equals-input invariant, for free, from the
   runtime's receive semantics.
3. Load balancing is emergent, not scheduled. With unbuffered lanes, a forwarder that
   is mid-send to a slow consumer is not looping back to receive from `in`, so it
   simply does not compete for the next value; the free forwarders do. A briefly-slow
   lane holds at most one in-flight value and the stream keeps flowing through the rest.

The close story is the payoff of one-owner-per-lane. In fan-in, many goroutines feed
one output, so closing exactly once needs a `WaitGroup` and a lone closer goroutine.
Here the ownership is inverted: each lane has exactly one writer, its forwarder. That
forwarder `defer close(lane)`s when its `range in` ends. Because no other goroutine
can ever close that lane, the close is unconditionally exactly-once with no
coordination at all — the structure, not a protocol, guarantees it. When `in` closes,
every forwarder's range terminates, every lane is closed once, and every draining
consumer sees `ok == false`.

Create `distributor.go`:

```go
package distributor

// Split fans a single receive-only input out across n competing lanes. Each
// input value is delivered to exactly one lane. The returned channels are
// receive-only so consumers cannot send or close them; Split owns the
// bidirectional ends and closes every lane exactly once when in drains.
//
// n must be >= 1. Each lane is served by one forwarder goroutine that competes
// with the others to receive the next value from in, so a briefly-slow lane
// consumer never gates the other lanes.
func Split[T any](in <-chan T, n int) []<-chan T {
	lanes := make([]chan T, n)
	views := make([]<-chan T, n)
	for i := range n {
		lanes[i] = make(chan T)
		views[i] = lanes[i] // implicit narrowing: chan T -> <-chan T
	}

	for i := range n {
		lane := lanes[i]
		go func() {
			// This forwarder is the sole owner of lane, so closing it here is
			// unconditionally exactly-once: no WaitGroup or shared closer is
			// needed because no other goroutine can close this lane.
			defer close(lane)
			for v := range in {
				lane <- v
			}
		}()
	}

	return views
}
```

### The runnable demo

The demo feeds the integers 1..12 into one input channel and splits them across
three lanes, draining all three concurrently. Which lane receives which value is
nondeterministic — that is the whole point of load balancing — so the demo collects
the union and sorts it before printing, which makes the output reproducible while
still proving no value was lost or duplicated.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"
	"sync"

	"example.com/distributor"
)

func main() {
	in := make(chan int)
	go func() {
		defer close(in)
		for i := 1; i <= 12; i++ {
			in <- i
		}
	}()

	lanes := distributor.Split(in, 3)

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		got []int
	)
	for _, lane := range lanes {
		wg.Go(func() {
			for v := range lane {
				mu.Lock()
				got = append(got, v)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	// Which lane received which value is nondeterministic (load balancing), so
	// the demo prints the sorted union to stay reproducible.
	slices.Sort(got)
	fmt.Println("lanes:", len(lanes))
	fmt.Println("received:", got)
	fmt.Println("count:", len(got))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lanes: 3
received: [1 2 3 4 5 6 7 8 9 10 11 12]
count: 12
```

### Tests

`TestUnionEqualsInputNoLossNoDuplication` is the central one: it drains four lanes
and asserts the multiset union equals the input `0..499`. Equal counts prove both
properties at once — nothing was lost (count matches) and each value landed in
exactly one lane (no count exceeds one) — and it checks every lane reports closed
via the comma-ok receive. `TestSingleLaneIsPassThrough` confirms `n==1` still
delivers everything and closes. `TestEmptyInputClosesAllLanes` closes the input
before any value flows and asserts all lanes close immediately. `TestSlowLaneDoesNotStallOthers`
stalls lane 0's consumer and asserts the two eager lanes drain everything but the
single in-flight value before the stall is released — the load-balancing guarantee.
Every lane is drained in its own goroutine, and the suite passes `-count=2 -race`.

Create `distributor_test.go`:

```go
package distributor

import (
	"sync"
	"testing"
	"time"
)

// drainAll reads every lane to completion, each in its own goroutine, and
// returns the combined multiset of received values plus a per-lane record of
// whether the comma-ok receive confirmed the lane was closed.
func drainAll[T any](lanes []<-chan T) (got []T, closed []bool) {
	closed = make([]bool, len(lanes))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for i, lane := range lanes {
		wg.Go(func() {
			for v := range lane {
				mu.Lock()
				got = append(got, v)
				mu.Unlock()
			}
			// range exited: the next comma-ok receive must report closed.
			if _, ok := <-lane; !ok {
				mu.Lock()
				closed[i] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return got, closed
}

func feed(n int) <-chan int {
	in := make(chan int)
	go func() {
		defer close(in)
		for i := range n {
			in <- i
		}
	}()
	return in
}

func counts(vs []int) map[int]int {
	m := make(map[int]int, len(vs))
	for _, v := range vs {
		m[v]++
	}
	return m
}

// TestUnionEqualsInputNoLossNoDuplication pins the central invariant: the
// multiset union of everything read across all lanes equals the input multiset.
// Equal counts prove simultaneously that no value was lost and that each value
// landed in exactly one lane (a duplicate would push some count above one).
func TestUnionEqualsInputNoLossNoDuplication(t *testing.T) {
	t.Parallel()

	const total = 500
	lanes := Split(feed(total), 4)
	got, closed := drainAll(lanes)

	if len(got) != total {
		t.Fatalf("received %d values, want %d", len(got), total)
	}
	want := make(map[int]int, total)
	for i := range total {
		want[i] = 1
	}
	for k, c := range counts(got) {
		if c != 1 {
			t.Fatalf("value %d delivered %d times, want exactly 1", k, c)
		}
	}
	if len(counts(got)) != len(want) {
		t.Fatalf("distinct values %d, want %d", len(counts(got)), len(want))
	}
	for _, ok := range closed {
		if !ok {
			t.Fatal("a lane was not observed closed after input drained")
		}
	}
}

// TestSingleLaneIsPassThrough checks that n==1 degenerates to a pass-through
// that still delivers every value and still closes the one lane.
func TestSingleLaneIsPassThrough(t *testing.T) {
	t.Parallel()

	const total = 50
	lanes := Split(feed(total), 1)
	if len(lanes) != 1 {
		t.Fatalf("Split(in, 1) returned %d lanes, want 1", len(lanes))
	}
	got, closed := drainAll(lanes)
	if len(got) != total {
		t.Fatalf("received %d values, want %d", len(got), total)
	}
	if !closed[0] {
		t.Fatal("the single lane was not closed")
	}
}

// TestEmptyInputClosesAllLanes checks that an already-drained input closes
// every lane immediately, so each drainer's range terminates.
func TestEmptyInputClosesAllLanes(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	close(in) // empty and closed

	lanes := Split(in, 3)
	got, closed := drainAll(lanes)
	if len(got) != 0 {
		t.Fatalf("received %d values from empty input, want 0", len(got))
	}
	for i, ok := range closed {
		if !ok {
			t.Fatalf("lane %d not closed on empty input", i)
		}
	}
}

// TestSlowLaneDoesNotStallOthers pins the load-balancing property: one lane
// whose consumer is blocked must not prevent the other lanes from making
// progress. The blocked lane's forwarder can hold at most one in-flight value,
// so the remaining lanes must together drain everything else before the block
// is released.
func TestSlowLaneDoesNotStallOthers(t *testing.T) {
	t.Parallel()

	const total = 100
	lanes := Split(feed(total), 3)

	release := make(chan struct{})
	var mu sync.Mutex
	var got []int
	progress := make(chan int, total)

	var wg sync.WaitGroup
	// Lane 0: a stalled consumer that reads nothing until release is closed.
	wg.Go(func() {
		<-release
		for v := range lanes[0] {
			mu.Lock()
			got = append(got, v)
			mu.Unlock()
		}
	})
	// Lanes 1 and 2: eager consumers that report each value on progress.
	for _, lane := range lanes[1:] {
		wg.Go(func() {
			for v := range lane {
				mu.Lock()
				got = append(got, v)
				mu.Unlock()
				progress <- v
			}
		})
	}

	// The two eager lanes must drain everything except at most one value held
	// in flight by lane 0's blocked forwarder: at least total-1 must arrive
	// while lane 0 is still stalled.
	deadline := time.After(5 * time.Second)
	for range total - 1 {
		select {
		case <-progress:
		case <-deadline:
			t.Fatal("eager lanes stalled behind the slow lane's blocked consumer")
		}
	}

	close(release) // let lane 0 drain its single in-flight value
	wg.Wait()

	if len(got) != total {
		t.Fatalf("received %d values, want %d", len(got), total)
	}
	for k, c := range counts(got) {
		if c != 1 {
			t.Fatalf("value %d delivered %d times, want exactly 1", k, c)
		}
	}
}
```

## Review

Correct here means three things hold at once: every input value reaches exactly one
lane, no value is lost or duplicated, and every lane is closed exactly once after the
input drains. The multiset-union assertion proves the first two in a single check —
equal counts cannot survive either a loss or a duplicate — and the comma-ok receive
on each lane proves the third. The invariant that makes it all cheap is one-owner-per-lane:
because a lane's sole forwarder is the only goroutine that can close it, `defer close(lane)`
is exactly-once by construction, with none of the `WaitGroup`-plus-lone-closer machinery
fan-in needs. The load-balancing test guards the production bug this pattern exists to
prevent: a distributor built as a fixed round-robin, or one that waits for lane `i` before
offering to lane `i+1`, lets a single slow database connection back up the entire ingest
pipeline; the competing-receive design lets the free lanes absorb the stream while the slow
one holds at most one in-flight record. Run `-race` to confirm the fan-out handoff and the
closes are clean.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out/fan-in vocabulary and the ownership rules this distributor specializes.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the receive-only return type that makes a lane view impossible to send on or close.
- [Go spec: Receive operator](https://go.dev/ref/spec#Receive_operator) — the comma-ok receive the tests use to confirm each lane is closed.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup.Go) — `WaitGroup.Go` (Go 1.25+), used by the demo and tests to drain every lane in its own goroutine.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-rate-limiter-ticker-refill.md](09-rate-limiter-ticker-refill.md) | Next: [11-sequential-segment-replay-concat.md](11-sequential-segment-replay-concat.md)
