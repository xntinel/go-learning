# Exercise 15: Batch Collector Reuse: clear() Versus Reassigning nil

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A client-side metrics collector in the style of the Prometheus Go client
library runs on a fixed interval: gather a tick's worth of counters and raw
samples, hand a snapshot to whatever exports it, then start the next tick.
Doing that every second for the lifetime of a process without allocating a
fresh map and a fresh slice on every single tick is not an optimization
nobody asked for -- at scrape-interval cadence across a fleet, a naive
"allocate new state, throw the old one away" reset is measurable GC pressure
for no reason. The idiomatic fix is to keep one scratch map and one scratch
slice as fields on the collector and empty them in place between ticks.

The trap is that "empty them in place" and "reassign the fields to a fresh
zero value" produce code that reads identically in review and diverges
sharply in production. `c.counts = nil; c.samples = nil` compiles, the next
line's `len(c.counts) == 0` assertion in a hasty test passes, and the bug
ships. The map is now genuinely nil, not merely empty, and the very next
`Record` call writes into it -- panicking with `assignment to entry in nil
map`, the exact failure mode this lesson's `00-concepts.md` warns about,
except deferred until the tick *after* the one that looked fine. The slice
half of the same mistake is quieter: no panic, but the capacity `NewCollector`
paid one allocation to reserve is thrown away, and the next tick's appends
pay to rebuild it from nothing.

The built-in `clear()`, added in Go 1.21, is the operation that actually does
what "reset between ticks" means. `clear(m)` on a map removes every entry and
leaves the map non-nil -- writable on the next call, exactly as before.
`s = s[:0]` on a slice sets its length back to zero while its capacity, and
the backing array underneath it, are untouched. Neither operation is a
special case to remember only for this one collector; they are the general
answer to "empty this without discarding it," and this module builds the one
place that answer has to be exactly right.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
scrape/                  module example.com/scrape
  go.mod                 go 1.24
  collector.go            Sample, Collector; NewCollector, Record, Reset, Counts, Samples
  collector_test.go       counter table, capacity edge cases, capacity-retention test,
                          nil-reset panic contrast, aliasing, ExampleCollector
```

- Files: `collector.go`, `collector_test.go`.
- Implement: `NewCollector(sampleCap int) (*Collector, error)` rejecting a negative capacity with `ErrInvalidCapacity`; `(*Collector).Record(name string, v float64)` accumulating a counter and appending a `Sample`; `(*Collector).Reset()` using `clear(c.counts)` and `c.samples = c.samples[:0]`; `(*Collector).Counts() map[string]float64` and `(*Collector).Samples() []Sample`, both returning clones.
- Test: the counter-accumulation table (empty, single record, repeated name); `NewCollector` rejecting negative capacity and accepting zero; `Reset` retaining the samples slice's capacity across a tick; `Reset` leaving the counts map writable with no panic; a `resetNaive` contrast proving reassignment to nil panics on the very next `Record`; `Counts`/`Samples` never aliasing collector state; and `ExampleCollector` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/15-batch-collector-clear-vs-nil-reset
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/15-batch-collector-clear-vs-nil-reset
go mod edit -go=1.24
```

### clear() empties in place; nil reassignment discards the allocation

`clear` is a built-in with two distinct behaviors depending on its argument's
type, and this module needs both. For a map, `clear(m)` removes every entry
-- `len(m)` becomes `0` -- but `m` itself is untouched: it is the same map
value, still non-nil, still holding whatever internal buckets it had before.
For a slice, `clear(s)` zeroes every element up to `len(s)` but does *not*
change the length or capacity; that is why this collector resets its samples
with the separate, more familiar idiom `s = s[:0]` instead, which does change
the length back to zero while leaving the backing array and its capacity
alone. Both operations share the same shape: the field keeps its identity,
only its contents change.

Reassigning to nil breaks that shape for entirely different reasons on each
field. On the map, `c.counts = nil` produces a value that is no longer a map
at all in any writable sense -- reads still work, but:

```go
func (c *Collector) resetTheWrongWay() {
    c.counts = nil    // "looks" cleared; len(c.counts) == 0 either way
    c.samples = nil
}
// next tick:
c.Record("requests_total", 1)   // panic: assignment to entry in nil map
```

The panic does not happen on the reset call. It happens one call later, on
whichever goroutine's turn it is to record the first metric of the next
tick -- which is exactly the kind of bug that survives a unit test asserting
`len(c.counts) == 0` right after `Reset` and only shows up under real load.
On the slice, `c.samples = nil` does not panic, but it silently discards the
capacity `NewCollector` allocated: the next `append` starts a fresh backing
array from zero, and the preallocation this module exists to teach is undone
one tick after it was paid for.

