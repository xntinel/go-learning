# Exercise 24: Feature Flag Evaluator with Pre-Compiled Rules Closure

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature-flagging system evaluates a rule set — segment, region, percentage
rollout — on every single incoming request, so re-parsing or re-validating
the rules on each call would waste work a server does thousands of times a
second. `NewEvaluator` compiles each `Rule` into its own closure exactly
once, and the returned evaluator just walks the already-compiled list on
every call.

## What you'll build

```text
flags/                     independent module: example.com/feature-flag-compiled-rules
  go.mod                   go 1.24
  flags.go                 Request, Rule, NewEvaluator returns func(Request) bool
  cmd/
    demo/
      main.go               two rules evaluated against four requests
  flags_test.go             table test: segment/region/rollout combinations, boundaries
```

- Files: `flags.go`, `cmd/demo/main.go`, `flags_test.go`.
- Implement: `NewEvaluator(rules []Rule) func(req Request) bool`, compiling each `Rule` into a `func(Request) bool` once and returning a closure that reports true if any compiled rule matches.
- Test: a table checks segment-only match, region-gated rollout in both directions of the percentage boundary, and a request matching neither rule; a second test checks the 0%/100% rollout boundaries; a third checks a nil rule set never matches.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/24-feature-flag-evaluator-compiled-rules/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/24-feature-flag-evaluator-compiled-rules
go mod edit -go=1.24
```

### Compiling a rule into a closure, once

`NewEvaluator` loops over `rules` exactly once and calls `compileRule` for
each, producing a `[]func(Request) bool` — the "compiled" form. Each compiled
closure captures its own `Rule` by value (Go 1.22's per-iteration loop
variables make this safe without the old `rule := rule` copy). The returned
evaluator closure then just ranges over the pre-built `compiled` slice on
every call and returns on the first match — the segment check, the region
check, and the percentage check are never re-derived from the original
`Rule` struct at request time, only executed.

The rollout percentage is deliberately not driven by a random draw: `bucketOf`
hashes the user ID with FNV-1a and reduces it mod 100, so the same user
always lands in the same bucket. That is what makes a flag's rollout
*sticky* — a user who sees a feature enabled today keeps seeing it enabled
tomorrow, rather than getting a new coin flip on every request — and it is
also what makes the rollout boundary exactly testable: a table can name two
concrete user IDs, one with a low bucket and one with a high bucket, and
assert each lands on the correct side of a given rollout percentage.

Create `flags.go`:

```go
package flags

import "hash/fnv"

// Request is the per-call context a feature-flag evaluator checks against
// each rule.
type Request struct {
	UserID  string
	Segment string
	Region  string
}

// Rule describes one way a flag can be enabled. An empty Segment or Region
// matches any request. RolloutPercent gates on a deterministic hash of the
// UserID: 0 never matches, 100 always matches (once Segment and Region also
// match), and anything between matches a consistent, stable subset of users.
type Rule struct {
	Segment        string
	Region         string
	RolloutPercent int
}

// NewEvaluator compiles rules into a slice of closures exactly once and
// returns a single evaluator closure that reports whether any compiled rule
// matches req. Compiling once means every call to the returned evaluator
// only ever does the (cheap) per-rule checks — it never re-parses or
// re-validates the rule set.
func NewEvaluator(rules []Rule) func(req Request) bool {
	compiled := make([]func(Request) bool, len(rules))
	for i, rule := range rules {
		compiled[i] = compileRule(rule)
	}

	return func(req Request) bool {
		for _, matches := range compiled {
			if matches(req) {
				return true
			}
		}
		return false
	}
}

func compileRule(rule Rule) func(Request) bool {
	return func(req Request) bool {
		if rule.Segment != "" && rule.Segment != req.Segment {
			return false
		}
		if rule.Region != "" && rule.Region != req.Region {
			return false
		}
		if rule.RolloutPercent <= 0 {
			return false
		}
		if rule.RolloutPercent >= 100 {
			return true
		}
		return bucketOf(req.UserID) < rule.RolloutPercent
	}
}

// bucketOf deterministically maps a user ID to a stable bucket in [0, 100),
// using an FNV-1a hash instead of randomness so the same user always lands
// in the same bucket and evaluator tests are reproducible.
func bucketOf(userID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(userID))
	return int(h.Sum32() % 100)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-flag-compiled-rules"
)

