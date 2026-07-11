# Slice Internals: Headers, Growth, and Aliasing in Production Buffers — Concepts

Slices are where hot-path backend code quietly bleeds allocations and grows
subtle correctness bugs. A request batcher that should be allocation-free
reallocates on every flush. A config accessor returns a view into its own
storage and a caller mutates it. A per-tenant fan-out appends into one
sub-slice and silently overwrites the next tenant's data. A connection pool
evicts an entry and leaks the pointer because the freed tail slot still holds
it. None of these are exotic — they all come from misunderstanding the three
machine words behind every slice and the rules of `append`. This file is the
conceptual foundation. Read it once and you have everything you need to reason
through the nine independent exercises that follow, each framed as a real
production artifact.

## Concepts

### A slice header is three machine words

A slice value is not the data. It is a small three-word header: a **pointer**
to the first element of its view into a backing array, a **length** (what `len`
reports), and a **capacity** (what `cap` reports, the number of elements from
the pointer to the end of the backing array). Assigning a slice, passing it to
a function, or returning it copies those three words by value. The backing
array is **shared, not copied**. This is the single fact from which every other
behavior in this lesson follows: two slice headers can point at the same array,
so a write through one is visible through the other.

```go
s := make([]int, 3, 8) // header: ptr->arr, len=3, cap=8
t := s[1:2]            // header: ptr->arr+1, len=1, cap=7 — SAME array
```

The header is observable. `unsafe.SliceData(s)` returns a pointer to the
underlying array (`nil` for a `nil` slice), which lets a diagnostic answer the
exact question you ask when hunting an aliasing bug: do these two slices share
a backing array?

### append reallocates only when len == cap

`append` writes new elements starting at index `len`. If there is spare
capacity (`len < cap`), it writes **in place** into the existing backing array
and returns a header with a larger length over the same array. Only when
`len == cap` does it allocate a **new** backing array, copy the existing
elements, write the new ones, and return a header over the new array. That
in-place write is exactly why `append` can mutate data visible through another
slice: if `s` and `t` share an array and `t` has spare capacity, `append(t, x)`
scribbles into `s`'s region. "append never touches its source" is false, and
believing it is a frequent cause of cross-goroutine data races.

### Growth is amortized O(1), not exactly 2x

When `append` reallocates, it does not grow by one element — that would make N
appends cost O(N^2). It grows by a factor, so a run of N appends triggers only
O(log N) reallocations and costs O(N) total (amortized O(1) per append). But
the factor is **not a hard 2x**. The Go runtime roughly doubles small slices,
then transitions toward ~1.25x growth for large ones, and rounds every request
up to a size class. Never hard-code "the capacity doubles" into your reasoning
about memory; measure with `cap` or `testing.AllocsPerRun`.

### The three-index expression and slices.Clip cap the capacity

The full slice expression `s[low:high:max]` produces a slice with
`len == high-low` and `cap == max-low`. Setting `max == high` yields a slice
whose capacity equals its length, so the very next `append` **must** allocate a
new array instead of scribbling into shared spare capacity. `slices.Clip(s)`
does the same thing by returning `s[:len(s):len(s)]`. This is the canonical fix
for spare-capacity aliasing: hand out `buf[i:j:j]` (or a clipped slice) so a
caller's `append` can never corrupt the neighbor.

### s = s[:0] reuses the array; s = nil discards it

`s = s[:0]` sets length to zero but keeps the same pointer and the same
capacity. The backing array survives, so the next `append` reuses it with zero
allocations — this is the reuse idiom behind `sync.Pool` buffers and every
allocation-free flush cycle. `s = nil` throws the header away: pointer nil,
len 0, cap 0. The array becomes garbage and the next `append` allocates a fresh
one. These two resets look interchangeable and are semantically opposite. Note
the trade-off: `s[:0]` retains the memory (the point, for reuse — but a leak if
unintended).

### make length versus make capacity

`make([]T, n)` allocates a backing array of `n` **zero values** and sets length
to `n`. `make([]T, 0, n)` allocates the same array but sets length to `0`.
Appending to the first form appends **after** the n zeros, so you get n leading
zero values and, once you pass n, a reallocation. Appending to the second form
fills the array from the front with no reallocation until you exceed n.
`make([]T, 0, n)` is the correct preallocation for a known-size append burst;
`make([]T, n)` then `append` is the classic off-by-a-whole-array bug.

### slices.Grow pre-extends without changing length

`slices.Grow(s, n)` guarantees room for `n` more appends without changing `len`:
it ensures `cap(s) >= len(s)+n`, reallocating at most once. It is the tool for a
hot path that knows how many elements are coming but wants to keep appending
through existing code — collapse a series of reallocations into one up-front
grow, then append freely.

