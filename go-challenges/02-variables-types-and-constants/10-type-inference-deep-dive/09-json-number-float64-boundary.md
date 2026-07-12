# Exercise 9: JSON Numbers Become float64: The 2^53 Precision Trap

Decoding untyped JSON is a boundary where static type is erased and every number
becomes a `float64`. You will build a webhook parser that demonstrates a large
integer id silently losing precision on the default path, and fix it with
`json.Decoder.UseNumber` so the id survives as an exact `int64`.

## What you'll build

```text
webhookid/                  independent module: example.com/webhookid
  go.mod                    go 1.26
  webhookid.go              LossyID (map[string]any) and ExactID (UseNumber)
  cmd/
    demo/
      main.go               decodes a 19-digit id both ways, prints both
  webhookid_test.go         precision loss, exact round-trip, type-switch assertion
```

Files: `webhookid.go`, `cmd/demo/main.go`, `webhookid_test.go`.
Implement: `LossyID(raw []byte)` decoding into `map[string]any`, and
`ExactID(raw []byte)` using a `json.Decoder` with `UseNumber`.
Test: a 19-digit id differs after the lossy path but round-trips exactly via
`UseNumber`+`Int64`, plus a type-switch asserting the default concrete type is
`float64`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/10-type-inference-deep-dive/09-json-number-float64-boundary/cmd/demo
cd go-solutions/02-variables-types-and-constants/10-type-inference-deep-dive/09-json-number-float64-boundary
go mod edit -go=1.26
```

## Why every JSON number arrives as float64

`encoding/json` has no way to know, from the wire, whether `1234567890123456789`
was meant to be an `int64`, a `uint64`, or a `float64`. When the decode target is
`any` or `map[string]any` — no struct field type to guide it — it makes one uniform
choice: **every JSON number becomes a `float64`**. A `float64` has a 53-bit mantissa,
so it represents every integer up to 2^53 (9007199254740992) exactly and *rounds*
anything larger. A Snowflake id, a large order id, or any 19-digit external id
therefore loses its low bits the instant it is decoded into `map[string]any`. The
value is not corrupted loudly; it is rounded to the nearest representable `float64`,
so `1234567890123456789` comes back as `1234567890123456768` — off by 21, silently.

The fix is `json.Decoder.UseNumber()`. With it set, the decoder delivers each JSON
number as a `json.Number` (a string under the hood) instead of a `float64`, so the
exact digits are preserved. You then call `Number.Int64()` to parse the integer with
full precision, or `Number.Float64()` when a float really is intended. The parser
below exposes both paths so the difference is measurable: `LossyID` goes through
`map[string]any` and reads the id as `float64`; `ExactID` uses `UseNumber` and reads
it as `int64`.

The type switch in the test is the other half of the lesson: after decoding into
`any`, the *static* type is gone, and you recover the *dynamic* type with a
`switch v := x.(type)`. On the default path the id's dynamic type is `float64`; with
`UseNumber` it is `json.Number`. Seeing that in a test is how you internalize that
the `any` boundary is where type information is decided, not where you declared the
field.

Create `webhookid.go`:

```go
package webhookid

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// LossyID decodes into map[string]any, where every JSON number becomes float64.
// A large integer id loses precision beyond 2^53. Returned as float64 to make
// the lossy type explicit.
func LossyID(raw []byte) (float64, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("unmarshal: %w", err)
	}
	v, ok := m["id"]
	if !ok {
		return 0, fmt.Errorf("missing id field")
	}
	f, ok := v.(float64) // the default dynamic type of a JSON number
	if !ok {
		return 0, fmt.Errorf("id is %T, want float64", v)
	}
	return f, nil
}

// ExactID uses a json.Decoder with UseNumber, so numbers arrive as json.Number
// and Int64 recovers the exact integer with no precision loss.
func ExactID(raw []byte) (int64, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	v, ok := m["id"]
	if !ok {
		return 0, fmt.Errorf("missing id field")
	}
	num, ok := v.(json.Number) // UseNumber makes this the dynamic type
	if !ok {
		return 0, fmt.Errorf("id is %T, want json.Number", v)
	}
	id, err := num.Int64()
	if err != nil {
		return 0, fmt.Errorf("id %q not an int64: %w", num, err)
	}
	return id, nil
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/webhookid"
)

