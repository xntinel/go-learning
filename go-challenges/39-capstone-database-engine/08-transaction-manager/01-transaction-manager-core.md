# Exercise 1: The Transaction Manager Core

This exercise builds the heart of the lesson: a working transaction manager that enforces ACID over an in-memory store, with strict (rigorous) two-phase locking, depth-first deadlock detection, savepoints for partial rollback, and ARIES-style crash recovery. Every other exercise in this lesson isolates one alternative mechanism — timestamp-based prevention, the update lock, hierarchical intention locks — and tests it on its own. This one is the integrated engine they orbit.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
txmgr.go              types, sentinel errors, WAL, Store, lockEntry
manager.go            Manager: Begin/Read/Write/Commit/Abort, locks,
                      deadlock detection, savepoints, ARIES Recover
cmd/
  demo/
    main.go           transfer under 2PL, then crash and recover on a shared WAL
txmgr_test.go         lifecycle, locking, deadlock, recovery, savepoint tests (-race)
```

- Files: `txmgr.go`, `manager.go`, `cmd/demo/main.go`, `txmgr_test.go`.
- Implement: the `WAL`, `Store`, and `Transaction` types; the `Manager` with `Begin`, `Read`, `Write`, `Commit`, `Abort`, `LockShared`, `LockExclusive`, `UpgradeLock`, `Savepoint`, `RollbackToSavepoint`, and `Recover`; and the background deadlock detector.
- Test: same-package tests cover commit durability, abort rollback, shared/exclusive lock behavior, lock upgrade, a crossed deadlock resolved by the detector, redo/undo recovery and its idempotence, savepoint partial rollback, and a concurrent stress test.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

### How the pieces interact

The manager is one struct because every subsystem touches the same state. A `Write` takes an exclusive lock from the lock table, appends a before/after record to the WAL, and mutates the store — three subsystems in one call. A `Commit` writes a COMMIT record, flushes, and only then releases the locks. An `Abort` walks the WAL undo chain (touching the store and writing CLRs) before it releases locks. The deadlock detector reads a wait-for graph that `LockShared` and `LockExclusive` populate as they block, and it resolves a cycle by calling `Abort` on a victim and broadcasting every lock's condition variable. Recovery reads only the WAL and rebuilds the store. Because the interactions are this dense, the type definitions live in one file and all the manager methods in another, but they are one package.

The integer ordering of the lock modes is load-bearing: `LockShared` is 0 and `LockExclusive` is 1, so a single `mode > existing` comparison promotes a held shared lock to exclusive without a branch. The WAL is in memory here — `Flush` is a no-op — because the durable-file version is the subject of the write-ahead-log lesson; what matters for the transaction manager is the *ordering* of `Flush` relative to lock release, not the syscall behind it.

Create `txmgr.go`:

```go
// Package txmgr implements a transaction manager that coordinates ACID
// transactions using a write-ahead log, strict two-phase locking, and
// ARIES-style crash recovery.
package txmgr

import (
	"errors"
	"sync"
)

// Sentinel errors used throughout the package.
var (
	ErrDeadlock           = errors.New("deadlock detected")
	ErrTxAborted          = errors.New("transaction is aborted")
	ErrTxCommitted        = errors.New("transaction is committed")
	ErrLockUpgradeFail    = errors.New("lock upgrade failed: other readers hold the lock")
	ErrSavepointNotFound  = errors.New("savepoint not found")
	ErrDuplicateSavepoint = errors.New("duplicate savepoint name")
)

// TxID is a monotonically increasing transaction identifier.
type TxID uint64

// LSN (log sequence number) identifies a position in the WAL.
type LSN uint64

// LockKey identifies a row by table name and row identifier.
type LockKey struct {
	Table string
	Row   string
}

// LockMode is either shared (read) or exclusive (write).
// The integer ordering (Shared < Exclusive) lets addLock promote a lock with >.
type LockMode int

const (
	LockShared    LockMode = iota // multiple holders allowed simultaneously
	LockExclusive                 // single holder; no concurrent readers or writers
)

// TxStatus is the lifecycle state of a transaction.
type TxStatus int

const (
	TxActive TxStatus = iota
	TxCommitted
	TxAborted
)

// WALRecordType classifies a WAL record.
type WALRecordType int

const (
	WALBegin WALRecordType = iota
	WALWrite               // data modification; carries before/after image
	WALCommit
	WALAbort
	WALCompensate // compensation log record (CLR) written during undo
)

// WALRecord is one entry in the write-ahead log.
type WALRecord struct {
	LSN         LSN
	TxID        TxID
	Type        WALRecordType
	Key         LockKey
	BeforeImage []byte // before-image for undo
	AfterImage  []byte // after-image for redo
	UndoNextLSN LSN    // CLR: skip back past the already-undone record
	PrevLSN     LSN    // previous LSN for this transaction (undo chain)
}

// Savepoint records a named position inside an active transaction.
type Savepoint struct {
	Name    string
	WalLSN  LSN
	LockSet map[LockKey]LockMode // value-copy of held locks at savepoint creation
}

// Transaction represents an in-progress, committed, or aborted transaction.
type Transaction struct {
	ID         TxID
	Status     TxStatus
	mu         sync.Mutex
	LockSet    map[LockKey]LockMode
	WriteSet   map[LockKey]struct{}
	Savepoints []Savepoint
	LastLSN    LSN
	BeginLSN   LSN
}

func (tx *Transaction) addLock(key LockKey, mode LockMode) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if existing, ok := tx.LockSet[key]; !ok || mode > existing {
		tx.LockSet[key] = mode
	}
}

