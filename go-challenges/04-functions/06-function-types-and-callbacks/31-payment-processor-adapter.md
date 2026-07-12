# Exercise 31: Payment Gateway Adapter — Function Type Implementing Processor Interface

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

`http.HandlerFunc` is the canonical example of a named function type
implementing an interface by defining one method on itself that just calls
the function value. This module applies the exact same adapter pattern to
payment gateways: a `Processor` interface, a `ProcessorFunc` that adapts
any matching function to it, and a `Middleware` chain (validation, call
counting) built entirely out of `Processor`-returning functions — so
swapping the underlying gateway never touches the code that calls it.

## What you'll build

```text
payment/                      independent module: example.com/payment-processor-adapter
  go.mod                       go 1.24
  payment.go                   type Processor, type ProcessorFunc, type Middleware, func Chain, WithValidation, WithCallCounter
  cmd/
    demo/
      main.go                    runnable demo: a mock gateway and a failing gateway behind the same interface
  payment_test.go                interface satisfaction, validation short-circuit, middleware order, gateway swap, concurrency (-race)
```

Files: `payment.go`, `cmd/demo/main.go`, `payment_test.go`.
Implement: `type Processor interface { Process(ctx, Payment) (Result, error) }`, `type ProcessorFunc func(ctx, Payment) (Result, error)` with a `Process` method that calls itself, `type Middleware func(next Processor) Processor`, `func Chain(p Processor, mws ...Middleware) Processor`, `WithValidation` (rejects non-positive amounts before calling next), and `WithCallCounter` (atomically counts every call regardless of outcome).
Test: `ProcessorFunc` satisfies `Processor` at compile time and at runtime; `WithValidation` rejects an invalid amount without calling `next`; `WithValidation` passes a valid amount through unchanged; `Chain` applies middlewares in outer-to-inner then inner-to-outer order around the base processor; swapping the underlying `ProcessorFunc` behind the same `Processor` variable changes the observed result without changing the calling code; concurrent `Process` calls through one chained `Processor` are race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/31-payment-processor-adapter/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/31-payment-processor-adapter
go mod edit -go=1.24
```

### Why `ProcessorFunc` needs only one method to become a `Processor`

`Processor` has one method, `Process`. `ProcessorFunc` is a named type
whose underlying type is a plain function with that exact signature, and
its `Process` method is one line: call the function value on itself,
`return f(ctx, p)`. That single method is enough for the Go compiler to
consider any `ProcessorFunc` value a full `Processor` — this is precisely
how `http.HandlerFunc` turns any `func(w, r)` into an `http.Handler`
without a struct. The payoff shows up in `WithValidation` and
`WithCallCounter`: both are ordinary functions that build a *new*
`ProcessorFunc` closing over `next`, so writing middleware never requires
defining a struct with an embedded `Processor` field. `Chain` folds a
slice of `Middleware` around a base `Processor` from the last one inward
— identical in shape to the interceptor chain built earlier in this
chapter — so `mws[0]` ends up outermost, seeing the payment first and the
result last. Because the adapter is the interface's only requirement,
`mockGateway` and `failingGateway` in the demo are completely
interchangeable behind one `Processor` variable: the code that calls
`gateway.Process(...)` never needs to know or care which concrete function
is underneath.

Create `payment.go`:

```go
// Package payment adapts plain functions to a Processor interface via a
// named function type, the same pattern as http.HandlerFunc, so gateway
// implementations can be swapped behind one interface without an
// interface-implementing struct for every gateway.
package payment

import (
	"context"
	"errors"
	"sync/atomic"
)

// Payment is the request a Processor charges.
type Payment struct {
	Amount    int64
	Currency  string
	Reference string
}

// Result is what a successful Processor.Process returns.
type Result struct {
	TransactionID string
	Amount        int64
}

// Processor is the interface every payment gateway implements.
type Processor interface {
	Process(ctx context.Context, p Payment) (Result, error)
}

