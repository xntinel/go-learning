# Exercise 2: Strategy via Function Types

When a strategy has a single method and no state worth naming, the struct-plus-interface ceremony is overhead. Go lets the algorithm be a plain function value: declare a named function type for the contract, and any function or closure that matches becomes a strategy directly. This exercise rebuilds the same pricing domain with no interface and no strategy struct, then uses the function shape to do something the struct form cannot do as cleanly — compose strategies out of other strategies.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
strategy.go          PricingFunc type; Flat, Percentage, None constructors;
                     CapDiscount and Best combinators; Checkout context
cmd/
  demo/
    main.go          run named, capped, composed, and inline strategies
strategy_test.go     each strategy, both combinators, the nil-strategy default
```

- Files: `strategy.go`, `cmd/demo/main.go`, `strategy_test.go`.
- Implement: the `PricingFunc` type, the `Flat`/`Percentage`/`None` constructors, the `CapDiscount` and `Best` combinators, and a `Checkout` whose strategy is a function value.
- Test: each strategy's math, `CapDiscount` clamping, `Best` selection, and the nil-strategy fallback.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p strategy-via-function-types/cmd/demo && cd strategy-via-function-types
go mod init example.com/pricing-func
```

### The function is the strategy

The contract is a function type instead of an interface:

```go
type PricingFunc func(subtotal float64) (discount float64, total float64)
```

`PricingFunc` is the same `(discount, total)` contract as the interface version, but now any value of this type — a top-level function, a closure, an inline `func` literal — is a complete strategy. There is no struct to define and no method to attach. This is the exact shape the standard library uses for `http.HandlerFunc`: a function type that lets an ordinary function stand in wherever the contract is expected.

Configuration moves from struct fields into closures. `Flat(amount)` does not return a `FlatDiscount{Amount: amount}`; it returns a function that has captured `amount` in its closure environment. The captured variable plays the role the struct field played, and the returned function is the strategy. `None` is even simpler — it is a plain top-level function with no configuration to capture, usable directly anywhere a `PricingFunc` is wanted, which is what makes a named function type more flexible than a one-method interface: the identity strategy needs no wrapper at all.

The encoding earns its place when you compose. Because a strategy is just a function, a decorator is just a function that calls another function and adjusts its result. `CapDiscount(max, inner)` returns a strategy that runs `inner`, then clamps the discount — no `Decorator` struct, no embedded interface field, no boilerplate forwarding method. `Best(strategies...)` takes a variadic of the same type, runs them all, and keeps the largest discount; the variadic works precisely because every candidate is one concrete type and they share a slice with no boxing. Composing interface-based strategies is possible but heavier: each combinator would be a struct implementing the interface and storing the wrapped strategies. With function values the combinator is a closure.

The `Checkout` context holds a `PricingFunc` field and calls it directly. Swapping the strategy is an assignment to the field — the same one-line swap as the interface version, but with a function value on the right-hand side. The one wrinkle is the zero value: an interface's zero value is `nil` and calling through it panics, and a function's zero value is also `nil` and calling it panics, so `Process` guards a nil strategy by falling back to `None`. A context that holds a function strategy should always decide what an unset strategy means rather than letting it panic.

Create `strategy.go`:

```go
package pricing

// PricingFunc is a strategy expressed as a plain function value: it maps a
// subtotal to a (discount, total) pair. No interface and no struct is needed;
// the function itself is the strategy.
type PricingFunc func(subtotal float64) (discount float64, total float64)

// Flat returns a strategy that subtracts a fixed amount, never more than the
// subtotal. The amount is captured in the returned closure.
func Flat(amount float64) PricingFunc {
	return func(subtotal float64) (float64, float64) {
		if subtotal <= 0 {
			return 0, subtotal
		}
		d := amount
		if d > subtotal {
			d = subtotal
		}
		return d, subtotal - d
	}
}

// Percentage returns a strategy that removes a percentage of the subtotal.
func Percentage(percent float64) PricingFunc {
	return func(subtotal float64) (float64, float64) {
		if subtotal <= 0 {
			return 0, subtotal
		}
		d := subtotal * (percent / 100)
		return d, subtotal - d
	}
}

// None is the identity strategy: it grants no discount. It is an ordinary
// function value, usable anywhere a PricingFunc is expected.
func None(subtotal float64) (float64, float64) { return 0, subtotal }

// CapDiscount decorates a strategy so the discount it returns never exceeds
// max. Because a strategy is just a function, a decorator is just a function
// that calls another and adjusts the result; no wrapper type is required.
func CapDiscount(max float64, inner PricingFunc) PricingFunc {
	return func(subtotal float64) (float64, float64) {
		d, _ := inner(subtotal)
		if d > max {
			d = max
		}
		return d, subtotal - d
	}
}

// Best composes strategies by running them all and keeping the largest
// discount. The variadic shape works because every candidate is the same
// function type, so they live in one slice with no boxing.
func Best(strategies ...PricingFunc) PricingFunc {
	return func(subtotal float64) (float64, float64) {
		bestD, bestT := 0.0, subtotal
		for _, s := range strategies {
			d, t := s(subtotal)
			if d > bestD {
				bestD, bestT = d, t
			}
		}
		return bestD, bestT
	}
}

// Checkout holds a strategy as a function value and calls it directly. The
// context never branches on a concrete type; swapping the strategy is one
// assignment to the field.
type Checkout struct {
	Price PricingFunc
}

func (c *Checkout) Process(items []float64) (discount, total float64) {
	subtotal := 0.0
	for _, item := range items {
		subtotal += item
	}
	if c.Price == nil {
		return None(subtotal)
	}
	return c.Price(subtotal)
}
```