// lockEntry is the per-key slot in the lock table.
type lockEntry struct {
	mu      sync.Mutex
	cond    *sync.Cond
	holders map[TxID]LockMode
}

func newLockEntry() *lockEntry {
	le := &lockEntry{holders: make(map[TxID]LockMode)}
	le.cond = sync.NewCond(&le.mu)
	return le
}

// WAL is an in-memory write-ahead log.
// In a production engine this is a durable file with fdatasync on each flush.
type WAL struct {
	mu      sync.Mutex
	records []WALRecord
	nextLSN LSN
}

// Append adds a record to the WAL and returns the assigned LSN.
func (w *WAL) Append(r WALRecord) LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextLSN++
	r.LSN = w.nextLSN
	w.records = append(w.records, r)
	return r.LSN
}

// Records returns a copy of all WAL records (safe for concurrent use).
func (w *WAL) Records() []WALRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]WALRecord, len(w.records))
	copy(out, w.records)
	return out
}

// Flush durably persists the WAL.
// In production: fdatasync. Here the WAL is in memory; Flush is a no-op.
func (w *WAL) Flush() error { return nil }

// Store is an in-memory key-value store that simulates the buffer pool.
type Store struct {
	mu   sync.RWMutex
	data map[LockKey][]byte
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{data: make(map[LockKey][]byte)}
}

// Get retrieves a value from the store.
func (s *Store) Get(key LockKey) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// Set writes a value to the store.
func (s *Store) Set(key LockKey, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Delete removes a key from the store.
func (s *Store) Delete(key LockKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}
```

`newLockEntry` initializes the `sync.Cond` pointing at the entry's own mutex, the required pattern from the `sync` documentation. `applyImage`, used by both abort and recovery, treats a nil image as a delete, so undoing the first write to a previously-absent key removes it rather than leaving an empty value.

### The Manager: lifecycle, locks, deadlock, savepoints, recovery

The single biggest correctness rule lives in `Commit`: write COMMIT, `Flush`, then release locks, then drop the transaction. `Abort` mirrors it: undo the chain, write ABORT, flush, release. The lock methods wrap every `cond.Wait()` in a `for` loop and recheck both the lock condition and the transaction's own status, so a deadlock victim that is aborted while parked wakes up, sees `TxAborted`, and returns `ErrDeadlock` instead of silently taking a lock it no longer deserves. The detector runs DFS over a snapshot of the wait-for graph and picks the highest transaction id in any cycle as the victim — the youngest, which has done the least work.

Create `manager.go`:

```go
package txmgr

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Manager is the transaction manager.
// It coordinates transaction lifecycle, lock management, WAL writes, and recovery.
type Manager struct {
	nextTxID atomic.Uint64

	txMu sync.RWMutex
	txns map[TxID]*Transaction

	lockMu sync.Mutex
	locks  map[LockKey]*lockEntry

	// waitFor[T1] = set of TxIDs that T1 is currently waiting for.
	waitForMu sync.Mutex
	waitFor   map[TxID]map[TxID]struct{}

	wal   *WAL
	store *Store

	deadlockInterval time.Duration
	stopDeadlock     chan struct{}
}

// NewManager creates a Manager backed by the given WAL and Store and starts
// the background deadlock-detection goroutine.
func NewManager(wal *WAL, store *Store) *Manager {
	m := &Manager{
		txns:             make(map[TxID]*Transaction),
		locks:            make(map[LockKey]*lockEntry),
		waitFor:          make(map[TxID]map[TxID]struct{}),
		wal:              wal,
		store:            store,
		deadlockInterval: 100 * time.Millisecond,
		stopDeadlock:     make(chan struct{}),
	}
	go m.deadlockDetectionLoop()
	return m
}

// Close stops the background deadlock-detection goroutine.
func (m *Manager) Close() {
	close(m.stopDeadlock)
}

// Begin starts a new transaction, writes a WAL BEGIN record, and returns the
// transaction descriptor.
func (m *Manager) Begin() (*Transaction, error) {
	id := TxID(m.nextTxID.Add(1))
	tx := &Transaction{
		ID:       id,
		Status:   TxActive,
		LockSet:  make(map[LockKey]LockMode),
		WriteSet: make(map[LockKey]struct{}),
	}
	lsn := m.wal.Append(WALRecord{TxID: id, Type: WALBegin})
	tx.BeginLSN = lsn
	tx.LastLSN = lsn

	m.txMu.Lock()
	m.txns[id] = tx
	m.txMu.Unlock()
	return tx, nil
}

// Read reads key under a shared lock.
func (m *Manager) Read(tx *Transaction, key LockKey) ([]byte, error) {
	if err := m.checkActive(tx); err != nil {
		return nil, err
	}
	if err := m.LockShared(tx, key); err != nil {
		return nil, err
	}
	v, _ := m.store.Get(key)
	return v, nil
}

// Write applies value under an exclusive lock and appends a WAL record with the
// before-image so that Abort can undo the change.
func (m *Manager) Write(tx *Transaction, key LockKey, value []byte) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	if err := m.LockExclusive(tx, key); err != nil {
		return err
	}
	before, _ := m.store.Get(key)
	lsn := m.wal.Append(WALRecord{
		TxID:        tx.ID,
		Type:        WALWrite,
		Key:         key,
		BeforeImage: before,
		AfterImage:  value,
		PrevLSN:     tx.LastLSN,
	})
	tx.mu.Lock()
	tx.LastLSN = lsn
	tx.WriteSet[key] = struct{}{}
	tx.mu.Unlock()
	m.store.Set(key, value)
	return nil
}

