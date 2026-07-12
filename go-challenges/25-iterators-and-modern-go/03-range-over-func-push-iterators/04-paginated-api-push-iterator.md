# Exercise 4: A Lazy Push Iterator Over a Paginated API

Most real data does not arrive as one slice. An HTTP API hands it back a page at
a time, each response carrying a cursor that points at the next page. This
exercise wraps that paginated source in a single `iter.Seq[Item]` that yields
items across page boundaries, fetches each page only when the consumer reaches
it, and -- the architecturally important part -- stops fetching the moment the
consumer breaks. The test proves laziness by counting fetches.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
pageiter.go          a fake cursor-paginated client and Client.Items (iter.Seq)
cmd/
  demo/
    main.go          walk every item across pages, then break early
pageiter_test.go     full walk, fetch-count == page-count, early break fetches fewer pages
```

- Files: `pageiter.go`, `cmd/demo/main.go`, `pageiter_test.go`.
- Implement: a fake `Client` whose `fetch(cursor)` returns one `page` (items plus
  a next cursor) and increments a `Fetched` counter, and `Client.Items()
  iter.Seq[Item]` that yields every item across pages, fetching lazily and
  honoring the yield protocol.
- Test: `pageiter_test.go` checks the full walk returns every item in order,
  checks a complete walk fetches exactly one page per page, and -- the
  load-bearing case -- breaks early and asserts only the pages actually needed
  were fetched.
- Verify: `go test -run TestItems -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/04-paginated-api-push-iterator/cmd/demo && cd go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/04-paginated-api-push-iterator
```

### The shape of a cursor-paginated source

A cursor-paginated API exposes one operation: give it a cursor, it returns a
batch of items and the cursor for the next batch. The first request sends an
empty cursor; the response whose next cursor is empty is the last page. Nothing
in the protocol tells the client how many pages exist up front -- you discover
the end only by following cursors until one comes back empty. That is precisely
why a push iterator fits: the iterator can keep an opaque cursor in a local
variable and decide, on each turn of its loop, whether another network round-trip
is even warranted.

The fake client models the network without one. `pages` maps a cursor to the
`page` reachable by it, and `fetch` increments `Fetched` every time it is called
so a test can see exactly how many round-trips the iterator made:

```go
func (c *Client) fetch(cursor string) page {
	c.Fetched++
	return c.pages[cursor]
}
```

That counter is the whole point of the exercise. In production the analogous cost
is a real HTTP call -- latency, rate-limit budget, money. An iterator that fetches
pages it never needed is a correctness-adjacent bug that a slice-returning API
would hide, because returning `[]Item` forces every page to be fetched before the
caller sees the first element.

### Yielding across page boundaries, lazily

The iterator runs an outer loop over pages and an inner loop over the items of
the current page. The cursor starts empty; after draining a page it advances to
that page's next cursor and only then fetches again. Two early exits matter. The
inner `if !yield(it) { return }` is the yield protocol: when the consumer breaks,
the iterator returns immediately -- and because the next `fetch` lives past that
`return`, breaking mid-page means the following page is never requested. The outer
`if pg.next == "" { return }` ends the walk naturally when the source signals the
last page, before a pointless fetch of an empty cursor.

```go
func (c *Client) Items() iter.Seq[Item] {
	return func(yield func(Item) bool) {
		cursor := ""
		for {
			pg := c.fetch(cursor)
			for _, it := range pg.items {
				if !yield(it) {
					return
				}
			}
			if pg.next == "" {
				return
			}
			cursor = pg.next
		}
	}
}
```

Read the laziness off the control flow. No page is fetched until the outer loop
reaches it, and the outer loop only reaches a page after the inner loop has fully
drained the previous one without the consumer breaking. So a consumer that takes
the first three items from a source paged two-at-a-time touches exactly two pages
-- the first to supply items one and two, the second to supply item three -- and
the iterator returns out of the inner loop before the third `fetch` ever runs.
The pages beyond that are never materialized. This is the difference between an
iterator and a function returning `[]Item`: the iterator lets the consumer's
`break` reach back and prune work the producer would otherwise have done.

Create `pageiter.go`:

```go
// Package pageiter wraps a fake cursor-paginated API in a single iter.Seq that
// yields items across page boundaries and fetches each page only on demand.
package pageiter

import (
	"fmt"
	"iter"
)

// Item is one record returned by the API.
type Item struct {
	ID   int
	Name string
}

// page is one API response: a batch of items and the cursor for the next page.
// An empty next cursor marks the last page.
type page struct {
	items []Item
	next  string
}

// Client is a fake paginated API client. Fetched counts how many page requests
// the iterator actually made, so a test can prove the walk is lazy.
type Client struct {
	pages   map[string]page
	Fetched int
}

// NewClient splits items into pages of pageSize and wires up the cursors. The
// first page is reached by the empty cursor; each later page by "c1", "c2", ...
func NewClient(pageSize int, items ...Item) *Client {
	if pageSize < 1 {
		pageSize = 1
	}
	var chunks [][]Item
	for i := 0; i < len(items); i += pageSize {
		end := min(i+pageSize, len(items))
		chunks = append(chunks, items[i:end])
	}
	if len(chunks) == 0 {
		chunks = [][]Item{nil}
	}
	c := &Client{pages: make(map[string]page, len(chunks))}
	for i, ch := range chunks {
		cursor := ""
		if i > 0 {
			cursor = fmt.Sprintf("c%d", i)
		}
		next := ""
		if i+1 < len(chunks) {
			next = fmt.Sprintf("c%d", i+1)
		}
		c.pages[cursor] = page{items: ch, next: next}
	}
	return c
}

