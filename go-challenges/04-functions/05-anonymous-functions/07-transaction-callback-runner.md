# Exercise 7: Transaction Boundary that Takes the Unit of Work as an Anonymous Function

The standard Go database transaction idiom hands the unit of work to a runner as a
function literal: `WithinTx(ctx, db, func(tx *Tx) error { ... })`. The caller's
literal captures local request data and describes *what* to do; the runner owns the
transaction boundary — begin, defer rollback, commit on success, rollback on error
or panic. This module builds that runner against a fake `Tx` so no real driver is
needed.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
txn/                          module example.com/txn
  go.mod
  txn.go                      DB, Tx (fake), WithinTx(ctx, db, fn) with begin/commit/rollback
  txn_test.go                 commit, rollback-on-error, rollback-on-panic, joined rollback error
  cmd/demo/main.go            one committing and one rolling-back unit of work
```

- Files: `txn.go`, `txn_test.go`, `cmd/demo/main.go`.
- Implement: `WithinTx(ctx, db, fn func(tx *Tx) error) error` that begins a tx, defers rollback guarded by a committed flag, commits on a nil error, and rolls back (joining errors) on error or panic.
- Test: success commits and applies the writes; a returned error rolls back with no commit; a panicking callback rolls back and surfaces `ErrPanic`; a rollback that itself errors is joined with the original error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/txn/cmd/demo
cd ~/go-exercises/txn
go mod init example.com/txn
```

### The runner owns the boundary; the literal owns the work

`WithinTx` is the inversion at the heart of callback-style APIs. The caller passes a
`func(tx *Tx) error` literal that stages writes on the transaction; the runner
guarantees the transaction is always resolved exactly once. The mechanism is a
deferred closure plus a `committed` flag:

- Begin the transaction.
- `defer` a closure that, unless `committed` is already true, calls `recover()` to
  turn a panic into an error, then rolls back and joins any rollback error with the
  in-flight error.
- Run `fn(tx)`; on error, return it (the defer rolls back).
- Commit; on error, return it (the defer rolls back).
- Set `committed = true` and return nil (the defer sees the flag and does nothing).

The `committed` flag is what distinguishes "we finished and committed" from "we
must roll back": without it, the defer would try to roll back a committed
transaction. `recover()` sits directly in the deferred literal — the only place it
works — so a panic in the caller's unit of work becomes a returned error and a
rollback rather than a crashed goroutine. `errors.Join` compounds a rollback error
with the original so neither is lost. The `Tx` here is a fake that records
`begins`/`commits`/`rollbacks` and stages writes in a map, applied to the store only
on commit — enough to test the boundary without a driver.

Create `txn.go`:

```go
package txn

import (
	"context"
	"errors"
	"fmt"
)

// ErrPanic marks a unit of work that panicked and was recovered.
var ErrPanic = errors.New("unit of work panicked")

// ErrRollback marks a rollback that itself failed.
var ErrRollback = errors.New("rollback failed")

// DB is a fake store that records transaction lifecycle calls.
type DB struct {
	store        map[string]string
	begins       int
	commits      int
	rollbacks    int
	failCommit   error
	failRollback error
}

// NewDB returns an empty fake DB.
func NewDB() *DB { return &DB{store: map[string]string{}} }

// Get returns the committed value for key.
func (db *DB) Get(key string) (string, bool) {
	v, ok := db.store[key]
	return v, ok
}

// Tx is a fake transaction that stages writes until commit.
type Tx struct {
	db     *DB
	staged map[string]string
}

// Set stages a write; it is applied to the store only on commit.
func (tx *Tx) Set(key, val string) { tx.staged[key] = val }

func (db *DB) begin() *Tx {
	db.begins++
	return &Tx{db: db, staged: map[string]string{}}
}

func (tx *Tx) commit() error {
	if tx.db.failCommit != nil {
		return tx.db.failCommit
	}
	for k, v := range tx.staged {
		tx.db.store[k] = v
	}
	tx.db.commits++
	return nil
}

func (tx *Tx) rollback() error {
	tx.db.rollbacks++
	return tx.db.failRollback // staged writes are simply discarded
}

// WithinTx runs fn inside a transaction. It commits if fn returns nil, and rolls
// back on error or panic. A rollback error is joined with the original.
func WithinTx(ctx context.Context, db *DB, fn func(tx *Tx) error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	tx := db.begin()
	committed := false
	defer func() {
		if committed {
			return
		}
		if p := recover(); p != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, p)
		}
		if rbErr := tx.rollback(); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: %w", ErrRollback, rbErr))
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.commit(); err != nil {
		return err
	}
	committed = true
	return nil
}
```

### The runnable demo

The demo runs one unit of work that commits and one that returns an error and rolls
back, showing the store reflects only the committed writes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/txn"
)

