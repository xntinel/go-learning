# Exercise 5: Serialize a Money Type Without Float Rounding (MarshalJSON/UnmarshalJSON)

Money must never touch `float64`. `0.1 + 0.2 != 0.3` in binary floating point, and
a billing system that rounds a fraction of a cent per transaction loses real
money and fails audits. The fix is to store money as an integer count of minor
units (cents) and own its JSON representation with `MarshalJSON`/`UnmarshalJSON`,
so it travels as an exact decimal string. This module builds that `Money` type,
and makes it usable as a JSON map key with `TextMarshaler`/`TextUnmarshaler`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
moneyjson/                     independent module: example.com/moneyjson
  go.mod                       go 1.24
  money/
    money.go                   Money{minor int64, currency}; Marshal/Unmarshal JSON + Text
  cmd/
    demo/
      main.go                  marshal amounts and a Money-keyed map
  money/money_test.go          exact round-trips, string-not-number, bad input, map key
```

Files: `money/money.go`, `cmd/demo/main.go`, `money/money_test.go`.
Implement: `Money` over `int64` minor units with `MarshalJSON`/`UnmarshalJSON` (decimal string) and `MarshalText`/`UnmarshalText` (map-key support); a sentinel `ErrInvalidMoney`.
Test: exact round-trip for 0, 100, negative, and a large near-`int64` value; marshaled form is a JSON string not a number; malformed input errors; `Money` works as a map key.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## One canonical text form, four interfaces

The design keeps the internal representation exact and the wire form a string. The
value is an `int64` count of minor units (cents) plus an ISO currency code. A
`String()` method formats that into a canonical decimal-plus-currency text like
`10.50 USD`, and a single `parse` routine reads it back into exact minor units with
no float in the path — integer arithmetic only (`units*100 + cents`).

Four interfaces then delegate to those two:

- `MarshalText`/`UnmarshalText` (`encoding.TextMarshaler`/`TextUnmarshaler`) carry
  the canonical string. Implementing these is what makes `Money` legal as a **JSON
  map key**: JSON object keys must be strings, and `encoding/json` uses a key
  type's `TextMarshaler` to produce the key and its `TextUnmarshaler` to read it
  back. They also make the type reusable by `encoding/xml`, database drivers, and
  anything else that speaks the `encoding` interfaces.
- `MarshalJSON`/`UnmarshalJSON` produce and consume that same text as a *quoted*
  JSON string. `MarshalJSON` calls `json.Marshal(m.String())`, which safely quotes
  and escapes; `UnmarshalJSON` unquotes into a `string` and hands it to `parse`.

A subtle point: `MarshalJSON` delegates to `json.Marshal` on a plain `string`, not
on the `Money` value, so there is no recursion. (Calling `json.Marshal(m)` inside
`Money.MarshalJSON` would re-enter the method forever — the classic custom-marshaler
footgun; here we sidestep it entirely by marshaling the formatted string.)

Because the wire form is a JSON string, a lossy consumer (JavaScript, a
spreadsheet) never sees a float, and the round-trip is bit-exact: the minor-unit
integer that goes out is the exact integer that comes back, including negatives and
values near the `int64` ceiling.

Create `money/money.go`:

```go
// money/money.go
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidMoney is returned when a text/JSON value cannot be parsed as money.
var ErrInvalidMoney = errors.New("invalid money value")

// Money is an exact monetary amount held as int64 minor units (e.g. cents) plus
// a currency code. It never uses float64.
type Money struct {
	minor    int64
	currency string
}

// New builds a Money from minor units (cents) and a currency code.
func New(minor int64, currency string) Money {
	return Money{minor: minor, currency: currency}
}

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the currency code.
func (m Money) Currency() string { return m.currency }

// String renders the canonical "<decimal> <CUR>" form, e.g. "10.50 USD".
func (m Money) String() string {
	v := m.minor
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, v/100, v%100, m.currency)
}

func (m *Money) parse(s string) error {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return fmt.Errorf("%w: %q", ErrInvalidMoney, s)
	}
	amount, currency := fields[0], fields[1]

	neg := strings.HasPrefix(amount, "-")
	amount = strings.TrimPrefix(amount, "-")

	intText, fracText, hasFrac := strings.Cut(amount, ".")
	if !hasFrac {
		fracText = "00"
	}
	if len(fracText) != 2 {
		return fmt.Errorf("%w: need exactly two fractional digits in %q", ErrInvalidMoney, s)
	}
	units, err := strconv.ParseInt(intText, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad integer part in %q", ErrInvalidMoney, s)
	}
	cents, err := strconv.ParseInt(fracText, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad fractional part in %q", ErrInvalidMoney, s)
	}
	minor := units*100 + cents
	if neg {
		minor = -minor
	}
	m.minor, m.currency = minor, currency
	return nil
}

