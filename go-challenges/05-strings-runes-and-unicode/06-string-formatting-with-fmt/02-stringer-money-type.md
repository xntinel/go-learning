# Exercise 2: A Safe fmt.Stringer for a Money Type in Logs

A payments service passes `Money` values through dozens of log lines and error
messages. If `Money` renders as a raw struct (`{1234 USD}`), every log is
unreadable; if its `String()` is written naively, the service crashes with a stack
overflow. This exercise builds the correct, recursion-safe `Stringer`.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
money/                     independent module: example.com/money
  go.mod                   go 1.24
  money.go                 type Money struct{Cents int64; Currency string}; String()
  cmd/
    demo/
      main.go              runnable demo: %v and %s of several amounts
  money_test.go            String()/verb-routing/recursion-regression/sign-edge tests
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Money{Cents int64; Currency string}` with `String() string` rendering `12.34 USD`, handling zero, negative, and sub-unit amounts, built from the integer fields (no `Sprintf("%v", m)`).
- Test: `String()` output for positive/zero/negative and several currencies; `%v` and `%s` route through `String()`; a regression test that formatting never recurses (bounded output vs. a hand-built expected string); a table of sign/rounding edge cases.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The recursion trap and how the fields avoid it

The whole point of a `Stringer` is that `%v` and `%s` dispatch to `String()`. That
dispatch is also the trap: if `String()` itself formats the receiver with a
value-dispatching verb — `fmt.Sprintf("%v", m)` or `fmt.Sprint(m)` — `fmt` sees
that `m` has a `String()` method, calls it, which calls `Sprintf("%v", m)` again,
and the recursion never bottoms out. It is not a subtle slowdown; it is an
immediate stack overflow the first time anything logs a `Money`.

The fix is to never hand the receiver to a value-dispatching verb inside its own
method. Instead, format the *fields*, which are `int64` and `string` and have no
`String()` method of their own:

- Split the signed cent count into a sign, whole units, and fractional cents.
  Take the absolute value first so the sign is handled once, at the front, rather
  than leaking a stray minus into the fractional part.
- Render the whole part with `strconv.FormatInt` (a direct, allocation-light
  integer conversion) and the fractional part with `%02d` so `5` cents becomes
  `.05`, not `.5`.
- Concatenate `sign + whole + "." + frac + " " + Currency`.

`%02d` is doing real work: the fractional cents are 0-99, and without the zero-pad
`5` would render as `5` giving `0.5 USD` (fifty cents) instead of `0.05 USD` (five
cents) — a real money bug. The alternative recursion-safe trick, when you want to
delegate to the default formatter, is to convert to a defined type that lacks the
method: `type plain Money; return fmt.Sprintf("%v", plain(m))`. Here the explicit
field formatting is clearer, so we use it.

Create `money.go`:

```go
package money

import (
	"fmt"
	"strconv"
)

// Money is a minor-unit amount in a currency. 1234 cents in "USD" is 12.34 USD.
// It is a value type so it is safe to copy and log freely.
type Money struct {
	Cents    int64
	Currency string
}

// String renders the amount as "<major>.<minor> <CURRENCY>", e.g. "12.34 USD".
// It formats the integer fields directly and never formats the receiver with a
// value-dispatching verb, so it cannot recurse into itself.
func (m Money) String() string {
	sign := ""
	c := m.Cents
	if c < 0 {
		sign = "-"
		c = -c
	}
	whole := c / 100
	frac := c % 100
	return sign + strconv.FormatInt(whole, 10) + "." + fmt.Sprintf("%02d", frac) + " " + m.Currency
}
```

### The runnable demo

The demo formats several amounts with both `%v` and `%s` to show they route
through the same `String()`, including the sub-unit and negative (refund) cases.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	amounts := []money.Money{
		{Cents: 1234, Currency: "USD"},
		{Cents: 0, Currency: "USD"},
		{Cents: 5, Currency: "USD"},
		{Cents: -500, Currency: "EUR"},
		{Cents: 100000, Currency: "JPY"},
	}
	for _, m := range amounts {
		fmt.Printf("%%v=%v  %%s=%s\n", m, m)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
%v=12.34 USD  %s=12.34 USD
%v=0.00 USD  %s=0.00 USD
%v=0.05 USD  %s=0.05 USD
%v=-5.00 EUR  %s=-5.00 EUR
%v=1000.00 JPY  %s=1000.00 JPY
```

