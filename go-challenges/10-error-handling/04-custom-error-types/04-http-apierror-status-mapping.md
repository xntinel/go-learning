# Exercise 4: An APIError Type That Maps Domain Failures to HTTP Responses

At the transport edge, a domain error becomes an HTTP response. This module builds
an `APIError{Status, Code, Message, Err}` and a `WriteError` responder that uses
`errors.As` to pull the typed error (falling back to a generic 500 for untyped
errors), writes a JSON body with a machine-readable `Code`, and keeps the internal
cause out of the wire payload while leaving it reachable for logging.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
apierr/                    independent module: example.com/apierr
  go.mod                   go 1.24
  apierr.go                APIError{Status,Code,Message,Err}; WriteError responder
  cmd/
    demo/
      main.go              serves one typed and one untyped error via httptest
  apierr_test.go           status/body/Content-Type; 500 fallback; no-leak; As
```

Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
Implement: `*APIError` with `Error()`/`Unwrap()`, constructors, and `WriteError(w, err)` that maps typed errors to their status and untyped errors to 500 without leaking the cause.
Test: an `*APIError` yields its status, `Content-Type: application/json`, and `Code`/`Message`; a bare error maps to 500 with a generic message and no leaked internal text; the internal `Err` stays reachable via `errors.As`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The wire representation is not the internal error

`APIError` carries four fields with two distinct audiences. `Status` and the
JSON-serialized `Code` and `Message` are for the *client*: stable, safe, and
machine-readable — a frontend can switch on `Code == "USER_NOT_FOUND"` without
parsing prose. `Err` is for the *server*: the wrapped internal cause (a SQL error,
a validation failure, a downstream timeout) that belongs in the logs and nowhere
near the response body. `Unwrap()` exposes `Err` so a logging middleware can walk
the full chain, but `WriteError` serializes only `Code` and `Message`.

This split is a security boundary, not a style preference. A response body that
echoes `err.Error()` can leak SQL fragments, internal hostnames and ports, file
paths, or stack details to an attacker. `WriteError` therefore serializes a fixed
DTO (`Code`, `Message`) and never the internal chain.

### The 500 fallback for untyped errors

`WriteError` uses `errors.As` to find an `*APIError` anywhere in the chain. If one
is present, it uses that error's `Status`/`Code`/`Message`. If not — a bare
`errors.New`, a panic recovered into an error, anything the code did not classify —
it must not guess: it emits `500 Internal Server Error` with a generic
`Code: "INTERNAL"` and a generic message, and (in real code) logs the actual error
server-side. The invariant is that an unclassified error never reaches the client
as anything but an opaque 500. This is why the no-leak test drives a bare error and
asserts the body contains neither the internal text nor a 200-range status.

Create `apierr.go`:

```go
// Package apierr maps domain errors to HTTP responses via a typed APIError,
// serializing a safe Code/Message to clients while keeping the internal cause for
// logs.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// APIError is the transport-edge error. Status/Code/Message are the client-facing
// wire representation; Err is the internal cause, kept for logging and never
// serialized to the body.
type APIError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s (%d): %v", e.Code, e.Status, e.Err)
	}
	return fmt.Sprintf("%s (%d)", e.Code, e.Status)
}

// Unwrap exposes the internal cause for logging middleware; it is NOT serialized.
func (e *APIError) Unwrap() error { return e.Err }

// NewAPIError constructs a typed API error wrapping an internal cause.
func NewAPIError(status int, code, message string, cause error) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Err: cause}
}

