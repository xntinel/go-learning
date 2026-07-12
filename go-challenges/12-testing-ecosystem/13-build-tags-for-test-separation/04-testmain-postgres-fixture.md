# Exercise 4: TestMain-owned Postgres repository integration tier

The canonical heavyweight integration fixture: a repository over real Postgres
whose connect, schema, and teardown happen exactly once per package in
`TestMain`, all behind `//go:build integration`. The default `go test ./...`
never compiles the SQL driver into its build graph; a separate CI stage runs the
tier with `-tags=integration` and a live `DATABASE_URL`.

Self-contained module: its own `go mod init`, an untagged repository plus
validation, an untagged demo and test, and the tagged integration file.

## What you'll build

```text
pgrepo/                     independent module: example.com/pgrepo
  go.mod
  user.go                  UserRepo over database/sql; ValidateUser; ErrInvalidEmail, ErrNotFound
  user_test.go             untagged: TestValidateUser (table-driven), ExampleValidateUser
  repo_integration_test.go //go:build integration: TestMain fixture + CRUD tests
  cmd/
    demo/
      main.go              validates sample users (no DB) with deterministic output
```

- Files: `user.go`, `user_test.go`, `repo_integration_test.go`, `cmd/demo/main.go`.
- Implement: a `UserRepo` with `Create` and `GetByID` over `database/sql`, a pure `ValidateUser`, and a `TestMain` that owns connect/schema/truncate/teardown once for the tagged tier.
- Test: untagged `TestValidateUser` runs under the default build with no driver; `repo_integration_test.go` connects once, shares one `*sql.DB` across CRUD tests, and skips cleanly when `DATABASE_URL` is unset.
- Verify: `go test -race ./...` (default, hermetic); `DATABASE_URL=... go test -tags=integration -race ./...` (live tier).

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/13-build-tags-for-test-separation/04-testmain-postgres-fixture/cmd/demo
cd go-solutions/12-testing-ecosystem/13-build-tags-for-test-separation/04-testmain-postgres-fixture
go mod edit -go=1.26
```

### Why the fixture lives in TestMain, and why os.Exit needs a helper

Connecting to Postgres, applying a schema, and truncating tables are expensive
and stateful. Paying that cost per test makes the tier slow and the tests
interfere with each other's rows. `func TestMain(m *testing.M)` runs once per
package: it is the single place to connect, migrate, seed, run the whole suite
via `m.Run()`, and tear down. Every test then borrows the one shared `*sql.DB`,
which is itself a concurrency-safe connection pool.

The subtlety that trips people up is `os.Exit`. `TestMain` must end by exiting
with the code `m.Run()` returns, but `os.Exit` does **not** run deferred
functions. If you write `defer db.Close()` and then `os.Exit(m.Run())`, the close
never fires. The fix is mechanical: put the body in a helper that *returns* the
code, keep the `defer` inside that helper, and have `TestMain` call
`os.Exit(run(m))`. The helper returns normally, its defers run, and only then
does `os.Exit` fire with the propagated code.

The whole `TestMain` and the driver import sit in a `//go:build integration`
file. That is the entire point: the default build has no `TestMain`, no
`database/sql` driver, and no fixture. The repository logic in `user.go` is
untagged and imports only `database/sql` from the standard library, which
compiles with no registered driver — a driver is only needed at `sql.Open` time,
which the default suite never reaches.

### Keeping validation pure so the default tier tests something real

`ValidateUser` is deliberately a pure function with no `*sql.DB` dependency, and
`Create` calls it before any round trip. That gives the hermetic default suite a
genuine behavioral test (`TestValidateUser`) and guarantees a malformed user
never reaches Postgres. The `errors.Is(err, sql.ErrNoRows)` branch in `GetByID`
maps the driver's sentinel to the package's own `ErrNotFound`, wrapped with `%w`
so callers can match it without importing `database/sql`.

Create `user.go`:

```go
package pgrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidEmail is returned (wrapped) when a user fails validation.
	ErrInvalidEmail = errors.New("pgrepo: invalid email")
	// ErrNotFound is returned (wrapped) when a lookup finds no row.
	ErrNotFound = errors.New("pgrepo: user not found")
)

// User is a persisted account.
type User struct {
	ID    int64
	Email string
	Name  string
}

// ValidateUser enforces the invariants the repository requires before it will
// touch the database. It is pure and unit-testable with no connection.
func ValidateUser(u User) error {
	if u.Email == "" || !strings.Contains(u.Email, "@") {
		return fmt.Errorf("validate %q: %w", u.Email, ErrInvalidEmail)
	}
	return nil
}

// UserRepo persists users in Postgres through database/sql.
type UserRepo struct {
	db *sql.DB
}

// NewUserRepo wraps an existing pool. The pool's lifecycle is owned by the
// caller (in the integration tier, by TestMain).
func NewUserRepo(db *sql.DB) *UserRepo {
	return &UserRepo{db: db}
}

// Create validates then inserts, returning the new id. Validation runs before
// any DB round trip, so a bad input never reaches Postgres.
func (r *UserRepo) Create(ctx context.Context, u User) (int64, error) {
	if err := ValidateUser(u); err != nil {
		return 0, err
	}
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id`,
		u.Email, u.Name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// GetByID returns the user, or ErrNotFound (wrapped) when the row is absent.
