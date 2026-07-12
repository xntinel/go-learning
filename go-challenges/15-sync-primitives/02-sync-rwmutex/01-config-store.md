# Exercise 1: A concurrent config store guarded by RWMutex

Almost every backend service holds a small bag of configuration — hostnames,
feature flags, tuning knobs — that is read on every single request and written
only on a rare reload. That is the canonical read-heavy server-state shape, and a
`sync.RWMutex` is its canonical guard. This exercise builds that store: reads take
a shared `RLock`, the rare write takes an exclusive `Lock`, and a `Snapshot`
method hands callers a defensive copy so they can never race the internal map.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
configstore/                 independent module: example.com/configstore
  go.mod                     module example.com/configstore
  config.go                  type Config; Get/Len (RLock), Set (Lock), Snapshot (RLock + maps.Clone)
  cmd/
    demo/
      main.go                runnable demo: set, read, snapshot, mutate, re-read
  config_test.go             concurrent read/write race test, snapshot-independence test, Example
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: a `*Config` wrapping `map[string]string` with `Get`, `Set`, `Len`, and `Snapshot` returning an independent copy under `RLock`.
Test: 64 readers racing one writer under `-race` with `Len` staying stable; a snapshot that a later `Set` does not mutate; an `Example` with pinned output.
Verify: `go test -count=1 -race ./...`

### Why RWMutex is the right guard here

The access pattern is lopsided: `Get` and `Len` fire on the hot path (every
request that consults config), while `Set` fires only on a reload or an admin
change. Under a plain `Mutex`, two concurrent `Get` calls serialize even though
neither mutates anything. Under an `RWMutex`, they both hold `RLock` at the same
time and proceed in parallel; only the rare `Set` takes the exclusive `Lock` and
briefly excludes readers. This is exactly the ratio the type is built for.

`Get` and `Len` take `RLock`/`RUnlock`; `Set` takes `Lock`/`Unlock`. Each uses
`defer` to release, so an early return or a panic can never leave the lock held.
Because the reads only inspect the map and never mutate it, the shared read lock
is safe — the invariant is that no code path mutates protected state without the
exclusive `Lock`.

### Why Snapshot returns a copy

Returning the internal map directly would be a latent race: the caller would hold
a reference to the very map that `Set` mutates under `Lock`, so any iteration over
it outside the lock races every future write. `Snapshot` takes `RLock` and returns
`maps.Clone(c.settings)` — a shallow copy the caller owns outright. A later `Set`
mutates the internal map and leaves the returned copy untouched. This is the
standard way to hand out a consistent, race-free view of guarded state: copy under
the lock, release, let the caller do whatever it likes with its own copy. For a
`map[string]string` the shallow clone is a full deep copy because the values are
immutable strings.

Create `config.go`:

```go
package configstore

import (
	"maps"
	"sync"
)

// Config is a concurrency-safe string keyed configuration store. Reads take a
// shared read lock; the rare write takes the exclusive lock. It matches the
// read-heavy shape of server configuration: read on every request, written on a
// rare reload.
type Config struct {
	mu       sync.RWMutex
	settings map[string]string
}

// NewConfig returns an empty store.
func NewConfig() *Config {
	return &Config{settings: make(map[string]string)}
}

// Get returns the value for key and whether it was present. It takes a shared
// read lock, so concurrent Gets never block one another.
func (c *Config) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.settings[key]
	return v, ok
}

// Set writes value under key. It takes the exclusive write lock, briefly
// excluding all readers and writers.
func (c *Config) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings[key] = value
}

// Len reports the number of stored keys under a shared read lock.
func (c *Config) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.settings)
}

// Snapshot returns an independent copy of the current settings. The caller owns
// the returned map and may read or mutate it without holding the lock; a later
// Set does not affect it.
func (c *Config) Snapshot() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return maps.Clone(c.settings)
}
```

### The runnable demo

The demo shows the defensive-copy contract concretely: it snapshots the store,
then `Set`s a new key, and prints both the snapshot (unchanged) and the live store
(updated), proving the snapshot is independent.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configstore"
)

func main() {
	cfg := configstore.NewConfig()
	cfg.Set("host", "localhost")
	cfg.Set("port", "8080")

	if v, ok := cfg.Get("host"); ok {
		fmt.Printf("host=%s\n", v)
	}
	fmt.Printf("len=%d\n", cfg.Len())

	snap := cfg.Snapshot()
	cfg.Set("scheme", "https")

	fmt.Printf("snapshot has scheme: %t\n", func() bool { _, ok := snap["scheme"]; return ok }())
	fmt.Printf("live has scheme: %t\n", func() bool { _, ok := cfg.Get("scheme"); return ok }())
	fmt.Printf("snapshot len=%d live len=%d\n", len(snap), cfg.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=localhost
len=2
snapshot has scheme: false
live has scheme: true
snapshot len=2 live len=3
```

### Tests

`TestConfigConcurrentReadWrite` is the core race test: 64 readers each do 500
`Get`s while one writer does 200 `Set`s, all overwriting the same two keys, so the
key count stays at exactly 2 throughout. Run under `-race`, a clean result proves
the read/write locking is correct — no read observes a torn map and no write races
a read. `TestSnapshotIsIndependent` pins the defensive-copy contract: a snapshot
taken before a `Set` never gains the newly-set key. `ExampleConfig` pins a simple
round-trip with verified output.

Create `config_test.go`:

```go
package configstore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConfigConcurrentReadWrite(t *testing.T) {
	t.Parallel()

	c := NewConfig()
	c.Set("k1", "v1")
	c.Set("k2", "v2")

	const readers = 64
	const reads = 500

	var wg sync.WaitGroup
	var seen atomic.Int64

	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for j := range reads {
				if _, ok := c.Get(fmt.Sprintf("k%d", (j%2)+1)); ok {
					seen.Add(1)
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := range 200 {
			c.Set(fmt.Sprintf("k%d", (j%2)+1), fmt.Sprintf("v%d", j))
		}
	}()
	wg.Wait()

	if got, want := c.Len(), 2; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
}

func TestSnapshotIsIndependent(t *testing.T) {
	t.Parallel()

	c := NewConfig()
	c.Set("host", "localhost")

	snap := c.Snapshot()
	c.Set("port", "8080")

	if _, ok := snap["port"]; ok {
		t.Fatal("snapshot gained a key set after it was taken")
	}
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}

	// Mutating the snapshot must not touch the live store.
	snap["host"] = "mutated"
	if v, _ := c.Get("host"); v != "localhost" {
		t.Fatalf("live host = %q, want localhost (snapshot mutation leaked)", v)
	}
}

func ExampleConfig() {
	c := NewConfig()
	c.Set("k", "v")
	v, _ := c.Get("k")
	fmt.Println(v)
	// Output: v
}
```

## Review

The store is correct when every read path takes `RLock` and every mutation takes
`Lock`, with no path mutating protected state under a read lock. The `-race`
detector is the hard proof: `TestConfigConcurrentReadWrite` hammers the store with
64 concurrent readers and a writer, and a silent detector means the locking holds.
The most common mistakes this exercise guards against are returning the internal
map from `Snapshot` (which would let callers race `Set`) and forgetting `defer` on
the unlock (which leaks the lock on an early return). Confirm correctness by
running `go test -race` and watching `TestSnapshotIsIndependent` prove a snapshot
is a real, owned copy.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock, `RLock`/`Lock` semantics.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the standard shallow map copy used by `Snapshot`.
- [Go Memory Model](https://go.dev/ref/mem) — why an unguarded read of a concurrently-written map is a race.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-read-through-cache-double-check.md](02-read-through-cache-double-check.md)
