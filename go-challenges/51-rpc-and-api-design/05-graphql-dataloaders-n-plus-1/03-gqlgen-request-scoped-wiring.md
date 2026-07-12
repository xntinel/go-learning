# Exercise 3: Wiring Loaders Per Request in a gqlgen Server

This is the artifact you actually ship: an HTTP middleware that builds a fresh
loader bundle for every request, stashes it in the request context under an
unexported key, and a resolver that pulls it back out with a `For(ctx)` accessor.
An instrumented repository proves the payoff — a query returning N todos collapses
from N+1 round-trips to 2 — and a second test proves the security property, that
two requests never share a loader cache.

This module is self-contained and imports only `dataloadgen`. Nothing here
imports another exercise. The resolver method signatures match exactly what
gqlgen generates, so this drops into a real gqlgen service unchanged.

## What you'll build

```text
gqlserver/                   independent module: example.com/gqlserver
  go.mod                     go 1.26; require github.com/vikstrous/dataloadgen v0.0.10
  model.go                   Todo, User
  repo.go                    Repo (round-trip counters); Todos, batchUsers, SetUserName
  loaders.go                 Loaders bundle; NewLoaders; Middleware; For(ctx); ctxKey
  server.go                  Resolver (Todos, User); the concurrent executor; NewServer
  cmd/
    demo/
      main.go                runnable demo over httptest, prints round-trip counts
  server_test.go             N+1 collapse (2 round-trips), per-request loader isolation
```

- Files: `model.go`, `repo.go`, `loaders.go`, `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: a `Middleware` that constructs a fresh `Loaders` per request and stores it under an unexported `ctxKey`; a `For(ctx) *Loaders` accessor; a `Todo.User` resolver that calls `For(ctx).UserLoader.Load(ctx, obj.UserID)`; and an executor that resolves the sibling `user` fields concurrently, the way gqlgen does.
- Test: an `httptest` round-trip asserting the repo saw exactly one todos call and one batched users call (2, not 1+N); a second test that mutates a row between two requests and confirms the second request sees fresh data, proving loaders are per-request.
- Verify: `go test -count=1 -race ./...` (external dependency; fetch with `GOFLAGS=-mod=mod`).

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/03-gqlgen-request-scoped-wiring/cmd/demo
cd go-solutions/51-rpc-and-api-design/05-graphql-dataloaders-n-plus-1/03-gqlgen-request-scoped-wiring
go mod edit -go=1.26
go get github.com/vikstrous/dataloadgen@v0.0.10
```

### Why the loader must be built in middleware

The concepts file made the case: a dataloader's cache is a per-request identity
cache, so its lifetime must be exactly one request. The mechanism that enforces
that is ordinary HTTP middleware. On each request, `Middleware` calls
`NewLoaders(repo)` — a brand-new bundle with an empty cache — puts it in the
request context, and calls the next handler with the derived context. When the
handler returns, the request context goes out of scope and the loader (and its
cache) becomes garbage. There is no shared state to leak across requests, no
stale rows, and no cross-tenant bleed, because request B literally cannot reach
request A's loader.

The context key is unexported. A `string` or exported key risks another package
writing the same key and colliding; an unexported `type ctxKey struct{}` is
unique to this package and cannot be forged or overwritten from outside. `For`
hides the type assertion behind a typed accessor so resolvers never touch
`ctx.Value` directly.

### Why concurrent resolution is what makes batching work

gqlgen resolves the elements of a list concurrently: for `todos { user { name }}`
it runs the `Todo.user` resolver for every todo on its own goroutine. Those
goroutines each call `UserLoader.Load` at nearly the same instant, so the keys
pile into one batch inside the wait window and a single `batchUsers` round-trip
serves them all. The executor in this exercise reproduces that concurrency
explicitly with a `WaitGroup` and one goroutine per todo — not because you would
write an executor by hand in production (gqlgen generates it), but so the batching
is visible and the round-trip count is something a test can assert. Without the
concurrency there would be nothing to batch: each `Load` would resolve before the
next began, and you would be back at N+1.

Create `model.go`:

```go
package gqlserver

// Todo is a parent row; its UserID points at the User to resolve for the nested
// user field.
type Todo struct {
	ID     int
	Title  string
	UserID int
}

// User is the entity the Todo.user field resolves to.
type User struct {
	ID   int
	Name string
}
```

Create `repo.go`. The counters are the whole point: they let a test see how many
times the datastore was actually hit.

