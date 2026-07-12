# Exercise 25: Multi-Level Cache Invalidation — Cascading Through Layers with Nested Iterators

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A three-tier cache -- an in-process L1, a shared L2 like Redis, an L3 backed
by disk or a CDN -- has to invalidate a key everywhere it might be cached,
not just in one tier, or a stale value in a slower tier can resurface after
the fast tier's copy is gone. Modeling the cascade as an outer `iter.Seq`
over keys with a small nested `iter.Seq[int]` driving the tier order makes
the top-down invalidation order an explicit, testable property of the code
rather than three copy-pasted `if` statements that could silently drift out
of order during a refactor. This exercise is an independent module with its
own `go mod init`.

## What you'll build

```text
cache/                     independent module: example.com/cache-invalidation-cascade
  go.mod                   module example.com/cache-invalidation-cascade
  cache.go                 Cache, New, Invalidation, Invalidate
  cmd/
    demo/
      main.go              runnable demo: cascading invalidation across L1/L2/L3
  cache_test.go            full cascade, unknown key, early-stop leaves later keys untouched
```

Implement: `New() *Cache` and `(*Cache) Invalidate(keys iter.Seq[string]) iter.Seq[Invalidation]` yielding one `Invalidation{Key, Levels}` per key, deleting the key from L1, then L2, then L3.
Test: a key present in all three tiers reports `[true,true,true]` and is gone from every map; a key present only in L1 reports `[true,false,false]`; an unknown key touches no tier; a consumer break after the first key leaves the second key's entries untouched in every tier.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

`Invalidate` ranges `keys` in the outer loop and, for each key, ranges a
private `levels()` iterator that just yields `0, 1, 2` -- the tier indices --
so the cascade order itself is a `for lvl := range levels()` loop rather
than three hand-unrolled `if` blocks that happen to appear in the right
order today but carry no guarantee they will stay that way after a future
edit. `levels()` is unexported and carries no state: it exists purely so the
cascade reads the same way every other combinator in this lesson does.
Deleting top-down -- L1 first, then L2, then L3 -- is not arbitrary: if an
early consumer break interrupted the cascade after only L1 had been cleared,
a request racing in immediately afterward could still be served a stale
value from L2 or L3, but it could never be served a stale value that was
already removed from the *fastest* tier, which is the tier most requests
hit first. Clearing bottom-up would leave exactly the opposite, worse
exposure: a stale L1 hit even after L2 and L3 were already cleared.

Create `cache.go`:

```go
package cache

import "iter"

// Cache is a three-tier cache; each tier is its own map, mirroring how a
// real multi-level cache lets a key live in L1 only, in L1 and L2, or in all
// three, and be promoted or evicted from each tier independently.
type Cache struct {
	L1, L2, L3 map[string]string
}

// New creates an empty three-tier Cache.
func New() *Cache {
	return &Cache{L1: map[string]string{}, L2: map[string]string{}, L3: map[string]string{}}
}

// Invalidation reports, for one key, which of the three tiers actually held
// (and had removed) the entry: index 0 is L1, 1 is L2, 2 is L3.
type Invalidation struct {
	Key    string
	Levels [3]bool
}

// levels is a tiny nested iter.Seq[int] that yields the tier indices 0,1,2
// in order. It exists so the cascade itself -- delete from L1, then L2, then
// L3 -- is expressed as a range loop, consistent with every other combinator
// in this lesson, instead of three hand-unrolled if-statements; it carries
// no state of its own and is not exported.
func levels() iter.Seq[int] {
	return func(yield func(int) bool) {
		for lvl := range 3 {
			if !yield(lvl) {
				return
			}
		}
	}
}

// Invalidate deletes key from L1, then L2, then L3, in that order, for every
// key in keys, and yields one Invalidation per key recording which tiers
// actually held the entry. Clearing top-down -- fastest tier first -- is
// what keeps the tiers coherent while the cascade for a single key is in
// progress: if a consumer's early break stopped the cascade after L1 alone,
// a value could otherwise still be served from a stale L2 or L3 entry, but
// clearing L1 first guarantees the fastest tier never answers with data an
// incomplete cascade left behind in a slower tier. Because every tier access
// happens on the single goroutine driving this range loop, no extra
// synchronization is needed here; a Cache shared across goroutines still
// needs its own locking around calls to Invalidate itself.
func (c *Cache) Invalidate(keys iter.Seq[string]) iter.Seq[Invalidation] {
	tiers := [3]map[string]string{c.L1, c.L2, c.L3}
	return func(yield func(Invalidation) bool) {
		for key := range keys {
			inv := Invalidation{Key: key}
			for lvl := range levels() {
				if _, ok := tiers[lvl][key]; ok {
					delete(tiers[lvl], key)
					inv.Levels[lvl] = true
				}
			}
			if !yield(inv) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cache-invalidation-cascade"
)

func main() {
	c := cache.New()
	c.L1["user:42"] = "cached-l1"
	c.L2["user:42"] = "cached-l2"
	c.L3["user:42"] = "cached-l3"
	c.L1["user:7"] = "cached-l1-only"

	keys := func(yield func(string) bool) {
		for _, k := range []string{"user:42", "user:7", "user:99"} {
			if !yield(k) {
				return
			}
		}
	}

	for inv := range c.Invalidate(keys) {
		fmt.Printf("key=%-9s L1=%v L2=%v L3=%v\n", inv.Key, inv.Levels[0], inv.Levels[1], inv.Levels[2])
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
key=user:42   L1=true L2=true L3=true
key=user:7    L1=true L2=false L3=false
key=user:99   L1=false L2=false L3=false
```

