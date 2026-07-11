# Exercise 3: A Config Loader That Merges Defaults Without Nil-Map Panics

Layered config — defaults overlaid by a file, overlaid by env — is a place where
nil maps show up constantly: a missing config file yields a nil layer, an empty
env set yields another. This exercise builds a merge that reads nil layers
safely, never writes to a nil map, and returns an independent result so a caller
cannot mutate the shared defaults.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
config/                    independent module: example.com/config
  go.mod
  config.go                Merge, Resolve, Get over map[string]string layers
  cmd/
    demo/
      main.go              resolves defaults <- file <- env, prints sorted
  config_test.go           nil-safe merge, override wins, distinct result
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Merge(base, over)` returning a new independent map, `Resolve(defaults, file, env)` chaining merges, and `Get(m, key)` reading a possibly-nil map with comma-ok.
Test: merging into a nil base does not panic; the override layer wins; the result is a distinct map (mutating it does not change the defaults); nil/empty layers are no-ops; reading a nil map returns the zero value with `ok == false`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/config/cmd/demo
cd ~/go-exercises/config
go mod init example.com/config
```

## The nil-map hazard, and how maps.Clone/Copy defuse it

The panic this exercise guards against is "assignment to entry in nil map": a
nil map is fine to read but panics on write. In a merge, the intermediate result
is built by writing overrides into a base — so if the base arrives nil and you
write to it directly, you panic. The naive `for k, v := range over { base[k] = v
}` blows up precisely when `base` is nil, which is exactly the "no defaults yet"
case you most want to handle.

`maps.Clone` and `maps.Copy` are the clean tools, but they have a nil-related
subtlety you must handle. `maps.Clone(nil)` returns `nil` (it preserves nil-ness
rather than allocating), so cloning a nil base gives you a nil result — and then
`maps.Copy(nilResult, over)` would itself panic when `over` is non-empty. So the
pattern is: clone the base, and if the clone is nil, allocate an empty map before
copying into it. After that, `maps.Copy(out, over)` is safe even when `over` is
nil (copying *from* a nil map is a no-op). The result is always a freshly
allocated map distinct from every input, which is what makes it safe to hand to a
caller: mutating the merged config cannot reach back and corrupt the shared
`defaults` map that other requests read.

`Get` demonstrates the read side of the asymmetry directly: `v, ok := m[key]` on
a nil map returns the value type's zero value and `ok == false`, no panic — which
is why reading defaults that were never populated is safe while writing them is
not.

Create `config.go`:

```go
package config

import "maps"

// Merge returns a new map equal to base overlaid with over. Both layers may be
// nil; the result is always a freshly allocated, independent map, so a caller
// can mutate it without affecting base or over.
func Merge(base, over map[string]string) map[string]string {
	out := maps.Clone(base) // Clone(nil) returns nil
	if out == nil {
		out = make(map[string]string)
	}
	maps.Copy(out, over) // Copy from a nil map is a no-op
	return out
}

// Resolve layers defaults, then file overrides, then env overrides, later
// layers winning. Any layer may be nil.
func Resolve(defaults, file, env map[string]string) map[string]string {
	return Merge(Merge(defaults, file), env)
}

// Get reads key from a possibly-nil map. A nil map yields the zero value and
// ok == false without panicking.
func Get(m map[string]string, key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}
```

## The runnable demo

The demo resolves three layers and prints the result in sorted key order (map
iteration order is unspecified, so sort for a stable demo). It shows the env
layer winning over the file layer, which wins over defaults.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/config"
)

func main() {
	defaults := map[string]string{"log": "info", "port": "8080"}
	file := map[string]string{"port": "9090"}
	env := map[string]string{"log": "debug"}

	res := config.Resolve(defaults, file, env)
	for _, k := range slices.Sorted(maps.Keys(res)) {
		fmt.Printf("%s=%s\n", k, res[k])
	}

	// defaults is untouched by the merge.
	fmt.Printf("defaults.port still %s\n", defaults["port"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
log=debug
port=9090
defaults.port still 8080
```

## Tests

`TestMergeIntoNilBase` proves the panic case is handled: merging into a nil base
returns the override layer, not a crash. `TestOverrideWins` pins the precedence.
`TestResultIsIndependent` mutates the merged map and asserts the defaults are
untouched — the "distinct map" contract. `TestNilLayersAreNoOps` checks that nil
and empty layers contribute nothing. `TestGetFromNil` proves the read-from-nil
zero-value/ok semantics.

Create `config_test.go`:

```go
package config

import (
	"maps"
	"testing"
)

func TestMergeIntoNilBase(t *testing.T) {
	t.Parallel()

	var base map[string]string // nil
	got := Merge(base, map[string]string{"a": "1"})
	if !maps.Equal(got, map[string]string{"a": "1"}) {
		t.Fatalf("Merge(nil, {a:1}) = %v, want {a:1}", got)
	}
}

func TestOverrideWins(t *testing.T) {
	t.Parallel()

	got := Merge(
		map[string]string{"port": "8080", "log": "info"},
		map[string]string{"port": "9090"},
	)
	if got["port"] != "9090" || got["log"] != "info" {
		t.Fatalf("Merge = %v, want port=9090 log=info", got)
	}
}

func TestResultIsIndependent(t *testing.T) {
	t.Parallel()

	defaults := map[string]string{"port": "8080"}
	got := Merge(defaults, nil)
	got["port"] = "1"
	got["new"] = "x"

	if defaults["port"] != "8080" {
		t.Fatalf("defaults mutated: port = %s, want 8080", defaults["port"])
	}
	if _, ok := defaults["new"]; ok {
		t.Fatal("defaults gained a key from mutating the merged result")
	}
}

func TestNilLayersAreNoOps(t *testing.T) {
	t.Parallel()

	got := Resolve(map[string]string{"a": "1"}, nil, nil)
	if !maps.Equal(got, map[string]string{"a": "1"}) {
		t.Fatalf("Resolve with nil layers = %v, want {a:1}", got)
	}
}

func TestGetFromNil(t *testing.T) {
	t.Parallel()

	var m map[string]string // nil
	if v, ok := Get(m, "missing"); v != "" || ok {
		t.Fatalf("Get(nil, missing) = %q,%v; want \"\",false", v, ok)
	}
}
```

## Review

The merge is correct when it never panics on a nil layer and never returns a map
that aliases an input. The two failure modes are symmetric: forgetting the `if
out == nil { out = make(...) }` guard reintroduces the nil-map-write panic when
the base is nil, and returning `base` itself (instead of a clone) lets a caller
mutate the shared defaults every other request reads. `maps.Clone` plus
`maps.Copy` gives you both properties cleanly, provided you handle
`Clone(nil) == nil`. Do not rely on map iteration order anywhere — the demo sorts
keys precisely because ranging a map is unordered. Run `go test -race` since a
resolver like this is typically read concurrently.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — returns a shallow copy; `Clone(nil)` returns nil.
- [`maps.Copy`](https://pkg.go.dev/maps#Copy) — copies entries; copying from nil is a no-op.
- [Go maps in action](https://go.dev/blog/maps) — nil-map read vs write semantics.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-absent-vs-zero-patch-handler.md](02-absent-vs-zero-patch-handler.md) | Next: [04-atomic-zero-value-counters.md](04-atomic-zero-value-counters.md)
