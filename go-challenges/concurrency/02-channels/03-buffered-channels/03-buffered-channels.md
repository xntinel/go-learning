# Exercise 3: Buffered Channels

Consider a logging pipeline. Your application goroutines generate log entries in
bursts -- a single HTTP request might emit five log lines in quick succession --
but the writer flushes to disk slowly, and disk I/O is orders of magnitude
slower than memory. With an unbuffered channel every log call blocks the caller
until the writer finishes the previous entry, so the whole application runs at
the speed of the disk. A buffered channel breaks that coupling: senders drop
entries into an internal queue and continue, blocking only when the queue is
full. This exercise builds a logging buffer to make the blocking rules, `len`/`cap`
inspection, and the buffered-versus-unbuffered timing difference concrete.

## What you'll build

```text
03-buffered-channels/
  main.go        buffered log channel: burst absorption, backpressure on
                 a full buffer, len/cap inspection, producer/consumer timing
```

- Build: a buffered logging pipeline that absorbs bursts and applies backpressure.
- Implement: a buffered log channel with `writeLogEntries`/`drainLogBuffer`, a `slowLogWriter` that demonstrates a full-buffer block, `printBufferState` using `len`/`cap`, and a `LogPipeline` pairing a fast producer with a slow flusher.
- Verify: `go run main.go`

### Why a buffer decouples producer from consumer

Buffered channels have an internal queue: a send only blocks when the buffer is full, and a receive only blocks when the buffer is empty. Application goroutines "drop off" log entries and continue immediately, as long as the buffer has space. When the writer falls behind and the buffer fills up, senders block -- this is called *backpressure*, and it prevents memory from growing without bound.

This is the fundamental tradeoff: buffered channels decouple the timing of producers and consumers, absorbing bursts while still applying backpressure when the consumer is overwhelmed.

## Step 1 -- A Logging Buffer

Create a buffered channel to model a log buffer. Application code can write several log entries without blocking, and the log writer consumes them at its own pace.

```go
package main

import "fmt"

const logBufferCapacity = 5

// writeLogEntries sends each entry into the buffered channel.
// None of these block because the buffer has capacity.
func writeLogEntries(buffer chan<- string, entries []string) {
	for _, entry := range entries {
		buffer <- entry
	}
}

// drainLogBuffer receives and prints all entries currently in the buffer.
func drainLogBuffer(buffer chan string) {
	for len(buffer) > 0 {
		entry := <-buffer
		fmt.Println("  Written:", entry)
	}
}

func main() {
	logBuffer := make(chan string, logBufferCapacity)

	entries := []string{
		"[INFO] request received: GET /api/users",
		"[INFO] auth token validated",
		"[INFO] query executed: SELECT * FROM users",
		"[INFO] response sent: 200 OK (12ms)",
	}
	writeLogEntries(logBuffer, entries)

	fmt.Printf("Log buffer: %d/%d entries\n", len(logBuffer), cap(logBuffer))
	drainLogBuffer(logBuffer)
}
```

Key difference from unbuffered: you can send four values *without* any goroutine receiving them. The buffer holds the values until they are consumed.

### Verification
```bash
go run main.go
# Expected:
#   Log buffer: 4/5 entries
#   Written: [INFO] request received: GET /api/users
#   Written: [INFO] auth token validated
#   Written: [INFO] query executed: SELECT * FROM users
#   Written: [INFO] response sent: 200 OK (12ms)
```

## Step 2 -- Backpressure: What Happens When the Buffer Is Full

Fill the log buffer completely, then try to write one more entry. The sender blocks until the writer drains at least one entry -- this is backpressure protecting you from unbounded memory growth.

```go
package main

import (
	"fmt"
	"time"
)

const backpressureBufferSize = 2
const writerDrainDelay = 500 * time.Millisecond

// slowLogWriter simulates a log writer that drains one entry after a delay.
func slowLogWriter(buffer <-chan string, delay time.Duration) {
	time.Sleep(delay)
	entry := <-buffer
	fmt.Printf("Writer drained: %s\n", entry)
}

func main() {
	logBuffer := make(chan string, backpressureBufferSize)
	logBuffer <- "[ERROR] connection timeout"
	logBuffer <- "[ERROR] retry failed"
	fmt.Printf("Buffer state: %d/%d (full)\n", len(logBuffer), cap(logBuffer))

	go slowLogWriter(logBuffer, writerDrainDelay)

	fmt.Println("App: writing 3rd log entry (will block -- buffer full)...")
	logBuffer <- "[WARN] circuit breaker tripped"
	fmt.Println("App: 3rd entry written (writer made room)")
}
```

The send blocks for ~500ms because the buffer is full. Once the writer receives a value and makes room, the send completes. Without this backpressure, the application would silently drop logs or consume memory without bound.

