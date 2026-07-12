# Exercise 3: Retry Policies and Non-Retryable Business Errors

Retrying is not free. A payment gateway 503 deserves a backed-off retry; an
"insufficient funds" rejection does not — retrying it burns the whole retry budget
and delays the saga's compensation while the answer stays no. This exercise builds
the failure-taxonomy layer: a retry policy the workflow owns, and a classifier that
turns terminal business errors into `temporal.NewNonRetryableApplicationError` so
Temporal fails them fast.

This module is fully self-contained: its own `go mod init`, the classifier, the
workflow, a demo, and tests.

## What you'll build

```text
retrypolicy/                   independent module: example.com/retrypolicy
  go.mod                       go 1.26; requires go.temporal.io/sdk
  retry.go                     sentinel errors; ClassifyPaymentError; ProcessPayment workflow
  cmd/
    demo/
      main.go                  //go:build temporal: run one payment
  retry_test.go                testsuite: transient-then-success, non-retryable fail-fast, budget cap
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `ClassifyPaymentError` mapping transient errors to retryable and business rejections to `NewNonRetryableApplicationError` with a `Type`; a `ProcessPayment` workflow whose `ActivityOptions.RetryPolicy` sets backoff, `MaximumAttempts`, and `NonRetryableErrorTypes`, and that inspects the charge error with `errors.As` to branch on `*temporal.ApplicationError.Type()`.
- Test: a transient error retried twice then succeeding; a non-retryable error failing after exactly one attempt with the `ApplicationError` type recovered via `errors.As`; and an always-failing transient error capped by `MaximumAttempts`.
- Verify: `go test -count=1 -race ./...` (with the module fetched; offline this lesson is validated by shape).

Set up the module:

```bash
go get go.temporal.io/sdk@latest
```

### The failure taxonomy is the workflow's policy

Two independent mechanisms mark an error non-retryable, and this exercise uses
both on purpose. The first is at the activity: returning
`temporal.NewNonRetryableApplicationError(message, errType, cause)` tells Temporal
"do not retry this, whatever the policy says", and stamps the error with a `Type`
string you choose. The second is at the orchestrator: listing those same type
strings in `RetryPolicy.NonRetryableErrorTypes` stops retries for any error whose
type matches, which is how you make an error non-retryable when you cannot change
the code that produces it. `NewNonRetryableApplicationError` sets the flag on the
error object; `NonRetryableErrorTypes` matches on the type name. Ordinary errors
returned from an activity are retried under the policy's backoff.

`ClassifyPaymentError` is the pure boundary between the messy world of downstream
error values and Temporal's taxonomy. A gateway timeout or 503 is transient, so it
is returned unchanged and the retry policy handles it with exponential backoff. A
business rejection — insufficient funds, invalid card — is terminal, so it becomes
a non-retryable application error with a stable `Type`. Keeping this classification
in one pure function means it is unit-testable without a workflow, and the activity
body stays a thin wrapper over the real gateway call.

The workflow owns the `RetryPolicy`: `InitialInterval`, `BackoffCoefficient`,
`MaximumInterval`, `MaximumAttempts`, and `NonRetryableErrorTypes`. After the charge
returns an error, the workflow uses `errors.As` to pull the `*temporal.ApplicationError`
out of the wrapped activity error and branches on its `Type()`: a business rejection
is logged as terminal (the charge never committed, so there is nothing to refund —
only the reservation is released), while an infrastructure failure that exhausted its
retries is logged as such. The activity error is then returned *unwrapped*: wrapping
it with `fmt.Errorf` would make Temporal's failure converter turn the wrapper into
the top-level `ApplicationError` (with `Type` "wrapError"), hiding the business type
from `errors.As` on the caller side. Either way the reservation is released by the
deferred compensation.

Create `retry.go`:

```go
package retrypolicy

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Business-error type strings. These are the identifiers used both in the
// non-retryable application errors and in RetryPolicy.NonRetryableErrorTypes.
const (
	ErrTypeInsufficientFunds = "InsufficientFunds"
	ErrTypeInvalidCard       = "InvalidCard"
)

// Domain sentinel errors returned by the payment gateway stub.
var (
	ErrGatewayUnavailable = errors.New("payment gateway unavailable") // transient
	ErrInsufficientFunds  = errors.New("insufficient funds")          // terminal
	ErrInvalidCard        = errors.New("invalid card")                // terminal
)

// Order is the workflow input.
type Order struct {
	ID        string
	AmountUSD int
}

