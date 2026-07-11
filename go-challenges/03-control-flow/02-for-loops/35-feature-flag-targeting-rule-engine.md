# Exercise 35: Feature Flag Targeting Engine with Rollout Rules

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A feature-flag service like LaunchDarkly or a homegrown equivalent has to
decide, for every single request, which variation of a flag a given user
sees — and the decision has to be both deterministic (the same user keeps
seeing the same variation across requests) and fast (the rule set is
typically cached in memory, refreshed periodically from a config store).
This module builds the targeting evaluation as an ordered loop over rules
with early exit on the first match, plus the TTL cache in front of it that
a hot request path actually needs, including the off-by-one boundary that
decides exactly when a cached rule set goes stale.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
flags/                         module example.com/flags
  go.mod                       go 1.24
  flags.go                     Rule; Engine; (*Engine).Evaluate; CachedEngine; NewCached; (*CachedEngine).Evaluate
  flags_test.go                   first-match order, segment mismatch, percentage rollout, predicate gate, default fallback, cache within TTL, cache at exact boundary, cache just before boundary
  cmd/demo/
    main.go                      three users evaluated against segment/rollout rules, then a cache TTL boundary crossing
```

- Files: `flags.go`, `flags_test.go`, `cmd/demo/main.go`.
- Implement: `(*Engine).Evaluate(userID string, ctx map[string]string) (variation, matchedRule string)` — a `for _, r := range e.Rules` loop with three early-`continue` gates (segment, percentage, predicate) and an early `return` on the first rule that clears all three; `(*CachedEngine).current() *Engine` — reload exactly when `!now.Before(expiresAt)`, never one instant later or earlier.
- Test: the first matching rule wins even when a later rule would also match; a segment mismatch skips a rule; percentage rollout is deterministic per user (same user, same bucket, every call); a predicate gate can pass or fail a rule independent of segment/percentage; no rule matching falls back to `Default`; the cache serves repeated calls within its TTL without reloading; the cache reloads exactly at the TTL boundary instant; the cache does not reload one nanosecond before that boundary.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flags/cmd/demo
cd ~/go-exercises/flags
go mod init example.com/flags
go mod edit -go=1.24
```

### Why rule order has to mean precedence, not just iteration order

`Evaluate`'s loop returns the instant it finds a rule whose segment,
percentage, and predicate gates all pass — it does not keep scanning to
see whether a "better" match exists further down the list. That is a
deliberate modeling decision, not an incidental optimization: a real
targeting configuration is authored so that the most specific override
comes first (an internal-testers segment rule) and the broadest rollout
comes last (a 10% general-availability rollout), precisely so that "first
match in list order" *is* "highest priority." An `Evaluate` that instead
scored every rule and picked the most specific one by some other criterion
would silently redefine what the person who ordered those rules meant —
which is a subtler and more dangerous bug than a crash, because it changes
*behavior* without changing any visible error.

The percentage gate deserves its own note because it is the one gate that
must be deterministic across repeated calls for the same user, not just
correct once: `bucketFor` hashes `userID` with FNV-1a and takes it modulo
100, so the same user always lands in the same bucket in `[0, 100)`
regardless of how many times `Evaluate` runs. This "sticky bucketing" is
what stops a user's variation from flipping on every request purely
because the caller reran the same rollout math on the same input — a
user's assignment only changes if the rollout percentage itself changes.

The `CachedEngine` boundary is the second subtle spot in this module. The
freshness check `now().Before(expiresAt)` treats `now == expiresAt` as
*already stale*: it is not `<`, it is the negation being `>=` in effect.
Writing the check as `now().After(expiresAt)` looks nearly identical but
is wrong in the opposite direction — it would still serve the cached
`Engine` for one extra evaluation at the exact instant it was supposed to
expire, silently extending the TTL by however long it takes a caller to
happen to land on that boundary. `TestCachedEngineExpiresExactlyAtBoundary`
and `TestCachedEngineDoesNotReloadJustBeforeBoundary` together pin the
exact instant this boundary has to fall on.

Create `flags.go`:

```go
package flags

import (
	"hash/fnv"
	"time"
)

// Rule is one targeting rule in priority order. A rule matches a request
// only if every configured gate passes: the segment gate (skipped if
// Segments is empty), the percentage rollout gate (skipped if Percentage
// is 0), and the Predicate gate (skipped if nil).
type Rule struct {
	Name       string
	Segments   []string
	Percentage int // 1-100; 0 means "no percentage gate"
	Predicate  func(ctx map[string]string) bool
	Variation  string
}

// Engine evaluates rules in order and falls back to Default if none match.
type Engine struct {
	Rules   []Rule
	Default string
}

// Evaluate returns the variation for userID/ctx: the Variation of the first
// rule (in configured order) whose gates all pass, or e.Default if no rule
// matches. matchedRule names which rule matched, or "" for the default.
//
// The for loop's early break is a deliberate precedence decision, not just
// an optimization: rules are meant to be mutually exclusive by design (an
// operator orders the most specific override first, the broadest rollout
// last), so returning at the first match is what makes rule order equal to
// rule precedence. Evaluating every rule and picking, say, the most
// specific match by some other criterion would silently change what
// "priority order" means to whoever configured the rules.
func (e *Engine) Evaluate(userID string, ctx map[string]string) (variation string, matchedRule string) {
	for _, r := range e.Rules {
		if !matchesSegment(r, ctx) {
			continue
		}
		if !matchesPercentage(r, userID) {
			continue
		}
		if r.Predicate != nil && !r.Predicate(ctx) {
			continue
		}
		return r.Variation, r.Name
	}
	return e.Default, ""
}

func matchesSegment(r Rule, ctx map[string]string) bool {
	if len(r.Segments) == 0 {
		return true
	}
	seg := ctx["segment"]
	for _, s := range r.Segments {
		if s == seg {
			return true
		}
	}
	return false
}

func matchesPercentage(r Rule, userID string) bool {
	if r.Percentage <= 0 {
		return true
	}
	return bucketFor(userID) < r.Percentage
}

// bucketFor deterministically maps userID to a bucket in [0, 100) using a
// stable hash, so the same user always lands in the same rollout bucket
// across evaluations -- the "sticky bucketing" a percentage rollout needs
// to avoid flipping a user's variation on every request.
func bucketFor(userID string) int {
	h := fnv.New32a()
	h.Write([]byte(userID))
	return int(h.Sum32() % 100)
}

// CachedEngine wraps an Engine loader behind a TTL cache, so a hot request
// path does not rebuild the rule set (which might involve a database or
// config-service call) on every single evaluation.
type CachedEngine struct {
	load      func() *Engine
	ttl       time.Duration
	now       func() time.Time
	engine    *Engine
	expiresAt time.Time
}

// NewCached builds a CachedEngine. load fetches a fresh Engine (from a
// database, a config service, or a static source); ttl controls how long a
// loaded Engine is trusted; now is the injected clock.
func NewCached(load func() *Engine, ttl time.Duration, now func() time.Time) *CachedEngine {
	return &CachedEngine{load: load, ttl: ttl, now: now}
}

// Evaluate returns the current (possibly refreshed) engine's evaluation for
// userID/ctx.
func (c *CachedEngine) Evaluate(userID string, ctx map[string]string) (string, string) {
	return c.current().Evaluate(userID, ctx)
}

// current returns the cached Engine if it is still within its TTL,
// otherwise reloads it. The boundary is deliberately !now.Before(expiresAt)
// -- an entry is stale the instant now reaches expiresAt, not only strictly
// after it. Using now.After(expiresAt) instead would serve one extra,
// already-expired evaluation at the exact boundary instant, silently
// widening the cache's effective TTL by however long a caller happens to
// evaluate exactly at that boundary.
func (c *CachedEngine) current() *Engine {
	if c.engine != nil && c.now().Before(c.expiresAt) {
		return c.engine
	}
	c.engine = c.load()
	c.expiresAt = c.now().Add(c.ttl)
	return c.engine
}
```

### The runnable demo

Two rules target three users: `user-9` is in the `internal` segment and
matches the first rule regardless of its rollout bucket; `user-1` falls
inside the 10% rollout bucket and `user-6` falls outside it. The demo then
shows the cache serving two calls within its TTL from a single load, and
reloading exactly when the clock reaches the TTL boundary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/flags"
)

