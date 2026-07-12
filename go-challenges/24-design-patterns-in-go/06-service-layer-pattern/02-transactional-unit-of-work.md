# Exercise 2: A Transactional Unit Of Work

Compensation is the tool when steps cannot share a transaction. When they *can* — debiting one account and crediting another in the same store — you want true atomicity, and you want it without welding the service to a particular database. This module builds a `TransferService` that moves money between accounts inside a `UnitOfWork`: an injectable transaction boundary that commits the whole transfer or rolls all of it back, including a debit it already applied, the instant any step fails.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
transfer.go          Account, the domain errors, AccountStore + UnitOfWork
                     interfaces, TransferService.Transfer
memdb.go             MemDB: an in-memory UnitOfWork with snapshot rollback,
                     and memTx, the per-transaction AccountStore
cmd/
  demo/
    main.go          a committing transfer, an insufficient-funds rollback,
                     a missing-destination rollback of an applied debit
transfer_test.go     conservation of total, every rollback path, one-transaction
```

- Files: `transfer.go`, `memdb.go`, `cmd/demo/main.go`, `transfer_test.go`.
- Implement: `(*TransferService).Transfer`, the `UnitOfWork` and `AccountStore` interfaces, and `MemDB` with its snapshot-isolation `Within`.
- Test: `transfer_test.go` covers the happy path, conservation of the total, insufficient-funds rollback, a missing-destination rollback of an already-applied debit, non-positive and same-account rejection, the nil-dependency guard, and that exactly one transaction is opened.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/06-service-layer-pattern/02-transactional-unit-of-work/cmd/demo && cd go-solutions/24-design-patterns-in-go/06-service-layer-pattern/02-transactional-unit-of-work
```

### Why the transaction boundary is an interface

A money transfer is two writes that must be one fact: the debit and the credit either both happen or neither does. A database gives that guarantee through a transaction, but if the service calls `db.Begin` and `tx.Commit` directly it is now bolted to that database, and the business rule — check the balance, debit, credit — is drowned in transaction bookkeeping. The unit of work pattern lifts the boundary into an interface. `UnitOfWork` has exactly one method, `Within(ctx, fn)`: it opens a transaction, hands `fn` a transaction-scoped `AccountStore`, commits if `fn` returns nil, and rolls back if `fn` returns any error. The service's whole responsibility becomes writing `fn`. It never types `Begin` or `Commit`; it states intent and trusts the boundary to make it atomic.

That separation is what makes the service both testable and portable. The `MemDB` here implements `Within` with snapshot isolation: it copies the committed balances into a private working map, runs the closure against that copy, and only publishes the copy back to the committed state if the closure succeeds. A returned error simply drops the working copy, so no partial write is ever visible. Swap `MemDB` for an implementation backed by a real `*sql.Tx` — begin on entry, commit on a nil return, rollback on an error return — and `TransferService.Transfer` does not change by one character. The business logic is written against the interface; the atomicity mechanism is a deployment detail.

The ordering inside the closure is deliberate and is what makes the rollback worth demonstrating. `Transfer` loads the source, checks the balance, debits the source, and *writes that debit to the store* — all before it even looks up the destination. If the destination does not exist, `Get` fails *after* the debit has already been applied to the working copy. A naive implementation would have leaked money. Because the write went to a transaction-scoped snapshot and the closure returned an error, `Within` discards the snapshot and the committed source balance is untouched. The atomicity is enforced entirely by the boundary; the service just returns the error and lets the unit of work clean up.

Note the division of validation. `amount <= 0` and `from == to` are checked *before* `Within` opens, because they need no data and there is no reason to start a transaction to reject them. Everything that reads or writes an account happens inside the closure. Money is modeled as `int64` minor units (cents), never `float64`, so the arithmetic is exact. Errors are sentinels wrapped with `%w`, so a caller branches with `errors.Is(err, banking.ErrInsufficientFunds)`.

Create `transfer.go`:

```go
package banking

import (
	"context"
	"errors"
	"fmt"
)

// Account is a balance keyed by id. Balance is in integer minor units (cents)
// so arithmetic is exact; never model money as float64.
type Account struct {
	ID      string
	Balance int64
}

// Domain sentinels returned by the service, wrapped with %w where they carry
// detail so callers branch with errors.Is.
var (
	ErrAccountNotFound   = errors.New("banking: account not found")
	ErrInsufficientFunds = errors.New("banking: insufficient funds")
	ErrNonPositiveAmount = errors.New("banking: amount must be positive")
	ErrSameAccount       = errors.New("banking: source and destination are the same")
)

// AccountStore is the read/write surface a use case needs inside one
// transaction. It is handed to the closure by the UnitOfWork; the closure never
// opens or commits a transaction itself.
type AccountStore interface {
	Get(ctx context.Context, id string) (*Account, error)
	Update(ctx context.Context, a *Account) error
}

// UnitOfWork is the transaction boundary. Within runs fn against a store scoped
// to a single transaction: if fn returns nil the work commits atomically, and
// if fn returns any error every write fn made is rolled back. The service
// orchestrates business steps; the UnitOfWork owns atomicity.
type UnitOfWork interface {
	Within(ctx context.Context, fn func(store AccountStore) error) error
}

// TransferService is the use case. It holds no data and no transaction logic of
// its own; it sequences domain steps inside a UnitOfWork.
type TransferService struct {
	uow UnitOfWork
}

// NewTransferService rejects a nil UnitOfWork at construction.
func NewTransferService(uow UnitOfWork) (*TransferService, error) {
	if uow == nil {
		return nil, errors.New("banking: a UnitOfWork is required")
	}
	return &TransferService{uow: uow}, nil
}

// Transfer moves amount from one account to another atomically. Input checks
// that need no data run before the transaction opens; everything that touches
// two accounts runs inside Within so a failure at any step rolls the whole
// transfer back, including a debit already applied to the source.
func (s *TransferService) Transfer(ctx context.Context, from, to string, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("%w: %d", ErrNonPositiveAmount, amount)
	}
	if from == to {
		return ErrSameAccount
	}

	return s.uow.Within(ctx, func(store AccountStore) error {
		src, err := store.Get(ctx, from)
		if err != nil {
			return err
		}
		if src.Balance < amount {
			return fmt.Errorf("%w: %s has %d, need %d", ErrInsufficientFunds, from, src.Balance, amount)
		}

		// Debit the source first, then credit the destination. If the credit
		// step fails for any reason, the UnitOfWork discards this debit too.
		src.Balance -= amount
		if err := store.Update(ctx, src); err != nil {
			return err
		}

		dst, err := store.Get(ctx, to)
		if err != nil {
			return err
		}
		dst.Balance += amount
		if err := store.Update(ctx, dst); err != nil {
			return err
		}
		return nil
	})
}
```

Now the in-memory boundary. `Within` is the entire transaction mechanism: snapshot on entry, run the closure against the snapshot, publish on success, discard on error. The single mutex serializes transactions, which is the simplest correct concurrency model to teach; a production store would use the database's own isolation.

Create `memdb.go`:

```go
package banking

import (
	"context"
	"fmt"
	"sync"
)

// MemDB is an in-memory UnitOfWork. It demonstrates the transaction boundary
// with snapshot-isolation semantics: Within copies the committed balances into
// a private working set, runs the closure against that copy, and only publishes
// the copy back if the closure succeeds. A returned error discards the copy, so
// no partial write is ever visible.
type MemDB struct {
	mu       sync.Mutex
	balances map[string]int64
}

// NewMemDB seeds the store with a copy of initial balances.
func NewMemDB(initial map[string]int64) *MemDB {
	b := make(map[string]int64, len(initial))
	for id, bal := range initial {
		b[id] = bal
	}
	return &MemDB{balances: b}
}

// Balance returns the committed balance of an account for assertions in tests
// and demos. The boolean reports whether the account exists.
func (db *MemDB) Balance(id string) (int64, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	v, ok := db.balances[id]
	return v, ok
}

// Within serializes transactions under the mutex, snapshots the committed
// state, and runs fn against the snapshot. On success it publishes the
// snapshot; on error it drops it, leaving the committed state untouched.
func (db *MemDB) Within(ctx context.Context, fn func(store AccountStore) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	working := make(map[string]int64, len(db.balances))
	for id, bal := range db.balances {
		working[id] = bal
	}

	if err := fn(&memTx{balances: working}); err != nil {
		return err // rollback: working is discarded, db.balances unchanged
	}

	db.balances = working // commit: publish the working set atomically
	return nil
}

// memTx is the per-transaction store. It reads and writes only the working
// snapshot it was given; it knows nothing about commit or rollback.
type memTx struct {
	balances map[string]int64
}

func (t *memTx) Get(_ context.Context, id string) (*Account, error) {
	bal, ok := t.balances[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	return &Account{ID: id, Balance: bal}, nil
}

func (t *memTx) Update(_ context.Context, a *Account) error {
	if _, ok := t.balances[a.ID]; !ok {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, a.ID)
	}
	t.balances[a.ID] = a.Balance
	return nil
}
```

