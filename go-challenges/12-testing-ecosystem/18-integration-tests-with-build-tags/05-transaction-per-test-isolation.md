# Exercise 5: Transaction-Per-Test Isolation With Rollback Cleanup

The classic integration flake is a test that passes alone and fails in a suite,
because it leaked rows into the shared database. This module builds the fastest
isolation strategy — transaction-per-test — where each test runs against a
`*sql.Tx` and registers `t.Cleanup(tx.Rollback)`, so its writes vanish and the next
test sees a clean slate. The repository functions are written against an interface
so their logic is tested in the default build.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
txrepo/                    independent module: example.com/txrepo
  go.mod
  repo.go                  Execer interface; InsertAccount, wrapped ErrNoInsert
  cmd/
    demo/
      main.go              runs InsertAccount against a recording Execer
  repo_test.go             default-build tests of InsertAccount + t.Cleanup LIFO proof
  tx_integration_test.go   //go:build integration: BeginTx + t.Cleanup(rollback), two ordered tests
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`, `tx_integration_test.go`.
- Implement: `InsertAccount(ctx, ex Execer, id, name)` over an `Execer` interface satisfied by `*sql.DB`, `*sql.Tx`, and a fake.
- Test: default-build tests of the insert logic and of `t.Cleanup` LIFO order; two ordered integration tests that each see a clean slate because the prior test rolled back.
- Verify: the default build tests the SQL logic with a fake; the integration build proves rollback isolation with `BeginTx` + `t.Cleanup`.

Set up the module:

```bash
mkdir -p ~/go-exercises/txrepo/cmd/demo
cd ~/go-exercises/txrepo
go mod init example.com/txrepo
```

### The three isolation strategies, and why rollback is the default

Every integration suite must decide how a test undoes its writes. There are three
strategies, with distinct trade-offs:

- **Transaction-per-test**: open `BeginTx`, run every read and write against the
  `*sql.Tx`, and `t.Cleanup(tx.Rollback)`. Fastest, auto-cleaning, no leftover rows.
  Two hard limits: the code under test must *not* commit its own transaction
  (rollback then cannot undo it, and the code cannot see uncommitted writes through
  a separate connection), and it is not parallel-safe on one shared connection.
- **Truncate-between-tests**: let tests commit, then `TRUNCATE` the tables in
  cleanup. Works when the code commits, but must serialize because a truncate is
  global to the table.
- **Fresh-schema-per-run**: a new database or schema per test. Most isolated, and by
  far the slowest.

Pick per what the code under test does. If it commits internally, rollback isolation
cannot see its writes and you must truncate. For a repository whose methods take an
`Execer` (so the *test* owns the transaction), rollback is the right default: it is
the fastest and it cleans up for free.

### Why the repository takes an Execer

`InsertAccount` accepts an `Execer` interface — `ExecContext(ctx, query, args...)` —
rather than a concrete `*sql.DB`. Both `*sql.DB` and `*sql.Tx` satisfy it, and so
does a fake. In production a caller passes the pool; in the integration test the
caller passes the per-test `*sql.Tx`; in the fast tier the test passes a recording
fake and asserts on the exact query and arguments, with no database at all. This is
the seam that lets the same repository code be tested three ways.

Create `repo.go`:

```go
package txrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoInsert is returned (wrapped) when an insert affects zero rows.
var ErrNoInsert = errors.New("txrepo: insert affected no rows")

// Execer is the write surface shared by *sql.DB, *sql.Tx, and a test fake. The
// test that owns the transaction decides which concrete value flows in.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// InsertAccount inserts one account row through ex. Passing a *sql.Tx makes the
// write part of the caller's transaction, so a rollback undoes it.
func InsertAccount(ctx context.Context, ex Execer, id, name string) error {
	const q = `INSERT INTO accounts (id, name) VALUES ($1, $2)`
	res, err := ex.ExecContext(ctx, q, id, name)
	if err != nil {
		return fmt.Errorf("insert %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert %q: rows affected: %w", id, err)
	}
	if n != 1 {
		return fmt.Errorf("insert %q: %w", id, ErrNoInsert)
	}
	return nil
}
```

Now the tag-gated isolation proof. `beginTx` opens a transaction and registers the
rollback as cleanup; the two ordered tests each write the *same* id and each expect
an empty table, which only holds if the previous test's write was rolled back:

Create `tx_integration_test.go`:

```go
//go:build integration

