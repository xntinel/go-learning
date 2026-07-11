# Exercise 2: Goroutine vs OS Thread

Go's headline capability is running thousands -- even millions -- of concurrent
tasks without exhausting system resources, and the reason is architectural:
goroutines are not OS threads. An OS thread reserves 1-8 MB of stack at
creation, needs a kernel context switch to schedule, and consumes significant
kernel resources. A goroutine starts with a stack of just 2-8 KB (the exact
size depends on the Go version) and is scheduled entirely in user space by the
Go runtime. This exercise measures that gap directly, in goroutine counts and
bytes, so the cost difference stops being folklore and becomes a number you can
plan capacity against.

## What you'll build

```text
02-goroutine-vs-os-thread/
  main.go        count goroutines and weigh their stack/memory footprint at
                 connection scale, then compare against the OS-thread equivalent
```

- Build: a set of measurement programs that count live goroutines and weigh their memory footprint at connection scale.
- Implement: `runtime.NumGoroutine()` tracking, `runtime.MemStats` sampling (`Sys` vs `StackInuse`), and a capacity table comparing goroutines to OS threads across server RAM sizes.
- Verify: `go run main.go`

### Why goroutines cost kilobytes, not megabytes

This difference is not academic. A Java application creating 10,000 threads
would consume roughly 10-80 GB of stack memory alone, making it impractical on
most machines. A Go application can comfortably create 10,000 goroutines using
just 20-80 MB of stack memory. This is what enables the
"one-goroutine-per-connection" pattern that makes Go so effective for network
servers.

Understanding this cost difference helps you make informed architectural
decisions. When you know a goroutine costs approximately the same as a small
struct allocation, you stop worrying about creating them and start thinking in
terms of concurrent tasks rather than thread pools.

## Step 1 -- Simulating 10K HTTP Connections

Imagine your service needs to handle 10,000 concurrent HTTP connections. Each connection reads a request, processes it, and sends a response. With goroutines, you can handle each connection independently. This step shows how `runtime.NumGoroutine()` tracks these simulated connections.

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

const (
	simulatedConnections = 10_000
	phaseDelay           = 1 * time.Millisecond
	settleDelay          = 50 * time.Millisecond
)

func handleConnection(done <-chan struct{}) {
	time.Sleep(phaseDelay) // Phase 1: Read request
	time.Sleep(phaseDelay) // Phase 2: Process
	time.Sleep(phaseDelay) // Phase 3: Send response
	<-done                 // Keep-alive: wait for disconnect signal
}

func launchConnections(count int, done <-chan struct{}) {
	for i := 0; i < count; i++ {
		go handleConnection(done)
	}
}

func printGoroutineCount(label string) {
	fmt.Printf("%-20s %d goroutines\n", label, runtime.NumGoroutine())
}

func main() {
	printGoroutineCount("Goroutines at start:")

	done := make(chan struct{})
	launchConnections(simulatedConnections, done)

	time.Sleep(settleDelay)
	printGoroutineCount("Active connections:")

	close(done)
	time.Sleep(settleDelay)
	printGoroutineCount("After disconnect:")
}
```

**What's happening here:** We simulate 10,000 concurrent HTTP connections, each in its own goroutine. Each connection goes through the typical read-process-respond lifecycle before blocking on a channel (simulating a keep-alive connection). `runtime.NumGoroutine()` reports the exact count of goroutines alive at that moment.

**Key insight:** The initial count is 1 because `main` itself is a goroutine. After launching 10,000, the count is 10,001. Closing the channel disconnects all simulated clients at once. In a real server, the operating system would be unable to create 10,000 OS threads on most machines, but 10,000 goroutines is trivial.

**What would happen if you forgot `close(done)`?** The 10,000 goroutines would be leaked -- they would block on `<-done` forever, consuming memory until the process exits. In a production server, goroutine leaks are a common source of memory exhaustion over time.

### Verification
```bash
go run main.go
```
Expected output:
```
Goroutines at start: 1
Active connections:  10001
After disconnect:    1
```

## Step 2 -- Measuring Memory per Connection

Use `runtime.MemStats` to measure how much memory 10,000 simulated connections actually consume. Then extrapolate what the same workload would cost with OS threads.

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

const (
	connectionCount     = 100_000
	osThreadStackBytes  = 8 * 1024 * 1024 // 8 MB per OS thread
	connectionSettleMs  = 200
)

type MemorySnapshot struct {
	Before runtime.MemStats
	After  runtime.MemStats
}

func captureBaseline() runtime.MemStats {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats
}

func simulateConnectionHandler(done <-chan struct{}) {
	time.Sleep(1 * time.Millisecond)
	<-done
}

func launchConnectionHandlers(count int, done <-chan struct{}) {
	for i := 0; i < count; i++ {
		go simulateConnectionHandler(done)
	}
}

func printMemoryComparison(before, after runtime.MemStats, count int) {
	totalBytes := after.Sys - before.Sys
	perGoroutine := totalBytes / uint64(count)

	fmt.Printf("Simulated connections:  %d\n", count)
	fmt.Printf("Active goroutines:     %d\n", runtime.NumGoroutine())
	fmt.Printf("Memory increase:       %.2f MB (Sys)\n", float64(totalBytes)/(1024*1024))
	fmt.Printf("Per connection:        ~%d bytes (~%.1f KB)\n", perGoroutine, float64(perGoroutine)/1024)

	fmt.Println()
	osThreadMem := uint64(count) * osThreadStackBytes
	fmt.Printf("Same connections as OS threads (8MB stack each):\n")
	fmt.Printf("  Would require:       %.2f GB\n", float64(osThreadMem)/(1024*1024*1024))
	fmt.Printf("  Goroutine advantage: ~%dx less memory\n", osThreadMem/totalBytes)
}

func main() {
	before := captureBaseline()

	done := make(chan struct{})
	launchConnectionHandlers(connectionCount, done)
	time.Sleep(connectionSettleMs * time.Millisecond)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	printMemoryComparison(before, after, connectionCount)

	close(done)
	time.Sleep(connectionSettleMs * time.Millisecond)
}
```

