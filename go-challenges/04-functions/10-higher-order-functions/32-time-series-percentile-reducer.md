# Exercise 32: Time-Series Metrics Reduction to Percentile Statistics

**Nivel: Intermedio** — validacion rapida (un test corto).

Every latency dashboard is built on the same reduction: a pile of
individual samples collapses into a handful of percentiles — p50, p95,
p99 — that summarize the distribution well enough to act on. `Percentiles`
is that reduction as one function: sort once, then fold each requested
percentile down to a single representative sample using the nearest-rank
method.

## What you'll build

```text
percentiles/                 independent module: example.com/percentiles
  go.mod                     go 1.24
  percentiles.go               func Percentiles
  percentiles_test.go          known dataset, single sample, empty input, no mutation, order-independence
  cmd/demo/
    main.go                  computes p50/p95/p99 over 100 samples
```

- Files: `percentiles.go`, `percentiles_test.go`, `cmd/demo/main.go`.
- Implement: `Percentiles(samples []float64, ps ...float64) (result map[float64]float64, ok bool)`.
- Test: a known 1-100 dataset produces exact p50/p95/p99 values; a single-sample dataset returns that sample for every requested percentile; an empty `samples` slice returns `ok=false`; the caller's `samples` slice is never mutated (no in-place sort); an unsorted input produces the same result as the same data pre-sorted.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/percentiles/cmd/demo
cd ~/go-exercises/percentiles
go mod init example.com/percentiles
go mod edit -go=1.24
```

### Sort once, reduce many times

`Percentiles` accepts a variadic list of percentiles precisely so the
expensive part — sorting `n` samples — happens exactly once no matter how
many percentiles the caller wants out of the same dataset. Each
percentile is then a cheap, independent lookup: nearest-rank maps a
percentage `p` to a rank via `ceil(p/100 * n)`, and that rank (1-indexed,
so it is converted to a 0-indexed slice position) picks out one already-sorted
sample. Clamping the resulting index into `[0, n-1]` is what keeps `p=100`
from reading one past the end of the slice and keeps a badly-formed
`p<=0` from indexing negative.

The caller's original slice is never touched — `Percentiles` copies into
`sorted` before calling `sort.Float64s`, because a metrics-collection
function that silently reorders the caller's underlying sample buffer as
a side effect is a surprising, hard-to-debug thing for a caller to
discover only after their own code that also holds that slice starts
seeing samples in a different order.

Create `percentiles.go`:

```go
package percentiles

import (
	"math"
	"sort"
)

// Percentiles reduces samples to a value per requested percentile in ps
// (each in (0, 100]) using the nearest-rank method: samples are sorted
// once, and each percentile picks the sample at the ceiling-rounded rank
// for that percentage of the dataset. It reports ok=false for an empty
// samples slice, since no percentile is meaningful over zero data points.
func Percentiles(samples []float64, ps ...float64) (result map[float64]float64, ok bool) {
	if len(samples) == 0 {
		return nil, false
	}

	// Sort a copy so the caller's slice is never reordered as a side
	// effect of computing percentiles over it.
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	n := len(sorted)
	result = make(map[float64]float64, len(ps))
	for _, p := range ps {
		rank := int(math.Ceil(p / 100 * float64(n)))
		idx := rank - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		result[p] = sorted[idx]
	}
	return result, true
}
```

### The runnable demo

The demo builds 100 samples with values 1 through 100 — a dataset chosen
so the nearest-rank percentiles land on round, easy-to-verify numbers —
and requests the three percentiles almost every latency dashboard cares
about.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/percentiles"
)

func main() {
	// 100 latency samples in milliseconds: 1, 2, ..., 100.
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = float64(i + 1)
	}

	result, ok := percentiles.Percentiles(samples, 50, 95, 99)
	if !ok {
		fmt.Println("no samples")
		return
	}

	fmt.Printf("p50=%.0fms p95=%.0fms p99=%.0fms\n", result[50], result[95], result[99])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
p50=50ms p95=95ms p99=99ms
```

For 100 sorted samples numbered 1 through 100, the nearest-rank formula
picks index `ceil(p/100*100) - 1`, which lands exactly on sample value
`p` for each of these three percentiles — a convenient property of this
particular dataset, not a general rule for arbitrary sample counts.

