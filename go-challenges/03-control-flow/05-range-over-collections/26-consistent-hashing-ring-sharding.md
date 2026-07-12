# Exercise 26: Consistent Hashing Ring for Shard Distribution

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed cache or log system splits its keyspace across N shard
processes, and the naive `hash(key) % N` scheme reshuffles almost every key
the moment N changes — adding one cache node to a ten-node fleet invalidates
roughly 90% of existing cache entries in one step, which is exactly the
thundering-herd-of-misses moment you cannot afford during a scale-up event.
Consistent hashing fixes this by placing both keys and nodes on the same
hash ring and assigning each key to the next node clockwise from it, so a
topology change only moves the keys that were genuinely owned by the node
that joined or left. This module builds that ring, ranges its sorted hash
points to locate the shard responsible for a key, and validates that a
topology change reassigns only the keys it must. The module is fully
self-contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
ring/                       independent module: example.com/consistent-hashing-ring-sharding
  go.mod                    go 1.24
  ring.go                   type Ring; AddNode, RemoveNode, Locate, Nodes
  cmd/
    demo/
      main.go               runnable demo: 4 nodes, 40 keys, add a node and count moves
  ring_test.go               table test: empty ring, determinism, minimal-reassignment on removal
```

- Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
- Implement: `Ring.AddNode`, `Ring.RemoveNode`, and `Ring.Locate`, all built
  on a sorted slice of virtual-node hash points.
- Test: an empty-ring case, a determinism case, and the minimal-reassignment
  invariant after `RemoveNode`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/26-consistent-hashing-ring-sharding/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/26-consistent-hashing-ring-sharding
go mod edit -go=1.24
```

### Virtual points turn "find the owner" into a range over a sorted slice

A ring with one point per physical node concentrates all of that node's
keyspace in one arc, so removing it dumps its entire share onto whichever
single neighbor happens to sit next to it on the ring — a spike, not a
smooth redistribution. The standard fix is virtual nodes: each physical node
gets `replicas` points scattered around the ring (`AddNode` hashes
`name + "#" + i` for `i` in `0..replicas`), so its keyspace is spread across
many small arcs instead of one big one, and losing the node spreads its load
across many neighbors instead of dumping it on one. `Locate` never needs a
tree or a binary-search library to find the owner — because `r.points` is
kept sorted on insert, a single `for _, p := range r.points` that returns the
first point `>= hash(key)` is both correct and simple to read, with the wrap-
around case (`hash(key)` greater than every point) handled by falling through
to `r.points[0]` after the loop. A production ring with thousands of virtual
points would swap that linear range for `sort.Search`'s binary search, but
the ring's correctness — which node owns which key — is identical either way;
only the constant factor changes.

The property this exercise actually validates is the reason consistent
hashing exists at all: `RemoveNode` deletes only the points that belonged to
the departing node, so every key that was *not* owned by that node keeps
its owner, and only the keys that were on it get reassigned to their new
nearest point. A plain modulo scheme has no analogous guarantee — removing
one shard out of N changes the owner of roughly `(N-1)/N` of all keys, not
just the departing shard's own.

Create `ring.go`:

```go
package ring

import (
	"hash/crc32"
	"sort"
	"strconv"
)

// Ring is a consistent-hashing ring that assigns keys to shard nodes. Each
// node is represented by multiple virtual points on the ring (replicas), so
// that removing or adding one node redistributes only a small, bounded slice
// of the key space instead of reshuffling everything.
type Ring struct {
	replicas int
	points   []uint32          // sorted ascending hash points on the ring
	nodeOf   map[uint32]string // hash point -> owning node name
}

// New builds an empty Ring where each node gets replicas virtual points.
// A higher replica count smooths the distribution at the cost of more
// bookkeeping; production caches typically use 100-200.
func New(replicas int) *Ring {
	return &Ring{
		replicas: replicas,
		nodeOf:   make(map[uint32]string),
	}
}

func hashPoint(label string) uint32 {
	return crc32.ChecksumIEEE([]byte(label))
}

// AddNode inserts name's virtual points into the ring, keeping r.points
// sorted so Locate can range it to find the first point at or after a key's
// hash.
func (r *Ring) AddNode(name string) {
	for i := 0; i < r.replicas; i++ {
		h := hashPoint(name + "#" + strconv.Itoa(i))
		if _, exists := r.nodeOf[h]; exists {
			continue // hash collision on this virtual point; skip rather than corrupt an existing owner
		}
		idx := sort.Search(len(r.points), func(j int) bool { return r.points[j] >= h })
		r.points = append(r.points, 0)
		copy(r.points[idx+1:], r.points[idx:])
		r.points[idx] = h
		r.nodeOf[h] = name
	}
}

// RemoveNode deletes every virtual point owned by name. Keys that hashed to
// those points move to their new nearest point; every other key's owner is
// unaffected, which is the entire point of consistent hashing over a plain
// mod-N sharding scheme.
func (r *Ring) RemoveNode(name string) {
	kept := r.points[:0:0]
	for _, h := range r.points {
		if r.nodeOf[h] == name {
			delete(r.nodeOf, h)
			continue
		}
		kept = append(kept, h)
	}
	r.points = kept
}

// Locate ranges the sorted ring points to find the node responsible for key:
// the owner of the first point at or after hash(key), wrapping around to the
// first point on the ring if key's hash is past every point. The second
// return is false only when the ring has no nodes at all.
func (r *Ring) Locate(key string) (string, bool) {
	if len(r.points) == 0 {
		return "", false
	}
	h := hashPoint(key)

	for _, p := range r.points {
		if p >= h {
			return r.nodeOf[p], true
		}
	}
	// h is past every point: wrap around to the ring's first point.
	return r.nodeOf[r.points[0]], true
}

// Nodes returns the distinct set of node names currently on the ring, sorted.
func (r *Ring) Nodes() []string {
	seen := make(map[string]bool)
	for _, h := range r.points {
		seen[r.nodeOf[h]] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
```

