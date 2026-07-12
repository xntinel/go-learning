# Exercise 3: Transaction Boundary: Shadowed Named Return Commits on Error

A money-transfer repository method with a deferred rollback-or-commit guard that
inspects a named return `err`. This is the classic silent bug: an inner
`if _, err := step()` shadows the named return, the guard sees `nil`, and it
commits state that should have rolled back. The correct version reassigns the
outer `err` with `=`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
bank/                           module: example.com/bank
  go.mod
  bank.go                       Tx, DB, Repo, Transfer with deferred commit/rollback guard
  cmd/
    demo/
      main.go                   in-memory bank: one good transfer, one rejected
  bank_test.go                  commit on success, rollback on step error, rollback on panic
```

- Files: `bank.go`, `cmd/demo/main.go`, `bank_test.go`.
- Implement: `Transfer(from, to string, amount int) (err error)` with a deferred guard that rolls back when the named return is non-nil, commits otherwise, and recovers panics into `err`.
- Test: a `fakeTx` recording Commit/Rollback; assert commit-on-success, rollback-and-wrapped-error-on-step-failure (`errors.Is`), and rollback-on-panic.
- Verify: `go test -count=1 -race ./...`

### Why the named return must be assigned with `=`

The deferred guard is the only place that decides commit versus rollback, and it
decides by reading the *named return* `err`:

```go
defer func() {
	if err != nil {
		tx.Rollback()
	} else {
		err = tx.Commit()
	}
}()
```

For that decision to be correct, every failure inside the body must reach *that*
`err`. The trap is a nested short declaration:

```go
if _, err := tx.Debit(from, amount); err != nil { // BUG: new inner err
	return err
}
```

The `:=` declares a fresh `err` scoped to the `if`. On this exact line the
`return err` copies its value into the named return, so a single-step failure
happens to propagate — which is why the bug hides. But the moment any path sets a
local `err` without an immediate `return`, or the guard is meant to observe a
failure recorded earlier, the named return is still `nil` and the deferred guard
commits corrupted state. The fix is uniform: use `=` so `err` is always the named
return.

The guard also recovers panics: a panic in a step (a nil map write, a driver
blow-up) is caught, converted into `err`, and turned into a rollback, so a panic
never leaves a transaction dangling open.

Create `bank.go`:

```go
package bank

import "fmt"

// Tx is a transaction with two write steps and commit/rollback.
type Tx interface {
	Debit(account string, amount int) error
	Credit(account string, amount int) error
	Commit() error
	Rollback() error
}

// DB opens transactions.
type DB interface {
	Begin() (Tx, error)
}

// Repo runs transfers against a DB.
type Repo struct {
	db DB
}

func New(db DB) *Repo {
	return &Repo{db: db}
}

// Transfer moves amount from one account to another atomically. The deferred
// guard reads the named return err: non-nil rolls back, nil commits, a panic is
// recovered into err and rolled back. Every inner assignment uses = so it reaches
// the named return; a := there would shadow it and the guard would commit on error.
func (r *Repo) Transfer(from, to string, amount int) (err error) {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("transfer panicked: %v", p)
			_ = tx.Rollback()
			return
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()

	if err = tx.Debit(from, amount); err != nil {
		return fmt.Errorf("debit %s: %w", from, err)
	}
	if err = tx.Credit(to, amount); err != nil {
		return fmt.Errorf("credit %s: %w", to, err)
	}
	return nil
}
```

### The runnable demo

The demo wires an in-memory bank whose transaction stages changes and applies
them only on `Commit`. A rejected transfer rolls back and leaves balances
untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bank"
)

type ledger struct {
	balances map[string]int
}

type memDB struct {
	l *ledger
}

func (d *memDB) Begin() (bank.Tx, error) {
	return &memTx{l: d.l, staged: map[string]int{}}, nil
}

type memTx struct {
	l      *ledger
	staged map[string]int
}

func (t *memTx) Debit(account string, amount int) error {
	if t.l.balances[account]+t.staged[account] < amount {
		return fmt.Errorf("insufficient funds in %s", account)
	}
	t.staged[account] -= amount
	return nil
}

func (t *memTx) Credit(account string, amount int) error {
	t.staged[account] += amount
	return nil
}

func (t *memTx) Commit() error {
	for account, delta := range t.staged {
		t.l.balances[account] += delta
	}
	return nil
}

func (t *memTx) Rollback() error {
	t.staged = nil
	return nil
}

func main() {
	l := &ledger{balances: map[string]int{"alice": 100, "bob": 0}}
	r := bank.New(&memDB{l: l})

	if err := r.Transfer("alice", "bob", 30); err != nil {
		fmt.Println("transfer 1 failed:", err)
	}
	fmt.Printf("after transfer: alice=%d bob=%d\n", l.balances["alice"], l.balances["bob"])

	if err := r.Transfer("alice", "bob", 1000); err != nil {
		fmt.Println("transfer 2 failed:", err)
	}
	fmt.Printf("after rejected: alice=%d bob=%d\n", l.balances["alice"], l.balances["bob"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after transfer: alice=70 bob=30
transfer 2 failed: debit alice: insufficient funds in alice
after rejected: alice=70 bob=30
```

