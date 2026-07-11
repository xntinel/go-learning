# Exercise 4: The Booking Saga

When a use case has to commit work across several systems that cannot share one database transaction — two inventory services and an external payment API — the service layer becomes a saga: a sequence of steps, each paired with the compensating action that undoes it. This module builds a `BookingService` whose `Book` reserves a flight, reserves a hotel, charges a card, and confirms the trip, rolling back every earlier step the moment a later one fails, driven by a small reusable `Saga` runner.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
saga.go              Step (Name, Action, Compensate), Saga.Add, Saga.Run,
                     Saga.compensate (reverse-order undo)
booking.go           Booking, BookRequest, domain sentinels, the four
                     collaborator interfaces, BookingService.Book built as a saga
cmd/
  demo/
    main.go          a happy booking, a declined payment, a hotel-full mid-step
booking_test.go      pin the step order, both rollback paths, the reverse-order
                     compensation, and the nil-dependency guard
```

- Files: `saga.go`, `booking.go`, `cmd/demo/main.go`, `booking_test.go`.
- Implement: a generic `Saga` (`Add`, `Run`, reverse-order `compensate`), `NewBookingService` (rejects a nil dependency), and `(*BookingService).Book` assembled as a four-step saga.
- Test: `booking_test.go` records the exact order of side effects and asserts the happy path, payment-failure rollback, hotel-full rollback, save-failure rollback (charge refunded), reverse compensation order, invalid-request rejection, and the nil guard.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p booking-saga/cmd/demo && cd booking-saga
go mod init example.com/booking-saga
```

### Why a saga, and how compensation differs from a rollback

A database transaction gives you atomicity for free: write, write, write, then `COMMIT` or `ROLLBACK`, and the engine guarantees all-or-nothing. The moment a use case spans systems that do not enlist in one transaction — a flight inventory, a hotel inventory, and a payment processor reachable only over HTTP — that guarantee is gone. You cannot `ROLLBACK` a credit-card charge; you can only issue a refund. This is the saga pattern: model the use case as an ordered list of steps, give each step an explicit *compensating action* that semantically undoes it, and on any failure run the compensations for the steps that already succeeded, in reverse order.

The reusable core is tiny. A `Step` is a name, a forward `Action`, and a `Compensate` closure. A `Saga` holds an ordered slice of steps; `Run` executes each `Action` in turn, appending the step to a `completed` list as it succeeds, and the instant an `Action` returns an error it calls `compensate(completed)` and returns an error that names the failed step. `compensate` walks `completed` backwards and calls each non-nil `Compensate`. Reverse order is the rule because a later step may depend on the effect of an earlier one, so undoing has to peel the work off in the opposite order it was applied. The last step needs no compensation — nothing runs after it, so there is nothing to undo if everything up to and including it succeeded — which is why its `Compensate` is `nil` and `compensate` skips nil entries.

The reason `Action` and `Compensate` are closures rather than methods on a fixed interface is that each step needs to thread *state* between its forward and undo phases. Reserving a flight returns an opaque reservation id; cancelling it needs that exact id. So `Book` declares `var flightRes, hotelRes, txnID string` in its own scope, and the flight step's `Action` assigns `flightRes` while its `Compensate` reads it. The closures capture those variables by reference, so by the time a compensation runs, it sees the id the forward action stored. This is the saga equivalent of a transaction's implicit undo log: the captured variables *are* the log.

Two failure modes are worth internalizing. First, a compensation can itself fail — the refund API might be down. `compensate` logs that and keeps going rather than aborting, because the caller already has a primary error to return and the operator's real need is to learn that an undo did not complete (a stranded charge needs a human). Returning the compensation error would mask the original cause. Second, the external charge sits in the middle of the saga deliberately: reservations are cheap to hold and cheap to release, so the service reserves first and only then charges, meaning a decline rolls back two cheap holds rather than stranding a charge against an unreserved trip. Order the steps cheapest-and-most-reversible first.

`Book` itself validates the request up front — an empty user, flight, or hotel id, or a negative amount, fails before any collaborator is touched — then builds the four steps and calls `saga.Run`. Read the four `Add` calls top to bottom and you read the business process; read the `Compensate` closures and you read the undo plan beside it.

Create `saga.go`:

```go
package booking

import (
	"context"
	"fmt"
)

// Step is one unit of work in a saga: a forward Action and the Compensate that
// undoes it. Compensate may be nil for a step that needs no rollback, which is
// typically the last step since nothing runs after it.
type Step struct {
	Name       string
	Action     func(ctx context.Context) error
	Compensate func(ctx context.Context) error
}

// Saga is an ordered list of steps run as one logical transaction. If any step
// fails, the steps that already succeeded are compensated in reverse order.
type Saga struct {
	steps []Step
}

// Add appends a step and returns the saga so construction can chain.
func (s *Saga) Add(step Step) *Saga {
	s.steps = append(s.steps, step)
	return s
}

// Run executes every step in order. On the first failure it compensates the
// completed steps in reverse and returns an error naming the step that failed.
func (s *Saga) Run(ctx context.Context) error {
	var completed []Step
	for _, step := range s.steps {
		if err := step.Action(ctx); err != nil {
			s.compensate(ctx, completed)
			return fmt.Errorf("saga: step %q failed: %w", step.Name, err)
		}
		completed = append(completed, step)
	}
	return nil
}

// compensate runs the undo of each completed step in reverse. A failed
// compensation is logged, not returned: the caller already holds the primary
// error, and an operator needs to know an undo did not complete.
func (s *Saga) compensate(ctx context.Context, completed []Step) {
	for i := len(completed) - 1; i >= 0; i-- {
		step := completed[i]
		if step.Compensate == nil {
			continue
		}
		if err := step.Compensate(ctx); err != nil {
			fmt.Printf("warning: compensation for step %q failed: %v\n", step.Name, err)
		}
	}
}
```

`Run` is the entire engine: try each action, record it, and on failure peel back the recorded ones. Note that `compensate` is unexported — it is an internal mechanism of `Run`, not part of the saga's surface.

Now the service. Each port is narrow and declared here; the `Reserve`/`Cancel` pairs and the `Charge`/`Refund` pair are the forward-and-compensate halves the saga wires together.

Create `booking.go`:

```go
package booking

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Booking is the artifact a fully successful saga produces.
type Booking struct {
	ID       string
	UserID   string
	FlightID string
	HotelID  string
	Amount   float64
	TxnID    string
	Status   string
	BookedAt time.Time
}

// BookRequest is the input to the use case.
type BookRequest struct {
	UserID   string
	FlightID string
	HotelID  string
	Amount   float64
}

// Domain sentinels. Every failure the service reports wraps one of these, so a
// caller branches with errors.Is and never matches on a string.
var (
	ErrInvalidRequest  = errors.New("booking: request is incomplete")
	ErrFlightFull      = errors.New("booking: flight has no seats")
	ErrHotelFull       = errors.New("booking: hotel has no rooms")
	ErrPaymentDeclined = errors.New("booking: payment declined")
)

// FlightRepository reserves and cancels a seat. Reserve returns an opaque
// reservation id that Cancel consumes to undo the reservation.
type FlightRepository interface {
	Reserve(ctx context.Context, flightID, userID string) (reservationID string, err error)
	Cancel(ctx context.Context, reservationID string) error
}

// HotelRepository reserves and cancels a room under the same id contract.
type HotelRepository interface {
	Reserve(ctx context.Context, hotelID, userID string) (reservationID string, err error)
	Cancel(ctx context.Context, reservationID string) error
}

// PaymentGateway is the external charge API. It cannot share a database
// transaction, which is exactly why the saga compensates it with Refund rather
// than rolling it back.
type PaymentGateway interface {
	Charge(ctx context.Context, userID string, amount float64) (txnID string, err error)
	Refund(ctx context.Context, txnID string) error
}

// BookingRepository allocates an id and persists the final confirmed booking.
type BookingRepository interface {
	NextID(ctx context.Context) (string, error)
	Save(ctx context.Context, b *Booking) error
}

// BookingService orchestrates a trip booking as a saga. It owns no data; it
// declares the narrow ports it needs and sequences them with compensation.
type BookingService struct {
	flights  FlightRepository
	hotels   HotelRepository
	payments PaymentGateway
	bookings BookingRepository
}

// NewBookingService wires the dependencies and rejects a nil one up front, so a
// misconfigured service fails at construction rather than mid-booking.
func NewBookingService(
	flights FlightRepository,
	hotels HotelRepository,
	payments PaymentGateway,
	bookings BookingRepository,
) (*BookingService, error) {
	if flights == nil || hotels == nil || payments == nil || bookings == nil {
		return nil, errors.New("booking: all dependencies are required")
	}
	return &BookingService{
		flights:  flights,
		hotels:   hotels,
		payments: payments,
		bookings: bookings,
	}, nil
}

// Book reserves a flight and a hotel, charges the card, and confirms the
// booking as one saga. Any step failing rolls back the steps before it: a
// declined charge cancels both reservations; a hotel with no rooms cancels the
// flight already reserved.
func (s *BookingService) Book(ctx context.Context, req BookRequest) (*Booking, error) {
	if req.UserID == "" || req.FlightID == "" || req.HotelID == "" {
		return nil, ErrInvalidRequest
	}
	if req.Amount < 0 {
		return nil, fmt.Errorf("%w: amount must not be negative", ErrInvalidRequest)
	}

	var flightRes, hotelRes, txnID string
	var booking *Booking
	saga := &Saga{}

	saga.Add(Step{
		Name: "reserve-flight",
		Action: func(ctx context.Context) error {
			id, err := s.flights.Reserve(ctx, req.FlightID, req.UserID)
			if err != nil {
				return err
			}
			flightRes = id
			return nil
		},
		Compensate: func(ctx context.Context) error {
			return s.flights.Cancel(ctx, flightRes)
		},
	})

	saga.Add(Step{
		Name: "reserve-hotel",
		Action: func(ctx context.Context) error {
			id, err := s.hotels.Reserve(ctx, req.HotelID, req.UserID)
			if err != nil {
				return err
			}
			hotelRes = id
			return nil
		},
		Compensate: func(ctx context.Context) error {
			return s.hotels.Cancel(ctx, hotelRes)
		},
	})

	saga.Add(Step{
		Name: "charge-payment",
		Action: func(ctx context.Context) error {
			id, err := s.payments.Charge(ctx, req.UserID, req.Amount)
			if err != nil {
				return err
			}
			txnID = id
			return nil
		},
		Compensate: func(ctx context.Context) error {
			return s.payments.Refund(ctx, txnID)
		},
	})

	saga.Add(Step{
		Name: "confirm-booking",
		Action: func(ctx context.Context) error {
			id, err := s.bookings.NextID(ctx)
			if err != nil {
				return err
			}
			b := &Booking{
				ID:       id,
				UserID:   req.UserID,
				FlightID: req.FlightID,
				HotelID:  req.HotelID,
				Amount:   req.Amount,
				TxnID:    txnID,
				Status:   "confirmed",
				BookedAt: time.Now(),
			}
			if err := s.bookings.Save(ctx, b); err != nil {
				return err
			}
			booking = b
			return nil
		},
		// The last step needs no compensation: nothing runs after it.
		Compensate: nil,
	})

	if err := saga.Run(ctx); err != nil {
		return nil, err
	}
	return booking, nil
}
```

The shape to notice: `Book` contains no `if err != nil { undo everything }` ladder. The undo logic lives once, in `Saga.Run`; `Book` only declares what each step does and how to reverse it. Adding a fifth step — issue loyalty points, say — means appending one `Add`, not threading another rollback branch through the method.

### The runnable demo

The demo wires hand-written fakes whose seat and room counters make the compensation visible: a successful booking decrements both, a declined payment releases both back to where they were, and a hotel with zero rooms cancels the flight that was already reserved. Each fake prints what it reserves and cancels, so the reverse-order undo shows up in the output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	booking "example.com/booking-saga"
)

type demoFlights struct {
	seats int
	seq   int
}

func (d *demoFlights) Reserve(_ context.Context, flightID, _ string) (string, error) {
	if d.seats <= 0 {
		return "", booking.ErrFlightFull
	}
	d.seats--
	d.seq++
	id := fmt.Sprintf("FL-%s-%d", flightID, d.seq)
	fmt.Printf("  reserved flight %s (seats left %d)\n", id, d.seats)
	return id, nil
}

func (d *demoFlights) Cancel(_ context.Context, id string) error {
	d.seats++
	fmt.Printf("  compensate: cancelled flight %s (seats left %d)\n", id, d.seats)
	return nil
}

type demoHotels struct {
	rooms int
	seq   int
}

