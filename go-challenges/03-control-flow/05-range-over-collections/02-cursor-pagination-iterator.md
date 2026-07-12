# Exercise 2: Cursor Pagination as a range-over-func Iterator

Cursor-based list endpoints ("give me a page and a token for the next page") are
everywhere in backend work, and callers hate writing the paging loop by hand. This
module wraps a cursor upstream behind an `iter.Seq2[Item, error]` so callers write
`for item, err := range client.All(ctx)` and stop early with `break` — and the
iterator honors that break by returning from `yield`, stopping the fetch.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cursorpager/                independent module: example.com/cursorpager
  go.mod                    go 1.24
  cursorpager.go            Item, Client, NewClient, All(ctx) iter.Seq2[Item, error]
  cmd/
    demo/
      main.go               runnable demo: page through a fake upstream, print items
  cursorpager_test.go       full traversal, early break, error propagation, context cancel
```

- Files: `cursorpager.go`, `cmd/demo/main.go`, `cursorpager_test.go`.
- Implement: `Client.All(ctx) iter.Seq2[Item, error]` that pages a `fetchPage(cursor) (items, nextCursor, error)` upstream and yields each item with a nil error, or a zero item with the error.
- Test: full traversal in order, early `break` stops fetching further pages, a page error is observed then the loop ends, a cancelled context yields `ctx.Err()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### How a push iterator wraps a cursor loop

The upstream contract is the usual cursor shape: `fetchPage(ctx, cursor)` returns a
page of items, the cursor for the next page (empty string means "no more"), and an
error. `All` returns a function of type `iter.Seq2[Item, error]` — that is, a
`func(yield func(Item, error) bool)`. When the caller writes
`for item, err := range client.All(ctx)`, the compiler synthesizes `yield` from the
loop body and calls the returned function once. Inside, the iterator runs the paging
loop and calls `yield(item, nil)` for each item; the body runs, and `yield` returns
`true` to keep going or `false` if the caller did `break`/`return`.

Three rules make this iterator correct. First, after every `yield` you must check
its return and `return` from the iterator when it is `false` — otherwise a caller's
`break` does not stop the fetching, and you page the entire upstream needlessly (a
real cost and a real leak when the pages are network calls). Second, on a fetch
error you yield `(zeroItem, err)` once and then return; the caller sees the error in
its loop and decides whether to break. Third, honor context cancellation: check
`ctx.Err()` before each fetch and, if the context is done, yield `(zeroItem, ctx.Err())`
and stop. That gives the caller the same "value, err" shape for cancellation as for
any other failure.

The zero cursor drives termination: the loop fetches, yields the page, then advances
`cursor = next`; when `next` is empty the loop ends and the iterator returns, which
is what makes the caller's `range` end after the last page.

Create `cursorpager.go`:

```go
package cursorpager

import (
	"context"
	"iter"
)

// Item is one element of a paginated list.
type Item struct {
	ID string
}

// FetchFunc fetches one page: the items on it and the cursor for the next page
// (empty when there are no more), or an error.
type FetchFunc func(ctx context.Context, cursor string) (items []Item, next string, err error)

// Client pages a cursor-based upstream.
type Client struct {
	fetch FetchFunc
}

func NewClient(fetch FetchFunc) *Client {
	return &Client{fetch: fetch}
}

// All returns a push iterator over every item across all pages. Callers may stop
// early with break; the iterator honors that by returning from yield's false.
func (c *Client) All(ctx context.Context) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) {
		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				yield(Item{}, err)
				return
			}
			items, next, err := c.fetch(ctx, cursor)
			if err != nil {
				yield(Item{}, err)
				return
			}
			for _, it := range items {
				if !yield(it, nil) {
					return // caller did break/return: stop paging
				}
			}
			if next == "" {
				return // no more pages
			}
			cursor = next
		}
	}
}
```

### The runnable demo

The demo wires a fake upstream of three pages behind the client and pages through
it with an ordinary `for ... range`, collecting the IDs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/cursorpager"
)

