# 17. Saga Orchestrator

A Saga is a sequence of local transactions, each paired with a compensating
transaction that undoes its effect. If any step fails, the orchestrator runs
compensations in reverse order for all completed steps, reaching eventual
consistency without distributed locks. The hard parts are: compensations can
themselves fail; every step and every compensation must be idempotent; the
orchestrator must survive crashes and resume. This lesson builds an in-process
saga engine and tests all three failure modes.

```text
saga/
  go.mod
  saga.go
  saga_test.go
  cmd/demo/main.go
```

## Concepts

### Saga vs Two-Phase Commit

Two-phase commit (2PC) blocks all participants while a coordinator collects
votes. If the coordinator crashes between prepare and commit, participants hold
locks indefinitely. A saga avoids this: each participant commits its local
transaction immediately, so no locks are held across the network. The cost is
that consistency is achieved only eventually, through the compensation path.

The original paper (Garcia-Molina & Salem, 1987) defines a saga as a finite
sequence of transactions T1 ... Tn with compensating transactions C1 ... Cn
such that: either all Ti complete, or some prefix T1 ... Tk completes and
Ck ... C1 run to undo the effect. Compensation is backward: the last successful
forward step is compensated first.

### Orchestration vs Choreography

In the orchestration pattern, a single coordinator knows the full workflow and
drives each participant. In the choreography pattern, participants react to
events from the previous step; there is no central coordinator. Orchestration
makes the control flow explicit and observable; choreography is more decoupled
but harder to reason about when failures occur.

### Idempotency

A step is idempotent if running it twice has the same effect as running it
once. Idempotency is required because the orchestrator may retry a step after
a timeout or a crash: the step may have already succeeded, but the
acknowledgment was lost. Compensations must also be idempotent for the same
reason.

A common technique is to pass an idempotency key (e.g. `sagaID + stepIndex`)
to each participant. The participant records whether it has already processed
that key and returns the stored result instead of re-executing.

### Compensation Retry and the "Stuck" State

Compensations can fail transiently (network partition, participant restarting).
The orchestrator retries with exponential backoff. If the backoff budget is
exhausted, the saga enters a "stuck" state that requires manual intervention:
no automatic rollback is possible without risking inconsistency.

### Persistent Saga Log

To survive a crash, the orchestrator writes the outcome of each step to a
durable log before moving to the next step. On restart, it replays the log to
determine which steps completed and resumes from the first incomplete step
(forward) or runs compensations for all completed steps (backward).

This lesson uses an in-memory log behind an interface so the engine is
testable without a database.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/saga/cmd/demo
cd ~/go-exercises/saga
go mod init example.com/saga
```

This is a library. Verification is done with `go test`, not by running a
program.

### Exercise 1: Core Types and the Orchestrator

Create `saga.go`:

```go
package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// State represents the current phase of a saga execution.
type State int

const (
	StatePending      State = iota // not yet started
	StateExecuting                 // stepping forward
	StateCompensating              // stepping backward after failure
	StateCompleted                 // all forward steps succeeded
	StateFailed                    // forward failed and compensation succeeded
	StateStuck                     // forward failed and compensation also failed
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateExecuting:
		return "executing"
	case StateCompensating:
		return "compensating"
	case StateCompleted:
		return "completed"
	case StateFailed:
		return "failed"
	case StateStuck:
		return "stuck"
	default:
		return "unknown"
	}
}

// StepOutcome records the result of one step in the log.
type StepOutcome int

const (
	OutcomePending     StepOutcome = iota
	OutcomeSucceeded               // forward step completed
	OutcomeCompensated             // compensation completed
)

// LogEntry is a single durable record in the saga log.
type LogEntry struct {
	SagaID    string
	StepIndex int
	Outcome   StepOutcome
	At        time.Time
}

// Log is the persistence interface for saga state.
type Log interface {
	Append(entry LogEntry) error
	Entries(sagaID string) ([]LogEntry, error)
}

// MemLog is a thread-safe in-memory Log for testing.
type MemLog struct {
	mu      sync.Mutex
	entries map[string][]LogEntry
}

// NewMemLog returns an empty in-memory log.
func NewMemLog() *MemLog {
	return &MemLog{entries: make(map[string][]LogEntry)}
}

func (m *MemLog) Append(e LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[e.SagaID] = append(m.entries[e.SagaID], e)
	return nil
}

func (m *MemLog) Entries(sagaID string) ([]LogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]LogEntry, len(m.entries[sagaID]))
	copy(cp, m.entries[sagaID])
	return cp, nil
}

