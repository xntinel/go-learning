# Exercise 10: Resolve Per-Plan Rate Limits With an Init-Statement Switch

A multi-tenant limiter has to turn a customer's plan into concrete numbers: how
many requests per second, what burst, what daily quota. This module builds that
resolver, and its defining feature is the *fail-closed default* — an unknown plan
resolves to the safest (Free) limits with an error, so a bug or a new plan can
never accidentally grant unlimited access.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
ratelimit/                 independent module: example.com/rate-limit-tier-resolver
  go.mod                   go 1.24
  ratelimit.go             Plan/Tier enums; LimitsFor(plan); MaxConcurrency(plan)
  cmd/
    demo/
      main.go              runnable demo resolving each plan and an unknown one
  ratelimit_test.go        exact-limits table + fail-closed + monotonic-burst check
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `LimitsFor(plan Plan) (RateLimits, error)` (`switch plan`) and `MaxConcurrency(plan Plan) int` (`switch tier := plan.Tier(); tier`), both fail-closed.
- Test: each known plan yields its exact `RateLimits`, an unknown plan fails closed to Free with a non-nil error, and Enterprise burst >= Pro burst >= Free burst.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Fail closed, and scope the tier to its switch

Two switch ideas meet here. `LimitsFor` is a plain expression switch on the plan,
mapping each known plan to a `RateLimits` struct. Its `default` is the exercise's
whole point: an unknown plan must not fall through to permissive or zero limits —
it returns the *Free* (most restrictive) limits *and* a non-nil error, so the
caller can log the anomaly while the limiter still applies a safe cap. A default
that returned unlimited, or an empty `RateLimits{}` that a naive limiter reads as
"no limit", is exactly the cost-and-security failure the concepts file warns
about. Fail closed means: when in doubt, restrict.

`MaxConcurrency` shows the init-statement form: `switch tier := plan.Tier(); tier`.
The plan is first mapped to a coarser `Tier`, and the tier variable is scoped to
the switch — it does not leak into the surrounding function, which is the point of
the init form. This is the resolve-then-branch pattern: compute the tier, branch
on it, discard the name. `Plan.Tier()` itself fails closed too, mapping any
unknown plan to the lowest tier.

The test asserts a monotonicity invariant — Enterprise burst >= Pro burst >= Free
burst — which is the cheap way to catch a value transposition (two structs' burst
values swapped in a copy-paste). A limiter where a cheaper plan accidentally got a
higher burst is a revenue leak; the ordering check turns that into a test failure.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"errors"
	"fmt"
)

// ErrUnknownPlan is returned by resolvers when a plan is not recognized; the
// resolver still returns safe (Free) limits alongside it.
var ErrUnknownPlan = errors.New("unknown plan")

// Plan is a customer's subscription plan.
type Plan int

const (
	PlanUnknown Plan = iota
	PlanFree
	PlanPro
	PlanEnterprise
	PlanInternal
)