**What's happening here:** We force garbage collection to get a clean baseline, create 100,000 goroutines simulating connection handlers, then measure the memory difference. The `Sys` field in `MemStats` represents total memory obtained from the OS, including stacks, heap, and runtime overhead.

**Key insight:** Each goroutine costs roughly 2-8 KB, not megabytes. For the same 100,000 concurrent connections, OS threads would need ~800 GB of stack memory -- clearly impossible on any machine. This is why Go servers can handle C10K (and beyond) without thread pools.

**What would happen if you skipped `runtime.GC()`?** The baseline measurement would include garbage from previous allocations, making the per-goroutine calculation less accurate.

### Verification
```bash
go run main.go
```
Expected output (values vary by system):
```
Simulated connections:  100000
Active goroutines:     100001
Memory increase:       ~200-800 MB (Sys includes reserves)
Per connection:        ~2000-8000 bytes (~2.0-8.0 KB)

Same connections as OS threads (8MB stack each):
  Would require:       781.25 GB
  Goroutine advantage: ~1000x less memory
```

## Step 3 -- Server Capacity Comparison Table

Print a table showing how many concurrent connections a server can handle with goroutines vs OS threads, given common server RAM sizes.

```go
package main

import (
	"fmt"
	"strings"
)

const (
	goroutineStackCost = 8 * 1024       // 8 KB per goroutine
	osThreadStackCost  = 8 * 1024 * 1024 // 8 MB per OS thread
	ramUsableFraction  = 2               // use 50% of RAM for stacks
)

type CapacityRow struct {
	RAMLabel       string
	GoConnections  int
	ThreadConns    int
	Advantage      int
}

func formatCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func calculateCapacity(ramGB int) CapacityRow {
	availableBytes := ramGB * 1024 * 1024 * 1024 / ramUsableFraction
	goConns := availableBytes / goroutineStackCost
	threadConns := availableBytes / osThreadStackCost

	return CapacityRow{
		RAMLabel:      fmt.Sprintf("%d GB", ramGB),
		GoConnections: goConns,
		ThreadConns:   threadConns,
		Advantage:     goConns / threadConns,
	}
}

func printCapacityTable(serverRAMs []int) {
	fmt.Println("=== Maximum Concurrent Connections by Server RAM ===")
	fmt.Println("(assuming 50% of RAM available for connection stacks)")
	fmt.Println()
	fmt.Printf("%-10s %-22s %-22s %-10s\n", "RAM", "Go (goroutines)", "Threads (8MB stack)", "Advantage")
	fmt.Println(strings.Repeat("-", 68))

	for _, gb := range serverRAMs {
		row := calculateCapacity(gb)
		fmt.Printf("%-10s %-22s %-22s %-10s\n",
			row.RAMLabel,
			formatCount(row.GoConnections),
			formatCount(row.ThreadConns),
			fmt.Sprintf("%dx", row.Advantage),
		)
	}

	fmt.Println()
	fmt.Println("This is why Go servers use goroutine-per-connection while")
	fmt.Println("Java/C++ servers need thread pools with connection queuing.")
}

func main() {
	serverRAMs := []int{1, 4, 8, 16, 32, 64}
	printCapacityTable(serverRAMs)
}
```

