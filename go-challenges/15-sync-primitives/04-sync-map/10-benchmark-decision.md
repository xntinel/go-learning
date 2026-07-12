# Exercise 10: The benchmark that decides: sync.Map vs map+RWMutex on your access profile

Every previous module deferred one question: on *your* workload, on *your*
toolchain, is `sync.Map` actually faster than `map`+`sync.RWMutex`? This
capstone builds the evidence — two interchangeable cache implementations and a
parallel benchmark suite across the three access profiles that occur in real
backends — so the data-structure choice survives code review with numbers
instead of folklore.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
benchdecision/                independent module: example.com/benchdecision
  go.mod                      go 1.26
  cache.go                    Cache interface; SyncMapCache; MutexCache (NewMutexCache)
  cmd/
    demo/
      main.go                 runnable demo: scripted workload, both caches end identical
  cache_test.go               shared contract table, concurrent same-key writes,
                              scripted-sequence agreement, Example
  bench_test.go               BenchmarkReadHeavyStableKeys / BenchmarkDisjointKeySets /
                              BenchmarkChurnFullKeyspace, each b.Run x b.RunParallel
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`, `bench_test.go`.
- Implement: a minimal `Cache` interface (`Get`, `Put`, `Delete`) with a `sync.Map`-backed and a `map[string]int64`+`sync.RWMutex`-backed implementation, plus three parallel benchmarks (read-heavy stable keys, disjoint key sets, full-keyspace churn) using `b.Run`, `b.RunParallel`, `b.ReportAllocs`, and `b.SetParallelism`.
- Test: a shared contract table runs both implementations through identical cases; 100 concurrent same-key writers leave a well-formed survivor; a deterministic scripted operation sequence leaves both caches in exactly the same final state.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem -count=1 ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/10-benchmark-decision && cd go-solutions/15-sync-primitives/04-sync-map/10-benchmark-decision
```

### Correctness before speed

A benchmark comparing a correct implementation against a broken one measures
nothing, so the module is structured the way a real performance investigation
is: first prove the two candidates are interchangeable, then race them. The
`Cache` interface is deliberately minimal — `Get`, `Put`, `Delete` over
`string` keys and `int64` values — because that is what an in-process route
table or tenant-metadata cache actually needs, and because any extra surface
(TTLs, callbacks) would smear the comparison. The contract test drives both
implementations through one table; the scripted-sequence test goes further and
replays a single deterministic 5,000-operation script (seeded
`math/rand/v2` PCG, so it is reproducible across runs and machines) against
both caches serially, then asserts they agree on the final state of every key.
If a refactor ever makes the two diverge, the benchmark results become
meaningless and this test fails first.

The two implementations embody the trade-off the whole lesson has circled.
`SyncMapCache` stores `int64` values through `any`, so every `Put` of a
non-small value can heap-allocate a box, and every `Get` pays a type
assertion — but its operations never take a lock the caller can feel.
`MutexCache` has a concrete value type, zero boxing, and a cheap `len()` if
you ever need one — but every reader shares one `RWMutex` cache line, and
under high parallelism the atomic reader-count in `RLock`/`RUnlock` itself
becomes the contention point. Neither cost is visible in code review. Both
are visible in `-benchmem` output.

### The three profiles, and why RunParallel

`b.RunParallel` runs the body on `GOMAXPROCS` goroutines (multiplied by
`b.SetParallelism`), each pulling iterations from a shared `testing.PB` via
`pb.Next()`. That is the shape of a real server — many goroutines hammering
one shared structure — and it is the only benchmark shape where lock
contention shows up at all; a serial loop would measure the uncontended fast
path and mislead you. Each goroutine gets its own `rand/v2` generator seeded
from `rand.Uint64()`, because sharing one `*rand.Rand` across goroutines
would serialize the benchmark on the generator's own lock and measure that
instead of the cache.

The three profiles are the three that actually occur in production:

- **Read-heavy stable keys** (99% `Get`, 1% overwrite over a fixed,
  pre-populated key set): a route table, a feature-flag snapshot, a service
  registry. This is the first documented `sync.Map` pattern and its home turf.
