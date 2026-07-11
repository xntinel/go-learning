# sync.Map: Concurrent Maps for Stable Keys and Disjoint Key Sets

A plain Go `map` is not safe for concurrent use. `sync.Map` is the standard
library's specialized concurrent map, and the senior skill is not memorizing its
API but knowing the two production shapes it was built for, the handful of real
backend idioms it unlocks, and — most of the time — deciding to reach for
`map` plus `sync.RWMutex` instead. This file is the conceptual foundation for the
ten independent modules that follow. Read it once and you have the model you need
to reason through every one of them: a per-URL visit counter, a typed registry, a
generic wrapper, a per-tenant connection registry, a metrics registry, a session
store with an expiry sweeper, a versioned config store, a credential hot-swap, an
idempotency guard, and the benchmark that decides the whole question.

## Concepts

### Why a plain map is unsafe, and why the failure is a fatal crash

A Go `map` is an ordinary in-memory hash table with no internal synchronization.
Reading it while another goroutine writes it is a data race not merely on your
values but on the runtime's own bucket pointers, tophash bytes, and grow state.
A concurrent write during a rehash can leave those internal structures
inconsistent in a way the runtime cannot recover from. Because this class of bug
silently corrupts memory and is nearly impossible to debug after the fact, the Go
runtime ships a lightweight concurrent-map-access detector: when it observes a map
being written while another goroutine accesses it, it calls `throw` with
`fatal error: concurrent map read and map write` (or `concurrent map writes`).
That is a *fatal* error, not a `panic` — it is unrecoverable, `recover()` does
nothing, and the whole process dies with a stack dump. The detector is
best-effort (it does not catch every race), so `-race` remains the real tool for
finding these, but the takeaway is blunt: a shared map with any concurrent write
must be protected, and "it worked in testing" is not evidence of safety. This is
the problem `sync.Map` and `map`+`RWMutex` both exist to solve.

### The two patterns sync.Map is optimized for

The package documentation is precise and worth taking literally. `sync.Map` is
optimized for exactly two use cases:

1. **Stable keys**: an entry for a given key is written once and then read many
   times — a cache that only grows, a service-discovery table populated as peers
   register, a memoized lookup table, a registry built at startup.
2. **Disjoint key sets**: multiple goroutines read, write, and overwrite entries
   for keys that do not overlap. Sharded workers each own a slice of the keyspace
   and rarely touch each other's keys.

In those two cases `sync.Map` can significantly reduce lock contention versus a
single mutex-guarded map. For everything else — frequent inserts and deletes
spread across the full keyspace, or a workload where you also need a cheap length
or a typed value — `map` plus `sync.RWMutex` is faster, more idiomatic, and gives
you a concrete value type instead of `any`. The documentation itself says most
code should prefer a plain map with separate locking. Treat `sync.Map` as a
specialist you deploy on evidence, not a default.

### Internal model, and why you must not depend on it

The original `sync.Map` (Go 1.9 through 1.23) was a read-mostly design: a
lock-free atomic "read" map fronting a mutex-guarded "dirty" map, with entries
amortized-promoted from dirty to read once they had been missed enough times.
That model explained why reads of stable keys were cheap and why churny writes
were expensive. Go 1.24 replaced the whole implementation with a `HashTrieMap`
(a concurrent hash-trie) that scales writes and deletes far better. The crucial
point for a senior engineer: the public API and the memory-model guarantees are
identical across that change, but the old "read path is lock-free, dirty
promotion" mental model is now an implementation detail, not a contract. Do not
design around internals that can change between releases. Decide on measured
behavior for the toolchain you actually ship, not on a blog post about how the
map worked three versions ago.

### LoadOrStore is the atomic check-and-set

`LoadOrStore(key, value) (actual, loaded)` is the primitive the interesting
idioms are built on. If the key is absent it stores `value` and returns
`(value, false)`; if present it leaves the map untouched and returns
`(existing, true)`. The check and the store are one atomic step: no goroutine can
observe the key as absent and then have another goroutine insert underneath it.
That single guarantee is what makes three canonical backend patterns safe:

- **Lock-free per-key counter**: `LoadOrStore(key, &atomic.Int64{})` gives every
  caller the *same* `*atomic.Int64`; they increment through the atomic, never
  through the map.
- **Thundering-herd-safe lazy init**: `LoadOrStore(key, &sync.Once{})` gives every
  caller the same `*sync.Once`, and `once.Do(build)` runs the expensive
  construction exactly once no matter how many goroutines arrive cold at the same
  moment.
- **Idempotency / singleflight**: `LoadOrStore(key, newCall())` where the call
  holds a `done` channel lets the one goroutine that inserted run the work while
  duplicates block on `done` and read the shared result.

### The typed-pointer idiom

Because `sync.Map` values are `any`, mutating a value you stored means either
re-`Store`ing it (racy and wasteful) or — the idiom — storing a **pointer** to a
mutable, concurrency-safe object and mutating *through the pointer*. `LoadOrStore`
a `*atomic.Int64` and every caller shares one atomic; the increment is lock-free
and the map is never touched again for that key. This is why the visit counter
stores `&atomic.Int64{}` rather than an `int64`: the map holds identity, the
atomic holds the mutable count. Store a value type and mutate a copy of it and you
have a silent lost-update bug.

### The full method set and its versions

- `Load`, `Store`, `LoadOrStore`, `Delete`, `Range` — original (Go 1.9).
- `LoadAndDelete(key) (value, loaded)` — Go 1.15: atomic remove-and-return. The
  sweeper idiom depends on it: it lets a background evictor delete a key *and*
  see the value it removed in one step, so it can skip an entry a writer just
  refreshed instead of clobbering it the way a blind `Delete` would.
- `Swap(key, value) (previous, loaded)` — Go 1.20: atomic replace-and-return, the
  hot-swap/rotation primitive; you install the new value and get the old one back
  for audit or zeroization while readers never see a torn state.
- `CompareAndSwap(key, old, new) (swapped)` and
  `CompareAndDelete(key, old) (deleted)` — Go 1.20: optimistic-concurrency
  updates. A stale writer whose `old` no longer matches fails cleanly instead of
  overwriting a newer value. Both compare with interface equality, so the value
  type **must be comparable** (no slices, maps, or funcs) or the call panics.
- `Clear()` — Go 1.23. There is deliberately no `Len`.

### Range is best-effort, not a snapshot

`Range(func(key, value any) bool)` visits each key at most once and stops early if
the callback returns `false`. It is emphatically **not** a consistent snapshot:
concurrent `Store`/`Delete` during iteration may or may not be observed, key order
is unspecified, and a key can appear with a value from any instant during the
call. That makes `Range` the correct primitive for best-effort enumeration and
scraping — walking every counter to export a `/metrics` page, sweeping expired
sessions — and the wrong primitive for anything that assumes "no writes happened
while I iterated". When you need a stable view, copy entries out (into a fresh map
or slice) as you range; the copy is what makes the exported view usable, and
because each counter is itself an atomic, each copied value is a valid snapshot of
that counter even though the set of counters is only eventually consistent.

### No cheap length

There is no `Len` method. Counting via `Range` is O(n) and, under concurrency,
only a best-effort number that may include a key being deleted or miss one being
inserted. If you need an accurate size cheaply, maintain a separate
`atomic.Int64` alongside the map, or use `map`+`RWMutex` where `len()` is O(1) and
exact. Reaching for `Range` to compute a hot-path length is a design smell.

### The memory-model guarantee, and what it does not cover

`sync.Map` operations establish happens-before edges just like the other sync
primitives: a value observed via `Load` happens-after the `Store` that put it
there, `LoadOrStore` is a write when it stores, `CompareAndSwap` is a write when
it swaps, and so on. So you never need extra synchronization to *publish* a value
through the map. What the map does **not** synchronize is subsequent mutation of
the object it points at: if two goroutines share a `*atomic.Int64` fetched from
the map, the atomicity of their increments comes from `atomic.Int64`, not from
`sync.Map`. Publish through the map, mutate through an atomic (or a mutex the
pointed-at object owns).

### Never copy a sync.Map

