# Exercise 10: Goroutine Leak Detection

A goroutine leak is a goroutine that was started but will never terminate.
Each one holds its stack memory (~2-8 KB), any heap objects it references, and
a slot in the scheduler's run queue. In a long-running service handling
thousands of requests per minute, even one leak per request means tens of
thousands of orphaned goroutines within hours -- the process looks healthy
right up until it is OOM-killed. This exercise reproduces the three classic
leak patterns, makes each one visible with `runtime.NumGoroutine()`, and fixes
them.

## What you'll build

```text
10-goroutine-leak-detection/
  main.go        one runnable program per leak pattern, plus a reusable
                 leak detector that snapshots the live goroutine count
```

- Build: an API handler simulator that reproduces the three classic goroutine leaks, alongside a reusable leak detector.
- Implement: `leakyHandlerUnbuffered` versus the buffered `leakyHandler`, `callExternalService` guarded by a timeout, `leakyPoller` versus `fixedPoller` with a quit channel, and a `LeakDetector` that snapshots `runtime.NumGoroutine()`.
- Verify: `go run main.go`

### Why a goroutine leak reads as a memory leak

Goroutine leaks are one of the most common production issues in Go services.
They are insidious because the service appears to work correctly at first --
responses are fast, errors are low. But memory slowly climbs, GC pauses grow,
and eventually the process is OOM-killed. The symptoms look like a memory leak,
but the root cause is a concurrency bug.

The three patterns that cause almost all goroutine leaks are: (1) sending to a
channel that nobody reads, (2) waiting for a response that never arrives, and
(3) a loop without an exit condition. In this exercise, you build an API
handler simulator that demonstrates each pattern, detect the leaks with
`runtime.NumGoroutine()`, and fix them.

## Step 1 -- Leak Pattern: Blocked on Channel Send

The most common leak: a goroutine tries to send a result to a channel, but the receiver has already moved on. The goroutine blocks forever.

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

func leakyHandler(userID int) string {
	resultCh := make(chan string, 1)

	go func() {
		// Simulate slow database query
		time.Sleep(100 * time.Millisecond)
		resultCh <- fmt.Sprintf("user-%d profile data", userID)
	}()

	// Caller gives up after 30ms -- but the goroutine is still running
	select {
	case result := <-resultCh:
		return result
	case <-time.After(30 * time.Millisecond):
		return "timeout"
		// The goroutine above will finish its 100ms sleep,
		// send to the buffered channel, and then exit.
		// With a buffered channel of size 1, this is safe.
	}
}

func leakyHandlerUnbuffered(userID int) string {
	resultCh := make(chan string) // unbuffered -- THIS LEAKS

	go func() {
		time.Sleep(100 * time.Millisecond)
		resultCh <- fmt.Sprintf("user-%d profile data", userID) // blocks forever!
	}()

	select {
	case result := <-resultCh:
		return result
	case <-time.After(30 * time.Millisecond):
		return "timeout"
		// Nobody will ever read from resultCh.
		// The goroutine blocks on send FOREVER.
	}
}

