# Exercise 9: Stable Pagination Cursor over an In-Memory Index

Paginating an in-memory map is the classic place the randomized iteration order
bites: range the map for page one, range it again for page two, and because the
order differs between ranges you skip some rows and duplicate others. The fix is to
impose a deterministic order once — `slices.Sorted(maps.Keys(index))` — and page
over that sorted key space with a cursor. This module builds that paginator and
proves a full traversal visits every item exactly once.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
pageindex/                  independent module: example.com/pageindex
  go.mod                    go 1.26
  pageindex.go              Item, Page(index, after, limit) ([]Item, nextCursor)
  cmd/
    demo/
      main.go               walk an index page by page, print each page
  pageindex_test.go         full traversal visits each item once, edge cases
```

Files: `pageindex.go`, `cmd/demo/main.go`, `pageindex_test.go`.
Implement: `Page(index map[string]Item, after string, limit int) ([]Item, string)`.
Test: repeated `Page` calls traverse every item exactly once with no duplicates; empty index yields an empty page and empty cursor; a limit larger than the remainder returns the tail; a cursor past the end returns empty.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/09-grouped-index-pagination/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/09-grouped-index-pagination
```

## Why a sorted key space plus a cursor

The bug this exercise exists to prevent is subtle and common. A paginator that
ranges the map directly — take the first `limit` entries this range, the next
`limit` the next range — is broken from the start, because Go randomizes iteration
order per range statement, so "the first `limit`" is a different set of keys each
call. Rows get skipped, rows get returned twice, and the total across all pages does
not equal the number of items.

The correct design derives a single deterministic order and paginates over it. Every
call to `Page` computes `keys := slices.Sorted(maps.Keys(index))` — the same sorted
key space regardless of insertion order or map internals — and the cursor is a key
in that space. Given `after`, the page starts at the first key strictly greater than
`after`; with an empty `after` it starts at the beginning. `slices.BinarySearch`
finds that start position in log time: it returns the index of `after` if present
(so the page starts one past it) or the insertion point if not (the first key
greater than `after`). The page is the next `limit` keys; the next cursor is the
last key of the page, or empty when the page reached the end.

Two details make it robust. `slices.Clip` trims the returned slice's capacity to its
length, so a caller that appends to the returned page cannot accidentally scribble
into the paginator's backing array. And the next cursor is empty exactly when there
are no more items, which is the termination signal a traversal loop watches for — so
a limit larger than the remaining count returns the tail with an empty cursor, and a
cursor already past the end returns an empty page with an empty cursor.

Create `pageindex.go`:

```go
package pageindex

import (
	"maps"
	"slices"
)

// Item is one indexed record.
type Item struct {
	ID    string
	Value string
}

// Page returns up to limit items whose key sorts after the given cursor, in
// deterministic key order, along with the cursor to pass for the next page. The
// next cursor is empty when there are no more items. A non-positive limit returns
// an empty page.
func Page(index map[string]Item, after string, limit int) ([]Item, string) {
	if limit <= 0 {
		return nil, ""
	}
	keys := slices.Sorted(maps.Keys(index))

	start := 0
	if after != "" {
		i, found := slices.BinarySearch(keys, after)
		if found {
			start = i + 1
		} else {
			start = i
		}
	}
	if start >= len(keys) {
		return nil, ""
	}

	end := min(start+limit, len(keys))
	pageKeys := keys[start:end]

	items := make([]Item, 0, len(pageKeys))
	for _, k := range pageKeys {
		items = append(items, index[k])
	}
	items = slices.Clip(items)

	next := ""
	if end < len(keys) {
		next = pageKeys[len(pageKeys)-1]
	}
	return items, next
}
```

`min` is the built-in from Go 1.21; no import needed. The next cursor is set only
when `end < len(keys)`, i.e. there is at least one more key beyond this page — so the
last page correctly reports an empty cursor and the traversal loop stops.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pageindex"
)

