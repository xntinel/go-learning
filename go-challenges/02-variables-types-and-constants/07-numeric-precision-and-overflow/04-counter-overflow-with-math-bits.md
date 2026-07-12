# Exercise 4: Wide uint64 Metrics Accumulator with math/bits Overflow Detection

A request pipeline sums the bytes served across every request. At a few GB/s a plain
`uint64` byte counter laps `2^64` in a matter of days, and when it wraps your metrics
lie — the rate goes negative or resets to near zero with no signal. This exercise
builds a bytes-served accumulator that uses `math/bits` to detect single-word
overflow and to carry a full 128-bit total that never wraps.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
bytescounter/                independent module: example.com/bytescounter
  go.mod                     module path
  counter.go                 AddChecked, MulChecked; type Wide128; Add, BigInt
  cmd/
    demo/
      main.go                folds a request stream, prints the 128-bit total
  counter_test.go            carryOut/hi overflow detection, 128-bit vs math/big reference
```

Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
Implement: `AddChecked(a, b uint64) (uint64, error)` using `bits.Add64` (error when
`carryOut == 1`); `MulChecked(a, b uint64) (uint64, error)` using `bits.Mul64` (error
when `hi != 0`); and a `Wide128` accumulator holding `(hi, lo)` words with `Add(v
uint64)` and `BigInt() *big.Int`.
Test: `AddChecked(MaxUint64, 1)` overflows; `MulChecked(1<<40, 1<<40)` overflows
while `MulChecked(1<<20, 1<<20)` does not; a `Wide128` fold of a slice matches a
`math/big` reference sum exactly.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/04-counter-overflow-with-math-bits/cmd/demo
cd go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/04-counter-overflow-with-math-bits
```

### How math/bits turns overflow into a value you can test

`math/bits` exposes the machine's carry-aware arithmetic. `bits.Add64(x, y, carry)`
returns `(sum, carryOut)`: `sum` is the low 64 bits of `x + y + carry`, and
`carryOut` is 1 exactly when the true sum did not fit in 64 bits. So overflow is not
something you infer after the fact — it is a return value. `AddChecked` calls
`bits.Add64(a, b, 0)` and returns `ErrOverflow` when `carryOut == 1`, otherwise the
`sum`. `bits.Mul64(x, y)` returns `(hi, lo)`, the full 128-bit product split into
high and low words; `hi != 0` means the product overflowed 64 bits, so `MulChecked`
returns the `lo` word only when `hi == 0`. These are the idiomatic, constant-time
primitives for checked `uint64` math — far cleaner and more correct than trying to
reconstruct overflow from the wrapped result.

For a counter that *legitimately* exceeds 64 bits, rejecting the add is the wrong
answer; you want a wider accumulator. `Wide128` keeps two words, `hi` and `lo`.
Adding a `uint64` ripples the carry: `lo, carry = bits.Add64(lo, v, 0)` absorbs the
value into the low word and produces a carry of 0 or 1, then `hi, _ = bits.Add64(hi,
0, carry)` folds that carry into the high word. A 128-bit accumulator overflows only
after `2^128` bytes, which is not a number this universe will reach, so `Add` needs
no error. `BigInt()` renders the pair as an exact `math/big.Int` — `hi << 64 + lo` —
so the total can be compared against an independent reference or exported without
loss. This is the shape real observability code uses when a single `uint64` is not
enough headroom.

Create `counter.go`:

```go
package bytescounter

import (
	"errors"
	"fmt"
	"math/big"
	"math/bits"
)

// ErrOverflow is returned when a single-word operation would exceed uint64.
var ErrOverflow = errors.New("counter overflow")

// AddChecked returns a+b, or ErrOverflow if the sum does not fit in uint64.
// The carry-out from bits.Add64 is the overflow signal.
func AddChecked(a, b uint64) (uint64, error) {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return 0, fmt.Errorf("add %d+%d: %w", a, b, ErrOverflow)
	}
	return sum, nil
}

// MulChecked returns a*b, or ErrOverflow if the product does not fit in uint64.
// A nonzero high word from bits.Mul64 is the overflow signal.
func MulChecked(a, b uint64) (uint64, error) {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return 0, fmt.Errorf("multiply %d*%d: %w", a, b, ErrOverflow)
	}
	return lo, nil
}

// Wide128 is a 128-bit accumulator that never wraps within any realistic byte count.
type Wide128 struct {
	hi, lo uint64
}

// Add folds a uint64 into the accumulator, rippling the carry into the high word.
func (w *Wide128) Add(v uint64) {
	var carry uint64
	w.lo, carry = bits.Add64(w.lo, v, 0)
	w.hi, _ = bits.Add64(w.hi, 0, carry)
}

// BigInt renders the accumulator as an exact arbitrary-precision integer.
func (w *Wide128) BigInt() *big.Int {
	out := new(big.Int).SetUint64(w.hi)
	out.Lsh(out, 64)
	out.Add(out, new(big.Int).SetUint64(w.lo))
	return out
}
```

