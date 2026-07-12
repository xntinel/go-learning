# Exercise 4: Repository Write — Named Return Driving Deferred Rollback

The canonical durable-storage cleanup: a repository `Save` that begins a
transaction and, in a *single* deferred closure keyed on the named return `err`,
rolls back on failure and commits on success — surfacing the commit or rollback
error back into `err`. This is the pattern you write on top of `database/sql`
every time a handler mutates more than one row, and getting the named-return
mechanics right is what makes it correct across the error, success, and panic
paths.

This module is fully self-contained. It uses a small fake transaction interface so
no real database is needed, but the interface mirrors `database/sql`'s `Tx`
(`Exec`, `Commit`, `Rollback`) exactly, so the shape transfers directly.

## What you'll build

```text
repowrite/                   independent module: example.com/repowrite
  go.mod                     module example.com/repowrite
  repowrite.go               DB/Tx interfaces, Repo.Save (named-return deferred commit/rollback)
  cmd/
    demo/
      main.go                runnable demo: a successful save and a failing one
  repowrite_test.go          commit path, rollback-preserves-error, commit-err, rollback-err joined
```

- Files: `repowrite.go`, `cmd/demo/main.go`, `repowrite_test.go`.
- Implement: `Repo.Save(ctx, User) (err error)` that begins a tx and defers a closure reading the named `err`: on `err != nil` it rolls back (joining any rollback error), otherwise it commits (propagating any commit error into `err`).
- Test (fake `Tx`): happy path commits and `err` stays nil; a mid-write error triggers rollback and preserves the original error; a `Commit` error is propagated; a rollback error is joined with the original, not swallowed.
- Verify: `go test -count=1 -race ./...`

### Why the closure form is mandatory here

The deferred cleanup must *decide* between commit and rollback based on how the
function ended, and it must be able to *change* the returned error. Both require
the closure form over a named return. Consider the two things this closure needs:

First, it reads `err` at return time. `defer tx.Rollback()` is wrong because it
always rolls back, even on success. `defer func() { if err != nil { tx.Rollback() } else { tx.Commit() } }()`
reads the *final* value of the named `err` — the value set by the last `return` —
so it correctly branches. This only works because `err` is a *named* result: the
closure closes over the result variable, not a copy. If you wrote the write logic
with a shadowed local (`if err := tx.Exec(...); err != nil`), the closure would
read the function's `err`, which stayed nil, and it would commit a failed
transaction. Every assignment to the error inside `Save` uses `err =`, never
`err :=`, precisely to keep the closure and the function looking at the same
variable.

Second, it writes `err`. A `Commit` can fail (a deadlock, a serialization
conflict, a lost connection at the worst moment), and that failure must reach the
caller — a `Save` that returns nil while the commit failed is a data-loss bug.
So the closure assigns the commit error into `err`. And a `Rollback` can fail
*after* the primary operation already failed; swallowing that leaves a broken
transaction the operator never sees, so the closure joins the rollback error onto
the original with `errors.Join` rather than discarding either.

Create `repowrite.go`:

```go
package repowrite

import (
	"context"
	"errors"
	"fmt"
)

// Tx mirrors the subset of database/sql.Tx this repository uses. A real
// implementation is *sql.Tx; the tests supply a fake.
type Tx interface {
	Exec(ctx context.Context, query string, args ...any) error
	Commit() error
	Rollback() error
}

// DB begins a transaction. In production this is a thin wrapper over *sql.DB
// whose BeginTx returns a *sql.Tx.
type DB interface {
	BeginTx(ctx context.Context) (Tx, error)
}

// User is the row being written.
type User struct {
	ID   int
	Name string
}

// Repo writes users transactionally.
type Repo struct {
	db DB
}

func NewRepo(db DB) *Repo {
	return &Repo{db: db}
}

// Save inserts a user and an audit row in one transaction. The single deferred
// closure keyed on the NAMED return err is the whole lesson: it commits on
// success, rolls back on failure, and never hides a commit or rollback error.
func (r *Repo) Save(ctx context.Context, u User) (err error) {
	tx, err := r.db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	defer func() {
		if err != nil {
			// The primary work failed. Roll back, and if the rollback ALSO
			// fails, surface both — a broken transaction must be visible.
			if rbErr := tx.Rollback(); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
			return
		}
		// The work succeeded. Commit, and propagate a commit failure into the
		// named return so the caller never sees a false success.
		if cErr := tx.Commit(); cErr != nil {
			err = fmt.Errorf("commit: %w", cErr)
		}
	}()

	if err = tx.Exec(ctx, "INSERT INTO users(id, name) VALUES(?, ?)", u.ID, u.Name); err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	if err = tx.Exec(ctx, "INSERT INTO audit(user_id) VALUES(?)", u.ID); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}
```

### The runnable demo

The demo uses a tiny in-memory `DB`/`Tx` so it runs with no database, and shows a
successful save (commit) and a failing one (rollback).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/repowrite"
)

// fakeTx records what happened and can be told to fail a specific step.
type fakeTx struct {
	failExec bool
	log      *[]string
}

func (f *fakeTx) Exec(ctx context.Context, query string, args ...any) error {
	if f.failExec {
		return errors.New("write conflict")
	}
	*f.log = append(*f.log, "exec")
	return nil
}
func (f *fakeTx) Commit() error   { *f.log = append(*f.log, "commit"); return nil }
func (f *fakeTx) Rollback() error { *f.log = append(*f.log, "rollback"); return nil }

