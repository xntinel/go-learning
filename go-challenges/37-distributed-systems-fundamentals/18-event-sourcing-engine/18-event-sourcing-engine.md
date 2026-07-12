# 18. Event Sourcing Engine

Event sourcing replaces mutable row updates with an append-only sequence of immutable events. The current state of an aggregate is always derived by replaying its events from scratch — or from the last snapshot. The hard parts are optimistic concurrency control (two writers racing on the same aggregate), snapshot strategy (when to cut one, how to load from it), and projection consistency (a subscriber that falls behind must catch up without duplicating side effects). This lesson builds a self-contained in-memory engine that exercises all three.

```text
eventsource/
  go.mod
  store.go          -- EventStore: Append, Load, Subscribe
  aggregate.go      -- Aggregate interface + BankAccount implementation
  snapshot.go       -- SnapshotStore: Save, Load
  projection.go     -- Projection: subscribes and materialises a read model
  store_test.go     -- table-driven tests, race detector
  cmd/demo/main.go  -- runnable demo touching only exported API
```

## Concepts

### Events Are the Source of Truth

In an event-sourced system the event log is the primary record. There are no UPDATE or DELETE statements. Every command either produces one or more events (and those events are appended) or is rejected. The current state of an aggregate is a pure function of its event history: `state = fold(apply, zero, events)`.

This has a direct Go consequence: the `Apply` method on an aggregate must be a pure state transition. It must not produce side effects. All side effects (emitting further events, calling external services) belong in the command handler.

### Optimistic Concurrency With Expected Version

Two goroutines may both load an aggregate at version 5, process a command, and try to append events. Without coordination the second writer would silently overwrite the first's events. The solution is version checking: the caller supplies the version it last observed; the store rejects an append if the current version differs.

```go
// Wrong: append without a version check loses concurrent updates.
store.Append(id, events, -1) // unconditional — WRONG

// Right: the caller tracks the version it loaded.
store.Append(id, newEvents, loadedVersion)
```

The sentinel error `ErrVersionConflict` lets callers distinguish a conflict (retry) from a permanent failure (return to user).

### Snapshots Bound Replay Cost

If an aggregate accumulates thousands of events, replaying them all on every load is expensive. A snapshot saves the serialised state at a particular version; loading an aggregate then means: load the latest snapshot (if any), then replay only the events from `snapshot.Version+1` onward.

The trade-off: snapshots add write-path complexity (when to cut, what to serialise) and a new storage layer. The common heuristic is to cut a snapshot every N events. For this lesson N is configurable.

### Projections Are Eventually Consistent Read Models

A projection subscribes to the event stream and maintains a materialised view. In this lesson the `AccountSummary` projection tracks the current balance and transaction count per account. Projections are read-only: they never write back to the event store.

The ordering guarantee: because the in-memory store notifies subscribers synchronously under a write lock, every projection sees events in append order. In a real system (Kafka, EventStoreDB) you lose synchronous delivery and must handle at-least-once delivery and idempotency.

### Temporal Queries

Because all events carry a timestamp, the state at any past instant can be recovered by replaying only the events whose timestamp is at or before the target time. This is one of the strongest arguments for event sourcing: the audit trail is not a separate concern — it is the primary storage.

## Exercises

This is a library, not a program: there is no top-level `main`. You verify it with `go test`.

### Exercise 1: The Event Store

The store holds an append-only log per aggregate and notifies subscribers on every successful append.

Create `store.go`:

```go
package eventsource

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrVersionConflict is returned when the expected version does not match the
// current aggregate version, indicating a concurrent write conflict.
var ErrVersionConflict = errors.New("event store: version conflict")

// Event is an immutable record of something that happened to an aggregate.
type Event struct {
	AggregateID string
	Type        string
	Version     int
	OccurredAt  time.Time
	Data        any
}

// EventStore is a thread-safe, in-memory, append-only event log.
type EventStore struct {
	mu          sync.RWMutex
	streams     map[string][]Event // aggregateID -> ordered events
	subscribers []func(Event)
}

// NewEventStore returns an empty EventStore.
func NewEventStore() *EventStore {
	return &EventStore{streams: make(map[string][]Event)}
}

// Subscribe registers a handler that is called for every successfully appended
// event. Handlers are called synchronously under the write lock; keep them fast.
func (s *EventStore) Subscribe(h func(Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, h)
}

// Append appends events to the aggregate's stream. expectedVersion is the
// version of the last event the caller observed; pass -1 to assert the stream
// is empty. Returns ErrVersionConflict if the current version differs.
func (s *EventStore) Append(aggregateID string, events []Event, expectedVersion int) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stream := s.streams[aggregateID]
	currentVersion := len(stream) - 1 // -1 when stream is empty
	if currentVersion != expectedVersion {
		return fmt.Errorf("%w: aggregate %q at version %d, expected %d",
			ErrVersionConflict, aggregateID, currentVersion, expectedVersion)
	}

	next := len(stream)
	for i := range events {
		events[i].AggregateID = aggregateID
		events[i].Version = next + i
		if events[i].OccurredAt.IsZero() {
			events[i].OccurredAt = time.Now().UTC()
		}
	}
	s.streams[aggregateID] = append(stream, events...)
	for _, e := range events {
		for _, h := range s.subscribers {
			h(e)
		}
	}
	return nil
}

// Load returns events for an aggregate starting at fromVersion (inclusive).
// Use fromVersion 0 to load all events.
func (s *EventStore) Load(aggregateID string, fromVersion int) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stream := s.streams[aggregateID]
	if fromVersion >= len(stream) {
		return nil, nil
	}
	out := make([]Event, len(stream)-fromVersion)
	copy(out, stream[fromVersion:])
	return out, nil
}

// LoadUntil returns events for an aggregate up to and including the given time.
func (s *EventStore) LoadUntil(aggregateID string, until time.Time) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	for _, e := range s.streams[aggregateID] {
		if !e.OccurredAt.After(until) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Version returns the current version of an aggregate (-1 if unknown).
func (s *EventStore) Version(aggregateID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.streams[aggregateID]) - 1
}
```

### Exercise 2: The Aggregate Interface and BankAccount

An aggregate is anything that can apply an event to update its state and handle a command to produce new events.

Create `aggregate.go`:

```go
package eventsource

import (
	"errors"
	"fmt"
)

// ErrInsufficientFunds is returned when a Withdraw command would make the
// balance negative.
var ErrInsufficientFunds = errors.New("bank account: insufficient funds")

// ErrAccountClosed is returned when a command targets a closed account.
var ErrAccountClosed = errors.New("bank account: account is closed")

// Aggregate is the interface all aggregates must implement.
type Aggregate interface {
	// Apply updates the aggregate's state from a single event. It must be
	// a pure function with no side effects.
	Apply(e Event)
	// AggregateID returns the identifier for this aggregate.
	AggregateID() string
	// Version returns the version of the last applied event (-1 if no events).
	Version() int
}

// Command types for BankAccount.
type (
	OpenAccount  struct{ InitialBalance int64 }
	Deposit      struct{ Amount int64 }
	Withdraw     struct{ Amount int64 }
	CloseAccount struct{}
)

// Event types emitted by BankAccount.
const (
	EventAccountOpened = "AccountOpened"
	EventDeposited     = "MoneyDeposited"
	EventWithdrawn     = "MoneyWithdrawn"
	EventAccountClosed = "AccountClosed"
)

// BankAccount is an event-sourced aggregate representing a bank account.
type BankAccount struct {
	id      string
	version int
	balance int64
	open    bool
}

// NewBankAccount returns a zero-value BankAccount with the given ID. Call
// Rebuild to replay events onto it.
func NewBankAccount(id string) *BankAccount {
	return &BankAccount{id: id, version: -1}
}

// AggregateID implements Aggregate.
func (a *BankAccount) AggregateID() string { return a.id }

// Version implements Aggregate.
func (a *BankAccount) Version() int { return a.version }

// Balance returns the current balance in cents.
func (a *BankAccount) Balance() int64 { return a.balance }

// IsOpen reports whether the account is open.
func (a *BankAccount) IsOpen() bool { return a.open }

// Apply implements Aggregate. It updates state from a single event and must
// not produce side effects.
func (a *BankAccount) Apply(e Event) {
	switch e.Type {
	case EventAccountOpened:
		d := e.Data.(OpenAccount)
		a.balance = d.InitialBalance
		a.open = true
	case EventDeposited:
		d := e.Data.(Deposit)
		a.balance += d.Amount
	case EventWithdrawn:
		d := e.Data.(Withdraw)
		a.balance -= d.Amount
	case EventAccountClosed:
		a.open = false
	}
	a.version = e.Version
}

// Handle processes a command against the current state and returns the events
// to append. It does not append events itself.
func (a *BankAccount) Handle(cmd any) ([]Event, error) {
	switch c := cmd.(type) {
	case OpenAccount:
		if a.open {
			return nil, fmt.Errorf("bank account %q: already open", a.id)
		}
		return []Event{{Type: EventAccountOpened, Data: c}}, nil
	case Deposit:
		if !a.open {
			return nil, fmt.Errorf("%w: %q", ErrAccountClosed, a.id)
		}
		if c.Amount <= 0 {
			return nil, fmt.Errorf("bank account: deposit amount must be positive, got %d", c.Amount)
		}
		return []Event{{Type: EventDeposited, Data: c}}, nil
	case Withdraw:
		if !a.open {
			return nil, fmt.Errorf("%w: %q", ErrAccountClosed, a.id)
		}
		if c.Amount > a.balance {
			return nil, fmt.Errorf("%w: balance %d, requested %d", ErrInsufficientFunds, a.balance, c.Amount)
		}
		return []Event{{Type: EventWithdrawn, Data: c}}, nil
	case CloseAccount:
		if !a.open {
			return nil, fmt.Errorf("%w: %q", ErrAccountClosed, a.id)
		}
		return []Event{{Type: EventAccountClosed, Data: c}}, nil
	default:
		return nil, fmt.Errorf("bank account: unknown command %T", cmd)
	}
}

// Rebuild replays a slice of events onto the aggregate.
func Rebuild(a *BankAccount, events []Event) {
	for _, e := range events {
		a.Apply(e)
	}
}
```

### Exercise 3: Snapshot Store

Snapshots bound the cost of aggregate reconstruction for long event histories.

Create `snapshot.go`:

```go
package eventsource

import (
	"sync"
)

// Snapshot captures the serialised state of an aggregate at a given version.
type Snapshot struct {
	AggregateID string
	Version     int
	Balance     int64 // BankAccount-specific; use encoding/json for generic state
	Open        bool
}

// SnapshotStore is a thread-safe in-memory snapshot repository.
type SnapshotStore struct {
	mu    sync.RWMutex
	snaps map[string]Snapshot
}

// NewSnapshotStore returns an empty SnapshotStore.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{snaps: make(map[string]Snapshot)}
}

// Save stores the latest snapshot for an aggregate, overwriting any previous one.
func (ss *SnapshotStore) Save(s Snapshot) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.snaps[s.AggregateID] = s
}

// Load returns the most recent snapshot for an aggregate, or (zero, false) if
// none exists.
func (ss *SnapshotStore) Load(aggregateID string) (Snapshot, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	s, ok := ss.snaps[aggregateID]
	return s, ok
}

// SnapshotOf captures the current state of a BankAccount into a Snapshot.
func SnapshotOf(a *BankAccount) Snapshot {
	return Snapshot{
		AggregateID: a.id,
		Version:     a.version,
		Balance:     a.balance,
		Open:        a.open,
	}
}

// RestoreSnapshot applies a snapshot to a zero-value BankAccount.
func RestoreSnapshot(a *BankAccount, s Snapshot) {
	a.balance = s.Balance
	a.open = s.Open
	a.version = s.Version
}
```

