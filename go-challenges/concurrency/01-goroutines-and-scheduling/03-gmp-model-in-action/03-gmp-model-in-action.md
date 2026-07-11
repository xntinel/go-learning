# Exercise 3: GMP Model in Action

The failure mode this exercise guards against is not a crash but a wrong mental model. Treat goroutines as "magic lightweight threads" and you will set `GOMAXPROCS` expecting it to cap how many goroutines can exist, or be baffled when a service doing heavy file I/O sprouts fifty OS threads on an eight-core box. Go's scheduler is a well-understood three-entity system -- G, M, and P -- and this exercise makes each entity observable at runtime so the "magic" turns into mechanism you can reason about.

## What you'll build

```text
gmp-model-in-action/
  main.go        four runnable programs: GOMAXPROCS vs CPU-bound work,
                 G count under a staged pipeline, M growth during blocking
                 I/O, and a GMP snapshot reporter for a mixed workload
```

- Build: a set of instrumentation programs that read G, M, and P counts while CPU-bound and IO-bound goroutines run.
- Implement: `benchmarkChecksumWorkers` across GOMAXPROCS settings; a `PipelineStage` that launches and drains barriers; `launchBlockingReaders` that forces M growth via `fsync`; and a `captureGMPSnapshot` / `GMPSnapshot` reporter.
- Verify: `go run main.go` at each step, comparing the printed G and P counts against the commentary.

### Why M count can exceed P count

Go's scheduler uses a model with three key entities: G (goroutine), M (machine/OS thread), and P (processor). Understanding this model transforms goroutines from "magic lightweight threads" into a well-understood scheduling system.

**G (Goroutine):** The unit of work. Contains the stack, instruction pointer, and other scheduling state. Gs are what your code creates with the `go` keyword. You can have millions of Gs.

**M (Machine):** An OS thread. The Go runtime creates Ms as needed to execute Gs. An M must be attached to a P to run Go code. Ms can be blocked in syscalls without holding a P. The runtime creates new Ms when existing ones are blocked.

**P (Processor):** A logical processor that acts as a resource context. Each P has a local run queue of Gs waiting to execute. The number of Ps is set by `GOMAXPROCS` and determines the maximum parallelism. A P must be acquired by an M before it can execute any G.

The key insight is that when an M blocks on a syscall (like file I/O or a CGo call), it releases its P so another M can pick it up and continue running Gs. This is why the number of Ms can grow beyond the number of Ps -- blocked Ms need to be replaced to maintain throughput.

## Step 1 -- Observing P Count with CPU and IO Workloads

In a real system, you often need to understand how GOMAXPROCS affects your specific workload. A data pipeline that computes checksums is CPU-bound. A service that reads configuration files is IO-bound. This step shows how P count relates to both.

```go
package main

import (
	"crypto/sha256"
	"fmt"
	"runtime"
	"time"
)

const (
	dataBlockSize      = 1024 * 1024 // 1 MB
	checksumWorkers    = 4
	checksumsPerWorker = 50
)

func generateTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	return data
}

func computeChecksums(data []byte, iterations int) {
	for i := 0; i < iterations; i++ {
		sha256.Sum256(data)
	}
}

func benchmarkChecksumWorkers(data []byte, workerCount, iterations int) time.Duration {
	start := time.Now()
	done := make(chan struct{}, workerCount)

	for w := 0; w < workerCount; w++ {
		go func() {
			computeChecksums(data, iterations)
			done <- struct{}{}
		}()
	}
	for w := 0; w < workerCount; w++ {
		<-done
	}

	return time.Since(start)
}

func printSystemInfo() {
	fmt.Printf("Number of CPUs:    %d\n", runtime.NumCPU())
	fmt.Printf("GOMAXPROCS (Ps):   %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("Default: GOMAXPROCS == NumCPU (since Go 1.5)\n\n")
}

func main() {
	printSystemInfo()

	data := generateTestData(dataBlockSize)
	numCPU := runtime.NumCPU()

	for _, procs := range []int{1, 2, numCPU} {
		runtime.GOMAXPROCS(procs)
		elapsed := benchmarkChecksumWorkers(data, checksumWorkers, checksumsPerWorker)
		fmt.Printf("  GOMAXPROCS=%-2d  %d workers x %d checksums: %v\n",
			procs, checksumWorkers, checksumsPerWorker, elapsed.Round(time.Millisecond))
	}

	runtime.GOMAXPROCS(numCPU)
	fmt.Printf("\nWith more Ps, CPU-bound checksum work runs faster because\n")
	fmt.Printf("multiple goroutines execute simultaneously on different cores.\n")
}
```

