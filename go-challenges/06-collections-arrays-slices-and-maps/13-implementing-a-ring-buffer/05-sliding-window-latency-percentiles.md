# Exercise 5: Sliding-Window Latency Tracker for p50/p95/p99

Every SRE dashboard wants rolling latency percentiles: p50/p95/p99 over recent
traffic, without storing every request forever. A ring of the last K durations is
the fixed-cost approximation. This module builds a `LatencyWindow` that records
durations into a ring and computes a percentile by snapshotting, sorting the copy,
and reading the nearest-rank quantile.

Self-contained: its own module, an inlined duration ring, the window, a demo, and
tests against a reference computation.

## What you'll build

```text
latencywindow/             independent module: example.com/latencywindow
  go.mod                   go 1.24
  latencywindow.go         ring + LatencyWindow (Record, Percentile, Len)
  cmd/
    demo/
      main.go              record 1..100ms, print p50/p95/p99
  latencywindow_test.go    reference percentiles, window roll, empty-window sentinel
```

Files: `latencywindow.go`, `cmd/demo/main.go`, `latencywindow_test.go`.
Implement: `LatencyWindow` over a ring of `time.Duration`; `Record(d)`, `Percentile(q float64) (time.Duration, error)`, `Len`.
Test: known distribution 1..100ms matches a reference nearest-rank computation on the last-K window; older-than-window samples excluded once it rolls; empty window returns a sentinel without panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Nearest-rank percentile, and why we copy before sorting

`Percentile(q)` uses the nearest-rank method: for `n` samples sorted ascending, the
rank is `ceil(q * n)`, and the value is the element at index `rank - 1` (clamped to
`[0, n-1]`). For `n = 100` values `1..100` ms: `q = 0.5` gives rank 50, index 49,
value 50 ms; `q = 0.95` gives rank 95, value 95 ms; `q = 0.99` gives rank 99, value
99 ms. Nearest-rank is exact and needs no interpolation, which makes it easy to test
against a reference.

The computation must not sort the ring's storage in place — that would scramble the
FIFO order the window depends on. So `Percentile` takes a `Snapshot` (a fresh copy),
sorts the copy with `slices.Sort`, and reads the quantile from it. In a concurrent
version the rule from the concepts file applies: copy under the lock, release, then
sort — never hold the lock across the O(k log k) sort. This module keeps the window
single-goroutine to focus on the statistics; wrapping it in the `SafeRing` mutex
pattern from Exercise 4 is the concurrency step.

### The count-based caveat is the senior point

This window is *count-based*: it holds the last K durations, whatever time span
that covers. `Percentile(0.99)` is "p99 over the last K requests," not "p99 over the
last minute." Under steady traffic those are close; under a burst the K samples span
a few milliseconds, and under idle they span minutes. Reporting a time-based SLO from
this structure is the mistake the concepts file warns about. The ring is the right
tool when "the most recent K observations" is genuinely the population you want to
summarize; for a true time window you bucket by time, and for a smooth long-horizon
estimate you use a decayed reservoir.

### Empty window returns a sentinel, never panics

`Percentile` on an empty window cannot compute a rank, so it returns `0` and a
sentinel error `ErrNoData` rather than indexing an empty slice and panicking. A
metrics scrape that hits a just-started process must degrade gracefully, not crash
the handler.

Create `latencywindow.go`:

```go
package latencywindow

import (
	"errors"
	"math"
	"slices"
	"time"
)

// ErrNoData is returned by Percentile when the window holds no samples.
var ErrNoData = errors.New("latencywindow: no samples")

type ring struct {
	data []time.Duration
	head int
	tail int
	size int
}

func newRing(capacity int) *ring {
	if capacity <= 0 {
		capacity = 1
	}
	return &ring{data: make([]time.Duration, capacity)}
}

func (r *ring) push(v time.Duration) {
	r.data[r.head] = v
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

func (r *ring) snapshot() []time.Duration {
	out := make([]time.Duration, r.size)
	for i := range r.size {
		out[i] = r.data[(r.tail+i)%len(r.data)]
	}
	return out
}

// LatencyWindow tracks percentiles over the most recent K request durations.
// The window is count-based: it holds the last K samples, not a fixed time span.
type LatencyWindow struct {
	r *ring
}

// NewLatencyWindow returns a window over the most recent k durations.
func NewLatencyWindow(k int) *LatencyWindow {
	return &LatencyWindow{r: newRing(k)}
}

// Record adds a duration, evicting the oldest when the window is full.
func (w *LatencyWindow) Record(d time.Duration) { w.r.push(d) }

// Len reports how many samples are currently in the window.
func (w *LatencyWindow) Len() int { return w.r.size }

// Percentile returns the nearest-rank quantile q in [0,1] over the current
// window, or ErrNoData if the window is empty. It sorts a copy, never the
// live storage.
func (w *LatencyWindow) Percentile(q float64) (time.Duration, error) {
	samples := w.r.snapshot()
	n := len(samples)
	if n == 0 {
		return 0, ErrNoData
	}
	slices.Sort(samples)
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	rank := int(math.Ceil(q * float64(n)))
	idx := rank - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return samples[idx], nil
}
```

