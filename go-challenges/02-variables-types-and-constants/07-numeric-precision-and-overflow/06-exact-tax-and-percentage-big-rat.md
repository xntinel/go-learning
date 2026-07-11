# Exercise 6: Exact Tax and Percentage Computation with math/big Rational Arithmetic

Tax and interest are the numbers auditors check. A sales-tax line that is off by a
cent because a `float64` misrounded is not a rounding preference — it is a
reconciliation failure. This exercise builds a tax calculator that lifts a minor-unit
amount into `math/big.Rat`, applies a percentage as an exact fraction, and converts
back to minor units with a documented half-to-even rule, so the result is exact and
auditable rather than whatever binary floating point happened to produce.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
exacttax/                    independent module: example.com/exacttax
  go.mod                     module path
  tax.go                     type Rate; ApplyRate (big.Rat, half-even); NaiveFloatRate
  cmd/
    demo/
      main.go                exact vs float tax on a divergent input
  tax_test.go                exact cents assertions, big.Rat vs float divergence
```

Files: `tax.go`, `cmd/demo/main.go`, `tax_test.go`.
Implement: `Rate` (an exact `num/den` percentage), `ApplyRate(amountMinor int64, r
Rate) int64` that computes `amount * rate` as a `big.Rat` and rounds half-to-even to
minor units, and `NaiveFloatRate(amountMinor int64, ratePercent float64) int64` that
does the same with `float64` + `math.Round`, to expose the divergence.
Test: exact-cents assertions where the `big.Rat` path is right and the float path is
wrong (e.g. 50% of 105 cents is 52 by half-to-even, not 53); a `7.25%` case matching a
hand-computed value; and a divergence assertion on a documented input.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/exacttax/cmd/demo
cd ~/go-exercises/exacttax
go mod init example.com/exacttax
```

### Exact until you choose to round

The reason `float64` misroundes tax is that both the rate and the product are stored
inexactly: `7.25%` is not representable, and neither is the cent-fraction result.
`math/big.Rat` removes both errors. A `Rate` is an exact fraction — `7.25%` is
`725/10000`, `50%` is `50/100` — with no rounding at construction. `ApplyRate` lifts
the integer amount into a `big.Rat` and multiplies by the rate `Rat`, producing the
*exact* rational product; the amount is built from a `big.Int` rather than
`SetFrac64(amount*num, den)` so a large amount times a large numerator cannot overflow
an intermediate `int64`. Nothing has rounded yet: `5299` cents at `7.25%` is exactly
`3841775/10000`, i.e. `384.1775` cents.

Rounding is a separate, explicit, documented step. The rule here is *round half to
even* (banker's rounding), the same rule IEEE-754 uses by default and the one that
avoids the upward bias of always rounding `.5` away from zero. The helper computes the
Euclidean floor of `num/den` with `big.Int.DivMod` (which yields a non-negative
remainder even for negative amounts), then compares twice the remainder to the
denominator: below, round down; above, round up; exactly half, round to the even
neighbor. This is correct for every sign, which matters for refunds and negative
adjustments. Contrast `NaiveFloatRate`, which multiplies `float64` values and calls
`math.Round`: for `50%` of `105` cents it computes `math.Round(52.5) = 53` (half away
from zero, on top of any binary error), where the exact half-to-even answer is `52`.
That single-cent gap, multiplied across a day of invoices, is the bug this exercise
prevents.

Create `tax.go`:

```go
package exacttax

import (
	"math"
	"math/big"
)

// Rate is an exact percentage expressed as num/den, e.g. 7.25% is Rate{725, 10000}.
type Rate struct {
	Num int64
	Den int64
}

// Percent builds a Rate from a percentage with hundredths precision, e.g.
// Percent(725) is 7.25%.
func Percent(hundredthsOfPercent int64) Rate {
	return Rate{Num: hundredthsOfPercent, Den: 10000}
}

// ApplyRate returns amountMinor * rate rounded to minor units, half-to-even. The
// product is computed exactly as a big.Rat; only the final step rounds.
func ApplyRate(amountMinor int64, r Rate) int64 {
	amount := new(big.Rat).SetInt(big.NewInt(amountMinor))
	rate := new(big.Rat).SetFrac64(r.Num, r.Den)
	product := new(big.Rat).Mul(amount, rate)
	return roundHalfEven(product).Int64()
}

// roundHalfEven rounds an exact rational to the nearest integer, ties to even.
func roundHalfEven(r *big.Rat) *big.Int {
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom()) // always > 0 for a normalized Rat
	q := new(big.Int)
	m := new(big.Int)
	q.DivMod(num, den, m) // q = floor(num/den); 0 <= m < den
	twoM := new(big.Int).Lsh(m, 1)
	switch twoM.Cmp(den) {
	case -1:
		return q
	case 1:
		return q.Add(q, big.NewInt(1))
	default: // exactly half: round to even
		if q.Bit(0) == 0 {
			return q
		}
		return q.Add(q, big.NewInt(1))
	}
}

// NaiveFloatRate is the float64 approach shown for contrast: it multiplies floats
// and rounds half away from zero, which misrounds where ApplyRate is exact.
func NaiveFloatRate(amountMinor int64, ratePercent float64) int64 {
	return int64(math.Round(float64(amountMinor) * ratePercent / 100))
}
```

### The runnable demo

The demo applies `50%` to `105` cents both ways so the divergence is visible: the
exact path yields `52`, the float path yields `53`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/exacttax"
)

