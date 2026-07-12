# Exercise 5: Batch Pagination for a Bulk API

Most bulk endpoints cap how many records one request may carry, so a large input has to be cut into fixed-size pages. This exercise builds a `batch` package whose `PageCount` computes how many requests a payload needs and whose `Pages` slices the input into those pages, with the page index driving the math through `for i := range count`. The last page may be a short remainder, an empty input needs zero pages, and a non-positive page size is rejected.

This module is fully self-contained. It has its own `go mod init`, defines every function it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
batch.go             PageCount (ceil division), Pages (generic windowing), ErrInvalidPageSize
cmd/
  demo/
    main.go          page a record set, show one request per page, reject size 0
batch_test.go        remainder, exact multiple, empty input, invalid size, capacity isolation
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `PageCount(total, size int) (int, error)`, `Pages[T any](items []T, size int) ([][]T, error)`, and the sentinel `ErrInvalidPageSize`.
- Test: pin the page count for empty, exact-multiple, and remainder inputs; pin the page contents including the short final page; reject a non-positive size; and prove each page's capacity is bounded so appending to one page cannot corrupt the next.
- Verify: `go test -count=1 -race ./...`

### Why the count is a ceiling, and why each page is capacity-bounded

The bulk API has a hard per-request ceiling: send it at most `size` records or it rejects the call. Covering `total` records therefore takes `ceil(total / size)` requests, and the remainder is the trap. Plain integer division `total / size` truncates: 10 records at 4 per page is `10 / 4 == 2`, but two pages of four only carry eight records — the last two would be silently dropped. The fix is the standard ceiling-division idiom `(total + size - 1) / size`, which rounds up without floating point: `(10 + 3) / 4 == 3`. Computing the count up front, before any slicing, lets a caller size a progress bar or a rate-limit budget without materializing the pages, and it is the loop bound `Pages` ranges over.

`Pages` turns that count into the actual windows. The page index `i` runs `for i := range count`, and each page spans `items[i*size : (i+1)*size]`, clamped so the final page stops at `len(items)` instead of running past the end. This is exactly where `for i := range count` earns its place over a `for start := 0; start < len(items); start += size` loop: the index *is* the page number, the start offset is `i*size`, and the bound is the count the caller already trusts, so off-by-one mistakes have nowhere to hide. A one-based "request 1, request 2" presentation is just `i+1` at the call site; the iterator stays zero-based.

The capacity detail is what separates a correct windowing helper from a subtly broken one. A sub-slice `items[start:end]` shares the backing array with `items` and, crucially, inherits capacity all the way to the end of that array. If a caller then `append`s to a page that has spare capacity, the append writes *in place* into the backing array — straight over the first elements of the next page. The three-index slice `items[start:end:end]` caps each page's capacity to its own length, so any append is forced to allocate a fresh array and the pages become mutation-isolated. For a bulk-API workflow, where each page is commonly decorated (a trailing checksum record, a per-batch envelope) before being sent, this isolation is the difference between independent requests and a corruption bug that only appears under append. The standard library's `slices.Chunk` (Go 1.23) makes the same guarantee — "all but the last sub-slice will have size n, all clipped to have no capacity beyond their length" — and this exercise reproduces that contract by hand.

The two boundaries are explicit. An empty input has `PageCount(0, size) == 0`, so `Pages` ranges zero times and returns an empty, non-nil `[][]T` — zero requests, not one empty request. A non-positive `size` is meaningless (a page must hold at least one record) and is rejected with `ErrInvalidPageSize`, wrapped with `%w` so callers can match it with `errors.Is`.

Create `batch.go`:

```go
package batch

import (
	"errors"
	"fmt"
)

// ErrInvalidPageSize is returned when a page size is not positive. A page must
// hold at least one record, so zero and negative sizes are rejected rather than
// treated as "no limit".
var ErrInvalidPageSize = errors.New("page size must be positive")

// PageCount returns how many fixed-size pages are needed to cover total items:
// the ceiling of total/size. An empty input (total <= 0) needs zero pages.
func PageCount(total, size int) (int, error) {
	if size <= 0 {
		return 0, fmt.Errorf("page size %d: %w", size, ErrInvalidPageSize)
	}
	if total <= 0 {
		return 0, nil
	}
	return (total + size - 1) / size, nil
}

// Pages splits items into consecutive pages of at most size elements. All pages
// but the last hold exactly size elements; the last holds the remainder. An
// empty input yields an empty, non-nil slice of pages. Each page's capacity is
// capped to its length, so appending to one page never overwrites another.
func Pages[T any](items []T, size int) ([][]T, error) {
	count, err := PageCount(len(items), size)
	if err != nil {
		return nil, err
	}

	pages := make([][]T, 0, count)
	for i := range count {
		start := i * size
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		pages = append(pages, items[start:end:end])
	}
	return pages, nil
}
```

`PageCount` is the single source of truth for the count, and `Pages` calls it so the loop bound and any caller-facing count can never disagree. The `end > len(items)` clamp is what makes the final page a short remainder instead of an out-of-range slice. The three-index `items[start:end:end]` sets each page's capacity equal to its length.

### The runnable demo

The demo pages ten records four-per-request, prints one line per request with a one-based label, and shows the rejected zero size. The output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/batch"
)

