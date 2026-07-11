# Exercise 29: Watermark-Based Stream Window Emission

**Nivel: Intermedio** — validacion rapida (un test corto).

A clickstream processor that groups events into one-minute buckets for
aggregation cannot hold the entire stream in memory and sort it after the
fact — it has to emit each bucket's results as soon as it is sure no more
events for that minute are coming, then free the memory. When the input
is already time-ordered (as it typically is once it passes through an
upstream partition or sort), that certainty arrives the instant an event
with a *later* timestamp shows up: the watermark has advanced, and the
previous bucket is done. This module builds that single-pass emission
loop, capped so a stream with an unexpectedly large number of distinct
time buckets cannot grow the result without bound.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
window/                        module example.com/window
  go.mod                       go 1.24
  window.go                    Event; Window; Emit(events, size, maxGroups) []Window
  window_test.go                 grouping, cap mid-stream, empty input, zero cap, single event, boundary timestamps
  cmd/demo/
    main.go                      five clickstream events emitted in full, then capped at two groups
```

- Files: `window.go`, `window_test.go`, `cmd/demo/main.go`.
- Implement: `Emit(events []Event, size time.Duration, maxGroups int) []Window` — a condition-only `for i < len(events) && len(windows) < maxGroups` loop that flushes the current bucket and re-checks the triggering event with `continue` the instant its window differs from the current one, plus a final flush after the loop for the last, still-open bucket.
- Test: five events spanning three one-minute windows group correctly; the cap stops emission before the stream ends; an empty stream returns `nil`; a `maxGroups` of zero returns `nil`; a single event still gets its window flushed; events that land exactly on window boundaries each start a new group.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/window/cmd/demo
cd ~/go-exercises/window
go mod init example.com/window
go mod edit -go=1.24
```

### Why the boundary event is re-checked instead of consumed immediately

`Emit`'s loop has exactly one subtlety, and it lives in the `continue`
inside the `if ws.After(current)` branch. The event that reveals the
watermark has advanced — the first event of the *new* window — is not yet
part of the bucket being flushed, but it also should not be silently
dropped; it belongs in the next bucket. Consuming it in the same pass that
triggers the flush would require the flush and the append to share one
iteration, which either duplicates the append logic or forces an awkward
"flush, then fall through to the normal append" structure. Instead, the
loop flushes the completed window, updates `current` to the new window's
start, and loops back around *without* advancing `i` — so the very next
pass re-evaluates the same event against the now-current window, finds
`ws.After(current)` false, and appends it normally. The event is examined
twice, but only ever counted into a bucket once, and the loop condition
`i < len(events)` still strictly decreases in total work because `i` never
goes backward, only pauses for one extra pass.

The cap interacts with this cleanly because it is checked in the loop
condition itself, not inside the body: the moment `len(windows) ==
maxGroups`, the loop simply stops, whether it was mid-bucket or between
buckets. `bucket` is guaranteed to be `nil` at that point whenever the cap
was hit exactly at a flush (the reset happens in the same branch as the
append), which is what keeps the post-loop "flush the final bucket" check
from double-emitting a window that the cap already accounted for.

Create `window.go`:

