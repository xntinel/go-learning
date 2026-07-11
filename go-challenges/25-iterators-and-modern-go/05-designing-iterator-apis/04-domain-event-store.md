# Exercise 4: A Domain Event Store with a Conventional Iterator API

A real collection is not just a slice with methods bolted on; its iterator surface is part of its contract. This exercise builds a time-series event store that keeps measurements ordered by time and exposes three iterators that a `slices`/`maps` user can read at a glance: `All() iter.Seq2[time.Time, float64]` for the full ordered walk, `Values() iter.Seq[float64]` for the bare measurements, and `Range(from, to) iter.Seq2[time.Time, float64]` for a half-open time window. Ordering is stable regardless of insertion order, and every iterator is reusable.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
eventstore.go        Store, Event, New, Append, Len, All, Values, Range
cmd/
  demo/
    main.go          append out of order, walk All chronologically, walk a Range window
eventstore_test.go   stable ordering, reusable iterators, break-safety, half-open Range bounds
```

- Files: `eventstore.go`, `cmd/demo/main.go`, `eventstore_test.go`.
- Implement: `Store` with `Append(t time.Time, v float64)`, `Len() int`, `All() iter.Seq2[time.Time, float64]`, `Values() iter.Seq[float64]`, and `Range(from, to time.Time) iter.Seq2[time.Time, float64]`.
- Test: `eventstore_test.go` checks that out-of-order appends still iterate in time order, that `Values()` ranged twice agrees with itself (reusable), that `break` stops every iterator, and that `Range` is half-open `[from, to)`.
- Verify: `go test -run TestStore -race ./...`

Set up the module:

```bash
mkdir -p eventstore/cmd/demo && cd eventstore
go mod init example.com/eventstore
```

### Designing the iterator surface around the domain's natural coordinate

The store's domain coordinate is time, so its iterators are shaped around `time.Time` the same way the list in Exercise 1 was shaped around the integer index. `All` returns `iter.Seq2[time.Time, float64]`: the first component is the timestamp, the second is the measurement, and a caller writes `for t, v := range store.All()`. This mirrors `slices.All` exactly, only with a domain key instead of a positional one — the convention transfers because the shape transfers. `Values` returns `iter.Seq[float64]` for the common case where a caller wants the measurements and not the clock, for instance to feed `slices.Max` or compute a mean. Returning a bare `Seq` here, rather than a `Seq2` whose key is ignored, keeps the call site honest: the method's type says it yields values, so a reader knows the timestamp was deliberately dropped, not forgotten.

`Range(from, to)` is the method that earns the store its keep. It returns `iter.Seq2[time.Time, float64]` over only the events whose time lies in the half-open interval `[from, to)` — `from` is included, `to` is excluded. Half-open is the right default for time windows because adjacent windows tile without overlap or gap: `Range(t0, t1)` and `Range(t1, t2)` together cover `[t0, t2)` and no event is visited twice. A closed `[from, to]` interval would double-count any event exactly at `t1` when you walk consecutive windows, which is the classic off-by-one that corrupts bucketed aggregates. The doc comment states the bound so a caller never has to read the body to learn it.

Stable ordering is a property of the store, not of any one iterator, so it is enforced at write time. `Append` inserts each event at the position that keeps the backing slice sorted by time, found with `sort.Search`. The predicate is `s.events[i].Time.After(t)` — the index of the first event strictly later than the new one — so an event whose time equals an existing one is inserted *after* it. That makes equal-time insertion order the tiebreaker, which is what "stable" means here: two measurements stamped at the same instant come out in the order they were appended, deterministically, on every walk. Because the slice is always sorted, `Range` can binary-search its lower bound with `sort.Search` and then walk forward until it crosses `to`, rather than scanning every event.

Every iterator returns a fresh closure over the slice the store still owns, so they are reusable: ranging `Values()` twice yields identical results, and ranging `All()` after a `Range()` is unaffected. None of them consume a shared cursor. And every loop checks `yield`'s boolean with `if !yield(...) { return }`, so a caller that `break`s stops the iterator immediately — the break-safety the tests assert.

Create `eventstore.go`:

```go
package eventstore

import (
	"iter"
	"sort"
	"time"
)

// Event is a single timestamped measurement.
type Event struct {
	Time  time.Time
	Value float64
}

// Store is an in-memory time-series buffer that keeps events ordered by time.
// Equal-time events retain their insertion order, so iteration is stable.
type Store struct {
	events []Event
}