**What's happening here:** We calculate how many concurrent connections different server configurations can sustain with goroutines vs OS threads. A modest 4 GB server can handle 262K goroutines but only 256 OS threads. This drives real architectural decisions.

**Key insight:** This 1000x difference is why Go can do "one goroutine per connection" while languages with native threads need thread pools. A Java server with 10,000 connections needs ~80 GB of thread stack memory. A Go server needs ~80 MB. This directly impacts your infrastructure costs.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Maximum Concurrent Connections by Server RAM ===
(assuming 50% of RAM available for connection stacks)

RAM        Go (goroutines)        Threads (8MB stack)    Advantage
--------------------------------------------------------------------
1 GB       65.5K                  64                     1024x
4 GB       262.1K                 256                    1024x
8 GB       524.3K                 512                    1024x
16 GB      1.0M                   1.0K                   1024x
32 GB      2.1M                   2.0K                   1024x
64 GB      4.2M                   4.1K                   1024x
```

## Step 4 -- Live Stack Measurement with StackInuse

Measure `StackInuse` (more precise than `Sys`) to see actual stack memory consumed by simulated connection handlers at different scales.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

const (
	osThreadStackSize   = 8 * 1024 * 1024
	measureSettleDelay  = 100 * time.Millisecond
	cleanupDelay        = 200 * time.Millisecond
)

type StackMeasurement struct {
	Count        int
	StackInUse   uint64
	PerGoroutine uint64
	OSEquivalent uint64
}

func measureStackAtScale(count int) StackMeasurement {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	done := make(chan struct{})
	for i := 0; i < count; i++ {
		go func() {
			<-done
		}()
	}
	time.Sleep(measureSettleDelay)

	runtime.ReadMemStats(&after)

	stackInUse := after.StackInuse - before.StackInuse

	close(done)
	time.Sleep(cleanupDelay)
	runtime.GC()

	return StackMeasurement{
		Count:        count,
		StackInUse:   stackInUse,
		PerGoroutine: stackInUse / uint64(count),
		OSEquivalent: uint64(count) * osThreadStackSize,
	}
}

func printStackTable(scales []int) {
	fmt.Println("=== Goroutine Stack Memory at Different Connection Scales ===")
	fmt.Printf("%-12s %-15s %-18s %-15s\n",
		"Connections", "Stack/conn", "Total Stack", "OS Thread Equiv")
	fmt.Println(strings.Repeat("-", 65))

	for _, count := range scales {
		m := measureStackAtScale(count)
		fmt.Printf("%-12d %-15s %-18s %-15s\n",
			m.Count,
			fmt.Sprintf("%d B (%.1f KB)", m.PerGoroutine, float64(m.PerGoroutine)/1024),
			fmt.Sprintf("%.2f MB", float64(m.StackInUse)/(1024*1024)),
			fmt.Sprintf("%.2f GB", float64(m.OSEquivalent)/(1024*1024*1024)),
		)
	}

	fmt.Println()
	fmt.Println("StackInuse measures ONLY stack memory, not heap or runtime overhead.")
	fmt.Println("This is the most accurate metric for goroutine stack consumption.")
}

func main() {
	scales := []int{1_000, 5_000, 10_000, 50_000}
	printStackTable(scales)
}
```

**What's happening here:** `StackInuse` measures only the stack memory actively used by goroutines, excluding heap and runtime overhead. This gives a more precise per-goroutine stack measurement than `Sys`. At each scale, we compare with what OS threads would require.

**Key insight:** The per-goroutine stack is typically 2,048-8,192 bytes for idle goroutines. This is the minimum allocation unit. Goroutines that do more work will have larger stacks (explored in exercise 04). The takeaway for capacity planning: each idle connection handler costs about 8 KB, so 100K connections costs about 800 MB of stack alone.

**What would happen if you used `StackSys` instead of `StackInuse`?** `StackSys` is memory *reserved* from the OS for stacks (may include unused pages). `StackInuse` is what goroutines are actually using. Always use `StackInuse` for measuring goroutine stack consumption.

