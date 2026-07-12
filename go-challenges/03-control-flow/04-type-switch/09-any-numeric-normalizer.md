# Exercise 9: Normalize Numeric Values Across Decode Sources Safely

Numbers reach a backend in incompatible forms: JSON gives `float64` or
`json.Number`, protobuf gives `int64`, an environment variable gives a `string`.
A reconciliation layer must normalize any of them into an `int64` or `float64`
with overflow- and precision-safe conversion. The type switch selects the source
form; explicit range guards keep the conversion honest.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
numnorm/                     independent module: example.com/numnorm
  go.mod                     go 1.26
  numnorm.go                 ToInt64(v any) (int64, error); ToFloat64(v any) (float64, error)
  cmd/
    demo/
      main.go                normalizes numbers from four decode sources
  numnorm_test.go            big-int via json.Number, fractional reject, string parse, widths, stability
```

- Files: `numnorm.go`, `cmd/demo/main.go`, `numnorm_test.go`.
- Implement: `ToInt64(v any) (int64, error)` and `ToFloat64(v any) (float64,
  error)` handling `int`/width variants, `float64`, `json.Number`, and numeric
  strings with overflow and precision guards.
- Test: a `json.Number` beyond `float64`'s exact-integer range converts via
  `Int64` without loss but overflows on `Float64`; a fractional `float64` is
  rejected by `ToInt64`; numeric string parsing and a non-numeric error; each
  integer width; a round-trip stability check.
- Verify: `go test -count=1 -race ./...`

## The two guards: precision and overflow

Numeric normalization has exactly two ways to go silently wrong, and both are
guarded on the `float64` branch. Precision: a JSON `float64` carrying `8080.5` is
not an integer, so `int64(8080.5)` truncating to `8080` is data loss; the guard is
`v != math.Trunc(v)`. Overflow: a `float64` of `1e19` exceeds `int64` range, so
`int64(1e19)` is undefined-in-practice garbage; the guard is a range comparison
against `math.MinInt64` and `math.MaxInt64` before the conversion.

`json.Number` is the source that most rewards care. Because it stores the exact
decimal string, `Int64()` returns a full-precision `int64` even for
`9223372036854775807`, which a `float64` could never hold exactly. So `ToInt64`
prefers `json.Number.Int64()` and never routes a `json.Number` through `float64`.
Conversely `ToFloat64` on a `json.Number` too large for a `float64` (say `1e400`)
returns the error `Float64()` reports, rather than silently yielding `+Inf`.

For string sources, `strconv.ParseInt(s, 10, 64)` and `strconv.ParseFloat(s, 64)`
carry the range checks for free and report a typed error on garbage. Native
integer widths (`int`, `int8`..`int64`, and the unsigned widths) widen to `int64`,
with the one guard that a `uint64` above `math.MaxInt64` overflows and is
rejected.

Create `numnorm.go`:

```go
package numnorm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ErrNotNumeric is the sentinel for a value that cannot be normalized.
var ErrNotNumeric = errors.New("not numeric")

// ToInt64 normalizes any supported numeric source into an int64, rejecting
// fractional and out-of-range values instead of truncating them.
func ToInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint:
		return uintToInt64(uint64(n))
	case uint8:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		return uintToInt64(n)
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("%w: %v has a fractional part", ErrNotNumeric, n)
		}
		if n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, fmt.Errorf("%w: %v out of int64 range", ErrNotNumeric, n)
		}
		return int64(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%w: %q: %v", ErrNotNumeric, n.String(), err)
		}
		return i, nil
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q: %v", ErrNotNumeric, n, err)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("%w: cannot convert %T to int64", ErrNotNumeric, v)
	}
}

func uintToInt64(n uint64) (int64, error) {
	if n > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %d out of int64 range", ErrNotNumeric, n)
	}
	return int64(n), nil
}

// ToFloat64 normalizes any supported numeric source into a float64.
func ToFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case float32:
		return float64(n), nil
	case float64:
		return n, nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, fmt.Errorf("%w: %q: %v", ErrNotNumeric, n.String(), err)
		}
		return f, nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q: %v", ErrNotNumeric, n, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("%w: cannot convert %T to float64", ErrNotNumeric, v)
	}
}
```

## The runnable demo

The demo normalizes the same logical numbers as they would arrive from four
sources — protobuf `int64`, JSON `json.Number`, env `string`, and a JSON
`float64` — into a common `int64`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/numnorm"
)

func main() {
	sources := []any{
		int64(1500),         // protobuf
		json.Number("1500"), // JSON with UseNumber
		"1500",              // env override
		1500.0,              // JSON without UseNumber
	}
	for _, src := range sources {
		got, err := numnorm.ToInt64(src)
		if err != nil {
			fmt.Printf("%-14T -> error: %v\n", src, err)
			continue
		}
		fmt.Printf("%-14T -> %d\n", src, got)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
int64          -> 1500
json.Number    -> 1500
string         -> 1500
float64        -> 1500
```

