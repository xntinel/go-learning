# Exercise 2: iter.Seq2 Key/Value Iterators

`iter.Seq2[K, V]` is the pair form of a push iterator: each value it pushes is
two values, ranged as `for k, v := range seq`. This exercise builds an
index/value `Enumerate` over a slice and a deterministic, key-sorted iterator
over a map -- the two shapes you reach for whenever a sequence is naturally
keyed.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
seq2.go              iter.Seq2 producers: Enumerate, Sorted (key-ordered map walk)
cmd/
  demo/
    main.go          enumerate a slice, walk a map in sorted key order
seq2_test.go         index/value pairs, deterministic sorted order, early-break
```

- Files: `seq2.go`, `cmd/demo/main.go`, `seq2_test.go`.
- Implement: `Enumerate[V any](s []V) iter.Seq2[int, V]` and `Sorted[K
  cmp.Ordered, V any](m map[K]V) iter.Seq2[K, V]`, both honoring the yield
  protocol.
- Test: `seq2_test.go` checks `Enumerate` produces `(index, value)` pairs, checks
  `Sorted` walks keys in ascending order regardless of map layout, and breaks
  early out of a `Seq2`.
- Verify: `go test -run TestSeq2 -race ./...`

Set up the module:

```bash
mkdir -p seq2-key-value-iterators/cmd/demo && cd seq2-key-value-iterators
go mod init example.com/seq2-kv
```

### Why a pair shape, and how its yield protocol differs

`iter.Seq2[K, V]` is `func(yield func(K, V) bool)`. It is the same machinery as
`iter.Seq[V]` with one more value per push, and it backs the two-variable form of
range: `for k, v := range seq` compiles to `seq(yield)` where `yield` takes two
arguments. The bool contract is identical -- `yield(k, v)` returns `false` when
the consumer is done, and the iterator must return on the next `false`. Use the
pair shape for any sequence whose elements are naturally two things: index and
value, key and value, or a value paired with an error. It is not "the map type";
`Enumerate` produces `(int, V)` pairs from a flat slice and uses `Seq2` precisely
because the index is meaningful.

`Enumerate` is the smaller piece. It mirrors the way `for i, v := range slice`
already works, but as a reusable iterator value you can pass around and compose:

```go
return func(yield func(int, V) bool) {
	for i, v := range s {
		if !yield(i, v) {
			return
		}
	}
}
```

### Determinism: walking a map in sorted key order

Ranging a Go map directly is intentionally randomized -- the runtime perturbs the
order so no code accidentally depends on it. That is the right default but the
wrong behavior when you want reproducible output, a stable diff, or a test whose
expected value is fixed. `Sorted` fixes the order by collecting the keys, sorting
them, and yielding `(k, m[k])` in that order. The clean way to collect-and-sort
is to compose standard-library iterators:

```go
for _, k := range slices.Sorted(maps.Keys(m)) {
	if !yield(k, m[k]) {
		return
	}
}
```

`maps.Keys(m)` returns an `iter.Seq[K]` over the map's keys, and
`slices.Sorted` consumes any `iter.Seq[K]` of an ordered type and returns a
sorted `[]K`. This is composition in miniature: one iterator feeds another, and
because the key type is constrained by `cmp.Ordered`, `slices.Sorted` knows how
to compare. The result is a `Seq2` whose order is fully determined by the keys,
so the same map always produces the same sequence -- which is exactly what makes
the demo's expected output and the test's expected slice legitimate to write
down.

Create `seq2.go`:

```go
// Package seq2kv builds two-value push iterators (iter.Seq2) over slices and
// over maps walked in deterministic, sorted key order.
package seq2kv

import (
	"cmp"
	"iter"
	"maps"
	"slices"
)

// Enumerate returns an iterator over (index, value) pairs of s, mirroring the
// two-variable form of `for i, v := range s` as a reusable iterator value.
func Enumerate[V any](s []V) iter.Seq2[int, V] {
	return func(yield func(int, V) bool) {
		for i, v := range s {
			if !yield(i, v) {
				return
			}
		}
	}
}

// Sorted returns an iterator over (key, value) pairs of m, with keys visited in
// ascending order. It composes maps.Keys and slices.Sorted so the sequence is
// deterministic regardless of the map's internal layout.
func Sorted[K cmp.Ordered, V any](m map[K]V) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo enumerates a slice of names to show the index travelling alongside the
value, then walks a map twice through `Sorted` to show the order is stable across
runs even though a direct `range` over the same map would not be.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/seq2-kv"
)

