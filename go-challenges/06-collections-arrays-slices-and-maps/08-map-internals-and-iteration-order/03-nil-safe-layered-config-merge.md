# Exercise 3: Layered Config Merge with Nil-Map Safety and Presence Semantics

Config in a real service arrives in layers: built-in defaults, a file, then
environment overrides, each winning over the last. This module builds that merge as a
`map[string]string` and, on the way, exercises the two nil-map behaviors that cause
production incidents (safe read vs panicking write), comma-ok presence semantics, a
delete-sentinel, and `maps.Clone` so the result never aliases a caller's layer.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
config/                    independent module: example.com/config
  go.mod                   go 1.26
  config.go                Merge, Lookup, Deletion sentinel; nil-safe accumulator
  cmd/
    demo/
      main.go              merges three layers, prints the effective config
  config_test.go           override, presence, nil-safety, sentinel, clone tests
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Merge(layers ...map[string]string) map[string]string` (later wins, a
  `Deleted` sentinel value removes an inherited key), `Lookup(m, key) (string, bool)`.
- Test: later layer overrides; empty-string value retained and distinguished from
  absent via comma-ok; merging with nil layers does not panic; sentinel deletes; the
  result is independent of the input layers.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/03-nil-safe-layered-config-merge/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/03-nil-safe-layered-config-merge
```

### The two faces of a nil map, and why they bite

A nil map reads safely and writes fatally. `Lookup` reads: ranging or indexing a nil
map returns the zero value, so `Lookup(nil, "x")` must return `("", false)` without a
panic. That is what makes a missing layer harmless to *read*. But the accumulator that
`Merge` builds up is *written*, and if it were left nil the first `acc[k] = v` would
panic. So `Merge` starts with `make(map[string]string)` — never `var acc
map[string]string` — and only then copies layers in. This is the exact incident the
concepts file warns about: a layer that is absent (nil) is fine to read through, but the
merge target must be constructed before any write.

`maps.Copy(dst, src)` copies every key from `src` into `dst`, overwriting on
collision. Applying it to the layers in order gives "later layer wins" for free.
`maps.Copy` tolerates a nil `src` (copying zero entries), so a nil layer in the
variadic list is skipped naturally — no `if layer != nil` guard needed.

### Presence semantics and the delete sentinel

`""` is a legitimate config value: "set this override back to empty". So the merge
cannot use `v == ""` to mean "absent" — that would make an intentional empty override
indistinguishable from a key that was never set. `Lookup` uses comma-ok (`v, ok :=
m[key]`) so callers branch on presence, and the empty string round-trips as a real
value.

To *remove* an inherited key, a later layer sets it to the `Deleted` sentinel (a
distinguished string constant). After copying all layers, `Merge` runs
`maps.DeleteFunc(acc, func(_, v string) bool { return v == Deleted })` to strip every
key whose final value is the sentinel. This is the standard "tombstone" pattern:
overriding to empty keeps the key with value `""`; overriding to `Deleted` removes it
entirely. The order matters — delete after the copies, so a later layer can resurrect a
key a middle layer tombstoned by setting it to a real value again.

Create `config.go`:

```go
package config

import "maps"

// Deleted is a tombstone value. A layer that sets a key to Deleted removes that
// key from the merged result, distinguishing "remove this key" from "override to
// the empty string".
const Deleted = "\x00__deleted__"

// Merge folds layers left to right into one config: a later layer's value wins,
// and a value of Deleted removes the key. The result is a freshly allocated map
// that aliases none of the inputs, so mutating it cannot corrupt a caller layer.
func Merge(layers ...map[string]string) map[string]string {
	acc := make(map[string]string) // never nil: the write path below would panic on nil
	for _, layer := range layers {
		maps.Copy(acc, layer) // maps.Copy tolerates a nil layer (copies nothing)
	}
	maps.DeleteFunc(acc, func(_, v string) bool { return v == Deleted })
	return acc
}

// Lookup reads a key with presence semantics. It is nil-safe: reading a nil map
// returns ("", false) rather than panicking, so an absent layer is harmless.
func Lookup(m map[string]string, key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/config"
)

func main() {
	defaults := map[string]string{"host": "localhost", "port": "8080", "debug": "false"}
	file := map[string]string{"port": "9090", "region": ""}           // region set to empty on purpose
	env := map[string]string{"debug": "true", "host": config.Deleted} // host removed

	merged := config.Merge(defaults, file, env)

	for _, k := range []string{"host", "port", "debug", "region"} {
		if v, ok := config.Lookup(merged, k); ok {
			fmt.Printf("%-7s = %q\n", k, v)
		} else {
			fmt.Printf("%-7s = <absent>\n", k)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host    = <absent>
port    = "9090"
debug   = "true"
region  = ""
```

