# Exercise 29: Evaluate Complex Feature Flag Targeting Rules

**Nivel: Intermedio** — validacion rapida (un test corto).

Every feature-flagging service — LaunchDarkly, Unleash, or the homegrown
version most companies build before they can justify buying one — exposes
the same handful of targeting primitives to product teams: roll out to a
percentage of users, restrict to certain organization tiers, restrict to
certain regions. "Roll out to 10% of Enterprise customers in the EU" isn't
one bespoke conditional; it's three composable rules evaluated together.
This module builds that composition: an outer switch dispatching on rule
type, and a nested tagless switch inside the tier/region rules composing
list membership with an include/exclude operator. It is self-contained:
its own `go mod init`, code, demo, and test.

## What you'll build

```text
featureflag/                independent module: example.com/feature-flag-rule-evaluator
  go.mod                     go 1.24
  featureflag.go              package featureflag; Rule; RuleType; ClauseOp; User; Evaluate([]Rule, User) bool
  cmd/demo/main.go            runnable demo over four users against a three-rule targeting set
  featureflag_test.go         table over the composed rule set, an empty rule set, and the clause operators in isolation
```

- Implement: `Evaluate(rules []Rule, u User) bool` — an outer switch on `RuleType` dispatches to a rollout-percentage check or a nested tagless switch (`matchesClause`) composing list membership with an `in`/`not_in` operator.
- Test: a table over the full three-rule composition (rollout, tier, region interacting), an empty rule set failing closed, and the clause operators tested directly.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/29-feature-flag-rule-evaluator/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/29-feature-flag-rule-evaluator
go mod edit -go=1.24
```

### Why the rollout percentage hashes on user ID, not a random draw

`RuleRollout` computes `u.ID % 100 < r.RolloutPercent`, and the choice to
hash a stable user attribute rather than draw a fresh random number per
request is the entire point of a percentage rollout: a user's eligibility
must be stable across requests, or the feature flickers on and off for the
same person from one page load to the next, which is worse for both user
experience and for engineers trying to reproduce a bug report. Hashing (or
here, simply taking the ID modulo 100, since the exercise's `User.ID` is
already a uniformly distributed synthetic key) guarantees the same input
always lands on the same side of the boundary.

`evaluateRule`'s outer switch on `RuleType` is a plain expression switch —
`RuleRollout`, `RuleOrgTier`, and `RuleRegion` are a closed, disjoint set,
so `==` comparison is exactly right and there's no ordering hazard to
reason about. `matchesClause`, underneath it, is a tagless switch composing
two independent booleans (list membership, and which operator was
requested) into the final answer:

```go
switch {
case op == OpIn && contains:
    return true
case op == OpNotIn && !contains:
    return true
default:
    return false
}
```

This is the nested-switch pattern the exercise is built to demonstrate:
rather than writing `if op == OpIn { return contains } else { return
!contains }` — which quietly assumes `op` is always one of exactly two
values and gives the wrong answer for any other input — the switch spells
out every combination explicitly and lets an unrecognized `op` value fall
through to `default: return false`, the fail-closed answer.

`Evaluate` requires every rule in the slice to match (logical AND), and an
empty rule slice returns `false`. That second choice is deliberate: a flag
with no targeting rules configured yet is off for everyone, not on for
everyone, which is the same fail-closed instinct the concepts file applies
to an unrecognized `default` case, extended to the "no rules at all" edge.

Create `featureflag.go`:

```go
// Package featureflag evaluates whether a feature is enabled for a given
// user by running a list of independent targeting rules, mirroring the
// rule types a real flagging service (LaunchDarkly, Unleash, or a
// homegrown one) exposes to product teams: percentage rollouts keyed off a
// stable user attribute, organization-tier gating, and geographic
// restriction. All rules attached to a flag must pass -- this is how
// "roll out to 10% of Enterprise customers in the EU" is expressed as three
// composable rules rather than one bespoke conditional.
package featureflag

import "slices"

