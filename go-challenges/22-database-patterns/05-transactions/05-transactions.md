# 5. Transactions

Transactions define the boundary where a set of changes becomes atomic. The hard part is the lifecycle: commit on success, rollback on any failure, and never leave the transaction open if `Commit` itself fails. This lesson builds a transfer repository that shows the canonical Go transaction pattern.

```text
dbtx/
  go.mod
  driver.go        (fake sql driver — the test seam)
  store.go         (package dbtx: Account, Store, atomic transfer)
  store_test.go
  example_test.go
  cmd/demo/main.go
```

## Concepts

### The Canonical Transaction Pattern

```go
tx, err := db.BeginTx(ctx, nil)
if err != nil {
    return err
}
defer tx.Rollback() // no-op after Commit; guards against panic or early return

// ... execute statements on tx ...

if err := tx.Commit(); err != nil {
    return err
}
return nil
```

The `defer tx.Rollback()` call is the safety net. If `Commit` succeeds, the subsequent `Rollback` is a no-op (most drivers return a specific error that can be ignored). If the function returns early due to an error, the deferred rollback releases the lock and the connection.

### `BeginTx` vs `Begin`

`db.BeginTx(ctx, opts)` accepts a context and `*sql.TxOptions`. Use it instead of `db.Begin()`:

- Context cancellation stops a transaction in-flight.
- `TxOptions.Isolation` sets the isolation level (e.g. `sql.LevelSerializable`) when the driver supports it.
- `TxOptions.ReadOnly` hints to the driver that no mutations are expected.

Passing `nil` for `TxOptions` uses the driver default, which is usually `READ COMMITTED`.

### Statements Must Execute on the Transaction

`tx` satisfies the same `QueryContext` / `ExecContext` interface as `*sql.DB`. Any statement executed on `db` inside the transaction boundary is outside the transaction. Use only `tx.ExecContext`, `tx.QueryContext`, and `tx.QueryRowContext` within a transaction function.

### Error Handling After Rollback

After a failed operation inside the transaction, call `Rollback` and return the original error, not the rollback error. If rollback also fails (e.g. connection lost), log the rollback error but still return the original one so the caller sees what actually went wrong.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/dbtx/cmd/demo
cd ~/go-exercises/dbtx
go mod init example.com/dbtx
go mod edit -go=1.26
```

This is a library package verified by `go test`.

### Exercise 1: The Fake Driver

Create `driver.go`:

```go
package dbtx

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync"
)

type fakeDriver struct {
	mu  sync.Mutex
	dbs map[string]*fakeDB
}

type fakeDB struct {
	mu        sync.Mutex
	cols      []string
	rows      [][]driver.Value
	committed []string // records of committed exec calls
}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dbs == nil {
		d.dbs = map[string]*fakeDB{}
	}
	db, ok := d.dbs[name]
	if !ok {
		db = &fakeDB{}
		d.dbs[name] = db
	}
	return &fakeConn{db: db}, nil
}

var globalDriver = &fakeDriver{}

func init() {
	sql.Register("fakedb", globalDriver)
}

func newFakeDB(name string) (*sql.DB, *fakeDB) {
	globalDriver.mu.Lock()
	if globalDriver.dbs == nil {
		globalDriver.dbs = map[string]*fakeDB{}
	}
	fdb := &fakeDB{}
	globalDriver.dbs[name] = fdb
	globalDriver.mu.Unlock()
	db, err := sql.Open("fakedb", name)
	if err != nil {
		panic(fmt.Sprintf("fakedb open: %v", err))
	}
	return db, fdb
}

func (fdb *fakeDB) addRow(cols []string, vals ...driver.Value) {
	fdb.mu.Lock()
	defer fdb.mu.Unlock()
	fdb.cols = cols
	fdb.rows = append(fdb.rows, vals)
}

func (fdb *fakeDB) commitCount() int {
	fdb.mu.Lock()
	defer fdb.mu.Unlock()
	return len(fdb.committed)
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{db: c.db}, nil
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{db: c.db}, nil }

type fakeStmt struct{ db *fakeDB }

func (s *fakeStmt) Close() error                                    { return nil }
func (s *fakeStmt) NumInput() int                                   { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) { return fakeResult{}, nil }
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	rows := s.db.rows
	cols := s.db.cols
	s.db.rows = nil
	return &fakeRows{cols: cols, rows: rows}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type fakeTx struct{ db *fakeDB }

func (tx *fakeTx) Commit() error {
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	tx.db.committed = append(tx.db.committed, "commit")
	return nil
}
func (tx *fakeTx) Rollback() error { return nil }

// ExposedFakeDB is the exported alias used by cmd/demo.
type ExposedFakeDB = fakeDB

// ExposedNewFakeDB opens a named fake database for use in cmd/demo.
func ExposedNewFakeDB(name string) (*sql.DB, *ExposedFakeDB) {
	return newFakeDB(name)
}

// AddRow seeds one row into the fake database.
func (fdb *fakeDB) AddRow(cols []string, vals ...driver.Value) {
	fdb.addRow(cols, vals...)
}
```

### Exercise 2: Atomic Transfer

Create `store.go`:

```go
package dbtx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrInvalidAmount is returned when the transfer amount is not positive.
	ErrInvalidAmount = errors.New("amount must be positive")
	// ErrSameAccount is returned when source and destination are the same.
	ErrSameAccount = errors.New("source and destination must differ")
)

// Transfer describes a movement of funds between two accounts.
type Transfer struct {
	FromID int64
	ToID   int64
	Amount int64 // in cents
}