(`host` was tombstoned by the env layer; `port` took the file override; `debug` took
the env override; `region` is present with an intentionally empty value.)

### Tests

The tests pin each behavior: override precedence, the empty-vs-absent distinction via
comma-ok, that merging nil layers (and an all-nil call) does not panic, the sentinel
delete and resurrection, and that the result is independent of its inputs.

Create `config_test.go`:

```go
package config

import (
	"maps"
	"testing"
)

func TestLaterLayerWins(t *testing.T) {
	t.Parallel()

	got := Merge(
		map[string]string{"a": "1", "b": "1"},
		map[string]string{"b": "2"},
	)
	want := map[string]string{"a": "1", "b": "2"}
	if !maps.Equal(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}
}

func TestEmptyValueRetainedAndDistinctFromAbsent(t *testing.T) {
	t.Parallel()

	got := Merge(map[string]string{"region": ""})

	if v, ok := Lookup(got, "region"); !ok || v != "" {
		t.Fatalf(`Lookup(region) = %q,%v; want "",true (present but empty)`, v, ok)
	}
	if _, ok := Lookup(got, "missing"); ok {
		t.Fatal("Lookup(missing) reported present")
	}
}

func TestNilLayersDoNotPanic(t *testing.T) {
	t.Parallel()

	// A nil layer in the middle, and an all-nil call, must both be safe.
	got := Merge(nil, map[string]string{"x": "1"}, nil)
	if v, _ := Lookup(got, "x"); v != "1" {
		t.Fatalf("x = %q, want 1", v)
	}
	if empty := Merge(nil, nil); len(empty) != 0 {
		t.Fatalf("Merge(nil,nil) len = %d, want 0", len(empty))
	}
}

func TestNilMapLookupIsSafe(t *testing.T) {
	t.Parallel()

	var absent map[string]string // nil
	if v, ok := Lookup(absent, "anything"); ok || v != "" {
		t.Fatalf("Lookup(nil) = %q,%v; want \"\",false", v, ok)
	}
}

func TestDeleteSentinel(t *testing.T) {
	t.Parallel()

	got := Merge(
		map[string]string{"a": "1", "b": "2"},
		map[string]string{"a": Deleted}, // remove a
	)
	if _, ok := Lookup(got, "a"); ok {
		t.Fatal("a should have been deleted by the sentinel")
	}
	if v, _ := Lookup(got, "b"); v != "2" {
		t.Fatalf("b = %q, want 2", v)
	}
}

func TestDeletedKeyCanBeResurrected(t *testing.T) {
	t.Parallel()

	got := Merge(
		map[string]string{"a": "1"},
		map[string]string{"a": Deleted}, // tombstone
		map[string]string{"a": "3"},     // bring it back with a real value
	)
	if v, ok := Lookup(got, "a"); !ok || v != "3" {
		t.Fatalf("a = %q,%v; want 3,true", v, ok)
	}
}

func TestResultIsIndependentOfLayers(t *testing.T) {
	t.Parallel()

	base := map[string]string{"a": "1"}
	got := Merge(base)
	got["a"] = "mutated" // mutate the result

	if base["a"] != "1" {
		t.Fatal("mutating the merged result leaked back into an input layer")
	}
}
```

## Review

The merge is correct when it is nil-safe on both paths and honest about presence.
The accumulator is `make`-d before any write (never `var acc map[string]string`), which
is the whole nil-map-write incident in one line; `Lookup` reads through nil without
panicking, which is why an absent layer is harmless. Empty string is a value, not an
absence — comma-ok is the only correct test — and `Deleted` is the separate signal for
"remove". Because `Merge` builds a fresh map and `maps.Copy` never shares backing
storage, the result aliases no input, so a caller mutating the merged config cannot
corrupt a defaults table shared across requests. Run `go test -race`.

## Resources

- [`maps.Copy`](https://pkg.go.dev/maps#Copy) and [`maps.DeleteFunc`](https://pkg.go.dev/maps#DeleteFunc) — the merge and tombstone primitives.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — nil-map read is safe, write panics.
- [Effective Go: Maps](https://go.dev/doc/effective_go#maps) — the comma-ok presence idiom.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-generic-set-membership.md](04-generic-set-membership.md)
