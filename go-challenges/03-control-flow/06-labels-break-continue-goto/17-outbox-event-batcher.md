# Exercise 17: Batch outbox records by service destination, skip unpublishable events

**Nivel: Intermedio** — validacion rapida (un test corto).

An event-sourcing system uses the transactional outbox pattern to guarantee
at-least-once delivery: a transaction inserts a row into an outbox table
alongside its business data, and a background processor batches those rows
by destination service before publishing. A batch must be all-or-nothing —
partially publishing a batch (some events sent, one dropped mid-way) leaves
downstream consumers with a gap they cannot detect. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
outbox/                     independent module: example.com/outbox
  go.mod                     go 1.24
  outbox.go                  Event, Batch, PublishableBatches
  cmd/
    demo/
      main.go                runnable demo: two clean batches, one poisoned, one unregistered
  outbox_test.go              table test: empty, clean batch, poisoned batch, unregistered destination, recovery after a bad batch
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `PublishableBatches(batches []Batch, allowed map[string]bool) (publishable []Batch, skipped []string)`, dropping any batch containing an unserializable event or targeting a destination not in `allowed`.
- Test: no batches, a fully clean batch, a batch poisoned by one bad event, an unregistered destination, and a bad batch followed by a good one.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why one bad event needs to abandon the whole batch

`PublishableBatches` has two loops: the outer one walks batches, one per
destination, and the inner one walks the events inside the batch currently
being validated. The destination check happens once, directly in the outer
loop's body, so a bare `continue` handles it fine. But the per-event
serialization check happens inside the inner loop, and the moment one event
fails, the entire batch — not just that event — is unsafe to publish: a
consumer that already saw the first two events of a three-event batch has no
way to know a third was silently dropped. A bare `continue` in the inner
loop would only skip that one bad event and let the rest of the batch
through, which is exactly the partial-publish bug the outbox pattern exists
to prevent. `continue batches`, fired from inside the event loop, abandons
every event already accepted from this batch and moves straight to the next
destination.

Create `outbox.go`:

```go
package outbox

// Event is one outbox record awaiting publication.
type Event struct {
	ID      string
	Payload string // empty means the event failed to serialize
}

// Batch groups the outbox records headed to one destination service.
type Batch struct {
	Destination string
	Events      []Event
}

// PublishableBatches walks each batch and validates every event inside it:
// an event with an empty Payload failed to serialize, and a destination not
// present in allowed is not a service this process is permitted to publish
// to. If EITHER check fails for ANY event in a batch, the WHOLE batch is
// unsafe to publish atomically and must be dropped, not partially sent — a
// labeled continue on the BATCHES loop, fired from inside the per-event
// validation loop, abandons the rest of this batch's events and moves
// straight to the next destination's batch.
func PublishableBatches(batches []Batch, allowed map[string]bool) (publishable []Batch, skipped []string) {
batches:
	for _, b := range batches {
		if !allowed[b.Destination] {
			skipped = append(skipped, b.Destination)
			continue batches
		}
		for _, e := range b.Events {
			if e.Payload == "" {
				skipped = append(skipped, b.Destination)
				continue batches
			}
		}
		publishable = append(publishable, b)
	}
	return publishable, skipped
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/outbox"
)

func main() {
	batches := []outbox.Batch{
		{
			Destination: "billing",
			Events: []outbox.Event{
				{ID: "evt-1", Payload: `{"amount":100}`},
				{ID: "evt-2", Payload: `{"amount":200}`},
			},
		},
		{
			Destination: "billing",
			Events: []outbox.Event{
				{ID: "evt-3", Payload: `{"amount":300}`},
				{ID: "evt-4", Payload: ""}, // failed to serialize
			},
		},
		{
			Destination: "unregistered-service",
			Events: []outbox.Event{
				{ID: "evt-5", Payload: `{"amount":400}`},
			},
		},
	}

	allowed := map[string]bool{"billing": true, "shipping": true}

	publishable, skipped := outbox.PublishableBatches(batches, allowed)
	fmt.Println("publishable batches:")
	for _, b := range publishable {
		fmt.Printf("  %s: %d events\n", b.Destination, len(b.Events))
	}
	fmt.Println("skipped destinations:", skipped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
publishable batches:
  billing: 2 events
skipped destinations: [billing unregistered-service]
```

