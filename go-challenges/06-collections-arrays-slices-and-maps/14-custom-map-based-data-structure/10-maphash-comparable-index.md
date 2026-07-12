# Exercise 10: Hashing arbitrary comparable keys with hash/maphash.Comparable

The sharded map in Exercise 6 hashed `string` keys, and the sketch in Exercise 1
hashed strings too. But a real composite key is often a small struct —
`{Method, Path}` for a route, `{Tenant, UserID}` for an identity. The built-in map
hashes such keys for you; a custom sharded map, sketch, or bucketed index has to
hash them explicitly. `hash/maphash.Comparable[T]` (Go 1.24) is the tool: it hashes
*any* comparable value with a per-process random seed, generalizing your key
hashing from `string` to any comparable `T` while keeping the hash-flooding
resistance the runtime's own map has.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
chindex/                   independent module: example.com/chindex
  go.mod                   go 1.24+ (maphash.Comparable)
  chindex.go               type Index[T comparable]; New, Add, Has, Len
  cmd/
    demo/
      main.go              dedup struct-keyed routes and string scopes
  chindex_test.go          dedup, struct+string keys, seed equality/randomization
```

- Files: `chindex.go`, `cmd/demo/main.go`, `chindex_test.go`.
- Implement: a hash-bucketed set `Index[T comparable]` that hashes any comparable key via `hash/maphash.Comparable[T]` with a per-instance random seed, with `Add`, `Has`, `Len`.
- Test: equal keys hash equal under a fixed seed; a struct key and a string key both work; different seeds yield different hashes (seed randomization is real).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24   # hash/maphash.Comparable requires Go 1.24+
```

### One hash for every comparable key, seeded per instance

The built-in map can key on any comparable type — strings, ints, and structs whose
fields are all comparable — because the runtime knows how to hash each. When you
build your own hash-based structure you lose that for free and must supply a hash.
For strings there is `maphash.String`; for everything comparable there is
`maphash.Comparable[T comparable](seed, v) uint64`, added in Go 1.24. It hashes a
struct key, a pointer key, an array key — anything the language calls comparable —
and it guarantees the property a hash for a set needs: if `v1 == v2` then
`Comparable(seed, v1) == Comparable(seed, v2)` under the same seed. Equal keys hash
equal; that is what lets `Has` find what `Add` stored.

The seed is per-instance and random (`maphash.MakeSeed()`), which matters for two
reasons. First, it is the same hash-flooding defense the runtime's map uses: an
attacker who could predict your hash could craft keys that all land in one bucket,
collapsing O(1) lookups to O(n) and turning your index into a denial-of-service
amplifier. A random seed they cannot see defeats that. Second, it is why you must
never persist or compare hashes across processes or instances — the same key
hashes differently under a different seed, by design.

The structure here is a bucketed set: an array of buckets, each a slice, with keys
routed by `Comparable(seed, v) % len(buckets)` and collisions chained in the
slice. `Add` scans the target bucket for an equal key (dedup) before appending;
`Has` scans the target bucket. This is the same shape as the sharded map's routing
and the sketch's column selection — the generalization is only that the key type is
now any comparable `T`, not just `string`.

Create `chindex.go`:

```go
package chindex

import "hash/maphash"

// Index is a hash-bucketed set for any comparable key type. Keys are hashed with
// hash/maphash.Comparable under a per-instance random seed, so struct keys work
// and the index resists hash-flooding.
type Index[T comparable] struct {
	seed    maphash.Seed
	buckets [][]T
}

// New returns an Index with n buckets (clamped to >= 1) and a fresh random seed.
func New[T comparable](n int) *Index[T] {
	if n < 1 {
		n = 1
	}
	return &Index[T]{
		seed:    maphash.MakeSeed(),
		buckets: make([][]T, n),
	}
}

func (idx *Index[T]) bucket(v T) int {
	return int(maphash.Comparable(idx.seed, v) % uint64(len(idx.buckets)))
}

// Add inserts v if absent, returning true when it was newly added.
func (idx *Index[T]) Add(v T) bool {
	b := idx.bucket(v)
	for _, existing := range idx.buckets[b] {
		if existing == v {
			return false
		}
	}
	idx.buckets[b] = append(idx.buckets[b], v)
	return true
}

// Has reports whether v is present.
func (idx *Index[T]) Has(v T) bool {
	b := idx.bucket(v)
	for _, existing := range idx.buckets[b] {
		if existing == v {
			return true
		}
	}
	return false
}

// Len reports the number of distinct elements.
func (idx *Index[T]) Len() int {
	n := 0
	for _, b := range idx.buckets {
		n += len(b)
	}
	return n
}
```

### The runnable demo