// Commit writes a WAL COMMIT record, flushes the WAL to disk (durability),
// releases all locks (strict 2PL shrinking phase), and removes the transaction
// from the active set. The flush must precede lock release.
func (m *Manager) Commit(tx *Transaction) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	tx.mu.Lock()
	tx.Status = TxCommitted
	lastLSN := tx.LastLSN
	tx.mu.Unlock()

	m.wal.Append(WALRecord{TxID: tx.ID, Type: WALCommit, PrevLSN: lastLSN})
	if err := m.wal.Flush(); err != nil {
		return fmt.Errorf("txmgr: commit flush: %w", err)
	}
	m.releaseAllLocks(tx)
	m.txMu.Lock()
	delete(m.txns, tx.ID)
	m.txMu.Unlock()
	return nil
}

// Abort undoes all writes by replaying WAL records in reverse, writes a WAL
// ABORT record, and releases all locks.
func (m *Manager) Abort(tx *Transaction) error {
	tx.mu.Lock()
	if tx.Status == TxAborted {
		tx.mu.Unlock()
		return nil
	}
	tx.Status = TxAborted
	lastLSN := tx.LastLSN
	tx.mu.Unlock()

	m.undoChain(tx.ID, lastLSN)
	m.wal.Append(WALRecord{TxID: tx.ID, Type: WALAbort, PrevLSN: lastLSN})
	if err := m.wal.Flush(); err != nil {
		return fmt.Errorf("txmgr: abort flush: %w", err)
	}
	m.releaseAllLocks(tx)
	m.txMu.Lock()
	delete(m.txns, tx.ID)
	m.txMu.Unlock()
	return nil
}

// undoChain walks the PrevLSN chain backwards from startLSN, restoring
// before-images. A CLR is written for each undone WALWrite record.
func (m *Manager) undoChain(txID TxID, startLSN LSN) {
	if startLSN == 0 {
		return
	}
	records := m.wal.Records()
	byLSN := make(map[LSN]WALRecord, len(records))
	for _, r := range records {
		byLSN[r.LSN] = r
	}
	lsn := startLSN
	for lsn != 0 {
		r, ok := byLSN[lsn]
		if !ok {
			break
		}
		switch r.Type {
		case WALWrite:
			applyImage(m.store, r.Key, r.BeforeImage)
			// CLR points past this record; recovery skips it if we crash during undo.
			m.wal.Append(WALRecord{
				TxID:        txID,
				Type:        WALCompensate,
				Key:         r.Key,
				AfterImage:  r.BeforeImage,
				UndoNextLSN: r.PrevLSN,
			})
			lsn = r.PrevLSN
		case WALCompensate:
			lsn = r.UndoNextLSN // skip over already-compensated record
		default:
			lsn = r.PrevLSN
		}
	}
}

// LockShared acquires a shared lock on key for tx.
// Blocks while any other transaction holds an exclusive lock.
func (m *Manager) LockShared(tx *Transaction, key LockKey) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	tx.mu.Lock()
	if existing, ok := tx.LockSet[key]; ok && existing >= LockShared {
		tx.mu.Unlock()
		return nil // already hold S or X
	}
	tx.mu.Unlock()

	le := m.getOrCreateLockEntry(key)
	le.mu.Lock()
	// for loop, not if: cond.Wait can return spuriously.
	for m.hasExclusiveOther(le, tx.ID) {
		m.addWaitFor(tx.ID, le)
		le.cond.Wait()
		m.removeWaitFor(tx.ID)
		if tx.Status == TxAborted {
			le.mu.Unlock()
			return ErrDeadlock
		}
	}
	le.holders[tx.ID] = LockShared
	le.mu.Unlock()

	tx.addLock(key, LockShared)
	return nil
}

// LockExclusive acquires an exclusive lock on key for tx.
// Blocks while any other transaction holds any lock on the key.
func (m *Manager) LockExclusive(tx *Transaction, key LockKey) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	tx.mu.Lock()
	if existing, ok := tx.LockSet[key]; ok && existing == LockExclusive {
		tx.mu.Unlock()
		return nil // already hold X
	}
	tx.mu.Unlock()

	le := m.getOrCreateLockEntry(key)
	le.mu.Lock()
	for !m.canGrantExclusive(le, tx.ID) {
		m.addWaitFor(tx.ID, le)
		le.cond.Wait()
		m.removeWaitFor(tx.ID)
		if tx.Status == TxAborted {
			le.mu.Unlock()
			return ErrDeadlock
		}
	}
	le.holders[tx.ID] = LockExclusive
	le.mu.Unlock()

	tx.addLock(key, LockExclusive)
	return nil
}

// UpgradeLock upgrades a shared lock to exclusive.
// Fails with ErrLockUpgradeFail if any other transaction holds a shared lock.
func (m *Manager) UpgradeLock(tx *Transaction, key LockKey) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	le := m.getOrCreateLockEntry(key)
	le.mu.Lock()
	defer le.mu.Unlock()
	if len(le.holders) != 1 {
		return ErrLockUpgradeFail
	}
	if _, ok := le.holders[tx.ID]; !ok {
		return ErrLockUpgradeFail
	}
	le.holders[tx.ID] = LockExclusive
	tx.addLock(key, LockExclusive)
	return nil
}

func (m *Manager) hasExclusiveOther(le *lockEntry, self TxID) bool {
	for id, mode := range le.holders {
		if id != self && mode == LockExclusive {
			return true
		}
	}
	return false
}

func (m *Manager) canGrantExclusive(le *lockEntry, self TxID) bool {
	for id := range le.holders {
		if id != self {
			return false
		}
	}
	return true
}

