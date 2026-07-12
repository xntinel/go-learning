# Exercise 15: Parallel Export Fan-Out — `for i := range numPages` Generating Offset/Limit Windows

**Nivel: Intermedio** — validacion rapida (un test corto).

Splitting a large table export across N parallel workers needs a static list of
non-overlapping `(offset, limit)` windows computed up front, not a live paginated
API to consume. This exercise builds `OffsetPages`, which computes the page count
once by ceiling division and then drives an `iter.Seq2[int, int]` with a plain
`for i := range numPages` counted loop.

## What you'll build

```text
fanout/                   independent module: example.com/fanout
  go.mod                  module example.com/fanout
  fanout.go                OffsetPages
  fanout_test.go           remainder, exact multiple, empty, early stop, panic
```

Implement: `OffsetPages(total, pageSize int) iter.Seq2[int, int]` yielding `(offset, limit)` windows covering `[0, total)`; panics if `pageSize < 1`.
Test: 25 rows at page size 10 yields `(0,10),(10,10),(20,5)`; an exact multiple has no partial window; `total=0` yields nothing; a consumer break after two windows stops there; `pageSize=0` panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

Create `fanout.go`:

```go
package fanout

import "iter"

// OffsetPages returns an iter.Seq2 of (offset, limit) windows that cover the
// range [0, total) in pageSize-sized chunks, for splitting a large table scan
// into independent ranges ("SELECT ... OFFSET offset LIMIT limit") that a
// fixed number of parallel export workers fetch concurrently. The number of
// windows is computed once by ceiling division and then driven with a plain
// `for i := range numPages` counted loop; the final window's limit is the
// remainder and may be smaller than pageSize. pageSize must be >= 1.
func OffsetPages(total, pageSize int) iter.Seq2[int, int] {
	if pageSize < 1 {
		panic("fanout: pageSize must be >= 1")
	}
	numPages := (total + pageSize - 1) / pageSize
	return func(yield func(int, int) bool) {
		for i := range numPages {
			offset := i * pageSize
			limit := pageSize
			if remaining := total - offset; remaining < limit {
				limit = remaining
			}
			if !yield(offset, limit) {
				return
			}
		}
	}
}
```

Create `fanout_test.go`:

```go
package fanout

import "testing"

type window struct {
	offset, limit int
}

func collect(total, pageSize int) []window {
	var got []window
	for offset, limit := range OffsetPages(total, pageSize) {
		got = append(got, window{offset, limit})
	}
	return got
}

func equalWindows(a, b []window) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOffsetPagesWithRemainder(t *testing.T) {
	t.Parallel()

	got := collect(25, 10)
	want := []window{{0, 10}, {10, 10}, {20, 5}}
	if !equalWindows(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestOffsetPagesEmptyTotal(t *testing.T) {
	t.Parallel()

	if got := collect(0, 10); len(got) != 0 {
		t.Fatalf("got = %v, want no windows for total 0", got)
	}
}

func TestOffsetPagesStopsEarly(t *testing.T) {
	t.Parallel()

	var got []window
	for offset, limit := range OffsetPages(1000, 10) {
		got = append(got, window{offset, limit})
		if len(got) == 2 {
			break
		}
	}
	want := []window{{0, 10}, {10, 10}}
	if !equalWindows(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestOffsetPagesPanicsOnInvalidPageSize(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for pageSize < 1")
		}
	}()
	OffsetPages(10, 0)
}
```

## Verify

```bash
go test -count=1 ./...
```

## Review

Computing `numPages` before returning the iterator is what makes `for i := range
numPages` the right tool here: the total amount of work is known up front, unlike
the live-pagination exercise earlier in this lesson where the next page token only
exists after a network round-trip. Deriving `offset` as `i * pageSize` instead of
carrying a running accumulator keeps every window's bounds a pure function of its
index, so a caller can hand window 7 to worker 7 without replaying windows 0
through 6 first — the property that actually makes fan-out to parallel workers
possible. The `remaining < limit` check is the one place a plain multiply is
wrong: the last window must be clamped to what is actually left.

## Resources

- [Go spec: `for` range clause (integers)](https://go.dev/ref/spec#For_range)
- [`iter.Seq2` documentation](https://pkg.go.dev/iter#Seq2)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-byte-budget-capped-export.md](14-byte-budget-capped-export.md) | Next: [16-token-bucket-rate-limiter.md](16-token-bucket-rate-limiter.md)
