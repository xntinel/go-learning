# Exercise 9: Bound request bodies and map decode failures

An ingestion endpoint that decodes an unbounded body is a denial-of-service
waiting to happen. This exercise builds a JSON POST endpoint that wraps the body
with `http.MaxBytesReader`, decodes with `DisallowUnknownFields`, and maps the
typed failures in the boundary: `*http.MaxBytesError` to 413, JSON syntax/type
errors and `io.EOF` to 400, via `errors.As`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
bodylimit/                   independent module: example.com/bodylimit
  go.mod                     go 1.26
  ingest.go                  DecodeJSON (MaxBytesReader + DisallowUnknownFields); WriteError maps 413/400
  cmd/
    demo/
      main.go                runnable demo: valid, oversized, malformed, unknown-field
  ingest_test.go             oversized->413, malformed->400, unknown->400, valid->201
```

Files: `ingest.go`, `cmd/demo/main.go`, `ingest_test.go`.
Implement: `DecodeJSON` that caps the body with `http.MaxBytesReader`, decodes with
`DisallowUnknownFields`, and returns typed errors; a boundary mapping
`*http.MaxBytesError`->413 and JSON/`io.EOF` errors->400 via `errors.As`.
Test: oversized body -> 413 (assert `errors.As` finds `*http.MaxBytesError`);
malformed JSON -> 400; unknown field -> 400; valid body -> 201.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/10-error-handling-middleware/09-request-body-limit/cmd/demo
cd go-solutions/10-error-handling/10-error-handling-middleware/09-request-body-limit
```

### Why bounding the body is not optional

`json.NewDecoder(r.Body).Decode(&v)` reads until the body ends. A client that
sends a multi-gigabyte body — or a slow trickle that never ends — makes the server
allocate without limit; one request can exhaust the instance's memory. The fix is
`http.MaxBytesReader(w, r.Body, n)`, which replaces the body with a reader that
returns an error once more than `n` bytes are read *and* signals the server to
close the connection. Its overflow error is a `*http.MaxBytesError` (added in Go
1.19), which carries the `Limit` that was exceeded. This maps to **413 Request
Entity Too Large**, not 400 — the request was well-formed, just too big, and 413 is
the status that tells the client so.

Two more hardening steps go with it. `decoder.DisallowUnknownFields()` makes the
decoder reject a body with fields not present in the target struct, which catches
typos and unexpected input and returns a descriptive error. And after decoding one
value you should check for a *second* one (`decoder.Decode(&struct{}{})` returning
anything but `io.EOF`) to reject trailing garbage like `{"a":1}{"b":2}`. Each of
these failures is a *client* error mapping to 400.

The boundary distinguishes the cases with `errors.As`, which extracts a typed
error from anywhere in the wrap chain:

- `*http.MaxBytesError` -> 413.
- `*json.SyntaxError` (malformed JSON), `*json.UnmarshalTypeError` (wrong JSON type
  for a field), and the `DisallowUnknownFields` error (a plain error whose message
  starts with `json: unknown field`) -> 400.
- `io.EOF` for an empty body -> 400.

The Go 1.26 addition `errors.AsType[E](err)` does the same extraction with a
generic return instead of a pointer-to-pointer target; either works, and the
concepts file notes it. This module uses `errors.As` for the typed cases and
`errors.Is` for `io.EOF`.

Create `ingest.go`:

```go
package bodylimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBody caps request bodies at 1 KiB for the demo; production picks a limit per
// endpoint.
const maxBody = 1 << 10

// User is the ingestion payload.
type User struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// ErrBadRequest and ErrTooLarge classify decode failures for the boundary.
var (
	ErrBadRequest = errors.New("bad request")
	ErrTooLarge   = errors.New("request too large")
)

// DecodeJSON caps the body, decodes strictly into dst, and returns a classified
// error: ErrTooLarge (wraps *http.MaxBytesError) or ErrBadRequest for malformed
// input. The caller maps these to 413 / 400.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var synErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &maxErr):
			// Multi-%w keeps the *http.MaxBytesError in the chain (for errors.As)
			// while classifying the failure as ErrTooLarge (for errors.Is).
			return fmt.Errorf("body exceeds %d bytes: %w: %w", maxErr.Limit, err, ErrTooLarge)
		case errors.As(err, &synErr):
			return fmt.Errorf("malformed json at offset %d: %w", synErr.Offset, ErrBadRequest)
		case errors.As(err, &typeErr):
			return fmt.Errorf("field %q wrong type: %w", typeErr.Field, ErrBadRequest)
		case errors.Is(err, io.EOF):
			return fmt.Errorf("empty body: %w", ErrBadRequest)
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			return fmt.Errorf("%v: %w", err, ErrBadRequest)
		default:
			return fmt.Errorf("decode: %w", ErrBadRequest)
		}
	}

	// Reject trailing data after the first JSON value.
	if dec.More() {
		return fmt.Errorf("unexpected trailing data: %w", ErrBadRequest)
	}
	return nil
}

// WriteError maps the classified errors to statuses.
func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrBadRequest):
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status)})
}

// CreateUser is the ingestion handler: decode strictly, else map the failure.
func CreateUser(w http.ResponseWriter, r *http.Request) {
	var u User
	if err := DecodeJSON(w, r, &u); err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(u)
}
```

