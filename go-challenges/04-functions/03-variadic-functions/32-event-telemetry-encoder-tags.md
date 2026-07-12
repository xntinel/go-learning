# Exercise 32: Event Telemetry Attribute Encoder with Tags

**Nivel: Intermedio** — validacion rapida (un test corto).

An observability pipeline attaches key/value tags to every event —
`method=GET`, `route=/orders/:id`, `status=200` — and code that builds
that event often computes a tag's final value in stages (start the
request with an assumed status, overwrite it once the handler actually
returns). `NewEvent(name, opts...)` builds the event through a variadic
list of `Option` functions, and applying `WithTag` twice for the same key
must update the value in place, not silently produce two conflicting tags
with the same key.

## What you'll build

```text
telemetry/                 independent module: example.com/telemetry
  go.mod                   go 1.24
  telemetry.go             package telemetry; type Tag struct{Key, Value string}; type Event struct{Name string; Tags []Tag}; type Option func(*Event); WithTag; NewEvent(name string, opts ...Option) Event
  cmd/
    demo/
      main.go              runnable demo: method/route/status tags, with status overwritten once
  telemetry_test.go         table tests: ordered application, overwrite-in-place, zero options
```

- Files: `telemetry.go`, `cmd/demo/main.go`, `telemetry_test.go`.
- Implement: `type Tag struct{ Key, Value string }`, `type Event struct{ Name string; Tags []Tag }`, `type Option func(*Event)`, `WithTag(key, value string) Option`, and `NewEvent(name string, opts ...Option) Event`.
- Test: options apply in the order given; applying `WithTag` twice for the same key overwrites the value at its original position rather than appending a duplicate; zero options produces a named event with no tags.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/32-event-telemetry-encoder-tags/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/32-event-telemetry-encoder-tags
go mod edit -go=1.24
```

### Functional options over a `...Tag` pair list, and why overwrite must preserve position

Earlier exercises in this chapter built key-value attributes as flat
`...string` pairs or a `...Tag` slice the caller assembles up front. This
one instead uses the functional-options pattern: `Option func(*Event)`
closures that each know how to mutate the `Event` being built, applied in
sequence by `NewEvent`. The win is that an `Option` can express more than
"set this key to this value" — it closes over arbitrary logic, so a
`WithTagIf(cond bool, key, value string) Option` or a
`WithComputedDuration(start time.Time) Option` slot into the exact same
variadic list without `NewEvent` itself changing at all. This is the same
shape `grpc.DialOption`, `http.Server` option helpers, and many other
Go APIs use for optional, extensible configuration.

The one subtlety that must not be glossed over is what happens when two
`Option`s target the same tag key. `WithTag("status", "500")` followed
later by `WithTag("status", "200")` must not produce two tags both named
`status` — that would make `Tags` ambiguous to any consumer that expects
one value per key. But it also should not move `status` to the end of
the list just because it was the one most recently updated: a telemetry
consumer that renders tags in a fixed column order (or a human scanning
the output) benefits from `status` staying wherever it was first placed.
`WithTag` therefore scans for an existing entry with the same key and
overwrites its `Value` field in place; only a genuinely new key gets
appended.

Create `telemetry.go`:

```go
// telemetry.go
package telemetry

// Tag is one key/value telemetry attribute attached to an Event.
type Tag struct {
	Key   string
	Value string
}

// Event is a telemetry record: a name plus an ordered list of tags.
type Event struct {
	Name string
	Tags []Tag
}

// Option configures an Event as it is built by NewEvent.
type Option func(*Event)

// WithTag attaches a key/value tag to the event. If key was already set
// by an earlier option, WithTag overwrites its value in place rather than
// appending a duplicate — the tag keeps its original position in Tags,
// and the most recently applied WithTag for a given key wins.
func WithTag(key, value string) Option {
	return func(e *Event) {
		for i := range e.Tags {
			if e.Tags[i].Key == key {
				e.Tags[i].Value = value
				return
			}
		}
		e.Tags = append(e.Tags, Tag{Key: key, Value: value})
	}
}

// NewEvent builds an Event named name, applying opts in order.
func NewEvent(name string, opts ...Option) Event {
	e := Event{Name: name}
	for _, opt := range opts {
		opt(&e)
	}
	return e
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/telemetry"
)

