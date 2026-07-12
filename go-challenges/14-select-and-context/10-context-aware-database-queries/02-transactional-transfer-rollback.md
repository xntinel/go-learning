# Exercise 2: A Transactional Transfer That Rolls Back When the Context Fires

A transfer is the canonical two-write transaction: debit one account, credit
another, and either both land or neither does. This exercise builds that on a
context-aware transaction so the outcome is driven only by whether the context
fires mid-flight — the same code path either commits cleanly or rolls back
leaving no partial state.

## What you'll build

```text
simtx/                       independent module: example.com/simtx
  go.mod                     go 1.25
  simtx.go                   DB, Tx (pending map, Commit/Rollback), Transfer
  cmd/
    demo/
      main.go                a committing transfer and a rolled-back one
  simtx_test.go              commit-on-success, rollback-on-cancel (no partial state), -race
```

Files: `simtx.go`, `cmd/demo/main.go`, `simtx_test.go`.
Implement: a `Tx` holding pending writes in its own map, with `Commit` flushing them and `Rollback` discarding them, plus a `Transfer` that begins, reads the source, writes source, writes dest, and commits — rolling back on any error or cancellation.
Test: a generous deadline commits both sides; a deadline shorter than the two writes returns `context.DeadlineExceeded` and leaves both keys at their original values.
Verify: `go test -count=1 -race ./...`

### Why the pending map is what makes rollback trivial

A transaction has to be atomic: an observer must never see the source debited
without the destination credited. The mechanism that guarantees it here is that
`Tx` writes go into a *pending* map owned by the transaction, not into the shared
store. Nothing the transaction does is visible to other readers until `Commit`
copies the pending map into the DB under the lock. `Rollback` simply drops the
pending map. That is why the failure path is so simple: there is no
compensating "undo the debit" logic to get wrong, because the debit never touched
shared state in the first place.

`Transfer` is the whole pattern in one function:

```
tx, err := d.Begin(ctx)          // fails fast if ctx already cancelled
// read source, write source, write dest -- all on the same ctx
// on ANY error: tx.Rollback(); return
tx.Commit()
```

The tests exercise both outcomes over the identical code. `TestTransferCommitsOnSuccess`
runs under a one-second deadline that easily covers the two 1ms writes, and
asserts both accounts reflect the transfer. `TestTransferRollsBackOnCancel` runs
under a deadline shorter than the two 30ms writes, so the context fires between
the source write and the destination write. It asserts two things: the returned
error `errors.Is` `context.DeadlineExceeded`, and — the real canary — both
accounts still hold their *original* values when read back under
`context.Background()`. If the rollback were broken, the source would show the
debit while the destination never got the credit, and the second assertion would
catch it. Reading the canary under `Background` (not the cancelled `ctx`) is
essential: a cancelled context could not perform the verification read.

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/10-context-aware-database-queries/02-transactional-transfer-rollback/cmd/demo
cd go-solutions/14-select-and-context/10-context-aware-database-queries/02-transactional-transfer-rollback
go mod edit -go=1.25
```

Create `simtx.go`:

```go
package simtx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is the sentinel for a missing key.
var ErrNotFound = errors.New("simtx: key not found")

// DB is a concurrency-safe in-memory store whose operations race a simulated
// latency against ctx.Done, modeling context-aware driver methods.
type DB struct {
	mu      sync.RWMutex
	data    map[string]string
	latency time.Duration
}

func New(latency time.Duration, kvs map[string]string) *DB {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	return &DB{data: cp, latency: latency}
}

func (d *DB) Get(ctx context.Context, key string) (string, error) {
	select {
	case <-time.After(d.latency):
	case <-ctx.Done():
		return "", fmt.Errorf("simtx.Get(%s): %w", key, ctx.Err())
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.data[key]
	if !ok {
		return "", fmt.Errorf("simtx.Get(%s): %w", key, ErrNotFound)
	}
	return v, nil
}

// Tx is a context-aware transaction. Writes accumulate in pending and become
// visible to other readers only on Commit; Rollback discards them.
type Tx struct {
	db      *DB
	pending map[string]string
}

// Begin starts a transaction. It fails fast if ctx is already cancelled.
func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("simtx.Begin: %w", err)
	}
	return &Tx{db: d, pending: make(map[string]string)}, nil
}

// Set records a pending write, racing the store's latency against ctx.Done.
func (t *Tx) Set(ctx context.Context, key, value string) error {
	select {
	case <-time.After(t.db.latency):
	case <-ctx.Done():
		return fmt.Errorf("simtx.Tx.Set(%s): %w", key, ctx.Err())
	}
	t.pending[key] = value
	return nil
}

// Commit flushes the pending writes into the store atomically under the lock.
func (t *Tx) Commit() {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	for k, v := range t.pending {
		t.db.data[k] = v
	}
	t.pending = nil
}

// Rollback discards the pending writes. It is safe to call after Commit.
func (t *Tx) Rollback() {
	t.pending = nil
}

