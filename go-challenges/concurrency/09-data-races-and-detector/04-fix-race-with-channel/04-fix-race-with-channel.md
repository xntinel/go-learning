# Exercise 4: Fix Race with Channel

A mutex fixes a data race by letting many goroutines touch the same variable and
serializing their access with a lock. The channel approach comes at the same
race from the opposite direction: instead of sharing a variable and guarding it,
you hand ownership of the variable to a single goroutine and have every other
goroutine communicate with it through a channel. This exercise rebuilds the hit
counter and the metrics collector from the mutex exercise using ownership and
the command pattern, then benchmarks the two side by side so you can feel where
each one belongs.

## What you'll build

```text
04-fix-race-with-channel/
  main.go        a channel-owned hit counter, a command-driven metrics
                 collector with request/reply, and a mutex-vs-channel benchmark
```

- Build: a race-free hit counter and metrics collector where one goroutine owns the state.
- Implement: a channel-based `channelHitCounter`, a `ChannelMetrics` owner loop driven by a `command` type (record/snapshot with a reply channel), and a mutex-vs-channel benchmark.
- Verify: `go run -race main.go` -- correct totals, zero race warnings.

### Why single ownership eliminates the race

This is the Go philosophy captured in the proverb: **"Don't communicate by sharing memory; share memory by communicating."**

When a single goroutine owns the data, there is no concurrent access, so there is no race. The channel serves as both the communication mechanism and the synchronization mechanism.

## Step 1 -- Channel-Based Hit Counter

Instead of locking a shared counter, send increment commands to a goroutine that owns the counter:

```go
package main

import (
	"fmt"
	"sync"
)

func channelHitCounter() int {
	increments := make(chan struct{}, 100)
	done := make(chan int)

	// Owner goroutine: the SOLE reader/writer of hitCount.
	go func() {
		hitCount := 0
		for range increments {
			hitCount++
		}
		done <- hitCount
	}()

	// Simulated HTTP handlers send increment signals.
	var wg sync.WaitGroup
	for handler := 0; handler < 100; handler++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := 0; req < 100; req++ {
				increments <- struct{}{}
			}
		}()
	}

	wg.Wait()
	close(increments)

	return <-done
}

func main() {
	result := channelHitCounter()
	fmt.Printf("Hit count: %d (expected 10000)\n", result)
}
```

Key observations:
- Only the owner goroutine reads and writes `hitCount`: no concurrent access
- `close(increments)` causes the `range` loop to exit
- `done <- hitCount` sends the final value back to the caller
- The buffered channel (capacity 100) reduces blocking

### Verification
```bash
go run -race main.go
```
Expected: 10000 with zero race warnings.

## Step 2 -- Channel-Based Metrics Collector

Build the same MetricsCollector from exercise 03, but using channels instead of a mutex. A single goroutine owns the map and processes commands sent through a channel:

```go
package main

import (
	"fmt"
	"sync"
)

type command struct {
	action   string
	endpoint string
	resultCh chan<- map[string]int
}

type ChannelMetrics struct {
	cmdCh chan command
}

func NewChannelMetrics() *ChannelMetrics {
	m := &ChannelMetrics{
		cmdCh: make(chan command, 256),
	}
	go m.run()
	return m
}

func (m *ChannelMetrics) run() {
	counters := make(map[string]int)
	for cmd := range m.cmdCh {
		switch cmd.action {
		case "record":
			counters[cmd.endpoint]++
		case "snapshot":
			snapshot := make(map[string]int, len(counters))
			for k, v := range counters {
				snapshot[k] = v
			}
			cmd.resultCh <- snapshot
		}
	}
}

func (m *ChannelMetrics) RecordRequest(endpoint string) {
	m.cmdCh <- command{action: "record", endpoint: endpoint}
}

func (m *ChannelMetrics) Snapshot() map[string]int {
	resultCh := make(chan map[string]int, 1)
	m.cmdCh <- command{action: "snapshot", resultCh: resultCh}
	return <-resultCh
}

func (m *ChannelMetrics) Close() {
	close(m.cmdCh)
}

func main() {
	metrics := NewChannelMetrics()
	var wg sync.WaitGroup

	endpoints := []string{"/api/users", "/api/orders", "/api/products", "/healthz"}

	for _, ep := range endpoints {
		for handler := 0; handler < 50; handler++ {
			wg.Add(1)
			go func(endpoint string) {
				defer wg.Done()
				for req := 0; req < 100; req++ {
					metrics.RecordRequest(endpoint)
				}
			}(ep)
		}
	}

	wg.Wait()

	fmt.Println("=== Channel-Based Metrics Collector ===")
	snapshot := metrics.Snapshot()
	total := 0
	for endpoint, count := range snapshot {
		fmt.Printf("  %-20s %d requests\n", endpoint, count)
		total += count
	}
	fmt.Printf("  %-20s %d requests\n", "TOTAL", total)
	fmt.Printf("\nExpected: 5000 per endpoint, 20000 total\n")

	metrics.Close()
}
```

