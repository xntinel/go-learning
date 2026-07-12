# Exercise 7: Use Untyped Constants and Compile-Time Overflow Guards for Numeric Limits

Untyped constants carry arbitrary precision until they land in a typed
destination, and you can weaponize that: a `const _ uintN = expr` assertion turns
"this limit must fit its type" into a build failure instead of a runtime
surprise. This module builds a pagination-limits package that combines a
compile-time boundary assertion with a runtime `SafeOffset` guard against integer
overflow.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
pagelimits/                     module: example.com/pagelimits
  go.mod                        go 1.26
  pagelimits.go                 MaxOffset, compile-time assertion, SafeOffset guard
  cmd/
    demo/
      main.go                   computes safe offsets and shows the overflow error
  pagelimits_test.go            SafeOffset in-range, overflow, boundary, negative
```

Files: `pagelimits.go`, `cmd/demo/main.go`, `pagelimits_test.go`.
Implement: `MaxOffset` derived from `math.MaxInt64`, a `const _ uint16 = MaxPageSize` compile-time assertion, and `SafeOffset(page, size int) (int, error)`.
Test: `SafeOffset` returns the product in range and an error (not a wrapped negative) on overflow; boundary at `math.MaxInt`; the constant equals its expected value.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/04-constants-and-iota/07-untyped-const-boundaries-overflow/cmd/demo
cd go-solutions/02-variables-types-and-constants/04-constants-and-iota/07-untyped-const-boundaries-overflow
```

## Two boundaries: one the compiler checks, one you must

There are two distinct overflow concerns here, and they call for two different
tools.

The first is a *static* limit: a page-size ceiling that must fit the type it is
stored in. Untyped constants make this checkable at compile time. Because
`MaxPageSize` is an untyped constant with arbitrary precision, you can force the
compiler to verify it fits a target type by assigning it to a throwaway typed
constant:

```go
const MaxPageSize = 500
const _ uint16 = MaxPageSize // build FAILS if MaxPageSize > 65535
```

If someone later bumps `MaxPageSize` to `100000`, the program *does not compile* —
the assertion `const _ uint16 = 100000` overflows `uint16` and the build stops.
This is the compiler acting as a boundary check. `math.MaxInt64`, `math.MaxInt`,
and `math.MaxUint16` give exact type boundaries for the same technique. A common
variant asserts a relationship, e.g. `const _ = uint(SomeLimit - Floor)` fails if
`SomeLimit < Floor` because the subtraction underflows an unsigned constant.

The second concern is *dynamic*: an offset computed from runtime inputs,
`page * size`. No compile-time assertion can see those values, so the guard must
run at execution time. The danger is that `page * size` silently wraps around
`int` and produces a *negative* offset — which a database might interpret as a
huge unsigned value or reject with a confusing error far from the cause. The
guard checks `page > math.MaxInt/size` *before* multiplying: if the product would
exceed `MaxInt`, it returns an explicit error instead of a wrapped-around result.
`MaxOffset` records the boundary itself, `math.MaxInt64`, as documentation and a
value other code can compare against.

Create `pagelimits.go`:

```go
package pagelimits

import (
	"errors"
	"fmt"
	"math"
)

// ErrOffsetOverflow is returned when a page offset would overflow int.
var ErrOffsetOverflow = errors.New("offset overflow")

// MaxOffset is the largest representable offset, the int64 upper bound.
const MaxOffset int64 = math.MaxInt64

// MaxPageSize is the hard ceiling on a page size.
const MaxPageSize = 500

// Compile-time assertion: MaxPageSize must fit in a uint16. If someone raises
// MaxPageSize above 65535, this line fails the build.
const _ uint16 = MaxPageSize

// SafeOffset computes page*size, returning an error rather than a wrapped
// negative value when the multiplication would overflow int.
func SafeOffset(page, size int) (int, error) {
	if page < 0 || size < 0 {
		return 0, fmt.Errorf("%w: negative input page=%d size=%d", ErrOffsetOverflow, page, size)
	}
	if size != 0 && page > math.MaxInt/size {
		return 0, fmt.Errorf("%w: page=%d size=%d", ErrOffsetOverflow, page, size)
	}
	return page * size, nil
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math"

	"example.com/pagelimits"
)

func main() {
	fmt.Printf("MaxOffset = %d\n", pagelimits.MaxOffset)

	off, err := pagelimits.SafeOffset(3, 50)
	fmt.Printf("SafeOffset(3, 50) = %d err=%v\n", off, err)

	_, err = pagelimits.SafeOffset(math.MaxInt, 2)
	fmt.Printf("SafeOffset(MaxInt, 2) err=%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
MaxOffset = 9223372036854775807
SafeOffset(3, 50) = 150 err=<nil>
SafeOffset(MaxInt, 2) err=offset overflow: page=9223372036854775807 size=2
```

## Tests

The table covers the in-range product, a zero size, the exact `math.MaxInt`
boundary (which must *not* error), the overflow just past it (which must error
with `ErrOffsetOverflow`), and a negative input. The compile-time assertion is
demonstrated in prose; the runtime tests exercise the dynamic guard.

Create `pagelimits_test.go`:

```go
package pagelimits

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestMaxOffset(t *testing.T) {
	t.Parallel()

	if MaxOffset != math.MaxInt64 {
		t.Fatalf("MaxOffset = %d, want %d", MaxOffset, int64(math.MaxInt64))
	}
}

func TestSafeOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		page    int
		size    int
		want    int
		wantErr bool
	}{
		{"zero page", 0, 100, 0, false},
		{"typical", 3, 50, 150, false},
		{"zero size", 10, 0, 0, false},
		{"max boundary", math.MaxInt, 1, math.MaxInt, false},
		{"overflow", math.MaxInt, 2, 0, true},
		{"negative page", -1, 10, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SafeOffset(tt.page, tt.size)
			if tt.wantErr {
				if !errors.Is(err, ErrOffsetOverflow) {
					t.Fatalf("SafeOffset(%d,%d) err = %v, want ErrOffsetOverflow", tt.page, tt.size, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeOffset(%d,%d): %v", tt.page, tt.size, err)
			}
			if got != tt.want {
				t.Fatalf("SafeOffset(%d,%d) = %d, want %d", tt.page, tt.size, got, tt.want)
			}
		})
	}
}

func ExampleSafeOffset() {
	off, err := SafeOffset(4, 25)
	fmt.Println(off, err)
	// Output: 100 <nil>
}
```

## Review

The package is correct when `SafeOffset` never returns a wrapped-around negative
offset: at the boundary it returns the exact product, and one step past it it
returns `ErrOffsetOverflow`. The compile-time assertion is the other half of the
discipline — it moves a whole class of "limit too big for its type" bug from a
runtime surprise to a failed build. Untyped constants make that possible because
they carry full precision until assigned; the assignment to a narrow typed
constant is where the overflow is caught.

## Resources

- [math: MaxInt, MaxInt64, MaxUint16](https://pkg.go.dev/math#pkg-constants)
- [Go Specification: Constants](https://go.dev/ref/spec#Constants)
- [Go Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-retry-backoff-duration-constants.md](06-retry-backoff-duration-constants.md) | Next: [08-enum-exhaustiveness-guard.md](08-enum-exhaustiveness-guard.md)