func main() {
	fmt.Println("=== Leak Pattern: Blocked on Channel Send ===")
	fmt.Printf("  Goroutines at start: %d\n\n", runtime.NumGoroutine())

	// Simulate 10 API requests with the LEAKY (unbuffered) handler
	fmt.Println("  --- Using LEAKY handler (unbuffered channel) ---")
	for i := 0; i < 10; i++ {
		result := leakyHandlerUnbuffered(i)
		_ = result
	}
	time.Sleep(50 * time.Millisecond) // let goroutines settle
	fmt.Printf("  Goroutines after 10 leaky requests: %d (should be ~1, got %d leaked)\n",
		runtime.NumGoroutine(), runtime.NumGoroutine()-1)

	// Wait for leaked goroutines to finish their sleep and block
	time.Sleep(200 * time.Millisecond)
	fmt.Printf("  Goroutines after waiting: %d (still leaked -- they never exit)\n\n",
		runtime.NumGoroutine())

	// Now use the FIXED handler (buffered channel)
	fmt.Println("  --- Using FIXED handler (buffered channel) ---")
	baseline := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		result := leakyHandler(i)
		_ = result
	}
	time.Sleep(200 * time.Millisecond) // let goroutines complete
	fmt.Printf("  Goroutines after 10 fixed requests: %d (back to baseline: %d)\n",
		runtime.NumGoroutine(), baseline)

	fmt.Println()
	fmt.Println("  Fix: use a buffered channel (size 1) so the goroutine can send")
	fmt.Println("  even if nobody is listening, and then exit naturally.")
}
```

**What's happening here:** `leakyHandlerUnbuffered` creates an unbuffered channel and starts a goroutine to query a database. If the caller times out, nobody reads from the channel. The goroutine blocks on `resultCh <- ...` forever. After 10 requests, 10 goroutines are permanently stuck. `leakyHandler` fixes this by using a buffered channel of size 1: the goroutine can send its result even if nobody reads it, and then exits.

**Key insight:** When a goroutine might outlive its caller (due to timeouts, cancellation, or early returns), always use a buffered channel with capacity for the expected number of sends. This ensures the goroutine can complete its send and exit even if the result is never consumed.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Leak Pattern: Blocked on Channel Send ===
  Goroutines at start: 1

  --- Using LEAKY handler (unbuffered channel) ---
  Goroutines after 10 leaky requests: 11 (should be ~1, got 10 leaked)
  Goroutines after waiting: 11 (still leaked -- they never exit)

  --- Using FIXED handler (buffered channel) ---
  Goroutines after 10 fixed requests: 11 (back to baseline: 11)

  Fix: use a buffered channel (size 1) so the goroutine can send
  even if nobody is listening, and then exit naturally.
```

## Step 2 -- Leak Pattern: Waiting for a Response That Never Comes

A goroutine calls an external service and blocks on receive. If the service never responds and there is no timeout, the goroutine waits forever.

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

func callExternalService(serviceID int) <-chan string {
	ch := make(chan string, 1)
	go func() {
		if serviceID == 3 {
			// Service 3 is dead -- never sends a response
			select {} // block forever, simulating a hung connection
		}
		time.Sleep(20 * time.Millisecond)
		ch <- fmt.Sprintf("response from service-%d", serviceID)
	}()
	return ch
}

func leakyConsumer() {
	for i := 1; i <= 5; i++ {
		responseCh := callExternalService(i)
		result := <-responseCh // blocks forever if service never responds!
		fmt.Printf("    got: %s\n", result)
	}
}

func fixedConsumer() {
	const requestTimeout = 50 * time.Millisecond

	for i := 1; i <= 5; i++ {
		responseCh := callExternalService(i)

		select {
		case result := <-responseCh:
			fmt.Printf("    got: %s\n", result)
		case <-time.After(requestTimeout):
			fmt.Printf("    service-%d: timeout after %v\n", i, requestTimeout)
		}
	}
}

