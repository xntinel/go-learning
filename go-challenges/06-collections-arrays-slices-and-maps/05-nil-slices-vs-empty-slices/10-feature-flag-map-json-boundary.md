# Exercise 10: Feature Flag Map Response: Null Versus Empty Object

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A feature-flag service like LaunchDarkly or a homegrown capability endpoint
answers one question per request: which flags are enabled for this client.
The response is a JSON object, `{"new-checkout":true}`, that the client SDK
merges into whatever flags it already cached locally. That merge only works
if the object is always an object. The moment a client rolled out to zero
flags -- a brand-new tenant, a segment with no active experiments, a canary
population outside every rollout -- the naive handler's `map[string]bool`
is still its zero value, and `json.Marshal` of a nil map does not emit `{}`.
It emits `null`.

The bug is structural, not a typo. A handler that only allocates its result
map inside the loop, on the first match, produces a nil map whenever the
loop never matches -- which is precisely the "healthy, boring, nothing is
enabled" case that ships to the largest slice of any user base. The client
SDK, written to `Object.assign(localFlags, response)` or the equivalent,
either throws on `null` or silently treats it as "no update," neither of
which is what a `{}` response is supposed to mean: *these are all the flags
currently on, which happens to be none.*

This module builds the evaluator as a package: a `FlagSet` that validates
its own rule list, an `Evaluate` method that always allocates its result map
before the loop runs so a zero-match evaluation still returns `{}`, and an
`Encode` helper that puts Go 1.24's `omitzero` next to the older `omitempty`
on the same field so the difference between them is visible in one struct.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
flagset/                 module example.com/flagset
  go.mod                 go 1.24
  flagset.go             Rule, FlagSet, Response; NewFlagSet, Evaluate, Encode; two sentinel errors
  flagset_test.go        evaluation table, zero-match non-nil, the buggy-evaluator contrast,
                          rule validation, omitzero-vs-omitempty, ExampleFlagSet_Evaluate
```

- Files: `flagset.go`, `flagset_test.go`.
- Implement: `NewFlagSet(rules []Rule) (*FlagSet, error)` rejecting an empty flag name with `ErrEmptyFlagName` and a repeated flag name with `ErrDuplicateFlag`; `(*FlagSet).Evaluate(ctx []string) map[string]bool` that always returns a non-nil map; `Encode(flags map[string]bool) ([]byte, error)` marshaling the same map through both an `omitzero` and an `omitempty` field.
- Test: the evaluation table (a scoped rule, a global rollout, an unmatched segment, a nil context); the zero-match result is non-nil and marshals to `{}`; `NewFlagSet` rejects bad rules; an empty rule list is valid; the `omitzero`-vs-`omitempty` contrast on the same value; and `ExampleFlagSet_Evaluate` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/10-feature-flag-map-json-boundary
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/10-feature-flag-map-json-boundary
go mod edit -go=1.24
```

### The map-side null, and the tag that finally fixes it

`json.Marshal` treats a nil map exactly like it treats a nil slice: it
writes `null`. A non-nil map, even an empty one built with `make`, writes
`{}`. Nothing about the *contents* differs -- both have zero entries -- but
the pointer inside the map header is nil in one case and not in the other,
and the encoder's `null`-vs-`{}` decision reads that pointer, not the
length. The naive evaluator gets this wrong by construction:

```go
var flags map[string]bool
for _, r := range rules {
    if r.matches(ctx) {
        if flags == nil {
            flags = make(map[string]bool)
        }
        flags[r.Flag] = true
    }
}
return flags   // nil if the loop never matched -- marshals to null
```

The map is only born the first time a rule matches. Zero matches means the
loop body that allocates it never runs, so the function returns the zero
value of `map[string]bool`, which is nil. The fix does not need a nil guard
at the end; it needs the allocation moved before the loop, so the map exists
whether or not anything ever gets written into it:

```go
enabled := make(map[string]bool)   // exists now, regardless of what follows
for _, r := range fs.rules {
    if r.matches(ctx) {
        enabled[r.Flag] = true
    }
}
return enabled
```

The second half of this lesson is the struct tag that controls what happens
to that map once it is wrapped in a JSON envelope. `omitempty` drops a field
whenever it is "empty" in the JSON sense -- for a map that means *both* nil
and non-nil-empty are dropped, so a field tagged `omitempty` can never
render `{}`; it either shows real content or vanishes. `omitzero`, added in
Go 1.24, drops a field only when it equals the type's zero value, and the
zero value of a map is nil -- so `omitzero` drops nil but keeps a non-nil
empty map, rendering `{}`. That is the lever that makes "zero flags enabled,
and we know it" distinguishable from "this endpoint doesn't have a flags
concept at all."

Create `flagset.go`:

```go
// Package flagset evaluates feature-flag rules against a request context and
// serializes the result as a JSON object that is always safe for a client SDK
// to merge into local storage: zero matched flags produce {}, never null.
package flagset

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by NewFlagSet. Callers should test for them with
// errors.Is rather than by comparing error strings.
var (
	// ErrEmptyFlagName means a Rule's Flag field was the empty string.
	ErrEmptyFlagName = errors.New("flagset: rule flag name must not be empty")
	// ErrDuplicateFlag means two rules declared the same flag name.
	ErrDuplicateFlag = errors.New("flagset: duplicate flag name")
)

// Rule describes when one feature flag is enabled. Segments lists the
// context tags that turn the flag on; an empty Segments means the flag is
// enabled for every context (a full rollout).
type Rule struct {
	Flag     string
	Segments []string
}

// matches reports whether ctx satisfies the rule: an empty Segments always
// matches, otherwise ctx must contain at least one of them.
func (r Rule) matches(ctx []string) bool {
	if len(r.Segments) == 0 {
		return true
	}
	for _, want := range r.Segments {
		if slices.Contains(ctx, want) {
			return true
		}
	}
	return false
}

// FlagSet evaluates a fixed list of rules against a caller-supplied context.
//
// A FlagSet is immutable after construction and is safe for concurrent use
// by multiple goroutines.
type FlagSet struct {
	rules []Rule
}

// NewFlagSet builds a FlagSet from rules. It returns ErrEmptyFlagName if any
// rule has an empty Flag, and ErrDuplicateFlag if two rules share a Flag.
func NewFlagSet(rules []Rule) (*FlagSet, error) {
	seen := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		if r.Flag == "" {
			return nil, ErrEmptyFlagName
		}
		if _, dup := seen[r.Flag]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateFlag, r.Flag)
		}
		seen[r.Flag] = struct{}{}
	}
	return &FlagSet{rules: slices.Clone(rules)}, nil
}

// Evaluate reports which flags are enabled for ctx. The returned map is
// always non-nil, even when no rule matches: it is built with make before
// the loop runs, not lazily on the first match, so json.Marshal of a
// zero-flag result renders {} rather than null.
//
// The returned map is a fresh allocation owned by the caller; mutating it
// does not affect the FlagSet.
func (fs *FlagSet) Evaluate(ctx []string) map[string]bool {
	enabled := make(map[string]bool)
	for _, r := range fs.rules {
		if r.matches(ctx) {
			enabled[r.Flag] = true
		}
	}
	return enabled
}

// Response is the JSON envelope a flag-evaluation endpoint returns. It
// carries the same evaluation result twice, tagged two different ways, to
// make the omitzero-versus-omitempty contract visible side by side: Flags is
// what a client SDK should read, Legacy shows what omitempty would have done
// to the identical value.
type Response struct {
	// Flags is tagged omitzero: a nil map is dropped (the field is absent),
	// but a non-nil empty map survives and renders as {}.
	Flags map[string]bool `json:"flags,omitzero"`
	// Legacy is tagged omitempty: both a nil map and a non-nil empty map are
	// dropped, so a caller reading this field cannot tell "no flags enabled"
	// from "field not sent at all".
	Legacy map[string]bool `json:"legacy,omitempty"`
}

// Encode marshals flags into a Response using both struct-tag strategies at
// once, so the two encodings can be compared directly for the same value.
func Encode(flags map[string]bool) ([]byte, error) {
	return json.Marshal(Response{Flags: flags, Legacy: flags})
}
```

