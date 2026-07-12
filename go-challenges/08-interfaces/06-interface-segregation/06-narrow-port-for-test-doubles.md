# Exercise 6: A Narrow Charger Port That Makes a Payment Fake Trivial

A checkout flow's only real need from a payment provider is: charge this amount
with this idempotency key. Yet the vendor SDK client exposes 20-plus methods —
refunds, disputes, payouts, webhooks, balances. This module defines a one-method
`Charger` at the consumer, adapts the fat SDK to it, and shows the payoff: the
checkout test uses a five-line fake instead of stubbing twenty methods with
panics.

## What you'll build

```text
checkout/                      independent module: example.com/checkout
  go.mod                       go 1.24
  charger.go                   Charger (Charge only); ChargeID; sentinel errors
  sdk.go                       fat vendorClient (20+ methods) adapted to Charger
  checkout.go                  Checkout uses Charger; dedupes by idempotency key
  cmd/
    demo/
      main.go                  charges once, retries same key, prints deduped result
  checkout_test.go             5-line fakeCharger: success, error propagation, dedup
```

Files: `charger.go`, `sdk.go`, `checkout.go`, `cmd/demo/main.go`, `checkout_test.go`.
Implement: `Charger interface { Charge(ctx, amount, idempotencyKey) (ChargeID, error) }`, a fat SDK adapted to it, and a `Checkout` that dedupes duplicate idempotency keys.
Test: a `fakeCharger` recording the last key and returning a canned id or configurable error; assert success, error propagation, and dedup; `var _ Charger` on the fake.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/06-interface-segregation/06-narrow-port-for-test-doubles/cmd/demo
cd go-solutions/08-interfaces/06-interface-segregation/06-narrow-port-for-test-doubles
go mod edit -go=1.24
```

### One method at the consumer, adapter at the boundary

The checkout code declares `Charger` with the single method it calls. The vendor
SDK — modeled by `vendorClient` with a realistic pile of unrelated methods —
does not satisfy `Charger` directly because its charging method has a different
signature (SDK APIs rarely match your port exactly). So a thin adapter,
`sdkCharger`, wraps the client and implements `Charge` by translating to the
SDK's call. This is the idiomatic move: the fat third-party type stays at the
edge behind an adapter, and everything inward depends on the one-method port.

The payoff is the test. Because `Checkout` depends on `Charger`, the test double
is a struct with one method that records the key it saw and returns a canned
result. Contrast the alternative: if `Checkout` depended on the SDK client
directly, its test would need a fake implementing all 20-plus methods, 19 of
them panic-stubs that exist only to satisfy the interface and are a runtime
hazard if ever hit. The narrow port deletes all of that.

`Checkout` also demonstrates idempotency, a real payment concern: it remembers
which idempotency keys it has already charged and returns the prior `ChargeID`
for a duplicate key instead of charging twice. Errors from the charger are
wrapped with `%w` so the caller can match the sentinel `ErrCardDeclined` with
`errors.Is`.

Create `charger.go`:

```go
package checkout

import (
	"context"
	"errors"
)

// ChargeID identifies a completed charge.
type ChargeID string

// ErrCardDeclined is the sentinel a charger returns when the card is declined.
var ErrCardDeclined = errors.New("card declined")

// Charger is the ONLY capability checkout needs from a payment provider.
// One method, declared at the consumer.
type Charger interface {
	Charge(ctx context.Context, amountCents int64, idempotencyKey string) (ChargeID, error)
}
```

Create `sdk.go`. The fat client and its narrow adapter:

```go
package checkout

import (
	"context"
	"fmt"
)

// vendorClient models a real payment SDK: many methods, only one of which
// checkout cares about. None of the others should reach the checkout code.
type vendorClient struct {
	declineAll bool
	counter    int
}

// CreatePaymentIntent is the SDK's charging primitive; note its signature does
// not match Charger, which is why an adapter is needed.
func (c *vendorClient) CreatePaymentIntent(_ context.Context, cents int64, key string) (string, error) {
	if c.declineAll {
		return "", ErrCardDeclined
	}
	c.counter++
	return fmt.Sprintf("pi_%s_%d", key, c.counter), nil
}

// The rest of the fat surface: checkout depends on none of these.
func (c *vendorClient) Refund(context.Context, string) error { return nil }

func (c *vendorClient) CreateDispute(context.Context, string) error { return nil }

func (c *vendorClient) ListPayouts(context.Context) ([]string, error) { return nil, nil }

func (c *vendorClient) RegisterWebhook(context.Context, string) error { return nil }

func (c *vendorClient) GetBalance(context.Context) (int64, error) { return 0, nil }

// sdkCharger adapts the fat client to the narrow Charger port.
type sdkCharger struct {
	client *vendorClient
}

// newSDKCharger returns a Charger backed by the vendor SDK.
func newSDKCharger(c *vendorClient) Charger {
	return &sdkCharger{client: c}
}

func (a *sdkCharger) Charge(ctx context.Context, cents int64, key string) (ChargeID, error) {
	id, err := a.client.CreatePaymentIntent(ctx, cents, key)
	if err != nil {
		return "", fmt.Errorf("charge %d: %w", cents, err)
	}
	return ChargeID(id), nil
}

