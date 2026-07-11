# Sorted Collections and Binary Search — Concepts

Binary search over a sorted slice is the workhorse behind most read-heavy
in-memory structures a backend engineer maintains without reaching for a
database: time-series and event indexes, consistent-hash rings for sharding and
caching, IP-to-metadata lookup tables, weighted traffic routers and canary
splitters, sliding-window rate limiters, schema-migration version resolution,
and leaderboards. The senior skill is not "call `sort.Search`". It is reasoning
about the invariant the search depends on, the insert-versus-query trade-off
against a map, half-open interval discipline for range and floor/ceiling
queries, the aliasing hazard when you return a sub-slice, and choosing between
the modern generic `slices`/`cmp` API and the older `sort.Search` closure form.
Read this once and you have the model behind all ten exercises that follow.

## Concepts

### The sorted invariant is a contract, and violating it fails silently

Binary search does not merely prefer a sorted slice — it *assumes* one, and it
assumes the sort key matches the comparator the search uses. Call any binary
search on a slice that is not sorted (or that is sorted by a different key) and
you get no panic, no error, no signal at all: it returns a wrong index or
`found=false` for an element that is present. The bug surfaces later as a cache
miss, a routing decision to the wrong shard, a lookup that says an IP is
unknown when it is not — far from the mutation that broke the order. That is why
mature code treats sortedness as an invariant that *every mutating operation
upholds* and validates it in constructors with `slices.IsSorted` /
`slices.IsSortedFunc` during development. The failure mode is not a crash; it is
quiet data corruption.

### `sort.Search` is the primitive: the lower bound / insertion point

`sort.Search(n, f)` returns the smallest index `i` in `[0, n)` for which `f(i)`
is true, and returns `n` if there is none. `f` must be *monotone*: false for a
prefix, then true for the rest (`false…false,true…true`). That single primitive
is the insertion point — the place where a new element would go to keep the
slice sorted — which is also the lower bound of any element you search for.
`sort.SearchInts` and `sort.SearchStrings` are just typed conveniences that pass
`s[i] >= target` as `f`. Everything else in this lesson is a specialization of
this one idea: express your query as a monotone predicate and read off the
boundary index.

### `slices.BinarySearch` answers "where" and "is it here" in one call

`slices.BinarySearch(s, target)` (Go 1.21+) returns `(pos, found)`: `pos` is the
insertion point (the same lower bound `sort.Search` gives you), and `found`
tells you whether `target` is actually present at `pos`. One call answers both
"where would it go" and "is it already here", which is exactly what set-style
insert, membership, and floor/ceiling all need. `slices.BinarySearchFunc(s,
target, cmp)` takes a comparator `func(element, target) int` returning negative
when `element < target`, zero on equal, positive when `element > target`. Prefer
these generic forms over `sort.Search` closures for new code; they are shorter,
type-safe, and return the `found` bit you would otherwise recompute by hand.

A subtlety worth internalizing: `pos` is meaningful *even when `found` is false*.
It is still the correct insertion point, and it is still the boundary that
floor/ceiling and range queries are built on. Treating the index as garbage
unless `found` is true throws away half of what the call computed.

### `sort.Find` is the closure equivalent of `BinarySearch`

`sort.Find(n, cmp)` (Go 1.19+) returns `(i, found)` where `cmp(i)` is positive
over the leading prefix, zero in the matching middle, and negative over the
trailing suffix; it returns the smallest `i` with `cmp(i) <= 0`. It is the
non-slice-backed analogue of `slices.BinarySearch` — use it when the data is not
a plain slice (a paged store, a computed sequence) but you still want the
`(index, found)` result. Note the sign convention is the *opposite* of
`slices.BinarySearchFunc`: `sort.Find`'s `cmp` is positive on the prefix (before
the target), while `slices.BinarySearchFunc`'s `cmp(element, target)` is negative
when the element is below the target. Swapping the two silently breaks the
search.

