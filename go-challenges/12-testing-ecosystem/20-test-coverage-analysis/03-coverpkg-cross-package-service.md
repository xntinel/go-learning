# Exercise 3: Cross-package coverage of an HTTP handler over service and repo layers

In a layered service the test lives in the `handler` package but the logic lives
in `service/` and `repo/`. The default `go test -cover` only credits statements in
the package under test, so a handler test that drives real logic in two other
packages reports nothing about them — and a naive reading concludes those layers
are untested. This module builds a three-package service and uses `-coverpkg` to
attribute execution across the boundary, revealing both the true number and one
repo branch no test touches.

This module is fully self-contained: one module, three internal packages, its own
demo and tests.

## What you'll build

```text
svc/                       independent module: example.com/svc
  go.mod
  repo/
    repo.go                UserRepo: in-memory store; Find, Save; ErrNotFound
  service/
    service.go             UserService: Register, Lookup over the repo
  handler/
    handler.go             HTTP handler: POST /users, GET /users/{id}
    handler_test.go        drives handler -> service -> repo via httptest
  cmd/
    demo/
      main.go              runnable demo hitting the handler in-process
```

- Files: `repo/repo.go`, `service/service.go`, `handler/handler.go`, `handler/handler_test.go`, `cmd/demo/main.go`.
- Implement: an in-memory `repo.UserRepo` (`Find`, `Save`, sentinel `ErrNotFound`), a `service.UserService` (`Register`, `Lookup`), and an `http.Handler` that routes `POST /users` and `GET /users/{id}` through the service.
- Test: `handler_test.go` sends requests through the real handler → service → repo chain with `httptest`, asserting both the created and not-found paths.
- Verify: `go test -coverpkg=./... -coverprofile=cover.out ./... && go tool cover -func=cover.out`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/20-test-coverage-analysis/03-coverpkg-cross-package-service/{repo,service,handler} go-solutions/12-testing-ecosystem/20-test-coverage-analysis/03-coverpkg-cross-package-service/cmd/demo
cd go-solutions/12-testing-ecosystem/20-test-coverage-analysis/03-coverpkg-cross-package-service
```

### Why the default number lies about layered code

`go test ./handler` instruments the `handler` package and reports the fraction of
`handler` statements executed. But when a request flows into `service.Lookup` and
then `repo.Find`, those statements belong to *other* packages, so the default
attribution ignores them entirely. Run `go test -cover ./...` and each package
gets its own line computed in isolation: `handler` shows the handler's own
coverage, `service` shows 0% (it has no `_test.go` of its own), `repo` shows 0%.
The conclusion "service and repo are untested" is false — the handler test ran
them thoroughly — it is an artifact of per-package attribution.

`-coverpkg=./...` changes the instrumented set to *all* matching packages, so the
handler test's execution is credited wherever it lands. Now the single profile
shows the real cross-package coverage: the handler, service, and repo lines all
reflect what the request actually touched. And because the profile now spans repo,
it also exposes what the request did *not* touch — here, `repo.Save`'s duplicate
branch and any repo method the handler never calls sit at less than 100%, which is
the genuinely useful output: a to-do list of untested code across the whole
service, not a misleading per-package snapshot.

Create `repo/repo.go`:

```go
package repo

import (
	"errors"
	"sync"
)

// ErrNotFound is returned by Find when no user has the given id.
var ErrNotFound = errors.New("user not found")

// ErrExists is returned by Save when the id is already taken.
var ErrExists = errors.New("user already exists")

// User is a stored user record.
type User struct {
	ID   string
	Name string
}

// UserRepo is an in-memory user store.
type UserRepo struct {
	mu    sync.RWMutex
	users map[string]User
}

// NewUserRepo returns an empty repository.
func NewUserRepo() *UserRepo {
	return &UserRepo{users: make(map[string]User)}
}

// Find returns the user with id, or ErrNotFound.
func (r *UserRepo) Find(id string) (User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

// Save stores u, rejecting a duplicate id with ErrExists.
func (r *UserRepo) Save(u User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[u.ID]; ok {
		return ErrExists
	}
	r.users[u.ID] = u
	return nil
}
```

Create `service/service.go`:

```go
package service

import (
	"fmt"
	"strings"

	"example.com/svc/repo"
)

// UserService holds the business rules over a UserRepo.
type UserService struct {
	repo *repo.UserRepo
}

// New returns a service backed by r.
func New(r *repo.UserRepo) *UserService {
	return &UserService{repo: r}
}

// Register validates and stores a new user.
func (s *UserService) Register(id, name string) (repo.User, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
		return repo.User{}, fmt.Errorf("register: id and name are required")
	}
	u := repo.User{ID: id, Name: name}
	if err := s.repo.Save(u); err != nil {
		return repo.User{}, fmt.Errorf("register %q: %w", id, err)
	}
	return u, nil
}

// Lookup returns the user with id.
func (s *UserService) Lookup(id string) (repo.User, error) {
	u, err := s.repo.Find(id)
	if err != nil {
		return repo.User{}, fmt.Errorf("lookup %q: %w", id, err)
	}
	return u, nil
}
```

Create `handler/handler.go`:

```go
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"example.com/svc/repo"
	"example.com/svc/service"
)

