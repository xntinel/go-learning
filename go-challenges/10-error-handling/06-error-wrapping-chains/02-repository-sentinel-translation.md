# Exercise 2: Repository Layer: Translate Driver Errors into Domain Sentinels

The repository is the boundary where the database driver's vocabulary must stop.
If `sql.ErrNoRows` leaks past it, every layer above becomes coupled to
`database/sql`. This module builds a `UserRepository` that catches the driver's
not-found signal and re-expresses it as a domain sentinel the service layer can
depend on — while still wrapping the original for debugging.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
userrepo/                   independent module: example.com/userrepo
  go.mod                    go 1.24
  userrepo.go               ErrUserNotFound; RowScanner interface; UserRepository.FindByID
  cmd/
    demo/
      main.go               runnable demo: found and not-found lookups
  userrepo_test.go          fake scanner returning sql.ErrNoRows; translation asserted
```

Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
Implement: `FindByID` that runs a query-row scan, translates `sql.ErrNoRows` into the domain `ErrUserNotFound` wrapped with query context via `%w`, and returns the user on success.
Test: a fake scanner returning `sql.ErrNoRows` yields an error where `errors.Is(err, ErrUserNotFound)` is true and `errors.Is(err, sql.ErrNoRows)` is false — translated, not leaked — and the message carries the userID.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userrepo/cmd/demo
cd ~/go-exercises/userrepo
go mod init example.com/userrepo
go mod edit -go=1.24
```

### Why translate, and why NOT leak the original

`database/sql` reports "no row matched" by returning the sentinel `sql.ErrNoRows`
from `Row.Scan`. That is a *driver* fact. The service layer's question is a
*domain* one: "does this user exist?" If the service writes
`errors.Is(err, sql.ErrNoRows)`, then importing a Redis or Mongo backend later —
which has no such sentinel — breaks the service without a compile error. The fix is
boundary translation: the repository maps `sql.ErrNoRows` to a package-level
`ErrUserNotFound` and returns *that*. The service depends only on
`userrepo.ErrUserNotFound`.

There is a deliberate design decision in exactly how we translate. We do **not**
wrap `sql.ErrNoRows` here; we replace its identity with the domain sentinel (still
carrying the userID as context). If we wrapped the raw `sql.ErrNoRows` with `%w`,
then `errors.Is(err, sql.ErrNoRows)` would remain true up the stack and the leak
we are trying to prevent would persist. So the translation asserts a stronger
contract than the migrator in Exercise 1: `errors.Is(err, ErrUserNotFound)` is
true *and* `errors.Is(err, sql.ErrNoRows)` is false. The driver sentinel is
consumed at the boundary. (When you *do* want the original preserved for
debugging, log it at the boundary before translating, or attach it to a typed
error's unexported field that `Is` does not traverse — but never leave it
`errors.Is`-reachable in the domain error.)

To keep the module a real, testable unit without a live database, `FindByID`
depends on a tiny `RowScanner` interface — one `Scan(dest ...any) error` method,
the exact shape of `*sql.Row`. Production code passes a real `*sql.Row`; the test
passes a fake that returns `sql.ErrNoRows`. This is standard repository testing:
the boundary is an interface, the driver is one implementation.

Create `userrepo.go`:

```go
package userrepo

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrUserNotFound is the domain sentinel the service layer classifies on. It is
// deliberately independent of database/sql so upper layers never import the driver.
var ErrUserNotFound = errors.New("user not found")

// User is the domain entity.
type User struct {
	ID   string
	Name string
}

// RowScanner is the minimal slice of *sql.Row the repository needs, so tests can
// substitute a fake without a live database.
type RowScanner interface {
	Scan(dest ...any) error
}

// QueryRunner abstracts the DB handle: it returns something that scans one row.
// It is exported so callers (and demos) can supply an in-memory implementation.
type QueryRunner interface {
	QueryRow(query string, args ...any) RowScanner
}

// UserRepository reads users from a query runner, translating driver errors.
type UserRepository struct {
	db QueryRunner
}

// NewUserRepository wires a repository over a query runner.
func NewUserRepository(db QueryRunner) *UserRepository {
	return &UserRepository{db: db}
}

// FindByID loads one user. sql.ErrNoRows is translated into ErrUserNotFound and
// NOT leaked; other scan errors are wrapped with query context.
func (r *UserRepository) FindByID(id string) (*User, error) {
	row := r.db.QueryRow("SELECT id, name FROM users WHERE id = ?", id)
	var u User
	if err := row.Scan(&u.ID, &u.Name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Translate: replace the driver sentinel's identity with the domain one.
			return nil, fmt.Errorf("find user %q: %w", id, ErrUserNotFound)
		}
		// Any other driver error is wrapped verbatim (still opaque to the domain,
		// but preserved for logging/debugging one layer down).
		return nil, fmt.Errorf("find user %q: %w", id, err)
	}
	return &u, nil
}
```

### The runnable demo

The demo wires the repository over a small in-memory fake query runner so it runs
with no database, then looks up a present and an absent user.

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql"
	"errors"
	"fmt"

	"example.com/userrepo"
)