### Verification
```bash
go run main.go
```
Expected output (values vary):
```
=== Goroutine Stack Memory at Different Connection Scales ===
Connections  Stack/conn      Total Stack        OS Thread Equiv
-----------------------------------------------------------------
1000         8192 B (8.0 KB) 8.00 MB            7.81 GB
5000         8192 B (8.0 KB) 40.00 MB           39.06 GB
10000        8192 B (8.0 KB) 80.00 MB           78.13 GB
50000        8192 B (8.0 KB) 400.00 MB          390.63 GB
```

## Common Mistakes

### Assuming Goroutines Are Free

**Wrong thinking:** "Goroutines are cheap, so I'll create one for every tiny operation."

**What happens:** While goroutines are cheap, they are not free. Each one consumes memory and scheduler time. Creating millions of goroutines that contend for the same resource will cause performance degradation. In a production server, unbounded goroutine creation during a traffic spike can OOM the process.

**Fix:** Use goroutines for genuinely concurrent work. For CPU-bound tasks, the optimal goroutine count is typically `runtime.NumCPU()`. For I/O-bound tasks, create goroutines based on the number of independent operations, but set upper bounds.

### Forgetting to Release Goroutines

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"runtime"
)

const leakedHandlerCount = 10_000

func main() {
	done := make(chan struct{})
	for i := 0; i < leakedHandlerCount; i++ {
		go func() {
			<-done // blocks forever if done is never closed
		}()
	}
	// forgot to close(done) -- 10,000 goroutines leaked
	fmt.Printf("Leaked goroutines: %d\n", runtime.NumGoroutine()-1)
}
```

**What happens:** Goroutine leak. In a real server, this pattern appears when connection handlers block on a channel that is never closed. Over hours of operation, memory grows until the process is OOM-killed.

**Correct -- complete program:**
```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

const handlerCount = 10_000

func launchBlockingHandlers(count int, done <-chan struct{}) {
	for i := 0; i < count; i++ {
		go func() {
			<-done
		}()
	}
}

func main() {
	done := make(chan struct{})
	launchBlockingHandlers(handlerCount, done)

	time.Sleep(10 * time.Millisecond)
	fmt.Printf("Before cleanup: %d goroutines\n", runtime.NumGoroutine())

	close(done)
	time.Sleep(10 * time.Millisecond)
	fmt.Printf("After cleanup:  %d goroutines\n", runtime.NumGoroutine())
}
```

### Confusing StackInuse with Sys

**Wrong:**
```go
totalBytes := after.Sys - before.Sys // includes heap, runtime, AND stack
perGoroutine := totalBytes / count   // inflated number
```

**Fix:** Use `StackInuse` when you want to know stack-specific consumption:
```go
stackBytes := after.StackInuse - before.StackInuse // just stacks
perGoroutine := stackBytes / count                  // accurate stack cost
```

## Review

The gap this exercise measures is roughly a thousandfold: a goroutine begins
life with a 2-8 KB stack while an OS thread reserves 1-8 MB, which is why one
process can host millions of goroutines but only thousands of threads.
`runtime.NumGoroutine()` reports how many are alive at any moment, and
`runtime.MemStats` weighs them -- `StackInuse` for stack memory alone, `Sys`
for everything the runtime obtained from the OS (stacks plus heap plus
overhead), so the two answer different questions and must not be conflated.
Cheap is not free: each goroutine still costs memory and scheduler time, and a
goroutine blocked on a channel that never closes is leaked, holding its stack
until the process dies. That lightweight footprint is the entire reason Go can
run one goroutine per connection and serve 10K+ concurrent connections on a
single machine.

To prove you own this, build the benchmark the exercise points at: launch
1,000, then 10,000, then 50,000 goroutines in separate rounds, each simulating
an HTTP connection handler with small read-process-respond delays; for every
round, time how long the launch takes and read `StackInuse` afterward; then
print a table of count, launch time, stack memory, and the equivalent OS-thread
cost. Use `time.Now()` and `time.Since()` for the timing, and force a GC between
rounds so each measurement starts from a clean baseline.

## Resources

- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) -- the call that returns the current count of live goroutines.
- [runtime.MemStats](https://pkg.go.dev/runtime#MemStats) -- the struct whose `StackInuse` and `Sys` fields expose stack-only versus total runtime memory.
- [Go Blog: Goroutines are not threads](https://go.dev/blog/waza-talk) -- Rob Pike's talk on why the runtime multiplexes goroutines onto threads.
- [Why goroutines instead of threads?](https://go.dev/doc/faq#goroutines) -- the FAQ entry on the design rationale and cost trade-offs.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-gmp-model-in-action](../03-gmp-model-in-action/03-gmp-model-in-action.md)