func main() {
	engine := &flags.Engine{
		Rules: []flags.Rule{
			{Name: "internal-testers", Segments: []string{"internal"}, Variation: "new-checkout"},
			{Name: "10-percent-rollout", Percentage: 10, Variation: "new-checkout"},
		},
		Default: "old-checkout",
	}

	users := []struct {
		id      string
		segment string
	}{
		{"user-1", "general"},
		{"user-6", "general"},
		{"user-9", "internal"},
	}

	for _, u := range users {
		variation, rule := engine.Evaluate(u.id, map[string]string{"segment": u.segment})
		fmt.Printf("%-8s segment=%-9s -> variation=%-13s matchedRule=%q\n", u.id, u.segment, variation, rule)
	}

	fmt.Println()

	loads := 0
	load := func() *flags.Engine {
		loads++
		return engine
	}
	t := time.Unix(0, 0)
	cached := flags.NewCached(load, time.Minute, func() time.Time { return t })

	cached.Evaluate("user-1", nil)
	cached.Evaluate("user-1", nil)
	fmt.Printf("loads after 2 calls within TTL: %d\n", loads)

	t = t.Add(time.Minute)
	cached.Evaluate("user-1", nil)
	fmt.Printf("loads after reaching the TTL boundary: %d\n", loads)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
user-1   segment=general   -> variation=new-checkout  matchedRule="10-percent-rollout"
user-6   segment=general   -> variation=old-checkout  matchedRule=""
user-9   segment=internal  -> variation=new-checkout  matchedRule="internal-testers"

loads after 2 calls within TTL: 1
loads after reaching the TTL boundary: 2
```

### Tests

`TestEvaluateFirstMatchingRuleWinsInOrder` and
`TestEvaluateSegmentMismatchSkipsRule` establish the core rule-matching
loop. `TestEvaluatePercentageRolloutIsDeterministicPerUser` pins two
specific users to their known FNV-1a buckets so the rollout gate's
determinism is checked against fixed values rather than a probabilistic
sample. `TestEvaluatePredicateGatesTheRule` and
`TestEvaluateNoRuleMatchesReturnsDefault` round out the gate combinations.
`TestCachedEngineServesCachedWithinTTL`,
`TestCachedEngineExpiresExactlyAtBoundary`, and
`TestCachedEngineDoesNotReloadJustBeforeBoundary` together pin the cache's
stale-eviction boundary to the exact instant, one nanosecond on either
side.

Create `flags_test.go`:

```go
package flags

import (
	"testing"
	"time"
)

func TestEvaluateFirstMatchingRuleWinsInOrder(t *testing.T) {
	t.Parallel()

	e := &Engine{
		Rules: []Rule{
			{Name: "beta-segment", Segments: []string{"beta"}, Variation: "on"},
			{Name: "everyone-else", Variation: "control"},
		},
		Default: "off",
	}

	variation, rule := e.Evaluate("user-1", map[string]string{"segment": "beta"})
	if variation != "on" || rule != "beta-segment" {
		t.Fatalf("Evaluate() = %q, %q; want on, beta-segment", variation, rule)
	}

	variation, rule = e.Evaluate("user-2", map[string]string{"segment": "general"})
	if variation != "control" || rule != "everyone-else" {
		t.Fatalf("Evaluate() = %q, %q; want control, everyone-else", variation, rule)
	}
}

func TestEvaluateSegmentMismatchSkipsRule(t *testing.T) {
	t.Parallel()

	e := &Engine{
		Rules: []Rule{
			{Name: "vip-only", Segments: []string{"vip"}, Variation: "premium"},
		},
		Default: "standard",
	}

	variation, rule := e.Evaluate("user-1", map[string]string{"segment": "general"})
	if variation != "standard" || rule != "" {
		t.Fatalf("Evaluate() = %q, %q; want standard, \"\" (segment does not match)", variation, rule)
	}
}

func TestEvaluatePercentageRolloutIsDeterministicPerUser(t *testing.T) {
	t.Parallel()

	e := &Engine{
		Rules: []Rule{
			{Name: "10-percent-rollout", Percentage: 10, Variation: "new-ui"},
		},
		Default: "old-ui",
	}

	// user-1 hashes into bucket 0 (always inside any positive percentage);
	// user-6 hashes into bucket 81 (outside a 10% rollout). Both are fixed
	// properties of FNV-1a over these exact strings, not flaky assumptions.
	variation, rule := e.Evaluate("user-1", nil)
	if variation != "new-ui" || rule != "10-percent-rollout" {
		t.Fatalf("Evaluate(user-1) = %q, %q; want new-ui, 10-percent-rollout", variation, rule)
	}

	variation, rule = e.Evaluate("user-6", nil)
	if variation != "old-ui" || rule != "" {
		t.Fatalf("Evaluate(user-6) = %q, %q; want old-ui, \"\" (outside the 10%% bucket)", variation, rule)
	}
}

func TestEvaluatePredicateGatesTheRule(t *testing.T) {
	t.Parallel()

	e := &Engine{
		Rules: []Rule{
			{
				Name:      "enterprise-plan-only",
				Predicate: func(ctx map[string]string) bool { return ctx["plan"] == "enterprise" },
				Variation: "advanced-reporting",
			},
		},
		Default: "basic-reporting",
	}

	variation, _ := e.Evaluate("user-1", map[string]string{"plan": "enterprise"})
	if variation != "advanced-reporting" {
		t.Fatalf("Evaluate() = %q, want advanced-reporting", variation)
	}

	variation, _ = e.Evaluate("user-1", map[string]string{"plan": "free"})
	if variation != "basic-reporting" {
		t.Fatalf("Evaluate() = %q, want basic-reporting", variation)
	}
}

func TestEvaluateNoRuleMatchesReturnsDefault(t *testing.T) {
	t.Parallel()

	e := &Engine{
		Rules: []Rule{
			{Name: "unreachable", Segments: []string{"nobody-is-in-this-segment"}, Variation: "x"},
		},
		Default: "fallback",
	}

	variation, rule := e.Evaluate("user-1", nil)
	if variation != "fallback" || rule != "" {
		t.Fatalf("Evaluate() = %q, %q; want fallback, \"\"", variation, rule)
	}
}

func TestCachedEngineServesCachedWithinTTL(t *testing.T) {
	t.Parallel()

	clock := time.Unix(0, 0)
	loads := 0
	load := func() *Engine {
		loads++
		return &Engine{Default: "v"}
	}

	c := NewCached(load, time.Minute, func() time.Time { return clock })

	c.Evaluate("user-1", nil)
	c.Evaluate("user-1", nil)
	c.Evaluate("user-1", nil)

	if loads != 1 {
		t.Fatalf("load called %d times, want 1 (served from cache)", loads)
	}
}

func TestCachedEngineExpiresExactlyAtBoundary(t *testing.T) {
	t.Parallel()

	clock := time.Unix(0, 0)
	loads := 0
	load := func() *Engine {
		loads++
		return &Engine{Default: "v"}
	}

	c := NewCached(load, time.Minute, func() time.Time { return clock })

	c.Evaluate("user-1", nil)
	if loads != 1 {
		t.Fatalf("load called %d times after first Evaluate, want 1", loads)
	}

	clock = clock.Add(time.Minute) // exactly at expiresAt: must be treated as stale
	c.Evaluate("user-1", nil)
	if loads != 2 {
		t.Fatalf("load called %d times at the exact TTL boundary, want 2 (must reload)", loads)
	}
}

func TestCachedEngineDoesNotReloadJustBeforeBoundary(t *testing.T) {
	t.Parallel()

	clock := time.Unix(0, 0)
	loads := 0
	load := func() *Engine {
		loads++
		return &Engine{Default: "v"}
	}

	c := NewCached(load, time.Minute, func() time.Time { return clock })

	c.Evaluate("user-1", nil)
	clock = clock.Add(time.Minute - time.Nanosecond)
	c.Evaluate("user-1", nil)

	if loads != 1 {
		t.Fatalf("load called %d times just before the TTL boundary, want 1", loads)
	}
}
```

## Review

`Evaluate` is correct when it returns the first rule in configured order
whose segment, percentage, and predicate gates all pass, and `CachedEngine`
is correct when its reload boundary is exactly `now == expiresAt`, neither
one instant earlier nor later. The common mistake this design avoids is
writing the cache boundary as `now().After(c.expiresAt)` instead of the
negation of `Before` — the two read as nearly interchangeable, but the
`After` version serves one additional stale evaluation at the exact expiry
instant every single time the cache happens to be checked at that boundary,
which is a real occurrence under any request volume, not a hypothetical
edge case. Run `go test -count=1 ./...`.

## Resources

- [LaunchDarkly: How feature flag targeting rules work](https://launchdarkly.com/docs/home/flags/rules) — the segment/percentage/predicate rule model this module mirrors.
- [hash/fnv package](https://pkg.go.dev/hash/fnv) — the stable hash behind deterministic percentage bucketing.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the early-return loop that turns rule order into rule precedence.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-exponential-backoff-deadline-budget.md](34-exponential-backoff-deadline-budget.md) | Next: [../03-switch-statements/00-concepts.md](../03-switch-statements/00-concepts.md)
