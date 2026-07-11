# Exercise 5: A Paginated Repository Behind One iter.Seq2[Item, error]

Cursor pagination is a backend detail that should never reach a caller. This exercise builds a repository whose entire public surface is one method, `All() iter.Seq2[Item, error]`, that walks every record across every page. The consumer writes a single `for item, err := range repo.All(fetch)` and never sees a cursor; the iterator fetches each page, yields its items, follows the next cursor, and surfaces any fetch failure as the error element of the pair.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
repo.go              Item, Page, Fetcher, All (iter.Seq2[Item, error]), PagedSource
cmd/
  demo/
    main.go          range every item across three pages with a single loop
repo_test.go         multi-page traversal yields every item; a mid-stream fetch error is surfaced
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `All(fetch Fetcher) iter.Seq2[Item, error]` that hides cursor pagination, plus `PagedSource(items []Item, pageSize int) Fetcher` for the demo and tests.
- Test: `repo_test.go` collects every item across multiple pages through one range loop, and asserts that a fetch failing on a later page surfaces the wrapped error after the earlier pages' items.
- Verify: `go test -run TestRepo -race ./...`

Set up the module:

```bash
mkdir -p repo/cmd/demo && cd repo
go mod init example.com/repo
```

### One iterator that hides the cursor loop

A paginated backend hands out data in chunks: each request returns a page of items plus a cursor for the next page, and an empty cursor means the end. Done naively, that loop leaks into every consumer — each call site has to initialize a cursor, call the fetcher, append the page, check for a next cursor, and repeat, and each one reinvents the same off-by-one and termination bugs. The fix is to express the whole traversal once, as an iterator, so the consumer's mental model collapses to "range over all items." `All` returns `iter.Seq2[Item, error]`: the cursor never appears in the signature, the page boundary never appears at the call site, and the consumer writes the same loop it would write over an in-memory slice — plus the one `if err != nil` that any fallible sequence requires.

The error half of the pair is what makes this honest rather than merely convenient. A fetch can fail on any page, including the third of five, and the iterator cannot return that error — it already returned the iterator. So it follows the fallible-iterator contract from Exercise 3: on a fetch failure it yields `(Item{}, err)` exactly once and then `return`s, making the error terminal. The error is wrapped with `%w` against the underlying failure, so the consumer can classify it with `errors.Is` instead of matching strings, and the wrap records which cursor failed, `fmt.Errorf("fetch page %q: %w", cursor, err)`, which is the one piece of pagination state worth surfacing for diagnosis. Crucially, the items from the pages that *did* load are yielded before the error arrives, so a consumer that streams into a sink keeps the partial, valid prefix and learns precisely where the stream broke.

The traversal itself is a plain loop the consumer never sees. It starts with the empty cursor, fetches a page, yields each item while checking `yield`'s boolean so a caller's `break` stops the walk mid-page, and then either follows `page.NextCursor` or returns when that cursor is empty. `PagedSource` is the test and demo backend: it slices a backing list into pages of `pageSize` and encodes the next offset as the cursor string, modeling a real cursor without a network. In production the `Fetcher` would be an HTTP or database call; the iterator's code does not change, because `Fetcher` is the single seam the pagination strategy hides behind.

Create `repo.go`:

```go
package repo

import (
	"fmt"
	"iter"
	"strconv"
)

// Item is a single record returned by the repository.
type Item struct {
	ID   int
	Name string
}

// Page is one page of results plus the cursor for the next page. An empty
// NextCursor means there are no more pages.
type Page struct {
	Items      []Item
	NextCursor string
}

// Fetcher retrieves one page given a cursor. The empty string requests the
// first page. It is the single seam behind which the pagination strategy hides.
type Fetcher func(cursor string) (Page, error)

// All returns an iterator over every item across all pages, hiding cursor
// pagination from the caller. It yields (item, nil) for each item in order and,
// if a fetch fails, yields (Item{}, err) exactly once and stops. Items from
// pages that loaded successfully are yielded before any error.
func All(fetch Fetcher) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) {
		cursor := ""
		for {
			page, err := fetch(cursor)
			if err != nil {
				yield(Item{}, fmt.Errorf("fetch page %q: %w", cursor, err))
				return
			}
			for _, it := range page.Items {
				if !yield(it, nil) {
					return
				}
			}
			if page.NextCursor == "" {
				return
			}
			cursor = page.NextCursor
		}
	}
}

// PagedSource builds a Fetcher that serves items in pages of pageSize, encoding
// the next offset as the cursor. It models a cursor-paginated backend in memory.
func PagedSource(items []Item, pageSize int) Fetcher {
	return func(cursor string) (Page, error) {
		start := 0
		if cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil {
				return Page{}, fmt.Errorf("invalid cursor %q: %w", cursor, err)
			}
			start = n
		}
		end := start + pageSize
		if end > len(items) {
			end = len(items)
		}
		next := ""
		if end < len(items) {
			next = strconv.Itoa(end)
		}
		return Page{Items: items[start:end], NextCursor: next}, nil
	}
}
```