func (r *UserRepo) GetByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := r.db.QueryRowContext(ctx,
		`SELECT id, email, name FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("get user %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("get user %d: %w", id, err)
	}
	return u, nil
}
```

### The runnable demo

The demo runs under the default build with no database — it exercises the pure
validation path so the output is deterministic and offline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pgrepo"
)

func main() {
	users := []pgrepo.User{
		{Email: "alice@example.com", Name: "Alice"},
		{Email: "", Name: "Bob"},
	}
	for _, u := range users {
		if err := pgrepo.ValidateUser(u); err != nil {
			fmt.Printf("%s: %v\n", label(u), err)
			continue
		}
		fmt.Printf("%s: valid\n", label(u))
	}
}

func label(u pgrepo.User) string {
	if u.Email == "" {
		return "(no email)"
	}
	return u.Email
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
alice@example.com: valid
(no email): validate "": pgrepo: invalid email
```

### The untagged test

Create `user_test.go`:

```go
package pgrepo

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidateUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		user    User
		wantErr error
	}{
		{"ok", User{Email: "alice@example.com", Name: "Alice"}, nil},
		{"empty email", User{Email: "", Name: "Bob"}, ErrInvalidEmail},
		{"no at sign", User{Email: "carol.example.com"}, ErrInvalidEmail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateUser(tc.user)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateUser(%+v) = %v, want nil", tc.user, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateUser(%+v) = %v, want %v", tc.user, err, tc.wantErr)
			}
		})
	}
}

func ExampleValidateUser() {
	fmt.Println(ValidateUser(User{Email: "alice@example.com"}))
	fmt.Println(ValidateUser(User{Email: "bad"}) != nil)
	// Output:
	// <nil>
	// true
}
```

### The tagged integration tier

This file compiles only under `-tags=integration`. It brings in the pgx driver
(`_ "github.com/jackc/pgx/v5/stdlib"`, registered under the name `pgx`) and owns
the whole fixture in `TestMain`. When `DATABASE_URL` is unset the tier still
compiles for build-safety, and each test skips through `requireDB` — so
`go build -tags=integration ./...` and `go vet -tags=integration ./...` verify
the code without a live database, while a CI stage with a DSN actually runs it.

Create `repo_integration_test.go`:

```go
//go:build integration

package pgrepo

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id    BIGSERIAL PRIMARY KEY,
	email TEXT NOT NULL,
	name  TEXT NOT NULL
);`

// testDB is the single pool shared by every test in the tier. It is nil when
// DATABASE_URL is unset, which makes the DB tests skip.
var testDB *sql.DB

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run owns the fixture lifecycle and returns the exit code, so the deferred
// db.Close fires before os.Exit (os.Exit itself skips defers).
func run(m *testing.M) int {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Println("DATABASE_URL unset; live Postgres tests will skip")
		return m.Run()
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE users RESTART IDENTITY"); err != nil {
		log.Fatalf("truncate: %v", err)
	}
	testDB = db
	return m.Run()
}

// requireDB returns a repo bound to the shared pool, skipping when no DB is
// configured so the tier stays build-safe without a live database.
func requireDB(t *testing.T) *UserRepo {
	t.Helper()
	if testDB == nil {
		t.Skip("DATABASE_URL not set; skipping live Postgres test")
	}
	return NewUserRepo(testDB)
}

func TestCreateAndGet(t *testing.T) {
	repo := requireDB(t)
	ctx := t.Context()

	id, err := repo.Create(ctx, User{Email: "dana@example.com", Name: "Dana"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID(%d): %v", id, err)
	}
	if got.Email != "dana@example.com" || got.Name != "Dana" {
		t.Fatalf("GetByID = %+v, want dana@example.com/Dana", got)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	repo := requireDB(t)
	if _, err := repo.GetByID(t.Context(), 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}
```

## Review

The tier is correct when the default `go test ./...` compiles no SQL driver at
all — confirm with `go build ./...` succeeding without pgx in `go.mod` — and the
integration file only enters the build under `-tags=integration`. The fixture is
correct when `TestMain` connects exactly once and routes its exit through `run`
so `db.Close` runs; a `TestMain` that writes `os.Exit(m.Run())` with a bare
`defer db.Close()` silently leaks the connection because `os.Exit` skips defers.
The skip-when-unset pattern is what lets `go vet -tags=integration ./...` keep the
tagged code honest in CI without provisioning a database for every vet. If a CI
stage has a DSN, `DATABASE_URL=... go test -tags=integration -race ./...` runs the
real CRUD path against Postgres.

## Resources

- [testing: Main (TestMain / testing.M)](https://pkg.go.dev/testing#hdr-Main) — the once-per-package entry point and `m.Run`.
- [database/sql](https://pkg.go.dev/database/sql) — `sql.Open`, `DB.PingContext`, `DB.QueryRowContext`, `Row.Scan`, `sql.ErrNoRows`.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how `-tags=integration` includes the file and its driver import.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-run-and-verify-gates.md](03-run-and-verify-gates.md) | Next: [05-e2e-tag-second-tier.md](05-e2e-tag-second-tier.md)
