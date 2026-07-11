# 19. CQRS and Eventual Consistency

CQRS (Command Query Responsibility Segregation) separates the write model from
the read model. Commands mutate state and emit events; queries read from
materialized projections that consume those events asynchronously. The hard part
is not the separation itself—it is the consistency gap: after a command
completes, projections are temporarily stale. Callers that immediately query may
read data that does not reflect their write. This lesson builds a self-contained,
in-process CQRS system that includes causal consistency tokens so readers can
wait until their projection has caught up to a known event version.

```text
cqrs/
  go.mod
  cqrs.go          -- EventStore, CommandBus, Projection, CausalToken
  cqrs_test.go     -- table-driven tests with t.Parallel(), errors.Is
  cmd/demo/main.go -- runnable demo: place-order / list-orders
```

## Concepts

### The Write Side: Commands and Events

A command expresses intent: `PlaceOrder`, `CancelOrder`. Command handlers
validate the intent against current aggregate state and, if valid, produce one or
more immutable domain events (`OrderPlaced`, `OrderCancelled`). Events are stored
in an append-only event store and assigned a monotonically increasing global
sequence number (version). That version is the causal token returned to the
caller.

The event store uses optimistic concurrency: the caller supplies the last version
it observed for the aggregate. If the stored version has advanced, the append is
rejected with `ErrVersionConflict`. This prevents lost updates without a global
lock.

### The Read Side: Projections

A projection subscribes to the event stream and builds a materialized view
optimized for a specific query pattern. Because the subscription is asynchronous
(events are delivered on a goroutine via a buffered channel), a projection's
applied version always lags behind the write side by some non-zero amount. That
lag is the source of eventual consistency.

Three projection types illustrate distinct access patterns:

- Summary projection: a single scalar per aggregate (e.g., order count).
- Detail projection: per-ID keyed view (e.g., full order record).
- Aggregate projection: grouped rollup (e.g., orders per customer).

### Causal Consistency Tokens

After a command, the write side returns the global event version at which the
command's effects were recorded. The caller passes this token to the read side
via `WaitForVersion`. The projection blocks until its applied version reaches the
token—or until a deadline fires—before returning the query result. This
implements "read-your-writes" without coupling the write and read sides into a
single transaction.

The wait is bounded: a timeout prevents indefinitely blocking callers when the
projection worker stalls.

### Projection Rebuilding

A projection can be rebuilt from scratch by replaying all events from version 0.
Rebuilding supports schema evolution: change the projection logic, replay all
events, swap the rebuilt view atomically. This is safe because events are
immutable; replaying them always produces the same deterministic state.

### Failure Modes

- **Stale reads without causal tokens:** the caller queries immediately after a
  command and reads pre-command data. Fix: use the causal token from the command
  response and call `WaitForVersion`.
- **Lost updates:** two goroutines append to the same aggregate concurrently
  without checking the expected version. Fix: use `ErrVersionConflict` and retry.
- **Projection drift:** a projection crashes mid-replay and resumes from a
  checkpoint that is behind the crash point, replaying some events twice. Fix:
  make every projection handler idempotent (applying the same event twice produces
  the same state as applying it once).
- **Head-of-line blocking:** a single slow projection holds up a shared worker.
  Fix: give each projection its own goroutine and its own buffered event channel.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/cqrs/cmd/demo
cd ~/go-exercises/cqrs
go mod init example.com/cqrs
```

This is a library plus a demo binary. Verify with `go test`.

### Exercise 1: Event Store and Domain Types

Create `cqrs.go`:

```go
package cqrs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors.
var (
	ErrVersionConflict  = errors.New("version conflict: aggregate was modified concurrently")
	ErrProjectionClosed = errors.New("projection is closed")
	ErrWaitTimeout      = errors.New("timed out waiting for projection to catch up")
	ErrOrderNotFound    = errors.New("order not found")
)

// EventType is the discriminator for domain events.
type EventType string

const (
	EventOrderPlaced    EventType = "OrderPlaced"
	EventOrderCancelled EventType = "OrderCancelled"
	EventOrderShipped   EventType = "OrderShipped"
)