### Half-open intervals compose; use them everywhere

A window expressed as `[lo, hi)` — lo inclusive, hi exclusive — is not a style
choice, it is what makes boundary queries compose. The count of a half-open
window is exactly `hi - lo`. Adjacent windows `[a, b)` and `[b, c)` share no
element and tile the space with no gap and no overlap. Empty is naturally
`lo == hi`. A range query, a time window, a floor, and a ceiling are all the same
move: a lower-bound search (first index with `element >= lo`) and an upper-bound
search (first index with `element >= hi`), and the answer is `s[lo:hi]`. If you
find yourself reasoning about closed intervals `[lo, hi]` and `+1`/`-1`
corrections, you are fighting the grain; switch to half-open and the off-by-ones
disappear.

### Duplicates split the query into two bounds

With duplicates present, `BinarySearch`/`sort.Search` return the *first* index of
an equal run — the lower bound. To get the whole run of elements equal to
`target` you also compute the upper bound: the first index whose element is
strictly greater than `target`. The run is `[lo, hi)`. This is the same
lower/upper-bound pair as a range query; "all events at timestamp T" and "all
events in `[from, to)`" are the same code with different comparators.

### Insertion is O(n); pick the structure by workload

Inserting into a sorted slice is O(n) even though the search is O(log n): finding
the position is cheap, but shifting the tail up one slot via `copy` or
`slices.Insert` is linear, and the shift dominates. `append` then re-sort on
every insert is strictly worse at O(n log n) per insert. A map is O(1) amortized
for insert and point lookup but has *no order at all* — no range scan, no
floor/ceiling, no ordered iteration. So choose by workload: a sorted slice wins
for range scans, ordered iteration, floor/ceiling, and read-mostly data built
once and queried many times; a map wins for point membership under heavy
mutation; a balanced tree or skip list earns its complexity only when you need
O(log n) inserts *and* ordering at large n simultaneously. For the common
backend case — build an index once, then serve many range and point queries — the
sorted slice is both the fastest and the simplest.

### Returning a sub-slice aliases your backing array

`return s[i:j]` hands the caller a window that shares your backing array. Two
things then go wrong. The caller can write through it and corrupt your internal
storage. And a later `append` that grows past `cap` reallocates while an append
that fits *overwrites the caller's view in place*. The fix is to sever the
aliasing: `slices.Clone(s[i:j])` or `append([]E(nil), s[i:j]...)` returns a fresh
backing array the caller owns. Any query that returns a portion of your storage
must clone unless the API explicitly documents that the result aliases and is
read-only.

### The circular case is why raw `sort.Search` still matters

A consistent-hash ring is a sorted slice of hash points treated as a circle: to
find the owner of a key you want the first ring point `>= hash(key)`, and when
that search returns `len(points)` — the key hashed past the largest point — the
owner wraps around to index 0. `sort.Search` returning `len` is exactly the
signal you need for the wraparound, expressed cleanly as `idx % len(points)`.
The plain `slices.BinarySearch` `found` bit does not express "wrap to the front",
which is why the circular structure is the one place the older primitive reads
more naturally than the generic one.

### Comparators must be a total order consistent with the sort

`cmp.Compare(x, y)` and `cmp.Or(...)` (Go 1.21+) build multi-key comparators
cleanly: `cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Name, b.Name))` is
"score descending, then name ascending" — `cmp.Or` returns its first non-zero
argument, so it reads as a priority list of tie-breakers. The non-negotiable
rule: the comparator you search with must be a *total order consistent with the
order the slice is sorted in*. If it returns 0 for elements that are not equal,
or if it is not transitive, both sorting and searching misbehave. Floats
containing NaN are the classic trap: NaN is unordered with respect to everything
(including itself), so a slice with a NaN cannot be totally ordered, and both
`slices.Sort` and `slices.BinarySearch` produce nonsense. Reject NaN at the
boundary or use a key type that cannot hold it.

