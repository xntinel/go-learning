# Exercise 6: Cooperative Scheduling

Go's scheduler is cooperative first: a goroutine gives up its processor only at well-defined points -- channel operations, mutex locks, system calls, `time.Sleep`, and function calls where the runtime checks the stack. At each of those the scheduler can switch to someone else. The failure mode is what happens between them: a goroutine that runs a tight computational loop with no such points can hold its P and starve every other goroutine sharing it, so a runaway calculation blinds your health checks and heartbeats while the process still looks alive. This exercise makes those yield points visible on a single core and shows the tools -- `runtime.Gosched()` and the runtime's own preemption -- that keep one job from monopolizing the rest.

## What you'll build

```text
06-cooperative-scheduling/
  main.go        four experiments on one shared P: natural scheduling points,
                 Gosched, async preemption, and a fairness comparison
```

- Build: a single-core task scheduler that shows when and how goroutines yield a shared P.
- Implement: `runMetricsCollector`/`runLogShipper` driven by natural scheduling points, `runPrimeGenerator` that calls `runtime.Gosched()`, a tight-loop worker the runtime preempts on its own, and a fairness experiment comparing tight loops, Gosched, and channel handoff.
- Verify: `go run main.go`

### Why async preemption did not make yielding obsolete

Before Go 1.14, a goroutine running a tight computational loop with no function calls and no scheduling points could monopolize a P indefinitely, starving other goroutines. Go 1.14 introduced asynchronous preemption using OS signals (SIGURG on Unix): the runtime periodically sends a signal to running goroutines, forcing them to yield even in tight loops.

Understanding scheduling behavior still helps you write code that plays well with the scheduler. While async preemption prevents outright starvation, cooperative yielding through natural scheduling points leads to smoother, more predictable concurrent behavior. In rare cases, you may need `runtime.Gosched()` to explicitly yield.

## Step 1 -- Task Scheduler: Natural Scheduling Points

Imagine you are building a task scheduler that runs multiple background jobs on a single core. Some jobs do IO (logging, metrics collection), and some do CPU-intensive work (computing prime numbers for cryptographic key generation). With GOMAXPROCS=1, we can see exactly when the scheduler switches between them.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
)

const iterationsPerJob = 5

func runMetricsCollector(wg *sync.WaitGroup) {
	defer wg.Done()
	ch := make(chan int, 1)
	for i := 0; i < iterationsPerJob; i++ {
		ch <- i  // scheduling point: channel send
		_ = <-ch // scheduling point: channel receive
		fmt.Printf("metrics-collector:%d ", i)
	}
}

func runLogShipper(wg *sync.WaitGroup) {
	defer wg.Done()
	for i := 0; i < iterationsPerJob; i++ {
		fmt.Printf("log-shipper:%d ", i) // scheduling point: I/O syscall
	}
}

func main() {
	runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(runtime.NumCPU())

	var wg sync.WaitGroup
	wg.Add(2)

	go runMetricsCollector(&wg)
	go runLogShipper(&wg)

	wg.Wait()
	fmt.Println()
}
```

**What's happening here:** With GOMAXPROCS=1, only one goroutine can run at a time. Both jobs hit scheduling points (channel ops for the metrics collector, I/O for the log shipper), giving the scheduler opportunities to switch between them. The output will be interleaved.

**Key insight:** Natural scheduling points include: channel send/receive, mutex lock/unlock, `time.Sleep`, system calls (I/O), function calls with stack checks, and memory allocation. At each of these, the scheduler can context-switch. In a real task scheduler, this means IO-heavy jobs naturally yield to others without any explicit coordination.

**What would happen if the metrics collector used a tight loop instead of channels?** Before Go 1.14, it would monopolize the P and the log shipper would never run. After Go 1.14, async preemption would eventually force it to yield, but with much worse fairness than channel-based yielding.

### Verification
```bash
go run main.go
```
Expected output (interleaved, exact order varies):
```
log-shipper:0 log-shipper:1 metrics-collector:0 log-shipper:2 metrics-collector:1 log-shipper:3 metrics-collector:2 log-shipper:4 metrics-collector:3 metrics-collector:4
```

## Step 2 -- CPU-Heavy Job Starving Others: runtime.Gosched()

When one job computes prime numbers for key generation (CPU-intensive, no natural scheduling points), it can starve other jobs. `runtime.Gosched()` explicitly yields control so the scheduler can run other goroutines.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

const (
	primeSearchLimit   = 100_000
	goSchedInterval    = 1000
	healthCheckTicks   = 5
	healthCheckPeriod  = 10 * time.Millisecond
)

func isPrime(n int) bool {
	if n < 2 {
		return false
	}
	for i := 2; i*i <= n; i++ {
		if n%i == 0 {
			return false
		}
	}
	return true
}

func runPrimeGenerator(wg *sync.WaitGroup, yieldEveryN int) {
	defer wg.Done()
	primes := 0
	for n := 2; n < primeSearchLimit; n++ {
		if isPrime(n) {
			primes++
		}
		if yieldEveryN > 0 && n%yieldEveryN == 0 {
			runtime.Gosched()
		}
	}
	fmt.Printf("  prime-generator: found %d primes\n", primes)
}

func runPeriodicHealthCheck(wg *sync.WaitGroup, start time.Time) int {
	defer wg.Done()
	completed := 0
	for i := 0; i < healthCheckTicks; i++ {
		time.Sleep(healthCheckPeriod)
		completed++
		fmt.Printf("  health-check: tick %d (%v)\n", i, time.Since(start).Round(time.Millisecond))
	}
	return completed
}

func runExperiment(label string, yieldInterval int) {
	fmt.Printf("--- %s ---\n", label)

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)

	go runPrimeGenerator(&wg, yieldInterval)

	healthChecks := 0
	go func() {
		healthChecks = runPeriodicHealthCheck(&wg, start)
	}()

	wg.Wait()
	fmt.Printf("  Health checks completed: %d/%d | Total: %v\n\n",
		healthChecks, healthCheckTicks, time.Since(start).Round(time.Millisecond))
}

func main() {
	runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(runtime.NumCPU())

	runExperiment("WITHOUT Gosched", 0)
	runExperiment("WITH Gosched", goSchedInterval)
}
```