Create `collector.go`:

```go
// Package scrape implements a client-side metrics collector modeled on the
// Prometheus client library: one scratch map and one scratch slice reused
// across scrape ticks so a hot collection loop does not allocate on every
// tick.
//
// The package exists to get one detail right that a hand-rolled "reset
// between ticks" routinely gets wrong: clearing must empty the existing map
// and slice in place, never reassign either field to nil. Reassigning to nil
// looks like clearing in review, compiles, and passes a test that only checks
// length -- and then panics in production the moment the next tick writes to
// the now-nil map. See the package tests for a side-by-side demonstration.
package scrape

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrInvalidCapacity is returned by NewCollector when the requested sample
// capacity is negative.
var ErrInvalidCapacity = errors.New("scrape: sample capacity must not be negative")

// Sample is one recorded observation: the metric name and the value passed to
// Record.
type Sample struct {
	Name  string
	Value float64
}

// Collector accumulates counter values and raw samples across a scrape tick,
// then is reset in place for the next tick rather than replaced.
//
// A Collector is not safe for concurrent use. The caller must serialize
// Record, Reset, Counts, and Samples calls -- typically by running the whole
// scrape tick on a single goroutine, or by holding an external mutex around
// it.
type Collector struct {
	counts  map[string]float64
	samples []Sample
}

// NewCollector returns a Collector whose sample scratch slice is preallocated
// to sampleCap. sampleCap is a hint, not a hard limit: Record still appends
// past it, growing the slice like any other. It returns ErrInvalidCapacity if
// sampleCap is negative.
func NewCollector(sampleCap int) (*Collector, error) {
	if sampleCap < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, sampleCap)
	}
	return &Collector{
		counts:  make(map[string]float64),
		samples: make([]Sample, 0, sampleCap),
	}, nil
}

// Record adds v to the running counter named name and appends a Sample
// carrying the same name and value to the tick's sample buffer.
func (c *Collector) Record(name string, v float64) {
	c.counts[name] += v
	c.samples = append(c.samples, Sample{Name: name, Value: v})
}

// Reset empties the collector for the next scrape tick without discarding
// either scratch allocation.
//
// clear(c.counts) removes every entry but leaves the map itself non-nil, so
// the very next Record call can write into it safely. c.samples = c.samples[:0]
// reslices to length zero while keeping the slice's capacity, so the next
// tick's appends reuse the existing backing array instead of allocating a new
// one. Reassigning either field to nil instead of doing this is the mistake
// this package is built to make impossible: see the test file for what that
// costs.
func (c *Collector) Reset() {
	clear(c.counts)
	c.samples = c.samples[:0]
}

// Counts returns a clone of the current counter values. The returned map does
// not alias the collector's internal state; the caller may mutate it freely
// without affecting future Record or Reset calls.
func (c *Collector) Counts() map[string]float64 {
	return maps.Clone(c.counts)
}

// Samples returns a clone of the current tick's recorded samples, in the
// order Record was called. The returned slice does not alias the collector's
// internal state; the caller may mutate or retain it beyond the next Reset.
func (c *Collector) Samples() []Sample {
	return slices.Clone(c.samples)
}
```

### Using it

Construct one `Collector` per scrape loop with `NewCollector`, sized to
roughly how many samples one tick produces, and drive it from a single
goroutine: `Record` during collection, then `Counts`/`Samples` to pull a
snapshot for export, then `Reset` before the next tick begins. Because
`Counts` and `Samples` both return clones, the exporter can hold onto its
snapshot and serialize it at leisure while the collector moves on to the next
tick underneath it -- that is the aliasing contract documented on both
methods, and it is what makes it safe to call `Reset` immediately after
taking a snapshot rather than having to wait for the exporter to finish.

The module has no `main.go`; a metrics collector is a package another service
imports, not a program run on its own. Its executable demonstration is
`ExampleCollector`: `go test` runs it and compares its standard output
against the `// Output:` comment, so the usage shown here cannot drift away
from the code.

```go
func ExampleCollector() {
	c, err := NewCollector(4)
	if err != nil {
		panic(err)
	}

	c.Record("requests_total", 1)
	c.Record("requests_total", 1)
	c.Record("errors_total", 1)

	counts := c.Counts()
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s=%g\n", name, counts[name])
	}
	fmt.Println("samples:", len(c.Samples()), "cap:", cap(c.samples))

	before := cap(c.samples)
	c.Reset()
	c.Record("requests_total", 1)
	fmt.Println("after reset samples:", len(c.samples), "cap unchanged:", cap(c.samples) == before)

	// Output:
	// errors_total=1
	// requests_total=2
	// samples: 3 cap: 4
	// after reset samples: 1 cap unchanged: true
}
```

