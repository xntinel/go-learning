# Slice Expressions and Sub-Slicing — Concepts

A slice in Go is three machine words: a pointer to a backing array, a length, and
a capacity. Every slice expression you write produces another such header that
usually points *into an array it does not own*. That single fact is the root of an
entire family of production bugs: a middleware that serves request A's identity to
request B, a batcher whose reused buffer corrupts in-flight events under load, a
handler that pins a multi-megabyte decoded payload alive by returning a four-line
summary, a zero-copy CSV ingest that mangles fields the instant its scanner
refills. None of these are exotic. They are what happens when an engineer reasons
about a slice as if it were a value (a copy) when it is actually a view (an alias).

This file is the conceptual foundation for the nine independent exercises that
follow. Read it once and you have the mental model each exercise then forces you to
apply under a concrete on-call-shaped scenario. The through-line is a single
decision rule you will apply again and again: *does this value outlive its producer,
or could a consumer mutate it?* If yes, detach it (`slices.Clone`, the three-index
full-slice expression, or a copy into a right-sized buffer). If it is a deliberately
shared window, bound its capacity so an `append` cannot escape into data you still
own.

## The two-index expression and inherited capacity

`s[low:high]` produces a slice whose length is `high - low` and — this is the part
that surprises people — whose capacity is `cap(s) - low`, not `high - low`. The
result silently inherits *all* the remaining capacity of the source past `low`.

```
s   := make([]int, 5, 10)   // len 5, cap 10
sub := s[1:3]               // len 2, cap 9  (10 - 1), NOT 2
```

`sub` looks like a two-element slice, but it has room to grow through the source's
backing array up to index 9. An `append(sub, x)` writes to `s[3]` in place,
mutating the source without reallocating. This inherited capacity is the mechanism
behind most aliasing surprises: the sub-slice is short, so it *reads* like an
isolated little window, but it is welded to the source's array for another seven
slots.

For a two/three-index expression, `high` may range up to `cap(s)`, not merely
`len(s)`. `s[2:8]` is legal on the `s` above even though `len(s)` is 5. The
expression panics only when the indices are out of order or exceed `cap(s)`
(`0 <= low <= high <= cap(s)`). This is why user-controlled offsets must be clamped
*before* slicing: an unchecked `lines[offset:offset+limit]` will panic on a
past-end offset, and clamping to `len` is the fix.

## The three-index expression is the isolation tool

`s[low:high:max]` sets the capacity explicitly: length is `high - low`, capacity is
`max - low`, under the constraint `0 <= low <= high <= max <= cap(s)`. The important
special case is `max == high` — the *full-slice expression* `s[low:high:high]` —
which makes `len == cap`. A slice with `len == cap` has no spare room, so the very
next `append` to it is forced to allocate a fresh backing array. The producer's
data is now unreachable through the returned slice.

```
buf := make([]int, 4, 8)
a := buf[0:2]     // len 2, cap 8  -> append clobbers buf[2], buf[3]
b := buf[0:2:2]   // len 2, cap 2  -> append reallocates, buf untouched
```

This is the primary tool for producer-side isolation when you want to hand out a
*view* but forbid the consumer's `append` from reaching back into your array.
`slices.Clip(s)` is exactly `s[:len(s):len(s)]` — the same trick applied to a whole
slice.

## Capacity, not length, is the sharing boundary

The single most useful reframing in this lesson: two slices alias each other when
their *capacities* overlap the same backing array, regardless of their lengths.
Two slices can compare equal in length and still clobber each other on `append`,
because length says nothing about how far into the shared array either one can
write. When you reason about "will these two slices interfere?", reason about
`cap`, not `len`. A four-element slice with `cap 1024` is a four-element window onto
a thousand-element array, and it can overwrite any of them.

## A sub-slice is a view; owned data must be copied

Reslicing never copies elements. `s[low:high]`, `s[k:]`, and `s[:n]` all produce a
new *header* over the *same* array. So any value that (a) outlives the frame of the
code that produced it, or (b) might be mutated by whoever receives it, must be an
owned copy, not a sub-slice. The canonical detach primitives:

- `slices.Clone(s)` — defined as `append(s[:0:0], s...)`; copies exactly `len(s)`
  elements into a fresh, right-sized array (`cap == len`). This is the idiomatic
  isolate-and-detach.
- `append([]T(nil), s...)` — the pre-generics form of the same thing, still common.
- `copy(dst, s)` into a right-sized `dst` — but see the mistake below about
  mis-sizing `dst`.

`slices.Clone` does double duty: besides isolating, its right-sized array *breaks
backing-array retention*, which is the fix for the large-array pinning leak.

## Head/tail trimming does not free memory

`s = s[k:]` (drop a head) and `s = s[:n]` (drop a tail) move no data and free no
memory. The trimmed-away region is still reachable through the original backing
array as long as the resulting slice is alive. This is how a tiny view pins a huge
array: return `bigSlice[:4]` from a handler and keep it, and the entire multi-megabyte
array stays live for the lifetime of that four-element summary — a real, silent
memory leak that shows up as a slow heap climb in `pprof`, never as an error. The
fix is to copy the head into a right-sized array (`slices.Clone(bigSlice[:4])`) so
the big array becomes collectable.

