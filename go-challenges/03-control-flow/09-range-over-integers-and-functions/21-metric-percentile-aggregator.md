# Exercise 21: Percentile Aggregator — Streaming Latency Bucketing with Memory-Bounded Rollup

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A telemetry pipeline computing p50/p95/p99 latency for a long-lived service
cannot buffer every sample it has ever seen -- that is an unbounded memory
leak dressed up as an analytics feature. The standard fix is to bucket
samples into fixed-size windows and compute percentiles per window, trading
some cross-window accuracy for a hard memory ceiling. Expressing that as an
`iter.Seq[time.Duration]` to `iter.Seq[Snapshot]` combinator keeps the buffer
itself scoped to the iterator's closure, capped at exactly one window's
worth of samples at any moment. This exercise is an independent module with
its own `go mod init`.

## What you'll build

```text
percentiles/               independent module: example.com/metric-percentile-aggregator
  go.mod                   module example.com/metric-percentile-aggregator
  percentiles.go           Snapshot, Aggregate
  cmd/
    demo/
      main.go              runnable demo: 7 samples bucketed into windows of 4
  percentiles_test.go      full+partial windows, exact multiple, early-stop, panic
```

Implement: `Aggregate(windowSize int, src iter.Seq[time.Duration]) iter.Seq[Snapshot]` yielding one `Snapshot{Count, P50, P95, P99}` per full window of `windowSize` samples, flushing a final partial window if any samples remain when `src` is exhausted; panics if `windowSize < 1`.
Test: 7 samples with `windowSize=4` yield a full window of 4 and a partial window of 3, each with correct percentiles; an exact multiple has no partial flush; a consumer break after the first window stops there; `windowSize=0` panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metric-percentile-aggregator/cmd/demo
cd ~/go-exercises/metric-percentile-aggregator
go mod init example.com/metric-percentile-aggregator
go mod edit -go=1.24
```

The buffer is allocated once, up front, with capacity `windowSize`, and
truncated back to length zero (`buf[:0]`) after every flush instead of being
reallocated -- reusing the same backing array is what keeps this from
allocating on every window in a stream that runs for the process's entire
lifetime. Computing percentiles requires a sorted view of the window, so
`snapshot` copies the buffer before sorting it in place; sorting the shared
`buf` directly would corrupt the next window's accumulation order. The
percentile itself uses the simple nearest-rank method -- `sorted[int(p *
float64(len(sorted)-1))]` -- which is exact and dependency-free for the
purposes of this exercise; production systems reaching for
sub-percent accuracy over huge datasets instead use a sketch like t-digest
or HDRHistogram, but the windowing discipline (bound memory, flush and
reset) is identical either way.

Create `percentiles.go`:

```go
package percentiles

import (
	"iter"
	"sort"
	"time"
)

// Snapshot holds the p50/p95/p99 latencies computed over one window's worth
// of samples.
type Snapshot struct {
	Count         int
	P50, P95, P99 time.Duration
}

// Aggregate buckets src into consecutive windows of exactly windowSize
// samples and yields one Snapshot per full window; if src is exhausted with
// a partial window still buffered, that final short window is flushed once
// before the sequence ends. Capping the buffer at windowSize instead of
// accumulating every sample ever observed is what keeps memory bounded in a
// telemetry stream that may run for the process's entire lifetime: only one
// window's worth of durations is ever held at a time, and the buffer is
// reused (truncated to length zero, not reallocated) between windows.
func Aggregate(windowSize int, src iter.Seq[time.Duration]) iter.Seq[Snapshot] {
	if windowSize < 1 {
		panic("percentiles: windowSize must be >= 1")
	}
	return func(yield func(Snapshot) bool) {
		buf := make([]time.Duration, 0, windowSize)
		for d := range src {
			buf = append(buf, d)
			if len(buf) == windowSize {
				if !yield(snapshot(buf)) {
					return
				}
				buf = buf[:0]
			}
		}
		if len(buf) > 0 {
			yield(snapshot(buf))
		}
	}
}

