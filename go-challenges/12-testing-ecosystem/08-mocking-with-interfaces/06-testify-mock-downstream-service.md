# Exercise 6: testify/mock for a Downstream Inventory Service

`testify/mock` is the terse, reflection-based alternative to gomock: you embed
`mock.Mock`, write each method body as `m.Called(...)`, program expectations with
`On(...).Return(...)`, and verify with `AssertExpectations`. Expectations are
order-independent and opt-in strict. This module drives a reservation service over
an inventory client and contrasts the ergonomics against the gomock modules.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It pulls one external dependency, `github.com/stretchr/testify`.

## What you'll build

```text
reservation/                 independent module: example.com/reservation
  go.mod                     go 1.26; requires github.com/stretchr/testify
  reservation.go             InventoryClient port; Service; Reserve; ErrInsufficientStock
  cmd/
    demo/
      main.go                runnable demo over a real in-memory inventory
  reservation_test.go        hand-written testify mock; On/Return/Once; MatchedBy; AssertNotCalled
```

- Files: `reservation.go`, `cmd/demo/main.go`, `reservation_test.go`.
- Implement: `Service.Reserve(ctx, sku, qty)` that reads `Available`, rejects `qty > available` with `ErrInsufficientStock`, else calls `Reserve` on the client.
- Test: a `MockInventory` embedding `mock.Mock`; program `Available` with `.Once()` and `Reserve` with `mock.MatchedBy`; verify with `AssertExpectations`; an insufficient-stock path asserting `Reserve` was never called via `AssertNotCalled`.
- Verify: `go test -count=1 -race ./...`

Set up the module and pull testify:

```bash
go get github.com/stretchr/testify@latest
```

### The service and its port

`InventoryClient` is a two-method port on a downstream inventory service:
`Available(sku)` returns how many units are in stock, and `Reserve(sku, qty)` holds
that many. The reservation service's rule is a guard: never reserve more than is
available. So `Reserve` first asks `Available`, and if the request exceeds stock it
returns `ErrInsufficientStock` *without* calling the downstream `Reserve` at all —
another no-call-on-failure contract. Only when stock suffices does it place the
reservation.

Create `reservation.go`:

```go
package reservation

import (
	"context"
	"errors"
)

// ErrInsufficientStock is returned when the requested quantity exceeds stock.
var ErrInsufficientStock = errors.New("insufficient stock")

// InventoryClient is the two-method port to the downstream inventory service.
type InventoryClient interface {
	Available(ctx context.Context, sku string) (int, error)
	Reserve(ctx context.Context, sku string, qty int) error
}

// Service reserves stock, guarding against over-reservation.
type Service struct {
	inv InventoryClient
}

// New injects the client through the constructor.
func New(inv InventoryClient) *Service {
	return &Service{inv: inv}
}

// Reserve holds qty units of sku, rejecting a request that exceeds availability
// before ever calling the downstream Reserve.
func (s *Service) Reserve(ctx context.Context, sku string, qty int) error {
	avail, err := s.inv.Available(ctx, sku)
	if err != nil {
		return err
	}
	if qty > avail {
		return ErrInsufficientStock
	}
	return s.inv.Reserve(ctx, sku, qty)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/reservation"
)

// memInventory is a real in-memory InventoryClient for the demo.
type memInventory struct {
	stock map[string]int
}

func (m *memInventory) Available(_ context.Context, sku string) (int, error) {
	return m.stock[sku], nil
}
func (m *memInventory) Reserve(_ context.Context, sku string, qty int) error {
	m.stock[sku] -= qty
	return nil
}

func main() {
	inv := &memInventory{stock: map[string]int{"widget": 5}}
	svc := reservation.New(inv)
	ctx := context.Background()

	if err := svc.Reserve(ctx, "widget", 3); err == nil {
		fmt.Printf("reserved 3, remaining %d\n", inv.stock["widget"])
	}
	err := svc.Reserve(ctx, "widget", 10)
	fmt.Printf("over-reserve rejected: %v\n", errors.Is(err, reservation.ErrInsufficientStock))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reserved 3, remaining 2
over-reserve rejected: true
```

### The testify mock and the tests

