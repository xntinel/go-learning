# Exercise 11: Cluster Quorum and Capacity Without Materializing a Slice

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every Raft- or etcd-style cluster runs the same check on every heartbeat: do we
have a majority of healthy voters. A scheduler asks the sibling question just as
often: how much free memory is there across every node. Both walk a
cluster-sized map -- node ID to status -- on a hot path that fires far more
often than the cluster's membership actually changes. That is exactly where
`00-concepts.md`'s point about iterators pays for itself: `maps.Values` and
`maps.All` return an `iter.Seq`, not a collection, and a plain range loop over
one can stop the instant it has an answer, without ever building a slice to
hold values the caller was never going to look at in full.

The trap is a habit carried over from the pre-1.23 `x/exp/maps` API, where
`Keys` and `Values` really did return slices: reach for `slices.Collect` first,
out of muscle memory, before doing anything else with a map's values. On a
quorum check that habit is worse than merely wasteful. `slices.Collect(maps.Values(m))`
must visit every entry to build the slice before the scan that follows it can
even begin, so a quorum satisfied by the very first node examined still pays
for materializing the whole cluster. The allocation is not just extra work; it
is work spent specifically on the part of the answer the caller has already
decided not to need.

This module builds `clusterscan`, a package with exactly the two heartbeat
checks described above, both written as a direct range over `maps.Values`:
early-exit for quorum, streaming accumulation for capacity. The materializing
form is never part of the package's API -- it lives in the test file, where an
allocation probe pins the cost the streaming form avoids.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
clusterscan/              module example.com/clusterscan
  go.mod                  go 1.24
  clusterscan.go          NodeStatus; HealthyQuorum; TotalCapacity
  clusterscan_test.go     quorum table, capacity table, materialized contrast x2,
                          concurrent-reads test, ExampleHealthyQuorum
```

- Files: `clusterscan.go`, `clusterscan_test.go`.
- Implement: `type NodeStatus struct{ Healthy bool; CapacityMB int }`; `HealthyQuorum(nodes map[string]NodeStatus, need int) bool` scanning `maps.Values(nodes)` and returning as soon as `need` healthy nodes are seen; `TotalCapacity(nodes map[string]NodeStatus) int` streaming the sum over the same iterator.
- Test: the quorum table (met, unmet, need beyond cluster size, zero/negative need, empty and nil maps); the capacity table (nil, empty, single node, mixed health); a `healthyQuorumMaterialized` and a `totalCapacityMaterialized` contrast, each proven correct-but-costlier via `testing.AllocsPerRun`; a concurrent-reads test exercising the documented concurrency contract; and `ExampleHealthyQuorum` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/11-quorum-capacity-lazy-iterator-scan
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/11-quorum-capacity-lazy-iterator-scan
go mod edit -go=1.24
```

### An iterator is not a collection -- and that is the point

`maps.Values(nodes)` returns an `iter.Seq[NodeStatus]`: a function of shape
`func(yield func(NodeStatus) bool)`, not a slice. It has no `len`, you cannot
index into it, and `sort.Slice` will not accept it. What it does have is a
range loop's full control flow, including early exit:

```go
values := slices.Collect(maps.Values(nodes)) // the whole cluster, materialized
healthy := 0
for _, v := range values {
    if v.Healthy {
        healthy++
    }
}
return healthy >= need
```

By the time this function's `for` loop runs even once, `slices.Collect` has
already ranged the entire map and allocated a slice sized to hold every node.
If `need` is 2 and the cluster has 500 nodes with the first 2 healthy, the
answer was knowable after 2 comparisons, but the code above pays for 500
regardless -- and pays for it *first*, before a single health check has
happened. Compare that to ranging `maps.Values` directly:

```go
healthy := 0
for status := range maps.Values(nodes) {
    if status.Healthy {
        healthy++
        if healthy >= need {
            return true   // stop; the rest of the cluster was never visited
        }
    }
}
```

No slice is ever built. The `return` inside the loop is a `break` that the
compiler understands is reachable from a range over a function value exactly
as it would be from a range over a slice -- `iter.Seq`'s `yield` callback
returns `false` and the iterator's internal loop stops calling it. This is
the same allocation-aware discipline `00-concepts.md` asks for everywhere
else in the `maps` package: bridge to a slice only when you actually need one
(sorting, indexing, retaining beyond the current call), and never as a reflex
before a scan that could have run directly on the iterator.