func snapshot(buf []time.Duration) Snapshot {
	sorted := make([]time.Duration, len(buf))
	copy(sorted, buf)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return Snapshot{
		Count: len(sorted),
		P50:   percentile(sorted, 0.50),
		P95:   percentile(sorted, 0.95),
		P99:   percentile(sorted, 0.99),
	}
}

// percentile returns the nearest-rank value at fraction p of a sorted,
// non-empty slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"
	"time"

	"example.com/metric-percentile-aggregator"
)

func main() {
	samples := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond,
		50 * time.Millisecond, 60 * time.Millisecond, 70 * time.Millisecond,
	}

	n := 0
	for s := range percentiles.Aggregate(4, slices.Values(samples)) {
		fmt.Printf("window %d: count=%d p50=%v p95=%v p99=%v\n", n, s.Count, s.P50, s.P95, s.P99)
		n++
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
window 0: count=4 p50=20ms p95=30ms p99=30ms
window 1: count=3 p50=60ms p95=60ms p99=60ms
```

The first four samples `[10,20,30,40]ms` fill the window and flush; nearest
rank at `p=0.50` over 4 sorted values indexes `int(0.5*3)=1`, giving `20ms`,
and both `p=0.95` and `p=0.99` index `int(2.85)=2` and `int(2.97)=2`, giving
`30ms` for both. The remaining three samples `[50,60,70]ms` are flushed as a
partial window once the source is exhausted, with all three percentiles
landing on `60ms` because the nearest-rank index for a 3-element window is
`1` at every one of these fractions.

### Tests

Create `percentiles_test.go`:

```go
package percentiles

import (
	"slices"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestAggregateFlushesFullAndPartialWindows(t *testing.T) {
	t.Parallel()

	samples := []time.Duration{
		ms(10), ms(20), ms(30), ms(40),
		ms(50), ms(60), ms(70),
	}

	var got []Snapshot
	for s := range Aggregate(4, slices.Values(samples)) {
		got = append(got, s)
	}

	want := []Snapshot{
		{Count: 4, P50: ms(20), P95: ms(30), P99: ms(30)},
		{Count: 3, P50: ms(60), P95: ms(60), P99: ms(60)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d snapshots, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("snapshot[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAggregateExactMultipleHasNoPartialFlush(t *testing.T) {
	t.Parallel()

	samples := []time.Duration{ms(1), ms(2), ms(3), ms(4)}
	count := 0
	for range Aggregate(2, slices.Values(samples)) {
		count++
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 full windows", count)
	}
}

func TestAggregateStopsEarly(t *testing.T) {
	t.Parallel()

	samples := []time.Duration{ms(1), ms(2), ms(3), ms(4), ms(5), ms(6)}
	count := 0
	for range Aggregate(2, slices.Values(samples)) {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestAggregatePanicsOnInvalidWindowSize(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for windowSize < 1")
		}
	}()
	Aggregate(0, slices.Values([]time.Duration{ms(1)}))
}
```

## Review

The property under test that matters most here is the partial-window
flush: it is tempting to only flush on `len(buf) == windowSize` and let a
trailing partial batch silently vanish when the source ends, which quietly
drops the last few samples of every scrape interval from the aggregate. The
`if len(buf) > 0` check after the loop is what recovers those samples. The
other detail worth internalizing is copying the buffer before sorting it:
`buf` is the same backing array reused across windows, so sorting it in
place would leave the next window's samples in whatever order the previous
sort left them in when `Aggregate` starts appending to `buf[:0]` again --
harmless for correctness of *this* window's percentile since it is
recomputed from scratch, but a `sort.Slice` on shared, growing state is the
kind of subtle aliasing bug that is worth eliminating on sight rather than
reasoning about.

## Resources

- [`sort.Slice` documentation](https://pkg.go.dev/sort#Slice)
- [Prometheus: histograms and summaries](https://prometheus.io/docs/practices/histograms/)
- [Wikipedia: percentile (nearest-rank method)](https://en.wikipedia.org/wiki/Percentile#The_nearest-rank_method)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-event-ordering-detector.md](20-event-ordering-detector.md) | Next: [22-pub-sub-fanout-broadcast.md](22-pub-sub-fanout-broadcast.md)
