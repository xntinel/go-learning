# Exercise 4: Wrap A Repository With A Read-Through Cache Decorator

A decorator is a type that both accepts an interface and satisfies it, so it can
wrap any implementation transparently. This module builds a `CachingRepository` that
holds a `Repository`, adds a read-through cache, and is itself a `Repository` — the
first layer of the decorator stack the chapter assembles.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
cachedecorator/             independent module: example.com/cachedecorator
  go.mod                    go 1.26
  repo.go                   Item, ErrNotFound; Repository iface; MemoryRepository
  caching.go                CachingRepository: accepts Repository, is a Repository
  cmd/
    demo/
      main.go               shows a cache hit avoiding a second backend read
  caching_test.go           counting fake proves hit-once; Put invalidates; miss not cached
```

Files: `repo.go`, `caching.go`, `cmd/demo/main.go`, `caching_test.go`.
Implement: a `CachingRepository` wrapping a `Repository`, caching successful `Get` results, invalidating on `Put`/`Delete`, and never caching `ErrNotFound` as a positive entry.
Test: a counting fake repo; two `Get`s of one id hit the backend once; a `Put` updates/invalidates the cached entry; `ErrNotFound` is not cached; `go test -race` on concurrent `Get`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/08-accept-interfaces-return-structs/04-caching-decorator/cmd/demo
cd go-solutions/08-interfaces/08-accept-interfaces-return-structs/04-caching-decorator
go mod edit -go=1.26
```

### Why the decorator is transparent

`CachingRepository` accepts a `Repository` (so it can wrap the real store, a fake, or
another decorator) and satisfies `Repository` (so anything that depended on the store
now depends on the cache with no change). That dual property — accept the interface,
be the interface — is what lets caching drop into a call chain silently. A service
built with `NewService(cache)` behaves exactly like one built with `NewService(store)`,
except reads are served from memory after the first miss.

Two correctness rules matter more than the caching itself. First, only *successful*
reads are cached. If `Get` returns `ErrNotFound`, the decorator must propagate it and
must not store a "this key is absent" positive entry — otherwise a later `Put` of that
key would be masked forever by the cached miss. This module caches only the found
value, so a subsequent `Put` seeds the entry and the next `Get` sees it. Second, any
write must keep the cache coherent: `Put` refreshes the cached value for that key, and
`Delete` evicts it. A decorator that cached reads but ignored writes would serve stale
data after the first mutation — the classic broken cache.

The cache has its own `sync.RWMutex`, independent of the wrapped store's lock:
`get`/`put`/`evict` on the cache map are short critical sections, and the backend call
happens outside the cache lock so a slow backend does not serialize cache readers.

Create `repo.go`:

```go
package cachedecorator

import (
	"errors"
	"sync"
)

var ErrNotFound = errors.New("cachedecorator: item not found")

type Item struct {
	ID    string
	Name  string
	Price int64
}

type Repository interface {
	Get(id string) (Item, error)
	Put(item Item) error
	Delete(id string) error
}

type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]Item
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]Item)}
}

var _ Repository = (*MemoryRepository)(nil)

func (m *MemoryRepository) Get(id string) (Item, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (m *MemoryRepository) Put(item Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.ID] = item
	return nil
}

func (m *MemoryRepository) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrNotFound
	}
	delete(m.items, id)
	return nil
}
```

Create `caching.go`:

```go
package cachedecorator

import (
	"errors"
	"sync"
)

// CachingRepository is a read-through cache in front of any Repository. It accepts
// a Repository and is a Repository, so it composes transparently.
type CachingRepository struct {
	next  Repository
	mu    sync.RWMutex
	cache map[string]Item
}

// NewCachingRepository wraps next and returns the concrete decorator.
func NewCachingRepository(next Repository) *CachingRepository {
	return &CachingRepository{next: next, cache: make(map[string]Item)}
}

var _ Repository = (*CachingRepository)(nil)

// Get serves from cache on a hit; on a miss it reads through to next and caches only
// a successful result. ErrNotFound is propagated, never cached as a positive entry.
func (c *CachingRepository) Get(id string) (Item, error) {
	c.mu.RLock()
	item, ok := c.cache[id]
	c.mu.RUnlock()
	if ok {
		return item, nil
	}

	item, err := c.next.Get(id)
	if err != nil {
		// Do NOT cache a miss as a positive entry: a later Put must be visible.
		return Item{}, err
	}

	c.mu.Lock()
	c.cache[id] = item
	c.mu.Unlock()
	return item, nil
}

// Put writes through to next and refreshes the cached value so reads stay coherent.
func (c *CachingRepository) Put(item Item) error {
	if err := c.next.Put(item); err != nil {
		return err
	}
	c.mu.Lock()
	c.cache[item.ID] = item
	c.mu.Unlock()
	return nil
}

// Delete writes through to next and evicts the cached entry. A not-found delete
// still evicts (harmless) and returns the underlying error.
func (c *CachingRepository) Delete(id string) error {
	err := c.next.Delete(id)
	c.mu.Lock()
	delete(c.cache, id)
	c.mu.Unlock()
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return err
}
```

### The runnable demo

The demo wraps a `MemoryRepository` in a `CachingRepository` and reads the same id
twice; a counting hook on the backend is not available to the demo (it only sees the
exported API), so the demo simply shows both reads returning the cached value while
narrating the intent. The test is where the hit-once count is asserted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cachedecorator"
)

