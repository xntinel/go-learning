# Exercise 11: Cluster Registry Snapshot: maps.Clone and the Concurrent-Map Crash

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service-discovery registry -- the in-memory catalog a Consul agent or an
etcd-backed catalog keeps of which nodes are up -- is written to constantly:
health checks arrive on a timer, every node in the fleet reports in every
few seconds. It is also read constantly by things that have nothing to do
with the write path, like a metrics exporter goroutine that wakes up once a
minute and wants to print the current node count by status. The obvious
implementation stores health in a mutex-guarded `map[string]string` and
gives the exporter a method to fetch it. The obvious mistake is what that
method returns.

If `Snapshot()` returns the registry's internal map field directly -- `return
r.nodes` -- the mutex protected nothing. The exporter now holds a reference
to live, mutable state, and the moment it starts a `range` over that map
while a health-check goroutine writes to it through `Update`, Go does not
give you a subtle, deniable data race. Reading and writing a map
concurrently without synchronization is a case the runtime checks for
directly: it terminates the process immediately with `fatal error:
concurrent map read and map write`, a message `recover` cannot catch,
because it is not a panic. One exporter goroutine having outlived its
snapshot by a few milliseconds can take down the whole service.

This module builds `Registry` as a package: a mutex-guarded map of node
health, an `Update` method, and a `Snapshot` method that holds the lock only
long enough to run `maps.Clone` before releasing it -- so the exporter's copy
shares no storage with the registry's, and the two can run at full speed
against each other with the race detector watching and finding nothing.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
registry/                module example.com/registry
  go.mod                 go 1.24
  registry.go            Registry; NewRegistry, Update, Status, Snapshot; one sentinel error
  registry_test.go       Update/Status table, snapshot aliasing, the buggy-snapshot contrast,
                          concurrent Update-vs-Snapshot under -race, ExampleRegistry_Snapshot
```

- Files: `registry.go`, `registry_test.go`.
- Implement: `NewRegistry() *Registry`; `(*Registry).Update(node, status string) error` returning `ErrEmptyNode` for an empty node name; `(*Registry).Status(node string) (string, bool)` using the comma-ok map form; `(*Registry).Snapshot() map[string]string` returning `maps.Clone` of the internal map, taken under the shortest possible lock.
- Test: `Update` and `Status` round-trip and overwrite correctly; repeated updates of the same node do not grow the snapshot; `Update` rejects an empty node with `ErrEmptyNode`; a fresh registry's `Snapshot` is non-nil and empty; two `Snapshot` calls taken before and after an `Update` are independent; mutating a `Snapshot` never reaches the registry; the buggy-snapshot contrast proving the naive version does alias; `Update` and `Snapshot` exercised concurrently by many goroutines under `-race`; and `ExampleRegistry_Snapshot` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/11-cluster-registry-map-clone-snapshot
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/11-cluster-registry-map-clone-snapshot
go mod edit -go=1.24
```

### A slice aliasing bug is silent; a map aliasing bug can crash the process

The rest of this lesson's earlier exercises treat handing out an internal
slice as a correctness problem: a caller's mutation corrupts state nobody
meant to share, and under concurrency the race detector flags it as a data
race -- serious, but something `-race` in CI catches before it reaches
production, and something a `sync.Mutex` around every access would still
technically survive, just slowly and with torn reads. A map is not that
forgiving. Go's map implementation carries an internal write-in-progress
flag specifically so it can detect concurrent read/write and multi-write
access *without* the race detector's instrumentation, and when it detects
one it does not corrupt memory quietly -- it calls `fatal error` and exits
the process. That check runs in production builds, with `-race` or without
it, and it cannot be caught: `fatal error` is the runtime giving up, not a
panic flowing through `recover`.

`Registry.Snapshot` avoids ever creating that situation by never handing out
the map a writer can still reach:

```go
func (r *Registry) Snapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.nodes)
}
```

The lock is held for exactly as long as `maps.Clone` needs to walk `r.nodes`
and copy every entry into a new map -- microseconds, not the lifetime of
whatever the caller does with the result. Once `Snapshot` returns, the
caller's map and the registry's map are two separate allocations; a writer
goroutine calling `Update` afterward touches only the registry's map, and
the exporter ranging over its copy touches only its own. Neither can ever
observe the other's write, because there is nothing shared left to observe.
The naive version skips the clone and returns `r.nodes` itself -- same
pointer, same backing storage, still being written to. That version is
explored in the test file, not in this package, because the API a caller
imports should only ever have one way to take a snapshot, and it should be
the safe one.

Create `registry.go`:

```go
// Package registry keeps a service-discovery-style catalog of node health in
// memory and hands out mutation-safe snapshots for callers such as a metrics
// exporter that periodically prints the current state.
package registry

