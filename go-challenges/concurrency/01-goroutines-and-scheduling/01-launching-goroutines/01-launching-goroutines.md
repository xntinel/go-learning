# Exercise 1: Launching Goroutines

Goroutines are the fundamental unit of concurrency in Go, and they are almost
free: a goroutine starts with a few kilobytes of stack and the runtime
multiplexes thousands of them onto a handful of OS threads. That cheapness is
what makes fan-out practical -- you can launch one goroutine per dependency
without a second thought. The catch is everything around the launch: `main` is
itself a goroutine, so when it returns the rest die mid-flight, and a loop that
shares one variable across its goroutines has every worker read the same value.
This exercise builds a service health checker that gets both right.

## What you'll build

```text
01-launching-goroutines/
  main.go        sequential vs concurrent health checks, fan-out with
                 channel results, and the closure-capture bug beside its fix
```

- Build: a concurrent service health checker that fans out one goroutine per dependency.
- Implement: `runSequentialChecks` / `runConcurrentChecks`, anonymous goroutines that send `HealthResult`s over a channel, and `fanOutHealthChecks` collecting results into a report.
- Verify: `go run main.go`, plus `go run -race main.go` to expose the shared-variable capture bug.

### Why waiting and safe argument passing are the bedrock

The `go` keyword is the gateway to concurrent programming in Go. By placing `go` before a function call, you tell the runtime to execute that function independently, without waiting for it to finish. Understanding how goroutines launch, how they interleave with `main`, and how to pass data to them safely is the bedrock upon which all other concurrency patterns are built.

A critical subtlety is that `main` itself runs in a goroutine. When `main` returns, all other goroutines are terminated immediately, regardless of whether they have finished. This means you must explicitly wait for goroutines to complete -- a theme that will recur throughout this series. In this exercise, we use `sync.WaitGroup` for proper synchronization rather than `time.Sleep`, which is fragile and non-deterministic.

## Step 1 -- Sequential vs Concurrent Health Checks

Imagine you operate a platform that depends on several upstream services: an authentication API, a payment gateway, a notification service, and others. Before deploying a new release, your CLI tool checks that every dependency is healthy. Running these checks one after another wastes time when each check is just waiting for a network response.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type ServiceEndpoint struct {
	Name    string
	Latency time.Duration
}

func checkService(name string, latency time.Duration) string {
	time.Sleep(latency)
	return fmt.Sprintf("%-18s UP  (%v)", name, latency)
}

func runSequentialChecks(services []ServiceEndpoint) time.Duration {
	start := time.Now()
	for _, svc := range services {
		result := checkService(svc.Name, svc.Latency)
		fmt.Printf("  %s\n", result)
	}
	return time.Since(start)
}

func runConcurrentChecks(services []ServiceEndpoint) time.Duration {
	start := time.Now()
	var wg sync.WaitGroup

	for _, svc := range services {
		wg.Add(1)
		go func(name string, latency time.Duration) {
			defer wg.Done()
			result := checkService(name, latency)
			fmt.Printf("  %s\n", result)
		}(svc.Name, svc.Latency)
	}
	wg.Wait()

	return time.Since(start)
}

func main() {
	services := []ServiceEndpoint{
		{"auth-api", 120 * time.Millisecond},
		{"payment-gateway", 200 * time.Millisecond},
		{"notification-svc", 80 * time.Millisecond},
		{"inventory-api", 150 * time.Millisecond},
		{"search-engine", 90 * time.Millisecond},
	}

	fmt.Println("--- Sequential Health Check ---")
	seqDuration := runSequentialChecks(services)
	fmt.Printf("  Sequential total: %v\n\n", seqDuration.Round(time.Millisecond))

	fmt.Println("--- Concurrent Health Check ---")
	concDuration := runConcurrentChecks(services)
	fmt.Printf("  Concurrent total: %v\n", concDuration.Round(time.Millisecond))
}
```

**What's happening here:** In the sequential version, each `checkService` call must finish before the next starts. Total time is the sum of all latencies: ~640ms. In the concurrent version, `go func(...)` launches each check as an independent goroutine. All five run simultaneously, so total time equals the slowest single check: ~200ms.

**Key insight:** The `go` keyword does not wait. It launches the function and returns immediately. `wg.Wait()` blocks until all goroutines call `wg.Done()`.

**What would happen if you removed** `wg.Wait()`**?** Main would exit immediately, killing all goroutines before they complete any health check. Your CLI would report nothing.

### Verification

```bash
go run main.go
```

Expected output:

```
--- Sequential Health Check ---
  auth-api           UP  (120ms)
  payment-gateway    UP  (200ms)
  notification-svc   UP  (80ms)
  inventory-api      UP  (150ms)
  search-engine      UP  (90ms)
  Sequential total: 640ms

