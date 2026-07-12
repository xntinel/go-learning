# Exercise 31: Feature Flag Evaluator With Context-Aware Overrides and Rollout Strategy

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag evaluator has to reconcile three sources of truth for the
same question — is this flag on? — in a fixed precedence: a per-context
override always wins, an empty context can't be safely bucketed so it falls
back to a configured default, and everything else goes through a
percentage rollout. This module builds that evaluator with functional
options, validating the rollout percentage and the override budget.

## What you'll build

```text
flag/                             independent module: example.com/feature-flag-evaluator-context
  go.mod                          go 1.24
  flag.go                         Evaluator, Option, New, WithRolloutPercentage,
                                   WithMaxOverrides, WithFallback, WithOverride,
                                   Evaluate, OverrideCount
  cmd/
    demo/
      main.go                     two forced overrides, an empty-context fallback, a rollout hit
  flag_test.go                     option-validation table, precedence, and determinism tests
```

- Files: `flag.go`, `cmd/demo/main.go`, `flag_test.go`.
- Implement: an `Evaluator` built by `New(name string, opts ...Option) (*Evaluator, error)` whose `Evaluate` checks overrides first, then the fallback for an empty context, then a deterministic hash-based rollout bucket, validating that the rollout percentage is 0-100 and that registered overrides never exceed the configured max.
- Test: every option-validation case including overrides registered before *and* after the max is set, override precedence over both a 0% and a 100% rollout, the empty-context fallback, the rollout percentage's exact boundaries, and determinism across repeated evaluations.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the override count is checked in New, not in WithOverride

`WithMaxOverrides` and `WithOverride` are independent options that a caller
can mix in any order — `WithMaxOverrides(1)` might come before or after two
`WithOverride` calls. If `WithOverride` tried to reject "too many
overrides" itself, it would have to guess at a max that a *later*
`WithMaxOverrides` call might still change; checking the count against the
configured max only works once every option has had its say. That is why
this rule lives in `New`, after the option loop, unlike the schema
validator's duplicate-rule-name check earlier in this chapter — that check
only ever needed to look *backward* at rules already registered, while this
one depends on a value that can still change *forward* in the option list.

### Fixed precedence, one method

`Evaluate` never blends its three sources of truth — it checks them in a
strict order and returns from the first one that applies: an override,
if the context key has one; the fallback, if the context key is empty and
therefore cannot be hashed into a stable bucket; and only then the rollout
percentage. That ordering is what makes
`TestEvaluateOverrideWinsOverRollout` meaningful: an override set to `true`
must win even against a 0% rollout, and one set to `false` must win even
against a 100% rollout, because the override check runs before the rollout
check ever executes, not after it as a tiebreaker.

### A deterministic rollout bucket

The rollout decision is a hash of the flag's name and the context key,
reduced to a bucket in `[0, 100)` and compared against the configured
percentage. `hash/fnv` is stdlib, requires no external dependency, and —
critically — is a pure function: the same flag name and context key always
hash to the same bucket, in this run and in every future run. That is what
`TestEvaluateIsDeterministic` checks, and it is why a real feature-flag
service can roll a percentage out gradually while guaranteeing the same
user keeps seeing the same answer as the percentage climbs.

Create `flag.go`:

```go
package flag

import (
	"fmt"
	"hash/fnv"
)

// Evaluator decides whether a single feature flag is on for a given
// context key, honoring per-key overrides ahead of a percentage rollout.
type Evaluator struct {
	name              string
	rolloutPercentage int
	maxOverrides      int
	fallback          bool
	overrides         map[string]bool
}

// Option configures an Evaluator and may reject invalid input.
type Option func(*Evaluator) error

// New builds an Evaluator for name, seeding a 0% rollout, a max of 10
// overrides, and a false fallback, then applies opts. It is the single
// validation boundary for the cross-field rule no single option can see on
// its own: the number of registered overrides must not exceed the
// configured max.
func New(name string, opts ...Option) (*Evaluator, error) {
	if name == "" {
		return nil, fmt.Errorf("flag name must not be empty")
	}

	e := &Evaluator{
		name:         name,
		maxOverrides: 10,
		overrides:    make(map[string]bool),
	}
	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, err
		}
	}

	if len(e.overrides) > e.maxOverrides {
		return nil, fmt.Errorf("%d overrides exceed configured max of %d", len(e.overrides), e.maxOverrides)
	}
	return e, nil
}

// WithRolloutPercentage sets what fraction of non-overridden contexts see
// the flag on (0-100).
func WithRolloutPercentage(p int) Option {
	return func(e *Evaluator) error {
		if p < 0 || p > 100 {
			return fmt.Errorf("rollout percentage must be 0-100, got %d", p)
		}
		e.rolloutPercentage = p
		return nil
	}
}

// WithMaxOverrides sets how many per-context overrides this evaluator may
// carry (>= 0).
func WithMaxOverrides(n int) Option {
	return func(e *Evaluator) error {
		if n < 0 {
			return fmt.Errorf("max overrides must not be negative, got %d", n)
		}
		e.maxOverrides = n
		return nil
	}
}

// WithFallback sets the value returned when a context key cannot be
// evaluated against the rollout (an empty key, most commonly).
func WithFallback(v bool) Option {
	return func(e *Evaluator) error {
		e.fallback = v
		return nil
	}
}

// WithOverride forces the flag's value for a specific context key,
// bypassing the rollout percentage entirely.
func WithOverride(contextKey string, value bool) Option {
	return func(e *Evaluator) error {
		if contextKey == "" {
			return fmt.Errorf("override context key must not be empty")
		}
		e.overrides[contextKey] = value
		return nil
	}
}

// Evaluate decides whether the flag is on for contextKey: an override wins
// first, an empty context key returns the fallback (it cannot be hashed
// into a stable rollout bucket), and otherwise a deterministic hash of the
// flag name and context key assigns it to one of 100 buckets compared
// against the rollout percentage.
func (e *Evaluator) Evaluate(contextKey string) bool {
	if v, ok := e.overrides[contextKey]; ok {
		return v
	}
	if contextKey == "" {
		return e.fallback
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(e.name + ":" + contextKey))
	bucket := int(h.Sum32() % 100)
	return bucket < e.rolloutPercentage
}

// OverrideCount reports how many context-key overrides are registered.
func (e *Evaluator) OverrideCount() int { return len(e.overrides) }
```

### The runnable demo

The demo builds an evaluator at 50% rollout with two forced overrides,
shows both overrides winning regardless of the rollout, shows an empty
context returning the configured fallback, and shows a plain context key
falling through to the deterministic rollout bucket. It then shows two
overrides rejected against a max of one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-flag-evaluator-context"
)

