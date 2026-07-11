# 3. Value Interning with the unique Package — Concepts

A metrics or tracing backend creates the same label over and over: every request produces a `{method:"GET", route:"/users/:id", status:"200"}` set, and a busy process holds millions of them. Stored naively, each is a fresh copy of the same strings, and using a label set as a map key compares those strings byte by byte on every lookup. Interning fixes both problems at once: canonicalize each distinct value to one shared copy, and represent it by a small handle whose equality is a pointer comparison. Go 1.23 added the `unique` package to do this safely — its canonical store uses weak references so unused values are reclaimed, unlike the classic `map[string]string` interner that leaks forever. This file is the conceptual foundation: read it once and you will have everything you need to reason through the exercise, which builds an interned label type for a metrics counter and then interns whole label *sets* into one order-independent series key, as an independent, self-contained Go module.

## Concepts

### What interning buys you

`unique.Make[T comparable](v T) Handle[T]` returns a globally unique handle for `v`. Two handles are equal exactly when the values that produced them are equal, and comparing handles is "trivial and typically much more efficient than comparing the values" — in the current implementation, effectively a pointer comparison against the single canonical copy (the docs promise the efficiency, not the exact mechanism). So interning gives you two things at once: every duplicate of a value collapses to one stored copy (memory), and equality/hashing of the handle is O(1) regardless of how big the value is (speed). For a `Label{Name, Value string}`, comparing two interned handles is one pointer compare instead of two string compares.

### Handles are comparable, so they make good map keys

`Handle[T]` is itself a comparable value with a `Value() T` method. That means an interned label can be used directly as a map key — the key comparison the runtime does on every lookup becomes the cheap pointer compare. A counter map keyed by `unique.Handle[Label]` is both smaller (it shares one copy of each label) and faster to probe than one keyed by the raw struct.

### Why it does not leak

A hand-rolled interner — `var pool = map[string]string{}` guarded by a mutex — keeps every value it ever saw alive forever. `unique` stores its canonical map behind weak references and a runtime cleanup, so once no live handle or value refers to an entry, the runtime reclaims it. You get interning without the unbounded growth. (Weak pointers and cleanups are the subject of the next lesson.)

### Canonicalization beyond single values

Interning is most powerful on a *derived* canonical form. A metric time series is identified by a *set* of labels, and `{a=1, b=2}` and `{b=2, a=1}` are the same series. If you sort the labels and join them into one canonical string before interning, two differently-ordered label sets collapse to the same handle — so series identity, deduplication, and map-key equality all become the single pointer compare, regardless of how the caller ordered the labels. The `series.go` half of the exercise does exactly this.

There is a subtlety in the join that is easy to miss: the canonical encoding must be *injective*. A naive `name=value,` join collides — the two-label set `{a="b", c="d"}` and the one-label set `{a="b,c=d"}` both render to the string `a=b,c=d`, so two genuinely different series would share one key. Quoting each component with `strconv.Quote` before joining removes the ambiguity, because the separators (`,` and `=`) can no longer appear unescaped inside a component. The encoding is the contract: if it is not injective, distinct series silently merge their counts.

### Handles stay valid and comparable

A handle keeps working even after every other copy of the underlying value is gone: the canonical store holds the one copy a live handle needs, and two handles made from equal values keep comparing equal for as long as either is alive. You can store handles in long-lived maps and rely on their identity; `Value()` reconstructs the underlying value when you need it back.

### Cost model and when not to intern

`Make` is concurrency-safe but not free: it hashes the value and does a lookup (and possibly an insert) in a sharded global map. Interning pays off when a value is *reused enough* to amortize that cost and you benefit from the cheap equality or the memory dedup — labels, tags, log field keys, interned identifiers in a parser. Do not intern values that are unique or used once: you pay the map cost and the dedup never happens. And `T` must be `comparable`, interned by value equality of all its fields; you cannot intern a slice or a map.

A rule of thumb: intern when the duplication factor is well above 1 *and* the value is large enough that the saved copy/compare outweighs the lookup — a 40-byte label set seen thousands of times, yes; a unique 16-byte id, no. Watch the *derived*-form cost too: `InternSeries` re-runs `slices.Clone` + `SortFunc` + a `strings.Builder` allocation on *every* call, even a cache hit. The pointer compare wins on the map probe, but the canonicalization is still O(n log n) plus an allocation per call, so a hot path should cache the `SeriesKey` per collector rather than re-canonicalize each event — which is what real metrics libraries do.

Why `unique` over a hand-rolled `sync.Map` interner: the canonical store is weak-backed (no manual eviction, no permanent growth), it is generic over any comparable `T` (not just strings), and the cleanup runs without you wiring it. The one caveat: reclamation is not synchronous — a value becomes collectible when no live handle refers to it, but the memory is freed at a later GC, so the win shows up for *churning* values, not for one you drop and immediately re-measure.

## Common Mistakes

### Hand-rolling a map interner that never frees

Wrong: `var pool = map[string]string{}` behind a mutex, returning `pool[s]`. It keeps every string ever interned alive for the life of the process.

Fix: `unique.Make`. Its canonical store uses weak references, so entries for values no longer in use are reclaimed.

### Interning unique or one-shot values

Wrong: calling `unique.Make` on a value seen once (a request ID, a UUID). You pay the lookup and gain no dedup.

Fix: intern only values with real duplication and reuse — labels, enums, field keys.

### Comparing Value() instead of the handle

Wrong: `a.Value() == b.Value()` — that throws away the benefit and compares the underlying strings again.

Fix: compare the handles directly (`a == b`); that is the cheap pointer compare interning exists to give you.

### Trying to intern a non-comparable type

Wrong: `unique.Make([]string{...})` — slices are not comparable and will not compile.

Fix: intern a comparable representation (a struct of strings, or a single joined string) instead.

### A non-injective canonical encoding for a derived form

Wrong: joining a label set with a plain `name=value,` separator. The separators can appear inside a value, so `{a="b", c="d"}` and `{a="b,c=d"}` produce the same canonical string and the same handle — two different series silently merge.

Fix: quote each component (`strconv.Quote`) before joining, so the separators can never appear unescaped inside a component and the encoding stays injective.

---

Next: [01-interned-metrics.md](01-interned-metrics.md)