### The runnable demo

The demo runs a named, a capped, and a composed strategy over one subtotal, then drives a `Checkout` through a runtime swap and finally assigns an inline closure as a strategy — the move that has no equivalent in the interface form without declaring a new type. On a `$250` subtotal, `Percentage(15)` gives `$37.50`, which `CapDiscount(30, ...)` clamps to `$30`, and `Best` of flat-$20 and 15% picks the 15% as the larger discount.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pricing-func"
)

func main() {
	subtotal := 250.0
	fmt.Printf("subtotal: $%.2f\n\n", subtotal)

	named := []struct {
		label string
		fn    pricing.PricingFunc
	}{
		{"none", pricing.None},
		{"flat $20", pricing.Flat(20)},
		{"15%", pricing.Percentage(15)},
		{"15% capped at $30", pricing.CapDiscount(30, pricing.Percentage(15))},
		{"best of flat/pct", pricing.Best(pricing.Flat(20), pricing.Percentage(15))},
	}
	for _, n := range named {
		d, total := n.fn(subtotal)
		fmt.Printf("%-20s discount=$%6.2f  total=$%7.2f\n", n.label, d, total)
	}

	fmt.Println()
	co := &pricing.Checkout{Price: pricing.Flat(20)}
	d, total := co.Process([]float64{120, 130})
	fmt.Printf("checkout[flat]: discount=$%.2f total=$%.2f\n", d, total)

	// Swap the strategy at runtime: assign a different function value.
	co.Price = pricing.Best(pricing.Flat(20), pricing.Percentage(15))
	d, total = co.Process([]float64{120, 130})
	fmt.Printf("checkout[best]: discount=$%.2f total=$%.2f\n", d, total)

	// An inline closure is a strategy too, with no named type to declare.
	co.Price = func(s float64) (float64, float64) {
		d := s - 199.0
		if d < 0 {
			d = 0
		}
		return d, s - d
	}
	d, total = co.Process([]float64{120, 130})
	fmt.Printf("checkout[inline]: discount=$%.2f total=$%.2f\n", d, total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subtotal: $250.00

none                 discount=$  0.00  total=$ 250.00
flat $20             discount=$ 20.00  total=$ 230.00
15%                  discount=$ 37.50  total=$ 212.50
15% capped at $30    discount=$ 30.00  total=$ 220.00
best of flat/pct     discount=$ 37.50  total=$ 212.50

checkout[flat]: discount=$20.00 total=$230.00
checkout[best]: discount=$37.50 total=$212.50
checkout[inline]: discount=$51.00 total=$199.00
```

### Tests

The tests cover each constructor's math, both combinators (including the boundary where `Best` is given no strategies and the cap is not reached), the nil-strategy fallback, and the proof that a bare closure assigned to a `PricingFunc` variable is a strategy. `Best` is checked both ways — a subtotal where the percentage wins and one where the flat wins — so the selection logic is pinned, not just the happy path.

Create `strategy_test.go`:

```go
package pricing

import "testing"

func TestFlat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		amount, sub, wantD, wantT float64
	}{
		{20, 100, 20, 80},
		{50, 50, 50, 0},
		{200, 50, 50, 0},
		{20, 0, 0, 0},
	}
	for _, tc := range cases {
		d, total := Flat(tc.amount)(tc.sub)
		if d != tc.wantD || total != tc.wantT {
			t.Errorf("Flat(%v)(%v) = (%v, %v), want (%v, %v)", tc.amount, tc.sub, d, total, tc.wantD, tc.wantT)
		}
	}
}

