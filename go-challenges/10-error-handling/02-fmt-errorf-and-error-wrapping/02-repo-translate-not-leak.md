# Exercise 2: Translate sql.ErrNoRows to a domain sentinel without leaking the driver

A repository is the boundary where storage vocabulary must stop. If a bare
`sql.ErrNoRows` escapes upward, every handler that wants to return 404 ends up
importing `database/sql` and coupling itself to your storage choice. This
exercise builds a `UserRepository.FindByID` that translates the driver's
not-found sentinel into a domain `ErrUserNotFound`, annotates genuine
infrastructure failures with a domain `ErrTransient` plus the id for debugging,
and never returns the raw driver sentinel across the package boundary.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
userrepo/                      independent module: example.com/userrepo
  go.mod                       go 1.24
  userrepo.go                  ErrUserNotFound, ErrTransient sentinels; QueryFunc; UserRepository.FindByID
  userrepo_test.go             no-rows -> domain sentinel; conn error -> transient; success; leak check
  cmd/
    demo/
      main.go                  three fake query funcs: found, missing, connection-down
```

- Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
- Implement: `UserRepository` over an injected `QueryFunc`, whose `FindByID` returns `ErrUserNotFound` for a no-rows result, wraps other failures with `ErrTransient` plus the id, and returns the user on success.
- Test: no-rows returns `ErrUserNotFound` and `errors.Is(err, sql.ErrNoRows)` is false (translated, not leaked); a connection error is `errors.Is(err, ErrTransient)` with the id and `find user` in the message; success returns the user and `nil`.
- Verify: `go test -count=1 -race ./...`

### Translate, do not leak

The repository takes its query as an injected `QueryFunc` — a `func(id int)
(User, error)` — so the tests need no real database; they hand it a closure that
returns whatever error a driver would. That is the standard shape for a testable
repository seam.

`FindByID` inspects the query's error and makes a deliberate boundary decision. On
`sql.ErrNoRows` it returns `fmt.Errorf("find user %d: %w", id, ErrUserNotFound)`.
Note what is *not* there: the driver's `sql.ErrNoRows` is not in the chain. We
wrap the *domain* sentinel, so `errors.Is(err, ErrUserNotFound)` is true while
`errors.Is(err, sql.ErrNoRows)` is false. The driver sentinel has been translated
out of existence at the boundary; no caller can accidentally depend on it.

For any other failure — a dropped connection surfacing as `sql.ErrConnDone`, a
context deadline, a driver protocol error — the operation is genuinely
infrastructural and probably retryable, so we annotate with a domain `ErrTransient`
that a retry loop can branch on. Here we keep the original cause's *text* for the
log using `%v`, but not its type: `fmt.Errorf("find user %d: %w (cause: %v)", id,
ErrTransient, err)`. `errors.Is(err, ErrTransient)` is true; `errors.Is(err,
sql.ErrConnDone)` is false. The engineer reading the log still sees the driver
detail as a string, but the driver type never escaped as an inspectable value.
That is the concept-point-9 pattern: preserve the cause for debugging with `%v`,
match the domain sentinel with `%w`.

Create `userrepo.go`:

```go
package userrepo

import (
	"database/sql"
	"errors"
	"fmt"
)

// Domain sentinels. Callers branch on these, never on database/sql errors.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrTransient    = errors.New("transient database failure")
)

type User struct {
	ID   int
	Name string
}

// QueryFunc is the injected storage seam: one row lookup by id. In production it
// wraps a *sql.DB QueryRow/Scan; in tests it is a closure.
type QueryFunc func(id int) (User, error)

type UserRepository struct {
	query QueryFunc
}

func NewUserRepository(q QueryFunc) *UserRepository {
	return &UserRepository{query: q}
}

