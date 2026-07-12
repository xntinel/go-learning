# Exercise 10: Hot-Swapping Config with atomic.Pointer[Config]

Live config reload wants readers to see a fully-formed config with no lock on the
read path. `atomic.Pointer[Config]` delivers it: each reload builds a fresh
`&Config{...}` snapshot and `Store`s it atomically, and readers `Load` the current
immutable snapshot. This exercise builds that holder, proves it is race-free under
concurrent readers and writers, and ties composite-literal construction to
lock-free safe publication.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
configreload/                 independent module: example.com/configreload
  go.mod                      go 1.26
  holder.go                   Config, Holder wrapping atomic.Pointer[Config]; Load, Reload, TryReload
  cmd/
    demo/
      main.go                 runnable demo: load, reload, observe old reader keeps its snapshot
  holder_test.go              -race concurrent readers/writers + CompareAndSwap semantics + distinct-snapshot
```

Files: `holder.go`, `cmd/demo/main.go`, `holder_test.go`.
Implement: a `Holder` wrapping `atomic.Pointer[Config]` with `Load`, `Reload`
(build a fresh `&Config{...}` and `Store`), and `TryReload` (CompareAndSwap).
Test: a `-race` test with concurrent `Load`ers and a `Store`ing writer asserting
readers always see a fully-initialized `Config`; `TryReload` swaps only when the
expected old pointer matches; reloading builds a distinct pointer so old readers
keep their snapshot.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/03-new-vs-composite-literal/10-atomic-pointer-config-reload/cmd/demo
cd go-solutions/09-pointers/03-new-vs-composite-literal/10-atomic-pointer-config-reload
```

### Why atomic.Pointer and immutable snapshots

A service that reloads config on a SIGHUP or a file-watch needs two things: readers
must never block on the hot path, and readers must never observe a half-updated
config. The wrong design mutates the fields of one shared `*Config` in place — a
reload sets `MaxConns`, then `Version`, then `Feature`, and a reader that `Load`s
between two of those writes sees a torn config with the new `MaxConns` and the old
`Version`. It is also a data race, which `-race` flags immediately.

The right design never mutates a published config. Each reload builds a brand-new
`&Config{...}` snapshot — fully initialized in one composite literal — and publishes
it with a single atomic `Store`. Readers call `Load`, which returns the current
snapshot pointer atomically; they then read that snapshot's fields with no lock,
because the snapshot is immutable and will never change. When a new snapshot is
stored, in-flight readers keep the pointer they already loaded, so they continue to
see a consistent older config until they `Load` again. There is no torn read
because nothing is mutated in place; there is no lock on the read path because the
`Load` is a single atomic word read. This is the safe-publication pattern:
construct fully, then publish the pointer atomically.

`atomic.Pointer[T]` (Go 1.19+, generic) provides exactly `Load() *T`,
`Store(*T)`, and `CompareAndSwap(old, new *T) bool`. `TryReload` uses
`CompareAndSwap` so a reloader can publish only if the config it is replacing is
still the one it observed — the way you avoid clobbering a concurrent reload that
already advanced the config. The `Config` fields must be treated as read-only once
published; the discipline is "build a new one, never edit a live one."

Create `holder.go`:

```go
package configreload

import "sync/atomic"

// Config is an immutable configuration snapshot. Once published via Holder, its
// fields must never be mutated; a reload builds a fresh Config instead.
type Config struct {
	Version  int
	MaxConns int
	Feature  bool
}

// Holder publishes Config snapshots to concurrent readers with no read-side lock.
type Holder struct {
	current atomic.Pointer[Config]
}

// NewHolder returns a Holder seeded with an initial snapshot.
func NewHolder(initial *Config) *Holder {
	h := &Holder{}
	h.current.Store(initial)
	return h
}

// Load returns the current snapshot atomically. Readers treat it as immutable and
// may read its fields with no lock.
func (h *Holder) Load() *Config {
	return h.current.Load()
}

// Reload builds a fresh snapshot from the given values and publishes it. It
// unconditionally replaces the current snapshot and returns the new pointer.
func (h *Holder) Reload(version, maxConns int, feature bool) *Config {
	next := &Config{Version: version, MaxConns: maxConns, Feature: feature}
	h.current.Store(next)
	return next
}

// TryReload publishes next only if the current snapshot is still expect. It
// returns true on success. Use it to avoid clobbering a concurrent reload.
func (h *Holder) TryReload(expect, next *Config) bool {
	return h.current.CompareAndSwap(expect, next)
}
```

### The runnable demo

The demo loads a snapshot, holds the pointer, reloads a new one, and shows that the
held pointer still sees the old config while a fresh `Load` sees the new one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configreload"
)

