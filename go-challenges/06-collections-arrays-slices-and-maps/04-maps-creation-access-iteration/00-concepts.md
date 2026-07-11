# Maps in Production: Creation, Access, Iteration, and the Failure Modes That Bite — Concepts

The map is the workhorse of every Go backend: the cache, the rate-limiter's
per-client counter, the CORS-origin allowlist, the inverted index behind a
search box, the per-tenant metrics table, the idempotency guard that stops a
webhook from being processed twice. It is also the single richest source of
production incidents in the language, because its convenient surface hides four
sharp edges that only cut at scale: the first write to a nil map panics, a
single unsynchronized concurrent write crashes the whole process with a fatal
that `recover` cannot catch, iteration order is deliberately randomized so any
code that leaks it into output is nondeterministic, and `maps.Clone` is shallow
so a defensive copy silently shares nested state. A senior engineer has to know
these cold, plus the modern Go 1.23+ toolkit (`maps.Clone/Equal/Copy/DeleteFunc`,
the `maps.Keys/Values/All` iterators, `slices.Sorted/SortedFunc`, `cmp.Or` for
tie-breaks) that replaces a decade of hand-rolled loops. This file is the
conceptual foundation; read it once and you have everything you need to reason
through each of the independent exercises that follow.

## Concepts

### A map is a reference to a runtime hash table

A map value is a small header (a pointer to a runtime `hmap` struct) that points
at the backing hash table. Assigning a map to another variable, or passing it to
a function, copies only the header — both names share the same table. A mutation
through one name is visible through every other name:

```go
func add(m map[string]int) { m["x"]++ } // caller sees the change
```

This is why you almost never take a `*map` parameter: the map already behaves
like a reference. The flip side is that there is no free isolation. If a handler
stashes a map in a struct and hands the same map to a caller, the caller can
mutate your internal state. Defensive copying (`maps.Clone`, below) is the fix,
with the shallow-copy caveat noted later.

Because all holders share one table, a map is **not safe for concurrent
read/write**. This is not a soft "might see stale data" hazard: the runtime
actively detects a concurrent write racing another access and calls
`fatal("concurrent map writes")`, which terminates the process. That path is not
a `panic` — `recover` cannot catch it, no deferred cleanup runs, the process is
gone. Guard shared maps with a `sync.RWMutex`, or reach for `sync.Map` when the
workload is the specific shape it is tuned for (many goroutines, keys written
once and read many times, or disjoint key sets per goroutine).

### The nil map: safe reads, panicking writes

The zero value of a map type is `nil`, not an empty map. A nil map is genuinely
usable for reads: `v, ok := m[k]` returns the value type's zero and `ok == false`,
`len(m)` is `0`, and `for range m` runs zero iterations. All of that is defined
and safe. The one thing you may not do is write: `m[k] = v` on a nil map panics
with `assignment to entry in nil map`.

The trap is that a struct field of map type starts nil, silently. A config
loader with a `Sections map[string]string` field that never runs a constructor
will read fine in every test that only reads, then panic the first time
production data triggers a write. The fixes are a constructor that `make`s the
map, or a lazy guard (`if m == nil { m = make(...) }`) immediately before the
first write. Note one benign case that hides the bug: `json.Unmarshal` into a
nil map field *works*, because the decoder allocates the map for you — so the
loader can look correct until a code path writes to the field directly.

### Comma-ok: distinguishing absent from zero

`v, ok := m[k]` is the only way to tell "the key is absent" from "the key is
present holding the zero value". This is load-bearing precisely when the zero
value is a legitimate stored value: a counter at `0`, a feature flag at `false`,
a cached empty string, a nil slice. Reading `m[k]` without the `ok` and treating
the returned zero as "the stored value" conflates the two and corrupts counters,
flag logic, and cache-hit accounting. The rule: whenever zero is a value you
might legitimately store, use comma-ok; never infer absence from a zero result.

A useful corollary: `m[k]++` on an absent key is correct and idiomatic. The read
yields the zero (`0`), the increment makes it `1`, and the write stores it. You
do not pre-initialize counters. The same works for `m[k] = append(m[k], v)` — the
read of an absent key yields a nil slice, and `append` to a nil slice allocates a
fresh one. Both are why frequency maps and grouping maps need no setup per key.

### Iteration order is deliberately randomized

`for k, v := range m` visits entries in an unspecified order, and the runtime
seeds that order randomly at each range start — even two consecutive ranges over
the same unmodified map can differ. This is a deliberate design decision (since
Go 1.0) to stop code from accidentally depending on an order that was never
promised. The consequence for backends is concrete: any output derived from a map
range — a serialized JSON object built by appending, a hash computed over
entries, a log line, a test golden — is nondeterministic and will produce
flapping diffs and CI flakes. The fix is one line: collect the keys and sort
them. In modern Go that is `slices.Sorted(maps.Keys(m))`, which returns the keys
as a sorted slice; range that slice, not the map.

