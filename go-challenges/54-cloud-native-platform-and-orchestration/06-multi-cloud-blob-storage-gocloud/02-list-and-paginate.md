# Exercise 2: Listing, Prefix Hierarchies and Cursor Pagination

A bucket is a flat keyspace, but real tooling needs to browse it: walk every key,
render one "directory" level, and serve a stateless paginated listing to an HTTP
client. This exercise builds all three on the two listing APIs the Go CDK
provides — the stateful iterator and the stateless page cursor.

## What you'll build

```text
bucketbrowser/                 independent module: example.com/bucketbrowser
  go.mod                       go 1.26; requires gocloud.dev
  browser.go                   Entry; ListAll (iterator); ListLevel (Delimiter); Page + NextPage (cursor)
  cmd/
    demo/
      main.go                  seeds keys, prints one directory level and a paged walk
  browser_test.go              flat listing, delimiter roll-up, full paginated recovery
```

Files: `browser.go`, `cmd/demo/main.go`, `browser_test.go`.
Implement: `ListAll` (walk the flat keyspace via `List`/`Next` to `io.EOF`), `ListLevel` (one directory level via `Delimiter`, exposing `IsDir` pseudo-entries), and `NextPage` (stateless `ListPage` cursor starting from `blob.FirstPageToken`).
Test: seed `a/1.txt`, `a/2.txt`, `b/1.txt`; assert `ListAll` returns all leaf keys sorted, `ListLevel` returns `a/` and `b/` as dirs, and a `NextPage` loop with `pageSize=1` recovers the full set and terminates on the empty token.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bucketbrowser/cmd/demo
cd ~/go-exercises/bucketbrowser
go mod init example.com/bucketbrowser
go get gocloud.dev/blob@latest
go mod edit -go=1.26
```

### The stateful iterator and io.EOF as a sentinel

`List(opts)` returns a `*blob.ListIterator`. You call `Next(ctx)` in a loop; each
call returns the next `*blob.ListObject`, and when the listing is exhausted it
returns `io.EOF`. `io.EOF` here is the normal, expected end — you break on it, you
do not log it as a failure. This is the shape for an in-process walk that runs to
completion in one call, because the iterator holds provider state (the underlying
list is itself paginated, but the iterator hides that). With no `Delimiter`, the
listing is flat: every key under the prefix comes back as its own leaf entry.
Providers return keys in lexicographic order, which is why the test can assert a
sorted result without sorting.

### Simulating a directory with Delimiter

Set `ListOptions.Delimiter` to `"/"` and the listing changes shape: instead of
returning every key under the prefix, it returns the entries at exactly one level
and rolls everything deeper into a single pseudo-entry per "subdirectory". Those
pseudo-entries have `IsDir == true` and a `Key` that ends with the delimiter
(`a/`, `b/`). There is no directory object in the bucket — the driver synthesizes
these from the shared key prefixes. That is how a file-browser UI renders one
folder level with one request instead of downloading the entire keyspace and
grouping client-side. To descend into `a/`, you list again with `Prefix: "a/"`.

### Stateless pagination with an opaque token

An HTTP endpoint cannot hold a `ListIterator` across requests — each request is a
fresh call, possibly to a different server instance. `ListPage(ctx, token,
pageSize, opts)` is built for exactly this: it returns up to `pageSize` objects
and an opaque `nextPageToken []byte`. The first call passes `blob.FirstPageToken`;
each subsequent call passes back the token the previous call returned; and a
returned token of length zero means there are no more pages. `NextPage` here wraps
that: a caller (or an HTTP handler) passes the token it last received — `nil` on
the first request — and `NextPage` substitutes `blob.FirstPageToken` for an empty
input so the client never has to know the sentinel. The token is opaque: never
parse it, never construct one, just echo it back. The loop terminates precisely
when the returned token has length zero; ignoring that and re-sending a non-empty
token forever is the classic pagination hang.

Create `browser.go`:

```go
// browser.go
package bucketbrowser

import (
	"context"
	"errors"
	"fmt"
	"io"

	"gocloud.dev/blob"
)

