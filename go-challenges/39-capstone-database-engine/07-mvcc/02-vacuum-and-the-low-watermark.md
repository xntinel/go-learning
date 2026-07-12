# Exercise 2: Vacuum Below the Low Watermark

The core engine's `GarbageCollect` truncates obsolete chain tails but reports nothing, which makes it impossible to test or tune. This exercise builds `Vacuum`, a reclamation pass that returns how much work it did, plus a `VersionCount` diagnostic for inspecting chain length. The point it proves is the operational heart of MVCC garbage collection: a version is reclaimable only when its deletion committed at or below the oldest active snapshot, so a single long-running transaction holds the low watermark down and pins every version superseded since it began — the same mechanism behind table bloat from idle-in-transaction sessions in production.

This module is fully self-contained. It reproduces the version-store scaffolding so it stands alone, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
mvcc.go          TxID, Version, Transaction, TransactionManager (scaffolding)
store.go         VersionStore, Read/Insert/Update/Delete, gcChain (scaffolding)
vacuum.go        VacuumStats, Vacuum, VersionCount, chainLen
cmd/
  demo/
    main.go      churn a row, vacuum it, then watch a long-running txn pin the tail
vacuum_test.go   reclaim-below-watermark, pinned-by-long-txn, concurrent vacuum + readers
```

- Files: `mvcc.go`, `store.go`, `vacuum.go`, `cmd/demo/main.go`, `vacuum_test.go`.
- Implement: `VacuumStats`, `(*VersionStore).Vacuum`, `(*VersionStore).VersionCount`, and `chainLen`, on top of the reproduced core engine.
- Test: `vacuum_test.go` proves a dead version below the watermark reclaims while a version pinned by a live snapshot is retained, that a long-running transaction blocks all reclamation until it commits, and that concurrent vacuumers, writers, and readers never tear a chain under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/07-mvcc/02-vacuum-and-the-low-watermark/cmd/demo && cd go-solutions/39-capstone-database-engine/07-mvcc/02-vacuum-and-the-low-watermark
```

### Why a version is reclaimable only below the watermark

A version is dead when no current or future transaction can ever see it. The current half is easy — no active transaction's snapshot includes the deletion. The future half is the subtle one: every transaction that begins from now on captures `startedAt >= lowWatermark`, where the low watermark is the minimum `startedAt` over all active transactions (`OldestActiveSnapshot`). If a superseded version's `Xmax` committed at a sequence at or below that watermark, then every present and future transaction will evaluate `commitLog[Xmax] <= startedAt` as true and skip the version. Only then is it safe to unlink.

Turn that around and you get the bloat hazard directly. A long-running transaction `tOld` holds the watermark down at its old snapshot. Every version superseded after `tOld` began has an `Xmax` that committed at a sequence *above* `tOld.startedAt`, so none of them clears the bar — they are all pinned, even though `tOld` may only actually need one of them. Reclamation cannot advance past the oldest open snapshot, period. When `tOld` finally commits and no transactions remain, `OldestActiveSnapshot` returns the maximum `uint64`, every superseded version clears the bar at once, and the tail collapses to the single live head.

`Vacuum` measures this. It snapshots the key set under the store lock, then locks each chain, counts the versions before and after pruning with `chainLen`, and accumulates the difference into `VacuumStats.Reclaimed`. Because the pruning reuses the same `gcChain` the core engine already trusts, `Vacuum` adds only the accounting; the reclamation rule itself is unchanged. `VersionCount` is a read-locked length probe for tests and tuning.

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

// GarbageCollect removes versions that no active or future transaction can ever see.
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

