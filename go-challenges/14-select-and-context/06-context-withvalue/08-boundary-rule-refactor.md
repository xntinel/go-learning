# Exercise 8: Debugging the Boundary Rule — Value vs. Parameter

This is a refactor exercise. You start from real anti-pattern code: a `CreateOrder`
that fishes its `productID` and `quantity` out of the context with string keys and
panics when they are missing. The task encodes the boundary rule mechanically —
domain arguments become explicit parameters, while genuinely cross-cutting trace
data stays in the context — and proves the fix with tests.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
boundary/                    independent module: example.com/boundary
  go.mod
  bad.go                     badCreateOrder: pulls productID/qty from ctx, panics on absence
  order.go                   CreateOrder(ctx, productID, qty): params explicit, trace in ctx
  cmd/
    demo/
      main.go                shows the panic path and the fixed path
  boundary_test.go           bad path panics on missing key; fixed path works with Background
```

Files: `bad.go`, `order.go`, `cmd/demo/main.go`, `boundary_test.go`.
Implement: keep the anti-pattern `badCreateOrder` to document the smell, then a fixed `CreateOrder(ctx, productID string, qty int)` that takes domain data as parameters and reads only trace from the context.
Test: `badCreateOrder` panics when the context lacks `"productID"` (asserted via recover); the fixed `CreateOrder` returns a correct result with `context.Background()` (no panic) and still carries the trace from the context.
Verify: `go test -count=1 -race ./...`

### The smell, the rule, and the fix

`badCreateOrder` is what the anti-pattern looks like in production: it retrieves
`productID` and `quantity` with `ctx.Value("productID").(string)` — string keys, no
comma-ok. This has two defects at once. First, string keys collide across packages,
so another package's `"productID"` could silently feed this function the wrong
value. Second, and worse, the naked type assertion *panics* the instant the value is
missing or the wrong type — which is a runtime crash triggered by a caller forgetting
to populate the context, with nothing in `CreateOrder`'s signature warning them.

Apply the boundary-rule test to each value. Delete `productID` from the context: the
order is created for the wrong product, or the code panics — correctness changes, so
`productID` was a required *parameter* in disguise. Delete `quantity`: same. Delete
the *trace ID*: the returned order is identical; only the log line loses its
correlation — observability degrades but correctness is untouched, so the trace is
legitimately request-scoped. The rule is exactly that: if removing a value breaks
correctness it was a missing parameter; if it only dims observability it belongs in
the context.

The fixed `CreateOrder(ctx, productID string, qty int)` takes the domain data as
explicit parameters — visible in the signature, impossible to forget without a
compile error, never panicking — and reads *only* the trace ID from the context, with
comma-ok, defaulting to `"none"` when absent. The proof that the refactor is
complete is that `CreateOrder` works correctly when called with `context.Background()`:
a function that no longer fishes domain data out of the context cannot panic on an
empty one.

Create `bad.go` — the anti-pattern, kept to document the smell:

```go
package boundary

import (
	"context"
	"strconv"
)

// badCreateOrder is the ANTI-PATTERN: it pulls domain arguments out of the
// context with string keys and a naked type assertion, so it panics the moment a
// caller forgets to populate the context. Kept only to test the smell.
func badCreateOrder(ctx context.Context) string {
	productID := ctx.Value("productID").(string) // panics if missing or wrong type
	qty := ctx.Value("quantity").(int)
	return productID + " x" + strconv.Itoa(qty)
}
```

Create `order.go` — the fix: domain data as parameters, trace in context:

```go
package boundary

import (
	"context"
	"fmt"
)

type traceKey struct{}

// WithTrace attaches a trace ID to ctx. This is the one value that legitimately
// belongs in the context: removing it degrades observability, not correctness.
func WithTrace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey{}, id)
}

func traceFrom(ctx context.Context) string {
	if id, ok := ctx.Value(traceKey{}).(string); ok {
		return id
	}
	return "none"
}

// Order is the created order.
type Order struct {
	ProductID string
	Quantity  int
	Trace     string
}

