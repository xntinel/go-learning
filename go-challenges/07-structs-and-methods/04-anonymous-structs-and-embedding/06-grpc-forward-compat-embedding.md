# Exercise 6: Forward-Compatible Service Stubs via Embedded Defaults

When a service interface grows a new method, every implementer breaks at once —
unless they embed a default that already covers it. This is exactly why generated
gRPC code ships an `UnimplementedServer`: concrete handlers embed it, implement
only the methods they support, and stay compiling when the interface evolves. This
exercise reproduces that pattern for a payment service.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
service/                    independent module: example.com/service
  go.mod                    module example.com/service
  service.go                PaymentService interface; UnimplementedPaymentService; Server
  cmd/
    demo/
      main.go               call an implemented and an unimplemented method; print
  service_test.go           compile-time satisfaction, sentinel error, extra implementer
```

Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
Implement: a `PaymentService` interface (`Charge`, `Refund`, `Balance`), an
`UnimplementedPaymentService` whose methods all return a wrapped
`ErrNotImplemented`, and a `Server` that embeds it and implements only `Charge`.
Test: `var _ PaymentService = (*Server)(nil)` compiles though `Server` defines one
method; the implemented method runs real logic; unimplemented methods return
`ErrNotImplemented` (checked with `errors.Is`); a second minimal implementer that
embeds the default satisfies the interface too.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/04-anonymous-structs-and-embedding/06-grpc-forward-compat-embedding/cmd/demo
cd go-solutions/07-structs-and-methods/04-anonymous-structs-and-embedding/06-grpc-forward-compat-embedding
```

### Why embedding a default gives forward compatibility

`PaymentService` has three methods. `UnimplementedPaymentService` implements all
three, each returning `ErrNotImplemented` wrapped with `%w` so callers can match
it with `errors.Is`. A concrete `Server` embeds `UnimplementedPaymentService` and
defines only `Charge`. Because the embedded default's methods are promoted,
`Server` satisfies the whole `PaymentService` interface: `Charge` is the real
implementation (it shadows the promoted default), while `Refund` and `Balance`
fall through to the promoted defaults and report "not implemented". The
compile-time assertion `var _ PaymentService = (*Server)(nil)` proves it.

The forward-compatibility payoff is the point of the whole pattern. Suppose you
later add a `Void(ctx, id) error` method to `PaymentService`. Add it once to
`UnimplementedPaymentService`, and *every* existing implementer that embeds the
default keeps compiling — the promoted `Void` fills the gap — until each is ready
to implement it for real. Without the embedded default, adding `Void` is an
immediate compile break across every implementer in the codebase. That trade-off
(a new method is a silent "not implemented" until implemented, versus a hard break
everywhere) is exactly the one gRPC chose, and it is why `mustEmbedUnimplemented`
exists.

Create `service.go`:

```go
package service

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotImplemented is the sentinel returned by every default method.
var ErrNotImplemented = errors.New("not implemented")

// PaymentService is the evolving service contract.
type PaymentService interface {
	Charge(ctx context.Context, cents int) (string, error)
	Refund(ctx context.Context, txnID string) error
	Balance(ctx context.Context, account string) (int, error)
}

// UnimplementedPaymentService provides a default for every method so that
// embedding it satisfies PaymentService for free. New interface methods added
// here do not break existing implementers.
type UnimplementedPaymentService struct{}

func (UnimplementedPaymentService) Charge(context.Context, int) (string, error) {
	return "", fmt.Errorf("Charge: %w", ErrNotImplemented)
}

func (UnimplementedPaymentService) Refund(context.Context, string) error {
	return fmt.Errorf("Refund: %w", ErrNotImplemented)
}

func (UnimplementedPaymentService) Balance(context.Context, string) (int, error) {
	return 0, fmt.Errorf("Balance: %w", ErrNotImplemented)
}

// Server embeds the default and implements only Charge. Refund and Balance fall
// through to the promoted defaults.
type Server struct {
	UnimplementedPaymentService
}

// Charge shadows the promoted default with real logic.
func (s *Server) Charge(_ context.Context, cents int) (string, error) {
	if cents <= 0 {
		return "", fmt.Errorf("charge: amount must be positive, got %d", cents)
	}
	return fmt.Sprintf("txn-%d", cents), nil
}

// Compile-time proof that *Server satisfies the whole interface with one method.
var _ PaymentService = (*Server)(nil)
```