// Event is an immutable record of something that happened.
type Event struct {
	// GlobalVersion is the position of this event in the global event log.
	// It is assigned by the EventStore on Append and is monotonically increasing.
	GlobalVersion int64
	// AggregateID identifies the entity (e.g., order ID).
	AggregateID string
	// AggregateVersion is the position of this event within the aggregate's log.
	AggregateVersion int64
	Type             EventType
	OccurredAt       time.Time
	Payload          any
}

// OrderPlacedPayload is the payload for EventOrderPlaced.
type OrderPlacedPayload struct {
	CustomerID string
	Amount     int64 // cents
}

// OrderCancelledPayload is the payload for EventOrderCancelled.
type OrderCancelledPayload struct {
	Reason string
}

// OrderShippedPayload is the payload for EventOrderShipped.
type OrderShippedPayload struct {
	TrackingNumber string
}

// EventStore is an in-memory, append-only event log.
type EventStore struct {
	mu     sync.RWMutex
	global []Event          // all events in insertion order
	byAgg  map[string][]int // aggregateID -> indices into global
	nextGV int64            // next global version
	subs   []chan<- Event   // active subscribers
}

// NewEventStore returns an empty EventStore.
func NewEventStore() *EventStore {
	return &EventStore{
		byAgg:  make(map[string][]int),
		nextGV: 1,
	}
}

// Subscribe returns a channel that receives every event appended after the call.
// The caller must drain the channel; the buffer holds up to 256 events.
func (s *EventStore) Subscribe() <-chan Event {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	return ch
}

// Append stores events for aggregateID. expectedVersion is the aggregate version
// the caller last observed; if the stored version differs, ErrVersionConflict is
// returned. Pass expectedVersion == 0 for a new aggregate.
func (s *EventStore) Append(aggregateID string, expectedVersion int64, events []Event) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.byAgg[aggregateID]
	currentVersion := int64(len(existing))
	if currentVersion != expectedVersion {
		return 0, fmt.Errorf("%w: aggregate %q at version %d, expected %d",
			ErrVersionConflict, aggregateID, currentVersion, expectedVersion)
	}

	var lastGV int64
	for i, e := range events {
		e.GlobalVersion = s.nextGV
		e.AggregateID = aggregateID
		e.AggregateVersion = currentVersion + int64(i) + 1
		e.OccurredAt = time.Now().UTC()
		idx := len(s.global)
		s.global = append(s.global, e)
		s.byAgg[aggregateID] = append(s.byAgg[aggregateID], idx)
		lastGV = e.GlobalVersion
		s.nextGV++
		// Fan out to subscribers without blocking: drop if the buffer is full.
		for _, ch := range s.subs {
			select {
			case ch <- e:
			default:
			}
		}
	}
	return lastGV, nil
}

// Load returns events for aggregateID starting at fromVersion (1-based).
func (s *EventStore) Load(aggregateID string, fromVersion int64) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	indices := s.byAgg[aggregateID]
	var result []Event
	for _, idx := range indices {
		e := s.global[idx]
		if e.AggregateVersion >= fromVersion {
			result = append(result, e)
		}
	}
	return result, nil
}

// LoadAll returns all events in global order, for projection rebuilding.
func (s *EventStore) LoadAll() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, len(s.global))
	copy(out, s.global)
	return out
}

// GlobalVersion returns the highest global version assigned so far.
func (s *EventStore) GlobalVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextGV - 1
}

// OrderStatus represents the current state of an order aggregate.
type OrderStatus string

const (
	StatusPending   OrderStatus = "pending"
	StatusCancelled OrderStatus = "cancelled"
	StatusShipped   OrderStatus = "shipped"
)

// OrderAggregate is the write-side aggregate for an order.
type OrderAggregate struct {
	ID         string
	Version    int64 // last applied aggregate version
	Status     OrderStatus
	CustomerID string
	Amount     int64
}

// Apply updates aggregate state from a single event (pure, no side effects).
func (o *OrderAggregate) Apply(e Event) {
	o.Version = e.AggregateVersion
	switch e.Type {
	case EventOrderPlaced:
		p := e.Payload.(OrderPlacedPayload)
		o.CustomerID = p.CustomerID
		o.Amount = p.Amount
		o.Status = StatusPending
	case EventOrderCancelled:
		o.Status = StatusCancelled
	case EventOrderShipped:
		o.Status = StatusShipped
	}
}

