# Exercise 1: The Version Store and Snapshot Isolation

This exercise builds the heart of the MVCC engine: a version store whose rows are chains of immutable versions, a transaction manager that hands out monotone commit sequence numbers, and the visibility predicate that ties them together. Once the predicate is right, every higher-level operation — read, scan, insert, update, delete, abort, garbage collection — is a thin walk over the chain that asks one question of each version: are you visible to me? Snapshot isolation, read-your-own-writes, first-writer-wins conflict detection, and atomic rollback all fall out of that single rule.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
mvcc.go          TxID, Version, Tuple, Transaction, TransactionManager, IsVisible
store.go         VersionStore, Read, Scan, Insert, Update, Delete, rollback, GarbageCollect
cmd/
  demo/
    main.go      snapshot isolation, abort/rollback, and GC walkthrough
mvcc_test.go     snapshot isolation, read-your-own-writes, conflict, abort, delete, GC, races
```

- Files: `mvcc.go`, `store.go`, `cmd/demo/main.go`, `mvcc_test.go`.
- Implement: `TransactionManager` (`Begin`, `Commit`, `Abort`, `committedBefore`, `isActive`, `OldestActiveSnapshot`, `IsVisible`) and `VersionStore` (`Read`, `Scan`, `Insert`, `Update`, `Delete`, `rollback`, `GarbageCollect`).
- Test: `mvcc_test.go` proves snapshot isolation across a concurrent commit, read-your-own-writes, first-writer-wins conflict, abort rollback, delete visibility, GC retention, and a 50-goroutine race.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/07-mvcc/01-version-store-and-snapshot-isolation/cmd/demo && cd go-solutions/39-capstone-database-engine/07-mvcc/01-version-store-and-snapshot-isolation
```

### The transaction manager and the visibility rule

A transaction is born at `Begin`: it gets a fresh id from one atomic counter and captures `startedAt` from a second atomic counter, the commit sequence. The commit sequence is the logical clock on which the entire snapshot argument rests. Commit removes the transaction from the active set, increments the commit sequence, and records the resulting value in a commit log keyed by id. The ordering is deliberate — the value `startedAt` reads at Begin is the sequence number of the last commit that had already finished, so "committed in my past" means "recorded with a sequence at or below my `startedAt`."

`IsVisible` is the whole engine in one function. A version is visible when its creator is in the transaction's past (or is the transaction itself, giving read-your-own-writes) and its deleter is not (or is absent, or is the transaction itself, which hides a row it deleted). The creator check consults the commit log, so an uncommitted or aborted `Xmin` — never recorded — fails it and stays invisible, which is what prevents dirty reads. The comparison is `<=`, not `<`: because `startedAt` is captured after a committing writer has already bumped the counter, a transaction whose recorded sequence equals `startedAt` finished committing before the snapshot was taken and must be visible. Strict `<` would hide a fully-committed predecessor and reintroduce a phantom.

`OldestActiveSnapshot` returns the minimum `startedAt` among active transactions, or the maximum `uint64` when none are active. That value is the low watermark the garbage collector uses; it lives on the manager because only the manager knows who is active.

Create `mvcc.go`:

```go
// Package mvcc implements Multi-Version Concurrency Control with snapshot isolation.
package mvcc

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// TxID is a monotonically increasing transaction identifier.
type TxID uint64

// Sentinel errors returned by store and manager operations.
var (
	ErrNotFound         = errors.New("mvcc: key not found")
	ErrWriteConflict    = errors.New("mvcc: write-write conflict")
	ErrTxNotActive      = errors.New("mvcc: transaction not active")
	ErrKeyAlreadyExists = errors.New("mvcc: key already exists")
)

// Version holds a single revision of a row in a version chain.
type Version struct {
	Data []byte   // serialized row data
	Xmin TxID     // transaction that created this version
	Xmax TxID     // transaction that deleted this version; 0 means the version is live
	prev *Version // older version in the chain; nil at the oldest end
}

// Tuple is the logical row exposed to callers of Read and Scan.
type Tuple struct {
	Key  string
	Data []byte
}

// opKind tags a write-set entry with the type of operation performed.
type opKind uint8

const (
	opInsert opKind = iota
	opUpdate
	opDelete
)

// writeOp records one write operation for rollback.
type writeOp struct {
	key    string
	kind   opKind
	newVer *Version // non-nil for opInsert and opUpdate
	oldVer *Version // non-nil for opUpdate and opDelete
}

// Transaction is the caller-facing handle for one database transaction.
type Transaction struct {
	ID        TxID
	startedAt uint64 // commit-sequence counter captured at Begin()
	mu        sync.Mutex
	writes    []writeOp
}

// TransactionManager coordinates Begin, Commit, and Abort across concurrent callers.
type TransactionManager struct {
	nextID    atomic.Uint64
	commitSeq atomic.Uint64 // incremented on every successful Commit
	commitLog sync.Map      // TxID -> uint64 (commit sequence number)
	activeMu  sync.RWMutex
	active    map[TxID]*Transaction
}

// NewTransactionManager returns a ready-to-use TransactionManager.
func NewTransactionManager() *TransactionManager {
	return &TransactionManager{active: make(map[TxID]*Transaction)}
}

// Begin starts a new transaction and captures a snapshot of the commit sequence.
// Every transaction that commits later increments commitSeq, so any transaction
// whose commit sequence exceeds startedAt is outside this transaction's snapshot.
func (m *TransactionManager) Begin() *Transaction {
	id := TxID(m.nextID.Add(1))
	snap := m.commitSeq.Load()
	tx := &Transaction{ID: id, startedAt: snap}
	m.activeMu.Lock()
	m.active[id] = tx
	m.activeMu.Unlock()
	return tx
}

// Commit finalises tx: removes it from the active set and records it in the commit
// log with the next sequence number. Returns ErrTxNotActive if tx was already
// committed or aborted.
func (m *TransactionManager) Commit(tx *Transaction) error {
	m.activeMu.Lock()
	if _, ok := m.active[tx.ID]; !ok {
		m.activeMu.Unlock()
		return fmt.Errorf("%w: tx %d", ErrTxNotActive, tx.ID)
	}
	delete(m.active, tx.ID)
	m.activeMu.Unlock()
	seq := m.commitSeq.Add(1)
	m.commitLog.Store(tx.ID, seq)
	return nil
}

// Abort removes tx from the active set and rolls back its writes via store.
// Returns ErrTxNotActive if tx was already committed or aborted.
func (m *TransactionManager) Abort(tx *Transaction, store *VersionStore) error {
	m.activeMu.Lock()
	if _, ok := m.active[tx.ID]; !ok {
		m.activeMu.Unlock()
		return fmt.Errorf("%w: tx %d", ErrTxNotActive, tx.ID)
	}
	delete(m.active, tx.ID)
	m.activeMu.Unlock()
	// activeMu is released before rollback to avoid a deadlock:
	// rollback acquires per-chain locks, and concurrent writes hold per-chain locks
	// before acquiring activeMu.RLock (for isActive checks).
	store.rollback(tx)
	return nil
}

// committedBefore reports whether txID appears in the commit log with a sequence
// number at or below seqBound. A result of true means the transaction committed
// before (or at) the captured snapshot.
func (m *TransactionManager) committedBefore(txID TxID, seqBound uint64) bool {
	v, ok := m.commitLog.Load(txID)
	if !ok {
		return false
	}
	return v.(uint64) <= seqBound
}

// isActive reports whether txID is currently in the active transaction set.
func (m *TransactionManager) isActive(txID TxID) bool {
	m.activeMu.RLock()
	_, ok := m.active[txID]
	m.activeMu.RUnlock()
	return ok
}

// OldestActiveSnapshot returns the minimum startedAt value among all active
// transactions. When no transactions are active, it returns ^uint64(0) (the maximum
// value), making all committed versions eligible for garbage collection.
func (m *TransactionManager) OldestActiveSnapshot() uint64 {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	min := ^uint64(0)
	for _, tx := range m.active {
		if tx.startedAt < min {
			min = tx.startedAt
		}
	}
	return min
}

// IsVisible reports whether ver is visible to tx under snapshot-isolation rules.
//
// A version is visible when both conditions hold:
//  1. The creating transaction (Xmin) committed before tx's snapshot, OR Xmin is tx
//     itself (read-your-own-writes for inserts and updates within the same transaction).
//  2. The version is live (Xmax == 0), OR the deleting transaction (Xmax) committed
//     after tx's snapshot, meaning the deletion is outside tx's view. If Xmax == tx.ID,
//     the version was deleted by tx itself and is not visible to tx.
func (m *TransactionManager) IsVisible(ver *Version, tx *Transaction) bool {
	creatorOK := ver.Xmin == tx.ID || m.committedBefore(ver.Xmin, tx.startedAt)
	if !creatorOK {
		return false
	}
	if ver.Xmax == 0 {
		return true // live version
	}
	if ver.Xmax == tx.ID {
		return false // tx deleted this version itself
	}
	// Deleted by another transaction: visible only when that deletion is not yet
	// committed from tx's perspective.
	return !m.committedBefore(ver.Xmax, tx.startedAt)
}
```

