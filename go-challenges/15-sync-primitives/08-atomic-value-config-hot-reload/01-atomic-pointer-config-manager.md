# Exercise 1: Immutable Config Manager with atomic.Pointer[T]

Your service reads its configuration on every request — connection limits,
timeouts, feature flags — but operations only pushes a new config every few
minutes. This exercise builds the core artifact of the whole lesson: a
`Manager` that publishes immutable `Config` snapshots behind an
`atomic.Pointer[Config]`, giving readers a single lock-free atomic load and
writers a single atomic store.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgmgr/                    independent module: example.com/cfgmgr
  go.mod
  config.go                type Config (MaxConnections, TimeoutMillis, FeatureFlags, Version)
  manager.go               type Manager; NewManager, Get, Update, Version
  cmd/
    demo/
      main.go              runnable demo: read v1, hot-swap to v2, read again
  manager_test.go          full contract suite + 50-reader/20-writer race test
```

- Files: `config.go`, `manager.go`, `cmd/demo/main.go`, `manager_test.go`.
- Implement: a plain `Config` value type and a `Manager` embedding `atomic.Pointer[Config]` with `NewManager(initial)`, `Get() *Config`, `Update(next *Config)`, and `Version() int`.
- Test: the contract suite (initial read, swap, previous-snapshot immutability, monotonic versions, an `Example`) plus a 50-reader/20-writer stress test and a `t.Context()`-driven reader stress test, all under `-race`.
- Verify: `go test -count=1 -race ./...`

### The design: one pointer, zero locks

The `Manager` is deliberately tiny. All state is one `atomic.Pointer[Config]`
embedded in the struct — embedded, not copied around, because the atomic types
carry a `noCopy` guard and `go vet` will flag any copy of a struct containing
one. `Get` is `ptr.Load()`; `Update` is `ptr.Store(next)`. That is the entire
synchronization story, and it is enough because of two facts working together:

1. The Go memory model makes the atomic `Store` synchronized-before any `Load`
   that observes it, so every write the updater made while *building* the new
   `Config` — including filling `FeatureFlags` — is visible to any reader that
   dereferences the snapshot.
2. Nothing ever writes to a `Config` after it is stored. Readers therefore
   read immutable memory, and reading immutable memory cannot race.

The second fact is a discipline, not a mechanism — the compiler will not stop
a caller from doing `m.Get().MaxConnections = 999`. The doc comments state the
contract, and the test suite pins it: `TestUpdateDoesNotMutatePrevious` holds
a snapshot across an `Update` and asserts it never changed.

Note what `Manager` does *not* do: it does not validate, order, or derive
configs. `Update` is a blind store, which means two concurrent updaters can
lose a write (last store wins, silently). That is acceptable when there is one
writer — a single reload goroutine — and exercise 6 adds the `CompareAndSwap`
defense for when there is not.

Create `config.go`:

```go
// Package cfgmgr publishes immutable configuration snapshots behind an
// atomic pointer: readers get lock-free consistent views, writers swap in
// complete new values.
package cfgmgr

// Config is one immutable configuration snapshot. Never mutate a Config
// after it has been passed to NewManager or Update: build a new one.
type Config struct {
	MaxConnections int
	TimeoutMillis  int
	FeatureFlags   map[string]bool
	Version        int
}
```

Create `manager.go`:

```go
package cfgmgr

import "sync/atomic"

// Manager holds the current Config snapshot. It must not be copied after
// first use (it embeds an atomic.Pointer, which carries a noCopy guard);
// share it by pointer.
type Manager struct {
	ptr atomic.Pointer[Config]
}

// NewManager returns a Manager serving initial as the current snapshot.
func NewManager(initial *Config) *Manager {
	m := &Manager{}
	m.ptr.Store(initial)
	return m
}

// Get returns the current config snapshot. It is always non-nil after
// construction. Callers must treat the returned Config as read-only, and
// should call Get once per request, threading the snapshot down, rather
// than re-loading mid-request and mixing two versions.
func (m *Manager) Get() *Config {
	return m.ptr.Load()
}

// Update atomically replaces the current snapshot. The caller must not
// mutate next after passing it in: ownership transfers here.
func (m *Manager) Update(next *Config) {
	m.ptr.Store(next)
}

// Version returns the version of the current snapshot. Useful for
// "did my push land?" checks and staleness probes.
func (m *Manager) Version() int {
	return m.ptr.Load().Version
}
```

### The runnable demo

The demo plays both roles: a reader printing the live snapshot, and an
operator hot-swapping v1 for v2. The two printed lines show the swap taking
effect without any lock and without restarting anything.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfgmgr"
)

func main() {
	v1 := &cfgmgr.Config{
		MaxConnections: 100,
		TimeoutMillis:  5000,
		FeatureFlags:   map[string]bool{"dark_mode": false},
		Version:        1,
	}
	m := cfgmgr.NewManager(v1)
	c := m.Get()
	fmt.Printf("v=%d max=%d dark=%v\n", c.Version, c.MaxConnections, c.FeatureFlags["dark_mode"])

	v2 := &cfgmgr.Config{
		MaxConnections: 200,
		TimeoutMillis:  3000,
		FeatureFlags:   map[string]bool{"dark_mode": true},
		Version:        2,
	}
	m.Update(v2)
	c = m.Get()
	fmt.Printf("v=%d max=%d dark=%v\n", c.Version, c.MaxConnections, c.FeatureFlags["dark_mode"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v=1 max=100 dark=false
v=2 max=200 dark=true
```

### Tests

