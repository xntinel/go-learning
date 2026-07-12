# Exercise 10: Sharded per-tenant counter store with a contention benchmark

The concepts file states the granularity trade-off; this module makes it
measurable. You build a per-tenant request-counter store twice — once behind a
single coarse mutex, once split into 32 hash-picked shards — prove both correct
under contention, then run a `b.RunParallel` benchmark that shows what sharding
buys and a snapshot method that shows what it costs: cross-shard atomicity.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
shardcount/                  independent module: example.com/shardcount
  go.mod                     go 1.26
  store.go                   type Sharded; NewSharded, Add, Get, TotalSnapshot
  single.go                  type SingleLock; NewSingleLock, Add, Get, Snapshot
  cmd/
    demo/
      main.go                runnable demo: concurrent adds, merged snapshot
  store_test.go              exact totals under contention, shard determinism,
                             detached snapshot, Get table, Example, and the
                             BenchmarkSingleLockAdd vs BenchmarkShardedAdd pair
```

- Files: `store.go`, `single.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Sharded` store of 32 `{mutex, map[string]int64}` shards picked by FNV-1a hash (`hash/fnv`), a coarse `SingleLock` baseline, and a `TotalSnapshot` that locks shards one at a time.
- Test: 100 goroutines x 200 `Add` calls across 50 tenant keys assert exact per-key totals and the exact grand total under `-race`; shard selection is pinned deterministic; the benchmark pair runs under `go test -bench . -cpu 8`.
- Verify: `go test -count=1 -race ./...` then `go test -bench . -benchmem -cpu 8 ./...`

### What sharding buys, what it costs, and how to decide

A single mutex in front of one map (exercise 2's shape) serializes *every*
operation: with 8 cores hammering `Add`, seven of them are queued behind the
eighth at any instant, and the lock's hand-off cost — not the map write —
dominates. Sharding attacks exactly that: split the keyspace into N independent
`{mutex, map}` shards and route each key to one shard by hash. Two operations
contend only when their keys land in the same shard, so contention drops by
roughly the shard count for a uniformly spread keyset. The critical sections do
not get shorter; there are just N of them running in parallel.

The routing must be deterministic and stateless: the same key must always reach
the same shard or updates scatter and counts silently split. FNV-1a from
`hash/fnv` is the standard choice for this job — non-cryptographic, fast,
allocation-light, and stable for a given byte sequence. `shardFor` hashes the
key with `New32a`, takes `Sum32() % shardCount`, and indexes a fixed-size
array. The shard count is a power of two by convention (32 here) and is fixed
at construction; resizing a live sharded structure means re-routing keys and is
a different, much harder exercise.

Now the cost, which is the half people skip: **you lose cross-shard
atomicity**. `TotalSnapshot` needs data from all 32 shards, and it locks them
one at a time — while it reads shard 7, a writer may already be mutating shard
2, which it has released. The merged map is therefore not a consistent
point-in-time cut of the store; it is a slightly smeared view, each shard exact
at a slightly different instant. For monotonic counters scraped by a metrics
endpoint that is perfectly fine and every production metrics library makes the
same call. For an invariant that spans keys — "these two balances always sum to
zero" — it is disqualifying, and exercise 8's hold-both-locks approach is the
only honest option. (Locking all 32 shards simultaneously would restore
atomicity, but then every snapshot stalls all writers, and the lock order across
shards becomes your problem — by ascending index, always.)

The decision procedure is the senior takeaway: start with the single lock,
because it is simpler and preserves atomic whole-structure operations. Shard
only when a contention profile (`go test -bench`, pprof's mutex profile) shows
the coarse lock is the bottleneck. The benchmark pair in this module is that
evidence machine: `b.RunParallel` distributes `b.N` iterations across
`GOMAXPROCS` goroutines, each spinning on `pb.Next()`, which is precisely the
many-cores-one-structure shape the store sees in a real service.

Create `store.go`:

```go
package shardcount

import (
	"hash/fnv"
	"maps"
	"sync"
)

const shardCount = 32

// shard is one independently locked slice of the keyspace.
type shard struct {
	mu     sync.Mutex
	counts map[string]int64
}

// Sharded is a per-tenant counter store split into shardCount shards. Two
// operations contend only when their keys hash to the same shard.
type Sharded struct {
	shards [shardCount]shard
}

// NewSharded returns a store with all shards initialized.
func NewSharded() *Sharded {
	s := &Sharded{}
	for i := range s.shards {
		s.shards[i].counts = make(map[string]int64)
	}
	return s
}

// shardFor routes a key to its shard by FNV-1a hash. The routing is stateless
// and deterministic: one key always reaches the same shard.
func (s *Sharded) shardFor(key string) *shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &s.shards[h.Sum32()%shardCount]
}

