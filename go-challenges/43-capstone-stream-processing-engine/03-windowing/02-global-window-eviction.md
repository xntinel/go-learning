# Exercise 2: Global Windows with Count Triggers and Eviction

Not every window is bounded by time. A *global* window assigns every record for a key to one window that never closes on its own, which is meaningless until you add a trigger that says when to emit and an evictor that says what to keep. This exercise builds a count-based sliding window — "the sum of the last N readings, recomputed every M readings" — out of exactly those three pieces: a single global window per key, a count trigger that fires every `Slide` records, and a count evictor that retains the most recent `Size`. The result references no clock at all.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
countwin.go            Window, CountWindowOperator, Add, BufferLen
cmd/
  demo/
    main.go            sliding count window, tumbling count window, per-key isolation
countwin_test.go       emission sequences, eviction bound, slide gating, key isolation
```

- Files: `countwin.go`, `cmd/demo/main.go`, `countwin_test.go`.
- Implement: `CountWindowOperator` with `Add(key, value) (Window, bool)` and `BufferLen(key) int`, parameterised by `Size` (evictor retention) and `Slide` (trigger period).
- Test: the emission sequence for sliding and tumbling configurations, the eviction bound on buffer length, that no window emits before `Slide` records arrive, and that two keys keep independent buffers.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a global window needs both a trigger and an evictor

A time window carries its own closing condition: when the clock passes the window end, the window fires and its state is purged. A global window has no such condition — it is one open-ended bucket per key — so on its own it would accumulate every record forever and never produce a result. Two policies make it useful, and they answer two different questions. The trigger answers *when do I emit?* The evictor answers *what do I keep?* Time windows fold both answers into "fire and purge at the boundary"; a global window has to state them separately, and that separation is exactly what makes count-based windows expressible.

Here the trigger is a count trigger with period `Slide`: it fires every `Slide` records. The evictor is a count evictor with retention `Size`: when the window fires, it keeps the most recent `Size` elements and discards the rest before the aggregate is computed. Pair `Size=3, Slide=1` and you get "the sum of the last three readings, emitted on every reading" — a sliding count window. Pair `Size=4, Slide=4` and every emission consumes a fresh, non-overlapping group of four — a tumbling count window. The same machinery, two different `(Size, Slide)` settings; this is the count-domain analogue of the time-domain tumbling-versus-sliding distinction, and Flink builds `countWindow(size, slide)` from precisely a global window plus a count trigger of `slide` plus a count evictor of `size`.

The early emissions are deliberately partial. The very first record fires the trigger (one record is a full `Slide` when `Slide=1`), and the evictor cannot keep three elements when only one has arrived, so the first window reports a count of one. This matches every production count-window implementation: the window warms up to its full size and the early results carry fewer elements. The tests pin this behaviour rather than hiding it.

### Why eviction must copy, not reslice

The evictor is where the unbounded global buffer becomes bounded, and it is also where a naive implementation leaks. The tempting line is `buf = buf[len(buf)-Size:]` — reslice to the last `Size` elements. It produces the right values, but the resliced header still points into the original backing array, so every evicted element it skipped over remains reachable and uncollectable. Over a long-running stream the backing array grows without limit even though the logical buffer never exceeds `Size`. The fix is to copy the retained tail into a fresh `Size`-length slice, which lets the old backing array — and every evicted element in it — be collected. `BufferLen` exists so a test can assert the bound holds: after any number of records, the buffer never exceeds `Size`.

Create `countwin.go`:

```go
// Package countwin implements count-based windows: a single global window per
// key, a count trigger that fires every Slide records, and a count evictor that
// retains the most recent Size records. No clock is involved.
package countwin

import "sync"

// Window is the result emitted when a count window fires: the aggregate over the
// records the evictor retained.
type Window struct {
	Sum   int64 // sum of the retained values
	Count int   // number of retained values (<= Size)
}

// CountWindowOperator maintains one global window per key. Every Slide records it
// fires, evicts all but the most recent Size records, and emits their aggregate.
//
// CountWindowOperator is safe for concurrent use.
type CountWindowOperator struct {
	size  int
	slide int

	mu        sync.Mutex
	buffers   map[string][]int64
	sinceFire map[string]int
}

