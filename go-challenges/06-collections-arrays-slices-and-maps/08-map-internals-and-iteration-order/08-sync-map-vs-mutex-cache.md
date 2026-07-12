# Exercise 8: Read-Heavy Metadata Cache — sync.Map vs RWMutex+map, Chosen by Benchmark

Feature flags and tenant config are read on nearly every request and written rarely.
That is the one workload `sync.Map` is actually built for — and the wrong default
everywhere else. This module implements the same read-mostly cache twice, once with
`sync.Map` and once with `RWMutex`+map, behind one interface, so a benchmark decides
between them instead of folklore.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
metacache/                 independent module: example.com/metacache
  go.mod                   go 1.26
  metacache.go             interface Cache; SyncCache (sync.Map), MutexCache (RWMutex+map)
  cmd/
    demo/
      main.go              lazy-inits a flag once, reads it, invalidates it
  metacache_test.go        LoadOrStore once, Range, CompareAndDelete, parallel benchmarks
```

- Files: `metacache.go`, `cmd/demo/main.go`, `metacache_test.go`.
- Implement: a `Cache` interface (`GetOrCompute`, `Load`, `Invalidate`, `Range`); a
  `SyncCache` over `sync.Map` and a `MutexCache` over `RWMutex`+map.
- Test: `GetOrCompute` returns `loaded=false` then `true` and runs the initializer
  exactly once under concurrency (atomic count); `Range` visits all entries;
  `Invalidate` only removes on value match; read-heavy and write-heavy benchmarks.
- Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchmem`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/08-sync-map-vs-mutex-cache/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/08-sync-map-vs-mutex-cache
```

### The decision rule, not the cargo cult

`sync.Map` is not "the concurrent map". It is a specialized structure tuned for two
access patterns: (a) a key is written once and then read many times (read-mostly,
stable keys), and (b) different goroutines touch disjoint key sets. In those cases it
avoids lock contention on the read path. Outside them it is *slower* than a plain
`RWMutex`+map, and it always costs `any` boxing plus a type assertion on every access
because its API is untyped. The senior default is `RWMutex`+map; you switch to
`sync.Map` only when a benchmark of your real access pattern shows it winning. This
module builds both behind one interface precisely so you can run that benchmark.

### Lazy init that runs exactly once

`LoadOrStore(key, value)` cannot lazily *compute* a value — you must hand it a value
already, so a naive "compute then `LoadOrStore`" runs the (possibly expensive)
initializer on every racing goroutine and throws all but one result away. The idiomatic
fix is to store a small *entry pointer* that carries a `sync.Once`: `LoadOrStore` a
fresh `&entry{}` (cheap, allocation only), then call `e.once.Do(compute)`. Every
goroutine that raced to that key shares the one `entry` the winner stored, and
`once.Do` guarantees `compute` runs exactly once regardless of how many raced. The
`loaded` bool comes straight from `LoadOrStore`: `false` for the goroutine that
installed the entry, `true` for the ones that found it. The test asserts the initializer
count is exactly 1 under a stampede.

`Invalidate` uses `CompareAndDelete`, which removes the key only if the stored value is
still the exact one you observed. That closes an ABA race: if another goroutine already
replaced the entry with a newer one, your delete is refused rather than clobbering the
fresh value. The `MutexCache` mirror does the same check under the write lock.

Create `metacache.go`:

```go
package metacache

import (
	"sync"
)

// Cache is a read-mostly string cache with lazy, run-once initialization.
type Cache interface {
	// GetOrCompute returns the value for key, computing and storing it exactly
	// once if absent. loaded is false only for the caller that installed it.
	GetOrCompute(key string, compute func() string) (value string, loaded bool)
	// Load returns the stored value with presence semantics.
	Load(key string) (string, bool)
	// Invalidate removes key only if its current value equals old.
	Invalidate(key, old string) bool
	// Range calls f for every entry; returning false stops iteration.
	Range(f func(key, value string) bool)
}

// entry carries a value behind a sync.Once so compute runs at most once per key.
type entry struct {
	once  sync.Once
	value string
}

// SyncCache implements Cache over sync.Map — suited to read-mostly, disjoint-key
// workloads.
type SyncCache struct {
	m sync.Map // string -> *entry
}

func (c *SyncCache) GetOrCompute(key string, compute func() string) (string, bool) {
	actual, loaded := c.m.LoadOrStore(key, &entry{})
	e := actual.(*entry)
	e.once.Do(func() { e.value = compute() })
	return e.value, loaded
}