--- Concurrent Health Check ---
  notification-svc   UP  (80ms)
  search-engine      UP  (90ms)
  auth-api           UP  (120ms)
  inventory-api      UP  (150ms)
  payment-gateway    UP  (200ms)
  Concurrent total: 200ms
```

## Step 2 -- Anonymous Goroutines with Channel Results

In production, you do not just print results -- you need to collect them for further processing. Anonymous goroutines can send results through channels so the caller decides what to do with them.

```go
package main

import (
	"fmt"
	"time"
)

const degradedService = "payment-gateway"

type HealthResult struct {
	Service string
	Healthy bool
	Latency time.Duration
}

func simulateHealthCheck(serviceName string) HealthResult {
	checkStart := time.Now()
	simulatedLatency := time.Duration(50+len(serviceName)*10) * time.Millisecond
	time.Sleep(simulatedLatency)

	return HealthResult{
		Service: serviceName,
		Healthy: serviceName != degradedService,
		Latency: time.Since(checkStart),
	}
}

func launchChecks(services []string) <-chan HealthResult {
	results := make(chan HealthResult, len(services))
	for _, svc := range services {
		go func(name string) {
			results <- simulateHealthCheck(name)
		}(svc)
	}
	return results
}

func collectResults(results <-chan HealthResult, count int) (downCount int) {
	for i := 0; i < count; i++ {
		r := <-results
		status := "UP"
		if !r.Healthy {
			status = "DOWN"
			downCount++
		}
		fmt.Printf("  %-20s %4s  (%v)\n", r.Service, status, r.Latency.Round(time.Millisecond))
	}
	return downCount
}

func main() {
	services := []string{"auth-api", "payment-gateway", "notification-svc", "inventory-api", "search-engine"}

	start := time.Now()
	results := launchChecks(services)
	downCount := collectResults(results, len(services))

	fmt.Printf("\n  Total: %v | Services down: %d/%d\n",
		time.Since(start).Round(time.Millisecond), downCount, len(services))
}
```

**What's happening here:** Each anonymous goroutine sends a `HealthResult` struct through a buffered channel. The main goroutine collects exactly `len(services)` results. The trailing `(svc)` on the anonymous function captures the loop variable safely.

**Key insight:** Parameters are copied at the moment the goroutine is launched, not when it executes. This is why passing `svc` as a function argument is safer than capturing the loop variable by reference.

### Verification

```bash
go run main.go
```

Expected output (order varies):

```
  auth-api             UP    (120ms)
  search-engine        UP    (180ms)
  notification-svc     UP    (210ms)
  payment-gateway      DOWN  (210ms)
  inventory-api        UP    (180ms)

  Total: 213ms | Services down: 1/5
```

## Step 3 -- The Closure Capture Bug in Real Code

When building goroutines inside a loop, a common production bug is accidentally sharing a variable declared *outside* the loop. In a health checker, this means every goroutine checks the SAME service, missing failures on others entirely.

```go
package main

import (
	"fmt"
	"sync"
)

func demonstrateBuggyCapture(endpoints []string) {
	fmt.Println("--- BUG: shared variable capture ---")
	var wg sync.WaitGroup

	// One single variable for the entire loop. Every goroutine closes over
	// this same memory, and main keeps overwriting it while they run.
	var target string

	for _, ep := range endpoints {
		target = ep
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("  [BUG] checking: %s\n", target)
		}()
	}
	wg.Wait()
}

func demonstrateCorrectCapture(endpoints []string) {
	fmt.Println()
	fmt.Println("--- FIX: argument passing ---")
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(index int, endpoint string) {
			defer wg.Done()
			fmt.Printf("  [OK]  goroutine %d checking: %s\n", index, endpoint)
		}(i, ep)
	}
	wg.Wait()
}

