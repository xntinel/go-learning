# Exercise 2: Money Value Object — ExampleT and ExampleT_M Naming

Every payments or billing backend owns a money type: an integer number of cents
with arithmetic and a formatting method, never a `float64`. Here you build that
type and document it with examples whose names attach precisely to the type and
to its methods, learning the `ExampleT` / `ExampleT_M` convention that decides
where each example lands on `pkg.go.dev`.

## What you'll build

```text
money/                      independent module: example.com/money
  go.mod                    go 1.26
  money.go                  type Money (cents); Add(Money) Money; String() string
  cmd/
    demo/
      main.go               runnable demo formatting and adding amounts
  money_test.go             table-driven Test + ExampleMoney, ExampleMoney_Add, ExampleMoney_String
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: `Money` as `int64` cents with `Add(Money) Money` and a `String() string` satisfying `fmt.Stringer`.
Test: a table-driven `Test`, plus `ExampleMoney` (type-level), `ExampleMoney_Add` (method), `ExampleMoney_String` (method).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/15-testable-examples/02-money-value-object-method-examples/cmd/demo
cd go-solutions/12-testing-ecosystem/15-testable-examples/02-money-value-object-method-examples
```

## Naming attaches an example to a symbol

The suffix after `Example` is an address into the documentation graph.
`ExampleMoney` (no further suffix) attaches to the *type* `Money` and renders at
the top of the type's section on `pkg.go.dev`. `ExampleMoney_Add` attaches to the
*method* `Add` on `Money`: the single underscore separates the type name from the
method name. This is the rule that is easy to fumble. Write `ExampleMoneyAdd`
without the underscore and Go cannot see a `MoneyAdd` symbol, so the example
detaches from `Money.Add` and renders as a stray package-level example next to
nothing. The ground-truth check is `go doc example.Money.Add`: when the naming is
right, the example appears under the method; rename it to `ExampleMoneyAdd` and it
vanishes from the method's doc. That single underscore is the whole convention.

`Money` stores cents as an `int64` so arithmetic is exact — floating-point money
is a classic production bug, where `0.1 + 0.2` is not `0.3`. `String` formats
dollars and cents; because it has the signature `String() string`, `Money`
satisfies `fmt.Stringer`, so `fmt.Println(m)` calls it automatically. That is why
the examples can print a `Money` directly and get `$13.25` rather than the raw
integer.

Create `money.go`:

```go
package money

import "fmt"

// Money is an amount in whole cents. Storing cents as an integer keeps
// arithmetic exact; a float64 would accumulate rounding error.
type Money int64

// Add returns the sum of m and other.
func (m Money) Add(other Money) Money {
	return m + other
}

// String formats the amount as dollars and cents, e.g. Money(1325) is "$13.25".
// Satisfying fmt.Stringer means fmt.Println(m) formats it automatically.
func (m Money) String() string {
	return fmt.Sprintf("$%d.%02d", m/100, m%100)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money"
)

func main() {
	price := money.Money(1250)
	fee := money.Money(75)
	total := price.Add(fee)

	fmt.Println("price:", price)
	fmt.Println("fee:  ", fee)
	fmt.Println("total:", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
price: $12.50
fee:   $0.75
total: $13.25
```

### Tests and examples

The table-driven `Test` checks `Add` and `String` across amounts; the examples
document the type and its methods. Watch the naming: `ExampleMoney` documents the
type, `ExampleMoney_Add` and `ExampleMoney_String` document methods via the
underscore.

Create `money_test.go`:

```go
package money

import (
	"fmt"
	"testing"
)

func TestMoney(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		amount Money
		want   string
	}{
		{"dollars and cents", 1325, "$13.25"},
		{"whole dollars", 1000, "$10.00"},
		{"cents only", 5, "$0.05"},
		{"zero", 0, "$0.00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.amount.String(); got != tt.want {
				t.Errorf("Money(%d).String() = %q, want %q", int64(tt.amount), got, tt.want)
			}
		})
	}

	if got := Money(1250).Add(75); got != 1325 {
		t.Errorf("Add: got %d, want 1325", int64(got))
	}
}

func ExampleMoney() {
	price := Money(999)
	fmt.Println(price)
	// Output: $9.99
}

func ExampleMoney_Add() {
	total := Money(1250).Add(Money(75))
	fmt.Println(total)
	// Output: $13.25
}

func ExampleMoney_String() {
	fmt.Println(Money(5).String())
	// Output: $0.05
}
```

## Review

The type is correct when arithmetic stays in integer cents and `String`
round-trips an amount to its `$D.CC` form, which the table pins for whole
dollars, cents-only, and zero. The examples are correct when each prints exactly
its `// Output:` line. The lesson-specific trap is the method-naming rule:
confirm with `go doc example.Money.Add` that `ExampleMoney_Add` renders under the
method, then imagine renaming it to `ExampleMoneyAdd` and watch it detach — the
underscore is load-bearing. Keep `gofmt -l` empty and `go vet ./...` clean; run
`go test -run 'ExampleMoney'` to see the method examples execute.

## Resources

- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — the `ExampleT` and `ExampleT_M` naming rules.
- [go/doc — Example](https://pkg.go.dev/go/doc#Example) — how examples are extracted and associated with symbols.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — why implementing `String() string` makes `fmt` format the value automatically.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-math-library-examples.md](01-math-library-examples.md) | Next: [03-json-response-deterministic-output.md](03-json-response-deterministic-output.md)
