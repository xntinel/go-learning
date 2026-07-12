# Exercise 2: Money as a Defined Integer Type in a Billing Ledger

Money in a backend must never be a bare `int64` or (worse) a `float64`: someone
will eventually add a raw item count to a balance, or let binary floating point
round a total to `$19.989999999`. This exercise models a ledger amount as
`type Cents int64` with arithmetic that stays in `Cents`, a `Format()` that
renders a dollar string, and a single parse boundary that centralizes rounding.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
money/                    independent module: example.com/money
  go.mod                  go 1.24
  money.go                type Cents int64; Add, Sub, Neg, Mul(int), Format;
                          ParseCents(decimal string); ErrMalformedAmount
  cmd/
    demo/
      main.go             parses a price, applies quantity, formats a total
  money_test.go           table tests: arithmetic, formatting, parse rejection
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Cents` with `Add`, `Sub`, `Neg`, `Mul(quantity int)`, `Format()`, and `ParseCents(s string) (Cents, error)`.
- Test: arithmetic stays in `Cents`; `Format()` renders negatives and sub-dollar values; `ParseCents` rejects malformed and over-precision input via a wrapped sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/02-money-cents-defined-type/cmd/demo
cd go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/02-money-cents-defined-type
go mod edit -go=1.24
```

### Why a defined type, and where rounding lives

`type Cents int64` is an integer count of the smallest currency unit, so there is
no floating-point representation error to accumulate — a balance is always an exact
number of cents. Making it a *defined* type (not an alias of `int64`) means the
compiler rejects `ledger + 5`, where `5` is a raw `int`, and rejects adding a
`float64` price directly. You cannot mix a monetary value with an unrelated
quantity without an explicit conversion, and the only blessed way to turn a
human-entered decimal string into `Cents` is `ParseCents`, which is the one place
rounding and precision rules live.

The arithmetic methods return `Cents`, so a whole expression stays typed:
`price.Mul(qty).Sub(discount)` never leaves the money domain. `Mul` takes a plain
`int` quantity deliberately — multiplying money by a count is meaningful (three
widgets at `$4.99`), while multiplying money by money is not, so the signature
encodes that.

`ParseCents` is strict: it accepts an optional sign, an integer part, and at most
two fractional digits, and it rejects anything else — including three-decimal
"prices" that would silently lose a tenth of a cent. Rejecting over-precision at
the boundary is a feature: it forces the caller to decide how to round *before*
the value enters the ledger, rather than discovering a rounding surprise later.

Create `money.go`:

```go
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedAmount is the sentinel returned when a decimal string cannot be
// parsed into an exact number of cents. Callers match it with errors.Is.
var ErrMalformedAmount = errors.New("malformed amount")

// Cents is a monetary value as a whole number of cents. It is a DEFINED type, so
// it cannot be mixed with a raw int or a float64 without an explicit conversion.
type Cents int64

// Add returns the sum of two amounts, staying in Cents.
func (c Cents) Add(o Cents) Cents { return c + o }

// Sub returns c minus o, staying in Cents.
func (c Cents) Sub(o Cents) Cents { return c - o }

// Neg returns the additive inverse.
func (c Cents) Neg() Cents { return -c }

// Mul scales an amount by an integer quantity (e.g. unit price times count).
func (c Cents) Mul(quantity int) Cents { return c * Cents(quantity) }

// Format renders the amount as a signed dollar string, e.g. "$1.50", "-$25.99",
// "$0.05". The sign is placed before the currency symbol.
func (c Cents) Format() string {
	n := int64(c)
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	return fmt.Sprintf("%s$%d.%02d", sign, n/100, n%100)
}

// ParseCents converts a decimal string like "19.99", "-4", or "0.05" into Cents.
// It accepts an optional leading sign, an integer part, and at most two
// fractional digits; anything else (extra dots, non-digits, over-precision) is
// rejected with an error wrapping ErrMalformedAmount.
func ParseCents(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrMalformedAmount)
	}

	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}

	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrMalformedAmount)
	}

	dollars, err := strconv.ParseInt(nonEmpty(whole), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", s, ErrMalformedAmount)
	}

	var cents int64
	if hasDot {
		if len(frac) == 0 || len(frac) > 2 {
			return 0, fmt.Errorf("parse amount %q: %w", s, ErrMalformedAmount)
		}
		if len(frac) == 1 {
			frac += "0"
		}
		cents, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse amount %q: %w", s, ErrMalformedAmount)
		}
	}

	total := dollars*100 + cents
	if neg {
		total = -total
	}
	return Cents(total), nil
}

// nonEmpty maps "" to "0" so ".5" parses its (missing) integer part as zero.
func nonEmpty(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
```

