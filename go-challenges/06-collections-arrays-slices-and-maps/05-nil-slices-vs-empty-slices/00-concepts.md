# Nil vs Empty: Slice, Map, and JSON Semantics on Real API Boundaries

A `nil` slice and an empty slice are two different values that share the same
length. Inside a Go program the difference is almost invisible: `append`,
`range`, `len`, and `cap` treat them identically, so business logic rarely needs
to care which one it holds. The difference becomes real, and sometimes a
production incident, the moment the value crosses a boundary — JSON on the wire,
a tri-state PATCH body, an equality check, a shared backing array under
concurrency. This lesson is about making that difference a deliberate contract
decision at each boundary instead of an accident that leaks out of a zero value.
Read this file once and every exercise that follows becomes a variation on the
same theme: decide what "no value" means here, and encode that decision so it
cannot silently regress.

## The slice header and what nil really is

A slice value is a three-word header: a pointer to a backing array, a length,
and a capacity. A `nil` slice (`var s []T`) has `pointer == nil`, `len == 0`,
`cap == 0`, and it compares equal to `nil`. An empty slice (`[]T{}` or
`make([]T, 0)`) has a *non-nil* pointer — it points at a zero-size backing array,
often a single shared runtime address every empty allocation reuses — together
with `len == 0` and `cap == 0`, and it is `!= nil`. Both have length zero, so
`len(s)` cannot tell them apart; the only operator that distinguishes them is
`s == nil`.

```go
var a []int          // nil:   a == nil is true
b := []int{}         // empty: b == nil is false
len(a) == len(b)     // true, both 0 — len cannot distinguish them
```

Because every read-only slice operation tolerates a nil header (a nil pointer
with length zero simply yields no elements), you can `range` a nil slice, take
its `len`/`cap`, reslice it, and `append` to it. Only an operation that actually
dereferences the backing array to read an element out of bounds panics, and a
zero-length slice never offers such an index. This is why "nil vs empty" is a
non-issue inside a function and a real decision at its edges.

## Why the distinction only matters at boundaries

Keep this framing: the nil/empty difference is observable exactly at four kinds
of boundary, and nowhere else worth worrying about.

1. Serialization. `json.Marshal` of a nil slice emits `null`; of a non-nil empty
   slice emits `[]`. The identical rule holds for maps: a nil map marshals to
   `null`, an empty map to `{}`. A client that treats `null` and `[]`
   differently — a pager that stops when a field is absent, a cache that
   invalidates on `null`, a tri-state PATCH — turns an accidental nil into a
   protocol bug.
2. Tri-state input. On the way in, a plain `[]T` field cannot record whether the
   JSON key was absent, was `null`, or was `[]`; they all decode to something
   with length zero. Recovering that distinction needs `*[]T` or
   `json.RawMessage`.
3. Value identity and equality. Slices are not comparable with `==` (that is a
   compile error) except against the untyped `nil`. `slices.Equal` compares
   contents; it reports a nil and an empty slice as equal because it only looks
   at length and elements. So "are these two the same value" and "do these two
   have the same contents" are different questions with different answers.
4. Aliasing under `append`. Two slices derived from one base can silently share a
   backing array, so a write through one is visible through the other. Under
   concurrency that sharing is a data race, not merely a surprising mutation.

## JSON on the wire: null, [], and the maps that match

`json.Marshal([]string(nil))` produces `null`; `json.Marshal([]string{})`
produces `[]`. `json.Marshal(map[string]int(nil))` produces `null`;
`json.Marshal(map[string]int{})` produces `{}`. The producer side of an API
therefore has a real choice to make per field: does "no value" mean *the field
has no value* (`null`) or *a known, currently-empty collection* (`[]`)? A list
endpoint that ran a query and matched zero rows almost always means the latter,
and a client paging through results is entitled to see `[]`, not `null`. The bug
is not choosing wrong on purpose; it is letting a repository return a nil slice
by accident and having the encoder render `null` for a value that logically was
an empty list.

## Go 1.24 struct tags: omitempty is not omitzero

Two struct-tag options govern whether a field is written at all, and Go 1.24
made the pair meaningful.

`omitempty` omits a field when it is "empty" in the JSON sense: length zero for
slices, maps, and strings, plus `false`, `0`, and nil pointers/interfaces. For a
slice that means it drops *both* a nil slice and a non-nil empty slice — you
cannot use `omitempty` to make an explicit empty list appear as `[]`, because it
removes the field entirely.

`omitzero` (added in Go 1.24) omits a field only when it equals its zero value,
or when the field's type has an `IsZero() bool` method that returns true. The
zero value of a slice is nil, so `omitzero` drops a nil slice but *keeps* a
non-nil empty slice, which then renders as `[]`. That is the clean lever for
"absent vs empty" on the wire: nil disappears, `[]` survives. When both tags are
present the conditions are OR-ed, but the useful design is to pick one per field
based on the contract you are advertising.

```go
type Resp struct {
	A []string `json:"a,omitempty"` // nil -> absent, []  -> absent
	B []string `json:"b,omitzero"`  // nil -> absent, []  -> "b":[]
}
```

## Tri-state on input: absent vs null vs empty

