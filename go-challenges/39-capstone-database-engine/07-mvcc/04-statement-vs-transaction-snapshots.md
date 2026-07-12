# Exercise 4: Statement-Level vs Transaction-Level Snapshots

The engine's default is Repeatable Read: one snapshot captured at Begin and reused for the transaction's whole life, so every read sees the same consistent state. Read Committed is the other common level — it captures a fresh snapshot at the start of every statement, so each statement sees the most recently committed data and a transaction can observe values that changed between its own statements. This exercise implements `StatementSnapshot` to model per-statement snapshot acquisition and proves the visibility difference: after a concurrent commit, only the Read Committed session observes the new value. The difference is purely one of *when* the snapshot is taken; the visibility predicate is identical in both cases.

This module is fully self-contained. It reproduces the version-store scaffolding so it stands alone, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
mvcc.go             TxID, Version, Transaction, TransactionManager (scaffolding)
store.go            VersionStore, Read/Insert/Update/Delete (scaffolding)
isolation.go        IsolationLevel, RepeatableRead, ReadCommitted, StatementSnapshot
cmd/
  demo/
    main.go         two sessions read v1, a writer commits v2, only read-committed sees it
isolation_test.go   level contrast (table-driven), watermark advance, concurrent read-committed
```

- Files: `mvcc.go`, `store.go`, `isolation.go`, `cmd/demo/main.go`, `isolation_test.go`.
- Implement: `IsolationLevel`, the `RepeatableRead` and `ReadCommitted` constants, and `(*TransactionManager).StatementSnapshot`, on top of the reproduced core engine.
- Test: `isolation_test.go` contrasts the two levels across a concurrent commit, proves a Read Committed refresh advances the low watermark it was pinning, and runs concurrent Read Committed sessions under `-race`.
- Verify: `go test -count=1 -race ./...`

### Snapshot acquisition is the only difference

The visibility predicate consults `tx.startedAt`. Repeatable Read sets that value once, at Begin, and never touches it again — so every read in the transaction asks the same question against the same snapshot and the answers are stable. Read Committed re-reads the commit sequence at the start of each statement and overwrites `tx.startedAt` with it, so the next read asks its question against a fresher snapshot that includes everything committed since the previous statement. Nothing else changes: the same `IsVisible` runs, the same chains are walked, only the `startedAt` it compares against moves.

`StatementSnapshot` is therefore a one-liner with a guard: under Read Committed it advances `tx.startedAt` to the current commit sequence; under Repeatable Read it is a no-op. The mutation takes the transaction's lock because a Read Committed session may have a concurrent reader, and the assignment to `startedAt` must not race a concurrent read of it. The crucial safety property is that snapshots only ever move *forward* — the commit sequence is monotone, so refreshing never lowers `startedAt`. That means a refresh can reveal newer commits but can never resurrect a version an earlier statement could already not see: an older deletion that was visible-as-deleted stays deleted, because its `Xmax` sequence is still at or below the new, higher `startedAt`.

This ties back to garbage collection. A session's `startedAt` is what `OldestActiveSnapshot` minimizes over, so a Read Committed session that refreshes its snapshot forward stops pinning the versions its old snapshot held down, and the low watermark can rise — which is exactly why long Read Committed transactions cause less bloat than long Repeatable Read ones holding the same duration.

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
	startedAt uint64 // commit-sequence counter captured at Begin() (or refreshed per statement)
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
		tx.mu.Lock()
		s := tx.startedAt
		tx.mu.Unlock()
		if s < min {
			min = s
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

Now the isolation-level control.

Create `isolation.go`:

```go
package mvcc

// IsolationLevel selects how a session acquires snapshots for its reads.
type IsolationLevel int

const (
	// RepeatableRead captures one snapshot at Begin and reuses it for the whole
	// transaction (this engine's default, snapshot isolation).
	RepeatableRead IsolationLevel = iota
	// ReadCommitted captures a fresh snapshot at the start of every statement, so
	// each statement sees the most recently committed data.
	ReadCommitted
)