`startedAt` captures the commit-sequence counter at `Begin`, not a wall-clock timestamp. Comparing commit sequence numbers rather than transaction ids is what lets a later-beginning transaction (higher id) correctly exclude a transaction that committed after it but holds a lower id — commit order, not id order, defines the snapshot.

### The version store and the per-chain lock

Each key owns a `chain` guarded by its own read-write lock; the store maps keys to chains under a separate store-level lock. This two-level locking is what keeps the engine concurrent: a read takes only the chain's read lock and walks from the head to the first visible version, so concurrent readers of the same key never block each other and a reader of one key never touches another key's lock. Writes take the chain's write lock for the entire operation — the visibility walk, the `Xmax` mutation, and the new-version prepend happen as one atomic step, which is what makes first-writer-wins correct: a second writer cannot slip between the conflict check and the mutation.

`Update` and `Delete` carry the conflict guard: if the version they would overwrite already has `Xmax` set by another transaction that is still active, they return `ErrWriteConflict`. `Insert` guards differently — it fails with `ErrKeyAlreadyExists` if any version of the key is already visible to the transaction. Every write also appends a `writeOp` to the transaction's write set so `rollback` can undo it in reverse, removing inserted/updated head versions and clearing `Xmax` on the versions an update or delete marked. `Scan` snapshots the key list under the store read lock and then reads each chain without holding it, so a concurrent insert to a new key never blocks an in-flight scan.

Create `store.go`:

```go
package mvcc

import (
	"fmt"
	"sync"
)

// chain holds the version chain for one key protected by its own read-write lock.
// Readers take RLock; writers take Lock.
type chain struct {
	mu   sync.RWMutex
	head *Version // newest version; nil when the chain exists but holds no versions
}

// VersionStore maps primary keys to version chains.
type VersionStore struct {
	mu     sync.RWMutex
	chains map[string]*chain
}

// NewVersionStore returns an empty VersionStore.
func NewVersionStore() *VersionStore {
	return &VersionStore{chains: make(map[string]*chain)}
}

func (s *VersionStore) getChain(key string) (*chain, bool) {
	s.mu.RLock()
	c, ok := s.chains[key]
	s.mu.RUnlock()
	return c, ok
}

func (s *VersionStore) getOrCreateChain(key string) *chain {
	s.mu.RLock()
	c, ok := s.chains[key]
	s.mu.RUnlock()
	if ok {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok = s.chains[key]; ok {
		return c // another goroutine created it while we waited for the write lock
	}
	c = &chain{}
	s.chains[key] = c
	return c
}

// Read returns the newest version of key visible to tx.
// Returns ErrNotFound when no version exists or none is visible to tx.
func (s *VersionStore) Read(key string, tx *Transaction, mgr *TransactionManager) (*Tuple, error) {
	c, ok := s.getChain(key)
	if !ok {
		return nil, ErrNotFound
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for v := c.head; v != nil; v = v.prev {
		if mgr.IsVisible(v, tx) {
			return &Tuple{Key: key, Data: v.Data}, nil
		}
	}
	return nil, ErrNotFound
}

// Scan returns one Tuple per key for which at least one version is visible to tx.
// The order of returned tuples is not guaranteed because map iteration is random.
func (s *VersionStore) Scan(tx *Transaction, mgr *TransactionManager) []*Tuple {
	s.mu.RLock()
	keys := make([]string, 0, len(s.chains))
	for k := range s.chains {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	out := make([]*Tuple, 0, len(keys))
	for _, k := range keys {
		if t, err := s.Read(k, tx, mgr); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// Insert creates a new version for key with Xmin = tx.ID.
// Returns ErrKeyAlreadyExists if a version of key is already visible to tx.
func (s *VersionStore) Insert(key string, data []byte, tx *Transaction, mgr *TransactionManager) error {
	c := s.getOrCreateChain(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	for v := c.head; v != nil; v = v.prev {
		if mgr.IsVisible(v, tx) {
			return fmt.Errorf("%w: %s", ErrKeyAlreadyExists, key)
		}
	}
	ver := &Version{Data: data, Xmin: tx.ID, prev: c.head}
	c.head = ver
	tx.mu.Lock()
	tx.writes = append(tx.writes, writeOp{key: key, kind: opInsert, newVer: ver})
	tx.mu.Unlock()
	return nil
}

// Update sets Xmax = tx.ID on the currently visible version of key and prepends a
// new version with the given data. Returns ErrNotFound when no visible version exists.
// Returns ErrWriteConflict when another active transaction has already set Xmax
// (first-writer-wins policy).
func (s *VersionStore) Update(key string, data []byte, tx *Transaction, mgr *TransactionManager) error {
	c, ok := s.getChain(key)
	if !ok {
		return ErrNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var visible *Version
	for v := c.head; v != nil; v = v.prev {
		if mgr.IsVisible(v, tx) {
			visible = v
			break
		}
	}
	if visible == nil {
		return ErrNotFound
	}
	// Conflict: another active transaction has already marked this version for deletion.
	if visible.Xmax != 0 && visible.Xmax != tx.ID && mgr.isActive(visible.Xmax) {
		return fmt.Errorf("%w: key=%s conflicting-tx=%d", ErrWriteConflict, key, visible.Xmax)
	}
	visible.Xmax = tx.ID
	ver := &Version{Data: data, Xmin: tx.ID, prev: c.head}
	c.head = ver
	tx.mu.Lock()
	tx.writes = append(tx.writes, writeOp{key: key, kind: opUpdate, newVer: ver, oldVer: visible})
	tx.mu.Unlock()
	return nil
}

// Delete marks the currently visible version of key as deleted by tx.
// Returns ErrNotFound when no visible version exists. Returns ErrWriteConflict
// when another active transaction has already set Xmax.
func (s *VersionStore) Delete(key string, tx *Transaction, mgr *TransactionManager) error {
	c, ok := s.getChain(key)
	if !ok {
		return ErrNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for v := c.head; v != nil; v = v.prev {
		if mgr.IsVisible(v, tx) {
			if v.Xmax != 0 && v.Xmax != tx.ID && mgr.isActive(v.Xmax) {
				return fmt.Errorf("%w: key=%s conflicting-tx=%d", ErrWriteConflict, key, v.Xmax)
			}
			v.Xmax = tx.ID
			tx.mu.Lock()
			tx.writes = append(tx.writes, writeOp{key: key, kind: opDelete, oldVer: v})
			tx.mu.Unlock()
			return nil
		}
	}
	return ErrNotFound
}

// rollback undoes all writes in tx.writes in reverse insertion order.
// It is called only from Abort, after the transaction has been removed from the
// active set. Each undo step holds the per-chain write lock to prevent concurrent
// readers from observing an intermediate state.
func (s *VersionStore) rollback(tx *Transaction) {
	tx.mu.Lock()
	writes := make([]writeOp, len(tx.writes))
	copy(writes, tx.writes)
	tx.mu.Unlock()
	for i := len(writes) - 1; i >= 0; i-- {
		w := writes[i]
		c, ok := s.getChain(w.key)
		if !ok {
			continue
		}
		c.mu.Lock()
		switch w.kind {
		case opInsert:
			// Remove the inserted version from the head of the chain.
			if c.head == w.newVer {
				c.head = w.newVer.prev
			}
		case opUpdate:
			// Remove the new version and clear Xmax on the overwritten version.
			if c.head == w.newVer {
				c.head = w.newVer.prev
			}
			if w.oldVer != nil {
				w.oldVer.Xmax = 0
			}
		case opDelete:
			// Clear Xmax on the version that was marked for deletion.
			if w.oldVer != nil {
				w.oldVer.Xmax = 0
			}
		}
		c.mu.Unlock()
	}
}

// GarbageCollect removes versions that no active or future transaction can ever see.
// The low watermark is the minimum startedAt value among active transactions; any
// version deleted before that watermark is unreachable.
func (s *VersionStore) GarbageCollect(mgr *TransactionManager) {
	low := mgr.OldestActiveSnapshot()
	s.mu.RLock()
	keys := make([]string, 0, len(s.chains))
	for k := range s.chains {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	for _, k := range keys {
		c, ok := s.getChain(k)
		if !ok {
			continue
		}
		c.mu.Lock()
		gcChain(c, mgr, low)
		c.mu.Unlock()
	}
}

// gcChain prunes versions from c that no current or future transaction can see.
// A version is prunable when its Xmax was committed before low (the oldest active
// snapshot), because all future transactions will start with startedAt >= low and
// will therefore see commitLog[Xmax] <= startedAt, making the version invisible.
func gcChain(c *chain, mgr *TransactionManager, low uint64) {
	if c.head == nil {
		return
	}
	// If the head version itself was deleted before low, the entire chain is obsolete.
	if c.head.Xmax != 0 && mgr.committedBefore(c.head.Xmax, low) {
		c.head = nil
		return
	}
	// Walk from head toward older versions and prune the tail at the first
	// version whose Xmax committed before low.
	for cur := c.head; cur != nil; cur = cur.prev {
		older := cur.prev
		if older == nil {
			break
		}
		if older.Xmax != 0 && mgr.committedBefore(older.Xmax, low) {
			cur.prev = nil // prune older and everything behind it
			break
		}
	}
}
```