**What's happening here:** Without Gosched, the prime generator dominates the single P. Health checks rely on `time.Sleep` (which is a scheduling point), but the prime generator runs without natural scheduling points between primes. With Gosched every 1000 iterations, the prime generator periodically yields, giving the health checker regular access to the P.

**Key insight:** Gosched puts the current goroutine at the back of the queue, but the scheduler MAY not pick the goroutine you expect next. It is NOT a synchronization primitive. However, for CPU-intensive background tasks that share a processor with latency-sensitive work (health checks, heartbeats), periodic Gosched calls dramatically improve fairness.

**What would happen with GOMAXPROCS=NumCPU?** Both jobs could run on separate Ps simultaneously, so Gosched would have no visible effect. The problem only manifests when goroutines share a P.

### Verification
```bash
go run main.go
```
Expected output:
```
--- WITHOUT Gosched ---
  health-check: tick 0 (10ms)
  health-check: tick 1 (20ms)
  ...
  prime-generator: found 9592 primes
  Health checks completed: 5/5 | Total: 85ms

--- WITH Gosched ---
  health-check: tick 0 (10ms)
  health-check: tick 1 (20ms)
  ...
  prime-generator: found 9592 primes
  Health checks completed: 5/5 | Total: 90ms
```

## Step 3 -- Async Preemption (Go 1.14+): No More Starvation

Demonstrate that even a tight CPU loop without scheduling points gets preempted. Before Go 1.14, this pattern would cause complete starvation of other goroutines.

```go
package main

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
)

const (
	monitorTicks       = 10
	monitorInterval    = 10 * time.Millisecond
)

func runTightCPULoop(counter *int64, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
			atomic.AddInt64(counter, 1)
		}
	}
}

func runPeriodicMonitor(counter *int64) bool {
	successes := 0
	for i := 0; i < monitorTicks; i++ {
		time.Sleep(monitorInterval)
		successes++
		fmt.Printf("  monitor tick %d (computations so far: %d)\n",
			i, atomic.LoadInt64(counter))
	}
	return successes == monitorTicks
}

func printPreemptionReport(allTicksCompleted bool) {
	if !allTicksCompleted {
		return
	}
	fmt.Println()
	fmt.Println("  All 10 monitoring ticks completed on schedule.")
	fmt.Println("  Async preemption (Go 1.14+) prevented the CPU-heavy")
	fmt.Println("  goroutine from starving the monitor.")
	fmt.Println()
	fmt.Println("  Before Go 1.14, the monitor would NEVER run. The CPU loop")
	fmt.Println("  would hold the P forever, and your monitoring would be blind.")
}

func main() {
	runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(runtime.NumCPU())

	var counter int64
	done := make(chan struct{})

	go runTightCPULoop(&counter, done)

	completed := make(chan bool)
	go func() {
		completed <- runPeriodicMonitor(&counter)
	}()

	success := <-completed
	close(done)

	printPreemptionReport(success)
}
```

**What's happening here:** Goroutine A runs a tight loop with no channel ops, no I/O, no function calls that trigger stack checks. Despite this, goroutine B still gets to run because the runtime sends a SIGURG signal to force A to yield at safe points.

