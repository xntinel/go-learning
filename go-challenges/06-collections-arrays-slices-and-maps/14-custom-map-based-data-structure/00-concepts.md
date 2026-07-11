# Custom Map-Based Data Structures for Backend Systems — Concepts

Every non-trivial backend service is full of hand-built, map-backed structures
that the standard library does not ship: a bounded cache in front of a slow
datastore, an idempotency guard on a POST write path, a secondary (inverted)
index inside an in-memory repository, a sharded counter for a hot metric, a
heavy-hitter detector feeding a rate limiter, and a probabilistic sketch for
when an exact counter would not fit in memory. Each one is a `map` plus a small
amount of extra machinery — a linked list, a heap, a lock strategy, a hash
seed — chosen to meet a specific constraint. This file is the conceptual
foundation for the ten independent exercises that follow; each exercise builds
one of those structures as a self-contained module with its own tests.

The senior decisions repeat across all of them and are worth stating up front:
(1) exact vs approximate given a memory budget; (2) the concurrency model — a
plain `map`+`RWMutex`, `sync.Map`, or stripe/shard locking — chosen for the
real read/write ratio; and (3) the eviction policy — LRU vs TTL, lazy vs active.
Getting these wrong produces concrete incidents: a `fatal error: concurrent map
read and map write` that `recover()` cannot catch and that takes down the whole
process, a map used as an unbounded cache that never shrinks and OOMs the pod,
or a test that silently depends on map iteration order and flakes in CI.

## The map is a hash table, and its iteration order is deliberately random

A Go `map` is a hash table with amortized O(1) access; since Go 1.24 the runtime
uses a Swiss-table implementation. The one property that surprises people is that
`range` over a map visits keys in a **randomized** order that changes from one
iteration to the next, even within a single run. This is not an accident of the
implementation — the runtime intentionally perturbs the start position so that
code cannot grow a dependency on order. The practical rule: when you need
deterministic output — a serialized response, a test assertion, a log line — you
must sort the keys yourself (`slices.Sorted(maps.Keys(m))`), never rely on the
order `range` happens to produce. A test that asserts "keys come out sorted" or
"in insertion order" is not testing your code; it is testing a coin flip, and it
will eventually flake.

## Comma-ok is the only way to distinguish absent from zero

`v := m[k]` returns the zero value for a missing key, which is indistinguishable
from a key whose stored value *is* the zero value. The two-result form
`v, ok := m[k]` is the only correct way to tell them apart: `ok` is false only
when the key is absent. This matters most for sets and for any map whose value
type has a meaningful zero (an `int` counter that can legitimately be 0, a
`bool` flag). Reaching for a bare `m[k]` to test membership is a latent bug that
surfaces the first time someone stores a zero value on purpose.

## Sets: `map[K]struct{}`, not `map[K]bool`

The idiomatic Go set is `map[K]struct{}`. The empty struct occupies zero bytes,
so the value half of every entry is free, and — because there is no value to
inspect — membership can only be tested with comma-ok on the key, which removes
the false-vs-absent ambiguity that `map[K]bool` invites. `map[K]bool` wastes a
byte per entry and tempts callers to write `if m[k]`, which conflates "present
and false" with "absent". Use `struct{}` for pure membership; reserve
`map[K]bool` for the rare case where you genuinely need to store a two-state flag
per key.

## Maps are not safe for concurrent use — and the failure is fatal, not a race

This is the single most important operational fact in this lesson. A concurrent
read and write of the same map does not merely produce a torn value; the Go
runtime actively detects it and calls `fatal error: concurrent map read and map
write`, which terminates the entire process. It is not a `panic`, so `recover()`
cannot catch it, and no amount of defensive `defer` around the call site will
save you. One goroutine doing `m[k]++` while another ranges the same map is
enough. The options, ranked by workload:

- **`RWMutex` + `map`** — the general-purpose default. Correct for any read/write
  mix; a single writer excludes all readers. The bottleneck is that one lock
  serializes the whole map, so under heavy write concurrency on many distinct
  keys it becomes a contention point.
- **`sync.Map`** — tuned for exactly two patterns: write-once-then-read-many
  (a cache populated at startup) and goroutines that operate on *disjoint* sets
  of keys. It is **not** a drop-in faster map; on a general or write-heavy
  workload a plain `RWMutex`+map usually wins. It also throws away compile-time
  type safety (keys and values are `any`), so you cast on every access. Benchmark
  before reaching for it; do not assume it is faster.
- **Shard / stripe locking** — partition the key space into N independent shards,
  each a `map` behind its own `RWMutex`, and route a key to its shard by hashing.
  A write to one shard does not block writes to the others, so contention drops
  by roughly a factor of N. This is the right tool for a hot counter with high
  write concurrency across many keys.

## Maps grow but never shrink — an unbounded map is a memory leak

`delete(m, k)` frees the entry logically, but the runtime does not return the
underlying bucket array to the heap; a map that once held a million keys keeps
that footprint even after you delete every key. Consequently a map used as a
cache with no eviction policy grows without bound and eventually OOMs the pod —
even if you diligently delete stale keys, the buckets stay allocated. There are
two cures: bound the size with an eviction policy (LRU or TTL), or periodically
rebuild into a fresh map (`m2 := make(map[K]V, len(m)); ... ; m = m2`) to reclaim
the buckets. "A map is my cache" without one of these is an incident waiting to
happen.

