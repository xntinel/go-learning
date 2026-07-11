# Exercise 20: Fan-Out Messages to Subscribers Matching Topic Patterns

**Nivel: Intermedio** — validacion rapida (un test corto).

A pub/sub broker's `Publish` is a fan-out, the mirror image of the fan-in
this lesson covers elsewhere: one message goes out to every subscriber whose
topic pattern matches, and a slow or broken subscriber must never stop
delivery to the rest. This module ranges the subscriber map once per
publish, filters by a glob pattern against the published topic, and collects
per-subscriber send failures instead of aborting on the first one. The
module is fully self-contained: its own `go mod init`, no external
dependencies.

## What you'll build

```text
broker/                     independent module: example.com/topic-subscription-dispatcher
  go.mod                    go 1.24
  broker.go                 type Broker, Subscriber, Failure; Publish(topic, message) []Failure
  cmd/
    demo/
      main.go               runnable demo: three subscribers, one pattern miss, one send failure
  broker_test.go            table test: pattern filtering + failure collection + no subscribers
```

- Files: `broker.go`, `cmd/demo/main.go`, `broker_test.go`.
- Implement: `Broker.Publish(topic, message string) []Failure` ranging
  `map[string]Subscriber`, filtering with `path.Match`, and collecting send
  errors.
- Test: one table with a mix of matching/non-matching patterns and a failing
  send, plus a no-subscribers case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/topic-subscription-dispatcher/cmd/demo