package txrepo

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func beginTx(t *testing.T) *sql.Tx {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration tier: set DATABASE_URL to run")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS accounts (id text PRIMARY KEY, name text NOT NULL)`); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Registered after the Close cleanup, so by LIFO it runs first: rollback, then
	// close. Every write on tx is undone before the connection is returned.
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func countAccounts(t *testing.T, tx *sql.Tx) int {
	t.Helper()
	var n int
	if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestIsolationFirstWriter(t *testing.T) {
	tx := beginTx(t)
	if n := countAccounts(t, tx); n != 0 {
		t.Fatalf("start count = %d, want 0 (prior test must have rolled back)", n)
	}
	if err := InsertAccount(t.Context(), tx, "acct:1", "alice"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := countAccounts(t, tx); n != 1 {
		t.Fatalf("after insert count = %d, want 1", n)
	}
}

func TestIsolationSecondWriter(t *testing.T) {
	tx := beginTx(t)
	// If the first test had committed acct:1, this would see a row and the
	// conflicting insert below would fail on the primary key. Rollback isolation
	// guarantees a clean slate.
	if n := countAccounts(t, tx); n != 0 {
		t.Fatalf("start count = %d, want 0", n)
	}
	if err := InsertAccount(t.Context(), tx, "acct:1", "bob"); err != nil {
		t.Fatalf("insert: %v", err)
	}
}
```

### The runnable demo

The demo runs `InsertAccount` against a tiny in-memory `Execer`, so it needs no
database while still exercising the real function.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"

	"example.com/txrepo"
)

// countingExecer is a trivial Execer that reports one affected row.
type countingExecer struct{ calls int }

func (e *countingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	e.calls++
	return result{rows: 1}, nil
}

type result struct{ rows int64 }

func (r result) LastInsertId() (int64, error) { return 0, nil }
func (r result) RowsAffected() (int64, error) { return r.rows, nil }

func main() {
	ex := &countingExecer{}
	if err := txrepo.InsertAccount(context.Background(), ex, "acct:1", "alice"); err != nil {
		fmt.Println("insert error:", err)
		return
	}
	fmt.Printf("inserted acct:1 in %d exec call(s)\n", ex.calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
inserted acct:1 in 1 exec call(s)
```

### Tests

The default-build tests pin the insert logic with a recording fake and prove the
`t.Cleanup` LIFO ordering that makes `t.Cleanup(tx.Rollback)` reliable — cleanups
run last-added-first, so registering rollback *after* the connection-close cleanup
guarantees the rollback happens before the close.

Create `repo_test.go`:

```go
package txrepo

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// recordingExecer captures the query and args, and returns a configurable result.
type recordingExecer struct {
	query string
	args  []any
	rows  int64
	err   error
}

func (e *recordingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	e.query = query
	e.args = args
	if e.err != nil {
		return nil, e.err
	}
	return fakeResult{rows: e.rows}, nil
}

type fakeResult struct{ rows int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.rows, nil }

func TestInsertAccountSuccess(t *testing.T) {
	t.Parallel()
	ex := &recordingExecer{rows: 1}
	if err := InsertAccount(t.Context(), ex, "acct:1", "alice"); err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	if ex.query == "" || len(ex.args) != 2 || ex.args[0] != "acct:1" || ex.args[1] != "alice" {
		t.Fatalf("recorded query=%q args=%v; want the insert with id and name", ex.query, ex.args)
	}
}

func TestInsertAccountNoRows(t *testing.T) {
	t.Parallel()
	ex := &recordingExecer{rows: 0}
	err := InsertAccount(t.Context(), ex, "acct:1", "alice")
	if !errors.Is(err, ErrNoInsert) {
		t.Fatalf("InsertAccount = %v, want wrapped ErrNoInsert", err)
	}
}

func TestInsertAccountExecError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("connection reset")
	ex := &recordingExecer{err: sentinel}
	err := InsertAccount(t.Context(), ex, "acct:1", "alice")
	if !errors.Is(err, sentinel) {
		t.Fatalf("InsertAccount = %v, want wrapped exec error", err)
	}
}

// TestCleanupLIFO proves t.Cleanup runs last-added-first. beginTx relies on this:
// registering rollback after the close cleanup makes rollback run before close.
func TestCleanupLIFO(t *testing.T) {
	t.Parallel()
	var order []int
	t.Run("inner", func(t *testing.T) {
		t.Cleanup(func() { order = append(order, 1) })
		t.Cleanup(func() { order = append(order, 2) })
		t.Cleanup(func() { order = append(order, 3) })
	})
	// The subtest returned, so its cleanups have run, in LIFO order.
	if want := []int{3, 2, 1}; !slices.Equal(order, want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
}

var _ Execer = (*recordingExecer)(nil)
```

## Review

Transaction-per-test is the fastest isolation strategy and cleans up for free, but
it has two hard boundaries: the code under test must not commit its own transaction,
and the shared connection is not parallel-safe — leaving `t.Parallel()` on tests
that share one `*sql.Tx` corrupts the transaction, and `t.Setenv` would panic under
parallelism anyway. The reliability of `t.Cleanup(tx.Rollback)` rests on the LIFO
ordering the default-build test pins: register rollback after the close cleanup and
rollback runs first. If your code under test commits internally, switch to
truncate-between-tests (serialized) or fresh-schema-per-run. Confirm the default
tests pass with a fake and no database, and that the two integration tests each
observe a zero starting count — the signature of rollback isolation working.

## Resources

- [database/sql: DB.BeginTx and Tx](https://pkg.go.dev/database/sql#DB.BeginTx) — starting a transaction and `Tx.Rollback`.
- [database/sql: TxOptions](https://pkg.go.dev/database/sql#TxOptions) — isolation level and read-only transactions.
- [testing: T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — LIFO cleanup ordering.
- [testing: T.Context](https://pkg.go.dev/testing#T.Context) — the per-test context used as the operation deadline.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-testmain-integration-fixture.md](04-testmain-integration-fixture.md) | Next: [06-db-readiness-retry-connect.md](06-db-readiness-retry-connect.md)
