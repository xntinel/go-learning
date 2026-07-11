# Exercise 29: Evaluate Nested Feature Flag Rules (AND/OR/NOT)

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag's targeting rule is a small boolean expression tree: AND,
OR, and NOT combining simple attribute checks like "plan == pro" or
"country == US". Evaluating it against a user's context is a one-line
recurrence per combinator — AND stops at the first false, OR stops at the
first true, NOT flips its child — which makes the naive implementation
almost too easy to write. The part that is easy to skip is that real rule
configurations reuse the same condition in more than one place: a
"pro-in-the-US OR pro-outside-the-US" rule checks `plan == pro` twice,
because whoever wrote it composed it out of the same building block
without thinking about duplication. Evaluated once per occurrence, that
is wasted work that scales with how the rule was written, not with how
much distinct information it contains.

This module is fully self-contained: its own `go mod init`, the rule types
inline, its own demo and tests.

## What you'll build

```text
flagrules/                    independent module: example.com/flagrules
  go.mod                         go 1.24
  flagrules.go                    type Rule (And/Or/Not/AttrEquals); type Evaluator (memoized, recursive)
  flagrules_test.go               leaf rule, combinators table, evaluator matches plain Eval, memo hit, per-user isolation
  cmd/
    demo/
      main.go                     a rule that reuses two conditions across branches, evaluated once with hit/miss counts printed
```

- Files: `flagrules.go`, `cmd/demo/main.go`, `flagrules_test.go`.
- Implement: `Rule` interface (`Eval`, unexported `key`) with `AttrEquals`, `And`, `Or`, and `Not` implementations, plus `Evaluator` with `(*Evaluator) Eval(rule Rule, userID string, ctx Context) bool` that recurses through subrules via itself, memoizing on `userID + rule.key()`.
- Test: a leaf condition; a table of AND/OR/NOT combinations; `Evaluator.Eval` matching plain recursive `Rule.Eval`; a rule that reuses two conditions across branches producing at least one memo hit; two different users never sharing a cached result.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flagrules/cmd/demo
cd ~/go-exercises/flagrules
go mod init example.com/flagrules
go mod edit -go=1.24
```

### Memoizing by content, keyed per user, recursing through the cache itself

Each `Rule` implementation carries two recursive functions, not one:
`Eval` computes the boolean answer, and `key` computes a stable,
content-based signature — `"and(eq(plan=\"pro\"),eq(country=\"US\"))"` and
so on, built by recursing into each subrule's own `key()`. That signature,
not the rule's memory address or its position in the tree, is what
`Evaluator` uses to decide whether it has already evaluated this exact
condition for this exact user: two structurally identical `AttrEquals{...}`
values built in two different places in the rule tree produce the same
key, so they are recognized as the same question.

The recursive step that actually makes this a *memoizing* evaluator, and
not just a rule tree with an unused cache attached, is `evalUncached`
calling `e.Eval` — not `e.evalUncached`, and not the plain `Rule.Eval` —
on every subrule. That routes each nested condition back through the
cache check first. Get this one call site wrong (call `evalUncached`
directly, or call the subrule's own `Eval` method) and the top-level rule
is still memoized correctly, but every subrule underneath it is
recomputed on every occurrence — the code compiles, the simple tests pass
identically either way, and only a test that specifically counts cache
hits across a rule with a genuinely shared subrule catches it.

Create `flagrules.go`:

```go
// Package flagrules evaluates nested feature-flag targeting rules -- AND,
// OR, and NOT combinators over simple attribute conditions -- recursively
// against a user's context. Real flag configurations reuse the same
// condition (e.g. "plan == pro") across many branches of the same rule, or
// across several different flags evaluated for the same request, so a
// memoizing Evaluator caches each subrule's result per user and reuses it
// instead of recomputing an identical condition twice.
package flagrules

import (
	"fmt"
	"strings"
)

// Context is a user's attributes, looked up by feature-flag rules.
type Context map[string]string

// Rule is a boolean feature-flag targeting rule. key returns a stable,
// content-based signature used to memoize evaluation results; it is
// unexported because only the combinators in this package need to compute
// it recursively.
type Rule interface {
	Eval(ctx Context) bool
	key() string
}

// AttrEquals is a leaf rule: true when ctx[Key] == Value.
type AttrEquals struct {
	Key   string
	Value string
}

func (r AttrEquals) Eval(ctx Context) bool { return ctx[r.Key] == r.Value }
func (r AttrEquals) key() string           { return fmt.Sprintf("eq(%s=%q)", r.Key, r.Value) }

// And is true when every subrule is true.
type And struct{ Rules []Rule }

