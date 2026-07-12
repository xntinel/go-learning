# Exercise 6: An HTTP Handler That Returns 422 problem+json

This is the boundary where structured errors pay off. A real `POST /users`
handler decodes the body strictly, runs validation, and on failure writes
`422 Unprocessable Entity` with `Content-Type: application/problem+json` and a
machine-readable per-field body — so the client can highlight exactly which
inputs to fix, rather than showing a bare "400".

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
usershandler/              independent module: example.com/usershandler
  go.mod                   go 1.26
  handler.go               User+Validate; ValidationError problem+json; UsersHandler (400/422/201)
  cmd/
    demo/
      main.go              runnable demo: drive the handler with httptest, print statuses
  handler_test.go          httptest: 201, 422+problem+json, 400 malformed, 400 unknown field
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `UsersHandler` that decodes with `DisallowUnknownFields` (malformed JSON and unknown fields -> 400), validates (failure -> 422 `application/problem+json`), and writes 201 on success.
- Test: valid body -> 201; body with two invalid fields -> 422 + correct `Content-Type` + both paths in the body; malformed JSON -> 400; unknown field -> 400.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/11-structured-error-types/06-validation-http-422-handler/cmd/demo
cd go-solutions/10-error-handling/11-structured-error-types/06-validation-http-422-handler
go mod edit -go=1.26
```

### Decode strictly, then validate, then translate

The handler has three failure modes and they map to two different status codes,
which is exactly the distinction structured errors let you make cleanly:

- The body is not the message the endpoint accepts — malformed JSON, or a field
  the schema does not know. This is a *client sent us garbage* error: 400 Bad
  Request. `json.Decoder.DisallowUnknownFields` turns an unexpected key into a
  decode error, so a client that sends `{"nam": "x"}` (a typo) gets a 400 rather
  than silently having the field ignored.
- The body is well-formed and every field is understood, but the *values* are
  invalid — a blank name, an out-of-range age. This is 422 Unprocessable Entity:
  the syntax is fine, the semantics are not. This is where the structured
  `ValidationError` is serialized as `application/problem+json`.
- Everything is valid: 201 Created.

The `Content-Type: application/problem+json` header is not decoration — it tells a
generic client (or an API gateway) that the body follows RFC 9457, so it can be
parsed by a shared problem-details reader instead of ad-hoc code. Set the header
*before* `WriteHeader`, because once the status is written the header map is
frozen.

Create `handler.go`:

```go
package usershandler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type Code string

const (
	CodeRequired Code = "required"
	CodeRange    Code = "range"
)

type FieldError struct {
	Code  Code
	Field string
}

func (e *FieldError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code Code   `json:"code"`
		Path string `json:"path"`
	}{Code: e.Code, Path: e.Field})
}

type ValidationError struct {
	Errors []*FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Field)
	}
	return "invalid: " + strings.Join(parts, ",")
}

func (e *ValidationError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type   string        `json:"type"`
		Title  string        `json:"title"`
		Status int           `json:"status"`
		Errors []*FieldError `json:"errors"`
	}{
		Type:   "about:blank",
		Title:  "Your request is invalid.",
		Status: http.StatusUnprocessableEntity,
		Errors: e.Errors,
	})
}

type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

func (u *User) Validate() error {
	var errs []*FieldError
	if u.Name == "" {
		errs = append(errs, &FieldError{Code: CodeRequired, Field: "name"})
	}
	if u.Email == "" {
		errs = append(errs, &FieldError{Code: CodeRequired, Field: "email"})
	}
	if u.Age < 0 || u.Age > 150 {
		errs = append(errs, &FieldError{Code: CodeRange, Field: "age"})
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// UsersHandler decodes strictly, validates, and translates the outcome to a
// status: 400 for a bad request body, 422 for invalid values, 201 on success.
func UsersHandler(w http.ResponseWriter, r *http.Request) {
	var u User
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		http.Error(w, fmt.Sprintf("bad request body: %v", err), http.StatusBadRequest)
		return
	}

	if err := u.Validate(); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(ve)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}
```

### The runnable demo

The demo drives the handler in-process with `httptest`, sending a valid body and
an invalid one, and printing the status codes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"strings"

	"example.com/usershandler"
)

func post(body string) int {
	req := httptest.NewRequest("POST", "/users", strings.NewReader(body))
	rec := httptest.NewRecorder()
	usershandler.UsersHandler(rec, req)
	return rec.Code
}

func main() {
	fmt.Println("valid:", post(`{"name":"Alice","email":"a@b.c","age":30}`))
	fmt.Println("invalid:", post(`{"name":"","email":"","age":30}`))
	fmt.Println("unknown field:", post(`{"nam":"Alice"}`))
	fmt.Println("malformed:", post(`{`))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid: 201
invalid: 422
unknown field: 400
malformed: 400
```

### Tests

Each test builds a request with `httptest.NewRequest`, records the response with
`httptest.NewRecorder`, and asserts the status code — plus, for the 422 case, the
`Content-Type` header and that both failing field paths appear in the body.

Create `handler_test.go`:

```go
package usershandler

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/users", strings.NewReader(body))
	rec := httptest.NewRecorder()
	UsersHandler(rec, req)
	return rec
}

func TestValidReturns201(t *testing.T) {
	t.Parallel()

	rec := do(t, `{"name":"Alice","email":"a@b.c","age":30}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestInvalidReturns422ProblemJSON(t *testing.T) {
	t.Parallel()

	rec := do(t, `{"name":"","email":"","age":30}`)
	if rec.Code != 422 {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	body := rec.Body.String()
	for _, path := range []string{`"path":"name"`, `"path":"email"`} {
		if !strings.Contains(body, path) {
			t.Fatalf("body missing %s: %s", path, body)
		}
	}
}

func TestMalformedJSONReturns400(t *testing.T) {
	t.Parallel()

	if rec := do(t, `{`); rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUnknownFieldReturns400(t *testing.T) {
	t.Parallel()

	if rec := do(t, `{"name":"Alice","nickname":"al"}`); rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func ExampleUsersHandler() {
	req := httptest.NewRequest("POST", "/users", strings.NewReader(`{"name":"","email":"","age":30}`))
	rec := httptest.NewRecorder()
	UsersHandler(rec, req)
	fmt.Println(rec.Code, rec.Header().Get("Content-Type"))
	// Output: 422 application/problem+json
}
```

## Review

The handler is correct when it distinguishes *malformed or unknown* (400, a
syntax/shape problem the client must fix in its request builder) from *invalid
values* (422, a data problem the user fixes in the form). `DisallowUnknownFields`
is what upgrades an unrecognized key from a silently-ignored typo to a 400 — a
real safety property, because a client sending `{"emial": "..."}` should be told,
not silently accepted with a blank email. The 422 body must carry
`application/problem+json` and the per-field paths, set before `WriteHeader`. The
non-validation error branch still returns 500 rather than pretending the failure
was the user's fault. Run `go test -race`.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder`, and `ResponseRecorder`.
- [`json.Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — rejecting unexpected keys.
- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457.html) — the `application/problem+json` media type.
- [`http.Error`](https://pkg.go.dev/net/http#Error) — writing a plain-text error status.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-field-error-json-public-contract.md](05-field-error-json-public-contract.md) | Next: [07-error-code-to-http-status-mapper.md](07-error-code-to-http-status-mapper.md)
