# Exercise 7: Report Domain Metrics with b.ReportMetric

Sometimes raw latency is the wrong story. A batch worker that processes 1,000 events
per call has a large `ns/op`, but what you actually want to know is the *per-item*
cost and how much data it moved. `b.ReportMetric(value, unit)` emits custom columns
like `events/op` and `bytes/event` alongside `ns/op`. This module benchmarks a
batch-processing worker and reports both.

## What you'll build

```text
batch/                     independent module: example.com/batch
  go.mod                   go 1.24
  batch.go                 type Event; type Result; ProcessBatch([]Event) Result
  cmd/
    demo/
      main.go              runnable demo: process a batch, print the result
  batch_test.go            TestProcessBatch (all events processed correctly);
                           BenchmarkProcessBatch with ReportMetric(events/op, bytes/event); Example
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `ProcessBatch(events []Event) Result` that aggregates a slice of events.
- Test: correctness of the aggregate, plus a benchmark reporting two custom metrics.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/05-benchmarks/07-custom-metrics-reportmetric/cmd/demo
cd go-solutions/12-testing-ecosystem/05-benchmarks/07-custom-metrics-reportmetric
go mod edit -go=1.24
```

### Why a custom metric says more than ns/op here

`ProcessBatch` takes a slice of events, and for each one accumulates a running total
of a numeric field and the total payload bytes, returning a `Result{Count, Sum,
Bytes}`. Benchmarked over a 1,000-event batch, the `ns/op` is the cost of the *whole
batch* — a big, batch-size-dependent number that tells you little about efficiency. The
question a senior engineer asks is "what does one event cost, amortized?" and "how much
data did we move per event?". Those are `events/op` and `bytes/event`, and they are
what `b.ReportMetric` is for.

The arithmetic is the part people get wrong. `ReportMetric(value, unit)` reports the
given value verbatim; it does not divide by `b.N`. So to report a *per-op* metric you
divide by the iteration count yourself. This benchmark uses the `b.N` form because the
divisor is then explicit and correct: it counts total events processed across all
iterations and reports `float64(processed)/float64(b.N)`, which equals the batch size
(1,000) — a sanity check that the metric is computed right. `bytes/event` is total
bytes divided by total events. The unit strings are single trailing tokens
(`"events/op"`, `"bytes/event"`); a unit with a space, like `"events per op"`, is a bug
that produces a malformed column.

Create `batch.go`:

```go
package batch

// Event is one unit of work in a batch: an amount to aggregate and a payload whose
// size contributes to the bytes moved.
type Event struct {
	Amount  int64
	Payload []byte
}

// Result is the aggregate of a processed batch.
type Result struct {
	Count int   // number of events processed
	Sum   int64 // sum of Amount across events
	Bytes int   // total payload bytes across events
}

// ProcessBatch aggregates a slice of events in one pass.
func ProcessBatch(events []Event) Result {
	var r Result
	for i := range events {
		r.Count++
		r.Sum += events[i].Amount
		r.Bytes += len(events[i].Payload)
	}
	return r
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batch"
)

func main() {
	events := []batch.Event{
		{Amount: 10, Payload: []byte("aaaa")},
		{Amount: 20, Payload: []byte("bb")},
		{Amount: 30, Payload: []byte("cccccc")},
	}
	r := batch.ProcessBatch(events)
	fmt.Printf("count=%d sum=%d bytes=%d\n", r.Count, r.Sum, r.Bytes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count=3 sum=60 bytes=12
```

### Tests

`TestProcessBatch` builds a known batch and asserts every field of the aggregate. The
benchmark processes a fixed 1,000-event batch, accumulates the total events and bytes
across all iterations, and reports the two per-item metrics with the correct divisor.

Create `batch_test.go`:

```go
package batch

import (
	"fmt"
	"testing"
)

func makeEvents(n int) []Event {
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{Amount: int64(i), Payload: make([]byte, 32)}
	}
	return events
}

func TestProcessBatch(t *testing.T) {
	t.Parallel()
	events := []Event{
		{Amount: 10, Payload: []byte("aaaa")},
		{Amount: 20, Payload: []byte("bb")},
		{Amount: 30, Payload: []byte("cccccc")},
	}
	got := ProcessBatch(events)
	want := Result{Count: 3, Sum: 60, Bytes: 12}
	if got != want {
		t.Fatalf("ProcessBatch = %+v, want %+v", got, want)
	}
}

func BenchmarkProcessBatch(b *testing.B) {
	const batchSize = 1000
	events := makeEvents(batchSize)
	b.ReportAllocs()

	var processed int
	var bytesMoved int
	for range b.N {
		r := ProcessBatch(events)
		processed += r.Count
		bytesMoved += r.Bytes
	}

	// ReportMetric does not divide by b.N; do it yourself for a per-op figure.
	b.ReportMetric(float64(processed)/float64(b.N), "events/op")
	b.ReportMetric(float64(bytesMoved)/float64(processed), "bytes/event")
}

func ExampleProcessBatch() {
	r := ProcessBatch([]Event{
		{Amount: 5, Payload: []byte("xy")},
		{Amount: 7, Payload: []byte("z")},
	})
	fmt.Printf("%d %d %d\n", r.Count, r.Sum, r.Bytes)
	// Output: 2 12 3
}
```

Run the benchmark; the custom columns appear next to `ns/op`:

```bash
go test -bench=. -benchmem
```

```text
BenchmarkProcessBatch-8   1204831   290.5 ns/op   32.00 bytes/event   1000 events/op   0 B/op   0 allocs/op
PASS
```

## Review

`TestProcessBatch` fixes the aggregate exactly, so a change that miscounts events or
mis-sums amounts fails immediately. The benchmark lesson is that the `events/op` column
reading exactly `1000` is both the useful amortized figure and a self-check that the
per-op division was done right — if it printed `1000 * something`, the divisor is wrong.
`bytes/event` of `32.00` matches the fixed 32-byte payloads, confirming the second
metric. The two traps to avoid are baked into the code as counter-examples to remember:
do not forget that `ReportMetric` reports the raw value (so you must divide by `b.N`
yourself for a per-op number), and keep the unit a single trailing token — `"events/op"`
renders as a clean column, `"events per op"` does not. A custom metric like this is how
you communicate per-item efficiency in a PR where a raw batch `ns/op` would hide it.

## Resources

- [`testing.B.ReportMetric`](https://pkg.go.dev/testing#B.ReportMetric) — emit a custom column; unit must be a single trailing token, repeated units are averaged.
- [`testing.B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs) — add the B/op and allocs/op columns.
- [go test benchmark flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — how `go test` formats benchmark output columns.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-contention-with-runparallel.md](06-contention-with-runparallel.md) | Next: [08-table-driven-benchmarks-scaling.md](08-table-driven-benchmarks-scaling.md)