// New returns an http.Handler routing user endpoints through svc.
func New(svc *service.UserService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		u, err := svc.Register(body.ID, body.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, err := svc.Lookup(r.PathValue("id"))
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u)
	})
	return mux
}
```

### The runnable demo

The demo wires the three layers in-process, registers a user, reads it back, and
attempts a missing lookup, printing the HTTP status codes so `go run ./cmd/demo`
is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"strings"

	"example.com/svc/handler"
	"example.com/svc/repo"
	"example.com/svc/service"
)

func main() {
	h := handler.New(service.New(repo.NewUserRepo()))
	srv := httptest.NewServer(h)
	defer srv.Close()

	create, _ := srv.Client().Post(srv.URL+"/users", "application/json",
		strings.NewReader(`{"id":"u1","name":"Alice"}`))
	fmt.Println("POST /users:", create.StatusCode)
	create.Body.Close()

	get, _ := srv.Client().Get(srv.URL + "/users/u1")
	fmt.Println("GET /users/u1:", get.StatusCode)
	get.Body.Close()

	miss, _ := srv.Client().Get(srv.URL + "/users/u404")
	fmt.Println("GET /users/u404:", miss.StatusCode)
	miss.Body.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
POST /users: 201
GET /users/u1: 200
GET /users/u404: 404
```

### The test drives all three layers

`handler_test.go` uses `httptest.NewRequest` and `httptest.NewRecorder` to send
requests straight into the handler. Each request flows handler → service → repo,
so under `-coverpkg=./...` all three packages accumulate coverage from this one
test file. The table covers the created path, the read-back path, the not-found
path, and a validation rejection.

Create `handler/handler_test.go`:

```go
package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/svc/repo"
	"example.com/svc/service"
)

func newHandler() http.Handler {
	return New(service.New(repo.NewUserRepo()))
}

func TestRegisterAndLookup(t *testing.T) {
	t.Parallel()
	h := newHandler()

	post := httptest.NewRequest(http.MethodPost, "/users",
		strings.NewReader(`{"id":"u1","name":"Alice"}`))
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, post)
	if pw.Code != http.StatusCreated {
		t.Fatalf("POST /users = %d, want 201; body=%s", pw.Code, pw.Body)
	}

	get := httptest.NewRequest(http.MethodGet, "/users/u1", nil)
	gw := httptest.NewRecorder()
	h.ServeHTTP(gw, get)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET /users/u1 = %d, want 200", gw.Code)
	}
	if !strings.Contains(gw.Body.String(), "Alice") {
		t.Fatalf("GET body = %q, want it to contain Alice", gw.Body.String())
	}
}

func TestLookupNotFound(t *testing.T) {
	t.Parallel()
	h := newHandler()
	req := httptest.NewRequest(http.MethodGet, "/users/missing", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET missing = %d, want 404", w.Code)
	}
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()
	h := newHandler()
	req := httptest.NewRequest(http.MethodPost, "/users",
		strings.NewReader(`{"id":"","name":""}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST empty = %d, want 400", w.Code)
	}
}

func TestBadJSON(t *testing.T) {
	t.Parallel()
	h := newHandler()
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("{"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST bad json = %d, want 400", w.Code)
	}
}
```

### Seeing the difference `-coverpkg` makes

First the misleading default. Each package is measured in isolation:

```bash
go test -cover ./...
```

Expected output (service and repo have no tests of their own, so they report no
statements):

```
ok      example.com/svc/handler   0.01s  coverage: 84.6% of statements
?       example.com/svc/repo      [no test files]
?       example.com/svc/service   [no test files]
```

Now attribute execution across the whole module with `-coverpkg=./...`. The single
profile credits the handler test's run of the service and repo code:

```bash
go test -coverpkg=./... -coverprofile=cover.out ./...
go tool cover -func=cover.out
```

Expected output (abbreviated) — note `repo.Save`'s duplicate branch is below 100%
because no test registers the same id twice, and the `ErrExists` path in
`service.Register` is likewise uncovered:

```
example.com/svc/handler/handler.go:...  New          92.3%
example.com/svc/repo/repo.go:...         Find         100.0%
example.com/svc/repo/repo.go:...         Save         66.7%
example.com/svc/service/service.go:...   Register     85.7%
example.com/svc/service/service.go:...   Lookup       100.0%
total:                                   (statements) 88.9%
```

The exact percentages depend on your formatting; the shape is what matters: repo
and service now carry real coverage, and the profile points straight at the
untested duplicate-id branch. Add a test that registers `u1` twice and asserts a
`400`, and `repo.Save` and `service.Register`'s error paths close.

## Review

The service is correct when a `POST` creates a user (201), a `GET` reads it back
(200), a missing id returns 404 via `errors.Is(err, repo.ErrNotFound)`, and empty
fields or malformed JSON return 400. The coverage lesson is correct when
`go test -cover ./...` shows service and repo with no attributed coverage while
`go test -coverpkg=./...` credits them — proving the default under-reports layered
code, not that the layers are untested.

The mistake to avoid is reading the default per-package number for a service whose
tests live in a different package and concluding the logic is untested. Use
`-coverpkg=./...` (or a specific package list) so cross-package execution is
attributed, then read the profile for the branches — like the duplicate-id path —
that genuinely have zero coverage and write asserting tests for them. Run
`go test -race ./...` to confirm the repo's mutex holds under the concurrent
handler tests.

## Resources

- [Testing flags (`-coverpkg`)](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — how `-coverpkg` changes the instrumented set.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder`, and `NewServer`.
- [`go tool cover`](https://pkg.go.dev/cmd/cover) — reading a multi-package profile with `-func`.

---

Back to [02-covermode-atomic-race-cache.md](02-covermode-atomic-race-cache.md) | Next: [04-integration-coverage-server-binary.md](04-integration-coverage-server-binary.md)