// Validate checks that the transfer is self-consistent before the DB call.
func (t Transfer) Validate() error {
	if t.Amount <= 0 {
		return fmt.Errorf("transfer: %w", ErrInvalidAmount)
	}
	if t.FromID == t.ToID {
		return fmt.Errorf("transfer: %w", ErrSameAccount)
	}
	return nil
}

// Store wraps a *sql.DB and exposes repository methods.
type Store struct {
	db *sql.DB
}

// NewStore creates a Store backed by the given *sql.DB.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Execute runs the transfer atomically: debit FromID and credit ToID.
// It validates the transfer before opening the transaction.
func (s *Store) Execute(ctx context.Context, t Transfer) error {
	if err := t.Validate(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transfer tx: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	if _, err := tx.ExecContext(ctx,
		"UPDATE accounts SET balance = balance - ? WHERE id = ?",
		t.Amount, t.FromID); err != nil {
		return fmt.Errorf("debit account %d: %w", t.FromID, err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE accounts SET balance = balance + ? WHERE id = ?",
		t.Amount, t.ToID); err != nil {
		return fmt.Errorf("credit account %d: %w", t.ToID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transfer: %w", err)
	}
	return nil
}
```

### Exercise 3: Table-Driven Tests

Create `store_test.go`:

```go
package dbtx

import (
	"context"
	"errors"
	"testing"
)

func TestTransferValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		t       Transfer
		wantErr error
	}{
		{
			name:    "valid",
			t:       Transfer{FromID: 1, ToID: 2, Amount: 100},
			wantErr: nil,
		},
		{
			name:    "zero amount",
			t:       Transfer{FromID: 1, ToID: 2, Amount: 0},
			wantErr: ErrInvalidAmount,
		},
		{
			name:    "negative amount",
			t:       Transfer{FromID: 1, ToID: 2, Amount: -50},
			wantErr: ErrInvalidAmount,
		},
		{
			name:    "same account",
			t:       Transfer{FromID: 3, ToID: 3, Amount: 100},
			wantErr: ErrSameAccount,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.t.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestExecuteCommits(t *testing.T) {
	t.Parallel()

	sqlDB, fdb := newFakeDB("execute-commits")
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	transfer := Transfer{FromID: 1, ToID: 2, Amount: 500}

	if err := store.Execute(context.Background(), transfer); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// fakeTx.Commit records each commit in fdb.committed.
	if fdb.commitCount() != 1 {
		t.Fatalf("commitCount = %d, want 1", fdb.commitCount())
	}
}

func TestExecuteRejectsInvalidTransfer(t *testing.T) {
	t.Parallel()

	sqlDB, _ := newFakeDB("execute-invalid")
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	err := store.Execute(context.Background(), Transfer{FromID: 1, ToID: 1, Amount: 100})
	if !errors.Is(err, ErrSameAccount) {
		t.Fatalf("err = %v, want ErrSameAccount", err)
	}
}
```

Create `example_test.go`:

```go
package dbtx

import (
	"context"
	"fmt"
)

func ExampleStore_Execute() {
	sqlDB, _ := newFakeDB("example-tx")
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	err := store.Execute(context.Background(), Transfer{FromID: 1, ToID: 2, Amount: 100})
	fmt.Println(err)
	// Output: <nil>
}
```

### Exercise 4: The Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/dbtx"
)

func main() {
	sqlDB, _ := dbtx.ExposedNewFakeDB("demo")
	defer sqlDB.Close()

	store := dbtx.NewStore(sqlDB)
	transfer := dbtx.Transfer{FromID: 1, ToID: 2, Amount: 500}

	if err := store.Execute(context.Background(), transfer); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("transferred %d cents from account %d to account %d\n",
		transfer.Amount, transfer.FromID, transfer.ToID)
}
```

Add one more test of your own: assert that calling `Execute` with `Amount: 0` returns an error that wraps `ErrInvalidAmount`.

## Common Mistakes

Wrong: Omit `defer tx.Rollback()`. What happens: if the function returns early due to a scan error or a business rule failure, the transaction is left open. The connection returns to the pool in a dirty state; subsequent callers may see uncommitted data. Fix: always defer `tx.Rollback()` immediately after `BeginTx`.

Wrong: Execute statements on `db` instead of `tx` inside the transaction. What happens: those statements are committed immediately, outside the transaction boundary, so the atomic guarantee is broken. Fix: pass `tx` through the transaction scope and execute all statements on `tx`.

Wrong: Return the rollback error instead of the original error. What happens: the caller sees "transaction already committed" or "connection lost" instead of the actual business error. Fix: return the original error from the failed operation; log the rollback error separately if needed.

## Verification

From the module directory:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- `db.BeginTx(ctx, nil)` starts a transaction; defer `tx.Rollback()` immediately as a safety net.
- All statements inside the transaction must execute on `tx`, not on `db`.
- Call `tx.Commit()` on success; the deferred `Rollback` becomes a no-op.
- Validate business rules before opening the transaction to avoid unnecessary locking.

## What's Next

Continue with [Null Handling](../06-null-handling/06-null-handling.md).

## Resources

- [Go database guide: transactions](https://go.dev/doc/database/execute-transactions)
- [database/sql.Tx](https://pkg.go.dev/database/sql#Tx)
- [database/sql.TxOptions](https://pkg.go.dev/database/sql#TxOptions)
- [Go testing package reference](https://pkg.go.dev/testing)
