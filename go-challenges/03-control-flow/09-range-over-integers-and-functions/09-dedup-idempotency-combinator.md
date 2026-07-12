# Exercise 9: Idempotency Guard — Distinct Combinator Dropping Duplicate Event IDs

An at-least-once event stream delivers duplicates; a side-effecting handler needs
effectively-once. This exercise builds `DistinctBy`, a combinator that drops
already-seen events by key, and a windowed variant that caps memory — because the
naive unbounded seen-map is a memory leak in a long-lived stream, and real dedup
is windowed or TTL-backed.

## What you'll build

```text
dedup/                    independent module: example.com/dedup
  go.mod                  module example.com/dedup
  dedup.go                Event, DistinctBy, DistinctWindow
  cmd/
    demo/
      main.go             runnable demo: dedup an at-least-once stream
  dedup_test.go           first-seen order, passthrough, early-break, window tests
```

Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
Implement: `DistinctBy[T, K](seq iter.Seq[T], key func(T) K) iter.Seq[T]` and a bounded `DistinctWindow` with FIFO eviction.
Test: duplicates collapse to first-seen order; unique/empty passthrough; early break stops upstream; a windowed key re-emits after eviction.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/09-dedup-idempotency-combinator/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/09-dedup-idempotency-combinator
```

## The design

`DistinctBy` carries a `map[K]struct{}` of seen keys in its closure. For each
value it computes `key(v)`; if the key is already present it skips (returns nothing
downstream, keeps pulling); otherwise it records the key and yields. Because the
seen-set is checked before yielding, the first occurrence of each key passes and
every later duplicate is dropped — so the output preserves *first-seen* order,
which is what an idempotency guard in front of a handler wants.

The honest caveat, stated in code by the second variant, is memory. `DistinctBy`'s
map grows with the number of *distinct* keys and never shrinks; on an infinite
event stream that is an unbounded leak. Production dedup bounds it. `DistinctWindow`
keeps a FIFO of the last `window` keys: when the window is full and a new distinct
key arrives, it evicts the oldest key (removing it from both the ring and the
seen-set) before admitting the new one. The consequence is deliberate and must be
understood: a key that scrolls out of the window is forgotten, so if it reappears
later it is treated as new and re-emitted. That is the classic dedup trade-off —
bounded memory in exchange for a bounded dedup horizon. A TTL-backed variant makes
the same trade on a time axis instead of a count axis.

Both combinators honor cooperative stop: the `if !yield(v) { return }` means a
consumer that breaks halts the upstream source, so dedup composes with `Take` and
the other combinators without draining the whole stream.

Create `dedup.go`:

```go
package dedup

import "iter"

// Event is one delivery in an at-least-once stream.
type Event struct {
	ID   string
	Body string
}

// DistinctBy drops values whose key was already seen, preserving first-seen
// order. The seen-set is unbounded: use it only for streams of bounded
// cardinality. For long-lived streams, use DistinctWindow.
func DistinctBy[T any, K comparable](seq iter.Seq[T], key func(T) K) iter.Seq[T] {
	return func(yield func(T) bool) {
		seen := make(map[K]struct{})
		for v := range seq {
			k := key(v)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			if !yield(v) {
				return
			}
		}
	}
}

// DistinctWindow is a bounded DistinctBy: it remembers only the last `window`
// distinct keys, evicting the oldest FIFO. A key evicted from the window can
// re-emit if it reappears later, trading dedup horizon for bounded memory.
func DistinctWindow[T any, K comparable](seq iter.Seq[T], key func(T) K, window int) iter.Seq[T] {
	return func(yield func(T) bool) {
		if window <= 0 {
			return
		}
		seen := make(map[K]struct{}, window)
		order := make([]K, 0, window)
		for v := range seq {
			k := key(v)
			if _, ok := seen[k]; ok {
				continue
			}
			if len(order) == window {
				oldest := order[0]
				order = order[1:]
				delete(seen, oldest)
			}
			seen[k] = struct{}{}
			order = append(order, k)
			if !yield(v) {
				return
			}
		}
	}
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/dedup"
)

func main() {
	stream := slices.Values([]dedup.Event{
		{ID: "e1", Body: "created"},
		{ID: "e2", Body: "updated"},
		{ID: "e1", Body: "created (redelivered)"},
		{ID: "e3", Body: "deleted"},
		{ID: "e2", Body: "updated (redelivered)"},
	})

	byID := func(e dedup.Event) string { return e.ID }
	for e := range dedup.DistinctBy(stream, byID) {
		fmt.Printf("handle %s: %s\n", e.ID, e.Body)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handle e1: created
handle e2: updated
handle e3: deleted
```

## Tests

Create `dedup_test.go`:

```go
package dedup

import (
	"reflect"
	"slices"
	"testing"
)

func ids(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func byID(e Event) string { return e.ID }

func TestDistinctFirstSeenOrder(t *testing.T) {
	t.Parallel()

	stream := slices.Values([]Event{
		{ID: "a"}, {ID: "b"}, {ID: "a"}, {ID: "c"}, {ID: "b"},
	})
	got := ids(slices.Collect(DistinctBy(stream, byID)))
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDistinctPassthrough(t *testing.T) {
	t.Parallel()

	unique := slices.Values([]Event{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	if got := ids(slices.Collect(DistinctBy(unique, byID))); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("unique passthrough = %v", got)
	}

	empty := slices.Values([]Event{})
	if got := slices.Collect(DistinctBy(empty, byID)); len(got) != 0 {
		t.Fatalf("empty = %v, want none", got)
	}
}

func TestDistinctEarlyBreakStopsUpstream(t *testing.T) {
	t.Parallel()

	var produced int
	src := func(yield func(Event) bool) {
		for i := range 1000 {
			produced++
			if !yield(Event{ID: string(rune('a' + i%3))}) {
				return
			}
		}
	}

	for range DistinctBy(src, byID) {
		break
	}

	if produced > 1 {
		t.Fatalf("produced = %d, want <= 1 (early break must stop upstream)", produced)
	}
}

func TestWindowEvictionReEmits(t *testing.T) {
	t.Parallel()

	// window 2: A,B fill it; C evicts A; A then re-emits (it was forgotten).
	stream := slices.Values([]Event{
		{ID: "A"}, {ID: "B"}, {ID: "A"}, {ID: "C"}, {ID: "A"},
	})
	got := ids(slices.Collect(DistinctWindow(stream, byID, 2)))
	want := []string{"A", "B", "C", "A"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("windowed = %v, want %v", got, want)
	}
}
```

## Review

`DistinctBy` is correct when each key appears exactly once in first-seen order and
unique or empty streams pass straight through — the order tests pin this. The
early-break test proves the guard honors cooperative stop so it composes with
`Take`. `DistinctWindow` is the production shape: the eviction test walks the
window boundary explicitly — `A,B` fill a size-2 window, `C` evicts `A`, and the
later `A` re-emits because it was forgotten — making the bounded-memory trade-off
concrete. The unbounded `DistinctBy` is fine for a finite batch; reach for the
windowed (or a TTL) variant the moment the stream is long-lived.

## Resources

- [`iter` package documentation](https://pkg.go.dev/iter)
- [`slices.Values`, `slices.Collect`](https://pkg.go.dev/slices)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-retry-backoff-seq.md](08-retry-backoff-seq.md) | Next: [10-error-carrying-seq2.md](10-error-carrying-seq2.md)
