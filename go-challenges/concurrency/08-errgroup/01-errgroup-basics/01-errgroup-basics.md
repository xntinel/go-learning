# Exercise 1: Errgroup Basics -- Deployment Health Checker

Before deploying a new version, you verify that every dependency is healthy:
the database is reachable, the cache responds, the queue accepts connections,
object storage is up. Those checks are independent, so running them in parallel
cuts the deployment gate from the sum of the checks down to the slowest single
one. The catch is that a bare `sync.WaitGroup` gives you synchronization but no
way to collect the errors -- and a health check whose failure never reaches the
caller is worse than no check at all, because it deploys onto a broken
dependency while reporting success.

## What you'll build

```text
01-errgroup-basics/
  main.go        a deployment health checker: sequential baseline, then the
                 manual errgroup (WaitGroup + sync.Once + first error)
```

- Build: a concurrent health checker that verifies multiple infrastructure services in parallel and gates the deploy on the first failure.
- Implement: `HealthChecker` with `RunSequential` and `RunParallel`, the errgroup pattern from scratch (`sync.WaitGroup` + `sync.Once` + an error variable), first-error semantics under multiple failures, and a reusable `RunAllParallel`.
- Verify: `go run main.go` for each step.

### Why WaitGroup alone cannot collect errors

When you launch goroutines with `sync.WaitGroup`, there is no built-in way to
collect errors. You need a WaitGroup for synchronization, a mutex or `sync.Once`
for thread-safe error capture, and an error variable to store the first failure.
This boilerplate repeats in every concurrent-with-errors scenario in your
codebase.

The "errgroup" pattern encapsulates all of this: launch goroutines that return
errors, wait for all of them, get back the first error. The
`golang.org/x/sync/errgroup` package provides this ready-made, but understanding
the underlying mechanics is more valuable than blindly importing a package.

## Step 1 -- Sequential Health Checks (The Baseline)

Start with the sequential version to understand the problem. Each service check takes time, and we run them one after another:

```go
package main

import (
	"fmt"
	"time"
)

type ServiceName string

const (
	Postgres ServiceName = "postgres"
	Redis    ServiceName = "redis"
	RabbitMQ ServiceName = "rabbitmq"
	S3       ServiceName = "s3"
)

type HealthChecker struct {
	services []ServiceName
}

func NewHealthChecker(services []ServiceName) *HealthChecker {
	return &HealthChecker{services: services}
}

func (hc *HealthChecker) RunSequential() error {
	for _, svc := range hc.services {
		if err := hc.checkHealth(svc); err != nil {
			return err
		}
		fmt.Printf("  OK: %s\n", svc)
	}
	return nil
}

func (hc *HealthChecker) checkHealth(service ServiceName) error {
	switch service {
	case Postgres:
		time.Sleep(120 * time.Millisecond) // simulates TCP connect + ping
		return nil
	case Redis:
		time.Sleep(30 * time.Millisecond)
		return nil
	case RabbitMQ:
		time.Sleep(80 * time.Millisecond)
		return fmt.Errorf("rabbitmq: connection refused on port 5672")
	case S3:
		time.Sleep(150 * time.Millisecond)
		return nil
	default:
		return fmt.Errorf("unknown service: %s", service)
	}
}

func main() {
	checker := NewHealthChecker([]ServiceName{Postgres, Redis, RabbitMQ, S3})

	fmt.Println("=== Sequential Health Check ===")
	start := time.Now()

	if err := checker.RunSequential(); err != nil {
		fmt.Printf("FAIL: %v\n", err)
	} else {
		fmt.Print("All services healthy. ")
	}
	fmt.Printf("Total time: %v\n", time.Since(start).Round(time.Millisecond))
}
```

**Expected output:**
```
=== Sequential Health Check ===
  OK: postgres
  OK: redis
FAIL: rabbitmq: connection refused on port 5672
Total time: 230ms
```

