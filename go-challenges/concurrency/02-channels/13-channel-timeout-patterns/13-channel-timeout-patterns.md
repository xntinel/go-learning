# Exercise 13: Channel Timeout Patterns

An API aggregator that fans out to five external services without timeouts is an outage waiting to happen: let one dependency hang and its goroutine blocks forever, the connection pool fills up, and the whole service stops responding. This is the number one cause of cascading failures in microservice architectures. This exercise works through Go's timeout toolbox -- `time.After`, `time.NewTimer`, and `context.WithTimeout` -- each of which prevents an unbounded wait, but with different trade-offs in safety, resource usage, and composability.

## What you'll build

```text
channel-timeout-patterns/
  main.go        per-operation timeouts, an overall aggregation deadline, the
                 time.After timer leak and its time.NewTimer fix, and the
                 production context.WithTimeout pattern
```

- Build: an API aggregator that guards individual calls and the whole request against unbounded waits, then hardens it with contexts.
- Implement: `fetchWithTimeout` racing a call against `time.After`; an `APIAggregator` with an overall deadline; `demonstrateTimerLeak` / `demonstrateTimerFix` contrasting `time.After` and `time.NewTimer`; and an `AggregatorService` composing per-call and overall `context.WithTimeout`.
- Verify: `go run main.go` at each step (some calls succeed and some time out by design).

### Why an unbounded wait is a cascading failure

Go provides multiple timeout mechanisms through channels: `time.After` for simple one-shot timeouts, `time.NewTimer` for reusable timers without leaks, and `context.WithTimeout` for propagating deadlines across goroutine boundaries. Each solves the same fundamental problem -- preventing unbounded waits -- but with different trade-offs in safety, resource usage, and composability.

## Step 1 -- Per-Operation Timeout with time.After

Call multiple external services concurrently, each with its own timeout. `time.After` returns a channel that fires once after the specified duration. Use it in a `select` to race the operation against the clock.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

type ServiceResponse struct {
	Service  string
	Data     string
	Duration time.Duration
}

func callService(name string, minLatency, maxLatency time.Duration) <-chan ServiceResponse {
	result := make(chan ServiceResponse, 1)
	go func() {
		latency := minLatency + time.Duration(rand.Int64N(int64(maxLatency-minLatency)))
		time.Sleep(latency)
		result <- ServiceResponse{
			Service:  name,
			Data:     fmt.Sprintf("data from %s", name),
			Duration: latency,
		}
	}()
	return result
}

func fetchWithTimeout(name string, timeout time.Duration, minLatency, maxLatency time.Duration) (ServiceResponse, error) {
	result := callService(name, minLatency, maxLatency)
	select {
	case resp := <-result:
		return resp, nil
	case <-time.After(timeout):
		return ServiceResponse{}, fmt.Errorf("service %s timed out after %v", name, timeout)
	}
}

