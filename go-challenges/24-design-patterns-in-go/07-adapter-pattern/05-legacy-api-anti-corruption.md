# Exercise 5: An Anti-Corruption Layer Over A Messy Versioned Billing API

The previous exercises adapted clean vendor SDKs. Real integrations are rarely clean: the third-party billing provider names a customer `Cust`, sends money as a floating-point dollar amount, lowercases the currency, reports a decline as a numeric result code buried in a success response, and -- to make migration interesting -- ships a second API version that disagrees with the first on every one of those choices. This exercise builds an anti-corruption layer (ACL): one clean domain `Charger` port, and two *versioned adapters* that translate the v1 and v2 vendor APIs into it. Because both adapters satisfy the same port, the domain migrates from v1 to v2 without changing a line of caller code.

This module is fully self-contained. It begins with its own `go mod init`, simulates the messy vendor in its own sub-package, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
billing.go                        Money, ChargeRequest/Result; Charger port; domain errors; DeclineError
adapters.go                       V1Adapter, V2Adapter; toV1Request/toV2Input translation helpers
legacy/legacy.go                  simulated vendor: v1 (dollars/float, RC code) and v2 (cents/int, Outcome string)
cmd/
  demo/main.go                    one domain function drives both v1 and v2 unchanged
billing_test.go                   asserts unit/currency translation and decline/error via errors.Is/As
```

- Files: `legacy/legacy.go`, `billing.go`, `adapters.go`, `cmd/demo/main.go`, `billing_test.go`.
- Implement: the `Charger` port and domain error vocabulary; `V1Adapter` (cents to dollars, lowercased currency, RC-code mapping) and `V2Adapter` (Outcome-string mapping), both translating the vendor's in-band failures into `ErrDeclined`/`ErrUpstream`/`ErrUnsupportedCurrency` and a structured `DeclineError`.
- Test: that the same domain request approves on both versions, that a decline surfaces as `ErrDeclined` and a recoverable `*DeclineError`, that a processor failure is `ErrUpstream`, that a transport failure keeps the vendor sentinel reachable, and that the unit/currency translation is exact.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

### The vendor we do not control

This is the corruption the layer exists to contain, so build it first and read how hostile it is. Both client versions return a Go `error` only for a genuine transport failure (`ErrTransport`); every business outcome -- approval, decline, processor error, bad currency -- is encoded *in-band* in a field of an otherwise-successful response. That is the status-code-as-error model, and it is exactly what a domain must never be forced to speak. v1 uses abbreviated field names, amounts in dollars as a `float64`, a lowercased currency, and a numeric `RC` result code. v2 "modernizes" to different names again, amounts in minor units as `int64`, an uppercased ISO code, and an `Outcome` string with a separate decline code. Neither is our domain.
Create `legacy/legacy.go`:

```go
// Package legacy simulates a messy third-party billing vendor with two API
// versions. Field names, units, currency casing, and the failure model all
// differ from our domain - and differ between v1 and v2.
package legacy

import "errors"

// ErrTransport is the ONLY thing either client returns as a Go error: a
// genuine network/transport failure. Business outcomes (declines, validation)
// are reported in-band through response fields, status-code-as-error style.
var ErrTransport = errors.New("legacy: transport failure")

// --- v1: the original, ugliest API ---

// V1Request uses abbreviated names and amounts in DOLLARS as a float. Currency
// arrives lowercased ("usd"), because v1 never normalized it.
type V1Request struct {
	Cust string
	Amt  float64
	Cur  string
	Memo string
}

// V1Response reports the outcome through RC (result code): 0=approved,
// 1=declined, 2=processor error. RCReason carries a short code on failure.
type V1Response struct {
	Txn      string
	RC       int
	RCReason string
}

type V1Client struct {
	Endpoint string
}

// DoCharge returns a Go error only for transport problems; everything else is
// encoded in V1Response.RC.
func (c *V1Client) DoCharge(r V1Request) (V1Response, error) {
	if c.Endpoint == "" {
		return V1Response{}, ErrTransport
	}
	switch {
	case r.Cur != "usd":
		return V1Response{RC: 2, RCReason: "unsupported_currency"}, nil
	case r.Amt <= 0:
		return V1Response{RC: 2, RCReason: "invalid_amount"}, nil
	case r.Cust == "cust_declined":
		return V1Response{RC: 1, RCReason: "insufficient_funds"}, nil
	case r.Cust == "cust_processor_down":
		return V1Response{RC: 2, RCReason: "processor_unavailable"}, nil
	}
	return V1Response{Txn: "v1-txn-001", RC: 0}, nil
}

