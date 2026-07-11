# Exercise 18: Merge Histogram Bucket Layers With slices.Sorted

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cumulative histogram -- the shape both Prometheus and OpenTelemetry use --
is only meaningful if its bucket boundaries are strictly ascending: bucket `i`
counts every observation less than or equal to boundary `i`, and bucket `i+1`
counts a superset of what bucket `i` counts, so the moment two boundaries are
out of order the "cumulative" part of the invariant is simply false. A real
service rarely defines its buckets in one place. There are built-in defaults,
a per-route override for an endpoint with different latency characteristics,
maybe a per-tenant override on top of that -- three or four independently
authored lists, merged once at startup into the list the histogram is actually
registered with.

The trap is assuming that merging preserves order because each input already
has it. A config loader that does `boundaries := append(defaults, overrides...)`
is reasoning "both lists are sorted, so the result is sorted" -- which is true
for an interleaved merge and false for a concatenation. `append` only joins;
it never interleaves. The bug is invisible in a demo where the override values
happen to be larger than every default, and it is very visible the day someone
adds a per-route override smaller than an existing default: the histogram
keeps accepting observations, keeps exporting numbers, and every downstream
quantile estimate is now silently wrong, because the client library assumed
what the config loader failed to guarantee.

This module builds `Planner`, a small accumulator that collects boundaries
from any number of layers into a set, then derives the canonical ordering with
`slices.Sorted` over `maps.Keys` -- the standard-library idiom for turning "a
set I built as I went" into "a deterministic, ascending slice", the exact
opposite of trusting concatenation to preserve order.

The reason a set is the right accumulator, and not just a slice with a final
sort tacked on, is that boundary layers do not always arrive as neat batches.
A per-tenant override might be discovered one flag at a time while parsing a
larger config file, a feature-flag service might push one bucket value per
API call during a gradual rollout, and the same boundary can legitimately show
up twice -- once in the defaults, once again because an operator "overrode" a
route with the value it already had. A `map[float64]struct{}` absorbs all of
that for free: insertion is idempotent, duplicates cost nothing, and the only
work left is deciding what order to present the result in once, at the very
end, right before the histogram is registered.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
histobuckets/            module example.com/histobuckets
  go.mod                 go 1.24
  histobuckets.go         Planner; NewPlanner, Add, Boundaries; ErrNonFiniteBoundary
  histobuckets_test.go    layer table, partial-Add-on-error, aliasing, the
                          naive-concatenation contrast, ExamplePlanner_Boundaries
```

- Files: `histobuckets.go`, `histobuckets_test.go`.
- Implement: `NewPlanner(defaults []float64) (*Planner, error)` seeding a Planner and rejecting a non-finite default with `ErrNonFiniteBoundary`; `(*Planner).Add(vals ...float64) error` merging another layer in, deduplicating, and stopping at the first non-finite value while keeping values added earlier in the same call; `(*Planner).Boundaries() []float64` returning `slices.Sorted(maps.Keys(p.seen))`.
- Test: construction from nil, sorted, and unsorted-with-duplicates defaults; NaN and infinite defaults rejected; merging a second layer that overlaps the first; a multi-value `Add` call that fails partway through, pinning which values survive and which do not; `Boundaries` returning a fresh slice on every call; a `mergeBucketsNaive` contrast proving concatenation of two individually-sorted layers is not itself sorted, next to `Planner` producing an ascending result from the same two layers; and `ExamplePlanner_Boundaries` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/histobuckets
cd ~/go-exercises/histobuckets
go mod init example.com/histobuckets
go mod edit -go=1.24
```

### A set gives you dedup for free; slices.Sorted gives you the order back

The natural way to accumulate boundaries from several layers, arriving over
several calls, is a `map[float64]struct{}`: adding the same value twice from
two different layers costs nothing extra and the duplicate simply does not
appear. What a map does not give you is an order to hand to a histogram
constructor -- map iteration order in Go is intentionally randomized, so
ranging over the set directly would reproduce the exact defect this module is
about, just relocated from "concatenation" to "map iteration".

`slices.Sorted` closes that gap. It takes an `iter.Seq[E]` of any ordered type
and returns a new, ascending, freshly allocated slice; `maps.Keys(m)` is
exactly such a sequence over a map's keys. `slices.Sorted(maps.Keys(seen))` is
the standard-library idiom for "I built a set as I went, now give me it back
as a deterministic slice" -- the same shape of problem as collecting distinct
tenant IDs seen in a log stream, or a Certificate Transparency monitor
merging log IDs, or any config layer that dedups by nature and needs a stable
output order once. It costs one allocation and one sort per call, which is the
right trade for something computed once at startup, not once per request.

