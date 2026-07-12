# Exercise 34: JSON Schema Field Validator with Rule Aggregation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A decoded JSON request body — `map[string]any`, exactly what
`encoding/json` produces when you unmarshal into `any` — needs field-level
validation: required strings, integers within range, the usual schema
constraints. `ValidateSchema(doc, rules...)` runs a variadic list of
`FieldRule`s against the document and aggregates every violation into one
structured error, including the case that trips up validators that were
only ever tested against well-formed input: a `nil` document.

## What you'll build

```text
schemaval/                 independent module: example.com/schemaval
  go.mod                   go 1.24
  schemaval.go             package schemaval; type Doc map[string]any; type FieldError struct{Field, Msg string}; type FieldRule func(Doc) error; RequiredString, IntRange; ValidateSchema(doc Doc, rules ...FieldRule) error
  cmd/
    demo/
      main.go              runnable demo: three simultaneous violations, errors.As recovery, and a nil doc
  schemaval_test.go         table of cases (valid, missing, wrong type, out of range, zero rules) + aggregation test + nil-doc edge case
```

- Files: `schemaval.go`, `cmd/demo/main.go`, `schemaval_test.go`.
- Implement: `type Doc map[string]any`, `type FieldError struct{ Field, Msg string }` with an `Error() string` method, `type FieldRule func(Doc) error`, constructors `RequiredString(field string) FieldRule` and `IntRange(field string, min, max int) FieldRule`, and `ValidateSchema(doc Doc, rules ...FieldRule) error`.
- Test: a table covering all-valid, missing field, wrong type, out-of-range, and zero-rules cases; a doc failing three rules at once returns one error mentioning all three fields, recoverable as a `*FieldError` via `errors.As`; a `nil` doc is handled safely (every field reads as absent, not a panic).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a typed `*FieldError`, and why `nil` needs its own thought

Earlier validation exercises in this chapter returned plain
`fmt.Errorf`-built errors, which is fine when the only consumer is a log
line or an HTTP response body built from `err.Error()`. A JSON schema
validator is more often consumed by code that wants to render one message
*per field* in an API error response (`{"errors": {"age": "out of range
[0, 130]"}}`), which needs structured access to `Field` and `Msg`
separately, not a string to parse back apart. `FieldError` is a proper
error type (`*FieldError` implements `error` via its `Error() string`
method), and because `errors.Join` preserves the tree of joined errors
rather than flattening them into one opaque string, `errors.As(err,
&fieldErr)` can recover the *first* `*FieldError` in that tree — and a
caller that wants all of them walks the tree via the `Unwrap() []error`
method `errors.Join`'s result exposes, exactly as in the payload-validation
exercise earlier in this chapter.

