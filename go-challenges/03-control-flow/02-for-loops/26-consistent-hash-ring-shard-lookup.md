# Exercise 26: Consistent Hash Ring for Shard Distribution

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Sharding a key-value store or a cache cluster across N backends by
`hash(key) % N` looks fine until a shard goes down or a shard is added:
every key's assignment shifts, and the entire cluster's cache goes cold at
once. A consistent hash ring fixes that by placing shards at many points
("virtual nodes") around a fixed hash space and routing a key to the first
node at or after its own hash position — removing one shard only
reassigns the keys that landed on that shard's virtual nodes, not the
whole keyspace. This module builds the ring and, more importantly, the
lookup's *probing* behavior when the resolved shard happens to be down.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
ring/                          module example.com/ring
  go.mod                       go 1.24
  ring.go                      Ring; New(shards, virtualNodes) *Ring; (*Ring).Lookup(key, isAlive) (string, bool)
  ring_test.go                   stability, distribution, probing past a down shard, all down, empty ring, wraparound
  cmd/demo/
    main.go                      four keys resolved with every shard alive, then with one shard marked down
```

- Files: `ring.go`, `ring_test.go`, `cmd/demo/main.go`.
- Implement: `New(shards []string, virtualNodes int) *Ring` placing each shard at `virtualNodes` hashed positions; `(*Ring).Lookup(key string, isAlive func(string) bool) (string, bool)` — binary search for the first virtual node at or after `key`'s hash (wrapping to the start), then a `for` loop bounded by the total virtual node count that walks forward probing owners until one is alive.
- Test: the same key always resolves to the same shard; 200 keys spread across all configured shards; a down shard is skipped in favor of the next live one; every shard down returns `false`; an empty ring returns `false`; a hash past the last node wraps to the first; the probe loop never runs more than once per virtual node.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/26-consistent-hash-ring-shard-lookup/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/26-consistent-hash-ring-shard-lookup
go mod edit -go=1.24
```

### Why the probe loop's bound is virtual nodes, not shard count

`Lookup` has two loop-shaped decisions layered on top of each other, and
conflating them is the easy way to get this wrong. `sort.Search` finds the
*position* on the ring in O(log n) — that part has nothing to do with
liveness. The probing itself is a separate, ordinary `for` loop that starts
at that position and walks forward, and its termination proof does not rest
on "eventually run out of distinct shards" — it rests on `i < len(r.hashes)`,
the total number of virtual nodes. That bound is deliberately generous: a
shard can own dozens of virtual nodes, so probing might revisit the *same*
down shard several times before reaching a different one, and the loop has
to tolerate that without a separate "have I tried this shard already" set.
The bound still terminates, because a full lap around the ring visits every
virtual node — and therefore every shard that owns at least one — exactly
once, so if a live shard exists anywhere on the ring the loop finds it well
before exhausting its bound, and if none exists the loop's own counter (not
a `break` on some other exit) is what ends it. That is the difference
between "found a live shard" (an early `return` from inside the loop) and
"attempted every virtual node" (the loop falls off its own bound) that this
exercise is built to make provable — `TestLookupProbeCountNeverExceedsVirtualNodeCount`
pins the second exit down directly by counting probes on a ring where every
node maps to the same, permanently-down shard.

The wraparound deserves its own note because it is the off-by-one trap of
this design: `sort.Search` returns `len(r.hashes)` when the key's hash is
greater than every virtual node's hash, meaning "insert past the end." That
is not a valid index — the correct owner is the *first* node on the ring,
because the hash space is circular, not linear. Forgetting the
`start == len(r.hashes)` check and indexing straight into `r.hashes[start]`
panics on exactly the keys whose hash happens to be the largest on the
ring, which is precisely the kind of bug that only shows up for specific
key/shard combinations in production and never in a quick smoke test.

Create `ring.go`:

