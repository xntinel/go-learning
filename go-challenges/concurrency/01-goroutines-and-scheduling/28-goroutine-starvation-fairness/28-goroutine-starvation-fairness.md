# Exercise 28: Goroutine Starvation and Fairness

Go's scheduler is cooperative at the goroutine level, with preemption at
function-call boundaries and, since Go 1.14, asynchronous preemption via
signals. Even so, a tight computational loop with no function calls can dominate
a processor for far longer than you expect. In a service that mixes CPU-heavy
work (parsing, aggregation, encryption) with latency-sensitive work (health
checks, API responses, heartbeats), the heavy goroutines can push the fast ones'
tail latency to unacceptable levels -- not through a deadlock, but through
starvation, where a latency-sensitive goroutine sits in the run queue while
every processor is busy grinding a batch loop. This exercise measures that
effect and then dials it back down.

## What you'll build

```text
28-goroutine-starvation-fairness/
  main.go        measure latency under CPU-heavy load, then mitigate with
                 runtime.Gosched() and by varying GOMAXPROCS
```

- Build: three measurement programs -- a starvation demo, a `Gosched` mitigation A/B test, and a `GOMAXPROCS` sweep.
- Implement: a `measureLatency` probe over `time.Sleep`, tight `cpuHeavyWork` loops with and without `runtime.Gosched()` yield points, and an experiment harness that varies `runtime.GOMAXPROCS`.
- Verify: `go run main.go` (numbers vary by machine; the direction is the lesson)

### Why the scheduler cannot fully save you

This is not a theoretical problem. A real-world example: a Go service processes
CSV batch imports (CPU-heavy parsing loops) and serves REST API requests
(latency-sensitive). During batch imports, API response times spike from 2ms to
200ms. The goroutines are not deadlocked -- they are starved. The batch
processing goroutines occupy the processors, and the API handler goroutines wait
in the run queue.

Understanding starvation requires understanding what the scheduler can and
cannot do. The scheduler can preempt goroutines at function call sites and
(since Go 1.14) via asynchronous signals, but tight loops doing inline
computation can still hold a processor for longer than expected. The mitigation
strategies -- `runtime.Gosched()`, adjusting `GOMAXPROCS`, or redesigning the
computation to have natural yield points -- are choices with trade-offs that
every Go developer working on mixed workloads must understand.


## Step 1 -- Demonstrate Starvation

Create a baseline measurement of latency for a fast goroutine, then introduce CPU-heavy goroutines and observe the latency increase.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

const (
	numMeasurements = 20
	measureInterval = 10 * time.Millisecond
)

func measureLatency(id string, stop <-chan struct{}, wg *sync.WaitGroup, results chan<- time.Duration) {
	defer wg.Done()

	for i := 0; i < numMeasurements; i++ {
		select {
		case <-stop:
			return
		default:
		}

		start := time.Now()
		time.Sleep(measureInterval)
		actual := time.Since(start)
		overhead := actual - measureInterval
		results <- overhead

		// small gap between measurements
		time.Sleep(1 * time.Millisecond)
	}
}

func cpuHeavyWork(stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		default:
		}
		// tight computational loop -- no function calls, no channel ops
		sum := 0
		for i := 0; i < 10_000_000; i++ {
			sum += i * i
		}
		_ = sum
	}
}

func collectResults(results <-chan time.Duration, count int) (minD, maxD, avgD time.Duration) {
	minD = time.Hour
	var total time.Duration
	collected := 0

	for collected < count {
		d := <-results
		total += d
		if d < minD {
			minD = d
		}
		if d > maxD {
			maxD = d
		}
		collected++
	}
	avgD = total / time.Duration(count)
	return
}

