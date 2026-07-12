# Exercise 3: An HTTP Handler That Depends on One Method, Not the Whole Service

A `GET /users/{id}` handler needs exactly one capability: fetch a user by id. Yet
it is routinely wired to depend on a `*UserService` that also does auth, email,
billing, and caching — so the handler transitively pulls all of that into its
blast radius and its tests. This module refactors the handler to depend on a
locally declared `UserFinder` interface with a single method, which the real
service satisfies implicitly.

## What you'll build

```text
userhandler/                   independent module: example.com/userhandler
  go.mod                       go 1.24
  handler.go                   UserFinder (one method); UserHandler serves GET /users/{id}
  service.go                   fat UserService satisfies UserFinder implicitly
  cmd/
    demo/
      main.go                  wires the real service into the handler, serves one request
  handler_test.go              httptest drives 200 and 404 with a one-method fake
```

Files: `handler.go`, `service.go`, `cmd/demo/main.go`, `handler_test.go`.
Implement: `UserFinder interface { FindByID(ctx, id) (User, error) }`, a `UserHandler` that depends only on it, and a fat `UserService` that also implements it.
Test: `httptest.NewRequest`/`NewRecorder` against a two-line `fakeFinder` returning a canned user (assert 200 body) and one returning not-found (assert 404).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/06-interface-segregation/03-consumer-defined-handler-dep/cmd/demo
cd go-solutions/08-interfaces/06-interface-segregation/03-consumer-defined-handler-dep
go mod edit -go=1.24
```

### The narrow port lives in the handler's package

The refactor moves the interface to where it is consumed. `UserFinder` is
declared in the handler's file, not in the service's. It names exactly the one
method the handler calls, `FindByID`. The real `UserService` — which also has
`SendWelcomeEmail`, `ChargeSubscription`, `InvalidateCache`, and more — satisfies
`UserFinder` structurally, with no `implements` and no adapter. The handler's
dependency is now one method wide.

Two things fall out of this. First, blast radius: a change to the billing method
signature no longer touches the handler or forces its test to recompile, because
the handler's type does not reference billing at all. Second, testability: the
handler's test needs a fake with one method, not a stand-in for the entire
service. The `fakeFinder` in the test is two lines; a fake for `*UserService`
would be dozens of panic-stubs.

The handler decodes the id from the path with `r.PathValue` (Go 1.22+ routing),
calls `FindByID`, and maps a `ErrUserNotFound` to HTTP 404 via `errors.Is`, any
other error to 500, and success to a JSON body. `context` flows from the request
so a client disconnect cancels the lookup.

Create `handler.go`:

```go
package userhandler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ErrUserNotFound is the sentinel a finder returns when no user matches.
var ErrUserNotFound = errors.New("user not found")

// User is the response payload.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// UserFinder is the ONLY capability the handler needs. Declared here, at the
// consumer, sized to the one method the handler calls.
type UserFinder interface {
	FindByID(ctx context.Context, id string) (User, error)
}

// UserHandler serves GET /users/{id}. It depends on UserFinder, not on any
// concrete service, so it never transitively depends on email/billing/cache.
type UserHandler struct {
	finder UserFinder
}

// NewUserHandler wires a finder into the handler.
func NewUserHandler(f UserFinder) *UserHandler {
	return &UserHandler{finder: f}
}

