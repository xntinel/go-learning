# Exercise 4: A Layered HTTP Service Wired Through Consumer Interfaces

A real service has more than one seam. This exercise builds a small but complete
three-layer HTTP service — an `http.Handler` on top, a use-case `service` in the
middle, a `repository` at the bottom — where every layer depends on the one below
it only through an interface the upper layer itself declares, and a single
composition root in `main` is the one place that names concrete types. The whole
stack is exercised end-to-end with `net/http/httptest` and in-memory fakes, so
the tests need no real database and bind no real port.

This module is fully self-contained. It starts with its own `go mod init`, defines
every type it needs across a handful of packages, and ships its own demo and tests.
Nothing here imports any other exercise.

## What you'll build

```text
domain/
  user.go            User aggregate and the shared ErrNotFound sentinel
repository/
  memory.go          InMemoryUserRepo: a map-backed Save/FindByID (the fake DB)
service/
  service.go         UserRepository + IDGenerator interfaces (consumer-owned here),
                     UserService.Register / Get, sentinel errors
transport/
  handler.go         UserService interface (consumer-owned here), Handler with a
                     net/http ServeMux: POST /users, GET /users/{id}, JSON in/out
  endtoend_test.go   httptest server over the real stack: create-then-get, 404,
                     400, plus a failing in-memory repo driving the 500 path
cmd/
  server/
    main.go          composition root: wire repo -> service -> handler, exercise it
```

- Files: `domain/user.go`, `repository/memory.go`, `service/service.go`, `transport/handler.go`, `transport/endtoend_test.go`, `cmd/server/main.go`.
- Implement: `UserService.Register/Get` over a consumer-owned `UserRepository`, and a `Handler` over a consumer-owned `UserService`, wired once in `main`.
- Test: drive the whole stack through `httptest`, asserting 201/200/404/400 and a 500 from an injected failing repository.
- Verify: `go test -race ./...`

### Why each layer owns the interface it consumes

The single rule that makes this architecture hold together is that an interface
belongs to the code that *calls* it, not the code that *implements* it. The
`service` package is the one that calls `Save` and `FindByID`, so the
`UserRepository` interface is declared in `service`, listing exactly those two
methods and nothing else. The `transport` package is the one that calls `Register`
and `Get`, so the `UserService` interface is declared in `transport`. The concrete
`repository.InMemoryUserRepo` and `service.UserService` never announce that they
satisfy anything — they just have the right methods, and Go's implicit interface
satisfaction does the wiring at the `main` boundary. The consequence is that the
dependency arrows all point *inward and downward through interfaces the upper layer
defines*: `transport` imports `domain` for the data type but depends on its own
`UserService` interface for behavior; `service` imports `domain` but depends on its
own `UserRepository` interface; only `main` imports the concrete `repository`. Swap
the map-backed repo for a Postgres one and not a single line of `service` or
`transport` changes, because neither of them ever named the concrete type.

This is also what makes the stack testable without infrastructure. Because
`transport.Handler` accepts a `UserService` interface, a test can hand it the real
`service.UserService` wired over the real in-memory repository and exercise the
genuine request path; and because `service.UserService` accepts a `UserRepository`
interface, a *different* test can hand it a repository whose `Save` always fails and
watch the handler turn that into a 500. The `IDGenerator` seam exists for the same
reason: identity is an injected dependency, so the demo gets a sequential generator
and the tests get a fixed one, and assertions on the returned ID are deterministic
rather than dependent on a clock or a random source.

The composition root is the load-bearing idea. Everywhere except `main` the code
deals in interfaces and never constructs a concrete collaborator; `main` is the
single function that knows `InMemoryUserRepo`, `UserService`, and `Handler` are the
chosen implementations, constructs them in dependency order — repo, then service
over the repo, then handler over the service — and hands the resulting `http.Handler`
to the server. Push that knowledge anywhere else and you have reintroduced the
coupling the interfaces were meant to remove.

Create `domain/user.go`:

```go
package domain

import "errors"

// User is the aggregate every layer agrees on. It is plain data with no behavior
// and no dependencies, so all three layers can import it without creating a cycle.
type User struct {
	ID    string
	Name  string
	Email string
}

// ErrNotFound is the shared sentinel a repository returns when a lookup misses.
// It lives in domain because both the repository (which raises it) and the
// service (which inspects it) need it, and domain depends on nothing.
var ErrNotFound = errors.New("domain: user not found")
```

