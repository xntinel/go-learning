# Exercise 1: Parse Money Text into Integer Minor Units Without float64

Every amount that enters a payments backend arrives as text — an HTTP form field,
a CSV cell, a JSON literal. The first thing you do with it decides whether the rest
of the system can be exact. This exercise builds `ParseCents`, the trust-boundary
parser that turns an untrusted decimal string into exact integer cents, rejecting
ambiguous precision and detecting overflow before it wraps.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
parsecents/                  independent module: example.com/parsecents
  go.mod                     module path
  money.go                   type Cents int64; ParseCents; String; ErrOverflow, ErrFormat
  cmd/
    demo/
      main.go                parses a few decimal strings, prints exact cents
  money_test.go              exact input->Cents table, rejection table, overflow via errors.Is
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: `ParseCents(raw string) (Cents, error)` that requires exactly two
fractional digits, rejects empty/negative/ambiguous input, and returns
`ErrOverflow` before the `dollars*100 + cents` combination wraps; plus a `String()`
method that renders `Cents` back to a signed decimal.
Test: a table of exact `input -> Cents` mappings, a rejection table, and an
explicit `ParseCents("92233720368547758.08")` returning `ErrOverflow` via
`errors.Is`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/01-parse-cents-decimal-parser/cmd/demo
cd go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/01-parse-cents-decimal-parser
```

### Why the parser is the trust boundary

The invariant this package guarantees is that once an amount is a `Cents`, it is an
exact integer number of minor units — no `float64` ever touched it. That invariant
is only as strong as its single entry point. `ParseCents` therefore does all of the
policy in one place: it trims surrounding whitespace, rejects the empty string and a
leading `-` (negative amounts are a policy this package does not accept as input),
splits the whole and fractional parts on the decimal point with `strings.Cut`, and
requires *exactly two* fractional digits. That last rule is deliberate and strict.
`"12.9"` is rejected because a human writing it might mean 12 dollars and 9 cents or
12 dollars and 90 cents — the parser refuses to guess. `"12.999"` is rejected
because a third digit is sub-cent precision this representation cannot hold, and
silently dropping it would be a lie about what was received.

The whole and fractional parts are parsed with `strconv.ParseInt` in base 10 with a
64-bit width, so a non-numeric character is a parse error rather than a silent zero.
The one arithmetic step, `dollars*100 + cents`, is the place an adversarial input can
overflow: a string like `"92233720368547758.08"` has a whole part just under
`math.MaxInt64 / 100`, and multiplying by 100 would wrap. The guard
`dollars > (math.MaxInt64 - cents) / 100` rearranges the overflow condition so the
comparison itself never overflows — it computes the largest whole-dollar value that
still leaves room for `cents`, and rejects anything larger with `ErrOverflow`. This
is the check-before-operating rule applied at the boundary: the wrap is prevented,
not detected.

`String()` is the inverse rendering. It records the sign, works on the absolute
value, and formats `value/100` and `value%100` with `%d.%02d` so a value of `105`
prints as `1.05`, not `1.5`. It is used by every downstream exercise that formats
money, and by the tests here to assert exact round-trips.

Create `money.go`:

```go
package parsecents

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrOverflow is returned when an amount cannot be represented in int64 cents.
var ErrOverflow = errors.New("money overflow")

// ErrFormat is returned when the decimal text is malformed or ambiguous.
var ErrFormat = errors.New("money format")

// Cents is an exact amount in integer minor units (hundredths).
type Cents int64

// ParseCents converts a decimal string with exactly two fractional digits into
// integer cents, failing closed on empty, negative, ambiguous, or overflowing
// input. No float64 is ever involved.
func ParseCents(raw string) (Cents, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty amount: %w", ErrFormat)
	}
	if strings.HasPrefix(raw, "-") {
		return 0, fmt.Errorf("negative amount %q not supported: %w", raw, ErrFormat)
	}

	whole, frac, hasFrac := strings.Cut(raw, ".")
	if !hasFrac {
		frac = "00"
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) != 2 {
		return 0, fmt.Errorf("amount %q needs exactly two decimals: %w", raw, ErrFormat)
	}

	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse whole part %q: %w", whole, ErrFormat)
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse fractional part %q: %w", frac, ErrFormat)
	}
	if dollars > (math.MaxInt64-cents)/100 {
		return 0, fmt.Errorf("amount %q exceeds int64 cents: %w", raw, ErrOverflow)
	}

	return Cents(dollars*100 + cents), nil
}

