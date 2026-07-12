# Exercise 3: Layered Config Loader

Every service resolves configuration the same way: start from built-in defaults,
overlay a config file, then let environment variables win on top. That is a map
merge with precedence, and it has exactly one subtle bug — mutating an input layer
during the merge. This module builds the merge the correct way, cloning the base
before copying, and proves no input layer is ever touched.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
configmerge/                independent module: example.com/configmerge
  go.mod                    go 1.26
  configmerge.go            MergeConfig(layers ...map[string]string) map[string]string
  cmd/
    demo/
      main.go               defaults <- file <- env, printed in precedence order
  configmerge_test.go       precedence, input-immutability, nil/empty, zero-layers
```

Files: `configmerge.go`, `cmd/demo/main.go`, `configmerge_test.go`.
Implement: `MergeConfig(layers ...map[string]string) map[string]string`.
Test: later layers win; no input layer is mutated; nil and empty layers are no-ops; zero layers yields a non-nil empty map.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/03-layered-config-merge/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/03-layered-config-merge
```

## Why clone-then-copy, and never copy-into-input

`maps.Copy(dst, src)` writes every entry of `src` into `dst` in place, and on a
key collision `src` silently overwrites. That silent-overwrite-with-precedence is
exactly the semantics config merging wants: later layers should win. The danger is
purely about *which map you copy into*.

The naive merge is `maps.Copy(base, fileLayer)` — copy the file layer into the
defaults. It produces the right merged values, but it has mutated `base`, which is
almost always a shared package-level `defaults` map. The next caller that reads
`defaults` now sees the first caller's file overrides baked in. This is a genuinely
nasty bug: it depends on call order, it is invisible in a single-request test, and
it corrupts a value that looked immutable.

The fix is to never merge into an input. `MergeConfig` starts from a fresh empty
map — an accumulator that shares nothing with any argument — and copies every
layer in order into it with `maps.Copy`. Every write lands on the private
accumulator; no argument is ever modified. The result is a new map the caller owns
outright.

Two edge cases round out the contract. A `nil` layer (a config file that was
absent) must be a no-op, not a panic — `maps.Copy(dst, nil)` copies zero entries,
so it just works, and `maps.Clone(nil)` returns `nil`, which the function
normalizes to an empty map. Calling `MergeConfig()` with no layers at all must
return a non-nil empty map so the caller can range it unconditionally.

Create `configmerge.go`:

```go
package configmerge

import "maps"

// MergeConfig merges configuration layers with later layers winning on key
// collision. It never mutates any input layer: it copies every layer in order
// into a fresh accumulator map, so no input is ever mutated. It always returns a
// non-nil map.
func MergeConfig(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, layer := range layers {
		maps.Copy(out, layer)
	}
	return out
}
```

The implementation is even simpler than "clone the base then copy the rest": start
from a fresh empty map and copy every layer in order. Because `out` is always the
private accumulator and never an argument, no input is mutated — and `maps.Copy`
with a `nil` `src` is a harmless no-op, so absent layers need no special case. The
clone-the-base phrasing and this copy-into-fresh phrasing are equivalent; copying
into a fresh map is the clearer expression of the invariant "never write to an
input."

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/configmerge"
)

func main() {
	defaults := map[string]string{"port": "8080", "log_level": "info", "timeout": "30s"}
	fileLayer := map[string]string{"log_level": "debug", "timeout": "60s"}
	envLayer := map[string]string{"port": "9090"}

	merged := configmerge.MergeConfig(defaults, fileLayer, envLayer)

	for _, k := range slices.Sorted(maps.Keys(merged)) {
		fmt.Printf("%s=%s\n", k, merged[k])
	}
	fmt.Println("defaults intact:", defaults["port"] == "8080" && defaults["log_level"] == "info")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
log_level=debug
port=9090
timeout=60s
defaults intact: true
```

`port` came from env (9090, beating the default 8080), `log_level` and `timeout`
came from the file, and the `defaults` map is unchanged — the merge produced a new
map without touching any layer.

### Tests

The tests pin the four contract points. `TestPrecedence` asserts the last layer
wins. `TestInputsNotMutated` snapshots each input layer before the merge and
asserts it is byte-for-byte unchanged afterward — the property that separates this
implementation from the naive `maps.Copy(base, ...)` bug. `TestNilAndEmptyLayers`
feeds `nil` and empty layers and asserts they are no-ops. `TestZeroLayers` asserts
the empty call returns a non-nil empty map.

Create `configmerge_test.go`:

```go
package configmerge

import (
	"fmt"
	"maps"
	"testing"
)

func TestPrecedence(t *testing.T) {
	t.Parallel()

	got := MergeConfig(
		map[string]string{"a": "1", "b": "1", "c": "1"},
		map[string]string{"b": "2", "c": "2"},
		map[string]string{"c": "3"},
	)
	want := map[string]string{"a": "1", "b": "2", "c": "3"}
	if !maps.Equal(got, want) {
		t.Fatalf("MergeConfig() = %v, want %v (later layers must win)", got, want)
	}
}

func TestInputsNotMutated(t *testing.T) {
	t.Parallel()

	defaults := map[string]string{"port": "8080", "log": "info"}
	fileLayer := map[string]string{"log": "debug"}
	defaultsBefore := maps.Clone(defaults)
	fileBefore := maps.Clone(fileLayer)

	_ = MergeConfig(defaults, fileLayer)

	if !maps.Equal(defaults, defaultsBefore) {
		t.Errorf("defaults layer was mutated: %v, want %v", defaults, defaultsBefore)
	}
	if !maps.Equal(fileLayer, fileBefore) {
		t.Errorf("file layer was mutated: %v, want %v", fileLayer, fileBefore)
	}
}

func TestNilAndEmptyLayers(t *testing.T) {
	t.Parallel()

	got := MergeConfig(
		map[string]string{"a": "1"},
		nil,
		map[string]string{},
		map[string]string{"b": "2"},
	)
	want := map[string]string{"a": "1", "b": "2"}
	if !maps.Equal(got, want) {
		t.Fatalf("MergeConfig with nil/empty layers = %v, want %v", got, want)
	}
}

func TestZeroLayers(t *testing.T) {
	t.Parallel()

	got := MergeConfig()
	if got == nil {
		t.Fatal("MergeConfig() returned nil, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("MergeConfig() = %v, want empty", got)
	}
}

func ExampleMergeConfig() {
	merged := MergeConfig(
		map[string]string{"level": "info"},
		map[string]string{"level": "debug"},
	)
	fmt.Println(merged["level"])
	// Output: debug
}
```

## Review

The one invariant to defend is: `MergeConfig` mutates nothing it is given. It gets
that by copying every layer into a fresh accumulator map instead of into one of
the arguments, so `TestInputsNotMutated` — which snapshots the inputs and compares
after — is the test that must never be removed. Precedence falls out of
`maps.Copy`'s last-writer-wins semantics applied in layer order. The nil/empty and
zero-layer cases keep callers from having to guard the result. If a service ever
reports config that "leaks" between requests or environments, suspect a merge that
wrote into a shared defaults map; this implementation cannot do that. Run
`go test -race`.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Copy`, `Clone`, `Equal`.
- [Go blog: Go 1.21 maps and slices](https://go.dev/blog/maps) — the merge/clone primitives.
- [The Twelve-Factor App: Config](https://12factor.net/config) — why env overrides file overrides defaults.

---

Back to [02-deterministic-map-serialization.md](02-deterministic-map-serialization.md) | Next: [04-state-reconcile-diff.md](04-state-reconcile-diff.md)
