# Exercise 1: A Map Transform Pipeline

The everyday map chores in a service — filter a bucket map down to the live
entries, normalize header keys, invert a lookup table, emit keys in a stable
order — are exactly where the `maps` and `slices` packages earn their place. This
module builds a small transform pipeline and pins the one property that separates
a correct transform from a bug: it never mutates the caller's map.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
mappipe/                    independent module: example.com/mappipe
  go.mod                    go 1.26
  mappipe.go                FilterPositive, UppercaseKeys, Invert, SortedKeys
  cmd/
    demo/
      main.go               runs each transform and prints the results
  mappipe_test.go           table tests + the input-not-modified property + nil/empty contract
```

Files: `mappipe.go`, `cmd/demo/main.go`, `mappipe_test.go`.
Implement: `FilterPositive`, `UppercaseKeys`, `Invert`, `SortedKeys`.
Test: each transform's output, the independent-copy property, the nil/empty contract, the duplicate-value collapse of `Invert`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mappipe/cmd/demo
cd ~/go-exercises/mappipe
go mod init example.com/mappipe
```

## Why the pipeline is shaped this way

`FilterPositive` is the load-bearing function. It clones the input with
`maps.Clone`, then removes the non-positive entries in place with
`maps.DeleteFunc`. Doing it in that order is the whole point: the caller's map is
never touched, because every deletion lands on the clone. If instead you called
`maps.DeleteFunc` on the argument directly you would silently prune the caller's
map — a data-corruption bug that surfaces far from its cause. The independent-copy
test below proves the contract by mutating the result and asserting the input is
unchanged.

`UppercaseKeys` cannot be done in place at all: renaming keys means building a new
map, because a key rename is a delete plus an insert and doing both while ranging
the same map you are inserting into is asking for trouble. It allocates a
right-sized `map[string]int` (capacity `len(m)`) and copies each entry under its
upper-cased key. This is the header-normalization pattern — HTTP header keys are
canonicalized before lookup so `content-type` and `Content-Type` collide
correctly.

`Invert` swaps keys and values, turning a `map[string]int` into a
`map[int]string`. It is honest about a limitation: if two keys share a value, the
inverted map keeps only one of them — last writer wins — because a map cannot hold
two entries for the same key. The test documents this collapse rather than
pretending it does not happen; when you must keep every colliding key you invert
into a `map[int][]string` instead.

`SortedKeys` is the determinism primitive. `maps.Keys(m)` returns an
`iter.Seq[string]` in randomized order; `slices.Sorted` consumes that iterator and
returns a lexicographically sorted `[]string` in one call. This replaces the
hand-rolled insertion sort the original lesson carried — `slices.Sorted` is the
idiom and it is correct by construction. Any code that needs a stable ordering of
a map's keys (logs, canonical output, pagination) starts here.

Create `mappipe.go`:

```go
package mappipe

import (
	"maps"
	"slices"
	"strings"
)

// FilterPositive returns a new map containing only the entries whose value is
// greater than zero. The input map is never modified: it is cloned first, then
// the non-positive entries are deleted from the clone in place.
func FilterPositive(m map[string]int) map[string]int {
	out := maps.Clone(m)
	if out == nil {
		out = make(map[string]int)
	}
	maps.DeleteFunc(out, func(_ string, v int) bool {
		return v <= 0
	})
	return out
}

// UppercaseKeys returns a new map with every key upper-cased. This cannot be an
// in-place transform: renaming a key is a delete plus an insert.
func UppercaseKeys(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[strings.ToUpper(k)] = v
	}
	return out
}

// Invert swaps keys and values. If two keys share a value the inverted map keeps
// only one of them (last writer wins), since a map holds one entry per key.
func Invert(m map[string]int) map[int]string {
	out := make(map[int]string, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// SortedKeys returns the map's keys in lexicographic order. maps.Keys yields an
// iterator in randomized order; slices.Sorted materializes it sorted.
func SortedKeys(m map[string]int) []string {
	return slices.Sorted(maps.Keys(m))
}
```

`maps.Clone(nil)` returns `nil`, so `FilterPositive` guards that case and returns
a non-nil empty map — callers should never have to distinguish "no positive
entries" from "nil map."

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mappipe"
)

