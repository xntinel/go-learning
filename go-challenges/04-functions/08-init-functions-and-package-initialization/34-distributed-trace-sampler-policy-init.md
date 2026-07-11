# Exercise 34: Distributed Tracing Sample Rate Policies Initialized at init with Validation and Precompiled Rules

**Nivel: Intermedio** — validacion rapida (un test corto, mas prueba de orden de reglas).

A distributed tracing system typically samples only a fraction of requests
— sampling everything at scale would be prohibitively expensive to store
and process. Different services often need different rates: a
high-traffic, well-understood service samples at 5%, a service under
active investigation samples at 100%, and a whole family of services
sharing a prefix might get its own rate. This exercise validates such a
policy and precompiles its patterns into matcher rules, ordered by
specificity, at package initialization.

## What you'll build

```text
tracesample/                independent module: example.com/tracesample
  go.mod                      module example.com/tracesample
  tracesample.go                policyEntry, compilePolicies (validate + sort by specificity), SampleRate
  cmd/
    demo/
      main.go                   resolves sample rates for an exact match, a prefix match, and the catch-all
  tracesample_test.go            validation table + specificity-ordering proof + SampleRate resolution tests
```

Files: `tracesample.go`, `cmd/demo/main.go`, `tracesample_test.go`.
Implement: `compilePolicies(raw []policyEntry) ([]Rule, error)` rejecting an empty pattern and a rate outside `[0,1]`, building a matcher per pattern (`"*"`, a `"prefix-*"` wildcard, or an exact name), and sorting the result from most to least specific; `SampleRate(service string) float64` returning the first matching rule's rate.
Test: an empty pattern and an out-of-range rate both fail; feeding the catch-all first and the exact match last in the raw policy still compiles to exact-then-prefix-then-catch-all order; `SampleRate` resolves an exact match, a prefix match, and the catch-all correctly; a policy with no catch-all does not match an unrelated service name.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tracesample/cmd/demo
cd ~/go-exercises/tracesample
go mod init example.com/tracesample
go mod edit -go=1.24
```

### Why sort by specificity instead of trusting declaration order

A sampling policy read from config is naturally written in whatever order
is convenient for a human: the catch-all default often comes first (it is
the baseline), with specific overrides listed afterward as afterthoughts.
If `SampleRate` matched rules in that same declaration order, the catch-all
`"*"` — matching every possible service name — would win over every more
specific rule listed after it, silently making every override dead
configuration. `compilePolicies` fixes this by sorting the compiled rules
by specificity before `SampleRate` ever sees them: an exact service name
always outranks a prefix wildcard, a longer prefix wildcard outranks a
shorter one (so `"payments-eu-*"` would win over `"payments-*"` if both
existed), and the bare `"*"` catch-all always sorts last. This means the
raw policy's declaration order in config is irrelevant to correctness —
exactly the same principle this chapter used for package-level variable
initialization order, applied here to a slice of rules instead of a set of
`var` declarations.

Validating each entry's rate to `[0,1]` and its pattern to non-empty at
compile time, rather than at each `SampleRate` call, is the same
fail-fast-at-init idiom used throughout this chapter: a sampling rate of
`1.5` or `-0.2` is nonsensical and should never reach a running tracer, and
catching it the instant the binary starts is strictly better than
discovering it when a sampling library rejects (or, worse, silently
clamps) an out-of-range probability at request time.

Create `tracesample.go`:

```go
// tracesample.go
// Package tracesample validates a distributed tracing sample-rate policy at
// package initialization and precompiles it into matcher rules ordered from
// most to least specific -- so resolving a service's sample rate at
// runtime is a short walk over precompiled matchers, and the declaration
// order of the raw policy entries in config never matters.
package tracesample

import (
	"fmt"
	"sort"
	"strings"
)

// policyEntry is one raw sample-rate policy: a service name pattern and the
// probability (0 to 1) of sampling a trace that matches it. Pattern is
// either "*" (matches every service), a prefix wildcard like "payments-*",
// or an exact service name.
type policyEntry struct {
	Pattern string
	Rate    float64
}

