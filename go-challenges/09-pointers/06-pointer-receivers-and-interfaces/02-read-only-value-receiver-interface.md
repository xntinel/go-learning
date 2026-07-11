# Exercise 2: Immutable Money Value: Value Receivers and Read-Only Interfaces

The mirror image of the mutable counter is the immutable domain value. A monetary
`Amount` — minor units plus a currency — should be copied freely and never mutated
in place. This module builds that value type with only value-receiver methods,
shows it satisfies a read-only interface from both `Amount` and `*Amount`, and
proves value semantics: a copy is independent of the original.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
money/                      independent module: example.com/money
  go.mod                    go 1.25
  amount.go                 type Amount (value receivers); ReadOnly interface; Stringer
  cmd/
    demo/
      main.go               construct, add, format amounts
  amount_test.go            value semantics, interface from T and *T, table test
```

- Files: `amount.go`, `cmd/demo/main.go`, `amount_test.go`.
- Implement: an `Amount` value type (minor units, currency) with value-receiver `Units`, `Currency`, `Add`, and `String`, and a `ReadOnly` interface (`Units`, `Currency`, `String`).
- Test: an `Amount` value (not a pointer) is assignable to `ReadOnly`; copying an `Amount` and reading the copy is independent of the original; a table over several amounts formats correctly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/money/cmd/demo
cd ~/go-exercises/money
go mod init example.com/money
go mod edit -go=1.25
```

### Why value receivers, and why both T and *T satisfy the interface

`Amount` holds `units int64` (the smallest currency unit, e.g. cents) and
`currency string`. It is immutable: `Add` does not modify the receiver, it returns
a new `Amount`. Because no method mutates and the struct is tiny, every method
takes a **value** receiver. This gives value semantics — each call and each
assignment operates on an independent copy — which is exactly what you want from a
money type: passing an `Amount` to a function cannot let that function corrupt
your copy.

A subtle payoff: an interface whose methods are *all* value receivers is satisfied
by both `T` and `*T`. The method set of `Amount` contains the value methods
(satisfying `ReadOnly`), and the method set of `*Amount` contains those same value
methods *plus* any pointer methods (there are none), so it also satisfies
`ReadOnly`. That is why both `var _ ReadOnly = Amount{}` and
`var _ ReadOnly = (*Amount)(nil)` compile. Contrast this with the counter from
Exercise 1, whose pointer-receiver interface was satisfied only by `*T`.

Create `amount.go`:

```go
// amount.go
package money

import "fmt"

// Amount is an immutable monetary value in minor units (e.g. cents) plus an ISO
// currency code. All methods have value receivers, so copies are safe and Add
// returns a new Amount rather than mutating the receiver.
type Amount struct {
	units    int64
	currency string
}

// ReadOnly is the read side of a monetary value. Every method has a value
// receiver on Amount, so both Amount and *Amount satisfy ReadOnly.
type ReadOnly interface {
	Units() int64
	Currency() string
	String() string
}

// Compile-time contracts: a value and a pointer both satisfy ReadOnly.
var (
	_ ReadOnly = Amount{}
	_ ReadOnly = (*Amount)(nil)
)

// New returns an Amount of the given minor units and currency.
func New(units int64, currency string) Amount {
	return Amount{units: units, currency: currency}
}

// Units returns the value in minor units.
func (a Amount) Units() int64 { return a.units }

// Currency returns the ISO currency code.
func (a Amount) Currency() string { return a.currency }

// Add returns a new Amount that is the sum; the receiver is unchanged. It panics
// on a currency mismatch, which is a programming error, not a runtime condition.
func (a Amount) Add(b Amount) Amount {
	if a.currency != b.currency {
		panic(fmt.Sprintf("money: cannot add %s and %s", a.currency, b.currency))
	}
	return Amount{units: a.units + b.units, currency: a.currency}
}

// String formats the amount as a major-unit decimal with two places, e.g.
// "12.34 USD". It uses a value receiver so fmt formats every Amount uniformly.
func (a Amount) String() string {
	sign := ""
	u := a.units
	if u < 0 {
		sign = "-"
		u = -u
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, u/100, u%100, a.currency)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	price := money.New(1299, "USD")
	tax := money.New(104, "USD")
	total := price.Add(tax)

	// price is unchanged by Add: value semantics.
	fmt.Printf("price: %s\n", price)
	fmt.Printf("tax:   %s\n", tax)
	fmt.Printf("total: %s\n", total)

	var r money.ReadOnly = total // an Amount value satisfies ReadOnly
	fmt.Printf("units: %d %s\n", r.Units(), r.Currency())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
price: 12.99 USD
tax:   1.04 USD
total: 14.03 USD
units: 1403 USD
```

