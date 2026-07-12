# Exercise 2: Offset Tracking and Consumer Lag

The offset is a consumer group's only durable memory: it records, per partition, exactly how far the group has processed. This exercise builds the thread-safe `OffsetStore` that holds those high-water marks, the `Lag` math that turns "latest written" and "last committed" into a count of unprocessed messages, a `Tracker` that reports per-partition and total lag, and the `KeyPartitioner` that routes a key to a stable partition.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
offset.go            OffsetStore (Commit/Fetch, -1 sentinel); Lag; Tracker; KeyPartitioner
cmd/
  demo/
    main.go          write per-partition latest, commit some, print lag and partition routing
offset_test.go       commit/fetch isolation, race test, lag table, tracker lifecycle, determinism
```

- Files: `offset.go`, `cmd/demo/main.go`, `offset_test.go`.
- Implement: `OffsetStore` with `Commit`/`Fetch`, the `Lag` function, the `Tracker` type, and `KeyPartitioner`.
- Test: group isolation, concurrent access under `-race`, the lag formula including the -1 sentinel, the tracker lifecycle, and partitioner determinism.
- Verify: `go test -race ./...`

### Why -1 is the sentinel, and why lag is a subtraction

A committed offset is a high-water mark: committing offset 41 asserts that every message at offsets 0 through 41 is processed. The store keys this by `group -> partition -> offset`, exactly the shape of Kafka's `__consumer_offsets` topic, so two groups reading the same partition keep independent progress and never see each other's commits. A `Fetch` on a key that was never committed returns -1, not 0, because 0 is a real offset - the very first message. Collapsing "processed message 0" and "processed nothing" into the same value would make the first message indistinguishable from an empty group, and the lag math would be off by one for every fresh consumer.

Lag is then a single subtraction: `latest - committed`. The sentinel makes it come out right at the boundary. A partition written up to offset 99 with nothing committed has lag `99 - (-1) = 100`, which is the exact count of pending messages at offsets 0..99. Commit through 49 and lag is `99 - 49 = 50` (offsets 50..99). Commit through 99 and lag is 0. The result is clamped at 0 so that a committed offset transiently ahead of the recorded latest - which can happen if the latest is updated by a different goroutine a beat later - reports 0 rather than a negative number.

The `Tracker` pairs the store with the highest offset seen per partition so it can answer "how far behind is this group". It guards its `latest` map with a mutex and snapshots it before fetching committed offsets, so a concurrent `SetLatest` can never tear a `Lag()` read. `KeyPartitioner` closes the loop: it hashes a key with FNV-1a and takes the result modulo the partition count, so the same key always lands on the same partition. That stability is what gives a key a total order - all of a key's messages share one partition, and within a partition offsets are monotonic, so per-key ordering is preserved even though the topic as a whole is only partially ordered.

Create `offset.go`:

```go
package offset

import (
	"hash/fnv"
	"sort"
	"sync"
)

// OffsetStore is a thread-safe in-memory store for committed consumer-group
// offsets. It models the semantics of Kafka's __consumer_offsets topic without
// any I/O dependency: one durable high-water mark per (group, partition).
type OffsetStore struct {
	mu      sync.RWMutex
	offsets map[string]map[int]int64 // group -> partition -> offset
}

// NewOffsetStore returns an empty, thread-safe offset store.
func NewOffsetStore() *OffsetStore {
	return &OffsetStore{offsets: make(map[string]map[int]int64)}
}

// Commit records that all messages up to and including offset have been
// processed by group on partition. Safe for concurrent use.
func (s *OffsetStore) Commit(group string, partition int, offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offsets[group] == nil {
		s.offsets[group] = make(map[int]int64)
	}
	s.offsets[group][partition] = offset
}

