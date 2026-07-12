# Exercise 5: Segregate a Cache Into a Getter View for Least-Privilege Consumers

Shared cache state is a classic place a stray write becomes a production
incident: a consumer that should only read a pricing snapshot accidentally
`Set`s a poisoned value and every other reader serves it. This module splits a
`Cache` (Get/Set/Delete/Flush) into a one-method `Getter` and hands the pricing
renderer only the `Getter`, so it is *structurally incapable* of mutating shared
state — least privilege enforced by the type system.

## What you'll build

```text
cacheview/                     independent module: example.com/cacheview
  go.mod                       go 1.24
  cache.go                     Getter (Get) and full Cache; TTL-aware memCache under RWMutex
  render.go                    render(Getter) reads a snapshot; cannot Set/Delete/Flush
  cmd/
    demo/
      main.go                  Set through full cache, render through the Getter view
  cache_test.go                interface satisfaction; expiry miss; same object serves both roles
```

Files: `cache.go`, `render.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: `Getter interface { Get(ctx, key) (string, bool) }`, a full `Cache` interface, and a TTL-aware `memCache` under `sync.RWMutex` satisfying both.
Test: compile-time `var _` checks; `render(Getter)` returns cached values and misses expired entries; a full-`Cache` test Sets then reads through the `Getter`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/06-interface-segregation/05-read-only-cache-view/cmd/demo
cd go-solutions/08-interfaces/06-interface-segregation/05-read-only-cache-view
go mod edit -go=1.24
```

### The Getter is the only handle the renderer holds

The least-privilege guarantee is only as strong as the handle. The renderer's
parameter is typed `Getter`, which has one method, `Get`. It has no way to reach
`Set`, `Delete`, or `Flush` because those methods are not in the type — not
hidden, not discouraged, simply absent. A reviewer does not have to trust that
the renderer "won't" mutate; the compiler makes mutation unrepresentable. The
critical discipline (see the concepts file) is that the narrow type must be the
*only* handle: if the renderer could also reach the concrete `*memCache`, the
segregation would be cosmetic. Here `render` receives a `Getter` and nothing
else.

The cache is TTL-aware to make the read path concrete. `Get` returns
`(value, true)` only when the key is present and its deadline has not passed;
an expired entry returns `("", false)` and is treated as absent (lazy expiry,
like a real read-through cache). The implementation uses `sync.RWMutex` because
reads vastly outnumber writes in a cache: many `Get`s can proceed concurrently
under `RLock`, while `Set`/`Delete` take the exclusive `Lock`. `time.Now` and a
stored `time.Time` deadline drive expiry; `context.Context` is the first
parameter of every method so a cancelled request abandons the lookup.

Create `cache.go`:

```go
package cacheview

import (
	"context"
	"sync"
	"time"
)

// Getter is the read-only view of a cache: exactly one method. A least-privilege
// consumer depends on this and cannot mutate shared state.
type Getter interface {
	Get(ctx context.Context, key string) (string, bool)
}

// Cache is the full surface. Owners that must populate the cache depend on this;
// read-only consumers depend on Getter.
type Cache interface {
	Getter
	Set(ctx context.Context, key, value string, ttl time.Duration)
	Delete(ctx context.Context, key string)
	Flush(ctx context.Context)
}

type item struct {
	value   string
	expires time.Time
}

// memCache is a TTL-aware in-memory cache. Reads take RLock; writes take Lock.
type memCache struct {
	mu    sync.RWMutex
	items map[string]item
}

// NewCache returns an empty *memCache as the concrete type (accept interfaces,
// return structs): callers narrow to Getter or Cache as they need.
func NewCache() *memCache {
	return &memCache{items: make(map[string]item)}
}

func (c *memCache) Get(_ context.Context, key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	it, ok := c.items[key]
	if !ok || !time.Now().Before(it.expires) {
		return "", false
	}
	return it.value, true
}

func (c *memCache) Set(_ context.Context, key, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = item{value: value, expires: time.Now().Add(ttl)}
}

func (c *memCache) Delete(_ context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

func (c *memCache) Flush(_ context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]item)
}

// Compile-time proof one concrete type satisfies both the narrow and full views.
var (
	_ Getter = (*memCache)(nil)
	_ Cache  = (*memCache)(nil)
)
```

Create `render.go`. The renderer takes only a `Getter`:

```go
package cacheview

import (
	"context"
	"fmt"
)

// render builds a pricing line from cached values. Its dependency is Getter, so
// it can read but is structurally incapable of poisoning the shared cache.
func render(ctx context.Context, g Getter, sku string) string {
	price, ok := g.Get(ctx, "price:"+sku)
	if !ok {
		return fmt.Sprintf("%s: price unavailable", sku)
	}
	return fmt.Sprintf("%s: %s", sku, price)
}
```

### The runnable demo

Create `cmd/demo/main.go`. The owner populates through the full cache; the
renderer reads through the narrow view.

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/cacheview"
)

func main() {
	ctx := context.Background()
	c := cacheview.NewCache()

	// Owner writes through the full Cache surface.
	var full cacheview.Cache = c
	full.Set(ctx, "price:WIDGET", "$19.99", time.Minute)

	// Renderer reads through the narrow Getter view only.
	var view cacheview.Getter = c
	fmt.Println(cacheview.Render(ctx, view, "WIDGET"))
	fmt.Println(cacheview.Render(ctx, view, "GADGET"))
}
```

The demo needs an exported entry point; add a thin exported wrapper over the
unexported `render`.

Add to `render.go`:

```go
// Render is the exported entry point over render, for demos and external
// consumers that hold only a Getter.
func Render(ctx context.Context, g Getter, sku string) string {
	return render(ctx, g, sku)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
WIDGET: $19.99
GADGET: price unavailable
```

### Tests

The tests prove the same `*memCache` serves both roles, that reads through the
`Getter` see writes made through the full `Cache`, and that an expired entry
misses. Expiry uses a real short TTL that the test steps past with a tiny sleep;
the deterministic-time technique lives in the synctest chapter, so here a short
real sleep keeps the module self-contained.

Create `cache_test.go`:

```go
package cacheview

import (
	"context"
	"testing"
	"time"
)

func TestRenderReadsThroughGetter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c := NewCache()
	c.Set(ctx, "price:SKU1", "$5.00", time.Minute)

	var view Getter = c
	if got := render(ctx, view, "SKU1"); got != "SKU1: $5.00" {
		t.Fatalf("render = %q, want %q", got, "SKU1: $5.00")
	}
}

func TestRenderMissesUnknownKey(t *testing.T) {
	t.Parallel()

	c := NewCache()
	got := render(context.Background(), c, "NOPE")
	if got != "NOPE: price unavailable" {
		t.Fatalf("render = %q, want unavailable", got)
	}
}

func TestExpiredEntryMisses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c := NewCache()
	c.Set(ctx, "price:FAST", "$1.00", 10*time.Millisecond)

	if _, ok := c.Get(ctx, "price:FAST"); !ok {
		t.Fatal("entry should be live immediately after Set")
	}

	time.Sleep(25 * time.Millisecond)

	if _, ok := c.Get(ctx, "price:FAST"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestSameObjectServesBothRoles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c := NewCache()

	// Write through the full Cache role.
	var full Cache = c
	full.Set(ctx, "price:DUAL", "$9.00", time.Minute)

	// Read the same object through the Getter role.
	var view Getter = c
	if v, ok := view.Get(ctx, "price:DUAL"); !ok || v != "$9.00" {
		t.Fatalf("Get via Getter = %q,%v; want $9.00,true", v, ok)
	}
}

func TestFlushClears(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c := NewCache()
	c.Set(ctx, "a", "1", time.Minute)
	c.Flush(ctx)
	if _, ok := c.Get(ctx, "a"); ok {
		t.Fatal("Flush should have cleared the entry")
	}
}
```

## Review

The segregation is correct when the renderer holds a `Getter` and only a
`Getter`: `render`'s signature makes `Set`/`Delete`/`Flush` unreachable, so a
reader poisoning the cache is a compile error, not a review comment. The two
compile-time `var _` assertions pin that the one `*memCache` satisfies both the
narrow and full views. The subtle failure mode is leaking the concrete pointer
past the narrow handle — if `render` could reach `*memCache`, the guarantee
evaporates; the narrow type must be the only handle it holds. The `RWMutex`
choice reflects a real read-heavy cache; run `go test -race` to confirm
concurrent `Get`/`Set` are properly synchronized.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [time package (Now, Time.Add, Time.Before)](https://pkg.go.dev/time)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- [Interface Segregation Principle](https://en.wikipedia.org/wiki/Interface_segregation_principle)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-reader-writer-streaming-pipeline.md](04-reader-writer-streaming-pipeline.md) | Next: [06-narrow-port-for-test-doubles.md](06-narrow-port-for-test-doubles.md)
