# Exercise 2: Ordered Map Iterators (All, Keys, Values)

A keyed collection's iterators follow the `maps` vocabulary, not the `slices` one: `All` yields key-value pairs, `Keys` yields keys, `Values` yields values. This exercise builds an insertion-ordered `OrderedMap[K, V]` so its iteration is deterministic, and exposes exactly those three methods with the standard shapes — making the contrast with the list's index-element `All` explicit.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
omap.go              OrderedMap[K, V], New, Set, Get, Len, All, Keys, Values
cmd/
  demo/
    main.go          build an ordered map, range All, print Keys, rebuild via maps.Collect
omap_test.go         insertion order, update-keeps-order, All/Keys/Values agree, early break
```

- Files: `omap.go`, `cmd/demo/main.go`, `omap_test.go`.
- Implement: `OrderedMap[K comparable, V any]` with `Set`, `Get`, `Len`, and `All() iter.Seq2[K, V]`, `Keys() iter.Seq[K]`, `Values() iter.Seq[V]`.
- Test: `omap_test.go` checks insertion order is preserved, that updating an existing key changes the value but not the order, that `All`, `Keys`, and `Values` agree, and that `break` stops iteration.
- Verify: `go test -run TestOrderedMap -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/05-designing-iterator-apis/02-ordered-map-iterators/cmd/demo && cd go-solutions/25-iterators-and-modern-go/05-designing-iterator-apis/02-ordered-map-iterators
```

### Why `All` is `Seq2[K, V]` here and what insertion order buys

The list's `All` paired each element with an `int` index because position is how you address a list. A map is addressed by key, so its `All` pairs each value with its key: `iter.Seq2[K, V]`, exactly as `maps.All(map[K]V) iter.Seq2[K, V]`. This is the lesson's central contrast. Both methods are named `All` and both return `Seq2`, but the first component means different things — index for the list, key for the map — and a caller reads the type to know which. Naming them both `All` is what lets the standard library's mental model carry over; giving the map a bespoke name like `Entries` or `Pairs` would force the caller to relearn a verb they already know. `Keys` returns `iter.Seq[K]` (`maps.Keys`) and `Values` returns `iter.Seq[V]` (`maps.Values`), the two single-axis projections.

A plain Go `map` iterates in randomized order, which makes any iterator over it non-deterministic and its tests flaky. `OrderedMap` fixes that by keeping a `keys` slice alongside the `values` map: `Set` appends to `keys` only when the key is new, so the slice records insertion order and updates to an existing key leave the order untouched. Every iterator then walks `keys` and looks each value up in the map, so `All`, `Keys`, and `Values` all visit entries in the same stable order. The `comparable` constraint on `K` is mandatory and not incidental: it is what lets `K` be used as a Go map key in the first place, the same constraint `maps.All` places on its key type.

Each iterator is a fresh closure over the `keys` slice and `values` map the `OrderedMap` owns, so the iterators are reusable across passes, and each checks `yield`'s boolean to honor an early `break`. Because `All` looks the value up by key inside the loop (`m.values[k]`), the three iterators cannot drift apart: they share one source of order and one source of values.

Create `omap.go`:

```go
package omap

import "iter"

// OrderedMap is a map that remembers insertion order, so iteration over it is
// deterministic. K must be comparable to serve as a Go map key.
type OrderedMap[K comparable, V any] struct {
	keys   []K
	values map[K]V
}

// New returns an empty OrderedMap.
func New[K comparable, V any]() *OrderedMap[K, V] {
	return &OrderedMap[K, V]{values: make(map[K]V)}
}

// Set stores v under k. A new key is appended to the iteration order; updating
// an existing key changes its value but keeps its original position.
func (m *OrderedMap[K, V]) Set(k K, v V) {
	if _, ok := m.values[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.values[k] = v
}

// Get returns the value stored under k and whether it was present.
func (m *OrderedMap[K, V]) Get(k K) (V, bool) {
	v, ok := m.values[k]
	return v, ok
}

// Len reports the number of entries.
func (m *OrderedMap[K, V]) Len() int {
	return len(m.keys)
}

// All iterates over key/value pairs in insertion order, mirroring maps.All.
// Unlike the standard map, this order is deterministic.
func (m *OrderedMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for _, k := range m.keys {
			if !yield(k, m.values[k]) {
				return
			}
		}
	}
}

// Keys iterates over keys in insertion order, mirroring maps.Keys.
func (m *OrderedMap[K, V]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		for _, k := range m.keys {
			if !yield(k) {
				return
			}
		}
	}
}

// Values iterates over values in insertion order, mirroring maps.Values.
func (m *OrderedMap[K, V]) Values() iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, k := range m.keys {
			if !yield(m.values[k]) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo builds an ordered map, updates an existing key to show the order is unaffected, walks it with `All`, prints the `Keys` projection, and then reconstructs a plain `map` from the iterator with `maps.Collect` — the standard sink that turns an `iter.Seq2[K, V]` back into a `map[K]V`, demonstrating that the method composes with the `maps` package it mirrors.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/omap"
)