func main() {
	index := map[string]pageindex.Item{
		"user:03": {ID: "user:03", Value: "carol"},
		"user:01": {ID: "user:01", Value: "alice"},
		"user:05": {ID: "user:05", Value: "erin"},
		"user:02": {ID: "user:02", Value: "bob"},
		"user:04": {ID: "user:04", Value: "dave"},
	}

	cursor := ""
	page := 1
	for {
		items, next := pageindex.Page(index, cursor, 2)
		if len(items) == 0 {
			break
		}
		fmt.Printf("page %d:", page)
		for _, it := range items {
			fmt.Printf(" %s", it.Value)
		}
		fmt.Println()
		if next == "" {
			break
		}
		cursor = next
		page++
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page 1: alice bob
page 2: carol dave
page 3: erin
```

Every user appears exactly once, in a stable order, across three pages of two —
regardless of the randomized order the map would range in.

### Tests

`TestFullTraversalVisitsEachOnce` is the property test: walk the whole index page by
page and assert every item is seen exactly once, with no duplicates and a total
equal to the index size. The remaining tests pin the edge cases — empty index, a
limit larger than the remainder, and a cursor past the end.

Create `pageindex_test.go`:

```go
package pageindex

import (
	"fmt"
	"strconv"
	"testing"
)

func buildIndex(n int) map[string]Item {
	idx := make(map[string]Item, n)
	for i := range n {
		id := fmt.Sprintf("k%03d", i)
		idx[id] = Item{ID: id, Value: strconv.Itoa(i)}
	}
	return idx
}

func TestFullTraversalVisitsEachOnce(t *testing.T) {
	t.Parallel()

	const n = 25
	index := buildIndex(n)

	seen := make(map[string]int)
	cursor := ""
	pages := 0
	for {
		items, next := Page(index, cursor, 4)
		if len(items) == 0 {
			break
		}
		pages++
		for _, it := range items {
			seen[it.ID]++
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != n {
		t.Fatalf("visited %d distinct items, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("item %s visited %d times, want exactly 1", id, count)
		}
	}
}

func TestEmptyIndex(t *testing.T) {
	t.Parallel()

	items, next := Page(map[string]Item{}, "", 10)
	if len(items) != 0 || next != "" {
		t.Fatalf("Page(empty) = %v, %q; want empty, empty cursor", items, next)
	}
}

func TestLimitLargerThanRemainder(t *testing.T) {
	t.Parallel()

	index := buildIndex(3)
	items, next := Page(index, "", 100)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (the whole tail)", len(items))
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty at the tail", next)
	}
}

func TestCursorPastEnd(t *testing.T) {
	t.Parallel()

	index := buildIndex(3)
	items, next := Page(index, "k999", 10)
	if len(items) != 0 || next != "" {
		t.Fatalf("Page past end = %v, %q; want empty, empty cursor", items, next)
	}
}

func TestNonPositiveLimit(t *testing.T) {
	t.Parallel()

	index := buildIndex(3)
	if items, next := Page(index, "", 0); items != nil || next != "" {
		t.Fatalf("Page(limit=0) = %v, %q; want nil, empty", items, next)
	}
}

func ExamplePage() {
	index := map[string]Item{
		"b": {ID: "b", Value: "2"},
		"a": {ID: "a", Value: "1"},
		"c": {ID: "c", Value: "3"},
	}
	items, next := Page(index, "", 2)
	fmt.Println(items[0].ID, items[1].ID, "next:", next)
	// Output: a b next: b
}
```

## Review

The paginator is correct when a full traversal visits every item exactly once with
no gaps or repeats — the property `TestFullTraversalVisitsEachOnce` asserts — and
that holds only because every page is derived from the same
`slices.Sorted(maps.Keys(index))` order rather than a raw map range. The cursor is a
key in that sorted space, `slices.BinarySearch` locates the start in log time, and
an empty next cursor is the unambiguous end-of-data signal that stops the loop. If a
paginator over a map ever duplicates or skips rows, the cause is always a missing
sort — someone ranged the map directly. `slices.Clip` on the returned page keeps a
caller's `append` from reaching into the paginator's buffer. Run `go test -race`.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Keys`.
- [slices package](https://pkg.go.dev/slices) — `Sorted`, `BinarySearch`, `Clip`.
- [Go spec: for statements with range clause](https://go.dev/ref/spec#For_range) — map iteration order is unspecified.

---

Back to [08-set-operations-scopes.md](08-set-operations-scopes.md) | Next: [10-postings-index-inverted-search.md](10-postings-index-inverted-search.md)