The suite pins the full contract. `TestUpdateDoesNotMutatePrevious` is the
immutability guarantee: a snapshot handed to a reader must be frozen forever,
even as new versions land. `TestConcurrentReadsAndWrites` is the hard test —
50 readers doing 500 loads each while 20 writers swap configs; under `-race`
this proves there are no torn pointers and no shared mutable state.
`TestStressReadersWithContext` uses `t.Context()` (cancelled automatically
when the test ends) to bound background readers, the modern replacement for
hand-rolled stop channels. The test helper `mk` deep-copies the flags map so
each test builds genuinely independent snapshots.

Create `manager_test.go`:

```go
package cfgmgr

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
)

// mk builds an immutable Config for tests, deep-copying flags so the
// caller's map never aliases the stored snapshot.
func mk(maxConn, timeoutMs, version int, flags map[string]bool) *Config {
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

func TestGetReturnsInitial(t *testing.T) {
	t.Parallel()

	initial := mk(10, 100, 1, map[string]bool{"a": true})
	m := NewManager(initial)
	got := m.Get()
	if got.MaxConnections != 10 || got.Version != 1 || !got.FeatureFlags["a"] {
		t.Fatalf("Get = %+v", got)
	}
}

func TestUpdateSwapsConfig(t *testing.T) {
	t.Parallel()

	m := NewManager(mk(10, 100, 1, nil))

	m.Update(mk(20, 200, 2, map[string]bool{"x": true}))

	got := m.Get()
	if got.Version != 2 || got.MaxConnections != 20 {
		t.Fatalf("after Update: %+v", got)
	}
	if !got.FeatureFlags["x"] {
		t.Fatalf("flag x missing")
	}
}

func TestUpdateDoesNotMutatePrevious(t *testing.T) {
	t.Parallel()

	// Mutating nothing: storing a new config must not change the old one
	// a reader already holds.
	m := NewManager(mk(10, 100, 1, map[string]bool{"f": false}))

	snapshot := m.Get()
	if snapshot.FeatureFlags["f"] {
		t.Fatal("initial flag should be false")
	}

	m.Update(mk(20, 200, 2, map[string]bool{"f": true}))

	if snapshot.FeatureFlags["f"] {
		t.Fatalf("previous config mutated: flag f is now true")
	}
	if snapshot.Version != 1 || snapshot.MaxConnections != 10 {
		t.Fatalf("previous snapshot changed: %+v", snapshot)
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()

	m := NewManager(mk(1, 1, 1, nil))

	const readers = 50
	const writers = 20
	const reads = 500

	var wg sync.WaitGroup
	var versionSum atomic.Int64

	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for range reads {
				c := m.Get()
				if c == nil {
					panic("Get returned nil snapshot")
				}
				versionSum.Add(int64(c.Version))
			}
		}()
	}

	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			for j := range writers {
				m.Update(mk(i*100+j+100, 100, i*100+j+100, nil))
			}
		}()
	}

	wg.Wait()

	if got := versionSum.Load(); got <= 0 {
		t.Fatalf("versionSum = %d, want > 0", got)
	}
}

func TestVersionMonotonicUnderUpdates(t *testing.T) {
	t.Parallel()

	m := NewManager(mk(1, 1, 1, nil))
	for v := 2; v <= 10; v++ {
		m.Update(mk(v, v, v, nil))
		if got := m.Version(); got != v {
			t.Fatalf("Version after update to %d = %d", v, got)
		}
	}
}

func TestStressReadersWithContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	m := NewManager(mk(1, 1, 1, nil))

	var wg sync.WaitGroup
	var badSnapshots atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				c := m.Get()
				if c == nil || c.Version < 1 {
					badSnapshots.Add(1)
					return
				}
			}
		}()
	}

	for v := 2; v <= 1000; v++ {
		m.Update(mk(v, v, v, nil))
	}
	cancel()
	wg.Wait()

	if n := badSnapshots.Load(); n != 0 {
		t.Fatalf("%d readers observed an invalid snapshot", n)
	}
	if got := m.Version(); got != 1000 {
		t.Fatalf("final Version = %d, want 1000", got)
	}
}

func ExampleManager() {
	m := NewManager(&Config{MaxConnections: 10, Version: 1})
	m.Update(&Config{MaxConnections: 20, Version: 2})
	c := m.Get()
	fmt.Println(c.MaxConnections, c.Version)
	// Output: 20 2
}
```

## Review

The manager is correct when three things hold at once: `Get` never returns nil
after construction, a snapshot handed out before an `Update` is bit-identical
forever afterward, and `-race` stays silent while 50 readers and 20 writers
hammer it. If `TestUpdateDoesNotMutatePrevious` ever fails, some code path is
mutating a published `Config` — the fix is never to add a lock, it is to
restore the build-new-then-store discipline. Note also what the stress test
does not assert: that all readers see the same version at the same instant.
They legitimately do not; per-reader snapshot consistency is the contract, and
the sum-of-versions check only proves every load returned a valid snapshot.

A subtle point worth internalizing from the code: `Update` transfers ownership.
The caller builds the `Config`, hands it over, and must not touch it again —
that convention, stated in the doc comment and enforced by the test suite, is
what makes the single atomic store sufficient. Run
`go test -count=1 -race ./...` and `go vet ./...`; vet additionally proves no
`Manager` is ever copied by value.

## Resources

- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the typed atomic pointer: Load, Store, Swap, CompareAndSwap.
- [The Go Memory Model](https://go.dev/ref/mem) — the synchronized-before guarantee that makes build-then-store safe.
- [Go blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) — the design philosophy this idiom deliberately trades against for read-mostly data.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-copy-on-write-flag-builder.md](02-copy-on-write-flag-builder.md)
