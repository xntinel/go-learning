# Exercise 31: Consistent Hash Ring Loses Keys During Rebalance

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed cache sharded with consistent hashing survives a node
leaving without reshuffling every key, because each key only ever
depends on its own position on the ring relative to the *nearest*
virtual node clockwise from it — removing one node should only affect
the keys that specific node owned, migrating each to its true
successor. The subtlety is entirely in how "true successor" gets
computed for potentially thousands of orphaned keys at once: the
mathematically simple description — walk the ring clockwise from each
key's hash — is easy to break by trying to make the migration loop
"smarter" than a fresh lookup per key, especially by assuming the keys
being migrated are visited in some convenient order. Go's map
iteration order is randomized by design, and a migration loop that
silently depends on ascending order gets it wrong for exactly the keys
sitting at the boundary between the node that left and its neighbor —
the ones a real production incident would show up as "some sessions
routed to a node that never should have owned them, right after a
scale-down." This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
ring/                         independent module: example.com/consistent-hash-ring-rebalance
  go.mod                       go 1.21
  ring.go                       Ring, New, Join, Owner, Put, Leave
  cmd/
    demo/
      main.go                    runnable demo: 3 nodes, 8 keys, one node leaves
  ring_test.go                    reference-ring cross-check, unaffected-keys boundary, wrap-around edge case
```

- Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
- Implement: `Ring.Leave(node string) map[string]string` that removes a node's virtual positions and reassigns every key it owned to its true clockwise successor on the resulting ring.
- Test: a 200-key migration cross-checked against an independently built reference ring; a boundary case asserting keys never owned by the leaving node are untouched; a wrap-around edge case for a single-node ring.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/31-consistent-hash-ring-rebalance/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/31-consistent-hash-ring-rebalance
```

### Why a shared cursor breaks the moment map order isn't ascending

The version that looks like a clever optimization walks the ring
*once* while migrating every orphaned key, advancing a single shared
index instead of doing an independent lookup per key:

```go
// BUG: assumes keys are visited in ascending hash order so a single
// cursor can walk the ring once, but Go's map iteration order is
// randomized -- the cursor can already be past a key's true successor by
// the time that key is processed.
func (r *Ring) Leave(node string) map[string]string {
	r.removeVnodes(node)
	migrated := make(map[string]string)
	cursor := 0
	for key, owner := range r.keys {
		if owner != node {
			continue
		}
		for cursor < len(r.nodes) && r.nodes[cursor].pos < hash(key) {
			cursor++
		}
		newOwner := r.nodes[cursor%len(r.nodes)].owner
		r.keys[key] = newOwner
		migrated[key] = newOwner
	}
	return migrated
}
```

This is correct exactly as long as `key`'s hash positions arrive at
the loop in ascending order, because then the cursor only ever needs
to move forward, never back — and it will pass every table-driven test
that happens to construct its fixture keys in hash order, or that only
migrates one or two keys where a coincidental ordering hides the bug.
It fails the moment two orphaned keys are visited out of hash order,
which Go's `for key := range map` makes no promise about and actively
randomizes across runs. Say key `A` hashes further around the ring
than key `B`, but the map iterates `A` first: the cursor advances past
`B`'s true successor while searching for `A`'s. When `B` is processed
next, the inner `for` loop's condition `r.nodes[cursor].pos <
hash(B)` is already false — the cursor never moves back to look — so
`B` gets assigned whatever node the cursor currently sits on, which is
`A`'s successor, not `B`'s own. The failure is silent: no error, no
panic, just a key quietly routed to the wrong shard, exactly at the
boundary between the departed node and its true neighbor — which is
precisely where it is hardest to notice in a spot check, because most
keys still land correctly.

The fix gives up the "walk the ring once" optimization and looks up
each orphaned key's true successor independently, via `Owner`, which
performs a fresh binary search relative to that specific key's hash
position every time:

```go
for key, owner := range r.keys {
	if owner != node {
		continue
	}
	newOwner := r.Owner(key) // independent ring walk, correct regardless of visit order
	r.keys[key] = newOwner
	migrated[key] = newOwner
}
```

Every key's new owner now depends only on that key's own hash position
and the current (post-removal) ring — never on which other keys were
migrated before it, or in what order the map happened to iterate.

Create `ring.go`:

```go
package ring

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"sort"
)

type vnode struct {
	pos   uint32
	owner string
}

// Ring is a consistent-hash ring with a configurable number of virtual
// nodes per physical node, used to spread a leaving node's keys across
// many different clockwise successors instead of dumping them all onto a
// single neighbor.
type Ring struct {
	vnodesPerNode int
	nodes         []vnode           // kept sorted ascending by pos
	keys          map[string]string // key -> current owner node
}

