# Exercise 7: Load shedding via non-blocking priority send under backpressure

Priority applies to the send direction too. When a downstream buffer fills, an
ingress must not block and build an unbounded queue — it sheds. This exercise
builds the send-side mirror of the priority peek: a non-blocking send that
enqueues to a bounded downstream channel and, when full, sheds latency-sensitive
high-priority items (with a counter) while diverting durable low-priority items to
an overflow / dead-letter path.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
loadshed/                    module example.com/loadshed
  go.mod
  shedder.go                 Priority; Outcome; type Shedder; NewShedder; Submit; counters
  cmd/
    demo/
      main.go                overloads a small buffer, prints enqueued/diverted/dropped
  shedder_test.go            exact counts, overflow-then-shed, space-frees, concurrent conservation
```

Files: `shedder.go`, `cmd/demo/main.go`, `shedder_test.go`.
Implement: `Shedder` with a bounded downstream channel and an overflow channel;
`Submit(it, priority) Outcome` that tries a non-blocking send to downstream and,
on a full buffer, sheds `High` items (counting drops) or diverts `Low` items to
overflow (shedding only if overflow is also full). Atomic counters for enqueued,
diverted, dropped.
Test: exact counts under a known input; overflow fills then sheds; freeing
downstream space lets sends succeed again; under concurrent submits, enqueued +
diverted + dropped equals the total (no send blocks or is lost silently).
Verify: `go test -count=1 -race ./...`

### Non-blocking send is the backpressure primitive

`select` with `default` on a *send* case is the primitive for backpressure:

```go
select {
case downstream <- item:
	// enqueued
default:
	// downstream full: do not block — decide what to do instead
}
```

The decision on a full buffer is where priority lives, and the policy here encodes
a real distinction between two kinds of traffic:

- **High priority is latency-sensitive.** Think live metrics or a real-time
  feed: an item that cannot be delivered *now* is stale by the time the buffer
  drains, so the right move is to shed it — drop it and increment a counter, so
  the loss is observable. Blocking to deliver a stale high-priority item would
  back-pressure into the caller and take the whole ingress down.
- **Low priority is durability-sensitive.** Think an audit log or a billing
  event: it is not urgent, but it must not be lost. On a full downstream buffer it
  is diverted to an overflow / dead-letter channel for later processing, and only
  shed if overflow is *also* full.

The counter is not optional. A non-blocking send that drops silently loses data
with no signal — the classic "downstream traffic mysteriously dropped 4%"
incident. Every shed increments `dropped`; every divert increments `diverted`.
Those counters are the difference between a shedding system you can operate and
one that is quietly losing data.

Create `shedder.go`:

```go
package loadshed

import "sync/atomic"

// Priority classifies an item's handling under backpressure.
type Priority int

const (
	Low  Priority = iota // durability-sensitive: divert to overflow when full
	High                 // latency-sensitive: shed when full
)

// Outcome is what happened to a submitted item.
type Outcome int

const (
	Enqueued Outcome = iota // accepted onto the downstream buffer
	Diverted                // downstream full: routed to overflow (low priority)
	Shed                    // dropped (high priority, or overflow also full)
)

func (o Outcome) String() string {
	switch o {
	case Enqueued:
		return "enqueued"
	case Diverted:
		return "diverted"
	default:
		return "shed"
	}
}

// Item is a unit of ingress traffic.
type Item struct {
	N int
}

// Shedder is a non-blocking ingress that never blocks the caller: it enqueues to
// a bounded downstream buffer, sheds high-priority items when full, and diverts
// low-priority items to an overflow path. Its counters are safe to read
// concurrently.
type Shedder struct {
	down     chan Item
	overflow chan Item
	enqueued atomic.Int64
	diverted atomic.Int64
	dropped  atomic.Int64
}

// NewShedder builds a Shedder with the given downstream and overflow capacities.
func NewShedder(downCap, overflowCap int) *Shedder {
	return &Shedder{
		down:     make(chan Item, downCap),
		overflow: make(chan Item, overflowCap),
	}
}

// Submit accepts an item without ever blocking, returning what happened to it.
func (s *Shedder) Submit(it Item, p Priority) Outcome {
	select {
	case s.down <- it:
		s.enqueued.Add(1)
		return Enqueued
	default:
	}
	// Downstream is full.
	if p == High {
		s.dropped.Add(1) // latency-sensitive: shed rather than deliver stale
		return Shed
	}
	select {
	case s.overflow <- it:
		s.diverted.Add(1) // durability-sensitive: keep for later
		return Diverted
	default:
		s.dropped.Add(1) // overflow full too: nothing left to do but shed
		return Shed
	}
}

// Enqueued, Diverted, and Dropped report the running counters.
func (s *Shedder) Enqueued() int64 { return s.enqueued.Load() }
func (s *Shedder) Diverted() int64 { return s.diverted.Load() }
func (s *Shedder) Dropped() int64  { return s.dropped.Load() }

