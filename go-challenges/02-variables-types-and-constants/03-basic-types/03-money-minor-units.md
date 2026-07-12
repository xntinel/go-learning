# Exercise 3: Money — Integer Minor Units Instead of float64

Money is the canonical example of a value that must never be a `float64`. This
module builds a `Money` type stored as `int64` minor units (cents) — the
representation real billing and ledger code uses — with an exact decimal parser, an
overflow-guarded `Add`, explicit rounding for tax, and a golden test that watches a
naive float accumulation drift away from the exact answer.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
money/                     independent module: example.com/money
  go.mod                   go 1.26
  money.go                 Money (int64 minor units); Parse, FromFloat, Add, WithTaxRate, Format
  cmd/
    demo/
      main.go              sums prices, applies tax, shows float drift
  money_test.go            exact sums, million-cent exactness, overflow, float divergence, Format
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Money` as `int64` cents plus a currency; `Parse` (exact decimal string to cents), `FromFloat` (rejects NaN/Inf), `Add` (overflow-guarded, currency-checked), `WithTaxRate` (explicit rounding), `Format` (`"USD 12.34"`).
- Test: `0.10 + 0.20` is exactly 30 cents; a million one-cent adds are exact; `Add` near `math.MaxInt64` errors instead of wrapping; a float accumulation diverges from `Money`; `Format` round-trips.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/03-basic-types/03-money-minor-units/cmd/demo
cd go-solutions/02-variables-types-and-constants/03-basic-types/03-money-minor-units
```

### Why cents, and where the overflow guard goes

`float64` cannot represent `0.10` exactly, so a `float64` ledger drifts: sum ten
dimes and you get `0.9999999999999999`, not `1.0`, and beyond `2^53` a `float64`
cannot even hold consecutive integers. Money must be exact, so it is stored as an
`int64` count of the smallest unit — cents for USD. Addition of two counts is then
exact integer addition; the only failure mode is `int64` overflow, and unlike a
`float64` that would quietly go to `Inf`, integer overflow *wraps* (a huge positive
plus a small positive can become negative). So `Add` must detect overflow before it
happens. The check is the two's-complement sign rule: adding two positives that
yield a negative (or two negatives that yield a positive) overflowed. `math/bits`
offers `Add64` for genuine multi-word arithmetic when you outgrow 64 bits; here a
single `int64` with the sign check is enough, and returning an error is the boundary
doing its job.

Parsing keeps the exactness by *never* going through `float64`: `Parse` splits
`"12.34"` on the decimal point and assembles `12*100 + 34` with integer arithmetic,
so no rounding error is introduced. `FromFloat` exists only for the cases where you
genuinely start from a measured float, and it guards `NaN`/`Inf` (which would
otherwise produce a garbage cent count) and rounds explicitly with `math.Round`
(half away from zero). `WithTaxRate` also rounds explicitly rather than truncating,
because "round the tax" is a documented business rule, not an accident of `int64`
conversion.

Create `money.go`:

```go
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Sentinel errors for the money boundary.
var (
	ErrParse            = errors.New("invalid money value")
	ErrOverflow         = errors.New("money addition overflows int64")
	ErrCurrencyMismatch = errors.New("currency mismatch")
)

// Money is an exact monetary amount stored as int64 minor units (e.g. cents),
// never as float64. currency is an ISO-like code such as "USD".
type Money struct {
	minor    int64
	currency string
}

// New builds a Money from a raw minor-unit count.
func New(minor int64, currency string) Money {
	return Money{minor: minor, currency: currency}
}

// Minor returns the raw minor-unit count.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the currency code.
func (m Money) Currency() string { return m.currency }

// Parse converts a decimal string like "12.34" into exact minor units without
// ever touching float64. At most two fractional digits are allowed.
func Parse(s, currency string) (Money, error) {
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return Money{}, fmt.Errorf("%w: %q", ErrParse, s)
	}

	var whole int64
	if intPart != "" {
		v, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return Money{}, fmt.Errorf("%w: %q", ErrParse, s)
		}
		whole = v
	}

	var cents int64
	if hasFrac {
		if len(fracPart) == 0 || len(fracPart) > 2 {
			return Money{}, fmt.Errorf("%w: %q has too many fractional digits", ErrParse, s)
		}
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		v, err := strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return Money{}, fmt.Errorf("%w: %q", ErrParse, s)
		}
		cents = v
	}

	total := whole*100 + cents
	if neg {
		total = -total
	}
	return Money{minor: total, currency: currency}, nil
}

// FromFloat builds Money from a measured float, rejecting NaN/Inf and rounding
// half away from zero. Prefer Parse for exact string input.
func FromFloat(dollars float64, currency string) (Money, error) {
	if math.IsNaN(dollars) || math.IsInf(dollars, 0) {
		return Money{}, fmt.Errorf("%w: %v", ErrParse, dollars)
	}
	return Money{minor: int64(math.Round(dollars * 100)), currency: currency}, nil
}

// Add returns the sum of two same-currency amounts, or an error on currency
// mismatch or int64 overflow.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	sum := m.minor + o.minor
	if (m.minor > 0 && o.minor > 0 && sum < 0) || (m.minor < 0 && o.minor < 0 && sum > 0) {
		return Money{}, fmt.Errorf("%w: %d + %d", ErrOverflow, m.minor, o.minor)
	}
	return Money{minor: sum, currency: m.currency}, nil
}

// WithTaxRate returns the amount plus tax at rate (e.g. 0.08 for 8%), rounding
// the tax half away from zero.
func (m Money) WithTaxRate(rate float64) Money {
	tax := int64(math.Round(float64(m.minor) * rate))
	return Money{minor: m.minor + tax, currency: m.currency}
}

// Format renders the amount as "USD 12.34".
func (m Money) Format() string {
	sign := ""
	minor := m.minor
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s %s%d.%02d", m.currency, sign, minor/100, minor%100)
}
```