func (m *Manager) getOrCreateLockEntry(key LockKey) *lockEntry {
	m.lockMu.Lock()
	defer m.lockMu.Unlock()
	le, ok := m.locks[key]
	if !ok {
		le = newLockEntry()
		m.locks[key] = le
	}
	return le
}

func (m *Manager) releaseAllLocks(tx *Transaction) {
	tx.mu.Lock()
	keys := make([]LockKey, 0, len(tx.LockSet))
	for k := range tx.LockSet {
		keys = append(keys, k)
	}
	tx.mu.Unlock()
	for _, key := range keys {
		m.releaseLock(tx.ID, key)
	}
}

func (m *Manager) releaseLock(txID TxID, key LockKey) {
	m.lockMu.Lock()
	le, ok := m.locks[key]
	m.lockMu.Unlock()
	if !ok {
		return
	}
	le.mu.Lock()
	delete(le.holders, txID)
	le.cond.Broadcast()
	le.mu.Unlock()
}

// addWaitFor records that txID is waiting for every current holder of le.
func (m *Manager) addWaitFor(txID TxID, le *lockEntry) {
	m.waitForMu.Lock()
	defer m.waitForMu.Unlock()
	if m.waitFor[txID] == nil {
		m.waitFor[txID] = make(map[TxID]struct{})
	}
	for holder := range le.holders {
		if holder != txID {
			m.waitFor[txID][holder] = struct{}{}
		}
	}
}

func (m *Manager) removeWaitFor(txID TxID) {
	m.waitForMu.Lock()
	defer m.waitForMu.Unlock()
	delete(m.waitFor, txID)
}

// deadlockDetectionLoop runs cycle detection on the wait-for graph every
// deadlockInterval and aborts the youngest transaction in any cycle found.
func (m *Manager) deadlockDetectionLoop() {
	ticker := time.NewTicker(m.deadlockInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.detectAndBreakDeadlocks()
		case <-m.stopDeadlock:
			return
		}
	}
}

// detectAndBreakDeadlocks finds one cycle in the wait-for graph using DFS and
// aborts the youngest (highest TxID) transaction in the cycle.
func (m *Manager) detectAndBreakDeadlocks() {
	m.waitForMu.Lock()
	graph := make(map[TxID][]TxID, len(m.waitFor))
	for from, toSet := range m.waitFor {
		for to := range toSet {
			graph[from] = append(graph[from], to)
		}
	}
	m.waitForMu.Unlock()

	visited := make(map[TxID]bool)
	inStack := make(map[TxID]bool)
	var cycle []TxID

	var dfs func(id TxID, path []TxID) bool
	dfs = func(id TxID, path []TxID) bool {
		visited[id] = true
		inStack[id] = true
		path = append(path, id)
		for _, next := range graph[id] {
			if !visited[next] {
				if dfs(next, path) {
					return true
				}
			} else if inStack[next] {
				cycle = make([]TxID, len(path))
				copy(cycle, path)
				return true
			}
		}
		inStack[id] = false
		return false
	}

	for id := range graph {
		if !visited[id] {
			if dfs(id, nil) {
				break
			}
		}
	}

	if len(cycle) == 0 {
		return
	}

	// Victim: youngest transaction (highest TxID) in the cycle.
	var victim TxID
	for _, id := range cycle {
		if id > victim {
			victim = id
		}
	}
	m.txMu.RLock()
	tx, ok := m.txns[victim]
	m.txMu.RUnlock()
	if !ok {
		return
	}
	// Abort the victim, then broadcast on all condition variables so waiting
	// goroutines re-check tx.Status and return ErrDeadlock.
	m.Abort(tx)
	m.lockMu.Lock()
	for _, le := range m.locks {
		le.mu.Lock()
		le.cond.Broadcast()
		le.mu.Unlock()
	}
	m.lockMu.Unlock()
}

// Savepoint records the current WAL position under name.
// Names must be unique within a transaction.
func (m *Manager) Savepoint(tx *Transaction, name string) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	for _, sp := range tx.Savepoints {
		if sp.Name == name {
			return fmt.Errorf("%w: %q", ErrDuplicateSavepoint, name)
		}
	}
	// Copy the lock set: a pointer alias would prevent partial-release detection.
	locks := make(map[LockKey]LockMode, len(tx.LockSet))
	for k, v := range tx.LockSet {
		locks[k] = v
	}
	tx.Savepoints = append(tx.Savepoints, Savepoint{
		Name:    name,
		WalLSN:  tx.LastLSN,
		LockSet: locks,
	})
	return nil
}

// RollbackToSavepoint undoes changes made after the named savepoint, releases
// locks acquired after the savepoint, and allows the transaction to continue.
func (m *Manager) RollbackToSavepoint(tx *Transaction, name string) error {
	if err := m.checkActive(tx); err != nil {
		return err
	}
	tx.mu.Lock()
	spIdx := -1
	for i, sp := range tx.Savepoints {
		if sp.Name == name {
			spIdx = i
			break
		}
	}
	if spIdx == -1 {
		tx.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrSavepointNotFound, name)
	}
	sp := tx.Savepoints[spIdx]
	tx.Savepoints = tx.Savepoints[:spIdx+1] // discard savepoints nested after this one
	savedLSN := sp.WalLSN
	currentLSN := tx.LastLSN
	tx.mu.Unlock()

	m.undoAfterLSN(tx.ID, savedLSN, currentLSN)

	// Release locks that were acquired after the savepoint.
	tx.mu.Lock()
	for key := range tx.LockSet {
		if _, heldBefore := sp.LockSet[key]; !heldBefore {
			delete(tx.LockSet, key)
			m.releaseLock(tx.ID, key)
		}
	}
	tx.LastLSN = savedLSN
	tx.mu.Unlock()
	return nil
}