// Entry is one listing result, either a leaf object or a synthesized directory.
type Entry struct {
	Key   string
	Size  int64
	IsDir bool
}

// ListAll walks the flat keyspace under prefix to completion, treating io.EOF
// as the normal end of the listing.
func ListAll(ctx context.Context, b *blob.Bucket, prefix string) ([]Entry, error) {
	it := b.List(&blob.ListOptions{Prefix: prefix})
	var out []Entry
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, err)
		}
		out = append(out, Entry{Key: obj.Key, Size: obj.Size, IsDir: obj.IsDir})
	}
}

// ListLevel returns exactly one directory level under prefix. Sub-hierarchies
// roll up into IsDir pseudo-entries whose Key ends in "/".
func ListLevel(ctx context.Context, b *blob.Bucket, prefix string) ([]Entry, error) {
	it := b.List(&blob.ListOptions{Prefix: prefix, Delimiter: "/"})
	var out []Entry
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list level %q: %w", prefix, err)
		}
		out = append(out, Entry{Key: obj.Key, Size: obj.Size, IsDir: obj.IsDir})
	}
}

// Page is a stateless slice of a listing plus the opaque cursor for the next
// call. A zero-length NextToken means the listing is exhausted.
type Page struct {
	Entries   []Entry
	NextToken []byte
}

// NextPage serves one page for an HTTP endpoint. Pass nil (or the empty slice)
// as token to start; pass back the previous Page.NextToken to continue.
func NextPage(ctx context.Context, b *blob.Bucket, token []byte, pageSize int, prefix string) (Page, error) {
	if len(token) == 0 {
		token = blob.FirstPageToken
	}
	objs, next, err := b.ListPage(ctx, token, pageSize, &blob.ListOptions{Prefix: prefix})
	if err != nil {
		return Page{}, fmt.Errorf("list page %q: %w", prefix, err)
	}
	entries := make([]Entry, len(objs))
	for i, o := range objs {
		entries[i] = Entry{Key: o.Key, Size: o.Size, IsDir: o.IsDir}
	}
	return Page{Entries: entries, NextToken: next}, nil
}
```

### The runnable demo

The demo seeds a memblob bucket with a small tree, prints the top directory level
(showing the `IsDir` roll-up), then walks the whole keyspace one page at a time to
show the cursor terminating on its own.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/bucketbrowser"
	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"
)

func main() {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()

	for _, k := range []string{"a/1.txt", "a/2.txt", "b/1.txt"} {
		if err := b.WriteAll(ctx, k, []byte("x"), nil); err != nil {
			log.Fatal(err)
		}
	}

	level, err := bucketbrowser.ListLevel(ctx, b, "")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("top level:")
	for _, e := range level {
		fmt.Printf("  %s dir=%v\n", e.Key, e.IsDir)
	}

	fmt.Println("paged walk (pageSize=1):")
	var token []byte
	for {
		page, err := pageOnce(ctx, b, token)
		if err != nil {
			log.Fatal(err)
		}
		for _, e := range page.Entries {
			fmt.Printf("  %s\n", e.Key)
		}
		if len(page.NextToken) == 0 {
			break
		}
		token = page.NextToken
	}
}

func pageOnce(ctx context.Context, b *blob.Bucket, token []byte) (bucketbrowser.Page, error) {
	return bucketbrowser.NextPage(ctx, b, token, 1, "")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
top level:
  a/ dir=true
  b/ dir=true
paged walk (pageSize=1):
  a/1.txt
  a/2.txt
  b/1.txt
```

### Tests

`TestListAllFlat` seeds the tree and asserts every leaf key comes back in sorted
order with no directory entries. `TestListLevelDelimiter` asserts the top level
is exactly `a/` and `b/`, both marked `IsDir`. `TestPaginateRecoversAll` is the
important one: it drives `NextPage` with `pageSize=1` from a `nil` token, collects
keys until the returned token has length zero, and asserts both that the loop
terminates and that the accumulated set equals the full keyspace — proving the
cursor is complete and self-terminating. An `Example` prints one directory level.

