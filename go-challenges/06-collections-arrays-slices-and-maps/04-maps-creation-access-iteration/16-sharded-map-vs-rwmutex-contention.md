# Exercise 16: Sharded Map vs RWMutex: Measuring Lock Contention

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A single `sync.RWMutex` guarding one map is the correct default for a shared
cache: it is simple, and `RLock` lets any number of readers proceed together.
The moment writers become frequent and concurrent, though, every writer --
no matter which key it touches -- serializes behind the same lock, and every
writer blocks every reader too. A per-connection rate-limiter counter, a
per-tenant request tally, or a hot session store under real traffic can turn
that single mutex into the bottleneck of the whole service, visible as CPU
time spent spinning on lock acquisition rather than doing work.

The fix production systems reach for is sharding: split the one map into N
independent maps, each with its own lock, and route each key to a shard by
hashing it. Two keys that land in different shards no longer contend at all.
This is the same idea behind `sync.Map`'s internal partitioning for its
read-mostly fast path and Java's `ConcurrentHashMap` segment locking -- both
trade a single global lock for many small ones so unrelated writers stop
blocking each other. This module builds both versions as a package you can
drop straight into a service, and proves they are race-free and behave
identically under concurrent load.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
shardmap/               module example.com/shardmap
  go.mod                go 1.24
  shardmap.go           RWMutexMap; ShardedMap (16 shards, FNV-1a routing); Get/Inc/Len on both
  shardmap_test.go      concurrent increment correctness (both types), shard distribution,
                         benchmarks, Example
```

- Files: `shardmap.go`, `shardmap_test.go`.
- Implement: `RWMutexMap` (one `map[string]int64` behind one `sync.RWMutex`) and `ShardedMap` (an array of 16 `shard` structs, each its own `map[string]int64` behind its own `sync.RWMutex`), both exposing `Get(key) (int64, bool)`, `Inc(key string, delta int64) int64`, and `Len() int`; `ShardedMap` routes a key to a shard via `shardFor(key)`, hashing the key with `hash/fnv`'s FNV-1a and taking it modulo the shard count.
- Test: many goroutines incrementing a small, shared set of keys concurrently on both map types under `-race`, asserting the final totals are exact; a distribution check that keys spread across more than one shard; parallel benchmarks of both types under contended writes; `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shardmap
cd ~/go-exercises/shardmap
go mod init example.com/shardmap
go mod edit -go=1.24
```

### Why one lock becomes a bottleneck, and what sharding actually buys you

`RWMutexMap`'s `Inc` takes the write lock for every increment, on every key.
Under concurrent load this means N goroutines incrementing N *different*
keys still serialize completely -- the lock does not know or care that the
keys are unrelated, it only knows one goroutine holds it and the rest must
wait. `Get` takes the read lock, and `RLock` calls can proceed together with
each other, but every single `Inc` still excludes every `Get` and every
other `Inc` for the whole map's duration, not just for the key involved.
That is the trap: a rate limiter tracking a thousand independent clients
pays for global serialization on every single request, when the actual data
dependency is only ever between operations on the *same* client's counter.

`ShardedMap` breaks that false dependency. Instead of one map behind one
lock, it holds `NumShards` (16) independent `shard` values, each its own map
and its own `sync.RWMutex`. `shardFor` hashes the key with FNV-1a -- fast,
allocation-light, and a pure function of the key's bytes, so the same key
always lands in the same shard -- and reduces the hash modulo 16 to pick an
index. Two keys that hash into different shards can be incremented by two
different goroutines truly in parallel: neither lock is shared, so neither
goroutine waits on the other. Two keys that happen to hash into the *same*
shard still serialize, same as before -- sharding reduces contention, it
does not eliminate it, and the more shards you have relative to concurrent
writers, the closer you get to no contention at all. Sixteen is a common,
modest default; production code sizes it to `GOMAXPROCS` or the expected
concurrent-writer count.

The correctness bar for both types is identical and simple: every `Inc`
must be atomic with respect to every other `Inc` and `Get` on the same key,
so that N concurrent increments of the same key always sum to exactly N.
`ShardedMap`'s per-shard lock only has to protect that one shard's map --
it does not need to know or coordinate with the other 15 -- which is exactly
what makes unrelated keys' operations independent.

Create `shardmap.go`:

```go
// Package shardmap compares a single RWMutex-guarded map against a sharded
// map that spreads keys across N independent locks, the same idea behind
// sync.Map's internal partitioning and Java's ConcurrentHashMap segments.
package shardmap

import (
	"hash/fnv"
	"sync"
)

// NumShards is the number of independent lock+map partitions ShardedMap
// uses.
const NumShards = 16

// RWMutexMap is a single map[string]int64 guarded by one sync.RWMutex.
// Every writer, regardless of key, contends for the same lock, and every
// writer blocks every reader.
//
// RWMutexMap is safe for concurrent use by multiple goroutines.
type RWMutexMap struct {
	mu sync.RWMutex
	m  map[string]int64
}

// NewRWMutexMap returns an empty RWMutexMap.
func NewRWMutexMap() *RWMutexMap {
	return &RWMutexMap{m: make(map[string]int64)}
}

