# Exercise 8: A Pub/Sub Dispatcher That Broadcasts One Event to N Subscribers

An in-process event bus fans one event out to every registered subscriber. The
hard part is that subscribers consume at different rates, and a single slow one must
not head-of-line-block delivery to everyone else. This module builds a dispatcher
whose `Broadcast` delivers to whichever subscriber is ready next — arbitrating a
runtime-sized set of *send* operations with `reflect.Select` — and drops a
subscriber that is still not ready by a delivery deadline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
pubsub/                         module example.com/pubsub
  go.mod                        go 1.26
  pubsub.go                     Dispatcher; Subscribe, Unsubscribe, Broadcast (reflect send)
  cmd/
    demo/
      main.go                   three subscribers; broadcast one event; print
  pubsub_test.go                reaches-all, slow-does-not-block, unsubscribe-stops
```

Files: `pubsub.go`, `cmd/demo/main.go`, `pubsub_test.go`.
Implement: a `Dispatcher` with `Subscribe`, `Unsubscribe`, and `Broadcast(evt) int` that delivers to a runtime-sized set of subscriber channels without head-of-line blocking.
Test: every subscriber receives the event once; a blocked subscriber does not stop the others; an unsubscribed one receives nothing.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pubsub/cmd/demo
cd ~/go-exercises/pubsub
go mod init example.com/pubsub
```

## Sending to a dynamic set without head-of-line blocking

The number of subscribers is a runtime value, so `Broadcast` cannot use a static
`select`; it builds a `reflect.SelectCase` per subscriber, this time with
`Dir: SelectSend` and `Send` set to the event. It appends one receive case on a
delivery-deadline timer. Then it loops, calling `reflect.Select` repeatedly:

- Each iteration delivers the event to whichever subscriber is *ready to receive
  next* — that is the anti-head-of-line-blocking property. A fast subscriber does
  not have to wait behind a slow one; the runtime picks a ready send, and the slow
  subscriber's send simply is not chosen until it becomes ready.
- After a successful send, that subscriber's case is disabled so it is not sent to
  twice. The disable trick for `reflect.Select` is to set the case's `Chan` to a
  zero `reflect.Value`: the docs specify that a send case with a zero `Chan` is
  ignored. That is the reflection equivalent of nil-ing a channel to turn off a
  static `select` case.
- If the timer case is chosen, the deadline elapsed with subscribers still not
  ready — a slow or stuck consumer. `Broadcast` stops and returns how many it
  reached, dropping the laggards rather than blocking the whole bus on them. That
  "drop a slow subscriber past a deadline" policy is what keeps an event bus from
  letting one wedged handler stall every other.

`Broadcast` snapshots the subscriber channels under the mutex and then sends
outside the lock, so a slow send never holds the lock and blocks `Subscribe`.
`chan struct{}`-style unregistration is folded in through `Unsubscribe`, which
removes the subscriber from the map and closes its channel so a ranging consumer
sees the stream end.

Create `pubsub.go`:

```go
package pubsub

import (
	"reflect"
	"sync"
	"time"
)

// Event is one message delivered to subscribers.
type Event struct {
	Topic   string
	Payload string
}

// Dispatcher is an in-process event bus. Broadcast fans one event out to every
// subscriber, dropping any that is not ready within the delivery deadline.
type Dispatcher struct {
	mu       sync.Mutex
	subs     map[int]chan Event
	nextID   int
	deadline time.Duration
}

// New returns a Dispatcher whose Broadcast gives each round up to deadline to
// reach every subscriber before dropping the ones still not ready.
func New(deadline time.Duration) *Dispatcher {
	return &Dispatcher{subs: make(map[int]chan Event), deadline: deadline}
}

// Subscribe registers a subscriber with the given channel buffer and returns its
// id and receive channel. A larger buffer tolerates a slower consumer.
func (d *Dispatcher) Subscribe(buffer int) (int, <-chan Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.nextID
	d.nextID++
	ch := make(chan Event, buffer)
	d.subs[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber and closes its channel so a ranging consumer
// sees the stream end. It is a no-op for an unknown id.
func (d *Dispatcher) Unsubscribe(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.subs[id]; ok {
		delete(d.subs, id)
		close(ch)
	}
}

// Broadcast delivers evt to every current subscriber, arbitrating the sends with
// reflect.Select so a slow subscriber does not head-of-line-block the others. It
// returns the number of subscribers reached; any not ready within the deadline
// are dropped.
func (d *Dispatcher) Broadcast(evt Event) int {
	d.mu.Lock()
	chans := make([]chan Event, 0, len(d.subs))
	for _, ch := range d.subs {
		chans = append(chans, ch)
	}
	d.mu.Unlock()

	if len(chans) == 0 {
		return 0
	}

	// One send case per subscriber, plus a trailing receive case on the deadline.
	cases := make([]reflect.SelectCase, len(chans)+1)
	for i, ch := range chans {
		cases[i] = reflect.SelectCase{
			Dir:  reflect.SelectSend,
			Chan: reflect.ValueOf(ch),
			Send: reflect.ValueOf(evt),
		}
	}
	timer := time.NewTimer(d.deadline)
	defer timer.Stop()
	timerIdx := len(chans)
	cases[timerIdx] = reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(timer.C),
	}

	delivered := 0
	for remaining := len(chans); remaining > 0; remaining-- {
		chosen, _, _ := reflect.Select(cases)
		if chosen == timerIdx {
			break // deadline elapsed; drop the subscribers still not ready
		}
		delivered++
		// Disable this send case: a zero Chan value makes reflect.Select ignore it.
		cases[chosen].Chan = reflect.Value{}
	}
	return delivered
}
```

