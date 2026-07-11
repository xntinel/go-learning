# Exercise 1: Constructor Injection With Consumer Interfaces

The canonical form of dependency injection is the constructor that accepts every
collaborator and stores it behind an unexported field, where each collaborator is
typed as a narrow interface the consumer itself declares. This exercise builds an
`OrderService` that charges a payment processor, persists to a store, and sends a
notification — three dependencies it never constructs, only receives — and proves
with a table of fakes that the pipeline both propagates and short-circuits
correctly.

This module is fully self-contained. It starts with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
service.go           Order, the three consumer interfaces, OrderService,
                     NewOrderService (validates, rejects nil), PlaceOrder,
                     plus InMemoryStore / StubPayment / ConsoleNotifier impls
cmd/
  demo/
    main.go          wire the real impls in the composition root and place an order
service_test.go      local fakes; happy path, payment short-circuit, nil rejection,
                     input validation, accessor wiring
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `NewOrderService(store, payment, notifier)` returning `(*OrderService, error)`, and `PlaceOrder(ctx, *Order) error`.
- Test: round-trip the happy path, assert payment failure stops before save and notify, reject nil dependencies and invalid input.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p constructor-injection/cmd/demo && cd constructor-injection
go mod init example.com/constructor-injection
```

### Why the interfaces live here and the constructor validates

Read the service from its dependencies inward. `OrderService` needs to do three
things to the outside world: charge money, persist an order, and send a message.
It does not need a database's full API, a payment vendor's full SDK, or an SMTP
library's full surface — it needs exactly `Charge`, `Save`/`FindByID`, and `Send`.
So those three interfaces are declared right here, in the consumer package, each
listing only the methods this one service calls. A concrete store in another
package satisfies `OrderStore` simply by having a `Save` and a `FindByID` method;
it never imports this package and never announces that it implements anything.
That is Go's implicit-interface rule doing the decoupling for free: the dependency
arrow points from the consumer's small interface to whatever concrete type the
caller chooses, and the concrete type is unaware of the contract it fulfills.

The constructor takes all three as interfaces and validates each one. A `nil`
passed by mistake is a programming error, and the constructor turns it into an
immediate, located failure — `return nil, errors.New("...")` — rather than storing
the `nil` and letting `PlaceOrder` panic later on a dereference, far from the
actual mistake. Because `NewOrderService` returns `(*OrderService, error)`, the
caller is forced to handle the construction failure, and a test can assert each of
the three rejections precisely.

`PlaceOrder` is a pipeline with a deliberate order and a deliberate failure
policy. It validates input first (an empty ID or email is rejected with a sentinel
error the caller can match with `errors.Is`), then charges, then saves, then
notifies. Each step that fails wraps its cause with `%w` so the wrap chain is
inspectable, and — critically — a failure short-circuits the rest: if `Charge`
returns an error, the order is never saved and no notification is sent. That
short-circuit is a correctness property worth testing directly, because the whole
point of running the charge first is to avoid persisting or announcing an order
that was never paid for.

Create `service.go`:

```go
package di

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Order is the aggregate the service operates on. PlaceOrder mutates Status,
// TxnID, and CreatedAt in place, so a caller holding the pointer observes the
// same values the store saw.
type Order struct {
	ID        string
	UserEmail string
	Items     []string
	Total     float64
	Status    string
	TxnID     string
	CreatedAt time.Time
}

// Sentinel errors let callers match specific failures with errors.Is.
var (
	ErrEmptyOrderID    = errors.New("di: order ID is required")
	ErrEmptyUserEmail  = errors.New("di: user email is required")
	ErrPaymentDeclined = errors.New("di: payment declined")
)

// OrderStore is the persistence seam, declared by the consumer with only the
// two methods OrderService actually uses.
type OrderStore interface {
	Save(ctx context.Context, order *Order) error
	FindByID(ctx context.Context, id string) (*Order, error)
}

// PaymentProcessor is the money seam: one method, returning a transaction ID.
type PaymentProcessor interface {
	Charge(ctx context.Context, email string, amount float64) (transactionID string, err error)
}