func main() {
	buckets := map[string]int{"a": 1, "b": -2, "c": 0, "d": 3}

	live := mappipe.FilterPositive(buckets)
	fmt.Printf("filtered keys: %v\n", mappipe.SortedKeys(live))
	fmt.Printf("input intact:  %v\n", mappipe.SortedKeys(buckets))

	headers := map[string]int{"content-length": 12, "x-trace": 7}
	fmt.Printf("upper keys:    %v\n", mappipe.SortedKeys(mappipe.UppercaseKeys(headers)))

	inv := mappipe.Invert(map[string]int{"a": 1, "b": 2, "c": 3})
	fmt.Printf("inverted[2]:   %s\n", inv[2])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
filtered keys: [a d]
input intact:  [a b c d]
upper keys:    [CONTENT-LENGTH X-TRACE]
inverted[2]:   b
```

### Tests

The tests pin each transform's output and, above all, the property that
`FilterPositive` returns an independent copy. `TestFilterPositiveContract` folds
in the nil-and-empty case: both `nil` and an empty map must return a non-nil empty
map. `TestInvertCollapsesDuplicates` documents the last-writer-wins behavior so it
is a known contract, not a surprise.

Create `mappipe_test.go`:

```go
package mappipe

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestFilterPositive(t *testing.T) {
	t.Parallel()

	in := map[string]int{"a": 1, "b": -2, "c": 0, "d": 3}
	got := FilterPositive(in)
	want := map[string]int{"a": 1, "d": 3}
	if !maps.Equal(got, want) {
		t.Fatalf("FilterPositive() = %v, want %v", got, want)
	}
}

func TestFilterPositiveReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	in := map[string]int{"a": 1, "b": -2}
	got := FilterPositive(in)
	got["c"] = 100
	if _, ok := in["c"]; ok {
		t.Fatal("mutating the result leaked into the input map")
	}
}

func TestFilterPositiveContract(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]map[string]int{
		"nil":   nil,
		"empty": {},
	} {
		got := FilterPositive(in)
		if got == nil {
			t.Errorf("%s: FilterPositive returned nil, want non-nil empty map", name)
		}
		if len(got) != 0 {
			t.Errorf("%s: FilterPositive returned %v, want empty", name, got)
		}
	}
}

func TestUppercaseKeys(t *testing.T) {
	t.Parallel()

	got := UppercaseKeys(map[string]int{"hello": 1, "world": 2})
	want := map[string]int{"HELLO": 1, "WORLD": 2}
	if !maps.Equal(got, want) {
		t.Fatalf("UppercaseKeys() = %v, want %v", got, want)
	}
}

func TestInvertSwapsKeysAndValues(t *testing.T) {
	t.Parallel()

	got := Invert(map[string]int{"a": 1, "b": 2, "c": 3})
	want := map[int]string{1: "a", 2: "b", 3: "c"}
	if !maps.Equal(got, want) {
		t.Fatalf("Invert() = %v, want %v", got, want)
	}
}

func TestInvertCollapsesDuplicates(t *testing.T) {
	t.Parallel()

	got := Invert(map[string]int{"a": 1, "b": 1})
	if len(got) != 1 {
		t.Fatalf("Invert with duplicate values kept %d entries, want 1 (last-writer-wins)", len(got))
	}
}

func TestSortedKeys(t *testing.T) {
	t.Parallel()

	got := SortedKeys(map[string]int{"c": 1, "a": 2, "b": 3})
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("SortedKeys() = %v, want %v", got, want)
	}
}

func ExampleSortedKeys() {
	fmt.Println(SortedKeys(map[string]int{"z": 1, "a": 2, "m": 3}))
	// Output: [a m z]
}
```

## Review

The correctness of this pipeline rests on one invariant: a transform returns a new
map and never mutates its argument. `FilterPositive` gets that from
clone-then-delete; the independent-copy test is the proof and must never be
deleted. The nil/empty contract matters because callers should be able to range
the result unconditionally — returning `nil` from `FilterPositive(nil)` would push
a nil check onto every caller. `SortedKeys` is the determinism seam: it uses
`slices.Sorted(maps.Keys(m))` rather than ranging the map, so its output is stable
regardless of insertion order. If you ever see a flaky test that compares
`SortedKeys` output, the fault is not here — it is code elsewhere that ranged a
map directly. Run `go test -race` to confirm.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Clone`, `DeleteFunc`, `Keys`.
- [slices package](https://pkg.go.dev/slices) — `Collect`, `Sorted`, `Equal`.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — why `maps.Keys` returns an iterator.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-deterministic-map-serialization.md](02-deterministic-map-serialization.md)
