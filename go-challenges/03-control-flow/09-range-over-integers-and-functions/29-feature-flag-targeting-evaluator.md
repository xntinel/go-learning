# Exercise 29: Feature Flag Targeting Evaluator — Conditional Dispatch by User/Org/Cohort Rules

**Nivel: Intermedio** — validacion rapida (un test corto).

Rolling a feature out to "50% of enterprise accounts, plus anyone in the
internal beta cohort" without an external config service means the targeting
logic has to live in the request path itself, and it has to be exact: a
user who is admitted on one request and rejected on the next because the
percentage bucketing is not stable produces a UI that flickers between two
code paths mid-session. Modeling each targeting condition as a `Rule func(Request)
bool` and composing rules with `All`/`Any` turns a canary rollout's targeting
spec into ordinary function composition instead of a bespoke rule-tree
interpreter. This exercise is an independent module with its own `go mod
init`.

## What you'll build

```text
flag/                      independent module: example.com/feature-flag-targeting-evaluator
  go.mod                    module example.com/feature-flag-targeting-evaluator
  flag.go                   Request, Rule, ByUserPercentage, ByOrgTier, ByCohort, All, Any, Evaluate
  cmd/
    demo/
      main.go               runnable demo: 6 requests, beta cohort OR 50% of enterprise
  flag_test.go               percentage stability, tier/cohort rules, All/Any composition, order, early-stop
```

Implement: `ByUserPercentage(pct int) Rule`, `ByOrgTier(tiers ...string) Rule`, `ByCohort(cohorts ...string) Rule`, `All(rules ...Rule) Rule`, `Any(rules ...Rule) Rule`, and `Evaluate(requests iter.Seq[Request], rule Rule) iter.Seq[Request]` filtering to admitted requests.
Test: a user's percentage bucket is stable across repeated calls and correctly bounded; `ByOrgTier`/`ByCohort` match exactly the given sets; `All` requires every sub-rule while `Any` requires only one; `Evaluate` preserves input order and filters correctly; a consumer break stops the source early.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

A `Rule` is nothing more than `func(Request) bool`, and that is the point:
`All` and `Any` do not need to know anything about percentages, org tiers,
or cohorts, because they only ever call the `Rule`s they are given and
combine boolean results. The interesting correctness property lives inside
`ByUserPercentage`: hashing `UserID` into a bucket `0..99` and comparing it
against `pct` gives the *same* answer for the same user on every call,
which is what makes a canary rollout stable instead of flickering --
without a stable bucket, a user with a session split across two backend
instances (or two requests a few seconds apart) could see the new feature
on one request and the old one on the next, which is a worse experience
than either version consistently. `Evaluate` itself makes no attempt to
explain *why* a request was rejected; it either yields the request or it
does not, matching how a real feature-flag SDK is consumed at the call
site -- `if evaluated { useNewPath() } else { useOldPath() }`, with no
third state to report.

Create `flag.go`:

```go
package flag

import (
	"hash/fnv"
	"iter"
)

// Request is one inbound request's targeting-relevant attributes.
type Request struct {
	UserID  string
	OrgTier string
	Cohort  string
}

// Rule is a targeting predicate: it reports whether a single Request should
// be admitted. Representing rules as plain functions instead of a data
// structure means rules compose with ordinary function composition (All,
// Any) rather than needing an interpreter for a rule-tree data type, and a
// caller can write an ad hoc Rule inline for a one-off condition without
// touching this package at all.
type Rule func(Request) bool

// ByUserPercentage returns a Rule that admits a stable pct% slice of users,
// selected by hashing UserID into a bucket 0..99. The same UserID always
// hashes to the same bucket, so a user who is in a 10% rollout stays in it
// on every subsequent request instead of flapping in and out -- essential
// for a canary or A/B test, where inconsistent bucketing would let the same
// user see both the old and new behavior across requests.
func ByUserPercentage(pct int) Rule {
	return func(r Request) bool {
		h := fnv.New32a()
		h.Write([]byte(r.UserID))
		return int(h.Sum32()%100) < pct
	}
}

// ByOrgTier returns a Rule that admits requests whose OrgTier is one of tiers.
func ByOrgTier(tiers ...string) Rule {
	set := make(map[string]bool, len(tiers))
	for _, t := range tiers {
		set[t] = true
	}
	return func(r Request) bool { return set[r.OrgTier] }
}

// ByCohort returns a Rule that admits requests whose Cohort is one of cohorts.
func ByCohort(cohorts ...string) Rule {
	set := make(map[string]bool, len(cohorts))
	for _, c := range cohorts {
		set[c] = true
	}
	return func(r Request) bool { return set[r.Cohort] }
}

// All returns a Rule admitting a Request only when every one of rules
// admits it -- a conjunction, useful for narrowing a percentage rollout to
// a specific org tier.
func All(rules ...Rule) Rule {
	return func(r Request) bool {
		for _, rule := range rules {
			if !rule(r) {
				return false
			}
		}
		return true
	}
}

// Any returns a Rule admitting a Request when at least one of rules admits
// it -- a disjunction, useful for combining an internal-dogfooding cohort
// with a percentage rollout so both groups see the feature.
func Any(rules ...Rule) Rule {
	return func(r Request) bool {
		for _, rule := range rules {
			if rule(r) {
				return true
			}
		}
		return false
	}
}

// Evaluate filters requests down to only those admitted by rule, preserving
// order. It never mutates a Request or the rule's decision -- the boolean
// outcome of rule is the sole gate on whether a request is yielded at all,
// unlike Broadcast-style combinators that yield every input annotated with
// an outcome; a rejected request here produces no observable event, which
// matches how feature-flag SDKs are actually consumed: a caller checks "is
// this on for me" and either takes the new code path or the old one, with
// no third state to report.
func Evaluate(requests iter.Seq[Request], rule Rule) iter.Seq[Request] {
	return func(yield func(Request) bool) {
		for r := range requests {
			if rule(r) {
				if !yield(r) {
					return
				}
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-flag-targeting-evaluator"
)

func main() {
	requests := []flag.Request{
		{UserID: "user-1", OrgTier: "free", Cohort: "general"},
		{UserID: "user-2", OrgTier: "enterprise", Cohort: "general"},
		{UserID: "user-3", OrgTier: "free", Cohort: "beta-testers"},
		{UserID: "user-4", OrgTier: "free", Cohort: "general"},
		{UserID: "user-5", OrgTier: "enterprise", Cohort: "general"},
		{UserID: "user-6", OrgTier: "free", Cohort: "general"},
	}
	src := func(yield func(flag.Request) bool) {
		for _, r := range requests {
			if !yield(r) {
				return
			}
		}
	}

	rule := flag.Any(
		flag.ByCohort("beta-testers"),
		flag.All(flag.ByOrgTier("enterprise"), flag.ByUserPercentage(50)),
	)

	for r := range flag.Evaluate(src, rule) {
		fmt.Printf("admitted: user=%-7s tier=%-10s cohort=%s\n", r.UserID, r.OrgTier, r.Cohort)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
admitted: user=user-3  tier=free       cohort=beta-testers
admitted: user=user-5  tier=enterprise cohort=general
```

`user-3` is admitted purely through the beta cohort, ignoring its `free`
tier entirely. `user-2` and `user-5` are both `enterprise`, but only
`user-5` hashes into the bottom 50% bucket -- `user-2` is rejected even
though it satisfies the org-tier half of the `All` rule, because a
conjunction needs every condition to hold.

### Tests

Create `flag_test.go`:

```go
package flag

import (
	"iter"
	"testing"
)

func reqSeq(reqs []Request) iter.Seq[Request] {
	return func(yield func(Request) bool) {
		for _, r := range reqs {
			if !yield(r) {
				return
			}
		}
	}
}

func TestByUserPercentageIsStableAndBounded(t *testing.T) {
	t.Parallel()

	// Buckets for these IDs under FNV-1a % 100 are fixed: user-1=0,
	// user-5=24, user-3=38, user-4=43, user-2=57, user-6=81.
	rule := ByUserPercentage(50)
	cases := []struct {
		userID string
		want   bool
	}{
		{"user-1", true},
		{"user-5", true},
		{"user-3", true},
		{"user-4", true},
		{"user-2", false},
		{"user-6", false},
	}
	for _, tc := range cases {
		got := rule(Request{UserID: tc.userID})
		if got != tc.want {
			t.Errorf("ByUserPercentage(50)(%q) = %v, want %v", tc.userID, got, tc.want)
		}
		// Stability: calling twice must agree.
		if again := rule(Request{UserID: tc.userID}); again != got {
			t.Errorf("ByUserPercentage(50)(%q) is not stable across calls: %v then %v", tc.userID, got, again)
		}
	}
}

func TestByOrgTierAndByCohort(t *testing.T) {
	t.Parallel()

	tier := ByOrgTier("enterprise", "pro")
	if !tier(Request{OrgTier: "enterprise"}) {
		t.Error("expected enterprise to match")
	}
	if tier(Request{OrgTier: "free"}) {
		t.Error("expected free not to match")
	}

	cohort := ByCohort("beta-testers")
	if !cohort(Request{Cohort: "beta-testers"}) {
		t.Error("expected beta-testers to match")
	}
	if cohort(Request{Cohort: "general"}) {
		t.Error("expected general not to match")
	}
}

func TestAllRequiresEveryRule(t *testing.T) {
	t.Parallel()

	rule := All(ByOrgTier("enterprise"), ByUserPercentage(50))
	// user-5 buckets at 24 (< 50) and is enterprise: admitted.
	if !rule(Request{UserID: "user-5", OrgTier: "enterprise"}) {
		t.Error("expected enterprise + in-bucket user to be admitted")
	}
	// user-2 buckets at 57 (>= 50): rejected even though enterprise.
	if rule(Request{UserID: "user-2", OrgTier: "enterprise"}) {
		t.Error("expected out-of-bucket enterprise user to be rejected")
	}
	// user-5 is in-bucket but not enterprise: rejected.
	if rule(Request{UserID: "user-5", OrgTier: "free"}) {
		t.Error("expected non-enterprise in-bucket user to be rejected")
	}
}

func TestAnyAdmitsOnFirstMatch(t *testing.T) {
	t.Parallel()

	rule := Any(ByCohort("beta-testers"), ByOrgTier("enterprise"))
	if !rule(Request{Cohort: "beta-testers", OrgTier: "free"}) {
		t.Error("expected beta-testers cohort alone to admit")
	}
	if !rule(Request{Cohort: "general", OrgTier: "enterprise"}) {
		t.Error("expected enterprise tier alone to admit")
	}
	if rule(Request{Cohort: "general", OrgTier: "free"}) {
		t.Error("expected neither condition to reject")
	}
}

func TestEvaluatePreservesOrderAndFilters(t *testing.T) {
	t.Parallel()

	reqs := []Request{
		{UserID: "user-1", OrgTier: "free"},
		{UserID: "user-2", OrgTier: "enterprise"},
		{UserID: "user-3", OrgTier: "free"},
	}
	rule := ByOrgTier("enterprise")

	var got []string
	for r := range Evaluate(reqSeq(reqs), rule) {
		got = append(got, r.UserID)
	}
	want := []string{"user-2"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestEvaluateStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	calls := 0
	src := func(yield func(Request) bool) {
		for i := 0; i < 1000; i++ {
			calls++
			if !yield(Request{UserID: "u", OrgTier: "enterprise"}) {
				return
			}
		}
	}

	admitAll := ByOrgTier("enterprise")
	count := 0
	for range Evaluate(src, admitAll) {
		count++
		if count == 3 {
			break
		}
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3: the source must stop, not run to completion", calls)
	}
}
```

## Review

The design decision worth defending is keeping `Rule` a bare function type
instead of an interface with a `Match(Request) bool` method and a registry
of implementations. A function type composes for free with `All` and `Any`
and lets a caller build a one-off rule with a two-line closure; an
interface-based rule registry would need reflection or a type switch the
moment `All`/`Any` needed to combine rules of different concrete types. The
mistake to avoid when extending this evaluator is computing the percentage
bucket from something that is not stable per user -- hashing a per-request
timestamp or request ID instead of `UserID` produces a rollout that
re-randomizes on every single request, which defeats the entire purpose of
a percentage rollout: consistent membership over time.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Martin Fowler: feature toggles](https://martinfowler.com/articles/feature-toggles.html)
- [`hash/fnv` package documentation](https://pkg.go.dev/hash/fnv)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-cron-expression-iterator.md](28-cron-expression-iterator.md) | Next: [30-gossip-protocol-peer-health-check.md](30-gossip-protocol-peer-health-check.md)
