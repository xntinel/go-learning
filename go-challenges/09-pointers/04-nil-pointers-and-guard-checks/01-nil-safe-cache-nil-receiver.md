# Exercise 1: A Cache Whose Methods Survive a Nil Receiver

A shared in-process cache is exactly the kind of dependency that is sometimes
nil: a code path that runs before wiring completes, a test that constructs the
service without a cache, a feature flag that disables caching by leaving the
field zero. This module builds a `Cache` whose every public method guards
`if c == nil` first, so a nil `*Cache` degrades to safe defaults instead of
panicking on live traffic.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
nilcache/                 independent module: example.com/nilcache
  go.mod                  go 1.24
  cache.go                type Cache; New, Get, Set, Delete, Len, Has, GetOrError; ErrKeyNotFound
  cmd/
    demo/
      main.go             runnable demo: nil cache degrades, real cache stores
  cache_test.go           table + dedicated nil-safety tests, -race concurrency
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: a `Cache` storing `[]byte` by string key, every public method guarding a nil receiver; `Get`/`Has` return comma-ok, `Set`/`Delete` are no-ops on nil, `Len` returns 0, `GetOrError` returns `ErrKeyNotFound`.
Test: nil `*Cache` never panics and returns safe defaults; the non-nil path stores, retrieves, and deletes; a `-race` concurrency test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/nilcache/cmd/demo
cd ~/go-exercises/nilcache
go mod init example.com/nilcache
go mod edit -go=1.24
```

### Why guard the receiver at all

The alternative to a nil-safe method is a nil check at every call site. In a
service with dozens of cache reads, that is dozens of `if cache != nil` branches,
any one of which can be forgotten — and the one that is forgotten panics under
production load, not in review. Pushing the guard into the method makes the
contract "calling any method on a nil `*Cache` is safe and returns the empty
answer" a property of the type, checked once. This is the null-object pattern
expressed with a nil pointer: the nil `*Cache` *is* the no-op cache.

The critical discipline is that the guard must be the first statement in each
method, before any access to `c.data`. `c.data[key]` is `(*c).data[key]`; it
dereferences `c`, so it must not run until after `if c == nil` has returned. The
internal state (`c.data`) is only ever touched on the non-nil path, so the helper
logic can assume a live map.

Two return conventions carry the "no value" signal without ambiguity. `Get` and
`Has` return comma-ok so a caller can tell "stored a nil/empty value" from "no
entry". `GetOrError` returns the sentinel `ErrKeyNotFound` wrapped-friendly value
so a caller that prefers error handling can `errors.Is` against it. A bare nil
return would collapse both signals.

Create `cache.go`:

```go
package cache

import "errors"

// ErrKeyNotFound is returned by GetOrError when a key is absent (or the cache
// itself is nil). Callers match it with errors.Is.
var ErrKeyNotFound = errors.New("cache: key not found")

// Cache stores byte payloads by string key. A nil *Cache is a valid, safe,
// empty cache: every method guards the receiver before touching internal state.
type Cache struct {
	data map[string][]byte
}

// New returns a ready-to-use cache with an initialized map.
func New() *Cache {
	return &Cache{data: make(map[string][]byte)}
}

// Get returns the stored value and true, or (nil, false) if absent or the cache
// is nil. The comma-ok form distinguishes "no entry" from "empty value".
func (c *Cache) Get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.data[key]
	return v, ok
}

// Set stores value under key. It is a no-op on a nil cache.
func (c *Cache) Set(key string, value []byte) {
	if c == nil {
		return
	}
	c.data[key] = value
}

// Delete removes key. It is a no-op on a nil cache or an absent key.
func (c *Cache) Delete(key string) {
	if c == nil {
		return
	}
	delete(c.data, key)
}

// Len reports the number of stored entries, or 0 on a nil cache.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	return len(c.data)
}

// Has reports whether key is present. It returns false on a nil cache.
func (c *Cache) Has(key string) bool {
	if c == nil {
		return false
	}
	_, ok := c.data[key]
	return ok
}

// GetOrError returns the value, or ErrKeyNotFound if the key is absent or the
// cache is nil.
func (c *Cache) GetOrError(key string) ([]byte, error) {
	if c == nil {
		return nil, ErrKeyNotFound
	}
	v, ok := c.data[key]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return v, nil
}
```

### The runnable demo

The demo shows the payoff directly: a nil cache (a dependency that was never
wired) is used exactly like a real one, degrading to misses instead of crashing,
and then a real cache stores and serves a value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/nilcache"
)

func main() {
	// A dependency that was never constructed: nil, but safe to use.
	var disabled *cache.Cache
	disabled.Set("k", []byte("v")) // no-op, no panic
	if _, ok := disabled.Get("k"); !ok {
		fmt.Printf("nil cache: miss, len=%d\n", disabled.Len())
	}

	c := cache.New()
	c.Set("session:alice", []byte("token-123"))
	if v, ok := c.Get("session:alice"); ok {
		fmt.Printf("real cache: hit %s, len=%d\n", v, c.Len())
	}

	c.Delete("session:alice")
	if _, err := c.GetOrError("session:alice"); errors.Is(err, cache.ErrKeyNotFound) {
		fmt.Println("after delete: key not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nil cache: miss, len=0
real cache: hit token-123, len=1
after delete: key not found
```