func main() {
	services := []struct {
		name       string
		timeout    time.Duration
		minLatency time.Duration
		maxLatency time.Duration
	}{
		{"user-service", 100 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond},
		{"billing-service", 100 * time.Millisecond, 20 * time.Millisecond, 80 * time.Millisecond},
		{"inventory-service", 100 * time.Millisecond, 10 * time.Millisecond, 60 * time.Millisecond},
		{"notification-service", 50 * time.Millisecond, 30 * time.Millisecond, 100 * time.Millisecond},
		{"analytics-service", 80 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond},
	}

	fmt.Println("=== Per-Operation Timeouts ===")
	for _, svc := range services {
		resp, err := fetchWithTimeout(svc.name, svc.timeout, svc.minLatency, svc.maxLatency)
		if err != nil {
			fmt.Printf("  TIMEOUT: %v\n", err)
			continue
		}
		fmt.Printf("  OK: %s responded in %v\n", resp.Service, resp.Duration)
	}
}
```

Each service call races against its own `time.After`. Fast services succeed; slow ones get cut off. This protects the caller from any single slow dependency.

### Verification
```bash
go run main.go
# Expected: some services respond OK, some may timeout
# notification-service and analytics-service are most likely to timeout
```

## Step 2 -- Overall Deadline for All Operations

Per-operation timeouts protect against individual slow services, but your API endpoint has its own SLA. An overall deadline ensures the entire aggregation completes within a fixed time, regardless of how many individual calls are pending.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

type ServiceResult struct {
	Name     string
	Data     string
	Duration time.Duration
	Err      error
}

func callServiceAsync(name string, latency time.Duration) <-chan ServiceResult {
	ch := make(chan ServiceResult, 1)
	go func() {
		time.Sleep(latency)
		ch <- ServiceResult{
			Name:     name,
			Data:     fmt.Sprintf("response from %s", name),
			Duration: latency,
		}
	}()
	return ch
}

type APIAggregator struct {
	overallDeadline time.Duration
}

func NewAPIAggregator(deadline time.Duration) *APIAggregator {
	return &APIAggregator{overallDeadline: deadline}
}

func (a *APIAggregator) FetchAll(serviceLatencies map[string]time.Duration) []ServiceResult {
	type indexedResult struct {
		index int
		result ServiceResult
	}

	channels := make([]<-chan ServiceResult, 0, len(serviceLatencies))
	names := make([]string, 0, len(serviceLatencies))

	for name, latency := range serviceLatencies {
		channels = append(channels, callServiceAsync(name, latency))
		names = append(names, name)
	}

	deadline := time.After(a.overallDeadline)
	var results []ServiceResult

	for i, ch := range channels {
		select {
		case result := <-ch:
			results = append(results, result)
		case <-deadline:
			for j := i; j < len(channels); j++ {
				results = append(results, ServiceResult{
					Name: names[j],
					Err:  fmt.Errorf("skipped: overall deadline of %v exceeded", a.overallDeadline),
				})
			}
			return results
		}
	}

	return results
}

func main() {
	latencies := map[string]time.Duration{
		"user-service":         20 * time.Millisecond,
		"billing-service":      40 * time.Millisecond,
		"inventory-service":    30 * time.Millisecond,
		"notification-service": 150 * time.Millisecond,
		"analytics-service":    time.Duration(50+rand.IntN(100)) * time.Millisecond,
	}

	aggregator := NewAPIAggregator(100 * time.Millisecond)
	results := aggregator.FetchAll(latencies)

	fmt.Println("=== API Aggregation Results (100ms deadline) ===")
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("  FAIL: %s -- %v\n", r.Name, r.Err)
			continue
		}
		fmt.Printf("  OK:   %s -- %v\n", r.Name, r.Duration)
	}
}
```

The overall deadline channel fires once. After that, all remaining services are marked as failed. The fast services still return their data -- the user gets a partial response rather than nothing.

### Verification
```bash
go run main.go
# Expected: fast services OK, notification-service (150ms) likely fails against 100ms deadline
```

## Step 3 -- The Timer Leak Problem and time.NewTimer Fix

`time.After` creates a timer that is not garbage collected until it fires. Inside a loop that runs thousands of times, this leaks memory. `time.NewTimer` lets you stop and reuse the timer.

```go
package main

import (
	"fmt"
	"time"
)

func demonstrateTimerLeak() {
	events := make(chan string, 100)

	go func() {
		for i := 0; i < 100; i++ {
			events <- fmt.Sprintf("event-%d", i)
			time.Sleep(time.Millisecond)
		}
		close(events)
	}()

	processed := 0
	for {
		select {
		case event, ok := <-events:
			if !ok {
				fmt.Printf("[LEAK VERSION] Processed %d events\n", processed)
				fmt.Println("  Problem: each iteration created a time.After timer")
				fmt.Println("  that lives until it fires, even if the event arrived first.")
				fmt.Println("  In a high-throughput loop, this leaks thousands of timers.")
				return
			}
			_ = event
			processed++
		case <-time.After(50 * time.Millisecond):
			// time.After allocates a NEW timer every loop iteration.
			// If the event arrives first, this timer is orphaned but
			// stays in memory until its 50ms expires.
			fmt.Println("Timeout waiting for event")
			return
		}
	}
}

func demonstrateTimerFix() {
	events := make(chan string, 100)

	go func() {
		for i := 0; i < 100; i++ {
			events <- fmt.Sprintf("event-%d", i)
			time.Sleep(time.Millisecond)
		}
		close(events)
	}()

	timeout := 50 * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop() // always stop when done

	processed := 0
	for {
		select {
		case event, ok := <-events:
			if !ok {
				fmt.Printf("[FIXED VERSION] Processed %d events\n", processed)
				fmt.Println("  Fix: one timer, reset each iteration. No leak.")
				return
			}
			_ = event
			processed++

			// Reset the timer for the next iteration.
			// Stop + drain + Reset is the safe pattern.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			fmt.Println("Timeout waiting for event")
			return
		}
	}
}

func main() {
	fmt.Println("=== Timer Leak Demonstration ===")
	fmt.Println()
	demonstrateTimerLeak()
	fmt.Println()
	demonstrateTimerFix()

	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Println("time.After in a loop: new timer each iteration (leaks until fired)")
	fmt.Println("time.NewTimer + Reset: one timer reused (no leak)")
	fmt.Println("Rule: use time.After only in one-shot selects, never in loops")
}
```