func TestPercentage(t *testing.T) {
	t.Parallel()

	d, total := Percentage(15)(250)
	if d != 37.5 || total != 212.5 {
		t.Errorf("Percentage(15)(250) = (%v, %v), want (37.5, 212.5)", d, total)
	}
}

func TestNone(t *testing.T) {
	t.Parallel()

	if d, total := None(99); d != 0 || total != 99 {
		t.Errorf("None(99) = (%v, %v), want (0, 99)", d, total)
	}
}

func TestCapDiscount(t *testing.T) {
	t.Parallel()

	// 15% of 250 = 37.50, capped at 30.
	d, total := CapDiscount(30, Percentage(15))(250)
	if d != 30 || total != 220 {
		t.Errorf("capped = (%v, %v), want (30, 220)", d, total)
	}
	// Below the cap, the inner discount passes through untouched.
	d, total = CapDiscount(30, Percentage(15))(100)
	if d != 15 || total != 85 {
		t.Errorf("under cap = (%v, %v), want (15, 85)", d, total)
	}
}

func TestBest_PicksLargestDiscount(t *testing.T) {
	t.Parallel()

	best := Best(Flat(20), Percentage(15))
	// On 250: flat=20, pct=37.50 -> pct wins.
	if d, total := best(250); d != 37.5 || total != 212.5 {
		t.Errorf("best(250) = (%v, %v), want (37.5, 212.5)", d, total)
	}
	// On 100: flat=20, pct=15 -> flat wins.
	if d, total := best(100); d != 20 || total != 80 {
		t.Errorf("best(100) = (%v, %v), want (20, 80)", d, total)
	}
	// No strategies: no discount.
	if d, total := Best()(100); d != 0 || total != 100 {
		t.Errorf("Best()(100) = (%v, %v), want (0, 100)", d, total)
	}
}

func TestCheckout_SwapsFunctionValue(t *testing.T) {
	t.Parallel()

	co := &Checkout{Price: Flat(20)}
	if d, total := co.Process([]float64{60, 40}); d != 20 || total != 80 {
		t.Errorf("flat checkout = (%v, %v), want (20, 80)", d, total)
	}
	co.Price = Percentage(10)
	if d, total := co.Process([]float64{60, 40}); d != 10 || total != 90 {
		t.Errorf("pct checkout = (%v, %v), want (10, 90)", d, total)
	}
}

func TestCheckout_NilStrategyIsIdentity(t *testing.T) {
	t.Parallel()

	co := &Checkout{}
	if d, total := co.Process([]float64{60, 40}); d != 0 || total != 100 {
		t.Errorf("nil checkout = (%v, %v), want (0, 100)", d, total)
	}
}

func TestInlineClosureIsAStrategy(t *testing.T) {
	t.Parallel()

	var s PricingFunc = func(sub float64) (float64, float64) {
		return sub * 0.2, sub * 0.8
	}
	if d, total := s(100); d != 20 || total != 80 {
		t.Errorf("inline = (%v, %v), want (20, 80)", d, total)
	}
}
```

## Review

The function encoding is correct when the same `(discount, total)` contract holds and the combinators are honest about what they wrap. Confirm `CapDiscount` clamps only when the inner discount exceeds the cap and passes a smaller discount through unchanged, and that it recomputes the total from the clamped discount rather than trusting the inner total. Confirm `Best` returns `(0, subtotal)` when handed no strategies, which is the safe identity rather than a panic on an empty slice. The decisive check is the nil guard in `Process`: a `Checkout{}` with no strategy set must behave as identity, because a `PricingFunc`'s zero value is `nil` and calling it would panic exactly like calling a nil interface.

Common mistakes for this feature. The first is forgetting that a function type's zero value is `nil` and callable nowhere — a context holding a function strategy must guard the unset case or document that the field is required. The second is reaching for a struct-and-interface decorator when a closure would do; with function-value strategies, `CapDiscount` and `Best` are a few lines each and need no new type. The third is letting a combinator trust a wrapped strategy's total instead of recomputing from the discount it actually returns, which silently desyncs the two numbers once a cap or floor changes the discount.

## Resources

- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the language rule that makes a named function type a first-class value you can store, pass, and call.
- [`net/http.HandlerFunc`](https://pkg.go.dev/net/http#HandlerFunc) — the standard library's canonical "function type satisfies a contract" adapter, the same move `PricingFunc` makes.
- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces) — how function types and interfaces interchange, including the `HandlerFunc` discussion.

---

Back to [01-strategy-via-interfaces.md](01-strategy-via-interfaces.md) | Next: [03-strategy-registry.md](03-strategy-registry.md)
