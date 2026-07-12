# Exercise 1: Strategy via Interfaces

The canonical form of the strategy pattern in Go is a small interface implemented by several stateless value types, with a context that holds the interface and delegates to it. This exercise builds a checkout that prices a cart through any of four interchangeable discount strategies and swaps between them at runtime without ever naming a concrete one in the pricing path.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
strategy.go          PricingStrategy interface; FlatDiscount, PercentageDiscount,
                     TieredPricing, PromotionalPricing; Checkout context
cmd/
  demo/
    main.go          price one cart through every strategy, then swap at runtime
strategy_test.go     per-strategy math, tier ordering, runtime swap, accessor
```

- Files: `strategy.go`, `cmd/demo/main.go`, `strategy_test.go`.
- Implement: the `PricingStrategy` interface (`Calculate` + `Name`), the four strategy types, and a `Checkout` context with `NewCheckout`, `SetStrategy`, `Strategy`, and `Process`.
- Test: each strategy's math, the tier selection order, the runtime swap invariant, and the accessor.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/01-strategy-via-interfaces/cmd/demo && cd go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/01-strategy-via-interfaces
```

### The interface is the family contract

`PricingStrategy` has one real job — turn a subtotal into a `(discount, total)` pair — plus a `Name` for labeling. It returns both numbers, not just the discount, so no caller ever recomputes `subtotal - discount`; the strategy owns the whole result and callers consume it. Keeping the method count low matters: every method here is a method all four strategies must implement, and a two-method interface is still narrow enough that a value type satisfies it for free.

```go
type PricingStrategy interface {
	Calculate(subtotal float64) (discount float64, total float64)
	Name() string
}
```

Each strategy is a small struct whose fields are its configuration, with value-receiver methods. `FlatDiscount{Amount: 10}` is a value you can copy, store in a slice, or compare; there is no object to construct and no lifecycle to manage. `FlatDiscount` caps the discount at the subtotal so a $200-off coupon on a $50 cart yields $50 off and not a negative total, and treats an empty cart as a no-op. `PercentageDiscount` scales the subtotal. `TieredPricing` walks its tiers from the highest threshold down and applies the first one the subtotal clears, which is why a $150 cart on a {0, 50, 100, 200} ladder gets the 100-tier rate; a subtotal below every threshold (including a negative one, below the `MinAmount: 0` floor) falls through to no discount. `PromotionalPricing` is the conditional case: it applies a flat amount only once the subtotal reaches a minimum, the shape every "spend $100, save $10" promo takes.

The context, `Checkout`, holds a `PricingStrategy` and nothing more. `Process` sums the items and delegates; it never branches on which strategy it has. `SetStrategy` is the runtime swap — a single assignment — and `Strategy` is the accessor the demo and tests read to confirm which strategy is active. Switching the entire pricing behavior is one method call, and `Process` is untouched by it.

Create `strategy.go`:

```go
package pricing

import "fmt"

type PricingStrategy interface {
	Calculate(subtotal float64) (discount float64, total float64)
	Name() string
}

type FlatDiscount struct {
	Amount float64
}

func (f FlatDiscount) Calculate(subtotal float64) (float64, float64) {
	if subtotal <= 0 {
		return 0, subtotal
	}
	discount := f.Amount
	if discount > subtotal {
		discount = subtotal
	}
	return discount, subtotal - discount
}

func (f FlatDiscount) Name() string { return fmt.Sprintf("flat $%.2f off", f.Amount) }

type PercentageDiscount struct {
	Percent float64
}

func (p PercentageDiscount) Calculate(subtotal float64) (float64, float64) {
	if subtotal <= 0 {
		return 0, subtotal
	}
	discount := subtotal * (p.Percent / 100)
	return discount, subtotal - discount
}

func (p PercentageDiscount) Name() string { return fmt.Sprintf("%.0f%% off", p.Percent) }

type Tier struct {
	MinAmount float64
	Percent   float64
}

type TieredPricing struct {
	Tiers []Tier
}

func (t TieredPricing) Calculate(subtotal float64) (float64, float64) {
	for i := len(t.Tiers) - 1; i >= 0; i-- {
		if subtotal >= t.Tiers[i].MinAmount {
			discount := subtotal * (t.Tiers[i].Percent / 100)
			return discount, subtotal - discount
		}
	}
	return 0, subtotal
}

func (t TieredPricing) Name() string { return "tiered pricing" }

type PromotionalPricing struct {
	Code        string
	Flat        float64
	MinSubtotal float64
}

func (p PromotionalPricing) Calculate(subtotal float64) (float64, float64) {
	if subtotal < p.MinSubtotal {
		return 0, subtotal
	}
	discount := p.Flat
	if discount > subtotal {
		discount = subtotal
	}
	return discount, subtotal - discount
}

func (p PromotionalPricing) Name() string { return fmt.Sprintf("promo %s", p.Code) }

type Checkout struct {
	pricing PricingStrategy
}

func NewCheckout(pricing PricingStrategy) *Checkout { return &Checkout{pricing: pricing} }

func (c *Checkout) SetStrategy(p PricingStrategy) { c.pricing = p }

func (c *Checkout) Strategy() PricingStrategy { return c.pricing }

func (c *Checkout) Process(items []float64) (discount, total float64) {
	subtotal := 0.0
	for _, item := range items {
		subtotal += item
	}
	return c.pricing.Calculate(subtotal)
}
```

