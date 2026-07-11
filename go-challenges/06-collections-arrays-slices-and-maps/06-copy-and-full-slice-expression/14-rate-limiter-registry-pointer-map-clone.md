# Exercise 14: Hot-Reloading a Rate-Limiter Registry Without maps.Clone's Shallow Trap

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Envoy's xDS control plane pushes new resource snapshots to a running proxy
without ever stopping traffic: a background goroutine applies the update to
live state while request-handling goroutines keep reading whatever snapshot
they already grabbed. A per-tenant rate-limiter registry needs the identical
shape -- a reloader pulling fresh limits from a config store and writing them
into the registry, while every request handler reads a `map[string]*Limiter`
snapshot to decide whether to allow or reject the call. The map is
`map[string]*Limiter`, not `map[string]Limiter`, because limiter state is
naturally mutable and handlers want to share one instance per tenant rather
than copy it on every request. That pointer-valued map is exactly where
`maps.Clone` stops doing what its name suggests.

`maps.Clone(m)` allocates a new map and copies every key and value into it
by assignment. For `map[string]*Limiter`, the value being assigned is a
pointer: copying a pointer copies the address, not the thing it points to.
The "snapshot" that comes out of `maps.Clone` is a new map spine pointing at
the *exact same* `*Limiter` values the live registry is holding. It looks
isolated -- adding a tenant to the snapshot does not appear in the live
registry -- right up until some caller does something completely reasonable
with a `*Limiter` it holds: adjusts a field on it. That mutation lands on
the live registry's own value, because there was never a second `Limiter`
to mutate, only a second pointer to the first one. This is the same shallow
-clone trap `http.Header.Clone` exists specifically to avoid for
`map[string][]string`, and it applies just as directly to any pointer- or
slice-valued map.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
limiterreg/                module example.com/limiterreg
  go.mod                    go 1.24
  limiterreg.go              Limiter, Clone; Registry, NewRegistry, Set, Get, Snapshot
  limiterreg_test.go         set/get table, snapshot independence, the
                             snapshotShallow contrast, concurrency, ExampleRegistry_Snapshot
```

- Files: `limiterreg.go`, `limiterreg_test.go`.
- Implement: `(*Limiter).Clone() *Limiter`, nil-safe; `NewRegistry() *Registry`; `(*Registry).Set(tenant string, limiter *Limiter) error` rejecting an empty tenant with `ErrEmptyTenant` and a nil limiter with `ErrNilLimiter`; `(*Registry).Get(tenant string) (*Limiter, bool)`; `(*Registry).Snapshot() map[string]*Limiter` cloning every `*Limiter` via `Limiter.Clone` so neither the map nor its values alias live registry state.
- Test: `Set`/`Get` including replacement and an unknown tenant; an empty registry's `Snapshot` being empty and non-nil; `Snapshot` independence from a mutation made through its result; a `snapshotShallow` contrast pinning the exact leak `maps.Clone` produces; `Limiter.Clone` of a nil receiver; a concurrent reloader-plus-readers test; and `ExampleRegistry_Snapshot` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/limiterreg
cd ~/go-exercises/limiterreg
go mod init example.com/limiterreg
go mod edit -go=1.24
```

### `maps.Clone` copies pointers, not what they point to

`maps.Clone` is defined, roughly, as: allocate a new map, then for every
key-value pair in the source, assign it into the new map. Assignment is the
whole story, and assignment of a pointer copies the pointer's bit pattern --
an address -- not the bytes at that address. For `map[string]int` this
distinction is invisible, because an `int` *is* its own value; copying it by
assignment is a real, independent copy. For `map[string]*Limiter` it is not:

```go
func snapshotShallow(r *Registry) map[string]*Limiter {
    return maps.Clone(r.limiters)   // new map, same *Limiter values
}

snap := snapshotShallow(r)
snap["acme"].Rate = 999             // writes through the live *Limiter
```

That last line does not touch the snapshot's own copy of anything, because
there is no such copy -- `snap["acme"]` and `r.limiters["acme"]` are the
same pointer, pointing at the same `Limiter` struct in memory. Every
goroutine that later calls `r.Get("acme")` sees `Rate: 999`, even though
nothing ever explicitly told the live registry to change. The fix mirrors
`http.Header.Clone`: clone the map spine, and clone every value too, so the
snapshot genuinely owns its own `Limiter` instances:

```go
func (r *Registry) Snapshot() map[string]*Limiter {
    out := make(map[string]*Limiter, len(r.limiters))
    for tenant, limiter := range r.limiters {
        out[tenant] = limiter.Clone()   // a fresh *Limiter, not the same one
    }
    return out
}
```

`maps.Clone` is not wrong or broken -- it does precisely what its
documentation says, a shallow copy. The mistake is expecting "shallow" to
mean "safe to mutate" for a map whose values are themselves references.

Create `limiterreg.go`:

```go
// Package limiterreg implements a per-tenant rate-limiter registry with the
// same hot-swap pattern Envoy uses for xDS resource snapshots: a background
// reloader keeps mutating the live registry while request-handling
// goroutines read an independent, point-in-time snapshot of it.
//
// The detail this package exists to get right is Snapshot's use of
// Limiter.Clone instead of maps.Clone. maps.Clone(m) copies a
// map[string]*Limiter's keys and pointers, not the Limiters they point to,
// so a "snapshot" built that way still shares every *Limiter with the live
// registry -- a reader that adjusts a field on one mutates the exact value
// every other goroutine is using. See Snapshot's doc comment.
package limiterreg

import (
	"errors"
	"sync"
)

// ErrEmptyTenant means Set was called with an empty tenant name.
var ErrEmptyTenant = errors.New("limiterreg: tenant must not be empty")

// ErrNilLimiter means Set was called with a nil limiter.
var ErrNilLimiter = errors.New("limiterreg: limiter must not be nil")

// Limiter holds one tenant's rate-limit configuration: a token-bucket rate
// and burst size.
type Limiter struct {
	Rate  int // requests permitted per second
	Burst int // maximum burst size
}

// Clone returns an independent copy of l. A nil receiver clones to nil.
func (l *Limiter) Clone() *Limiter {
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

// Registry maps tenant names to their current *Limiter and supports
// publishing an isolated snapshot for concurrent readers.
//
// Safe for concurrent use by multiple goroutines.
type Registry struct {
	mu       sync.RWMutex
	limiters map[string]*Limiter
}

// NewRegistry returns an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{limiters: make(map[string]*Limiter)}
}

// Set installs or replaces the *Limiter for tenant. It returns
// ErrEmptyTenant or ErrNilLimiter for invalid input.
//
// The Registry takes ownership of limiter: the caller must not mutate it
// after Set returns, since the same pointer is now the registry's live
// value for tenant and may be read concurrently by other goroutines.
func (r *Registry) Set(tenant string, limiter *Limiter) error {
	if tenant == "" {
		return ErrEmptyTenant
	}
	if limiter == nil {
		return ErrNilLimiter
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limiters[tenant] = limiter
	return nil
}

// Get returns the live *Limiter for tenant, or ok=false if tenant is
// unknown. The returned pointer aliases the Registry's own storage:
// callers must treat it as read-only, or call Snapshot for an isolated
// copy safe to mutate.
func (r *Registry) Get(tenant string) (limiter *Limiter, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	limiter, ok = r.limiters[tenant]
	return limiter, ok
}

// Snapshot returns an independent copy of the registry: a fresh map, and a
// fresh *Limiter for every tenant obtained via Limiter.Clone. Neither the
// map nor any *Limiter in it aliases the Registry's live state, so a
// caller may read, mutate, or hold the snapshot indefinitely without
// affecting, or being affected by, concurrent calls to Set.
func (r *Registry) Snapshot() map[string]*Limiter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*Limiter, len(r.limiters))
	for tenant, limiter := range r.limiters {
		out[tenant] = limiter.Clone()
	}
	return out
}
```

### Using it

Build one `Registry` at startup with `NewRegistry`, have your config
reloader call `Set` per tenant whenever it picks up a new limit, and have
every request-handling goroutine call `Snapshot` once per request (or once
per tick, cached) to get its own isolated view. `Get` exists for the
narrower case of reading one tenant's live limiter without copying, and its
doc comment is explicit that the result is read-only: mutate it and you are
mutating the registry's own state, exactly the mistake this module is
about. `Registry` is safe for concurrent use -- `Set`, `Get`, and `Snapshot`
all take the internal lock -- so the reloader and every reader goroutine can
call it without any coordination of their own.