// Rehydrate reconstructs an OrderAggregate from a slice of events.
func Rehydrate(id string, events []Event) *OrderAggregate {
	o := &OrderAggregate{ID: id}
	for _, e := range events {
		o.Apply(e)
	}
	return o
}

// CommandBus dispatches commands to handlers.
type CommandBus struct {
	store *EventStore
}

// NewCommandBus returns a CommandBus backed by store.
func NewCommandBus(store *EventStore) *CommandBus {
	return &CommandBus{store: store}
}

// PlaceOrder is the command to create a new order.
type PlaceOrder struct {
	OrderID    string
	CustomerID string
	Amount     int64
}

// CancelOrder is the command to cancel an existing order.
type CancelOrder struct {
	OrderID string
	Reason  string
}

// ShipOrder is the command to mark an order as shipped.
type ShipOrder struct {
	OrderID        string
	TrackingNumber string
}

// HandlePlaceOrder processes PlaceOrder and returns the causal token.
func (b *CommandBus) HandlePlaceOrder(cmd PlaceOrder) (int64, error) {
	// New aggregate: expected version is 0.
	events := []Event{
		{Type: EventOrderPlaced, Payload: OrderPlacedPayload{
			CustomerID: cmd.CustomerID,
			Amount:     cmd.Amount,
		}},
	}
	gv, err := b.store.Append(cmd.OrderID, 0, events)
	if err != nil {
		return 0, fmt.Errorf("PlaceOrder: %w", err)
	}
	return gv, nil
}

// HandleCancelOrder processes CancelOrder and returns the causal token.
func (b *CommandBus) HandleCancelOrder(cmd CancelOrder) (int64, error) {
	existing, err := b.store.Load(cmd.OrderID, 1)
	if err != nil {
		return 0, err
	}
	if len(existing) == 0 {
		return 0, fmt.Errorf("CancelOrder: %w", ErrOrderNotFound)
	}
	agg := Rehydrate(cmd.OrderID, existing)
	if agg.Status == StatusCancelled {
		return 0, fmt.Errorf("CancelOrder: order is already cancelled")
	}
	events := []Event{
		{Type: EventOrderCancelled, Payload: OrderCancelledPayload{Reason: cmd.Reason}},
	}
	gv, err := b.store.Append(cmd.OrderID, agg.Version, events)
	if err != nil {
		return 0, fmt.Errorf("CancelOrder: %w", err)
	}
	return gv, nil
}

// HandleShipOrder processes ShipOrder and returns the causal token.
func (b *CommandBus) HandleShipOrder(cmd ShipOrder) (int64, error) {
	existing, err := b.store.Load(cmd.OrderID, 1)
	if err != nil {
		return 0, err
	}
	if len(existing) == 0 {
		return 0, fmt.Errorf("ShipOrder: %w", ErrOrderNotFound)
	}
	agg := Rehydrate(cmd.OrderID, existing)
	if agg.Status != StatusPending {
		return 0, fmt.Errorf("ShipOrder: cannot ship order in status %q", agg.Status)
	}
	events := []Event{
		{Type: EventOrderShipped, Payload: OrderShippedPayload{TrackingNumber: cmd.TrackingNumber}},
	}
	gv, err := b.store.Append(cmd.OrderID, agg.Version, events)
	if err != nil {
		return 0, fmt.Errorf("ShipOrder: %w", err)
	}
	return gv, nil
}

// OrderView is the read-model record for a single order.
type OrderView struct {
	ID             string
	CustomerID     string
	Amount         int64
	Status         OrderStatus
	TrackingNumber string
}

// OrderProjection builds and queries a per-ID order read model.
type OrderProjection struct {
	mu             sync.RWMutex
	orders         map[string]*OrderView
	appliedVersion int64 // highest global version applied
	versionReady   *sync.Cond
	closed         bool
}

// NewOrderProjection returns an empty projection. Call Run to start consuming.
func NewOrderProjection() *OrderProjection {
	p := &OrderProjection{
		orders: make(map[string]*OrderView),
	}
	p.versionReady = sync.NewCond(&p.mu)
	return p
}