func main() {
	endpoints := []string{
		"https://auth.internal/health",
		"https://payments.internal/health",
		"https://notifications.internal/health",
		"https://inventory.internal/health",
	}

	demonstrateBuggyCapture(endpoints)
	demonstrateCorrectCapture(endpoints)
}
```

**What's happening here:** In the BUG version, `target` is a single variable shared by every goroutine. Main overwrites it on each iteration while the goroutines read it, so they almost always observe the last endpoint. In the FIX version, each goroutine receives its own copy via function arguments.

**Key insight:** Go 1.22+ changed loop variable semantics so that `for _, ep := range endpoints` creates a new `ep` per iteration -- capturing `ep` directly is now safe. The bug survives only for variables declared *outside* the loop, like `target` here. This is also a genuine data race: `go run -race main.go` reports it.

### Verification

```bash
go run main.go
```

Expected output (the BUG lines almost always show the last endpoint, and the FIX order varies):

```
--- BUG: shared variable capture ---
  [BUG] checking: https://inventory.internal/health
  [BUG] checking: https://inventory.internal/health
  [BUG] checking: https://inventory.internal/health
  [BUG] checking: https://inventory.internal/health

--- FIX: argument passing ---
  [OK]  goroutine 2 checking: https://notifications.internal/health
  [OK]  goroutine 0 checking: https://auth.internal/health
  [OK]  goroutine 3 checking: https://inventory.internal/health
  [OK]  goroutine 1 checking: https://payments.internal/health
```

Confirm the race the bug depends on:

```bash
go run -race main.go
```

## Step 4 -- Fan-Out Health Check with Timeout Simulation

The complete pattern: launch one goroutine per service, collect results through a channel, and report a structured summary. This is the foundation of every concurrent CLI tool.

```go
package main

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"
)

const (
	minLatency       = 30
	maxExtraLatency  = 150
	degradedChance   = 0.15
	avgLatencyPerSvc = 100 // milliseconds, for sequential estimate
)

type ServiceHealth struct {
	Name    string
	Status  string
	Latency time.Duration
}

type HealthReport struct {
	Results  []ServiceHealth
	Healthy  int
	Degraded int
}

func checkServiceHealth(name string) ServiceHealth {
	checkStart := time.Now()
	latency := time.Duration(rand.IntN(maxExtraLatency)+minLatency) * time.Millisecond
	time.Sleep(latency)

	status := "UP"
	if rand.Float32() < degradedChance {
		status = "DEGRADED"
	}

	return ServiceHealth{
		Name:    name,
		Status:  status,
		Latency: time.Since(checkStart),
	}
}

func fanOutHealthChecks(services []string) []ServiceHealth {
	results := make(chan ServiceHealth, len(services))
	for _, svc := range services {
		go func(name string) {
			results <- checkServiceHealth(name)
		}(svc)
	}

	var all []ServiceHealth
	for i := 0; i < len(services); i++ {
		all = append(all, <-results)
	}

	slices.SortFunc(all, func(a, b ServiceHealth) int { return cmp.Compare(a.Latency, b.Latency) })
	return all
}

func buildReport(results []ServiceHealth) HealthReport {
	report := HealthReport{Results: results}
	for _, r := range results {
		if r.Status == "DEGRADED" {
			report.Degraded++
		} else {
			report.Healthy++
		}
	}
	return report
}

func printReport(report HealthReport, wallClock time.Duration) {
	fmt.Println("=== Service Health Report ===")
	for _, r := range report.Results {
		marker := "  "
		if r.Status == "DEGRADED" {
			marker = "!!"
		}
		fmt.Printf("  %s %-22s %-10s %v\n", marker, r.Name, r.Status, r.Latency.Round(time.Millisecond))
	}

	total := len(report.Results)
	fmt.Printf("\n  Checked %d services in %v\n", total, wallClock.Round(time.Millisecond))
	fmt.Printf("  Healthy: %d | Degraded: %d\n", report.Healthy, report.Degraded)
	fmt.Printf("  Sequential would have taken: ~%v\n",
		time.Duration(total*avgLatencyPerSvc)*time.Millisecond)
}

func main() {
	services := []string{
		"auth-api", "payment-gateway", "notification-svc",
		"inventory-api", "search-engine", "user-profile-svc",
		"order-service", "analytics-api", "cdn-gateway", "cache-cluster",
	}

	start := time.Now()
	results := fanOutHealthChecks(services)
	wallClock := time.Since(start)

	report := buildReport(results)
	printReport(report, wallClock)
}
```

**What's happening here:** Ten goroutines start simultaneously, each simulating a health check with variable latency. Results arrive in completion order through the channel, get sorted by latency, and are printed as a structured report. The fan-out pattern is safe because each goroutine operates on its own data.

**Key insight:** The fan-out pattern is the natural fit for independent checks. Wall-clock time equals the slowest service (~180ms), not the sum of all (~1000ms). In production, this is the difference between a deployment check that takes 10 seconds and one that takes 200ms.

### Verification

```bash
go run main.go
```

Expected output (order and values vary):

```
=== Service Health Report ===
     notification-svc       UP         35ms
     cache-cluster          UP         52ms
  !! search-engine          DEGRADED   67ms
     auth-api               UP         89ms
     order-service          UP         102ms
     ...

  Checked 10 services in 178ms
  Healthy: 8 | Degraded: 2
  Sequential would have taken: ~1s