func main() {
	fmt.Println("=== Leak Pattern: No Response + No Timeout ===")
	fmt.Printf("  Goroutines at start: %d\n\n", runtime.NumGoroutine())

	// The leaky version would block forever on service 3,
	// so we show the fix instead and explain the problem.
	fmt.Println("  --- FIXED consumer (with timeout) ---")
	fixedConsumer()

	time.Sleep(100 * time.Millisecond)
	remaining := runtime.NumGoroutine()
	fmt.Printf("\n  Goroutines after fixed consumer: %d\n", remaining)

	if remaining > 2 {
		fmt.Printf("  Note: %d goroutine(s) still running from hung service-3.\n", remaining-1)
		fmt.Println("  The consumer moved on, but the hung goroutine in callExternalService")
		fmt.Println("  is still blocked on `select{}`. A real fix requires cancellation")
		fmt.Println("  propagation (context.Context) which is covered in section 06.")
	}

	fmt.Println()
	fmt.Println("  Without the timeout, the leaky consumer would block forever on service 3,")
	fmt.Println("  never reaching services 4 and 5. The goroutine for service 3 is stuck in")
	fmt.Println("  an empty select{}, consuming a scheduler slot permanently.")
	fmt.Println()
	fmt.Println("  Fix: ALWAYS use timeouts when waiting for external responses.")
	fmt.Println("  select + time.After is the minimum. context.WithTimeout is the standard.")
}
```

**What's happening here:** `callExternalService` simulates calling 5 services. Service 3 is dead and never sends a response. Without a timeout, `<-responseCh` blocks forever. The fixed version uses `select` with `time.After` to abandon the wait after 50ms.

**Key insight:** Every channel receive that depends on an external system must have a timeout. The pattern `select { case v := <-ch: ... case <-time.After(d): ... }` is the minimum protection. In production, use `context.WithTimeout` for propagation across function boundaries. A single missing timeout can leak one goroutine per request, which at 1000 req/s means 86 million leaked goroutines per day.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Leak Pattern: No Response + No Timeout ===
  Goroutines at start: 1

  --- FIXED consumer (with timeout) ---
    got: response from service-1
    got: response from service-2
    service-3: timeout after 50ms
    got: response from service-4
    got: response from service-5

  Goroutines after fixed consumer: 2
  Note: 1 goroutine(s) still running from hung service-3.
  The consumer moved on, but the hung goroutine in callExternalService
  is still blocked on `select{}`. A real fix requires cancellation
  propagation (context.Context) which is covered in section 06.

  Without the timeout, the leaky consumer would block forever on service 3,
  never reaching services 4 and 5. The goroutine for service 3 is stuck in
  an empty select{}, consuming a scheduler slot permanently.

  Fix: ALWAYS use timeouts when waiting for external responses.
  select + time.After is the minimum. context.WithTimeout is the standard.
```

## Step 3 -- Leak Pattern: Infinite Loop Without Exit

A goroutine in an infinite loop that lacks a quit signal or termination condition runs forever.

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

func leakyPoller(source string, results chan<- string) {
	// BUG: no way to stop this goroutine
	for {
		time.Sleep(50 * time.Millisecond)
		results <- fmt.Sprintf("[%s] data at %v", source, time.Now().Format("15:04:05.000"))
	}
}

func fixedPoller(source string, results chan<- string, quit <-chan struct{}) {
	for {
		select {
		case <-quit:
			fmt.Printf("  poller %q: shutting down\n", source)
			return
		default:
			time.Sleep(50 * time.Millisecond)
			select {
			case results <- fmt.Sprintf("[%s] data at %v", source, time.Now().Format("15:04:05.000")):
			case <-quit:
				fmt.Printf("  poller %q: shutting down\n", source)
				return
			}
		}
	}
}

