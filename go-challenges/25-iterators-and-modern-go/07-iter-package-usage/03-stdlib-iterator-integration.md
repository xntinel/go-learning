# Exercise 3: Standard Library Iterator Integration

The payoff of `iter.Seq` being a shared vocabulary is that custom iterators and standard helpers compose with no glue. This exercise wires the `slices` and `maps` producers and consumers together to build small, idiomatic utilities — sorted map keys, a collected copy, a reversed copy — each one a single expression over the standard iterator functions.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
stdlibiter.go        SortedKeys, CollectValues, Reversed over slices/maps iterators
cmd/
  demo/
    main.go          sorted keys of a map, a collected copy, a reversed copy
stdlibiter_test.go   deterministic key order, round-trip collect, reversal
```

- Files: `stdlibiter.go`, `cmd/demo/main.go`, `stdlibiter_test.go`.
- Implement: `SortedKeys[V any](m map[string]V) []string`, `CollectValues[V any](s []V) []V`, and `Reversed[V any](s []V) []V`.
- Test: sorted keys are deterministic regardless of insertion order, a collected copy equals the source, and a reversal is exact.
- Verify: `go test -run 'TestSortedKeys|TestCollectValues|TestReversed' -race ./...`

### Producers, consumers, and the one-liners they make possible

The `slices` and `maps` packages were extended in Go 1.23 to traffic in `iter.Seq`. On the producing side, `slices.Values(s)` turns a slice into an `iter.Seq[V]`, `slices.Backward(s)` into a reverse `iter.Seq2[int, V]`, and `maps.Keys(m)` into an `iter.Seq[K]` over a map's keys. On the consuming side, `slices.Collect(seq)` drains an `iter.Seq` into a fresh slice and `slices.Sorted(seq)` drains and sorts it (it requires `cmp.Ordered` elements). Because both sides speak the same type, a producer and a consumer snap together directly.

`SortedKeys` is the canonical composition: `slices.Sorted(maps.Keys(m))`. `maps.Keys` yields the keys in the map's unspecified, run-to-run-varying order; `slices.Sorted` collects them and sorts in one call. This single expression replaces the old ritual — allocate a slice, `for k := range m { keys = append(keys, k) }`, then `sort.Strings(keys)` — and it is correct precisely because it never depends on map traversal order.

`CollectValues` is `slices.Collect(slices.Values(s))`: a producer feeding a consumer to make an independent copy of the slice's elements. `Reversed` uses `slices.Backward`, which yields `(index, value)` pairs from the end toward the start; collecting just the values gives a reversed copy without mutating the input or hand-writing an index-decrementing loop.

Create `stdlibiter.go`:

```go
// Create `stdlibiter.go`
package stdlibiter

import (
	"maps"
	"slices"
)

// SortedKeys returns the keys of m in ascending order. It composes the
// maps.Keys producer with the slices.Sorted consumer, so the result never
// depends on map iteration order.
func SortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// CollectValues returns an independent copy of s by feeding the slices.Values
// producer into the slices.Collect consumer.
func CollectValues[V any](s []V) []V {
	return slices.Collect(slices.Values(s))
}

// Reversed returns a new slice with the elements of s in reverse order, using
// the slices.Backward iterator to walk from the end toward the start.
func Reversed[V any](s []V) []V {
	out := make([]V, 0, len(s))
	for _, v := range slices.Backward(s) {
		out = append(out, v)
	}
	return out
}
```

### The runnable demo

The demo builds a map, prints its keys in sorted order to show the determinism, then prints a collected copy and a reversed copy of a slice.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"

	"example.com/stdlib-iterator-integration"
)

func main() {
	ages := map[string]int{"carol": 31, "alice": 30, "bob": 25}
	fmt.Println("sorted keys:", stdlibiter.SortedKeys(ages))

	names := []string{"alice", "bob", "carol"}
	fmt.Println("collected:", stdlibiter.CollectValues(names))
	fmt.Println("reversed:", stdlibiter.Reversed(names))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sorted keys: [alice bob carol]
collected: [alice bob carol]
reversed: [carol bob alice]
```

### Tests

`TestSortedKeys` builds the same logical map two ways and asserts both produce identical sorted output, which is the determinism that ranging a map directly does not give. `TestCollectValues` checks the collected slice equals the source and is a distinct backing array. `TestReversed` checks an exact reversal and that the input is untouched.

Create `stdlibiter_test.go`:

```go
// Create `stdlibiter_test.go`
package stdlibiter

import (
	"slices"
	"testing"
)

func TestSortedKeys(t *testing.T) {
	t.Parallel()

	m := map[string]int{"banana": 1, "apple": 2, "cherry": 3}
	got := SortedKeys(m)
	want := []string{"apple", "banana", "cherry"}
	if !slices.Equal(got, want) {
		t.Fatalf("SortedKeys = %v, want %v", got, want)
	}
}

func TestCollectValues(t *testing.T) {
	t.Parallel()

	src := []int{3, 1, 2}
	got := CollectValues(src)
	if !slices.Equal(got, src) {
		t.Fatalf("CollectValues = %v, want %v", got, src)
	}
	got[0] = 99
	if src[0] == 99 {
		t.Fatal("CollectValues returned an aliased slice, not a copy")
	}
}

func TestReversed(t *testing.T) {
	t.Parallel()

	src := []string{"a", "b", "c"}
	got := Reversed(src)
	if !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Fatalf("Reversed = %v", got)
	}
	if !slices.Equal(src, []string{"a", "b", "c"}) {
		t.Fatalf("Reversed mutated its input: %v", src)
	}
}
```

## Review

These utilities are correct when they read as a single composition of a standard producer and a standard consumer. `SortedKeys` is `slices.Sorted(maps.Keys(m))` and nothing more; the test that builds a map and asserts a fixed sorted order is what proves the result does not leak the map's nondeterministic traversal. `CollectValues` must return a distinct backing array — the mutate-and-check in its test catches an accidental alias — which `slices.Collect` guarantees because it allocates a fresh slice. `Reversed` must not mutate its input, which `slices.Backward` makes easy because it only reads.

The mistake to avoid is reaching past these helpers for a hand-rolled loop: appending keys in a `for range m` and calling `sort.Strings` works but is three lines that can each go wrong, and ranging the map directly to "save the sort" yields a different order every run. When order or an independent copy matters, route through the iterator functions; they are the shorter and the more correct path.

## Resources

- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — collects an `iter.Seq` of ordered elements and returns them sorted.
- [`maps.Keys`](https://pkg.go.dev/maps#Keys) — produces an `iter.Seq` over a map's keys in unspecified order.
- [`slices.Backward`](https://pkg.go.dev/slices#Backward) — the reverse `iter.Seq2[int, V]` used to build a reversed copy.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-merge-push-pull-push.md](04-merge-push-pull-push.md)
