# Exercise 2: An Anti-Corruption Layer Over A Payment Gateway

The previous exercise adapted SDKs that, however different, all returned an idiomatic Go `error`. This one adapts a vendor whose contract is genuinely alien to Go: it speaks in integer cents, names its method `Submit`, and never returns an `error` at all. Failure is packed into a numeric `StatusCode` field on the result struct, the way many older or non-Go-native payment SDKs actually behave. The adapter you build is an anti-corruption layer: it translates the vendor's units into your `Money` type and, more importantly, translates the vendor's status codes into Go's error model so the rest of your program can branch on `errors.Is` and recover structure with `errors.As`.

This module is fully self-contained. It begins with its own `go mod init`, simulates the vendor SDK in its own sub-package, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
payment.go                        Money, Receipt, PaymentProcessor port; DeclineError; PayProAdapter
thirdparty/
  paypro/paypro.go                vendor SDK: minor units in, StatusCode out, never an error
cmd/
  demo/main.go                    one approved charge, then four translated failures
payment_test.go                   approval round-trip, decline sentinel + code, status mapping
```

- Files: `thirdparty/paypro/paypro.go`, `payment.go`, `cmd/demo/main.go`, `payment_test.go`.
- Implement: the `PaymentProcessor` port; `Money` with a validated minor-unit conversion; `DeclineError` that implements `Is`; `PayProAdapter` whose `Charge` maps every vendor `StatusCode` to a domain error.
- Test: an approved charge returns a `Receipt`, a decline matches `ErrDeclined` and exposes its code, each failure status maps to its sentinel, bad money and a cancelled context are rejected before the vendor is called, and an unknown status falls back to a catch-all sentinel.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

### A vendor that does not believe in errors

The simulated gateway is deliberately un-Go-like. `Submit` takes a `ChargeInput` whose amount is `AmountMinor int64` — cents, not a money type — and returns a `ChargeOutput` in *every* case, success or failure, with no second `error` return value. The caller is expected to read `Accepted` and `StatusCode` to find out what happened. A missing API key yields `4010`, an outage (modelled by a `Down` flag) yields `5000`, a non-positive amount yields `4000`, an amount over a ceiling yields `4020` ("insufficient funds"), and anything else is approved with `Accepted: true` and `StatusCode: 2000`.

This is the shape that makes an anti-corruption layer earn its name. If you let `ChargeOutput` flow into your domain, every caller would have to remember that `2000` means success and `4020` means a decline, and a forgotten check would read a declined charge as settled money. The adapter exists to make that impossible.

Create `thirdparty/paypro/paypro.go`:

```go
// Package paypro simulates an incompatible third-party payment SDK. It speaks
// in integer minor units, names its method Submit, and signals failure through
// a numeric StatusCode field rather than a Go error.
package paypro

// ChargeInput is the vendor's request shape: amount in minor units (cents),
// currency as an ISO 4217 code, an opaque customer reference, and an
// idempotency key the vendor uses to dedupe retried charges.
type ChargeInput struct {
	AmountMinor int64
	CurrencyISO string
	CustomerRef string
	IdemKey     string
}

// ChargeOutput is the vendor's response shape. The vendor never returns a Go
// error; Accepted plus StatusCode/StatusText carry the outcome.
type ChargeOutput struct {
	Accepted   bool
	Reference  string
	AuthCode   string
	StatusCode int
	StatusText string
}

// Gateway is the vendor client. Down simulates an upstream outage.
type Gateway struct {
	APIKey string
	Down   bool
}