The demo dedups struct-keyed routes (a duplicate route is not added twice) and,
through the same generic `Index`, a set of string scopes. The output is booleans
and counts, never hash values, so it is deterministic despite the random seed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/chindex"
)

// route is a composite comparable key. The built-in map supports it, and so does
// maphash.Comparable, which a custom index must call explicitly.
type route struct {
	Method string
	Path   string
}

func main() {
	seen := chindex.New[route](16)
	seen.Add(route{"GET", "/health"})
	seen.Add(route{"POST", "/orders"})
	seen.Add(route{"GET", "/health"}) // duplicate: not added again

	fmt.Println("distinct routes:", seen.Len())
	fmt.Println("has GET /health:", seen.Has(route{"GET", "/health"}))
	fmt.Println("has DELETE /orders:", seen.Has(route{"DELETE", "/orders"}))

	scopes := chindex.New[string](8)
	scopes.Add("read")
	scopes.Add("write")
	fmt.Println("has write scope:", scopes.Has("write"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
distinct routes: 2
has GET /health: true
has DELETE /orders: false
has write scope: true
```

### Tests

The tests cover the set behavior (dedup, present-vs-absent), that both a struct key
and a string key work through the same generic index, and the two hashing
properties `maphash.Comparable` guarantees: equal keys hash equal under a fixed
seed, and two independent seeds do not produce identical hashes for every key
(seed randomization is real, not a no-op).

Create `chindex_test.go`:

```go
package chindex

import (
	"fmt"
	"hash/maphash"
	"testing"
)

type key struct {
	A string
	B int
}

func TestAddHasDedup(t *testing.T) {
	t.Parallel()

	idx := New[key](16)
	if !idx.Add(key{"x", 1}) {
		t.Fatal("first Add should report newly added")
	}
	if idx.Add(key{"x", 1}) {
		t.Fatal("duplicate Add should report already present")
	}
	if idx.Len() != 1 {
		t.Fatalf("Len = %d, want 1", idx.Len())
	}
	if !idx.Has(key{"x", 1}) {
		t.Fatal("Has should find the added key")
	}
	if idx.Has(key{"x", 2}) {
		t.Fatal("Has should not find an absent key")
	}
}

func TestStructAndStringKeysBothWork(t *testing.T) {
	t.Parallel()

	structs := New[key](8)
	structs.Add(key{"a", 1})
	if !structs.Has(key{"a", 1}) {
		t.Fatal("struct key not found")
	}

	strs := New[string](8)
	strs.Add("hello")
	if !strs.Has("hello") {
		t.Fatal("string key not found")
	}
}

func TestEqualKeysHashEqualUnderFixedSeed(t *testing.T) {
	t.Parallel()

	seed := maphash.MakeSeed()
	k1 := key{"same", 7}
	k2 := key{"same", 7}
	if maphash.Comparable(seed, k1) != maphash.Comparable(seed, k2) {
		t.Fatal("equal keys must hash equal under the same seed")
	}
}

func TestDifferentSeedsGiveDifferentHashes(t *testing.T) {
	t.Parallel()

	s1 := maphash.MakeSeed()
	s2 := maphash.MakeSeed()
	allEqual := true
	for i := range 32 {
		k := key{"k", i}
		if maphash.Comparable(s1, k) != maphash.Comparable(s2, k) {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Fatal("two independent seeds produced identical hashes for all keys")
	}
}

func Example() {
	idx := New[key](16)
	idx.Add(key{"a", 1})
	idx.Add(key{"a", 1}) // duplicate
	idx.Add(key{"b", 2})
	fmt.Println(idx.Len())
	fmt.Println(idx.Has(key{"a", 1}))
	// Output:
	// 2
	// true
}
```

## Review

The index is correct when `Add`/`Has` dedup and locate keys, and when it relies on
the two `maphash.Comparable` guarantees: equal comparable values hash equal under a
fixed seed (so `Has` finds what `Add` stored), and the per-instance random seed
makes the hash unpredictable across instances (so it must never be persisted or
compared cross-process). The point of the exercise is the generalization: the same
routing you wrote for `string` keys in the sharded map now works for any comparable
`T`, including struct composite keys, because `Comparable[T]` hashes them. This
requires Go 1.24+; on an older toolchain `maphash.Comparable` does not exist. Run
`go test -count=1 -race ./...`.

## Resources

- [`hash/maphash` package](https://pkg.go.dev/hash/maphash) — `MakeSeed`, `Seed`, `String`, and `Comparable[T]` (Go 1.24).
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — the addition of `maphash.Comparable` and `WriteComparable`.
- [`comparable` constraint](https://go.dev/ref/spec#Comparison_operators) — which types are comparable and thus valid keys.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-topk-heavy-hitters.md](09-topk-heavy-hitters.md) | Next: [11-deterministic-cache-key-signature.md](11-deterministic-cache-key-signature.md)
