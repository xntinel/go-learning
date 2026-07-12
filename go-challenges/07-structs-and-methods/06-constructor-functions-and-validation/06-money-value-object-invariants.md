# Exercise 6: Enforce Non-Negotiable Invariants in a Money Value Object

Money is the canonical value object: an amount in minor units plus a currency,
where a bypassable invariant is a correctness bug in billing. This exercise builds
a `Money` type whose fields are unexported, constructed through `NewMoney` that
rejects unknown currencies, with `Add`/`Sub` that refuse to combine mismatched
currencies rather than silently producing garbage.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
money/                       independent module: example.com/money-value-object-invariants
  go.mod
  money.go                   Money (unexported amount, currency), NewMoney, Add, Sub, String, accessors, sentinels
  cmd/
    demo/
      main.go                constructs, adds, and prints amounts including a negative balance
  money_test.go              construction rejects/normalizes, arithmetic, mismatch, zero inert, String format
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `NewMoney(amount int64, currency string) (Money, error)` plus `Add`, `Sub`, `String`, and accessors, with unexported fields.
- Test: construction rejects unknown/empty currency and normalizes case; matching-currency `Add`/`Sub` are correct; mismatched `Add` returns `ErrCurrencyMismatch` via `errors.Is`; the zero value is inert; and `String()` renders the documented format including negatives.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/06-constructor-functions-and-validation/06-money-value-object-invariants/cmd/demo
cd go-solutions/07-structs-and-methods/06-constructor-functions-and-validation/06-money-value-object-invariants
```

### Why the invariant must be unbypassable

The `Money` invariant is "amount is in minor units of a known currency". If a
caller could write `Money{amount: 100, currency: "XYZ"}` directly, the constructor's
currency check would be theater — some code path would eventually build a `Money`
in a currency the rest of the system does not understand, and the bug would
surface as a mispriced invoice far from its cause. Unexported fields close that
door: the only way to obtain a `Money` is `NewMoney`, which uppercases the currency
(so `"usd"` and `"USD"` are the same value) and rejects anything outside the known
set. The type is immutable — value receivers, no setters — so a constructed
`Money` stays valid for its whole life and is safe to share and to use as a map
key.

`Add` and `Sub` enforce the second invariant: arithmetic only combines matching
currencies. Adding dollars to euros is not a number, it is a bug, so the methods
return `ErrCurrencyMismatch` instead of quietly summing the minor units. Amounts
are stored as `int64` minor units — cents — never `float64`, because binary
floating point cannot represent `0.10` exactly and money must be exact. `String`
renders the minor units as a fixed two-decimal amount with the currency prefix,
handling negatives so an overdrawn balance reads correctly.

Create `money.go`:

```go
package money

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrEmptyCurrency    = errors.New("currency is required")
	ErrUnknownCurrency  = errors.New("unknown currency")
	ErrCurrencyMismatch = errors.New("currency mismatch")
)

var knownCurrencies = map[string]bool{
	"USD": true,
	"EUR": true,
	"GBP": true,
	"JPY": true,
}

// Money is an exact monetary amount in minor units (e.g. cents) of a known
// currency. Its fields are unexported: the only way to obtain a valid Money is
// NewMoney, and it is immutable thereafter.
type Money struct {
	amount   int64
	currency string
}

// NewMoney constructs a Money, normalizing the currency to upper case and
// rejecting the empty or unknown ones.
func NewMoney(amount int64, currency string) (Money, error) {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" {
		return Money{}, ErrEmptyCurrency
	}
	if !knownCurrencies[c] {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, currency)
	}
	return Money{amount: amount, currency: c}, nil
}

// Amount returns the amount in minor units.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the ISO currency code.
func (m Money) Currency() string { return m.currency }

// Add returns the sum of m and other, or ErrCurrencyMismatch if their
// currencies differ.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return Money{amount: m.amount + other.amount, currency: m.currency}, nil
}

// Sub returns m minus other, or ErrCurrencyMismatch if their currencies differ.
func (m Money) Sub(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return Money{amount: m.amount - other.amount, currency: m.currency}, nil
}