### Tests

The table test pins `String()` for positive, zero, sub-unit, negative, and large
amounts across currencies. `TestVerbsRouteThroughString` asserts `%v` and `%s`
produce exactly what `String()` returns — the dispatch contract. The important one
is `TestNoRecursion`: it formats a `Money` and compares against a hand-built
expected string. If `String()` had called `Sprintf("%v", m)`, this test would not
merely fail — it would overflow the stack and crash the whole test binary, which
is itself the signal. Comparing against a hand-built string (not against
`m.String()`) is deliberate: it fixes the expected output independently of the
method under test.

Create `money_test.go`:

```go
package money

import (
	"fmt"
	"testing"
)

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    Money
		want string
	}{
		{"dollars and cents", Money{Cents: 1234, Currency: "USD"}, "12.34 USD"},
		{"zero", Money{Cents: 0, Currency: "USD"}, "0.00 USD"},
		{"sub unit", Money{Cents: 5, Currency: "USD"}, "0.05 USD"},
		{"single cent short of a unit", Money{Cents: 99, Currency: "USD"}, "0.99 USD"},
		{"negative refund", Money{Cents: -500, Currency: "EUR"}, "-5.00 EUR"},
		{"negative sub unit", Money{Cents: -5, Currency: "EUR"}, "-0.05 EUR"},
		{"large amount", Money{Cents: 100000, Currency: "JPY"}, "1000.00 JPY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.m.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerbsRouteThroughString(t *testing.T) {
	t.Parallel()

	m := Money{Cents: 1234, Currency: "USD"}
	if got := fmt.Sprintf("%v", m); got != "12.34 USD" {
		t.Fatalf("%%v = %q, want 12.34 USD", got)
	}
	if got := fmt.Sprintf("%s", m); got != "12.34 USD" {
		t.Fatalf("%%s = %q, want 12.34 USD", got)
	}
}

// TestNoRecursion proves String() does not format its own receiver with a
// value-dispatching verb. If it did, this call would overflow the stack rather
// than return; a bounded, correct result is the proof it is recursion-safe.
func TestNoRecursion(t *testing.T) {
	t.Parallel()

	m := Money{Cents: 4200, Currency: "GBP"}
	got := fmt.Sprintf("balance=%v", m)
	const want = "balance=42.00 GBP"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func Example() {
	m := Money{Cents: 999, Currency: "USD"}
	fmt.Printf("total %s\n", m)
	// Output: total 9.99 USD
}
```

## Review

The type is correct when `String()` is a pure function of its two fields, produces
`major.minor CURRENCY` with a zero-padded two-digit minor part, and places the
sign once at the front. The `%02d` on the fractional part is load-bearing: without
it, `5` cents renders as `0.5` and every sub-unit amount is off by a factor of ten.
The recursion regression is the trap this exercise exists to teach — the fix is
that `String()` formats `int64`/`string` fields, never the `Money` receiver with
`%v`/`%s`/`Sprint`. If you did want to delegate to the default formatter, convert
to a `type plain Money` first so the method is not on the value being formatted.
Run `go test -race`; the type is immutable so there is nothing to race, and the
test confirms it.

## Resources

- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the `String()` interface and which verbs dispatch to it.
- [`strconv.FormatInt`](https://pkg.go.dev/strconv#FormatInt) — direct integer-to-string conversion.
- [Go Code Review Comments: String method](https://go.dev/wiki/CodeReviewComments#stringmethod) — the official warning about `Sprintf` recursing into `String`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-formatter-secret-redaction.md](03-formatter-secret-redaction.md)
