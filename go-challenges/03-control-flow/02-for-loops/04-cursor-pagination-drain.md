# Exercise 4: Draining a Cursor-Paginated Upstream API

Paging through a cursor-based API — S3 `ListObjectsV2` with its continuation token,
a REST endpoint that returns a `next` cursor, a database keyset scan — is the
canonical use of an infinite `for {}`. There is no counter and no simple predicate;
the loop runs until the upstream hands back an empty cursor, and its correct exits
are exactly that terminal cursor, a cancelled context, and a safety cap that bounds
a misbehaving upstream. This module builds `FetchAll` and proves all three exits.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
pagination/                  module example.com/pagination
  go.mod
  pagination.go              FetchAll[T](ctx, fetch, maxPages); ErrTooManyPages
  pagination_test.go         scripted fetcher: full drain, page cap trips, ctx cancel between pages
  cmd/demo/
    main.go                  drains a scripted 3-page source and prints the items
```

- Files: `pagination.go`, `pagination_test.go`, `cmd/demo/main.go`.
- Implement: `FetchAll[T any](ctx, fetch func(ctx, cursor string) ([]T, string, error), maxPages int) ([]T, error)` with an infinite `for {}` that breaks on an empty cursor, caps pages with `ErrTooManyPages`, exits on `ctx.Done()`, and assembles pages with `slices.Concat`.
- Test: a scripted fetcher returns pages then an empty cursor (assert full ordered drain and termination); a fetcher that never clears the cursor (assert `ErrTooManyPages`); a fetcher that fails; a context cancelled between pages (assert `ctx.Err()` with the partial results collected so far).
- Verify: `go test -count=1 -race ./...`

### Why an infinite loop needs three exits, not one

A cursor drain has a natural terminal condition: the upstream returns an empty next
cursor, meaning "no more pages." That is the *structural* exit — `if next == ""
{ break }`. If the upstream always behaved, that would be enough. It does not
always behave, so the loop needs two more exits.

The first is a *safety cap*. A buggy or malicious upstream that keeps returning a
non-empty cursor forever would turn the drain into an infinite hot loop that grows
memory without bound. So the loop counts pages and returns `ErrTooManyPages` once
it exceeds `maxPages`. This is the pagination version of the universal rule: any
loop driven by an external system needs an explicit upper bound.

The second is *cancellation*. Between pages the loop checks `ctx.Err()` and returns
it if the caller has cancelled or the deadline has passed. Crucially, it returns
the items collected *so far* alongside the error, so a caller who cancels a long
drain can still use the partial result if it wants to. The order matters: check
`ctx` at the top of each iteration, before the next network call, so a cancelled
context stops the drain immediately rather than issuing one more request.

Pages are accumulated as a slice of slices and flattened once at the end with
`slices.Concat`, which allocates the final backing array a single time. Appending
each page into a growing slice would also work; collecting then concatenating keeps
the per-iteration body free of amortized-growth reasoning and makes the "assemble
in order" step explicit.

Create `pagination.go`:

```go
package pagination

import (
	"context"
	"errors"
	"slices"
)

// ErrTooManyPages means the upstream exceeded the page cap without ending, which
// almost always signals a cursor that never clears.
var ErrTooManyPages = errors.New("exceeded max pages")

// Fetcher returns one page of items and the cursor for the next page. An empty
// next cursor means there are no more pages. The first call receives cursor "".
type Fetcher[T any] func(ctx context.Context, cursor string) (items []T, next string, err error)

// FetchAll drains every page from a cursor-paginated source in order. It stops
// on an empty cursor (success), returns ctx.Err() if the context is cancelled
// between pages, returns fetch's error if a page fails, and returns
// ErrTooManyPages if the source produces more than maxPages pages. On any error
// it returns the items collected before the error.
func FetchAll[T any](ctx context.Context, fetch Fetcher[T], maxPages int) ([]T, error) {
	var pages [][]T
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return slices.Concat(pages...), err
		}
		if len(pages) >= maxPages {
			return slices.Concat(pages...), ErrTooManyPages
		}
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return slices.Concat(pages...), err
		}
		pages = append(pages, items)
		if next == "" {
			return slices.Concat(pages...), nil
		}
		cursor = next
	}
}
```

### The runnable demo

The demo scripts a three-page source (cursors `p1`, `p2`, then empty) and drains it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/pagination"
)

func main() {
	pages := map[string]struct {
		items []int
		next  string
	}{
		"":   {items: []int{1, 2}, next: "p1"},
		"p1": {items: []int{3, 4}, next: "p2"},
		"p2": {items: []int{5}, next: ""},
	}

	fetch := func(_ context.Context, cursor string) ([]int, string, error) {
		p := pages[cursor]
		return p.items, p.next, nil
	}

	all, err := pagination.FetchAll(context.Background(), fetch, 100)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Printf("drained %d items: %v\n", len(all), all)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
drained 5 items: [1 2 3 4 5]
```

