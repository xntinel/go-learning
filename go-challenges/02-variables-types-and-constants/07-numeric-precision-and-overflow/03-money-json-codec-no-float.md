# Exercise 3: Decode API Money Fields Without Losing Precision to float64

A payment API receives `{"amount":"12.99"}` over the wire. If the handler decodes
that amount into a `float64`, precision is already gone before any business logic
runs. This exercise builds a `Money` type with custom `json.Marshaler` /
`json.Unmarshaler` that routes the exact JSON literal through an integer parser, and
a decoder helper that uses `Decoder.UseNumber()` — so the boundary never sees a
`float64`.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
moneyjson/                   independent module: example.com/moneyjson
  go.mod                     module path
  money.go                   type Money; MarshalJSON, UnmarshalJSON; ParseCents; DecodeAmounts
  cmd/
    demo/
      main.go                round-trips a request, shows float64 loss vs UseNumber
  money_test.go              round-trip exact, UseNumber preserves literal, malformed errors
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: `Money` (an `int64` cents alias) with `UnmarshalJSON` that accepts the
value as an exact literal (string or JSON number) and routes it through `ParseCents`,
and `MarshalJSON` that emits the canonical two-decimal string; plus
`DecodeAmounts(r io.Reader) ([]Money, error)` using `Decoder.UseNumber()`.
Test: round-trip `{"amount":"12.99"}` and a large amount through Unmarshal/Marshal
and assert exact cents and re-emitted string; show that a bare `0.1` preserved as
`json.Number` differs from the `float64` an `interface{}` decode produces; assert a
malformed amount errors instead of silently zeroing.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/moneyjson/cmd/demo
cd ~/go-exercises/moneyjson
go mod init example.com/moneyjson
```

### Why the default decode is dangerous, and how the custom codec fixes it

`encoding/json` maps a JSON number onto `float64` whenever the destination is
`interface{}` or a `map[string]any`. That is the trap: a handler that decodes a body
into `map[string]any` and reads `body["amount"].(float64)` has already lost every
sub-cent decimal and every integer above `2^53`. The literal `0.1` in the JSON text
becomes the `float64` nearest to 0.1, whose exact value is
`0.1000000000000000055511151231257827021181583404541015625` — not the decimal the
sender wrote.

The fix is to never let a `float64` stand between the wire and your integer
representation. `Money.UnmarshalJSON` receives the *raw bytes* of the value, whatever
its JSON kind. If the value is a JSON string (`"12.99"`), it unquotes it; otherwise
it treats the raw bytes as the exact numeric literal (`12.99`). Either way it hands
the exact text to `ParseCents`, which produces integer cents with no float in the
path. `MarshalJSON` does the inverse, emitting the canonical two-decimal string so
the amount round-trips as text. Because the type owns both directions, the `float64`
decode simply never happens for a `Money` field.

`DecodeAmounts` shows the streaming-decoder tool for the case where you *do* decode
into `interface{}` — for example, walking heterogeneous line items. Calling
`dec.UseNumber()` makes the decoder yield `json.Number` (a `string` alias that
preserves the literal) instead of `float64`, so you can parse each amount into cents
yourself. The demo and tests contrast the two: the same `0.1` is a lossy `float64`
under a plain decode and an exact literal under `UseNumber`.

Create `money.go`:

```go
package moneyjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ErrFormat marks a malformed or ambiguous amount; ErrOverflow marks one that
// cannot fit in int64 cents.
var (
	ErrFormat   = errors.New("money format")
	ErrOverflow = errors.New("money overflow")
)

// Money is an exact amount in integer minor units.
type Money int64

// ParseCents converts an exact decimal literal into integer cents without float64.
func ParseCents(raw string) (Money, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty amount: %w", ErrFormat)
	}
	if strings.HasPrefix(raw, "-") {
		return 0, fmt.Errorf("negative amount %q: %w", raw, ErrFormat)
	}
	whole, frac, hasFrac := strings.Cut(raw, ".")
	if !hasFrac {
		frac = "00"
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) != 2 {
		return 0, fmt.Errorf("amount %q needs two decimals: %w", raw, ErrFormat)
	}
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse whole %q: %w", whole, ErrFormat)
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse frac %q: %w", frac, ErrFormat)
	}
	if dollars > (math.MaxInt64-cents)/100 {
		return 0, fmt.Errorf("amount %q overflows: %w", raw, ErrOverflow)
	}
	return Money(dollars*100 + cents), nil
}

// String renders the amount as a two-decimal string.
func (m Money) String() string {
	sign := ""
	v := m
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// MarshalJSON emits the canonical two-decimal string, e.g. "12.99".
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON accepts the amount as a JSON string or a JSON number, preserving
// the exact literal and routing it through ParseCents. A float64 is never used.
func (m *Money) UnmarshalJSON(data []byte) error {
	literal := strings.TrimSpace(string(data))
	if len(literal) >= 2 && literal[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("decode amount string: %w", err)
		}
		literal = s
	}
	c, err := ParseCents(literal)
	if err != nil {
		return err
	}
	*m = c
	return nil
}