## You cannot address or partially mutate a map element

`&m[k]` is a compile error, and so is `m[k].field = x` when the value is a struct
— because a map element is not addressable (the runtime may move it during a
grow). To mutate part of a stored struct you must either store a pointer
(`map[K]*V`, then `m[k].field = x` works because you mutate through the pointer)
or read-modify-write the whole value (`v := m[k]; v.field = x; m[k] = v`). This
constraint shapes every structure here: the sharded map's `Update` reads the
whole value, bumps it, and writes it back precisely because it cannot take the
address of the counter in place.

## Key types must be comparable, and NaN keys are a trap

A map key type must be comparable with `==`: slices, maps, and functions are
illegal keys and fail to compile. Structs are fine as keys if *all* their fields
are comparable, which makes a small struct (`struct{ Method, Path string }`) a
natural composite key. The pathological case is a floating-point key that can be
`NaN`: because `NaN != NaN`, an entry stored under a `NaN` key can never be
looked up again — it is a permanent, unreachable leak. Never key a map on a
float that could be `NaN`.

## Preallocate when the final size is known

`make(map[K]V, sizeHint)` sizes the initial bucket array so the map can hold
roughly `sizeHint` entries before it has to grow and rehash. On a hot path that
builds a map of known size — decoding a batch, indexing a fixed set — the hint
eliminates repeated grow/rehash churn. It is a hint, not a cap; the map still
grows past it if needed.

## LRU = map for lookup + doubly linked list for recency

A least-recently-used cache needs two O(1) operations: look up a key, and move
the just-touched key to the "most recent" end while evicting the "least recent"
end. A `map[K]*list.Element` gives O(1) lookup; a `container/list` doubly linked
list gives O(1) move-to-front and O(1) removal of the tail. The trick is that the
map value *is* the list node (`*list.Element`), so both structures point at the
same object: the map finds the node, and the node carries its position in the
recency order. `Get` must `MoveToFront` on a hit or recency never updates and the
policy silently degrades to random eviction.

## TTL eviction has two modes, and real caches use both

Time-to-live expiry can be enforced two ways. **Lazy** eviction treats an expired
entry as a miss on read (`Get` checks the deadline and reports absent) but leaves
it in the map — cheap, but a key that is written once and never read again
lingers forever and holds its memory. **Active** eviction runs a background
janitor on a ticker that sweeps expired entries and deletes them — it reclaims
memory but costs CPU and, crucially, needs a clean shutdown path or the goroutine
leaks. Production caches combine them: lazy on read for correctness, active in the
background to bound memory. The janitor must be stoppable (a context or a stop
channel), and its lifecycle must be tested, or you have shipped a goroutine leak.

## time.Time carries a monotonic clock; inject a clock in tests

A `time.Time` from `time.Now()` carries both a wall-clock reading and a monotonic
reading; duration math and `t.After(deadline)` use the monotonic component, so
expiry comparisons stay correct even if the wall clock is stepped by NTP. For
*tests*, though, you do not want to depend on real time passing — inject a clock
(`now func() time.Time`, or a small fake with an atomic nanosecond counter) so a
test can advance time instantly and assert expiry deterministically, with no
`time.Sleep` and no flakiness. (Go 1.25's `testing/synctest` is another route,
covered elsewhere; explicit clock injection is the portable one.)

## Probabilistic structures trade exactness for fixed memory

When the number of distinct items is so large that an exact `map[string]int`
counter would not fit the memory budget, a **Count-Min Sketch** estimates
frequencies in O(width × depth) counters — a footprint independent of how many
distinct items you see. Its error is **one-sided**: it never *under*-counts, only
*over*-counts, because collisions can only add to a counter. The over-estimate is
bounded by roughly `N / width` (N = total additions), and the confidence that
the bound holds rises with the depth (number of rows). You feed it a stream and
ask "is this item hot?"; you accept that a cold item may occasionally look warm,
in exchange for constant memory. The row independence that the error bound
assumes comes from **pairwise-independent** hashes; the practical shortcut is one
strong base hash (FNV-1a) XORed with a distinct per-row salt, not the same hash
reused across every row. `Estimate` returns the **minimum** across rows (the
tightest upper bound); returning the maximum would give a useless lower bound.

## Deterministic hashing vs DoS-resistant hashing

`hash/fnv` is a fixed, deterministic hash: the same bytes hash to the same value
in every process, which is what a reproducible sketch wants. `hash/maphash`
instead uses a per-process **random seed**, which is what the built-in map uses
to resist hash-flooding — an attacker who could predict your hash could feed you
keys that all collide into one bucket and turn O(1) lookups into O(n). When you
build your *own* sharded map or bucketed index you lose the runtime's built-in
seeding, so you reintroduce it with `maphash.MakeSeed()` and
`maphash.String`/`maphash.Comparable`. `maphash.Comparable[T]` (Go 1.24) hashes
*any* comparable value — struct keys included — which is exactly what you need to
route a composite key to a shard.

