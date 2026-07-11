# Exercise 9: Multi-Currency Amounts with Correct Minor-Unit Scale and Signed Balances

A ledger that holds only US dollars is a toy. Real payment systems carry JPY (no
decimal places), USD (two), and BHD (three) side by side, and a money type that
hard-codes `/100` formats JPY with phantom decimals and rounds BHD to the wrong
scale. This exercise builds a `Money` type that pairs an `int64` minor-unit amount
with a `Currency` carrying its ISO minor-unit exponent, so parsing, formatting, and
signed arithmetic all derive the scale from the currency.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
multicurrency/               independent module: example.com/multicurrency
  go.mod                     module path
  money.go                   type Currency, Money; Parse, String, Add, Sub; sentinels
  cmd/
    demo/
      main.go                parses several currencies, adds, shows a mismatch
  money_test.go              per-currency round-trip, mismatch, JPY no-decimals, underflow
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: `Currency{Code string; Exp int}`, `Money{Minor int64; Cur Currency}`,
`Parse(cur Currency, text string) (Money, error)` deriving scale from `10^Exp`,
`String()` formatting to the currency's precision, and `Add`/`Sub` that reject a
currency mismatch and enforce signed overflow/underflow guards.
Test: parse+format round-trips per currency (`"1000"` JPY, `"1.500"` BHD, `"12.99"`
USD); `Add` of two currencies returns `ErrCurrencyMismatch`; a JPY amount never formats
with decimals; a signed `Sub` underflow near `MinInt64` returns `ErrOverflow`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/multicurrency/cmd/demo
cd ~/go-exercises/multicurrency
go mod init example.com/multicurrency
```

### Scale is a property of the currency, not a constant

The minor-unit exponent is defined per currency by ISO 4217: JPY and KRW have
`Exp = 0` (the yen has no sub-unit), most currencies have `Exp = 2`, and BHD, KWD, and
OMR have `Exp = 3`. The `Money` type carries the `Currency`, and every operation
derives its scale as `10^Exp` instead of assuming `100`. `Parse` uses the exponent
three ways: for a zero-exponent currency it rejects any decimal point at all (a JPY
amount with a `.` is malformed), and for a nonzero exponent it requires *exactly* that
many fractional digits — `"1.500"` is valid BHD (three digits), `"1.50"` is not. It
then combines `whole * scale + frac` into the integer minor amount, guarding the
`whole * scale` multiplication against `int64` overflow with the same
check-before-operating pattern as the cents parser. `String` is the inverse: for
`Exp = 0` it prints the integer with no decimal point, and otherwise it prints
`minor/scale` and `minor%scale` zero-padded to `Exp` digits with `%0*d`, so JPY renders
`1000` and BHD renders `1.500`.

`Add` and `Sub` enforce two invariants. First, currencies must match: adding USD to JPY
is meaningless, so it returns `ErrCurrencyMismatch` rather than producing a nonsense
number — mixing currencies is a class of bug this type refuses to allow. Second, the
signed bounds are enforced in both directions, because a debit that underflows past
`math.MinInt64` corrupts a balance exactly as a credit that overflows past
`math.MaxInt64`. `Add`'s guards are the mirror pair from the checked-arithmetic
exercise; `Sub` is implemented so that subtracting toward `MinInt64` is caught before
the wrap, which is why a `Sub` that would drive a balance below `MinInt64` returns
`ErrOverflow`. Whether negative balances are permitted at all is a separate policy — this
type allows them but refuses to let them wrap.

Create `money.go`:

```go
package multicurrency

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Sentinel errors for the money type.
var (
	ErrFormat           = errors.New("money format")
	ErrOverflow         = errors.New("money overflow")
	ErrCurrencyMismatch = errors.New("currency mismatch")
)

// Currency carries an ISO 4217 code and its minor-unit exponent.
type Currency struct {
	Code string
	Exp  int
}

// Common currencies with their real minor-unit scales.
var (
	JPY = Currency{Code: "JPY", Exp: 0}
	USD = Currency{Code: "USD", Exp: 2}
	BHD = Currency{Code: "BHD", Exp: 3}
)

// Money is an exact amount in a currency's minor units.
type Money struct {
	Minor int64
	Cur   Currency
}

func pow10(n int) int64 {
	out := int64(1)
	for range n {
		out *= 10
	}
	return out
}

