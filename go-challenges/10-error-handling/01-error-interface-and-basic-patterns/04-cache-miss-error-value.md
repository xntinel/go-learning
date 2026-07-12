# Exercise 4: A Cache-Aside Read Where ErrCacheMiss Drives Control Flow

Not every error is a failure. In the cache-aside pattern a cache miss is the
*normal* case on a cold key, and the code treats it as a signal to fall through to
the backing store rather than as something to report. This exercise builds an
in-memory cache whose `Get` returns a package-level `ErrCacheMiss` sentinel, and a
`ReadThrough` that branches on it — showing that errors are ordinary comparable
values used for flow decisions.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cacheaside/                  independent module: example.com/cacheaside
  go.mod                     go 1.26
  cache.go                   ErrCacheMiss, ErrCacheClosed; Cache Get/Set/Close; ReadThrough
  cmd/
    demo/
      main.go                runnable demo: cold miss loads, warm hit serves
  cache_test.go              miss/hit/loader-failure/closed + identity-of-errors.New test
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a `Cache` with `Get(key) (string, error)` returning `ErrCacheMiss` on absence and `ErrCacheClosed` after `Close`; a `ReadThrough(cache, key, loader)` that treats `ErrCacheMiss` as fall-through, serves hits directly, and propagates any other error unchanged.
- Test: a miss returns `errors.Is(err, ErrCacheMiss)` and triggers exactly one loader call; a hit returns the cached value with nil error and zero loader calls; a loader failure propagates verbatim; a closed cache propagates `ErrCacheClosed` without calling the loader; two `errors.New` with identical text are not `==`.
- Verify: `go test -count=1 -race ./...`

### Errors as flow signals, and why identity matters

`ReadThrough` reads three outcomes from `cache.Get` and does something different
with each. A nil error is a hit: return the value, done. `ErrCacheMiss` is not a
failure at all — it means "cold key", so fall through to the loader, populate the
cache, and return the loaded value. Any *other* error (here `ErrCacheClosed`) is a
real fault the cache-aside layer cannot paper over, so it propagates unchanged.
This three-way branch is the whole point: the sentinel is a control-flow value,
not just something to log.

For the branch to work, `errors.Is(err, ErrCacheMiss)` must reliably recognize the
miss. That reliability rests on identity. `errors.New` returns a distinct
`*errorString` pointer on every call, so two errors with identical text are not
equal — `errors.New("cache miss") != errors.New("cache miss")`. A sentinel works
precisely because it is created *once* as a package-level `var` and every miss
returns that same value. If `Get` built its miss error inline
(`return "", errors.New("cache miss")`), each call would produce a fresh,
unmatchable value and `ReadThrough` would fall into the "other error" branch on
every cold key. The test `TestErrorsNewAreDistinct` pins this property directly so
the reason the sentinel pattern exists is never mistaken for magic.

`Cache` uses a `sync.RWMutex`: reads take the read lock so concurrent hits do not
serialize, writes and `Close` take the write lock. The `ErrCacheClosed` path is
what makes the "propagate other error" branch of `ReadThrough` reachable and
testable — a closed cache must not silently look like an infinite stream of
misses that hammer the loader.

Create `cache.go`:

```go
package cacheaside

import (
	"errors"
	"sync"
)

// ErrCacheMiss signals an absent key. It is a normal, expected outcome that
// callers use to fall through to a backing store, not a failure.
var ErrCacheMiss = errors.New("cache miss")

// ErrCacheClosed is returned by a cache used after Close. Unlike a miss, it is a
// real fault that must propagate rather than trigger a load.
var ErrCacheClosed = errors.New("cache closed")

// Cache is a concurrency-safe in-memory string cache.
type Cache struct {
	mu     sync.RWMutex
	items  map[string]string
	closed bool
}

func NewCache() *Cache {
	return &Cache{items: make(map[string]string)}
}

// Get returns the cached value, or ErrCacheMiss if absent, or ErrCacheClosed if
// the cache has been closed.
func (c *Cache) Get(key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return "", ErrCacheClosed
	}
	v, ok := c.items[key]
	if !ok {
		return "", ErrCacheMiss
	}
	return v, nil
}

// Set stores a value under key.
func (c *Cache) Set(key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrCacheClosed
	}
	c.items[key] = value
	return nil
}

// Close marks the cache unusable; subsequent Get/Set return ErrCacheClosed.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

// ReadThrough serves key from the cache, falling through to loader on a miss and
// caching the loaded value. A cache miss is treated as a normal signal; any other
// error from the cache is propagated unchanged, and a loader error is returned
// verbatim rather than being swallowed as a miss.
func ReadThrough(c *Cache, key string, loader func(string) (string, error)) (string, error) {
	v, err := c.Get(key)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, ErrCacheMiss) {
		return "", err // e.g. ErrCacheClosed: a real fault, do not load
	}

	loaded, lerr := loader(key)
	if lerr != nil {
		return "", lerr
	}
	_ = c.Set(key, loaded)
	return loaded, nil
}
```

### The runnable demo

The demo reads a cold key (miss, loads from a fake store), then the same key again
(hit, no load), counting loader calls to make the fall-through visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cacheaside"
)

