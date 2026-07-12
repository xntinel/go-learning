# Exercise 1: Order-Fulfillment Saga with LIFO Compensation

An order spans three services with no shared transaction: reserve inventory,
charge payment, create shipment. This exercise builds that saga as a Temporal
workflow where each successful step registers its own compensation via `defer`, so
a later failure unwinds the completed steps in reverse (LIFO) order — and the
original failure is aggregated with any compensation error via `errors.Join`.

This module is fully self-contained. It begins with its own `go mod init`, defines
the workflow, the activities, and a pure ordering helper, and ships its own demo
and tests. Nothing here imports another exercise.

## What you'll build

```text
ordersaga/                     independent module: example.com/ordersaga
  go.mod                       go 1.26; requires go.temporal.io/sdk
  saga.go                      Order; activities; FulfillOrder workflow; LIFO helper
  cmd/
    demo/
      main.go                  //go:build temporal: client.Dial, worker, run one order
  saga_test.go                 testsuite: happy path, payment fail, shipment fail (LIFO), join
```

- Files: `saga.go`, `cmd/demo/main.go`, `saga_test.go`.
- Implement: a `FulfillOrder(ctx workflow.Context, o Order) error` workflow that runs `ReserveInventory`, `ChargePayment`, `CreateShipment` as activities, registers each step's compensation with `defer` after it succeeds, and on failure unwinds LIFO joining every compensation error with `errors.Join`; plus a generic `LIFO` helper.
- Test: `testsuite` cases for the happy path (no compensation), payment failure (only `ReleaseInventory` runs), shipment failure (both compensations run, in `RefundPayment` then `ReleaseInventory` order), and a failing compensation whose error still surfaces.
- Verify: `go test -count=1 -race ./...` (with the module fetched; offline this lesson is validated by shape).

Set up the module:

```bash
go get go.temporal.io/sdk@latest
```

### Why defer is the right tool for LIFO compensation

The saga has a natural shape: do a step, and if it succeeds, record how to undo it.
When something later fails, undo everything recorded so far, newest first. Go's
`defer` already implements exactly that discipline — deferred calls run in
last-in-first-out order when the function returns — so registering each
compensation as a `defer` immediately after its forward step succeeds gives you the
correct reverse-order unwind for free, with no explicit stack to manage.

The mechanism that makes this work is the *named return value*. The workflow is
declared `func FulfillOrder(ctx workflow.Context, o Order) (err error)`. Each
deferred closure checks `err`: it runs its compensation only when `err != nil` at
return time. A statement like `return e` sets the named return `err` to `e` before
any deferred function runs, so every deferred closure observes the failure and
compensates. On the happy path `err` is `nil` at return, every closure sees `nil`,
and no compensation fires. This is why the compensation is registered *after* the
forward step and guarded by `err != nil`: a step that never ran registered no
`defer`, and a run that never failed triggers none of them.

Compensation is not rollback. `ReleaseInventory` and `RefundPayment` are
business-level inverses that operate on already-committed state in other services;
they can themselves fail. So each closure does not just call its activity — it folds
any compensation error into the return value with
`err = errors.Join(err, ce)`. `errors.Join` returns a single error that still
reports every cause, so a failed refund during rollback becomes visible in the
workflow's final error instead of being swallowed. Because each compensation is its
own independent `defer`, a refund that fails does not stop the inventory release
that was registered before it from running.

### The activities and the workflow

Activities are ordinary Go functions taking a standard `context.Context` first
parameter; they are the only place real I/O happens and the only place `time.Now`,
network calls, and randomness are allowed. Here they are stubs standing in for
service calls — in production each would carry an idempotency key derived from
`o.ID` plus the step name so an at-least-once retry cannot double-charge. The
workflow itself does no I/O: it only orchestrates, calling
`workflow.ExecuteActivity(ctx, Activity, o).Get(ctx, nil)` and reacting to the
result. `RetryPolicy{MaximumAttempts: 1}` keeps this exercise focused on
compensation by making a failed activity fail immediately; retry policy is the
subject of Exercise 3.

Create `saga.go`:

```go
package ordersaga

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Order is the input to the fulfillment saga. All fields are exported so the
// Temporal data converter can serialize them into workflow history.
type Order struct {
	ID        string
	Item      string
	Quantity  int
	AmountUSD int
}

// ReserveInventory holds stock for the order. A real implementation would call
// the inventory service with an idempotency key derived from o.ID so an
// at-least-once retry does not double-reserve.
func ReserveInventory(_ context.Context, _ Order) error { return nil }

// ChargePayment debits the customer. Idempotent on o.ID in a real system.
func ChargePayment(_ context.Context, _ Order) error { return nil }

// CreateShipment books the carrier. This is the last forward step, so it needs
// no compensation of its own.
func CreateShipment(_ context.Context, _ Order) error { return nil }

// ReleaseInventory compensates ReserveInventory.
func ReleaseInventory(_ context.Context, _ Order) error { return nil }

// RefundPayment compensates ChargePayment.
func RefundPayment(_ context.Context, _ Order) error { return nil }

// LIFO returns a copy of in reversed: the order in which compensations run
// relative to the order their forward steps completed.
func LIFO[T any](in []T) []T {
	out := make([]T, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// FulfillOrder runs the three-step distributed transaction. Each successful step
// registers its compensation with defer; on any later failure the completed steps
// are undone in reverse order and every error is joined into the return value.
func FulfillOrder(ctx workflow.Context, o Order) (err error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 2 * time.Minute,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	logger := workflow.GetLogger(ctx)

	var completed []string
	defer func() {
		if err != nil {
			logger.Error("order failed; compensating",
				"order", o.ID, "rollback", LIFO(completed))
		}
	}()

	// Step 1: reserve inventory.
	if err = workflow.ExecuteActivity(ctx, ReserveInventory, o).Get(ctx, nil); err != nil {
		return err
	}
	completed = append(completed, "ReleaseInventory")
	defer func() {
		if err != nil {
			if ce := workflow.ExecuteActivity(ctx, ReleaseInventory, o).Get(ctx, nil); ce != nil {
				err = errors.Join(err, ce)
			}
		}
	}()

	// Step 2: charge payment.
	if err = workflow.ExecuteActivity(ctx, ChargePayment, o).Get(ctx, nil); err != nil {
		return err
	}
	completed = append(completed, "RefundPayment")
	defer func() {
		if err != nil {
			if ce := workflow.ExecuteActivity(ctx, RefundPayment, o).Get(ctx, nil); ce != nil {
				err = errors.Join(err, ce)
			}
		}
	}()

	// Step 3: create shipment. The last step needs no compensation.
	if err = workflow.ExecuteActivity(ctx, CreateShipment, o).Get(ctx, nil); err != nil {
		return err
	}

	logger.Info("order fulfilled", "order", o.ID)
	return nil
}
```

### The runnable demo

The demo is the piece that needs a real Temporal service, so it lives behind a
`//go:build temporal` tag: the offline gate never compiles it, and it does not drag
a server dependency into the test path. It dials the local dev server, registers
the workflow and activities on a worker, starts one order, and prints the outcome.
In a dedicated worker process you would instead block on
`w.Run(worker.InterruptCh())`, which runs until SIGINT/SIGTERM; here `Start`/`Stop`
lets the demo run one order and exit.

Create `cmd/demo/main.go`:

```go
//go:build temporal

package main

import (
	"context"
	"fmt"
	"log"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"example.com/ordersaga"
)

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	const taskQueue = "order-fulfillment"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(ordersaga.FulfillOrder)
	w.RegisterActivity(ordersaga.ReserveInventory)
	w.RegisterActivity(ordersaga.ChargePayment)
	w.RegisterActivity(ordersaga.CreateShipment)
	w.RegisterActivity(ordersaga.ReleaseInventory)
	w.RegisterActivity(ordersaga.RefundPayment)

	if err := w.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "order-1001",
		TaskQueue: taskQueue,
	}, ordersaga.FulfillOrder, ordersaga.Order{ID: "1001", Item: "widget", Quantity: 2, AmountUSD: 4999})
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}

	if err := run.Get(context.Background(), nil); err != nil {
		fmt.Printf("order %s failed: %v\n", run.GetID(), err)
		return
	}
	fmt.Printf("order %s fulfilled\n", run.GetID())
}
```

Run it against a local dev server (`temporal server start-dev`), building with the
tag:

```bash
go run -tags temporal ./cmd/demo
```

Expected output (worker logs go to stderr; this is the stdout result line):

```
order order-1001 fulfilled
```

### Tests

