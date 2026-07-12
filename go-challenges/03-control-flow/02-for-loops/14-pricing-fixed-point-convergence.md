# Exercise 14: Fixed-Point Iteration for Pricing Convergence

**Nivel: Intermedio** — validacion rapida (un test corto).

A dynamic repricing engine nudges a price toward equilibrium a little at a
time and stops when it settles — the price stops changing more than a cent
between calls. That "stops changing" condition, not a fixed attempt count, is
the loop's real termination test; the attempt count is only the safety net
for a repricer that never settles. This module builds `Converge` around
exactly that shape.

This module is fully self-contained: its own `go mod init` and one test file.

## What you'll build

```text
pricingconverge/              module example.com/pricingconverge
  go.mod                      go 1.24
  converge.go                 ErrNotConverged; Converge(start, tolerance, maxIterations, reprice)
  converge_test.go             reaches target, exact iteration count, budget exceeded, diverging
```

- Files: `converge.go`, `converge_test.go`.
- Implement: `Converge(start, tolerance float64, maxIterations int, reprice func(price float64) float64) (float64, int, error)` — a counted loop `for iter := 1; iter <= maxIterations; iter++` that calls `reprice`, checks `math.Abs(next-price) <= tolerance` as the real exit, and returns `ErrNotConverged` if the loop runs out of attempts first.
- Test: a damped repricer that converges well inside the budget; the same repricer with an exact hand-computed iteration count; the same repricer capped too tight to converge; a diverging repricer that never settles.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the exit condition is the diff, not the counter

`Converge` is a counted loop shape (`for iter := 1; iter <= maxIterations`),
but the counter is not what decides when it succeeds — the *tolerance check*
is. Every iteration applies `reprice` once and compares the new price to the
old one; the loop returns the instant two consecutive prices are close
enough, regardless of which iteration that happens to be. `maxIterations`
only exists because `reprice` is an arbitrary function the caller supplies:
a bad damping factor, a discontinuous demand curve, or a bug can make the
sequence oscillate or diverge forever, and without a hard iteration cap that
would hang the caller. This is the same "bounded budget plus a real exit
condition" discipline as a retry loop, applied to numerical convergence
instead of a network call.

Create `converge.go`:

```go
package pricing

import (
	"errors"
	"math"
)

// ErrNotConverged means reprice did not settle within tolerance before
// maxIterations was reached.
var ErrNotConverged = errors.New("pricing: did not converge")

// Converge repeatedly applies reprice to price, treating it as a fixed-point
// iteration: it stops as soon as two consecutive prices differ by no more
// than tolerance, which is the loop's real termination condition, not a fixed
// attempt count. maxIterations is the safety bound in case reprice never
// settles (a bad damping factor, a discontinuous demand curve). It returns
// the converged price and the number of iterations it took.
func Converge(start, tolerance float64, maxIterations int, reprice func(price float64) float64) (float64, int, error) {
	price := start
	for iter := 1; iter <= maxIterations; iter++ {
		next := reprice(price)
		if math.Abs(next-price) <= tolerance {
			return next, iter, nil
		}
		price = next
	}
	return 0, maxIterations, ErrNotConverged
}
```

### Tests

`TestConvergeReachesTarget` uses a repricer that halves the gap to a target
of 100 on every call and asserts convergence within a generous budget.
`TestConvergeExactIterationCount` pins down the exact iteration this
particular sequence converges on (the gap halves from 50 each time, so it
takes exactly 14 iterations to drop to 0.01), which doubles as documentation
of how fast damped convergence is. `TestConvergeNotConvergedWithinBudget`
takes the identical repricer and caps the budget at 3, well short of 14, and
asserts the sentinel error. `TestConvergeDivergingRepriceNeverSettles` covers
a repricer that overshoots and grows every call, proving the cap catches a
genuinely unbounded sequence too.

Create `converge_test.go`:

```go
package pricing

import (
	"errors"
	"math"
	"testing"
)

func TestConvergeReachesTarget(t *testing.T) {
	t.Parallel()

	reprice := func(price float64) float64 {
		return price + 0.5*(100-price)
	}

	got, iters, err := Converge(0, 0.01, 100, reprice)
	if err != nil {
		t.Fatalf("Converge() error = %v, want nil", err)
	}
	if math.Abs(got-100) > 0.01 {
		t.Fatalf("Converge() = %v, want within 0.01 of 100", got)
	}
	if iters <= 0 || iters > 100 {
		t.Fatalf("iters = %d, want in (0, 100]", iters)
	}
}

func TestConvergeExactIterationCount(t *testing.T) {
	t.Parallel()

	// diff halves every iteration starting at 50, so it takes exactly 14
	// iterations to reach <= 0.01: 50 / 2^13 = 0.006103515625.
	reprice := func(price float64) float64 {
		return price + 0.5*(100-price)
	}

	_, iters, err := Converge(0, 0.01, 100, reprice)
	if err != nil {
		t.Fatalf("Converge() error = %v, want nil", err)
	}
	if iters != 14 {
		t.Fatalf("iters = %d, want 14", iters)
	}
}

func TestConvergeNotConvergedWithinBudget(t *testing.T) {
	t.Parallel()

	reprice := func(price float64) float64 {
		return price + 0.5*(100-price)
	}

	// The exact same reprice needs 14 iterations; capping at 3 must fail.
	_, iters, err := Converge(0, 0.01, 3, reprice)
	if !errors.Is(err, ErrNotConverged) {
		t.Fatalf("err = %v, want ErrNotConverged", err)
	}
	if iters != 3 {
		t.Fatalf("iters = %d, want 3", iters)
	}
}

func TestConvergeDivergingRepriceNeverSettles(t *testing.T) {
	t.Parallel()

	// A repricer that overshoots and grows every call never settles.
	reprice := func(price float64) float64 {
		return -price*1.5 - 1
	}

	_, _, err := Converge(1, 0.01, 20, reprice)
	if !errors.Is(err, ErrNotConverged) {
		t.Fatalf("err = %v, want ErrNotConverged", err)
	}
}
```

## Review

`Converge` is correct when it stops the instant the tolerance check passes —
never later, which `TestConvergeExactIterationCount` pins down to the exact
iteration — and when it reports `ErrNotConverged` honestly whenever the
budget runs out first, whether that is because the budget was too tight
(`TestConvergeNotConvergedWithinBudget`) or because `reprice` never settles
at all (`TestConvergeDivergingRepriceNeverSettles`). The loop shape is
counted, but the counter is only ever the safety net; the tolerance
comparison is the exit condition that actually matters, and every test here
is really testing that comparison. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counted form with an early return used here.
- [math package](https://pkg.go.dev/math) — `math.Abs` for the tolerance check.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-backoff-schedule-plateau.md](13-backoff-schedule-plateau.md) | Next: [15-sorted-id-reconciliation-merge.md](15-sorted-id-reconciliation-merge.md)
