# Exercise 7: Screen NaN and Inf Out of a float64 Telemetry Feed Before Aggregation

A pricing or sensor feed legitimately uses `float64` — the values are measurements,
not money. But one malformed sample carrying `NaN` will poison a running mean the
instant it is folded in, and every downstream comparison becomes meaningless. This
exercise builds the ingestion validator that screens `NaN` and `Inf` at the boundary,
so the aggregate stays finite and trustworthy.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
feedguard/                   independent module: example.com/feedguard
  go.mod                     module path
  feedguard.go               type Aggregator; Add, Mean; ParseSample; NearlyEqual; ErrNotFinite
  cmd/
    demo/
      main.go                folds a feed with a NaN and shows it is skipped
  feedguard_test.go          NaN/Inf rejected, aggregate stays finite, epsilon compare
```

Files: `feedguard.go`, `cmd/demo/main.go`, `feedguard_test.go`.
Implement: an `Aggregator` with `Add(x float64) error` that rejects `NaN`/`Inf` (and
values outside a sane domain) before folding into a running sum/count, `Mean() float64`,
`ParseSample(string) (float64, error)`, and a `NearlyEqual(a, b, eps float64) bool`
helper for the one place floats are compared.
Test: a slice containing a `NaN` produces a skip/error and the aggregate stays finite;
`Inf` is rejected; `NearlyEqual` treats `0.1+0.2` and `0.3` as equal while `==` does
not; `x == x` is `false` for the `NaN` sentinel.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/feedguard/cmd/demo
cd ~/go-exercises/feedguard
go mod init example.com/feedguard
```

### Why one NaN ruins everything downstream

`NaN` is the "not a number" result of an undefined float operation (`0.0/0.0`, a
parse of `"NaN"`, a subtraction of two infinities). It has one property that makes it
uniquely dangerous in aggregation: every comparison involving `NaN` is `false`,
including `NaN == NaN`. So `x == x` being `false` is the tell — it is the only value
in Go that is not equal to itself. Worse, `NaN` is absorbing under arithmetic:
`anything + NaN == NaN`. Fold a single `NaN` sample into a running sum and the sum is
`NaN` forever after; the mean is `NaN`; any threshold check like `mean > limit`
silently returns `false` because comparisons against `NaN` are false. The corruption
is total and irreversible, and it produces no error on its own.

`+Inf` and `-Inf` are the overflow/`x/0` results and are almost as bad: `Inf - Inf`
is `NaN`, and an `Inf` in a sum pins the mean to an infinity. The defense is to screen
at the *entry point*, before the value joins the aggregate. `Aggregator.Add` calls
`math.IsNaN(x)` and `math.IsInf(x, 0)` (the `0` sign argument matches either
infinity) and returns `ErrNotFinite` for either, and it optionally rejects values
outside a sane domain (here, a configurable max magnitude) so an absurd-but-finite
reading does not silently skew the mean. Only a value that passes every screen is
added to `sum` and `count`. Because the aggregate only ever sees finite, in-domain
numbers, `Mean` is always finite.

The one place a float comparison is unavoidable — checking whether two computed
values agree — uses `NearlyEqual(a, b, eps)`, which compares `math.Abs(a-b) <= eps`.
This is why, for `float64` *variables*, `a + b == c` is `false` when `a,b,c` hold
`0.1, 0.2, 0.3` (the left side computes to `0.30000000000000004`) but
`NearlyEqual(a+b, c, 1e-9)` is `true`: you compare within a tolerance, never for
bit-exact equality. One subtlety worth internalizing for this chapter: the *literal*
expression `0.1+0.2 == 0.3` is `true`, because Go evaluates untyped floating-point
constants in arbitrary precision at compile time, so the rounding never happens. The
inexactness only appears once the values live in `float64` variables at runtime —
which is exactly why the demo and tests below force runtime evaluation through
variables. Note `NearlyEqual` deliberately returns `false` when either input is `NaN`,
since `Abs(NaN-x)` is `NaN` and `NaN <= eps` is `false` — the guard composes correctly
with the screening.

