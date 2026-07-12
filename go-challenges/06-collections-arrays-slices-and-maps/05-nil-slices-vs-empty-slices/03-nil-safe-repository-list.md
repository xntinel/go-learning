# Exercise 3: A repository List() that never hands a nil slice to the API

A repository method that collects matches by appending to a locally-declared
slice returns nil the moment nothing matches — and if the HTTP layer marshals
that nil, the client gets `null` where it expected `[]`. This exercise fixes the
contract at the right place: the repository normalizes to a non-nil slice at the
port boundary, so the whole layer above it is spared from thinking about nil.

This module is fully self-contained: its own `go mod init`, its own `repo`
package, its own demo and tests.

## What you'll build

```text
userrepo/                     independent module: example.com/userrepo
  go.mod
  repo/repo.go                UserRepository.List (non-nil contract), internal matching helper, Handler
  repo/repo_test.go           non-nil-on-empty, order, and repo->handler->JSON round trips
  cmd/demo/main.go            lists all and a no-match filter, prints nil-ness
```

Files: `repo/repo.go`, `repo/repo_test.go`, `cmd/demo/main.go`.
Implement: `UserRepository.List(ctx, Filter)` that always returns a non-nil
slice, backed by an internal `matching` helper that is allowed to return nil, and
a `Handler` that serializes `{"users":[...]}`.
Test: a no-match filter yields a non-nil length-zero slice; the full
repo→handler→JSON round trip yields `"users":[]`; populated results preserve
order; a regression guard against returning nil.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/03-nil-safe-repository-list/repo go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/03-nil-safe-repository-list/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/03-nil-safe-repository-list
```

### Normalize once, at the boundary

The natural way to write a filter is to declare a result slice and append the
matches: `var out []User; for ... { out = append(out, u) }`. That is idiomatic
and correct, and when zero users match, `out` is still nil, because `append` was
never called. Inside the package that nil is harmless — every caller ranges or
lengths it without noticing. The danger is only at the edge, when the HTTP layer
marshals it and emits `null`.

The wrong fix is to sprinkle `if out == nil { out = []User{} }` everywhere a
slice is handed around, or to make every internal helper non-nil "to be safe."
That spreads a boundary concern through the whole codebase and still misses cases.
The right fix is to name the boundary and normalize exactly there. Here the port
boundary is `List` — the method the API layer calls. Its internal helper
`matching` is free to return nil; `List` normalizes the result to `make([]User, 0)`
if it came back nil, and documents that it never returns nil. Everything above
`List` can now assume a non-nil slice, and everything below it can stay simple.

The `Handler` closes the loop: because `List` never returns nil, the JSON encoder
always sees a non-nil slice and writes `"users":[]` for an empty result, never
`"users":null`. The contract is enforced in one place and observable at the wire.

Create `repo/repo.go`:

```go
package repo

import (
	"context"
	"encoding/json"
	"net/http"
)

// User is a stored record.
type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Dept string `json:"dept"`
}

// Filter selects users. A zero Filter matches everything.
type Filter struct {
	Dept string // "" matches any department
}

// UserRepository is an in-memory store standing in for a database-backed repo.
type UserRepository struct {
	users []User
}

// NewUserRepository seeds the store.
func NewUserRepository(seed ...User) *UserRepository {
	return &UserRepository{users: seed}
}

// matching is the INTERNAL filter helper. It is allowed to return a nil slice
// on zero matches; nil vs empty does not matter inside the package.
func (r *UserRepository) matching(f Filter) []User {
	var out []User // nil is fine here
	for _, u := range r.users {
		if f.Dept != "" && u.Dept != f.Dept {
			continue
		}
		out = append(out, u)
	}
	return out
}

// List is the PORT boundary the API layer calls. Its contract: it never returns
// a nil slice, so the HTTP layer always serializes "users":[] (never null) for
// an empty result. Normalization happens here, once, not in every caller.
func (r *UserRepository) List(ctx context.Context, f Filter) []User {
	_ = ctx
	out := r.matching(f)
	if out == nil {
		out = make([]User, 0)
	}
	return out
}