func main() {
	db := txn.NewDB()

	err := txn.WithinTx(context.Background(), db, func(tx *txn.Tx) error {
		tx.Set("user:1", "alice")
		tx.Set("user:2", "bob")
		return nil
	})
	fmt.Println("commit err:", err)
	v, _ := db.Get("user:1")
	fmt.Println("user:1 =", v)

	err = txn.WithinTx(context.Background(), db, func(tx *txn.Tx) error {
		tx.Set("user:3", "carol")
		return errors.New("business rule violated")
	})
	fmt.Println("rolled back:", err != nil)
	_, ok := db.Get("user:3")
	fmt.Println("user:3 present:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
commit err: <nil>
user:1 = alice
rolled back: true
user:3 present: false
```

### Tests

The tests assert the boundary against the fake's counters. `TestCommitAppliesWrites`
confirms a successful unit commits once, never rolls back, and the writes land.
`TestErrorRollsBack` confirms a returned error rolls back, never commits, and leaves
the store untouched. `TestPanicRollsBack` confirms a panicking unit surfaces
`ErrPanic` and rolls back. `TestRollbackErrorJoined` sets the fake to fail rollback
and confirms both the original error and `ErrRollback` are present in the joined
result.

Create `txn_test.go`:

```go
package txn

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCommitAppliesWrites(t *testing.T) {
	t.Parallel()
	db := NewDB()
	err := WithinTx(context.Background(), db, func(tx *Tx) error {
		tx.Set("k", "v")
		return nil
	})
	if err != nil {
		t.Fatalf("WithinTx = %v, want nil", err)
	}
	if v, ok := db.Get("k"); !ok || v != "v" {
		t.Fatalf("Get(k) = %q,%v; want v,true", v, ok)
	}
	if db.commits != 1 || db.rollbacks != 0 {
		t.Fatalf("commits=%d rollbacks=%d, want 1 and 0", db.commits, db.rollbacks)
	}
}

func TestErrorRollsBack(t *testing.T) {
	t.Parallel()
	db := NewDB()
	sentinel := errors.New("business rule violated")
	err := WithinTx(context.Background(), db, func(tx *Tx) error {
		tx.Set("k", "v")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithinTx = %v, want wrapping sentinel", err)
	}
	if _, ok := db.Get("k"); ok {
		t.Fatal("write was applied despite rollback")
	}
	if db.commits != 0 || db.rollbacks != 1 {
		t.Fatalf("commits=%d rollbacks=%d, want 0 and 1", db.commits, db.rollbacks)
	}
}

func TestPanicRollsBack(t *testing.T) {
	t.Parallel()
	db := NewDB()
	err := WithinTx(context.Background(), db, func(tx *Tx) error {
		tx.Set("k", "v")
		panic("driver segfault")
	})
	if !errors.Is(err, ErrPanic) {
		t.Fatalf("WithinTx = %v, want wrapping ErrPanic", err)
	}
	if db.commits != 0 || db.rollbacks != 1 {
		t.Fatalf("commits=%d rollbacks=%d, want 0 and 1", db.commits, db.rollbacks)
	}
}

func TestRollbackErrorJoined(t *testing.T) {
	t.Parallel()
	db := NewDB()
	db.failRollback = errors.New("connection lost")
	original := errors.New("business rule violated")

	err := WithinTx(context.Background(), db, func(tx *Tx) error {
		return original
	})
	if !errors.Is(err, original) {
		t.Fatalf("joined error lost the original: %v", err)
	}
	if !errors.Is(err, ErrRollback) {
		t.Fatalf("joined error lost ErrRollback: %v", err)
	}
}

func ExampleWithinTx() {
	db := NewDB()
	_ = WithinTx(context.Background(), db, func(tx *Tx) error {
		tx.Set("k", "v")
		return nil
	})
	v, ok := db.Get("k")
	fmt.Println(v, ok)
	// Output: v true
}
```

## Review

The runner is correct when the transaction is resolved exactly once on every path:
committed on success, rolled back on a returned error, rolled back on a panic (with
`ErrPanic` surfaced through the named return), and — when rollback itself fails —
both errors joined so neither is silently lost. The counters in the fake `Tx` are
the proof. The two load-bearing details are the `committed` flag, which stops the
deferred closure from rolling back an already-committed transaction, and calling
`recover()` directly in the deferred literal so a panicking unit of work becomes a
handled rollback instead of a crashed goroutine. This is the exact shape of the
real `database/sql` transaction helper you will write in production.

## Resources

- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [database/sql: Tx](https://pkg.go.dev/database/sql#Tx)
- [Go wiki: transactions with defer/commit/rollback](https://go.dev/doc/database/execute-transactions)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-errgroup-fanout-literals.md](06-errgroup-fanout-literals.md) | Next: [08-context-afterfunc-cleanup-hook.md](08-context-afterfunc-cleanup-hook.md)
