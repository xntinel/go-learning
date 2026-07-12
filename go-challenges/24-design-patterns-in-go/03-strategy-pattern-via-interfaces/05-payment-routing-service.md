# Exercise 5: A Payment-Routing Service with Strategy Selection and Fallback

A real payment platform does not hard-wire one processor. It holds several — each a strategy with its own regional coverage, fee schedule, and current health — and for every charge it selects the best eligible one and falls back when the preferred choice is down. This exercise builds that router: a `Processor` strategy interface, two concrete fee models, and a `Router` that picks the cheapest healthy processor serving the payment's region, falling back to the next best when the preferred one is unavailable, and returns the chosen `Route` with the reason it was chosen.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
routing.go           Processor interface; PercentProcessor, FlatProcessor;
                     Payment, Route; Router with Route() selection + fallback
cmd/
  demo/
    main.go          route several payments across regions and print each Route
routing_test.go      primary selection, fallback on unhealthy, unserved region,
                     all-unhealthy error, and fee math
```

- Files: `routing.go`, `cmd/demo/main.go`, `routing_test.go`.
- Implement: the `Processor` interface (`Name`, `Supports`, `Healthy`, `Quote`), two concrete processors, and a `Router` whose `Route` selects and falls back.
- Test: cheapest-healthy primary selection, fallback when the preferred processor is unhealthy, an error for an unserved region, an error when every in-region processor is down, and the per-processor fee math.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/05-payment-routing-service/cmd/demo && cd go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/05-payment-routing-service
```

### Each processor is a strategy

The unit of variation is a payment processor, and the system depends only on what every processor can answer:

```go
type Processor interface {
	Name() string
	Supports(region string) bool
	Healthy() bool
	Quote(p Payment) int64
}
```

`Supports` reports regional eligibility, `Healthy` reports whether the processor is currently accepting traffic (a value a real system would refresh from health checks or circuit breakers), and `Quote` returns the fee in integer cents for a given payment. Money is integer cents throughout — never `float64` — because fees are exact accounting quantities and floating-point rounding has no place in a billing path.

Two concrete strategies implement the interface with genuinely different fee models. `PercentProcessor` charges basis points of the amount plus a fixed component — the shape of a card acquirer's "2.9% + 30c". `FlatProcessor` charges one fixed fee regardless of amount — the shape of a flat-rate bank rail. They are different *algorithms* for the same `Quote` question, which is what makes this a strategy family rather than one configurable type, and the router compares them only through `Quote`, never by knowing which kind it holds.

### Selection, then fallback, expressed as filters

`Router.Route` is the policy, and it reads as a short pipeline over the processor list:

1. **Eligibility by region.** Keep only processors whose `Supports` returns true for the payment's region. If none remain, the region is unserved and that is an error — naming the region so an operator can see the coverage gap.
2. **The preferred route.** Among the eligible processors, the cheapest by `Quote` is the *preferred* choice, evaluated independent of health. This is what the platform would route to if everything were up.
3. **Health filter and fallback.** Keep only the eligible processors that are also `Healthy`. If none are healthy, every processor for that region is down and that is a distinct error. Otherwise the *chosen* route is the cheapest healthy one. When the chosen processor is the preferred one, the route is "primary"; when it differs — because the preferred was unhealthy — the route records a fallback, naming both the processor it wanted and the one it used.

Separating "preferred" from "chosen" is what turns fallback from a vague notion into an explicit, inspectable outcome. The returned `Route` carries the processor name, the fee, and a human-readable reason, so the decision is auditable: a finance or on-call engineer reading a log sees not just where the charge went but *why*, including whether a fallback fired. That auditability is the real-world requirement that distinguishes a routing service from a one-line "pick the first processor" helper.

A note on the comparison helper: `cheapest` uses a strict `<` when scanning, so on a fee tie the earliest processor in the list wins. That makes selection deterministic and lets the configuration order encode a tie-break preference, rather than leaving the choice to map iteration or some other unstable order.

Create `routing.go`:

