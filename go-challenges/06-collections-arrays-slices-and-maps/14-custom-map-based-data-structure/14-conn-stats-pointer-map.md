# Exercise 14: Per-Connection Stats Registry via map[K]*V

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A reverse proxy exposes per-connection byte counters on an admin endpoint --
how many bytes each open connection has read and written -- so an operator
can spot the one client saturating a link without restarting anything. Two
goroutines touch each connection's counters: the connection's own goroutine,
bumping them on every single read and write, dozens or hundreds of times a
second on a busy connection, and the admin-endpoint goroutine, scraping
every connection's totals whenever a request comes in, on no predictable
schedule and possibly while the connection goroutine is mid-bump. The
natural home for "one small mutable struct per key" is a
`map[string]ConnStats`, and that is exactly the choice that makes every one
of those bumps slower than it needs to be, for a reason specific to Go maps:
a map element is not addressable.

`&m[k]` is a compile error. So is `m[k].BytesRead++`. The runtime is free to
move a map's entries around during a grow, so it never hands out a stable
address into the map's storage -- which means the *only* way to change one
field of a value stored in `map[K]V` is to read the whole struct out, mutate
the copy, and write the whole struct back in. For a byte counter bumped on
every read and write, that read-modify-write has to happen under a lock that
guards the entire map, because two goroutines racing a copy-out/copy-back on
the same map are not merely unsynchronized, they can lose an update outright
-- the second writer's copy-back overwrites the first writer's, and one of
the two increments vanishes. The result is a registry-wide lock taken once
per byte-count bump, serializing every connection's hot counters against
every other connection's, for no reason related to the data itself: two
different connections' counters have nothing to do with each other, and yet
they now contend on the same lock every time either one is touched.

`map[K]*V` sidesteps the constraint instead of working around it. A pointer
*is* addressable regardless of where the map itself stores it, so once a
caller has the `*ConnStats` a lookup returned, every later mutation goes
straight through that pointer and never touches the map or its lock again.
This module builds that as a package: `ConnStats`, whose counters are
`atomic.Int64` so concurrent bumps and concurrent reads need no lock at all,
and `Registry`, whose own mutex is needed only to create, find, or remove an
entry -- never to bump one.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
connstats/                module example.com/connstats
  go.mod                  go 1.24
  connstats.go             ConnStats; Registry; NewRegistry, GetOrCreate, Remove, Len
  connstats_test.go        stable-pointer table, the value-map contrast, concurrent
                           GetOrCreate and counter updates, Remove edge cases,
                           ExampleRegistry_GetOrCreate
```

- Files: `connstats.go`, `connstats_test.go`.
- Implement: `ConnStats` with `AddRead(n int64)`, `AddWritten(n int64)`, `Snapshot() (read, written int64)`, backed by `atomic.Int64` counters; `NewRegistry() *Registry`; `(*Registry) GetOrCreate(id string) *ConnStats` backed by `map[string]*ConnStats`, returning the same pointer for every call on the same `id`; `(*Registry) Remove(id string)`, a no-op on an unknown id and never touching a `ConnStats` a caller already holds; `(*Registry) Len() int`.
- Test: `GetOrCreate` returns a stable pointer across repeated and concurrent calls for the same id; `AddRead`/`AddWritten` are correct under concurrent bumps from many goroutines; the unexported `valueRegistry` contrast shows the `map[string]ConnStats` shape forced into copy-out/mutate/copy-back under its own lock on every single bump, while the pointer-map form never re-locks the registry after creation; `Remove` detaches an id without resetting a `ConnStats` a caller still holds, and a subsequent `GetOrCreate` for the same id starts fresh at zero; `Remove` on an unknown id and `GetOrCreate("")` are handled; and `ExampleRegistry_GetOrCreate` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/14-conn-stats-pointer-map
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/14-conn-stats-pointer-map
go mod edit -go=1.24
```

### A map element is not addressable; a pointer stored in one is

The Go spec is explicit that map index expressions are not addressable, and
the reason is operational, not arbitrary: the runtime may relocate a map's
entries during a grow, so it cannot promise that the address it hands out
today is still valid tomorrow. That rules out `&m[k]` at compile time and,
with it, any pattern that needs to mutate one field of a stored struct in
place:

```go
// valueRegistry.addRead — the shape map[string]ConnStats forces on you.
func (r *valueRegistry) addRead(id string, n int64) {
    r.mu.Lock()
    defer r.mu.Unlock()
    s := r.conns[id]        // copy the whole struct out
    s.BytesRead += n        // mutate the copy
    r.conns[id] = s         // copy the whole struct back in
}
```

Every byte-count bump here takes `r.mu`, the one lock that also guards every
other connection's entry, because the copy-out/copy-back is not atomic on
its own -- two goroutines each reading a stale copy and writing it back
would silently lose one of the two increments. Note that storing
`atomic.Int64` fields inside the struct would not rescue this shape either:
calling a pointer-receiver method like `(*atomic.Int64).Add` requires an
addressable value, and `m[id].BytesRead` is exactly as non-addressable as
`m[id]` itself. The map's element type is the whole problem, not the type of
its fields.

`map[K]*V` changes what the map stores: not the mutable struct, but a
pointer to one allocated on the heap. `&r.conns[id]` is still illegal, but
`r.conns[id]`, the pointer *value*, is a perfectly ordinary value to read out
of the map -- and once a caller has it, every field access and every method
call goes through that pointer directly, never back through the map. The
registry's lock is needed exactly once per connection, at the moment its
`*ConnStats` is first created; from then on the connection's own goroutine
owns that pointer for as long as the connection lives.

Create `connstats.go`:

```go
// Package connstats exposes per-connection byte counters for a reverse
// proxy: each connection's own goroutine bumps its counters on every read
// and write, while a separate goroutine scrapes every connection's totals
// for an admin endpoint at any time, concurrently and without coordination.
//
// The counters live behind a stable pointer per connection, obtained once
// from Registry.GetOrCreate, so that every later bump touches only that one
// connection's memory and never the registry's own lock.
package connstats

import (
	"sync"
	"sync/atomic"
)

// ConnStats holds the byte counters for one connection.
//
// ConnStats is safe for concurrent use by multiple goroutines: every
// counter is an atomic.Int64, so the connection's own goroutine may call
// AddRead/AddWritten while an unrelated goroutine calls Snapshot, with no
// lock and no risk of a torn read.
type ConnStats struct {
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64
}

// AddRead adds n to the connection's read-byte counter. n is typically the
// return value of a single Read call and may be added many times per
// second from the connection's own goroutine.
func (c *ConnStats) AddRead(n int64) {
	c.bytesRead.Add(n)
}

// AddWritten adds n to the connection's written-byte counter.
func (c *ConnStats) AddWritten(n int64) {
	c.bytesWritten.Add(n)
}

// Snapshot returns the counters' current values. Because each counter is
// updated independently, a concurrent Snapshot may observe read and
// written advancing at slightly different points in time relative to each
// other; it never observes a torn value for either counter individually.
func (c *ConnStats) Snapshot() (read, written int64) {
	return c.bytesRead.Load(), c.bytesWritten.Load()
}

// Registry maps connection IDs to their ConnStats.
//
// Registry is safe for concurrent use by multiple goroutines. Its mutex
// guards only the map itself -- creating, finding, and removing entries --
// never a byte-count update: once a caller holds the *ConnStats a
// GetOrCreate call returned, every subsequent AddRead or AddWritten on it
// bypasses the registry entirely, so unrelated connections' hot counters
// never contend with each other or with the registry's lock.
type Registry struct {
	mu    sync.RWMutex
	conns map[string]*ConnStats
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]*ConnStats)}
}

// GetOrCreate returns the ConnStats for id, creating one if this is the
// first time id has been seen. Every call for the same id, concurrent or
// not, returns the identical *ConnStats: the pointer is stable for the
// life of the connection, and the caller may retain it and mutate through
// it freely without any further interaction with the Registry.
//
// The common case -- id already registered -- takes only a read lock. A
// write lock, and a second existence check under it, is needed only the
// first time a given id is seen, which is what "guarded by a mutex only
// around creation" means in practice: the registry-wide lock is on the
// critical path for a connection's first byte, never for its millionth.
func (r *Registry) GetOrCreate(id string) *ConnStats {
	r.mu.RLock()
	if s, ok := r.conns[id]; ok {
		r.mu.RUnlock()
		return s
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.conns[id]; ok {
		// Another goroutine created it between the RUnlock above and this
		// Lock; return that one so every caller for id shares one pointer.
		return s
	}
	s := &ConnStats{}
	r.conns[id] = s
	return s
}

// Remove deletes id's entry from the registry. It does not reset or
// otherwise touch the ConnStats a caller may still be holding a pointer
// to; that value remains valid and keeps whatever counts it last had, it
// is simply no longer reachable through the registry. A subsequent
// GetOrCreate(id) allocates a fresh ConnStats starting at zero. Removing an
// id that was never registered is a no-op.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, id)
}

// Len reports the number of connections currently tracked.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.conns)
}
```

