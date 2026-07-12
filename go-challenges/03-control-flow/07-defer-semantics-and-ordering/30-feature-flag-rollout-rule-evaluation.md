# Exercise 30: Feature Flags — Deferred Rule Context Cleanup Prevents Context Leak

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag SDK evaluates targeting rules in order — an explicit
cohort allow-list first, then a stable percentage rollout as the
fallback — and most real ones expose *why* a flag came out the way it
did, for debugging a support ticket where one customer insists a flag
is on and another insists it is off. That diagnostic trail is exactly
the kind of per-call state that must never leak: if evaluating user A
left trace entries behind that evaluating user B's flag then appended
to, an operator debugging B's ticket would see A's rule matches mixed
into the explanation. This module builds `Flag.Evaluate` so its
diagnostic trace is always exactly this call's trace, using a deferred
closure to make that true on every exit path, including the short-circuit
one where the first rule already decides the outcome. The module is
fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
flags/                       independent module: example.com/feature-flag-rollout-rule-evaluation
  go.mod                      go 1.24
  flags.go                     EvalContext, Rule, Flag (Evaluate, LastTrace), PercentageRule, CohortRule
  cmd/
    demo/
      main.go                 runnable demo: cohort + percentage rollout evaluated for 4 users
  flags_test.go                trace-isolation case; percentage stability case; cohort-bypass case
```

- Files: `flags.go`, `cmd/demo/main.go`, `flags_test.go`.
- Implement: `Flag` (`Evaluate`, `LastTrace`), `PercentageRule(percent int) Rule`, `CohortRule(members map[string]bool, enabled bool) Rule`.
- Test: a case proving the trace never carries over between calls, a case proving percentage-rule stability per user, and a case proving cohort membership bypasses the percentage rule.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/30-feature-flag-rollout-rule-evaluation/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/30-feature-flag-rollout-rule-evaluation
go mod edit -go=1.24
```

### Why the trace copy has to be deferred, not done at the bottom of the loop

`Evaluate` can return from three different places: inside the loop, the
moment any rule reports a match, or after the loop, if no rule ever
matches. A plain assignment `f.lastTrace = ctx.Trace` written only after
the loop — the "normal completion" spot — would simply never execute on
the far more common path where an early rule (the cohort allow-list, in
the demo below) already decides the outcome and the function returns
from inside the loop instead. `f.lastTrace` would then still hold
whatever a *previous* call's full trace was, silently mixing two
unrelated evaluations' diagnostics together. Registering the copy as a
deferred closure, at the top of `Evaluate`, sidesteps the problem
entirely: a deferred call runs at the function's true end regardless of
which `return` statement got it there, so `ctx.Trace` — built up to
however many rules actually ran, however few — is always what ends up in
`f.lastTrace`.

Create `flags.go`:

```go
package flags

import (
	"fmt"
	"hash/fnv"
)

// EvalContext carries diagnostic state for a single flag evaluation: which
// rules were checked and why the decision came out the way it did. It is
// meant to be discarded at the end of one Evaluate call -- if it leaked
// into the next call, an unrelated evaluation would inherit stale trace
// entries from someone else's request.
type EvalContext struct {
	Trace []string
}

// Rule inspects userID and reports whether it matched this rule and, if
// so, the decision that match implies.
type Rule func(ctx *EvalContext, userID string) (matched bool, enabled bool)

// Flag evaluates a list of targeting rules, in order, for a given user.
type Flag struct {
	Name  string
	Rules []Rule

	// lastTrace holds the most recent evaluation's trace -- overwritten,
	// not accumulated, on every call.
	lastTrace []string
}

// Evaluate runs each rule in order against userID and returns the first
// matching rule's decision, or false if no rule matches. A fresh
// EvalContext is created for this call only; a deferred closure copies its
// trace into lastTrace, which is what guarantees a caller reading
// LastTrace afterward always sees this call's trace -- never a previous
// call's -- regardless of which rule (if any) short-circuited the loop.
// Without the defer running on every exit path, a caller who reads
// LastTrace after a rule matches early would see whatever the *previous*
// evaluation left behind instead of this one's.
func (f *Flag) Evaluate(userID string) (enabled bool) {
	ctx := &EvalContext{}
	defer func() {
		f.lastTrace = ctx.Trace
	}()

	for i, rule := range f.Rules {
		matched, dec := rule(ctx, userID)
		ctx.Trace = append(ctx.Trace, fmt.Sprintf("rule[%d] matched=%v decision=%v", i, matched, dec))
		if matched {
			return dec
		}
	}
	return false
}

// LastTrace returns a copy of the most recent Evaluate call's trace.
func (f *Flag) LastTrace() []string {
	out := make([]string, len(f.lastTrace))
	copy(out, f.lastTrace)
	return out
}

// PercentageRule matches every user unconditionally and enables the flag
// for a stable, hash-derived percent of them -- the same userID always
// hashes to the same bucket, so a given user's outcome does not flip
// between calls.
func PercentageRule(percent int) Rule {
	return func(_ *EvalContext, userID string) (matched, enabled bool) {
		h := fnv.New32a()
		h.Write([]byte(userID))
		bucket := int(h.Sum32() % 100)
		return true, bucket < percent
	}
}

// CohortRule matches only if userID is in members, deciding enabled
// unconditionally for members and deferring to later rules otherwise.
func CohortRule(members map[string]bool, enabled bool) Rule {
	return func(_ *EvalContext, userID string) (matched, dec bool) {
		if members[userID] {
			return true, enabled
		}
		return false, false
	}
}
```

