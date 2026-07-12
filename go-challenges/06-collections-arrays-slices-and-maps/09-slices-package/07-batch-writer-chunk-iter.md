# Exercise 7: Batch Bulk Writes Into Provider-Limited Groups (slices.Chunk iterator)

Backend providers cap bulk operations: DynamoDB `BatchWriteItem` takes 25 items,
SQS `SendMessageBatch` takes 10, many databases have a parameter limit per
statement. A batching layer splits a large slice of records into fixed-size groups
and flushes each to a writer. `slices.Chunk` is the Go 1.23 iterator that yields
those groups, driving a range-over-func loop; this module builds the layer,
handles the ragged final chunk, and rejects an invalid chunk size.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
batchwriter/                   module example.com/batchwriter
  go.mod                       go 1.24
  batch.go                     type Record, Writer; WriteAll (slices.Chunk), size validation
  cmd/
    demo/
      main.go                  runnable demo: 7 records into batches of 3
  batch_test.go                exact multiple/ragged/single/empty, len<=n, all elements once, n<1 panics
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `WriteAll(w Writer, records []Record, n int) error` that iterates `slices.Chunk(records, n)` and flushes each chunk to `w`, returning an error for `n < 1` before touching `Chunk`.
- Test: exact multiples, ragged remainder, single element, empty input (no batches), each chunk `len <= n` with the last possibly shorter, every element in exactly one batch and order preserved, and that `slices.Chunk` panics for `n < 1`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Chunk yields sub-slices; validate n yourself

`slices.Chunk(s, n)` returns an `iter.Seq[[]E]` — an iterator you range over that
yields successive sub-slices of length `n`, with the final one shorter when
`len(s)` is not a multiple of `n`. Ranging is the natural loop:
`for batch := range slices.Chunk(records, n) { ... }`. Each iteration hands you one
batch to flush. If the writer returns an error, `return` from the loop and the
iterator stops cleanly.

Two contracts matter. First, `Chunk` panics if `n < 1`. A batch size of zero or
negative is a programming error, but a panic deep inside a bulk-write path is a
poor failure mode, so `WriteAll` validates `n >= 1` up front and returns a wrapped
sentinel error (`ErrInvalidBatchSize`) instead of letting `Chunk` panic. The test
covers both: `WriteAll` returns the error, and calling `slices.Chunk` directly with
`n < 1` panics (asserted via `recover`). Second, the sub-slices `Chunk` yields
*alias the source backing array* — they are windows into `records`, not copies. For
a synchronous flush that is fine and efficient. But if a handler retained a batch
past its iteration (stored it, or handed it to a goroutine), a later mutation of
`records` would be visible through it; retain-and-defer code must `slices.Clone`
the batch. The demo and tests flush synchronously, so aliasing is safe here.

The empty-input case yields zero batches: ranging over `Chunk(nil, n)` runs the
body zero times, so `WriteAll` on empty input is a successful no-op. That is the
right behavior — nothing to write, no error.

Create `batch.go`:

```go
package batchwriter

import (
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidBatchSize is returned when the batch size is not at least 1.
var ErrInvalidBatchSize = errors.New("batchwriter: batch size must be >= 1")

// Record is one item to write.
type Record struct {
	ID string
}

// Writer flushes one provider-limited batch. A real implementation calls
// BatchWriteItem, SendMessageBatch, or a multi-row INSERT.
type Writer interface {
	WriteBatch(batch []Record) error
}

// WriteAll splits records into groups of n and flushes each via w. It rejects
// n < 1 with ErrInvalidBatchSize rather than letting slices.Chunk panic.
func WriteAll(w Writer, records []Record, n int) error {
	if n < 1 {
		return fmt.Errorf("WriteAll: %w (got %d)", ErrInvalidBatchSize, n)
	}
	for batch := range slices.Chunk(records, n) {
		if err := w.WriteBatch(batch); err != nil {
			return fmt.Errorf("WriteAll: write batch: %w", err)
		}
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchwriter"
)

// printWriter records each batch's size as it is flushed.
type printWriter struct{}

func (printWriter) WriteBatch(batch []batchwriter.Record) error {
	ids := make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}
	fmt.Printf("batch(%d): %v\n", len(batch), ids)
	return nil
}

func main() {
	records := make([]batchwriter.Record, 7)
	for i := range records {
		records[i] = batchwriter.Record{ID: fmt.Sprintf("r%d", i)}
	}

	if err := batchwriter.WriteAll(printWriter{}, records, 3); err != nil {
		fmt.Println("error:", err)
		return
	}

	if err := batchwriter.WriteAll(printWriter{}, records, 0); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch(3): [r0 r1 r2]
batch(3): [r3 r4 r5]
batch(1): [r6]
rejected: WriteAll: batchwriter: batch size must be >= 1 (got 0)
```

