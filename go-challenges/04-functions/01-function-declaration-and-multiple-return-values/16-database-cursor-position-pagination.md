# Exercise 16: Cursor-Based Pagination Over SQL Rows

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Offset pagination (`LIMIT 20 OFFSET 4000`) gets slower the deeper a client
pages, because the database still has to walk and discard every skipped
row. Cursor pagination fixes this by encoding a *position* — typically the
last row's key — into an opaque token the client hands back on the next
request, turning `OFFSET 4000` into `WHERE id > ?`, an index seek instead of
a scan. This exercise builds a `Store.FetchPage(cursor, limit, dest)
(nextCursor string, hasMore bool, error)` that writes the page into a
caller-owned destination slice and returns only the pagination bookkeeping
as its three-value result.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
cursorpage/                 independent module: example.com/database-cursor-position-pagination
  go.mod                    go 1.24
  cursorpage.go             package cursorpage; Row, Store, encode/decodeCursor, FetchPage(cursor,limit,dest) (nextCursor,hasMore,error)
  cmd/
    demo/
      main.go               walks all pages of a 7-row table with limit=3
  cursorpage_test.go        full walk; last page hasMore=false; invalid cursor; non-positive limit
```

- Files: `cursorpage.go`, `cmd/demo/main.go`, `cursorpage_test.go`.
- Implement: `Store.FetchPage(cursor string, limit int, dest *[]Row) (nextCursor string, hasMore bool, err error)`, over-fetching `limit+1` rows to detect a further page without a second query, encoding the cursor as an opaque token over the last kept row's ID.
- Test: walking every page of a fixed table recovers every row in order; the final page reports `hasMore == false`; a malformed cursor and a non-positive limit both return an error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/16-database-cursor-position-pagination/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/16-database-cursor-position-pagination
go mod edit -go=1.24
```

### Why the cursor is not the whole answer

The instinct is to give `FetchPage` a single return: `(page []Row, error)`,
and let the caller compute the next cursor from `page[len(page)-1].ID`
itself. That leaks an implementation detail — "cursors are last-row IDs" —
into every call site, and it breaks the moment the store switches to a
composite key or a keyset that isn't the row's own ID. Keeping cursor
encoding inside the store means the three-value return
`(nextCursor, hasMore, error)` is the entire contract a caller needs:
opaque token in, opaque token out, a boolean for "is there more", an error
for "something went wrong deciding". The actual page of rows goes into
`dest`, a caller-owned destination — the same shape `database/sql`'s
`Row.Scan` uses, passing pointers in rather than turning every value into
a return.

The core trick is over-fetching by one:

```go
end := start + limit + 1
window := s.rows[start:end]
hasMore = len(window) > limit
```

Asking for `limit+1` rows and checking whether the extra one showed up
tells you whether more data exists *without* a second round trip (a
`COUNT(*)` or a second query) — the same technique `database/sql` drivers
use internally for "has next" style cursors.

Create `cursorpage.go`:

```go
package cursorpage

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// ErrInvalidCursor is returned when a caller-supplied cursor cannot be
// decoded back into a row position — a tampered, truncated, or foreign
// cursor string.
var ErrInvalidCursor = errors.New("invalid cursor")

// Row is one record of the simulated table, ordered by ID.
type Row struct {
	ID   int
	Name string
}

// Store simulates a SQL table with a stable ORDER BY id ascending.
type Store struct {
	rows []Row
}

// NewStore copies rows and sorts them by ID, standing in for a table with
// a primary-key index.
func NewStore(rows []Row) *Store {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return &Store{rows: sorted}
}

// encodeCursor turns a row ID into an opaque, URL-safe cursor string —
// callers must treat it as a token, never parse it themselves.
func encodeCursor(id int) string {
	return base64.URLEncoding.EncodeToString([]byte(strconv.Itoa(id)))
}

// decodeCursor reverses encodeCursor. An empty cursor decodes to position 0
// (the start of the table); anything else that fails to parse is
// ErrInvalidCursor.
func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	b, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	id, err := strconv.Atoi(string(b))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	if id < 0 {
		return 0, fmt.Errorf("%w: negative position", ErrInvalidCursor)
	}
	return id, nil
}

// FetchPage simulates `SELECT * FROM rows WHERE id > ? ORDER BY id LIMIT ?+1`
// followed by pagination bookkeeping. It writes up to limit rows into dest
// (a caller-owned destination, the way database/sql.Rows scans into
// caller-owned variables) and returns the opaque cursor for the next page,
// whether more rows remain beyond this page, and any error decoding cursor
// or validating limit.
func (s *Store) FetchPage(cursor string, limit int, dest *[]Row) (nextCursor string, hasMore bool, err error) {
	if limit <= 0 {
		return "", false, fmt.Errorf("limit must be positive, got %d", limit)
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return "", false, err
	}

	start := sort.Search(len(s.rows), func(i int) bool { return s.rows[i].ID > after })

	// Over-fetch by one row to detect whether another page exists without
	// a second round trip.
	end := start + limit + 1
	if end > len(s.rows) {
		end = len(s.rows)
	}
	window := s.rows[start:end]

	hasMore = len(window) > limit
	if hasMore {
		window = window[:limit]
	}

	*dest = append((*dest)[:0], window...)

	if len(window) == 0 {
		return "", false, nil
	}
	last := window[len(window)-1]
	return encodeCursor(last.ID), hasMore, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/database-cursor-position-pagination"
)

func main() {
	rows := make([]cursorpage.Row, 0, 7)
	for i := 1; i <= 7; i++ {
		rows = append(rows, cursorpage.Row{ID: i, Name: fmt.Sprintf("item-%d", i)})
	}
	store := cursorpage.NewStore(rows)

	cursor := ""
	page := 1
	for {
		var dest []cursorpage.Row
		next, hasMore, err := store.FetchPage(cursor, 3, &dest)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		ids := make([]int, 0, len(dest))
		for _, r := range dest {
			ids = append(ids, r.ID)
		}
		fmt.Printf("page %d: ids=%v hasMore=%t\n", page, ids, hasMore)
		if !hasMore {
			break
		}
		cursor = next
		page++
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page 1: ids=[1 2 3] hasMore=true
page 2: ids=[4 5 6] hasMore=true
page 3: ids=[7] hasMore=false
```