func (h *UserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	u, err := h.finder.FindByID(r.Context(), id)
	if errors.Is(err, ErrUserNotFound) {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(u)
}
```

Create `service.go`. The fat service satisfies `UserFinder` among many other
methods:

```go
package userhandler

import "context"

// UserService is the real service: it does far more than lookups. The handler
// depends on none of this beyond FindByID.
type UserService struct {
	users map[string]User
}

// NewUserService builds a service seeded with users.
func NewUserService(seed map[string]User) *UserService {
	return &UserService{users: seed}
}

// FindByID is the one method the handler needs; UserService satisfies
// UserFinder implicitly by having it.
func (s *UserService) FindByID(_ context.Context, id string) (User, error) {
	u, ok := s.users[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return u, nil
}

// SendWelcomeEmail, ChargeSubscription, and InvalidateCache stand in for the
// rest of the fat service surface the handler must NOT depend on.
func (s *UserService) SendWelcomeEmail(_ context.Context, id string) error {
	_ = id
	return nil
}

func (s *UserService) ChargeSubscription(_ context.Context, id string, cents int64) error {
	_, _ = id, cents
	return nil
}

func (s *UserService) InvalidateCache(id string) {
	_ = id
}

// Compile-time proof the fat service satisfies the narrow port.
var _ UserFinder = (*UserService)(nil)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/userhandler"
)

func main() {
	svc := userhandler.NewUserService(map[string]userhandler.User{
		"u1": {ID: "u1", Name: "Alice", Email: "alice@example.com"},
	})

	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", userhandler.NewUserHandler(svc))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/users/u1")
	fmt.Printf("status: %d\n", resp.StatusCode)
	resp.Body.Close()

	miss, _ := http.Get(srv.URL + "/users/nope")
	fmt.Printf("status: %d\n", miss.StatusCode)
	miss.Body.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
status: 404
```

### Tests

The test drives the handler with `httptest.NewRequest`/`NewRecorder` and a fake
that has exactly one method — the whole point. Because Go 1.22 path values come
from the `ServeMux`, the test routes through a `mux` so `r.PathValue("id")` is
populated.

Create `handler_test.go`:

```go
package userhandler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeFinder is the entire test double: one method.
type fakeFinder struct {
	user User
	err  error
}

func (f fakeFinder) FindByID(_ context.Context, _ string) (User, error) {
	return f.user, f.err
}

// compile-time proof the two-line fake satisfies the narrow port.
var _ UserFinder = fakeFinder{}

func newMux(h *UserHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", h)
	return mux
}

func TestHandlerReturnsUser(t *testing.T) {
	t.Parallel()

	h := NewUserHandler(fakeFinder{user: User{ID: "u1", Name: "Alice", Email: "a@x.io"}})
	req := httptest.NewRequest(http.MethodGet, "/users/u1", nil)
	rec := httptest.NewRecorder()

	newMux(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got User
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Name != "Alice" {
		t.Fatalf("Name = %q, want Alice", got.Name)
	}
}

func TestHandlerReturns404OnNotFound(t *testing.T) {
	t.Parallel()

	h := NewUserHandler(fakeFinder{err: ErrUserNotFound})
	req := httptest.NewRequest(http.MethodGet, "/users/ghost", nil)
	rec := httptest.NewRecorder()

	newMux(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerServiceSatisfiesFinder(t *testing.T) {
	t.Parallel()

	// The fat service is usable as the narrow port with no adapter.
	svc := NewUserService(map[string]User{"u1": {ID: "u1", Name: "Bob"}})
	var f UserFinder = svc
	u, err := f.FindByID(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "Bob" {
		t.Fatalf("Name = %q, want Bob", u.Name)
	}
}
```

## Review

The handler is correct when its only dependency is `UserFinder`: the struct field
is typed as the interface, the fat `*UserService` flows in with no adapter, and
the test double is one method. The mistake to avoid is typing the field as
`*UserService` "because that's what we pass in production" — that couples the
handler to email and billing, forces the test to construct or mock the whole
service, and turns any change to an unrelated service method into a handler
recompile. Keeping the port at the consumer, sized to the one call, is what keeps
the handler's blast radius one method wide. Run `go test -race` since the handler
serves concurrent requests over the shared finder.

## Resources

- [net/http package (ServeMux path patterns, PathValue)](https://pkg.go.dev/net/http#ServeMux)
- [net/http/httptest package](https://pkg.go.dev/net/http/httptest)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-split-fat-repository.md](02-split-fat-repository.md) | Next: [04-reader-writer-streaming-pipeline.md](04-reader-writer-streaming-pipeline.md)
