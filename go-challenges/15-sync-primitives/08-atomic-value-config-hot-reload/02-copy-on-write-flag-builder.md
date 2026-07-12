# Exercise 2: Copy-on-Write Builders: Defensive Copies at the Trust Boundary

The atomic pointer swap in exercise 1 is only as safe as the immutability of
what it points to — and the easiest way to break that immutability is to embed
a caller's map by reference. This exercise builds the construction layer that
makes broken snapshots unrepresentable: builders that deep-copy at the trust
boundary and derive new configs from old ones without touching them.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgbuild/                  independent module: example.com/cfgbuild
  go.mod
  config.go                type Config; type Manager (atomic.Pointer[Config])
  builder.go               WithFlags (deep-copy constructor), WithOverrides + Override options
  cmd/
    demo/
      main.go              runnable demo: caller mutates its map after Store; snapshot unaffected
  builder_test.go          copy contract, nil-flags contract, base-untouched proof
```

- Files: `config.go`, `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: `WithFlags` cloning the caller's flags map (`maps.Clone`, nil normalized to an empty map), and `WithOverrides(base, ...Override)` deriving a new `Config` from a live snapshot without mutating it.
- Test: the preserved `TestWithFlagsCopiesInput` and nil-flags contract, plus proofs that `WithOverrides` leaves the base snapshot bit-identical and that no caller map ever aliases a stored config.
- Verify: `go test -count=1 -race ./...`

### Why the copy happens in the constructor, not at the call site

The snapshot idiom has exactly one dangerous seam: interior mutable state.
The pointer swap is atomic, but a `map[string]bool` inside the `Config` is
plain shared memory — if the constructor stores the caller's map by
reference, then any later write through the caller's variable races with
every reader of the snapshot, and a concurrent map read/write does not merely
corrupt data, it fatals the whole process (the runtime throws, unrecoverably).

You could document "callers must not reuse the map", but that pushes the
invariant onto every call site forever. The production answer is to make the
constructor the trust boundary: `WithFlags` clones the map with `maps.Clone`
and nothing that crosses into a `Config` is ever aliased by the outside
world. One subtlety: `maps.Clone(nil)` returns nil, and a nil `FeatureFlags`
would force every reader to nil-check before indexing. Reads of a nil map are
legal in Go (they return the zero value), so this is an ergonomics-and-JSON
contract rather than a crash fix — but normalizing nil to an empty map at
construction gives every snapshot the same shape and pins a guarantee tests
can state simply: `FeatureFlags` is never nil.

`WithOverrides` is the second half, and it is what ops tooling actually does:
"take the live config and bump one field". The temptation is
`base.TimeoutMillis = 200` — an in-place patch of a published snapshot, which
is precisely the race this whole lesson exists to prevent. Instead,
`WithOverrides` copies the base (struct copy plus a fresh clone of its flag
map), applies functional options to the copy, auto-increments the version so
the derived config is observably newer, and returns it. The base is never
written through; the returned config shares no mutable memory with it.

Create `config.go`:

```go
// Package cfgbuild builds immutable Config snapshots for atomic publication.
// All construction paths deep-copy mutable state at the boundary, so no
// caller-held map or slice ever aliases a stored snapshot.
package cfgbuild

import "sync/atomic"

// Config is one immutable configuration snapshot. Build it with WithFlags
// or derive it with WithOverrides; never mutate it after construction.
type Config struct {
	MaxConnections int
	TimeoutMillis  int
	FeatureFlags   map[string]bool
	Version        int
}

// Manager publishes the current snapshot. Share by pointer; do not copy.
type Manager struct {
	ptr atomic.Pointer[Config]
}

// NewManager returns a Manager serving initial.
func NewManager(initial *Config) *Manager {
	m := &Manager{}
	m.ptr.Store(initial)
	return m
}

// Get returns the current snapshot (read-only, always non-nil).
func (m *Manager) Get() *Config {
	return m.ptr.Load()
}

// Update atomically replaces the snapshot; ownership of next transfers.
func (m *Manager) Update(next *Config) {
	m.ptr.Store(next)
}
```

Create `builder.go`:

```go
package cfgbuild

import "maps"

// WithFlags builds an immutable Config, deep-copying flags so the caller
// may freely reuse or mutate its map afterward. A nil flags map is
// normalized to an empty one: FeatureFlags on a built Config is never nil.
func WithFlags(maxConn, timeoutMs, version int, flags map[string]bool) *Config {
	cp := maps.Clone(flags)
	if cp == nil {
		cp = map[string]bool{}
	}
	return &Config{
		MaxConnections: maxConn,
		TimeoutMillis:  timeoutMs,
		FeatureFlags:   cp,
		Version:        version,
	}
}

// Override mutates the private copy inside WithOverrides. Overrides never
// see or touch the base snapshot.
type Override func(*Config)

// OverrideMaxConnections sets MaxConnections on the derived config.
func OverrideMaxConnections(n int) Override {
	return func(c *Config) { c.MaxConnections = n }
}

// OverrideTimeoutMillis sets TimeoutMillis on the derived config.
func OverrideTimeoutMillis(ms int) Override {
	return func(c *Config) { c.TimeoutMillis = ms }
}

// OverrideFlag sets one feature flag on the derived config.
func OverrideFlag(name string, on bool) Override {
	return func(c *Config) { c.FeatureFlags[name] = on }
}

// WithOverrides derives a new Config from base without mutating it: the
// struct is copied, the flag map is cloned, the overrides are applied to
// the copy, and the version is incremented so the result is observably
// newer than its base.
func WithOverrides(base *Config, overrides ...Override) *Config {
	next := *base
	next.FeatureFlags = maps.Clone(base.FeatureFlags)
	if next.FeatureFlags == nil {
		next.FeatureFlags = map[string]bool{}
	}
	for _, o := range overrides {
		o(&next)
	}
	next.Version = base.Version + 1
	return &next
}
```

### The runnable demo

The demo stages the exact accident the builder prevents: the caller builds a
config from a map, stores it, then keeps mutating its own map (as request
code juggling a scratch map naturally would). The stored snapshot is
unaffected. It then plays the ops-tooling move — derive v2 from the live
snapshot with one field overridden — and prints both versions to show the
base survived.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfgbuild"
)

