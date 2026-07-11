# Exercise 10: Map Internal Error Codes to Stable API Codes and HTTP Statuses

This module is the lesson's thesis in one artifact: an internal `ErrorCode` enum
whose `iota` ordinal is private, exposed to the outside world only through a
stable string code and an HTTP status held in an explicit registry. The ordinal
can be reordered freely; the external contract — `"RESOURCE_NOT_FOUND"` and
`404` — is decoupled from declaration order and locked by tests.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
apierr/                         module: example.com/apierr
  go.mod                        go 1.26
  apierr.go                     ErrorCode enum, registry, String/HTTPStatus/MarshalJSON
  cmd/
    demo/
      main.go                   marshals an APIError and prints its status
  apierr_test.go                completeness, code/status pairs, MarshalJSON is a string
```

Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
Implement: `ErrorCode` with a `maxCode` sentinel, a registry mapping each code to a stable string and HTTP status, and `String`, `HTTPStatus`, `MarshalJSON`.
Test: every code has a registry entry (completeness), `String`/`HTTPStatus` return the registered values, and `MarshalJSON` emits the quoted stable string, never the ordinal.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p apierr/cmd/demo
cd apierr
go mod init example.com/apierr
```

## Why a registry, not a switch

Everything in this lesson has been building to one rule: the ordinal is an
internal encoding, and the external contract must not depend on it. An error
catalog is where that rule pays off most, because an error code appears in three
external places at once — the response body's `code` field, the HTTP status
line, and any client-side handling keyed on the string. All three must be stable
across a `const`-block reorder.

The cleanest way to express that is a single registry keyed by the enum:

```go
var registry = map[ErrorCode]struct {
	code   string
	status int
}{ ... }
```

The registry is the *one* place the public contract lives. `String()` and
`HTTPStatus()` read from it, and `MarshalJSON()` emits the string from it, so
there is no second source of truth to drift out of sync. Reordering the `const`
block changes the ordinals but not the map keys' associations, so the external
`code`/`status` for each named constant is unchanged — which the tests assert
with specific pairs (`CodeResourceNotFound` → `"RESOURCE_NOT_FOUND"` → `404`).

`MarshalJSON` is the serialization guard. Implementing `json.Marshaler` makes
`encoding/json` emit the stable string (`strconv.Quote` produces the quoted
form) instead of the default int encoding of the underlying `uint8`. Without it,
an `APIError{Code: CodeResourceNotFound}` would serialize as `{"code":1}`, and
every client parsing that number is one reorder away from breakage.

Finally, the `maxCode` sentinel powers a completeness test: every code from the
first real one up to `maxCode` must have a registry entry. Add
`CodePreconditionFailed` and forget to register it, and the completeness test
fails — the same exhaustiveness discipline as the previous module, applied to the
error catalog. The HTTP statuses reference `net/http` constants, so `404` is
`http.StatusNotFound`, not a literal.

Create `apierr.go`:

```go
package apierr

import (
	"net/http"
	"strconv"
)

// ErrorCode is an internal error identifier. Its ordinal is private; the stable
// string code and HTTP status in the registry are the external contract.
type ErrorCode uint8

const (
	CodeUnknown ErrorCode = iota
	CodeResourceNotFound
	CodeValidationFailed
	CodeUnauthorized
	CodeConflict
	CodeRateLimited
	CodeInternal
	maxCode // sentinel: count of codes
)

type entry struct {
	code   string
	status int
}

// registry is the single source of truth for the external contract.
var registry = map[ErrorCode]entry{
	CodeResourceNotFound: {"RESOURCE_NOT_FOUND", http.StatusNotFound},
	CodeValidationFailed: {"VALIDATION_FAILED", http.StatusBadRequest},
	CodeUnauthorized:     {"UNAUTHORIZED", http.StatusUnauthorized},
	CodeConflict:         {"CONFLICT", http.StatusConflict},
	CodeRateLimited:      {"RATE_LIMITED", http.StatusTooManyRequests},
	CodeInternal:         {"INTERNAL", http.StatusInternalServerError},
}

// String returns the stable external code string.
func (c ErrorCode) String() string {
	if e, ok := registry[c]; ok {
		return e.code
	}
	return "UNKNOWN"
}

// HTTPStatus returns the HTTP status to send for this error.
func (c ErrorCode) HTTPStatus() int {
	if e, ok := registry[c]; ok {
		return e.status
	}
	return http.StatusInternalServerError
}

// MarshalJSON emits the stable string code, never the ordinal.
func (c ErrorCode) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(c.String())), nil
}

// APIError is the JSON error envelope returned to clients.
type APIError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/apierr"
)

func main() {
	e := apierr.APIError{Code: apierr.CodeResourceNotFound, Message: "user 42 not found"}
	out, err := json.Marshal(e)
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	fmt.Println(string(out))
	fmt.Printf("status=%d code=%s\n", e.Code.HTTPStatus(), e.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"code":"RESOURCE_NOT_FOUND","message":"user 42 not found"}
status=404 code=RESOURCE_NOT_FOUND
```

