# 20. Distributed Transaction Coordinator

A distributed transaction coordinator manages transactions that span multiple
independent participants. The hard parts are: atomicity across participants
(all commit or all abort), isolation between concurrent transactions (no dirty
reads, no lost updates), conflict detection when two transactions race for the
same resource, and recovery after a coordinator crash. This lesson builds an
in-process coordinator that combines two-phase commit for atomicity,
per-resource pessimistic locking for isolation, non-blocking write-conflict
detection, and a write-ahead log for crash recovery.

```text
txcoord/
  go.mod
  txcoord.go
  txcoord_test.go
  cmd/demo/main.go
```

## Concepts

### Two-Phase Commit and Why It Blocks

Two-phase commit (2PC) is the canonical protocol for distributed atomicity.
Phase 1 (prepare): the coordinator sends Prepare to every participant; each
participant writes its intent to a durable log and votes Yes or No. Phase 2
(commit/abort): if every vote is Yes the coordinator logs the commit decision
and broadcasts Commit; if any vote is No it broadcasts Abort.

The guarantee comes from the durability invariant: a participant that votes Yes
has already logged enough state to commit later, even after a crash. The cost
is the blocking problem: if the coordinator crashes after collecting Yes votes
but before sending the decision, every participant that voted Yes is stuck. It
cannot commit (it does not know whether all voted Yes) and cannot abort (the
coordinator might have committed). It must wait for the coordinator to recover
and re-examine the log.

This lesson's implementation uses a write-ahead log (WAL) that the coordinator
writes before sending any message. On recovery, the coordinator replays the log
and resends the outcome to participants whose decision was logged but never
acknowledged.

### Pessimistic Locking and Isolation

Each resource is owned by exactly one participant. Concurrent transactions
acquire locks before reading or writing. Two lock modes are used:

- Read lock (shared): multiple transactions may hold a read lock on the same
  resource simultaneously.
- Write lock (exclusive): only one transaction may hold a write lock; no read
  locks may coexist.

Locks are held until the transaction commits or aborts (two-phase locking,
2PL). 2PL with the growing and shrinking phases guarantees serializability: the
execution is equivalent to some serial ordering of the transactions.

### Write-Conflict Detection

This lock manager is non-blocking: `Acquire` either grants the lock
immediately or returns `ErrConflict` — it never queues the caller. If
transaction A requests a lock held by transaction B, `Acquire` returns
`ErrConflict` right away so the coordinator can abort the conflicting
transaction and retry (or surface the error to the caller).

This is simpler than a blocking acquire with deadlock detection, which would
require a wait-for graph (directed edges A -> B meaning "A is waiting for B")
and a cycle-detection pass on every blocked request. A production system would
implement that full scheme and abort the youngest transaction in a detected
cycle. The trade-off: the non-blocking design never deadlocks, but it aborts
transactions more eagerly than necessary — any conflict, not just a cycle,
triggers an abort.

### Write-Ahead Log and Recovery

Every state transition the coordinator performs is written to the log before
the transition takes effect. The log records:

- `BEGIN txid` - a new transaction started
- `PREPARE txid` - all participants acknowledged Prepare
- `COMMIT txid` - coordinator decided to commit
- `ABORT txid` - coordinator decided to abort

On recovery the coordinator reads the log forward. Transactions with a COMMIT
or ABORT entry are in a known final state; the coordinator re-broadcasts the
outcome if participants have not yet acknowledged it. Transactions with only a
BEGIN (never reached Prepare) are presumed aborted. Transactions with a PREPARE
but no COMMIT/ABORT are in-doubt; the implementation uses presumed-abort: the
coordinator aborts them.

### Deadlock vs Livelock vs Starvation

Deadlock: two or more transactions each hold a lock the other needs. Neither
can proceed without releasing, but neither releases without proceeding.
Detected by cycle in the wait-for graph.

Livelock: transactions abort and retry repeatedly, each interfering with the
other. No deadlock detection fires because no cycle exists. Mitigated by
backoff and retry-order randomization (not in scope for this lesson).

Starvation: a transaction is repeatedly bypassed by higher-priority
transactions. Mitigated by FIFO lock queues (not in scope for this lesson).

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/txcoord/cmd/demo
cd ~/go-exercises/txcoord
go mod init example.com/txcoord
```

This is a library. Verification is done with `go test`, not by running a
program.

### Exercise 1: Core Types and the Write-Ahead Log

Create `txcoord.go`:

```go
package txcoord

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// TxID is an opaque transaction identifier.
type TxID string

