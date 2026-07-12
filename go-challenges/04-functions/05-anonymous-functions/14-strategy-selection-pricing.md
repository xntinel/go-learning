# Exercise 14: Selecting a Pricing Strategy from a Map of Literals

**Nivel: Intermedio** — validacion rapida (un test corto).

Different customer tiers apply different discounts, and the cleanest way to
express "pick the right rule for this tier" is a map from tier name straight
to a function value — no `switch` to extend every time a tier is added. This
module builds that selector, where each strategy is a small anonymous
function stored in the map and handed back to the caller as a callable
value.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
pricing/                      module example.com/pricing
  go.mod
  pricing.go                  Strategy, strategies map, Select, Apply
  pricing_test.go             known tiers, unknown tier, calling a selected strategy directly
```

- Files: `pricing.go`, `pricing_test.go`.
- Implement: `Strategy` (`func(base float64) float64`); a package-level `strategies` map of tier name to anonymous `Strategy` literal; `Select(tier) (Strategy, error)`; `Apply(tier, base) (float64, error)` that selects and calls the strategy, rounding to cents.
- Test: `standard`, `member`, and `vip` each produce the expected discounted price; an unknown tier returns an error from `Apply`; `Select` returns a function value the test can call directly.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/14-strategy-selection-pricing
cd go-solutions/04-functions/05-anonymous-functions/14-strategy-selection-pricing
go mod edit -go=1.24
```

### A map of function values as a strategy table

`strategies` is `map[string]Strategy`, and each entry is an anonymous
function literal — the strategy pattern without an interface or a type
switch. Adding a new tier is one more map entry, not a new `case` in a
`switch` scattered across the codebase. `Select` separates "find the right
behavior" from "run it": it returns the `Strategy` itself, a plain callable
value, so a caller that wants to apply the same strategy to many prices can
select once and call many times instead of paying the map lookup on every
call. `Apply` is the common case that does both in one step.

Create `pricing.go`:

```go
package pricing

import (
	"fmt"
	"math"
)

// Strategy computes a final price from a base price.
type Strategy func(base float64) float64

// strategies maps a customer tier to the discount rule for that tier, each
// one an anonymous function value. Selecting a strategy is just a map
// lookup; applying it is just calling the returned function value.
var strategies = map[string]Strategy{
	"standard": func(base float64) float64 {
		return base
	},
	"member": func(base float64) float64 {
		return base * 0.90
	},
	"vip": func(base float64) float64 {
		return base * 0.80
	},
}

// Select returns the Strategy registered for tier, or an error if tier is
// not registered. The returned value is a plain function value the caller
// can invoke directly or store.
func Select(tier string) (Strategy, error) {
	strat, ok := strategies[tier]
	if !ok {
		return nil, fmt.Errorf("pricing: unknown tier %q", tier)
	}
	return strat, nil
}

// Apply looks up tier's strategy and applies it to base, rounding to cents.
func Apply(tier string, base float64) (float64, error) {
	strat, err := Select(tier)
	if err != nil {
		return 0, err
	}
	return round2(strat(base)), nil
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
```

### Tests

`TestApplyKnownTiers` is table-driven over the three registered tiers and
their expected discounted totals. `TestApplyUnknownTier` checks the error
path. `TestSelectReturnsCallableStrategy` calls `Select` directly and invokes
the returned function value itself, confirming it behaves the same as going
through `Apply`.

Create `pricing_test.go`:

```go
package pricing

import "testing"

func TestApplyKnownTiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tier string
		base float64
		want float64
	}{
		{"standard", 100, 100.00},
		{"member", 100, 90.00},
		{"vip", 100, 80.00},
	}
	for _, tc := range cases {
		got, err := Apply(tc.tier, tc.base)
		if err != nil {
			t.Fatalf("Apply(%q, %v) error = %v, want nil", tc.tier, tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("Apply(%q, %v) = %v, want %v", tc.tier, tc.base, got, tc.want)
		}
	}
}

func TestApplyUnknownTier(t *testing.T) {
	t.Parallel()
	if _, err := Apply("platinum", 100); err == nil {
		t.Fatal("Apply(platinum, 100) error = nil, want error")
	}
}

func TestSelectReturnsCallableStrategy(t *testing.T) {
	t.Parallel()
	strat, err := Select("vip")
	if err != nil {
		t.Fatalf("Select(vip) error = %v, want nil", err)
	}
	if got := round2(strat(50)); got != 40.00 {
		t.Fatalf("selected strategy(50) = %v, want 40", got)
	}
}
```

## Review

`strategies` is the whole design: a lookup table of function values replaces
what would otherwise be a `switch` that grows a case per tier and tends to
drift out of sync with whatever list of valid tiers the rest of the system
uses. `TestSelectReturnsCallableStrategy` is the test that matters most here
— it proves `Select` really does hand back an ordinary, independently
callable function value, not something that only works when routed through
`Apply`.

## Resources

- [Function types](https://go.dev/ref/spec#Function_types)
- [Maps](https://go.dev/blog/maps)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-scoped-flag-override-cleanup.md](13-scoped-flag-override-cleanup.md) | Next: [15-functional-filter-map-reduce-pipeline.md](15-functional-filter-map-reduce-pipeline.md)
