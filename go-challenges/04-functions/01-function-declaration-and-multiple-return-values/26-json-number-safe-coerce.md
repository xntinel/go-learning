# Exercise 26: Safe JSON Number Parsing With Overflow Detection

**Nivel: Intermedio** — validacion rapida (un test corto).

`encoding/json` decodes every JSON number into a `float64` unless you opt in
to `json.Number`. A `float64` mantissa only has 53 bits, so any integer past
`2^53` — a Snowflake ID, a 64-bit database primary key, a large Unix nano
timestamp — may already have been rounded to the *wrong* integer before your
code ever sees it. This exercise builds `CoerceInt64(v any) (value int64,
overflow bool, error)` so a caller can tell "clean integer" from "this
magnitude cannot be trusted" from "this was never an integer at all".

This module is fully self-contained: its own `go mod init`, all code inline,
its own demo and tests.

## What you'll build

```text
jsonnum/                    independent module: example.com/json-number-safe-coerce
  go.mod                    go 1.24
  jsonnum.go                package jsonnum; ErrOverflow; CoerceInt64(v any) (value int64, overflow bool, error)
  cmd/
    demo/
      main.go                plain float64 decode vs json.Number decode of the same large id
  jsonnum_test.go            table of int/float/json.Number cases; NaN/Inf; overflow wraps ErrOverflow
```

- Files: `jsonnum.go`, `cmd/demo/main.go`, `jsonnum_test.go`.
- Implement: `CoerceInt64(v any) (value int64, overflow bool, err error)` accepting `int`, `int64`, `float64`, and `json.Number`; rejecting non-finite and fractional floats with an error; and reporting `overflow == true` alongside a wrapped `ErrOverflow` whenever a `float64`'s magnitude exceeds `2^53`, the largest integer a float64 mantissa represents exactly.
- Test: plain ints and small floats convert cleanly; a fractional float64 errors; a `float64` beyond `2^53` reports `overflow == true` and `errors.Is(err, ErrOverflow)`; a `json.Number` holding the exact same large integer as a decimal string converts without overflow because no float64 rounding ever occurred; NaN and `+Inf` both error; an unsupported type (e.g. `string`) errors.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/jsonnum/cmd/demo
cd ~/go-exercises/jsonnum
go mod init example.com/json-number-safe-coerce
go mod edit -go=1.24
```

### Why a plain conversion is not enough here

Naive code does `int64(v.(float64))` after unmarshaling a dynamic JSON
payload. For small numbers that is fine. But feed it `{"id":
9223372036854775807}` — a valid `int64` written by another service — and
`encoding/json` first parses that literal into a `float64`, which cannot
represent it exactly: the nearest representable `float64` is a different
integer entirely (rounded to the nearest multiple of `2^11` in this range).
The bug does not announce itself. `int64(v.(float64))` returns *a* number,
just the wrong one, and nothing downstream fails loudly — the id silently
drifts, and a lookup by that id returns not-found or, worse, hits an
unrelated row after further arithmetic.

Two returns are not enough to describe this. A `(int64, error)` signature
cannot distinguish "the caller passed a string" from "this is a fraction"
from "this magnitude is fundamentally unsafe from a float64" — three
outcomes that call for three different fixes (fix the caller, reject the
input, or re-decode with `json.Number`/`UseNumber()` so no rounding ever
happens). `overflow bool` names the specific one that a generic `error`
would otherwise bury: the value was never wrong until `encoding/json`
already made it wrong.

Create `jsonnum.go`:

```go
package jsonnum

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// ErrOverflow is returned when a decoded JSON number's magnitude exceeds the
// largest integer a float64 can represent exactly, meaning encoding/json may
// have already rounded it to a different integer than what was written.
var ErrOverflow = errors.New("value exceeds safe integer precision")

// maxSafeFloat is 2^53, the largest integer magnitude representable exactly
// by a float64 mantissa. Beyond it, adjacent integers collapse onto the same
// float64 bit pattern.
const maxSafeFloat = 1 << 53

// CoerceInt64 converts a decoded JSON value into an int64, distinguishing
// three outcomes: a clean integer conversion, a value whose magnitude is
// inherently unsafe to trust because encoding/json already lost precision
// decoding it into a float64, and a value that is not an integer at all.
func CoerceInt64(v any) (value int64, overflow bool, err error) {
	switch n := v.(type) {
	case json.Number:
		// json.Number preserves the original decimal text, so an exact
		// parse here is authoritative -- no float64 rounding occurred
		// during decode, unlike the plain float64 path below.
		if i, convErr := n.Int64(); convErr == nil {
			return i, false, nil
		}
		f, convErr := n.Float64()
		if convErr != nil {
			return 0, false, fmt.Errorf("coerce %q: %w", n.String(), convErr)
		}
		return coerceFloat(f)
	case float64:
		return coerceFloat(n)
	case int:
		return int64(n), false, nil
	case int64:
		return n, false, nil
	default:
		return 0, false, fmt.Errorf("coerce: unsupported type %T", v)
	}
}

func coerceFloat(f float64) (int64, bool, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("coerce %v: not a finite number", f)
	}
	if f != math.Trunc(f) {
		return 0, false, fmt.Errorf("coerce %v: not an integer value", f)
	}
	if f > maxSafeFloat || f < -maxSafeFloat {
		return 0, true, fmt.Errorf("coerce %v: %w", f, ErrOverflow)
	}
	return int64(f), false, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	jsonnum "example.com/json-number-safe-coerce"
)