### The runnable demo

The demo sums two prices exactly, applies 8% tax with explicit rounding, and then
prints a `float64` accumulation of ten dimes beside the exact `Money` answer so the
drift is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/money"
)

func main() {
	a, err := money.Parse("19.99", "USD")
	if err != nil {
		log.Fatal(err)
	}
	b, err := money.Parse("5.01", "USD")
	if err != nil {
		log.Fatal(err)
	}
	sum, err := a.Add(b)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("subtotal:", sum.Format())
	fmt.Println("with tax:", sum.WithTaxRate(0.08).Format())

	dime, _ := money.Parse("0.10", "USD")
	exact := money.New(0, "USD")
	var drift float64
	for range 10 {
		exact, _ = exact.Add(dime)
		drift += 0.10
	}
	fmt.Printf("ten dimes: money=%s float=%.17g\n", exact.Format(), drift)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subtotal: USD 25.00
with tax: USD 27.00
ten dimes: money=USD 1.00 float=0.99999999999999989
```

### Tests

The tests are the proof that integer minor units buy exactness. `TestExactSum`
parses `0.10` and `0.20` and asserts the sum is exactly 30 cents. `TestMillionCents`
adds one cent a million times and asserts the total is exactly `1_000_000` cents —
a loop a `float64` accumulator would drift on. `TestAddOverflow` adds one to
`math.MaxInt64` and asserts an `ErrOverflow` rather than a wrapped negative.
`TestFloatDivergence` accumulates `0.10` ten times in a `float64` and shows it is not
`1.0`, while the `Money` path lands exactly on `100` cents. `TestFormat` round-trips
known amounts including a negative.

Create `money_test.go`:

```go
package money

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestExactSum(t *testing.T) {
	t.Parallel()

	a, err := Parse("0.10", "USD")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("0.20", "USD")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Minor() != 30 {
		t.Fatalf("0.10 + 0.20 = %d cents, want 30", sum.Minor())
	}
}

func TestMillionCents(t *testing.T) {
	t.Parallel()

	total := New(0, "USD")
	cent := New(1, "USD")
	for range 1_000_000 {
		var err error
		total, err = total.Add(cent)
		if err != nil {
			t.Fatal(err)
		}
	}
	if total.Minor() != 1_000_000 {
		t.Fatalf("million cents = %d, want 1000000", total.Minor())
	}
}

func TestAddOverflow(t *testing.T) {
	t.Parallel()

	_, err := New(math.MaxInt64, "USD").Add(New(1, "USD"))
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("Add near MaxInt64 error = %v, want ErrOverflow", err)
	}
}

func TestCurrencyMismatch(t *testing.T) {
	t.Parallel()

	_, err := New(100, "USD").Add(New(100, "EUR"))
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestFloatDivergence(t *testing.T) {
	t.Parallel()

	exact := New(0, "USD")
	dime := New(10, "USD")
	var drift float64
	for range 10 {
		var err error
		exact, err = exact.Add(dime)
		if err != nil {
			t.Fatal(err)
		}
		drift += 0.10
	}
	if exact.Minor() != 100 {
		t.Fatalf("money path = %d cents, want 100", exact.Minor())
	}
	if drift == 1.0 {
		t.Fatal("float accumulation unexpectedly exact; the point is that it drifts")
	}
}

func TestParseRejectsTooPrecise(t *testing.T) {
	t.Parallel()

	if _, err := Parse("1.234", "USD"); !errors.Is(err, ErrParse) {
		t.Fatalf("Parse(1.234) error = %v, want ErrParse", err)
	}
}

func TestFromFloatRejectsNaN(t *testing.T) {
	t.Parallel()

	if _, err := FromFloat(math.NaN(), "USD"); !errors.Is(err, ErrParse) {
		t.Fatalf("FromFloat(NaN) error = %v, want ErrParse", err)
	}
}

func ExampleMoney_Format() {
	fmt.Println(New(1234, "USD").Format())
	fmt.Println(New(-500, "EUR").Format())
	// Output:
	// USD 12.34
	// EUR -5.00
}
```

## Review

The type is correct when exactness is structural, not incidental: because the amount
is an `int64` count, `TestMillionCents` and `TestExactSum` cannot drift, and
`TestFloatDivergence` documents the failure the type prevents. The overflow guard is
the one place integer math can still betray you — `TestAddOverflow` proves `Add`
returns an error where a plain `m.minor + o.minor` would wrap negative. `Parse` stays
exact by never constructing a `float64`; `FromFloat` is the quarantined entry point
where a measured float becomes cents, and it rounds explicitly so the rounding rule
is a decision, not a side effect of truncating conversion.

## Resources

- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — background on Go's value representations.
- [math package](https://pkg.go.dev/math#Round) — `math.Round`, `math.MaxInt64`, `IsNaN`/`IsInf`.
- [math/bits: Add64](https://pkg.go.dev/math/bits#Add64) — multi-word addition when 64 bits is not enough.
- [Floating-Point Arithmetic (IEEE 754 overview)](https://go.dev/ref/spec#Numeric_types) — why `0.1` is not exact.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-safe-integer-narrowing.md](04-safe-integer-narrowing.md)