// DecodeAmounts reads a JSON array of numbers/strings using UseNumber so each
// literal is preserved exactly, then parses it into Money.
func DecodeAmounts(r io.Reader) ([]Money, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var raw []any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode array: %w", err)
	}
	out := make([]Money, 0, len(raw))
	for _, v := range raw {
		switch t := v.(type) {
		case json.Number:
			c, err := ParseCents(t.String())
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		case string:
			c, err := ParseCents(t)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("amount %v has unexpected type %T: %w", v, v, ErrFormat)
		}
	}
	return out, nil
}
```

### The runnable demo

The demo round-trips a request struct, then decodes the same `0.1` two ways to show
the loss the codec avoids.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"example.com/moneyjson"
)

type payment struct {
	Amount moneyjson.Money `json:"amount"`
}

func main() {
	var p payment
	if err := json.Unmarshal([]byte(`{"amount":"12.99"}`), &p); err != nil {
		fmt.Println("decode:", err)
		return
	}
	out, _ := json.Marshal(p)
	fmt.Printf("cents=%d re-emitted=%s\n", int64(p.Amount), out)

	// The default interface{} decode turns 0.1 into a lossy float64.
	var loose any
	_ = json.Unmarshal([]byte(`0.1`), &loose)
	fmt.Printf("float64 decode: %.17g\n", loose.(float64))

	// UseNumber preserves the exact literal.
	dec := json.NewDecoder(strings.NewReader(`0.1`))
	dec.UseNumber()
	var exact any
	_ = dec.Decode(&exact)
	fmt.Printf("UseNumber decode: %s\n", exact.(json.Number))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cents=1299 re-emitted={"amount":"12.99"}
float64 decode: 0.10000000000000001
UseNumber decode: 0.1
```

### Tests

The round-trip test asserts exact cents in and the exact string out, for both a small
amount and a large one that would lose precision as a `float64`. The precision test
proves the divergence concretely: the exact rational of the `float64` decode of `0.1`
is *not* `1/10`, while the `json.Number` literal is exactly `"0.1"`. The malformed
test proves a bad amount surfaces an error rather than a silent zero.

Create `money_test.go`:

```go
package moneyjson

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

type payment struct {
	Amount Money `json:"amount"`
}

func TestRoundTripExact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body      string
		wantCents int64
		wantJSON  string
	}{
		{`{"amount":"12.99"}`, 1299, `{"amount":"12.99"}`},
		{`{"amount":123456789012345.67}`, 12345678901234567, `{"amount":"123456789012345.67"}`},
		{`{"amount":"0.05"}`, 5, `{"amount":"0.05"}`},
	}
	for _, c := range cases {
		var p payment
		if err := json.Unmarshal([]byte(c.body), &p); err != nil {
			t.Fatalf("Unmarshal(%s): %v", c.body, err)
		}
		if int64(p.Amount) != c.wantCents {
			t.Fatalf("Unmarshal(%s) cents = %d, want %d", c.body, int64(p.Amount), c.wantCents)
		}
		out, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(out) != c.wantJSON {
			t.Fatalf("Marshal = %s, want %s", out, c.wantJSON)
		}
	}
}

func TestUseNumberPreservesLiteral(t *testing.T) {
	t.Parallel()

	// Default interface{} decode: 0.1 becomes a float64 whose exact value is not 1/10.
	var loose any
	if err := json.Unmarshal([]byte(`0.1`), &loose); err != nil {
		t.Fatal(err)
	}
	floatRat := new(big.Rat).SetFloat64(loose.(float64))
	if floatRat.Cmp(big.NewRat(1, 10)) == 0 {
		t.Fatal("float64 decode of 0.1 was exactly 1/10; expected precision loss")
	}

	// UseNumber decode: the literal text is preserved exactly.
	dec := json.NewDecoder(strings.NewReader(`0.1`))
	dec.UseNumber()
	var exact any
	if err := dec.Decode(&exact); err != nil {
		t.Fatal(err)
	}
	if got := exact.(json.Number).String(); got != "0.1" {
		t.Fatalf("UseNumber literal = %q, want \"0.1\"", got)
	}
}

func TestDecodeAmountsAndMalformed(t *testing.T) {
	t.Parallel()

	got, err := DecodeAmounts(strings.NewReader(`["12.99", 0.05, "100.00"]`))
	if err != nil {
		t.Fatalf("DecodeAmounts: %v", err)
	}
	want := []Money{1299, 5, 10000}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("amount[%d] = %d, want %d", i, got[i], want[i])
		}
	}

	var p payment
	if err := json.Unmarshal([]byte(`{"amount":"12.9"}`), &p); !errors.Is(err, ErrFormat) {
		t.Fatalf("malformed amount error = %v, want ErrFormat", err)
	}
	if p.Amount != 0 {
		t.Fatalf("malformed decode left amount = %d, want untouched 0", p.Amount)
	}
}
```

## Review

The codec is correct when a `float64` never appears between the wire and the integer
representation. Confirm it by round-tripping both a small and a large amount and
asserting exact cents and the re-emitted string. The precision test is the load-bearing
one: it shows that the *default* decode of `0.1` produces a `float64` whose exact
rational differs from `1/10`, while `UseNumber` preserves the literal — the reason the
custom `UnmarshalJSON` exists. The common failure is decoding a money field into
`float64` or `any` and never noticing, because small amounts happen to look right;
assert against a large decimal like `123456789012345.67`, where the loss is
unmistakable, and require malformed input to error rather than silently zero the field.

## Resources

- [`encoding/json#Number`](https://pkg.go.dev/encoding/json#Number) — the exact-literal string type with `Int64`/`Float64`/`String`.
- [`json.Decoder.UseNumber`](https://pkg.go.dev/encoding/json#Decoder.UseNumber) — decode numbers as `json.Number` instead of `float64`.
- [`json.Unmarshaler`](https://pkg.go.dev/encoding/json#Unmarshaler) — the custom-decoding interface the `Money` type implements.
- [`math/big#Rat.SetFloat64`](https://pkg.go.dev/math/big#Rat.SetFloat64) — recover a float's exact rational value, used to prove the loss.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-counter-overflow-with-math-bits.md](04-counter-overflow-with-math-bits.md)