`sync.Map` embeds internal state and must not be copied after first use. Copying
it — passing it by value, or embedding it in a struct you then pass or return by
value — duplicates that state and breaks correctness. Always store and pass it by
pointer, and embed it in a struct you also handle by pointer. `go vet`'s
`copylocks` pass catches many of these but not all; do not rely on the vet catch
in place of the discipline.

### The any-boxing cost is real

Every value you put into a `sync.Map` crosses the `any` interface. For pointer
values that is cheap (the pointer is the interface's data word), but for non-
pointer value types it can force a heap allocation to box the value, plus an
indirection on every read. That allocation is a concrete, measurable reason
`map`+`RWMutex` with a concrete value type can beat `sync.Map` on write-heavy or
allocation-sensitive paths. When you benchmark the decision, run `-benchmem`: the
allocs/op column is where the boxing shows up, and it is often the deciding
number.

## Common Mistakes

### Reaching for sync.Map as the default concurrent map

Wrong: use `sync.Map` any time a map is shared between goroutines. For
read/write/delete-heavy workloads over the full keyspace it is slower than
`map`+`sync.RWMutex` because of `any`-boxing and internal indirection, and you
lose type safety and a cheap `len`. Fix: default to `map`+`RWMutex`; reach for
`sync.Map` only for the two documented patterns (stable keys, disjoint key sets),
and confirm the choice with a benchmark on your access profile.

### Treating Range as a consistent snapshot

Wrong: assume `Range` gives a stable view — that no writes happened during
iteration, or that keys arrive in some order. Concurrent writes can appear or not,
mid-update, in unspecified order. Fix: copy entries out as you range for a
snapshot, or design the consumer for eventual-consistency scraping.

### Storing a mutable container by value and mutating the copy

Wrong: `actual, _ := m.LoadOrStore(k, []int{})` then
`actual.([]int) = append(actual.([]int), x)`. Two goroutines share the same slice
header and array; `append` may reallocate and updates are silently lost. Fix:
store a pointer to a mutable container (`*[]int`, `*bytes.Buffer`) or an atomic,
and mutate through the pointer.

### Type-asserting Load's result without comma-ok

Wrong: `v := m.Load(k).(int)`. When the key is absent `Load` returns `(nil,
false)` and the single-result assertion panics. Fix: always use the two-result
form `v, ok := m.Load(k)` and handle the miss, or wrap the map in a generic typed
map so the assertion is impossible to get wrong at the call site.

### Building the expensive object before LoadOrStore

Wrong: `m.LoadOrStore(k, newExpensiveClient())`. Go evaluates the argument first,
so `newExpensiveClient()` runs on *every* call even when the key already exists —
the exact duplicate-open bug you were trying to prevent. Fix: `LoadOrStore` a
cheap `*sync.Once` (or a small lazy-entry struct) and do the expensive build
inside `once.Do`, so construction happens exactly once.

### Copying a sync.Map

Wrong: passing a `sync.Map` by value, or embedding it in a struct you pass or
return by value. That duplicates internal state and corrupts behavior. Fix: pass
by pointer, embed in a pointer-handled struct, and let `go vet copylocks` help
without relying on it to catch every case.

### Calling CompareAndSwap or CompareAndDelete on a non-comparable value

Wrong: using CAS with a slice, map, or func value. Interface equality panics on
those types, so the call panics at runtime. Fix: only use the CAS methods with
comparable value types, or wrap the value in a comparable pointer or struct.

### Assuming Delete during Range is unsafe

Wrong: avoiding `Delete` inside `Range` for fear of a crash. Both are
concurrency-safe, so it is fine — but a deleted key may or may not still be
visited, and a blind `Delete` in a sweeper can drop a value a writer refreshed a
microsecond earlier. Fix: for a remove-and-return sweep use `LoadAndDelete` and
inspect the returned value, so you only act on the version you actually removed.

### Expecting a cheap Len

Wrong: computing size on a hot path by ranging the map. There is no `Len`, and
ranging is O(n) and only best-effort under concurrency. Fix: keep a separate
`atomic.Int64` counter, or use `map`+`RWMutex` with `len()`.

Next: [01-visit-counter.md](01-visit-counter.md)
