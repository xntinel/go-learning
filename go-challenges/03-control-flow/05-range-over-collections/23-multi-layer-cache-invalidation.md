# Exercise 23: Propagate Cache Invalidation Across Layers with Concurrent Mutation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A multi-level cache — an in-process L1, a shared L2 like Redis, an L3 disk
cache — must invalidate a stale key everywhere it might be cached, not just
in the layer that changed, or a reader hitting a lower layer sees data an
upstream write already overwrote. This module ranges each layer in order to
propagate an invalidation downward, marks entries stale rather than deleting
them immediately, and gives each layer its own sweep that ranges under lock
to clean up the dangling stale entries — all while concurrent readers and
writers keep hitting the same layers. The module is fully self-contained:
its own `go mod init`, no external dependencies.

## What you'll build

```text
cache/                      independent module: example.com/multi-layer-cache-invalidation
  go.mod                    go 1.24
  cache.go                  type MultiCache, Layer; Invalidate(key) int; Layer.Sweep() int
  cmd/
    demo/
      main.go               runnable demo: set in all layers, invalidate, sweep
  cache_test.go             table test: invalidation coverage + edge cases; concurrent Get/Set/Invalidate/Sweep under -race
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: `MultiCache.Invalidate(key string) int` ranging `Layers` in
  order, and `Layer.Sweep() int` ranging one layer's entries under its own
  lock.
- Test: one table covering a key absent everywhere, present in every layer,
  and present in only one layer; one concurrency test running concurrent
  `Get`/`Set`/`Invalidate`/`Sweep` under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/23-multi-layer-cache-invalidation/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/23-multi-layer-cache-invalidation
go mod edit -go=1.24
```

### Mark-then-sweep, not delete-then-forget, and why each layer owns its own lock

`Invalidate` does not delete the key from each layer's map directly. It sets
a `stale` flag on the entry, under that layer's own `sync.RWMutex`, and moves
on to the next layer. Marking instead of deleting matters for a subtle
reason: `Get` checks the flag and reports "not found" for a stale entry
immediately, so correctness (no reader ever observes stale data) is achieved
the instant `Invalidate` touches a layer — cleanup can happen later, on its
own schedule, without any reader ever being exposed to a half-cleaned state.
`Sweep` is that later cleanup: it ranges the layer's `entries` map under
lock and deletes every entry whose `stale` flag is set — the "dangling
reference" the exercise brief refers to, memory `Invalidate` marked as dead
weight but did not reclaim.

Each `Layer` has its own mutex, not one mutex for the whole `MultiCache`.
That is a deliberate concurrency design, not just convenience: layers are
independent stores in practice (L1 is in-process memory, L2 might be a
Redis client, L3 a disk cache), so serializing access to L3 behind a lock
that also guards L1 would make an L1-only read wait on unrelated L3
contention. `Invalidate`'s loop takes and releases each layer's lock in
turn — it never holds two layers' locks at once — so a concurrent `Get` on
L2 is completely unaffected by `Invalidate` currently working on L1.

Create `cache.go`:

```go
package cache

import (
	"fmt"
	"sync"
)

// entry is one cached value plus a staleness flag. A stale entry is treated
// as absent by Get but still occupies memory until Sweep removes it.
type entry struct {
	value string
	stale bool
}

// Layer is one level of a multi-level cache (e.g. L1, L2, L3). All methods
// are safe for concurrent use.
type Layer struct {
	Name    string
	mu      sync.RWMutex
	entries map[string]*entry
}

func newLayer(name string) *Layer {
	return &Layer{Name: name, entries: make(map[string]*entry)}
}

// Set stores value for key in this layer only.
func (l *Layer) Set(key, value string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[key] = &entry{value: value}
}

// Get returns the value for key if present and not stale.
func (l *Layer) Get(key string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.entries[key]
	if !ok || e.stale {
		return "", false
	}
	return e.value, true
}

// Sweep ranges this layer's entries under lock, removing every entry marked
// stale — a dangling reference left behind once Invalidate has run — and
// returns how many were removed. Deleting the currently-ranged key is safe;
// a concurrent Get or Set on this layer blocks on the same mutex until the
// sweep finishes.
func (l *Layer) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for k, e := range l.entries {
		if e.stale {
			delete(l.entries, k)
			removed++
		}
	}
	return removed
}

// Len reports how many entries this layer currently holds, stale or not.
func (l *Layer) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

// MultiCache is an ordered stack of cache layers, L1 first.
type MultiCache struct {
	Layers []*Layer
}

// New builds a MultiCache with n layers named L1..Ln.
func New(n int) *MultiCache {
	layers := make([]*Layer, n)
	for i := range layers {
		layers[i] = newLayer(fmt.Sprintf("L%d", i+1))
	}
	return &MultiCache{Layers: layers}
}

// Invalidate propagates an invalidation downward through every layer,
// ranging Layers in order. Each layer that holds key is marked stale under
// its own lock; a concurrent Get on that layer will therefore see the entry
// as absent even mid-propagation, and a concurrent Sweep will clean it up.
// It returns how many layers were affected.
func (c *MultiCache) Invalidate(key string) int {
	affected := 0
	for _, layer := range c.Layers {
		layer.mu.Lock()
		if e, ok := layer.entries[key]; ok {
			e.stale = true
			affected++
		}
		layer.mu.Unlock()
	}
	return affected
}
```

