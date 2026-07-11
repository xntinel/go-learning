# Exercise 21: Generic Protobuf Field Extractor

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A protobuf `Any` field wraps a type URL plus opaque bytes — `anypb`'s real
`UnmarshalTo` decodes those bytes into a concrete message, but the shape
that matters for this lesson is what happens *after* decoding: you have a
dynamically-typed value and a type URL that claims what it is, and pulling
a strongly-typed field out of it needs a comma-ok assertion, not a bare
one. This exercise builds a generic `GetField[T any](fields, name,
wantTypeURL) (value T, found bool)` that checks the type URL and the Go
type together, so a mismatch on either one returns `false` instead of
panicking.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
anyfield/                    independent module: example.com/protobuf-field-extract-typed
  go.mod                     go 1.24
  anyfield.go                package anyfield; Any, type URL constants, GetField[T any](fields,name,wantTypeURL) (value,found)
  cmd/
    demo/
      main.go                extract a string, an int64, a missing field, and a type-mismatched field
  anyfield_test.go           matching type; missing name; type URL mismatch; matching type URL but wrong Go type
```

- Files: `anyfield.go`, `cmd/demo/main.go`, `anyfield_test.go`.
- Implement: `GetField[T any](fields map[string]Any, name, wantTypeURL string) (value T, found bool)`, checking presence, then type URL, then comma-ok asserting the stored `any` value to `T`.
- Test: a present field whose type URL and Go type both match returns `found == true`; an absent name, a type-URL mismatch, and a type-URL match with the wrong Go type parameter all return the zero value and `found == false`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/anyfield/cmd/demo
cd ~/go-exercises/anyfield
go mod init example.com/protobuf-field-extract-typed
go mod edit -go=1.24
```

### Two things can disagree, and both must fail the same way

A dynamically-typed field can be wrong in two independent ways: the sender
tagged it with a type URL the receiver did not ask for (`wantTypeURL`
mismatch — a schema disagreement), or the type URL matches but the actual
Go value stored under it does not assert to the type parameter `T` the
caller requested (a decoder bug, or two systems that agree on the type URL
string but not on what Go type it decodes to). Neither should panic, and a
generic function makes the second case reachable in a way a
non-generic `any`-returning function would just push onto the caller:

```go
func GetField[T any](fields map[string]Any, name, wantTypeURL string) (value T, found bool) {
	f, ok := fields[name]
	if !ok || f.TypeURL != wantTypeURL {
		var zero T
		return zero, false
	}
	value, found = f.Value.(T)
	return value, found
}
```

The comma-ok form `f.Value.(T)` is what makes the third failure mode safe:
if `f.Value` holds an `int64` and the caller instantiated `GetField[int32]`,
the assertion simply fails and reports `found == false` — a plain
`f.Value.(T)` without the second return value would panic instead, and
that panic would only ever surface at runtime, on whichever request
happened to hit the mismatched field first.

Create `anyfield.go`:

```go
package anyfield

// This package models the shape of a protobuf `Any` field without a real
// protobuf dependency: a type URL identifying the wrapped message kind,
// plus the already-decoded Go value that a real `anypb.UnmarshalTo` would
// produce. The interesting part for this exercise is the generic,
// comma-ok extractor below — the same shape applies whether the value
// came from real unmarshalled protobuf bytes or, as here, a plain map.

// Well-known type URLs, mirroring the wrapper types under
// type.googleapis.com/google.protobuf.*.
const (
	TypeURLString = "type.googleapis.com/google.protobuf.StringValue"
	TypeURLInt64  = "type.googleapis.com/google.protobuf.Int64Value"
	TypeURLBool   = "type.googleapis.com/google.protobuf.BoolValue"
)

// Any is a dynamically-typed field: a type URL plus its decoded value.
type Any struct {
	TypeURL string
	Value   any
}

// GetField extracts a typed value for name out of fields. It returns
// found=false — never a panic — for three distinct reasons a caller does
// not need to tell apart: the field is absent, its type URL does not match
// wantTypeURL, or (defensively) its underlying Go value does not assert to
// T even though the type URL matched, which would indicate the sender and
// receiver disagree about what that type URL decodes to.
func GetField[T any](fields map[string]Any, name, wantTypeURL string) (value T, found bool) {
	f, ok := fields[name]
	if !ok || f.TypeURL != wantTypeURL {
		var zero T
		return zero, false
	}
	value, found = f.Value.(T)
	return value, found
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/protobuf-field-extract-typed"
)

func main() {
	fields := map[string]anyfield.Any{
		"user_id":      {TypeURL: anyfield.TypeURLString, Value: "u-123"},
		"amount_cents": {TypeURL: anyfield.TypeURLInt64, Value: int64(4999)},
		"is_refund":    {TypeURL: anyfield.TypeURLBool, Value: true},
	}

	userID, found := anyfield.GetField[string](fields, "user_id", anyfield.TypeURLString)
	fmt.Printf("user_id:      value=%q found=%t\n", userID, found)

	amount, found := anyfield.GetField[int64](fields, "amount_cents", anyfield.TypeURLInt64)
	fmt.Printf("amount_cents: value=%d found=%t\n", amount, found)

	_, found = anyfield.GetField[string](fields, "missing_field", anyfield.TypeURLString)
	fmt.Printf("missing:      found=%t\n", found)

	// Type URL matches but the caller asked for the wrong Go type.
	_, found = anyfield.GetField[int32](fields, "amount_cents", anyfield.TypeURLInt64)
	fmt.Printf("wrong Go type (int32 for an int64 value): found=%t\n", found)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user_id:      value="u-123" found=true
amount_cents: value=4999 found=true
missing:      found=false
wrong Go type (int32 for an int64 value): found=false
```