```go
package window

import "time"

// Event is one timestamped record from a sorted input stream (already
// ordered by Timestamp, as a stream processor's input typically is once it
// has passed through an upstream sort or a partition that guarantees order).
type Event struct {
	Timestamp time.Time
	Value     string
}

// Window is one emitted group: every event whose timestamp fell in
// [Start, Start+size).
type Window struct {
	Start  time.Time
	Events []string
}

// Emit groups sorted events into fixed-size time windows and emits a Window
// each time the watermark -- the boundary of the current window -- advances
// past an event's timestamp, capping the total number of emitted windows at
// maxGroups so a stream with pathologically many distinct time buckets
// cannot grow the result without bound.
//
// The loop is condition-only: it runs while there are events left to
// consume AND the group cap has not been reached, so it has two entirely
// different ways to stop, and both are checked at the top of every pass
// rather than buried in the body. When an event's window differs from the
// current one, the loop flushes the completed window and re-checks the same
// event against the new window on the next pass (via continue) instead of
// consuming it immediately, which is what lets a single event correctly
// trigger a flush before it is itself counted into the next group.
func Emit(events []Event, size time.Duration, maxGroups int) []Window {
	var windows []Window
	if len(events) == 0 || maxGroups <= 0 {
		return windows
	}

	current := events[0].Timestamp.Truncate(size)
	var bucket []string

	i := 0
	for i < len(events) && len(windows) < maxGroups {
		ws := events[i].Timestamp.Truncate(size)
		if ws.After(current) {
			windows = append(windows, Window{Start: current, Events: bucket})
			bucket = nil
			current = ws
			continue // re-check this same event against the new window
		}
		bucket = append(bucket, events[i].Value)
		i++
	}

	if len(windows) < maxGroups && len(bucket) > 0 {
		windows = append(windows, Window{Start: current, Events: bucket})
	}
	return windows
}
```

### The runnable demo

Five clickstream events span three one-minute windows. The demo emits them
in full, then again capped at two groups, so the middle example line
disappears entirely from the capped output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/window"
)

func at(minute, second int) time.Time {
	return time.Date(2026, 7, 5, 9, minute, second, 0, time.UTC)
}

