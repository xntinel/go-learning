# Exercise 5: Copy-on-write config via atomic.Pointer vs RWMutex

For read-mostly *immutable* state, there is a level above `RWMutex`:
copy-on-write with `atomic.Pointer[T]`. Readers do a lock-free `Load` of the
current snapshot and never touch a lock, so they can never block behind a writer
at all; a writer builds a brand-new immutable snapshot and publishes it atomically.
This exercise builds the same read-mostly config two ways — an `RWMutex`-guarded
version and an `atomic.Pointer` copy-on-write version — proves both are consistent
under concurrent access, and benchmarks the read paths so you can see when
graduating off the read lock pays off.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
cowconfig/                   independent module: example.com/cowconfig
  go.mod                     module example.com/cowconfig
  config.go                  type MutexConfig (RWMutex); type COWConfig (atomic.Pointer[snapshot])
  cmd/
    demo/
      main.go                runnable demo: read/update both, show consistency
  config_test.go             consistency + immutability tests, Example, read-throughput benchmarks
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `MutexConfig` with `Get`/`Update` under `RWMutex`; `COWConfig` with lock-free `Get` via `atomic.Pointer[snapshot].Load()` and `Update` building a new immutable snapshot and publishing it with a `CompareAndSwap` loop.
Test: readers always see a fully-consistent snapshot (never half-updated); a loaded snapshot is unaffected by a later `Update`; a benchmark comparing read throughput.
Verify: `go test -count=1 -race ./...`

### Why copy-on-write beats the read lock for read-mostly snapshots

An `RWMutex` lets readers proceed concurrently, but they still pay the reader-count
atomics on every `RLock`/`RUnlock`, and a reader that arrives while a writer holds
`Lock` blocks until the writer is done. For state that is read constantly and
written rarely — and that can be treated as an immutable value swapped wholesale —
copy-on-write removes both costs. The reader does a single atomic pointer `Load`
and dereferences it: no lock, no reader count, no possibility of blocking behind a
writer. The writer never mutates the live snapshot; it builds a *new* one and
swaps the pointer with `CompareAndSwap`. Readers holding the old pointer keep
reading a valid, immutable snapshot; new readers pick up the new one. The cost is
an allocation per write and the discipline that a published snapshot is never
mutated in place.

The consistency guarantee is the point. Suppose the config has two fields that must
change together (a host and its matching port). Under `RWMutex`, a reader that took
`RLock` between the two writes could see a half-updated pair — unless every write
is a single `Lock` that swaps both. Under copy-on-write, a reader can *never* see a
half-update, because the reader only ever observes a fully-built snapshot: the
writer assembles the complete new snapshot privately, then makes it visible with
one atomic store. There is no window in which a partial update is observable.

`Update` uses a `CompareAndSwap` loop rather than a bare `Store` because the new
snapshot derives from the current one: load the current pointer, build a new
snapshot from it, and `CompareAndSwap(old, new)`. If another writer published in
the meantime, the CAS fails, and the loop retries against the newer snapshot — so
concurrent updates compose correctly instead of clobbering each other. The
`MutexConfig` version is the honest baseline: correct, simple, and the right choice
when the state is not naturally immutable or when writes are frequent.

Create `config.go`:

```go
package cowconfig

import (
	"maps"
	"sync"
	"sync/atomic"
)

// MutexConfig guards a settings map with an RWMutex. Reads take RLock; each
// Update replaces the whole map under Lock so a reader never sees a half-update.
type MutexConfig struct {
	mu       sync.RWMutex
	settings map[string]string
}

// NewMutexConfig returns a MutexConfig seeded with the given settings.
func NewMutexConfig(seed map[string]string) *MutexConfig {
	return &MutexConfig{settings: maps.Clone(seed)}
}

// Get returns the value for key under a shared read lock.
func (c *MutexConfig) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.settings[key]
	return v, ok
}

// Update applies mutate to a private copy and installs it under the write lock,
// so readers only ever observe a complete map.
func (c *MutexConfig) Update(mutate func(m map[string]string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := maps.Clone(c.settings)
	mutate(next)
	c.settings = next
}

// snapshot is an immutable config value published by COWConfig. Once stored, its
// map is never mutated; a new snapshot is built for every update.
type snapshot struct {
	settings map[string]string
}

// COWConfig holds an immutable snapshot behind an atomic pointer. Reads are
// lock-free and never block behind a writer.
type COWConfig struct {
	ptr atomic.Pointer[snapshot]
}

// NewCOWConfig returns a COWConfig seeded with the given settings.
func NewCOWConfig(seed map[string]string) *COWConfig {
	c := &COWConfig{}
	c.ptr.Store(&snapshot{settings: maps.Clone(seed)})
	return c
}

// Get reads the value for key with a single lock-free atomic Load.
func (c *COWConfig) Get(key string) (string, bool) {
	s := c.ptr.Load()
	v, ok := s.settings[key]
	return v, ok
}

// Update builds a new immutable snapshot from the current one and publishes it
// with a CompareAndSwap loop, so concurrent updates compose instead of clobber.
func (c *COWConfig) Update(mutate func(m map[string]string)) {
	for {
		old := c.ptr.Load()
		next := maps.Clone(old.settings)
		mutate(next)
		if c.ptr.CompareAndSwap(old, &snapshot{settings: next}) {
			return
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cowconfig"
)

func main() {
	seed := map[string]string{"host": "localhost", "port": "8080"}

	m := cowconfig.NewMutexConfig(seed)
	m.Update(func(s map[string]string) { s["port"] = "9090" })
	mp, _ := m.Get("port")
	fmt.Printf("mutex config port=%s\n", mp)

	c := cowconfig.NewCOWConfig(seed)
	c.Update(func(s map[string]string) {
		s["host"] = "prod.internal"
		s["port"] = "443"
	})
	ch, _ := c.Get("host")
	cp, _ := c.Get("port")
	fmt.Printf("cow config host=%s port=%s\n", ch, cp)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mutex config port=9090
cow config host=prod.internal port=443
```

