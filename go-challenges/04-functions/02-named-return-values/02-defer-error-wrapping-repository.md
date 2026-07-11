# Exercise 2: Wrap a Repository Error Once in a Deferred Closure

A repository method has several failure exits — bad input, a cancelled context, a
not-found from the store. Every one of them should reach the caller with the same
operation context in front of it, and you should write that decoration exactly
once. This is the single most valuable production use of named returns: a deferred
closure that wraps whatever non-nil `err` the function is about to return.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
userrepo/                    independent module: example.com/userrepo
  go.mod
  userrepo.go                User; Store; Repository.FindUser (one deferred wrapper)
  cmd/demo/
    main.go                  runnable demo: found user, wrapped not-found
  userrepo_test.go           asserts prefix + errors.Is chain, success leaves err nil
```

- Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
- Implement: `FindUser(ctx, id) (User, error)` whose one deferred closure decorates any non-nil named `err` with the operation and id via `fmt.Errorf("... %w", err)`.
- Test: a fake store returning a sentinel `ErrNotFound`; assert the message carries the `FindUser "id"` prefix AND `errors.Is` still matches the sentinel; assert the success path leaves `err` nil and unwrapped.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userrepo/cmd/demo
cd ~/go-exercises/userrepo
go mod init example.com/userrepo
```

### One decoration, every exit

`FindUser` has three return sites: empty id, cancelled context, and the store
result. Without named returns you would write `fmt.Errorf("FindUser %q: %w", id,
err)` at each one, and the fourth return site someone adds next quarter would leak
an undecorated error. Instead, register one deferred closure:

```go
defer func() {
	if err != nil {
		err = fmt.Errorf("FindUser %q: %w", id, err)
	}
}()
```

Because `err` is a named result, the closure runs *after* the `return` statement
has copied the returned error into `err`, sees whether it is non-nil, and rewrites
it in place before the caller receives it. The `%w` verb is load-bearing: it keeps
the underlying error reachable, so a caller can still write `errors.Is(err,
ErrNotFound)` even though the message now reads `FindUser "42": user not found`.
The `if err != nil` guard means the success path is untouched — a found user
returns a nil error, not a wrapped nil.

One discipline to notice: the context-cancellation guard uses a distinctly named
local (`cerr`) rather than `err`, so it cannot shadow the named result. Shadowing
`err` in an inner scope is the failure mode this whole lesson circles back to;
Exercise 7 reproduces it deliberately.

Create `userrepo.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned (wrapped) when no user has the given id.
var ErrNotFound = errors.New("user not found")

// User is the record the repository returns.
type User struct {
	ID   string
	Name string
}

// Store is the backing data source. Production wires a database; a test wires a
// fake.
type Store interface {
	Load(ctx context.Context, id string) (User, error)
}

// Repository loads users through a Store, decorating every failure uniformly.
type Repository struct {
	store Store
}

// New builds a Repository over the given store.
func New(store Store) *Repository {
	return &Repository{store: store}
}

// FindUser loads a user by id. A single deferred closure wraps any non-nil result
// error with the operation name and id, so every failure exit carries the same
// context without repeating fmt.Errorf at each return. %w preserves the chain, so
// errors.Is(err, ErrNotFound) still matches through the wrapper.
func (r *Repository) FindUser(ctx context.Context, id string) (u User, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("FindUser %q: %w", id, err)
		}
	}()

	if id == "" {
		return User{}, errors.New("empty id")
	}
	if cerr := ctx.Err(); cerr != nil {
		return User{}, cerr
	}
	return r.store.Load(ctx, id)
}
```

### The runnable demo

The demo wires a tiny in-memory store, finds a user that exists, then asks for one
that does not so you can see the wrapped error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/userrepo"
)

type memStore map[string]userrepo.User

func (m memStore) Load(_ context.Context, id string) (userrepo.User, error) {
	u, ok := m[id]
	if !ok {
		return userrepo.User{}, userrepo.ErrNotFound
	}
	return u, nil
}

func main() {
	repo := userrepo.New(memStore{"42": {ID: "42", Name: "Ada"}})
	ctx := context.Background()

	u, err := repo.FindUser(ctx, "42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("found: %+v\n", u)

	_, err = repo.FindUser(ctx, "99")
	fmt.Println("error:", err)
	fmt.Println("is not-found:", errors.Is(err, userrepo.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: {ID:42 Name:Ada}
error: FindUser "99": user not found
is not-found: true
```

### Tests

The tests assert the two properties that make the pattern correct: the wrapper
prefix is present on failure, and `%w` keeps the sentinel matchable. A separate
case proves the success path is left entirely alone.

Create `userrepo_test.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeStore struct {
	user User
	err  error
}

func (f fakeStore) Load(_ context.Context, _ string) (User, error) {
	return f.user, f.err
}

func TestFindUserWrapsAndPreservesChain(t *testing.T) {
	t.Parallel()

	repo := New(fakeStore{err: ErrNotFound})
	_, err := repo.FindUser(context.Background(), "42")
	if err == nil {
		t.Fatal("FindUser: want error, got nil")
	}
	if !strings.Contains(err.Error(), `FindUser "42"`) {
		t.Fatalf("error %q missing operation+id prefix", err.Error())
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error %q lost the ErrNotFound chain", err.Error())
	}
}

func TestFindUserSuccessLeavesErrNil(t *testing.T) {
	t.Parallel()

	repo := New(fakeStore{user: User{ID: "42", Name: "Ada"}})
	u, err := repo.FindUser(context.Background(), "42")
	if err != nil {
		t.Fatalf("FindUser: unexpected error: %v", err)
	}
	if u.Name != "Ada" {
		t.Fatalf("FindUser = %+v, want Ada", u)
	}
}

func TestFindUserRejectsEmptyID(t *testing.T) {
	t.Parallel()

	repo := New(fakeStore{user: User{ID: "42"}})
	_, err := repo.FindUser(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Fatalf("FindUser(\"\") err = %v, want wrapped empty id", err)
	}
}

func TestFindUserWrapsCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	repo := New(fakeStore{user: User{ID: "42"}})
	_, err := repo.FindUser(ctx, "42")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FindUser err = %v, want context.Canceled in chain", err)
	}
}

func ExampleRepository_FindUser() {
	repo := New(fakeStore{err: ErrNotFound})
	_, err := repo.FindUser(context.Background(), "7")
	fmt.Println(err)
	// Output: FindUser "7": user not found
}
```

## Review

The wrapper is correct when every non-nil exit gains the `FindUser "id"` prefix
and none of them loses the underlying error to `errors.Is`, while the success path
returns a clean nil. The two ways to break it are both worth internalizing: drop
the `%w` (use `%v` or `%s`) and the sentinel chain is severed, so
`errors.Is(err, ErrNotFound)` starts returning false; or forget the `if err != nil`
guard and the closure wraps a nil into a non-nil `fmt.Errorf`, turning every
success into a spurious failure. The context guard uses `cerr`, not `err`, on
purpose — an inner `err :=` would shadow the named result and, on a naked-return
path, silently defeat the very wrapper this exercise is about. Run `go test -race`.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`fmt.Errorf` and the `%w` verb](https://pkg.go.dev/fmt#Errorf)
- [`errors.Is`](https://pkg.go.dev/errors#Is)
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-header-parser-guard-clauses.md](01-header-parser-guard-clauses.md) | Next: [03-tx-commit-rollback-boundary.md](03-tx-commit-rollback-boundary.md)