### Using it

Call `GetOrCreate(id)` exactly once when a connection is accepted and keep
the returned `*ConnStats` for that connection's lifetime; its own goroutine
then calls `AddRead`/`AddWritten` directly on that pointer, with no further
registry interaction at all. The admin-endpoint goroutine calls `GetOrCreate`
again (or would iterate a separate tracking structure keyed the same way in
a fuller service) to get the identical pointer and read it with `Snapshot`,
concurrently with the connection goroutine's bumps -- `atomic.Int64` is what
makes that safe without either side taking a lock. Call `Remove(id)` when
the connection closes; a `*ConnStats` some other goroutine is still holding
at that moment stays valid and keeps its last values; it is simply no longer
reachable through the registry.

The `Example` below is the runnable demonstration of this module: `go test`
executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift from the code that actually
runs.

```go
func ExampleRegistry_GetOrCreate() {
	reg := NewRegistry()

	// The connection's own goroutine: obtain the pointer once, then bump
	// it directly on every read and write.
	stats := reg.GetOrCreate("conn-42")
	stats.AddRead(1024)
	stats.AddRead(512)
	stats.AddWritten(2048)

	// The admin-endpoint goroutine: look the same connection back up and
	// scrape its current totals, concurrently and without coordinating
	// with the connection's goroutine beyond what the atomics guarantee.
	scraped := reg.GetOrCreate("conn-42")
	read, written := scraped.Snapshot()
	fmt.Printf("conn-42: read=%d written=%d\n", read, written)
	fmt.Println("same connection object:", scraped == stats)

	reg.Remove("conn-42")
	fmt.Println("tracked connections after Remove:", reg.Len())

	// Output:
	// conn-42: read=1536 written=2048
	// same connection object: true
	// tracked connections after Remove: 0
}
```

### Tests

`TestGetOrCreateReturnsStablePointer` and
`TestGetOrCreateIsIdempotentUnderConcurrency` pin the property the whole
package exists to guarantee: repeated calls, including fifty concurrent
ones for the same id, all return the identical `*ConnStats`, and `Len`
reports exactly one entry rather than one per racing goroutine.
`TestAddReadAndAddWrittenAreConcurrencySafe` runs twenty goroutines bumping
one connection's counters under `-race` and checks the final totals against
the exact expected sums. `TestValueMapSerializesEveryBump` is the heart of
the module: `valueRegistry` and `valueStats` are unexported and unreachable
from the package API, and exist only so the test can pin, numerically, what
the `map[K]V` shape costs -- five hundred bumps on one connection take the
registry-wide lock five hundred times, once per bump -- immediately
contrasted with the same five hundred bumps through `Registry`+`ConnStats`,
which never touches the registry's lock after the first `GetOrCreate`.
`TestRemoveDetachesWithoutResettingHeldPointer`,
`TestRemoveUnknownIsNoop`, and `TestGetOrCreateEmptyID` cover the edge
cases at the API boundary.

Create `connstats_test.go`:

```go
package connstats

import (
	"fmt"
	"sync"
	"testing"
)

// valueStats and valueRegistry demonstrate the map[string]V shape that
// GetOrCreate deliberately avoids. A map element is not addressable, so
// there is no way to write &conns[id] and bump a field through it -- that
// is a compile error -- and even storing atomic.Int64 fields in the struct
// would not help, since calling a pointer-receiver method on a
// non-addressable value is also a compile error. The only option left is
// to copy the whole struct out, mutate the copy, and copy it back in,
// under the one lock that guards the entire map. Neither type is ever
// exported or reachable from the package API; they exist only so the test
// can pin what this shape costs.
type valueStats struct {
	BytesRead    int64
	BytesWritten int64
}

type valueRegistry struct {
	mu               sync.Mutex
	conns            map[string]valueStats
	lockAcquisitions int
}

func newValueRegistry() *valueRegistry {
	return &valueRegistry{conns: make(map[string]valueStats)}
}

// addRead is the naive bump: copy-out, mutate, copy-back, all under the
// registry-wide lock. That lock guards every connection, not just id, so
// it is taken and released once per byte-count update, no matter how many
// distinct connections exist.
func (r *valueRegistry) addRead(id string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lockAcquisitions++
	s := r.conns[id]
	s.BytesRead += n
	r.conns[id] = s
}

func TestGetOrCreateReturnsStablePointer(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	first := reg.GetOrCreate("conn-1")
	second := reg.GetOrCreate("conn-1")
	if first != second {
		t.Fatal("GetOrCreate returned two different pointers for the same id")
	}

	first.AddRead(10)
	read, _ := second.Snapshot()
	if read != 10 {
		t.Fatalf("Snapshot via the second pointer read = %d, want 10 (both point at the same ConnStats)", read)
	}
}

func TestGetOrCreateIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	const goroutines = 50

	results := make([]*ConnStats, goroutines)
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = reg.GetOrCreate("shared-conn")
		}(i)
	}
	wg.Wait()

	want := results[0]
	for i, got := range results {
		if got != want {
			t.Fatalf("goroutine %d got a different *ConnStats than goroutine 0; GetOrCreate is not idempotent under concurrency", i)
		}
	}
	if got := reg.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1; concurrent GetOrCreate calls for one id must create exactly one entry", got)
	}
}

func TestAddReadAndAddWrittenAreConcurrencySafe(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	stats := reg.GetOrCreate("hot-conn")

	const goroutines, bumpsPerGoroutine = 20, 200
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range bumpsPerGoroutine {
				stats.AddRead(1)
				stats.AddWritten(2)
			}
		}()
	}
	wg.Wait()

	read, written := stats.Snapshot()
	wantRead := int64(goroutines * bumpsPerGoroutine)
	wantWritten := int64(goroutines * bumpsPerGoroutine * 2)
	if read != wantRead {
		t.Errorf("BytesRead = %d, want %d", read, wantRead)
	}
	if written != wantWritten {
		t.Errorf("BytesWritten = %d, want %d", written, wantWritten)
	}
}

func TestRemoveDetachesWithoutResettingHeldPointer(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	stats := reg.GetOrCreate("conn-1")
	stats.AddRead(42)

	reg.Remove("conn-1")
	if got := reg.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0 after Remove", got)
	}

	// A caller that already holds the pointer keeps seeing its last value;
	// Remove only detaches the registry's reference, it does not zero the
	// ConnStats out from under whoever still has it.
	read, _ := stats.Snapshot()
	if read != 42 {
		t.Fatalf("Snapshot after Remove = %d, want 42 (Remove must not reset a held ConnStats)", read)
	}

	// A fresh GetOrCreate for the same id after Remove starts a new
	// counter at zero, not the old one's 42.
	fresh := reg.GetOrCreate("conn-1")
	if fresh == stats {
		t.Fatal("GetOrCreate after Remove returned the old, detached ConnStats instead of a fresh one")
	}
	freshRead, _ := fresh.Snapshot()
	if freshRead != 0 {
		t.Fatalf("fresh ConnStats read = %d, want 0", freshRead)
	}
}

func TestRemoveUnknownIsNoop(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.GetOrCreate("conn-1")
	reg.Remove("never-registered")
	if got := reg.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1; removing an unknown id must not touch conn-1", got)
	}
}

func TestGetOrCreateEmptyID(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	s := reg.GetOrCreate("")
	s.AddRead(5)
	if s2 := reg.GetOrCreate(""); s2 != s {
		t.Fatal("GetOrCreate(\"\") did not return a stable pointer for the empty-string id")
	}
}

// TestValueMapSerializesEveryBump is the heart of the module. It contrasts
// valueRegistry, whose only option is read-modify-write under one shared
// lock, against Registry+ConnStats, where a byte-count bump never touches
// the registry's lock at all once the connection is registered.
func TestValueMapSerializesEveryBump(t *testing.T) {
	t.Parallel()

	naive := newValueRegistry()
	const bumps = 500
	for range bumps {
		naive.addRead("conn-1", 1)
	}
	if naive.lockAcquisitions != bumps {
		t.Fatalf("valueRegistry.lockAcquisitions = %d, want %d: every single bump had to "+
			"take the registry-wide lock", naive.lockAcquisitions, bumps)
	}

	reg := NewRegistry()
	stats := reg.GetOrCreate("conn-1")
	for range bumps {
		stats.AddRead(1) // never touches reg.mu
	}
	read, _ := stats.Snapshot()
	if read != bumps {
		t.Fatalf("ConnStats.Snapshot read = %d, want %d", read, bumps)
	}
}

// ExampleRegistry_GetOrCreate is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment
// below.
func ExampleRegistry_GetOrCreate() {
	reg := NewRegistry()

	// The connection's own goroutine: obtain the pointer once, then bump
	// it directly on every read and write.
	stats := reg.GetOrCreate("conn-42")
	stats.AddRead(1024)
	stats.AddRead(512)
	stats.AddWritten(2048)

	// The admin-endpoint goroutine: look the same connection back up and
	// scrape its current totals, concurrently and without coordinating
	// with the connection's goroutine beyond what the atomics guarantee.
	scraped := reg.GetOrCreate("conn-42")
	read, written := scraped.Snapshot()
	fmt.Printf("conn-42: read=%d written=%d\n", read, written)
	fmt.Println("same connection object:", scraped == stats)

	reg.Remove("conn-42")
	fmt.Println("tracked connections after Remove:", reg.Len())

	// Output:
	// conn-42: read=1536 written=2048
	// same connection object: true
	// tracked connections after Remove: 0
}
```

