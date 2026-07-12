# Exercise 3: Range Query Over an Append-Mostly Event Log by Timestamp

An in-memory metrics or trace buffer is an append-mostly slice of events kept
sorted by timestamp. Its hot path is the *window query*: "give me every event in
`[from, to)`". That is two binary searches — a lower bound at `from` and an upper
bound at `to` — over the sorted timestamps. This exercise builds that read path
with `slices.BinarySearchFunc`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
eventwindow/                 independent module: example.com/eventwindow
  go.mod
  store.go                   type Event, Store; Append, Window, Len
  cmd/
    demo/
      main.go                seed events, query a time window
  store_test.go              duplicate timestamps, boundary inclusion, empty windows, copy proof
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: `Store` holding events sorted by `UnixNano`, with `Append(Event)`, `Window(from, to time.Time) []Event`, `Len() int`.
Test: duplicate timestamps at the boundaries, windows before/after/covering all data, `from == to`, and a copy proof.
Verify: `go test -count=1 -race ./...`

### Two boundary searches, and why the interval is half-open

An event carries a timestamp and a payload; the store keeps them sorted by
`Time.UnixNano()`. `Append` assumes events arrive in non-decreasing time order
(the append-mostly assumption real telemetry buffers make) and records them; it
also asserts the invariant in development so an out-of-order append is caught
rather than silently corrupting later queries.

`Window(from, to)` computes two indices with the *same* comparator:

- `lo` = first index whose timestamp is `>= from` — the lower bound.
- `hi` = first index whose timestamp is `>= to` — the upper bound.

`slices.BinarySearchFunc(events, target, cmp)` returns exactly the smallest index
`i` at which `cmp(events[i], target) >= 0`, which is the lower bound for that
target. We call it twice, once with `from` and once with `to`, and the window is
`events[lo:hi]`.

The half-open interval `[from, to)` is what makes the boundaries behave. Events
whose timestamp equals `from` are *included* (the lower bound stops at the first
`>= from`), and events whose timestamp equals `to` are *excluded* (the upper
bound stops at the first `>= to`, so the run equal to `to` is left out). This is
the correct semantics for time windows: consecutive windows `[t0, t1)` and
`[t1, t2)` tile the timeline with no event counted twice and none dropped. When
timestamps are duplicated, the lower bound lands before the whole equal run and
the upper bound after it, so the window includes every event *at* `from` and no
event *at* `to`.

The comparator uses `cmp.Compare(a, b)` on the `int64` nanosecond values rather
than raw subtraction — subtracting two `int64` timestamps can overflow and flip
the sign, which would corrupt the search. `cmp.Compare` is overflow-safe.

Finally `Window` returns `slices.Clone(events[lo:hi])`: the caller gets an
independent copy, so a later `Append` that grows the store cannot clobber a
window the caller is still reading.

Create `store.go`:

```go
package eventwindow

import (
	"cmp"
	"slices"
	"time"
)

// Event is a timestamped record in the store.
type Event struct {
	Time    time.Time
	Payload string
}

// Store holds events sorted by timestamp. It is append-mostly: Append expects
// non-decreasing timestamps, which keeps the slice sorted for Window's binary
// searches.
type Store struct {
	events []Event
}

// New returns an empty store.
func New() *Store {
	return &Store{}
}

// byTime compares an event against a target instant by nanosecond, overflow-safe.
func byTime(e Event, target time.Time) int {
	return cmp.Compare(e.Time.UnixNano(), target.UnixNano())
}

// Append records e. It panics if e would break the sorted-by-time invariant,
// turning an out-of-order producer into a loud failure instead of silent
// query corruption.
func (s *Store) Append(e Event) {
	if n := len(s.events); n > 0 && e.Time.UnixNano() < s.events[n-1].Time.UnixNano() {
		panic("eventwindow: Append out of order")
	}
	s.events = append(s.events, e)
}

// Window returns a copy of every event in the half-open interval [from, to):
// timestamps equal to from are included, timestamps equal to to are excluded.
func (s *Store) Window(from, to time.Time) []Event {
	lo, _ := slices.BinarySearchFunc(s.events, from, byTime)
	hi, _ := slices.BinarySearchFunc(s.events, to, byTime)
	if lo >= hi {
		return []Event{}
	}
	return slices.Clone(s.events[lo:hi])
}

// Len reports the number of stored events.
func (s *Store) Len() int {
	return len(s.events)
}
```

### The runnable demo

The demo uses a fixed base instant so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/eventwindow"
)

