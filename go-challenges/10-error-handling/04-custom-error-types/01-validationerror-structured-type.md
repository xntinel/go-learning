# Exercise 1: A ValidationError Type That Carries Field, Value, and Rule

Config validation is the first place a service turns hostile input into a precise,
actionable failure. This module builds a `Config.Validate()` that returns a
`*ValidationError` carrying the field, the offending value, the rule that failed,
and a wrapped sentinel — so a caller can `errors.Is` the category, `errors.As` the
typed error to read its fields, and match by field. It is the foundational
custom-error-type artifact the rest of the chapter builds on.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
configval/                 independent module: example.com/configval
  go.mod                   go 1.24
  validate.go              ValidationError{Field,Value,Rule,Err}; Config.Validate
  cmd/
    demo/
      main.go              validates a bad config, prints the typed fields
  validate_test.go         errors.Is/As table, Is-by-field, message pin
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: sentinels `ErrRequired`/`ErrInvalid`/`ErrTooShort`/`ErrTooLong`, a `*ValidationError` with `Error()`/`Unwrap()`/`Is()`, and `Config.Validate()`.
Test: valid config returns nil; each invalid field asserts `errors.Is` the right sentinel; `errors.As` extracts the typed error; `Is`-by-field matches; message contains the field.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/04-custom-error-types/01-validationerror-structured-type/cmd/demo
cd go-solutions/10-error-handling/04-custom-error-types/01-validationerror-structured-type
go mod edit -go=1.24
```

### Why the pointer receiver is mandatory here

`Error()` is declared on `*ValidationError`, not `ValidationError`. That is a
deliberate contract, not a style tic. With a pointer receiver, only
`*ValidationError` satisfies `error`, so the concrete type that lives in every
returned error's chain is unambiguously `*ValidationError`. When a caller writes
`var ve *ValidationError; errors.As(err, &ve)`, the target `**ValidationError`
points at exactly the type in the chain, and the assignment succeeds. Switch to a
value receiver and both `ValidationError` and `*ValidationError` satisfy `error`;
identity and comparability blur, and `errors.As` into a `*ValidationError` target
can silently fail to match a value wrapped in the chain. The next module studies
that failure mode empirically; here we simply take the safe default.

### Why the type wraps a sentinel instead of only a string

Each `ValidationError` carries `Err`, a wrapped package-level sentinel
(`ErrRequired`, `ErrInvalid`, `ErrTooShort`, `ErrTooLong`), and `Unwrap()`
returns it. This gives callers two independent axes of inspection. `errors.Is(err,
ErrRequired)` answers the *category* question ("is this a missing-required-value
failure?") without caring which field; `errors.As(err, &ve)` then answers the
*specifics* ("which field, what rule, what value?"). If the type held only a
formatted string, the caller would be reduced to `strings.Contains(err.Error(),
"required")` — brittle, untestable, and locale-fragile. The sentinel is the
machine-readable category; the struct fields are the structured detail; the
message is only for humans.

### The custom Is matches by field

`ValidationError.Is` returns true when the target is a `*ValidationError` with the
same `Field`. This makes `errors.Is(err, &ValidationError{Field: "Host"})` a
"did any Host-field rule fail?" query. Note the deliberate narrowness: it matches
on `Field` and nothing else, so it does not swallow unrelated validation errors on
other fields. A custom `Is` that returned true for *any* `*ValidationError` would
make every `errors.Is` against the type succeed and silently hide real mismatches
— the broadening bug the concepts file warns about.

Create `validate.go`:

```go
// Package configval validates a service Config and reports each failure as a
// *ValidationError carrying the field, value, rule, and a wrapped sentinel.
package configval

import (
	"errors"
	"fmt"
)

// Sentinels name the category of a validation failure. Callers match them with
// errors.Is without depending on the human-readable message.
var (
	ErrRequired = errors.New("required")
	ErrInvalid  = errors.New("invalid")
	ErrTooShort = errors.New("too short")
	ErrTooLong  = errors.New("too long")
)

// ValidationError is a structured validation failure. Field/Value/Rule are the
// machine-readable detail; Err is the wrapped sentinel category.
type ValidationError struct {
	Field string
	Value string
	Rule  string
	Err   error
}

// Error formats the structured fields for humans and logs. It is a view of the
// data, never the interface callers program against.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed: field=%q value=%q rule=%s (%s)",
		e.Field, e.Value, e.Rule, e.Err)
}

// Unwrap exposes the wrapped sentinel so errors.Is(err, ErrRequired) works.
func (e *ValidationError) Unwrap() error { return e.Err }

// Is defines a category match by Field: errors.Is(err, &ValidationError{Field:
// "Host"}) is true for any Host-field failure. It matches on Field ONLY, so it
// does not swallow failures on other fields.
func (e *ValidationError) Is(target error) bool {
	t, ok := target.(*ValidationError)
	if !ok {
		return false
	}
	return e.Field == t.Field
}

// Config is a minimal service configuration.
type Config struct {
	Host string
	Port int
	Name string
}

