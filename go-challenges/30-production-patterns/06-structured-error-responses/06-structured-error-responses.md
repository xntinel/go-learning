# 6. Structured Error Responses

HTTP APIs that return bare `500 Internal Server Error` with an empty body leave clients unable to distinguish a missing resource from a transient fault. Structured error responses give each failure a machine-readable code, a human-readable message, and enough context for a client to decide whether to retry, fix the request, or escalate. RFC 7807 standardizes this shape, and the interesting engineering problems are: how to carry HTTP status and an error code through the Go error chain, how to map domain errors to RFC 7807 responses in a single, reusable place, and how to prevent internal details from leaking to callers.

```text
errresponse/
  go.mod
  apierr.go
  apierr_test.go
  cmd/demo/main.go
```

## Concepts

### The AppError Interface

The Go `error` interface carries only a text message. An HTTP handler also needs a status code (404, 422, 429) and a machine-readable error code ("RESOURCE_NOT_FOUND") that a client can branch on without parsing human text. The idiomatic solution is a richer interface:

```go
type AppError interface {
	error
	StatusCode() int
	ErrorCode() string
}
```

Any concrete type that satisfies `AppError` can be used anywhere an `error` is expected. The centralized error handler uses `errors.As` to ask "does this error chain contain an `AppError`?" and extracts the concrete behavior without a type switch.

### RFC 7807 Problem Details

RFC 7807 (Problem Details for HTTP APIs) specifies a JSON response shape:

```
{
  "type":     "https://api.example.com/errors/RESOURCE_NOT_FOUND",
  "title":    "Not Found",
  "status":   404,
  "detail":   "user with id \"42\" not found",
  "instance": "/users/42"
}
```

The `type` field is a URI that uniquely identifies the error class; `title` is the stable human label (never put dynamic data here); `detail` carries the per-request explanation. Extensions add type-specific fields: a 422 response adds `validation_errors`, a 429 response adds `retry_after`. The content-type is `application/problem+json`.

### errors.As for Centralized Dispatch

`errors.As` walks the error chain produced by `%w` wrapping and sets a typed target if any link satisfies the target type. This lets a handler wrap a `*NotFoundError` deep inside business logic, and the middleware layer unpacks it without knowing what layers wrapped it:

```go
var appErr AppError
if !errors.As(err, &appErr) {
	appErr = &InternalError{Err: err}
}
```

If no link satisfies `AppError`, the handler treats the error as an internal fault and returns 500 — without leaking the cause to the client, but logging it server-side.

### Information Hazards in 5xx Responses

For 4xx errors the detail is driven by the client's own request and is safe to echo back. For 5xx errors the detail could contain connection strings, file paths, or stack traces — internal information the client must not see. The rule is: log the full error for 5xx, but send a generic "an unexpected error occurred" detail to the client.

### Sentinel Errors and Wrapping

Concrete error types can be checked with `errors.As`. Sentinel errors (`errors.New`) can be checked with `errors.Is`. When a domain function wraps a sentinel with `fmt.Errorf("...: %w", ErrNotFound)`, both checks work on the resulting chain. Use sentinel errors for leaf conditions, concrete types for conditions that carry structured data (field-level validation errors, retry-after seconds).

## Exercises

This is a library, not a program: there is no `main` at the root. Verify it with `go test`.

### Exercise 1: The AppError Interface and Concrete Types

Create `apierr.go`:

```go
package errresponse

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// AppError is the contract every domain error must satisfy so that
// the centralized handler can map it to an HTTP status and error code.
type AppError interface {
	error
	StatusCode() int
	ErrorCode() string
}

// ProblemDetail is the RFC 7807 response envelope.
type ProblemDetail struct {
	Type             string       `json:"type"`
	Title            string       `json:"title"`
	Status           int          `json:"status"`
	Detail           string       `json:"detail"`
	Instance         string       `json:"instance,omitempty"`
	ValidationErrors []FieldError `json:"validation_errors,omitempty"`
	RetryAfter       int          `json:"retry_after,omitempty"`
}

// FieldError describes one invalid field in a validation failure.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Sentinel errors for leaf conditions.
var (
	ErrNotFound   = errors.New("resource not found")
	ErrConflict   = errors.New("resource conflict")
	ErrValidation = errors.New("validation failed")
	ErrRateLimit  = errors.New("rate limit exceeded")
)

// NotFoundError is returned when a requested resource does not exist.
type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s with id %q not found", e.Resource, e.ID)
}
func (e *NotFoundError) StatusCode() int   { return http.StatusNotFound }
func (e *NotFoundError) ErrorCode() string { return "RESOURCE_NOT_FOUND" }
func (e *NotFoundError) Unwrap() error     { return ErrNotFound }

// ConflictError is returned when a unique constraint is violated.
type ConflictError struct {
	Resource string
	Field    string
	Value    string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s with %s %q already exists", e.Resource, e.Field, e.Value)
}
func (e *ConflictError) StatusCode() int   { return http.StatusConflict }
func (e *ConflictError) ErrorCode() string { return "RESOURCE_CONFLICT" }
func (e *ConflictError) Unwrap() error     { return ErrConflict }

// ValidationError is returned when the caller's input is invalid.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Field + ": " + f.Message
	}
	return fmt.Sprintf("validation failed: %s", strings.Join(msgs, "; "))
}
func (e *ValidationError) StatusCode() int   { return http.StatusUnprocessableEntity }
func (e *ValidationError) ErrorCode() string { return "VALIDATION_FAILED" }
func (e *ValidationError) Unwrap() error     { return ErrValidation }

// RateLimitError is returned when the caller has exceeded their request budget.
type RateLimitError struct {
	RetryAfter int // seconds the caller must wait
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit exceeded; retry after %d seconds", e.RetryAfter)
}
func (e *RateLimitError) StatusCode() int   { return http.StatusTooManyRequests }
func (e *RateLimitError) ErrorCode() string { return "RATE_LIMIT_EXCEEDED" }
func (e *RateLimitError) Unwrap() error     { return ErrRateLimit }

// InternalError wraps an unexpected error without leaking details to the caller.
type InternalError struct {
	Err error
}

func (e *InternalError) Error() string     { return e.Err.Error() }
func (e *InternalError) StatusCode() int   { return http.StatusInternalServerError }
func (e *InternalError) ErrorCode() string { return "INTERNAL_ERROR" }
func (e *InternalError) Unwrap() error     { return e.Err }

// typeBase is the URI prefix for all error type URIs.
const typeBase = "https://api.example.com/errors/"

// ErrorCode returns the machine-readable code embedded in the Type URI.
func (pd ProblemDetail) ErrorCode() string {
	if len(pd.Type) > len(typeBase) {
		return pd.Type[len(typeBase):]
	}
	return ""
}

// BuildProblem converts any AppError into an RFC 7807 ProblemDetail.
// The instance field is the request path where the error occurred.
func BuildProblem(appErr AppError, instance string) ProblemDetail {
	status := appErr.StatusCode()
	detail := appErr.Error()
	if status >= 500 {
		detail = "An unexpected error occurred. Please try again later."
	}

	pd := ProblemDetail{
		Type:     typeBase + appErr.ErrorCode(),
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: instance,
	}

	var valErr *ValidationError
	if errors.As(appErr, &valErr) {
		pd.ValidationErrors = valErr.Fields
	}

	var rateErr *RateLimitError
	if errors.As(appErr, &rateErr) {
		pd.RetryAfter = rateErr.RetryAfter
	}

	return pd
}

// Respond writes an RFC 7807 JSON response derived from err.
// If err satisfies AppError the status and code are taken from it; otherwise
// the error is wrapped in an InternalError and a 500 is returned.
// The caller is responsible for logging the raw error before calling Respond.
func Respond(w http.ResponseWriter, r *http.Request, err error) {
	var appErr AppError
	if !errors.As(err, &appErr) {
		appErr = &InternalError{Err: err}
	}

	pd := BuildProblem(appErr, r.URL.Path)

	var rateErr *RateLimitError
	if errors.As(err, &rateErr) {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", rateErr.RetryAfter))
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(pd.Status)
	json.NewEncoder(w).Encode(pd) //nolint:errcheck
}
```