### The safety win you cannot test

The line that will not compile is the whole point:

```go
// var total Cents = price + 5 // does not compile: 5 is int, not Cents
```

To add five cents you must say `price.Add(5)` (an untyped constant `5` converts
to `Cents`) or `price + Cents(5)` — an explicit, visible conversion. A raw `int`
variable or a `float64` cannot slip into a monetary expression by accident.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	unit, err := money.ParseCents("4.99")
	if err != nil {
		panic(err)
	}

	subtotal := unit.Mul(3) // three units
	tax, _ := money.ParseCents("1.20")
	total := subtotal.Add(tax)
	refund := total.Neg()

	fmt.Println("unit:    ", unit.Format())
	fmt.Println("subtotal:", subtotal.Format())
	fmt.Println("total:   ", total.Format())
	fmt.Println("refund:  ", refund.Format())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unit:     $4.99
subtotal: $14.97
total:    $16.17
refund:   -$16.17
```

### Tests

The tests cover arithmetic staying in `Cents`, `Format` rendering negatives and
sub-dollar amounts, and `ParseCents` accepting the legal shapes while rejecting
malformed and over-precision input. The rejection tests assert the wrapped
sentinel with `errors.Is`, which is how a caller distinguishes "bad amount" from
some other failure.

Create `money_test.go`:

```go
package money

import (
	"errors"
	"fmt"
	"testing"
)

func TestArithmetic(t *testing.T) {
	t.Parallel()

	price := Cents(499)
	if got := price.Mul(3); got != Cents(1497) {
		t.Errorf("Mul = %d, want 1497", got)
	}
	if got := price.Add(Cents(1)); got != Cents(500) {
		t.Errorf("Add = %d, want 500", got)
	}
	if got := price.Sub(Cents(500)); got != Cents(-1) {
		t.Errorf("Sub = %d, want -1", got)
	}
	if got := price.Neg(); got != Cents(-499) {
		t.Errorf("Neg = %d, want -499", got)
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   Cents
		want string
	}{
		{0, "$0.00"},
		{5, "$0.05"},
		{150, "$1.50"},
		{1600, "$16.00"},
		{-2599, "-$25.99"},
		{-5, "-$0.05"},
	}
	for _, tc := range tests {
		if got := tc.in.Format(); got != tc.want {
			t.Errorf("Cents(%d).Format() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseCents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Cents
		wantErr bool
	}{
		{"19.99", 1999, false},
		{"4", 400, false},
		{"0.05", 5, false},
		{"0.5", 50, false},
		{".5", 50, false},
		{"-25.99", -2599, false},
		{"+7.00", 700, false},
		{"  12.34  ", 1234, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.234", 0, true}, // over precision
		{"1.2.3", 0, true}, // extra dot
		{"1.", 0, true},    // dot with no fraction
		{"1.2x", 0, true},  // non-digit fraction
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCents(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedAmount) {
					t.Fatalf("ParseCents(%q) err = %v, want ErrMalformedAmount", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCents(%q) unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseCents(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleCents_Format() {
	c, _ := ParseCents("-25.99")
	fmt.Println(c.Format())
	// Output: -$25.99
}
```

## Review

The type is correct when a whole billing expression stays in `Cents` without a
single float, when `Format` handles the sign and the sub-dollar `%02d` padding,
and when `ParseCents` refuses anything it cannot represent exactly — most
importantly a three-decimal amount, which must be a caller decision, not a silent
truncation. The classic mistake is modeling money as `float64` "because prices
have decimals"; binary floating point cannot represent `0.10` exactly and totals
drift. The second mistake is aliasing (`type Cents = int64`), which throws away the
compile error on `balance + rawCount`. Assert the wrapped sentinel with
`errors.Is`, not by string-matching the message.

## Resources

- [Go Language Spec: Type definitions](https://go.dev/ref/spec#Type_definitions) — defined types and their distinctness.
- [`strconv.ParseInt`](https://pkg.go.dev/strconv#ParseInt) — the exact base/bit-size semantics used at the parse boundary.
- [`errors.Is` and wrapping with `%w`](https://pkg.go.dev/errors#Is) — matching a sentinel through a wrapped error.

---

Prev: [01-domain-ids-and-legacy-alias.md](01-domain-ids-and-legacy-alias.md) | Next: [03-public-api-package-migration-alias.md](03-public-api-package-migration-alias.md)
