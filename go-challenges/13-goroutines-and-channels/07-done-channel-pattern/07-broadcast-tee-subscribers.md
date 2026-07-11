# Exercise 7: Broadcast One Stream to Many Subscribers, Unsubscribe via Done

One event source, many listeners: a config-change or cache-invalidation event must reach every
connected worker, and workers come and go. The mechanism is a broadcaster that fans a single input
channel to N per-subscriber output channels, where each subscriber carries its own `done`. When a
subscriber closes its `done`, its forwarder detaches — the broadcaster stops delivering to it and
closes its channel — without stalling the other subscribers or the source.

## What you'll build

```text
teebroadcast/                      independent module: example.com/teebroadcast
  go.mod
  broadcast.go                     Broadcaster; Subscribe(done) <-chan int; Start(); per-sub guarded send
  cmd/
    demo/
      main.go                      runnable demo: two subscribers each sum the same stream
  broadcast_test.go                receives-all and subscriber-leaves-early (detach) tests; -race
```

Files: `broadcast.go`, `cmd/demo/main.go`, `broadcast_test.go`.
Implement: a `Broadcaster` over an input channel with `Subscribe(done <-chan struct{}) <-chan int` and `Start()`, delivering each input value to every live subscriber via `select { case sub.ch <- v: case <-sub.done: detach }`.
Test: two subscribers both receive every value; one subscriber closing its `done` detaches (its channel closes) while the other still receives all remaining values.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/teebroadcast/cmd/demo
cd ~/go-exercises/teebroadcast
go mod init example.com/teebroadcast
```

### Per-subscriber done is what enables clean detach

Each subscriber owns its own `done` channel. The broadcaster's pump reads one value from the input
and delivers it to every live subscriber with a two-case select:

```go
select {
case sub.ch <- v:
	// delivered to this subscriber
case <-sub.done:
	sub.dead = true
	close(sub.ch)
}
```

If the subscriber is reading, the send succeeds. If the subscriber has closed its `done` — it left —
the `<-sub.done` case fires immediately (a closed channel is always ready), the broadcaster marks that
subscriber dead, closes its output so the departed subscriber's own range loop terminates, and moves
on to the next subscriber. Crucially, delivering to a departed subscriber never blocks: without the
`sub.done` guard, a subscriber that stopped reading would block the pump on `sub.ch <- v` forever,
stalling delivery to *every other* subscriber and backing up the source. The per-subscriber done is
what turns "a subscriber left" from a system-wide stall into a local detach.

The pump runs in one goroutine. Subscribers are registered before `Start`, so the `subs` slice and
each subscriber's `dead` flag are only ever touched by the pump goroutine — no lock is needed on them.
When the input channel is drained, a `defer` closes every still-live subscriber's channel exactly once,
so every subscriber sees a clean end.

Create `broadcast.go`:

```go
package teebroadcast

type subscriber struct {
	ch   chan int
	done <-chan struct{}
	dead bool
}

// Broadcaster fans a single input channel out to many subscriber channels.
// Each subscriber carries its own done; closing it detaches that subscriber
// without disturbing the others or the source.
type Broadcaster struct {
	in   <-chan int
	subs []*subscriber
}

// NewBroadcaster returns a Broadcaster reading from in. Register subscribers
// with Subscribe, then call Start.
func NewBroadcaster(in <-chan int) *Broadcaster {
	return &Broadcaster{in: in}
}

// Subscribe registers a subscriber and returns its output channel. Closing done
// detaches the subscriber: the broadcaster stops delivering to it and closes the
// returned channel. Call Subscribe before Start.
func (b *Broadcaster) Subscribe(done <-chan struct{}) <-chan int {
	s := &subscriber{ch: make(chan int), done: done}
	b.subs = append(b.subs, s)
	return s.ch
}

