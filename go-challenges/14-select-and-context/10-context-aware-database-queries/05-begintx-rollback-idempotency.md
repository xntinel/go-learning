# Exercise 5: A Transactional Write With Correct defer-Rollback and ErrTxDone Handling

The single most important habit in transactional code is `defer tx.Rollback()`
on the line right after `BeginTx`. This exercise builds a money-transfer ledger
over `*sql.Tx` that proves why: the deferred rollback covers every early-return
and error path, and after a successful `Commit` it is a harmless no-op that
returns `sql.ErrTxDone`. A `CHECK(balance >= 0)` constraint gives us a real
mid-transaction failure to roll back.

## What you'll build

```text
ledger/                      independent module: example.com/ledger
  go.mod                     go 1.25; requires modernc.org/sqlite
  ledger.go                  Ledger.Transfer over BeginTx(Serializable) with defer-Rollback
  cmd/
    demo/
      main.go                a committing transfer and an overdraw that rolls back
  ledger_test.go             commit, constraint-rollback, ErrTxDone, no-leak, -race
```

Files: `ledger.go`, `cmd/demo/main.go`, `ledger_test.go`.
Implement: `Transfer(ctx, from, to, amount)` that `BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})`, `defer tx.Rollback()`, debits and credits on the same `ctx`, and `Commit`s.
Test: a valid transfer commits both sides; an overdraw trips the `CHECK` and the whole transaction rolls back leaving balances unchanged; `Rollback` after `Commit` returns `sql.ErrTxDone`; with `SetMaxOpenConns(1)` many transfers never leak a connection.
Verify: `go test -count=1 -race ./...`

### The fixed shape, and why the deferred rollback is not redundant with Commit

Every transactional function has the same skeleton, and it is worth internalizing
as muscle memory:

```
tx, err := l.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
if err != nil { return err }
defer tx.Rollback()          // covers every path below
// ... ExecContext / QueryRowContext, all on ctx ...
return tx.Commit()
```

The deferred `Rollback` is doing real work on three paths. If the debit `Exec`
fails, the function returns and the defer rolls back — releasing the connection
and any locks. If the credit `Exec` fails (our `CHECK` violation), same thing. If
the code panics between the two writes, the defer still runs during unwinding. On
the *success* path, `Commit` runs first and finishes the transaction; then the
deferred `Rollback` runs, sees the transaction is already done, and returns
`sql.ErrTxDone` — which is why you tolerate that specific error and treat any
*other* rollback error as real. A named return plus `errors.Join` is the clean
way to surface an unexpected rollback failure without masking a `Commit` error:

```
defer func() {
    if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
        err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
    }
}()
```

Without the deferred rollback, every early return would leak the transaction —
its connection stays checked out until the driver's finalizer reaps it, and under
`SetMaxOpenConns(1)` the very next `BeginTx` would block forever. The
"no connection leak" test makes that concrete: it pins the pool to one connection
and runs many transfers (valid and overdrawn), which only completes if every
failed transaction released its connection. A leak would deadlock the test.

`sql.LevelSerializable` requests the strictest isolation; the driver enforces
what it supports. `ReadOnly` is left false here because we write; a read-only
report query would set `&sql.TxOptions{ReadOnly: true}` both as a correctness
guard and as a replica-routing hint.

Set up the module:

```bash
go mod edit -go=1.25
go get modernc.org/sqlite
```

Create `ledger.go`:

```go
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Ledger is a double-entry account store over a *sql.DB.
type Ledger struct {
	db *sql.DB
}

// New wraps db in a Ledger.
func New(db *sql.DB) *Ledger { return &Ledger{db: db} }

// Migrate creates the accounts table. The CHECK constraint makes an overdraw a
// real mid-transaction failure to roll back.
func (l *Ledger) Migrate(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS accounts (
	id      TEXT PRIMARY KEY,
	balance INTEGER NOT NULL CHECK(balance >= 0)
)`
	if _, err := l.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("ledger.Migrate: %w", err)
	}
	return nil
}

// CreateAccount inserts an account with a starting balance.
func (l *Ledger) CreateAccount(ctx context.Context, id string, balance int64) error {
	if _, err := l.db.ExecContext(ctx, `INSERT INTO accounts(id, balance) VALUES(?, ?)`, id, balance); err != nil {
		return fmt.Errorf("ledger.CreateAccount(%s): %w", id, err)
	}
	return nil
}

// Balance returns an account's current balance.
func (l *Ledger) Balance(ctx context.Context, id string) (int64, error) {
	var b int64
	if err := l.db.QueryRowContext(ctx, `SELECT balance FROM accounts WHERE id = ?`, id).Scan(&b); err != nil {
		return 0, fmt.Errorf("ledger.Balance(%s): %w", id, err)
	}
	return b, nil
}