The tests prove the saga without a server. `TestFulfillOrder_HappyPath` mocks the
three forward activities to succeed and asserts the workflow completes with no error
and that neither compensation ran. `TestFulfillOrder_PaymentFails` fails
`ChargePayment` and asserts that only `ReleaseInventory` ran — because
`RefundPayment` was never registered, the charge having not committed.
`TestFulfillOrder_ShipmentFails_CompensatesLIFO` fails the last step and asserts
both compensations ran in `RefundPayment`-then-`ReleaseInventory` order, recorded
by having the mocks append to a shared, mutex-guarded slice.
`TestFulfillOrder_CompensationErrorJoined` makes the refund itself fail and proves
the inventory release still runs and the workflow still errors, which is the whole
point of joining compensation errors instead of returning on the first one.

Create `saga_test.go`:

```go
package ordersaga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// recorder captures activity invocation order. Guarded because the test
// environment may invoke mock return functions from its own goroutines.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(name string) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestFulfillOrder_HappyPath(t *testing.T) {
	t.Parallel()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(CreateShipment, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(FulfillOrder, Order{ID: "1", Item: "widget", Quantity: 1, AmountUSD: 100})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything)
}

func TestFulfillOrder_PaymentFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(errors.New("gateway 500"))
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("ReleaseInventory"); return nil })

	env.ExecuteWorkflow(FulfillOrder, Order{ID: "2"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, []string{"ReleaseInventory"}, rec.snapshot())
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything)
}

func TestFulfillOrder_ShipmentFails_CompensatesLIFO(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(CreateShipment, mock.Anything, mock.Anything).Return(errors.New("carrier unavailable"))
	env.OnActivity(RefundPayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("RefundPayment"); return nil })
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("ReleaseInventory"); return nil })

	env.ExecuteWorkflow(FulfillOrder, Order{ID: "3"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, []string{"RefundPayment", "ReleaseInventory"}, rec.snapshot())
	env.AssertActivityCalled(t, "RefundPayment", mock.Anything, mock.Anything)
	env.AssertActivityCalled(t, "ReleaseInventory", mock.Anything, mock.Anything)
}

func TestFulfillOrder_CompensationErrorJoined(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(CreateShipment, mock.Anything, mock.Anything).Return(errors.New("carrier unavailable"))
	env.OnActivity(RefundPayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("RefundPayment"); return errors.New("refund declined") })
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("ReleaseInventory"); return nil })

	env.ExecuteWorkflow(FulfillOrder, Order{ID: "4"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	// A failed refund does not stop the inventory release registered before it.
	require.Equal(t, []string{"RefundPayment", "ReleaseInventory"}, rec.snapshot())
}

func ExampleLIFO() {
	completed := []string{"ReleaseInventory", "RefundPayment", "CancelShipment"}
	fmt.Println(LIFO(completed))
	// Output: [CancelShipment RefundPayment ReleaseInventory]
}
```

## Review

The saga is correct when compensation is scoped to exactly the steps that
committed and runs newest-first. The payment-failure test is the scope proof: only
`ReleaseInventory` runs because `RefundPayment`'s `defer` was never reached. The
shipment-failure test is the ordering proof: the recorder shows `RefundPayment`
before `ReleaseInventory`, the reverse of the order they were registered. If either
of those flips, the bug is almost always a compensation registered before its
forward step succeeded, or registered unconditionally.

The mistakes to avoid: do not return on the first compensation error — join it with
`errors.Join` and let the remaining `defer`s run, as the joined-error test shows,
so a failed refund is both surfaced and does not block the inventory release. Do
not do any real I/O in the workflow function; the activities are the only place
effects belong, and the workflow must stay deterministic. And remember these stub
activities must be idempotent in a real system: with at-least-once delivery, a
retry can run `ChargePayment` twice, so key it on the order ID. Run the tests with
the module present via `go test -race ./...`; offline, this lesson is validated by
its shape because `go.temporal.io/sdk` is not vendored here.

## Resources

- [`go.temporal.io/sdk/workflow`](https://pkg.go.dev/go.temporal.io/sdk/workflow) — `ExecuteActivity`, `WithActivityOptions`, `ActivityOptions`, `GetLogger`.
- [`go.temporal.io/sdk/testsuite`](https://pkg.go.dev/go.temporal.io/sdk/testsuite) — `WorkflowTestSuite` and the in-memory `TestWorkflowEnvironment`.
- [Temporal Go saga sample](https://github.com/temporalio/samples-go/tree/main/saga) — the official defer-based compensation workflow this exercise is modeled on.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating the forward failure with compensation errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-saga-coordinator-disconnected-ctx.md](02-saga-coordinator-disconnected-ctx.md)