The safe reset pattern: `Stop()` returns false if the timer already fired, in which case you must drain `timer.C` before calling `Reset()`. Skipping the drain can cause the next `select` to read a stale timeout.

### Verification
```bash
go run main.go
# Expected: both versions process 100 events; commentary explains the leak difference
```

## Step 4 -- Production Pattern: context.WithTimeout

In real services, timeouts propagate across function boundaries. `context.WithTimeout` creates a context that automatically cancels after a deadline, and every function in the call chain can check it. This is the standard Go pattern for production timeout handling.

```go
package main

import (
	"context"
	"fmt"
	"time"
)

type ServiceResponse struct {
	Name string
	Data string
}

func callExternalService(ctx context.Context, name string, latency time.Duration) (ServiceResponse, error) {
	resultCh := make(chan ServiceResponse, 1)

	go func() {
		time.Sleep(latency)
		resultCh <- ServiceResponse{Name: name, Data: fmt.Sprintf("data from %s", name)}
	}()

	select {
	case resp := <-resultCh:
		return resp, nil
	case <-ctx.Done():
		return ServiceResponse{}, fmt.Errorf("service %s: %w", name, ctx.Err())
	}
}

type AggregatorService struct {
	perCallTimeout  time.Duration
	overallTimeout  time.Duration
}

func NewAggregatorService(perCall, overall time.Duration) *AggregatorService {
	return &AggregatorService{
		perCallTimeout: perCall,
		overallTimeout: overall,
	}
}

func (a *AggregatorService) Aggregate(services map[string]time.Duration) ([]ServiceResponse, []error) {
	overallCtx, overallCancel := context.WithTimeout(context.Background(), a.overallTimeout)
	defer overallCancel()

	type result struct {
		resp ServiceResponse
		err  error
	}

	results := make(chan result, len(services))

	for name, latency := range services {
		go func(name string, latency time.Duration) {
			perCallCtx, perCallCancel := context.WithTimeout(overallCtx, a.perCallTimeout)
			defer perCallCancel()

			resp, err := callExternalService(perCallCtx, name, latency)
			results <- result{resp: resp, err: err}
		}(name, latency)
	}

	var responses []ServiceResponse
	var errors []error

	for i := 0; i < len(services); i++ {
		r := <-results
		if r.err != nil {
			errors = append(errors, r.err)
			continue
		}
		responses = append(responses, r.resp)
	}

	return responses, errors
}

func main() {
	services := map[string]time.Duration{
		"user-service":         20 * time.Millisecond,
		"billing-service":      40 * time.Millisecond,
		"inventory-service":    60 * time.Millisecond,
		"notification-service": 150 * time.Millisecond,
		"analytics-service":    300 * time.Millisecond,
	}

	aggregator := NewAggregatorService(
		100*time.Millisecond, // per-call timeout
		200*time.Millisecond, // overall deadline
	)

	responses, errors := aggregator.Aggregate(services)

	fmt.Println("=== API Aggregation with context.WithTimeout ===")
	fmt.Printf("Per-call timeout: 100ms | Overall deadline: 200ms\n\n")

	fmt.Println("Successful responses:")
	for _, resp := range responses {
		fmt.Printf("  %s: %s\n", resp.Name, resp.Data)
	}

	if len(errors) > 0 {
		fmt.Println()
		fmt.Println("Failed calls:")
		for _, err := range errors {
			fmt.Printf("  %v\n", err)
		}
	}

	fmt.Printf("\nTotal: %d succeeded, %d failed\n", len(responses), len(errors))
}
```

`context.WithTimeout` composes naturally. The per-call context is derived from the overall context, so if the overall deadline expires, all per-call contexts cancel automatically. This is how production Go services handle layered timeouts.

### Verification
```bash
go run main.go
# Expected:
#   user-service, billing-service, inventory-service: OK (under 100ms)
#   notification-service: fails (150ms > 100ms per-call timeout)
#   analytics-service: fails (300ms > 100ms per-call timeout or 200ms overall)
```

## Step 5 -- Choosing the Right Timeout Pattern