// Apply processes a single event into the read model.
func (p *OrderProjection) Apply(e Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch e.Type {
	case EventOrderPlaced:
		pay := e.Payload.(OrderPlacedPayload)
		p.orders[e.AggregateID] = &OrderView{
			ID:         e.AggregateID,
			CustomerID: pay.CustomerID,
			Amount:     pay.Amount,
			Status:     StatusPending,
		}
	case EventOrderCancelled:
		if o, ok := p.orders[e.AggregateID]; ok {
			o.Status = StatusCancelled
		}
	case EventOrderShipped:
		pay := e.Payload.(OrderShippedPayload)
		if o, ok := p.orders[e.AggregateID]; ok {
			o.Status = StatusShipped
			o.TrackingNumber = pay.TrackingNumber
		}
	}
	if e.GlobalVersion > p.appliedVersion {
		p.appliedVersion = e.GlobalVersion
		p.versionReady.Broadcast()
	}
}

// Run drains ch, applying each event to the read model. Returns when ch is closed.
func (p *OrderProjection) Run(ch <-chan Event) {
	for e := range ch {
		p.Apply(e)
	}
	p.mu.Lock()
	p.closed = true
	p.versionReady.Broadcast()
	p.mu.Unlock()
}

// Rebuild replaces the projection state by replaying a slice of events.
// Safe to call while Run is not active (e.g., before starting it, or after stopping).
func (p *OrderProjection) Rebuild(events []Event) {
	p.mu.Lock()
	p.orders = make(map[string]*OrderView)
	p.appliedVersion = 0
	p.mu.Unlock()
	for _, e := range events {
		p.Apply(e)
	}
}

// WaitForVersion blocks until the projection has applied globalVersion or ctx is done.
// Returns ErrWaitTimeout if the context deadline fires first.
func (p *OrderProjection) WaitForVersion(ctx context.Context, globalVersion int64) error {
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		p.mu.Lock()
		p.versionReady.Broadcast()
		p.mu.Unlock()
		close(done)
	}()

	p.mu.Lock()
	defer p.mu.Unlock()
	for p.appliedVersion < globalVersion && !p.closed {
		select {
		case <-done:
			return ErrWaitTimeout
		default:
			p.versionReady.Wait()
		}
	}
	if ctx.Err() != nil && p.appliedVersion < globalVersion {
		return ErrWaitTimeout
	}
	return nil
}

// Get returns a copy of the OrderView for id, or ErrOrderNotFound.
func (p *OrderProjection) Get(id string) (OrderView, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	o, ok := p.orders[id]
	if !ok {
		return OrderView{}, ErrOrderNotFound
	}
	return *o, nil
}

// List returns a snapshot of all OrderViews.
func (p *OrderProjection) List() []OrderView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]OrderView, 0, len(p.orders))
	for _, o := range p.orders {
		out = append(out, *o)
	}
	return out
}

// Count returns the number of orders in the projection.
func (p *OrderProjection) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.orders)
}

// AppliedVersion returns the highest global version applied so far.
func (p *OrderProjection) AppliedVersion() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.appliedVersion
}
```

The `EventStore` assigns global sequence numbers; the `CommandBus` validates
commands and appends events; `OrderProjection` consumes the subscription channel
in `Run` and exposes `WaitForVersion` for causal consistency.

### Exercise 2: Tests

Create `cqrs_test.go`:

```go
package cqrs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ExampleCommandBus_HandlePlaceOrder shows placing an order and reading it
// back through the projection after waiting for the causal token.
func ExampleCommandBus_HandlePlaceOrder() {
	store := NewEventStore()
	bus := NewCommandBus(store)
	proj := NewOrderProjection()

	ch := store.Subscribe()
	go proj.Run(ch)

	gv, err := bus.HandlePlaceOrder(PlaceOrder{
		OrderID:    "ord-1",
		CustomerID: "cust-1",
		Amount:     1999,
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := proj.WaitForVersion(ctx, gv); err != nil {
		fmt.Println("wait error:", err)
		return
	}

	view, _ := proj.Get("ord-1")
	fmt.Printf("order %s status=%s amount=%d\n", view.ID, view.Status, view.Amount)
	// Output:
	// order ord-1 status=pending amount=1999
}

func TestPlaceOrderCreatesProjection(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)
	proj := NewOrderProjection()

	ch := store.Subscribe()
	go proj.Run(ch)

	gv, err := bus.HandlePlaceOrder(PlaceOrder{
		OrderID:    "ord-A",
		CustomerID: "cust-X",
		Amount:     5000,
	})
	if err != nil {
		t.Fatalf("HandlePlaceOrder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := proj.WaitForVersion(ctx, gv); err != nil {
		t.Fatalf("WaitForVersion: %v", err)
	}

	view, err := proj.Get("ord-A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Status != StatusPending {
		t.Errorf("status = %q, want pending", view.Status)
	}
	if view.Amount != 5000 {
		t.Errorf("amount = %d, want 5000", view.Amount)
	}
	if view.CustomerID != "cust-X" {
		t.Errorf("customerID = %q, want cust-X", view.CustomerID)
	}
}

func TestCancelOrderUpdatesProjection(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)
	proj := NewOrderProjection()

	ch := store.Subscribe()
	go proj.Run(ch)

	gv1, err := bus.HandlePlaceOrder(PlaceOrder{OrderID: "ord-B", CustomerID: "cust-Y", Amount: 100})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	if err := proj.WaitForVersion(ctx1, gv1); err != nil {
		t.Fatalf("WaitForVersion after place: %v", err)
	}

	gv2, err := bus.HandleCancelOrder(CancelOrder{OrderID: "ord-B", Reason: "changed mind"})
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := proj.WaitForVersion(ctx2, gv2); err != nil {
		t.Fatalf("WaitForVersion after cancel: %v", err)
	}

	view, err := proj.Get("ord-B")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled", view.Status)
	}
}

