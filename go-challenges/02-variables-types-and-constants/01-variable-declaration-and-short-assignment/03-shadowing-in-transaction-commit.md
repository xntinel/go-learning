# Exercise 3: Shadowing the Error in a Transaction Commit Path

The most expensive declaration bug in backend Go is a shadowed error in a
transaction path: a `:=` inside an inner scope redeclares `err`, so a failed
`Commit` or `Rollback` returns `nil` and the caller believes data was persisted
when it was not. This exercise reproduces the bug, then fixes it with correct use
of `=` and immediate returns.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs (a fake DB and tx), and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
txn/                           independent module: example.com/txn
  go.mod                       module example.com/txn
  txn.go                       DB, Tx interfaces; WithTx(ctx, db, fn) error
  cmd/
    demo/
      main.go                  runs a commit that fails and prints the surfaced error
  txn_test.go                  commit-fails and step-fails-rollback cases
```

- Files: `txn.go`, `cmd/demo/main.go`, `txn_test.go`.
- Implement: `WithTx(ctx, db, fn)` that begins a tx, runs `fn`, and commits — without shadowing `err`, so a failed `Commit` or `Rollback` is surfaced.
- Test: inject a tx whose `Commit` fails and assert `WithTx` returns it; inject a step failure and assert the step error propagates while `Rollback` is invoked.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/txn/cmd/demo
cd ~/go-exercises/txn
go mod init example.com/txn
```

### The bug, stated precisely

Here is the classic shadowing mistake, the kind that ships to production and
silently loses writes:

```go
// WRONG: the commit error is shadowed and dropped.
func WithTx(ctx context.Context, db DB, fn func(Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil { // new err, scoped to this if
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil { // ALSO a new err, scoped to this if
		return err
	}
	return nil // unreachable-looking, but the point is the shape below
}
```

That particular shape happens to work because each inner `if` returns from its own
scope. The dangerous variant is when someone "tidies" the commit into a deferred
cleanup and reads a shadowed `err`:

```go
// WRONG: err in the defer is the outer err, but the inner := never touched it.
func WithTx(ctx context.Context, db DB, fn func(Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil { // outer err
			_ = tx.Rollback(ctx)
			return
		}
		err = tx.Commit(ctx)
	}()
	if res, err := fn(tx); err != nil { // res, err are NEW; outer err stays nil
		_ = res
		return err // returns, but the outer named err the defer reads is still nil
	}
	return nil
}
```

The inner `res, err := fn(tx)` declares a fresh `err` because it introduces the new
name `res`, so the assignment does not touch the named return `err` the deferred
closure inspects. When `fn` fails, the `return err` does hand back the inner error,
but any code path that relies on the named `err` being set (a metric, a log, a
second deferred stage) sees `nil`. The rule from the concepts file bites here: in a
multi-name `:=`, if any name is new, the whole statement declares — it does not
reassign the existing `err`.

### The fix

Declare the destination once and use `=` for reassignments, so every write lands
on the same `err` the deferred cleanup reads. Where a value is genuinely local
(the tx handle from `Begin`), a `:=` is fine because there is no outer variable of
that name to shadow.

Create `txn.go`:

```go
package txn

import (
	"context"
	"errors"
	"fmt"
)

// Tx is a minimal transaction handle.
type Tx interface {
	Exec(ctx context.Context, stmt string) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// DB opens transactions.
type DB interface {
	Begin(ctx context.Context) (Tx, error)
}

// WithTx runs fn inside a transaction. If fn fails, the tx is rolled back and
// fn's error is returned. If Commit fails, that error is surfaced. The single
// err destination means no branch can silently shadow a failure.
func WithTx(ctx context.Context, db DB, fn func(Tx) error) (err error) {
	tx, beginErr := db.Begin(ctx)
	if beginErr != nil {
		return fmt.Errorf("begin: %w", beginErr)
	}

	// Reassign the named err (=, not :=) so the deferred cleanup sees the truth.
	if err = fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
```

Note the shape that keeps the bug away: `beginErr` and `rbErr` have distinct names
precisely so no reader mistakes them for the outer `err`, and every write to the
result uses `err = ...` on the single named return. No `:=` reintroduces `err` in
an inner scope.

### The runnable demo

The demo uses a tiny in-memory DB whose `Commit` is rigged to fail, so you can see
`WithTx` surface the commit error instead of returning `nil`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/txn"
)

type fakeTx struct{ commitErr error }

func (t *fakeTx) Exec(context.Context, string) error { return nil }
func (t *fakeTx) Commit(context.Context) error       { return t.commitErr }
func (t *fakeTx) Rollback(context.Context) error     { return nil }

type fakeDB struct{ commitErr error }