### slices.Delete and slices.Insert reslice in place and zero the tail

`slices.Delete(s, i, j)` removes elements `[i:j)` by shifting the tail left and
returning a shorter slice; `slices.Insert(s, i, v...)` shifts right to open a
gap. Both operate on the same backing array. Critically, since Go 1.22
`slices.Delete` **zeroes the freed tail slots**. For a slice of pointers that
matters: after a naive manual shift (`copy(s[i:], s[i+1:]); s = s[:len-1]`) the
old last slot still holds a live pointer that the GC cannot collect — a
pointer-element leak in a long-lived slice. `slices.Delete` nils that slot so
the evicted pointer becomes GC-eligible.

### Two distinct memory leaks: capacity pinning and tail retention

There are two ways slices defeat the garbage collector, and they are different.
(a) **Capacity pinning**: a small sub-slice keeps the *entire* backing array
alive because its pointer and capacity still reference it — `header := big[:8]`
retains all of `big`. The fix is `slices.Clone(big[:8])` to copy out just what
you need. (b) **Tail retention**: shrinking a slice's length with `s = s[:n]`
leaves pointer-typed elements in the region `[n:cap]` reachable through the
array, so the pointed-at objects survive. The fix is to zero the tail (which
`slices.Delete` does for you). Both keep memory alive that logically should be
freed.

### Defensive copying at API boundaries

Returning a sub-slice of your internal storage (`return s[a:b]`) hands the
caller a header into your own array; they can mutate your state through it, and
two callers can race on it. At an API boundary — an accessor on a config store,
a snapshot method on a buffer — return `slices.Clone(...)` or copy into a fresh
slice. The through-line of this whole lesson is control: control allocation on
the write path (preallocate, reuse with `s[:0]`, grow once), and control
aliasing on the read path (clip what you hand out, clone what you return).

## Common Mistakes

### Assuming append never mutates its source

Wrong: treating `append(view, x)` as harmless when `view` is a sub-slice of a
larger buffer. If `view` has spare capacity in the shared array, the write lands
in the neighbor's region and corrupts it — and if another goroutine is reading
that region, it is a data race.

Fix: hand out `buf[i:j:j]` or `slices.Clip(buf[i:j])` so the next append is
forced to allocate. Clip at the boundary where you split a shared buffer.

### Returning a sub-slice of internal storage

Wrong: an accessor does `return s.items[a:b]`. The caller now holds a view into
your storage and can mutate it (or you overwrite it under them on the next
write).

Fix: return `slices.Clone(s.items[a:b])` or copy into a fresh slice. Pay the
copy at the boundary; keep internal storage private.

### Using len(backingSlice) as the logical size

Wrong: treating `len` of a fixed-capacity reused buffer as the item count. Once
the buffer is full, `len == cap`, not the number of logical items, and a
`len == 0` "is it empty" check is always false.

Fix: track logical size in a separate field, as the ring-buffer history in the
first exercise does with its `size` counter.

### Believing s = s[:0] frees memory

Wrong: resetting with `s = s[:0]` expecting the backing array to be collected.
It keeps the array and the full capacity alive.

Fix: that retention is correct for a reuse buffer (that is the point). If you
actually want the memory freed, use `s = nil`. Know which one you mean.

### Deleting from a slice of pointers with a naive shift

Wrong: `copy(pool[i:], pool[i+1:]); pool = pool[:len(pool)-1]`. The old last
slot still holds the evicted pointer; the GC cannot reclaim what it points to.

Fix: `slices.Delete(pool, i, i+1)` — it zeroes the freed tail slot (Go 1.22+).
Or nil the slot explicitly before reslicing.

### Preallocating length instead of capacity

Wrong: `args := make([]any, n)` then `append(args, v)`. You get n leading zero
values, the real data after them, and still a reallocation.

Fix: `args := make([]any, 0, n)` then append — length 0, capacity n, filled
from the front, no reallocation until you exceed n.

### Hard-coding a 2x growth assumption

Wrong: reasoning about capacity or memory as if `append` always doubles. The
factor is size-dependent (~2x small, ~1.25x large) and rounded to a size class.

Fix: measure. Read `cap` after growth, or count allocations with
`testing.AllocsPerRun`. Do not assert an exact capacity you did not observe.

### Building rows from one reused scratch buffer without cloning

Wrong: `scratch = scratch[:0]; scratch = append(scratch, fields...);
rows = append(rows, scratch)` in a loop. Every row aliases the same backing
array, so all rows end up showing the last record.

Fix: `rows = append(rows, slices.Clone(scratch))` so each row owns its data.

Next: [01-bounded-event-history.md](01-bounded-event-history.md)
