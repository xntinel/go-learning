# Exercise 28: JSON Schema Validator With Custom Rule Composition and Precedence

**Nivel: Intermedio** — validacion rapida (un test corto).

A validator that lets callers register their own rules on top of a required
field list has two things to get right: no two rules can silently shadow
each other under the same name, and a document missing a required field
should be reported before any custom rule even runs — there is no point
telling a caller their email format is wrong on a document that never had an
email field's sibling `user_id` in the first place. This module builds that
validator with functional options.

## What you'll build

```text
schema/                          independent module: example.com/schema-validator-rule-engine
  go.mod                         go 1.24
  validator.go                   Validator, Option, New, WithRequiredFields, WithRule,
                                  WithMode, Validate, RuleNames
  cmd/
    demo/
      main.go                    collect-all vs fail-fast against the same invalid document
  validator_test.go               option-validation table, precedence, mode, and ordering tests
```

- Files: `validator.go`, `cmd/demo/main.go`, `validator_test.go`.
- Implement: a `Validator` built by `New(opts ...Option) (*Validator, error)` whose `WithRule` rejects a duplicate rule name at registration time and whose `Validate` always checks required fields before any custom rule, honoring a "fail-fast" or "collect-all" mode.
- Test: every option-validation case including a duplicate rule name, required-field precedence over custom rules, both validation modes, a fully passing document, and rule-name ordering.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/28-schema-validator-rule-engine/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/28-schema-validator-rule-engine
go mod edit -go=1.24
```

### Catching a duplicate rule name without a second pass

Every other cross-field check in this chapter waits until `New` has applied
every option, because two independent options can't see each other's
values while either one runs. `WithRule` is different: functional options
apply strictly in the order they were passed to `New`, which means that by
the time the *second* `WithRule("email_format", ...)` call's closure runs,
the *first* one has already appended its rule to `v.rules`. `WithRule` can
therefore check for a duplicate name against `v.rules` directly, inside its
own closure, and reject it the instant it happens — no separate pass over
the finished `Validator` is needed. This only works because rule
registration is inherently ordered and cumulative; it would not work for two
options that each own a single, independent field (like the timeout and
interval on the load balancer earlier in this chapter).

### Why required fields are checked first

`Validate` runs two loops: required fields, then custom rules, always in
that order — never interleaved, and never rules first. That ordering means
a document missing a required field is reported before any custom rule
callback executes at all, which matters most in fail-fast mode: if rules ran
first, a document missing `user_id` but otherwise triggering a completely
unrelated `age_range` rule violation would report the wrong problem first
and stop there, hiding the missing field entirely. Putting required-field
checks in their own loop, ahead of the rules loop, makes "required fields
first" true by construction rather than by convention that a future rule
addition could accidentally violate.

Create `validator.go`:

```go
package schema

import (
	"errors"
	"fmt"
)

// rule is a named custom validation function.
type rule struct {
	name string
	fn   func(map[string]any) error
}

// Validator checks a document against a set of required fields and custom
// rules, either failing on the first violation or collecting every one.
type Validator struct {
	requiredFields []string
	rules          []rule
	mode           string // "fail-fast" or "collect-all"
}

// Option configures a Validator and may reject invalid input.
type Option func(*Validator) error

