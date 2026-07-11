# Slices: Creation, Append, and Capacity — Concepts

The slice is Go's most-used data structure, and it is also the origin of a
disproportionate share of subtle production bugs. The reason is that a slice is
not a container; it is a three-word *header* — a `(pointer, len, cap)` triple —
that describes a *view* onto a backing array it does not own exclusively.
Passing a slice, reslicing it, storing it in a cache, or handing it to a
downstream function all copy that header while sharing the array underneath. A
senior backend engineer has to reason about a slice the way they reason about a
raw pointer with a length: who owns the backing array, when does `append` decide
to allocate a fresh one, and which retention paths must take a copy. Get that
wrong and you get the classic failures — a cached value silently corrupted by an
unrelated caller's `append`, bytes read from a pooled buffer that mutate out from
under you on the next read, or a two-element slice that pins a ten-megabyte array
alive because nobody clipped the capacity. This file is the model; internalize it
once and each of the exercises that follow is an application of it.

## Concepts

### The slice header is a value; the backing array is shared

A slice value is exactly three machine words: a pointer to the first element of
the view, the length (`len`), and the capacity (`cap`). Assigning a slice,
passing it to a function, or returning it copies those three words — never the
elements. So two slice variables can point at the *same* backing array, and a
write through one is visible through the other wherever their views overlap. This
is why a function that takes a `[]byte` and writes into it mutates the caller's
data, and why storing a slice you received from a caller can mean storing a
window into memory the caller still owns and may reuse.

`len` is the number of elements the view exposes. `cap` is the number of elements
from the slice's pointer to the *end of the backing array*. The invariant is
`0 <= len <= cap`. A `make([]int, 5, 10)` has five addressable elements and room
to grow to ten before the array must change.

### append writes in place until it can't, then reallocates

`append(s, x)` is the pivot of the whole model. If `len(s) < cap(s)`, there is
spare room in the backing array: `append` writes `x` at index `len(s)`, returns a
header with `len+1`, and the backing array is unchanged — so the returned slice
*aliases* the input. If `len(s) == cap(s)`, there is no room: `append` allocates
a new, larger backing array, copies the existing elements over, writes `x`, and
returns a header pointing at the new array — so the returned slice does *not*
alias the input. Growth is roughly geometric, but the exact growth factor is an
implementation detail, not a language contract, and it has changed between
releases; never write code that depends on a specific resulting capacity.

The consequence that trips people up: whether `append` reallocated is *not
observable from the call site* without checking `cap`. The same line of code
aliases the input on one call and detaches from it on the next, depending only on
whether there happened to be spare capacity. That non-determinism is the root of
most append bugs, and it is exactly why the rule "always assign the result back —
`s = append(s, x)`" exists. Code that mutates through an *old* header after an
`append` has undefined sharing semantics: sometimes the write lands in the same
array the new header sees, sometimes not.

### The three-index expression bounds capacity for safe hand-offs

`s[low:high]` produces a view with `len == high-low` and `cap == cap(s)-low` —
that is, its capacity runs all the way to the end of the original backing array.
So if you hand `s[i:j]` to a consumer and that consumer appends, the append can
fall into the still-spare capacity *past* index `j` and overwrite bytes that
belong to something else sharing the array.

The three-index form `s[low:high:max]` fixes this: it sets `cap == max-low`. Using
`s[i:j:j]` yields a view whose `cap == len`, so the consumer's very first append
that exceeds the length finds no spare capacity and is *forced* to allocate a
fresh array and copy — it cannot reach back into the shared storage. This is the
correct, safe way to hand out a sub-slice you do not want a downstream `append` to
clobber: wire-protocol frame payloads, a row's column view, any window into a
shared read buffer.

### Preallocation turns N reallocations into one

`make([]T, 0, n)` creates an empty slice with capacity `n` already reserved. If
you then `append` up to `n` elements, every append writes in place and the
backing array is allocated exactly once. The naive alternative — starting from a
`nil` slice and appending — reallocates and copies each time capacity is
exhausted, doing O(log n) allocations and O(n) total copying as the array
doubles. Whenever you know a bound on the final size — a SQL `LIMIT`, a row count
from the driver, a `Content-Length`, the number of items in a batch — size the
slice from it. This is one of the highest-leverage, lowest-effort performance
habits in Go backend code.

### s[:0] retains the array for reuse and in-place filtering

`s[:0]` reslices to zero length while keeping the *same* backing array and its
full capacity. Two idioms rest on this. First, buffer reuse across cycles: a
batcher that does `buf = buf[:0]` after a flush keeps appending into the same
array instead of allocating a new one every batch. Second, the zero-allocation
filter-in-place: `keep := s[:0]; for _, e := range s { if pred(e) { keep =
append(keep, e) } }` compacts the surviving elements to the front of the original
array with no new allocation, because every `append` lands within the existing
capacity.

### Shrinking a slice of pointers leaks unless you clear the tail

The filter-in-place and delete idioms shorten `len` but leave the vacated tail
slots of the backing array still holding their old values. For a slice of
pointers (or of structs containing pointers), those slots keep the removed objects
reachable through the array, so the garbage collector cannot reclaim them even
though they are logically deleted — a slow, silent memory leak in any long-lived
slice like a session table or connection pool. Fix it by explicitly nil-ing the
freed slots, or `clear(s[newLen:len(s)])`, before shrinking the view. A logical
delete that skips this is a memory leak.

### The slices package: Clone, Grow, Clip, Delete

