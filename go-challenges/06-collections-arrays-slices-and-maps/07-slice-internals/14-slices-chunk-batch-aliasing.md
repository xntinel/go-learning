# Exercise 14: slices.Chunk Batches Alias the Source Slice

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A bulk indexer sending records to a search or analytics engine -- an
Elasticsearch `_bulk` request, a Kafka producer batching sends -- almost
never ships one record at a time. It splits a large slice of records into
fixed-size network batches first, because the network API itself imposes a
maximum batch size. Go 1.23 added `slices.Chunk` for exactly this: given a
slice and a size, it returns an `iter.Seq[[]T]` yielding consecutive views
of at most that many elements each, no manual index arithmetic required.
What the function's signature does not announce, and what its documentation
states plainly if you read it, is that every yielded batch is a *view*: it
shares the source slice's backing array, not a copy of it.

That aliasing is efficient -- splitting a million-record slice into
thousand-record batches allocates nothing beyond the iterator itself -- and
it is also a trap for exactly the kind of code a bulk indexer tends to
write. Send a batch, and if the network call fails, queue that batch for
retry and move on to the next one. If the retry queue keeps the slice
`Split` handed it without cloning it, and the caller later reuses the source
buffer for the next batch job -- `records = records[:0]` followed by
appending the next job's records into the same backing array, a common
buffer-reuse optimization -- every batch still sitting in the retry queue
silently changes underneath it. The retry logic reads what looks like the
batch that failed and is actually whatever the next job wrote into that
memory. Nothing panics; nothing errors; the wrong records simply get
retried, or the right ones silently don't.

This module builds `bulkbatch`, a package wrapping `slices.Chunk` behind a
validated `Batcher` and documenting the aliasing contract explicitly on
`Split`, alongside `CloneBatches` for the one case that actually needs
independence: retaining a batch past the next mutation of its source. The
naive, uncloned retry queue never appears in the package; it lives only in
the test file, as the contrast the tests pin against.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
bulkbatch/                module example.com/bulkbatch
  go.mod                   go 1.24
  bulkbatch.go              Batcher[T]; NewBatcher, (*Batcher).Split, CloneBatches
  bulkbatch_test.go         split table, aliasing pin, the retry-queue contrast,
                            ExampleBatcher_Split
```

- Files: `bulkbatch.go`, `bulkbatch_test.go`.
- Implement: `NewBatcher[T any](size int) (*Batcher[T], error)` rejecting a non-positive size with `ErrInvalidBatchSize`; `(*Batcher[T]).Split(items []T) iter.Seq[[]T]`, built with `slices.Chunk` and documented as aliasing `items`; `CloneBatches[T any](seq iter.Seq[[]T]) [][]T`, materializing a sequence into independently owned batches.
- Test: the split table (exact division, remainder, batch size larger than the input, size one, empty and nil input); a batch's writes propagating into the source slice; the heart of the module -- an unexported `retryQueueNaive` queuing batches without cloning, and pinning that a queued batch silently changes once the source buffer is reused for the next job; `CloneBatches` surviving that same reuse; and `ExampleBatcher_Split` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/14-slices-chunk-batch-aliasing
cd go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/14-slices-chunk-batch-aliasing
go mod edit -go=1.24
```

### An iterator's yielded view is not a snapshot

`slices.Chunk(items, n)` is, roughly, a generator that yields successive
sub-slices of `items`: `items[0:n]`, `items[n:2n]`, and so on. Each of those
is an ordinary Go sub-slice expression under the hood, and an ordinary
sub-slice shares its source's backing array -- that fact does not change
just because the sub-slice arrived through an `iter.Seq` instead of manual
indexing. A caller that treats a yielded batch as if it were an independent
snapshot of "the records I need to retry" is making the same mistake as
returning a sub-slice of internal storage from an accessor, just one step
removed through the iterator:

```go
// retryQueueNaive queues a failed batch for later retry -- but the batch it
// queues is still a view into the caller's records slice.
func retryQueueNaive[T any](seq iter.Seq[[]T], failed func(batch []T) bool) [][]T {
    var queued [][]T
    for batch := range seq {
        if failed(batch) {
            queued = append(queued, batch) // no clone: still aliases items
        }
    }
    return queued
}
```

