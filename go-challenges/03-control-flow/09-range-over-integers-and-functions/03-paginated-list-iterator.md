# Exercise 3: Cloud SDK Pagination — Lazy `iter.Seq` Across Page Boundaries

Cloud list APIs (S3 `ListObjectsV2`, DynamoDB `Scan`, most REST list endpoints)
return one page plus an opaque `NextPageToken`; you fetch the next page only by
passing that token back. This exercise wraps that protocol in an
`iter.Seq[Object]` so pagination becomes a lazy fold: the next network round-trip
happens only when the consumer exhausts the current page and asks for more, and a
`Take` or `break` prevents the speculative fetch entirely.

## What you'll build

```text
listing/                  independent module: example.com/listing
  go.mod                  module example.com/listing
  listing.go              Object, Page, Client, ListObjects, FakeClient
  cmd/
    demo/
      main.go             runnable demo: list across pages, print fetch count
  listing_test.go         full enumeration, mid-page break, empty-set tests
```

Files: `listing.go`, `cmd/demo/main.go`, `listing_test.go`.
Implement: `ListObjects(c Client, prefix string) iter.Seq[Object]` that pages via `NextPageToken`, plus a `FakeClient` that counts fetches.
Test: full enumeration flattens all pages and fetches exactly N; a mid-page break fetches exactly one page; an empty result fetches once and yields nothing.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/listing/cmd/demo
cd ~/go-exercises/listing
go mod init example.com/listing
```

## The design

`ListObjects` runs an unbounded loop over pages. It starts with an empty token,
fetches a page, yields each item, and then decides: if the page's
`NextPageToken` is empty the listing is done; otherwise it stores the token and
loops to fetch the next page. The subtle correctness point is *ordering*: the
next fetch must happen only after the current page is fully yielded AND the
consumer is still consuming. Because each `yield` can return `false`, a consumer
that breaks mid-page returns from the iterator before the loop ever reaches the
next fetch — so `Take(1, ListObjects(...))` costs exactly one HTTP round-trip,
never two.

The failure mode this design avoids is the eager paginator that fetches all pages
up front into a `[]Object`. On a bucket with millions of keys that is both a
memory blowup and a latency disaster; the caller usually wants the first few
matches. Modeling pagination as a lazy `iter.Seq` puts the "how much to fetch"
decision where it belongs — with the consumer.

The `FakeClient` stands in for the network and counts `ListPage` calls, so the
tests can assert the exact number of round-trips. Each fake page carries a token
that is just the next page index rendered as a string; a real client would treat
it as opaque bytes.

Create `listing.go`:

```go
package listing

import (
	"iter"
	"strconv"
)

// Object is one entry in a listing.
type Object struct {
	Key  string
	Size int64
}

// Page is one response: a slice of items and a token for the next page. An
// empty NextPageToken marks the end of the listing.
type Page struct {
	Items         []Object
	NextPageToken string
}

// Client models a paginated list API. token is empty for the first page.
type Client interface {
	ListPage(prefix, token string) (Page, error)
}

// ListObjects streams every object under prefix, fetching the next page only
// when the current one is exhausted and the consumer is still consuming.
func ListObjects(c Client, prefix string) iter.Seq[Object] {
	return func(yield func(Object) bool) {
		token := ""
		for {
			page, err := c.ListPage(prefix, token)
			if err != nil {
				return
			}
			for _, obj := range page.Items {
				if !yield(obj) {
					return
				}
			}
			if page.NextPageToken == "" {
				return
			}
			token = page.NextPageToken
		}
	}
}

// FakeClient serves Pages from an in-memory slice-of-slices and counts fetches,
// modeling network round-trips without a real endpoint.
type FakeClient struct {
	Pages   [][]Object
	Fetches int
}

func (c *FakeClient) ListPage(prefix, token string) (Page, error) {
	c.Fetches++
	idx := 0
	if token != "" {
		idx, _ = strconv.Atoi(token)
	}
	if idx >= len(c.Pages) {
		return Page{}, nil
	}
	next := ""
	if idx+1 < len(c.Pages) {
		next = strconv.Itoa(idx + 1)
	}
	return Page{Items: c.Pages[idx], NextPageToken: next}, nil
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/listing"
)

func main() {
	c := &listing.FakeClient{Pages: [][]listing.Object{
		{{Key: "a", Size: 1}, {Key: "b", Size: 2}},
		{{Key: "c", Size: 3}, {Key: "d", Size: 4}},
		{{Key: "e", Size: 5}},
	}}

	for obj := range listing.ListObjects(c, "") {
		fmt.Printf("%s (%d)\n", obj.Key, obj.Size)
	}
	fmt.Printf("fetched %d page(s)\n", c.Fetches)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a (1)
b (2)
c (3)
d (4)
e (5)
fetched 3 page(s)
```

## Tests

Create `listing_test.go`:

```go
package listing

import (
	"reflect"
	"testing"
)

func threePages() *FakeClient {
	return &FakeClient{Pages: [][]Object{
		{{Key: "a"}, {Key: "b"}},
		{{Key: "c"}, {Key: "d"}},
		{{Key: "e"}},
	}}
}

func TestFullEnumeration(t *testing.T) {
	t.Parallel()

	c := threePages()
	var keys []string
	for obj := range ListObjects(c, "") {
		keys = append(keys, obj.Key)
	}

	want := []string{"a", "b", "c", "d", "e"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if c.Fetches != 3 {
		t.Fatalf("Fetches = %d, want 3", c.Fetches)
	}
}

func TestBreakInsideFirstPageDoesNotPrefetch(t *testing.T) {
	t.Parallel()

	c := threePages()
	for obj := range ListObjects(c, "") {
		if obj.Key == "a" {
			break
		}
	}

	if c.Fetches != 1 {
		t.Fatalf("Fetches = %d, want 1 (no speculative next-page fetch)", c.Fetches)
	}
}

func TestEmptyResultFetchesOnce(t *testing.T) {
	t.Parallel()

	c := &FakeClient{Pages: nil}
	count := 0
	for range ListObjects(c, "") {
		count++
	}

	if count != 0 {
		t.Fatalf("yielded %d objects, want 0", count)
	}
	if c.Fetches != 1 {
		t.Fatalf("Fetches = %d, want 1", c.Fetches)
	}
}
```

## Review

Pagination is correct when the number of fetches equals the number of pages the
consumer actually needed — not one more. The full-enumeration test pins three
fetches for three pages; the break test pins exactly one fetch because the
consumer stopped inside page one and the iterator returned before it could
request the next page's token. That is the lazy-fold property: the network
round-trip is deferred until the consumer proves it wants more. The empty-set
case still fetches once because the client cannot know the result is empty until
it asks. Note this exercise swallows a client error by returning; the per-item
error idiom for streams that must surface failures mid-flight is
`iter.Seq2[T, error]`, covered in the streaming-decoder exercise.

## Resources

- [`iter` package documentation](https://pkg.go.dev/iter)
- [S3 ListObjectsV2 (pagination tokens)](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-repository-seq-scan.md](02-repository-seq-scan.md) | Next: [04-pull-merge-sorted-streams.md](04-pull-merge-sorted-streams.md)
