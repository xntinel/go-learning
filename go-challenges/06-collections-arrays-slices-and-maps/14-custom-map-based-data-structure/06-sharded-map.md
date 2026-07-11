# Exercise 6: Striped-lock sharded map for a hot concurrent counter

A counter service that tracks per-endpoint request counts is written by every
request-handling goroutine at once. Behind a single `RWMutex`, every write
serializes on that one lock and it becomes the bottleneck. The fix is to shard:
split the key space into N independent maps, each behind its own lock, so a write
to one shard never blocks a write to another. This module builds that sharded map
and proves it loses no updates under heavy concurrency.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
shardedmap/                independent module: example.com/shardedmap
  go.mod
  shardedmap.go            type ShardedMap[V]; New, Get, Set, Delete, Update, Len, Range
  cmd/
    demo/
      main.go              per-endpoint request counter with a snapshot
  shardedmap_test.go       no-lost-updates under concurrency, shard distribution
```

- Files: `shardedmap.go`, `cmd/demo/main.go`, `shardedmap_test.go`.
- Implement: `ShardedMap[V any]` with N shards, each a `map[string]V` behind its own `sync.RWMutex`, routing a key to its shard via `hash/maphash`; `Get`, `Set`, `Delete`, `Update` (read-modify-write), `Len`, and `Range` (snapshot).
- Test: many goroutines running `Update` on overlapping keys with the summed total equal to expected (no lost updates), and keys spreading across all shards.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shardedmap/cmd/demo
cd ~/go-exercises/shardedmap
go mod init example.com/shardedmap
```

### Stripe locking, and why the hash must be a real hash

The structure is an array of shards; each shard is a `map[string]V` plus its own
`sync.RWMutex`. A key is routed to exactly one shard by hashing it and taking the
hash modulo the shard count, so the shard a key lives in is stable and every
operation on that key touches the same lock. Because different keys tend to land
on different shards, concurrent writes proceed in parallel across shards; only
writes to the *same* shard contend. With N shards you cut lock contention by
roughly a factor of N compared to a single global lock.

The routing hash matters. A tempting shortcut — shard by `key[0]` or `len(key)` —
is a trap: real keys cluster (many endpoints start with `GET `, many IDs share a
length), so a weak transform produces a few hot shards and defeats the point. Use
a real hash. `hash/maphash` is the right one: it seeds from a per-process random
value (`maphash.MakeSeed()`), which spreads keys uniformly and, as a bonus,
resists hash-flooding — an attacker cannot precompute keys that all collide into
one shard the way they could against a fixed hash.

`Update` deserves attention because it is where the "cannot address a map element"
rule bites. You cannot write `&shard.m[key]` to increment a counter in place, and
`shard.m[key]++` is fine for a map but must happen under the lock. `Update` takes
a function, reads the whole current value under the shard's write lock, applies
the function, and writes the result back — a locked read-modify-write. Incrementing
is `Update(key, func(n int) int { return n + 1 })`. This is the race-free way to do
a counter; two goroutines incrementing the same key serialize on that shard's lock
and neither update is lost.

How many shards? Defaulting to a multiple of `runtime.GOMAXPROCS(0)` scales the
parallelism with the number of CPUs actually available to the process. Contrast
the whole design with the alternatives: a single `RWMutex`+map is simpler but
serializes writers; `sync.Map` helps only for write-once-read-many or disjoint-key
workloads and throws away type safety; sharding is the tool for high write
concurrency across many keys — exactly a hot counter.

Create `shardedmap.go`:

```go
package shardedmap

import (
	"hash/maphash"
	"runtime"
	"sync"
)

type shard[V any] struct {
	mu sync.RWMutex
	m  map[string]V
}

// ShardedMap is a concurrent string-keyed map partitioned into independently
// locked shards, so writes to different shards do not contend.
type ShardedMap[V any] struct {
	seed   maphash.Seed
	shards []*shard[V]
}

// New returns a ShardedMap sized to the available parallelism.
func New[V any]() *ShardedMap[V] {
	return NewWithShards[V](defaultShardCount())
}

// NewWithShards returns a ShardedMap with exactly n shards (clamped to >= 1).
func NewWithShards[V any](n int) *ShardedMap[V] {
	if n < 1 {
		n = 1
	}
	shards := make([]*shard[V], n)
	for i := range shards {
		shards[i] = &shard[V]{m: make(map[string]V)}
	}
	return &ShardedMap[V]{seed: maphash.MakeSeed(), shards: shards}
}

func defaultShardCount() int {
	n := runtime.GOMAXPROCS(0) * 4
	if n < 1 {
		n = 1
	}
	return n
}

func (m *ShardedMap[V]) shardFor(key string) *shard[V] {
	return m.shards[maphash.String(m.seed, key)%uint64(len(m.shards))]
}

// Get returns the value for key, if present.
func (m *ShardedMap[V]) Get(key string) (V, bool) {
	s := m.shardFor(key)
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}

// Set stores value under key.
func (m *ShardedMap[V]) Set(key string, value V) {
	s := m.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
}

// Delete removes key.
func (m *ShardedMap[V]) Delete(key string) {
	s := m.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// Update read-modify-writes the value for key under the shard lock. This is the
// race-free way to do i++ style updates: you cannot take the address of a map
// element to mutate it in place, so you read the whole value, apply f, and write
// it back. A missing key reads as the zero value.
func (m *ShardedMap[V]) Update(key string, f func(old V) V) {
	s := m.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = f(s.m[key])
}

// Len reports the total number of entries across all shards.
func (m *ShardedMap[V]) Len() int {
	n := 0
	for _, s := range m.shards {
		s.mu.RLock()
		n += len(s.m)
		s.mu.RUnlock()
	}
	return n
}

// Range returns a snapshot copy of every entry. It locks each shard in turn
// (never all at once), so it does not freeze the whole map.
func (m *ShardedMap[V]) Range() map[string]V {
	out := make(map[string]V, m.Len())
	for _, s := range m.shards {
		s.mu.RLock()
		for k, v := range s.m {
			out[k] = v
		}
		s.mu.RUnlock()
	}
	return out
}

// Shards reports the number of shards.
func (m *ShardedMap[V]) Shards() int { return len(m.shards) }

// shardIndex reports which shard a key routes to (used by tests).
func (m *ShardedMap[V]) shardIndex(key string) int {
	return int(maphash.String(m.seed, key) % uint64(len(m.shards)))
}
```

