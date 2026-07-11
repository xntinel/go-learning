# Exercise 2: A Bounded ETL Pipeline Service

A real extract-transform-load job is the pipeline pattern under production constraints: stages connected by bounded channels so memory stays capped, a `context.Context` so an operator or a deadline can stop the run, and a clean shutdown that returns aggregated statistics instead of leaking goroutines. This exercise builds that service — parse, transform, batching sink — as a single configurable `Pipeline` type.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
etl.go               Record, Stats, Pipeline, parseLine, Pipeline.Run
etl_test.go          full run, invalid/dropped counts, cancellation, sink error, leak check
cmd/
  demo/
    main.go          run a small ETL job and print the stats and batches
```

- Files: `etl.go`, `etl_test.go`, `cmd/demo/main.go`.
- Implement: `Record`, `Stats`, the `Pipeline` config type, the `parseLine` helper, and `Pipeline.Run(ctx, src) (Stats, error)` wiring a parse stage, a transform stage, and a batching sink with bounded channels.
- Test: a full successful run, the invalid- and dropped-record counts, cancellation via the context, a sink error that aborts the run, and a goroutine-leak check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p etl-pipeline-service/cmd/demo && cd etl-pipeline-service
go mod init example.com/etl-pipeline-service
```

### Three stages, bounded channels, one context

The service is three goroutines connected by two channels, both `make(chan Record, BufferSize)`. The bound is the whole point: a buffered channel of fixed capacity lets a fast stage run a little ahead of a slow one to absorb jitter, but caps the in-flight work, so the parse stage cannot race ahead and buffer the entire input in memory when the sink is slow. That is backpressure with a hard ceiling.

The parse stage reads raw lines, calls `parseLine`, counts the unparseable ones as `Invalid` and skips them, and sends each good `Record` downstream inside a `select` that also watches `ctx.Done()`. The transform stage applies the configured `Transform` function, counts rejected records as `Dropped`, and forwards the kept ones, again selecting on `ctx.Done()` at the send. The sink stage accumulates records into a batch and flushes a full batch through the configured `Sink`; when the input channel closes it flushes the final partial batch and returns. Every stage opens with `defer close(out)` (the parse and transform stages, which own an outbound channel) so termination cascades: when the source list is exhausted the parse stage closes `parsed`, the transform stage's range ends and it closes `transformed`, the sink drains the last records and returns, and `Run`'s `wg.Wait()` unblocks.

Cancellation is a single derived context. `Run` does `ctx, cancel := context.WithCancel(parent)` so it can cancel the whole pipeline from the inside — when the sink returns an error — while still honoring a cancel or deadline from the caller's `parent`. Every stage selects on `ctx.Done()` at its send, so a cancel from either source stops all three stages promptly: the stage that is blocked sending takes the `ctx.Done()` branch, returns, runs its deferred close, and the cascade tears the rest down. Because `Run` blocks on `wg.Wait()` before returning, the function returning is itself proof that all three goroutines exited — there is no leak even when the run is cancelled mid-flight.

The statistics are race-free by construction: each stage writes only its own private `Stats` value, and `Run` merges the three only after `wg.Wait()`, which establishes the happens-before edge that makes the merged read safe. The `sinkErr` is likewise written only by the sink goroutine and read only after the wait. The error contract on return is layered: a sink error wins (it is the cause of the cancel), then a caller cancellation (`parent.Err()`), then `nil` for a clean full run.

Create `etl.go`:

```go
// Package etl implements a bounded, context-cancellable extract-transform-load
// pipeline: a parse stage, a transform stage, and a batching sink, connected by
// bounded channels and shut down cleanly when the source drains or the context
// is cancelled.
package etl

import (
	"context"
	"strconv"
	"strings"
	"sync"
)

// Record is one parsed unit of work flowing through the pipeline.
type Record struct {
	Key   string
	Value int
}

// Stats is the aggregated outcome of a Run.
type Stats struct {
	Invalid int // raw lines that failed to parse
	Parsed  int // records that parsed successfully
	Dropped int // parsed records the transform rejected
	Written int // records handed to the sink
	Batches int // sink flushes
}

// Pipeline configures the three stages.
type Pipeline struct {
	// BufferSize bounds each inter-stage channel; zero is treated as 1. A small
	// value gives backpressure with a hard ceiling on in-flight records.
	BufferSize int
	// Transform maps a parsed record to an output record; the bool reports
	// whether to keep it. If nil, records pass through unchanged.
	Transform func(Record) (Record, bool)
	// BatchSize is the number of records per sink flush; zero is treated as 1.
	BatchSize int
	// Sink receives each non-empty batch. The batch slice is freshly allocated
	// per flush, so a Sink may retain it. If Sink returns an error the pipeline
	// cancels and Run returns that error. If nil, batches are discarded.
	Sink func(batch []Record) error
}

// parseLine parses "key,value" into a Record. It reports ok=false on any line
// that is not exactly a non-empty key and an integer value.
func parseLine(raw string) (Record, bool) {
	k, v, found := strings.Cut(raw, ",")
	if !found || strings.TrimSpace(k) == "" {
		return Record{}, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return Record{}, false
	}
	return Record{Key: strings.TrimSpace(k), Value: n}, true
}

// Run executes the pipeline over src and returns aggregated statistics. It
// returns a non-nil error if the sink failed or the caller's context was
// cancelled. Run blocks until every stage goroutine has exited.
func (p *Pipeline) Run(parent context.Context, src []string) (Stats, error) {
	buf := p.BufferSize
	if buf < 1 {
		buf = 1
	}
	batchSize := p.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	parsed := make(chan Record, buf)
	transformed := make(chan Record, buf)

	var (
		pStats, tStats, sStats Stats
		sinkErr                error
		wg                     sync.WaitGroup
	)
	wg.Add(3)

	// Parse stage: raw lines -> parsed records.
	go func() {
		defer wg.Done()
		defer close(parsed)
		for _, raw := range src {
			rec, ok := parseLine(raw)
			if !ok {
				pStats.Invalid++
				continue
			}
			select {
			case parsed <- rec:
				pStats.Parsed++
			case <-ctx.Done():
				return
			}
		}
	}()

	// Transform stage: parsed records -> kept records.
	go func() {
		defer wg.Done()
		defer close(transformed)
		for rec := range parsed {
			out := rec
			keep := true
			if p.Transform != nil {
				out, keep = p.Transform(rec)
			}
			if !keep {
				tStats.Dropped++
				continue
			}
			select {
			case transformed <- out:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Sink stage: batch and flush.
	go func() {
		defer wg.Done()
		batch := make([]Record, 0, batchSize)
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			if p.Sink != nil {
				if err := p.Sink(batch); err != nil {
					sinkErr = err
					cancel()
					return false
				}
			}
			sStats.Written += len(batch)
			sStats.Batches++
			batch = make([]Record, 0, batchSize)
			return true
		}
		for {
			select {
			case rec, ok := <-transformed:
				if !ok {
					flush() // final partial batch
					return
				}
				batch = append(batch, rec)
				if len(batch) >= batchSize {
					if !flush() {
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()

	stats := Stats{
		Invalid: pStats.Invalid,
		Parsed:  pStats.Parsed,
		Dropped: tStats.Dropped,
		Written: sStats.Written,
		Batches: sStats.Batches,
	}
	if sinkErr != nil {
		return stats, sinkErr
	}
	if err := parent.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}
```

### The runnable demo

The demo runs a small job end to end: eight raw lines, one of them malformed and one carrying a negative value the transform drops, doubling the survivors and batching them three at a time into an in-memory store. Because each stage is a single goroutine and the channels preserve order, the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/etl-pipeline-service"
)