// gcChain prunes versions from c whose Xmax committed at or below low.
func gcChain(c *chain, mgr *TransactionManager, low uint64) {
	if c.head == nil {
		return
	}
	if c.head.Xmax != 0 && mgr.committedBefore(c.head.Xmax, low) {
		c.head = nil
		return
	}
	for cur := c.head; cur != nil; cur = cur.prev {
		older := cur.prev
		if older == nil {
			break
		}
		if older.Xmax != 0 && mgr.committedBefore(older.Xmax, low) {
			cur.prev = nil
			break
		}
	}
}
```

Now the vacuum pass itself.

Create `vacuum.go`:

```go
package mvcc

// VacuumStats reports the outcome of a Vacuum pass.
type VacuumStats struct {
	Scanned   int // version-chain nodes inspected
	Reclaimed int // dead versions unlinked from chains
}

// Vacuum reclaims dead versions whose deleting transaction committed at or below
// the low watermark (the oldest active snapshot). Versions still reachable by any
// live snapshot are preserved. It returns counts describing the work performed.
func (s *VersionStore) Vacuum(mgr *TransactionManager) VacuumStats {
	low := mgr.OldestActiveSnapshot()
	s.mu.RLock()
	keys := make([]string, 0, len(s.chains))
	for k := range s.chains {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	var stats VacuumStats
	for _, k := range keys {
		c, ok := s.getChain(k)
		if !ok {
			continue
		}
		c.mu.Lock()
		before := chainLen(c.head)
		gcChain(c, mgr, low)
		after := chainLen(c.head)
		c.mu.Unlock()
		stats.Scanned += before
		stats.Reclaimed += before - after
	}
	return stats
}

// VersionCount returns the number of versions linked in key's chain, or 0 when the
// key has no chain. It is a diagnostic helper for tests and GC tuning.
func (s *VersionStore) VersionCount(key string) int {
	c, ok := s.getChain(key)
	if !ok {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return chainLen(c.head)
}

// chainLen counts the versions reachable from head via prev pointers.
func chainLen(head *Version) int {
	n := 0
	for v := head; v != nil; v = v.prev {
		n++
	}
	return n
}
```

### The runnable demo

The demo makes the watermark visible. It churns a row to a four-version chain, vacuums with no transactions active (so the watermark is maximal and the three dead versions collapse to one), then opens a long-running transaction, churns the row twice more, and vacuums again — this time reclaiming nothing, because the open transaction pins the tail. Only after it commits does a final vacuum free the dead versions.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strconv"

	"example.com/mvcc-vacuum"
)

func main() {
	mgr := mvcc.NewTransactionManager()
	store := mvcc.NewVersionStore()

	// Seed one row, then churn it three times so the chain grows.
	seed := mgr.Begin()
	if err := store.Insert("counter", []byte("0"), seed, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(seed); err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		tx := mgr.Begin()
		if err := store.Update("counter", []byte(strconv.Itoa(i)), tx, mgr); err != nil {
			log.Fatal(err)
		}
		if err := mgr.Commit(tx); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("versions before vacuum: %d\n", store.VersionCount("counter"))

	// No transactions are active: the watermark is maximal and dead tails reclaim.
	stats := store.Vacuum(mgr)
	fmt.Printf("vacuum: scanned=%d reclaimed=%d\n", stats.Scanned, stats.Reclaimed)
	fmt.Printf("versions after vacuum: %d\n", store.VersionCount("counter"))

	// A long-running transaction holds the watermark down and pins old versions.
	tOld := mgr.Begin()
	for i := 4; i <= 5; i++ {
		tx := mgr.Begin()
		if err := store.Update("counter", []byte(strconv.Itoa(i)), tx, mgr); err != nil {
			log.Fatal(err)
		}
		if err := mgr.Commit(tx); err != nil {
			log.Fatal(err)
		}
	}
	pinned := store.Vacuum(mgr)
	fmt.Printf("with long-running txn open: reclaimed=%d (versions pinned)\n", pinned.Reclaimed)

	// Once it commits, the watermark advances and the tail is finally freed.
	if err := mgr.Commit(tOld); err != nil {
		log.Fatal(err)
	}
	freed := store.Vacuum(mgr)
	fmt.Printf("after long-running txn commits: reclaimed=%d\n", freed.Reclaimed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
versions before vacuum: 4
vacuum: scanned=4 reclaimed=3
versions after vacuum: 1
with long-running txn open: reclaimed=0 (versions pinned)
after long-running txn commits: reclaimed=2
```

### Tests

`TestVacuumReclaimsDeadBelowWatermark` builds two keys with different timelines: B is superseded before `tOld` begins (so b1 is dead from every live view and is reclaimed) while A is superseded after `tOld` begins (so a1 stays pinned and `tOld` still reads it). `TestVacuumPinnedByLongRunningTxn` is the bloat scenario made into an assertion: with `tOld` open, a four-version chain reclaims nothing; the moment `tOld` commits, the same chain collapses to its single live head. `TestVacuumConcurrentWithReaders` runs writers, vacuumers, and readers together under `-race` to prove the chain is never torn mid-walk.

Create `vacuum_test.go`:

```go
package mvcc

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestVacuumReclaimsDeadBelowWatermark(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	// Seed A=a1 and B=b1 in one committed transaction.
	seed := mgr.Begin()
	if err := store.Insert("A", []byte("a1"), seed, mgr); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert("B", []byte("b1"), seed, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(seed); err != nil {
		t.Fatal(err)
	}

	// Supersede B before tOld begins, so b1's deletion sits below tOld's snapshot.
	upB := mgr.Begin()
	if err := store.Update("B", []byte("b2"), upB, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(upB); err != nil {
		t.Fatal(err)
	}

	// tOld captures its snapshot here: b1 is already dead from its view, but a1 is
	// not yet superseded, so tOld must keep seeing a1.
	tOld := mgr.Begin()

	// Supersede A after tOld begins, so a1 must stay alive for tOld.
	upA := mgr.Begin()
	if err := store.Update("A", []byte("a2"), upA, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(upA); err != nil {
		t.Fatal(err)
	}

	stats := store.Vacuum(mgr)
	if stats.Reclaimed != 1 {
		t.Fatalf("Reclaimed = %d, want 1 (only b1 is below the watermark)", stats.Reclaimed)
	}

	cases := []struct {
		name      string
		key       string
		wantData  string
		wantCount int
	}{
		{"A retained for live snapshot", "A", "a1", 2},
		{"B old version reclaimed", "B", "b2", 1},
	}
	for _, tc := range cases {
		if got := store.VersionCount(tc.key); got != tc.wantCount {
			t.Errorf("%s: VersionCount(%q) = %d, want %d", tc.name, tc.key, got, tc.wantCount)
		}
		tup, err := store.Read(tc.key, tOld, mgr)
		if err != nil {
			t.Fatalf("%s: tOld read %q: %v", tc.name, tc.key, err)
		}
		if string(tup.Data) != tc.wantData {
			t.Errorf("%s: tOld read %q = %q, want %q", tc.name, tc.key, tup.Data, tc.wantData)
		}
	}
	_ = mgr.Commit(tOld)
}

// TestVacuumPinnedByLongRunningTxn proves a long-running transaction holds the low
// watermark down: while it stays open no superseded version it might still need is
// reclaimed, and only once it commits does the tail become collectable.
func TestVacuumPinnedByLongRunningTxn(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	seed := mgr.Begin()
	if err := store.Insert("k", []byte("v0"), seed, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(seed); err != nil {
		t.Fatal(err)
	}

	// tOld pins the watermark at its snapshot for the rest of the test.
	tOld := mgr.Begin()

	// Several committed updates pile new versions onto the chain while tOld is open.
	for i := 1; i <= 3; i++ {
		tx := mgr.Begin()
		if err := store.Update("k", []byte(strconv.Itoa(i)), tx, mgr); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(tx); err != nil {
			t.Fatal(err)
		}
	}

	before := store.VersionCount("k")
	if before != 4 {
		t.Fatalf("VersionCount before vacuum = %d, want 4", before)
	}

	// Every superseded version was deleted after tOld's snapshot, so none is reclaimable.
	pinned := store.Vacuum(mgr)
	if pinned.Reclaimed != 0 {
		t.Fatalf("Reclaimed while tOld open = %d, want 0 (watermark pins the tail)", pinned.Reclaimed)
	}
	if got := store.VersionCount("k"); got != before {
		t.Fatalf("VersionCount after pinned vacuum = %d, want %d", got, before)
	}

	// Once tOld commits, the watermark advances and the dead tail is collectable.
	if err := mgr.Commit(tOld); err != nil {
		t.Fatal(err)
	}
	freed := store.Vacuum(mgr)
	if freed.Reclaimed != before-1 {
		t.Fatalf("Reclaimed after tOld commit = %d, want %d", freed.Reclaimed, before-1)
	}
	if got := store.VersionCount("k"); got != 1 {
		t.Fatalf("VersionCount after final vacuum = %d, want 1", got)
	}
}

func TestVacuumConcurrentWithReaders(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	for i := 0; i < 8; i++ {
		tx := mgr.Begin()
		if err := store.Insert(fmt.Sprintf("k%d", i), []byte("v0"), tx, mgr); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(tx); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	// Writers churn versions so chains grow and become eligible for reclamation.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 50; r++ {
				tx := mgr.Begin()
				key := fmt.Sprintf("k%d", r%8)
				if err := store.Update(key, []byte("vN"), tx, mgr); err != nil {
					_ = mgr.Abort(tx, store)
					continue
				}
				_ = mgr.Commit(tx)
			}
		}()
	}
	// A vacuumer reclaims concurrently and must never tear a chain.
	for v := 0; v < 2; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 50; r++ {
				store.Vacuum(mgr)
			}
		}()
	}
	// Readers run concurrently and must never observe a torn chain.
	for rdr := 0; rdr < 4; rdr++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 50; r++ {
				tx := mgr.Begin()
				_, _ = store.Read(fmt.Sprintf("k%d", r%8), tx, mgr)
				_ = mgr.Commit(tx)
			}
		}()
	}
	wg.Wait()
}
```

## Review

Vacuum is correct when reclamation tracks the watermark exactly. A version whose deletion committed at or below the oldest active snapshot must be reclaimed; a version superseded after a live transaction's snapshot must be retained and must still read correctly through that transaction. The long-running-transaction test is the load-bearing one: it proves the watermark, not elapsed time or chain length, governs reclamation — nothing frees while `tOld` is open, everything dead frees the instant it commits. The concurrent test must stay clean under `-race`, proving that locking each chain for the count-prune-count step keeps vacuum atomic against readers and writers.

The mistake to avoid is reclaiming by age or chain length instead of by the watermark — a tempting "keep the last N versions" heuristic silently breaks a long-running reader that needs version N+1. The watermark is the only safe horizon, and a single idle-in-transaction session legitimately pins the whole tail.

## Resources

- [PostgreSQL: Routine Vacuuming](https://www.postgresql.org/docs/current/routine-vacuuming.html) — VACUUM, the dead-tuple horizon, and why long transactions block reclamation.
- [PostgreSQL: Introduction to MVCC](https://www.postgresql.org/docs/current/mvcc-intro.html) — how `xmin`/`xmax` and the transaction horizon decide tuple visibility.
- [CMU 15-445/645 lecture 19: Multi-Version Concurrency Control](https://15445.courses.cs.cmu.edu/fall2024/notes/19-multiversioning.pdf) — the garbage-collection section on tuple- and transaction-level reclamation.

---

Back to [01-version-store-and-snapshot-isolation.md](01-version-store-and-snapshot-isolation.md) | Next: [03-write-skew-and-for-update.md](03-write-skew-and-for-update.md)
