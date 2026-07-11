# Exercise 10: A Narrow Seam at an External Dependency

The one place an interface almost always earns its keep is a process boundary you
must fake: an external service you cannot call in a unit test. This module builds
an order service that depends on a two-method `PaymentGateway` interface —
declared in the order package, consumer-side — wrapping a real `*StripeClient`
that returns a concrete type. A hand-written `fakeGateway` drives the retry and
idempotency logic without touching the network.

## What you'll build

```text
payments/                   independent module: example.com/payments
  go.mod                    go 1.26
  gateway.go                PaymentGateway interface{Authorize;Capture}; sentinel errors; types
  service.go                OrderService: retry with one idempotency key, no double Capture
  stripe.go                 concrete *StripeClient over net/http; var _ PaymentGateway
  cmd/
    demo/
      main.go               a demo gateway that fails once then succeeds
  service_test.go           fakeGateway: retry-then-succeed; permanent decline; no double capture
```

- Files: `gateway.go`, `service.go`, `stripe.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a consumer-side `PaymentGateway interface { Authorize; Capture }`, an `OrderService` that retries a transient `Authorize` failure with the same idempotency key and captures exactly once, and a concrete `*StripeClient` returned (never an interface) by its constructor.
- Test: a `fakeGateway` that fails `Authorize` once then succeeds — assert the service retries with the same idempotency key and does not double-`Capture`; a second fake returning a permanent `ErrDeclined` — assert the service surfaces it without retrying.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/payments/cmd/demo
cd ~/go-exercises/payments
go mod init example.com/payments
```

### Why this is where an interface belongs

Every earlier module argued against interfaces. This one shows the case that
passes the cost/benefit test cleanly. The order service must talk to a payment
provider over the network. In a unit test you cannot — and must not — hit the real
Stripe API: it is slow, non-deterministic, and charges money. So there is a
genuine boundary you must fake, which is exactly the second of the three
questions ("is there a boundary you must fake?") that justifies an interface. The
`PaymentGateway` interface is that seam.

Two disciplines keep the seam clean. First, it is narrow: the service calls only
`Authorize` and `Capture`, so the interface has only those two methods — not the
dozens the real Stripe client exposes. Second, it is consumer-side: the interface
is declared in the order package that uses it, while the real client
(`*StripeClient`) is a concrete type its constructor returns. The order package
depends on its own two-method abstraction; the Stripe client knows nothing about
it and satisfies it implicitly. `var _ PaymentGateway = (*StripeClient)(nil)` pins
that the real client still fits.

The logic under test is the reason the seam matters: retrying a transient
authorization failure safely. The service retries `Authorize` on a transient
error, but always with the SAME idempotency key, so the provider deduplicates and
the customer is never double-charged. On a permanent decline (`ErrDeclined`) it
does not retry — retrying a decline is pointless and wastes a round trip. And it
calls `Capture` exactly once, after a successful authorization, never in a retry
loop. A `fakeGateway` programmed to fail once then succeed lets the test assert
all three properties deterministically: the retry happened, the key was stable,
and capture ran once.

Create `gateway.go`:

```go
package payments

import (
	"context"
	"errors"
)

// ErrDeclined is a permanent failure: the payment was refused and must not be
// retried.
var ErrDeclined = errors.New("payments: declined")

// ErrTransient is a temporary failure that may succeed on retry.
var ErrTransient = errors.New("payments: transient failure")

// AuthRequest is an authorization request. IdempotencyKey must be stable across
// retries so the provider deduplicates.
type AuthRequest struct {
	IdempotencyKey string
	AmountCents    int
}

// AuthResult is a successful authorization.
type AuthResult struct {
	AuthID string
}

// PaymentGateway is the consumer-side seam: exactly the two methods OrderService
// calls, declared in the package that uses them.
type PaymentGateway interface {
	Authorize(ctx context.Context, req AuthRequest) (AuthResult, error)
	Capture(ctx context.Context, authID string) error
}
```

Create `service.go`:

```go
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OrderService charges an order through a PaymentGateway. It accepts the narrow
// interface (accept interfaces) so it can be faked at the boundary.
type OrderService struct {
	gateway    PaymentGateway
	maxRetries int
	backoff    time.Duration
}

// NewOrderService returns a concrete *OrderService.
func NewOrderService(gw PaymentGateway, maxRetries int, backoff time.Duration) *OrderService {
	return &OrderService{gateway: gw, maxRetries: maxRetries, backoff: backoff}
}

// Charge authorizes then captures a payment. It retries a transient Authorize
// failure with the SAME idempotency key, does not retry a decline, and captures
// exactly once.
func (s *OrderService) Charge(ctx context.Context, idempotencyKey string, amountCents int) (string, error) {
	req := AuthRequest{IdempotencyKey: idempotencyKey, AmountCents: amountCents}

	var auth AuthResult
	var err error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		auth, err = s.gateway.Authorize(ctx, req) // same req => same idempotency key
		if err == nil {
			break
		}
		if errors.Is(err, ErrDeclined) {
			return "", fmt.Errorf("charge %q: %w", idempotencyKey, ErrDeclined)
		}
		// Transient: wait and retry with the identical request.
		if attempt < s.maxRetries {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(s.backoff):
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("charge %q: authorize failed after %d attempts: %w",
			idempotencyKey, s.maxRetries+1, err)
	}

	// Capture exactly once, outside the retry loop.
	if err := s.gateway.Capture(ctx, auth.AuthID); err != nil {
		return "", fmt.Errorf("charge %q: capture: %w", idempotencyKey, err)
	}
	return auth.AuthID, nil
}
```

Create `stripe.go`:

```go
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// StripeClient is the real, concrete gateway over net/http. Its constructor
// returns *StripeClient (return structs); the order package depends on the
// PaymentGateway interface, which this type satisfies implicitly.
type StripeClient struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

func NewStripeClient(baseURL, apiKey string) *StripeClient {
	return &StripeClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
}

func (c *StripeClient) Authorize(ctx context.Context, req AuthRequest) (AuthResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return AuthResult{}, fmt.Errorf("stripe: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/authorizations", bytes.NewReader(body))
	if err != nil {
		return AuthResult{}, fmt.Errorf("stripe: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return AuthResult{}, fmt.Errorf("stripe: authorize: %w", ErrTransient)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out AuthResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return AuthResult{}, fmt.Errorf("stripe: decode: %w", err)
		}
		return out, nil
	case http.StatusPaymentRequired:
		return AuthResult{}, ErrDeclined
	default:
		return AuthResult{}, fmt.Errorf("stripe: status %d: %w", resp.StatusCode, ErrTransient)
	}
}

func (c *StripeClient) Capture(ctx context.Context, authID string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/authorizations/"+authID+"/capture", nil)
	if err != nil {
		return fmt.Errorf("stripe: new request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("stripe: capture: %w", ErrTransient)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stripe: capture status %d: %w", resp.StatusCode, ErrTransient)
	}
	return nil
}

// Compile-time proof the concrete client satisfies the consumer-side seam.
var _ PaymentGateway = (*StripeClient)(nil)
```

### The runnable demo

The demo defines a small in-process gateway that fails the first `Authorize` and
succeeds the second, so you can watch the retry-then-capture flow without a
network.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/payments"
)

// demoGateway fails Authorize once, then succeeds. It implements the exported
// PaymentGateway interface from package main -- no mock framework.
type demoGateway struct {
	authCalls    int
	captureCalls int
	keys         []string
}

func (g *demoGateway) Authorize(ctx context.Context, req payments.AuthRequest) (payments.AuthResult, error) {
	g.authCalls++
	g.keys = append(g.keys, req.IdempotencyKey)
	if g.authCalls == 1 {
		return payments.AuthResult{}, payments.ErrTransient
	}
	return payments.AuthResult{AuthID: "auth_123"}, nil
}

func (g *demoGateway) Capture(ctx context.Context, authID string) error {
	g.captureCalls++
	return nil
}