func main() {
	var store [][]etl.Record
	p := &etl.Pipeline{
		BufferSize: 4,
		BatchSize:  3,
		Transform: func(r etl.Record) (etl.Record, bool) {
			if r.Value < 0 {
				return etl.Record{}, false // drop negatives
			}
			r.Value *= 2
			return r, true
		},
		Sink: func(batch []etl.Record) error {
			store = append(store, batch)
			return nil
		},
	}

	src := []string{"a,1", "b,2", "bad-line", "c,-5", "d,4", "e,5", "f,6", "g,7"}

	stats, err := p.Run(context.Background(), src)
	if err != nil {
		fmt.Println("run error:", err)
		return
	}

	fmt.Printf("invalid=%d parsed=%d dropped=%d written=%d batches=%d\n",
		stats.Invalid, stats.Parsed, stats.Dropped, stats.Written, stats.Batches)
	for i, b := range store {
		fmt.Printf("batch %d:", i)
		for _, r := range b {
			fmt.Printf(" %s=%d", r.Key, r.Value)
		}
		fmt.Println()
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
invalid=1 parsed=7 dropped=1 written=6 batches=2
batch 0: a=2 b=4 d=8
batch 1: e=10 f=12 g=14
```

### Tests

The tests pin the contract. `TestPipelineProcessesAll` runs a clean job and checks both the stats and the sink contents. `TestPipelineCountsInvalidAndDropped` proves the malformed lines and rejected records are counted, not silently passed. `TestPipelineAlreadyCancelled` hands `Run` an already-cancelled context and asserts it returns `context.Canceled`. `TestPipelineCancelMidStream` cancels the parent from inside the sink after two batches and asserts the run stops early and reports the cancellation. `TestPipelineSinkError` returns an error from the sink and asserts `Run` surfaces it. `TestPipelineNoGoroutineLeak` runs a large job and asserts the goroutine count returns to its baseline, proving clean shutdown.

Create `etl_test.go`:

```go
package etl

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func makeSrc(n int) []string {
	src := make([]string, 0, n)
	for i := 0; i < n; i++ {
		src = append(src, fmt.Sprintf("k%d,%d", i, i))
	}
	return src
}

func TestPipelineProcessesAll(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []Record
	p := &Pipeline{
		BufferSize: 2,
		BatchSize:  4,
		Transform: func(r Record) (Record, bool) {
			r.Value *= 10
			return r, true
		},
		Sink: func(batch []Record) error {
			mu.Lock()
			got = append(got, batch...)
			mu.Unlock()
			return nil
		},
	}

	stats, err := p.Run(context.Background(), []string{"a,1", "b,2", "c,3", "d,4", "e,5"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Parsed != 5 || stats.Written != 5 || stats.Invalid != 0 || stats.Dropped != 0 {
		t.Fatalf("stats = %+v, want parsed/written=5 invalid/dropped=0", stats)
	}
	if stats.Batches != 2 {
		t.Fatalf("batches = %d, want 2", stats.Batches)
	}
	if len(got) != 5 {
		t.Fatalf("sink received %d records, want 5", len(got))
	}
	sum := 0
	for _, r := range got {
		sum += r.Value
	}
	if sum != 150 {
		t.Fatalf("sum = %d, want 150", sum)
	}
}

func TestPipelineCountsInvalidAndDropped(t *testing.T) {
	t.Parallel()

	p := &Pipeline{
		BufferSize: 1,
		BatchSize:  10,
		Transform: func(r Record) (Record, bool) {
			return r, r.Value >= 0
		},
		Sink: func(batch []Record) error { return nil },
	}

	src := []string{"a,1", "nope", "b,-2", "c,3", "", "d,-4"}
	stats, err := p.Run(context.Background(), src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Invalid != 2 { // "nope" and ""
		t.Fatalf("invalid = %d, want 2", stats.Invalid)
	}
	if stats.Parsed != 4 { // a, b, c, d
		t.Fatalf("parsed = %d, want 4", stats.Parsed)
	}
	if stats.Dropped != 2 { // b=-2, d=-4
		t.Fatalf("dropped = %d, want 2", stats.Dropped)
	}
	if stats.Written != 2 { // a, c
		t.Fatalf("written = %d, want 2", stats.Written)
	}
}

func TestPipelineAlreadyCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run

	p := &Pipeline{BufferSize: 1, BatchSize: 4, Sink: func(b []Record) error { return nil }}
	_, err := p.Run(ctx, makeSrc(1000))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestPipelineCancelMidStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const batchSize = 5
	batches := 0
	p := &Pipeline{
		BufferSize: 1,
		BatchSize:  batchSize,
		Sink: func(b []Record) error {
			batches++
			if batches == 2 {
				cancel() // stop the whole run from the sink side
			}
			return nil
		},
	}

	stats, err := p.Run(ctx, makeSrc(1000))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if stats.Written < 2*batchSize {
		t.Fatalf("written = %d, want at least %d", stats.Written, 2*batchSize)
	}
	if stats.Written >= 1000 {
		t.Fatalf("written = %d, expected the run to stop well before the full input", stats.Written)
	}
}

func TestPipelineSinkError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sink down")
	p := &Pipeline{
		BufferSize: 1,
		BatchSize:  4,
		Sink:       func(b []Record) error { return sentinel },
	}

	stats, err := p.Run(context.Background(), makeSrc(100))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if stats.Written != 0 {
		t.Fatalf("written = %d, want 0 (first batch failed)", stats.Written)
	}
}

// TestPipelineNoGoroutineLeak is intentionally NOT parallel so it runs alone in
// the sequential phase; NumGoroutine is process-global and parallel tests would
// perturb it.
func TestPipelineNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	p := &Pipeline{BufferSize: 2, BatchSize: 8, Sink: func(b []Record) error { return nil }}
	if _, err := p.Run(context.Background(), makeSrc(500)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline %d: now %d", base, runtime.NumGoroutine())
}
```

## Review

The service is correct when the three stages cascade-close on a clean run and tear down promptly on a cancel. Confirm the parse and transform stages each open with `defer close` so exhausting the source closes `parsed`, ends the transform range, closes `transformed`, and lets the sink flush its final partial batch and return. Confirm every send selects on `ctx.Done()` so a cancel — from the caller's parent or from the sink's own `cancel()` after a sink error — stops a blocked stage instead of deadlocking it, and that `Run` returning is proof of no leak because it blocks on `wg.Wait()`. The stats are sound only because each stage owns its private counter and the merge reads them after the wait; the same wait makes the `sinkErr` read safe. All of this holding under `go test -race` is the real verification.

Common mistakes for this feature. The first is making the inter-stage channels unbounded (or sizing them to the whole input) to "avoid blocking," which discards backpressure and lets a slow sink balloon memory; a small fixed `BufferSize` is the point. The second is reading the per-stage stats before `wg.Wait()`, or worse incrementing one shared `Stats` from all three goroutines, which is a data race — keep counters private and merge after the barrier. The third is forgetting to select on `ctx.Done()` at a send, so a cancelled run with a stalled sink leaks the upstream goroutines on a full channel. The fourth is returning `nil` from `Run` when the sink errored: the error is the reason the pipeline cancelled and must win over the context error in the return contract.

## Resources

- [`context` package](https://pkg.go.dev/context) — `WithCancel`, `Done`, and `Err`, the cancellation substrate this service threads through every stage.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the explicit-cancellation discipline that keeps a multi-stage pipeline leak-free.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — how a server propagates cancellation and deadlines across a request's goroutines.

---

Back to [01-pipeline-core.md](01-pipeline-core.md) | Next: [03-log-event-pipeline-draining.md](03-log-event-pipeline-draining.md)
