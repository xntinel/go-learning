# Exercise 2: Package-Level Sentinel Errors in a Repository

A repository's error contract is an API, and the declaration form is what makes it
usable: sentinel errors must be package-level `var`s created with `errors.New` so
an HTTP handler can map "not found" to 404 and "duplicate" to 409 by *identity*
with `errors.Is`, not by matching a message string that any refactor can break.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
repo/                          independent module: example.com/repo
  go.mod                       module example.com/repo
  repo.go                      type User, UserRepo; var ErrUserNotFound, ErrDuplicateKey
  status.go                    StatusFor(err) int maps sentinels to HTTP codes
  cmd/
    demo/
      main.go                  inserts, double-inserts, gets a missing user
  repo_test.go                 errors.Is on wrapped returns; caller-side status mapping
```

- Files: `repo.go`, `status.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: an in-memory `UserRepo` whose `Get`/`Delete` return `ErrUserNotFound` and whose `Insert` returns `ErrDuplicateKey`, both wrapped with `%w`, plus `StatusFor` mapping sentinels to status codes.
- Test: `errors.Is(err, ErrUserNotFound)` after a wrapped return, `ErrDuplicateKey` on double-insert, and a caller-side switch mapping sentinels to 404/409; never compare `err.Error()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repo/cmd/demo
cd ~/go-exercises/repo
go mod init example.com/repo
```

### Why sentinels are `var`, not `const` or local

`errors.New` is a function call. A `const` may only bind a compile-time constant
expression, so a sentinel *cannot* be a `const` — it must be a `var`. And it must
be package-level: the entire value of a sentinel is that a distant caller can name
it. If `ErrUserNotFound` were a local, no handler could write
`errors.Is(err, repo.ErrUserNotFound)`, and the only way to react would be to
compare `err.Error() == "user not found"`. That string comparison is a latent bug:
it breaks silently the moment someone adds context to the message, wraps the error,
or fixes a typo in the wording. Identity, not text, is the contract.

### Why wrap with `%w`

`Get` wraps: `fmt.Errorf("get user %s: %w", id, ErrUserNotFound)`. The `%w` verb
records `ErrUserNotFound` as the wrapped error, so the returned value carries both
human context ("get user 42") and machine-matchable identity. `errors.Is` walks
the wrap chain, so `errors.Is(err, ErrUserNotFound)` is still true even though the
message now contains the user id. `errors.Join` composes several sentinels when an
operation can violate more than one invariant at once; the joined error matches
each of them with `errors.Is`.

### The caller side: mapping identity to status

`StatusFor` is the whole point made concrete. An HTTP handler does not care about
messages; it cares whether this was a missing row (404) or a uniqueness violation
(409) or something unexpected (500). It answers that with `errors.Is`, so the
mapping is robust to any wrapping the repository layer adds.

Create `repo.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors are package-level vars created with errors.New so callers can
// match them by identity with errors.Is, not by comparing message strings.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrDuplicateKey = errors.New("duplicate key")
)

type User struct {
	ID    string
	Email string
}

// UserRepo is an in-memory user store safe for concurrent use.
type UserRepo struct {
	mu    sync.RWMutex
	users map[string]User
}

func NewUserRepo() *UserRepo {
	return &UserRepo{users: make(map[string]User)}
}

// Insert adds u, returning a wrapped ErrDuplicateKey if the ID already exists.
func (r *UserRepo) Insert(u User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[u.ID]; ok {
		return fmt.Errorf("insert user %s: %w", u.ID, ErrDuplicateKey)
	}
	r.users[u.ID] = u
	return nil
}

// Get returns the user by ID or a wrapped ErrUserNotFound.
func (r *UserRepo) Get(id string) (User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return User{}, fmt.Errorf("get user %s: %w", id, ErrUserNotFound)
	}
	return u, nil
}

// Delete removes the user by ID or returns a wrapped ErrUserNotFound.
func (r *UserRepo) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[id]; !ok {
		return fmt.Errorf("delete user %s: %w", id, ErrUserNotFound)
	}
	delete(r.users, id)
	return nil
}
```

Create `status.go`:

```go
package repo

import (
	"errors"
	"net/http"
)

// StatusFor maps a repository error to an HTTP status code by identity. A handler
// uses this instead of inspecting error strings.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUserNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrDuplicateKey):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

func main() {
	r := repo.NewUserRepo()

	fmt.Println(repo.StatusFor(r.Insert(repo.User{ID: "u1", Email: "a@x.com"})))
	fmt.Println(repo.StatusFor(r.Insert(repo.User{ID: "u1", Email: "b@x.com"})))

	_, err := r.Get("missing")
	fmt.Println(repo.StatusFor(err))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200
409
404
```

The first insert succeeds (200), the second collides on `u1` (409), and getting an
absent id is a 404 — all decided by `errors.Is`, never by a string.

### Tests

The tests assert identity across the wrap boundary and the caller-side mapping.
None of them compares `err.Error()`.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestGetMissingIsNotFound(t *testing.T) {
	t.Parallel()
	r := NewUserRepo()

	_, err := r.Get("nope")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("Get miss = %v, want ErrUserNotFound", err)
	}
}

func TestDoubleInsertIsDuplicate(t *testing.T) {
	t.Parallel()
	r := NewUserRepo()

	if err := r.Insert(User{ID: "u1"}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := r.Insert(User{ID: "u1"})
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("second insert = %v, want ErrDuplicateKey", err)
	}
}

func TestDeleteMissingIsNotFound(t *testing.T) {
	t.Parallel()
	r := NewUserRepo()

	err := r.Delete("ghost")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("Delete miss = %v, want ErrUserNotFound", err)
	}
}

func TestStatusForMapsSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, http.StatusOK},
		{"not found", ErrUserNotFound, http.StatusNotFound},
		{"wrapped not found", errors.Join(errors.New("ctx"), ErrUserNotFound), http.StatusNotFound},
		{"duplicate", ErrDuplicateKey, http.StatusConflict},
		{"unknown", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StatusFor(tc.err); got != tc.want {
				t.Fatalf("StatusFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func ExampleStatusFor() {
	r := NewUserRepo()
	_, err := r.Get("absent")
	fmt.Println(StatusFor(err))
	// Output: 404
}
```

## Review

The repository is correct when its error contract is identity-based end to end:
sentinels are package-level `var`s (they cannot be `const` because `errors.New` is
a call), each failing path wraps with `%w` so context and identity coexist, and the
caller maps status codes with `errors.Is` over the wrap chain. The
`wrapped not found` test case is the proof that `%w`/`errors.Join` preserve the
match — if `StatusFor` compared strings it would return 500 there.

The mistakes to avoid: never declare a sentinel as a local or compare
`err.Error()`; never return a bare `ErrUserNotFound` when you could add context
with `%w`; and never map status by string matching. Run `go test -race` to confirm
the `sync.RWMutex` guards the map under concurrent access.

## Resources

- [errors package (New, Is, Join)](https://pkg.go.dev/errors)
- [Go Blog: Working with Errors in Go 1.13 (%w, errors.Is)](https://go.dev/blog/go1.13-errors)
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-config-loader-declaration-choices.md](01-config-loader-declaration-choices.md) | Next: [03-shadowing-in-transaction-commit.md](03-shadowing-in-transaction-commit.md)
