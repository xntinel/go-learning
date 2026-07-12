# Exercise 2: Shard the hot map behind N independent locks

The mutex profile of the single-lock store points at one place: every operation
queuing behind one `Lock`. The canonical fix for a single mutex over a hot map is
to shard it — split the state into N independent maps, each with its own lock, and
route each key to a shard by its hash. Unrelated keys stop serializing behind one
another. This module builds that sharded store and proves the refactor is
behavior-preserving against the single-lock baseline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sharded-store/                independent module: example.com/sharded-store
  go.mod                      go 1.26
  store.go                    type Single (baseline); type Sharded; busyWork
  cmd/
    demo/
      main.go                 runnable demo: fill both stores, print shard spread
  store_test.go               distribution test, -race equivalence-to-Single test
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Sharded` store that hashes keys with `hash/fnv` `New32a` into `numShards` shards, each a `sync.Mutex` + map, with the same `Increment`/`Get` contract as `Single`, plus `ShardCount`/`ShardSize` test hooks.
- Test: `TestShardedHasMultipleShards` inserting 100 distinct keys and asserting more than one shard is populated; a `-race` test asserting `Sharded` produces the same total count as `Single` for the same workload.
- Verify: `go test -count=1 -race ./...`

### How sharding removes contention, and what it costs

`Sharded` holds a slice of `*shard`, each an independent `sync.Mutex` and
`map[string]int`. `shardFor(key)` hashes the key with FNV-1a (`fnv.New32a`) and
indexes into the slice with `sum % numShards`. Two goroutines touching keys that
hash to different shards take different locks and never wait for each other; only
keys landing in the same shard still serialize. With N shards and evenly-distributed
keys, contention drops roughly N-fold — which is why hash quality matters. FNV-1a
is a good non-cryptographic hash with strong avalanche behavior, so distinct keys
spread across shards well; a weak hash would pile keys into one shard and you would
be back to a single lock.

Sharding is not free, and this exercise is where you internalize the trade-offs. N
shards cost N maps and N locks of memory. They break cross-shard atomicity: you
cannot atomically update two keys that live in different shards. And any operation
that needs a globally-consistent view — total size, iteration, a snapshot — must
touch every shard, which is why the honest way to size a shard count is "near your
real parallelism", not "as large as possible". Over-sharding wastes memory and
hurts cache locality with no benefit once shards outnumber the goroutines
contending.

The critical property to prove is that sharding is *behavior-preserving*: for the
same workload, `Sharded` must produce exactly the same counts as `Single`. This
module bundles both types so the test can drive an identical load through each and
assert equality — a refactor that is faster but changes results is a bug, not an
optimization. `ShardCount` and `ShardSize` are test hooks that let a test confirm
keys actually spread across shards rather than collapsing into one.

Create `store.go`:

```go
package store

import (
	"hash/fnv"
	"sync"
)

// Single is the single-lock baseline: one mutex serializes every key.
type Single struct {
	mu   sync.Mutex
	data map[string]int
}

// NewSingle returns an empty single-lock store.
func NewSingle() *Single {
	return &Single{data: make(map[string]int)}
}

// Increment adds one to key's count under the one lock.
func (s *Single) Increment(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key]++
	guard(busyWork(64))
}

// Get returns key's count under the one lock.
func (s *Single) Get(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key]
}

type shard struct {
	mu   sync.Mutex
	data map[string]int
}

// Sharded splits the keyspace across numShards independent locks. Keys that hash
// to different shards never contend with each other.
type Sharded struct {
	shards    []*shard
	numShards uint32
}

// NewSharded returns a store with numShards independent shards (at least one).
func NewSharded(numShards int) *Sharded {
	if numShards < 1 {
		numShards = 1
	}
	shards := make([]*shard, numShards)
	for i := range shards {
		shards[i] = &shard{data: make(map[string]int)}
	}
	return &Sharded{shards: shards, numShards: uint32(numShards)}
}

// shardFor routes a key to its shard via FNV-1a hashing.
func (s *Sharded) shardFor(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return s.shards[h.Sum32()%s.numShards]
}

// Increment adds one to key's count under that key's shard lock.
func (s *Sharded) Increment(key string) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.data[key]++
	guard(busyWork(64))
}

// Get returns key's count under that key's shard lock.
func (s *Sharded) Get(key string) int {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.data[key]
}

