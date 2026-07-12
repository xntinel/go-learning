# Exercise 1: A Money Balance as an Immutable Value Object

A `Balance` is the archetypal value object: no identity, defined entirely by its
fields, immutable, and comparable. This module builds it the production way —
unexported fields, a validating constructor that refuses a negative balance, and
pure `Add`/`Sub` operations that return new values with an overdraw guard rather
than mutating in place.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
balance/                    independent module: example.com/balance
  go.mod                    go 1.26
  balance.go                type Balance; NewBalance, Total, Add, Sub; sentinels
  cmd/
    demo/
      main.go               runnable demo: build, add, subtract, overdraw
  balance_test.go           table tests: reject negative, overdraw, purity, equality
```

- Files: `balance.go`, `cmd/demo/main.go`, `balance_test.go`.
- Implement: a `Balance` with unexported `amount`/`cents`, `NewBalance` rejecting negatives, `Total`, pure `Add`, and `Sub` with an overdraw guard.
- Test: `NewBalance(-1,0)` returns `ErrNonNegative`; `Sub` returns `ErrOverdraw` when the result would go negative; `Add` leaves inputs unchanged; equal fields compare `==`.
- Verify: `go test -count=1 -race ./...`

### Why unexported fields and a constructor

The invariant is "a balance is never negative". The only way to guarantee it for
every caller — handler, worker, test — is to make an invalid `Balance`
unbuildable. Two things enforce that together. The fields `amount` and `cents` are
unexported, so no code outside package `balance` can assign them; the zero value
`Balance{}` is a valid zero balance, and every other value must come from
`NewBalance`, which checks the sign. Holding a `Balance` is therefore proof it is
non-negative.

The operations are pure. `Add` returns a brand-new `Balance` and never touches its
receiver or its argument, so no caller can be surprised by a value mutating under
them, and a `Balance` is safe to share across goroutines with no lock. `Sub` is
the same but fallible: it computes the result in total minor units, and if that
would be negative it returns `ErrOverdraw` and the zero `Balance` rather than
producing an illegal value. The overdraw check *is* the invariant, enforced at the
boundary.

Because all fields are comparable `int64`, two `Balance` values with equal fields
satisfy `==`. That is the value-object equality contract, and it is what lets a
`Balance` be a map key or be compared cheaply. `TestBalanceEqualityIsByValue`
pins it.

Create `balance.go`:

```go
package balance

import (
	"errors"
	"fmt"
)

// Sentinel errors let callers branch on the failure mode with errors.Is.
var (
	ErrNonNegative = errors.New("balance: amount must be non-negative")
	ErrOverdraw    = errors.New("balance: cannot subtract more than the balance")
)

// Balance is an immutable value object: a non-negative amount of money held as a
// whole-unit part and a cents part. It has no identity; two Balances with equal
// fields are interchangeable and compare == because all fields are comparable.
type Balance struct {
	amount int64
	cents  int64
}

// NewBalance is the only door into a non-zero Balance. It rejects negative input
// so an invalid Balance cannot be constructed.
func NewBalance(amount, cents int64) (Balance, error) {
	if amount < 0 || cents < 0 {
		return Balance{}, fmt.Errorf("%w: got amount=%d cents=%d", ErrNonNegative, amount, cents)
	}
	return Balance{amount: amount, cents: cents}, nil
}

// Total reports the balance in minor units (cents).
func (b Balance) Total() int64 {
	return b.amount*100 + b.cents
}

// Add returns a new Balance and mutates neither operand.
func (b Balance) Add(other Balance) Balance {
	total := b.Total() + other.Total()
	return Balance{amount: total / 100, cents: total % 100}
}

// Sub returns a new Balance or ErrOverdraw if the result would be negative.
func (b Balance) Sub(other Balance) (Balance, error) {
	total := b.Total() - other.Total()
	if total < 0 {
		return Balance{}, ErrOverdraw
	}
	return Balance{amount: total / 100, cents: total % 100}, nil
}
```

### The runnable demo

`cmd/demo` is a separate `package main`, so it can only touch the exported API —
which is exactly why `Total` and the constructor are exported. It builds two
balances, adds them, subtracts one, and then triggers the overdraw guard.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/balance"
)

func main() {
	wallet, _ := balance.NewBalance(10, 50) // $10.50
	deposit, _ := balance.NewBalance(5, 25) // $5.25

	afterDeposit := wallet.Add(deposit)
	fmt.Printf("after deposit: %d cents\n", afterDeposit.Total())

	afterSpend, _ := afterDeposit.Sub(deposit)
	fmt.Printf("after spend:   %d cents\n", afterSpend.Total())

	big, _ := balance.NewBalance(100, 0)
	if _, err := wallet.Sub(big); errors.Is(err, balance.ErrOverdraw) {
		fmt.Println("overdraw rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after deposit: 1575 cents
after spend:   1050 cents
overdraw rejected
```

