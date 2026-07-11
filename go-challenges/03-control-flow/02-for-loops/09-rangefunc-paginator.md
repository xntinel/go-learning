# Exercise 9: Range-Over-Func Iterator for Lazy Pagination (Go 1.23)

Go 1.23's range-over-func lets a package expose its own iteration behind a plain
`for v := range seq`. This is the modern way a data-access layer hides pagination:
the caller writes `for item := range p.All(ctx)` and each page is fetched *lazily*,
only as the caller pulls, honoring an early `break` by not fetching the next page.
This module builds a `Paginator` returning an `iter.Seq[Item]`, with errors
surfaced through a companion `Err()` method.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
rangefunc/                   module example.com/rangefunc
  go.mod
  paginator.go               Paginator.All() iter.Seq[Item]; Err(); NewPaginator
  paginator_test.go          full drain, early break stops fetching, fetch error via Err(), empty source
  cmd/demo/
    main.go                  ranges over a lazy 3-page source and prints items
```

- Files: `paginator.go`, `paginator_test.go`, `cmd/demo/main.go`.
- Implement: `NewPaginator(ctx, fetch)` and `(*Paginator).All() iter.Seq[Item]` whose body is `func(yield func(Item) bool)`, fetching each page lazily, stopping when `yield` returns false, and recording any fetch error in a field exposed by `(*Paginator).Err()`.
- Test: full consumption yields all items in order; an early `break` stops after the expected number of fetches (no extra page); a fetch error halts iteration and is exposed via `Err()`; an empty source yields zero iterations.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rangefunc/cmd/demo
cd ~/go-exercises/rangefunc
go mod init example.com/rangefunc
```

### How `iter.Seq` inverts control, and the two obligations it creates

`iter.Seq[V]` is defined as `func(yield func(V) bool)`. When you write `for item :=
range p.All(ctx)`, the compiler rewrites the loop body into a `yield` function and
calls your sequence with it. Your producer calls `yield(item)` once per element;
`yield` returns `true` to mean "keep going" and `false` to mean "the consumer used
`break` or `return` — stop." Control is inverted: the *producer* drives, calling
back into the consumer's body through `yield`.

That inversion creates two obligations, and getting either wrong is the classic
range-func bug. First, **the producer must check `yield`'s return value**: `if
!yield(item) { return }`. If it ignores the `false` and keeps looping — fetching
the next page after the consumer already broke out — it does exactly the
over-fetching the lazy abstraction was meant to prevent, and worse, calling `yield`
again after it returned `false` panics. Second, **fetching is lazy**: the next page
is fetched only when the current page's items are exhausted and the consumer is
still pulling, so a consumer that breaks after the first few items never triggers
the second page's network call at all.

Errors need a home. An `iter.Seq` yields values, not errors, so a fetch failure
cannot be returned from the loop. The idiomatic pattern is a stateful iterator: the
`Paginator` records the error in a field and stops yielding, and the caller checks
`p.Err()` after the loop. This mirrors `bufio.Scanner` (`for scanner.Scan()` then
`scanner.Err()`) and `database/sql.Rows`. The loop ends either because the data ran
out (`Err()` is nil), the consumer broke (`Err()` is nil — a break is not an error),
or a fetch failed (`Err()` is non-nil).

Create `paginator.go`:

```go
package rangefunc

import (
	"context"
	"iter"
)

// Item is one element of the paginated source.
type Item struct {
	ID   int
	Name string
}

// Page is one page returned by the fetch function: its items and the cursor for
// the next page. An empty Next means there are no more pages.
type Page struct {
	Items []Item
	Next  string
}

// FetchFunc returns one page for a cursor. The first call receives "".
type FetchFunc func(ctx context.Context, cursor string) (Page, error)

// Paginator exposes a cursor-paginated source as an iter.Seq[Item], fetching
// pages lazily. Any fetch error is captured and reported by Err.
type Paginator struct {
	ctx   context.Context
	fetch FetchFunc
	err   error
}

// NewPaginator builds a Paginator over fetch.
func NewPaginator(ctx context.Context, fetch FetchFunc) *Paginator {
	return &Paginator{ctx: ctx, fetch: fetch}
}

// All returns a sequence over every item across all pages. Pages are fetched
// lazily as the consumer pulls; a consumer break stops fetching. After ranging,
// call Err to learn whether iteration stopped on a fetch error.
func (p *Paginator) All() iter.Seq[Item] {
	return func(yield func(Item) bool) {
		p.err = nil
		cursor := ""
		for {
			if err := p.ctx.Err(); err != nil {
				p.err = err
				return
			}
			page, err := p.fetch(p.ctx, cursor)
			if err != nil {
				p.err = err
				return
			}
			for _, item := range page.Items {
				if !yield(item) {
					return // consumer broke out: stop, do not fetch more
				}
			}
			if page.Next == "" {
				return // no more pages
			}
			cursor = page.Next
		}
	}
}

