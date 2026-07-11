# Exercise 5: Export the Interface, Hide the Implementation

The most durable public surface for a data-access layer is a small interface plus a
constructor that returns it, with the concrete struct kept unexported. Callers depend
on `UserRepository`, never on the concrete type, so you can change fields, swap the
backend, or add caching without a breaking release. This exercise builds that layer,
proves the concrete type never escapes the package, and shows the interface is the
seam by dropping in a second implementation in the test.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
userrepo/                  independent module: example.com/userrepo
  go.mod                   go 1.26
  userrepo.go              UserRepository interface, NewUserRepository, unexported sqlUserRepository
  cmd/
    demo/
      main.go              constructs via NewUserRepository, uses the interface
  userrepo_test.go         package userrepo_test: binds to the interface, drops in a fake
```

- Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
- Implement: an exported `UserRepository` interface and `NewUserRepository` constructor returning it, with an unexported concrete `sqlUserRepository` whose fields are unexported; `context.Context` first param; exported sentinel errors.
- Test: a black-box test that constructs via `NewUserRepository`, asserts the returned value satisfies `UserRepository` at compile time, and defines a second in-memory implementation to prove call sites bind to the interface, not the concrete type.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userrepo/cmd/demo
cd ~/go-exercises/userrepo
go mod init example.com/userrepo
go mod edit -go=1.26
```

### Why the interface is exported and the struct is not

A repository is a boundary: business logic above it should not know or care whether
users live in Postgres, DynamoDB, or a map. If the constructor returns the concrete
`*sqlUserRepository`, then every caller is coupled to that type, its fields become part
of what people reach into, and swapping the backend, or even renaming a field, is a
breaking change. If instead the constructor returns a small `UserRepository` interface,
the concrete type never escapes the package: callers hold a `UserRepository`, so you are
free to change the struct's fields, wrap it in a caching decorator, or replace it with a
different backend entirely, and no downstream code needs to change or recompile against a
new type name.

This also inverts the dependency. Because call sites depend on the interface, their tests
can substitute an in-memory fake without a database, which is precisely what the test
below does: it defines a `fakeRepository` in the test package, and the same business code
that runs against `sqlUserRepository` in production runs against the fake in tests. The
interface is the seam.

Keep the interface small. A two-method `UserRepository` is easy to implement, easy to
fake, and easy to keep stable; a fat interface with fifteen methods is as hard to change
as an exported struct. The concrete type here is backed by an in-memory map so the
exercise is self-contained; the same pattern holds verbatim when the unexported field is
a `*sql.DB` and the methods run queries, the point is that the field, whatever it is,
stays unexported.

Create `userrepo.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrNotFound = errors.New("userrepo: user not found")
	ErrConflict = errors.New("userrepo: user already exists")
)

// User is the exported domain value the repository stores and returns.
type User struct {
	ID    string
	Email string
}

// UserRepository is the exported surface: a small interface callers depend on.
// The concrete implementation is hidden, so backends can change without breaking
// callers.
type UserRepository interface {
	Create(ctx context.Context, u User) error
	FindByID(ctx context.Context, id string) (User, error)
}

// sqlUserRepository is unexported: callers can hold it only as a UserRepository,
// never name it. Its field (here an in-memory map; in production a *sql.DB) is
// unexported too.
type sqlUserRepository struct {
	mu    sync.Mutex
	users map[string]User
}

// NewUserRepository returns the interface, not the concrete type. This is the
// only construction path and the only thing callers name.
func NewUserRepository() UserRepository {
	return &sqlUserRepository{users: make(map[string]User)}
}

func (r *sqlUserRepository) Create(ctx context.Context, u User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.users[u.ID]; exists {
		return ErrConflict
	}
	r.users[u.ID] = u
	return nil
}

func (r *sqlUserRepository) FindByID(ctx context.Context, id string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}
```

### The runnable demo