The sequential approach takes the sum of all check durations up to the first failure: 120 + 30 + 80 = 230ms. If all services were healthy, it would take 120 + 30 + 80 + 150 = 380ms. In production with 10+ services and network latency, this adds seconds to every deployment.

## Step 2 -- Parallel Health Checks with the Manual Errgroup Pattern

Now build the errgroup pattern from scratch. You need three standard library primitives working together:

1. `sync.WaitGroup` -- wait for all goroutines to finish
2. `sync.Once` -- capture only the first error (thread-safe)
3. An `error` variable -- store the captured error

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type ServiceName string

const (
	Postgres ServiceName = "postgres"
	Redis    ServiceName = "redis"
	RabbitMQ ServiceName = "rabbitmq"
	S3       ServiceName = "s3"
)

type HealthChecker struct {
	services []ServiceName
}

func NewHealthChecker(services []ServiceName) *HealthChecker {
	return &HealthChecker{services: services}
}

func (hc *HealthChecker) RunParallel() error {
	start := time.Now()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, svc := range hc.services {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := hc.checkHealth(svc); err != nil {
				once.Do(func() { firstErr = err })
				return
			}
			fmt.Printf("  OK: %s (%v)\n", svc, time.Since(start).Round(time.Millisecond))
		}()
	}

	wg.Wait()
	return firstErr
}

func (hc *HealthChecker) checkHealth(service ServiceName) error {
	switch service {
	case Postgres:
		time.Sleep(120 * time.Millisecond)
		return nil
	case Redis:
		time.Sleep(30 * time.Millisecond)
		return nil
	case RabbitMQ:
		time.Sleep(80 * time.Millisecond)
		return fmt.Errorf("rabbitmq: connection refused on port 5672")
	case S3:
		time.Sleep(150 * time.Millisecond)
		return nil
	default:
		return fmt.Errorf("unknown service: %s", service)
	}
}