### Key types must be comparable

A map key type must be comparable with `==`. Comparable types are booleans, all
numeric types, strings, pointers, channels, interfaces, and — crucially —
structs and arrays whose fields/elements are *all* comparable. Slices, maps, and
functions are **not** comparable and fail to compile as a key type (and as a
struct field of a struct used as a key). This is a compile-time guarantee, not a
runtime check.

Comparable struct keys are a design tool, not a limitation. A composite key like
`struct{ Tenant, RequestID string }` gives you an O(1) lookup keyed by a tuple,
with value semantics: two keys are equal exactly when all fields are equal. This
is the right shape for idempotency guards, deduplication, and any "seen this
(a, b) pair?" question. The wrong workaround, reached for by engineers who hit
the non-comparable-slice error, is to `fmt.Sprintf("%v-%v", a, b)` a string key —
slower, allocation-heavy, and prone to delimiter-collision bugs. Design a
comparable struct key instead.

### Map values are not addressable

You cannot take the address of a map element, and therefore you cannot assign to
a field of a struct-valued map element: `m[k].Field = x` does not compile
(`cannot assign to struct field m[k].Field in map`). The reason is that the
runtime may move entries as the table grows, so a pointer into the table would be
invalid; Go forbids the address entirely rather than let you hold a dangling one.

There are two correct fixes, and choosing between them is a real design decision.
Read-modify-write copies the value out, mutates the copy, and writes it back:
`v := m[k]; v.F++; m[k] = v`. It keeps value semantics (each entry is independent,
no aliasing) at the cost of copying the struct on every update. Storing pointers
(`map[K]*T`) lets you mutate in place — `m[k].F++` compiles and works because the
element is a pointer and you are dereferencing it — but it introduces aliasing
(two names for the same struct) and keeps the pointee alive as long as the map
holds it, which matters for GC in long-lived services. Prefer value read-modify-
write for small structs and independent entries; prefer pointers when the struct
is large, mutated frequently, or legitimately shared.

### The modern maps and slices packages

Since Go 1.21 the `maps` package ships `Clone`, `Equal`, `Copy`, and `DeleteFunc`;
Go 1.23 added the iterator forms `Keys`, `Values`, and `All` (returning
`iter.Seq[K]`, `iter.Seq[V]`, and `iter.Seq2[K, V]`). Paired with
`slices.Sorted`, `slices.SortedFunc`, and `slices.Collect`, they replace the
hand-rolled key-collection loops that used to litter every codebase.
`maps.Clone(m)` returns an independent copy (and `Clone(nil)` returns nil).
`maps.Equal(a, b)` reports whether two maps have the same key/value pairs.
`maps.Copy(dst, src)` overlays `src` onto `dst`, later keys winning — the layering
primitive for config. `maps.DeleteFunc(m, pred)` prunes by predicate in place.

The single most important caveat: **`maps.Clone` is a shallow copy**. If the
value type is a slice, a map, or a pointer, the clone shares that nested state
with the original. Mutating a nested slice through the clone corrupts the
original, which defeats the entire point of the defensive copy. When values are
themselves reference types, you must deep-copy them yourself.

### Sets: map[T]struct{} vs map[T]bool

Go has no built-in set, and the idiom is `map[T]struct{}`. The empty struct
`struct{}` occupies zero bytes, so a set of a million strings pays only for the
keys, not for a million values. Membership is `_, ok := set[v]`; insertion is
`set[v] = struct{}{}`. Reach for `map[T]bool` only when you genuinely need to
store a real true/false distinction *per key* (present-and-true vs
present-and-false), which is rare — and note that storing `false` in a set-style
`map[T]bool` makes `Contains` ambiguous, a classic bug. For a plain allowlist,
denylist, or "have I seen this ID" set, `map[T]struct{}` is the correct type.

### Delete, insert, and pruning during iteration

`delete(m, k)` removes the entry for `k`, is a no-op when `k` is absent, and has
no return value. Deleting during a range is allowed and well-defined: if the
entry has not yet been produced by the iterator, it will not be. Inserting during
a range is also allowed, but a newly inserted entry *may or may not* be produced
by the ongoing range — do not rely on either outcome. For predicate-based
pruning, `maps.DeleteFunc(m, func(k K, v V) bool { ... })` is cleaner and correct
than a hand-written delete-while-ranging loop.

### Maps do not shrink; memory and long-lived servers

A map grows its backing table as entries are added, but it does **not** shrink
that table when entries are deleted. A map that ballooned to a million entries
and was then emptied with `delete` still holds the allocation for a million
entries. In a long-lived server this is steady-state memory bloat: a cache or
buffer that spikes and drains keeps the high-water-mark memory forever. The fixes
are to reassign a fresh map (`m = make(map[K]V)`) when you truly want to reclaim,
or to use a bounded structure with an eviction policy so the map never grows
unbounded in the first place. This is a real cause of "why does my service RSS
never come back down" investigations.