// CreateOrder takes its domain arguments explicitly and reads only the trace ID
// from the context. It never calls ctx.Value for domain data, so it cannot panic
// on an empty context.
func CreateOrder(ctx context.Context, productID string, qty int) (Order, error) {
	if productID == "" {
		return Order{}, fmt.Errorf("create order: empty product id")
	}
	if qty <= 0 {
		return Order{}, fmt.Errorf("create order: quantity must be positive, got %d", qty)
	}
	return Order{ProductID: productID, Quantity: qty, Trace: traceFrom(ctx)}, nil
}
```

### The demo

The demo shows both worlds: the anti-pattern panics under a bare context (recovered
so the program continues), and the fixed API produces a correct order from
`context.Background()` plus explicit arguments, still carrying a trace when one is
present.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/boundary"
)

func main() {
	// The fixed API: domain data as parameters, works with a bare context.
	order, err := boundary.CreateOrder(context.Background(), "SKU-1", 3)
	if err != nil {
		panic(err)
	}
	fmt.Printf("order: product=%s qty=%d trace=%s\n", order.ProductID, order.Quantity, order.Trace)

	// The same call under a traced context: correctness identical, trace enriched.
	ctx := boundary.WithTrace(context.Background(), "trace-77")
	traced, _ := boundary.CreateOrder(ctx, "SKU-1", 3)
	fmt.Printf("order: product=%s qty=%d trace=%s\n", traced.ProductID, traced.Quantity, traced.Trace)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order: product=SKU-1 qty=3 trace=none
order: product=SKU-1 qty=3 trace=trace-77
```

### The tests

`TestBadCreateOrderPanicsWithoutProductID` documents the smell as a shipped,
passing test: it calls the anti-pattern with a bare context and asserts, via
`recover`, that it panicked — proving the failure mode is real. The fixed-API tests
assert the opposite: `CreateOrder` with `context.Background()` returns a correct
order and never panics (the proof it no longer reads domain data from the context),
and the trace still flows when the context carries one.

Create `boundary_test.go`:

```go
package boundary

import (
	"context"
	"testing"
)

func TestBadCreateOrderPanicsWithoutProductID(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("badCreateOrder did not panic on a context missing productID")
		}
	}()

	_ = badCreateOrder(context.Background()) // the smell: panics on absence
}

func TestCreateOrderWorksWithBackgroundContext(t *testing.T) {
	t.Parallel()

	order, err := CreateOrder(context.Background(), "SKU-9", 2)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.ProductID != "SKU-9" || order.Quantity != 2 {
		t.Fatalf("order = %+v, want product SKU-9 qty 2", order)
	}
	if order.Trace != "none" {
		t.Fatalf("trace = %q, want none on an untraced context", order.Trace)
	}
}

func TestCreateOrderTraceStillFlows(t *testing.T) {
	t.Parallel()

	ctx := WithTrace(context.Background(), "trace-1")
	order, err := CreateOrder(ctx, "SKU-9", 2)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Trace != "trace-1" {
		t.Fatalf("trace = %q, want trace-1", order.Trace)
	}
}

func TestCreateOrderValidatesDomainArgs(t *testing.T) {
	t.Parallel()

	if _, err := CreateOrder(context.Background(), "", 1); err == nil {
		t.Fatal("empty product id should error")
	}
	if _, err := CreateOrder(context.Background(), "SKU-1", 0); err == nil {
		t.Fatal("non-positive quantity should error")
	}
}
```

## Review

The refactor is complete when `CreateOrder` produces a correct order from
`context.Background()` — that single fact proves it no longer depends on the context
for domain data and therefore cannot panic when a caller forgets to populate it. The
anti-pattern test is not there to be fixed away; it is there to document, as
executable proof, why the old shape was dangerous: a missing context value became a
runtime panic with no signature-level warning. The boundary rule is the takeaway,
and it is mechanical: delete a value in your head — if correctness changes it was a
parameter, if only observability dims it was request-scoped. `productID` and
`quantity` became parameters; the trace ID stayed in the context. That single
discipline eliminates a whole class of hidden-dependency panics.

## Resources

- [context.WithValue](https://pkg.go.dev/context#WithValue) — "not for passing optional parameters to functions."
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs) — the guidance that domain data belongs in parameters or structs.
- [Go Code Review Comments: Contexts](https://go.dev/wiki/CodeReviewComments#contexts) — the community rule against stuffing parameters into a context.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-idempotency-key-guard.md](07-idempotency-key-guard.md) | Next: [09-locale-content-negotiation.md](09-locale-content-negotiation.md)
