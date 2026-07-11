# 1. HTTP Server with net/http

Build a small HTTP service whose handlers demonstrate the four interactions every
`net/http` server must handle correctly: writing a body, reading the request URL
and method, returning a non-200 status, and registering routes on an isolated
`ServeMux`. The lesson focuses on the difference between the package-level
`http.HandleFunc` (registers on the global `DefaultServeMux`) and a mux created
with `http.NewServeMux()` (isolated, testable, the only safe choice in a real
program).

```text
httpserver/
  go.mod
  internal/server/server.go
  internal/server/server_test.go
  cmd/demo/main.go
```

The package exposes a `Server` type with a configurable address and a `Handler`
method that returns the `http.Handler`. The test exercises the standard cases
with `httptest`, the verification commands compile and run that test.

## Concepts

### `http.Handler` Is The Contract

An `http.Handler` is any value with a `ServeHTTP(http.ResponseWriter, *http.Request)`
method. `http.HandlerFunc` adapts a plain function to the interface. Handlers
are values: pass them around, wrap them, compose them, test them in isolation
with `httptest.NewRecorder`.

### `ResponseWriter` Is The Output Stream

The handler receives an `http.ResponseWriter`. Writing to it produces the
response body. Headers must be set before the first `Write` (or `WriteHeader`)
call, because the first write flushes the status line and headers. `WriteHeader`
sets the status code; calling it after `Write` logs a warning and has no effect.

### `*http.Request` Is The Input Value

The handler receives a pointer to the incoming request. `r.Method`, `r.URL`,
`r.Header`, `r.Body`, and `r.Context()` carry everything the server knows about
the call. `r.URL.Query()` parses the query string lazily into a `url.Values`
map.

### `ServeMux` Routes Patterns To Handlers

A `ServeMux` matches a request path to a handler. `http.NewServeMux()` creates
an isolated mux; `http.DefaultServeMux` is a global package-level value that any
imported package can register on. A library or testable binary should never
register on the default mux.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/httpserver/internal/server
mkdir -p ~/go-exercises/httpserver/cmd/demo
cd ~/go-exercises/httpserver
go mod init example.com/httpserver
```

### Exercise 1: The Server Type And Its Handlers

Create `internal/server/server.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Server struct {
	addr    string
	started time.Time
}

func New(addr string) *Server {
	return &Server{addr: addr}
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) helloHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "World"
	}
	fmt.Fprintf(w, "Hello, %s!\n", name)
	fmt.Fprintf(w, "Method: %s\n", r.Method)
	fmt.Fprintf(w, "Path: %s\n", r.URL.Path)
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", s.helloHandler)
	mux.HandleFunc("GET /health", s.healthHandler)
	return mux
}
```

The `Handler` method returns a fresh mux each call, so tests that call it twice
get independent state. Registering method-specific patterns (`GET /hello`) lets
`ServeMux` answer a wrong method with 405 on its own, and any unmatched path
falls through to the mux's built-in 404. Deliberately not registering a `/`
catch-all is what preserves the automatic 405: a `/` handler matches every
request, including `POST /hello`, and would turn that 405 into whatever the
catch-all returns.

### Exercise 2: Test The Handlers With `httptest`

Create `internal/server/server_test.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHelloUsesQueryName(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/hello?name=Gopher", nil)
	rr := httptest.NewRecorder()

	New(":0").Handler().ServeHTTP(rr, req)

	if got := rr.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	body, _ := io.ReadAll(rr.Body)
	if want := "Hello, Gopher!\n"; string(body[:len(want)]) != want {
		t.Fatalf("body = %q, want prefix %q", string(body), want)
	}
}

func TestHelloFallsBackToWorld(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rr := httptest.NewRecorder()
	New(":0").Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if want := "Hello, World!\n"; string(body[:len(want)]) != want {
		t.Fatalf("body = %q, want prefix %q", string(body), want)
	}
}