// fetch returns the page reached by cursor and records the request. In a real
// client this is the network round-trip the iterator is trying not to waste.
func (c *Client) fetch(cursor string) page {
	c.Fetched++
	return c.pages[cursor]
}

// Items yields every item across every page, fetching each page only when the
// consumer reaches it and stopping all further fetches when the consumer breaks.
func (c *Client) Items() iter.Seq[Item] {
	return func(yield func(Item) bool) {
		cursor := ""
		for {
			pg := c.fetch(cursor)
			for _, it := range pg.items {
				if !yield(it) {
					return
				}
			}
			if pg.next == "" {
				return
			}
			cursor = pg.next
		}
	}
}
```

### The runnable demo

The demo builds a client over six items paged two at a time, walks them all to
show the iterator stitching three pages into one flat sequence, and reports the
fetch count. Then it builds a fresh client and breaks after three items to show
the fetch count stay at two -- the later pages never requested.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pageiter"
)

func main() {
	items := []pageiter.Item{
		{ID: 1, Name: "ada"}, {ID: 2, Name: "bob"},
		{ID: 3, Name: "cy"}, {ID: 4, Name: "dot"},
		{ID: 5, Name: "eve"}, {ID: 6, Name: "fin"},
	}

	full := pageiter.NewClient(2, items...)
	fmt.Print("all:")
	for it := range full.Items() {
		fmt.Printf(" %s", it.Name)
	}
	fmt.Printf("\nfetched %d pages\n", full.Fetched)

	early := pageiter.NewClient(2, items...)
	fmt.Print("first 3:")
	n := 0
	for it := range early.Items() {
		if n == 3 {
			break
		}
		fmt.Printf(" %s", it.Name)
		n++
	}
	fmt.Printf("\nfetched %d pages\n", early.Fetched)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all: ada bob cy dot eve fin
fetched 3 pages
first 3: ada bob cy
fetched 2 pages
```

### Tests

`TestItemsAll` walks the whole source and asserts every item comes back in order
across the page boundaries. `TestItemsFetchCount` walks fully and asserts the
fetch count equals the page count -- one round-trip per page, no more.
`TestItemsEarlyBreakStopsFetching` is the load-bearing case: it breaks after three
items from a source paged two-at-a-time and asserts `Fetched == 2`, proving the
iterator never requested the three pages it did not need. A slice-returning API
could not pass this test, because it would have to fetch everything before
returning.

Create `pageiter_test.go`:

```go
package pageiter

import (
	"testing"
)

func sample() []Item {
	return []Item{
		{ID: 1, Name: "ada"}, {ID: 2, Name: "bob"},
		{ID: 3, Name: "cy"}, {ID: 4, Name: "dot"},
		{ID: 5, Name: "eve"}, {ID: 6, Name: "fin"},
		{ID: 7, Name: "gil"}, {ID: 8, Name: "hua"},
		{ID: 9, Name: "ivo"}, {ID: 10, Name: "jo"},
	}
}

func TestItemsAll(t *testing.T) {
	t.Parallel()

	c := NewClient(2, sample()...)
	var got []int
	for it := range c.Items() {
		got = append(got, it.ID)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("walked %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestItemsFetchCount(t *testing.T) {
	t.Parallel()

	c := NewClient(2, sample()...) // 10 items, 2 per page => 5 pages
	for range c.Items() {
	}
	if c.Fetched != 5 {
		t.Fatalf("full walk fetched %d pages, want 5", c.Fetched)
	}
}

func TestItemsEarlyBreakStopsFetching(t *testing.T) {
	t.Parallel()

	c := NewClient(2, sample()...)
	var got []int
	for it := range c.Items() {
		if len(got) == 3 {
			break
		}
		got = append(got, it.ID)
	}
	// The third yielded item triggers the break; it lives on page two, so the
	// iterator must have fetched exactly pages one and two and no further.
	if c.Fetched != 2 {
		t.Fatalf("early break fetched %d pages, want 2", c.Fetched)
	}
}
```

## Review

The paginated iterator is correct when two properties hold at once: a full walk
returns every item in cursor order, and an early break prunes the fetches the
consumer never needed. The first lives in the outer loop following `pg.next`
until it is empty; `TestItemsAll` and `TestItemsFetchCount` together check the
items and that the walk costs exactly one fetch per page. The second lives in the
inner `if !yield(it) { return }` sitting before the next `fetch`;
`TestItemsEarlyBreakStopsFetching` asserting `Fetched == 2` is the proof that the
break reached back into the producer and stopped it. Confirm a fresh `Client` per
test, since `Fetched` is stateful and shared state would make the counts lie.

Common mistakes for this feature. The first is fetching eagerly -- looping every
page into a slice and then ranging the slice -- which gives the right items but
defeats the entire purpose, because the early-break test then fetches all five
pages. The second is forgetting the `if pg.next == "" { return }` guard and
fetching one empty page past the end, an off-by-one that inflates the fetch count
and, against a real API, makes a needless round-trip on every walk. The third is
checking the bool of `yield` only at the end of a page instead of after every
item, which delays the stop until the page boundary and can fetch one page too
many.

## Resources

- [`iter` package](https://pkg.go.dev/iter) -- `Seq[V]` and the yield contract the
  lazy page walk must honor.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) -- how
  a consumer's `break` becomes a `false` return from `yield`, which is what lets
  the iterator skip the remaining fetches.
- [GitHub REST API: pagination](https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api)
  -- a real cursor/link paginated API with the same fetch-the-next-page model.

---

Back to [03-tree-in-order-iterator.md](03-tree-in-order-iterator.md) | Next: [05-database-cursor-seq2-iterator.md](05-database-cursor-seq2-iterator.md)
