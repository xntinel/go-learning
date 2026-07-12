# Exercise 31: Pub/Sub Broadcast Subscriber Fanout Register

**Nivel: Intermedio** — validacion rapida (un test corto).

One published event — "order created" — often needs to reach several
independent listeners: a log sink, an analytics pipeline, an audit
trail. None of them should know the others exist, and a temporary outage
in the analytics pipeline should never stop the log sink or audit trail
from receiving the event. `Broadcast(event, subs...)` fans one event out
to a variadic list of subscriber callbacks and reports every delivery
failure without letting any one of them block the rest.

## What you'll build

```text
broadcast/                 independent module: example.com/broadcast
  go.mod                   go 1.24
  broadcast.go             package broadcast; type Event struct{Topic, Data string}; type Subscriber func(Event) error; Named; Broadcast(ev Event, subs ...Subscriber) error
  cmd/
    demo/
      main.go              runnable demo: three subscribers, one of which fails, all three still called
  broadcast_test.go         table tests: all succeed, one fails but all still run, named-subscriber error text, zero subscribers
```

- Files: `broadcast.go`, `cmd/demo/main.go`, `broadcast_test.go`.
- Implement: `type Event struct{ Topic, Data string }`, `type Subscriber func(Event) error`, `Named(name string, sub Subscriber) Subscriber`, and `Broadcast(ev Event, subs ...Subscriber) error`.
- Test: three succeeding subscribers all receive the event and `Broadcast` returns `nil`; a failing subscriber in the middle of the list does not stop the ones after it from being called; the returned error names which subscriber (by index and, if wrapped, by name) failed; zero subscribers is always valid.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/31-pubsub-broadcast-subscriber-fanout/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/31-pubsub-broadcast-subscriber-fanout
go mod edit -go=1.24
```

### Why fanout must not stop at the first failure, and how the error still says which subscriber broke

A publish-subscribe fanout is the clearest case where "stop at the first
error" is actively wrong: the whole point of decoupling subscribers is
that they fail independently, so one subscriber's outage must never
become an outage for every subscriber registered after it in the list.
`Broadcast` therefore always calls every subscriber — the loop has no
early return — and only after every one has been given the event does it
join whatever errors came back with `errors.Join`. A caller can then
decide per-topic whether a partial delivery failure is worth retrying,
alerting on, or ignoring; `Broadcast` itself never makes that call for
them by silently dropping the subscribers it never got to.

Because several subscribers can each fail for their own reason, the
aggregated error needs to say *which one* broke, not just that "something"
did. `Broadcast` wraps each failing subscriber's error with its index in
the list (`"subscriber 1: ..."`), and `Named` lets a caller additionally
give a subscriber a human-readable name that survives inside that
wrapping (`"subscriber 1: analytics: connection refused"`). This is the
same `fmt.Errorf("%w", ...)` wrapping pattern used throughout the standard
library — each layer adds one fact and defers to the next with `%w`,
so `errors.Unwrap` (or a human reading the message) can always trace an
error back through every layer that touched it.

Create `broadcast.go`:

```go
// broadcast.go
package broadcast

import (
	"errors"
	"fmt"
)

// Event is one message fanned out to every subscriber.
type Event struct {
	Topic string
	Data  string
}

// Subscriber receives an Event and reports an error if it could not
// process it (e.g. a downstream send failed). One subscriber's error
// never affects delivery to any other subscriber.
type Subscriber func(Event) error

