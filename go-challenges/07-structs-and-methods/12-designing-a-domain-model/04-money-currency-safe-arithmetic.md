# Exercise 4: Currency-Safe Money to Prevent Mismatched-Currency Bugs

Money is the value object every backend gets wrong at least once, and the two
classic bugs are float arithmetic that loses cents and adding two amounts of
different currencies. This module builds a `Money` type that stores an `int64` of
minor units plus a `Currency`, and whose `Add`/`Sub` reject a currency mismatch —
turning a silent data-corruption bug into a loud, rejected operation.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
money/                      independent module: example.com/money
  go.mod                    go 1.26
  money.go                  type Currency; type Money (int64 minor units); NewMoney, Add, Sub, String
  cmd/
    demo/
      main.go               runnable demo: add same-currency, reject cross-currency
  money_test.go             tests: same-currency sum, ErrCurrencyMismatch, unknown currency, int64-only
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Money` with an `int64` `minor` field and a `Currency`, `NewMoney` validating a known currency, `Add`/`Sub` rejecting a mismatch with `ErrCurrencyMismatch`, and a `String` for display.
- Test: `Add(USD, USD)` sums minor units; `Add(USD, EUR)` returns `ErrCurrencyMismatch`; `NewMoney` with an unknown currency is rejected; amounts stay `int64` (no float path exists).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/money/cmd/demo
cd ~/go-exercises/money
go mod init example.com/money
```

### Integer minor units and a currency tag

`float64` cannot represent `0.10` exactly; it stores the nearest binary fraction,
and a long chain of additions on cents drifts until a reconciliation report is off
by a penny no one can trace. The industry-standard fix is to never let a float
touch money: store the amount as an integer count of the *minor unit* — cents for
USD, pence for GBP — in an `int64`, and do every arithmetic operation on those
integers, where `10 + 20` is exactly `30`, always. `Money` here has an
`int64` field named `minor` and no float anywhere; that absence is the design.

The second correctness property is the currency tag. An amount without a currency
is just a number, and adding `100` USD-cents to `100` EUR-cents to produce `200`
of nothing is silent corruption. `Money` carries a `Currency`, and `Add`/`Sub`
compare the two currencies first, returning `ErrCurrencyMismatch` when they
differ. The mismatched-currency bug becomes a rejected operation at the exact call
site that caused it, instead of a corrupt total that surfaces three services
downstream.

`NewMoney` validates that the currency is one the system knows — an unknown code
is rejected with `ErrUnknownCurrency` — so a `Money` that exists always carries a
real currency. The policy here allows negative amounts (a ledger needs debits and
credits, refunds, and adjustments), and that choice is documented on the
constructor; a different domain might forbid negatives, but the point is to make
the policy explicit rather than accidental. `Money` is a value object: immutable
(operations return new values), comparable (both fields are comparable, so `==`
works and it is a valid map key), and defined entirely by its amount and currency.

Create `money.go`:

```go
package money

import (
	"errors"
	"fmt"
)

var (
	ErrUnknownCurrency  = errors.New("money: unknown currency")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
)

// Currency is an ISO-4217-style code. Only known codes may be constructed.
type Currency string

const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
)

// minorDigits maps a known currency to its number of minor-unit decimal places.
var minorDigits = map[Currency]int{USD: 2, EUR: 2, GBP: 2}

// Money is an immutable value object: an integer count of minor units (e.g.
// cents) plus a currency. There is no float field, so cash arithmetic is exact.
type Money struct {
	minor    int64
	currency Currency
}

// NewMoney builds Money from minor units. It rejects an unknown currency.
// Policy: negative amounts are allowed (ledgers need debits and refunds).
func NewMoney(minor int64, currency Currency) (Money, error) {
	if _, ok := minorDigits[currency]; !ok {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, currency)
	}
	return Money{minor: minor, currency: currency}, nil
}

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the currency code.
func (m Money) Currency() Currency { return m.currency }

// Add returns a new Money or ErrCurrencyMismatch if the currencies differ.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

// Sub returns a new Money or ErrCurrencyMismatch if the currencies differ.
func (m Money) Sub(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

// String renders the amount with its minor-unit decimal places, e.g. "10.50 USD".
func (m Money) String() string {
	digits := minorDigits[m.currency]
	div := int64(1)
	for range digits {
		div *= 10
	}
	whole := m.minor / div
	frac := m.minor % div
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%d.%0*d %s", whole, digits, frac, m.currency)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/money"
)

func main() {
	price, _ := money.NewMoney(1050, money.USD) // $10.50
	tax, _ := money.NewMoney(84, money.USD)     // $0.84

	total, _ := price.Add(tax)
	fmt.Printf("total: %s\n", total)

	euros, _ := money.NewMoney(500, money.EUR)
	if _, err := price.Add(euros); errors.Is(err, money.ErrCurrencyMismatch) {
		fmt.Println("cross-currency add rejected")
	}

	if _, err := money.NewMoney(100, money.Currency("XYZ")); errors.Is(err, money.ErrUnknownCurrency) {
		fmt.Println("unknown currency rejected")
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
cross-currency add rejected
unknown currency rejected
```