// Fetch returns the last committed offset for group on partition, or -1 if
// nothing has been committed. The sentinel -1 (rather than 0) distinguishes
// "nothing processed" from "processed exactly offset 0".
func (s *OffsetStore) Fetch(group string, partition int) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g := s.offsets[group]; g != nil {
		if off, ok := g[partition]; ok {
			return off
		}
	}
	return -1
}

// Lag returns the number of unconsumed messages given the highest written
// offset (latest) and the last committed offset. A committed offset of -1
// means nothing is processed, so lag is latest+1 (offsets 0..latest). The
// result is clamped at 0 so a committed offset ahead of latest never reports
// negative lag.
func Lag(latest, committed int64) int64 {
	l := latest - committed
	if l < 0 {
		return 0
	}
	return l
}

// Tracker pairs an OffsetStore with the highest offset written per partition so
// it can report consumer lag for one group. It is safe for concurrent use.
type Tracker struct {
	store *OffsetStore
	group string

	mu     sync.Mutex
	latest map[int]int64
}

// NewTracker returns a lag tracker for group backed by store.
func NewTracker(store *OffsetStore, group string) *Tracker {
	return &Tracker{store: store, group: group, latest: make(map[int]int64)}
}

// SetLatest records the highest offset written to partition.
func (t *Tracker) SetLatest(partition int, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latest[partition] = offset
}

// Commit records group's processed high-water mark for partition.
func (t *Tracker) Commit(partition int, offset int64) {
	t.store.Commit(t.group, partition, offset)
}

// Lag returns per-partition lag for every partition that has had a latest
// offset recorded. lag(p) = Lag(latest[p], committed[p]).
func (t *Tracker) Lag() map[int]int64 {
	t.mu.Lock()
	snap := make(map[int]int64, len(t.latest))
	for p, v := range t.latest {
		snap[p] = v
	}
	t.mu.Unlock()

	out := make(map[int]int64, len(snap))
	for p, latest := range snap {
		out[p] = Lag(latest, t.store.Fetch(t.group, p))
	}
	return out
}

// TotalLag sums Lag over all tracked partitions.
func (t *Tracker) TotalLag() int64 {
	var total int64
	for _, l := range t.Lag() {
		total += l
	}
	return total
}

// Partitions returns the sorted list of partitions the tracker knows about.
func (t *Tracker) Partitions() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	ps := make([]int, 0, len(t.latest))
	for p := range t.latest {
		ps = append(ps, p)
	}
	sort.Ints(ps)
	return ps
}

// KeyPartitioner maps a message key to a partition index using FNV-1a hashing.
// The same key always maps to the same partition for a given numPartitions,
// which keeps all messages for one key ordered inside a single partition.
func KeyPartitioner(key []byte, numPartitions int) int {
	h := fnv.New32a()
	h.Write(key)
	return int(h.Sum32()) % numPartitions
}
```

The `Lag` function is intentionally a free function, not a method, so it can be unit-tested over a table of `(latest, committed)` pairs with no store at all - the sentinel boundary and the negative clamp are the two cases that matter. `Tracker.Lag` snapshots `latest` under the lock, releases the lock, then fetches committed offsets from the store (which has its own lock); holding one lock while acquiring another in a fixed order avoids the classic lock-ordering deadlock.

### The runnable demo

The demo writes a thousand messages to each of three partitions (latest offset 999), commits partition 0 fully, partition 1 halfway, and leaves partition 2 untouched, then prints the per-partition and total lag. It also shows that a different group sees the uncommitted sentinel, and that the key partitioner routes a few sample keys to stable partitions.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/offset"
)

func main() {
	store := offset.NewOffsetStore()
	tr := offset.NewTracker(store, "orders")

	// Three partitions, each written up to offset 999 (1000 messages).
	for p := 0; p < 3; p++ {
		tr.SetLatest(p, 999)
	}

	fmt.Println("After writing 1000 messages per partition, nothing committed:")
	fmt.Printf("  total lag = %d\n", tr.TotalLag())

	// Commit progress: partition 0 fully caught up, 1 halfway, 2 untouched.
	tr.Commit(0, 999)
	tr.Commit(1, 499)
	lag := tr.Lag()
	fmt.Println("After committing 999/499/none:")
	for _, p := range tr.Partitions() {
		fmt.Printf("  partition %d: lag %d\n", p, lag[p])
	}
	fmt.Printf("  total lag = %d\n", tr.TotalLag())

	// Group isolation: a different group sees no committed offsets.
	fmt.Printf("\nother-group committed offset for partition 0: %d (uncommitted sentinel)\n",
		store.Fetch("other-group", 0))

	// Key partitioning is deterministic and key-stable.
	fmt.Println("\nKey partitioner (12 partitions):")
	for _, k := range []string{"user-alice", "user-bob", "user-carol"} {
		fmt.Printf("  %q -> partition %d\n", k, offset.KeyPartitioner([]byte(k), 12))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
After writing 1000 messages per partition, nothing committed:
  total lag = 3000
After committing 999/499/none:
  partition 0: lag 0
  partition 1: lag 500
  partition 2: lag 1000
  total lag = 1500

other-group committed offset for partition 0: -1 (uncommitted sentinel)

Key partitioner (12 partitions):
  "user-alice" -> partition 3
  "user-bob" -> partition 4
  "user-carol" -> partition 6
```

