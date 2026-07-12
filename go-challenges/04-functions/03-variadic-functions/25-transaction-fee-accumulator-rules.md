# Exercise 25: Transaction Fee Accumulator with Rule Functions

**Nivel: Intermedio** — validacion rapida (un test corto).

A payment's total fee is rarely one number: a flat lookup fee, a percentage
tax, an interchange fee, maybe a currency-conversion surcharge — each is an
independent rule applied to the same transaction amount, and the total is
just their sum. Modeling each rule as a `FeeRule` function and summing over
a variadic list of them means adding a new fee type later is "write one more
function," never a change to the summation logic itself.

## What you'll build

```text
feeacc/                     independent module: example.com/feeacc
  go.mod                    go 1.24
  feeacc.go                 package feeacc; type FeeRule func(int64) (int64, error); Flat(feeCents int64), Percent(bps int64); TotalFee(amountCents int64, rules ...FeeRule) (int64, error)
  cmd/
    demo/
      main.go               runnable demo: a lookup fee, tax, and interchange summed, then a rule error
  feeacc_test.go            table tests: full sum, zero rules, stop-on-error with no partial sum
```

- Files: `feeacc.go`, `cmd/demo/main.go`, `feeacc_test.go`.
- Implement: `type FeeRule func(amountCents int64) (feeCents int64, err error)`, constructors `Flat(feeCents int64) FeeRule` and `Percent(bps int64) FeeRule`, and `TotalFee(amountCents int64, rules ...FeeRule) (int64, error)`.
- Test: a flat fee plus two percentage fees sum to the exact expected cents total; zero rules sums to zero; a rule that errors (a negative amount) makes `TotalFee` return `0` and the error, never a partial sum.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Rules as values, and why a failed rule voids the whole total

`FeeRule` is `func(amountCents int64) (feeCents int64, err error)` — a value
that, given the transaction amount, computes its own fee independently of
every other rule. `Flat` ignores the amount entirely and always returns the
same fee; `Percent` computes basis points of the amount, truncated down to
the nearest cent (`amountCents * bps / 10000`, using integer division, which
is why every rule works in cents rather than floating-point currency — money
math should never be a `float64`). Because each rule is just a function, the
list of rules a given transaction type applies is itself data: a "domestic
card" transaction might apply `Flat` and one `Percent`, while a
"cross-border" one adds a third `Percent` for currency conversion, and
`TotalFee` doesn't need to change either way.

`TotalFee` returns `0, err` — not `total, err` with `total` holding whatever
had accumulated before the failing rule — the moment any rule errors. A fee
total is only meaningful as a complete, correctly computed number; a partial
sum that silently omits the rules after the one that failed would look like
a valid (if too-low) fee to a caller that only checks `err != nil` loosely,
which for a billing system is the kind of bug that undercharges silently
instead of failing loudly.

Create `feeacc.go`:

```go
// feeacc.go
package feeacc

import "fmt"

// FeeRule computes a fee, in cents, for a transaction of amountCents, or
// returns an error if the rule cannot be applied to that amount.
type FeeRule func(amountCents int64) (feeCents int64, err error)

// Flat returns a FeeRule that always charges a fixed fee regardless of the
// transaction amount (a typical per-lookup fee).
func Flat(feeCents int64) FeeRule {
	return func(amountCents int64) (int64, error) {
		return feeCents, nil
	}
}

// Percent returns a FeeRule that charges bps basis points (1/100 of a
// percent) of the transaction amount, rounded down to the nearest cent
// (a typical tax or interchange fee).
func Percent(bps int64) FeeRule {
	return func(amountCents int64) (int64, error) {
		if amountCents < 0 {
			return 0, fmt.Errorf("feeacc: negative amount %d", amountCents)
		}
		return amountCents * bps / 10000, nil
	}
}

// TotalFee sums the fee produced by every rule for a transaction of
// amountCents, in the order the rules are given. It stops at, and returns,
// the first rule's error, if any — a fee rule that cannot be evaluated means
// the total fee itself cannot be trusted, so TotalFee never returns a
// partial sum alongside an error.
func TotalFee(amountCents int64, rules ...FeeRule) (int64, error) {
	var total int64
	for i, rule := range rules {
		fee, err := rule(amountCents)
		if err != nil {
			return 0, fmt.Errorf("feeacc: rule %d: %w", i, err)
		}
		total += fee
	}
	return total, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/feeacc"
)

func main() {
	const amountCents = 10_000 // $100.00

	total, err := feeacc.TotalFee(amountCents,
		feeacc.Flat(150),    // $1.50 lookup fee
		feeacc.Percent(250), // 2.5% tax
		feeacc.Percent(180), // 1.8% interchange
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("total fee: %d cents\n", total)

	_, err = feeacc.TotalFee(-500, feeacc.Percent(250))
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total fee: 580 cents
error: feeacc: rule 0: feeacc: negative amount -500
```

### Tests

`TestTotalFeeStopsAtFirstErrorWithoutAPartialSum` is the one that pins the
"never a partial sum" guarantee: the first rule (`Flat(150)`) would
contribute `150` before the second rule errors on a negative amount, and the
test asserts the returned total is `0`, not `150`.

Create `feeacc_test.go`:

```go
// feeacc_test.go
package feeacc

import (
	"testing"
)

func TestTotalFeeSumsAllRules(t *testing.T) {
	t.Parallel()

	total, err := TotalFee(10_000, Flat(150), Percent(250), Percent(180))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(580); total != want {
		t.Fatalf("TotalFee = %d, want %d", total, want)
	}
}

func TestTotalFeeWithZeroRulesIsZero(t *testing.T) {
	t.Parallel()

	total, err := TotalFee(10_000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 {
		t.Fatalf("TotalFee = %d, want 0", total)
	}
}

func TestTotalFeeStopsAtFirstErrorWithoutAPartialSum(t *testing.T) {
	t.Parallel()

	total, err := TotalFee(-500, Flat(150), Percent(250))
	if err == nil {
		t.Fatalf("expected an error for a negative amount")
	}
	if total != 0 {
		t.Fatalf("TotalFee on error = %d, want 0 (no partial sum)", total)
	}
}
```

## Review

`TotalFee` is correct when it sums exactly the fee each rule reports for the
given amount, zero rules sum to zero, and any rule error voids the entire
result — the function returns `0` alongside the error, never a partial sum
computed from the rules that happened to run before the failing one. The
senior point is treating money as integer cents throughout (`int64`, never
`float64`) and truncating percentage math with integer division rather than
rounding — `Percent` computing `amountCents * bps / 10000` is deterministic
and reproducible in a way that floating-point cents arithmetic is not, which
matters enormously the moment two independently computed totals need to
match exactly, as they must in reconciliation. The mistake to avoid is
returning `total, err` on a rule failure instead of `0, err` — a caller that
checks the error but still logs or displays the numeric total "just in case"
would show a plausible-looking but wrong number.

## Resources

- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [ISO 4217](https://www.iso.org/iso-4217-currency-codes.html) — the minor-unit (cents) convention this exercise's amounts follow.
- [Effective Go: Variadic functions](https://go.dev/doc/effective_go#variadic-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-oauth-scope-claim-verifier.md](24-oauth-scope-claim-verifier.md) | Next: [26-worker-pool-task-enqueuer.md](26-worker-pool-task-enqueuer.md)
