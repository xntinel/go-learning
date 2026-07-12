# Exercise 1: Metrics Aggregator — range over slice, map, and channel

A metrics aggregator is the canonical place where three range shapes meet in one
type: it accepts samples through a variadic slice, drains them from a channel, and
snapshots its internal map into a sorted slice. This module builds a
concurrency-safe `Sample` -> `Summary` aggregator and tests it under `-race`, so
each range shape is exercised the way production code hits it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
metricsagg/                 independent module: example.com/metricsagg
  go.mod                    go 1.24
  agg/
    agg.go                  Sample, Summary, Aggregator; Record/Consume/Snapshot/Reset; TotalCount/MaxCount
    agg_test.go             table + channel-drain + concurrency (-race) + sorted + empty-label tests
  cmd/
    demo/
      main.go               runnable demo: record samples, print sorted snapshot
```

- Files: `agg/agg.go`, `cmd/demo/main.go`, `agg/agg_test.go`.
- Implement: `Aggregator` with `Record(...Sample)` (ranges a variadic slice), `Consume(<-chan Sample)` (ranges a channel until close), `Snapshot() []Summary` (ranges the map, then sorts), plus `TotalCount`/`MaxCount` over summary slices.
- Test: record+snapshot correctness, channel-drain termination on close, 8x100 concurrent records under `-race`, sorted order, and empty-label ignored.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/01-metrics-aggregator-range-forms/agg go-solutions/03-control-flow/05-range-over-collections/01-metrics-aggregator-range-forms/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/01-metrics-aggregator-range-forms
go mod edit -go=1.24
```

### Why each range shape fits its step

`Record(samples ...Sample)` receives a variadic slice, so ranging it with
`for _, s := range samples` is the natural read: the caller may pass zero, one, or
many samples, and the index is irrelevant so it is blanked. `Consume(in <-chan Sample)`
is the channel shape — `for s := range in` — chosen precisely because the producer
size is unknown and the loop must end exactly when the producer closes `in`, not a
moment sooner. `Snapshot()` ranges the internal map to copy every summary into a
slice, then `sort.Slice` imposes a stable order; the map range order is randomized,
so without the sort the snapshot would be a different sequence every call and every
test assertion would flake.

The whole type is guarded by a `sync.Mutex` because `Record` mutates the shared map
and `Snapshot` reads it, and real callers fan samples in from multiple goroutines.
`Reset` uses the `clear` builtin, which empties a map in place without reallocating.
Empty labels are skipped in `Record` so a malformed sample never creates an
unlabeled bucket — a small guard that the test pins so a refactor cannot silently
start tracking it.

`MaxCount` ranges `summaries[1:]` rather than the whole slice, seeding `best` with
element 0 so it never compares the first element against itself; this is the idiom
for "reduce a non-empty slice" and it returns a zero result for the empty case
without indexing out of range.

Create `agg/agg.go`:

```go
package agg

import (
	"sort"
	"sync"
	"time"
)

// Sample is a single labeled measurement at an instant.
type Sample struct {
	Label string
	Value float64
	At    time.Time
}

// Summary is the running aggregate for one label.
type Summary struct {
	Label string
	Count int
	Sum   float64
	Min   float64
	Max   float64
	Mean  float64
}

// Aggregator accumulates Samples into per-label Summaries. It is safe for
// concurrent use by multiple producers.
type Aggregator struct {
	mu        sync.Mutex
	summaries map[string]Summary
}

func New() *Aggregator {
	return &Aggregator{summaries: make(map[string]Summary)}
}

// Record folds zero or more samples into the running summaries. Samples with an
// empty label are ignored. It ranges the variadic slice.
func (a *Aggregator) Record(samples ...Sample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range samples {
		if s.Label == "" {
			continue
		}
		summary, ok := a.summaries[s.Label]
		if !ok {
			summary = Summary{Label: s.Label, Min: s.Value, Max: s.Value}
		}
		summary.Count++
		summary.Sum += s.Value
		if s.Value < summary.Min {
			summary.Min = s.Value
		}
		if s.Value > summary.Max {
			summary.Max = s.Value
		}
		summary.Mean = summary.Sum / float64(summary.Count)
		a.summaries[s.Label] = summary
	}
}

// Consume records every sample from in and returns when in is closed. It ranges
// the channel; the producer owns closing it.
func (a *Aggregator) Consume(in <-chan Sample) {
	for s := range in {
		a.Record(s)
	}
}

// Snapshot returns the summaries sorted by label. It ranges the internal map into
// a slice, then sorts to impose a stable order the map range does not provide.
func (a *Aggregator) Snapshot() []Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Summary, 0, len(a.summaries))
	for _, s := range a.summaries {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// Reset drops all summaries, reusing the map via the clear builtin.
func (a *Aggregator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.summaries)
}

// TotalCount sums Count across a summary slice.
func TotalCount(summaries []Summary) int {
	total := 0
	for _, s := range summaries {
		total += s.Count
	}
	return total
}

// MaxCount returns the label with the most samples and that count, or ("",0) for
// an empty slice. It ranges summaries[1:] so element 0 seeds the max.
func MaxCount(summaries []Summary) (string, int) {
	if len(summaries) == 0 {
		return "", 0
	}
	best := summaries[0]
	for _, s := range summaries[1:] {
		if s.Count > best.Count {
			best = s
		}
	}
	return best.Label, best.Count
}
```

### The runnable demo

The demo records a handful of HTTP samples across two labels and prints the sorted
snapshot, so you can see the map-into-sorted-slice step produce stable output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/metricsagg/agg"
)

func main() {
	a := agg.New()
	a.Record(
		agg.Sample{Label: "http.requests", Value: 12, At: time.Unix(0, 0).UTC()},
		agg.Sample{Label: "http.requests", Value: 4, At: time.Unix(1, 0).UTC()},
		agg.Sample{Label: "http.requests", Value: 17, At: time.Unix(2, 0).UTC()},
		agg.Sample{Label: "http.errors", Value: 1, At: time.Unix(3, 0).UTC()},
		agg.Sample{Label: "http.errors", Value: 0, At: time.Unix(4, 0).UTC()},
	)

	for _, s := range a.Snapshot() {
		fmt.Printf("%-15s count=%d min=%v max=%v mean=%.2f\n", s.Label, s.Count, s.Min, s.Max, s.Mean)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
http.errors     count=2 min=0 max=1 mean=0.50
http.requests   count=3 min=4 max=17 mean=11.00
```

### Tests

The table test proves the slice-range record path computes count/sum/min/max/mean
correctly and that the snapshot is sorted. The channel test proves `Consume` ends
on close. The concurrency test drives 8 producers of 100 records each under `-race`
and asserts the exact final count, which only holds if the mutex actually serializes
the map writes. The empty-label test pins that a blank label is ignored.

Create `agg/agg_test.go`:

```go
package agg

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func TestRecordAndSnapshot(t *testing.T) {
	t.Parallel()
	a := New()
	a.Record(
		Sample{Label: "http.requests", Value: 12, At: time.Unix(0, 0).UTC()},
		Sample{Label: "http.requests", Value: 4, At: time.Unix(1, 0).UTC()},
		Sample{Label: "http.errors", Value: 1, At: time.Unix(2, 0).UTC()},
	)

	got := a.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(got))
	}
	if got[0].Label != "http.errors" {
		t.Fatalf("first label = %q, want http.errors (sorted)", got[0].Label)
	}
	if got[0].Count != 1 || got[0].Sum != 1 || got[0].Min != 1 || got[0].Max != 1 {
		t.Fatalf("http.errors = %+v", got[0])
	}
	if got[1].Count != 2 || got[1].Sum != 16 || got[1].Min != 4 || got[1].Max != 12 {
		t.Fatalf("http.requests = %+v", got[1])
	}
	if math.Abs(got[1].Mean-8.0) > 1e-9 {
		t.Fatalf("http.requests mean = %v, want 8", got[1].Mean)
	}
}

