# Exercise 26: Consistent hashing ring for shard routing

**Nivel: Intermedio** — validacion rapida (un test corto).

Every cache and sharded database faces the same rebalancing problem: add or
remove a node, and a naive `hash(key) % nodeCount` scheme reassigns nearly
every key, turning a routine capacity change into a cache-wide stampede.
Consistent hashing fixes this by placing many virtual nodes per physical
shard around a ring and routing a key to the first virtual node clockwise
from its hash — adding or removing a shard then only touches the keys that
landed on that shard's own virtual nodes, a small, local fraction of the
keyspace. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
chring/                     independent module: example.com/chring
  go.mod                     go 1.24
  chring.go                   Ring; AddShard, RemoveShard, Route
  cmd/
    demo/
      main.go                runnable demo: 300 keys, remove a shard, count exactly which keys moved
  chring_test.go              table test: empty ring, routing stability, remove touches only its own keys, add only redistributes onto the new shard
```

- Files: `chring.go`, `cmd/demo/main.go`, `chring_test.go`.
- Implement: `Ring.AddShard(shard string, replicas int)` placing `replicas` virtual nodes on the ring with collision-safe salting, `Ring.RemoveShard(shard string)` dropping them, and `Ring.Route(key string) (shard string, ok bool)` finding the first virtual node clockwise from `hash(key)`.
- Test: routing on an empty ring, the same key always routing to the same shard, removing a shard moving only the keys that were on it, and adding a shard redistributing keys only onto the newly added one.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/chring/cmd/demo
cd ~/go-exercises/chring
go mod init example.com/chring
go mod edit -go=1.24
```

### Why placing a virtual node needs a labeled continue, not a bare one

`AddShard` is two loops deep for a reason that has nothing to do with
routing and everything to do with hash collisions. The outer loop walks the
`replicas` virtual nodes to create for this shard. For each one, an inner
retry loop hashes a name built from the shard, the replica index, and a
salt, then scans every existing virtual node on the ring to check whether
that exact hash is already taken — vanishingly unlikely with a 32-bit hash,
but a ring implementation that silently overwrote a colliding neighbor's
entry would corrupt routing for whichever shard lost its virtual node, and
that failure would only ever surface in production, at the worst possible
moment, on the shard whose data quietly became unreachable.

The collision scan is a loop of its own, nested inside the retry loop. The
moment it finds a match, the only correct move is to bump the salt and
re-hash — which means re-entering the *retry* loop, not just continuing the
collision scan. A bare `continue` there would only advance to the next
existing virtual node in the scan, comparing the same (unchanged, still
colliding) candidate hash against the rest of the ring instead of computing
a fresh one. `continue dedupe`, fired from inside the collision-scan loop,
is what actually restarts the hash computation with the bumped salt.

Create `chring.go`:

```go
package chring

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// vnode is one virtual node placed on the ring at hash, owned by shard.
type vnode struct {
	hash  uint32
	shard string
}

// Ring is a consistent-hash ring using virtual nodes: each physical shard
// gets several vnodes scattered around the ring, so adding or removing a
// shard only ever redistributes the keys that fell on that shard's OWN
// vnodes, never a large fraction of the whole keyspace.
type Ring struct {
	vnodes []vnode // kept sorted ascending by hash
}

// NewRing returns an empty ring.
func NewRing() *Ring {
	return &Ring{}
}

// AddShard places replicas virtual nodes for shard on the ring. Two
// different shards' vnodes can legitimately land on the same hash only in
// the astronomically unlikely case of an FNV collision, but the dedupe loop
// below handles it correctly rather than silently overwriting a neighbor's
// vnode.
func (r *Ring) AddShard(shard string, replicas int) {
	for i := range replicas {
		salt := 0
	dedupe:
		for {
			h := hashKey(fmt.Sprintf("%s#%d#%d", shard, i, salt))
			for _, v := range r.vnodes {
				if v.hash == h {
					// Collision with an existing vnode (possibly on another
					// shard): retry with a new salt. A bare continue here
					// would only advance to the next EXISTING vnode in the
					// collision-scan, not restart the hash computation --
					// continue dedupe is what actually retries.
					salt++
					continue dedupe
				}
			}
			r.vnodes = append(r.vnodes, vnode{hash: h, shard: shard})
			break dedupe
		}
	}
	sort.Slice(r.vnodes, func(i, j int) bool { return r.vnodes[i].hash < r.vnodes[j].hash })
}

// RemoveShard drops every virtual node belonging to shard. The keys that
// hashed to those vnodes fall to whichever vnode is next clockwise on the
// ring -- a small, local redistribution, never a full reshuffle.
func (r *Ring) RemoveShard(shard string) {
	kept := r.vnodes[:0]
	for _, v := range r.vnodes {
		if v.shard == shard {
			continue
		}
		kept = append(kept, v)
	}
	r.vnodes = kept
}

// Route returns the shard owning key: the first vnode clockwise from
// hash(key), wrapping around to the ring's first vnode if the hash falls
// past every vnode present.
func (r *Ring) Route(key string) (shard string, ok bool) {
	if len(r.vnodes) == 0 {
		return "", false
	}
	h := hashKey(key)
	for _, v := range r.vnodes {
		if v.hash >= h {
			return v.shard, true
		}
	}
	// Past the last vnode: wrap around to the first one on the ring.
	return r.vnodes[0].shard, true
}

func hashKey(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/chring"
)

func main() {
	r := chring.NewRing()
	r.AddShard("shard-a", 50)
	r.AddShard("shard-b", 50)
	r.AddShard("shard-c", 50)

	keys := make([]string, 300)
	for i := range keys {
		keys[i] = fmt.Sprintf("user:%d", i)
	}

	before := make(map[string]string, len(keys))
	counts := map[string]int{}
	for _, k := range keys {
		shard, _ := r.Route(k)
		before[k] = shard
		counts[shard]++
	}
	fmt.Println("distribution before removing shard-b:", counts)

	r.RemoveShard("shard-b")

	moved := 0
	after := map[string]int{}
	for _, k := range keys {
		shard, _ := r.Route(k)
		after[shard]++
		if shard != before[k] {
			moved++
			if before[k] != "shard-b" {
				fmt.Printf("unexpected move: %s was on %s, not shard-b\n", k, before[k])
			}
		}
	}
	fmt.Println("distribution after removing shard-b: ", after)
	fmt.Println("keys that moved:", moved, "out of", len(keys))
	fmt.Println("keys that were on shard-b:", counts["shard-b"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
distribution before removing shard-b: map[shard-a:64 shard-b:95 shard-c:141]
distribution after removing shard-b:  map[shard-a:102 shard-c:198]
keys that moved: 95 out of 300
keys that were on shard-b: 95
```