### The runnable demo

The demo prices one cart through all four strategies behind the single `PricingStrategy` interface, then builds a `Checkout` and swaps its strategy at runtime to show the context is blind to the concrete type. The `$99.97` subtotal is deliberately below the promo's `$100` minimum, so the promo row shows the conditional doing nothing while the others discount.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pricing-strategy"
)

func main() {
	cart := []float64{29.99, 49.99, 19.99}
	subtotal := 0.0
	for _, p := range cart {
		subtotal += p
	}
	fmt.Printf("subtotal: $%.2f\n\n", subtotal)

	strategies := []pricing.PricingStrategy{
		pricing.FlatDiscount{Amount: 10},
		pricing.PercentageDiscount{Percent: 15},
		pricing.TieredPricing{Tiers: []pricing.Tier{
			{MinAmount: 0, Percent: 0},
			{MinAmount: 50, Percent: 5},
			{MinAmount: 100, Percent: 10},
			{MinAmount: 200, Percent: 15},
		}},
		pricing.PromotionalPricing{Code: "WELCOME10", Flat: 10, MinSubtotal: 100},
	}
	for _, s := range strategies {
		d, total := s.Calculate(subtotal)
		fmt.Printf("%-16s discount=$%5.2f  total=$%6.2f\n", s.Name(), d, total)
	}

	fmt.Println()
	co := pricing.NewCheckout(pricing.FlatDiscount{Amount: 10})
	d, total := co.Process(cart)
	fmt.Printf("checkout[%s]: discount=$%.2f total=$%.2f\n", co.Strategy().Name(), d, total)

	co.SetStrategy(pricing.PercentageDiscount{Percent: 15})
	d, total = co.Process(cart)
	fmt.Printf("checkout[%s]: discount=$%.2f total=$%.2f\n", co.Strategy().Name(), d, total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subtotal: $99.97

flat $10.00 off  discount=$10.00  total=$ 89.97
15% off          discount=$15.00  total=$ 84.97
tiered pricing   discount=$ 5.00  total=$ 94.97
promo WELCOME10  discount=$ 0.00  total=$ 99.97

checkout[flat $10.00 off]: discount=$10.00 total=$89.97
checkout[15% off]: discount=$15.00 total=$84.97
```

### Tests

The tests pin each strategy's arithmetic and the two structural properties that matter: tier selection walks from the highest eligible threshold down, and the `Checkout` swaps strategies without changing its pricing path. `TestTieredPricing_NoEligibleTier` is the boundary case — a negative subtotal clears no tier, not even the `MinAmount: 0` floor, and returns the input unchanged.

Create `strategy_test.go`:

```go
package pricing

import "testing"

func TestFlatDiscount_CappedAtSubtotal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		amount, sub      float64
		wantD, wantTotal float64
	}{
		{"normal", 10, 100, 10, 90},
		{"exact", 100, 100, 100, 0},
		{"over", 200, 50, 50, 0},
		{"empty cart", 10, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, total := FlatDiscount{Amount: tc.amount}.Calculate(tc.sub)
			if d != tc.wantD || total != tc.wantTotal {
				t.Errorf("Flat(%v) on %v = (%v, %v), want (%v, %v)", tc.amount, tc.sub, d, total, tc.wantD, tc.wantTotal)
			}
		})
	}
}

func TestPercentageDiscount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		percent, sub, wantD, wantT float64
	}{
		{15, 150, 22.5, 127.5},
		{0, 100, 0, 100},
		{100, 80, 80, 0},
	}
	for _, tc := range cases {
		d, total := PercentageDiscount{Percent: tc.percent}.Calculate(tc.sub)
		if d != tc.wantD || total != tc.wantT {
			t.Errorf("Pct(%v) on %v = (%v, %v), want (%v, %v)", tc.percent, tc.sub, d, total, tc.wantD, tc.wantT)
		}
	}
}

