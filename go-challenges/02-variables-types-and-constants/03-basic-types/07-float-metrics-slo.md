# Exercise 7: Metrics Aggregation — NaN/Inf Guards and Epsilon Comparison for SLOs

An SLO evaluator averages latency and error-rate samples and compares a computed
success ratio against a target. Two `float64` hazards can wreck it: a single `NaN`
sample poisons the whole mean, and comparing a computed ratio to a threshold with
`==` is unreliable. This module builds the aggregator that guards both.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
slo/                       independent module: example.com/slo
  go.mod                   go 1.26
  slo.go                   Mean (NaN/Inf-skipping); SLO.Met; CompareWithin; KahanSum
  cmd/
    demo/
      main.go              aggregates poisoned samples, evaluates an SLO
  slo_test.go              NaN/Inf skipped, epsilon at threshold, 0.1+0.2 case, no-data
```

- Files: `slo.go`, `cmd/demo/main.go`, `slo_test.go`.
- Implement: a `Mean` that skips `NaN`/`Inf` with a counter, `SLO.Met` comparing a ratio to a threshold within an epsilon, `CompareWithin` (epsilon-tolerant three-way compare via `cmp.Compare`), and `KahanSum` for order-stable summation.
- Test: a `NaN` sample does not poison the mean; a ratio exactly at threshold compares equal within epsilon; `0.1+0.2 != 0.3` under `==` but passes under epsilon; `Inf` handled; no-data errors.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/slo/cmd/demo
cd ~/go-exercises/slo
go mod init example.com/slo
```

### Why NaN and == are both traps

`NaN` is contagious: `NaN + x` is `NaN`, so one bad sample (a division by zero
upstream, a failed parse that became `NaN`) turns an entire running sum into `NaN`,
and because `NaN != NaN` the poison is invisible to an equality check afterward. The
aggregator must therefore reject or skip non-finite samples *before* they enter the
sum. This `Mean` skips them and counts them, so the mean stays finite and the skip
count surfaces that something upstream is producing garbage — silently dropping them
without a counter would hide a real problem. `+Inf`/`-Inf` from overflow get the same
treatment via `math.IsInf`.

Comparing the computed ratio to the threshold with `==` is the second trap. Two
mathematically equal computations can differ in the last bit — `0.1 + 0.2` is
`0.30000000000000004`, not `0.3` — so `ratio == threshold` can be false for a ratio
that is "really" at the target. The fix is a tolerance: `CompareWithin` treats two
values within `epsilon` as equal and otherwise falls back to `cmp.Compare` for the
ordering. `SLO.Met` then passes when the ratio is at or above the threshold within
that tolerance. For summing many samples, `KahanSum` uses compensated summation to
limit the drift that a naive left-to-right `+=` accumulates over a large count.

One subtlety the tests make explicit: the `0.1 + 0.2 != 0.3` drift only appears at
*runtime*, on `float64` values. Go's untyped constants are arbitrary precision, so the
literal expression `0.1 + 0.2 == 0.3` is folded by the compiler with exact arithmetic
and evaluates to `true` — the rounding never happens. To observe the real `float64`
behavior you must put the values in `float64` variables first, which is exactly what an
aggregator does with runtime samples.

Create `slo.go`:

```go
package slo

import (
	"cmp"
	"errors"
	"math"
)

// ErrNoData is returned when an SLO is evaluated with no observations.
var ErrNoData = errors.New("no data")

// Mean is a running average that skips non-finite samples instead of letting a
// single NaN or Inf poison the aggregate.
type Mean struct {
	sum     float64
	n       int
	skipped int
}

// Add incorporates x, or skips and counts it if it is NaN or Inf. It reports
// whether the sample was accepted.
func (m *Mean) Add(x float64) bool {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		m.skipped++
		return false
	}
	m.sum += x
	m.n++
	return true
}

// Value returns the mean of accepted samples, or (0, false) if there were none.
func (m *Mean) Value() (float64, bool) {
	if m.n == 0 {
		return 0, false
	}
	return m.sum / float64(m.n), true
}

// Skipped reports how many non-finite samples were rejected.
func (m *Mean) Skipped() int { return m.skipped }

// CompareWithin returns 0 when a and b are within eps, else cmp.Compare(a, b).
// It is the epsilon-tolerant replacement for == on computed floats.
func CompareWithin(a, b, eps float64) int {
	if math.Abs(a-b) <= eps {
		return 0
	}
	return cmp.Compare(a, b)
}

// SLO is a success-ratio target with a comparison tolerance.
type SLO struct {
	Threshold float64
	Epsilon   float64
}

// Met reports whether success/total meets or exceeds the threshold within the
// epsilon tolerance. It errors when there is no data.
func (s SLO) Met(success, total int) (bool, error) {
	if total <= 0 {
		return false, ErrNoData
	}
	ratio := float64(success) / float64(total)
	return CompareWithin(ratio, s.Threshold, s.Epsilon) >= 0, nil
}

// KahanSum sums xs with compensated (Kahan) summation to limit floating-point
// drift over many samples.
func KahanSum(xs []float64) float64 {
	var sum, c float64
	for _, x := range xs {
		y := x - c
		t := sum + y
		c = (t - sum) - y
		sum = t
	}
	return sum
}
```

### The runnable demo

The demo feeds a mix of good, `NaN`, and `Inf` latency samples and shows the mean
stays finite with a skip count, evaluates an SLO exactly at its threshold, and
contrasts `==` with the epsilon compare on `0.1 + 0.2`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math"

	"example.com/slo"
)