func main() {
	gw := &demoGateway{}
	svc := payments.NewOrderService(gw, 3, 0)

	authID, err := svc.Charge(context.Background(), "order-42", 4999)
	if err != nil {
		panic(err)
	}

	fmt.Printf("authID=%s\n", authID)
	fmt.Printf("authorize calls=%d capture calls=%d\n", gw.authCalls, gw.captureCalls)
	fmt.Printf("same idempotency key each attempt=%v\n", gw.keys[0] == gw.keys[1])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
authID=auth_123
authorize calls=2 capture calls=1
same idempotency key each attempt=true
```

### Tests

`fakeGateway` records every call so the test can assert retry count, idempotency
key stability, and that `Capture` ran exactly once. `TestRetryThenSucceed` fails
`Authorize` once and asserts the retry and single capture. `TestPermanentDecline`
returns `ErrDeclined` and asserts no retry and no capture.
`ExampleOrderService_Charge` pins the happy-path output so `go test` verifies the
snippet too.

Create `service_test.go`:

```go
package payments

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeGateway is the hand-written fake at the boundary. It records calls so the
// test can assert the service's retry and idempotency behavior.
type fakeGateway struct {
	authErrs     []error // returned in order, one per Authorize call
	authCalls    int
	captureCalls int
	seenKeys     []string
	capturedIDs  []string
}

func (g *fakeGateway) Authorize(ctx context.Context, req AuthRequest) (AuthResult, error) {
	g.seenKeys = append(g.seenKeys, req.IdempotencyKey)
	i := g.authCalls
	g.authCalls++
	if i < len(g.authErrs) && g.authErrs[i] != nil {
		return AuthResult{}, g.authErrs[i]
	}
	return AuthResult{AuthID: "auth_ok"}, nil
}

func (g *fakeGateway) Capture(ctx context.Context, authID string) error {
	g.captureCalls++
	g.capturedIDs = append(g.capturedIDs, authID)
	return nil
}

func TestRetryThenSucceed(t *testing.T) {
	t.Parallel()

	gw := &fakeGateway{authErrs: []error{ErrTransient}} // fail once, then succeed
	svc := NewOrderService(gw, 3, 0)

	authID, err := svc.Charge(context.Background(), "order-1", 1000)
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if authID != "auth_ok" {
		t.Fatalf("authID = %q, want auth_ok", authID)
	}
	if gw.authCalls != 2 {
		t.Fatalf("authorize calls = %d, want 2 (one retry)", gw.authCalls)
	}
	if gw.captureCalls != 1 {
		t.Fatalf("capture calls = %d, want 1 (no double capture)", gw.captureCalls)
	}
	for _, k := range gw.seenKeys {
		if k != "order-1" {
			t.Fatalf("idempotency key drifted: got %q, want order-1", k)
		}
	}
}

func TestPermanentDecline(t *testing.T) {
	t.Parallel()

	gw := &fakeGateway{authErrs: []error{ErrDeclined}}
	svc := NewOrderService(gw, 3, 0)

	_, err := svc.Charge(context.Background(), "order-2", 1000)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("Charge err = %v, want errors.Is(_, ErrDeclined)", err)
	}
	if gw.authCalls != 1 {
		t.Fatalf("authorize calls = %d, want 1 (decline is not retried)", gw.authCalls)
	}
	if gw.captureCalls != 0 {
		t.Fatalf("capture calls = %d, want 0 (declined, never captured)", gw.captureCalls)
	}
}

func TestExhaustsRetriesOnPersistentTransient(t *testing.T) {
	t.Parallel()

	// Every attempt is transient; with maxRetries=2 that is 3 attempts total.
	gw := &fakeGateway{authErrs: []error{ErrTransient, ErrTransient, ErrTransient}}
	svc := NewOrderService(gw, 2, 0)

	_, err := svc.Charge(context.Background(), "order-3", 1000)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Charge err = %v, want errors.Is(_, ErrTransient)", err)
	}
	if gw.authCalls != 3 {
		t.Fatalf("authorize calls = %d, want 3", gw.authCalls)
	}
	if gw.captureCalls != 0 {
		t.Fatalf("capture calls = %d, want 0", gw.captureCalls)
	}
}

// ExampleOrderService_Charge drives the service through the fake gateway on the
// happy path; the // Output line is auto-verified by `go test`.
func ExampleOrderService_Charge() {
	gw := &fakeGateway{}
	svc := NewOrderService(gw, 3, 0)
	authID, _ := svc.Charge(context.Background(), "order-1", 1000)
	fmt.Println(authID)
	// Output: auth_ok
}
```

## Review

This is the interface the earlier modules were saving you for: a boundary you
must fake, described by exactly the two methods the service calls, declared in the
consumer, satisfied implicitly by a concrete `*StripeClient`. The tests prove the
properties that matter in production — a transient failure is retried with a
stable idempotency key so no customer is double-charged, a decline is not retried,
and `Capture` runs exactly once — all without a network call, because the seam is
fakeable. Draw the interface here, and only here: at the process edge, as narrow
as the caller's usage. Do not extend it to mirror the real Stripe client's full
surface; the service depends on two methods, so the seam is two methods wide.

## Resources

- [Go Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — consumer-side, minimal interfaces; return concrete types.
- [Stripe API — Idempotent requests](https://docs.stripe.com/api/idempotent_requests) — why the same idempotency key across retries prevents double charges.
- [net/http — Client and NewRequestWithContext](https://pkg.go.dev/net/http#Client) — the concrete client the seam wraps.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-generics-over-boxed-interface.md](09-generics-over-boxed-interface.md) | Next: [../13-designing-a-plugin-system/00-concepts.md](../13-designing-a-plugin-system/00-concepts.md)