// Submit attempts a charge. It returns a ChargeOutput in every case and never a
// Go error: the caller must inspect StatusCode to learn what happened.
func (g *Gateway) Submit(in ChargeInput) ChargeOutput {
	if g.APIKey == "" {
		return ChargeOutput{StatusCode: 4010, StatusText: "unauthorized: missing api key"}
	}
	if g.Down {
		return ChargeOutput{StatusCode: 5000, StatusText: "gateway temporarily unavailable"}
	}
	if in.AmountMinor <= 0 {
		return ChargeOutput{StatusCode: 4000, StatusText: "amount must be positive"}
	}
	if in.AmountMinor > 1_000_000 {
		return ChargeOutput{StatusCode: 4020, StatusText: "insufficient funds"}
	}
	return ChargeOutput{
		Accepted:   true,
		StatusCode: 2000,
		StatusText: "approved",
		Reference:  "pp_" + in.IdemKey,
		AuthCode:   "AUTH-" + in.CurrencyISO,
	}
}
```

### The domain side: Money, a structured decline, and the port

The domain models money the way the business thinks about it, not the way the vendor stores it. `Money` carries a `Currency`, a whole part, and a 0..99 `Cents` part, and the conversion to the vendor's flat minor-unit integer lives in the unexported `minorUnits` method. That method is the single place the `whole*100 + cents` arithmetic exists, and it bounds-checks the inputs — a 120-cent value or a negative amount is rejected with `ErrInvalidAmount` *before* any network call. Putting the conversion in the adapter's territory means a caller can never accidentally send dollars where the gateway wanted cents.

`DeclineError` is the centerpiece of the failure translation. A declined charge needs to satisfy two different callers: code that wants to branch on "was this a decline?" with `errors.Is(err, ErrDeclined)`, and code that wants the vendor's reason code for logging or a retry decision with `errors.As(err, &de)`. A single value serves both because `DeclineError` implements `Is(target error) bool` returning `target == ErrDeclined`. That one method makes every `*DeclineError` match the `ErrDeclined` sentinel while still carrying its structured `Code` and `Reason`. This is the idiomatic way to give a custom error type a sentinel identity without wrapping.

`PaymentProcessor` is the port — `Charge(ctx, customerID, amount) (Receipt, error)` — and `Receipt` is a domain-only success type with no `paypro` fields anywhere in it. The `var _ PaymentProcessor = (*PayProAdapter)(nil)` line is a compile-time assertion that the adapter actually satisfies the port; if a signature drifts, the build fails here rather than at some distant call site.

The adapter's `Charge` reads top to bottom as a translation pipeline. It first honors the context (a cancelled context returns before any work), then validates the domain inputs, then converts `Money` to the vendor's `ChargeInput`, calls `Submit`, and finally branches on the result: `Accepted` becomes a `Receipt`, and anything else is routed through `translateStatus`, the `switch` that turns each numeric code into a domain error with a catch-all `default` so a status the vendor adds tomorrow surfaces as `ErrUnknownStatus` instead of being silently swallowed.

Create `payment.go`:

```go
package payments

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"example.com/payments/thirdparty/paypro"
)

var (
	ErrInvalidAmount       = errors.New("payments: invalid amount")
	ErrMissingCurrency     = errors.New("payments: currency is required")
	ErrDeclined            = errors.New("payments: charge declined")
	ErrGatewayUnauthorized = errors.New("payments: gateway rejected credentials")
	ErrGatewayUnavailable  = errors.New("payments: gateway unavailable")
	ErrUnknownStatus       = errors.New("payments: unknown gateway status")
	ErrMissingClient       = errors.New("payments: gateway client is required")
)

// Money is the domain's representation of an amount: a whole-unit part, a
// 0..99 minor part, and an ISO 4217 currency. The vendor speaks only in minor
// units, so converting Money is the adapter's job, not the caller's.
type Money struct {
	Currency string
	Whole    int64
	Cents    int8
}

func (m Money) minorUnits() (int64, error) {
	if m.Cents < 0 || m.Cents >= 100 {
		return 0, fmt.Errorf("%w: cents must be in 0..99, got %d", ErrInvalidAmount, m.Cents)
	}
	if m.Whole < 0 {
		return 0, fmt.Errorf("%w: whole units must be non-negative, got %d", ErrInvalidAmount, m.Whole)
	}
	return m.Whole*100 + int64(m.Cents), nil
}

// Receipt is the domain's successful result. It carries no vendor types.
type Receipt struct {
	TransactionID string
	AuthCode      string
	AmountMinor   int64
	Currency      string
}

// PaymentProcessor is the domain port. Callers depend on this, never on paypro.
type PaymentProcessor interface {
	Charge(ctx context.Context, customerID string, amount Money) (Receipt, error)
}

