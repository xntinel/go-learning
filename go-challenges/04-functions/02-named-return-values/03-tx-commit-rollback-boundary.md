# Exercise 3: Transaction Boundary — Commit or Rollback from One defer

A transactional operation must end exactly one way: commit on success, roll back
on error, roll back and re-raise on panic — and if the commit itself fails, that
failure must reach the caller. Written naively this smears `tx.Rollback()` across
every error branch and forgets at least one. The canonical Go idiom collapses it
to a single deferred closure keyed on the named `err`.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
txn/                        independent module: example.com/txn
  go.mod
  txn.go                    Tx, DB interfaces; Transfer (one settle-tx defer)
  cmd/demo/
    main.go                 runnable demo: a committed op and a rolled-back op
  txn_test.go               commit/rollback/commit-fail/panic cases, exactly-one-of
```

- Files: `txn.go`, `cmd/demo/main.go`, `txn_test.go`.
- Implement: `Transfer(db, work) (err error)` that begins a tx and, in one deferred closure, commits when `err` is nil, rolls back when `err` is non-nil, and rolls back then re-raises on panic; a commit failure is surfaced through `err`.
- Test: a fake `Tx` recording Commit/Rollback calls; assert exactly-one-of semantics across success, business error, commit failure, and panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/03-tx-commit-rollback-boundary/cmd/demo
cd go-solutions/04-functions/02-named-return-values/03-tx-commit-rollback-boundary
```

### The single-exit settlement

The interfaces mirror `database/sql`: a `Tx` has `Commit() error` and
`Rollback() error`, and a `DB` can `Begin() (Tx, error)`. `Transfer` begins the
transaction, then registers one closure that runs on every exit path:

```go
defer func() {
	if p := recover(); p != nil {
		_ = tx.Rollback()
		panic(p)
	}
	if err != nil {
		_ = tx.Rollback()
		return
	}
	err = tx.Commit()
}()
```

Read it as three cases in priority order. If the stack is unwinding from a panic,
`recover()` returns the panic value; we roll back and re-raise it with `panic(p)`
so a programming bug is still fatal but the transaction does not leak. Otherwise,
if the named `err` is non-nil — the business logic failed — we roll back and stop.
Otherwise the work succeeded, so we commit; and because `err = tx.Commit()` assigns
the named result, a commit failure becomes the returned error. Every one of these
depends on `err` being a named result the closure can both read and write. The
rollback errors are intentionally discarded with `_ =`: on the error and panic
paths there is already a more meaningful failure to report, and rollback failures
on an aborting transaction are rarely actionable.

The `return work(tx)` line is where the named `err` is set. If `work` returns an
error, `return` copies it into `err`, then the defer sees it and rolls back. If
`work` panics, the panic propagates into the defer, which recovers, rolls back,
and re-raises.

Create `txn.go`:

```go
package txn

import "fmt"

// Tx is a minimal transaction, mirroring database/sql.Tx's settlement methods.
type Tx interface {
	Commit() error
	Rollback() error
}

// DB starts transactions.
type DB interface {
	Begin() (Tx, error)
}

// Transfer runs work inside a transaction and settles it in one deferred closure:
// commit on success, roll back on a business error, roll back and re-raise on a
// panic, and surface a commit failure through the named err. The single-exit
// contract is only expressible because err is a named result the defer inspects.
func Transfer(db DB, work func(Tx) error) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()

	return work(tx)
}
```

### The runnable demo

The demo defines a tiny in-memory transaction that records what happened, runs one
operation that succeeds and one that returns an error, and prints how each was
settled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/txn"
)

type memTx struct {
	committed bool
	rolled    bool
}

func (t *memTx) Commit() error   { t.committed = true; return nil }
func (t *memTx) Rollback() error { t.rolled = true; return nil }

type memDB struct{ tx *memTx }

func (d memDB) Begin() (txn.Tx, error) { return d.tx, nil }