// Err reports the error that stopped iteration, or nil if it finished normally
// or the consumer broke out.
func (p *Paginator) Err() error {
	return p.err
}
```

### The runnable demo

The demo ranges over a lazy three-page source with a plain `for range`, printing
each item's name.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/rangefunc"
)

func main() {
	script := map[string]rangefunc.Page{
		"":   {Items: []rangefunc.Item{{ID: 1, Name: "ann"}, {ID: 2, Name: "bob"}}, Next: "p1"},
		"p1": {Items: []rangefunc.Item{{ID: 3, Name: "cat"}}, Next: "p2"},
		"p2": {Items: []rangefunc.Item{{ID: 4, Name: "dan"}}, Next: ""},
	}
	fetch := func(_ context.Context, cursor string) (rangefunc.Page, error) {
		return script[cursor], nil
	}

	p := rangefunc.NewPaginator(context.Background(), fetch)
	for item := range p.All() {
		fmt.Printf("%d:%s\n", item.ID, item.Name)
	}
	if err := p.Err(); err != nil {
		fmt.Printf("error: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
1:ann
2:bob
3:cat
4:dan
```

### Tests

`TestFullConsumption` collects every item and asserts order. `TestEarlyBreakStopsFetching`
is the key test: it breaks after two items and asserts the fetcher was called only
once — proving the second page is never fetched, which is the whole promise of lazy
iteration. `TestFetchErrorHaltsAndSurfaces` injects a failing fetch and asserts both
that iteration stopped and that `Err()` returns the failure.

Create `paginator_test.go`:

```go
package rangefunc

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func scriptedFetch(script map[string]Page, calls *int) FetchFunc {
	return func(_ context.Context, cursor string) (Page, error) {
		*calls++
		return script[cursor], nil
	}
}

func TestFullConsumption(t *testing.T) {
	t.Parallel()

	script := map[string]Page{
		"":  {Items: []Item{{1, "a"}, {2, "b"}}, Next: "x"},
		"x": {Items: []Item{{3, "c"}}, Next: ""},
	}
	calls := 0
	p := NewPaginator(context.Background(), scriptedFetch(script, &calls))

	var got []int
	for item := range p.All() {
		got = append(got, item.ID)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if want := []int{1, 2, 3}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if calls != 2 {
		t.Fatalf("fetch called %d times, want 2", calls)
	}
}

func TestEarlyBreakStopsFetching(t *testing.T) {
	t.Parallel()

	script := map[string]Page{
		"":  {Items: []Item{{1, "a"}, {2, "b"}}, Next: "x"},
		"x": {Items: []Item{{3, "c"}}, Next: ""},
	}
	calls := 0
	p := NewPaginator(context.Background(), scriptedFetch(script, &calls))

	var got []int
	for item := range p.All() {
		got = append(got, item.ID)
		if len(got) == 2 {
			break
		}
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times after early break, want 1 (no second page)", calls)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil (a break is not an error)", err)
	}
}

func TestFetchErrorHaltsAndSurfaces(t *testing.T) {
	t.Parallel()

	boom := errors.New("upstream 500")
	fetch := func(_ context.Context, cursor string) (Page, error) {
		if cursor == "" {
			return Page{Items: []Item{{1, "a"}}, Next: "x"}, nil
		}
		return Page{}, boom
	}
	p := NewPaginator(context.Background(), fetch)

	var got []int
	for item := range p.All() {
		got = append(got, item.ID)
	}
	if !errors.Is(p.Err(), boom) {
		t.Fatalf("Err() = %v, want boom", p.Err())
	}
	if want := []int{1}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("items before error = %v, want %v", got, want)
	}
}

func TestEmptySource(t *testing.T) {
	t.Parallel()

	fetch := func(context.Context, string) (Page, error) {
		return Page{Items: nil, Next: ""}, nil
	}
	p := NewPaginator(context.Background(), fetch)

	count := 0
	for range p.All() {
		count++
	}
	if count != 0 {
		t.Fatalf("iterations = %d, want 0", count)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

func ExamplePaginator_All() {
	script := map[string]Page{
		"": {Items: []Item{{1, "x"}, {2, "y"}}, Next: ""},
	}
	fetch := func(_ context.Context, cursor string) (Page, error) {
		return script[cursor], nil
	}
	p := NewPaginator(context.Background(), fetch)
	for item := range p.All() {
		fmt.Println(item.Name)
	}
	// Output:
	// x
	// y
}
```

## Review

The paginator is correct when the producer honors both `iter.Seq` obligations. It
must check `yield`'s return — `if !yield(item) { return }` — so a consumer `break`
stops iteration and no further page is fetched; ignoring it would over-fetch and,
worse, panic on the next `yield` after a `false`. And fetching must be lazy: the
next page's `fetch` call happens only when the current page is exhausted and the
consumer is still pulling. `TestEarlyBreakStopsFetching` is the direct proof — two
items consumed, one `fetch` call, the second page never touched. Errors live in a
captured field surfaced by `Err()`, mirroring `bufio.Scanner`: after the loop, a
`nil` `Err()` means the data ran out or the consumer broke, and a non-nil `Err()`
means a fetch failed. Run `go test -count=1 -race ./...`.

## Resources

- [Go Blog: Range over function types](https://go.dev/blog/range-functions) — the mechanics of `iter.Seq` and `yield`.
- [iter package](https://pkg.go.dev/iter) — `Seq` and `Seq2` definitions.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the range-over-func rewrite and how `break` maps to `yield` returning false.
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — the `Scan()`/`Err()` pattern this iterator's error handling mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-readiness-poll.md](08-readiness-poll.md) | Next: [10-stream-filter-continue.md](10-stream-filter-continue.md)