func main() {
	store := cachedecorator.NewMemoryRepository()
	_ = store.Put(cachedecorator.Item{ID: "sku-1", Name: "widget", Price: 1299})

	cached := cachedecorator.NewCachingRepository(store)

	first, _ := cached.Get("sku-1")
	fmt.Printf("first read (backend): %s\n", first.Name)

	second, _ := cached.Get("sku-1")
	fmt.Printf("second read (cache):  %s\n", second.Name)

	// A write refreshes the cache so the next read is coherent.
	_ = cached.Put(cachedecorator.Item{ID: "sku-1", Name: "widget-v2", Price: 1399})
	third, _ := cached.Get("sku-1")
	fmt.Printf("after put (cache):    %s\n", third.Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first read (backend): widget
second read (cache):  widget
after put (cache):    widget-v2
```

### Tests

`countingRepo` wraps a map and counts every `Get` that reaches it. The central test
reads one id twice and asserts the backend saw exactly one `Get` — proof the second
read was served from cache. Further tests prove a `Put` updates the cached value, a
`Delete` evicts it, and a miss (`ErrNotFound`) is not cached as a positive entry.

Create `caching_test.go`:

```go
package cachedecorator

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// countingRepo is a Repository that counts backend Get calls, to prove cache hits.
type countingRepo struct {
	mu    sync.Mutex
	items map[string]Item
	gets  atomic.Int64
}

func newCountingRepo() *countingRepo {
	return &countingRepo{items: make(map[string]Item)}
}

var _ Repository = (*countingRepo)(nil)

func (r *countingRepo) Get(id string) (Item, error) {
	r.gets.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (r *countingRepo) Put(item Item) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[item.ID] = item
	return nil
}

func (r *countingRepo) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func TestCacheHitReadsBackendOnce(t *testing.T) {
	t.Parallel()
	backend := newCountingRepo()
	_ = backend.Put(Item{ID: "sku-1", Name: "widget", Price: 1299})
	cached := NewCachingRepository(backend)

	for range 3 {
		got, err := cached.Get("sku-1")
		if err != nil || got.Name != "widget" {
			t.Fatalf("Get = %+v, %v", got, err)
		}
	}
	if n := backend.gets.Load(); n != 1 {
		t.Fatalf("backend Get called %d times, want 1 (cache hit)", n)
	}
}

func TestPutUpdatesCachedEntry(t *testing.T) {
	t.Parallel()
	backend := newCountingRepo()
	_ = backend.Put(Item{ID: "sku-1", Name: "widget", Price: 1299})
	cached := NewCachingRepository(backend)

	_, _ = cached.Get("sku-1") // seed cache
	if err := cached.Put(Item{ID: "sku-1", Name: "widget-v2", Price: 1399}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _ := cached.Get("sku-1")
	if got.Name != "widget-v2" {
		t.Fatalf("after Put, Get = %q, want widget-v2 (stale cache)", got.Name)
	}
}

func TestDeleteEvictsCachedEntry(t *testing.T) {
	t.Parallel()
	backend := newCountingRepo()
	_ = backend.Put(Item{ID: "sku-1", Name: "widget"})
	cached := NewCachingRepository(backend)

	_, _ = cached.Get("sku-1") // seed cache
	if err := cached.Delete("sku-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cached.Get("sku-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound (stale cache)", err)
	}
}

func TestMissIsNotCachedAsPositive(t *testing.T) {
	t.Parallel()
	backend := newCountingRepo()
	cached := NewCachingRepository(backend)

	if _, err := cached.Get("later"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(later) err = %v, want ErrNotFound", err)
	}
	// A Put must now be visible: the earlier miss must not have been cached.
	_ = cached.Put(Item{ID: "later", Name: "arrived"})
	got, err := cached.Get("later")
	if err != nil || got.Name != "arrived" {
		t.Fatalf("Get(later) = %+v, %v; miss was wrongly cached", got, err)
	}
}

func TestConcurrentGet(t *testing.T) {
	t.Parallel()
	backend := newCountingRepo()
	_ = backend.Put(Item{ID: "sku-1", Name: "widget"})
	cached := NewCachingRepository(backend)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cached.Get("sku-1")
		}()
	}
	wg.Wait()
}
```

## Review

The decorator is correct when a repeated `Get` reaches the backend once (asserted via
the counting fake), when a `Put` refreshes and a `Delete` evicts the cached entry so
reads never go stale, and when a miss is propagated but never cached as a positive
entry so a later `Put` becomes visible. Those are the three ways a cache decorator
silently breaks: never invalidating (stale reads), caching negatives (a resurrected
key stays "absent"), and racing on the map (caught by `-race`). Because
`CachingRepository` accepts `Repository` and is a `Repository`, none of this touches
any call site — the cache is inserted purely at construction, which is the entire
argument for the accept-interface / return-struct rule.

## Resources

- [Decorator pattern](https://refactoring.guru/design-patterns/decorator) — the structural pattern this implements, with Go-applicable diagrams.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the cache map.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — `atomic.Int64`, used by the counting fake to tally backend calls race-free.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-narrow-consumer-interface.md](03-narrow-consumer-interface.md) | Next: [05-retry-decorator.md](05-retry-decorator.md)