// ProcessorFunc adapts a plain function to the Processor interface, the
// same pattern as http.HandlerFunc adapting a func to http.Handler.
type ProcessorFunc func(ctx context.Context, p Payment) (Result, error)

// Process calls f itself, satisfying Processor.
func (f ProcessorFunc) Process(ctx context.Context, p Payment) (Result, error) {
	return f(ctx, p)
}

// compile-time check that ProcessorFunc satisfies Processor.
var _ Processor = ProcessorFunc(nil)

// ErrInvalidAmount is returned by WithValidation for a non-positive amount.
var ErrInvalidAmount = errors.New("invalid payment amount")

// Middleware wraps a Processor with another Processor, the same shape as
// an http middleware wrapping a Handler.
type Middleware func(next Processor) Processor

// Chain applies mws around p in order, so mws[0] is outermost.
func Chain(p Processor, mws ...Middleware) Processor {
	for i := len(mws) - 1; i >= 0; i-- {
		p = mws[i](p)
	}
	return p
}

// WithValidation rejects non-positive amounts before calling next.
func WithValidation(next Processor) Processor {
	return ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		if p.Amount <= 0 {
			return Result{}, ErrInvalidAmount
		}
		return next.Process(ctx, p)
	})
}

// WithCallCounter increments counter atomically around every call to next,
// regardless of outcome.
func WithCallCounter(counter *int64) Middleware {
	return func(next Processor) Processor {
		return ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
			atomic.AddInt64(counter, 1)
			return next.Process(ctx, p)
		})
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/payment-processor-adapter"
)

// mockGateway and failingGateway are two interchangeable gateway
// implementations, both adapted to payment.Processor via
// payment.ProcessorFunc.
func mockGateway(ctx context.Context, p payment.Payment) (payment.Result, error) {
	return payment.Result{TransactionID: "mock-" + p.Reference, Amount: p.Amount}, nil
}

func failingGateway(ctx context.Context, p payment.Payment) (payment.Result, error) {
	return payment.Result{}, fmt.Errorf("gateway unreachable")
}

func run(name string, gateway payment.Processor, p payment.Payment) {
	res, err := gateway.Process(context.Background(), p)
	if err != nil {
		fmt.Printf("%s: error: %v\n", name, err)
		return
	}
	fmt.Printf("%s: transaction %s for %d\n", name, res.TransactionID, res.Amount)
}

func main() {
	valid := payment.Payment{Amount: 500, Currency: "USD", Reference: "order-1"}
	invalid := payment.Payment{Amount: -10, Currency: "USD", Reference: "order-2"}

	protected := payment.Chain(payment.ProcessorFunc(mockGateway), payment.WithValidation)
	run("mock+validation", protected, valid)
	run("mock+validation", protected, invalid)

	// Swap the underlying gateway behind the same interface: caller code
	// (run) does not change at all.
	fallback := payment.Chain(payment.ProcessorFunc(failingGateway), payment.WithValidation)
	run("failing+validation", fallback, valid)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mock+validation: transaction mock-order-1 for 500
mock+validation: error: invalid payment amount
failing+validation: error: gateway unreachable
```

### Tests

Create `payment_test.go`:

```go
package payment

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProcessorFuncSatisfiesProcessorInterface(t *testing.T) {
	t.Parallel()
	var p Processor = ProcessorFunc(func(ctx context.Context, pay Payment) (Result, error) {
		return Result{TransactionID: "t1"}, nil
	})
	res, err := p.Process(context.Background(), Payment{Amount: 100})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.TransactionID != "t1" {
		t.Fatalf("TransactionID = %q, want t1", res.TransactionID)
	}
}

func TestWithValidationRejectsNonPositiveAmountWithoutCallingNext(t *testing.T) {
	t.Parallel()
	nextCalled := false
	next := ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		nextCalled = true
		return Result{}, nil
	})
	validated := WithValidation(next)

	_, err := validated.Process(context.Background(), Payment{Amount: 0})
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("err = %v, want ErrInvalidAmount", err)
	}
	if nextCalled {
		t.Fatal("next ran despite an invalid amount")
	}
}