// Step is one unit of work in a saga: a forward action and its compensation.
// Both Execute and Compensate must be idempotent.
type Step[T any] struct {
	Name       string
	Execute    func(ctx context.Context, data T) error
	Compensate func(ctx context.Context, data T) error
}

// Sentinel errors.
var (
	ErrStepFailed         = errors.New("saga: step failed")
	ErrCompensationFailed = errors.New("saga: compensation failed after retries")
)

// Result is the final report of a saga run.
type Result struct {
	SagaID         string
	FinalState     State
	FailedStep     int
	FailedStepName string
}

// Orchestrator executes sagas defined as ordered slices of Step.
type Orchestrator[T any] struct {
	log            Log
	maxCompRetries int
	retryDelay     time.Duration
}

// New creates an Orchestrator. maxCompRetries is the maximum number of times
// to retry a failing compensation (minimum 0 = try once). retryDelay is the
// base delay between retries (doubled on each retry).
func New[T any](log Log, maxCompRetries int, retryDelay time.Duration) *Orchestrator[T] {
	if maxCompRetries < 0 {
		maxCompRetries = 0
	}
	return &Orchestrator[T]{log: log, maxCompRetries: maxCompRetries, retryDelay: retryDelay}
}

// Run executes the saga identified by sagaID over steps. It returns a Result
// describing the final state. Run is safe for concurrent calls with different
// sagaIDs.
func (o *Orchestrator[T]) Run(ctx context.Context, sagaID string, steps []Step[T], data T) (Result, error) {
	res := Result{SagaID: sagaID, FailedStep: -1}

	// Replay the log to find how far we got (crash recovery).
	entries, err := o.log.Entries(sagaID)
	if err != nil {
		return res, fmt.Errorf("saga: read log: %w", err)
	}

	succeeded := make(map[int]bool)
	compensated := make(map[int]bool)
	for _, e := range entries {
		switch e.Outcome {
		case OutcomeSucceeded:
			succeeded[e.StepIndex] = true
		case OutcomeCompensated:
			compensated[e.StepIndex] = true
		}
	}

	// Forward pass.
	for i, step := range steps {
		if succeeded[i] {
			continue // already completed before crash; idempotent skip
		}
		if err := step.Execute(ctx, data); err != nil {
			res.FailedStep = i
			res.FailedStepName = step.Name
			// Compensate steps 0..i-1 in reverse.
			if cErr := o.compensate(ctx, sagaID, steps, i-1, succeeded, compensated, data); cErr != nil {
				res.FinalState = StateStuck
				return res, fmt.Errorf("%w: step %q failed (%v); compensation also failed (%v)",
					ErrCompensationFailed, step.Name, err, cErr)
			}
			res.FinalState = StateFailed
			return res, fmt.Errorf("%w: step %q: %v", ErrStepFailed, step.Name, err)
		}
		if logErr := o.log.Append(LogEntry{
			SagaID:    sagaID,
			StepIndex: i,
			Outcome:   OutcomeSucceeded,
			At:        time.Now().UTC(),
		}); logErr != nil {
			return res, fmt.Errorf("saga: write log: %w", logErr)
		}
		succeeded[i] = true
	}

	res.FinalState = StateCompleted
	return res, nil
}

// compensate runs compensations for steps 0..upTo in reverse order, skipping
// steps that were already compensated (idempotency on the compensation path).
func (o *Orchestrator[T]) compensate(
	ctx context.Context,
	sagaID string,
	steps []Step[T],
	upTo int,
	succeeded map[int]bool,
	compensated map[int]bool,
	data T,
) error {
	for i := upTo; i >= 0; i-- {
		if !succeeded[i] || compensated[i] {
			continue
		}
		if err := o.retryCompensate(ctx, steps[i], data); err != nil {
			return fmt.Errorf("compensate step %d %q: %w", i, steps[i].Name, err)
		}
		_ = o.log.Append(LogEntry{
			SagaID:    sagaID,
			StepIndex: i,
			Outcome:   OutcomeCompensated,
			At:        time.Now().UTC(),
		})
		compensated[i] = true
	}
	return nil
}