### The runnable demo

A cohort of beta testers is always enabled; everyone else falls through
to a 30% rollout. `user-42` is in the cohort and short-circuits after
one rule; the other three fall through to the percentage rule, each
landing in a different, but stable, hash bucket.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-flag-rollout-rule-evaluation"
)

func main() {
	betaTesters := map[string]bool{"user-42": true}

	flag := &flags.Flag{
		Name: "new-checkout-flow",
		Rules: []flags.Rule{
			flags.CohortRule(betaTesters, true),
			flags.PercentageRule(30),
		},
	}

	for _, user := range []string{"user-42", "user-100", "user-7", "user-200"} {
		enabled := flag.Evaluate(user)
		fmt.Printf("user=%s enabled=%v\n", user, enabled)
		fmt.Printf("  trace: %v\n", flag.LastTrace())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
user=user-42 enabled=true
  trace: [rule[0] matched=true decision=true]
user=user-100 enabled=true
  trace: [rule[0] matched=false decision=false rule[1] matched=true decision=true]
user=user-7 enabled=false
  trace: [rule[0] matched=false decision=false rule[1] matched=true decision=false]
user=user-200 enabled=false
  trace: [rule[0] matched=false decision=false rule[1] matched=true decision=false]
```

`user-42`'s trace has exactly one entry — the cohort rule short-circuited
the loop — while every other user's trace has two, and none of them ever
carries an entry left over from the user evaluated before it.

### Tests

`TestEvaluateTraceDoesNotLeakBetweenCalls` is the core property: a call
that falls through both rules leaves a two-entry trace, and the very
next call, which short-circuits after the first rule, must show exactly
one entry — never three. `TestPercentageRuleIsStableForSameUser` checks
the same user hashes into the same bucket across repeated calls.
`TestCohortRuleBypassesPercentageRule` checks cohort membership decides
the outcome unconditionally, without ever consulting the percentage
rule behind it.

Create `flags_test.go`:

```go
package flags

import "testing"

func TestEvaluateTraceDoesNotLeakBetweenCalls(t *testing.T) {
	f := &Flag{Rules: []Rule{
		CohortRule(map[string]bool{"vip": true}, true),
		PercentageRule(100), // always matches, always enabled
	}}

	f.Evaluate("someone-else") // cohort doesn't match: falls through, 2 trace entries
	if got := len(f.LastTrace()); got != 2 {
		t.Fatalf("trace after non-cohort call = %d entries, want 2", got)
	}

	f.Evaluate("vip") // cohort matches immediately: short-circuits after rule 0
	if got := len(f.LastTrace()); got != 1 {
		t.Fatalf("trace after cohort-match call = %d entries, want 1 (must not carry over the previous call's 2 entries)", got)
	}
}

func TestPercentageRuleIsStableForSameUser(t *testing.T) {
	f := &Flag{Rules: []Rule{PercentageRule(50)}}
	first := f.Evaluate("user-abc")
	for i := 0; i < 5; i++ {
		if got := f.Evaluate("user-abc"); got != first {
			t.Fatalf("call %d = %v, want %v (stable across calls)", i, got, first)
		}
	}
}

func TestCohortRuleBypassesPercentageRule(t *testing.T) {
	f := &Flag{Rules: []Rule{
		CohortRule(map[string]bool{"vip": true}, true),
		PercentageRule(0), // would deny everyone if reached
	}}

	if got := f.Evaluate("vip"); !got {
		t.Fatalf("Evaluate(vip) = %v, want true (cohort membership overrides percentage rule)", got)
	}
	if got := f.Evaluate("not-vip"); got {
		t.Fatalf("Evaluate(not-vip) = %v, want false (falls through to a 0%% rollout)", got)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Evaluate` is correct when `LastTrace` always reflects exactly the rules
consulted during the *most recent* call — no more (leaked entries from
an earlier call) and no fewer (a partial trace that stopped updating
early). The deferred closure is what makes this true regardless of
which rule, if any, ends the loop early: it copies `ctx.Trace` at the
function's real end, whichever `return` reached it. The mistake this
design avoids is assigning `f.lastTrace = ctx.Trace` as a plain statement
positioned after the loop — code that looks complete because it "runs at
the end of the function" in the author's mental model, but which a
short-circuiting `return dec` inside the loop skips over entirely,
leaving stale diagnostic data in place for exactly the evaluations that
matter enough to have short-circuited.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a deferred call runs at function return regardless of which return statement was taken.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the non-cryptographic hash used here for a stable, deterministic percentage bucket per user ID.
- [LaunchDarkly: Targeting users with flags](https://docs.launchdarkly.com/home/flags/targeting) — production feature-flag targeting rules (individual, segment, percentage) this exercise's `Rule` chain models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-circuit-breaker-half-open-probe.md](29-circuit-breaker-half-open-probe.md) | Next: [31-mvcc-snapshot-isolation-transaction.md](31-mvcc-snapshot-isolation-transaction.md)