// LogKind records the phase transition that was durably written.
type LogKind int

const (
	LogBegin   LogKind = iota // coordinator started the transaction
	LogPrepare                // all participants voted Yes
	LogCommit                 // coordinator decided to commit
	LogAbort                  // coordinator decided to abort
)

func (k LogKind) String() string {
	switch k {
	case LogBegin:
		return "BEGIN"
	case LogPrepare:
		return "PREPARE"
	case LogCommit:
		return "COMMIT"
	case LogAbort:
		return "ABORT"
	default:
		return "UNKNOWN"
	}
}

// LogEntry is one record in the write-ahead log.
type LogEntry struct {
	TxID TxID
	Kind LogKind
	At   time.Time
}

// WAL is the write-ahead log interface. Implementations must be safe for
// concurrent use.
type WAL interface {
	// Append writes a log entry durably before the corresponding action is taken.
	Append(e LogEntry) error
	// Entries returns all entries in append order.
	Entries() ([]LogEntry, error)
}

// Sentinel errors.
var (
	ErrAborted   = errors.New("txcoord: transaction aborted")
	ErrUnknownTx = errors.New("txcoord: unknown transaction")
	ErrConflict  = errors.New("txcoord: write conflict")
)

// TxState is the lifecycle phase of a transaction.
type TxState int

const (
	TxActive    TxState = iota // executing, holding locks
	TxPreparing                // voted; waiting for coordinator decision
	TxCommitted                // all participants committed
	TxAborted                  // rolled back
)

func (s TxState) String() string {
	switch s {
	case TxActive:
		return "active"
	case TxPreparing:
		return "preparing"
	case TxCommitted:
		return "committed"
	case TxAborted:
		return "aborted"
	default:
		return "unknown"
	}
}

// tx is the coordinator's internal record for one transaction.
type tx struct {
	id        TxID
	state     TxState
	startedAt time.Time
	writes    map[string]string // resource -> new value (staged)
	reads     map[string]struct{}
}
```

### Exercise 2: Memory WAL and the Lock Manager

Append to `txcoord.go`:

```go
// MemWAL is a thread-safe in-memory write-ahead log for testing.
type MemWAL struct {
	mu      sync.Mutex
	entries []LogEntry
}

func NewMemWAL() *MemWAL { return &MemWAL{} }

func (w *MemWAL) Append(e LogEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, e)
	return nil
}

func (w *MemWAL) Entries() ([]LogEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := make([]LogEntry, len(w.entries))
	copy(cp, w.entries)
	return cp, nil
}

// lockMode distinguishes shared (read) from exclusive (write) locks.
type lockMode int

const (
	lockRead  lockMode = iota // shared: many readers allowed
	lockWrite                 // exclusive: one writer, no readers
)

// lockHolder records who holds or is waiting for a lock on one resource.
type lockHolder struct {
	txID TxID
	mode lockMode
}

// lockManager serializes access to named resources.
//
// The locking protocol is strict two-phase locking (S2PL): locks are acquired
// during execution and released only at commit/abort, guaranteeing
// serializability.
type lockManager struct {
	mu      sync.Mutex
	holders map[string][]lockHolder // resource -> current lock holders
}

func newLockManager() *lockManager {
	return &lockManager{
		holders: make(map[string][]lockHolder),
	}
}

// compatible reports whether a new request (mode m) is compatible with
// existing holders.
func compatible(holders []lockHolder, id TxID, m lockMode) bool {
	for _, h := range holders {
		if h.txID == id {
			continue // same transaction: upgrading or re-acquiring
		}
		if m == lockWrite || h.mode == lockWrite {
			return false
		}
	}
	return true
}

// tryAcquire attempts to grant a lock immediately. Returns true if granted.
// Must be called with lm.mu held.
func (lm *lockManager) tryAcquire(resource string, id TxID, m lockMode) bool {
	if compatible(lm.holders[resource], id, m) {
		lm.holders[resource] = append(lm.holders[resource], lockHolder{id, m})
		return true
	}
	return false
}

