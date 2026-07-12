# Exercise 4: Boundary Conversions — Narrowing int64 to Fixed Widths Without Silent Truncation

A 64-bit domain integer has to become a 32-bit protobuf field, a 16-bit length
prefix, an unsigned wire value. In Go every one of those conversions wraps silently
on overflow — no panic, no error. This module builds the guarded conversions a
boundary needs, each range-checking against `math.Max*`/`math.Min*` before it
converts, next to a demonstration of the wrap they prevent.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
narrow/                    independent module: example.com/narrow
  go.mod                   go 1.26
  narrow.go                ToInt32, ToUint16, ToUint32 (guarded); RawNarrowInt32 (the wrap)
  cmd/
    demo/
      main.go              a successful narrow next to a silent wrap
  narrow_test.go           boundary table per width; wrap-justifies-guard test
```

- Files: `narrow.go`, `cmd/demo/main.go`, `narrow_test.go`.
- Implement: `ToInt32(int64)`, `ToUint16(int)`, `ToUint32(int)` that range-check and return an error on overflow, plus `RawNarrowInt32` showing the unguarded wrap.
- Test: boundary values per width (`MaxInt32`, `MaxInt32+1`, `MinInt32-1`, negative into unsigned, `MaxUint16`+1); a test asserting `RawNarrowInt32(1<<31)` is negative.
- Verify: `go test -count=1 -race ./...`

### Why a plain conversion is a bug generator

`int32(x)` does not check anything. If `x` is `2147483648` (one past `MaxInt32`),
`int32(x)` is `-2147483648` — the same bit pattern reinterpreted as a signed 32-bit
value. If `x` is `-1`, `uint32(x)` is `4294967295`. The compiler is happy, `go vet`
sees only constant expressions, and at runtime there is no signal at all; the wrong
value simply flows onward. This is exactly how a byte count computed as a difference
turns into a four-gigabyte length prefix, or a large database `bigint` gets stored in
a protobuf `int32` field as a negative id.

The fix is not clever — it is a range check against the target type's limits, using
the `math` constants (`math.MaxInt32`, `math.MinInt32`, `math.MaxUint16`,
`math.MaxUint32`) so the bound is named, not a magic number. Because Go's untyped
constants are arbitrary precision, comparing an `int64` or `int` against
`math.MaxUint32` in the guard does not itself overflow; the constant converts to the
comparison's type at compile time. Each helper returns an error when the value does
not fit, so the boundary rejects the value instead of silently corrupting it. The
`RawNarrowInt32` function is kept only to demonstrate — and to test — the wrap the
guards exist to prevent.

Create `narrow.go`:

```go
package narrow

import (
	"errors"
	"fmt"
	"math"
)

// ErrOverflow marks a value that does not fit the target width.
var ErrOverflow = errors.New("integer overflow")

// ToInt32 narrows a wide domain int64 (e.g. a DB bigint) into a 32-bit field,
// erroring instead of wrapping when it does not fit.
func ToInt32(x int64) (int32, error) {
	if x < math.MinInt32 || x > math.MaxInt32 {
		return 0, fmt.Errorf("%w: %d does not fit int32", ErrOverflow, x)
	}
	return int32(x), nil
}

// ToUint16 narrows a byte count or similar into a 16-bit length prefix,
// rejecting negatives and values above MaxUint16.
func ToUint16(n int) (uint16, error) {
	if n < 0 || n > math.MaxUint16 {
		return 0, fmt.Errorf("%w: %d does not fit uint16", ErrOverflow, n)
	}
	return uint16(n), nil
}

// ToUint32 narrows an int into an unsigned 32-bit value, rejecting negatives
// (which would wrap to a huge number) and values above MaxUint32.
func ToUint32(n int) (uint32, error) {
	if n < 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("%w: %d does not fit uint32", ErrOverflow, n)
	}
	return uint32(n), nil
}