// rawPolicies is the static configuration. Order here does not matter --
// compilePolicies sorts by specificity, not declaration order.
var rawPolicies = []policyEntry{
	{Pattern: "*", Rate: 0.05},
	{Pattern: "payments-*", Rate: 0.5},
	{Pattern: "debug-service", Rate: 1.0},
}

// Rule is one precompiled, ready-to-match policy entry.
type Rule struct {
	Pattern string
	Rate    float64
	matcher func(string) bool
}

// compiledRules holds rawPolicies compiled and sorted by specificity,
// built once at init.
var compiledRules []Rule

func init() {
	rules, err := compilePolicies(rawPolicies)
	if err != nil {
		panic("tracesample: " + err.Error())
	}
	compiledRules = rules
}

// compilePolicies validates every entry's rate is in [0,1] and its pattern
// is non-empty, builds a matcher function per entry, and returns the rules
// sorted from most to least specific: exact names first, then prefix
// wildcards (longer prefix first), then the catch-all "*" last -- so the
// first matching rule in the returned order is always the most specific
// one, regardless of the order entries were declared in. Extracted from
// init so tests can exercise validation and ordering directly.
func compilePolicies(raw []policyEntry) ([]Rule, error) {
	rules := make([]Rule, 0, len(raw))
	for _, e := range raw {
		if e.Pattern == "" {
			return nil, fmt.Errorf("policy pattern is empty")
		}
		if e.Rate < 0 || e.Rate > 1 {
			return nil, fmt.Errorf("policy %q: rate %v is out of range [0,1]", e.Pattern, e.Rate)
		}
		rules = append(rules, Rule{Pattern: e.Pattern, Rate: e.Rate, matcher: matcherFor(e.Pattern)})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		return specificity(rules[i].Pattern) > specificity(rules[j].Pattern)
	})
	return rules, nil
}

// matcherFor builds the matcher function for one pattern.
func matcherFor(pattern string) func(string) bool {
	if pattern == "*" {
		return func(string) bool { return true }
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return func(s string) bool { return strings.HasPrefix(s, prefix) }
	}
	return func(s string) bool { return s == pattern }
}

// specificity ranks a pattern for sorting: an exact name outranks any
// prefix wildcard, a longer prefix wildcard outranks a shorter one, and the
// catch-all "*" ranks lowest of all.
func specificity(pattern string) int {
	if pattern == "*" {
		return -1
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return len(prefix)
	}
	return 1 << 30 // exact match: always more specific than any wildcard
}

// SampleRate returns the sample rate for service, using the first (most
// specific) matching precompiled rule. It returns 0 if no rule matches,
// which cannot happen with a policy that includes a "*" catch-all.
func SampleRate(service string) float64 {
	for _, r := range compiledRules {
		if r.matcher(service) {
			return r.Rate
		}
	}
	return 0
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/tracesample"
)