### Using it

Build one `FlagSet` at startup from the tenant's or the environment's rule
list -- `NewFlagSet` rejects a malformed rule before the service ever
answers a request -- and call `Evaluate` per request with the caller's
segment tags. Because `FlagSet` holds nothing but its own validated,
defensively cloned rule slice, a single value can be shared across every
handler goroutine without a mutex, and `Evaluate`'s returned map is a fresh
allocation the caller may retain, wrap, or mutate freely.

The module has no `main.go`, because a flag evaluator is a library, not a
tool. Its executable demonstration is `ExampleFlagSet_Evaluate`: `go test`
runs it and compares its standard output against the `// Output:` comment,
so the usage shown below cannot drift away from the code.

```go
func ExampleFlagSet_Evaluate() {
	fs, err := NewFlagSet(demoRules())
	if err != nil {
		panic(err)
	}

	matched := fs.Evaluate([]string{"beta"})
	b, err := Encode(matched)
	if err != nil {
		panic(err)
	}
	fmt.Println("beta context:", string(b))

	scoped, err := NewFlagSet([]Rule{{Flag: "new-checkout", Segments: []string{"beta"}}})
	if err != nil {
		panic(err)
	}
	zero := scoped.Evaluate([]string{"nobody-matches-this"})
	b2, err := Encode(zero)
	if err != nil {
		panic(err)
	}
	fmt.Println("no scoped rule matches:", string(b2))

	// Output:
	// beta context: {"flags":{"dark-mode":true,"new-checkout":true},"legacy":{"dark-mode":true,"new-checkout":true}}
	// no scoped rule matches: {"flags":{}}
}
```

The second line is the whole module in one comparison: `flags` still shows
up as `{}` because it is tagged `omitzero` and its value, while empty, is
non-nil; `legacy` disappears entirely because `omitempty` drops a non-nil
empty map exactly as it would drop a nil one.

### Tests

`TestEvaluate` is the table: a rule scoped to one segment, a global rollout
with no segments at all, a context that matches nothing but the rollout, and
a nil context, which must still see the rollout since `matches` on an empty
`Segments` never even looks at `ctx`. `TestEvaluateZeroMatchesIsNonNil`
isolates the case that matters most -- a context that satisfies no rule --
and checks both the Go-level non-nilness and the JSON-level `{}`.

`TestBuggyEvaluatorSerializesNull` is the heart of the module. `evaluateBuggy`
is unexported and unreachable from the package API; it exists so the test
can state the defect precisely -- for the identical rules and context,
`evaluateBuggy` returns `nil` and marshals to `null`, while `FlagSet.Evaluate`
returns a non-nil empty map and marshals to `{}`. If a future edit moves the
`make` call back inside the loop, this test fails here instead of in a
client's `Object.assign`.

`TestNewFlagSetRejectsBadRules` and `TestNewFlagSetEmptyRulesIsValid` cover
construction: an empty flag name, a duplicate flag name, and the legitimate
edge case of a `FlagSet` with zero rules, which must still evaluate to a
non-nil empty map rather than erroring. `TestEncodeOmitzeroVersusOmitempty`
pins the exact byte output of both tag strategies against the same value.

Create `flagset_test.go`:

```go
package flagset

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func demoRules() []Rule {
	return []Rule{
		{Flag: "new-checkout", Segments: []string{"beta"}},
		{Flag: "dark-mode", Segments: nil}, // empty Segments: global rollout
		{Flag: "admin-panel", Segments: []string{"internal", "staff"}},
	}
}

// evaluateBuggy is the naive evaluator: it declares the result map with its
// zero value and only allocates on the first match. It is never exported and
// never reachable from FlagSet; it exists so the tests can pin the defect it
// ships: a zero-flag evaluation returns nil instead of an empty map, and
// json.Marshal renders that as null.
func evaluateBuggy(rules []Rule, ctx []string) map[string]bool {
	var flags map[string]bool
	for _, r := range rules {
		if r.matches(ctx) {
			if flags == nil {
				flags = make(map[string]bool)
			}
			flags[r.Flag] = true
		}
	}
	return flags
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	fs, err := NewFlagSet(demoRules())
	if err != nil {
		t.Fatalf("NewFlagSet: %v", err)
	}

	tests := []struct {
		name string
		ctx  []string
		want map[string]bool
	}{
		{
			name: "beta segment matches two rules",
			ctx:  []string{"beta"},
			want: map[string]bool{"new-checkout": true, "dark-mode": true},
		},
		{
			name: "staff segment matches admin and global",
			ctx:  []string{"staff"},
			want: map[string]bool{"admin-panel": true, "dark-mode": true},
		},
		{
			name: "unknown segment still gets the global rollout",
			ctx:  []string{"unrelated-tag"},
			want: map[string]bool{"dark-mode": true},
		},
		{
			name: "nil context still gets the global rollout",
			ctx:  nil,
			want: map[string]bool{"dark-mode": true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := fs.Evaluate(tc.ctx)
			if got == nil {
				t.Fatal("Evaluate returned a nil map")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Evaluate(%v) = %v, want %v", tc.ctx, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("Evaluate(%v)[%q] = %v, want %v", tc.ctx, k, got[k], v)
				}
			}
		})
	}
}

// TestEvaluateZeroMatchesIsNonNil is the module's central assertion: a
// context that matches no scoped rule (and there is no global rollout in
// this fixture) must still yield a non-nil, empty map.
func TestEvaluateZeroMatchesIsNonNil(t *testing.T) {
	t.Parallel()

	rules := []Rule{{Flag: "new-checkout", Segments: []string{"beta"}}}
	fs, err := NewFlagSet(rules)
	if err != nil {
		t.Fatalf("NewFlagSet: %v", err)
	}

	got := fs.Evaluate([]string{"nobody-matches-this"})
	if got == nil {
		t.Fatal("Evaluate with zero matches returned nil, want a non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("Evaluate with zero matches = %v, want empty", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Fatalf("Marshal(zero-match result) = %s, want {}", b)
	}
}

// TestBuggyEvaluatorSerializesNull contrasts evaluateBuggy against Evaluate
// for the exact same rules and context: the buggy path returns nil and
// serializes to null, corrupting the client's Object.assign(local, flags)
// merge; the package's own Evaluate returns {} for the identical input.
func TestBuggyEvaluatorSerializesNull(t *testing.T) {
	t.Parallel()

	rules := []Rule{{Flag: "new-checkout", Segments: []string{"beta"}}}
	ctx := []string{"nobody-matches-this"}

	buggy := evaluateBuggy(rules, ctx)
	if buggy != nil {
		t.Fatalf("evaluateBuggy = %v, want nil (that is the defect being pinned)", buggy)
	}
	bb, err := json.Marshal(buggy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(bb) != "null" {
		t.Fatalf("Marshal(evaluateBuggy result) = %s, want null", bb)
	}

	fs, err := NewFlagSet(rules)
	if err != nil {
		t.Fatalf("NewFlagSet: %v", err)
	}
	good := fs.Evaluate(ctx)
	gb, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(gb) != "{}" {
		t.Fatalf("Marshal(Evaluate result) = %s, want {}", gb)
	}
}

func TestNewFlagSetRejectsBadRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rules []Rule
		want  error
	}{
		{
			name:  "empty flag name",
			rules: []Rule{{Flag: ""}},
			want:  ErrEmptyFlagName,
		},
		{
			name:  "duplicate flag name",
			rules: []Rule{{Flag: "dup"}, {Flag: "dup"}},
			want:  ErrDuplicateFlag,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewFlagSet(tc.rules); !errors.Is(err, tc.want) {
				t.Fatalf("NewFlagSet error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewFlagSetEmptyRulesIsValid(t *testing.T) {
	t.Parallel()

	fs, err := NewFlagSet(nil)
	if err != nil {
		t.Fatalf("NewFlagSet(nil): %v", err)
	}
	got := fs.Evaluate([]string{"anything"})
	if got == nil || len(got) != 0 {
		t.Fatalf("Evaluate on an empty FlagSet = %v, want a non-nil empty map", got)
	}
}

// TestEncodeOmitzeroVersusOmitempty pins the two encodings side by side for
// the exact same zero-flag value: omitzero keeps the field and renders {},
// omitempty drops the field entirely.
func TestEncodeOmitzeroVersusOmitempty(t *testing.T) {
	t.Parallel()

	b, err := Encode(map[string]bool{})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(b) != `{"flags":{}}` {
		t.Fatalf("Encode(empty map) = %s, want {\"flags\":{}}", b)
	}

	nb, err := Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(nb) != `{}` {
		t.Fatalf("Encode(nil map) = %s, want {}", nb)
	}
}

// ExampleFlagSet_Evaluate is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment below.
func ExampleFlagSet_Evaluate() {
	fs, err := NewFlagSet(demoRules())
	if err != nil {
		panic(err)
	}

	matched := fs.Evaluate([]string{"beta"})
	b, err := Encode(matched)
	if err != nil {
		panic(err)
	}
	fmt.Println("beta context:", string(b))

	scoped, err := NewFlagSet([]Rule{{Flag: "new-checkout", Segments: []string{"beta"}}})
	if err != nil {
		panic(err)
	}
	zero := scoped.Evaluate([]string{"nobody-matches-this"})
	b2, err := Encode(zero)
	if err != nil {
		panic(err)
	}
	fmt.Println("no scoped rule matches:", string(b2))

	// Output:
	// beta context: {"flags":{"dark-mode":true,"new-checkout":true},"legacy":{"dark-mode":true,"new-checkout":true}}
	// no scoped rule matches: {"flags":{}}
}
```

