# Pointers in Slices and Maps — Concepts

Every in-memory backend store is a slice or a map of either values or pointers: a
job queue, a session cache, a per-tenant metrics registry, a delayed-retry
scheduler. That one choice — `[]*T` versus `[]T`, `map[string]*T` versus
`map[string]T` — is not a style preference. It silently decides your mutation
semantics (can a lookup mutate the stored object in place?), your defensive-copy
correctness (does the "snapshot" you hand an HTTP caller still alias internal
state?), your GC behavior (does `delete` actually free the object, or does an
ordering slice pin it forever?), and your race surface (do two goroutines mutate
one shared instance or two independent copies?). A senior engineer owns these as
operational properties, not as syntax. This file is the model; each of the ten
exercises that follow is a real artifact — a repository layer, a TTL cache, an
HTTP snapshot handler, a `sync.Map` registry, a `container/heap` scheduler — where
one of these properties is the whole point.

## The mutation-semantics decision

A `[]*T` stores addresses. The slice element *is* a pointer, so mutating `*s[i]`
is visible to every other holder of that same pointer — the queue, the caller who
added the job, a background worker that captured the handle. A `map[string]*T`
has the same property on the lookup path: `j := m[id]; j.Status = Running` reaches
through the pointer and changes the one canonical object. This is exactly what you
want for a mutable store with O(1) lookup: one instance per key, mutated in place.

A `[]T` (or `map[string]T`) stores values. Each holder gets an independent copy,
and `append` may reallocate the backing array and copy every element to a new
one. That independence is what you want when the return type is a contract that
says "you may not touch my internal state" — a `Snapshot() []T` hands out copies,
so a caller mutating `snap[0].Status` cannot corrupt the store. The return type is
the boundary of your data structure: return `[]T` (or a deep copy) when callers
must not mutate; return `[]*T` when they intentionally share and mutate.

## Non-addressable map values

A map value is not addressable, and this has a hard consequence people hit in
production. If `m` is `map[string]Session`, then

```
m[id].LastSeen = now   // does not compile: cannot assign to m[id].LastSeen
```

is rejected by the compiler, because `m[id]` yields a *copy* that has no stable
address to assign through. There are exactly two correct fixes. Either
read-modify-write the whole struct back:

```
s := m[id]; s.LastSeen = now; m[id] = s
```

or switch the map to `map[string]*Session` and mutate through the returned
pointer: `m[id].LastSeen = now` now compiles, because `m[id]` is a pointer and
`(*ptr).LastSeen` is addressable. The read-modify-write pattern is fine for small
structs and avoids the extra allocation and indirection; the pointer map is what
you reach for when many call sites mutate the same object or when the struct is
large.

## The nil zero value on a pointer map

A lookup on a pointer-valued map returns the zero value for a *missing* key, and
the zero value of `*T` is `nil`. So `r := m[id]` on a `map[string]*Record` gives
you a nil `*Record` when `id` is absent, and the next line that dereferences it —
a field access or a method call — panics with a nil-pointer dereference. This is a
cache-miss bug that only shows up on the cold path. Always read a pointer map with
the comma-ok form (`v, ok := m[id]`) and translate `!ok` into a sentinel error.
Where the zero case is meaningful, define it with nil-receiver-safe methods:
`func (r *Record) IsActive() bool { if r == nil { return false }; ... }` makes
"missing" a defined answer instead of a panic.

## Shallow clone is not a defensive copy

`slices.Clone` and `maps.Clone` are shallow, and their signatures say so:
`func Clone[S ~[]E, E any](s S) S` and
`func Clone[M ~map[K]V, K comparable, V any](m M) M` copy each element by simple
assignment. For a `[]*T` or a `map[string]*T`, "copy by assignment" copies the
*pointer*: the clone is a new slice header or map, but every element still points
at the same shared pointee. Mutating a `*T` through the clone mutates the
original. Worse, even for a `[]T` of *values*, if the struct contains a nested
reference type — a `Tags []string`, an `Attempts []Attempt`, an inner map — the
value copy duplicates the slice *header* but not the backing array, so
`snap[0].Tags[0] = "x"` writes straight through into the store's memory. A real
defensive snapshot must deep-copy every reference-typed field: copy the struct,
then `slices.Clone` each nested slice, `maps.Clone` each nested map, recursively.
"I called `maps.Clone`, so it's safe" is one of the most expensive false beliefs
in a concurrent read path.

## Interior pointers and reallocation

Taking `&slice[i]` gives you an *interior pointer* into the backing array. It is
valid only as long as that backing array is the live one. `append` past `cap`
allocates a *new* backing array, copies the elements, and repoints the slice
header at the new array — but any `&slice[i]` you captured before the grow still
points at the *old*, now-orphaned array. From that moment the captured pointer and
the live slice diverge silently: you mutate through `buf[0]` and the stale pointer
never sees it. This bites aggregation buffers that build an index of `&buf[key]`
into a growing slice. Two correct designs: reserve capacity up front with
`slices.Grow` so no reallocation happens while the interior pointers are alive,
or abandon interior pointers entirely and store `[]*T` where each `*T` is
independently allocated and stable regardless of how the outer slice grows.

## delete does not always free

