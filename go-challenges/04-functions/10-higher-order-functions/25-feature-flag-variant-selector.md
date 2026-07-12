# Exercise 25: Feature Flag Evaluator with Rule Chain and Fallback

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag is rarely "on or off" in production — it is a chain of
rules, each answering "does this apply to this request?", with a safe
default when none of them do. `Chain` builds that evaluator out of an
ordered list of `Rule` values and a fallback variant, so adding a new
targeting rule never touches the rules already in place.

## What you'll build

```text
flagrules/                   independent module: example.com/flagrules
  go.mod                     go 1.24
  flagrules.go                type Context, Rule, Evaluator; func Chain
  flagrules_test.go           first-match wins, fallthrough, fallback, empty chain
  cmd/demo/
    main.go                  evaluates three users against a two-rule chain
```

- Files: `flagrules.go`, `flagrules_test.go`, `cmd/demo/main.go`.
- Implement: `Context map[string]any`, `Rule func(ctx Context) (variant string, matched bool)`, `Evaluator func(ctx Context) string`, and `Chain(fallback string, rules ...Rule) Evaluator`.
- Test: the first matching rule wins and later rules are never called; a declining rule falls through to the next one; the fallback is returned when no rule matches; an empty rule chain always returns the fallback.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A strategy that can decline, not just answer

Each `Rule` is a strategy the chain injects into a fixed algorithm — but
unlike a simple callback that must always produce an answer, a `Rule` can
explicitly decline by returning `(_, false)`. That third state is what
lets rules compose: a rule for beta users does not need to know anything
about internal-staff overrides, because declining just passes the
decision to whatever rule comes next. `Chain` itself stays a thin loop —
try each rule, return on the first match, fall back otherwise — which
means the actual targeting logic lives entirely in the injected `Rule`
values, not in the chain that runs them.

Ordering matters and is the caller's responsibility: `Chain` tries rules
in the exact order given, so a narrower rule (an explicit staff email)
placed before a broader one (a plan tier) wins for users who satisfy
both, while the same two rules in the opposite order would let the
broader rule shadow the narrower one entirely.

Create `flagrules.go`:

```go
package flagrules

// Context carries the attributes a Rule inspects to decide whether it
// applies — user ID, environment, plan tier, whatever the caller wants to
// key variant selection on.
type Context map[string]any

// Rule inspects ctx and either claims it (returning the variant name and
// true) or declines (returning "", false), letting the next rule in the
// chain have a turn.
type Rule func(ctx Context) (variant string, matched bool)

// Evaluator resolves a Context to a single variant name.
type Evaluator func(ctx Context) string

// Chain builds an Evaluator from an ordered list of rules and a fallback.
// Rules are tried in order; the first one that matches wins. If no rule
// matches — including when rules is empty — fallback is returned. This is
// the same shape as a middleware chain, but each link either fully
// resolves the request or explicitly declines instead of always calling
// the next link.
func Chain(fallback string, rules ...Rule) Evaluator {
	return func(ctx Context) string {
		for _, rule := range rules {
			if variant, ok := rule(ctx); ok {
				return variant
			}
		}
		return fallback
	}
}
```

### The runnable demo

The demo builds a two-rule chain — beta plan, then a hardcoded internal
staff email — and evaluates it against three different users to show
first-match, fallthrough, and fallback all in one run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flagrules"
)