```go
package paymentrouting

import "fmt"

// Payment is the charge to route. Money is integer cents to keep fees exact.
type Payment struct {
	Region      string
	AmountCents int64
	Currency    string
}

// Route is the routing decision: which processor handles the payment, the fee
// it quoted, and why it was chosen (including whether a fallback fired).
type Route struct {
	Processor string
	FeeCents  int64
	Reason    string
}

// Processor is the strategy contract for a payment processor. The router
// depends only on these four questions and never on a concrete type.
type Processor interface {
	Name() string
	Supports(region string) bool
	Healthy() bool
	Quote(p Payment) int64
}

// PercentProcessor charges basis points of the amount plus a fixed component,
// e.g. Bps=290, FlatCents=30 is "2.90% + 30c". A region of "*" serves every
// region.
type PercentProcessor struct {
	ProcName  string
	Regions   []string
	Up        bool
	Bps       int64
	FlatCents int64
}

func (p PercentProcessor) Name() string { return p.ProcName }

func (p PercentProcessor) Supports(region string) bool { return servesRegion(p.Regions, region) }

func (p PercentProcessor) Healthy() bool { return p.Up }

func (p PercentProcessor) Quote(pm Payment) int64 {
	return pm.AmountCents*p.Bps/10000 + p.FlatCents
}

// FlatProcessor charges one fixed fee regardless of amount, the shape of a
// flat-rate bank rail.
type FlatProcessor struct {
	ProcName string
	Regions  []string
	Up       bool
	Fee      int64
}

func (f FlatProcessor) Name() string { return f.ProcName }

func (f FlatProcessor) Supports(region string) bool { return servesRegion(f.Regions, region) }

func (f FlatProcessor) Healthy() bool { return f.Up }

func (f FlatProcessor) Quote(pm Payment) int64 { return f.Fee }

// servesRegion reports whether regions covers region; "*" matches any region.
func servesRegion(regions []string, region string) bool {
	for _, r := range regions {
		if r == region || r == "*" {
			return true
		}
	}
	return false
}

// Router holds the available processors and routes each payment to one.
type Router struct {
	processors []Processor
}

// NewRouter builds a router over the given processors. Their order is the
// tie-break preference when fees are equal.
func NewRouter(processors ...Processor) *Router {
	return &Router{processors: processors}
}

// Route selects a processor for p: the cheapest healthy processor serving p's
// region, falling back from the preferred (cheapest overall) one when it is
// unhealthy. It errors if no processor serves the region, or if every processor
// for the region is unhealthy.
func (r *Router) Route(p Payment) (Route, error) {
	var eligible []Processor
	for _, proc := range r.processors {
		if proc.Supports(p.Region) {
			eligible = append(eligible, proc)
		}
	}
	if len(eligible) == 0 {
		return Route{}, fmt.Errorf("paymentrouting: no processor serves region %q", p.Region)
	}

	// The preferred route is the cheapest eligible processor, healthy or not.
	preferred := cheapest(eligible, p)

	var healthy []Processor
	for _, proc := range eligible {
		if proc.Healthy() {
			healthy = append(healthy, proc)
		}
	}
	if len(healthy) == 0 {
		return Route{}, fmt.Errorf("paymentrouting: every processor for region %q is unhealthy", p.Region)
	}

	chosen := cheapest(healthy, p)
	route := Route{Processor: chosen.Name(), FeeCents: chosen.Quote(p)}
	if chosen.Name() == preferred.Name() {
		route.Reason = "primary: cheapest healthy processor in region"
	} else {
		route.Reason = fmt.Sprintf("fallback: preferred %q unavailable, routed to %q", preferred.Name(), chosen.Name())
	}
	return route, nil
}

// cheapest returns the processor with the lowest Quote; ties go to the earliest
// in the slice, making selection deterministic.
func cheapest(ps []Processor, p Payment) Processor {
	best := ps[0]
	bestFee := best.Quote(p)
	for _, proc := range ps[1:] {
		if fee := proc.Quote(p); fee < bestFee {
			best, bestFee = proc, fee
		}
	}
	return best
}
```

### The runnable demo

The demo wires three processors with overlapping regional coverage and one deliberately marked down, then routes four payments. The `eu` charge shows fallback: the cheapest eligible processor (`eu-local-bank`, a flat 150c) is unhealthy, so the router falls back to the next cheapest healthy one (`adyen`). The `us` and `asia` charges show ordinary primary selection, and `antarctica` shows the unserved-region error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/paymentrouting"
)

