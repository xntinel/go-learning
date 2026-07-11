# Exercise 8: Demonstrate why 100% coverage can still be wrong

Coverage records execution, not correctness. This module proves it head-on: a fee
calculation with a tie-breaking rounding rule, one test that reaches 100% coverage
while asserting nothing (green even against a wrong implementation), and one table
test that reaches the *same* 100% while asserting the exact cents — the difference
between a metric and a guarantee.

This module is fully self-contained: its own `go mod init`, a demo, and both test
styles.

## What you'll build

```text
fees/                      independent module: example.com/fees
  go.mod
  fees.go                  FeeCents: rate applied with round-half-to-even
  cmd/
    demo/
      main.go              runnable demo showing a tie case
  fees_test.go             assertion-free test + real table test (same coverage)
```

- Files: `fees.go`, `cmd/demo/main.go`, `fees_test.go`.
- Implement: `FeeCents(amountCents, ratePerMille int64) int64` computing `amount*rate/1000` with banker's rounding (round half to even) on the remainder.
- Test: an assertion-free `TestExecutesEverything` that reaches 100% and proves nothing, and a `TestFeeCents` table that reaches the same 100% and asserts exact values including a tie.
- Verify: `go test -count=1 -race -cover ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fees/cmd/demo
cd ~/go-exercises/fees
go mod init example.com/fees
```

### Where the trap lives: the rounding tie

`FeeCents` applies a per-mille rate to an amount in cents: `amount * rate / 1000`.
That division has a remainder, and how you round it is a real money-handling
decision. Financial code commonly uses *banker's rounding* — round half to even —
because always rounding halves up introduces a systematic upward bias across many
transactions. The rule: if twice the remainder is less than 1000, round down; if
greater, round up; if exactly 1000 (a perfect half), round to the *even* quotient.

The tie case is where a bug hides in plain sight. Consider `amount=250,
rate=10`: the product is 2500, so the exact fee is 2.5 cents. Banker's rounding
gives 2 (2 is even). The naive "round half up" gives 3. A single input
distinguishes the correct implementation from the common wrong one — and *both*
implementations execute exactly the same lines, so both reach 100% statement
coverage. Coverage cannot tell them apart. Only an assertion on the output at that
input can.

Here is the wrong version many engineers write — round half up on every tie. It
compiles, and a test that merely calls it hits every line:

```go
// WRONG: rounds every half up, introducing upward bias on ties.
func feeHalfUp(amountCents, ratePerMille int64) int64 {
	product := amountCents * ratePerMille
	q := product / 1000
	r := product % 1000
	if 2*r >= 1000 {
		return q + 1 // a tie rounds UP here — biased
	}
	return q
}
```

The correct version rounds a tie to even:

Create `fees.go`:

```go
package fees

// FeeCents returns the fee in cents for applying ratePerMille (parts per
// thousand) to amountCents, rounding the fractional cent half to even (banker's
// rounding). Inputs are assumed non-negative.
func FeeCents(amountCents, ratePerMille int64) int64 {
	product := amountCents * ratePerMille
	q := product / 1000
	r := product % 1000
	switch {
	case 2*r < 1000:
		return q
	case 2*r > 1000:
		return q + 1
	default: // exact half: round to even
		if q%2 == 0 {
			return q
		}
		return q + 1
	}
}
```

The only difference from `feeHalfUp` is the tie: `feeHalfUp` always returns `q+1`
when `2*r >= 1000`, while `FeeCents` returns the even neighbor. On every non-tie
input the two agree; on `amount=250, rate=10` they diverge (2 versus 3).

### The runnable demo

The demo prints a non-tie fee and the tie case, so `go run ./cmd/demo` shows the
banker's-rounding result deterministically.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fees"
)