### The runnable demo

The demo builds five items served two per page, so the traversal spans three pages, and walks them all with a single range loop that never mentions a cursor. The output proves every item arrived in order across the page boundaries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

func main() {
	items := []repo.Item{
		{ID: 1, Name: "ada"},
		{ID: 2, Name: "babbage"},
		{ID: 3, Name: "lovelace"},
		{ID: 4, Name: "turing"},
		{ID: 5, Name: "hopper"},
	}
	fetch := repo.PagedSource(items, 2) // 3 pages: 2, 2, 1

	fmt.Println("All items (paging hidden):")
	for item, err := range repo.All(fetch) {
		if err != nil {
			fmt.Println("  error:", err)
			break
		}
		fmt.Printf("  %d: %s\n", item.ID, item.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
All items (paging hidden):
  1: ada
  2: babbage
  3: lovelace
  4: turing
  5: hopper
```

### Tests

The tests cover the two behaviors that define the contract. `TestRepoMultiPage` ranges a multi-page source through one loop and asserts every item is collected in order, proving the page boundaries are invisible. `TestRepoMidStreamError` wraps a fetcher so it fails on the second page, then asserts the first page's items arrive, the error is surfaced exactly once via the error element, and it is classifiable with `errors.Is` through the `%w` wrap.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"testing"
)

func sample(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i + 1, Name: "item-" + string(rune('a'+i))}
	}
	return items
}

func TestRepoMultiPage(t *testing.T) {
	t.Parallel()

	items := sample(5)
	fetch := PagedSource(items, 2) // pages of 2, 2, 1

	var gotIDs []int
	for item, err := range All(fetch) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		gotIDs = append(gotIDs, item.ID)
	}
	if len(gotIDs) != 5 {
		t.Fatalf("collected %d items, want 5", len(gotIDs))
	}
	for i, id := range gotIDs {
		if id != i+1 {
			t.Fatalf("item %d has ID %d, want %d (order broken across pages)", i, id, i+1)
		}
	}
}

var errBackend = errors.New("backend unavailable")

func TestRepoMidStreamError(t *testing.T) {
	t.Parallel()

	base := PagedSource(sample(4), 2) // page 0 then page with cursor "2"
	failing := func(cursor string) (Page, error) {
		if cursor == "2" {
			return Page{}, errBackend
		}
		return base(cursor)
	}

	var gotIDs []int
	var gotErr error
	errCount := 0
	for item, err := range All(failing) {
		if err != nil {
			gotErr = err
			errCount++
			break
		}
		gotIDs = append(gotIDs, item.ID)
	}

	if len(gotIDs) != 2 {
		t.Fatalf("got %d items before error, want 2 (first page)", len(gotIDs))
	}
	if errCount != 1 {
		t.Fatalf("error surfaced %d times, want exactly 1", errCount)
	}
	if !errors.Is(gotErr, errBackend) {
		t.Fatalf("error = %v, want errors.Is(_, errBackend)", gotErr)
	}
}
```

## Review

The repository is well-designed when the consumer cannot tell it is paginated except by the one error check every fallible sequence needs. Confirm `All`'s signature is `iter.Seq2[Item, error]` with no cursor anywhere in the public surface, and that the traversal starts from the empty cursor, yields each item while honoring `yield`'s boolean, and terminates on an empty `NextCursor`. Confirm the failure path follows the fallible-iterator contract: a fetch error yields `(Item{}, err)` once, wrapped with `%w` so `errors.Is` works, and then returns — the mid-stream test asserts the first page's two items arrive before the single error, which is what proves the partial prefix is preserved and the error is terminal.

The common mistakes are leaking the cursor, mishandling the failure, and breaking termination. Exposing the cursor — returning `Page` from the public method, or taking a cursor parameter — pushes the pagination loop back onto every caller, defeating the point. Returning the fetch error from the iterator constructor is impossible (it already returned the iterator), and logging it while ending the sequence quietly makes a backend outage look like an empty result set, the same silent-truncation bug fallible iterators exist to prevent. Wrapping with `%v` instead of `%w` severs the chain `errors.Is` walks. Forgetting to stop on an empty `NextCursor`, or advancing the cursor before checking it, turns the traversal into an infinite loop or skips the last page.

## Resources

- [`iter` package: Pull and Seq2](https://pkg.go.dev/iter) — the `iter.Seq2[V, error]` shape this repository exposes and the yield contract it follows.
- [`errors.Is` and `fmt.Errorf` `%w`](https://pkg.go.dev/errors#Is) — wrapping the backend failure so a consumer classifies a streamed error without string matching.
- [AIP-158: Pagination](https://google.aip.dev/158) — the API design standard for cursor/page-token pagination this iterator hides behind a single call.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the Go blog post introducing range-over-func and the fallible `Seq2[V, error]` pattern.

---

Back to [04-domain-event-store.md](04-domain-event-store.md) | Next: [../06-composing-iterators/00-concepts.md](../06-composing-iterators/00-concepts.md)