### Tests

`fakeTx` records whether Commit and Rollback ran. The step-error test proves the
guard rolls back and never commits, and that the wrapped error reaches the caller.

Create `bank_test.go`:

```go
package bank

import (
	"errors"
	"fmt"
	"testing"
)

type fakeTx struct {
	debitErr   error
	creditErr  error
	panicOn    string
	committed  bool
	rolledBack bool
}

func (t *fakeTx) Debit(account string, amount int) error {
	if t.panicOn == "debit" {
		panic("driver exploded")
	}
	return t.debitErr
}

func (t *fakeTx) Credit(account string, amount int) error {
	if t.panicOn == "credit" {
		panic("driver exploded")
	}
	return t.creditErr
}

func (t *fakeTx) Commit() error   { t.committed = true; return nil }
func (t *fakeTx) Rollback() error { t.rolledBack = true; return nil }

type fakeDB struct {
	tx *fakeTx
}

func (d *fakeDB) Begin() (Tx, error) { return d.tx, nil }

func TestTransferCommitsOnSuccess(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{}
	r := New(&fakeDB{tx: tx})

	if err := r.Transfer("alice", "bob", 10); err != nil {
		t.Fatal(err)
	}
	if !tx.committed {
		t.Fatal("Commit did not run on success")
	}
	if tx.rolledBack {
		t.Fatal("Rollback ran on success")
	}
}

func TestTransferRollsBackOnStepError(t *testing.T) {
	t.Parallel()

	insufficient := errors.New("insufficient funds")
	tx := &fakeTx{debitErr: insufficient}
	r := New(&fakeDB{tx: tx})

	err := r.Transfer("alice", "bob", 10)
	if !errors.Is(err, insufficient) {
		t.Fatalf("error = %v, want wrap of %v", err, insufficient)
	}
	if tx.committed {
		t.Fatal("Commit ran despite a failed step (shadowed named return?)")
	}
	if !tx.rolledBack {
		t.Fatal("Rollback did not run on step error")
	}
}

func TestTransferRollsBackOnPanic(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{panicOn: "credit"}
	r := New(&fakeDB{tx: tx})

	err := r.Transfer("alice", "bob", 10)
	if err == nil {
		t.Fatal("expected an error after a panicking step")
	}
	if tx.committed {
		t.Fatal("Commit ran after a panic")
	}
	if !tx.rolledBack {
		t.Fatal("Rollback did not run after a panic")
	}
}

func ExampleRepo_Transfer() {
	tx := &fakeTx{}
	r := New(&fakeDB{tx: tx})
	err := r.Transfer("alice", "bob", 5)
	fmt.Println(err, tx.committed, tx.rolledBack)
	// Output: <nil> true false
}
```

## Review

The transaction boundary is correct when a failed or panicking step always rolls
back and never commits. `TestTransferRollsBackOnStepError` is the one that a
shadowed named return breaks: with `if _, err := tx.Debit(...)` the deferred guard
would see `nil`, run `Commit`, and the assertion `tx.committed` would fail the
test. `TestTransferRollsBackOnPanic` proves the `recover` path also rolls back.
The single rule that makes it work is assigning the named return with `=`. Keep
the guard's logic entirely in terms of the named `err`, and never introduce a
same-named local inside the body.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — how a deferred func can modify named return values.
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — recovering a panic inside a deferred function.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the named-return-plus-defer pattern.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-codec-registry-blank-import.md](04-codec-registry-blank-import.md)
