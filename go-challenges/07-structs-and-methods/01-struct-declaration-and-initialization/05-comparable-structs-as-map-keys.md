# Exercise 5: A Composite Cache Key From a Comparable Struct

When a cache is keyed by more than one field — tenant, resource, version — the
clean answer is a struct key. A struct all of whose fields are comparable is
itself comparable and hashable, so `map[CacheKey]V` just works, with no manual
string concatenation. This module builds that cache, then shows the two hazards:
a `float64` field (`NaN != NaN` loses entries) and a slice field (uncomparable, a
compile error).

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
cachekey/                   independent module: example.com/cachekey
  go.mod                    go 1.24
  cache.go                  type CacheKey (comparable); Cache[V] using map[CacheKey]V
  cmd/
    demo/
      main.go               puts and gets by composite key, prints hits/misses
  cache_test.go             equal keys hit; differing Version misses; NaN-key hazard
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a `CacheKey{TenantID string; Resource string; Version int}` used directly as a map key in a generic `Cache[V]` with `Put`/`Get`.
- Test: two independently built equal `CacheKey`s hit the same entry; a differing `Version` misses; a `StatsKey` with a `NaN` `float64` field written then looked up misses, and `math.IsNaN` confirms the field. Prose shows the slice-key compile error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cachekey/cmd/demo
cd ~/go-exercises/cachekey
go mod init example.com/cachekey
go mod edit -go=1.24
```

### When a struct is a safe map key

Go's `==` on a struct compares field by field, and a map key must be comparable.
The rule is transitive: a struct is comparable exactly when every field is
comparable. `CacheKey{TenantID, Resource string; Version int}` is built from
strings and an int — all comparable — so `CacheKey` is comparable, hashable, and a
perfect composite key. Two `CacheKey` values built independently with the same
field values are `==`, so they hit the same map entry. This is cleaner and faster
than concatenating `tenant + "|" + resource + "|" + strconv.Itoa(version)` into a
string key: no allocation, no separator-collision bug, and the type system checks
the shape.

Two hazards break this cleanness, and both are worth internalizing.

**Floats.** A `float64` field is comparable, so a struct containing one *compiles*
as a map key. But IEEE-754 says `NaN != NaN`. A key written with a `NaN` field can
therefore never be equal to any key you look up with — not even a byte-identical
one — so the entry is written and then permanently unreachable, a silent memory leak. If a
key can carry a float that might be `NaN` (a computed ratio, a parsed metric),
either canonicalize it or key on something else.

**Slices, maps, funcs.** These are not comparable at all. A struct containing one
of them cannot be used as a map key: `map[BadKey]V` fails to compile with
`invalid map key type`. When the natural key is uncomparable, derive a canonical
string (for example, sort and join the slice) and key on that.

Create `cache.go`:

```go
package cache

// CacheKey is a composite key. Every field is comparable, so CacheKey is
// comparable and usable directly as a map key.
type CacheKey struct {
	TenantID string
	Resource string
	Version  int
}

// Cache is a tiny in-memory cache keyed by a composite struct key.
type Cache[V any] struct {
	items map[CacheKey]V
}

// New returns an empty cache.
func New[V any]() *Cache[V] {
	return &Cache[V]{items: make(map[CacheKey]V)}
}

// Put stores value under key.
func (c *Cache[V]) Put(key CacheKey, value V) {
	c.items[key] = value
}

// Get returns the value and whether it was present.
func (c *Cache[V]) Get(key CacheKey) (V, bool) {
	v, ok := c.items[key]
	return v, ok
}

// StatsKey demonstrates the float hazard: it is comparable and compiles as a map
// key, but a NaN Ratio can never be read back because NaN != NaN.
type StatsKey struct {
	Metric string
	Ratio  float64
}
```

A slice-bearing key would look like this, and it does **not** compile — which is
why it is shown as illustration only, not built into the module:

```
// invalid map key type: Tags is a []string, so LabelKey is not comparable.
type LabelKey struct {
	Name string
	Tags []string
}

var m map[LabelKey]int // compile error: invalid map key type LabelKey
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cachekey"
)