// undoAfterLSN undoes WAL records for txID whose LSN is in (afterLSN, upToLSN].
func (m *Manager) undoAfterLSN(txID TxID, afterLSN, upToLSN LSN) {
	if upToLSN == 0 || upToLSN <= afterLSN {
		return
	}
	records := m.wal.Records()
	byLSN := make(map[LSN]WALRecord, len(records))
	for _, r := range records {
		byLSN[r.LSN] = r
	}
	lsn := upToLSN
	for lsn != 0 && lsn > afterLSN {
		r, ok := byLSN[lsn]
		if !ok {
			break
		}
		if r.TxID == txID && r.Type == WALWrite {
			applyImage(m.store, r.Key, r.BeforeImage)
			m.wal.Append(WALRecord{
				TxID:        txID,
				Type:        WALCompensate,
				Key:         r.Key,
				AfterImage:  r.BeforeImage,
				UndoNextLSN: r.PrevLSN,
			})
		}
		lsn = r.PrevLSN
	}
}

// Recover performs ARIES-style crash recovery from the WAL.
//
// Analysis: classify each transaction as committed, aborted, or active.
// Redo: replay all WAL records in order to reach the crash state.
// Undo: for each transaction that was active at crash time, restore before-images.
func (m *Manager) Recover() error {
	records := m.wal.Records()

	// --- Analysis ---
	committed := make(map[TxID]bool)
	aborted := make(map[TxID]bool)
	lastLSN := make(map[TxID]LSN)

	for _, r := range records {
		switch r.Type {
		case WALBegin:
			lastLSN[r.TxID] = r.LSN
		case WALWrite, WALCompensate:
			lastLSN[r.TxID] = r.LSN
		case WALCommit:
			committed[r.TxID] = true
			delete(lastLSN, r.TxID)
		case WALAbort:
			aborted[r.TxID] = true
			delete(lastLSN, r.TxID)
		}
	}

	// --- Redo ---
	// Replay every WAL record. CLRs carry the same AfterImage convention.
	for _, r := range records {
		if r.Type == WALWrite || r.Type == WALCompensate {
			applyImage(m.store, r.Key, r.AfterImage)
		}
	}

	// --- Undo ---
	// Rollback transactions that were active at crash time.
	byLSN := make(map[LSN]WALRecord, len(records))
	for _, r := range records {
		byLSN[r.LSN] = r
	}

	for txID, lsn := range lastLSN {
		if committed[txID] || aborted[txID] {
			continue
		}
		cur := lsn
		for cur != 0 {
			r, ok := byLSN[cur]
			if !ok {
				break
			}
			switch r.Type {
			case WALWrite:
				applyImage(m.store, r.Key, r.BeforeImage)
				m.wal.Append(WALRecord{
					TxID:        txID,
					Type:        WALCompensate,
					Key:         r.Key,
					AfterImage:  r.BeforeImage,
					UndoNextLSN: r.PrevLSN,
				})
				cur = r.PrevLSN
			case WALCompensate:
				cur = r.UndoNextLSN
			default:
				cur = r.PrevLSN
			}
		}
		m.wal.Append(WALRecord{TxID: txID, Type: WALAbort})
	}
	return nil
}

// ActiveTxCount returns the number of in-progress transactions.
func (m *Manager) ActiveTxCount() int {
	m.txMu.RLock()
	defer m.txMu.RUnlock()
	return len(m.txns)
}

func (m *Manager) checkActive(tx *Transaction) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	switch tx.Status {
	case TxAborted:
		return fmt.Errorf("txmgr: tx %d: %w", tx.ID, ErrTxAborted)
	case TxCommitted:
		return fmt.Errorf("txmgr: tx %d: %w", tx.ID, ErrTxCommitted)
	}
	return nil
}

// applyImage writes value to key, or deletes the key when value is nil.
func applyImage(store *Store, key LockKey, value []byte) {
	if value == nil {
		store.Delete(key)
	} else {
		store.Set(key, value)
	}
}
```

The `for` loops around `le.cond.Wait()` are not optional. A spurious wakeup that bypassed the guard with a single `if` would let a goroutine install a lock while an incompatible holder still owns the key. The status recheck right after the wait is what lets the deadlock detector preempt a parked transaction: `detectAndBreakDeadlocks` aborts the victim (flipping its status to `TxAborted`) and broadcasts every condition variable, so the victim's loop wakes, sees the new status, and returns `ErrDeadlock`.

### The runnable demo

The demo touches only the exported API. It seeds two account balances, transfers 200 from A to B as a read-modify-write under strict 2PL (so the invariant `A + B = 1500` holds), then simulates a crash: it starts a third transaction, writes an uncommitted value, and recovers a fresh store from the shared WAL. Recovery redoes the committed transfer and undoes the in-flight write, restoring the invariant.

Create `cmd/demo/main.go`:

```go
// cmd/demo exercises the transaction manager: a transfer between two accounts
// using read-modify-write under strict two-phase locking, followed by crash
// recovery on a shared WAL.
package main

import (
	"fmt"
	"log"
	"strconv"

	"example.com/txmgr"
)

