# Exercise 5: Concurrent Feature-Flag Registry with Clone-Based Snapshots

A feature-flag registry is read constantly and written rarely. Readers want a
stable view they can consult over the course of a request without a writer
changing it mid-flight. The standard answer is a snapshot: under a read lock,
`maps.Clone` the live map and hand the caller the private copy. This module builds
that registry, tests it under `-race`, and demonstrates the shallow-clone boundary
that bites when values are pointers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
flagreg/                    independent module: example.com/flagreg
  go.mod                    go 1.26
  flagreg.go                Registry (RWMutex), Set, Delete, Get, Snapshot
  cmd/
    demo/
      main.go               take a snapshot, write, show the snapshot is stable
  flagreg_test.go           -race concurrent Set/Snapshot, snapshot stability, shallow-clone caveat
```

Files: `flagreg.go`, `cmd/demo/main.go`, `flagreg_test.go`.
Implement: `Registry` guarded by `sync.RWMutex`; `Set`, `Delete`, `Get`, and `Snapshot() map[string]bool` returning a clone.
Test: concurrent `Set`/`Snapshot` under `-race`; a snapshot stays stable across later writes; a pointer-value case shows shared mutation and the deep-copy fix.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/05-immutable-snapshot-registry/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/05-immutable-snapshot-registry
```

## Why you hand out a clone, not the live map

The registry holds a `map[string]bool` guarded by a `sync.RWMutex`. `Set` and
`Delete` take the write lock and mutate; `Get` takes the read lock and does a
single comma-ok lookup. The interesting method is `Snapshot`. A reader that needs
a consistent view across many lookups cannot just be handed the live map: the
moment the read lock is released, a writer could mutate it, and even holding the
lock for the whole request would serialize every writer behind every reader.
Instead, `Snapshot` takes the read lock briefly, `maps.Clone`s the map, releases
the lock, and returns the clone. The caller now owns a private `map[string]bool`
that no writer can touch — it is a point-in-time view, stable no matter what
happens to the registry afterward.

This is safe precisely because the value type is `bool`, an immutable scalar. The
clone duplicates every entry, and since `bool` values cannot be mutated in place,
the snapshot is genuinely independent. Running the concurrent test under `-race`
proves the locking is correct: many goroutines call `Set` while others call
`Snapshot`, and the race detector stays silent because every access to the shared
map is under the lock, and the clone happens inside the read-locked section.

Note what would be a bug: cloning *outside* the lock. `maps.Clone` reads every
entry of the source map; if a writer mutates the map during that read, it is a
concurrent map read/write — a fatal, detectable race. The clone must happen while
the read lock is held. That is the entire reason `Snapshot` locks around the
`maps.Clone` call rather than around only the map field access.

Create `flagreg.go`:

```go
package flagreg

import (
	"maps"
	"sync"
)

// Registry is a concurrency-safe feature-flag store. Readers take point-in-time
// snapshots; writers mutate under the write lock.
type Registry struct {
	mu    sync.RWMutex
	flags map[string]bool
}

func New() *Registry {
	return &Registry{flags: make(map[string]bool)}
}

// Set enables or disables a flag.
func (r *Registry) Set(name string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flags[name] = on
}

// Delete removes a flag entirely.
func (r *Registry) Delete(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.flags, name)
}

// Get reports a single flag's value and whether it is set.
func (r *Registry) Get(name string) (on bool, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.flags[name]
	return v, ok
}

// Snapshot returns an independent copy of all flags. The clone happens under the
// read lock so it cannot race a concurrent writer. Because the value type is
// bool (immutable), the snapshot is a true point-in-time view.
func (r *Registry) Snapshot() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.flags)
}
```

### The shallow-clone boundary

The clean snapshot above works only because `bool` cannot be mutated in place.
Change the value type to a pointer or a slice and `maps.Clone` no longer gives you
an immutable snapshot: it copies the pointer, so the snapshot and the live map
point at the same underlying object, and a writer mutating that object corrupts the
snapshot. The demo and a test below make this concrete with a `*Rule` value, and
show the fix: deep-copy the pointed-to value when you snapshot.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/flagreg"
)