func main() {
	procs := runtime.GOMAXPROCS(0)
	fmt.Printf("=== Goroutine Starvation Demo ===\n")
	fmt.Printf("  GOMAXPROCS: %d\n\n", procs)

	// Baseline: measure latency without CPU-heavy goroutines
	fmt.Println("--- Baseline (no CPU-heavy goroutines) ---")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	results := make(chan time.Duration, numMeasurements)

	wg.Add(1)
	go measureLatency("baseline", stop, &wg, results)
	wg.Wait()

	minB, maxB, avgB := collectResults(results, numMeasurements)
	fmt.Printf("  Latency overhead: min=%v avg=%v max=%v\n\n", minB.Round(time.Microsecond), avgB.Round(time.Microsecond), maxB.Round(time.Microsecond))

	// With CPU-heavy goroutines
	numHeavy := procs * 2
	fmt.Printf("--- With %d CPU-heavy goroutines ---\n", numHeavy)

	stop2 := make(chan struct{})
	results2 := make(chan time.Duration, numMeasurements)
	var wgHeavy sync.WaitGroup
	var wgMeasure sync.WaitGroup

	for i := 0; i < numHeavy; i++ {
		wgHeavy.Add(1)
		go cpuHeavyWork(stop2, &wgHeavy)
	}

	time.Sleep(50 * time.Millisecond) // let CPU goroutines saturate

	wgMeasure.Add(1)
	go measureLatency("starved", stop2, &wgMeasure, results2)
	wgMeasure.Wait()

	close(stop2)
	wgHeavy.Wait()

	minS, maxS, avgS := collectResults(results2, numMeasurements)
	fmt.Printf("  Latency overhead: min=%v avg=%v max=%v\n", minS.Round(time.Microsecond), avgS.Round(time.Microsecond), maxS.Round(time.Microsecond))

	if avgS > avgB*2 {
		fmt.Printf("\n  Starvation detected: avg overhead increased %.1fx\n", float64(avgS)/float64(avgB))
	} else {
		fmt.Printf("\n  Minimal starvation (async preemption effective on this runtime)\n")
	}
}
```

**What's happening here:** The baseline measures how much overhead a `time.Sleep(10ms)` has when the system is idle -- this captures scheduler overhead alone. Then we launch `GOMAXPROCS * 2` CPU-heavy goroutines that run tight loops, saturating all processors. The measurement goroutine still calls `time.Sleep(10ms)`, but when it wakes up, it must wait for a processor to become available. The difference between baseline and starved overhead shows starvation impact.

**Key insight:** Since Go 1.14, asynchronous preemption means the scheduler can preempt goroutines even in tight loops. However, the overhead still increases because the scheduler must send signals and context-switch, which takes time. On some workloads, the max latency increases significantly even if the average is moderate. This P99 latency impact is what matters in production services.

### Verification
```bash
go run main.go
```
Expected output (values vary by machine):
```
=== Goroutine Starvation Demo ===
  GOMAXPROCS: 8

--- Baseline (no CPU-heavy goroutines) ---
  Latency overhead: min=52us avg=180us max=420us

--- With 16 CPU-heavy goroutines ---
  Latency overhead: min=200us avg=1.2ms max=4.5ms

  Starvation detected: avg overhead increased 6.7x
