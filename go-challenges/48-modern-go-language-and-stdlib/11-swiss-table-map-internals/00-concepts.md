# Swiss-Table Map Internals — Concepts

The built-in `map` is the single most-used data structure in a Go backend: request
caches, dedup sets, config lookups, per-connection state. Go 1.24 silently swapped
its implementation from the old chained-bucket design to a Swiss Table, changing the
memory and latency curve under you without a single source change. A senior owns
three consequences of that swap: capacity planning (bytes per entry, why
`make(map[K]V, n)` matters on hot paths), the delete-does-not-shrink gotcha that
turns a bounded cache into a slow leak, and reading benchmarks on the new
implementation correctly so a "regression" is real and not measurement noise. The
internals are opaque from userland — the built-in map exposes no probe counter — so
this lesson makes them concrete by rebuilding a small open-addressing table where
probe behavior becomes directly measurable, then applies that mental model to
profile and benchmark the real map.

## Concepts

### The old map: chained buckets

Before 1.24 a Go map was an array of buckets. Each bucket stored up to 8 key/value
pairs plus a pointer to an overflow bucket. On a collision the runtime walked the
overflow chain; on deletion it left holes that were never compacted. Two costs
followed from that shape. First, a hot key whose bucket had overflowed cost a
pointer-chase per overflow bucket, and each hop was a fresh cache line — poor
locality exactly when you are latency-sensitive. Second, a map churned with inserts
and deletes grew overflow chains that `len` could not see and that only a full grow
would reclaim. The design was simple and correct, but its worst case was a linked
list, and linked lists are where CPUs go to stall.

### The Swiss Table model

A Swiss Table is a flat array of slots divided into groups of 8, with a parallel
control array holding one byte per slot. The hash of a key is split into two parts.
H1 is the upper 57 bits and selects the starting group. H2 is the lower 7 bits and
is stored in the slot's control byte. The 8th bit of the control byte encodes state:
a full slot has its top bit clear and holds H2 directly (a value in `0..127`); an
empty slot is `0b1000_0000`; a deleted slot (a tombstone) is `0b1111_1110`. So one
control byte tells you both whether a slot is occupied and, if so, a 7-bit fingerprint
of the key that lives there — without touching the key itself.

### The fast path: match a whole group at once

The point of the control byte is bulk comparison. To look up a key, compute H2, then
compare it against all 8 control bytes in the group in one shot — SIMD on amd64,
word-arithmetic tricks elsewhere — producing an 8-bit mask of candidate slots. Only
the candidates (usually zero or one) get a full key comparison. A lookup therefore
typically touches a single cache line (the control word plus the slot) and performs
at most a couple of real key comparisons, even in a group that is nearly full. This
is why the Swiss Table wins on locality: the expensive key comparison is gated behind
a cheap byte-mask filter.

### Probing: what you can only see by building it

If the group has no H2 match and no empty slot, the key is not here and the search
moves to the next group — linear probing across groups, wrapping around. Probe
length (how many groups you visit) rises with the load factor: a table at 7/8 full
has long probe chains; a table at 1/2 full almost never leaves the home group. This
is the single behavior you cannot observe on the built-in map, because it exposes no
introspection at all. The exercise set builds a table with a `ProbeStats()` method
precisely so probe length becomes a number you can print and assert on, which is the
only honest way to build intuition for why load factor governs map latency.

### Growth: a directory of tables (extendible hashing)

A production Go map is not one Swiss Table. It is a directory of independent tables,
each capped near 1024 entries, with the upper hash bits selecting which table. When
one table fills, the runtime splits just that table into two rather than rehashing
the whole map. This bounds the worst-case latency of any single insert: you never pay
to rehash millions of entries at once, only to split one ~1024-entry table. For a
request path that shares a large map, that bounded per-insert cost is what keeps tail
latency flat. (The teaching table in this lesson is a single Swiss Table, not a
directory — enough to make probing and tombstones concrete without the directory
bookkeeping.)

### Deletion and tombstones

Deleting a key that sits in the middle of a probe chain cannot simply mark the slot
empty: a later key reached by probing *past* that slot would become unreachable if
the search stopped at the new hole. So deletion writes a tombstone (the deleted
state) instead of empty. A search treats a tombstone as "keep probing" but an insert
may reuse it. The consequence a senior must internalize: a map churned with
insert/delete accumulates tombstones, and tombstones are only reclaimed when the
table grows or rehashes. Until then they lengthen probe chains and occupy capacity.

### Memory: maps never shrink

This is the highest-value fact in the lesson. `delete(m, k)` removes the entry but
never returns capacity to the allocator; the backing tables stay allocated. Delete
every key and `len(m)` reads 0 while the memory footprint is unchanged. `clear(m)`
empties the map in one call but likewise keeps capacity. A map used as a bounded or
TTL cache, or as a per-request scratch buffer that grows huge on one pathological
request, therefore leaks: it holds its high-water-mark memory for as long as the map
is reachable. The only ways to reclaim are to drop the reference and assign a fresh
map (`m = make(map[K]V, keep)`), or to bound the map's lifetime (scope it per
request and let it be collected). For a real cache, use an eviction structure, not a
raw map you delete from.