func TestWithValidationPassesThroughValidAmount(t *testing.T) {
	t.Parallel()
	next := ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		return Result{TransactionID: "ok"}, nil
	})
	validated := WithValidation(next)

	res, err := validated.Process(context.Background(), Payment{Amount: 50})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.TransactionID != "ok" {
		t.Fatalf("TransactionID = %q, want ok", res.TransactionID)
	}
}

func TestChainAppliesMiddlewareInOrder(t *testing.T) {
	t.Parallel()
	var order []string
	tag := func(name string) Middleware {
		return func(next Processor) Processor {
			return ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
				order = append(order, "before:"+name)
				res, err := next.Process(ctx, p)
				order = append(order, "after:"+name)
				return res, err
			})
		}
	}
	base := ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		order = append(order, "base")
		return Result{}, nil
	})

	chained := Chain(base, tag("outer"), tag("inner"))
	_, err := chained.Process(context.Background(), Payment{Amount: 10})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	want := []string{"before:outer", "before:inner", "base", "after:inner", "after:outer"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestSwappingUnderlyingProcessorChangesResultBehindSameInterface(t *testing.T) {
	t.Parallel()
	gatewayErr := errors.New("gateway down")
	mock := ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		return Result{TransactionID: "mock"}, nil
	})
	failing := ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		return Result{}, gatewayErr
	})

	callWith := func(p Processor) (Result, error) {
		return p.Process(context.Background(), Payment{Amount: 10})
	}

	res, err := callWith(mock)
	if err != nil || res.TransactionID != "mock" {
		t.Fatalf("mock: res=%v err=%v", res, err)
	}

	_, err = callWith(failing)
	if !errors.Is(err, gatewayErr) {
		t.Fatalf("failing: err = %v, want %v", err, gatewayErr)
	}
}

func TestConcurrentProcessCallsThroughChainAreRaceFree(t *testing.T) {
	t.Parallel()
	var counter int64
	var mu sync.Mutex
	seen := 0

	base := ProcessorFunc(func(ctx context.Context, p Payment) (Result, error) {
		mu.Lock()
		seen++
		mu.Unlock()
		return Result{TransactionID: "ok"}, nil
	})
	chained := Chain(base, WithCallCounter(&counter), WithValidation)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = chained.Process(context.Background(), Payment{Amount: 10})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&counter); got != 50 {
		t.Fatalf("counter = %d, want 50", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 50 {
		t.Fatalf("seen = %d, want 50", seen)
	}
}
```

## Review

`ProcessorFunc` is correct exactly when it never does anything except
forward to the wrapped function — the compile-time check `var _
Processor = ProcessorFunc(nil)` catches the case where the method
signature drifts from the interface, and `TestProcessorFuncSatisfiesProcessorInterface`
catches the runtime behavior. `TestWithValidationRejectsNonPositiveAmountWithoutCallingNext`
is the test that matters most for a payment system specifically: a
middleware that is supposed to gate a side effect (charging money) must
be provably unable to let that side effect happen on the rejected path,
not just "usually" skip it. `TestChainAppliesMiddlewareInOrder` confirms
the same outer-to-inner composition used for the interceptor chain
earlier in this chapter applies here too — this pattern is not specific
to gRPC, it is a general shape for anything typed `func(next X) X`.
`TestSwappingUnderlyingProcessorChangesResultBehindSameInterface` is the
test that validates the whole point of the adapter: `callWith` is written
once against `Processor` and never edited, yet its behavior changes
completely depending on which `ProcessorFunc` value it receives.

## Resources

- [net/http: HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [sync/atomic package](https://pkg.go.dev/sync/atomic)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-message-handler-type-registry.md](30-message-handler-type-registry.md) | Next: [32-permission-evaluator-callback-chain.md](32-permission-evaluator-callback-chain.md)
