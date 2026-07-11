# Exercise 4: Context WithDeadline

`WithTimeout` says "cancel after this many seconds from now"; `WithDeadline` says "cancel at this exact instant." The difference is invisible until a deadline arrives from outside. A gateway that received a request at 14:00:00 with a 14:00:05 SLA, and only reaches your handler at 14:00:02, must propagate the original 14:00:05 -- start a fresh five-second timeout there and you have quietly pushed the deadline to 14:00:07 and broken the contract. This exercise builds an SLA enforcer around absolute deadlines, watches the time budget shrink as a request flows through pipeline stages, and pins down exactly when to reach for `WithDeadline` over `WithTimeout`.

## What you'll build

```text
context-withdeadline/
  main.go        an SLA enforcer that meets and then misses an absolute
                 deadline, a WithDeadline-vs-WithTimeout comparison, a
                 fail-fast budget check, and budget shrinking across layers
```

- Build: a request pipeline governed by a single absolute deadline that every stage and layer reads from the context.
- Implement: an `SLAEnforcer` whose `processStage` races work against `ctx.Done()`; a `QueryExecutor` that skips work when `time.Until(deadline)` is too small; and a `RequestPipeline` where each layer logs its remaining budget.
- Verify: `go run main.go` at each step, reading the budget-remaining lines and the point where the deadline fires.

### Why WithDeadline is the lower-level primitive

`WithDeadline` is the lower-level primitive. `WithTimeout(parent, d)` is implemented internally as `WithDeadline(parent, time.Now().Add(d))`. Understanding both lets you choose the right tool: `WithTimeout` for relative durations ("timeout after 2 seconds"), `WithDeadline` for absolute points in time ("must complete by 14:00:05").

## Step 1 -- SLA Enforcer: Request Must Complete by Absolute Time

Build an SLA enforcer that sets an absolute deadline for request processing. Multiple stages must complete before the deadline expires:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const slaBudget = 500 * time.Millisecond

type PipelineStage struct {
	Name     string
	Duration time.Duration
}

type SLAEnforcer struct {
	stages []PipelineStage
}

func NewSLAEnforcer(stages []PipelineStage) *SLAEnforcer {
	return &SLAEnforcer{stages: stages}
}

func (e *SLAEnforcer) processStage(ctx context.Context, stage PipelineStage) error {
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline).Round(time.Millisecond)
	fmt.Printf("[%-12s] starting (budget remaining: %v, needs: %v)\n",
		stage.Name, remaining, stage.Duration)

	if remaining < stage.Duration {
		fmt.Printf("[%-12s] WARNING: insufficient budget, may timeout\n", stage.Name)
	}

	select {
	case <-time.After(stage.Duration):
		fmt.Printf("[%-12s] completed\n", stage.Name)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", stage.Name, ctx.Err())
	}
}