// RuleType identifies which targeting dimension a Rule evaluates.
type RuleType int

const (
	RuleRollout RuleType = iota
	RuleOrgTier
	RuleRegion
)

// ClauseOp is the comparison a RuleOrgTier or RuleRegion rule applies
// between its Values list and the user's attribute.
type ClauseOp string

const (
	OpIn    ClauseOp = "in"
	OpNotIn ClauseOp = "not_in"
)

// Rule is one targeting condition. Only the fields relevant to its Type are
// populated; a RuleRollout ignores Op and Values, a RuleOrgTier or
// RuleRegion ignores RolloutPercent.
type Rule struct {
	Type           RuleType
	RolloutPercent int // used when Type == RuleRollout, 0-100
	Op             ClauseOp
	Values         []string
}

// User is the subject a flag is evaluated against.
type User struct {
	ID      uint64
	OrgTier string
	Region  string
}

// Evaluate reports whether every rule in rules matches u. An empty rule set
// evaluates to false -- a flag with no targeting rules attached is off by
// default, not on for everyone, which is the fail-closed choice for a
// feature that hasn't been explicitly configured yet.
func Evaluate(rules []Rule, u User) bool {
	if len(rules) == 0 {
		return false
	}
	for _, r := range rules {
		if !evaluateRule(r, u) {
			return false
		}
	}
	return true
}

// evaluateRule dispatches on the rule's Type. RuleRollout is a numeric
// modulo test against the user's stable ID -- stable specifically because
// hashing on ID (rather than a random draw per request) guarantees the
// same user always lands on the same side of the rollout boundary, so a
// user doesn't flicker between "has the feature" and "doesn't" from one
// request to the next. RuleOrgTier and RuleRegion both delegate to the same
// membership-and-operator check, since "is this attribute in (or not in)
// this list" is identical logic regardless of which attribute is being
// tested.
func evaluateRule(r Rule, u User) bool {
	switch r.Type {
	case RuleRollout:
		return u.ID%100 < uint64(r.RolloutPercent)
	case RuleOrgTier:
		return matchesClause(r.Op, r.Values, u.OrgTier)
	case RuleRegion:
		return matchesClause(r.Op, r.Values, u.Region)
	default:
		return false
	}
}

