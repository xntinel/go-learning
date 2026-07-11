# Exercise 17: Channel Streaming Backpressure

A log ingestion pipeline reads lines from a high-speed source (10ms per line)
and writes them to a database (50ms per line): the producer is five times faster
than the consumer. Without flow control it would buffer logs in memory
indefinitely until the system runs out of RAM and crashes. A buffered channel
fixes this for free -- the producer sends until the buffer fills, then blocks,
and that block is backpressure that forces the fast producer down to the
consumer's pace. This exercise measures the effect across unbuffered, small, and
large buffers, so the latency-versus-burst-absorption tradeoff becomes numbers
instead of intuition.

## What you'll build

```text
17-channel-streaming-backpressure/
  main.go        fast producer, slow consumer, and a rate tracker measuring
                 how a buffered channel equalizes their rates
```

- Build: an instrumented producer/consumer log pipeline that measures how backpressure equalizes rates.
- Implement: a `LogProducer` and `LogConsumer` over a `LogEntry` channel, a `RateTracker` that reports throughput every N lines, and a `runBenchmark` that compares buffer sizes 1, 10, and 100 for total time and average and max latency.
- Verify: `go run main.go`

### Why blocking is the flow-control mechanism

Buffered channels solve this elegantly. The channel acts as a fixed-size queue between producer and consumer. The producer sends freely until the buffer fills, then blocks. This blocking is backpressure: the fast producer is forced to slow down to the consumer's pace. No explicit rate limiting, no polling, no overflow -- just a blocked goroutine waiting for space.

The buffer size controls the tradeoff. A small buffer means the producer blocks frequently but memory usage is low. A large buffer absorbs bursts and smooths throughput but uses more memory. An unbuffered channel provides zero buffering -- the producer blocks on every single send. This exercise measures all three behaviors.

## Step 1 -- Unbuffered: Producer Blocks on Every Send

With an unbuffered channel, the producer cannot send until the consumer is ready. The producer effectively runs at the consumer's speed. Every send is a synchronous handoff.

```go
package main

import (
	"fmt"
	"time"
)

const (
	produceInterval = 10 * time.Millisecond
	consumeInterval = 50 * time.Millisecond
	totalLines      = 10
)

// LogEntry represents a single log line with a creation timestamp.
type LogEntry struct {
	LineNumber int
	Message    string
	CreatedAt  time.Time
}

// LogProducer generates log entries at a fixed rate.
type LogProducer struct {
	interval time.Duration
}

// Produce sends totalCount entries into the channel, sleeping between each.
func (p *LogProducer) Produce(out chan<- LogEntry, totalCount int) {
	for i := 1; i <= totalCount; i++ {
		entry := LogEntry{
			LineNumber: i,
			Message:    fmt.Sprintf("log line %d", i),
			CreatedAt:  time.Now(),
		}
		out <- entry
		if i < totalCount {
			time.Sleep(p.interval)
		}
	}
	close(out)
}

// LogConsumer reads log entries and simulates slow processing.
type LogConsumer struct {
	interval time.Duration
}

// Consume reads all entries from the channel, simulating slow DB writes.
func (c *LogConsumer) Consume(in <-chan LogEntry) int {
	count := 0
	for entry := range in {
		time.Sleep(c.interval)
		latency := time.Since(entry.CreatedAt).Round(time.Millisecond)
		fmt.Printf("  consumed line %2d (latency: %v)\n", entry.LineNumber, latency)
		count++
	}
	return count
}

func main() {
	ch := make(chan LogEntry) // unbuffered

	producer := &LogProducer{interval: produceInterval}
	consumer := &LogConsumer{interval: consumeInterval}

	fmt.Println("=== Unbuffered Channel (buffer=0) ===")
	fmt.Printf("Producer rate: 1 line / %v\n", produceInterval)
	fmt.Printf("Consumer rate: 1 line / %v\n", consumeInterval)
	fmt.Println()

	start := time.Now()

	go producer.Produce(ch, totalLines)
	consumed := consumer.Consume(ch)

	elapsed := time.Since(start).Round(time.Millisecond)
	effectiveRate := elapsed / time.Duration(consumed)

	fmt.Printf("\nConsumed: %d lines in %v\n", consumed, elapsed)
	fmt.Printf("Effective rate: 1 line / %v (consumer-limited)\n", effectiveRate)
	fmt.Println("Producer blocked on every send, waiting for consumer.")
}
```