The `service` package sits in the middle. It declares the two interfaces it
consumes, validates input, generates an ID through the injected generator, and
translates the repository's `domain.ErrNotFound` into its own `ErrNotFound` so the
transport layer matches a service-level sentinel rather than reaching down to a
domain detail. Every error that crosses out of a method is wrapped with `%w`, so the
handler can use `errors.Is` to classify it.

Create `service/service.go`:

```go
package service

import (
	"context"
	"errors"
	"fmt"

	"example.com/layered/domain"
)

// Sentinel errors the transport layer matches with errors.Is.
var (
	ErrEmptyName  = errors.New("service: name is required")
	ErrEmptyEmail = errors.New("service: email is required")
	ErrNotFound   = errors.New("service: user not found")
)

// UserRepository is the persistence seam, declared by its consumer (the service)
// with only the two methods UserService actually calls.
type UserRepository interface {
	Save(ctx context.Context, u *domain.User) error
	FindByID(ctx context.Context, id string) (*domain.User, error)
}

// IDGenerator is injected so identity is a dependency, not a hidden global: the
// demo gets a sequential generator and tests get a deterministic one.
type IDGenerator interface {
	NewID() string
}

// UserService is the use-case layer. It holds its collaborators behind unexported
// interface fields, so it can never name a concrete repository or ID source.
type UserService struct {
	repo UserRepository
	ids  IDGenerator
}

// NewUserService injects both dependencies and rejects nil, turning a wiring
// mistake into a located error instead of a later panic.
func NewUserService(repo UserRepository, ids IDGenerator) (*UserService, error) {
	if repo == nil {
		return nil, errors.New("service: repository is required")
	}
	if ids == nil {
		return nil, errors.New("service: id generator is required")
	}
	return &UserService{repo: repo, ids: ids}, nil
}

// Register validates, mints an ID, and persists the new user.
func (s *UserService) Register(ctx context.Context, name, email string) (*domain.User, error) {
	if name == "" {
		return nil, ErrEmptyName
	}
	if email == "" {
		return nil, ErrEmptyEmail
	}
	u := &domain.User{ID: s.ids.NewID(), Name: name, Email: email}
	if err := s.repo.Save(ctx, u); err != nil {
		return nil, fmt.Errorf("service: save user: %w", err)
	}
	return u, nil
}

// Get fetches a user, translating the repository's not-found sentinel into the
// service-level one so the transport layer never reaches down to a domain detail.
func (s *UserService) Get(ctx context.Context, id string) (*domain.User, error) {
	u, err := s.repo.FindByID(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("service: get user %s: %w", id, err)
	}
	return u, nil
}
```

The `repository` package is the bottom layer and the only concrete persistence in
the build. It is a map guarded by a mutex, and it stores copies rather than the
caller's pointer so a later mutation of the input cannot reach inside the store —
the same defensive copy a real database boundary gives you for free. It satisfies
`service.UserRepository` structurally and imports only `domain`.

Create `repository/memory.go`:

```go
package repository

import (
	"context"
	"sync"

	"example.com/layered/domain"
)

// InMemoryUserRepo is the fake database: a real, working implementation backed by
// a map. It implements service.UserRepository without importing service.
type InMemoryUserRepo struct {
	mu    sync.RWMutex
	users map[string]*domain.User
}

func NewInMemoryUserRepo() *InMemoryUserRepo {
	return &InMemoryUserRepo{users: make(map[string]*domain.User)}
}

func (r *InMemoryUserRepo) Save(_ context.Context, u *domain.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := *u // copy in, so a later caller mutation cannot reach the store
	r.users[u.ID] = &stored
	return nil
}

func (r *InMemoryUserRepo) FindByID(_ context.Context, id string) (*domain.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := *u // copy out, so a caller cannot mutate the store through the result
	return &out, nil
}
```

The `transport` package is the top layer. It declares the `UserService` interface
it depends on, builds a `net/http.ServeMux` using Go 1.22 method-and-wildcard
patterns (`POST /users`, `GET /users/{id}`), decodes and encodes JSON, and maps
service errors onto status codes: validation sentinels become 400, the not-found
sentinel becomes 404, anything else becomes a 500 with no internal detail leaked to
the client.

Create `transport/handler.go`:

```go
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"example.com/layered/domain"
	"example.com/layered/service"
)

// UserService is the seam the handler depends on, declared here in the consumer.
// service.UserService satisfies it structurally; the handler never names it.
type UserService interface {
	Register(ctx context.Context, name, email string) (*domain.User, error)
	Get(ctx context.Context, id string) (*domain.User, error)
}

// Handler adapts HTTP requests to the use-case layer behind the UserService seam.
type Handler struct {
	svc UserService
}

func NewHandler(svc UserService) (*Handler, error) {
	if svc == nil {
		return nil, errors.New("transport: service is required")
	}
	return &Handler{svc: svc}, nil
}

// Routes builds the mux. Method-and-wildcard patterns are Go 1.22+.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", h.create)
	mux.HandleFunc("GET /users/{id}", h.get)
	return mux
}

type createRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type userResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	u, err := h.svc.Register(r.Context(), req.Name, req.Email)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrEmptyName), errors.Is(err, service.ErrEmptyEmail):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "could not create user")
		}
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(u))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, "user not found")
		default:
			writeError(w, http.StatusInternalServerError, "could not fetch user")
		}
		return
	}
	writeJSON(w, http.StatusOK, toResponse(u))
}

func toResponse(u *domain.User) userResponse {
	return userResponse{ID: u.ID, Name: u.Name, Email: u.Email}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
```

### The runnable demo

The demo is the composition root. It names the three concrete types, wires them in
dependency order, then exercises the assembled stack in-process with an
`httptest.Server` so it runs without binding a real port or needing a database. The
sequential ID generator lives here in `main` because choosing an identity strategy
is exactly the kind of decision the composition root exists to make.

Create `cmd/server/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"example.com/layered/repository"
	"example.com/layered/service"
	"example.com/layered/transport"
)

// seqIDs is the demo's concrete IDGenerator: a monotonic counter.
type seqIDs struct{ n atomic.Int64 }

func (s *seqIDs) NewID() string { return fmt.Sprintf("usr-%03d", s.n.Add(1)) }

type user struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func main() {
	// Composition root: the only place that names concrete types and wires them.
	repo := repository.NewInMemoryUserRepo()
	svc, err := service.NewUserService(repo, &seqIDs{})
	if err != nil {
		log.Fatalf("service: %v", err)
	}
	h, err := transport.NewHandler(svc)
	if err != nil {
		log.Fatalf("handler: %v", err)
	}

	// Exercise the wired stack in-process; no real port, no real database.
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	created := mustPostUser(srv.URL, `{"name":"Ada Lovelace","email":"ada@example.com"}`)
	fmt.Printf("created user id=%s name=%s\n", created.ID, created.Name)

	fetched := mustGetUser(srv.URL, created.ID)
	fmt.Printf("fetched user id=%s email=%s\n", fetched.ID, fetched.Email)

	fmt.Printf("GET missing user -> %d\n", getStatus(srv.URL+"/users/usr-999"))
}

func mustPostUser(base, body string) user {
	resp, err := http.Post(base+"/users", "application/json", strings.NewReader(body))
	if err != nil {
		log.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var u user
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		log.Fatalf("decode: %v", err)
	}
	return u
}

func mustGetUser(base, id string) user {
	resp, err := http.Get(base + "/users/" + id)
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var u user
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		log.Fatalf("decode: %v", err)
	}
	return u
}

func getStatus(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
```

Run it:

```bash
go run ./cmd/server
```

Expected output:

```
created user id=usr-001 name=Ada Lovelace
fetched user id=usr-001 email=ada@example.com
GET missing user -> 404
```

### Tests

The end-to-end test stands up an `httptest.Server` over the *real* stack — real
repository, real service, real handler — and drives it with an HTTP client, which is
the closest a test can get to production without infrastructure. `TestEndToEnd_CreateThenGet`
posts a user, asserts 201, then fetches it back and asserts 200 with the same
identity. The 404 and 400 cases pin the error classification. The load-bearing test
is `TestEndToEnd_RepositoryFailureIs500`: it injects an in-memory repository whose
`Save` always fails into the otherwise-real stack and asserts the handler reports
500 without leaking the internal error — proof that the seam between service and
repository is genuinely substitutable and that failures map to the right status.

Create `transport/endtoend_test.go`:

```go
package transport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/layered/domain"
	"example.com/layered/repository"
	"example.com/layered/service"
	"example.com/layered/transport"
)

// fixedIDs is a deterministic IDGenerator for tests.
type fixedIDs struct{ id string }

func (f fixedIDs) NewID() string { return f.id }

// failingRepo is an in-memory fake whose Save always fails, to drive the 500 path.
type failingRepo struct{}

func (failingRepo) Save(context.Context, *domain.User) error { return errors.New("disk full") }
func (failingRepo) FindByID(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}

func newServer(t *testing.T, repo service.UserRepository, ids service.IDGenerator) *httptest.Server {
	t.Helper()
	svc, err := service.NewUserService(repo, ids)
	if err != nil {
		t.Fatalf("NewUserService: %v", err)
	}
	h, err := transport.NewHandler(svc)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestEndToEnd_CreateThenGet(t *testing.T) {
	t.Parallel()
	srv := newServer(t, repository.NewInMemoryUserRepo(), fixedIDs{id: "usr-1"})

	resp, err := http.Post(srv.URL+"/users", "application/json",
		strings.NewReader(`{"name":"Grace","email":"grace@example.com"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	resp.Body.Close()
	if created.ID != "usr-1" || created.Name != "Grace" {
		t.Fatalf("created = %+v, want id=usr-1 name=Grace", created)
	}

	resp, err = http.Get(srv.URL + "/users/usr-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	resp.Body.Close()
	if got.Email != "grace@example.com" {
		t.Errorf("email = %q, want grace@example.com", got.Email)
	}
}

func TestEndToEnd_MissingUserIs404(t *testing.T) {
	t.Parallel()
	srv := newServer(t, repository.NewInMemoryUserRepo(), fixedIDs{id: "x"})

	resp, err := http.Get(srv.URL + "/users/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEndToEnd_InvalidInputIs400(t *testing.T) {
	t.Parallel()
	srv := newServer(t, repository.NewInMemoryUserRepo(), fixedIDs{id: "x"})

	resp, err := http.Post(srv.URL+"/users", "application/json",
		strings.NewReader(`{"name":"","email":"e@x.com"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEndToEnd_RepositoryFailureIs500(t *testing.T) {
	t.Parallel()
	srv := newServer(t, failingRepo{}, fixedIDs{id: "x"})

	resp, err := http.Post(srv.URL+"/users", "application/json",
		strings.NewReader(`{"name":"Grace","email":"grace@example.com"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(body["error"], "disk full") {
		t.Errorf("internal error leaked to client: %q", body["error"])
	}
}
```

## Review

The architecture is correct when the only package that imports `repository` is
`main`. Trace the imports: `transport` imports `domain` and `service` (for the
sentinels) but never `repository`; `service` imports `domain` only; `repository`
imports `domain` only; `main` imports all three concrete packages and is the lone
composition root. Each upper layer depends on the lower one through an interface it
declares itself — `UserService` in `transport`, `UserRepository` in `service` — so
the concrete types are chosen at the `main` boundary and substituted freely in
tests. The end-to-end test proves the seams are real by driving genuine HTTP through
the assembled stack, and the failing-repository test proves the bottom seam is
substitutable by swapping in a repo that fails and watching the failure surface as a
500 rather than a panic.

The common mistakes are three. The first is declaring `UserRepository` in the
`repository` package "next to" the implementation; that inverts the ownership and
makes `service` import the adapter, recoupling the layers — the interface belongs to
the consumer. The second is letting a layer below `transport` know about HTTP:
status codes, request bodies, and JSON tags live only in `transport`, and the
service speaks in domain values and typed errors. The third is leaking the internal
error to the client on a 500; classify with `errors.Is`, return a generic message,
and keep the wrapped cause for your logs — the test asserts the raw `disk full`
string never reaches the response body.

## Resources

- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — the standard router whose method-and-wildcard patterns this handler uses, no third-party framework required.
- [Routing Enhancements for Go 1.22](https://go.dev/blog/routing-enhancements) — how `POST /users` and `GET /users/{id}` plus `PathValue` work in the standard mux.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — the in-process server and client used to drive the whole stack end-to-end without binding a port.
- [Organizing a Go module](https://go.dev/doc/modules/layout) — the official guidance on splitting a module into `domain`/`service`/`transport` packages and a `cmd` composition root.

---

Back to [03-provider-container.md](03-provider-container.md) | Next: [05-order-processing-compensation.md](05-order-processing-compensation.md)
