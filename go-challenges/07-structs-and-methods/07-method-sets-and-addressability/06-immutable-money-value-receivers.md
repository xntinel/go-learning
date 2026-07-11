# Exercise 6: Immutable Money Value Type — Value Receivers and Copy Semantics

The builder in module 1 used pointer receivers to mutate one object; `Money` is
the deliberate mirror. It is a small, immutable value object — an int64 of minor
units plus a currency — whose methods take value receivers, never mutate, and
return a brand-new `Money`. This module builds it, proves immutability and
comparability, and makes the case for when value semantics are the right choice.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
money/                         independent module: example.com/money
  go.mod                       module path + go directive
  money.go                     type Money; New, Add, Sub, Mul, String (value receivers)
  cmd/
    demo/
      main.go                  arithmetic that returns new values, prints formatted
  money_test.go                immutability, currency-mismatch, map-key, property tests
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Money` (int64 minor units + currency), value-receiver `Add`/`Sub` returning `(Money, error)`, `Mul(factor)` returning `Money`, and `String()` on the value receiver.
- Test: `Add`/`Sub` leave the original unchanged; currency mismatch errors; `Money` works as a map key; the property `a.Add(b).Sub(b) == a`.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p ~/go-exercises/money/cmd/demo
cd ~/go-exercises/money
go mod init example.com/money
```

### Why value receivers are correct here

`Money` holds two small fields: an `int64` amount in minor units (cents) and a
short currency code. It is immutable by design — an amount of money does not
change; you compute a new amount. That is exactly the profile where value
semantics win: the struct is tiny, copying it is a couple of machine words, it has
no identity (two `Money{100, "USD"}` values are interchangeable), and it holds no
lock or other non-copyable state.

So every method takes a value receiver and returns a new `Money` rather than
mutating the receiver. `a.Add(b)` computes a fresh `Money` and leaves `a` and `b`
untouched; the caller must use the return value. This is the opposite discipline
from the builder, and both are correct for their type: the builder has identity
and accumulates state, so it mutates through a pointer; `Money` is a value, so it
copies and returns. Value methods are in the method set of both `Money` and
`*Money`, so either form can call them — but there is nothing a pointer buys you
here, so plain values are idiomatic.

Two payoffs of value semantics show up in the tests. Because `Money` is a struct
of comparable fields with no pointers or slices, it is itself comparable with
`==` and usable as a map key — a price table keyed by `Money` just works.
Arithmetic that returns new values makes `Money` safe to share freely: no method
can mutate a `Money` another part of the program is holding.

`Add` and `Sub` return an error on a currency mismatch: adding USD to EUR is a
programming error, not a silent reinterpretation, so it surfaces as a wrapped
sentinel. `Mul` (scaling by an integer factor, e.g. quantity × unit price) cannot
mismatch currencies, so it returns just a `Money`.

Create `money.go`:

```go
package money

import (
	"errors"
	"fmt"
)

// ErrCurrencyMismatch is returned when an operation combines two currencies.
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// Money is an immutable amount in minor units (e.g. cents) of a currency. It is
// small, comparable, and copy-cheap, so its methods take value receivers and
// return new values instead of mutating the receiver.
type Money struct {
	minor    int64  // amount in the smallest unit, e.g. cents
	currency string // ISO 4217 code, e.g. "USD"
}

// New builds a Money from minor units and a currency code.
func New(minor int64, currency string) Money {
	return Money{minor: minor, currency: currency}
}

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the ISO 4217 code.
func (m Money) Currency() string { return m.currency }

// Add returns a new Money that is m plus other. Currencies must match.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("add %s to %s: %w", other.currency, m.currency, ErrCurrencyMismatch)
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

// Sub returns a new Money that is m minus other. Currencies must match.
func (m Money) Sub(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("sub %s from %s: %w", other.currency, m.currency, ErrCurrencyMismatch)
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

// Mul scales the amount by an integer factor (e.g. quantity). No currency risk.
func (m Money) Mul(factor int64) Money {
	return Money{minor: m.minor * factor, currency: m.currency}
}

// String formats the amount with two decimal places. Declared on the value
// receiver so both Money and *Money format via fmt.
func (m Money) String() string {
	sign := ""
	minor := m.minor
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, minor/100, minor%100, m.currency)
}
```

