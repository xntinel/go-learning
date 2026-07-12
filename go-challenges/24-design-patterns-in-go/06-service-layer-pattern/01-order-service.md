# Exercise 1: The Order Service

A service layer earns its place the moment a single business action has to touch more than one collaborator. This module builds an `OrderService` whose `PlaceOrder` validates the request, resolves the buyer, reserves every item, charges an external payment API, persists the order, and sends a confirmation — and, crucially, undoes the inventory it reserved if any later step fails. It is the canonical service-layer shape: orchestration plus compensation, depending only on narrow interfaces it declares itself.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
service.go           User, OrderItem, Order, PlaceOrderRequest, the five
                     collaborator interfaces, OrderService.PlaceOrder + releaseAll
cmd/
  demo/
    main.go          run a happy order, a declined payment, an out-of-stock order
service_test.go      pin the pipeline, the validators, and both rollback paths
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `NewOrderService` (rejects a nil dependency) and `(*OrderService).PlaceOrder`, with `releaseAll` as the compensating action and `Orders`/`Inventory`/`Payments` accessors.
- Test: `service_test.go` covers the happy path, the total computation, empty-order and bad-quantity validation, payment-failure and stock-failure rollback, an unknown user, fire-and-forget notification, and the nil-dependency guard.
- Verify: `go test -race ./...`

### Why orchestration belongs here, and how compensation works

`PlaceOrder` is a use case, and a use case is a sequence of steps that each delegate to a collaborator. The service stores nothing; it holds five interfaces — `OrderRepository`, `InventoryRepository`, `UserRepository`, `PaymentProcessor`, `Notifier` — and sequences calls against them. Read the method top to bottom and it states the business rule in the domain's own words: reject an empty or malformed request, resolve the user, reserve each item against available stock, total the line items, charge the card, allocate an id, save the order, and notify. Every line names a verb the business cares about; not one names a SQL statement or an HTTP endpoint. That is the property the service layer exists to preserve.

The hard part is failure. Reserving inventory and charging a card cannot share a database transaction, because the charge is an external API call. So the service compensates: each successful `Reserve` is appended to a `reserved` slice, and the moment any later step returns an error — a stock check, the charge, the id allocation, the save — the service calls `releaseAll`, which walks that slice in reverse and `Release`s every reservation. Reverse order is the general rule for undo, because a later step may depend on an earlier one; for independent reservations it is simply the disciplined default. Tracking *every* reservation rather than only the last is what prevents a slow stock leak: an order that reserved three products and then failed must release all three, not just the third.

Two design details carry their weight. First, `reservation` is a package-level named type, not an anonymous struct declared inside `PlaceOrder`. The compensating method `releaseAll` takes a `[]reservation` parameter, and a method signature cannot name a type that was declared inside another function — so the type has to live at package scope. (Declaring `type reserved struct{...}` inside `PlaceOrder` and then trying to pass `[]reserved` to a method expecting `[]struct{...}` is a genuine compile error, because a defined type and an unnamed struct type are not identical.) Second, the notification is fire-and-forget: by the time `Send` runs, the order is already charged and saved, so a failure there is logged with `fmt.Printf` and swallowed, never returned. Returning it would cancel a paid order over a transient mail outage.

Errors are sentinels wrapped with `%w`. `ErrInsufficientStock`, `ErrPaymentDeclined`, `ErrInvalidQuantity`, and the rest are package-level values; every failure path wraps the right one with context, so a caller writes `errors.Is(err, service.ErrPaymentDeclined)` and never matches on a string. `NewOrderService` rejects a nil collaborator up front, turning a misconfiguration into a clear startup error instead of a nil-pointer panic mid-checkout.

Create `service.go`:

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// User is the buyer placing an order. The service only needs the email to
// charge and notify; the rest of a real user record is irrelevant here.
type User struct {
	ID    string
	Email string
}