func TestShipOrderUpdatesProjection(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)
	proj := NewOrderProjection()

	ch := store.Subscribe()
	go proj.Run(ch)

	gv1, _ := bus.HandlePlaceOrder(PlaceOrder{OrderID: "ord-C", CustomerID: "cust-Z", Amount: 200})
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	if err := proj.WaitForVersion(ctx1, gv1); err != nil {
		t.Fatalf("WaitForVersion after place: %v", err)
	}

	gv2, err := bus.HandleShipOrder(ShipOrder{OrderID: "ord-C", TrackingNumber: "TRK-999"})
	if err != nil {
		t.Fatalf("ShipOrder: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := proj.WaitForVersion(ctx2, gv2); err != nil {
		t.Fatalf("WaitForVersion after ship: %v", err)
	}

	view, _ := proj.Get("ord-C")
	if view.Status != StatusShipped {
		t.Errorf("status = %q, want shipped", view.Status)
	}
	if view.TrackingNumber != "TRK-999" {
		t.Errorf("trackingNumber = %q, want TRK-999", view.TrackingNumber)
	}
}

func TestVersionConflictOnDuplicatePlaceOrder(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)

	if _, err := bus.HandlePlaceOrder(PlaceOrder{OrderID: "ord-D", CustomerID: "c", Amount: 1}); err != nil {
		t.Fatalf("first PlaceOrder: %v", err)
	}
	_, err := bus.HandlePlaceOrder(PlaceOrder{OrderID: "ord-D", CustomerID: "c", Amount: 1})
	if !errors.Is(err, ErrVersionConflict) {
		t.Errorf("err = %v, want ErrVersionConflict", err)
	}
}

func TestCancelAlreadyCancelledOrderErrors(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)

	bus.HandlePlaceOrder(PlaceOrder{OrderID: "ord-E", CustomerID: "c", Amount: 1}) //nolint
	bus.HandleCancelOrder(CancelOrder{OrderID: "ord-E", Reason: "first"})          //nolint
	_, err := bus.HandleCancelOrder(CancelOrder{OrderID: "ord-E", Reason: "second"})
	if err == nil {
		t.Fatal("expected error cancelling an already-cancelled order")
	}
}

func TestShipNonPendingOrderErrors(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)

	bus.HandlePlaceOrder(PlaceOrder{OrderID: "ord-F", CustomerID: "c", Amount: 1}) //nolint
	bus.HandleCancelOrder(CancelOrder{OrderID: "ord-F", Reason: "nope"})           //nolint
	_, err := bus.HandleShipOrder(ShipOrder{OrderID: "ord-F", TrackingNumber: "T"})
	if err == nil {
		t.Fatal("expected error shipping a cancelled order")
	}
}