// Notifier is the messaging seam.
type Notifier interface {
	Send(ctx context.Context, to, subject, body string) error
}

// OrderService holds its dependencies behind unexported fields. The constructor
// is the only way to populate them, so an OrderService is always fully wired.
type OrderService struct {
	store    OrderStore
	payment  PaymentProcessor
	notifier Notifier
}

// NewOrderService injects the three dependencies and rejects any nil, turning a
// wiring mistake into an immediate, located error instead of a later panic.
func NewOrderService(store OrderStore, payment PaymentProcessor, notifier Notifier) (*OrderService, error) {
	if store == nil {
		return nil, errors.New("di: store is required")
	}
	if payment == nil {
		return nil, errors.New("di: payment is required")
	}
	if notifier == nil {
		return nil, errors.New("di: notifier is required")
	}
	return &OrderService{store: store, payment: payment, notifier: notifier}, nil
}

// PlaceOrder runs the pipeline: validate, charge, save, notify. Any failure
// short-circuits the rest, and each wraps its cause with %w.
func (s *OrderService) PlaceOrder(ctx context.Context, order *Order) error {
	if order == nil {
		return errors.New("di: order is required")
	}
	if order.ID == "" {
		return ErrEmptyOrderID
	}
	if order.UserEmail == "" {
		return ErrEmptyUserEmail
	}

	txnID, err := s.payment.Charge(ctx, order.UserEmail, order.Total)
	if err != nil {
		return fmt.Errorf("payment: %w", err)
	}
	order.TxnID = txnID

	order.Status = "confirmed"
	order.CreatedAt = time.Now()
	if err := s.store.Save(ctx, order); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	body := fmt.Sprintf("Your order %s for $%.2f has been confirmed.", order.ID, order.Total)
	if err := s.notifier.Send(ctx, order.UserEmail, "Order Confirmed", body); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// Store, Payment, and Notifier expose the injected dependencies read-only, so
// the demo can report what was wired and tests can confirm the wiring survived.
func (s *OrderService) Store() OrderStore         { return s.store }
func (s *OrderService) Payment() PaymentProcessor { return s.payment }
func (s *OrderService) Notifier() Notifier        { return s.notifier }

// InMemoryStore is a fake store: a real, working implementation backed by a map.
type InMemoryStore struct {
	orders map[string]*Order
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{orders: make(map[string]*Order)}
}

func (s *InMemoryStore) Save(_ context.Context, order *Order) error {
	s.orders[order.ID] = order
	return nil
}

func (s *InMemoryStore) FindByID(_ context.Context, id string) (*Order, error) {
	o, ok := s.orders[id]
	if !ok {
		return nil, fmt.Errorf("di: order %s not found", id)
	}
	return o, nil
}

func (s *InMemoryStore) Count() int { return len(s.orders) }

// StubPayment is a deterministic stub: it returns a canned transaction ID, or a
// declined error when Fail is set.
type StubPayment struct {
	TxnPrefix string
	Fail      bool
}

func (p StubPayment) Charge(_ context.Context, email string, amount float64) (string, error) {
	if p.Fail {
		return "", ErrPaymentDeclined
	}
	return fmt.Sprintf("%s_%s_%.0f", p.TxnPrefix, email[:min(3, len(email))], amount*100), nil
}

// ConsoleNotifier is a real notifier that prints to stdout.
type ConsoleNotifier struct{}

func (ConsoleNotifier) Send(_ context.Context, to, subject, _ string) error {
	fmt.Printf("  [EMAIL] to=%s subject=%q\n", to, subject)
	return nil
}
```

`StubPayment.Charge` uses the built-in `min` (Go 1.21+) to clip the email to its
first three bytes for a readable, deterministic transaction ID; there is no
hand-rolled helper. The three concrete types live in this same file purely so the
demo and tests have something real to wire — in a production layout they would sit
in their own packages, but the interfaces they satisfy would be unchanged.

### The runnable demo

The demo is the composition root in miniature: the one place that names the
concrete implementations, constructs them, and threads them through the
constructor in dependency order. Everything below `main` deals only in interfaces.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/constructor-injection"
)

func main() {
	// Composition root: choose and wire the concrete implementations here.
	store := di.NewInMemoryStore()
	svc, err := di.NewOrderService(store, di.StubPayment{TxnPrefix: "txn"}, di.ConsoleNotifier{})
	if err != nil {
		log.Fatalf("NewOrderService: %v", err)
	}

	order := &di.Order{
		ID:        "ORD-001",
		UserEmail: "alice@example.com",
		Items:     []string{"Widget", "Gadget"},
		Total:     99.95,
	}

	fmt.Printf("placing order %s for %s total=%.2f\n", order.ID, order.UserEmail, order.Total)
	if err := svc.PlaceOrder(context.Background(), order); err != nil {
		log.Fatalf("PlaceOrder: %v", err)
	}

	saved, _ := store.FindByID(context.Background(), order.ID)
	fmt.Printf("order confirmed: id=%s status=%s txn=%s stored=%d\n",
		saved.ID, saved.Status, saved.TxnID, store.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
placing order ORD-001 for alice@example.com total=99.95
  [EMAIL] to=alice@example.com subject="Order Confirmed"
order confirmed: id=ORD-001 status=confirmed txn=txn_ali_9995 stored=1
```

### Tests

The tests inject local doubles — a mock store that records what it saved, a stub
payment, and a mock notifier that records its recipients — and pin the contract
from both directions. `TestPlaceOrder_HappyPath` proves the order is saved exactly
once, stamped `confirmed`, and carries the transaction ID. The
payment-failure test is the load-bearing one: it proves a declined charge stops
the pipeline, so nothing is saved and no notification is sent.
`TestNewOrderService_RejectsNilDependencies` and
`TestPlaceOrder_RejectsInvalidInput` pin the construction-time and call-time
guards, and `TestAccessorsReturnDependencies` proves the injected values survive
intact behind the accessors.

Create `service_test.go`:

```go
package di

import (
	"context"
	"errors"
	"testing"
)

type mockStore struct {
	saved []*Order
}

func (m *mockStore) Save(_ context.Context, o *Order) error {
	m.saved = append(m.saved, o)
	return nil
}

func (m *mockStore) FindByID(_ context.Context, id string) (*Order, error) {
	for _, o := range m.saved {
		if o.ID == id {
			return o, nil
		}
	}
	return nil, errors.New("not found")
}

type mockPayment struct {
	shouldFail bool
	txnID      string
}

func (m mockPayment) Charge(_ context.Context, _ string, _ float64) (string, error) {
	if m.shouldFail {
		return "", ErrPaymentDeclined
	}
	if m.txnID != "" {
		return m.txnID, nil
	}
	return "mock-txn-001", nil
}

type mockNotifier struct {
	sent []string
	err  error
}

func (m *mockNotifier) Send(_ context.Context, to, _, _ string) error {
	m.sent = append(m.sent, to)
	return m.err
}

func TestNewOrderService_RejectsNilDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewOrderService(nil, mockPayment{}, &mockNotifier{}); err == nil {
		t.Error("expected error for nil store")
	}
	if _, err := NewOrderService(&mockStore{}, nil, &mockNotifier{}); err == nil {
		t.Error("expected error for nil payment")
	}
	if _, err := NewOrderService(&mockStore{}, mockPayment{}, nil); err == nil {
		t.Error("expected error for nil notifier")
	}
}

func TestPlaceOrder_HappyPath(t *testing.T) {
	t.Parallel()

	store := &mockStore{}
	notifier := &mockNotifier{}
	svc, err := NewOrderService(store, mockPayment{txnID: "txn-1"}, notifier)
	if err != nil {
		t.Fatalf("NewOrderService: %v", err)
	}

	order := &Order{ID: "test-1", UserEmail: "test@test.com", Total: 50}
	if err := svc.PlaceOrder(context.Background(), order); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved count = %d, want 1", len(store.saved))
	}
	if store.saved[0].Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", store.saved[0].Status)
	}
	if order.TxnID != "txn-1" {
		t.Errorf("txn = %q, want txn-1", order.TxnID)
	}
	if store.saved[0].CreatedAt.IsZero() {
		t.Error("CreatedAt should be set on save")
	}
	if len(notifier.sent) != 1 || notifier.sent[0] != "test@test.com" {
		t.Errorf("notifier recipients = %v, want [test@test.com]", notifier.sent)
	}
}

func TestPlaceOrder_PaymentFailureShortCircuits(t *testing.T) {
	t.Parallel()

	store := &mockStore{}
	notifier := &mockNotifier{}
	svc, _ := NewOrderService(store, mockPayment{shouldFail: true}, notifier)

	order := &Order{ID: "test-2", UserEmail: "u@x.com", Total: 50}
	err := svc.PlaceOrder(context.Background(), order)
	if err == nil {
		t.Fatal("expected payment error, got nil")
	}
	if !errors.Is(err, ErrPaymentDeclined) {
		t.Errorf("err = %v, want ErrPaymentDeclined", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("order should not be saved after payment failure, got %d", len(store.saved))
	}
	if len(notifier.sent) != 0 {
		t.Errorf("notifier should not run after payment failure, got %d", len(notifier.sent))
	}
}

func TestPlaceOrder_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	svc, _ := NewOrderService(&mockStore{}, mockPayment{}, &mockNotifier{})

	if err := svc.PlaceOrder(context.Background(), &Order{UserEmail: "u@x.com"}); !errors.Is(err, ErrEmptyOrderID) {
		t.Errorf("missing ID err = %v, want ErrEmptyOrderID", err)
	}
	if err := svc.PlaceOrder(context.Background(), &Order{ID: "x"}); !errors.Is(err, ErrEmptyUserEmail) {
		t.Errorf("missing email err = %v, want ErrEmptyUserEmail", err)
	}
	if err := svc.PlaceOrder(context.Background(), nil); err == nil {
		t.Error("nil order should be rejected")
	}
}

func TestAccessorsReturnDependencies(t *testing.T) {
	t.Parallel()

	store := &mockStore{}
	payment := mockPayment{}
	notifier := &mockNotifier{}
	svc, _ := NewOrderService(store, payment, notifier)

	if svc.Store() != store {
		t.Error("Store() did not return the injected store")
	}
	if svc.Payment() != payment {
		t.Error("Payment() did not return the injected payment")
	}
	if svc.Notifier() != notifier {
		t.Error("Notifier() did not return the injected notifier")
	}
}
```

## Review

The service is correctly injected when nothing inside it names a concrete type:
the three fields are interfaces, the constructor is the only way to set them, and
every concrete implementation is chosen by the caller. Confirm the constructor
rejects each `nil` with its own error rather than deferring the failure, and that
`PlaceOrder` runs validate-charge-save-notify in that order with a hard
short-circuit — the payment-failure test is the one that proves you did not save
or notify an unpaid order. The accessors return the very values that were
injected, which is what lets the demo report the wiring and the test assert it.

The common mistakes here are three. The first is widening the interfaces: if
`OrderStore` grew to a dozen methods, every mock in the test file would have to
stub all of them and every unrelated change would ripple; keep each interface to
the methods this service actually calls. The second is skipping the nil checks and
returning a `*OrderService` anyway, which converts a clear construction-time error
into an obscure later panic. The third is reaching for a shared mock package; the
doubles here are a few lines each and live next to the test, so they say exactly
what this service needs and nothing more.

## Resources

- [Go Wiki: Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — the rule that interfaces belong to the package that consumes them, not the one that implements them.
- [Go Blog: Errors are values](https://go.dev/blog/errors-are-values) — the error-as-value model behind the `%w` wrapping and `errors.Is` matching the pipeline relies on.
- [Uber Go Style Guide: Test tables and mocks](https://github.com/uber-go/guide/blob/master/style.md#test-tables) — idiomatic table-driven tests and locally-defined test doubles.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-injecting-seams.md](02-injecting-seams.md)
