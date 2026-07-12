# Exercise 4: Hot-Reload Config Lock-Free with atomic.Pointer

Backends reload configuration and feature flags without restarting: a watcher sees
a changed file or a control-plane push and swaps the live config. On the read path
this is the hottest code in the service — every request reads config — so a lock is
a bad fit. This module builds a `ConfigStore` on `atomic.Pointer[Config]` that
gives readers a lock-free immutable snapshot and lets a writer swap the whole
pointer atomically, and proves under `-race` that the swap is the only
synchronization needed.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
hotconfig/                 independent module: example.com/hotconfig
  go.mod                   module example.com/hotconfig
  hotconfig.go             Config (immutable snapshot); ConfigStore.Load/Reload/Swap on atomic.Pointer[Config]
  cmd/
    demo/
      main.go              runnable demo: readers observe a consistent generation across a reload
  hotconfig_test.go        concurrent readers + writer under -race; snapshot-consistency; never-nil
```

- Files: `hotconfig.go`, `cmd/demo/main.go`, `hotconfig_test.go`.
- Implement: an immutable `Config` snapshot and a `ConfigStore` wrapping `atomic.Pointer[Config]` with `Load() *Config`, `Reload(*Config)`, and `Swap(*Config) *Config`.
- Test: concurrent readers loop `Load` while a writer loops `Reload`; assert every observed snapshot is internally consistent (one generation) and never nil, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/09-pointer-aliasing-and-data-races/04-atomic-pointer-config-hotreload/cmd/demo
cd go-solutions/09-pointers/09-pointer-aliasing-and-data-races/04-atomic-pointer-config-hotreload
```

### Copy-on-write, and the rule that makes it safe

The pattern is copy-on-write. Readers call `Load()` and get a `*Config` — one
atomic pointer load, no lock, so the read path scales to any number of goroutines
with zero contention. The writer never mutates the live config; it builds an
entirely new `Config` value and `Store`s the new pointer. A reader that loaded the
old pointer keeps using the old snapshot; a reader that loads after the store sees
the new one. There is no in-between, because a pointer store is atomic — a reader
can never observe a half-updated `Config`.

The invariant that makes this correct is strict and worth stating as a rule: **a
published `*Config` is never mutated after the store.** Readers alias the pointed-to
value, so mutating a field of a config that has been made visible is a race against
every reader that already loaded it. That is why `Config` carries a generation
number and its fields are treated as read-only after construction: the test asserts
that every snapshot a reader observes belongs to a single generation, which can
only hold if the writer always swaps a fresh whole value and never edits in place.

`Swap` returns the previous pointer, which is useful when the caller wants to
inspect or reclaim the old config; `Reload` is the fire-and-forget form. `Load`
must never return nil, so the store seeds an initial config at construction — a
reader that races the very first writer still sees a valid snapshot, never a nil
dereference.

Create `hotconfig.go`:

```go
package hotconfig

import "sync/atomic"

// Config is an immutable configuration snapshot. Once published via ConfigStore
// it must never be mutated; readers alias it, so an in-place write would race.
// The Generation and the two fields it labels are all set at construction and
// then read-only, which is what lets a reader trust that a loaded snapshot is
// internally consistent.
type Config struct {
	Generation int
	MaxConns   int
	Timeout    int // milliseconds
}

// ConfigStore holds the live config behind an atomic pointer. Readers Load a
// snapshot lock-free; a watcher Reloads a brand-new snapshot.
type ConfigStore struct {
	ptr atomic.Pointer[Config]
}

// NewConfigStore seeds the store so Load never returns nil.
func NewConfigStore(initial *Config) *ConfigStore {
	s := &ConfigStore{}
	s.ptr.Store(initial)
	return s
}

// Load returns the current snapshot with a single atomic pointer load and no
// lock. The returned *Config must be treated as read-only.
func (s *ConfigStore) Load() *Config {
	return s.ptr.Load()
}

// Reload atomically publishes a new snapshot. The caller must pass a freshly
// built Config and must not mutate it afterward.
func (s *ConfigStore) Reload(next *Config) {
	s.ptr.Store(next)
}

// Swap atomically publishes next and returns the previous snapshot.
func (s *ConfigStore) Swap(next *Config) *Config {
	return s.ptr.Swap(next)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/hotconfig"
)

func main() {
	store := hotconfig.NewConfigStore(&hotconfig.Config{Generation: 1, MaxConns: 100, Timeout: 500})

	var wg sync.WaitGroup
	// One writer publishes new generations.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for gen := 2; gen <= 4; gen++ {
			store.Reload(&hotconfig.Config{Generation: gen, MaxConns: gen * 100, Timeout: gen * 500})
		}
	}()

	// Readers always observe a consistent generation.
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := store.Load()
			// MaxConns and Timeout always belong to the same generation.
			_ = c.MaxConns == c.Generation*100 && c.Timeout == c.Generation*500
		}()
	}
	wg.Wait()

	final := store.Load()
	fmt.Printf("final generation: %d, maxconns: %d, timeout: %d\n", final.Generation, final.MaxConns, final.Timeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
final generation: 4, maxconns: 400, timeout: 2000
```

