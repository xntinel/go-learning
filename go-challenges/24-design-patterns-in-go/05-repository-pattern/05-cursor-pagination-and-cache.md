# Exercise 5: A Paginated, Filterable Account Repository With A Read-Through Cache

A production repository is rarely asked for everything at once; it is asked for "the next page of active accounts whose name starts with A", over and over, and it must answer fast. This exercise builds an account repository with filtering, cursor-based pagination behind an opaque token, and a read-through cache layered in front of single-entity reads, then drives the whole thing through a small service so you can see how the pieces compose.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
accounts.go              Account entity, Filter, opaque cursor encode/decode,
                         Repository interface, in-memory Store with a filtered,
                         sorted, cursor-paginated List, CachedRepository
                         read-through decorator, AccountService over the interface
cmd/
  demo/
    main.go              walk active accounts two-per-page by cursor, then show a
                         cache hit and a write-driven invalidation
accounts_test.go         pagination walks every row once, page boundaries
                         (exact multiple, oversize page, empty result), bad
                         cursor rejected, cache hit/miss + invalidation, -race
                         concurrent reads
```

- Files: `accounts.go`, `cmd/demo/main.go`, `accounts_test.go`.
- Implement: the `Account` entity, the `Filter`, the opaque `encodeCursor`/`decodeCursor` pair, the `Repository` interface, the in-memory `Store` with a paginated `List`, the `CachedRepository` read-through decorator, and the `AccountService`.
- Test: pagination visits every matching row exactly once and terminates, the boundary cases behave, a malformed cursor is rejected, the cache serves hits and is invalidated by writes, and concurrent reads are race-free.
- Verify: `go test -race ./...`

### Why cursor pagination and what the opaque token hides

The naive way to page is offset/limit: "give me 20 rows starting at row 40". It is easy and it is wrong at scale for two reasons. It gets slower the deeper you go, because the store must still walk and discard the first forty rows to reach the forty-first. Worse, it is unstable under concurrent writes: if a row is inserted near the front between two requests, every later row shifts by one, so page two re-shows a row that was on page one or skips one entirely. Cursor pagination fixes both. Instead of "start at index N" it says "start after this key", where the key is a value from a stable total order. Here accounts are ordered by `ID`, and a page of size `n` is "the first `n` matching accounts whose `ID` is greater than the cursor's ID". Resuming is a `sort.Search` to the first ID past the cursor, not a walk-and-discard, and an insert elsewhere in the set cannot shift the boundary, so no row is shown twice or skipped.

The cursor is deliberately opaque: the caller receives a `NextCursor` string and passes it back verbatim, with no contract about its contents. Internally it is just the last returned ID with a version tag, base64-encoded — but encoding it makes the boundary an implementation detail the client cannot depend on. If a later version changes the sort key from `ID` to `(CreatedAt, ID)`, the token's bytes change and no client breaks, because no client was ever parsing them. The encode/decode pair is the whole contract: `encodeCursor` turns an internal key into the opaque token, `decodeCursor` validates the token and rejects a malformed one with `ErrBadCursor` rather than trusting attacker-supplied bytes. The version prefix (`v1:`) is what lets `decodeCursor` tell a token it minted from random garbage.

Filtering composes with paging but is applied first: `List` selects the matching subset, sorts it into the stable order, then slices the page out of that subset. The cursor therefore points into the filtered, ordered sequence — which is why a cursor minted under one filter should be replayed under the same filter; the token addresses a position in "active accounts named A...", not an absolute row.

The page slice itself encodes the has-more signal without a separate count query. `List` looks at the window of rows after the cursor: if that window has more than a page worth, it returns one page and sets `NextCursor` to the last returned ID; if the window has a page or fewer, those are the last rows and `NextCursor` is empty. An empty `NextCursor` is the canonical "you have reached the end" signal, which is exactly what the demo's loop and the boundary tests check.

### Why the cache wraps reads but not pages

Layered in front of the store is a `CachedRepository`, the same decorator shape as exercise 3: it implements `Repository`, holds an inner `Repository`, and serves `Get` from an in-memory map, falling through on a miss and invalidating on every `Save`/`Delete`. Single-entity reads are the right thing to cache — a profile page reads the same account repeatedly, and those reads are a perfect hit. List results are deliberately not cached: a page is a function of the live data plus a filter plus a cursor, the data changes underneath it, and caching a page invites serving a stale or torn page that omits a row inserted after it was cached. So `List` delegates straight through while `Get` is cached, which is an honest statement of what is safe to memoize and what is not. The invalidation discipline is the same as before: a write evicts the entry after the inner write succeeds, so the next `Get` re-reads the fresh value, and a failed write never evicts a still-valid entry.

The `AccountService` sits on top of the `Repository` interface and never names a concrete type. It reads a profile through `Get` (cache-accelerated), lists a page of active accounts through `List`, and renames by `Get`-modify-`Save`, where the `Save` invalidates the cache so a subsequent read sees the new name. Because it depends only on the interface, the same service runs over the bare `Store`, the `CachedRepository`, or a future SQL-backed implementation, with the cache being a composition-root decision rather than something the service knows about.

Create `accounts.go`:

```go
package accounts

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Domain sentinels.
var (
	ErrNotFound  = errors.New("accounts: not found")
	ErrBadCursor = errors.New("accounts: malformed cursor")
)

