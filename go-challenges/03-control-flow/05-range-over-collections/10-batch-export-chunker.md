# Exercise 10: Batch Export Downstream Writes with slices.Chunk

Bulk sinks — a `INSERT ... VALUES` batch, an SQS `SendMessageBatch`, a Kafka
producer flush — all cap how many records go in one call. This module flushes a
slice of records to a downstream sink in fixed-size chunks using
`for chunk := range slices.Chunk(records, batchSize)`, handling the final partial
chunk and guarding the batch size so the `slices.Chunk` panic never fires in
production.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
batchexport/                independent module: example.com/batchexport
  go.mod                    go 1.24
  batchexport.go            Record; ErrInvalidBatchSize; Export(records, batchSize, write) error
  cmd/
    demo/
      main.go               runnable demo: export 7 records in batches of 3
  batchexport_test.go       partial last chunk, single chunk, empty, size<=cap, invalid size
```

- Files: `batchexport.go`, `cmd/demo/main.go`, `batchexport_test.go`.
- Implement: `Export(records []Record, batchSize int, write func([]Record) error) error` ranging `slices.Chunk`, propagating write errors, and returning `ErrInvalidBatchSize` when `batchSize < 1`.
- Test: sizes `[n,n,...,remainder]` with no records dropped or duplicated; `batchSize >= len` gives one chunk; empty input gives zero chunks; each write size `<= batchSize`; invalid size returns the sentinel error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/10-batch-export-chunker/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/10-batch-export-chunker
go mod edit -go=1.24
```

### Chunking a slice into an iterator of sub-slices

`slices.Chunk(s, n)` returns an `iter.Seq[[]E]` that yields consecutive sub-slices
of length `n`; the final chunk is shorter when `len(s)` is not a multiple of `n`.
Ranging it with `for chunk := range slices.Chunk(records, batchSize)` gives you each
batch to hand to the sink, and the partial-last-chunk handling is automatic — you do
not compute `i` and `min(i+n, len)` bounds by hand, which is exactly where the
off-by-one bugs live.

Two facts matter for correctness. First, `slices.Chunk` panics if `n < 1`. A batch
size often comes from configuration, where a `0` or negative value is a real
possibility, so `Export` validates it up front and returns `ErrInvalidBatchSize`
rather than letting the panic escape — a bad config value should be a handled error
at the boundary, not a crash. Second, the chunks are views into the original backing
array, not copies; that is fine here because the sink reads each batch synchronously
before the next iteration, but a sink that retained the slice past the call would
need to copy it (the iterator reuses the backing array across the sequence).

`Export` propagates the first write error and stops, because a failed bulk insert
means the export did not fully succeed and the caller must decide whether to retry.

Create `batchexport.go`:

```go
package batchexport

import (
	"errors"
	"slices"
)

// ErrInvalidBatchSize is returned when the batch size is less than 1.
var ErrInvalidBatchSize = errors.New("batchexport: batch size must be >= 1")

// Record is one exportable row.
type Record struct {
	ID int
}

// Export writes records to the sink in batches of at most batchSize, using
// slices.Chunk. It returns ErrInvalidBatchSize if batchSize < 1 and propagates the
// first error the sink returns.
func Export(records []Record, batchSize int, write func(batch []Record) error) error {
	if batchSize < 1 {
		return ErrInvalidBatchSize
	}
	for chunk := range slices.Chunk(records, batchSize) {
		if err := write(chunk); err != nil {
			return err
		}
	}
	return nil
}
```

### The runnable demo

The demo exports seven records in batches of three, printing the size of each batch
so the partial final chunk is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchexport"
)

