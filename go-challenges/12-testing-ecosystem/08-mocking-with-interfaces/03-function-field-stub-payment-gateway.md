# Exercise 3: Configurable Stub via Function Fields — A Payment Charge Flow

When a test needs a *different* canned response per scenario — success here,
decline there, a transient failure that must trigger a retry — writing a new stub
type for each is tedious. The idiom is a single configurable stub whose behavior is
a *function field*: each test assigns the exact `ChargeFunc` it wants. No mocking
library, full control, and a call counter for idempotency assertions.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
checkout/                    independent module: example.com/checkout
  go.mod                     go 1.26
  checkout.go                PaymentGateway port; Order; Service; Pay with one retry; typed errors
  cmd/
    demo/
      main.go                runnable demo: a paid order and a declined order
  checkout_test.go           func-field stub + atomic counter; success/decline/retry/cancel tests
```

- Files: `checkout.go`, `cmd/demo/main.go`, `checkout_test.go`.
- Implement: `Service.Pay(ctx, order, token)` calling `PaymentGateway.Charge`; on a *transient* error retry exactly once; map a decline to a domain error; respect context cancellation.
- Test: a `stubGateway` whose `ChargeFunc` field is set per scenario, with an `atomic.Int64` call counter; assert paid-on-success, unpaid-on-decline, exactly-two-charges-on-retry, and one-charge-on-cancel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/08-mocking-with-interfaces/03-function-field-stub-payment-gateway/cmd/demo
cd go-solutions/12-testing-ecosystem/08-mocking-with-interfaces/03-function-field-stub-payment-gateway
```

### Why a function field beats a bespoke stub per test

The gateway is a real external dependency: a payment processor reached over the
network. It fails in several distinct, meaningful ways, and the service must handle
each differently — a *decline* is a business outcome (the card was refused; do not
retry, tell the caller), while a *transient* network error is worth one retry. To
test all of that you need the stub to return a different thing each time, and
sometimes a different thing *on the second call than the first* (fail once, then
succeed). A plain struct with fixed fields cannot express "fail the first time,
succeed the second." A function field can: the test supplies a closure that closes
over a counter and decides per call.

The retry policy is deliberately narrow and typed. The port returns typed errors so
the service can distinguish them: `ErrCardDeclined` (do not retry) versus
`ErrTransient` (retry once). Crucially, the service checks `ctx.Err()` before the
retry, so a cancelled or timed-out context aborts *without* a second attempt — the
counter test proves the gateway was hit exactly once in that case. This
exactly-once / exactly-twice counting is genuine interaction-as-contract: there is
no state on the service that records "charged twice," only the call count.

Create `checkout.go`:

```go
package checkout

import (
	"context"
	"errors"
	"fmt"
)

// Typed gateway errors let the service branch on the failure kind.
var (
	// ErrCardDeclined is a business outcome: do not retry.
	ErrCardDeclined = errors.New("card declined")
	// ErrTransient is a retryable infrastructure failure.
	ErrTransient = errors.New("transient gateway error")
	// ErrPaymentFailed wraps a terminal payment failure for the caller.
	ErrPaymentFailed = errors.New("payment failed")
)

// ChargeID identifies a successful charge at the processor.
type ChargeID string

// PaymentGateway is the one-method port to the payment processor.
type PaymentGateway interface {
	Charge(ctx context.Context, amountCents int64, token string) (ChargeID, error)
}

// Order carries the amount and the result of a payment attempt.
type Order struct {
	AmountCents int64
	Paid        bool
	ChargeID    ChargeID
}

// Service runs the checkout charge flow with a single retry on transient errors.
type Service struct {
	gw PaymentGateway
}

// New injects the gateway through the constructor.
func New(gw PaymentGateway) *Service {
	return &Service{gw: gw}
}

// Pay charges the order. A decline is terminal; a transient error is retried once;
// a cancelled context aborts before any retry. On success the order is marked paid.
func (s *Service) Pay(ctx context.Context, order *Order, token string) error {
	const maxAttempts = 2
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		id, err := s.gw.Charge(ctx, order.AmountCents, token)
		switch {
		case err == nil:
			order.Paid = true
			order.ChargeID = id
			return nil
		case errors.Is(err, ErrCardDeclined):
			return fmt.Errorf("%w: %w", ErrPaymentFailed, err)
		case errors.Is(err, ErrTransient):
			lastErr = err
			continue // retry
		default:
			return err
		}
	}

	return fmt.Errorf("%w: %w", ErrPaymentFailed, lastErr)
}
```

### The runnable demo

The demo wires two fixed stubs — one that always succeeds and one that always
declines — to show both terminal outcomes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/checkout"
)

// alwaysOK charges successfully. alwaysDeclined refuses the card.
type alwaysOK struct{}

func (alwaysOK) Charge(context.Context, int64, string) (checkout.ChargeID, error) {
	return "ch_123", nil
}

type alwaysDeclined struct{}

func (alwaysDeclined) Charge(context.Context, int64, string) (checkout.ChargeID, error) {
	return "", checkout.ErrCardDeclined
}