// --- v2: the "modernized" API, still not our domain ---

// V2ChargeInput uses different names again, amounts in MINOR units (cents) as
// int64, an uppercased ISO code, and an idempotency key.
type V2ChargeInput struct {
	AccountRef string
	MinorUnits int64
	ISOCode    string
	IdemKey    string
}

// V2ChargeOutput reports outcome as a STRING, with a separate decline code.
type V2ChargeOutput struct {
	ID          string
	Outcome     string
	DeclineCode string
}

type V2Client struct {
	BaseURL string
}

// Charge mirrors DoCharge: transport errors only; business outcome in Outcome.
func (c *V2Client) Charge(in V2ChargeInput) (V2ChargeOutput, error) {
	if c.BaseURL == "" {
		return V2ChargeOutput{}, ErrTransport
	}
	switch {
	case in.ISOCode != "USD":
		return V2ChargeOutput{Outcome: "failed", DeclineCode: "currency_not_supported"}, nil
	case in.MinorUnits <= 0:
		return V2ChargeOutput{Outcome: "failed", DeclineCode: "amount_invalid"}, nil
	case in.AccountRef == "acct_declined":
		return V2ChargeOutput{Outcome: "declined", DeclineCode: "do_not_honor"}, nil
	case in.AccountRef == "acct_down":
		return V2ChargeOutput{Outcome: "failed", DeclineCode: "gateway_timeout"}, nil
	}
	return V2ChargeOutput{ID: "v2-ch-777", Outcome: "approved"}, nil
}
```

### The clean domain port

The domain side is everything the vendor is not. `Money` carries minor units as an `int64` and an uppercase ISO currency, so there is no float rounding and no casing ambiguity. `Charger` is the port, phrased in domain words, and it is the only type the rest of the application depends on. The error vocabulary is the contract callers match on: `ErrInvalidRequest` for a request the domain itself rejects, `ErrUnsupportedCurrency` for a currency neither side handles, `ErrDeclined` for a refused charge, and `ErrUpstream` for a processor failure. `DeclineError` is a structured decline a caller can pull out with `errors.As` to read the machine reason, and its `Is` method reports it as `ErrDeclined` so a caller that only cares "was it declined?" can use the cheaper `errors.Is`. Validation runs in the domain, before any vendor call, so an empty customer or a non-USD currency never reaches the wire.
Create `billing.go`:

```go
package billing

import (
	"context"
	"errors"
	"fmt"
)

// Money is the domain's value type: a currency-tagged amount in MINOR units
// (cents). The domain never deals in floats, and never in a vendor's casing.
type Money struct {
	Currency string // ISO 4217, uppercase, e.g. "USD"
	Minor    int64  // amount in minor units, e.g. 1999 == $19.99
}

// ChargeRequest and ChargeResult are the clean domain contract.
type ChargeRequest struct {
	CustomerID string
	Amount     Money
	Reference  string
}

type ChargeResult struct {
	TransactionID string
}