### The runnable demo

The demo does a little arithmetic — a unit price times a quantity, plus a fee —
and prints formatted amounts, showing that each operation returns a new value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	unit := money.New(1250, "USD") // $12.50
	line := unit.Mul(3)            // $37.50

	fee := money.New(199, "USD") // $1.99
	total, err := line.Add(fee)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("unit: %s\n", unit)   // original unchanged
	fmt.Printf("line: %s\n", line)   // 3 units
	fmt.Printf("total: %s\n", total) // line + fee

	if _, err := unit.Add(money.New(100, "EUR")); err != nil {
		fmt.Println("mismatch:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unit: 12.50 USD
line: 37.50 USD
total: 39.49 USD
mismatch: add EUR to USD: money: currency mismatch
```

### Tests

The tests assert immutability (the receiver is unchanged after `Add`/`Sub`),
currency-mismatch errors via `errors.Is`, that `Money` is comparable and usable as
a map key, and the round-trip property `a.Add(b).Sub(b) == a`.

Create `money_test.go`:

```go
package money

import (
	"errors"
	"fmt"
	"testing"
)

func TestAddIsImmutable(t *testing.T) {
	t.Parallel()
	a := New(1000, "USD")
	b := New(250, "USD")

	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Minor() != 1250 {
		t.Fatalf("sum = %d, want 1250", sum.Minor())
	}
	// Value receivers copy, so the originals are untouched.
	if a.Minor() != 1000 || b.Minor() != 250 {
		t.Fatalf("originals mutated: a=%d b=%d", a.Minor(), b.Minor())
	}
}

func TestCurrencyMismatch(t *testing.T) {
	t.Parallel()
	_, err := New(100, "USD").Add(New(100, "EUR"))
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add across currencies = %v, want ErrCurrencyMismatch", err)
	}
}

func TestMoneyAsMapKey(t *testing.T) {
	t.Parallel()
	// Money is comparable, so it can key a map (e.g. a price->label table).
	labels := map[Money]string{
		New(1250, "USD"): "coffee",
		New(199, "USD"):  "tip",
	}
	if labels[New(1250, "USD")] != "coffee" {
		t.Fatalf("map lookup failed: %v", labels)
	}
	if New(1250, "USD") != New(1250, "USD") {
		t.Fatal("equal Money values compared unequal")
	}
}

func TestAddSubRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct{ a, b int64 }{{1000, 250}, {0, 500}, {-300, 700}, {99, 1}}
	for _, tc := range cases {
		a := New(tc.a, "USD")
		b := New(tc.b, "USD")
		sum, err := a.Add(b)
		if err != nil {
			t.Fatal(err)
		}
		back, err := sum.Sub(b)
		if err != nil {
			t.Fatal(err)
		}
		if back != a { // property: a.Add(b).Sub(b) == a
			t.Fatalf("round trip: got %v, want %v", back, a)
		}
	}
}

func ExampleMoney_String() {
	fmt.Println(New(1250, "USD"))
	// Output: 12.50 USD
}
```

## Review

`Money` is correct when no operation mutates its receiver: `TestAddIsImmutable`
proves the originals are untouched after `Add`, which is guaranteed by the value
receiver copying the receiver on every call. `TestMoneyAsMapKey` and the round-trip
property lean on the other gift of value semantics — comparability — that a
pointer-based type would not give you for free.

The decision this module teaches is when to pick value receivers: small,
immutable, copy-cheap value objects with no identity and no locks. Pick pointer
receivers instead when the type has identity, is large, must be mutated in place,
or carries a `sync.Mutex` (module 4). Do not mix the two on one type. Declaring
`String()` on the value receiver (not the pointer) is deliberate — it keeps the
method in the value's method set so `fmt` formats a `Money` value correctly, the
subject of module 8. Run `gofmt -l`, `go vet`, and `go test -race`.

## Resources

- [Go FAQ: Should I define methods on values or pointers?](https://go.dev/doc/faq#methods_on_values_or_pointers) — the value-vs-pointer decision this module applies.
- [Go Language Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — why a struct of comparable fields is comparable and map-key usable.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the interface `String()` satisfies for formatting.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-http-handler-method-values-expressions.md](07-http-handler-method-values-expressions.md)
