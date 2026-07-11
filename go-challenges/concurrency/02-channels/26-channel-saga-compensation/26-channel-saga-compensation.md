# Exercise 26: Channel-Based Saga / Compensation

A checkout is not one database transaction. It spans independent, potentially remote services -- inventory reserves stock, payment charges a card, shipping creates a label, notification confirms -- and none of them can join a distributed two-phase commit. So when the payment charge succeeds but the shipping service is down, you must reverse the payment yourself: automatically, reliably, and in the right order. This exercise builds that reversal as an orchestrated saga, where each step is a goroutine that knows how to do its action and how to undo it, and a coordinator that runs forward until a failure and then compensates backward.

## What you'll build

```text
channel-saga-compensation/
  main.go        the forward-only happy path, compensation on payment
                 failure, reverse compensation of two steps, and a full
                 timeline log comparing a committed and a rolled-back saga
```

- Build: an orchestrated checkout saga over channel-connected step goroutines that commits on success and rolls back in reverse on failure.
- Implement: a `sagaStep` goroutine handling `execute` and `compensate` on a `StepCommand` channel; a coordinator that stops at the first failure and compensates completed steps from `completedSteps - 1` down to 0; and a `TimelineEntry` log with relative timestamps.
- Verify: `go run main.go` at each step, watching the forward and compensation phases in the output.

### Why compensation runs in reverse

The saga pattern solves this: define each step as a forward action and a compensating action. Execute steps in sequence. If step N fails, execute compensations for steps N-1 through 1 in reverse order. This is "choreography" when each service triggers the next, or "orchestration" when a central coordinator drives the flow.

In Go, the orchestrated saga maps naturally to channels: each step is a goroutine with an input command channel and an output result channel. The coordinator sends commands and reads results. On failure, it sends compensation commands to previously completed steps in reverse order. Each step's goroutine handles both its forward action and its compensation, keeping the logic co-located and testable.

## Step 1 -- Happy Path: Four Sequential Steps

Build the forward-only path: four steps execute in sequence through channels. No failure handling yet.

```go
package main

import (
	"fmt"
	"time"
)

const stepDelay = 30 * time.Millisecond

// StepResult carries the outcome of a saga step.
type StepResult struct {
	StepName string
	Success  bool
	Message  string
}

// StepCommand tells a step what to do.
type StepCommand struct {
	Action  string // "execute" or "compensate"
	OrderID string
}

// sagaStep runs as a goroutine, processing commands on its input channel.
func sagaStep(name string, input <-chan StepCommand, output chan<- StepResult) {
	for cmd := range input {
		time.Sleep(stepDelay)
		switch cmd.Action {
		case "execute":
			output <- StepResult{
				StepName: name,
				Success:  true,
				Message:  fmt.Sprintf("%s completed for order %s", name, cmd.OrderID),
			}
		case "compensate":
			output <- StepResult{
				StepName: name,
				Success:  true,
				Message:  fmt.Sprintf("%s compensated for order %s", name, cmd.OrderID),
			}
		}
	}
}

func main() {
	orderID := "ORD-1001"

	stepNames := []string{"reserve-inventory", "charge-payment", "create-shipment", "send-confirmation"}

	type stepChannels struct {
		name   string
		cmdCh  chan StepCommand
		resCh  chan StepResult
	}

	steps := make([]stepChannels, len(stepNames))
	for i, name := range stepNames {
		cmdCh := make(chan StepCommand, 1)
		resCh := make(chan StepResult, 1)
		steps[i] = stepChannels{name: name, cmdCh: cmdCh, resCh: resCh}
		go sagaStep(name, cmdCh, resCh)
	}

	// Execute all steps in sequence.
	fmt.Printf("=== Saga: Order %s ===\n\n", orderID)
	for _, step := range steps {
		step.cmdCh <- StepCommand{Action: "execute", OrderID: orderID}
		result := <-step.resCh
		fmt.Printf("[FORWARD] %s\n", result.Message)
	}

	// Cleanup: close all command channels.
	for _, step := range steps {
		close(step.cmdCh)
	}

	fmt.Printf("\nSaga completed successfully\n")
}
```

Each step is a goroutine with its own command/result channel pair. The coordinator sends "execute" and waits for the result before proceeding to the next step. This is the simplest orchestrated saga.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Saga: Order ORD-1001 ===

