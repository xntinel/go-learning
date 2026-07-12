# Exercise 1: Transactional Store with Deferred Rollback

An in-memory key/value store with `Begin`/`Commit`/`Rollback`, where every method
uses the acquire-then-defer-release mutex pattern and a transaction snapshots
state so `Rollback` can restore it. This is the original lesson artifact, kept
whole as its own self-contained module: it is the smallest realistic thing that
exercises deferred cleanup, LIFO unwinding, and sentinel-error transaction state.

This module is fully self-contained: it has its own `go mod init`, all types
inline, its own demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
txstore/                     independent module: example.com/txstore
  go.mod                     module example.com/txstore
  txstore.go                 Store (map+mutex), Tx (snapshot+pending), Begin/Commit/Rollback
  cmd/
    demo/
      main.go                runnable demo: commit one tx, roll back another
  txstore_test.go            defer-LIFO proof, rollback/commit, sentinel errors, -race
```

- Files: `txstore.go`, `cmd/demo/main.go`, `txstore_test.go`.
- Implement: a `Store` over `map[string]int` guarded by a `sync.Mutex`, and a `Tx` that snapshots state on `Begin`, stages writes into a `pending` map, applies them under the store lock on `Commit`, and discards them on `Rollback`; double-commit and commit-then-rollback return `ErrTxCommitted`.
- Test: set/get roundtrip, rollback restores the snapshot, commit persists, double-commit and commit-then-rollback are rejected, a LIFO-ordering proof, and 32 concurrent transactions each committing a distinct key.
- Verify: `go test -count=1 -race ./...`

### Why the deferred-unlock pattern is the backbone

Every method that touches shared state follows the same three-line shape: lock,
`defer` unlock, do the work. The `defer` is what makes the unlock correct on every
path out — the early `return` when a key is missing, the normal return, and even a
panic — so the mutex is never left held. That is the whole reason to prefer
`defer mu.Unlock()` over a manual `mu.Unlock()` before each `return`: you cannot
forget a path, and a panic mid-critical-section still releases the lock.

`Begin` takes a *snapshot* of the store under the store lock using `maps.Clone`,
so the transaction reads a stable view and a `Rollback` is simply "throw the
transaction away" — there is nothing to undo because nothing was applied. Writes
during the transaction go into a separate `pending` map, invisible to the store
and to other transactions until `Commit` copies them into the store under the
store lock. This snapshot-and-stage design is the same shape a real transactional
system uses, and it makes rollback trivial and safe: the deferred rollback in the
next exercise (a repository over `database/sql`) is the durable-storage version of
exactly this idea.

The transaction's own flags (`committed`, `rolledBack`, `done`) are guarded by the
`Tx` mutex, distinct from the store mutex. `Commit` and `Rollback` are terminal:
once a transaction is committed, a second `Commit` or a later `Rollback` returns
the sentinel `ErrTxCommitted`, wrapped so callers can match it with `errors.Is`.

Create `txstore.go`:

```go
package txstore

import (
	"errors"
	"maps"
	"sync"
)

// Sentinel errors let callers match transaction-state failures with errors.Is.
var (
	ErrTxCommitted  = errors.New("transaction already committed")
	ErrTxRolledBack = errors.New("transaction already rolled back")
)

// Store is a concurrency-safe map[string]int. Its mutex is the only lock that
// guards the committed data.
type Store struct {
	mu   sync.Mutex
	data map[string]int
}

func New() *Store {
	return &Store{data: make(map[string]int)}
}

// Get returns the committed value for key, if present.
func (s *Store) Get(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// Set writes a value directly into the store, outside any transaction.
func (s *Store) Set(key string, value int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Tx is a snapshot-isolated transaction. Reads see the snapshot plus its own
// pending writes; nothing is visible to the store until Commit.
type Tx struct {
	store      *Store
	mu         sync.Mutex
	snapshot   map[string]int
	pending    map[string]int
	done       bool
	committed  bool
	rolledBack bool
}

// Begin snapshots the current committed state under the store lock.
func (s *Store) Begin() *Tx {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Tx{
		store:    s,
		snapshot: maps.Clone(s.data),
		pending:  make(map[string]int),
	}
}

// Get reads the transaction's own pending write first, then the snapshot.
func (t *Tx) Get(key string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.pending[key]; ok {
		return v, true
	}
	v, ok := t.snapshot[key]
	return v, ok
}

// Set stages a write into the transaction; it is a no-op after Commit/Rollback.
func (t *Tx) Set(key string, value int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.pending[key] = value
}

// Commit applies every staged write to the store under the store lock. Both the
// Tx lock and the store lock unwind in LIFO order via their defers.
func (t *Tx) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.committed {
		return ErrTxCommitted
	}
	if t.rolledBack {
		return ErrTxRolledBack
	}
	t.committed = true
	t.done = true
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	for k, v := range t.pending {
		t.store.data[k] = v
	}
	return nil
}

// Rollback discards the staged writes. Because nothing was applied, there is
// literally nothing to undo.
func (t *Tx) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.committed {
		return ErrTxCommitted
	}
	if t.rolledBack {
		return ErrTxRolledBack
	}
	t.rolledBack = true
	t.done = true
	return nil
}
```

Note the double `defer` in `Commit`: it locks the `Tx`, defers that unlock, then
locks the store and defers *that* unlock. On return the store unlock runs first
(last registered), then the `Tx` unlock — reverse acquisition order, exactly the
LIFO rule, and exactly the order that avoids a lock-ordering inversion.

### The runnable demo

The demo commits one transaction and rolls another back, then reads the store to
show the committed write survived and the rolled-back write did not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/txstore"
)

func main() {
	s := txstore.New()
	s.Set("balance", 100)

	// A committed transaction.
	tx1 := s.Begin()
	tx1.Set("balance", 150)
	if err := tx1.Commit(); err != nil {
		fmt.Println("commit:", err)
	}

	// A rolled-back transaction.
	tx2 := s.Begin()
	tx2.Set("balance", 999)
	if err := tx2.Rollback(); err != nil {
		fmt.Println("rollback:", err)
	}

	v, _ := s.Get("balance")
	fmt.Printf("balance = %d\n", v)

	// A terminal transaction rejects a second Commit.
	if err := tx1.Commit(); err != nil {
		fmt.Println("second commit:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
balance = 150
second commit: transaction already committed
```