```go
package ring

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// Ring is a consistent hash ring: each shard owns several virtual nodes
// spread around a 32-bit hash space, so adding or removing a shard only
// reshuffles the keys that landed on that shard's virtual nodes instead of
// the entire keyspace.
type Ring struct {
	hashes []uint32          // sorted virtual node hashes
	owners map[uint32]string // virtual node hash -> owning shard
}

// New builds a ring from shard names, each given virtualNodes positions on
// the ring. More virtual nodes smooth out load distribution at the cost of
// more memory; production rings typically use 100-200 per shard.
func New(shards []string, virtualNodes int) *Ring {
	r := &Ring{owners: make(map[uint32]string, len(shards)*virtualNodes)}
	for _, shard := range shards {
		for i := 0; i < virtualNodes; i++ {
			h := hashKey(fmt.Sprintf("%s#%d", shard, i))
			if _, exists := r.owners[h]; exists {
				continue // extremely rare hash collision: keep the first owner
			}
			r.owners[h] = shard
			r.hashes = append(r.hashes, h)
		}
	}
	sort.Slice(r.hashes, func(i, j int) bool { return r.hashes[i] < r.hashes[j] })
	return r
}

// Lookup finds the shard responsible for key: the first virtual node at or
// after key's hash position on the ring, wrapping around to the start if key
// hashes past every node. If that shard is not alive, Lookup walks forward
// around the ring probing the next virtual node's owner, until it finds a
// live shard or has probed every virtual node once.
//
// The for loop is bounded by len(r.hashes), which is exactly the number of
// virtual nodes on the ring: starting at the resolved position and taking at
// most that many steps visits every virtual node exactly once and then stops,
// so the loop terminates whether or not a live shard is ever found. Its two
// exits are therefore both provable: "found a live shard" returns immediately
// from inside the loop, and "attempted every virtual node" falls through to
// the final return after the loop body never breaks.
func (r *Ring) Lookup(key string, isAlive func(shard string) bool) (string, bool) {
	if len(r.hashes) == 0 {
		return "", false
	}
	return r.lookupFrom(hashKey(key), isAlive)
}

func (r *Ring) lookupFrom(h uint32, isAlive func(shard string) bool) (string, bool) {
	start := sort.Search(len(r.hashes), func(i int) bool { return r.hashes[i] >= h })
	if start == len(r.hashes) {
		start = 0 // key hashes past the last node: wrap to the first
	}

	for i := 0; i < len(r.hashes); i++ {
		idx := (start + i) % len(r.hashes)
		shard := r.owners[r.hashes[idx]]
		if isAlive(shard) {
			return shard, true
		}
	}
	return "", false
}

func hashKey(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
```

### The runnable demo

The demo resolves four keys on a three-shard ring, first with every shard
alive, then again after marking whichever shard owned `user:1` as down —
every key that had resolved to that shard now probes forward to a live one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ring"
)

