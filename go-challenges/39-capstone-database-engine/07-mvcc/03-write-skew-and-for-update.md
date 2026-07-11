# Exercise 3: Write Skew and the FOR UPDATE Guard

Snapshot isolation forbids dirty reads, non-repeatable reads, and lost updates, but Berenson et al. proved it is still weaker than serializable. This exercise demonstrates the gap concretely: two transactions read an overlapping set and then write *different* rows, so first-writer-wins detects no conflict, both commit, and together they break a cross-row invariant that each believed it preserved. That is write skew. Then it implements `ForUpdate` — the SELECT ... FOR UPDATE pattern — which materializes a read as a write intent, converting the invisible read-write conflict into a detectable first-writer-wins `ErrWriteConflict` and restoring serializable behavior for that pattern.

Snapshot isolation does *not* prevent write skew; the engine here exhibits it. The guard is a deliberate extra step the application takes, exactly as a real SI database requires.

This module is fully self-contained. It reproduces the version-store scaffolding so it stands alone, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
mvcc.go            TxID, Version, Transaction, TransactionManager (scaffolding)
store.go           VersionStore, Read/Insert/Update/Delete (scaffolding)
writeskew.go       ForUpdate
cmd/
  demo/
    main.go        the unguarded anomaly, then the FOR UPDATE guard holding the invariant
writeskew_test.go  write-skew anomaly, ForUpdate guard (table-driven), ForUpdate on missing key
```

- Files: `mvcc.go`, `store.go`, `writeskew.go`, `cmd/demo/main.go`, `writeskew_test.go`.
- Implement: `(*VersionStore).ForUpdate`, on top of the reproduced core engine.
- Test: `writeskew_test.go` proves the unguarded anomaly (both commit, invariant violated), that locking the read-set with `ForUpdate` forces the second transaction to abort regardless of lock order, and that `ForUpdate` on a missing key returns `ErrNotFound`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p mvcc-write-skew/cmd/demo && cd mvcc-write-skew
go mod init example.com/mvcc-write-skew
```

### Why write skew slips past first-writer-wins

First-writer-wins detects a conflict only when two transactions write the *same* row: the second writer finds `Xmax` already stamped by an active transaction and aborts. Write skew is the case the policy is structurally blind to. Take the doctors-on-call invariant: rows `x` and `y` are 0/1 flags and the rule is `x + y >= 1` (at least one doctor on call). Two transactions begin against the same snapshot where both flags are 1. Each reads both rows, sees the sum is 2, and concludes it is safe to take its own doctor off call. The first writes `x = 0`; the second writes `y = 0`. The writes touch *disjoint* rows, so there is no write-write conflict — neither transaction stamps a row the other is also writing — and both commit. The committed state has `x = 0` and `y = 0`, a sum of 0, and the invariant is violated even though each transaction in isolation preserved it. No serial order of the two could have produced that state, which is the definition of a non-serializable execution.

The fix is to make the read a write. `ForUpdate` reads the currently visible version and then immediately `Update`s it with the same data — the value is unchanged, but the update stamps `Xmax` on the prior version and prepends a new one owned by the caller. Now the read carries a write intent. If two transactions both try to `ForUpdate` the same row, the second hits first-writer-wins and gets `ErrWriteConflict`. So a transaction that locks its *entire read-set* for update before deciding forces any concurrent transaction sharing even one row of that set to abort. The invisible read-write conflict has become a visible write-write conflict the engine already knows how to catch. This is the manual equivalent of what Serializable Snapshot Isolation (Cahill et al., PostgreSQL's SERIALIZABLE level) does automatically by tracking read-write dependency edges.

First, reproduce the core engine so this module stands alone.

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
func (m *TransactionManager) Begin() *Transaction {
	id := TxID(m.nextID.Add(1))
	snap := m.commitSeq.Load()
	tx := &Transaction{ID: id, startedAt: snap}
	m.activeMu.Lock()
	m.active[id] = tx
	m.activeMu.Unlock()
	return tx
}

// Commit finalises tx and records it in the commit log with the next sequence number.
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
func (m *TransactionManager) Abort(tx *Transaction, store *VersionStore) error {
	m.activeMu.Lock()
	if _, ok := m.active[tx.ID]; !ok {
		m.activeMu.Unlock()
		return fmt.Errorf("%w: tx %d", ErrTxNotActive, tx.ID)
	}
	delete(m.active, tx.ID)
	m.activeMu.Unlock()
	store.rollback(tx)
	return nil
}

// committedBefore reports whether txID committed at a sequence at or below seqBound.
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
// transactions, or ^uint64(0) when none are active (the low watermark for GC).
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
func (m *TransactionManager) IsVisible(ver *Version, tx *Transaction) bool {
	creatorOK := ver.Xmin == tx.ID || m.committedBefore(ver.Xmin, tx.startedAt)
	if !creatorOK {
		return false
	}
	if ver.Xmax == 0 {
		return true
	}
	if ver.Xmax == tx.ID {
		return false
	}
	return !m.committedBefore(ver.Xmax, tx.startedAt)
}
```

Create `store.go`:

```go
package mvcc

import (
	"fmt"
	"sync"
)

// chain holds the version chain for one key protected by its own read-write lock.
type chain struct {
	mu   sync.RWMutex
	head *Version
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
		return c
	}
	c = &chain{}
	s.chains[key] = c
	return c
}

