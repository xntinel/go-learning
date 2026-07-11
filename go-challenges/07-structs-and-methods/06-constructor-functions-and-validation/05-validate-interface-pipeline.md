# Exercise 5: Validate an HTTP Request DTO Through a Validator Pipeline

At the top of an HTTP handler the flow is always decode-then-validate: unmarshal
the request body into a DTO, then check its fields before letting it near the
domain. This exercise builds that step as a `Validator` interface plus a
`ValidateAll` helper that runs a slice of field rules and joins their failures,
carrying the offending field name in a typed `FieldError` the caller can extract
with `errors.As`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
httpvalid/                   independent module: example.com/validate-interface-pipeline
  go.mod
  httpvalid.go               Validator, CreateUserRequest, FieldError, ValidateAll, DecodeAndValidate, sentinels
  cmd/
    demo/
      main.go                decodes a good and a bad body, prints the outcomes
  httpvalid_test.go          valid DTO, each field rule, aggregation, errors.As FieldError, malformed vs invalid
```

- Files: `httpvalid.go`, `cmd/demo/main.go`, `httpvalid_test.go`.
- Implement: a `Validator` interface, a `CreateUserRequest` that implements it via `ValidateAll`, a `FieldError` type, and `DecodeAndValidate([]byte)`.
- Test: a well-formed DTO passes; required / length-bound / enum-membership each fail with their sentinel via `errors.Is`; `ValidateAll` aggregates; `errors.As` retrieves the field name from a `FieldError`; and malformed JSON is distinguished from valid-JSON-but-invalid-DTO.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/httpvalid/cmd/demo
cd ~/go-exercises/httpvalid
go mod init example.com/validate-interface-pipeline
```

### Keep transport out of the domain

Decoding and validation are two different failures with two different HTTP
responses. Malformed JSON is a `400` that means "your bytes are not even JSON";
a valid JSON body that violates a field rule is a `422`/`400` that means "your
values are wrong, here is which field". `DecodeAndValidate` keeps them separate:
`json.Unmarshal` failures return a distinct error the caller can detect, and only
a successfully-decoded DTO proceeds to `Validate`. Mixing the two — validating
inside the domain constructor, or treating every failure as one blob — loses the
distinction the handler needs to answer the client correctly.

The field rules are small closures returning `error`, and each wraps a sentinel
inside a `FieldError` that records which field failed. `FieldError` implements
`Unwrap`, so `errors.Is(err, ErrRequired)` still matches through it, while
`errors.As(err, &fe)` extracts the `Field` for a structured API response.
`ValidateAll` runs the rules and returns `errors.Join`, so a DTO with three bad
fields reports all three, each tagged with its field name. The DTO's `Validate`
method is what satisfies the `Validator` interface, so a generic middleware can
call `Validate()` on any DTO without knowing its concrete type.

Create `httpvalid.go`:

```go
package httpvalid

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	ErrRequired    = errors.New("field is required")
	ErrTooLong     = errors.New("field is too long")
	ErrInvalidEnum = errors.New("field has an invalid value")
	ErrMalformed   = errors.New("request body is not valid JSON")
)

// Validator is implemented by any DTO that can check its own invariants.
type Validator interface {
	Validate() error
}

// FieldError tags a wrapped sentinel with the field that failed, so an API can
// report the field name while errors.Is still matches the underlying rule.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return fmt.Sprintf("%s: %v", e.Field, e.Err) }
func (e *FieldError) Unwrap() error { return e.Err }

func fieldErr(field string, err error) error {
	return &FieldError{Field: field, Err: err}
}

// ValidateAll runs each rule and joins their failures into one error.
func ValidateAll(rules ...func() error) error {
	var errs []error
	for _, rule := range rules {
		if err := rule(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var validRoles = []string{"admin", "member", "viewer"}

// CreateUserRequest is the decoded body of POST /users.
type CreateUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// Validate implements Validator by running every field rule.
func (r CreateUserRequest) Validate() error {
	return ValidateAll(
		func() error {
			if strings.TrimSpace(r.Name) == "" {
				return fieldErr("name", ErrRequired)
			}
			if len(r.Name) > 50 {
				return fieldErr("name", ErrTooLong)
			}
			return nil
		},
		func() error {
			if strings.TrimSpace(r.Email) == "" {
				return fieldErr("email", ErrRequired)
			}
			return nil
		},
		func() error {
			if !slices.Contains(validRoles, r.Role) {
				return fieldErr("role", ErrInvalidEnum)
			}
			return nil
		},
	)
}

// DecodeAndValidate unmarshals body into a CreateUserRequest and validates it,
// distinguishing malformed JSON (ErrMalformed) from a valid-but-invalid DTO.
func DecodeAndValidate(body []byte) (CreateUserRequest, error) {
	var r CreateUserRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return CreateUserRequest{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := r.Validate(); err != nil {
		return CreateUserRequest{}, err
	}
	return r, nil
}
```

