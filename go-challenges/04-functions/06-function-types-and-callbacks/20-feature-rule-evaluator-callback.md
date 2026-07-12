# Exercise 20: Feature Flag Rule Evaluation with Composable Callbacks

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag rarely turns on for "everyone" or "no one" — it targets
enterprise accounts outside a restricted region, or a stable 50% rollout
bucket by user ID. Every one of those conditions is a `Rule` callback, and
`All`/`Any`/`Not` combine them the same way boolean operators combine plain
booleans, so a targeting policy is built, not hard-coded.

## What you'll build

```text
flags/                      independent module: example.com/feature-rule-evaluator-callback
  go.mod                     go 1.24
  flags.go                     type Context, type Rule, Equals, Percentage, All, Any, Not, func Evaluate
  cmd/
    demo/
      main.go                  runnable demo: an All/Not targeting rule, then a stable percentage rollout
  flags_test.go                table test: each combinator, both AND/OR identities, percentage determinism and bounds
```

Files: `flags.go`, `cmd/demo/main.go`, `flags_test.go`.
Implement: `type Context map[string]any`, `type Rule func(ctx Context) bool`, `Equals(key string, want any) Rule`, `Percentage(bucketKey func(Context) string, pct float64) Rule` hashed with `hash/fnv`, the combinators `All`, `Any`, `Not`, and `func Evaluate(ctx Context, rule Rule) bool`.
Test: `Equals` hit/miss/absent-key, `All` requiring every sub-rule (plus the empty-`All` AND identity), `Any` requiring one sub-rule (plus the empty-`Any` OR identity), `Not` inverting both ways, `Percentage` giving the same answer across repeated calls for the same bucket key, and the `0%`/`100%` boundary cases.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/20-feature-rule-evaluator-callback/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/20-feature-rule-evaluator-callback
go mod edit -go=1.24
```

### Why targeting rules are callbacks, and why the percentage bucket is a hash, not a coin flip

A feature flag's targeting policy — "enterprise plan AND not in a
restricted region," "beta users OR internal accounts," "the inverse of the
maintenance-mode flag" — is exactly a boolean expression over predicates,
and Go already has a value for "a predicate over some input": a function.
`Rule func(ctx Context) bool` lets `All`, `Any`, and `Not` be tiny, reusable
combinators that work over *any* rule, including ones built from other
combinators, instead of a policy engine trying to parse a DSL string.
`Percentage` is a rule with a subtler requirement: a 50% rollout must put
the *same* user in the *same* bucket on every request, or a user's
experience flips randomly between page loads. Calling `rand.Float64()` per
evaluation would violate that immediately. Hashing a stable identity key
(`bucketKey(ctx)`, typically the user or account ID) with `hash/fnv` gives a
number that is a pure function of that key — same input, same bucket,
forever, with no shared state and no clock involved.

Create `flags.go`:

```go
// Package flags evaluates feature-flag targeting rules built by composing
// small Rule callbacks with boolean combinators.
package flags

import "hash/fnv"

// Context is the request-scoped data a targeting rule evaluates against
// (user ID, plan tier, region, and so on).
type Context map[string]any

// Rule reports whether ctx matches a targeting condition.
type Rule func(ctx Context) bool

// Equals matches when ctx[key] equals want.
func Equals(key string, want any) Rule {
	return func(ctx Context) bool {
		return ctx[key] == want
	}
}

// Percentage matches a deterministic pct% of contexts, bucketed by
// hashing the string bucketKey extracts from ctx (typically a stable user
// or account ID, so the same entity always lands in the same bucket).
func Percentage(bucketKey func(ctx Context) string, pct float64) Rule {
	return func(ctx Context) bool {
		h := fnv.New32a()
		h.Write([]byte(bucketKey(ctx)))
		bucket := float64(h.Sum32() % 100)
		return bucket < pct
	}
}

// All combines rules with AND: it matches only if every rule matches.
// An empty All matches everything (the identity for AND).
func All(rules ...Rule) Rule {
	return func(ctx Context) bool {
		for _, r := range rules {
			if !r(ctx) {
				return false
			}
		}
		return true
	}
}

// Any combines rules with OR: it matches if at least one rule matches.
// An empty Any matches nothing (the identity for OR).
func Any(rules ...Rule) Rule {
	return func(ctx Context) bool {
		for _, r := range rules {
			if r(ctx) {
				return true
			}
		}
		return false
	}
}

// Not inverts a rule.
func Not(r Rule) Rule {
	return func(ctx Context) bool {
		return !r(ctx)
	}
}

// Evaluate runs rule against ctx. It exists mainly to give the top-level
// "should this feature be on for this request" call a name distinct from
// invoking a Rule value directly.
func Evaluate(ctx Context, rule Rule) bool {
	return rule(ctx)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-rule-evaluator-callback"
)