// Add increments key by n, locking only that key's shard.
func (s *Sharded) Add(key string, n int64) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	sh.counts[key] += n
	sh.mu.Unlock()
}

// Get returns the count for key and whether it exists.
func (s *Sharded) Get(key string) (int64, bool) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	v, ok := sh.counts[key]
	return v, ok
}

// TotalSnapshot merges every shard into one detached map. Shards are locked
// ONE AT A TIME, so the result is not a consistent point-in-time cut: each
// shard is exact as of a slightly different instant. That smear is acceptable
// for monotonic counters on a metrics endpoint; it is not acceptable for
// invariants that span keys in different shards.
func (s *Sharded) TotalSnapshot() map[string]int64 {
	out := make(map[string]int64)
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		maps.Copy(out, sh.counts)
		sh.mu.Unlock()
	}
	return out
}
```

The coarse baseline is deliberately the shape of exercise 2 — one mutex, one
map, `maps.Clone` for the detached snapshot — so the benchmark compares this
chapter's default answer against its escalation.

Create `single.go`:

```go
package shardcount

import (
	"maps"
	"sync"
)

// SingleLock is the coarse baseline: one mutex in front of one map. Simpler
// than Sharded, atomic across the whole structure, and the right first choice
// until a contention profile says otherwise.
type SingleLock struct {
	mu     sync.Mutex
	counts map[string]int64
}

// NewSingleLock returns an empty store.
func NewSingleLock() *SingleLock {
	return &SingleLock{counts: make(map[string]int64)}
}

// Add increments key by n under the one lock.
func (s *SingleLock) Add(key string, n int64) {
	s.mu.Lock()
	s.counts[key] += n
	s.mu.Unlock()
}

// Get returns the count for key and whether it exists.
func (s *SingleLock) Get(key string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.counts[key]
	return v, ok
}

// Snapshot returns a detached copy of the whole store. Unlike
// Sharded.TotalSnapshot, this IS a consistent point-in-time cut: one lock
// covers the entire structure.
func (s *SingleLock) Snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.counts)
}
```

### The runnable demo

The demo fans 100 workers over three tenant keys and prints the merged
snapshot in sorted key order. The totals are exact — sharding must never trade
away correctness, only snapshot consistency.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"example.com/shardcount"
)

func main() {
	s := shardcount.NewSharded()

	const workers = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			s.Add("tenant-a", 1)
			s.Add("tenant-b", 2)
			s.Add("tenant-c", 3)
		}()
	}
	wg.Wait()

	snap := s.TotalSnapshot()
	var total int64
	for _, k := range slices.Sorted(maps.Keys(snap)) {
		fmt.Printf("%s = %d\n", k, snap[k])
		total += snap[k]
	}
	fmt.Printf("total = %d\n", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tenant-a = 100
tenant-b = 200
tenant-c = 300
total = 600
```

### Tests and the benchmark pair

Correctness comes first: `TestExactTotalsUnderContention` drives 100 goroutines
x 200 `Add` calls across 50 tenant keys arranged so every key receives exactly
400 increments — any routing instability or lost update breaks an exact
assertion. `TestSameKeySameShard` pins routing determinism directly, including
across store instances. The benchmarks then quantify the trade: run them with
`go test -bench . -benchmem -cpu 8` and read ns/op — the sharded store's
advantage grows with core count and shrinks to nothing (or inverts, because of
the extra hash) at `-cpu 1`. That inversion is the "start coarse" argument in
one number.

Create `store_test.go`:

```go
package shardcount

import (
	"fmt"
	"sync"
	"testing"
)

func TestExactTotalsUnderContention(t *testing.T) {
	t.Parallel()

	s := NewSharded()
	const goroutines, perG, tenants = 100, 200, 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			for j := range perG {
				s.Add(fmt.Sprintf("tenant-%02d", (g*perG+j)%tenants), 1)
			}
		}()
	}
	wg.Wait()

	const wantPer = goroutines * perG / tenants
	for i := range tenants {
		key := fmt.Sprintf("tenant-%02d", i)
		got, ok := s.Get(key)
		if !ok || got != wantPer {
			t.Errorf("Get(%q) = %d, %v; want %d, true (lost or misrouted update)", key, got, ok, wantPer)
		}
	}

	var total int64
	for _, v := range s.TotalSnapshot() {
		total += v
	}
	if want := int64(goroutines * perG); total != want {
		t.Fatalf("grand total = %d, want exactly %d", total, want)
	}
}

func TestSameKeySameShard(t *testing.T) {
	t.Parallel()

	s := NewSharded()
	other := NewSharded()
	keys := []string{"tenant-a", "tenant-b", "eu-west-1/tenant-c", "x", ""}
	for _, key := range keys {
		if s.shardFor(key) != s.shardFor(key) {
			t.Errorf("shardFor(%q) is not stable within one store", key)
		}
		// The route depends only on the key bytes, never on store identity:
		// the shard INDEX must match across independent instances.
		i1 := indexOf(t, s, s.shardFor(key))
		i2 := indexOf(t, other, other.shardFor(key))
		if i1 != i2 {
			t.Errorf("shardFor(%q) index = %d in one store, %d in another", key, i1, i2)
		}
	}
}

func indexOf(t *testing.T, s *Sharded, sh *shard) int {
	t.Helper()
	for i := range s.shards {
		if &s.shards[i] == sh {
			return i
		}
	}
	t.Fatal("shard pointer not found in store")
	return -1
}

func TestGet(t *testing.T) {
	t.Parallel()

	s := NewSharded()
	s.Add("tenant-a", 3)
	s.Add("tenant-a", 4)
	s.Add("tenant-b", 0)

	tests := []struct {
		key    string
		want   int64
		wantOK bool
	}{
		{"tenant-a", 7, true},
		{"tenant-b", 0, true}, // present with zero: ok distinguishes it
		{"tenant-z", 0, false},
	}
	for _, tt := range tests {
		if got, ok := s.Get(tt.key); got != tt.want || ok != tt.wantOK {
			t.Errorf("Get(%q) = %d, %v; want %d, %v", tt.key, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestTotalSnapshotDetached(t *testing.T) {
	t.Parallel()

	s := NewSharded()
	s.Add("tenant-a", 5)

	snap := s.TotalSnapshot()
	snap["tenant-a"] = 999_999 // mutating the merge must not reach the store

	if got, _ := s.Get("tenant-a"); got != 5 {
		t.Fatalf("Get after snapshot mutation = %d, want 5", got)
	}
}

func ExampleSharded_Add() {
	s := NewSharded()
	s.Add("tenant-a", 3)
	s.Add("tenant-a", 4)
	v, ok := s.Get("tenant-a")
	fmt.Println(v, ok)
	// Output: 7 true
}

// benchKeys is a hot keyset small enough that the single lock is genuinely
// contended and the sharded store spreads it across many shards.
func benchKeys() []string {
	keys := make([]string, 16)
	for i := range keys {
		keys[i] = fmt.Sprintf("tenant-%02d", i)
	}
	return keys
}

func BenchmarkSingleLockAdd(b *testing.B) {
	s := NewSingleLock()
	keys := benchKeys()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Add(keys[i%len(keys)], 1)
			i++
		}
	})
}

func BenchmarkShardedAdd(b *testing.B) {
	s := NewSharded()
	keys := benchKeys()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Add(keys[i%len(keys)], 1)
			i++
		}
	})
}
```

Run the benchmark pair:

```bash
go test -bench . -benchmem -cpu 8 ./...
```

One real run on an 8-core arm64 machine (absolute numbers vary; the *ratio* is
the finding):

```
BenchmarkSingleLockAdd-8   	14204415	        91.58 ns/op	       0 B/op	       0 allocs/op
BenchmarkShardedAdd-8      	31079895	        39.86 ns/op	       0 B/op	       0 allocs/op
```

Re-run with `-cpu 1,2,4,8` and watch the gap open as cores are added: at one
CPU the two stores are near-identical (sharding pays its hash for nothing),
and the crossover point is exactly the evidence a design review should ask for
before accepting the sharded version's complexity.

## Review

The store is correct when routing is a pure function of the key bytes and every
`Add`/`Get` touches exactly one shard's lock — the exact-totals test fails on
either a lost update (a shard mutex missing) or a routing instability (the same
key reaching two shards, splitting its count). The snapshot test proves the
merge is detached; the *documentation* on `TotalSnapshot` is as much a part of
the exercise as the code, because the consistency smear is invisible in any
test that runs at quiescence.

The mistake this module exists to prevent is sharding by reflex. The sharded
store costs a hash per operation, loses point-in-time snapshots, and cannot
support cross-key invariants — and at low core counts or low contention it is
no faster. Keep the `SingleLock` shape as the default; escalate when
`-bench -cpu N` or a pprof mutex profile shows the coarse lock is where time
goes, and record the benchmark numbers in the commit that introduces the
shards. Run `go test -count=1 -race ./...` and then the benchmark pair.

## Resources

- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — FNV-1a: `New32a`, `Sum32`, and its non-cryptographic intent.
- [`testing.B.RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — the parallel benchmark harness and `PB.Next`.
- [`maps`](https://pkg.go.dev/maps) — `Clone` and `Copy` for detached snapshots.
- [Go blog: subtests and sub-benchmarks](https://go.dev/blog/subtests) — structuring comparative benchmarks.

---

Back to [09-trylock-refresh-guard.md](09-trylock-refresh-guard.md) | Next: [../02-sync-rwmutex/00-concepts.md](../02-sync-rwmutex/00-concepts.md)
