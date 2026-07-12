# Exercise 5: Custom fmt.Formatter For A Money Type With Verb, Width, And Flag Control

When one representation is not enough — when a ledger needs `$12.34` under `%s`,
raw minor units under `%d`, the currency code under `%+v`, and column alignment
under `%8s` — you escalate from `Stringer` to `fmt.Formatter`. This module builds a
`Money` type that owns its formatting across verbs, flags, and width.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
money/                      independent module: example.com/money
  go.mod
  money.go                  type Money; Format(fmt.State, rune) over %s %d %+v %#v + width
  cmd/
    demo/
      main.go               prints the same amount under several verbs and widths
  money_test.go             table over verb+flag+width; negative case; go vet printf-clean
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Money{Minor int64; Currency string}` with `Format(f fmt.State, verb rune)` so `%s`/`%v` print `$12.34`, `%d` prints raw minor units, `%+v` appends the currency code, `%#v` prints Go syntax, and `Width()`/the `-` flag drive padding.
- Test: a table over `%s`, `%d`, `%+v`, `%#v`, `%8s`, `%-8s`; padding applied when `Width()` is set; `+` toggles the currency code; a negative amount; `go vet` printf checker clean.
- Verify: `go test -count=1 -race ./...` and `go vet ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/10-implementing-stringer/05-fmt-formatter-verbs-and-flags/cmd/demo
cd go-solutions/07-structs-and-methods/10-implementing-stringer/05-fmt-formatter-verbs-and-flags
```

### Why Formatter, and how it takes over

`Money` needs verb-dependent output, which a single `String()` cannot give. So it
implements `fmt.Formatter`: `Format(f fmt.State, verb rune)`. Once a type
implements `Formatter`, `fmt` routes *every* verb through it — `%s`, `%d`, `%v`,
`%#v`, all of them — passing the verb rune and a `fmt.State`. `Formatter` takes
precedence over both `Stringer` and `GoStringer`, so the type has total control and
must handle each verb it cares about (and produce a sane fallback for the rest,
mirroring what `fmt` does for an unknown verb).

`fmt.State` is the formatting context. It is an `io.Writer` (so `f.Write` and
`io.WriteString(f, ...)` emit output), plus three inspectors: `Width() (int, bool)`
returns the requested field width and whether one was given, `Precision() (int, bool)`
the same for precision, and `Flag(c int) bool` reports whether a flag rune such as
`'+'`, `'-'`, `'#'`, `' '`, or `'0'` was present. Because the type is now doing the
formatting, `fmt` does *not* apply width padding for you — the `Format` method must
read `Width()` and pad itself. That is the price of taking over.

The verb map here: `'d'` writes the raw minor-unit integer (useful for exact
ledger math and for `SUM()` in exports); `'s'` and `'v'` write the money string
`$12.34`; the `'+'` flag on any of those appends the currency code; the `'#'` flag
on `'v'` (`%#v`) writes a Go-syntax representation for debugging. Money carries a
fixed two-decimal scale (minor units are hundredths), so there is no precision knob
to honor — but width and flags very much apply, and a real invoice renderer needs
them for column alignment.

Create `money.go`:

```go
package money

import (
	"fmt"
	"io"
	"strconv"
)

// Money is an exact monetary amount: an integer count of minor units (cents for
// USD) plus an ISO currency code. Storing minor units as int64 avoids float
// rounding error in ledgers.
type Money struct {
	Minor    int64
	Currency string
}

// Format implements fmt.Formatter. It handles %d (raw minor units), %s and %v
// (the $-formatted amount), the '+' flag (append currency code), and %#v (Go
// syntax), honoring field width and the '-' left-align flag itself.
func (m Money) Format(f fmt.State, verb rune) {
	switch verb {
	case 'd':
		m.writePadded(f, strconv.FormatInt(m.Minor, 10))
	case 'v':
		if f.Flag('#') {
			m.writePadded(f, m.goSyntax())
			return
		}
		fallthrough
	case 's':
		s := m.amount()
		if f.Flag('+') {
			s += " " + m.Currency
		}
		m.writePadded(f, s)
	default:
		// Unknown verb: mirror fmt's bad-verb marker without recursing.
		io.WriteString(f, "%!"+string(verb)+"(money="+m.amount()+")")
	}
}

// amount renders the value as a signed $-string with two decimals, e.g. "$12.34"
// or "-$5.00".
func (m Money) amount() string {
	minor := m.Minor
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return fmt.Sprintf("%s$%d.%02d", sign, minor/100, minor%100)
}

// goSyntax renders a Go-literal form for %#v.
func (m Money) goSyntax() string {
	return fmt.Sprintf("money.Money{Minor:%d, Currency:%q}", m.Minor, m.Currency)
}

// writePadded emits s honoring the State's width and '-' (left-align) flag. When
// the type formats itself, fmt does not pad for it, so it pads here.
func (m Money) writePadded(f fmt.State, s string) {
	w, ok := f.Width()
	if !ok || w <= len(s) {
		io.WriteString(f, s)
		return
	}
	pad := w - len(s)
	if f.Flag('-') {
		io.WriteString(f, s)
		writeSpaces(f, pad)
		return
	}
	writeSpaces(f, pad)
	io.WriteString(f, s)
}

func writeSpaces(f fmt.State, n int) {
	const spaces = "                                "
	for n > 0 {
		chunk := min(n, len(spaces))
		io.WriteString(f, spaces[:chunk])
		n -= chunk
	}
}
```