// holderIDs returns the set of transaction IDs currently holding a lock on
// resource, excluding id itself. Must be called with lm.mu held.
func (lm *lockManager) holderIDs(resource string, id TxID) []TxID {
	var out []TxID
	seen := map[TxID]struct{}{}
	for _, h := range lm.holders[resource] {
		if h.txID != id {
			if _, ok := seen[h.txID]; !ok {
				out = append(out, h.txID)
				seen[h.txID] = struct{}{}
			}
		}
	}
	return out
}

// Acquire grants the lock immediately or returns ErrConflict if the resource
// is held by another transaction in an incompatible mode.
//
// This implementation is non-blocking: it never queues the caller. A conflict
// means the coordinator must abort the current transaction and retry later.
// The caller must release the lock by calling Release when the transaction
// commits or aborts.
func (lm *lockManager) Acquire(resource string, id TxID, m lockMode) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if lm.tryAcquire(resource, id, m) {
		return nil
	}

	blockers := lm.holderIDs(resource, id)
	return fmt.Errorf("%w: resource %q held by %v", ErrConflict, resource, blockers)
}

// Release removes all locks held by transaction id.
func (lm *lockManager) Release(id TxID) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	for resource, holders := range lm.holders {
		filtered := holders[:0]
		for _, h := range holders {
			if h.txID != id {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) == 0 {
			delete(lm.holders, resource)
		} else {
			lm.holders[resource] = filtered
		}
	}
}
```

### Exercise 3: The In-Memory Data Store and Participant Interface

Append to `txcoord.go`:

```go
// Participant is a resource manager that can execute 2PC phases.
type Participant interface {
	// Prepare stages the writes for txID without making them visible.
	// Returns ErrConflict if the resource is locked by another transaction.
	Prepare(txID TxID, writes map[string]string) error
	// Commit makes the staged writes visible and releases locks.
	Commit(txID TxID) error
	// Abort discards the staged writes and releases locks.
	Abort(txID TxID) error
}

// MemStore is an in-memory key-value store that acts as a 2PC participant.
// It uses pessimistic locking through the shared lock manager.
type MemStore struct {
	mu     sync.RWMutex
	data   map[string]string // committed state
	staged map[TxID]map[string]string
	lm     *lockManager
}

func NewMemStore(lm *lockManager) *MemStore {
	return &MemStore{
		data:   make(map[string]string),
		staged: make(map[TxID]map[string]string),
		lm:     lm,
	}
}