The `nil`-document edge case is worth calling out because it is easy to
assume "no document" is a distinct error condition that needs special
handling, when in Go it mostly isn't: reading `d[field]` on a `nil` map is
completely safe and returns the zero value with `ok == false`, the same
as reading a missing key from a non-nil map. `ValidateSchema(nil,
RequiredString("name"))` therefore does not need a nil-check at all — it
naturally produces "name: required field missing," which is exactly the
right answer, but it is exactly the kind of behavior a test suite should
pin explicitly rather than leave to accident, since a *different*
implementation choice (say, iterating the map's keys to check something)
could easily introduce a nil-map-specific panic that only a nil-input
test would ever catch.

Create `schemaval.go`:

```go
// schemaval.go
package schemaval

import (
	"errors"
	"fmt"
)

// Doc is a decoded JSON object: field name to decoded value (string,
// float64, bool, map[string]any, []any, or nil, per encoding/json's
// default unmarshaling into any).
type Doc map[string]any

// FieldError reports which field violated a schema rule and why. Callers
// that need structured access (to render one message per field, say) can
// recover the original *FieldError values with errors.As.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Msg)
}

// FieldRule inspects a Doc and returns a *FieldError (or nil) describing
// one schema violation.
type FieldRule func(Doc) error

// ValidateSchema runs every rule against doc and aggregates every
// resulting error with errors.Join, so a document violating several rules
// at once reports all of them in one result. A nil Doc is valid input:
// every rule simply sees every field as absent.
func ValidateSchema(doc Doc, rules ...FieldRule) error {
	var errs []error
	for _, rule := range rules {
		if err := rule(doc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RequiredString returns a FieldRule that fails unless field is present
// and holds a non-empty string.
func RequiredString(field string) FieldRule {
	return func(d Doc) error {
		v, ok := d[field]
		if !ok {
			return &FieldError{Field: field, Msg: "required field missing"}
		}
		s, ok := v.(string)
		if !ok {
			return &FieldError{Field: field, Msg: fmt.Sprintf("expected string, got %T", v)}
		}
		if s == "" {
			return &FieldError{Field: field, Msg: "must not be empty"}
		}
		return nil
	}
}

// IntRange returns a FieldRule that fails unless field is present, holds
// a JSON number, and that number (truncated to int) falls within
// [min, max] inclusive. A missing field is not this rule's concern (pair
// it with a required-field rule to cover that case explicitly).
func IntRange(field string, min, max int) FieldRule {
	return func(d Doc) error {
		v, ok := d[field]
		if !ok {
			return nil
		}
		n, ok := v.(float64) // encoding/json decodes JSON numbers as float64
		if !ok {
			return &FieldError{Field: field, Msg: fmt.Sprintf("expected number, got %T", v)}
		}
		i := int(n)
		if i < min || i > max {
			return &FieldError{Field: field, Msg: fmt.Sprintf("value %d out of range [%d, %d]", i, min, max)}
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
	"errors"
	"fmt"

	"example.com/schemaval"
)

func main() {
	doc := schemaval.Doc{
		"name": "",
		"age":  float64(200),
	}
	err := schemaval.ValidateSchema(doc,
		schemaval.RequiredString("name"),
		schemaval.RequiredString("email"),
		schemaval.IntRange("age", 0, 130),
	)
	fmt.Println(err)

	var fe *schemaval.FieldError
	if errors.As(err, &fe) {
		fmt.Printf("first field error via errors.As: field=%q msg=%q\n", fe.Field, fe.Msg)
	}

	fmt.Println("nil doc:", schemaval.ValidateSchema(nil, schemaval.RequiredString("name")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name: must not be empty
email: required field missing
age: value 200 out of range [0, 130]
first field error via errors.As: field="name" msg="must not be empty"
nil doc: name: required field missing
```

### Tests

`TestValidateSchemaTableOfCases` is the case table: valid input, a missing
field, a wrong-typed field, an out-of-range integer, and zero rules, each
asserting only whether an error occurred. `TestValidateSchemaNilDocIsHandledSafely`
is the edge case: it calls `ValidateSchema(nil, ...)` directly and asserts
it returns a normal `*FieldError`-bearing error rather than panicking.

Create `schemaval_test.go`:

```go
// schemaval_test.go
package schemaval

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSchemaTableOfCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		doc     Doc
		rules   []FieldRule
		wantErr bool
	}{
		{
			name:    "all valid",
			doc:     Doc{"name": "ana", "age": float64(30)},
			rules:   []FieldRule{RequiredString("name"), IntRange("age", 0, 130)},
			wantErr: false,
		},
		{
			name:    "empty string fails required",
			doc:     Doc{"name": ""},
			rules:   []FieldRule{RequiredString("name")},
			wantErr: true,
		},
		{
			name:    "missing field fails required",
			doc:     Doc{},
			rules:   []FieldRule{RequiredString("name")},
			wantErr: true,
		},
		{
			name:    "wrong type fails required",
			doc:     Doc{"name": float64(5)},
			rules:   []FieldRule{RequiredString("name")},
			wantErr: true,
		},
		{
			name:    "out of range int",
			doc:     Doc{"age": float64(200)},
			rules:   []FieldRule{IntRange("age", 0, 130)},
			wantErr: true,
		},
		{
			name:    "missing field ok for IntRange alone",
			doc:     Doc{},
			rules:   []FieldRule{IntRange("age", 0, 130)},
			wantErr: false,
		},
		{
			name:    "zero rules always valid",
			doc:     Doc{"anything": "goes"},
			rules:   nil,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSchema(tc.doc, tc.rules...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSchema() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateSchemaAggregatesMultipleViolations(t *testing.T) {
	t.Parallel()

	doc := Doc{"name": "", "age": float64(200)}
	err := ValidateSchema(doc,
		RequiredString("name"),
		RequiredString("email"),
		IntRange("age", 0, 130),
	)
	if err == nil {
		t.Fatal("expected an aggregated error")
	}

	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatal("expected errors.As to recover a *FieldError")
	}

	msg := err.Error()
	for _, field := range []string{"name", "email", "age"} {
		if !strings.Contains(msg, field) {
			t.Errorf("aggregated error %q missing violation for field %q", msg, field)
		}
	}
}

func TestValidateSchemaNilDocIsHandledSafely(t *testing.T) {
	t.Parallel()

	err := ValidateSchema(nil, RequiredString("name"), IntRange("age", 0, 130))
	if err == nil {
		t.Fatal("expected an error: nil doc is missing every field")
	}
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatal("expected a *FieldError")
	}
}
```

## Review

`ValidateSchema` is correct when every rule that fails contributes a
distinct, recoverable `*FieldError` to the joined result, a `nil` document
is treated exactly like an all-fields-absent document rather than a
special case, and a fully valid document returns `nil`. The senior points
are making the error type structured (`*FieldError` with named `Field`
and `Msg` fields, recoverable via `errors.As`) rather than a bag of
strings, and testing `nil` input explicitly rather than trusting that Go's
"reading a missing key from a nil map is safe" behavior will keep holding
as the implementation evolves — a future rule that ranges over `doc`'s
keys instead of indexing into it would behave identically for a `nil` map
(ranging over `nil` is also safe and yields zero iterations), but a rule
that called `len(doc)` and divided by it would not, and only a targeted
nil-input test catches that kind of regression before production does.

## Resources

- [`encoding/json`: decoding into `any`](https://pkg.go.dev/encoding/json#Unmarshal)
- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [`errors.As`](https://pkg.go.dev/errors#As)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-header-dedup-merge-preserve-order.md](33-header-dedup-merge-preserve-order.md) | Next: [35-env-config-merge-with-precedence.md](35-env-config-merge-with-precedence.md)