func main() {
	raw := []byte(`{"id": 9223372036854775807, "price": 19.99, "count": 42, "label": "sku-1"}`)

	// Plain Unmarshal decodes every JSON number as float64, silently
	// losing precision for large integers.
	var loose map[string]any
	_ = json.Unmarshal(raw, &loose)

	value, overflow, err := jsonnum.CoerceInt64(loose["id"])
	fmt.Printf("id (float64 path):     value=%d overflow=%t err=%v\n", value, overflow, err)

	value, overflow, err = jsonnum.CoerceInt64(loose["count"])
	fmt.Printf("count (float64 path):  value=%d overflow=%t err=%v\n", value, overflow, err)

	value, overflow, err = jsonnum.CoerceInt64(loose["price"])
	fmt.Printf("price (float64 path):  value=%d overflow=%t err=%v\n", value, overflow, err)

	value, overflow, err = jsonnum.CoerceInt64(loose["label"])
	fmt.Printf("label (float64 path):  value=%d overflow=%t err=%v\n", value, overflow, err)

	// Decoding with UseNumber preserves the original digit string, so the
	// same large id can be coerced exactly.
	var strict map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&strict)

	value, overflow, err = jsonnum.CoerceInt64(strict["id"])
	fmt.Printf("id (json.Number path): value=%d overflow=%t err=%v\n", value, overflow, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id (float64 path):     value=0 overflow=true err=coerce 9.223372036854776e+18: value exceeds safe integer precision
count (float64 path):  value=42 overflow=false err=<nil>
price (float64 path):  value=0 overflow=false err=coerce 19.99: not an integer value
label (float64 path):  value=0 overflow=false err=coerce: unsupported type string
id (json.Number path): value=9223372036854775807 overflow=false err=<nil>
```

### Tests

Create `jsonnum_test.go`:

```go
package jsonnum

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCoerceInt64(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		in           any
		wantValue    int64
		wantOverflow bool
		wantErr      bool
		wantErrIsOv  bool
	}{
		{name: "plain int", in: 42, wantValue: 42},
		{name: "int64", in: int64(7), wantValue: 7},
		{name: "clean float64", in: float64(100), wantValue: 100},
		{name: "fractional float64", in: 19.99, wantErr: true},
		{name: "overflowing float64", in: float64(int64(1) << 60), wantOverflow: true, wantErr: true, wantErrIsOv: true},
		{name: "json.Number exact large int", in: json.Number("9223372036854775807"), wantValue: 9223372036854775807},
		{name: "json.Number small", in: json.Number("42"), wantValue: 42},
		{name: "json.Number fractional", in: json.Number("3.5"), wantErr: true},
		{name: "unsupported type", in: "sku-1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, overflow, err := CoerceInt64(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if overflow != tc.wantOverflow {
				t.Fatalf("overflow = %t, want %t", overflow, tc.wantOverflow)
			}
			if !tc.wantErr && value != tc.wantValue {
				t.Fatalf("value = %d, want %d", value, tc.wantValue)
			}
			if tc.wantErrIsOv && !errors.Is(err, ErrOverflow) {
				t.Fatalf("err = %v, want it to wrap ErrOverflow", err)
			}
		})
	}
}

func TestCoerceInt64NaNAndInf(t *testing.T) {
	t.Parallel()

	if _, _, err := CoerceInt64(nanValue()); err == nil {
		t.Fatal("want an error for NaN")
	}
	if _, _, err := CoerceInt64(infValue()); err == nil {
		t.Fatal("want an error for +Inf")
	}
}

func nanValue() float64 {
	var zero float64
	return zero / zero
}

func infValue() float64 {
	var one, zero float64 = 1, 0
	return one / zero
}
```

## Review

`CoerceInt64` is correct when its three outcomes never blur: a clean
integer, a value whose `float64` magnitude already lost precision before
this function ever ran, and a value that was never an integer. The
`json.Number` cases are the load-bearing ones — they prove the overflow is
a property of *how the number was decoded*, not of the number itself: the
exact same digits that overflow through the `float64` path convert cleanly
through `json.Number`, because that path never rounds.

The mistake to avoid is checking `f > math.MaxInt64` instead of the safe
float boundary `2^53`. `math.MaxInt64` is itself not exactly representable
as a `float64`, so a bound check against it still lets through values in
the gap between `2^53` and `math.MaxInt64` whose `float64` representation
may already differ from the integer that was written — the whole point of
`overflow` is to catch precision loss, not just magnitude that literally
cannot fit in an `int64`.

## Resources

- [encoding/json: Number](https://pkg.go.dev/encoding/json#Number) — the `UseNumber` decoding mode that avoids float64 rounding entirely.
- [math.Trunc](https://pkg.go.dev/math#Trunc) and [math.IsInf](https://pkg.go.dev/math#IsInf) — the finiteness and integrality checks this exercise relies on.
- [IEEE 754: Safe integers in floating point](https://en.wikipedia.org/wiki/Double-precision_floating-point_format#Precision_limitations_on_integer_values) — why `2^53` is the exact-representation boundary for `float64`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-kafka-offset-commit-verify.md](25-kafka-offset-commit-verify.md) | Next: [27-ldap-directory-attribute-extract.md](27-ldap-directory-attribute-extract.md)
