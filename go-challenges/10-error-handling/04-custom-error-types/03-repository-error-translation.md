# Exercise 3: Translating Driver Errors into a Typed RepositoryError

`sql.ErrNoRows` must never escape the repository. This module builds a repository
`GetUser` that wraps low-level driver failures into a `*RepositoryError{Op, Entity,
Kind, Err}` exposing a domain `Kind` (NotFound / Conflict / Transient) while still
wrapping the original so callers can `errors.Is` the underlying sentinel. This is
the classic "don't leak the driver past the repo boundary" pattern.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
userrepo/                  independent module: example.com/userrepo
  go.mod                   go 1.24
  userrepo.go              RepositoryError+Kind; Repository.GetUser; fake driver err
  cmd/
    demo/
      main.go              runs GetUser against a stub returning sql.ErrNoRows
  userrepo_test.go         Is(sql.ErrNoRows) + As Kind==NotFound/Conflict, nil path
```

Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
Implement: a `Kind` enum, `*RepositoryError` with `Error()`/`Unwrap()`, a `store` interface, and `Repository.GetUser` that translates `sql.ErrNoRows` and a constraint error into a `Kind`.
Test: inject `sql.ErrNoRows` and assert both `errors.Is(sql.ErrNoRows)` and `errors.As` Kind==NotFound; inject a constraint error, assert Kind==Conflict; nil path returns nil.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userrepo/cmd/demo
cd ~/go-exercises/userrepo
go mod init example.com/userrepo
go mod edit -go=1.24
```

### Translate at the boundary, but keep the chain

The repository is the seam between the database driver and the domain. Above it,
the rest of the service should reason in domain terms: "the user was not found",
"there was a uniqueness conflict", "the store is temporarily unavailable, a retry
might help". Below it live driver facts: `sql.ErrNoRows`, a Postgres SQLSTATE like
`23505` for a unique violation, a connection-reset. `GetUser` reads the driver
error and *classifies* it into a `Kind`, packaging `Op` ("GetUser") and `Entity`
("user") for context.

The subtlety that makes this pattern correct rather than lossy: `RepositoryError`
wraps the original driver error with `%w` via its `Unwrap()`. So a caller that
still wants to ask the low-level question — `errors.Is(err, sql.ErrNoRows)` — can,
because the chain is intact, while a caller that wants the domain view uses
`errors.As` to read `Kind`. You translate *and* preserve; you do not translate by
throwing the cause away. If you wrapped with `%v` (or stored only a formatted
string), the `errors.Is(sql.ErrNoRows)` check downstream would break — and that is
the exact regression this module's test guards against.

### The fake driver constraint error

Real code would import a driver package and match its error type (e.g.
`*pq.Error` with `Code == "23505"`, or `*mysql.MySQLError`). To keep the module
hermetic and offline, we model that with a small `driverError{Code string}` type;
`GetUser` matches `Code == "23505"` the same way you would match a real driver
code. The classification logic is identical; only the concrete driver type is
stubbed.

Create `userrepo.go`:

```go
// Package userrepo is a repository layer that translates database driver errors
// into a typed, domain-classified *RepositoryError while preserving the original
// error in the chain.
package userrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Kind is the domain classification of a repository failure. The rest of the
// service switches on Kind and never on driver-specific errors.
type Kind int

const (
	KindUnknown Kind = iota
	KindNotFound
	KindConflict
	KindTransient
)

func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// RepositoryError is the typed error the repository returns. Op and Entity give
// context; Kind is the domain category; Err is the wrapped driver cause, kept so
// callers can still errors.Is the low-level sentinel.
type RepositoryError struct {
	Op     string
	Entity string
	Kind   Kind
	Err    error
}

func (e *RepositoryError) Error() string {
	return fmt.Sprintf("%s %s: %s: %v", e.Op, e.Entity, e.Kind, e.Err)
}

// Unwrap keeps the driver error reachable: errors.Is(err, sql.ErrNoRows) works.
func (e *RepositoryError) Unwrap() error { return e.Err }

// driverError models a driver-specific constraint error (like a Postgres
// SQLSTATE). Real code would match the driver's own error type.
type driverError struct {
	Code string
	Msg  string
}

func (e *driverError) Error() string { return fmt.Sprintf("driver %s: %s", e.Code, e.Msg) }

// store is the low-level dependency: it speaks in driver errors. A real
// implementation wraps *sql.DB; the test injects a stub.
type store interface {
	QueryUser(ctx context.Context, id int) (User, error)
}

// User is the domain entity.
type User struct {
	ID   int
	Name string
}

// Repository is the domain-facing data access layer.
type Repository struct {
	store store
}

func NewRepository(s store) *Repository { return &Repository{store: s} }

// GetUser fetches a user and translates any driver error into a classified
// *RepositoryError, wrapping the original so the chain stays searchable.
func (r *Repository) GetUser(ctx context.Context, id int) (User, error) {
	u, err := r.store.QueryUser(ctx, id)
	if err == nil {
		return u, nil
	}

	kind := classify(err)
	return User{}, &RepositoryError{Op: "GetUser", Entity: "user", Kind: kind, Err: err}
}

// classify maps a driver error to a domain Kind. This is the whole translation:
// driver facts in, domain category out.
func classify(err error) Kind {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return KindNotFound
	case isUniqueViolation(err):
		return KindConflict
	case isTransient(err):
		return KindTransient
	default:
		return KindUnknown
	}
}

func isUniqueViolation(err error) bool {
	var de *driverError
	return errors.As(err, &de) && de.Code == "23505"
}

func isTransient(err error) bool {
	var de *driverError
	// 08006 = connection failure in SQLSTATE class 08.
	return errors.As(err, &de) && de.Code == "08006"
}

// NewConstraintError is a test/demo helper producing a unique-violation driver
// error, as a driver package would.
func NewConstraintError(msg string) error { return &driverError{Code: "23505", Msg: msg} }

// NewConnFailure is a test/demo helper producing a transient connection error.
func NewConnFailure(msg string) error { return &driverError{Code: "08006", Msg: msg} }
```