Decoding is where nil-vs-empty changes real behavior, in a PATCH handler whose
`Tags` field has four possible meanings: key absent means *leave unchanged*,
`null` means *clear to nil*, `[]` means *set to explicit empty*, and `[...]`
means *replace*. A plain `[]T` field collapses absent and `null` into the same
zero value and cannot tell them apart. A pointer `*[]T` is only a partial fix,
and it is worth being precise about why: unmarshaling both an absent key and an
explicit `null` leaves the pointer nil, so `*[]T` distinguishes "present with a
value" from "absent-or-null" but still cannot separate absent from `null`. The
reliable tri-state (really four-state) tool is `json.RawMessage`: an absent key
leaves it length zero, `null` gives it the bytes `null`, `[]` gives it `[]`, and
`["x"]` gives it the element bytes. Inspect the raw bytes, then apply the
correct mutation. That is the canonical place where the whole lesson pays off.

## Maps: reads are safe, the first write panics

A nil map is not symmetric the way a nil slice is. Reading a nil map is always
safe: `m[k]` returns the zero value, the comma-ok form reports `false`, `len(m)`
is `0`, and `range` iterates zero times. Writing to a nil map panics with
`assignment to entry in nil map`. This bites most often when a struct has a map
field that a constructor forgot to initialize; the zero-value struct reads fine
in tests and then panics a worker the first time it records something. The fix is
to initialize the map in the constructor, or to lazily `make` it on first write.
Never assume a zero-value map is writable.

## Backing-array aliasing and the three-index expression

`append` may reuse the source slice's backing array when its capacity allows.
Two slices built by appending to the same base with spare capacity therefore
write to the same underlying storage, and the second append overwrites the first.
The standard defenses are: the full three-index slice expression `base[:n:n]`,
which sets the derived slice's capacity to its length so the next `append` is
forced to allocate fresh storage; `slices.Clip`, which trims capacity down to
length for the same effect; and `slices.Clone`, which makes an independent copy
up front. Deriving per-request data by appending to a shared package-level base
with spare capacity is the classic version of this bug, and because two requests
run on two goroutines it is a data race that `-race` will flag, not just a logic
error.

## Capacity, cost, and the empty result of a filter

Starting from a nil slice and growing it with repeated `append` is always
*correct* — but it reallocates roughly log2(N) times as capacity doubles, each
reallocation copying the elements so far. When the final size is known,
`make([]T, 0, N)` allocates the backing array once, and `slices.Grow` extends an
existing slice's capacity in one step; both cut allocations and GC pressure in a
hot path. Measure the difference with `testing.AllocsPerRun` or a benchmark
rather than guessing.

Lifetime is the other half of the story. Filtering a slice of pointers in place
with the `s[:0]` idiom or `slices.Delete` leaves the removed elements sitting in
the now-unused tail capacity, and those pointers keep the pointed-to objects
alive against the garbage collector. Zero the tail (with the `clear` builtin over
the removed region) or use `slices.DeleteFunc`, which zeroes removed elements for
you. The legitimate result of filtering everything out is frequently an
empty-but-non-nil slice — which loops right back to the boundary question of what
that empty result serializes to.

## Common Mistakes

### Returning nil from a repository where the API then serializes null

Wrong: a `List` method returns `[]T(nil)` on zero matches, the HTTP layer
marshals it, and a paging client that expects `[]` receives `null` and breaks.
Fix: normalize to a non-nil slice at the port boundary — `make([]T, 0)` once,
where the repository hands results to the API — not scattered defensively through
every caller.

### Assuming omitempty preserves an explicit empty list

Wrong: tagging a field `omitempty` and expecting `[]` to appear on the wire.
`omitempty` drops a non-nil empty slice just as it drops nil, so the field
vanishes. Fix: use `omitzero` (Go 1.24+) when a non-nil empty slice must render
as `[]`.

### Modeling a PATCH body with a plain slice and losing the tri-state

Wrong: a `Tags []string` field that cannot tell "key absent" (leave unchanged)
from `null` (clear) from `[]` (set empty). Fix: use `json.RawMessage` (or a
custom wrapper) and inspect the raw bytes; a bare `*[]T` still cannot separate
absent from `null`.

### Writing to a nil map

Wrong: a struct's map field left uninitialized by the constructor, panicking with
`assignment to entry in nil map` on the first write in production. Fix: initialize
the map in the constructor, or lazily `make` it before the first write.

### Handing out an internal slice or map directly

Wrong: a getter returns the package's internal slice; a caller's `append` or
index write corrupts shared state, and concurrently it is a data race. Fix:
return `slices.Clone` / `maps.Clone`, or document the value as immutable and mean
it.

### Appending to a shared base with spare capacity

Wrong: building per-request values by appending to a package-level base that has
extra capacity, so concurrent requests clobber each other through the shared
backing array. Fix: `base[:n:n]`, `slices.Clip`, or `slices.Clone` before
appending.

### Comparing slices with == or reflect.DeepEqual out of habit

Wrong: `s1 == s2` (a compile error for slices) or reaching for
`reflect.DeepEqual`. Fix: `slices.Equal` for content comparison; reserve `== nil`
for the identity question.

### Leaking pointers in the tail of an in-place filter

Wrong: filtering a `[]*T` down with `s[:0]` or `slices.Delete` and leaving dead
pointers in the tail capacity, pinning the objects against GC. Fix: `clear` the
removed region, or use `slices.DeleteFunc`, which zeroes removed elements.

### Treating nil and empty as interchangeable without documenting the contract

Wrong: never stating which one "no value" maps to, so the choice is whatever the
zero value happened to be. Fix: state it explicitly in the function's contract
and pin it with a test.

### Growing a hot-path accumulator from nil when the size is known

Wrong: starting every transform from a nil slice and append-growing, multiplying
allocations under load. Fix: `make([]T, 0, N)` or `slices.Grow` when N is known;
confirm the win with `testing.AllocsPerRun`.

Next: [01-json-null-vs-empty-encoder.md](01-json-null-vs-empty-encoder.md)
