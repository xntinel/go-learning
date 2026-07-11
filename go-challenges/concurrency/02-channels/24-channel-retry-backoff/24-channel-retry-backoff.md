# Exercise 24: Channel-Based Retry with Exponential Backoff

Every production service calls external APIs that fail transiently -- a database
returns "too many connections," a cloud provider throws 503 for two seconds
during a deploy, a payment gateway times out under load. Wrapping every call
site in its own retry loop scatters that policy across the codebase, tangles it
with business logic, and makes it impossible to change consistently. The
channel-based approach lifts retry out into a standalone pipeline stage: failed
operations flow in, a retry goroutine owns the backoff timer and the attempt
counter, successes flow out, and permanent failures land in a dead-letter
channel. Business logic never sees a retry.

## What you'll build

```text
24-channel-retry-backoff/
  main.go        fixed-delay retry, exponential backoff with jitter, a
                 dead-letter channel, and multiple concurrent retry workers
```

- Build: a channel-based retry stage with exponential backoff, jitter, and dead-lettering.
- Implement: a `retryLoop`/`retryWorker` reading failed operations, `backoffDelay` built on `time.NewTimer`, and routing exhausted operations to a `DeadLetter` channel across multiple concurrent workers.
- Verify: `go run main.go`, and `go run -race main.go` for the multi-worker stage.

### Why retry belongs in its own pipeline stage

The channel-based approach extracts retry into a standalone pipeline stage. Failed operations flow into a retry channel. A retry goroutine owns the backoff timer, the attempt counter, and the max-retry policy. Successful retries flow out to a results channel. Permanently failed operations flow to a dead-letter channel for alerting or manual review. Business logic never sees retry -- it sends an operation and receives either a result or a dead-letter notification.

This separation makes retry policy testable in isolation, swappable at runtime, and consistent across every call site. The same pattern works for HTTP retries, message queue redelivery, and distributed task execution.

## Step 1 -- Single Retry with Fixed Delay

Start with the simplest case: one operation fails, gets retried once after a fixed delay, and succeeds.

```go
package main

import (
	"fmt"
	"time"
)

const fixedDelay = 200 * time.Millisecond

// Operation represents a unit of work that may need retrying.
type Operation struct {
	ID      int
	Name    string
	Attempt int
}

// Result carries the outcome of an operation attempt.
type Result struct {
	OpID    int
	Success bool
	Message string
	Attempt int
}

// simulateAPI fails on first attempt, succeeds on second.
func simulateAPI(op Operation) Result {
	if op.Attempt < 2 {
		return Result{
			OpID:    op.ID,
			Success: false,
			Message: fmt.Sprintf("503 Service Unavailable (attempt %d)", op.Attempt),
			Attempt: op.Attempt,
		}
	}
	return Result{
		OpID:    op.ID,
		Success: true,
		Message: fmt.Sprintf("200 OK: %s completed (attempt %d)", op.Name, op.Attempt),
		Attempt: op.Attempt,
	}
}

// retryWorker reads from failed, waits, increments attempt, resends to output.
func retryWorker(failed <-chan Operation, output chan<- Operation, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for op := range failed {
		timer := time.NewTimer(fixedDelay)
		<-timer.C
		op.Attempt++
		fmt.Printf("  [retry] %s: retrying (attempt %d)\n", op.Name, op.Attempt)
		output <- op
	}
}

func main() {
	failed := make(chan Operation, 10)
	output := make(chan Operation, 10)
	done := make(chan struct{}, 1)

	go retryWorker(failed, output, done)

	op := Operation{ID: 1, Name: "create-vm", Attempt: 1}

	// First attempt.
	result := simulateAPI(op)
	fmt.Printf("attempt %d: %s\n", result.Attempt, result.Message)

	// Send to retry.
	failed <- op
	close(failed)

	// Receive retried operation.
	retriedOp := <-output
	result = simulateAPI(retriedOp)
	fmt.Printf("attempt %d: %s\n", result.Attempt, result.Message)

	<-done
}
```

Key observations:
- The retry worker uses `time.NewTimer` instead of `time.Sleep` -- timers can be stopped and are select-compatible
- The worker increments `Attempt` before resending, so the API call knows which attempt this is
- The `done` channel signals that the retry worker has finished processing

