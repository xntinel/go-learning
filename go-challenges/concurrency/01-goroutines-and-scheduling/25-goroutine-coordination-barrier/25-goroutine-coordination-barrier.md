# Exercise 25: Goroutine Coordination Barrier

A deployment pipeline has strict ordering: validate every config, then run
database migrations, then deploy pods. Within a phase, tasks run concurrently
for speed; between phases, nothing may start until everything in the current
phase has finished. That is barrier synchronization -- "all must finish before
any can continue" -- and a bare `sync.WaitGroup` only covers the waiting half.
The moment one config validation fails, running migrations is pointless and
possibly dangerous, so the barrier also has to carry fail-fast cancellation and
error aggregation.

## What you'll build

```text
25-goroutine-coordination-barrier/
  main.go        phased deploy orchestrator: PhaseBarrier, fail-fast
                 cancellation, and a multi-phase pipeline with error reports
```

- Build: a phased deploy orchestrator that runs tasks concurrently within a phase and sequentially across phases.
- Implement: `PhaseBarrier` (wait-for-all), `FailFastBarrier` (cancel siblings on the first failure via a cancel channel and `sync.Once`), and `DeployPipeline` (multi-phase execution with per-phase error aggregation).
- Verify: `go run main.go`.

### Why "all must finish before any continue"

This is barrier synchronization: "all must finish before any can continue." It
appears everywhere in production systems -- CI/CD pipelines, distributed
transactions (two-phase commit), MapReduce (shuffle barrier between map and
reduce), game servers (all players must load before the match starts), and
parallel scientific computations (all workers must finish iteration N before
starting N+1).

The naive approach is a `sync.WaitGroup` per phase. But that only handles the
"wait for all" part. You also need error aggregation (which tasks failed?),
fail-fast (stop remaining tasks if one fails), and clean phase transitions (pass
results from phase N to phase N+1). Building a `PhaseBarrier` that handles all of
this teaches you how to coordinate groups of goroutines with both completion
synchronization and error handling.


## Step 1 -- Basic Phase Barrier

Build a `PhaseBarrier` that runs N tasks concurrently and waits for all to complete before returning.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	taskMinMs = 50
	taskMaxMs = 200
)

type TaskResult struct {
	Name    string
	Success bool
	Error   string
	Elapsed time.Duration
}

type PhaseBarrier struct {
	name string
}

func NewPhaseBarrier(name string) *PhaseBarrier {
	return &PhaseBarrier{name: name}
}

func (b *PhaseBarrier) Run(tasks map[string]func() error) []TaskResult {
	results := make([]TaskResult, 0, len(tasks))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, fn := range tasks {
		wg.Add(1)
		go func(taskName string, taskFn func() error) {
			defer wg.Done()

			start := time.Now()
			err := taskFn()
			elapsed := time.Since(start)

			result := TaskResult{
				Name:    taskName,
				Success: err == nil,
				Elapsed: elapsed,
			}
			if err != nil {
				result.Error = err.Error()
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(name, fn)
	}

	wg.Wait()
	return results
}

func simulateTask(name string, failRate float64) func() error {
	return func() error {
		duration := time.Duration(taskMinMs+rand.IntN(taskMaxMs-taskMinMs)) * time.Millisecond
		time.Sleep(duration)
		if rand.Float64() < failRate {
			return fmt.Errorf("%s: check failed", name)
		}
		return nil
	}
}

func printResults(phase string, results []TaskResult) {
	fmt.Printf("\n  Phase: %s (%d tasks)\n", phase, len(results))
	allOK := true
	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = fmt.Sprintf("FAIL: %s", r.Error)
			allOK = false
		}
		fmt.Printf("    %-25s %v  %s\n", r.Name, r.Elapsed.Round(time.Millisecond), status)
	}
	if allOK {
		fmt.Printf("  Result: ALL PASSED\n")
	} else {
		fmt.Printf("  Result: PHASE FAILED\n")
	}
}

