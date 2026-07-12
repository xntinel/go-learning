# Exercise 1: Build the shared platform library every service imports

Every service in a platform monorepo should return errors the same way and stamp
requests with the same kind of identifier. That shared behavior belongs in one
package that every service imports. This exercise builds `platform/httpx`: a
structured JSON error envelope plus a request-id generator, laid out in the
single-module topology (one `go.mod` at the repo root, shared code under a
top-level package, services under `cmd/`). It is the anchor artifact the next
exercises consume.

## What you'll build

```text
mono/                         single module: example.com/mono
  go.mod                      one go.mod for the whole repo
  platform/
    httpx/
      httpx.go                Envelope, APIError, WriteError, RequestID
      httpx_test.go           JSON round-trip + empty-message + errors.Is tests
  cmd/
    demo/
      main.go                 renders an error envelope to an httptest recorder
```

- Files: `platform/httpx/httpx.go`, `platform/httpx/httpx_test.go`, `cmd/demo/main.go`.
- Implement: `Envelope`, an `APIError` carrying an HTTP status and a stable code, `WriteError(w, *APIError)` that emits the JSON envelope with the right status, a sentinel `ErrNotFound`, and `RequestID()`.
- Test: table-driven marshal/unmarshal round-trip of the envelope (status, code, message), an empty-message edge case that still yields well-formed JSON, and an `errors.Is` check against the sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the single module. One `go.mod` at the root is the whole point of the

### Why a shared envelope, and why it lives here

The value of a monorepo is one implementation of cross-cutting behavior shared by
many services. Error responses are the canonical example: if `cmd/api` returns
`{"error":"not found"}` and `cmd/worker` logs `job failed: 404`, clients and
dashboards see two shapes for the same condition. Centralizing the envelope in
`platform/httpx` means every service that imports it is consistent by
construction, and a change to the shape is one edit with one atomic version — the
single-module payoff from the concepts file.

`APIError` carries three things: an HTTP `Status` (which never appears twice — it
is both the response code and a field in the body), a stable machine-readable
`Code` string that clients switch on, and a human `Message`. It implements
`error`, so a handler can `return httpx.NewError(...)` and callers can wrap it
with `%w`. `WriteError` is the one place that knows how to turn an `*APIError`
into an HTTP response: set the content type, write the status header once, encode
the `Envelope`. Keeping the wire shape in a dedicated `Envelope` struct (rather
than marshaling `APIError` directly) means the internal error type and the public
JSON contract can evolve independently.

`RequestID` returns a short random hex string. It reads from `crypto/rand`, so it
is unpredictable (a request id doubles as a weak correlation token and should not
be guessable/sequential). It is deliberately tiny and dependency-free so every
service can stamp a request without pulling in a UUID library.

The edge case worth pinning is an empty message. A senior reviewer's instinct is
"what does this do with degenerate input" — here, an `APIError` with an empty
`Message` must still serialize to *well-formed* JSON (`"message":""`), never a
truncated or invalid body. That is the modern echo of the original "handles empty
name" contract, and the test asserts it.

Create `platform/httpx/httpx.go`:

```go
package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
)

// Envelope is the JSON error body every service in the monorepo returns. Keeping
// the wire shape separate from APIError lets the internal error type and the
// public JSON contract evolve independently.
type Envelope struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError is a structured error carrying an HTTP status and a stable, machine-
// readable code. It implements error, so handlers return it and callers wrap it.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Message, e.Status)
}

// ErrNotFound is the shared sentinel for a missing resource. Handlers wrap it
// with %w so callers can match it with errors.Is.
var ErrNotFound = &APIError{
	Status:  http.StatusNotFound,
	Code:    "not_found",
	Message: "resource not found",
}

// NewError builds an APIError for an ad-hoc condition.
func NewError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// WriteError renders e as a JSON envelope with the matching HTTP status code.
func WriteError(w http.ResponseWriter, e *APIError) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	return json.NewEncoder(w).Encode(Envelope{
		Status:  e.Status,
		Code:    e.Code,
		Message: e.Message,
	})
}

// RequestID returns a random 16-hex-character request identifier read from
// crypto/rand, so it is unpredictable and safe to use as a correlation token.
func RequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never fails on supported platforms; be explicit.
		panic("httpx: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
```