// Charger is the port. Both versioned adapters implement it, so the domain can
// migrate v1 -> v2 without changing a line of caller code.
type Charger interface {
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

// Domain error vocabulary. The whole point of the ACL is that callers match on
// THESE, never on a vendor's RC int or Outcome string.
var (
	ErrInvalidRequest      = errors.New("billing: invalid request")
	ErrUnsupportedCurrency = errors.New("billing: unsupported currency")
	ErrDeclined            = errors.New("billing: charge declined")
	ErrUpstream            = errors.New("billing: upstream processor error")
)

// DeclineError is a structured decline a caller can reach with errors.As to
// read the machine-readable reason. It reports as ErrDeclined via Is.
type DeclineError struct {
	Reason string
}

func (e *DeclineError) Error() string {
	return fmt.Sprintf("billing: declined (%s)", e.Reason)
}

func (e *DeclineError) Is(target error) bool {
	return target == ErrDeclined
}

func validate(req ChargeRequest) error {
	if req.CustomerID == "" {
		return fmt.Errorf("%w: customer id is required", ErrInvalidRequest)
	}
	if req.Amount.Minor <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if req.Amount.Currency != "USD" {
		return fmt.Errorf("%w: %s", ErrUnsupportedCurrency, req.Amount.Currency)
	}
	return nil
}
```

### The two versioned adapters

Each adapter is a complete anti-corruption layer over one vendor version, and both satisfy the single `Charger` port -- the compile-time `var _ Charger = ...` lines prove it. The translation has two halves. On the way in, a small pure helper (`toV1Request`, `toV2Input`) builds the vendor's wire request from the domain request: `toV1Request` divides minor units by 100 to get the vendor's dollar float and lowercases the currency, while `toV2Input` is mostly a field rename because v2 already speaks minor units. Pulling request-building into a pure function is what makes the unit and currency translation directly testable without a live client.

On the way out, each adapter turns the vendor's in-band outcome into a domain result or a domain error. `V1Adapter` switches on the numeric `RC`: 0 is an approval carrying the transaction id, 1 becomes a `*DeclineError` with the vendor's reason, and anything else is an `ErrUpstream` -- except an `unsupported_currency` reason, which the layer surfaces as `ErrUnsupportedCurrency` so the domain sees a precise cause. `V2Adapter` does the same against the `Outcome` string. A transport error from either client is wrapped with two `%w` verbs so the result is both `ErrUpstream` and the original `legacy.ErrTransport`, keeping the cause visible to a log while the domain matches on the class.
Create `adapters.go`:

```go
package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"example.com/billing/legacy"
)

// V1Adapter is the anti-corruption layer over the v1 vendor API. It converts
// cents to dollars, lowercases the currency, and maps the RC result code onto
// the domain error vocabulary.
type V1Adapter struct {
	client *legacy.V1Client
}

func NewV1Adapter(client *legacy.V1Client) (*V1Adapter, error) {
	if client == nil {
		return nil, errors.New("billing: v1 client is required")
	}
	return &V1Adapter{client: client}, nil
}