Seven records split into batches of 3, 3, and a ragged 1; a zero batch size is
rejected with the wrapped sentinel rather than panicking.

### Tests

`TestWriteAllChunking` is table-driven over exact multiple, ragged remainder,
single element, and empty input, asserting the batch sizes and that every input
element appears exactly once in order. `TestInvalidSize` asserts `WriteAll` returns
`ErrInvalidBatchSize` for `n < 1`. `TestChunkPanicsOnZero` proves `slices.Chunk`
itself panics for `n < 1` via `recover`.

Create `batch_test.go`:

```go
package batchwriter

import (
	"errors"
	"slices"
	"testing"
)

// capWriter records the size of each batch and the flattened element order.
type capWriter struct {
	sizes []int
	flat  []string
}

func (c *capWriter) WriteBatch(batch []Record) error {
	c.sizes = append(c.sizes, len(batch))
	for _, r := range batch {
		c.flat = append(c.flat, r.ID)
	}
	return nil
}

func mkRecords(n int) ([]Record, []string) {
	recs := make([]Record, n)
	ids := make([]string, n)
	for i := range recs {
		id := string(rune('a' + i))
		recs[i] = Record{ID: id}
		ids[i] = id
	}
	return recs, ids
}

func TestWriteAllChunking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		count     int
		n         int
		wantSizes []int
	}{
		{"exact multiple", 6, 3, []int{3, 3}},
		{"ragged remainder", 7, 3, []int{3, 3, 1}},
		{"single element", 1, 3, []int{1}},
		{"empty input", 0, 3, nil},
		{"n larger than input", 2, 10, []int{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recs, ids := mkRecords(tc.count)
			w := &capWriter{}
			if err := WriteAll(w, recs, tc.n); err != nil {
				t.Fatalf("WriteAll error: %v", err)
			}
			if !slices.Equal(w.sizes, tc.wantSizes) {
				t.Fatalf("batch sizes = %v, want %v", w.sizes, tc.wantSizes)
			}
			for _, size := range w.sizes {
				if size > tc.n {
					t.Fatalf("batch size %d exceeds n=%d", size, tc.n)
				}
			}
			// Every input element appears exactly once, in order.
			if !slices.Equal(w.flat, ids) {
				t.Fatalf("flattened = %v, want %v", w.flat, ids)
			}
		})
	}
}

func TestInvalidSize(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1} {
		err := WriteAll(&capWriter{}, []Record{{ID: "a"}}, n)
		if !errors.Is(err, ErrInvalidBatchSize) {
			t.Fatalf("WriteAll(n=%d) err = %v, want ErrInvalidBatchSize", n, err)
		}
	}
}

func TestChunkPanicsOnZero(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("slices.Chunk(_, 0) did not panic")
		}
	}()
	// The iterator is created lazily; ranging it triggers the panic.
	for range slices.Chunk([]Record{{ID: "a"}}, 0) {
		t.Fatal("loop body ran; expected panic before first iteration")
	}
}
```

## Review

The batching layer is correct when every input element lands in exactly one batch,
order is preserved, and no batch exceeds `n` (only the last may be shorter). The
`slices.Chunk` iterator gives all of that for free; the two decisions the code owns
are validating `n >= 1` before `Chunk` can panic, and flushing synchronously so the
aliased sub-slices stay valid. Empty input correctly yields zero batches and no
error. The panic test documents the raw `Chunk` contract that `WriteAll` shields
callers from. Run `go test -race`; each case owns its writer.

## Resources

- [`slices.Chunk`](https://pkg.go.dev/slices#Chunk) — yields sub-slice batches, panics if n < 1, aliases the source.
- [`iter` package](https://pkg.go.dev/iter) — `iter.Seq`, the range-over-func mechanism Chunk drives.
- [Go blog: range over function iterators](https://go.dev/blog/range-functions) — how the ranged loop works.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-shard-fanin-merge-concat-sorted.md](08-shard-fanin-merge-concat-sorted.md)