// RawNarrowInt32 is the unguarded conversion, kept to demonstrate the silent
// two's-complement wrap that the guarded helpers exist to prevent.
func RawNarrowInt32(x int64) int32 { return int32(x) }
```

### The runnable demo

The demo narrows a valid length (512) successfully, then feeds `1<<31` — one past
`MaxInt32` — to both the guarded and the raw conversion, so the error and the
silent wrap sit side by side. It also rejects a negative into an unsigned width.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strconv"

	"example.com/narrow"
)

func main() {
	if n, err := narrow.ToUint16(512); err == nil {
		fmt.Println("length prefix:", n)
	}

	big := int64(1) << 31 // 2147483648, one past MaxInt32
	if _, err := narrow.ToInt32(big); err != nil {
		fmt.Println("guarded:", err)
	}
	fmt.Println("raw wrap:", strconv.FormatInt(int64(narrow.RawNarrowInt32(big)), 10))

	if _, err := narrow.ToUint32(-1); err != nil {
		fmt.Println("negative rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
length prefix: 512
guarded: integer overflow: 2147483648 does not fit int32
raw wrap: -2147483648
negative rejected: integer overflow: -1 does not fit uint32
```

### Tests

The tests walk the boundary of each width. `TestToInt32` checks `MaxInt32` and
`MinInt32` pass and that one step past either errors. `TestToUint16` and
`TestToUint32` check the top of the range and that a negative is rejected — the case
a plain conversion would turn into a huge unsigned value. `TestRawWrapJustifiesGuard`
asserts `RawNarrowInt32(1<<31)` is exactly `math.MinInt32`, documenting the wrap the
guards prevent.

Create `narrow_test.go`:

```go
package narrow

import (
	"errors"
	"math"
	"testing"
)

func TestToInt32(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      int64
		want    int32
		wantErr bool
	}{
		{name: "max fits", in: math.MaxInt32, want: math.MaxInt32},
		{name: "min fits", in: math.MinInt32, want: math.MinInt32},
		{name: "one past max", in: math.MaxInt32 + 1, wantErr: true},
		{name: "one below min", in: math.MinInt32 - 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ToInt32(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("ToInt32(%d) error = %v, want ErrOverflow", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToInt32(%d) unexpected error %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ToInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestToUint16(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      int
		want    uint16
		wantErr bool
	}{
		{name: "max fits", in: math.MaxUint16, want: math.MaxUint16},
		{name: "one past max", in: math.MaxUint16 + 1, wantErr: true},
		{name: "negative", in: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ToUint16(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("ToUint16(%d) error = %v, want ErrOverflow", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ToUint16(%d) = %d,%v; want %d,nil", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestToUint32Negative(t *testing.T) {
	t.Parallel()

	if _, err := ToUint32(-1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("ToUint32(-1) error = %v, want ErrOverflow", err)
	}
	if got, err := ToUint32(math.MaxUint32); err != nil || got != math.MaxUint32 {
		t.Fatalf("ToUint32(MaxUint32) = %d,%v; want max,nil", got, err)
	}
}

func TestRawWrapJustifiesGuard(t *testing.T) {
	t.Parallel()

	if got := RawNarrowInt32(int64(1) << 31); got != math.MinInt32 {
		t.Fatalf("RawNarrowInt32(1<<31) = %d, want %d (silent wrap)", got, math.MinInt32)
	}
}
```

## Review

The helpers are correct when they error exactly at the width boundary a raw
conversion would cross silently: `TestToInt32` pins both ends, and
`TestRawWrapJustifiesGuard` shows the alternative is a value flipped to negative with
no signal. Two easy mistakes to avoid: comparing against a hand-typed magic number
instead of the `math` constant (which drifts if someone edits it), and forgetting the
negative-into-unsigned case, which is the most damaging because `-1` becomes the
largest possible value rather than an obviously wrong small one. Prefer these guarded
helpers over inline conversions anywhere a value crosses from a wide domain type into
a narrower wire or storage width.

## Resources

- [math package constants](https://pkg.go.dev/math#pkg-constants) — `MaxInt32`, `MinInt32`, `MaxUint16`, `MaxUint32`.
- [Go Specification: Conversions](https://go.dev/ref/spec#Conversions) — the defined truncation/wrap behavior of numeric conversions.
- [Go Specification: Numeric types](https://go.dev/ref/spec#Numeric_types) — the exact ranges of the sized integer types.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-utf8-input-validation.md](05-utf8-input-validation.md)