### Tests

`TestConcurrentReadReload` is the load-bearing test: many readers loop calling
`Load` while a writer loops calling `Reload` with configs whose fields are a pure
function of the generation (`MaxConns == Generation*100`, `Timeout ==
Generation*500`). Each reader asserts the loaded snapshot satisfies that
relationship — proving it observed one whole generation, never a torn mix of old
and new fields — and that it is never nil. Run under `-race`, it confirms the
atomic swap is the only synchronization needed. `TestSwapReturnsPrevious` pins the
`Swap` contract.

Create `hotconfig_test.go`:

```go
package hotconfig

import (
	"fmt"
	"sync"
	"testing"
)

func consistent(c *Config) bool {
	return c != nil && c.MaxConns == c.Generation*100 && c.Timeout == c.Generation*500
}

func TestConcurrentReadReload(t *testing.T) {
	t.Parallel()

	store := NewConfigStore(&Config{Generation: 1, MaxConns: 100, Timeout: 500})

	var wg sync.WaitGroup

	// Writer: publish generations 2..200, each a fresh whole value.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for gen := 2; gen <= 200; gen++ {
			store.Reload(&Config{Generation: gen, MaxConns: gen * 100, Timeout: gen * 500})
		}
	}()

	// Readers: every loaded snapshot must be one consistent generation.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				if c := store.Load(); !consistent(c) {
					t.Errorf("observed torn snapshot: %+v", c)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := store.Load(); got.Generation != 200 {
		t.Fatalf("final generation = %d, want 200", got.Generation)
	}
}

func TestSwapReturnsPrevious(t *testing.T) {
	t.Parallel()

	store := NewConfigStore(&Config{Generation: 1, MaxConns: 100, Timeout: 500})
	prev := store.Swap(&Config{Generation: 2, MaxConns: 200, Timeout: 1000})

	if prev.Generation != 1 {
		t.Fatalf("Swap returned generation %d, want previous 1", prev.Generation)
	}
	if got := store.Load(); got.Generation != 2 {
		t.Fatalf("after Swap, Load generation = %d, want 2", got.Generation)
	}
}

func TestLoadNeverNil(t *testing.T) {
	t.Parallel()

	store := NewConfigStore(&Config{Generation: 1, MaxConns: 100, Timeout: 500})
	if store.Load() == nil {
		t.Fatal("Load returned nil from a seeded store")
	}
}

func Example() {
	store := NewConfigStore(&Config{Generation: 1, MaxConns: 100, Timeout: 500})
	store.Reload(&Config{Generation: 2, MaxConns: 200, Timeout: 1000})
	fmt.Println(store.Load().Generation)
	// Output: 2
}
```

## Review

The store is correct when every reader observes a whole, single-generation
snapshot and never nil, with a clean `-race` run — proving the atomic pointer swap
is sufficient synchronization with no lock on the read path. The mistake that
breaks it is mutating a published `Config` in place ("just bumping the timeout on
the live config"): readers already alias it, so the write races them. The discipline
is copy-on-write — build a fresh value, `Reload`/`Swap`, never touch the old one.
The consistency invariant in the test (`MaxConns == Generation*100`) is the machine
check for exactly this: if the writer ever edited fields of a live config
independently, a reader would catch a snapshot where the fields disagree.

## Resources

- [`sync/atomic` — `Pointer[T]`](https://pkg.go.dev/sync/atomic#Pointer) — `Load`, `Store`, `Swap`, `CompareAndSwap` on a typed pointer.
- [The Go Memory Model](https://go.dev/ref/mem) — why an atomic store/load pair provides the happens-before edge for the snapshot.
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) — confirming lock-free copy-on-write is race-clean.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-defensive-copy-repository.md](05-defensive-copy-repository.md)