// DefaultPageSize is used when a PageRequest asks for a non-positive size.
const DefaultPageSize = 20

// Account is the stored entity. ID is the stable pagination key.
type Account struct {
	ID     string
	Name   string
	Email  string
	Active bool
}

// Filter selects a subset of accounts. The zero Filter matches everything.
type Filter struct {
	ActiveOnly bool
	NamePrefix string
}

func (f Filter) matches(a Account) bool {
	if f.ActiveOnly && !a.Active {
		return false
	}
	if f.NamePrefix != "" && !strings.HasPrefix(a.Name, f.NamePrefix) {
		return false
	}
	return true
}

// PageRequest asks for up to Size accounts after Cursor. An empty Cursor starts
// at the beginning of the filtered, ordered sequence.
type PageRequest struct {
	Cursor string
	Size   int
}

// Page is one page of results plus the opaque cursor for the next page. An empty
// NextCursor means there are no more results.
type Page struct {
	Items      []Account
	NextCursor string
}

// encodeCursor turns an internal key into an opaque token the client echoes back
// without interpreting it.
func encodeCursor(lastID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + lastID))
}

// decodeCursor validates a token and returns the key after which to resume. An
// empty token means "from the beginning"; anything we did not mint is rejected.
func decodeCursor(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", ErrBadCursor
	}
	rest, ok := strings.CutPrefix(string(raw), "v1:")
	if !ok {
		return "", ErrBadCursor
	}
	return rest, nil
}

// Repository is the contract the service depends on. The cache and the store
// both satisfy it.
type Repository interface {
	Get(ctx context.Context, id string) (Account, error)
	Save(ctx context.Context, a Account) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, f Filter, p PageRequest) (Page, error)
}

// Store is the in-memory backing Repository, safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	data map[string]Account
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{data: make(map[string]Account)}
}

func (s *Store) Get(ctx context.Context, id string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	return a, nil
}

// Save upserts an account by ID.
func (s *Store) Save(ctx context.Context, a Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[a.ID] = a
	return nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return ErrNotFound
	}
	delete(s.data, id)
	return nil
}

// List applies the filter, orders the matches by ID, and slices out the page
// after the cursor. NextCursor is empty when the page reaches the end.
func (s *Store) List(ctx context.Context, f Filter, p PageRequest) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	after, err := decodeCursor(p.Cursor)
	if err != nil {
		return Page{}, err
	}
	size := p.Size
	if size <= 0 {
		size = DefaultPageSize
	}

	s.mu.RLock()
	matched := make([]Account, 0, len(s.data))
	for _, a := range s.data {
		if f.matches(a) {
			matched = append(matched, a)
		}
	}
	s.mu.RUnlock()

	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })

	// Resume at the first ID strictly greater than the cursor.
	start := 0
	if after != "" {
		start = sort.Search(len(matched), func(i int) bool { return matched[i].ID > after })
	}
	window := matched[start:]

	var page Page
	if len(window) > size {
		page.Items = append(page.Items, window[:size]...)
		page.NextCursor = encodeCursor(page.Items[len(page.Items)-1].ID)
	} else {
		page.Items = append(page.Items, window...)
	}
	return page, nil
}

// CachedRepository is a read-through cache over an inner Repository. Get is
// cached; List is delegated unchanged; writes invalidate the touched entry.
type CachedRepository struct {
	next   Repository
	mu     sync.Mutex
	cache  map[string]Account
	hits   int
	misses int
}

