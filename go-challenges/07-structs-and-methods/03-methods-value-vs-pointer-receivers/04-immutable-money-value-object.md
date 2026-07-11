# Exercise 4: An Immutable Money Value Object (Value Receivers Done Right)

Pointer receivers are for mutable state; value receivers are for values. This
module builds the canonical value object — a `Money` amount in a payments domain —
and shows the inverse rule of the first three exercises: when a type is a small
immutable value, value receivers whose methods RETURN new values are the correct,
safer design. No shared mutable state, no aliasing bugs, and the type stays
comparable so it can be a map key.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
money/                     independent module: example.com/money
  go.mod
  money.go                 type Money (value receivers); Add, Sub, Mul, Equal, String
  cmd/
    demo/
      main.go              add two amounts; reject a currency mismatch
  money_test.go            immutability, currency-mismatch error, value equality, map key
```

Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
Implement: `Money{amount int64, currency string}` with value-receiver methods `Add`/`Sub` (returning `(Money, error)`), `Mul(factor int64) Money`, `Equal(Money) bool`, and a `fmt.Stringer`; `Add`/`Sub` return `ErrCurrencyMismatch` on differing currencies.
Test: same-currency add, currency-mismatch error via `errors.Is`, the receiver is NOT mutated by `Add`, value equality, and `Money` usable as a map key.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/money/cmd/demo
cd ~/go-exercises/money
go mod init example.com/money
```

### Why value receivers are correct here

`Money` is a *value*: an amount of `1050` minor units in `USD` is 10.50 dollars,
full stop, and it never changes into something else — you compute new amounts
from it. Modeling money as mutable state invites the worst class of financial
bug: two references to the same `*Money`, one of them mutating the balance out
from under the other. Value semantics make that impossible. Each `Money` is an
independent copy; there is nothing to alias.

So every method takes a *value* receiver and, instead of mutating, returns a
*new* `Money`. `Add` does not change the receiver; it computes and returns a fresh
value. This is the discipline the standard library uses for `time.Time` and
`time.Duration`, and it buys three things: the type is race-free without any lock,
it is `comparable` (so `==` and map keys work — the amount is `int64` and the
currency is `string`, both comparable), and no method can produce a spooky
action-at-a-distance mutation.

Amounts are stored as `int64` *minor units* (cents), never `float64`. Floating
point cannot represent `0.10` exactly and silently accumulates rounding error
across a ledger; integer minor units are exact. `Add` and `Sub` refuse to combine
different currencies — adding USD to EUR is a domain error, not an arithmetic one
— and return the package sentinel `ErrCurrencyMismatch` wrapped with `%w` so
callers can both read a descriptive message and match the sentinel with
`errors.Is`.

Create `money.go`:

```go
package money

import (
	"errors"
	"fmt"
)

// ErrCurrencyMismatch is returned when an operation mixes two currencies.
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// Money is an immutable amount in minor units (e.g. cents) of a currency. It is
// a value object: methods take value receivers and RETURN new Money values
// rather than mutating. Both fields are comparable, so Money supports == and is
// usable as a map key.
type Money struct {
	amount   int64
	currency string
}

// New builds a Money value. amount is in minor units.
func New(amount int64, currency string) Money {
	return Money{amount: amount, currency: currency}
}

// Amount returns the raw minor-unit amount.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the ISO-like currency code.
func (m Money) Currency() string { return m.currency }

// Add returns a new Money equal to m plus other, or an error if the currencies
// differ. The receiver m is NOT modified.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("add %s to %s: %w", other.currency, m.currency, ErrCurrencyMismatch)
	}
	return Money{amount: m.amount + other.amount, currency: m.currency}, nil
}

// Sub returns a new Money equal to m minus other, or an error on mismatch.
func (m Money) Sub(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("sub %s from %s: %w", other.currency, m.currency, ErrCurrencyMismatch)
	}
	return Money{amount: m.amount - other.amount, currency: m.currency}, nil
}

// Mul scales the amount by an integer factor, returning a new Money.
func (m Money) Mul(factor int64) Money {
	return Money{amount: m.amount * factor, currency: m.currency}
}

// Equal reports whether two amounts are equal in both amount and currency.
func (m Money) Equal(other Money) bool {
	return m == other
}

// String renders the amount with two decimal places and its currency.
func (m Money) String() string {
	sign := ""
	a := m.amount
	if a < 0 {
		sign = "-"
		a = -a
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, a/100, a%100, m.currency)
}
```