func TestHealthReturnsJSON(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	New(":0").Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var payload map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status = %q, want ok", payload["status"])
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rr := httptest.NewRecorder()
	New(":0").Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHelloMethodNotAllowed(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/hello", nil)
	rr := httptest.NewRecorder()
	New(":0").Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestServerAddr(t *testing.T) {
	t.Parallel()

	s := New(":8080")
	if s.Addr() != ":8080" {
		t.Fatalf("Addr() = %q, want :8080", s.Addr())
	}
}
```

`TestHelloMethodNotAllowed` proves that the `"GET /hello"` pattern rejects
non-GET methods: `ServeMux` returns 405 automatically when specific methods are
registered and no broader pattern intercepts the request. `TestUnknownPathReturns404`
proves that a path with no matching pattern falls through to the mux's built-in
404.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"example.com/httpserver/internal/server"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	s := server.New(addr)
	srv := &http.Server{
		Addr:              s.Addr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Printf("listening on %s\n", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
```

Run it:

```bash
go run ./cmd/demo
```

In another terminal:

```bash
curl -s "http://localhost:8080/hello?name=Gopher"
curl -s "http://localhost:8080/health"
curl -i "http://localhost:8080/nonexistent"
```

### Example Function (auto-verified)

Add to `server_test.go`:

```go
func ExampleNew() {
	s := New(":9999")
	_ = s.Handler()
	fmt.Println(s.Addr())
	// Output: :9999
}
```

## Common Mistakes

### Calling `WriteHeader` After `Write`

Wrong:

```go
fmt.Fprintf(w, "Hello")
w.WriteHeader(http.StatusCreated) // too late
```

What happens: the status line is flushed when the first `Write` happens, so
the implicit 200 has already been sent. `WriteHeader` after that logs a
warning to `Server.ErrorLog` and is otherwise a no-op.

Fix: set headers and call `WriteHeader` before writing the body, or use
`http.Error` which writes the status and body in the correct order.

### Forgetting `return` After `http.Error`

Wrong:

```go
if err != nil {
    http.Error(w, "bad request", http.StatusBadRequest)
    // falls through and writes more bytes
}
```

What happens: the handler keeps running and the second write corrupts the
response (headers have already been flushed by `http.Error`).

Fix: add `return` immediately after `http.Error`.

### Registering On The Default `ServeMux`

Wrong: `http.HandleFunc("/hello", h)` inside a package other than `main`.

What happens: the default mux is a global. Any imported package can register
on it, which makes routing implicit and impossible to test in isolation. A test
that imports the package leaks routes into the default mux.

Fix: create a local `http.NewServeMux()` and pass it to `http.Server` (or to
`httptest.NewServer`). The lesson's `Server.Handler()` returns a fresh mux.

## Verification

From `~/go-exercises/httpserver`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification; the `ExampleNew` function
runs as part of the suite.

Your turn: add a `TestHelloRejectsMissingName` that calls `r.URL.Query()` for
`name=` (empty value) and asserts the response still starts with `"Hello,
World!"`. The test pins the "missing query falls back to default" contract.

## Summary

- An `http.Handler` is any value with a `ServeHTTP(w, r)` method; `http.HandlerFunc`
  adapts a plain function.
- The handler writes the response body to `http.ResponseWriter`; headers must
  be set before the first `Write`.
- The handler reads the request via `*http.Request` (`r.Method`, `r.URL`,
  `r.Header`, `r.Body`, `r.Context()`).
- `http.NewServeMux()` creates an isolated mux; avoid the global `DefaultServeMux`
  in anything but a one-file demo.
- `httptest.NewRecorder` and `httptest.NewServer` let you exercise handlers
  without binding a TCP port.

## What's Next

Next: [HTTP Client](../02-http-client/02-http-client.md).

## Resources

- [net/http package](https://pkg.go.dev/net/http)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)
- [Writing Web Applications](https://go.dev/doc/articles/wiki/)
- [Go Blog: Go 1.22 enhanced routing patterns](https://go.dev/blog/routing-enhancements)