### Exercise 2: Tests and Example

Create `apierr_test.go`:

```go
package errresponse

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotFoundErrorSatisfiesAppError(t *testing.T) {
	t.Parallel()

	var appErr AppError
	err := &NotFoundError{Resource: "user", ID: "42"}
	if !errors.As(err, &appErr) {
		t.Fatal("NotFoundError does not satisfy AppError")
	}
	if appErr.StatusCode() != http.StatusNotFound {
		t.Fatalf("StatusCode = %d, want 404", appErr.StatusCode())
	}
	if appErr.ErrorCode() != "RESOURCE_NOT_FOUND" {
		t.Fatalf("ErrorCode = %q, want RESOURCE_NOT_FOUND", appErr.ErrorCode())
	}
}

func TestNotFoundErrorUnwrapsToSentinel(t *testing.T) {
	t.Parallel()

	err := &NotFoundError{Resource: "user", ID: "99"}
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("errors.Is(err, ErrNotFound) should be true")
	}
}

func TestValidationErrorCarriesFields(t *testing.T) {
	t.Parallel()

	err := &ValidationError{Fields: []FieldError{
		{Field: "name", Message: "is required"},
		{Field: "email", Message: "must contain @"},
	}}
	if err.StatusCode() != http.StatusUnprocessableEntity {
		t.Fatalf("StatusCode = %d, want 422", err.StatusCode())
	}
	if len(err.Fields) != 2 {
		t.Fatalf("want 2 field errors, got %d", len(err.Fields))
	}
}

func TestBuildProblemPopulatesFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        AppError
		wantStatus int
		wantCode   string
	}{
		{"not found", &NotFoundError{Resource: "item", ID: "1"}, 404, "RESOURCE_NOT_FOUND"},
		{"conflict", &ConflictError{Resource: "user", Field: "email", Value: "x@y.com"}, 409, "RESOURCE_CONFLICT"},
		{"validation", &ValidationError{Fields: []FieldError{{Field: "name", Message: "required"}}}, 422, "VALIDATION_FAILED"},
		{"rate limit", &RateLimitError{RetryAfter: 30}, 429, "RATE_LIMIT_EXCEEDED"},
		{"internal", &InternalError{Err: errors.New("db timeout")}, 500, "INTERNAL_ERROR"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pd := BuildProblem(tc.err, "/test")
			if pd.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", pd.Status, tc.wantStatus)
			}
			if pd.ErrorCode() != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", pd.ErrorCode(), tc.wantCode)
			}
			if pd.Instance != "/test" {
				t.Errorf("Instance = %q, want /test", pd.Instance)
			}
		})
	}
}

func TestBuildProblemHidesInternalDetail(t *testing.T) {
	t.Parallel()

	pd := BuildProblem(&InternalError{Err: errors.New("sql: connection refused")}, "/")
	if pd.Detail == "sql: connection refused" {
		t.Fatal("internal error detail must not leak to the caller")
	}
}

func TestBuildProblemAttachesValidationErrors(t *testing.T) {
	t.Parallel()

	fields := []FieldError{
		{Field: "email", Message: "must contain @"},
	}
	pd := BuildProblem(&ValidationError{Fields: fields}, "/users")
	if len(pd.ValidationErrors) != 1 {
		t.Fatalf("want 1 validation error, got %d", len(pd.ValidationErrors))
	}
	if pd.ValidationErrors[0].Field != "email" {
		t.Errorf("Field = %q, want email", pd.ValidationErrors[0].Field)
	}
}

func TestBuildProblemAttachesRetryAfter(t *testing.T) {
	t.Parallel()

	pd := BuildProblem(&RateLimitError{RetryAfter: 60}, "/api")
	if pd.RetryAfter != 60 {
		t.Fatalf("RetryAfter = %d, want 60", pd.RetryAfter)
	}
}

func TestRespondWritesContentType(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/users/99", nil)
	w := httptest.NewRecorder()

	Respond(w, req, &NotFoundError{Resource: "user", ID: "99"})

	res := w.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	ct := res.Header.Get("Content-Type")
	if ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestRespondWrapsUnknownErrorAs500(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	Respond(w, req, errors.New("unexpected db failure"))

	res := w.Result()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}

	var pd ProblemDetail
	if err := json.NewDecoder(res.Body).Decode(&pd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pd.Detail == "unexpected db failure" {
		t.Fatal("internal error detail must not appear in response body")
	}
}

func TestRespondSetsRetryAfterHeader(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	Respond(w, req, &RateLimitError{RetryAfter: 30})

	res := w.Result()
	if res.Header.Get("Retry-After") != "30" {
		t.Fatalf("Retry-After = %q, want 30", res.Header.Get("Retry-After"))
	}
}

func TestWrappedAppErrorIsUnwrapped(t *testing.T) {
	t.Parallel()

	// Simulate a domain layer that wraps the AppError.
	inner := &NotFoundError{Resource: "order", ID: "7"}
	wrapped := fmt.Errorf("order service: %w", inner)

	req := httptest.NewRequest(http.MethodGet, "/orders/7", nil)
	w := httptest.NewRecorder()
	Respond(w, req, wrapped)

	res := w.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — errors.As must unwrap the chain", res.StatusCode)
	}
}

// ExampleBuildProblem shows the RFC 7807 shape for a 404.
func ExampleBuildProblem() {
	err := &NotFoundError{Resource: "user", ID: "42"}
	pd := BuildProblem(err, "/users/42")
	fmt.Printf("status=%d code=%s\n", pd.Status, pd.ErrorCode())
	// Output: status=404 code=RESOURCE_NOT_FOUND
}
```