func settle(db memDB, work func(txn.Tx) error) {
	err := txn.Transfer(db, work)
	fmt.Printf("err=%v committed=%v rolled=%v\n", err, db.tx.committed, db.tx.rolled)
}

func main() {
	settle(memDB{tx: &memTx{}}, func(txn.Tx) error {
		return nil // success -> commit
	})
	settle(memDB{tx: &memTx{}}, func(txn.Tx) error {
		return errors.New("insufficient funds") // error -> rollback
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err=<nil> committed=true rolled=false
err=insufficient funds committed=false rolled=true
```

### Tests

The fake `Tx` counts Commit and Rollback calls and can be told to fail either. The
tests assert exactly-one-of settlement across the four cases, plus that a commit
failure surfaces and a panic both rolls back and re-raises.

Create `txn_test.go`:

```go
package txn

import (
	"errors"
	"testing"
)

type fakeTx struct {
	commits   int
	rollbacks int
	commitErr error
}

func (f *fakeTx) Commit() error   { f.commits++; return f.commitErr }
func (f *fakeTx) Rollback() error { f.rollbacks++; return nil }

type fakeDB struct{ tx *fakeTx }

func (d fakeDB) Begin() (Tx, error) { return d.tx, nil }

func TestTransferCommitsOnSuccess(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{}
	err := Transfer(fakeDB{tx: tx}, func(Tx) error { return nil })
	if err != nil {
		t.Fatalf("Transfer: unexpected error: %v", err)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commits=%d rollbacks=%d, want 1/0", tx.commits, tx.rollbacks)
	}
}

func TestTransferRollsBackOnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	tx := &fakeTx{}
	err := Transfer(fakeDB{tx: tx}, func(Tx) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Transfer err = %v, want boom", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commits=%d rollbacks=%d, want 0/1", tx.commits, tx.rollbacks)
	}
}

func TestTransferSurfacesCommitError(t *testing.T) {
	t.Parallel()

	commitErr := errors.New("commit failed")
	tx := &fakeTx{commitErr: commitErr}
	err := Transfer(fakeDB{tx: tx}, func(Tx) error { return nil })
	if !errors.Is(err, commitErr) {
		t.Fatalf("Transfer err = %v, want commit failure", err)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commits=%d rollbacks=%d, want 1/0", tx.commits, tx.rollbacks)
	}
}

func TestTransferRollsBackAndReraisesOnPanic(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Transfer swallowed the panic; want re-raise")
		}
		if tx.rollbacks != 1 || tx.commits != 0 {
			t.Fatalf("commits=%d rollbacks=%d, want 0/1 on panic", tx.commits, tx.rollbacks)
		}
	}()

	_ = Transfer(fakeDB{tx: tx}, func(Tx) error { panic("worker exploded") })
}

func ExampleTransfer() {
	tx := &fakeTx{}
	err := Transfer(fakeDB{tx: tx}, func(Tx) error { return nil })
	// commits=1 on the happy path, and err is nil.
	_ = err
	// Output:
}
```

## Review

The settlement is correct when exactly one of Commit/Rollback runs on every path:
commit on success, rollback on business error, rollback-then-re-raise on panic,
and a commit failure surfaced through the named `err`. The classic bugs are a bare
`defer tx.Rollback()` (which rolls back even on success, or discards a commit
error), and forgetting the panic branch (a panic then leaves the transaction open
until the connection is reclaimed). The re-raise matters: recovering a panic into a
plain returned error here would hide a real programming bug behind a rolled-back
transaction. Note the tests assert the *counts*, not just the final error, so a
double-rollback or a missing commit is caught. Run `go test -race`.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [`database/sql.Tx`](https://pkg.go.dev/database/sql#Tx)
- [Go Spec: Handling panics](https://go.dev/ref/spec#Handling_panics)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-defer-error-wrapping-repository.md](02-defer-error-wrapping-repository.md) | Next: [04-panic-recovery-to-error.md](04-panic-recovery-to-error.md)