### The runnable demo

The demo adds two USD amounts and then tries to add EUR to USD, printing the
domain error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/money"
)

func main() {
	price := money.New(1050, "USD") // 10.50 USD
	tax := money.New(84, "USD")     //  0.84 USD

	total, err := price.Add(tax)
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("total: %s\n", total)

	// The original is untouched by Add: value receivers do not mutate.
	fmt.Printf("price still: %s\n", price)

	if _, err := price.Add(money.New(500, "EUR")); errors.Is(err, money.ErrCurrencyMismatch) {
		fmt.Printf("rejected: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total: 11.34 USD
price still: 10.50 USD
rejected: add EUR to USD: money: currency mismatch
```

### Tests

The two tests that carry the lesson are `TestOriginalUnchangedAfterAdd`, which
proves the value-object contract that `Add` does not mutate its receiver, and
`TestMoneyIsUsableAsMapKey`, which only compiles because `Money` is comparable —
the property a pointer receiver would not have cost you but a mutable design
would. The rest cover same-currency arithmetic, the mismatch error, and value
equality.

Create `money_test.go`:

```go
package money

import (
	"errors"
	"fmt"
	"testing"
)

func TestAddSameCurrency(t *testing.T) {
	t.Parallel()

	got, err := New(1050, "USD").Add(New(84, "USD"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := New(1134, "USD"); !got.Equal(want) {
		t.Fatalf("Add = %v, want %v", got, want)
	}
}

func TestAddMismatchedCurrencyReturnsError(t *testing.T) {
	t.Parallel()

	_, err := New(1050, "USD").Add(New(500, "EUR"))
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestSubMismatchedCurrencyReturnsError(t *testing.T) {
	t.Parallel()

	_, err := New(1050, "USD").Sub(New(500, "EUR"))
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestOriginalUnchangedAfterAdd(t *testing.T) {
	t.Parallel()

	original := New(1050, "USD")
	if _, err := original.Add(New(84, "USD")); err != nil {
		t.Fatal(err)
	}
	// Add returned a new value; the receiver must be untouched.
	if !original.Equal(New(1050, "USD")) {
		t.Fatalf("receiver mutated to %v; a value object must be immutable", original)
	}
}

func TestEqualIsValueEquality(t *testing.T) {
	t.Parallel()

	a := New(1050, "USD")
	b := New(1050, "USD")
	c := New(1050, "EUR")

	if !a.Equal(b) {
		t.Fatal("equal amounts in the same currency should be Equal")
	}
	if a.Equal(c) {
		t.Fatal("same amount in different currencies must not be Equal")
	}
}

func TestMoneyIsUsableAsMapKey(t *testing.T) {
	t.Parallel()

	// This only compiles because Money is comparable (value receivers, no
	// pointer fields). A pointer-heavy or mutable design would not allow it.
	counts := map[Money]int{}
	counts[New(1050, "USD")]++
	counts[New(1050, "USD")]++
	if got := counts[New(1050, "USD")]; got != 2 {
		t.Fatalf("map[Money] count = %d, want 2", got)
	}
}

func TestMulScales(t *testing.T) {
	t.Parallel()

	if got := New(1050, "USD").Mul(3); !got.Equal(New(3150, "USD")) {
		t.Fatalf("Mul(3) = %v, want 31.50 USD", got)
	}
}

func ExampleMoney_String() {
	fmt.Println(New(1134, "USD"))
	fmt.Println(New(-75, "EUR"))
	// Output:
	// 11.34 USD
	// -0.75 EUR
}
```

## Review

`Money` is correct when arithmetic is exact, currency mixing is an error, and —
the crux — no operation ever mutates a receiver: `TestOriginalUnchangedAfterAdd`
is the value-object contract in one assertion. The design choices reinforce each
other: `int64` minor units keep the math exact, value receivers keep every amount
independent and race-free, and comparable fields let `Money` serve as a map key,
which `TestMoneyIsUsableAsMapKey` exercises. The mistake to resist is switching to
pointer receivers "for performance" on a two-field struct — you would gain
nothing measurable and lose comparability and immutability, trading a safe value
for an aliasable one.

## Resources

- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — small immutable types take value receivers.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the `String()` method `fmt` calls for a value.
- [Go Spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — why a struct of comparable fields is comparable and usable as a map key.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-concurrent-safe-counter.md](03-concurrent-safe-counter.md) | Next: [05-silent-mutation-loss-bug.md](05-silent-mutation-loss-bug.md)