func main() {
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	s := eventwindow.New()
	for i := range 6 {
		s.Append(eventwindow.Event{
			Time:    base.Add(time.Duration(i) * time.Second),
			Payload: fmt.Sprintf("evt-%d", i),
		})
	}

	from := base.Add(2 * time.Second)
	to := base.Add(5 * time.Second)
	win := s.Window(from, to)

	fmt.Printf("total events: %d\n", s.Len())
	fmt.Printf("window [2s,5s) has %d events:\n", len(win))
	for _, e := range win {
		fmt.Printf("  %s at +%ds\n", e.Payload, int(e.Time.Sub(base).Seconds()))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total events: 6
window [2s,5s) has 3 events:
  evt-2 at +2s
  evt-3 at +3s
  evt-4 at +4s
```

### Tests

The suite seeds events including *duplicate* timestamps to prove the boundary
semantics, then covers a window before all data, after all data, covering
everything, an exact-boundary window, and a degenerate `from == to`. A final test
mutates the returned slice and asserts the store is intact.

Create `store_test.go`:

```go
package eventwindow

import (
	"fmt"
	"testing"
	"time"
)

var base = time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

// seed builds a store with timestamps 1,2,2,2,3,5 seconds (note the triple at 2s).
func seed(t *testing.T) *Store {
	t.Helper()
	s := New()
	for _, sec := range []int{1, 2, 2, 2, 3, 5} {
		s.Append(Event{Time: at(sec), Payload: "e"})
	}
	return s
}

func TestWindowIncludesFromExcludesTo(t *testing.T) {
	t.Parallel()

	s := seed(t)
	// [2s, 3s): must include all three events at 2s, exclude the 3s event.
	got := s.Window(at(2), at(3))
	if len(got) != 3 {
		t.Fatalf("Window(2s,3s) = %d events, want 3 (the triple at 2s)", len(got))
	}
	for _, e := range got {
		if e.Time.UnixNano() != at(2).UnixNano() {
			t.Fatalf("window contained an event at %v, want all at 2s", e.Time)
		}
	}
}

func TestWindowBeforeAllData(t *testing.T) {
	t.Parallel()

	if got := seed(t).Window(base.Add(-time.Hour), at(0)); len(got) != 0 {
		t.Fatalf("window before all data = %d events, want 0", len(got))
	}
}

func TestWindowAfterAllData(t *testing.T) {
	t.Parallel()

	if got := seed(t).Window(at(6), at(10)); len(got) != 0 {
		t.Fatalf("window after all data = %d events, want 0", len(got))
	}
}

func TestWindowCoversEverything(t *testing.T) {
	t.Parallel()

	s := seed(t)
	if got := s.Window(at(0), at(100)); len(got) != s.Len() {
		t.Fatalf("full window = %d events, want %d", len(got), s.Len())
	}
}

func TestWindowFromEqualsTo(t *testing.T) {
	t.Parallel()

	if got := seed(t).Window(at(2), at(2)); len(got) != 0 {
		t.Fatalf("Window(2s,2s) = %d events, want 0 (empty half-open interval)", len(got))
	}
}

func TestWindowReturnsCopy(t *testing.T) {
	t.Parallel()

	s := seed(t)
	got := s.Window(at(1), at(6))
	if len(got) == 0 {
		t.Fatal("expected a non-empty window")
	}
	got[0].Payload = "mutated"

	again := s.Window(at(1), at(6))
	if again[0].Payload == "mutated" {
		t.Fatal("mutating the window corrupted the store")
	}
}

func TestAppendOutOfOrderPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("out-of-order Append should panic")
		}
	}()
	s := New()
	s.Append(Event{Time: at(5)})
	s.Append(Event{Time: at(1)}) // earlier: must panic
}

func Example() {
	base := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	s := New()
	for _, sec := range []int{10, 20, 30, 40} {
		s.Append(Event{Time: base.Add(time.Duration(sec) * time.Second), Payload: "e"})
	}
	win := s.Window(base.Add(20*time.Second), base.Add(40*time.Second))
	fmt.Println(len(win)) // events at 20s and 30s; 40s excluded
	// Output: 2
}
```

## Review

The window is correct when the lower bound includes the whole run at `from` and
the upper bound excludes the whole run at `to` — the triple-timestamp test is what
proves it. The three most common regressions: using `> from` instead of `>= from`
for the lower bound (drops events exactly at `from`); using `>= to` semantics but
reading `events[lo:hi]` with `hi` computed as an *inclusive* bound (includes
`to`); and returning `events[lo:hi]` without cloning (a later `Append` clobbers
the caller). Using `cmp.Compare` rather than timestamp subtraction avoids an
`int64` overflow flipping the comparator. Run `go test -race`.

## Resources

- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the comparator contract and the returned lower bound.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — overflow-safe three-way comparison.
- [`time.Time.UnixNano`](https://pkg.go.dev/time#Time.UnixNano) — the monotone key the store sorts on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-consistent-hash-ring.md](04-consistent-hash-ring.md)