type fakeDB struct {
	failExec bool
	log      *[]string
}

func (d *fakeDB) BeginTx(ctx context.Context) (repowrite.Tx, error) {
	return &fakeTx{failExec: d.failExec, log: d.log}, nil
}

func main() {
	ctx := context.Background()

	var okLog []string
	repo := repowrite.NewRepo(&fakeDB{log: &okLog})
	if err := repo.Save(ctx, repowrite.User{ID: 1, Name: "alice"}); err != nil {
		fmt.Println("save:", err)
	}
	fmt.Printf("success path: %v\n", okLog)

	var failLog []string
	repoFail := repowrite.NewRepo(&fakeDB{failExec: true, log: &failLog})
	err := repoFail.Save(ctx, repowrite.User{ID: 2, Name: "bob"})
	fmt.Printf("failure path: %v, err=%v\n", failLog, err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
success path: [exec exec commit]
failure path: [rollback], err=true
```

### Tests

The tests use a configurable fake `Tx` to drive each branch. `TestHappyPathCommits`
proves success commits and `err` is nil. `TestWriteErrorRollsBack` proves a
mid-write failure rolls back and the *original* error survives.
`TestCommitErrorPropagates` proves a commit failure becomes the returned error.
`TestRollbackErrorIsJoined` proves a rollback failure after a write failure surfaces
*both* errors via `errors.Join`.

Create `repowrite_test.go`:

```go
package repowrite

import (
	"context"
	"errors"
	"testing"
)

var (
	errWrite    = errors.New("write failed")
	errCommit   = errors.New("commit failed")
	errRollback = errors.New("rollback failed")
)

// fakeTx is a configurable transaction double.
type fakeTx struct {
	execErr     error
	commitErr   error
	rollbackErr error
	committed   bool
	rolledBack  bool
}

func (f *fakeTx) Exec(ctx context.Context, query string, args ...any) error {
	return f.execErr
}
func (f *fakeTx) Commit() error {
	f.committed = true
	return f.commitErr
}
func (f *fakeTx) Rollback() error {
	f.rolledBack = true
	return f.rollbackErr
}

type fakeDB struct{ tx *fakeTx }

func (d *fakeDB) BeginTx(ctx context.Context) (Tx, error) { return d.tx, nil }

func TestHappyPathCommits(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{}
	repo := NewRepo(&fakeDB{tx: tx})

	if err := repo.Save(t.Context(), User{ID: 1, Name: "alice"}); err != nil {
		t.Fatalf("Save() = %v, want nil", err)
	}
	if !tx.committed {
		t.Fatal("expected Commit to be called")
	}
	if tx.rolledBack {
		t.Fatal("Rollback should not be called on success")
	}
}

func TestWriteErrorRollsBack(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{execErr: errWrite}
	repo := NewRepo(&fakeDB{tx: tx})

	err := repo.Save(t.Context(), User{ID: 2, Name: "bob"})
	if !errors.Is(err, errWrite) {
		t.Fatalf("Save() = %v, want to wrap errWrite", err)
	}
	if !tx.rolledBack {
		t.Fatal("expected Rollback to be called on write failure")
	}
	if tx.committed {
		t.Fatal("Commit must not be called after a write failure")
	}
}

func TestCommitErrorPropagates(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{commitErr: errCommit}
	repo := NewRepo(&fakeDB{tx: tx})

	err := repo.Save(t.Context(), User{ID: 3, Name: "carol"})
	if !errors.Is(err, errCommit) {
		t.Fatalf("Save() = %v, want to wrap errCommit", err)
	}
}

func TestRollbackErrorIsJoined(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{execErr: errWrite, rollbackErr: errRollback}
	repo := NewRepo(&fakeDB{tx: tx})

	err := repo.Save(t.Context(), User{ID: 4, Name: "dave"})
	// Both the original write error and the rollback failure must be visible.
	if !errors.Is(err, errWrite) {
		t.Errorf("Save() = %v, want to wrap errWrite", err)
	}
	if !errors.Is(err, errRollback) {
		t.Errorf("Save() = %v, want to wrap errRollback", err)
	}
}
```

## Review

The write path is correct when the returned `err` is the single source of truth
about the outcome and the deferred closure derives its commit/rollback decision
from it. The two failure modes this exercise drills are the ones that bite in
production: a `Commit` error swallowed into a false nil return
(`TestCommitErrorPropagates` guards it) and a `Rollback` error discarded on the
failure path so a broken transaction goes unnoticed (`TestRollbackErrorIsJoined`
guards it with `errors.Join`). The mechanical rule to carry away: name the return
`err`, assign to it with `err =` throughout (never shadow with `err :=` in a block
whose value the closure needs), and let one deferred closure own the commit-or-
rollback decision. On a real `*sql.Tx`, `Rollback` after a successful `Commit`
returns `sql.ErrTxDone`, which is why the branch structure — commit *xor* rollback,
never both — matters.

## Resources

- [`database/sql`](https://pkg.go.dev/database/sql) — `DB.BeginTx`, `Tx.Commit`, `Tx.Rollback`, and `ErrTxDone`.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining the primary error with a rollback failure.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — deferred closures observing and modifying named returns.
- [`context.Context`](https://pkg.go.dev/context) — carried into `BeginTx` and every `Exec`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-defer-loop-fd-leak.md](03-defer-loop-fd-leak.md) | Next: [05-deferred-close-error-capture.md](05-deferred-close-error-capture.md)