// Transfer moves amount from one account to another in a single serializable
// transaction. The deferred Rollback covers every early return; it is a no-op
// after Commit (returns sql.ErrTxDone). An overdraw trips the CHECK and rolls
// the whole transaction back.
func (l *Ledger) Transfer(ctx context.Context, from, to string, amount int64) (err error) {
	tx, err := l.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("ledger.Transfer begin: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("ledger.Transfer rollback: %w", rbErr))
		}
	}()

	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET balance = balance - ? WHERE id = ?`, amount, from); err != nil {
		return fmt.Errorf("ledger.Transfer debit %s: %w", from, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET balance = balance + ? WHERE id = ?`, amount, to); err != nil {
		return fmt.Errorf("ledger.Transfer credit %s: %w", to, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("ledger.Transfer commit: %w", err)
	}
	return nil
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

	"example.com/ledger"

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
	l := ledger.New(db)
	if err := l.Migrate(ctx); err != nil {
		panic(err)
	}
	if err := l.CreateAccount(ctx, "alice", 100); err != nil {
		panic(err)
	}
	if err := l.CreateAccount(ctx, "bob", 50); err != nil {
		panic(err)
	}

	err = l.Transfer(ctx, "alice", "bob", 30)
	a, _ := l.Balance(ctx, "alice")
	b, _ := l.Balance(ctx, "bob")
	fmt.Printf("commit: err=%v alice=%d bob=%d\n", err, a, b)

	err = l.Transfer(ctx, "alice", "bob", 9999)
	a, _ = l.Balance(ctx, "alice")
	b, _ = l.Balance(ctx, "bob")
	fmt.Printf("overdraw rolled back: failed=%v alice=%d bob=%d\n", err != nil, a, b)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
commit: err=<nil> alice=70 bob=80
overdraw rolled back: failed=true alice=70 bob=80
```

### Tests

The tests live in `package ledger` so they can reach the unexported `db` field to
demonstrate `ErrTxDone` directly.

Create `ledger_test.go`:

```go
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Skipf("sqlite driver unavailable: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	l := New(db)
	ctx := context.Background()
	if err := l.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := l.CreateAccount(ctx, "alice", 100); err != nil {
		t.Fatalf("CreateAccount alice: %v", err)
	}
	if err := l.CreateAccount(ctx, "bob", 50); err != nil {
		t.Fatalf("CreateAccount bob: %v", err)
	}
	return l
}

func TestTransferCommits(t *testing.T) {
	t.Parallel()
	l := newTestLedger(t)
	ctx := context.Background()

	if err := l.Transfer(ctx, "alice", "bob", 30); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	a, _ := l.Balance(ctx, "alice")
	b, _ := l.Balance(ctx, "bob")
	if a != 70 || b != 80 {
		t.Fatalf("after commit alice=%d bob=%d, want 70 and 80", a, b)
	}
}

func TestTransferRollsBackOnConstraint(t *testing.T) {
	t.Parallel()
	l := newTestLedger(t)
	ctx := context.Background()

	err := l.Transfer(ctx, "alice", "bob", 9999) // overdraw: trips CHECK on debit
	if err == nil {
		t.Fatal("Transfer overdraw: err = nil, want a constraint error")
	}
	a, _ := l.Balance(ctx, "alice")
	b, _ := l.Balance(ctx, "bob")
	if a != 100 || b != 50 {
		t.Fatalf("after rolled-back overdraw alice=%d bob=%d, want 100 and 50 (partial write leaked)", a, b)
	}
}

func TestRollbackAfterCommitIsErrTxDone(t *testing.T) {
	t.Parallel()
	l := newTestLedger(t)
	ctx := context.Background()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET balance = balance - 1 WHERE id = 'alice'`); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
		t.Fatalf("Rollback after Commit = %v, want sql.ErrTxDone", rbErr)
	}
}

func TestNoConnectionLeak(t *testing.T) {
	t.Parallel()
	l := newTestLedger(t) // SetMaxOpenConns(1): a leaked tx connection would deadlock the next BeginTx
	ctx := context.Background()

	for i := range 50 {
		// Alternate a valid transfer and an overdraw; both must release the connection.
		if i%2 == 0 {
			_ = l.Transfer(ctx, "bob", "alice", 1)
		} else {
			_ = l.Transfer(ctx, "alice", "bob", 100000)
		}
	}
	// If any transaction leaked its connection, we would never reach here.
	if _, err := l.Balance(ctx, "alice"); err != nil {
		t.Fatalf("Balance after many transfers: %v", err)
	}
}

func ExampleLedger_Transfer() {
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := context.Background()
	l := New(db)
	_ = l.Migrate(ctx)
	_ = l.CreateAccount(ctx, "alice", 100)
	_ = l.CreateAccount(ctx, "bob", 50)
	_ = l.Transfer(ctx, "alice", "bob", 30)
	a, _ := l.Balance(ctx, "alice")
	fmt.Println(a)
	// Output: 70
}
```

## Review

The transaction is correct when the failure path leaves no trace. `TestTransferRollsBackOnConstraint`
overdraws so the `CHECK` fails on the debit `Exec`; the function returns, the
deferred `Rollback` fires, and both balances read back unchanged — if either
moved, a partial write leaked. `TestRollbackAfterCommitIsErrTxDone` proves the
defer is safe to leave in place: after a real `Commit`, `Rollback` returns exactly
`sql.ErrTxDone`, which the production code tolerates while surfacing any other
rollback error via `errors.Join`. `TestNoConnectionLeak` is the operability
canary — pinned to one connection, 50 mixed transfers only complete if every
transaction (committed or rolled back) returned its connection to the pool. The
most common real bug this whole shape prevents is forgetting the deferred
rollback on an error path, which silently leaks connections until the pool is
exhausted. Run `-race`.

## Resources

- [database/sql: DB.BeginTx](https://pkg.go.dev/database/sql#DB.BeginTx) and [Tx.Rollback](https://pkg.go.dev/database/sql#Tx.Rollback) — the transaction lifecycle and `ErrTxDone`.
- [database/sql: TxOptions](https://pkg.go.dev/database/sql#TxOptions) — isolation levels and `ReadOnly`.
- [database/sql: ErrTxDone](https://pkg.go.dev/database/sql#pkg-variables) — returned by `Commit`/`Rollback` on a finished transaction.
- [Go docs: Executing transactions](https://go.dev/doc/database/execute-transactions) — the official `BeginTx`/`defer Rollback` guidance.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-per-query-timeout-budget.md](06-per-query-timeout-budget.md)