This is the **command pattern**: callers send commands through a channel, and a single goroutine processes them sequentially. The map is never accessed concurrently because only one goroutine ever touches it.

For `Snapshot()`, the caller sends a command with a response channel. The owner processes the request, builds a copy of the map, and sends it back. This request-response pattern over channels is common in production Go code.

### Verification
```bash
go run -race main.go
```
Expected: 5000 per endpoint, 20000 total, zero race warnings.

## Step 3 -- Compare Mutex vs Channel

Both approaches solve the same problem. Which should you choose?

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	benchHandlers       = 100
	benchReqsPerHandler = 1000
	endpointCount       = 4
	channelBufferSize   = 256
)

// BenchmarkResult holds the timing outcome of a synchronization approach.
type BenchmarkResult struct {
	Label   string
	Elapsed time.Duration
}

// MutexMetricsBench measures mutex-based map protection throughput.
type MutexMetricsBench struct {
	mu       sync.Mutex
	counters map[string]int
}

func NewMutexMetricsBench() *MutexMetricsBench {
	return &MutexMetricsBench{counters: make(map[string]int)}
}

func (b *MutexMetricsBench) Run(handlers, reqs int) BenchmarkResult {
	var wg sync.WaitGroup
	start := time.Now()

	for h := 0; h < handlers; h++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ep := fmt.Sprintf("/api/endpoint-%d", id%endpointCount)
			for r := 0; r < reqs; r++ {
				b.mu.Lock()
				b.counters[ep]++
				b.mu.Unlock()
			}
		}(h)
	}

	wg.Wait()
	return BenchmarkResult{"Mutex", time.Since(start)}
}

// ChannelMetricsBench measures channel-based ownership throughput.
type ChannelMetricsBench struct {
	cmdCh chan string
	done  chan struct{}
}

func NewChannelMetricsBench() *ChannelMetricsBench {
	b := &ChannelMetricsBench{
		cmdCh: make(chan string, channelBufferSize),
		done:  make(chan struct{}),
	}
	go b.ownerLoop()
	return b
}

func (b *ChannelMetricsBench) ownerLoop() {
	counters := make(map[string]int)
	for ep := range b.cmdCh {
		counters[ep]++
	}
	close(b.done)
}

func (b *ChannelMetricsBench) Run(handlers, reqs int) BenchmarkResult {
	var wg sync.WaitGroup
	start := time.Now()

	for h := 0; h < handlers; h++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ep := fmt.Sprintf("/api/endpoint-%d", id%endpointCount)
			for r := 0; r < reqs; r++ {
				b.cmdCh <- ep
			}
		}(h)
	}

	wg.Wait()
	close(b.cmdCh)
	<-b.done
	return BenchmarkResult{"Channel", time.Since(start)}
}

