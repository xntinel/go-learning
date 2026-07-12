# Exercise 31: Consistent Hashing: Route Requests to Partitions with Minimal Redistribution

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Naive hash-mod-N partitioning (`hash(key) % numPartitions`) reroutes almost
every key the moment a partition is added or removed, because the modulus
itself changes: a cache cluster or a sharded data store using that scheme
turns any capacity change into a mass cache-miss stampede or a full data
reshuffle. Consistent hashing fixes this by placing partitions and keys on
the same fixed-size ring: a key always routes to the next partition
clockwise from its own position, so adding or removing one partition only
ever affects the keys that land in the arc that partition owned — everyone
else's nearest neighbor on the ring never changes. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
consistenthash/              independent module: example.com/consistent-hashing-partition-routing
  go.mod                    go 1.24
  ring.go                   Ring (atomic.Pointer-backed membership), NewRing, Rebuild, Route, routeInRing
  cmd/
    demo/
      main.go               40 keys routed, then a partition removed and only its keys move
  ring_test.go              routeInRing table incl. wrap-around; empty ring; minimal redistribution; -race
```

- Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
- Implement: a `Ring` struct whose sorted membership lives behind `atomic.Pointer[[]member]`, with `NewRing(partitions []string, replicas int) *Ring`, `Rebuild(partitions []string)` recomputing and atomically swapping the membership, and `Route(key string) (string, error)` hashing the key and delegating to a pure `routeInRing(members []member, h uint32) (string, error)` that binary-searches for the next member clockwise and wraps around at the end of the ring.
- Test: a table over `routeInRing` with hand-picked hashes covering an exact match, a value between two members, a value below the lowest member, and a value above the highest member (wrap-around); an empty-ring error; a property test proving that removing one partition and rebuilding only moves the keys that were previously routed to it; a concurrency test hammering `Route` while `Rebuild` runs repeatedly, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/31-consistent-hashing-partition-routing/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/31-consistent-hashing-partition-routing
go mod edit -go=1.24
```

### Why the ring search is split into a pure function over hand-picked hashes

`Route` computes a real hash from a real key and looks it up in the live
ring, but the actual branching logic worth testing exhaustively — exact
match, between two members, below everything, and the wrap-around past the
last member — lives entirely in `routeInRing`, which takes an
already-sorted `[]member` and a target hash as plain values. That split
matters because reasoning about wrap-around from real key hashes would mean
first reverse-engineering which of a large set of candidate strings happens
to hash above every member's position, an exercise in guessing rather than
testing. With `routeInRing` pure, the table test simply invents members at
hashes 10, 20, and 30 and asks for the answer at 35 — the wrap-around
condition is exercised directly and unambiguously, with no dependency on
`hash/fnv`'s actual output for any particular string. The same split is why
membership lives behind `atomic.Pointer`: `Rebuild` computes an entirely new,
fully-sorted slice off to the side and only *then* swaps the pointer, so a
`Route` call running concurrently with a `Rebuild` always sees one complete,
consistent membership — the whole old one or the whole new one — never a
half-sorted or half-populated slice torn between the two.

Create `ring.go`:

```go
// Package consistenthash routes keys to partitions on a hash ring: the same
// key always routes to the same partition, and adding or removing a
// partition only redistributes roughly 1/n of all keys instead of reshuffling
// everything, because every other key's nearest ring neighbor never changes.
package consistenthash

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync/atomic"
)

// member is one point on the ring: a virtual-node hash and the partition it
// belongs to. Several members share the same partition (replicas per
// partition) so the ring divides more evenly than one point per partition
// would.
type member struct {
	hash      uint32
	partition string
}

// hashKey hashes an arbitrary string into a ring position.
func hashKey(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return h.Sum32()
}

// Ring routes keys to partitions. The membership is stored behind an atomic
// pointer: Route never blocks on a lock, and Rebuild swaps in a whole new,
// fully-sorted membership in one indivisible step, so a Route call in flight
// during a Rebuild sees either the entire old ring or the entire new one,
// never a half-updated one.
type Ring struct {
	members  atomic.Pointer[[]member]
	replicas int
}

// NewRing builds a Ring over partitions, with replicas virtual nodes per
// partition. More replicas spread load more evenly across a small partition
// count, at the cost of a larger membership slice.
func NewRing(partitions []string, replicas int) *Ring {
	r := &Ring{replicas: replicas}
	r.Rebuild(partitions)
	return r
}

// Rebuild recomputes the ring's membership from scratch and swaps it in
// atomically. Call this whenever a partition is added or removed.
func (r *Ring) Rebuild(partitions []string) {
	members := make([]member, 0, len(partitions)*r.replicas)
	for _, p := range partitions {
		for i := 0; i < r.replicas; i++ {
			key := fmt.Sprintf("%s#%d", p, i)
			members = append(members, member{hash: hashKey(key), partition: p})
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].hash < members[j].hash })
	r.members.Store(&members)
}

// Route reports which partition owns key: the partition of the first member
// whose hash is greater than or equal to key's hash, walking clockwise from
// key's position. If key's hash is greater than every member's hash, the
// search wraps around to the first member on the ring.
func (r *Ring) Route(key string) (string, error) {
	members := *r.members.Load()
	if len(members) == 0 {
		return "", errors.New("consistenthash: ring is empty")
	}

	h := hashKey(key)
	return routeInRing(members, h)
}

// routeInRing is the pure guard behind Route: given an already-sorted
// membership and a target hash, decide which partition owns it. Separating
// this from Route means the ring-search logic — including the wrap-around
// case — is table-testable with hand-picked hash values instead of needing
// to reverse-engineer which real keys land where on a live hash ring.
func routeInRing(members []member, h uint32) (string, error) {
	if len(members) == 0 {
		return "", errors.New("consistenthash: ring is empty")
	}

	idx := sort.Search(len(members), func(i int) bool { return members[i].hash >= h })
	if idx == len(members) {
		idx = 0 // wrap around: h is past the last member, so the first member owns it
	}
	return members[idx].partition, nil
}
```

### The runnable demo

Forty keys are routed across four partitions with two hundred virtual nodes
each for even spread. One partition is then removed and the ring rebuilt:
only the keys that were previously routed to the removed partition change
partition — everyone else's assignment is untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	consistenthash "example.com/consistent-hashing-partition-routing"
)

func main() {
	partitions := []string{"partition-0", "partition-1", "partition-2", "partition-3"}
	ring := consistenthash.NewRing(partitions, 200)

	keys := make([]string, 40)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%03d", i)
	}

	before := make(map[string]string, len(keys))
	fmt.Println("routing with 4 partitions:")
	for _, k := range keys {
		p, err := ring.Route(k)
		if err != nil {
			fmt.Println("route error:", err)
			return
		}
		before[k] = p
		fmt.Printf("  %-10s -> %s\n", k, p)
	}

	// Remove one partition and rebuild; only keys that were routed to the
	// removed partition should move.
	ring.Rebuild([]string{"partition-0", "partition-1", "partition-3"})

	moved := 0
	fmt.Println("routing with partition-2 removed:")
	for _, k := range keys {
		p, err := ring.Route(k)
		if err != nil {
			fmt.Println("route error:", err)
			return
		}
		changed := p != before[k]
		if changed {
			moved++
		}
		fmt.Printf("  %-10s -> %-12s changed=%v\n", k, p, changed)
	}
	fmt.Printf("keys redistributed: %d/%d\n", moved, len(keys))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
