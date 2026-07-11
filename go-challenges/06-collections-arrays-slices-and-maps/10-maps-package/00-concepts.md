# The maps Package — Concepts

Maps are the workhorse of every backend service. A config loader is a stack of
`map[string]string` layers. A feature-flag registry, a routing table, a set of
rate-limit buckets — all maps held in memory and read on the hot path. A
reconciler compares a desired-state map against an observed-state map. A cache is
a map plus an eviction policy. Set-based authorization is a map of scopes. The
standard `maps` package (Go 1.21, reshaped around iterators in Go 1.23) together
with `slices` and the `iter` range-over-func machinery is the modern,
allocation-aware toolkit for operating on all of them.

The senior skill here is not "call the function." It is knowing the sharp edges:
non-deterministic iteration order silently corrupting ETags, cache keys, and log
diffs; `maps.Clone` handing out a "snapshot" that still shares its pointer and
slice values; `maps.Copy` overwriting on key collision during a config merge;
`maps.Equal`'s `comparable` constraint being a compile-time gate, not a runtime
panic; and the fact that none of these functions synchronize anything, so a
shared map still needs a lock. This file is the model. The exercises that follow
each pin one of these contracts with a real `*_test.go`.

## The iterator turn: Keys, Values, All

Before Go 1.23 there was an experimental `x/exp/maps` whose `Keys` and `Values`
returned slices. That API is gone. In the standard library today,
`maps.Keys(m)` returns an `iter.Seq[K]`, `maps.Values(m)` an `iter.Seq[V]`, and
`maps.All(m)` an `iter.Seq2[K, V]`. An `iter.Seq[K]` is just a function,
`func(yield func(K) bool)`, that a `for k := range maps.Keys(m)` loop drives.

An iterator is not a collection. You cannot index it, take its `len`, or sort it
in place. To get a concrete slice you bridge with the `slices` package:
`slices.Collect(maps.Keys(m))` materializes an unordered `[]K`, and
`slices.Sorted(maps.Keys(m))` materializes a sorted one in a single call. To go
the other direction — build a map from a key/value stream — you use
`maps.Collect(seq)` on an `iter.Seq2`, or fold a stream into an existing map with
`maps.Insert(dst, seq)`. Forgetting the bridge is the number-one beginner error
after the turn: `keys := maps.Keys(m)` followed by `sort.Strings(keys)` does not
compile, because `keys` is a function, not a `[]string`.

The upside of iterators is laziness and composition: `maps.Keys` allocates
nothing by itself; the allocation happens only when you `Collect` or `Sorted`.
For a membership check or a bounded scan you can range the iterator directly and
never build the slice at all.

## Iteration order is randomized on purpose

Ranging a Go map — directly, or through `maps.Keys`/`maps.All` before you sort —
visits entries in an order the runtime deliberately randomizes per range
statement. This is a language guarantee, not an implementation accident: it
exists to stop code from depending on an order the runtime is free to change.

The consequence for backend work is concrete and unforgiving. Any output that
must be stable across runs or across processes — an HTTP `ETag`, an
idempotency-key fingerprint, a canonical JSON body you sign or hash, a
cache key, a diff you write to a log, a pagination cursor — MUST sort its keys
before it emits anything. The idiom is `slices.Sorted(maps.Keys(m))`, then range
the sorted slice. Skip it and you get a test that passes ninety-nine times and
fails on the hundredth, an ETag that changes on every request for an unchanged
resource, and a paginator that skips and duplicates rows. The randomization is
doing you a favor by surfacing the bug early; sorting is the fix.

## Clone is shallow — a snapshot is only as immutable as its values

`maps.Clone(m)` returns a new map with the same entries. The top-level map is
genuinely independent: adding, deleting, or reassigning a key in the clone does
not touch the original. That is exactly what you want for a snapshot handed to a
reader while writers keep mutating the source.

But the copy is shallow. If the value type is a pointer, a slice, or another map,
the clone copies the pointer/header, not the pointee. Both maps now point at the
same underlying object. Mutating `*clone[k]` or `clone[k][i]` is visible through
the original, and vice versa. A "snapshot" built with `maps.Clone` is immutable
only if its values are themselves immutable (strings, ints, structs of scalars).
When the values are reference types you must deep-copy each value to get a real
snapshot — clone the map, then clone every slice/pointed-to struct. Treating
`maps.Clone` as a deep copy is the classic way to ship a snapshot that a later
mutation silently corrupts.

## Copy is destructive and silent — the right primitive for a merge, used carefully

`maps.Copy(dst, src)` writes every entry of `src` into `dst`, in place, and on a
key collision `src` wins with no signal. That destructiveness is precisely what a
layered config merge wants: defaults, then file, then env, each later layer
overriding the earlier one. The correct pattern is `dst := maps.Clone(base)`
followed by `maps.Copy(dst, next)` for each subsequent layer — clone the base so
the merge mutates a fresh map and never one of the input layers. The mistake is
`maps.Copy(base, next)`, which mutates `base` (an input) as a side effect, so the
"defaults" map is now polluted for every later caller.

## Equal is a compile-time constraint, not a runtime panic

`maps.Equal(m1, m2)` reports whether two maps have the same keys and `==`-equal
values. Its constraint requires `V` to be `comparable`, and that is enforced by
the compiler: a map whose value type is not comparable (a slice, a func, a struct
containing one) will not compile in a call to `maps.Equal`. This is a frequently
mis-taught point. `maps.Equal` does not "panic at runtime on non-comparable
values"; the program with a non-comparable value type never builds. The escape
hatch is `maps.EqualFunc(m1, m2, eq)`, which takes an explicit equality function
and so handles non-comparable values, and — more usefully in practice — lets you
define equality that ignores a volatile field (a `LastSeen` timestamp, a
cached-derived field) so two semantically-equal states compare equal even though
a naive `==` would not.

