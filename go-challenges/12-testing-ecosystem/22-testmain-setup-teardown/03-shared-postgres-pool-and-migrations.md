# Exercise 3: Provision a Shared *sql.DB and Run Migrations Once for the Whole Package

Integration tests for a repository layer need a real database, a schema, and a
connection pool. Opening a pool and running migrations in every test is slow and
racy. This exercise builds the standard harness: `TestMain` opens one shared
`*sql.DB`, pings it, applies the schema exactly once, publishes the pool through a
package var, and closes it in teardown — and it skips the whole integration
surface cleanly when no DSN is configured, so unit tests still run everywhere.

This module is fully self-contained: its own `go mod init`, repository, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
pgstore/                       independent module: example.com/pgstore
  go.mod                       go 1.26
  store.go                     UserStore over *sql.DB; Migrate; ErrNotFound
  cmd/
    demo/
      main.go                  runnable demo: uses the DSN if present, else skips
  store_test.go                TestMain opens/pings/migrates once; gated skips otherwise
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: `Migrate(ctx, db)`, `UserStore` with `Insert(ctx, email) (int64, error)` and `GetEmail(ctx, id) (string, error)` returning a wrapped `ErrNotFound`.
Test: a `TestMain`/`run()` that reads `TEST_DATABASE_DSN`, and when present opens+pings+migrates one pool, defers `Close` and a table drop; integration tests use the shared pool and skip when it is absent or `-short`.
Verify: `go test -count=1 -race ./...` (runs unit tests; integration tests skip without a DSN)

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/03-shared-postgres-pool-and-migrations/cmd/demo
cd go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/03-shared-postgres-pool-and-migrations
```

### Why the pool lives in TestMain

`sql.Open` returns a `*sql.DB` that is a *pool*, not a single connection, and it
is safe for concurrent use. Opening it is cheap, but pinging it (proving the
database is reachable) and running migrations are not, and neither should happen
more than once. `TestMain` runs before any test and on the main goroutine, so it
is the correct serialization point: open once, `PingContext` once, `Migrate` once,
publish the `*sql.DB` in a package var, and let every repository test share it.

This module compiles and its unit tests pass with no database at all. `sql.Open`
requires a registered driver, but the harness only calls it when a DSN is present;
offline, the DSN is empty, so the integration path is skipped and no driver import
is needed to build. To exercise the real path in a project, set
`TEST_DATABASE_DSN` and add a blank driver import to the test binary, for example
`import _ "github.com/jackc/pgx/v5/stdlib"` (then `sql.Open("pgx", dsn)`). The
harness is written so the driver name comes from the same place the DSN does.

### Graceful skip is the CI-safe default

A developer with no local Postgres, and a CI lane that only runs unit tests, must
still get a green run. So `TestMain` reads `TEST_DATABASE_DSN`; if it is empty (or
`-short` is set) it logs a line and returns `m.Run()` with the shared pool left
`nil`. Every integration test begins with `if DB == nil { t.Skip(...) }`. The
suite therefore degrades to "unit tests only" instead of failing. Only when a DSN
is present does the harness open, ping, migrate, and — via the `run()` wrapper so
the defers actually fire — close the pool and drop its table on the way out.

### Shared mutable DB forces per-test isolation

One pool shared across tests that may run in parallel means the tests write to
common tables. That is a contamination hazard. The two production answers are
unique keys per test (each test uses an email nobody else uses) or a per-test
transaction that is rolled back in `t.Cleanup`. This harness uses unique keys; the
round-trip test inserts an email derived from the test name so parallel tests
never collide.

Create `store.go`:

```go
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned (wrapped) when a lookup matches no row.
var ErrNotFound = errors.New("user not found")

// Migrate applies the schema. It is idempotent: safe to run on every startup.
func Migrate(ctx context.Context, db *sql.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS users (
	id    BIGSERIAL PRIMARY KEY,
	email TEXT UNIQUE NOT NULL
)`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate users: %w", err)
	}
	return nil
}

// UserStore is a repository over a shared connection pool.
type UserStore struct {
	db *sql.DB
}

// NewUserStore wraps a pool. The pool is owned by whoever opened it (TestMain in
// tests, main in production); the store does not close it.
func NewUserStore(db *sql.DB) *UserStore {
	return &UserStore{db: db}
}

// Insert stores an email and returns the new id.
func (s *UserStore) Insert(ctx context.Context, email string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert user %q: %w", email, err)
	}
	return id, nil
}

// GetEmail returns the email for id, or a wrapped ErrNotFound if there is no row.
func (s *UserStore) GetEmail(ctx context.Context, id int64) (string, error) {
	var email string
	err := s.db.QueryRowContext(ctx,
		`SELECT email FROM users WHERE id = $1`, id).Scan(&email)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("user %d: %w", id, ErrNotFound)
	case err != nil:
		return "", fmt.Errorf("get user %d: %w", id, err)
	}
	return email, nil
}
```

### The runnable demo

The demo mirrors the harness: it reads `TEST_DATABASE_DSN`, and without one it
prints a skip line and exits 0, so `go run ./cmd/demo` works on any machine.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		fmt.Println("TEST_DATABASE_DSN not set; skipping database demo")
		return
	}
	// With a DSN and a blank-imported driver, this is where you would
	// sql.Open, Migrate, insert, and read back. Kept out of the offline demo
	// so the package builds and runs with no driver dependency.
	fmt.Println("TEST_DATABASE_DSN set; run the integration tests to exercise the pool")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