func main() {
	r := flagreg.New()
	r.Set("new-checkout", true)
	r.Set("dark-mode", false)

	snap := r.Snapshot()

	// A later write does not change the snapshot we already took.
	r.Set("dark-mode", true)
	r.Set("beta-api", true)

	fmt.Println("snapshot keys:", slices.Sorted(maps.Keys(snap)))
	fmt.Println("snapshot dark-mode:", snap["dark-mode"])

	live, _ := r.Get("dark-mode")
	fmt.Println("live dark-mode:", live)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
snapshot keys: [dark-mode new-checkout]
snapshot dark-mode: false
live dark-mode: true
```

The snapshot froze `dark-mode` at `false` and never saw `beta-api`, even though the
live registry moved on — exactly the stable view a request handler wants.

### Tests

`TestSnapshotIsStable` takes a snapshot, mutates the registry, and asserts the
snapshot is unchanged. `TestConcurrentSetSnapshot` runs many `Set` and `Snapshot`
goroutines to catch any lock mistake under `-race`. `TestShallowCloneCaveat`
demonstrates the pointer-value trap directly: a shallow clone shares the pointee,
so mutating it leaks into the snapshot, and a deep copy fixes it.

Create `flagreg_test.go`:

```go
package flagreg

import (
	"fmt"
	"maps"
	"sync"
	"testing"
)

func TestSnapshotIsStable(t *testing.T) {
	t.Parallel()

	r := New()
	r.Set("a", true)
	r.Set("b", false)

	snap := maps.Clone(r.Snapshot())

	r.Set("b", true)
	r.Set("c", true)
	r.Delete("a")

	if snap["a"] != true {
		t.Error("snapshot lost flag a after registry delete")
	}
	if snap["b"] != false {
		t.Error("snapshot saw a later write to flag b")
	}
	if _, ok := snap["c"]; ok {
		t.Error("snapshot saw a flag added after it was taken")
	}
}

func TestConcurrentSetSnapshot(t *testing.T) {
	t.Parallel()

	r := New()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Set("flag", i%2 == 0)
		}()
		go func() {
			defer wg.Done()
			_ = r.Snapshot()
		}()
	}
	wg.Wait()
}

// Rule is a mutable pointer value, used to show the shallow-clone boundary.
type Rule struct{ Percent int }

func TestShallowCloneCaveat(t *testing.T) {
	t.Parallel()

	live := map[string]*Rule{"rollout": {Percent: 10}}

	// Shallow clone shares the pointee.
	shallow := maps.Clone(live)
	live["rollout"].Percent = 90
	if shallow["rollout"].Percent != 90 {
		t.Fatal("expected shallow clone to share the pointer value")
	}

	// Deep copy isolates it.
	deep := make(map[string]*Rule, len(live))
	for k, v := range live {
		cp := *v
		deep[k] = &cp
	}
	live["rollout"].Percent = 5
	if deep["rollout"].Percent != 90 {
		t.Fatalf("deep copy should be isolated, got %d", deep["rollout"].Percent)
	}
}

func ExampleRegistry_Snapshot() {
	r := New()
	r.Set("x", true)
	snap := r.Snapshot()
	r.Set("x", false)
	fmt.Println(snap["x"])
	// Output: true
}
```

## Review

The registry is correct when every access to the shared map is under the lock and
the snapshot is cloned inside the read-locked section — clone outside the lock and
you reintroduce the race the lock exists to prevent, which `-race` will catch. The
snapshot's immutability is real only because the value type is an immutable scalar;
`TestShallowCloneCaveat` is the reminder that `maps.Clone` on pointer/slice values
shares the pointee, so a "snapshot" of reference types needs a deep copy to be a
true point-in-time view. This is the single most common way a supposedly-immutable
snapshot silently gets corrupted in production. Run `go test -race` — the point of
this module is that it stays green under the detector.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Clone` and its shallow-copy semantics.
- [sync package](https://pkg.go.dev/sync) — `RWMutex`.
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` proves.

---

Back to [04-state-reconcile-diff.md](04-state-reconcile-diff.md) | Next: [06-streaming-map-collect.md](06-streaming-map-collect.md)