// ShardCount reports the number of shards (test hook).
func (s *Sharded) ShardCount() int { return int(s.numShards) }

// ShardSize reports the number of entries in the i-th shard (test hook).
func (s *Sharded) ShardSize(i int) int {
	if i < 0 || i >= len(s.shards) {
		return -1
	}
	s.shards[i].mu.Lock()
	defer s.shards[i].mu.Unlock()
	return len(s.shards[i].data)
}

func busyWork(iterations int) uint64 {
	var acc uint64
	for i := range iterations {
		acc += uint64(i)*2 + 1
	}
	return acc
}

var sink uint64

func guard(v uint64) {
	if v == 1<<63 {
		sink = v
	}
}
```

### The runnable demo

The demo fills the sharded store with 100 distinct keys and prints how many shards
ended up populated, so you can see the hash spreading keys around.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sharded-store"
)

func main() {
	sh := store.NewSharded(16)
	for i := range 100 {
		sh.Increment(fmt.Sprintf("k-%d", i))
	}
	populated := 0
	for i := range sh.ShardCount() {
		if sh.ShardSize(i) > 0 {
			populated++
		}
	}
	fmt.Printf("shards=%d populated=%d\n", sh.ShardCount(), populated)

	single := store.NewSingle()
	single.Increment("x")
	single.Increment("x")
	fmt.Printf("single x=%d sharded k-0=%d\n", single.Get("x"), sh.Get("k-0"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shards=16 populated=16
single x=2 sharded k-0=1
```

### Tests

`TestShardedHasMultipleShards` proves the hash distributes: after inserting 100
distinct keys, more than one shard must be populated (a single populated shard
would mean the hash collapsed everything into one lock). `TestEquivalentToSingle`
proves the refactor preserves behavior: an identical concurrent workload driven
through both stores must yield the same total count.

Create `store_test.go`:

```go
package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestShardedHasMultipleShards(t *testing.T) {
	t.Parallel()
	s := NewSharded(16)
	for i := range 100 {
		s.Increment(fmt.Sprintf("k-%d", i))
	}
	populated := 0
	for i := range s.ShardCount() {
		if s.ShardSize(i) > 0 {
			populated++
		}
	}
	if populated < 2 {
		t.Fatalf("only %d shard(s) populated; want > 1 (a good hash distributes keys)", populated)
	}
}

func TestEquivalentToSingle(t *testing.T) {
	t.Parallel()
	const goroutines, ops = 16, 1000
	single := NewSingle()
	sharded := NewSharded(16)

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				single.Increment("k")
			}
		}()
		go func() {
			defer wg.Done()
			for range ops {
				sharded.Increment("k")
			}
		}()
	}
	wg.Wait()

	want := goroutines * ops
	if got := single.Get("k"); got != want {
		t.Fatalf("single.Get(k) = %d, want %d", got, want)
	}
	if got := sharded.Get("k"); got != want {
		t.Fatalf("sharded.Get(k) = %d, want %d (sharding changed the result)", got, want)
	}
}

func ExampleSharded() {
	s := NewSharded(8)
	s.Increment("a")
	s.Increment("a")
	s.Increment("b")
	fmt.Println(s.Get("a"), s.Get("b"), s.Get("missing"))
	// Output: 2 1 0
}
```

## Review

The sharded store is correct when it is indistinguishable from the single-lock
store in results and merely faster under contention. `TestEquivalentToSingle`
guards the "same results" half — a refactor that returns different counts is a bug.
`TestShardedHasMultipleShards` guards the "actually sharded" half — if the hash
collapsed every key into one shard you would have all the memory cost of sharding
and none of the contention benefit. The trap to avoid is over-sharding: a shard
count far above your real parallelism buys nothing and makes any global view
(total size, iteration) touch more locks than it needs to. Size shards to the
concurrency you actually run, and remember sharding breaks cross-shard atomicity —
you cannot update two keys in different shards as one atomic step.

## Resources

- [hash/fnv](https://pkg.go.dev/hash/fnv) — `New32a` and the FNV-1a construction used to route keys.
- [hash.Hash32](https://pkg.go.dev/hash#Hash32) — the `Write`/`Sum32` interface the router uses.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the per-shard lock and its zero-value contract.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-contended-single-mutex-store.md](01-contended-single-mutex-store.md) | Next: [03-busywork-critical-section-helper.md](03-busywork-critical-section-helper.md)