### Verification
```bash
go run main.go
# Expected:
#   Buffer state: 2/2 (full)
#   App: writing 3rd log entry (will block -- buffer full)...
#   Writer drained: [ERROR] connection timeout
#   App: 3rd entry written (writer made room)
```

## Step 3 -- Monitoring the Buffer with len() and cap()

`len(ch)` returns the number of values currently in the buffer. `cap(ch)` returns the total capacity. These are useful for diagnostics (dashboards, metrics) but should NOT be used for synchronization -- the values change between checking and acting in concurrent code.

```go
package main

import "fmt"

const monitorBufferCapacity = 5

// printBufferState prints the current fill level and total capacity.
func printBufferState(label string, buffer chan string) {
	fmt.Printf("%-12s %d/%d\n", label, len(buffer), cap(buffer))
}

func main() {
	logBuffer := make(chan string, monitorBufferCapacity)
	printBufferState("Empty:", logBuffer)

	logBuffer <- "[INFO] startup"
	logBuffer <- "[INFO] ready"
	printBufferState("After 2:", logBuffer)

	<-logBuffer
	printBufferState("After drain:", logBuffer)

	logBuffer <- "[WARN] high latency"
	logBuffer <- "[ERROR] timeout"
	logBuffer <- "[ERROR] retry"
	logBuffer <- "[INFO] recovered"
	printBufferState("After burst:", logBuffer)
}
```

### Verification
```bash
go run main.go
# Expected:
#   Empty:       0/5
#   After 2:     2/5
#   After drain: 1/5
#   After burst: 5/5
```

## Step 4 -- Unbuffered vs Buffered: The Timing Difference

This comparison demonstrates why buffered channels matter for logging. With an unbuffered channel, the application waits for each log write. With a buffered channel, the application writes all logs instantly and continues its work.

```go
package main

import (
	"fmt"
	"time"
)

const (
	logEntryCount       = 5
	simulatedDiskWrite  = 100 * time.Millisecond
)

// slowDiskWriter simulates a log writer that flushes each entry to disk.
func slowDiskWriter(entries <-chan string, count int) {
	for i := 0; i < count; i++ {
		entry := <-entries
		_ = entry
		time.Sleep(simulatedDiskWrite)
	}
}

// benchmarkUnbuffered measures how long the app is blocked when writing
// to an unbuffered channel backed by a slow consumer.
func benchmarkUnbuffered() {
	fmt.Println("=== Unbuffered (app blocks each time) ===")
	unbuffered := make(chan string)
	start := time.Now()

	go slowDiskWriter(unbuffered, logEntryCount)

	for i := 1; i <= logEntryCount; i++ {
		unbuffered <- fmt.Sprintf("[INFO] request %d", i)
		fmt.Printf("  Logged request %d at +%v\n", i, time.Since(start).Round(time.Millisecond))
	}
	fmt.Printf("  Total: %v (app was blocked by disk)\n\n", time.Since(start).Round(time.Millisecond))
}

// benchmarkBuffered measures how quickly the app can write all entries
// when the buffer has enough capacity for the entire burst.
func benchmarkBuffered() {
	fmt.Println("=== Buffered (cap=5, app writes instantly) ===")
	buffered := make(chan string, logEntryCount)
	start := time.Now()

	for i := 1; i <= logEntryCount; i++ {
		buffered <- fmt.Sprintf("[INFO] request %d", i)
		fmt.Printf("  Logged request %d at +%v\n", i, time.Since(start).Round(time.Millisecond))
	}
	fmt.Printf("  Total: %v (app continued immediately)\n", time.Since(start).Round(time.Millisecond))

	// Drain buffered entries (log writer catches up later).
	for len(buffered) > 0 {
		<-buffered
	}
}

func main() {
	benchmarkUnbuffered()
	benchmarkBuffered()
}
```

### Verification
```bash
go run main.go
# Unbuffered logs are spaced ~100ms apart (blocked by disk write)
# Buffered logs complete in <1ms (all 5 fit in buffer)
```

## Step 5 -- Full Logging Pipeline with Backpressure

A realistic logging pipeline where the application generates log entries 3x faster than the writer can flush them. Watch the buffer fill up, block the fast producer, then drain as the writer catches up.