The producer generates a line every 10ms, but the consumer takes 50ms to process each. With an unbuffered channel, the producer's `out <- entry` blocks until the consumer calls `range` for the next entry. The effective throughput equals the consumer's rate: ~50ms per line.

### Verification
```bash
go run main.go
# Expected:
#   Each line shows latency growing (consumer is the bottleneck)
#   Total time ~500ms (10 lines * 50ms)
#   Effective rate ~50ms/line (consumer speed, not producer speed)
```

## Step 2 -- Buffer of 10: Producer Runs Ahead Then Blocks

With a buffer of 10, the producer can send up to 10 entries before blocking. It fills the buffer quickly (10 entries * 10ms = ~100ms), then blocks until the consumer drains entries. The consumer sees lower latency for the first batch because entries were already waiting in the buffer.

```go
package main

import (
	"fmt"
	"time"
)

const (
	step2ProduceRate = 10 * time.Millisecond
	step2ConsumeRate = 50 * time.Millisecond
	step2LineCount   = 20
	step2BufferSize  = 10
)

type LogEntry struct {
	LineNumber int
	Message    string
	CreatedAt  time.Time
}

type LogProducer struct {
	interval time.Duration
}

func (p *LogProducer) Produce(out chan<- LogEntry, totalCount int) {
	for i := 1; i <= totalCount; i++ {
		entry := LogEntry{
			LineNumber: i,
			Message:    fmt.Sprintf("log line %d", i),
			CreatedAt:  time.Now(),
		}
		sendStart := time.Now()
		out <- entry
		blocked := time.Since(sendStart).Round(time.Millisecond)
		if blocked > 1*time.Millisecond {
			fmt.Printf("  [producer] line %2d blocked for %v (buffer full)\n", i, blocked)
		}
		if i < totalCount {
			time.Sleep(p.interval)
		}
	}
	close(out)
}

type LogConsumer struct {
	interval time.Duration
}

func (c *LogConsumer) Consume(in <-chan LogEntry) int {
	count := 0
	for entry := range in {
		time.Sleep(c.interval)
		latency := time.Since(entry.CreatedAt).Round(time.Millisecond)
		count++
		fmt.Printf("  [consumer] line %2d (latency: %6v, total consumed: %d)\n",
			entry.LineNumber, latency, count)
	}
	return count
}

func main() {
	ch := make(chan LogEntry, step2BufferSize)

	producer := &LogProducer{interval: step2ProduceRate}
	consumer := &LogConsumer{interval: step2ConsumeRate}

	fmt.Println("=== Buffered Channel (buffer=10) ===")
	fmt.Printf("Producer: 1 line / %v\n", step2ProduceRate)
	fmt.Printf("Consumer: 1 line / %v\n", step2ConsumeRate)
	fmt.Printf("Buffer:   %d slots\n\n", step2BufferSize)

	start := time.Now()

	go producer.Produce(ch, step2LineCount)
	consumed := consumer.Consume(ch)

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nConsumed: %d lines in %v\n", consumed, elapsed)
	fmt.Printf("Effective rate: ~%v per line\n", elapsed/time.Duration(consumed))
	fmt.Println()
	fmt.Println("Observation: producer fills buffer fast, then blocks repeatedly.")
	fmt.Println("Latency grows as entries wait longer in the buffer.")
}
```

Watch the output: the first ~10 lines are produced quickly (buffer absorbs them), then the producer starts blocking as the buffer fills. Latency climbs because entries sit in the buffer waiting for the slow consumer.

