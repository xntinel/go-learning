# Exercise 3: Decorating A Repository With Caching And Logging

Caching, logging, metrics, and retries are cross-cutting concerns that every repository wants and none of them belong in the storage code. Because a repository is an interface, you can add them from the outside: a decorator is a type that both implements the interface and holds another implementation of it, delegating each call while adding behavior around the delegation. This exercise builds a generic in-memory repository and two decorators — logging and caching — that stack on top of it without either layer knowing the other exists.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
repo.go                  Repository[T] interface, MemoryRepository[T] base,
                         LoggingRepository[T] decorator, CachingRepository[T]
                         decorator with hit/miss counters and invalidation
cmd/
  demo/
    main.go              stack Logging(Caching(Memory)) and show a cache hit
repo_test.go             base contract, caching hit/miss + invalidation, logging output
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: the generic `Repository[T]` interface, `MemoryRepository[T]`, `LoggingRepository[T]`, and `CachingRepository[T]` with `Hits`/`Misses` accessors.
- Test: the base contract, that caching serves hits and invalidates on write, and that logging records every call.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p decorator-cache-and-logging/cmd/demo && cd decorator-cache-and-logging
go mod init example.com/decorator-cache-and-logging
```

### Why a decorator instead of editing the storage code

The wrong way to add a cache is to reach into the storage implementation and sprinkle a map and a logger through every method. That bloats the storage code with concerns it should not own, forces a second implementation (a SQL one, say) to reimplement the same plumbing, and makes the cache impossible to remove or reorder without editing storage. The decorator avoids all of it by exploiting the interface: a `CachingRepository[T]` holds a `Repository[T]` (the inner one), implements `Repository[T]` itself, and on each call decides whether to answer locally or delegate.

The interface here is generic — `Repository[T]` with `Get`, `Put`, and `Delete` — so the same decorators wrap a repository of any element type. Type parameters live on the interface and the struct, not on the methods (Go does not allow type parameters on methods), which is exactly why the type variable is declared once at `type Repository[T any] interface` and reused by every method signature.

`LoggingRepository` is the trivial decorator: it logs the operation, then forwards to the inner repository and returns whatever the inner one returns. It adds an observable side effect and changes nothing about the result. `CachingRepository` is the instructive one, because it is the decorator that must stay consistent with the data underneath it. On `Get` it checks its cache: a hit returns immediately and never touches the inner repository (the whole point), a miss delegates, stores the successful result, and counts the miss. The half that is easy to forget is invalidation: `Put` and `Delete` change what the correct answer is, so each must evict the cached entry before returning, or a subsequent `Get` would serve a value that has been overwritten or deleted. A caching decorator that populates on read but never invalidates on write is the classic stale-cache bug, and the test pins it directly.

Because each decorator satisfies the same interface as what it wraps, they compose by nesting at construction time: `NewLoggingRepository(NewCachingRepository(NewMemoryRepository[T]()), logger)` is a `Repository[T]` where every call is logged, reads are served from cache, and storage is reached only on a miss. The order is a real decision made in this one line: logging outermost (as here) logs every call including cache hits; logging innermost would log only the calls that fall through to storage. No layer hard-codes the order; the composition root chooses it.

Create `repo.go`:

```go
package repo

import (
	"context"
	"errors"
	"log"
	"sync"
)

// ErrNotFound is the domain sentinel for a missing key.
var ErrNotFound = errors.New("repo: not found")

// Repository is the generic collection contract. Decorators implement this same
// interface and hold an inner Repository[T].
type Repository[T any] interface {
	Get(ctx context.Context, id string) (T, error)
	Put(ctx context.Context, id string, v T) error
	Delete(ctx context.Context, id string) error
}

// MemoryRepository is the base in-memory Repository[T].
type MemoryRepository[T any] struct {
	mu   sync.RWMutex
	data map[string]T
}

// NewMemoryRepository returns a ready-to-use in-memory repository.
func NewMemoryRepository[T any]() *MemoryRepository[T] {
	return &MemoryRepository[T]{data: make(map[string]T)}
}

func (r *MemoryRepository[T]) Get(ctx context.Context, id string) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.data[id]
	if !ok {
		return zero, ErrNotFound
	}
	return v, nil
}

func (r *MemoryRepository[T]) Put(ctx context.Context, id string, v T) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[id] = v
	return nil
}

func (r *MemoryRepository[T]) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.data[id]; !ok {
		return ErrNotFound
	}
	delete(r.data, id)
	return nil
}