Your turn: add `TestConflictErrorUnwrapsToSentinel` that calls `errors.Is(err, ErrConflict)` on a `*ConflictError` and asserts it returns `true`.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	errresponse "example.com/errresponse"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// handler is a thin adapter: business logic returns an error, the adapter
	// calls errresponse.Respond if the error is non-nil.
	handle := func(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				if err2 := errors.Unwrap(err); err2 != nil {
					logger.Error("handler error", "err", err)
				}
				errresponse.Respond(w, r, err)
			}
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /users/{id}", handle(func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("id")
		users := map[string]string{"1": "Alice", "2": "Bob"}
		name, ok := users[id]
		if !ok {
			return &errresponse.NotFoundError{Resource: "user", ID: id}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "name": name}) //nolint:errcheck
		return nil
	}))

	mux.HandleFunc("POST /users", handle(func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return &errresponse.ValidationError{Fields: []errresponse.FieldError{
				{Field: "body", Message: "invalid JSON"},
			}}
		}

		var fieldErrs []errresponse.FieldError
		if body.Name == "" {
			fieldErrs = append(fieldErrs, errresponse.FieldError{Field: "name", Message: "is required"})
		}
		if body.Email == "" || !strings.Contains(body.Email, "@") {
			fieldErrs = append(fieldErrs, errresponse.FieldError{Field: "email", Message: "must contain @"})
		}
		if len(fieldErrs) > 0 {
			return &errresponse.ValidationError{Fields: fieldErrs}
		}

		if body.Email == "alice@example.com" {
			return &errresponse.ConflictError{Resource: "user", Field: "email", Value: body.Email}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": body.Name, "email": body.Email}) //nolint:errcheck
		return nil
	}))

	addr := ":8080"
	logger.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
}
```

Run the demo server with:

```bash
go run ./cmd/demo
```

In a second terminal:

```bash
# 404
curl -s localhost:8080/users/999 | jq .
# 422
curl -s -X POST localhost:8080/users -H 'Content-Type: application/json' \
  -d '{"name":"","email":"bad"}' | jq .
