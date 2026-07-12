# Exercise 31: Time Series Bucketing Aggregator — Group Events by Sliding/Tumbling Windows, Yield Buckets

**Nivel: Intermedio** — validacion rapida (un test corto).

A metrics pipeline that computes `sum`, `count`, `min`, and `max` per minute
cannot wait for the whole day's samples to arrive before it emits the first
minute's aggregate -- the dashboard consuming it needs each window as soon
as it closes. But "as soon as it closes" is exactly where the hard case
lives: network retries and clock skew mean a sample can arrive slightly
out of order, timestamped into a window that has technically already
shipped. This exercise builds a tumbling-window bucketer that closes and
yields a window the moment a later sample proves it is done, and explicitly
tracks (rather than silently corrupts) the data that arrives too late to
matter. This exercise is an independent module with its own `go mod init`.

## What you'll build

```text
tsbucket/                  independent module: example.com/time-series-bucketing-aggregator
  go.mod                    module example.com/time-series-bucketing-aggregator
  tsbucket.go               Sample, Bucket, Bucketizer, New, Bucketize, Dropped
  cmd/
    demo/
      main.go               runnable demo: 3 one-minute windows plus one late sample
  tsbucket_test.go           in-order aggregation, late-arrival drop, EOF flush, early-stop, panic
```

Implement: `New(window time.Duration) *Bucketizer`, `(*Bucketizer) Bucketize(samples iter.Seq[Sample]) iter.Seq[Bucket]` yielding each window's `Bucket{Start, End, Count, Sum, Min, Max}` as soon as it closes, and `(*Bucketizer) Dropped() int` for the count of late-arriving samples excluded after the fact.
Test: in-order samples aggregate into the correct per-window sum/min/max/count; a sample timestamped into an already-closed window is excluded from that bucket and counted in `Dropped`, not silently folded in; the final open window is flushed at end-of-stream even with no following sample to close it; a consumer break stops the source; `New` panics on a non-positive window.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

`Bucketize` keeps exactly one open `Bucket` at a time and compares each
incoming sample's truncated window against it: equal folds into the open
bucket, later closes it (yield) and opens a new one, earlier means the
sample's window has already shipped. That last case is the one production
detail that is easy to get wrong: it would be tempting to reopen the closed
window and re-yield a corrected `Bucket`, but the consumer may already have
persisted or alerted on the original aggregate, and un-sending a metric is
not a thing a `for...range` loop can do. So a late sample is counted in
`Dropped` and otherwise discarded -- an explicit, measurable data-quality
signal instead of a bucket whose sum silently drifts depending on how out
of order the stream happened to be on a given run. `Dropped` is read *after*
the range loop finishes, the same "terminal state read after the loop"
idiom `database/sql`'s `rows.Err()` uses, because the final count is only
known once no more samples can possibly arrive.

Create `tsbucket.go`:

```go
package tsbucket

import (
	"iter"
	"time"
)

// Sample is one (timestamp, value) metric observation.
type Sample struct {
	Timestamp time.Time
	Value     float64
}

// Bucket is a closed tumbling window's aggregate: how many samples fell
// into [Start, End), and their sum, min, and max.
type Bucket struct {
	Start, End    time.Time
	Count         int
	Sum, Min, Max float64
}

// Avg returns the bucket's mean value. Percentiles like p99 are
// deliberately not tracked here: an exact percentile needs every sample
// retained (or an approximating sketch), which is a materially different
// memory budget than the four running numbers this bucket carries: a
// production aggregator adds a t-digest or similar sketch only once a
// percentile is actually required, rather than paying for it by default.
func (b Bucket) Avg() float64 {
	if b.Count == 0 {
		return 0
	}
	return b.Sum / float64(b.Count)
}

// Bucketizer groups samples into fixed-size tumbling windows.
type Bucketizer struct {
	window  time.Duration
	dropped int
}

// New creates a Bucketizer with the given tumbling window size. It panics
// if window is not positive: a zero or negative window cannot partition
// time into anything meaningful.
func New(window time.Duration) *Bucketizer {
	if window <= 0 {
		panic("tsbucket: window must be > 0")
	}
	return &Bucketizer{window: window}
}

// Dropped reports how many samples arrived late -- timestamped into a
// window that had already been closed and yielded -- across every call to
// Bucketize on this Bucketizer. It must be read after the range loop over
// Bucketize's result has finished, the same "read the terminal count after
// the loop" idiom database/sql's rows.Err() uses, because the count is only
// final once no more samples can arrive.
func (b *Bucketizer) Dropped() int { return b.dropped }

// Bucketize groups samples into tumbling windows of size window and yields
// each window's Bucket once it closes. A window closes -- and is yielded --
// the moment a sample arrives whose timestamp falls in a strictly later
// window; this is a single-pass, single-open-window algorithm, so it
// assumes the stream is close to arrival order. A sample whose timestamp
// falls into a window that has already closed and been yielded cannot be
// retroactively merged into an already-emitted Bucket -- the consumer may
// already have acted on it -- so it is counted in Dropped and otherwise
// ignored, which is the realistic trade-off every tumbling-window
// aggregator with an unbounded allowed-lateness makes: a bounded amount of
// out-of-order slack is tolerated within a window, but data that arrives
// after its window has already shipped is too late to matter without
// reopening a decision that was already made.
func (b *Bucketizer) Bucketize(samples iter.Seq[Sample]) iter.Seq[Bucket] {
	return func(yield func(Bucket) bool) {
		var open Bucket
		var openStart time.Time
		hasOpen := false

		for s := range samples {
			ws := s.Timestamp.Truncate(b.window)
			switch {
			case !hasOpen:
				open = Bucket{Start: ws, End: ws.Add(b.window), Count: 1, Sum: s.Value, Min: s.Value, Max: s.Value}
				openStart = ws
				hasOpen = true
			case ws.Equal(openStart):
				open.Count++
				open.Sum += s.Value
				if s.Value < open.Min {
					open.Min = s.Value
				}
				if s.Value > open.Max {
					open.Max = s.Value
				}
			case ws.After(openStart):
				if !yield(open) {
					return
				}
				open = Bucket{Start: ws, End: ws.Add(b.window), Count: 1, Sum: s.Value, Min: s.Value, Max: s.Value}
				openStart = ws
			default: // ws.Before(openStart): a late arrival for an already-closed window
				b.dropped++
			}
		}

		if hasOpen {
			if !yield(open) {
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
	"time"

	"example.com/time-series-bucketing-aggregator"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Arrival order, not timestamp order: note the late sample at index 4,
	// which is timestamped into window 0 but arrives after window 0 has
	// already closed.
	raw := []tsbucket.Sample{
		{Timestamp: base, Value: 10},
		{Timestamp: base.Add(10 * time.Second), Value: 20},
		{Timestamp: base.Add(65 * time.Second), Value: 5},
		{Timestamp: base.Add(70 * time.Second), Value: 7},
		{Timestamp: base.Add(5 * time.Second), Value: 999}, // late for window 0
		{Timestamp: base.Add(130 * time.Second), Value: 3},
		{Timestamp: base.Add(140 * time.Second), Value: 4},
	}
	src := func(yield func(tsbucket.Sample) bool) {
		for _, s := range raw {
			if !yield(s) {
				return
			}
		}
	}

	b := tsbucket.New(time.Minute)
	for bucket := range b.Bucketize(src) {
		fmt.Printf("[%s,%s) count=%d sum=%.0f min=%.0f max=%.0f avg=%.1f\n",
			bucket.Start.Format("15:04:05"), bucket.End.Format("15:04:05"),
			bucket.Count, bucket.Sum, bucket.Min, bucket.Max, bucket.Avg())
	}
	fmt.Printf("dropped late samples: %d\n", b.Dropped())
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
[00:00:00,00:01:00) count=2 sum=30 min=10 max=20 avg=15.0
[00:01:00,00:02:00) count=2 sum=12 min=5 max=7 avg=6.0
[00:02:00,00:03:00) count=2 sum=7 min=3 max=4 avg=3.5
dropped late samples: 1
```

The `Value: 999` sample is timestamped 5 seconds into window 0, but it
arrives fifth, after window 0 has already closed on the strength of the
65-second sample -- it is excluded from the `sum=30` already reported for
that window and shows up only in the final `dropped late samples: 1` line.

### Tests

Create `tsbucket_test.go`:

```go
package tsbucket

import (
	"testing"
	"time"
)

func sampleSeq(samples []Sample) func(yield func(Sample) bool) {
	return func(yield func(Sample) bool) {
		for _, s := range samples {
			if !yield(s) {
				return
			}
		}
	}
}

func TestBucketizeAggregatesInOrderArrival(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	samples := []Sample{
		{Timestamp: base, Value: 10},
		{Timestamp: base.Add(10 * time.Second), Value: 20},
		{Timestamp: base.Add(65 * time.Second), Value: 5},
	}

	b := New(time.Minute)
	var got []Bucket
	for bucket := range b.Bucketize(sampleSeq(samples)) {
		got = append(got, bucket)
	}

	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2", len(got))
	}
	if got[0].Count != 2 || got[0].Sum != 30 || got[0].Min != 10 || got[0].Max != 20 {
		t.Fatalf("bucket[0] = %+v, want count=2 sum=30 min=10 max=20", got[0])
	}
	if got[1].Count != 1 || got[1].Sum != 5 {
		t.Fatalf("bucket[1] = %+v, want count=1 sum=5", got[1])
	}
	if b.Dropped() != 0 {
		t.Fatalf("Dropped() = %d, want 0", b.Dropped())
	}
}

func TestBucketizeDropsLateArrivalAndCountsIt(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	samples := []Sample{
		{Timestamp: base, Value: 10},
		{Timestamp: base.Add(65 * time.Second), Value: 5},  // closes window 0
		{Timestamp: base.Add(5 * time.Second), Value: 999}, // late for window 0
	}

	b := New(time.Minute)
	var got []Bucket
	for bucket := range b.Bucketize(sampleSeq(samples)) {
		got = append(got, bucket)
	}

	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2", len(got))
	}
	// The late sample must not have been folded into the already-closed
	// window 0 bucket: its sum stays 10, not 1009.
	if got[0].Sum != 10 || got[0].Count != 1 {
		t.Fatalf("bucket[0] = %+v, want the late sample excluded (sum=10 count=1)", got[0])
	}
	if b.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", b.Dropped())
	}
}

func TestBucketizeFlushesFinalOpenBucketAtEOF(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	samples := []Sample{{Timestamp: base, Value: 42}}

	b := New(time.Minute)
	var got []Bucket
	for bucket := range b.Bucketize(sampleSeq(samples)) {
		got = append(got, bucket)
	}
	if len(got) != 1 || got[0].Count != 1 || got[0].Sum != 42 {
		t.Fatalf("got %+v, want a single flushed bucket with the one sample", got)
	}
}

func TestBucketizeStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	src := func(yield func(Sample) bool) {
		for i := 0; i < 100; i++ {
			calls++
			s := Sample{Timestamp: base.Add(time.Duration(i) * time.Minute), Value: float64(i)}
			if !yield(s) {
				return
			}
		}
	}

	b := New(time.Minute)
	seen := 0
	for range b.Bucketize(src) {
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Fatalf("seen = %d, want 3", seen)
	}
	// Each sample lands in its own window (one per minute), so closing 3
	// buckets requires having observed the 4th sample that closed the 3rd.
	if calls != 4 {
		t.Fatalf("calls = %d, want 4: the source must stop, not run to completion", calls)
	}
}

func TestNewPanicsOnNonPositiveWindow(t *testing.T) {
	t.Parallel()

	cases := []time.Duration{0, -time.Second}
	for _, window := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for window=%v", window)
				}
			}()
			New(window)
		}()
	}
}
```

## Review

The property this design protects is that a `Bucket`, once yielded, is
final -- nothing later in the stream can revise it. That is precisely why a
late sample increments `Dropped` instead of triggering a "corrected" second
emission of the same window: a consumer that has already written the first
`Bucket` to a time-series database or fired an alert off it has no contract
with this iterator promising a correction will ever come, so silently
mutating history would be worse than an honest, countable drop. The common
mistake when building a bucketer like this is keeping a `map[windowStart]*Bucket`
and only flushing at the very end -- it looks simpler, but it means no
aggregate is available until the entire stream has been consumed, which
defeats the reason for streaming in the first place: a live dashboard needs
each window the moment it closes, not all of them at once after the last
sample.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [`time.Time.Truncate`](https://pkg.go.dev/time#Time.Truncate)
- [Prometheus: histograms and summaries](https://prometheus.io/docs/practices/histograms/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-gossip-protocol-peer-health-check.md](30-gossip-protocol-peer-health-check.md) | Next: [32-request-coalescing-singleflight.md](32-request-coalescing-singleflight.md)
