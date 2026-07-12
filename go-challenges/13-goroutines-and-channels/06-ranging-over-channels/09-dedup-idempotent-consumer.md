# Exercise 9: Idempotent Event Consumer: Dedup by Key While Ranging

Every at-least-once message broker can redeliver the same event, and a naive
consumer processes it twice — double-charging a card, double-sending an email. The
guard is idempotency: dedup by an idempotency key while ranging, so each unique
event is emitted exactly once. This exercise builds that consumer with a seen-key
set.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
dedupconsumer/              independent module: example.com/dedupconsumer
  go.mod                    go 1.26
  consumer.go               type Event; Consumer.Consume dedups by key (for-select)
  cmd/
    demo/
      main.go               a stream with redeliveries, deduped
  consumer_test.go          duplicates collapsed, all-unique, empty, heavy-dup counts
```

Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
Implement: `Event` with an idempotency `Key`, and a `Consumer` whose `Consume(ctx, ch)` ranges the event stream (via `for-select` so it also honors cancellation), skipping any event whose key was already seen and emitting each unique event once, in first-seen order.
Test: a stream with duplicate keys yields only the first occurrence per key in first-seen order; an all-unique stream passes through unchanged; an empty-closed stream yields none; a large heavily-duplicated stream yields the right unique count and a seen-set sized to the unique keys.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/06-ranging-over-channels/09-dedup-idempotent-consumer/cmd/demo
cd go-solutions/13-goroutines-and-channels/06-ranging-over-channels/09-dedup-idempotent-consumer
```

### The seen-set and first-seen ordering

The dedup guard is a `map[string]struct{}` — a set of keys already processed.
`struct{}` is the zero-width value type, so the map stores only keys and no
payload, which is the idiomatic Go set. On each event, a comma-ok lookup
(`_, dup := seen[e.Key]`) decides: if the key is present, this is a redelivery and
we skip it; otherwise we record the key and emit the event. Because we emit on the
*first* sight of a key and skip every later one, the output preserves first-seen
order, which is the property a downstream expects — the first delivery wins, the
retries are silently dropped.

Two deliberate choices. First, the loop is a `for-select` over `ctx.Done()` and
the channel, not a plain `range`: a real event consumer runs until shutdown, so it
must observe cancellation, which a plain `range` cannot. Second, the seen-set lives
on the `Consumer`, not inside `Consume`, so it persists across calls and can be
inspected — in production this set is bounded (an LRU, a TTL, or a Redis/DB unique
constraint) because it cannot grow forever, but the dedup logic is identical. The
exercise keeps an unbounded in-memory set to focus on the ranging structure; the
`SeenCount` accessor exists so a test can assert the set is sized to the unique
keys, not the total deliveries.

This is the smallest correct idempotency guard. It does not survive a process
restart (the set is in memory) and it is single-consumer (no cross-instance
coordination). Those are real limits a senior engineer names: durable idempotency
needs a shared store keyed by the same idempotency key. The in-loop dedup shown
here is the pattern; the storage backing the set is an orthogonal decision.

Create `consumer.go`:

```go
package dedupconsumer

import "context"

// Event is a delivered message. Key is its idempotency key: two deliveries of the
// same logical event share a Key.
type Event struct {
	Key     string
	Payload string
}

// Consumer deduplicates events by key across the events it has seen.
type Consumer struct {
	seen map[string]struct{}
}

func New() *Consumer {
	return &Consumer{seen: make(map[string]struct{})}
}

// Consume ranges ch (via for-select so it honors ctx cancellation), emitting each
// event whose key it has not seen before, in first-seen order. Redeliveries of an
// already-seen key are skipped.
func (c *Consumer) Consume(ctx context.Context, ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case <-ctx.Done():
			return out
		case e, ok := <-ch:
			if !ok {
				return out
			}
			if _, dup := c.seen[e.Key]; dup {
				continue // redelivery: already processed this key
			}
			c.seen[e.Key] = struct{}{}
			out = append(out, e)
		}
	}
}

// SeenCount reports the number of distinct keys processed so far.
func (c *Consumer) SeenCount() int { return len(c.seen) }
```

### The runnable demo

The demo feeds a stream where two events are redelivered (keys `a` and `b` appear
twice), and prints the deduped output plus the seen count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/dedupconsumer"
)

