# Exercise 7: Idempotent Schema Migration And Fixture Seeding In TestMain

An integration package owns its schema and its baseline data, and establishes both
exactly once. This module builds `migrate` (idempotent `CREATE TABLE IF NOT EXISTS`
DDL) and `seed` (idempotent upserts), written against an `Execer` interface so the
idempotent *shape* is tested in the default build, and invoked once from a tag-gated
`TestMain` before `m.Run()`.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
schemaseed/                independent module: example.com/schemaseed
  go.mod
  schema.go                Execer; migrations DDL; migrate; seed; Account
  cmd/
    demo/
      main.go              runs migrate+seed against a recording Execer
  schema_test.go           idempotency + IF NOT EXISTS / ON CONFLICT assertions
  main_integration_test.go //go:build integration: TestMain runs migrate+seed once
```

- Files: `schema.go`, `cmd/demo/main.go`, `schema_test.go`, `main_integration_test.go`.
- Implement: `migrate(ctx, ex)` applying `CREATE TABLE IF NOT EXISTS` DDL and `seed(ctx, ex)` upserting a fixed fixture set.
- Test: assert `migrate` is idempotent (runs twice, no error) and uses `IF NOT EXISTS`; assert `seed` uses `ON CONFLICT` and returns a known row count.
- Verify: the default build tests the idempotent shape with a fake; the tag-gated `TestMain` runs migrate+seed once and aborts with a non-zero exit if migration fails.

### Why idempotent, and why once in TestMain

The integration tests depend on a known baseline: specific tables, and a fixed set
of seed rows. Two properties make that baseline reproducible. First, migrations use
`CREATE TABLE IF NOT EXISTS` so applying them to an already-migrated database is a
no-op rather than a "relation already exists" error — which means re-running the
suite, or running `migrate` twice in one run, is safe. Second, seeds use upserts
(`INSERT ... ON CONFLICT (id) DO UPDATE`) so re-seeding does not duplicate rows or
fail on the primary key; the baseline converges to the same state every time.

Both run once, in `TestMain`, before `m.Run()` — not per test. Per-test migration is
slow and hammers connection limits; once-per-package keeps the tier fast. And a
failed migration must abort the whole suite with a non-zero exit *before* any test
runs, because tests against a half-migrated schema produce noise, not signal. The
`run(m) int` helper returns `1` on migration failure, so no test executes.

Testing the idempotent *shape* does not need a database: `migrate` and `seed` take
an `Execer`, so a recording fake captures every statement, and the default-build
test asserts the DDL contains `IF NOT EXISTS` and the seed contains `ON CONFLICT`,
and that running `migrate` twice returns no error. The real idempotency against
Postgres is then confirmed in the integration tier.

Create `schema.go`:

```go
package schemaseed

import (
	"context"
	"database/sql"
	"fmt"
)

// Execer is the write surface shared by *sql.DB, *sql.Tx, and a test fake.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Account is a seed fixture row.
type Account struct {
	ID   string
	Name string
}

// migrations are idempotent DDL statements: applying them twice is a no-op.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS accounts (id text PRIMARY KEY, name text NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sessions (id text PRIMARY KEY, account_id text NOT NULL REFERENCES accounts(id))`,
}

// seedAccounts is the deterministic baseline the integration tests depend on.
var seedAccounts = []Account{
	{ID: "acct:1", Name: "alice"},
	{ID: "acct:2", Name: "bob"},
	{ID: "acct:3", Name: "carol"},
}