```


## Step 2 -- Mitigate with runtime.Gosched

Modify the CPU-heavy goroutines to yield periodically using `runtime.Gosched()`. Measure the latency improvement and the throughput cost.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	numMeasurements  = 20
	measureInterval  = 10 * time.Millisecond
	testDuration     = 500 * time.Millisecond
	yieldEveryN      = 100_000
	iterationsPerRun = 10_000_000
)

func measureLatency(stop <-chan struct{}, wg *sync.WaitGroup, results chan<- time.Duration) {
	defer wg.Done()
	for i := 0; i < numMeasurements; i++ {
		select {
		case <-stop:
			return
		default:
		}
		start := time.Now()
		time.Sleep(measureInterval)
		results <- time.Since(start) - measureInterval
		time.Sleep(1 * time.Millisecond)
	}
}

func cpuHeavyNoYield(stop <-chan struct{}, wg *sync.WaitGroup, ops *int64) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		default:
		}
		sum := 0
		for i := 0; i < iterationsPerRun; i++ {
			sum += i * i
		}
		_ = sum
		atomic.AddInt64(ops, 1)
	}
}

func cpuHeavyWithYield(stop <-chan struct{}, wg *sync.WaitGroup, ops *int64) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		default:
		}
		sum := 0
		for i := 0; i < iterationsPerRun; i++ {
			sum += i * i
			if i%yieldEveryN == 0 {
				runtime.Gosched()
			}
		}
		_ = sum
		atomic.AddInt64(ops, 1)
	}
}

func runExperiment(label string, workerFn func(<-chan struct{}, *sync.WaitGroup, *int64)) (avgLatency time.Duration, maxLatency time.Duration, throughput int64) {
	procs := runtime.GOMAXPROCS(0)
	numHeavy := procs * 2

	stop := make(chan struct{})
	results := make(chan time.Duration, numMeasurements)
	var wg sync.WaitGroup
	var ops int64

	for i := 0; i < numHeavy; i++ {
		wg.Add(1)
		go workerFn(stop, &wg, &ops)
	}
	time.Sleep(50 * time.Millisecond)

	wg.Add(1)
	go measureLatency(stop, &wg, results)

	var total time.Duration
	maxLatency = 0
	for i := 0; i < numMeasurements; i++ {
		d := <-results
		total += d
		if d > maxLatency {
			maxLatency = d
		}
	}
	avgLatency = total / numMeasurements

	close(stop)
	wg.Wait()
	throughput = atomic.LoadInt64(&ops)
	return
}

func main() {
	procs := runtime.GOMAXPROCS(0)
	fmt.Printf("=== Gosched Mitigation ===\n")
	fmt.Printf("  GOMAXPROCS: %d | Heavy goroutines: %d\n", procs, procs*2)
	fmt.Printf("  Yield every: %d iterations\n\n", yieldEveryN)

	fmt.Println("--- Without Gosched ---")
	avgNo, maxNo, opsNo := runExperiment("no-yield", cpuHeavyNoYield)
	fmt.Printf("  Avg overhead: %v | Max overhead: %v | Throughput: %d batches\n\n",
		avgNo.Round(time.Microsecond), maxNo.Round(time.Microsecond), opsNo)

	fmt.Println("--- With Gosched ---")
	avgYes, maxYes, opsYes := runExperiment("yield", cpuHeavyWithYield)
	fmt.Printf("  Avg overhead: %v | Max overhead: %v | Throughput: %d batches\n\n",
		avgYes.Round(time.Microsecond), maxYes.Round(time.Microsecond), opsYes)

	fmt.Println("=== Comparison ===")
	if avgNo > 0 {
		fmt.Printf("  Avg latency improvement: %.1fx\n", float64(avgNo)/float64(avgYes))
	}
	if maxNo > 0 {
		fmt.Printf("  Max latency improvement: %.1fx\n", float64(maxNo)/float64(maxYes))
	}
	if opsNo > 0 {
		throughputCost := (1.0 - float64(opsYes)/float64(opsNo)) * 100
		fmt.Printf("  Throughput cost: %.1f%% fewer batches\n", throughputCost)
	}
	fmt.Println()
	fmt.Println("  Gosched trades a small throughput cost for significantly better latency fairness")
}
```

**What's happening here:** Two experiments run back-to-back. The first uses CPU-heavy goroutines that never yield. The second uses goroutines that call `runtime.Gosched()` every 100,000 iterations. `Gosched` voluntarily gives up the processor, allowing other goroutines in the run queue (including the latency measurement goroutine) to execute. The throughput counter tracks how many computation batches complete, showing the cost of yielding.

**Key insight:** `runtime.Gosched()` is a hint, not a guarantee. It puts the current goroutine at the back of the run queue and lets the scheduler pick another goroutine to run. The frequency of yielding is a tuning knob: yield too often and throughput drops significantly; yield too rarely and latency-sensitive goroutines still starve. In production, profiling determines the right frequency.

### Verification
```bash
go run main.go
```
Expected output (values vary by machine):
```
=== Gosched Mitigation ===
  GOMAXPROCS: 8 | Heavy goroutines: 16
  Yield every: 100000 iterations

--- Without Gosched ---
  Avg overhead: 1.1ms | Max overhead: 4.2ms | Throughput: 42 batches

--- With Gosched ---
  Avg overhead: 250us | Max overhead: 800us | Throughput: 35 batches

=== Comparison ===
  Avg latency improvement: 4.4x
  Max latency improvement: 5.2x
  Throughput cost: 16.7% fewer batches

  Gosched trades a small throughput cost for significantly better latency fairness
```


## Step 3 -- GOMAXPROCS Effect on Fairness

Vary `GOMAXPROCS` and observe how it affects both latency and throughput when running mixed workloads. More processors means more opportunities for latency-sensitive goroutines to find a free processor.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	numMeasurements  = 15
	measureInterval  = 10 * time.Millisecond
	numHeavyPerProc  = 2
	iterationsPerRun = 5_000_000
)

func measureLatency(stop <-chan struct{}, wg *sync.WaitGroup, results chan<- time.Duration) {
	defer wg.Done()
	for i := 0; i < numMeasurements; i++ {
		select {
		case <-stop:
			return
		default:
		}
		start := time.Now()
		time.Sleep(measureInterval)
		results <- time.Since(start) - measureInterval
		time.Sleep(1 * time.Millisecond)
	}
}

func cpuHeavy(stop <-chan struct{}, wg *sync.WaitGroup, ops *int64) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		default:
		}
		sum := 0
		for i := 0; i < iterationsPerRun; i++ {
			sum += i * i
		}
		_ = sum
		atomic.AddInt64(ops, 1)
	}
}

