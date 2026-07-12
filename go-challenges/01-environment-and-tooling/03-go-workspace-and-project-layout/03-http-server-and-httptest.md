# Exercise 3: A Thin HTTP Server Binary Tested With httptest

An HTTP server binary follows the same thin-`main` discipline as a CLI, with one
extra layout decision: the handler lives at package scope in an importable
package, precisely so it can be tested without binding a socket. This exercise
builds `cmd/server` — a thin `main` assembling an `*http.Server` with a
`ReadHeaderTimeout` and a `ServeMux` — plus an `internal/api` handler that calls
the library and maps the empty-name sentinel to HTTP 400, driven in tests through
`httptest.NewRecorder` with no listener.

This module is fully self-contained: its own `go mod init`, the greeting library
inline, its own demo and tests.

## What you'll build

```text
myapp/                         module github.com/example/myapp
  go.mod                       go 1.24
  internal/
    greeting/greeting.go       ErrEmptyName, Greet
    api/
      api.go                   Handler, NewMux
      api_test.go              table-driven over httptest.NewRecorder
  cmd/
    server/main.go             thin main: *http.Server with ReadHeaderTimeout
    demo/main.go               runnable: httptest.NewServer + a real GET
```

- Files: `internal/greeting/greeting.go`, `internal/api/api.go`, `internal/api/api_test.go`, `cmd/server/main.go`, `cmd/demo/main.go`.
- Implement: `api.Handler(w, r)` greeting the `name` query param (default `"World"`), mapping `ErrEmptyName` to `http.StatusBadRequest`; `api.NewMux()`; a thin `main` building `*http.Server{ReadHeaderTimeout: ...}` that tolerates `http.ErrServerClosed`.
- Test: table-driven `TestHandler` over {default name, explicit name, whitespace rejected} using `httptest.NewRecorder`, asserting `rec.Code` and `rec.Body.String()` with no socket bound.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The handler is at package scope on purpose

The single most consequential layout choice for a testable server is putting the
handler at *package scope* in an importable package, not as an inline closure
inside `main()`. An inline `mux.HandleFunc("/", func(w, r){...})` can only be
exercised by starting a real server and making real requests. A package-level
`func Handler(w http.ResponseWriter, r *http.Request)` can be handed an
`httptest.NewRecorder` and an `httptest.NewRequest` directly — no port, no
listener, no flakiness. The handler lives in `internal/api` so both `cmd/server`
(the real binary) and `cmd/demo` can import it; a handler stuck in `package main`
could be reached by neither the demo nor a same-binary test that wants to reuse
it elsewhere.

The handler maps errors to status codes deliberately. It reads the `name` query
parameter, defaults an empty parameter to `"World"`, and calls `greeting.Greet`.
A whitespace-only name still reaches `Greet` and comes back as `ErrEmptyName`,
which the handler catches with `errors.Is` and turns into a `400 Bad Request` via
`http.Error`. Any other error becomes a `500`. This is the HTTP analogue of the
CLI's error mapping: an internal sentinel translated into the protocol's own
vocabulary at the boundary.

`main()` stays thin: it builds a `ServeMux` from `api.NewMux()`, wraps it in an
`*http.Server` with an explicit `ReadHeaderTimeout` (leaving it unset is a slowloris
foot-gun, which is why a production server always sets it), and calls
`ListenAndServe`. It treats `http.ErrServerClosed` as a normal shutdown, not an
error to `log.Fatal` on.

Create `internal/greeting/greeting.go`:

```go
package greeting

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greet when the name is empty after trimming.
var ErrEmptyName = errors.New("name must not be empty")

// Greet formats a greeting for name, rejecting empty input with ErrEmptyName.
func Greet(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("[%s %s] %s says hello", "myapp", "0.1.0", trimmed), nil
}
```

Create `internal/api/api.go`:

```go
package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/example/myapp/internal/greeting"
)

// Handler greets the "name" query parameter, defaulting an absent name to
// "World". A whitespace-only name is rejected as 400 Bad Request; any other
// failure is 500. It is a package-level function so tests can drive it with an
// httptest.NewRecorder, with no network listener.
func Handler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "World"
	}
	g, err := greeting.Greet(name)
	if err != nil {
		if errors.Is(err, greeting.ErrEmptyName) {
			http.Error(w, "name parameter is required", http.StatusBadRequest)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	fmt.Fprintln(w, g)
}

// NewMux returns a ServeMux with the greeting handler mounted at "/".
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", Handler)
	return mux
}
```

Create `cmd/server/main.go`:

```go
package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/example/myapp/internal/api"
)

func main() {
	addr := ":8080"
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
```

### The runnable demo

Rather than bind a real port (which would block and not produce clean output),
the demo starts an in-process `httptest.NewServer` over the same mux, makes one
real HTTP GET against it, and prints the status and body. This exercises the full
request path — routing, handler, response — deterministically.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/example/myapp/internal/api"
)

func main() {
	srv := httptest.NewServer(api.NewMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/?name=Gopher")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("status: %d\n", resp.StatusCode)
	fmt.Printf("body: %s", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
body: [myapp 0.1.0] Gopher says hello
```

### Tests

The test drives `Handler` directly with a recorder and a synthetic request, so no
socket is ever bound. Each row asserts the status code and the exact response
body, including the trailing newline that `http.Error` and `fmt.Fprintln` add.

Create `internal/api/api_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantBody   string
	}{
		{name: "default name", query: "", wantStatus: http.StatusOK, wantBody: "[myapp 0.1.0] World says hello\n"},
		{name: "explicit name", query: "name=Go", wantStatus: http.StatusOK, wantBody: "[myapp 0.1.0] Go says hello\n"},
		{name: "whitespace rejected", query: "name=%20%20", wantStatus: http.StatusBadRequest, wantBody: "name parameter is required\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			rec := httptest.NewRecorder()

			Handler(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}
```

## Review

The server is correct when the handler is a pure function of the request: an
absent `name` yields `200` and the World greeting, an explicit name yields `200`
and its greeting, and a whitespace-only name yields `400` with the fixed message
— each with the exact trailing newline the writers emit. The test proves this
through a recorder with no listener, which is only possible because `Handler` is
a package-level function rather than an inline closure. Watch two things: keep
the handler out of `package main` so it stays importable and testable, and always
set `ReadHeaderTimeout` on a production `*http.Server` (an unbounded header read
is a denial-of-service vector). Run `go test -race ./...` to confirm the full
module builds and the handler behaves.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder`, `NewRequest`, and `NewServer` for handler tests without a real port.
- [`net/http.Server`](https://pkg.go.dev/net/http#Server) — the `ReadHeaderTimeout` field and `ErrServerClosed` shutdown sentinel.
- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — routing with `NewServeMux` and `HandleFunc`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-package-patterns-and-go-list.md](04-package-patterns-and-go-list.md)