func main() {
	checker := NewHealthChecker([]ServiceName{Postgres, Redis, RabbitMQ, S3})

	fmt.Println("=== Parallel Health Check (manual errgroup) ===")
	start := time.Now()

	if err := checker.RunParallel(); err != nil {
		fmt.Printf("FAIL: %v\n", err)
		fmt.Printf("Total time: %v (all checks ran in parallel)\n", time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Printf("All services healthy. Total time: %v\n", time.Since(start).Round(time.Millisecond))
	}
}
```

**Expected output:**
```
=== Parallel Health Check (manual errgroup) ===
  OK: redis (30ms)
  OK: postgres (120ms)
  OK: s3 (150ms)
FAIL: rabbitmq: connection refused on port 5672
Total time: 150ms (all checks ran in parallel)
```

Total time is now 150ms (the slowest check), not 230ms (sum of checks up to failure). All four checks ran concurrently. The `sync.Once` ensures that only the first error is captured -- if both rabbitmq and another service failed, you still get exactly one error.

Notice the boilerplate: `WaitGroup` (Add, Done, Wait), `sync.Once`, error variable, goroutine closure with captured variable. Five moving parts that must be wired correctly every time.

## Step 3 -- First-Error Semantics with Multiple Failures

When multiple services fail, only the first error is kept. The "first" error depends on which goroutine completes first, which is determined by timing:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type ServiceName string

const (
	Postgres ServiceName = "postgres"
	Redis    ServiceName = "redis"
	RabbitMQ ServiceName = "rabbitmq"
	S3       ServiceName = "s3"
)

type HealthChecker struct {
	services []ServiceName
}

func NewHealthChecker(services []ServiceName) *HealthChecker {
	return &HealthChecker{services: services}
}

func (hc *HealthChecker) RunParallelAllFailing() error {
	start := time.Now()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, svc := range hc.services {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := hc.checkHealthMostFailing(svc); err != nil {
				once.Do(func() { firstErr = err })
				fmt.Printf("  FAIL: %s (%v)\n", svc, time.Since(start).Round(time.Millisecond))
				return
			}
			fmt.Printf("  OK: %s (%v)\n", svc, time.Since(start).Round(time.Millisecond))
		}()
	}

	wg.Wait()
	return firstErr
}

func (hc *HealthChecker) checkHealthMostFailing(service ServiceName) error {
	switch service {
	case Postgres:
		time.Sleep(120 * time.Millisecond)
		return fmt.Errorf("postgres: authentication failed")
	case Redis:
		time.Sleep(30 * time.Millisecond)
		return nil // redis is OK
	case RabbitMQ:
		time.Sleep(80 * time.Millisecond)
		return fmt.Errorf("rabbitmq: connection refused")
	case S3:
		time.Sleep(150 * time.Millisecond)
		return fmt.Errorf("s3: bucket not found")
	default:
		return fmt.Errorf("unknown service: %s", service)
	}
}

func main() {
	checker := NewHealthChecker([]ServiceName{Postgres, Redis, RabbitMQ, S3})

	fmt.Println("=== Multiple Failures ===")

	firstErr := checker.RunParallelAllFailing()
	fmt.Printf("\nWait returned: %v (only the first error is kept)\n", firstErr)
}
```

**Expected output:**
```
=== Multiple Failures ===
  OK: redis (30ms)
  FAIL: rabbitmq (80ms)
  FAIL: postgres (120ms)
  FAIL: s3 (150ms)

Wait returned: rabbitmq: connection refused (only the first error is kept)
```

Three services fail, but the error variable holds only `rabbitmq: connection refused` because rabbitmq fails at 80ms -- before postgres (120ms) and s3 (150ms). The `sync.Once` blocks the later errors from overwriting the first. If you need all errors, use a mutex-protected slice instead of `sync.Once`.

## Step 4 -- Reusable HealthChecker

Extract the pattern into a reusable function. This is essentially what `golang.org/x/sync/errgroup` does internally:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type HealthCheckFn func() error

type HealthCheck struct {
	Name string
	Fn   HealthCheckFn
}

type HealthChecker struct {
	checks []HealthCheck
}

func NewHealthChecker(checks []HealthCheck) *HealthChecker {
	return &HealthChecker{checks: checks}
}

func (hc *HealthChecker) RunAllParallel() error {
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, check := range hc.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := check.Fn(); err != nil {
				once.Do(func() { firstErr = err })
			}
		}()
	}

	wg.Wait()
	return firstErr
}

func simulateService(latency time.Duration, errMsg string) HealthCheckFn {
	return func() error {
		time.Sleep(latency)
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return nil
	}
}