func main() {
	c := cacheaside.NewCache()
	store := map[string]string{"user:1": "alice"}
	calls := 0
	loader := func(key string) (string, error) {
		calls++
		return store[key], nil
	}

	v1, _ := cacheaside.ReadThrough(c, "user:1", loader)
	fmt.Printf("cold read: %s (loader calls=%d)\n", v1, calls)

	v2, _ := cacheaside.ReadThrough(c, "user:1", loader)
	fmt.Printf("warm read: %s (loader calls=%d)\n", v2, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cold read: alice (loader calls=1)
warm read: alice (loader calls=1)
```

### Tests

The table pins each branch of `ReadThrough` and the identity property that makes
sentinels work. `TestMissTriggersLoaderOnce` asserts exactly one loader call and a
cached second read; `TestHitSkipsLoader` asserts a warm key never calls the loader;
`TestLoaderFailurePropagates` asserts a loader error surfaces verbatim rather than
being swallowed; `TestClosedCachePropagates` asserts the non-miss error path
returns `ErrCacheClosed` without loading; `TestErrorsNewAreDistinct` proves two
`errors.New` calls with identical text are not `==`.

Create `cache_test.go`:

```go
package cacheaside

import (
	"errors"
	"testing"
)

func TestMissTriggersLoaderOnce(t *testing.T) {
	t.Parallel()

	c := NewCache()
	calls := 0
	loader := func(string) (string, error) { calls++; return "loaded", nil }

	v, err := ReadThrough(c, "k", loader)
	if err != nil || v != "loaded" {
		t.Fatalf("first ReadThrough = %q,%v; want loaded,nil", v, err)
	}
	if calls != 1 {
		t.Fatalf("loader calls = %d, want 1", calls)
	}

	v, err = ReadThrough(c, "k", loader)
	if err != nil || v != "loaded" {
		t.Fatalf("second ReadThrough = %q,%v; want loaded,nil", v, err)
	}
	if calls != 1 {
		t.Fatalf("loader calls after warm read = %d, want 1", calls)
	}
}

func TestHitSkipsLoader(t *testing.T) {
	t.Parallel()

	c := NewCache()
	_ = c.Set("k", "cached")
	loader := func(string) (string, error) {
		t.Fatal("loader must not be called on a cache hit")
		return "", nil
	}

	v, err := ReadThrough(c, "k", loader)
	if err != nil || v != "cached" {
		t.Fatalf("ReadThrough on hit = %q,%v; want cached,nil", v, err)
	}
}

func TestGetReturnsMissSentinel(t *testing.T) {
	t.Parallel()

	c := NewCache()
	_, err := c.Get("absent")
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get(absent) err = %v, want ErrCacheMiss", err)
	}
}

func TestLoaderFailurePropagates(t *testing.T) {
	t.Parallel()

	c := NewCache()
	sentinel := errors.New("db down")
	loader := func(string) (string, error) { return "", sentinel }

	_, err := ReadThrough(c, "k", loader)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReadThrough err = %v, want the loader error", err)
	}
}

func TestClosedCachePropagates(t *testing.T) {
	t.Parallel()

	c := NewCache()
	c.Close()
	loader := func(string) (string, error) {
		t.Fatal("loader must not be called when the cache errors non-miss")
		return "", nil
	}

	_, err := ReadThrough(c, "k", loader)
	if !errors.Is(err, ErrCacheClosed) {
		t.Fatalf("ReadThrough on closed cache err = %v, want ErrCacheClosed", err)
	}
}

// TestErrorsNewAreDistinct proves sentinels must be single shared values: two
// errors.New calls with identical text are distinct pointers, so == is false.
func TestErrorsNewAreDistinct(t *testing.T) {
	t.Parallel()

	a := errors.New("cache miss")
	b := errors.New("cache miss")
	if a == b {
		t.Fatal("two errors.New with identical text compared equal; they must not")
	}
}
```

## Review

`ReadThrough` is correct when its three branches are each exercised: a hit returns
the value with no load, a miss loads exactly once and caches, and a non-miss error
propagates without touching the loader. The `TestClosedCachePropagates` case is
what keeps the "other error" branch honest — without it, a real fault could be
mistaken for an endless stream of misses that hammer the backing store. The
identity test is the conceptual anchor: sentinels compare by pointer identity, so
they must be shared package-level values, never rebuilt inline.

The mistake to avoid is swallowing a loader failure as if it were a miss (retrying
forever or returning empty on a real outage) — propagate it verbatim. The other is
building the miss error at the return site; each call would be a distinct value
`errors.Is` cannot match, and the fall-through would break silently.

## Resources

- [pkg.go.dev: errors.New](https://pkg.go.dev/errors#New) — returns a distinct value each call; the basis of sentinel identity.
- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — matching a sentinel through an unwrap chain.
- [Go Blog: Errors are values](https://go.dev/blog/errors-are-values) — Rob Pike on treating errors as ordinary values to program with.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-typed-nil-interface-trap.md](03-typed-nil-interface-trap.md) | Next: [05-validated-client-constructor.md](05-validated-client-constructor.md)
