# Exercise 8: Repository Page That Serves From Cache Without Leaking the Cache Array

An in-memory repository holds a master `[]Item` and exposes a `Page(offset, limit)`
method an HTTP handler calls. Returning `cache[lo:hi]` hands the handler a view into
the repository's own array: the handler can mutate cached items or `append` into the
cache's backing storage, corrupting shared state under concurrent reads. This
exercise returns an isolated page (three-index full-slice expression plus a clone)
and proves it holds under concurrent readers and a writer.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod                   go 1.24
  repo.go                  type Item, Repo; New; Page; Set; Append; Len
  cmd/
    demo/
      main.go              runnable demo: page, mutate, append, master intact
  repo_test.go             isolation test, cap==len test, concurrent readers+writer
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `Page` returning an isolated copy of the requested window under
  `RWMutex`; `Set`/`Append` mutating the master under a write lock.
- Test: mutate a returned element and `append` to the returned page, assert the
  master is unchanged in element and length; the page's `cap == len`; many
  concurrent `Page` reads while a writer mutates stay `-race` clean and internally
  consistent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Why the page must be an owned copy

The repository owns `items`. If `Page` returned `items[lo:hi]`, the handler would
receive a window welded to the repository's array. Three separate hazards follow.
First, the handler can write `page[0].Name = ...` and silently mutate the cached
item every other request sees. Second, the handler can `append` to the page; because
the two-index window inherits the cache's spare capacity, that `append` overwrites
the *next* cached item in place. Third — and this is what makes it a production
incident rather than a bug in a unit test — all of this happens while other
goroutines are reading the cache, so the corruption races with concurrent reads and
manifests intermittently under load.

The fix is to return an isolated copy. `slices.Clone(items[lo:hi:hi])` does two
things: the three-index full-slice expression bounds the intermediate window's
capacity to its length (documenting the intent that no spare capacity escapes), and
`Clone` copies the elements into a fresh array the handler owns (`cap == len`). The
handler can now mutate and append freely; nothing reaches back into the cache. Since
`Item` here is a value type, the clone also detaches the item contents; if `Item`
held a pointer or slice field, isolating those would require a deeper copy, but the
top-level slice is what a paging handler mutates.

Concurrency is the other half. `Page` takes a read lock so many handlers page in
parallel; `Set` and `Append` take the write lock. The clone happens *inside* the
read lock, so the snapshot is consistent, and the returned copy is safe to use after
the lock is released precisely because it shares nothing with the cache.

Create `repo.go`:

```go
package repo

import (
	"slices"
	"sync"
)

// Item is a cached record. Name is derived from ID (item-<ID>), an invariant the
// concurrency test checks to detect torn reads.
type Item struct {
	ID   int
	Name string
}

// Repo is an in-memory repository guarded by an RWMutex.
type Repo struct {
	mu    sync.RWMutex
	items []Item
}

// New builds a Repo owning a copy of items.
func New(items []Item) *Repo {
	return &Repo{items: slices.Clone(items)}
}

// Page returns an isolated copy of the [offset, offset+limit) window, clamped to
// the cache bounds. The caller may mutate or append to the result without ever
// touching the cache.
func (r *Repo) Page(offset, limit int) []Item {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if offset < 0 {
		offset = 0
	}
	if offset > len(r.items) {
		offset = len(r.items)
	}
	hi := offset + limit
	if limit < 0 || hi < offset {
		hi = offset
	}
	if hi > len(r.items) {
		hi = len(r.items)
	}
	// Three-index expression bounds capacity to length; Clone detaches into a
	// fresh, right-sized array (cap == len).
	return slices.Clone(r.items[offset:hi:hi])
}

// Set replaces the item at index i under the write lock.
func (r *Repo) Set(i int, it Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[i] = it
}

// Append adds an item under the write lock.
func (r *Repo) Append(it Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, it)
}

// Len reports the number of cached items.
func (r *Repo) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

func main() {
	seed := []repo.Item{
		{ID: 0, Name: "item-0"},
		{ID: 1, Name: "item-1"},
		{ID: 2, Name: "item-2"},
		{ID: 3, Name: "item-3"},
	}
	r := repo.New(seed)

	page := r.Page(1, 2)
	fmt.Printf("page: %v (len=%d cap=%d)\n", page, len(page), cap(page))

	// Mutate and append to the page: the cache must be untouched.
	page[0].Name = "HACKED"
	page = append(page, repo.Item{ID: 99, Name: "injected"})

	after := r.Page(1, 2)
	fmt.Printf("cache after handler abuse: %v (len=%d)\n", after, r.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page: [{1 item-1} {2 item-2}] (len=2 cap=2)
cache after handler abuse: [{1 item-1} {2 item-2}] (len=4)
```