## Review

`Evaluate` is correct when a zero-match request still returns a non-nil map
-- the property `TestEvaluateZeroMatchesIsNonNil` and the contrast test both
pin. The mechanism is moving `make(map[string]bool)` out of the loop and in
front of it: a map that exists before any rule is checked exists no matter
how the checking turns out, while a map born inside an `if` only exists on
the branch that runs it. Around that core, `NewFlagSet` rejects an empty or
duplicate flag name with a sentinel checkable via `errors.Is`, `Evaluate`
never aliases the `FlagSet`'s own rule slice back to the caller, and `Encode`
puts `omitzero` next to `omitempty` on the same map value so the difference
-- absent field versus `{}` -- is something a reader can see rather than
take on faith. `ExampleFlagSet_Evaluate` is the executable documentation:
`go test` verifies its output. Run `go test -count=1 -race ./...`.

## Resources

- [`encoding/json` — Marshal](https://pkg.go.dev/encoding/json#Marshal) — the rule that a nil map encodes as `null` and a non-nil empty map as `{}`.
- [Go 1.24 release notes — omitzero](https://go.dev/doc/go1.24#encodingjsonpkgencodingjson) — the new struct-tag option and how it differs from `omitempty`.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the defensive copy `NewFlagSet` takes of the caller's rule slice.
- [`slices.Contains`](https://pkg.go.dev/slices#Contains) — used by `Rule.matches` to test segment membership.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-filter-in-place-zero-tail-pointers.md](09-filter-in-place-zero-tail-pointers.md) | Next: [11-cluster-registry-map-clone-snapshot.md](11-cluster-registry-map-clone-snapshot.md)