cd ~/go-exercises/topic-subscription-dispatcher
go mod init example.com/topic-subscription-dispatcher
go mod edit -go=1.24
```

### Filtering during iteration, and why one subscriber can't take down the fan-out

`Publish`'s single range over `b.subs` does three things per subscriber in
sequence: decide if the pattern matches (filter), and if so, attempt
delivery (side effect), and if that fails, record it (bookkeeping) — without
ever stopping the loop. That last property is the one that matters in
production: subscribers are independent consumers, often over a network, and
one of them being down must not delay or block delivery to the others. The
alternative — returning on the first error — would turn a single flaky
subscriber into an outage for every other subscriber on the same topic.

The filter itself uses `path.Match`, a small stdlib glob matcher (`*` and
`?` wildcards, `[...]` character classes) that treats `/` as a path
separator but nothing else specially — so a pattern like `"orders.*"`
matches `"orders.created"` the same way it would match a file glob, without
pulling in a heavier `regexp` dependency for what is usually a simple
prefix-style match. `path.Match` can itself return an error for a malformed
pattern (an unterminated `[` class, for instance); that error is routed into
the same `Failure` slice as a send failure, because from the publisher's
point of view both mean "this subscriber did not receive the message," and
the caller needs to see both kinds equally.

Create `broker.go`:

```go
package broker

import (
	"fmt"
	"path"
	"sort"
)

// Subscriber receives messages published to any topic matching Pattern, a
// shell-style glob (e.g. "orders.*").
type Subscriber struct {
	ID      string
	Pattern string
	Send    func(topic, message string) error
}

// Broker fans a published message out to every matching subscriber.
type Broker struct {
	subs map[string]Subscriber // keyed by subscriber ID
}

// New builds an empty Broker.
func New() *Broker {
	return &Broker{subs: make(map[string]Subscriber)}
}

// Subscribe registers or replaces the subscriber with this ID.
func (b *Broker) Subscribe(s Subscriber) {
	b.subs[s.ID] = s
}

// Failure records one subscriber's failed delivery.
type Failure struct {
	SubscriberID string
	Err          error
}

// Publish fans message out to every subscriber whose Pattern matches topic,
// ranging the subscriber map once. A subscriber whose Send returns an error,
// or whose Pattern is malformed, is recorded as a Failure instead of
// aborting the fan-out — one bad subscriber must never block delivery to
// the rest. The returned failures are sorted by SubscriberID so the result
// is deterministic regardless of map iteration order.
func (b *Broker) Publish(topic, message string) []Failure {
	var failures []Failure

	for id, sub := range b.subs {
		matched, err := path.Match(sub.Pattern, topic)
		if err != nil {
			failures = append(failures, Failure{
				SubscriberID: id,
				Err:          fmt.Errorf("bad pattern %q: %w", sub.Pattern, err),
			})
			continue
		}
		if !matched {
			continue
		}
		if err := sub.Send(topic, message); err != nil {
			failures = append(failures, Failure{SubscriberID: id, Err: err})
		}
	}

	sort.Slice(failures, func(i, j int) bool {
		return failures[i].SubscriberID < failures[j].SubscriberID
	})
	return failures
}
```

### The runnable demo

The demo registers three subscribers — one matching a broad pattern that
succeeds, one matching an exact pattern that fails to send, and one that
does not match the published topic at all.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/topic-subscription-dispatcher"
)

func main() {
	b := broker.New()

	b.Subscribe(broker.Subscriber{
		ID:      "audit-log",
		Pattern: "orders.*",
		Send:    func(topic, message string) error { return nil },
	})
	b.Subscribe(broker.Subscriber{
		ID:      "billing",
		Pattern: "orders.created",
		Send:    func(topic, message string) error { return errors.New("billing service unreachable") },
	})
	b.Subscribe(broker.Subscriber{
		ID:      "search-index",
		Pattern: "inventory.*",
		Send:    func(topic, message string) error { return nil },
	})

	failures := b.Publish("orders.created", "order #42 created")
	fmt.Printf("failures: %d\n", len(failures))
	for _, f := range failures {
		fmt.Printf("%s: %v\n", f.SubscriberID, f.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
failures: 1
billing: billing service unreachable
```

`search-index` never receives the message (its pattern does not match), and
`audit-log` receives it successfully — only `billing`'s failed send is
reported.

### Tests

The table registers subscribers with a mix of matching and non-matching
patterns, one of which fails its send, plus a no-subscribers case.

Create `broker_test.go`:

```go
package broker

import (
	"errors"
	"testing"
)

func TestPublish(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func(b *Broker)
		topic   string
		wantIDs []string
	}{
		{
			name:    "no subscribers",
			build:   func(b *Broker) {},
			topic:   "orders.created",
			wantIDs: nil,
		},
		{
			name: "pattern filters recipients, only failing sends reported",
			build: func(b *Broker) {
				b.Subscribe(Subscriber{
					ID:      "audit-log",
					Pattern: "orders.*",
					Send:    func(topic, message string) error { return nil },
				})
				b.Subscribe(Subscriber{
					ID:      "billing",
					Pattern: "orders.created",
					Send:    func(topic, message string) error { return errors.New("unreachable") },
				})
				b.Subscribe(Subscriber{
					ID:      "search-index",
					Pattern: "inventory.*",
					Send:    func(topic, message string) error { return nil },
				})
			},
			topic:   "orders.created",
			wantIDs: []string{"billing"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := New()
			tc.build(b)

			failures := b.Publish(tc.topic, "payload")
			if len(failures) != len(tc.wantIDs) {
				t.Fatalf("Publish() failures = %d, want %d", len(failures), len(tc.wantIDs))
			}
			for i, id := range tc.wantIDs {
				if failures[i].SubscriberID != id {
					t.Errorf("failures[%d].SubscriberID = %q, want %q", i, failures[i].SubscriberID, id)
				}
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

`Publish` is correct when every subscriber whose pattern matches is sent the
message, every subscriber whose pattern does not match is skipped entirely,
and every failure — whether a bad pattern or a failed send — is reported
without stopping delivery to the remaining subscribers. The bug this design
avoids is treating fan-out like fan-in: a fan-in collector can legitimately
stop early on a fatal error, but a fan-out publisher's job is to reach as
many subscribers as it can and tell the caller which ones it could not,
never to give up on the rest because one subscriber misbehaved.

## Resources

- [package path (Match)](https://pkg.go.dev/path#Match)
- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_range)
- [Google Cloud Pub/Sub: Subscriber overview](https://cloud.google.com/pubsub/docs/subscriber) — production fan-out delivery semantics.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-ordered-event-resequencer.md](19-ordered-event-resequencer.md) | Next: [21-sql-column-projection-iterator.md](21-sql-column-projection-iterator.md)