- **Disjoint key sets**: each goroutine claims a private 64-key shard (an
  atomic counter hands out shard IDs inside `RunParallel`, since the body
  cannot take parameters) and read-modify-writes only its own keys. The
  second documented pattern: goroutines never contend on the same keys, only
  on the structure itself.
- **Churn**: 40% `Put`, 10% `Delete`, 50% `Get` spread across the whole
  keyspace — a session table under load. This is the profile the `sync.Map`
  documentation historically steered away from, and the one where the answer
  has genuinely changed: Go 1.24 replaced the two-map read/dirty design with
  a concurrent HashTrieMap that scales writes and deletes far better.

`b.ReportAllocs` is non-negotiable here: allocs/op is where the `any`-boxing
tax appears, and a structure that wins ns/op while allocating on every write
can still lose in a service where GC pressure is the real budget. Note one
subtlety you will see in your own numbers: Go interns small integers when
converting to `any`, so the read-heavy profile (which only ever stores the
value 1) shows zero allocs for `sync.Map`, while churn (random values up to a
million) pays one allocation per `Put`. The boxing cost depends on the values
you store, not just the operation mix — another reason to benchmark your
profile, not a synthetic one.

### Reading the results: a decision matrix

On an Apple M4, Go 1.26 (`go test -bench=. -benchmem`), this suite produced:

```text
BenchmarkReadHeavyStableKeys/syncmap-10    4.5 ns/op     0 B/op   0 allocs/op
BenchmarkReadHeavyStableKeys/rwmutex-10   55.3 ns/op     0 B/op   0 allocs/op
BenchmarkDisjointKeySets/syncmap-10       28.6 ns/op    71 B/op   2 allocs/op
BenchmarkDisjointKeySets/rwmutex-10      106.4 ns/op     0 B/op   0 allocs/op
BenchmarkChurnFullKeyspace/syncmap-10     21.2 ns/op    29 B/op   1 allocs/op
BenchmarkChurnFullKeyspace/rwmutex-10     62.4 ns/op     0 B/op   0 allocs/op
```