// DeclineError is a structured domain error: it both matches the ErrDeclined
// sentinel via errors.Is and exposes the vendor's reason code via errors.As.
type DeclineError struct {
	Code   int
	Reason string
}

func (e *DeclineError) Error() string {
	return fmt.Sprintf("payments: declined (code=%d): %s", e.Code, e.Reason)
}

// Is lets errors.Is(err, ErrDeclined) succeed for any DeclineError.
func (e *DeclineError) Is(target error) bool {
	return target == ErrDeclined
}

// PayProAdapter adapts *paypro.Gateway to the PaymentProcessor port.
type PayProAdapter struct {
	gateway *paypro.Gateway
}

func NewPayProAdapter(gw *paypro.Gateway) (*PayProAdapter, error) {
	if gw == nil {
		return nil, ErrMissingClient
	}
	return &PayProAdapter{gateway: gw}, nil
}

func (a *PayProAdapter) Charge(ctx context.Context, customerID string, amount Money) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if amount.Currency == "" {
		return Receipt{}, ErrMissingCurrency
	}
	minor, err := amount.minorUnits()
	if err != nil {
		return Receipt{}, err
	}

	out := a.gateway.Submit(paypro.ChargeInput{
		AmountMinor: minor,
		CurrencyISO: amount.Currency,
		CustomerRef: customerID,
		IdemKey:     customerID + "-" + strconv.FormatInt(minor, 10),
	})
	if out.Accepted {
		return Receipt{
			TransactionID: out.Reference,
			AuthCode:      out.AuthCode,
			AmountMinor:   minor,
			Currency:      amount.Currency,
		}, nil
	}
	return Receipt{}, translateStatus(out)
}

// translateStatus maps the vendor's numeric status codes onto domain errors.
// This is the heart of the anti-corruption layer: numbers in, Go errors out.
func translateStatus(out paypro.ChargeOutput) error {
	switch out.StatusCode {
	case 4000:
		return fmt.Errorf("%w: %s", ErrInvalidAmount, out.StatusText)
	case 4010:
		return fmt.Errorf("%w (code=%d)", ErrGatewayUnauthorized, out.StatusCode)
	case 4020:
		return &DeclineError{Code: out.StatusCode, Reason: out.StatusText}
	case 5000:
		return fmt.Errorf("%w (code=%d): %s", ErrGatewayUnavailable, out.StatusCode, out.StatusText)
	default:
		return fmt.Errorf("%w: code=%d text=%q", ErrUnknownStatus, out.StatusCode, out.StatusText)
	}
}

var _ PaymentProcessor = (*PayProAdapter)(nil)
```

### A runnable demo

The demo runs one approved charge to show the `Receipt`, then triggers each failure category: a decline (recovered as a `*DeclineError` with its code), an outage, bad credentials, and an out-of-range cents value. Every failure line confirms the relevant `errors.Is` match, which is the whole promise of the anti-corruption layer — the caller never touches a `paypro` type.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/payments"
	"example.com/payments/thirdparty/paypro"
)

func main() {
	ctx := context.Background()
	proc, err := payments.NewPayProAdapter(&paypro.Gateway{APIKey: "live_key"})
	if err != nil {
		fmt.Println("setup error:", err)
		return
	}

	rec, err := proc.Charge(ctx, "cust-42", payments.Money{Currency: "USD", Whole: 19, Cents: 99})
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("approved txn=%s auth=%s minor=%d %s\n", rec.TransactionID, rec.AuthCode, rec.AmountMinor, rec.Currency)

	fmt.Println("--- failure translation ---")

	_, err = proc.Charge(ctx, "cust-42", payments.Money{Currency: "USD", Whole: 20000})
	var de *payments.DeclineError
	if errors.As(err, &de) {
		fmt.Printf("declined: is(ErrDeclined)=%v code=%d reason=%q\n", errors.Is(err, payments.ErrDeclined), de.Code, de.Reason)
	}

	down, _ := payments.NewPayProAdapter(&paypro.Gateway{APIKey: "live_key", Down: true})
	_, err = down.Charge(ctx, "cust-42", payments.Money{Currency: "USD", Whole: 5})
	fmt.Printf("outage: is(ErrGatewayUnavailable)=%v\n", errors.Is(err, payments.ErrGatewayUnavailable))

	noKey, _ := payments.NewPayProAdapter(&paypro.Gateway{})
	_, err = noKey.Charge(ctx, "cust-42", payments.Money{Currency: "USD", Whole: 5})
	fmt.Printf("bad creds: is(ErrGatewayUnauthorized)=%v\n", errors.Is(err, payments.ErrGatewayUnauthorized))

	_, err = proc.Charge(ctx, "cust-42", payments.Money{Currency: "USD", Whole: 1, Cents: 120})
	fmt.Printf("bad money: is(ErrInvalidAmount)=%v\n", errors.Is(err, payments.ErrInvalidAmount))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
approved txn=pp_cust-42-1999 auth=AUTH-USD minor=1999 USD
--- failure translation ---
declined: is(ErrDeclined)=true code=4020 reason="insufficient funds"
outage: is(ErrGatewayUnavailable)=true
bad creds: is(ErrGatewayUnauthorized)=true
bad money: is(ErrInvalidAmount)=true
```