### The runnable demo

The demo wires a stub store that returns `sql.ErrNoRows`, calls `GetUser`, and
shows both views of the same error: the domain kind and the still-reachable driver
sentinel.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"example.com/userrepo"
)

type missingStore struct{}

func (missingStore) QueryUser(ctx context.Context, id int) (userrepo.User, error) {
	return userrepo.User{}, sql.ErrNoRows
}

func main() {
	repo := userrepo.NewRepository(missingStore{})

	_, err := repo.GetUser(context.Background(), 42)

	var re *userrepo.RepositoryError
	if errors.As(err, &re) {
		fmt.Printf("domain kind: %s (op=%s entity=%s)\n", re.Kind, re.Op, re.Entity)
	}
	fmt.Printf("still errors.Is sql.ErrNoRows: %v\n", errors.Is(err, sql.ErrNoRows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
domain kind: not_found (op=GetUser entity=user)
still errors.Is sql.ErrNoRows: true
```

### Tests

The table injects each driver failure through a stub store and asserts the
resulting `Kind`, then the not-found case additionally asserts the driver sentinel
is still reachable — the "translate but preserve" contract. The nil path asserts a
found user returns no error.

Create `userrepo_test.go`:

```go
package userrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// stubStore returns a fixed (user, err) pair, standing in for the driver.
type stubStore struct {
	user User
	err  error
}

func (s stubStore) QueryUser(ctx context.Context, id int) (User, error) {
	return s.user, s.err
}

func TestGetUserClassifies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		storeErr error
		wantKind Kind
	}{
		{"no rows", sql.ErrNoRows, KindNotFound},
		{"unique violation", NewConstraintError("dup email"), KindConflict},
		{"conn failure", NewConnFailure("reset by peer"), KindTransient},
		{"unknown", errors.New("weird"), KindUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := NewRepository(stubStore{err: tc.storeErr})

			_, err := repo.GetUser(context.Background(), 1)

			var re *RepositoryError
			if !errors.As(err, &re) {
				t.Fatalf("errors.As failed to extract *RepositoryError from %v", err)
			}
			if re.Kind != tc.wantKind {
				t.Errorf("Kind = %s; want %s", re.Kind, tc.wantKind)
			}
			if re.Op != "GetUser" || re.Entity != "user" {
				t.Errorf("Op/Entity = %s/%s; want GetUser/user", re.Op, re.Entity)
			}
		})
	}
}

func TestNotFoundStillMatchesDriverSentinel(t *testing.T) {
	t.Parallel()
	repo := NewRepository(stubStore{err: sql.ErrNoRows})

	_, err := repo.GetUser(context.Background(), 1)

	if !errors.Is(err, sql.ErrNoRows) {
		t.Error("translated error must still errors.Is(sql.ErrNoRows)")
	}
	var re *RepositoryError
	if !errors.As(err, &re) || re.Kind != KindNotFound {
		t.Error("translated error must also expose Kind==KindNotFound")
	}
}

func TestGetUserSuccess(t *testing.T) {
	t.Parallel()
	repo := NewRepository(stubStore{user: User{ID: 7, Name: "ada"}})

	u, err := repo.GetUser(context.Background(), 7)
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if u.Name != "ada" {
		t.Errorf("Name = %q; want ada", u.Name)
	}
}

func ExampleRepository_GetUser() {
	repo := NewRepository(stubStore{err: sql.ErrNoRows})
	_, err := repo.GetUser(context.Background(), 1)
	var re *RepositoryError
	errors.As(err, &re)
	// Both views hold at once: domain kind and driver sentinel.
	fmt.Println(re.Kind, errors.Is(err, sql.ErrNoRows))
	// Output: not_found true
}
```

## Review

The repository is correct when it presents two consistent views of one failure:
`errors.As` yields a `*RepositoryError` with the right domain `Kind`, `Op`, and
`Entity`, and `errors.Is` against the driver sentinel still succeeds because the
cause is wrapped with `%w` via `Unwrap`. `TestNotFoundStillMatchesDriverSentinel`
is the guard against the most common regression — someone "cleaning up" the error
by formatting it into a string, which silently severs the chain and breaks every
`errors.Is(sql.ErrNoRows)` above. The classification lives entirely in the repo, so
the handler in Exercise 4 can switch on `Kind` without ever importing
`database/sql`. Run `go test -race` to confirm.

## Resources

- [database/sql: ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables) — the sentinel every repository must translate, not leak.
- [errors package](https://pkg.go.dev/errors) — `Is`/`As`/`Unwrap` traversal used for classification.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping with `%w` to keep a cause reachable.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-pointer-vs-value-error-identity.md](02-pointer-vs-value-error-identity.md) | Next: [04-http-apierror-status-mapping.md](04-http-apierror-status-mapping.md)