Nothing in this function is incorrect the moment it runs: `queued[0]`
genuinely holds the failed batch's records, right up until whatever the
caller does with `items` next. If that next thing is `items = items[:0]`
followed by appending a new job's records into the same array -- a
legitimate way to reuse a buffer across batch jobs -- every entry in
`queued` that still points into that array now reads the new job's data
instead. The fix is unglamorous and exactly matches the lesson's
defensive-copying idiom: clone what you intend to keep, at the point where
you decide to keep it.

Create `bulkbatch.go`:

```go
// Package bulkbatch splits a slice of records into fixed-size batches for a
// bulk network API -- an Elasticsearch bulk request, a Kafka producer's
// batch send -- using slices.Chunk, and documents the aliasing contract that
// comes with it.
package bulkbatch

import (
	"errors"
	"fmt"
	"iter"
	"slices"
)

// ErrInvalidBatchSize is returned by NewBatcher when size is not positive.
var ErrInvalidBatchSize = errors.New("bulkbatch: batch size must be positive")

// Batcher splits a slice of records into batches of a fixed maximum size.
//
// Batcher is immutable after construction and is safe for concurrent use by
// multiple goroutines; each call to Split is independent. What is not safe
// is mutating the items slice passed to Split, or reusing its backing
// storage, while any batch yielded from that call is still in use -- see
// Split.
type Batcher[T any] struct {
	size int
}

// NewBatcher returns a Batcher that splits into batches of at most size
// elements. It returns ErrInvalidBatchSize if size is not positive.
func NewBatcher[T any](size int) (*Batcher[T], error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidBatchSize, size)
	}
	return &Batcher[T]{size: size}, nil
}

// Split returns an iterator over items in consecutive batches of at most
// Batcher's configured size (the final batch may be shorter), built with
// slices.Chunk. An empty or nil items yields no batches.
//
// Each yielded batch aliases items's backing array: it is a view, not a
// copy. Two consequences follow. First, a batch is only guaranteed to hold
// what it held at yield time until items is next mutated -- reusing items's
// backing storage for another purpose (for example, records = records[:0]
// to start the next bulk job) silently changes the content of any batch
// still retained from a previous Split call over that array. Second,
// writing into a yielded batch writes into items itself. A caller that must
// retain a batch past the next mutation of items -- to queue a failed batch
// for retry, for instance -- must copy it first, with slices.Clone or with
// CloneBatches for the whole sequence.
func (b *Batcher[T]) Split(items []T) iter.Seq[[]T] {
	return slices.Chunk(items, b.size)
}

// CloneBatches materializes seq into a slice of independently owned
// batches, each produced with slices.Clone, so the result outlives any
// later mutation of the slice the batches were split from.
func CloneBatches[T any](seq iter.Seq[[]T]) [][]T {
	var out [][]T
	for batch := range seq {
		out = append(out, slices.Clone(batch))
	}
	return out
}
```

### Using it

Build one `Batcher` per configured network batch size and call `Split` for
every slice of records that needs sending -- the type carries no mutable
state, so a single `Batcher` is safe to share across goroutines, and each
`Split` call is independent of any other. What is not free is what `Split`
returns: it is a view, and the doc comment on `Split` is where the aliasing
contract is written down, not left implicit. A caller that only ever ranges
over the sequence once and sends each batch immediately never notices the
aliasing at all -- it only matters the moment a batch is retained past the
point where its source might change.

`ExampleBatcher_Split` is this module's runnable demonstration: `go test`
executes it and checks its output against the `// Output:` block below, so
the behavior it shows is verified on every run.

```go
func ExampleBatcher_Split() {
	b, err := NewBatcher[int](3)
	if err != nil {
		panic(err)
	}

	records := []int{1, 2, 3, 4, 5, 6, 7}
	for batch := range b.Split(records) {
		fmt.Println(batch)
	}

	kept := CloneBatches(b.Split(records))
	records[0] = 99 // mutating the source no longer affects kept
	fmt.Println(kept[0])

	// Output:
	// [1 2 3]
	// [4 5 6]
	// [7]
	// [1 2 3]
}
```

Seven records split into batches of three yield `[1 2 3]`, `[4 5 6]`, and a
shorter final `[7]`. `kept`, built with `CloneBatches`, is unaffected by
`records[0] = 99` afterward -- it is an owned copy, exactly the guarantee a
retry queue needs and `Split`'s raw output does not provide on its own.