### Tests

`TestValueSatisfiesReadOnly` assigns an `Amount` *value* (not a pointer) to a
`ReadOnly` variable. `TestCopyIsIndependent` proves value semantics: it copies an
`Amount`, adds to the copy, and asserts the original is untouched — the guarantee
that makes a money type safe to pass around. The table test formats several
amounts, including a negative and a sub-dollar value.

Create `amount_test.go`:

```go
// amount_test.go
package money

import (
	"fmt"
	"testing"
)

func TestValueSatisfiesReadOnly(t *testing.T) {
	t.Parallel()

	var r ReadOnly = New(500, "EUR") // Amount value, not pointer
	if r.Units() != 500 || r.Currency() != "EUR" {
		t.Fatalf("ReadOnly = %d %s, want 500 EUR", r.Units(), r.Currency())
	}
}

func TestPointerSatisfiesReadOnly(t *testing.T) {
	t.Parallel()

	a := New(750, "GBP")
	var r ReadOnly = &a
	if r.Units() != 750 {
		t.Fatalf("Units() = %d, want 750", r.Units())
	}
}

func TestCopyIsIndependent(t *testing.T) {
	t.Parallel()

	orig := New(1000, "USD")
	cp := orig                   // value copy
	cp = cp.Add(New(500, "USD")) // mutate only the copy's binding

	if orig.Units() != 1000 {
		t.Fatalf("original changed to %d, want 1000 (value semantics broken)", orig.Units())
	}
	if cp.Units() != 1500 {
		t.Fatalf("copy = %d, want 1500", cp.Units())
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		units int64
		cur   string
		want  string
	}{
		{"whole", 1200, "USD", "12.00 USD"},
		{"cents", 1299, "USD", "12.99 USD"},
		{"sub-dollar", 5, "USD", "0.05 USD"},
		{"negative", -1299, "EUR", "-12.99 EUR"},
		{"zero", 0, "JPY", "0.00 JPY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := New(tc.units, tc.cur).String()
			if got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func ExampleAmount() {
	a := New(2550, "USD")
	fmt.Println(a)
	// Output: 25.50 USD
}
```

## Review

The type is correct when no method can mutate the receiver and `Add` returns a
fresh value: `TestCopyIsIndependent` is the assertion that pins this — if `Add`
were ever changed to mutate `a` in place, the original would move and the test
would fail. The two compile-time contracts document the read-only property from
both `Amount` and `*Amount`; that both compile is the concrete demonstration that
an all-value-receiver interface is satisfied by the value and the pointer alike.
Defining `String` on a value receiver (not a pointer) is deliberate — it is what
lets `fmt.Printf("%s", amount)` format every `Amount`, including elements of a
slice or map, uniformly (the failure mode when it is a pointer receiver is the
subject of Exercise 8).

## Resources

- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when value receivers are the right call.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the interface fmt consults for `%v`/`%s`.
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — why both `T` and `*T` satisfy an all-value-receiver interface.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-counter-service-pointer-receivers.md](01-counter-service-pointer-receivers.md) | Next: [03-http-handler-pointer-receiver.md](03-http-handler-pointer-receiver.md)
