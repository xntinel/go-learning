# Exercise 7: Transaction Guard — Roll Back and Re-Panic on Failure

A panic in the middle of a database transaction is where non-deferred cleanup
kills you: if the rollback only runs on the normal path, a panic leaves the
transaction open, its connection held, and the write half-applied. This module
builds a `WithTx` guard whose deferred function rolls back on both an error and a
panic — and re-panics genuine faults so a bug still crashes instead of committing
corruption.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
txguard/                     independent module: example.com/txguard
  go.mod                     go 1.26
  txguard.go                 Tx, DB interfaces; WithTx(ctx, db, fn) error
  cmd/
    demo/
      main.go                runnable demo: commit on success, rollback on error
  txguard_test.go            fake Tx records calls: commit/rollback/panic/commit-error cases
```

Files: `txguard.go`, `cmd/demo/main.go`, `txguard_test.go`.
Implement: a `Tx` interface (`Commit`/`Rollback`) and `DB` interface (`Begin`), and `WithTx(ctx, db, fn func(Tx) error) error` that commits on success, rolls back on a returned error, and — via a deferred function — rolls back and re-panics if the body panicked; Commit and Rollback are never both called.
Test: a fake `Tx` recording calls; success commits once with no rollback; a returned error rolls back and propagates with no commit; a panic rolls back AND re-raises (caught by the test); a Commit error is surfaced; Commit and Rollback are never both called.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/txguard/cmd/demo
cd ~/go-exercises/txguard
go mod init example.com/txguard
go mod edit -go=1.26
```

### defer is the only path that survives a panic

The whole point of a transaction guard is that its cleanup runs no matter *how* the
body exits: a clean return, a returned error, or a panic. Only `defer` runs during
a panic unwind, so the rollback has to live in a deferred function. Three exit
paths, one deferred guard:

- *Success* — `fn` returns nil, `Commit` is attempted. The deferred function must
  then do nothing (no rollback after a commit).
- *Returned error* — `fn` returns non-nil, `WithTx` returns it, and the deferred
  function rolls back. `Commit` is never called.
- *Panic* — `fn` panics; the deferred function's `recover` sees it, rolls back to
  release the transaction, then **re-panics** with the original value so the fault
  still propagates (a genuine bug must not be silently turned into a committed or
  swallowed operation).

The invariant "`Commit` and `Rollback` are never both called" needs a single flag.
Once the code reaches the commit attempt, it sets `done = true`; the deferred
function only rolls back when `done` is false. So even a *failing* Commit does not
trigger a rollback — the transaction is already in the driver's hands. Rolling back
a returned rollback error is joined onto the original with `errors.Join` so neither
error is lost.

Create `txguard.go`:

```go
package txguard

import (
	"context"
	"errors"
	"fmt"
)

// Tx is the minimal transaction surface, modeled on database/sql.Tx.
type Tx interface {
	Commit() error
	Rollback() error
}

// DB begins transactions.
type DB interface {
	Begin(ctx context.Context) (Tx, error)
}

// WithTx runs fn inside a transaction. It commits on success, rolls back on a
// returned error, and — because the rollback is deferred — rolls back and
// re-panics if fn panics. Commit and Rollback are never both called.
func WithTx(ctx context.Context, db DB, fn func(Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// done becomes true once the commit has been attempted; it gates the deferred
	// rollback so a commit is never followed by a rollback.
	done := false
	defer func() {
		if r := recover(); r != nil {
			if !done {
				_ = tx.Rollback()
			}
			panic(r) // re-panic: a genuine fault must still crash
		}
		if !done {
			if rbErr := tx.Rollback(); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
		}
	}()

	if err = fn(tx); err != nil {
		return err // done stays false -> deferred rollback
	}

	done = true // commit attempted; no rollback regardless of its outcome
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
```

### The runnable demo

The demo supplies a tiny in-memory `DB`/`Tx` (the interfaces are exported, so a
different package can implement them) and runs both a committing and a
rolling-back transaction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/txguard"
)

type memTx struct{ committed, rolledBack bool }

func (t *memTx) Commit() error   { t.committed = true; return nil }
func (t *memTx) Rollback() error { t.rolledBack = true; return nil }

type memDB struct{ last *memTx }

func (d *memDB) Begin(context.Context) (txguard.Tx, error) {
	d.last = &memTx{}
	return d.last, nil
}