func main() {
	fmt.Println("1% of $10.00:", fees.FeeCents(1000, 10))
	fmt.Println("tie 250 x 10:", fees.FeeCents(250, 10))
	fmt.Println("tie 750 x 10:", fees.FeeCents(750, 10))
	fmt.Println("round up 260 x 10:", fees.FeeCents(260, 10))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1% of $10.00: 10
tie 250 x 10: 2
tie 750 x 10: 8
round up 260 x 10: 3
```

(`250x10`: quotient 2, remainder 500, tie, 2 is even -> 2. `750x10`: quotient 7,
remainder 500, tie, 7 is odd -> round to 8. `260x10`: remainder 600 > 500 -> 3.)

### Two tests, same coverage, opposite value

`TestExecutesEverything` is the trap made explicit. It calls `FeeCents` across a
spread of inputs that drives every branch — the round-down case, the round-up
case, and both tie parities — and asserts *nothing*. It passes. It would also pass
if `FeeCents` were the buggy `feeHalfUp`, because coverage does not look at return
values. Run it under `-cover` and you get 100.0% of statements, green, meaningless.

`TestFeeCents` reaches the identical 100% but asserts exact expected cents,
including the `250x10` tie that separates banker's rounding from half-up. It passes
on the correct code and would *fail* on `feeHalfUp` — the same coverage, a real
guarantee.

Create `fees_test.go`:

```go
package fees

import "testing"

// TestExecutesEverything reaches 100% statement coverage and proves nothing: it
// executes every branch of FeeCents but asserts no output. It would stay green
// even if FeeCents were wrong. This is the assertion-free coverage trap.
func TestExecutesEverything(t *testing.T) {
	t.Parallel()
	inputs := []struct{ amount, rate int64 }{
		{1000, 10}, // exact
		{260, 10},  // round up (remainder 600)
		{240, 10},  // round down (remainder 400)
		{250, 10},  // tie, even quotient
		{750, 10},  // tie, odd quotient
	}
	for _, in := range inputs {
		_ = FeeCents(in.amount, in.rate) // executed, never asserted
	}
}

// TestFeeCents reaches the SAME 100% coverage but asserts exact cents, including
// the tie case that distinguishes banker's rounding from round-half-up.
func TestFeeCents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		amount, rate int64
		want         int64
	}{
		{"exact", 1000, 10, 10},
		{"round-down", 240, 10, 2}, // 2400/1000 = 2.4 -> 2
		{"round-up", 260, 10, 3},   // 2600/1000 = 2.6 -> 3
		{"tie-even", 250, 10, 2},   // 2500/1000 = 2.5 -> even -> 2
		{"tie-odd", 750, 10, 8},    // 7500/1000 = 7.5 -> even -> 8
		{"zero-amount", 0, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FeeCents(tt.amount, tt.rate); got != tt.want {
				t.Errorf("FeeCents(%d,%d) = %d, want %d", tt.amount, tt.rate, got, tt.want)
			}
		})
	}
}

// TestBankersDiffersFromHalfUp pins the exact input where the correct rule and
// the common buggy rule diverge, so a regression to half-up is caught here.
func TestBankersDiffersFromHalfUp(t *testing.T) {
	t.Parallel()
	// On a tie with an even quotient, banker's rounding keeps the quotient (2),
	// whereas round-half-up would return 3.
	if got := FeeCents(250, 10); got != 2 {
		t.Fatalf("FeeCents(250,10) = %d, want 2 (banker's rounding); got the half-up answer?", got)
	}
}
```

### The two tests reach the same number

Run each test in isolation under coverage and compare. Both report 100% of the
`fees` package statements:

```bash
go test -run TestExecutesEverything -cover ./...
go test -run TestFeeCents -cover ./...
```

Expected output (both):

```
ok      example.com/fees        0.00s  coverage: 100.0% of statements
```

The coverage percentage is identical; the value is not. `TestExecutesEverything`
would stay green if you replaced `FeeCents`'s tie branch with the biased
`feeHalfUp` logic; `TestFeeCents` and `TestBankersDiffersFromHalfUp` would turn
red. That gap — same coverage, opposite ability to catch the bug — is the whole
point: coverage measures the absence of untested lines, never the presence of
correct assertions.

## Review

`FeeCents` is correct when it applies the rate, rounds non-ties by magnitude, and
rounds ties to the even quotient — pinned by the table including both tie parities
and by the divergence test at `250x10`. The lesson is correct when
`TestExecutesEverything` and `TestFeeCents` both report 100% coverage while only
the latter can catch a regression to half-up rounding.

The mistake to avoid is the one the assertion-free test embodies: writing tests
that call functions to move the coverage number without asserting on outputs. Under
a mandated coverage target that is the path of least resistance, and it produces
green builds over wrong code. Judge a test by its assertions, not by the lines it
touches; use coverage to find untested branches, then assert on what they return.
Run `go test -race -cover` to see both tests pass and report the same 100%.

## Resources

- [Testing flags (`-cover`)](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — what the coverage percentage does and does not measure.
- [The cover story](https://go.dev/blog/cover) — coverage as a tool to find untested code, explicitly not a correctness measure.
- [IEEE 754 round half to even](https://en.wikipedia.org/wiki/Rounding#Rounding_half_to_even) — why financial code prefers banker's rounding.

---

Back to [07-error-branch-coverage-repository.md](07-error-branch-coverage-repository.md) | Next: [09-scope-coverage-exclude-generated.md](09-scope-coverage-exclude-generated.md)
