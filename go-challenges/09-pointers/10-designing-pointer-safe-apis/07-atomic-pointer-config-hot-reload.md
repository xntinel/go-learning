# Exercise 7: Lock-Free Config Snapshots with atomic.Pointer[Config]

A service that reloads its configuration at runtime has a read-heavy, write-rare
piece of shared state: thousands of requests read the config per second, an
operator reloads it once an hour. This module builds a `ConfigStore` around
`atomic.Pointer[Config]`, so readers `Load()` a complete, consistent snapshot with
no lock and a reload atomically swaps the whole pointer.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
config/                     independent module: example.com/config
  go.mod                    go 1.25
  config.go                 Config (immutable snapshot); ConfigStore over atomic.Pointer[Config]
  cmd/
    demo/
      main.go               load-before-store (nil), reload, load-after (new), hold-old-pointer
  config_test.go            nil-before-store, concurrent -race readers, immutable-snapshot, CAS tests
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `ConfigStore` with `Load() *Config`, `Reload(*Config)`, and `CompareAndSwap(old, new *Config) bool`, all delegating to an embedded `atomic.Pointer[Config]`.
- Test: `Load` before any `Store` returns nil and the reader handles it; after `Reload`, concurrent goroutines each `Load` a non-nil self-consistent snapshot under `-race`; a reader holding an old pointer still sees old values after a reload (snapshots are immutable); a `CompareAndSwap` conditional-reload test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/config/cmd/demo
cd ~/go-exercises/config
go mod init example.com/config
go mod edit -go=1.25
```

### The atomic-pointer publication pattern

`atomic.Pointer[Config]` holds a single pointer word that can be read and written
atomically. Readers call `Load()` and receive whatever `*Config` was most recently
stored — a complete value, never a half-written one, because the only thing that
changed on a reload was one pointer, published in a single atomic operation. A
writer builds an entirely *new* `Config` value, fully populated, and then `Store`s
its address. There is no window in which a reader sees a config with the new
timeout but the old feature flags: it sees the old pointer or the new pointer,
nothing in between.

This beats a mutex-guarded, mutated-in-place config for a read-heavy path because
readers never block and never contend. A `sync.RWMutex` would serialize readers
against the rare write and add lock/unlock overhead to every hot-path read; the
atomic pointer adds a single atomic load. The trade is one allocation per reload,
which is free in practice because reloads are rare.

### The invariant: never mutate a published snapshot

The whole scheme rests on one rule: once a `*Config` is stored, it is *immutable*.
A reader may `Load()` a pointer and hold it across a reload — it will keep seeing
the old values, which is correct, because that snapshot was frozen at the instant
it was published. If a writer instead mutated the fields of an already-published
`Config`, every reader holding that pointer would observe the change mid-flight,
torn or not, defeating the point. So `Reload` always builds a fresh value; it never
reaches into the current one. The immutability test pins this: a reader captures a
pointer, a reload swaps in a new config, and the captured pointer still reports the
old values.

`Load` on an empty store returns `nil` (the zero value of an `atomic.Pointer`), so
a reader on the cold path must handle nil — typically by falling back to defaults.
The demo and tests exercise that branch.

Create `config.go`:

```go
package config

import (
	"sync/atomic"
	"time"
)

// Config is an immutable configuration snapshot. Once published via Reload it is
// never mutated; a reload swaps in a wholly new value.
type Config struct {
	Timeout     time.Duration
	MaxConns    int
	FeatureFlag bool
}

// ConfigStore publishes Config snapshots lock-free. Readers Load; a writer
// Reloads a fully-built snapshot.
type ConfigStore struct {
	p atomic.Pointer[Config]
}

// Load returns the current snapshot, or nil if nothing has been published yet.
func (s *ConfigStore) Load() *Config {
	return s.p.Load()
}

// Reload atomically publishes a new snapshot. The caller must not mutate c after
// passing it here.
func (s *ConfigStore) Reload(c *Config) {
	s.p.Store(c)
}

// CompareAndSwap publishes next only if the current snapshot is still old,
// enabling a conditional reload. It reports whether the swap happened.
func (s *ConfigStore) CompareAndSwap(old, next *Config) bool {
	return s.p.CompareAndSwap(old, next)
}
```

### The runnable demo

The demo shows the full lifecycle: a `Load` before any publish (nil, handled with
a default), a `Reload`, a `Load` of the new snapshot, and a captured old pointer
that keeps its old value across a second reload.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/config"
)

func main() {
	var store config.ConfigStore

	if store.Load() == nil {
		fmt.Println("before publish: nil (use defaults)")
	}

	store.Reload(&config.Config{Timeout: 5 * time.Second, MaxConns: 100, FeatureFlag: false})
	old := store.Load()
	fmt.Printf("after first reload: timeout=%s flag=%t\n", old.Timeout, old.FeatureFlag)

	store.Reload(&config.Config{Timeout: 2 * time.Second, MaxConns: 200, FeatureFlag: true})
	fmt.Printf("after second reload: timeout=%s flag=%t\n", store.Load().Timeout, store.Load().FeatureFlag)

	// The captured old pointer is an immutable snapshot; it still reads the old values.
	fmt.Printf("held old snapshot: timeout=%s flag=%t\n", old.Timeout, old.FeatureFlag)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before publish: nil (use defaults)
after first reload: timeout=5s flag=false
after second reload: timeout=2s flag=true
held old snapshot: timeout=5s flag=false
```