// NewCountWindowOperator returns an operator with the given retention (size) and
// trigger period (slide). Both are clamped to a minimum of 1.
func NewCountWindowOperator(size, slide int) *CountWindowOperator {
	if size < 1 {
		size = 1
	}
	if slide < 1 {
		slide = 1
	}
	return &CountWindowOperator{
		size:      size,
		slide:     slide,
		buffers:   make(map[string][]int64),
		sinceFire: make(map[string]int),
	}
}

// Add appends value to key's global window. It returns (window, true) when the
// count trigger fires on this record, or (zero, false) when the record only
// accumulates. On a firing, the evictor retains the most recent Size values and
// the returned Window aggregates exactly those.
func (op *CountWindowOperator) Add(key string, value int64) (Window, bool) {
	op.mu.Lock()
	defer op.mu.Unlock()

	buf := append(op.buffers[key], value)
	op.sinceFire[key]++

	if op.sinceFire[key] < op.slide {
		op.buffers[key] = buf
		return Window{}, false
	}

	// Trigger fires. Evict: retain the most recent Size values. Copy into a fresh
	// slice so the old backing array (and every evicted value) can be collected.
	if len(buf) > op.size {
		keep := make([]int64, op.size)
		copy(keep, buf[len(buf)-op.size:])
		buf = keep
	}
	op.buffers[key] = buf
	op.sinceFire[key] = 0

	var sum int64
	for _, v := range buf {
		sum += v
	}
	return Window{Sum: sum, Count: len(buf)}, true
}

// BufferLen reports how many values are currently retained for key. It never
// exceeds Size once the window has fired at least once past its warm-up.
func (op *CountWindowOperator) BufferLen(key string) int {
	op.mu.Lock()
	defer op.mu.Unlock()
	return len(op.buffers[key])
}
```

### The runnable demo

The demo shows the three behaviours that the `(Size, Slide)` pair selects. With `Size=3, Slide=1` the operator emits on every record and the sums slide over the most recent three values, warming up from a count of one. With `Size=4, Slide=4` the operator emits once per four records over disjoint groups. With two keys it keeps independent buffers, so interleaved records never mix.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/countwin"
)

func main() {
	fmt.Println("=== Sliding count window (size=3, slide=1) ===")
	{
		op := countwin.NewCountWindowOperator(3, 1)
		for _, v := range []int64{10, 20, 30, 40, 50} {
			if w, fired := op.Add("s1", v); fired {
				fmt.Printf("  add %3d -> emit sum=%d count=%d\n", v, w.Sum, w.Count)
			}
		}
	}

	fmt.Println("\n=== Tumbling count window (size=4, slide=4) ===")
	{
		op := countwin.NewCountWindowOperator(4, 4)
		for _, v := range []int64{1, 2, 3, 4, 5, 6, 7, 8} {
			if w, fired := op.Add("s2", v); fired {
				fmt.Printf("  add %d -> emit sum=%d count=%d\n", v, w.Sum, w.Count)
			}
		}
	}

	fmt.Println("\n=== Per-key isolation (size=2, slide=2) ===")
	{
		op := countwin.NewCountWindowOperator(2, 2)
		type ev struct {
			key string
			val int64
		}
		for _, e := range []ev{{"a", 1}, {"b", 10}, {"a", 2}, {"b", 20}} {
			if w, fired := op.Add(e.key, e.val); fired {
				fmt.Printf("  key=%s emit sum=%d count=%d\n", e.key, w.Sum, w.Count)
			}
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== Sliding count window (size=3, slide=1) ===
  add  10 -> emit sum=10 count=1
  add  20 -> emit sum=30 count=2
  add  30 -> emit sum=60 count=3
  add  40 -> emit sum=90 count=3
  add  50 -> emit sum=120 count=3

=== Tumbling count window (size=4, slide=4) ===
  add 4 -> emit sum=10 count=4
  add 8 -> emit sum=26 count=4

=== Per-key isolation (size=2, slide=2) ===
  key=a emit sum=3 count=2
  key=b emit sum=30 count=2
```

### Tests

The tests pin the full emission sequence for the sliding and tumbling configurations, so any change to the eviction or trigger logic is caught by an exact mismatch rather than a vague failure. `TestEvictionBoundsBuffer` feeds far more records than `Size` and asserts the retained buffer never exceeds `Size` — the property that makes the global window safe to run forever. `TestNoEmitBeforeSlide` checks the trigger gates correctly, and `TestKeyIsolation` proves two keys never share a buffer.

Create `countwin_test.go`:

```go
package countwin

import "testing"

// feed adds every value for key and collects the windows that fired.
func feed(op *CountWindowOperator, key string, vals ...int64) []Window {
	var out []Window
	for _, v := range vals {
		if w, fired := op.Add(key, v); fired {
			out = append(out, w)
		}
	}
	return out
}

func equalWindows(a, b []Window) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSlidingCountWindow(t *testing.T) {
	t.Parallel()
	op := NewCountWindowOperator(3, 1)
	got := feed(op, "s1", 10, 20, 30, 40, 50)
	want := []Window{
		{Sum: 10, Count: 1},
		{Sum: 30, Count: 2},
		{Sum: 60, Count: 3},
		{Sum: 90, Count: 3},
		{Sum: 120, Count: 3},
	}
	if !equalWindows(got, want) {
		t.Fatalf("sliding windows = %v, want %v", got, want)
	}
}

func TestTumblingCountWindow(t *testing.T) {
	t.Parallel()
	op := NewCountWindowOperator(4, 4)
	got := feed(op, "s2", 1, 2, 3, 4, 5, 6, 7, 8)
	want := []Window{
		{Sum: 10, Count: 4},
		{Sum: 26, Count: 4},
	}
	if !equalWindows(got, want) {
		t.Fatalf("tumbling windows = %v, want %v", got, want)
	}
}

func TestEvictionBoundsBuffer(t *testing.T) {
	t.Parallel()
	op := NewCountWindowOperator(3, 1)
	for i := int64(0); i < 1000; i++ {
		op.Add("k", i)
		if got := op.BufferLen("k"); got > 3 {
			t.Fatalf("buffer length = %d after %d records, want <= 3", got, i+1)
		}
	}
	if got := op.BufferLen("k"); got != 3 {
		t.Fatalf("final buffer length = %d, want 3", got)
	}
}

func TestNoEmitBeforeSlide(t *testing.T) {
	t.Parallel()
	op := NewCountWindowOperator(3, 3)
	if _, fired := op.Add("k", 1); fired {
		t.Fatal("first record fired with slide=3")
	}
	if _, fired := op.Add("k", 2); fired {
		t.Fatal("second record fired with slide=3")
	}
	w, fired := op.Add("k", 3)
	if !fired {
		t.Fatal("third record did not fire with slide=3")
	}
	if w.Sum != 6 || w.Count != 3 {
		t.Fatalf("window = %v, want {6 3}", w)
	}
}

func TestKeyIsolation(t *testing.T) {
	t.Parallel()
	op := NewCountWindowOperator(2, 2)
	op.Add("a", 1)
	op.Add("b", 100)
	wa, firedA := op.Add("a", 2)
	if !firedA || wa.Sum != 3 || wa.Count != 2 {
		t.Fatalf("key a window = %v fired=%v, want {3 2} true", wa, firedA)
	}
	wb, firedB := op.Add("b", 200)
	if !firedB || wb.Sum != 300 || wb.Count != 2 {
		t.Fatalf("key b window = %v fired=%v, want {300 2} true", wb, firedB)
	}
}
```

## Review

The operator is correct when the trigger and the evictor each do their one job and nothing more. The most common error is forgetting that a global window must be bounded by the evictor: drop the eviction step and the buffer grows without limit while the sums stay correct, so tests that only check sums pass while the process slowly runs out of memory — which is why `TestEvictionBoundsBuffer` checks the length, not the value. The second error is reslicing instead of copying in the evictor, which bounds the logical length but pins the original backing array and every value it ever held; the copy is what actually releases memory. The third is resetting the buffer to empty on a firing instead of retaining the most recent `Size` — that turns a sliding count window into a tumbling one and breaks the overlap. The fourth is gating the trigger on the wrong counter: it must fire every `Slide` records since the last firing, not every `Slide` records since the start. Running under `go test -race` confirms the per-key map access is properly synchronised.

## Resources

- [Apache Flink: Triggers and Evictors](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/windows/#triggers) — the production model of a global window driven by a separate trigger and evictor, exactly as built here.
- [Apache Flink: GlobalWindows](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/windows/#global-windows) — the assigner that puts every element into one never-closing window, the basis for count windows.
- [Go slices: usage and internals](https://go.dev/blog/slices-intro) — why reslicing a tail keeps the whole backing array alive, and why the evictor copies.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-window-operator.md](01-window-operator.md) | Next: [03-pane-sharing.md](03-pane-sharing.md)