func (e *SLAEnforcer) Execute(ctx context.Context) error {
	for _, stage := range e.stages {
		if err := e.processStage(ctx, stage); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	slaDeadline := time.Now().Add(slaBudget)
	ctx, cancel := context.WithDeadline(context.Background(), slaDeadline)
	defer cancel()

	fmt.Printf("SLA deadline: %v\n", slaDeadline.Format("15:04:05.000"))
	fmt.Printf("Current time: %v\n\n", time.Now().Format("15:04:05.000"))

	enforcer := NewSLAEnforcer([]PipelineStage{
		{Name: "auth", Duration: 80 * time.Millisecond},
		{Name: "validation", Duration: 60 * time.Millisecond},
		{Name: "processing", Duration: 120 * time.Millisecond},
		{Name: "persistence", Duration: 100 * time.Millisecond},
	})

	if err := enforcer.Execute(ctx); err != nil {
		fmt.Printf("\nSLA VIOLATED: %v\n", err)
		fmt.Printf("Deadline was: %v\n", slaDeadline.Format("15:04:05.000"))
		fmt.Printf("Failed at:    %v\n", time.Now().Format("15:04:05.000"))
		return
	}

	fmt.Printf("\nSLA MET: all stages completed before %v\n",
		slaDeadline.Format("15:04:05.000"))
}
```

### Verification
```bash
go run main.go
```
Expected output (times will vary):
```
SLA deadline: 14:30:01.500
Current time: 14:30:01.000

[auth        ] starting (budget remaining: 499ms, needs: 80ms)
[auth        ] completed
[validation  ] starting (budget remaining: 419ms, needs: 60ms)
[validation  ] completed
[processing  ] starting (budget remaining: 358ms, needs: 120ms)
[processing  ] completed
[persistence ] starting (budget remaining: 237ms, needs: 100ms)
[persistence ] completed

SLA MET: all stages completed before 14:30:01.500
```

Each stage reports how much budget remains. You can see the budget shrinking as each stage consumes time. This is how real request pipelines work -- middleware, business logic, and data access all share a single request deadline.

## Step 2 -- SLA Violation: Budget Runs Out Mid-Request

Now increase the processing time so the deadline is exceeded during one of the stages:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const tightSLABudget = 400 * time.Millisecond

type PipelineStage struct {
	Name     string
	Duration time.Duration
}

type SLAEnforcer struct {
	stages []PipelineStage
}

func NewSLAEnforcer(stages []PipelineStage) *SLAEnforcer {
	return &SLAEnforcer{stages: stages}
}

func (e *SLAEnforcer) processStage(ctx context.Context, stage PipelineStage) error {
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline).Round(time.Millisecond)
	fmt.Printf("[%-12s] starting (remaining: %v)\n", stage.Name, remaining)

	select {
	case <-time.After(stage.Duration):
		fmt.Printf("[%-12s] completed in %v\n", stage.Name, stage.Duration)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s timed out after %v: %w", stage.Name,
			stage.Duration-time.Until(deadline), ctx.Err())
	}
}

func (e *SLAEnforcer) Execute(ctx context.Context) error {
	for _, stage := range e.stages {
		if err := e.processStage(ctx, stage); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	slaDeadline := time.Now().Add(tightSLABudget)
	ctx, cancel := context.WithDeadline(context.Background(), slaDeadline)
	defer cancel()

	fmt.Printf("SLA budget: %v\n", tightSLABudget)
	fmt.Println("Stages: auth(80ms) + validate(60ms) + process(300ms) + persist(100ms) = 540ms")
	fmt.Printf("Expected: SLA violation during 'process' stage\n\n")

	enforcer := NewSLAEnforcer([]PipelineStage{
		{Name: "auth", Duration: 80 * time.Millisecond},
		{Name: "validate", Duration: 60 * time.Millisecond},
		{Name: "process", Duration: 300 * time.Millisecond},
		{Name: "persist", Duration: 100 * time.Millisecond},
	})

	if err := enforcer.Execute(ctx); err != nil {
		fmt.Printf("\nSLA VIOLATED: %v\n", err)
		return
	}
	fmt.Println("SLA MET")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
SLA budget: 400ms
Stages: auth(80ms) + validate(60ms) + process(300ms) + persist(100ms) = 540ms
Expected: SLA violation during 'process' stage

[auth        ] starting (remaining: 400ms)
[auth        ] completed in 80ms
[validate    ] starting (remaining: 319ms)
[validate    ] completed in 60ms
[process     ] starting (remaining: 259ms)

SLA VIOLATED: process timed out after 300ms: context deadline exceeded
```

The "process" stage needed 300ms but only 259ms remained. The context deadline fired automatically, and the error tells you exactly which stage failed and why.

## Step 3 -- Comparing WithDeadline and WithTimeout

Show that `WithTimeout(parent, d)` is exactly `WithDeadline(parent, time.Now().Add(d))`. This matters when you need to choose between them:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const comparisonDuration = 2 * time.Second

func main() {
	now := time.Now()

	ctxTimeout, cancelTimeout := context.WithTimeout(context.Background(), comparisonDuration)
	defer cancelTimeout()

	ctxDeadline, cancelDeadline := context.WithDeadline(context.Background(), now.Add(comparisonDuration))
	defer cancelDeadline()

	deadlineFromTimeout, _ := ctxTimeout.Deadline()
	deadlineFromDeadline, _ := ctxDeadline.Deadline()

	diff := deadlineFromTimeout.Sub(deadlineFromDeadline).Abs()
	fmt.Printf("WithTimeout  deadline: %v\n", deadlineFromTimeout.Format("15:04:05.000000"))
	fmt.Printf("WithDeadline deadline: %v\n", deadlineFromDeadline.Format("15:04:05.000000"))
	fmt.Printf("Difference: %v (microseconds apart)\n\n", diff)

	fmt.Println("WHEN TO USE WHICH:")
	fmt.Println("  WithTimeout  -> relative: 'give this 2 seconds from now'")
	fmt.Println("  WithDeadline -> absolute: 'must finish by 14:00:05'")
	fmt.Println()
	fmt.Println("USE WithDeadline when:")
	fmt.Println("  - Propagating an SLA deadline from an upstream caller")
	fmt.Println("  - A gRPC/HTTP header carries an absolute deadline")
	fmt.Println("  - A batch job must finish before a maintenance window")
	fmt.Println()
	fmt.Println("USE WithTimeout when:")
	fmt.Println("  - Setting a per-call timeout on a database query")
	fmt.Println("  - Giving an HTTP request N seconds to complete")
	fmt.Println("  - Any 'max duration' from the current moment")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
WithTimeout  deadline: 14:30:03.000000
WithDeadline deadline: 14:30:03.000000
Difference: 50us (microseconds apart)

WHEN TO USE WHICH:
  WithTimeout  -> relative: 'give this 2 seconds from now'
  WithDeadline -> absolute: 'must finish by 14:00:05'

USE WithDeadline when:
  - Propagating an SLA deadline from an upstream caller
  - A gRPC/HTTP header carries an absolute deadline
  - A batch job must finish before a maintenance window

USE WithTimeout when:
  - Setting a per-call timeout on a database query
  - Giving an HTTP request N seconds to complete
  - Any 'max duration' from the current moment
```

## Step 4 -- Fail Fast: Check Budget Before Starting Expensive Work

In a real system, you should check whether you have enough budget before starting an expensive operation. Starting a 500ms database query with only 100ms of budget left is wasteful:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const queryBudget = 300 * time.Millisecond

type QueryExecutor struct{}

func NewQueryExecutor() *QueryExecutor {
	return &QueryExecutor{}
}

func (qe *QueryExecutor) Execute(ctx context.Context, query string, estimatedDuration time.Duration) (string, error) {
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		remaining := time.Until(deadline)
		if remaining < estimatedDuration {
			return "", fmt.Errorf(
				"query %q needs ~%v but only %v remains -- skipping to avoid wasted work",
				query, estimatedDuration, remaining.Round(time.Millisecond))
		}
		fmt.Printf("[db] executing %q (needs ~%v, budget: %v)\n",
			query, estimatedDuration, remaining.Round(time.Millisecond))
	}

	select {
	case <-time.After(estimatedDuration):
		return fmt.Sprintf("results for: %s", query), nil
	case <-ctx.Done():
		return "", fmt.Errorf("query %q: %w", query, ctx.Err())
	}
}

func main() {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(queryBudget))
	defer cancel()

	executor := NewQueryExecutor()

	result, err := executor.Execute(ctx, "SELECT * FROM users LIMIT 10", 100*time.Millisecond)
	if err != nil {
		fmt.Printf("[error] %v\n", err)
	} else {
		fmt.Printf("[ok]    %s\n", result)
	}

	result, err = executor.Execute(ctx, "SELECT * FROM orders JOIN ...", 500*time.Millisecond)
	if err != nil {
		fmt.Printf("[error] %v\n", err)
	} else {
		fmt.Printf("[ok]    %s\n", result)
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
[db] executing "SELECT * FROM users LIMIT 10" (needs ~100ms, budget: 300ms)
[ok]    results for: SELECT * FROM users LIMIT 10
[error] query "SELECT * FROM orders JOIN ..." needs ~500ms but only 199ms remains -- skipping to avoid wasted work
```

The second query detects that it does not have enough budget and fails immediately instead of starting work that is guaranteed to timeout. This saves database connections and CPU.

## Step 5 -- Remaining Budget Decreases Through Layers

Show how a single deadline propagates through multiple service layers, with each layer seeing less remaining budget:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	requestBudget     = 500 * time.Millisecond
	gatewayDelay      = 50 * time.Millisecond
	serviceDelay      = 80 * time.Millisecond
	repositoryDelay   = 100 * time.Millisecond
)

type RequestPipeline struct{}

func NewRequestPipeline() *RequestPipeline {
	return &RequestPipeline{}
}

func (p *RequestPipeline) logBudget(ctx context.Context, layer string) {
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline).Round(time.Millisecond)
	fmt.Printf("[%-12s] budget remaining: %v\n", layer, remaining)
}