type ExperimentResult struct {
	Procs      int
	NumHeavy   int
	AvgLatency time.Duration
	MaxLatency time.Duration
	Throughput int64
}

func runWithProcs(procs int) ExperimentResult {
	prev := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(prev)

	numHeavy := procs * numHeavyPerProc
	stop := make(chan struct{})
	results := make(chan time.Duration, numMeasurements)
	var wg sync.WaitGroup
	var ops int64

	for i := 0; i < numHeavy; i++ {
		wg.Add(1)
		go cpuHeavy(stop, &wg, &ops)
	}
	time.Sleep(50 * time.Millisecond)

	wg.Add(1)
	go measureLatency(stop, &wg, results)

	var total, max time.Duration
	for i := 0; i < numMeasurements; i++ {
		d := <-results
		total += d
		if d > max {
			max = d
		}
	}

	close(stop)
	wg.Wait()

	return ExperimentResult{
		Procs:      procs,
		NumHeavy:   numHeavy,
		AvgLatency: total / numMeasurements,
		MaxLatency: max,
		Throughput: atomic.LoadInt64(&ops),
	}
}

func main() {
	maxProcs := runtime.NumCPU()
	fmt.Printf("=== GOMAXPROCS Effect on Fairness ===\n")
	fmt.Printf("  CPU cores: %d | Heavy goroutines: %dx GOMAXPROCS\n\n", maxProcs, numHeavyPerProc)

	procsToTest := []int{1, 2}
	if maxProcs >= 4 {
		procsToTest = append(procsToTest, 4)
	}
	if maxProcs >= 8 {
		procsToTest = append(procsToTest, 8)
	}
	if maxProcs > 8 {
		procsToTest = append(procsToTest, maxProcs)
	}

	fmt.Printf("  %-12s %-12s %-15s %-15s %-12s\n",
		"GOMAXPROCS", "Heavy GRs", "Avg Overhead", "Max Overhead", "Throughput")
	fmt.Println("  " + "--------------------------------------------------------------------")

	for _, procs := range procsToTest {
		result := runWithProcs(procs)
		fmt.Printf("  %-12d %-12d %-15v %-15v %-12d\n",
			result.Procs, result.NumHeavy,
			result.AvgLatency.Round(time.Microsecond),
			result.MaxLatency.Round(time.Microsecond),
			result.Throughput)
	}

	fmt.Println()
	fmt.Println("=== Analysis ===")
	fmt.Println("  - GOMAXPROCS=1: all goroutines share one processor, highest starvation")
	fmt.Println("  - More processors: latency-sensitive goroutines find idle processors faster")
	fmt.Println("  - Diminishing returns: beyond CPU core count, adding processors has no effect")
	fmt.Println("  - The ratio of heavy goroutines to processors determines starvation severity")
}
```

**What's happening here:** The experiment runs the same mixed workload with different `GOMAXPROCS` values. With `GOMAXPROCS=1`, every goroutine competes for a single processor. With higher values, the latency-sensitive goroutine has more chances to find an available processor. The number of CPU-heavy goroutines scales proportionally (`2x GOMAXPROCS`) to maintain the same contention ratio.

**Key insight:** `GOMAXPROCS` controls how many OS threads can execute goroutines simultaneously. Increasing it does not eliminate starvation -- it reduces the probability of starvation. With `GOMAXPROCS=1`, the measurement goroutine must wait for a CPU-heavy goroutine to be preempted. With `GOMAXPROCS=8`, there are more scheduling opportunities. But if you have 16 CPU-heavy goroutines and 8 processors, starvation is still possible because every processor might be running a heavy goroutine when the measurement goroutine wakes up. The real solution is combining `GOMAXPROCS` with yield points.

### Verification
```bash
go run main.go
```
Expected output (values vary by machine):
```
=== GOMAXPROCS Effect on Fairness ===
  CPU cores: 8 | Heavy goroutines: 2x GOMAXPROCS

  GOMAXPROCS   Heavy GRs    Avg Overhead    Max Overhead    Throughput
  --------------------------------------------------------------------
  1            2            2.5ms           8.1ms           12
  2            4            1.1ms           4.3ms           22
  4            8            600us           2.1ms           38
  8            16           350us           1.5ms           65

=== Analysis ===
  - GOMAXPROCS=1: all goroutines share one processor, highest starvation
  - More processors: latency-sensitive goroutines find idle processors faster
  - Diminishing returns: beyond CPU core count, adding processors has no effect
  - The ratio of heavy goroutines to processors determines starvation severity