func main() {
	betaUsers := flagrules.Rule(func(ctx flagrules.Context) (string, bool) {
		if ctx["plan"] == "beta" {
			return "new-checkout", true
		}
		return "", false
	})

	internalStaff := flagrules.Rule(func(ctx flagrules.Context) (string, bool) {
		if ctx["email"] == "staff@example.com" {
			return "internal-preview", true
		}
		return "", false
	})

	evaluate := flagrules.Chain("legacy-checkout", betaUsers, internalStaff)

	users := []flagrules.Context{
		{"plan": "beta", "email": "a@example.com"},
		{"plan": "free", "email": "staff@example.com"},
		{"plan": "free", "email": "b@example.com"},
	}

	for _, ctx := range users {
		fmt.Printf("email=%v plan=%v -> variant=%s\n", ctx["email"], ctx["plan"], evaluate(ctx))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email=a@example.com plan=beta -> variant=new-checkout
email=staff@example.com plan=free -> variant=internal-preview
email=b@example.com plan=free -> variant=legacy-checkout
```

The first user matches the beta rule immediately. The second is not on
the beta plan, so the chain falls through to the staff-email rule, which
matches. The third matches neither rule and gets the fallback.

### Tests

`TestChainFirstRuleMatches` proves short-circuiting: when the first rule
matches, the second is never invoked at all, which matters once rules
carry side effects like metrics or logging. `TestChainFallsThroughToSecondRule`
and `TestChainUsesFallbackWhenNoRuleMatches` cover the two ways a rule can
decline — passing to the next rule, or exhausting the whole chain.
`TestChainWithNoRulesUsesFallback` pins down the degenerate case: a chain
built with zero rules is a valid, if boring, evaluator that always returns
the fallback.

Create `flagrules_test.go`:

```go
package flagrules

import "testing"

func TestChainFirstRuleMatches(t *testing.T) {
	t.Parallel()

	second := 0
	first := func(ctx Context) (string, bool) { return "variant-a", true }
	rest := func(ctx Context) (string, bool) { second++; return "variant-b", true }

	evaluate := Chain("fallback", first, rest)
	if got := evaluate(Context{}); got != "variant-a" {
		t.Fatalf("evaluate() = %q, want %q", got, "variant-a")
	}
	if second != 0 {
		t.Fatalf("second rule was called %d times, want 0", second)
	}
}

func TestChainFallsThroughToSecondRule(t *testing.T) {
	t.Parallel()

	declines := func(ctx Context) (string, bool) { return "", false }
	matches := func(ctx Context) (string, bool) { return "variant-b", true }

	evaluate := Chain("fallback", declines, matches)
	if got := evaluate(Context{}); got != "variant-b" {
		t.Fatalf("evaluate() = %q, want %q", got, "variant-b")
	}
}

func TestChainUsesFallbackWhenNoRuleMatches(t *testing.T) {
	t.Parallel()

	declines := func(ctx Context) (string, bool) { return "", false }
	evaluate := Chain("fallback", declines, declines)
	if got := evaluate(Context{}); got != "fallback" {
		t.Fatalf("evaluate() = %q, want %q", got, "fallback")
	}
}

func TestChainWithNoRulesUsesFallback(t *testing.T) {
	t.Parallel()

	evaluate := Chain("fallback")
	if got := evaluate(Context{"anything": true}); got != "fallback" {
		t.Fatalf("evaluate() = %q, want %q", got, "fallback")
	}
}
```

## Review

`Chain` is correct because it treats "no match" as a first-class,
distinguishable outcome instead of forcing every rule to return some
variant — that three-valued contract (`match`, `decline`, and the
chain's own `fallback`) is what lets rules be added, removed, and
reordered independently. The short-circuit test is the one that actually
matters in production: a chain that evaluated every rule before picking
the first match would silently run expensive or side-effecting rules
that should never fire. Keep the fallback explicit at the call site
rather than baking a default into a rule — a `Rule` that says "match
everything" is indistinguishable from a badly ordered chain and hides the
real default from the reader.

## Resources

- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `Rule`/`Evaluator` decorator and strategy shapes.
- [maps package](https://pkg.go.dev/maps) — utilities for working with `Context` when rules need to inspect or merge attributes.
- [LaunchDarkly: Targeting rules](https://docs.launchdarkly.com/home/flags/targeting-rules) — a production feature-flag system with the same ordered-rule-chain-plus-fallback model.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-distributed-lock-acquire-with-retry.md](24-distributed-lock-acquire-with-retry.md) | Next: [26-request-dedup-time-window-singleflight.md](26-request-dedup-time-window-singleflight.md)
