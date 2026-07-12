# Exercise 8: A Domain Type That Plugs Into Stringer, error, and json.Marshaler

A domain type earns its place in the ecosystem by implicitly satisfying small
stdlib interfaces: `fmt.Stringer` controls `%v`, `error` puts it in
`errors.Is`/`errors.As` chains, and `json.Marshaler` controls the wire format.
This module builds an order-processing domain — an `OrderStatus` enum and an
`OrderError` — where each type satisfies multiple stdlib interfaces, and it makes
the crucial precedence rule explicit: when a type implements both `error` and
`Stringer`, `fmt` uses `Error` for `%v`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
orderdomain/                  independent module: example.com/orderdomain
  go.mod                      go 1.26
  order.go                    OrderStatus (Stringer + json.Marshaler); OrderError (error + json.Marshaler)
  cmd/
    demo/
      main.go                 runnable demo: format, marshal, and unwrap the domain types
  order_test.go               %v uses String; json byte-for-byte; errors.As; error-outranks-Stringer
```

- Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: an `OrderStatus` int enum with `String` and `MarshalJSON`, and an `*OrderError` with `Error` and `MarshalJSON`, each guarded by a compile-time interface check.
- Test: `fmt.Sprintf("%v", status)` uses `String`; `json.Marshal` emits the custom wire format byte-for-byte; `errors.As` extracts `*OrderError`; `%v` on the error uses `Error`, not `String`.
- Verify: `go test -count=1 -race ./...`

### One concrete type, several stdlib interfaces

`OrderStatus` is an `int` with named constants. On its own an `int` prints as a
number and marshals as a number — useless in an API. By adding two one-method
implementations it plugs into the ecosystem:

- `String() string` satisfies `fmt.Stringer`, so `%v`/`%s` and `fmt.Println`
  render `StatusPaid` as `paid` instead of `1`.
- `MarshalJSON() ([]byte, error)` satisfies `json.Marshaler`, so `json.Marshal`
  emits `"paid"` (a stable string) instead of `1` (a brittle integer that breaks
  clients when the enum order changes).

Both use value receivers, so `OrderStatus(0)` (a value) satisfies both interfaces —
there is no shared mutable state, so a pointer receiver would only get in the way.

`OrderError` is a struct carrying the failing order id, the status, and a reason.
It satisfies `error` via `Error() string`, so it flows through `errors.Is`/
`errors.As`, and it satisfies `json.Marshaler` via `MarshalJSON` so an API can
serialize it into a stable error body. Its methods take pointer receivers, matching
the convention for error types constructed with `&OrderError{...}`.

The precedence subtlety worth its own paragraph: `fmt` resolves `%v` by checking
whether the operand implements `error` *before* it checks `Stringer`. So if a type
implements both, `%v` calls `Error`, never `String`. `OrderStatus` implements only
`Stringer`, so `%v` uses `String`. `OrderError` implements `error` (not
`Stringer`), so `%v` uses `Error`. Getting this wrong — expecting `%v` to call
`String` on a type that is also an error — is a real formatting bug; the test pins
the actual behavior.

Create `order.go`:

```go
package orderdomain

import (
	"encoding/json"
	"fmt"
)

// OrderStatus is a domain enum. It satisfies fmt.Stringer and json.Marshaler so
// it renders and serializes as a stable string rather than a brittle integer.
type OrderStatus int

const (
	StatusPending OrderStatus = iota
	StatusPaid
	StatusFailed
	StatusRefunded
)

var statusNames = map[OrderStatus]string{
	StatusPending:  "pending",
	StatusPaid:     "paid",
	StatusFailed:   "failed",
	StatusRefunded: "refunded",
}

// String satisfies fmt.Stringer: %v/%s render the status name.
func (s OrderStatus) String() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return fmt.Sprintf("OrderStatus(%d)", int(s))
}

// MarshalJSON satisfies json.Marshaler: the wire format is the status name in
// quotes, not the underlying integer.
func (s OrderStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// OrderError is the domain error for a failed order. It satisfies error and
// json.Marshaler, so it works in errors.Is/As chains and serializes to a stable
// API error body.
type OrderError struct {
	OrderID string
	Status  OrderStatus
	Reason  string
}

// Error satisfies error. Because OrderError implements error, fmt uses this for
// %v (error outranks Stringer in fmt's precedence).
func (e *OrderError) Error() string {
	return fmt.Sprintf("order %s failed: %s (status=%s)", e.OrderID, e.Reason, e.Status)
}

// MarshalJSON satisfies json.Marshaler with a stable error envelope.
func (e *OrderError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Error   string      `json:"error"`
		OrderID string      `json:"order_id"`
		Status  OrderStatus `json:"status"`
	}{
		Error:   e.Reason,
		OrderID: e.OrderID,
		Status:  e.Status,
	})
}