```

## Common Mistakes

### Capturing a Variable Declared Outside the Loop

**Wrong -- complete program:**

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	endpoints := []string{"auth", "payments", "orders", "users"}
	var wg sync.WaitGroup

	var current string // one variable, shared by every goroutine
	for _, ep := range endpoints {
		current = ep
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Println(current) // reads whatever main last wrote
		}()
	}
	wg.Wait()
}
```

**What happens:** All goroutines read the same `current`, so they almost always print `users` -- the last endpoint. It is also a data race: `go run -race` reports a concurrent read and write of `current`. In a real health checker, every goroutine would check only the last endpoint, leaving the others unmonitored.

Note that the loop variable `ep` itself is *not* the problem. Since Go 1.22 each iteration gets a fresh `ep` (and a fresh `i` in `for i := 0; ...`), so capturing it directly is safe. Only variables declared outside the loop are shared.

**Correct -- complete program:**

```go
package main

import (
	"fmt"
	"sync"
)

func printEndpoint(endpoint string, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Println(endpoint)
}

func main() {
	endpoints := []string{"auth", "payments", "orders", "users"}
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		wg.Add(1)
		go printEndpoint(ep, &wg)
	}
	wg.Wait()
}
```

### Forgetting to Wait for Goroutines

**Wrong -- complete program:**

```go
package main

import "fmt"

func main() {
	go fmt.Println("health check complete")
	// main exits immediately -- goroutine never runs
}
```

**What happens:** The program exits before the goroutine has a chance to execute. In a CI/CD pipeline, your health check reports success without actually checking anything.

**Correct -- complete program:**

```go
package main

import (
	"fmt"
	"sync"
)

func reportHealthCheck(wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Println("health check complete")
}

func main() {
	var wg sync.WaitGroup
	wg.Add(1)
	go reportHealthCheck(&wg)
	wg.Wait()
}
```

### Trying to Get a Return Value from `go`

**Wrong:**

```go
go result := checkHealth("auth-api") // syntax error: go does not return values
```

**What happens:** Compilation error. The `go` keyword starts a function call concurrently; it cannot capture return values.

**Correct -- use a channel:**

```go
package main

import "fmt"

func checkHealth(service string) string {
	return service + ": UP"
}

func main() {
	ch := make(chan string)
	go func() {
		ch <- checkHealth("auth-api")
	}()
	result := <-ch
	fmt.Println(result) // auth-api: UP
}
```

## Review

The `go` keyword launches a function call as an independent goroutine and
returns immediately -- it never blocks and never yields a return value, which
is why results come back over a channel and completion comes back through a
`sync.WaitGroup` rather than through the call itself. Because `main` is just
another goroutine, its return kills every sibling still running, so `wg.Wait()`
(never `time.Sleep`) is what stands between a working fan-out and a checker that
silently reports nothing. The payoff of getting this right is the fan-out
pattern: one goroutine per independent check turns wall-clock time from the sum
of the latencies into the maximum single latency. The one invariant that keeps
it safe is data ownership -- each goroutine must operate on its own copy, which
since Go 1.22 the loop variable gives you for free, but a variable declared
outside the loop still does not.

You should now be able to build a multi-region health checker without
re-reading any of this: six services across three regions, eighteen goroutines
launched one per service-region pair, each simulating a check with random
latency and a random pass or fail, every result sent on a buffered channel
sized to hold all eighteen so no sender blocks, and the main goroutine
collecting exactly eighteen and grouping them by region to report the fastest
and slowest in each. If you can say why the channel must be buffered, why
execution order is non-deterministic, and why passing the region string as an
argument matters more than it looks, the exercise has done its job.

## Resources

- [Go Tour: Goroutines](https://go.dev/tour/concurrency/1) -- the one-slide introduction to the `go` keyword with a live example.
- [Effective Go: Goroutines](https://go.dev/doc/effective_go#goroutines) -- how goroutines are multiplexed onto threads and why they are cheap.
- [Go Spec: Go Statements](https://go.dev/ref/spec#Go_statements) -- the exact rules for what `go` accepts and when its arguments are evaluated.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- Add/Done/Wait semantics for waiting on a set of goroutines.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-goroutine-vs-os-thread](../02-goroutine-vs-os-thread/02-goroutine-vs-os-thread.md)