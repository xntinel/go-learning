# Exercise 2: Safe Subscribe and Unsubscribe Under Concurrency

The event bus routes events by string key; this exercise zooms in on the single hardest sub-problem in that design and builds it in isolation as a typed broadcaster: letting subscribers come and go while values are being broadcast, with no "send on closed channel" panic, no "close of closed channel" panic, and no data race. The deliverable is a generic `Broadcaster[T]` whose `Unsubscribe` is idempotent and whose safety is proven by a test that hammers subscribe, unsubscribe, and broadcast from many goroutines under the race detector.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
broadcast.go         Broadcaster[T], Subscription[T], Subscribe, Unsubscribe, Broadcast, Close
cmd/
  demo/
    main.go          two subscribers, broadcast, idempotent unsubscribe, observe
broadcast_test.go    idempotent-unsubscribe + a -race subscribe/unsubscribe/broadcast sweep
```

- Files: `broadcast.go`, `cmd/demo/main.go`, `broadcast_test.go`.
- Implement: `Broadcaster[T]` with `Subscribe(buf int) *Subscription[T]`, `Broadcast(T)`, `Close()`, `Len() int`, and `Subscription[T].Unsubscribe()`.
- Test: `broadcast_test.go` proves unsubscribe is idempotent, that `Close` closes live channels, and that concurrent subscribe/unsubscribe/broadcast is race-free.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p safe-subscribe-unsubscribe/cmd/demo && cd safe-subscribe-unsubscribe
go mod init example.com/safe-subscribe-unsubscribe
```

### Identity by id, exactly-once close, and the lock that ties it together

When subscribers are transient, the publisher needs to find and remove a specific subscriber cheaply, and removal must close that subscriber's channel exactly once even when an `Unsubscribe` races a bus-wide `Close`. A slice of channels (as in the event bus) makes removal an O(n) scan and identifies a subscriber by channel value; a `map[int]chan T` keyed by a monotonically increasing id makes removal O(1) and gives every subscription a stable identity that never collides. `Subscribe` allocates the next id under the lock, stores the channel, and hands the caller a `*Subscription[T]` carrying that id.

Three rules keep it safe. First, `Unsubscribe` is wrapped in a `sync.Once` so a caller may call it as many times as it likes — a `defer s.Unsubscribe()` plus an explicit early one is ordinary code — and only the first call does anything. Second, inside that once-guarded body the close is gated on the id still being present in the map: `Close` may have removed and closed the channel already, in which case `Unsubscribe` finds nothing and does nothing. Whoever deletes the id from the map first owns the close. Third, `Broadcast` does its non-blocking sends while holding the same lock that `Unsubscribe` and `Close` take, so a channel can never be closed in the window between selecting it and sending to it — the same structural defense against "send on closed channel" the event bus uses.

The closed-bus case needs one guard so it cannot collide with a real subscriber. A `Subscribe` on an already-closed broadcaster returns an immediately-closed channel and a subscription whose id is the zero value; ids handed to real subscribers start at 1, so the zero id is never in the map and that subscription's `Unsubscribe` is a safe no-op.

Create `broadcast.go`:

```go
package broadcast

import "sync"

// Broadcaster fans each broadcast value out to every current subscriber. It is
// safe for concurrent Subscribe, Unsubscribe, Broadcast, and Close.
type Broadcaster[T any] struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan T
	closed bool
}

// Subscription is a handle to one subscriber. Receive on C; call Unsubscribe to
// detach. Unsubscribe is idempotent.
type Subscription[T any] struct {
	C    <-chan T
	b    *Broadcaster[T]
	ch   chan T
	id   int
	once sync.Once
}

// New returns an empty Broadcaster. Subscriber ids start at 1 so the zero id can
// never name a live subscriber.
func New[T any]() *Broadcaster[T] {
	return &Broadcaster[T]{nextID: 1, subs: make(map[int]chan T)}
}

// Subscribe registers a new subscriber with a buffer of at least 1 and returns
// its handle. On a closed broadcaster it returns a handle whose channel is
// already closed and whose Unsubscribe is a no-op.
func (b *Broadcaster[T]) Subscribe(buf int) *Subscription[T] {
	if buf < 1 {
		buf = 1
	}
	ch := make(chan T, buf)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return &Subscription[T]{C: ch, b: b, ch: ch}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	return &Subscription[T]{C: ch, b: b, ch: ch, id: id}
}

// Unsubscribe detaches the subscriber and closes its channel exactly once. It is
// safe to call repeatedly and safe to call concurrently with Close.
func (s *Subscription[T]) Unsubscribe() {
	s.once.Do(func() {
		s.b.mu.Lock()
		defer s.b.mu.Unlock()
		if ch, ok := s.b.subs[s.id]; ok {
			delete(s.b.subs, s.id)
			close(ch)
		}
	})
}

// Broadcast delivers v to every subscriber with a non-blocking send; a full
// subscriber is skipped. The send runs under the lock so no channel can be
// closed between selecting it and sending to it.
func (b *Broadcaster[T]) Broadcast(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

// Close marks the broadcaster closed and closes every live subscriber channel
// exactly once. It is idempotent.
func (b *Broadcaster[T]) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}

// Len reports the number of live subscribers.
func (b *Broadcaster[T]) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
```