**What's happening here:** `GOMAXPROCS(0)` is a read-only call. We compute SHA-256 checksums of a 1 MB data block -- a realistic CPU-bound operation similar to verifying file integrity or processing uploads. With GOMAXPROCS=1, all workers share one P and take turns. With GOMAXPROCS=NumCPU, they run in parallel.

**Key insight:** GOMAXPROCS controls how many Ps exist, which limits how many goroutines can execute Go code simultaneously. It does NOT limit how many goroutines can exist.

**What would happen if you set GOMAXPROCS(1)?** Only one P would exist, so the four checksum workers would run sequentially on a single core. Total time would be 4x longer than with 4 Ps.

### Verification
```bash
go run main.go
```
Expected output (CPU count and times vary):
```
Number of CPUs:    8
GOMAXPROCS (Ps):   8
Default: GOMAXPROCS == NumCPU (since Go 1.5)

  GOMAXPROCS=1   4 workers x 50 checksums: 680ms
  GOMAXPROCS=2   4 workers x 50 checksums: 350ms
  GOMAXPROCS=8   4 workers x 50 checksums: 175ms

With more Ps, CPU-bound checksum work runs faster because
multiple goroutines execute simultaneously on different cores.
```

## Step 2 -- G Count Under Load: Simulated File Processing Pipeline

In a data pipeline, you might process files in stages: read, transform, write. Each stage creates goroutines. This step shows how G count grows and shrinks as stages complete.

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

const stageSettleDelay = 10 * time.Millisecond

type PipelineStage struct {
	Name    string
	Count   int
	Barrier chan struct{}
}

func NewPipelineStage(name string, count int) PipelineStage {
	return PipelineStage{
		Name:    name,
		Count:   count,
		Barrier: make(chan struct{}),
	}
}

func (ps PipelineStage) Launch() {
	for j := 0; j < ps.Count; j++ {
		go func(barrier <-chan struct{}) {
			<-barrier
		}(ps.Barrier)
	}
}

func (ps PipelineStage) Complete() {
	close(ps.Barrier)
}

func main() {
	stages := []PipelineStage{
		NewPipelineStage("file-readers", 100),
		NewPipelineStage("checksum-workers", 500),
		NewPipelineStage("write-uploaders", 200),
	}

	for _, stage := range stages {
		stage.Launch()
		time.Sleep(stageSettleDelay)
		fmt.Printf("After launching %-20s (+%d): total G = %d\n",
			stage.Name, stage.Count, runtime.NumGoroutine())
	}

	for i := len(stages) - 1; i >= 0; i-- {
		stages[i].Complete()
		time.Sleep(stageSettleDelay)
		fmt.Printf("After completing %-20s:       total G = %d\n",
			stages[i].Name, runtime.NumGoroutine())
	}
}
```

**What's happening here:** We simulate a three-stage file processing pipeline. Each stage adds goroutines to the total count. Barriers keep them alive until the stage completes. Completing stages in reverse order shows the G count decreasing as each stage drains.

**Key insight:** The G count can grow to millions while P stays fixed at GOMAXPROCS. Gs are just data structures in the runtime's run queues; Ps are the execution slots. In a real pipeline, understanding this helps you reason about memory usage and backpressure.

### Verification
```bash
go run main.go
```
Expected output:
```
After launching file-readers          (+100): total G = 101
After launching checksum-workers      (+500): total G = 601
After launching write-uploaders       (+200): total G = 801
After completing write-uploaders      :       total G = 601
After completing checksum-workers     :       total G = 101
After completing file-readers         :       total G = 1
```

## Step 3 -- M Growth During Blocking I/O

When goroutines perform blocking system calls (file reads, DNS lookups, CGo), the runtime creates additional OS threads (Ms) to keep other goroutines running. This is critical for understanding why IO-heavy services sometimes have more OS threads than expected.

```go
package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