Not one "unexpected move" line is printed: every key that moved was, without
exception, a key that had been on `shard-b` — exactly the invariant
consistent hashing exists to provide. `shard-a` and `shard-c` between them
absorb precisely the 95 displaced keys (`64+141+95 = 102+198`), with none of
their own pre-existing keys disturbed.

### Tests

`TestRouteEmptyRing` and `TestRouteIsStableForTheSameKey` pin the basics.
`TestRemoveShardOnlyMovesItsOwnKeys` is the core property test: it snapshots
every key's shard before removal, then asserts that after removing one
shard, every key that moved was previously on that shard, and every key that
was not previously on that shard is completely undisturbed.
`TestAddShardExpandsCapacityWithoutDisturbingOthers` checks the mirror case.

Create `chring_test.go`:

```go
package chring

import "testing"

func TestRouteEmptyRing(t *testing.T) {
	t.Parallel()

	r := NewRing()
	if _, ok := r.Route("anything"); ok {
		t.Fatal("Route on an empty ring should report ok=false")
	}
}

func TestRouteIsStableForTheSameKey(t *testing.T) {
	t.Parallel()

	r := NewRing()
	r.AddShard("a", 10)
	r.AddShard("b", 10)

	first, ok := r.Route("stable-key")
	if !ok {
		t.Fatal("expected a routable shard")
	}
	for range 5 {
		got, ok := r.Route("stable-key")
		if !ok || got != first {
			t.Fatalf("Route(%q) = %q, want stable %q", "stable-key", got, first)
		}
	}
}

func TestRemoveShardOnlyMovesItsOwnKeys(t *testing.T) {
	t.Parallel()

	r := NewRing()
	r.AddShard("a", 30)
	r.AddShard("b", 30)
	r.AddShard("c", 30)

	keys := make([]string, 200)
	before := make(map[string]string, len(keys))
	for i := range keys {
		keys[i] = string(rune('a'+i%26)) + string(rune('0'+i%10)) + "-key"
		shard, ok := r.Route(keys[i])
		if !ok {
			t.Fatalf("Route(%q) unexpectedly failed", keys[i])
		}
		before[keys[i]] = shard
	}

	r.RemoveShard("b")

	for _, k := range keys {
		after, ok := r.Route(k)
		if !ok {
			t.Fatalf("Route(%q) unexpectedly failed after removal", k)
		}
		if before[k] == "b" {
			if after == "b" {
				t.Fatalf("key %q was on removed shard b but still routes to b", k)
			}
			continue
		}
		if after != before[k] {
			t.Fatalf("key %q was on shard %q (not removed) but moved to %q", k, before[k], after)
		}
	}
}

func TestAddShardExpandsCapacityWithoutDisturbingOthers(t *testing.T) {
	t.Parallel()

	r := NewRing()
	r.AddShard("a", 20)
	r.AddShard("b", 20)

	keys := make([]string, 100)
	before := make(map[string]string, len(keys))
	for i := range keys {
		keys[i] = string(rune('a'+i%26)) + "-item"
		shard, _ := r.Route(keys[i])
		before[keys[i]] = shard
	}

	r.AddShard("c", 20)

	moved := 0
	for _, k := range keys {
		after, _ := r.Route(k)
		if after != before[k] {
			moved++
			if after != "c" {
				t.Fatalf("key %q moved to %q, want it to move only to the newly added shard c (or stay put)", k, after)
			}
		}
	}
	if moved == 0 {
		t.Fatal("expected at least some keys to redistribute onto the newly added shard")
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The ring is correct when removing a shard moves *only* the keys that were on
it, and adding a shard redistributes keys *only* onto the newly added one —
`TestRemoveShardOnlyMovesItsOwnKeys` fails loudly the moment a key that
should have been untouched changes shards, which is exactly what a
`% nodeCount` scheme would do to nearly every key. The collision-handling
detail is the one worth re-reading: `continue dedupe`, not a bare
`continue`, is what actually re-hashes with a bumped salt instead of
comparing a stale candidate against the rest of the ring. Increasing
`replicas` reduces load-distribution variance across shards (more virtual
nodes per shard means the law of large numbers smooths out the ring's
randomness) at the cost of more entries to keep sorted.

## Resources

- [Consistent hashing and random trees (Karger et al., 1997)](https://www.akamai.com/site/en/documents/research-paper/consistent-hashing-and-random-trees-distributed-caching-protocols-for-relieving-hot-spots-on-the-world-wide-web-technical-publication.pdf) — the original paper introducing the technique this ring implements.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the hash function used to place vnodes on the ring.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-sliding-window-rate-limiter-with-cleanup.md](25-sliding-window-rate-limiter-with-cleanup.md) | Next: [27-wal-compaction-grooming.md](27-wal-compaction-grooming.md)
