# Exercise 6: Bulk Writer — Batch an `iter.Seq` into Fixed-Size Chunks for DB Insert

Bulk `INSERT` and bulk publish want fixed-size batches, not one row per round-trip
and not the whole stream at once. This exercise builds `Batch`, which groups an
`iter.Seq[T]` into slices of up to `size` — flushing a partial final batch — and
wires it to a fake bulk-insert sink. The subtle hazard it drills is buffer
aliasing: yield a reused backing slice and the consumer's retained batch silently
mutates.

## What you'll build

```text
batch/                    independent module: example.com/batch
  go.mod                  module example.com/batch
  batch.go                Batch, BulkInsert
  cmd/
    demo/
      main.go             runnable demo: batch a stream, "insert" each chunk
  batch_test.go           chunking, exact-multiple, empty, no-aliasing, early-break
```

Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
Implement: `Batch[T](seq iter.Seq[T], size int) iter.Seq[[]T]` that copies each batch into a fresh slice, and `BulkInsert` that ranges batches into a sink.
Test: stream of 10 with size 3 gives `[3,3,3,1]`; exact multiple gives no trailing empty; empty stream gives nothing; retained batches do not alias; early break stops upstream.
Verify: `go test -count=1 -race ./...`

## The design

`Batch` carries a buffer across yields — closure state that must be reset after
each flush. It appends each incoming value; when the buffer reaches `size` it
flushes a batch and truncates the buffer to length zero (keeping the capacity).
After the source is exhausted, a non-empty remainder is flushed as the final
partial batch. Guarding `size <= 0` up front avoids an infinite non-flushing
loop.

The load-bearing decision is what to yield. The internal `buf` is reused every
cycle, so yielding `buf` directly would hand every batch the *same* backing array
— after the next append overwrites it, a consumer that stored an earlier batch
sees corrupted data. The fix is to copy `buf` into a fresh slice per flush and
yield that. This is the exact hazard `bufio` and many streaming APIs document
("the slice is valid only until the next call"); here we pay one allocation per
batch to make the batch safe to retain, which is almost always what a bulk writer
needs since it hands the batch to a driver.

`Batch` also honors cooperative stop: if the sink returns `false` (the consumer
broke), the flush's `if !yield(batch) { return }` unwinds and the source stops —
so a writer that aborts after the first batch does not drain the whole stream.
Note the contrast with `slices.Chunk`, which also produces size-`n` sub-slices but
only over an already-materialized slice; `Batch` works on an unbounded stream that
was never a slice.

Create `batch.go`:

```go
package batch

import "iter"

// Batch groups seq into slices of up to size, flushing a partial final batch.
// Each yielded batch is a fresh copy, safe to retain past the next iteration.
func Batch[T any](seq iter.Seq[T], size int) iter.Seq[[]T] {
	return func(yield func([]T) bool) {
		if size <= 0 {
			return
		}
		buf := make([]T, 0, size)
		flush := func() bool {
			out := make([]T, len(buf))
			copy(out, buf)
			buf = buf[:0]
			return yield(out)
		}
		for v := range seq {
			buf = append(buf, v)
			if len(buf) == size {
				if !flush() {
					return
				}
			}
		}
		if len(buf) > 0 {
			flush()
		}
	}
}

// BulkInsert ranges seq in batches of size, calling insert on each. It returns
// the first insert error, stopping the stream cooperatively.
func BulkInsert[T any](seq iter.Seq[T], size int, insert func([]T) error) error {
	var err error
	for b := range Batch(seq, size) {
		if err = insert(b); err != nil {
			return err
		}
	}
	return err
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/batch"
)

func main() {
	stream := slices.Values([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})

	err := batch.BulkInsert(stream, 3, func(b []int) error {
		fmt.Printf("insert batch of %d: %v\n", len(b), b)
		return nil
	})
	fmt.Println("err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
insert batch of 3: [1 2 3]
insert batch of 3: [4 5 6]
insert batch of 3: [7 8 9]
insert batch of 1: [10]
err: <nil>
```

## Tests

Create `batch_test.go`:

```go
package batch

import (
	"reflect"
	"slices"
	"testing"
)

func sizes(batches [][]int) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}
	return out
}

func TestBatchSizes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		n    int
		size int
		want []int
	}{
		{"ten by three", 10, 3, []int{3, 3, 3, 1}},
		{"exact multiple", 10, 5, []int{5, 5}},
		{"single batch", 2, 5, []int{2}},
		{"size one", 3, 1, []int{1, 1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := slices.Values(seqN(tc.n))
			got := sizes(slices.Collect(Batch(src, tc.size)))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sizes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmptyStreamNoBatches(t *testing.T) {
	t.Parallel()

	got := slices.Collect(Batch(slices.Values([]int{}), 3))
	if len(got) != 0 {
		t.Fatalf("got %d batches, want 0", len(got))
	}
}

func TestBatchesDoNotAlias(t *testing.T) {
	t.Parallel()

	src := slices.Values(seqN(6))
	batches := slices.Collect(Batch(src, 2))

	want := [][]int{{0, 1}, {2, 3}, {4, 5}}
	if !reflect.DeepEqual(batches, want) {
		t.Fatalf("retained batches = %v, want %v (aliasing would corrupt earlier ones)", batches, want)
	}
}

func TestEarlyBreakStopsUpstream(t *testing.T) {
	t.Parallel()

	var produced int
	src := func(yield func(int) bool) {
		for i := range 1000 {
			produced++
			if !yield(i) {
				return
			}
		}
	}

	for range Batch(src, 3) {
		break // consume only the first batch
	}

	if produced > 3 {
		t.Fatalf("produced = %d, want <= 3 (early break must stop upstream)", produced)
	}
}

func seqN(n int) []int {
	out := make([]int, n)
	for i := range n {
		out[i] = i
	}
	return out
}
```

## Review

`Batch` is correct when the size sequence matches the stream length modulo the
batch size — including a partial final batch and no spurious empty trailing batch
on an exact multiple — and when an empty stream yields nothing. The aliasing test
is the one that catches the subtle bug: collecting all batches and comparing the
whole `[][]int` only holds if each batch is an independent copy; yield the shared
`buf` instead and the earlier batches would show the last batch's contents. The
early-break test proves the flush honors `yield`'s `false` so a writer that aborts
does not drain the source. Reach for `slices.Chunk` only when you already have a
materialized slice; `Batch` is for the streaming case.

## Resources

- [`iter` package documentation](https://pkg.go.dev/iter)
- [`slices.Chunk`](https://pkg.go.dev/slices#Chunk)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-log-lines-seq-pipeline.md](05-log-lines-seq-pipeline.md) | Next: [07-layered-config-maps-seq.md](07-layered-config-maps-seq.md)