func main() {
	ctx := context.Background()

	paid := &checkout.Order{AmountCents: 4999}
	if err := checkout.New(alwaysOK{}).Pay(ctx, paid, "tok_visa"); err == nil {
		fmt.Printf("order 1: paid=%v charge=%s\n", paid.Paid, paid.ChargeID)
	}

	declined := &checkout.Order{AmountCents: 4999}
	err := checkout.New(alwaysDeclined{}).Pay(ctx, declined, "tok_bad")
	fmt.Printf("order 2: paid=%v declined=%v\n", declined.Paid, errors.Is(err, checkout.ErrCardDeclined))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order 1: paid=true charge=ch_123
order 2: paid=false declined=true
```

### The configurable stub and the tests

`stubGateway` has two fields: `ChargeFunc`, the closure each test installs, and
`calls`, an `atomic.Int64` counter incremented on every invocation. Because
`atomic.Int64` is already goroutine-safe, no mutex is needed even though the type
would be safe to share. Each test writes exactly the behavior it needs:

- success returns a `ChargeID`; the service marks the order paid; the counter is 1.
- decline returns `ErrCardDeclined`; the order stays unpaid; the error unwraps to
  `ErrCardDeclined`; the counter is 1 (no retry on a business decline).
- the retry scenario returns `ErrTransient` on the first call and success on the
  second (the closure branches on the counter); the counter ends at 2 and the order
  is paid.
- the cancel scenario cancels the context before `Pay`; the service's `ctx.Err()`
  guard aborts before any charge, so the counter is 0.

Create `checkout_test.go`:

```go
package checkout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// stubGateway is a configurable stub: behavior is the ChargeFunc field, and every
// call bumps an atomic counter for idempotency/retry assertions.
type stubGateway struct {
	ChargeFunc func(ctx context.Context, amountCents int64, token string) (ChargeID, error)
	calls      atomic.Int64
}

func (s *stubGateway) Charge(ctx context.Context, amountCents int64, token string) (ChargeID, error) {
	s.calls.Add(1)
	return s.ChargeFunc(ctx, amountCents, token)
}

func (s *stubGateway) Calls() int64 { return s.calls.Load() }

func TestPaySuccess(t *testing.T) {
	t.Parallel()
	gw := &stubGateway{ChargeFunc: func(context.Context, int64, string) (ChargeID, error) {
		return "ch_ok", nil
	}}
	order := &Order{AmountCents: 1000}
	if err := New(gw).Pay(context.Background(), order, "tok"); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if !order.Paid || order.ChargeID != "ch_ok" {
		t.Fatalf("order = %+v, want paid with ch_ok", *order)
	}
	if n := gw.Calls(); n != 1 {
		t.Fatalf("Charge called %d times, want 1", n)
	}
}

func TestPayDeclinedIsTerminal(t *testing.T) {
	t.Parallel()
	gw := &stubGateway{ChargeFunc: func(context.Context, int64, string) (ChargeID, error) {
		return "", ErrCardDeclined
	}}
	order := &Order{AmountCents: 1000}
	err := New(gw).Pay(context.Background(), order, "tok")
	if !errors.Is(err, ErrCardDeclined) {
		t.Fatalf("err = %v, want ErrCardDeclined", err)
	}
	if !errors.Is(err, ErrPaymentFailed) {
		t.Fatalf("err = %v, want it to wrap ErrPaymentFailed", err)
	}
	if order.Paid {
		t.Fatal("order marked paid after decline")
	}
	if n := gw.Calls(); n != 1 {
		t.Fatalf("Charge called %d times, want 1 (no retry on decline)", n)
	}
}

func TestPayRetriesOnceOnTransient(t *testing.T) {
	t.Parallel()
	gw := &stubGateway{}
	gw.ChargeFunc = func(context.Context, int64, string) (ChargeID, error) {
		if gw.Calls() == 1 { // first call: transient failure
			return "", ErrTransient
		}
		return "ch_retry", nil // second call: success
	}
	order := &Order{AmountCents: 1000}
	if err := New(gw).Pay(context.Background(), order, "tok"); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if !order.Paid {
		t.Fatal("order not paid after successful retry")
	}
	if n := gw.Calls(); n != 2 {
		t.Fatalf("Charge called %d times, want exactly 2", n)
	}
}

func TestPayAbortsOnCancelledContext(t *testing.T) {
	t.Parallel()
	gw := &stubGateway{ChargeFunc: func(context.Context, int64, string) (ChargeID, error) {
		return "ch_never", nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Pay

	order := &Order{AmountCents: 1000}
	if err := New(gw).Pay(ctx, order, "tok"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := gw.Calls(); n != 0 {
		t.Fatalf("Charge called %d times, want 0 on cancelled context", n)
	}
}
```

## Review

The charge flow is correct when the outcome branches on the *kind* of gateway
error: success marks the order paid, a decline is terminal and wraps
`ErrPaymentFailed`, a transient error is retried exactly once, and a cancelled
context aborts before any attempt. The function-field stub is what makes all four
provable from one type — the retry test in particular could not be written with a
fixed-value stub, because it must return different results on the first and second
call. The counter turns "retried once" and "aborted before charging" into
assertions rather than hopes. The trap to avoid is retrying a decline: a business
refusal is not a transient fault, and hammering the processor with retries on a
declined card is both wrong and abusive — the decline test pins the count at 1. Run
`go test -race`; the `atomic.Int64` counter must be race-clean under it.

## Resources

- [context](https://pkg.go.dev/context) — `context.WithCancel` and the `ctx.Err()` guard the retry loop checks.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the race-free call counter.
- [errors: wrapping with %w](https://pkg.go.dev/errors) — the `fmt.Errorf("%w: %w", ...)` double-wrap the decline path uses.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-gomock-generated-order-repository.md](04-gomock-generated-order-repository.md)