### Presizing

`make(map[K]V, n)` allocates the directory and tables up front, so inserting `n`
entries never triggers a grow-and-rehash. When you know the size — a lookup table
built from a config file, a dedup set for a known batch — presizing is a measurable
win on the 1.24 implementation. The Swiss Tables blog post cites roughly 35% faster
assignment into presized maps, about 30% on large maps beyond 1024 entries, and
roughly 10% faster iteration (up to ~60% for sparsely loaded maps). Not presizing a
map you will fill to a known size is leaving that on the table for no reason.

### Measuring is genuinely hard

The Swiss Tables blog post is a lesson in humility: microbenchmarks showed up to 60%
improvements, but the full-application geomean was only about 1.5%, and some cases
regressed. Real numbers require the Go 1.24 `for b.Loop()` benchmark form, which
manages the timer automatically (no manual `ResetTimer`/`StopTimer`) and, critically,
keeps the compiler from deleting your benchmark body as dead code. The old
`for i := 0; i < b.N; i++` form with an unused result let the optimizer erase the
work, so the benchmark measured nothing and reported a fantastic number. Absolute
byte counts from `runtime.ReadMemStats` are equally treacherous across GOARCH and GC
settings; force a `runtime.GC()` before each read and compare only relative
inequalities, or use `testing.AllocsPerRun`, which is deterministic.

### What did not change

Iteration order is still randomized on purpose — a deliberate feature to stop code
from depending on order, not an accident of the old layout. The rewrite did not make
maps concurrency-safe: concurrent writes still trip the runtime's fatal "concurrent
map writes" detector, and you still reach for `sync.Map`, a sharded map, or a mutex.
During the 1.24 transition the change sat behind `GOEXPERIMENT=swissmap` (default on),
with `GOEXPERIMENT=noswissmap` reverting to the old map — a temporary escape hatch
useful for A/B verifying a suspected regression, and one that later releases remove.

### Hashing any comparable key: hash/maphash.Comparable

To rebuild a table you need a hash for arbitrary comparable keys. Go 1.24 added
`hash/maphash.Comparable[T comparable](seed Seed, v T) uint64`, which returns a
seeded, collision-resistant hash for any comparable type in one call — no reaching
for FNV or a third-party xxhash and no hand-rolling per-type. A fresh
`maphash.MakeSeed()` per table means two tables hash the same key differently, which
is exactly the property that makes hash-flooding attacks against a shared map fail.

## Common Mistakes

### Treating delete as freeing memory

Wrong: assuming a long-lived map you `delete` from returns to a small footprint. It
stays at its high-water mark forever because maps never shrink. Fix: rebuild the map
(`m = make(map[K]V, keep)`) or scope its lifetime; for a cache, use a real eviction
structure rather than a map you delete from.

### Not presizing a map whose size you know

Wrong: `make(map[K]V)` followed by inserting a known-large `n`, which triggers
repeated grow-and-rehash. Fix: `make(map[K]V, n)`. On the 1.24 implementation the
difference is a measurable, free win.

### Benchmarking with `for i := 0; i < b.N; i++` and discarding the result

Wrong: a `b.N` loop whose result is unused, which the compiler is free to delete,
making the benchmark measure nothing. Fix: use `for b.Loop()` (1.24), which prevents
dead-code elimination of the loop body and excludes setup/teardown from timing
without manual `ResetTimer`/`StopTimer`.

### Asserting absolute byte counts from ReadMemStats

Wrong: asserting `HeapAlloc` equals some literal number; it varies across GOARCH and
GC settings and flakes. Fix: call `runtime.GC()` before each `ReadMemStats`, and
assert only relative inequalities, or use `testing.AllocsPerRun` for a deterministic
allocation count.

### Depending on iteration order

Wrong: relying on map iteration order because the new layout "looks stable". It is
still randomized deliberately. Fix: sort keys when order matters —
`slices.Sorted(maps.Keys(m))`.

### Rolling your own hash table because "Swiss tables are faster"

Wrong: reimplementing a hash map for speed. The built-in map already is a Swiss
Table. The userland table in these exercises is a teaching artifact, not production
advice; only special cases (custom eviction policy, off-heap storage, an unusual key
type) justify rolling your own.

### Hand-rolling a key hash

Wrong: pulling in FNV or xxhash to hash a struct key. Fix:
`hash/maphash.Comparable[T]` (1.24) gives a seeded, collision-resistant hash for any
comparable type in one call.

### Assuming the rewrite made maps concurrency-safe

Wrong: sharing a map across goroutines that write, expecting the new implementation
to tolerate it. It does not; concurrent writes still panic with a fatal error. Fix:
`sync.Map`, a sharded map, or a mutex.

Next: [01-swiss-table-open-addressing.md](01-swiss-table-open-addressing.md)