Create `clusterscan.go`:

```go
// Package clusterscan answers the two questions cluster membership code asks
// on every heartbeat: is there a healthy quorum, and how much capacity is
// free. Both functions range an iter.Seq directly over the cluster map
// instead of materializing a slice first, so they never allocate memory the
// caller did not ask for and can stop scanning the instant they have their
// answer.
//
// maps.Values(nodes) returns an iter.Seq[NodeStatus]: a function, not a
// collection. It has no length and cannot be indexed or sorted in place, but
// a plain range loop drives it directly, and a range loop can exit early
// with break or return -- which is exactly what a quorum check wants to do
// the moment it already knows the answer, without waiting to see the rest of
// the cluster.
package clusterscan

import "maps"

// NodeStatus is one node's health and free capacity as of its last
// heartbeat. It carries no pointers or slices, so a NodeStatus copied out of
// a map (as every value received from maps.Values is) is an independent
// value with no aliasing to the source map.
type NodeStatus struct {
	Healthy    bool
	CapacityMB int
}

// HealthyQuorum reports whether at least need nodes in nodes are healthy.
//
// It scans nodes' values through maps.Values and returns true the instant
// need healthy nodes have been seen, without visiting the remainder of the
// map and without allocating a slice to hold the values first. A need of
// zero or less is trivially satisfied by any map, including a nil one.
//
// HealthyQuorum performs no synchronization: the caller must ensure nodes is
// not concurrently written while the scan runs, the same requirement any
// range over a map carries.
func HealthyQuorum(nodes map[string]NodeStatus, need int) bool {
	if need <= 0 {
		return true
	}
	healthy := 0
	for status := range maps.Values(nodes) {
		if status.Healthy {
			healthy++
			if healthy >= need {
				return true
			}
		}
	}
	return false
}

// TotalCapacity sums CapacityMB across every node in nodes.
//
// It streams the sum through maps.Values instead of collecting a slice of
// NodeStatus first, so it allocates nothing beyond the loop variable no
// matter how large the cluster is. A nil or empty nodes map sums to zero.
//
// TotalCapacity performs no synchronization; see HealthyQuorum.
func TotalCapacity(nodes map[string]NodeStatus) int {
	total := 0
	for status := range maps.Values(nodes) {
		total += status.CapacityMB
	}
	return total
}
```

### Using it

Both functions take the cluster map directly and return a plain value, so
there is nothing to construct and nothing to hold onto: call `HealthyQuorum`
before routing a write, call `TotalCapacity` before admitting a new
scheduling request, and call them as often as the heartbeat demands. Neither
function mutates `nodes` or retains a reference to it after returning, so the
caller is free to mutate the map on the next heartbeat without any aliasing
concern. The one contract that does cross the boundary is concurrency: like
any range over a map, a concurrent write to `nodes` while either function is
scanning it is a data race, so a cluster map shared across goroutines still
needs its own lock -- these functions do not provide one.

`ExampleHealthyQuorum` is the runnable demonstration of this module: `go
test` executes it and compares its stdout against the `// Output:` comment.

```go
func ExampleHealthyQuorum() {
	nodes := map[string]NodeStatus{
		"node-a": {Healthy: true, CapacityMB: 4096},
		"node-b": {Healthy: false, CapacityMB: 2048},
		"node-c": {Healthy: true, CapacityMB: 8192},
	}

	fmt.Println(HealthyQuorum(nodes, 2))
	fmt.Println(HealthyQuorum(nodes, 3))
	fmt.Println(TotalCapacity(nodes))

	// Output:
	// true
	// false
	// 14336
}
```

### Tests

`TestHealthyQuorum` and `TestTotalCapacity` are the tables: the ordinary
cases plus the borders that a real membership check will eventually hit -- a
need larger than the cluster, a need of zero or less, an empty map, and a nil
map. `healthyQuorumMaterialized` and `totalCapacityMaterialized` are the
unexported contrasts: each reproduces the correct answer through
`slices.Collect(maps.Values(m))` first, and their matching tests assert two
things -- that the answer agrees with the streaming version, and that the
materialized version's allocation count is strictly higher, never a specific
number, since the runtime's own growth curve for `slices.Collect` is not a
contract this package should pin. Both allocation tests skip `t.Parallel`
because `testing.AllocsPerRun` panics if invoked from a parallel test: a
concurrent goroutine allocating in the background would corrupt the
measurement. `TestConcurrentReadsDoNotRace` exercises the concurrency
contract stated on both functions -- many goroutines reading the same map at
once, no writer -- under `-race`.

