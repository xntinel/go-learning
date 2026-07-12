# Exercise 9: When a Type Parameter Beats an Interface

Sometimes the only reason an interface (or `any`) exists is to be generic over
types — a container that holds "some value." A `map[string]any` cache boxes every
value and forces a runtime type assertion at every `Get`, turning a type error
into a panic. This module replaces it with a generic `Cache[V any]` that returns
the concrete value type with compile-time safety, no assertion, and no boxing.

## What you'll build

```text
tscache/                    independent module: example.com/tscache
  go.mod                    go 1.26
  anycache.go               AnyCache (map[string]any) -- the boxed, panic-prone version
  cache.go                  generic Cache[V any]: Set/Get/Delete/Sweep, injectable clock
  cmd/
    demo/
      main.go               typed Get with no assertion; deterministic TTL expiry
  cache_test.go             typed value; TTL via fake clock; any-cache panic (recovered)
```

- Files: `anycache.go`, `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a generic `Cache[V any]` with `Set(key, value, ttl)`, `Get(key) (V, bool)`, `Delete(key)`, and a TTL `Sweep`, with an injectable `now func() time.Time`; and an `AnyCache` built on `map[string]any` for contrast.
- Test: instantiate `Cache[User]` and `Cache[int]`, assert `Get` returns the concrete value with the `(value, ok)` idiom and no assertion; TTL expiry with a fake clock; a test that the `any`-based version panics on a wrong assertion.
- Verify: `go test -count=1 -race ./...`

### Why a type parameter, not an interface

Before Go had generics, a reusable container held `any` (formerly `interface{}`)
and callers asserted the type back out at every read: `v, _ := c.Get("u1"); u := v.(User)`.
That assertion is a runtime gamble. Store a `User` under a key, read it later
expecting an `int`, and the program panics — a type error that the compiler could
have caught is deferred to production. The `any` container is an interface used
purely to erase the value's type, and erasing the type is exactly the mistake:
you throw away the information the compiler needs to protect you, then pay for it
with an assertion and a panic risk at every call site.

A type parameter keeps the type. `Cache[V any]` is instantiated as `Cache[User]`
or `Cache[int]`, and its `Get` returns a `V` — a real `User`, a real `int` — with
the comma-ok idiom `(V, bool)` for presence. No assertion, no boxing, no panic:
`Cache[User].Get` cannot return anything but a `User`, and asking it for an `int`
is a compile error, not a runtime one. This is the rule: when you are abstracting
over the TYPE of a value while the behavior is identical (store, fetch, expire),
reach for a type parameter. Reserve interfaces for abstracting over BEHAVIOR that
genuinely differs between implementations — that is runtime polymorphism, which
generics do not replace.

The cache also carries a TTL and an injectable clock (`now func() time.Time`), so
expiry is testable deterministically with a fake clock and swept explicitly by
`Sweep`. That is orthogonal to the generics point but makes the type a realistic
cache rather than a toy map.

Create `anycache.go` (the boxed version, kept for contrast):

```go
package tscache

// AnyCache is the pre-generics design: values are boxed as any and callers must
// assert the type back out. A wrong assertion panics at runtime. This is the
// design the generic Cache replaces.
type AnyCache struct {
	items map[string]any
}

func NewAnyCache() *AnyCache {
	return &AnyCache{items: make(map[string]any)}
}

func (c *AnyCache) Set(key string, value any) {
	c.items[key] = value
}

func (c *AnyCache) Get(key string) (any, bool) {
	v, ok := c.items[key]
	return v, ok
}
```

Create `cache.go`:

```go
package tscache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe, type-safe, TTL cache. The value type is a type
// parameter, so Get returns a real V with no assertion and no panic risk.
type Cache[V any] struct {
	mu    sync.RWMutex
	now   func() time.Time
	items map[string]entry[V]
}

// New builds a cache using the real wall clock.
func New[V any]() *Cache[V] {
	return NewWithClock[V](time.Now)
}

// NewWithClock builds a cache with an injectable clock, for deterministic tests.
func NewWithClock[V any](now func() time.Time) *Cache[V] {
	return &Cache[V]{now: now, items: make(map[string]entry[V])}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[V]) Set(key string, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: c.now().Add(ttl)}
}

// Get returns the value and true if present and unexpired, else the zero V and
// false. The returned value is a concrete V -- no type assertion.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || !c.now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Delete removes key.
func (c *Cache[V]) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Sweep removes every expired entry and returns how many it evicted.
func (c *Cache[V]) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.items {
		if !c.now().Before(e.expires) {
			delete(c.items, k)
			n++
		}
	}
	return n
}