### Tests

`TestRecordAccumulatesCounts` is the ordinary table: no records, one record,
and the same name recorded twice to confirm it accumulates rather than
overwrites. `TestNewCollectorRejectsNegativeCapacity` and
`TestNewCollectorAcceptsZeroCapacity` are the constructor's edge cases -- a
zero-capacity collector must still work, just with its first append paying to
grow from nothing.

`TestResetRetainsCapacity` is the property this module is about: it records
enough samples to grow the slice past its initial capacity's obvious case,
captures `cap(c.samples)` before `Reset`, and asserts it is exactly the same
after `Reset`, and still the same after one more `Record`. No allocation
count is asserted, only the capacity value itself, which `clear`-based reset
guarantees deterministically rather than as a runtime growth-curve detail.
`TestResetLeavesMapWritable` is the map half of the same property: a `Record`
call immediately after `Reset` must not panic and must start the counter over
at the recorded value, not accumulate onto stale state.

`TestResetNaivePanicsOnNextRecord` is the test that pins the actual
production incident. `resetNaive` is unexported and unreachable from the
package API; it reassigns both fields to nil the way a first draft usually
does, and the test proves the panic lands one `Record` call later, not on the
reset itself -- which is exactly why the bug survives a test that only checks
state immediately after `Reset`. `TestCountsAndSamplesDoNotAlias` confirms
both accessors return independent clones by mutating the returned values and
checking the collector's own state is untouched.

Create `collector_test.go`:

```go
package scrape

import (
	"errors"
	"fmt"
	"sort"
	"testing"
)

// resetNaive is the "clearing" a Reset method is often first written as: it
// reassigns both scratch fields to nil instead of emptying them in place. It
// is never exported and never reachable from the package API; it exists so
// the tests can pin what it breaks.
func resetNaive(c *Collector) {
	c.counts = nil
	c.samples = nil
}

func TestRecordAccumulatesCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Sample
		want    map[string]float64
	}{
		{
			name:    "empty",
			records: nil,
			want:    map[string]float64{},
		},
		{
			name:    "single record",
			records: []Sample{{Name: "requests_total", Value: 1}},
			want:    map[string]float64{"requests_total": 1},
		},
		{
			name: "repeated name accumulates",
			records: []Sample{
				{Name: "requests_total", Value: 1},
				{Name: "requests_total", Value: 1},
				{Name: "errors_total", Value: 1},
			},
			want: map[string]float64{"requests_total": 2, "errors_total": 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := NewCollector(4)
			if err != nil {
				t.Fatalf("NewCollector: %v", err)
			}
			for _, s := range tc.records {
				c.Record(s.Name, s.Value)
			}
			got := c.Counts()
			if len(got) != len(tc.want) {
				t.Fatalf("Counts() = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("Counts()[%q] = %g, want %g", k, got[k], v)
				}
			}
		})
	}
}

func TestNewCollectorRejectsNegativeCapacity(t *testing.T) {
	t.Parallel()

	for _, n := range []int{-1, -100} {
		if _, err := NewCollector(n); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("NewCollector(%d) error = %v, want ErrInvalidCapacity", n, err)
		}
	}
}

func TestNewCollectorAcceptsZeroCapacity(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(0)
	if err != nil {
		t.Fatalf("NewCollector(0): %v", err)
	}
	c.Record("x", 1)
	if got := c.Counts()["x"]; got != 1 {
		t.Fatalf("Counts()[x] = %g, want 1", got)
	}
}

// TestResetRetainsCapacity is the point of the module: Reset must keep the
// samples backing array instead of discarding it. The exact number of
// reallocations across many ticks is a runtime growth-curve detail and is not
// asserted; what is asserted is the property that matters -- capacity
// preallocated by NewCollector survives a Reset unchanged.
func TestResetRetainsCapacity(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(8)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	for i := 0; i < 5; i++ {
		c.Record("tick_a", float64(i))
	}
	before := cap(c.samples)

	c.Reset()
	if len(c.samples) != 0 {
		t.Fatalf("len(samples) after Reset = %d, want 0", len(c.samples))
	}
	if cap(c.samples) != before {
		t.Fatalf("cap(samples) after Reset = %d, want %d (unchanged)", cap(c.samples), before)
	}

	c.Record("tick_b", 1)
	if cap(c.samples) != before {
		t.Fatalf("cap(samples) after post-reset Record = %d, want %d (no reallocation)", cap(c.samples), before)
	}
}

// TestResetLeavesMapWritable proves clear(c.counts) does not turn the map
// into a nil map: a Record call immediately after Reset must not panic, and
// the counter must start over from zero.
func TestResetLeavesMapWritable(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(4)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	c.Record("requests_total", 3)
	c.Reset()
	if len(c.Counts()) != 0 {
		t.Fatalf("Counts() after Reset = %v, want empty", c.Counts())
	}

	c.Record("requests_total", 1)
	if got := c.Counts()["requests_total"]; got != 1 {
		t.Fatalf("Counts()[requests_total] after post-reset Record = %g, want 1", got)
	}
}

// TestResetNaivePanicsOnNextRecord is the heart of the module: it pins the
// exact failure resetNaive ships to production. Reassigning c.counts to nil
// compiles and "looks cleared", but the very next Record call writes into a
// nil map and panics with assignment to entry in nil map -- reintroducing the
// nil-map write panic from 00-concepts one tick later than the reset itself.
func TestResetNaivePanicsOnNextRecord(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(4)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	c.Record("requests_total", 1)
	resetNaive(c)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Record after resetNaive did not panic; want assignment to entry in nil map")
		}
	}()
	c.Record("requests_total", 1)
	t.Fatal("unreachable: Record should have panicked before this line")
}

// TestCountsAndSamplesDoNotAlias proves the accessors return clones: mutating
// the returned map or slice must not change what the collector reports next.
func TestCountsAndSamplesDoNotAlias(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(4)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	c.Record("requests_total", 1)

	counts := c.Counts()
	counts["requests_total"] = 999
	counts["injected"] = 1
	if got := c.Counts()["requests_total"]; got != 1 {
		t.Fatalf("mutating the returned map changed collector state: %g", got)
	}
	if _, ok := c.Counts()["injected"]; ok {
		t.Fatal("mutating the returned map injected a key into collector state")
	}

	samples := c.Samples()
	samples[0].Value = 999
	if got := c.Samples()[0].Value; got != 1 {
		t.Fatalf("mutating the returned slice changed collector state: %g", got)
	}
}

// ExampleCollector is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleCollector() {
	c, err := NewCollector(4)
	if err != nil {
		panic(err)
	}

	c.Record("requests_total", 1)
	c.Record("requests_total", 1)
	c.Record("errors_total", 1)

	counts := c.Counts()
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s=%g\n", name, counts[name])
	}
	fmt.Println("samples:", len(c.Samples()), "cap:", cap(c.samples))

	before := cap(c.samples)
	c.Reset()
	c.Record("requests_total", 1)
	fmt.Println("after reset samples:", len(c.samples), "cap unchanged:", cap(c.samples) == before)

	// Output:
	// errors_total=1
	// requests_total=2
	// samples: 3 cap: 4
	// after reset samples: 1 cap unchanged: true
}
```

## Review

`Reset` is correct when both scratch fields keep their identity across a
tick: the map stays the same non-nil map with zero entries, and the slice
stays the same backing array with its length back at zero. `clear(c.counts)`
and `c.samples = c.samples[:0]` deliver exactly that. Reassigning either field
to nil instead is the mistake this module isolates: on the map it recreates
the nil-map write panic one `Record` call after the reset that looked clean,
and on the slice it throws away the capacity `NewCollector` paid to reserve.
`NewCollector` rejects a negative sample capacity with `ErrInvalidCapacity`,
checkable with `errors.Is`, and accepts zero. `Counts` and `Samples` both
return clones, so a caller can hold a snapshot across a `Reset` without a data
race, though the type itself is documented as not safe for concurrent use --
callers must still serialize their own access. `ExampleCollector` is the
executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [Go 1.21 Release Notes: clear](https://go.dev/doc/go1.21#language) — the built-in's behavior for maps and slices.
- [Go Specification: Clear](https://go.dev/ref/spec#Clear) — the precise per-type semantics `clear` guarantees.
- [Go Wiki: nil map panics](https://go.dev/doc/faq#nil_error) — background on why a nil map is readable but not writable.
- [Prometheus Go client library](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus) — the production shape this collector is modeled on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-acl-decision-cache-tristate.md](14-acl-decision-cache-tristate.md) | Next: [16-typed-nil-any-kv-store.md](16-typed-nil-any-kv-store.md)