import (
	"errors"
	"maps"
	"sync"
)

// ErrEmptyNode is returned by Update when node is the empty string.
var ErrEmptyNode = errors.New("registry: node name must not be empty")

// Registry holds live node health keyed by node name, guarded by a mutex.
//
// Registry is safe for concurrent use by multiple goroutines: every method
// that touches nodes takes the mutex for the shortest span that correctness
// allows.
type Registry struct {
	mu    sync.Mutex
	nodes map[string]string
}

// NewRegistry returns an empty Registry ready to record node status.
func NewRegistry() *Registry {
	return &Registry{nodes: make(map[string]string)}
}

// Update records status for node, overwriting any previous status. It
// returns ErrEmptyNode if node is the empty string.
func (r *Registry) Update(node, status string) error {
	if node == "" {
		return ErrEmptyNode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[node] = status
	return nil
}

// Status reports node's most recently recorded status. The comma-ok result
// is false if node has never been updated.
func (r *Registry) Status(node string) (status string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, ok = r.nodes[node]
	return status, ok
}

// Snapshot returns an independent copy of the registry's current node
// health, safe to range over or print without holding any lock and without
// racing the registry's own writer goroutines.
//
// The lock is held only long enough to run maps.Clone; the returned map
// shares no storage with the registry's internal map, so the caller may
// retain, mutate, or range over it freely, and a concurrent Update can never
// observe or corrupt that read. maps.Clone(nil) is nil, but nodes is always
// initialized by NewRegistry, so Snapshot never returns nil.
func (r *Registry) Snapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.nodes)
}
```

### Using it

Construct one `Registry` per process with `NewRegistry` and share the
pointer across every goroutine that reports or reads node health -- that is
what the doc comment's concurrency contract promises, and what
`TestSnapshotSafeUnderConcurrentUpdate` holds it to. Health-check goroutines
call `Update`; anything that needs a point-in-time view, like an exporter or
an HTTP handler serving `/debug/registry`, calls `Snapshot` and then works
with the result at leisure, entirely outside the registry's lock.

That last point is the whole contract in one sentence: once `Snapshot`
returns, the caller owns its copy outright and the registry has already
moved on, so there is no window in which the two can interfere with each
other, no matter how long the caller holds onto what it got back or how
often the registry is updated in the meantime.

The module has no `main.go`, because a registry is a library, not a tool.
Its executable demonstration is `ExampleRegistry_Snapshot`: `go test` runs
it and compares its standard output against the `// Output:` comment, so the
usage shown below cannot drift away from the code.

```go
func ExampleRegistry_Snapshot() {
	r := NewRegistry()
	_ = r.Update("node-1", "healthy")
	_ = r.Update("node-2", "degraded")

	snap := r.Snapshot()
	for _, k := range slices.Sorted(maps.Keys(snap)) {
		fmt.Printf("%s=%s\n", k, snap[k])
	}

	snap["node-1"] = "tampered" // mutating the snapshot ...
	status, _ := r.Status("node-1")
	fmt.Println("registry after mutating the snapshot:", status) // ... never shows here

	// Output:
	// node-1=healthy
	// node-2=degraded
	// registry after mutating the snapshot: healthy
}
```

Map iteration order is randomized by design in Go, so the example sorts the
snapshot's keys with `slices.Sorted(maps.Keys(snap))` before printing --
without that, the output would not be reproducible and `go test` could not
check it.

### Tests

`TestUpdateAndStatus` and `TestUpdateRejectsEmptyNode` cover the ordinary
path and the one validated input. `TestSnapshotOnFreshRegistryIsNonNilEmpty`
pins the boundary this lesson's earlier exercises established: an empty
collection is still non-nil.

`TestRepeatedUpdateOfSameNodeDoesNotGrowSnapshot` confirms the registry is
keyed by node name, not by call count, and `TestSnapshotIsIndependentAcrossCalls`
confirms a snapshot taken before a later `Update` never picks up that
write -- two snapshots of the same registry, taken at different times, are
two genuinely separate values.