// Read returns the newest version of key visible to tx.
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

// Update sets Xmax on the visible version of key and prepends a new version.
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
			if c.head == w.newVer {
				c.head = w.newVer.prev
			}
		case opUpdate:
			if c.head == w.newVer {
				c.head = w.newVer.prev
			}
			if w.oldVer != nil {
				w.oldVer.Xmax = 0
			}
		case opDelete:
			if w.oldVer != nil {
				w.oldVer.Xmax = 0
			}
		}
		c.mu.Unlock()
	}
}
```

Now the FOR UPDATE guard.

Create `writeskew.go`:

```go
package mvcc

// ForUpdate acquires a write intent on key for tx by materializing the currently
// visible version as a fresh version owned by tx, the equivalent of SELECT ... FOR
// UPDATE. The row data is unchanged, but the write stamps Xmax on the prior version,
// so any concurrent writer or ForUpdate caller on the same key collides under the
// first-writer-wins policy and receives ErrWriteConflict. This converts an invisible
// read-write conflict into a detectable write-write conflict, the manual guard
// against write skew under snapshot isolation.
//
// Returns ErrNotFound when no version of key is visible to tx, and ErrWriteConflict
// when another active transaction already holds the intent.
func (s *VersionStore) ForUpdate(key string, tx *Transaction, mgr *TransactionManager) (*Tuple, error) {
	tup, err := s.Read(key, tx, mgr)
	if err != nil {
		return nil, err
	}
	if err := s.Update(key, tup.Data, tx, mgr); err != nil {
		return nil, err
	}
	return tup, nil
}
```

### The runnable demo

The demo runs both halves back to back. First the unguarded anomaly: two transactions each take a flag to 0 on disjoint rows, both commit, and the invariant `x + y >= 1` is violated (the sum is 0). Then the guarded version: the first transaction locks both rows with `ForUpdate`, the second collides and aborts, and the surviving transaction commits alone with the invariant intact (the sum stays 1).

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"

	"example.com/mvcc-write-skew"
)

// readInt reads key as an integer flag (0 or 1) under tx's snapshot.
func readInt(store *mvcc.VersionStore, mgr *mvcc.TransactionManager, tx *mvcc.Transaction, key string) int {
	tup, err := store.Read(key, tx, mgr)
	if err != nil {
		log.Fatal(err)
	}
	n, err := strconv.Atoi(string(tup.Data))
	if err != nil {
		log.Fatal(err)
	}
	return n
}

func seed(store *mvcc.VersionStore, mgr *mvcc.TransactionManager) {
	tx := mgr.Begin()
	if err := store.Insert("x", []byte("1"), tx, mgr); err != nil {
		log.Fatal(err)
	}
	if err := store.Insert("y", []byte("1"), tx, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(tx); err != nil {
		log.Fatal(err)
	}
}

func main() {
	// Part 1: the unguarded write-skew anomaly. The invariant is x+y >= 1.
	mgr := mvcc.NewTransactionManager()
	store := mvcc.NewVersionStore()
	seed(store, mgr)

	t1 := mgr.Begin()
	t2 := mgr.Begin()
	// Each sees sum 2 and takes one flag to 0, believing the other stays at 1.
	_ = readInt(store, mgr, t1, "x") + readInt(store, mgr, t1, "y")
	_ = readInt(store, mgr, t2, "x") + readInt(store, mgr, t2, "y")
	if err := store.Update("x", []byte("0"), t1, mgr); err != nil {
		log.Fatal(err)
	}
	if err := store.Update("y", []byte("0"), t2, mgr); err != nil {
		log.Fatal(err)
	}
	// Disjoint rows: no write-write conflict, so both commit.
	if err := mgr.Commit(t1); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(t2); err != nil {
		log.Fatal(err)
	}
	final := mgr.Begin()
	sum := readInt(store, mgr, final, "x") + readInt(store, mgr, final, "y")
	_ = mgr.Commit(final)
	fmt.Printf("unguarded write skew: x+y = %d (invariant x+y >= 1 violated)\n", sum)

	// Part 2: the FOR UPDATE guard materializes the reads as write intents.
	mgr2 := mvcc.NewTransactionManager()
	store2 := mvcc.NewVersionStore()
	seed(store2, mgr2)

	ta := mgr2.Begin()
	tb := mgr2.Begin()
	if _, err := store2.ForUpdate("x", ta, mgr2); err != nil {
		log.Fatal(err)
	}
	if _, err := store2.ForUpdate("y", ta, mgr2); err != nil {
		log.Fatal(err)
	}
	_, errX := store2.ForUpdate("x", tb, mgr2)
	fmt.Printf("guarded: t2 ForUpdate(x) -> conflict=%t\n", errors.Is(errX, mvcc.ErrWriteConflict))
	if err := mgr2.Abort(tb, store2); err != nil {
		log.Fatal(err)
	}
	if err := store2.Update("x", []byte("0"), ta, mgr2); err != nil {
		log.Fatal(err)
	}
	if err := mgr2.Commit(ta); err != nil {
		log.Fatal(err)
	}
	final2 := mgr2.Begin()
	sum2 := readInt(store2, mgr2, final2, "x") + readInt(store2, mgr2, final2, "y")
	_ = mgr2.Commit(final2)
	fmt.Printf("guarded with FOR UPDATE: x+y = %d (invariant holds)\n", sum2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unguarded write skew: x+y = 0 (invariant x+y >= 1 violated)
guarded: t2 ForUpdate(x) -> conflict=true
guarded with FOR UPDATE: x+y = 1 (invariant holds)
```