func TestProjectionRebuild(t *testing.T) {
	t.Parallel()

	store := NewEventStore()
	bus := NewCommandBus(store)

	// Place several orders without a live projection.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("rebuild-ord-%d", i)
		if _, err := bus.HandlePlaceOrder(PlaceOrder{OrderID: id, CustomerID: "c", Amount: int64(i * 100)}); err != nil {
			t.Fatalf("PlaceOrder %d: %v", i, err)
		}
	}

	// Build projection from replay (no subscriber goroutine needed).
	proj := NewOrderProjection()
	proj.Rebuild(store.LoadAll())

	if got := proj.Count(); got != 5 {
		t.Errorf("Count = %d, want 5", got)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("rebuild-ord-%d", i)
		view, err := proj.Get(id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			continue
		}
		if view.Status != StatusPending {
			t.Errorf("%s: status = %q, want pending", id, view.Status)
		}
	}
}

func TestCausalTokenWaitTimeout(t *testing.T) {
	t.Parallel()

	// A projection with no running worker: WaitForVersion must time out.
	proj := NewOrderProjection()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := proj.WaitForVersion(ctx, 999)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Errorf("err = %v, want ErrWaitTimeout", err)
	}
}

func TestTableDrivenPlaceOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cmd        PlaceOrder
		wantStatus OrderStatus
	}{
		{
			name:       "basic order",
			cmd:        PlaceOrder{OrderID: "tbl-1", CustomerID: "c1", Amount: 100},
			wantStatus: StatusPending,
		},
		{
			name:       "zero amount order",
			cmd:        PlaceOrder{OrderID: "tbl-2", CustomerID: "c2", Amount: 0},
			wantStatus: StatusPending,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := NewEventStore()
			bus := NewCommandBus(store)
			proj := NewOrderProjection()

			ch := store.Subscribe()
			go proj.Run(ch)

			gv, err := bus.HandlePlaceOrder(tc.cmd)
			if err != nil {
				t.Fatalf("HandlePlaceOrder: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := proj.WaitForVersion(ctx, gv); err != nil {
				t.Fatalf("WaitForVersion: %v", err)
			}

			view, err := proj.Get(tc.cmd.OrderID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if view.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", view.Status, tc.wantStatus)
			}
		})
	}
}

// Your turn: add TestOrderNotFoundAfterCancel that calls proj.Get on an order ID
// that was never placed and asserts errors.Is(err, ErrOrderNotFound).
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/cqrs"
)