### The runnable demo

The demo posts four bodies — valid, oversized, malformed, and with an unknown
field — and prints the status each maps to.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/bodylimit"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(bodylimit.CreateUser))
	defer srv.Close()

	cases := []struct {
		name string
		body string
	}{
		{"valid", `{"name":"alice","age":30}`},
		{"oversized", `{"name":"` + strings.Repeat("x", 2000) + `"}`},
		{"malformed", `{"name":`},
		{"unknown", `{"name":"bob","role":"admin"}`},
	}
	for _, c := range cases {
		resp, err := http.Post(srv.URL+"/users", "application/json", strings.NewReader(c.body))
		if err != nil {
			panic(err)
		}
		resp.Body.Close()
		fmt.Printf("%-10s -> %d\n", c.name, resp.StatusCode)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid      -> 201
oversized  -> 413
malformed  -> 400
unknown    -> 400
```

### Tests

The table drives each failure mode through the real handler with `httptest` and
asserts the mapped status. A dedicated `TestOversizedIsMaxBytesError` posts an
oversized body and asserts, via `errors.As` on the error `DecodeJSON` returns, that
the underlying cause really is `*http.MaxBytesError` — proving the 413 is not an
accident of message matching.

Create `ingest_test.go`:

```go
package bodylimit

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(body))
	CreateUser(rec, req)
	return rec
}

func TestIngestStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"valid", `{"name":"alice","age":30}`, http.StatusCreated},
		{"oversized", `{"name":"` + strings.Repeat("x", 2000) + `"}`, http.StatusRequestEntityTooLarge},
		{"malformed", `{"name":`, http.StatusBadRequest},
		{"wrong type", `{"name":"a","age":"thirty"}`, http.StatusBadRequest},
		{"unknown field", `{"name":"b","role":"admin"}`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"trailing data", `{"name":"a"}{"name":"b"}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := post(t, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestOversizedIsMaxBytesError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/users",
		strings.NewReader(`{"name":"`+strings.Repeat("x", 5000)+`"}`))

	var u User
	err := DecodeJSON(rec, req, &u)
	if err == nil {
		t.Fatal("expected an error for oversized body")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("error %v does not wrap *http.MaxBytesError", err)
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error %v is not classified as ErrTooLarge", err)
	}
}

func ExampleDecodeJSON() {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`{"name":"z","age":9}`))
	var u User
	err := DecodeJSON(rec, req, &u)
	fmt.Println(u.Name, u.Age, err)
	// Output: z 9 <nil>
}
```

## Review

The endpoint is correct when every ill-formed or oversized input maps to the right
status and no input can allocate without bound. `TestOversizedIsMaxBytesError` is
the load-bearing assertion: it proves the 413 comes from a real
`*http.MaxBytesError` reached through `errors.As`, not from a fragile string
match, and that the same error is classified `ErrTooLarge`. The common bug this
guards against is mapping the overflow to 400 — a size failure is a 413, and
conflating the two misleads clients about whether shrinking the payload will help.
`DisallowUnknownFields` plus the trailing-data check turn silent acceptance of
malformed input into explicit 400s. Run `-race`; the decode path and the
`MaxBytesReader`-triggered connection close must be clean.

## Resources

- [`net/http#MaxBytesReader`](https://pkg.go.dev/net/http#MaxBytesReader) — capping the body and the `*http.MaxBytesError` it returns.
- [`net/http#MaxBytesError`](https://pkg.go.dev/net/http#MaxBytesError) — the typed overflow error mapped to 413.
- [`encoding/json#Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — strict decoding that rejects unexpected fields.
- [`errors#As`](https://pkg.go.dev/errors#As) — extracting the typed cause from the wrap chain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-log-once-at-boundary.md](10-log-once-at-boundary.md)