// OrderItem is one line of an order: a product, how many, and the unit price.
type OrderItem struct {
	ProductID string
	Quantity  int
	UnitPrice float64
}

// Order is the artifact the service produces once every step has succeeded.
type Order struct {
	ID        string
	UserID    string
	Items     []OrderItem
	Total     float64
	Status    string
	CreatedAt time.Time
}

// PlaceOrderRequest is the input to the use case. It carries only what a caller
// supplies; the service derives the total, the id, and the timestamp itself.
type PlaceOrderRequest struct {
	UserID string
	Items  []OrderItem
}

// Domain sentinels. Every failure the service reports is one of these, wrapped
// with %w so callers can branch with errors.Is without string matching.
var (
	ErrEmptyOrder        = errors.New("service: order must contain at least one item")
	ErrInvalidQuantity   = errors.New("service: item quantity must be positive")
	ErrInvalidPrice      = errors.New("service: item price must not be negative")
	ErrUnknownUser       = errors.New("service: unknown user")
	ErrInsufficientStock = errors.New("service: insufficient stock")
	ErrPaymentDeclined   = errors.New("service: payment declined")
)

// OrderRepository persists orders and hands out ids. The service declares this
// narrow interface itself; it does not import a database package.
type OrderRepository interface {
	Save(ctx context.Context, order *Order) error
	NextID(ctx context.Context) (string, error)
}

// InventoryRepository is the stock ledger. Reserve and Release are inverses:
// Release is the compensating action that undoes a Reserve.
type InventoryRepository interface {
	GetStock(ctx context.Context, productID string) (int, error)
	Reserve(ctx context.Context, productID string, quantity int) error
	Release(ctx context.Context, productID string, quantity int) error
}

// UserRepository resolves a user id to a user record.
type UserRepository interface {
	GetByID(ctx context.Context, id string) (*User, error)
}

// PaymentProcessor is the external charge API. It cannot join a database
// transaction, which is why the service compensates rather than rolls back.
type PaymentProcessor interface {
	Charge(ctx context.Context, email string, amount float64) (txnID string, err error)
}

// Notifier delivers the confirmation. A failure here is non-critical: the order
// is already charged and saved, so the service logs and moves on.
type Notifier interface {
	Send(ctx context.Context, to, subject, body string) error
}

// reservation records one successful Reserve so PlaceOrder can undo it on a
// later failure. A package-level named type lets the compensating method take a
// slice of it; an anonymous struct type declared inside PlaceOrder could not be
// named in a method signature.
type reservation struct {
	productID string
	quantity  int
}

// OrderService orchestrates the order pipeline. It owns no data of its own; it
// sequences calls to the injected repositories and external processors.
type OrderService struct {
	orders    OrderRepository
	inventory InventoryRepository
	users     UserRepository
	payments  PaymentProcessor
	notifier  Notifier
}

// NewOrderService wires the dependencies and rejects a nil one up front, so a
// misconfigured service fails at construction rather than mid-order.
func NewOrderService(
	orders OrderRepository,
	inventory InventoryRepository,
	users UserRepository,
	payments PaymentProcessor,
	notifier Notifier,
) (*OrderService, error) {
	if orders == nil || inventory == nil || users == nil || payments == nil || notifier == nil {
		return nil, errors.New("service: all dependencies are required")
	}
	return &OrderService{
		orders:    orders,
		inventory: inventory,
		users:     users,
		payments:  payments,
		notifier:  notifier,
	}, nil
}

