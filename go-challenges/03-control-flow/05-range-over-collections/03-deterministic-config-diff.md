# Exercise 3: Deterministic Config Diff over maps.Keys and slices.Sorted

When a service reloads its configuration, you want to log exactly what changed —
and that log has to be stable, or you cannot diff two reload events or reproduce a
build. This module diffs an old and new `map[string]string` of settings into
sorted `Added`/`Removed`/`Changed` key lists, using `maps.Keys` + `slices.Sorted`
so the output is byte-identical every run. The whole lesson is here: never range a
map for user-facing output without imposing order.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
configdiff/                 independent module: example.com/configdiff
  go.mod                    go 1.24
  configdiff.go             type Diff; func Compute(old, new map[string]string) Diff
  cmd/
    demo/
      main.go               runnable demo: diff two config maps, print sorted result
  configdiff_test.go        table diff correctness + 50-run determinism test
```

- Files: `configdiff.go`, `cmd/demo/main.go`, `configdiff_test.go`.
- Implement: `Compute(old, new)` returning `Diff{Added, Removed, Changed []string}`, each sorted, using `maps.Keys` and `slices.Sorted`.
- Test: a table over old/new pairs asserting exact sorted slices, and a determinism test running the diff 50 times asserting identical output.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the sort is the whole point

The obvious way to compute this diff is to range both maps: a key in `new` but not
`old` is Added, a key in `old` but not `new` is Removed, a key in both whose value
differs is Changed. That logic is correct — but if you append to the result slices
*during* the map range, the slices come out in the map's randomized order, which
differs on every run. A test that asserts `Added == ["a","b"]` then flakes when the
range happens to visit `b` first, and a log line you diff across two deploys shows
spurious reordering.

The fix is to impose an order. `maps.Keys(m)` returns a lazy `iter.Seq[string]`
over the map's keys (in randomized order), and `slices.Sorted` collects that
iterator into a sorted slice. So `slices.Sorted(maps.Keys(m))` is the canonical
"give me this map's keys in stable order" idiom. Here we compute each category by
ranging the *sorted* key sequences, so the result slices are sorted by
construction. `Changed` requires a key present in both with differing values;
`Added`/`Removed` are set differences. Because `string` satisfies `cmp.Ordered`,
`slices.Sorted` sorts it without a comparison function.

Note `maps.Keys` returns an iterator, not a slice — you cannot index it. You must
`slices.Sorted` (or `slices.Collect`) it first. That is the trap the concepts file
warns about, and this exercise leans into the correct idiom.

Create `configdiff.go`:

```go
package configdiff

import (
	"maps"
	"slices"
)

// Diff is the stable, sorted set of key-level changes between two config maps.
type Diff struct {
	Added   []string // keys in new but not old
	Removed []string // keys in old but not new
	Changed []string // keys in both whose value differs
}

// Compute diffs old against new. Every result slice is sorted, so the output is
// deterministic regardless of map iteration order.
func Compute(old, new map[string]string) Diff {
	var d Diff

	// Added and Changed: walk new's keys in sorted order.
	for _, k := range slices.Sorted(maps.Keys(new)) {
		oldVal, ok := old[k]
		switch {
		case !ok:
			d.Added = append(d.Added, k)
		case oldVal != new[k]:
			d.Changed = append(d.Changed, k)
		}
	}

	// Removed: keys in old missing from new, in sorted order.
	for _, k := range slices.Sorted(maps.Keys(old)) {
		if _, ok := new[k]; !ok {
			d.Removed = append(d.Removed, k)
		}
	}

	return d
}
```

### The runnable demo

The demo diffs a small before/after config and prints each category, so you can
see the stable ordering.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configdiff"
)

func main() {
	old := map[string]string{
		"log.level":    "info",
		"timeout.ms":   "5000",
		"feature.beta": "off",
	}
	new := map[string]string{
		"log.level":  "debug", // changed
		"timeout.ms": "5000",  // same
		"max.conns":  "100",   // added
	}

	d := configdiff.Compute(old, new)
	fmt.Println("added:  ", d.Added)
	fmt.Println("removed:", d.Removed)
	fmt.Println("changed:", d.Changed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
added:   [max.conns]
removed: [feature.beta]
changed: [log.level]
```

### Tests

The table test pins the exact sorted slices for several old/new pairs, including an
add, a remove, a change, and a no-op. The determinism test runs `Compute` 50 times
on the same input and asserts every run produces an identical result — the guard
against someone "optimizing" the sort away and reintroducing raw map-range order.

Create `configdiff_test.go`:

```go
package configdiff

import (
	"reflect"
	"testing"
)

func TestComputeTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		old, new map[string]string
		want     Diff
	}{
		{
			name: "added removed changed",
			old:  map[string]string{"a": "1", "b": "2", "c": "3"},
			new:  map[string]string{"a": "1", "b": "9", "d": "4"},
			want: Diff{Added: []string{"d"}, Removed: []string{"c"}, Changed: []string{"b"}},
		},
		{
			name: "no change",
			old:  map[string]string{"x": "1", "y": "2"},
			new:  map[string]string{"x": "1", "y": "2"},
			want: Diff{},
		},
		{
			name: "all added from empty",
			old:  map[string]string{},
			new:  map[string]string{"m": "1", "k": "2"},
			want: Diff{Added: []string{"k", "m"}},
		},
		{
			name: "all removed to empty",
			old:  map[string]string{"m": "1", "k": "2"},
			new:  map[string]string{},
			want: Diff{Removed: []string{"k", "m"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute(tc.old, tc.new)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Compute() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestComputeIsDeterministic(t *testing.T) {
	t.Parallel()
	old := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	new := map[string]string{"a": "9", "c": "3", "e": "8", "f": "6", "g": "7"}

	first := Compute(old, new)
	for range 50 {
		got := Compute(old, new)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("non-deterministic output: %+v vs %+v", got, first)
		}
	}
	// Spot-check the sorted expectation too.
	want := Diff{
		Added:   []string{"f", "g"},
		Removed: []string{"b", "d"},
		Changed: []string{"a", "e"},
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("Compute() = %+v, want %+v", first, want)
	}
}
```

## Review

The diff is correct when Added/Removed/Changed partition the keys exactly (a
changed key is never also added or removed) and every slice is sorted. The failure
mode this exercise targets is not a logic bug but a determinism bug: appending
during a raw `for k := range m` loop compiles, passes a single-run test, then
flakes in CI and produces reordered logs in production. `slices.Sorted(maps.Keys(m))`
removes that class of bug at the source. Remember `maps.Keys` yields a lazy
iterator; you sort it into a slice before ranging, never index it directly.

## Resources

- [package maps (Keys)](https://pkg.go.dev/maps#Keys)
- [package slices (Sorted)](https://pkg.go.dev/slices#Sorted)
- [package cmp (Ordered)](https://pkg.go.dev/cmp#Ordered)
- [package iter (Seq)](https://pkg.go.dev/iter)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-cursor-pagination-iterator.md](02-cursor-pagination-iterator.md) | Next: [04-worker-pool-fan-in.md](04-worker-pool-fan-in.md)