func main() {
	scratch := map[string]bool{"dark_mode": false}
	v1 := cfgbuild.WithFlags(100, 5000, 1, scratch)
	m := cfgbuild.NewManager(v1)

	scratch["dark_mode"] = true // caller reuses its map; snapshot must not care
	scratch["beta"] = true

	c := m.Get()
	fmt.Printf("v=%d dark=%v beta=%v\n", c.Version, c.FeatureFlags["dark_mode"], c.FeatureFlags["beta"])

	v2 := cfgbuild.WithOverrides(m.Get(),
		cfgbuild.OverrideTimeoutMillis(3000),
		cfgbuild.OverrideFlag("dark_mode", true),
	)
	m.Update(v2)

	fmt.Printf("v=%d timeout=%d dark=%v\n", m.Get().Version, m.Get().TimeoutMillis, m.Get().FeatureFlags["dark_mode"])
	fmt.Printf("base survived: v=%d timeout=%d dark=%v\n", v1.Version, v1.TimeoutMillis, v1.FeatureFlags["dark_mode"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v=1 dark=false beta=false
v=2 timeout=3000 dark=true
base survived: v=1 timeout=5000 dark=false
```

### Tests

`TestWithFlagsCopiesInput` is the preserved copy contract: mutate the input
map after construction and prove nothing leaked. `TestWithFlagsNilBecomesEmpty`
pins the nil-normalization contract. `TestWithOverridesLeavesBaseIdentical`
snapshots every field of the base before deriving and compares after — the
base must be bit-identical, including its flag map, checked with
`maps.Equal`. `TestDerivedSharesNoMemoryWithBase` attacks from the other
side: mutating the *derived* config's map must not reach the base.

Create `builder_test.go`:

```go
package cfgbuild

import (
	"fmt"
	"maps"
	"testing"
)

func TestWithFlagsCopiesInput(t *testing.T) {
	t.Parallel()

	input := map[string]bool{"a": true}
	c := WithFlags(1, 1, 1, input)

	input["a"] = false
	input["b"] = true

	if !c.FeatureFlags["a"] {
		t.Fatal("flag a should still be true in stored config")
	}
	if c.FeatureFlags["b"] {
		t.Fatal("flag b should not exist in stored config")
	}
}

func TestWithFlagsNilBecomesEmpty(t *testing.T) {
	t.Parallel()

	m := NewManager(WithFlags(1, 1, 1, nil))
	got := m.Get().FeatureFlags
	if got == nil {
		t.Fatal("FeatureFlags is nil; want empty non-nil map")
	}
	if len(got) != 0 {
		t.Fatalf("FeatureFlags = %v; want empty", got)
	}
}

func TestWithOverridesLeavesBaseIdentical(t *testing.T) {
	t.Parallel()

	base := WithFlags(100, 5000, 7, map[string]bool{"dark_mode": false, "beta": true})
	wantFlags := maps.Clone(base.FeatureFlags)

	derived := WithOverrides(base,
		OverrideMaxConnections(200),
		OverrideTimeoutMillis(3000),
		OverrideFlag("dark_mode", true),
	)

	if base.MaxConnections != 100 || base.TimeoutMillis != 5000 || base.Version != 7 {
		t.Fatalf("base scalar fields changed: %+v", base)
	}
	if !maps.Equal(base.FeatureFlags, wantFlags) {
		t.Fatalf("base flags changed: %v", base.FeatureFlags)
	}
	if derived.MaxConnections != 200 || derived.TimeoutMillis != 3000 {
		t.Fatalf("overrides not applied: %+v", derived)
	}
	if !derived.FeatureFlags["dark_mode"] || !derived.FeatureFlags["beta"] {
		t.Fatalf("derived flags wrong: %v", derived.FeatureFlags)
	}
	if derived.Version != 8 {
		t.Fatalf("derived Version = %d, want base+1 = 8", derived.Version)
	}
}

func TestDerivedSharesNoMemoryWithBase(t *testing.T) {
	t.Parallel()

	base := WithFlags(1, 1, 1, map[string]bool{"f": false})
	derived := WithOverrides(base)

	derived.FeatureFlags["f"] = true
	derived.FeatureFlags["new"] = true

	if base.FeatureFlags["f"] {
		t.Fatal("mutating derived flags leaked into base")
	}
	if _, ok := base.FeatureFlags["new"]; ok {
		t.Fatal("new derived flag leaked into base")
	}
}

func TestWithOverridesFromNilFlagsBase(t *testing.T) {
	t.Parallel()

	// A base built as a raw literal (bypassing WithFlags) may carry nil
	// flags; deriving from it must still yield a non-nil map so
	// OverrideFlag cannot panic on assignment to a nil map.
	base := &Config{Version: 1}
	derived := WithOverrides(base, OverrideFlag("x", true))

	if derived.FeatureFlags == nil || !derived.FeatureFlags["x"] {
		t.Fatalf("derived flags = %v; want {x:true}", derived.FeatureFlags)
	}
}

func TestStoredConfigNeverAliasesCallerMap(t *testing.T) {
	t.Parallel()

	input := map[string]bool{"a": true}
	m := NewManager(WithFlags(1, 1, 1, input))
	snapshot := m.Get()

	// Caller mutates its map after the config is live; a reader holding
	// the snapshot must be unaffected. Under -race, an aliased map here
	// would also be reported when readers run concurrently.
	input["a"] = false

	if !snapshot.FeatureFlags["a"] {
		t.Fatal("caller map mutation leaked into stored snapshot")
	}
}

func ExampleWithOverrides() {
	base := WithFlags(100, 5000, 1, map[string]bool{"dark_mode": false})
	derived := WithOverrides(base, OverrideTimeoutMillis(3000))
	fmt.Println(base.TimeoutMillis, derived.TimeoutMillis, derived.Version)
	// Output: 5000 3000 2
}
```

## Review

The module is correct when no mutable memory crosses the constructor boundary
in either direction: caller map mutations never reach a stored config, and
derived-config mutations never reach the base. The two directions fail for
different reasons — the first is a missing `maps.Clone` in `WithFlags`, the
second a missing re-clone in `WithOverrides` (copying the struct copies the
map *header*, not the map, so `next := *base` alone still aliases the flags).
That struct-copy trap is the one engineers hit most: a shallow copy of a
struct containing a map is not a copy of the map.

Also note what `WithOverrides` chose about versions: it derives
`base.Version + 1` rather than letting the caller pick, so a derived config
is always observably newer than its base. That choice is only sound because
this module assumes a single updater; when multiple writers derive from the
same base concurrently, two configs with the same version appear and
last-store-wins loses one — the `CompareAndSwap` gate in exercise 6 is the
fix. Verify with `go test -count=1 -race ./...`.

## Resources

- [maps.Clone](https://pkg.go.dev/maps#Clone) — shallow-clones a map; returns nil for a nil input, hence the normalization.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the publication primitive these builders feed.
- [The Go Memory Model](https://go.dev/ref/mem) — why memory written before the Store is safe to read after the Load, and aliased writes after it are not.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-atomic-pointer-config-manager.md](01-atomic-pointer-config-manager.md) | Next: [03-atomic-value-legacy-adapter.md](03-atomic-value-legacy-adapter.md)
