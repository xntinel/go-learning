# Exercise 2: Race Detector Flag

In the previous exercise, the hit counter produced wrong numbers. But how do you
find a race in a codebase with thousands of lines? You cannot rely on observing
wrong output, because data races are non-deterministic: the program might appear
correct all through testing and then crash or corrupt state in production. You
need a tool that finds the race whether or not it happened to manifest on this
particular run.

## What you'll build

```text
02-race-detector-flag/
  main.go        hit-counter run under -race, a CI guide, and a caught race
                 next to a channel-synchronized write the detector clears
```

- Build: a set of programs that expose, report, and explain data races through the `-race` flag.
- Implement: a `HitCounter`/`RacyCounter` with unsynchronized increments, a `SynchronizedCounter` whose channel close creates a happens-before edge, and a `CIGuide` listing the CI commands.
- Verify: `go run -race main.go`.

### Why you cannot eyeball a race

Go ships with a built-in race detector based on Google's **ThreadSanitizer** technology. When enabled, it instruments every memory access and tracks happens-before relationships between goroutine operations. If two accesses to the same memory lack a happens-before edge and at least one is a write, the detector reports a race.

Go ships with a built-in race detector based on Google's **ThreadSanitizer** technology. When enabled, it instruments every memory access and tracks happens-before relationships between goroutine operations. If two accesses to the same memory lack a happens-before edge and at least one is a write, the detector reports a race.

Key properties:
- **Zero false positives**: every reported race is a real bug
- **Dynamic analysis**: it only detects races that occur during that specific execution
- **Performance cost**: roughly 5-10x CPU and 5-10x memory overhead
- **Use case**: development and CI, not production

## Step 1 -- Detect the Race in the Hit Counter

Create `main.go` with the hit counter from exercise 01, then run it with the race detector:

```go
package main

import (
	"fmt"
	"sync"
)

const (
	handlerCount  = 100
	requestsPerHandler = 100
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

func (hc *HitCounter) Expected() int {
	return hc.handlers * hc.reqsPerHandler
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
				hitCount++
			}
		}()
	}

	wg.Wait()
	return hitCount
}

func main() {
	fmt.Println("=== Race Detector Demo ===")
	counter := NewHitCounter(handlerCount, requestsPerHandler)
	result := counter.CountHits()
	fmt.Printf("Hit count: %d (expected %d)\n", result, counter.Expected())
}
```

### Verification

First, run without the race detector to see the wrong result:
```bash
go run main.go
```

Now run with the race detector enabled:
```bash
go run -race main.go
```

The race detector prints a report like this:
```
==================
WARNING: DATA RACE
Read at 0x00c00001e0b8 by goroutine 7:
  main.racyHitCounter.func1()
      /path/to/main.go:16 +0x38

Previous write at 0x00c00001e0b8 by goroutine 8:
  main.racyHitCounter.func1()
      /path/to/main.go:16 +0x4e

Goroutine 7 (running) created at:
  main.racyHitCounter()
      /path/to/main.go:12 +0x7c

Goroutine 8 (running) created at:
  main.racyHitCounter()
      /path/to/main.go:12 +0x7c
==================
```

The program still runs to completion despite the warning. The detector reports the race but does not stop execution.

## Step 2 -- Read the Race Report

The race detector report has four sections. Knowing how to read them is essential for debugging production race conditions:

**Section 1: "Read at 0x... by goroutine N"**
- The memory address (`0x00c00001e0b8`) is the `hitCount` variable
- `goroutine 7` is one of the handler goroutines
- `main.go:16` points to the exact line: `hitCount++`
- The `+0x38` is the offset within the function (useful for assembly debugging)

**Section 2: "Previous write at 0x... by goroutine M"**
- The SAME memory address, but a DIFFERENT goroutine
- "Previous" means the detector saw this access first, not that it happened first chronologically
- Same line `main.go:16` confirms both goroutines hit the same `hitCount++`

**Section 3 and 4: "Goroutine N/M created at:"**
- Stack traces showing WHERE each goroutine was launched
- `main.go:12` points to the `go func(id int)` line
- These traces let you find the `go` statement responsible

**Reading strategy**: Start at Section 1 (what variable is being raced), then check Section 2 (who else touches it), then Sections 3-4 (where those goroutines came from).

## Step 3 -- Add Race Detection to Your CI Pipeline

The `-race` flag works with multiple Go commands. The most important use is in CI:

```go
package main

import "fmt"

// CIGuide holds the reference commands for race detection in CI pipelines.
type CIGuide struct {
	Commands []CICommand
}

// CICommand represents a single CI pipeline command with its description.
type CICommand struct {
	Description string
	Command     string
}

func NewCIGuide() *CIGuide {
	return &CIGuide{
		Commands: []CICommand{
			{"Run all tests with race detection", "go test -race ./..."},
			{"Build an instrumented binary for integration tests", "go build -race -o myservice ./cmd/server"},
			{"Log race reports to a file", `GORACE="log_path=race.log" go test -race ./...`},
		},
	}
}

func (g *CIGuide) Print() {
	fmt.Println("=== Race Detection in CI ===")
	fmt.Println()
	fmt.Println("Add to your CI pipeline (GitHub Actions, GitLab CI, etc.):")
	fmt.Println()
	for _, cmd := range g.Commands {
		fmt.Printf("  # %s\n", cmd.Description)
		fmt.Printf("  %s\n\n", cmd.Command)
	}
	printOverheadWarning()
}

func printOverheadWarning() {
	fmt.Println("Performance overhead:")
	fmt.Println("  - CPU:    ~10x slower")
	fmt.Println("  - Memory: ~5-10x more")
	fmt.Println("  - Binary: significantly larger")
	fmt.Println()
	fmt.Println("NEVER deploy a -race binary to production.")
	fmt.Println("Use it in dev, test, and CI only.")
}

func main() {
	guide := NewCIGuide()
	guide.Print()
}
```