// Broadcast delivers ev to every subscriber, in the order given, and
// aggregates every delivery error with errors.Join. A slow or failing
// subscriber never prevents the others from receiving the event: Broadcast
// always calls all of them before returning.
func Broadcast(ev Event, subs ...Subscriber) error {
	var errs []error
	for i, sub := range subs {
		if err := sub(ev); err != nil {
			errs = append(errs, fmt.Errorf("subscriber %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// Named wraps a Subscriber so its errors are reported with a caller-chosen
// name instead of a bare index, which is more useful once subscribers are
// registered dynamically rather than as a fixed literal list.
func Named(name string, sub Subscriber) Subscriber {
	return func(ev Event) error {
		if err := sub(ev); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"

	"example.com/broadcast"
)

func main() {
	var delivered []string

	logSub := func(ev broadcast.Event) error {
		delivered = append(delivered, "log:"+ev.Data)
		return nil
	}
	analyticsSub := broadcast.Named("analytics", func(ev broadcast.Event) error {
		return errors.New("connection refused")
	})
	auditSub := func(ev broadcast.Event) error {
		delivered = append(delivered, "audit:"+ev.Data)
		return nil
	}

	err := broadcast.Broadcast(
		broadcast.Event{Topic: "order.created", Data: "order-42"},
		logSub, analyticsSub, auditSub,
	)

	fmt.Println("delivered to:", delivered)
	fmt.Println("broadcast error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered to: [log:order-42 audit:order-42]
broadcast error: subscriber 1: analytics: connection refused
```

### Tests

`TestBroadcastRunsAllDespiteOneFailing` is the one that matters: a failing
subscriber sits between two succeeding ones, and the test asserts both
succeeding subscribers still ran — the middle failure must not have short-
circuited the loop.

Create `broadcast_test.go`:

```go
// broadcast_test.go
package broadcast

import (
	"errors"
	"testing"
)

func TestBroadcastDeliversToAllOnSuccess(t *testing.T) {
	t.Parallel()

	var got []string
	sub := func(name string) Subscriber {
		return func(ev Event) error {
			got = append(got, name)
			return nil
		}
	}

	err := Broadcast(Event{Topic: "t", Data: "d"}, sub("a"), sub("b"), sub("c"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("delivered to %d subscribers, want 3: %v", len(got), got)
	}
}

func TestBroadcastRunsAllDespiteOneFailing(t *testing.T) {
	t.Parallel()

	var got []string
	failing := func(ev Event) error { return errors.New("boom") }
	ok := func(name string) Subscriber {
		return func(ev Event) error {
			got = append(got, name)
			return nil
		}
	}

	err := Broadcast(Event{Topic: "t", Data: "d"}, ok("first"), failing, ok("third"))
	if err == nil {
		t.Fatal("expected an aggregated error")
	}
	if len(got) != 2 {
		t.Fatalf("expected both non-failing subscribers to run, got %v", got)
	}
}

func TestBroadcastErrorNamesTheFailingSubscriber(t *testing.T) {
	t.Parallel()

	err := Broadcast(Event{Topic: "t"},
		func(ev Event) error { return nil },
		Named("analytics", func(ev Event) error { return errors.New("connection refused") }),
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	want := "subscriber 1: analytics: connection refused"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestBroadcastNoSubscribersIsValid(t *testing.T) {
	t.Parallel()

	if err := Broadcast(Event{Topic: "t"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

## Review

`Broadcast` is correct when every subscriber in the list is called
exactly once regardless of any other subscriber's outcome, and the
returned error, if any, identifies every subscriber that failed rather
than just reporting a generic "broadcast failed." The senior point is
recognizing fanout as a distinct shape from the pipeline and aggregation
exercises seen so far: it is not "stop at first error" (there is no
dependency between subscribers) and it is not merely "collect every
error" either — it specifically must guarantee delivery attempts to all
subscribers happen before any error is even inspected, which is a
liveness guarantee, not just a correctness one. This module keeps
delivery synchronous and sequential for determinism; a production fanout
serving many slow subscribers would run each in its own goroutine and
join their errors, at which point the aggregation logic here is exactly
what collects the results once every goroutine returns.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [`fmt.Errorf` error wrapping with `%w`](https://pkg.go.dev/fmt#Errorf)
- [Go blog: Error handling and Go](https://go.dev/blog/error-handling-and-go)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-multipart-upload-validator-rules.md](30-multipart-upload-validator-rules.md) | Next: [32-event-telemetry-encoder-tags.md](32-event-telemetry-encoder-tags.md)