func main() {
	fmt.Println("=== Leak Pattern: Infinite Loop Without Exit ===")
	fmt.Printf("  Goroutines at start: %d\n\n", runtime.NumGoroutine())

	// --- Demonstrate the leak ---
	fmt.Println("  --- LEAKY pollers (no quit signal) ---")
	leakyResults := make(chan string, 10)

	go leakyPoller("metrics", leakyResults)
	go leakyPoller("logs", leakyResults)
	go leakyPoller("traces", leakyResults)

	// Read a few results then "move on"
	for i := 0; i < 3; i++ {
		msg := <-leakyResults
		fmt.Printf("    %s\n", msg)
	}

	fmt.Printf("  Goroutines after leaky pollers (stopped reading): %d\n",
		runtime.NumGoroutine())
	fmt.Println("  The 3 pollers keep running and filling the buffer.")
	fmt.Println("  Once the buffer is full, they block on send -- leaked forever.")

	time.Sleep(300 * time.Millisecond) // let buffer fill
	fmt.Printf("  Goroutines after 300ms: %d (still leaked)\n\n",
		runtime.NumGoroutine())

	// --- Demonstrate the fix ---
	fmt.Println("  --- FIXED pollers (with quit channel) ---")
	fixedResults := make(chan string, 10)
	quit := make(chan struct{})

	go fixedPoller("metrics", fixedResults, quit)
	go fixedPoller("logs", fixedResults, quit)
	go fixedPoller("traces", fixedResults, quit)

	// Read a few results
	for i := 0; i < 3; i++ {
		msg := <-fixedResults
		fmt.Printf("    %s\n", msg)
	}

	// Signal all pollers to stop
	close(quit)
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  Goroutines after closing quit: %d\n", runtime.NumGoroutine())
	fmt.Println()
	fmt.Println("  Fix: every infinite loop must check a quit channel or context.")
	fmt.Println("  Pair every `go func()` with a way to stop it.")
}
```

**What's happening here:** Three leaky pollers run in infinite loops, sending data to a results channel. When the consumer stops reading, the pollers keep running until the buffer fills, then block on send forever. The fixed version adds a quit channel that each poller checks on every iteration.

**Key insight:** For every `go func()` you write, ask: "How does this goroutine stop?" If the answer is "it does not" or "when the process exits," you have a potential leak. Every long-running goroutine must have an explicit exit mechanism: a quit channel, a context cancellation, or a finite loop.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Leak Pattern: Infinite Loop Without Exit ===
  Goroutines at start: 1

  --- LEAKY pollers (no quit signal) ---
    [metrics] data at 10:30:15.123
    [logs] data at 10:30:15.125
    [traces] data at 10:30:15.128
  Goroutines after leaky pollers (stopped reading): 4
  The 3 pollers keep running and filling the buffer.
  Once the buffer is full, they block on send -- leaked forever.
  Goroutines after 300ms: 4 (still leaked)

  --- FIXED pollers (with quit channel) ---
    [metrics] data at 10:30:15.552
    [logs] data at 10:30:15.554
    [traces] data at 10:30:15.556
  poller "metrics": shutting down
  poller "logs": shutting down
  poller "traces": shutting down
  Goroutines after closing quit: 4

  Fix: every infinite loop must check a quit channel or context.
  Pair every `go func()` with a way to stop it.
```

## Step 4 -- Leak Detection Dashboard

Build a reusable leak detector that monitors goroutine count over time and reports anomalies.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

type GoroutineSnapshot struct {
	Timestamp time.Time
	Count     int
	Delta     int
}

type LeakDetector struct {
	snapshots []GoroutineSnapshot
	baseline  int
}

func NewLeakDetector() *LeakDetector {
	return &LeakDetector{
		baseline: runtime.NumGoroutine(),
	}
}

func (ld *LeakDetector) TakeSnapshot(label string) {
	count := runtime.NumGoroutine()
	delta := count - ld.baseline
	snap := GoroutineSnapshot{
		Timestamp: time.Now(),
		Count:     count,
		Delta:     delta,
	}
	ld.snapshots = append(ld.snapshots, snap)

	status := "OK"
	if delta > 5 {
		status = "LEAK?"
	}
	fmt.Printf("  [%s] %-30s goroutines: %d (delta: %+d)\n",
		status, label, count, delta)
}

func (ld *LeakDetector) Report() {
	fmt.Println()
	fmt.Println("  --- Leak Detection Report ---")
	maxCount := 0
	maxDelta := 0
	for _, s := range ld.snapshots {
		if s.Count > maxCount {
			maxCount = s.Count
		}
		if s.Delta > maxDelta {
			maxDelta = s.Delta
		}
	}

	finalDelta := ld.snapshots[len(ld.snapshots)-1].Delta
	fmt.Printf("  Baseline:    %d goroutines\n", ld.baseline)
	fmt.Printf("  Peak:        %d goroutines (delta: +%d)\n", maxCount, maxDelta)
	fmt.Printf("  Final:       %d goroutines (delta: %+d)\n",
		ld.snapshots[len(ld.snapshots)-1].Count, finalDelta)

	if finalDelta > 2 {
		fmt.Printf("  VERDICT:     LEAK DETECTED -- %d goroutines not cleaned up\n", finalDelta)
	} else {
		fmt.Println("  VERDICT:     CLEAN -- all goroutines properly terminated")
	}
}