### Verification
```bash
go run main.go
# Expected:
#   First 10 lines: producer rarely blocks (buffer absorbs)
#   Lines 11+: producer blocks frequently (buffer full)
#   Latency grows from ~50ms to several hundred ms
#   Total time ~1000ms (20 lines * 50ms, consumer-limited)
```

## Step 3 -- Measure and Print Actual Rates

Instrument both the producer and consumer to report their actual throughput every 5 lines. This makes the rate equalization visible: the producer starts fast but slows to the consumer's pace once the buffer fills.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	step3ProduceRate = 10 * time.Millisecond
	step3ConsumeRate = 50 * time.Millisecond
	step3LineCount   = 30
	step3BufferSize  = 10
	step3ReportEvery = 5
)

type LogEntry struct {
	LineNumber int
	CreatedAt  time.Time
}

type RateTracker struct {
	mu        sync.Mutex
	name      string
	count     int
	lastReset time.Time
}

func NewRateTracker(name string) *RateTracker {
	return &RateTracker{name: name, lastReset: time.Now()}
}

// Record increments the counter and prints a report every `interval` items.
func (rt *RateTracker) Record(reportEvery int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.count++
	if rt.count%reportEvery == 0 {
		elapsed := time.Since(rt.lastReset).Round(time.Millisecond)
		rate := elapsed / time.Duration(reportEvery)
		fmt.Printf("  [%s] %d items in %v (avg: %v/item)\n",
			rt.name, reportEvery, elapsed, rate)
		rt.lastReset = time.Now()
	}
}

func produce(out chan<- LogEntry, count int, interval time.Duration, tracker *RateTracker) {
	for i := 1; i <= count; i++ {
		entry := LogEntry{LineNumber: i, CreatedAt: time.Now()}
		out <- entry
		tracker.Record(step3ReportEvery)
		if i < count {
			time.Sleep(interval)
		}
	}
	close(out)
}

func consume(in <-chan LogEntry, interval time.Duration, tracker *RateTracker) int {
	count := 0
	for range in {
		time.Sleep(interval)
		count++
		tracker.Record(step3ReportEvery)
	}
	return count
}