## DeleteFunc prunes in place — cheaper than rebuild-on-every-pass

`maps.DeleteFunc(m, pred)` removes every entry for which `pred(k, v)` is true,
mutating `m` in place. For a TTL eviction sweep, a filter, or any pruning pass
this is the allocation-efficient primitive: one traversal, deletions applied to
the existing backing store, no new map. The alternative some code reaches for —
allocate a fresh map and copy the survivors — allocates the whole survivor set on
every pass and churns the garbage collector on a hot janitor. Deleting from a map
during a range over that same map is explicitly permitted by the language, which
is what makes the in-place sweep sound. Note that `DeleteFunc` on a `maps.Clone`
of the input is exactly how you filter without mutating the caller's map (the
preserved `FilterPositive` exercise leans on this).

## map[K]struct{} is the canonical set

When you only need membership — the set of scopes on a token, the set of keys to
delete, the set of nodes seen — the idiomatic Go set is `map[K]struct{}`. The
value `struct{}{}` is zero-width, so the set stores only keys and never wastes a
byte on a value; membership is the comma-ok test `_, ok := set[k]`. Prefer it
over `map[K]bool`, which invites the ambiguity of a stored `false` and spends a
byte per entry for no benefit. Set algebra — union, intersection, difference —
composes naturally on top: clone one operand, then add or `DeleteFunc` against
the other. Difference is how you compute "required scopes the token is missing."

## The ~map[K]V constraint preserves your domain types

Every function in the package is generic over `~map[K]V`, the tilde meaning "any
type whose underlying type is `map[K]V`." So a named domain type — `type Headers
map[string]string`, `type ScopeSet map[string]struct{}` — flows through
`maps.Clone`, `maps.Copy`, `maps.Equal`, and friends and comes back as the same
named type, not a bare `map`. Your abstractions survive the operation; you do not
have to convert to `map[string]string` and back.

## None of it is synchronized

The `maps` functions do no locking. A map shared across goroutines still needs a
`sync.RWMutex` or `sync.Mutex`, and — this is the subtle part — a concurrent
write while `maps.Clone` or `maps.Copy` is reading the source is a data race that
the race detector will flag and that can crash the program with a concurrent
map read/write fatal error. The standard safe pattern is clone-under-read-lock:
take the read lock, `maps.Clone` the shared map, release the lock, and let the
caller work on the private copy. You hand out the clone, never the live map, so a
reader can never observe a torn write and a writer can never race a reader.

## Common Mistakes

### Expecting Keys/Values to still return slices

Wrong: `keys := maps.Keys(m); sort.Strings(keys)`. Since Go 1.23 `maps.Keys`
returns an `iter.Seq[K]`, a function, so this fails to compile with a type
mismatch (or "cannot range").

Fix: bridge through `slices` — `keys := slices.Collect(maps.Keys(m))` for an
unordered slice, or `keys := slices.Sorted(maps.Keys(m))` for a sorted one.

### Ranging a map to produce stable output

Wrong: building an ETag, a cache key, a canonical JSON body, or a page of results
by ranging the map directly. The randomized order flakes tests and breaks
ETag/idempotency/pagination semantics.

Fix: `for _, k := range slices.Sorted(maps.Keys(m))` and emit in key order.

### Treating Clone as a deep copy

Wrong: `snap := maps.Clone(live)` where the values are pointers or slices, then
handing `snap` out as an immutable snapshot. A later mutation of a shared value
corrupts the snapshot.

Fix: deep-copy the reference-typed values too — clone the map, then clone each
slice / copy each pointed-to struct.

### Merging config into an input layer

Wrong: `maps.Copy(defaults, fileLayer)` to merge, which mutates `defaults` (an
input) so every later caller sees the polluted map.

Fix: `merged := maps.Clone(defaults); maps.Copy(merged, fileLayer)` — clone the
base first, mutate the fresh copy.

### Believing Equal panics on non-comparable values

Wrong: "wrap `maps.Equal` in `recover` in case the values aren't comparable." The
comparability requirement is a compile-time constraint; a non-comparable value
type simply does not build.

Fix: for structs with volatile or non-comparable fields, use `maps.EqualFunc`
with an explicit equality function.

### Expecting Invert / Collect to keep colliding entries

Wrong: inverting `map[string]int{"a":1,"b":1}` and expecting two entries. The
duplicate value collapses to one key — last writer wins. Same trap with
`maps.Collect` and `maps.Insert` on duplicate keys.

Fix: document the uniqueness requirement, or accumulate colliding entries into a
`map[V][]K` when you must keep them all.

### Reaching for a maps.ValuesFunc that never existed

Wrong: `maps.ValuesFunc(m, pred)`. There is no such function.

Fix: `maps.Values(m)` plus a filter over the iterator, or `maps.DeleteFunc` on a
`maps.Clone` of the map.

### Rebuilding a map to prune it

Wrong: allocating a fresh map and copying survivors on every eviction pass.

Fix: `maps.DeleteFunc(m, pred)` prunes in place with no new allocation.

### Trusting the maps package for concurrency safety

Wrong: sharing a map across goroutines and assuming `maps.Clone`/`maps.Copy` make
it safe. A concurrent write during the clone is a data race, fatal under `-race`.

Fix: guard the shared map with a `sync.RWMutex` and clone under the read lock;
hand out the clone, not the live map.

Next: [01-map-transform-pipeline.md](01-map-transform-pipeline.md)