func (d *fakeDB) Begin(context.Context) (txn.Tx, error) {
	return &fakeTx{commitErr: d.commitErr}, nil
}

func main() {
	db := &fakeDB{commitErr: errors.New("connection reset")}
	err := txn.WithTx(context.Background(), db, func(tx txn.Tx) error {
		return tx.Exec(context.Background(), "INSERT ...")
	})
	fmt.Printf("WithTx returned: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
WithTx returned: commit: connection reset
```

The buggy version would print `WithTx returned: <nil>` here — the whole point.

### Tests

The tests inject failures at the two dangerous points: a failing `Commit` and a
failing step that triggers `Rollback`. Both assert the error is surfaced and, for
rollback, that it was actually invoked.

Create `txn_test.go`:

```go
package txn

import (
	"context"
	"errors"
	"testing"
)

type spyTx struct {
	execErr     error
	commitErr   error
	rollbackErr error
	committed   bool
	rolledBack  bool
}

func (t *spyTx) Exec(context.Context, string) error { return t.execErr }
func (t *spyTx) Commit(context.Context) error {
	t.committed = true
	return t.commitErr
}
func (t *spyTx) Rollback(context.Context) error {
	t.rolledBack = true
	return t.rollbackErr
}

type spyDB struct {
	tx       *spyTx
	beginErr error
}

func (d *spyDB) Begin(context.Context) (Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

func TestWithTxSurfacesCommitError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("commit exploded")
	tx := &spyTx{commitErr: wantErr}
	db := &spyDB{tx: tx}

	err := WithTx(context.Background(), db, func(Tx) error { return nil })

	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTx err = %v, want it to wrap %v", err, wantErr)
	}
	if !tx.committed {
		t.Fatal("Commit was never attempted")
	}
	if tx.rolledBack {
		t.Fatal("Rollback should not run when the step succeeded")
	}
}

func TestWithTxRollsBackOnStepFailure(t *testing.T) {
	t.Parallel()
	stepErr := errors.New("insert failed")
	tx := &spyTx{}
	db := &spyDB{tx: tx}

	err := WithTx(context.Background(), db, func(Tx) error { return stepErr })

	if !errors.Is(err, stepErr) {
		t.Fatalf("WithTx err = %v, want step error %v", err, stepErr)
	}
	if !tx.rolledBack {
		t.Fatal("Rollback should run when the step fails")
	}
	if tx.committed {
		t.Fatal("Commit must not run after a failed step")
	}
}

func TestWithTxJoinsRollbackError(t *testing.T) {
	t.Parallel()
	stepErr := errors.New("step failed")
	rbErr := errors.New("rollback failed")
	tx := &spyTx{rollbackErr: rbErr}
	db := &spyDB{tx: tx}

	err := WithTx(context.Background(), db, func(Tx) error { return stepErr })

	if !errors.Is(err, stepErr) {
		t.Fatalf("err lost the step error: %v", err)
	}
	if !errors.Is(err, rbErr) {
		t.Fatalf("err lost the rollback error: %v", err)
	}
}

func TestWithTxWrapsBeginError(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("no connections")
	db := &spyDB{beginErr: beginErr}

	err := WithTx(context.Background(), db, func(Tx) error { return nil })
	if !errors.Is(err, beginErr) {
		t.Fatalf("WithTx err = %v, want begin error", err)
	}
}
```

`TestWithTxJoinsRollbackError` proves the `errors.Join` branch: when both the step
and the rollback fail, the returned error matches *both* sentinels, so nothing is
silently dropped.

## Review

`WithTx` is correct when there is exactly one error destination and every write
lands on it with `=`. The distinct helper names (`beginErr`, `rbErr`) exist so no
inner `:=` reintroduces `err` and desynchronizes it from what the caller and the
deferred cleanup observe. The commit-failure test is the direct guard against the
shadowing bug: the buggy variant returns `nil` there.

The mistakes to avoid: an inner `res, err := fn(tx)` declares a new `err` (because
`res` is new), so a step failure would not update the outer/named `err`; and
dropping the rollback error loses the reason a rollback itself failed. Run
`go vet` and, in review, the shadow analyzer (`go vet -vettool=$(which shadow)`) to
catch redeclarations. Run `go test -race` to confirm the paths.

## Resources

- [Go Specification: Short variable declarations (redeclaration rules)](https://go.dev/ref/spec#Short_variable_declarations)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [golang.org/x/tools shadow analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/shadow)
- [database/sql: Tx.Commit / Tx.Rollback semantics](https://pkg.go.dev/database/sql#Tx)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-sentinel-errors-repository-layer.md](02-sentinel-errors-repository-layer.md) | Next: [04-comma-ok-cache-lookup.md](04-comma-ok-cache-lookup.md)