func main() {
	fmt.Println("=== Phase Barrier: Basic ===")

	validate := NewPhaseBarrier("validate")
	results := validate.Run(map[string]func() error{
		"check-db-config":    simulateTask("db-config", 0),
		"check-api-config":   simulateTask("api-config", 0),
		"check-cache-config": simulateTask("cache-config", 0),
		"check-auth-config":  simulateTask("auth-config", 0),
	})
	printResults("validate", results)

	allPassed := true
	for _, r := range results {
		if !r.Success {
			allPassed = false
			break
		}
	}

	if !allPassed {
		fmt.Println()
		fmt.Println("  Pipeline aborted: validation failed")
		return
	}

	migrate := NewPhaseBarrier("migrate")
	results = migrate.Run(map[string]func() error{
		"migrate-users":    simulateTask("users", 0),
		"migrate-orders":   simulateTask("orders", 0),
		"migrate-products": simulateTask("products", 0),
	})
	printResults("migrate", results)

	fmt.Println()
	fmt.Println("  Pipeline complete")
}
```

**What's happening here:** `PhaseBarrier.Run` launches all tasks as goroutines, each protected by a `WaitGroup`. Results are collected with a mutex-protected slice. The main goroutine blocks on `wg.Wait()` until every task has finished. Only after all validation tasks pass does the pipeline proceed to migrations.

**Key insight:** The barrier is the `wg.Wait()` call. No task in the next phase can start until this returns. The results are checked after the barrier, and if any failed, the pipeline stops. This is sequential between phases, concurrent within phases.

### Verification
```bash
go run main.go
```
Expected output (durations vary, all tasks pass since failRate is 0):
```
=== Phase Barrier: Basic ===

  Phase: validate (4 tasks)
    check-db-config           87ms   OK
    check-cache-config        142ms  OK
    check-api-config          65ms   OK
    check-auth-config         178ms  OK
  Result: ALL PASSED

  Phase: migrate (3 tasks)
    migrate-users             120ms  OK
    migrate-orders            93ms   OK
    migrate-products          156ms  OK
  Result: ALL PASSED

  Pipeline complete
```


## Step 2 -- Fail-Fast with Cancellation

Enhance the barrier so that when one task fails, remaining tasks in the same phase are cancelled immediately.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	ffTaskMinMs = 100
	ffTaskMaxMs = 500
)

type PhaseResult struct {
	Name    string
	Success bool
	Error   string
	Elapsed time.Duration
}

type FailFastBarrier struct {
	name string
}

func NewFailFastBarrier(name string) *FailFastBarrier {
	return &FailFastBarrier{name: name}
}

func (b *FailFastBarrier) Run(tasks map[string]func(cancel <-chan struct{}) error) ([]PhaseResult, bool) {
	cancel := make(chan struct{})
	var cancelOnce sync.Once

	results := make([]PhaseResult, 0, len(tasks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	allOK := true

	for name, fn := range tasks {
		wg.Add(1)
		go func(taskName string, taskFn func(cancel <-chan struct{}) error) {
			defer wg.Done()

			start := time.Now()
			err := taskFn(cancel)
			elapsed := time.Since(start)

			result := PhaseResult{
				Name:    taskName,
				Success: err == nil,
				Elapsed: elapsed,
			}
			if err != nil {
				result.Error = err.Error()
				mu.Lock()
				allOK = false
				mu.Unlock()
				cancelOnce.Do(func() { close(cancel) })
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(name, fn)
	}

	wg.Wait()
	return results, allOK
}

func cancellableTask(name string, duration time.Duration, shouldFail bool) func(cancel <-chan struct{}) error {
	return func(cancel <-chan struct{}) error {
		deadline := time.After(duration)
		select {
		case <-deadline:
			if shouldFail {
				return fmt.Errorf("%s: validation error", name)
			}
			return nil
		case <-cancel:
			return fmt.Errorf("%s: cancelled", name)
		}
	}
}

func main() {
	fmt.Println("=== Fail-Fast Barrier ===")

	fmt.Println()
	fmt.Println("--- Scenario 1: All tasks succeed ---")
	barrier1 := NewFailFastBarrier("deploy-checks")
	results, ok := barrier1.Run(map[string]func(cancel <-chan struct{}) error{
		"health-check":  cancellableTask("health", 100*time.Millisecond, false),
		"disk-space":    cancellableTask("disk", 150*time.Millisecond, false),
		"memory-check":  cancellableTask("memory", 80*time.Millisecond, false),
	})
	printPhaseResults("deploy-checks", results, ok)

	fmt.Println()
	fmt.Println("--- Scenario 2: One task fails fast ---")
	barrier2 := NewFailFastBarrier("validation")
	results, ok = barrier2.Run(map[string]func(cancel <-chan struct{}) error{
		"schema-check":  cancellableTask("schema", 500*time.Millisecond, false),
		"auth-check":    cancellableTask("auth", 100*time.Millisecond, true),
		"config-check":  cancellableTask("config", 400*time.Millisecond, false),
		"cert-check":    cancellableTask("cert", 300*time.Millisecond, false),
	})
	printPhaseResults("validation", results, ok)

	if !ok {
		fmt.Println()
		fmt.Println("  Pipeline halted: fail-fast triggered")
		fmt.Println("  Note: slow tasks were cancelled, saving ~400ms of wasted work")
	}
}

func printPhaseResults(phase string, results []PhaseResult, allOK bool) {
	fmt.Printf("\n  Phase: %s (%d tasks)\n", phase, len(results))
	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = fmt.Sprintf("FAIL: %s", r.Error)
		}
		fmt.Printf("    %-20s %v  %s\n", r.Name, r.Elapsed.Round(time.Millisecond), status)
	}
	if allOK {
		fmt.Printf("  Result: ALL PASSED\n")
	} else {
		fmt.Printf("  Result: PHASE FAILED (fail-fast)\n")
	}
}
```