// Set seeds the store with initial committed data (for tests and demos).
func (s *MemStore) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Get returns the committed value for key visible outside any transaction.
func (s *MemStore) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *MemStore) Prepare(txID TxID, writes map[string]string) error {
	for key := range writes {
		if err := s.lm.Acquire(key, txID, lockWrite); err != nil {
			return fmt.Errorf("store prepare: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]string, len(writes))
	for k, v := range writes {
		cp[k] = v
	}
	s.staged[txID] = cp
	return nil
}

func (s *MemStore) Commit(txID TxID) error {
	s.mu.Lock()
	writes, ok := s.staged[txID]
	if ok {
		for k, v := range writes {
			s.data[k] = v
		}
		delete(s.staged, txID)
	}
	s.mu.Unlock()
	s.lm.Release(txID)
	return nil
}

func (s *MemStore) Abort(txID TxID) error {
	s.mu.Lock()
	delete(s.staged, txID)
	s.mu.Unlock()
	s.lm.Release(txID)
	return nil
}
```

### Exercise 4: The Coordinator

Append to `txcoord.go`:

```go
// Coordinator manages distributed transactions across multiple participants.
// It is safe for concurrent use.
type Coordinator struct {
	mu           sync.Mutex
	wal          WAL
	lm           *lockManager
	transactions map[TxID]*tx
	nextID       int
}

// NewCoordinator creates a Coordinator backed by wal. Call Recover after
// construction to replay any in-doubt transactions from a previous run.
func NewCoordinator(wal WAL) *Coordinator {
	return &Coordinator{
		wal:          wal,
		lm:           newLockManager(),
		transactions: make(map[TxID]*tx),
	}
}

// Begin starts a new transaction and returns its ID.
func (c *Coordinator) Begin() (TxID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := TxID(fmt.Sprintf("tx-%d", c.nextID))
	if err := c.wal.Append(LogEntry{TxID: id, Kind: LogBegin, At: time.Now().UTC()}); err != nil {
		return "", fmt.Errorf("coordinator begin: %w", err)
	}
	c.transactions[id] = &tx{
		id:        id,
		state:     TxActive,
		startedAt: time.Now(),
		writes:    make(map[string]string),
		reads:     make(map[string]struct{}),
	}
	return id, nil
}

// Write stages a key-value write for txID on participant p. The write is not
// visible to other transactions until Commit is called.
func (c *Coordinator) Write(txID TxID, p Participant, key, value string) error {
	c.mu.Lock()
	t, ok := c.transactions[txID]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnknownTx, txID)
	}
	if t.state != TxActive {
		c.mu.Unlock()
		return fmt.Errorf("%w: transaction %s is %s", ErrAborted, txID, t.state)
	}
	t.writes[key] = value
	c.mu.Unlock()

	// Acquire a write lock immediately so conflicts surface before Commit.
	if err := c.lm.Acquire(key, txID, lockWrite); err != nil {
		return fmt.Errorf("coordinator write: %w", err)
	}
	return nil
}

// Commit runs two-phase commit across all participants that received writes.
// On success the transaction is durably committed. On failure all writes are
// rolled back.
func (c *Coordinator) Commit(txID TxID, participants []Participant) error {
	c.mu.Lock()
	t, ok := c.transactions[txID]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnknownTx, txID)
	}
	if t.state != TxActive {
		c.mu.Unlock()
		return fmt.Errorf("%w: %s is %s", ErrAborted, txID, t.state)
	}
	writes := t.writes
	c.mu.Unlock()

	// Phase 1: Prepare all participants.
	for _, p := range participants {
		if err := p.Prepare(txID, writes); err != nil {
			// At least one participant voted No; abort everyone.
			_ = c.wal.Append(LogEntry{TxID: txID, Kind: LogAbort, At: time.Now().UTC()})
			for _, ap := range participants {
				_ = ap.Abort(txID)
			}
			c.mu.Lock()
			t.state = TxAborted
			c.mu.Unlock()
			return fmt.Errorf("%w: prepare failed: %v", ErrAborted, err)
		}
	}

	// Log Prepare (all voted Yes) before sending the commit decision.
	if err := c.wal.Append(LogEntry{TxID: txID, Kind: LogPrepare, At: time.Now().UTC()}); err != nil {
		_ = c.wal.Append(LogEntry{TxID: txID, Kind: LogAbort, At: time.Now().UTC()})
		for _, p := range participants {
			_ = p.Abort(txID)
		}
		c.mu.Lock()
		t.state = TxAborted
		c.mu.Unlock()
		return fmt.Errorf("coordinator commit: log prepare: %w", err)
	}

	// Log Commit durably — from this point the transaction MUST commit.
	if err := c.wal.Append(LogEntry{TxID: txID, Kind: LogCommit, At: time.Now().UTC()}); err != nil {
		return fmt.Errorf("coordinator commit: log commit: %w", err)
	}

	// Phase 2: Commit all participants.
	for _, p := range participants {
		if err := p.Commit(txID); err != nil {
			// Log the error; in a real system we would retry until success.
			return fmt.Errorf("coordinator commit: participant commit: %w", err)
		}
	}

	c.mu.Lock()
	t.state = TxCommitted
	c.mu.Unlock()
	return nil
}

// Abort rolls back the transaction and releases all locks.
func (c *Coordinator) Abort(txID TxID, participants []Participant) error {
	c.mu.Lock()
	t, ok := c.transactions[txID]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnknownTx, txID)
	}
	if t.state == TxCommitted {
		c.mu.Unlock()
		return nil // already committed; nothing to abort
	}
	c.mu.Unlock()

	if err := c.wal.Append(LogEntry{TxID: txID, Kind: LogAbort, At: time.Now().UTC()}); err != nil {
		return fmt.Errorf("coordinator abort: %w", err)
	}
	for _, p := range participants {
		_ = p.Abort(txID)
	}

	c.mu.Lock()
	t.state = TxAborted
	c.mu.Unlock()
	return nil
}