The first `billing` batch (two clean events) makes it through. The second
`billing` batch has one event with an empty payload, so it is dropped
entirely — its two good-looking events never reach `publishable` — and the
unregistered destination is skipped without its single event ever being
inspected.

### Tests

`TestPublishableBatches` covers no input, a batch that is entirely clean, a
batch poisoned by a single bad event, a destination outside the allow-list,
and a bad batch immediately followed by a good one, proving the poisoning
does not leak into later batches.

Create `outbox_test.go`:

```go
package outbox

import "testing"

func TestPublishableBatches(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"billing": true, "shipping": true}

	tests := map[string]struct {
		batches         []Batch
		wantPublishable []string // destinations expected to survive, in order
		wantSkipped     []string
	}{
		"no batches": {
			batches:         nil,
			wantPublishable: nil,
			wantSkipped:     nil,
		},
		"a clean batch is fully publishable": {
			batches: []Batch{
				{Destination: "billing", Events: []Event{{ID: "e1", Payload: "ok"}}},
			},
			wantPublishable: []string{"billing"},
			wantSkipped:     nil,
		},
		"one unserializable event drops the whole batch": {
			batches: []Batch{
				{Destination: "billing", Events: []Event{
					{ID: "e1", Payload: "ok"},
					{ID: "e2", Payload: ""},
				}},
			},
			wantPublishable: nil,
			wantSkipped:     []string{"billing"},
		},
		"an unregistered destination is skipped without inspecting events": {
			batches: []Batch{
				{Destination: "ghost", Events: []Event{{ID: "e1", Payload: "ok"}}},
			},
			wantPublishable: nil,
			wantSkipped:     []string{"ghost"},
		},
		"a bad batch does not block a good batch after it": {
			batches: []Batch{
				{Destination: "billing", Events: []Event{{ID: "e1", Payload: ""}}},
				{Destination: "shipping", Events: []Event{{ID: "e2", Payload: "ok"}}},
			},
			wantPublishable: []string{"shipping"},
			wantSkipped:     []string{"billing"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			publishable, skipped := PublishableBatches(tc.batches, allowed)
			if len(publishable) != len(tc.wantPublishable) {
				t.Fatalf("publishable = %v, want destinations %v", publishable, tc.wantPublishable)
			}
			for i, b := range publishable {
				if b.Destination != tc.wantPublishable[i] {
					t.Fatalf("publishable[%d] = %q, want %q", i, b.Destination, tc.wantPublishable[i])
				}
			}
			if len(skipped) != len(tc.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", skipped, tc.wantSkipped)
			}
			for i, s := range skipped {
				if s != tc.wantSkipped[i] {
					t.Fatalf("skipped[%d] = %q, want %q", i, s, tc.wantSkipped[i])
				}
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The batcher is correct when a single bad event removes the *entire* batch
from `publishable`, never a partial slice of it — the "one unserializable
event drops the whole batch" test is the one to study, since the batch's
first event looks perfectly fine on its own. The bug this exercise guards
against is a bare `continue` inside the event-validation loop: it would skip
only the bad event and let the batch's remaining events through, silently
producing a partial publish that a consumer has no way to detect as
incomplete. The "recovery" test confirms the reverse is also true: a
poisoned batch does not contaminate the destinations that come after it.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [Transactional outbox pattern (microservices.io)](https://microservices.io/patterns/data/transactional-outbox.html) — the delivery guarantee this batcher protects.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-replica-consistency-quorum-check.md](16-replica-consistency-quorum-check.md) | Next: [18-token-bucket-rate-limiter.md](18-token-bucket-rate-limiter.md)
