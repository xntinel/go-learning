# Exercise 28: Payload Validation Rule Engine with Aggregation

**Nivel: Intermedio** — validacion rapida (un test corto).

A signup form with three broken fields should tell the user about all
three in one response, not make them fix one, resubmit, discover the
second, fix it, resubmit again. `Validate(payload, rules...)` runs every
rule against the same payload and joins every failure into one error,
because a validation engine's job is to describe everything wrong with
the input in a single pass, not to short-circuit at the first problem it
happens to notice.

## What you'll build

```text
payloadval/                independent module: example.com/payloadval
  go.mod                   go 1.24
  payloadval.go            package payloadval; type Payload map[string]any; type Rule func(Payload) error; RequireField, MaxLen; Validate(p Payload, rules ...Rule) error
  cmd/
    demo/
      main.go              runnable demo: a valid payload, then one with three simultaneous violations
  payloadval_test.go        table tests: all-pass, aggregated multi-violation, zero rules
```

- Files: `payloadval.go`, `cmd/demo/main.go`, `payloadval_test.go`.
- Implement: `type Payload map[string]any`, `type Rule func(Payload) error`, constructors `RequireField(field string) Rule` and `MaxLen(field string, max int) Rule`, and `Validate(p Payload, rules ...Rule) error`.
- Test: a payload passing every rule returns `nil`; a payload failing three rules at once returns one error mentioning all three fields, not just the first; zero rules is always valid.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why aggregation instead of stop-at-first, and how errors.Join makes it structured

Contrast this with a pipeline where each stage depends on the previous
one succeeding (like the fee accumulator's `TotalFee`, which must stop at
the first bad rule because a partial sum is a wrong sum) — here each rule
is checking an *independent* fact about the same payload, so there is
nothing invalid about continuing to run `MaxLen` after `RequireField` has
already failed. Stopping early would mean a user who forgot both
`username` and `email` only ever learns about `username`, fixes it,
resubmits, and only then learns about `email`. `Validate` instead collects
every rule's error into a slice and hands it to `errors.Join`, which
returns a single `error` whose `.Error()` string lists each cause on its
own line, and whose `Unwrap() []error` lets a caller that wants structured
access — say, to render one message per form field — recover the original
list with `errors.As` against a `interface{ Unwrap() []error }` target,
rather than string-parsing the combined message.

`RequireField` and `MaxLen` are deliberately narrow: `RequireField` only
checks presence and non-emptiness, `MaxLen` only checks length and is a
no-op on a missing or non-string field. This narrowness is the point — a
rule engine's rules should each fail for exactly one reason, so composing
`RequireField("bio")` and `MaxLen("bio", 500)` produces two independent,
individually-testable and individually-readable failure messages instead
of one rule that tries to check "present, right type, and right length"
and reports a single message that conflates all three.

Create `payloadval.go`:

```go
// payloadval.go
package payloadval

import (
	"errors"
	"fmt"
)

// Payload is a decoded request body: field name to value.
type Payload map[string]any

// Rule inspects a payload and returns an error describing one constraint
// violation, or nil if the payload satisfies the rule.
type Rule func(Payload) error

// Validate runs every rule against p and aggregates all of their errors
// with errors.Join, rather than stopping at the first failure. A caller
// filling out a form wants every violation reported at once, not one
// error per submit-fix-resubmit cycle. Validate returns nil only if every
// rule passes.
func Validate(p Payload, rules ...Rule) error {
	var errs []error
	for _, rule := range rules {
		if err := rule(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RequireField returns a Rule that fails if field is absent or its value
// is the empty string.
func RequireField(field string) Rule {
	return func(p Payload) error {
		v, ok := p[field]
		if !ok {
			return fmt.Errorf("%s: required field missing", field)
		}
		if s, ok := v.(string); ok && s == "" {
			return fmt.Errorf("%s: required field is empty", field)
		}
		return nil
	}
}

// MaxLen returns a Rule that fails if field's string value is longer than
// max characters. A missing field, or a field that is not a string, is
// not this rule's concern and passes (pair it with RequireField and a
// type-specific rule to cover those cases explicitly).
func MaxLen(field string, max int) Rule {
	return func(p Payload) error {
		v, ok := p[field]
		if !ok {
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return nil
		}
		if len(s) > max {
			return fmt.Errorf("%s: length %d exceeds max %d", field, len(s), max)
		}
		return nil
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/payloadval"
)

func main() {
	good := payloadval.Payload{"username": "ana", "bio": "backend engineer"}
	if err := payloadval.Validate(good,
		payloadval.RequireField("username"),
		payloadval.MaxLen("bio", 100),
	); err != nil {
		fmt.Println("unexpected error:", err)
	} else {
		fmt.Println("good payload: valid")
	}

	bad := payloadval.Payload{"username": "", "bio": "this bio is way too long for the limit"}
	err := payloadval.Validate(bad,
		payloadval.RequireField("username"),
		payloadval.RequireField("email"),
		payloadval.MaxLen("bio", 10),
	)
	fmt.Println("bad payload errors:")
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good payload: valid
bad payload errors:
username: required field is empty
email: required field missing
bio: length 38 exceeds max 10
```

### Tests

`TestValidateAggregatesAllViolations` is the one that matters: it feeds
in a payload failing all three rules and asserts the combined error
message mentions every field, then uses `errors.As` against an
`Unwrap() []error` target to confirm exactly three distinct errors were
joined — not one, not a string concatenation that happens to contain the
right substrings.

Create `payloadval_test.go`:

```go
// payloadval_test.go
package payloadval

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAllPassesReturnsNil(t *testing.T) {
	t.Parallel()

	p := Payload{"username": "ana", "bio": "short"}
	err := Validate(p, RequireField("username"), MaxLen("bio", 100))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAggregatesAllViolations(t *testing.T) {
	t.Parallel()

	p := Payload{"username": "", "bio": "way too long for the limit set"}
	err := Validate(p,
		RequireField("username"),
		RequireField("email"),
		MaxLen("bio", 10),
	)
	if err == nil {
		t.Fatal("expected an aggregated error")
	}

	msg := err.Error()
	for _, want := range []string{"username", "email", "bio"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error %q missing mention of %q", msg, want)
		}
	}

	if got := len(unwrapAll(err)); got != 3 {
		t.Fatalf("got %d joined errors, want 3", got)
	}
}

func TestValidateZeroRulesIsValid(t *testing.T) {
	t.Parallel()

	if err := Validate(Payload{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func unwrapAll(err error) []error {
	type multi interface{ Unwrap() []error }
	var m multi
	if errors.As(err, &m) {
		return m.Unwrap()
	}
	return []error{err}
}
```

## Review

`Validate` is correct when every rule runs regardless of earlier
failures, `nil` results are simply skipped, and a `nil` overall result
means every single rule passed — not "the first rule passed." The senior
point is recognizing which shape a given aggregation problem needs:
independent checks over the same input (here) want `errors.Join` and full
aggregation, while a dependent pipeline where each step's output feeds
the next (`TotalFee` from the transaction fee exercise) wants stop-at-first
because a partial result there would be actively misleading. Reaching for
`errors.Join` by default without checking which one applies is the
mistake — it would silently turn a validated-but-wrong partial total into
something that looks like a real number.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [`errors.As`](https://pkg.go.dev/errors#As)
- [Go blog: Working with errors in Go 1.13](https://go.dev/blog/go1.13-errors)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-batch-collector-size-trigger.md](27-batch-collector-size-trigger.md) | Next: [29-schema-migration-sequencer-graph.md](29-schema-migration-sequencer-graph.md)