// LoggingRepository logs every call, then delegates to the inner repository.
type LoggingRepository[T any] struct {
	next   Repository[T]
	logger *log.Logger
}

// NewLoggingRepository wraps next so each call is logged to logger.
func NewLoggingRepository[T any](next Repository[T], logger *log.Logger) *LoggingRepository[T] {
	return &LoggingRepository[T]{next: next, logger: logger}
}

func (r *LoggingRepository[T]) Get(ctx context.Context, id string) (T, error) {
	r.logger.Printf("GET %s", id)
	return r.next.Get(ctx, id)
}

func (r *LoggingRepository[T]) Put(ctx context.Context, id string, v T) error {
	r.logger.Printf("PUT %s", id)
	return r.next.Put(ctx, id, v)
}

func (r *LoggingRepository[T]) Delete(ctx context.Context, id string) error {
	r.logger.Printf("DELETE %s", id)
	return r.next.Delete(ctx, id)
}

// CachingRepository serves Get from an in-memory cache, falling through to the
// inner repository on a miss and invalidating the cache on every write.
type CachingRepository[T any] struct {
	next   Repository[T]
	mu     sync.Mutex
	cache  map[string]T
	hits   int
	misses int
}

// NewCachingRepository wraps next with a read-through cache.
func NewCachingRepository[T any](next Repository[T]) *CachingRepository[T] {
	return &CachingRepository[T]{next: next, cache: make(map[string]T)}
}

func (r *CachingRepository[T]) Get(ctx context.Context, id string) (T, error) {
	r.mu.Lock()
	if v, ok := r.cache[id]; ok {
		r.hits++
		r.mu.Unlock()
		return v, nil
	}
	r.misses++
	r.mu.Unlock()

	v, err := r.next.Get(ctx, id)
	if err != nil {
		return v, err
	}

	r.mu.Lock()
	r.cache[id] = v
	r.mu.Unlock()
	return v, nil
}

func (r *CachingRepository[T]) Put(ctx context.Context, id string, v T) error {
	if err := r.next.Put(ctx, id, v); err != nil {
		return err
	}
	r.invalidate(id)
	return nil
}

func (r *CachingRepository[T]) Delete(ctx context.Context, id string) error {
	if err := r.next.Delete(ctx, id); err != nil {
		return err
	}
	r.invalidate(id)
	return nil
}

func (r *CachingRepository[T]) invalidate(id string) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// Hits reports cache hits served without touching the inner repository.
func (r *CachingRepository[T]) Hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

// Misses reports cache misses that fell through to the inner repository.
func (r *CachingRepository[T]) Misses() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.misses
}
```

Each decorator's constructor returns a concrete pointer type, but every field that holds an inner repository is typed as the `Repository[T]` interface, so any of the three types can be nested inside any other. The caching decorator invalidates after a successful inner write — never before — so a failed `Put` leaves the cache untouched and a successful one evicts the now-stale entry; the next `Get` re-reads the fresh value from storage and re-caches it.

### The runnable demo

The demo builds the full stack — `Logging(Caching(Memory))` — and runs one write and two reads of the same key. The first read is a cache miss that reaches storage; the second is a hit served from the cache. Because logging is the outermost layer, every call is logged including the hit, and the hit/miss counters from the caching layer prove storage was reached only once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"example.com/decorator-cache-and-logging"
)

func main() {
	logger := log.New(os.Stdout, "", 0)

	base := repo.NewMemoryRepository[string]()
	cached := repo.NewCachingRepository[string](base)
	logged := repo.NewLoggingRepository[string](cached, logger)

	ctx := context.Background()

	_ = logged.Put(ctx, "1", "Alice")
	// First read misses and falls through to storage.
	_, _ = logged.Get(ctx, "1")
	// Second read is served from the cache.
	v, _ := logged.Get(ctx, "1")

	fmt.Printf("value=%s\n", v)
	fmt.Printf("hits=%d misses=%d\n", cached.Hits(), cached.Misses())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PUT 1
GET 1
GET 1
value=Alice
hits=1 misses=1
```

The two `GET 1` lines come from the logging layer logging both reads; the `hits=1 misses=1` line proves the caching layer reached storage only on the first.

### Tests