func (p Plan) String() string {
	switch p {
	case PlanFree:
		return "free"
	case PlanPro:
		return "pro"
	case PlanEnterprise:
		return "enterprise"
	case PlanInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Tier is a coarse grouping of plans used for infrastructure limits.
type Tier int

const (
	TierBasic Tier = iota
	TierStandard
	TierPremium
)

// Tier maps a plan to its tier, failing closed to TierBasic for unknown plans.
func (p Plan) Tier() Tier {
	switch p {
	case PlanEnterprise, PlanInternal:
		return TierPremium
	case PlanPro:
		return TierStandard
	default:
		return TierBasic
	}
}

// RateLimits are the concrete limits applied to a request stream.
type RateLimits struct {
	RPS        int
	Burst      int
	DailyQuota int // 0 means unlimited
}

// LimitsFor resolves a plan to its RateLimits. Unknown plans fail closed to the
// safest (Free) limits plus a non-nil error, so a bug or a new plan can never
// grant unlimited access by accident.
func LimitsFor(plan Plan) (RateLimits, error) {
	free := RateLimits{RPS: 5, Burst: 10, DailyQuota: 10_000}
	switch plan {
	case PlanFree:
		return free, nil
	case PlanPro:
		return RateLimits{RPS: 50, Burst: 100, DailyQuota: 1_000_000}, nil
	case PlanEnterprise:
		return RateLimits{RPS: 500, Burst: 1_000, DailyQuota: 100_000_000}, nil
	case PlanInternal:
		return RateLimits{RPS: 5_000, Burst: 10_000, DailyQuota: 0}, nil
	default:
		return free, fmt.Errorf("%w: %d (falling back to free limits)", ErrUnknownPlan, int(plan))
	}
}

// MaxConcurrency resolves the max concurrent connections for a plan using an
// init-statement switch: the tier is scoped to the switch and does not leak.
func MaxConcurrency(plan Plan) int {
	switch tier := plan.Tier(); tier {
	case TierPremium:
		return 256
	case TierStandard:
		return 64
	default:
		return 8
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rate-limit-tier-resolver"
)

func main() {
	plans := []ratelimit.Plan{
		ratelimit.PlanFree,
		ratelimit.PlanPro,
		ratelimit.PlanEnterprise,
		ratelimit.PlanInternal,
		ratelimit.Plan(99), // unknown
	}
	for _, p := range plans {
		limits, err := ratelimit.LimitsFor(p)
		note := ""
		if err != nil {
			note = " (" + err.Error() + ")"
		}
		fmt.Printf("%-11s rps=%-5d burst=%-6d quota=%-11d conc=%d%s\n",
			p, limits.RPS, limits.Burst, limits.DailyQuota, ratelimit.MaxConcurrency(p), note)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
free        rps=5     burst=10     quota=10000       conc=8
pro         rps=50    burst=100    quota=1000000     conc=64
enterprise  rps=500   burst=1000   quota=100000000   conc=256
internal    rps=5000  burst=10000  quota=0           conc=256
unknown     rps=5     burst=10     quota=10000       conc=8 (unknown plan: 99 (falling back to free limits))
```

### Tests

`TestLimitsFor` asserts each known plan yields its exact `RateLimits`.
`TestUnknownPlanFailsClosed` proves an unknown plan returns the Free limits *and*
`errors.Is(err, ErrUnknownPlan)` — never unlimited. `TestBurstMonotonic` checks
Enterprise >= Pro >= Free burst to catch a value transposition, and
`TestMaxConcurrency` covers the init-statement switch across tiers.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"errors"
	"testing"
)

func TestLimitsFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		plan Plan
		want RateLimits
	}{
		{PlanFree, RateLimits{RPS: 5, Burst: 10, DailyQuota: 10_000}},
		{PlanPro, RateLimits{RPS: 50, Burst: 100, DailyQuota: 1_000_000}},
		{PlanEnterprise, RateLimits{RPS: 500, Burst: 1_000, DailyQuota: 100_000_000}},
		{PlanInternal, RateLimits{RPS: 5_000, Burst: 10_000, DailyQuota: 0}},
	}

	for _, tc := range tests {
		got, err := LimitsFor(tc.plan)
		if err != nil {
			t.Errorf("LimitsFor(%s) err = %v, want nil", tc.plan, err)
			continue
		}
		if got != tc.want {
			t.Errorf("LimitsFor(%s) = %+v, want %+v", tc.plan, got, tc.want)
		}
	}
}

func TestUnknownPlanFailsClosed(t *testing.T) {
	t.Parallel()

	free, _ := LimitsFor(PlanFree)
	for _, p := range []Plan{PlanUnknown, Plan(99), Plan(-1)} {
		got, err := LimitsFor(p)
		if !errors.Is(err, ErrUnknownPlan) {
			t.Errorf("LimitsFor(%d) err = %v, want errors.Is ErrUnknownPlan", int(p), err)
		}
		if got != free {
			t.Errorf("LimitsFor(%d) = %+v, want fail-closed free limits %+v", int(p), got, free)
		}
	}
}

func TestBurstMonotonic(t *testing.T) {
	t.Parallel()

	free, _ := LimitsFor(PlanFree)
	pro, _ := LimitsFor(PlanPro)
	ent, _ := LimitsFor(PlanEnterprise)
	if !(ent.Burst >= pro.Burst && pro.Burst >= free.Burst) {
		t.Errorf("burst not monotonic: free=%d pro=%d enterprise=%d", free.Burst, pro.Burst, ent.Burst)
	}
}

func TestMaxConcurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		plan Plan
		want int
	}{
		{PlanFree, 8},
		{PlanPro, 64},
		{PlanEnterprise, 256},
		{PlanInternal, 256},
		{Plan(99), 8}, // unknown fails closed to the lowest tier
	}
	for _, tc := range tests {
		if got := MaxConcurrency(tc.plan); got != tc.want {
			t.Errorf("MaxConcurrency(%s) = %d, want %d", tc.plan, got, tc.want)
		}
	}
}
```

Note the compile-shape of the init form: `tier` from
`switch tier := plan.Tier(); tier` is in scope only inside `MaxConcurrency`'s
switch; referencing it after the closing brace would not compile, which is the
lifetime narrowing the init form buys.

## Review

The resolver is correct when every known plan maps to its exact limits and every
unknown one fails closed to the safest limits with an error — never to unlimited
or to a zero-value struct a naive limiter treats as "no cap". The
`TestUnknownPlanFailsClosed` case is the one that matters most in production: it
is the difference between a new plan enum value quietly bypassing the limiter and
the limiter safely capping it while the error surfaces the misconfiguration. The
monotonic-burst check is a cheap guard against transposed values, and the
init-statement `MaxConcurrency` shows the resolve-then-branch pattern with the
tier scoped to exactly the switch that uses it.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the init-statement (`switch x := f(); x`) form and scoping.
- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the token-bucket limiter these RPS/Burst values feed.
- [Effective Go: Switch](https://go.dev/doc/effective_go#switch) — idiomatic expression and comma-list switches.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-maintenance-window-gate.md](09-maintenance-window-gate.md) | Next: [11-webhook-event-router.md](11-webhook-event-router.md)