### Verification
```bash
go run main.go
```
Expected output:
```
attempt 1: 503 Service Unavailable (attempt 1)
  [retry] create-vm: retrying (attempt 2)
attempt 2: 200 OK: create-vm completed (attempt 2)
```

## Step 2 -- Exponential Backoff with Jitter

Replace fixed delay with exponential backoff: each retry waits `baseDelay * 2^attempt`, plus random jitter to prevent thundering herd.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	baseDelay  = 100 * time.Millisecond
	maxDelay   = 2 * time.Second
	maxRetries = 5
)

type Operation struct {
	ID      int
	Name    string
	Attempt int
}

type Result struct {
	OpID    int
	Success bool
	Message string
	Attempt int
}

// simulateUnstableAPI fails for the first 3 attempts.
func simulateUnstableAPI(op Operation) Result {
	if op.Attempt <= 3 {
		return Result{
			OpID:    op.ID,
			Success: false,
			Message: fmt.Sprintf("503 error (attempt %d)", op.Attempt),
			Attempt: op.Attempt,
		}
	}
	return Result{
		OpID:    op.ID,
		Success: true,
		Message: fmt.Sprintf("200 OK: %s done (attempt %d)", op.Name, op.Attempt),
		Attempt: op.Attempt,
	}
}

// backoffDelay calculates exponential delay with jitter, capped at maxDelay.
func backoffDelay(attempt int) time.Duration {
	delay := baseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
			break
		}
	}
	// Add jitter: 0% to 50% of the delay.
	jitter := time.Duration(rand.Int64N(int64(delay / 2)))
	return delay + jitter
}

// retryLoop reads failed ops, applies backoff, resends or gives up.
func retryLoop(failed <-chan Operation, output chan<- Result) {
	for op := range failed {
		for op.Attempt <= maxRetries {
			result := simulateUnstableAPI(op)
			if result.Success {
				output <- result
				break
			}
			delay := backoffDelay(op.Attempt)
			fmt.Printf("  [retry] %s attempt %d failed, backoff %v\n",
				op.Name, op.Attempt, delay.Round(time.Millisecond))
			timer := time.NewTimer(delay)
			<-timer.C
			op.Attempt++
		}
		if op.Attempt > maxRetries {
			output <- Result{
				OpID:    op.ID,
				Success: false,
				Message: fmt.Sprintf("%s: exhausted %d retries", op.Name, maxRetries),
				Attempt: op.Attempt,
			}
		}
	}
	close(output)
}

func main() {
	failed := make(chan Operation, 10)
	output := make(chan Result, 10)

	go retryLoop(failed, output)

	epoch := time.Now()
	failed <- Operation{ID: 1, Name: "create-vm", Attempt: 1}
	close(failed)

	for result := range output {
		elapsed := time.Since(epoch).Round(time.Millisecond)
		status := "SUCCESS"
		if !result.Success {
			status = "FAILED"
		}
		fmt.Printf("[%v] %s: %s\n", elapsed, status, result.Message)
	}
}
```

The backoff doubles each attempt: ~100ms, ~200ms, ~400ms. Jitter adds 0-50% randomness so that multiple clients retrying the same endpoint do not all hit it at exactly the same moment.

### Verification
```bash
go run main.go
```
Expected output (delays vary due to jitter):
```
  [retry] create-vm attempt 1 failed, backoff 120ms
  [retry] create-vm attempt 2 failed, backoff 250ms
  [retry] create-vm attempt 3 failed, backoff 480ms