// migrate applies the schema. Idempotent: safe to run on a fresh or existing DB.
func migrate(ctx context.Context, ex Execer) error {
	for _, stmt := range migrations {
		if _, err := ex.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// seed upserts the baseline accounts and returns how many rows it seeded.
// Idempotent: re-seeding updates rather than duplicating.
func seed(ctx context.Context, ex Execer) (int, error) {
	const q = `INSERT INTO accounts (id, name) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`
	for _, a := range seedAccounts {
		if _, err := ex.ExecContext(ctx, q, a.ID, a.Name); err != nil {
			return 0, fmt.Errorf("seed %q: %w", a.ID, err)
		}
	}
	return len(seedAccounts), nil
}

// Migrate and Seed are the exported entry points TestMain calls.
func Migrate(ctx context.Context, ex Execer) error { return migrate(ctx, ex) }

func Seed(ctx context.Context, ex Execer) (int, error) { return seed(ctx, ex) }

// SeedCount reports the baseline row count the tests expect.
func SeedCount() int { return len(seedAccounts) }
```

Now the tag-gated fixture runs migrate+seed once and aborts on migration failure:

Create `main_integration_test.go`:

```go
//go:build integration

package schemaseed

import (
	"context"
	"database/sql"
	"log"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var testDB *sql.DB

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Println("DATABASE_URL not set; nothing to run")
		return 0
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("open: %v", err)
		return 1
	}
	defer db.Close()

	ctx := context.Background()
	// A failed migration aborts the whole suite before any test runs.
	if err := Migrate(ctx, db); err != nil {
		log.Printf("migrate: %v", err)
		return 1
	}
	if _, err := Seed(ctx, db); err != nil {
		log.Printf("seed: %v", err)
		return 1
	}
	testDB = db
	return m.Run()
}

func TestSeedBaseline(t *testing.T) {
	var n int
	if err := testDB.QueryRowContext(t.Context(), `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != SeedCount() {
		t.Fatalf("account count = %d, want %d", n, SeedCount())
	}

	// Re-running migrate and seed must be a no-op (idempotent), not an error.
	if err := Migrate(t.Context(), testDB); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := Seed(t.Context(), testDB); err != nil {
		t.Fatalf("second seed: %v", err)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"

	"example.com/schemaseed"
)

// recordingExecer counts the statements migrate and seed issue.
type recordingExecer struct{ stmts int }

func (e *recordingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	e.stmts++
	return result{}, nil
}

type result struct{}

func (result) LastInsertId() (int64, error) { return 0, nil }
func (result) RowsAffected() (int64, error) { return 1, nil }

func main() {
	ctx := context.Background()
	ex := &recordingExecer{}
	if err := schemaseed.Migrate(ctx, ex); err != nil {
		fmt.Println("migrate:", err)
		return
	}
	n, _ := schemaseed.Seed(ctx, ex)
	fmt.Printf("migrate+seed issued %d statements, seeded %d rows\n", ex.stmts, n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
migrate+seed issued 5 statements, seeded 3 rows
```

### Tests

The default-build tests capture every statement with a recording fake and assert the
idempotent constructs are present and that a double `migrate` returns no error.

Create `schema_test.go`:

```go
package schemaseed

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// recordingExecer records every statement it is asked to run.
type recordingExecer struct {
	stmts []string
}

func (e *recordingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	e.stmts = append(e.stmts, query)
	return fakeResult{}, nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

func TestMigrateIsIdempotentShape(t *testing.T) {
	t.Parallel()
	ex := &recordingExecer{}
	if err := migrate(t.Context(), ex); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Running a second time must not error (idempotent).
	if err := migrate(t.Context(), ex); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(ex.stmts) != 2*len(migrations) {
		t.Fatalf("issued %d statements, want %d", len(ex.stmts), 2*len(migrations))
	}
	for _, s := range ex.stmts {
		if !strings.Contains(s, "IF NOT EXISTS") {
			t.Fatalf("DDL %q missing IF NOT EXISTS; not idempotent", s)
		}
	}
}

func TestSeedIsUpsertAndCounts(t *testing.T) {
	t.Parallel()
	ex := &recordingExecer{}
	n, err := seed(t.Context(), ex)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n != SeedCount() {
		t.Fatalf("seed count = %d, want %d", n, SeedCount())
	}
	for _, s := range ex.stmts {
		if !strings.Contains(s, "ON CONFLICT") {
			t.Fatalf("seed statement %q is not an upsert; re-seeding would duplicate", s)
		}
	}
}

var _ Execer = (*recordingExecer)(nil)
```

## Review

The two properties that make the baseline reproducible are `IF NOT EXISTS` in the
DDL and `ON CONFLICT ... DO UPDATE` in the seed: together they make running migrate
and seed twice a no-op, which the default-build test pins and the integration test
confirms against Postgres. Both run once in `TestMain` before `m.Run()`, never per
test, and a failed migration returns a non-zero exit so no test runs against a
half-built schema. The mistake to avoid is per-test migration — it is slow, it
exhausts connections, and it multiplies the chance of a flake. Confirm the default
tests pass with a fake and no database, and that the integration `TestSeedBaseline`
sees exactly `SeedCount()` rows and survives a second migrate+seed.

## Resources

- [database/sql: DB.ExecContext](https://pkg.go.dev/database/sql#DB.ExecContext) — issuing DDL and upsert statements.
- [testing: TestMain](https://pkg.go.dev/testing#hdr-Main) — the once-per-package hook that owns migrate+seed.
- [Accessing a relational database](https://go.dev/doc/tutorial/database-access) — the official `database/sql` tutorial.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-db-readiness-retry-connect.md](06-db-readiness-retry-connect.md) | Next: [08-e2e-tier-second-build-tag.md](08-e2e-tier-second-build-tag.md)