func main() {
	ch := make(chan LogEntry, step3BufferSize)

	prodTracker := NewRateTracker("producer")
	consTracker := NewRateTracker("consumer")

	fmt.Println("=== Rate Measurement (buffer=10, 30 lines) ===")
	fmt.Printf("Producer target: %v/item\n", step3ProduceRate)
	fmt.Printf("Consumer target: %v/item\n", step3ConsumeRate)
	fmt.Println()

	start := time.Now()
	go produce(ch, step3LineCount, step3ProduceRate, prodTracker)
	consumed := consume(ch, step3ConsumeRate, consTracker)

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nTotal: %d lines in %v\n", consumed, elapsed)
	fmt.Println()
	fmt.Println("Analysis:")
	fmt.Println("- Producer starts at ~10ms/item (its natural rate)")
	fmt.Println("- After buffer fills, producer slows to ~50ms/item (consumer's rate)")
	fmt.Println("- Both rates equalize: this is backpressure in action")
}
```

The rate reports tell the story: the producer's first batch is fast (~10ms/item) because the buffer absorbs sends. After the buffer fills, the producer's rate drops to match the consumer's ~50ms/item. The two rates converge. This is backpressure -- the slow consumer forces the fast producer to its pace, without any explicit throttling code.

### Verification
```bash
go run main.go
# Expected:
#   Producer first report: ~10ms/item (buffer absorbing)
#   Producer later reports: ~50ms/item (blocked on full buffer)
#   Consumer reports: consistently ~50ms/item
#   Rates converge after buffer fills
```

## Step 4 -- Buffer Size Comparison: 1 vs 10 vs 100

Run the same workload with three buffer sizes and compare throughput, total time, and average latency. This makes the buffer-size tradeoff concrete and measurable.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	step4ProduceRate = 10 * time.Millisecond
	step4ConsumeRate = 50 * time.Millisecond
	step4LineCount   = 20
)

type LogEntry struct {
	LineNumber int
	CreatedAt  time.Time
}

// BenchmarkResult holds the metrics from a single run.
type BenchmarkResult struct {
	BufferSize   int
	TotalTime    time.Duration
	AvgLatency   time.Duration
	MaxLatency   time.Duration
	LinesPerSec  float64
}

func runBenchmark(bufferSize int, lineCount int, produceRate, consumeRate time.Duration) BenchmarkResult {
	ch := make(chan LogEntry, bufferSize)

	var totalLatency time.Duration
	var maxLatency time.Duration
	var mu sync.Mutex

	start := time.Now()

	// Producer goroutine.
	go func() {
		for i := 1; i <= lineCount; i++ {
			ch <- LogEntry{LineNumber: i, CreatedAt: time.Now()}
			if i < lineCount {
				time.Sleep(produceRate)
			}
		}
		close(ch)
	}()

	// Consumer in main goroutine for this benchmark.
	consumed := 0
	for entry := range ch {
		time.Sleep(consumeRate)
		latency := time.Since(entry.CreatedAt)

		mu.Lock()
		totalLatency += latency
		if latency > maxLatency {
			maxLatency = latency
		}
		mu.Unlock()

		consumed++
	}

	elapsed := time.Since(start)
	avgLatency := totalLatency / time.Duration(consumed)
	lps := float64(consumed) / elapsed.Seconds()

	return BenchmarkResult{
		BufferSize:  bufferSize,
		TotalTime:   elapsed.Round(time.Millisecond),
		AvgLatency:  avgLatency.Round(time.Millisecond),
		MaxLatency:  maxLatency.Round(time.Millisecond),
		LinesPerSec: lps,
	}
}

func main() {
	bufferSizes := []int{1, 10, 100}

	fmt.Println("=== Buffer Size Comparison ===")
	fmt.Printf("Producer: %v/line, Consumer: %v/line, Lines: %d\n\n",
		step4ProduceRate, step4ConsumeRate, step4LineCount)

	results := make([]BenchmarkResult, 0, len(bufferSizes))
	for _, size := range bufferSizes {
		fmt.Printf("Running with buffer=%d...\n", size)
		result := runBenchmark(size, step4LineCount, step4ProduceRate, step4ConsumeRate)
		results = append(results, result)
	}

	// Print comparison table.
	fmt.Println()
	fmt.Println("+--------+-----------+------------+------------+----------+")
	fmt.Println("| Buffer | Total     | Avg Latency| Max Latency| Lines/s  |")
	fmt.Println("+--------+-----------+------------+------------+----------+")
	for _, r := range results {
		fmt.Printf("| %6d | %9v | %10v | %10v | %8.1f |\n",
			r.BufferSize, r.TotalTime, r.AvgLatency, r.MaxLatency, r.LinesPerSec)
	}
	fmt.Println("+--------+-----------+------------+------------+----------+")

	fmt.Println()
	fmt.Println("=== Analysis ===")
	fmt.Println("Total time: roughly equal for all buffer sizes (consumer-limited).")
	fmt.Println("  The consumer processes at 50ms/line regardless of buffer.")
	fmt.Println()
	fmt.Println("Avg latency: increases with buffer size.")
	fmt.Println("  Larger buffer = entries wait longer before being consumed.")
	fmt.Println()
	fmt.Println("Max latency: increases with buffer size.")
	fmt.Println("  The first entries in a large buffer wait the longest.")
	fmt.Println()
	fmt.Println("Lines/sec: roughly equal (~20 lines/sec = 1/50ms).")
	fmt.Println("  Throughput is bounded by the slowest stage, not the buffer.")
	fmt.Println()
	fmt.Println("Key insight: buffer size trades latency for burst absorption.")
	fmt.Println("  Small buffer = low latency, producer blocks often.")
	fmt.Println("  Large buffer = high latency, producer absorbs bursts.")
}
```

