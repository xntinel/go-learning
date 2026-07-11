# Exercise 4: A Repository Over database/sql That Maps sql.ErrNoRows to a Domain Sentinel

Now the same patterns against a real SQL engine. This exercise builds a
`UserRepo` over `*sql.DB` using the pure-Go `modernc.org/sqlite` driver (no cgo),
and does the one translation every production repository owes its callers: turn
the `database/sql` sentinel `sql.ErrNoRows` into a domain `ErrNotFound` at the
boundary, so nothing above the repository ever learns SQL exists underneath.

## What you'll build

```text
userrepo/                    independent module: example.com/userrepo
  go.mod                     go 1.25; requires modernc.org/sqlite
  userrepo.go                UserRepo over *sql.DB: Migrate, Create, FindByID (ErrNoRows -> ErrNotFound)
  cmd/
    demo/
      main.go                open in-memory sqlite, migrate, create, find, miss
  userrepo_test.go           found / not-found (no sql leak) / cancelled-ctx, -race
```

Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
Implement: `FindByID` using `QueryRowContext` + `Row.Scan` that translates `sql.ErrNoRows` into a wrapped domain `ErrNotFound`; `Create` using `ExecContext`; `Migrate` for the schema.
Test: against an in-memory sqlite, a present row round-trips; a missing id returns `ErrNotFound` via `errors.Is` while `sql.ErrNoRows` does NOT leak; a cancelled context returns a context error.
Verify: `go test -count=1 -race ./...`

### Why translate at the boundary, and why exactly once

`sql.ErrNoRows` is returned by `Row.Scan` when a single-row query matched nothing.
It is a `database/sql` value, driver-independent, which is why you assert it with
`errors.Is(err, sql.ErrNoRows)` and never by matching the string
`"sql: no rows in result set"`. But `sql.ErrNoRows` is a *storage* concept, and
it should not escape the repository. If a service handler had to import
`database/sql` just to check whether a user exists, you have leaked the storage
layer into the domain, and swapping SQL for anything else becomes a cross-cutting
change. So the repository translates it once:

```
err := row.Scan(&u.ID, &u.Name, &u.Email)
if errors.Is(err, sql.ErrNoRows) {
    return User{}, fmt.Errorf("userrepo.FindByID(%d): %w", id, ErrNotFound)
}
```

Two properties matter, and the test pins both. First, `errors.Is(err, ErrNotFound)`
is true — callers get a clean domain assertion. Second, `errors.Is(err, sql.ErrNoRows)`
is *false* — because we wrapped `ErrNotFound`, not `sql.ErrNoRows`, the storage
sentinel does not leak up the chain. If you had instead written
`fmt.Errorf("...: %w", sql.ErrNoRows)` and defined `ErrNotFound` separately,
callers could accidentally match either, and the abstraction would be porous.

`Create` uses `ExecContext` and reads `LastInsertId` from the result. `FindByID`
uses `QueryRowContext`, whose returned `*sql.Row` defers its error until `Scan` —
so the `sql.ErrNoRows` check happens on the `Scan` result, not on a separate error
return. A cancelled context passed to `QueryRowContext` surfaces as a context
error from `Scan` (with modernc.org/sqlite, `context.Canceled` wrapped in the
driver error), which the fall-through `if err != nil` branch wraps and returns.

The driver is registered by a blank import, `_ "modernc.org/sqlite"`, and the
data-source name `:memory:` gives an in-memory database. Setting
`SetMaxOpenConns(1)` pins the whole test to a single connection so the schema and
rows created on one call are visible to the next — with the default pool, each
`:memory:` connection would otherwise get its own empty database.

Set up the module:

```bash
mkdir -p ~/go-exercises/userrepo/cmd/demo
cd ~/go-exercises/userrepo
go mod init example.com/userrepo
go mod edit -go=1.25
go get modernc.org/sqlite
```

Create `userrepo.go`:

```go
package userrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is the domain sentinel a caller asserts with errors.Is. The
// repository translates sql.ErrNoRows into this at its boundary so callers never
// import database/sql to check existence.
var ErrNotFound = errors.New("userrepo: user not found")

// User is the domain model returned by the repository.
type User struct {
	ID    int64
	Name  string
	Email string
}

// UserRepo is a repository over a *sql.DB.
type UserRepo struct {
	db *sql.DB
}

// NewUserRepo wraps db in a repository.
func NewUserRepo(db *sql.DB) *UserRepo {
	return &UserRepo{db: db}
}

// Migrate creates the users table if it does not exist.
func (r *UserRepo) Migrate(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS users (
	id    INTEGER PRIMARY KEY,
	name  TEXT NOT NULL,
	email TEXT NOT NULL UNIQUE
)`
	if _, err := r.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("userrepo.Migrate: %w", err)
	}
	return nil
}