// NewCachedRepository wraps next with a read-through single-entity cache.
func NewCachedRepository(next Repository) *CachedRepository {
	return &CachedRepository{next: next, cache: make(map[string]Account)}
}

func (r *CachedRepository) Get(ctx context.Context, id string) (Account, error) {
	r.mu.Lock()
	if a, ok := r.cache[id]; ok {
		r.hits++
		r.mu.Unlock()
		return a, nil
	}
	r.misses++
	r.mu.Unlock()

	a, err := r.next.Get(ctx, id)
	if err != nil {
		return Account{}, err
	}
	r.mu.Lock()
	r.cache[id] = a
	r.mu.Unlock()
	return a, nil
}

func (r *CachedRepository) Save(ctx context.Context, a Account) error {
	if err := r.next.Save(ctx, a); err != nil {
		return err
	}
	r.invalidate(a.ID)
	return nil
}

func (r *CachedRepository) Delete(ctx context.Context, id string) error {
	if err := r.next.Delete(ctx, id); err != nil {
		return err
	}
	r.invalidate(id)
	return nil
}

// List is intentionally not cached: a page depends on live data plus filter plus
// cursor, so it is delegated straight to the inner repository.
func (r *CachedRepository) List(ctx context.Context, f Filter, p PageRequest) (Page, error) {
	return r.next.List(ctx, f, p)
}

func (r *CachedRepository) invalidate(id string) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// Hits reports reads served from the cache.
func (r *CachedRepository) Hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

// Misses reports reads that fell through to the inner repository.
func (r *CachedRepository) Misses() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.misses
}

// AccountService is the application layer over any Repository.
type AccountService struct {
	repo Repository
}

// NewAccountService builds a service over repo.
func NewAccountService(repo Repository) *AccountService {
	return &AccountService{repo: repo}
}

// Profile reads one account; with a CachedRepository underneath, repeat reads
// are served from cache.
func (s *AccountService) Profile(ctx context.Context, id string) (Account, error) {
	return s.repo.Get(ctx, id)
}

// Rename loads, modifies, and saves; the Save invalidates the cache entry.
func (s *AccountService) Rename(ctx context.Context, id, name string) error {
	a, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	a.Name = name
	return s.repo.Save(ctx, a)
}

// ActivePage returns one cursor page of active accounts whose name has prefix.
func (s *AccountService) ActivePage(ctx context.Context, prefix, cursor string, size int) (Page, error) {
	return s.repo.List(ctx, Filter{ActiveOnly: true, NamePrefix: prefix}, PageRequest{Cursor: cursor, Size: size})
}
```

`Store` and `CachedRepository` both satisfy `Repository`; the compile-time assertions in the test pin that. The interesting line in `List` is `sort.Search(len(matched), func(i int) bool { return matched[i].ID > after })`: because `matched` is sorted ascending by ID, the predicate flips from false to true exactly at the first row past the cursor, so the search returns that index in logarithmic time without scanning the skipped prefix.

### The runnable demo

The demo seeds five accounts, one of them inactive, then walks the active ones two per page by feeding each page's `NextCursor` into the next request until the cursor comes back empty. Then it reads one profile twice to show a cache hit, and renames it to show that the write invalidates the cache so the following read misses and returns the new name.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/accounts"
)

func main() {
	ctx := context.Background()
	store := accounts.NewStore()
	cached := accounts.NewCachedRepository(store)
	svc := accounts.NewAccountService(cached)

	seed := []accounts.Account{
		{ID: "a01", Name: "Alice", Email: "alice@example.com", Active: true},
		{ID: "a02", Name: "Bob", Email: "bob@example.com", Active: true},
		{ID: "a03", Name: "Carol", Email: "carol@example.com", Active: true},
		{ID: "a04", Name: "Dave", Email: "dave@example.com", Active: false},
		{ID: "a05", Name: "Erin", Email: "erin@example.com", Active: true},
	}
	for _, a := range seed {
		_ = store.Save(ctx, a)
	}

	// Walk active accounts two per page using opaque cursors.
	cursor := ""
	for page := 1; ; page++ {
		p, _ := svc.ActivePage(ctx, "", cursor, 2)
		ids := make([]string, len(p.Items))
		for i, a := range p.Items {
			ids[i] = a.ID
		}
		fmt.Printf("page %d: %v more=%t\n", page, ids, p.NextCursor != "")
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}

	// Read-through cache: first read misses, second is served from cache.
	_, _ = svc.Profile(ctx, "a01")
	_, _ = svc.Profile(ctx, "a01")
	fmt.Printf("after 2 reads: hits=%d misses=%d\n", cached.Hits(), cached.Misses())

	// A write invalidates, so the next read misses and sees the new name.
	_ = svc.Rename(ctx, "a01", "Alice Smith")
	p, _ := svc.Profile(ctx, "a01")
	fmt.Printf("after rename: name=%s misses=%d\n", p.Name, cached.Misses())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page 1: [a01 a02] more=true
page 2: [a03 a05] more=false
after 2 reads: hits=1 misses=1
after rename: name=Alice Smith misses=2
```

