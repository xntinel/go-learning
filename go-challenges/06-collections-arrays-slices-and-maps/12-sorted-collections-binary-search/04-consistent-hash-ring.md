# Exercise 4: A Consistent-Hash Ring for Shard/Node Selection

To spread cache keys across a set of backend nodes so that adding or removing a
node moves as few keys as possible, you use a consistent-hash ring: a sorted
slice of hash points on a circle, each point owned by a node. `Get(key)` hashes
the key and walks clockwise to the first ring point at or past that hash — and
when the hash lands past the largest point, it *wraps* to the front. This is the
one place where raw `sort.Search` reads more naturally than `slices.BinarySearch`,
because the wraparound is exactly "search returned `len`".

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
hashring/                    independent module: example.com/hashring
  go.mod
  ring.go                    type Ring; New, Add, Remove, Get; virtual nodes
  cmd/
    demo/
      main.go                three nodes, key placement, distribution
  ring_test.go               fixed placement, wraparound, balance, removal locality
```

Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
Implement: `Ring` with `New`, `Add(node)`, `Remove(node)`, `Get(key) (string, bool)`, using virtual nodes and `sort.Search` with wraparound.
Test: a fixed key maps to a known node, wraparound past the max point, every node gets a nonzero share, and removing a node only redistributes its own keys.
Verify: `go test -count=1 -race ./...`

### Virtual nodes, the circle, and the wraparound

Each physical node is placed at many points on the ring — *virtual nodes* — so
its share of the key space is spread out rather than one big contiguous arc; that
is what keeps the distribution even and keeps a node removal from dumping all its
keys onto a single neighbor. `Add(node)` hashes `node#0, node#1, … node#(R-1)`
with FNV-1a to get `R` points, records each point's owner, and re-sorts the point
slice. `Remove(node)` drops every point owned by that node and its owner entries.

`Get(key)` is the ring walk. Hash the key to `h`, then find the first ring point
`>= h`:

```go
i := sort.Search(len(points), func(i int) bool { return points[i] >= h })
```

If `h` is larger than every point, `sort.Search` returns `len(points)` — and on a
*circle* the next point clockwise from the end is the first point, index 0. So the
wraparound is a single line: `if i == len(points) { i = 0 }`. This is the idiom
the concepts file calls out: `slices.BinarySearch`'s `(pos, found)` result does
not express "wrap to the front", but `sort.Search` returning `len` does it
cleanly. Getting this wrong — treating `len` as an out-of-range miss — would leave
the arc between the largest point and the smallest point unowned, and every key
that hashes into that arc would fail to place.

The hash is FNV-1a via `hash/fnv`, which is deterministic and stable, so the same
key always lands on the same node for a given membership. That determinism is
what lets the tests assert a concrete key-to-node mapping.

Create `ring.go`:

```go
// ring.go
package hashring

import (
	"hash/fnv"
	"slices"
	"sort"
	"strconv"
)

const replicas = 50

// Ring is a consistent-hash ring over backend node names.
type Ring struct {
	points []uint32
	owner  map[uint32]string
}

func New() *Ring {
	return &Ring{owner: make(map[uint32]string)}
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// Add places node at `replicas` virtual points, then re-sorts.
func (r *Ring) Add(node string) {
	for i := range replicas {
		p := hash32(node + "#" + strconv.Itoa(i))
		if _, dup := r.owner[p]; dup {
			continue
		}
		r.owner[p] = node
		r.points = append(r.points, p)
	}
	slices.Sort(r.points)
}

// Remove deletes every point owned by node.
func (r *Ring) Remove(node string) {
	kept := r.points[:0:0]
	for _, p := range r.points {
		if r.owner[p] == node {
			delete(r.owner, p)
			continue
		}
		kept = append(kept, p)
	}
	r.points = kept
}

// find returns the owning node for hash h, wrapping around the circle.
func (r *Ring) find(h uint32) string {
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i] >= h })
	if i == len(r.points) {
		i = 0
	}
	return r.owner[r.points[i]]
}

// Get returns the node owning key, or ("", false) if the ring is empty.
func (r *Ring) Get(key string) (string, bool) {
	if len(r.points) == 0 {
		return "", false
	}
	return r.find(hash32(key)), true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strconv"

	"example.com/hashring"
)

func main() {
	r := hashring.New()
	for _, n := range []string{"cache-a", "cache-b", "cache-c"} {
		r.Add(n)
	}

	for _, k := range []string{"user:1001", "user:2002", "session:abcdef", "cart:42"} {
		owner, _ := r.Get(k)
		fmt.Printf("%s -> %s\n", k, owner)
	}

	counts := map[string]int{}
	for i := range 3000 {
		owner, _ := r.Get("key:" + strconv.Itoa(i))
		counts[owner]++
	}
	fmt.Printf("distribution over 3000 keys: a=%d b=%d c=%d\n",
		counts["cache-a"], counts["cache-b"], counts["cache-c"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:1001 -> cache-a
user:2002 -> cache-c
session:abcdef -> cache-c
cart:42 -> cache-a
distribution over 3000 keys: a=1658 b=461 c=881
```