// retryCompensate calls step.Compensate with exponential backoff.
func (o *Orchestrator[T]) retryCompensate(ctx context.Context, step Step[T], data T) error {
	delay := o.retryDelay
	for attempt := 0; attempt <= o.maxCompRetries; attempt++ {
		err := step.Compensate(ctx, data)
		if err == nil {
			return nil
		}
		if attempt == o.maxCompRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return nil
}
```

### Exercise 2: Test the Contract

Create `saga_test.go`:

```go
package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------------

func noopStep(name string) Step[*orderData] {
	return Step[*orderData]{
		Name:       name,
		Execute:    func(_ context.Context, _ *orderData) error { return nil },
		Compensate: func(_ context.Context, _ *orderData) error { return nil },
	}
}

type orderData struct {
	mu          sync.Mutex
	reserved    bool
	charged     bool
	compensated []string
}

func (d *orderData) record(action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.compensated = append(d.compensated, action)
}

func newOrch() *Orchestrator[*orderData] {
	return New[*orderData](NewMemLog(), 2, time.Millisecond)
}

// --- tests -------------------------------------------------------------------

func TestSuccessfulSaga(t *testing.T) {
	t.Parallel()

	orch := newOrch()
	data := &orderData{}

	steps := []Step[*orderData]{
		{
			Name: "reserve",
			Execute: func(_ context.Context, d *orderData) error {
				d.reserved = true
				return nil
			},
			Compensate: func(_ context.Context, d *orderData) error {
				d.reserved = false
				return nil
			},
		},
		{
			Name: "charge",
			Execute: func(_ context.Context, d *orderData) error {
				d.charged = true
				return nil
			},
			Compensate: func(_ context.Context, d *orderData) error {
				d.charged = false
				return nil
			},
		},
	}

	res, err := orch.Run(context.Background(), "saga-1", steps, data)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if res.FinalState != StateCompleted {
		t.Fatalf("state = %v, want completed", res.FinalState)
	}
	if !data.reserved || !data.charged {
		t.Fatal("both steps should have executed")
	}
}

func TestFailureAtFirstStep(t *testing.T) {
	t.Parallel()

	orch := newOrch()
	data := &orderData{}
	failErr := errors.New("reserve failed")

	steps := []Step[*orderData]{
		{
			Name:       "reserve",
			Execute:    func(_ context.Context, _ *orderData) error { return failErr },
			Compensate: func(_ context.Context, _ *orderData) error { return nil },
		},
		noopStep("charge"),
	}

	res, err := orch.Run(context.Background(), "saga-2", steps, data)
	if !errors.Is(err, ErrStepFailed) {
		t.Fatalf("err = %v, want ErrStepFailed", err)
	}
	if res.FinalState != StateFailed {
		t.Fatalf("state = %v, want failed", res.FinalState)
	}
	if res.FailedStep != 0 {
		t.Fatalf("FailedStep = %d, want 0", res.FailedStep)
	}
}

func TestFailureAtSecondStepCompensatesFirst(t *testing.T) {
	t.Parallel()

	orch := newOrch()
	data := &orderData{}
	failErr := errors.New("charge failed")

	steps := []Step[*orderData]{
		{
			Name: "reserve",
			Execute: func(_ context.Context, d *orderData) error {
				d.reserved = true
				return nil
			},
			Compensate: func(_ context.Context, d *orderData) error {
				d.record("compensate-reserve")
				d.reserved = false
				return nil
			},
		},
		{
			Name:       "charge",
			Execute:    func(_ context.Context, _ *orderData) error { return failErr },
			Compensate: func(_ context.Context, _ *orderData) error { return nil },
		},
	}

	res, err := orch.Run(context.Background(), "saga-3", steps, data)
	if !errors.Is(err, ErrStepFailed) {
		t.Fatalf("err = %v, want ErrStepFailed", err)
	}
	if res.FinalState != StateFailed {
		t.Fatalf("state = %v, want failed", res.FinalState)
	}
	if data.reserved {
		t.Fatal("reserve should have been compensated")
	}
	data.mu.Lock()
	got := data.compensated
	data.mu.Unlock()
	if len(got) == 0 || got[0] != "compensate-reserve" {
		t.Fatalf("compensations = %v, want [compensate-reserve]", got)
	}
}

func TestCompensationFailureCausesStuck(t *testing.T) {
	t.Parallel()

	// Zero retries so the test is fast.
	orch := New[*orderData](NewMemLog(), 0, time.Millisecond)
	data := &orderData{}
	compErr := errors.New("cannot refund")

	steps := []Step[*orderData]{
		{
			Name:       "reserve",
			Execute:    func(_ context.Context, _ *orderData) error { return nil },
			Compensate: func(_ context.Context, _ *orderData) error { return compErr },
		},
		{
			Name:       "charge",
			Execute:    func(_ context.Context, _ *orderData) error { return errors.New("charge failed") },
			Compensate: func(_ context.Context, _ *orderData) error { return nil },
		},
	}

	res, err := orch.Run(context.Background(), "saga-4", steps, data)
	if !errors.Is(err, ErrCompensationFailed) {
		t.Fatalf("err = %v, want ErrCompensationFailed", err)
	}
	if res.FinalState != StateStuck {
		t.Fatalf("state = %v, want stuck", res.FinalState)
	}
}

func TestCompensationRetry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	// Fail the first call, succeed on the second.
	compensate := func(_ context.Context, _ *orderData) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	}

	orch := New[*orderData](NewMemLog(), 2, time.Millisecond)
	data := &orderData{}

	steps := []Step[*orderData]{
		{
			Name:       "reserve",
			Execute:    func(_ context.Context, _ *orderData) error { return nil },
			Compensate: compensate,
		},
		{
			Name:       "charge",
			Execute:    func(_ context.Context, _ *orderData) error { return errors.New("charge failed") },
			Compensate: func(_ context.Context, _ *orderData) error { return nil },
		},
	}

	res, err := orch.Run(context.Background(), "saga-5", steps, data)
	if !errors.Is(err, ErrStepFailed) {
		t.Fatalf("err = %v, want ErrStepFailed", err)
	}
	if res.FinalState != StateFailed {
		t.Fatalf("state = %v, want failed (compensation should have eventually succeeded)", res.FinalState)
	}
	if calls.Load() < 2 {
		t.Fatalf("compensate called %d times, want >= 2", calls.Load())
	}
}