Create `browser_test.go`:

```go
// browser_test.go
package bucketbrowser

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"
)

func seed(t *testing.T, keys ...string) *blob.Bucket {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	for _, k := range keys {
		if err := b.WriteAll(t.Context(), k, []byte("x"), nil); err != nil {
			t.Fatalf("seed %q: %v", k, err)
		}
	}
	return b
}

func TestListAllFlat(t *testing.T) {
	t.Parallel()
	b := seed(t, "a/1.txt", "a/2.txt", "b/1.txt")

	got, err := ListAll(t.Context(), b, "")
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	var keys []string
	for _, e := range got {
		if e.IsDir {
			t.Errorf("flat listing returned a dir entry %q", e.Key)
		}
		keys = append(keys, e.Key)
	}
	want := []string{"a/1.txt", "a/2.txt", "b/1.txt"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("ListAll keys = %v, want %v", keys, want)
	}
}

func TestListLevelDelimiter(t *testing.T) {
	t.Parallel()
	b := seed(t, "a/1.txt", "a/2.txt", "b/1.txt")

	got, err := ListLevel(t.Context(), b, "")
	if err != nil {
		t.Fatalf("ListLevel: %v", err)
	}
	var dirs []string
	for _, e := range got {
		if !e.IsDir {
			t.Errorf("top level returned a leaf %q; want only dirs", e.Key)
		}
		dirs = append(dirs, e.Key)
	}
	want := []string{"a/", "b/"}
	if !reflect.DeepEqual(dirs, want) {
		t.Fatalf("ListLevel dirs = %v, want %v", dirs, want)
	}
}

func TestPaginateRecoversAll(t *testing.T) {
	t.Parallel()
	b := seed(t, "a/1.txt", "a/2.txt", "b/1.txt")

	var token []byte
	var keys []string
	pages := 0
	for {
		page, err := NextPage(t.Context(), b, token, 1, "")
		if err != nil {
			t.Fatalf("NextPage: %v", err)
		}
		for _, e := range page.Entries {
			keys = append(keys, e.Key)
		}
		pages++
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
		if len(page.NextToken) == 0 {
			break
		}
		token = page.NextToken
	}
	want := []string{"a/1.txt", "a/2.txt", "b/1.txt"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("paginated keys = %v, want %v", keys, want)
	}
}

func Example() {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()
	_ = b.WriteAll(ctx, "docs/readme.md", []byte("x"), nil)

	level, _ := ListLevel(ctx, b, "")
	fmt.Printf("%s %v\n", level[0].Key, level[0].IsDir)
	// Output: docs/ true
}
```

## Review

The listing code is correct when `ListAll` treats `io.EOF` as the loop's normal
exit (not an error), when `ListLevel` surfaces `IsDir` pseudo-entries whose keys
end in `/`, and when `NextPage` starts from `blob.FirstPageToken` and terminates
exactly on a zero-length token. The bug this lesson guards against is the
non-terminating page loop: `TestPaginateRecoversAll` both checks the full set is
recovered and caps the iteration, so a cursor that never signals "done" fails
instead of hanging. Do not sort the results yourself expecting the provider not
to — object stores list lexicographically, and the tests rely on that; if you
need a different order, sort explicitly and say so. Do not confuse `ListLevel`
with `ListAll`: the delimiter is what makes hierarchy appear, and without it you
get every leaf. Run `go test -count=1 -race ./...` to confirm.

## Resources

- [gocloud.dev/blob List and ListPage](https://pkg.go.dev/gocloud.dev/blob#Bucket.List) — the iterator, `ListOptions`, and the paging cursor.
- [Go CDK How-To: Blob storage](https://gocloud.dev/howto/blob/) — listing with prefixes and delimiters.
- [io.EOF](https://pkg.go.dev/io#EOF) — the sentinel returned at the end of a listing.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-portable-object-store.md](01-portable-object-store.md) | Next: [03-conditional-writes-and-idempotency.md](03-conditional-writes-and-idempotency.md)