[850ms] SUCCESS: 200 OK: create-vm done (attempt 4)
```

## Step 3 -- Dead-Letter Channel for Permanent Failures

Add a dead-letter channel: operations that exhaust all retries are routed there instead of being silently dropped.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	dlBaseDelay  = 50 * time.Millisecond
	dlMaxDelay   = 500 * time.Millisecond
	dlMaxRetries = 3
)

type Operation struct {
	ID      int
	Name    string
	Attempt int
}

type Result struct {
	OpID    int
	Success bool
	Message string
	Attempt int
}

// DeadLetter holds a permanently failed operation with context.
type DeadLetter struct {
	Op       Operation
	Reason   string
	FailedAt time.Time
}

// simulateFlaky fails based on operation ID: even IDs always fail, odd succeed on attempt 2.
func simulateFlaky(op Operation) Result {
	alwaysFails := op.ID%2 == 0
	if alwaysFails || op.Attempt < 2 {
		return Result{
			OpID:    op.ID,
			Success: false,
			Message: fmt.Sprintf("503 error (attempt %d)", op.Attempt),
			Attempt: op.Attempt,
		}
	}
	return Result{
		OpID:    op.ID,
		Success: true,
		Message: fmt.Sprintf("%s completed (attempt %d)", op.Name, op.Attempt),
		Attempt: op.Attempt,
	}
}

func backoffWithJitter(attempt int) time.Duration {
	delay := dlBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > dlMaxDelay {
			delay = dlMaxDelay
			break
		}
	}
	jitter := time.Duration(rand.Int64N(int64(delay/4) + 1))
	return delay + jitter
}

// retryWithDeadLetter retries operations and routes permanent failures to deadLetters.
func retryWithDeadLetter(
	input <-chan Operation,
	results chan<- Result,
	deadLetters chan<- DeadLetter,
) {
	for op := range input {
		var lastResult Result
		succeeded := false
		for op.Attempt <= dlMaxRetries {
			lastResult = simulateFlaky(op)
			if lastResult.Success {
				results <- lastResult
				succeeded = true
				break
			}
			if op.Attempt == dlMaxRetries {
				break
			}
			delay := backoffWithJitter(op.Attempt)
			fmt.Printf("  [retry] op %d attempt %d failed, backoff %v\n",
				op.ID, op.Attempt, delay.Round(time.Millisecond))
			timer := time.NewTimer(delay)
			<-timer.C
			op.Attempt++
		}
		if !succeeded {
			deadLetters <- DeadLetter{
				Op:       op,
				Reason:   lastResult.Message,
				FailedAt: time.Now(),
			}
		}
	}
	close(results)
	close(deadLetters)
}

func main() {
	input := make(chan Operation, 10)
	results := make(chan Result, 10)
	deadLetters := make(chan DeadLetter, 10)

	go retryWithDeadLetter(input, results, deadLetters)

	operations := []Operation{
		{ID: 1, Name: "create-vm", Attempt: 1},
		{ID: 2, Name: "attach-disk", Attempt: 1},
		{ID: 3, Name: "configure-network", Attempt: 1},
		{ID: 4, Name: "install-agent", Attempt: 1},
	}

	for _, op := range operations {
		input <- op
	}
	close(input)

	// Drain both output channels.
	resultsDone := false
	deadDone := false
	successCount, deadCount := 0, 0

	for !resultsDone || !deadDone {
		select {
		case r, ok := <-results:
			if !ok {
				resultsDone = true
				continue
			}
			fmt.Printf("SUCCESS: op %d - %s\n", r.OpID, r.Message)
			successCount++
		case dl, ok := <-deadLetters:
			if !ok {
				deadDone = true
				continue
			}
			fmt.Printf("DEAD-LETTER: op %d (%s) - %s\n",
				dl.Op.ID, dl.Op.Name, dl.Reason)
			deadCount++
		}
	}

	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("Succeeded:    %d\n", successCount)
	fmt.Printf("Dead-lettered: %d\n", deadCount)
}
```

The dead-letter channel is a critical production pattern: it prevents permanent failures from being silently lost. A monitoring system can read from the dead-letter channel to trigger alerts or queue manual intervention.

### Verification
```bash
go run main.go
```
Expected output (order may vary):
```
  [retry] op 1 attempt 1 failed, backoff 55ms
SUCCESS: op 1 - create-vm completed (attempt 2)
  [retry] op 2 attempt 1 failed, backoff 52ms
  [retry] op 2 attempt 2 failed, backoff 108ms
DEAD-LETTER: op 2 (attach-disk) - 503 error (attempt 3)
  [retry] op 3 attempt 1 failed, backoff 60ms
SUCCESS: op 3 - configure-network completed (attempt 2)
  [retry] op 4 attempt 1 failed, backoff 53ms
  [retry] op 4 attempt 2 failed, backoff 105ms
DEAD-LETTER: op 4 (install-agent) - 503 error (attempt 3)

=== Summary ===
Succeeded:    2
Dead-lettered: 2
```