func TestCrashRecovery(t *testing.T) {
	t.Parallel()

	log := NewMemLog()
	data := &orderData{}

	var step0Calls atomic.Int32

	steps := []Step[*orderData]{
		{
			Name: "reserve",
			Execute: func(_ context.Context, d *orderData) error {
				step0Calls.Add(1)
				d.reserved = true
				return nil
			},
			Compensate: func(_ context.Context, _ *orderData) error { return nil },
		},
		{
			Name: "charge",
			Execute: func(_ context.Context, d *orderData) error {
				d.charged = true
				return nil
			},
			Compensate: func(_ context.Context, _ *orderData) error { return nil },
		},
	}

	// Simulate: first orchestrator ran step 0 and logged it, then crashed.
	_ = log.Append(LogEntry{
		SagaID:    "saga-recover",
		StepIndex: 0,
		Outcome:   OutcomeSucceeded,
		At:        time.Now().UTC(),
	})

	// Second orchestrator picks up from the log.
	orch := New[*orderData](log, 0, time.Millisecond)
	res, err := orch.Run(context.Background(), "saga-recover", steps, data)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if res.FinalState != StateCompleted {
		t.Fatalf("state = %v, want completed", res.FinalState)
	}
	// Step 0 must NOT be called again (idempotent skip via log replay).
	if step0Calls.Load() != 0 {
		t.Fatalf("reserve Execute called %d times after recovery, want 0", step0Calls.Load())
	}
	if !data.charged {
		t.Fatal("step 1 (charge) should have executed on recovery")
	}
}

func TestConcurrentSagasAreIndependent(t *testing.T) {
	t.Parallel()

	log := NewMemLog()
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			orch := New[*orderData](log, 0, time.Millisecond)
			steps := []Step[*orderData]{noopStep("a"), noopStep("b")}
			d := &orderData{}
			res, err := orch.Run(context.Background(), fmt.Sprintf("concurrent-%d", id), steps, d)
			if err != nil {
				errs[id] = err
				return
			}
			if res.FinalState != StateCompleted {
				errs[id] = fmt.Errorf("saga %d: state = %v, want completed", id, res.FinalState)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v", i, e)
		}
	}
}

