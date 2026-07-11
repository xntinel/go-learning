# The slices Package: Production Collection Operations — Concepts

The `slices` package is what you actually reach for in backend Go instead of
hand-rolled sort, dedup, filter, and search loops. It is fast (its `Sort` is a
pattern-defeating introsort that resists adversarial O(n^2) inputs), it is
well-tested, and it is allocation-aware. But the senior question is never "which
function exists" — it is the *operational contract* of each function: which ones
mutate their argument in place versus return a new slice header you must
reassign, which ones alias the caller's backing array, which ones zero the freed
tail (and why that is a correctness feature, not cosmetics), and which ones
silently return a wrong answer when you violate an invariant they never check.
This file is the conceptual foundation for the ten independent modules that
follow; read it once and each module becomes an exercise in feeling one of these
trade-offs on a concrete backend artifact — a reconcile loop, a batch writer, a
sorted index, an eviction sweep, a fan-in merge.

## Concepts

### Mutate-in-place versus return-a-new-header

The single most important split in the package is between functions that change
the slice through the pointer you passed and functions that hand you back a new
header you are obligated to keep.

`Sort`, `SortFunc`, `SortStableFunc`, and `Reverse` mutate the elements in place
and return nothing. The length and capacity do not change; only the ordering
does. You call them as statements: `slices.SortFunc(xs, cmp)`.

`Compact`, `CompactFunc`, `Delete`, `DeleteFunc`, `Insert`, `Replace`, `Clip`,
`Grow`, and `Concat` return a slice header whose length (and sometimes capacity,
and sometimes backing array) differs from the input. You MUST reassign:
`xs = slices.Compact(xs)`. The classic production bug is calling
`slices.Delete(xs, i, j)` as a statement and throwing away the result: the
variable `xs` still spans the old length, so the code goes on to read stale or
zeroed elements in the tail. If a function returns a slice, that return value is
not advisory.

### Backing-array aliasing: they truncate, they do not allocate

`Compact`, `CompactFunc`, `Delete`, `DeleteFunc`, `Insert`, and `Replace` reuse
the input's backing array. They shift surviving elements down (or up) and adjust
the length; they do not allocate a fresh array. That is exactly why they are
fast, and exactly why they are dangerous when a caller still holds the original
slice. If a function receives `s`, deletes from it, and returns the shortened
result, any other variable that aliased `s`'s backing array now sees mutated
contents through its own (still-long) length view. When a routine must not touch
the caller's data — a pipeline stage, a pure transform — `slices.Clone` first and
operate on the copy. `Clone` is the one-line, allocation-honest way to get an
independent backing array.

### Tail zeroing is a correctness feature

`Compact`, `CompactFunc`, `Delete`, `DeleteFunc`, and `Replace` zero the elements
between the new (shorter) length and the old length. For a `[]int` this looks
pointless. For a `[]*Conn`, a `[]*Event`, or a slice of structs holding pointers
or secrets, it is essential: zeroing those trailing slots drops the references so
the garbage collector can reclaim the pointed-to objects, and any secret material
does not linger in the array's freed region. This only works if you reassign the
returned (shortened) slice — the live slice header must be the short one, so the
now-zeroed slots live in the unreachable `[len:cap]` region. Keep the old long
header around and you have pinned exactly the objects you meant to release. You
can observe the contract directly: re-slice the result up to its capacity
(`full := s[:cap(s)]`) and the elements past the new length read as the zero
value.

### BinarySearch requires a sorted invariant it never verifies

`slices.BinarySearch` and `slices.BinarySearchFunc` assume the slice is already
sorted in the same order the comparator implies, and they do NOT check it. Feed
an unsorted slice and you get a plausible-looking index with `found == false`
(or, worse, a wrong index) and no error, ever. This is the archetypal silent
logic bug: the code compiles, the types are right, the tests on sorted data pass,
and production returns wrong answers on data that drifted out of order.
`BinarySearch` takes no comparator (it uses the natural ordering); the moment you
sorted with a custom `SortFunc` you must search with `BinarySearchFunc` using the
*same* ordering. A case-insensitive sort paired with a case-sensitive search will
report present keys as absent.

### Stable versus unstable sort

`SortFunc` is not guaranteed to be stable: two elements the comparator calls
equal may come out in either relative order, and that order can change between
runs or Go versions. `SortStableFunc` guarantees equal elements keep their input
order. This matters the instant ties are meaningful: cursor pagination that
orders by a non-unique key needs equal-key rows to stay in a deterministic
sequence (usually insertion id) so page boundaries do not shift when a later page
re-sorts the same data. Either reach for `SortStableFunc`, or make the comparator
a *total order* by breaking ties on a unique key with `cmp.Or`, at which point
`SortFunc` is deterministic because no two elements are ever equal.

### The comparator contract

A comparator is `func(a, b T) int` returning a value < 0 when a sorts before b, 0
when they are equal, > 0 when a sorts after b. It must be a consistent total
order: transitive, and antisymmetric (cmp(a,b) and cmp(b,a) have opposite signs).
`cmp.Compare` gives you this for any ordered type; `cmp.Or` composes several key
comparisons into one by returning the first non-zero — a clean way to express
"by status, then by created-at, then by id". A comparator whose result depends on
mutable external state, or that is not transitive, makes the sort's output
undefined; the package trusts you here.

### nil versus empty is per-function