func main() {
	records := make([]batchexport.Record, 7)
	for i := range records {
		records[i] = batchexport.Record{ID: i}
	}

	err := batchexport.Export(records, 3, func(batch []batchexport.Record) error {
		fmt.Printf("flush batch of %d\n", len(batch))
		return nil
	})
	if err != nil {
		fmt.Println("export error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
flush batch of 3
flush batch of 3
flush batch of 1
```

### Tests

A sink spy records each batch's size and the IDs it received. The partial-chunk test
asserts sizes `[3,3,1]` for seven records and that all seven IDs arrive exactly once.
The single-chunk test uses a batch size larger than the input. The empty test asserts
zero writes. A shared invariant check asserts every write is `<= batchSize`. The
invalid-size test asserts `ErrInvalidBatchSize` via `errors.Is` and that the sink was
never called.

Create `batchexport_test.go`:

```go
package batchexport

import (
	"errors"
	"testing"
)

// spy records batch sizes and the flattened IDs it received.
type spy struct {
	sizes []int
	ids   []int
}

func (s *spy) write(batch []Record) error {
	s.sizes = append(s.sizes, len(batch))
	for _, r := range batch {
		s.ids = append(s.ids, r.ID)
	}
	return nil
}

func makeRecords(n int) []Record {
	rs := make([]Record, n)
	for i := range rs {
		rs[i] = Record{ID: i}
	}
	return rs
}

func TestExportPartialLastChunk(t *testing.T) {
	t.Parallel()
	var sp spy
	if err := Export(makeRecords(7), 3, sp.write); err != nil {
		t.Fatalf("Export = %v, want nil", err)
	}
	wantSizes := []int{3, 3, 1}
	if len(sp.sizes) != len(wantSizes) {
		t.Fatalf("batch sizes = %v, want %v", sp.sizes, wantSizes)
	}
	for i := range wantSizes {
		if sp.sizes[i] != wantSizes[i] {
			t.Fatalf("batch sizes = %v, want %v", sp.sizes, wantSizes)
		}
	}
	// No record dropped or duplicated: IDs 0..6 each once, in order.
	if len(sp.ids) != 7 {
		t.Fatalf("received %d ids, want 7", len(sp.ids))
	}
	for i := range sp.ids {
		if sp.ids[i] != i {
			t.Fatalf("ids = %v, want 0..6 in order", sp.ids)
		}
	}
	// Invariant: every batch <= batchSize.
	for _, sz := range sp.sizes {
		if sz > 3 {
			t.Fatalf("batch of %d exceeds batchSize 3", sz)
		}
	}
}

func TestExportSingleChunkWhenSizeExceedsLen(t *testing.T) {
	t.Parallel()
	var sp spy
	if err := Export(makeRecords(4), 100, sp.write); err != nil {
		t.Fatalf("Export = %v, want nil", err)
	}
	if len(sp.sizes) != 1 || sp.sizes[0] != 4 {
		t.Fatalf("batch sizes = %v, want [4]", sp.sizes)
	}
}

func TestExportEmptyInput(t *testing.T) {
	t.Parallel()
	var sp spy
	if err := Export(nil, 5, sp.write); err != nil {
		t.Fatalf("Export = %v, want nil", err)
	}
	if len(sp.sizes) != 0 {
		t.Fatalf("empty input produced %d writes, want 0", len(sp.sizes))
	}
}

func TestExportInvalidBatchSize(t *testing.T) {
	t.Parallel()
	var sp spy
	err := Export(makeRecords(3), 0, sp.write)
	if !errors.Is(err, ErrInvalidBatchSize) {
		t.Fatalf("Export = %v, want ErrInvalidBatchSize", err)
	}
	if len(sp.sizes) != 0 {
		t.Fatalf("sink called %d times despite invalid size, want 0", len(sp.sizes))
	}
}

func TestExportPropagatesWriteError(t *testing.T) {
	t.Parallel()
	boom := errors.New("sink down")
	calls := 0
	err := Export(makeRecords(9), 3, func(batch []Record) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Export = %v, want boom", err)
	}
	if calls != 2 {
		t.Fatalf("sink called %d times, want 2 (stopped on error)", calls)
	}
}
```

## Review

The exporter is correct when the batches partition the input exactly — every record
delivered once, each batch no larger than `batchSize`, the last batch possibly
smaller — and an invalid batch size is a handled error, not a panic. `slices.Chunk`
does the partitioning, so the module's job is the boundary discipline: validate the
size before calling `Chunk` (which panics on `n < 1`), propagate the sink's first
error, and remember the chunks are views into the backing array so a sink that
retains a batch must copy it. Run `go test`; the partial-chunk and no-drop
assertions are the proof the batching is exact.

## Resources

- [package slices (Chunk)](https://pkg.go.dev/slices#Chunk)
- [package iter (Seq)](https://pkg.go.dev/iter#Seq)
- [Go Specification: For statements (range over func)](https://go.dev/ref/spec#For_range)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-log-line-rune-scan.md](09-log-line-rune-scan.md) | Next: [11-tenant-usage-rollup.md](11-tenant-usage-rollup.md)