### Reassign the result of `slices.Insert`, `append`, `Delete`, `Compact`

`slices.Insert(s, i, v...)` returns the possibly-reallocated slice after shifting
`s[i:]` up; you must assign the result back — `s = slices.Insert(s, i, v)`.
Dropping the return value leaves the slice unchanged (or loses a reallocation)
and is a silent bug. The same reassign-the-result discipline applies to
`append`, `slices.Delete`, and `slices.Compact`: they all may reallocate and all
return the new slice header.

## Common Mistakes

### Searching a slice that is not sorted by the comparator's key

Wrong: calling any binary search on a slice that is unsorted, or sorted by a
different key than the comparator inspects. There is no panic — just a wrong
index or a false "not found" for a present element, surfacing later as
corruption.

Fix: uphold the invariant on every mutation, and guard constructors with
`slices.IsSorted` / `slices.IsSortedFunc` so an unsorted input is rejected at the
boundary instead of silently mis-answered.

### `append(s, v); sort.Sort(s)` on every insert

Wrong: growing then re-sorting on each insert is O(n log n) per insert.

Fix: binary-search the position and `slices.Insert` (O(n) per insert), or, for a
bulk build, append everything and sort once at the end (O(n log n) total, not per
insert).

### Returning the internal sub-slice from a query

Wrong: `return t.data[i:j]` lets the caller mutate your storage, and a later
`append` can clobber the caller's view.

Fix: `return slices.Clone(t.data[i:j])` (or `append([]E(nil), t.data[i:j]...)`)
to break the aliasing.

### Off-by-one at interval boundaries

Wrong: mixing `>= hi` and `> hi` for the upper bound, or using the `>=` lower
bound where you needed the strictly-greater bound — windows that wrongly include
or drop the boundary element.

Fix: commit to half-open `[lo, hi)`. Lower bound is first index with
`element >= lo`; upper bound is first index with `element >= hi`. The run of
elements equal to a key uses `element > key` for the upper bound.

### Getting the comparator sign or argument order wrong

Wrong: assuming `slices.BinarySearchFunc` and `sort.Find` use the same sign
convention. `BinarySearchFunc`'s `cmp(element, target)` is negative when the
element is below the target; `sort.Find`'s `cmp(i)` is positive over the prefix
before the target.

Fix: keep the two straight — `BinarySearchFunc` compares `element` to `target`
the natural way (`cmp.Compare(element, target)`); `sort.Find` returns the sign
you would get from `cmp.Compare(target, s[i])`.

### Forgetting the wraparound in a circular structure

Wrong: on a hash ring, treating `sort.Search` returning `len(points)` as an
out-of-range miss.

Fix: `idx % len(points)` — a search result of `len` wraps to index 0, which is
the correct owner on a circle.

### Dropping the result of `slices.Insert` / `append`

Wrong: `slices.Insert(s, i, v)` as a statement, discarding the return value —
the slice is unchanged or loses its reallocation.

Fix: `s = slices.Insert(s, i, v)`. Reassign the result of every op that may
reallocate.

### Treating the search index as meaningless when `found` is false

Wrong: ignoring `pos` from `BinarySearch` unless `found` is true.

Fix: `pos` is the correct insertion point regardless of `found` — it is exactly
what floor, ceiling, and insert need.

### A comparator that is not a strict total order

Wrong: a `cmp` that returns 0 for unequal elements, or one built from raw float
subtraction that can overflow or produce NaN.

Fix: build comparators from `cmp.Compare` and `cmp.Or`; reject NaN at the
boundary so the order stays total.

### Reaching for a sorted slice when inserts dominate

Wrong: using a sorted slice as the default ordered container even under heavy
mutation, paying O(n) shifts on every write.

Fix: at high mutation rates prefer a map plus a periodic re-sort, or a proper
ordered tree; reserve the sorted slice for read-mostly, range-scan workloads.

Next: [01-sorted-int-index.md](01-sorted-int-index.md)
