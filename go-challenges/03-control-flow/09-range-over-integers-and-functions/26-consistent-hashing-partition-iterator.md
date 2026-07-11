# Exercise 26: Consistent Hashing Partition Iterator — Shard a Stream to N Replica Buckets via Ring Hash

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed system that routes requests to replica shards by plain `hash(key) %
N` breaks almost every key's assignment the moment `N` changes -- adding one
replica to rebalance load remaps nearly the entire keyspace, which is exactly
the mass cache-miss storm or connection-reshuffle a rolling deploy should never
cause. Consistent hashing fixes this by placing replicas and keys on the same
hash ring: a key is routed to the first replica found walking clockwise from
its hash, so adding or removing one replica only ever remaps the keys between
its neighbors, not the whole ring. This exercise is an independent module with
its own `go mod init`.

## What you'll build

```text
partition/                 independent module: example.com/consistent-hashing-partition-iterator
  go.mod                    module example.com/consistent-hashing-partition-iterator
  partition.go              Ring, New, PartitionBy
  cmd/
    demo/
      main.go               runnable demo: 12 keys sharded across 3 replicas
  partition_test.go         full coverage, determinism, early-stop, panics
```

Implement: `New(replicas, vnodesPerReplica int) *Ring` and `(*Ring) PartitionBy(keys iter.Seq[string]) []iter.Seq[string]` returning one lazy stream per replica.
Test: every key lands in exactly one replica's stream; the same ring assigns the same key to the same replica across repeated calls; breaking out of one replica's stream early leaves the others reachable; `New` panics on a non-positive replica or vnode count.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/consistent-hashing-partition-iterator/cmd/demo
cd ~/go-exercises/consistent-hashing-partition-iterator
go mod init example.com/consistent-hashing-partition-iterator
go mod edit -go=1.24
```

A ring with exactly one point per replica is lumpy: whichever replica happens
to own the largest arc of hash space gets a disproportionate share of keys,
and that share is entirely a function of where the hash function happened to
place three or four numbers. The fix is virtual nodes -- each replica is
represented by many points scattered around the ring (`vnodesPerReplica`),
so the law of large numbers evens out each replica's total arc length even
though any single point is still placed arbitrarily. `PartitionBy` returns
`N` separate `iter.Seq[string]`, one per replica, and each one independently
ranges the same `keys` source and filters by `assign(key) == rep`. That means
`keys` runs once per replica requested, not once overall -- correct because
every source this exercise hands it is a pure, re-runnable closure over a
slice, and it keeps each replica's stream lazy: a consumer that only wants
the first key on replica 0 never forces replica 1 or 2 to be walked at all.
A production ring built over a live key source that is *not* re-runnable
(a single-pass channel drain, say) would need to materialize `keys` once
first and bucket from the materialized slice instead.

Create `partition.go`:

```go
package partition

import (
	"hash/fnv"
	"iter"
	"sort"
	"strconv"
)

// Ring is a consistent-hash ring over a fixed number of replicas. Each
// replica owns several virtual nodes (vnodes) scattered around the hash
// space, which is what keeps the key distribution roughly even even though
// there are only a handful of replicas; a ring with exactly one vnode per
// replica would let one unlucky replica own a disproportionate arc.
type Ring struct {
	replicas int
	points   []point // sorted ascending by hash
}

// point is one vnode's position on the ring and the replica it belongs to.
type point struct {
	hash    uint32
	replica int
}

// New builds a Ring with the given number of replicas, each represented by
// vnodesPerReplica points scattered across the hash space. It panics if
// replicas or vnodesPerReplica is less than 1, since a ring with zero
// replicas or zero vnodes cannot route anything.
func New(replicas, vnodesPerReplica int) *Ring {
	if replicas < 1 {
		panic("partition: replicas must be >= 1")
	}
	if vnodesPerReplica < 1 {
		panic("partition: vnodesPerReplica must be >= 1")
	}
	r := &Ring{replicas: replicas}
	for rep := 0; rep < replicas; rep++ {
		for v := 0; v < vnodesPerReplica; v++ {
			label := "replica-" + strconv.Itoa(rep) + "#" + strconv.Itoa(v)
			r.points = append(r.points, point{hash: hashKey(label), replica: rep})
		}
	}
	sort.Slice(r.points, func(i, j int) bool { return r.points[i].hash < r.points[j].hash })
	return r
}

// hashKey hashes a string to a uint32 using FNV-1a, which is deterministic
// across runs and platforms -- required so a key always lands on the same
// replica on every call, not just within a single process lifetime.
func hashKey(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// assign walks the ring clockwise from key's hash and returns the replica
// index owning the first vnode at or past that position, wrapping around to
// the first vnode if key's hash is past every point. This is the classic
// consistent-hashing lookup: a binary search over sorted hash points.
func (r *Ring) assign(key string) int {
	h := hashKey(key)
	idx := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= h })
	if idx == len(r.points) {
		idx = 0
	}
	return r.points[idx].replica
}