### Exercise 4: Projection

A projection subscribes to the event stream and builds a read-optimised view.

Create `projection.go`:

```go
package eventsource

import (
	"sync"
)

// AccountSummary is the read model maintained by the AccountProjection.
type AccountSummary struct {
	AggregateID string
	Balance     int64
	TxCount     int
	Open        bool
}

// AccountProjection listens on the event stream and maintains per-account summaries.
type AccountProjection struct {
	mu       sync.RWMutex
	accounts map[string]*AccountSummary
}

// NewAccountProjection returns a ready projection. Call store.Subscribe(p.Handle)
// to wire it to the event store.
func NewAccountProjection() *AccountProjection {
	return &AccountProjection{accounts: make(map[string]*AccountSummary)}
}

// Handle processes a single event. It is safe to call from multiple goroutines.
func (p *AccountProjection) Handle(e Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc := p.accounts[e.AggregateID]
	if acc == nil {
		acc = &AccountSummary{AggregateID: e.AggregateID}
		p.accounts[e.AggregateID] = acc
	}
	switch e.Type {
	case EventAccountOpened:
		d := e.Data.(OpenAccount)
		acc.Balance = d.InitialBalance
		acc.Open = true
	case EventDeposited:
		acc.Balance += e.Data.(Deposit).Amount
		acc.TxCount++
	case EventWithdrawn:
		acc.Balance -= e.Data.(Withdraw).Amount
		acc.TxCount++
	case EventAccountClosed:
		acc.Open = false
	}
}

// Get returns the summary for an account, or (zero, false) if not found.
func (p *AccountProjection) Get(aggregateID string) (AccountSummary, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	acc, ok := p.accounts[aggregateID]
	if !ok {
		return AccountSummary{}, false
	}
	return *acc, true
}
```

### Exercise 5: Tests

Create `store_test.go`:

```go
package eventsource

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAppendAndLoad(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	id := "acct-1"

	acct := NewBankAccount(id)
	evts, err := acct.Handle(OpenAccount{InitialBalance: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(id, evts, -1); err != nil {
		t.Fatal(err)
	}
	acct.Apply(evts[0])

	evts2, err := acct.Handle(Deposit{Amount: 500})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(id, evts2, acct.version); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("len(loaded) = %d, want 2", len(loaded))
	}
}

func TestVersionConflict(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	id := "acct-conflict"

	e1 := []Event{{Type: EventAccountOpened, Data: OpenAccount{InitialBalance: 0}}}
	if err := store.Append(id, e1, -1); err != nil {
		t.Fatal(err)
	}

	// Both goroutines observed version 0; only one can win.
	e2 := []Event{{Type: EventDeposited, Data: Deposit{Amount: 100}}}
	e3 := []Event{{Type: EventDeposited, Data: Deposit{Amount: 200}}}

	err2 := store.Append(id, e2, 0)
	err3 := store.Append(id, e3, 0)

	// Exactly one must succeed and one must be a version conflict.
	if err2 == nil && err3 == nil {
		t.Fatal("both appends succeeded; expected one version conflict")
	}
	if err2 != nil && err3 != nil {
		t.Fatalf("both appends failed: err2=%v err3=%v", err2, err3)
	}
	conflictErr := err2
	if conflictErr == nil {
		conflictErr = err3
	}
	if !errors.Is(conflictErr, ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", conflictErr)
	}
}

func TestRebuildFromEvents(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	id := "acct-rebuild"

	acct := NewBankAccount(id)
	cmds := []any{
		OpenAccount{InitialBalance: 1000},
		Deposit{Amount: 500},
		Withdraw{Amount: 200},
	}
	for _, cmd := range cmds {
		evts, err := acct.Handle(cmd)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(id, evts, acct.version); err != nil {
			t.Fatal(err)
		}
		Rebuild(acct, evts)
	}

	if acct.Balance() != 1300 {
		t.Fatalf("balance = %d, want 1300", acct.Balance())
	}

	// Rebuild from scratch to verify event log.
	fresh := NewBankAccount(id)
	loaded, _ := store.Load(id, 0)
	Rebuild(fresh, loaded)
	if fresh.Balance() != 1300 {
		t.Fatalf("rebuilt balance = %d, want 1300", fresh.Balance())
	}
}

func TestInsufficientFunds(t *testing.T) {
	t.Parallel()

	acct := NewBankAccount("acct-insuf")
	evts, _ := acct.Handle(OpenAccount{InitialBalance: 100})
	Rebuild(acct, evts)

	_, err := acct.Handle(Withdraw{Amount: 200})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
}

func TestAccountClosed(t *testing.T) {
	t.Parallel()

	acct := NewBankAccount("acct-closed")
	setup := []any{OpenAccount{InitialBalance: 500}, CloseAccount{}}
	for _, cmd := range setup {
		evts, err := acct.Handle(cmd)
		if err != nil {
			t.Fatal(err)
		}
		Rebuild(acct, evts)
	}

	_, err := acct.Handle(Deposit{Amount: 100})
	if !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("err = %v, want ErrAccountClosed", err)
	}
}

func TestSnapshotRestoresState(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	snaps := NewSnapshotStore()
	id := "acct-snap"

	acct := NewBankAccount(id)
	for _, cmd := range []any{
		OpenAccount{InitialBalance: 0},
		Deposit{Amount: 100},
		Deposit{Amount: 200},
		Deposit{Amount: 300},
	} {
		evts, _ := acct.Handle(cmd)
		store.Append(id, evts, acct.version) //nolint:errcheck
		Rebuild(acct, evts)
	}

	// Save snapshot after 4 events.
	snaps.Save(SnapshotOf(acct))

	// Append one more event after the snapshot.
	evts, _ := acct.Handle(Deposit{Amount: 50})
	store.Append(id, evts, acct.version) //nolint:errcheck
	Rebuild(acct, evts)

	// Restore: load snapshot + replay from snapshot.Version+1.
	snap, ok := snaps.Load(id)
	if !ok {
		t.Fatal("snapshot not found")
	}
	restored := NewBankAccount(id)
	RestoreSnapshot(restored, snap)
	tail, _ := store.Load(id, snap.Version+1)
	Rebuild(restored, tail)

	if restored.Balance() != acct.Balance() {
		t.Fatalf("restored balance = %d, want %d", restored.Balance(), acct.Balance())
	}
}

func TestProjectionTracksBalance(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	proj := NewAccountProjection()
	store.Subscribe(proj.Handle)

	id := "acct-proj"
	acct := NewBankAccount(id)
	for _, cmd := range []any{
		OpenAccount{InitialBalance: 0},
		Deposit{Amount: 300},
		Withdraw{Amount: 100},
	} {
		evts, _ := acct.Handle(cmd)
		store.Append(id, evts, acct.version) //nolint:errcheck
		Rebuild(acct, evts)
	}

	sum, ok := proj.Get(id)
	if !ok {
		t.Fatal("projection has no entry for account")
	}
	if sum.Balance != 200 {
		t.Fatalf("projection balance = %d, want 200", sum.Balance)
	}
	if sum.TxCount != 2 {
		t.Fatalf("projection txCount = %d, want 2", sum.TxCount)
	}
}

func TestTemporalQuery(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	id := "acct-temporal"
	base := time.Now()

	events := []Event{
		{Type: EventAccountOpened, Data: OpenAccount{InitialBalance: 1000}, OccurredAt: base},
		{Type: EventDeposited, Data: Deposit{Amount: 500}, OccurredAt: base.Add(1 * time.Minute)},
		{Type: EventWithdrawn, Data: Withdraw{Amount: 300}, OccurredAt: base.Add(2 * time.Minute)},
	}
	// Append all events claiming the same initial version to avoid the version check.
	// We simulate historical loading, so assign versions manually.
	for i := range events {
		events[i].Version = i
	}
	store.mu.Lock()
	store.streams[id] = events
	store.mu.Unlock()

	// State at base+90s should include only the first two events.
	historical, _ := store.LoadUntil(id, base.Add(90*time.Second))
	snap := NewBankAccount(id)
	Rebuild(snap, historical)
	if snap.Balance() != 1500 {
		t.Fatalf("temporal balance = %d, want 1500", snap.Balance())
	}
}

func TestMultipleSubscribers(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	var counts [2]int
	store.Subscribe(func(Event) { counts[0]++ })
	store.Subscribe(func(Event) { counts[1]++ })

	e := []Event{{Type: EventAccountOpened, Data: OpenAccount{InitialBalance: 0}}}
	store.Append("acct-sub", e, -1) //nolint:errcheck

	if counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("subscriber counts = %v, want [1 1]", counts)
	}
}

func ExampleNewBankAccount() {
	store := NewEventStore()
	proj := NewAccountProjection()
	store.Subscribe(proj.Handle)

	id := "demo-acct"
	acct := NewBankAccount(id)

	cmds := []any{
		OpenAccount{InitialBalance: 0},
		Deposit{Amount: 1000},
		Withdraw{Amount: 250},
	}
	for _, cmd := range cmds {
		evts, err := acct.Handle(cmd)
		if err != nil {
			panic(err)
		}
		if err := store.Append(id, evts, acct.version); err != nil {
			panic(err)
		}
		Rebuild(acct, evts)
	}

	sum, _ := proj.Get(id)
	fmt.Printf("balance=%d txCount=%d\n", sum.Balance, sum.TxCount)
	// Output: balance=750 txCount=2
}
```