`memTx` knows nothing about commit or rollback — it just reads and writes the working map it was handed. That ignorance is the point: the store implements the data verbs, the unit of work implements atomicity, and the service implements the business rule. Three responsibilities, three types.

### The runnable demo

The demo seeds two accounts and runs three transfers: one that commits and moves the money, one rejected for insufficient funds, and one to a non-existent account. The third is the interesting one — the service debits the source inside the transaction before discovering the destination is missing, yet the committed source balance is unchanged afterward, because the unit of work rolled the debit back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"example.com/transactional-transfer"
)

func show(db *banking.MemDB, ids ...string) {
	for _, id := range ids {
		bal, _ := db.Balance(id)
		fmt.Printf("  %s=%d", id, bal)
	}
	fmt.Println()
}

func main() {
	ctx := context.Background()
	db := banking.NewMemDB(map[string]int64{"alice": 1000, "bob": 500})

	svc, err := banking.NewTransferService(db)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== start ===")
	show(db, "alice", "bob")

	fmt.Println("=== transfer 300 alice -> bob (commits) ===")
	if err := svc.Transfer(ctx, "alice", "bob", 300); err != nil {
		log.Fatalf("unexpected: %v", err)
	}
	show(db, "alice", "bob")

	fmt.Println("=== transfer 5000 alice -> bob (insufficient, rolls back) ===")
	err = svc.Transfer(ctx, "alice", "bob", 5000)
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  insufficient funds? %v\n", errors.Is(err, banking.ErrInsufficientFunds))
	show(db, "alice", "bob")

	fmt.Println("=== transfer 200 alice -> carol (missing dest, debit rolls back) ===")
	err = svc.Transfer(ctx, "alice", "carol", 200)
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  account not found? %v\n", errors.Is(err, banking.ErrAccountNotFound))
	show(db, "alice", "bob")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== start ===
  alice=1000  bob=500
=== transfer 300 alice -> bob (commits) ===
  alice=700  bob=800
=== transfer 5000 alice -> bob (insufficient, rolls back) ===
  error: banking: insufficient funds: alice has 700, need 5000
  insufficient funds? true
  alice=700  bob=800
=== transfer 200 alice -> carol (missing dest, debit rolls back) ===
  error: banking: account not found: carol
  account not found? true
  alice=700  bob=800
```

After the committing transfer alice is 700 and bob is 800; both failed transfers leave those numbers exactly intact, which is the visible proof of rollback.

### Tests

`TestTransfer_HappyPath` checks the two balances move correctly. `TestTransfer_ConservesTotal` runs two transfers and asserts the sum across all accounts is unchanged, the invariant a transfer must never violate. `TestTransfer_InsufficientFundsRollsBack` and `TestTransfer_MissingDestinationRollsBackDebit` are the heart of the module: the second proves that a debit already written to the transaction-scoped store is discarded when a later step fails. The remaining tests pin non-positive-amount and same-account rejection (which never open a transaction), the nil-`UnitOfWork` guard, and — via a counting wrapper around the real boundary — that `Transfer` opens exactly one transaction.

Create `transfer_test.go`:

```go
package banking

import (
	"context"
	"errors"
	"testing"
)

func TestTransfer_HappyPath(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 1000, "b": 500})
	svc, err := NewTransferService(db)
	if err != nil {
		t.Fatalf("NewTransferService: %v", err)
	}

	if err := svc.Transfer(context.Background(), "a", "b", 300); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if bal, _ := db.Balance("a"); bal != 700 {
		t.Errorf("a = %d, want 700", bal)
	}
	if bal, _ := db.Balance("b"); bal != 800 {
		t.Errorf("b = %d, want 800", bal)
	}
}

func TestTransfer_ConservesTotal(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 1000, "b": 500, "c": 0})
	svc, _ := NewTransferService(db)

	_ = svc.Transfer(context.Background(), "a", "b", 250)
	_ = svc.Transfer(context.Background(), "b", "c", 400)

	var total int64
	for _, id := range []string{"a", "b", "c"} {
		bal, _ := db.Balance(id)
		total += bal
	}
	if total != 1500 {
		t.Errorf("total = %d, want 1500 (conserved)", total)
	}
}