## Step 4 -- Multiple Concurrent Retry Loops

Run 3 retry workers in parallel. Operations are distributed across workers, increasing throughput while each worker independently manages its own backoff timers.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	concBaseDelay  = 50 * time.Millisecond
	concMaxDelay   = 400 * time.Millisecond
	concMaxRetries = 3
	workerCount    = 3
	operationCount = 9
)

type Operation struct {
	ID      int
	Name    string
	Attempt int
}

type Result struct {
	OpID     int
	Success  bool
	Message  string
	Attempt  int
	WorkerID int
}

type DeadLetter struct {
	Op       Operation
	Reason   string
	WorkerID int
}

// simulateMixedAPI: ops 1-3 succeed on attempt 1, 4-6 on attempt 2, 7-9 always fail.
func simulateMixedAPI(op Operation) bool {
	switch {
	case op.ID <= 3:
		return true
	case op.ID <= 6:
		return op.Attempt >= 2
	default:
		return false
	}
}

func concBackoff(attempt int) time.Duration {
	delay := concBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > concMaxDelay {
			delay = concMaxDelay
			break
		}
	}
	jitter := time.Duration(rand.Int64N(int64(delay/4) + 1))
	return delay + jitter
}

// retryWorker processes operations with retry logic.
func retryWorker(
	id int,
	input <-chan Operation,
	results chan<- Result,
	deadLetters chan<- DeadLetter,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	for op := range input {
		succeeded := false
		for op.Attempt <= concMaxRetries {
			if simulateMixedAPI(op) {
				results <- Result{
					OpID:     op.ID,
					Success:  true,
					Message:  fmt.Sprintf("%s completed", op.Name),
					Attempt:  op.Attempt,
					WorkerID: id,
				}
				succeeded = true
				break
			}
			if op.Attempt == concMaxRetries {
				break
			}
			delay := concBackoff(op.Attempt)
			fmt.Printf("  [worker %d] op %d attempt %d failed, backoff %v\n",
				id, op.ID, op.Attempt, delay.Round(time.Millisecond))
			timer := time.NewTimer(delay)
			<-timer.C
			op.Attempt++
		}
		if !succeeded {
			deadLetters <- DeadLetter{
				Op:       op,
				Reason:   fmt.Sprintf("exhausted %d retries", concMaxRetries),
				WorkerID: id,
			}
		}
	}
}

func main() {
	input := make(chan Operation, operationCount)
	results := make(chan Result, operationCount)
	deadLetters := make(chan DeadLetter, operationCount)

	var wg sync.WaitGroup
	for i := 1; i <= workerCount; i++ {
		wg.Add(1)
		go retryWorker(i, input, results, deadLetters, &wg)
	}

	// Submit operations.
	epoch := time.Now()
	for i := 1; i <= operationCount; i++ {
		input <- Operation{
			ID:      i,
			Name:    fmt.Sprintf("provision-step-%d", i),
			Attempt: 1,
		}
	}
	close(input)

	// Wait for all workers, then close output channels.
	go func() {
		wg.Wait()
		close(results)
		close(deadLetters)
	}()

	// Drain results.
	successCount, deadCount := 0, 0
	resultsDone, deadDone := false, false

	for !resultsDone || !deadDone {
		select {
		case r, ok := <-results:
			if !ok {
				resultsDone = true
				continue
			}
			fmt.Printf("SUCCESS [worker %d]: op %d %s (attempt %d)\n",
				r.WorkerID, r.OpID, r.Message, r.Attempt)
			successCount++
		case dl, ok := <-deadLetters:
			if !ok {
				deadDone = true
				continue
			}
			fmt.Printf("DEAD [worker %d]: op %d (%s) - %s\n",
				dl.WorkerID, dl.Op.ID, dl.Op.Name, dl.Reason)
			deadCount++
		}
	}

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("Workers:       %d\n", workerCount)
	fmt.Printf("Operations:    %d\n", operationCount)
	fmt.Printf("Succeeded:     %d\n", successCount)
	fmt.Printf("Dead-lettered: %d\n", deadCount)
	fmt.Printf("Wall time:     %v\n", elapsed)
}
```

Three workers process operations concurrently. Ops 1-3 succeed immediately, ops 4-6 need one retry, ops 7-9 exhaust retries and go to dead-letter. Wall time is much less than sequential because workers retry in parallel.

### Verification
```bash
go run -race main.go
```
Expected output (order varies):
```
SUCCESS [worker 1]: op 1 provision-step-1 completed (attempt 1)
SUCCESS [worker 2]: op 2 provision-step-2 completed (attempt 1)
SUCCESS [worker 3]: op 3 provision-step-3 completed (attempt 1)
  [worker 1] op 4 attempt 1 failed, backoff 55ms
  [worker 2] op 5 attempt 1 failed, backoff 58ms
  ...