The comparison table reveals the core tradeoff: total throughput is always consumer-limited (~50ms/line), regardless of buffer size. But latency increases with buffer size because entries sit in the buffer longer. A buffer of 1 has low latency but blocks the producer constantly. A buffer of 100 absorbs the entire workload as a burst but entries wait much longer before processing.

### Verification
```bash
go run main.go
# Expected:
#   Total time: ~1000ms for all 3 (20 lines * 50ms)
#   Avg latency: buffer=1 < buffer=10 < buffer=100
#   Max latency: buffer=1 < buffer=10 < buffer=100
#   Lines/sec: ~20 for all 3 (consumer-limited)
```

## Common Mistakes

### Assuming a Bigger Buffer Means Faster Throughput

**Wrong assumption:** "If I increase the buffer from 10 to 1000, my pipeline will process 100x faster."

**Reality:** The buffer does not change the consumer's processing speed. Throughput is always limited by the slowest stage. A larger buffer only absorbs bursts -- it does not speed up steady-state processing. In this exercise, all buffer sizes produce ~20 lines/sec because the consumer takes 50ms per line.

**When a larger buffer helps:** Bursty producers that generate many items at once, then pause. The buffer absorbs the burst while the consumer catches up.

### Ignoring Latency When Choosing Buffer Size

**Wrong:**
```go
ch := make(chan LogEntry, 10000) // huge buffer to "avoid blocking"
```

**What happens:** The producer dumps 10000 entries into the buffer. The oldest entry waits `10000 * 50ms = 500 seconds` before being consumed. If these are real-time alerts, a 500-second delay is unacceptable.

**Fix:** Choose buffer size based on acceptable latency, not just throughput. If latency matters, use a small buffer and accept that the producer will block.

### No Buffer When Burst Tolerance Matters

**Wrong:**
```go
ch := make(chan LogEntry) // unbuffered
```

**What happens:** The producer blocks on every single send, even when the consumer is momentarily ready. If the producer generates 100 lines in a 1ms burst, it takes 5 seconds (100 * 50ms) because each send waits for the consumer.

**Fix:** If the producer is bursty, use a buffer sized to the expected burst size.

## Review

The pipeline's steady-state throughput is set by its slowest stage, full stop.
When the consumer needs 50ms per line, the system delivers one line every 50ms
no matter how large the buffer is, because a buffer stores work but does not
process it. What the buffer changes is latency and coupling: with no buffer the
producer blocks on every send, tightly bound to the consumer's rhythm; with a
buffer the producer runs ahead until the queue fills and only then blocks, so
early entries wait longer before being consumed. That is the whole tradeoff -- a
bigger buffer absorbs bursts and decouples the producer, at the cost of entries
sitting longer in the queue. Backpressure is the blocking itself: the slow
consumer forces the fast producer to its pace with no explicit throttling code.

Hold onto the four questions the exercise poses. Total throughput stays flat
across buffer sizes because it is bounded by the consumer, not the queue;
latency rises with buffer size because entries wait behind more predecessors
before their turn; a larger buffer only earns its keep when the producer is
bursty -- generating a batch all at once, then pausing -- so the buffer smooths
the burst while the consumer catches up; and an unbuffered channel differs from
a buffer of one by exactly one send's worth of slack, coupling producer and
consumer one step more tightly. If you can answer all four from the measurements
you printed, you understand backpressure.

## Resources
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- idiomatic channels, including buffered channels used as bounded queues.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how producer and consumer stages compose into pipelines and where backpressure fits.
- [Go Spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the formal blocking semantics of buffered and unbuffered channels.

---

Back to [Concurrency](../../concurrency.md) | Next: [18-multi-producer-single-consumer](../18-multi-producer-single-consumer/18-multi-producer-single-consumer.md)
