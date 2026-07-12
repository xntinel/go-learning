# Exercise 2: Checked Add and Multiply That Fail Closed Before Wraparound

A checkout computes a subtotal by multiplying a unit price by a quantity, then adds
tax and shipping. Every one of those operations is a place an `int64` can wrap, and
a wrapped total is a silently corrupted charge. This exercise builds the checked
arithmetic core on a `Cents` money type: `Add` and `Multiply` that return
`ErrOverflow` instead of ever producing a wrapped value.

This module is fully self-contained: its own module, demo, and tests. It re-declares
the `Cents` type it needs so it depends on nothing else.

## What you'll build

```text
checkedmoney/                independent module: example.com/checkedmoney
  go.mod                     module path
  money.go                   type Cents; Add, Multiply, String; ErrOverflow, ErrNegative
  cmd/
    demo/
      main.go                runs a checkout, then shows a guarded overflow
  money_test.go              checkout flow on exact String, overflow + underflow via errors.Is
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: `Add(a, b Cents) (Cents, error)` with both-direction guards,
`Multiply(price Cents, quantity int64) (Cents, error)` that rejects negatives,
short-circuits zero, and checks `price > MaxInt64/quantity` before multiplying, plus
`String()`.
Test: a realistic checkout (`Multiply(1299,3) -> "38.97"`, then `Add(subtotal,250) ->
"41.47"`) on exact strings; `Add(MaxInt64,1)` and `Multiply(MaxInt64/2+1,2)` both
returning `ErrOverflow`; `Add(MinInt64,-1)` returning `ErrOverflow` for the lower
bound.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/02-checked-add-multiply-arithmetic/cmd/demo
cd go-solutions/02-variables-types-and-constants/07-numeric-precision-and-overflow/02-checked-add-multiply-arithmetic
```

### The two guards, and why they are written this way

`Add` must reject a sum that would leave the `int64` range in *either* direction,
and it must do so without itself overflowing. The insight is that overflow can only
happen toward the sign of `b`. When `b > 0`, the sum can only exceed `MaxInt64`, and
the exact condition is `a > MaxInt64 - b`; because `b > 0`, `MaxInt64 - b` is a
value strictly below `MaxInt64` and the subtraction never overflows. When `b < 0`,
the sum can only fall below `MinInt64`, and the condition is `a < MinInt64 - b`;
because `b < 0`, `MinInt64 - b` adds a positive magnitude and stays in range. When
`b == 0` neither branch triggers and `a + 0` is trivially safe. This is the mirror
pair the concepts file describes: guarding only the `MaxInt64` side would let a
debit underflow past `MinInt64` silently, which is why `Add(MinInt64, -1)` must be
rejected too.

`Multiply` takes a `price` in cents and an integer `quantity`. It first rejects
negative operands as a policy error (`ErrNegative`) — a negative price or quantity
is a caller bug, not an arithmetic edge — then short-circuits when either operand is
zero, because the division-based guard would divide by zero otherwise. With both
operands positive, the check `int64(price) > math.MaxInt64 / quantity` asks whether
`price` exceeds the largest multiplicand that still fits: integer division truncates,
so `MaxInt64 / quantity` is exactly that largest value, and any `price` above it
would overflow. Only after that check passes does the multiplication run. Checking
after — computing `price * quantity` and testing whether it "looks too small" — is
the mistake the concepts file warns against, because the product has already wrapped
by then.

Create `money.go`:

```go
package checkedmoney

import (
	"errors"
	"fmt"
	"math"
)

// ErrOverflow is returned when an operation would leave the int64 cents range.
var ErrOverflow = errors.New("money overflow")

// ErrNegative is returned when a non-negative operand is required.
var ErrNegative = errors.New("negative operand")

// Cents is an exact amount in integer minor units.
type Cents int64

// Add returns a+b, or ErrOverflow if the sum would leave the int64 range in
// either direction. The guard is computed before the addition so it never wraps.
func Add(a, b Cents) (Cents, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("add %d+%d: %w", a, b, ErrOverflow)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("add %d+%d: %w", a, b, ErrOverflow)
	}
	return a + b, nil
}

// Multiply returns price*quantity in cents. It rejects negative operands, treats
// a zero operand as an exact zero, and checks the product fits before computing it.
func Multiply(price Cents, quantity int64) (Cents, error) {
	if price < 0 || quantity < 0 {
		return 0, fmt.Errorf("price=%d quantity=%d: %w", price, quantity, ErrNegative)
	}
	if price == 0 || quantity == 0 {
		return 0, nil
	}
	if int64(price) > math.MaxInt64/quantity {
		return 0, fmt.Errorf("multiply %d*%d: %w", price, quantity, ErrOverflow)
	}
	return Cents(int64(price) * quantity), nil
}

// String renders cents as a signed decimal with two fractional digits.
func (c Cents) String() string {
	sign := ""
	v := c
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/checkedmoney"
)

func main() {
	subtotal, err := checkedmoney.Multiply(1299, 3) // 3 items at $12.99
	if err != nil {
		fmt.Println("multiply:", err)
		return
	}
	total, err := checkedmoney.Add(subtotal, 250) // + $2.50 shipping
	if err != nil {
		fmt.Println("add:", err)
		return
	}
	fmt.Printf("subtotal=%s total=%s\n", subtotal, total)

	_, err = checkedmoney.Add(checkedmoney.Cents(9223372036854775807), 1)
	fmt.Println("overflow guarded:", errors.Is(err, checkedmoney.ErrOverflow))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subtotal=38.97 total=41.47
overflow guarded: true
```