func main() {
	ctx := context.Background()

	db := &memDB{}
	_ = txguard.WithTx(ctx, db, func(tx txguard.Tx) error { return nil })
	fmt.Printf("success:  committed=%t rolledBack=%t\n", db.last.committed, db.last.rolledBack)

	db2 := &memDB{}
	err := txguard.WithTx(ctx, db2, func(tx txguard.Tx) error { return errors.New("constraint violation") })
	fmt.Printf("failure:  committed=%t rolledBack=%t err=%v\n", db2.last.committed, db2.last.rolledBack, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
success:  committed=true rolledBack=false
failure:  committed=false rolledBack=true err=constraint violation
```

### Tests

The fake `Tx` counts calls so each case can assert exactly which of Commit/Rollback
ran. The panic case wraps `WithTx` in the test's own deferred recover to prove the
guard both rolled back *and* re-raised.

Create `txguard_test.go`:

```go
package txguard

import (
	"context"
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

type fakeDB struct {
	tx       *fakeTx
	beginErr error
}

func (d *fakeDB) Begin(context.Context) (Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

func neverBoth(t *testing.T, tx *fakeTx) {
	t.Helper()
	if tx.commits > 0 && tx.rollbacks > 0 {
		t.Fatalf("Commit and Rollback both called (commits=%d rollbacks=%d)", tx.commits, tx.rollbacks)
	}
}

func TestCommitOnSuccess(t *testing.T) {
	t.Parallel()
	tx := &fakeTx{}
	db := &fakeDB{tx: tx}

	err := WithTx(t.Context(), db, func(Tx) error { return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if tx.commits != 1 {
		t.Fatalf("commits = %d, want 1", tx.commits)
	}
	if tx.rollbacks != 0 {
		t.Fatalf("rollbacks = %d, want 0", tx.rollbacks)
	}
	neverBoth(t, tx)
}

func TestRollbackOnError(t *testing.T) {
	t.Parallel()
	tx := &fakeTx{}
	db := &fakeDB{tx: tx}
	sentinel := errors.New("constraint")

	err := WithTx(t.Context(), db, func(Tx) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if tx.commits != 0 {
		t.Fatalf("commits = %d, want 0", tx.commits)
	}
	if tx.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1", tx.rollbacks)
	}
	neverBoth(t, tx)
}

func TestRollbackAndRePanicOnPanic(t *testing.T) {
	t.Parallel()
	tx := &fakeTx{}
	db := &fakeDB{tx: tx}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic was swallowed; WithTx must re-panic")
			}
		}()
		_ = WithTx(t.Context(), db, func(Tx) error { panic("boom in tx") })
	}()

	if tx.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1 (rollback must run on panic)", tx.rollbacks)
	}
	if tx.commits != 0 {
		t.Fatalf("commits = %d, want 0", tx.commits)
	}
	neverBoth(t, tx)
}

func TestCommitErrorSurfaced(t *testing.T) {
	t.Parallel()
	commitErr := errors.New("commit failed")
	tx := &fakeTx{commitErr: commitErr}
	db := &fakeDB{tx: tx}

	err := WithTx(t.Context(), db, func(Tx) error { return nil })
	if !errors.Is(err, commitErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, commitErr)
	}
	// A failing commit must NOT trigger a rollback.
	if tx.rollbacks != 0 {
		t.Fatalf("rollbacks = %d, want 0 after a commit attempt", tx.rollbacks)
	}
	neverBoth(t, tx)
}

func TestBeginErrorPropagated(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("no connection")
	db := &fakeDB{beginErr: beginErr}

	err := WithTx(t.Context(), db, func(Tx) error { return nil })
	if !errors.Is(err, beginErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, beginErr)
	}
}
```

## Review

The guard is correct when the deferred rollback covers exactly the two failing
exits — a returned error and a panic — and stays out of the way of a successful
(or attempted) commit. The `done` flag is what enforces "Commit and Rollback are
never both called": once the commit is attempted, `done` is true and the deferred
function does nothing, so even a failed commit is not followed by a rollback. The
re-panic in the panic branch is the fail-fast half: the transaction is released,
but a genuine bug still crashes rather than being quietly turned into a rolled-back
success the caller never hears about. If the rollback were on the normal path
instead of deferred, `TestRollbackAndRePanicOnPanic` would show zero rollbacks —
the exact leak this pattern prevents. Run `go test -race`.

## Resources

- [`database/sql.Tx`](https://pkg.go.dev/database/sql#Tx) — the real Commit/Rollback surface this models.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — deferred cleanup during an unwind.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining a rollback error with the original.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-mutex-unlock-on-panic-poisoned-state.md](08-mutex-unlock-on-panic-poisoned-state.md)