### Tests

`TestSplitTable` covers the batching arithmetic: exact division, a
remainder that shortens the final batch, a batch size larger than the whole
input, a batch size of one, and both empty and nil input yielding no
batches. `TestSplitBatchesAliasSourceBackingArray` pins half of the aliasing
contract directly: writing into a yielded batch writes into the source
slice.

`TestNaiveRetryQueueAliasesReusedBuffer` is the heart of the module.
`retryQueueNaive` is unexported and never reachable from the package's API;
the test queues a batch through it, reuses the source buffer for a
simulated next job, and asserts the queued batch's content changed to match
the *new* job's data -- the exact failure mode a retry queue built this way
would suffer in production. `TestCloneBatchesSurvivesSourceReuse` runs the
identical reuse scenario through `CloneBatches` and asserts the opposite: no
change at all.

Create `bulkbatch_test.go`:

```go
package bulkbatch

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"testing"
)

// retryQueueNaive is the antipattern this module warns about: it queues a
// failed batch for later retry by keeping the slice Split handed it,
// without cloning. Every queued batch aliases the source items slice, so if
// that slice's backing storage is reused for the next bulk job before the
// retry actually runs, the queued batch silently shows the new job's data
// instead of the batch that failed.
func retryQueueNaive[T any](seq iter.Seq[[]T], failed func(batch []T) bool) [][]T {
	var queued [][]T
	for batch := range seq {
		if failed(batch) {
			queued = append(queued, batch) // no clone: aliases items
		}
	}
	return queued
}

func TestNewBatcherRejectsNonPositiveSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1, -5} {
		if _, err := NewBatcher[int](size); !errors.Is(err, ErrInvalidBatchSize) {
			t.Errorf("NewBatcher(%d) error = %v, want ErrInvalidBatchSize", size, err)
		}
	}
}

func TestSplitTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		size  int
		items []int
		want  [][]int
	}{
		{name: "exact division", size: 2, items: []int{1, 2, 3, 4}, want: [][]int{{1, 2}, {3, 4}}},
		{name: "remainder", size: 3, items: []int{1, 2, 3, 4, 5, 6, 7}, want: [][]int{{1, 2, 3}, {4, 5, 6}, {7}}},
		{name: "size larger than items", size: 10, items: []int{1, 2, 3}, want: [][]int{{1, 2, 3}}},
		{name: "size one", size: 1, items: []int{1, 2}, want: [][]int{{1}, {2}}},
		{name: "empty items", size: 3, items: []int{}, want: nil},
		{name: "nil items", size: 3, items: nil, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := NewBatcher[int](tc.size)
			if err != nil {
				t.Fatalf("NewBatcher: %v", err)
			}
			var got [][]int
			for batch := range b.Split(tc.items) {
				got = append(got, batch)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Split produced %v, want %v", got, tc.want)
			}
			for i := range got {
				if !slices.Equal(got[i], tc.want[i]) {
					t.Fatalf("batch %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitBatchesAliasSourceBackingArray(t *testing.T) {
	t.Parallel()

	b, err := NewBatcher[int](2)
	if err != nil {
		t.Fatalf("NewBatcher: %v", err)
	}
	items := []int{1, 2, 3, 4}
	for batch := range b.Split(items) {
		batch[0] = -1 // writing into a batch writes into items itself
		break
	}
	if items[0] != -1 {
		t.Fatalf("items[0] = %d after mutating the first batch, want -1", items[0])
	}
}

// TestNaiveRetryQueueAliasesReusedBuffer is the heart of the module: a
// batch queued for retry without cloning silently changes once the caller
// reuses the source buffer for the next bulk job, because it never stopped
// aliasing that buffer's backing array.
func TestNaiveRetryQueueAliasesReusedBuffer(t *testing.T) {
	t.Parallel()

	b, err := NewBatcher[int](2)
	if err != nil {
		t.Fatalf("NewBatcher: %v", err)
	}
	records := []int{1, 2, 3, 4}

	queued := retryQueueNaive(b.Split(records), func(batch []int) bool {
		return batch[0] == 3
	})
	if len(queued) != 1 {
		t.Fatalf("queued %d batches, want 1", len(queued))
	}
	if want := []int{3, 4}; !slices.Equal(queued[0], want) {
		t.Fatalf("queued batch = %v, want %v", queued[0], want)
	}

	// The next bulk job reuses records's backing array.
	records = records[:0]
	records = append(records, 9, 9, 9, 9)

	if want := []int{3, 4}; slices.Equal(queued[0], want) {
		t.Fatal("queued batch should have changed after the buffer was reused, but did not")
	}
	if want := []int{9, 9}; !slices.Equal(queued[0], want) {
		t.Fatalf("queued batch after reuse = %v, want %v (it must alias the reused buffer)", queued[0], want)
	}
}

// TestCloneBatchesSurvivesSourceReuse is the fix side of the same scenario:
// a batch queue built with CloneBatches must keep showing the batch it was
// given, even after the source buffer is reused for the next job.
func TestCloneBatchesSurvivesSourceReuse(t *testing.T) {
	t.Parallel()

	b, err := NewBatcher[int](2)
	if err != nil {
		t.Fatalf("NewBatcher: %v", err)
	}
	records := []int{1, 2, 3, 4}

	cloned := CloneBatches(b.Split(records))
	want := [][]int{{1, 2}, {3, 4}}
	for i := range cloned {
		if !slices.Equal(cloned[i], want[i]) {
			t.Fatalf("cloned batch %d = %v, want %v", i, cloned[i], want[i])
		}
	}

	records = records[:0]
	records = append(records, 9, 9, 9, 9)

	for i := range cloned {
		if !slices.Equal(cloned[i], want[i]) {
			t.Fatalf("cloned batch %d changed after reuse: got %v, want %v", i, cloned[i], want[i])
		}
	}
}

// ExampleBatcher_Split is the runnable demonstration of this module: it
// splits a slice of records into network-sized batches, then shows that
// cloning them (CloneBatches) is what lets a caller keep a batch past a
// later mutation of the source slice.
func ExampleBatcher_Split() {
	b, err := NewBatcher[int](3)
	if err != nil {
		panic(err)
	}

	records := []int{1, 2, 3, 4, 5, 6, 7}
	for batch := range b.Split(records) {
		fmt.Println(batch)
	}

	kept := CloneBatches(b.Split(records))
	records[0] = 99 // mutating the source no longer affects kept
	fmt.Println(kept[0])

	// Output:
	// [1 2 3]
	// [4 5 6]
	// [7]
	// [1 2 3]
}
```