// PlaceOrder runs the whole use case: validate, resolve the user, reserve every
// item, charge, persist, and notify. Any failure after the first reservation
// releases everything reserved so far before returning.
func (s *OrderService) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error) {
	if len(req.Items) == 0 {
		return nil, ErrEmptyOrder
	}
	for _, it := range req.Items {
		if it.Quantity <= 0 {
			return nil, fmt.Errorf("%w: %s", ErrInvalidQuantity, it.ProductID)
		}
		if it.UnitPrice < 0 {
			return nil, fmt.Errorf("%w: %s", ErrInvalidPrice, it.ProductID)
		}
	}

	user, err := s.users.GetByID(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownUser, req.UserID)
	}

	var reserved []reservation
	for _, it := range req.Items {
		stock, err := s.inventory.GetStock(ctx, it.ProductID)
		if err != nil {
			s.releaseAll(ctx, reserved)
			return nil, fmt.Errorf("stock check %s: %w", it.ProductID, err)
		}
		if stock < it.Quantity {
			s.releaseAll(ctx, reserved)
			return nil, fmt.Errorf("%w: %s have=%d need=%d", ErrInsufficientStock, it.ProductID, stock, it.Quantity)
		}
		if err := s.inventory.Reserve(ctx, it.ProductID, it.Quantity); err != nil {
			s.releaseAll(ctx, reserved)
			return nil, fmt.Errorf("reserve %s: %w", it.ProductID, err)
		}
		reserved = append(reserved, reservation{productID: it.ProductID, quantity: it.Quantity})
	}

	var total float64
	for _, it := range req.Items {
		total += it.UnitPrice * float64(it.Quantity)
	}

	txnID, err := s.payments.Charge(ctx, user.Email, total)
	if err != nil {
		s.releaseAll(ctx, reserved)
		return nil, fmt.Errorf("%w: %v", ErrPaymentDeclined, err)
	}

	orderID, err := s.orders.NextID(ctx)
	if err != nil {
		s.releaseAll(ctx, reserved)
		return nil, fmt.Errorf("next id: %w", err)
	}
	order := &Order{
		ID:        orderID,
		UserID:    req.UserID,
		Items:     req.Items,
		Total:     total,
		Status:    "confirmed",
		CreatedAt: time.Now(),
	}
	if err := s.orders.Save(ctx, order); err != nil {
		s.releaseAll(ctx, reserved)
		return nil, fmt.Errorf("save order: %w", err)
	}

	body := fmt.Sprintf("Your order %s for $%.2f is confirmed (txn=%s).", order.ID, total, txnID)
	if err := s.notifier.Send(ctx, user.Email, "Order Confirmed", body); err != nil {
		fmt.Printf("warning: notification to %s failed: %v\n", user.Email, err)
	}

	return order, nil
}

// releaseAll runs the compensating action for every reservation, in reverse
// order. A failed Release is logged, not returned: the caller already has a
// primary error and the operator needs to know an undo did not complete.
func (s *OrderService) releaseAll(ctx context.Context, reserved []reservation) {
	for i := len(reserved) - 1; i >= 0; i-- {
		r := reserved[i]
		if err := s.inventory.Release(ctx, r.productID, r.quantity); err != nil {
			fmt.Printf("warning: compensating release of %d x %s failed: %v\n", r.quantity, r.productID, err)
		}
	}
}