## Tests

`TestCompleteness` walks every code up to the `maxCode` sentinel and asserts a
registry entry exists — the guard that fails when a new code is added but not
registered. `TestPairs` pins specific code → string → status triples so a reorder
of the `const` block cannot change the external contract unnoticed.
`TestMarshalJSONIsString` asserts the JSON is the quoted string and never the
ordinal.

Create `apierr_test.go`:

```go
package apierr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestCompleteness(t *testing.T) {
	t.Parallel()

	for c := CodeUnknown + 1; c < maxCode; c++ {
		if _, ok := registry[c]; !ok {
			t.Fatalf("ErrorCode %d has no registry entry (unregistered code)", c)
		}
	}
	if len(registry) != int(maxCode)-1 {
		t.Fatalf("registry has %d entries, want %d (sentinel drift)", len(registry), int(maxCode)-1)
	}
}

func TestPairs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code   ErrorCode
		string string
		status int
	}{
		{CodeResourceNotFound, "RESOURCE_NOT_FOUND", http.StatusNotFound},
		{CodeValidationFailed, "VALIDATION_FAILED", http.StatusBadRequest},
		{CodeUnauthorized, "UNAUTHORIZED", http.StatusUnauthorized},
		{CodeConflict, "CONFLICT", http.StatusConflict},
		{CodeRateLimited, "RATE_LIMITED", http.StatusTooManyRequests},
		{CodeInternal, "INTERNAL", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.string, func(t *testing.T) {
			t.Parallel()
			if got := tt.code.String(); got != tt.string {
				t.Fatalf("String() = %q, want %q", got, tt.string)
			}
			if got := tt.code.HTTPStatus(); got != tt.status {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tt.status)
			}
		})
	}
}

func TestMarshalJSONIsString(t *testing.T) {
	t.Parallel()

	out, err := json.Marshal(APIError{Code: CodeRateLimited, Message: "slow down"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"code":"RATE_LIMITED","message":"slow down"}`
	if string(out) != want {
		t.Fatalf("Marshal = %s, want %s", out, want)
	}
}

func ExampleErrorCode_MarshalJSON() {
	out, _ := json.Marshal(CodeResourceNotFound)
	fmt.Println(string(out))
	// Output: "RESOURCE_NOT_FOUND"
}
```

## Review

The catalog is correct when the external contract is a pure function of the
registry, not the ordinal: `String`, `HTTPStatus`, and `MarshalJSON` all read
from the same map, so there is no second source of truth. The completeness test
is what keeps the catalog honest as it grows — a new code with no entry fails the
build rather than serializing as `"UNKNOWN"` in production. `MarshalJSON` is
non-negotiable: without it the code serializes as a bare integer, re-exposing the
exact wire-contract fragility this whole lesson exists to prevent.

## Resources

- [encoding/json: Marshaler](https://pkg.go.dev/encoding/json#Marshaler)
- [net/http: Status constants](https://pkg.go.dev/net/http#pkg-constants)
- [strconv: Quote](https://pkg.go.dev/strconv#Quote)
- [Effective Go: Constants and iota](https://go.dev/doc/effective_go#constants)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-http-retry-classifier-constants.md](09-http-retry-classifier-constants.md) | Next: [../05-type-conversions-and-type-assertions/00-concepts.md](../05-type-conversions-and-type-assertions/00-concepts.md)
