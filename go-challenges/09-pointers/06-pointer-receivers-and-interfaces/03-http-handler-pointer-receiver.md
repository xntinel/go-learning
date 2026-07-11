# Exercise 3: Stateful HTTP Handler: Why the Handler Is a *T on the Method Set

An HTTP handler that carries dependencies — a store, a logger — is the everyday
place where you register `*T` as an `http.Handler`. This module builds an
`*APIHandler` whose `ServeHTTP` has a pointer receiver, registers it on an
`http.ServeMux`, and drives it with `httptest`, showing why the pointer receiver
is the idiomatic way to inject shared state into a handler.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
apihandler/                 independent module: example.com/apihandler
  go.mod                    go 1.25
  handler.go                type APIHandler (deps + *slog.Logger); ServeHTTP (pointer recv)
  cmd/
    demo/
      main.go               wire the handler onto a ServeMux, hit it in-process
  handler_test.go           httptest drive; http.Handler contract; status/body asserts
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: an `APIHandler` holding a `Store` interface and a `*slog.Logger`, with `ServeHTTP` on a pointer receiver, registered via `mux.Handle`.
- Test: `var _ http.Handler = (*APIHandler)(nil)`; drive `ServeHTTP` with `httptest.NewRequest` + `httptest.NewRecorder` and assert status codes and JSON body for hit, miss, and wrong-method.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/apihandler/cmd/demo
cd ~/go-exercises/apihandler
go mod init example.com/apihandler
go mod edit -go=1.25
```

### Why the handler is a pointer

`APIHandler` holds injected dependencies: a `Store` (a port interface it depends
on) and a `*slog.Logger`. It is constructed once at wiring time and registered on
a mux; the same handler value then serves every request. `ServeHTTP` has a pointer
receiver for two reasons. First, `http.ServeMux` and the server hold the handler
as an `http.Handler` interface value; registering `&APIHandler{...}` stores the
pointer, so all requests share one handler and its one set of dependencies — no
per-request copy of the store or logger. Second, if the handler ever needs to
mutate shared state (a hit counter, a cache), a pointer receiver is required for
the mutation to persist; keeping `ServeHTTP` a pointer receiver from the start
means the method set is uniform and future stateful methods fit without breaking
the `http.Handler` satisfaction.

The method-set rule bites here in a concrete way: `http.Handler` requires
`ServeHTTP(http.ResponseWriter, *http.Request)`. Because `ServeHTTP` has a pointer
receiver, only `*APIHandler` satisfies `http.Handler` — a bare `APIHandler{}`
value does not, and `mux.Handle("/x", APIHandler{...})` would not compile. The
contract `var _ http.Handler = (*APIHandler)(nil)` documents this at the
definition site.

Create `handler.go`:

```go
// handler.go
package apihandler

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Store is the port the handler depends on. Callers wire in any implementation;
// the handler depends on the interface, not a concrete type.
type Store interface {
	Get(id string) (string, bool)
}

// APIHandler is a stateful HTTP handler holding injected dependencies. It is
// constructed once and registered on a mux; ServeHTTP has a pointer receiver so
// the mux stores *APIHandler and every request shares the same deps.
type APIHandler struct {
	store Store
	log   *slog.Logger
}

// Compile-time contract: *APIHandler is an http.Handler. A value APIHandler{}
// is NOT, because ServeHTTP has a pointer receiver:
//
//	var _ http.Handler = APIHandler{} // would not compile
var _ http.Handler = (*APIHandler)(nil)

// NewAPIHandler wires the handler with its dependencies.
func NewAPIHandler(store Store, log *slog.Logger) *APIHandler {
	return &APIHandler{store: store, log: log}
}

// ServeHTTP handles GET /users/{id}, returning the user JSON or 404.
func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	name, ok := h.store.Get(id)
	if !ok {
		h.log.Info("user not found", "id", id)
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "name": name})
}
```

Note the handler uses `r.PathValue("id")`, the standard-library path-parameter
API (Go 1.22+), populated when the route pattern includes `{id}`.

### The runnable demo

The demo defines a tiny in-memory store, wires the handler onto a `ServeMux` with
a `{id}` wildcard route, and issues two in-process requests so the output is
deterministic and needs no external port.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/apihandler"
)

type mapStore map[string]string

func (m mapStore) Get(id string) (string, bool) { v, ok := m[id]; return v, ok }

func main() {
	h := apihandler.NewAPIHandler(
		mapStore{"1": "alice"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", h)

	for _, id := range []string{"1", "2"} {
		req := httptest.NewRequest(http.MethodGet, "/users/"+id, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		fmt.Printf("GET /users/%s -> %d %s", id, rec.Code, rec.Body.String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /users/1 -> 200 {"id":"1","name":"alice"}
GET /users/2 -> 404 {"error":"not found"}
```

### Tests

The tests drive the handler through `httptest` with no network. A stub store lets
each case control the hit/miss outcome. The wrong-method case asserts the 405
branch. `TestHandlerContract` records the compile-time satisfaction explicitly.

Create `handler_test.go`:

```go
// handler_test.go
package apihandler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubStore map[string]string

func (s stubStore) Get(id string) (string, bool) { v, ok := s[id]; return v, ok }

func newTestHandler(s Store) *APIHandler {
	return NewAPIHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// serve wires the handler on a mux (so PathValue is populated) and returns the
// recorder for one request.
func serve(h *APIHandler, method, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("/users/{id}", h)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHandlerContract(t *testing.T) {
	t.Parallel()
	var _ http.Handler = (*APIHandler)(nil)
}

func TestServeHTTP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		method     string
		target     string
		store      Store
		wantStatus int
		wantBody   string
	}{
		{"hit", http.MethodGet, "/users/1", stubStore{"1": "alice"}, http.StatusOK, `"name":"alice"`},
		{"miss", http.MethodGet, "/users/9", stubStore{}, http.StatusNotFound, `"error":"not found"`},
		{"wrong method", http.MethodPost, "/users/1", stubStore{"1": "alice"}, http.StatusMethodNotAllowed, `"method not allowed"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := serve(newTestHandler(tc.store), tc.method, tc.target)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want to contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestContentTypeOnHit(t *testing.T) {
	t.Parallel()

	rec := serve(newTestHandler(stubStore{"1": "alice"}), http.MethodGet, "/users/1")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}
```

## Review

The handler is correct when it depends only on the `Store` interface and the
mux stores it as `*APIHandler`: the `var _ http.Handler = (*APIHandler)(nil)`
contract is what guarantees the registration compiles, and it would break loudly
if `ServeHTTP` were given a value receiver on a type any of whose methods needed a
pointer. Driving it through `httptest.NewRecorder` and `httptest.NewRequest` keeps
the test hermetic — no ports, no goroutines, no flakes — and asserting the 200,
404, and 405 branches covers the whole method. The lesson to carry: a stateful
handler is a `*T`, injected once, shared across requests; never store it or pass
it by value, or each request pays to copy its dependencies.

## Resources

- [net/http.Handler](https://pkg.go.dev/net/http#Handler) — the one-method interface and `ServeMux.Handle`.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for in-process handler tests.
- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — method+wildcard patterns and `Request.PathValue`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-read-only-value-receiver-interface.md](02-read-only-value-receiver-interface.md) | Next: [04-json-unmarshaler-pointer-receiver.md](04-json-unmarshaler-pointer-receiver.md)