### The runnable demo

The demo calls the implemented `Charge` and the unimplemented `Refund` to show the
default surfacing through promotion.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/service"
)

func main() {
	ctx := context.Background()
	var svc service.PaymentService = &service.Server{}

	txn, err := svc.Charge(ctx, 500)
	fmt.Printf("charge: %s err=%v\n", txn, err)

	err = svc.Refund(ctx, txn)
	fmt.Printf("refund is implemented: %v\n", !errors.Is(err, service.ErrNotImplemented))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
charge: txn-500 err=<nil>
refund is implemented: false
```

### Tests

The tests assert real logic on `Charge`, the wrapped sentinel on the unimplemented
methods (via `errors.Is`), and that a second, differently-scoped implementer also
satisfies the interface by embedding the default.

Create `service_test.go`:

```go
package service

import (
	"context"
	"errors"
	"testing"
)

func TestChargeRunsRealLogic(t *testing.T) {
	t.Parallel()

	s := &Server{}
	txn, err := s.Charge(context.Background(), 1200)
	if err != nil {
		t.Fatal(err)
	}
	if txn != "txn-1200" {
		t.Fatalf("Charge = %q, want txn-1200", txn)
	}
}

func TestChargeRejectsNonPositive(t *testing.T) {
	t.Parallel()

	if _, err := (&Server{}).Charge(context.Background(), 0); err == nil {
		t.Fatal("Charge(0) = nil error, want rejection")
	}
}

func TestUnimplementedMethodsReturnSentinel(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := &Server{}

	if err := s.Refund(ctx, "txn-1"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Refund err = %v, want wrapped ErrNotImplemented", err)
	}
	if _, err := s.Balance(ctx, "acct-1"); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Balance err = %v, want wrapped ErrNotImplemented", err)
	}
}

// readOnlyServer is a second implementer that supports only Balance. It embeds
// the default, so it satisfies the full interface even though it defines one
// method. This is the forward-compatible-implementer property in action.
type readOnlyServer struct {
	UnimplementedPaymentService
}

func (readOnlyServer) Balance(context.Context, string) (int, error) { return 42, nil }

func TestSecondImplementerSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var svc PaymentService = readOnlyServer{}
	n, err := svc.Balance(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Fatalf("Balance = %d, want 42", n)
	}
	// Charge is not implemented here: the promoted default answers.
	if _, err := svc.Charge(context.Background(), 1); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Charge err = %v, want wrapped ErrNotImplemented", err)
	}
}
```

## Review

The pattern is correct when a `Server` defining a single method still satisfies
the full interface — that is the compile-time assertion — and when the
unimplemented methods return the sentinel wrapped with `%w` so `errors.Is` matches
it. The forward-compatibility claim is demonstrated by the second implementer:
embedding the default is all it takes to satisfy the contract, so adding a method
to the interface (and its default) never breaks existing code. The mistakes to
avoid: returning a bare `errors.New` instead of wrapping `ErrNotImplemented`,
which defeats `errors.Is`; expecting the embedded default's method to somehow call
back into `Server` (Go shadows, it does not dispatch); and forgetting that only
`*Server` is in the interface's satisfier set when `Charge` has a pointer
receiver.

## Resources

- [gRPC-Go: The mustEmbedUnimplemented pattern](https://pkg.go.dev/google.golang.org/grpc) — the real-world source of this technique.
- [errors: Is and wrapping with %w](https://pkg.go.dev/errors#Is) — matching a sentinel through a wrap chain.
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — why `*Server` satisfies the interface.

---

Prev: [05-store-decorator-interface-embed.md](05-store-decorator-interface-embed.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-ambiguous-embedded-selector.md](07-ambiguous-embedded-selector.md)