Create `clusterscan_test.go`:

```go
package clusterscan

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestHealthyQuorum(t *testing.T) {
	t.Parallel()

	nodes := map[string]NodeStatus{
		"a": {Healthy: true, CapacityMB: 1},
		"b": {Healthy: true, CapacityMB: 1},
		"c": {Healthy: false, CapacityMB: 1},
	}
	tests := []struct {
		name  string
		nodes map[string]NodeStatus
		need  int
		want  bool
	}{
		{"quorum met exactly", nodes, 2, true},
		{"quorum unmet", nodes, 3, false},
		{"need exceeds cluster size entirely", nodes, 10, false},
		{"zero need is trivially satisfied", nodes, 0, true},
		{"negative need is trivially satisfied", nodes, -5, true},
		{"empty map cannot satisfy positive need", map[string]NodeStatus{}, 1, false},
		{"nil map cannot satisfy positive need", nil, 1, false},
		{"nil map satisfies zero need", nil, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := HealthyQuorum(tc.nodes, tc.need); got != tc.want {
				t.Fatalf("HealthyQuorum(need=%d) = %v, want %v", tc.need, got, tc.want)
			}
		})
	}
}

func TestTotalCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes map[string]NodeStatus
		want  int
	}{
		{"nil map sums to zero", nil, 0},
		{"empty map sums to zero", map[string]NodeStatus{}, 0},
		{"single node sums to its own capacity", map[string]NodeStatus{"a": {CapacityMB: 512}}, 512},
		{
			"multiple nodes sum regardless of health",
			map[string]NodeStatus{
				"a": {Healthy: true, CapacityMB: 100},
				"b": {Healthy: false, CapacityMB: 250},
				"c": {Healthy: true, CapacityMB: 50},
			},
			400,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := TotalCapacity(tc.nodes); got != tc.want {
				t.Fatalf("TotalCapacity() = %d, want %d", got, tc.want)
			}
		})
	}
}

// healthyQuorumMaterialized is the version of HealthyQuorum a first draft
// reaches for: collect the values into a slice, then scan the slice. It
// mirrors the Invert/Collect trap documented in 00-concepts.md -- calling
// slices.Collect(maps.Values(m)) before scanning allocates an O(n) slice up
// front and, worse, cannot stop early: the full collection happens before a
// single node has been examined, even if the very first entry would have
// satisfied the quorum. It is never exported and never reachable from the
// package API; it exists so the tests can pin the cost it pays.
func healthyQuorumMaterialized(nodes map[string]NodeStatus, need int) bool {
	values := slices.Collect(maps.Values(nodes))
	healthy := 0
	for _, v := range values {
		if v.Healthy {
			healthy++
		}
	}
	return healthy >= need
}

// TestMaterializedVersionAgreesButAllocatesMore proves the two forms produce
// the same answer and then shows why the streaming form is preferred: the
// exact allocation count is a runtime detail and is not asserted, but that
// the materialized version needs strictly more allocations holds across
// toolchains, because it always builds a full slice before it can look at a
// single element.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestMaterializedVersionAgreesButAllocatesMore(t *testing.T) {
	nodes := make(map[string]NodeStatus, 64)
	for i := range 64 {
		nodes[fmt.Sprintf("node-%d", i)] = NodeStatus{Healthy: i%3 == 0, CapacityMB: i}
	}

	if got, want := HealthyQuorum(nodes, 5), healthyQuorumMaterialized(nodes, 5); got != want {
		t.Fatalf("HealthyQuorum = %v, healthyQuorumMaterialized = %v, want equal", got, want)
	}

	streaming := testing.AllocsPerRun(100, func() {
		HealthyQuorum(nodes, 5)
	})
	materialized := testing.AllocsPerRun(100, func() {
		healthyQuorumMaterialized(nodes, 5)
	})
	if !(streaming < materialized) {
		t.Fatalf("allocations: streaming = %v, materialized = %v; want streaming < materialized", streaming, materialized)
	}
}

// totalCapacityMaterialized mirrors the same Invert/Collect trap for the
// summing case: collect every value into a slice with slices.Collect, then
// range the slice to add it up. The sum comes out identical, but the slice
// it did not need to allocate is paid for anyway. It is never exported and
// never reachable from the package API.
func totalCapacityMaterialized(nodes map[string]NodeStatus) int {
	values := slices.Collect(maps.Values(nodes))
	total := 0
	for _, v := range values {
		total += v.CapacityMB
	}
	return total
}

// TestTotalCapacityMaterializedAgreesButAllocatesMore is the summing
// counterpart of TestMaterializedVersionAgreesButAllocatesMore: same
// property, exact < materialized, never a hard-coded count, and the same
// reason for skipping t.Parallel.
func TestTotalCapacityMaterializedAgreesButAllocatesMore(t *testing.T) {
	nodes := make(map[string]NodeStatus, 64)
	for i := range 64 {
		nodes[fmt.Sprintf("node-%d", i)] = NodeStatus{CapacityMB: i}
	}

	if got, want := TotalCapacity(nodes), totalCapacityMaterialized(nodes); got != want {
		t.Fatalf("TotalCapacity = %d, totalCapacityMaterialized = %d, want equal", got, want)
	}

	streaming := testing.AllocsPerRun(100, func() {
		TotalCapacity(nodes)
	})
	materialized := testing.AllocsPerRun(100, func() {
		totalCapacityMaterialized(nodes)
	})
	if !(streaming < materialized) {
		t.Fatalf("allocations: streaming = %v, materialized = %v; want streaming < materialized", streaming, materialized)
	}
}

// TestConcurrentReadsDoNotRace exercises the concurrency contract in the
// package doc comment: HealthyQuorum and TotalCapacity hold no state of
// their own, so many goroutines may call them over the same map at once as
// long as nothing writes to that map concurrently. This test only reads.
func TestConcurrentReadsDoNotRace(t *testing.T) {
	t.Parallel()

	nodes := make(map[string]NodeStatus, 32)
	for i := range 32 {
		nodes[fmt.Sprintf("node-%d", i)] = NodeStatus{Healthy: i%2 == 0, CapacityMB: i}
	}

	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			HealthyQuorum(nodes, 4)
			TotalCapacity(nodes)
		}()
	}
	for range 8 {
		<-done
	}
}

// ExampleHealthyQuorum is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleHealthyQuorum() {
	nodes := map[string]NodeStatus{
		"node-a": {Healthy: true, CapacityMB: 4096},
		"node-b": {Healthy: false, CapacityMB: 2048},
		"node-c": {Healthy: true, CapacityMB: 8192},
	}

	fmt.Println(HealthyQuorum(nodes, 2))
	fmt.Println(HealthyQuorum(nodes, 3))
	fmt.Println(TotalCapacity(nodes))

	// Output:
	// true
	// false
	// 14336
}
```