func TestConsumeEndsOnClose(t *testing.T) {
	t.Parallel()
	a := New()
	in := make(chan Sample, 4)
	in <- Sample{Label: "latency.ms", Value: 5}
	in <- Sample{Label: "latency.ms", Value: 9}
	in <- Sample{Label: "latency.ms", Value: 3}
	close(in)

	a.Consume(in) // returns only because in is closed
	got := a.Snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot has %d entries, want 1", len(got))
	}
	if got[0].Count != 3 || got[0].Min != 3 || got[0].Max != 9 || got[0].Sum != 17 {
		t.Fatalf("latency = %+v", got[0])
	}
}

func TestConcurrentRecordExactCount(t *testing.T) {
	t.Parallel()
	a := New()
	const producers = 8
	const perProducer = 100
	var wg sync.WaitGroup
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perProducer {
				a.Record(Sample{Label: "shared", Value: float64(i)})
			}
		}()
	}
	wg.Wait()

	got := a.Snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot = %d entries, want 1", len(got))
	}
	if got[0].Count != producers*perProducer {
		t.Fatalf("count = %d, want %d", got[0].Count, producers*perProducer)
	}
}

func TestSnapshotIsSorted(t *testing.T) {
	t.Parallel()
	a := New()
	a.Record(
		Sample{Label: "zeta", Value: 1},
		Sample{Label: "alpha", Value: 1},
		Sample{Label: "mu", Value: 1},
	)
	got := a.Snapshot()
	if got[0].Label != "alpha" || got[1].Label != "mu" || got[2].Label != "zeta" {
		t.Fatalf("snapshot not sorted: %+v", got)
	}
}

func TestEmptyLabelIgnored(t *testing.T) {
	t.Parallel()
	a := New()
	a.Record(Sample{Label: "", Value: 1}, Sample{Label: "", Value: 2})
	if got := a.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot = %+v, want empty (blank labels ignored)", got)
	}
}

func TestTotalCountAndMaxCount(t *testing.T) {
	t.Parallel()
	summaries := []Summary{
		{Label: "a", Count: 1},
		{Label: "b", Count: 5},
		{Label: "c", Count: 3},
	}
	if got := TotalCount(summaries); got != 9 {
		t.Fatalf("TotalCount = %d, want 9", got)
	}
	if label, count := MaxCount(summaries); label != "b" || count != 5 {
		t.Fatalf("MaxCount = (%s, %d), want (b, 5)", label, count)
	}
	if label, count := MaxCount(nil); label != "" || count != 0 {
		t.Fatalf("MaxCount(nil) = (%q, %d), want (\"\", 0)", label, count)
	}
}

func Example() {
	a := New()
	a.Record(Sample{Label: "cache.hits", Value: 3}, Sample{Label: "cache.hits", Value: 7})
	s := a.Snapshot()[0]
	fmt.Printf("%s count=%d mean=%.1f\n", s.Label, s.Count, s.Mean)
	// Output: cache.hits count=2 mean=5.0
}
```

The `Example` needs `fmt`; add it to the import block above if you split files —
here it lives in the same test file, so include `"fmt"` in the imports.

## Review

The aggregator is correct when each range shape does its job: `Record` folds the
variadic slice with running min/max/mean, `Consume` terminates exactly on channel
close, and `Snapshot` produces the same sorted sequence every call regardless of
map order. The most common way to break this is to assert on the raw map order
(flaky) or to forget the mutex and watch `TestConcurrentRecordExactCount` fail its
exact-count assertion under `-race`. Keep expiry of stale labels out of scope here;
this type only accumulates. Run `go test -race` to confirm the mutex guards the
map under the 8-producer load.

Note the `Example` uses `fmt`, so the test file imports it alongside `math`,
`sync`, `testing`, and `time`.

## Resources

- [Go Specification: For statements (range clause)](https://go.dev/ref/spec#For_range)
- [sort.Slice](https://pkg.go.dev/sort#Slice)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [The clear builtin](https://pkg.go.dev/builtin#clear)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cursor-pagination-iterator.md](02-cursor-pagination-iterator.md)
