# Copy, Three-Index Slice Expressions, and Ownership Boundaries — Concepts

Slice aliasing is one of the most expensive classes of production bug in Go. Two
slices silently share one backing array, and an `append` or an index-write
through one of them corrupts the other. The corruption is non-local: it surfaces
far from the line that caused it, often across a request or goroutine boundary,
as intermittent data leakage between tenants, as a "race" that `-race` sometimes
cannot see (aliasing is not always a data race — a single goroutine can logically
corrupt its own data), or as heap retention where a tiny live slice pins a
multi-megabyte backing array alive. The discipline that prevents all of this is
the same one every time: at every API boundary, decide who owns the backing
array. When a slice crosses a trust or lifetime boundary — a repository read, a
cache read, a scanner token, a header snapshot, a value handed to an async sink —
either copy it defensively or bound its capacity so the consumer's next `append`
is forced to reallocate. This file is the conceptual foundation; read it once and
you have everything you need to reason through each independent exercise that
follows.

## Concepts

### What `copy` actually does

`copy(dst, src)` copies exactly `min(len(dst), len(src))` elements and returns
that count. It allocates nothing — the destination backing array must already
exist with room. The critical, endlessly-misremembered detail is that the bound
is the *length* of `dst`, not its capacity. `copy(make([]int, 0, 100), src)`
copies zero elements, because `len(dst)` is zero, even though the capacity is
100. If you want `copy` to fill a buffer, size that buffer by length:
`make([]int, n)`.

`copy` behaves like C's `memmove`, not `memcpy`: it handles overlapping source
and destination correctly. That property is why `copy(s[i:], s[i+1:])` is the
canonical way to delete element `i` in place by shifting the tail left one slot,
and it is exactly what `slices.Delete` does under the hood.

### Copying a slice: `append([]T(nil), src...)` and `slices.Clone`

`append([]T(nil), src...)` produces an independent slice with a fresh backing
array holding the same elements. `slices.Clone(src)` does the same thing and is
the modern, intent-revealing form; prefer it in new code. Both preserve `len`.
`slices.Clone` makes no promise about `cap` — the result "may have additional
unused capacity" — so never assert on `cap(clone)`, only on its contents and
length. The point of either is the same: after cloning, the consumer can sort,
filter, append to, or truncate the result without the source noticing.

### The three-index slice expression `s[low:high:max]`

The full slice expression sets `len = high - low` and `cap = max - low`. The form
you reach for constantly is `s[lo:hi:hi]`, which makes `cap == len`. A slice with
no spare capacity forces the consumer's very next `append` to allocate a new
backing array instead of writing into the shared one. That is how you hand a
sub-region of a larger buffer to a downstream consumer that may append, without
letting its `append` stomp the adjacent bytes that are still live in the shared
array. Contrast the two-index `s[lo:hi]`: it inherits the parent's capacity, so
an `append` writes straight into whatever lives after `hi` in the same array.

### Reslicing never copies

Every slice expression — two-index or three-index — aliases the backing array. It
copies nothing. Capacity bounding with `s[lo:hi:hi]` changes the *behavior* of a
future `append`; it is not memory safety. A consumer still holds a window into
the shared array and can read and overwrite any index in `[0, cap)`. Bounding
capacity protects you only from `append`-driven reallocation stomps, not from a
deliberate or accidental index write. The real safety boundary is a copy.

### Ownership boundaries are the whole discipline

Whenever a slice crosses a trust or lifetime boundary, decide who owns the
backing array and act on that decision. A repository or cache that returns its
internal slice has handed the caller a live handle to canonical state; the caller
sorting or appending to it corrupts the store. Return `slices.Clone` instead. A
protocol parser that hands out a sub-slice of its read buffer has to bound
capacity or copy, or the consumer's `append` corrupts the next field. A function
that returns a slice to an untrusted caller owns nothing it does not copy. The
question "who owns this backing array?" should be a reflex at every signature.

### View-into-an-internal-buffer APIs: copy before you retain

`bufio.Scanner.Bytes()` returns a view into the scanner's internal buffer that is
overwritten on the next call to `Scan`. It is valid only until then. If you store
the returned token and keep scanning, every stored token ends up pointing at the
same buffer, so they all show the last line's contents. To retain a token past
the next `Scan`, copy it: `bytes.Clone(tok)`, `slices.Clone(tok)`, or convert to
`string(tok)` (which allocates an immutable copy). The same rule applies to any
"here is a view into memory I will reuse" API.

### `slices.Delete` / `slices.DeleteFunc` zero the freed tail

`slices.Delete(s, i, j)` and `slices.DeleteFunc(s, pred)` shift the survivors
down with `copy` and return a shorter slice. Since Go 1.22 they also zero the
elements between the new length and the old length. For element types that are
pointers or contain pointers (`[]*Session`, `[]struct{ p *T }`), that zeroing is
what stops a removed element from staying reachable through the still-live backing
array — without it you get a subtle heap leak where a "deleted" object is never
collected because the array's tail slot still points at it. If you shift manually
with `copy(s[i:], s[i+1:]); s = s[:len(s)-1]`, you must nil the freed tail slot
yourself (`s[len(s):len(s)+1][0] = nil`, or use `clear` on the tail); `slices.Delete`
is preferable precisely because it does not forget.

### Truncate-and-reuse: `buf = buf[:0]`

