# Exercise 20: Event Ordering Validator — Windowed Sequencing Check as `iter.Seq2[Event, error]`

**Nivel: Intermedio** — validacion rapida (un test corto).

An event stream consumer -- a Kafka topic reader, a webhook queue drainer --
often assumes strictly ascending event IDs, and when that assumption breaks
(a duplicate delivery, a dropped message leaving a gap, a reordered replay)
the consumer needs to know exactly which violation occurred and on which
event, without buffering the entire history of IDs ever seen. Wrapping the
event stream in an `iter.Seq2[Event, error]` combinator that only remembers
the single last-seen ID gives per-item violation detection at O(1) memory.
This exercise is an independent module with its own `go mod init`.

## What you'll build

```text
ordercheck/                independent module: example.com/event-ordering-detector
  go.mod                   module example.com/event-ordering-detector
  ordercheck.go            Event, Validate, ErrDuplicate, ErrGap, ErrReorder
  cmd/
    demo/
      main.go              runnable demo: a clean run, a duplicate, and a gap
  ordercheck_test.go       clean sequence, duplicate/gap/reorder, early-stop, empty source
```

Implement: `Validate(src iter.Seq[Event]) iter.Seq2[Event, error]` yielding every event paired with a non-nil error the moment its ID breaks strict ascending order.
Test: a clean ascending sequence yields no errors; `[1,1,4,2]` yields `nil, ErrDuplicate, ErrGap, ErrReorder` in order; a consumer that breaks on the first non-nil error stops there; an empty source yields nothing.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

`Validate` carries exactly one piece of state across yields: the ID last
seen. On every event it classifies the new ID against that single value --
equal means a duplicate, lower means a reorder, more than one higher means a
gap, exactly one higher (or the very first event) means clean -- and then
always updates `last` to the new ID regardless of which case fired, so the
next comparison is always against the most recent event, not the last
*valid* one. That is a deliberate choice: after a gap from 2 to 5, the next
expected ID is 6, not 3, because the detector is reporting what actually
happened in the stream, not trying to resync to an idealized sequence. Each
sentinel error (`ErrDuplicate`, `ErrGap`, `ErrReorder`) is wrapped with
`fmt.Errorf("%w: ...")` so a caller can `errors.Is` against the specific kind
of violation while still getting a human-readable message with the offending
IDs.

Create `ordercheck.go`:

```go
package ordercheck

import (
	"errors"
	"fmt"
	"iter"
)

// Sentinel errors identify which kind of ordering violation occurred; wrap
// them with fmt.Errorf("%w: ...") so callers can still match with errors.Is.
var (
	ErrDuplicate = errors.New("duplicate event id")
	ErrGap       = errors.New("gap in event id sequence")
	ErrReorder   = errors.New("event id out of order")
)

// Event is one item of a supposedly strictly-ascending-by-ID stream.
type Event struct {
	ID   int
	Data string
}

// Validate wraps src and yields every event paired with a non-nil error the
// moment its ID does not continue the strictly ascending sequence expected
// from the ID last seen: err is ErrDuplicate for a repeated ID, ErrReorder
// for an ID lower than one already observed, ErrGap for a skipped ID. Only
// the single last-seen ID is retained across yields -- not a history of
// every ID ever seen -- so memory use is O(1) regardless of how long the
// stream runs, at the cost of only catching reorders and duplicates
// relative to the immediately preceding event, not against the whole
// history. Iteration continues past a violation so the caller decides
// whether one bad event should stop the scan or just be logged; a consumer
// that wants to stop on the first error does so with its own `if err != nil
// { break }` in the range body, which this combinator honors like any other
// early termination.
func Validate(src iter.Seq[Event]) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		last := 0
		first := true
		for e := range src {
			var err error
			switch {
			case first:
				first = false
			case e.ID == last:
				err = fmt.Errorf("%w: id %d", ErrDuplicate, e.ID)
			case e.ID < last:
				err = fmt.Errorf("%w: id %d after %d", ErrReorder, e.ID, last)
			case e.ID > last+1:
				err = fmt.Errorf("%w: expected %d, got %d", ErrGap, last+1, e.ID)
			}
			last = e.ID
			if !yield(e, err) {
				return
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
	"slices"

	"example.com/event-ordering-detector"
)

func main() {
	events := []ordercheck.Event{
		{ID: 1, Data: "a"},
		{ID: 2, Data: "b"},
		{ID: 2, Data: "b-retry"},
		{ID: 5, Data: "e"},
	}

	for e, err := range ordercheck.Validate(slices.Values(events)) {
		if err != nil {
			fmt.Printf("event %d: %s -- violation: %v\n", e.ID, e.Data, err)
			continue
		}
		fmt.Printf("event %d: %s -- ok\n", e.ID, e.Data)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
event 1: a -- ok
event 2: b -- ok
event 2: b-retry -- violation: duplicate event id: id 2
event 5: e -- violation: gap in event id sequence: expected 3, got 5
```

The redelivered `id 2` is flagged as a duplicate against the ID immediately
before it, and the jump straight to `id 5` is flagged as a gap expecting
`3`, since `last` was updated to `2` by the duplicate event, not rolled back
to the previous valid ID.

### Tests

Create `ordercheck_test.go`:

```go
package ordercheck

import (
	"errors"
	"slices"
	"testing"
)

func TestValidateCleanSequenceHasNoErrors(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: 1}, {ID: 2}, {ID: 3}}
	for _, err := range Validate(slices.Values(events)) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestValidateDetectsDuplicateGapAndReorder(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: 1}, {ID: 1}, {ID: 4}, {ID: 2}}
	var errs []error
	for _, err := range Validate(slices.Values(events)) {
		errs = append(errs, err)
	}

	if len(errs) != 4 {
		t.Fatalf("got %d results, want 4", len(errs))
	}
	if errs[0] != nil {
		t.Fatalf("errs[0] = %v, want nil", errs[0])
	}
	if !errors.Is(errs[1], ErrDuplicate) {
		t.Fatalf("errs[1] = %v, want ErrDuplicate", errs[1])
	}
	if !errors.Is(errs[2], ErrGap) {
		t.Fatalf("errs[2] = %v, want ErrGap", errs[2])
	}
	if !errors.Is(errs[3], ErrReorder) {
		t.Fatalf("errs[3] = %v, want ErrReorder", errs[3])
	}
}

func TestValidateStopsEarlyOnFirstError(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: 1}, {ID: 5}, {ID: 6}, {ID: 7}}
	var seen []Event
	for e, err := range Validate(slices.Values(events)) {
		seen = append(seen, e)
		if err != nil {
			break
		}
	}
	if len(seen) != 2 {
		t.Fatalf("seen = %v, want 2 events (stop at first gap)", seen)
	}
}

func TestValidateEmptySourceYieldsNothing(t *testing.T) {
	t.Parallel()

	count := 0
	for range Validate(slices.Values([]Event{})) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}
```

## Review

The design decision that makes this detector actually useful in production
is bounding its memory to a single ID instead of a set of every ID ever
seen: a naive "have I seen this ID before" dedup check needs `O(n)` memory
over the life of a long-running stream, while comparing only against `last`
is `O(1)` forever, at the cost of only detecting duplicates and reorders
relative to the immediately adjacent event. That is the right tradeoff for
a consumer whose real concern is "is my stream still monotonic right now",
not "has this exact ID ever appeared in history" -- the latter is a job for
the windowed or TTL-bounded dedup combinator, not this one. Updating `last`
unconditionally, even on a violation, is the other detail that is easy to
get backwards: rolling `last` back to the last *valid* ID would make a
duplicate look like it re-validates the stream, hiding a genuine gap that
follows it.

## Resources

- [`iter.Seq2` documentation](https://pkg.go.dev/iter#Seq2)
- [`errors.Is` and wrapped errors](https://pkg.go.dev/errors#Is)
- [Apache Kafka: message ordering guarantees](https://kafka.apache.org/documentation/#semantics)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-health-check-scheduler.md](19-health-check-scheduler.md) | Next: [21-metric-percentile-aggregator.md](21-metric-percentile-aggregator.md)