`delete(m, k)` removes the map entry, but it does not free the pointee if that
same `*T` is still reachable from anywhere else — an ordering slice, an LRU list,
a secondary index. Under sustained insert/evict churn, an eviction that removes
from the map but leaves the pointer in a `[]*T` order slice grows that slice (and
retains every "evicted" object) without bound: an unbounded memory leak that no
`delete` count will reveal. Correct eviction removes the key from *every* structure
that references it, and nils out any lingering `[]*T` slot (`order[i] = nil`) so
the garbage collector can actually reclaim the object. The rule: a pointer is
freed only when it is unreachable from *all* live structures, not from one of them.

## sync.Map and shared pointer values

`sync.Map` is tuned for read-mostly, disjoint-key workloads — a per-tenant counter
registry, a per-key rate-limiter bucket store — where the alternative is a
`sync.RWMutex` around a plain map with heavy read contention. Its
`LoadOrStore(key, value) (actual any, loaded bool)` is the get-or-create primitive:
under a race of goroutines all trying to create key `k`, exactly one stored value
wins and every caller receives that *same* `actual`. The value must be a pointer
(`*Counter`), because that is what lets all the winners and losers mutate the one
shared instance — with `atomic.Int64` fields, so the shared mutation is race-free
without a per-entry lock. Storing a value (`Counter`) would give each goroutine a
copy of a snapshot and lose every increment but the last.

## container/heap needs pointers and an index

`container/heap.Interface` embeds `sort.Interface` (`Len`, `Less`, `Swap`) and adds
`Push(x any)` and `Pop() any`. A delayed-retry scheduler is a min-heap of tasks
ordered by `NextRunAt`. Store `[]*Task`, not `[]Task`, for two reasons. First, so a
caller can hold a live handle to a queued task and reschedule it. Second, so you
can maintain an `index` field on each `*Task`, updated inside `Swap`, which is what
makes O(log n) reprioritization possible: after mutating a task's `NextRunAt` in
place, `heap.Fix(pq, task.index)` restores the invariant in log time, versus an
O(n) scan to find it. `heap.Remove(pq, i)` deletes by position. A value heap
cannot support any of this — there is no stable handle and no `index` to fix
against.

## Modern Go idioms throughout

Go 1.22+ gives every loop iteration its own variable, so the old
`for _, v := range s { go func(){ use(v) }() }` aliasing bug is gone and you never
write `v := v`. Use `for i := range n` and `for i := range s`; use
`atomic.Int64.Add` over the `sync/atomic` free functions for pointer-held counters;
use `t.Context()` in tests. These exercises assume `go 1.24`+ semantics.

## Common Mistakes

### Assigning to a field of a value-map element

Wrong: `m[id].Status = StatusRunning` where `m` is `map[string]Job`. The compiler
rejects it because `m[id]` is not addressable. Fix: store `map[string]*Job` and
mutate through the pointer, or read-modify-write the whole struct back
(`s := m[id]; s.Status = StatusRunning; m[id] = s`).

### Trusting slices.Clone / maps.Clone as a defensive copy

Wrong: returning `maps.Clone(byID)` (a `map[string]*Job`) or `slices.Clone(items)`
(a `[]*Job`), or a `[]Job` whose elements have a nested `Tags []string`, and
calling it a safe snapshot. The clone shares every pointee and every nested slice
header, so mutations leak across the "copy". Fix: deep-copy every reference-typed
field explicitly.

### Mutating a per-iteration copy of a value slice

Wrong: `for _, j := range jobs { j.Status = StatusFailed }` on a `[]Job`. `j` is a
copy each iteration and the write is discarded; the slice is unchanged. Fix:
`for i := range jobs { jobs[i].Status = StatusFailed }`, or take `&jobs[i]`.

### Capturing &slice[i] then appending

Wrong: `p := &buf[0]` and then `buf = append(buf, more...)` past `cap`. The
reallocation leaves `p` dangling on the old backing array, diverging from live
data. Fix: `slices.Grow` to reserve capacity before taking interior pointers, or
store independently allocated `*T`.

### Dereferencing a missing pointer-map key

Wrong: `r := m[id]; if r.Active { ... }` where `m` is `map[string]*Record` and
`id` is absent — `r` is nil and the field access panics. Fix:
`r, ok := m[id]; if !ok { return ErrNotFound }`, and add nil-receiver-safe methods.

### Deleting from the map but not the ordering slice

Wrong: `delete(byID, id)` while the same `*Entry` is still referenced by a
`[]*Entry` order slice. The object is never collected; under churn the order slice
grows without bound. Fix: remove from all structures and nil out the vacated slot
so GC can reclaim.

### Returning []*T when the caller must not mutate

Wrong: a store method returning `[]*Job` when the caller only needs to read. The
caller edits internal jobs through the pointers and breaks your invariants. Fix:
return `[]Job` values, or a deep copy.

### Filling a []*T with one repeated address

Wrong: appending `&tmp` in a loop where `tmp` is one reused variable (or, pre-1.22,
`&loopVar`): every entry points at one object. Fix: allocate a distinct `*T` per
element, or take `&slice[i]` on a stable, pre-grown slice.

### A value heap with no handle to reprioritize

Wrong: storing `[]Task` in a `container/heap` and expecting to reschedule an entry:
there is no stable pointer and no `index` field, so no O(log n) `heap.Fix` path and
callers hold stale copies. Fix: store `[]*Task` with a maintained `index` field.

Next: [01-job-queue-pointer-store.md](01-job-queue-pointer-store.md)