`TestSnapshotDoesNotAliasInternalMap` and
`TestBuggySnapshotAliasesInternalMap` are the heart of the module, and they
are deliberately built as a safe, deterministic pair rather than as an
attempt to trigger the runtime crash directly -- that crash is unconditional
and unrecoverable, so provoking it inside this test suite would take down
`go test` itself. Instead, both tests pin the same root cause the crash
depends on: whether the returned map shares storage with the registry's.
`snapshotBuggy` is unexported and unreachable from the package API; mutating
its result changes the registry, while mutating `Snapshot`'s result does
not. `TestSnapshotSafeUnderConcurrentUpdate` then exercises `Update` and
`Snapshot` from twenty goroutines at once -- the property `-race` verifies
here is exactly the one that keeps the crash from ever being reachable
through the real API.

Create `registry_test.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
)

// snapshotBuggy is the naive "snapshot" a Registry.Snapshot is easy to write
// by accident: it returns the internal map field itself instead of a clone.
// It is never exported and never reachable through the package API. Calling
// it concurrently with Update is exactly the scenario this module warns
// about -- a goroutine ranging over the returned map while Update writes to
// the same map triggers the Go runtime's unconditional, unrecoverable
// "fatal error: concurrent map read and map write", which crashes the whole
// process and cannot be caught with recover. That crash is not exercised
// here on purpose: it would abort this very test binary. What the tests
// below pin instead is the deterministic root cause -- aliasing -- which is
// what makes the crash possible in the first place.
func snapshotBuggy(r *Registry) map[string]string {
	return r.nodes
}

func TestUpdateAndStatus(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if _, ok := r.Status("node-1"); ok {
		t.Fatal("Status on an empty registry reported ok=true")
	}

	if err := r.Update("node-1", "healthy"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := r.Update("node-2", "degraded"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := r.Update("node-1", "unhealthy"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	status, ok := r.Status("node-1")
	if !ok || status != "unhealthy" {
		t.Fatalf("Status(node-1) = %q, %v, want %q, true", status, ok, "unhealthy")
	}
	status, ok = r.Status("node-2")
	if !ok || status != "degraded" {
		t.Fatalf("Status(node-2) = %q, %v, want %q, true", status, ok, "degraded")
	}
}

// TestRepeatedUpdateOfSameNodeDoesNotGrowSnapshot confirms that updating the
// same node repeatedly overwrites its entry rather than accumulating one per
// call: the registry is keyed by node name, not by call count.
func TestRepeatedUpdateOfSameNodeDoesNotGrowSnapshot(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	for i := 0; i < 5; i++ {
		if err := r.Update("node-1", fmt.Sprintf("status-%d", i)); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	if snap["node-1"] != "status-4" {
		t.Fatalf("Snapshot[node-1] = %q, want the latest status", snap["node-1"])
	}
}

func TestUpdateRejectsEmptyNode(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Update("", "healthy"); !errors.Is(err, ErrEmptyNode) {
		t.Fatalf("Update(\"\") error = %v, want ErrEmptyNode", err)
	}
}

func TestSnapshotOnFreshRegistryIsNonNilEmpty(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	snap := r.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot on a fresh registry is nil, want a non-nil empty map")
	}
	if len(snap) != 0 {
		t.Fatalf("Snapshot on a fresh registry = %v, want empty", snap)
	}
}

// TestSnapshotIsIndependentAcrossCalls confirms two Snapshot calls taken
// before and after an Update return two independent maps: the earlier one
// must not silently pick up the later write.
func TestSnapshotIsIndependentAcrossCalls(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Update("node-1", "healthy"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	before := r.Snapshot()
	if err := r.Update("node-1", "degraded"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after := r.Snapshot()

	if before["node-1"] != "healthy" {
		t.Fatalf("before-snapshot mutated by a later Update: %q", before["node-1"])
	}
	if after["node-1"] != "degraded" {
		t.Fatalf("after-snapshot = %q, want the latest status", after["node-1"])
	}
}

// TestSnapshotDoesNotAliasInternalMap is the module's central assertion:
// mutating the map Snapshot returns must never change the registry's own
// state, because Snapshot clones under the lock instead of handing out its
// internal map.
func TestSnapshotDoesNotAliasInternalMap(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Update("node-1", "healthy"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	snap := r.Snapshot()
	snap["node-1"] = "tampered"
	snap["node-2"] = "injected"

	status, ok := r.Status("node-1")
	if !ok || status != "healthy" {
		t.Fatalf("mutating the snapshot changed the registry: Status(node-1) = %q, %v", status, ok)
	}
	if _, ok := r.Status("node-2"); ok {
		t.Fatal("mutating the snapshot injected a key into the registry")
	}
}

// TestBuggySnapshotAliasesInternalMap contrasts snapshotBuggy against
// Snapshot for the identical registry: the buggy version hands back the
// live map, so a caller's mutation of the "snapshot" corrupts the registry
// itself -- the deterministic, always-reproducible half of the bug that the
// concurrent crash builds on.
func TestBuggySnapshotAliasesInternalMap(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Update("node-1", "healthy"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	buggy := snapshotBuggy(r)
	buggy["node-1"] = "tampered"

	status, ok := r.Status("node-1")
	if !ok || status != "tampered" {
		t.Fatalf("snapshotBuggy did not alias the registry: Status(node-1) = %q, %v, want %q, true", status, ok, "tampered")
	}
}

// TestSnapshotSafeUnderConcurrentUpdate exercises Update and Snapshot from
// many goroutines at once. Registry declares itself safe for concurrent
// use; this test is what -race holds that claim to. A failure here would
// show up as a data race report, not as a test assertion.
func TestSnapshotSafeUnderConcurrentUpdate(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	const goroutines = 20
	const rounds = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			node := fmt.Sprintf("node-%d", g)
			for i := 0; i < rounds; i++ {
				if err := r.Update(node, fmt.Sprintf("status-%d", i)); err != nil {
					t.Errorf("Update: %v", err)
					return
				}
				snap := r.Snapshot()
				if _, ok := snap[node]; !ok {
					t.Errorf("Snapshot missing %q right after Update", node)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	final := r.Snapshot()
	if len(final) != goroutines {
		t.Fatalf("final Snapshot has %d nodes, want %d", len(final), goroutines)
	}
}

// ExampleRegistry_Snapshot is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below. Map iteration order is randomized, so the keys are sorted before
// printing to keep the output deterministic.
func ExampleRegistry_Snapshot() {
	r := NewRegistry()
	_ = r.Update("node-1", "healthy")
	_ = r.Update("node-2", "degraded")

	snap := r.Snapshot()
	for _, k := range slices.Sorted(maps.Keys(snap)) {
		fmt.Printf("%s=%s\n", k, snap[k])
	}

	snap["node-1"] = "tampered" // mutating the snapshot ...
	status, _ := r.Status("node-1")
	fmt.Println("registry after mutating the snapshot:", status) // ... never shows here

	// Output:
	// node-1=healthy
	// node-2=degraded
	// registry after mutating the snapshot: healthy
}
```