func main() {
	c := cache.New[string]()
	k := cache.CacheKey{TenantID: "acme", Resource: "invoice", Version: 2}
	c.Put(k, "rendered-invoice-v2")

	// An independently built equal key hits the same entry.
	same := cache.CacheKey{TenantID: "acme", Resource: "invoice", Version: 2}
	if v, ok := c.Get(same); ok {
		fmt.Println("hit:", v)
	}

	// A different version misses.
	other := cache.CacheKey{TenantID: "acme", Resource: "invoice", Version: 3}
	if _, ok := c.Get(other); !ok {
		fmt.Println("miss: version 3")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit: rendered-invoice-v2
miss: version 3
```

### Tests

`TestEqualKeysHit` and `TestDifferentVersionMisses` prove struct-key equality is
field-based. `TestNaNKeyIsUnreadable` is the important one: it puts an entry under
a `StatsKey` whose `Ratio` is `math.NaN()`, then looks it up with a key holding
`math.NaN()` again and asserts the lookup **misses**, demonstrating the float
hazard, and confirms the field really is `NaN` with `math.IsNaN`.

Create `cache_test.go`:

```go
package cache

import (
	"fmt"
	"math"
	"testing"
)

func TestEqualKeysHit(t *testing.T) {
	t.Parallel()
	c := New[int]()
	c.Put(CacheKey{"acme", "invoice", 2}, 42)

	got, ok := c.Get(CacheKey{"acme", "invoice", 2})
	if !ok || got != 42 {
		t.Fatalf("Get = %d,%v; want 42,true", got, ok)
	}
}

func TestDifferentVersionMisses(t *testing.T) {
	t.Parallel()
	c := New[int]()
	c.Put(CacheKey{"acme", "invoice", 2}, 42)

	if _, ok := c.Get(CacheKey{"acme", "invoice", 3}); ok {
		t.Fatal("version 3 should miss")
	}
	if _, ok := c.Get(CacheKey{"other", "invoice", 2}); ok {
		t.Fatal("different tenant should miss")
	}
}

func TestNaNKeyIsUnreadable(t *testing.T) {
	t.Parallel()
	m := make(map[StatsKey]int)
	k := StatsKey{Metric: "latency", Ratio: math.NaN()}
	m[k] = 7

	if !math.IsNaN(k.Ratio) {
		t.Fatal("test setup: Ratio should be NaN")
	}
	// Looking up with a NaN-bearing key can never match, even a byte-identical one.
	if _, ok := m[StatsKey{Metric: "latency", Ratio: math.NaN()}]; ok {
		t.Fatal("NaN key unexpectedly matched")
	}
	// Even the very same key value misses, because NaN != NaN.
	if _, ok := m[k]; ok {
		t.Fatal("the original NaN key matched itself; NaN should never compare equal")
	}
}

func ExampleCache() {
	c := New[string]()
	c.Put(CacheKey{"t1", "r1", 1}, "v1")
	v, ok := c.Get(CacheKey{"t1", "r1", 1})
	fmt.Println(v, ok)
	// Output: v1 true
}
```

## Review

The composite key is correct when equality is purely field-based: two `CacheKey`
values are the same map entry exactly when all three fields match, with no
manual string building and no separator ambiguity. The `NaN` test makes the float
hazard concrete — an entry written under a `NaN` key is unreachable forever, which
is why a key type should be built from strings and integers, and any float that
could be `NaN` should be canonicalized out of the key or the key derived as a
string. The slice case is a compile-time wall, not a runtime hazard: a struct with
a slice, map, or func field simply cannot be a map key, so when the natural key is
uncomparable you build a canonical string key instead. Run `go test -race` and
`go vet`.

## Resources

- [Go Spec: comparison operators](https://go.dev/ref/spec#Comparison_operators) — struct comparability and the map-key requirement.
- [Go Spec: map types](https://go.dev/ref/spec#Map_types) — "the comparison operators == and != must be fully defined for the key type."
- [`math.NaN` / `math.IsNaN`](https://pkg.go.dev/math#NaN) — the `NaN != NaN` behavior behind the hazard.
- [Go maps in action](https://go.dev/blog/maps) — key types and when a struct key is the right call.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-embedded-entity-composition.md](06-embedded-entity-composition.md)