### The runnable demo

The demo populates all three layers with the same key, invalidates it, shows
every layer now reports it absent, then sweeps L1 to reclaim the stale entry.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/multi-layer-cache-invalidation"
)

func main() {
	mc := cache.New(3)
	for _, layer := range mc.Layers {
		layer.Set("user:42", "cached-profile-v1")
	}

	if v, ok := mc.Layers[0].Get("user:42"); ok {
		fmt.Printf("before invalidate, L1 get: %s\n", v)
	}

	affected := mc.Invalidate("user:42")
	fmt.Printf("invalidated in %d layers\n", affected)

	for _, layer := range mc.Layers {
		_, ok := layer.Get("user:42")
		fmt.Printf("%s get after invalidate: found=%v\n", layer.Name, ok)
	}

	removed := mc.Layers[0].Sweep()
	fmt.Printf("L1 swept %d stale entries, len now %d\n", removed, mc.Layers[0].Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
before invalidate, L1 get: cached-profile-v1
invalidated in 3 layers
L1 get after invalidate: found=false
L2 get after invalidate: found=false
L3 get after invalidate: found=false
L1 swept 1 stale entries, len now 0
```

### Tests

The table covers a key present in no layers, present in every layer, and
present in exactly one layer — each asserting the invalidation count and
that `Sweep` reclaims what `Invalidate` marked. A separate test runs `Get`,
`Set`, `Invalidate`, and `Sweep` concurrently from many goroutines and must
pass cleanly under `-race`.

Create `cache_test.go`:

```go
package cache

import (
	"sync"
	"testing"
)

func TestInvalidateAndSweep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setup        func(mc *MultiCache)
		key          string
		wantAffected int
		wantSweptL1  int
	}{
		{
			name:         "key not present in any layer",
			setup:        func(mc *MultiCache) {},
			key:          "missing",
			wantAffected: 0,
			wantSweptL1:  0,
		},
		{
			name: "key present in all layers gets invalidated everywhere",
			setup: func(mc *MultiCache) {
				for _, l := range mc.Layers {
					l.Set("user:42", "v1")
				}
			},
			key:          "user:42",
			wantAffected: 3,
			wantSweptL1:  1,
		},
		{
			name: "key present in only one layer",
			setup: func(mc *MultiCache) {
				mc.Layers[1].Set("user:7", "v1")
			},
			key:          "user:7",
			wantAffected: 1,
			wantSweptL1:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := New(3)
			tc.setup(mc)

			affected := mc.Invalidate(tc.key)
			if affected != tc.wantAffected {
				t.Errorf("Invalidate() affected = %d, want %d", affected, tc.wantAffected)
			}

			for _, l := range mc.Layers {
				if _, ok := l.Get(tc.key); ok {
					t.Errorf("layer %s: Get(%q) still found after invalidate", l.Name, tc.key)
				}
			}

			swept := mc.Layers[0].Sweep()
			if swept != tc.wantSweptL1 {
				t.Errorf("L1 Sweep() = %d, want %d", swept, tc.wantSweptL1)
			}
		})
	}
}

func TestConcurrentReadWriteInvalidate(t *testing.T) {
	t.Parallel()

	mc := New(3)
	for _, l := range mc.Layers {
		l.Set("hot-key", "v0")
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mc.Layers[0].Get("hot-key")
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mc.Invalidate("hot-key")
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mc.Layers[0].Sweep()
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mc.Layers[0].Set("other-key", "v")
		}(i)
	}
	wg.Wait()

	// hot-key must have been invalidated by the concurrent Invalidate calls.
	if _, ok := mc.Layers[0].Get("hot-key"); ok {
		t.Fatal("hot-key still readable after concurrent Invalidate calls")
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The cache is correct when, after `Invalidate(key)` returns, no layer's `Get`
reports that key — even under concurrent load — and `Sweep` only ever
removes entries actually marked stale, never a live one. The bug this design
specifically avoids is a single global lock across all layers: that would
make an L1-only read block on L3 activity it has nothing to do with, which
defeats the point of having independent layers in the first place. Each
`Layer` owning its own `sync.RWMutex`, and `Invalidate` acquiring them one at
a time rather than all at once, is what keeps the layers' contention
independent while still guaranteeing every layer eventually sees the
invalidation.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [Go Specification: For statements (range over map, concurrency)](https://go.dev/ref/spec#For_range)
- [Caching at scale: invalidation strategies](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/) — production rationale for stale-then-sweep invalidation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-connection-pool-lru-eviction.md](22-connection-pool-lru-eviction.md) | Next: [24-write-ahead-log-replayer.md](24-write-ahead-log-replayer.md)