The matrix this buys you in review: on this toolchain `sync.Map` wins ns/op
on all three profiles — including churn, where pre-1.24 folklore says it
loses — but it pays allocations on every boxed write while `RWMutex` pays
none. So the honest conclusions are conditional, not absolute: read-heavy
stable keys is a `sync.Map` landslide (the documented case, unchanged);
disjoint and churn are now `sync.Map`-favored on latency *because of the Go
1.24 HashTrieMap rewrite*, at the price of per-write garbage that a
GC-sensitive service may refuse; and a Go 1.23-era benchmark result cached in
your head (or your team's wiki) is stale. Your machine, core count, key-set
size, value types, and Go version will move these numbers — which is the
final lesson: the durable rule is not "sync.Map wins churn now", it is
*measure on the toolchain you ship, with your access profile, and re-measure
when the toolchain changes*. For statistically sound before/after comparisons
run with `-count=10` and feed the output to `benchstat`.

Create `cache.go`:

```go
// Package benchdecision holds two implementations of the same tiny cache
// contract — one on sync.Map, one on map+sync.RWMutex — so a parallel
// benchmark suite can decide which one a given access profile deserves.
package benchdecision

import "sync"

// Cache is the minimal contract an in-process metadata cache (route table,
// tenant lookup) needs. Both implementations must satisfy it identically so
// the benchmark compares data structures, not features.
type Cache interface {
	Get(key string) (int64, bool)
	Put(key string, v int64)
	Delete(key string)
}

// SyncMapCache backs the contract with sync.Map. Values cross the any
// interface on every Put, which for a non-pointer int64 can heap-allocate:
// that cost is exactly what -benchmem exposes.
type SyncMapCache struct {
	m sync.Map // map[string]int64
}

// Get returns the value for key and whether it was present.
func (c *SyncMapCache) Get(key string) (int64, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return 0, false
	}
	return v.(int64), true
}

// Put stores v under key, overwriting any previous value.
func (c *SyncMapCache) Put(key string, v int64) { c.m.Store(key, v) }

// Delete removes key; deleting an absent key is a no-op.
func (c *SyncMapCache) Delete(key string) { c.m.Delete(key) }

// MutexCache backs the contract with a plain typed map guarded by a
// sync.RWMutex: concrete value type, no boxing, cheap len if you ever need
// it — the documented default for most concurrent-map workloads.
type MutexCache struct {
	mu sync.RWMutex
	m  map[string]int64
}

// NewMutexCache returns a ready-to-use MutexCache.
func NewMutexCache() *MutexCache {
	return &MutexCache{m: make(map[string]int64)}
}

// Get returns the value for key and whether it was present.
func (c *MutexCache) Get(key string) (int64, bool) {
	c.mu.RLock()
	v, ok := c.m[key]
	c.mu.RUnlock()
	return v, ok
}

// Put stores v under key, overwriting any previous value.
func (c *MutexCache) Put(key string, v int64) {
	c.mu.Lock()
	c.m[key] = v
	c.mu.Unlock()
}

// Delete removes key; deleting an absent key is a no-op.
func (c *MutexCache) Delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}
```

Note that `MutexCache.Get` uses explicit `RLock`/`RUnlock` rather than
`defer`: on a path this hot the defer overhead, though small in modern Go, is
part of what you are measuring, and the explicit pair keeps the comparison
about the data structures. Also note `SyncMapCache` is usable as its zero
value (as `sync.Map` itself is) while `MutexCache` needs a constructor to
allocate the map — a small API asymmetry that the shared interface hides from
callers.

### The runnable demo

The demo replays one deterministic scripted workload (seeded PCG: about 90%
puts, 10% deletes over 256 route keys) against both caches and verifies they
finish in exactly the same state — the interchangeability proof, condensed.

Create `cmd/demo/main.go`:

```go
// Command demo drives both cache implementations through one deterministic
// scripted workload and shows they end in the same state — the correctness
// precondition that makes the benchmark comparison meaningful.
package main

import (
	"fmt"
	"math/rand/v2"

	"example.com/benchdecision"
)

func main() {
	rng := rand.New(rand.NewPCG(1, 2))
	sm := &benchdecision.SyncMapCache{}
	mx := benchdecision.NewMutexCache()

	ks := make([]string, 256)
	for i := range ks {
		ks[i] = fmt.Sprintf("route-%03d", i)
	}

	puts, dels := 0, 0
	for range 10_000 {
		k := ks[rng.IntN(len(ks))]
		if rng.IntN(10) == 0 {
			sm.Delete(k)
			mx.Delete(k)
			dels++
		} else {
			v := int64(rng.IntN(1_000))
			sm.Put(k, v)
			mx.Put(k, v)
			puts++
		}
	}

	agree, live := 0, 0
	for _, k := range ks {
		sv, sok := sm.Get(k)
		mv, mok := mx.Get(k)
		if sv == mv && sok == mok {
			agree++
		}
		if sok {
			live++
		}
	}

	fmt.Println("puts:", puts, "deletes:", dels)
	fmt.Println("keys agreeing across implementations:", agree, "of", len(ks))
	fmt.Println("live keys after churn:", live)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
puts: 9011 deletes: 989
keys agreeing across implementations: 256 of 256
live keys after churn: 226
```

The output is identical on every run and every machine because `rand/v2`'s
PCG is a specified, seedable algorithm — the same property the scripted
agreement test relies on.

### Tests

The contract table runs each case against each implementation as its own
parallel subtest, so a failure names the exact implementation and case.
`TestConcurrentSameKeyWrites` is the well-formedness check under `-race`:
100 racing writers to one key must leave *some* written value, never a torn
or absent one. `TestScriptedSequenceAgreement` is the interchangeability
proof described above.

Create `cache_test.go`:

```go
package benchdecision

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

var implementations = []struct {
	name string
	make func() Cache
}{
	{"syncmap", func() Cache { return &SyncMapCache{} }},
	{"rwmutex", func() Cache { return NewMutexCache() }},
}

func TestCacheContract(t *testing.T) {
	t.Parallel()

	steps := []struct {
		name   string
		op     func(c Cache)
		key    string
		wantV  int64
		wantOK bool
	}{
		{"miss returns ok=false", func(c Cache) {}, "absent", 0, false},
		{"put then get round-trips", func(c Cache) { c.Put("a", 41) }, "a", 41, true},
		{"put overwrites", func(c Cache) { c.Put("a", 1); c.Put("a", 2) }, "a", 2, true},
		{"delete removes", func(c Cache) { c.Put("a", 9); c.Delete("a") }, "a", 0, false},
		{"delete absent is a no-op", func(c Cache) { c.Delete("ghost") }, "ghost", 0, false},
	}

	for _, impl := range implementations {
		for _, tc := range steps {
			t.Run(impl.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				c := impl.make()
				tc.op(c)
				v, ok := c.Get(tc.key)
				if v != tc.wantV || ok != tc.wantOK {
					t.Fatalf("Get(%q) = (%d, %t), want (%d, %t)",
						tc.key, v, ok, tc.wantV, tc.wantOK)
				}
			})
		}
	}
}

func TestConcurrentSameKeyWrites(t *testing.T) {
	t.Parallel()

	for _, impl := range implementations {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()
			c := impl.make()

			const writers = 100
			var wg sync.WaitGroup
			for i := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.Put("hot", int64(i))
				}()
			}
			wg.Wait()

			v, ok := c.Get("hot")
			if !ok {
				t.Fatal("key vanished after concurrent writes")
			}
			if v < 0 || v >= writers {
				t.Fatalf("survivor %d is not one of the written values [0,%d)", v, writers)
			}
		})
	}
}

// TestScriptedSequenceAgreement drives both implementations through one
// deterministic operation script and asserts they agree on the final state
// of every key: the benchmark below compares two caches that are provably
// interchangeable.
func TestScriptedSequenceAgreement(t *testing.T) {
	t.Parallel()

	sm := &SyncMapCache{}
	mx := NewMutexCache()
	rng := rand.New(rand.NewPCG(11, 7))

	ks := make([]string, 128)
	for i := range ks {
		ks[i] = fmt.Sprintf("key-%03d", i)
	}

	for range 5_000 {
		k := ks[rng.IntN(len(ks))]
		switch rng.IntN(10) {
		case 0, 1: // 20% delete
			sm.Delete(k)
			mx.Delete(k)
		default: // 80% put
			v := int64(rng.IntN(1_000_000))
			sm.Put(k, v)
			mx.Put(k, v)
		}
	}

	for _, k := range ks {
		sv, sok := sm.Get(k)
		mv, mok := mx.Get(k)
		if sv != mv || sok != mok {
			t.Fatalf("final state diverges at %q: syncmap=(%d,%t) rwmutex=(%d,%t)",
				k, sv, sok, mv, mok)
		}
	}
}

func ExampleCache() {
	for _, c := range []Cache{&SyncMapCache{}, NewMutexCache()} {
		c.Put("tenant-7", 42)
		v, ok := c.Get("tenant-7")
		fmt.Println(v, ok)
	}
	// Output:
	// 42 true
	// 42 true
}
```

Create `bench_test.go`:

```go
package benchdecision

import (
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"
)

const (
	keyspace  = 1024
	shardSize = 64
)

func benchKeys() []string {
	ks := make([]string, keyspace)
	for i := range keyspace {
		ks[i] = fmt.Sprintf("key-%04d", i)
	}
	return ks
}

// BenchmarkReadHeavyStableKeys models a route table: the key set is fixed
// and pre-populated, 99% of operations are Gets, 1% are overwriting Puts.
// This is documented sync.Map home turf.
func BenchmarkReadHeavyStableKeys(b *testing.B) {
	ks := benchKeys()
	for _, impl := range implementations {
		b.Run(impl.name, func(b *testing.B) {
			c := impl.make()
			for i, k := range ks {
				c.Put(k, int64(i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
				for pb.Next() {
					k := ks[rng.IntN(keyspace)]
					if rng.IntN(100) == 0 {
						c.Put(k, 1)
					} else {
						c.Get(k)
					}
				}
			})
		})
	}
}

// BenchmarkDisjointKeySets models sharded workers: each benchmark goroutine
// owns a private 64-key range and read-modify-writes only inside it. The
// second documented sync.Map pattern.
func BenchmarkDisjointKeySets(b *testing.B) {
	ks := benchKeys()
	for _, impl := range implementations {
		b.Run(impl.name, func(b *testing.B) {
			c := impl.make()
			var next atomic.Int64
			b.SetParallelism(4)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				id := int(next.Add(1) - 1)
				lo := (id * shardSize) % keyspace
				own := ks[lo : lo+shardSize]
				i := 0
				for pb.Next() {
					k := own[i%shardSize]
					v, _ := c.Get(k)
					c.Put(k, v+1)
					i++
				}
			})
		})
	}
}

// BenchmarkChurnFullKeyspace models a session table under load: every
// goroutine Puts, Deletes, and Gets over the whole keyspace. The profile
// the sync.Map docs steer away from — and the one Go 1.24's HashTrieMap
// rewrite changed the most.
func BenchmarkChurnFullKeyspace(b *testing.B) {
	ks := benchKeys()
	for _, impl := range implementations {
		b.Run(impl.name, func(b *testing.B) {
			c := impl.make()
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
				for pb.Next() {
					k := ks[rng.IntN(keyspace)]
					switch rng.IntN(10) {
					case 0, 1, 2, 3: // 40% put
						c.Put(k, int64(rng.IntN(1_000_000)))
					case 4: // 10% delete
						c.Delete(k)
					default: // 50% get
						c.Get(k)
					}
				}
			})
		})
	}
}
```

Run the correctness suite, then the benchmarks:

```bash
go test -count=1 -race ./...
go test -bench=. -benchmem -count=1 ./...
```

Three details in `bench_test.go` are the ones interviewers and reviewers
probe. `b.ResetTimer()` after the pre-population excludes setup from the
measurement — without it the read-heavy numbers include 1,024 Puts.
`b.SetParallelism(4)` in the disjoint benchmark multiplies the goroutine
count to 4 x GOMAXPROCS so shard IDs wrap the keyspace and the structure sees
more concurrent owners than cores, which is what a real sharded worker pool
looks like. And the disjoint body takes its shard from an atomic counter
because `RunParallel` gives each goroutine no identity — inventing one is the
standard idiom for per-goroutine state in parallel benchmarks.

## Review

The module is correct when both implementations pass the shared contract and
the scripted-sequence test agrees on every key, and it is *useful* when the
benchmark table lets you defend a data-structure choice with numbers from the
toolchain you ship. The classic mistakes: benchmarking with a serial loop
(no contention, so `RWMutex` looks free and the comparison is fiction);
sharing one `*rand.Rand` across `RunParallel` goroutines (you measure the
generator's mutex, not the cache); omitting `-benchmem` (the boxing tax —
often the deciding column — stays invisible); forgetting `b.ResetTimer()`
after pre-population; and generalizing a result across Go versions when the
1.24 HashTrieMap rewrite specifically changed write/delete scaling. Confirm
correctness with `go test -count=1 -race ./...`, then run
`go test -bench=. -benchmem -count=1 ./...` and check that the read-heavy
profile favors `sync.Map`, that allocs/op is nonzero only for the boxed
`sync.Map` writes of large values, and that your churn numbers match your
toolchain — not a blog post's.

## Resources

- [sync.Map documentation](https://pkg.go.dev/sync#Map) — the two documented access patterns this benchmark encodes.
- [testing.B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — parallel benchmarks, `testing.PB`, `ReportAllocs`, `SetParallelism`.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — the HashTrieMap-based `sync.Map` rewrite that changed churn performance.
- [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) — statistically sound comparison of repeated benchmark runs.

---

Back to [09-idempotency-guard.md](09-idempotency-guard.md) | Next: [../05-sync-pool/00-concepts.md](../05-sync-pool/00-concepts.md)