// matchesClause is the nested tagless switch: it composes list membership
// with the clause's operator, so "in" and "not_in" share one membership
// check instead of two near-duplicate loops that could drift apart.
func matchesClause(op ClauseOp, values []string, actual string) bool {
	contains := slices.Contains(values, actual)
	switch {
	case op == OpIn && contains:
		return true
	case op == OpNotIn && !contains:
		return true
	default:
		return false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	featureflag "example.com/feature-flag-rule-evaluator"
)

func main() {
	rules := []featureflag.Rule{
		{Type: featureflag.RuleRollout, RolloutPercent: 25},
		{Type: featureflag.RuleOrgTier, Op: featureflag.OpIn, Values: []string{"enterprise", "growth"}},
		{Type: featureflag.RuleRegion, Op: featureflag.OpNotIn, Values: []string{"restricted-zone"}},
	}

	users := []featureflag.User{
		{ID: 10, OrgTier: "enterprise", Region: "eu-west"},     // in rollout (10%100<25), enterprise, allowed region
		{ID: 40, OrgTier: "enterprise", Region: "eu-west"},     // outside rollout (40%100 >= 25)
		{ID: 5, OrgTier: "free", Region: "eu-west"},            // in rollout, wrong tier
		{ID: 12, OrgTier: "growth", Region: "restricted-zone"}, // in rollout, right tier, restricted region
	}

	for _, u := range users {
		fmt.Printf("user %+v -> enabled=%v\n", u, featureflag.Evaluate(rules, u))
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
user {ID:10 OrgTier:enterprise Region:eu-west} -> enabled=true
user {ID:40 OrgTier:enterprise Region:eu-west} -> enabled=false
user {ID:5 OrgTier:free Region:eu-west} -> enabled=false
user {ID:12 OrgTier:growth Region:restricted-zone} -> enabled=false
```

### Tests

`TestEvaluate` runs the three-rule composition from the demo plus the
exact rollout boundary. `TestEvaluateEmptyRuleSetIsDisabled` checks the
fail-closed default. `TestMatchesClauseOperators` drives all four
combinations of operator and membership directly against the nested
switch.

Create `featureflag_test.go`:

```go
package featureflag

import "testing"

func TestEvaluate(t *testing.T) {
	t.Parallel()

	rules := []Rule{
		{Type: RuleRollout, RolloutPercent: 25},
		{Type: RuleOrgTier, Op: OpIn, Values: []string{"enterprise", "growth"}},
		{Type: RuleRegion, Op: OpNotIn, Values: []string{"restricted-zone"}},
	}

	tests := []struct {
		name string
		u    User
		want bool
	}{
		{
			name: "inside rollout, allowed tier, allowed region",
			u:    User{ID: 10, OrgTier: "enterprise", Region: "eu-west"},
			want: true,
		},
		{
			name: "outside the rollout percentage",
			u:    User{ID: 40, OrgTier: "enterprise", Region: "eu-west"},
			want: false,
		},
		{
			name: "inside rollout but wrong org tier",
			u:    User{ID: 5, OrgTier: "free", Region: "eu-west"},
			want: false,
		},
		{
			name: "inside rollout, right tier, but restricted region",
			u:    User{ID: 12, OrgTier: "growth", Region: "restricted-zone"},
			want: false,
		},
		{
			name: "rollout boundary is exclusive on the upper edge",
			u:    User{ID: 25, OrgTier: "enterprise", Region: "eu-west"},
			want: false,
		},
	}

	for _, tc := range tests {
		if got := Evaluate(rules, tc.u); got != tc.want {
			t.Errorf("%s: Evaluate(%+v) = %v, want %v", tc.name, tc.u, got, tc.want)
		}
	}
}

func TestEvaluateEmptyRuleSetIsDisabled(t *testing.T) {
	t.Parallel()

	if Evaluate(nil, User{ID: 1, OrgTier: "enterprise", Region: "eu-west"}) {
		t.Fatal("Evaluate() with no rules = true, want false (fail closed)")
	}
}

func TestMatchesClauseOperators(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		op     ClauseOp
		values []string
		actual string
		want   bool
	}{
		{"in, present", OpIn, []string{"a", "b"}, "a", true},
		{"in, absent", OpIn, []string{"a", "b"}, "c", false},
		{"not_in, present", OpNotIn, []string{"a", "b"}, "a", false},
		{"not_in, absent", OpNotIn, []string{"a", "b"}, "c", true},
	}

	for _, tc := range tests {
		if got := matchesClause(tc.op, tc.values, tc.actual); got != tc.want {
			t.Errorf("%s: matchesClause() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The evaluator is correct when every attached rule must independently pass
(a flag with a rollout rule and a tier rule needs both, not either), when
the rollout boundary is computed from a stable user attribute rather than
a fresh random draw, and when an empty rule set or an unrecognized rule
type both fail closed rather than defaulting to enabled. Carry this
forward: when a decision is naturally two-layered — dispatch on a type,
then evaluate a compound condition within that type — nest a tagless
switch inside an expression switch's case rather than flattening both
layers into one giant boolean expression that's hard to extend when a
third rule type shows up.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the expression switch and the tagless (expressionless) form.
- [slices package](https://pkg.go.dev/slices) — `slices.Contains` for list-membership checks.
- [LaunchDarkly: Targeting users](https://docs.launchdarkly.com/home/flags/targeting) — percentage rollouts and attribute-based targeting in a real flagging product.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-cron-schedule-expression-matcher.md](28-cron-schedule-expression-matcher.md) | Next: [30-transaction-isolation-level-selector.md](30-transaction-isolation-level-selector.md)