// PartitionBy returns one iter.Seq[string] per replica (indices 0..replicas-1),
// each streaming, in the original order of keys, only the keys this ring
// assigns to that replica. Because keys is itself an iter.Seq, each returned
// per-replica iterator re-runs keys and filters -- cheap and correct as long
// as keys is a pure, re-runnable sequence (a slice-backed source, not a
// single-pass channel drain), which is the case for every caller in this
// exercise. Filtering per replica this way, instead of first collecting keys
// into a []string and bucketing them into N slices, keeps each replica's
// stream lazy: a consumer of replica 0 that only wants the first match never
// forces the other replicas to be materialized at all.
func (r *Ring) PartitionBy(keys iter.Seq[string]) []iter.Seq[string] {
	out := make([]iter.Seq[string], r.replicas)
	for rep := 0; rep < r.replicas; rep++ {
		rep := rep
		out[rep] = func(yield func(string) bool) {
			for key := range keys {
				if r.assign(key) == rep {
					if !yield(key) {
						return
					}
				}
			}
		}
	}
	return out
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/consistent-hashing-partition-iterator"
)

func main() {
	ring := partition.New(3, 300)

	keys := func(yield func(string) bool) {
		for _, k := range []string{
			"user:1001", "user:1002", "user:1003", "user:1004",
			"user:1005", "user:1006", "user:1007", "user:1008",
			"user:2001", "user:2002", "user:3003", "user:4004",
		} {
			if !yield(k) {
				return
			}
		}
	}

	streams := ring.PartitionBy(keys)
	for rep, stream := range streams {
		fmt.Printf("replica %d:", rep)
		for key := range stream {
			fmt.Printf(" %s", key)
		}
		fmt.Println()
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
replica 0: user:2001
replica 1: user:1004 user:1005 user:2002 user:3003 user:4004
replica 2: user:1001 user:1002 user:1003 user:1006 user:1007 user:1008
```

The split is uneven with only 12 keys and 300 vnodes per replica -- that is
expected, not a bug: consistent hashing's guarantee is about *stability under
membership change*, not perfectly even load with a small sample. With
thousands of keys the law of large numbers makes each replica's share
converge toward `1/replicas` of the total.

### Tests

The determinism test is the one that matters most in production: it proves
that two separate calls to `PartitionBy` on the same `Ring` assign every key
to the same replica, which is the property a caller relies on to route a
retried request to the replica that already has the connection or cached
state warm.

Create `partition_test.go`:

```go
package partition

import "testing"

func keysSeq(keys []string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for _, k := range keys {
			if !yield(k) {
				return
			}
		}
	}
}

func TestPartitionByCoversEveryKeyExactlyOnce(t *testing.T) {
	t.Parallel()

	ring := New(4, 50)
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	streams := ring.PartitionBy(keysSeq(keys))
	if len(streams) != 4 {
		t.Fatalf("got %d streams, want 4", len(streams))
	}

	seen := map[string]int{}
	for _, stream := range streams {
		for key := range stream {
			seen[key]++
		}
	}
	if len(seen) != len(keys) {
		t.Fatalf("got %d distinct keys assigned, want %d", len(seen), len(keys))
	}
	for _, key := range keys {
		if seen[key] != 1 {
			t.Fatalf("key %q assigned to %d replicas, want exactly 1", key, seen[key])
		}
	}
}

func TestPartitionByIsDeterministic(t *testing.T) {
	t.Parallel()

	ring := New(3, 20)
	keys := []string{"order:1", "order:2", "order:3"}

	first := map[string]int{}
	for rep, stream := range ring.PartitionBy(keysSeq(keys)) {
		for key := range stream {
			first[key] = rep
		}
	}

	second := map[string]int{}
	for rep, stream := range ring.PartitionBy(keysSeq(keys)) {
		for key := range stream {
			second[key] = rep
		}
	}

	for key, rep := range first {
		if second[key] != rep {
			t.Fatalf("key %q assigned to replica %d then %d: assignment must be stable", key, rep, second[key])
		}
	}
}

func TestPartitionByEarlyStopOnOneStreamLeavesOthersReachable(t *testing.T) {
	t.Parallel()

	ring := New(2, 20)
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
	streams := ring.PartitionBy(keysSeq(keys))

	for range streams[0] {
		break // stop replica 0's stream after at most one key
	}

	total := 0
	for _, stream := range streams {
		for range stream {
			total++
		}
	}
	if total != len(keys) {
		t.Fatalf("got %d keys across a fresh pass, want %d: breaking one stream must not affect the others", total, len(keys))
	}
}

func TestNewPanicsOnInvalidArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		replicas         int
		vnodesPerReplica int
	}{
		{"zero replicas", 0, 10},
		{"zero vnodes", 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			New(tc.replicas, tc.vnodesPerReplica)
		})
	}
}
```

## Review

The property that makes this a consistent-hash ring rather than a plain
`hash(key) % N` sharder is that `assign` never looks at `replicas` directly --
it only ever finds "the next point clockwise." That is what limits the blast
radius of a topology change to the arc around the point that moved: adding a
replica inserts new points and only the keys that used to map to the vnode
immediately after each new point get reassigned, everything else keeps its
existing owner. The common mistake is reaching for `hash(key) % replicas`
because it looks simpler -- it is, but every membership change remaps
essentially the whole keyspace, which is precisely the cache-stampede and
connection-storm failure mode consistent hashing was invented to avoid.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Karger et al., "Consistent Hashing and Random Trees" (1997)](https://dl.acm.org/doi/10.1145/258533.258660)
- [`hash/fnv` package documentation](https://pkg.go.dev/hash/fnv)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-cache-invalidation-cascade.md](25-cache-invalidation-cascade.md) | Next: [27-bloom-filter-space-efficient-dedup.md](27-bloom-filter-space-efficient-dedup.md)