### The runnable demo

The demo renders an error the way a handler would — through `WriteError` into an
`httptest.ResponseRecorder` — then prints the captured status and body. It also
prints the length of a `RequestID` (16 hex chars) so the output stays
deterministic even though the id itself is random.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"strings"

	"example.com/mono/platform/httpx"
)

func main() {
	rec := httptest.NewRecorder()
	if err := httpx.WriteError(rec, httpx.ErrNotFound); err != nil {
		fmt.Println("encode error:", err)
		return
	}

	body := strings.TrimSpace(rec.Body.String())
	fmt.Printf("status=%d body=%s\n", rec.Code, body)
	fmt.Printf("request-id length: %d\n", len(httpx.RequestID()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=404 body={"status":404,"code":"not_found","message":"resource not found"}
request-id length: 16
```

### Tests

The tests pin the JSON contract. `TestEnvelopeRoundTrip` is table-driven: it
writes each `APIError` through `WriteError`, then unmarshals the recorded body
back into an `Envelope` and asserts all three fields survived — including the
empty-message row, which proves degenerate input still yields well-formed JSON.
`TestWriteErrorStatus` confirms `WriteError` sets the HTTP status header, not just
the body field. `TestErrNotFoundIs` wraps the sentinel with `%w` and matches it
with `errors.Is`, the pattern every handler relies on.

Create `platform/httpx/httpx_test.go`:

```go
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *APIError
	}{
		{"not found", ErrNotFound},
		{"bad request", NewError(http.StatusBadRequest, "bad_request", "id must be a positive integer")},
		{"empty message still valid", NewError(http.StatusConflict, "conflict", "")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			if err := WriteError(rec, tc.err); err != nil {
				t.Fatalf("WriteError: %v", err)
			}

			var got Envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not valid JSON: %v (%q)", err, rec.Body.String())
			}

			want := Envelope{Status: tc.err.Status, Code: tc.err.Code, Message: tc.err.Message}
			if got != want {
				t.Errorf("round-trip = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWriteErrorStatus(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if err := WriteError(rec, ErrNotFound); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestErrNotFoundIs(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("loading user 42: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("errors.Is could not match the wrapped ErrNotFound sentinel")
	}
}

func TestRequestIDShape(t *testing.T) {
	t.Parallel()

	id := RequestID()
	if len(id) != 16 {
		t.Errorf("RequestID length = %d, want 16", len(id))
	}
	if id == RequestID() {
		t.Error("two RequestID calls returned the same value")
	}
}

func ExampleWriteError() {
	rec := httptest.NewRecorder()
	_ = WriteError(rec, NewError(http.StatusBadRequest, "bad_request", "missing field"))
	fmt.Print(rec.Body.String())
	// Output: {"status":400,"code":"bad_request","message":"missing field"}
}
```

## Review

The library is correct when the wire shape is exactly what the `Envelope` struct
declares and nothing else can change it: `WriteError` sets the status header once,
sets the content type, and encodes the three fields, so the recorded body always
round-trips back to an equal `Envelope`. The empty-message row is the honest edge
case — an `APIError` with no message must still marshal to `"message":""`, and the
test fails loudly if the code ever produced a truncated or invalid body instead.

Two structural traps. First, do not marshal `APIError` directly to the wire —
route it through `Envelope`, so the internal error representation and the public
JSON contract are free to diverge later without breaking clients. Second, keep
`RequestID` on `crypto/rand`, not `math/rand`: a correlation token that is
guessable or sequential leaks information and collides across restarts. Run
`go test -race` to confirm the package is clean under the race detector even
though it holds no shared state yet — the services in the next exercises will.

## Resources

- [`encoding/json`](https://pkg.go.dev/encoding/json) — `Marshal`, `NewEncoder`, and struct tag semantics for the envelope.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` for asserting a handler's status and body with no network.
- [`crypto/rand`](https://pkg.go.dev/crypto/rand) — the CSPRNG behind `RequestID`.
- [Error handling and Go](https://go.dev/blog/error-handling-and-go) — sentinels, wrapping with `%w`, and `errors.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-service-a-http-api.md](02-service-a-http-api.md)