Create `feedguard.go`:

```go
package feedguard

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ErrNotFinite marks a sample that is NaN, Inf, or outside the accepted domain.
var ErrNotFinite = errors.New("sample is not a finite in-domain value")

// Aggregator folds finite float64 samples into a running mean. It never lets a
// non-finite value enter the sum.
type Aggregator struct {
	sum    float64
	count  int
	maxAbs float64 // reject |x| beyond this; 0 means no magnitude limit
}

// NewAggregator returns an aggregator that rejects samples with magnitude above
// maxAbs. Pass 0 to accept any finite value.
func NewAggregator(maxAbs float64) *Aggregator {
	return &Aggregator{maxAbs: maxAbs}
}

// Add screens x and folds it into the running sum, or returns ErrNotFinite.
func (a *Aggregator) Add(x float64) error {
	if math.IsNaN(x) {
		return fmt.Errorf("NaN sample: %w", ErrNotFinite)
	}
	if math.IsInf(x, 0) {
		return fmt.Errorf("infinite sample: %w", ErrNotFinite)
	}
	if a.maxAbs > 0 && math.Abs(x) > a.maxAbs {
		return fmt.Errorf("sample %g exceeds max magnitude %g: %w", x, a.maxAbs, ErrNotFinite)
	}
	a.sum += x
	a.count++
	return nil
}

// Mean returns the running mean, or 0 when no samples have been accepted. It is
// always finite because only finite samples are ever added.
func (a *Aggregator) Mean() float64 {
	if a.count == 0 {
		return 0
	}
	return a.sum / float64(a.count)
}

// Count reports how many samples were accepted.
func (a *Aggregator) Count() int { return a.count }

// ParseSample parses a decimal sample and rejects a non-finite literal like "NaN".
func ParseSample(s string) (float64, error) {
	x, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sample %q: %w", s, ErrNotFinite)
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, fmt.Errorf("sample %q is not finite: %w", s, ErrNotFinite)
	}
	return x, nil
}

// NearlyEqual reports whether a and b are within eps of each other. It returns
// false if either is NaN, composing safely with the screening above.
func NearlyEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
```

### The runnable demo

The demo folds a feed that contains a `NaN` between good samples, counts how many were
accepted, and prints the finite mean — the `NaN` is skipped, not absorbed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math"

	"example.com/feedguard"
)