func (a *V1Adapter) Charge(_ context.Context, req ChargeRequest) (ChargeResult, error) {
	if err := validate(req); err != nil {
		return ChargeResult{}, err
	}
	resp, err := a.client.DoCharge(toV1Request(req))
	if err != nil {
		// Only transport failures arrive as a Go error here.
		return ChargeResult{}, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	switch resp.RC {
	case 0:
		return ChargeResult{TransactionID: resp.Txn}, nil
	case 1:
		return ChargeResult{}, &DeclineError{Reason: resp.RCReason}
	default:
		if resp.RCReason == "unsupported_currency" {
			return ChargeResult{}, fmt.Errorf("%w: rejected by v1", ErrUnsupportedCurrency)
		}
		return ChargeResult{}, fmt.Errorf("%w: rc=%d reason=%s", ErrUpstream, resp.RC, resp.RCReason)
	}
}

// V2Adapter is the anti-corruption layer over the v2 vendor API. The domain
// already speaks minor units, so no float conversion is needed, but the
// Outcome string still has to be translated.
type V2Adapter struct {
	client *legacy.V2Client
}

func NewV2Adapter(client *legacy.V2Client) (*V2Adapter, error) {
	if client == nil {
		return nil, errors.New("billing: v2 client is required")
	}
	return &V2Adapter{client: client}, nil
}

func (a *V2Adapter) Charge(_ context.Context, req ChargeRequest) (ChargeResult, error) {
	if err := validate(req); err != nil {
		return ChargeResult{}, err
	}
	out, err := a.client.Charge(toV2Input(req))
	if err != nil {
		return ChargeResult{}, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	switch out.Outcome {
	case "approved":
		return ChargeResult{TransactionID: out.ID}, nil
	case "declined":
		return ChargeResult{}, &DeclineError{Reason: out.DeclineCode}
	default: // "failed"
		if out.DeclineCode == "currency_not_supported" {
			return ChargeResult{}, fmt.Errorf("%w: rejected by v2", ErrUnsupportedCurrency)
		}
		return ChargeResult{}, fmt.Errorf("%w: outcome=%s code=%s", ErrUpstream, out.Outcome, out.DeclineCode)
	}
}

// toV1Request builds the v1 wire request from a domain request: minor units
// become dollars, and the ISO currency is lowercased to match v1's casing.
func toV1Request(req ChargeRequest) legacy.V1Request {
	return legacy.V1Request{
		Cust: req.CustomerID,
		Amt:  float64(req.Amount.Minor) / 100.0,
		Cur:  strings.ToLower(req.Amount.Currency),
		Memo: req.Reference,
	}
}

// toV2Input builds the v2 wire request: v2 already speaks minor units and
// uppercase ISO codes, so this is mostly a field rename.
func toV2Input(req ChargeRequest) legacy.V2ChargeInput {
	return legacy.V2ChargeInput{
		AccountRef: req.CustomerID,
		MinorUnits: req.Amount.Minor,
		ISOCode:    req.Amount.Currency,
		IdemKey:    req.Reference,
	}
}

// Compile-time proof that both versioned adapters satisfy the same port.
var (
	_ Charger = (*V1Adapter)(nil)
	_ Charger = (*V2Adapter)(nil)
)
```

### A runnable demo

The payoff of the ACL is that ordinary domain code depends only on `Charger`. The `runCharges` function below is written once and run against both the v1 and the v2 adapter unchanged; the only version-specific detail is which customer id the simulated vendor treats as declined, which a real system would not have. It exercises an approval, a decline read through `errors.As`, an unsupported currency, and a transport failure that keeps the vendor sentinel reachable.
Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"example.com/billing"
	"example.com/billing/legacy"
)

// runCharges is ordinary domain code. It depends only on the billing.Charger
// port, so the SAME function drives both the v1 and the v2 vendor.
func runCharges(label string, c billing.Charger) {
	ctx := context.Background()
	fmt.Printf("== %s ==\n", label)

	res, err := c.Charge(ctx, billing.ChargeRequest{
		CustomerID: "cust_ok",
		Amount:     billing.Money{Currency: "USD", Minor: 1999},
		Reference:  "order-42",
	})
	if err != nil {
		fmt.Printf("approved charge failed: %v\n", err)
	} else {
		fmt.Printf("approved txn=%s\n", res.TransactionID)
	}

	_, err = c.Charge(ctx, billing.ChargeRequest{
		CustomerID: declinedID(label),
		Amount:     billing.Money{Currency: "USD", Minor: 500},
	})
	var de *billing.DeclineError
	switch {
	case errors.As(err, &de):
		fmt.Printf("declined is=%v reason=%s\n", errors.Is(err, billing.ErrDeclined), de.Reason)
	default:
		fmt.Printf("unexpected decline result: %v\n", err)
	}

	_, err = c.Charge(ctx, billing.ChargeRequest{
		CustomerID: "cust_ok",
		Amount:     billing.Money{Currency: "EUR", Minor: 1000},
	})
	fmt.Printf("eur unsupported=%v\n", errors.Is(err, billing.ErrUnsupportedCurrency))
}

func declinedID(label string) string {
	if label == "v2" {
		return "acct_declined"
	}
	return "cust_declined"
}

func main() {
	v1, err := billing.NewV1Adapter(&legacy.V1Client{Endpoint: "https://v1.example"})
	if err != nil {
		log.Fatalf("v1: %v", err)
	}
	v2, err := billing.NewV2Adapter(&legacy.V2Client{BaseURL: "https://v2.example"})
	if err != nil {
		log.Fatalf("v2: %v", err)
	}

	runCharges("v1", v1)
	runCharges("v2", v2)

	fmt.Println("== transport failure ==")
	broken, _ := billing.NewV1Adapter(&legacy.V1Client{})
	_, err = broken.Charge(context.Background(), billing.ChargeRequest{
		CustomerID: "cust_ok",
		Amount:     billing.Money{Currency: "USD", Minor: 100},
	})
	fmt.Printf("upstream=%v vendor_sentinel=%v\n",
		errors.Is(err, billing.ErrUpstream), errors.Is(err, legacy.ErrTransport))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
== v1 ==
approved txn=v1-txn-001
declined is=true reason=insufficient_funds
eur unsupported=true
== v2 ==
approved txn=v2-ch-777
declined is=true reason=do_not_honor
eur unsupported=true
== transport failure ==
upstream=true vendor_sentinel=true
```

### Tests

The tests pin both directions of the translation. `TestCharge_ApprovedAcrossVersions` and `TestCharge_RejectsUnsupportedCurrencyBeforeCallingVendor` run one domain request against both adapters and assert identical domain behavior, which is the property that makes the v1-to-v2 migration safe. `TestCharge_DeclinedTranslatesToDomainError` asserts a decline is reachable both as `ErrDeclined` (via `errors.Is`, through `DeclineError.Is`) and as a recoverable `*DeclineError` carrying the vendor reason (via `errors.As`). `TestCharge_ProcessorErrorIsUpstream` and `TestCharge_TransportErrorWrapsVendorSentinel` separate a business processor error from a transport failure and confirm the vendor sentinel survives the wrap. `TestTranslation_UnitsAndCurrency` asserts the exact cents-to-dollars and currency-casing conversion on the pure helpers.
Create `billing_test.go`:

```go
package billing

import (
	"context"
	"errors"
	"testing"

	"example.com/billing/legacy"
)

func usd(minor int64) Money { return Money{Currency: "USD", Minor: minor} }

// bothAdapters returns the v1 and v2 adapters wired to healthy clients, so a
// single test body can assert identical domain behavior across versions.
func bothAdapters(t *testing.T) map[string]Charger {
	t.Helper()
	v1, err := NewV1Adapter(&legacy.V1Client{Endpoint: "https://v1.example"})
	if err != nil {
		t.Fatalf("NewV1Adapter: %v", err)
	}
	v2, err := NewV2Adapter(&legacy.V2Client{BaseURL: "https://v2.example"})
	if err != nil {
		t.Fatalf("NewV2Adapter: %v", err)
	}
	return map[string]Charger{"v1": v1, "v2": v2}
}

func TestCharge_ApprovedAcrossVersions(t *testing.T) {
	t.Parallel()
	for name, c := range bothAdapters(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, err := c.Charge(context.Background(), ChargeRequest{
				CustomerID: "cust_ok",
				Amount:     usd(1999),
				Reference:  "order-1",
			})
			if err != nil {
				t.Fatalf("Charge = %v, want approved", err)
			}
			if res.TransactionID == "" {
				t.Errorf("expected a transaction id, got empty")
			}
		})
	}
}

func TestCharge_DeclinedTranslatesToDomainError(t *testing.T) {
	t.Parallel()
	// v1 declines for cust_declined; v2 declines for acct_declined.
	cases := map[string]struct {
		adapter    func(*testing.T) Charger
		customerID string
		reason     string
	}{
		"v1": {
			adapter: func(t *testing.T) Charger {
				a, _ := NewV1Adapter(&legacy.V1Client{Endpoint: "https://v1.example"})
				return a
			},
			customerID: "cust_declined",
			reason:     "insufficient_funds",
		},
		"v2": {
			adapter: func(t *testing.T) Charger {
				a, _ := NewV2Adapter(&legacy.V2Client{BaseURL: "https://v2.example"})
				return a
			},
			customerID: "acct_declined",
			reason:     "do_not_honor",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.adapter(t).Charge(context.Background(), ChargeRequest{
				CustomerID: tc.customerID,
				Amount:     usd(500),
			})
			if !errors.Is(err, ErrDeclined) {
				t.Fatalf("errors.Is(ErrDeclined) = false, err = %v", err)
			}
			var de *DeclineError
			if !errors.As(err, &de) {
				t.Fatalf("errors.As(*DeclineError) = false, err = %v", err)
			}
			if de.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", de.Reason, tc.reason)
			}
		})
	}
}

func TestCharge_ProcessorErrorIsUpstream(t *testing.T) {
	t.Parallel()
	v1, _ := NewV1Adapter(&legacy.V1Client{Endpoint: "https://v1.example"})
	_, err := v1.Charge(context.Background(), ChargeRequest{CustomerID: "cust_processor_down", Amount: usd(100)})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("v1 err = %v, want ErrUpstream", err)
	}
	if errors.Is(err, ErrDeclined) {
		t.Errorf("processor error must not be a decline")
	}

	v2, _ := NewV2Adapter(&legacy.V2Client{BaseURL: "https://v2.example"})
	_, err = v2.Charge(context.Background(), ChargeRequest{CustomerID: "acct_down", Amount: usd(100)})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("v2 err = %v, want ErrUpstream", err)
	}
}

func TestCharge_TransportErrorWrapsVendorSentinel(t *testing.T) {
	t.Parallel()
	// Empty endpoint makes the vendor return legacy.ErrTransport.
	v1, _ := NewV1Adapter(&legacy.V1Client{})
	_, err := v1.Charge(context.Background(), ChargeRequest{CustomerID: "cust_ok", Amount: usd(100)})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if !errors.Is(err, legacy.ErrTransport) {
		t.Fatalf("vendor transport sentinel lost: %v", err)
	}
}

func TestCharge_RejectsUnsupportedCurrencyBeforeCallingVendor(t *testing.T) {
	t.Parallel()
	for name, c := range bothAdapters(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := c.Charge(context.Background(), ChargeRequest{
				CustomerID: "cust_ok",
				Amount:     Money{Currency: "EUR", Minor: 1000},
			})
			if !errors.Is(err, ErrUnsupportedCurrency) {
				t.Fatalf("err = %v, want ErrUnsupportedCurrency", err)
			}
		})
	}
}

func TestCharge_RejectsInvalidRequest(t *testing.T) {
	t.Parallel()
	c := bothAdapters(t)["v2"]
	if _, err := c.Charge(context.Background(), ChargeRequest{Amount: usd(100)}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("missing customer => %v, want ErrInvalidRequest", err)
	}
	if _, err := c.Charge(context.Background(), ChargeRequest{CustomerID: "x", Amount: usd(0)}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("zero amount => %v, want ErrInvalidRequest", err)
	}
}

func TestTranslation_UnitsAndCurrency(t *testing.T) {
	t.Parallel()
	req := ChargeRequest{CustomerID: "cust_ok", Amount: usd(1999), Reference: "r"}

	v1 := toV1Request(req)
	if v1.Amt != 19.99 {
		t.Errorf("v1.Amt = %v, want 19.99 (cents converted to dollars)", v1.Amt)
	}
	if v1.Cur != "usd" {
		t.Errorf("v1.Cur = %q, want lowercase usd", v1.Cur)
	}
	if v1.Cust != "cust_ok" {
		t.Errorf("v1.Cust = %q", v1.Cust)
	}

	v2 := toV2Input(req)
	if v2.MinorUnits != 1999 {
		t.Errorf("v2.MinorUnits = %d, want 1999 (no conversion)", v2.MinorUnits)
	}
	if v2.ISOCode != "USD" {
		t.Errorf("v2.ISOCode = %q, want USD", v2.ISOCode)
	}
}

func TestConstructors_RejectNilClients(t *testing.T) {
	t.Parallel()
	if _, err := NewV1Adapter(nil); err == nil {
		t.Error("nil v1 client must be rejected")
	}
	if _, err := NewV2Adapter(nil); err == nil {
		t.Error("nil v2 client must be rejected")
	}
}
```

## Review

The layer is correct when three properties hold. First, the domain never sees a vendor shape: only `adapters.go` imports `legacy`, and `Money`, `Charger`, and the error vocabulary carry no float amounts, no lowercase currencies, and no numeric result codes. If a caller has to switch on an `RC` int or an `Outcome` string, the corruption has leaked past the layer. Second, in-band failures become real Go errors: a decline is an `error` value that `errors.Is` matches as `ErrDeclined` and `errors.As` opens as `*DeclineError`, not a success struct the caller must remember to inspect. Third, the two versioned adapters are interchangeable: the same domain request and the same caller code work against both, which is the entire reason the port exists.

The common mistakes are the ones the tests catch. Forgetting the unit conversion sends 1999 dollars instead of 19.99 to v1; `TestTranslation_UnitsAndCurrency` pins the divide-by-100. Mapping every non-approval `RC` to a generic error throws away the decline-versus-processor distinction a caller needs to decide whether to retry; the switch separates `RC` 1 from the rest, and `TestCharge_ProcessorErrorIsUpstream` asserts a processor error is not a decline. Returning the vendor's transport error verbatim forces the domain to import `legacy` to recognize it; wrapping it as `ErrUpstream` with a second `%w` keeps the class clean and the cause reachable. Putting business rules in the layer instead of pure translation is the subtler trap the Azure guidance warns about -- the adapter maps and nothing more; validation that belongs to the domain lives in `validate`, before the layer is even reached. Running `go test -race ./...` exercises every path, and the parallel subtests confirm the adapters hold no shared mutable state.

## Resources

- [Anti-Corruption Layer pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/anti-corruption-layer) — the pattern this exercise implements, including the rule that the layer does translation only and keeps business rules out.
- [Adapter pattern](https://refactoring.guru/design-patterns/adapter) — the object-adapter structure each versioned adapter follows.
- [`errors.As`](https://pkg.go.dev/errors#As) — the standard-library function that recovers the structured `*DeclineError` from a wrapped chain.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the official explanation of `%w`, `errors.Is`, and `errors.As`, and of implementing a custom `Is` method like `DeclineError.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-messaging-failover-adapter.md](04-messaging-failover-adapter.md)
