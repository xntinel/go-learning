# Exercise 9: A Comparable Struct As A Map Key And Dedup Set

A cache or dedup keyed by more than one field is cleanest when the key is a small
struct used directly as a map key — no string concatenation, no delimiter
injection bugs. This works because Go structs are comparable with `==` (and thus
map-keyable) iff all their fields are comparable, and the zero-value struct is
itself a valid, distinct key. This exercise builds a per-key cache and a request
dedup set on a `{Tenant, Resource, Version}` key, and documents the failure mode
when an incomparable field sneaks in.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cachekey/                  independent module: example.com/cachekey
  go.mod
  cachekey.go              Key, Cache[V], Set (dedup), zero-key handling
  cmd/
    demo/
      main.go              caches by key, dedups requests, uses the zero key
  cachekey_test.go         equal-field collision, zero-key, dedup collapse
```

Files: `cachekey.go`, `cmd/demo/main.go`, `cachekey_test.go`.
Implement: a comparable `Key{Tenant, Resource string; Version int}`, a generic `Cache[V]` keyed by it, and a `Set` that dedups by key returning whether the key was newly added.
Test: two structs with equal fields map to one entry; the zero-value key stores and reads correctly; dedup collapses duplicates. Document why a struct with a slice field cannot be a key.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/09-comparable-struct-cache-key/cmd/demo
cd go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/09-comparable-struct-cache-key
```

## Why struct keys work, and where they break

A Go map requires a comparable key type, and a struct is comparable exactly when
every field is comparable — strings, numbers, booleans, pointers, channels, and
comparable structs/arrays all qualify. Two struct values are equal under `==`
when all their corresponding fields are equal, so `Key{"acme", "invoice", 3}`
computed in two different requests is the *same* map key: it hashes and compares
identically, and both land on one entry. That is what lets you key a cache on a
composite identity without inventing a string format like
`tenant + "|" + resource` — which is fragile (what if a tenant name contains
`|`?) and slower.

The zero-value key is not special-cased away: `Key{}` — empty tenant, empty
resource, version `0` — is a perfectly valid, distinct key you can store under
and read back, just like any other. That matters because it means a "missing"
lookup and a "stored under the zero key" lookup are told apart by the comma-ok
result, not by the key's value. The cache returns `(value, ok)` so a stored zero
value and an absent key are distinguishable.

The dedup set uses the same key type: `Set.Add(k)` records `k` and returns
whether it was newly seen, so a stream of requests carrying duplicate keys
collapses to one processed request each — the shape of an idempotency guard. It
lazily allocates its backing map so its zero value is usable.

The failure mode to internalize: add a slice, map, or func field to `Key` and it
stops being comparable — the code using it as a map key or comparing it with `==`
no longer *compiles*. This is a compile-time safety net, but it is also a
breaking change you can trigger accidentally by "just adding a `Labels []string`
field". When you need such data alongside the key, keep it *out* of the key (put
it in the value) or reduce it to a comparable form (a joined string, an array).
The illustrative snippet below does not compile and is intentionally not built:

```go
// Does NOT compile: a slice field makes the struct incomparable.
// type BadKey struct {
// 	Tenant string
// 	Labels []string // slice -> BadKey is not comparable -> cannot be a map key
// }
// var m map[BadKey]int // error: invalid map key type BadKey
```

Create `cachekey.go`:

```go
package cachekey

// Key is a composite cache/dedup key. Every field is comparable, so Key is
// comparable and usable as a map key; the zero value Key{} is a valid key.
type Key struct {
	Tenant   string
	Resource string
	Version  int
}

// Cache maps a Key to a value of type V.
type Cache[V any] struct {
	m map[Key]V
}

// NewCache returns an empty cache.
func NewCache[V any]() *Cache[V] {
	return &Cache[V]{m: make(map[Key]V)}
}

// Put stores v under k, overwriting any existing entry.
func (c *Cache[V]) Put(k Key, v V) {
	c.m[k] = v
}

// Get returns the value stored under k and whether it was present. A stored
// zero value is distinguished from an absent key by ok.
func (c *Cache[V]) Get(k Key) (V, bool) {
	v, ok := c.m[k]
	return v, ok
}

// Len reports the number of distinct keys stored.
func (c *Cache[V]) Len() int {
	return len(c.m)
}

// Set is a dedup set of Keys. Its zero value is usable: var s Set.
type Set struct {
	seen map[Key]struct{}
}

// Add records k and reports whether it was newly added (true) or already
// present (false). The backing map is allocated lazily on first use.
func (s *Set) Add(k Key) bool {
	if s.seen == nil {
		s.seen = make(map[Key]struct{})
	}
	if _, ok := s.seen[k]; ok {
		return false
	}
	s.seen[k] = struct{}{}
	return true
}
```