SUCCESS [worker 1]: op 4 provision-step-4 completed (attempt 2)
DEAD [worker 3]: op 9 (provision-step-9) - exhausted 3 retries
...

=== Summary ===
Workers:       3
Operations:    9
Succeeded:     6
Dead-lettered: 3
Wall time:     Xms
```

## Common Mistakes

### Using time.Sleep Instead of time.Timer
**What happens:** `time.Sleep` blocks the goroutine unconditionally. You cannot cancel the sleep with a context or select statement. In production, a shutdown signal arrives but the goroutine sleeps for 30 seconds before noticing.
**Fix:** Use `time.NewTimer(delay)` and wait on `timer.C` inside a `select`. This lets you also listen for a cancellation channel: `select { case <-timer.C: ... case <-ctx.Done(): ... }`.

### No Jitter on Backoff
**What happens:** 100 clients all fail at the same time and retry at exactly the same intervals (100ms, 200ms, 400ms). The server gets hit by the same thundering herd at each retry wave, making the outage worse.
**Fix:** Add random jitter (typically 0-50% of the delay) so clients spread their retries over time. This is why `backoffDelay` adds a random component.

### Closing Output Channels from the Wrong Goroutine
**What happens:** With multiple workers sharing the same output channels, one worker finishes and closes the channel while another is still sending. Sending to a closed channel panics.
**Fix:** Use a `sync.WaitGroup` to wait for all workers, then close the output channels from a separate goroutine after `wg.Wait()` returns.

## Review

The pattern makes retry a stage rather than a habit. Failed operations arrive on
an input channel; the retry goroutine owns the attempt counter, the backoff
timer, and the max-retry policy; successes leave on a results channel and
operations that exhaust their retries leave on a dead-letter channel instead of
vanishing. Two mechanics matter. Backoff is exponential -- roughly
`base * 2^attempt`, capped -- with random jitter added on top so that a hundred
clients failing at the same instant do not synchronize into a thundering herd on
each retry wave. And the wait uses `time.NewTimer` on `timer.C` rather than
`time.Sleep`, because a timer channel can sit inside a `select` alongside a
cancellation channel while `Sleep` blocks unconditionally for its full duration.

Scaling the stage to several workers changes one rule: no individual worker may
close the shared output channels, because a second worker might still be sending
and a send on a closed channel panics -- a coordinator goroutine calls
`wg.Wait()` and closes them once. The extension worth building from here is
cancellation: thread a `context.Context` into each worker so that when it is
cancelled, workers stop waiting on the backoff timer immediately --
`select { case <-timer.C: case <-ctx.Done(): }` -- and drain whatever is left
straight to the dead-letter channel. If cancelling mid-backoff stops the retries
within one timer tick, the stage is doing its job.

## Resources
- [Exponential Backoff and Jitter (AWS Architecture Blog)](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) -- why jitter, and which jitter formula actually spreads a retry storm.
- [Go time package: Timer](https://pkg.go.dev/time#Timer) -- NewTimer, timer.C, and Stop, the select-friendly alternative to Sleep.
- [Go Concurrency Patterns: Pipelines](https://go.dev/blog/pipelines) -- composing channel stages and the rule for who closes each channel.
- [Go Blog: Context](https://go.dev/blog/context) -- the cancellation plumbing the final extension threads through each worker.

---

Back to [Concurrency](../../concurrency.md) | Next: [25-ordered-fan-out-results](../25-ordered-fan-out-results/25-ordered-fan-out-results.md)
