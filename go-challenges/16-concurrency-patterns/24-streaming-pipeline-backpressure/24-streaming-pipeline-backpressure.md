# 24. Streaming Pipeline with Backpressure

A naive channel pipeline moves data as fast as the slowest stage allows. When
the consumer is slower than the producer, one of two things happens: the
producer blocks on an unbuffered channel (halting the whole pipeline) or it
writes to a large buffered channel until memory runs out. Neither is acceptable
for long-lived streams. Backpressure is the signal that flows upstream when a
downstream stage cannot keep up: a bounded channel full, a produce call blocks,
the source slows down. This lesson builds a three-stage pipeline with bounded
buffers and measures the backpressure effect.

```text
pipeline/
  go.mod
  internal/pipeline/pipeline.go
  internal/pipeline/pipeline_test.go
  cmd/demo/main.go
```

Module path: `example.com/pipeline`. Set up with:

```bash
mkdir -p ~/go-exercises/pipeline/internal/pipeline ~/go-exercises/pipeline/cmd/demo
cd ~/go-exercises/pipeline
go mod init example.com/pipeline
```

## Concepts

### Bounded Channels as the Backpressure Mechanism

A `make(chan T, N)` channel holds at most N items before a send blocks. When
stage B is slower than stage A, stage A's send on the channel between them
blocks. Stage A therefore stops draining its input channel. That in turn causes
stage A's input channel to fill up, which blocks stage A's upstream sender, and
so on up the chain. The bounded buffer size N controls the trade-off between
latency (smaller N means less buffering) and throughput (larger N allows bursts
to be absorbed without immediately stalling the producer).

### Three-Stage Model

This lesson uses Source -> Process -> Sink:

- Source generates items at a configurable rate.
- Process transforms each item (slow by design).
- Sink consumes results (can be slowed further to create backpressure).

Each stage runs in its own goroutine. The channel between Source and Process,
and between Process and Sink, is bounded to a fixed capacity. When Sink is
slow, Process blocks writing to the Process->Sink channel. Process then blocks
reading from the Source->Process channel. Source then blocks writing to that
channel. The source feels backpressure.

### Context Cancellation and Drain

When `context.Done()` is received, the source stops generating items. Stages
already in-flight continue processing and draining their input channels before
returning. The canonical pattern is:

```
for {
    select {
    case <-ctx.Done():
        return
    case item, ok := <-in:
        if !ok { return }
        // process item
        out <- result
    }
}
```

Close the output channel with `defer close(out)` so the next stage's range loop
exits cleanly.

### Metrics with Atomic Counters

Per-stage item counts use `sync/atomic` to avoid mutexes on the hot path. Buffer
utilisation is `len(ch) / cap(ch)`, sampled at observation time. Both are
approximations — channel length changes between the sample and the print — but
they are sufficient for observability.

### Lossy vs Lossless Backpressure

A lossless pipeline blocks the source when downstream is full. A lossy pipeline
drops items instead of blocking, trading correctness for latency. The choice
depends on the domain: a log pipeline can drop; a payment pipeline cannot. This
lesson implements lossless backpressure (blocking send) but the `Dropped`
counter on `Metrics` supports a lossy extension.

## Exercises

### Exercise 1: Pipeline Types and Stage Implementation

Create `internal/pipeline/pipeline.go`:

```go
package pipeline

import (
	"context"
	"sync/atomic"
)

// Metrics tracks items processed by one stage.
type Metrics struct {
	Processed atomic.Int64
	Dropped   atomic.Int64
}

// Pipeline connects three stages with bounded channels.
type Pipeline struct {
	srcOut  chan int
	procOut chan int
	SrcM    Metrics
	ProcM   Metrics
	SinkM   Metrics
}

// New creates a Pipeline with the given buffer capacity between stages.
func New(bufSize int) *Pipeline {
	return &Pipeline{
		srcOut:  make(chan int, bufSize),
		procOut: make(chan int, bufSize),
	}
}

// Run starts all three stages and blocks until the context is cancelled and all
// stages have drained. items is the list of items the source will emit.
func (p *Pipeline) Run(ctx context.Context, items []int, process func(int) int) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		sinkDone := make(chan struct{})
		procDone := make(chan struct{})
		go p.runSink(sinkDone)
		go p.runProcess(ctx, process, procDone)
		p.runSource(ctx, items)
		<-procDone
		<-sinkDone
	}()
	<-done
}

func (p *Pipeline) runSource(ctx context.Context, items []int) {
	defer close(p.srcOut)
	for _, item := range items {
		select {
		case <-ctx.Done():
			return
		case p.srcOut <- item:
			p.SrcM.Processed.Add(1)
		}
	}
}

func (p *Pipeline) runProcess(ctx context.Context, fn func(int) int, done chan<- struct{}) {
	defer close(p.procOut)
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-p.srcOut:
			if !ok {
				return
			}
			result := fn(item)
			p.procOut <- result
			p.ProcM.Processed.Add(1)
		}
	}
}

func (p *Pipeline) runSink(done chan<- struct{}) {
	defer close(done)
	for range p.procOut {
		p.SinkM.Processed.Add(1)
	}
}

// SrcBufLen returns the current number of items buffered between source and process.
func (p *Pipeline) SrcBufLen() int { return len(p.srcOut) }

// ProcBufLen returns the current number of items buffered between process and sink.
func (p *Pipeline) ProcBufLen() int { return len(p.procOut) }

// BufCap returns the buffer capacity used between all stages.
func (p *Pipeline) BufCap() int { return cap(p.srcOut) }
```

### Exercise 2: Tests

Create `internal/pipeline/pipeline_test.go`:

```go
package pipeline_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"example.com/pipeline/internal/pipeline"
)

// TestItemsFlowThrough verifies that all items pass through all three stages.
func TestItemsFlowThrough(t *testing.T) {
	t.Parallel()

	p := pipeline.New(8)
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}

	ctx := context.Background()
	p.Run(ctx, items, func(v int) int { return v * 2 })

	if got := p.SinkM.Processed.Load(); got != int64(len(items)) {
		t.Fatalf("sink processed %d, want %d", got, len(items))
	}
	if got := p.ProcM.Processed.Load(); got != int64(len(items)) {
		t.Fatalf("process processed %d, want %d", got, len(items))
	}
}

// TestBackpressureSlowsSink verifies that a slow sink causes the source to be
// held back: source finishes after the pipeline completes, not before.
func TestBackpressureSlowsSink(t *testing.T) {
	t.Parallel()

	const bufSize = 2
	const itemCount = 20

	p := pipeline.New(bufSize)
	items := make([]int, itemCount)
	for i := range items {
		items[i] = i
	}

	// Slow process: sleeps briefly to create backpressure.
	process := func(v int) int {
		time.Sleep(2 * time.Millisecond)
		return v
	}

	ctx := context.Background()
	p.Run(ctx, items, process)

	if got := p.SinkM.Processed.Load(); got != itemCount {
		t.Fatalf("sink got %d items, want %d", got, itemCount)
	}
}

// TestContextCancellation verifies that cancelling the context stops the
// source from sending more items and the pipeline drains without hang.
func TestContextCancellation(t *testing.T) {
	t.Parallel()

	p := pipeline.New(4)
	// Large item set so cancellation happens mid-stream.
	items := make([]int, 1000)
	for i := range items {
		items[i] = i
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	p.Run(ctx, items, func(v int) int {
		time.Sleep(time.Millisecond)
		return v
	})

	// After cancellation, the sink processed fewer items than the total.
	// We can't assert an exact number, but we can assert it terminated.
	processed := p.SinkM.Processed.Load()
	if processed > int64(len(items)) {
		t.Fatalf("sink processed %d, more than total %d", processed, len(items))
	}
}

func ExamplePipeline() {
	p := pipeline.New(4)
	items := []int{1, 2, 3, 4, 5}
	p.Run(context.Background(), items, func(v int) int { return v * v })
	fmt.Printf("processed %d items\n", p.SinkM.Processed.Load())
	// Output: processed 5 items
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/pipeline/internal/pipeline"
)

func main() {
	const bufSize = 4
	const itemCount = 30

	p := pipeline.New(bufSize)
	items := make([]int, itemCount)
	for i := range items {
		items[i] = i + 1
	}

	// process sleeps briefly to simulate a slow stage.
	process := func(v int) int {
		time.Sleep(5 * time.Millisecond)
		return v * v
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	p.Run(ctx, items, process)
	elapsed := time.Since(start)

	fmt.Printf("source:  %d items emitted\n", p.SrcM.Processed.Load())
	fmt.Printf("process: %d items transformed\n", p.ProcM.Processed.Load())
	fmt.Printf("sink:    %d items consumed\n", p.SinkM.Processed.Load())
	fmt.Printf("elapsed: %v\n", elapsed.Round(time.Millisecond))
}
```

## Common Mistakes

### Using Unbuffered Channels Between All Stages

Wrong: `make(chan int)` between every stage pair.

What happens: each stage can only send after the next stage is ready to receive.
The pipeline throughput equals the throughput of the slowest stage with no
burst absorption. A single slow tick in any stage stalls the entire pipeline
for that tick's duration.

Fix: use `make(chan int, N)` with a buffer sized to absorb burst differences
between adjacent stages. N = 2x the estimated burst size is a common starting
point.

### Closing the Channel From the Wrong End

Wrong: the consumer closes the channel to signal it is done.

What happens: the producer's next send panics with "send on closed channel".

Fix: only the sender (producer) closes a channel. The consumer signals "stop"
via a shared `context.Context` or a `done <-chan struct{}`.

### Forgetting defer close(out) in a Stage

Wrong: returning from a stage goroutine without closing its output channel.

What happens: the downstream stage's `for range in` loop blocks forever, leaking
the goroutine.

Fix: `defer close(out)` at the top of every stage goroutine so the close runs
on every return path including cancellation and error paths.

### Sampling Buffer Depth Inside a Lock

Wrong: reading `len(ch)` while holding a lock to get a "consistent" view.

What happens: `len(ch)` is already a point-in-time approximation; the lock
adds latency to the hot path without making the sample more meaningful.

Fix: read `len(ch)` without a lock. Accept that the value is approximate and
document that metrics are best-effort snapshots.

## Verification

From `~/go-exercises/pipeline`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit 0. The race detector confirms no shared-state
accesses between stages escape the channel boundary.

## Summary

- Bounded channels create natural backpressure: a full channel blocks the
  sender, propagating pressure upstream stage by stage.
- Each stage runs in its own goroutine with `defer close(out)` to signal
  completion to the downstream stage.
- Context cancellation is the producer's stop signal; stages drain their in-
  flight items before returning.
- Only the sender closes a channel; the consumer signals via context.
- Buffer size N is a throughput/latency trade-off: larger N absorbs bursts,
  smaller N reduces end-to-end latency.

## What's Next

Next: [Actor Model in Go](../25-actor-model-in-go/25-actor-model-in-go.md).

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) - foundational pipeline patterns with cancellation
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) - goroutine and channel fundamentals
- [Go channel internals](https://go.dev/src/runtime/chan.go) - how buffered channels implement blocking
- [Reactive Streams specification](https://www.reactive-streams.org/) - backpressure semantics formalised for the JVM world
- [go vet documentation](https://pkg.go.dev/cmd/vet) - static analysis including channel misuse detection