## Review

Both functions are correct when they agree with the exhaustive, materialized
scan on every input -- `TestMaterializedVersionAgreesButAllocatesMore` and its
capacity counterpart pin exactly that agreement. What makes them worth
choosing over the materialized form is not correctness, which both share, but
cost: ranging `maps.Values` directly never allocates a slice, and
`HealthyQuorum` additionally returns the moment `need` healthy nodes are seen
rather than after the full map has been visited. `slices.Collect(maps.Values(m))`
before a scan is the mistake this module isolates -- it is never wrong, only
strictly more expensive, and the allocation probes prove that property without
pinning a specific count that a future Go release could change. Neither
function synchronizes access to the map it is given; a cluster map shared
across goroutines still needs its own lock around any concurrent writer, which
`TestConcurrentReadsDoNotRace` deliberately does not introduce. Run
`go test -count=1 -race ./...`.

## Resources

- [`maps.Values`](https://pkg.go.dev/maps#Values) and [`maps.All`](https://pkg.go.dev/maps#All) — the iterator-returning functions this module ranges directly.
- [`iter.Seq`](https://pkg.go.dev/iter#Seq) — the range-over-func type these iterators return.
- [`slices.Collect`](https://pkg.go.dev/slices#Collect) — the bridge from an iterator to a slice, and the allocation this module avoids paying before it is needed.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-postings-index-inverted-search.md](10-postings-index-inverted-search.md) | Next: [12-named-route-table-generic-constraint.md](12-named-route-table-generic-constraint.md)
