# Exercise 5: Injected Order Processing With Compensation

A use case that touches several external systems has to answer a hard question:
what happens when step three fails after steps one and two already succeeded? This
exercise builds an `OrderProcessor` whose three external dependencies — an inventory
service, a payment gateway, and a notifier — are all injected behind consumer-owned
interfaces, and whose `Process` method runs them as a saga: each successful step is
remembered, and any later failure compensates the completed steps in reverse order
(refund the charge, release the reservation) before returning. A happy-path test and
two failure-path tests with recording fakes prove the rollback is exact.

This module is fully self-contained. It starts with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
processor.go         Order, Receipt; the Inventory / PaymentGateway / Notifier
                     consumer interfaces (each with its compensating method);
                     OrderProcessor.Process running the saga with reverse-order
                     compensation; sentinel errors; small working fakes for the demo
cmd/
  demo/
    main.go          wire the fakes in the composition root and process an order
processor_test.go    recording fakes; happy path, payment-failure releases the
                     reservation (no refund), notify-failure refunds then releases
                     in exact reverse order
```

- Files: `processor.go`, `cmd/demo/main.go`, `processor_test.go`.
- Implement: `NewOrderProcessor(inventory, payments, notifier)` and `Process(ctx, Order) (*Receipt, error)` with reverse-order compensation on failure.
- Test: assert the happy path returns a full receipt, a payment failure releases the reservation and never refunds, and a notify failure refunds the charge then releases the reservation.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/04-dependency-injection/05-order-processing-compensation/cmd/demo && cd go-solutions/24-design-patterns-in-go/04-dependency-injection/05-order-processing-compensation
```

### Why compensation, and why reverse order

The dependencies here are not interchangeable side effects; they have *consequences
in the outside world* that cannot be undone by a deferred rollback the way a database
transaction can. Reserving stock removes it from other buyers. Charging a card moves
real money. Sending a confirmation email cannot be unsent. Because there is no shared
transaction across three independent systems, the only way to keep them consistent is
the saga pattern: perform the steps in sequence, remember the result of each one that
succeeds, and if a later step fails, run a *compensating action* for each completed
step to walk the world back to where it started. That is why every dependency
interface in this exercise carries two methods — a forward action and its inverse:
`Reserve`/`Release` on inventory, `Charge`/`Refund` on payments. The notifier is the
terminal step and has nothing to compensate, so it has only `NotifyConfirmed`.

The order of compensation matters and it is the reverse of the order the steps ran.
`Process` reserves, then charges, then notifies. If the notify step fails, the things
that succeeded are the reservation (first) and the charge (second), so they must be
undone last-in-first-out: refund the charge, then release the reservation. Reverse
order is not cosmetic — it mirrors how `defer` unwinds and how a stack of acquired
resources is released, and it matters whenever a later step's compensation depends on
an earlier step still being in place. The test that drives a notify failure asserts
the recorded sequence is exactly `reserve, charge, refund, release`, which is the
forward steps followed by their compensations in mirror order.

The failure cases are deliberately different and both are worth pinning. When the
charge fails, only one step succeeded — the reservation — so only one compensation
runs: release the stock, and crucially *no refund is attempted*, because no money
ever moved and refunding a charge that never happened is itself a bug. When the
notify fails, two steps succeeded, so two compensations run. Encoding this as
"compensate exactly the steps that completed, in reverse" rather than "always refund
and always release" is the difference between a correct saga and one that issues
phantom refunds. Compensation errors are themselves joined onto the original failure
with `errors.Join` rather than swallowed, so an operator sees both that the order
failed and whether the cleanup also failed.

Create `processor.go`:

```go
package orders

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors callers match with errors.Is.
var (
	ErrEmptyOrderID = errors.New("orders: order ID is required")
	ErrNoItems      = errors.New("orders: order has no items")
	ErrOutOfStock   = errors.New("orders: item is out of stock")
	ErrCardDeclined = errors.New("orders: card declined")
	ErrNotifyFailed = errors.New("orders: notification failed")
)

// Order is the input to the use case.
type Order struct {
	ID         string
	CustomerID string
	SKUs       []string
	Total      float64
}

func (o Order) validate() error {
	if o.ID == "" {
		return ErrEmptyOrderID
	}
	if len(o.SKUs) == 0 {
		return ErrNoItems
	}
	return nil
}

// Receipt is the success result: the IDs minted by the two stateful steps.
type Receipt struct {
	OrderID       string
	ReservationID string
	ChargeID      string
}

// Inventory is the stock seam. Reserve is the forward action; Release is its
// compensation. The consumer declares both methods because the saga needs both.
type Inventory interface {
	Reserve(ctx context.Context, skus []string) (reservationID string, err error)
	Release(ctx context.Context, reservationID string) error
}

// PaymentGateway is the money seam: Charge forward, Refund to compensate.
type PaymentGateway interface {
	Charge(ctx context.Context, customerID string, amount float64) (chargeID string, err error)
	Refund(ctx context.Context, chargeID string) error
}

// Notifier is the terminal seam: there is nothing to undo once it succeeds, so it
// has no compensating method.
type Notifier interface {
	NotifyConfirmed(ctx context.Context, orderID string) error
}

// OrderProcessor holds its three dependencies behind unexported interface fields.
type OrderProcessor struct {
	inventory Inventory
	payments  PaymentGateway
	notifier  Notifier
}

// NewOrderProcessor injects the dependencies and rejects any nil.
func NewOrderProcessor(inventory Inventory, payments PaymentGateway, notifier Notifier) (*OrderProcessor, error) {
	if inventory == nil {
		return nil, errors.New("orders: inventory is required")
	}
	if payments == nil {
		return nil, errors.New("orders: payments is required")
	}
	if notifier == nil {
		return nil, errors.New("orders: notifier is required")
	}
	return &OrderProcessor{inventory: inventory, payments: payments, notifier: notifier}, nil
}

// Process runs the saga: reserve, charge, notify. Each successful step is
// remembered, and any later failure compensates the completed steps in reverse
// order before returning the (joined) error.
func (p *OrderProcessor) Process(ctx context.Context, o Order) (*Receipt, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}

	// Step 1: reserve stock. Nothing has happened yet, so a failure compensates
	// nothing.
	reservationID, err := p.inventory.Reserve(ctx, o.SKUs)
	if err != nil {
		return nil, fmt.Errorf("orders: reserve: %w", err)
	}

	// Step 2: charge. On failure, compensate step 1 (release) but do NOT refund —
	// no charge ever succeeded.
	chargeID, err := p.payments.Charge(ctx, o.CustomerID, o.Total)
	if err != nil {
		err = fmt.Errorf("orders: charge: %w", err)
		if rel := p.inventory.Release(ctx, reservationID); rel != nil {
			err = errors.Join(err, fmt.Errorf("orders: release after charge failure: %w", rel))
		}
		return nil, err
	}

	// Step 3: notify. On failure, compensate steps 2 and 1 in reverse order:
	// refund the charge, then release the reservation.
	if nerr := p.notifier.NotifyConfirmed(ctx, o.ID); nerr != nil {
		err = fmt.Errorf("orders: notify: %w", nerr)
		if ref := p.payments.Refund(ctx, chargeID); ref != nil {
			err = errors.Join(err, fmt.Errorf("orders: refund during rollback: %w", ref))
		}
		if rel := p.inventory.Release(ctx, reservationID); rel != nil {
			err = errors.Join(err, fmt.Errorf("orders: release during rollback: %w", rel))
		}
		return nil, err
	}

	return &Receipt{OrderID: o.ID, ReservationID: reservationID, ChargeID: chargeID}, nil
}

// --- small working fakes, used only by the demo ---

// MemInventory is a working inventory fake: it hands out sequential reservation
// IDs and records releases.
type MemInventory struct {
	next     int
	Released []string
}

func (m *MemInventory) Reserve(_ context.Context, _ []string) (string, error) {
	m.next++
	return fmt.Sprintf("rsv-%d", m.next), nil
}

func (m *MemInventory) Release(_ context.Context, id string) error {
	m.Released = append(m.Released, id)
	return nil
}

// MemPayments is a working payment fake that hands out charge IDs and records
// refunds.
type MemPayments struct {
	next     int
	Refunded []string
}

func (m *MemPayments) Charge(_ context.Context, _ string, _ float64) (string, error) {
	m.next++
	return fmt.Sprintf("chg-%d", m.next), nil
}

func (m *MemPayments) Refund(_ context.Context, id string) error {
	m.Refunded = append(m.Refunded, id)
	return nil
}

// LogNotifier is a working notifier fake that prints the confirmation.
type LogNotifier struct{}

func (LogNotifier) NotifyConfirmed(_ context.Context, orderID string) error {
	fmt.Printf("  [NOTIFY] order %s confirmed\n", orderID)
	return nil
}
```

