# Exercise 1: Algebraic Invariants of an Integer-Cents Money Type

Money is the code where a handful of example rows is most dangerous, because the
values that break arithmetic — overflow at `math.MaxInt64`, the asymmetry of
`math.MinInt64`, zero — are exactly the ones a hand-written table omits. This
exercise upgrades the classic "math library" property test into a real `Money`
type stored as `int64` cents, and asserts its algebraic invariants (commutativity,
associativity, identity, inverse, distributivity) plus the deliberate
non-property that subtraction is not commutative, over ten thousand generated
inputs with both `testing/quick` and a preserved hand-rolled `math/rand` loop.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
money/                      independent module: example.com/money
  go.mod                    go 1.26
  money.go                  type Money (int64 cents); Add, Sub, Neg, MulScalar (saturating); String
  cmd/
    demo/
      main.go               runnable demo: build amounts, add, negate, format
  money_test.go             quick.Check/CheckEqual property tests + preserved math/rand baseline
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: a `Money int64` value type with saturating `Add`, `Sub`, `Neg`, and `MulScalar`, plus a `String` accessor for the demo.
Test: `quick.Check`/`quick.CheckEqual` properties (commutative, associative, identity, inverse, distributive, non-commutative subtraction) over bounded generated values; an explicit deterministic saturation test at the boundaries; a table-driven `String` formatter test covering the sign and `MinInt64` edges; and a preserved `math/rand` baseline loop.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/24-property-based-testing/01-money-algebraic-invariants/cmd/demo
cd go-solutions/12-testing-ecosystem/24-property-based-testing/01-money-algebraic-invariants
go mod edit -go=1.26
```

### Why a value type, and why saturation makes overflow explicit

Storing money as a floating-point dollar amount is the canonical production bug:
`0.1 + 0.2 != 0.3` in binary floating point, and a cent lost per transaction is a
reconciliation nightmare. Real systems store money as an integer number of the
smallest unit — cents — in an `int64`. `Money` here is a defined type over
`int64`, so a cents value is a `Money`, and the arithmetic lives on methods.

The subtle decision a property forces you to make is overflow. `int64` addition
wraps around silently: `MaxInt64 + 1` becomes `MinInt64`. A property that asserted
associativity while relying on wraparound would be asserting a lie — wraparound is
never the behavior a payments system wants. So `Add`, `Sub`, `Neg`, and
`MulScalar` here *saturate*: on overflow they clamp to `MaxInt64` or `MinInt64`
rather than wrap. Saturation is a deliberate, explicit choice; an alternative is to
return an error, which some ledgers prefer. Either way, the point is that writing
the property made overflow a decision instead of an accident.

Saturation has a consequence you must respect when choosing the property's input
range. Saturating addition is still commutative — clamping does not depend on
argument order. But saturating addition is *not* associative in general: with
`a = MaxInt64`, `b = 1`, `c = -1`, `(a+b)+c` saturates to `MaxInt64` then
subtracts one to `MaxInt64-1`, while `a+(b+c)` is `a+0 = MaxInt64`. The two
differ. That is not a bug in `Money`; it is a true fact about saturating
arithmetic. So the algebraic-invariant properties are asserted over a *bounded*
range of amounts small enough that no operation saturates — where saturating
arithmetic coincides exactly with real integer arithmetic and the invariants hold
exactly — and saturation itself is tested *separately* by a deterministic test at
the boundaries. Isolating the two is the senior move; conflating them produces a
property that is either wrong or silently relies on wraparound.

Create `money.go`:

```go
package money

import (
	"math"
	"strconv"
)

// Money is an amount in the smallest currency unit (cents), stored as int64 so
// arithmetic is exact. Never store money as a float.
type Money int64

// Zero is the additive identity.
const Zero Money = 0

// addSat returns a+b, clamped to [MinInt64, MaxInt64] on overflow.
func addSat(a, b int64) int64 {
	s := a + b
	// Overflow happened iff a and b share a sign that differs from the sum's.
	if a > 0 && b > 0 && s < 0 {
		return math.MaxInt64
	}
	if a < 0 && b < 0 && s >= 0 {
		return math.MinInt64
	}
	return s
}

// negSat returns -a, clamped so that -MinInt64 saturates to MaxInt64.
func negSat(a int64) int64 {
	if a == math.MinInt64 {
		return math.MaxInt64
	}
	return -a
}