**What's happening here:** Each task receives a `cancel` channel. When any task fails, it closes the cancel channel (via `sync.Once` to prevent double-close). Other tasks check the cancel channel in their `select` and return immediately with a "cancelled" error. The barrier still waits for all goroutines to exit (via `wg.Wait()`), but cancelled tasks exit quickly instead of running to completion.

**Key insight:** Fail-fast saves resources. If a 100ms task fails, there is no point waiting 500ms for the slow task to finish. The cancel channel is the broadcast mechanism -- closing it unblocks all goroutines waiting on it simultaneously. `sync.Once` ensures the channel is closed exactly once, even if multiple tasks fail at the same time.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Fail-Fast Barrier ===

--- Scenario 1: All tasks succeed ---

  Phase: deploy-checks (3 tasks)
    memory-check         80ms   OK
    health-check         100ms  OK
    disk-space           150ms  OK
  Result: ALL PASSED

--- Scenario 2: One task fails fast ---

  Phase: validation (4 tasks)
    auth-check           100ms  FAIL: auth: validation error
    schema-check         100ms  FAIL: schema: cancelled
    cert-check           100ms  FAIL: cert: cancelled
    config-check         100ms  FAIL: config: cancelled
  Result: PHASE FAILED (fail-fast)

  Pipeline halted: fail-fast triggered
  Note: slow tasks were cancelled, saving ~400ms of wasted work