### Tests

The checkout test asserts exact `String()` output — the only honest way to verify
money formatting. The overflow tests assert error *identity* with `errors.Is` at the
exact boundaries: `Add(MaxInt64, 1)` overflows, `Multiply(MaxInt64/2+1, 2)`
overflows (because `MaxInt64/2 + 1` is the smallest multiplicand that laps the range
when doubled), and `Add(MinInt64, -1)` underflows — the lower-bound case that a
one-sided guard would miss.

Create `money_test.go`:

```go
package checkedmoney

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestCheckoutFlow(t *testing.T) {
	t.Parallel()

	subtotal, err := Multiply(1299, 3)
	if err != nil {
		t.Fatalf("Multiply: %v", err)
	}
	if subtotal.String() != "38.97" {
		t.Fatalf("subtotal = %s, want 38.97", subtotal)
	}

	total, err := Add(subtotal, 250)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if total.String() != "41.47" {
		t.Fatalf("total = %s, want 41.47", total)
	}
}

func TestAddOverflowBothDirections(t *testing.T) {
	t.Parallel()

	if _, err := Add(math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Add(MaxInt64,1) error = %v, want ErrOverflow", err)
	}
	if _, err := Add(math.MinInt64, -1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Add(MinInt64,-1) error = %v, want ErrOverflow", err)
	}
	// A sum that stays in range must succeed.
	if got, err := Add(math.MaxInt64-1, 1); err != nil || got != math.MaxInt64 {
		t.Fatalf("Add(MaxInt64-1,1) = %d,%v; want MaxInt64,nil", got, err)
	}
}

func TestMultiplyGuards(t *testing.T) {
	t.Parallel()

	if _, err := Multiply(math.MaxInt64/2+1, 2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Multiply overflow error = %v, want ErrOverflow", err)
	}
	if _, err := Multiply(-1, 3); !errors.Is(err, ErrNegative) {
		t.Fatalf("Multiply(-1,3) error = %v, want ErrNegative", err)
	}
	if got, err := Multiply(0, math.MaxInt64); err != nil || got != 0 {
		t.Fatalf("Multiply(0,MaxInt64) = %d,%v; want 0,nil", got, err)
	}
}

func ExampleMultiply() {
	subtotal, _ := Multiply(1299, 3)
	total, _ := Add(subtotal, 250)
	fmt.Println(subtotal, total)
	// Output: 38.97 41.47
}
```

## Review

The core is correct when no input can produce a wrapped value: every overflowing
path returns `ErrOverflow` (checked with `errors.Is`), and every in-range path
returns the exact result. The two structural traps are asymmetry and after-the-fact
checking. Guarding `Add` only against `MaxInt64` passes the overflow test but leaves
`Add(MinInt64, -1)` to wrap — the underflow case is why both branches exist.
Computing `price * quantity` and then inspecting it is too late, because the product
wrapped before you looked; the `price > MaxInt64/quantity` guard is the fix, and the
zero short-circuit exists so that guard never divides by zero. Assert exact strings
for money, never floats.

## Resources

- [Go Specification: Integer overflow](https://go.dev/ref/spec#Integer_overflow) — why runtime `int64` arithmetic wraps instead of trapping.
- [`math` constants](https://pkg.go.dev/math#pkg-constants) — `MaxInt64` and `MinInt64`, the bounds the guards compare against.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors, as the tests do.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-money-json-codec-no-float.md](03-money-json-codec-no-float.md)