// New builds a Validator, defaulting to "collect-all" mode with no required
// fields and no rules, then applies opts in order.
func New(opts ...Option) (*Validator, error) {
	v := &Validator{mode: "collect-all"}
	for _, opt := range opts {
		if err := opt(v); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// WithRequiredFields adds field names that must be present in any document
// passed to Validate.
func WithRequiredFields(fields ...string) Option {
	return func(v *Validator) error {
		for _, f := range fields {
			if f == "" {
				return fmt.Errorf("required field name must not be empty")
			}
			v.requiredFields = append(v.requiredFields, f)
		}
		return nil
	}
}

// WithRule registers a named custom validation function. Because options
// apply in order, every rule already registered by an earlier WithRule call
// is present in v.rules by the time this option runs, so a duplicate name
// is caught immediately rather than needing a second pass after New applies
// every option.
func WithRule(name string, fn func(map[string]any) error) Option {
	return func(v *Validator) error {
		if name == "" {
			return fmt.Errorf("rule name must not be empty")
		}
		if fn == nil {
			return fmt.Errorf("rule %q has a nil check function", name)
		}
		for _, r := range v.rules {
			if r.name == name {
				return fmt.Errorf("duplicate rule name: %q", name)
			}
		}
		v.rules = append(v.rules, rule{name: name, fn: fn})
		return nil
	}
}

// WithMode sets the validation mode: "fail-fast" stops at the first
// violation, "collect-all" runs every check and reports every violation.
func WithMode(mode string) Option {
	return func(v *Validator) error {
		if mode != "fail-fast" && mode != "collect-all" {
			return fmt.Errorf("unsupported mode: %q", mode)
		}
		v.mode = mode
		return nil
	}
}

// Validate checks data against the required fields first, then against
// every custom rule in registration order. Required fields are checked
// first so a missing field is always reported before any rule violation,
// and — in fail-fast mode — before any rule even runs. It returns nil if
// data passes every check, one joined error listing every violation in
// collect-all mode, or the single first violation in fail-fast mode.
func (v *Validator) Validate(data map[string]any) error {
	var errs []error

	for _, field := range v.requiredFields {
		if _, ok := data[field]; !ok {
			errs = append(errs, fmt.Errorf("required field missing: %s", field))
			if v.mode == "fail-fast" {
				return errors.Join(errs...)
			}
		}
	}

	for _, r := range v.rules {
		if err := r.fn(data); err != nil {
			errs = append(errs, fmt.Errorf("rule %q failed: %w", r.name, err))
			if v.mode == "fail-fast" {
				return errors.Join(errs...)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// RuleNames returns the registered custom rule names in registration order.
func (v *Validator) RuleNames() []string {
	names := make([]string, len(v.rules))
	for i, r := range v.rules {
		names[i] = r.name
	}
	return names
}
```

### The runnable demo

The demo builds two validators from the same options apart from mode, feeds
both the same invalid document, and shows collect-all reporting all three
violations while fail-fast stops at the first (the missing required field,
ahead of either custom rule).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/schema-validator-rule-engine"
)

func emailRule(data map[string]any) error {
	email, _ := data["email"].(string)
	if email != "" && !strings.Contains(email, "@") {
		return fmt.Errorf("email %q is missing @", email)
	}
	return nil
}

func ageRule(data map[string]any) error {
	age, ok := data["age"].(int)
	if ok && (age < 0 || age > 150) {
		return fmt.Errorf("age %d is out of range 0-150", age)
	}
	return nil
}

func main() {
	collectAll, err := schema.New(
		schema.WithRequiredFields("user_id", "email"),
		schema.WithRule("email_format", emailRule),
		schema.WithRule("age_range", ageRule),
		schema.WithMode("collect-all"),
	)
	if err != nil {
		panic(err)
	}

	badDoc := map[string]any{"email": "not-an-email", "age": 200}
	if err := collectAll.Validate(badDoc); err != nil {
		fmt.Printf("collect-all found: %v\n", err)
	}

	failFast, err := schema.New(
		schema.WithRequiredFields("user_id", "email"),
		schema.WithRule("email_format", emailRule),
		schema.WithRule("age_range", ageRule),
		schema.WithMode("fail-fast"),
	)
	if err != nil {
		panic(err)
	}
	if err := failFast.Validate(badDoc); err != nil {
		fmt.Printf("fail-fast stopped at: %v\n", err)
	}

	goodDoc := map[string]any{"user_id": "u1", "email": "a@b.com", "age": 30}
	fmt.Printf("valid document error: %v\n", collectAll.Validate(goodDoc))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
collect-all found: required field missing: user_id
rule "email_format" failed: email "not-an-email" is missing @
rule "age_range" failed: age 200 is out of range 0-150
fail-fast stopped at: required field missing: user_id
valid document error: <nil>
```

### Tests

`TestNewValidation` tables construction failures, including the duplicate
rule name. `TestValidateChecksRequiredFieldsFirst` proves a fail-fast
validator reports the missing field and never runs the rule that would also
have failed. `TestValidateFailFastStopsAtFirstViolation` proves fail-fast
stops after the first rule, never reaching the second.
`TestValidateCollectAllReportsEveryViolation` proves collect-all reports
all three. `TestValidatePassesCleanDocument` is the happy path.
`TestRuleNamesPreservesRegistrationOrder` guards the ordering `WithRule`'s
duplicate check depends on.

Create `validator_test.go`:

```go
package schema

import (
	"fmt"
	"strings"
	"testing"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	noop := func(map[string]any) error { return nil }

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "empty required field", opts: []Option{WithRequiredFields("")}, wantErr: true},
		{name: "empty rule name", opts: []Option{WithRule("", noop)}, wantErr: true},
		{name: "nil rule fn", opts: []Option{WithRule("r1", nil)}, wantErr: true},
		{
			name:    "duplicate rule name",
			opts:    []Option{WithRule("r1", noop), WithRule("r1", noop)},
			wantErr: true,
		},
		{
			name: "distinct rule names",
			opts: []Option{WithRule("r1", noop), WithRule("r2", noop)},
		},
		{name: "unsupported mode", opts: []Option{WithMode("best-effort")}, wantErr: true},
		{name: "fail-fast mode", opts: []Option{WithMode("fail-fast")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func alwaysFail(msg string) func(map[string]any) error {
	return func(map[string]any) error { return fmt.Errorf("%s", msg) }
}

func TestValidateChecksRequiredFieldsFirst(t *testing.T) {
	t.Parallel()

	v, err := New(
		WithRequiredFields("id"),
		WithRule("always_fails", alwaysFail("rule violation")),
		WithMode("fail-fast"),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = v.Validate(map[string]any{})
	if err == nil {
		t.Fatal("expected an error for a missing required field")
	}
	if !strings.Contains(err.Error(), "required field missing: id") {
		t.Fatalf("error = %v, want it to report the missing required field first", err)
	}
	if strings.Contains(err.Error(), "rule violation") {
		t.Fatalf("error = %v, fail-fast should have stopped before running any rule", err)
	}
}

func TestValidateFailFastStopsAtFirstViolation(t *testing.T) {
	t.Parallel()

	v, err := New(
		WithRule("r1", alwaysFail("first")),
		WithRule("r2", alwaysFail("second")),
		WithMode("fail-fast"),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = v.Validate(map[string]any{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "first") || strings.Contains(err.Error(), "second") {
		t.Fatalf("error = %v, want only the first rule's violation", err)
	}
}

func TestValidateCollectAllReportsEveryViolation(t *testing.T) {
	t.Parallel()

	v, err := New(
		WithRequiredFields("id"),
		WithRule("r1", alwaysFail("first")),
		WithRule("r2", alwaysFail("second")),
		WithMode("collect-all"),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = v.Validate(map[string]any{})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"required field missing: id", "first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestValidatePassesCleanDocument(t *testing.T) {
	t.Parallel()

	v, err := New(
		WithRequiredFields("id"),
		WithRule("always_pass", func(map[string]any) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(map[string]any{"id": "u1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRuleNamesPreservesRegistrationOrder(t *testing.T) {
	t.Parallel()

	v, err := New(
		WithRule("first", func(map[string]any) error { return nil }),
		WithRule("second", func(map[string]any) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	names := v.RuleNames()
	if len(names) != 2 || names[0] != "first" || names[1] != "second" {
		t.Fatalf("RuleNames() = %v, want [first second]", names)
	}
}
```

## Review

The validator is correct when a duplicate rule name can never sneak into
`v.rules` — because `WithRule` checks the accumulated list at the moment it
applies, not after the fact — and when a caller can trust that
`required field missing` always shows up before any rule-specific message,
in either mode. That second guarantee comes from structure, not a
special case: `Validate` simply never starts its rules loop until its
required-fields loop has finished. The fail-fast/collect-all split is the
same shape as any other early-return-versus-accumulate decision — a fixed
`mode` field checked at every violation site, rather than two different
code paths that could drift apart.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [JSON Schema specification](https://json-schema.org/specification)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-user-session-storage-backend.md](27-user-session-storage-backend.md) | Next: [29-distributed-cache-replication.md](29-distributed-cache-replication.md)
