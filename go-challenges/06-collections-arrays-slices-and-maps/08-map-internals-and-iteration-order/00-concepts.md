# Map Internals, Iteration Order, and Concurrency-Safe Maps — Concepts

The map is the most-used data structure in a Go backend. Request counters, in-memory
indexes and caches, layered configuration, dedup and allowlist sets, metric label
sets — almost every service is a few maps under load. That ubiquity is exactly why
every one of a map's failure modes is a real incident: a `/report` endpoint whose
output reorders on every scrape and breaks golden diffs; a `fatal error: concurrent
map writes` that crashes the whole process (not a recoverable panic, not merely a
`-race` warning); a nil-map write panic in a config path that only fires when one
layer is absent; an in-memory index that thrashes through repeated grow-and-rehash
because it was built with no capacity hint; a cache that leaks because entries are
never pruned, or corrupts because entries are added during a `range`. This file is
the conceptual foundation for the ten independent exercises that follow. Read it once
and you can reason through all of them.

## Concepts

### The Go 1.24 map is a Swiss table

Through Go 1.23 a map was a classic open-hashing table: an array of buckets, each
holding up to 8 key/value pairs, chained to overflow buckets when a bucket filled.
Go 1.24 replaced the runtime map with a *Swiss table* design (the same family Abseil
popularized in C++), and understanding its shape is what lets you reason about cache
behavior, load factor, and iteration cost instead of guessing.