## Review

`Snapshot` is correct when the map it returns shares no storage with the
registry -- `TestSnapshotDoesNotAliasInternalMap` pins that directly, and
`TestBuggySnapshotAliasesInternalMap` shows what breaks the instant it does.
The mechanism is `maps.Clone` run inside the shortest lock the method can
get away with: the copy is what turns "a read that might overlap a write"
into "a read that finished before anyone else could touch the data." The
stakes are higher here than the equivalent slice-aliasing mistake elsewhere
in this lesson, because a concurrent map read and write is not a data race
Go quietly tolerates until `-race` finds it -- it is a condition the runtime
detects on its own and responds to by crashing the process outright, a
failure mode `recover` cannot intercept. `Update` rejects an empty node name
with `ErrEmptyNode`, checkable via `errors.Is`, and `Registry` is safe to
share across every goroutine in the service, which
`TestSnapshotSafeUnderConcurrentUpdate` exercises directly under the race
detector. Run `go test -count=1 -race ./...`.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the defensive copy `Snapshot` returns; `Clone(nil)` is nil.
- [Go blog: The Go Memory Model](https://go.dev/ref/mem) — what synchronization actually guarantees between goroutines.
- [Go issue tracker: fatal error: concurrent map read and map write](https://github.com/golang/go/issues/7970) — background on the runtime's built-in, unrecoverable detection.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock `Registry` holds only across `maps.Clone`, never across the caller's use of the result.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-feature-flag-map-json-boundary.md](10-feature-flag-map-json-boundary.md) | Next: [12-reconciler-nil-empty-equality-diff.md](12-reconciler-nil-empty-equality-diff.md)