### The runnable demo

The demo prints one amount under every verb the type handles, plus width variants
and a negative amount, so the whole `Format` surface is visible in one run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	m := money.Money{Minor: 1234, Currency: "USD"}
	fmt.Printf("%%s   = %s\n", m)
	fmt.Printf("%%d   = %d\n", m)
	fmt.Printf("%%+v  = %+v\n", m)
	fmt.Printf("%%#v  = %#v\n", m)
	fmt.Printf("%%8s  = [%8s]\n", m)
	fmt.Printf("%%-8s = [%-8s]\n", m)

	neg := money.Money{Minor: -500, Currency: "USD"}
	fmt.Printf("neg   = %s\n", neg)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
%s   = $12.34
%d   = 1234
%+v  = $12.34 USD
%#v  = money.Money{Minor:1234, Currency:"USD"}
%8s  = [  $12.34]
%-8s = [$12.34  ]
neg   = -$5.00
```

### Tests

The table drives each verb+flag+width combination through `fmt.Sprintf` and
compares the exact output — which is the only way to pin `Format` behavior, since
the method's whole job is producing bytes. The negative case guards the sign logic,
and `go vet`'s printf checker (run in verification) confirms the format strings are
well-formed and the argument counts match.

Create `money_test.go`:

```go
package money

import (
	"fmt"
	"testing"
)

var _ fmt.Formatter = Money{}

func TestFormat(t *testing.T) {
	t.Parallel()
	m := Money{Minor: 1234, Currency: "USD"}
	neg := Money{Minor: -500, Currency: "USD"}
	tests := []struct {
		name   string
		format string
		arg    Money
		want   string
	}{
		{"string", "%s", m, "$12.34"},
		{"default", "%v", m, "$12.34"},
		{"minor", "%d", m, "1234"},
		{"plus", "%+v", m, "$12.34 USD"},
		{"gosyntax", "%#v", m, `money.Money{Minor:1234, Currency:"USD"}`},
		{"width", "%8s", m, "  $12.34"},
		{"leftwidth", "%-8s", m, "$12.34  "},
		{"widewidth", "%12s", m, "      $12.34"},
		{"negative", "%s", neg, "-$5.00"},
		{"negplus", "%+v", neg, "-$5.00 USD"},
		{"zero", "%s", Money{Minor: 0, Currency: "EUR"}, "$0.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fmt.Sprintf(tc.format, tc.arg); got != tc.want {
				t.Errorf("Sprintf(%q) = %q, want %q", tc.format, got, tc.want)
			}
		})
	}
}

func ExampleMoney_Format() {
	m := Money{Minor: 999, Currency: "GBP"}
	fmt.Printf("%s / %d / %+v\n", m, m, m)
	// Output: $9.99 / 999 / $9.99 GBP
}
```

## Review

`Formatter` is the right tool precisely when one string cannot serve every caller:
the ledger sums minor units (`%d`), the invoice shows dollars (`%s`), the audit
log wants the currency (`%+v`), and the debugger wants Go syntax (`%#v`). The
non-obvious responsibility is width: once the type formats itself, `fmt` stops
padding for it, so `writePadded` must read `Width()` and the `-` flag and do the
padding by hand — forget this and `%8s` silently produces an unpadded string. Keep
the `amount()` and `goSyntax()` helpers pure, and make sure the default arm emits a
visible bad-verb marker rather than nothing, so a stray `%x` in a log format is
diagnosable. Do not reach for `Formatter` when a single `String()` would do; the
cost is exactly this manual verb-and-width handling, justified only when the type
genuinely has several representations.

## Resources

- [fmt: Formatter](https://pkg.go.dev/fmt#Formatter) — the interface and its precedence over Stringer.
- [fmt: State](https://pkg.go.dev/fmt#State) — `Write`, `Width`, `Precision`, `Flag`.
- [fmt package overview: format flags](https://pkg.go.dev/fmt#hdr-Printing) — what `+`, `-`, `#`, `0` mean.

---

Back to [04-logvaluer-redaction.md](04-logvaluer-redaction.md) | Next: [06-sql-valuer-scanner-enum-column.md](06-sql-valuer-scanner-enum-column.md)