### Tests

Create `cursorpage_test.go`:

```go
package cursorpage

import "testing"

func newTestStore() *Store {
	rows := make([]Row, 0, 7)
	for i := 1; i <= 7; i++ {
		rows = append(rows, Row{ID: i})
	}
	return NewStore(rows)
}

func TestFetchPageWalksAllRows(t *testing.T) {
	t.Parallel()
	store := newTestStore()

	var gotIDs []int
	cursor := ""
	for i := 0; i < 10; i++ { // bound the loop so a bug can't hang the test
		var dest []Row
		next, hasMore, err := store.FetchPage(cursor, 3, &dest)
		if err != nil {
			t.Fatalf("FetchPage: %v", err)
		}
		for _, r := range dest {
			gotIDs = append(gotIDs, r.ID)
		}
		if !hasMore {
			break
		}
		cursor = next
	}

	want := []int{1, 2, 3, 4, 5, 6, 7}
	if len(gotIDs) != len(want) {
		t.Fatalf("got %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("got %v, want %v", gotIDs, want)
		}
	}
}

func TestFetchPageLastPageHasMoreFalse(t *testing.T) {
	t.Parallel()
	store := newTestStore()

	var dest []Row
	// Page 1: ids 1-3.
	cursor, hasMore, err := store.FetchPage("", 3, &dest)
	if err != nil || !hasMore {
		t.Fatalf("page1: cursor=%q hasMore=%t err=%v", cursor, hasMore, err)
	}
	// Page 2: ids 4-6.
	cursor, hasMore, err = store.FetchPage(cursor, 3, &dest)
	if err != nil || !hasMore {
		t.Fatalf("page2: cursor=%q hasMore=%t err=%v", cursor, hasMore, err)
	}
	// Page 3: id 7 only, no more after.
	_, hasMore, err = store.FetchPage(cursor, 3, &dest)
	if err != nil {
		t.Fatalf("page3: err=%v", err)
	}
	if hasMore {
		t.Fatal("page3: hasMore = true, want false")
	}
	if len(dest) != 1 || dest[0].ID != 7 {
		t.Fatalf("page3 dest = %+v, want [{7}]", dest)
	}
}

func TestFetchPageInvalidCursor(t *testing.T) {
	t.Parallel()
	store := newTestStore()

	var dest []Row
	_, _, err := store.FetchPage("not-a-valid-cursor!!!", 3, &dest)
	if err == nil {
		t.Fatal("want an error for a malformed cursor, got nil")
	}
}

func TestFetchPageRejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	store := newTestStore()

	var dest []Row
	_, _, err := store.FetchPage("", 0, &dest)
	if err == nil {
		t.Fatal("want an error for limit=0, got nil")
	}
}
```

## Review

The store is correct when the page and the cursor stay in lockstep: the
over-fetch of `limit+1` rows is the only signal `hasMore` ever needs, and
the cursor always encodes the *last kept* row of the current page, never
the extra probe row. `TestFetchPageWalksAllRows` is the load-bearing test —
it drives the pagination loop the way a real client would, bounded so a
cursor bug that never sets `hasMore=false` fails loudly instead of hanging
the test suite. `TestFetchPageLastPageHasMoreFalse` catches the
off-by-one that is easy to introduce: forgetting to trim the probe row
before encoding the cursor, which would leak the extra row into the page
or point the next cursor one row too far.

The mistake to avoid is encoding the cursor from the *first* row of a page
or from `limit` itself instead of the actual last row's ID — either one
works by accident when every page is full, and both break silently the
moment a page is short (the last page, or any page after a delete).

## Resources

- [Use the cursor pattern for pagination](https://use-the-index-luke.com/no-offset) — why offset pagination degrades and cursor/keyset pagination does not.
- [sort.Search](https://pkg.go.dev/sort#Search) — binary search over the sorted table, used to find the start of the next page.
- [database/sql.Rows.Scan](https://pkg.go.dev/database/sql#Rows.Scan) — the caller-owned-destination pattern `FetchPage` mirrors with `dest *[]Row`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-tiered-config-lookup-value-found-source.md](15-tiered-config-lookup-value-found-source.md) | Next: [17-oauth-token-decode-claims.md](17-oauth-token-decode-claims.md)