`TestMemoryRepository_Contract` checks the base storage in isolation. `TestCachingHitMissAndInvalidation` is the core test: it confirms a second read is a hit, that a `Put` invalidates so the next read sees the new value and counts as a fresh miss, and that `Delete` invalidates too. `TestLoggingRecordsCalls` captures the log output in a buffer and asserts every operation was recorded, which proves the decorator forwards while observing.

Create `repo_test.go`:

```go
package repo

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
)

var (
	_ Repository[string] = (*MemoryRepository[string])(nil)
	_ Repository[string] = (*LoggingRepository[string])(nil)
	_ Repository[string] = (*CachingRepository[string])(nil)
)

func TestMemoryRepository_Contract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := NewMemoryRepository[string]()

	if _, err := r.Get(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
	if err := r.Put(ctx, "x", "v"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := r.Get(ctx, "x")
	if err != nil || got != "v" {
		t.Fatalf("Get = %q, %v; want v, nil", got, err)
	}
	if err := r.Delete(ctx, "x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
}

func TestCachingHitMissAndInvalidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := NewMemoryRepository[string]()
	cached := NewCachingRepository[string](base)

	if err := base.Put(ctx, "1", "Alice"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// First read: miss, fills cache.
	if v, _ := cached.Get(ctx, "1"); v != "Alice" {
		t.Fatalf("read 1 = %q, want Alice", v)
	}
	// Second read: hit, no storage access.
	if v, _ := cached.Get(ctx, "1"); v != "Alice" {
		t.Fatalf("read 2 = %q, want Alice", v)
	}
	if cached.Hits() != 1 || cached.Misses() != 1 {
		t.Fatalf("hits=%d misses=%d, want 1 and 1", cached.Hits(), cached.Misses())
	}

	// Write through the cache: must invalidate the stale entry.
	if err := cached.Put(ctx, "1", "Alice Smith"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, err := cached.Get(ctx, "1")
	if err != nil || v != "Alice Smith" {
		t.Fatalf("post-write read = %q, %v; want Alice Smith", v, err)
	}
	if cached.Misses() != 2 {
		t.Errorf("misses after invalidation = %d, want 2", cached.Misses())
	}

	// Delete invalidates and the inner repo no longer has it.
	if err := cached.Delete(ctx, "1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cached.Get(ctx, "1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete read = %v, want ErrNotFound", err)
	}
}

func TestLoggingRecordsCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	logged := NewLoggingRepository[string](NewMemoryRepository[string](), logger)

	_ = logged.Put(ctx, "1", "Alice")
	_, _ = logged.Get(ctx, "1")
	_ = logged.Delete(ctx, "1")

	out := buf.String()
	for _, want := range []string{"PUT 1", "GET 1", "DELETE 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}
}
```

## Review

The decorators are correct when each one satisfies `Repository[T]` and the caching layer never lets the cache disagree with storage. The compile-time `var _ Repository[string] = ...` assertions pin the interface conformance. The decisive test is the invalidation case: after `Put` overwrites a cached key, the next `Get` must return the new value and register as a miss, proving the stale entry was evicted rather than served. Confirm that a cache hit returns without calling the inner repository (the hit counter rising while the miss counter holds), and that invalidation happens after a successful inner write so a failed write cannot evict a still-valid entry.

Common mistakes for this feature. The first and most damaging is a caching decorator that fills on `Get` but forgets to invalidate on `Put`/`Delete`; it serves a value that has since changed, and the invalidation test catches it the moment the post-write read returns the old string. The second is trying to put a type parameter on a method (`func (r *Caching) Get[T]...`), which the language rejects — the parameter belongs on the type. The third is invalidating before the inner write succeeds, which evicts a good entry on a write that then fails, costing a needless miss; invalidate only after the inner call returns nil. Running under `go test -race ./...` confirms the cache map and the counters are properly guarded, since the whole point of a shared cache is concurrent access.

## Resources

- [Refactoring Guru: Decorator](https://refactoring.guru/design-patterns/decorator) — the structural pattern of wrapping an object in another that shares its interface and adds behavior.
- [Go Blog: Why generics](https://go.dev/blog/why-generics) — the motivation and rules for the type parameters that make `Repository[T]` and its decorators reusable across element types.
- [`log.New`](https://pkg.go.dev/log#New) — constructing the `*log.Logger` the logging decorator writes to, with flags controlling the line prefix.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-specification-queries.md](02-specification-queries.md) | Next: [04-unit-of-work-optimistic-concurrency.md](04-unit-of-work-optimistic-concurrency.md)