// ClassifyPaymentError maps a gateway error to Temporal's failure taxonomy:
// business rejections become non-retryable application errors (fail fast), while
// transient infrastructure errors are returned unchanged so the RetryPolicy
// retries them. nil maps to nil.
func ClassifyPaymentError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrInsufficientFunds):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInsufficientFunds, err)
	case errors.Is(err, ErrInvalidCard):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInvalidCard, err)
	default:
		return err // transient: retry under the policy
	}
}

// chargeGateway stands in for the external payment call. In production this is an
// HTTP/gRPC client; here the outcome is derived from the amount for a
// deterministic demo.
func chargeGateway(o Order) error {
	switch {
	case o.AmountUSD <= 0:
		return ErrInvalidCard
	case o.AmountUSD > 100000:
		return ErrInsufficientFunds
	default:
		return nil
	}
}

func ReserveInventory(_ context.Context, _ Order) error { return nil }
func ReleaseInventory(_ context.Context, _ Order) error { return nil }

// ChargePayment calls the gateway and classifies the result so Temporal retries
// transient faults and fails business rejections fast.
func ChargePayment(_ context.Context, o Order) error {
	return ClassifyPaymentError(chargeGateway(o))
}

func isBusinessRejection(t string) bool {
	return t == ErrTypeInsufficientFunds || t == ErrTypeInvalidCard
}

// ProcessPayment reserves inventory, then charges under a retry policy. A
// transient charge failure is retried with backoff up to MaximumAttempts; a
// business rejection fails fast. On any charge failure the reservation is
// released; no refund is issued because the charge never committed.
func ProcessPayment(ctx workflow.Context, o Order) (err error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        time.Second,
			BackoffCoefficient:     2.0,
			MaximumInterval:        30 * time.Second,
			MaximumAttempts:        5,
			NonRetryableErrorTypes: []string{ErrTypeInsufficientFunds, ErrTypeInvalidCard},
		},
	})
	logger := workflow.GetLogger(ctx)

	if err = workflow.ExecuteActivity(ctx, ReserveInventory, o).Get(ctx, nil); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if ce := workflow.ExecuteActivity(ctx, ReleaseInventory, o).Get(ctx, nil); ce != nil {
				err = errors.Join(err, ce)
			}
		}
	}()

	chargeErr := workflow.ExecuteActivity(ctx, ChargePayment, o).Get(ctx, nil)
	if chargeErr != nil {
		var appErr *temporal.ApplicationError
		if errors.As(chargeErr, &appErr) && isBusinessRejection(appErr.Type()) {
			logger.Info("payment rejected; no refund needed", "type", appErr.Type())
		} else {
			logger.Error("payment failed after retries", "error", chargeErr.Error())
		}
		// Return the activity error unwrapped: wrapping it with fmt.Errorf would
		// make Temporal convert the wrapper into the top-level ApplicationError
		// (Type "wrapError"), hiding the business Type from errors.As on the
		// caller side.
		err = chargeErr
		return err
	}
	return nil
}
```

### The runnable demo

Behind the `temporal` tag: dial the dev server, register the workflow and
activities, run one payment with a normal amount (which succeeds), and print the
result.

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

	"example.com/retrypolicy"
)

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	const taskQueue = "payment-processing"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(retrypolicy.ProcessPayment)
	w.RegisterActivity(retrypolicy.ReserveInventory)
	w.RegisterActivity(retrypolicy.ChargePayment)
	w.RegisterActivity(retrypolicy.ReleaseInventory)

	if err := w.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "pay-3003",
		TaskQueue: taskQueue,
	}, retrypolicy.ProcessPayment, retrypolicy.Order{ID: "3003", AmountUSD: 4200})
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}

	if err := run.Get(context.Background(), nil); err != nil {
		fmt.Printf("payment %s failed: %v\n", run.GetID(), err)
		return
	}
	fmt.Printf("payment %s processed\n", run.GetID())
}
```

Run it against a local dev server (`temporal server start-dev`):

```bash
go run -tags temporal ./cmd/demo
```

Expected output (stdout result line; worker logs go to stderr):

```
payment pay-3003 processed
```

### Tests

`TestProcessPayment_TransientRetriedThenSucceeds` returns `ErrGatewayUnavailable`
for the first two attempts and `nil` on the third; the workflow completes with no
error and the charge activity ran exactly three times, proving the transient error
was retried. `TestProcessPayment_NonRetryableFailsFast` returns a non-retryable
application error and asserts the charge ran exactly once (no retries) and that
`errors.As` recovers an `*temporal.ApplicationError` whose `Type()` is
`InsufficientFunds`. `TestProcessPayment_RetryBudgetCapped` returns a transient
error forever and asserts the workflow eventually errors with a bounded number of
attempts, proving `MaximumAttempts` stops the retries rather than looping. The
attempt counter is an `atomic.Int32` incremented inside the mock's return function,
which the retry loop calls once per attempt.