# 409
curl -s -X POST localhost:8080/users -H 'Content-Type: application/json' \
  -d '{"name":"Eve","email":"alice@example.com"}' | jq .
# 201
curl -s -X POST localhost:8080/users -H 'Content-Type: application/json' \
  -d '{"name":"Eve","email":"eve@example.com"}' | jq .
```

## Common Mistakes

### Switching on Error Type Instead of Using errors.As

Wrong: a middleware that does a `switch err.(type)` catches only the outermost type. An `AppError` wrapped two layers deep by domain code is invisible.

Fix: use `errors.As(err, &appErr)`. It walks the entire chain. Every concrete error type in this package implements `Unwrap()`, so wrapping never hides the underlying domain error.

### Leaking Internal Details in 5xx Responses

Wrong: returning `err.Error()` directly for any status, including 500. The message might contain a database DSN, a file path, or a connection string.

Fix: `BuildProblem` replaces the detail with a generic message for any status >= 500. Log the full error server-side with `slog`.

### Returning 200 with an Error Body

Wrong: calling `json.NewEncoder(w).Encode(errBody)` before setting the status code. `net/http` defaults to 200, so the body encodes as a success even though it describes a failure.

Fix: always call `w.WriteHeader(status)` before encoding the body. In `Respond`, `WriteHeader` is called before `Encode`.

### Forgetting the Content-Type Header

Wrong: writing a JSON body without setting `Content-Type: application/problem+json`. Clients and proxies inspect the content type to decide how to parse the body.

Fix: set the header before calling `WriteHeader` (headers are flushed on the first `WriteHeader` call).

### Validation Errors Without Field Detail

Wrong: returning a single-field `ValidationError` like `{Field: "input", Message: "invalid"}`. Clients cannot auto-populate the form field that failed.

Fix: collect all field errors before returning and include the exact field name and a message that describes the constraint that was violated.

## Verification

From `~/go-exercises/errresponse`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The test file is the verification — there is no program to eyeball.

## Summary

- Define an `AppError` interface with `StatusCode() int` and `ErrorCode() string` so the error chain carries HTTP metadata.
- Each concrete error type implements `Unwrap()` to expose a sentinel, enabling both `errors.Is` (sentinel) and `errors.As` (structured data) checks.
- `BuildProblem` converts an `AppError` to an RFC 7807 `ProblemDetail`; `Respond` writes it as `application/problem+json`.
- For 5xx errors replace the detail with a generic message before writing the response; log the full error server-side.
- `errors.As` walks the error chain, so domain code can wrap errors freely without breaking the middleware.

## What's Next

Next: [OpenTelemetry Instrumentation](../07-opentelemetry-instrumentation/07-opentelemetry-instrumentation.md).

## Resources

- [RFC 7807 — Problem Details for HTTP APIs](https://datatracker.ietf.org/doc/html/rfc7807)
- [pkg.go.dev/errors](https://pkg.go.dev/errors) — `errors.As`, `errors.Is`, `errors.New`
- [pkg.go.dev/net/http](https://pkg.go.dev/net/http) — `http.StatusText`, `ResponseWriter`, `httptest`
- [go.dev/blog/go1.13-errors](https://go.dev/blog/go1.13-errors) — error wrapping design rationale
- [go.dev/doc/effective_go#errors](https://go.dev/doc/effective_go#errors) — idiomatic Go error handling