// New creates an empty Ring using vnodesPerNode virtual positions per
// physical node joined later.
func New(vnodesPerNode int) *Ring {
	return &Ring{vnodesPerNode: vnodesPerNode, keys: make(map[string]string)}
}

func hash(s string) uint32 {
	sum := sha1.Sum([]byte(s))
	return binary.BigEndian.Uint32(sum[:4])
}

// Join adds node to the ring with vnodesPerNode virtual positions.
func (r *Ring) Join(node string) {
	for i := 0; i < r.vnodesPerNode; i++ {
		pos := hash(fmt.Sprintf("%s#%d", node, i))
		r.nodes = append(r.nodes, vnode{pos: pos, owner: node})
	}
	sort.Slice(r.nodes, func(i, j int) bool { return r.nodes[i].pos < r.nodes[j].pos })
}

// Owner returns the node owning key: the first virtual node clockwise
// from key's hash position, wrapping around to the first virtual node on
// the ring if key's hash is past every position. This is a fresh,
// independent ring walk every time it is called -- it never depends on
// what any other key's lookup happened to find.
func (r *Ring) Owner(key string) string {
	h := hash(key)
	i := sort.Search(len(r.nodes), func(i int) bool { return r.nodes[i].pos >= h })
	if i == len(r.nodes) {
		i = 0
	}
	return r.nodes[i].owner
}

// Put assigns key to its current ring owner and remembers the assignment
// so a later Leave can find every key that needs to migrate.
func (r *Ring) Put(key string) string {
	owner := r.Owner(key)
	r.keys[key] = owner
	return owner
}

// Leave removes node's virtual positions from the ring and reassigns
// every key it owned to its new clockwise successor, returning the
// key->newOwner map of everything that moved. Each key's new owner is
// found via its own independent Owner lookup against the post-removal
// ring, in that key's own hash order -- not via a single shared cursor
// advanced once across keys visited in arbitrary map iteration order.
func (r *Ring) Leave(node string) map[string]string {
	kept := make([]vnode, 0, len(r.nodes))
	for _, vn := range r.nodes {
		if vn.owner != node {
			kept = append(kept, vn)
		}
	}
	r.nodes = kept

	migrated := make(map[string]string)
	for key, owner := range r.keys {
		if owner != node {
			continue
		}
		newOwner := r.Owner(key)
		r.keys[key] = newOwner
		migrated[key] = newOwner
	}
	return migrated
}
```

### The runnable demo

Three nodes, four virtual positions each, eight keys placed onto the
ring, then node `b` leaves — the demo prints the full before-and-after
placement so the migrated keys' new owners are visible directly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/consistent-hash-ring-rebalance"
)

func main() {
	r := ring.New(4)
	r.Join("a")
	r.Join("b")
	r.Join("c")

	keys := []string{"cart-1", "cart-2", "cart-3", "cart-4", "cart-5", "cart-6", "cart-7", "cart-8"}
	fmt.Println("initial placement:")
	for _, k := range keys {
		fmt.Printf("  %s -> %s\n", k, r.Put(k))
	}

	migrated := r.Leave("b")

	fmt.Println("node b left; migrated keys:")
	names := make([]string, 0, len(migrated))
	for k := range migrated {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Printf("  %s -> %s\n", k, migrated[k])
	}

	fmt.Println("final placement:")
	for _, k := range keys {
		fmt.Printf("  %s -> %s\n", k, r.Owner(k))
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
initial placement:
  cart-1 -> c
  cart-2 -> a
  cart-3 -> b
  cart-4 -> c
  cart-5 -> c
  cart-6 -> a
  cart-7 -> b
  cart-8 -> a
node b left; migrated keys:
  cart-3 -> c
  cart-7 -> a
final placement:
  cart-1 -> c
  cart-2 -> a
  cart-3 -> c
  cart-4 -> c
  cart-5 -> c
  cart-6 -> a
  cart-7 -> a
  cart-8 -> a
```

### Tests

`TestLeaveMigratesEveryKeyToItsTrueClockwiseSuccessor` is the core
case: 200 keys placed on a 3-node ring, one node removed, and every
migrated key's new owner cross-checked against an independently built
reference ring containing only the surviving nodes — this is the test
shape that actually catches an out-of-order cursor bug, since with 200
keys arriving in Go's randomized map order, at least one is virtually
guaranteed to expose it. `TestLeaveDoesNotTouchKeysOwnedByOtherNodes`
is the boundary case: keys that were never owned by the leaving node
must keep their exact original owner. `TestOwnerWrapsAroundTheRing` is
the wrap-around edge case for a single-node ring, where every key's
hash falls "before" the one node's positions in ring order at least
once per lap.