The distinction between a nil slice and a non-nil zero-length slice is not
uniform across the package, and tests must pin the behavior callers depend on.
`slices.Equal` treats nil and empty as equal (both are length zero). `Clone`
preserves nilness: `Clone(nil)` is nil, `Clone([]int{})` is non-nil empty.
`Concat`, `Collect`, `Sorted`, and `SortedFunc` return nil for empty input.
`Repeat` never returns nil. If a caller marshals the result to JSON, nil becomes
`null` while empty becomes `[]` — a difference an API contract may care about.

### Iterator-based additions (Go 1.23)

The 1.23 release wired `slices` to the `iter` package. `All`, `Backward`, and
`Values` turn a slice into an `iter.Seq`/`iter.Seq2` you can range over with a
function. `Chunk` yields fixed-size sub-slice batches and drives a range-over-func
loop — the natural tool for provider-limited bulk writes (DynamoDB BatchWrite of
25, SQS SendMessageBatch of 10). `Chunk` panics if the size is less than 1, and
each sub-slice it yields aliases the source's backing array, so do not retain a
chunk past its loop iteration without cloning. `Collect`, `Sorted`, `SortedFunc`,
`SortedStableFunc`, and `AppendSeq` bridge iterators back into concrete slices.

### Capacity control: Clip and Grow

`Clip` returns `s[:len(s):len(s)]`, capping capacity at length so the next
`append` is forced to allocate a fresh array instead of writing into shared tail
capacity. This is the fix for append-aliasing corruption: when you hand a
sub-slice of a shared backing array to another goroutine or store it, a later
`append` into it can silently overwrite a sibling's elements unless you clipped
it first. `Grow` is the inverse concern — it pre-reserves capacity for a known
number of upcoming appends so a hot loop does not reallocate and copy repeatedly.
`Grow(s, n)` guarantees `cap(s) >= len(s)+n` and panics on a negative n.

### Choose the package over hand-rolled loops

`slices.Sort` uses pattern-defeating quicksort (pdqsort/introsort) that degrades
gracefully on adversarial inputs where a naive quicksort goes quadratic.
`DeleteFunc` is a single O(n) compaction pass; the tempting alternative — calling
`slices.Delete` inside a loop as you scan — is O(n^2) and also shifts the indices
under your own cursor, a correctness trap on top of the performance one. These
functions are the well-tested, allocation-aware defaults; reimplementing them by
hand loses the pdqsort guarantee and invites the exact bugs the package was
written to prevent.

## Common Mistakes

### Ignoring the returned slice from Compact/Delete/Insert/Concat

Wrong: `slices.Compact(s)` as a statement. The variable `s` still spans the
original length, so subsequent reads walk into the stale-or-zeroed tail.

Fix: `s = slices.Compact(s)`. If a function returns a slice, capture it.

### Mutating the caller's slice without cloning

Wrong: sorting or compacting a slice the caller passed and still relies on, in
place. The caller's data is silently reordered or truncated underneath it.

Fix: `out := slices.Clone(in)` and operate on `out` when the contract is "do not
touch my input".

### Trusting BinarySearch on an unsorted slice

Wrong: feeding a slice that drifted out of order to `BinarySearchFunc` and using
the result. The returned index is plausible but wrong, with no error.

Fix: maintain the sorted invariant on every insert (ordered insert via
`BinarySearchFunc` + `Insert`), or `IsSortedFunc` before searching in a guard.

### Searching with a different order than you sorted with

Wrong: sorting case-insensitively with `SortFunc` then searching with
`BinarySearch` (case-sensitive) or a differently-keyed `BinarySearchFunc`.
Present keys report `found == false`.

Fix: use the identical comparator for the sort and the search.

### Assuming Compact removes all duplicates

Wrong: expecting `Compact` to de-duplicate an unsorted slice. It only collapses
*consecutive* equal runs, so far-apart duplicates survive.

Fix: sort first if you need global de-duplication, then `Compact`.

### Using SortFunc where ties must stay ordered

Wrong: `SortFunc` for pagination or reproducible output where equal-key rows must
keep their prior order. The reordering is silent and non-deterministic.

Fix: `SortStableFunc`, or a total-order comparator via `cmp.Or` on a unique
tiebreak key.

### Confusing nil and empty semantics

Wrong: assuming `slices.Equal(nil, []int{})` is false, or that `Clone(nil)`
returns a non-nil empty slice, or that `Concat()` of empties is non-nil.

Fix: `Equal` says nil == empty; `Clone` preserves nilness; `Concat`/`Collect`
return nil for empty. Pin the one your callers depend on in a test.

### Calling Max/Min/MaxFunc/MinFunc on a possibly-empty slice

Wrong: `slices.Max(samples)` where `samples` can be empty. These panic on an
empty slice.

Fix: guard the length and return an `ok bool`, or ensure a non-empty precondition
at the call site.

### Handing out shared sub-slices without Clip

Wrong: passing `buf[a:b]` of a shared backing array to a consumer that appends;
the append writes into capacity that belongs to a sibling sub-slice, corrupting
it.

Fix: `slices.Clip(buf[a:b])` so the consumer's append must allocate.

### Repeated Delete in a filter loop, or Chunk(n<1)

Wrong: calling `slices.Delete` once per removed element inside a scan (O(n^2) and
shifting indices), or `slices.Chunk(s, 0)` (panics).

Fix: a single `slices.DeleteFunc` pass for filtering; validate `n >= 1` before
`Chunk`.

Next: [01-normalize-pipeline-stage.md](01-normalize-pipeline-stage.md)
