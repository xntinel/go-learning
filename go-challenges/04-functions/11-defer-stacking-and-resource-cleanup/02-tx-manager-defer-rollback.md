# Exercise 2: Transaction Helper — defer Rollback, Commit on the Happy Path

`WithTx(ctx, db, fn)` is the single most common `defer` pattern in production Go
that touches SQL: begin a transaction, run the caller's work, and use one
deferred closure keyed on a named return error to roll back on failure or commit
on success — never both, never neither. This module builds it against small
interfaces so the whole thing tests with an in-memory fake and no real database.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
txm/                        independent module: example.com/txm
  go.mod
  tx/tx.go                  Tx, Beginner interfaces; SQLBeginner adapter; WithTx
  cmd/demo/main.go          in-memory Beginner showing commit and rollback paths
  tx/tx_test.go             table tests over a fake tx: commit, rollback, panic, commit-fail
```

- Files: `tx/tx.go`, `cmd/demo/main.go`, `tx/tx_test.go`.
- Implement: `WithTx(ctx, db Beginner, opts *sql.TxOptions, fn func(Tx) error) (err error)` that begins a tx, runs `fn`, and in a deferred closure over the named `err` rolls back on error or panic (re-raising the panic) and commits on success (surfacing the commit error).
- Test: `fn` returns nil (commit, no rollback); `fn` returns an error (rollback, error propagated, no commit); `fn` panics (rollback then re-panic); `Commit` fails (error surfaced). Assert the exactly-one-of {commit, rollback} invariant on every path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/txm/tx ~/go-exercises/txm/cmd/demo
cd ~/go-exercises/txm
go mod init example.com/txm
```

### Interfaces, so the test needs no database

The real types are `*sql.DB` and `*sql.Tx`. But `*sql.DB.BeginTx` returns a
`*sql.Tx`, and you cannot construct a usable `*sql.Tx` without a live driver — so
testing against the concrete types would require a database. Instead, define the
tiny behavioral interfaces the helper actually uses:

- `Tx` is `{ Commit() error; Rollback() error }`. A real `*sql.Tx` already
  satisfies it — both methods exist with those exact signatures — so no
  adaptation is needed on the transaction side.
- `Beginner` is `{ BeginTx(ctx, *sql.TxOptions) (Tx, error) }`. A real `*sql.DB`
  does *not* satisfy this directly, because its `BeginTx` returns the concrete
  `*sql.Tx`, not the `Tx` interface. So a one-line adapter, `SQLBeginner`, wraps a
  `*sql.DB` and widens the return to the interface. That adapter is the only place
  the real `database/sql` types appear, and returning a `*sql.Tx` where a `Tx` is
  expected is a valid implicit interface conversion.

With these interfaces, the test injects a fake `Beginner` whose `Tx` records how
many times `Commit` and `Rollback` were called, and asserts the invariant without
any I/O.

### The deferred closure keyed on the named return

The whole helper is a `defer` over a named return `err`. There are three exit
conditions the closure must distinguish:

- A panic unwinding the stack. `recover()` returns non-nil; roll back so the
  half-finished transaction does not leak, then re-`panic` the same value so the
  panic is not silently swallowed — the caller asked for a transaction wrapper,
  not a panic firewall.
- A normal error return. `err` is non-nil and there was no panic; roll back. If
  the rollback itself fails, join that error onto the original so neither is lost.
- Success. `err` is nil; commit. If the commit fails, that failure *becomes* the
  returned error — a transaction whose commit failed did not happen, and the
  caller must learn that.

Ordering inside the closure matters: check `recover()` first (a panic is not an
ordinary `err`), then branch on `err`. This structure guarantees the exactly-one-of
invariant mechanically: exactly one of `Commit`/`Rollback` runs on the panic path
and on the error path (rollback) and on the success path (commit).

Create `tx/tx.go`:

```go
package tx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tx is the subset of *sql.Tx that WithTx drives. A real *sql.Tx satisfies it.
type Tx interface {
	Commit() error
	Rollback() error
}

// Beginner starts a transaction. Wrap a *sql.DB with SQLBeginner to satisfy it.
type Beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error)
}

// SQLBeginner adapts a *sql.DB (whose BeginTx returns the concrete *sql.Tx) to
// the Beginner interface (whose BeginTx returns the Tx interface).
type SQLBeginner struct{ DB *sql.DB }

func (b SQLBeginner) BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	return b.DB.BeginTx(ctx, opts)
}

// WithTx runs fn inside a transaction. It commits if fn returns nil, rolls back
// if fn returns an error, and rolls back then re-panics if fn panics. Exactly
// one of Commit/Rollback runs on every path.
func WithTx(ctx context.Context, db Beginner, opts *sql.TxOptions, fn func(Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			// A panic is not an ordinary error: roll back, then re-raise so the
			// panic is never swallowed.
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
			return
		}
		if cErr := tx.Commit(); cErr != nil {
			err = fmt.Errorf("commit: %w", cErr)
		}
	}()

	return fn(tx)
}
```

### The runnable demo

The demo defines its own in-memory `Tx` and `Beginner` in `package main` — legal
because both interfaces are exported — so it can show the commit and rollback
paths with a call log and no database.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"example.com/txm/tx"
)

type memTx struct{ log *[]string }