func (d *demoHotels) Reserve(_ context.Context, hotelID, _ string) (string, error) {
	if d.rooms <= 0 {
		return "", booking.ErrHotelFull
	}
	d.rooms--
	d.seq++
	id := fmt.Sprintf("HO-%s-%d", hotelID, d.seq)
	fmt.Printf("  reserved hotel %s (rooms left %d)\n", id, d.rooms)
	return id, nil
}

func (d *demoHotels) Cancel(_ context.Context, id string) error {
	d.rooms++
	fmt.Printf("  compensate: cancelled hotel %s (rooms left %d)\n", id, d.rooms)
	return nil
}

type demoPayments struct{ fail bool }

func (d demoPayments) Charge(_ context.Context, _ string, amount float64) (string, error) {
	if d.fail {
		return "", booking.ErrPaymentDeclined
	}
	id := fmt.Sprintf("txn-%d", int(amount*100))
	fmt.Printf("  charged $%.2f (%s)\n", amount, id)
	return id, nil
}

func (d demoPayments) Refund(_ context.Context, txnID string) error {
	fmt.Printf("  compensate: refunded %s\n", txnID)
	return nil
}

type demoBookings struct{ seq int }

func (d *demoBookings) NextID(_ context.Context) (string, error) {
	d.seq++
	return fmt.Sprintf("BK-%03d", d.seq), nil
}

func (d *demoBookings) Save(_ context.Context, _ *booking.Booking) error { return nil }