// Orders, Inventory, and Payments expose the wired dependencies so tests and
// the demo can assert on what was injected while the fields stay unexported.
func (s *OrderService) Orders() OrderRepository        { return s.orders }
func (s *OrderService) Inventory() InventoryRepository { return s.inventory }
func (s *OrderService) Payments() PaymentProcessor     { return s.payments }
```

`PlaceOrder` reads as the recipe; `releaseAll` is the entire undo mechanism. Note that the accessors return the interface values, not the concrete types, so a test can compare identity against what it injected while the fields stay unexported.

### The runnable demo

The demo wires three hand-written fakes and runs the pipeline through its three interesting outcomes: a successful order that decrements stock and notifies, a declined payment that releases everything reserved and leaves stock exactly as it was, and an out-of-stock order whose first item reserved cleanly before the second item failed the stock check — proving the mid-pipeline rollback. The fake inventory prints each release so the compensation is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	service "example.com/order-service"
)

type demoOrders struct{ seq int }

func (d *demoOrders) Save(_ context.Context, _ *service.Order) error { return nil }
func (d *demoOrders) NextID(_ context.Context) (string, error) {
	d.seq++
	return fmt.Sprintf("ORD-%03d", d.seq), nil
}

type demoInventory struct{ stock map[string]int }

func (d *demoInventory) GetStock(_ context.Context, p string) (int, error) { return d.stock[p], nil }
func (d *demoInventory) Reserve(_ context.Context, p string, q int) error {
	d.stock[p] -= q
	return nil
}
func (d *demoInventory) Release(_ context.Context, p string, q int) error {
	d.stock[p] += q
	fmt.Printf("  compensate: released %d x %s\n", q, p)
	return nil
}

type demoUsers struct{}

func (demoUsers) GetByID(_ context.Context, id string) (*service.User, error) {
	return &service.User{ID: id, Email: "alice@example.com"}, nil
}

type demoPayments struct{ fail bool }

func (d demoPayments) Charge(_ context.Context, _ string, amount float64) (string, error) {
	if d.fail {
		return "", errors.New("card declined")
	}
	return fmt.Sprintf("txn-%d", int(amount*100)), nil
}

type demoNotifier struct{}

func (demoNotifier) Send(_ context.Context, to, subject, _ string) error {
	fmt.Printf("  notified %s [%s]\n", to, subject)
	return nil
}

func main() {
	ctx := context.Background()
	inv := &demoInventory{stock: map[string]int{"Widget": 10, "Gadget": 5}}

	fmt.Println("=== successful order ===")
	svc, err := service.NewOrderService(&demoOrders{}, inv, demoUsers{}, demoPayments{}, demoNotifier{})
	if err != nil {
		log.Fatal(err)
	}
	order, err := svc.PlaceOrder(ctx, service.PlaceOrderRequest{
		UserID: "u1",
		Items: []service.OrderItem{
			{ProductID: "Widget", Quantity: 3, UnitPrice: 49.99},
			{ProductID: "Gadget", Quantity: 1, UnitPrice: 19.99},
		},
	})
	if err != nil {
		log.Fatalf("unexpected: %v", err)
	}
	fmt.Printf("  %s status=%s total=$%.2f\n", order.ID, order.Status, order.Total)
	fmt.Printf("  stock now Widget=%d Gadget=%d\n", inv.stock["Widget"], inv.stock["Gadget"])

	fmt.Println("=== payment failure (full rollback) ===")
	svc2, _ := service.NewOrderService(&demoOrders{}, inv, demoUsers{}, demoPayments{fail: true}, demoNotifier{})
	_, err = svc2.PlaceOrder(ctx, service.PlaceOrderRequest{
		UserID: "u1",
		Items: []service.OrderItem{
			{ProductID: "Widget", Quantity: 2, UnitPrice: 49.99},
			{ProductID: "Gadget", Quantity: 1, UnitPrice: 19.99},
		},
	})
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  payment declined? %v\n", errors.Is(err, service.ErrPaymentDeclined))
	fmt.Printf("  stock restored Widget=%d Gadget=%d\n", inv.stock["Widget"], inv.stock["Gadget"])

	fmt.Println("=== insufficient stock (mid-pipeline rollback) ===")
	_, err = svc.PlaceOrder(ctx, service.PlaceOrderRequest{
		UserID: "u1",
		Items: []service.OrderItem{
			{ProductID: "Gadget", Quantity: 1, UnitPrice: 19.99},
			{ProductID: "Widget", Quantity: 99, UnitPrice: 49.99},
		},
	})
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  insufficient stock? %v\n", errors.Is(err, service.ErrInsufficientStock))
	fmt.Printf("  stock restored Widget=%d Gadget=%d\n", inv.stock["Widget"], inv.stock["Gadget"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== successful order ===
  notified alice@example.com [Order Confirmed]
  ORD-001 status=confirmed total=$169.96
  stock now Widget=7 Gadget=4
=== payment failure (full rollback) ===
  compensate: released 1 x Gadget
  compensate: released 2 x Widget
  error: service: payment declined: card declined
  payment declined? true
  stock restored Widget=7 Gadget=4
=== insufficient stock (mid-pipeline rollback) ===
  compensate: released 1 x Gadget
  error: service: insufficient stock: Widget have=7 need=99
  insufficient stock? true
  stock restored Widget=7 Gadget=4
```