The `slices` package gives named, correct implementations of the idioms above.
`slices.Clone(s)` returns a shallow copy backed by a fresh array — the standard
way to detach a value you are about to retain from the caller's storage.
`slices.Grow(s, n)` returns a slice with the same `len` but capacity guaranteed
for at least `n` more appends without reallocating — preallocation when you start
from an existing slice. `slices.Clip(s)` returns `s[:len(s):len(s)]`, trimming
capacity down to length so the excess tail of a large backing array can be freed
before you hand the slice to a long-lived owner. `slices.Delete(s, i, j)` removes
the range `[i:j)` preserving order by shifting the tail down with a copy (O(n));
the unordered *swap-remove* — move the last element into the gap — is O(1) but
reorders. Choose by whether order matters, and remember both must clear the now-
unused tail slots for pointer slices. Do not confuse `slices.Clip` (releases
excess capacity) with `slices.Compact` (deduplicates *adjacent equal* elements);
they solve unrelated problems.

### Retaining bytes from a pooled or reused buffer requires a copy

`bufio.Reader`, `net.Conn.Read`, and `sync.Pool`-managed byte buffers all hand
you a slice into memory they intend to reuse. The bytes are valid only until the
next read or the buffer's return to the pool; after that they are overwritten. If
you store that slice — put it in a cache, append it to a batch, send it to another
goroutine — you have a use-after-overwrite bug that manifests as data changing
after you thought you captured it. The fix is to copy before retaining:
`bytes.Clone(b)`, `append([]byte(nil), b...)`, or `dst := make([]byte, len(b));
copy(dst, b)` all produce independent storage you own.

### nil versus empty

A `nil` slice and a non-nil empty slice (`make([]T, 0)` or `[]T{}`) both have
`len == 0` and both are safe to `append` to, index-loop over, and pass around. The
only place the distinction matters is where it becomes observable — most commonly
JSON, where a `nil` slice marshals to `null` and an empty non-nil slice marshals
to `[]`. Reach for the distinction only when a serialization boundary cares;
otherwise treat them interchangeably and prefer `var s []T` (nil) as the zero
value you append onto.

## Common Mistakes

### Ignoring append's return value

Wrong: writing `append(s, x)` as a statement, discarding the result. When
`append` reallocates, the growth is lost entirely (the new element is written to a
throwaway array); when it does not reallocate, you have silently mutated shared
backing storage while the length the rest of your code sees is stale.

Fix: always `s = append(s, x)`. The result carries both the possibly-new pointer
and the new length.

### Reasoning about aliasing from a false invariant

Wrong: assuming `append` *always* reallocates (so "the input is safe to keep
using") or *never* does (so "the append is visible everywhere"). Either belief
lets you write code whose correctness depends on which array a shared write lands
in — and that depends on spare capacity, which you usually cannot see.

Fix: treat the alias/no-alias outcome of `append` as unknowable at the call site.
If you need isolation, force it with `slices.Clone` or a three-index cap; if you
need sharing, do not append through a separate header at all.

### Caching a sub-slice of a larger buffer without copying

Wrong: storing `big[i:j]` in a cache or returning it, when `big` is still owned by
someone who will `append` to or overwrite it. A later append into `big` can reach
into the shared capacity and rewrite the "cached" bytes.

Fix: store `slices.Clone(big[i:j])`, or hand out `big[i:j:j]` so the consumer's
append is forced to copy. The cached value must own isolated storage.

### Using len(backing) as the logical size after wraparound

Wrong: in a ring buffer, treating `len(r.data)` (which equals `cap` once the array
is fully written) as the number of live elements. After wraparound the array is
full-length whether the buffer holds one element or is at capacity.

Fix: track logical size in a separate counter, as the ring exercise's `size` field
does. `len` of the backing array is not the ring's length.

### Deleting from a pointer slice without clearing the tail

Wrong: `s = slices.Delete(s, i, i+1)` (or the `append(s[:i], s[i+1:]...)` form) on
a `[]*T` and stopping there. The old last element is still referenced by the now-
unused tail slot of the backing array and cannot be collected.

Fix: nil the vacated slot(s) — `s[len(s)-1] = nil` for swap-remove, or
`clear(s[newLen:])` — before or as part of shrinking.

### Retaining pooled/reused bytes without copying

Wrong: `store(buf)` where `buf` came from `bufio`, a `net.Conn` read, or a
`sync.Pool`. The next read overwrites it and your stored data changes.

Fix: copy first — `bytes.Clone(buf)`, `append([]byte(nil), buf...)`, or
`make`+`copy` — then store the copy.

### Growing by repeated single appends when the size is known

Wrong: appending one element at a time from `nil` when you already know the final
count, turning one allocation into O(log n) allocations plus O(n) copying.

Fix: `make([]T, 0, n)` or `slices.Grow(s, n)` up front, sized from the known
bound.

### Believing you can add capacity without a new array

Wrong: thinking `s = s[:cap(s)]` or appending to a re-lengthened slice increases
capacity. Capacity is fixed at allocation; only a new backing array can change it.
`s[:cap(s)]` merely exposes the already-allocated tail.

Fix: to genuinely enlarge capacity, allocate a new array — `slices.Grow`, a bigger
`make` plus `copy`, or letting `append` reallocate.

### Handing out a two-index slice to a consumer that appends

Wrong: returning `s[i:j]` (capacity runs to the end of `s`) to code that will
append, letting that append overwrite bytes past `j` in the shared array.

Fix: return `s[i:j:j]`, which caps capacity at the length, so the consumer's first
over-length append must allocate its own array.

### Confusing Clip with Compact

Wrong: reaching for `slices.Compact` to release excess capacity, or `slices.Clip`
to deduplicate. `Compact` removes *adjacent equal* elements; `Clip` trims capacity
to length. They are unrelated jobs.

Fix: `Clip` to drop the spare tail before long-lived retention; `Compact` to
collapse runs of duplicates in a sorted or run-grouped slice.

Next: [01-ring-buffer-fixed-capacity.md](01-ring-buffer-fixed-capacity.md)