func main() {
	events := []window.Event{
		{Timestamp: at(0, 2), Value: "click"},
		{Timestamp: at(0, 47), Value: "view"},
		{Timestamp: at(1, 5), Value: "click"},
		{Timestamp: at(1, 6), Value: "purchase"},
		{Timestamp: at(2, 58), Value: "click"},
	}

	windows := window.Emit(events, time.Minute, 100)
	fmt.Printf("full emit: %d windows\n", len(windows))
	for _, w := range windows {
		fmt.Printf("  [%s] %v\n", w.Start.Format("15:04"), w.Events)
	}

	capped := window.Emit(events, time.Minute, 2)
	fmt.Printf("capped at 2 groups: %d windows\n", len(capped))
	for _, w := range capped {
		fmt.Printf("  [%s] %v\n", w.Start.Format("15:04"), w.Events)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
full emit: 3 windows
  [09:00] [click view]
  [09:01] [click purchase]
  [09:02] [click]
capped at 2 groups: 2 windows
  [09:00] [click view]
  [09:01] [click purchase]
```

### Tests

`TestEmitGroupsByMinuteWatermark` checks the ordinary multi-window case.
`TestEmitCapsAtMaxGroups` confirms the cap stops emission before the
stream is exhausted. `TestEmitEmptyEventsReturnsNil` and
`TestEmitZeroMaxGroupsReturnsNil` cover the two ways the function should
do nothing at all. `TestEmitSingleEventFlushesFinalWindow` exercises the
post-loop flush directly. `TestEmitEventsExactlyOnWindowBoundaries` checks
that events landing precisely on a truncation boundary still each start
their own window rather than bleeding into the previous one. Fixed
timestamps built with `time.Date` keep every assertion reproducible
without a real clock.

Create `window_test.go`:

```go
package window

import (
	"slices"
	"testing"
	"time"
)

func at(minute, second int) time.Time {
	return time.Date(2026, 7, 5, 10, minute, second, 0, time.UTC)
}

func TestEmitGroupsByMinuteWatermark(t *testing.T) {
	t.Parallel()

	events := []Event{
		{Timestamp: at(0, 5), Value: "a"},
		{Timestamp: at(0, 40), Value: "b"},
		{Timestamp: at(1, 10), Value: "c"},
		{Timestamp: at(1, 50), Value: "d"},
		{Timestamp: at(2, 0), Value: "e"},
	}

	got := Emit(events, time.Minute, 100)
	want := []Window{
		{Start: at(0, 0), Events: []string{"a", "b"}},
		{Start: at(1, 0), Events: []string{"c", "d"}},
		{Start: at(2, 0), Events: []string{"e"}},
	}
	assertWindowsEqual(t, got, want)
}

func TestEmitCapsAtMaxGroups(t *testing.T) {
	t.Parallel()

	events := []Event{
		{Timestamp: at(0, 0), Value: "a"},
		{Timestamp: at(1, 0), Value: "b"},
		{Timestamp: at(2, 0), Value: "c"},
		{Timestamp: at(3, 0), Value: "d"},
	}

	got := Emit(events, time.Minute, 2)
	want := []Window{
		{Start: at(0, 0), Events: []string{"a"}},
		{Start: at(1, 0), Events: []string{"b"}},
	}
	assertWindowsEqual(t, got, want)
}

func TestEmitEmptyEventsReturnsNil(t *testing.T) {
	t.Parallel()

	got := Emit(nil, time.Minute, 100)
	if got != nil {
		t.Fatalf("Emit(nil) = %v, want nil", got)
	}
}

func TestEmitZeroMaxGroupsReturnsNil(t *testing.T) {
	t.Parallel()

	events := []Event{{Timestamp: at(0, 0), Value: "a"}}
	got := Emit(events, time.Minute, 0)
	if got != nil {
		t.Fatalf("Emit() with maxGroups=0 = %v, want nil", got)
	}
}

func TestEmitSingleEventFlushesFinalWindow(t *testing.T) {
	t.Parallel()

	events := []Event{{Timestamp: at(5, 30), Value: "only"}}
	got := Emit(events, time.Minute, 100)
	want := []Window{{Start: at(5, 0), Events: []string{"only"}}}
	assertWindowsEqual(t, got, want)
}

func TestEmitEventsExactlyOnWindowBoundaries(t *testing.T) {
	t.Parallel()

	events := []Event{
		{Timestamp: at(0, 0), Value: "a"},
		{Timestamp: at(1, 0), Value: "b"},
		{Timestamp: at(2, 0), Value: "c"},
	}

	got := Emit(events, time.Minute, 100)
	want := []Window{
		{Start: at(0, 0), Events: []string{"a"}},
		{Start: at(1, 0), Events: []string{"b"}},
		{Start: at(2, 0), Events: []string{"c"}},
	}
	assertWindowsEqual(t, got, want)
}

func assertWindowsEqual(t *testing.T, got, want []Window) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("Emit() returned %d windows, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !slices.Equal(got[i].Events, want[i].Events) {
			t.Fatalf("window %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
```

## Review

`Emit` is correct when every emitted window's events all truncate to that
window's `Start`, windows appear in ascending time order, and the total
count never exceeds `maxGroups` — and each of those depends on the same
loop invariant: `current` always equals the truncated timestamp of every
event in `bucket`, and it is only ever advanced at the same instant
`bucket` is flushed and reset. The common mistake this design avoids is
consuming the boundary-crossing event in the same step that triggers the
flush (`windows = append(...); bucket = []string{events[i].Value}; i++`
all in the `if` branch) — that looks equivalent for a single flush, but it
silently duplicates the append/reset logic in two places, and the two
copies drifting apart over time (one gets a bugfix, the other does not) is
a realistic way this class of streaming code rots. Run `go test -count=1
./...`.

## Resources

- [Streaming Systems (O'Reilly) — watermarks](https://www.oreilly.com/library/view/streaming-systems/9781491983867/) — the watermark model this module's window boundary implements.
- [time.Time.Truncate](https://pkg.go.dev/time#Time.Truncate) — rounding a timestamp down to its containing window.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the condition-only loop with two independent stopping conditions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-bloom-filter-cardinality-sketch.md](28-bloom-filter-cardinality-sketch.md) | Next: [30-write-ahead-log-compaction-snapshot.md](30-write-ahead-log-compaction-snapshot.md)