// Parse converts a decimal string into Money, deriving the required number of
// fractional digits from the currency's exponent. No float64 is involved.
func Parse(cur Currency, text string) (Money, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Money{}, fmt.Errorf("empty amount: %w", ErrFormat)
	}
	neg := false
	if rest, ok := strings.CutPrefix(text, "-"); ok {
		neg = true
		text = rest
	}

	whole, frac, hasFrac := strings.Cut(text, ".")
	if cur.Exp == 0 {
		if hasFrac {
			return Money{}, fmt.Errorf("%s takes no decimals: %w", cur.Code, ErrFormat)
		}
	} else {
		if !hasFrac {
			frac = strings.Repeat("0", cur.Exp)
		}
		if len(frac) != cur.Exp {
			return Money{}, fmt.Errorf("%s needs exactly %d decimals: %w", cur.Code, cur.Exp, ErrFormat)
		}
	}
	if whole == "" {
		whole = "0"
	}

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("parse whole %q: %w", whole, ErrFormat)
	}
	var f int64
	if cur.Exp > 0 {
		f, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return Money{}, fmt.Errorf("parse frac %q: %w", frac, ErrFormat)
		}
	}
	scale := pow10(cur.Exp)
	if w > (math.MaxInt64-f)/scale {
		return Money{}, fmt.Errorf("amount %q overflows %s: %w", text, cur.Code, ErrOverflow)
	}
	minor := w*scale + f
	if neg {
		minor = -minor
	}
	return Money{Minor: minor, Cur: cur}, nil
}

// String renders the amount to the currency's precision: no decimals for Exp 0.
func (m Money) String() string {
	sign := ""
	v := m.Minor
	if v < 0 {
		sign = "-"
		v = -v
	}
	if m.Cur.Exp == 0 {
		return fmt.Sprintf("%s%d", sign, v)
	}
	scale := pow10(m.Cur.Exp)
	return fmt.Sprintf("%s%d.%0*d", sign, v/scale, m.Cur.Exp, v%scale)
}

// Add returns a+b, rejecting a currency mismatch or a signed overflow.
func Add(a, b Money) (Money, error) {
	if a.Cur.Code != b.Cur.Code {
		return Money{}, fmt.Errorf("add %s+%s: %w", a.Cur.Code, b.Cur.Code, ErrCurrencyMismatch)
	}
	if b.Minor > 0 && a.Minor > math.MaxInt64-b.Minor {
		return Money{}, fmt.Errorf("add overflow: %w", ErrOverflow)
	}
	if b.Minor < 0 && a.Minor < math.MinInt64-b.Minor {
		return Money{}, fmt.Errorf("add underflow: %w", ErrOverflow)
	}
	return Money{Minor: a.Minor + b.Minor, Cur: a.Cur}, nil
}

// Sub returns a-b, rejecting a currency mismatch or a signed overflow/underflow.
func Sub(a, b Money) (Money, error) {
	if a.Cur.Code != b.Cur.Code {
		return Money{}, fmt.Errorf("sub %s-%s: %w", a.Cur.Code, b.Cur.Code, ErrCurrencyMismatch)
	}
	if b.Minor < 0 && a.Minor > math.MaxInt64+b.Minor {
		return Money{}, fmt.Errorf("sub overflow: %w", ErrOverflow)
	}
	if b.Minor > 0 && a.Minor < math.MinInt64+b.Minor {
		return Money{}, fmt.Errorf("sub underflow: %w", ErrOverflow)
	}
	return Money{Minor: a.Minor - b.Minor, Cur: a.Cur}, nil
}
```

### The runnable demo

The demo parses one amount in each currency, adds two USD amounts, and shows a
cross-currency add being rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/multicurrency"
)

func main() {
	yen, _ := multicurrency.Parse(multicurrency.JPY, "1000")
	bhd, _ := multicurrency.Parse(multicurrency.BHD, "1.500")
	a, _ := multicurrency.Parse(multicurrency.USD, "12.99")
	b, _ := multicurrency.Parse(multicurrency.USD, "2.50")

	fmt.Printf("JPY: %s (minor=%d)\n", yen, yen.Minor)
	fmt.Printf("BHD: %s (minor=%d)\n", bhd, bhd.Minor)

	sum, _ := multicurrency.Add(a, b)
	fmt.Printf("USD: %s + %s = %s\n", a, b, sum)

	if _, err := multicurrency.Add(a, yen); err != nil {
		fmt.Println("mismatch:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
JPY: 1000 (minor=1000)
BHD: 1.500 (minor=1500)
USD: 12.99 + 2.50 = 15.49
mismatch: add USD+JPY: currency mismatch
```

### Tests

