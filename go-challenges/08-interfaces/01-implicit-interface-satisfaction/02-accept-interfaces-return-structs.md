# Exercise 2: An HTTP Handler That Depends on a Narrow Consumer-Defined Interface

The "accept interfaces, return concrete structs" idiom is not abstract advice — it
is what makes an HTTP handler testable without a mocking framework. This module
builds a `GET /users/{id}` handler that depends on a one-method `UserReader`
interface declared in the handler's own package, while the concrete repository is
returned as a struct. The test injects a hand-written fake, and that fake is
trivial precisely because the interface is narrow.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
usersvc/                      independent module: example.com/usersvc
  go.mod                      go 1.26
  handler.go                  UserReader interface (GetUser); User; UsersHandler; ErrUserNotFound
  repo.go                     concrete PostgresUserRepo returned as a struct (in-memory stand-in)
  cmd/
    demo/
      main.go                 runnable demo: wire repo into handler, serve two requests
  handler_test.go             hand-written fake UserReader; 200 + JSON and 404 via httptest
```

- Files: `handler.go`, `repo.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: a `UserReader` interface (`GetUser(ctx, id) (User, error)`), a `UsersHandler` that depends on it and writes JSON, and a concrete repository constructor that returns `*PostgresUserRepo`.
- Test: a fake `UserReader` returning a canned user or `ErrUserNotFound`, driven through `httptest.NewRecorder`; assert 200 with a JSON body and 404.
- Verify: `go test -count=1 -race ./...`

### The seam: consumer declares the interface, producer returns a struct

The handler needs exactly one operation on its data dependency: fetch a user by
id. So the handler's package declares `UserReader` with that one method and
nothing else. It does not import a repository package to get an interface; it owns
the interface. The concrete repository (`PostgresUserRepo`, here backed by an
in-memory map so the module builds offline) is constructed by a function that
returns `*PostgresUserRepo` — the concrete type, not `UserReader`. The caller in
`main` wires the concrete repo into the handler, and the assignment to a
`UserReader` parameter is where satisfaction is checked.

Why this matters operationally: the test does not need `gomock`, `mockery`, or any
framework. Because `UserReader` is one method, a fake is a five-line struct with a
function field. The interface being narrow is what makes the fake cheap; a fat
repository interface would force the fake to stub methods the handler never calls.

