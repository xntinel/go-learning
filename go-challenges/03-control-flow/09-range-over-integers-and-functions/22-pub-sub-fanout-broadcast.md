# Exercise 22: Pub/Sub Fan-Out Broadcast — Multi-Subscriber Dispatch Using `for i := range subscribers`

**Nivel: Intermedio** — validacion rapida (un test corto).

A publisher delivering one event to many subscribers cannot let one
misbehaving subscriber block or crash delivery to the rest -- a slow webhook
endpoint or a subscriber that has unsubscribed mid-flight must fail in
isolation while every other subscriber still gets the event. Iterating the
subscriber list once per event with a plain `for i := range subscribers` and
yielding a per-pair delivery result turns that isolation into the natural
shape of the loop rather than something wrapped in error-recovery logic.
This exercise is an independent module with its own `go mod init`.

## What you'll build

```text
pubsub/                    independent module: example.com/pub-sub-fanout-broadcast
  go.mod                   module example.com/pub-sub-fanout-broadcast
  pubsub.go                Delivery, Broadcast
  cmd/
    demo/
      main.go              runnable demo: 2 events, 3 subscribers, one partial failure
  pubsub_test.go           per-pair delivery, failure isolation, early-stop
```

Implement: `Broadcast(events, subscribers []string, send func(event, sub string) bool) iter.Seq[Delivery]` yielding one `Delivery{Event, Subscriber, Delivered}` per `(event, subscriber)` pair.
Test: 2 events fanned out to 3 subscribers yield 6 deliveries; a subscriber whose `send` always fails still receives every event with `Delivered=false`, and the other subscribers are unaffected; a consumer break after two deliveries stops there.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

The outer loop over `events` and the inner `for i := range subscribers`
counted loop together produce exactly `len(events) * len(subscribers)`
`Delivery` values, one per pair, in a fixed and predictable order: every
subscriber sees every event, in event order, and within an event, in
subscriber order. The `send` call's boolean result is captured into
`Delivery.Delivered` and yielded regardless of whether it was `true` or
`false` -- there is no `if !ok { continue without yielding }` branch, because
the whole point of this combinator is to report per-subscriber outcomes to
the caller, not to silently swallow the failures. That is what "handling...
partial subscriber failure without blocking the publisher" means in
practice: the loop never stops or retries because one `send` failed, it just
records the fact and moves to the next pair.

Create `pubsub.go`:

```go
package pubsub

import "iter"

// Delivery is the outcome of trying to deliver one event to one subscriber.
type Delivery struct {
	Event      string
	Subscriber string
	Delivered  bool
}

// Broadcast yields one Delivery per (event, subscriber) pair: for every
// event in events it iterates subscribers with a plain `for i := range
// subscribers` counted loop and calls send, recording whether that single
// delivery succeeded. A subscriber whose send fails does not stop delivery
// to the rest of the list -- the failure is reported per-pair instead of
// aborting the whole fan-out -- which is what keeps one slow or broken
// subscriber from blocking every other subscriber, and keeps the publisher
// itself decoupled from any single subscriber's health.
func Broadcast(events []string, subscribers []string, send func(event, subscriber string) bool) iter.Seq[Delivery] {
	return func(yield func(Delivery) bool) {
		for _, event := range events {
			for i := range subscribers {
				sub := subscribers[i]
				ok := send(event, sub)
				if !yield(Delivery{Event: event, Subscriber: sub, Delivered: ok}) {
					return
				}
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

	"example.com/pub-sub-fanout-broadcast"
)

func main() {
	events := []string{"order.created", "order.shipped"}
	subs := []string{"billing", "inventory", "notifications"}

	send := func(event, sub string) bool {
		return sub != "inventory" || event != "order.shipped"
	}

	for d := range pubsub.Broadcast(events, subs, send) {
		fmt.Printf("event=%-14s subscriber=%-13s delivered=%v\n", d.Event, d.Subscriber, d.Delivered)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
event=order.created  subscriber=billing       delivered=true
event=order.created  subscriber=inventory     delivered=true
event=order.created  subscriber=notifications delivered=true
event=order.shipped  subscriber=billing       delivered=true
event=order.shipped  subscriber=inventory     delivered=false
event=order.shipped  subscriber=notifications delivered=true
```

`inventory` fails only on `order.shipped`, and that single failure has no
effect on `billing` or `notifications` receiving that same event, or on
`inventory` itself receiving the earlier `order.created` event.

### Tests

Create `pubsub_test.go`:

```go
package pubsub

import "testing"

func TestBroadcastDeliversToEverySubscriberPerEvent(t *testing.T) {
	t.Parallel()

	events := []string{"e1", "e2"}
	subs := []string{"s1", "s2", "s3"}
	failing := map[string]bool{"s2": true}

	send := func(event, sub string) bool { return !failing[sub] }

	var got []Delivery
	for d := range Broadcast(events, subs, send) {
		got = append(got, d)
	}

	if len(got) != len(events)*len(subs) {
		t.Fatalf("got %d deliveries, want %d", len(got), len(events)*len(subs))
	}

	failures := 0
	for _, d := range got {
		if !d.Delivered {
			failures++
			if d.Subscriber != "s2" {
				t.Fatalf("unexpected failing subscriber: %+v", d)
			}
		}
	}
	if failures != len(events) {
		t.Fatalf("got %d failures, want %d (one per event for s2)", failures, len(events))
	}
}

func TestBroadcastContinuesPastSubscriberFailure(t *testing.T) {
	t.Parallel()

	send := func(event, sub string) bool { return sub != "bad" }
	var got []string
	for d := range Broadcast([]string{"e1"}, []string{"good1", "bad", "good2"}, send) {
		got = append(got, d.Subscriber)
	}
	want := []string{"good1", "bad", "good2"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestBroadcastStopsEarly(t *testing.T) {
	t.Parallel()

	send := func(string, string) bool { return true }
	count := 0
	for range Broadcast([]string{"e1", "e2"}, []string{"s1", "s2", "s3"}, send) {
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}
```

## Review

The correctness property that matters is that `send` returning `false` is
just data, not a control-flow signal: the loop keeps iterating subscribers
and events exactly as if it had succeeded, and the only thing that changes
is the `Delivered` field of the yielded value. The common mistake is writing
this as `if !send(...) { continue }` without yielding anything, which
silently drops the failure from the caller's view entirely -- the caller
would have no way to know delivery to that subscriber was ever attempted.
Keeping the failure visible in the yielded stream is what lets a consumer
build an accurate delivery report or trigger a retry queue keyed on exactly
the `(event, subscriber)` pairs that failed.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Publish-subscribe pattern — Wikipedia](https://en.wikipedia.org/wiki/Publish%E2%80%93subscribe_pattern)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-metric-percentile-aggregator.md](21-metric-percentile-aggregator.md) | Next: [23-merge-join-multiple-sorted-streams.md](23-merge-join-multiple-sorted-streams.md)