var _ Charger = (*sdkCharger)(nil)
```

Create `checkout.go`:

```go
package checkout

import (
	"context"
	"fmt"
	"sync"
)

// Checkout charges customers, deduplicating by idempotency key so a retried
// request never double-charges. It depends only on Charger.
type Checkout struct {
	charger Charger

	mu   sync.Mutex
	seen map[string]ChargeID
}

// NewCheckout wires a Charger into the flow.
func NewCheckout(c Charger) *Checkout {
	return &Checkout{charger: c, seen: make(map[string]ChargeID)}
}

// Pay charges amountCents once per idempotency key. A duplicate key returns the
// prior ChargeID without charging again.
func (co *Checkout) Pay(ctx context.Context, amountCents int64, idempotencyKey string) (ChargeID, error) {
	co.mu.Lock()
	if prior, ok := co.seen[idempotencyKey]; ok {
		co.mu.Unlock()
		return prior, nil
	}
	co.mu.Unlock()

	id, err := co.charger.Charge(ctx, amountCents, idempotencyKey)
	if err != nil {
		return "", fmt.Errorf("pay: %w", err)
	}

	co.mu.Lock()
	co.seen[idempotencyKey] = id
	co.mu.Unlock()
	return id, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`. Expose a constructor that wires the real SDK adapter.

Add to `sdk.go`:

```go
// NewLiveCheckout builds a Checkout backed by the vendor SDK adapter, for demos
// and production wiring.
func NewLiveCheckout() *Checkout {
	return NewCheckout(newSDKCharger(&vendorClient{}))
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/checkout"
)

func main() {
	ctx := context.Background()
	co := checkout.NewLiveCheckout()

	first, _ := co.Pay(ctx, 1999, "order-42")
	fmt.Printf("first charge: %s\n", first)

	// Same idempotency key: deduplicated, no second charge.
	retry, _ := co.Pay(ctx, 1999, "order-42")
	fmt.Printf("retry charge: %s\n", retry)
	fmt.Printf("deduped: %t\n", first == retry)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first charge: pi_order-42_1
retry charge: pi_order-42_1
deduped: true
```

### Tests

The whole argument of this exercise is the size of the fake. `fakeCharger` is a
handful of lines with one method; it records the last idempotency key and returns
a configurable id or error. That is all the checkout test needs.

Create `checkout_test.go`:

```go
package checkout

import (
	"context"
	"errors"
	"testing"
)

// fakeCharger is the ENTIRE test double: one method, a recorded key, a canned
// result. No panic-stubs for refunds/disputes/payouts.
type fakeCharger struct {
	lastKey string
	id      ChargeID
	err     error
	calls   int
}

func (f *fakeCharger) Charge(_ context.Context, _ int64, key string) (ChargeID, error) {
	f.calls++
	f.lastKey = key
	return f.id, f.err
}

var _ Charger = (*fakeCharger)(nil)

func TestPaySuccess(t *testing.T) {
	t.Parallel()

	fc := &fakeCharger{id: "ch_1"}
	co := NewCheckout(fc)

	id, err := co.Pay(context.Background(), 500, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ch_1" {
		t.Fatalf("id = %q, want ch_1", id)
	}
	if fc.lastKey != "key-1" {
		t.Fatalf("lastKey = %q, want key-1", fc.lastKey)
	}
}

func TestPayPropagatesDecline(t *testing.T) {
	t.Parallel()

	fc := &fakeCharger{err: ErrCardDeclined}
	co := NewCheckout(fc)

	_, err := co.Pay(context.Background(), 500, "key-2")
	if !errors.Is(err, ErrCardDeclined) {
		t.Fatalf("err = %v, want ErrCardDeclined", err)
	}
}

func TestPayDeduplicatesByIdempotencyKey(t *testing.T) {
	t.Parallel()

	fc := &fakeCharger{id: "ch_9"}
	co := NewCheckout(fc)
	ctx := context.Background()

	first, err := co.Pay(ctx, 700, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := co.Pay(ctx, 700, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("dedup failed: %q != %q", first, second)
	}
	if fc.calls != 1 {
		t.Fatalf("charger called %d times, want 1 (duplicate must not re-charge)", fc.calls)
	}
}
```

## Review

The narrow port is doing its job when the checkout test's fake is five lines and
has exactly one method — that is the direct, measurable payoff of segregation.
The trap the exercise avoids is depending on the vendor SDK type directly, which
would force the test to stub twenty methods with panics that are pure noise and a
latent crash. Keep the fat SDK behind an adapter at the boundary and depend
inward only on `Charger`. The dedup test pins a real payment invariant: a retried
request with the same idempotency key must not charge twice, verified by
asserting the charger was called exactly once. Run `go test -race` because the
`seen` map is read and written under concurrent checkouts.

## Resources

- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- [errors package (Is, wrapping with %w)](https://pkg.go.dev/errors)
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces)
- [Interface Segregation Principle](https://en.wikipedia.org/wiki/Interface_segregation_principle)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-read-only-cache-view.md](05-read-only-cache-view.md) | Next: [07-compose-role-interfaces.md](07-compose-role-interfaces.md)
