# 5. Building a Struct Validator

Libraries like `go-playground/validator` drive validation from struct tags at runtime. Building one yourself teaches you how reflection, tag parsing, and multiple-error collection work together. Even when you use a library in production, knowing the internals lets you debug edge cases and write custom rules that a generic library cannot anticipate.

```text
validator/
  go.mod
  validator.go
  validator_test.go
  cmd/demo/main.go
```

## Concepts

### Tag-Driven Rule Discovery

`reflect.StructField.Tag.Get("validate")` returns the raw tag value for a field. A convention like `validate:"required,min=2,max=50"` packs multiple rules into one comma-separated string. The validator splits on `,`, then on `=` to separate rule name from parameter.

The critical property is that the tag is read at runtime, not at compile time. There is no compile-time check that `min=2` is meaningful for a `bool` field. The validator is responsible for returning a clear error when a rule does not apply to a field's kind.

### Collecting Multiple Errors

A validator that stops at the first error forces the user to fix one problem, re-submit, discover the next problem, and repeat. Collecting all errors in a single pass gives a better experience and is a simple change: instead of returning on the first failure, append to a `[]FieldError` and return the accumulated slice only after all fields have been checked.

The returned error is a `ValidationErrors` value (a named `[]FieldError` type) that also satisfies `error`. Callers that only need a pass/fail result use `err != nil`; callers that need structured output type-assert to `ValidationErrors`.

### Nested Struct Handling

Nested structs must be validated recursively. A `validate:"dive"` tag on an embedded struct field triggers recursive validation. The field name path is built with dot notation (`Address.City`) so the caller can pinpoint exactly which field failed. Alternatively, you can always recurse into struct-kind fields regardless of a special tag — the choice is an API design decision.

### IsZero and the Required Rule

`reflect.Value.IsZero()` returns `true` when the value equals its type's zero value (0 for integers, `""` for strings, `nil` for pointers and slices, etc.). The `required` rule uses this single call regardless of the field's kind — no type-switch needed for the zero check.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/validator/cmd/demo
cd ~/go-exercises/validator
go mod init example.com/validator
```

### Exercise 1: Error Types and the Validate Entry Point

Create `validator.go`:

```go
package validator

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// FieldError records a single validation failure.
type FieldError struct {
	Field   string
	Rule    string
	Message string
}

func (e FieldError) Error() string {
	return fmt.Sprintf("field %q failed rule %q: %s", e.Field, e.Rule, e.Message)
}

// ValidationErrors is the error type returned when one or more fields fail.
// Callers can range over it to inspect individual failures.
type ValidationErrors []FieldError

func (ve ValidationErrors) Error() string {
	msgs := make([]string, len(ve))
	for i, e := range ve {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}

// HasErrors reports whether there are any validation errors.
func (ve ValidationErrors) HasErrors() bool { return len(ve) > 0 }

var emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// Validate inspects s (a struct or pointer to struct) and returns a
// ValidationErrors if any validate tags fail, or nil if all pass.
func Validate(s any) error {
	v := reflect.ValueOf(s)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("validator: expected struct, got %v", v.Kind())
	}

	var errs ValidationErrors
	validateStruct(v, "", &errs)
	if errs.HasErrors() {
		return errs
	}
	return nil
}

func validateStruct(v reflect.Value, prefix string, errs *ValidationErrors) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}

		name := sf.Name
		if prefix != "" {
			name = prefix + "." + name
		}

		fv := v.Field(i)
		tag := sf.Tag.Get("validate")

		// Always recurse into embedded structs (with or without dive).
		if fv.Kind() == reflect.Struct && (tag == "dive" || tag == "") {
			validateStruct(fv, name, errs)
			continue
		}
		if tag == "" || tag == "-" {
			continue
		}

		for _, rule := range strings.Split(tag, ",") {
			if fe := applyRule(name, fv, rule); fe != nil {
				*errs = append(*errs, *fe)
			}
		}
	}
}

func applyRule(fieldName string, v reflect.Value, rule string) *FieldError {
	parts := strings.SplitN(rule, "=", 2)
	name := parts[0]
	param := ""
	if len(parts) == 2 {
		param = parts[1]
	}

	switch name {
	case "required":
		if v.IsZero() {
			return &FieldError{Field: fieldName, Rule: "required", Message: "is required"}
		}
	case "min":
		return checkMin(fieldName, v, param)
	case "max":
		return checkMax(fieldName, v, param)
	case "oneof":
		return checkOneOf(fieldName, v, param)
	case "email":
		if v.Kind() != reflect.String || !emailRe.MatchString(v.String()) {
			return &FieldError{Field: fieldName, Rule: "email", Message: "invalid email address"}
		}
	case "dive":
		// handled at the struct level; skip here
	default:
		return &FieldError{Field: fieldName, Rule: name, Message: "unknown rule"}
	}
	return nil
}