The inactive account `a04` never appears because the filter drops it, so the four active accounts page as `[a01 a02]` then `[a03 a05]`, and the second page returns an empty cursor signaling the end. The two reads of `a01` are one miss then one hit. `Rename` reads `a01` (a hit, since it is still cached), saves, and invalidates; the final `Profile` therefore misses, lifting the miss count to two and returning the renamed value.

### Tests

`TestPaginationVisitsEveryRowOnce` is the load-bearing pagination test: it seeds seven accounts, walks them three per page, and asserts the concatenation of all pages equals the full sorted set with no duplicate and no omission, and that the walk terminates. `TestPageBoundaries` covers the three edges — a total that is an exact multiple of the page size, a page larger than the result set, and an empty result. `TestBadCursorRejected` confirms a token the store never minted is rejected. `TestCacheHitAndInvalidation` drives the read-through and the write-invalidation through the cache directly. `TestConcurrentReadsRaceFree` hammers `Get` from many goroutines so `-race` exercises the cache's mutex.

Create `accounts_test.go`:

```go
package accounts

import (
	"context"
	"errors"
	"sync"
	"testing"
)

var (
	_ Repository = (*Store)(nil)
	_ Repository = (*CachedRepository)(nil)
)

func seedN(t *testing.T, n int) *Store {
	t.Helper()
	ctx := context.Background()
	s := NewStore()
	for i := 1; i <= n; i++ {
		id := string(rune('a' + i - 1)) // a, b, c, ...
		if err := s.Save(ctx, Account{ID: id, Name: "N" + id, Active: true}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	return s
}

func TestPaginationVisitsEveryRowOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := seedN(t, 7)

	var got []string
	seen := make(map[string]bool)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
		page, err := s.List(ctx, Filter{}, PageRequest{Cursor: cursor, Size: 3})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, a := range page.Items {
			if seen[a.ID] {
				t.Fatalf("row %s returned twice", a.ID)
			}
			seen[a.ID] = true
			got = append(got, a.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	want := []string{"a", "b", "c", "d", "e", "f", "g"}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestPageBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ExactMultiple", func(t *testing.T) {
		s := seedN(t, 4)
		p1, _ := s.List(ctx, Filter{}, PageRequest{Size: 2})
		if len(p1.Items) != 2 || p1.NextCursor == "" {
			t.Fatalf("page1 = %v, cursor empty=%t; want 2 items and a cursor", p1.Items, p1.NextCursor == "")
		}
		p2, _ := s.List(ctx, Filter{}, PageRequest{Cursor: p1.NextCursor, Size: 2})
		if len(p2.Items) != 2 || p2.NextCursor != "" {
			t.Fatalf("page2 = %v, cursor=%q; want 2 items and empty cursor", p2.Items, p2.NextCursor)
		}
	})

	t.Run("PageLargerThanResult", func(t *testing.T) {
		s := seedN(t, 3)
		p, _ := s.List(ctx, Filter{}, PageRequest{Size: 50})
		if len(p.Items) != 3 || p.NextCursor != "" {
			t.Fatalf("page = %v, cursor=%q; want all 3 and empty cursor", p.Items, p.NextCursor)
		}
	})

	t.Run("EmptyResult", func(t *testing.T) {
		s := seedN(t, 3)
		p, err := s.List(ctx, Filter{NamePrefix: "zzz"}, PageRequest{Size: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(p.Items) != 0 || p.NextCursor != "" {
			t.Fatalf("page = %v, cursor=%q; want empty page and empty cursor", p.Items, p.NextCursor)
		}
	})
}

func TestBadCursorRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := seedN(t, 2)
	if _, err := s.List(ctx, Filter{}, PageRequest{Cursor: "not-a-valid-token!!", Size: 2}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("List with bad cursor = %v, want ErrBadCursor", err)
	}
}

func TestFilterCombines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()
	_ = s.Save(ctx, Account{ID: "1", Name: "Alice", Active: true})
	_ = s.Save(ctx, Account{ID: "2", Name: "Anna", Active: false})
	_ = s.Save(ctx, Account{ID: "3", Name: "Bob", Active: true})

	p, _ := s.List(ctx, Filter{ActiveOnly: true, NamePrefix: "A"}, PageRequest{Size: 10})
	if len(p.Items) != 1 || p.Items[0].ID != "1" {
		t.Fatalf("filtered = %v, want only account 1", p.Items)
	}
}

func TestCacheHitAndInvalidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := NewStore()
	_ = base.Save(ctx, Account{ID: "a1", Name: "Alice", Active: true})
	cached := NewCachedRepository(base)

	// First read misses and fills the cache; second is a hit.
	if _, err := cached.Get(ctx, "a1"); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if _, err := cached.Get(ctx, "a1"); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if cached.Hits() != 1 || cached.Misses() != 1 {
		t.Fatalf("hits=%d misses=%d, want 1 and 1", cached.Hits(), cached.Misses())
	}

	// A write invalidates, so the next read misses and sees the new value.
	if err := cached.Save(ctx, Account{ID: "a1", Name: "Alice Smith", Active: true}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := cached.Get(ctx, "a1")
	if err != nil || got.Name != "Alice Smith" {
		t.Fatalf("post-write read = %q, %v; want Alice Smith", got.Name, err)
	}
	if cached.Misses() != 2 {
		t.Errorf("misses after invalidation = %d, want 2", cached.Misses())
	}

	// Delete invalidates too.
	if err := cached.Delete(ctx, "a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cached.Get(ctx, "a1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete read = %v, want ErrNotFound", err)
	}
}

func TestConcurrentReadsRaceFree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := NewStore()
	_ = base.Save(ctx, Account{ID: "a1", Name: "Alice", Active: true})
	cached := NewCachedRepository(base)

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := cached.Get(ctx, "a1"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := cached.Hits() + cached.Misses(); got != n {
		t.Errorf("hits+misses = %d, want %d", got, n)
	}
}
```