func simulateLeakyEndpoint() {
	ch := make(chan string)
	go func() {
		time.Sleep(50 * time.Millisecond)
		ch <- "result" // blocks if nobody reads
	}()
	// "Forget" to read from ch -- goroutine leaks
}

func simulateCleanEndpoint(quit <-chan struct{}) {
	ch := make(chan string, 1) // buffered: goroutine can send and exit
	go func() {
		time.Sleep(50 * time.Millisecond)
		select {
		case ch <- "result":
		case <-quit:
		}
	}()
	// Even if we don't read, the goroutine exits after sending to buffer
}

func main() {
	fmt.Println("=== Goroutine Leak Detection Dashboard ===")
	fmt.Println()
	ld := NewLeakDetector()

	ld.TakeSnapshot("startup")

	// Phase 1: simulate leaky requests
	fmt.Println()
	fmt.Println("  Phase 1: sending 20 leaky requests...")
	for i := 0; i < 20; i++ {
		simulateLeakyEndpoint()
	}
	time.Sleep(100 * time.Millisecond)
	ld.TakeSnapshot("after 20 leaky requests")

	// Phase 2: more leaky requests
	fmt.Println()
	fmt.Println("  Phase 2: sending 20 more leaky requests...")
	for i := 0; i < 20; i++ {
		simulateLeakyEndpoint()
	}
	time.Sleep(100 * time.Millisecond)
	ld.TakeSnapshot("after 40 total leaky requests")

	// Phase 3: clean requests using fixed pattern
	quit := make(chan struct{})
	fmt.Println()
	fmt.Println("  Phase 3: sending 20 clean requests...")
	for i := 0; i < 20; i++ {
		simulateCleanEndpoint(quit)
	}
	time.Sleep(100 * time.Millisecond)
	close(quit)
	time.Sleep(50 * time.Millisecond)
	ld.TakeSnapshot("after 20 clean requests")

	fmt.Println()
	fmt.Println(strings.Repeat("-", 60))
	ld.Report()

	fmt.Println()
	fmt.Println("  In production, integrate runtime.NumGoroutine() into your")
	fmt.Println("  metrics (Prometheus, Datadog). Alert when the count grows")
	fmt.Println("  monotonically over time -- that is a leak.")
}
```

**What's happening here:** The `LeakDetector` takes goroutine count snapshots at different points and computes deltas from the baseline. Phase 1 and 2 use the leaky pattern (unbuffered channel, no reader), accumulating 40 leaked goroutines. Phase 3 uses the fixed pattern (buffered channel + quit signal), which leaves no leaks. The final report identifies the leak.

**Key insight:** In production, export `runtime.NumGoroutine()` as a Prometheus gauge or Datadog metric. A healthy service has a stable goroutine count that rises and falls with traffic. A leaking service shows monotonically increasing goroutine count. Set an alert on sustained growth over 10-15 minutes. This single metric catches the majority of goroutine leak bugs.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Goroutine Leak Detection Dashboard ===

  [OK] startup                        goroutines: 1 (delta: +0)

  Phase 1: sending 20 leaky requests...
  [LEAK?] after 20 leaky requests       goroutines: 21 (delta: +20)

  Phase 2: sending 20 more leaky requests...
  [LEAK?] after 40 total leaky requests goroutines: 41 (delta: +40)

  Phase 3: sending 20 clean requests...
  [LEAK?] after 20 clean requests       goroutines: 41 (delta: +40)

------------------------------------------------------------
  --- Leak Detection Report ---
  Baseline:    1 goroutines
  Peak:        41 goroutines (delta: +40)
  Final:       41 goroutines (delta: +40)
  VERDICT:     LEAK DETECTED -- 40 goroutines not cleaned up

  In production, integrate runtime.NumGoroutine() into your
  metrics (Prometheus, Datadog). Alert when the count grows
  monotonically over time -- that is a leak.
```

## Common Mistakes

### Using Unbuffered Channels in Fire-and-Forget Goroutines

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