func (r And) Eval(ctx Context) bool {
	for _, sub := range r.Rules {
		if !sub.Eval(ctx) {
			return false
		}
	}
	return true
}

func (r And) key() string { return "and(" + joinKeys(r.Rules) + ")" }

// Or is true when at least one subrule is true.
type Or struct{ Rules []Rule }

func (r Or) Eval(ctx Context) bool {
	for _, sub := range r.Rules {
		if sub.Eval(ctx) {
			return true
		}
	}
	return false
}

func (r Or) key() string { return "or(" + joinKeys(r.Rules) + ")" }

// Not inverts its subrule.
type Not struct{ Rule Rule }

func (r Not) Eval(ctx Context) bool { return !r.Rule.Eval(ctx) }
func (r Not) key() string           { return "not(" + r.Rule.key() + ")" }

func joinKeys(rules []Rule) string {
	parts := make([]string, len(rules))
	for i, r := range rules {
		parts[i] = r.key()
	}
	return strings.Join(parts, ",")
}

// Evaluator evaluates Rule trees against a user's context, memoizing each
// subrule's result per user so a condition shared by several branches (or
// several flags) is computed once per user and reused thereafter.
type Evaluator struct {
	memo   map[string]bool
	Hits   int
	Misses int
}

// NewEvaluator returns an Evaluator with an empty memo.
func NewEvaluator() *Evaluator {
	return &Evaluator{memo: make(map[string]bool)}
}

// Eval evaluates rule for userID against ctx, checking the memo first and
// recursing through the rule tree -- via itself, so every subrule's result
// is independently cached -- on a miss.
func (e *Evaluator) Eval(rule Rule, userID string, ctx Context) bool {
	key := userID + "|" + rule.key()
	if v, ok := e.memo[key]; ok {
		e.Hits++
		return v
	}
	e.Misses++
	result := e.evalUncached(rule, userID, ctx)
	e.memo[key] = result
	return result
}

// evalUncached dispatches on rule's dynamic type and recurses into each
// subrule through e.Eval, so nested conditions are memoized too, not just
// the top-level rule.
func (e *Evaluator) evalUncached(rule Rule, userID string, ctx Context) bool {
	switch r := rule.(type) {
	case And:
		for _, sub := range r.Rules {
			if !e.Eval(sub, userID, ctx) {
				return false
			}
		}
		return true
	case Or:
		for _, sub := range r.Rules {
			if e.Eval(sub, userID, ctx) {
				return true
			}
		}
		return false
	case Not:
		return !e.Eval(r.Rule, userID, ctx)
	case AttrEquals:
		return r.Eval(ctx)
	default:
		return rule.Eval(ctx)
	}
}
```

### The runnable demo

The demo builds a rule where `isPro` and `isUS` each appear in more than
one branch, evaluates it once through a memoized `Evaluator`, and prints
the resulting hit/miss counts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flagrules"
)

func main() {
	isPro := flagrules.AttrEquals{Key: "plan", Value: "pro"}
	isUS := flagrules.AttrEquals{Key: "country", Value: "US"}

	// "pro users in the US, OR pro users anywhere outside the US" -- a
	// contrived rule, but a realistic shape: isPro appears in both
	// branches, and isUS is checked both directly and negated.
	rule := flagrules.Or{Rules: []flagrules.Rule{
		flagrules.And{Rules: []flagrules.Rule{isPro, isUS}},
		flagrules.And{Rules: []flagrules.Rule{isPro, flagrules.Not{Rule: isUS}}},
	}}

	ctx := flagrules.Context{"plan": "pro", "country": "CA"}
	eval := flagrules.NewEvaluator()

	enabled := eval.Eval(rule, "user-42", ctx)
	fmt.Printf("enabled=%v hits=%d misses=%d\n", enabled, eval.Hits, eval.Misses)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enabled=true hits=2 misses=6
```

### Tests

`TestAttrEqualsLeaf` and `TestAndOrNotCombinators` check the plain,
unmemoized `Rule.Eval` recursion against a table of combinator shapes.
`TestEvaluatorMatchesPlainEval` proves the memoized path never changes the
answer. `TestEvaluatorMemoizesSharedSubrule` is the test this exercise
exists for: the demo's rule, evaluated once, must produce at least one
memo hit, since `isPro` and `isUS` each recur across branches.
`TestEvaluatorCachesPerUser` guards the adjacent mistake of keying the
memo on the rule alone: two different users must never see each other's
cached result.

Create `flagrules_test.go`:

```go
package flagrules

import "testing"

func TestAttrEqualsLeaf(t *testing.T) {
	t.Parallel()

	ctx := Context{"plan": "pro"}
	if !(AttrEquals{Key: "plan", Value: "pro"}).Eval(ctx) {
		t.Fatal("expected true for matching attribute")
	}
	if (AttrEquals{Key: "plan", Value: "free"}).Eval(ctx) {
		t.Fatal("expected false for non-matching attribute")
	}
}

func TestAndOrNotCombinators(t *testing.T) {
	t.Parallel()

	ctx := Context{"plan": "pro", "country": "CA"}
	isPro := AttrEquals{Key: "plan", Value: "pro"}
	isUS := AttrEquals{Key: "country", Value: "US"}

	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"and-both-true", And{Rules: []Rule{isPro, Not{Rule: isUS}}}, true},
		{"and-one-false", And{Rules: []Rule{isPro, isUS}}, false},
		{"or-one-true", Or{Rules: []Rule{isUS, isPro}}, true},
		{"or-all-false", Or{Rules: []Rule{isUS, Not{Rule: isPro}}}, false},
		{"not-inverts", Not{Rule: isUS}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rule.Eval(ctx); got != tc.want {
				t.Fatalf("%s: Eval() = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestEvaluatorMatchesPlainEval(t *testing.T) {
	t.Parallel()

	ctx := Context{"plan": "pro", "country": "CA"}
	isPro := AttrEquals{Key: "plan", Value: "pro"}
	isUS := AttrEquals{Key: "country", Value: "US"}
	rule := Or{Rules: []Rule{
		And{Rules: []Rule{isPro, isUS}},
		And{Rules: []Rule{isPro, Not{Rule: isUS}}},
	}}

	want := rule.Eval(ctx)
	e := NewEvaluator()
	got := e.Eval(rule, "user-42", ctx)
	if got != want {
		t.Fatalf("Evaluator.Eval() = %v, want %v (matching plain Eval)", got, want)
	}
}

// TestEvaluatorMemoizesSharedSubrule is the test that justifies the whole
// exercise: isPro and isUS each appear in more than one branch of rule for
// the same user, and the second occurrence of each must be served from the
// memo.
func TestEvaluatorMemoizesSharedSubrule(t *testing.T) {
	t.Parallel()

	ctx := Context{"plan": "pro", "country": "CA"}
	isPro := AttrEquals{Key: "plan", Value: "pro"}
	isUS := AttrEquals{Key: "country", Value: "US"}
	rule := Or{Rules: []Rule{
		And{Rules: []Rule{isPro, isUS}},
		And{Rules: []Rule{isPro, Not{Rule: isUS}}},
	}}

	e := NewEvaluator()
	got := e.Eval(rule, "user-42", ctx)
	if !got {
		t.Fatalf("Eval() = %v, want true", got)
	}
	if e.Hits == 0 {
		t.Fatal("Hits = 0, want at least 1 (isPro and isUS each recur across branches)")
	}
}

// TestEvaluatorCachesPerUser proves the memo is keyed per user: the same
// rule evaluated for two different users with different attributes must
// not leak one user's cached result to the other.
func TestEvaluatorCachesPerUser(t *testing.T) {
	t.Parallel()

	isPro := AttrEquals{Key: "plan", Value: "pro"}
	e := NewEvaluator()

	proUser := e.Eval(isPro, "user-a", Context{"plan": "pro"})
	freeUser := e.Eval(isPro, "user-b", Context{"plan": "free"})
	if !proUser {
		t.Error("user-a: Eval() = false, want true")
	}
	if freeUser {
		t.Error("user-b: Eval() = true, want false (must not reuse user-a's cached result)")
	}
}
```

## Review

`Evaluator.Eval` is correct when it always agrees with the plain,
unmemoized `Rule.Eval` recursion, and when a condition repeated across
branches of the same rule (or across rules sharing a subrule) is charged
to the memo exactly once per user. `TestEvaluatorMemoizesSharedSubrule` is
the test that would fail — with `Hits` stuck at zero, though every answer
would still be correct — on the tempting mistake this exercise targets:
routing `evalUncached`'s recursive calls through a subrule's own `Eval`
(or through `evalUncached` directly) instead of back through `e.Eval`.
That mistake is invisible to every test that only checks the final
boolean, because the answer does not change; it only shows up once
something is actually counting how much redundant work got skipped.
`TestEvaluatorCachesPerUser` guards the second most tempting shortcut,
keying the memo on the rule's signature alone and forgetting the user is
part of the question being cached.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [LaunchDarkly: Targeting users with flags (rule evaluation model)](https://docs.launchdarkly.com/home/flags/targeting-rules)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-merkle-tree-verification-memoized.md](28-merkle-tree-verification-memoized.md) | Next: [30-hateoas-link-traversal-bounded.md](30-hateoas-link-traversal-bounded.md)