TEST_DATABASE_DSN not set; skipping database demo
```

### Tests

`TestMain` delegates to `run()` so its defers (Close, drop) run even though the
suite ends in `os.Exit`. When a DSN is present it opens, pings, migrates, and
publishes the pool; otherwise it leaves `DB == nil`. `TestUserStoreRoundTrip` is
the integration test — it skips without a pool and otherwise inserts a
test-name-scoped email and reads it back. `TestGetEmailNotFound` asserts the
wrapped sentinel with `errors.Is`. `TestIntegrationGate` is a pure unit test that
documents the gating decision and always runs.

Create `store_test.go`:

```go
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

const dsnEnv = "TEST_DATABASE_DSN"

// DB is the shared pool. It is nil when no DSN is configured.
var DB *sql.DB

func run(m *testing.M) int {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" || testing.Short() {
		// No integration surface: unit tests still run, integration tests skip.
		return m.Run()
	}

	// Driver name travels with the DSN in real projects; "pgx" here assumes a
	// blank import of github.com/jackc/pgx/v5/stdlib in the test binary.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		// A misconfigured DSN degrades to unit-only rather than failing the lane.
		os.Stderr.WriteString("sql.Open failed, skipping integration: " + err.Error() + "\n")
		return m.Run()
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		os.Stderr.WriteString("ping failed, skipping integration: " + err.Error() + "\n")
		return m.Run()
	}
	if err := Migrate(ctx, db); err != nil {
		os.Stderr.WriteString("migrate failed: " + err.Error() + "\n")
		return 1
	}
	defer db.ExecContext(context.Background(), `DROP TABLE IF EXISTS users`)

	DB = db
	return m.Run()
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func TestIntegrationGate(t *testing.T) {
	t.Parallel()
	if _, ok := os.LookupEnv(dsnEnv); ok {
		t.Skip("DSN present: pool exercised by the integration tests")
	}
	if DB != nil {
		t.Fatal("DB should be nil when no DSN is configured")
	}
}

func TestUserStoreRoundTrip(t *testing.T) {
	t.Parallel()
	if DB == nil {
		t.Skip("no shared pool; set TEST_DATABASE_DSN to run integration tests")
	}
	store := NewUserStore(DB)
	ctx := t.Context()

	// Unique per test so parallel tests never collide on the UNIQUE(email).
	email := t.Name() + "@example.com"
	id, err := store.Insert(ctx, email)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := store.GetEmail(ctx, id)
	if err != nil {
		t.Fatalf("GetEmail: %v", err)
	}
	if got != email {
		t.Fatalf("GetEmail(%d) = %q, want %q", id, got, email)
	}
}

func TestGetEmailNotFound(t *testing.T) {
	t.Parallel()
	if DB == nil {
		t.Skip("no shared pool; set TEST_DATABASE_DSN to run integration tests")
	}
	store := NewUserStore(DB)
	_, err := store.GetEmail(t.Context(), -1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEmail(-1) err = %v, want ErrNotFound", err)
	}
}
```

## Review

The harness is correct when the expensive work — open, ping, migrate — happens
exactly once in `TestMain`, the pool is shared through a package var, and teardown
(Close, drop) lives in the `run()` wrapper so `os.Exit` does not skip it. The
skip-without-DSN path is what makes it CI-safe: unit tests run everywhere and the
integration tests opt in only when a database is present. The two hazards to keep
in mind are isolation (parallel tests share one pool, so keys must be unique or
each test must own a transaction it rolls back) and error wrapping
(`GetEmail` maps `sql.ErrNoRows` to a wrapped `ErrNotFound` asserted with
`errors.Is`, never with `==` on the string). Offline, `go test -race` runs the
unit test and skips the integration tests; with a DSN and a blank driver import it
exercises the real pool.

## Resources

- [`database/sql`: Open, DB, PingContext](https://pkg.go.dev/database/sql#Open) — the pool, and why `*sql.DB` is safe for concurrent use.
- [`database/sql.ErrNoRows`](https://pkg.go.dev/database/sql#pkg-variables) — the sentinel returned by `Row.Scan` when there is no row.
- [`testing.Short`](https://pkg.go.dev/testing#Short) — the `-short` flag used to skip slow integration tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-run-wrapper-for-deferred-teardown.md](02-run-wrapper-for-deferred-teardown.md) | Next: [04-shared-httptest-harness.md](04-shared-httptest-harness.md)