A typical CI configuration:
```yaml
# .github/workflows/test.yml (relevant step)
- name: Test with race detector
  run: go test -race -count=1 ./...
```

The `-count=1` flag disables test caching, ensuring every test actually runs with the race detector on every pipeline execution. Without it, cached test results skip the race detection.

## Step 4 -- Understand What the Detector Can and Cannot Find

The race detector only finds races that **execute during the test run**. Create this program to see both a detected race and proper synchronization:

```go
package main

import (
	"fmt"
	"sync"
)

const racyWorkers = 10

// RacyCounter demonstrates a data race the detector WILL catch.
// BUG: hitCount is shared across goroutines without synchronization.
type RacyCounter struct {
	workers int
}

func NewRacyCounter(workers int) *RacyCounter {
	return &RacyCounter{workers: workers}
}

// Count increments a shared variable from multiple goroutines.
// DATA RACE: hitCount has no synchronization.
func (rc *RacyCounter) Count() int {
	hitCount := 0
	var wg sync.WaitGroup

	for i := 0; i < rc.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hitCount++ // RACE: no synchronization
		}()
	}

	wg.Wait()
	return hitCount
}

// SynchronizedCounter demonstrates a write that the detector will NOT flag,
// because the channel close establishes a happens-before edge.
type SynchronizedCounter struct {
	value int
}

func NewSynchronizedCounter() *SynchronizedCounter {
	return &SynchronizedCounter{}
}

// Set writes a value in a goroutine and waits via channel synchronization.
func (sc *SynchronizedCounter) Set(val int) int {
	done := make(chan struct{})

	go func() {
		sc.value = val // write
		close(done)    // synchronization point
	}()

	<-done // happens-after close(done)
	return sc.value
}

func printDetectorLimitations() {
	fmt.Println("The detector uses happens-before analysis, not timing.")
	fmt.Println("Channel close -> receive establishes a happens-before edge.")
	fmt.Println()
	fmt.Println("What the detector CANNOT find:")
	fmt.Println("  - Deadlocks")
	fmt.Println("  - Livelocks")
	fmt.Println("  - Goroutine leaks")
	fmt.Println("  - Logical race conditions (timing-dependent behavior that is technically race-free)")
	fmt.Println("  - Races in code paths that were not exercised during the run")
}

func main() {
	fmt.Println("=== What the Detector Finds ===")
	fmt.Println()

	racy := NewRacyCounter(racyWorkers)
	fmt.Printf("Racy counter: %d (WILL trigger race warning)\n", racy.Count())

	synced := NewSynchronizedCounter()
	fmt.Printf("Synchronized: %d (NO race warning)\n", synced.Set(42))

	fmt.Println()
	printDetectorLimitations()
}
```

### Verification
```bash
go run -race main.go
```

The `racyCounter` function triggers `WARNING: DATA RACE`. The `synchronizedCounter` function produces no warning because the channel close establishes a happens-before relationship.

## Common Mistakes

### Ignoring Race Detector Warnings
The race detector has **zero false positives**. Every reported race is a real bug. Never dismiss a race report as a "false alarm." Fix every race it reports.

### Assuming No Report Means No Races
The race detector only finds races that **occur during execution**. If a code path is not exercised, its races will not be found. Write comprehensive tests and run them with `-race` to maximize coverage.

### Running -race in Production
The race detector adds significant overhead (5-10x slower, 5-10x more memory). Use it in development and CI, not in production binaries. A `-race` binary under production load will be unacceptably slow and use too much memory.

### Thinking the Detector Finds All Concurrency Bugs
The race detector finds **data races** specifically. It does NOT find deadlocks, livelocks, starvation, goroutine leaks, or logical race conditions where the program is technically race-free but still has timing-dependent behavior.

## Review

The `-race` flag turns on Go's race detector, a ThreadSanitizer-based instrument
that works with `go run`, `go test`, and `go build`. It does not guess: it tracks
happens-before edges between goroutine operations and reports a race only when
two accesses to the same memory lack such an edge and at least one is a write.
Its reports are worth learning to read -- the conflicting accesses, the
goroutines involved, the source lines, and the stacks where those goroutines were
launched -- because that is exactly the trail you follow to the offending `go`
statement. Two properties bound its usefulness. It has zero false positives, so
every report is a real bug you must fix; but it is a dynamic tool, so it only
finds races that actually execute during the run, which is why comprehensive
tests run under `-race -count=1` in CI matter. The instrumentation costs roughly
5-10x in CPU and memory, so it belongs in development and CI, never in a
production binary.

Make sure you can say what "happens-before" means and how it underpins
detection, name three operations that establish it in Go (a channel send paired
with its receive, a channel close observed by a receiver, a mutex unlock before a
later lock -- with `sync.WaitGroup` and `sync.Once` doing the same), state plainly
that the detector produces no false positives yet can miss real races on
unexercised paths, and explain why `-count=1` belongs on `go test -race` in CI:
without it, cached results let a passing package skip the instrumented run
entirely.

## Resources
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) -- the tool's design and how to turn it on.
- [Go Memory Model](https://go.dev/ref/mem) -- the happens-before rules the detector enforces.
- [ThreadSanitizer Algorithm](https://github.com/google/sanitizers/wiki/ThreadSanitizerAlgorithm) -- how the underlying detection actually works.
- [Go Command: Testing Flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) -- where `-race`, `-count`, and friends sit among the `go test` flags.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-fix-race-with-mutex](../03-fix-race-with-mutex/03-fix-race-with-mutex.md)
