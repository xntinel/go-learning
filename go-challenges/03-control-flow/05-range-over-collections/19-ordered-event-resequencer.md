# Exercise 19: Resequence Out-of-Order Events by Timestamp with Deduplication

**Nivel: Intermedio** — validacion rapida (un test corto).

In event sourcing and stream reconstruction, events from a distributed
source rarely arrive in the order they were produced: network partitions,
retries, and multiple partitions writing concurrently all scramble delivery
order, and a redelivered event must not be applied twice. This module ranges
a batch of events into a map keyed by `(Source, EventID)` to drop duplicates,
then sorts the survivors by timestamp to reconstruct the canonical order the
events actually happened in. The module is fully self-contained: its own
`go mod init`, no external dependencies.

## What you'll build

```text
resequencer/                independent module: example.com/ordered-event-resequencer
  go.mod                    go 1.24
  resequencer.go            type Event; Resequence(events []Event) []Event
  cmd/
    demo/
      main.go               runnable demo: out-of-order events plus a duplicate
  resequencer_test.go       table test: reorder + dedup + empty input
```

- Files: `resequencer.go`, `cmd/demo/main.go`, `resequencer_test.go`.
- Implement: `Resequence(events []Event) []Event` deduplicating via a
  `map[key]Event` keyed by `(Source, EventID)`, then sorting by `Timestamp`.
- Test: one table with events delivered out of order and a redelivered
  duplicate, plus an empty-input case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Dedup by identity, order by timestamp — two different fields, two different jobs

It is tempting to conflate "is this the same event" with "which one happened
first," but they are unrelated questions answered by unrelated fields.
Identity is `(Source, EventID)`: two deliveries with the same pair are the
same logical event, no matter what their payload or arrival order says —
`Resequence` keeps only the first one it ranges into the map, on the theory
that a retried event's payload should be identical to the original (if it is
not, that is a producer bug this function intentionally does not paper over
by picking the "latest" one). Order, in contrast, comes entirely from
`Timestamp`, which is independent of arrival order — that is the whole
premise of the exercise: a `range` over the raw slice sees events in
whatever order the network delivered them, and only an explicit sort after
dedup recovers the order they actually occurred in.

The `order []key` slice recorded alongside the `seen` map exists to make the
first pass single-purpose: it walks the input exactly once, deciding
membership as it goes, without a second pass over the input to rebuild a
deduplicated list. Flattening from `order` (not from ranging `seen`) also
sidesteps the classic mistake of ranging a map to build a slice you are
about to sort by something other than the sort key — here it does not matter
because the immediately following `sort.Slice` fixes the final order
regardless, but keeping the pre-sort order deterministic is one less thing
to reason about while reading the dedup logic.

Create `resequencer.go`:

```go
package resequencer

import (
	"sort"
	"time"
)

// Event is one record from a distributed source: it may arrive out of order
// relative to other events, and it may be redelivered.
type Event struct {
	Source    string
	EventID   string
	Timestamp time.Time
	Payload   string
}

type key struct {
	source string
	id     string
}

// Resequence deduplicates events by (Source, EventID) — keeping the first
// occurrence of each — and returns the survivors sorted by Timestamp
// ascending, reconstructing a canonical event order out of a stream that can
// arrive out of order and can redeliver the same event more than once.
func Resequence(events []Event) []Event {
	seen := make(map[key]Event, len(events))
	order := make([]key, 0, len(events))

	for _, e := range events {
		k := key{e.Source, e.EventID}
		if _, ok := seen[k]; ok {
			continue // duplicate delivery: keep the first occurrence
		}
		seen[k] = e
		order = append(order, k)
	}

	out := make([]Event, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.Before(out[j].Timestamp)
		}
		// deterministic tiebreak when two distinct events share a timestamp
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].EventID < out[j].EventID
	})

	return out
}
```

### The runnable demo

The demo delivers three logical events out of arrival order, with the first
one redelivered a moment later carrying a different (and discarded) payload.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ordered-event-resequencer"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	events := []resequencer.Event{
		{Source: "shard-2", EventID: "e3", Timestamp: base.Add(3 * time.Second), Payload: "c"},
		{Source: "shard-1", EventID: "e1", Timestamp: base.Add(1 * time.Second), Payload: "a"},
		{Source: "shard-1", EventID: "e1", Timestamp: base.Add(1 * time.Second), Payload: "a-retry"},
		{Source: "shard-2", EventID: "e2", Timestamp: base.Add(2 * time.Second), Payload: "b"},
	}

	for _, e := range resequencer.Resequence(events) {
		fmt.Printf("%s/%s %s\n", e.Source, e.EventID, e.Payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
shard-1/e1 a
shard-2/e2 b
shard-2/e3 c
```

The redelivered `shard-1/e1` with payload `"a-retry"` never appears — the
first occurrence, `"a"`, is the one that survives.

### Tests

The table delivers events out of arrival order with one redelivered event
interleaved, asserting both that duplicates collapse to the first occurrence
and that the final order follows `Timestamp`, not arrival order; plus an
empty-input case.

Create `resequencer_test.go`:

```go
package resequencer

import (
	"reflect"
	"testing"
	"time"
)

func TestResequence(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		events []Event
		want   []Event
	}{
		{
			name:   "empty input",
			events: []Event{},
			want:   []Event{},
		},
		{
			name: "out of order arrival reordered, duplicate keeps first occurrence",
			events: []Event{
				{Source: "shard-2", EventID: "e3", Timestamp: base.Add(3 * time.Second), Payload: "c"},
				{Source: "shard-1", EventID: "e1", Timestamp: base.Add(1 * time.Second), Payload: "a"},
				{Source: "shard-1", EventID: "e1", Timestamp: base.Add(1 * time.Second), Payload: "a-retry"},
				{Source: "shard-2", EventID: "e2", Timestamp: base.Add(2 * time.Second), Payload: "b"},
			},
			want: []Event{
				{Source: "shard-1", EventID: "e1", Timestamp: base.Add(1 * time.Second), Payload: "a"},
				{Source: "shard-2", EventID: "e2", Timestamp: base.Add(2 * time.Second), Payload: "b"},
				{Source: "shard-2", EventID: "e3", Timestamp: base.Add(3 * time.Second), Payload: "c"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resequence(tc.events)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Resequence() = %+v, want %+v", got, tc.want)
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

`Resequence` is correct when every distinct `(Source, EventID)` appears
exactly once in the output, holding its first-delivered payload, and the
output is ordered by `Timestamp` regardless of the input's arrival order.
The bug this design specifically avoids is using arrival order as a proxy
for either identity or causality: two different fields answer two different
questions here, and collapsing them — for instance, deduplicating by
position in the slice, or ordering by arrival instead of `Timestamp` — would
silently reintroduce the exact out-of-order and duplicate-delivery problems
this function exists to fix.

## Resources

- [Go Specification: For statements (range over slice)](https://go.dev/ref/spec#For_range)
- [package sort (Slice)](https://pkg.go.dev/sort#Slice)
- [Martin Kleppmann: Event sourcing and stream processing](https://martin.kleppmann.com/2015/03/04/turning-the-database-inside-out.html) — background on reconstructing order from an event stream.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-health-check-aggregator.md](18-health-check-aggregator.md) | Next: [20-topic-subscription-dispatcher.md](20-topic-subscription-dispatcher.md)
