# Exercise 17: Type-Safe Read-Only Snapshot Over sync.Map

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A reverse proxy in the Envoy cluster-manager mold keeps an in-memory table of
upstream endpoints. That table is rebuilt wholesale exactly once per
control-plane push — a new xDS response, a service-discovery refresh — and
then read by every proxied request goroutine concurrently until the *next*
push replaces the whole table again. No request goroutine ever writes to it.
This is the one shape of workload `sync.Map` was actually designed for:
write-once (or write-rarely), then read-many, by many goroutines, with no
writer contending against the readers in the steady state.

`sync.Map` is easy to reach for and easy to misuse, because its documentation
undersells how narrow its sweet spot is. Its `Store`/`Load`/`Range` API is
typed `any` on both keys and values, which throws away the compiler's help
at every call site: nothing stops two unrelated call sites from storing
different concrete types under the same map, and the only way to find out is
a type assertion at read time — one that panics if it is wrong. That is a
real production incident waiting for the day someone stores a debug string
where an `Endpoint` was expected and a proxy goroutine panics mid-request
instead of degrading gracefully.

This module builds `snapshot`, a generic wrapper that restores compile-time
key and value types over `sync.Map` and replaces every unchecked type
assertion with an internal comma-ok one, so a mis-typed entry is reported as
"not found" instead of crashing the goroutine that touched it. The wrapper
does not change what `sync.Map` is good at or fix its narrow fitness — it
only removes the type-safety tax of using it correctly.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
snapshot/                module example.com/snapshot
  go.mod                 go 1.24
  snapshot.go            Snapshot[K, V]; New, Store, Load, LoadOrStore, Range
  snapshot_test.go        store/load table, missing-key zero value, the
                          unchecked-assertion contrast, write-once-read-many
                          concurrency, ExampleSnapshot
```

- Files: `snapshot.go`, `snapshot_test.go`.
- Implement: `Snapshot[K comparable, V any]` wrapping `sync.Map`; `New[K, V]() *Snapshot[K, V]`; `(*Snapshot[K, V]).Store(K, V)`; `(*Snapshot[K, V]).Load(K) (V, bool)`; `(*Snapshot[K, V]).LoadOrStore(K, V) (V, bool)`; `(*Snapshot[K, V]).Range(func(K, V) bool)`.
- Test: store/load round-trip, a missing key returning the zero value and `false`, `LoadOrStore` keeping the first-written value, `Range` visiting every entry and honoring an early `false` return, an empty snapshot's `Range` calling nothing, a raw-`sync.Map` contrast proving an unchecked type assertion panics on a mis-typed entry while `Snapshot.Load` returns `(zero, false)` for the same entry, a write-once-then-read-many concurrency test, and `ExampleSnapshot` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/17-readonly-snapshot-syncmap
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/17-readonly-snapshot-syncmap
go mod edit -go=1.24
```

### sync.Map's narrow sweet spot, and the type-safety tax it charges

`sync.Map` predates generics; every value that goes in or comes out is typed
`any`. Reading one back means a type assertion, and the version people
actually write is the unchecked, single-result form:

```go
v, _ := endpoints.Load(clusterName)
addr := v.(Endpoint).Addr   // panics if v is not an Endpoint
```

Nothing in the compiler catches a call site elsewhere in the program that
stored a `string` under the same map by mistake — a debug placeholder, a
value from a different code path that reused the map for something else.
The panic surfaces at the *read* site, in a goroutine that may have nothing
to do with the bug that caused it, and it takes that goroutine down with it.
The fix is the two-result comma-ok form, `v, ok := raw.(Endpoint)`, applied
once inside a wrapper so every caller of the wrapper gets it automatically
instead of having to remember to write it at every call site.

That type-safety fix does not change what `sync.Map` is *for*. Its internal
design — two structures, a fast read-only map and a slower dirty map that
promotes into the read map when writes stabilize — pays off precisely when
writes are rare relative to reads, or when goroutines touch disjoint keys
and therefore never contend. A general read/write workload, or a
write-heavy one, is the case where a `sync.RWMutex`-guarded plain map or a
striped/sharded map (partition the key space into N independently locked
shards) wins instead: one global write lock in the `RWMutex` case
serializes every writer, but a sharded map lets writes to different shards
proceed in parallel, and neither pays `sync.Map`'s internal promotion
bookkeeping for a workload it was never optimized for. Reach for `Snapshot`
only when the shape is genuinely write-once-then-read-many or disjoint-key;
otherwise benchmark a sharded map first.

Create `snapshot.go`:

```go
// Package snapshot provides a generic, type-safe wrapper over sync.Map for
// the write-once-then-read-many workload sync.Map is actually tuned for: a
// control-plane snapshot that is rebuilt wholesale on every push and read
// concurrently by every request goroutine until the next push replaces it
// wholesale.
package snapshot

import "sync"

// Snapshot is a type-safe wrapper over sync.Map, restoring the compile-time
// key and value types that sync.Map's any-typed Store and Load throw away.
//
// Concurrency contract: Snapshot is safe for concurrent use by multiple
// goroutines, including concurrent Store, Load, LoadOrStore, and Range calls
// from different goroutines. This is the same guarantee the embedded
// sync.Map already makes; Snapshot adds a type layer on top of it, nothing
// less safe.
//
// Fitness contract: like the sync.Map it wraps, Snapshot is tuned for
// exactly two access patterns -- (1) write-once-then-read-many, where one
// goroutine populates the map and many goroutines only read it afterward
// (a reverse proxy's cluster snapshot, rebuilt on every control-plane push
// and read by every proxied request until the next push replaces it
// wholesale), and (2) disjoint-key workloads, where distinct goroutines
// touch non-overlapping keys. Outside those two patterns -- general
// read/write traffic on shared keys, or write-heavy workloads -- a
// striped/sharded map behind per-shard mutexes typically outperforms
// sync.Map by a wide margin. Benchmark before reaching for this package on
// a workload that is not one of the two above.
//
// The zero value of Snapshot is an empty, ready-to-use snapshot; New is
// provided for symmetry with the rest of this package's constructors.
type Snapshot[K comparable, V any] struct {
	m sync.Map
}

// New returns an empty Snapshot ready for concurrent use.
func New[K comparable, V any]() *Snapshot[K, V] {
	return &Snapshot[K, V]{}
}

// Store sets the value for key, overwriting any existing value.
func (s *Snapshot[K, V]) Store(key K, value V) {
	s.m.Store(key, value)
}

// Load returns the value stored for key and reports whether it was present.
//
// A stored entry whose dynamic type does not match V -- which cannot happen
// through this type's own API, but could if something outside the package
// reached into the embedded sync.Map -- is reported as (zero value, false)
// rather than panicking: Load performs the type assertion in comma-ok form
// internally, so a caller of this package can never trigger the panic that
// an unchecked v.(V) assertion on a raw sync.Map's Load result would.
func (s *Snapshot[K, V]) Load(key K) (V, bool) {
	raw, ok := s.m.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	v, ok := raw.(V)
	if !ok {
		var zero V
		return zero, false
	}
	return v, true
}

// LoadOrStore returns the existing value for key if one is present;
// otherwise it stores and returns value. The second result reports which
// case occurred, exactly as sync.Map.LoadOrStore does. A mis-typed existing
// entry is treated the same way Load treats one: reported as the zero value
// rather than panicking.
func (s *Snapshot[K, V]) LoadOrStore(key K, value V) (V, bool) {
	raw, loaded := s.m.LoadOrStore(key, value)
	v, ok := raw.(V)
	if !ok {
		var zero V
		return zero, loaded
	}
	return v, loaded
}

// Range calls f for each key/value pair currently in the Snapshot, in no
// particular order, stopping early if f returns false.
//
// Range follows sync.Map's own consistency contract: it reflects a
// point-in-time view that may or may not include entries stored
// concurrently with the Range call, and it is safe to call Store or Load
// from within f, but calling Range itself from within f will deadlock --
// the same restriction sync.Map.Range documents. A stored entry whose
// dynamic type does not match K or V is skipped rather than passed to f.
func (s *Snapshot[K, V]) Range(f func(K, V) bool) {
	s.m.Range(func(rawKey, rawValue any) bool {
		key, ok := rawKey.(K)
		if !ok {
			return true
		}
		value, ok := rawValue.(V)
		if !ok {
			return true
		}
		return f(key, value)
	})
}
```

### Using it

A control-plane push handler owns the write side: build a fresh `*Snapshot`
(or reuse one and overwrite every key) and hand it to request handlers
through whatever mechanism swaps it in — an `atomic.Pointer`, a package
variable protected by its own lock, dependency injection at request scope.
Nothing about `Snapshot` itself enforces the write-once discipline; that
discipline lives in how the caller wires it up, and the type's doc comment
says exactly which workload shapes make that wiring pay off.

Every value that comes back from `Load`, `LoadOrStore`, or a `Range`
callback is a plain `V`, not `any` — a caller never writes a type assertion
against this package's API, checked or unchecked, because `Snapshot`
already performed it internally. That is the entire value proposition: the
same `sync.Map` underneath, with the panic surface removed.

`ExampleSnapshot` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment.

