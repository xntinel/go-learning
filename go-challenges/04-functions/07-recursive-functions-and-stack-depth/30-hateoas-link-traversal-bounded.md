# Exercise 30: Follow HATEOAS Links with Recursion Depth and Request Limits

**Nivel: Intermedio** — validacion rapida (un test corto).

A HATEOAS API tells the client what to do next by embedding links in each
response — `next`, `prev`, `related` — rather than the client hardcoding
URL patterns. Following those links to discover a whole resource graph is
naturally recursive: fetch a resource, then recurse into every link it
names. The trouble is that the graph is not yours; it is served by
whatever the API returns right now, including whatever bug is currently
live on that API. A pagination bug that makes the last page's `next` point
back at the first page turns a recursive crawl into an infinite loop. A
bug (or a deliberately hostile API) that returns a fresh, never-before-seen
URL on every request turns it into a crawl with no cycle to catch, growing
forever. A single resource with an unreasonable number of links turns it
into a crawl that is technically bounded in depth but unbounded in width.
A safe crawler needs all three defenses at once: skip anything already
visited, cap how deep the link chain can nest, and cap how many requests
the whole crawl is allowed to make.

This module is fully self-contained: its own `go mod init`, the crawler
inline, its own demo and tests.

## What you'll build

```text
hateoas/                      independent module: example.com/hateoas
  go.mod                        go 1.24
  hateoas.go                     type Resource; Fetcher/MapFetcher/FetcherFunc; Crawl (recursive, depth+budget guarded)
  hateoas_test.go                acyclic graph, cycle handled cleanly, chain past maxDepth, fan-out past maxRequests, fetch error, invalid budgets
  cmd/
    demo/
      main.go                     a pagination cycle crawled cleanly, then an endless chain rejected by maxDepth
```

- Files: `hateoas.go`, `cmd/demo/main.go`, `hateoas_test.go`.
- Implement: `Resource{URL string; Data map[string]any; Links map[string]string}`, `Fetcher` interface with `MapFetcher` and `FetcherFunc` implementations, and `Crawl(f Fetcher, startURL string, maxDepth, maxRequests int) (CrawlResult, error)` recursing through an unexported `crawlState.visit`.
- Test: every linked resource in a small acyclic graph; a three-page pagination cycle crawled without revisiting or erroring; an endless unique-URL chain rejected past `maxDepth`; a resource fanning out into more links than `maxRequests` allows; a fetch error propagated with context; invalid depth/request budgets.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hateoas/cmd/demo
cd ~/go-exercises/hateoas
go mod init example.com/hateoas
go mod edit -go=1.24
```

### Three independent guards for three independent failure shapes

`visit` checks, in order: has this exact URL already been visited (skip,
no error — this is what makes cycles harmless); has recursion gone deeper
than `maxDepth` (an unbounded or pathologically long chain); has the crawl
already spent its whole request budget (a wide fan-out, or a chain of
distinct URLs no seen-check can catch). Each guard defends against a
different way a real API can misbehave, and none of the three subsumes
either of the others. A cycle back to an already-visited URL is caught by
the seen-set regardless of how deep it is. A chain of never-before-seen
URLs — the failure mode a buggy API produces by incrementing a query
parameter forever — never triggers the seen-set at all, because every URL
really is new; only the depth counter catches it. A single resource that
links to a thousand others is shallow (depth 2) and has no duplicates, so
neither the seen-set nor the depth guard helps; only the total-request
budget does.

The seen-set's placement matters for correctness, not just efficiency:
`visit` marks a URL as seen immediately after a successful fetch, *before*
recursing into that resource's own links. That ordering is what makes a
cycle back to an ancestor still on the call stack safe — by the time any
descendant's recursive call reaches that ancestor's URL again, the URL is
already marked, and `visit` returns immediately instead of fetching (and
recursing) a second time.

Create `hateoas.go`:

```go
// Package hateoas recursively follows hypermedia links (HATEOAS-style
// "_links" relations) from a starting resource, discovering the rest of a
// linked resource graph one fetch at a time. A crawler like this talks to
// APIs it does not control: a bug on the server side can make a "next"
// link point back at an earlier page (a cycle), or generate a distinct
// URL forever (an unbounded chain), or a single resource can fan out into
// far more links than any real crawl should follow. Crawl guards against
// all three with a seen-set, a maximum recursion depth, and a maximum
// total request budget.
package hateoas

