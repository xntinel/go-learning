# Exercise 3: Fixed-Size Batch Chunking for a Bulk Insert

Turning N rows into a series of bounded bulk INSERT statements is a repository-layer
chore that shows up in every data pipeline: databases cap the number of parameters
per statement, message brokers cap batch bytes, and downstream APIs cap request
size. The task is a counted loop that steps by more than one — `i += size` — and
the whole correctness story lives in the slice math for the final short chunk.
This module builds `ChunkInsert` and proves it never over-slices.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
batch/                       module example.com/batch
  go.mod
  batch.go                   ChunkInsert[T](records, size, flush) using i += size and min
  batch_test.go              table-driven: multiples, short final chunk, empty, oversized size, invalid size
  cmd/demo/
    main.go                  chunks 7 rows into batches of 3 and prints each flush
```

- Files: `batch.go`, `batch_test.go`, `cmd/demo/main.go`.
- Implement: `ChunkInsert[T any](records []T, size int, flush func([]T) error) error` with a `for i := 0; i < len(records); i += size` loop, `min(i+size, len(records))` for the upper bound, and `errors.Join` for accumulated flush errors.
- Test: exact multiples, non-multiples, empty input (zero flushes), size larger than len (one chunk), `size <= 0` rejected with a sentinel, batches reconstruct the input, flush called ceil(n/size) times, a flush error surfaced.
- Verify: `go test -count=1 -race ./...`

### Why the final chunk is the whole problem

The loop is `for i := 0; i < len(records); i += size`. The index does not advance
by one; it jumps by `size`, so on each iteration `records[i:i+size]` would be the
batch — except on the last iteration, where `i+size` can run past the end of the
slice and panic. The fix is a half-open bound clamped with the `min` builtin:
`end := min(i+size, len(records))`, then `batch := records[i:end]`. Go slices are
half-open (`records[i:end]` includes `i`, excludes `end`), so a batch of exactly
`size` elements is `records[i : i+size]` and the final short batch is
`records[i : len(records)]` — no off-by-one, no panic, no empty trailing batch.

Two guards make the function total. A non-positive `size` is nonsense (a chunk
size of zero would loop forever), so it is rejected up front with a sentinel
`ErrInvalidSize`. And flush errors are accumulated with `errors.Join` rather than
returned on the first failure, because a bulk pipeline usually wants to attempt
every batch and report all the failures at once; `errors.Join(nil, nil)` is `nil`,
so a fully successful run returns no error naturally.

The number of flushes is exactly `ceil(len(records) / size)`: an empty input
flushes zero times, `size >= len(records)` flushes once, and the final short chunk
counts as one flush.

Create `batch.go`:

```go
package batch

import "errors"

// ErrInvalidSize means a non-positive chunk size was passed.
var ErrInvalidSize = errors.New("chunk size must be positive")

