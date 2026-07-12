# Exercise 3: Repository — Translate sql.ErrNoRows Into a Domain Error

The repository is the boundary where storage-specific errors must stop. This
exercise builds a `GetUser` method that, when the driver reports `sql.ErrNoRows`,
returns a domain `ErrUserNotFound` instead — so no layer above the repository ever
imports `database/sql` to detect absence — while wrapping any other driver error
unchanged so it still surfaces.

This module is fully self-contained: its own `go mod init`, demo, and tests. It
uses no real database; the row source is abstracted behind a small interface.

## What you'll build

```text
repotrans/                      independent module: example.com/repotrans
  go.mod                        go 1.25
  repo.go                       ErrUserNotFound, Querier/Row seams, Repo.GetUser
  repo_test.go                  fake source returning sql.ErrNoRows / a driver error
  cmd/demo/main.go              runnable demo over an in-memory fake source
```

Files: `repo.go`, `repo_test.go`, `cmd/demo/main.go`.
Implement: `Repo.GetUser` that translates `sql.ErrNoRows` to `ErrUserNotFound` with `fmt.Errorf("getUser %s: %w", id, ...)`, and wraps other driver errors unchanged.
Test: inject `sql.ErrNoRows`, assert `errors.Is(err, ErrUserNotFound)` is true AND `errors.Is(err, sql.ErrNoRows)` is now false; assert a generic driver error is wrapped and surfaced.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/03-errors-is-and-errors-as/03-repository-driver-error-translation/cmd/demo
cd go-solutions/10-error-handling/03-errors-is-and-errors-as/03-repository-driver-error-translation
go mod edit -go=1.25
```

### The boundary that stops leaking sql sentinels

`database/sql` reports "no row matched" by returning the sentinel
`sql.ErrNoRows` from `Row.Scan`. That sentinel is a storage-layer detail. If a
repository returns it straight to the service and handler layers, every one of
those layers must import `database/sql` and check `errors.Is(err, sql.ErrNoRows)`
to mean "user does not exist" — and the day you swap `database/sql` for a
different driver, or a NoSQL store that has no such sentinel, all of that code
breaks. The repository's job is to translate: catch `sql.ErrNoRows` at this edge
and return a domain sentinel `ErrUserNotFound` that upper layers own.

The translation is deliberate about *not* preserving the driver sentinel. When
`GetUser` sees `sql.ErrNoRows`, it wraps `ErrUserNotFound` — a fresh
domain sentinel — and does *not* wrap the sql error. So above this boundary,
`errors.Is(err, ErrUserNotFound)` is true and `errors.Is(err, sql.ErrNoRows)` is
false. That is the point of the test: the driver sentinel is gone from the tree,
proving the storage detail did not leak. Every *other* driver error — a connection
failure, a constraint violation — is wrapped with `%w` and returned unchanged,
because those are genuine faults the caller may need to inspect, and there is no
domain meaning to substitute.

To keep the exercise runnable offline with no real database, the row source is an
interface: a `Querier` that returns a `Row`, and a `Row` whose `Scan` fills the
destination or returns an error. A real `*sql.DB` satisfies this shape (its
`QueryRow(...).Scan(...)` returns `sql.ErrNoRows` for a missing row); the tests and
demo supply an in-memory fake that returns whatever error the case needs. The
repository code that translates is identical either way — it only depends on the
interface and on `sql.ErrNoRows`, which is a plain package variable you can import
without opening a connection.

Create `repo.go`:

```go
package repotrans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrUserNotFound is the DOMAIN sentinel. Callers above the repository match on
// this and never import database/sql.
var ErrUserNotFound = errors.New("user not found")

type User struct {
	ID   string
	Name string
}

// Row is the seam over sql.Row: Scan fills dest or returns an error (possibly
// sql.ErrNoRows).
type Row interface {
	Scan(dest ...any) error
}

// Querier is the seam over *sql.DB: QueryRow returns a Row. A real *sql.DB's
// QueryRowContext matches this shape.
type Querier interface {
	QueryRow(ctx context.Context, query string, args ...any) Row
}

type Repo struct {
	DB Querier
}