The mistake this module contrasts against never touches a map at all -- it
looks entirely reasonable on its own terms:

```go
boundaries := append(defaults, overrides...)   // both individually sorted...
```

Each of `defaults` and `overrides` is ascending on its own; the concatenation
is not, unless every value in `overrides` happens to exceed every value in
`defaults`. `slices.IsSorted` on the result reports `false` the moment that
assumption fails, and nothing else in a typical test suite checks it, because
the test data was written by the same person who wrote the merge and shares
its blind spot.

Compare that to what an interleaved merge -- `slices.Merge` in spirit, though
the standard library exposes it only for iterators of pre-sorted sequences --
would have to do: walk both inputs simultaneously, always emitting the
smaller of the two current heads. That is genuinely more code than
concatenation, which is exactly why concatenation is what gets written under
time pressure and why it is worth naming the two operations differently in
review. `Planner` sidesteps the whole question by never merging sorted
sequences at all: it collects into a set as values arrive, in whatever order
they arrive, and sorts the set's contents exactly once. There is no ordering
assumption for a new layer to violate, because no ordering was ever assumed
of any layer in the first place.

Create `histobuckets.go`:

```go
// Package histobuckets collects histogram bucket boundaries from several
// configuration layers -- built-in defaults, a per-route override, a
// per-tenant override -- and produces the single ascending, deduplicated
// slice a cumulative histogram (the shape Prometheus and OpenTelemetry
// both use) requires its bucket boundaries to be in.
package histobuckets

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
)

// ErrNonFiniteBoundary is returned when a boundary is NaN or +/-Inf.
// Cumulative histograms need an ordered, comparable value at every
// boundary; NaN in particular compares unequal to itself, so it can never
// occupy a stable position no matter how the boundaries are sorted.
var ErrNonFiniteBoundary = errors.New("histobuckets: boundary must be finite")

// Planner accumulates histogram bucket boundaries from any number of
// configuration layers and exposes them, on demand, as a single canonical
// ascending slice.
//
// Planner is not safe for concurrent use: it is meant to be built up by a
// single goroutine during startup, before the histogram it configures is
// registered.
type Planner struct {
	seen map[float64]struct{}
}

// NewPlanner returns a Planner seeded with defaults. It returns
// ErrNonFiniteBoundary, wrapped with the offending value, if any default is
// NaN or infinite.
func NewPlanner(defaults []float64) (*Planner, error) {
	p := &Planner{seen: make(map[float64]struct{})}
	if err := p.Add(defaults...); err != nil {
		return nil, err
	}
	return p, nil
}

// Add merges additional boundaries from another configuration layer (a
// per-route or per-tenant override, say) into the Planner. A value already
// present is silently deduplicated. Add returns ErrNonFiniteBoundary,
// wrapped with the offending value, on the first NaN or infinite input;
// boundaries added earlier in the same call remain in the Planner.
func (p *Planner) Add(vals ...float64) error {
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: got %v", ErrNonFiniteBoundary, v)
		}
		p.seen[v] = struct{}{}
	}
	return nil
}

// Boundaries returns every distinct boundary added so far, sorted in
// ascending order -- the form a cumulative histogram requires.
//
// The returned slice is freshly allocated on every call and never aliases
// the Planner's internal storage; the caller may sort, mutate, or retain it
// freely. An empty Planner returns nil, matching slices.Sorted's contract
// for an empty input.
func (p *Planner) Boundaries() []float64 {
	return slices.Sorted(maps.Keys(p.seen))
}
```

### Using it

`Planner` is meant to live for exactly as long as configuration loading takes:
construct it once with `NewPlanner` from whatever defaults a service ships
with, call `Add` once per additional layer as each one is discovered --
environment overrides, per-route config, a feature flag's own bucket list --
and call `Boundaries` exactly once, right before registering the histogram.
Nothing about the type expects to survive past that point, which is why it
declares itself unsafe for concurrent use rather than paying for a mutex no
caller needs: startup code is single-goroutine by construction.

Two contracts matter to a caller. `Add` stops at the first non-finite value in
a given call but keeps everything added before it -- so a caller that wants
all-or-nothing semantics for one layer should validate that layer's slice
itself before calling `Add`, rather than relying on partial rollback that the
type does not provide. And `Boundaries` never hands back a slice that aliases
the `Planner`'s internal map, so a caller is free to sort it differently, clip
it, or hold onto it after the `Planner` itself is discarded.