The stock figures are the proof: after the successful order Widget is 7 and Gadget is 4, and both failure scenarios restore those exact numbers, because every reservation they made was released.

### Tests

The tests pin every branch. `TestPlaceOrder_HappyPath` runs the full pipeline and checks the total, the saved order, the notification, and the decremented stock. `TestPlaceOrder_TotalMatchesSum` proves the total is the sum of `UnitPrice * Quantity` over the items. The two validator tests pin `ErrEmptyOrder` and `ErrInvalidQuantity`. `TestPlaceOrder_RollsBackInventoryOnPaymentFailure` and `TestPlaceOrder_RollsBackPartialReservationOnStockFailure` prove compensation runs on both a late (payment) and a mid-pipeline (stock) failure. `TestPlaceOrder_RejectsUnknownUser` proves the service stops before touching inventory when the user does not resolve. `TestPlaceOrder_NotificationFailureDoesNotFail` pins the fire-and-forget semantics, and `TestAccessorsExposeWiring` confirms the accessors return exactly what was injected.

Create `service_test.go`:

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type mockOrders struct {
	mu    sync.Mutex
	saved []*Order
	seq   int
}

func (m *mockOrders) Save(_ context.Context, o *Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, o)
	return nil
}

func (m *mockOrders) NextID(_ context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("ORD-%d", m.seq), nil
}

type mockInventory struct {
	mu    sync.Mutex
	stock map[string]int
}

func newMockInventory(stocks map[string]int) *mockInventory {
	return &mockInventory{stock: stocks}
}

func (m *mockInventory) GetStock(_ context.Context, p string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stock[p], nil
}

func (m *mockInventory) Reserve(_ context.Context, p string, q int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stock[p] < q {
		return errors.New("not enough")
	}
	m.stock[p] -= q
	return nil
}

func (m *mockInventory) Release(_ context.Context, p string, q int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stock[p] += q
	return nil
}

type mockUsers struct{ user *User }

func (m mockUsers) GetByID(_ context.Context, _ string) (*User, error) {
	if m.user == nil {
		return nil, errors.New("no such user")
	}
	return m.user, nil
}

type mockPayments struct {
	fail bool
	mu   sync.Mutex
	amt  []float64
}

func (m *mockPayments) Charge(_ context.Context, _ string, amount float64) (string, error) {
	if m.fail {
		return "", errors.New("card declined")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.amt = append(m.amt, amount)
	return "txn-1", nil
}

type mockNotifier struct {
	mu   sync.Mutex
	sent []string
	fail bool
}

func (m *mockNotifier) Send(_ context.Context, to, _, _ string) error {
	if m.fail {
		return errors.New("smtp down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to)
	return nil
}

func newService(t *testing.T, inv *mockInventory, pay *mockPayments) (*OrderService, *mockOrders, *mockNotifier) {
	t.Helper()
	orders := &mockOrders{}
	notifier := &mockNotifier{}
	user := &User{ID: "u1", Email: "alice@example.com"}
	svc, err := NewOrderService(orders, inv, mockUsers{user: user}, pay, notifier)
	if err != nil {
		t.Fatalf("NewOrderService: %v", err)
	}
	return svc, orders, notifier
}

func TestPlaceOrder_HappyPath(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"p1": 10})
	svc, orders, notifier := newService(t, inv, &mockPayments{})

	order, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "u1",
		Items:  []OrderItem{{ProductID: "p1", Quantity: 3, UnitPrice: 10}},
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if order.Total != 30 {
		t.Errorf("Total = %v, want 30", order.Total)
	}
	if order.Status != "confirmed" {
		t.Errorf("Status = %q, want confirmed", order.Status)
	}
	if len(orders.saved) != 1 {
		t.Errorf("saved = %d, want 1", len(orders.saved))
	}
	if len(notifier.sent) != 1 {
		t.Errorf("notified = %d, want 1", len(notifier.sent))
	}
	if inv.stock["p1"] != 7 {
		t.Errorf("stock after = %d, want 7", inv.stock["p1"])
	}
}

func TestPlaceOrder_TotalMatchesSum(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"a": 100, "b": 100, "c": 100})
	svc, _, _ := newService(t, inv, &mockPayments{})

	order, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "u1",
		Items: []OrderItem{
			{ProductID: "a", Quantity: 4, UnitPrice: 2.5},
			{ProductID: "b", Quantity: 2, UnitPrice: 3},
			{ProductID: "c", Quantity: 10, UnitPrice: 0.5},
		},
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if order.Total != 21 {
		t.Errorf("Total = %v, want 21", order.Total)
	}
}