func main() {
	r := ring.New([]string{"shard-0", "shard-1", "shard-2"}, 100)

	keys := []string{"user:1", "user:2", "user:3", "order:9001"}

	fmt.Println("all shards alive:")
	owner := make(map[string]string, len(keys))
	for _, k := range keys {
		shard, _ := r.Lookup(k, func(string) bool { return true })
		owner[k] = shard
		fmt.Printf("  %-12s -> %s\n", k, shard)
	}

	down := owner["user:1"]
	fmt.Printf("\n%s goes down, probing for a replacement:\n", down)
	isAlive := func(s string) bool { return s != down }
	for _, k := range keys {
		shard, ok := r.Lookup(k, isAlive)
		fmt.Printf("  %-12s -> %s (ok=%v)\n", k, shard, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
all shards alive:
  user:1       -> shard-2
  user:2       -> shard-2
  user:3       -> shard-2
  order:9001   -> shard-0

shard-2 goes down, probing for a replacement:
  user:1       -> shard-0 (ok=true)
  user:2       -> shard-0 (ok=true)
  user:3       -> shard-0 (ok=true)
  order:9001   -> shard-0 (ok=true)
```

### Tests

`TestLookupIsConsistentForTheSameKey` and
`TestLookupDistributesAcrossAllShards` establish the ring's basic contract.
`TestLookupProbesNextShardWhenOwnerIsDown` and
`TestLookupAllShardsDownReturnsFalse` cover the liveness probing this
module is built around. `TestLookupWrapsAroundToFirstNode` and
`TestLookupProbeCountNeverExceedsVirtualNodeCount` call the unexported
`lookupFrom` directly with a hand-built ring so the wraparound boundary and
the probe bound can be pinned to exact values instead of depending on where
a real string happens to hash.

Create `ring_test.go`:

```go
package ring

import (
	"strconv"
	"testing"
)

func alwaysAlive(string) bool { return true }

func TestLookupIsConsistentForTheSameKey(t *testing.T) {
	t.Parallel()

	r := New([]string{"shard-a", "shard-b", "shard-c"}, 10)

	first, ok := r.Lookup("user:42", alwaysAlive)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	for i := 0; i < 5; i++ {
		got, ok := r.Lookup("user:42", alwaysAlive)
		if !ok || got != first {
			t.Fatalf("Lookup() = %q, %v; want %q, true (must be stable across calls)", got, ok, first)
		}
	}
}

func TestLookupDistributesAcrossAllShards(t *testing.T) {
	t.Parallel()

	r := New([]string{"shard-a", "shard-b", "shard-c"}, 50)

	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		shard, ok := r.Lookup(keyFor(i), alwaysAlive)
		if !ok {
			t.Fatalf("Lookup(%d) ok = false", i)
		}
		seen[shard] = true
	}
	if len(seen) != 3 {
		t.Fatalf("shards used = %v, want all 3 shards represented across 200 keys", seen)
	}
}

func TestLookupProbesNextShardWhenOwnerIsDown(t *testing.T) {
	t.Parallel()

	r := New([]string{"shard-a", "shard-b", "shard-c"}, 50)

	shard, ok := r.Lookup("order:1001", alwaysAlive)
	if !ok {
		t.Fatal("ok = false, want true")
	}

	isAlive := func(s string) bool { return s != shard }
	got, ok := r.Lookup("order:1001", isAlive)
	if !ok {
		t.Fatal("ok = false, want true once probing reaches a live shard")
	}
	if got == shard {
		t.Fatalf("Lookup() returned the down shard %q again", shard)
	}
}

func TestLookupAllShardsDownReturnsFalse(t *testing.T) {
	t.Parallel()

	r := New([]string{"shard-a", "shard-b"}, 20)

	_, ok := r.Lookup("anything", func(string) bool { return false })
	if ok {
		t.Fatal("ok = true, want false when every shard is down")
	}
}

func TestLookupEmptyRingReturnsFalse(t *testing.T) {
	t.Parallel()

	r := New(nil, 10)
	_, ok := r.Lookup("key", alwaysAlive)
	if ok {
		t.Fatal("ok = true, want false on an empty ring")
	}
}

// TestLookupWrapsAroundToFirstNode exercises the ring wraparound directly,
// bypassing the string hash so the boundary is exact: a hash greater than
// every virtual node hash must resolve to the first node on the ring.
func TestLookupWrapsAroundToFirstNode(t *testing.T) {
	t.Parallel()

	r := &Ring{
		hashes: []uint32{100, 500, 900},
		owners: map[uint32]string{100: "shard-a", 500: "shard-b", 900: "shard-c"},
	}

	shard, ok := r.lookupFrom(950, alwaysAlive)
	if !ok || shard != "shard-a" {
		t.Fatalf("lookupFrom(950) = %q, %v; want shard-a, true (wrap to first node)", shard, ok)
	}
}

func TestLookupProbeCountNeverExceedsVirtualNodeCount(t *testing.T) {
	t.Parallel()

	r := &Ring{
		hashes: []uint32{10, 20, 30},
		owners: map[uint32]string{10: "a", 20: "a", 30: "a"},
	}

	probes := 0
	_, ok := r.lookupFrom(5, func(string) bool {
		probes++
		return false
	})
	if ok {
		t.Fatal("ok = true, want false")
	}
	if probes != 3 {
		t.Fatalf("probes = %d, want 3 (exactly the number of virtual nodes)", probes)
	}
}

func keyFor(i int) string {
	return "key-" + strconv.Itoa(i)
}
```

## Review

`Lookup` is correct when it returns a shard that `isAlive` accepted
whenever *any* configured shard is alive, and returns `false` only when
every shard is down — and both properties trace to the same bounded `for`
loop rather than to any special-casing of shard count versus virtual node
count. The common mistake this design avoids is writing the probe as
`for shard := down; !isAlive(shard); shard = next(shard) {}` with no
counter at all: that terminates correctly when some shard is alive, but
spins forever the moment every shard is down, because nothing ever stops
it. Binding the loop to the ring's own virtual node count instead of an
external "retry budget" is what makes the termination proof independent of
configuration elsewhere in the system. Run `go test -count=1 ./...`.

## Resources

- [Consistent hashing (Wikipedia)](https://en.wikipedia.org/wiki/Consistent_hashing) — the ring construction and virtual-node technique this module implements.
- [sort package](https://pkg.go.dev/sort#Search) — `sort.Search`, used to find the first virtual node at or after a key's hash.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the bounded probing loop and its two distinct exits.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-stream-n-way-merger.md](25-stream-n-way-merger.md) | Next: [27-vector-clock-causality-propagation.md](27-vector-clock-causality-propagation.md)