// listBody is the response envelope.
type listBody struct {
	Users []User `json:"users"`
}

// Handler serves the list as JSON. Because List never returns nil, an empty
// result serializes as {"users":[]}.
func (r *UserRepository) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		users := r.List(req.Context(), Filter{Dept: req.URL.Query().Get("dept")})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listBody{Users: users})
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"example.com/userrepo/repo"
)

func main() {
	r := repo.NewUserRepository(
		repo.User{ID: 1, Name: "alice", Dept: "eng"},
		repo.User{ID: 2, Name: "bob", Dept: "sales"},
	)

	all := r.List(context.Background(), repo.Filter{})
	none := r.List(context.Background(), repo.Filter{Dept: "legal"})

	fmt.Printf("all:  len=%d nil=%v\n", len(all), all == nil)
	fmt.Printf("none: len=%d nil=%v\n", len(none), none == nil)

	b, _ := json.Marshal(struct {
		Users []repo.User `json:"users"`
	}{none})
	fmt.Printf("empty result JSON: %s\n", b)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all:  len=2 nil=false
none: len=0 nil=false
empty result JSON: {"users":[]}
```

### Tests

`TestListNoMatchReturnsNonNilEmpty` is the regression guard: it fails if a future
change lets `List` return nil, which is exactly the bug that would break a paging
client. `TestListPreservesOrder` confirms filtering keeps store order.
`TestRoundTripEmptyResultIsArray` drives a real `httptest.Server`, decodes the
`users` field as raw JSON, and asserts it is literally `[]`.
`TestRoundTripPopulatedIsArray` pins the full body for a populated result.

Create `repo/repo_test.go`:

```go
package repo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func seed() *UserRepository {
	return NewUserRepository(
		User{ID: 1, Name: "alice", Dept: "eng"},
		User{ID: 2, Name: "bob", Dept: "sales"},
		User{ID: 3, Name: "carol", Dept: "eng"},
	)
}

func TestListNoMatchReturnsNonNilEmpty(t *testing.T) {
	t.Parallel()
	got := seed().List(context.Background(), Filter{Dept: "legal"})
	if got == nil {
		t.Fatal("List returned a nil slice; the port contract is non-nil")
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestListPreservesOrder(t *testing.T) {
	t.Parallel()
	got := seed().List(context.Background(), Filter{Dept: "eng"})
	want := []int{1, 3}
	ids := make([]int, len(got))
	for i, u := range got {
		ids[i] = u.ID
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestRoundTripEmptyResultIsArray(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(seed().Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?dept=legal")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["users"]); got != "[]" {
		t.Fatalf(`users = %s, want []`, got)
	}
}

func TestRoundTripPopulatedIsArray(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?dept=eng", nil)
	seed().Handler().ServeHTTP(rec, req)

	body := strings.TrimSpace(rec.Body.String())
	want := `{"users":[{"id":1,"name":"alice","dept":"eng"},{"id":3,"name":"carol","dept":"eng"}]}`
	if body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
}
```

## Review

The contract holds when `List` returns a non-nil, length-zero slice for a
no-match filter and the round-trip test sees `"users":[]` on the wire. The design
point is location: the internal `matching` helper stays nil-friendly because nil
is invisible inside the package, and normalization lives once at the port
boundary where the value is about to become a wire contract. Resist the urge to
normalize in every function — that is noise that spreads a boundary concern and
still leaks. Fix it where the boundary is, prove it with a regression test, and
the API layer above never has to think about nil again.

## Resources

- [encoding/json — Marshal](https://pkg.go.dev/encoding/json#Marshal) — nil slice marshals to null, non-nil empty to [].
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — Server and ResponseRecorder used in the round-trip tests.
- [slices.Equal](https://pkg.go.dev/slices#Equal) — content comparison for the order test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-slices-clone-defensive-copy.md](04-slices-clone-defensive-copy.md)