func main() {
	ev := telemetry.NewEvent("http.request",
		telemetry.WithTag("method", "GET"),
		telemetry.WithTag("route", "/orders/:id"),
		telemetry.WithTag("status", "500"),
		telemetry.WithTag("status", "200"), // overwrites, keeps position
	)

	fmt.Println("event:", ev.Name)
	for _, tag := range ev.Tags {
		fmt.Printf("  %s=%s\n", tag.Key, tag.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event: http.request
  method=GET
  route=/orders/:id
  status=200
```

### Tests

`TestWithTagOverwritesInPlace` is the one that pins the dedup-with-
position-preserved behavior: `status` is set to `"500"` before `route` is
even added, then overwritten to `"200"` last, and the test asserts it
still sits at index 1 (its original position) with the final value, and
that `Tags` has exactly three entries, not four.

Create `telemetry_test.go`:

```go
// telemetry_test.go
package telemetry

import "testing"

func TestNewEventAppliesTagsInOrder(t *testing.T) {
	t.Parallel()

	ev := NewEvent("http.request",
		WithTag("method", "GET"),
		WithTag("route", "/orders/:id"),
	)
	if ev.Name != "http.request" {
		t.Fatalf("Name = %q, want %q", ev.Name, "http.request")
	}
	want := []Tag{{Key: "method", Value: "GET"}, {Key: "route", Value: "/orders/:id"}}
	if len(ev.Tags) != len(want) {
		t.Fatalf("Tags = %v, want %v", ev.Tags, want)
	}
	for i, tag := range ev.Tags {
		if tag != want[i] {
			t.Errorf("Tags[%d] = %v, want %v", i, tag, want[i])
		}
	}
}

func TestWithTagOverwritesInPlace(t *testing.T) {
	t.Parallel()

	ev := NewEvent("http.request",
		WithTag("method", "GET"),
		WithTag("status", "500"),
		WithTag("route", "/orders/:id"),
		WithTag("status", "200"),
	)

	// status must have kept its original position (index 1), not been
	// moved to the end, and its value must be the last one applied.
	if len(ev.Tags) != 3 {
		t.Fatalf("Tags = %v, want 3 entries (status deduplicated)", ev.Tags)
	}
	if ev.Tags[1].Key != "status" || ev.Tags[1].Value != "200" {
		t.Fatalf("Tags[1] = %v, want status=200 at position 1", ev.Tags[1])
	}
}

func TestNewEventWithNoTags(t *testing.T) {
	t.Parallel()

	ev := NewEvent("heartbeat")
	if ev.Name != "heartbeat" {
		t.Fatalf("Name = %q, want heartbeat", ev.Name)
	}
	if len(ev.Tags) != 0 {
		t.Fatalf("Tags = %v, want empty", ev.Tags)
	}
}
```

## Review

`NewEvent` is correct when options apply in the exact order given, a
repeated tag key overwrites its value without duplicating the key or
moving its position, and zero options still produce a valid, empty-tagged
event. The senior point is the functional-options shape itself: an
`Option func(*Event)` composes arbitrary configuration logic — not just
key/value assignment — behind one variadic parameter, which is why it
scales to real APIs (`grpc.DialOption`, `http.Server` builders) far better
than a fixed struct of every possible field ever would. The detail most
likely to be glossed over, and the one this exercise's test pins
explicitly, is that "overwrite" must mean in place, not append-then-
dedupe-later — a consumer that reads `Tags[i]` positionally (for a fixed-
column log format, say) would otherwise see tags silently reorder
depending on which options happened to run last.

## Resources

- [Functional options for friendly APIs (Dave Cheney)](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [`google.golang.org/grpc`: `DialOption`](https://pkg.go.dev/google.golang.org/grpc#DialOption)
- [OpenTelemetry: attributes](https://opentelemetry.io/docs/specs/otel/common/#attribute)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-pubsub-broadcast-subscriber-fanout.md](31-pubsub-broadcast-subscriber-fanout.md) | Next: [33-header-dedup-merge-preserve-order.md](33-header-dedup-merge-preserve-order.md)