// MarshalText implements encoding.TextMarshaler (also used for JSON map keys).
func (m Money) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (m *Money) UnmarshalText(b []byte) error { return m.parse(string(b)) }

// MarshalJSON encodes money as a quoted JSON string, never a number. It marshals
// the formatted string (not the Money value) to avoid infinite recursion.
func (m Money) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

// UnmarshalJSON parses money from a JSON string.
func (m *Money) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%w: not a JSON string", ErrInvalidMoney)
	}
	return m.parse(s)
}
```

## The runnable demo

The demo marshals a positive amount, a negative amount, and a one-entry map keyed
by `Money`, showing the string-on-the-wire form and the map-key form.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/moneyjson/money"
)

func main() {
	pos, _ := json.Marshal(money.New(1050, "USD"))
	fmt.Printf("positive: %s\n", pos)

	neg, _ := json.Marshal(money.New(-250, "EUR"))
	fmt.Printf("negative: %s\n", neg)

	prices := map[money.Money]int{money.New(999, "USD"): 3}
	m, _ := json.Marshal(prices)
	fmt.Printf("map key:  %s\n", m)

	var back money.Money
	_ = json.Unmarshal([]byte(`"10.50 USD"`), &back)
	fmt.Printf("parsed:   %d minor %s\n", back.Minor(), back.Currency())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
positive: "10.50 USD"
negative: "-2.50 EUR"
map key:  {"9.99 USD":3}
parsed:   1050 minor USD
```

## Tests

`TestRoundTripExact` round-trips 0, 100, a negative, and a value near the `int64`
ceiling, asserting exact minor-unit equality. `TestMarshalsAsString` asserts the
JSON form is a quoted string, not a bare number. `TestRejectsMalformed` feeds
`1.2.3` and an empty string and asserts `ErrInvalidMoney`. `TestMoneyAsMapKey`
round-trips a `map[Money]int` through JSON.

Create `money/money_test.go`:

```go
// money/money_test.go
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRoundTripExact(t *testing.T) {
	t.Parallel()
	cases := []int64{0, 100, -100, -1, 999, 92233720368547758}
	for _, minor := range cases {
		original := New(minor, "USD")
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal %d: %v", minor, err)
		}
		var got Money
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if got.Minor() != minor || got.Currency() != "USD" {
			t.Fatalf("round trip %d: got %d minor %s (%s)", minor, got.Minor(), got.Currency(), data)
		}
	}
}

func TestMarshalsAsString(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(New(100, "USD"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `"`) || !strings.HasSuffix(string(data), `"`) {
		t.Fatalf("money must marshal as a JSON string, got %s", data)
	}
	if string(data) != `"1.00 USD"` {
		t.Fatalf("got %s, want \"1.00 USD\"", data)
	}
}

func TestRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`"1.2.3 USD"`, `""`, `"abc USD"`, `"1.00"`} {
		var m Money
		err := json.Unmarshal([]byte(in), &m)
		if !errors.Is(err, ErrInvalidMoney) {
			t.Fatalf("input %s: expected ErrInvalidMoney, got %v", in, err)
		}
	}
}

func TestMoneyAsMapKey(t *testing.T) {
	t.Parallel()
	in := map[Money]int{New(999, "USD"): 3, New(-50, "EUR"): 1}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[Money]int
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[New(999, "USD")] != 3 || out[New(-50, "EUR")] != 1 {
		t.Fatalf("map round trip failed: %v (%s)", out, data)
	}
}

func ExampleMoney() {
	b, _ := json.Marshal(New(1050, "USD"))
	fmt.Println(string(b))
	// Output: "10.50 USD"
}
```

## Review

The type is correct when every amount round-trips to the exact same `int64`,
including negatives and values near the `int64` ceiling, and when the wire form is
always a quoted string — never a JSON number that a float consumer could round. The
two custom-marshaler traps this module inoculates against are recursion (broken by
marshaling the formatted `string`, not the `Money`) and map keys (solved by
`TextMarshaler`, since `MarshalJSON` is not consulted for object keys). Keep the
`parse` path integer-only; the instant a `strconv.ParseFloat` sneaks in, you have
reintroduced the rounding bug the whole type exists to prevent.

## Resources

- [`encoding/json.Marshaler`](https://pkg.go.dev/encoding/json#Marshaler) — `MarshalJSON`/`UnmarshalJSON` and how a type owns its JSON form.
- [`encoding.TextMarshaler`](https://pkg.go.dev/encoding#TextMarshaler) — the text interfaces that enable JSON map keys and cross-encoder reuse.
- [`strconv.ParseInt`](https://pkg.go.dev/strconv#ParseInt) — exact integer parsing with no floating point.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-absent-null-zero-patch-semantics.md](04-absent-null-zero-patch-semantics.md) | Next: [06-secret-redaction-via-marshaljson.md](06-secret-redaction-via-marshaljson.md)