// Recover reads the WAL and resolves in-doubt transactions using presumed-abort.
// Call once after constructing a coordinator whose WAL contains entries from a
// previous run.
func (c *Coordinator) Recover(participants []Participant) error {
	entries, err := c.wal.Entries()
	if err != nil {
		return fmt.Errorf("coordinator recover: %w", err)
	}

	type txPhase struct {
		committed bool
		aborted   bool
		prepared  bool
	}
	phases := map[TxID]*txPhase{}

	for _, e := range entries {
		if phases[e.TxID] == nil {
			phases[e.TxID] = &txPhase{}
		}
		switch e.Kind {
		case LogCommit:
			phases[e.TxID].committed = true
		case LogAbort:
			phases[e.TxID].aborted = true
		case LogPrepare:
			phases[e.TxID].prepared = true
		}
	}

	for txID, ph := range phases {
		switch {
		case ph.committed:
			// Already committed; ensure participants also committed.
			for _, p := range participants {
				_ = p.Commit(txID)
			}
		case ph.aborted || !ph.prepared:
			// Presumed abort: no PREPARE or explicit ABORT entry.
			for _, p := range participants {
				_ = p.Abort(txID)
			}
		default:
			// In-doubt: prepared but no decision. Presume abort.
			_ = c.wal.Append(LogEntry{TxID: txID, Kind: LogAbort, At: time.Now().UTC()})
			for _, p := range participants {
				_ = p.Abort(txID)
			}
		}
	}
	return nil
}

// TxInfo is the externally observable state of a transaction.
type TxInfo struct {
	ID        TxID
	State     TxState
	StartedAt time.Time
}

// Info returns a snapshot of the transaction's current state.
func (c *Coordinator) Info(txID TxID) (TxInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.transactions[txID]
	if !ok {
		return TxInfo{}, fmt.Errorf("%w: %s", ErrUnknownTx, txID)
	}
	return TxInfo{ID: t.id, State: t.state, StartedAt: t.startedAt}, nil
}
```

### Exercise 5: Test the Contract

Create `txcoord_test.go`:

```go
package txcoord

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// helpers

func newCoordAndStore() (*Coordinator, *MemStore) {
	wal := NewMemWAL()
	coord := NewCoordinator(wal)
	store := NewMemStore(coord.lm)
	return coord, store
}