// Len reports the number of stored entries (expired or not).
func (c *Cache[V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### The runnable demo

The demo uses an injectable clock so TTL expiry is deterministic, and shows the
typed `Get` returning a `User` with no assertion.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tscache"
)

type User struct {
	ID   string
	Name string
}

func main() {
	// A manual clock we advance by reassigning the captured variable.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cache := tscache.NewWithClock[User](func() time.Time { return now })

	cache.Set("u1", User{ID: "u1", Name: "Alice"}, time.Minute)

	// Typed Get: u is a User, no assertion, no panic risk.
	u, ok := cache.Get("u1")
	fmt.Printf("hit=%v name=%s\n", ok, u.Name)

	// Advance the clock past the TTL; the entry is now expired.
	now = now.Add(2 * time.Minute)
	_, ok = cache.Get("u1")
	fmt.Printf("after ttl hit=%v\n", ok)

	// Sweep reclaims the expired entry.
	evicted := cache.Sweep()
	fmt.Printf("swept=%d len=%d\n", evicted, cache.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit=true name=Alice
after ttl hit=false
swept=1 len=0
```

### Tests

`TestGenericReturnsTypedValue` instantiates `Cache[User]` and asserts `Get`
returns a `User` directly. `TestGenericIntCache` does the same for `Cache[int]`.
`TestTTLExpiry` drives expiry with a fake clock. `TestAnyCachePanicsOnWrongAssertion`
proves the boxed version panics when a caller asserts the wrong type — the failure
mode the generic version turns into a compile error. `ExampleCache_Get` pins the
typed `Get` output so `go test` verifies the snippet too.

Create `cache_test.go`:

```go
package tscache

import (
	"fmt"
	"testing"
	"time"
)

type User struct {
	ID   string
	Name string
}

func TestGenericReturnsTypedValue(t *testing.T) {
	t.Parallel()

	c := New[User]()
	c.Set("u1", User{ID: "u1", Name: "Alice"}, time.Minute)

	// u is a User with no assertion. Asking for an int here would be a COMPILE
	// error, e.g. the next line does not compile:
	//   var n int = u
	u, ok := c.Get("u1")
	if !ok {
		t.Fatal("Get(u1) not found")
	}
	if u.Name != "Alice" {
		t.Fatalf("u.Name = %q, want Alice", u.Name)
	}
}

func TestGenericIntCache(t *testing.T) {
	t.Parallel()

	c := New[int]()
	c.Set("answer", 42, time.Minute)

	v, ok := c.Get("answer")
	if !ok || v != 42 {
		t.Fatalf("Get(answer) = %d,%v, want 42,true", v, ok)
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewWithClock[string](func() time.Time { return now })

	c.Set("k", "v", time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry missing before TTL")
	}

	now = now.Add(2 * time.Second) // advance past TTL
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry still present after TTL")
	}
	if n := c.Sweep(); n != 1 {
		t.Fatalf("Sweep evicted %d, want 1", n)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d after sweep, want 0", c.Len())
	}
}

// TestAnyCachePanicsOnWrongAssertion documents the failure the generic cache
// removes: the boxed version panics at runtime on a wrong type assertion.
func TestAnyCachePanicsOnWrongAssertion(t *testing.T) {
	t.Parallel()

	c := NewAnyCache()
	c.Set("u1", User{ID: "u1", Name: "Alice"})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a panic asserting User as int, got none")
		}
	}()

	v, _ := c.Get("u1")
	_ = v.(int) // panics: interface conversion, User is not int
}

// ExampleCache_Get shows the generic cache returning a concrete int with no
// assertion; the // Output line is auto-verified by `go test`.
func ExampleCache_Get() {
	c := New[int]()
	c.Set("answer", 42, time.Minute)
	v, ok := c.Get("answer")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

## Review

The generic cache moved a whole class of bug from runtime to compile time.
`TestAnyCachePanicsOnWrongAssertion` shows the `any` version panicking on a wrong
assertion; the generic `Cache[User]` cannot even be asked for an `int`, because
its `Get` returns `User` and the mismatched line does not compile (the test
documents that with a comment, since a real non-compiling line would fail the
build). The rule to carry away: use a type parameter when you are abstracting over
the value's TYPE and the behavior is uniform — containers, caches, result sets.
Keep an interface for abstracting over differing BEHAVIOR, which is genuine
runtime polymorphism that generics do not cover. The injectable clock is a
reminder that a type-safe container is still a real cache with TTL and sweeping,
tested deterministically without sleeping.

## Resources

- [Go blog — An Introduction to Generics](https://go.dev/blog/intro-generics) — type parameters, instantiation, and when they replace `interface{}`.
- [Go spec — Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — the language rules for generic types.
- [Type assertions](https://go.dev/ref/spec#Type_assertions) — the runtime conversion (and panic) the generic cache avoids.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-single-caller-premature-abstraction.md](08-single-caller-premature-abstraction.md) | Next: [10-payment-gateway-seam-narrow-interface.md](10-payment-gateway-seam-narrow-interface.md)