The unit is a *group* of 8 slots. Alongside each group sits a 64-bit *control word*:
one byte per slot, and that byte stores a 7-bit hash fragment (call it `h2`, the low
bits of the key's hash) plus a marker for empty/deleted. A lookup hashes the key
once, uses the high bits to pick a group, then scans that group's control word for
bytes matching `h2` — and because the control word is a single 64-bit value, the
match scan is a handful of word-at-a-time bit operations rather than eight separate
comparisons. Only slots whose `h2` matches are then compared by full key. The effect
is that most negative lookups (key absent) are resolved without a single full key
comparison, which is where the speedup comes from on read-heavy workloads.

A single Swiss table holds up to a bounded number of groups. To scale beyond that,
Go uses *extendible hashing*: a directory of pointers to independent tables, each
table capped at 1024 group-slots. When one table gets too full it splits, and only
that table is rehashed — not the whole map. That is what makes growth *incremental*:
inserts have a bounded worst-case latency because a single oversized insert never has
to rehash millions of entries at once. The old design doubled the whole bucket array
and migrated it a bit at a time; the Swiss design splits one 1024-entry table at a
time. For a backend this matters under load: p99 insert latency into a large,
growing index is bounded rather than spiking on the one unlucky insert that triggers
a full-table rehash.

Load factor is why presizing pays. A table grows (splits) when it crosses a fill
threshold. If you build a map from a slice of known length without a size hint, you
pay for several splits and rehashes as it grows through the thresholds; `make(map[K]V,
n)` allocates enough tables up front so the build inserts into a table that never has
to split. The `prealloc-index-build` exercise measures this with a benchmark rather
than asserting it as folklore.

### Iteration order is randomized on purpose

`for k, v := range m` visits entries in an order that is deliberately randomized: the
runtime picks a random starting group (and a random offset within it) for every
`range` statement, on every call. This is not an accident of the hash — it is code
that exists specifically to break your dependence on order. The motivation is
Hyrum's law: if iteration were stable, some code would come to rely on the observed
order, and that code would silently break when the map grew, when the Go version
changed, or when a key was added. By randomizing, the runtime surfaces the bug on
your machine on the second run instead of in production a year later.

Three consequences a senior engineer internalizes. First, order is randomized *per
range call*, not per map: two ranges over the same unchanged map can differ, so you
can never memoize an order observed from a previous range. Second, there is no
"consistent within one run" guarantee either. Third, the moment output must be
deterministic — a report, a log line, a JSON list, a metrics exposition, a golden
test — you must collect the keys, sort them, and iterate the sorted keys. The modern
idiom is `slices.Sorted(maps.Keys(m))` for a plain sorted-key list, or
`slices.SortedFunc(maps.Keys(m), cmp)` for a custom order. `maps.Keys` returns an
`iter.Seq[K]` (Go 1.23 range-over-function iterators); `slices.Sorted` collects and
sorts it in one call.

### A map is a reference; its zero value is nil

A `map[K]V` value is a small header pointing at the table. Passing a map to a
function does not copy the entries — the callee sees and mutates the same table. That
is why a `Snapshot` accessor must return `maps.Clone(m)` (an independent copy), not
the internal map: returning the live map lets callers mutate your shared state, and,
worse, iterate it while you write and observe a torn or crashing read.

The zero value of a map is `nil`. Reading a nil map is safe and returns the zero
value (`v := m[k]` gives the zero `V`; `v, ok := m[k]` gives `zero, false`). *Writing*
a nil map panics: `var m map[string]int; m["x"] = 1` is a runtime panic. This asymmetry
is a real config-path incident: a struct field or a config layer left at its zero
value reads fine in every test that only reads it, then panics the first time
production takes the write path. The fix is always `make(map[K]V)` (or a constructor)
before the first write.

### Presence vs zero value: comma-ok

`v := m[k]` cannot distinguish "key absent" from "key present with the zero value".
A counter of 0, an empty-string config value, a `false` flag — all read back as the
zero value whether the key was set or not. The two-result form `v, ok := m[k]` returns
`ok == true` only when the key is present. Any code that must treat "explicitly set to
empty" differently from "never set" — a config layer that can *override a default back
to empty*, a cache that stores a legitimately-zero value — must branch on `ok`, not on
`v == zero`.

### Concurrency: a data race here is a process-killer

A plain map is not safe for concurrent use. This is stronger than the usual data-race
story. The runtime has built-in concurrent-access detection: if it observes a write
racing with any other access, it does not corrupt silently and it does not merely warn
under `-race` — it calls `fatal error: concurrent map writes` (or `concurrent map
iteration and map write`) and *kills the whole process*. This is not a `recover`-able
panic; a single buggy handler goroutine takes down every in-flight request in the
process. So concurrent map safety is not a nicety you add when `-race` complains — a
shared-writer map is a latent crash, and `-race` in tests is how you find it before
production does.

There are four options, and choosing between them is the senior decision:

- **Plain map** — only when a single goroutine owns it, or it is built once and then
  read-only forever (frozen after init, no further writes).
- **`RWMutex` + map** — the default for shared read/write. Reads take `RLock` (many
  concurrent readers), writes take `Lock`. Simple, predictable, and the right answer
  far more often than folklore suggests. A `Snapshot` clones under the read lock.
- **`sync.Map`** — a concurrent map tuned for two specific workloads: (a) read-mostly,
  write-once keys (a key is written once then read many times), and (b) disjoint key
  sets across goroutines. It uses `any` for keys and values, so it costs boxing and
  type assertions, and it is *slower* than `RWMutex`+map under write-heavy or
  overlapping-key contention. Its API is `Load`, `Store`, `LoadOrStore` (idempotent
  lazy init), `Range` (snapshot-ish, no order guarantee), `CompareAndDelete`,
  `CompareAndSwap`.
- **Sharded map** — an array of `N` `RWMutex`+map shards keyed by `hash(key) % N`, to
  spread lock contention across shards. Worth it only when a single lock is measurably
  the bottleneck.

The rule threaded through the concurrency exercises: default to `RWMutex`+map; reach
for `sync.Map` only for a *measured* read-mostly, disjoint-key workload; and defend
the choice with `-race` plus a benchmark, not a blog post you half-remember.

### Ranging while mutating: the spec's exact rules

The language spec is precise about mutation during `range` over a map, and the two
directions are not symmetric. **Deleting** the current key — or any key — during the
range is defined and safe: a key deleted before it is reached will not be produced.
That makes an expiry sweep that ranges and deletes-in-place correct and idiomatic;
`maps.DeleteFunc(m, pred)` is the packaged form. **Adding** a key during the range is
permitted but its effect is *unspecified*: an entry created during iteration may or
may not be produced by that same range. So an insert path must never run concurrently
against a range on the same map, and a single-threaded routine that needs to add keys
based on what it sees must collect the additions into a separate slice and apply them
after the range finishes. Delete-in-range: fine. Add-in-range: forbidden by contract.

### Map element addressability and key comparability

Two compile/runtime traps worth naming. Map elements are *not addressable*:
`m[k].Field = x` does not compile, because indexing a map yields a non-addressable
value (the entry could move on a rehash). To mutate a struct value you copy it out,
change it, and store it back — `v := m[k]; v.Field = x; m[k] = v` — or store `*T` and
mutate through the pointer. Separately, keys must be *comparable*: any comparable type
works (including a struct of comparables), but an interface key whose *dynamic* type is
not comparable (a `[]byte` boxed in an `any`, a map, a slice) compiles and then panics
at runtime when used as a key. Key on a concrete comparable type; convert `[]byte` to
`string` before using it as a key.

## Common Mistakes

### Ranging a map directly into output

Wrong: `for k, v := range m { fmt.Fprintln(w, k, v) }` into an API response, log line,
report, or metrics exposition. The output reorders on every run (randomized start
group), so golden tests flap and clients that (against the contract) rely on order
break intermittently.

Fix: collect keys, sort, then iterate — `for _, k := range slices.Sorted(maps.Keys(m))`.

### Writing to a nil map

Wrong: `var m map[string]int; m[k] = 1` panics. A map field left at its zero value, or
a config layer that was never initialized, triggers this only on the write path, so
read-only tests never catch it.

Fix: `make(map[K]V)` (or a constructor) before the first write.

### Treating a present zero value as absent

Wrong: `if m[k] == "" { useDefault() }` — this cannot tell "set to empty on purpose"
from "never set", so an intentional empty override silently falls back to the default.

Fix: `if v, ok := m[k]; ok { ... }` — branch on presence, not on the value.

### Sharing a plain map across goroutines

Wrong: one goroutine writing while others read (or two writers). The runtime raises
`fatal error: concurrent map writes` and kills the process — not a warning, not
recoverable.

Fix: `RWMutex`+map, `sync.Map`, or a sharded map, and prove it with `go test -race`.

### Returning the internal map from a Snapshot

Wrong: returning the live map (or a `sync.Map`'s live view) from an accessor. Callers
then mutate your shared state, and a caller ranging it while you write can crash.

Fix: return `maps.Clone(m)` (built under the read lock) or a copied slice.

### Reaching for sync.Map by default

Wrong: `sync.Map` "because it's concurrent". Under write-heavy or overlapping-key load
it is slower than `RWMutex`+map and forces `any` boxing plus type assertions.

Fix: default to `RWMutex`+map; use `sync.Map` only for a measured read-mostly,
disjoint-key workload.

### Tokenizing with strings.Fields when you meant punctuation

Wrong: `strings.Fields("hello,world")` returns `["hello,world"]` — one token, because
`Fields` splits on whitespace only.

Fix: split on non-alphanumeric runes (`unicode.IsLetter`/`IsDigit`), as the word
counter's `tokenize` does.

### Adding keys during range and expecting them

Wrong: adding entries to a map while ranging it and assuming the new keys appear or
that the result is stable. The spec leaves this unspecified.

Fix: collect additions into a separate slice and apply them after the range.
Delete-in-range is fine; add-in-range is not.

### Building a large index with no capacity hint

Wrong: `m := map[K]V{}` then inserting a known number of records, paying repeated
grow-and-rehash (table splits) as it grows.

Fix: `make(map[K]V, n)` with the length hint; confirm the allocation drop with
`b.ReportAllocs()`.

### Mutating a struct stored by value in a map

Wrong: `m[k].Field = x` does not compile — map elements are not addressable.

Fix: `v := m[k]; v.Field = x; m[k] = v`, or store `*T` and mutate through the pointer.

### Comparing maps the wrong way

Wrong: `m1 == m2` (illegal for maps beyond comparison to `nil`), or leaning on
`reflect.DeepEqual` in a hot path.

Fix: `maps.Equal(m1, m2)` for comparable values; reserve `reflect.DeepEqual` for tests
where the failure message clarity is worth it.

### Using an incomparable dynamic type as a key

Wrong: keying a `map[any]V` with a `[]byte` boxed in an `any` — compiles, then panics
at runtime.

Fix: key on a comparable concrete type; convert `[]byte` to `string` first.

### Assuming order is stable within a run

Wrong: memoizing an order observed from one range and reusing it. Order is randomized
per range call, even for the same unchanged map.

Fix: never depend on observed order; sort keys every time you need determinism.

Next: [01-stable-word-count.md](01-stable-word-count.md)