func main() {
	raw := []byte(`{"id": 1234567890123456789}`)

	lossy, err := webhookid.LossyID(raw)
	if err != nil {
		panic(err)
	}
	exact, err := webhookid.ExactID(raw)
	if err != nil {
		panic(err)
	}

	fmt.Printf("original: 1234567890123456789\n")
	fmt.Printf("lossy (float64 -> int64): %d\n", int64(lossy))
	fmt.Printf("exact (UseNumber):        %d\n", exact)
	fmt.Printf("precision lost: %v\n", int64(lossy) != exact)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
original: 1234567890123456789
lossy (float64 -> int64): 1234567890123456768
exact (UseNumber):        1234567890123456789
precision lost: true
```

## Tests

The tests prove the lossy path corrupts the id while the exact path preserves it,
and a type-switch test asserts the default dynamic type of a JSON number is
`float64`.

Create `webhookid_test.go`:

```go
package webhookid

import (
	"encoding/json"
	"testing"
)

const bigID = int64(1234567890123456789)

func TestLossyLosesPrecision(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"id": 1234567890123456789}`)
	f, err := LossyID(raw)
	if err != nil {
		t.Fatal(err)
	}
	if int64(f) == bigID {
		t.Fatalf("expected precision loss, but int64(%v) == %d", f, bigID)
	}
}

func TestExactRoundTrip(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"id": 1234567890123456789}`)
	id, err := ExactID(raw)
	if err != nil {
		t.Fatal(err)
	}
	var _ int64 = id // pin
	if id != bigID {
		t.Fatalf("ExactID = %d, want %d", id, bigID)
	}
}

func TestDefaultDynamicTypeIsFloat64(t *testing.T) {
	t.Parallel()

	var m map[string]any
	if err := json.Unmarshal([]byte(`{"id": 42}`), &m); err != nil {
		t.Fatal(err)
	}
	switch v := m["id"].(type) {
	case float64:
		// expected: the default concrete type for a JSON number
	case json.Number:
		t.Fatalf("got json.Number without UseNumber; want float64")
	default:
		t.Fatalf("id has unexpected dynamic type %T", v)
	}
}

func TestUseNumberDynamicTypeIsNumber(t *testing.T) {
	t.Parallel()

	id, err := ExactID([]byte(`{"id": 100}`))
	if err != nil {
		t.Fatal(err)
	}
	if id != 100 {
		t.Fatalf("ExactID = %d, want 100", id)
	}
}
```

## Review

The parser is correct when the lossy path returns a `float64` that no longer equals
the original 19-digit id, and the exact path returns an `int64` that does. The
mechanism to remember: `encoding/json` decodes every number into `float64` when the
target is `any`/`map[string]any`, and a `float64` cannot represent integers beyond
2^53, so ids get rounded silently. `json.Decoder.UseNumber()` plus `Number.Int64()`
is the fix, and the type switch shows exactly where the concrete type is decided —
at the `any` boundary, not at your struct declaration. In real handlers, prefer
decoding into a struct with a typed `int64` field when the shape is known;
`UseNumber` is for the genuinely dynamic `map[string]any` case.

## Resources

- [json.Decoder.UseNumber](https://pkg.go.dev/encoding/json#Decoder.UseNumber) — deliver numbers as `json.Number`.
- [json.Number](https://pkg.go.dev/encoding/json#Number) — `Int64` and `Float64` for exact recovery.
- [IEEE 754 double precision](https://en.wikipedia.org/wiki/Double-precision_floating-point_format) — the 53-bit mantissa and the 2^53 integer limit.

---

Back to [08-config-precedence-cmp-or.md](08-config-precedence-cmp-or.md) | Next: [10-generic-clamp-ordered.md](10-generic-clamp-ordered.md)
