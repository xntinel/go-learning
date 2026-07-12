# Exercise 2: Reusable Saga Coordinator that Compensates Under Cancellation

The `defer` pattern from Exercise 1 works, but you would not want to re-derive it in
every workflow. This exercise generalizes it into a small `Saga` coordinator any
workflow can embed — a stack of registered compensations with `AddCompensation` and
`Compensate` — and adds the production detail that a one-off saga usually forgets:
when the workflow is cancelled, compensation must run on a context detached from
the cancellation, or it silently does not run at all.

This module is fully self-contained: its own `go mod init`, the coordinator, the
workflow that uses it, a deliberately-wrong variant for contrast, a demo, and tests.

## What you'll build

```text
sagacoord/                     independent module: example.com/sagacoord
  go.mod                       go 1.26; requires go.temporal.io/sdk
  coordinator.go               Saga{AddCompensation, Compensate, CompensationPlan}; PlaceOrder; PlaceOrderNaive
  cmd/
    demo/
      main.go                  //go:build temporal: run one order through the coordinator
  coordinator_test.go          testsuite: rollback LIFO, rollback under cancel, naive-skips proof
```

- Files: `coordinator.go`, `cmd/demo/main.go`, `coordinator_test.go`.
- Implement: a reusable `Saga` type with `AddCompensation(name, fn)`, a pure `CompensationPlan()` returning names in LIFO order, and `Compensate(ctx)` that runs every compensation in reverse on a `workflow.NewDisconnectedContext`, joining errors; a `PlaceOrder` workflow that uses it; and a `PlaceOrderNaive` that compensates on the inherited context to demonstrate the bug.
- Test: rollback in LIFO order on a normal failure; rollback still runs when the workflow is cancelled mid-flight (asserted canceled via `temporal.IsCanceledError`); and a negative test proving the naive variant skips compensation under cancellation.
- Verify: `go test -count=1 -race ./...` (with the module fetched; offline this lesson is validated by shape).

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/08-temporal-durable-execution/02-saga-coordinator-disconnected-ctx/cmd/demo
cd go-solutions/50-messaging-and-event-driven/08-temporal-durable-execution/02-saga-coordinator-disconnected-ctx
go get go.temporal.io/sdk@latest
```

### Why a cancelled workflow needs a disconnected context

Cancellation propagates through the workflow's `workflow.Context`. When a client
cancels the workflow (or a parent cancels a child), that context transitions to a
cancelled state, and `ctx.Err()` starts reporting a cancellation. The crucial and
non-obvious consequence: `workflow.ExecuteActivity(ctx, ...)` on a *cancelled*
context does not schedule the activity at all — the returned future resolves
immediately to a cancelled error. So a compensation written the obvious way, on the
same `ctx` the workflow was using, never actually runs when the workflow is being
cancelled. The refund you carefully wrote is skipped exactly when someone cancels an
in-flight order, which is precisely when a charge may already be committed. This is
invisible on the happy path and in any test that fails a step without cancelling; it
surfaces as a production incident.

`workflow.NewDisconnectedContext(parent)` returns a new `workflow.Context` that
inherits the parent's values and options but is *detached* from its cancellation,
plus a `workflow.CancelFunc` to cancel the disconnected context independently.
Scheduling compensation activities on that disconnected context lets them run to
completion even while the workflow itself is unwinding a cancellation. The
coordinator does this once, in `Compensate`, so every workflow that uses it inherits
correct cancellation-safe rollback.

### The coordinator

`Saga` is a stack of named compensations. `AddCompensation` pushes one after its
forward step succeeds. `CompensationPlan` is a pure function returning the names in
the order they will run — reverse of registration — used both to log the plan and to
make the ordering unit-testable without a workflow. `Compensate` creates the
disconnected context, walks the stack in reverse, and joins any errors so a failed
compensation is surfaced rather than swallowed. The `defer cancel()` releases the
disconnected context when rollback finishes.

Create `coordinator.go`:

```go
package sagacoord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Order is the saga input. Exported fields so Temporal can serialize it.
type Order struct {
	ID        string
	AmountUSD int
}

func ReserveInventory(_ context.Context, _ Order) error { return nil }
func ChargePayment(_ context.Context, _ Order) error    { return nil }
func CreateShipment(_ context.Context, _ Order) error   { return nil }
func ReleaseInventory(_ context.Context, _ Order) error { return nil }
func RefundPayment(_ context.Context, _ Order) error    { return nil }

type step struct {
	name string
	fn   func(workflow.Context) error
}

// Saga is a reusable coordinator: a stack of compensations that runs them in
// reverse order, on a context detached from the workflow's cancellation.
type Saga struct {
	steps []step
}