### Tests

The fetcher is a fake driven by scripted pages, so every exit is exercised
deterministically. `TestFetchAllDrains` asserts the full ordered result and that
the loop terminates. `TestFetchAllCapTrips` uses a fetcher whose cursor never
clears and asserts `ErrTooManyPages`. `TestFetchAllCancelledBetweenPages` cancels
the context after the first page and asserts the loop returns `ctx.Err()` with the
partial result.

Create `pagination_test.go`:

```go
package pagination

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestFetchAllDrains(t *testing.T) {
	t.Parallel()

	script := map[string]struct {
		items []int
		next  string
	}{
		"":  {[]int{1, 2}, "a"},
		"a": {[]int{3}, "b"},
		"b": {[]int{4, 5}, ""},
	}
	calls := 0
	fetch := func(_ context.Context, cursor string) ([]int, string, error) {
		calls++
		p := script[cursor]
		return p.items, p.next, nil
	}

	got, err := FetchAll(context.Background(), fetch, 100)
	if err != nil {
		t.Fatalf("FetchAll() = %v, want nil", err)
	}
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if calls != 3 {
		t.Fatalf("fetch called %d times, want 3", calls)
	}
}

func TestFetchAllCapTrips(t *testing.T) {
	t.Parallel()

	// A cursor that never clears: every page points to the next.
	fetch := func(_ context.Context, _ string) ([]int, string, error) {
		return []int{0}, "more", nil
	}

	got, err := FetchAll(context.Background(), fetch, 5)
	if !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("err = %v, want ErrTooManyPages", err)
	}
	if len(got) != 5 {
		t.Fatalf("collected %d items before cap, want 5", len(got))
	}
}

func TestFetchAllPropagatesFetchError(t *testing.T) {
	t.Parallel()

	boom := errors.New("upstream 503")
	fetch := func(_ context.Context, cursor string) ([]int, string, error) {
		if cursor == "" {
			return []int{1}, "next", nil
		}
		return nil, "", boom
	}

	got, err := FetchAll(context.Background(), fetch, 100)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if want := []int{1}; !slices.Equal(got, want) {
		t.Fatalf("partial items = %v, want %v", got, want)
	}
}

func TestFetchAllCancelledBetweenPages(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	fetch := func(_ context.Context, cursor string) ([]int, string, error) {
		if cursor == "" {
			return []int{1, 2}, "next", nil
		}
		t.Fatal("fetch called after cancellation")
		return nil, "", nil
	}

	// Wrap fetch to cancel right after the first page returns.
	calls := 0
	wrapped := func(c context.Context, cursor string) ([]int, string, error) {
		items, next, err := fetch(c, cursor)
		calls++
		if calls == 1 {
			cancel()
		}
		return items, next, err
	}

	got, err := FetchAll(ctx, wrapped, 100)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if want := []int{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("partial items = %v, want %v", got, want)
	}
}

func ExampleFetchAll() {
	script := map[string]struct {
		items []string
		next  string
	}{
		"":  {[]string{"a"}, "x"},
		"x": {[]string{"b", "c"}, ""},
	}
	fetch := func(_ context.Context, cursor string) ([]string, string, error) {
		p := script[cursor]
		return p.items, p.next, nil
	}
	all, _ := FetchAll(context.Background(), fetch, 10)
	fmt.Println(all)
	// Output: [a b c]
}
```

## Review

The drain is correct when its infinite loop has all three exits wired: the
structural exit on an empty cursor (`next == ""`), the safety cap returning
`ErrTooManyPages`, and the cancellation check at the *top* of the loop returning
`ctx.Err()` before issuing another request. Each error path returns the partial
result assembled with `slices.Concat`, so a caller can distinguish "no data" from
"some data then a failure." The trap this guards against is the one-exit drain —
`for { fetch; if next == "" break }` with no cap and no context check — which
against a stuck upstream becomes an infinite hot loop or an unbounded memory grow.
`TestFetchAllCapTrips` and `TestFetchAllCancelledBetweenPages` prove the other two
exits fire. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the infinite `for {}` and `break`.
- [slices.Concat](https://pkg.go.dev/slices#Concat) — flattening the collected pages once.
- [context package](https://pkg.go.dev/context) — `Context.Err` and `Context.Done` for the cancellation exit.
- [AWS ListObjectsV2 pagination](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html) — a real cursor/continuation-token API this pattern drains.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-batch-chunk-writer.md](03-batch-chunk-writer.md) | Next: [05-bounded-worker-pool.md](05-bounded-worker-pool.md)