// Transfer debits from and credits to inside a single transaction. On any error
// or cancellation it rolls back, so no partial write is ever visible.
func (d *DB) Transfer(ctx context.Context, from, to string, amount int) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}

	cur, err := d.Get(ctx, from)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("transfer read source: %w", err)
	}

	if err := tx.Set(ctx, from, fmt.Sprintf("%s-%d", cur, amount)); err != nil {
		tx.Rollback()
		return fmt.Errorf("transfer write source: %w", err)
	}

	if err := tx.Set(ctx, to, fmt.Sprintf("+%d", amount)); err != nil {
		tx.Rollback()
		return fmt.Errorf("transfer write dest: %w", err)
	}

	tx.Commit()
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/simtx"
)

func main() {
	ok := simtx.New(10*time.Millisecond, map[string]string{"alice": "100", "bob": "50"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := ok.Transfer(ctx, "alice", "bob", 30)
	a, _ := ok.Get(ctx, "alice")
	b, _ := ok.Get(ctx, "bob")
	fmt.Printf("commit: err=%v alice=%s bob=%s\n", err, a, b)

	slow := simtx.New(30*time.Millisecond, map[string]string{"carol": "100", "dave": "50"})
	tight, cancelTight := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelTight()
	err = slow.Transfer(tight, "carol", "dave", 30)
	c, _ := slow.Get(context.Background(), "carol")
	d, _ := slow.Get(context.Background(), "dave")
	fmt.Printf("rollback: deadline=%v carol=%s dave=%s\n",
		errors.Is(err, context.DeadlineExceeded), c, d)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
commit: err=<nil> alice=100-30 bob=+30
rollback: deadline=true carol=100 dave=50
```

### Tests

Create `simtx_test.go`:

```go
package simtx

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTransferCommitsOnSuccess(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, map[string]string{"alice": "100", "bob": "50"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := db.Transfer(ctx, "alice", "bob", 30); err != nil {
		t.Fatalf("Transfer: err = %v, want nil", err)
	}
	a, err := db.Get(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if a != "100-30" {
		t.Fatalf("alice = %q, want 100-30", a)
	}
	b, err := db.Get(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if b != "+30" {
		t.Fatalf("bob = %q, want +30", b)
	}
}

func TestTransferRollsBackOnCancel(t *testing.T) {
	t.Parallel()
	db := New(30*time.Millisecond, map[string]string{"alice": "100", "bob": "50"})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	err := db.Transfer(ctx, "alice", "bob", 30)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Transfer: err = %v, want DeadlineExceeded", err)
	}

	// Canary: read under Background (the cancelled ctx could not perform a read).
	// Both accounts must still hold their original values.
	a, _ := db.Get(context.Background(), "alice")
	if a != "100" {
		t.Fatalf("alice after rollback = %q, want 100 (rollback left partial state)", a)
	}
	b, _ := db.Get(context.Background(), "bob")
	if b != "50" {
		t.Fatalf("bob after rollback = %q, want 50 (rollback left partial state)", b)
	}
}

func TestTransferMissingSource(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, map[string]string{"bob": "50"})
	err := db.Transfer(context.Background(), "ghost", "bob", 10)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Transfer: err = %v, want ErrNotFound", err)
	}
	// Destination must be untouched because the tx rolled back before writing it.
	b, _ := db.Get(context.Background(), "bob")
	if b != "50" {
		t.Fatalf("bob = %q, want 50 (dest written despite failed transfer)", b)
	}
}

func ExampleDB_Transfer() {
	db := New(time.Millisecond, map[string]string{"alice": "100", "bob": "50"})
	_ = db.Transfer(context.Background(), "alice", "bob", 30)
	a, _ := db.Get(context.Background(), "alice")
	fmt.Println(a)
	// Output: 100-30
}
```

## Review

The transaction is correct when partial state is impossible: because writes live
in a pending map until `Commit` copies them under the lock, a `Rollback` (on
error or cancellation) leaves the store exactly as it was. `TestTransferRollsBackOnCancel`
is the canary — if `alice` or `bob` ever read back as anything but their original
values after a cancelled transfer, atomicity is broken. Two details make the test
trustworthy: the verification reads use `context.Background()` (a cancelled
context cannot read), and the deadline is chosen to fire *between* the two writes,
which is precisely the window where a naive "write directly, undo on failure"
design would leave the source debited. Run `-race`: `Commit` takes the write lock
while concurrent `Get`s hold the read lock, so the pending-map flush must be
correctly synchronized.

## Resources

- [database/sql: DB.BeginTx](https://pkg.go.dev/database/sql#DB.BeginTx) — the real transaction API this models.
- [database/sql: Tx](https://pkg.go.dev/database/sql#Tx) — `Commit`, `Rollback`, and their lifetime rules.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — cancellation propagation through a call chain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-batch-read-loop-cancellation.md](03-batch-read-loop-cancellation.md)