## Review

`Split` is correct when its aliasing behavior matches what its doc comment
promises -- `TestSplitBatchesAliasSourceBackingArray` pins that a write into
a yielded batch is a write into the source. The mistake this module warns
about is not a bug in `Split` itself; `slices.Chunk`'s aliasing is
documented, intentional, and exactly what makes batching a huge slice cheap.
The mistake is downstream: `retryQueueNaive`, unexported and never part of
the package's API, retains a batch across a point where the caller reuses
its source buffer, and the queued batch silently becomes whatever the next
job wrote there --
`TestNaiveRetryQueueAliasesReusedBuffer` pins that failure numerically.
`CloneBatches` is the fix for exactly that situation: it materializes a
sequence into independently owned batches with `slices.Clone`, and
`TestCloneBatchesSurvivesSourceReuse` confirms it is immune to the identical
reuse. Around that core, `NewBatcher` rejects a non-positive batch size with
`ErrInvalidBatchSize`, and `Batcher` is immutable and safe for concurrent
use once constructed. Run `go test -count=1 -race ./...` to confirm the
split table, the aliasing pin, the retry-queue contrast, and
`ExampleBatcher_Split`.

## Resources

- [`slices.Chunk`](https://pkg.go.dev/slices#Chunk) — the Go 1.23 iterator this module wraps, including its documented aliasing behavior.
- [`iter.Seq`](https://pkg.go.dev/iter#Seq) — the range-over-func iterator type `Split` returns.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the operation `CloneBatches` applies to each yielded batch to make it independently owned.
- [Elasticsearch: Bulk API](https://www.elastic.co/guide/en/elasticsearch/reference/current/docs-bulk.html) — a production bulk-indexing API with exactly this fixed-size-batch shape.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-slices-concat-segment-merge.md](13-slices-concat-segment-merge.md) | Next: [15-header-pass-by-value-filter-return.md](15-header-pass-by-value-filter-return.md)
