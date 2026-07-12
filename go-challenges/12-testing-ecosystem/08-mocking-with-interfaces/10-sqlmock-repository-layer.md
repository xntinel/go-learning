# Exercise 10: go-sqlmock — Testing the SQL Repository Against the Driver

An in-memory fake of a repository interface is fast but never runs a line of SQL, so
it cannot catch a typo in a query, a wrong argument binding, or a broken transaction
sequence. go-sqlmock mocks the `database/sql` *driver* underneath a real repository,
so the actual SQL, arg binding, row scanning, and `Begin`/`Query`/`Exec`/`Commit`
ordering all execute. This module builds a transactional repository and tests it at
that level.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. It pulls one external dependency, `github.com/DATA-DOG/go-sqlmock`.

## What you'll build

```text
orderrepo/                   independent module: example.com/orderrepo
  go.mod                     go 1.26; requires github.com/DATA-DOG/go-sqlmock
  repo.go                    OrderRepo over *sql.DB; CreateOrder (begin, insert, outbox, commit)
  cmd/
    demo/
      main.go                runnable demo driving CreateOrder against a sqlmock DB
  repo_test.go               sqlmock: ExpectBegin/Query/Exec/Commit; rollback path; ExpectationsWereMet
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `OrderRepo.CreateOrder(ctx, o)` that begins a tx, `INSERT ... RETURNING id`, inserts an outbox row, and commits; a deferred rollback covers the error path.
- Test: with `QueryMatcherEqual`, expect the exact statements in order (`ExpectBegin`, `ExpectQuery.WithArgs.WillReturnRows`, `ExpectExec`, `ExpectCommit`); a failure path where the outbox exec errors and the tx rolls back (`ExpectRollback`); assert `ExpectationsWereMet`.
- Verify: `go test -count=1 -race ./...`

Set up the module and pull go-sqlmock:

```bash
go get github.com/DATA-DOG/go-sqlmock@latest
```

### The transactional repository

`CreateOrder` is the shape of a real write path that must be atomic: it opens a
transaction, inserts the order and reads back its generated id with `INSERT ...
RETURNING id`, writes a corresponding row to an `outbox` table (so a downstream
publisher can pick it up), and commits — order row and outbox row succeed or fail
together. The `defer tx.Rollback()` is the idiomatic safety net: after a successful
`Commit` it returns `sql.ErrTxDone` and is harmlessly ignored, but on any early
`return` it rolls the transaction back. The SQL is exported as constants so the test
and the demo assert against the exact same strings the repository executes.

Create `repo.go`:

```go
package repo

import (
	"context"
	"database/sql"
)

// Exported so tests and the demo match the exact SQL the repository runs.
const (
	InsertOrder  = `INSERT INTO orders (customer, total) VALUES ($1, $2) RETURNING id`
	InsertOutbox = `INSERT INTO outbox (topic, payload) VALUES ($1, $2)`
)

// Order is the row being written.
type Order struct {
	ID       int64
	Customer string
	Total    int64
}

// OrderRepo is a concrete repository over database/sql (not an interface fake).
type OrderRepo struct {
	db *sql.DB
}

// New injects the *sql.DB. In production it is a real pool; in tests it is the
// *sql.DB returned by sqlmock.
func New(db *sql.DB) *OrderRepo {
	return &OrderRepo{db: db}
}

// CreateOrder inserts the order and its outbox event in one transaction, returning
// the generated id. Any failure rolls the whole transaction back.
func (r *OrderRepo) CreateOrder(ctx context.Context, o *Order) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after a successful Commit; rolls back on early return

	var id int64
	if err := tx.QueryRowContext(ctx, InsertOrder, o.Customer, o.Total).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, InsertOutbox, "order.created", o.Customer); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
```

### The runnable demo

sqlmock is a driver, so the demo can drive `CreateOrder` deterministically with no
real database — it programs the expected statements, runs the method, and prints the
returned id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/DATA-DOG/go-sqlmock"

	"example.com/orderrepo"
)

func main() {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		fmt.Println("sqlmock:", err)
		return
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(repo.InsertOrder).
		WithArgs("acme", int64(4999)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectExec(repo.InsertOutbox).
		WithArgs("order.created", "acme").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	id, err := repo.New(db).CreateOrder(context.Background(), &repo.Order{Customer: "acme", Total: 4999})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("created order id=%d\n", id)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the mock returns id 7):

```
created order id=7
```

### The tests

`sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))` switches from
the default regex matcher to exact string matching, so the expected SQL must equal
what the repository runs — the reason the SQL lives in shared constants. Each test
programs the full statement sequence: `ExpectBegin`, then `ExpectQuery` for the
`RETURNING id` insert with `WithArgs` and `WillReturnRows`, then `ExpectExec` for the
outbox insert, then `ExpectCommit`. Running `CreateOrder` drives the real
`database/sql` machinery against the mock driver, and `ExpectationsWereMet()` at the
end fails if any statement was missing, extra, or out of order.

`TestCreateOrderRollsBack` makes the outbox `ExpectExec` return an error and expects
a `Rollback` instead of a `Commit`; because the repository's deferred
`tx.Rollback()` fires on the early return, the mock's `ExpectRollback` is satisfied
and `ExpectationsWereMet` confirms the transaction was aborted, not committed. This
is exactly the class of bug the in-memory fake from Exercise 2 cannot catch: the SQL,
the binding, and the transaction sequencing are all real here.

Create `repo_test.go`:

```go
package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCreateOrderCommits(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(InsertOrder).
		WithArgs("acme", int64(4999)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
	mock.ExpectExec(InsertOutbox).
		WithArgs("order.created", "acme").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	id, err := New(db).CreateOrder(context.Background(), &Order{Customer: "acme", Total: 4999})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreateOrderRollsBack(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	boom := errors.New("outbox insert failed")
	mock.ExpectBegin()
	mock.ExpectQuery(InsertOrder).
		WithArgs("acme", int64(4999)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
	mock.ExpectExec(InsertOutbox).
		WithArgs("order.created", "acme").
		WillReturnError(boom)
	mock.ExpectRollback()

	_, err = New(db).CreateOrder(context.Background(), &Order{Customer: "acme", Total: 4999})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the outbox error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
```

## Review

The repository is correct when `CreateOrder` runs the two inserts and the commit in
one transaction and rolls the whole thing back on any failure. go-sqlmock verifies
this at the driver level, which is its entire value over an interface fake: the exact
SQL, the argument binding, the `RETURNING id` scan, and the statement ordering are
all exercised, so a query typo or a missing `Commit` fails the test. Two rules make
it reliable: use `QueryMatcherOption(QueryMatcherEqual)` for exact SQL (the default
regex matcher can match statements you did not intend) and always assert
`ExpectationsWereMet()` so a missing or extra query is reported. Pair this level-2
test with the level-1 in-memory fake from Exercise 2: the fake is faster and proves
service logic, sqlmock proves the SQL. Run `go test -race`.

## Resources

- [DATA-DOG/go-sqlmock](https://github.com/DATA-DOG/go-sqlmock) — the driver mock, `Expect*` API, `QueryMatcherEqual`, and `ExpectationsWereMet`.
- [database/sql](https://pkg.go.dev/database/sql) — `BeginTx`, `QueryRowContext`, `ExecContext`, `Tx.Commit`/`Rollback`.
- [Go blog: Using database/sql](https://go.dev/doc/database/) — transactions and the deferred-rollback idiom.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../09-httptest/00-concepts.md](../09-httptest/00-concepts.md)