Create `ring_test.go`:

```go
package ring

import (
	"fmt"
	"testing"
)

// TestLeaveMigratesEveryKeyToItsTrueClockwiseSuccessor is the core case: it
// puts 200 keys onto a 3-node ring, removes one node, and checks that every
// migrated key's new owner matches what an independently built reference
// ring (containing only the surviving nodes) computes for that same key.
// A cursor-based migration that assumes keys arrive in ascending hash
// order -- which Go's randomized map iteration does not guarantee -- would
// misassign at least one of these 200 keys; comparing against a fully
// independent reference ring catches that regardless of which key it is.
func TestLeaveMigratesEveryKeyToItsTrueClockwiseSuccessor(t *testing.T) {
	r := New(4)
	r.Join("a")
	r.Join("b")
	r.Join("c")

	keys := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("session-%d", i)
		r.Put(k)
		keys = append(keys, k)
	}

	migrated := r.Leave("b")
	if len(migrated) == 0 {
		t.Fatal("Leave migrated 0 keys, want at least one key previously owned by b")
	}

	want := New(4)
	want.Join("a")
	want.Join("c")

	for key, gotOwner := range migrated {
		wantOwner := want.Owner(key)
		if gotOwner != wantOwner {
			t.Fatalf("key %q migrated to %q, want its true clockwise successor %q", key, gotOwner, wantOwner)
		}
	}

	for _, key := range keys {
		if owner := r.Owner(key); owner == "b" {
			t.Fatalf("key %q still resolves to removed node b", key)
		}
	}
}

// TestLeaveDoesNotTouchKeysOwnedByOtherNodes is the boundary case: keys
// that were never owned by the leaving node must keep their original
// owner untouched -- Leave must not accidentally reassign the whole ring.
func TestLeaveDoesNotTouchKeysOwnedByOtherNodes(t *testing.T) {
	r := New(4)
	r.Join("a")
	r.Join("b")
	r.Join("c")

	before := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("session-%d", i)
		before[k] = r.Put(k)
	}

	r.Leave("b")

	for key, ownerBefore := range before {
		if ownerBefore == "b" {
			continue // this key was supposed to migrate
		}
		if got := r.Owner(key); got != ownerBefore {
			t.Fatalf("key %q owned by %q before Leave now resolves to %q, want unchanged", key, ownerBefore, got)
		}
	}
}

// TestOwnerWrapsAroundTheRing is the wrap-around edge case: a key whose
// hash falls past every virtual node's position must resolve to the
// first virtual node on the ring, not panic or resolve to nothing.
func TestOwnerWrapsAroundTheRing(t *testing.T) {
	r := New(8)
	r.Join("only-node")

	for i := 0; i < 50; i++ {
		owner := r.Owner(fmt.Sprintf("key-%d", i))
		if owner != "only-node" {
			t.Fatalf("Owner(key-%d) = %q, want only-node", i, owner)
		}
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`Leave` is correct when every key it reassigns lands on the same node
an independent, from-scratch ring lookup would compute for that key —
proven by cross-checking against a reference ring built from only the
surviving nodes, not by trusting the migration loop's own bookkeeping.
The mistake this design avoids is optimizing a per-key operation into
a single shared pass by assuming an iteration order that Go's map
explicitly does not guarantee: a cursor that only ever advances is
correct for ascending input and silently wrong for anything else, and
"anything else" is exactly what a `for range map` produces. The fix
gives up the appearance of an O(migrated keys) single ring walk in
favor of `sort.Search`'s O(log n) independent lookup per key — slightly
more total work, but each key's correctness no longer depends on every
other key's visit order, which is the property an operation billed as
"safe to run per key, in any order" actually has to hold.

## Resources

- [Karger et al., "Consistent Hashing and Random Trees"](https://www.akamai.com/site/en/documents/technical-publication/consistent-hashing-and-random-trees-distributed-caching-protocols-for-relieving-hot-spots-on-the-world-wide-web-technical-publication.pdf) — the original consistent-hashing paper and its virtual-node (replication) technique.
- [sort.Search](https://pkg.go.dev/sort#Search) — binary search over the sorted virtual-node slice used by `Owner`.
- [Go Specification: For statements, range clause](https://go.dev/ref/spec#For_range) — map iteration order is unspecified and randomized across runs.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-lease-renewal-expiry-off-by-one.md](30-lease-renewal-expiry-off-by-one.md) | Next: [32-database-conn-pool-multiplexing.md](32-database-conn-pool-multiplexing.md)