func main() {
	feed := []float64{10.0, math.NaN(), 20.0, math.Inf(1), 30.0}
	agg := feedguard.NewAggregator(0)

	skipped := 0
	for _, x := range feed {
		if err := agg.Add(x); err != nil {
			skipped++
		}
	}

	fmt.Printf("accepted=%d skipped=%d mean=%.1f\n", agg.Count(), skipped, agg.Mean())

	// Use variables so the sum is a runtime float64, not an exact compile-time constant.
	a, b, c := 0.1, 0.2, 0.3
	fmt.Printf("a+b == c: %v\n", a+b == c)
	fmt.Printf("NearlyEqual(a+b, c, 1e-9): %v\n", feedguard.NearlyEqual(a+b, c, 1e-9))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
accepted=3 skipped=2 mean=20.0
a+b == c: false
NearlyEqual(a+b, c, 1e-9): true
```

### Tests

`TestScreensNonFinite` folds a feed with a `NaN` and an `Inf` and asserts both are
rejected, only the finite samples are counted, and the mean is finite. `TestNaNSentinel`
asserts the property that motivates the whole guard: `x == x` is `false` when `x` is
`math.NaN()`. `TestNearlyEqual` shows `==` fails on `0.1+0.2` vs `0.3` while the
epsilon compare succeeds, and that `NearlyEqual` rejects a `NaN` operand.
`TestParseSampleRejectsNaN` proves a `"NaN"` literal is caught at parse time.

Create `feedguard_test.go`:

```go
package feedguard

import (
	"errors"
	"math"
	"testing"
)

func TestScreensNonFinite(t *testing.T) {
	t.Parallel()

	feed := []float64{10, math.NaN(), 20, math.Inf(1), 30, math.Inf(-1)}
	agg := NewAggregator(0)
	rejected := 0
	for _, x := range feed {
		if err := agg.Add(x); err != nil {
			if !errors.Is(err, ErrNotFinite) {
				t.Fatalf("Add(%v) error = %v, want ErrNotFinite", x, err)
			}
			rejected++
		}
	}
	if agg.Count() != 3 {
		t.Fatalf("accepted count = %d, want 3", agg.Count())
	}
	if rejected != 3 {
		t.Fatalf("rejected count = %d, want 3", rejected)
	}
	if m := agg.Mean(); math.IsNaN(m) || math.IsInf(m, 0) || m != 20 {
		t.Fatalf("mean = %v, want finite 20", m)
	}
}

func TestNaNSentinel(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	if nan == nan {
		t.Fatal("NaN == NaN was true; expected false (this is why screening is required)")
	}
}

func TestNearlyEqual(t *testing.T) {
	t.Parallel()

	// Variables force runtime float64 evaluation; the literal form would fold to an
	// exact compile-time constant and compare equal.
	a, b, c := 0.1, 0.2, 0.3
	if a+b == c {
		t.Fatal("a+b == c was true at runtime; expected false due to binary rounding")
	}
	if !NearlyEqual(a+b, c, 1e-9) {
		t.Fatal("NearlyEqual(a+b, c, 1e-9) was false; expected true")
	}
	if NearlyEqual(math.NaN(), c, 1e-9) {
		t.Fatal("NearlyEqual with a NaN operand was true; expected false")
	}
}

func TestParseSampleRejectsNaN(t *testing.T) {
	t.Parallel()

	if _, err := ParseSample("NaN"); !errors.Is(err, ErrNotFinite) {
		t.Fatalf("ParseSample(NaN) error = %v, want ErrNotFinite", err)
	}
	if got, err := ParseSample("19.95"); err != nil || !NearlyEqual(got, 19.95, 1e-9) {
		t.Fatalf("ParseSample(19.95) = %v,%v; want ~19.95,nil", got, err)
	}
}

func TestMaxMagnitude(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(1000)
	if err := agg.Add(5000); !errors.Is(err, ErrNotFinite) {
		t.Fatalf("Add(5000) with maxAbs=1000 error = %v, want ErrNotFinite", err)
	}
	if err := agg.Add(999); err != nil {
		t.Fatalf("Add(999) with maxAbs=1000: unexpected error %v", err)
	}
}
```

## Review

The validator is correct when the aggregate can never observe a non-finite value: every
`NaN` and `Inf` returns `ErrNotFinite` at `Add`, and `Mean` is provably finite because
only finite, in-domain samples reach the sum. The two tests that carry the lesson are
`TestNaNSentinel` (`NaN == NaN` is `false`, the reason `==` cannot be your filter) and
`TestNearlyEqual` (bit-exact `==` fails on `0.1+0.2` vs `0.3`; epsilon comparison is the
only honest float equality). The mistake to avoid is trusting a float comparison to
catch a bad value — a `NaN` slips through every `<`, `>`, and `==` check because those
are all `false` — so you must call `math.IsNaN`/`math.IsInf` explicitly at the boundary.

## Resources

- [`math.IsNaN`](https://pkg.go.dev/math#IsNaN) — the only reliable NaN test, since `==` is always false for NaN.
- [`math.IsInf`](https://pkg.go.dev/math#IsInf) — infinity test with a sign selector (`0` matches either).
- [`math.NaN` / `math.Inf`](https://pkg.go.dev/math#NaN) — the non-finite sentinels used to build test feeds.
- [Go Specification: Floating-point operators](https://go.dev/ref/spec#Floating-point_operators) — IEEE-754 semantics, including NaN comparison rules.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-deterministic-remainder-allocation.md](08-deterministic-remainder-allocation.md)