import (
	"errors"
	"fmt"
	"sort"
)

// ErrMaxDepthExceeded is returned when following links would recurse
// deeper than the configured maximum.
var ErrMaxDepthExceeded = errors.New("hateoas: link chain exceeds maximum depth")

// ErrMaxRequestsExceeded is returned when the crawl's total fetch budget
// would be exceeded.
var ErrMaxRequestsExceeded = errors.New("hateoas: crawl exceeds maximum request budget")

// Resource is one hypermedia resource: its own data plus a set of named
// links (rel -> URL) to related resources.
type Resource struct {
	URL   string
	Data  map[string]any
	Links map[string]string
}

// Fetcher fetches one resource by URL. A real implementation would issue
// an HTTP GET and decode a HAL/HATEOAS-style JSON body; tests and the demo
// use an in-memory MapFetcher instead.
type Fetcher interface {
	Fetch(url string) (Resource, error)
}

// MapFetcher is an in-memory Fetcher over a fixed set of resources, keyed
// by URL.
type MapFetcher map[string]Resource

// Fetch looks up url in the map.
func (f MapFetcher) Fetch(url string) (Resource, error) {
	res, ok := f[url]
	if !ok {
		return Resource{}, fmt.Errorf("hateoas: %s: 404 not found", url)
	}
	return res, nil
}

// FetcherFunc adapts a plain function to the Fetcher interface.
type FetcherFunc func(url string) (Resource, error)

// Fetch calls f.
func (f FetcherFunc) Fetch(url string) (Resource, error) { return f(url) }

// CrawlResult is the outcome of a bounded crawl.
type CrawlResult struct {
	Visited  []Resource
	Requests int
}

// Crawl recursively follows every link from every resource reached
// starting at startURL, fetching each distinct URL at most once. It fails
// with ErrMaxDepthExceeded if the link chain nests deeper than maxDepth
// (startURL is depth 1), or with ErrMaxRequestsExceeded if following links
// would issue more than maxRequests fetches.
func Crawl(f Fetcher, startURL string, maxDepth, maxRequests int) (CrawlResult, error) {
	if maxDepth < 1 {
		return CrawlResult{}, fmt.Errorf("hateoas: maxDepth must be >= 1, got %d", maxDepth)
	}
	if maxRequests < 1 {
		return CrawlResult{}, fmt.Errorf("hateoas: maxRequests must be >= 1, got %d", maxRequests)
	}
	s := &crawlState{fetcher: f, maxDepth: maxDepth, maxRequests: maxRequests, seen: make(map[string]bool)}
	if err := s.visit(startURL, 1); err != nil {
		return CrawlResult{}, err
	}
	return CrawlResult{Visited: s.visited, Requests: s.requests}, nil
}

type crawlState struct {
	fetcher     Fetcher
	maxDepth    int
	maxRequests int
	seen        map[string]bool
	visited     []Resource
	requests    int
}

