# Exercise 1: Your First Data Race

A data race is what happens when two or more goroutines touch the same memory at
the same time, at least one of them writing, with no synchronization between
them. It is the number one concurrency bug in production Go, and it is insidious
precisely because the program usually looks correct -- right up until it
silently produces wrong numbers under load or on different hardware. The Go
memory model calls the result undefined behavior: once you have a race, the
compiler and runtime promise nothing about the outcome. This exercise reproduces
one on purpose, with a shared hit counter that loses a different number of
updates every single run.

## What you'll build

```text
01-your-first-data-race/
  main.go        a shared hit counter incremented by many goroutines,
                 losing updates every run
```

- Build: a concurrent web-hit counter that races on an unsynchronized `hitCount++`.
- Implement: `HitCounter` with a `CountHits` whose read-modify-write has no synchronization; a multi-run benchmark; and a `measureLoss` sweep across traffic levels.
- Verify: `go run main.go` (run it several times -- each run loses a different count)

### Why a lost update corrupts your numbers

A data race occurs when three conditions are ALL true simultaneously:

1. Two or more goroutines access the same memory location
2. At least one of the accesses is a write
3. There is no synchronization between the accesses

Data races are the **number one concurrency bug** in production Go code. They are insidious because the program may appear correct most of the time, then fail unpredictably under load or on different hardware.

The Go memory model explicitly states that a data race results in **undefined behavior**. The compiler and runtime make no guarantees about the outcome.

Consider a web application that tracks page hits. Every HTTP handler increments a shared counter. Under light traffic, the counter looks correct. Under production load with hundreds of concurrent requests, increments silently disappear. Your analytics dashboard shows 50,000 daily visitors when the real number is 80,000. Your billing system undercharges because it counted fewer API calls than actually occurred. Your alerting thresholds never trigger because the error counter is perpetually low.

This is not a hypothetical scenario. It is the direct consequence of an unprotected shared counter.

## Step 1 -- Build the Hit Counter

Create a file called `main.go`. This simulates a web server where multiple HTTP handlers increment a shared hit counter concurrently. Each goroutine represents a request handler processing incoming traffic:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	defaultHandlers       = 100
	defaultReqsPerHandler = 100
	benchmarkRuns         = 5
)

// HitCounter simulates a web server's page-view tracker.
// BUG: hitCount is shared across goroutines without synchronization.
type HitCounter struct {
	handlers       int
	reqsPerHandler int
}

func NewHitCounter(handlers, reqsPerHandler int) *HitCounter {
	return &HitCounter{
		handlers:       handlers,
		reqsPerHandler: reqsPerHandler,
	}
}

// CountHits launches concurrent handlers that all increment the same variable.
// DATA RACE: the read-modify-write on hitCount has no synchronization.
func (hc *HitCounter) CountHits() int {
	hitCount := 0
	var wg sync.WaitGroup

	for handler := 0; handler < hc.handlers; handler++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := 0; req < hc.reqsPerHandler; req++ {
				hitCount++ // DATA RACE: read-modify-write without synchronization
			}
		}()
	}

	wg.Wait()
	return hitCount
}

func (hc *HitCounter) Expected() int {
	return hc.handlers * hc.reqsPerHandler
}

func runBenchmark(counter *HitCounter) {
	expected := counter.Expected()
	results := make([]int, benchmarkRuns)

	for run := 0; run < benchmarkRuns; run++ {
		start := time.Now()
		actual := counter.CountHits()
		elapsed := time.Since(start)
		lost := expected - actual
		results[run] = actual
		fmt.Printf("Run %d: %d hits recorded, %d lost (%v)\n",
			run+1, actual, lost, elapsed)
	}

	fmt.Println()
	printProductionImpact(results)
}

func printProductionImpact(results []int) {
	fmt.Println("--- Production Impact ---")
	fmt.Printf("Results across %d runs: %v\n", len(results), results)
	fmt.Println("Every run produces a different number. None reach 10000.")
	fmt.Println()
	fmt.Println("If this were real:")
	fmt.Println("  - Analytics: dashboard shows 6000 visitors instead of 10000")
	fmt.Println("  - Billing:   customer charged for 7000 API calls instead of 10000")
	fmt.Println("  - Alerting:  error counter shows 50 errors instead of 80, threshold never triggers")
	fmt.Println("  - Capacity:  load balancer thinks server handles fewer requests than it does")
}

func main() {
	fmt.Println("=== Web Hit Counter Data Race ===")
	fmt.Println("Expected: 10000 hits (100 handlers x 100 requests each)")
	fmt.Println()

	counter := NewHitCounter(defaultHandlers, defaultReqsPerHandler)
	runBenchmark(counter)
}
```

## Step 2 -- Run and Observe

### Verification
```bash
go run main.go
```

Sample output (your numbers WILL differ):
```
=== Web Hit Counter Data Race ===
Expected: 10000 hits (100 handlers x 100 requests each)

Run 1: 6482 hits recorded, 3518 lost (1.2ms)
Run 2: 7201 hits recorded, 2799 lost (1.1ms)
Run 3: 5893 hits recorded, 4107 lost (1.3ms)
Run 4: 6819 hits recorded, 3181 lost (1.1ms)
Run 5: 7044 hits recorded, 2956 lost (1.2ms)