### Tests

`TestWriteSkewUnderSnapshotIsolation` is the proof that SI is not serializable: both transactions commit and the sum drops to 0, so the test *expects* the anomaly and fails if the invariant somehow held. `TestWriteSkewGuardedByForUpdate` is table-driven over lock order — t1 locks x-then-y, and separately y-then-x — to show the guard works regardless of which row the second transaction reaches first; in both cases t2 collides on both rows and must abort, and the invariant survives. `TestForUpdateOnMissingKey` confirms the lock attempt surfaces the same `ErrNotFound` sentinel as a plain read.

Create `writeskew_test.go`:

```go
package mvcc

import (
	"errors"
	"strconv"
	"testing"
)

// onCall parses a stored row as an integer flag (1 = on call, 0 = off).
func onCall(t *testing.T, store *VersionStore, mgr *TransactionManager, tx *Transaction, key string) int {
	t.Helper()
	tup, err := store.Read(key, tx, mgr)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	n, err := strconv.Atoi(string(tup.Data))
	if err != nil {
		t.Fatalf("parse %q: %v", key, err)
	}
	return n
}

func seedOnCall(t *testing.T, store *VersionStore, mgr *TransactionManager) {
	t.Helper()
	tx := mgr.Begin()
	if err := store.Insert("x", []byte("1"), tx, mgr); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert("y", []byte("1"), tx, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(tx); err != nil {
		t.Fatal(err)
	}
}

// TestWriteSkewUnderSnapshotIsolation shows that two transactions under snapshot
// isolation can both commit while violating the cross-row invariant x+y >= 1,
// proving SI is not serializable.
func TestWriteSkewUnderSnapshotIsolation(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()
	seedOnCall(t, store, mgr)

	// Both transactions see x=1, y=1 (sum 2) and each takes one doctor off call,
	// each believing the other stays on call.
	t1 := mgr.Begin()
	t2 := mgr.Begin()
	if got := onCall(t, store, mgr, t1, "x") + onCall(t, store, mgr, t1, "y"); got != 2 {
		t.Fatalf("t1 initial sum = %d, want 2", got)
	}
	if got := onCall(t, store, mgr, t2, "x") + onCall(t, store, mgr, t2, "y"); got != 2 {
		t.Fatalf("t2 initial sum = %d, want 2", got)
	}

	// t1 sets x=0; t2 sets y=0. Different rows, so no write-write conflict.
	if err := store.Update("x", []byte("0"), t1, mgr); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("y", []byte("0"), t2, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(t1); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	if err := mgr.Commit(t2); err != nil {
		t.Fatalf("t2 commit: %v", err)
	}

	// Both committed; the invariant x+y >= 1 is now violated.
	final := mgr.Begin()
	sum := onCall(t, store, mgr, final, "x") + onCall(t, store, mgr, final, "y")
	_ = mgr.Commit(final)
	if sum >= 1 {
		t.Fatalf("expected write-skew anomaly (sum < 1), got sum = %d", sum)
	}
}

// TestWriteSkewGuardedByForUpdate shows that materializing the read-set with
// ForUpdate (SELECT ... FOR UPDATE) turns the silent read-write conflict into a
// first-writer-wins ErrWriteConflict, so the second transaction is forced to abort
// and the invariant holds.
func TestWriteSkewGuardedByForUpdate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		firstLock  string
		secondLock string
	}{
		{"t1 locks x then y", "x", "y"},
		{"t1 locks y then x", "y", "x"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr := NewTransactionManager()
			store := NewVersionStore()
			seedOnCall(t, store, mgr)

			t1 := mgr.Begin()
			t2 := mgr.Begin()

			// t1 locks its whole read-set {x, y} for update.
			if _, err := store.ForUpdate(tc.firstLock, t1, mgr); err != nil {
				t.Fatalf("t1 lock %q: %v", tc.firstLock, err)
			}
			if _, err := store.ForUpdate(tc.secondLock, t1, mgr); err != nil {
				t.Fatalf("t1 lock %q: %v", tc.secondLock, err)
			}

			// t2 now tries to lock the same rows and must hit the conflict on both.
			_, errX := store.ForUpdate("x", t2, mgr)
			_, errY := store.ForUpdate("y", t2, mgr)
			if !errors.Is(errX, ErrWriteConflict) || !errors.Is(errY, ErrWriteConflict) {
				t.Fatalf("expected ErrWriteConflict for t2, got errX=%v errY=%v", errX, errY)
			}
			if err := mgr.Abort(t2, store); err != nil {
				t.Fatalf("t2 abort: %v", err)
			}

			// t1 proceeds with its decision and commits alone.
			if err := store.Update("x", []byte("0"), t1, mgr); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Commit(t1); err != nil {
				t.Fatalf("t1 commit: %v", err)
			}

			final := mgr.Begin()
			sum := onCall(t, store, mgr, final, "x") + onCall(t, store, mgr, final, "y")
			_ = mgr.Commit(final)
			if sum < 1 {
				t.Fatalf("invariant x+y >= 1 violated despite guard: sum = %d", sum)
			}
		})
	}
}

// TestForUpdateOnMissingKey confirms ForUpdate surfaces the same sentinel as Read
// when the key was never inserted: the lock attempt fails fast with ErrNotFound.
func TestForUpdateOnMissingKey(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	tx := mgr.Begin()
	_, err := store.ForUpdate("ghost", tx, mgr)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ForUpdate on missing key = %v, want ErrNotFound", err)
	}
	_ = mgr.Commit(tx)
}
```