Create `retry_test.go`:

```go
package retrypolicy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestProcessPayment_TransientRetriedThenSucceeds(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error {
			if attempts.Add(1) <= 2 {
				return ErrGatewayUnavailable
			}
			return nil
		})

	env.ExecuteWorkflow(ProcessPayment, Order{ID: "1", AmountUSD: 100})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, int32(3), attempts.Load())
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything)
}

func TestProcessPayment_NonRetryableFailsFast(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error {
			attempts.Add(1)
			return temporal.NewNonRetryableApplicationError(
				"insufficient funds", ErrTypeInsufficientFunds, ErrInsufficientFunds)
		})

	env.ExecuteWorkflow(ProcessPayment, Order{ID: "2", AmountUSD: 999999})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.Equal(t, int32(1), attempts.Load()) // failed fast, no retries

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr))
	require.Equal(t, ErrTypeInsufficientFunds, appErr.Type())
	env.AssertActivityCalled(t, "ReleaseInventory", mock.Anything, mock.Anything)
}

func TestProcessPayment_RetryBudgetCapped(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	env.OnActivity(ReserveInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ReleaseInventory, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(ChargePayment, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ Order) error {
			attempts.Add(1)
			return ErrGatewayUnavailable // always transient
		})

	env.ExecuteWorkflow(ProcessPayment, Order{ID: "3", AmountUSD: 100})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	// MaximumAttempts caps the retries: it stops rather than looping forever.
	// The test env may run one extra attempt (a known quirk), so assert a bound
	// instead of an exact count.
	got := attempts.Load()
	require.GreaterOrEqual(t, got, int32(2))
	require.LessOrEqual(t, got, int32(6))
	env.AssertActivityCalled(t, "ReleaseInventory", mock.Anything, mock.Anything)
}

func ExampleClassifyPaymentError() {
	// A business rejection becomes a non-retryable application error.
	rejected := ClassifyPaymentError(ErrInsufficientFunds)
	var appErr *temporal.ApplicationError
	if errors.As(rejected, &appErr) {
		fmt.Printf("%s nonRetryable=%v\n", appErr.Type(), appErr.NonRetryable())
	}
	// A transient error is returned unchanged, so the policy retries it.
	fmt.Println(ClassifyPaymentError(ErrGatewayUnavailable))
	// nil stays nil.
	fmt.Println(ClassifyPaymentError(nil))
	// Output:
	// InsufficientFunds nonRetryable=true
	// payment gateway unavailable
	// <nil>
}
```

## Review

The classifier is correct when the taxonomy is exhaustive and pure: transient in,
transient out; business rejection in, non-retryable application error out with a
stable `Type`; nil in, nil out. The transient-then-success test proves retries
happen (three attempts) and the non-retryable test proves they do not when the
error is terminal (one attempt), with the `ApplicationError` type recovered through
the wrapped activity error via `errors.As`. If a business rejection were left as an
ordinary error, the non-retryable test would show five attempts instead of one — the
exact latency-and-load waste the taxonomy exists to prevent.

Two things to keep honest. First, `MaximumAttempts` counts total attempts including
the first, so `MaximumAttempts: 5` means at most four retries; and the in-memory test
environment has historically run one extra attempt at the cap, which is why the
budget-cap test asserts a bound rather than an exact equality. Second, put the retry
policy on the workflow's `ActivityOptions`, not inside the activity: backoff, budget,
and the non-retryable type list are orchestration policy the workflow owns, and an
activity that retries internally hides that policy and defeats Temporal's own
retrying. Run the tests with the module present via `go test -race ./...`; offline,
this lesson is validated by its shape.

## Resources

- [`go.temporal.io/sdk/temporal`](https://pkg.go.dev/go.temporal.io/sdk/temporal) — `RetryPolicy`, `NewNonRetryableApplicationError`, `IsApplicationError`, `ApplicationError.Type`.
- [Temporal: failure detection (Go SDK)](https://docs.temporal.io/develop/go/failure-detection) — retry policies and non-retryable application errors.
- [`errors.As`](https://pkg.go.dev/errors#As) — recovering the `*temporal.ApplicationError` from a wrapped activity error.

---

Back to [02-saga-coordinator-disconnected-ctx.md](02-saga-coordinator-disconnected-ctx.md) | Next: [../09-river-postgres-job-queue/00-concepts.md](../09-river-postgres-job-queue/00-concepts.md)
