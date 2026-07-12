# Exercise 1: Unit-test handlers with httptest.NewRecorder

The fastest way to test an HTTP handler is to skip the network entirely: build a
request, run the handler against an in-memory `httptest.ResponseRecorder`, and
assert on what it captured. This module builds a small greeting/health API and
unit-tests its handler logic — status, headers, decoded JSON body — with a
recorder.

## What you'll build

```text
greetapi/                       independent module: example.com/recorder-unit-handler
  go.mod                        go 1.26
  api.go                        Greeting; GreetHandler, HealthHandler, NewMux
  cmd/
    demo/
      main.go                   runs GreetHandler against a recorder, prints the result
  api_test.go                   table-driven recorder tests; Code, Header, decoded body
```

- Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
- Implement: `GreetHandler` (JSON greeting from the path, 400 on missing name), `HealthHandler`, and `NewMux` wiring both.
- Test: table-driven cases (valid name, missing name, health) calling the handler with a recorder; decode `rec.Body` into `Greeting`; assert `rec.Code` and the `Content-Type` header.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/09-httptest/01-recorder-unit-handler/cmd/demo
cd go-solutions/12-testing-ecosystem/09-httptest/01-recorder-unit-handler
```

### Why a recorder, and what it captures

`httptest.NewRecorder()` returns a `*httptest.ResponseRecorder` that implements
`http.ResponseWriter` entirely in memory. There is no listener, no port, no
serving goroutine — you call the handler as a plain function,
`GreetHandler(rec, req)`, and everything it writes lands in the recorder. Three
fields carry the result: `rec.Code` is the status the handler passed to
`WriteHeader` (defaulting to `200` if it only called `Write`), `rec.Body` is a
`*bytes.Buffer` holding the response bytes, and `rec.Header()` is the header map
the handler mutated. This is the right tool for handler *logic*: given this
request, does the handler produce the right status, headers, and body?

The request side uses `httptest.NewRequest(method, target, body)`, which builds a
*server-side* `*http.Request` — the shape a handler receives, with `RemoteAddr`
set. It is not something you send over a wire; it is something you feed to a
handler. That is exactly what we want here.

`GreetHandler` derives the name from the path with
`strings.TrimPrefix(r.URL.Path, "/greet/")`. When the request targets exactly
`/greet/` the trimmed result is empty; when it does not start with `/greet/` the
`TrimPrefix` returns the path unchanged, so `name == r.URL.Path` detects that too.
Either case is a client error, and `http.Error(w, msg, http.StatusBadRequest)`
writes a `400` with a plain-text body. On success the handler sets
`Content-Type: application/json` and streams the struct with a
`json.NewEncoder(w).Encode(...)`, which appends a trailing newline — a detail the
test accounts for by decoding rather than doing a raw string compare.

Create `api.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Greeting is the JSON payload returned by GreetHandler.
type Greeting struct {
	Message string `json:"message"`
}

// GreetHandler writes a JSON greeting for the name in the path (/greet/{name}).
// A missing name is a 400.
func GreetHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/greet/")
	if name == "" || name == r.URL.Path {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Greeting{Message: fmt.Sprintf("Hello, %s", name)})
}

// HealthHandler reports service liveness as JSON.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// NewMux wires the routes onto a fresh ServeMux.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", HealthHandler)
	mux.HandleFunc("/greet/", GreetHandler)
	return mux
}
```

### The demo

The demo exercises the handler exactly as the tests do — through a recorder — so
it needs no port. It runs `GreetHandler` for `/greet/ada`, decodes the JSON, and
prints the status and message.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	api "example.com/recorder-unit-handler"
)

func main() {
	req := httptest.NewRequest(http.MethodGet, "/greet/ada", nil)
	rec := httptest.NewRecorder()
	api.GreetHandler(rec, req)

	fmt.Printf("status: %d\n", rec.Code)
	fmt.Printf("content-type: %s\n", rec.Header().Get("Content-Type"))

	var g api.Greeting
	_ = json.NewDecoder(rec.Body).Decode(&g)
	fmt.Printf("message: %s\n", g.Message)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
content-type: application/json
message: Hello, ada
```

### Tests

The tests are table-driven. Each case builds a request, runs the target handler
against a fresh recorder, and asserts the status and `Content-Type`; the greeting
cases additionally decode `rec.Body` into a `Greeting` and compare the message.
Each subtest calls `t.Parallel()` — the handlers hold no shared state, so this is
safe. Note that decoding the body is more robust than a string compare: it does
not care about the encoder's trailing newline or key ordering.

Create `api_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		handler     http.HandlerFunc
		target      string
		wantCode    int
		wantCT      string
		wantMessage string // empty means do not decode a Greeting
	}{
		{
			name:        "greet valid name",
			handler:     GreetHandler,
			target:      "/greet/alice",
			wantCode:    http.StatusOK,
			wantCT:      "application/json",
			wantMessage: "Hello, alice",
		},
		{
			name:     "greet missing name",
			handler:  GreetHandler,
			target:   "/greet/",
			wantCode: http.StatusBadRequest,
			wantCT:   "text/plain; charset=utf-8",
		},
		{
			name:     "health ok",
			handler:  HealthHandler,
			target:   "/healthz",
			wantCode: http.StatusOK,
			wantCT:   "application/json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			rec := httptest.NewRecorder()
			tc.handler(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantCT {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantCT)
			}
			if tc.wantMessage != "" {
				var g Greeting
				if err := json.NewDecoder(rec.Body).Decode(&g); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if g.Message != tc.wantMessage {
					t.Fatalf("message = %q, want %q", g.Message, tc.wantMessage)
				}
			}
		})
	}
}

func TestMuxRoutes(t *testing.T) {
	t.Parallel()

	mux := NewMux()
	req := httptest.NewRequest(http.MethodGet, "/greet/bob", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var g Greeting
	if err := json.NewDecoder(rec.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	if g.Message != "Hello, bob" {
		t.Fatalf("message = %q, want Hello, bob", g.Message)
	}
}

func ExampleGreetHandler() {
	req := httptest.NewRequest(http.MethodGet, "/greet/ada", nil)
	rec := httptest.NewRecorder()
	GreetHandler(rec, req)

	var g Greeting
	_ = json.NewDecoder(rec.Body).Decode(&g)
	println(g.Message)
	// Output:
}
```

The `ExampleGreetHandler` uses `println` (which writes to stderr, not captured by
the example runner) so it has an empty `// Output:`; its value is that it compiles
and documents the recorder pattern. The real assertions live in the table test.
As a "your turn" addition, add a case that posts a name containing a slash (e.g.
`/greet/a/b`) and decide, then assert, what the greeting should be.

## Review

The recorder is correct for this lesson because the questions are about handler
*logic*, not wire behavior: status, headers, and body shape given an input
request. Decoding `rec.Body` into `Greeting` rather than string-matching is the
detail that keeps the test robust against the JSON encoder's trailing newline. The
`400` path returns Go's default `text/plain; charset=utf-8` from `http.Error`,
which the missing-name case pins exactly — asserting the content type of an error
response is a real contract, not a nicety. Run `go test -race` even though there
is no obvious concurrency; the parallel subtests share the handlers, and `-race`
confirms they hold no hidden mutable state.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder`, `NewRequest`, and `ResponseRecorder`.
- [net/http `ServeMux`](https://pkg.go.dev/net/http#ServeMux) — routing semantics behind `NewMux`.
- [encoding/json `NewEncoder`/`NewDecoder`](https://pkg.go.dev/encoding/json#NewEncoder) — streaming JSON and the trailing newline.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-newserver-e2e-handler.md](02-newserver-e2e-handler.md)