// GetUser translates sql.ErrNoRows into the domain ErrUserNotFound and wraps any
// other driver error unchanged. Above this method, database/sql is invisible.
func (r *Repo) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	err := r.DB.QueryRow(ctx, "SELECT id, name FROM users WHERE id = ?", id).Scan(&u.ID, &u.Name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Translate: return the domain sentinel, NOT the sql one.
		return User{}, fmt.Errorf("getUser %s: %w", id, ErrUserNotFound)
	case err != nil:
		// Other driver errors are wrapped and surfaced unchanged.
		return User{}, fmt.Errorf("getUser %s: %w", id, err)
	}
	return u, nil
}
```

### The runnable demo

The demo backs the repository with a tiny in-memory `Querier` that returns
`sql.ErrNoRows` for unknown ids, and shows a caller classifying absence with the
domain sentinel alone.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"example.com/repotrans"
)

type mapRow struct {
	id, name string
	err      error
}

func (r mapRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.name
	return nil
}

type mapDB map[string]string

func (m mapDB) QueryRow(ctx context.Context, query string, args ...any) repotrans.Row {
	id := args[0].(string)
	name, ok := m[id]
	if !ok {
		return mapRow{err: sql.ErrNoRows}
	}
	return mapRow{id: id, name: name}
}

func main() {
	repo := &repotrans.Repo{DB: mapDB{"u1": "alice"}}

	u, err := repo.GetUser(context.Background(), "u1")
	fmt.Printf("found: %s (err=%v)\n", u.Name, err)

	_, err = repo.GetUser(context.Background(), "u404")
	fmt.Printf("is ErrUserNotFound: %v\n", errors.Is(err, repotrans.ErrUserNotFound))
	fmt.Printf("is sql.ErrNoRows:   %v\n", errors.Is(err, sql.ErrNoRows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: alice (err=<nil>)
is ErrUserNotFound: true
is sql.ErrNoRows:   false
```

### Tests

The tests use a fake row source. `TestNoRowsTranslated` injects `sql.ErrNoRows`
and asserts the two facts that define the boundary: the result *is*
`ErrUserNotFound` and is *not* `sql.ErrNoRows`. `TestDriverErrorSurfaced` injects
a generic driver error and asserts it is still reachable through the wrap.
`TestFound` covers the success path.

Create `repo_test.go`:

```go
package repotrans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

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

type fakeDB struct{ row fakeRow }

func (d fakeDB) QueryRow(ctx context.Context, query string, args ...any) Row {
	return d.row
}

func TestNoRowsTranslated(t *testing.T) {
	t.Parallel()
	repo := &Repo{DB: fakeDB{row: fakeRow{err: sql.ErrNoRows}}}

	_, err := repo.GetUser(context.Background(), "u404")

	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("errors.Is(err, ErrUserNotFound) = false, want true; err=%v", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("sql.ErrNoRows leaked above the repository boundary: %v", err)
	}
}

func TestDriverErrorSurfaced(t *testing.T) {
	t.Parallel()
	driverErr := errors.New("connection reset by peer")
	repo := &Repo{DB: fakeDB{row: fakeRow{err: driverErr}}}

	_, err := repo.GetUser(context.Background(), "u1")

	if errors.Is(err, ErrUserNotFound) {
		t.Fatalf("driver error wrongly translated to ErrUserNotFound: %v", err)
	}
	if !errors.Is(err, driverErr) {
		t.Fatalf("driver error not surfaced through wrap: %v", err)
	}
}

func TestFound(t *testing.T) {
	t.Parallel()
	repo := &Repo{DB: fakeDB{row: fakeRow{id: "u1", name: "alice"}}}

	u, err := repo.GetUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Name != "alice" {
		t.Fatalf("u.Name = %q, want alice", u.Name)
	}
}

func Example() {
	repo := &Repo{DB: fakeDB{row: fakeRow{err: sql.ErrNoRows}}}
	_, err := repo.GetUser(context.Background(), "x")
	fmt.Println(errors.Is(err, ErrUserNotFound), errors.Is(err, sql.ErrNoRows))
	// Output: true false
}
```

## Review

The repository is correct when the storage sentinel stops here: above `GetUser`,
`errors.Is(err, ErrUserNotFound)` answers "does the user exist?" and
`sql.ErrNoRows` is not reachable in the tree. The two-assertion test is the proof
— translating means substituting the domain sentinel, not wrapping the sql one, so
`errors.Is(err, sql.ErrNoRows)` must be false. The mistake to avoid is
`fmt.Errorf("getUser: %w", err)` on the no-rows path: that would keep `sql.ErrNoRows`
in the tree and leak it upward. Wrap `ErrUserNotFound` instead on that branch, and
reserve the wrap-the-original form for genuine driver faults, which the caller may
legitimately need to inspect. Because the row source is an interface, no database
is needed to test either branch. Run `go test -race`.

## Resources

- [database/sql: ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables) — the sentinel returned by `Row.Scan` for a missing row.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the translated domain sentinel.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and translating at boundaries.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-http-boundary-error-mapping.md](02-http-boundary-error-mapping.md) | Next: [04-validation-join-multierror.md](04-validation-join-multierror.md)