### Tests

The tests pin both halves of the translation. `TestCharge_ApprovedReturnsReceipt` proves the unit conversion: `Whole: 19, Cents: 99` becomes `1999` minor units and shows up in both the `Receipt.AmountMinor` and the vendor-derived `TransactionID`. `TestCharge_DeclineMatchesSentinelAndCarriesCode` is the key test for `DeclineError`: it asserts the same error satisfies `errors.Is(err, ErrDeclined)` and `errors.As(err, &de)` with `de.Code == 4020`. The remaining tests map each failure status to its sentinel, confirm bad cents and a missing currency are rejected before the vendor is touched, confirm a cancelled context short-circuits, and call `translateStatus` directly with an invented `9999` status to prove the catch-all `default` returns `ErrUnknownStatus`.

Create `payment_test.go`:

```go
package payments

import (
	"context"
	"errors"
	"testing"

	"example.com/payments/thirdparty/paypro"
)

func TestCharge_ApprovedReturnsReceipt(t *testing.T) {
	t.Parallel()

	a, err := NewPayProAdapter(&paypro.Gateway{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewPayProAdapter: %v", err)
	}
	rec, err := a.Charge(context.Background(), "cust-1", Money{Currency: "USD", Whole: 19, Cents: 99})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if rec.AmountMinor != 1999 {
		t.Errorf("AmountMinor = %d, want 1999", rec.AmountMinor)
	}
	if rec.TransactionID != "pp_cust-1-1999" {
		t.Errorf("TransactionID = %q", rec.TransactionID)
	}
	if rec.AuthCode != "AUTH-USD" {
		t.Errorf("AuthCode = %q", rec.AuthCode)
	}
}

func TestCharge_DeclineMatchesSentinelAndCarriesCode(t *testing.T) {
	t.Parallel()

	a, _ := NewPayProAdapter(&paypro.Gateway{APIKey: "k"})
	_, err := a.Charge(context.Background(), "cust-2", Money{Currency: "USD", Whole: 20000})
	if err == nil {
		t.Fatal("expected decline, got nil")
	}
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("errors.Is(err, ErrDeclined) = false, err = %v", err)
	}
	var de *DeclineError
	if !errors.As(err, &de) {
		t.Fatalf("errors.As to *DeclineError failed, err = %v", err)
	}
	if de.Code != 4020 {
		t.Errorf("Code = %d, want 4020", de.Code)
	}
}

func TestCharge_UnauthorizedMapsToSentinel(t *testing.T) {
	t.Parallel()

	a, _ := NewPayProAdapter(&paypro.Gateway{})
	_, err := a.Charge(context.Background(), "cust-3", Money{Currency: "USD", Whole: 5})
	if !errors.Is(err, ErrGatewayUnauthorized) {
		t.Fatalf("err = %v, want ErrGatewayUnauthorized", err)
	}
}

func TestCharge_UnavailableMapsToSentinel(t *testing.T) {
	t.Parallel()

	a, _ := NewPayProAdapter(&paypro.Gateway{APIKey: "k", Down: true})
	_, err := a.Charge(context.Background(), "cust-4", Money{Currency: "USD", Whole: 5})
	if !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("err = %v, want ErrGatewayUnavailable", err)
	}
}

func TestCharge_ZeroAmountIsInvalid(t *testing.T) {
	t.Parallel()

	a, _ := NewPayProAdapter(&paypro.Gateway{APIKey: "k"})
	_, err := a.Charge(context.Background(), "cust-5", Money{Currency: "USD"})
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("err = %v, want ErrInvalidAmount", err)
	}
}

func TestCharge_RejectsBadCentsBeforeCallingVendor(t *testing.T) {
	t.Parallel()

	a, _ := NewPayProAdapter(&paypro.Gateway{APIKey: "k"})
	_, err := a.Charge(context.Background(), "cust-6", Money{Currency: "USD", Whole: 1, Cents: 120})
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("err = %v, want ErrInvalidAmount", err)
	}
}

func TestCharge_RejectsMissingCurrency(t *testing.T) {
	t.Parallel()

	a, _ := NewPayProAdapter(&paypro.Gateway{APIKey: "k"})
	_, err := a.Charge(context.Background(), "cust-7", Money{Whole: 5, Cents: 0})
	if !errors.Is(err, ErrMissingCurrency) {
		t.Fatalf("err = %v, want ErrMissingCurrency", err)
	}
}

func TestCharge_HonorsCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, _ := NewPayProAdapter(&paypro.Gateway{APIKey: "k"})
	_, err := a.Charge(ctx, "cust-8", Money{Currency: "USD", Whole: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewPayProAdapter_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := NewPayProAdapter(nil)
	if !errors.Is(err, ErrMissingClient) {
		t.Fatalf("err = %v, want ErrMissingClient", err)
	}
}

func TestTranslateStatus_UnknownCodeFallsBack(t *testing.T) {
	t.Parallel()

	err := translateStatus(paypro.ChargeOutput{StatusCode: 9999, StatusText: "teapot"})
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("err = %v, want ErrUnknownStatus", err)
	}
}
```