`ExamplePlanner_Boundaries` is the runnable demonstration of this module: `go
test` executes it and compares its stdout against the `// Output:` comment
below.

```go
func ExamplePlanner_Boundaries() {
	p, err := NewPlanner([]float64{1, 0.5, 0.1, 5})
	if err != nil {
		panic(err)
	}
	if err := p.Add(0.5, 2, 0.1); err != nil { // per-route override layer
		panic(err)
	}
	fmt.Println(p.Boundaries())

	empty, _ := NewPlanner(nil)
	fmt.Println(empty.Boundaries() == nil)

	if err := p.Add(math.NaN()); err != nil {
		fmt.Println(err)
	}

	// Output:
	// [0.1 0.5 1 2 5]
	// true
	// histobuckets: boundary must be finite: got NaN
}
```

The first line shows two overlapping layers -- defaults `{1, 0.5, 0.1, 5}` and
an override `{0.5, 2, 0.1}` -- collapsing to five distinct boundaries in
ascending order, regardless of the order either layer listed them in. The
second line shows an empty `Planner` returning `nil` rather than an empty
non-nil slice, which is `slices.Sorted`'s own documented behavior for an empty
input. The third line shows `Add` rejecting `NaN` with a wrapped
`ErrNonFiniteBoundary`.

### Tests

`TestNewPlanner` is the construction table: nil defaults, defaults already in
order, defaults out of order with a duplicate, and a `NaN` or `+Inf` default
each rejected. `TestAddMergesAcrossLayersAndStopsAtFirstBadValue` merges a
second, overlapping layer and checks the combined result, then makes a single
`Add` call whose middle argument is `NaN` and confirms the values before it
were kept and the value after it was not -- pinning the "stops at the first
bad value, keeps what came before" contract stated on the doc comment.
`TestBoundariesReturnsFreshSlice` mutates one call's result and confirms a
later call is unaffected. `TestMergeBucketsNaiveIsNotSorted` is the heart of
the module: `mergeBucketsNaive` concatenates two individually-sorted layers
and the result is not sorted, checked directly with `slices.IsSorted`, while
`Planner` given the same two layers produces an ascending result right next to
it -- the exact contrast between "trusting concatenation" and "sorting the
merged set once."

Create `histobuckets_test.go`:

```go
package histobuckets

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
)

// mergeBucketsNaive is how a config loader is usually written the first
// time: each layer's boundary list is already sorted on its own -- the
// built-in defaults are sorted by definition, and a human typing a
// per-route override list usually types it in ascending order too -- so
// concatenating the layers "should" stay sorted. It does not: append only
// joins, it never interleaves, and the bug is invisible until a later
// layer's smallest value is less than an earlier layer's largest one.
// Never exported, never reachable from Planner.
func mergeBucketsNaive(layers ...[]float64) []float64 {
	var out []float64
	for _, layer := range layers {
		out = append(out, layer...)
	}
	return out
}

func TestNewPlanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		defaults []float64
		want     []float64
		wantErr  error
	}{
		{name: "nil defaults", defaults: nil, want: nil},
		{name: "sorted already", defaults: []float64{0.1, 0.5, 1}, want: []float64{0.1, 0.5, 1}},
		{name: "unsorted with duplicates", defaults: []float64{1, 0.1, 1, 0.5}, want: []float64{0.1, 0.5, 1}},
		{name: "NaN default rejected", defaults: []float64{1, math.NaN()}, wantErr: ErrNonFiniteBoundary},
		{name: "+Inf default rejected", defaults: []float64{math.Inf(1)}, wantErr: ErrNonFiniteBoundary},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPlanner(tc.defaults)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NewPlanner error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPlanner: unexpected error: %v", err)
			}
			if got := p.Boundaries(); !slices.Equal(got, tc.want) {
				t.Fatalf("Boundaries() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAddMergesAcrossLayersAndStopsAtFirstBadValue(t *testing.T) {
	t.Parallel()

	p, err := NewPlanner([]float64{0.1, 0.5, 1, 5})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	if err := p.Add(0.5, 2, 0.1); err != nil { // per-route layer, overlaps defaults
		t.Fatalf("Add: %v", err)
	}
	want := []float64{0.1, 0.5, 1, 2, 5}
	if got := p.Boundaries(); !slices.Equal(got, want) {
		t.Fatalf("Boundaries() = %v, want %v", got, want)
	}

	err = p.Add(10, -1, math.NaN(), 99)
	if !errors.Is(err, ErrNonFiniteBoundary) {
		t.Fatalf("Add error = %v, want ErrNonFiniteBoundary", err)
	}
	// 10 and -1 precede the NaN in the same call and must have been kept;
	// 99 follows it and must not have been added.
	want = []float64{-1, 0.1, 0.5, 1, 2, 5, 10}
	if got := p.Boundaries(); !slices.Equal(got, want) {
		t.Fatalf("Boundaries() after partial Add = %v, want %v", got, want)
	}
}

func TestBoundariesReturnsFreshSlice(t *testing.T) {
	t.Parallel()

	p, err := NewPlanner([]float64{1, 2, 3})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	first := p.Boundaries()
	first[0] = 999
	second := p.Boundaries()
	if second[0] == 999 {
		t.Fatal("mutating a returned slice affected a later call to Boundaries")
	}
}

// TestMergeBucketsNaiveIsNotSorted is the heart of the module: two
// individually-sorted layers, concatenated, do not stay sorted, and
// Planner never produces this because it sorts once from the merged set
// rather than trusting each layer's own order.
func TestMergeBucketsNaiveIsNotSorted(t *testing.T) {
	t.Parallel()

	defaults := []float64{0.1, 0.5, 1, 5}
	perRoute := []float64{0.2, 2, 10}

	naive := mergeBucketsNaive(defaults, perRoute)
	if slices.IsSorted(naive) {
		t.Fatalf("mergeBucketsNaive produced a sorted slice %v; the bug should have broken order", naive)
	}

	p, err := NewPlanner(defaults)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	if err := p.Add(perRoute...); err != nil {
		t.Fatalf("Add: %v", err)
	}
	planned := p.Boundaries()
	if !slices.IsSorted(planned) {
		t.Fatalf("Planner.Boundaries() = %v, want ascending order", planned)
	}
}

// ExamplePlanner_Boundaries is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment.
func ExamplePlanner_Boundaries() {
	p, err := NewPlanner([]float64{1, 0.5, 0.1, 5})
	if err != nil {
		panic(err)
	}
	if err := p.Add(0.5, 2, 0.1); err != nil { // per-route override layer
		panic(err)
	}
	fmt.Println(p.Boundaries())

	empty, _ := NewPlanner(nil)
	fmt.Println(empty.Boundaries() == nil)

	if err := p.Add(math.NaN()); err != nil {
		fmt.Println(err)
	}

	// Output:
	// [0.1 0.5 1 2 5]
	// true
	// histobuckets: boundary must be finite: got NaN
}
```

## Review

`Planner` is correct when `Boundaries()` is always ascending regardless of how
many layers fed it or what order each layer listed its own values in --
`slices.Sorted(maps.Keys(p.seen))` guarantees that by construction, because it
derives the order from the merged set on every call rather than trusting any
input to already be in the right order. The mistake it avoids is
`append(defaults, overrides...)`: syntactically a merge, semantically a
concatenation, correct only in the special case where the reader never
verifies it and the data never puts it to the test. Around that core,
`NewPlanner` and `Add` reject `NaN` and infinite boundaries with
`ErrNonFiniteBoundary`, checkable with `errors.Is`, while documenting that a
partially-invalid call keeps what came before the failure; `Boundaries` never
aliases the `Planner`'s internal map, so a caller can do anything it wants
with the result. `ExamplePlanner_Boundaries` is the executable documentation
`go test` verifies. Run `go test -count=1 -race ./...`.

## Resources

- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — collects an `iter.Seq[E]` into a fresh ascending slice; the function this module is built around.
- [`maps.Keys`](https://pkg.go.dev/maps#Keys) — the `iter.Seq[K]` over a map's keys that `Boundaries` feeds into `slices.Sorted`.
- [`slices.IsSorted`](https://pkg.go.dev/slices#IsSorted) — the check the contrast test uses to pin the concatenation bug directly.
- [Prometheus: Histograms and summaries](https://prometheus.io/docs/practices/histograms/) — why cumulative bucket boundaries must be strictly ascending.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-ssh-hostkey-pinset-contains.md](17-ssh-hostkey-pinset-contains.md) | Next: [19-etcd-range-scan-appendseq.md](19-etcd-range-scan-appendseq.md)