The same non-freeing behavior underlies the sliding-window trap: a rate limiter
that trims expired timestamps with `events = events[firstFresh:]` never lowers the
low bound of the backing array, so the array can only grow. You reclaim it with an
occasional `slices.Clone` / `slices.Clip` compaction.

## In-place filtering and the pointer-zeroing hazard

The zero-allocation filter idiom reuses the backing array:

```
out := s[:0]
for _, v := range s {
	if keep(v) {
		out = append(out, v)
	}
}
```

`out` starts at length 0 but shares `s`'s array, so re-appending kept elements
overwrites the front of `s` in place — no allocation. But the tail `s[len(out):]`
now holds *stale* elements: for a slice of pointers or structs-containing-pointers,
those are the removed entries (or duplicates of kept ones), and they remain
reachable through the array, so the GC cannot reclaim what they point at. You must
zero them: `clear(s[len(out):])`. The standard-library helpers
`slices.Delete`, `slices.DeleteFunc`, `slices.Insert`, and `slices.Replace` shift
in place, may reallocate, and — for element types that contain pointers — zero the
vacated tail for you. A hand-rolled `append(s[:i], s[i+1:]...)` shifts correctly but
does *not* zero the old tail slot, leaking the pointer that used to live there.

## Ephemeral buffers hand out short-lived views

`bufio.Scanner.Bytes()`, a pooled `[]byte` from a `sync.Pool`, a reused scratch
slice — all of these hand you a sub-slice that is valid only until the next reuse.
`Scanner.Bytes()` explicitly documents that its result "may be overwritten by a
subsequent call to Scan". Split such a buffer into fields with byte-index slice
expressions on the hot path (zero allocation), but the moment you keep a field past
the next `Scan()`, `bytes.Clone` it. Retaining an ephemeral view is the number-one
zero-copy-parsing bug.

## Return an empty slice, not nil, at API boundaries

For an empty result, returning a non-nil empty slice (`[]T{}` or `s[:0]`) rather
than `nil` keeps caller iteration and marshaling uniform — `range` handles both, but
`encoding/json` renders `nil` as `null` and an empty slice as `[]`. When the empty
distinction crosses an API boundary (a JSON response, a gRPC field), prefer the
non-nil empty slice so the wire shape is stable.

## Common Mistakes

### Returning a sub-slice from a producer that owns the data

Wrong: `return lines[low:high]` from a function that owns `lines`. The caller can
mutate your source through the returned slice, or an `append` can overwrite source
elements via inherited capacity. Fix: `return slices.Clone(lines[low:high])` (or
`append([]T(nil), lines[low:high]...)`).

### Pre-sizing with make then copy, and losing the tail

Wrong: `out := make([]T, high-low); copy(out, src)`. `copy` stops at the shorter of
the two lengths, so if `src` is shorter than `high-low` the result has zero-valued
trailing elements you did not intend. Fix: `slices.Clone(src)` or
`append([]T(nil), src...)` copy exactly the source range, whatever its length.

### Two-index cut of a reused buffer

Wrong: handing out `buf[i:j]` from a shared buffer. The sub-batch inherits `buf`'s
spare capacity, so the consumer's `append` silently clobbers later, not-yet-flushed
elements still living in `buf`. Fix: cut with the three-index `buf[i:j:j]` so
`len == cap` and any `append` reallocates.

### Keeping a small head of a large slice

Wrong: `summary := decoded[:4]` and storing `summary`. The full original array
stays alive as long as `summary` is referenced. Fix: `summary := slices.Clone(decoded[:4])`
to release the big array.

### In-place filter without clearing the tail

Wrong: `s = s[:0]; ...append kept...` and stopping there. Removed pointer elements
in `s[len:cap]` stay reachable and leak. Fix: `clear(s[newLen:])` before reslicing
down, or use `slices.DeleteFunc`, which zeros the tail for you.

### Retaining Scanner.Bytes past the next Scan

Wrong: appending `sc.Bytes()` (or a sub-slice of it) into a slice you keep. The
bytes mutate underneath you on the next `Scan`. Fix: `bytes.Clone` the fields you
retain.

### Assuming append always allocates

Wrong: believing "I passed a slice down; append there can't affect mine." `append`
reallocates only when `len == cap`. A sub-slice with spare capacity mutates the
shared array in place — the source of every "my input changed after I passed it
down" bug. Fix: reason about `cap`; hand out `buf[i:j:j]` or a clone.

### Hand-rolled deletion of a pointer slice

Wrong: `s = append(s[:i], s[i+1:]...)` for `[]*T`. It shifts correctly but leaves a
dangling pointer in the old last slot, which the GC cannot reclaim. Fix:
`s = slices.Delete(s, i, i+1)` (it zeros the tail), or clear the slot yourself.

### Confusing len and cap when reasoning about isolation

Wrong: concluding two slices are independent because their lengths differ. They can
still alias and corrupt each other because their capacities overlap the same array.
Fix: isolation is a statement about capacity and the backing array, not length.

Next: [01-log-window-pagination.md](01-log-window-pagination.md)