[FORWARD] reserve-inventory completed for order ORD-1001
[FORWARD] charge-payment completed for order ORD-1001
[FORWARD] create-shipment completed for order ORD-1001
[FORWARD] send-confirmation completed for order ORD-1001

Saga completed successfully
```

## Step 2 -- Compensation on Payment Failure

Make the payment step fail. The coordinator detects the failure and compensates the inventory step (the only completed step).

```go
package main

import (
	"fmt"
	"time"
)

const compStepDelay = 30 * time.Millisecond

type StepResult struct {
	StepName string
	Success  bool
	Message  string
}

type StepCommand struct {
	Action  string
	OrderID string
}

// sagaStep simulates a step. failOnExecute controls whether execute fails.
func sagaStep(name string, failOnExecute bool, input <-chan StepCommand, output chan<- StepResult) {
	for cmd := range input {
		time.Sleep(compStepDelay)
		switch cmd.Action {
		case "execute":
			if failOnExecute {
				output <- StepResult{
					StepName: name,
					Success:  false,
					Message:  fmt.Sprintf("%s FAILED for order %s", name, cmd.OrderID),
				}
			} else {
				output <- StepResult{
					StepName: name,
					Success:  true,
					Message:  fmt.Sprintf("%s completed for order %s", name, cmd.OrderID),
				}
			}
		case "compensate":
			output <- StepResult{
				StepName: name,
				Success:  true,
				Message:  fmt.Sprintf("%s compensated for order %s", name, cmd.OrderID),
			}
		}
	}
}

type stepChannels struct {
	name  string
	cmdCh chan StepCommand
	resCh chan StepResult
}

func main() {
	orderID := "ORD-1002"

	// Payment (index 1) will fail.
	stepDefs := []struct {
		name string
		fail bool
	}{
		{"reserve-inventory", false},
		{"charge-payment", true},
		{"create-shipment", false},
		{"send-confirmation", false},
	}

	steps := make([]stepChannels, len(stepDefs))
	for i, def := range stepDefs {
		cmdCh := make(chan StepCommand, 1)
		resCh := make(chan StepResult, 1)
		steps[i] = stepChannels{name: def.name, cmdCh: cmdCh, resCh: resCh}
		go sagaStep(def.name, def.fail, cmdCh, resCh)
	}

	fmt.Printf("=== Saga: Order %s ===\n\n", orderID)

	// Forward execution: stop at first failure.
	completedSteps := 0
	var failedStep string
	for _, step := range steps {
		step.cmdCh <- StepCommand{Action: "execute", OrderID: orderID}
		result := <-step.resCh
		if result.Success {
			fmt.Printf("[FORWARD] %s\n", result.Message)
			completedSteps++
		} else {
			fmt.Printf("[FAILED]  %s\n", result.Message)
			failedStep = result.StepName
			break
		}
	}

	// Compensate completed steps in reverse order.
	if failedStep != "" {
		fmt.Printf("\nCompensating %d completed step(s) due to %s failure...\n\n", completedSteps, failedStep)
		for i := completedSteps - 1; i >= 0; i-- {
			steps[i].cmdCh <- StepCommand{Action: "compensate", OrderID: orderID}
			result := <-steps[i].resCh
			fmt.Printf("[COMPENSATE] %s\n", result.Message)
		}
	}

	for _, step := range steps {
		close(step.cmdCh)
	}

	if failedStep != "" {
		fmt.Printf("\nSaga rolled back due to %s failure\n", failedStep)
	} else {
		fmt.Printf("\nSaga completed successfully\n")
	}
}
```

When payment fails at step 2, only step 1 (inventory) was completed. The coordinator compensates step 1 by sending "compensate" on its command channel. Steps 3 and 4 were never executed, so they need no compensation.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Saga: Order ORD-1002 ===

[FORWARD] reserve-inventory completed for order ORD-1002
[FAILED]  charge-payment FAILED for order ORD-1002

Compensating 1 completed step(s) due to charge-payment failure...

[COMPENSATE] reserve-inventory compensated for order ORD-1002

Saga rolled back due to charge-payment failure
```

## Step 3 -- Shipment Failure: Compensate Two Steps

Move the failure to step 3 (shipment). Now both payment and inventory must be compensated, in reverse order: payment first, then inventory.