## Review

The repository is correct when pagination is total and stable and the cache never contradicts the store. The decisive pagination property is in `TestPaginationVisitsEveryRowOnce`: walking the cursors must visit every matching row exactly once and then stop. The boundary cases are where implementations slip — when the result count is an exact multiple of the page size, the last full page must still report "no more" with an empty cursor rather than handing out a cursor that yields an empty page, and an oversize page or an empty filter result must return an empty cursor immediately. Confirm that a malformed cursor is rejected with `ErrBadCursor` instead of being decoded into a garbage key, and that the filter is applied before the slice so the cursor addresses a position within the filtered sequence.

Common mistakes for this feature. The first is building the cursor from an offset instead of a key, which reintroduces the instability cursor pagination exists to remove; resume from the last ID with `sort.Search`, not from a count. The second is the off-by-one at the page boundary: returning a non-empty `NextCursor` when the window is exactly one page long produces a phantom empty final page, so set the cursor only when the window has strictly more than a page. The third is caching `List` results, which serves stale or torn pages as the data changes underneath; cache single-entity `Get`, delegate `List`. The fourth is the stale-cache classic — filling the cache on read but forgetting to invalidate on `Save`/`Delete` — which `TestCacheHitAndInvalidation` catches the moment the post-write read returns the old name. Running `go test -race ./...` confirms the store's `RWMutex` and the cache's mutex actually guard their maps under the concurrent reads.

## Resources

- [Google AIP-158: Pagination](https://google.aip.dev/158) — the API design guidance behind opaque page tokens, why they are not offsets, and how the empty next-token signals the end.
- [Relay Cursor Connections Specification](https://relay.dev/graphql/connections.htm) — the widely adopted cursor-connection model that formalizes opaque cursors and has-next-page semantics.
- [`encoding/base64`](https://pkg.go.dev/encoding/base64) — the encoding that turns the internal key into the opaque token, including the URL-safe `RawURLEncoding` used here.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — the binary search that resumes a page at the first key past the cursor in logarithmic time.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-unit-of-work-optimistic-concurrency.md](04-unit-of-work-optimistic-concurrency.md)