### Tests

The tests pin the invariants directly. Negative construction must fail with
`ErrNonNegative`; `Sub` past the balance must fail with `ErrOverdraw`; `Add` must
be pure (its inputs unchanged afterward); and two equal balances must compare
`==`, which is the value-object equality contract carried over from the original
lesson as `TestBalanceEqualityIsByValue`.

Create `balance_test.go`:

```go
package balance

import (
	"errors"
	"testing"
)

func TestNewBalance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		amount, cents  int64
		wantErr        error
		wantTotalCents int64
	}{
		{name: "valid", amount: 10, cents: 50, wantTotalCents: 1050},
		{name: "zero", amount: 0, cents: 0, wantTotalCents: 0},
		{name: "negative amount", amount: -1, cents: 0, wantErr: ErrNonNegative},
		{name: "negative cents", amount: 0, cents: -1, wantErr: ErrNonNegative},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := NewBalance(tt.amount, tt.cents)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("NewBalance(%d,%d) err = %v, want %v", tt.amount, tt.cents, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewBalance(%d,%d) unexpected err = %v", tt.amount, tt.cents, err)
			}
			if got := b.Total(); got != tt.wantTotalCents {
				t.Fatalf("Total = %d, want %d", got, tt.wantTotalCents)
			}
		})
	}
}

func TestAddIsPure(t *testing.T) {
	t.Parallel()
	a, _ := NewBalance(10, 0)
	b, _ := NewBalance(5, 50)
	sum := a.Add(b)
	if sum.Total() != 1550 {
		t.Fatalf("sum.Total = %d, want 1550", sum.Total())
	}
	if a.Total() != 1000 || b.Total() != 550 {
		t.Fatalf("Add mutated an operand: a=%d b=%d", a.Total(), b.Total())
	}
}

func TestSubRejectsOverdraw(t *testing.T) {
	t.Parallel()
	a, _ := NewBalance(1, 0)
	b, _ := NewBalance(10, 0)
	if _, err := a.Sub(b); !errors.Is(err, ErrOverdraw) {
		t.Fatalf("Sub err = %v, want ErrOverdraw", err)
	}
}

func TestSubOK(t *testing.T) {
	t.Parallel()
	a, _ := NewBalance(10, 0)
	b, _ := NewBalance(5, 50)
	diff, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Total() != 450 {
		t.Fatalf("diff.Total = %d, want 450", diff.Total())
	}
}

// TestBalanceEqualityIsByValue pins the value-object contract: equal fields
// compare ==, so a Balance is safe as a map key.
func TestBalanceEqualityIsByValue(t *testing.T) {
	t.Parallel()
	a, _ := NewBalance(3, 33)
	b, _ := NewBalance(3, 33)
	if a != b {
		t.Fatalf("equal-field balances compare unequal: %+v vs %+v", a, b)
	}
	seen := map[Balance]int{a: 1}
	if seen[b] != 1 {
		t.Fatal("equal Balance did not resolve to the same map key")
	}
}
```

## Review

The `Balance` is correct when validity is an unconditional property of the type:
`NewBalance` is the only door and it rejects negatives, `Sub` refuses to produce a
negative result, and `Add`/`Sub` never mutate their operands. The equality test is
not decoration — it proves the fields stayed comparable, which is what makes a
`Balance` usable as a map key downstream. The mistakes to avoid are exporting the
fields (which would let a caller assign a negative amount and bypass every check)
and turning `Add` into a free function (the type owns its data, so it owns the
operation). Run `go test -race` to confirm the pure value semantics hold with no
shared mutable state.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — field visibility and struct comparability.
- [Effective Go: Methods](https://go.dev/doc/effective_go#methods) — value vs pointer receivers.
- [`errors` package](https://pkg.go.dev/errors) — `errors.New` and `errors.Is` for sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-user-entity-invariants.md](02-user-entity-invariants.md)