`buf = buf[:0]` sets the length to zero while keeping the capacity and the same
backing array. It is the idiomatic way to reuse a buffer across iterations with
no reallocation. The trap is that any sub-slice you handed out before truncating
still points at that same array, and your subsequent appends will overwrite it.
So a flush that hands a batch to a sink and then does `buf = buf[:0]` must hand
out a *copy* (`slices.Clone`), not `buf` itself — otherwise the next batch's
appends overwrite the not-yet-consumed data, especially with an async sink.

### `maps.Clone` is shallow one level down

`maps.Clone(m)` copies the map's keys and values by assignment. For
`map[K][]V` or `map[K]*T` the values are slices or pointers, and assignment
copies the header/pointer, not the pointee — so the clone's value slices share
backing arrays with the source. A caller that appends to a value slice in the
"clone" mutates the original. A deep snapshot must clone each value too, e.g.
`slices.Clone` per entry. This is exactly what `http.Header.Clone` does, and why
it exists rather than callers just using `maps.Clone`.

### `make([]T, n)` + `io.ReadFull` for known-length reads

When the length is known up front — a fixed frame, a length-prefixed body — the
precise tool is `make([]byte, n)` followed by `io.ReadFull`. There is no
over-allocation, no aliasing with any other buffer, and the size is deterministic.
`io.ReadFull` reads exactly `len(buf)` bytes, returns `io.EOF` only if it read
nothing, and `io.ErrUnexpectedEOF` if the stream ended mid-frame — the short-read
semantics a single `r.Read` does not give you. Prefer this over `append` growth
whenever you already know how many bytes you need.

### A small live slice can pin a huge backing array

Slicing a large buffer and keeping only a small piece keeps the entire backing
array alive as long as the piece is reachable, because the slice header still
points into it. If you read a 10 MB response and keep a 20-byte token sliced from
it, the 10 MB cannot be collected. The fix is to clone the piece into a
right-sized slice (`bytes.Clone(tok)`) so the big array becomes unreachable. This
is the retention counterpart to the corruption problems: same shared-array fact,
different failure mode.

## Common Mistakes

### Storing a caller's slice directly instead of copying

Wrong: `q.data[i] = window`. The internal slot now *is* the caller's slice; the
caller mutating `window` mutates your storage, and vice versa. Fix: copy on the
way in — `q.data[i] = slices.Clone(window)` (or `append([]int(nil), window...)`).

### Returning internal state from a repository or cache

Wrong: `return r.records`. The caller sorts, appends, or truncates the returned
slice and silently corrupts your canonical state, or another reader observes a
half-mutated array. Fix: `return slices.Clone(r.records)`. Decide ownership at the
boundary; the store keeps its array, the caller gets its own.

### Two-index hand-off to a consumer that appends

Wrong: handing `buf[lo:hi]` to a decoder that appends — its `append` writes into
the shared array past `hi`, corrupting the next field still living there. Fix:
`buf[lo:hi:hi]` so `cap == len` and the consumer's `append` reallocates.

### Retaining a `Scanner.Bytes()` token without copying

Wrong: `matches = append(matches, tok)` where `tok = scanner.Bytes()`. Every
element aliases the scanner's buffer, so after the loop they all equal the last
line. Fix: `matches = append(matches, bytes.Clone(tok))` (or `string(tok)`).

### Assuming `append` always allocates

Wrong: reasoning that "`append` returns a new slice, so it is safe." If the
source slice still has spare capacity, `append` writes in place into the shared
backing array and corrupts every other slice viewing that array. Fix: clone
first, or bound capacity with `s[lo:hi:hi]` so the `append` is forced to
reallocate.

### Treating `slices.Clone` / `maps.Clone` as deep

Wrong: `maps.Clone(header)` for a `map[string][]string` and then handing it out
as an isolated snapshot — the value slices are still shared. Fix: clone each value
too (`slices.Clone` per entry), the way `http.Header.Clone` does. Same for
`slices.Clone` of a `[]*T` or `[][]T`: it is shallow one level down.

### Handing `buf[:0]`-reused memory to an async sink

Wrong: `sink.Send(buf); buf = buf[:0]` then keep appending. The sink still holds a
handle to the same array your new appends now overwrite. Fix: `sink.Send(slices.Clone(buf))`
before truncating, so the flushed batch is independent of the reused buffer.

### Sizing a `copy` destination by capacity

Wrong: `dst := make([]int, 0, n); copy(dst, src)` copies zero elements because
`len(dst) == 0`. Fix: size by length — `dst := make([]int, len(src)); copy(dst, src)`
— or just use `slices.Clone(src)`.

### Manual copy-shift delete that forgets to nil the tail

Wrong: `copy(s[i:], s[i+1:]); s = s[:len(s)-1]` on a `[]*T` — the old last slot
still points at the removed object, keeping it alive (heap leak). Fix: nil the
freed tail slot, or just use `slices.Delete`/`slices.DeleteFunc`, which zero it
for you.

### Using a single `Read` for a fixed-length frame

Wrong: `r.Read(buf)` once and treating a partial read as a complete frame. `Read`
may return fewer bytes than requested. Fix: `io.ReadFull(r, buf)`, which loops
until `buf` is full or the stream ends, and reports `io.ErrUnexpectedEOF` on a
truncated frame.

Next: [01-window-queue-copy-on-push.md](01-window-queue-copy-on-push.md)