| Pattern | Best For | Watch Out For |
|---|---|---|
| `time.After` | One-shot timeout in a single `select` | Leaks timers if used in a loop |
| `time.NewTimer` | Timeouts inside loops, reusable timers | Must stop and drain before reset |
| `context.WithTimeout` | Production services, propagating deadlines | Must call cancel (use `defer cancel()`) |
| Overall deadline + per-call | API aggregators, SLA enforcement | Per-call derives from overall for composability |

```go
package main

import "fmt"

func main() {
	fmt.Println("Timeout Pattern Decision Guide")
	fmt.Println("==============================")
	fmt.Println()
	fmt.Println("Q: Is this a one-shot operation (single select)?")
	fmt.Println("   Yes -> time.After is fine")
	fmt.Println()
	fmt.Println("Q: Is this inside a loop processing many events?")
	fmt.Println("   Yes -> time.NewTimer with Stop/Reset")
	fmt.Println()
	fmt.Println("Q: Does the timeout need to propagate to called functions?")
	fmt.Println("   Yes -> context.WithTimeout")
	fmt.Println()
	fmt.Println("Q: Do you need both per-call and overall deadlines?")
	fmt.Println("   Yes -> nested context.WithTimeout (child from parent)")
	fmt.Println()
	fmt.Println("Golden rule: always have a timeout. An unbounded wait")
	fmt.Println("is a production outage waiting to happen.")
}
```

### Verification
```bash
go run main.go
# Expected: decision guide printed
```

## Verification

Run all programs and confirm:
1. Per-operation timeouts cancel slow individual calls
2. The overall deadline cuts off the entire aggregation
3. The timer leak version and fixed version both process all events, but the fix avoids timer allocation per iteration
4. `context.WithTimeout` composes per-call and overall timeouts

## Common Mistakes

### Using time.After in a Hot Loop

**Wrong:**
```go
for msg := range messages {
    select {
    case process <- msg:
    case <-time.After(time.Second): // new timer EVERY iteration
    }
}
```

**Fix:** Use `time.NewTimer` and reset it each iteration. `time.After` inside a loop that runs thousands of times creates thousands of orphaned timers.

### Forgetting to Cancel the Context

**Wrong:**
```go
ctx, _ := context.WithTimeout(parentCtx, 5*time.Second)
// cancel function discarded -- timer resources leak
```

**Fix:** Always capture and defer the cancel function: `ctx, cancel := context.WithTimeout(...); defer cancel()`. Even if the operation completes before the timeout, `cancel()` releases the timer.

### Not Draining timer.C Before Reset

**Wrong:**
```go
timer.Stop()
timer.Reset(timeout) // if timer already fired, timer.C has a value -- next select reads stale timeout
```

**Fix:** After `Stop()` returns false, drain the channel before resetting:
```go
if !timer.Stop() {
    select {
    case <-timer.C:
    default:
    }
}
timer.Reset(timeout)
```

## Review

The four steps are one escalating answer to "how do I stop waiting?" `time.After` is the convenient one-shot: perfect inside a single `select`, but every call allocates a timer that lives until it fires, so using it inside a loop that runs thousands of times orphans thousands of timers -- the leak Step 3 measures. `time.NewTimer` fixes that by giving you a timer you own and reuse, with the Stop/drain/Reset dance guarding against a stale value sitting in `timer.C` after the timer already fired. `context.WithTimeout` is the production tool because a timeout is rarely local: deriving a per-call context from an overall context means the child cancels the instant either its own deadline or the parent's passes, so per-call and overall SLAs compose without any manual arithmetic. The invariant underneath all of it is that every blocking operation in a real service must have a timeout or deadline, because an unbounded wait is a cascading failure waiting to happen.

You should be able to answer three questions without rereading. Why does `time.After` inside a loop leak memory, and what changes when you switch to `time.NewTimer`? What is the exact safe sequence for resetting a timer, and why does skipping the drain break the next `select`? And how does deriving a per-call context from an overall context enforce layered deadlines rather than letting a slow call blow past the request's total budget? If you also remember to always `defer cancel()` -- releasing the timer even when the call finishes early -- you have the whole pattern.

## Resources

- [Go Blog: Context](https://go.dev/blog/context) -- how contexts carry deadlines and cancellation across API boundaries.
- [time.After](https://pkg.go.dev/time#After) -- the one-shot timer channel, with the note that it is not reclaimed until it fires.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) -- the reusable timer whose Stop/Reset semantics the leak fix depends on.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) -- the relative-deadline constructor the production aggregator layers per-call over overall.

---

Back to [Concurrency](../../concurrency.md) | Next: [14-channel-pipeline-basics](../14-channel-pipeline-basics/14-channel-pipeline-basics.md)