func main() {
	router := paymentrouting.NewRouter(
		paymentrouting.PercentProcessor{ProcName: "stripe", Regions: []string{"us", "eu"}, Up: true, Bps: 290, FlatCents: 30},
		paymentrouting.PercentProcessor{ProcName: "adyen", Regions: []string{"eu", "asia"}, Up: true, Bps: 250, FlatCents: 10},
		paymentrouting.FlatProcessor{ProcName: "eu-local-bank", Regions: []string{"eu"}, Up: false, Fee: 150},
	)

	payments := []paymentrouting.Payment{
		{Region: "eu", AmountCents: 10000, Currency: "EUR"},
		{Region: "us", AmountCents: 10000, Currency: "USD"},
		{Region: "asia", AmountCents: 5000, Currency: "SGD"},
		{Region: "antarctica", AmountCents: 5000, Currency: "USD"},
	}
	for _, p := range payments {
		route, err := router.Route(p)
		if err != nil {
			fmt.Printf("%-11s -> error: %v\n", p.Region, err)
			continue
		}
		fmt.Printf("%-11s -> %-13s fee=%3d cents  (%s)\n", p.Region, route.Processor, route.FeeCents, route.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
eu          -> adyen         fee=260 cents  (fallback: preferred "eu-local-bank" unavailable, routed to "adyen")
us          -> stripe        fee=320 cents  (primary: cheapest healthy processor in region)
asia        -> adyen         fee=135 cents  (primary: cheapest healthy processor in region)
antarctica  -> error: paymentrouting: no processor serves region "antarctica"
```

### Tests

The tests pin the selection policy and both failure modes. `TestRoute_PrimaryCheapestHealthy` confirms the cheapest healthy in-region processor wins; `TestRoute_FallbackWhenPreferredUnhealthy` confirms an unhealthy cheapest forces a fallback whose reason names both processors; `TestRoute_HealthBeatsCheaperUnhealthy` confirms a healthy pricier processor is preferred over a cheaper down one; and two error tests cover an unserved region and an all-unhealthy region. `TestProcessor_QuoteMath` pins the two fee models.

Create `routing_test.go`:

```go
package paymentrouting

import (
	"strings"
	"testing"
)

func TestRoute_PrimaryCheapestHealthy(t *testing.T) {
	t.Parallel()

	r := NewRouter(
		PercentProcessor{ProcName: "stripe", Regions: []string{"eu"}, Up: true, Bps: 290, FlatCents: 30},
		PercentProcessor{ProcName: "adyen", Regions: []string{"eu"}, Up: true, Bps: 250, FlatCents: 10},
	)
	got, err := r.Route(Payment{Region: "eu", AmountCents: 10000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Processor != "adyen" {
		t.Errorf("processor = %q, want adyen (cheapest)", got.Processor)
	}
	if got.FeeCents != 260 {
		t.Errorf("fee = %d, want 260", got.FeeCents)
	}
	if !strings.HasPrefix(got.Reason, "primary") {
		t.Errorf("reason = %q, want a primary route", got.Reason)
	}
}

func TestRoute_FallbackWhenPreferredUnhealthy(t *testing.T) {
	t.Parallel()

	r := NewRouter(
		FlatProcessor{ProcName: "eu-local-bank", Regions: []string{"eu"}, Up: false, Fee: 150},
		PercentProcessor{ProcName: "adyen", Regions: []string{"eu"}, Up: true, Bps: 250, FlatCents: 10},
	)
	got, err := r.Route(Payment{Region: "eu", AmountCents: 10000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Processor != "adyen" {
		t.Errorf("processor = %q, want adyen (fallback target)", got.Processor)
	}
	if !strings.Contains(got.Reason, "fallback") ||
		!strings.Contains(got.Reason, "eu-local-bank") ||
		!strings.Contains(got.Reason, "adyen") {
		t.Errorf("reason = %q, want a fallback naming both processors", got.Reason)
	}
}

func TestRoute_HealthBeatsCheaperUnhealthy(t *testing.T) {
	t.Parallel()

	// The cheapest processor is down; the healthy pricier one must be chosen.
	r := NewRouter(
		FlatProcessor{ProcName: "cheap-but-down", Regions: []string{"us"}, Up: false, Fee: 50},
		FlatProcessor{ProcName: "pricey-but-up", Regions: []string{"us"}, Up: true, Fee: 500},
	)
	got, err := r.Route(Payment{Region: "us", AmountCents: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Processor != "pricey-but-up" || got.FeeCents != 500 {
		t.Errorf("got (%q, %d), want (pricey-but-up, 500)", got.Processor, got.FeeCents)
	}
}

func TestRoute_UnservedRegionErrors(t *testing.T) {
	t.Parallel()

	r := NewRouter(
		PercentProcessor{ProcName: "stripe", Regions: []string{"us"}, Up: true, Bps: 290, FlatCents: 30},
	)
	_, err := r.Route(Payment{Region: "antarctica", AmountCents: 1000})
	if err == nil {
		t.Fatal("unserved region: want error, got nil")
	}
	if !strings.Contains(err.Error(), "antarctica") {
		t.Errorf("error %q should name the region", err.Error())
	}
}

func TestRoute_AllUnhealthyErrors(t *testing.T) {
	t.Parallel()

	r := NewRouter(
		PercentProcessor{ProcName: "stripe", Regions: []string{"eu"}, Up: false, Bps: 290, FlatCents: 30},
		FlatProcessor{ProcName: "eu-local-bank", Regions: []string{"eu"}, Up: false, Fee: 150},
	)
	_, err := r.Route(Payment{Region: "eu", AmountCents: 1000})
	if err == nil {
		t.Fatal("all unhealthy: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("error %q should report the unhealthy region", err.Error())
	}
}

func TestProcessor_QuoteMath(t *testing.T) {
	t.Parallel()

	pct := PercentProcessor{Bps: 290, FlatCents: 30}
	if got := pct.Quote(Payment{AmountCents: 10000}); got != 320 {
		t.Errorf("percent quote = %d, want 320", got)
	}
	flat := FlatProcessor{Fee: 150}
	if got := flat.Quote(Payment{AmountCents: 999999}); got != 150 {
		t.Errorf("flat quote = %d, want 150 regardless of amount", got)
	}
}
```

## Review

The router is correct when selection is health-aware, cost-minimizing, and fallback is explicit. Confirm `Route` first filters by region and errors on an empty result, so an unserved region is a clear coverage signal rather than a panic on an empty slice. Confirm the preferred route is computed independently of health, because that is what lets the reason distinguish a primary route from a fallback — collapse the two and you lose the audit trail that tells an operator a processor was down. Confirm a healthy pricier processor beats a cheaper unhealthy one: routing a charge to a down processor to save a few cents is the failure this design exists to prevent. Confirm the router compares processors only through `Quote`, `Supports`, and `Healthy`, never by switching on concrete type, so a third processor with a new fee model joins by being passed to `NewRouter` with no change to `Route`.

Common mistakes for this feature. The first is conflating "no processor serves this region" with "every processor here is down" — they are different operational problems (a coverage gap versus an outage) and deserve distinct errors. The second is using `float64` for money; fees are exact cents and must be integers all the way through. The third is making fallback implicit: returning only the chosen processor without recording that the preferred one was skipped throws away the one fact an incident reviewer needs. The fourth is non-deterministic tie-breaking; without a stable rule (here, list order via strict `<`), two equal-fee processors could be chosen unpredictably, making the router's behavior impossible to reason about or test.

## Resources

- [Strategy pattern](https://refactoring.guru/design-patterns/strategy) — the context/strategy/contract roles that the router, `Processor`, and the concrete processors play here.
- [Payment gateway (Wikipedia)](https://en.wikipedia.org/wiki/Payment_gateway) — what payment processors and gateways do, the domain this router coordinates.
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — keeping the `Processor` interface small so a new processor satisfies it structurally with no change to the router.

---

Back to [04-rate-limiter-strategies.md](04-rate-limiter-strategies.md) | Next: [../04-dependency-injection/00-concepts.md](../04-dependency-injection/00-concepts.md)