Partition 1's lag is 500, not 499: committing offset 499 means offsets 500..999 are still pending, which is 500 messages. The off-by-one is exactly the thing the sentinel-aware subtraction gets right.

### Tests

`TestOffsetStoreCommitAndFetch` pins the sentinel and group isolation. `TestOffsetStoreConcurrentAccess` hammers the store from 400 goroutines so the race detector can prove the locking is sound. `TestLagFormula` is a table over the four interesting `(latest, committed)` pairs, including the -1 sentinel and the clamp. `TestTrackerLagLifecycle` and `TestTrackerTotalLag` walk a tracker from "nothing committed" to "fully caught up". `TestKeyPartitionerIsDeterministic` checks that the same key always maps to the same in-range partition across a thousand calls. The two Example functions are verified by `go test` against their `// Output:` blocks.

Create `offset_test.go`:

```go
package offset

import (
	"fmt"
	"sync"
	"testing"
)

func TestOffsetStoreCommitAndFetch(t *testing.T) {
	t.Parallel()

	s := NewOffsetStore()
	if got := s.Fetch("g", 0); got != -1 {
		t.Fatalf("Fetch before commit = %d, want -1", got)
	}
	s.Commit("g", 0, 42)
	if got := s.Fetch("g", 0); got != 42 {
		t.Fatalf("Fetch after commit = %d, want 42", got)
	}
	if got := s.Fetch("other-group", 0); got != -1 {
		t.Fatalf("Fetch from different group = %d, want -1 (groups are isolated)", got)
	}
}

func TestOffsetStoreConcurrentAccess(t *testing.T) {
	t.Parallel()

	s := NewOffsetStore()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		go func(n int64) {
			defer wg.Done()
			s.Commit("g", int(n%5), n)
		}(int64(i))
		go func(n int) {
			defer wg.Done()
			s.Fetch("g", n%5)
		}(i)
	}
	wg.Wait()
}

func TestLagFormula(t *testing.T) {
	t.Parallel()

	cases := []struct {
		latest, committed, want int64
	}{
		{99, -1, 100}, // nothing committed: offsets 0..99 all pending
		{99, 49, 50},  // committed through 49: offsets 50..99 pending
		{99, 99, 0},   // fully caught up
		{99, 100, 0},  // committed ahead of latest: clamp to 0
	}
	for _, c := range cases {
		if got := Lag(c.latest, c.committed); got != c.want {
			t.Errorf("Lag(%d, %d) = %d, want %d", c.latest, c.committed, got, c.want)
		}
	}
}

func TestTrackerLagLifecycle(t *testing.T) {
	t.Parallel()

	tr := NewTracker(NewOffsetStore(), "grp")
	tr.SetLatest(0, 99)

	if got := tr.Lag()[0]; got != 100 {
		t.Fatalf("lag before commit = %d, want 100", got)
	}
	tr.Commit(0, 49)
	if got := tr.Lag()[0]; got != 50 {
		t.Fatalf("lag after commit 49 = %d, want 50", got)
	}
	tr.Commit(0, 99)
	if got := tr.Lag()[0]; got != 0 {
		t.Fatalf("lag fully caught up = %d, want 0", got)
	}
}

func TestTrackerTotalLag(t *testing.T) {
	t.Parallel()

	tr := NewTracker(NewOffsetStore(), "grp")
	for p := 0; p < 3; p++ {
		tr.SetLatest(p, 9) // 10 messages each
	}
	if got := tr.TotalLag(); got != 30 {
		t.Fatalf("total lag = %d, want 30", got)
	}
	tr.Commit(0, 9)
	if got := tr.TotalLag(); got != 20 {
		t.Fatalf("total lag after one partition drained = %d, want 20", got)
	}
}

func TestKeyPartitionerIsDeterministic(t *testing.T) {
	t.Parallel()

	const numPartitions = 12
	for i := 0; i < 1000; i++ {
		key := []byte(fmt.Sprintf("order-%d", i%100))
		p1 := KeyPartitioner(key, numPartitions)
		p2 := KeyPartitioner(key, numPartitions)
		if p1 != p2 {
			t.Fatalf("KeyPartitioner(%q, %d) returned %d and %d", key, numPartitions, p1, p2)
		}
		if p1 < 0 || p1 >= numPartitions {
			t.Fatalf("KeyPartitioner(%q) = %d out of range [0,%d)", key, p1, numPartitions)
		}
	}
}

func ExampleOffsetStore() {
	s := NewOffsetStore()
	s.Commit("my-group", 0, 42)
	fmt.Println(s.Fetch("my-group", 0))
	fmt.Println(s.Fetch("my-group", 1))
	// Output:
	// 42
	// -1
}

func ExampleLag() {
	fmt.Println(Lag(99, -1))
	fmt.Println(Lag(99, 49))
	fmt.Println(Lag(99, 99))
	// Output:
	// 100
	// 50
	// 0
}
```