func main() {
	ch := make(chan dedupconsumer.Event, 5)
	deliveries := []dedupconsumer.Event{
		{Key: "a", Payload: "charge"},
		{Key: "b", Payload: "email"},
		{Key: "a", Payload: "charge"}, // redelivery
		{Key: "c", Payload: "ship"},
		{Key: "b", Payload: "email"}, // redelivery
	}
	for _, e := range deliveries {
		ch <- e
	}
	close(ch)

	c := dedupconsumer.New()
	unique := c.Consume(context.Background(), ch)

	fmt.Printf("delivered %d, processed %d unique\n", len(deliveries), len(unique))
	for _, e := range unique {
		fmt.Printf("%s: %s\n", e.Key, e.Payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered 5, processed 3 unique
a: charge
b: email
c: ship
```

### Tests

`TestDuplicatesCollapsed` feeds redeliveries and asserts the output keys are the
first occurrences in order. `TestAllUniquePassThrough` asserts a no-duplicate
stream is unchanged. `TestEmptyClosed` yields nothing. `TestHeavyDuplication`
feeds a large stream where each of K keys appears many times and asserts the
unique count and that `SeenCount` equals K — the seen-set is sized to distinct
keys, not total deliveries.

Create `consumer_test.go`:

```go
package dedupconsumer

import (
	"context"
	"fmt"
	"testing"
)

func stream(events ...Event) <-chan Event {
	ch := make(chan Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

func keys(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Key
	}
	return out
}

func TestDuplicatesCollapsed(t *testing.T) {
	t.Parallel()
	ch := stream(
		Event{Key: "a"}, Event{Key: "b"}, Event{Key: "a"},
		Event{Key: "c"}, Event{Key: "b"},
	)
	got := keys(New().Consume(context.Background(), ch))
	want := []string{"a", "b", "c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("keys = %v, want %v (first-seen order)", got, want)
	}
}

func TestAllUniquePassThrough(t *testing.T) {
	t.Parallel()
	ch := stream(Event{Key: "x"}, Event{Key: "y"}, Event{Key: "z"})
	got := keys(New().Consume(context.Background(), ch))
	want := []string{"x", "y", "z"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestEmptyClosed(t *testing.T) {
	t.Parallel()
	ch := make(chan Event)
	close(ch)
	if got := New().Consume(context.Background(), ch); len(got) != 0 {
		t.Fatalf("Consume on empty stream = %v, want none", got)
	}
}

func TestHeavyDuplication(t *testing.T) {
	t.Parallel()
	const uniqueKeys, copies = 50, 20
	var events []Event
	for c := range copies {
		for k := range uniqueKeys {
			events = append(events, Event{Key: fmt.Sprintf("k%d", k), Payload: fmt.Sprintf("c%d", c)})
		}
	}

	consumer := New()
	got := consumer.Consume(context.Background(), stream(events...))
	if len(got) != uniqueKeys {
		t.Fatalf("unique = %d, want %d", len(got), uniqueKeys)
	}
	if consumer.SeenCount() != uniqueKeys {
		t.Fatalf("SeenCount = %d, want %d (set sized to distinct keys)", consumer.SeenCount(), uniqueKeys)
	}
}

func ExampleConsumer_Consume() {
	ch := stream(Event{Key: "a"}, Event{Key: "a"}, Event{Key: "b"})
	got := New().Consume(context.Background(), ch)
	fmt.Println(keys(got))
	// Output: [a b]
}
```

## Review

The consumer is correct when each idempotency key is emitted exactly once, in
first-seen order, no matter how many times it is redelivered.
`TestDuplicatesCollapsed` proves the first-seen semantics; `TestHeavyDuplication`
proves the seen-set is sized to distinct keys, which is what keeps a bounded
version's memory predictable. The `for-select` shape is chosen so the consumer can
be cancelled — a real event loop runs until shutdown, and a plain `range` could
not stop. The honest caveat, stated in the concepts, is that this in-memory set is
per-process and non-durable; durable at-least-once idempotency keys the set in a
shared store, but the ranging-and-dedup structure is exactly this.

## Resources

- [Go spec: The blank struct as a set value](https://go.dev/ref/spec#Struct_types) — `struct{}` is zero-width; `map[K]struct{}` is the idiomatic set.
- [pkg.go.dev: context](https://pkg.go.dev/context) — the cancellation the `for-select` observes.
- [Stripe API: Idempotent requests](https://docs.stripe.com/api/idempotent_requests) — an idempotency key so a retried request runs once.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-result-consumer-error-join.md](08-result-consumer-error-join.md) | Next: [10-bounded-window-consumer.md](10-bounded-window-consumer.md)