func main() {
	const amount = 105 // cents

	exact := exacttax.ApplyRate(amount, exacttax.Percent(5000)) // 50.00%
	naive := exacttax.NaiveFloatRate(amount, 50)

	fmt.Printf("exact half-even: %d cents\n", exact)
	fmt.Printf("naive float:     %d cents\n", naive)

	tax := exacttax.ApplyRate(10000, exacttax.Percent(725)) // 7.25% of $100.00
	fmt.Printf("7.25%% of 10000 cents = %d cents\n", tax)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
exact half-even: 52 cents
naive float:     53 cents
7.25% of 10000 cents = 725 cents
```

### Tests

The exact-cents tests pin known results: `7.25%` of `10000` cents is exactly `725`;
`7.25%` of `5299` cents is `384.1775`, which half-to-even rounds to `384`. The
half-to-even tests pin the ties: `50%` of `105` is `52` (even) and of `115` is `58`
(even), where naive rounding would give `53` and `58` respectively. The divergence
test asserts that on the documented input `(105, 50%)` the exact and float paths
disagree, proving the exact representation is not academic. A negative case shows the
rule is sign-correct.

Create `tax_test.go`:

```go
package exacttax

import "testing"

func TestApplyRateExact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		amount int64
		rate   Rate
		want   int64
	}{
		{10000, Percent(725), 725}, // 7.25% of $100.00
		{5299, Percent(725), 384},  // 384.1775 -> 384
		{105, Percent(5000), 52},   // 52.5 -> 52 (even)
		{115, Percent(5000), 58},   // 57.5 -> 58 (even)
		{-105, Percent(5000), -52}, // -52.5 -> -52 (even)
		{0, Percent(725), 0},
	}
	for _, c := range cases {
		if got := ApplyRate(c.amount, c.rate); got != c.want {
			t.Fatalf("ApplyRate(%d, %v) = %d, want %d", c.amount, c.rate, got, c.want)
		}
	}
}

func TestExactDivergesFromFloat(t *testing.T) {
	t.Parallel()

	const amount = 105
	exact := ApplyRate(amount, Percent(5000))
	naive := NaiveFloatRate(amount, 50)
	if exact == naive {
		t.Fatalf("expected exact (%d) and float (%d) to diverge on the documented input", exact, naive)
	}
	if exact != 52 || naive != 53 {
		t.Fatalf("exact=%d naive=%d; want exact=52 naive=53", exact, naive)
	}
}

func TestAmountPlusTaxIsExact(t *testing.T) {
	t.Parallel()

	// The exact product for 5299 @ 7.25% is 384.1775, so the rounded tax is 384
	// and the grossed-up total is 5299 + 384 = 5683 with no drift.
	tax := ApplyRate(5299, Percent(725))
	if tax != 384 {
		t.Fatalf("tax = %d, want 384", tax)
	}
	if total := 5299 + tax; total != 5683 {
		t.Fatalf("total = %d, want 5683", total)
	}
}
```

## Review

The calculator is correct when the amount and rate stay exact through the
multiplication and only the final, documented step rounds. Confirm the exact cases
(`725`, `384`) and the half-to-even ties (`52`, `58`, `-52`) — the negative tie proves
`DivMod`'s non-negative remainder makes the rule sign-correct. The divergence test is
the point of the exercise: it shows a concrete input where `big.Rat` gives `52` and
`float64` + `math.Round` gives `53`, so "just use a float and round at the end" is
demonstrably wrong for money. The trap to avoid is `SetFrac64(amount*rateNum, den)`,
where `amount*rateNum` can overflow `int64` before `big` ever sees it; building the
`Rat` from a `big.Int` amount keeps the whole computation exact.

## Resources

- [`math/big#Rat`](https://pkg.go.dev/math/big#Rat) — exact rational arithmetic: `SetFrac64`, `Mul`, `Num`, `Denom`.
- [`math/big#Int.DivMod`](https://pkg.go.dev/math/big#Int.DivMod) — Euclidean division with a non-negative remainder, the basis of the half-even rule.
- [Round half to even](https://en.wikipedia.org/wiki/Rounding#Rounding_half_to_even) — the banker's-rounding rule and why it avoids bias.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-float-nan-inf-ingestion-guard.md](07-float-nan-inf-ingestion-guard.md)