// ChunkInsert slices records into fixed-size batches and calls flush on each,
// as a bulk insert would. The final batch is short when len(records) is not a
// multiple of size. flush errors are accumulated and returned joined; a
// successful run returns nil. A non-positive size returns ErrInvalidSize and
// flushes nothing.
func ChunkInsert[T any](records []T, size int, flush func([]T) error) error {
	if size <= 0 {
		return ErrInvalidSize
	}
	var errs []error
	for i := 0; i < len(records); i += size {
		end := min(i+size, len(records))
		if err := flush(records[i:end]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo chunks seven rows into batches of three, showing two full batches and one
short final batch of one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batch"
)

func main() {
	rows := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6"}

	flushes := 0
	err := batch.ChunkInsert(rows, 3, func(b []string) error {
		flushes++
		fmt.Printf("flush %d: %d rows %v\n", flushes, len(b), b)
		return nil
	})
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Printf("done in %d flushes\n", flushes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
flush 1: 3 rows [r0 r1 r2]
flush 2: 3 rows [r3 r4 r5]
flush 3: 1 rows [r6]
done in 3 flushes
```

### Tests

The suite is table-driven over the shapes that matter: exact multiples,
non-multiples (short final chunk), empty input, `size` larger than the input, and
an invalid `size`. Each case's fake flush records the batches so the test can
assert two things at once — the flush count equals `ceil(n/size)`, and
concatenating the recorded batches reconstructs the original input exactly (proving
nothing was dropped or duplicated). A separate test injects a flush error and
asserts it is surfaced.

Create `batch_test.go`:

```go
package batch

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestChunkInsert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		n           int
		size        int
		wantFlushes int
	}{
		{"exact multiple", 6, 3, 2},
		{"short final chunk", 7, 3, 3},
		{"single element", 1, 3, 1},
		{"size larger than len", 2, 10, 1},
		{"size one", 4, 1, 4},
		{"empty input", 0, 3, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			records := make([]int, tc.n)
			for i := range records {
				records[i] = i
			}

			var batches [][]int
			err := ChunkInsert(records, tc.size, func(b []int) error {
				batches = append(batches, slices.Clone(b))
				return nil
			})
			if err != nil {
				t.Fatalf("ChunkInsert() = %v, want nil", err)
			}
			if len(batches) != tc.wantFlushes {
				t.Fatalf("flushes = %d, want %d", len(batches), tc.wantFlushes)
			}
			got := slices.Concat(batches...)
			if !slices.Equal(got, records) {
				t.Fatalf("reassembled = %v, want %v", got, records)
			}
			for i, b := range batches {
				if len(b) == 0 || len(b) > tc.size {
					t.Fatalf("batch %d has %d elements, want 1..%d", i, len(b), tc.size)
				}
			}
		})
	}
}

func TestChunkInsertRejectsInvalidSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1, -10} {
		called := false
		err := ChunkInsert([]int{1, 2, 3}, size, func([]int) error {
			called = true
			return nil
		})
		if !errors.Is(err, ErrInvalidSize) {
			t.Fatalf("size=%d: err = %v, want ErrInvalidSize", size, err)
		}
		if called {
			t.Fatalf("size=%d: flush must not be called", size)
		}
	}
}

func TestChunkInsertJoinsFlushError(t *testing.T) {
	t.Parallel()

	boom := errors.New("bulk insert failed")
	records := []int{0, 1, 2, 3, 4}

	err := ChunkInsert(records, 2, func(b []int) error {
		if b[0] == 2 { // fail the middle batch only
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to carry boom", err)
	}
}

func ExampleChunkInsert() {
	err := ChunkInsert([]int{1, 2, 3, 4, 5}, 2, func(b []int) error {
		fmt.Println(b)
		return nil
	})
	fmt.Println("err:", err)
	// Output:
	// [1 2]
	// [3 4]
	// [5]
	// err: <nil>
}
```

## Review

The chunker is correct when the upper bound of every batch is `min(i+size,
len(records))` and the slice indices are half-open, so the final short chunk is
sliced exactly to `len(records)` with no panic and no empty trailing batch. The
proof against silent data loss is `TestChunkInsert`: for every shape,
`slices.Concat` of the recorded batches equals the original input, and the flush
count equals `ceil(n/size)`. A non-positive `size` is rejected before the loop —
never looped on, which would hang — and flush errors accumulate through
`errors.Join` so a partial failure surfaces without aborting the remaining
batches. The common trap this guards against is `records[i:i+size]` without the
`min` clamp, which panics on the last iteration of any non-multiple input. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — half-open `a[low:high]` bounds.
- [Go Specification: Min and max](https://go.dev/ref/spec#Min_and_max) — the `min` builtin used to bound the final chunk.
- [slices package](https://pkg.go.dev/slices) — `Concat`, `Clone`, and `Equal` used to verify reassembly.
- [errors.Join](https://pkg.go.dev/errors#Join) — accumulating per-batch flush errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-exponential-backoff-retry.md](02-exponential-backoff-retry.md) | Next: [04-cursor-pagination-drain.md](04-cursor-pagination-drain.md)