### Tests

`TestFixedPlacement` pins a known key-to-node mapping (FNV is deterministic).
`TestWraparound` reaches through the same-package seam `find` to confirm a hash
one past the maximum point wraps to the owner of `points[0]`. `TestBalance`
distributes many keys and asserts every node gets a nonzero share.
`TestRemovalLocality` records owners, removes a node, and asserts every key *not*
previously owned by the removed node keeps its owner — the consistent-hashing
guarantee.

Create `ring_test.go`:

```go
package hashring

import (
	"fmt"
	"strconv"
	"testing"
)

func threeNodeRing() *Ring {
	r := New()
	for _, n := range []string{"cache-a", "cache-b", "cache-c"} {
		r.Add(n)
	}
	return r
}

func TestEmptyRing(t *testing.T) {
	t.Parallel()
	if _, ok := New().Get("anything"); ok {
		t.Fatal("Get on empty ring should return ok=false")
	}
}

func TestFixedPlacement(t *testing.T) {
	t.Parallel()
	r := threeNodeRing()
	got, ok := r.Get("user:1001")
	if !ok || got != "cache-a" {
		t.Fatalf("Get(user:1001) = %q,%v; want cache-a,true", got, ok)
	}
}

func TestWraparound(t *testing.T) {
	t.Parallel()
	r := threeNodeRing()
	max := r.points[len(r.points)-1]
	want := r.owner[r.points[0]]
	if got := r.find(max + 1); got != want {
		t.Fatalf("find(max+1) = %q, want wraparound owner %q", got, want)
	}
}

func TestBalance(t *testing.T) {
	t.Parallel()
	r := threeNodeRing()
	counts := map[string]int{}
	for i := range 3000 {
		owner, _ := r.Get("key:" + strconv.Itoa(i))
		counts[owner]++
	}
	for _, n := range []string{"cache-a", "cache-b", "cache-c"} {
		if counts[n] == 0 {
			t.Fatalf("node %s received no keys; distribution %v", n, counts)
		}
	}
}

func TestRemovalLocality(t *testing.T) {
	t.Parallel()
	r := threeNodeRing()

	keys := make([]string, 2000)
	before := make(map[string]string, len(keys))
	for i := range keys {
		keys[i] = "key:" + strconv.Itoa(i)
		before[keys[i]], _ = r.Get(keys[i])
	}

	r.Remove("cache-b")

	for _, k := range keys {
		after, ok := r.Get(k)
		if !ok {
			t.Fatalf("key %s lost its owner after removal", k)
		}
		if before[k] != "cache-b" && after != before[k] {
			t.Fatalf("key %s owned by %s moved to %s despite cache-b removal",
				k, before[k], after)
		}
	}
}

func Example() {
	r := New()
	r.Add("cache-a")
	r.Add("cache-b")
	r.Add("cache-c")
	owner, _ := r.Get("user:1001")
	fmt.Println(owner)
	// Output: cache-a
}
```

## Review

The ring is correct when placement is deterministic, the wraparound owns the arc
past the last point, and removing a node moves only that node's keys. The
wraparound test is the crux: delete the `if i == len(points) { i = 0 }` line and
`TestWraparound` fails immediately, because keys hashing past the maximum point
would otherwise index out of range or be dropped. `TestRemovalLocality` is the
property that justifies consistent hashing over a plain `hash(key) % N`, which
would remap almost every key when `N` changes. Run `go test -race`.

## Resources

- [`sort.Search`](https://pkg.go.dev/sort#Search) — returns `len` when nothing matches, which drives the wraparound.
- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — the deterministic non-cryptographic hash used for ring points.
- [`slices.Sort`](https://pkg.go.dev/slices#Sort) — keeps the point slice ordered after each `Add`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-ip-range-lookup.md](05-ip-range-lookup.md)