```go
package gqlserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrUserNotFound is wrapped for a missing id in the batch fetch.
var ErrUserNotFound = errors.New("gqlserver: user not found")

// Repo is an in-memory stand-in for a database that counts round-trips so the
// N+1 collapse is observable.
type Repo struct {
	mu        sync.RWMutex
	todos     []Todo
	users     map[int]User
	todoCalls atomic.Int64
	userCalls atomic.Int64
}

// NewRepo seeds a Repo.
func NewRepo(todos []Todo, users []User) *Repo {
	m := make(map[int]User, len(users))
	for _, u := range users {
		m[u.ID] = u
	}
	return &Repo{todos: todos, users: m}
}

// TodoCalls and UserCalls report the number of round-trips of each kind.
func (r *Repo) TodoCalls() int64 { return r.todoCalls.Load() }
func (r *Repo) UserCalls() int64 { return r.userCalls.Load() }

// Todos returns all todos in one round-trip.
func (r *Repo) Todos(ctx context.Context) ([]*Todo, error) {
	r.todoCalls.Add(1)
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Todo, len(r.todos))
	for i := range r.todos {
		td := r.todos[i]
		out[i] = &td
	}
	return out, nil
}

// batchUsers is the loader's batch fetch: one round-trip returning users aligned
// by position with ids. A missing id occupies its slot with a wrapped error so
// siblings are not disturbed.
func (r *Repo) batchUsers(ctx context.Context, ids []int) ([]*User, []error) {
	r.userCalls.Add(1)
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*User, len(ids))
	errs := make([]error, len(ids))
	for i, id := range ids {
		if u, ok := r.users[id]; ok {
			cp := u
			out[i] = &cp
		} else {
			errs[i] = fmt.Errorf("user %d: %w", id, ErrUserNotFound)
		}
	}
	return out, errs
}

// SetUserName mutates a user, used to prove a later request sees fresh data.
func (r *Repo) SetUserName(id int, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[id]
	u.Name = name
	r.users[id] = u
}
```

Create `loaders.go`:

```go
package gqlserver

import (
	"context"
	"net/http"
	"time"

	"github.com/vikstrous/dataloadgen"
)

// Loaders is the per-request bundle of dataloaders. A real service has one field
// per batched entity; here there is just the user loader.
type Loaders struct {
	UserLoader *dataloadgen.Loader[int, *User]
}

// NewLoaders builds a fresh bundle with an empty cache. Call it once per request.
func NewLoaders(repo *Repo) *Loaders {
	return &Loaders{
		UserLoader: dataloadgen.NewLoader(
			repo.batchUsers,
			dataloadgen.WithWait(2*time.Millisecond),
		),
	}
}

// ctxKey is unexported so no other package can collide with or forge it.
type ctxKey struct{}

// Middleware builds a fresh Loaders for each request and stores it in the
// request context, so the loader cache lives and dies with the request.
func Middleware(repo *Repo, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loaders := NewLoaders(repo)
		ctx := context.WithValue(r.Context(), ctxKey{}, loaders)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// For returns the loaders bound to this request's context. It panics if the
// middleware was not applied, which surfaces a wiring bug immediately.
func For(ctx context.Context) *Loaders {
	return ctx.Value(ctxKey{}).(*Loaders)
}
```

Create `server.go`. The resolver methods have the exact signatures gqlgen
generates — `Todos(ctx)` for the query field and `User(ctx, obj *Todo)` for the
nested field — so this code is what you would paste into a generated resolver.

```go
package gqlserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// Resolver holds the dependencies gqlgen resolvers close over.
type Resolver struct {
	Repo *Repo
}

// Todos resolves the query root field with a single round-trip.
func (r *Resolver) Todos(ctx context.Context) ([]*Todo, error) {
	return r.Repo.Todos(ctx)
}

// User resolves the nested Todo.user field. It calls the request-scoped loader
// instead of the repo directly, so the N sibling calls coalesce into one batch.
func (r *Resolver) User(ctx context.Context, obj *Todo) (*User, error) {
	return For(ctx).UserLoader.Load(ctx, obj.UserID)
}

type todoView struct {
	Title string `json:"title"`
	User  string `json:"user"`
}

// executeTodosQuery mimics gqlgen: it resolves the list field once, then resolves
// the nested user field for every element concurrently so their Load calls batch.
func executeTodosQuery(res *Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		todos, err := res.Todos(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		views := make([]todoView, len(todos))
		var wg sync.WaitGroup
		for i, td := range todos {
			wg.Add(1)
			go func() {
				defer wg.Done()
				views[i].Title = td.Title
				if u, err := res.User(ctx, td); err == nil && u != nil {
					views[i].User = u.Name
				}
			}()
		}
		wg.Wait()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(views)
	}
}

// NewServer wires the middleware around the query handler.
func NewServer(repo *Repo) http.Handler {
	res := &Resolver{Repo: repo}
	return Middleware(repo, executeTodosQuery(res))
}
```

### The runnable demo

The demo starts the server over `httptest`, issues one request, and prints the
body plus the round-trip counters — three todos over two distinct users resolve in
one todos call and one batched users call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/gqlserver"
)