// TestCommitAppliesWrites verifies that a committed write becomes visible.
func TestCommitAppliesWrites(t *testing.T) {
	t.Parallel()

	coord, store := newCoordAndStore()
	participants := []Participant{store}

	txID, err := coord.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := coord.Write(txID, store, "balance:alice", "100"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := coord.Commit(txID, participants); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, ok := store.Get("balance:alice")
	if !ok || got != "100" {
		t.Fatalf("balance:alice = %q %v, want 100 true", got, ok)
	}
}

// TestAbortDiscardsWrites verifies that an aborted write is invisible.
func TestAbortDiscardsWrites(t *testing.T) {
	t.Parallel()

	coord, store := newCoordAndStore()
	store.Set("balance:alice", "200")
	participants := []Participant{store}

	txID, _ := coord.Begin()
	_ = coord.Write(txID, store, "balance:alice", "999")
	if err := coord.Abort(txID, participants); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	got, _ := store.Get("balance:alice")
	if got != "200" {
		t.Fatalf("balance:alice = %q after abort, want 200", got)
	}
}

// TestWriteConflictDetected verifies that a second transaction attempting to
// write the same key while the first holds a write lock gets ErrConflict.
func TestWriteConflictDetected(t *testing.T) {
	t.Parallel()

	coord, store := newCoordAndStore()

	tx1, _ := coord.Begin()
	tx2, _ := coord.Begin()

	if err := coord.Write(tx1, store, "account:alice", "500"); err != nil {
		t.Fatalf("tx1 Write: %v", err)
	}

	err := coord.Write(tx2, store, "account:alice", "300")
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

// TestLockConflictBothDirections verifies that both transactions in a circular
// lock request pattern receive ErrConflict. This is what a non-blocking lock
// manager returns: it does not queue requests or detect cycles; it rejects any
// incompatible request immediately.
func TestLockConflictBothDirections(t *testing.T) {
	t.Parallel()

	lm := newLockManager()

	// tx1 holds key-A, tx2 holds key-B.
	tx1 := TxID("tx-conflict-1")
	tx2 := TxID("tx-conflict-2")

	if err := lm.Acquire("key-A", tx1, lockWrite); err != nil {
		t.Fatalf("tx1 acquire key-A: %v", err)
	}
	if err := lm.Acquire("key-B", tx2, lockWrite); err != nil {
		t.Fatalf("tx2 acquire key-B: %v", err)
	}

	// tx1 tries to acquire key-B (held by tx2): must get ErrConflict.
	err1 := lm.Acquire("key-B", tx1, lockWrite)
	if !errors.Is(err1, ErrConflict) {
		t.Fatalf("tx1->key-B: want ErrConflict, got %v", err1)
	}

	// tx2 tries to acquire key-A (held by tx1): must also get ErrConflict.
	err2 := lm.Acquire("key-A", tx2, lockWrite)
	if !errors.Is(err2, ErrConflict) {
		t.Fatalf("tx2->key-A: want ErrConflict, got %v", err2)
	}
}

// TestTransactionStateProgression verifies that state transitions are correct.
func TestTransactionStateProgression(t *testing.T) {
	t.Parallel()

	coord, store := newCoordAndStore()
	participants := []Participant{store}

	txID, _ := coord.Begin()
	info, err := coord.Info(txID)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.State != TxActive {
		t.Fatalf("initial state = %v, want active", info.State)
	}

	_ = coord.Write(txID, store, "k", "v")
	_ = coord.Commit(txID, participants)

	info, _ = coord.Info(txID)
	if info.State != TxCommitted {
		t.Fatalf("post-commit state = %v, want committed", info.State)
	}
}

// TestRecoveryPresumesAbortForIndoubt verifies that a transaction that reached
// PREPARE but never got a COMMIT is aborted on recovery.
func TestRecoveryPresumesAbortForIndoubt(t *testing.T) {
	t.Parallel()

	wal := NewMemWAL()
	indoubtID := TxID("tx-indoubt")

	// Simulate a WAL from a previous crashed run: BEGIN + PREPARE, no COMMIT.
	_ = wal.Append(LogEntry{TxID: indoubtID, Kind: LogBegin})
	_ = wal.Append(LogEntry{TxID: indoubtID, Kind: LogPrepare})

	lm := newLockManager()
	store := NewMemStore(lm)
	// Stage a write as if the participant prepared.
	store.staged[indoubtID] = map[string]string{"balance:alice": "999"}

	coord := NewCoordinator(wal)
	if err := coord.Recover([]Participant{store}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The in-doubt transaction must have been aborted; staged write must be gone.
	store.mu.RLock()
	_, hasStaged := store.staged[indoubtID]
	store.mu.RUnlock()
	if hasStaged {
		t.Fatal("staged write should have been cleared by recovery abort")
	}
	// The committed data must not contain the aborted write.
	if v, ok := store.Get("balance:alice"); ok {
		t.Fatalf("aborted write leaked into committed state: %q", v)
	}
}

// TestRecoveryCommitsKnownCommitted verifies that a transaction logged as
// COMMIT is re-committed during recovery.
func TestRecoveryCommitsKnownCommitted(t *testing.T) {
	t.Parallel()

	wal := NewMemWAL()
	committedID := TxID("tx-committed")

	_ = wal.Append(LogEntry{TxID: committedID, Kind: LogBegin})
	_ = wal.Append(LogEntry{TxID: committedID, Kind: LogPrepare})
	_ = wal.Append(LogEntry{TxID: committedID, Kind: LogCommit})

	lm := newLockManager()
	store := NewMemStore(lm)
	store.staged[committedID] = map[string]string{"balance:bob": "500"}

	coord := NewCoordinator(wal)
	if err := coord.Recover([]Participant{store}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	v, ok := store.Get("balance:bob")
	if !ok || v != "500" {
		t.Fatalf("balance:bob = %q %v after recovery commit, want 500 true", v, ok)
	}
}

// TestConcurrentNonConflictingTransactions verifies that transactions on
// distinct keys proceed without interfering.
func TestConcurrentNonConflictingTransactions(t *testing.T) {
	t.Parallel()

	coord, store := newCoordAndStore()
	participants := []Participant{store}
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			val := fmt.Sprintf("val-%d", i)
			txID, err := coord.Begin()
			if err != nil {
				errs[i] = err
				return
			}
			if err := coord.Write(txID, store, key, val); err != nil {
				errs[i] = err
				return
			}
			errs[i] = coord.Commit(txID, participants)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%d", i)
		want := fmt.Sprintf("val-%d", i)
		if got, ok := store.Get(key); !ok || got != want {
			t.Errorf("%s = %q %v, want %q true", key, got, ok, want)
		}
	}
}

// TestLogKindString verifies the string representation of LogKind.
func TestLogKindString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		k    LogKind
		want string
	}{
		{LogBegin, "BEGIN"},
		{LogPrepare, "PREPARE"},
		{LogCommit, "COMMIT"},
		{LogAbort, "ABORT"},
	}
	for _, tc := range cases {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("LogKind(%d).String() = %q, want %q", tc.k, got, tc.want)
		}
	}
}