```


## Step 3 -- Multi-Phase Pipeline with Error Aggregation

Build a complete deploy orchestrator that executes multiple phases in sequence, with fail-fast within each phase and proper error reporting across the pipeline.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

const (
	pipelineMinMs = 50
	pipelineMaxMs = 300
)

type StepResult struct {
	Task    string
	OK      bool
	Error   string
	Elapsed time.Duration
}

type PhaseReport struct {
	Name     string
	Results  []StepResult
	Success  bool
	Duration time.Duration
}

type PipelineReport struct {
	Phases   []PhaseReport
	Success  bool
	Duration time.Duration
}

type Phase struct {
	Name  string
	Tasks map[string]func(cancel <-chan struct{}) error
}

type DeployPipeline struct {
	phases []Phase
}

func NewDeployPipeline() *DeployPipeline {
	return &DeployPipeline{
		phases: make([]Phase, 0),
	}
}

func (p *DeployPipeline) AddPhase(name string, tasks map[string]func(cancel <-chan struct{}) error) {
	p.phases = append(p.phases, Phase{Name: name, Tasks: tasks})
}

func (p *DeployPipeline) Execute() PipelineReport {
	report := PipelineReport{
		Phases: make([]PhaseReport, 0, len(p.phases)),
	}
	pipelineStart := time.Now()

	for _, phase := range p.phases {
		phaseReport := p.runPhase(phase)
		report.Phases = append(report.Phases, phaseReport)

		if !phaseReport.Success {
			report.Success = false
			report.Duration = time.Since(pipelineStart)
			return report
		}
	}

	report.Success = true
	report.Duration = time.Since(pipelineStart)
	return report
}

func (p *DeployPipeline) runPhase(phase Phase) PhaseReport {
	phaseStart := time.Now()
	cancel := make(chan struct{})
	var cancelOnce sync.Once

	results := make([]StepResult, 0, len(phase.Tasks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	phaseOK := true

	for name, fn := range phase.Tasks {
		wg.Add(1)
		go func(taskName string, taskFn func(cancel <-chan struct{}) error) {
			defer wg.Done()

			start := time.Now()
			err := taskFn(cancel)
			elapsed := time.Since(start)

			result := StepResult{
				Task:    taskName,
				OK:      err == nil,
				Elapsed: elapsed,
			}
			if err != nil {
				result.Error = err.Error()
				mu.Lock()
				phaseOK = false
				mu.Unlock()
				cancelOnce.Do(func() { close(cancel) })
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(name, fn)
	}

	wg.Wait()
	return PhaseReport{
		Name:     phase.Name,
		Results:  results,
		Success:  phaseOK,
		Duration: time.Since(phaseStart),
	}
}

func deployTask(name string, failRate float64) func(cancel <-chan struct{}) error {
	return func(cancel <-chan struct{}) error {
		duration := time.Duration(pipelineMinMs+rand.IntN(pipelineMaxMs-pipelineMinMs)) * time.Millisecond
		select {
		case <-time.After(duration):
			if rand.Float64() < failRate {
				return fmt.Errorf("%s failed: unexpected error", name)
			}
			return nil
		case <-cancel:
			return fmt.Errorf("%s: cancelled", name)
		}
	}
}

func printReport(report PipelineReport) {
	fmt.Printf("\n=== Pipeline Report ===\n")
	fmt.Printf("  Status:   %s\n", statusString(report.Success))
	fmt.Printf("  Duration: %v\n", report.Duration.Round(time.Millisecond))
	fmt.Printf("  Phases:   %d\n\n", len(report.Phases))

	for i, phase := range report.Phases {
		marker := "PASS"
		if !phase.Success {
			marker = "FAIL"
		}
		fmt.Printf("  Phase %d: %s [%s] (%v)\n", i+1, phase.Name, marker, phase.Duration.Round(time.Millisecond))

		for _, r := range phase.Results {
			status := "ok"
			if !r.OK {
				status = r.Error
			}
			fmt.Printf("    %-30s %v  %s\n", r.Task, r.Elapsed.Round(time.Millisecond), status)
		}
		fmt.Println()
	}

	if !report.Success {
		fmt.Println("  FAILED TASKS:")
		for _, phase := range report.Phases {
			for _, r := range phase.Results {
				if !r.OK && !strings.Contains(r.Error, "cancelled") {
					fmt.Printf("    [%s] %s: %s\n", phase.Name, r.Task, r.Error)
				}
			}
		}
	}
}

func statusString(ok bool) string {
	if ok {
		return "SUCCESS"
	}
	return "FAILED"
}

func main() {
	fmt.Println("=== Deploy Orchestrator ===")

	fmt.Println()
	fmt.Println("--- Run 1: All phases succeed ---")
	pipeline1 := NewDeployPipeline()
	pipeline1.AddPhase("validate", map[string]func(cancel <-chan struct{}) error{
		"validate-db-schema":  deployTask("db-schema", 0),
		"validate-api-config": deployTask("api-config", 0),
		"validate-secrets":    deployTask("secrets", 0),
	})
	pipeline1.AddPhase("migrate", map[string]func(cancel <-chan struct{}) error{
		"migrate-users-table":  deployTask("users", 0),
		"migrate-orders-table": deployTask("orders", 0),
	})
	pipeline1.AddPhase("deploy", map[string]func(cancel <-chan struct{}) error{
		"deploy-api-pods":     deployTask("api-pods", 0),
		"deploy-worker-pods":  deployTask("worker-pods", 0),
		"deploy-gateway":      deployTask("gateway", 0),
	})
	printReport(pipeline1.Execute())

	fmt.Println()
	fmt.Println("--- Run 2: Migration fails (deploy never runs) ---")
	pipeline2 := NewDeployPipeline()
	pipeline2.AddPhase("validate", map[string]func(cancel <-chan struct{}) error{
		"validate-db-schema":  deployTask("db-schema", 0),
		"validate-api-config": deployTask("api-config", 0),
	})
	pipeline2.AddPhase("migrate", map[string]func(cancel <-chan struct{}) error{
		"migrate-users-table":    deployTask("users", 1.0),
		"migrate-orders-table":   deployTask("orders", 0),
		"migrate-products-table": deployTask("products", 0),
	})
	pipeline2.AddPhase("deploy", map[string]func(cancel <-chan struct{}) error{
		"deploy-api-pods": deployTask("api-pods", 0),
	})
	printReport(pipeline2.Execute())
}
```