### The runnable demo

The demo decodes a valid body, a body that is valid JSON but fails the rules, and
a body that is not JSON at all, printing what each produces.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validate-interface-pipeline"
)

func main() {
	good := []byte(`{"name":"Alice","email":"alice@example.com","role":"admin"}`)
	r, err := httpvalid.DecodeAndValidate(good)
	fmt.Printf("good: %s <%s> role=%s err=%v\n", r.Name, r.Email, r.Role, err)

	bad := []byte(`{"name":"","email":"","role":"wizard"}`)
	_, err = httpvalid.DecodeAndValidate(bad)
	fmt.Println("bad:")
	fmt.Println(err)

	notJSON := []byte(`{not json`)
	_, err = httpvalid.DecodeAndValidate(notJSON)
	fmt.Printf("malformed: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: Alice <alice@example.com> role=admin err=<nil>
bad:
name: field is required
email: field is required
role: field has an invalid value
malformed: request body is not valid JSON: invalid character 'n' looking for beginning of object key string
```

### Tests

`TestFieldErrorExtractable` proves the two-layer error contract: `errors.Is` finds
the rule sentinel through the `FieldError`, and `errors.As` extracts the concrete
`FieldError` to read `Field`. `TestMalformedDistinct` proves decode failures are
separable from validation failures.

Create `httpvalid_test.go`:

```go
package httpvalid

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidDTOPasses(t *testing.T) {
	t.Parallel()
	r := CreateUserRequest{Name: "Alice", Email: "alice@example.com", Role: "admin"}
	if err := r.Validate(); err != nil {
		t.Fatalf("valid DTO rejected: %v", err)
	}
}

func TestFieldRules(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		dto  CreateUserRequest
		want error
	}{
		"missing name":  {CreateUserRequest{Email: "a@b.co", Role: "admin"}, ErrRequired},
		"name too long": {CreateUserRequest{Name: string(make([]byte, 51)), Email: "a@b.co", Role: "admin"}, ErrTooLong},
		"missing email": {CreateUserRequest{Name: "Al", Role: "admin"}, ErrRequired},
		"bad role":      {CreateUserRequest{Name: "Al", Email: "a@b.co", Role: "wizard"}, ErrInvalidEnum},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := tc.dto.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAggregatesFailures(t *testing.T) {
	t.Parallel()
	r := CreateUserRequest{Name: "", Email: "", Role: "wizard"}
	err := r.Validate()
	if !errors.Is(err, ErrRequired) || !errors.Is(err, ErrInvalidEnum) {
		t.Fatalf("expected required and enum sentinels, got %v", err)
	}
}

func TestFieldErrorExtractable(t *testing.T) {
	t.Parallel()
	r := CreateUserRequest{Name: "Al", Email: "a@b.co", Role: "wizard"}
	err := r.Validate()
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("expected a *FieldError in %v", err)
	}
	if fe.Field != "role" {
		t.Fatalf("FieldError.Field = %q, want role", fe.Field)
	}
	if !errors.Is(fe, ErrInvalidEnum) {
		t.Fatalf("FieldError should wrap ErrInvalidEnum, got %v", fe.Err)
	}
}

func TestMalformedDistinct(t *testing.T) {
	t.Parallel()
	_, err := DecodeAndValidate([]byte(`{not json`))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed body err = %v, want ErrMalformed", err)
	}
	_, err = DecodeAndValidate([]byte(`{"name":"","email":"","role":"x"}`))
	if errors.Is(err, ErrMalformed) {
		t.Fatalf("valid JSON with bad fields should not be ErrMalformed: %v", err)
	}
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func ExampleCreateUserRequest_Validate() {
	r := CreateUserRequest{Name: "Alice", Email: "alice@example.com", Role: "admin"}
	fmt.Println(r.Validate())
	// Output: <nil>
}
```

## Review

The pipeline is correct when a good DTO passes, each field rule fails with its
sentinel, and the two failure kinds — malformed JSON and invalid values — are
distinguishable, because a handler answers them with different responses. The
`FieldError` demonstrates the full error contract: `Unwrap` keeps `errors.Is`
matching the rule while `errors.As` recovers the field name for a structured
response. The mistake to avoid is collapsing decode and validation into one path,
or string-matching the message instead of using `errors.Is`/`errors.As`, both of
which break the handler's ability to react precisely.

## Resources

- [errors package (Is, As, Join)](https://pkg.go.dev/errors) — the matching and extraction primitives the pipeline relies on.
- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — decoding the request body.
- [slices.Contains](https://pkg.go.dev/slices#Contains) — the enum-membership check.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-parse-dont-validate-primitives.md](04-parse-dont-validate-primitives.md) | Next: [06-money-value-object-invariants.md](06-money-value-object-invariants.md)