func main() {
	m := omap.New[string, int]()
	m.Set("charlie", 1)
	m.Set("alice", 2)
	m.Set("bob", 3)
	m.Set("alice", 20) // update keeps alice's original position

	fmt.Println("All (key, value) in insertion order:")
	for k, v := range m.All() {
		fmt.Printf("  %s=%d\n", k, v)
	}

	fmt.Println("Keys:", slices.Collect(m.Keys()))

	plain := maps.Collect(m.All())
	fmt.Println("maps.Collect rebuilt", len(plain), "entries; alice =", plain["alice"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
All (key, value) in insertion order:
  charlie=1
  alice=20
  bob=3
Keys: [charlie alice bob]
maps.Collect rebuilt 3 entries; alice = 20
```

### Tests

The tests pin the order semantics and the agreement between the three iterators. `TestOrderedMapOrder` inserts keys out of alphabetical order and asserts `Keys` returns them in insertion order. `TestOrderedMapUpdateKeepsOrder` updates an existing key and asserts the order is unchanged but the value is new. `TestOrderedMapAllAgrees` collects `All`, `Keys`, and `Values` and asserts the keys and values line up pairwise. `TestOrderedMapBreak` breaks out of an `All` loop early and asserts iteration stopped.

Create `omap_test.go`:

```go
package omap

import (
	"slices"
	"testing"
)

func TestOrderedMapOrder(t *testing.T) {
	t.Parallel()

	m := New[string, int]()
	m.Set("charlie", 1)
	m.Set("alice", 2)
	m.Set("bob", 3)

	if got := slices.Collect(m.Keys()); !slices.Equal(got, []string{"charlie", "alice", "bob"}) {
		t.Fatalf("Keys = %v, want insertion order [charlie alice bob]", got)
	}
}

func TestOrderedMapUpdateKeepsOrder(t *testing.T) {
	t.Parallel()

	m := New[string, int]()
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("a", 100) // update, not insert

	if m.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (update must not add a key)", m.Len())
	}
	if got := slices.Collect(m.Keys()); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Keys = %v, want [a b]", got)
	}
	if v, _ := m.Get("a"); v != 100 {
		t.Fatalf("Get(a) = %d, want 100", v)
	}
}

func TestOrderedMapAllAgrees(t *testing.T) {
	t.Parallel()

	m := New[string, int]()
	m.Set("x", 10)
	m.Set("y", 20)
	m.Set("z", 30)

	var keys []string
	var vals []int
	for k, v := range m.All() {
		keys = append(keys, k)
		vals = append(vals, v)
	}
	if !slices.Equal(keys, slices.Collect(m.Keys())) {
		t.Fatalf("All keys %v disagree with Keys %v", keys, slices.Collect(m.Keys()))
	}
	if !slices.Equal(vals, slices.Collect(m.Values())) {
		t.Fatalf("All values %v disagree with Values %v", vals, slices.Collect(m.Values()))
	}
}

func TestOrderedMapBreak(t *testing.T) {
	t.Parallel()

	m := New[string, int]()
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("c", 3)

	var seen []string
	for k := range m.All() {
		if k == "b" {
			break
		}
		seen = append(seen, k)
	}
	if !slices.Equal(seen, []string{"a"}) {
		t.Fatalf("break did not stop iteration: saw %v, want [a]", seen)
	}
}
```

## Review

The API is correct when its names and shapes match the `maps` package and its order is deterministic. Confirm `All` is `iter.Seq2[K, V]` — key first, value second — so the contrast with the list's index-element `All` is explicit, and that `Keys` and `Values` are the two single-axis `iter.Seq` projections. The order tests are what justify the extra `keys` slice: drop it and iterate the bare map directly and the tests flake, because a plain Go map randomizes its iteration order. The update test pins the one piece of order logic worth getting wrong — `Set` must append to `keys` only when the key is absent, or a repeated `Set` of the same key duplicates it in the order and `Len` overcounts.

The common mistakes here are giving the keyed `All` the wrong shape or a bespoke name, and breaking the order invariant. Returning `iter.Seq[V]` from `All` (values only) throws away the key the caller needs and breaks the parallel with `maps.All`; naming it `Entries` forces a caller to relearn a verb. Appending to `keys` unconditionally in `Set` corrupts the order on the first update. Looking values up outside the shared `keys` walk — for instance ranging the bare map in `Values` — lets `Values` drift out of agreement with `All` and `Keys`, which the agreement test catches.

## Resources

- [`maps.All`, `maps.Keys`, `maps.Values`](https://pkg.go.dev/maps#All) — the exact signatures this ordered map mirrors for a keyed collection.
- [`maps.Collect`](https://pkg.go.dev/maps#Collect) — the eager sink that rebuilds a `map[K]V` from an `iter.Seq2[K, V]`, used in the demo.
- [`iter` package: Naming Conventions](https://pkg.go.dev/iter#hdr-Naming_Conventions) — why `All` returns `Seq2` and `Keys`/`Values` return `Seq`.

---

Back to [01-collection-iterators.md](01-collection-iterators.md) | Next: [03-fallible-iterators.md](03-fallible-iterators.md)