func main() {
	wal := &txmgr.WAL{}
	store := txmgr.NewStore()
	m := txmgr.NewManager(wal, store)
	defer m.Close()

	keyA := txmgr.LockKey{Table: "accounts", Row: "A"}
	keyB := txmgr.LockKey{Table: "accounts", Row: "B"}

	// Seed balances.
	setup, _ := m.Begin()
	if err := m.Write(setup, keyA, []byte("1000")); err != nil {
		log.Fatal(err)
	}
	if err := m.Write(setup, keyB, []byte("500")); err != nil {
		log.Fatal(err)
	}
	if err := m.Commit(setup); err != nil {
		log.Fatal(err)
	}

	// Transfer 200 from A to B under strict 2PL.
	tx, _ := m.Begin()
	rawA, _ := m.Read(tx, keyA)
	rawB, _ := m.Read(tx, keyB)
	balA, _ := strconv.Atoi(string(rawA))
	balB, _ := strconv.Atoi(string(rawB))
	balA -= 200
	balB += 200
	m.Write(tx, keyA, []byte(strconv.Itoa(balA)))
	m.Write(tx, keyB, []byte(strconv.Itoa(balB)))
	if err := m.Commit(tx); err != nil {
		log.Fatal(err)
	}

	vA, _ := store.Get(keyA)
	vB, _ := store.Get(keyB)
	fmt.Printf("A=%s  B=%s  sum=%d\n", vA, vB, mustAtoi(vA)+mustAtoi(vB))

	// Crash recovery demo.
	fmt.Println("--- simulating crash and recovery ---")
	txCrash, _ := m.Begin()
	m.Write(txCrash, keyA, []byte("9999"))
	// no commit: simulate crash

	fresh := txmgr.NewStore()
	fresh.Set(keyA, []byte("9999")) // post-crash disk state
	fresh.Set(keyB, vB)

	m2 := txmgr.NewManager(wal, fresh)
	defer m2.Close()
	if err := m2.Recover(); err != nil {
		log.Fatal(err)
	}

	recA, _ := fresh.Get(keyA)
	recB, _ := fresh.Get(keyB)
	fmt.Printf("after recovery: A=%s  B=%s  sum=%d\n",
		recA, recB, mustAtoi(recA)+mustAtoi(recB))
}

func mustAtoi(b []byte) int {
	n, _ := strconv.Atoi(string(b))
	return n
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
A=800  B=700  sum=1500
--- simulating crash and recovery ---
after recovery: A=800  B=700  sum=1500
```

### Tests

The tests are same-package (`package txmgr`, not `txmgr_test`) because the lock-table invariants — `m.store`, `m.locks`, `le.holders` — are not visible through the exported API and are exactly what must be asserted. `newFastManager` constructs a manager with a 20 ms detection interval so the crossed-deadlock test resolves quickly; the recovery tests construct a second manager over the same WAL and a fresh store to model a restart. `TestStressSerializability` runs ten concurrent commit-on-one-key transactions and asserts the final value is exactly one that was actually written, never a torn intermediate.

Create `txmgr_test.go`:

```go
package txmgr

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestManager returns a Manager and a cleanup function.
func newTestManager() (*Manager, func()) {
	wal := &WAL{}
	store := NewStore()
	m := NewManager(wal, store)
	return m, func() { m.Close() }
}

// newFastManager creates a Manager with a short deadlock-detection interval
// so that deadlock tests resolve quickly.
func newFastManager(wal *WAL, store *Store) *Manager {
	m := &Manager{
		txns:             make(map[TxID]*Transaction),
		locks:            make(map[LockKey]*lockEntry),
		waitFor:          make(map[TxID]map[TxID]struct{}),
		wal:              wal,
		store:            store,
		deadlockInterval: 20 * time.Millisecond,
		stopDeadlock:     make(chan struct{}),
	}
	go m.deadlockDetectionLoop()
	return m
}

func TestBeginReturnsActiveTx(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	tx, err := m.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != TxActive {
		t.Fatalf("status = %v, want TxActive", tx.Status)
	}
	if m.ActiveTxCount() != 1 {
		t.Fatalf("active count = %d, want 1", m.ActiveTxCount())
	}
}

func TestCommitPersistsData(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "users", Row: "u1"}
	tx, _ := m.Begin()
	if err := m.Write(tx, key, []byte("alice")); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(tx); err != nil {
		t.Fatal(err)
	}

	got, ok := m.store.Get(key)
	if !ok || string(got) != "alice" {
		t.Fatalf("store = %q %v, want \"alice\" true", got, ok)
	}
	if m.ActiveTxCount() != 0 {
		t.Fatalf("active after commit = %d, want 0", m.ActiveTxCount())
	}
}

func TestAbortRollsBackWrites(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "orders", Row: "o1"}
	m.store.Set(key, []byte("original"))

	tx, _ := m.Begin()
	if err := m.Write(tx, key, []byte("modified")); err != nil {
		t.Fatal(err)
	}
	if err := m.Abort(tx); err != nil {
		t.Fatal(err)
	}

	got, _ := m.store.Get(key)
	if string(got) != "original" {
		t.Fatalf("store after abort = %q, want \"original\"", got)
	}
}

func TestDoubleCommitReturnsErrTxCommitted(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	tx, _ := m.Begin()
	if err := m.Commit(tx); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(tx); !errors.Is(err, ErrTxCommitted) {
		t.Fatalf("second Commit err = %v, want ErrTxCommitted", err)
	}
}

func TestConcurrentSharedLocksAllowed(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "t", Row: "r"}
	m.store.Set(key, []byte("val"))

	tx1, _ := m.Begin()
	tx2, _ := m.Begin()

	if _, err := m.Read(tx1, key); err != nil {
		t.Fatalf("tx1 Read: %v", err)
	}
	if _, err := m.Read(tx2, key); err != nil {
		t.Fatalf("tx2 Read: %v", err)
	}
	m.Commit(tx1)
	m.Commit(tx2)
}