**What's happening here:** The `DeployPipeline` holds an ordered list of phases. `Execute` runs each phase sequentially. Within each phase, tasks run concurrently with fail-fast. If any phase fails, the pipeline stops -- later phases are never executed. The report shows per-phase and per-task results, distinguishing between tasks that actually failed and tasks that were cancelled.

**Key insight:** The pipeline enforces two levels of coordination: (1) barrier synchronization between phases (sequential), and (2) concurrent execution within each phase (parallel with fail-fast). The report distinguishes "real failures" from "cancelled tasks" so operators know exactly what went wrong. In the second run, the migrate phase fails so the deploy phase never executes -- this prevents deploying code against an incompatible database schema.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Deploy Orchestrator ===

--- Run 1: All phases succeed ---

=== Pipeline Report ===
  Status:   SUCCESS
  Duration: 583ms
  Phases:   3

  Phase 1: validate [PASS] (198ms)
    validate-db-schema             120ms  ok
    validate-api-config            198ms  ok
    validate-secrets               87ms   ok

  Phase 2: migrate [PASS] (165ms)
    migrate-users-table            165ms  ok
    migrate-orders-table           92ms   ok

  Phase 3: deploy [PASS] (220ms)
    deploy-api-pods                156ms  ok
    deploy-worker-pods             220ms  ok
    deploy-gateway                 189ms  ok


--- Run 2: Migration fails (deploy never runs) ---

=== Pipeline Report ===
  Status:   FAILED
  Duration: 362ms
  Phases:   2

  Phase 1: validate [PASS] (178ms)
    validate-db-schema             142ms  ok
    validate-api-config            178ms  ok

  Phase 2: migrate [FAIL] (105ms)
    migrate-users-table            105ms  users failed: unexpected error
    migrate-orders-table           105ms  orders: cancelled
    migrate-products-table         105ms  products: cancelled

  FAILED TASKS:
    [migrate] migrate-users-table: users failed: unexpected error
```


## Common Mistakes

### Not Waiting for Cancelled Goroutines to Exit

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	cancel := make(chan struct{})

	for i := 0; i < 5; i++ {
		go func(id int) {
			select {
			case <-time.After(1 * time.Second):
				fmt.Printf("  task %d completed\n", id)
			case <-cancel:
				fmt.Printf("  task %d cancelled\n", id)
				// goroutine exits, but nobody waits for it
			}
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(cancel)
	// main continues immediately without waiting for goroutines to print
	fmt.Println("done")
}
```
**What happens:** After closing `cancel`, the goroutines are notified but `main` does not wait for them to finish. Some goroutines may not even have time to print their "cancelled" message before the program exits. In production, this means cleanup code (closing files, releasing resources) may not run.