func TestPlaceOrder_RejectsEmptyOrder(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"p1": 10})
	svc, _, _ := newService(t, inv, &mockPayments{})

	_, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{UserID: "u1"})
	if !errors.Is(err, ErrEmptyOrder) {
		t.Errorf("err = %v, want ErrEmptyOrder", err)
	}
}

func TestPlaceOrder_RejectsBadQuantity(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"p1": 10})
	svc, _, _ := newService(t, inv, &mockPayments{})

	_, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "u1",
		Items:  []OrderItem{{ProductID: "p1", Quantity: 0, UnitPrice: 1}},
	})
	if !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("err = %v, want ErrInvalidQuantity", err)
	}
}

func TestPlaceOrder_RollsBackInventoryOnPaymentFailure(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"p1": 10})
	svc, orders, _ := newService(t, inv, &mockPayments{fail: true})

	_, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "u1",
		Items:  []OrderItem{{ProductID: "p1", Quantity: 4, UnitPrice: 5}},
	})
	if !errors.Is(err, ErrPaymentDeclined) {
		t.Fatalf("err = %v, want ErrPaymentDeclined", err)
	}
	if inv.stock["p1"] != 10 {
		t.Errorf("stock after rollback = %d, want 10 (compensated)", inv.stock["p1"])
	}
	if len(orders.saved) != 0 {
		t.Errorf("orders saved = %d, want 0", len(orders.saved))
	}
}

func TestPlaceOrder_RollsBackPartialReservationOnStockFailure(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"p1": 10, "p2": 1})
	svc, orders, _ := newService(t, inv, &mockPayments{})

	_, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "u1",
		Items: []OrderItem{
			{ProductID: "p1", Quantity: 2, UnitPrice: 5},
			{ProductID: "p2", Quantity: 5, UnitPrice: 5},
		},
	})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	if inv.stock["p1"] != 10 {
		t.Errorf("p1 stock after rollback = %d, want 10", inv.stock["p1"])
	}
	if inv.stock["p2"] != 1 {
		t.Errorf("p2 stock after rollback = %d, want 1", inv.stock["p2"])
	}
	if len(orders.saved) != 0 {
		t.Errorf("orders saved = %d, want 0", len(orders.saved))
	}
}

func TestPlaceOrder_NotificationFailureDoesNotFail(t *testing.T) {
	t.Parallel()

	inv := newMockInventory(map[string]int{"p1": 10})
	orders := &mockOrders{}
	notifier := &mockNotifier{fail: true}
	user := &User{ID: "u1", Email: "a@x.com"}
	svc, err := NewOrderService(orders, inv, mockUsers{user: user}, &mockPayments{}, notifier)
	if err != nil {
		t.Fatalf("NewOrderService: %v", err)
	}

	order, err := svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "u1",
		Items:  []OrderItem{{ProductID: "p1", Quantity: 1, UnitPrice: 1}},
	})
	if err != nil {
		t.Fatalf("PlaceOrder should succeed despite notification error, got %v", err)
	}
	if len(orders.saved) != 1 {
		t.Errorf("order should still be saved: got %d", len(orders.saved))
	}
	if order.Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", order.Status)
	}
}