```go
package main

import (
	"fmt"
	"time"
)

const multiCompDelay = 30 * time.Millisecond

type StepResult struct {
	StepName string
	Success  bool
	Message  string
}

type StepCommand struct {
	Action  string
	OrderID string
}

func sagaStep(name string, failOnExecute bool, input <-chan StepCommand, output chan<- StepResult) {
	for cmd := range input {
		time.Sleep(multiCompDelay)
		switch cmd.Action {
		case "execute":
			if failOnExecute {
				output <- StepResult{
					StepName: name,
					Success:  false,
					Message:  fmt.Sprintf("%s FAILED for order %s", name, cmd.OrderID),
				}
			} else {
				output <- StepResult{
					StepName: name,
					Success:  true,
					Message:  fmt.Sprintf("%s completed for order %s", name, cmd.OrderID),
				}
			}
		case "compensate":
			output <- StepResult{
				StepName: name,
				Success:  true,
				Message:  fmt.Sprintf("%s compensated for order %s", name, cmd.OrderID),
			}
		}
	}
}

type stepChannels struct {
	name  string
	cmdCh chan StepCommand
	resCh chan StepResult
}

// runSaga executes the saga and returns whether it succeeded.
func runSaga(orderID string, steps []stepChannels) bool {
	completedSteps := 0
	var failedStep string

	// Forward execution.
	for _, step := range steps {
		step.cmdCh <- StepCommand{Action: "execute", OrderID: orderID}
		result := <-step.resCh
		if result.Success {
			fmt.Printf("  [FORWARD]    %s\n", result.Message)
			completedSteps++
		} else {
			fmt.Printf("  [FAILED]     %s\n", result.Message)
			failedStep = result.StepName
			break
		}
	}

	if failedStep == "" {
		return true
	}

	// Reverse compensation.
	fmt.Printf("\n  Compensating %d step(s) due to %s failure:\n", completedSteps, failedStep)
	for i := completedSteps - 1; i >= 0; i-- {
		steps[i].cmdCh <- StepCommand{Action: "compensate", OrderID: orderID}
		result := <-steps[i].resCh
		fmt.Printf("  [COMPENSATE] %s\n", result.Message)
	}

	return false
}

func main() {
	// Shipment (index 2) will fail.
	stepDefs := []struct {
		name string
		fail bool
	}{
		{"reserve-inventory", false},
		{"charge-payment", false},
		{"create-shipment", true},
		{"send-confirmation", false},
	}

	steps := make([]stepChannels, len(stepDefs))
	for i, def := range stepDefs {
		cmdCh := make(chan StepCommand, 1)
		resCh := make(chan StepResult, 1)
		steps[i] = stepChannels{name: def.name, cmdCh: cmdCh, resCh: resCh}
		go sagaStep(def.name, def.fail, cmdCh, resCh)
	}

	fmt.Println("=== Saga: Order ORD-1003 ===")
	fmt.Println()
	success := runSaga("ORD-1003", steps)

	for _, step := range steps {
		close(step.cmdCh)
	}

	fmt.Println()
	if success {
		fmt.Println("Result: saga completed successfully")
	} else {
		fmt.Println("Result: saga rolled back")
	}
}
```

The compensation order is critical: charge-payment is compensated before reserve-inventory. This mirrors real business logic -- you must refund the payment before releasing the reserved stock, because a refund may depend on inventory state.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Saga: Order ORD-1003 ===

  [FORWARD]    reserve-inventory completed for order ORD-1003
  [FORWARD]    charge-payment completed for order ORD-1003
  [FAILED]     create-shipment FAILED for order ORD-1003

  Compensating 2 step(s) due to create-shipment failure:
  [COMPENSATE] charge-payment compensated for order ORD-1003
  [COMPENSATE] reserve-inventory compensated for order ORD-1003

Result: saga rolled back
```

## Step 4 -- Full Timeline Log with Timestamps

Add structured timeline logging so every action (forward and compensation) is recorded with relative timestamps. Run both a successful and a failed saga to compare timelines.

```go
package main

import (
	"fmt"
	"time"
)

const timelineStepDelay = 30 * time.Millisecond

type StepResult struct {
	StepName string
	Success  bool
	Message  string
}

type StepCommand struct {
	Action  string
	OrderID string
}

// TimelineEntry records a single event in the saga timeline.
type TimelineEntry struct {
	Elapsed time.Duration
	Phase   string
	Step    string
	Status  string
	Message string
}