**Key insight:** Go 1.14+ uses OS signals to asynchronously preempt long-running goroutines. The runtime periodically (every ~10ms) checks if a goroutine has been running too long and sends a signal to interrupt it. In production, this prevents a single runaway computation from killing your service's health checks, metrics collection, or heartbeats.

**What would happen on Go 1.13?** Goroutine A would monopolize the P indefinitely. The monitor would never get scheduled. In production, this means your service appears healthy from the outside (process is running) but is actually unresponsive (no goroutine can make progress).

### Verification
```bash
go run main.go
```
Expected output:
```
  monitor tick 0 (computations so far: 12345678)
  monitor tick 1 (computations so far: 24567890)
  ...
  monitor tick 9 (computations so far: 98765432)

  All 10 monitoring ticks completed on schedule.
  Async preemption (Go 1.14+) prevented the CPU-heavy
  goroutine from starving the monitor.

  Before Go 1.14, the monitor would NEVER run. The CPU loop
  would hold the P forever, and your monitoring would be blind.
```

## Step 4 -- Scheduling Fairness: Comparing Patterns for Worker Jobs

Build a comparison that measures how different scheduling patterns affect fairness across 3 worker jobs competing for one P. This simulates a scenario where your task scheduler must share CPU time fairly between background jobs.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	workerCount        = 3
	experimentDuration = 100 * time.Millisecond
	goSchedFrequency   = 1000
)

type WorkerFunc func(id int, counter *int64, stop <-chan struct{})

type FairnessResult struct {
	Name    string
	Counts  [workerCount]int64
}

func runFairnessExperiment(name string, workFn WorkerFunc) FairnessResult {
	var counters [workerCount]int64
	stop := make(chan struct{})

	for i := 0; i < workerCount; i++ {
		go workFn(i, &counters[i], stop)
	}

	time.Sleep(experimentDuration)
	close(stop)
	time.Sleep(10 * time.Millisecond)

	return FairnessResult{Name: name, Counts: counters}
}

func tightLoopWorker(_ int, counter *int64, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			atomic.AddInt64(counter, 1)
		}
	}
}

func goSchedWorker(_ int, counter *int64, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			atomic.AddInt64(counter, 1)
			if atomic.LoadInt64(counter)%goSchedFrequency == 0 {
				runtime.Gosched()
			}
		}
	}
}

func channelYieldWorker(_ int, counter *int64, stop <-chan struct{}) {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	for {
		select {
		case <-stop:
			return
		default:
			<-ch
			atomic.AddInt64(counter, 1)
			ch <- struct{}{}
		}
	}
}

func calculateFairness(counts [workerCount]int64) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	avg := float64(total) / float64(workerCount)
	variance := 0.0
	for _, c := range counts {
		diff := float64(c) - avg
		variance += diff * diff
	}
	return 1.0 - (variance / (avg * avg * float64(workerCount)))
}

func printFairnessTable(results []FairnessResult) {
	fmt.Printf("=== Scheduling Fairness: %d Workers Sharing 1 P for %v ===\n", workerCount, experimentDuration)
	fmt.Println()
	fmt.Printf("%-30s %12s %12s %12s %10s\n", "Strategy", "Worker 0", "Worker 1", "Worker 2", "Fairness")
	fmt.Println(strings.Repeat("-", 80))

	for _, r := range results {
		fairness := calculateFairness(r.Counts)
		fmt.Printf("%-30s %12d %12d %12d %10.3f\n",
			r.Name, r.Counts[0], r.Counts[1], r.Counts[2], fairness)
	}

	fmt.Println()
	fmt.Println("Fairness 1.000 = perfectly equal work distribution.")
	fmt.Println("Tight loops: worst fairness (one worker hogs the P for ~10ms at a time)")
	fmt.Println("Channel ops: best fairness but lowest throughput (context switch overhead)")
	fmt.Println("Gosched:     good balance of fairness and throughput")
}

func main() {
	runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(runtime.NumCPU())

	results := []FairnessResult{
		runFairnessExperiment("tight loop (preemption only)", tightLoopWorker),
		runFairnessExperiment("Gosched every 1K iters", goSchedWorker),
		runFairnessExperiment("channel yield per iter", channelYieldWorker),
	}

	printFairnessTable(results)
}
```

**What's happening here:** Three goroutines compete for one P (GOMAXPROCS=1). Each increments its own counter as fast as possible. We measure how evenly work distributes across the three workers after 100ms.

**Key insight:** Tight loops have the worst fairness because the scheduler can only preempt at ~10ms intervals. Gosched every 1000 iterations improves fairness by yielding more frequently. Channel operations yield on every iteration, giving the best fairness but lower total throughput. For a real task scheduler, the Gosched approach gives you the best balance: high throughput with acceptable fairness.

**What would happen with GOMAXPROCS=3?** All three workers could run simultaneously on separate Ps, and fairness would be perfect regardless of strategy. Scheduling patterns only matter when goroutines share Ps -- which happens when you have more goroutines than cores.

### Verification
```bash
go run main.go
```
Expected output pattern:
```
=== Scheduling Fairness: 3 Workers Sharing 1 P for 100ms ===