### Exercise 6: Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"

	"example.com/eventsource"
)

func main() {
	store := eventsource.NewEventStore()
	snaps := eventsource.NewSnapshotStore()
	proj := eventsource.NewAccountProjection()
	store.Subscribe(proj.Handle)

	id := "alice"
	acct := eventsource.NewBankAccount(id)

	type step struct {
		cmd any
		tag string
	}
	steps := []step{
		{eventsource.OpenAccount{InitialBalance: 0}, "open"},
		{eventsource.Deposit{Amount: 5000}, "deposit 5000"},
		{eventsource.Deposit{Amount: 3000}, "deposit 3000"},
		{eventsource.Withdraw{Amount: 2000}, "withdraw 2000"},
	}

	for _, s := range steps {
		evts, err := acct.Handle(s.cmd)
		if err != nil {
			log.Fatalf("handle %s: %v", s.tag, err)
		}
		if err := store.Append(id, evts, acct.Version()); err != nil {
			log.Fatalf("append %s: %v", s.tag, err)
		}
		eventsource.Rebuild(acct, evts)
		fmt.Printf("after %-16s balance=%d\n", s.tag, acct.Balance())
	}

	// Save snapshot after 4 events.
	snaps.Save(eventsource.SnapshotOf(acct))
	fmt.Printf("snapshot saved at version %d\n", acct.Version())

	// One more event after the snapshot.
	evts, _ := acct.Handle(eventsource.Deposit{Amount: 1000})
	store.Append(id, evts, acct.Version()) //nolint:errcheck
	eventsource.Rebuild(acct, evts)

	// Restore from snapshot.
	snap, _ := snaps.Load(id)
	restored := eventsource.NewBankAccount(id)
	eventsource.RestoreSnapshot(restored, snap)
	tail, _ := store.Load(id, snap.Version+1)
	eventsource.Rebuild(restored, tail)
	fmt.Printf("restored balance=%d (matches live=%v)\n", restored.Balance(), restored.Balance() == acct.Balance())

	// Projection.
	sum, _ := proj.Get(id)
	fmt.Printf("projection: balance=%d txCount=%d open=%v\n", sum.Balance, sum.TxCount, sum.Open)

	// Demonstrate conflict detection.
	e1 := []eventsource.Event{{Type: eventsource.EventDeposited, Data: eventsource.Deposit{Amount: 10}}}
	e2 := []eventsource.Event{{Type: eventsource.EventDeposited, Data: eventsource.Deposit{Amount: 20}}}
	store.Append(id, e1, acct.Version()) //nolint:errcheck
	err := store.Append(id, e2, acct.Version())
	if err != nil {
		fmt.Println("conflict detected (expected):", err)
	}
}
```

Your turn: add a test `TestDepositZeroIsRejected` that calls `acct.Handle(Deposit{Amount: 0})` and asserts the error is non-nil. Then add a test `TestCloseAccountTwiceFails` that closes an open account and verifies a second `CloseAccount` command returns `ErrAccountClosed`.

## Common Mistakes

### Mutating Event Data After Append

Wrong: reusing and modifying the same event slice after passing it to `Append`. Because the store retains a reference to the appended events, the caller's mutation corrupts the log.

Fix: the `Handle` method always returns freshly allocated event slices. Never modify an event after it has been appended.

### Applying Events in the Command Handler Instead of Apply

Wrong:

```go
func (a *BankAccount) Handle(cmd any) ([]Event, error) {
	switch c := cmd.(type) {
	case Deposit:
		a.balance += c.Amount // mutates state directly — WRONG
		return []Event{{Type: EventDeposited, Data: c}}, nil
	}
	// ...
}
```

What happens: if `Append` fails (version conflict, validation error), the aggregate's in-memory state is already corrupted.

Fix: `Handle` only validates and produces events; `Apply` mutates state. The caller calls `Rebuild` only after a successful `Append`.

### Passing the Wrong Expected Version

Wrong: passing `0` as the expected version when loading an aggregate that has no events, then wondering why the first `Append` fails.

Fix: a fresh aggregate starts at version `-1` (no events). Pass `-1` to assert the stream is empty. Version `0` means "I have seen the first event".

### Forgetting to Replay Events After a Snapshot

Wrong: loading a snapshot, constructing the aggregate from it, and treating the snapshot version as current — missing any events appended after the snapshot.

Fix: after restoring from a snapshot at version V, load events from `V+1` and replay them with `Rebuild`.

### Blocking Projections

Wrong: doing slow work (database writes, HTTP calls) synchronously inside a subscriber registered with `Subscribe`. The store holds its write lock while calling subscribers, so a slow handler blocks all concurrent `Append` calls.

Fix: in a real system, use an outbox or a buffered channel to decouple projection updates from the write path. For this lesson, keep handlers fast (in-memory map updates only).

## Verification

From `~/go-exercises/eventsource`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must complete without errors. The race detector (`-race`) catches data races in concurrent append and projection scenarios.

## Summary

- Event sourcing stores state changes as immutable, append-only events; current state is a pure replay of that log.
- Optimistic concurrency control uses an expected-version check to detect concurrent writes and return `ErrVersionConflict`.
- Snapshots bound replay cost: restore the snapshot, then replay only the events after `snapshot.Version`.
- Projections are eventually consistent read models that subscribe to the event stream and materialise a view.
- Temporal queries replay events up to a given timestamp to reconstruct historical state.
- `Handle` only validates and produces events; `Apply` only mutates state. Never conflate the two.

## What's Next

Next: [CQRS and Eventual Consistency](../19-cqrs-eventual-consistency/19-cqrs-eventual-consistency.md).

## Resources

- [Event Sourcing (Martin Fowler)](https://martinfowler.com/eaaDev/EventSourcing.html) — pattern definition and trade-offs
- [pkg.go.dev: sync package](https://pkg.go.dev/sync) — RWMutex, used for the event store and projection
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) — concurrency idioms underlying the projection handler design
- [CQRS Documents (Greg Young)](https://cqrs.files.wordpress.com/2010/11/cqrs_documents.pdf) — foundational paper on event sourcing and CQRS
- [pkg.go.dev: errors package](https://pkg.go.dev/errors) — errors.Is, errors.New, and %w wrapping used throughout