func main() {
	store := cqrs.NewEventStore()
	bus := cqrs.NewCommandBus(store)
	proj := cqrs.NewOrderProjection()

	ch := store.Subscribe()
	go proj.Run(ch)

	// Place three orders.
	orders := []cqrs.PlaceOrder{
		{OrderID: "ord-001", CustomerID: "alice", Amount: 4999},
		{OrderID: "ord-002", CustomerID: "bob", Amount: 1200},
		{OrderID: "ord-003", CustomerID: "alice", Amount: 8750},
	}

	var lastGV int64
	for _, cmd := range orders {
		gv, err := bus.HandlePlaceOrder(cmd)
		if err != nil {
			log.Fatalf("PlaceOrder %s: %v", cmd.OrderID, err)
		}
		lastGV = gv
		fmt.Printf("placed order %s (causal token %d)\n", cmd.OrderID, gv)
	}

	// Ship one order.
	gv, err := bus.HandleShipOrder(cqrs.ShipOrder{
		OrderID:        "ord-001",
		TrackingNumber: "TRK-XYZ",
	})
	if err != nil {
		log.Fatalf("ShipOrder: %v", err)
	}
	lastGV = gv
	fmt.Printf("shipped ord-001 (causal token %d)\n", gv)

	// Cancel another order.
	gv, err = bus.HandleCancelOrder(cqrs.CancelOrder{
		OrderID: "ord-002",
		Reason:  "customer request",
	})
	if err != nil {
		log.Fatalf("CancelOrder: %v", err)
	}
	lastGV = gv
	fmt.Printf("cancelled ord-002 (causal token %d)\n", gv)

	// Wait for the projection to catch up to the last causal token.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := proj.WaitForVersion(ctx, lastGV); err != nil {
		log.Fatalf("projection did not catch up: %v", err)
	}

	// Query the projection.
	fmt.Printf("\nAll orders (%d total):\n", proj.Count())
	for _, v := range proj.List() {
		fmt.Printf("  %s  customer=%s  amount=%d  status=%s  tracking=%s\n",
			v.ID, v.CustomerID, v.Amount, v.Status, v.TrackingNumber)
	}

	// Demonstrate projection rebuilding from the event store.
	rebuilt := cqrs.NewOrderProjection()
	rebuilt.Rebuild(store.LoadAll())
	fmt.Printf("\nRebuilt projection count: %d (applied version: %d)\n",
		rebuilt.Count(), rebuilt.AppliedVersion())
}
```

The demo exercises only exported API. `proj.Count()`, `proj.List()`, and
`proj.AppliedVersion()` are small exported accessors added specifically so the
demo can report state without exporting the raw map.

## Common Mistakes

### Querying the Projection Without Waiting for the Causal Token

Wrong: immediately calling `proj.Get(id)` after `HandlePlaceOrder` returns. The
projection worker is on a separate goroutine; the event may not have been applied
yet, so `Get` returns `ErrOrderNotFound`.

Fix: use the global version returned by the command handler as a causal token and
call `proj.WaitForVersion(ctx, gv)` before querying.

### Omitting the Expected Version in Append

Wrong: always passing `0` as `expectedVersion` to `EventStore.Append` for every
command, including updates. Two concurrent `CancelOrder` commands on the same
order will both succeed, appending duplicate cancellation events.

Fix: load the aggregate, read its current version, and pass that version as
`expectedVersion`. `ErrVersionConflict` signals the caller to reload and retry.

### Making the Projection Handler Non-Idempotent

Wrong: a projection handler that increments a counter on every `EventOrderPlaced`
event. If events are delivered twice (e.g., after a projection restart from a
stale checkpoint), the counter drifts.

Fix: write projection handlers as pure replacements: set the field to the event's
value rather than adding to it. The `OrderProjection.Apply` in this lesson
replaces `orders[id]` on `EventOrderPlaced` rather than incrementing any counter.

### Unbounded WaitForVersion

Wrong: calling `proj.WaitForVersion` with `context.Background()` (no deadline).
If the projection worker stalls or the channel drops an event, the caller blocks
forever.

Fix: always supply a deadline. Use `context.WithTimeout`. If the wait times out,
surface `ErrWaitTimeout` to the caller; do not silently discard it.

### Exporting Raw Fields Instead of Accessors for the Demo

Wrong: making the `orders` map field exported so `cmd/demo` can read it directly.
This breaks encapsulation and forces callers to copy internal implementation
details.

Fix: add small exported accessor methods (`Count`, `List`, `AppliedVersion`) that
return a copy of the data. The demo stays decoupled from the internal map type.

## Verification

From `~/go-exercises/cqrs`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed. The `go test` step is the primary verification; `go run`
is a sanity check for the demo binary. The race detector (`-race`) verifies that
`EventStore` and `OrderProjection` are safe for concurrent use.

## Summary

- CQRS separates the write model (command handlers, aggregate state) from the read model (projections, materialized views).
- The write side uses optimistic concurrency (`expectedVersion`) to prevent lost updates.
- Projections consume an event channel asynchronously; they always lag the write side by some non-zero amount.
- Causal consistency tokens (the global event version returned by a command) let callers block until the projection has caught up.
- Projection rebuilding replays all events from the event store to reconstruct the read model; handlers must be idempotent.
- The projection's goroutine must be started explicitly (`go proj.Run(ch)`); forgetting it means `WaitForVersion` blocks forever.

## What's Next

Next: [Distributed Transaction Coordinator](../20-distributed-transaction-coordinator/20-distributed-transaction-coordinator.md).

## Resources

- [CQRS (Martin Fowler)](https://martinfowler.com/bliki/CQRS.html) — canonical pattern overview
- [Greg Young, CQRS Documents](https://cqrs.files.wordpress.com/2010/11/cqrs_documents.pdf) — foundational document on commands, events, and projections
- [sync package — pkg.go.dev](https://pkg.go.dev/sync) — Mutex, RWMutex, Cond used in EventStore and OrderProjection
- [context package — pkg.go.dev](https://pkg.go.dev/context) — WithTimeout and Done channel used in WaitForVersion
- [Go Memory Model](https://go.dev/ref/mem) — happens-before guarantees relied on by the subscription fan-out and Cond.Broadcast
