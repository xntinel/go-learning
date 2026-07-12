# Exercise 2: Wire service-a — an HTTP API that consumes the shared library

A deployable service proves the monorepo works: it lives under `cmd/`, imports
the shared platform package by its in-module path, and returns the shared error
envelope so its responses match every other service. This exercise wires
`cmd/api` as a real `http.Handler` around a tiny in-memory user store, emitting
`platform/httpx` errors on the failure paths.

## What you'll build

```text
mono/                         single module: example.com/mono
  go.mod
  platform/
    httpx/
      httpx.go                shared envelope + APIError (bundled here)
  cmd/
    api/
      handler.go              UserHandler.ServeHTTP: 200 on hit, httpx error on miss
      handler_test.go         httptest-driven status + envelope assertions
      main.go                 wires the handler onto a ServeMux (demo entry)
```

- Files: `platform/httpx/httpx.go`, `cmd/api/handler.go`, `cmd/api/handler_test.go`, `cmd/api/main.go`.
- Implement: a `UserHandler` with an in-memory map, whose `ServeHTTP` returns a JSON user on a hit and calls `httpx.WriteError` with `ErrNotFound`/a bad-request error on the failure paths.
- Test: build a request with `httptest.NewRequest`, record with `httptest.NewRecorder`, assert the status code and that the body decodes to the shared `httpx.Envelope`.
- Verify: `go test -count=1 -race ./...`

Set up the module. Each exercise is self-contained, so it bundles its own copy of
the shared package under the same import path it would have in the real repo:

```bash
mkdir -p go-solutions/11-packages-and-modules/10-monorepo-module-strategy/02-service-a-http-api/platform/httpx go-solutions/11-packages-and-modules/10-monorepo-module-strategy/02-service-a-http-api/cmd/api
cd go-solutions/11-packages-and-modules/10-monorepo-module-strategy/02-service-a-http-api
```

### Why the handler is the unit, not main

The thing worth testing in a service is the `http.Handler`, not `main`. `main`
binds a port, reads flags, and blocks on `ListenAndServe` — none of which you want
in a unit test. So the design splits them: `handler.go` holds a `UserHandler`
whose `ServeHTTP` is a pure function of request → response, and `main.go` only
mounts it on a `ServeMux` and serves. The test drives `ServeHTTP` directly through
`httptest` with no socket, no port, no `os.Args`.

The handler consumes the shared library exactly as a real service would: it
imports `example.com/mono/platform/httpx` and funnels every error through
`httpx.WriteError`. A missing user returns the shared `ErrNotFound`; a malformed
request returns an ad-hoc `httpx.NewError(400, ...)`. The payoff is uniformity —
this service's 404 body is byte-for-byte the envelope from Exercise 1, so a
client that understands one service understands them all. That is the concrete,
testable expression of "shared implementation, one version".

Note the import path: `example.com/mono/platform/httpx`. Within the single
module, a service under `cmd/api` reaches shared code by the module path plus the
package's directory. No `replace`, no workspace — the single-module topology
makes cross-package imports trivially correct.

Create `platform/httpx/httpx.go` (the shared library, bundled so this module
stands alone):

```go
package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Envelope is the JSON error body every service in the monorepo returns.
type Envelope struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError is a structured error carrying an HTTP status and a stable code.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Message, e.Status)
}

// ErrNotFound is the shared sentinel for a missing resource.
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
```

Create `cmd/api/handler.go`:

```go
package main

import (
	"encoding/json"
	"net/http"

	"example.com/mono/platform/httpx"
)

// User is the resource this API serves.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// UserHandler serves users from an in-memory store. In a real service the map is
// a repository; the handler shape and the shared error contract are identical.
type UserHandler struct {
	users map[string]User
}

// NewUserHandler seeds the store.
func NewUserHandler(users map[string]User) *UserHandler {
	return &UserHandler{users: users}
}

func (h *UserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		_ = httpx.WriteError(w, httpx.NewError(http.StatusBadRequest, "bad_request", "missing user id"))
		return
	}

	u, ok := h.users[id]
	if !ok {
		_ = httpx.WriteError(w, httpx.ErrNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(u)
}
```

### The service entry point

`main.go` is the deployable's entry point. It builds the handler, mounts it on a
`ServeMux` with a wildcard path so `PathValue("id")` works, and serves. It is
never exercised by the test; it exists so `go run ./cmd/api` and the eventual
container build have a `package main` to compile.

Create `cmd/api/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
)

func main() {
	h := NewUserHandler(map[string]User{
		"1": {ID: "1", Name: "alice"},
	})

	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", h)

	// Exercise both paths against the mux in-process so the demo is runnable
	// without binding a port.
	for _, path := range []string{"/users/1", "/users/999"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("GET %s -> %d %s\n", path, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
```

Run it:

```bash
go run ./cmd/api
```

Expected output:

```
GET /users/1 -> 200 {"id":"1","name":"alice"}
GET /users/999 -> 404 {"status":404,"code":"not_found","message":"resource not found"}
```

### Tests

The test drives `ServeHTTP` through the mux (so `PathValue` is populated) with
`httptest` and asserts two things per case: the status code, and — on the failure
paths — that the body decodes to the shared `httpx.Envelope` with the expected
code. That second assertion is the important one: it proves the service really
speaks the shared error contract, not a look-alike shape.

Create `cmd/api/handler_test.go`:

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/mono/platform/httpx"
)

func newTestMux() *http.ServeMux {
	h := NewUserHandler(map[string]User{"1": {ID: "1", Name: "alice"}})
	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", h)
	return mux
}

func TestUserHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantCode   string // "" means expect a User body, not an error envelope
	}{
		{"hit", "/users/1", http.StatusOK, ""},
		{"miss", "/users/999", http.StatusNotFound, "not_found"},
	}

	mux := newTestMux()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if tc.wantCode == "" {
				var u User
				if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
					t.Fatalf("body is not a User: %v", err)
				}
				if u.Name != "alice" {
					t.Errorf("user name = %q, want alice", u.Name)
				}
				return
			}

			var env httpx.Envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not a shared Envelope: %v", err)
			}
			if env.Code != tc.wantCode || env.Status != tc.wantStatus {
				t.Errorf("envelope = %+v, want code %q status %d", env, tc.wantCode, tc.wantStatus)
			}
		})
	}
}
```

## Review

The service is correct when the handler is a pure request → response function and
`main` is a thin binder: the test never touches `main`, yet it exercises every
branch of `ServeHTTP` through the mux. The load-bearing assertion is that the
error body decodes into `httpx.Envelope` with the right `Code` — that is what
proves this service consumes the shared contract rather than hand-rolling a
similar-looking one.

The trap to avoid is testing through a real listener. `httptest.NewRequest` plus a
`ResponseRecorder` (or the mux's `ServeHTTP`) gives you the full routing and
handler behavior with no port, no goroutine, and no flakiness. Reserve a bound
socket for an explicit integration test, not the default unit path. Run
`go test -race` to keep the handler honest as it grows shared state.

## Resources

- [`net/http` `ServeMux` and method patterns](https://pkg.go.dev/net/http#ServeMux) — the `GET /users/{id}` routing and `Request.PathValue`.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for handler tests without a network.
- [Writing Web Applications](https://go.dev/doc/articles/wiki/) — the handler-plus-mux shape this service follows.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-service-b-worker.md](03-service-b-worker.md)