func checkMin(field string, v reflect.Value, param string) *FieldError {
	n, err := strconv.Atoi(param)
	if err != nil {
		return &FieldError{Field: field, Rule: "min", Message: "invalid parameter: " + param}
	}
	switch v.Kind() {
	case reflect.String:
		if v.Len() < n {
			return &FieldError{Field: field, Rule: "min",
				Message: fmt.Sprintf("length must be >= %d, got %d", n, v.Len())}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() < int64(n) {
			return &FieldError{Field: field, Rule: "min",
				Message: fmt.Sprintf("must be >= %d, got %d", n, v.Int())}
		}
	case reflect.Slice, reflect.Array, reflect.Map:
		if v.Len() < n {
			return &FieldError{Field: field, Rule: "min",
				Message: fmt.Sprintf("length must be >= %d, got %d", n, v.Len())}
		}
	default:
		return &FieldError{Field: field, Rule: "min",
			Message: fmt.Sprintf("rule does not apply to kind %v", v.Kind())}
	}
	return nil
}

func checkMax(field string, v reflect.Value, param string) *FieldError {
	n, err := strconv.Atoi(param)
	if err != nil {
		return &FieldError{Field: field, Rule: "max", Message: "invalid parameter: " + param}
	}
	switch v.Kind() {
	case reflect.String:
		if v.Len() > n {
			return &FieldError{Field: field, Rule: "max",
				Message: fmt.Sprintf("length must be <= %d, got %d", n, v.Len())}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() > int64(n) {
			return &FieldError{Field: field, Rule: "max",
				Message: fmt.Sprintf("must be <= %d, got %d", n, v.Int())}
		}
	default:
		return &FieldError{Field: field, Rule: "max",
			Message: fmt.Sprintf("rule does not apply to kind %v", v.Kind())}
	}
	return nil
}

func checkOneOf(field string, v reflect.Value, param string) *FieldError {
	options := strings.Fields(param)
	got := fmt.Sprintf("%v", v.Interface())
	for _, opt := range options {
		if got == opt {
			return nil
		}
	}
	return &FieldError{Field: field, Rule: "oneof",
		Message: fmt.Sprintf("must be one of [%s], got %q", strings.Join(options, ", "), got)}
}
```

### Exercise 2: Tests

Create `validator_test.go`:

```go
package validator

import (
	"errors"
	"fmt"
	"testing"
)

type Address struct {
	Street string `validate:"required,min=5"`
	City   string `validate:"required"`
	Zip    string `validate:"required,min=5,max=10"`
}

type User struct {
	Name    string  `validate:"required,min=2,max=50"`
	Email   string  `validate:"required,email"`
	Age     int     `validate:"required,min=18,max=120"`
	Role    string  `validate:"required,oneof=admin user guest"`
	Address Address // no tag — recurses automatically
}

func validUser() User {
	return User{
		Name:  "Alice",
		Email: "alice@example.com",
		Age:   30,
		Role:  "admin",
		Address: Address{
			Street: "123 Main St",
			City:   "Springfield",
			Zip:    "62701",
		},
	}
}

func TestValidStructPassesWithNoErrors(t *testing.T) {
	t.Parallel()

	if err := Validate(validUser()); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
}

func TestValidateCollectsMultipleErrors(t *testing.T) {
	t.Parallel()

	bad := User{
		Name:  "A", // too short (min=2)
		Email: "not-an-email",
		Age:   10,           // under 18
		Role:  "superadmin", // not in oneof
		Address: Address{
			Street: "123", // too short
			City:   "",    // required
			Zip:    "1",   // too short
		},
	}

	err := Validate(bad)
	if err == nil {
		t.Fatal("expected validation errors, got nil")
	}

	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("err is %T, want ValidationErrors", err)
	}
	if len(ve) < 5 {
		t.Errorf("expected at least 5 errors, got %d: %v", len(ve), ve)
	}
}

func TestRequiredRule(t *testing.T) {
	t.Parallel()

	type Simple struct {
		Name string `validate:"required"`
	}

	err := Validate(Simple{Name: ""})
	if err == nil {
		t.Fatal("expected error for empty required field")
	}
	var ve ValidationErrors
	errors.As(err, &ve)
	if ve[0].Rule != "required" {
		t.Errorf("rule = %q, want required", ve[0].Rule)
	}
}

func TestMinRuleString(t *testing.T) {
	t.Parallel()

	type T struct {
		S string `validate:"min=3"`
	}
	if err := Validate(T{S: "ab"}); err == nil {
		t.Fatal("expected min=3 failure for len-2 string")
	}
	if err := Validate(T{S: "abc"}); err != nil {
		t.Fatalf("unexpected error for len-3 string: %v", err)
	}
}

func TestMaxRuleString(t *testing.T) {
	t.Parallel()

	type T struct {
		S string `validate:"max=5"`
	}
	if err := Validate(T{S: "toolong"}); err == nil {
		t.Fatal("expected max=5 failure for len-7 string")
	}
}

func TestOneOfRule(t *testing.T) {
	t.Parallel()

	type T struct {
		Role string `validate:"oneof=admin user"`
	}
	if err := Validate(T{Role: "superadmin"}); err == nil {
		t.Fatal("expected oneof failure")
	}
	if err := Validate(T{Role: "admin"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEmailRule(t *testing.T) {
	t.Parallel()

	type T struct {
		Email string `validate:"email"`
	}
	cases := []struct {
		email string
		ok    bool
	}{
		{"alice@example.com", true},
		{"not-an-email", false},
		{"missing@tld.", false},
		{"a@b.co", true},
	}
	for _, tc := range cases {
		err := Validate(T{Email: tc.email})
		if tc.ok && err != nil {
			t.Errorf("email %q: unexpected error: %v", tc.email, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("email %q: expected error, got nil", tc.email)
		}
	}
}

func TestNestedStructValidated(t *testing.T) {
	t.Parallel()

	u := validUser()
	u.Address.City = "" // violates required

	err := Validate(u)
	if err == nil {
		t.Fatal("expected error for empty nested City")
	}
	var ve ValidationErrors
	errors.As(err, &ve)

	var found bool
	for _, e := range ve {
		if e.Field == "Address.City" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error for Address.City, got: %v", ve)
	}
}

func TestPointerInputAccepted(t *testing.T) {
	t.Parallel()

	u := validUser()
	if err := Validate(&u); err != nil {
		t.Fatalf("Validate(&user) = %v, want nil", err)
	}
}

func TestMinMaxOnInapplicableKind(t *testing.T) {
	t.Parallel()

	type T struct {
		B bool `validate:"min=2"`
	}
	err := Validate(T{B: true})
	if err == nil {
		t.Fatal("expected error for min on bool field, got nil")
	}
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("err is %T, want ValidationErrors", err)
	}
	if ve[0].Rule != "min" {
		t.Errorf("rule = %q, want min", ve[0].Rule)
	}
}

func ExampleValidate() {
	type Item struct {
		Name string `validate:"required,min=2"`
	}
	err := Validate(Item{Name: "x"})
	if err != nil {
		fmt.Println("invalid")
	} else {
		fmt.Println("valid")
	}
	// Output: invalid
}
```

Add `"fmt"` to the import block — the Example uses it.

**Your turn:** add `TestUnknownRuleReturnsError` that creates a struct with `validate:"nosuchrule"` and asserts the returned error contains a `FieldError` with `Rule == "nosuchrule"`.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/validator"
)

type Address struct {
	Street string `validate:"required,min=5"`
	City   string `validate:"required"`
	Zip    string `validate:"required,min=5,max=10"`
}

type User struct {
	Name    string `validate:"required,min=2,max=50"`
	Email   string `validate:"required,email"`
	Age     int    `validate:"required,min=18,max=120"`
	Role    string `validate:"required,oneof=admin user guest"`
	Address Address
}

func main() {
	good := User{
		Name:    "Alice",
		Email:   "alice@example.com",
		Age:     30,
		Role:    "admin",
		Address: Address{Street: "123 Main St", City: "Springfield", Zip: "62701"},
	}
	if err := validator.Validate(good); err != nil {
		log.Fatal("unexpected:", err)
	}
	fmt.Println("valid user: OK")

	bad := User{
		Name:    "A",
		Email:   "bad",
		Age:     5,
		Role:    "superadmin",
		Address: Address{Street: "X", City: "", Zip: "1"},
	}
	if err := validator.Validate(bad); err != nil {
		var ve validator.ValidationErrors
		if errors.As(err, &ve) {
			fmt.Printf("validation failed (%d errors):\n", len(ve))
			for _, e := range ve {
				fmt.Printf("  %s\n", e)
			}
		}
	}
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Stopping at the First Error

Wrong: returning the error from `applyRule` immediately instead of appending.

What happens: the user sees only the first broken field, fixes it, and re-submits, discovering the next error. Each validation round is a separate round-trip.

Fix: accumulate errors into a `[]FieldError` slice and return the full list.

### Not Recursing Into Nested Structs

Wrong: skipping fields whose `Kind()` is `reflect.Struct`.

What happens: fields in nested structs are never validated, even when they have `validate` tags.

Fix: when the field kind is `reflect.Struct`, call `validateStruct` recursively with the dotted field name as the prefix.

### Using String Comparison for Required on Non-String Types

Wrong: checking `v.String() == ""` for the `required` rule on all types.

What happens: `v.String()` on an integer returns a representation string like `<int Value>`, never empty.

Fix: use `v.IsZero()` which is type-aware and returns `true` for 0, `""`, `false`, `nil`, and zero structs.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- Tag-based validation reads `validate` struct tags with `reflect.StructField.Tag.Get("validate")`.
- Split rules on `,`, then rule name from parameter on `=`.
- `v.IsZero()` handles the `required` check for any kind.
- Collect all errors into `ValidationErrors` rather than returning on the first failure.
- Recurse into nested struct fields with a dotted prefix for clear error paths.
- `reflect.StructField.IsExported()` guards against unexported fields.

## What's Next

Next: [DeepEqual and Custom Comparison](../06-deepequal-and-custom-comparison/06-deepequal-and-custom-comparison.md).

## Resources

- [reflect package](https://pkg.go.dev/reflect)
- [reflect.StructField.Tag](https://pkg.go.dev/reflect#StructField)
- [Well-known struct tags](https://go.dev/wiki/Well-known-struct-tags)
- [go-playground/validator](https://github.com/go-playground/validator) — production struct validator to compare with