func (t memTx) Commit() error   { *t.log = append(*t.log, "commit"); return nil }
func (t memTx) Rollback() error { *t.log = append(*t.log, "rollback"); return nil }

type memDB struct{ log *[]string }

func (d memDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (tx.Tx, error) {
	*d.log = append(*d.log, "begin")
	return memTx{log: d.log}, nil
}

func main() {
	var log []string
	db := memDB{log: &log}

	err := tx.WithTx(context.Background(), db, nil, func(t tx.Tx) error {
		return nil // happy path
	})
	fmt.Println("happy:", log, "err:", err)

	log = nil
	err = tx.WithTx(context.Background(), db, nil, func(t tx.Tx) error {
		return errors.New("insert failed")
	})
	fmt.Println("failure:", log, "err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
happy: [begin commit] err: <nil>
failure: [begin rollback] err: insert failed
```

### Tests

The table drives a fake `Tx` that counts `Commit` and `Rollback` and can be
configured to fail either. The panic case lives in its own test because it must
recover the re-raised panic. Every case asserts the exactly-one-of invariant.

Create `tx/tx_test.go`:

```go
package tx

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

type fakeTx struct {
	commits     int
	rollbacks   int
	commitErr   error
	rollbackErr error
}

func (f *fakeTx) Commit() error   { f.commits++; return f.commitErr }
func (f *fakeTx) Rollback() error { f.rollbacks++; return f.rollbackErr }

type fakeDB struct {
	tx       *fakeTx
	beginErr error
}

func (d *fakeDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

var errWork = errors.New("work failed")

func TestWithTx(t *testing.T) {
	t.Parallel()

	commitBoom := errors.New("commit boom")

	tests := []struct {
		name          string
		fn            func(Tx) error
		commitErr     error
		wantCommits   int
		wantRollbacks int
		wantErrIs     error
	}{
		{
			name:          "success commits",
			fn:            func(Tx) error { return nil },
			wantCommits:   1,
			wantRollbacks: 0,
			wantErrIs:     nil,
		},
		{
			name:          "error rolls back",
			fn:            func(Tx) error { return errWork },
			wantCommits:   0,
			wantRollbacks: 1,
			wantErrIs:     errWork,
		},
		{
			name:          "commit failure surfaces",
			fn:            func(Tx) error { return nil },
			commitErr:     commitBoom,
			wantCommits:   1,
			wantRollbacks: 0,
			wantErrIs:     commitBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ftx := &fakeTx{commitErr: tt.commitErr}
			db := &fakeDB{tx: ftx}

			err := WithTx(context.Background(), db, nil, tt.fn)

			if tt.wantErrIs == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErrIs)
			}
			if ftx.commits != tt.wantCommits {
				t.Errorf("commits = %d, want %d", ftx.commits, tt.wantCommits)
			}
			if ftx.rollbacks != tt.wantRollbacks {
				t.Errorf("rollbacks = %d, want %d", ftx.rollbacks, tt.wantRollbacks)
			}
			// Exactly-one-of invariant.
			if ftx.commits+ftx.rollbacks != 1 {
				t.Errorf("commits+rollbacks = %d, want exactly 1", ftx.commits+ftx.rollbacks)
			}
		})
	}
}

func TestWithTxPanicRollsBackAndReRaises(t *testing.T) {
	t.Parallel()

	ftx := &fakeTx{}
	db := &fakeDB{tx: ftx}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate")
		}
		if got, ok := r.(string); !ok || got != "boom" {
			t.Fatalf("recovered %v, want \"boom\"", r)
		}
		if ftx.rollbacks != 1 {
			t.Errorf("rollbacks = %d, want 1", ftx.rollbacks)
		}
		if ftx.commits != 0 {
			t.Errorf("commits = %d, want 0", ftx.commits)
		}
	}()

	_ = WithTx(context.Background(), db, nil, func(Tx) error {
		panic("boom")
	})
}

func TestWithTxBeginError(t *testing.T) {
	t.Parallel()

	beginErr := errors.New("cannot connect")
	db := &fakeDB{beginErr: beginErr}

	ran := false
	err := WithTx(context.Background(), db, nil, func(Tx) error {
		ran = true
		return nil
	})
	if !errors.Is(err, beginErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, beginErr)
	}
	if ran {
		t.Fatal("fn ran despite begin failure")
	}
}
```

## Review

The helper is correct when the exactly-one-of invariant holds on all four paths:
success commits and never rolls back; an error return rolls back and never
commits; a panic rolls back, re-raises, and never commits; a begin failure runs
neither and returns the wrapped begin error. The subtlety that trips people is the
ordering inside the deferred closure — you must consult `recover()` before you
branch on `err`, because a panic leaves `err` at its zero value (nil) and would
otherwise be misread as success and *committed*. Commit's error must overwrite (or
join) the returned error; a helper that commits and throws away the commit error
reports success for a transaction that never durably happened, the worst failure
mode of all. Run `go test -race`; the fake counters make every path's
commit/rollback count directly assertable.

## Resources

- [database/sql: Tx (Begin, Commit, Rollback)](https://pkg.go.dev/database/sql#Tx)
- [database/sql: DB.BeginTx and TxOptions](https://pkg.go.dev/database/sql#DB.BeginTx)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-connection-pool-defer-return.md](01-connection-pool-defer-return.md) | Next: [03-defer-close-error-into-named-return.md](03-defer-close-error-into-named-return.md)