func main() {
	for i, name := range seq2kv.Enumerate([]string{"alice", "bob", "carol"}) {
		fmt.Printf("%d=%s ", i, name)
	}
	fmt.Println()

	scores := map[string]int{"carol": 91, "alice": 70, "bob": 85}
	for k, v := range seq2kv.Sorted(scores) {
		fmt.Printf("%s:%d ", k, v)
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
0=alice 1=bob 2=carol 
alice:70 bob:85 carol:91 
```

### Tests

`TestSeq2Enumerate` collects the `(index, value)` pairs and checks both
coordinates. `TestSeq2Sorted` builds a map with keys out of order and asserts the
iterator visits them ascending, the property that direct map ranging does not
give. `TestSeq2EarlyBreak` breaks after the first pair and asserts the iterator
stopped -- the `Seq2` form of the same yield protocol.

Create `seq2_test.go`:

```go
package seq2kv

import (
	"slices"
	"testing"
)

func TestSeq2Enumerate(t *testing.T) {
	t.Parallel()

	var idx []int
	var val []string
	for i, v := range Enumerate([]string{"x", "y", "z"}) {
		idx = append(idx, i)
		val = append(val, v)
	}
	if want := []int{0, 1, 2}; !slices.Equal(idx, want) {
		t.Fatalf("indexes = %v, want %v", idx, want)
	}
	if want := []string{"x", "y", "z"}; !slices.Equal(val, want) {
		t.Fatalf("values = %v, want %v", val, want)
	}
}

func TestSeq2Sorted(t *testing.T) {
	t.Parallel()

	m := map[string]int{"delta": 4, "alpha": 1, "charlie": 3, "bravo": 2}
	var keys []string
	var vals []int
	for k, v := range Sorted(m) {
		keys = append(keys, k)
		vals = append(vals, v)
	}
	if want := []string{"alpha", "bravo", "charlie", "delta"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if want := []int{1, 2, 3, 4}; !slices.Equal(vals, want) {
		t.Fatalf("vals = %v, want %v", vals, want)
	}
}

func TestSeq2EarlyBreak(t *testing.T) {
	t.Parallel()

	m := map[string]int{"a": 1, "b": 2, "c": 3}
	var seen int
	for k := range Sorted(m) {
		seen++
		if k == "a" {
			break
		}
	}
	if seen != 1 {
		t.Fatalf("visited %d pairs before break, want 1", seen)
	}
}
```

## Review

The pair iterators are correct when the index/key coordinate is meaningful and
the order is what you promised. For `Enumerate` that means the first value is the
slice index, checked by `TestSeq2Enumerate`. For `Sorted` it means the keys come
out ascending no matter how the map is laid out internally, checked by
`TestSeq2Sorted` with deliberately out-of-order keys -- the reason a fixed
expected slice is even writable. Confirm `TestSeq2EarlyBreak` stops after one
pair, proving the bool protocol works identically in the two-value form. All
three passing under `go test -race ./...` establishes determinism and the
protocol together.

Common mistakes for this feature. The first is assuming `Seq2` is only for maps
and forcing index/value sequences into `Seq[struct{...}]`; the pair shape exists
for any two-coordinate sequence, including a slice index. The second is yielding
a map by ranging it directly and expecting stable order; Go randomizes map
iteration on purpose, so any deterministic walk must collect and sort the keys
first, which is what composing `maps.Keys` with `slices.Sorted` does. The third
is forgetting that the bool protocol is unchanged in the pair form -- both
coordinates go in, one bool comes back, and the iterator must still return on
`false`.

## Resources

- [`iter` package](https://pkg.go.dev/iter) -- `Seq2[K, V]` and the documented
  two-value yield contract.
- [`maps` package](https://pkg.go.dev/maps) -- `maps.Keys` returning an
  `iter.Seq[K]` over a map's keys.
- [`slices` package](https://pkg.go.dev/slices) -- `slices.Sorted`, which
  consumes an `iter.Seq` of an ordered type and returns a sorted slice.

---

Back to [01-seq-and-the-yield-protocol.md](01-seq-and-the-yield-protocol.md) | Next: [03-tree-in-order-iterator.md](03-tree-in-order-iterator.md)