func TestExclusiveLockBlocksThenUnblocks(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "t", Row: "r"}
	tx1, _ := m.Begin()
	tx2, _ := m.Begin()

	if err := m.LockExclusive(tx1, key); err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		readDone <- m.LockShared(tx2, key)
	}()

	time.Sleep(30 * time.Millisecond)
	select {
	case <-readDone:
		t.Fatal("tx2 should be blocked but returned immediately")
	default:
	}

	m.Commit(tx1) // releases the exclusive lock; tx2 should unblock
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("tx2 LockShared after tx1 commit: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tx2 did not unblock after tx1 committed")
	}
	m.Commit(tx2)
}

func TestLockUpgradeSucceeds(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "t", Row: "r"}
	tx, _ := m.Begin()
	if err := m.LockShared(tx, key); err != nil {
		t.Fatal(err)
	}
	if err := m.UpgradeLock(tx, key); err != nil {
		t.Fatalf("UpgradeLock: %v", err)
	}
	le := m.locks[key]
	le.mu.Lock()
	mode := le.holders[tx.ID]
	le.mu.Unlock()
	if mode != LockExclusive {
		t.Fatalf("mode after upgrade = %v, want LockExclusive", mode)
	}
	m.Commit(tx)
}

func TestLockUpgradeFailsWithOtherReader(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "t", Row: "r"}
	tx1, _ := m.Begin()
	tx2, _ := m.Begin()
	m.LockShared(tx1, key)
	m.LockShared(tx2, key)

	if err := m.UpgradeLock(tx1, key); !errors.Is(err, ErrLockUpgradeFail) {
		t.Fatalf("err = %v, want ErrLockUpgradeFail", err)
	}
	m.Commit(tx1)
	m.Commit(tx2)
}

func TestDeadlockDetectedAndVictimAborted(t *testing.T) {
	// tx1 holds key A and waits for key B.
	// tx2 holds key B and waits for key A.
	// The background detector must abort exactly one of them within 500ms.
	wal := &WAL{}
	store := NewStore()
	m := newFastManager(wal, store)
	defer m.Close()

	keyA := LockKey{Table: "t", Row: "A"}
	keyB := LockKey{Table: "t", Row: "B"}

	tx1, _ := m.Begin()
	tx2, _ := m.Begin()
	m.LockExclusive(tx1, keyA)
	m.LockExclusive(tx2, keyB)

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		err1 = m.LockExclusive(tx1, keyB)
	}()
	go func() {
		defer wg.Done()
		err2 = m.LockExclusive(tx2, keyA)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("deadlock was not resolved within 500ms")
	}

	if err1 == nil && err2 == nil {
		t.Fatal("expected ErrDeadlock for at least one transaction, got nil for both")
	}
	if err1 != nil && !errors.Is(err1, ErrDeadlock) {
		t.Fatalf("err1 = %v, want ErrDeadlock", err1)
	}
	if err2 != nil && !errors.Is(err2, ErrDeadlock) {
		t.Fatalf("err2 = %v, want ErrDeadlock", err2)
	}
}

func TestCrashRecoveryRedoesCommitted(t *testing.T) {
	t.Parallel()
	wal := &WAL{}
	store := NewStore()
	m := NewManager(wal, store)
	defer m.Close()

	key := LockKey{Table: "t", Row: "r"}
	tx, _ := m.Begin()
	m.Write(tx, key, []byte("committed"))
	m.Commit(tx)

	// Recover into a fresh store using the same WAL.
	fresh := NewStore()
	m2 := &Manager{
		txns:             make(map[TxID]*Transaction),
		locks:            make(map[LockKey]*lockEntry),
		waitFor:          make(map[TxID]map[TxID]struct{}),
		wal:              wal,
		store:            fresh,
		deadlockInterval: 100 * time.Millisecond,
		stopDeadlock:     make(chan struct{}),
	}
	go m2.deadlockDetectionLoop()
	defer m2.Close()

	if err := m2.Recover(); err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get(key)
	if !ok || string(got) != "committed" {
		t.Fatalf("after recovery: %q %v, want \"committed\" true", got, ok)
	}
}

func TestCrashRecoveryUndoesUncommitted(t *testing.T) {
	t.Parallel()
	wal := &WAL{}
	store := NewStore()
	m := NewManager(wal, store)
	defer m.Close()

	key := LockKey{Table: "t", Row: "r"}
	store.Set(key, []byte("original"))
	tx, _ := m.Begin()
	m.Write(tx, key, []byte("in-flight"))
	// No commit — simulate crash here.

	// Post-crash disk state reflects the in-flight write.
	// Redo re-applies it; the undo phase then restores the before-image.
	fresh := NewStore()
	fresh.Set(key, []byte("in-flight"))
	m2 := &Manager{
		txns:             make(map[TxID]*Transaction),
		locks:            make(map[LockKey]*lockEntry),
		waitFor:          make(map[TxID]map[TxID]struct{}),
		wal:              wal,
		store:            fresh,
		deadlockInterval: 100 * time.Millisecond,
		stopDeadlock:     make(chan struct{}),
	}
	go m2.deadlockDetectionLoop()
	defer m2.Close()

	m2.Recover()

	got, _ := fresh.Get(key)
	if string(got) != "original" {
		t.Fatalf("after recovery: %q, want \"original\"", got)
	}
}