### The runnable demo

The demo is the composition root: it constructs the three fakes, injects them
through `NewOrderProcessor`, and processes one order down the happy path. The
receipt it prints shows the two IDs the stateful steps minted, and the empty
release/refund slices confirm no compensation ran.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/order-processing"
)

func main() {
	// Composition root: choose and wire the concrete implementations here.
	inv := &orders.MemInventory{}
	pay := &orders.MemPayments{}
	proc, err := orders.NewOrderProcessor(inv, pay, orders.LogNotifier{})
	if err != nil {
		log.Fatalf("NewOrderProcessor: %v", err)
	}

	order := orders.Order{
		ID:         "ORD-77",
		CustomerID: "cust-9",
		SKUs:       []string{"sku-a", "sku-b"},
		Total:      149.50,
	}

	fmt.Printf("processing order %s for %s total=%.2f\n", order.ID, order.CustomerID, order.Total)
	receipt, err := proc.Process(context.Background(), order)
	if err != nil {
		log.Fatalf("Process: %v", err)
	}

	fmt.Printf("receipt: order=%s reservation=%s charge=%s\n",
		receipt.OrderID, receipt.ReservationID, receipt.ChargeID)
	fmt.Printf("compensations: released=%v refunded=%v\n", inv.Released, pay.Refunded)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processing order ORD-77 for cust-9 total=149.50
  [NOTIFY] order ORD-77 confirmed
receipt: order=ORD-77 reservation=rsv-1 charge=chg-1
compensations: released=[] refunded=[]
```

### Tests

The tests use recording fakes that append every call to a shared event log, so an
assertion can pin not only *which* compensations ran but their exact order.
`TestProcess_HappyPath` proves a full receipt comes back and nothing is released or
refunded. `TestProcess_PaymentFailureReleasesReservation` is the first
compensation proof: the charge fails, so the reservation is released and — the
subtle part — `Refund` is never called, because no charge succeeded.
`TestProcess_NotifyFailureRollsBackInReverse` is the full-rollback proof: the notify
fails after two successful steps, and the recorded sequence must be exactly
`reserve, charge, refund, release` — forward steps then their compensations in
mirror order — with the refund carrying the charge ID and the release carrying the
reservation ID.

Create `processor_test.go`:

```go
package orders

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// recorder is a shared, ordered log of the calls the fakes make.
type recorder struct{ events []string }

func (r *recorder) add(e string) { r.events = append(r.events, e) }

type fakeInventory struct {
	rec         *recorder
	failReserve bool
}

func (f *fakeInventory) Reserve(context.Context, []string) (string, error) {
	if f.failReserve {
		return "", ErrOutOfStock
	}
	f.rec.add("reserve")
	return "rsv-1", nil
}

func (f *fakeInventory) Release(_ context.Context, id string) error {
	f.rec.add("release:" + id)
	return nil
}

type fakePayments struct {
	rec        *recorder
	failCharge bool
}

func (f *fakePayments) Charge(context.Context, string, float64) (string, error) {
	if f.failCharge {
		return "", ErrCardDeclined
	}
	f.rec.add("charge")
	return "chg-1", nil
}

func (f *fakePayments) Refund(_ context.Context, id string) error {
	f.rec.add("refund:" + id)
	return nil
}

type fakeNotifier struct {
	rec        *recorder
	failNotify bool
}

func (f *fakeNotifier) NotifyConfirmed(context.Context, string) error {
	if f.failNotify {
		return ErrNotifyFailed
	}
	f.rec.add("notify")
	return nil
}

func sampleOrder() Order {
	return Order{ID: "ORD-1", CustomerID: "c-1", SKUs: []string{"s-1"}, Total: 10}
}

func TestProcess_HappyPath(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	proc, err := NewOrderProcessor(
		&fakeInventory{rec: rec},
		&fakePayments{rec: rec},
		&fakeNotifier{rec: rec},
	)
	if err != nil {
		t.Fatalf("NewOrderProcessor: %v", err)
	}

	receipt, err := proc.Process(context.Background(), sampleOrder())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if receipt.ReservationID != "rsv-1" || receipt.ChargeID != "chg-1" {
		t.Errorf("receipt = %+v, want reservation=rsv-1 charge=chg-1", receipt)
	}
	want := []string{"reserve", "charge", "notify"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Errorf("events = %v, want %v (no compensation on success)", rec.events, want)
	}
}

func TestProcess_PaymentFailureReleasesReservation(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	proc, _ := NewOrderProcessor(
		&fakeInventory{rec: rec},
		&fakePayments{rec: rec, failCharge: true},
		&fakeNotifier{rec: rec},
	)

	_, err := proc.Process(context.Background(), sampleOrder())
	if !errors.Is(err, ErrCardDeclined) {
		t.Fatalf("err = %v, want ErrCardDeclined", err)
	}
	// Reservation was released; no refund, because no charge ever succeeded.
	want := []string{"reserve", "release:rsv-1"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Errorf("events = %v, want %v", rec.events, want)
	}
	for _, e := range rec.events {
		if len(e) >= 6 && e[:6] == "refund" {
			t.Errorf("refund must not run when the charge never succeeded: %v", rec.events)
		}
	}
}

func TestProcess_NotifyFailureRollsBackInReverse(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	proc, _ := NewOrderProcessor(
		&fakeInventory{rec: rec},
		&fakePayments{rec: rec},
		&fakeNotifier{rec: rec, failNotify: true},
	)

	_, err := proc.Process(context.Background(), sampleOrder())
	if !errors.Is(err, ErrNotifyFailed) {
		t.Fatalf("err = %v, want ErrNotifyFailed", err)
	}
	// Forward steps, then their compensations in exact reverse order, with the
	// right IDs threaded through.
	want := []string{"reserve", "charge", "refund:chg-1", "release:rsv-1"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Errorf("events = %v, want %v", rec.events, want)
	}
}

func TestNewOrderProcessor_RejectsNil(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	if _, err := NewOrderProcessor(nil, &fakePayments{rec: rec}, &fakeNotifier{rec: rec}); err == nil {
		t.Error("expected error for nil inventory")
	}
	if _, err := NewOrderProcessor(&fakeInventory{rec: rec}, nil, &fakeNotifier{rec: rec}); err == nil {
		t.Error("expected error for nil payments")
	}
	if _, err := NewOrderProcessor(&fakeInventory{rec: rec}, &fakePayments{rec: rec}, nil); err == nil {
		t.Error("expected error for nil notifier")
	}
}
```

## Review

The saga is correct when it compensates exactly the steps that completed, in the
reverse of the order they ran. The two failure tests are the proof: a charge failure
releases the reservation and issues no refund, because refunding a charge that never
happened would move money that was never taken; a notify failure refunds the charge
and then releases the reservation, in that mirror order. Confirm each dependency
interface pairs its forward action with its compensating one — `Reserve`/`Release`,
`Charge`/`Refund` — and that the terminal notifier has none, because there is nothing
to undo once it succeeds. Confirm too that compensation errors are joined onto the
original failure with `errors.Join` rather than discarded, so a failed cleanup is
visible alongside the failure that triggered it.

The common mistakes are three. The first is always running every compensation
regardless of how far the saga got, which issues phantom refunds or releases for
steps that never ran; compensate only what completed. The second is compensating in
forward order, which is wrong whenever a later step's undo depends on an earlier
step still being present — always unwind last-in-first-out, the way `defer` does.
The third is swallowing the compensation error so a refund that itself failed
vanishes; join it onto the returned error so the failure of the cleanup is reported,
not hidden.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — combines the original failure with any compensation errors into one inspectable error, as the rollback paths do.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the `%w` wrapping and `errors.Is` matching the saga relies on to classify failures.
- [Pattern: Saga](https://microservices.io/patterns/data/saga.html) — Chris Richardson's reference for sequencing local transactions and compensating completed steps when a later one fails.

---

Back to [04-layered-http-service.md](04-layered-http-service.md) | Next: [Repository Pattern](../05-repository-pattern/00-concepts.md)