### The runnable demo

The demo builds a 3-node ring, snapshots where 40 sample keys land, adds a
fourth node, and counts how many of those 40 keys actually moved.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/consistent-hashing-ring-sharding"
)

func main() {
	r := ring.New(9)
	r.AddNode("cache-a")
	r.AddNode("cache-b")
	r.AddNode("cache-c")

	keys := make([]string, 40)
	for i := range keys {
		keys[i] = fmt.Sprintf("session-%d", i)
	}

	before := make(map[string]string, len(keys))
	for _, k := range keys {
		node, _ := r.Locate(k)
		before[k] = node
	}

	r.AddNode("cache-d")

	moved := 0
	for _, k := range keys {
		node, _ := r.Locate(k)
		if node != before[k] {
			moved++
		}
	}

	fmt.Printf("nodes=%v\n", r.Nodes())
	fmt.Printf("keys=%d moved_after_add=%d\n", len(keys), moved)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
nodes=[cache-a cache-b cache-c cache-d]
keys=40 moved_after_add=1
```

### Tests

The subtests cover an empty ring (no owner exists yet), that `Locate` is
deterministic for a fixed key, and the invariant that actually matters in
production: after `RemoveNode`, no key resolves to the removed node anymore,
and the number of keys that changed owner equals exactly the number that
were on the removed node before — nothing else moves.

Create `ring_test.go`:

```go
package ring

import (
	"fmt"
	"testing"
)

func TestLocate(t *testing.T) {
	t.Parallel()

	t.Run("empty ring reports no owner", func(t *testing.T) {
		r := New(9)
		if _, ok := r.Locate("anything"); ok {
			t.Fatalf("Locate() ok = true on an empty ring, want false")
		}
	})

	t.Run("locate is deterministic for the same key", func(t *testing.T) {
		r := New(9)
		r.AddNode("cache-a")
		r.AddNode("cache-b")
		r.AddNode("cache-c")

		first, ok := r.Locate("session-42")
		if !ok {
			t.Fatalf("Locate() ok = false, want true")
		}
		second, _ := r.Locate("session-42")
		if first != second {
			t.Fatalf("Locate() = %q then %q, want the same node both times", first, second)
		}
	})

	t.Run("removing a node only reassigns keys that were on it", func(t *testing.T) {
		r := New(9)
		r.AddNode("cache-a")
		r.AddNode("cache-b")
		r.AddNode("cache-c")

		keys := make([]string, 200)
		for i := range keys {
			keys[i] = fmt.Sprintf("key-%d", i)
		}

		before := make(map[string]string, len(keys))
		wasOnB := 0
		for _, k := range keys {
			node, _ := r.Locate(k)
			before[k] = node
			if node == "cache-b" {
				wasOnB++
			}
		}

		r.RemoveNode("cache-b")

		reassigned := 0
		for _, k := range keys {
			node, _ := r.Locate(k)
			if node == "cache-b" {
				t.Fatalf("Locate(%q) = cache-b after RemoveNode(cache-b)", k)
			}
			if node != before[k] {
				reassigned++
			}
		}

		if reassigned != wasOnB {
			t.Fatalf("reassigned = %d keys, want exactly %d (the keys that were on cache-b)", reassigned, wasOnB)
		}
	})
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The ring is correct when `Locate` always returns the owner of the first
point clockwise from a key's hash, `AddNode`/`RemoveNode` keep `r.points`
sorted so that range still works, and removing a node never changes the
owner of a key that was not on it. The bug this design specifically avoids
is the plain `hash(key) % N` scheme's blast radius: because ownership is
determined by proximity on a ring instead of by the current node count,
`RemoveNode` moves only the keys the departing node actually owned — a
fixed, small fraction of the keyspace — instead of invalidating the entire
cache the instant N changes.

## Resources

- [Go Specification: For statements (range over slice)](https://go.dev/ref/spec#For_statements)
- [hash/crc32](https://pkg.go.dev/hash/crc32)
- [Karger et al., "Consistent Hashing and Random Trees" (1997)](https://www.cs.princeton.edu/courses/archive/fall09/cos518/papers/chash.pdf) — the original paper this exercise's `Ring` implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-s3-multipart-upload-bufferer.md](25-s3-multipart-upload-bufferer.md) | Next: [27-bloom-filter-false-positive-dedup.md](27-bloom-filter-false-positive-dedup.md)