### The runnable demo

The demo folds a small stream of large per-request byte counts — each near
`math.MaxUint64` — so the total plainly exceeds 64 bits, and prints the exact 128-bit
result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math"

	"example.com/bytescounter"
)

func main() {
	counts := []uint64{math.MaxUint64, math.MaxUint64, math.MaxUint64}
	var acc bytescounter.Wide128
	for _, c := range counts {
		acc.Add(c)
	}
	fmt.Printf("bytes served: %s\n", acc.BigInt())

	if _, err := bytescounter.AddChecked(math.MaxUint64, 1); err != nil {
		fmt.Println("single-word add guarded:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the total is `3 * (2^64 - 1) = 55340232221128654845`):

```
bytes served: 55340232221128654845
single-word add guarded: add 18446744073709551615+1: counter overflow
```

### Tests

`TestAddCheckedOverflow` asserts that `MaxUint64 + 1` reports overflow via the carry,
and that a sum in range succeeds. `TestMulCheckedOverflow` contrasts the two products
from the brief: `(1<<40) * (1<<40) = 2^80` overflows because `hi != 0`, while
`(1<<20) * (1<<20) = 2^40` fits. `TestWide128MatchesBigReference` folds a slice of
`uint64` counts (including maxima) into both the `Wide128` accumulator and an
independent `math/big` running sum, and asserts the rendered totals are equal — proof
that the carry ripple is correct.

Create `counter_test.go`:

```go
package bytescounter

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
)

func TestAddCheckedOverflow(t *testing.T) {
	t.Parallel()

	if _, err := AddChecked(math.MaxUint64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("AddChecked(MaxUint64,1) error = %v, want ErrOverflow", err)
	}
	if got, err := AddChecked(math.MaxUint64-1, 1); err != nil || got != math.MaxUint64 {
		t.Fatalf("AddChecked(MaxUint64-1,1) = %d,%v; want MaxUint64,nil", got, err)
	}
}

func TestMulCheckedOverflow(t *testing.T) {
	t.Parallel()

	if _, err := MulChecked(1<<40, 1<<40); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MulChecked(1<<40,1<<40) error = %v, want ErrOverflow", err)
	}
	got, err := MulChecked(1<<20, 1<<20)
	if err != nil {
		t.Fatalf("MulChecked(1<<20,1<<20): unexpected error %v", err)
	}
	if got != 1<<40 {
		t.Fatalf("MulChecked(1<<20,1<<20) = %d, want %d", got, uint64(1<<40))
	}
}

func TestWide128MatchesBigReference(t *testing.T) {
	t.Parallel()

	counts := []uint64{math.MaxUint64, 1, math.MaxUint64, 42, 1 << 63, math.MaxUint64}
	var acc Wide128
	ref := new(big.Int)
	for _, c := range counts {
		acc.Add(c)
		ref.Add(ref, new(big.Int).SetUint64(c))
	}
	if acc.BigInt().Cmp(ref) != 0 {
		t.Fatalf("Wide128 total = %s, want %s", acc.BigInt(), ref)
	}
}

func ExampleWide128() {
	var acc Wide128
	acc.Add(math.MaxUint64)
	acc.Add(math.MaxUint64)
	fmt.Println(acc.BigInt())
	// Output: 36893488147419103230
}
```

## Review

The accumulator is correct when overflow is read from the primitive, not guessed from
the wrapped value. Confirm `AddChecked` flags `MaxUint64 + 1` via `carryOut` and
`MulChecked` flags `2^80` via a nonzero `hi`, and that in-range operations return the
exact result. The 128-bit path is correct when its rendered total equals an
independent `math/big` sum over the same inputs, including several `MaxUint64` values
that force the carry to ripple; if the ripple were dropped, the totals would diverge
by a multiple of `2^64`. The mistake to avoid is treating a `uint64` counter as
"big enough" and inferring overflow from a suspicious drop — `math/bits` makes the
carry explicit so you never have to.

## Resources

- [`math/bits#Add64`](https://pkg.go.dev/math/bits#Add64) — carry-aware addition; `carryOut` is the overflow bit.
- [`math/bits#Mul64`](https://pkg.go.dev/math/bits#Mul64) — full 128-bit product; `hi != 0` signals 64-bit overflow.
- [`math/bits#Sub64`](https://pkg.go.dev/math/bits#Sub64) — the borrow-aware mirror for checked subtraction.
- [`math/big#Int`](https://pkg.go.dev/math/big#Int) — exact arbitrary-precision rendering of the 128-bit total.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-safe-narrowing-conversions.md](05-safe-narrowing-conversions.md)