// StatementSnapshot models per-statement snapshot acquisition. Call it at the start
// of each statement: under ReadCommitted it advances tx's snapshot to the current
// commit sequence, so the next read sees every transaction that has committed so
// far; under RepeatableRead it leaves the transaction-level snapshot untouched.
//
// Snapshots only ever move forward, so this never resurrects a version that an
// earlier statement could already not see.
func (m *TransactionManager) StatementSnapshot(tx *Transaction, lvl IsolationLevel) {
	if lvl != ReadCommitted {
		return
	}
	tx.mu.Lock()
	tx.startedAt = m.commitSeq.Load()
	tx.mu.Unlock()
}
```

### The runnable demo

The demo opens two sessions against the same row, refreshes each at its declared level, and has both read `v1`. A third transaction then commits `v2`. After each session refreshes its per-statement snapshot again, the Repeatable Read session still reads `v1` (its transaction-level snapshot is frozen) while the Read Committed session reads `v2` (its statement-level snapshot moved forward).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/mvcc-isolation"
)

func main() {
	mgr := mvcc.NewTransactionManager()
	store := mvcc.NewVersionStore()

	seed := mgr.Begin()
	if err := store.Insert("k", []byte("v1"), seed, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(seed); err != nil {
		log.Fatal(err)
	}

	// Two sessions begin and each reads v1 in its first statement.
	rr := mgr.Begin()
	rc := mgr.Begin()
	mgr.StatementSnapshot(rr, mvcc.RepeatableRead)
	mgr.StatementSnapshot(rc, mvcc.ReadCommitted)
	rr1, _ := store.Read("k", rr, mgr)
	rc1, _ := store.Read("k", rc, mgr)
	fmt.Printf("first statement: repeatable-read=%s read-committed=%s\n", rr1.Data, rc1.Data)

	// A concurrent transaction commits v2.
	w := mgr.Begin()
	if err := store.Update("k", []byte("v2"), w, mgr); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Commit(w); err != nil {
		log.Fatal(err)
	}

	// Second statement: read-committed refreshes its snapshot, repeatable-read does not.
	mgr.StatementSnapshot(rr, mvcc.RepeatableRead)
	mgr.StatementSnapshot(rc, mvcc.ReadCommitted)
	rr2, _ := store.Read("k", rr, mgr)
	rc2, _ := store.Read("k", rc, mgr)
	fmt.Printf("after concurrent commit: repeatable-read=%s read-committed=%s\n", rr2.Data, rc2.Data)
	_ = mgr.Commit(rr)
	_ = mgr.Commit(rc)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first statement: repeatable-read=v1 read-committed=v1
after concurrent commit: repeatable-read=v1 read-committed=v2
```

### Tests

`TestStatementVsTransactionSnapshot` is table-driven over the two levels: both read `v1` first, then after a concurrent commit of `v2` the Repeatable Read row still reads `v1` while the Read Committed row reads `v2`. `TestReadCommittedAdvancesWatermark` makes the GC consequence concrete — a Read Committed refresh moves the session's `startedAt` past a concurrent commit, so `OldestActiveSnapshot` rises and the version the old snapshot pinned becomes collectable. `TestReadCommittedConcurrent` runs independent sessions that refresh their snapshots per statement while a writer commits a stream of fresh keys, all under `-race`.

Create `isolation_test.go`:

```go
package mvcc

import (
	"fmt"
	"sync"
	"testing"
)

// TestStatementVsTransactionSnapshot contrasts Read Committed (per-statement
// snapshot) with Repeatable Read (transaction-level snapshot): after a concurrent
// commit, only the Read Committed session observes the new value.
func TestStatementVsTransactionSnapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		lvl       IsolationLevel
		wantFirst string
		wantAfter string
	}{
		{"repeatable read keeps txn snapshot", RepeatableRead, "v1", "v1"},
		{"read committed sees latest commit", ReadCommitted, "v1", "v2"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr := NewTransactionManager()
			store := NewVersionStore()

			seed := mgr.Begin()
			if err := store.Insert("k", []byte("v1"), seed, mgr); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Commit(seed); err != nil {
				t.Fatal(err)
			}

			// The session begins and reads v1 in its first statement.
			tx := mgr.Begin()
			mgr.StatementSnapshot(tx, tc.lvl)
			first, err := store.Read("k", tx, mgr)
			if err != nil {
				t.Fatal(err)
			}
			if string(first.Data) != tc.wantFirst {
				t.Fatalf("first read = %q, want %q", first.Data, tc.wantFirst)
			}

			// A concurrent transaction commits a new value.
			w := mgr.Begin()
			if err := store.Update("k", []byte("v2"), w, mgr); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Commit(w); err != nil {
				t.Fatal(err)
			}

			// Second statement: ReadCommitted refreshes the snapshot, RepeatableRead
			// does not.
			mgr.StatementSnapshot(tx, tc.lvl)
			after, err := store.Read("k", tx, mgr)
			if err != nil {
				t.Fatal(err)
			}
			if string(after.Data) != tc.wantAfter {
				t.Fatalf("second read = %q, want %q (level %v)", after.Data, tc.wantAfter, tc.lvl)
			}
			_ = mgr.Commit(tx)
		})
	}
}

// TestReadCommittedAdvancesWatermark proves a Read Committed statement refresh moves
// the session's snapshot forward, lifting the low watermark it had been pinning so a
// superseded version becomes collectable.
func TestReadCommittedAdvancesWatermark(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	seed := mgr.Begin()
	if err := store.Insert("k", []byte("v1"), seed, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(seed); err != nil {
		t.Fatal(err)
	}

	// A Read Committed session opens and pins the watermark at its first snapshot.
	tx := mgr.Begin()
	mgr.StatementSnapshot(tx, ReadCommitted)
	before := mgr.OldestActiveSnapshot()

	// Another transaction supersedes the row and commits, advancing the sequence.
	w := mgr.Begin()
	if err := store.Update("k", []byte("v2"), w, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(w); err != nil {
		t.Fatal(err)
	}

	// Refreshing the per-statement snapshot moves tx forward past that commit.
	mgr.StatementSnapshot(tx, ReadCommitted)
	after := mgr.OldestActiveSnapshot()
	if after <= before {
		t.Fatalf("watermark did not advance: before=%d after=%d", before, after)
	}
	_ = mgr.Commit(tx)
}

// TestReadCommittedConcurrent runs independent Read Committed sessions while a
// writer advances a key, exercising per-statement snapshot refresh under -race.
func TestReadCommittedConcurrent(t *testing.T) {
	t.Parallel()
	mgr := NewTransactionManager()
	store := NewVersionStore()

	seed := mgr.Begin()
	if err := store.Insert("k", []byte("0"), seed, mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(seed); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// One writer commits a stream of fresh keys, advancing the commit sequence so
	// each reader's per-statement snapshot keeps moving forward. It inserts new keys
	// rather than superseding "k" so the stable key is never deleted mid-commit.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			w := mgr.Begin()
			if err := store.Insert(fmt.Sprintf("ins%d", i), []byte("v"), w, mgr); err != nil {
				_ = mgr.Abort(w, store)
				continue
			}
			_ = mgr.Commit(w)
		}
	}()
	// Each reader owns a private session and refreshes its snapshot per statement.
	// "k" is committed and never modified, so a correct Read Committed read always
	// observes it regardless of how far the snapshot has advanced.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx := mgr.Begin()
			for s := 0; s < 50; s++ {
				mgr.StatementSnapshot(tx, ReadCommitted)
				tup, err := store.Read("k", tx, mgr)
				if err != nil {
					t.Errorf("read under read committed: %v", err)
					return
				}
				if string(tup.Data) != "0" {
					t.Errorf("read k = %q, want 0", tup.Data)
					return
				}
			}
			_ = mgr.Commit(tx)
		}()
	}
	wg.Wait()
}
```

## Review

The level control is correct when the same predicate produces stable reads under Repeatable Read and fresh reads under Read Committed, differing only in when the snapshot was taken. The table-driven test pins exactly that contrast across one concurrent commit. The watermark test proves the GC tie-in: a refreshed Read Committed snapshot stops pinning its old position, so `OldestActiveSnapshot` advances and reclamation can proceed — the practical reason a Read Committed session of the same wall-clock duration holds the horizon down less than a Repeatable Read one. The concurrent test must stay clean under `-race`, which is why `StatementSnapshot` takes the transaction lock to publish the new `startedAt` and `OldestActiveSnapshot` takes it to read each one.

The trap is assuming a snapshot refresh could move backward and un-hide an old deletion. It cannot: the commit sequence is monotone, so a refresh only ever reveals newer commits, never resurrects a version an earlier statement could already not see. Forgetting the lock on the `startedAt` mutation is the other trap — a concurrent reader of `startedAt` races the refresh and the detector flags it.

## Resources

- [PostgreSQL: Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) — the precise Read Committed vs Repeatable Read semantics this exercise models.
- [Berenson, Bernstein, Gray, Melton, O'Neil, O'Neil: A Critique of ANSI SQL Isolation Levels (1995)](https://www.microsoft.com/en-us/research/publication/a-critique-of-ansi-sql-isolation-levels/) — the anomaly-based definition of the isolation levels.
- [CMU 15-445/645 lecture 19: Multi-Version Concurrency Control](https://15445.courses.cs.cmu.edu/fall2024/notes/19-multiversioning.pdf) — how snapshot acquisition interacts with the visibility check and the transaction horizon.

---

Back to [03-write-skew-and-for-update.md](03-write-skew-and-for-update.md)
