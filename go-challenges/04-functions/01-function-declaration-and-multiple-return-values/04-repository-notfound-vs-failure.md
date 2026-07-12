# Exercise 4: A Repository That Distinguishes Not-Found From A Real Failure

A repository read can go wrong two very different ways: the row does not exist (a
`404`, a normal outcome), or the store is broken (a `500`, an operational
failure). A single `(User, bool)` collapses those into one indistinguishable
`false`. This exercise builds `FindByID(ctx, id) (User, error)` with a dedicated
`ErrNotFound` sentinel — exactly the `sql.ErrNoRows` pattern — so the caller can
tell absence from breakage with `errors.Is`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
userrepo/                  independent module: example.com/userrepo
  go.mod                   go 1.25
  userrepo.go              type User; ErrNotFound sentinel; Repo with FindByID(ctx,id) (User,error)
  handler.go               HTTP handler mapping ErrNotFound->404, any other error->500
  cmd/
    demo/
      main.go              found, not-found, and forced-failure lookups
  userrepo_test.go         found; errors.Is(ErrNotFound); a wrapped driver failure is NOT ErrNotFound; -race
```

- Files: `userrepo.go`, `handler.go`, `cmd/demo/main.go`, `userrepo_test.go`.
- Implement: an in-memory `Repo` with a package-level `ErrNotFound = errors.New(...)`; `FindByID` returns `(User, ErrNotFound)` for a missing id and `(User, wrapped driver error)` when the store is forced to fail.
- Test: a present id returns the user and nil; an absent id gives `errors.Is(err, ErrNotFound) == true`; a forced failure gives `errors.Is(err, ErrNotFound) == false` yet wraps the cause; a handler maps `ErrNotFound` to `404` and anything else to `500`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/04-repository-notfound-vs-failure/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/04-repository-notfound-vs-failure
go mod edit -go=1.25
```

### Why a bool is not enough here

The signature `FindByID(ctx, id) (User, bool)` would work for a pure in-memory
cache where the only thing that can happen is "present" or "absent". A repository
backed by a database has a third outcome that dwarfs the other two in importance:
the query itself failed — the connection dropped, the context was cancelled, the
driver returned an error. If `FindByID` folds that into `false`, the HTTP layer
replies `404 Not Found` to a user who exists, hiding an outage behind a wrong
status code, and no alert fires.

The stdlib solved this in `database/sql`: `Row.Scan` returns `sql.ErrNoRows` for
the empty-result case, distinct from any driver error. We mirror that. `ErrNotFound`
is a package-level sentinel created with `errors.New`. `FindByID` returns it
directly for a missing id, and returns a `%w`-wrapped driver error for a real
failure. The caller then writes:

```go
switch {
case errors.Is(err, ErrNotFound):
	// 404
case err != nil:
	// 500
}
```

`errors.Is(err, ErrNotFound)` is true only when the not-found sentinel is on the
chain; a wrapped driver failure is on a *different* chain, so `errors.Is` returns
false and the handler correctly escalates to `500`. Returning the sentinel by
identity (not wrapping it in `fmt.Errorf` unless you add context) keeps the match
cheap and unambiguous.

Create `userrepo.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is the sentinel for "no such user" — the repository analogue of
// sql.ErrNoRows. Callers match it with errors.Is and map it to 404.
var ErrNotFound = errors.New("user not found")

type User struct {
	ID   string
	Name string
}

// Repo is an in-memory user store. failNext, when set, simulates a driver/
// connection failure on the next read so tests can exercise the 500 path.
type Repo struct {
	users    map[string]User
	failNext error
}

func NewRepo() *Repo {
	return &Repo{users: make(map[string]User)}
}

// Add seeds a user (stands in for an INSERT).
func (r *Repo) Add(u User) {
	r.users[u.ID] = u
}

// FailNextWith forces the next FindByID to return a wrapped copy of err,
// simulating a store/driver failure.
func (r *Repo) FailNextWith(err error) {
	r.failNext = err
}

// FindByID returns the user for id. It distinguishes three outcomes:
//   - found:      (user, nil)
//   - absent:     (zero, ErrNotFound)       -> caller maps to 404
//   - store down: (zero, wrapped failure)   -> caller maps to 500
func (r *Repo) FindByID(ctx context.Context, id string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, fmt.Errorf("find user %q: %w", id, err)
	}
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return User{}, fmt.Errorf("find user %q: %w", id, err)
	}
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}
```

Create `handler.go`:

```go
package userrepo

import (
	"errors"
	"net/http"
)

// UserHandler serves GET /users/{id}, translating the repository's error shape
// into the right HTTP status: ErrNotFound -> 404, anything else -> 500.
func UserHandler(repo *Repo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		u, err := repo.FindByID(r.Context(), id)
		switch {
		case errors.Is(err, ErrNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
			return
		case err != nil:
			// Do not leak the internal cause to the client.
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(u.Name))
	}
}
```

`r.Context()` returns a `context.Context` without importing the package here, so
`handler.go` needs only `errors` and `net/http`.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/userrepo"
)

func main() {
	ctx := context.Background()
	repo := userrepo.NewRepo()
	repo.Add(userrepo.User{ID: "u1", Name: "alice"})

	if u, err := repo.FindByID(ctx, "u1"); err == nil {
		fmt.Printf("found: %s\n", u.Name)
	}

	_, err := repo.FindByID(ctx, "u2")
	fmt.Printf("absent: errors.Is(ErrNotFound)=%t\n", errors.Is(err, userrepo.ErrNotFound))

	repo.FailNextWith(errors.New("connection refused"))
	_, err = repo.FindByID(ctx, "u1")
	fmt.Printf("store down: errors.Is(ErrNotFound)=%t err=%v\n", errors.Is(err, userrepo.ErrNotFound), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: alice
absent: errors.Is(ErrNotFound)=true
store down: errors.Is(ErrNotFound)=false err=find user "u1": connection refused
```

### Tests

Create `userrepo_test.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFindByIDFound(t *testing.T) {
	t.Parallel()
	repo := NewRepo()
	repo.Add(User{ID: "u1", Name: "alice"})

	u, err := repo.FindByID(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Name != "alice" {
		t.Fatalf("Name = %q, want alice", u.Name)
	}
}

func TestFindByIDNotFound(t *testing.T) {
	t.Parallel()
	repo := NewRepo()

	_, err := repo.FindByID(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFindByIDStoreFailureIsNotNotFound(t *testing.T) {
	t.Parallel()
	repo := NewRepo()
	repo.Add(User{ID: "u1", Name: "alice"})
	driverErr := errors.New("connection refused")
	repo.FailNextWith(driverErr)

	_, err := repo.FindByID(context.Background(), "u1")
	if err == nil {
		t.Fatal("want a failure, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("store failure must NOT be ErrNotFound")
	}
	if !errors.Is(err, driverErr) {
		t.Fatalf("err = %v, want it to wrap the driver error", err)
	}
}

func TestHandlerStatusMapping(t *testing.T) {
	t.Parallel()
	repo := NewRepo()
	repo.Add(User{ID: "u1", Name: "alice"})

	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", UserHandler(repo))

	cases := []struct {
		name   string
		setup  func()
		id     string
		status int
	}{
		{"found", func() {}, "u1", http.StatusOK},
		{"not found", func() {}, "u2", http.StatusNotFound},
		{"store down", func() { repo.FailNextWith(errors.New("boom")) }, "u1", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			req := httptest.NewRequest(http.MethodGet, "/users/"+tc.id, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}
```

Note `TestHandlerStatusMapping` shares one `repo` across subcases and mutates it
with `FailNextWith`, so those subcases must not run in parallel with each other —
they do not call `t.Parallel()`.

## Review

The repository is correct when the three outcomes stay distinct: found is
`(user, nil)`, absent is `(zero, ErrNotFound)` matched by `errors.Is`, and a store
failure is a wrapped error that is *not* `ErrNotFound` yet still carries its cause.
`TestFindByIDStoreFailureIsNotNotFound` is the load-bearing test: it proves a
driver failure does not masquerade as not-found, which is exactly the bug a bare
`bool` return would cause. The handler test then shows the payoff — the same error
shape drives `404` versus `500` with no extra plumbing.

The mistake to avoid is wrapping `ErrNotFound` with additional context on the
not-found path *and* wrapping driver errors the same way, so that both chains look
alike — keep the not-found return as the bare sentinel (or a wrap that still has
`ErrNotFound` on its chain) and never put `ErrNotFound` on a real-failure chain.
Also, do not leak the internal cause to the HTTP client on the `500` path; log it
server-side and return a generic body.

## Resources

- [database/sql.ErrNoRows](https://pkg.go.dev/database/sql#ErrNoRows) — the stdlib not-found sentinel this pattern mirrors.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped chain against a sentinel.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the `%w` wrapping and `Is`/`As` model.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-http-handler-multi-return-unpack.md](03-http-handler-multi-return-unpack.md) | Next: [05-comma-ok-type-assertion-payload.md](05-comma-ok-type-assertion-payload.md)
