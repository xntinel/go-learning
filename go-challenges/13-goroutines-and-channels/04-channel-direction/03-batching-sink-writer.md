# Exercise 3: A Batching Sink That Consumes a Receive-Only Record Stream

The mirror of a source is a sink: a function that accepts `<-chan Record`,
accumulates values up to a batch size, and flushes each batch to a repository —
the shape of every bulk-insert writer and log shipper. The receive-only parameter
guarantees the sink can never accidentally send upstream or close the producer's
channel.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
sink/                        independent module: example.com/sink
  go.mod                     go 1.26
  sink.go                    type Record; Drain(in <-chan Record, batchSize int,
                             flush func([]Record) error) error
  cmd/
    demo/
      main.go                runnable demo: batch records into flushes
  sink_test.go               full batches, partial-on-close, empty stream
```

Files: `sink.go`, `cmd/demo/main.go`, `sink_test.go`.
Implement: `Drain(in <-chan Record, batchSize int, flush func([]Record) error) error` — accumulate up to `batchSize`, flush, and flush the trailing partial batch when the channel closes.
Test: two full flushes for `2*batchSize` records, one trailing partial flush on a non-multiple count, no flush on an empty stream.
Verify: `go test -count=1 -race ./...`

### Why receive-only, and how close drives the final flush

`Drain` takes `in <-chan Record`. That type is the contract: the sink drains, it
does not produce. It cannot send a `Record` back onto `in`, and it cannot
`close(in)` — both are compile errors on a receive-only channel. Closing is the
producer's job; the sink only reacts to the close.

The batching logic hangs on that reaction. `Drain` accumulates records in a slice.
When the slice reaches `batchSize`, it flushes and starts a fresh batch. The
`for v := range in` loop ends when the producer closes the channel — and that is
exactly when a real bulk writer must flush whatever partial batch is left, so no
records are lost at end-of-stream. After the loop, `Drain` flushes any non-empty
remainder. Channel close is therefore doing double duty: it terminates the loop
*and* signals "flush the tail."

A correctness detail that matters under `-race`: each flush must hand the callback
a batch the sink will not mutate afterward. Reusing one backing slice across
flushes would alias — if the repository retained the slice, the next batch would
overwrite it. So `Drain` allocates a fresh batch after every flush. The `flush`
callback returns an `error`; `Drain` stops and returns the first flush error,
because a bulk writer that keeps buffering after its sink has failed just loses
more data.

Create `sink.go`:

```go
package sink

// Record is one item to be written to the sink.
type Record struct {
	Key   string
	Value int
}

// Drain reads records from in and flushes them in batches of at most batchSize.
// It flushes a full batch as soon as it fills, and flushes any trailing partial
// batch when in is closed. in is receive-only: Drain cannot send on it or close
// it. Drain returns the first error flush reports, or nil once in is drained.
func Drain(in <-chan Record, batchSize int, flush func([]Record) error) error {
	batch := make([]Record, 0, batchSize)
	for r := range in {
		batch = append(batch, r)
		if len(batch) == batchSize {
			if err := flush(batch); err != nil {
				return err
			}
			batch = make([]Record, 0, batchSize)
		}
	}
	if len(batch) > 0 {
		if err := flush(batch); err != nil {
			return err
		}
	}
	return nil
}
```

### The runnable demo

The demo feeds seven records through a source with a batch size of three, so the
sink flushes batches of 3, 3, and a trailing 1.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sink"
)

func main() {
	in := make(chan sink.Record)
	go func() {
		defer close(in)
		for i := 1; i <= 7; i++ {
			in <- sink.Record{Key: fmt.Sprintf("k%d", i), Value: i}
		}
	}()

	batchNo := 0
	err := sink.Drain(in, 3, func(batch []sink.Record) error {
		batchNo++
		fmt.Printf("flush %d: %d records\n", batchNo, len(batch))
		return nil
	})
	if err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flush 1: 3 records
flush 2: 3 records
flush 3: 1 records
```

### Tests

The tests use a mutex-guarded recorder that stores a copy of every batch, so the
`-race` detector has no false positives and the assertions can inspect batch
sizes and contents. `TestFlushesFullBatches` feeds `2*batchSize` records and
asserts exactly two flushes of `batchSize`. `TestFlushesPartialBatchOnClose`
feeds a non-multiple and asserts the trailing partial is flushed exactly once
when the channel closes. `TestNoFlushOnEmptyStream` asserts a closed-but-empty
channel triggers no flush at all.

Create `sink_test.go`:

```go
package sink

import (
	"errors"
	"slices"
	"sync"
	"testing"
)

type recorder struct {
	mu      sync.Mutex
	batches [][]Record
}

func (r *recorder) flush(batch []Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, slices.Clone(batch))
	return nil
}

func (r *recorder) sizes() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.batches))
	for i, b := range r.batches {
		out[i] = len(b)
	}
	return out
}

func feed(records int) <-chan Record {
	in := make(chan Record)
	go func() {
		defer close(in)
		for i := range records {
			in <- Record{Key: "k", Value: i}
		}
	}()
	return in
}

func TestFlushesFullBatches(t *testing.T) {
	t.Parallel()

	var rec recorder
	if err := Drain(feed(6), 3, rec.flush); err != nil {
		t.Fatalf("Drain returned %v", err)
	}
	if got, want := rec.sizes(), []int{3, 3}; !slices.Equal(got, want) {
		t.Fatalf("batch sizes = %v, want %v", got, want)
	}
}

func TestFlushesPartialBatchOnClose(t *testing.T) {
	t.Parallel()

	var rec recorder
	if err := Drain(feed(7), 3, rec.flush); err != nil {
		t.Fatalf("Drain returned %v", err)
	}
	if got, want := rec.sizes(), []int{3, 3, 1}; !slices.Equal(got, want) {
		t.Fatalf("batch sizes = %v, want %v", got, want)
	}
}

func TestNoFlushOnEmptyStream(t *testing.T) {
	t.Parallel()

	var rec recorder
	if err := Drain(feed(0), 3, rec.flush); err != nil {
		t.Fatalf("Drain returned %v", err)
	}
	if got := rec.sizes(); len(got) != 0 {
		t.Fatalf("empty stream produced flushes %v, want none", got)
	}
}

var errFlush = errors.New("flush failed")

func TestDrainReturnsFlushError(t *testing.T) {
	t.Parallel()

	err := Drain(feed(6), 3, func([]Record) error { return errFlush })
	if !errors.Is(err, errFlush) {
		t.Fatalf("Drain error = %v, want %v", err, errFlush)
	}
}
```

## Review

The sink is correct when the batch boundary is exactly `batchSize` and the
close-driven final flush loses nothing. The partial-on-close test is the
load-bearing one: a bulk writer that only flushes on the size threshold silently
drops the last, sub-`batchSize` group of records at shutdown — a real
data-loss bug. The receive-only parameter is what makes "the sink accidentally
closes or feeds the producer's channel" impossible to write. The fresh-slice
allocation per flush keeps the batches from aliasing under `-race`; run
`go test -race` to confirm.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — receive-only parameters and the operations they forbid.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — sinks that drain `for range` until close.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — copying a batch so a retained slice does not alias the reused buffer.

---

Prev: [02-event-source-generator.md](02-event-source-generator.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-fan-in-merge.md](04-fan-in-merge.md)
