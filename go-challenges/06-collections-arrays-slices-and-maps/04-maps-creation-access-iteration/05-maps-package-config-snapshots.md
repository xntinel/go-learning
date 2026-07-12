# Exercise 5: maps package: defensive Clone, Equal diffing, layered Copy, DeleteFunc pruning

A config manager that layers defaults, file, and environment values and detects
changes for hot-reload is the natural home for the modern `maps` package. This
exercise builds one using `maps.Clone` for defensive snapshots, `maps.Equal` for
change detection, `maps.Copy` for layering, and `maps.DeleteFunc` for pruning —
and confronts the shallow-clone caveat that bites defensive copies.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
configmgr/                 independent module: example.com/configmgr
  go.mod
  config.go                Manager; New, Merge, Snapshot, Get, Changed, Prune, Dump, Values
  cmd/
    demo/
      main.go              runnable demo: layer defaults/file/env, detect change, defensive snapshot
  config_test.go           Clone independence, Clone(nil), Equal, Copy overwrite, DeleteFunc, shallow-clone caveat
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Snapshot` (defensive `maps.Clone`), `Merge` (layered `maps.Copy`), `Changed` (`maps.Equal`), `Prune` (`maps.DeleteFunc`), and a deterministic `Dump`.
- Test: `Clone` is independent and `Clone(nil)==nil`, `Equal` distinguishes differing values, `Copy` overwrites on collision, `DeleteFunc` removes only matching keys, and the shallow-clone caveat with a slice value.
- Verify: `go test -count=1 -race ./...`

### Why each maps function, and the shallow-clone trap

Four `maps` functions map onto four config operations. `maps.Copy(dst, src)`
overlays `src` onto `dst`, later keys winning — exactly the semantics of layering
file config over defaults, then env over both, so `Merge` is one call.
`maps.Clone(m)` returns an independent copy; `Snapshot` returns
`maps.Clone(current)` so a caller can read a stable config without being able to
mutate the manager's internal state (assigning the internal map directly would
share it, and the caller could corrupt live config). `maps.Equal(a, b)` reports
whether two maps hold the same pairs; `Changed` is `!maps.Equal(old, new)`, the
hot-reload trigger. `maps.DeleteFunc(m, pred)` prunes in place by predicate — drop
every key whose value is empty, say — replacing a delete-while-ranging loop.

The trap that must be internalized: **`maps.Clone` is shallow**. For
`map[string]string` that is fine, because strings are values. But if the value
type were `[]string` or a pointer, the clone would share that nested state with
the original, and mutating a nested slice through the "defensive" copy would
corrupt the source — silently defeating the copy. The test suite demonstrates this
directly with a `map[string][]string`, so the caveat is not just prose. The
manager here uses `map[string]string`, where the shallow clone is a genuinely
independent snapshot, but a senior engineer must know the boundary.

`Dump` and `Values` show the deterministic-view idiom over the `maps` iterators:
`slices.Sorted(maps.Keys(m))` for sorted keys, `slices.Sorted(maps.Values(m))` (or
`slices.Collect(maps.Values(m))` when order does not matter) for values.

Create `config.go`:

```go
package configmgr

import (
	"maps"
	"slices"
	"sync"
)

// Manager holds a layered configuration and hands out defensive snapshots.
type Manager struct {
	mu      sync.RWMutex
	current map[string]string
}

// New returns a Manager seeded with a defensive clone of defaults.
func New(defaults map[string]string) *Manager {
	return &Manager{current: maps.Clone(defaults)}
}

// Merge overlays layer onto the current config; keys in layer win.
func (m *Manager) Merge(layer map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		m.current = make(map[string]string)
	}
	maps.Copy(m.current, layer)
}

// Snapshot returns a defensive clone; callers may mutate it without affecting
// the manager's internal state.
func (m *Manager) Snapshot() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.current)
}

// Get returns the current value for key via comma-ok.
func (m *Manager) Get(key string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.current[key]
	return v, ok
}

// Prune drops every key for which drop reports true, returning the count removed.
func (m *Manager) Prune(drop func(key, value string) bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.current)
	maps.DeleteFunc(m.current, drop)
	return before - len(m.current)
}

// Dump returns the config as sorted "key=value" lines.
func (m *Manager) Dump() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.current))
	for _, k := range slices.Sorted(maps.Keys(m.current)) {
		out = append(out, k+"="+m.current[k])
	}
	return out
}

// Values returns the config values in ascending order.
func (m *Manager) Values() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Values(m.current))
}