`ExampleRegistry_Snapshot` in the test file is the executable demonstration
of this module: `go test` runs it and compares its stdout against the
`// Output:` comment, so the usage shown below cannot drift from the code.
It takes a snapshot, mutates the snapshot's copy, and shows the live
registry's value is unchanged.

### Tests

`TestSetRejectsInvalidInput`, `TestSetAndGet`, and
`TestSetReplacesExistingTenant` cover the ordinary `Set`/`Get` surface,
including replacing an existing tenant's limiter and looking up one that
was never set. `TestSnapshotOfEmptyRegistryIsEmptyNotNil` is the boundary
where the registry has no tenants at all. `TestLimiterCloneOfNilIsNil`
pins `Clone`'s nil-receiver case.

`TestSnapshotIsIndependent` is the heart of the module: it takes a
snapshot, mutates a field on one of its `*Limiter` values, and confirms the
live registry's own value for that tenant is unaffected -- which only holds
because `Snapshot` calls `Limiter.Clone` on the way out.
`TestSnapshotShallowLeaksMutation` runs the identical scenario through
`snapshotShallow`, an unexported helper built on bare `maps.Clone` and
never reachable from `Registry`, and confirms the opposite: the mutation
*does* reach the live registry, because `snapshotShallow`'s map shares
every `*Limiter` pointer with `r.limiters`. `TestRegistryIsSafeForConcurrentUse`
runs a reloader goroutine calling `Set` in a loop against twenty reader
goroutines calling `Snapshot`, the production shape this package models,
and relies on `-race` to catch any unsynchronized access the implementation
might have missed.

Create `limiterreg_test.go`:

```go
package limiterreg

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
)

// snapshotShallow is Snapshot as it is usually written the first time: it
// reaches for maps.Clone, which copies the map's key/pointer pairs but not
// the Limiters those pointers reference. It is never exported and never
// reachable from Registry; it exists only so the tests can pin the leak it
// produces.
func snapshotShallow(r *Registry) map[string]*Limiter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.limiters)
}

func TestSetRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Set("", &Limiter{Rate: 10}); !errors.Is(err, ErrEmptyTenant) {
		t.Errorf("Set(\"\", ...) error = %v, want ErrEmptyTenant", err)
	}
	if err := r.Set("acme", nil); !errors.Is(err, ErrNilLimiter) {
		t.Errorf("Set(\"acme\", nil) error = %v, want ErrNilLimiter", err)
	}
}

func TestSetAndGet(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Set("acme", &Limiter{Rate: 10, Burst: 5}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok := r.Get("acme")
	if !ok {
		t.Fatal("Get(\"acme\"): want ok=true")
	}
	if got.Rate != 10 || got.Burst != 5 {
		t.Fatalf("Get(\"acme\") = %+v, want {Rate:10 Burst:5}", got)
	}

	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(\"nope\"): want ok=false")
	}
}

func TestSetReplacesExistingTenant(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Set("acme", &Limiter{Rate: 10}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := r.Set("acme", &Limiter{Rate: 20}); err != nil {
		t.Fatalf("Set (replace): %v", err)
	}

	got, _ := r.Get("acme")
	if got.Rate != 20 {
		t.Fatalf("Get(\"acme\").Rate = %d, want 20", got.Rate)
	}
}

func TestSnapshotOfEmptyRegistryIsEmptyNotNil(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	snap := r.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() is nil, want an empty non-nil map")
	}
	if len(snap) != 0 {
		t.Fatalf("len(Snapshot()) = %d, want 0", len(snap))
	}
}

// TestSnapshotIsIndependent is the heart of the module: it proves a
// mutation made through a snapshot's *Limiter never reaches the live
// registry, which only holds because Snapshot clones each Limiter rather
// than sharing the live registry's pointers.
func TestSnapshotIsIndependent(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Set("acme", &Limiter{Rate: 10, Burst: 5}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	snap := r.Snapshot()
	snap["acme"].Rate = 999 // a reader adjusting its own copy

	live, _ := r.Get("acme")
	if live.Rate != 10 {
		t.Fatalf("live Rate = %d after mutating the snapshot, want unchanged 10", live.Rate)
	}
}

// TestSnapshotShallowLeaksMutation contrasts the buggy path directly:
// mutating a *Limiter obtained from snapshotShallow corrupts the exact
// value the live registry hands out to every other caller of Get.
func TestSnapshotShallowLeaksMutation(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Set("acme", &Limiter{Rate: 10, Burst: 5}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	shallow := snapshotShallow(r)
	shallow["acme"].Rate = 999

	live, _ := r.Get("acme")
	if live.Rate != 999 {
		t.Fatalf("live Rate = %d, want 999: maps.Clone should have shared the *Limiter", live.Rate)
	}
}

func TestLimiterCloneOfNilIsNil(t *testing.T) {
	t.Parallel()

	var l *Limiter
	if l.Clone() != nil {
		t.Fatal("Clone of a nil *Limiter should be nil")
	}
}

// TestRegistryIsSafeForConcurrentUse runs a reloader goroutine calling Set
// against several reader goroutines calling Snapshot, matching the
// production shape this package models. It relies on go test -race to
// surface any unsynchronized access.
func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Set("acme", &Limiter{Rate: 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for rate := 1; rate <= 50; rate++ {
			if err := r.Set("acme", &Limiter{Rate: rate}); err != nil {
				t.Errorf("Set: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := r.Snapshot()
			if l, ok := snap["acme"]; ok && (l.Rate < 1 || l.Rate > 50) {
				t.Errorf("snapshot Rate = %d, want in [1,50]", l.Rate)
			}
		}()
	}
	wg.Wait()
}

// ExampleRegistry_Snapshot is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below.
func ExampleRegistry_Snapshot() {
	r := NewRegistry()
	if err := r.Set("acme", &Limiter{Rate: 10, Burst: 5}); err != nil {
		panic(err)
	}

	snap := r.Snapshot()
	snap["acme"].Rate = 999 // a reader mutating its own copy

	live, _ := r.Get("acme")
	fmt.Println("snapshot rate:", snap["acme"].Rate)
	fmt.Println("live rate:", live.Rate)

	// Output:
	// snapshot rate: 999
	// live rate: 10
}
```

## Review

`Snapshot` is correct when a mutation made through its result never reaches
the live registry -- `TestSnapshotIsIndependent` pins exactly that, and
`TestSnapshotShallowLeaksMutation` shows what `maps.Clone` alone gets
wrong: a "snapshot" that shares every `*Limiter` pointer with live state,
so the very first field write through it corrupts what every other reader
sees. `maps.Clone` is not buggy; it does a real, shallow copy of the map
spine, which is genuinely enough for a `map[string]int`. It stops being
enough the moment the value type is a pointer, because copying a pointer by
assignment copies an address, not the data at that address -- the same fact
`http.Header.Clone` exists to work around for `map[string][]string`.
Around that core, `Set` rejects an empty tenant with `ErrEmptyTenant` and a
nil limiter with `ErrNilLimiter`, `Get` documents its result as
alias-and-read-only, and `Registry` is safe for concurrent use, which
`TestRegistryIsSafeForConcurrentUse` exercises directly with a reloader
goroutine racing readers under `-race`. Run `go test -count=1 -race ./...`
to confirm all of it, including `ExampleRegistry_Snapshot`, the runnable
demonstration `go test` checks against its `// Output:` comment.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — documented as a shallow copy; the source of the trap.
- [`http.Header.Clone`](https://pkg.go.dev/net/http#Header.Clone) — the standard library's own deep-clone-one-level-down pattern for a reference-valued map.
- [Envoy: Dynamic configuration (xDS)](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/dynamic_configuration) — the hot-swap snapshot pattern this exercise's Registry models.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the synchronization `Registry` uses to stay safe for concurrent use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-circuit-breaker-window-insert-shift.md](13-circuit-breaker-window-insert-shift.md) | Next: [15-frame-inspector-eof-vs-unexpected-eof.md](15-frame-inspector-eof-vs-unexpected-eof.md)