func main() {
	h := configreload.NewHolder(&configreload.Config{Version: 1, MaxConns: 100})

	// A reader that loaded the old snapshot and holds onto it.
	old := h.Load()

	// Reload publishes a fresh snapshot.
	h.Reload(2, 250, true)

	fresh := h.Load()
	fmt.Printf("held old:  version=%d conns=%d feature=%v\n",
		old.Version, old.MaxConns, old.Feature)
	fmt.Printf("fresh load: version=%d conns=%d feature=%v\n",
		fresh.Version, fresh.MaxConns, fresh.Feature)
	fmt.Printf("distinct pointers: %v\n", old != fresh)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
held old:  version=1 conns=100 feature=false
fresh load: version=2 conns=250 feature=true
distinct pointers: true
```

### Tests

`TestConcurrentReadersDuringReload` runs many `Load`ers alongside a writer under
`-race`, asserting every observed snapshot is internally consistent (a config's
fields always belong together, never torn). `TestTryReloadCASSemantics` proves
`CompareAndSwap` swaps only against the current pointer. `TestReloadBuildsDistinctPointer`
proves an old reader keeps its snapshot after a reload.

Create `holder_test.go`:

```go
package configreload

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentReadersDuringReload(t *testing.T) {
	t.Parallel()

	// Snapshots are consistent iff MaxConns == Version*100. A torn read would
	// break that invariant.
	h := NewHolder(&Config{Version: 1, MaxConns: 100})

	var wg sync.WaitGroup

	// Writer: publish a sequence of consistent snapshots.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := 2; v <= 500; v++ {
			h.Reload(v, v*100, v%2 == 0)
		}
	}()

	// Readers: every loaded snapshot must satisfy the invariant.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				c := h.Load()
				if c.MaxConns != c.Version*100 {
					t.Errorf("torn read: version=%d maxConns=%d", c.Version, c.MaxConns)
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestTryReloadCASSemantics(t *testing.T) {
	t.Parallel()

	initial := &Config{Version: 1, MaxConns: 100}
	h := NewHolder(initial)

	next := &Config{Version: 2, MaxConns: 200}
	if !h.TryReload(initial, next) {
		t.Fatal("TryReload against the current pointer should succeed")
	}
	if h.Load() != next {
		t.Fatal("after successful TryReload, Load should return next")
	}

	// A stale expect must fail: initial is no longer current.
	stale := &Config{Version: 3, MaxConns: 300}
	if h.TryReload(initial, stale) {
		t.Fatal("TryReload against a stale pointer should fail")
	}
	if h.Load() != next {
		t.Fatal("failed TryReload must not change the current snapshot")
	}
}

func TestReloadBuildsDistinctPointer(t *testing.T) {
	t.Parallel()

	h := NewHolder(&Config{Version: 1, MaxConns: 100})
	old := h.Load()

	h.Reload(2, 200, true)
	fresh := h.Load()

	if old == fresh {
		t.Fatal("reload must build a distinct pointer")
	}
	if old.Version != 1 || old.MaxConns != 100 {
		t.Fatalf("old snapshot mutated: %+v", *old)
	}
	if fresh.Version != 2 || fresh.MaxConns != 200 {
		t.Fatalf("fresh snapshot wrong: %+v", *fresh)
	}
}

// ExampleHolder shows that an old reader keeps its snapshot after a reload while a
// fresh Load sees the new one. It is sequential, so the output is deterministic.
func ExampleHolder() {
	h := NewHolder(&Config{Version: 1, MaxConns: 100})
	old := h.Load()
	h.Reload(2, 250, true)
	fresh := h.Load()
	fmt.Println(old.Version, fresh.Version, old != fresh)
	// Output:
	// 1 2 true
}
```

## Review

The holder is correct when concurrent readers never see a torn config and never
block: `TestConcurrentReadersDuringReload` encodes an invariant into each
snapshot (`MaxConns == Version*100`) and asserts it across thousands of loads
under `-race`, which passes only because every published `Config` is built whole
in one `&Config{...}` literal and swapped in with a single atomic `Store` — no
in-place field mutation, so no torn read. `TestReloadBuildsDistinctPointer` pins
the immutability guarantee: an old reader's pointer keeps pointing at the old
snapshot after a reload, so long-running readers stay consistent. `TryReload`'s
`CompareAndSwap` is the tool for "publish only if nothing changed under me," which
`TestTryReloadCASSemantics` checks on both the success and stale paths. The
anti-pattern to reject is mutating a shared `*Config`'s fields in place on reload;
it races and hands readers half-updated state. Build a fresh snapshot, publish the
pointer atomically.

## Resources

- [sync/atomic.Pointer](https://pkg.go.dev/sync/atomic#Pointer) — `Load`, `Store`, `CompareAndSwap` on a typed pointer.
- [The Go Memory Model](https://go.dev/ref/mem) — why an atomic store safely publishes a fully-constructed value.
- [Go 1.19 release notes: atomic types](https://go.dev/doc/go1.19#atomic_types) — the addition of the typed atomic wrappers like `atomic.Pointer`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../04-nil-pointers-and-guard-checks/00-concepts.md](../04-nil-pointers-and-guard-checks/00-concepts.md)