func main() {
	newCheckout := flags.All(
		flags.Equals("plan", "enterprise"),
		flags.Not(flags.Equals("region", "restricted")),
	)

	contexts := []flags.Context{
		{"plan": "enterprise", "region": "us"},
		{"plan": "enterprise", "region": "restricted"},
		{"plan": "free", "region": "us"},
	}

	for i, ctx := range contexts {
		fmt.Printf("context %d: enabled=%v\n", i, flags.Evaluate(ctx, newCheckout))
	}

	rollout := flags.Percentage(func(ctx flags.Context) string {
		return ctx["userID"].(string)
	}, 50)

	for _, user := range []string{"user-1", "user-2", "user-3"} {
		ctx := flags.Context{"userID": user}
		first := flags.Evaluate(ctx, rollout)
		second := flags.Evaluate(ctx, rollout)
		fmt.Printf("%s: enabled=%v stable=%v\n", user, first, first == second)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
context 0: enabled=true
context 1: enabled=false
context 2: enabled=false
user-1: enabled=true stable=true
user-2: enabled=false stable=true
user-3: enabled=true stable=true
```

### Tests

Create `flags_test.go`:

```go
package flags

import "testing"

func TestEqualsMatchesExactValue(t *testing.T) {
	t.Parallel()
	rule := Equals("plan", "pro")
	if !rule(Context{"plan": "pro"}) {
		t.Error("expected match on equal value")
	}
	if rule(Context{"plan": "free"}) {
		t.Error("expected no match on different value")
	}
	if rule(Context{}) {
		t.Error("expected no match when key is absent")
	}
}

func TestAllRequiresEveryRule(t *testing.T) {
	t.Parallel()
	rule := All(Equals("plan", "pro"), Equals("region", "us"))
	if !rule(Context{"plan": "pro", "region": "us"}) {
		t.Error("expected match when both conditions hold")
	}
	if rule(Context{"plan": "pro", "region": "eu"}) {
		t.Error("expected no match when one condition fails")
	}
}

func TestAllOfNoRulesMatchesEverything(t *testing.T) {
	t.Parallel()
	if !All()(Context{}) {
		t.Error("empty All should match everything (AND identity)")
	}
}

func TestAnyRequiresOneRule(t *testing.T) {
	t.Parallel()
	rule := Any(Equals("plan", "pro"), Equals("plan", "enterprise"))
	if !rule(Context{"plan": "enterprise"}) {
		t.Error("expected match on second alternative")
	}
	if rule(Context{"plan": "free"}) {
		t.Error("expected no match when neither alternative holds")
	}
}

func TestAnyOfNoRulesMatchesNothing(t *testing.T) {
	t.Parallel()
	if Any()(Context{}) {
		t.Error("empty Any should match nothing (OR identity)")
	}
}

func TestNotInvertsRule(t *testing.T) {
	t.Parallel()
	rule := Not(Equals("beta", true))
	if rule(Context{"beta": true}) {
		t.Error("expected Not to invert a matching rule to false")
	}
	if !rule(Context{"beta": false}) {
		t.Error("expected Not to invert a non-matching rule to true")
	}
}

func TestPercentageIsDeterministicForTheSameBucketKey(t *testing.T) {
	t.Parallel()
	rule := Percentage(func(ctx Context) string { return ctx["id"].(string) }, 50)
	ctx := Context{"id": "stable-user"}
	first := rule(ctx)
	for i := 0; i < 5; i++ {
		if rule(ctx) != first {
			t.Fatalf("Percentage rule is not deterministic across repeated calls")
		}
	}
}

func TestPercentageZeroAlwaysExcludes(t *testing.T) {
	t.Parallel()
	rule := Percentage(func(ctx Context) string { return ctx["id"].(string) }, 0)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if rule(Context{"id": id}) {
			t.Errorf("0%% rollout should exclude %q", id)
		}
	}
}

func TestPercentageHundredAlwaysIncludes(t *testing.T) {
	t.Parallel()
	rule := Percentage(func(ctx Context) string { return ctx["id"].(string) }, 100)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !rule(Context{"id": id}) {
			t.Errorf("100%% rollout should include %q", id)
		}
	}
}

func TestEvaluateCallsTheGivenRule(t *testing.T) {
	t.Parallel()
	called := false
	rule := Rule(func(ctx Context) bool {
		called = true
		return true
	})
	if !Evaluate(Context{}, rule) {
		t.Fatal("Evaluate should return the rule's result")
	}
	if !called {
		t.Fatal("Evaluate should invoke the rule")
	}
}
```

## Review

Every combinator loops over `Rule` values without knowing what any of them
check, which is why `All()` and `Any()` — the zero-argument cases — matter
as much as the populated ones: they pin the AND and OR identities (`All`
with nothing to fail on is vacuously true, `Any` with nothing to succeed on
is vacuously false), the same identities `all()`/`any()` builtins use in
every language that has them. `Percentage`'s determinism test is the one
that actually protects production: a rollout rule that isn't a pure function
of its bucket key would put the same user on different sides of the flag on
different requests, which is worse than the flag not existing. The `0`/`100`
boundary tests close off the two edges a hash-mod-100 comparison can get
backwards (`<` vs `<=` at either end).

## Resources

- [hash/fnv](https://pkg.go.dev/hash/fnv)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [LaunchDarkly: percentage rollouts](https://docs.launchdarkly.com/home/flags/rollouts)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-elasticsearch-query-builder-callback.md](19-elasticsearch-query-builder-callback.md) | Next: [21-media-type-codec-strategy.md](21-media-type-codec-strategy.md)