// memRow implements the one method FindByID scans through.
type memRow struct {
	id, name string
	err      error
}

func (r memRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.name
	return nil
}

// memDB is an in-memory table keyed by id.
type memDB struct{ users map[string]string }

func (d memDB) QueryRow(query string, args ...any) userrepo.RowScanner {
	id := args[0].(string)
	name, ok := d.users[id]
	if !ok {
		return memRow{err: sql.ErrNoRows}
	}
	return memRow{id: id, name: name}
}

func main() {
	repo := userrepo.NewUserRepository(memDB{users: map[string]string{"u1": "Alice"}})

	if u, err := repo.FindByID("u1"); err == nil {
		fmt.Printf("found: %s (%s)\n", u.Name, u.ID)
	}

	_, err := repo.FindByID("u2")
	fmt.Printf("errors.Is ErrUserNotFound: %v\n", errors.Is(err, userrepo.ErrUserNotFound))
	fmt.Printf("errors.Is sql.ErrNoRows:   %v\n", errors.Is(err, sql.ErrNoRows))
}
```

Note that `memDB.QueryRow` returns the exported `userrepo.RowScanner`; a method's
return type must match the interface's exactly (identical, not merely assignable),
which is why the interfaces are exported rather than private.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: Alice (u1)
errors.Is ErrUserNotFound: true
errors.Is sql.ErrNoRows:   false
```

### Tests

The fake scanner returns whatever error the test wants — `sql.ErrNoRows` for the
not-found case, `nil` (with data) for the happy path, a generic error for the
"other driver error" branch. The central assertions are the two `errors.Is` calls:
the domain sentinel is found, and the driver sentinel is *not* — proving the
translation consumed it. The message is asserted to carry the userID so an
operator can see which lookup failed.

Create `userrepo_test.go`:

```go
package userrepo

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRow returns a preset error or fills in preset data.
type fakeRow struct {
	id, name string
	err      error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.name
	return nil
}

// fakeDB returns a preset row for any query.
type fakeDB struct{ row fakeRow }

func (d fakeDB) QueryRow(query string, args ...any) RowScanner { return d.row }

func TestFindByID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		row        fakeRow
		wantErr    bool
		wantIs     error
		wantIsNot  error
		wantUser   string
		wantMsgHas string
	}{
		{
			name:     "found",
			row:      fakeRow{id: "u1", name: "Alice"},
			wantUser: "Alice",
		},
		{
			name:       "not found translates and does not leak",
			row:        fakeRow{err: sql.ErrNoRows},
			wantErr:    true,
			wantIs:     ErrUserNotFound,
			wantIsNot:  sql.ErrNoRows,
			wantMsgHas: "u42",
		},
		{
			name:    "other driver error is wrapped",
			row:     fakeRow{err: errors.New("connection reset")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := NewUserRepository(fakeDB{row: tt.row})
			u, err := repo.FindByID("u42")

			if tt.wantErr == (err == nil) {
				t.Fatalf("FindByID error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if u.Name != tt.wantUser {
					t.Fatalf("user name = %q, want %q", u.Name, tt.wantUser)
				}
				return
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(err, %v) = false, want true", tt.wantIs)
			}
			if tt.wantIsNot != nil && errors.Is(err, tt.wantIsNot) {
				t.Errorf("errors.Is(err, %v) = true, want false (driver sentinel leaked)", tt.wantIsNot)
			}
			if tt.wantMsgHas != "" && !strings.Contains(err.Error(), tt.wantMsgHas) {
				t.Errorf("error %q does not carry id %q", err.Error(), tt.wantMsgHas)
			}
		})
	}
}

func ExampleUserRepository_FindByID() {
	repo := NewUserRepository(fakeDB{row: fakeRow{err: sql.ErrNoRows}})
	_, err := repo.FindByID("u9")
	fmt.Println(errors.Is(err, ErrUserNotFound), errors.Is(err, sql.ErrNoRows))
	// Output: true false
}
```

## Review

The repository is correct when the driver sentinel dies at the boundary: after
`FindByID`, `errors.Is(err, ErrUserNotFound)` must be true and
`errors.Is(err, sql.ErrNoRows)` must be false. That asymmetry is the whole point —
it is what lets the service layer classify not-found without importing
`database/sql`. The tempting mistake is to "preserve everything" by wrapping the
raw `sql.ErrNoRows` with `%w`; that keeps it `errors.Is`-reachable and re-leaks the
coupling. Translate by replacing identity (domain sentinel + id context), and if
you need the original for debugging, log it at the boundary rather than leaving it
in the returned chain. The generic-error branch shows the other half of the rule:
errors you have no domain meaning for are wrapped verbatim so they are not silently
lost.

## Resources

- [database/sql ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables) — the driver sentinel this exercise translates.
- [errors package](https://pkg.go.dev/errors) — `Is` and `New` for domain sentinels.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and boundary translation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-http-error-to-status-mapping.md](03-http-error-to-status-mapping.md)