### The runnable demo

The demo counts requests per endpoint with `Update`, then takes a `Range`
snapshot and prints the counts in sorted order (map order is random, so it must be
sorted for stable output).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/shardedmap"
)

func main() {
	counts := shardedmap.NewWithShards[int](8)
	incr := func(n int) int { return n + 1 }

	requests := []string{
		"GET /health", "GET /health", "POST /orders",
		"GET /health", "POST /orders", "GET /users",
	}
	for _, ep := range requests {
		counts.Update(ep, incr)
	}

	snapshot := counts.Range()
	endpoints := make([]string, 0, len(snapshot))
	for ep := range snapshot {
		endpoints = append(endpoints, ep)
	}
	slices.Sort(endpoints)
	for _, ep := range endpoints {
		fmt.Printf("%s = %d\n", ep, snapshot[ep])
	}
	fmt.Printf("shards: %d\n", counts.Shards())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /health = 3
GET /users = 1
POST /orders = 2
shards: 8
```

### Tests

The concurrency test is the important one: many goroutines increment overlapping
keys, and the summed total must equal the exact number of `Update` calls — any
lost update (an increment that read a stale value) would make the total short.
Under `-race` this also proves no two goroutines touch a shard's map
unsynchronized. The distribution test confirms keys spread across all shards, so
the hashing is not degenerate.

Create `shardedmap_test.go`:

```go
package shardedmap

import (
	"fmt"
	"sync"
	"testing"
)

func TestSetGetDelete(t *testing.T) {
	t.Parallel()

	m := New[string]()
	m.Set("k", "v")
	if v, ok := m.Get("k"); !ok || v != "v" {
		t.Fatalf("Get(k) = %q,%v; want v,true", v, ok)
	}
	m.Delete("k")
	if _, ok := m.Get("k"); ok {
		t.Fatal("Get(k) present after Delete")
	}
}

func TestConcurrentIncrNoLostUpdates(t *testing.T) {
	t.Parallel()

	m := New[int]()
	const goroutines = 50
	const perGoroutine = 1000
	keys := []string{"a", "b", "c", "d"}
	incr := func(n int) int { return n + 1 }

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perGoroutine {
				m.Update(keys[(g+i)%len(keys)], incr)
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, v := range m.Range() {
		total += v
	}
	if want := goroutines * perGoroutine; total != want {
		t.Fatalf("total increments = %d, want %d (lost updates)", total, want)
	}
}

func TestKeysSpreadAcrossShards(t *testing.T) {
	t.Parallel()

	m := NewWithShards[int](16)
	used := make(map[int]struct{})
	for i := range 1000 {
		key := fmt.Sprintf("key-%d", i)
		m.Set(key, i)
		used[m.shardIndex(key)] = struct{}{}
	}
	if len(used) != m.Shards() {
		t.Fatalf("keys populated %d of %d shards; want all shards used", len(used), m.Shards())
	}
}

func Example() {
	m := New[int]()
	incr := func(n int) int { return n + 1 }
	m.Update("requests", incr)
	m.Update("requests", incr)
	v, _ := m.Get("requests")
	fmt.Println(v)
	// Output: 2
}
```

## Review

The sharded map is correct when concurrent `Update`s lose nothing — the summed
total equals the number of calls — and every key routes to a stable shard via a
real hash. The mistakes to avoid are sharding by a weak key transform (`key[0]` or
`len(key)`), which produces hot shards and uneven contention, and doing an
unlocked read-modify-write (reading the value, then writing back outside the lock),
which races and loses updates — `Update` holds the shard's write lock across the
whole read-apply-write. Remember why `Update` takes a function at all: you cannot
address a map element to increment it in place, so the whole value is read and
rewritten. Run `go test -count=1 -race ./...`.

## Resources

- [`hash/maphash` package](https://pkg.go.dev/hash/maphash) — `MakeSeed` and `String`, the per-process-seeded hash used for routing.
- [`sync` package](https://pkg.go.dev/sync) — `RWMutex`, one per shard.
- [`sync.Map` documentation](https://pkg.go.dev/sync#Map) — the two workloads it is tuned for, and why sharding beats it under general write concurrency.
- [`runtime.GOMAXPROCS`](https://pkg.go.dev/runtime#GOMAXPROCS) — sizing the shard count to available parallelism.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-ttl-cache.md](05-ttl-cache.md) | Next: [07-secondary-index-multimap.md](07-secondary-index-multimap.md)