// errorBody is the exact DTO sent to clients. It deliberately omits the internal
// cause.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes err as a JSON HTTP error. A typed *APIError uses its own
// status/code/message; any other error falls back to a generic 500 with no leak
// of the internal text.
func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := errorBody{Code: "INTERNAL", Message: "internal server error"}

	var ae *APIError
	if errors.As(err, &ae) {
		status = ae.Status
		body = errorBody{Code: ae.Code, Message: ae.Message}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
```

### The runnable demo

The demo serves two handlers through `httptest`: one returns a typed 404
`APIError`, the other a bare error that must become a generic 500.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"

	"example.com/apierr"
)

func main() {
	// Typed error: a 404 with a machine-readable code.
	rec := httptest.NewRecorder()
	apierr.WriteError(rec, apierr.NewAPIError(404, "USER_NOT_FOUND", "user not found", errors.New("sql: no rows in result set")))
	dump(rec)

	// Untyped error: must become a generic 500, no leak of the cause.
	rec2 := httptest.NewRecorder()
	apierr.WriteError(rec2, errors.New("connection refused to 10.0.3.14:5432"))
	dump(rec2)
}

func dump(rec *httptest.ResponseRecorder) {
	body, _ := io.ReadAll(rec.Result().Body)
	fmt.Printf("status=%d body=%s", rec.Code, body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=404 body={"code":"USER_NOT_FOUND","message":"user not found"}
status=500 body={"code":"INTERNAL","message":"internal server error"}
```

### Tests

`TestWriteTypedError` asserts the status, `Content-Type`, and JSON `Code`/`Message`
for a typed error. `TestWriteUntypedFallsBackTo500` drives a bare error and asserts
a 500 with the generic code. `TestNoLeak` is the security guard: it puts a secret
internal string in the cause and asserts it never appears in the body.
`TestInternalCauseReachable` proves the cause is still available to logging via
`errors.As`.

Create `apierr_test.go`:

```go
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func decode(t *testing.T, r io.Reader) errorBody {
	t.Helper()
	var b errorBody
	if err := json.NewDecoder(r).Decode(&b); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return b
}

func TestWriteTypedError(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	WriteError(rec, NewAPIError(404, "USER_NOT_FOUND", "user not found", errors.New("no rows")))

	res := rec.Result()
	if res.StatusCode != 404 {
		t.Errorf("status = %d; want 404", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
	body := decode(t, res.Body)
	if body.Code != "USER_NOT_FOUND" || body.Message != "user not found" {
		t.Errorf("body = %+v; want code=USER_NOT_FOUND message=user not found", body)
	}
}

func TestWriteUntypedFallsBackTo500(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	WriteError(rec, errors.New("some unclassified failure"))

	res := rec.Result()
	if res.StatusCode != 500 {
		t.Errorf("status = %d; want 500", res.StatusCode)
	}
	body := decode(t, res.Body)
	if body.Code != "INTERNAL" {
		t.Errorf("code = %q; want INTERNAL", body.Code)
	}
}

func TestNoLeak(t *testing.T) {
	t.Parallel()
	secret := "connection refused to 10.0.3.14:5432"
	rec := httptest.NewRecorder()

	// Even a typed error must not serialize its internal cause.
	WriteError(rec, NewAPIError(503, "UNAVAILABLE", "service unavailable", errors.New(secret)))

	raw, _ := io.ReadAll(rec.Result().Body)
	if strings.Contains(string(raw), secret) {
		t.Errorf("response body leaked the internal cause: %s", raw)
	}
	if strings.Contains(string(raw), "10.0.3.14") {
		t.Error("response body leaked an internal host/port")
	}
}

func TestInternalCauseReachable(t *testing.T) {
	t.Parallel()
	cause := errors.New("root cause")
	err := NewAPIError(500, "INTERNAL", "internal", cause)

	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatal("errors.As should extract *APIError")
	}
	if !errors.Is(ae, cause) {
		t.Error("internal cause must stay reachable via the chain for logging")
	}
}

func ExampleWriteError() {
	rec := httptest.NewRecorder()
	WriteError(rec, NewAPIError(403, "FORBIDDEN", "not allowed", nil))
	raw, _ := io.ReadAll(rec.Result().Body)
	// trim the trailing newline the JSON encoder adds
	fmt.Print(strings.TrimSpace(string(raw)))
	// Output: {"code":"FORBIDDEN","message":"not allowed"}
}
```

## Review

The responder is correct when it presents two separate faces of an error. To the
client it writes only `Status` plus a JSON `Code`/`Message` DTO — `TestNoLeak`
proves the internal cause, including a host and port, never crosses the wire — and
an unclassified error becomes an opaque 500 rather than a leaked stack. To the
server, `Unwrap` keeps the full chain reachable so logging middleware can record
the real cause (`TestInternalCauseReachable`). The `errors.As` lookup is what lets
one `WriteError` handle every typed error uniformly while still catching the
untyped tail with a safe default. Run `go test -race` to confirm the four
behaviors, and note the handler never imports `database/sql` — classification
happened at the repository boundary in Exercise 3.

## Resources

- [net/http: ResponseWriter](https://pkg.go.dev/net/http#ResponseWriter) — `Header().Set`, `WriteHeader`, and status constants.
- [net/http/httptest: ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder) — capturing a handler's response in tests.
- [encoding/json: Encoder](https://pkg.go.dev/encoding/json#Encoder) — streaming a DTO to the response body.
- [errors: As](https://pkg.go.dev/errors#As) — extracting the typed error to pick the status.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-repository-error-translation.md](03-repository-error-translation.md) | Next: [05-aggregated-validation-errors.md](05-aggregated-validation-errors.md)