// New returns an empty Store.
func New() *Store {
	return &Store{}
}

// Append inserts a measurement, keeping events sorted by time. An event whose
// time equals an existing one is placed after it, so equal-time events keep
// insertion order (stable ordering).
func (s *Store) Append(t time.Time, v float64) {
	i := sort.Search(len(s.events), func(i int) bool {
		return s.events[i].Time.After(t)
	})
	s.events = append(s.events, Event{})
	copy(s.events[i+1:], s.events[i:])
	s.events[i] = Event{Time: t, Value: v}
}

// Len reports the number of stored events.
func (s *Store) Len() int {
	return len(s.events)
}

// All iterates over (time, value) pairs in chronological order, mirroring
// slices.All with time as the coordinate. The sequence is reusable.
func (s *Store) All() iter.Seq2[time.Time, float64] {
	return func(yield func(time.Time, float64) bool) {
		for _, e := range s.events {
			if !yield(e.Time, e.Value) {
				return
			}
		}
	}
}

// Values iterates over measurements alone in chronological order, mirroring
// slices.Values. The sequence is reusable.
func (s *Store) Values() iter.Seq[float64] {
	return func(yield func(float64) bool) {
		for _, e := range s.events {
			if !yield(e.Value) {
				return
			}
		}
	}
}