## Review

The offset layer is correct when the sentinel and the subtraction agree at the boundary: an uncommitted partition fetches -1, and `Lag(latest, -1)` equals `latest + 1`, the true count of pending messages. Confirm group isolation - two groups never see each other's commits - and confirm the concurrent test passes under `-race`, which is the real proof that the `RWMutex` discipline holds. The tracker must snapshot its `latest` map under the lock before reaching into the store, so a `Lag()` read and a concurrent `SetLatest` cannot interleave into a torn result.

Common mistakes for this feature. The first is using 0 as the "nothing committed" sentinel, which makes a fresh group's lag off by one and hides whether message 0 was ever processed. The second is writing `lag = latest - committed - 1`, which double-corrects and under-counts; the subtraction is already exact once the sentinel is -1. The third is forgetting the negative clamp, so a latest that briefly trails a just-committed offset reports a nonsensical negative lag instead of 0.

## Resources

- [`hash/fnv`](https://pkg.go.dev/hash/fnv) - the FNV-1a hash `KeyPartitioner` uses to route keys to stable partitions.
- [`sync` package](https://pkg.go.dev/sync) - `sync.Mutex` and `sync.RWMutex`, the primitives that make the store and tracker concurrency-safe.
- [Kafka: Offset Management](https://kafka.apache.org/documentation/#design_consumerposition) - how a production broker stores and fetches committed consumer positions.

---

Back to [01-partition-assignment.md](01-partition-assignment.md) | Next: [03-consumer-group-coordinator.md](03-consumer-group-coordinator.md)