## Secondary indexes are derived state that must stay in sync

An inverted index (`map[fieldValue][]ID`) alongside the primary store turns an
O(n) scan into an O(1)+O(k) lookup, but it is *derived* state: every `Insert` and
`Delete` on the primary store must update the index inside the same critical
section, or the two drift apart and a query returns a deleted row. The subtle
part is cleanup — when the last ID for a field value is removed, the empty slice
(and its key) must be pruned, or the index accumulates empty entries and, per the
"maps never shrink" rule, leaks memory over time.

## Exact top-K = frequency map + bounded min-heap

To find the K most frequent keys exactly, count with a `map[string]int`, then
push counts into a `container/heap` min-heap that you keep capped at size K: when
the heap exceeds K, pop the smallest, so what remains is the K largest in
O(n log K). Choose this over a sketch when the cardinality is small enough to
count exactly and you need exact numbers; choose the sketch when it is not. Ties
must be broken deterministically (by key) or the output — and its test — becomes
order-dependent.

## Idempotency guards store the first result and serialize duplicates

A retried POST (client timeout, load-balancer retry) must not execute the write
twice. An idempotency guard keys a `map[string]result` on a client-supplied
`Idempotency-Key`, stores the first response, and replays it for any retry with
the same key. Concurrent duplicates — two goroutines arriving with the same key
at once — are serialized with an in-flight marker (a `chan struct{}` the second
caller waits on) so the handler runs exactly once and both callers get the same
answer. A per-key TTL bounds how long the stored result is remembered.

## Common Mistakes

### Reading and writing a map from multiple goroutines unsynchronized

This is not a data race you can shrug off — it is a fatal runtime crash
(`concurrent map read and map write`) that `recover()` cannot trap and that
takes the whole process down. One goroutine writing while another reads or ranges
the same map is enough. Guard every shared map with a lock (or shard it, or use
`sync.Map` where it fits); never assume "it's just a read" is safe while another
goroutine might write.

### Modeling a set as `map[K]bool`

Wrong: `map[K]bool`, then `if m[k] { ... }`. It wastes a byte per entry and makes
"present and false" indistinguishable from "absent". Fix: `map[K]struct{}` and
test membership with comma-ok (`_, ok := m[k]`).

### Relying on map range order

Wrong: a serializer or test that assumes `range` yields sorted or
insertion-ordered keys. The order is randomized per iteration and such code flakes
in CI. Fix: sort explicitly — `slices.Sorted(maps.Keys(m))` — whenever output
must be deterministic.

### Using a map as a cache with no eviction

Wrong: an ever-growing `map` cache. Because maps never shrink, memory climbs
without bound even if you delete keys. Fix: bound it with LRU or TTL eviction, or
periodically rebuild into a fresh map.

### Trying to mutate a struct stored by value in a map

Wrong: `m[k].field = x` or `&m[k]` — both are compile errors, because map
elements are not addressable. Fix: store `map[K]*V` and mutate through the
pointer, or read-modify-write the whole value (`v := m[k]; v.field = x; m[k] = v`).

### Forgetting MoveToFront in an LRU Get

Wrong: an LRU whose `Get` returns the value without moving the node to the front,
so recency never updates and eviction becomes effectively random; or evicting
from the wrong end of the list. Fix: `MoveToFront` on every hit and evict the
`Back()` node.

### A TTL janitor with no shutdown path

Wrong: starting a ticker/sweeper goroutine that nothing can stop — a goroutine
leak on every cache instance. Also wrong: comparing deadlines against a clock the
test cannot control, forcing real `time.Sleep`s and flaky tests. Fix: drive the
janitor with a context or stop channel and verify it exits; inject a clock so
expiry is deterministic.

### Reusing one hash across all sketch rows, or returning the max

Wrong: `row[hash(item) % width]` with the same hash for every row — the counters
are correlated and the independence the error bound assumes is destroyed. Also
wrong: `Estimate` returning the maximum across rows (a lower bound). Fix: combine
the base hash with a distinct per-row salt, and return the minimum (the correct
upper bound).

### Sharding by a weak key transform

Wrong: choosing a shard by `key[0]` or `len(key)` — real keys cluster, producing
hot shards and uneven contention that defeats the point of sharding. Fix: hash the
key with `maphash` and take the result modulo the shard count.

### Using a NaN-capable float as a key

Wrong: keying a map on a `float64` that could be `NaN`. Because `NaN != NaN`, the
entry becomes permanently unreachable — a silent leak. Fix: never key on a float
that can be NaN; use an integer, string, or comparable struct.

### Assuming `sync.Map` is universally faster

Wrong: reaching for `sync.Map` by default "for performance". On write-heavy or
general workloads a plain `RWMutex`+map or a sharded map usually wins, and
`sync.Map` throws away type safety. Fix: use it only for its two intended
patterns (write-once-read-many, disjoint key sets), and benchmark before
committing.

Next: [01-cms-sketch-implement.md](01-cms-sketch-implement.md)
