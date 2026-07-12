# Exercise 7: Hot-Reloadable Config With atomic.Pointer and a Nil-Before-First-Store Guard

A config that reloads on SIGHUP or a file-watch, read by every request goroutine
with no lock on the hot path, is a natural fit for `atomic.Pointer[T]`. This
module builds that store and handles its one sharp edge: an `atomic.Pointer[T]`
that has never been `Store`d `Load`s as a nil `*T`, so the accessor must guard
that window with a baked-in default snapshot.

This module is fully self-contained.

## What you'll build

```text
confstore/                independent module: example.com/confstore
  go.mod                  go 1.24
  store.go                type Config; type Store wrapping atomic.Pointer[Config]; Current guards nil
  cmd/
    demo/
      main.go             runnable demo: default before store, snapshot after
  store_test.go           default-before-store, after-store, -race readers vs writer
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: a `Store` wrapping `atomic.Pointer[Config]` with `Reload(*Config)` to swap in a snapshot and `Current()` that returns the default snapshot before the first store and never nil.
Test: `Current()` before any store returns the default (not nil); after a store it returns the new snapshot; a `-race` test with reader goroutines `Load`ing while a writer `Store`s proves no data race and no nil deref.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/04-nil-pointers-and-guard-checks/07-atomic-pointer-hot-config-reload/cmd/demo
cd go-solutions/09-pointers/04-nil-pointers-and-guard-checks/07-atomic-pointer-hot-config-reload
go mod edit -go=1.24
```

### The lock-free read path and its nil window

`atomic.Pointer[T]` holds a `*T` that can be `Store`d and `Load`ed atomically. A
background reloader builds a fresh `*Config` and `Store`s it; request goroutines
`Load` the current pointer with no mutex, no reader-writer contention, and a
single atomic read on the hot path. Swapping a whole immutable snapshot at once
also means a reader never observes a half-updated config — it sees the old
snapshot or the new one, never a mix.

The sharp edge is the zero value. A `Store` whose `atomic.Pointer[Config]` has
never been written `Load`s as `nil`. If `Current()` returned that directly, the
first request — the one that arrives before the reloader has run even once —
would dereference nil and panic. The guard turns the nil window into the default:

```go
func (s *Store) Current() *Config {
	c := s.p.Load()
	if c == nil {
		return defaultConfig // baked-in snapshot for the pre-first-store window
	}
	return c
}
```

Now every caller gets a usable, non-nil `*Config` from the very first request.
Because the default is a package-level immutable snapshot shared read-only, and
`Reload` only ever replaces the pointer (never mutates a snapshot in place),
there is no data race between the default and a concurrent reload.

Create `store.go`:

```go
package confstore

import "sync/atomic"

// Config is an immutable snapshot swapped atomically on reload. Never mutate a
// Config after publishing it; build a new one and Reload instead.
type Config struct {
	MaxConns    int
	FeatureFlag bool
	UpstreamURL string
}

// defaultConfig is the baked-in snapshot returned before the first Reload.
var defaultConfig = &Config{
	MaxConns:    10,
	FeatureFlag: false,
	UpstreamURL: "http://localhost:8080",
}

// Store holds the current config snapshot behind a lock-free atomic pointer.
type Store struct {
	p atomic.Pointer[Config]
}

// Current returns the live snapshot, or the default before any Reload. It never
// returns nil.
func (s *Store) Current() *Config {
	c := s.p.Load()
	if c == nil {
		return defaultConfig
	}
	return c
}

// Reload atomically publishes a new snapshot. A nil argument is ignored so a
// failed reload cannot blank out the config.
func (s *Store) Reload(c *Config) {
	if c == nil {
		return
	}
	s.p.Store(c)
}

// TrySwap atomically replaces old with next only if the current snapshot is
// still old, reporting success. It is the compare-and-swap variant for reloads
// that must not clobber a concurrent update.
func (s *Store) TrySwap(old, next *Config) bool {
	return s.p.CompareAndSwap(old, next)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/confstore"
)

func main() {
	var s confstore.Store

	// Before any reload: the default snapshot, not nil.
	c := s.Current()
	fmt.Printf("initial: maxconns=%d flag=%v\n", c.MaxConns, c.FeatureFlag)

	// A reload publishes a new snapshot atomically.
	s.Reload(&confstore.Config{MaxConns: 100, FeatureFlag: true, UpstreamURL: "http://prod:9000"})
	c = s.Current()
	fmt.Printf("reloaded: maxconns=%d flag=%v url=%s\n", c.MaxConns, c.FeatureFlag, c.UpstreamURL)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial: maxconns=10 flag=false
reloaded: maxconns=100 flag=true url=http://prod:9000
```