// AddCompensation registers a compensation. Call it only after the matching
// forward step has succeeded, so a step that never ran is never undone.
func (s *Saga) AddCompensation(name string, fn func(workflow.Context) error) {
	s.steps = append(s.steps, step{name: name, fn: fn})
}

// CompensationPlan returns the compensation names in the order Compensate will
// run them: reverse (LIFO) of registration. Pure and independently testable.
func (s *Saga) CompensationPlan() []string {
	names := make([]string, len(s.steps))
	for i, st := range s.steps {
		names[len(s.steps)-1-i] = st.name
	}
	return names
}

// Compensate runs every registered compensation in LIFO order on a context
// disconnected from the parent's cancellation, so rollback completes even when
// the workflow is being cancelled. Every compensation error is joined in.
func (s *Saga) Compensate(ctx workflow.Context) error {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	workflow.GetLogger(ctx).Info("compensating", "plan", s.CompensationPlan())

	var errs []error
	for i := len(s.steps) - 1; i >= 0; i-- {
		st := s.steps[i]
		if err := st.fn(dctx); err != nil {
			errs = append(errs, fmt.Errorf("compensate %s: %w", st.name, err))
		}
	}
	return errors.Join(errs...)
}

func activityCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
}

// PlaceOrder runs the saga using the coordinator. On any failure or cancellation
// the deferred Compensate unwinds completed steps on a disconnected context.
func PlaceOrder(ctx workflow.Context, o Order) (err error) {
	ctx = activityCtx(ctx)
	saga := &Saga{}
	defer func() {
		if err != nil {
			if cerr := saga.Compensate(ctx); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
	}()

	if err = workflow.ExecuteActivity(ctx, ReserveInventory, o).Get(ctx, nil); err != nil {
		return err
	}
	saga.AddCompensation("ReleaseInventory", func(c workflow.Context) error {
		return workflow.ExecuteActivity(c, ReleaseInventory, o).Get(c, nil)
	})

	if err = workflow.ExecuteActivity(ctx, ChargePayment, o).Get(ctx, nil); err != nil {
		return err
	}
	saga.AddCompensation("RefundPayment", func(c workflow.Context) error {
		return workflow.ExecuteActivity(c, RefundPayment, o).Get(c, nil)
	})

	// The last step awaits the carrier; a cancel typically lands here.
	if err = workflow.ExecuteActivity(ctx, CreateShipment, o).Get(ctx, nil); err != nil {
		return err
	}
	return nil
}

// PlaceOrderNaive is the WRONG version: it compensates on the inherited (already
// cancelled) context, so under cancellation its compensations fail immediately
// and never run. Kept only to prove why NewDisconnectedContext is required.
func PlaceOrderNaive(ctx workflow.Context, o Order) (err error) {
	ctx = activityCtx(ctx)
	var comps []func(workflow.Context) error
	defer func() {
		if err != nil {
			for i := len(comps) - 1; i >= 0; i-- {
				if ce := comps[i](ctx); ce != nil { // WRONG: original, cancelled ctx
					err = errors.Join(err, ce)
				}
			}
		}
	}()

	if err = workflow.ExecuteActivity(ctx, ReserveInventory, o).Get(ctx, nil); err != nil {
		return err
	}
	comps = append(comps, func(c workflow.Context) error {
		return workflow.ExecuteActivity(c, ReleaseInventory, o).Get(c, nil)
	})
	if err = workflow.ExecuteActivity(ctx, ChargePayment, o).Get(ctx, nil); err != nil {
		return err
	}
	comps = append(comps, func(c workflow.Context) error {
		return workflow.ExecuteActivity(c, RefundPayment, o).Get(c, nil)
	})
	if err = workflow.ExecuteActivity(ctx, CreateShipment, o).Get(ctx, nil); err != nil {
		return err
	}
	return nil
}
```

### The runnable demo

Behind the `temporal` tag: dial the dev server, register the workflow and
activities, run one order through the coordinator, and print the outcome.

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

	"example.com/sagacoord"
)

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	const taskQueue = "saga-coordinator"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(sagacoord.PlaceOrder)
	w.RegisterActivity(sagacoord.ReserveInventory)
	w.RegisterActivity(sagacoord.ChargePayment)
	w.RegisterActivity(sagacoord.CreateShipment)
	w.RegisterActivity(sagacoord.ReleaseInventory)
	w.RegisterActivity(sagacoord.RefundPayment)

	if err := w.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "order-2002",
		TaskQueue: taskQueue,
	}, sagacoord.PlaceOrder, sagacoord.Order{ID: "2002", AmountUSD: 7500})
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}

	if err := run.Get(context.Background(), nil); err != nil {
		fmt.Printf("order %s rolled back: %v\n", run.GetID(), err)
		return
	}
	fmt.Printf("order %s placed\n", run.GetID())
}
```

Run it against a local dev server (`temporal server start-dev`):

```bash
go run -tags temporal ./cmd/demo
```

Expected output (stdout result line; worker logs go to stderr):

```
order order-2002 placed
```

### Tests

`TestPlaceOrder_RollbackLIFO` fails the shipment with an ordinary error and asserts
both compensations ran in `RefundPayment`-then-`ReleaseInventory` order.
`TestPlaceOrder_CompensatesOnCancellation` is the load-bearing one: the shipment
mock is delayed an hour so it is still in flight when a delayed callback calls
`env.CancelWorkflow` after a minute; the workflow error is a cancellation
(`temporal.IsCanceledError`), and — because `Compensate` uses a disconnected
context — both compensations still ran. `TestPlaceOrderNaive_SkipsCompensation`
runs the identical scenario against the naive workflow and asserts, via
`AssertNotCalled`, that its compensations did *not* run, proving the disconnected
context is what makes rollback survive cancellation.

Create `coordinator_test.go`:

```go
package sagacoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

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

func TestPlaceOrder_RollbackLIFO(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(CreateShipment, mock.Anything, mock.Anything).Return(errors.New("carrier down"))
	env.OnActivity(RefundPayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("RefundPayment"); return nil })
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("ReleaseInventory"); return nil })

	env.ExecuteWorkflow(PlaceOrder, Order{ID: "1"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, []string{"RefundPayment", "ReleaseInventory"}, rec.snapshot())
}

func TestPlaceOrder_CompensatesOnCancellation(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(nil)
	// Shipment stays in flight; the cancel lands before it completes.
	env.OnActivity(CreateShipment, mock.Anything, mock.Anything).Return(nil).After(time.Hour)
	env.OnActivity(RefundPayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("RefundPayment"); return nil })
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error { rec.add("ReleaseInventory"); return nil })

	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Minute)
	env.ExecuteWorkflow(PlaceOrder, Order{ID: "2"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()))
	require.Equal(t, []string{"RefundPayment", "ReleaseInventory"}, rec.snapshot())
}

func TestPlaceOrderNaive_SkipsCompensation(t *testing.T) {
	t.Parallel()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(CreateShipment, mock.Anything, mock.Anything).Return(nil).After(time.Hour)
	env.OnActivity(RefundPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Minute)
	env.ExecuteWorkflow(PlaceOrderNaive, Order{ID: "3"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	// Compensations scheduled on the cancelled context never actually run.
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything)
}

func ExampleSaga_CompensationPlan() {
	s := &Saga{}
	s.AddCompensation("ReleaseInventory", func(workflow.Context) error { return nil })
	s.AddCompensation("RefundPayment", func(workflow.Context) error { return nil })
	s.AddCompensation("CancelShipment", func(workflow.Context) error { return nil })
	fmt.Println(s.CompensationPlan())
	// Output: [CancelShipment RefundPayment ReleaseInventory]
}
```

## Review

The coordinator is correct when rollback runs newest-first and survives
cancellation. `TestPlaceOrder_RollbackLIFO` is the ordering proof and
`TestPlaceOrder_CompensatesOnCancellation` is the cancellation proof: both
compensations run even though the workflow error is a cancellation. The negative
test is what makes the lesson stick — the naive workflow is byte-for-byte the same
except it schedules compensation on the inherited context, and its compensations do
not run under cancellation. If you delete `NewDisconnectedContext` from
`Compensate`, `TestPlaceOrder_CompensatesOnCancellation` starts failing exactly like
the naive one.

The mistakes to avoid: never assume a `defer`-based rollback is cancellation-safe
just because it passes the failure test; failure and cancellation are different
paths, and only the second needs the disconnected context. Register a compensation
only after its forward step succeeds, so the stack reflects committed work. And keep
joining compensation errors with `errors.Join` so a failed rollback is visible.
Run the tests with the module present via `go test -race ./...`; offline, this
lesson is validated by its shape.

## Resources

- [`workflow.NewDisconnectedContext`](https://pkg.go.dev/go.temporal.io/sdk/workflow#NewDisconnectedContext) — a context detached from the parent's cancellation, for cleanup.
- [`go.temporal.io/sdk/temporal`](https://pkg.go.dev/go.temporal.io/sdk/temporal) — `IsCanceledError` and the application-error helpers.
- [`go.temporal.io/sdk/testsuite`](https://pkg.go.dev/go.temporal.io/sdk/testsuite) — `RegisterDelayedCallback` and `CancelWorkflow` for cancellation tests.
- [Temporal: cancellation and cleanup](https://docs.temporal.io/develop/go/cancellation) — why cleanup runs on a disconnected context.

---

Back to [01-saga-compensation-workflow.md](01-saga-compensation-workflow.md) | Next: [03-retryable-vs-terminal-failures.md](03-retryable-vs-terminal-failures.md)