// Create inserts a user and returns its new id.
func (r *UserRepo) Create(ctx context.Context, name, email string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO users(name, email) VALUES(?, ?)`, name, email)
	if err != nil {
		return 0, fmt.Errorf("userrepo.Create(%s): %w", email, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("userrepo.Create(%s): %w", email, err)
	}
	return id, nil
}

// FindByID returns the user with the given id, translating sql.ErrNoRows into a
// wrapped domain ErrNotFound so the storage sentinel never leaks to callers.
func (r *UserRepo) FindByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := r.db.QueryRowContext(ctx, `SELECT id, name, email FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name, &u.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("userrepo.FindByID(%d): %w", id, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("userrepo.FindByID(%d): %w", id, err)
	}
	return u, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"example.com/userrepo"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	repo := userrepo.NewUserRepo(db)
	if err := repo.Migrate(ctx); err != nil {
		panic(err)
	}

	id, err := repo.Create(ctx, "Alice", "alice@example.com")
	if err != nil {
		panic(err)
	}
	u, err := repo.FindByID(ctx, id)
	fmt.Printf("found: id=%d name=%s err=%v\n", u.ID, u.Name, err)

	_, err = repo.FindByID(ctx, 999)
	fmt.Printf("missing: is-not-found=%v leaks-sql=%v\n",
		errors.Is(err, userrepo.ErrNotFound), errors.Is(err, sql.ErrNoRows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: id=1 name=Alice err=<nil>
missing: is-not-found=true leaks-sql=false
```

### Tests

The tests open a fresh in-memory database, migrate, and exercise all three
contracts. `TestFindByIDNotFound` is the important one: it asserts both that the
domain sentinel is present *and* that the storage sentinel is absent.

Create `userrepo_test.go`:

```go
package userrepo

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestRepo(t *testing.T) *UserRepo {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Skipf("sqlite driver unavailable: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	repo := NewUserRepo(db)
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return repo
}

func TestFindByIDReturnsUser(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	id, err := repo.Create(ctx, "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	u, err := repo.FindByID(ctx, id)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if u.ID != id || u.Name != "Alice" || u.Email != "alice@example.com" {
		t.Fatalf("FindByID = %+v, want id=%d Alice alice@example.com", u, id)
	}
}

func TestFindByIDNotFound(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)

	_, err := repo.FindByID(context.Background(), 424242)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByID: err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FindByID leaked sql.ErrNoRows to caller: %v", err)
	}
}

func TestFindByIDCancelledContext(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	if _, err := repo.Create(context.Background(), "Bob", "bob@example.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.FindByID(ctx, 1)
	if err == nil {
		t.Fatal("FindByID with cancelled ctx: err = nil, want a context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FindByID: err = %v, want context.Canceled", err)
	}
}

func TestCreateDuplicateEmail(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()
	if _, err := repo.Create(ctx, "Alice", "dup@example.com"); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := repo.Create(ctx, "Alice2", "dup@example.com"); err == nil {
		t.Fatal("second Create with duplicate email: err = nil, want a UNIQUE constraint error")
	}
}
```

## Review

The repository is correct when the storage sentinel is fully contained. The two
assertions in `TestFindByIDNotFound` are the whole point: `errors.Is(err, ErrNotFound)`
must be true (callers get their domain error) and `errors.Is(err, sql.ErrNoRows)`
must be false (the driver concept did not escape). This only works because
`FindByID` wraps `ErrNotFound` with `%w` after detecting `sql.ErrNoRows`, never
the other way around. The cancelled-context test proves the read is genuinely
context-aware — `QueryRowContext` carries the deadline into the driver, and a
context error surfaces from `Scan`. Note the test helper skips (not fails) if the
driver cannot open, keeps everything on one connection so an in-memory schema
persists across calls, and registers `db.Close` with `t.Cleanup`. Run `-race`.

## Resources

- [database/sql: DB.QueryRowContext](https://pkg.go.dev/database/sql#DB.QueryRowContext) and [Row.Scan](https://pkg.go.dev/database/sql#Row.Scan) — where `sql.ErrNoRows` originates.
- [database/sql: ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables) — the storage sentinel you translate.
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — the pure-Go (cgo-free) driver used here.
- [Go docs: Canceling in-progress operations](https://go.dev/doc/database/cancel-operations) — how context cancellation reaches the driver.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-begintx-rollback-idempotency.md](05-begintx-rollback-idempotency.md)