## Review

The anti-corruption layer is correct when no `paypro` type and no raw status number ever crosses into the domain. Every successful charge comes back as a `Receipt` built from domain fields; every failure comes back as one of the package sentinels or a `*DeclineError`. The `var _ PaymentProcessor = (*PayProAdapter)(nil)` assertion guarantees the adapter keeps satisfying the port as both evolve. The unit conversion is owned in one place, `minorUnits`, and validated before the network call, so a malformed `Money` never reaches the gateway.

The mistakes this exercise guards against are specific to a non-idiomatic vendor. The first and worst is treating the absence of a Go `error` as success: because `Submit` always returns a `ChargeOutput`, a naive adapter that ignores `StatusCode` would read a `4020` decline as a settled payment. The fix is to branch on the vendor's real success signal (`Accepted`) and translate every other code. The second is collapsing a decline into a bare sentinel and losing the reason code; `DeclineError` keeps both the sentinel identity (through its `Is` method) and the structured `Code` (through `errors.As`). The third is leaving the `switch` without a `default`, so a status the vendor introduces later is silently swallowed; the catch-all `ErrUnknownStatus` turns an unknown code into a visible failure. Running `go test -race ./...` exercises every status branch, including the `default`, which the test reaches by calling `translateStatus` directly.

## Resources

- [`errors.As` and custom `Is`](https://pkg.go.dev/errors#As) — the standard-library contract for type-recovering errors and for a custom `Is` method, both of which `DeclineError` relies on.
- [Anti-Corruption Layer pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/anti-corruption-layer) — the architectural pattern this exercise implements, with the same "translate at the boundary" rationale.
- [Stripe API error handling](https://docs.stripe.com/error-handling) — how a real payment API classifies failures (card errors, declines, rate limits), the categories your translation `switch` is modelled on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-notifier-adapters.md](01-notifier-adapters.md) | Next: [03-io-stream-adapters.md](03-io-stream-adapters.md)