// TestTxStateString verifies the string representation of TxState.
func TestTxStateString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		s    TxState
		want string
	}{
		{TxActive, "active"},
		{TxPreparing, "preparing"},
		{TxCommitted, "committed"},
		{TxAborted, "aborted"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("TxState(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// ExampleCoordinator_Commit shows a minimal cross-store transfer: Alice pays
// Bob. Both accounts live in the same store for simplicity.
func ExampleCoordinator_Commit() {
	wal := NewMemWAL()
	coord := NewCoordinator(wal)
	store := NewMemStore(coord.lm)
	store.Set("alice", "100")
	store.Set("bob", "50")

	txID, _ := coord.Begin()
	_ = coord.Write(txID, store, "alice", "80")
	_ = coord.Write(txID, store, "bob", "70")
	_ = coord.Commit(txID, []Participant{store})

	a, _ := store.Get("alice")
	b, _ := store.Get("bob")
	fmt.Printf("alice=%s bob=%s\n", a, b)
	// Output: alice=80 bob=70
}
```

Your turn: add `TestUnknownTxReturnsError` that calls `coord.Write` and
`coord.Info` with a TxID that was never created and asserts that both return
an error wrapping `ErrUnknownTx`.

### Exercise 6: The Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"

	"example.com/txcoord"
)

func main() {
	wal := txcoord.NewMemWAL()
	coord := txcoord.NewCoordinator(wal)

	// Two stores represent two partitions of the banking database.
	// A real system would route by account prefix; here we use two named stores.
	storeA := txcoord.NewMemStore(coord.LockManager())
	storeB := txcoord.NewMemStore(coord.LockManager())
	storeA.Set("alice", "1000")
	storeB.Set("bob", "500")

	participants := []txcoord.Participant{storeA, storeB}

	fmt.Println("=== transfer $200 from alice (storeA) to bob (storeB) ===")

	txID, err := coord.Begin()
	if err != nil {
		log.Fatal(err)
	}

	if err := coord.Write(txID, storeA, "alice", "800"); err != nil {
		log.Fatalf("write alice: %v", err)
	}
	if err := coord.Write(txID, storeB, "bob", "700"); err != nil {
		log.Fatalf("write bob: %v", err)
	}

	if err := coord.Commit(txID, participants); err != nil {
		log.Fatalf("commit: %v", err)
	}

	a, _ := storeA.Get("alice")
	b, _ := storeB.Get("bob")
	fmt.Printf("alice = %s (was 1000)\n", a)
	fmt.Printf("bob   = %s (was 500)\n", b)

	info, _ := coord.Info(txID)
	fmt.Printf("tx %s state = %s\n", info.ID, info.State)

	fmt.Println()
	fmt.Println("=== conflict: two transactions race to update alice ===")

	tx1, _ := coord.Begin()
	tx2, _ := coord.Begin()

	if err := coord.Write(tx1, storeA, "alice", "600"); err != nil {
		log.Fatalf("tx1 write alice: %v", err)
	}

	// tx2 attempts to write alice while tx1 holds the lock — expect conflict.
	err = coord.Write(tx2, storeA, "alice", "400")
	if err != nil {
		fmt.Printf("tx2 write blocked: %v\n", err)
	}

	// Commit tx1 so the value lands.
	if err := coord.Commit(tx1, []txcoord.Participant{storeA}); err != nil {
		log.Fatalf("tx1 commit: %v", err)
	}
	_ = coord.Abort(tx2, []txcoord.Participant{storeA, storeB})

	a, _ = storeA.Get("alice")
	fmt.Printf("alice after conflict resolution = %s\n", a)

	fmt.Println()
	fmt.Println("=== WAL entries ===")
	entries, _ := wal.Entries()
	for _, e := range entries {
		fmt.Printf("  %s  %s\n", e.Kind, e.TxID)
	}
}
```

The demo uses `coord.LockManager()` — add this exported accessor to
`txcoord.go` so `cmd/demo` (a separate `package main`) can reach the shared
lock manager:

Append to `txcoord.go`:

```go
// LockManager returns the coordinator's shared lock manager. Callers that
// create MemStore instances sharing the same coordinator must pass this to
// NewMemStore so lock conflicts are detected across stores.
func (c *Coordinator) LockManager() *lockManager {
	return c.lm
}
```

Run the demo with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Logging After Acting Instead of Before

Wrong: commit the participant, then append LogCommit to the WAL.

Fix: append LogCommit before sending the commit message to any participant.
If the coordinator crashes between the log write and the broadcast, recovery
can re-send the commit. If the log write happens after, a crash leaves the
decision unknown and the transaction is presumed aborted — even though at
least one participant may have already committed.

### Releasing Locks Before Commit Completes

Wrong: call `lm.Release(txID)` at the start of Phase 2, before all
participants have confirmed.

Fix: each participant releases its own locks only inside its `Commit` or
`Abort` method, after the write is durable. Early release breaks the S2PL
guarantee: another transaction can read a half-committed state.

### Holding Locks After Conflict

Wrong: when `Acquire` returns `ErrConflict`, skip calling `Abort` and leave
the transaction's previously acquired locks in place.

Fix: any conflict must trigger a full abort: call `lm.Release` via the
participant's `Abort` method before retrying. Locks held by an aborted-but-not-
released transaction prevent all other transactions from acquiring those
resources, starving the system.

### Not Using errors.Is for Sentinel Errors

Wrong: `if err.Error() == "txcoord: write conflict"`.

Fix: `if errors.Is(err, ErrConflict)`. The sentinel errors in this lesson
are wrapped with `fmt.Errorf("%w: ...", ErrConflict)`, so `errors.Is`
unwraps through the chain. String comparison breaks as soon as the message
changes.

### Assuming 2PC Is Non-Blocking

Wrong: designing a system that blocks an unbounded number of participants on
coordinator availability.

Fix: document the blocking window explicitly (between LogPrepare and the
decision broadcast). For workloads that require non-blocking progress under
coordinator failure, prefer a saga (lesson 17) or a consensus-based commit
protocol (Paxos/Raft).

## Verification

From `~/go-exercises/txcoord`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector verifies that concurrent transactions
do not share mutable state without synchronization.

## Summary

- Two-phase commit (2PC) guarantees atomicity across participants: all commit
  or all abort. Phase 1 collects votes (Prepare); Phase 2 broadcasts the
  decision (Commit or Abort).
- The blocking problem: if the coordinator crashes after all votes are
  collected but before the decision is broadcast, prepared participants are
  stuck. Presumed-abort resolves this at the cost of aborting some
  transactions that could have committed.
- Strict two-phase locking (S2PL) holds all locks until commit or abort,
  guaranteeing serializability. Locks are released only inside `Commit` or
  `Abort`.
- The lock manager is non-blocking: `Acquire` either grants immediately or
  returns `ErrConflict`. The coordinator aborts the conflicting transaction and
  may retry. This avoids deadlock by construction but aborts more aggressively
  than a blocking design with cycle detection would.
- The write-ahead log records every phase transition before it takes effect.
  On recovery, committed transactions are re-applied; in-doubt transactions
  (PREPARE but no COMMIT/ABORT) are presumed aborted.

## What's Next

Next: [Anti-Entropy Protocol](../21-anti-entropy-protocol/21-anti-entropy-protocol.md).

## Resources

- [Two-Phase Commit, Wikipedia](https://en.wikipedia.org/wiki/Two-phase_commit_protocol) -- protocol phases and the blocking problem
- [sync package, pkg.go.dev](https://pkg.go.dev/sync) -- Mutex, RWMutex used throughout this lesson
- [errors package, pkg.go.dev](https://pkg.go.dev/errors) -- sentinel errors and errors.Is for test assertions
- [CockroachDB Transaction Layer](https://www.cockroachlabs.com/docs/stable/architecture/transaction-layer.html) -- production distributed transaction implementation
- [Designing Data-Intensive Applications, Chapter 7 (Kleppmann)](https://dataintensive.net/) -- transactions and isolation levels in depth