`Read` holds the chain's read lock for the whole walk so a concurrent `Update` cannot relink the chain mid-traversal. `GarbageCollect` consults `OldestActiveSnapshot` once, then truncates each chain at the first version whose deletion committed at or below that watermark — everything behind it is unreachable by any present or future snapshot.

### The runnable demo

The demo stages the canonical snapshot-isolation story: a writer commits two rows, a reader opens its snapshot, a second writer raises a price and commits, and the reader — its snapshot frozen before that commit — still sees the old price. It then shows abort restoring a row's prior value and a final GC pass with no active transactions.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/mvcc"
)

func main() {
	mgr := mvcc.NewTransactionManager()
	store := mvcc.NewVersionStore()

	// T1 inserts two product rows and commits.
	t1 := mgr.Begin()
	if err := store.Insert("product:1", []byte(`{"name":"Widget","price":9.99}`), t1, mgr); err != nil {
		log.Fatal(err)
	}
	if err := store.Insert("product:2", []byte(`{"name":"Gadget","price":19.99}`), t1, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(t1); err != nil {
		log.Fatal(err)
	}
	fmt.Println("T1 committed: inserted product:1 and product:2")

	// T2 begins its read transaction -- snapshot is captured here.
	t2 := mgr.Begin()

	// T3 raises the price on product:1 and commits.
	t3 := mgr.Begin()
	if err := store.Update("product:1", []byte(`{"name":"Widget","price":12.99}`), t3, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(t3); err != nil {
		log.Fatal(err)
	}
	fmt.Println("T3 committed: raised product:1 price to 12.99")

	// T2 reads product:1 -- its snapshot predates T3's commit, so it still sees 9.99.
	tup, err := store.Read("product:1", t2, mgr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("T2 reads product:1 (snapshot view): %s\n", tup.Data)
	if err := mgr.Commit(t2); err != nil {
		log.Fatal(err)
	}

	// T4 demonstrates abort/rollback: update product:2 then abort.
	t4 := mgr.Begin()
	if err := store.Update("product:2", []byte(`{"name":"Gadget","price":0.01}`), t4, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Abort(t4, store); err != nil {
		log.Fatal(err)
	}
	fmt.Println("T4 aborted: product:2 price rollback")

	// T5 confirms product:2 still has its original price after T4's abort.
	t5 := mgr.Begin()
	tup2, err := store.Read("product:2", t5, mgr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("T5 reads product:2 (after abort): %s\n", tup2.Data)
	_ = mgr.Commit(t5)

	// Garbage-collect: no active transactions remain, so obsolete versions are safe to drop.
	store.GarbageCollect(mgr)
	fmt.Println("GC complete")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
T1 committed: inserted product:1 and product:2
T3 committed: raised product:1 price to 12.99
T2 reads product:1 (snapshot view): {"name":"Widget","price":9.99}
T4 aborted: product:2 price rollback
T5 reads product:2 (after abort): {"name":"Gadget","price":19.99}
GC complete
```

### Tests

The tests pin every property of the engine independently. `TestSnapshotIsolation` is the centerpiece: a reader whose snapshot predates a concurrent update must still read the old value. `TestSnapshotIsolationAfterDelete` is the symmetric case for the deleter half of the predicate — a snapshot taken before a delete commits still sees the row, because the deleter's `Xmax` lies in the reader's future. The remaining tests cover read-your-own-writes, first-writer-wins conflict, abort rollback, delete visibility, insert-already-exists, GC retention of the current version, and a 50-goroutine stress test that must stay clean under `-race`.

Create `mvcc_test.go`:

```go
package mvcc

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestSnapshotIsolation(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	// T1 inserts "k1" with value "v1" and commits.
	t1 := mgr.Begin()
	if err := store.Insert("k1", []byte("v1"), t1, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(t1); err != nil {
		t.Fatal(err)
	}

	// T2 captures its snapshot here — before T3 commits.
	t2 := mgr.Begin()

	// T3 updates "k1" to "v2" and commits.
	t3 := mgr.Begin()
	if err := store.Update("k1", []byte("v2"), t3, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(t3); err != nil {
		t.Fatal(err)
	}

	// T2's snapshot predates T3's commit, so T2 must still read "v1".
	tup, err := store.Read("k1", t2, mgr)
	if err != nil {
		t.Fatalf("T2 read failed: %v", err)
	}
	if string(tup.Data) != "v1" {
		t.Fatalf("T2 read %q, want v1 (snapshot isolation broken)", string(tup.Data))
	}
	_ = mgr.Commit(t2)
}

func TestReadYourOwnWrites(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	tx := mgr.Begin()
	if err := store.Insert("k1", []byte("myval"), tx, mgr); err != nil {
		t.Fatal(err)
	}
	// The inserting transaction must see its own uncommitted insert.
	tup, err := store.Read("k1", tx, mgr)
	if err != nil {
		t.Fatalf("read-your-own-writes failed: %v", err)
	}
	if string(tup.Data) != "myval" {
		t.Fatalf("got %q, want myval", string(tup.Data))
	}
	_ = mgr.Commit(tx)
}

func TestWriteWriteConflict(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	// Establish a committed base version.
	base := mgr.Begin()
	if err := store.Insert("k1", []byte("base"), base, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(base); err != nil {
		t.Fatal(err)
	}

	// T1 updates "k1" but does not commit yet.
	t1 := mgr.Begin()
	if err := store.Update("k1", []byte("t1v"), t1, mgr); err != nil {
		t.Fatal(err)
	}

	// T2 tries to update the same key while T1 is active: first-writer-wins.
	t2 := mgr.Begin()
	err := store.Update("k1", []byte("t2v"), t2, mgr)
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("expected ErrWriteConflict, got %v", err)
	}
	_ = mgr.Abort(t2, store)
	_ = mgr.Commit(t1)
}

func TestAbortRollsBackWrites(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	base := mgr.Begin()
	if err := store.Insert("k1", []byte("orig"), base, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(base); err != nil {
		t.Fatal(err)
	}

	t1 := mgr.Begin()
	if err := store.Update("k1", []byte("changed"), t1, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Abort(t1, store); err != nil {
		t.Fatal(err)
	}

	// After abort, a new transaction must see the original "orig".
	t2 := mgr.Begin()
	tup, err := store.Read("k1", t2, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if string(tup.Data) != "orig" {
		t.Fatalf("after abort: got %q, want orig", string(tup.Data))
	}
	_ = mgr.Commit(t2)
}

func TestDeleteAndVisibility(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	base := mgr.Begin()
	if err := store.Insert("k1", []byte("row"), base, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(base); err != nil {
		t.Fatal(err)
	}

	t1 := mgr.Begin()
	if err := store.Delete("k1", t1, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(t1); err != nil {
		t.Fatal(err)
	}

	// Transaction started after the delete was committed must not see "k1".
	t2 := mgr.Begin()
	_, err := store.Read("k1", t2, mgr)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete+commit, got %v", err)
	}
	_ = mgr.Commit(t2)
}

// TestSnapshotIsolationAfterDelete proves a snapshot taken before a delete commits
// still sees the row: the deleter's Xmax sits in the reader's future.
func TestSnapshotIsolationAfterDelete(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	base := mgr.Begin()
	if err := store.Insert("k1", []byte("row"), base, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(base); err != nil {
		t.Fatal(err)
	}

	// T2 captures its snapshot before the delete commits.
	t2 := mgr.Begin()

	// A separate transaction deletes the key and commits.
	del := mgr.Begin()
	if err := store.Delete("k1", del, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(del); err != nil {
		t.Fatal(err)
	}

	// T2's snapshot predates the delete, so it must still read "row".
	tup, err := store.Read("k1", t2, mgr)
	if err != nil {
		t.Fatalf("T2 read after concurrent delete: %v", err)
	}
	if string(tup.Data) != "row" {
		t.Fatalf("T2 read %q, want row (snapshot must predate the delete)", string(tup.Data))
	}
	_ = mgr.Commit(t2)
}

func TestInsertConflictsWithExistingKey(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	base := mgr.Begin()
	if err := store.Insert("k1", []byte("first"), base, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(base); err != nil {
		t.Fatal(err)
	}

	tx := mgr.Begin()
	err := store.Insert("k1", []byte("second"), tx, mgr)
	if !errors.Is(err, ErrKeyAlreadyExists) {
		t.Fatalf("expected ErrKeyAlreadyExists, got %v", err)
	}
	_ = mgr.Abort(tx, store)
}

func TestGarbageCollect(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	t1 := mgr.Begin()
	if err := store.Insert("k1", []byte("v1"), t1, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(t1); err != nil {
		t.Fatal(err)
	}

	t2 := mgr.Begin()
	if err := store.Update("k1", []byte("v2"), t2, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(t2); err != nil {
		t.Fatal(err)
	}

	// No active transactions: the old "v1" version is safe to collect.
	store.GarbageCollect(mgr)

	// The current version must still be readable after GC.
	t3 := mgr.Begin()
	tup, err := store.Read("k1", t3, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if string(tup.Data) != "v2" {
		t.Fatalf("after GC: got %q, want v2", string(tup.Data))
	}
	_ = mgr.Commit(t3)
}

func TestConcurrentTransactions(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	// Pre-populate ten keys so goroutines have rows to read and update.
	for i := 0; i < 10; i++ {
		tx := mgr.Begin()
		key := fmt.Sprintf("k%d", i)
		if err := store.Insert(key, []byte("init"), tx, mgr); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(tx); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tx := mgr.Begin()
			key := fmt.Sprintf("k%d", n%10)
			switch n % 3 {
			case 0:
				_, _ = store.Read(key, tx, mgr)
				_ = mgr.Commit(tx)
			case 1:
				err := store.Update(key, []byte("upd"), tx, mgr)
				if errors.Is(err, ErrWriteConflict) {
					_ = mgr.Abort(tx, store)
				} else {
					_ = mgr.Commit(tx)
				}
			case 2:
				newKey := fmt.Sprintf("new%d", n)
				_ = store.Insert(newKey, []byte("new"), tx, mgr)
				_ = mgr.Commit(tx)
			}
		}(i)
	}
	wg.Wait()
}

func ExampleVersionStore_Read() {
	mgr := NewTransactionManager()
	store := NewVersionStore()

	writer := mgr.Begin()
	_ = store.Insert("user:1", []byte(`{"name":"Alice"}`), writer, mgr)
	_ = mgr.Commit(writer)

	reader := mgr.Begin()
	tup, _ := store.Read("user:1", reader, mgr)
	fmt.Printf("key=%s data=%s\n", tup.Key, tup.Data)
	_ = mgr.Commit(reader)
	// Output:
	// key=user:1 data={"name":"Alice"}
}
```

## Review

The engine is correct when the visibility predicate is correct and everything else is a walk over it. Confirm that a reader whose snapshot predates a concurrent update still reads the old value, and that the same holds across a concurrent delete — both are direct tests of the two halves of `IsVisible`, and both hinge on the `<=` comparison in `committedBefore`. A writer that reaches a row already marked by an active transaction must receive `errors.Is(err, ErrWriteConflict)`; an aborted update must leave the prior value intact; a transaction must read its own uncommitted writes. The 50-goroutine test must stay clean under `-race`, which is the proof that the per-chain locking holds the read-modify-write of each conflict check atomic and that rollback never tears a chain a reader is walking.

The mistakes that bite here are the universal MVCC ones: a strict `<` in `committedBefore` that hides a just-committed predecessor; releasing the chain lock between the conflict check and the version prepend so two writers both think they won; and calling Abort after a successful Commit, which finds the transaction already gone and silently rolls nothing back.

## Resources

- [PostgreSQL: Introduction to MVCC](https://www.postgresql.org/docs/current/mvcc-intro.html) — the production system whose `xmin`/`xmax` model this engine mirrors.
- [CMU 15-445/645 lecture 19: Multi-Version Concurrency Control](https://15445.courses.cs.cmu.edu/fall2024/notes/19-multiversioning.pdf) — version storage, visibility, and the design space.
- [Brandur Leach: How Postgres Makes Transactions Atomic](https://brandur.org/postgres-atomicity) — a careful walk through snapshots and the commit-sequence argument.
- [pkg.go.dev: sync/atomic — atomic.Uint64](https://pkg.go.dev/sync/atomic#Uint64) — the monotone counter backing the commit sequence.

---

Next: [02-vacuum-and-the-low-watermark.md](02-vacuum-and-the-low-watermark.md)