func fetchAsync(url string) <-chan string {
	ch := make(chan string) // unbuffered!
	go func() {
		time.Sleep(100 * time.Millisecond)
		ch <- fmt.Sprintf("data from %s", url) // blocks if caller timed out
	}()
	return ch
}

func main() {
	ch := fetchAsync("https://api.example.com")

	select {
	case result := <-ch:
		fmt.Println(result)
	case <-time.After(50 * time.Millisecond):
		fmt.Println("timeout -- but the goroutine is now leaked!")
	}

	time.Sleep(200 * time.Millisecond)
	fmt.Printf("Leaked goroutines: %d\n", runtime.NumGoroutine()-1)
}
```

**What happens:** The caller times out after 50ms, but the goroutine runs for 100ms and then blocks on the unbuffered send forever. In a server handling 1000 req/s, this leaks 1000 goroutines per second.

**Correct -- buffered channel:**
```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

func fetchAsync(url string) <-chan string {
	ch := make(chan string, 1) // buffered: goroutine can send and exit
	go func() {
		time.Sleep(100 * time.Millisecond)
		ch <- fmt.Sprintf("data from %s", url)
	}()
	return ch
}

func main() {
	ch := fetchAsync("https://api.example.com")

	select {
	case result := <-ch:
		fmt.Println(result)
	case <-time.After(50 * time.Millisecond):
		fmt.Println("timeout -- goroutine will complete and exit cleanly")
	}

	time.Sleep(200 * time.Millisecond)
	fmt.Printf("Goroutines: %d (clean)\n", runtime.NumGoroutine())
}
```

### Forgetting the Quit Path in Long-Running Goroutines

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

func backgroundSync() {
	for {
		time.Sleep(100 * time.Millisecond)
		// sync data...
	}
}

func main() {
	go backgroundSync() // runs FOREVER -- no way to stop it
	time.Sleep(300 * time.Millisecond)
	fmt.Printf("Goroutines: %d (backgroundSync will never stop)\n",
		runtime.NumGoroutine())
}
```

**What happens:** `backgroundSync` has no exit condition. In a test, it leaks between test cases. In production, if you restart a component that owned this goroutine, the old goroutine keeps running alongside the new one.

**Fix:** Accept a quit channel or context.Context and check it on every iteration.

## Review

Every goroutine leak in this exercise reduces to one broken assumption: a
goroutine was started without a guaranteed path to termination. The three
shapes it takes are a send onto a channel nobody will read, a receive with no
timeout, and an infinite loop with no quit signal -- and each has a matching
fix. A buffered channel of size one lets a goroutine that outlived its caller
finish its send and exit; a `select` with `time.After` (or a context deadline)
bounds every external wait; a quit channel or context gives every long-running
loop an exit. `runtime.NumGoroutine()` is the instrument that makes the leak
observable: a healthy service holds a stable count that rises and falls with
traffic, while monotonic growth is the signature of a leak. At 1000 requests
per second, a single leaked goroutine per request is 86 million orphans a day.

To prove you can find these on your own, build a leak audit tool: three
simulated handlers -- one leaking through an unbuffered channel, one through a
missing timeout, one clean -- each run fifteen times in a loop, with a
`runtime.NumGoroutine()` snapshot taken before and after each batch. The delta
divided by the call count is the per-call leak rate, and that number is enough
to label each handler by severity and to name which of the three patterns it
suffers from. If you can write that tool without re-reading the steps, you can
recognize the same leaks in production code.

## Resources
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) -- the counter you sample to detect leaks; export it as a gauge.
- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how to give every pipeline stage a shutdown path so goroutines cannot leak.
- [Uber Go Style Guide: Don't fire-and-forget goroutines](https://github.com/uber-go/guide/blob/master/style.md) -- the rule that every goroutine needs a known point of exit.
- [goleak: Goroutine leak detector for tests](https://github.com/uber-go/goleak) -- a library that fails a test when goroutines outlive it.

---

Back to [Concurrency](../../concurrency.md) | Next: [11-goroutine-error-handling](../11-goroutine-error-handling/11-goroutine-error-handling.md)