## Review

The exercise is correct when the anomaly is real and the guard removes it. The unguarded test must show both transactions committing and the invariant violated — if it ever holds, the engine is doing more than snapshot isolation and the demonstration is wrong. The guarded test must abort the second transaction regardless of lock order, because a real read-set is locked in whatever order the application visits it, and the guard cannot depend on a lucky ordering. `ForUpdate` on a missing key must return `errors.Is(err, ErrNotFound)`, the same sentinel a read would, so callers handle the absent-row case uniformly.

The conceptual trap to avoid is believing snapshot isolation alone prevents write skew; it does not, and an engine that markets SI as serializable is making a false promise. The guard is the application's responsibility under SI, or the database's under a true SERIALIZABLE level (SSI), but never a free property of snapshot reads.

## Resources

- [Berenson, Bernstein, Gray, Melton, O'Neil, O'Neil: A Critique of ANSI SQL Isolation Levels (1995)](https://www.microsoft.com/en-us/research/publication/a-critique-of-ansi-sql-isolation-levels/) — defines snapshot isolation and proves it admits write skew.
- [Cahill, Rohm, Fekete: Serializable Isolation for Snapshot Databases (SSI)](https://dl.acm.org/doi/10.1145/1620585.1620587) — the dependency-tracking scheme PostgreSQL's SERIALIZABLE level implements.
- [PostgreSQL: Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) — the worked write-skew example and the `SELECT ... FOR UPDATE` / SERIALIZABLE remedies.

---

Back to [02-vacuum-and-the-low-watermark.md](02-vacuum-and-the-low-watermark.md) | Next: [04-statement-vs-transaction-snapshots.md](04-statement-vs-transaction-snapshots.md)