// Compile-time guards: one line per interface each type claims to satisfy.
var (
	_ fmt.Stringer   = OrderStatus(0)
	_ json.Marshaler = OrderStatus(0)
	_ error          = (*OrderError)(nil)
	_ json.Marshaler = (*OrderError)(nil)
)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"example.com/orderdomain"
)

func main() {
	// Stringer: %v renders the name.
	fmt.Printf("status: %v\n", orderdomain.StatusPaid)

	// json.Marshaler: the enum serializes as a string.
	b, _ := json.Marshal(orderdomain.StatusPaid)
	fmt.Printf("status json: %s\n", b)

	// error + json.Marshaler on the domain error.
	var err error = &orderdomain.OrderError{
		OrderID: "ord-42",
		Status:  orderdomain.StatusFailed,
		Reason:  "card declined",
	}
	fmt.Printf("error: %v\n", err)

	body, _ := json.Marshal(err)
	fmt.Printf("error json: %s\n", body)

	// errors.As extracts the concrete domain error from the chain.
	var oe *orderdomain.OrderError
	if errors.As(err, &oe) {
		fmt.Printf("extracted order: %s\n", oe.OrderID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: paid
status json: "paid"
error: order ord-42 failed: card declined (status=failed)
error json: {"error":"card declined","order_id":"ord-42","status":"failed"}
```

### Tests

The JSON assertions are byte-for-byte, because the wire format is a contract with
clients. `TestErrorOutranksStringer` pins the precedence rule: `%v` on the error
uses `Error`, and its output would differ if `Stringer` were consulted instead.

Create `order_test.go`:

```go
package orderdomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestStatusStringer(t *testing.T) {
	t.Parallel()

	if got := fmt.Sprintf("%v", StatusRefunded); got != "refunded" {
		t.Fatalf("%%v = %q, want refunded", got)
	}
}

func TestStatusJSON(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(StatusPaid)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `"paid"`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}

func TestOrderErrorJSON(t *testing.T) {
	t.Parallel()

	e := &OrderError{OrderID: "ord-7", Status: StatusFailed, Reason: "timeout"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":"timeout","order_id":"ord-7","status":"failed"}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}

func TestErrorsAsExtracts(t *testing.T) {
	t.Parallel()

	var err error = &OrderError{OrderID: "ord-9", Status: StatusFailed, Reason: "declined"}
	wrapped := fmt.Errorf("processing: %w", err)

	var oe *OrderError
	if !errors.As(wrapped, &oe) {
		t.Fatal("errors.As failed to extract *OrderError")
	}
	if oe.OrderID != "ord-9" {
		t.Fatalf("OrderID = %q, want ord-9", oe.OrderID)
	}
}

func TestErrorOutranksStringer(t *testing.T) {
	t.Parallel()

	// OrderError implements error but not Stringer, and fmt checks error first,
	// so %v uses Error(). This pins the precedence rule.
	e := &OrderError{OrderID: "ord-1", Status: StatusPaid, Reason: "n/a"}
	got := fmt.Sprintf("%v", e)
	want := "order ord-1 failed: n/a (status=paid)"
	if got != want {
		t.Fatalf("%%v = %q, want %q", got, want)
	}
}

func Example() {
	b, _ := json.Marshal(StatusPaid)
	fmt.Println(StatusPaid, string(b))
	// Output: paid "paid"
}
```

## Review

The design is correct when each type's stdlib interfaces produce the exact,
tested output: `OrderStatus` renders and serializes as its name, and `OrderError`
carries a stable JSON error envelope and unwraps via `errors.As`. The byte-for-byte
JSON tests treat the wire format as the contract it is. The senior subtlety is the
`fmt` precedence rule: `error` is checked before `Stringer` for `%v`, so a type
that implements both formats via `Error` — a genuine surprise if you expected
`String`. `TestErrorOutranksStringer` documents it. The common mistake is leaving
an enum as a bare `int`, which prints and marshals as a number and breaks API
clients the moment the constant order changes; adding `String` and `MarshalJSON`
makes the wire format explicit and stable. Run `go test -race` to confirm.

## Resources

- [`fmt` package: formatting precedence](https://pkg.go.dev/fmt#hdr-Printing) — `error` is consulted before `Stringer` for `%v`.
- [`encoding/json.Marshaler`](https://pkg.go.dev/encoding/json#Marshaler) — custom wire formats.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting a concrete error type from a chain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-optional-interface-capability-detection.md](09-optional-interface-capability-detection.md)