// String renders cents as a signed decimal with two fractional digits.
func (c Cents) String() string {
	sign := ""
	v := c
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/parsecents"
)

func main() {
	for _, raw := range []string{"0", "12.99", "001.05", "100000"} {
		c, err := parsecents.ParseCents(raw)
		if err != nil {
			fmt.Printf("%-8s -> error: %v\n", raw, err)
			continue
		}
		fmt.Printf("%-8s -> %d cents (%s)\n", raw, int64(c), c)
	}

	if _, err := parsecents.ParseCents("92233720368547758.08"); err != nil {
		fmt.Println("overflow guarded:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
0        -> 0 cents (0.00)
12.99    -> 1299 cents (12.99)
001.05   -> 105 cents (1.05)
100000   -> 10000000 cents (100000.00)
overflow guarded: amount "92233720368547758.08" exceeds int64 cents: money overflow
```

### Tests

The tests assert exact integers and error identity — never a float comparison, never
a printed-output check. The mapping table pins the semantics (`"0"` is zero, leading
zeros are ignored, a whole number gets two zero cents). The rejection table proves
the parser fails closed on the four ambiguous inputs. The overflow case asserts the
boundary is caught with `errors.Is(err, ErrOverflow)`, and a companion case one unit
lower proves the boundary is exactly placed.

Create `money_test.go`:

```go
package parsecents

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseCentsExact(t *testing.T) {
	t.Parallel()

	tests := map[string]Cents{
		"0":       0,
		"12.99":   1299,
		"001.05":  105,
		"100000":  10000000,
		"  7.00 ": 700,
	}
	for input, want := range tests {
		got, err := ParseCents(input)
		if err != nil {
			t.Fatalf("ParseCents(%q): unexpected error %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseCents(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestParseCentsRejects(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "12.9", "12.999", "-1.00", "1.2x"} {
		if _, err := ParseCents(input); !errors.Is(err, ErrFormat) {
			t.Fatalf("ParseCents(%q) error = %v, want ErrFormat", input, err)
		}
	}
}

func TestParseCentsOverflow(t *testing.T) {
	t.Parallel()

	if _, err := ParseCents("92233720368547758.08"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("ParseCents overflow error = %v, want ErrOverflow", err)
	}
	// One cent under the boundary parses exactly.
	got, err := ParseCents("92233720368547758.07")
	if err != nil {
		t.Fatalf("ParseCents at boundary-1: unexpected error %v", err)
	}
	if int64(got) != 9223372036854775807 {
		t.Fatalf("boundary value = %d, want math.MaxInt64", int64(got))
	}
}

func ExampleParseCents() {
	c, _ := ParseCents("12.99")
	fmt.Println(int64(c), c.String())
	// Output: 1299 12.99
}
```

## Review

The parser is correct when it is the only door into the `Cents` type and every path
through it either returns an exact integer or a wrapped sentinel error. Confirm the
semantics with the exact-mapping table (leading zeros collapse, a bare integer gains
`.00`), the rejection table (`errors.Is(err, ErrFormat)` for every ambiguous input),
and the overflow boundary (`errors.Is(err, ErrOverflow)` at `...758.08`, exact parse
at `...758.07`). The subtle correctness point is the overflow guard: it is written as
`dollars > (math.MaxInt64 - cents) / 100` precisely so the comparison never performs
the multiplication that would overflow. Rewriting it as `dollars*100 + cents >
math.MaxInt64` would wrap first and defeat the check — that is the mistake this
exercise exists to prevent.

## Resources

- [Go Specification: Integer overflow](https://go.dev/ref/spec#Integer_overflow) — the wraparound semantics the guard defends against.
- [`strconv.ParseInt`](https://pkg.go.dev/strconv#ParseInt) — base-10, 64-bit parsing that errors instead of returning a silent zero.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — the single-split idiom used to separate whole and fractional parts.
- [`math` constants](https://pkg.go.dev/math#pkg-constants) — `MaxInt64` and the other integer limits.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-checked-add-multiply-arithmetic.md](02-checked-add-multiply-arithmetic.md)