func main() {
	evaluate := flags.NewEvaluator([]flags.Rule{
		{Segment: "beta", RolloutPercent: 100},
		{Region: "us", RolloutPercent: 30},
	})

	requests := []flags.Request{
		{UserID: "user-6", Segment: "beta", Region: "eu"},
		{UserID: "user-1", Segment: "general", Region: "us"},
		{UserID: "user-6", Segment: "general", Region: "us"},
		{UserID: "user-1", Segment: "general", Region: "eu"},
	}

	for _, req := range requests {
		fmt.Printf("user=%s segment=%s region=%s enabled=%v\n",
			req.UserID, req.Segment, req.Region, evaluate(req))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user=user-6 segment=beta region=eu enabled=true
user=user-1 segment=general region=us enabled=true
user=user-6 segment=general region=us enabled=false
user=user-1 segment=general region=eu enabled=false
```

### Tests

Create `flags_test.go`:

```go
package flags

import "testing"

func TestEvaluatorAppliesCompiledRules(t *testing.T) {
	// bucketOf("user-1") == 0, bucketOf("user-6") == 81 (deterministic FNV
	// hash, see bucketOf); chosen so the rollout boundary in rule B is
	// exercised in both directions.
	evaluate := NewEvaluator([]Rule{
		{Segment: "beta", RolloutPercent: 100},
		{Region: "us", RolloutPercent: 30},
	})

	tests := []struct {
		name string
		req  Request
		want bool
	}{
		{
			name: "beta segment matches regardless of region or user",
			req:  Request{UserID: "user-6", Segment: "beta", Region: "eu"},
			want: true,
		},
		{
			name: "non-beta in us region, low bucket falls inside 30% rollout",
			req:  Request{UserID: "user-1", Segment: "general", Region: "us"},
			want: true,
		},
		{
			name: "non-beta in us region, high bucket falls outside 30% rollout",
			req:  Request{UserID: "user-6", Segment: "general", Region: "us"},
			want: false,
		},
		{
			name: "non-beta outside us region never matches either rule",
			req:  Request{UserID: "user-1", Segment: "general", Region: "eu"},
			want: false,
		},
	}

	for _, tc := range tests {
		if got := evaluate(tc.req); got != tc.want {
			t.Fatalf("%s: evaluate(%+v) = %v, want %v", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestEvaluatorRolloutBoundaries(t *testing.T) {
	always := NewEvaluator([]Rule{{RolloutPercent: 100}})
	never := NewEvaluator([]Rule{{RolloutPercent: 0}})

	req := Request{UserID: "any-user-id", Segment: "x", Region: "y"}
	if !always(req) {
		t.Fatal("RolloutPercent 100: got false, want true regardless of bucket")
	}
	if never(req) {
		t.Fatal("RolloutPercent 0: got true, want false regardless of bucket")
	}
}

func TestEvaluatorWithNoRulesNeverMatches(t *testing.T) {
	evaluate := NewEvaluator(nil)
	if evaluate(Request{UserID: "x"}) {
		t.Fatal("evaluator with no rules matched, want false")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The main table exercises the rollout boundary in both directions with two
concrete users chosen for their known, deterministic buckets, plus the case
where neither rule matches at all. The boundary test isolates the two edges a
percentage-based rule must get exactly right regardless of any user's hash:
100% always enabled, 0% always disabled. The nil-rules test is the trivial
case a compiled pipeline must not panic on. None of this needs a fake RNG or
`-race`, because the whole point of hashing the user ID is to remove
randomness from the evaluation path entirely — the same input always
produces the same decision.

## Resources

- [pkg.go.dev: hash/fnv](https://pkg.go.dev/hash/fnv) — the deterministic hash used to bucket users for percentage rollouts.
- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — the Go 1.22 per-iteration loop variable that makes capturing `rule` in each compiled closure safe.
- [LaunchDarkly docs: Percentage rollouts](https://docs.launchdarkly.com/home/flags/rollouts) — the production feature-flagging concept this evaluator's `RolloutPercent` models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-distributed-lock-lease-renewal-gate.md](23-distributed-lock-lease-renewal-gate.md) | Next: [25-cardinality-limiter-unique-labels.md](25-cardinality-limiter-unique-labels.md)