**Fix:** Always pair cancel channels with a `WaitGroup`:
```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	cancel := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			select {
			case <-time.After(1 * time.Second):
				fmt.Printf("  task %d completed\n", id)
			case <-cancel:
				fmt.Printf("  task %d cancelled\n", id)
			}
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(cancel)
	wg.Wait() // ensures all goroutines have finished
	fmt.Println("done")
}
```


### Closing the Cancel Channel Multiple Times

```go
package main

import "fmt"

func main() {
	cancel := make(chan struct{})

	// Two goroutines both try to signal cancellation
	go func() {
		close(cancel) // first close: ok
	}()
	go func() {
		close(cancel) // second close: PANIC: close of closed channel
	}()

	<-cancel
	fmt.Println("cancelled")
}
```
**What happens:** Closing an already-closed channel panics. If multiple tasks can fail concurrently (which is the whole point of concurrent execution), multiple goroutines may try to close the cancel channel simultaneously.

**Fix:** Use `sync.Once` to ensure the channel is closed exactly once:
```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	cancel := make(chan struct{})
	var once sync.Once

	for i := 0; i < 5; i++ {
		go func() {
			once.Do(func() { close(cancel) })
		}()
	}

	time.Sleep(50 * time.Millisecond)
	<-cancel
	fmt.Println("cancelled safely")
}
```


### Proceeding to Next Phase Without Checking Errors

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	phases := []string{"validate", "migrate", "deploy"}

	for _, phase := range phases {
		var wg sync.WaitGroup
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if p == "migrate" {
				fmt.Printf("  %s: FAILED\n", p)
				return
			}
			fmt.Printf("  %s: ok\n", p)
		}(phase)
		wg.Wait()
		// BUG: proceeds to next phase even though migrate failed
	}

	fmt.Println("deploy completed (but it shouldn't have)")
}
```
**What happens:** The loop waits for each phase to complete but never checks whether it succeeded. The deploy phase runs after a failed migration, potentially deploying code against an incompatible database.

**Fix:** Collect results from each phase and stop the pipeline on failure, as shown in Step 3 of this exercise.


## Review

Barrier synchronization is the guarantee that no goroutine in the next phase
starts until every goroutine in the current one has finished, and in this
exercise the barrier is always a `wg.Wait()` -- `Add` before each launch, `Done`
on exit, `Wait` to block. On top of that base sit two refinements. Fail-fast
turns a single failure into a broadcast: the first task to fail closes a shared
cancel channel (guarded by `sync.Once`, because closing a closed channel panics
and several tasks may fail at once), and every other task, blocked in a `select`
on that channel, returns immediately instead of running to completion. Error
aggregation at the phase level then separates the task that actually failed from
the ones that were merely cancelled, so an operator reading the report sees the
root cause rather than a wall of noise. The whole structure is sequential
between phases and concurrent within them -- both patterns composed into one
pipeline.

To prove you own the pattern, build a four-phase build pipeline -- lint, test,
build, publish -- where each task waits on `time.After` inside a `select` with
the cancel channel rather than `time.Sleep`, and inject a failure into one test
task. A correct implementation lets the lint phase finish, makes the test phase
fail fast so the cancelled tasks return early, never runs build or publish, and
files a report that names the root-cause failure apart from the cancellations.
Remember too that cancellation is a notification, not a stop: you must still
`wg.Wait()` for cancelled goroutines so their cleanup runs before the phase is
declared done.

## Resources
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- Add/Done/Wait, the wait-for-all barrier primitive.
- [sync.Once](https://pkg.go.dev/sync#Once) -- guarantees the cancel channel is closed exactly once even when several tasks fail together.
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- official patterns for wiring cancellation through concurrent stages.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- goroutine and channel coordination idioms.

---

Back to [Concurrency](../../concurrency.md) | Next: [26-goroutine-safe-service-registry](../26-goroutine-safe-service-registry/26-goroutine-safe-service-registry.md)