`TestParseFormatRoundTrip` asserts exact minor units and exact formatted strings per
currency, including that JPY never gains a decimal point and BHD keeps three places.
`TestMismatch` asserts `Add` and `Sub` across currencies return `ErrCurrencyMismatch`.
`TestSignedBounds` asserts a `Sub` that would drive a balance below `math.MinInt64`
returns `ErrOverflow`, and that an in-range subtraction (a negative balance) succeeds.

Create `money_test.go`:

```go
package multicurrency

import (
	"errors"
	"math"
	"testing"
)

func TestParseFormatRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cur       Currency
		text      string
		wantMinor int64
		wantStr   string
	}{
		{JPY, "1000", 1000, "1000"},
		{BHD, "1.500", 1500, "1.500"},
		{USD, "12.99", 1299, "12.99"},
		{USD, "0.05", 5, "0.05"},
		{USD, "-3.20", -320, "-3.20"},
	}
	for _, c := range cases {
		m, err := Parse(c.cur, c.text)
		if err != nil {
			t.Fatalf("Parse(%s, %q): %v", c.cur.Code, c.text, err)
		}
		if m.Minor != c.wantMinor {
			t.Fatalf("Parse(%s, %q) minor = %d, want %d", c.cur.Code, c.text, m.Minor, c.wantMinor)
		}
		if got := m.String(); got != c.wantStr {
			t.Fatalf("Parse(%s, %q).String() = %q, want %q", c.cur.Code, c.text, got, c.wantStr)
		}
	}
}

func TestJPYRejectsDecimals(t *testing.T) {
	t.Parallel()

	if _, err := Parse(JPY, "10.00"); !errors.Is(err, ErrFormat) {
		t.Fatalf("Parse(JPY, 10.00) error = %v, want ErrFormat", err)
	}
	// USD requires exactly two decimals.
	if _, err := Parse(USD, "1.5"); !errors.Is(err, ErrFormat) {
		t.Fatalf("Parse(USD, 1.5) error = %v, want ErrFormat", err)
	}
}

func TestMismatch(t *testing.T) {
	t.Parallel()

	usd, _ := Parse(USD, "10.00")
	yen, _ := Parse(JPY, "100")
	if _, err := Add(usd, yen); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add(USD, JPY) error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := Sub(usd, yen); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub(USD, JPY) error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestSignedBounds(t *testing.T) {
	t.Parallel()

	floor := Money{Minor: math.MinInt64, Cur: USD}
	one := Money{Minor: 1, Cur: USD}
	if _, err := Sub(floor, one); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Sub underflow error = %v, want ErrOverflow", err)
	}
	// A negative balance that stays in range is allowed.
	got, err := Sub(Money{Minor: 100, Cur: USD}, Money{Minor: 300, Cur: USD})
	if err != nil {
		t.Fatalf("Sub in range: unexpected error %v", err)
	}
	if got.Minor != -200 || got.String() != "-2.00" {
		t.Fatalf("Sub = %d (%s), want -200 (-2.00)", got.Minor, got)
	}
}
```

## Review

The type is correct when every operation derives its scale from the currency and never
assumes 100. Confirm the round-trips (`1000` JPY has no decimals, `1.500` BHD keeps
three, `12.99` USD keeps two) and that JPY rejects a decimal point while USD requires
exactly two. The arithmetic is correct when it refuses to mix currencies
(`ErrCurrencyMismatch`) and enforces the signed bounds in both directions — a `Sub`
that would breach `math.MinInt64` returns `ErrOverflow` rather than wrapping a balance
into a large positive number. The mistake this exercise exists to prevent is a
hard-coded two-decimal, `/100` money type: it silently corrupts every currency whose
minor-unit scale is not 2, which is a large fraction of the world's currencies.

## Resources

- [ISO 4217 currency minor units](https://en.wikipedia.org/wiki/ISO_4217) — the per-currency exponents (JPY 0, USD 2, BHD 3).
- [`strconv.ParseInt` / `FormatInt`](https://pkg.go.dev/strconv#ParseInt) — exact integer parsing and formatting for minor units.
- [`strings.CutPrefix`](https://pkg.go.dev/strings#CutPrefix) — the sign-stripping helper used before parsing.
- [`math` constants](https://pkg.go.dev/math#pkg-constants) — `MaxInt64`/`MinInt64`, the signed bounds the guards enforce.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../08-untyped-constants-and-constant-expressions/00-concepts.md](../08-untyped-constants-and-constant-expressions/00-concepts.md)