`user:42` was cached in all three tiers and is reported as removed from all
three; `user:7` only ever lived in L1 and is reported accordingly; `user:99`
was never cached anywhere, so the cascade touches no tier for it and every
flag is `false`.

### Tests

Create `cache_test.go`:

```go
package cache

import "testing"

func TestInvalidateCascadesThroughPresentTiers(t *testing.T) {
	t.Parallel()

	c := New()
	c.L1["k1"] = "v1"
	c.L2["k1"] = "v1"
	c.L3["k1"] = "v1"

	c.L1["k2"] = "v2" // only in L1

	c.L2["k3"] = "v3" // only in L2
	c.L3["k3"] = "v3" // and L3

	keys := func(yield func(string) bool) {
		for _, k := range []string{"k1", "k2", "k3"} {
			if !yield(k) {
				return
			}
		}
	}

	var got []Invalidation
	for inv := range c.Invalidate(keys) {
		got = append(got, inv)
	}

	want := []Invalidation{
		{Key: "k1", Levels: [3]bool{true, true, true}},
		{Key: "k2", Levels: [3]bool{true, false, false}},
		{Key: "k3", Levels: [3]bool{false, true, true}},
	}
	if len(got) != len(want) {
		t.Fatalf("got = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if _, ok := c.L1["k1"]; ok {
		t.Fatal("k1 should be gone from L1")
	}
	if _, ok := c.L2["k1"]; ok {
		t.Fatal("k1 should be gone from L2")
	}
	if _, ok := c.L3["k1"]; ok {
		t.Fatal("k1 should be gone from L3")
	}
}

func TestInvalidateUnknownKeyTouchesNoTier(t *testing.T) {
	t.Parallel()

	c := New()
	c.L1["present"] = "v"

	keys := func(yield func(string) bool) {
		yield("missing")
	}

	var got Invalidation
	for inv := range c.Invalidate(keys) {
		got = inv
	}
	want := Invalidation{Key: "missing", Levels: [3]bool{false, false, false}}
	if got != want {
		t.Fatalf("got = %+v, want %+v", got, want)
	}
	if len(c.L1) != 1 {
		t.Fatalf("L1 should be untouched, got %v", c.L1)
	}
}

func TestInvalidateStopsEarlyLeavesLaterKeysUntouched(t *testing.T) {
	t.Parallel()

	c := New()
	c.L1["k1"] = "v"
	c.L1["k2"] = "v"

	keys := func(yield func(string) bool) {
		for _, k := range []string{"k1", "k2"} {
			if !yield(k) {
				return
			}
		}
	}

	count := 0
	for range c.Invalidate(keys) {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if _, ok := c.L1["k1"]; ok {
		t.Fatal("k1 should have been invalidated before the break")
	}
	if _, ok := c.L1["k2"]; !ok {
		t.Fatal("k2 should still be present: cascade stopped before reaching it")
	}
}
```

## Review

The nested `levels()` iterator is a small piece of ceremony for something
that could be three `if` statements, and that trade is deliberate: with
three explicit statements, a future edit that adds an L0 or reorders a tier
has to be applied by hand in the right place, whereas the `for lvl := range
levels()` loop makes the order a single, obvious sequence to change in one
spot. The property that actually matters for correctness, though, is the
top-down deletion order itself -- get it backwards and a consumer that reads
through this same `Cache` between two calls to `Invalidate` (or that breaks
out of one cascade early) can observe a state where a slower tier was
cleared but the fast tier still serves the stale value, which is precisely
the incoherence multi-level invalidation exists to prevent.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [AWS: caching invalidation strategies](https://aws.amazon.com/caching/best-practices/)
- [Redis: cache invalidation patterns](https://redis.io/docs/latest/develop/use/patterns/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-graceful-drain-ordered-shutdown.md](24-graceful-drain-ordered-shutdown.md) | Next: [26-consistent-hashing-partition-iterator.md](26-consistent-hashing-partition-iterator.md)
