# Exercise 3: Translating SQL Errors into Domain Errors at the Repository Boundary

The repository is the only layer that should know your storage engine. This
exercise builds a `UserRepo` whose `Get` and `Insert` translate storage errors into
domain errors at exactly one place — `sql.ErrNoRows` becomes `ErrUserNotFound`, a
unique-constraint violation becomes `ErrUserExists`, and any other driver error is
hidden behind `ErrDomain` — so the domain core never imports `database/sql`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise. It uses a fake query layer, so it
needs no real database.

## What you'll build

```text
repo-error-translation/            module example.com/repo-error-translation
  go.mod
  repo.go                          domain errors; queryer port; UserRepo.Get/Insert; translate()
  cmd/demo/main.go                 inject canned driver errors, print the translated domain errors
  repo_test.go                     sql.ErrNoRows->NotFound; unique->Exists; generic->ErrDomain, no leak
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: domain sentinels `ErrDomain`/`ErrUserNotFound`/`ErrUserExists`, a narrow `queryer` port, a `UserRepo` over it, and a `translate` function that is the single boundary where storage errors become domain errors.
- Test: inject `sql.ErrNoRows` and assert the result `errors.Is` `ErrUserNotFound`; inject a simulated unique violation and assert `ErrUserExists`; inject a generic error and assert `errors.Is` `ErrDomain` while the raw driver error does not leak as a domain category.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/13-designing-an-error-hierarchy/03-repository-error-translation/cmd/demo
cd go-solutions/10-error-handling/13-designing-an-error-hierarchy/03-repository-error-translation
```

### Translation is the boundary's whole job

The dependency rule says arrows point inward: the domain core depends on nothing,
and the outer layers depend on it. A repository that returns `sql.ErrNoRows` to its
caller violates that rule — now the caller must import `database/sql` to check for
it, and the day you swap Postgres for a document store, every caller breaks. The
fix is translation: the repository converts foreign errors into its own domain
vocabulary *once*, at the boundary it owns, and the foreign error never escapes.

`translate` is that boundary. It recognizes two storage errors and maps them to
domain categories: `sql.ErrNoRows` (checked with `errors.Is`, never `==`, because a
real driver may wrap it) becomes `ErrUserNotFound`, and a unique-constraint
violation — here a package-private `errUniqueViolation` standing in for a
`*pq.Error` with SQLSTATE `23505` — becomes `ErrUserExists`. Everything else is the
interesting case. An unrecognized driver error (a dial failure, a broken pipe) must
still become a domain error so the caller can handle it, but its *identity* must not
leak: the caller should not be able to `errors.Is` the result against
`sql.ErrNoRows` or against the driver's own sentinels. So the default branch wraps
`ErrDomain` with `%w` (keeping only `ErrDomain` on the category chain) and renders
the driver text with `%v` (preserving it for the log message but keeping it out of
the `Is` walk). The result answers `errors.Is(err, ErrDomain) == true` and
`errors.Is(err, sql.ErrNoRows) == false`, which is exactly the containment you want.

The `queryer` port is deliberately narrow — two methods — so the test can supply a
fake that returns canned errors and no real database is involved. In production a
`*sql.DB`-backed adapter implements the same port.

Create `repo.go`:

```go
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Domain vocabulary. Callers of the repo branch on these, never on driver types.
var (
	ErrDomain       = errors.New("domain error")
	ErrUserNotFound = fmt.Errorf("user: not found: %w", ErrDomain)
	ErrUserExists   = fmt.Errorf("user: already exists: %w", ErrDomain)
)

// User is the domain model. It has no storage-specific fields.
type User struct {
	ID    string
	Email string
}

// errUniqueViolation stands in for a driver-specific unique-constraint error
// (for example a *pq.Error with Code "23505"). It never escapes this package.
var errUniqueViolation = errors.New("pq: duplicate key value violates unique constraint")

// queryer is the narrow storage port the repo depends on. A real one is backed
// by *sql.DB; the test injects a fake that returns canned driver errors.
type queryer interface {
	QueryUser(ctx context.Context, id string) (*User, error)
	InsertUser(ctx context.Context, u *User) error
}

type UserRepo struct {
	db queryer
}

func NewUserRepo(db queryer) *UserRepo { return &UserRepo{db: db} }

// Get translates storage errors into domain errors at this single boundary,
// so no caller ever imports database/sql or the driver.
func (r *UserRepo) Get(ctx context.Context, id string) (*User, error) {
	u, err := r.db.QueryUser(ctx, id)
	if err != nil {
		return nil, translate("get", err)
	}
	return u, nil
}

func (r *UserRepo) Insert(ctx context.Context, u *User) error {
	if err := r.db.InsertUser(ctx, u); err != nil {
		return translate("insert", err)
	}
	return nil
}

// translate is the only place that knows storage-specific errors. Recognized
// ones become domain categories; anything else is hidden behind ErrDomain with
// the driver text kept for the message but kept OUT of the Is-chain.
func translate(op string, err error) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%s: %w", op, ErrUserNotFound)
	case errors.Is(err, errUniqueViolation):
		return fmt.Errorf("%s: %w", op, ErrUserExists)
	default:
		// %v renders the driver text for logs; %w keeps only ErrDomain in the
		// category chain, so the raw driver error is not exposed as a category.
		return fmt.Errorf("%s: unexpected storage error: %v: %w", op, err, ErrDomain)
	}
}
```