func printComparisonGuide(mutex, channel BenchmarkResult) {
	fmt.Printf("  %-10s %v\n", mutex.Label+":", mutex.Elapsed)
	fmt.Printf("  %-10s %v\n", channel.Label+":", channel.Elapsed)
	fmt.Println()
	fmt.Println("When to use Mutex:")
	fmt.Println("  - Simple state protection (counters, maps)")
	fmt.Println("  - High-frequency updates where channel overhead matters")
	fmt.Println("  - Familiar lock-based reasoning")
	fmt.Println()
	fmt.Println("When to use Channel:")
	fmt.Println("  - Complex state machines with multiple operations")
	fmt.Println("  - When you want clear data ownership")
	fmt.Println("  - Pipeline architectures with stages")
	fmt.Println("  - When the command pattern makes the API clearer")
}

func main() {
	fmt.Println("=== Mutex vs Channel Comparison ===")
	fmt.Println()

	mutexBench := NewMutexMetricsBench()
	mutexResult := mutexBench.Run(benchHandlers, benchReqsPerHandler)

	channelBench := NewChannelMetricsBench()
	channelResult := channelBench.Run(benchHandlers, benchReqsPerHandler)

	printComparisonGuide(mutexResult, channelResult)
}
```

### Verification
```bash
go run main.go
```

For simple counters and maps, the mutex approach is typically faster because each channel send/receive has more overhead than a lock/unlock. The channel approach shines when the owned state is complex, the commands carry meaningful data, or the architecture is naturally a pipeline.

## Common Mistakes

### Forgetting to Close the Channel
```go
wg.Wait()
// forgot close(increments)
return <-done // DEADLOCK: owner is still ranging over increments
```
The owner goroutine blocks forever on `range increments`. Always `close(increments)` after all senders are done.

### Closing the Channel Before All Sends Complete
```go
go func() {
    defer wg.Done()
    for j := 0; j < 100; j++ {
        increments <- struct{}{}
    }
    close(increments) // BUG: other goroutines are still sending!
}()
```
Sending on a closed channel causes a **panic**. Close the channel once from the coordinating goroutine, after `wg.Wait()` confirms all senders have finished.

### Leaking the Owner Goroutine
If nothing ever closes the command channel, the owner goroutine runs forever. In a real server, call `Close()` during graceful shutdown to clean up.

### Not Considering Batching
For high-frequency counters, sending one signal per increment is expensive. Consider batching: accumulate counts locally and send a single batch update, or use a mutex instead.

## Review

The channel approach eliminates a race not by guarding shared memory but by
removing the sharing: one goroutine owns the state and is its only reader and
writer, and every other goroutine sends it a command over a channel. That is the
command pattern -- callers push `record` and `snapshot` commands into the owner's
channel, and the owner processes them one at a time, so the underlying map is
never touched concurrently and the race detector has nothing to find.
Request-response operations like `Snapshot` work by including a reply channel in
the command: the owner builds a copy of its state and sends it back on that
channel, so the caller gets an answer without ever reaching into the owned data.
The lifecycle discipline is the same as any producer/consumer channel -- `close`
happens once, from the coordinator, only after every sender is done, because
closing early or from a sender panics.

The benchmark answers the "which should I use" question honestly: for a plain
counter or map, a mutex is usually faster, because a lock and unlock is cheaper
than a channel send and receive. The channel design earns its overhead when the
owned state is a complex machine, when commands carry meaningful data, or when
the architecture is already a pipeline of stages -- anywhere the command-based
API makes the code clearer than scattered locks would. You should be able to run
`go run -race main.go` and see clean output, explain why `counters` never races,
trace exactly how `Snapshot()` gets its data back through a reply channel, and
say when you would reach for ownership over a mutex -- without rereading the
steps.

## Resources
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) -- the canonical write-up of the ownership philosophy this exercise applies.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- channel idioms including the command and reply-channel pattern.
- [Go Proverbs](https://go-proverbs.github.io/) -- the source of "don't communicate by sharing memory; share memory by communicating."

---

Back to [Concurrency](../../concurrency.md) | Next: [05-fix-race-with-atomic](../05-fix-race-with-atomic/05-fix-race-with-atomic.md)