func sagaStep(name string, failOnExecute bool, input <-chan StepCommand, output chan<- StepResult) {
	for cmd := range input {
		time.Sleep(timelineStepDelay)
		switch cmd.Action {
		case "execute":
			if failOnExecute {
				output <- StepResult{StepName: name, Success: false,
					Message: fmt.Sprintf("%s FAILED", name)}
			} else {
				output <- StepResult{StepName: name, Success: true,
					Message: fmt.Sprintf("%s OK", name)}
			}
		case "compensate":
			output <- StepResult{StepName: name, Success: true,
				Message: fmt.Sprintf("%s undone", name)}
		}
	}
}

type stepChannels struct {
	name  string
	cmdCh chan StepCommand
	resCh chan StepResult
}

func runSagaWithTimeline(orderID string, steps []stepChannels, epoch time.Time) (bool, []TimelineEntry) {
	var timeline []TimelineEntry
	completedSteps := 0
	var failedStep string

	for _, step := range steps {
		step.cmdCh <- StepCommand{Action: "execute", OrderID: orderID}
		result := <-step.resCh
		status := "OK"
		if !result.Success {
			status = "FAILED"
			failedStep = result.StepName
		} else {
			completedSteps++
		}
		timeline = append(timeline, TimelineEntry{
			Elapsed: time.Since(epoch).Round(time.Millisecond),
			Phase:   "FORWARD",
			Step:    result.StepName,
			Status:  status,
			Message: result.Message,
		})
		if failedStep != "" {
			break
		}
	}

	if failedStep != "" {
		for i := completedSteps - 1; i >= 0; i-- {
			steps[i].cmdCh <- StepCommand{Action: "compensate", OrderID: orderID}
			result := <-steps[i].resCh
			timeline = append(timeline, TimelineEntry{
				Elapsed: time.Since(epoch).Round(time.Millisecond),
				Phase:   "COMPENSATE",
				Step:    result.StepName,
				Status:  "UNDONE",
				Message: result.Message,
			})
		}
		return false, timeline
	}

	return true, timeline
}

func printTimeline(orderID string, success bool, timeline []TimelineEntry) {
	fmt.Printf("%-10s %-12s %-22s %-8s %s\n",
		"ELAPSED", "PHASE", "STEP", "STATUS", "MESSAGE")
	fmt.Println("--------------------------------------------------------------------")
	for _, e := range timeline {
		fmt.Printf("%-10v %-12s %-22s %-8s %s\n",
			e.Elapsed, e.Phase, e.Step, e.Status, e.Message)
	}
	result := "COMMITTED"
	if !success {
		result = "ROLLED BACK"
	}
	fmt.Printf("\nOrder %s: %s (%d events)\n", orderID, result, len(timeline))
}

func buildSteps(defs []struct{ name string; fail bool }) []stepChannels {
	steps := make([]stepChannels, len(defs))
	for i, def := range defs {
		cmdCh := make(chan StepCommand, 1)
		resCh := make(chan StepResult, 1)
		steps[i] = stepChannels{name: def.name, cmdCh: cmdCh, resCh: resCh}
		go sagaStep(def.name, def.fail, cmdCh, resCh)
	}
	return steps
}

func closeSteps(steps []stepChannels) {
	for _, step := range steps {
		close(step.cmdCh)
	}
}