### The aggregation pattern: map accumulator, sorted slice view

The canonical shape for counting, grouping, and ranking is: use a map as the
O(1) accumulator, then materialize a slice and sort it for the deterministic,
ordered view. Count status codes into `map[int]int`, then build a slice of
(code, count) pairs and sort by count with `slices.SortFunc` or `slices.SortedFunc`,
breaking ties by key with `cmp.Or(cmp.Compare(a.count, b.count), ...)` so the
Top-N is reproducible in tests and dashboards. The map is the accumulator; the
slice is the ordered report. Never try to produce ordered output from the map
directly — see the iteration-order section.

### Nested maps vs composite struct keys

Two-level data — per-tenant, per-endpoint counters — can be modeled as a nested
`map[string]map[string]int64` or as a flat `map[struct{ Tenant, Endpoint string }]int64`.
The nested form requires lazy initialization of the inner map on first touch
(`if m[t] == nil { m[t] = make(...) }` before `m[t][e]++`), and forgetting it
reproduces the nil-map write panic on the inner map. The composite-key form is
flatter, allocates one table instead of N+1, and cannot hit the nil-inner-map
trap. Prefer the composite struct key unless you frequently need to operate on a
whole inner group at once (enumerate all endpoints for one tenant), which the
nested form makes cheap.

## Common Mistakes

### Writing to a nil map

Wrong: `m[k] = v` where `m` was never `make`d, or is a zero-value struct field.
Panics with `assignment to entry in nil map`.

Fix: initialize in a constructor, or lazy-guard with `if m == nil { m = make(map[K]V) }`
immediately before the first write.

### Ignoring the ok in comma-ok

Wrong: `v := m[k]` and treating the returned zero as "the stored value",
conflating absent with present-and-zero. Corrupts counters, flags, and cache-hit
logic whenever zero is a legitimate stored value.

Fix: `v, ok := m[k]` and branch on `ok`.

### Concurrent map access without a lock

Wrong: two goroutines writing (or one writing while another reads) the same map
with no synchronization. The runtime calls `fatal("concurrent map writes")` and
the process dies; `recover` cannot catch it.

Fix: guard with `sync.RWMutex`, or use `sync.Map` for its intended workload.

### Depending on range order for output

Wrong: building serialized output, a hash, or a test golden by ranging a map.
Passes locally, produces nondeterministic diffs and flaky CI.

Fix: `for _, k := range slices.Sorted(maps.Keys(m))` and range the sorted keys.

### Assigning to a map value's field

Wrong: `m[k].Field = x`, confused by `cannot assign to struct field ... in map`.
Map values are not addressable.

Fix: read-modify-write (`v := m[k]; v.F = x; m[k] = v`) or store pointers
(`map[K]*T`).

### Assuming maps.Clone deep-copies

Wrong: `maps.Clone(m)` and then mutating a nested slice/map/pointer in the clone,
expecting the original untouched. The clone is shallow; nested state is shared.

Fix: deep-copy the reference-typed values yourself after `Clone`.

### Non-comparable key types and stringified workarounds

Wrong: a struct-with-a-slice-field key (compile error), or reaching for
`fmt.Sprintf`-stringified composite keys to dodge it.

Fix: design a comparable struct key (all fields comparable).

### Expecting an emptied map to release memory

Wrong: `delete`-ing every entry from a huge map and expecting RSS to drop. The
backing table does not shrink.

Fix: reassign `m = make(map[K]V)` to reclaim, or bound the map with eviction.

### map[T]bool for a set, then storing false

Wrong: a set built on `map[T]bool` where some entries are `false`; `Contains`
becomes ambiguous.

Fix: use `map[T]struct{}`; membership is presence of the key.

### Writing m[a][b] without initializing the inner map

Wrong: `m[a][b] = v` on a nested map when `m[a]` is nil. Panics on the nil inner
map.

Fix: `if m[a] == nil { m[a] = make(map[B]V) }` before the inner write, or use a
composite struct key.

### Picking "the first" element of a map

Wrong: ranging a map and `break`ing to grab "an" element, expecting a stable
choice. Iteration is randomized; there is no first.

Fix: sort keys and take `keys[0]`, or redesign so no arbitrary choice is needed.

### Sleep-based tests for TTL and window behavior

Wrong: `time.Sleep`-ing past a TTL or rate-limit window to test expiry. Slow and
flaky under CI load.

Fix: inject a clock (`now func() time.Time` field) so time is deterministic, or
test time-dependent logic under `testing/synctest`.

Next: [01-ttl-cache.md](01-ttl-cache.md)