func main() {
	checker := NewHealthChecker([]HealthCheck{
		{Name: "postgres", Fn: simulateService(120*time.Millisecond, "")},
		{Name: "redis", Fn: simulateService(30*time.Millisecond, "")},
		{Name: "rabbitmq", Fn: simulateService(80*time.Millisecond, "rabbitmq: connection refused on port 5672")},
		{Name: "s3", Fn: simulateService(150*time.Millisecond, "")},
	})

	fmt.Println("=== Reusable Health Checker ===")
	start := time.Now()

	if err := checker.RunAllParallel(); err != nil {
		fmt.Printf("Deployment blocked: %v\n", err)
	} else {
		fmt.Println("All services healthy -- deploy!")
	}

	fmt.Printf("Total time: %v\n", time.Since(start).Round(time.Millisecond))

	// The golang.org/x/sync/errgroup package provides exactly this pattern:
	//
	//   var g errgroup.Group
	//   for _, check := range checks {
	//       g.Go(check.Fn)
	//   }
	//   err := g.Wait()
	//
	// That is the entire implementation. g.Go() replaces wg.Add+go+defer wg.Done.
	// g.Wait() replaces wg.Wait and returns the first error.
	// No WaitGroup, no Once, no error variable -- one type does it all.
}
```

**Expected output:**
```
=== Reusable Health Checker ===
Deployment blocked: rabbitmq: connection refused on port 5672
Total time: 150ms
```

The `runChecksParallel` function is a minimal errgroup. The real `golang.org/x/sync/errgroup.Group` adds context integration (exercise 02), concurrency limits (exercise 03), and is battle-tested across thousands of Go services. But the core idea is exactly what you built here: WaitGroup + Once + first error.

## Verification

At this point, verify:
1. The sequential version takes ~230ms (sum of checks up to failure)
2. The parallel version takes ~150ms (max of all checks)
3. Multiple failures result in only the first error being returned
4. The reusable function produces identical behavior

## Common Mistakes

### Capturing a variable declared outside the loop

**Wrong:**
```go
var svc ServiceName
for _, s := range services {
    svc = s // reused across iterations -- not a fresh variable
    wg.Add(1)
    go func() {
        defer wg.Done()
        checkHealth(svc) // all goroutines race on the same svc
    }()
}
```

**What happens:** `svc` is declared once, outside the loop, so every goroutine closes over that single variable. By the time the goroutines run, the loop has advanced and they all observe whichever value `svc` holds last -- and they race on it while the loop writes to it. This is NOT true of the `for range` variable itself: since Go 1.22 each iteration gets a fresh copy of `s`, so closing over `s` directly is already safe. The bug only appears when you hoist a variable out of the loop body.

**Fix:** Close over the per-iteration `for range` variable, which is already scoped to the loop body:

```go
for _, s := range services {
    wg.Add(1)
    go func() {
        defer wg.Done()
        checkHealth(s) // fresh s each iteration since Go 1.22
    }()
}
```

### Using a mutex instead of sync.Once for first-error capture

**Not wrong, but unnecessary complexity:**
```go
mu.Lock()
if firstErr == nil {
    firstErr = err
}
mu.Unlock()
```

**Better:** `sync.Once` is purpose-built for "do this exactly once." It communicates intent more clearly and avoids the if-nil check.

### Swallowing errors inside the goroutine

**Wrong:**
```go
go func() {
    defer wg.Done()
    if err := checkHealth(svc); err != nil {
        log.Println(err) // logged but not propagated
    }
}()
```

**What happens:** The caller of `wg.Wait()` never knows a check failed. The deployment proceeds with a broken dependency. In production, this means deploying to a cluster where the database is unreachable.

## Review

The errgroup pattern is three standard-library pieces wired together: a
`sync.WaitGroup` waits for every goroutine, a `sync.Once` captures exactly one
error even when several checks fail at once, and an error variable holds that
first failure to return from `Wait`. Launch goroutines that return errors, wait
for all of them, get back the first -- and note what the pattern does not do: all
the checks still run to completion, because first-error capture is not
cancellation (that needs a context, which exercise 02 adds). Which error is
"first" is decided by timing, since `sync.Once` blocks every later write. This is
exactly what `golang.org/x/sync/errgroup.Group` packages as `g.Go` and `g.Wait`,
and having built it by hand you know precisely what those two methods replace.
Two habits matter: never swallow an error inside a goroutine -- propagate it so
`Wait` can see it -- and remember that since Go 1.22 a `for range` variable is
per-iteration, so closing over it directly is safe; only a variable hoisted
outside the loop needs a fresh copy.

To confirm it holds together, run the full program and check that sequential
checks take the sum of the durations while parallel checks take the max, that
`sync.Once` returns exactly one error when several services fail, that the
reusable checker behaves identically to the inline version, and that a run with
no failures returns nil.

## Resources

- [sync.WaitGroup documentation](https://pkg.go.dev/sync#WaitGroup) -- the Add/Done/Wait counter that synchronizes the parallel checks.
- [sync.Once documentation](https://pkg.go.dev/sync#Once) -- the "do exactly once" primitive that captures only the first error thread-safely.
- [errgroup package documentation](https://pkg.go.dev/golang.org/x/sync/errgroup) -- the production package that packages this exact pattern as `errgroup.Group`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the concurrency-with-errors background that motivates canceling siblings on first failure.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-errgroup-with-context](../02-errgroup-with-context/02-errgroup-with-context.md)