// Validate returns the first failure as a *ValidationError, or nil when the
// config is valid. It is fail-fast; Exercise 5 shows the collect-all variant.
func (c *Config) Validate() error {
	if c.Host == "" {
		return &ValidationError{Field: "Host", Value: "", Rule: "required", Err: ErrRequired}
	}
	if c.Port <= 0 || c.Port > 65535 {
		return &ValidationError{Field: "Port", Value: fmt.Sprintf("%d", c.Port), Rule: "range", Err: ErrInvalid}
	}
	if len(c.Name) < 3 {
		return &ValidationError{Field: "Name", Value: c.Name, Rule: "min_len_3", Err: ErrTooShort}
	}
	if len(c.Name) > 50 {
		return &ValidationError{Field: "Name", Value: c.Name, Rule: "max_len_50", Err: ErrTooLong}
	}
	return nil
}
```

### The runnable demo

The demo validates a config with a missing host, then extracts the typed error to
read its structured fields — the exact thing a string error could not offer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/configval"
)

func main() {
	cfg := &configval.Config{Host: "", Port: 8080, Name: "billing"}

	err := cfg.Validate()
	if err == nil {
		fmt.Println("config valid")
		return
	}

	// Category axis: is this a required-value failure?
	fmt.Printf("is required error: %v\n", errors.Is(err, configval.ErrRequired))

	// Detail axis: which field and rule failed?
	var ve *configval.ValidationError
	if errors.As(err, &ve) {
		fmt.Printf("field=%s rule=%s\n", ve.Field, ve.Rule)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
is required error: true
field=Host rule=required
```

### Tests

The table drives every invalid field to its sentinel with `errors.Is`, then the
typed tests prove `errors.As` extraction, the by-field `Is`, and that the message
carries the field name.

Create `validate_test.go`:

```go
package configval

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateCategories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error // nil means valid
	}{
		{"valid", Config{Host: "localhost", Port: 8080, Name: "billing"}, nil},
		{"missing host", Config{Host: "", Port: 8080, Name: "billing"}, ErrRequired},
		{"port too high", Config{Host: "h", Port: 70000, Name: "billing"}, ErrInvalid},
		{"port zero", Config{Host: "h", Port: 0, Name: "billing"}, ErrInvalid},
		{"name too short", Config{Host: "h", Port: 80, Name: "ab"}, ErrTooShort},
		{"name too long", Config{Host: "h", Port: 80, Name: strings.Repeat("x", 51)}, ErrTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v; want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateExtractsTypedFields(t *testing.T) {
	t.Parallel()

	cfg := &Config{Host: "", Port: 0, Name: ""}
	err := cfg.Validate()

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As failed to extract *ValidationError from %v", err)
	}
	if ve.Field != "Host" {
		t.Errorf("ve.Field = %q; want Host", ve.Field)
	}
	if ve.Rule != "required" {
		t.Errorf("ve.Rule = %q; want required", ve.Rule)
	}
}

func TestIsByField(t *testing.T) {
	t.Parallel()

	err := (&Config{Host: "", Port: 0, Name: ""}).Validate()

	if !errors.Is(err, &ValidationError{Field: "Host"}) {
		t.Error("errors.Is should match a Host-field target")
	}
	if errors.Is(err, &ValidationError{Field: "Port"}) {
		t.Error("errors.Is must NOT match a different field")
	}
}

func TestErrorMessageIncludesField(t *testing.T) {
	t.Parallel()

	err := (&Config{Host: "", Port: 8080, Name: "billing"}).Validate()
	if !strings.Contains(err.Error(), "Host") {
		t.Errorf("error message %q does not contain the field name", err.Error())
	}
}

func ExampleConfig_Validate() {
	err := (&Config{Host: "", Port: 8080, Name: "billing"}).Validate()
	var ve *ValidationError
	errors.As(err, &ve)
	fmt.Printf("field=%s required=%v\n", ve.Field, errors.Is(err, ErrRequired))
	// Output: field=Host required=true
}
```

## Review

The type is correct when its two inspection axes are both honest: `errors.Is`
against a sentinel answers the category question for any field, and `errors.As`
into `*ValidationError` yields the exact `Field`/`Value`/`Rule` that failed. The
pointer receiver is what makes the second axis reliable — it fixes the concrete
type in the chain to `*ValidationError` so the `errors.As` target matches. The
custom `Is` is deliberately narrow (Field only); widen it and you would start
swallowing unrelated errors in every downstream `errors.Is`. Run `go test -race`
to confirm, and note that `Validate` is fail-fast: it returns the first failure,
which is the right shape for config loaded once at startup. Exercise 5 builds the
collect-all variant for form and API input where the caller wants every failure at
once.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Is`, `As`, `Unwrap`, and the custom `Is`/`As` method contracts.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping, sentinels, and `Is`/`As`.
- [Go Specification: Errors](https://go.dev/ref/spec#Errors) — the `error` interface definition.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pointer-vs-value-error-identity.md](02-pointer-vs-value-error-identity.md)