### Tests

`TestPercentilesOnOneToOneHundred` is the exact-value check the demo's
output depends on. `TestPercentilesSingleSample` covers the degenerate
case where every percentile, however extreme, can only ever point at the
one sample that exists. `TestPercentilesEmptySamplesReturnsNotOK` is the
guard clause: zero samples must not silently return a zero-valued map
that looks like a real (if boring) answer. `TestPercentilesDoesNotMutateCallerSlice`
directly verifies the defensive-copy claim by comparing the caller's
slice before and after the call. `TestPercentilesUnsortedInputMatchesPreSortedInput`
confirms the result depends only on the sample values, never on the order
they were given in.

Create `percentiles_test.go`:

```go
package percentiles

import "testing"

func TestPercentilesOnOneToOneHundred(t *testing.T) {
	t.Parallel()

	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = float64(i + 1)
	}

	result, ok := Percentiles(samples, 50, 95, 99)
	if !ok {
		t.Fatal("Percentiles() ok = false, want true")
	}

	cases := map[float64]float64{50: 50, 95: 95, 99: 99}
	for p, want := range cases {
		if got := result[p]; got != want {
			t.Fatalf("result[%v] = %v, want %v", p, got, want)
		}
	}
}

func TestPercentilesSingleSample(t *testing.T) {
	t.Parallel()

	result, ok := Percentiles([]float64{42}, 1, 50, 99)
	if !ok {
		t.Fatal("Percentiles() ok = false, want true")
	}
	for _, p := range []float64{1, 50, 99} {
		if got := result[p]; got != 42 {
			t.Fatalf("result[%v] = %v, want 42", p, got)
		}
	}
}

func TestPercentilesEmptySamplesReturnsNotOK(t *testing.T) {
	t.Parallel()

	_, ok := Percentiles(nil, 50)
	if ok {
		t.Fatal("Percentiles(nil, ...) ok = true, want false")
	}
}

func TestPercentilesDoesNotMutateCallerSlice(t *testing.T) {
	t.Parallel()

	samples := []float64{5, 3, 1, 4, 2}
	original := append([]float64(nil), samples...)

	Percentiles(samples, 50)

	for i := range samples {
		if samples[i] != original[i] {
			t.Fatalf("samples = %v, want unchanged %v (Percentiles must not sort in place)", samples, original)
		}
	}
}

func TestPercentilesUnsortedInputMatchesPreSortedInput(t *testing.T) {
	t.Parallel()

	unsorted := []float64{40, 10, 30, 20, 50}
	sorted := []float64{10, 20, 30, 40, 50}

	got, _ := Percentiles(unsorted, 50)
	want, _ := Percentiles(sorted, 50)

	if got[50] != want[50] {
		t.Fatalf("Percentiles(unsorted) = %v, want %v (input order must not matter)", got[50], want[50])
	}
}
```

## Review

`Percentiles` is correct because sorting happens exactly once, on a copy,
before any percentile is computed — combining "sort" and "look up a
rank" into one pass per percentile would either resort redundantly for
every requested `p` or, worse, tempt an implementation into using
`sort.Slice` directly on the caller's backing array. The clamping on
`idx` is the detail a first draft is most likely to skip: without it,
`p=100` computes a rank equal to `n`, and `sorted[n]` panics with an
index-out-of-range on exactly the boundary value a real caller is most
likely to request. Nearest-rank is one of several valid percentile
methods (linear interpolation is another, and produces different, often
non-sample values) — state which one a function implements, since
mixing methods across a codebase's metrics produces dashboards that
disagree with each other over the exact same raw data.

## Resources

- [sort package](https://pkg.go.dev/sort) — `Float64s`, the sorting this reduction is built on.
- [math package](https://pkg.go.dev/math) — `Ceil`, the rounding rule nearest-rank depends on.
- [Prometheus: Histograms and summaries](https://prometheus.io/docs/practices/histograms/) — a production metrics system whose percentile/quantile computation this exercise models a simplified version of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-result-wrapper-timing-and-metadata.md](31-result-wrapper-timing-and-metadata.md) | Next: [33-schema-validate-transform-chain.md](33-schema-validate-transform-chain.md)