func main() {
	ctx := context.Background()
	flights := &demoFlights{seats: 2}
	hotels := &demoHotels{rooms: 2}
	bookings := &demoBookings{}

	fmt.Println("=== successful booking ===")
	svc, err := booking.NewBookingService(flights, hotels, demoPayments{}, bookings)
	if err != nil {
		log.Fatal(err)
	}
	b, err := svc.Book(ctx, booking.BookRequest{UserID: "u1", FlightID: "AA100", HotelID: "RITZ", Amount: 540})
	if err != nil {
		log.Fatalf("unexpected: %v", err)
	}
	fmt.Printf("  %s status=%s txn=%s\n", b.ID, b.Status, b.TxnID)
	fmt.Printf("  seats left %d, rooms left %d\n", flights.seats, hotels.rooms)

	fmt.Println("=== payment declined (rolls back flight and hotel) ===")
	svcDeclined, _ := booking.NewBookingService(flights, hotels, demoPayments{fail: true}, bookings)
	_, err = svcDeclined.Book(ctx, booking.BookRequest{UserID: "u1", FlightID: "AA200", HotelID: "RITZ", Amount: 300})
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  payment declined? %v\n", errors.Is(err, booking.ErrPaymentDeclined))
	fmt.Printf("  seats restored %d, rooms restored %d\n", flights.seats, hotels.rooms)

	fmt.Println("=== hotel full (rolls back the flight already reserved) ===")
	flights2 := &demoFlights{seats: 1}
	hotels2 := &demoHotels{rooms: 0}
	svc2, _ := booking.NewBookingService(flights2, hotels2, demoPayments{}, bookings)
	_, err = svc2.Book(ctx, booking.BookRequest{UserID: "u1", FlightID: "AA300", HotelID: "FULL", Amount: 120})
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  hotel full? %v\n", errors.Is(err, booking.ErrHotelFull))
	fmt.Printf("  seats restored %d\n", flights2.seats)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== successful booking ===
  reserved flight FL-AA100-1 (seats left 1)
  reserved hotel HO-RITZ-1 (rooms left 1)
  charged $540.00 (txn-54000)
  BK-001 status=confirmed txn=txn-54000
  seats left 1, rooms left 1
=== payment declined (rolls back flight and hotel) ===
  reserved flight FL-AA200-2 (seats left 0)
  reserved hotel HO-RITZ-2 (rooms left 0)
  compensate: cancelled hotel HO-RITZ-2 (rooms left 1)
  compensate: cancelled flight FL-AA200-2 (seats left 1)
  error: saga: step "charge-payment" failed: booking: payment declined
  payment declined? true
  seats restored 1, rooms restored 1
=== hotel full (rolls back the flight already reserved) ===
  reserved flight FL-AA300-1 (seats left 0)
  compensate: cancelled flight FL-AA300-1 (seats left 1)
  error: saga: step "reserve-hotel" failed: booking: hotel has no rooms
  hotel full? true
  seats restored 1
```

The proof is in the cancellation lines and the restored counters: the declined-payment case undoes the hotel before the flight — reverse order — and both inventories return to exactly the value they held after the successful booking. The hotel-full case never charges anything and cancels only the one reservation it had made.

### Tests

The tests share a `callLog` that records the order of every side effect, which is what lets them assert not just *that* compensation ran but that it ran in reverse. `TestBook_HappyPath` pins the forward order `flight.reserve, hotel.reserve, payment.charge, booking.save`. `TestBook_PaymentFailureRollsBackReservations` is the central mid-failure assertion: after the charge fails the log must read `..., hotel.cancel, flight.cancel`, proving both reservations were compensated and in reverse. `TestBook_HotelFullRollsBackFlight` proves an earlier-step rollback — the flight is cancelled and payment is never reached. `TestBook_SaveFailureRefundsAndCancels` proves the external charge is refunded when the final persistence fails. The rest pin invalid-request rejection (no collaborator touched) and the nil-dependency guard.

Create `booking_test.go`:

```go
package booking

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// callLog records the order of side effects so a test can assert that
// compensation ran, and ran in reverse.
type callLog struct {
	mu     sync.Mutex
	events []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, s)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

type mockFlights struct {
	log     *callLog
	failNow bool
	seq     int
}

func (m *mockFlights) Reserve(_ context.Context, _, _ string) (string, error) {
	if m.failNow {
		return "", ErrFlightFull
	}
	m.seq++
	m.log.add("flight.reserve")
	return "fl-1", nil
}

func (m *mockFlights) Cancel(_ context.Context, _ string) error {
	m.log.add("flight.cancel")
	return nil
}

type mockHotels struct {
	log     *callLog
	failNow bool
}

func (m *mockHotels) Reserve(_ context.Context, _, _ string) (string, error) {
	if m.failNow {
		return "", ErrHotelFull
	}
	m.log.add("hotel.reserve")
	return "ho-1", nil
}

func (m *mockHotels) Cancel(_ context.Context, _ string) error {
	m.log.add("hotel.cancel")
	return nil
}

type mockPayments struct {
	log     *callLog
	failNow bool
}

func (m *mockPayments) Charge(_ context.Context, _ string, _ float64) (string, error) {
	if m.failNow {
		return "", ErrPaymentDeclined
	}
	m.log.add("payment.charge")
	return "txn-1", nil
}

func (m *mockPayments) Refund(_ context.Context, _ string) error {
	m.log.add("payment.refund")
	return nil
}

type mockBookings struct {
	log     *callLog
	failNow bool
	saved   []*Booking
}

func (m *mockBookings) NextID(_ context.Context) (string, error) { return "BK-1", nil }

func (m *mockBookings) Save(_ context.Context, b *Booking) error {
	if m.failNow {
		return errors.New("db: write failed")
	}
	m.log.add("booking.save")
	m.saved = append(m.saved, b)
	return nil
}

func validRequest() BookRequest {
	return BookRequest{UserID: "u1", FlightID: "AA100", HotelID: "RITZ", Amount: 540}
}

func TestBook_HappyPath(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	flights := &mockFlights{log: log}
	hotels := &mockHotels{log: log}
	payments := &mockPayments{log: log}
	bookings := &mockBookings{log: log}
	svc, err := NewBookingService(flights, hotels, payments, bookings)
	if err != nil {
		t.Fatalf("NewBookingService: %v", err)
	}

	b, err := svc.Book(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if b.Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", b.Status)
	}
	if b.TxnID != "txn-1" {
		t.Errorf("txn = %q, want txn-1", b.TxnID)
	}
	want := []string{"flight.reserve", "hotel.reserve", "payment.charge", "booking.save"}
	got := log.snapshot()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func TestBook_PaymentFailureRollsBackReservations(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	flights := &mockFlights{log: log}
	hotels := &mockHotels{log: log}
	payments := &mockPayments{log: log, failNow: true}
	bookings := &mockBookings{log: log}
	svc, _ := NewBookingService(flights, hotels, payments, bookings)

	_, err := svc.Book(context.Background(), validRequest())
	if !errors.Is(err, ErrPaymentDeclined) {
		t.Fatalf("err = %v, want ErrPaymentDeclined", err)
	}
	got := log.snapshot()
	want := []string{"flight.reserve", "hotel.reserve", "hotel.cancel", "flight.cancel"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (compensation must run in reverse)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v (compensation must run in reverse)", got, want)
		}
	}
	if len(bookings.saved) != 0 {
		t.Errorf("saved = %d, want 0", len(bookings.saved))
	}
}

func TestBook_HotelFullRollsBackFlight(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	flights := &mockFlights{log: log}
	hotels := &mockHotels{log: log, failNow: true}
	payments := &mockPayments{log: log}
	bookings := &mockBookings{log: log}
	svc, _ := NewBookingService(flights, hotels, payments, bookings)

	_, err := svc.Book(context.Background(), validRequest())
	if !errors.Is(err, ErrHotelFull) {
		t.Fatalf("err = %v, want ErrHotelFull", err)
	}
	got := log.snapshot()
	want := []string{"flight.reserve", "flight.cancel"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func TestBook_SaveFailureRefundsAndCancels(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	flights := &mockFlights{log: log}
	hotels := &mockHotels{log: log}
	payments := &mockPayments{log: log}
	bookings := &mockBookings{log: log, failNow: true}
	svc, _ := NewBookingService(flights, hotels, payments, bookings)

	_, err := svc.Book(context.Background(), validRequest())
	if err == nil {
		t.Fatal("expected an error when Save fails")
	}
	got := log.snapshot()
	want := []string{"flight.reserve", "hotel.reserve", "payment.charge", "payment.refund", "hotel.cancel", "flight.cancel"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func TestBook_RejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	flights := &mockFlights{log: log}
	hotels := &mockHotels{log: log}
	payments := &mockPayments{log: log}
	bookings := &mockBookings{log: log}
	svc, _ := NewBookingService(flights, hotels, payments, bookings)

	_, err := svc.Book(context.Background(), BookRequest{UserID: "", FlightID: "AA100", HotelID: "RITZ"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if len(log.snapshot()) != 0 {
		t.Errorf("no collaborator should be touched for invalid input: %v", log.snapshot())
	}
}

func TestNewBookingService_RejectsNilDependencies(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	_, err := NewBookingService(nil, &mockHotels{log: log}, &mockPayments{log: log}, &mockBookings{log: log})
	if err == nil {
		t.Error("expected error for nil flights repository")
	}
}
```

## Review

The saga is correct when the forward path reads as the business process and every failure path leaves the world as if `Book` had never been called. Confirm `Saga.Run` records each completed step and, on the first failure, compensates the recorded steps in reverse — the payment-failure test fails the instant compensation runs in forward order or skips a step. Confirm each step threads its state through captured variables: the flight step stores the reservation id its compensation later cancels, which is why the closures, not a fixed interface, carry the undo data. Confirm the external charge is refunded when a *later* step (the save) fails — that is the whole reason the charge is modeled as a compensable step rather than a final commit.

The mistakes to avoid: rolling back only the most recent step, which strands earlier reservations; compensating in forward order, which can undo a dependency before the thing that depends on it; and returning a compensation failure as the operation's error, which masks the real cause and hides the stranded resource an operator must clean up by hand. The deeper trap is reaching for a database transaction across services that cannot enlist in one — you cannot `ROLLBACK` an HTTP charge, so the saga's explicit compensation is not a workaround but the correct model. Order the steps so the cheapest, most-reversible work happens first and the irreversible external call happens late, against work already secured.

## Resources

- [Saga pattern (microservices.io)](https://microservices.io/patterns/data/saga.html) — Chris Richardson's canonical description of maintaining data consistency across services with a sequence of local transactions and compensating transactions.
- [Saga distributed transactions pattern (Microsoft Azure Architecture Center)](https://learn.microsoft.com/en-us/azure/architecture/patterns/saga) — when to choose a saga over a distributed transaction, and the orchestration-versus-choreography trade-off behind this orchestrated `Saga.Run`.
- [Compensating Transaction pattern (Microsoft Azure Architecture Center)](https://learn.microsoft.com/en-us/azure/architecture/patterns/compensating-transaction) — why a compensation is a semantic undo rather than a rollback, and why it must tolerate its own failures.
- [Martin Fowler: Service Layer](https://martinfowler.com/eaaCatalog/serviceLayer.html) — the layer that "coordinates the application's response in each operation," which a saga is one disciplined way to implement.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-validation-and-error-mapping.md](03-validation-and-error-mapping.md) | Next: [05-idempotent-registration.md](05-idempotent-registration.md)