func main() {
	repo := gqlserver.NewRepo(
		[]gqlserver.Todo{
			{ID: 1, Title: "write RFC", UserID: 10},
			{ID: 2, Title: "review PR", UserID: 11},
			{ID: 3, Title: "deploy", UserID: 10},
		},
		[]gqlserver.User{
			{ID: 10, Name: "Alice"},
			{ID: 11, Name: "Bob"},
		},
	)

	srv := httptest.NewServer(gqlserver.NewServer(repo))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	fmt.Print(string(body))
	fmt.Printf("todo round-trips=%d user round-trips=%d\n", repo.TodoCalls(), repo.UserCalls())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[{"title":"write RFC","user":"Alice"},{"title":"review PR","user":"Bob"},{"title":"deploy","user":"Alice"}]
todo round-trips=1 user round-trips=1
```

### Tests

`TestN1Collapse` runs a real request through the middleware and asserts the repo
saw exactly one todos round-trip and one users round-trip — 2 total, not 1+N —
which is the whole reason the loader exists. `TestPerRequestLoaderIsolation`
issues one request, mutates a user through the repo, then issues a second request
and asserts the second response carries the new name; if loaders were shared, the
second request would return the first request's cached value.

Create `server_test.go`:

```go
package gqlserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seededRepo() *Repo {
	return NewRepo(
		[]Todo{
			{ID: 1, Title: "a", UserID: 10},
			{ID: 2, Title: "b", UserID: 11},
			{ID: 3, Title: "c", UserID: 10},
			{ID: 4, Title: "d", UserID: 11},
		},
		[]User{
			{ID: 10, Name: "Alice"},
			{ID: 11, Name: "Bob"},
		},
	)
}

func getViews(t *testing.T, url string) []todoView {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var views []todoView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return views
}

func TestN1Collapse(t *testing.T) {
	t.Parallel()
	repo := seededRepo()
	srv := httptest.NewServer(NewServer(repo))
	defer srv.Close()

	views := getViews(t, srv.URL)
	if len(views) != 4 {
		t.Fatalf("got %d views; want 4", len(views))
	}
	if views[0].User != "Alice" || views[1].User != "Bob" {
		t.Errorf("wrong user mapping: %+v", views)
	}
	if repo.TodoCalls() != 1 {
		t.Errorf("todo round-trips = %d; want 1", repo.TodoCalls())
	}
	if repo.UserCalls() != 1 {
		t.Errorf("user round-trips = %d; want 1 (batched), got N+1 if higher", repo.UserCalls())
	}
}

func TestPerRequestLoaderIsolation(t *testing.T) {
	t.Parallel()
	repo := seededRepo()
	srv := httptest.NewServer(NewServer(repo))
	defer srv.Close()

	first := getViews(t, srv.URL)
	if first[0].User != "Alice" {
		t.Fatalf("first request user = %q; want Alice", first[0].User)
	}

	repo.SetUserName(10, "Alice2")

	second := getViews(t, srv.URL)
	if second[0].User != "Alice2" {
		t.Errorf("second request user = %q; want Alice2 (a shared loader would cache Alice)", second[0].User)
	}
}

func Example() {
	repo := seededRepo()
	srv := httptest.NewServer(NewServer(repo))
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Four todos over two users: one todos round-trip, one batched users round-trip.
	fmt.Printf("todo=%d user=%d\n", repo.TodoCalls(), repo.UserCalls())
	// Output: todo=1 user=1
}
```

## Review

The server is correct when a list query costs a constant number of round-trips.
`TestN1Collapse` is the proof: four todos resolve their `user` fields in a single
batched `batchUsers` call, so the repo sees two round-trips regardless of list
length. If `UserCalls` grows with the number of todos, the loader is not batching
— usually because the sibling resolvers are running sequentially rather than
concurrently, or because a fresh loader is being built inside the loop instead of
once per request.

The correctness-and-security property is per-request isolation, checked by
`TestPerRequestLoaderIsolation`: because `Middleware` builds a new `Loaders` for
every request, a mutation between two requests is visible to the second one. A
package-level loader would fail this test by serving the first request's cached
row — the same mechanism that, across tenants, would serve one user's data to
another. Keep the loader construction in the middleware, the context key
unexported, and the batch fetch reading straight from the datastore (never back
through the loader, which would deadlock). Run `go test -race` to confirm the
concurrent resolver goroutines and the loader are race-free.

## Resources

- [gqlgen: Dataloaders](https://gqlgen.com/reference/dataloaders/) — the official guide to per-request loaders and middleware wiring.
- [`dataloadgen` reference](https://pkg.go.dev/github.com/vikstrous/dataloadgen) — the `Loader` type and options used in the bundle.
- [`context.WithValue`](https://pkg.go.dev/context#WithValue) — the unexported-key pattern for request-scoped values.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — the in-process server the tests use to exercise the middleware.

---

Back to [02-dataloadgen-loaders.md](02-dataloadgen-loaders.md) | Next: [../06-api-versioning-strategies/00-concepts.md](../06-api-versioning-strategies/00-concepts.md)