func TestRecoveryIsIdempotent(t *testing.T) {
	t.Parallel()
	wal := &WAL{}
	store := NewStore()
	m := NewManager(wal, store)
	defer m.Close()

	key := LockKey{Table: "t", Row: "r"}
	tx, _ := m.Begin()
	m.Write(tx, key, []byte("value"))
	m.Commit(tx)

	newMgr := func(s *Store) *Manager {
		mg := &Manager{
			txns:             make(map[TxID]*Transaction),
			locks:            make(map[LockKey]*lockEntry),
			waitFor:          make(map[TxID]map[TxID]struct{}),
			wal:              wal,
			store:            s,
			deadlockInterval: 100 * time.Millisecond,
			stopDeadlock:     make(chan struct{}),
		}
		go mg.deadlockDetectionLoop()
		return mg
	}

	s1 := NewStore()
	m1 := newMgr(s1)
	defer m1.Close()
	m1.Recover()

	s2 := NewStore()
	m2 := newMgr(s2)
	defer m2.Close()
	m2.Recover()
	m2.Recover() // second recovery pass on the same store

	v1, _ := s1.Get(key)
	v2, _ := s2.Get(key)
	if string(v1) != string(v2) {
		t.Fatalf("idempotent: %q != %q", v1, v2)
	}
}

func TestSavepointPartialRollback(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	keyA := LockKey{Table: "t", Row: "A"}
	keyB := LockKey{Table: "t", Row: "B"}

	tx, _ := m.Begin()
	m.Write(tx, keyA, []byte("a1"))

	if err := m.Savepoint(tx, "sp1"); err != nil {
		t.Fatal(err)
	}

	m.Write(tx, keyB, []byte("b1"))

	if err := m.RollbackToSavepoint(tx, "sp1"); err != nil {
		t.Fatal(err)
	}

	// Write before savepoint must still be present.
	gotA, _ := m.store.Get(keyA)
	if string(gotA) != "a1" {
		t.Fatalf("keyA = %q, want \"a1\"", gotA)
	}
	// Write after savepoint must have been rolled back.
	gotB, _ := m.store.Get(keyB)
	if string(gotB) == "b1" {
		t.Fatal("keyB should have been rolled back to its pre-savepoint value")
	}

	m.Commit(tx)
}

func TestSavepointNotFoundReturnsError(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	tx, _ := m.Begin()
	if err := m.RollbackToSavepoint(tx, "nonexistent"); !errors.Is(err, ErrSavepointNotFound) {
		t.Fatalf("err = %v, want ErrSavepointNotFound", err)
	}
	m.Abort(tx)
}

func TestDuplicateSavepointNameReturnsError(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	tx, _ := m.Begin()
	if err := m.Savepoint(tx, "sp1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Savepoint(tx, "sp1"); !errors.Is(err, ErrDuplicateSavepoint) {
		t.Fatalf("err = %v, want ErrDuplicateSavepoint", err)
	}
	m.Abort(tx)
}

func TestStressSerializability(t *testing.T) {
	t.Parallel()
	m, cleanup := newTestManager()
	defer cleanup()

	key := LockKey{Table: "accounts", Row: "acc-1"}
	const n = 10
	written := make(map[string]bool, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			val := fmt.Sprintf("v%d", i)
			tx, _ := m.Begin()
			if err := m.Write(tx, key, []byte(val)); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			if err := m.Commit(tx); err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			mu.Lock()
			written[val] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	got, ok := m.store.Get(key)
	if !ok {
		t.Fatal("key missing after concurrent commits")
	}
	if !written[string(got)] {
		t.Fatalf("final value %q was never written by any transaction", got)
	}
}

func ExampleManager_Write() {
	wal := &WAL{}
	store := NewStore()
	m := NewManager(wal, store)
	defer m.Close()

	key := LockKey{Table: "accounts", Row: "acc-1"}
	tx, _ := m.Begin()
	_ = m.Write(tx, key, []byte("balance=1000"))
	_ = m.Commit(tx)

	val, _ := store.Get(key)
	fmt.Println(string(val))
	// Output: balance=1000
}
```

## Review

The manager is correct when the orderings hold and the locks are honest. Commit must write the COMMIT record, flush, and only then release locks; reversing the last two steps lets a second transaction commit on top of a not-yet-durable commit, which a crash then erases. Abort and recovery must both write CLRs as they undo, and the undo walk must skip CLRs via their undo-next pointer, or a crash mid-undo corrupts the store on the next run. Confirm that concurrent shared locks coexist, that an exclusive lock blocks readers until release, that the crossed deadlock resolves within the detector interval with exactly one victim, that recovery redoes committed and undoes uncommitted work and is idempotent across repeated passes, and that the whole suite stays clean under `go test -race`. The stress test is the integration check: ten concurrent commit-on-one-key transactions must leave a value that some transaction actually wrote, never a torn blend.

## Resources

- [ARIES: A Transaction Recovery Method Supporting Fine-Granularity Locking and Partial Rollbacks Using Write-Ahead Logging (Mohan et al., 1992)](https://cs.stanford.edu/people/chr101/aries.html) — the primary source; sections 2 through 5 cover analysis, redo, undo, and CLRs.
- [CMU 15-445/645 Lecture 16: Two-Phase Locking](https://15445.courses.cs.cmu.edu/fall2024/slides/16-twophaselocking.pdf) — lock compatibility, strict versus plain 2PL, and deadlock-detection strategy.
- [pkg.go.dev/sync — sync.Cond](https://pkg.go.dev/sync#Cond) — the spurious-wakeup contract for `Wait` and why a `for` loop is mandatory.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-deadlock-prevention.md](02-deadlock-prevention.md)