// Start launches the pump. It delivers every input value to every live
// subscriber, detaching any whose done has closed, and closes all remaining
// subscriber channels when the input is drained.
func (b *Broadcaster) Start() {
	go func() {
		defer func() {
			for _, s := range b.subs {
				if !s.dead {
					close(s.ch)
				}
			}
		}()
		for v := range b.in {
			for _, s := range b.subs {
				if s.dead {
					continue
				}
				select {
				case s.ch <- v:
				case <-s.done:
					s.dead = true
					close(s.ch)
				}
			}
		}
	}()
}
```

### The runnable demo

Two subscribers each sum the same stream of five values; both see all of it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/teebroadcast"
)

func main() {
	in := make(chan int)
	b := teebroadcast.NewBroadcaster(in)

	done := make(chan struct{})
	defer close(done)
	chA := b.Subscribe(done)
	chB := b.Subscribe(done)
	b.Start()

	var wg sync.WaitGroup
	wg.Add(2)
	var sumA, sumB int
	go func() {
		defer wg.Done()
		for v := range chA {
			sumA += v
		}
	}()
	go func() {
		defer wg.Done()
		for v := range chB {
			sumB += v
		}
	}()

	for i := 1; i <= 5; i++ {
		in <- i
	}
	close(in)
	wg.Wait()

	fmt.Printf("subscriber A sum: %d\n", sumA)
	fmt.Printf("subscriber B sum: %d\n", sumB)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subscriber A sum: 15
subscriber B sum: 15
```

### Tests

`TestBroadcastReceivesAll` runs two subscribers reading concurrently and asserts both accumulate the
full stream. `TestSubscriberLeavesEarly` is the detach proof: both subscribers read the first value,
then subscriber A closes its `done` and stops reading; the test feeds two more values, which only B
receives, and asserts A's channel is closed (its forwarder detached) while B saw every value. Because
the pump delivers to A before B in registration order and A's send is guarded by its now-closed `done`,
delivery to A never blocks and B keeps receiving.

Create `broadcast_test.go`:

```go
package teebroadcast

import (
	"sync"
	"testing"
)

func TestBroadcastReceivesAll(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	b := NewBroadcaster(in)
	done := make(chan struct{})
	defer close(done)
	chA := b.Subscribe(done)
	chB := b.Subscribe(done)
	b.Start()

	var wg sync.WaitGroup
	wg.Add(2)
	var sumA, sumB int
	go func() {
		defer wg.Done()
		for v := range chA {
			sumA += v
		}
	}()
	go func() {
		defer wg.Done()
		for v := range chB {
			sumB += v
		}
	}()

	for i := 1; i <= 4; i++ {
		in <- i
	}
	close(in)
	wg.Wait()

	if sumA != 10 || sumB != 10 {
		t.Fatalf("sums = A:%d B:%d, want 10 and 10", sumA, sumB)
	}
}

func TestSubscriberLeavesEarly(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	b := NewBroadcaster(in)
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	defer close(doneB)
	chA := b.Subscribe(doneA)
	chB := b.Subscribe(doneB)
	b.Start()

	// Value 1: both subscribers read it (A is delivered first, then B).
	go func() { in <- 1 }()
	if v := <-chA; v != 1 {
		t.Fatalf("A first value = %d, want 1", v)
	}
	if v := <-chB; v != 1 {
		t.Fatalf("B first value = %d, want 1", v)
	}

	// A leaves and stops reading.
	close(doneA)

	// Values 2 and 3 go only to B; delivery to A takes the done branch.
	got := []int{}
	go func() {
		in <- 2
		in <- 3
		close(in)
	}()
	for v := range chB {
		got = append(got, v)
	}

	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("B received %v after A left, want [2 3]", got)
	}
	// A's channel must be closed: its forwarder detached.
	if _, ok := <-chA; ok {
		t.Fatal("A's channel still open after A closed its done")
	}
}
```

## Review

The broadcaster is correct when every live subscriber receives every value and when a subscriber that
closes its `done` detaches locally — its channel closes and the pump keeps serving the rest. The
receives-all test proves the fan-out; the leaves-early test proves the detach, and it hinges on the
guarded send: with a bare `sub.ch <- v`, A's departure would block the pump and B would never see values
2 and 3, so the test would hang. Registering subscribers before `Start` keeps the `subs` slice single-
goroutine, which is why no lock guards it; run `go test -race` to confirm there is no data race between
the pump and the concurrent readers. A dynamic subscriber registry that admits `Subscribe` calls after
`Start` would need a mutex around `subs` — a deliberate scope limit kept out of this exercise.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation (the tee stage)](https://go.dev/blog/pipelines)
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)
- [Go Language Spec: Close](https://go.dev/ref/spec#Close)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-nonblocking-send-avoid-leak.md](06-nonblocking-send-avoid-leak.md) | Next: [08-graceful-shutdown-coordinator.md](08-graceful-shutdown-coordinator.md)