// FindByID looks up a user, translating infrastructure errors into domain
// sentinels so no database/sql type escapes this package.
func (r *UserRepository) FindByID(id int) (User, error) {
	u, err := r.query(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Translate: wrap the DOMAIN sentinel, not sql.ErrNoRows. The driver
			// sentinel is deliberately severed at this boundary.
			return User{}, fmt.Errorf("find user %d: %w", id, ErrUserNotFound)
		}
		// Genuine infra failure: match a domain sentinel with %w, keep the
		// cause's text for debugging with %v, but do not leak its type.
		return User{}, fmt.Errorf("find user %d: %w (cause: %v)", id, ErrTransient, err)
	}
	return u, nil
}
```

### The runnable demo

The demo wires three fake query funcs — a hit, a miss, and a downed connection —
and shows each translated result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql"
	"errors"
	"fmt"

	"example.com/userrepo"
)

func main() {
	found := userrepo.NewUserRepository(func(id int) (userrepo.User, error) {
		return userrepo.User{ID: id, Name: "alice"}, nil
	})
	missing := userrepo.NewUserRepository(func(id int) (userrepo.User, error) {
		return userrepo.User{}, sql.ErrNoRows
	})
	down := userrepo.NewUserRepository(func(id int) (userrepo.User, error) {
		return userrepo.User{}, sql.ErrConnDone
	})

	u, _ := found.FindByID(1)
	fmt.Printf("found: %s\n", u.Name)

	_, err := missing.FindByID(7)
	fmt.Printf("missing: %v\n", err)
	fmt.Printf("  is ErrUserNotFound=%v  leaks sql.ErrNoRows=%v\n",
		errors.Is(err, userrepo.ErrUserNotFound), errors.Is(err, sql.ErrNoRows))

	_, err = down.FindByID(42)
	fmt.Printf("down: %v\n", err)
	fmt.Printf("  is ErrTransient=%v  leaks sql.ErrConnDone=%v\n",
		errors.Is(err, userrepo.ErrTransient), errors.Is(err, sql.ErrConnDone))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: alice
missing: find user 7: user not found
  is ErrUserNotFound=true  leaks sql.ErrNoRows=false
down: find user 42: transient database failure (cause: sql: connection is already closed)
  is ErrTransient=true  leaks sql.ErrConnDone=false
```

### Tests

The tests are the whole point of this exercise: they prove translation happened
(`errors.Is` finds the domain sentinel) *and* that the driver sentinel did not
leak (`errors.Is` against the `sql` sentinel is false). Everything is asserted
through `errors.Is`, never string comparison, except a substring check that the id
and operation reached the message.

Create `userrepo_test.go`:

```go
package userrepo

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func repoReturning(err error) *UserRepository {
	return NewUserRepository(func(id int) (User, error) {
		return User{}, err
	})
}

func TestFindByIDTranslatesNoRows(t *testing.T) {
	t.Parallel()

	_, err := repoReturning(sql.ErrNoRows).FindByID(7)

	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want errors.Is ErrUserNotFound", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("sql.ErrNoRows leaked across the repository boundary")
	}
}

func TestFindByIDWrapsTransient(t *testing.T) {
	t.Parallel()

	_, err := repoReturning(sql.ErrConnDone).FindByID(42)

	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want errors.Is ErrTransient", err)
	}
	if errors.Is(err, sql.ErrConnDone) {
		t.Fatal("sql.ErrConnDone leaked across the repository boundary")
	}
	msg := err.Error()
	if !strings.Contains(msg, "find user") {
		t.Fatalf("err.Error() = %q, want the find user prefix", msg)
	}
	if !strings.Contains(msg, "42") {
		t.Fatalf("err.Error() = %q, want the id for debugging", msg)
	}
}

func TestFindByIDSuccess(t *testing.T) {
	t.Parallel()

	repo := NewUserRepository(func(id int) (User, error) {
		return User{ID: id, Name: "bob"}, nil
	})

	u, err := repo.FindByID(3)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if u.ID != 3 || u.Name != "bob" {
		t.Fatalf("user = %+v, want {3 bob}", u)
	}
}
```

## Review

The repository is correct when its returned errors speak only the domain's
vocabulary. The two leak-checks (`errors.Is(err, sql.ErrNoRows)` false,
`errors.Is(err, sql.ErrConnDone)` false) are as important as the positive
assertions: they are what a code review would look for, because the failure mode
is silent — leaking the driver type compiles fine and works until, months later, a
handler starts type-asserting `*pq.Error` and the storage layer can no longer be
swapped. Note the deliberate asymmetry between the two branches: not-found is
translated with `%w` on the domain sentinel and the driver cause fully dropped,
because a missing row is expected and carries no useful driver detail; a transient
failure keeps the cause as `%v` text because that detail matters at 03:00, but
still severs the type. If you find yourself reaching for `%w` on the driver error
"just in case a caller wants it", that is the exact instinct this exercise exists
to retrain.

## Resources

- [database/sql#ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables) — the driver not-found and `ErrConnDone` sentinels.
- [errors package](https://pkg.go.dev/errors) — `Is` traversal used for translation checks.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — `%w` versus `%v` at a boundary.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and the compatibility promise of unwrapping.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-errwrap-pipeline.md](01-errwrap-pipeline.md) | Next: [03-config-loader-w-vs-v.md](03-config-loader-w-vs-v.md)