```


## Common Mistakes

### Assuming Goroutines Are Always Preempted Fairly

```go
// Wrong assumption: "the scheduler will handle it"
func processCSVBatch(data [][]string) {
	for _, row := range data {
		// millions of rows, pure computation, no function calls
		// the scheduler preempts via signals but there's still overhead
		result := parseAndValidateInline(row)
		_ = result
	}
}

// Meanwhile, in another goroutine:
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	// expects sub-millisecond response
	// but may wait for CSV processing to yield
	w.WriteHeader(http.StatusOK)
}
```
**What happens:** Even with Go 1.14+ asynchronous preemption, tight loops create scheduling pressure. The health check handler experiences unpredictable latency spikes. In Kubernetes, a slow health check response causes the pod to be marked unhealthy and restarted -- creating a cascading failure.

**Fix:** Add explicit yield points in the computational loop, or move CPU-heavy work to a separate process with resource limits.


### Calling Gosched Too Frequently

```go
// Wrong: yielding on every iteration destroys throughput
func cpuWork(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		// one iteration of real work
		result := computeOne()
		_ = result
		runtime.Gosched() // context switch overhead per iteration
	}
}
```
**What happens:** Each `Gosched` call involves saving and restoring goroutine state. Called millions of times per second, the overhead dominates the actual computation. Throughput drops by 10-100x depending on how cheap each iteration is.

**Fix:** Yield every N iterations where N is tuned via benchmarking. Start with N=10,000 and adjust based on latency measurements.


### Using GOMAXPROCS(1) to "Simplify" Concurrency

```go
// Wrong: forcing single-processor to avoid "concurrency bugs"
func init() {
	runtime.GOMAXPROCS(1) // "this makes it simpler"
}
```
**What happens:** All goroutines share one processor. Any CPU-heavy goroutine starves everything else. The program cannot use multiple CPU cores. What was intended to simplify concurrency actually introduces starvation that would not exist with higher `GOMAXPROCS`.

**Fix:** Let `GOMAXPROCS` default to `runtime.NumCPU()`. Fix concurrency bugs with proper synchronization, not by eliminating parallelism.


## Review

Starvation is a latency problem, not a liveness one: nothing deadlocks, but
CPU-heavy goroutines monopolize the processors and everything else waits its
turn in the run queue. Go 1.14's asynchronous preemption blunts the worst case,
yet tight inline loops still create scheduling pressure and measurable overhead,
so the average and -- more importantly -- the tail latency climb. Two knobs pull
in different directions. `runtime.Gosched()` voluntarily parks the current
goroutine at the back of the run queue, buying fairness at a throughput cost
that grows with how often you yield, which makes the yield frequency a tuning
decision, not a constant. `GOMAXPROCS` sets how many goroutines run at once;
raising it lowers the probability of starvation but never eliminates it, because
the severity is really governed by the ratio of heavy goroutines to available
processors. In production you combine both: adequate `GOMAXPROCS` plus yield
points where the loops are hottest -- and you decide with percentile
measurements (P50, P99, max), never averages alone.

To confirm the intuition, build a priority scheduler simulation: a buffered
`highPriority` queue (capacity 10) and a buffered `lowPriority` queue (capacity
100), three workers that try `highPriority` first via `select` with a `default`
and fall back to `lowPriority`, and a producer emitting 50 fast high-priority
tasks (1ms each) and 200 slow low-priority tasks (5ms each). Measure average
latency per class and total throughput, then compare against round-robin on a
single channel. The `select`-plus-`default` idiom is the whole trick: read
`highPriority` if anything is there, otherwise serve `lowPriority`, without
blocking when the high queue is empty.

## Resources

- [runtime.Gosched](https://pkg.go.dev/runtime#Gosched) -- the call that voluntarily yields the processor to other runnable goroutines.
- [runtime.GOMAXPROCS](https://pkg.go.dev/runtime#GOMAXPROCS) -- reads or sets how many OS threads may run Go code simultaneously.
- [Go 1.14 Release Notes: Goroutine Preemption](https://go.dev/doc/go1.14#runtime) -- the release that added signal-based asynchronous preemption of tight loops.
- [GMP Model](https://go.dev/src/runtime/proc.go) -- the scheduler source, where goroutine/machine/processor scheduling actually lives.

---

Back to [Concurrency](../../concurrency.md) | Next: [29-goroutine-work-stealing](../29-goroutine-work-stealing/29-goroutine-work-stealing.md)