### Tests

`TestCOWConsistentPair` publishes snapshots whose two fields must match (`a == b`
in every version) while readers repeatedly `Load` the current snapshot and check
both fields in it; no loaded snapshot may ever contain a mismatched pair, proving
the atomic swap makes half-updates unobservable. (Note the readers deliberately
take *one* `Load` per check: two separate `Get` calls could straddle a publish
and mix two versions — cross-call consistency is not what copy-on-write
promises; per-snapshot completeness is.)
`TestCOWSnapshotImmutable` captures a value from `Get`, runs an `Update`, and
asserts the earlier read is unaffected — a published snapshot is immutable.
`BenchmarkMutexRead` and `BenchmarkCOWRead` compare read throughput under a
reader-heavy `RunParallel` load, making the lock-free read path's advantage
measurable.

Create `config_test.go`:

```go
package cowconfig

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestCOWConsistentPair(t *testing.T) {
	t.Parallel()

	c := NewCOWConfig(map[string]string{"a": "0", "b": "0"})

	var wg sync.WaitGroup

	// Writer: publish versions where a and b always match.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			v := strconv.Itoa(i)
			c.Update(func(s map[string]string) {
				s["a"] = v
				s["b"] = v
			})
		}
	}()

	// Readers: both fields must come from ONE snapshot Load — two separate
	// Get calls may straddle a publish and legitimately mix versions. The
	// guarantee under test is that any single published snapshot is complete.
	const readers = 8
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for range 2000 {
				s := c.ptr.Load()
				a, b := s.settings["a"], s.settings["b"]
				if a != b {
					t.Errorf("inconsistent snapshot: a=%q b=%q", a, b)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestCOWSnapshotImmutable(t *testing.T) {
	t.Parallel()

	c := NewCOWConfig(map[string]string{"v": "old"})
	before, _ := c.Get("v")

	c.Update(func(s map[string]string) { s["v"] = "new" })

	if before != "old" {
		t.Fatalf("captured value changed to %q after Update", before)
	}
	if after, _ := c.Get("v"); after != "new" {
		t.Fatalf("Get after Update = %q, want new", after)
	}
}

func ExampleCOWConfig() {
	c := NewCOWConfig(map[string]string{"env": "dev"})
	c.Update(func(s map[string]string) { s["env"] = "prod" })
	v, _ := c.Get("env")
	fmt.Println(v)
	// Output: prod
}

func BenchmarkMutexRead(b *testing.B) {
	c := NewMutexConfig(map[string]string{"host": "localhost"})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get("host")
		}
	})
}

func BenchmarkCOWRead(b *testing.B) {
	c := NewCOWConfig(map[string]string{"host": "localhost"})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get("host")
		}
	})
}
```

## Review

Both configs are correct; the exercise is about *when to graduate*. The
`RWMutex` version is right when the state is not naturally immutable or when writes
are frequent enough that per-write allocation hurts. The copy-on-write version wins
when reads dominate an immutable snapshot: the read path is a single atomic `Load`
that never blocks behind a writer, which `BenchmarkCOWRead` makes visible against
`BenchmarkMutexRead` under a reader-heavy load. The mistakes to avoid are mutating
a published snapshot in place (it must be treated as immutable — build a new one),
and using a bare `Store` where the new value derives from the old (that loses
concurrent updates; use the `CompareAndSwap` loop). Run `go test -race`;
`TestCOWConsistentPair` proves readers never see a half-updated snapshot.

## Resources

- [`sync/atomic` `Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — `Load`, `Store`, `CompareAndSwap` for the snapshot pointer.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the baseline read/write lock.
- [Go Memory Model](https://go.dev/ref/mem) — why an atomic publish makes a fully-built snapshot safely visible.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-ttl-cache-with-sweeper.md](06-ttl-cache-with-sweeper.md)