func TestTieredPricing_PicksHighestEligibleTier(t *testing.T) {
	t.Parallel()

	tiers := TieredPricing{Tiers: []Tier{
		{MinAmount: 0, Percent: 0},
		{MinAmount: 50, Percent: 5},
		{MinAmount: 100, Percent: 10},
		{MinAmount: 200, Percent: 15},
	}}
	cases := []struct {
		sub, wantD float64
	}{
		{40, 0},
		{75, 3.75},
		{150, 15},
		{200, 30},
	}
	for _, tc := range cases {
		d, total := tiers.Calculate(tc.sub)
		if d != tc.wantD || total != tc.sub-tc.wantD {
			t.Errorf("tiers on %v = (%v, %v), want (%v, %v)", tc.sub, d, total, tc.wantD, tc.sub-tc.wantD)
		}
	}
}

func TestTieredPricing_NoEligibleTier(t *testing.T) {
	t.Parallel()

	tiers := TieredPricing{Tiers: []Tier{
		{MinAmount: 0, Percent: 0},
		{MinAmount: 50, Percent: 5},
	}}
	d, total := tiers.Calculate(-1)
	if d != 0 || total != -1 {
		t.Errorf("negative subtotal = (%v, %v), want (0, -1)", d, total)
	}
}

func TestPromotionalPricing_RespectsMinimum(t *testing.T) {
	t.Parallel()

	p := PromotionalPricing{Code: "WELCOME10", Flat: 10, MinSubtotal: 100}
	if d, total := p.Calculate(50); d != 0 || total != 50 {
		t.Errorf("below minimum: (%v, %v), want (0, 50)", d, total)
	}
	if d, total := p.Calculate(150); d != 10 || total != 140 {
		t.Errorf("above minimum: (%v, %v), want (10, 140)", d, total)
	}
}

func TestCheckout_SwapsStrategyAtRuntime(t *testing.T) {
	t.Parallel()

	co := NewCheckout(FlatDiscount{Amount: 10})
	if d, total := co.Process([]float64{50, 50}); d != 10 || total != 90 {
		t.Errorf("flat checkout = (%v, %v), want (10, 90)", d, total)
	}
	co.SetStrategy(PercentageDiscount{Percent: 20})
	if d, total := co.Process([]float64{50, 50}); d != 20 || total != 80 {
		t.Errorf("pct checkout = (%v, %v), want (20, 80)", d, total)
	}
}

func TestCheckout_StrategyAccessor(t *testing.T) {
	t.Parallel()

	want := PercentageDiscount{Percent: 10}
	co := NewCheckout(want)
	if got := co.Strategy(); got != PricingStrategy(want) {
		t.Errorf("Strategy() = %v, want %v", got, want)
	}
}
```

## Review

The design is sound when the context names no concrete strategy. Read `Process` and confirm it calls `c.pricing.Calculate` and nothing else — no `switch`, no type assertion, no construction of a specific strategy. That is what lets a fifth discount type be added as a new struct with two methods and zero edits to `Checkout`. Confirm each `Calculate` returns the full `(discount, total)` pair so no caller subtracts for itself, and that the guards hold at the edges: a flat or promo discount never exceeds the subtotal, an empty or negative cart never produces a negative total, and `TieredPricing` falls through to no discount when the subtotal clears no threshold.

Common mistakes for this feature. The first is moving the dispatch back into the context — a `Process(items, kind string)` that switches on `kind` reintroduces exactly the coupling the pattern removes; keep `Process` strategy-blind and inject through `NewCheckout`/`SetStrategy`. The second is returning only the discount from `Calculate`, which scatters `subtotal - discount` across callers where it drifts. The third is widening the interface with methods most strategies do not need; `Calculate` plus `Name` is already at the edge of what every strategy should be forced to implement.

## Resources

- [Strategy pattern](https://refactoring.guru/design-patterns/strategy) — the language-agnostic description of the context/strategy/contract roles this exercise encodes in Go.
- [A Tour of Go: Interfaces](https://go.dev/tour/methods/9) — how a value satisfies an interface structurally, which is what lets a new strategy plug in without touching the context.
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — idiomatic Go interface design, including keeping interfaces small.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-strategy-via-function-types.md](02-strategy-via-function-types.md)
