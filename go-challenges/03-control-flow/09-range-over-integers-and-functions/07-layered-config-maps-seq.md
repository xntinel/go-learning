# Exercise 7: Config Loader — Deterministic Merge with `maps.All`, `maps.Insert`, `slices.Sorted`

A config resolver layers defaults under file under env under flags, later layers
winning, and it must produce a *reproducible* dump for diffing and logging. This
exercise builds that resolver with the Go 1.24 map iterators — `maps.Insert(dst,
maps.All(src))` for override semantics — and a `DumpSorted` that yields keys in
deterministic order via `slices.Sorted(maps.Keys(...))`. The central lesson: map
iteration order is unspecified, so anything serialized must be sorted first.

## What you'll build

```text
config/                   independent module: example.com/config
  go.mod                  module example.com/config
  config.go               MergeLayers, DumpSorted
  cmd/
    demo/
      main.go             runnable demo: merge layers, dump sorted
  config_test.go          precedence, determinism, nil-layer, round-trip tests
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `MergeLayers(layers ...map[string]string) map[string]string` using `maps.Insert(out, maps.All(layer))`, and `DumpSorted(cfg) iter.Seq2[string, string]` ordered by `slices.Sorted(maps.Keys(cfg))`.
Test: later layers override earlier, disjoint keys union; the dump is byte-identical across many runs; nil layers are handled; `maps.Collect(DumpSorted(cfg))` round-trips.
Verify: `go test -count=1 -race ./...`

## The design

`MergeLayers` takes layers in increasing precedence order — `MergeLayers(defaults,
file, env, flags)` — and folds them into a fresh map. Each layer is inserted with
`maps.Insert(out, maps.All(layer))`: `maps.All(layer)` is an `iter.Seq2[string,
string]` over that layer's entries, and `maps.Insert` writes each pair into `out`,
overwriting any existing key. Because later layers are inserted last, their values
win — which is exactly precedence. Starting from a fresh `out` (rather than
mutating the first layer) keeps the inputs immutable, which matters when the
defaults map is shared.

`DumpSorted` is where the map-order lesson lands. `maps.Keys(cfg)` yields keys in
a deliberately randomized order, so a dump that ranged the map directly would emit
a different line order on every run — flaky golden files, noisy diffs, unstable
logs. `slices.Sorted(maps.Keys(cfg))` collects the keys and sorts them (they are
`cmp.Ordered` strings), and `DumpSorted` yields `(key, value)` in that stable
order as an `iter.Seq2[string, string]`. The determinism test runs the dump many
times and asserts byte-identical output — the property production depends on for
reproducible config snapshots.

`maps.Collect` closes the loop: it consumes an `iter.Seq2[K, V]` back into a
`map[K]V`, so `maps.Collect(DumpSorted(cfg))` reconstructs the original map,
proving the dump is a faithful, lossless view.

Create `config.go`:

```go
package config

import (
	"iter"
	"maps"
	"slices"
)

// MergeLayers folds layers into one map, later layers overriding earlier ones.
// Precedence increases left to right: MergeLayers(defaults, file, env, flags).
func MergeLayers(layers ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, layer := range layers {
		maps.Insert(out, maps.All(layer))
	}
	return out
}

// DumpSorted yields cfg's entries in ascending key order, so serialized or
// diffed output is deterministic despite randomized map iteration.
func DumpSorted(cfg map[string]string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, k := range slices.Sorted(maps.Keys(cfg)) {
			if !yield(k, cfg[k]) {
				return
			}
		}
	}
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/config"
)

func main() {
	defaults := map[string]string{"host": "localhost", "port": "8080", "log": "info"}
	env := map[string]string{"port": "9090"}
	flags := map[string]string{"log": "debug"}

	cfg := config.MergeLayers(defaults, env, flags)
	for k, v := range config.DumpSorted(cfg) {
		fmt.Printf("%s=%s\n", k, v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=localhost
log=debug
port=9090
```

## Tests

Create `config_test.go`:

```go
package config

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

func dump(cfg map[string]string) string {
	var b strings.Builder
	for k, v := range DumpSorted(cfg) {
		fmt.Fprintf(&b, "%s=%s;", k, v)
	}
	return b.String()
}

func TestLaterLayerOverrides(t *testing.T) {
	t.Parallel()

	base := map[string]string{"host": "localhost", "port": "8080"}
	override := map[string]string{"port": "9090", "tls": "on"}

	got := MergeLayers(base, override)
	want := map[string]string{"host": "localhost", "port": "9090", "tls": "on"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestDumpIsDeterministic(t *testing.T) {
	t.Parallel()

	cfg := MergeLayers(map[string]string{
		"z": "1", "a": "2", "m": "3", "b": "4", "k": "5",
	})

	first := dump(cfg)
	if first != "a=2;b=4;k=5;m=3;z=1;" {
		t.Fatalf("dump = %q, want sorted order", first)
	}
	for range 100 {
		if got := dump(cfg); got != first {
			t.Fatalf("dump not deterministic: %q != %q", got, first)
		}
	}
}

func TestNilLayerHandled(t *testing.T) {
	t.Parallel()

	got := MergeLayers(nil, map[string]string{"a": "1"}, nil)
	want := map[string]string{"a": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestCollectRoundTrips(t *testing.T) {
	t.Parallel()

	cfg := map[string]string{"a": "1", "b": "2", "c": "3"}
	got := maps.Collect(DumpSorted(cfg))
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("round-trip = %v, want %v", got, cfg)
	}
}
```

## Review

`MergeLayers` is correct when overlapping keys take the last layer's value and
disjoint keys union — the precedence test pins both — and when `nil` layers are
inert (`maps.All(nil)` yields nothing). `DumpSorted` is correct when its output is
byte-identical across runs; the determinism test hammers it a hundred times
because a direct `for k := range cfg` would pass once and flake later. `maps.Insert`
mutates its destination in place, which is why `MergeLayers` builds into a fresh
`out` rather than the caller's first layer, and `maps.Collect(DumpSorted(cfg))`
round-tripping proves the sorted view loses nothing.

## Resources

- [`maps` package (All, Keys, Insert, Collect)](https://pkg.go.dev/maps)
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted)
- [`iter` package documentation](https://pkg.go.dev/iter)
- [Go 1.24 release notes](https://go.dev/doc/go1.24)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-batching-iterator-bulk-writer.md](06-batching-iterator-bulk-writer.md) | Next: [08-retry-backoff-seq.md](08-retry-backoff-seq.md)