### The runnable demo

The demo records 1..100 ms in order into a window of size 100, then prints the three
percentiles.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/latencywindow"
)

func main() {
	w := latencywindow.NewLatencyWindow(100)
	for i := 1; i <= 100; i++ {
		w.Record(time.Duration(i) * time.Millisecond)
	}
	for _, q := range []float64{0.50, 0.95, 0.99} {
		p, _ := w.Percentile(q)
		fmt.Printf("p%02.0f = %v\n", q*100, p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
p50 = 50ms
p95 = 95ms
p99 = 99ms
```

### Tests

The reference computation mirrors the nearest-rank formula independently, so the
test is not merely restating the implementation: it builds an expected value from
the raw sorted samples and compares. One test feeds a shuffled 1..100 and checks the
three percentiles; one records more than K samples and asserts the old ones fell out
of the window (the p50 shifts because the population changed); one checks the empty
window returns `ErrNoData`.

Create `latencywindow_test.go`:

```go
package latencywindow

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"
)

// refPercentile is an independent nearest-rank reference over raw samples.
func refPercentile(samples []time.Duration, q float64) time.Duration {
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := int(math.Ceil(q * float64(len(s))))
	idx := rank - 1
	if idx < 0 {
		idx = 0
	}
	return s[idx]
}

func TestPercentilesMatchReference(t *testing.T) {
	t.Parallel()
	// A known distribution 1..100ms, recorded in a rotated order so the code is
	// not accidentally tested only on already-sorted input.
	all := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		all = append(all, time.Duration(i)*time.Millisecond)
	}
	w := NewLatencyWindow(100)
	for _, i := range []int{50, 1, 99, 100, 2, 75, 25, 88, 63, 37} {
		w.Record(time.Duration(i) * time.Millisecond)
	}
	// Then fill the rest to cover the full 1..100 population exactly once.
	recorded := map[int]bool{50: true, 1: true, 99: true, 100: true, 2: true, 75: true, 25: true, 88: true, 63: true, 37: true}
	for i := 1; i <= 100; i++ {
		if !recorded[i] {
			w.Record(time.Duration(i) * time.Millisecond)
		}
	}
	for _, q := range []float64{0.5, 0.95, 0.99} {
		got, err := w.Percentile(q)
		if err != nil {
			t.Fatalf("Percentile(%v): %v", q, err)
		}
		want := refPercentile(all, q)
		if got != want {
			t.Fatalf("Percentile(%v) = %v, want %v", q, got, want)
		}
	}
}

func TestWindowRollsAndDropsOldSamples(t *testing.T) {
	t.Parallel()
	w := NewLatencyWindow(5)
	// Record 1..10ms; only the last 5 (6..10ms) remain in the window.
	for i := 1; i <= 10; i++ {
		w.Record(time.Duration(i) * time.Millisecond)
	}
	if w.Len() != 5 {
		t.Fatalf("Len = %d, want 5", w.Len())
	}
	// Nearest-rank p50 over {6,7,8,9,10}: rank=ceil(0.5*5)=3 -> index 2 -> 8ms.
	got, err := w.Percentile(0.5)
	if err != nil {
		t.Fatalf("Percentile: %v", err)
	}
	if got != 8*time.Millisecond {
		t.Fatalf("p50 over rolled window = %v, want 8ms (old samples not dropped)", got)
	}
	// The dropped samples (1..5ms) must not appear as the minimum: p0-ish is 6ms.
	min, _ := w.Percentile(0)
	if min != 6*time.Millisecond {
		t.Fatalf("window minimum = %v, want 6ms", min)
	}
}

func TestEmptyWindowReturnsSentinel(t *testing.T) {
	t.Parallel()
	w := NewLatencyWindow(10)
	_, err := w.Percentile(0.99)
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("empty Percentile err = %v, want ErrNoData", err)
	}
}
```

## Review

The tracker is correct when `Percentile(q)` equals the nearest-rank quantile of the
*current* window and an empty window returns `ErrNoData` instead of panicking.
`TestPercentilesMatchReference` compares against an independent formula, so it
catches an off-by-one in the rank math; `TestWindowRollsAndDropsOldSamples` proves
the window is genuinely sliding — the p50 moves to 8 ms only if 1..5 ms were evicted.
Two traps: sorting the ring's own storage in place (scrambles FIFO order — sort a
`Snapshot`), and reporting a count-based percentile as if it were time-based (the
window is K requests, not K seconds). If you make this concurrent, copy the samples
under the lock and sort after releasing, so an O(k log k) sort never stalls the
producers recording latencies.

## Resources

- [`slices` package](https://pkg.go.dev/slices) — `slices.Sort`, `slices.Clone`.
- [`math` package](https://pkg.go.dev/math) — `math.Ceil` for the nearest-rank index.
- [Nearest-rank percentile method](https://en.wikipedia.org/wiki/Percentile#The_nearest-rank_method) — the exact definition this module implements.

---

Back to [04-thread-safe-ring-mutex.md](04-thread-safe-ring-mutex.md) | Next: [06-byte-ring-io-reader-writer.md](06-byte-ring-io-reader-writer.md)