## Tests

The big-integer test proves a `json.Number` at `math.MaxInt64` converts via
`Int64` with no loss, while the same value beyond `float64` range errors on
`ToFloat64`. The fractional test proves `ToInt64` rejects `8080.5`. The string
tests cover a good parse and a bad one. The widths test covers each integer type.
The stability test asserts `ToInt64` then boxing back to `any` and normalizing
again yields the same value.

Create `numnorm_test.go`:

```go
package numnorm

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestBigIntegerPrecision(t *testing.T) {
	t.Parallel()
	const maxInt64 = "9223372036854775807"
	got, err := ToInt64(json.Number(maxInt64))
	if err != nil {
		t.Fatalf("ToInt64: %v", err)
	}
	if got != 9223372036854775807 {
		t.Fatalf("ToInt64 = %d, want max int64 (float64 would lose precision)", got)
	}
	// A magnitude beyond float64 range must error, not become +Inf.
	if _, err := ToFloat64(json.Number("1e400")); err == nil {
		t.Fatal("ToFloat64(1e400) = nil error, want overflow error")
	}
}

func TestToInt64RejectsFractional(t *testing.T) {
	t.Parallel()
	if _, err := ToInt64(8080.5); !errors.Is(err, ErrNotNumeric) {
		t.Fatalf("ToInt64(8080.5) err = %v, want ErrNotNumeric", err)
	}
}

func TestToInt64Strings(t *testing.T) {
	t.Parallel()
	got, err := ToInt64("4096")
	if err != nil || got != 4096 {
		t.Fatalf("ToInt64(\"4096\") = %d, %v", got, err)
	}
	if _, err := ToInt64("not-a-number"); !errors.Is(err, ErrNotNumeric) {
		t.Fatalf("ToInt64(garbage) err = %v, want ErrNotNumeric", err)
	}
}

func TestToInt64Widths(t *testing.T) {
	t.Parallel()
	widths := []any{int(7), int8(7), int16(7), int32(7), int64(7), uint(7), uint8(7), uint16(7), uint32(7), uint64(7)}
	for _, w := range widths {
		got, err := ToInt64(w)
		if err != nil || got != 7 {
			t.Fatalf("ToInt64(%T) = %d, %v; want 7", w, got, err)
		}
	}
}

func TestToInt64Stability(t *testing.T) {
	t.Parallel()
	values := []int64{0, 1, -1, 1500, 9223372036854775807, -9223372036854775808}
	for _, want := range values {
		first, err := ToInt64(want)
		if err != nil {
			t.Fatalf("ToInt64(%d): %v", want, err)
		}
		var boxed any = first
		second, err := ToInt64(boxed)
		if err != nil {
			t.Fatalf("ToInt64(boxed %d): %v", want, err)
		}
		if second != want {
			t.Fatalf("stability: got %d, want %d", second, want)
		}
	}
}
```

## Review

The normalizer is correct when a `json.Number` at the `int64` maximum converts
without loss, when a fractional or out-of-range `float64` is rejected rather than
truncated, and when a `uint64` above `math.MaxInt64` overflows into an error
rather than a negative number. The guards are the whole point: dropping the
`math.Trunc` check turns `8080.5` into `8080`, and dropping the range check turns
`1e19` into garbage — both compile and pass a lazy test. Prefer `json.Number`'s
own `Int64`/`Float64` over routing a number through `float64`, because that is the
only path that preserves integer precision beyond 2^53.

## Resources

- [encoding/json.Number (Int64, Float64)](https://pkg.go.dev/encoding/json#Number)
- [strconv.ParseInt / ParseFloat](https://pkg.go.dev/strconv#ParseInt)
- [math constants (MaxInt64, MinInt64)](https://pkg.go.dev/math#pkg-constants)
- [math.Trunc](https://pkg.go.dev/math#Trunc)

---

Prev: [08-domain-error-to-http-status.md](08-domain-error-to-http-status.md) | Up: [00-concepts.md](00-concepts.md) | Next: [10-message-queue-payload-dispatcher.md](10-message-queue-payload-dispatcher.md)