// visit fetches url (unless already seen) and recurses into every link it
// reports, in a deterministic (rel-sorted) order. Marking url as seen
// immediately after a successful fetch, before recursing into its links,
// is what makes a direct or indirect cycle back to url safe: by the time
// any descendant's recursive call reaches url again, it is already marked
// and visit returns immediately instead of fetching, or recursing, again.
func (s *crawlState) visit(url string, depth int) error {
	if s.seen[url] {
		return nil
	}
	if depth > s.maxDepth {
		return fmt.Errorf("%w: %s at depth %d", ErrMaxDepthExceeded, url, depth)
	}
	if s.requests >= s.maxRequests {
		return fmt.Errorf("%w: budget of %d reached before visiting %s", ErrMaxRequestsExceeded, s.maxRequests, url)
	}

	s.requests++
	res, err := s.fetcher.Fetch(url)
	if err != nil {
		return fmt.Errorf("hateoas: %w", err)
	}
	res.URL = url
	s.seen[url] = true
	s.visited = append(s.visited, res)

	rels := make([]string, 0, len(res.Links))
	for rel := range res.Links {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := s.visit(res.Links[rel], depth+1); err != nil {
			return err
		}
	}
	return nil
}
```

### The runnable demo

The demo first crawls a three-page pagination API where a bug makes the
last page's `next` link point back at the first page, showing the crawl
still terminates cleanly with each page visited exactly once. It then
crawls a fabricated API that always returns a brand-new `next` URL,
showing the depth guard reject it once no seen-set could.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hateoas"
)

func main() {
	// A pagination API with a bug: page 3's "next" link points back to
	// page 1 instead of terminating.
	api := hateoas.MapFetcher{
		"/orders/1": {Data: map[string]any{"id": 1}, Links: map[string]string{"next": "/orders/2"}},
		"/orders/2": {Data: map[string]any{"id": 2}, Links: map[string]string{"next": "/orders/3", "prev": "/orders/1"}},
		"/orders/3": {Data: map[string]any{"id": 3}, Links: map[string]string{"next": "/orders/1", "prev": "/orders/2"}},
	}

	result, err := hateoas.Crawl(api, "/orders/1", 10, 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("visited %d resources in %d requests\n", len(result.Visited), result.Requests)
	for _, r := range result.Visited {
		fmt.Printf("  %s -> id=%v\n", r.URL, r.Data["id"])
	}

	// A malformed API that always returns a fresh "next" link: an
	// unbounded chain with no cycle for the seen-set to catch.
	counter := 0
	endless := hateoas.FetcherFunc(func(url string) (hateoas.Resource, error) {
		counter++
		next := fmt.Sprintf("/endless/%d", counter)
		return hateoas.Resource{Links: map[string]string{"next": next}}, nil
	})
	_, err = hateoas.Crawl(endless, "/endless/0", 5, 100)
	fmt.Println("endless chain result:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
visited 3 resources in 3 requests
  /orders/1 -> id=1
  /orders/2 -> id=2
  /orders/3 -> id=3
endless chain result: hateoas: link chain exceeds maximum depth: /endless/5 at depth 6
```

### Tests

`TestCrawlVisitsEveryLinkedResource` checks the basic acyclic case.
`TestCrawlHandlesCycleWithoutRevisiting` is the demo's pagination bug as
an assertion: exactly three requests, no error, despite the cycle.
`TestCrawlRejectsChainPastMaxDepth` uses an endless `FetcherFunc` to prove
the depth guard catches what the seen-set structurally cannot.
`TestCrawlRejectsFanOutPastMaxRequests` proves the request budget catches
a shallow but wide graph that neither the seen-set nor the depth guard
would stop. `TestCrawlPropagatesFetchError` and `TestCrawlRejectsInvalidBudgets`
cover ordinary error handling.

Create `hateoas_test.go`:

```go
package hateoas

import (
	"errors"
	"fmt"
	"testing"
)

func TestCrawlVisitsEveryLinkedResource(t *testing.T) {
	t.Parallel()

	api := MapFetcher{
		"/a": {Links: map[string]string{"next": "/b"}},
		"/b": {Links: map[string]string{"next": "/c"}},
		"/c": {},
	}
	result, err := Crawl(api, "/a", 5, 10)
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if len(result.Visited) != 3 {
		t.Fatalf("len(Visited) = %d, want 3", len(result.Visited))
	}
	if result.Requests != 3 {
		t.Fatalf("Requests = %d, want 3", result.Requests)
	}
}

func TestCrawlHandlesCycleWithoutRevisiting(t *testing.T) {
	t.Parallel()

	// Page 3's "next" link points back to page 1: a pagination bug.
	api := MapFetcher{
		"/orders/1": {Links: map[string]string{"next": "/orders/2"}},
		"/orders/2": {Links: map[string]string{"next": "/orders/3", "prev": "/orders/1"}},
		"/orders/3": {Links: map[string]string{"next": "/orders/1", "prev": "/orders/2"}},
	}
	result, err := Crawl(api, "/orders/1", 10, 10)
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if result.Requests != 3 {
		t.Fatalf("Requests = %d, want 3 (each page fetched exactly once)", result.Requests)
	}
	if len(result.Visited) != 3 {
		t.Fatalf("len(Visited) = %d, want 3", len(result.Visited))
	}
}

func TestCrawlRejectsChainPastMaxDepth(t *testing.T) {
	t.Parallel()

	counter := 0
	endless := FetcherFunc(func(url string) (Resource, error) {
		counter++
		return Resource{Links: map[string]string{"next": fmt.Sprintf("/endless/%d", counter)}}, nil
	})

	_, err := Crawl(endless, "/endless/0", 5, 1000)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("Crawl() error = %v, want %v", err, ErrMaxDepthExceeded)
	}
}

func TestCrawlRejectsFanOutPastMaxRequests(t *testing.T) {
	t.Parallel()

	api := MapFetcher{
		"/root": {Links: map[string]string{"a": "/a", "b": "/b", "c": "/c"}},
		"/a":    {},
		"/b":    {},
		"/c":    {},
	}
	_, err := Crawl(api, "/root", 5, 2)
	if !errors.Is(err, ErrMaxRequestsExceeded) {
		t.Fatalf("Crawl() error = %v, want %v", err, ErrMaxRequestsExceeded)
	}
}

func TestCrawlPropagatesFetchError(t *testing.T) {
	t.Parallel()

	api := MapFetcher{
		"/a": {Links: map[string]string{"next": "/missing"}},
	}
	_, err := Crawl(api, "/a", 5, 10)
	if err == nil {
		t.Fatal("expected error for a link to a resource the fetcher does not have")
	}
}

func TestCrawlRejectsInvalidBudgets(t *testing.T) {
	t.Parallel()

	api := MapFetcher{"/a": {}}
	if _, err := Crawl(api, "/a", 0, 10); err == nil {
		t.Fatal("expected error for maxDepth < 1")
	}
	if _, err := Crawl(api, "/a", 5, 0); err == nil {
		t.Fatal("expected error for maxRequests < 1")
	}
}
```

## Review

`Crawl` is correct when it visits every resource reachable within its
budgets exactly once, tolerates any cycle silently, and fails cleanly and
specifically (which guard tripped, and where) whenever the graph exceeds
either budget. `TestCrawlRejectsChainPastMaxDepth` is the test that would
fail (by hanging) on a version of this exercise that relies on the
seen-set alone and skips the depth guard, reasoning that "cycles are the
only way this loops forever" — that reasoning is false the moment an API
can generate a URL it has never returned before, which is exactly what
`FetcherFunc`'s counter simulates. `TestCrawlRejectsFanOutPastMaxRequests`
is the parallel test for the opposite false assumption, that a depth cap
alone is enough: a single shallow resource with too many distinct links
never trips a depth guard at all.

## Resources

- [Richardson Maturity Model / HATEOAS overview](https://en.wikipedia.org/wiki/HATEOAS)
- [HAL (Hypertext Application Language) specification draft](https://datatracker.ietf.org/doc/html/draft-kelly-json-hal)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-feature-flag-rule-evaluation-memoized.md](29-feature-flag-rule-evaluation-memoized.md) | Next: [31-invoice-tree-line-item-aggregation.md](31-invoice-tree-line-item-aggregation.md)
