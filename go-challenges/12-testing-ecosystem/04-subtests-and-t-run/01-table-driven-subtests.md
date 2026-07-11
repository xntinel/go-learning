# Exercise 1: Table-Driven Subtests Over a Query Matcher

A log-search endpoint answers "which line contains this substring, and where?"
The core is a `Search(haystack, needle)` matcher returning the byte offset of the
first occurrence. This exercise establishes the baseline pattern the whole lesson
builds on: a table of named cases driven through `t.Run`, plus two standalone
tests that pin edge-case contracts.

This module is fully self-contained: its own `go mod init`, its own matcher, its
own demo, and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
querymatch/                 independent module: example.com/querymatch
  go.mod                    go 1.26
  search.go                 func Search(haystack, needle string) int
  cmd/
    demo/
      main.go               runnable demo: match a needle in a log line
  search_test.go            table-driven subtests + empty-haystack + overlap contract
```

- Files: `search.go`, `cmd/demo/main.go`, `search_test.go`.
- Implement: `Search(haystack, needle string) int` returning the first-occurrence
  byte offset, `0` for an empty needle, and `-1` when absent.
- Test: a table of named cases run with `t.Run`, a separate empty-haystack test,
  and an overlapping-needle first-occurrence contract test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/querymatch/cmd/demo
cd ~/go-exercises/querymatch
go mod init example.com/querymatch
```

### Why a table plus t.Run is the baseline

A bare `for` loop that asserts each case works, but when case four fails you get a
single anonymous failure with no name, and the loop stops at the first `t.Fatalf`
so you never learn whether cases five through ten also broke. Wrapping each
iteration in `t.Run(tc.name, …)` fixes both: every case reports under its own name
(`TestSearch/found_in_middle`), and a failing case is isolated to its own subtest
so its siblings still run and still report. The name is also the handle `-run`
uses, so you can re-run exactly the one case that broke.

`Search` itself is a thin, honest wrapper over `strings.Index`, which is the
stdlib's Boyer-Moore-ish substring search returning the byte offset of the first
occurrence or `-1`. The one contract worth stating explicitly is the empty-needle
case: `strings.Index(s, "")` returns `0` (the empty string occurs at position
zero of anything), and we preserve that so an empty query matches at the start
rather than being treated as "not found". The overlapping-needle contract —
`Search("aaaa", "aa")` is `0`, the first occurrence, not `1` or `2` — is exactly
`strings.Index`'s first-match semantics, and the dedicated test pins it so a later
"optimization" cannot silently start returning a different match.

Create `search.go`:

```go
package search

import "strings"

// Search returns the byte offset of the first occurrence of needle in haystack,
// 0 for an empty needle, or -1 when needle is absent. It is the matcher behind a
// log-search endpoint: given a stored line and a query fragment, where does the
// fragment first appear?
func Search(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	return strings.Index(haystack, needle)
}
```

### The runnable demo

The demo plays the endpoint's role: it takes a stored log line and a query, and
prints where the query matched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/querymatch"
)

func main() {
	const line = "2026-07-02T10:15:03Z level=error msg=timeout upstream=db"

	for _, q := range []string{"error", "upstream=db", "cache-miss"} {
		if at := search.Search(line, q); at >= 0 {
			fmt.Printf("query %-12q matched at offset %d\n", q, at)
		} else {
			fmt.Printf("query %-12q no match\n", q)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
query "error"       matched at offset 27
query "upstream=db" matched at offset 45
query "cache-miss"  no match
```

### Tests

`TestSearch` is the table-driven core: a slice of named cases, each run under
`t.Run(tc.name, …)` with `t.Parallel()` so independent cases run concurrently
under `-race`. `TestSearchEmptyHaystack` is a standalone single-case test for the
"needle in an empty string is `-1`" edge. `TestSearchOverlappingNeedle` pins the
first-occurrence contract on overlapping matches. The `Example` documents the
common case and is auto-verified by its `// Output:` line.

Create `search_test.go`:

```go
package search

import (
	"fmt"
	"testing"
)

func TestSearch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		haystack string
		needle   string
		want     int
	}{
		{"found_at_start", "hello world", "hello", 0},
		{"found_in_middle", "hello world", "lo wo", 3},
		{"found_at_end", "hello world", "world", 6},
		{"not_found", "hello world", "xyz", -1},
		{"empty_needle", "hello", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Search(tc.haystack, tc.needle); got != tc.want {
				t.Fatalf("Search(%q, %q) = %d, want %d", tc.haystack, tc.needle, got, tc.want)
			}
		})
	}
}

func TestSearchEmptyHaystack(t *testing.T) {
	t.Parallel()

	if got := Search("", "x"); got != -1 {
		t.Fatalf("Search(%q, %q) = %d, want -1", "", "x", got)
	}
}

func TestSearchOverlappingNeedle(t *testing.T) {
	t.Parallel()

	// "aa" occurs at offsets 0, 1, and 2 in "aaaa"; Search must return the
	// first, 0. This pins the first-occurrence contract against a future
	// optimization that might return a different match.
	if got := Search("aaaa", "aa"); got != 0 {
		t.Fatalf("Search(%q, %q) = %d, want 0 (first occurrence)", "aaaa", "aa", got)
	}
}

func ExampleSearch() {
	fmt.Println(Search("level=error msg=timeout", "error"))
	// Output: 6
}
```

## Review

The matcher is correct when its result is exactly `strings.Index`'s offset for a
non-empty needle and `0` for the empty needle: the table checks start, middle,
end, absent, and empty; `TestSearchEmptyHaystack` checks the absent-in-empty edge;
and `TestSearchOverlappingNeedle` pins first-occurrence. The structural lesson is
the shape, not the algorithm: each case runs as a named subtest, so a failure
reports as `TestSearch/found_in_middle` and its siblings keep running. Give every
case a unique, space-free name — that name is what the next exercise filters on
with `-run`. Run `go test -race` to confirm the parallel subtests share nothing
mutable.

## Resources

- [testing.T.Run — pkg.go.dev](https://pkg.go.dev/testing#T.Run)
- [strings.Index — pkg.go.dev](https://pkg.go.dev/strings#Index)
- [Using Subtests and Sub-benchmarks — Go Blog](https://go.dev/blog/subtests)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-run-filter-and-skip.md](02-run-filter-and-skip.md)