## Review

`Registry` is correct when a connection's hot read/write path never
contends with an unrelated connection's, and when the admin endpoint's scrape
never corrupts or blocks a live bump -- both properties `map[string]*ConnStats`
delivers essentially for free, because the pointer a `GetOrCreate` call
returns is addressable and stable regardless of anything the map does
afterward. The trap is that `map[string]ConnStats` looks like the more
obvious choice and is quietly much slower under load: a map element is not
addressable, so mutating one field requires a full copy-out/mutate/copy-back
under a lock that guards the whole map, turning every byte-count bump into a
serialization point for every connection, not just the one being updated --
`TestValueMapSerializesEveryBump` counts exactly that, five hundred lock
acquisitions for five hundred bumps on one connection. `Registry`'s own
`RWMutex` is only ever on the critical path for a connection's first
`GetOrCreate`; every bump after that goes straight through the pointer via
`atomic.Int64`, no lock at all. `Remove` detaches an id from the registry
without disturbing a `ConnStats` some other goroutine still holds. Run
`go test -count=1 -race ./...` to confirm the stable-pointer and concurrency
properties, the value-map contrast, and the `Remove` edge cases.

## Resources

- [Go Spec: Address operators](https://go.dev/ref/spec#Address_operators) — the rule that map index expressions are not addressable.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — the lock-free counter type `ConnStats` is built on.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the double-checked-locking pattern `GetOrCreate` uses to keep the common case cheap.
- [Go Wiki: CodeReviewComments — Mutexes](https://go.dev/wiki/CodeReviewComments#mutexes) — general guidance on where a lock should and should not live.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-conn-registry-map-compaction.md](13-conn-registry-map-compaction.md) | Next: [15-error-ratio-histogram-nan-guard.md](15-error-ratio-histogram-nan-guard.md)