```go
package main

import (
	"fmt"
	"time"
)

const (
	pipelineBufferSize = 3
	appProduceRate     = 50 * time.Millisecond
	writerFlushRate    = 150 * time.Millisecond
)

// LogPipeline models a log pipeline with a buffered channel between
// a fast producer and a slow writer.
type LogPipeline struct {
	buffer chan string
	done   chan struct{}
}

// NewLogPipeline creates a pipeline with the given buffer capacity.
func NewLogPipeline(capacity int) *LogPipeline {
	return &LogPipeline{
		buffer: make(chan string, capacity),
		done:   make(chan struct{}),
	}
}

// ProduceEntries generates log entries at the given rate and closes
// the buffer when all entries have been queued.
func (lp *LogPipeline) ProduceEntries(levels []string, rate time.Duration) {
	for i, level := range levels {
		entry := fmt.Sprintf("[%s] event-%d", level, i+1)
		lp.buffer <- entry
		fmt.Printf("App queued:  %-25s | buffer: %d/%d\n",
			entry, len(lp.buffer), cap(lp.buffer))
		time.Sleep(rate)
	}
	close(lp.buffer)
}

// FlushEntries drains the buffer at the given rate, simulating slow disk writes.
func (lp *LogPipeline) FlushEntries(rate time.Duration) {
	for entry := range lp.buffer {
		fmt.Printf("Writer flush: %-25s | buffer: %d/%d\n",
			entry, len(lp.buffer), cap(lp.buffer))
		time.Sleep(rate)
	}
	lp.done <- struct{}{}
}

// Wait blocks until the writer finishes flushing all entries.
func (lp *LogPipeline) Wait() {
	<-lp.done
}

func main() {
	pipeline := NewLogPipeline(pipelineBufferSize)

	levels := []string{"INFO", "WARN", "ERROR", "DEBUG", "INFO",
		"ERROR", "INFO", "WARN", "INFO", "INFO"}

	go pipeline.ProduceEntries(levels, appProduceRate)
	go pipeline.FlushEntries(writerFlushRate)

	pipeline.Wait()
	fmt.Println("All log entries flushed")
}
```

### Verification
```bash
go run main.go
# You will see the buffer fill to 3/3, then the app blocks until the writer drains
```

## Verification

Run the programs and confirm:
1. Buffered sends succeed without a receiver (up to capacity)
2. A full buffer blocks the sender until the consumer makes room
3. The unbuffered version is significantly slower for the application than the buffered version

## Common Mistakes

### Using Buffer Size as a Replacement for Proper Design

**Wrong:**
```go
package main

func main() {
    ch := make(chan string, 10000)
    // "I'll just make the buffer huge so it never fills"
    for i := 0; i < 20000; i++ {
        ch <- "log entry" // blocks at entry 10001!
    }
}
```

**What happens:** If you produce more than the buffer holds, you block. A large buffer hides the problem temporarily but does not solve it. In production, the burst will eventually exceed your buffer and the application stalls anyway.

**Fix:** Size the buffer based on expected burst sizes and consumer throughput, not as a substitute for backpressure handling.

### Checking len() Before Sending

**Wrong:**
```go
// In concurrent code:
if len(logBuffer) < cap(logBuffer) {
    logBuffer <- entry // RACE: another goroutine might have filled it
}
```

**What happens:** Between checking `len()` and sending, another goroutine might fill the buffer. The send still blocks.

**Fix:** Just send. If you need non-blocking behavior, use `select` with a `default` case (covered in the select section).

## Review

A buffered channel is an internal FIFO queue with a fixed capacity, and its
behavior collapses to two rules: a send blocks only when the queue is full, a
receive only when it is empty. That is what lets a fast producer and a slow
consumer run at their own paces -- entries pile up in the buffer during a burst
and drain later -- while still bounding memory, because once the buffer fills the
sender blocks and cannot outrun the writer. That blocking is backpressure, and
it is why a buffer is not a synchronization primitive: `len(ch)` and `cap(ch)`
report the queue's fill level and capacity for diagnostics, but reading them to
decide whether to send is a race, since another goroutine can change the count
between the check and the send. An unbuffered channel is the opposite extreme
-- capacity zero, every send a synchronous rendezvous with a receiver.

You should now be able to say, without running the code, exactly how
`make(chan string)` differs from `make(chan string, 10)`: the first is a
synchronization point where each send waits for a receiver, the second an async
queue that holds ten entries before it blocks. You should know what happens when
you send to a full buffered channel -- the sender blocks until a receive frees a
slot -- and be able to justify when a logging system wants a buffer at all:
whenever the producer emits in bursts faster than the writer flushes, so the
buffer absorbs the burst instead of stalling the request path on disk I/O.

## Resources
- [A Tour of Go: Buffered Channels](https://go.dev/tour/concurrency/3) -- the minimal introduction to `make(chan T, n)` and when sends begin to block.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- idiomatic channel use, including buffered channels used as queues.
- [Go Spec: Making channels](https://go.dev/ref/spec#Making_slices_maps_and_channels) -- the precise semantics of channel capacity and the blocking rules it implies.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-channel-direction](../04-channel-direction/04-channel-direction.md)