func main() {
	for _, service := range []string{"debug-service", "payments-api", "auth-service"} {
		fmt.Printf("%s sample rate: %v\n", service, tracesample.SampleRate(service))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
debug-service sample rate: 1
payments-api sample rate: 0.5
auth-service sample rate: 0.05
```

### Tests

Create `tracesample_test.go`:

```go
// tracesample_test.go
package tracesample

import (
	"strings"
	"testing"
)

func TestCompilePoliciesValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     []policyEntry
		wantErr string
	}{
		{"ok", []policyEntry{{Pattern: "*", Rate: 0.1}}, ""},
		{"empty pattern", []policyEntry{{Pattern: "", Rate: 0.1}}, "empty"},
		{"rate too high", []policyEntry{{Pattern: "*", Rate: 1.5}}, "out of range"},
		{"rate negative", []policyEntry{{Pattern: "*", Rate: -0.1}}, "out of range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := compilePolicies(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestCompilePoliciesSortsBySpecificityRegardlessOfInputOrder feeds the
// catch-all first and the exact match last, and asserts the compiled order
// is exact, then prefix wildcard, then catch-all -- proving compilePolicies
// sorts by specificity rather than preserving declaration order.
func TestCompilePoliciesSortsBySpecificityRegardlessOfInputOrder(t *testing.T) {
	t.Parallel()

	raw := []policyEntry{
		{Pattern: "*", Rate: 0.05},
		{Pattern: "payments-*", Rate: 0.5},
		{Pattern: "debug-service", Rate: 1.0},
	}
	rules, err := compilePolicies(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("len(rules) = %d, want 3", len(rules))
	}
	wantOrder := []string{"debug-service", "payments-*", "*"}
	for i, want := range wantOrder {
		if rules[i].Pattern != want {
			t.Fatalf("rules[%d].Pattern = %q, want %q (full order: %v)", i, rules[i].Pattern, want, rulePatterns(rules))
		}
	}
}

func rulePatterns(rules []Rule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.Pattern
	}
	return out
}

func TestSampleRateResolvesMostSpecificMatch(t *testing.T) {
	t.Parallel()

	if got := SampleRate("debug-service"); got != 1.0 {
		t.Fatalf("SampleRate(debug-service) = %v, want 1.0 (exact match)", got)
	}
	if got := SampleRate("payments-api"); got != 0.5 {
		t.Fatalf("SampleRate(payments-api) = %v, want 0.5 (prefix match)", got)
	}
	if got := SampleRate("auth-service"); got != 0.05 {
		t.Fatalf("SampleRate(auth-service) = %v, want 0.05 (catch-all)", got)
	}
}

func TestSampleRateWithoutCatchAllReturnsZeroForUnmatched(t *testing.T) {
	t.Parallel()

	rules, err := compilePolicies([]policyEntry{{Pattern: "payments-*", Rate: 0.5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Exercise the matcher directly since SampleRate reads the package-level
	// compiledRules; here we only need to confirm no rule in this list
	// matches an unrelated service name.
	matched := false
	for _, r := range rules {
		if r.matcher("auth-service") {
			matched = true
		}
	}
	if matched {
		t.Fatal("payments-* should not match auth-service")
	}
}
```

## Review

`compilePolicies` is correct when it rejects an empty pattern and any rate
outside `[0,1]`, and when it always sorts an exact name ahead of every
prefix wildcard and every wildcard ahead of the bare `"*"` catch-all —
`TestCompilePoliciesSortsBySpecificityRegardlessOfInputOrder` proves this
by feeding the entries in the least helpful order (catch-all first, exact
match last) and checking the compiled order anyway comes out
exact-prefix-catchall. `SampleRate` is correct when it resolves each of the
three shapes of match to the right rate, which only holds because the
rules it walks are pre-sorted; if `SampleRate` walked the raw, unsorted
policy instead, the catch-all's `0.05` would win for every service,
including `"debug-service"`, whose whole purpose is to override it.

The mistake to avoid is trusting a config file's declaration order to also
be the correct matching precedence. That works by accident as long as
whoever edits the config remembers to keep specific rules listed before the
catch-all — and breaks the moment someone reorders entries for readability,
adds a new override at the top, or generates the config programmatically in
a different order. Sorting explicitly by a computed specificity, as
`compilePolicies` does, removes declaration order from the correctness
picture entirely.

## Resources

- [OpenTelemetry — Sampling](https://opentelemetry.io/docs/concepts/sampling/) — why and how distributed tracing systems sample only a fraction of requests.
- [sort.SliceStable](https://pkg.go.dev/sort#SliceStable) — the stable sort `compilePolicies` uses so equally-specific rules keep their relative declaration order.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-encryption-key-derivation-lazy-init.md](33-encryption-key-derivation-lazy-init.md) | Next: [../09-closure-gotchas-loop-variable-capture/00-concepts.md](../09-closure-gotchas-loop-variable-capture/00-concepts.md)