--- Production Impact ---
Results across 5 runs: [6482 7201 5893 6819 7044]
Every run produces a different number. None reach 10000.
```

Run it several times. Each execution produces different results. This non-determinism is the unmistakable signature of a data race.

## Step 3 -- Understand Why Increments Are Lost

The operation `hitCount++` is **not atomic**. It consists of three CPU-level steps:
1. **READ** the current value of hitCount from memory
2. **ADD** one to the value
3. **WRITE** the new value back to memory

When two request handlers execute simultaneously:

```
Time    Handler A              Handler B              hitCount (memory)
----    ---------              ---------              -----------------
 1      READ hitCount (= 42)                          42
 2                             READ hitCount (= 42)   42
 3      WRITE hitCount (= 43)                         43
 4                             WRITE hitCount (= 43)  43  <-- increment LOST!
```

Both handlers read 42, both compute 43, both write 43. Two requests were processed, but the counter only went up by one. This is called a **lost update**. With 100 goroutines competing, thousands of increments vanish per second.

## Step 4 -- Measure How Bad It Gets

Add this function to see the relationship between concurrency level and data loss:

```go
package main

import (
	"fmt"
	"sync"
)

// TrafficScenario describes a concurrency level for benchmarking data loss.
type TrafficScenario struct {
	Handlers       int
	ReqsPerHandler int
	Label          string
}

// HitCounter simulates a web server's page-view tracker.
// BUG: hitCount is shared across goroutines without synchronization.
type HitCounter struct {
	handlers       int
	reqsPerHandler int
}

func NewHitCounter(handlers, reqsPerHandler int) *HitCounter {
	return &HitCounter{
		handlers:       handlers,
		reqsPerHandler: reqsPerHandler,
	}
}

func (hc *HitCounter) Expected() int {
	return hc.handlers * hc.reqsPerHandler
}

// CountHits launches concurrent handlers that all increment the same variable.
// DATA RACE: the read-modify-write on hitCount has no synchronization.
func (hc *HitCounter) CountHits() int {
	hitCount := 0
	var wg sync.WaitGroup

	for h := 0; h < hc.handlers; h++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < hc.reqsPerHandler; r++ {
				hitCount++
			}
		}()
	}

	wg.Wait()
	return hitCount
}

func measureLoss(scenario TrafficScenario) {
	counter := NewHitCounter(scenario.Handlers, scenario.ReqsPerHandler)
	expected := counter.Expected()
	actual := counter.CountHits()
	lossPercent := float64(expected-actual) / float64(expected) * 100
	fmt.Printf("%-35s expected=%d actual=%d lost=%.1f%%\n",
		scenario.Label, expected, actual, lossPercent)
}

func main() {
	fmt.Println("=== Data Loss vs Concurrency Level ===")
	fmt.Println()

	scenarios := []TrafficScenario{
		{10, 1000, "Light traffic (10 handlers)"},
		{50, 1000, "Moderate traffic (50 handlers)"},
		{200, 1000, "Heavy traffic (200 handlers)"},
		{500, 1000, "Peak traffic (500 handlers)"},
	}

	for _, s := range scenarios {
		measureLoss(s)
	}

	fmt.Println()
	fmt.Println("More concurrency = more lost updates = worse data corruption")
}
```

### Verification
```bash
go run main.go
```

More goroutines means more contention, which means more lost updates. Under peak traffic (when accuracy matters most), the data is least reliable.

## Common Mistakes

### Thinking "It Worked Once, So It's Fine"
A data race may produce the correct result on some runs, especially on single-core machines or with few goroutines. The absence of symptoms does NOT prove the absence of the bug. Data races are undefined behavior: they must be eliminated, not tolerated.

### Assuming Small Operations Are Atomic
Even `hitCount++` (or `hitCount += 1`) is NOT atomic in Go. It compiles to multiple machine instructions. Only operations from the `sync/atomic` package are guaranteed to be atomic (see exercise 05).

### Using time.Sleep as Synchronization
Sleeping does not synchronize memory. Even if you sleep "long enough," the compiler and CPU may reorder memory operations. Only proper synchronization primitives (`sync.Mutex`, channels, `sync/atomic`) establish happens-before relationships.

## Review

The bug lives in a single line. `hitCount++` looks atomic but compiles to three
separate steps -- read the current value, add one, write it back -- and when two
goroutines interleave those steps, both read the same starting value, both
compute the same successor, and both write it, so two increments collapse into
one. That is a lost update, and with a hundred goroutines competing it happens
thousands of times per run. The visible symptom is non-determinism: every
execution lands on a different, always-too-low total, and none reach the
expected count. The consequence in a real service is not an abstraction -- it is
undercounted analytics, undercharged billing, alerts that never fire, and
capacity numbers you cannot trust. Worse, the loss grows with concurrency, so
the data is least reliable exactly under the peak load where accuracy matters
most.

Make sure you can answer the questions the exercise is built around. What are the
three conditions that must all hold for a data race to exist? Why does
`hitCount++` produce wrong results across goroutines when it looks like one
operation? And the subtle one: if a run happens to print 10000, does that prove
the code is race-free? It does not -- a race is undefined behavior that can
produce the right answer by luck, which is why the absence of symptoms is never
proof of correctness.

## Resources
- [Go Memory Model](https://go.dev/ref/mem) -- why unsynchronized concurrent access is undefined behavior.
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) -- the tool the next exercise uses to catch this bug automatically.
- [Go Spec: Statements](https://go.dev/ref/spec#Statements) -- how an assignment like `x++` is defined, and why it is not a single atomic step.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-race-detector-flag](../02-race-detector-flag/02-race-detector-flag.md)