routing with 4 partitions:
  key-000    -> partition-1
  key-001    -> partition-1
  key-002    -> partition-1
  key-003    -> partition-1
  key-004    -> partition-1
  key-005    -> partition-1
  key-006    -> partition-1
  key-007    -> partition-1
  key-008    -> partition-0
  key-009    -> partition-0
  key-010    -> partition-0
  key-011    -> partition-3
  key-012    -> partition-3
  key-013    -> partition-3
  key-014    -> partition-3
  key-015    -> partition-3
  key-016    -> partition-3
  key-017    -> partition-3
  key-018    -> partition-0
  key-019    -> partition-0
  key-020    -> partition-2
  key-021    -> partition-2
  key-022    -> partition-2
  key-023    -> partition-2
  key-024    -> partition-2
  key-025    -> partition-2
  key-026    -> partition-0
  key-027    -> partition-2
  key-028    -> partition-0
  key-029    -> partition-0
  key-030    -> partition-1
  key-031    -> partition-1
  key-032    -> partition-1
  key-033    -> partition-1
  key-034    -> partition-1
  key-035    -> partition-1
  key-036    -> partition-1
  key-037    -> partition-1
  key-038    -> partition-1
  key-039    -> partition-1
routing with partition-2 removed:
  key-000    -> partition-1  changed=false
  key-001    -> partition-1  changed=false
  key-002    -> partition-1  changed=false
  key-003    -> partition-1  changed=false
  key-004    -> partition-1  changed=false
  key-005    -> partition-1  changed=false
  key-006    -> partition-1  changed=false
  key-007    -> partition-1  changed=false
  key-008    -> partition-0  changed=false
  key-009    -> partition-0  changed=false
  key-010    -> partition-0  changed=false
  key-011    -> partition-3  changed=false
  key-012    -> partition-3  changed=false
  key-013    -> partition-3  changed=false
  key-014    -> partition-3  changed=false
  key-015    -> partition-3  changed=false
  key-016    -> partition-3  changed=false
  key-017    -> partition-3  changed=false
  key-018    -> partition-0  changed=false
  key-019    -> partition-0  changed=false
  key-020    -> partition-0  changed=true
  key-021    -> partition-0  changed=true
  key-022    -> partition-0  changed=true
  key-023    -> partition-0  changed=true
  key-024    -> partition-0  changed=true
  key-025    -> partition-0  changed=true
  key-026    -> partition-0  changed=false
  key-027    -> partition-0  changed=true
  key-028    -> partition-0  changed=false
  key-029    -> partition-0  changed=false
  key-030    -> partition-1  changed=false
  key-031    -> partition-1  changed=false
  key-032    -> partition-1  changed=false
  key-033    -> partition-1  changed=false
  key-034    -> partition-1  changed=false
  key-035    -> partition-1  changed=false
  key-036    -> partition-1  changed=false
  key-037    -> partition-1  changed=false
  key-038    -> partition-1  changed=false
  key-039    -> partition-1  changed=false
keys redistributed: 7/40
```

### Tests

The `routeInRing` table drives the ring-search logic directly with
hand-picked hashes, including the wrap-around case. A property test builds a
real ring, records every key's partition, removes one partition, rebuilds,
and asserts that every key not previously on the removed partition kept its
assignment — the actual guarantee consistent hashing exists to provide. A
concurrency test hammers `Route` from many goroutines while `Rebuild` runs
repeatedly, under `-race`.

Create `ring_test.go`:

```go
package consistenthash

import (
	"fmt"
	"sync"
	"testing"
)

func TestRouteInRing(t *testing.T) {
	t.Parallel()

	members := []member{
		{hash: 10, partition: "a"},
		{hash: 20, partition: "b"},
		{hash: 30, partition: "c"},
	}

	tests := []struct {
		name string
		h    uint32
		want string
	}{
		{name: "exact match on a member's own hash", h: 20, want: "b"},
		{name: "between two members takes the next clockwise", h: 15, want: "b"},
		{name: "below the lowest member takes the lowest", h: 5, want: "a"},
		{name: "above the highest member wraps to the first", h: 35, want: "a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := routeInRing(members, tc.h)
			if err != nil {
				t.Fatalf("routeInRing(%d) returned error: %v", tc.h, err)
			}
			if got != tc.want {
				t.Errorf("routeInRing(%d) = %q, want %q", tc.h, got, tc.want)
			}
		})
	}
}