The demo constructs through `NewUserRepository` and holds the result as a
`UserRepository`, never naming the concrete type. It creates a user, reads it back, and
shows the conflict and not-found paths.

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
	var repo userrepo.UserRepository = userrepo.NewUserRepository()

	_ = repo.Create(ctx, userrepo.User{ID: "u1", Email: "ada@example.com"})

	if u, err := repo.FindByID(ctx, "u1"); err == nil {
		fmt.Printf("found: %s <%s>\n", u.ID, u.Email)
	}

	if err := repo.Create(ctx, userrepo.User{ID: "u1", Email: "dup@example.com"}); errors.Is(err, userrepo.ErrConflict) {
		fmt.Println("create u1 again: conflict")
	}

	if _, err := repo.FindByID(ctx, "missing"); errors.Is(err, userrepo.ErrNotFound) {
		fmt.Println("find missing: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: u1 <ada@example.com>
create u1 again: conflict
find missing: not found
```

### Tests

The black-box test proves three things. `var _ userrepo.UserRepository = userrepo.NewUserRepository()`
is a compile-time assertion that the constructor's return value satisfies the interface.
`registerUser` is a tiny piece of "business logic" that takes the interface, and the test
runs it against both the real repository and a hand-written `fakeRepository`, proving call
sites bind to the interface rather than the concrete type. The commented line records that
the concrete type cannot be named from outside the package.

Create `userrepo_test.go`:

```go
package userrepo_test

import (
	"context"
	"errors"
	"testing"

	"example.com/userrepo"
)

// Compile-time proof that the constructor returns something satisfying the
// exported interface.
var _ userrepo.UserRepository = userrepo.NewUserRepository()

// registerUser is business logic that depends only on the interface. It runs
// unchanged against the real repo and against a fake.
func registerUser(ctx context.Context, repo userrepo.UserRepository, u userrepo.User) error {
	if _, err := repo.FindByID(ctx, u.ID); err == nil {
		return userrepo.ErrConflict
	}
	return repo.Create(ctx, u)
}

// fakeRepository is a second implementation living in the test package. Its
// existence proves the seam is the interface: registerUser cannot tell it apart
// from sqlUserRepository.
type fakeRepository struct {
	users map[string]userrepo.User
}

func newFake() *fakeRepository {
	return &fakeRepository{users: make(map[string]userrepo.User)}
}

func (f *fakeRepository) Create(ctx context.Context, u userrepo.User) error {
	if _, ok := f.users[u.ID]; ok {
		return userrepo.ErrConflict
	}
	f.users[u.ID] = u
	return nil
}

func (f *fakeRepository) FindByID(ctx context.Context, id string) (userrepo.User, error) {
	u, ok := f.users[id]
	if !ok {
		return userrepo.User{}, userrepo.ErrNotFound
	}
	return u, nil
}

func TestRegisterAgainstRealAndFake(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repos := map[string]userrepo.UserRepository{
		"real": userrepo.NewUserRepository(),
		"fake": newFake(),
	}

	for name, repo := range repos {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			u := userrepo.User{ID: "u1", Email: "e@x.com"}
			if err := registerUser(ctx, repo, u); err != nil {
				t.Fatalf("first register: %v", err)
			}
			if err := registerUser(ctx, repo, u); !errors.Is(err, userrepo.ErrConflict) {
				t.Fatalf("second register err = %v, want ErrConflict", err)
			}
			got, err := repo.FindByID(ctx, "u1")
			if err != nil {
				t.Fatalf("FindByID: %v", err)
			}
			if got.Email != "e@x.com" {
				t.Fatalf("email = %q, want e@x.com", got.Email)
			}
		})
	}
}

func TestFindByIDNotFound(t *testing.T) {
	t.Parallel()

	repo := userrepo.NewUserRepository()
	if _, err := repo.FindByID(context.Background(), "nope"); !errors.Is(err, userrepo.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The concrete type cannot be named from outside the package:
//
//	var r userrepo.sqlUserRepository   // undefined: userrepo.sqlUserRepository
//
// Callers can only hold a UserRepository, which is the whole point.
```

## Review

The design is correct when the constructor's return type is the interface (not
`*sqlUserRepository`), when the compile-time `var _` assertion holds, and when
`registerUser` runs identically against the real repository and the fake, proving nothing
in the call path is coupled to the concrete type. The value of hiding the implementation
is future freedom: with the struct unexported you can change its fields, replace the map
with a `*sql.DB`, or wrap it in a caching layer, and no caller recompiles against a new
type. The one discipline to hold is interface size, a small two-method interface stays
stable and is trivial to fake, while a fat one becomes as rigid as an exported struct.
The commented `sqlUserRepository` line records the compiler's `undefined` error, making
the boundary executable knowledge rather than a comment.

## Resources

- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — small interfaces and returning them from constructors.
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — why the consumer, not the producer, usually owns the interface, and returning concrete vs. interface.
- [`context.Context`](https://pkg.go.dev/context) — the first-parameter convention the repository methods follow.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the exported sentinels across implementations.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-export-test-seam.md](04-export-test-seam.md) | Next: [06-functional-options-constructor.md](06-functional-options-constructor.md)