### Tests

The tests pin the nil window (default before store), the swap (new snapshot after
store), and the concurrency contract: many readers calling `Current()` while a
writer `Reload`s must run clean under `-race` and never see nil.

Create `store_test.go`:

```go
package confstore

import (
	"sync"
	"testing"
)

func TestCurrentBeforeReloadIsDefault(t *testing.T) {
	t.Parallel()

	var s Store
	c := s.Current()
	if c == nil {
		t.Fatal("Current() returned nil before any Reload")
	}
	if c.MaxConns != defaultConfig.MaxConns {
		t.Fatalf("MaxConns = %d, want default %d", c.MaxConns, defaultConfig.MaxConns)
	}
}

func TestCurrentAfterReload(t *testing.T) {
	t.Parallel()

	var s Store
	s.Reload(&Config{MaxConns: 50, FeatureFlag: true})
	c := s.Current()
	if c.MaxConns != 50 || !c.FeatureFlag {
		t.Fatalf("Current() = %+v, want MaxConns=50 FeatureFlag=true", *c)
	}
}

func TestReloadNilIsIgnored(t *testing.T) {
	t.Parallel()

	var s Store
	s.Reload(&Config{MaxConns: 50})
	s.Reload(nil) // must not blank the config
	if s.Current().MaxConns != 50 {
		t.Fatalf("nil Reload clobbered config: %+v", *s.Current())
	}
}

func TestConcurrentReadWriteIsRaceFree(t *testing.T) {
	t.Parallel()

	var s Store
	var wg sync.WaitGroup

	// Writer: keep publishing new snapshots.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			s.Reload(&Config{MaxConns: i})
		}
	}()

	// Readers: keep reading; each must get a non-nil snapshot.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				if c := s.Current(); c == nil {
					t.Error("Current() returned nil under concurrency")
					return
				}
			}
		}()
	}
	wg.Wait()
}
```

## Review

The store is correct when `Current()` never returns nil and always returns either
the default (before the first `Reload`) or the most recently published snapshot.
`TestCurrentBeforeReloadIsDefault` pins the nil window; `TestConcurrentReadWriteIsRaceFree`
under `-race` proves the lock-free read path is safe against a concurrent writer.
If the guard in `Current()` were removed, the concurrency test would panic on the
first read that races the writer.

The mistake avoided: `Load`ing the atomic pointer and dereferencing without
handling the pre-first-store nil, or mutating a published snapshot in place
(which would race a reader) instead of building a new one and swapping.

## Resources

- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — `Load`, `Store`, `Swap`, and `CompareAndSwap` on a typed atomic pointer.
- [The Go Memory Model](https://go.dev/ref/mem) — why atomic publish/consume of an immutable snapshot is safe without a lock.
- [Go Blog: Go 1.19 sync/atomic types](https://go.dev/blog/go1.19) — introduction of the typed atomic types including `atomic.Pointer`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-optional-observability-hook-guard.md](06-optional-observability-hook-guard.md) | Next: [08-nested-pointer-chain-guard.md](08-nested-pointer-chain-guard.md)