// Changed reports whether old and new differ in any key or value.
func Changed(old, new map[string]string) bool {
	return !maps.Equal(old, new)
}
```

### The runnable demo

The demo layers defaults, a file layer, and an env layer (later layers winning),
detects that the config changed, dumps it in sorted order, and proves a snapshot
is defensive by tampering with it and showing internal state is untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configmgr"
)

func main() {
	m := configmgr.New(map[string]string{
		"log_level": "info",
		"timeout":   "30s",
		"region":    "us-east-1",
	})

	before := m.Snapshot()
	m.Merge(map[string]string{"log_level": "debug", "max_conns": "100"}) // file layer
	m.Merge(map[string]string{"region": "eu-west-1"})                    // env layer
	after := m.Snapshot()

	fmt.Println("changed:", configmgr.Changed(before, after))
	fmt.Println("dump:")
	for _, line := range m.Dump() {
		fmt.Println("  " + line)
	}

	snap := m.Snapshot()
	snap["log_level"] = "TAMPERED"
	v, _ := m.Get("log_level")
	fmt.Println("internal log_level still:", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
changed: true
dump:
  log_level=debug
  max_conns=100
  region=eu-west-1
  timeout=30s
internal log_level still: debug
```

`log_level` is `debug` (the file layer overrode the default) and `region` is
`eu-west-1` (the env layer overrode the default); `max_conns` came only from the
file layer. Tampering with the snapshot does not touch internal state because
`Snapshot` returns a `maps.Clone`, not the live map.

### Tests

`TestSnapshotIndependent` mutates a snapshot and asserts the original is intact,
and checks `maps.Clone(nil) == nil`. `TestChanged` covers equal and differing
maps. `TestMergeOverwrites` proves collisions take the later value while
non-overlapping keys survive. `TestPruneDeleteFunc` removes only matching keys.
`TestCloneIsShallow` is the caveat made concrete: cloning a `map[string][]string`
and mutating a nested slice through the clone corrupts the original.

Create `config_test.go`:

```go
package configmgr

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestSnapshotIndependent(t *testing.T) {
	t.Parallel()

	m := New(map[string]string{"a": "1"})
	snap := m.Snapshot()
	snap["a"] = "mutated"
	snap["b"] = "new"

	if v, _ := m.Get("a"); v != "1" {
		t.Fatalf("internal a = %q, want 1 (snapshot must be defensive)", v)
	}
	if _, ok := m.Get("b"); ok {
		t.Fatal("internal map gained key b from snapshot mutation")
	}
	if maps.Clone(map[string]string(nil)) != nil {
		t.Fatal("maps.Clone(nil) should be nil")
	}
}

func TestChanged(t *testing.T) {
	t.Parallel()

	a := map[string]string{"x": "1", "y": "2"}
	same := map[string]string{"x": "1", "y": "2"}
	diff := map[string]string{"x": "1", "y": "9"}

	if Changed(a, same) {
		t.Fatal("identical maps reported as changed")
	}
	if !Changed(a, diff) {
		t.Fatal("differing value not reported as changed")
	}
}

func TestMergeOverwrites(t *testing.T) {
	t.Parallel()

	m := New(map[string]string{"a": "1", "b": "2"})
	m.Merge(map[string]string{"b": "9", "c": "3"})

	want := map[string]string{"a": "1", "b": "9", "c": "3"}
	if got := m.Snapshot(); !maps.Equal(got, want) {
		t.Fatalf("after merge = %v, want %v", got, want)
	}
}

func TestPruneDeleteFunc(t *testing.T) {
	t.Parallel()

	m := New(map[string]string{"keep": "x", "drop1": "", "drop2": ""})
	removed := m.Prune(func(_, v string) bool { return v == "" })

	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if got := m.Dump(); !slices.Equal(got, []string{"keep=x"}) {
		t.Fatalf("after prune = %v, want [keep=x]", got)
	}
}

func TestCloneIsShallow(t *testing.T) {
	t.Parallel()

	original := map[string][]string{"tags": {"a", "b"}}
	clone := maps.Clone(original)
	clone["tags"][0] = "MUTATED" // mutates the shared backing slice

	if original["tags"][0] != "MUTATED" {
		t.Fatal("expected shallow clone: nested slice should be shared")
	}
}

func ExampleManager_Dump() {
	m := New(map[string]string{"b": "2", "a": "1"})
	fmt.Println(m.Dump())
	// Output: [a=1 b=2]
}
```

## Review

The manager is correct when a snapshot is a genuinely independent copy for
`map[string]string`: `TestSnapshotIndependent` mutates the snapshot and the
internal map is untouched. `Merge` layers with later-wins semantics via
`maps.Copy`; `Changed` fires exactly when `maps.Equal` reports a difference;
`Prune` removes exactly the keys the predicate selects via `maps.DeleteFunc`.
`Dump` is deterministic because it sorts keys, not because the map cooperates. The
one caveat a senior engineer must carry out of this exercise is
`TestCloneIsShallow`: `maps.Clone` copies the top level only, so a defensive copy
of a map with slice, map, or pointer values still shares that nested state — deep-
copy the values yourself when they are reference types.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Clone`, `Equal`, `Copy`, `DeleteFunc`, `Keys`, `Values`.
- [slices.Sorted / slices.Collect](https://pkg.go.dev/slices#Sorted) — materializing iterators to slices.
- [maps.Clone shallow-copy note](https://pkg.go.dev/maps#Clone) — "a shallow clone: the new keys and values are set using ordinary assignment".

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-group-by-inverted-index.md](04-group-by-inverted-index.md) | Next: [06-two-level-metrics-nested-maps.md](06-two-level-metrics-nested-maps.md)