func TestStateString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		s    State
		want string
	}{
		{StatePending, "pending"},
		{StateExecuting, "executing"},
		{StateCompensating, "compensating"},
		{StateCompleted, "completed"},
		{StateFailed, "failed"},
		{StateStuck, "stuck"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// ExampleOrchestrator_Run shows the order-processing saga: reserve inventory,
// charge payment. If charging fails, inventory is released.
func ExampleOrchestrator_Run() {
	type order struct {
		reserved bool
		charged  bool
	}
	o := &order{}

	steps := []Step[*order]{
		{
			Name: "reserve",
			Execute: func(_ context.Context, d *order) error {
				d.reserved = true
				return nil
			},
			Compensate: func(_ context.Context, d *order) error {
				d.reserved = false
				return nil
			},
		},
		{
			Name:       "charge",
			Execute:    func(_ context.Context, _ *order) error { return errors.New("card declined") },
			Compensate: func(_ context.Context, _ *order) error { return nil },
		},
	}

	orch := New[*order](NewMemLog(), 0, time.Millisecond)
	res, _ := orch.Run(context.Background(), "order-1", steps, o)
	fmt.Printf("state=%s reserved=%v\n", res.FinalState, o.reserved)
	// Output: state=failed reserved=false
}
```

Your turn: add `TestFailureAtThirdStep` that defines three steps where step 2
fails, and asserts that `res.FailedStep == 2` and both steps 0 and 1 are
compensated in reverse order (step 1 compensated before step 0).

### Exercise 3: The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/saga"
)

func main() {
	type orderData struct {
		items    int
		reserved bool
		charged  bool
	}

	log := saga.NewMemLog()
	orch := saga.New[*orderData](log, 1, 0)

	o := &orderData{items: 3}

	steps := []saga.Step[*orderData]{
		{
			Name: "reserve-inventory",
			Execute: func(_ context.Context, d *orderData) error {
				fmt.Printf("  execute: reserve %d items\n", d.items)
				d.reserved = true
				return nil
			},
			Compensate: func(_ context.Context, d *orderData) error {
				fmt.Println("  compensate: release inventory")
				d.reserved = false
				return nil
			},
		},
		{
			Name: "charge-payment",
			Execute: func(_ context.Context, d *orderData) error {
				fmt.Println("  execute: charge payment")
				d.charged = true
				return nil
			},
			Compensate: func(_ context.Context, d *orderData) error {
				fmt.Println("  compensate: refund payment")
				d.charged = false
				return nil
			},
		},
		{
			Name: "create-shipment",
			Execute: func(_ context.Context, _ *orderData) error {
				fmt.Println("  execute: create shipment -> FAIL")
				return errors.New("warehouse unavailable")
			},
			Compensate: func(_ context.Context, _ *orderData) error {
				fmt.Println("  compensate: cancel shipment")
				return nil
			},
		},
	}

	fmt.Println("=== order saga: shipment fails ===")
	res, err := orch.Run(context.Background(), "demo-order", steps, o)
	fmt.Printf("final state : %s\n", res.FinalState)
	fmt.Printf("failed step : %s\n", res.FailedStepName)
	fmt.Printf("saga error  : %v\n", err)
	fmt.Printf("reserved    : %v (should be false — compensated)\n", o.reserved)
	fmt.Printf("charged     : %v (should be false — compensated)\n", o.charged)
}
```

Run it with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Compensating in Forward Order Instead of Reverse

Wrong: on failure after steps A, B, C, compensate A then B then C.

Fix: compensate C then B then A. Forward compensation re-introduces
inconsistency: B's compensation may depend on C having been undone first. The
engine in this lesson iterates `for i := upTo; i >= 0; i--`.

### Not Logging Before Moving Forward

Wrong: execute step i, then move to step i+1, then log that step i succeeded.

Fix: log step i's success before starting step i+1. If the orchestrator
crashes between log-write and start of step i+1, the replay correctly skips
step i. If the log write happens after starting step i+1, a crash leaves the
log in an inconsistent state.

### Mutable Shared State in Data

Wrong: passing a single `orderData` pointer to concurrent sagas and having
steps mutate it without a lock.

Fix: give each saga its own data value, or add a mutex to the data struct.
The tests in this lesson give each goroutine its own `&orderData{}`.

### Non-Idempotent Compensations

Wrong: a refund function that charges the account for a negative amount
unconditionally. If called twice, it charges twice.

Fix: the participant records which idempotency keys have been processed. On a
duplicate call, it returns the stored outcome without executing again.

## Verification

From `~/go-exercises/saga`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector catches data races between concurrent
sagas.

## Summary

- A saga is a sequence of local transactions paired with compensating
  transactions; compensation runs in reverse order on failure.
- Orchestration centralizes control; choreography distributes it through events.
- Every step and every compensation must be idempotent to be safe under retries.
- The orchestrator logs step outcomes durably before advancing; replay enables
  crash recovery.
- Compensation failures are retried with exponential backoff; exhausted retries
  leave the saga stuck, requiring manual intervention.
- Sagas trade atomicity for availability: no distributed locks are held.

## What's Next

Next: [Event Sourcing Engine](../18-event-sourcing-engine/18-event-sourcing-engine.md).

## Resources

- [Sagas, Garcia-Molina & Salem, 1987](https://www.cs.cornell.edu/andru/cs711/2002fa/reading/sagas.pdf) -- original paper defining the pattern
- [Saga pattern, microservices.io](https://microservices.io/patterns/data/saga.html) -- modern application to microservices
- [context package, pkg.go.dev](https://pkg.go.dev/context) -- cancellation and deadline propagation used in Step.Execute
- [sync/atomic package, pkg.go.dev](https://pkg.go.dev/sync/atomic) -- atomic counters used in retry and concurrency tests
- [errors package, pkg.go.dev](https://pkg.go.dev/errors) -- sentinel errors and errors.Is for test assertions