### Tests

`TestRollbackRestoresSnapshot` is the lesson's centerpiece: a `Set` inside a `Tx`
is invisible to the store until `Commit`, and a `Rollback` leaves the store at its
pre-transaction state. `TestDeferOrderIsLIFO` pins the ordering rule directly by
registering N deferred appends and asserting the recorded order is reversed.
`TestConcurrentTransactionsDoNotCorruptState` runs 32 transactions in parallel,
each committing a distinct key, and proves the snapshot/commit path is race-free
under `-race`.

Create `txstore_test.go`:

```go
package txstore

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestSetAndGet(t *testing.T) {
	t.Parallel()

	s := New()
	s.Set("alpha", 1)
	s.Set("beta", 2)

	if v, ok := s.Get("alpha"); !ok || v != 1 {
		t.Fatalf("alpha = (%d, %v)", v, ok)
	}
	if v, ok := s.Get("beta"); !ok || v != 2 {
		t.Fatalf("beta = (%d, %v)", v, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("missing key should return false")
	}
}

func TestRollbackRestoresSnapshot(t *testing.T) {
	t.Parallel()

	s := New()
	s.Set("alpha", 1)

	tx := s.Begin()
	tx.Set("alpha", 99)
	tx.Set("new_key", 42)

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback() = %v", err)
	}

	if v, _ := s.Get("alpha"); v != 1 {
		t.Fatalf("alpha after rollback = %d, want 1", v)
	}
	if _, ok := s.Get("new_key"); ok {
		t.Fatal("new_key should not exist after rollback")
	}
}

func TestCommitPersistsChanges(t *testing.T) {
	t.Parallel()

	s := New()
	s.Set("alpha", 1)

	tx := s.Begin()
	tx.Set("alpha", 99)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() = %v", err)
	}

	if v, _ := s.Get("alpha"); v != 99 {
		t.Fatalf("alpha after commit = %d, want 99", v)
	}
}

func TestDoubleCommitIsRejected(t *testing.T) {
	t.Parallel()

	s := New()
	tx := s.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTxCommitted) {
		t.Fatalf("err = %v, want ErrTxCommitted", err)
	}
}

func TestCommitThenRollbackIsRejected(t *testing.T) {
	t.Parallel()

	s := New()
	tx := s.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); !errors.Is(err, ErrTxCommitted) {
		t.Fatalf("err = %v, want ErrTxCommitted", err)
	}
}

// TestDeferOrderIsLIFO proves the last-registered defer runs first.
func TestDeferOrderIsLIFO(t *testing.T) {
	t.Parallel()

	const n = 5
	var order []int
	func() {
		for i := range n {
			defer func() { order = append(order, i) }()
		}
	}()

	want := []int{4, 3, 2, 1, 0}
	if !slices.Equal(order, want) {
		t.Fatalf("defer order = %v, want %v", order, want)
	}
}

func TestConcurrentTransactionsDoNotCorruptState(t *testing.T) {
	t.Parallel()

	s := New()

	const txCount = 32
	var wg sync.WaitGroup
	for i := range txCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx := s.Begin()
			tx.Set(fmt.Sprintf("tx.%d", i), i)
			if err := tx.Commit(); err != nil {
				t.Errorf("Commit() = %v", err)
			}
		}()
	}
	wg.Wait()

	for i := range txCount {
		key := fmt.Sprintf("tx.%d", i)
		v, ok := s.Get(key)
		if !ok {
			t.Fatalf("missing key %s", key)
		}
		if v != i {
			t.Fatalf("%s = %d, want %d", key, v, i)
		}
	}
}

func Example() {
	s := New()
	s.Set("x", 1)

	tx := s.Begin()
	tx.Set("x", 2)
	tx.Rollback()

	v, _ := s.Get("x")
	fmt.Println(v)
	// Output: 1
}
```

## Review

The store is correct when a transaction's writes are invisible until `Commit` and
a `Rollback` is a pure discard: `TestRollbackRestoresSnapshot` proves both, and
`TestCommitPersistsChanges` proves the write reaches the store. The deferred
unlock is what keeps the mutex from leaking on the early-return paths — remove the
`defer` and add manual unlocks and you will eventually miss one. The two most
common mistakes this artifact guards against are lock-ordering inversions (the
double `defer` in `Commit` releases store-then-Tx, the reverse of acquisition) and
reusing a terminal transaction (the `ErrTxCommitted`/`ErrTxRolledBack` sentinels,
matched with `errors.Is`). Run `go test -race` to confirm the snapshot/commit path
holds up under 32 concurrent producers.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — evaluation and LIFO execution order.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock behind the acquire-then-defer-release pattern.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the shallow snapshot used by `Begin`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the transaction-state sentinels.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-defer-lifo-resource-stack.md](02-defer-lifo-resource-stack.md)