func main() {
	m := &slo.Mean{}
	for _, x := range []float64{12, math.NaN(), 18, math.Inf(1), 30} {
		m.Add(x)
	}
	mean, ok := m.Value()
	fmt.Printf("mean=%.1f ok=%t skipped=%d\n", mean, ok, m.Skipped())

	s := slo.SLO{Threshold: 0.99, Epsilon: 1e-9}
	met, _ := s.Met(99, 100)
	fmt.Println("slo met:", met)

	// float64 variables, so the sum is rounded at runtime (constant 0.1+0.2
	// would be folded exactly and wrongly compare equal to 0.3).
	a, b, c := 0.1, 0.2, 0.3
	fmt.Println("0.1+0.2 == 0.3:", a+b == c)
	fmt.Println("within eps:", slo.CompareWithin(a+b, c, 1e-9) == 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mean=20.0 ok=true skipped=2
slo met: true
0.1+0.2 == 0.3: false
within eps: true
```

### Tests

`TestMeanSkipsNonFinite` documents the hazard (a naive `10 + NaN + 20` is `NaN`) and
then shows the aggregator's mean is finite and the skip count is right.
`TestEpsilonCatchesFloatError` asserts `0.1+0.2 != 0.3` under `==` but is equal under
`CompareWithin`. `TestSLOAtThreshold` checks a ratio exactly at the threshold is met,
and `TestNextafterWithinEpsilon` shows a value one ULP above the threshold
(`math.Nextafter`) is still treated as met. `TestNoData` asserts the empty case errors.

Create `slo_test.go`:

```go
package slo

import (
	"errors"
	"math"
	"testing"
)

func TestMeanSkipsNonFinite(t *testing.T) {
	t.Parallel()

	// The hazard: a single NaN poisons a naive sum.
	if naive := 10.0 + math.NaN() + 20.0; !math.IsNaN(naive) {
		t.Fatal("expected naive sum with NaN to be NaN")
	}

	m := &Mean{}
	for _, x := range []float64{10, math.NaN(), 20, math.Inf(1), math.Inf(-1)} {
		m.Add(x)
	}
	got, ok := m.Value()
	if !ok || got != 15 {
		t.Fatalf("Value = %v,%v; want 15,true", got, ok)
	}
	if m.Skipped() != 3 {
		t.Fatalf("Skipped = %d, want 3", m.Skipped())
	}
}

func TestEpsilonCatchesFloatError(t *testing.T) {
	t.Parallel()

	// float64 variables force runtime rounding; the untyped constant 0.1+0.2
	// is folded exactly by the compiler and would (wrongly) equal 0.3.
	a, b, c := 0.1, 0.2, 0.3
	if a+b == c {
		t.Fatal("expected 0.1+0.2 != 0.3 under ==")
	}
	if CompareWithin(a+b, c, 1e-9) != 0 {
		t.Fatal("epsilon compare should treat 0.1+0.2 and 0.3 as equal")
	}
}

func TestSLOAtThreshold(t *testing.T) {
	t.Parallel()

	s := SLO{Threshold: 0.99, Epsilon: 1e-9}
	met, err := s.Met(99, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !met {
		t.Fatal("ratio exactly at threshold should be met")
	}

	below, _ := s.Met(98, 100)
	if below {
		t.Fatal("ratio below threshold should not be met")
	}
}

func TestNextafterWithinEpsilon(t *testing.T) {
	t.Parallel()

	just := math.Nextafter(0.99, 1) // one ULP above the threshold
	if CompareWithin(just, 0.99, 1e-9) != 0 {
		t.Fatal("one ULP difference should be within epsilon")
	}
}

func TestNoData(t *testing.T) {
	t.Parallel()

	if _, err := (SLO{Threshold: 0.99}).Met(0, 0); !errors.Is(err, ErrNoData) {
		t.Fatalf("Met(0,0) error = %v, want ErrNoData", err)
	}
}

func TestKahanSum(t *testing.T) {
	t.Parallel()

	xs := make([]float64, 1000)
	for i := range xs {
		xs[i] = 0.1
	}
	got := KahanSum(xs)
	if CompareWithin(got, 100.0, 1e-9) != 0 {
		t.Fatalf("KahanSum = %v, want ~100", got)
	}
}
```

## Review

The aggregator is correct when no single sample can poison the result and no computed
comparison relies on `==`. `TestMeanSkipsNonFinite` shows the mean stays finite where a
naive sum would be `NaN`, and the skip counter keeps the dropped samples visible rather
than hidden. The epsilon tests are the other half: a ratio at or one ULP above the
threshold is met, and `0.1+0.2` compares equal to `0.3` under tolerance though not under
`==`. `KahanSum` is there for the large-count case where left-to-right `+=` would drift;
for a handful of samples either is fine, but the compensated version documents the
concern.

## Resources

- [math package](https://pkg.go.dev/math#IsNaN) — `IsNaN`, `IsInf`, `Abs`, `Nextafter`.
- [cmp.Compare](https://pkg.go.dev/cmp#Compare) — the ordered three-way compare used under the tolerance.
- [Go Specification: Floating-point operators](https://go.dev/ref/spec#Arithmetic_operators) — NaN and infinity semantics.
- [Kahan summation](https://en.wikipedia.org/wiki/Kahan_summation_algorithm) — compensated summation for large sample counts.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-duration-timeout-config.md](08-duration-timeout-config.md)