func main() {
	evaluator, err := flag.New(
		"new-checkout",
		flag.WithRolloutPercentage(50),
		flag.WithMaxOverrides(2),
		flag.WithFallback(false),
		flag.WithOverride("user-42", true),
		flag.WithOverride("user-7", false),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("user-42 (override on): %t\n", evaluator.Evaluate("user-42"))
	fmt.Printf("user-7 (override off): %t\n", evaluator.Evaluate("user-7"))
	fmt.Printf("empty context (fallback): %t\n", evaluator.Evaluate(""))
	fmt.Printf("user-999 (rollout bucket): %t\n", evaluator.Evaluate("user-999"))

	_, err = flag.New(
		"too-many-overrides",
		flag.WithMaxOverrides(1),
		flag.WithOverride("a", true),
		flag.WithOverride("b", true),
	)
	fmt.Printf("override count exceeding max rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user-42 (override on): true
user-7 (override off): false
empty context (fallback): false
user-999 (rollout bucket): true
override count exceeding max rejected: true
```

### Tests

`TestNewValidation` tables construction failures, including both orders of
`WithMaxOverrides` relative to `WithOverride` calls, proving the count is
checked after every option has run regardless of which came first.
`TestEvaluateOverrideWinsOverRollout` proves overrides beat both a 0% and a
100% rollout. `TestEvaluateEmptyContextReturnsFallback` proves the fallback
wins over the rollout at both extremes. `TestEvaluateRolloutBoundaries`
checks 0% and 100% for a non-overridden context. `TestEvaluateIsDeterministic`
proves the same context key always evaluates the same way.
`TestOverrideCount` covers the plain accessor.

Create `flag_test.go`:

```go
package flag

import "testing"

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flag    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only", flag: "f"},
		{name: "empty flag name", flag: "", wantErr: true},
		{name: "negative rollout", flag: "f", opts: []Option{WithRolloutPercentage(-1)}, wantErr: true},
		{name: "rollout over 100", flag: "f", opts: []Option{WithRolloutPercentage(101)}, wantErr: true},
		{name: "rollout 0 is allowed", flag: "f", opts: []Option{WithRolloutPercentage(0)}},
		{name: "rollout 100 is allowed", flag: "f", opts: []Option{WithRolloutPercentage(100)}},
		{name: "negative max overrides", flag: "f", opts: []Option{WithMaxOverrides(-1)}, wantErr: true},
		{name: "empty override key", flag: "f", opts: []Option{WithOverride("", true)}, wantErr: true},
		{
			name: "overrides within max",
			flag: "f",
			opts: []Option{WithMaxOverrides(2), WithOverride("a", true), WithOverride("b", false)},
		},
		{
			name:    "overrides exceed max",
			flag:    "f",
			opts:    []Option{WithMaxOverrides(1), WithOverride("a", true), WithOverride("b", false)},
			wantErr: true,
		},
		{
			name:    "max set after overrides is still checked",
			flag:    "f",
			opts:    []Option{WithOverride("a", true), WithOverride("b", false), WithMaxOverrides(1)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.flag, tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestEvaluateOverrideWinsOverRollout(t *testing.T) {
	t.Parallel()

	e, err := New("f", WithRolloutPercentage(0), WithOverride("user-1", true))
	if err != nil {
		t.Fatal(err)
	}
	if !e.Evaluate("user-1") {
		t.Fatal("override should force the flag on even at 0% rollout")
	}

	e, err = New("f", WithRolloutPercentage(100), WithOverride("user-2", false))
	if err != nil {
		t.Fatal(err)
	}
	if e.Evaluate("user-2") {
		t.Fatal("override should force the flag off even at 100% rollout")
	}
}

func TestEvaluateEmptyContextReturnsFallback(t *testing.T) {
	t.Parallel()

	e, err := New("f", WithRolloutPercentage(100), WithFallback(false))
	if err != nil {
		t.Fatal(err)
	}
	if e.Evaluate("") {
		t.Fatal("an empty context key should return the fallback, not the rollout result")
	}

	e, err = New("f", WithRolloutPercentage(0), WithFallback(true))
	if err != nil {
		t.Fatal(err)
	}
	if !e.Evaluate("") {
		t.Fatal("an empty context key should return the fallback even at 0% rollout")
	}
}

func TestEvaluateRolloutBoundaries(t *testing.T) {
	t.Parallel()

	zero, err := New("f", WithRolloutPercentage(0))
	if err != nil {
		t.Fatal(err)
	}
	if zero.Evaluate("any-user") {
		t.Fatal("0% rollout should never turn the flag on for a non-overridden context")
	}

	full, err := New("f", WithRolloutPercentage(100))
	if err != nil {
		t.Fatal(err)
	}
	if !full.Evaluate("any-user") {
		t.Fatal("100% rollout should always turn the flag on for a non-overridden context")
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	t.Parallel()

	e, err := New("f", WithRolloutPercentage(50))
	if err != nil {
		t.Fatal(err)
	}
	first := e.Evaluate("stable-user")
	for i := 0; i < 10; i++ {
		if e.Evaluate("stable-user") != first {
			t.Fatal("Evaluate should return the same result for the same context key every time")
		}
	}
}

func TestOverrideCount(t *testing.T) {
	t.Parallel()

	e, err := New("f", WithOverride("a", true), WithOverride("b", false))
	if err != nil {
		t.Fatal(err)
	}
	if e.OverrideCount() != 2 {
		t.Fatalf("OverrideCount() = %d, want 2", e.OverrideCount())
	}
}
```

## Review

The evaluator is correct when its three sources of truth never blend and
always check in the same fixed order, and when the one cross-field rule
this configuration has — override count against max — is checked exactly
once, after every option has run, rather than guessed at mid-registration.
That second point matters because, unlike the schema validator's
duplicate-name check, this rule genuinely depends on a value (`maxOverrides`)
that a later option can still change; there is no "look backward only"
shortcut available here, so the constructor-boundary pattern used
throughout this chapter is the only correct place for it. The deterministic
hash-based rollout is what makes a percentage rollout meaningful in
practice: the same user needs to see the same answer today and tomorrow,
not a new coin flip on every request.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [hash/fnv](https://pkg.go.dev/hash/fnv)
- [LaunchDarkly: percentage rollouts](https://docs.launchdarkly.com/home/flags/rollouts)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-full-text-search-analyzer.md](30-full-text-search-analyzer.md) | Next: [32-transaction-isolation-levels.md](32-transaction-isolation-levels.md)