func main() {
	pages := map[string][]cursorpager.Item{
		"":   {{ID: "a"}, {ID: "b"}},
		"p1": {{ID: "c"}, {ID: "d"}},
		"p2": {{ID: "e"}},
	}
	nextOf := map[string]string{"": "p1", "p1": "p2", "p2": ""}

	client := cursorpager.NewClient(func(_ context.Context, cursor string) ([]cursorpager.Item, string, error) {
		return pages[cursor], nextOf[cursor], nil
	})

	var ids []string
	for it, err := range client.All(context.Background()) {
		if err != nil {
			fmt.Println("error:", err)
			break
		}
		ids = append(ids, it.ID)
	}
	fmt.Println(ids)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
[a b c d e]
```

### Tests

The full-traversal test asserts all items arrive in page order with a nil error.
The early-break test counts fetch calls through a spy and asserts that breaking
after the first item stops before the second page is ever fetched. The
error-propagation test makes page two fail and asserts the loop observes the error
after the page-one items. The context-cancel test passes an already-cancelled
context and asserts the first yielded pair carries `ctx.Err()`.

Create `cursorpager_test.go`:

```go
package cursorpager

import (
	"context"
	"errors"
	"testing"
)

// threePager builds a fetch func over fixed pages and counts calls via *calls.
func threePager(calls *int) FetchFunc {
	pages := map[string][]Item{
		"":   {{ID: "a"}, {ID: "b"}},
		"p1": {{ID: "c"}, {ID: "d"}},
		"p2": {{ID: "e"}},
	}
	next := map[string]string{"": "p1", "p1": "p2", "p2": ""}
	return func(_ context.Context, cursor string) ([]Item, string, error) {
		*calls++
		return pages[cursor], next[cursor], nil
	}
}

func TestAllTraversesEveryPageInOrder(t *testing.T) {
	t.Parallel()
	calls := 0
	c := NewClient(threePager(&calls))

	var got []string
	for it, err := range c.All(context.Background()) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, it.ID)
	}
	want := []string{"a", "b", "c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if calls != 3 {
		t.Fatalf("fetched %d pages, want 3", calls)
	}
}

func TestEarlyBreakStopsFetching(t *testing.T) {
	t.Parallel()
	calls := 0
	c := NewClient(threePager(&calls))

	var got []string
	for it, err := range c.All(context.Background()) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, it.ID)
		break // stop after the first item
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %v, want [a]", got)
	}
	if calls != 1 {
		t.Fatalf("fetched %d pages after break, want 1", calls)
	}
}

var errUpstream = errors.New("upstream failed")

func TestErrorIsYieldedThenLoopEnds(t *testing.T) {
	t.Parallel()
	c := NewClient(func(_ context.Context, cursor string) ([]Item, string, error) {
		switch cursor {
		case "":
			return []Item{{ID: "a"}}, "p1", nil
		default:
			return nil, "", errUpstream
		}
	})

	var got []string
	var sawErr error
	for it, err := range c.All(context.Background()) {
		if err != nil {
			sawErr = err
			break
		}
		got = append(got, it.ID)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("items before error = %v, want [a]", got)
	}
	if !errors.Is(sawErr, errUpstream) {
		t.Fatalf("error = %v, want errUpstream", sawErr)
	}
}

func TestContextCancelYieldsCtxErr(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before iterating

	calls := 0
	c := NewClient(threePager(&calls))

	var sawErr error
	for _, err := range c.All(ctx) {
		sawErr = err
		break
	}
	if !errors.Is(sawErr, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", sawErr)
	}
	if calls != 0 {
		t.Fatalf("fetched %d pages despite cancelled context, want 0", calls)
	}
}
```

## Review

The iterator is correct when `break` in the caller stops the upstream fetching, an
error reaches the loop as the second range value, and a cancelled context short-
circuits before the first fetch. The classic failure is ignoring the boolean
`yield` returns: the loop still `break`s (the language guarantees that), but your
iterator keeps calling `fetch` for pages nobody reads. The early-break test exists
precisely to catch that — it asserts `calls == 1`. Keep the error path yielding a
zero item plus the error rather than panicking, so the caller handles it with the
same `if err != nil` it already writes.

## Resources

- [package iter (Seq, Seq2)](https://pkg.go.dev/iter)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)
- [Go Specification: For statements (range clause)](https://go.dev/ref/spec#For_range)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-metrics-aggregator-range-forms.md](01-metrics-aggregator-range-forms.md) | Next: [03-deterministic-config-diff.md](03-deterministic-config-diff.md)