Strategy                         Worker 0     Worker 1     Worker 2   Fairness
--------------------------------------------------------------------------------
tight loop (preemption only)     45000000      1200000      1100000      0.421
Gosched every 1K iters            8500000      8200000      8300000      0.998
channel yield per iter              350000       340000       345000      0.999

Fairness 1.000 = perfectly equal work distribution.
Tight loops: worst fairness (one worker hogs the P for ~10ms at a time)
Channel ops: best fairness but lowest throughput (context switch overhead)
Gosched:     good balance of fairness and throughput
```

## Common Mistakes

### Relying on Gosched for Synchronization

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"runtime"
)

func main() {
	var data int

	go func() {
		data = 42
		runtime.Gosched() // does NOT guarantee main sees data=42
	}()

	runtime.Gosched()
	fmt.Println(data) // DATA RACE! May print 0 or 42
}
```

**What happens:** Gosched yields the processor but provides no memory ordering guarantees. This is a data race. In production, this creates intermittent bugs that are nearly impossible to reproduce.

**Correct -- use a channel:**
```go
package main

import "fmt"

func main() {
	ch := make(chan int)

	go func() {
		ch <- 42 // channel send provides happens-before guarantee
	}()

	data := <-ch // guaranteed to see 42
	fmt.Println(data)
}
```

### Adding Gosched Everywhere for "Performance"

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"runtime"
)

func main() {
	for i := 0; i < 1000; i++ {
		fmt.Println(i)
		runtime.Gosched() // unnecessary: fmt.Println already yields (I/O syscall)
	}
}
```

**What happens:** Each Gosched introduces a context switch. In this code, fmt.Println already yields because it does I/O. Adding Gosched doubles the context switch overhead for no benefit.

**Fix:** Only use Gosched when you have a measured scheduling problem. In nearly all code, natural scheduling points (I/O, channel ops) provide sufficient yielding.

### Assuming Cooperative Scheduling Means Deterministic Order

**Wrong thinking:** "If I call Gosched after every iteration, goroutines will alternate perfectly."

**What happens:** Gosched puts the goroutine at the back of the run queue, but the scheduler may not pick the goroutine you expect next. There could be other goroutines in the queue (GC workers, runtime goroutines).

**Fix:** Never rely on execution order. Use explicit synchronization (channels, mutexes) when order matters.

## Review

Go's scheduler leans on cooperation: goroutines yield at channel operations, syscalls, mutex locks, and function calls, and `runtime.Gosched()` lets a goroutine volunteer a yield by placing itself at the back of the run queue. What Go 1.14 added on top is async preemption -- the runtime sends SIGURG to a goroutine that has run too long -- so a tight loop can no longer starve its neighbors outright. But preemption fires only about every 10ms, so cooperative yielding through natural points still produces smoother, fairer behavior; the fairness experiment makes the gap concrete, with tight loops scoring worst and channel handoff best. The one thing `Gosched` is not is a synchronization primitive: it orders nothing in memory, so it can never stand in for a channel or a mutex, and with GOMAXPROCS above one the question often disappears because workers run on separate Ps.

To prove the model has landed, build a small task scheduler from scratch: four CPU-intensive background jobs pinned to `GOMAXPROCS=1`, run under three strategies -- no yielding, `Gosched` every N iterations, and a channel ticker -- and measure how evenly the iteration counts land across the jobs. Sweep the `Gosched` frequency across 100, 1000, and 10000 iterations to find where fairness and throughput trade off, and decide which strategy you would actually reach for when latency-sensitive health checks have to share a core with that background work. If you can predict the shape of those results before running them, you understand cooperative scheduling.

## Resources
- [runtime.Gosched](https://pkg.go.dev/runtime#Gosched) -- the one-call API for volunteering a yield, and the guarantees it does (and does not) make.
- [Go 1.14 Release Notes: Goroutine preemption](https://go.dev/doc/go1.14#runtime) -- the release that introduced signal-based async preemption of tight loops.
- [Proposal: Non-cooperative Goroutine Preemption](https://github.com/golang/proposal/blob/master/design/24543-non-cooperative-preemption.md) -- the design document behind that change, with the safe-point mechanics.

---

Back to [Concurrency](../../concurrency.md) | Next: [07-goroutine-per-request](../07-goroutine-per-request/07-goroutine-per-request.md)