### Tests

Create `anyfield_test.go`:

```go
package anyfield

import "testing"

func testFields() map[string]Any {
	return map[string]Any{
		"user_id":      {TypeURL: TypeURLString, Value: "u-123"},
		"amount_cents": {TypeURL: TypeURLInt64, Value: int64(4999)},
	}
}

func TestGetFieldMatchingType(t *testing.T) {
	t.Parallel()
	fields := testFields()

	value, found := GetField[string](fields, "user_id", TypeURLString)
	if !found || value != "u-123" {
		t.Fatalf("GetField = (%q, %t), want (u-123, true)", value, found)
	}

	amount, found := GetField[int64](fields, "amount_cents", TypeURLInt64)
	if !found || amount != 4999 {
		t.Fatalf("GetField = (%d, %t), want (4999, true)", amount, found)
	}
}

func TestGetFieldMissingName(t *testing.T) {
	t.Parallel()
	fields := testFields()

	value, found := GetField[string](fields, "does_not_exist", TypeURLString)
	if found {
		t.Fatalf("found = true for a missing field, value=%q", value)
	}
	if value != "" {
		t.Fatalf("value = %q for a missing field, want zero value", value)
	}
}

func TestGetFieldTypeURLMismatch(t *testing.T) {
	t.Parallel()
	fields := testFields()

	_, found := GetField[string](fields, "user_id", TypeURLInt64)
	if found {
		t.Fatal("found = true when the requested type URL does not match the stored one")
	}
}

func TestGetFieldWrongGoType(t *testing.T) {
	t.Parallel()
	fields := testFields()

	// Type URL matches (Int64Value) but T is the wrong concrete Go type.
	value, found := GetField[int32](fields, "amount_cents", TypeURLInt64)
	if found {
		t.Fatal("found = true despite an int64 value not asserting to int32")
	}
	if value != 0 {
		t.Fatalf("value = %d, want zero value on assertion failure", value)
	}
}
```

## Review

`GetField` is correct when all three failure modes — missing name, type
URL mismatch, and Go-type mismatch — return the zero value and
`found == false` with no panic, and when a fully matching field returns the
exact stored value with its exact type. `TestGetFieldWrongGoType` is the
load-bearing test: it is the one case a naive, non-generic implementation
(`func GetField(fields, name, wantTypeURL string) (any, bool)`, leaving the
final assertion to the caller) would push onto every call site instead of
handling once, centrally, here.

The mistake to avoid is checking the Go-type assertion before the type URL
— `f.Value.(T)` without first confirming `f.TypeURL == wantTypeURL` can
return `found == true` for a field that merely happens to hold the right
Go type under the *wrong* semantic type URL (an `int64` age field
accidentally read as an `int64` amount field, say), which is a much harder
bug to track down than a clean `false`.

## Resources

- [google.protobuf.Any](https://protobuf.dev/programming-guides/proto3/#any) — the type-URL-plus-bytes shape this exercise models.
- [Go spec: type parameters](https://go.dev/ref/spec#Type_parameters) — the generic function declaration `GetField[T any](...)`.
- [Go spec: type assertions](https://go.dev/ref/spec#Type_assertions) — the comma-ok form `value, found = f.Value.(T)` used here instead of the panicking single-result form.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-env-var-lookup-with-source.md](20-env-var-lookup-with-source.md) | Next: [22-cache-lookup-with-age.md](22-cache-lookup-with-age.md)