`MockInventory` embeds `mock.Mock`; each method body funnels through `m.Called` and
returns the recorded values in the *exact* order of the real signature —
`args.Int(0), args.Error(1)` for `Available`, `args.Error(0)` for `Reserve`.
Mis-indexing here is the classic testify bug (returning `args.Error(0)` when the
error is at position 1 hands back a nil error the SUT should never see), so the
bodies mirror the signatures precisely.

`TestReserveSuccess` programs `Available` to return 5 with `.Once()` and `Reserve`
to accept any request whose quantity is at most available via
`mock.MatchedBy(func(qty int) bool { return qty <= 5 })`, uses `mock.Anything` for
the context argument, runs the service, and calls `AssertExpectations(t)` to verify
every programmed expectation was met — order-independent, unlike gomock's `InOrder`.
`TestReserveInsufficientStock` programs only `Available` (returning 2); it sets no
`Reserve` expectation, so testify would panic on an unexpected `Reserve` call, and
the test additionally asserts `AssertNotCalled(t, "Reserve", ...)` to document the
no-call contract explicitly.

Create `reservation_test.go`:

```go
package reservation

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
)

// MockInventory is a hand-written testify mock. Each body returns the m.Called
// values in the real signature's return order.
type MockInventory struct {
	mock.Mock
}

func (m *MockInventory) Available(ctx context.Context, sku string) (int, error) {
	args := m.Called(ctx, sku)
	return args.Int(0), args.Error(1)
}

func (m *MockInventory) Reserve(ctx context.Context, sku string, qty int) error {
	args := m.Called(ctx, sku, qty)
	return args.Error(0)
}

func TestReserveSuccess(t *testing.T) {
	t.Parallel()

	inv := &MockInventory{}
	inv.On("Available", mock.Anything, "widget").Return(5, nil).Once()
	inv.On("Reserve", mock.Anything, "widget",
		mock.MatchedBy(func(qty int) bool { return qty <= 5 })).Return(nil).Once()

	if err := New(inv).Reserve(context.Background(), "widget", 3); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	inv.AssertExpectations(t)
}

func TestReserveInsufficientStock(t *testing.T) {
	t.Parallel()

	inv := &MockInventory{}
	inv.On("Available", mock.Anything, "widget").Return(2, nil).Once()
	// No On("Reserve", ...): the downstream Reserve must not be called.

	err := New(inv).Reserve(context.Background(), "widget", 10)
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	inv.AssertExpectations(t)
	inv.AssertNotCalled(t, "Reserve", mock.Anything, mock.Anything, mock.Anything)
}

func TestReserveAvailableError(t *testing.T) {
	t.Parallel()

	boom := errors.New("inventory service unavailable")
	inv := &MockInventory{}
	inv.On("Available", mock.Anything, "widget").Return(0, boom).Once()

	if err := New(inv).Reserve(context.Background(), "widget", 1); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the Available error", err)
	}
	inv.AssertExpectations(t)
}
```

## Review

The service is correct when it reads availability, rejects an over-reservation with
`ErrInsufficientStock` before touching the downstream `Reserve`, and otherwise
places the reservation. testify's ergonomics are the point of contrast with gomock:
the mock method bodies are hand-written and terse, expectations are programmed with
`On/Return` and checked order-independently by `AssertExpectations`, and a negative
is expressed both implicitly (an unexpected call panics because no `On` matches) and
explicitly (`AssertNotCalled`). The bug to guard against is the return-index mistake
in the method bodies — `args.Int(0), args.Error(1)` must mirror `(int, error)`
exactly, or the mock silently returns a wrong value. Run `go test -race`.

## Resources

- [github.com/stretchr/testify/mock](https://pkg.go.dev/github.com/stretchr/testify/mock) — `mock.Mock`, `Called`, `On`, `Return`, `MatchedBy`, `AssertExpectations`, `AssertNotCalled`.
- [testify Arguments](https://pkg.go.dev/github.com/stretchr/testify/mock#Arguments) — `Int`, `Error`, `String`, and the return-index accessors.
- [testing](https://pkg.go.dev/testing) — the harness the assertions report through.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-mock-roundtripper-external-api-client.md](07-mock-roundtripper-external-api-client.md)