func TestPlaceOrder_RejectsUnknownUser(t *testing.T) {
	t.Parallel()

	orders := &mockOrders{}
	inv := newMockInventory(map[string]int{"p1": 10})
	svc, err := NewOrderService(orders, inv, mockUsers{user: nil}, &mockPayments{}, &mockNotifier{})
	if err != nil {
		t.Fatalf("NewOrderService: %v", err)
	}
	_, err = svc.PlaceOrder(context.Background(), PlaceOrderRequest{
		UserID: "ghost",
		Items:  []OrderItem{{ProductID: "p1", Quantity: 1, UnitPrice: 1}},
	})
	if !errors.Is(err, ErrUnknownUser) {
		t.Errorf("err = %v, want ErrUnknownUser", err)
	}
	if inv.stock["p1"] != 10 {
		t.Errorf("no inventory should be touched: stock = %d, want 10", inv.stock["p1"])
	}
}

func TestNewOrderService_RejectsNilDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewOrderService(nil, newMockInventory(nil), mockUsers{}, &mockPayments{}, &mockNotifier{}); err == nil {
		t.Error("expected error for nil orders")
	}
}

func TestAccessorsExposeWiring(t *testing.T) {
	t.Parallel()

	orders := &mockOrders{}
	inv := newMockInventory(nil)
	pay := &mockPayments{}
	svc, err := NewOrderService(orders, inv, mockUsers{}, pay, &mockNotifier{})
	if err != nil {
		t.Fatalf("NewOrderService: %v", err)
	}
	if svc.Orders() != orders {
		t.Error("Orders() did not return the injected repository")
	}
	if svc.Inventory() != inv {
		t.Error("Inventory() did not return the injected repository")
	}
	if svc.Payments() != pay {
		t.Error("Payments() did not return the injected processor")
	}
}
```

## Review

The service is correct when `PlaceOrder` reads as a domain recipe and every failure path either validates before doing work or compensates the work already done. Confirm the compensation slice tracks every reservation and releases them in reverse: the payment-failure and stock-failure tests both assert that stock returns to its starting value, which only holds if no reservation is stranded. Confirm the notification path logs and swallows its error rather than returning it — the test that injects a failing notifier still expects a saved, confirmed order. Confirm `NewOrderService` rejects a nil dependency, because that is the cheap place to catch a misconfiguration.

The common mistakes mirror the concepts. Treating the service like a repository (giving it a database handle) makes it untestable; the narrow interfaces keep it pure. Wrapping the external `Charge` in a database transaction is the bug compensation exists to avoid. Compensating only the last reserved item leaks stock on every failed order, which is why `releaseAll` walks the whole slice. And the most damaging mistake — returning a notification error as the order's error — cancels paid orders over a mail outage; the fire-and-forget test exists specifically to forbid it. One subtle compile-time trap worth internalizing: the reservation type must be package-level so the compensating method can name it in its signature.

## Resources

- [Martin Fowler: Service Layer](https://martinfowler.com/eaaCatalog/serviceLayer.html) — the original catalog entry defining the layer that "establishes a set of available operations and coordinates the application's response in each operation."
- [Compensating Transaction pattern (Microsoft Azure Architecture Center)](https://learn.microsoft.com/en-us/azure/architecture/patterns/compensating-transaction) — why and how to undo completed steps, in reverse, when a later step in a multi-step operation fails.
- [Error handling and Go](https://go.dev/blog/error-handling-and-go) — the standard-library error model the sentinel-plus-`%w` approach in this service is built on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-transactional-unit-of-work.md](02-transactional-unit-of-work.md)