## The runnable demo

The demo registers three buffered subscribers, broadcasts one event, and prints
what each received. All three are buffered, so all three are reached.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pubsub"
)

func main() {
	d := pubsub.New(50_000_000) // 50ms delivery deadline (nanoseconds)

	chans := make([]<-chan pubsub.Event, 0, 3)
	for range 3 {
		_, ch := d.Subscribe(1)
		chans = append(chans, ch)
	}

	reached := d.Broadcast(pubsub.Event{Topic: "orders", Payload: "created"})
	fmt.Printf("reached %d subscribers\n", reached)

	for i, ch := range chans {
		evt := <-ch
		fmt.Printf("subscriber %d got %s/%s\n", i, evt.Topic, evt.Payload)
	}
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
reached 3 subscribers
subscriber 0 got orders/created
subscriber 1 got orders/created
subscriber 2 got orders/created
```

## Tests

`TestBroadcastReachesAllSubscribers` registers several buffered subscribers and
asserts each receives the event exactly once and `Broadcast` reports reaching all
of them. `TestBroadcastSlowSubscriberDoesNotBlockOthers` mixes buffered subscribers
with one unbuffered subscriber that nobody reads; it asserts the buffered ones
still receive and the blocked one is dropped after the deadline, proving the send
arbitration does not head-of-line-block. `TestUnsubscribeStopsDelivery` unsubscribes
a subscriber and asserts its channel is closed and it receives no event.
`TestConcurrentSubscribeBroadcast` runs subscribes and broadcasts concurrently under
`-race`.

Create `pubsub_test.go`:

```go
package pubsub

import (
	"sync"
	"testing"
	"time"
)

func TestBroadcastReachesAllSubscribers(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	const m = 5
	chans := make([]<-chan Event, 0, m)
	for range m {
		_, ch := d.Subscribe(1) // buffered: ready to receive immediately
		chans = append(chans, ch)
	}

	evt := Event{Topic: "t", Payload: "p"}
	if reached := d.Broadcast(evt); reached != m {
		t.Fatalf("Broadcast reached %d subscribers, want %d", reached, m)
	}

	for i, ch := range chans {
		select {
		case got := <-ch:
			if got != evt {
				t.Fatalf("subscriber %d got %+v, want %+v", i, got, evt)
			}
		default:
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestBroadcastSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	d := New(50 * time.Millisecond)

	const fastCount = 4
	fast := make([]<-chan Event, 0, fastCount)
	for range fastCount {
		_, ch := d.Subscribe(1)
		fast = append(fast, ch)
	}
	// One unbuffered subscriber that no one reads: its send is never ready.
	_, slow := d.Subscribe(0)
	_ = slow

	evt := Event{Topic: "t", Payload: "p"}
	reached := d.Broadcast(evt)
	if reached != fastCount {
		t.Fatalf("Broadcast reached %d, want %d (slow subscriber must be dropped)", reached, fastCount)
	}

	for i, ch := range fast {
		select {
		case got := <-ch:
			if got != evt {
				t.Fatalf("fast subscriber %d got %+v, want %+v", i, got, evt)
			}
		default:
			t.Fatalf("fast subscriber %d blocked behind the slow one", i)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	id, ch := d.Subscribe(1)
	d.Unsubscribe(id)

	if reached := d.Broadcast(Event{Topic: "t", Payload: "p"}); reached != 0 {
		t.Fatalf("Broadcast reached %d after unsubscribe, want 0", reached)
	}

	// The channel is closed and was never sent to: receive yields ok == false.
	if _, ok := <-ch; ok {
		t.Fatal("unsubscribed channel delivered an event")
	}
}

func TestConcurrentSubscribeBroadcast(t *testing.T) {
	t.Parallel()

	d := New(20 * time.Millisecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { d.Subscribe(4) })
	}
	for range 8 {
		wg.Go(func() { d.Broadcast(Event{Topic: "t", Payload: "p"}) })
	}
	wg.Wait()
}
```

## Review

The dispatcher is correct when a broadcast reaches every ready subscriber exactly
once, a slow or stuck subscriber is dropped at the deadline instead of stalling the
bus, and an unsubscribed channel is closed and receives nothing further. The
load-bearing detail is the send-case disable: setting `cases[chosen].Chan` to a
zero `reflect.Value` after each successful send is what stops a subscriber from
being delivered to twice, and it is the reflection analogue of nil-ing a static
`select` case. Sending outside the lock keeps a slow consumer from blocking
`Subscribe`. A production bus would also refcount subscribers so `Unsubscribe`
cannot race a `Broadcast` mid-send; here that pairing is left sequential, which
`-race` confirms is clean under the test's concurrency.

## Resources

- [reflect.Select with SelectSend](https://pkg.go.dev/reflect#Select) — send-case arbitration and the zero-`Chan` disable rule.
- [reflect.SelectCase](https://pkg.go.dev/reflect#SelectCase) — the `Dir`, `Chan`, and `Send` fields.
- [Go Specification: Send statements](https://go.dev/ref/spec#Send_statements) — send-on-closed panics, which is why Unsubscribe must not race a send.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-drain-without-busyspin.md](07-drain-without-busyspin.md) | Next: [09-select-fairness-audit.md](09-select-fairness-audit.md)