### Tests

`TestLoadBeforeStoreIsNil` pins the cold-path nil. `TestConcurrentReadersUnderReload`
spins many reader goroutines that `Load` and read fields while a writer reloads,
asserting each observed snapshot is internally self-consistent (a fixed
relationship between its fields) — under `-race` this proves there is no torn read.
`TestSnapshotIsImmutable` captures a pointer, reloads, and asserts the captured
pointer still holds the old values. `TestCompareAndSwap` drives a conditional
reload: a CAS from the current pointer succeeds, a CAS from a stale pointer fails.

Create `config_test.go`:

```go
package config

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLoadBeforeStoreIsNil(t *testing.T) {
	t.Parallel()
	var s ConfigStore
	if s.Load() != nil {
		t.Fatal("Load before any Store = non-nil, want nil")
	}
}

func TestConcurrentReadersUnderReload(t *testing.T) {
	t.Parallel()
	var s ConfigStore
	// Snapshots maintain the invariant MaxConns == 10 * (Timeout in seconds).
	mk := func(sec int) *Config {
		return &Config{Timeout: time.Duration(sec) * time.Second, MaxConns: sec * 10}
	}
	s.Reload(mk(1))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				c := s.Load()
				if c == nil {
					t.Error("Load returned nil after publish")
					return
				}
				if c.MaxConns != int(c.Timeout/time.Second)*10 {
					t.Errorf("torn snapshot: %+v", c)
					return
				}
			}
		}()
	}
	for i := 1; i <= 500; i++ {
		s.Reload(mk(i%9 + 1))
	}
	wg.Wait()
}

func TestSnapshotIsImmutable(t *testing.T) {
	t.Parallel()
	var s ConfigStore
	s.Reload(&Config{Timeout: 5 * time.Second, FeatureFlag: false})
	held := s.Load()

	s.Reload(&Config{Timeout: 2 * time.Second, FeatureFlag: true})

	if held.Timeout != 5*time.Second || held.FeatureFlag {
		t.Fatalf("held snapshot changed after reload: %+v", held)
	}
	if s.Load().Timeout != 2*time.Second {
		t.Fatal("current snapshot did not reflect the reload")
	}
}

func TestCompareAndSwap(t *testing.T) {
	t.Parallel()
	var s ConfigStore
	first := &Config{Timeout: time.Second}
	s.Reload(first)

	next := &Config{Timeout: 2 * time.Second}
	if !s.CompareAndSwap(first, next) {
		t.Fatal("CAS from current pointer should succeed")
	}
	stale := &Config{Timeout: 9 * time.Second}
	if s.CompareAndSwap(first, stale) {
		t.Fatal("CAS from a stale pointer should fail")
	}
	if s.Load() != next {
		t.Fatal("store should hold the CAS-published pointer")
	}
}

func ExampleConfigStore() {
	var s ConfigStore
	fmt.Println(s.Load() == nil)

	s.Reload(&Config{Timeout: 5 * time.Second, FeatureFlag: true})
	fmt.Println(s.Load().FeatureFlag)
	// Output:
	// true
	// true
}
```

## Review

The store is correct when `Load` returns a complete snapshot (or nil before the
first publish), when concurrent readers under a reload never observe a torn config
(the self-consistency invariant holds under `-race`), and when a held pointer keeps
its old values across a reload. The invariant that makes it all work is immutability
after publication: `Reload` builds a fresh `Config` and never mutates the current
one. The mistakes to avoid are mutating a published snapshot (every holder sees it),
holding a `Load`ed pointer and expecting it to reflect later reloads (re-`Load` to
observe them), and — a `go vet` catch — copying the `ConfigStore` by value, since it
embeds an `atomic.Pointer` that must not be copied after use. Pass the store by
pointer.

## Resources

- [`sync/atomic.Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — `Load`, `Store`, `Swap`, `CompareAndSwap`.
- [Go memory model](https://go.dev/ref/mem) — why an atomic publish is visible to a subsequent atomic load.
- [`go vet` copylocks](https://pkg.go.dev/cmd/vet) — flags copying a type that embeds `atomic.Pointer`/`sync.Mutex`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-nil-guard-http-handler.md](06-nil-guard-http-handler.md) | Next: [08-receiver-method-set-addressability.md](08-receiver-method-set-addressability.md)