## Tests

The isolation test mutates and appends to a returned page and asserts the master is
unchanged in both element content and length. The cap test pins `cap == len` on the
page. The concurrency test runs many `Page` readers alongside a writer that mutates
under the lock, asserting `-race` cleanliness and that every paged item satisfies the
`item-<ID>` invariant (a torn read would break it).

Create `repo_test.go`:

```go
package repo

import (
	"fmt"
	"sync"
	"testing"
)

func seedItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i, Name: fmt.Sprintf("item-%d", i)}
	}
	return items
}

func TestPageIsIsolated(t *testing.T) {
	t.Parallel()
	r := New(seedItems(5))

	page := r.Page(1, 2)
	page[0].Name = "mutated"
	page = append(page, Item{ID: 99, Name: "extra"})

	master := r.Page(1, 2)
	if master[0].Name != "item-1" {
		t.Fatalf("element mutation leaked into cache: %q", master[0].Name)
	}
	if r.Len() != 5 {
		t.Fatalf("append leaked into cache: len = %d, want 5", r.Len())
	}
}

func TestPageCapEqualsLen(t *testing.T) {
	t.Parallel()
	r := New(seedItems(10))
	page := r.Page(2, 3)
	if len(page) != 3 || cap(page) != 3 {
		t.Fatalf("page len=%d cap=%d; want 3,3", len(page), cap(page))
	}
}

func TestPageClamps(t *testing.T) {
	t.Parallel()
	r := New(seedItems(3))
	tests := []struct {
		offset, limit, wantLen int
	}{
		{0, 10, 3},
		{2, 10, 1},
		{3, 5, 0},
		{-1, 2, 2},
		{1, -1, 0},
	}
	for _, tc := range tests {
		page := r.Page(tc.offset, tc.limit)
		if page == nil {
			t.Fatalf("Page(%d,%d) = nil; want non-nil", tc.offset, tc.limit)
		}
		if len(page) != tc.wantLen {
			t.Fatalf("Page(%d,%d) len = %d; want %d", tc.offset, tc.limit, len(page), tc.wantLen)
		}
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	r := New(seedItems(100))

	var wg sync.WaitGroup

	// Writer: rewrites items, preserving the item-<ID> invariant, under the lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := range 200 {
			i := round % 100
			r.Set(i, Item{ID: i, Name: fmt.Sprintf("item-%d", i)})
		}
	}()

	// Readers: page concurrently and verify each item's invariant.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				page := r.Page(10, 20)
				for _, it := range page {
					if it.Name != fmt.Sprintf("item-%d", it.ID) {
						t.Errorf("torn read: %+v", it)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
}
```

## Review

The repository is correct when a page reflects a consistent snapshot at read time and
shares nothing with the cache afterward. The isolation test proves element and length
independence; the cap test proves no spare capacity escaped; the concurrency test
proves reads and writes do not race and no reader sees a torn item. The wrong version
returns `items[lo:hi]` and passes a single-threaded happy-path test, then corrupts the
cache the first time a handler appends to a page or two requests overlap. The rule:
data served out of shared, mutable, concurrently-read state must be copied at the
boundary, and the copy must happen inside the same lock that guarantees the snapshot.
Run `go test -race`; the concurrency test exists specifically to fail without the
locking and the clone.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex)
- [Go Specification: Slice expressions (full slice expression)](https://go.dev/ref/spec#Slice_expressions)
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-subslice-memory-leak-truncation.md](07-subslice-memory-leak-truncation.md) | Next: [09-ordered-schedule-insert-delete.md](09-ordered-schedule-insert-delete.md)