### Tests

The tests prove the two contracts: a nil `*Cache` never panics and returns the
safe default for every method (including `Delete`, which the original left as a
follow-up), and the non-nil path stores, retrieves, and deletes correctly. The
concurrency test drives `Set`/`Get` from many goroutines under `-race` to confirm
the map is not touched concurrently in a way the guard hides.

Note: this cache is not internally synchronized (the guard is about nil, not
about locking), so the concurrency test partitions keys per goroutine and only
reads its own — it checks the guard and the map access are race-free per key, not
that the type is safe for concurrent writes to the same key.

Create `cache_test.go`:

```go
package cache

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNilCacheReadsAreSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(c *Cache) (any, bool)
	}{
		{"Get", func(c *Cache) (any, bool) { v, ok := c.Get("k"); return v, ok }},
		{"Has", func(c *Cache) (any, bool) { ok := c.Has("k"); return nil, ok }},
		{"Len", func(c *Cache) (any, bool) { return c.Len(), c.Len() != 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c *Cache // nil
			if _, ok := tt.run(c); ok {
				t.Fatalf("%s on nil cache returned ok/non-zero, want empty", tt.name)
			}
		})
	}
}

func TestNilCacheSetIsNoOp(t *testing.T) {
	t.Parallel()

	var c *Cache
	c.Set("k", []byte("v")) // must not panic
	if c.Has("k") {
		t.Fatal("nil cache stored a value")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

func TestNilCacheDeleteIsNoOp(t *testing.T) {
	t.Parallel()

	var c *Cache
	c.Delete("k") // must not panic
	if c != nil {
		t.Fatal("Delete mutated the nil pointer")
	}
}

func TestNilCacheGetOrError(t *testing.T) {
	t.Parallel()

	var c *Cache
	_, err := c.GetOrError("k")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("err = %v, want ErrKeyNotFound", err)
	}
}

func TestNonNilCacheRoundTrip(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("k1", []byte("v1"))
	c.Set("k2", []byte("v2"))
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	if v, ok := c.Get("k1"); !ok || string(v) != "v1" {
		t.Fatalf("Get(k1) = %q,%v; want v1,true", v, ok)
	}

	c.Delete("k1")
	if c.Has("k1") {
		t.Fatal("Has(k1) = true after delete")
	}
	if _, err := c.GetOrError("k1"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("GetOrError(k1) err = %v, want ErrKeyNotFound", err)
	}
}

func TestConcurrentPerKeyAccess(t *testing.T) {
	t.Parallel()

	c := New()
	// Pre-populate one key per goroutine so each touches only its own.
	for i := range 50 {
		c.Set(fmt.Sprintf("k%d", i), []byte("v"))
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			if _, ok := c.Get(key); !ok {
				t.Errorf("missing %s", key)
			}
		}()
	}
	wg.Wait()
}

func ExampleCache_GetOrError() {
	c := New()
	c.Set("answer", []byte("42"))

	v, _ := c.GetOrError("answer")
	fmt.Printf("%s\n", v)

	_, err := c.GetOrError("missing")
	fmt.Println(err)
	// Output:
	// 42
	// cache: key not found
}
```

## Review

The cache is correct when every public method's first statement is the nil guard
and the guard returns the type's empty answer: `(nil, false)` for `Get`, `false`
for `Has`, `0` for `Len`, a no-op for `Set`/`Delete`, and `ErrKeyNotFound` for
`GetOrError`. The proof is `TestNilCacheReadsAreSafe` and the two no-op tests: if
any guard were placed after a `c.data` access it would panic here rather than
return. `TestNonNilCacheRoundTrip` pins the ordinary path. Run `go test -race` to
confirm the per-key concurrent reads are clean.

The traps this exercise inoculates against: guarding after a dereference (dead
code), and returning a bare nil that collapses "no entry" into "nil value" — both
avoided by the first-statement guard and the comma-ok / sentinel-error return
conventions.

## Resources

- [Go Specification: Pointer types](https://go.dev/ref/spec#Pointer_types) — what nil pointers permit and what dereferencing them does.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — method sets and receiver semantics.
- [Go Blog: Errors are values](https://go.dev/blog/errors-are-values) — why a sentinel error is a value you compare, not a string you match.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-config-defaults-nil-guard.md](02-config-defaults-nil-guard.md)