func TestRouteInRingEmptyRing(t *testing.T) {
	t.Parallel()

	if _, err := routeInRing(nil, 42); err == nil {
		t.Fatal("routeInRing on an empty ring = nil error, want an error")
	}
}

func TestRingRouteIsDeterministic(t *testing.T) {
	t.Parallel()

	ring := NewRing([]string{"p0", "p1", "p2"}, 50)
	first, err := ring.Route("some-key")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := ring.Route("some-key")
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if got != first {
			t.Fatalf("Route(%q) = %q on call %d, want the same partition every time (%q)", "some-key", got, i, first)
		}
	}
}

func TestRouteEmptyRing(t *testing.T) {
	t.Parallel()

	ring := &Ring{replicas: 10}
	ring.Rebuild(nil)
	if _, err := ring.Route("key"); err == nil {
		t.Fatal("Route on an empty ring = nil error, want an error")
	}
}

func TestRebuildOnlyRedistributesKeysFromTheRemovedPartition(t *testing.T) {
	t.Parallel()

	ring := NewRing([]string{"p0", "p1", "p2", "p3"}, 200)

	keys := make([]string, 60)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%03d", i)
	}

	before := make(map[string]string, len(keys))
	for _, k := range keys {
		p, err := ring.Route(k)
		if err != nil {
			t.Fatalf("Route(%q): %v", k, err)
		}
		before[k] = p
	}

	ring.Rebuild([]string{"p0", "p1", "p3"}) // remove p2

	for _, k := range keys {
		after, err := ring.Route(k)
		if err != nil {
			t.Fatalf("Route(%q) after rebuild: %v", k, err)
		}
		if before[k] != "p2" && after != before[k] {
			t.Errorf("key %q was on %q (not the removed partition) but moved to %q after removing p2", k, before[k], after)
		}
		if after == "p2" {
			t.Errorf("key %q still routes to the removed partition p2", k)
		}
	}
}

func TestConcurrentRouteDuringRebuild(t *testing.T) {
	t.Parallel()

	ring := NewRing([]string{"p0", "p1", "p2"}, 50)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := ring.Route("some-key"); err != nil {
						t.Errorf("Route during concurrent rebuild: %v", err)
						return
					}
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			ring.Rebuild([]string{"p0", "p1", "p2"})
		} else {
			ring.Rebuild([]string{"p0", "p2"})
		}
	}
	close(stop)
	wg.Wait()
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestRebuildOnlyRedistributesKeysFromTheRemovedPartition` is the test that
actually matters here — the `routeInRing` table proves the search logic is
correct in isolation, but only this property test proves the *reason*
consistent hashing exists: capacity changes cost roughly `1/n` of the
keyspace, not all of it. Carry this forward: a ring, a load-balancer
weighting table, or any other "minimal disruption on membership change"
structure needs a test that changes membership and asserts what *didn't*
move, not just a test that the routing function returns a value.

## Resources

- [Consistent Hashing and Random Trees (Karger et al., 1997)](https://www.akamai.com/site/en/documents/research-paper/consistent-hashing-and-random-trees-distributed-caching-protocols-for-relieving-hot-spots-on-the-world-wide-web-technical-publication.pdf) — the original paper this module's ring implements a simplified version of.
- [Amazon Dynamo paper, Section 4.3](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — a production system's use of consistent hashing with virtual nodes.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the primitive keeping `Rebuild` and `Route` race-free without a reader-side lock.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-atomic-config-reload-zero-downtime.md](30-atomic-config-reload-zero-downtime.md) | Next: [32-gossip-protocol-state-merge.md](32-gossip-protocol-state-merge.md)