`Subscription` carries both `C` (the receive-only view the caller uses) and `ch` (the bidirectional channel, kept only so the closed-bus handle has a concrete value to expose). The `sync.Once` plus the `subs[s.id]` membership check are the whole exactly-once argument: repeated `Unsubscribe` calls collapse to one, and whichever of `Unsubscribe`/`Close` deletes the id first is the one that closes the channel. Because `Subscription` embeds a `sync.Once` (which must not be copied), it is always handled through the `*Subscription[T]` pointer `Subscribe` returns; `go vet`'s copylocks check would flag any copy.

### The runnable demo

The demo is deterministic: it broadcasts into buffered channels and drains them in a fixed order from `main`. It shows two subscribers receiving the same values, an idempotent double `Unsubscribe`, that a later broadcast reaches only the survivor, and that both `Unsubscribe` and `Close` leave channels closed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/safe-subscribe-unsubscribe"
)

func main() {
	b := broadcast.New[string]()

	s1 := b.Subscribe(4)
	s2 := b.Subscribe(4)
	fmt.Printf("subscribers: %d\n", b.Len())

	b.Broadcast("tick-1")
	b.Broadcast("tick-2")

	fmt.Printf("s1: %s\n", <-s1.C)
	fmt.Printf("s1: %s\n", <-s1.C)
	fmt.Printf("s2: %s\n", <-s2.C)
	fmt.Printf("s2: %s\n", <-s2.C)

	s2.Unsubscribe()
	s2.Unsubscribe() // idempotent: no panic, no effect.
	fmt.Printf("subscribers after unsubscribe: %d\n", b.Len())

	b.Broadcast("tick-3")
	fmt.Printf("s1: %s\n", <-s1.C)
	if _, ok := <-s2.C; !ok {
		fmt.Println("s2 channel closed")
	}

	b.Close()
	if _, ok := <-s1.C; !ok {
		fmt.Println("s1 channel closed after Close")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subscribers: 2
s1: tick-1
s1: tick-2
s2: tick-1
s2: tick-2
subscribers after unsubscribe: 1
s1: tick-3
s2 channel closed
s1 channel closed after Close
```

### Tests

`TestUnsubscribe_Idempotent` calls `Unsubscribe` three times and asserts no panic and a closed channel. `TestClose_ClosesLiveChannels` proves a survivor's channel is closed by `Close`. `TestConcurrentChurn` is the centerpiece: many goroutines broadcast while many others repeatedly subscribe, drain a little, and unsubscribe twice — the pattern that would trip "send on closed channel" or "close of closed channel" without the lock and the once-guard. Run under `-race`.

Create `broadcast_test.go`:

```go
package broadcast

import (
	"sync"
	"testing"
)

func TestUnsubscribe_Idempotent(t *testing.T) {
	t.Parallel()

	b := New[int]()
	s := b.Subscribe(1)
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}

	s.Unsubscribe()
	s.Unsubscribe()
	s.Unsubscribe()

	if b.Len() != 0 {
		t.Fatalf("Len after unsubscribe = %d, want 0", b.Len())
	}
	if _, ok := <-s.C; ok {
		t.Fatal("expected channel closed after unsubscribe")
	}
}

func TestClose_ClosesLiveChannels(t *testing.T) {
	t.Parallel()

	b := New[int]()
	s := b.Subscribe(1)
	b.Close()
	b.Close() // idempotent.

	if _, ok := <-s.C; ok {
		t.Fatal("expected channel closed after Close")
	}
	// Subscribing to a closed broadcaster yields an already-closed channel.
	s2 := b.Subscribe(1)
	if _, ok := <-s2.C; ok {
		t.Fatal("expected closed channel from Subscribe on closed broadcaster")
	}
	s2.Unsubscribe() // must be a safe no-op.
}

func TestConcurrentChurn(t *testing.T) {
	t.Parallel()

	b := New[int]()
	defer b.Close()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 500 {
				b.Broadcast(j)
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				s := b.Subscribe(4)
				select {
				case <-s.C:
				default:
				}
				s.Unsubscribe()
				s.Unsubscribe() // idempotent under contention.
			}
		}()
	}
	wg.Wait()
}
```

## Review

The broadcaster is correct when three properties hold together. Identity is stable: each subscription owns a unique id from a counter advanced under the lock, so removal is O(1) and never targets the wrong subscriber. The close is exactly-once: `sync.Once` collapses repeated `Unsubscribe` calls, and the `subs[s.id]` membership check means whichever of `Unsubscribe`/`Close` deletes the id first is the sole closer — the other is a no-op. And the fan-out is panic-free: `Broadcast` sends under the same lock that closes channels, so "send on closed channel" cannot happen. `TestConcurrentChurn` under `-race` exercises all three at once; a missing `once`, an unconditional close, or an unlocked broadcast each turns it red.

Common mistakes for this feature. The first is identifying subscribers by channel value in a slice and forgetting that two different subscriptions can have buffers that make the scan ambiguous under churn; a map keyed by id removes the ambiguity and the O(n) scan. The second is closing in `Unsubscribe` without the `sync.Once` and the membership check, which double-closes the moment two cancels race or a cancel races `Close`. The third is reusing the zero id for the closed-bus handle while real ids also start at zero, so a no-op unsubscribe accidentally closes a live subscriber's channel — starting real ids at 1 keeps the sentinel distinct.

## Resources

- [`sync.Once`](https://pkg.go.dev/sync#Once) — the exactly-once primitive that makes `Unsubscribe` idempotent.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` instruments and why the churn test must run under it.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before rules that make the lock-guarded map and channel operations well-defined across goroutines.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — closing channels to signal shutdown to a dynamic set of goroutines.

---

Back to [01-event-bus.md](01-event-bus.md) | Next: [03-non-blocking-fanout.md](03-non-blocking-fanout.md)