// Get returns the value stored for key and whether it was present.
func (r *RWMutexMap) Get(key string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.m[key]
	return v, ok
}

// Inc adds delta to the value stored for key and returns the new value.
func (r *RWMutexMap) Inc(key string, delta int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[key] += delta
	return r.m[key]
}

// Len returns the number of distinct keys stored.
func (r *RWMutexMap) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}

// shard is one lock-protected partition of a ShardedMap.
type shard struct {
	mu sync.RWMutex
	m  map[string]int64
}

// ShardedMap partitions keys across NumShards independent shards, each
// with its own lock. Writers to keys that land in different shards do not
// contend with each other, only writers whose keys hash to the same shard
// do -- the same trade Redis Cluster's slot model and ConcurrentHashMap's
// bucket segments make to cut lock contention under many concurrent
// writers.
//
// ShardedMap is safe for concurrent use by multiple goroutines.
type ShardedMap struct {
	shards [NumShards]*shard
}

// NewShardedMap returns an empty ShardedMap.
func NewShardedMap() *ShardedMap {
	s := &ShardedMap{}
	for i := range s.shards {
		s.shards[i] = &shard{m: make(map[string]int64)}
	}
	return s
}

// shardFor picks the shard for key by hashing it with FNV-1a: fast,
// allocation-free per write, and a pure function of the key bytes, so the
// same key always routes to the same shard.
func (s *ShardedMap) shardFor(key string) *shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return s.shards[h.Sum32()%NumShards]
}

// Get returns the value stored for key and whether it was present.
func (s *ShardedMap) Get(key string) (int64, bool) {
	sh := s.shardFor(key)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.m[key]
	return v, ok
}

// Inc adds delta to the value stored for key and returns the new value.
// Only the one shard key hashes to is locked; increments to keys in other
// shards proceed in parallel.
func (s *ShardedMap) Inc(key string, delta int64) int64 {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.m[key] += delta
	return sh.m[key]
}

// Len returns the number of distinct keys stored across all shards.
func (s *ShardedMap) Len() int {
	total := 0
	for _, sh := range s.shards {
		sh.mu.RLock()
		total += len(sh.m)
		sh.mu.RUnlock()
	}
	return total
}
```

### Using it

Both types share the same three-method shape -- `Get`, `Inc`, `Len` -- so a
caller can start with `RWMutexMap` for its simplicity and switch to
`ShardedMap` later without touching call sites, only the constructor. Both
are safe for concurrent use by multiple goroutines, which is the whole point
of either type; neither returns a slice or map that could alias caller
state, so there is no aliasing contract to document beyond that. The
decision to shard is purely about contention under concurrent writers, never
about correctness -- `ShardedMap` is not "more correct" than `RWMutexMap`,
it is faster once enough goroutines are writing to different keys at once.

The module has no `main.go`: a contended-map comparison is a package you
import into a benchmark or a service, not a tool you run standalone. Its
executable demonstration is `Example`: `go test` runs it and compares its
standard output against the `// Output:` comment, so the usage shown below
cannot drift away from the code. It hammers both map types with the
identical concurrent workload and shows their totals agree exactly --
sharding must never change a single observable count, only how fast the
increments land. The real throughput difference belongs to
`go test -bench=. -benchtime=1000x`, where `ns/op` is measured under a
controlled harness; it cannot appear in a reproducible `Output` block
because timing depends on the machine.

Choosing between the two types in a real service is a one-line decision at
construction time, never a runtime flag: start every new counter or cache
with `RWMutexMap` unless a profiler has already shown contention on that
specific lock, then switch the one constructor call to `NewShardedMap`.
Resist the urge to expose a "sharded vs single-lock" mode on the type
itself -- that would turn a well-understood contention trade-off into a
runtime branch nobody profiles, which is exactly the shape of decision this
module keeps out of the package's exported API.

### Tests

`TestConcurrentIncrement` is the correctness core: it runs the identical
concurrent workload -- 32 goroutines, 200 increments each, spread over 8
keys, with a `Get` interleaved after every `Inc` -- against both map types
through a shared `counterMap` interface, and asserts the summed totals
match exactly and `Len` never exceeds the number of distinct keys used.
Running it under `-race` is what actually proves neither implementation has
a data race, not just that the arithmetic came out right.
`TestShardedMapDistributesAcrossShards` is a sanity check on the hash
routing itself: 200 distinct keys must not all collapse onto one shard, or
sharding would buy nothing. The two `Benchmark*` functions back up the
throughput claim under `go test -bench`, and `Example` is the runnable
demonstration that both types agree.

Create `shardmap_test.go`:

```go
package shardmap

import (
	"fmt"
	"sync"
	"testing"
)

// counterMap is the shape both RWMutexMap and ShardedMap implement, so the
// concurrency test and the Example exercise both under identical load.
type counterMap interface {
	Get(key string) (int64, bool)
	Inc(key string, delta int64) int64
	Len() int
}

func TestConcurrentIncrement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		new  func() counterMap
	}{
		{"RWMutexMap", func() counterMap { return NewRWMutexMap() }},
		{"ShardedMap", func() counterMap { return NewShardedMap() }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := tc.new()
			const goroutines = 32
			const perGoroutine = 200
			const numKeys = 8

			var wg sync.WaitGroup
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < perGoroutine; i++ {
						key := fmt.Sprintf("key-%d", (g+i)%numKeys)
						m.Inc(key, 1)
						m.Get(key) // concurrent reader interleaved with writers
					}
				}(g)
			}
			wg.Wait()

			var total int64
			for k := 0; k < numKeys; k++ {
				v, _ := m.Get(fmt.Sprintf("key-%d", k))
				total += v
			}
			want := int64(goroutines * perGoroutine)
			if total != want {
				t.Fatalf("total increments = %d, want %d", total, want)
			}
			if got := m.Len(); got > numKeys {
				t.Fatalf("Len() = %d, want at most %d", got, numKeys)
			}
		})
	}
}

func TestShardedMapDistributesAcrossShards(t *testing.T) {
	t.Parallel()

	s := NewShardedMap()
	seen := make(map[int]bool)
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("k-%d", i)
		sh := s.shardFor(key)
		for idx, cand := range s.shards {
			if cand == sh {
				seen[idx] = true
				break
			}
		}
	}
	if len(seen) < 2 {
		t.Fatalf("expected keys to spread across multiple shards, got %d distinct shard(s)", len(seen))
	}
}

func BenchmarkRWMutexMap_ParallelInc(b *testing.B) {
	m := NewRWMutexMap()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%64)
			m.Inc(key, 1)
			i++
		}
	})
}

func BenchmarkShardedMap_ParallelInc(b *testing.B) {
	m := NewShardedMap()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%64)
			m.Inc(key, 1)
			i++
		}
	})
}

// hammer runs 8 goroutines, each doing 1000 increments spread over 64
// rotating keys, against any counterMap.
func hammer(c counterMap) {
	const workers, perWorker, numKeys = 8, 1000, 64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				c.Inc(fmt.Sprintf("key-%d", i%numKeys), 1)
			}
		}()
	}
	wg.Wait()
}

// sumAll adds up every one of the 64 rotating keys hammer touches, so two
// implementations can be compared by an exact total.
func sumAll(c counterMap) int64 {
	const numKeys = 64
	var total int64
	for i := 0; i < numKeys; i++ {
		v, _ := c.Get(fmt.Sprintf("key-%d", i))
		total += v
	}
	return total
}

// Example hammers both map types with the identical concurrent workload --
// 8 goroutines times 1000 increments each, spread over 64 keys -- and shows
// they agree exactly. Sharding changes how the locks are laid out; it must
// never change a single observable count. The throughput difference this
// buys under contention belongs in go test -bench, not here: ns/op depends
// on the machine and cannot be a reproducible Output block.
func Example() {
	rw := NewRWMutexMap()
	sm := NewShardedMap()
	hammer(rw)
	hammer(sm)

	rwTotal, smTotal := sumAll(rw), sumAll(sm)
	fmt.Printf("RWMutexMap: len=%d total=%d\n", rw.Len(), rwTotal)
	fmt.Printf("ShardedMap: len=%d total=%d\n", sm.Len(), smTotal)
	fmt.Printf("both maps agree: %v\n", rwTotal == smTotal && rw.Len() == sm.Len())
	// Output:
	// RWMutexMap: len=64 total=8000
	// ShardedMap: len=64 total=8000
	// both maps agree: true
}
```

## Review

Both map types are correct exactly when `TestConcurrentIncrement` passes
under `-race`: the summed totals matching the expected count proves no
increment was lost to a race, and a clean `-race` run proves the runtime
never caught a concurrent unsynchronized access. The difference between
them is purely about contention, not correctness -- `RWMutexMap` is just as
correct as `ShardedMap`, it is only slower under concurrent writers because
every write serializes behind one lock regardless of which key it touches.
`TestShardedMapDistributesAcrossShards` guards the one thing that would
silently defeat the whole design: a broken or degenerate hash function that
routes most keys into a single shard, which would make `ShardedMap`
indistinguishable from `RWMutexMap` under load while looking like it
should be faster. `Example` pins that both types land on the identical
totals for the identical workload, which is the whole safety argument for
sharding: it must remove only the *false* dependency between unrelated
keys, never a real one between operations on the same key. Always run
`go test -count=1 -race ./...` before trusting either implementation.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the lock both map types build on, and why `RLock` alone does not remove writer-vs-writer contention.
- [hash/fnv package](https://pkg.go.dev/hash/fnv) — the FNV-1a hash used to route keys to shards.
- [sync.Map](https://pkg.go.dev/sync#Map) — the standard library's own answer to this trade-off, tuned for a different workload shape (keys written once, read many times).
- [testing.B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — the benchmarking primitive both the tests and the Example's helpers use to generate concurrent load.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-unhashable-interface-key-panic-guard.md](15-unhashable-interface-key-panic-guard.md) | Next: [17-deterministic-config-render-sorted-keys.md](17-deterministic-config-render-sorted-keys.md)