func (p *RequestPipeline) Gateway(ctx context.Context) (string, error) {
	p.logBudget(ctx, "gateway")
	time.Sleep(gatewayDelay)
	return p.Service(ctx)
}

func (p *RequestPipeline) Service(ctx context.Context) (string, error) {
	p.logBudget(ctx, "service")
	time.Sleep(serviceDelay)
	return p.Repository(ctx)
}

func (p *RequestPipeline) Repository(ctx context.Context) (string, error) {
	p.logBudget(ctx, "repository")
	select {
	case <-time.After(repositoryDelay):
		return "data", nil
	case <-ctx.Done():
		return "", fmt.Errorf("repository: %w", ctx.Err())
	}
}

func main() {
	deadline := time.Now().Add(requestBudget)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	fmt.Printf("Absolute deadline: %v\n\n", deadline.Format("15:04:05.000"))

	pipeline := NewRequestPipeline()
	result, err := pipeline.Gateway(ctx)
	if err != nil {
		fmt.Printf("\nFailed: %v\n", err)
	} else {
		fmt.Printf("\nResult: %s\n", result)
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Absolute deadline: 14:30:01.500

[gateway     ] budget remaining: 500ms
[service     ] budget remaining: 449ms
[repository  ] budget remaining: 369ms

Result: data
```

The same absolute deadline is visible at every layer. Each layer sees less remaining time because previous layers consumed part of the budget. This is the natural behavior of deadline propagation -- no layer needs to compute its own timeout.

## Common Mistakes

### Confusing Deadline with Timeout
**Wrong:** Using `WithDeadline` with a duration instead of an absolute time:
```go
ctx, cancel := context.WithDeadline(parent, 5*time.Second) // compile error: wrong type
```
**Fix:** `WithDeadline` takes a `time.Time`, not a `time.Duration`:
```go
ctx, cancel := context.WithDeadline(parent, time.Now().Add(5*time.Second))
// or simply:
ctx, cancel := context.WithTimeout(parent, 5*time.Second)
```

### Assuming Child Can Extend Parent Deadline
A child context always gets the minimum of its own deadline and its parent's. You cannot use `WithDeadline` to grant more time than the parent allows. The parent's SLA is a hard ceiling.

### Not Checking Deadline Before Starting Expensive Work
As shown in Step 4, always check the remaining budget before starting operations with known minimum durations. Starting work that is guaranteed to timeout wastes connections, CPU, and may cause lock contention in the database.

## Review

The through-line is that a deadline is a single absolute point in time that the context carries for everyone. `context.WithDeadline(parent, t)` fires at `t`; `WithTimeout(parent, d)` is just `WithDeadline(parent, time.Now().Add(d))`, so the choice between them is really relative versus absolute -- reach for `WithDeadline` when the moment comes from upstream (an SLA, a gRPC/HTTP deadline header, a maintenance window) and for `WithTimeout` when you mean "give this N more seconds from here." Because a child context inherits the shorter of its own and its parent's deadline, the parent's SLA is a hard ceiling no descendant can extend, which is exactly why propagating the deadline rather than minting a new timeout preserves the contract. Two habits follow: read the remaining budget with `time.Until(deadline)` and fail fast before starting work that cannot finish in time, and expect that budget to shrink naturally as the request descends through layers -- no layer computes its own timeout because they all read the same clock.

You should be able to build the proof yourself. Assemble a three-stage pipeline of 100ms stages under a 350ms deadline and run it twice: once with a generous budget where all three complete, and once with a tight ~220ms budget where the context fires partway through and `ctx.Err()` reports `context deadline exceeded` at the exact stage that ran out of room. If you can predict which stage gets cut off from the budget-remaining line alone, and explain why a child could never have been granted more time than its parent, the mechanics of absolute deadlines are yours.

## Resources

- [Package context: WithDeadline](https://pkg.go.dev/context#WithDeadline) -- the absolute-deadline constructor and its cancellation semantics.
- [Package context: WithTimeout](https://pkg.go.dev/context#WithTimeout) -- the relative wrapper defined in terms of `WithDeadline`.
- [time.Until](https://pkg.go.dev/time#Until) -- how each stage computes its remaining budget from the context's deadline.

---

Back to [Concurrency](../../concurrency.md) | Next: [05-context-withvalue](../05-context-withvalue/05-context-withvalue.md)
