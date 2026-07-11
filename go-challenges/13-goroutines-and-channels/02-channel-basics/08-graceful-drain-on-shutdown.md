# Exercise 8: Drain Pending Work After Close so Nothing Is Lost

The last thing a metrics batcher must do on shutdown is flush the samples it has
already accepted — dropping them means a gap in your dashboards exactly when you
most want data. The property that makes a clean drain possible is that closing a
channel does not discard already-sent values: receivers keep getting them until the
channel is empty, then see the close. This exercise builds a batcher whose
`Shutdown` closes the input and drains every queued sample into a final flush.

This module is self-contained: its own module, a `metrics` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
metrics/                     independent module: example.com/metrics
  go.mod                     go 1.26
  metrics.go                 type Sample, Batcher; New, Ingest, Shutdown, Flushed
  cmd/demo/main.go           runnable demo: ingest a burst, shut down, count flush
  metrics_test.go            drains all, no loss under concurrency, terminates
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `New(buffer int) *Batcher`, `(*Batcher).Ingest(Sample)`, `(*Batcher).Shutdown()` that closes the input and drains it, `(*Batcher).Flushed() []Sample`.
- Test: all M ingested samples appear in the flush; no sample is lost under concurrent ingest; the drainer terminates after close.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metrics/cmd/demo
cd ~/go-exercises/metrics
go mod init example.com/metrics
```

### Close, then drain to completion

The batcher's `run` goroutine loops on the input channel and records each sample.
The subtle correctness comes from how it exits. Written with the comma-ok form:

```go
for {
	s, ok := <-b.in
	if !ok {
		return // channel closed AND fully drained
	}
	b.record(s)
}
```

a receive returns `ok == false` only once the channel is both closed *and* empty.
So after `Shutdown` calls `close(b.in)`, this loop keeps pulling every sample still
queued in the buffer — recording each with `ok == true` — and returns only when the
last one is gone. That is the drain. A plain `for s := range b.in` behaves
identically; the comma-ok form is spelled out here to make the "closed and empty"
condition explicit.

The bug this design avoids is a `run` loop that watches a separate `quit` channel in
a `select` and returns the moment `quit` fires — abandoning whatever samples are
still in `b.in`. That loses the tail. The correct shutdown signal *is* the close of
the data channel, not a side channel; closing the data channel is what lets the
drainer finish the queue before stopping.

`Shutdown` closes `b.in` and then waits (`<-b.done`) for `run` to finish draining, so
when `Shutdown` returns the flush is guaranteed complete. `Ingest` must not be called
after `Shutdown` (a send on the closed `b.in` would panic); the tests keep all
ingest before shutdown, and a real service would gate `Ingest` behind the same
lifecycle.

Create `metrics.go`:

```go
package metrics

import "sync"

// Sample is one metric observation.
type Sample struct {
	Name  string
	Value float64
}

// Batcher accepts samples on a channel and, on Shutdown, drains every queued
// sample into a final flush so nothing already accepted is lost.
type Batcher struct {
	in   chan Sample
	done chan struct{}

	mu      sync.Mutex
	flushed []Sample
}

// New starts a Batcher whose input channel has the given buffer capacity.
func New(buffer int) *Batcher {
	b := &Batcher{
		in:   make(chan Sample, buffer),
		done: make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *Batcher) run() {
	defer close(b.done)
	for {
		s, ok := <-b.in
		if !ok {
			return // closed and drained
		}
		b.record(s)
	}
}

func (b *Batcher) record(s Sample) {
	b.mu.Lock()
	b.flushed = append(b.flushed, s)
	b.mu.Unlock()
}

// Ingest accepts a sample. It must not be called after Shutdown.
func (b *Batcher) Ingest(s Sample) {
	b.in <- s
}

// Shutdown closes the input and blocks until every queued sample is flushed.
func (b *Batcher) Shutdown() {
	close(b.in)
	<-b.done
}

// Flushed returns a snapshot copy of the flushed samples.
func (b *Batcher) Flushed() []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Sample(nil), b.flushed...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func main() {
	b := metrics.New(64)
	for i := range 100 {
		b.Ingest(metrics.Sample{Name: "req", Value: float64(i)})
	}
	b.Shutdown()
	fmt.Println("flushed:", len(b.Flushed()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed: 100
```

### Tests

`TestShutdownFlushesAllPendingSamples` ingests M samples into a buffer, shuts down,
and asserts all M are in the flush — the direct proof that close does not discard
queued values. `TestNoSamplesLostUnderConcurrentIngest` has many goroutines ingest
concurrently, joins them, then shuts down and counts, proving the drain is correct
even when producers were racing. `TestDrainTerminatesAfterClose` confirms `Shutdown`
returns (the drainer reaches `ok == false` and exits) rather than hanging — the
default test timeout backs this up.

Create `metrics_test.go`:

```go
package metrics

import (
	"sync"
	"testing"
)

func TestShutdownFlushesAllPendingSamples(t *testing.T) {
	t.Parallel()

	const m = 500
	b := New(m) // buffer large enough to hold the burst
	for i := range m {
		b.Ingest(Sample{Name: "x", Value: float64(i)})
	}
	b.Shutdown()

	if got := len(b.Flushed()); got != m {
		t.Fatalf("flushed %d samples, want %d", got, m)
	}
}

func TestNoSamplesLostUnderConcurrentIngest(t *testing.T) {
	t.Parallel()

	b := New(8)
	const goroutines, each = 10, 100

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				b.Ingest(Sample{Name: "x", Value: float64(g*each + i)})
			}
		}()
	}
	wg.Wait() // all sends done before we close
	b.Shutdown()

	if got := len(b.Flushed()); got != goroutines*each {
		t.Fatalf("flushed %d samples, want %d", got, goroutines*each)
	}
}

func TestDrainTerminatesAfterClose(t *testing.T) {
	t.Parallel()

	b := New(4)
	b.Ingest(Sample{Name: "a", Value: 1})
	b.Ingest(Sample{Name: "b", Value: 2})
	b.Shutdown() // returns only if the drainer terminated

	got := b.Flushed()
	if len(got) != 2 {
		t.Fatalf("flushed %d, want 2", len(got))
	}
}
```

## Review

The batcher is correct when every sample accepted before shutdown appears in the
flush. The mechanism is the comma-ok drain loop that exits only on closed-and-empty,
combined with `Shutdown` waiting on `done` so the flush is complete before it
returns. `TestShutdownFlushesAllPendingSamples` proves no tail is lost; the
concurrent test proves it under racing producers; and a clean `-race` run proves the
`record` mutex and the drain ordering are sound. The classic mistake this exercise
inoculates against is a `select`-on-quit loop that returns the instant a shutdown
signal fires, discarding queued samples — the fix is to make the close of the data
channel itself the shutdown signal, so the drainer always finishes the queue first.

## Resources

- [Go Language Spec: Receive operator](https://go.dev/ref/spec#Receive_operator) — receiving from a closed channel drains remaining values before reporting closed.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — draining and orderly shutdown of channel stages.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the flushed slice for a consistent snapshot.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-fan-in-merge-health-checks.md](07-fan-in-merge-health-checks.md) | Next: [09-ordered-results-index.md](09-ordered-results-index.md)