const (
	limitedProcs       = 2
	concurrentReaders  = 20
	sampleConfigData   = `{"service": "auth", "port": 8080, "timeout": "30s"}` + "\n"
)

func simulateBlockingFileIO() error {
	tempFile, err := os.CreateTemp("", "config-reader-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tempPath := tempFile.Name()

	if _, err := tempFile.Write([]byte(sampleConfigData)); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return fmt.Errorf("writing config data: %w", err)
	}

	if err := tempFile.Sync(); err != nil { // blocking syscall: forces M into kernel
		tempFile.Close()
		os.Remove(tempPath)
		return fmt.Errorf("syncing file: %w", err)
	}

	tempFile.Close()
	os.Remove(tempPath)
	return nil
}

func launchBlockingReaders(count int) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := simulateBlockingFileIO(); err != nil {
				fmt.Printf("  reader error: %v\n", err)
			}
		}()
	}
	return &wg
}

func main() {
	previousProcs := runtime.GOMAXPROCS(limitedProcs)
	defer runtime.GOMAXPROCS(previousProcs)

	fmt.Printf("GOMAXPROCS (Ps): %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("Goroutines before: %d\n\n", runtime.NumGoroutine())

	wg := launchBlockingReaders(concurrentReaders)

	time.Sleep(5 * time.Millisecond)
	fmt.Printf("During file I/O:   %d goroutines active\n", runtime.NumGoroutine())
	fmt.Println("With GOMAXPROCS=2, only 2 Ps exist, but the runtime creates")
	fmt.Println("additional OS threads (Ms) when goroutines block in syscalls.")
	fmt.Println("This is the M hand-off mechanism: blocked M releases its P")
	fmt.Println("so a new M can continue running other goroutines.")

	wg.Wait()
	fmt.Printf("\nAfter completion:  %d goroutines\n", runtime.NumGoroutine())
}
```

**What's happening here:** With GOMAXPROCS=2, only 2 Ps exist. But we launch 20 goroutines that each do file I/O (creating temp files, writing, fsyncing). When a goroutine's M blocks in `f.Sync()` (a kernel-level fsync call), the M releases its P. The runtime creates a new M to pick up the freed P and keep running other goroutines.

**Key insight:** P limits parallelism of Go code execution. M is the actual OS thread. During heavy syscall usage (file reads, DNS lookups, database connections via CGo), M count floats upward as the runtime compensates for blocked threads. In production, you might see 30+ OS threads on a service with GOMAXPROCS=8 if it does heavy file or network I/O.

**What would happen without the hand-off mechanism?** With 2 Ps and 2 Ms, as soon as both Ms enter syscalls, all other goroutines would be stuck. Your config file readers would serialize, and your service startup would take 10x longer.

### Verification
```bash
go run main.go
```
Expected output:
```
GOMAXPROCS (Ps): 2
Goroutines before: 1

During file I/O:   21 goroutines active
With GOMAXPROCS=2, only 2 Ps exist, but the runtime creates
additional OS threads (Ms) when goroutines block in syscalls.
This is the M hand-off mechanism: blocked M releases its P
so a new M can continue running other goroutines.

After completion:  1 goroutines
```

## Step 4 -- GMP Status Reporter for a Mixed Workload

Build a utility that prints GMP-related stats at labeled points during a mixed CPU-bound and IO-bound workload. This is the kind of instrumentation you would add to debug scheduler behavior in a real application.

```go
package main

import (
	"crypto/sha256"
	"fmt"
	"runtime"
	"time"
)

const (
	ioReadLatency        = 5 * time.Millisecond
	checksumBlockSize    = 64 * 1024
	checksumsPerGoroutine = 10
)

type GMPSnapshot struct {
	Label      string
	Goroutines int
	Procs      int
	NumCPU     int
	StackKB    float64
	SysMB      float64
}

func captureGMPSnapshot(label string) GMPSnapshot {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return GMPSnapshot{
		Label:      label,
		Goroutines: runtime.NumGoroutine(),
		Procs:      runtime.GOMAXPROCS(0),
		NumCPU:     runtime.NumCPU(),
		StackKB:    float64(m.StackInuse) / 1024,
		SysMB:      float64(m.Sys) / (1024 * 1024),
	}
}

func (s GMPSnapshot) Print() {
	fmt.Printf("[%-30s] G=%-6d P=%-3d NumCPU=%-3d StackInUse=%.1fKB  Sys=%.1fMB\n",
		s.Label, s.Goroutines, s.Procs, s.NumCPU, s.StackKB, s.SysMB)
}

func launchIOBoundGoroutines(count int, done <-chan struct{}) {
	for i := 0; i < count; i++ {
		go func() {
			time.Sleep(ioReadLatency)
			<-done
		}()
	}
}

func launchCPUBoundGoroutines(count int, data []byte, done <-chan struct{}) {
	for i := 0; i < count; i++ {
		go func() {
			for j := 0; j < checksumsPerGoroutine; j++ {
				sha256.Sum256(data)
			}
			<-done
		}()
	}
}

func main() {
	captureGMPSnapshot("startup").Print()

	done := make(chan struct{})

	launchIOBoundGoroutines(200, done)
	time.Sleep(20 * time.Millisecond)
	captureGMPSnapshot("200 IO-bound goroutines").Print()

	data := make([]byte, checksumBlockSize)
	launchCPUBoundGoroutines(50, data, done)
	time.Sleep(50 * time.Millisecond)
	captureGMPSnapshot("200 IO + 50 CPU goroutines").Print()

	launchIOBoundGoroutines(300, done)
	time.Sleep(20 * time.Millisecond)
	captureGMPSnapshot("500 IO + 50 CPU goroutines").Print()

	close(done)
	time.Sleep(50 * time.Millisecond)
	captureGMPSnapshot("all released").Print()
}
```

**What's happening here:** At each snapshot, we print G count (changes), P count (stays constant), and memory metrics. IO-bound goroutines simulate file/network reads; CPU-bound goroutines compute checksums. This demonstrates that G and stack memory scale together while P remains fixed.

**Key insight:** P is constant (set once at startup). G can grow to millions. StackInuse correlates with G count because each goroutine has its own stack. This status reporter pattern is directly useful for production instrumentation -- adding periodic GMP snapshots to your metrics helps diagnose goroutine leaks and memory growth.

### Verification
```bash
go run main.go
```
Expected output (memory values vary):
```
[startup                       ] G=1      P=8   NumCPU=8   StackInUse=32.0KB  Sys=7.2MB
[200 IO-bound goroutines       ] G=201    P=8   NumCPU=8   StackInUse=1664.0KB  Sys=12.1MB
[200 IO + 50 CPU goroutines    ] G=251    P=8   NumCPU=8   StackInUse=2080.0KB  Sys=14.5MB
[500 IO + 50 CPU goroutines    ] G=551    P=8   NumCPU=8   StackInUse=4544.0KB  Sys=18.3MB
[all released                  ] G=1      P=8   NumCPU=8   StackInUse=32.0KB  Sys=18.3MB
```

## Deep Dive: How P Acquisition Works

The GMP model has a subtle but important mechanism: work stealing. When a P's local run queue is empty, the M holding that P tries to steal work from other Ps' run queues. This ensures that idle Ps do not sit around while other Ps have goroutines waiting.

The scheduling cycle for an M looks like this:
1. Check local run queue on the current P
2. Check the global run queue (shared across all Ps)
3. Check the network poller (for goroutines waiting on I/O)
4. Attempt to steal work from another random P's run queue

This is why you do not need to manually distribute goroutines across Ps. The scheduler balances the load automatically. In a real server handling both CPU-intensive checksum verification and IO-heavy file serving, the scheduler ensures all cores stay busy without any manual tuning.

## Common Mistakes

### Confusing GOMAXPROCS with Goroutine Limit

**Wrong thinking:** "Setting GOMAXPROCS(4) means only 4 goroutines can exist."

**What happens:** GOMAXPROCS sets the number of Ps (logical processors), not the number of Gs. You can have millions of goroutines with GOMAXPROCS=1 -- they just run one at a time.

**Fix:** GOMAXPROCS controls parallelism (how many goroutines run simultaneously), not concurrency (how many goroutines exist).

### Assuming M Count Equals P Count

**Wrong thinking:** "There are always exactly GOMAXPROCS OS threads."

**What happens:** The runtime creates additional Ms when goroutines block in syscalls. The M count can grow well beyond GOMAXPROCS. A service doing heavy file I/O might have 50+ OS threads despite GOMAXPROCS=8.

**Fix:** Think of P as the parallelism limit for Go code execution. Ms are the actual OS threads, and their count floats based on demand from blocking syscalls.

### Using runtime.GOMAXPROCS in Production Code

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"net/http"
	"runtime"
)

func handleGreeting(w http.ResponseWriter, r *http.Request) {
	runtime.GOMAXPROCS(1) // terrible: affects the ENTIRE process
	fmt.Fprintf(w, "hello")
}

func main() {
	http.HandleFunc("/", handleGreeting)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("server failed: %v\n", err)
	}
}
```

**What happens:** `GOMAXPROCS` is a process-wide setting. Changing it in a request handler affects all goroutines in the process, not just the one handling this request. Every other connection handler suddenly runs on a single core.

**Correct approach:** Set GOMAXPROCS once at startup (via environment variable `GOMAXPROCS=N` or in `main`) or let the default apply. Never change it at runtime in business logic.

## Review

The three entities divide cleanly. A G is a unit of work created with `go`, cheap enough to have millions of at once. An M is an OS thread that actually executes G code. A P is a logical processor -- an execution slot -- whose count is fixed by `GOMAXPROCS`, which defaults to `runtime.NumCPU()` since Go 1.5. The invariant that ties them together is that a P must be held by an M for any Go code to run, which is exactly why `GOMAXPROCS` bounds parallelism but never the number of goroutines. The asymmetry the steps expose is that M count floats above P count: when an M blocks in a syscall it releases its P to another M, so a syscall-heavy service can carry far more OS threads than it has Ps, while the work-stealing scheduler keeps every P busy without any manual distribution.

You should be able to build the diagnostic yourself. Print the initial GMP status; launch a hundred CPU-bound goroutines computing SHA-256 over data blocks and print again; launch a hundred IO-bound goroutines that mostly sleep and print again; then run both together. In each phase you should be able to say why G climbs while P holds constant, and why the IO phase is the one that pushes M upward. If you can also explain why `GOMAXPROCS(1)` serializes four checksum workers onto one core without reducing the goroutine count, the model has landed.

## Resources

- [Go Scheduler Design Doc](https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw) -- the original design write-up for the GMP scheduler and its work-stealing run queues.
- [runtime.GOMAXPROCS](https://pkg.go.dev/runtime#GOMAXPROCS) -- the call that reads or sets the P count, and the note that it does not bound the number of goroutines.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) -- the counter the steps use to watch G rise and fall.
- [Scalable Go Scheduler (Dmitry Vyukov)](https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw) -- the deeper rationale for local run queues and M hand-off during syscalls.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-goroutine-stack-growth](../04-goroutine-stack-growth/04-goroutine-stack-growth.md)