// Downstream and Overflow expose the channels for a consumer to drain.
func (s *Shedder) Downstream() <-chan Item { return s.down }
func (s *Shedder) Overflow() <-chan Item   { return s.overflow }
```

### The runnable demo

The demo overloads a downstream buffer of 3 and an overflow of 4 without draining
either. Five high items fill downstream (3) and shed the rest (2); five low items
find downstream still full, so four divert to overflow and the fifth sheds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loadshed"
)

func main() {
	s := loadshed.NewShedder(3, 4)

	for i := range 5 {
		s.Submit(loadshed.Item{N: i}, loadshed.High)
	}
	for i := range 5 {
		s.Submit(loadshed.Item{N: i}, loadshed.Low)
	}

	fmt.Printf("enqueued=%d diverted=%d dropped=%d\n",
		s.Enqueued(), s.Diverted(), s.Dropped())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enqueued=3 diverted=4 dropped=3
```

### Tests

`TestExactCounts` submits a known sequence and asserts every counter exactly,
including the transition where freeing a downstream slot lets a subsequent high
submit succeed again. `TestOverflowThenShed` fills both buffers and asserts a low
submit sheds once overflow is full. `TestConcurrentConservation` submits from many
goroutines and asserts the conservation law — enqueued + diverted + dropped equals
the total submitted — which under `-race` also proves the counters and channel
sends are safe and that no submit blocked or vanished.

Create `shedder_test.go`:

```go
package loadshed

import (
	"sync"
	"testing"
)

func TestExactCounts(t *testing.T) {
	t.Parallel()

	s := NewShedder(2, 5)

	// Two high items fill downstream (cap 2).
	if got := s.Submit(Item{N: 0}, High); got != Enqueued {
		t.Fatalf("submit 0 = %v, want enqueued", got)
	}
	if got := s.Submit(Item{N: 1}, High); got != Enqueued {
		t.Fatalf("submit 1 = %v, want enqueued", got)
	}
	// Third high: downstream full, latency-sensitive -> shed.
	if got := s.Submit(Item{N: 2}, High); got != Shed {
		t.Fatalf("submit 2 = %v, want shed", got)
	}
	// A low item: downstream full, durability-sensitive -> divert.
	if got := s.Submit(Item{N: 3}, Low); got != Diverted {
		t.Fatalf("submit 3 = %v, want diverted", got)
	}
	// Free a downstream slot; the next high enqueues again.
	<-s.Downstream()
	if got := s.Submit(Item{N: 4}, High); got != Enqueued {
		t.Fatalf("submit 4 = %v, want enqueued after draining", got)
	}

	if s.Enqueued() != 3 || s.Diverted() != 1 || s.Dropped() != 1 {
		t.Fatalf("counts = enq %d div %d drop %d, want 3/1/1",
			s.Enqueued(), s.Diverted(), s.Dropped())
	}
}

func TestOverflowThenShed(t *testing.T) {
	t.Parallel()

	s := NewShedder(1, 1)
	s.Submit(Item{N: 0}, High) // fills downstream (cap 1)
	if got := s.Submit(Item{N: 1}, Low); got != Diverted {
		t.Fatalf("submit 1 = %v, want diverted", got)
	}
	// Downstream full and overflow full: a low item now sheds.
	if got := s.Submit(Item{N: 2}, Low); got != Shed {
		t.Fatalf("submit 2 = %v, want shed", got)
	}
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", s.Dropped())
	}
}

func TestConcurrentConservation(t *testing.T) {
	t.Parallel()

	s := NewShedder(8, 8)
	const goroutines, per = 16, 100
	const total = goroutines * per

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range per {
				p := Low
				if (g+i)%2 == 0 {
					p = High
				}
				s.Submit(Item{N: i}, p)
			}
		}()
	}
	wg.Wait()

	sum := s.Enqueued() + s.Diverted() + s.Dropped()
	if sum != total {
		t.Fatalf("enq+div+drop = %d, want %d (an item was lost or double-counted)", sum, total)
	}
}
```

## Review

The shedder is correct when every submit is accounted for exactly — enqueued,
diverted, or dropped — and when no submit ever blocks. `TestConcurrentConservation`
is the load-bearing check: the conservation law can only hold if each `Submit`
takes exactly one branch and increments exactly one counter, and `-race` confirms
the atomics and channel sends are synchronized. The mistake to avoid is a
non-blocking send whose `default` drops without a counter — the data is gone with
no signal. The high-versus-low policy is deliberate and worth restating: high is
shed on backpressure because a late high-priority item is worthless, while low is
diverted because it is durable and can wait; invert that only if your traffic's
value profile is inverted.

## Resources

- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking send via a `default` case.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — the lock-free shed/divert/enqueue counters.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — load shedding and graceful degradation under backpressure.

---

Back to [06-rate-limited-priority-dispatcher.md](06-rate-limited-priority-dispatcher.md) | Next: [08-dynamic-priority-reflect-select.md](08-dynamic-priority-reflect-select.md)