// String renders the amount as "CUR major.minor", e.g. "USD 12.34" or
// "USD -5.00".
func (m Money) String() string {
	a := m.amount
	sign := ""
	if a < 0 {
		sign = "-"
		a = -a
	}
	return fmt.Sprintf("%s %s%d.%02d", m.currency, sign, a/100, a%100)
}
```

### The runnable demo

The demo constructs two amounts, adds them, subtracts to a negative balance, and
shows a rejected cross-currency add.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/money-value-object-invariants"
)

func main() {
	price, _ := money.NewMoney(1299, "usd")
	discount, _ := money.NewMoney(300, "USD")

	total, _ := price.Sub(discount)
	fmt.Printf("total: %s\n", total)

	overdraft, _ := money.NewMoney(2000, "USD")
	balance, _ := total.Sub(overdraft)
	fmt.Printf("balance: %s\n", balance)

	eur, _ := money.NewMoney(500, "EUR")
	if _, err := total.Add(eur); err != nil {
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
total: USD 9.99
balance: USD -10.01
rejected: currency mismatch: USD + EUR
```

### Tests

`TestMismatchRejected` is the correctness guard: adding across currencies must
error, never silently sum. `TestZeroValueInert` shows the zero `Money` has an
empty currency, so combining it with a real amount is a mismatch — the zero value
cannot masquerade as a valid balance.

Create `money_test.go`:

```go
package money

import (
	"errors"
	"fmt"
	"testing"
)

func TestConstructionNormalizesAndRejects(t *testing.T) {
	t.Parallel()
	m, err := NewMoney(100, "usd")
	if err != nil || m.Currency() != "USD" {
		t.Fatalf("NewMoney normalized wrong: %q, %v", m.Currency(), err)
	}
	if _, err := NewMoney(100, ""); !errors.Is(err, ErrEmptyCurrency) {
		t.Fatalf("empty currency err = %v, want ErrEmptyCurrency", err)
	}
	if _, err := NewMoney(100, "XYZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("unknown currency err = %v, want ErrUnknownCurrency", err)
	}
}

func TestArithmetic(t *testing.T) {
	t.Parallel()
	a, _ := NewMoney(1000, "USD")
	b, _ := NewMoney(250, "USD")
	sum, err := a.Add(b)
	if err != nil || sum.Amount() != 1250 {
		t.Fatalf("Add = %d, %v", sum.Amount(), err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != 750 {
		t.Fatalf("Sub = %d, %v", diff.Amount(), err)
	}
}

func TestMismatchRejected(t *testing.T) {
	t.Parallel()
	usd, _ := NewMoney(1000, "USD")
	eur, _ := NewMoney(1000, "EUR")
	if _, err := usd.Add(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("cross-currency Add err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Sub(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("cross-currency Sub err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestZeroValueInert(t *testing.T) {
	t.Parallel()
	var zero Money
	if zero.Currency() != "" || zero.Amount() != 0 {
		t.Fatalf("zero Money should be empty, got %+v", zero)
	}
	usd, _ := NewMoney(100, "USD")
	if _, err := usd.Add(zero); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("adding zero-value Money should mismatch, got %v", err)
	}
}

func TestStringFormat(t *testing.T) {
	t.Parallel()
	tests := map[int64]string{
		1234: "USD 12.34",
		5:    "USD 0.05",
		1000: "USD 10.00",
		-501: "USD -5.01",
	}
	for amount, want := range tests {
		m, _ := NewMoney(amount, "USD")
		if got := m.String(); got != want {
			t.Fatalf("String(%d) = %q, want %q", amount, got, want)
		}
	}
}

func ExampleNewMoney() {
	m, _ := NewMoney(1299, "usd")
	fmt.Println(m)
	// Output: USD 12.99
}
```

## Review

The value object is correct when construction is the only way in, unknown and
empty currencies are rejected, case is normalized, and arithmetic refuses to
combine mismatched currencies. The design decisions worth naming: amounts are
`int64` minor units, never `float64`, because money must be exact; the fields are
unexported so no package can mutate a `Money` past its invariant; and the zero
value is inert (empty currency) so it cannot pass as a real balance. The mistake
to avoid is exporting the fields "for convenience", which makes the constructor's
validation meaningless the moment another package assigns them directly.

## Resources

- [errors package](https://pkg.go.dev/errors) — sentinels and `errors.Is` for the mismatch branch.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the interface `String()` satisfies for human formatting.
- [Martin Fowler: Value Object](https://martinfowler.com/bliki/ValueObject.html) — the pattern and why immutability matters.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-validate-interface-pipeline.md](05-validate-interface-pipeline.md) | Next: [07-must-constructor-init-invariants.md](07-must-constructor-init-invariants.md)