func (c *SyncCache) Load(key string) (string, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return "", false
	}
	return v.(*entry).value, true
}

func (c *SyncCache) Invalidate(key, old string) bool {
	v, ok := c.m.Load(key)
	if !ok {
		return false
	}
	e := v.(*entry)
	if e.value != old {
		return false
	}
	// CompareAndDelete removes only if the stored pointer is still e, so a
	// concurrently-replaced entry is not clobbered.
	return c.m.CompareAndDelete(key, e)
}

func (c *SyncCache) Range(f func(key, value string) bool) {
	c.m.Range(func(k, v any) bool {
		return f(k.(string), v.(*entry).value)
	})
}

// MutexCache implements Cache over an RWMutex and a plain map — the default choice
// unless a benchmark proves SyncCache wins for the real workload.
type MutexCache struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewMutexCache returns an initialized MutexCache.
func NewMutexCache() *MutexCache {
	return &MutexCache{m: make(map[string]string)}
}

func (c *MutexCache) GetOrCompute(key string, compute func() string) (string, bool) {
	c.mu.RLock()
	if v, ok := c.m[key]; ok {
		c.mu.RUnlock()
		return v, true
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; ok { // re-check: another writer may have won the race
		return v, true
	}
	v := compute()
	c.m[key] = v
	return v, false
}

func (c *MutexCache) Load(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *MutexCache) Invalidate(key, old string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; !ok || v != old {
		return false
	}
	delete(c.m, key)
	return true
}

func (c *MutexCache) Range(f func(key, value string) bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for k, v := range c.m {
		if !f(k, v) {
			return
		}
	}
}
```

Note the `MutexCache.GetOrCompute` double-check: it takes the read lock for the common
hit, and only on a miss upgrades to the write lock and re-checks, so a second goroutine
that won the race does not compute twice. Its `compute` runs under the write lock, which
is acceptable for a cheap flag lookup; if `compute` were expensive you would move to the
`sync.Once`-per-key pattern the `SyncCache` uses.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metacache"
)

func main() {
	var c metacache.SyncCache

	v, loaded := c.GetOrCompute("flag:new_ui", func() string { return "on" })
	fmt.Printf("first:  value=%s loaded=%v\n", v, loaded)

	v, loaded = c.GetOrCompute("flag:new_ui", func() string { return "recomputed" })
	fmt.Printf("second: value=%s loaded=%v\n", v, loaded)

	removed := c.Invalidate("flag:new_ui", "on")
	fmt.Printf("invalidate(on): removed=%v\n", removed)

	_, ok := c.Load("flag:new_ui")
	fmt.Printf("after invalidate: present=%v\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first:  value=on loaded=false
second: value=on loaded=true
invalidate(on): removed=true
after invalidate: present=false
```

(The second `GetOrCompute` returns `on`, not `recomputed`: the value was already
installed, so the initializer does not run again.)

### Tests and benchmarks

Both implementations run through the same table of behavioral tests via the `Cache`
interface, so any divergence is caught. `TestInitRunsOnceUnderStampede` launches many
goroutines at one key and asserts an atomic counter reached exactly 1. The benchmarks
compare `SyncCache` and `MutexCache` under a read-heavy and a write-heavy parallel load.

Create `metacache_test.go`:

```go
package metacache

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func implementations() map[string]func() Cache {
	return map[string]func() Cache{
		"SyncCache":  func() Cache { return &SyncCache{} },
		"MutexCache": func() Cache { return NewMutexCache() },
	}
}

func TestGetOrComputeLoadedFlag(t *testing.T) {
	t.Parallel()

	for name, newCache := range implementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newCache()
			if v, loaded := c.GetOrCompute("k", func() string { return "v" }); loaded || v != "v" {
				t.Fatalf("first GetOrCompute = %q,%v; want v,false", v, loaded)
			}
			if v, loaded := c.GetOrCompute("k", func() string { return "other" }); !loaded || v != "v" {
				t.Fatalf("second GetOrCompute = %q,%v; want v,true", v, loaded)
			}
		})
	}
}

func TestInitRunsOnceUnderStampede(t *testing.T) {
	t.Parallel()

	for name, newCache := range implementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newCache()
			var calls atomic.Int64
			var wg sync.WaitGroup
			for range 100 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.GetOrCompute("hot", func() string {
						calls.Add(1)
						return "computed"
					})
				}()
			}
			wg.Wait()
			if got := calls.Load(); got != 1 {
				t.Fatalf("initializer ran %d times, want exactly 1", got)
			}
		})
	}
}

func TestRangeVisitsAll(t *testing.T) {
	t.Parallel()

	for name, newCache := range implementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newCache()
			for i := range 5 {
				k := strconv.Itoa(i)
				c.GetOrCompute(k, func() string { return "v" + k })
			}
			seen := map[string]string{}
			c.Range(func(k, v string) bool {
				seen[k] = v
				return true
			})
			if len(seen) != 5 {
				t.Fatalf("Range visited %d entries, want 5", len(seen))
			}
			for i := range 5 {
				k := strconv.Itoa(i)
				if seen[k] != "v"+k {
					t.Fatalf("Range missing or wrong for %q: %q", k, seen[k])
				}
			}
		})
	}
}

func TestInvalidateOnlyOnMatch(t *testing.T) {
	t.Parallel()

	for name, newCache := range implementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newCache()
			c.GetOrCompute("k", func() string { return "v1" })

			if c.Invalidate("k", "wrong") {
				t.Fatal("Invalidate removed on a value mismatch")
			}
			if _, ok := c.Load("k"); !ok {
				t.Fatal("entry gone after a refused Invalidate")
			}
			if !c.Invalidate("k", "v1") {
				t.Fatal("Invalidate refused on a value match")
			}
			if _, ok := c.Load("k"); ok {
				t.Fatal("entry still present after a matching Invalidate")
			}
		})
	}
}

func benchRead(b *testing.B, c Cache) {
	for i := range 128 {
		k := strconv.Itoa(i)
		c.GetOrCompute(k, func() string { return "v" })
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Load(strconv.Itoa(i & 127))
			i++
		}
	})
}

func benchWrite(b *testing.B, c Cache) {
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := strconv.Itoa(i & 127)
			c.Invalidate(k, "v")
			c.GetOrCompute(k, func() string { return "v" })
			i++
		}
	})
}

func BenchmarkReadHeavy_Sync(b *testing.B)   { benchRead(b, &SyncCache{}) }
func BenchmarkReadHeavy_Mutex(b *testing.B)  { benchRead(b, NewMutexCache()) }
func BenchmarkWriteHeavy_Sync(b *testing.B)  { benchWrite(b, &SyncCache{}) }
func BenchmarkWriteHeavy_Mutex(b *testing.B) { benchWrite(b, NewMutexCache()) }

func ExampleSyncCache() {
	var c SyncCache
	v, loaded := c.GetOrCompute("k", func() string { return "v" })
	fmt.Println(v, loaded)
	v, loaded = c.GetOrCompute("k", func() string { return "other" })
	fmt.Println(v, loaded)
	// Output:
	// v false
	// v true
}
```

Run the benchmarks:

```bash
go test -bench=. -benchmem
```

You should typically see `SyncCache` ahead on `ReadHeavy` (disjoint reads, no lock
contention) and `MutexCache` competitive or ahead on `WriteHeavy` (where `sync.Map`'s
bookkeeping and boxing cost more). Numbers vary by machine and core count — the point is
that you *measure* the choice on your workload, which is why the decision lives in a
benchmark and not in this paragraph.

## Review

Both caches are correct when they satisfy the same interface tests, so the choice
between them is purely about cost, not behavior. The two mechanisms worth internalizing:
`LoadOrStore` of a `sync.Once`-bearing entry is how you get *lazy* init that still runs
exactly once under a stampede (plain `LoadOrStore` cannot, because it needs the value up
front), and `CompareAndDelete` is how invalidation avoids clobbering a
concurrently-refreshed entry. The senior takeaway is the default: reach for
`MutexCache`, and only adopt `SyncCache` when a benchmark of the real read/write ratio
and key distribution shows it winning. Run `go test -race` for correctness and
`go test -bench=. -benchmem` to pick.

## Resources

- [`sync.Map`](https://pkg.go.dev/sync#Map) — `Load`, `Store`, `LoadOrStore`, `Range`, `CompareAndDelete` semantics and the intended use cases.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — run-exactly-once initialization.
- [`testing.B.RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — parallel benchmark harness.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-safe-prune-during-range.md](09-safe-prune-during-range.md)