// mulSat returns a*b, clamped to [MinInt64, MaxInt64] on overflow.
func mulSat(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	p := a * b
	// Overflow iff dividing the product back does not recover a operand, or the
	// MinInt64 * -1 corner overflows.
	if a == math.MinInt64 && b == -1 || b == math.MinInt64 && a == -1 || p/b != a {
		if (a > 0) == (b > 0) {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return p
}

// Add returns the saturating sum of two amounts.
func (m Money) Add(n Money) Money { return Money(addSat(int64(m), int64(n))) }

// Sub returns the saturating difference m-n.
func (m Money) Sub(n Money) Money { return Money(addSat(int64(m), negSat(int64(n)))) }

// Neg returns the saturating additive inverse -m.
func (m Money) Neg() Money { return Money(negSat(int64(m))) }

// MulScalar scales an amount by an integer factor, saturating on overflow.
func (m Money) MulScalar(k int64) Money { return Money(mulSat(int64(m), k)) }

// String renders the amount as a signed decimal with two fractional digits.
func (m Money) String() string {
	n := int64(m)
	sign := ""
	if n < 0 {
		sign = "-"
		// MinInt64 cannot be negated in place (-MinInt64 overflows int64), so
		// return its precomputed decimal form directly.
		if n == math.MinInt64 {
			return sign + "92233720368547758.08"
		}
		n = -n
	}
	dollars := n / 100
	cents := n % 100
	return sign + strconv.FormatInt(dollars, 10) + "." + pad2(cents)
}

func pad2(c int64) string {
	if c < 10 {
		return "0" + strconv.FormatInt(c, 10)
	}
	return strconv.FormatInt(c, 10)
}
```

### The runnable demo

The demo builds a few amounts, exercises each operation, and formats the results
so a reader can see the type behave against real values.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	price := money.Money(1999)   // $19.99
	tax := money.Money(160)      // $1.60
	total := price.Add(tax)      // $21.59
	refund := total.Neg()        // -$21.59
	triple := price.MulScalar(3) // $59.97

	fmt.Println("price  :", price)
	fmt.Println("tax    :", tax)
	fmt.Println("total  :", total)
	fmt.Println("refund :", refund)
	fmt.Println("triple :", triple)
	fmt.Println("net    :", total.Add(refund)) // back to zero
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
price  : 19.99
tax    : 1.60
total  : 21.59
refund : -21.59
triple : 59.97
net    : 0.00
```

### The property tests

`testing/quick` generates a custom type by having it implement `quick.Generator`.
`Money.Generate` below returns amounts in a bounded range (`±10^9` cents) so that
sums, three-way sums, and scalar products stay well inside `int64` and never
saturate — the range where the algebraic invariants hold exactly. `smallInt` is a
separate bounded generator for the scalar factor, kept small so `amount × factor`
also stays in range. Note that `quick.Generator` predates `math/rand/v2` and is
defined against `*math/rand.Rand` (version 1), so `Generate` uses that package;
this is the one place the v1 API is required.

`quick.CheckEqual(f, g, cfg)` asserts that two functions return equal output for
the same generated arguments — the natural spelling of commutativity and
associativity. `quick.Check(f, cfg)` asserts a `bool`-returning predicate. The
`TestSaturation` test is deterministic (not generated): it pins the overflow
behavior at the exact boundaries, so the properties never have to rely on it. And
`TestAddCommutativeBaseline` preserves the original hand-rolled `math/rand` loop
verbatim in spirit — a fixed-seed thousand-iteration loop — as a baseline next to
the `quick`-driven version.

Create `money_test.go`:

```go
package money

import (
	"fmt"
	"math"
	mrand "math/rand"
	"math/rand/v2"
	"reflect"
	"testing"
	"testing/quick"
)

// bound keeps generated amounts small enough that no operation saturates, so the
// algebraic invariants hold exactly (saturating arithmetic == exact arithmetic
// in this range).
const bound = 1_000_000_000

// Generate makes Money satisfy quick.Generator, drawing bounded amounts plus the
// occasional exact-zero to exercise the identity edge.
func (Money) Generate(r *mrand.Rand, _ int) reflect.Value {
	if r.Intn(20) == 0 {
		return reflect.ValueOf(Zero)
	}
	return reflect.ValueOf(Money(r.Int63n(2*bound+1) - bound))
}

// smallInt is a bounded scalar factor so amount*factor stays in range.
type smallInt int64

func (smallInt) Generate(r *mrand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(smallInt(r.Int63n(2001) - 1000))
}

func cfg() *quick.Config { return &quick.Config{MaxCount: 10000} }

func TestAddCommutative(t *testing.T) {
	t.Parallel()
	f := func(a, b Money) Money { return a.Add(b) }
	g := func(a, b Money) Money { return b.Add(a) }
	if err := quick.CheckEqual(f, g, cfg()); err != nil {
		t.Fatal(err)
	}
}

func TestAddAssociative(t *testing.T) {
	t.Parallel()
	f := func(a, b, c Money) Money { return a.Add(b).Add(c) }
	g := func(a, b, c Money) Money { return a.Add(b.Add(c)) }
	if err := quick.CheckEqual(f, g, cfg()); err != nil {
		t.Fatal(err)
	}
}

func TestAddIdentity(t *testing.T) {
	t.Parallel()
	prop := func(a Money) bool { return a.Add(Zero) == a && Zero.Add(a) == a }
	if err := quick.Check(prop, cfg()); err != nil {
		t.Fatal(err)
	}
}

func TestNegIsAdditiveInverse(t *testing.T) {
	t.Parallel()
	prop := func(a Money) bool { return a.Add(a.Neg()) == Zero }
	if err := quick.Check(prop, cfg()); err != nil {
		t.Fatal(err)
	}
}

func TestSubIsNotCommutative(t *testing.T) {
	t.Parallel()
	// A negative property: for distinct amounts, order matters.
	prop := func(a, b Money) bool {
		if a == b {
			return true // vacuously; equal operands do commute
		}
		return a.Sub(b) != b.Sub(a)
	}
	if err := quick.Check(prop, cfg()); err != nil {
		t.Fatal(err)
	}
}

func TestMulScalarDistributes(t *testing.T) {
	t.Parallel()
	// (a+b)*k == a*k + b*k
	prop := func(a, b Money, k smallInt) bool {
		left := a.Add(b).MulScalar(int64(k))
		right := a.MulScalar(int64(k)).Add(b.MulScalar(int64(k)))
		return left == right
	}
	if err := quick.Check(prop, cfg()); err != nil {
		t.Fatal(err)
	}
}

// TestSaturation pins overflow behavior at the exact boundaries, deterministically,
// so the algebraic properties above never have to reason about it.
func TestSaturation(t *testing.T) {
	t.Parallel()
	max := Money(9223372036854775807)
	min := Money(-9223372036854775808)
	if got := max.Add(1); got != max {
		t.Errorf("max.Add(1) = %d, want saturate to max", int64(got))
	}
	if got := min.Add(-1); got != min {
		t.Errorf("min.Add(-1) = %d, want saturate to min", int64(got))
	}
	if got := min.Neg(); got != max {
		t.Errorf("min.Neg() = %d, want saturate to max", int64(got))
	}
	if got := max.MulScalar(2); got != max {
		t.Errorf("max.MulScalar(2) = %d, want saturate to max", int64(got))
	}
	if got := max.MulScalar(-2); got != min {
		t.Errorf("max.MulScalar(-2) = %d, want saturate to min", int64(got))
	}
}

// TestAddCommutativeBaseline preserves the original hand-rolled math/rand loop as
// a baseline next to the quick-driven property. It is intentionally simpler and
// narrower; the quick tests above subsume and extend it.
func TestAddCommutativeBaseline(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	for range 1000 {
		a := Money(r.IntN(1000) - 500)
		b := Money(r.IntN(1000) - 500)
		if a.Add(b) != b.Add(a) {
			t.Fatalf("Add(%d,%d) != Add(%d,%d)", int64(a), int64(b), int64(b), int64(a))
		}
	}
}

// TestString covers the formatter across sign and the MinInt64 edge, whose
// magnitude cannot be produced by negation and so takes a dedicated branch.
func TestString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Money
		want string
	}{
		{"positive", Money(2159), "21.59"},
		{"negative", Money(-2159), "-21.59"},
		{"zero", Zero, "0.00"},
		{"sub-dollar", Money(7), "0.07"},
		{"max", Money(math.MaxInt64), "92233720368547758.07"},
		{"min", Money(math.MinInt64), "-92233720368547758.08"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.String(); got != tt.want {
				t.Errorf("Money(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
			}
		})
	}
}

func ExampleMoney_Add() {
	total := Money(1999).Add(Money(160))
	fmt.Println(total, total.Neg())
	// Output: 21.59 -21.59
}
```

## Review

The type is correct when every operation is a pure, deterministic function of its
`int64` operands and the saturation boundaries are exactly `MaxInt64` and
`MinInt64`. The property suite proves the algebra where it must hold: commutativity
and associativity via `CheckEqual`, `Zero` as the additive identity, `Neg` as the
additive inverse, distributivity of scalar multiplication, and the deliberate
non-commutativity of subtraction — each over ten thousand bounded inputs. The
separate `TestSaturation` proves the overflow policy at the boundaries, so no
property silently depends on wraparound.

The mistakes to avoid are specific to arithmetic types. First, do not assert the
algebraic invariants over the full `int64` range: saturating addition is not
associative once an operand saturates, so a full-range associativity property is
simply false — bound the generator to the non-saturating range and test saturation
separately, as here. Second, do not store money as a float "to keep it simple";
the whole reason for the `int64` cents representation is that float arithmetic
loses cents. Third, keep the `quick.Generator` on the v1 `*math/rand.Rand`
signature — `quick` predates `math/rand/v2`, and mismatching the signature means
the type is silently not recognized as a generator. Run `go test -race` to confirm
the value type has no shared state to race on.

## Resources

- [`testing/quick`](https://pkg.go.dev/testing/quick) — `Check`, `CheckEqual`, `Config`, and the `Generator` interface.
- [Go blog: Constants](https://go.dev/blog/constants) — why integer arithmetic is exact and floats are not, the reason money is stored as `int64` cents.
- [`math` package](https://pkg.go.dev/math#pkg-constants) — `MaxInt64` and `MinInt64`, the saturation boundaries.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-roundtrip-queryparam-codec.md](02-roundtrip-queryparam-codec.md)