```go
func ExampleSnapshot() {
	snap := New[string, endpoint]()
	snap.Store("cluster-a", endpoint{Addr: "10.0.0.1:8080"})
	snap.Store("cluster-b", endpoint{Addr: "10.0.0.2:8080"})

	if v, ok := snap.Load("cluster-a"); ok {
		fmt.Println("cluster-a:", v.Addr)
	}
	if _, ok := snap.Load("cluster-z"); !ok {
		fmt.Println("cluster-z: not found")
	}

	var keys []string
	snap.Range(func(k string, _ endpoint) bool {
		keys = append(keys, k)
		return true
	})
	slices.Sort(keys)
	fmt.Println("keys:", keys)

	// Output:
	// cluster-a: 10.0.0.1:8080
	// cluster-z: not found
	// keys: [cluster-a cluster-b]
}
```

`Range`'s visit order is unspecified — the same randomization every Go map
carries — so the example collects keys into a slice and sorts them before
printing, exactly the discipline the concepts lesson for this chapter
teaches for any map-derived output that must be deterministic.

### Tests

`TestUncheckedAssertionPanicsButWrapperReturnsFalse` is the test the whole
module exists for. It stores a `string` under a raw `sync.Map` where an
`endpoint` was expected — the mistake that happens when two unrelated call
sites disagree about what a map holds — and shows the naive read,
`v.(endpoint)` with no comma-ok, panics under `recover()`. It then
reproduces the identical mis-typed entry through `Snapshot`, reaching into
its unexported `m` field the way an external bug would have to, and shows
`Load` returns `(endpoint{}, false)` instead of panicking. The rest of the
table covers the ordinary contract: store/load round-trip, a missing key's
zero value, `LoadOrStore` keeping the first write, `Range` visiting every
entry and stopping early on `false`, and an empty snapshot's `Range` calling
nothing. `TestConcurrentWriteOnceReadMany` populates a `Snapshot` before
starting any reader, then runs twenty goroutines reading every key
concurrently — the pattern the type's doc comment says it is for — and
`-race` proves no reader synchronization is missing.

Create `snapshot_test.go`:

```go
package snapshot

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// endpoint stands in for the kind of value a control-plane snapshot really
// stores: an address a proxy dials, not the delimiter-joined strings other
// modules in this lesson worry about.
type endpoint struct {
	Addr string
}

func TestStoreLoad(t *testing.T) {
	t.Parallel()

	s := New[string, endpoint]()
	s.Store("cluster-a", endpoint{Addr: "10.0.0.1:8080"})

	v, ok := s.Load("cluster-a")
	if !ok || v.Addr != "10.0.0.1:8080" {
		t.Fatalf("Load(cluster-a) = %+v,%v, want {10.0.0.1:8080},true", v, ok)
	}
}

func TestLoadMissingKeyReturnsZeroValue(t *testing.T) {
	t.Parallel()

	s := New[string, endpoint]()
	v, ok := s.Load("missing")
	if ok {
		t.Fatalf("Load(missing) ok = true, want false")
	}
	if v != (endpoint{}) {
		t.Fatalf("Load(missing) value = %+v, want zero value", v)
	}
}

func TestLoadOrStore(t *testing.T) {
	t.Parallel()

	s := New[string, int]()

	v, loaded := s.LoadOrStore("hits", 1)
	if loaded || v != 1 {
		t.Fatalf("first LoadOrStore = %d,%v, want 1,false", v, loaded)
	}

	v, loaded = s.LoadOrStore("hits", 99)
	if !loaded || v != 1 {
		t.Fatalf("second LoadOrStore = %d,%v, want 1,true (existing value kept)", v, loaded)
	}
}

func TestRangeVisitsAllAndStopsEarly(t *testing.T) {
	t.Parallel()

	s := New[string, int]()
	for _, k := range []string{"a", "b", "c"} {
		s.Store(k, len(k))
	}

	var visited []string
	s.Range(func(k string, _ int) bool {
		visited = append(visited, k)
		return true
	})
	slices.Sort(visited)
	if want := []string{"a", "b", "c"}; !slices.Equal(visited, want) {
		t.Fatalf("Range visited = %v, want %v", visited, want)
	}

	count := 0
	s.Range(func(string, int) bool {
		count++
		return false // stop after the first
	})
	if count != 1 {
		t.Fatalf("Range with early stop visited %d entries, want 1", count)
	}
}

func TestEmptySnapshotRangeVisitsNothing(t *testing.T) {
	t.Parallel()

	s := New[string, int]()
	s.Range(func(string, int) bool {
		t.Fatal("Range called f on an empty Snapshot")
		return true
	})
}

// TestUncheckedAssertionPanicsButWrapperReturnsFalse is the heart of this
// module. A raw sync.Map with a mis-typed entry -- the state a bug
// elsewhere in a program could produce -- panics under an unchecked type
// assertion, which is exactly how a hand-rolled sync.Map caller usually
// reads a stored value. Snapshot.Load never lets that panic reach a caller
// of this package: its internal comma-ok assertion reports absence instead.
func TestUncheckedAssertionPanicsButWrapperReturnsFalse(t *testing.T) {
	t.Parallel()

	var raw sync.Map
	raw.Store("endpoint-1", "not-an-endpoint") // wrong type stored by mistake

	panicked := func() (panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		v, _ := raw.Load("endpoint-1")
		_ = v.(endpoint) // the antipattern: an unchecked type assertion
		return false
	}()
	if !panicked {
		t.Fatal("unchecked v.(endpoint) on a mis-typed sync.Map entry did not panic")
	}

	// Reproduce the same mis-typed entry through Snapshot, reaching into
	// its embedded sync.Map the way an external mistake would have to: not
	// possible through Snapshot's own type-safe Store.
	s := New[string, endpoint]()
	s.m.Store("endpoint-1", "not-an-endpoint")
	v, ok := s.Load("endpoint-1")
	if ok {
		t.Fatalf("Load returned ok=true for a mis-typed entry, want false; v=%+v", v)
	}
	if v != (endpoint{}) {
		t.Fatalf("Load returned %+v for a mis-typed entry, want the zero value", v)
	}
}

// TestConcurrentWriteOnceReadMany exercises the pattern Snapshot is tuned
// for: one goroutine populates the snapshot before any reader starts, then
// many goroutines read concurrently. Under -race this also proves no reader
// races with another reader, which sync.Map already guarantees and this
// wrapper does not weaken.
func TestConcurrentWriteOnceReadMany(t *testing.T) {
	t.Parallel()

	s := New[string, int]()
	for i := range 50 {
		s.Store(fmt.Sprintf("key-%d", i), i)
	}

	var wg sync.WaitGroup
	for r := range 20 {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			for i := range 50 {
				v, ok := s.Load(fmt.Sprintf("key-%d", i))
				if !ok || v != i {
					t.Errorf("reader %d: Load(key-%d) = %d,%v, want %d,true", reader, i, v, ok, i)
				}
			}
		}(r)
	}
	wg.Wait()
}

// ExampleSnapshot is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleSnapshot() {
	snap := New[string, endpoint]()
	snap.Store("cluster-a", endpoint{Addr: "10.0.0.1:8080"})
	snap.Store("cluster-b", endpoint{Addr: "10.0.0.2:8080"})

	if v, ok := snap.Load("cluster-a"); ok {
		fmt.Println("cluster-a:", v.Addr)
	}
	if _, ok := snap.Load("cluster-z"); !ok {
		fmt.Println("cluster-z: not found")
	}

	var keys []string
	snap.Range(func(k string, _ endpoint) bool {
		keys = append(keys, k)
		return true
	})
	slices.Sort(keys)
	fmt.Println("keys:", keys)

	// Output:
	// cluster-a: 10.0.0.1:8080
	// cluster-z: not found
	// keys: [cluster-a cluster-b]
}
```