// Range iterates over (time, value) pairs whose time lies in the half-open
// interval [from, to): from is included, to is excluded. The lower bound is
// found by binary search since events are kept sorted. The sequence is reusable.
func (s *Store) Range(from, to time.Time) iter.Seq2[time.Time, float64] {
	return func(yield func(time.Time, float64) bool) {
		lo := sort.Search(len(s.events), func(i int) bool {
			return !s.events[i].Time.Before(from) // Time >= from
		})
		for i := lo; i < len(s.events); i++ {
			if !s.events[i].Time.Before(to) { // Time >= to: past the window
				return
			}
			if !yield(s.events[i].Time, s.events[i].Value) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo appends four measurements deliberately out of order, then walks `All()` to show they emerge in time order, and walks a `Range` window to show the half-open bound dropping the event at the upper edge. Timestamps are fixed `time.Date` values so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/eventstore"
)

func at(hour int) time.Time {
	return time.Date(2026, 6, 26, hour, 0, 0, 0, time.UTC)
}

func main() {
	s := eventstore.New()
	// Appended out of order on purpose.
	s.Append(at(12), 21.5)
	s.Append(at(9), 18.0)
	s.Append(at(15), 24.2)
	s.Append(at(10), 19.1)

	fmt.Println("All (chronological):")
	for t, v := range s.All() {
		fmt.Printf("  %s  %.1f\n", t.Format("15:04"), v)
	}

	fmt.Println("Range [09:00, 12:00):")
	for t, v := range s.Range(at(9), at(12)) {
		fmt.Printf("  %s  %.1f\n", t.Format("15:04"), v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
All (chronological):
  09:00  18.0
  10:00  19.1
  12:00  21.5
  15:00  24.2
Range [09:00, 12:00):
  09:00  18.0
  10:00  19.1
```

### Tests

The tests pin the four properties the design promises. `TestStoreStableOrder` appends out of order, including two events at the same instant, and asserts the walk is sorted with the equal-time pair in insertion order. `TestStoreValuesReusable` ranges `Values()` twice and asserts the passes agree. `TestStoreBreak` breaks out of `All()` early and asserts the iterator stopped. `TestStoreRangeHalfOpen` asserts `Range` includes `from`, excludes `to`, and that adjacent windows tile without double-counting.

Create `eventstore_test.go`:

```go
package eventstore

import (
	"slices"
	"testing"
	"time"
)

func at(hour int) time.Time {
	return time.Date(2026, 6, 26, hour, 0, 0, 0, time.UTC)
}

func TestStoreStableOrder(t *testing.T) {
	t.Parallel()

	s := New()
	s.Append(at(12), 21.5)
	s.Append(at(9), 18.0)
	s.Append(at(9), 18.9) // same instant as the previous: must come after it
	s.Append(at(10), 19.1)

	var hours []int
	var vals []float64
	for ts, v := range s.All() {
		hours = append(hours, ts.Hour())
		vals = append(vals, v)
	}
	if !slices.Equal(hours, []int{9, 9, 10, 12}) {
		t.Fatalf("hours = %v, want [9 9 10 12]", hours)
	}
	if !slices.Equal(vals, []float64{18.0, 18.9, 19.1, 21.5}) {
		t.Fatalf("values = %v, want stable equal-time order", vals)
	}
}

func TestStoreValuesReusable(t *testing.T) {
	t.Parallel()

	s := New()
	s.Append(at(9), 1)
	s.Append(at(10), 2)
	s.Append(at(11), 3)

	first := slices.Collect(s.Values())
	second := slices.Collect(s.Values())
	if !slices.Equal(first, second) {
		t.Fatalf("reusable iterator differs: first %v, second %v", first, second)
	}
	if !slices.Equal(first, []float64{1, 2, 3}) {
		t.Fatalf("values = %v, want [1 2 3]", first)
	}
}

func TestStoreBreak(t *testing.T) {
	t.Parallel()

	s := New()
	for h := 9; h <= 13; h++ {
		s.Append(at(h), float64(h))
	}

	var seen []float64
	for _, v := range s.All() {
		if len(seen) == 2 {
			break
		}
		seen = append(seen, v)
	}
	if !slices.Equal(seen, []float64{9, 10}) {
		t.Fatalf("break did not stop iteration: saw %v, want [9 10]", seen)
	}
}

func TestStoreRangeHalfOpen(t *testing.T) {
	t.Parallel()

	s := New()
	for h := 9; h <= 14; h++ {
		s.Append(at(h), float64(h))
	}

	collect := func(from, to time.Time) []int {
		var hs []int
		for ts := range s.Range(from, to) {
			hs = append(hs, ts.Hour())
		}
		return hs
	}

	// from included, to excluded.
	if got := collect(at(10), at(13)); !slices.Equal(got, []int{10, 11, 12}) {
		t.Fatalf("Range[10,13) = %v, want [10 11 12]", got)
	}
	// Adjacent windows tile without overlap: 13 appears once, total has no gap.
	left := collect(at(9), at(12))
	right := collect(at(12), at(15))
	if !slices.Equal(left, []int{9, 10, 11}) {
		t.Fatalf("left window = %v, want [9 10 11]", left)
	}
	if !slices.Equal(right, []int{12, 13, 14}) {
		t.Fatalf("right window = %v, want [12 13 14]", right)
	}
	combined := append(append([]int{}, left...), right...)
	if !slices.Equal(combined, []int{9, 10, 11, 12, 13, 14}) {
		t.Fatalf("tiled windows = %v, want no overlap and no gap", combined)
	}
}
```

## Review

The store is well-designed when its iterator shapes match the standard library and its ordering is a property of the type rather than of any single walk. Confirm `All` and `Range` both return `iter.Seq2[time.Time, float64]` so a caller writes `for t, v := range` over either, and that `Values` returns the bare `iter.Seq[float64]` so dropping the timestamp is visible in the type. Confirm ordering is enforced in `Append`: the `sort.Search` predicate `Time.After(t)` places equal-time events after the ones already present, which is what makes the stable-order test pass on every run. Confirm `Range` is half-open by reading the two `Before` checks — `!Time.Before(from)` includes `from`, `!Time.Before(to)` stops before `to` — which is exactly what lets adjacent windows tile without double-counting the boundary event.

The common mistakes are deviating from the conventional shapes, sorting lazily, and getting the interval bound wrong. Returning `Seq2` from `Values` with an ignored key, or `Seq` from `Range` that hides the timestamp, both compile but mislead the reader about what the method yields. Leaving events unsorted and sorting inside each iterator would make two walks of the same store potentially disagree if `Append` ran between them, and would turn every iteration into an O(n log n) sort; sorting once at write time keeps the iterators O(n) and their results stable. Making `Range` closed on both ends, `[from, to]`, is the bug that silently double-counts the boundary event when a caller walks consecutive windows — the tiling assertion is the test that catches it.

## Resources

- [`iter` package: Naming Conventions](https://pkg.go.dev/iter#hdr-Naming_Conventions) — the standard `All`/`Values` names and the `Seq`/`Seq2` shapes a domain collection should mirror.
- [`slices.All` and `slices.Values`](https://pkg.go.dev/slices#All) — the positional-coordinate iterators whose shape this store reuses with time as the key.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — the binary search that keeps appends sorted and finds the lower bound of a `Range` window.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the Go blog post introducing the iterator design and the `All` convention.

---

Back to [03-fallible-iterators.md](03-fallible-iterators.md) | Next: [05-paginated-repository.md](05-paginated-repository.md)