func main() {
	// Scenario 1: happy path.
	fmt.Println("=== Scenario 1: Happy Path (ORD-2001) ===")
	fmt.Println()
	happyDefs := []struct{ name string; fail bool }{
		{"reserve-inventory", false},
		{"charge-payment", false},
		{"create-shipment", false},
		{"send-confirmation", false},
	}
	happySteps := buildSteps(happyDefs)
	epoch1 := time.Now()
	success1, timeline1 := runSagaWithTimeline("ORD-2001", happySteps, epoch1)
	printTimeline("ORD-2001", success1, timeline1)
	closeSteps(happySteps)

	fmt.Println()

	// Scenario 2: shipment fails.
	fmt.Println("=== Scenario 2: Shipment Failure (ORD-2002) ===")
	fmt.Println()
	failDefs := []struct{ name string; fail bool }{
		{"reserve-inventory", false},
		{"charge-payment", false},
		{"create-shipment", true},
		{"send-confirmation", false},
	}
	failSteps := buildSteps(failDefs)
	epoch2 := time.Now()
	success2, timeline2 := runSagaWithTimeline("ORD-2002", failSteps, epoch2)
	printTimeline("ORD-2002", success2, timeline2)
	closeSteps(failSteps)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Scenario 1: Happy Path (ORD-2001) ===

ELAPSED    PHASE        STEP                   STATUS   MESSAGE
--------------------------------------------------------------------
30ms       FORWARD      reserve-inventory      OK       reserve-inventory OK
60ms       FORWARD      charge-payment         OK       charge-payment OK
90ms       FORWARD      create-shipment        OK       create-shipment OK
120ms      FORWARD      send-confirmation      OK       send-confirmation OK

Order ORD-2001: COMMITTED (4 events)

=== Scenario 2: Shipment Failure (ORD-2002) ===

ELAPSED    PHASE        STEP                   STATUS   MESSAGE
--------------------------------------------------------------------
30ms       FORWARD      reserve-inventory      OK       reserve-inventory OK
60ms       FORWARD      charge-payment         OK       charge-payment OK
90ms       FORWARD      create-shipment        FAILED   create-shipment FAILED
120ms      COMPENSATE   charge-payment         UNDONE   charge-payment undone
150ms      COMPENSATE   reserve-inventory      UNDONE   reserve-inventory undone

Order ORD-2002: ROLLED BACK (5 events)
```

## Common Mistakes

### Compensating the Failed Step
**What happens:** The coordinator sends a "compensate" command to the step that failed its forward execution. That step never completed its action, so its compensation is meaningless at best and harmful at worst (e.g., refunding a charge that was never made).
**Fix:** Only compensate steps that completed successfully. The loop iterates from `completedSteps - 1` down to 0, excluding the failed step.

### Compensation in Forward Order
**What happens:** The coordinator compensates step 1 before step 2. In a checkout, this means releasing inventory before refunding the payment. If another customer grabs the released inventory, the refund may need different logic.
**Fix:** Always compensate in reverse order. The most recently completed step is undone first: `for i := completedSteps - 1; i >= 0; i--`.

### Not Handling Compensation Failure
**What happens:** A compensation step fails (e.g., the payment gateway is down during refund). The coordinator ignores the failure and continues compensating. The customer is charged but never refunded.
**Fix:** In production, log compensation failures, retry them (using the retry pattern from exercise 24), and alert operations. A failed compensation is a critical incident that requires human intervention.

## Review

The saga coordinates a workflow that cannot be a single transaction by pairing every step with a compensating action and driving them through a uniform topology: each step is a goroutine with a command channel and a result channel, and it handles both `execute` and `compensate` on that same pair, so the undo logic lives next to the do logic and stays testable. The coordinator runs forward one step at a time, stopping at the first failure, then walks the completed steps backward -- from `completedSteps - 1` down to 0 -- issuing compensations. Two invariants make it correct. The failed step is never compensated, because it never completed its action and undoing something that never happened is at best meaningless and at worst a phantom refund. And compensation must run in reverse of execution, because business dependencies are ordered: you refund the charge before you release the reserved stock, not the other way around. A structured timeline log turns the whole run -- forward and back -- into an audit trail you can actually debug.

To push past the happy cases, make compensation itself fallible: give `sagaStep` a `compensationFailRate` so some undo attempts fail, then wrap each compensation in a retry that backs off exponentially and, after three failures, logs a critical alert instead of silently dropping it. That last detail is the real production lesson -- a failed compensation means a customer was charged and not refunded, which is an incident requiring a human, not a line swallowed by the coordinator.

## Resources

- [Saga Pattern (Chris Richardson)](https://microservices.io/patterns/data/saga.html) -- the canonical description of choreographed versus orchestrated sagas and compensating actions.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the channel-and-goroutine building blocks the coordinator is assembled from.
- [Compensating Transaction Pattern (Microsoft)](https://learn.microsoft.com/en-us/azure/architecture/patterns/compensating-transaction) -- why reverse-order undo preserves consistency when a distributed operation fails partway.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the philosophy behind driving each step over its own command/result channels.

---

Back to [Concurrency](../../concurrency.md) | Next: [27-channel-priority-queue](../27-channel-priority-queue/27-channel-priority-queue.md)