func TestTransfer_InsufficientFundsRollsBack(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 100, "b": 0})
	svc, _ := NewTransferService(db)

	err := svc.Transfer(context.Background(), "a", "b", 500)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if bal, _ := db.Balance("a"); bal != 100 {
		t.Errorf("a = %d, want 100 (unchanged)", bal)
	}
	if bal, _ := db.Balance("b"); bal != 0 {
		t.Errorf("b = %d, want 0 (unchanged)", bal)
	}
}

func TestTransfer_MissingDestinationRollsBackDebit(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 1000})
	svc, _ := NewTransferService(db)

	// "a" is debited inside the transaction before Get("ghost") fails. The
	// UnitOfWork must discard that debit; the committed balance stays 1000.
	err := svc.Transfer(context.Background(), "a", "ghost", 200)
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
	if bal, _ := db.Balance("a"); bal != 1000 {
		t.Errorf("a = %d, want 1000 (debit rolled back)", bal)
	}
}

func TestTransfer_RejectsNonPositiveAmount(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 100, "b": 100})
	svc, _ := NewTransferService(db)

	for _, amt := range []int64{0, -50} {
		err := svc.Transfer(context.Background(), "a", "b", amt)
		if !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("amount %d: err = %v, want ErrNonPositiveAmount", amt, err)
		}
	}
	if bal, _ := db.Balance("a"); bal != 100 {
		t.Errorf("a = %d, want 100 (untouched)", bal)
	}
}

func TestTransfer_RejectsSameAccount(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 100})
	svc, _ := NewTransferService(db)

	if err := svc.Transfer(context.Background(), "a", "a", 10); !errors.Is(err, ErrSameAccount) {
		t.Errorf("err = %v, want ErrSameAccount", err)
	}
}

func TestNewTransferService_RejectsNilUOW(t *testing.T) {
	t.Parallel()

	if _, err := NewTransferService(nil); err == nil {
		t.Error("expected error for nil UnitOfWork")
	}
}

// countingUOW wraps a real UnitOfWork to prove the service runs every transfer
// inside exactly one transaction boundary.
type countingUOW struct {
	inner UnitOfWork
	calls int
}

func (c *countingUOW) Within(ctx context.Context, fn func(AccountStore) error) error {
	c.calls++
	return c.inner.Within(ctx, fn)
}

func TestTransfer_UsesOneTransaction(t *testing.T) {
	t.Parallel()

	db := NewMemDB(map[string]int64{"a": 100, "b": 0})
	uow := &countingUOW{inner: db}
	svc, _ := NewTransferService(uow)

	if err := svc.Transfer(context.Background(), "a", "b", 40); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if uow.calls != 1 {
		t.Errorf("Within called %d times, want 1", uow.calls)
	}
}
```

## Review

The unit of work is correct when the service contains no transaction bookkeeping and the boundary contains no business logic. Confirm `Transfer` writes the debit before it looks up the destination — that ordering is what makes `TestTransfer_MissingDestinationRollsBackDebit` meaningful, because it forces a real partial write that the boundary must discard. Confirm `Within` publishes the working copy only on a nil return and drops it on any error; the conservation and rollback tests all depend on that single decision. Confirm the cheap, data-free validations run before `Within` opens, so a same-account or non-positive transfer never starts a transaction at all.

The mistakes to avoid: calling `Begin`/`Commit` inside the service couples it to one database and buries the rule; reaching for compensation here (a manual "credit-back" undo) reinvents, badly, the atomicity the boundary already provides; and modeling money as `float64` makes the conservation test flaky from rounding. The discipline is one method on the boundary, one closure in the service, and `int64` cents throughout.

## Resources

- [Martin Fowler: Unit of Work](https://martinfowler.com/eaaCatalog/unitOfWork.html) — the catalog entry: "maintains a list of objects affected by a business transaction and coordinates the writing out of changes."
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is`, `errors.As`, and `%w` wrapping, the tools the service uses to expose stable domain errors.
- [database/sql Tx](https://pkg.go.dev/database/sql#Tx) — the real transaction type a production `UnitOfWork` wraps; `Begin`, `Commit`, and `Rollback` map one-to-one onto `Within`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-order-service.md](01-order-service.md) | Next: [03-validation-and-error-mapping.md](03-validation-and-error-mapping.md)