The handler uses `http.Request.PathValue("id")`, the Go 1.22+ router-pattern
accessor, so it reads the `{id}` wildcard without a third-party router. It writes
the user as JSON on success and a 404 with a plain body when the repository
reports `ErrUserNotFound` via `errors.Is`. Any other error is a 500 — the handler
distinguishes "not found" (client's fault, 404) from "something broke" (500).

Create `handler.go`:

```go
package usersvc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ErrUserNotFound is returned by a UserReader when no user has the given id.
var ErrUserNotFound = errors.New("user not found")

// User is the domain type the handler serializes.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// UserReader is the narrow, consumer-defined interface the handler depends on.
// It declares only the one method the handler calls. The concrete repository
// satisfies it implicitly.
type UserReader interface {
	GetUser(ctx context.Context, id string) (User, error)
}

// UsersHandler serves GET /users/{id}. It accepts an interface (UserReader),
// which is what makes it testable with a hand-written fake.
type UsersHandler struct {
	Users UserReader
}

// ServeHTTP implements http.Handler.
func (h *UsersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	u, err := h.Users.GetUser(r.Context(), id)
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

Create `repo.go` — the concrete producer, returned as a struct:

```go
package usersvc

import "context"

// PostgresUserRepo is the concrete repository. In production its methods would
// query a database; here it is backed by an in-memory map so the module builds
// offline. The constructor returns the concrete *PostgresUserRepo, not an
// interface: callers keep the full type and choose which narrow interface to
// view it through.
type PostgresUserRepo struct {
	rows map[string]User
}

// NewPostgresUserRepo returns a concrete *PostgresUserRepo seeded with rows.
func NewPostgresUserRepo(seed map[string]User) *PostgresUserRepo {
	rows := make(map[string]User, len(seed))
	for k, v := range seed {
		rows[k] = v
	}
	return &PostgresUserRepo{rows: rows}
}

// GetUser satisfies UserReader implicitly.
func (r *PostgresUserRepo) GetUser(_ context.Context, id string) (User, error) {
	u, ok := r.rows[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return u, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/usersvc"
)

func main() {
	repo := usersvc.NewPostgresUserRepo(map[string]usersvc.User{
		"42": {ID: "42", Name: "Alice"},
	})

	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", &usersvc.UsersHandler{Users: repo})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, id := range []string{"42", "99"} {
		resp, err := http.Get(srv.URL + "/users/" + id)
		if err != nil {
			fmt.Println("request error:", err)
			return
		}
		fmt.Printf("GET /users/%s -> %d\n", id, resp.StatusCode)
		resp.Body.Close()
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /users/42 -> 200
GET /users/99 -> 404
```

### Tests

The test never touches `PostgresUserRepo`. It injects `fakeUserReader`, a struct
whose `GetUser` returns whatever the test configured — a canned user, or
`ErrUserNotFound`. Because `UserReader` has one method, the fake is trivial. The
handler is driven with `httptest.NewRequest` (with the `{id}` path value set via
`SetPathValue`, since the request bypasses the router) and `httptest.NewRecorder`,
and the recorder's status and body are asserted.

Create `handler_test.go`:

```go
package usersvc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUserReader is a hand-written fake. It exists only because UserReader is
// narrow: one method, so the fake is one function field.
type fakeUserReader struct {
	user User
	err  error
}

func (f fakeUserReader) GetUser(_ context.Context, _ string) (User, error) {
	return f.user, f.err
}

func TestUsersHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string
		reader     fakeUserReader
		wantStatus int
		wantName   string
	}{
		{
			name:       "found returns 200 and JSON",
			id:         "42",
			reader:     fakeUserReader{user: User{ID: "42", Name: "Alice"}},
			wantStatus: http.StatusOK,
			wantName:   "Alice",
		},
		{
			name:       "missing returns 404",
			id:         "99",
			reader:     fakeUserReader{err: ErrUserNotFound},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := &UsersHandler{Users: tc.reader}

			req := httptest.NewRequest(http.MethodGet, "/users/"+tc.id, nil)
			req.SetPathValue("id", tc.id)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}

			var got User
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got.Name != tc.wantName {
				t.Fatalf("name = %q, want %q", got.Name, tc.wantName)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestPostgresUserRepoSatisfiesUserReader(t *testing.T) {
	t.Parallel()

	// Compile-time and runtime proof that the concrete producer satisfies the
	// consumer-defined interface.
	var _ UserReader = (*PostgresUserRepo)(nil)

	repo := NewPostgresUserRepo(map[string]User{"1": {ID: "1", Name: "Bob"}})
	u, err := repo.GetUser(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "Bob" {
		t.Fatalf("name = %q, want Bob", u.Name)
	}
}
```

## Review

The design is correct when the handler names the interface it needs and nothing
more, the repository constructor returns the concrete `*PostgresUserRepo`, and the
test uses a hand-written fake rather than a framework. The tell that you got the
seam right is exactly that the fake is cheap: a one-method interface yields a one-
method fake. The common mistake is to invert this — define a twelve-method
`Repository` in a shared package, import it into the handler, and then reach for a
mock generator because stubbing twelve methods by hand is tedious. That tedium is
a signal the interface is too wide and lives in the wrong place. Note that the
request in the test sets the path value with `req.SetPathValue`, because a request
built with `httptest.NewRequest` did not pass through the router that would
otherwise populate it. Run `go test -race` to confirm the handler and fake are
concurrency-clean.

## Resources

- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — accept interfaces, return concrete types.
- [`http.Request.PathValue`](https://pkg.go.dev/net/http#Request.PathValue) — reading `{id}` wildcards from the 1.22+ router.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for handler tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-pointer-vs-value-receiver-method-sets.md](03-pointer-vs-value-receiver-method-sets.md)