## The runnable demo

The demo caches a value under a key, reads it back with an equal-but-separately-
constructed key (showing they collide to one entry), stores under the zero key,
and runs a duplicate-laden request stream through the dedup set.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cachekey"
)

func main() {
	c := cachekey.NewCache[string]()
	c.Put(cachekey.Key{Tenant: "acme", Resource: "invoice", Version: 3}, "cached-body")

	// A separately-constructed but field-equal key hits the same entry.
	got, ok := c.Get(cachekey.Key{Tenant: "acme", Resource: "invoice", Version: 3})
	fmt.Printf("lookup ok=%v value=%q entries=%d\n", ok, got, c.Len())

	// The zero-value key is a valid, distinct key.
	c.Put(cachekey.Key{}, "zero-key-body")
	zv, ok := c.Get(cachekey.Key{})
	fmt.Printf("zero-key ok=%v value=%q entries=%d\n", ok, zv, c.Len())

	var s cachekey.Set
	requests := []cachekey.Key{
		{Tenant: "acme", Resource: "invoice", Version: 3},
		{Tenant: "acme", Resource: "invoice", Version: 3}, // duplicate
		{Tenant: "acme", Resource: "invoice", Version: 4},
	}
	processed := 0
	for _, r := range requests {
		if s.Add(r) {
			processed++
		}
	}
	fmt.Printf("requests=%d processed=%d\n", len(requests), processed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lookup ok=true value="cached-body" entries=1
zero-key ok=true value="zero-key-body" entries=2
requests=3 processed=2
```

## Tests

`TestEqualKeysCollide` proves two separately-built, field-equal keys map to one
entry. `TestZeroKey` proves the zero-value key stores and reads back correctly
and is distinct from an absent key via comma-ok. `TestDedupCollapses` runs a
duplicate-laden stream through `Set` and asserts only the distinct keys are newly
added.

Create `cachekey_test.go`:

```go
package cachekey

import "testing"

func TestEqualKeysCollide(t *testing.T) {
	t.Parallel()

	c := NewCache[int]()
	c.Put(Key{Tenant: "acme", Resource: "invoice", Version: 3}, 1)
	c.Put(Key{Tenant: "acme", Resource: "invoice", Version: 3}, 2) // same key

	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 (equal keys collide)", c.Len())
	}
	got, ok := c.Get(Key{Tenant: "acme", Resource: "invoice", Version: 3})
	if !ok || got != 2 {
		t.Fatalf("Get = %d,%v; want 2,true (last write wins)", got, ok)
	}
}

func TestZeroKey(t *testing.T) {
	t.Parallel()

	c := NewCache[string]()

	if _, ok := c.Get(Key{}); ok {
		t.Fatal("zero key should be absent before Put")
	}
	c.Put(Key{}, "")
	got, ok := c.Get(Key{})
	if !ok || got != "" {
		t.Fatalf("Get(zero key) = %q,%v; want \"\",true", got, ok)
	}
}

func TestDedupCollapses(t *testing.T) {
	t.Parallel()

	var s Set
	keys := []Key{
		{Tenant: "acme", Resource: "invoice", Version: 3},
		{Tenant: "acme", Resource: "invoice", Version: 3},
		{Tenant: "acme", Resource: "invoice", Version: 4},
		{}, // zero key counts as one distinct key
		{},
	}

	added := 0
	for _, k := range keys {
		if s.Add(k) {
			added++
		}
	}
	if added != 3 {
		t.Fatalf("newly added = %d, want 3 distinct keys", added)
	}
}
```

## Review

The cache is correct when field-equal keys — however they were constructed —
collide to one entry, and when the zero-value key behaves like any other key,
distinguished from "absent" only by the comma-ok result. Prefer a struct key over
a hand-built string key: it is faster, allocation-free, and immune to delimiter-
injection bugs. The compile-time trap is real and worth respecting: adding a
slice or map field to `Key` breaks every map and `==` use of it at build time, so
keep incomparable data in the *value*, not the key. Because these types hold no
locks, the tests do not need `-race` to prove correctness, but running the suite
with `-race` is still the standard verification.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — when struct types are comparable.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — the key type must be comparable.
- [Go maps in action](https://go.dev/blog/maps) — using structs as keys.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-zero-value-default-options.md](08-zero-value-default-options.md) | Next: [10-sql-null-vs-pointer-columns.md](10-sql-null-vs-pointer-columns.md)