## Review

`Snapshot` is correct when every value that crosses its API is the declared
`V`, never `any`, and when a mis-typed entry — however it got there —
degrades to "not found" instead of a panic. The mistake this module
inoculates against is the unchecked `v.(V)` a raw `sync.Map` caller writes
by habit: `TestUncheckedAssertionPanicsButWrapperReturnsFalse` shows that
assertion panicking on a mis-typed entry, and `Load`'s internal comma-ok
assertion avoiding it for exactly the same entry. None of that changes what
`sync.Map` is *for*: the type's doc comment is explicit that it fits only
write-once-then-read-many or disjoint-key workloads, and that a striped or
sharded map wins on general or write-heavy traffic. `Store`, `Load`,
`LoadOrStore`, and `Range` all forward to the embedded `sync.Map` and
inherit its concurrency guarantee unchanged — `TestConcurrentWriteOnceReadMany`
exercises exactly the shape this type is built for. Run
`go test -count=1 -race ./...` to confirm the table, the panic contrast, and
the concurrent read pattern.

## Resources

- [`sync.Map`](https://pkg.go.dev/sync#Map) — the type this package wraps, including its documented fitness for write-once-read-many and disjoint-key workloads.
- [Go blog: sync.Map internals](https://go.dev/src/sync/map.go) — the read-map/dirty-map design that explains why sync.Map favors read-heavy, write-light traffic.
- [Type assertions](https://go.dev/ref/spec#Type_assertions) — the single-result (panicking) versus comma-ok (checked) forms this module contrasts.
- [Go Wiki: CodeReviewComments on sync.Map](https://go.dev/wiki/CodeReviewComments) — general guidance on choosing between sync.Map and a mutex-guarded map.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-rib-import-sizehint-tool.md](16-rib-import-sizehint-tool.md) | Next: [18-content-hash-dedup-tool.md](18-content-hash-dedup-tool.md)