func main() {
	records := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	const pageSize = 4

	count, err := batch.PageCount(len(records), pageSize)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d records, %d per request => %d requests\n", len(records), pageSize, count)

	pages, err := batch.Pages(records, pageSize)
	if err != nil {
		log.Fatal(err)
	}
	for i, page := range pages {
		fmt.Printf("request %d: %v\n", i+1, page)
	}

	if _, err := batch.Pages(records, 0); err != nil {
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
10 records, 4 per request => 3 requests
request 1: [a b c d]
request 2: [e f g h]
request 3: [i j]
rejected: page size must be positive
```

### Tests

`TestPageCount` is a table that pins the ceiling math across the cases that matter: empty input (zero pages), an exact multiple, a remainder, a single partial page when the size exceeds the total, and one-per-page. `TestPagesRemainder` and `TestPagesExactMultiple` pin the actual page contents, including the short final page. `TestPagesEmpty` proves an empty input yields zero pages rather than one empty page. `TestInvalidSize` drives both functions through a non-positive size and matches `ErrInvalidPageSize` with `errors.Is`. `TestPagesCapacityIsolation` is the one that justifies the three-index slice: it appends to the first page and asserts the second page is untouched, which fails if the pages share spare capacity.

Create `batch_test.go`:

```go
package batch

import (
	"errors"
	"reflect"
	"testing"
)

func TestPageCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		total, size int
		want        int
	}{
		{name: "empty", total: 0, size: 5, want: 0},
		{name: "exact multiple", total: 9, size: 3, want: 3},
		{name: "remainder", total: 10, size: 3, want: 4},
		{name: "single partial page", total: 2, size: 5, want: 1},
		{name: "one per page", total: 4, size: 1, want: 4},
	}

	for _, tt := range tests {
		got, err := PageCount(tt.total, tt.size)
		if err != nil {
			t.Fatalf("%s: PageCount(%d, %d) error = %v", tt.name, tt.total, tt.size, err)
		}
		if got != tt.want {
			t.Fatalf("%s: PageCount(%d, %d) = %d, want %d", tt.name, tt.total, tt.size, got, tt.want)
		}
	}
}

func TestPagesRemainder(t *testing.T) {
	t.Parallel()

	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	pages, err := Pages(items, 3)
	if err != nil {
		t.Fatalf("Pages error = %v", err)
	}
	want := [][]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {9}}
	if !reflect.DeepEqual(pages, want) {
		t.Fatalf("Pages = %v, want %v", pages, want)
	}
}

func TestPagesExactMultiple(t *testing.T) {
	t.Parallel()

	items := []int{1, 2, 3, 4, 5, 6}
	pages, err := Pages(items, 2)
	if err != nil {
		t.Fatalf("Pages error = %v", err)
	}
	want := [][]int{{1, 2}, {3, 4}, {5, 6}}
	if !reflect.DeepEqual(pages, want) {
		t.Fatalf("Pages = %v, want %v", pages, want)
	}
}

func TestPagesEmpty(t *testing.T) {
	t.Parallel()

	pages, err := Pages([]int{}, 4)
	if err != nil {
		t.Fatalf("Pages error = %v", err)
	}
	if pages == nil || len(pages) != 0 {
		t.Fatalf("Pages(empty) = %v, want empty non-nil slice", pages)
	}
}

func TestInvalidSize(t *testing.T) {
	t.Parallel()

	if _, err := PageCount(10, 0); !errors.Is(err, ErrInvalidPageSize) {
		t.Fatalf("PageCount size 0 err = %v, want ErrInvalidPageSize", err)
	}
	if _, err := Pages([]int{1, 2}, -1); !errors.Is(err, ErrInvalidPageSize) {
		t.Fatalf("Pages size -1 err = %v, want ErrInvalidPageSize", err)
	}
}

func TestPagesCapacityIsolation(t *testing.T) {
	t.Parallel()

	items := []int{1, 2, 3, 4, 5}
	pages, err := Pages(items, 2)
	if err != nil {
		t.Fatalf("Pages error = %v", err)
	}

	// Appending to the first page must allocate, not write into the next page's
	// region of the backing array. This holds only because each page's capacity
	// is capped to its length by the three-index slice.
	_ = append(pages[0], 99)
	if !reflect.DeepEqual(pages[1], []int{3, 4}) {
		t.Fatalf("append to page 0 corrupted page 1: got %v, want [3 4]", pages[1])
	}
}
```

## Review

The package is correct when the count rounds up, the final page is the remainder, and the pages are mutation-isolated. `PageCount` uses `(total + size - 1) / size` so a remainder never drops records, and returns zero for an empty input. `Pages` ranges `for i := range count` with the page index driving `start = i*size`, clamps the last page to `len(items)`, and caps each page's capacity with the three-index slice so an append to one page allocates rather than overwriting the next. A non-positive size is rejected with a `%w`-wrapped `ErrInvalidPageSize` that `errors.Is` matches.

The traps this code avoids: truncating with plain `total / size` and silently dropping the remainder; running the final window past `len(items)` and panicking; returning one empty page for empty input instead of zero pages; and handing out pages that share spare capacity, so that decorating one page with `append` corrupts the next. The capacity-isolation test, together with the remainder and empty-input tests, establishes that the count, the windows, and the isolation all hold. The standard library's `slices.Chunk` provides the same behavior as an iterator; building it by hand here makes the ceiling math and the capacity contract explicit.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the rule that `for i := range count` binds i to 0..count-1, used here as the page index.
- [`slices.Chunk`](https://pkg.go.dev/slices#Chunk) — the Go 1.23 standard-library function that windows a slice into fixed-size, capacity-clipped chunks; this exercise reproduces its contract by hand.
- [Go Slices: usage and internals](https://go.dev/blog/slices-intro) — how a sub-slice shares the backing array and inherits capacity, the mechanism the three-index slice bounds.

---

Back to [04-worker-pool-dispatcher.md](04-worker-pool-dispatcher.md) | Next: [../02-loopvar-semantic-change/00-concepts.md](../02-loopvar-semantic-change/00-concepts.md)
