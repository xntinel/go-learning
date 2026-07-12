# Exercise 11: A Page-Count Safety Cap on a Paginated Drain

**Nivel: Intermedio** — validacion rapida (un test corto).

A classic backend loop: call a paginated upstream, follow its cursor, collect
every item, stop when the cursor comes back empty. That structural exit is not
enough on its own — a bug in the upstream (or a malicious response) can hand
back the same cursor forever, and a `for` loop with only that exit condition
spins until the process is killed. This module builds `DrainAll` with a hard
page-count cap as the second, defensive exit.

This module is fully self-contained: its own `go mod init` and one test file.

## What you'll build

```text
drain/                       module example.com/paginated-drain
  go.mod                     go 1.24
  drain.go                   ErrTooManyPages; DrainAll(fetch, maxPages) ([]string, error)
  drain_test.go              full drain, cursor-loop cap, wrapped fetch error
```

- Files: `drain.go`, `drain_test.go`.
- Implement: `DrainAll(fetch func(cursor string) (items []string, next string, err error), maxPages int) ([]string, error)` — a `for` loop with a page counter that calls `fetch`, appends its items, and follows `next` until it is empty; exceeding `maxPages` returns the sentinel `ErrTooManyPages`; a `fetch` error is wrapped with `fmt.Errorf` and the page number.
- Test: a three-page fake `fetch` asserts the full ordered item list; a `fetch` that always returns the same cursor asserts `ErrTooManyPages` via `errors.Is`; a `fetch` that fails on page 2 asserts the wrapped error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/11-paginated-drain-safety-cap
cd go-solutions/03-control-flow/02-for-loops/11-paginated-drain-safety-cap
go mod edit -go=1.24
```

### Why the cursor's own "done" signal is not a safe exit

`next == ""` is the loop's structural exit, and on a well-behaved upstream it
is the only one that ever fires. But "well-behaved" is an assumption about
someone else's code, and pagination cursors are exactly the kind of contract
that breaks quietly: an off-by-one in the upstream's cursor logic, a caching
bug that always serves the first page's `next` value, or a hostile response
in a test double, and the caller is now in an infinite loop with no upstream
error to catch. The loop cannot tell "still more real pages" apart from
"stuck" using only the data the upstream sends it.

A page counter checked against `maxPages` is a cap the caller controls,
independent of anything the upstream claims. It turns an unbounded loop into
one with a provable worst case: at most `maxPages` calls to `fetch`, after
which the loop gives up loudly with `ErrTooManyPages` instead of hanging
silently. The cap is deliberately checked at the *top* of each iteration,
before calling `fetch`, so `maxPages` reads naturally as "the maximum number
of pages this call will fetch."

The other failure mode is a `fetch` that returns an error outright — a
timeout, a 500, a malformed page. That is wrapped with `fmt.Errorf` and the
page number it happened on, so a caller debugging a partial drain knows
exactly where it broke instead of just "fetch failed."

Create `drain.go`:

```go
package drain

import (
	"errors"
	"fmt"
)

// ErrTooManyPages is returned when the page count exceeds maxPages before the
// upstream ever hands back an empty cursor. It protects a caller from a cursor
// loop bug in the upstream API turning a bounded drain into an unbounded one.
var ErrTooManyPages = errors.New("drain: too many pages")

// DrainAll follows fetch's cursor until it returns an empty next cursor,
// appending every page's items in order. maxPages bounds the number of calls
// to fetch; exceeding it returns ErrTooManyPages instead of looping forever.
// A fetch error is wrapped with the page number it failed on.
func DrainAll(fetch func(cursor string) (items []string, next string, err error), maxPages int) ([]string, error) {
	var all []string
	cursor := ""

	for page := 1; ; page++ {
		if page > maxPages {
			return all, ErrTooManyPages
		}

		items, next, err := fetch(cursor)
		if err != nil {
			return all, fmt.Errorf("drain: fetch page %d: %w", page, err)
		}

		all = append(all, items...)

		if next == "" {
			return all, nil
		}
		cursor = next
	}
}
```

### Tests

The suite drives `DrainAll` with in-memory closures, so there is no real
network and every case is deterministic. `TestDrainAllFullDrain` scripts a
three-page source via a small cursor-to-page map and asserts the exact,
ordered item list. `TestDrainAllTooManyPages` is the cap in action: a `fetch`
that always returns the same cursor never reaches `next == ""`, so the loop
must stop on the counter and the test asserts `ErrTooManyPages` via
`errors.Is`. `TestDrainAllFetchError` fails on the second call and asserts the
returned error both `errors.Is`-matches the underlying cause and carries the
page number in its message.

Create `drain_test.go`:

```go
package drain

import (
	"errors"
	"testing"
)

func TestDrainAllFullDrain(t *testing.T) {
	t.Parallel()

	pages := map[string][]string{
		"":   {"a", "b"},
		"p2": {"c"},
		"p3": {"d", "e"},
	}
	next := map[string]string{"": "p2", "p2": "p3", "p3": ""}

	fetch := func(cursor string) ([]string, string, error) {
		return pages[cursor], next[cursor], nil
	}

	items, err := DrainAll(fetch, 10)
	if err != nil {
		t.Fatalf("DrainAll() error = %v, want nil", err)
	}

	want := []string{"a", "b", "c", "d", "e"}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %v", len(items), len(want), items)
	}
	for i, w := range want {
		if items[i] != w {
			t.Errorf("item[%d] = %q, want %q", i, items[i], w)
		}
	}
}

func TestDrainAllTooManyPages(t *testing.T) {
	t.Parallel()

	// A cursor that never clears simulates the upstream bug this cap defends
	// against: every call returns the same "next" cursor, so a naive drain
	// loops forever.
	fetch := func(cursor string) ([]string, string, error) {
		return []string{"x"}, "same", nil
	}

	_, err := DrainAll(fetch, 5)
	if !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("err = %v, want ErrTooManyPages", err)
	}
}

func TestDrainAllFetchError(t *testing.T) {
	t.Parallel()

	upstreamErr := errors.New("upstream 500")
	call := 0
	fetch := func(cursor string) ([]string, string, error) {
		call++
		if call == 2 {
			return nil, "", upstreamErr
		}
		return []string{"a"}, "p2", nil
	}

	_, err := DrainAll(fetch, 10)
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("err = %v, want wrapped upstreamErr", err)
	}
	wantMsg := "drain: fetch page 2: upstream 500"
	if err.Error() != wantMsg {
		t.Fatalf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}
}
```

## Review

`DrainAll` is correct when it has exactly two ways to stop cleanly, and one
way to stop loudly. The clean stop is `next == ""`, proven by the full-drain
test. The loud stop is the page cap: checked before every `fetch` call, so a
cursor that never clears is caught deterministically at `maxPages` instead of
running away, and the sentinel `ErrTooManyPages` lets a caller tell "capped
out" apart from "upstream failed." That second failure mode — `fetch`
returning an error — is wrapped with the page number so a caller debugging a
partial drain knows exactly where it broke. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counter-and-condition form used here.
- [errors.New and errors.Is](https://pkg.go.dev/errors) — sentinel errors and how `Is` unwraps `%w` chains.
- [fmt: Printing (Errorf and %w)](https://pkg.go.dev/fmt#Errorf) — wrapping an error with additional context.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-stream-filter-continue.md](10-stream-filter-continue.md) | Next: [12-sliding-window-rate-counter.md](12-sliding-window-rate-counter.md)