### Tests

The tests pin exactness and the currency guard. Same-currency `Add` sums the
`int64` minor units precisely; a cross-currency `Add` and `Sub` return
`ErrCurrencyMismatch`; an unknown currency is rejected at construction; and a
repeated-addition test demonstrates the exactness that a float would lose — adding
one cent ten thousand times lands on exactly `10000`, not `9999.9999...`.

Create `money_test.go`:

```go
package money

import (
	"errors"
	"testing"
)

func TestAddSameCurrency(t *testing.T) {
	t.Parallel()
	a, _ := NewMoney(1050, USD)
	b, _ := NewMoney(84, USD)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Minor() != 1134 {
		t.Fatalf("sum.Minor = %d, want 1134", sum.Minor())
	}
	if sum.Currency() != USD {
		t.Fatalf("sum.Currency = %s, want USD", sum.Currency())
	}
}

func TestAddCrossCurrencyRejected(t *testing.T) {
	t.Parallel()
	usd, _ := NewMoney(100, USD)
	eur, _ := NewMoney(100, EUR)
	if _, err := usd.Add(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Sub(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestUnknownCurrencyRejected(t *testing.T) {
	t.Parallel()
	if _, err := NewMoney(100, Currency("XYZ")); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("NewMoney err = %v, want ErrUnknownCurrency", err)
	}
}

func TestExactRepeatedAddition(t *testing.T) {
	t.Parallel()
	acc, _ := NewMoney(0, USD)
	oneCent, _ := NewMoney(1, USD)
	for range 10000 {
		acc, _ = acc.Add(oneCent)
	}
	if acc.Minor() != 10000 {
		t.Fatalf("after 10000 additions Minor = %d, want 10000 (float would drift)", acc.Minor())
	}
}

func TestString(t *testing.T) {
	t.Parallel()
	m, _ := NewMoney(1050, USD)
	if got := m.String(); got != "10.50 USD" {
		t.Fatalf("String = %q, want 10.50 USD", got)
	}
}
```

## Review

`Money` is correct when no float ever touches it and every cross-currency
operation is rejected. The repeated-addition test is the honest demonstration:
ten thousand one-cent additions land on exactly `10000` because the arithmetic is
integer; the same loop on a `float64` accumulates rounding error. The mistakes to
avoid are the two named in the concepts: representing money as `float64` (which
silently loses cents), and adding two amounts of different currencies with no
guard (which corrupts a total that then propagates). Note the policy decision —
negatives are allowed here for ledger use — is documented on the constructor
rather than left implicit.

## Resources

- [Floating-Point Arithmetic (Go Blog: constants)](https://go.dev/blog/constants) — why binary floats cannot represent decimal fractions exactly.
- [`fmt` package](https://pkg.go.dev/fmt) — `fmt.Errorf` with `%w`, and `%0*d` width formatting.
- [ISO 4217 currency codes](https://www.iso.org/iso-4217-currency-codes.html) — the standard the `Currency` type mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-value-object-equality-and-map-keys.md](03-value-object-equality-and-map-keys.md) | Next: [05-constructor-functional-options.md](05-constructor-functional-options.md)