### The runnable demo

The demo injects two canned driver errors — `sql.ErrNoRows` and a dial failure —
and prints the translated domain errors, showing that the not-found maps to the
category and the generic error is contained by `ErrDomain` without leaking
`sql.ErrNoRows`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"example.com/repo-error-translation"
)

// fakeDB returns a preset error to show translation without a real database.
type fakeDB struct{ err error }

func (f fakeDB) QueryUser(context.Context, string) (*repo.User, error) {
	return nil, f.err
}
func (f fakeDB) InsertUser(context.Context, *repo.User) error { return f.err }

func main() {
	ctx := context.Background()

	r := repo.NewUserRepo(fakeDB{err: sql.ErrNoRows})
	_, err := r.Get(ctx, "u1")
	fmt.Printf("no rows -> %v\n", err)
	fmt.Printf("  Is ErrUserNotFound=%v\n", errors.Is(err, repo.ErrUserNotFound))

	r2 := repo.NewUserRepo(fakeDB{err: errors.New("dial tcp: connection refused")})
	err2 := r2.Insert(ctx, &repo.User{ID: "u2"})
	fmt.Printf("driver error -> %v\n", err2)
	fmt.Printf("  Is ErrDomain=%v leaks sql.ErrNoRows=%v\n",
		errors.Is(err2, repo.ErrDomain), errors.Is(err2, sql.ErrNoRows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
no rows -> get: user: not found: domain error
  Is ErrUserNotFound=true
driver error -> insert: unexpected storage error: dial tcp: connection refused: domain error
  Is ErrDomain=true leaks sql.ErrNoRows=false
```

### Tests

The tests inject each class of driver error through the fake and assert the
translation. The containment test is the important one: a generic driver error must
become `ErrDomain` *and* must not be `errors.Is`-equal to `sql.ErrNoRows` or the
unique-violation sentinel, proving the raw error stays inside the repository.

Create `repo_test.go`:

```go
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

type fakeDB struct {
	user *User
	err  error
}

func (f fakeDB) QueryUser(context.Context, string) (*User, error) {
	return f.user, f.err
}
func (f fakeDB) InsertUser(context.Context, *User) error { return f.err }

func TestGetTranslatesNoRows(t *testing.T) {
	t.Parallel()
	r := NewUserRepo(fakeDB{err: sql.ErrNoRows})
	_, err := r.Get(context.Background(), "u1")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v; want errors.Is ErrUserNotFound", err)
	}
}

func TestInsertTranslatesUniqueViolation(t *testing.T) {
	t.Parallel()
	r := NewUserRepo(fakeDB{err: errUniqueViolation})
	err := r.Insert(context.Background(), &User{ID: "u1"})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("err = %v; want errors.Is ErrUserExists", err)
	}
}

func TestGenericDriverErrorIsContained(t *testing.T) {
	t.Parallel()
	driverErr := errors.New("write: broken pipe")
	r := NewUserRepo(fakeDB{err: driverErr})
	_, err := r.Get(context.Background(), "u1")

	if !errors.Is(err, ErrDomain) {
		t.Fatalf("err = %v; want errors.Is ErrDomain", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Error("generic driver error leaked as sql.ErrNoRows category")
	}
	if errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrUserExists) {
		t.Error("generic driver error leaked as a specific user category")
	}
}

func TestSuccessReturnsUser(t *testing.T) {
	t.Parallel()
	r := NewUserRepo(fakeDB{user: &User{ID: "u1", Email: "a@x.com"}})
	u, err := r.Get(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.ID != "u1" {
		t.Fatalf("u.ID = %q; want u1", u.ID)
	}
}

func ExampleUserRepo_Get() {
	r := NewUserRepo(fakeDB{err: sql.ErrNoRows})
	_, err := r.Get(context.Background(), "u1")
	fmt.Println(errors.Is(err, ErrUserNotFound))
	// Output: true
}
```

## Review

The boundary is correct when the domain core could compile with `database/sql`
removed from its imports: only `repo.go` names it, and only `translate` maps its
errors. The containment test is the one that proves the discipline held — a generic
driver error must satisfy `errors.Is(err, ErrDomain)` yet fail
`errors.Is(err, sql.ErrNoRows)`, which is what the `%v`-for-message,
`%w`-for-category split buys you. The classic mistake this exercise inoculates
against is `if err == sql.ErrNoRows`: it looks fine until a driver or a
connection-pool layer wraps the error, at which point the `==` silently stops
matching and every "not found" becomes a 500. Always translate with `errors.Is`,
and keep the driver's vocabulary inside the repository package where it belongs.

## Resources

- [`database/sql` package variables](https://pkg.go.dev/database/sql#pkg-variables) — `sql.ErrNoRows` and friends.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — why `errors.Is(err, sql.ErrNoRows)` survives wrapping and `==` does not.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and translating at boundaries.

---

Back to [02-typed-domain-error-with-is.md](02-typed-domain-error-with-is.md) | Next: [04-http-problem-details-mapping.md](04-http-problem-details-mapping.md)
