# Slice and Map Memory Leaks: Backing-Array Pinning — Concepts

Memory leaks in Go are almost never "forgot to free." They are *liveness leaks*:
a small, long-lived reference keeps a large object reachable, so the garbage
collector — which frees only what is unreachable — is doing exactly its job when
it declines to reclaim the large object. In a backend service this is the bug
that does not show up in a unit test and does not show up on day one. It shows up
as resident set size (RSS) climbing a few kilobytes per request until, after days
of uptime, the pod trips its memory limit and the orchestrator OOM-kills it. Or
it shows up in a heap profile as a 4 KiB session struct that, multiplied across a
million live connections, pins gigabytes of request buffers no one is reading
anymore. The senior skill is recognizing the four shapes this takes in code
review and in `pprof`, and reaching for the exact idiom that severs each pin.

This file is the conceptual foundation for the modules that follow. Read it once
and you will have the model needed to build and test each of the nine
self-contained exercises: a non-pinning bounded buffer, a `runtime.ReadMemStats`
leak harness, a pub/sub registry that zeroes its tail on removal, a token parser
that clones out of a request buffer, a log aggregator that copies scanner lines,
a `sync.Pool` with a capacity guard, a rate-limiter map that rebuilds to reclaim,
a framing window bounded by the three-index expression, and a `weak.Pointer`
canonicalization cache.

## Concepts

### Reachability, not scope, decides liveness

The single idea under everything here: an object lives exactly as long as it is
*reachable* from a root (a goroutine stack, a global, a register). Going out of
lexical scope does nothing; being small does nothing. A one-byte sub-slice of a
four-mebibyte array keeps all four mebibytes alive, because the slice header
carries a pointer to the array's *base*, not to the one byte you can see. The GC
traces that pointer, marks the whole array live, and moves on. No amount of "but
I only use one byte" changes the arithmetic — the unit of reclamation is the
allocation, and the allocation is the whole array.

### A slice header is {ptr, len, cap}

A slice value is three words: a pointer to an element of a backing array, a
length, and a capacity. Sub-slicing (`s[low:high]`) produces a *new header* over
the *same array*: it adjusts `ptr` and `len` (and, with the three-index form,
`cap`), but it never changes which array is underneath. Two slices cut from one
array share and pin the same memory. This is why passing a sub-slice across an
ownership boundary is a decision about lifetime, not just about data: you are
handing the receiver a pin on the producer's array for as long as the receiver
keeps the header.

### Copy, Clone, and append([]T(nil), …) sever the pin

Three idioms allocate a *fresh* backing array and so break the pin: `copy` into a
`make`'d slice, `slices.Clone(s)`, and `append([]T(nil), src...)`. All three are
the same move — "defensive copy at an ownership boundary" — and all three cost one
allocation plus a memcpy of only the visible window, not the whole source array.
When an exported function returns a view into internal storage, or a long-lived
struct retains a slice a caller handed in, this copy is the fix. The window's
size is what you copy; the array's size is what you would otherwise pin, and they
are usually wildly different.

### Clip drops spare capacity so the next append reallocates

`slices.Clip(s)` returns `s[:len(s):len(s)]` — same array, but capacity trimmed to
length. It does not copy and it does not, by itself, sever an existing pin on the
current `len(s)` bytes. What it does is force the *next* `append` to allocate a
new array instead of writing into spare capacity. Use it when you hand a
caller-owned slice to long-lived storage and want to guarantee that later appends
(by you or the caller) do not scribble into each other's tails, and that any
elements hiding in the spare capacity beyond `len` are not retained. Clip is
about the tail; Clone is about the whole array. Reach for Clone when you must not
pin the source at all; reach for Clip when you own the array and only need to
bound future growth.

### The three-index expression bounds write-through, not the read pin

`a[low:high:max]` sets the result's capacity to `max - low`. The common, safe form
is `a[low:high:high]`, giving `cap == len`. This is what you hand a framing
consumer so that when it `append`s to its frame, the append sees no spare
capacity and reallocates, instead of silently overwriting the *next* frame still
living in the producer's array. Understand its exact scope: it bounds
*write-through* (the consumer cannot grow into your data), but it does **not**
break the read pin — the consumer's slice still points into your array and keeps
it alive until the consumer copies. Three-index prevents corruption; only a copy
prevents the pin.

### Delete and DeleteFunc zero the vacated tail

The naive way to remove element `i` from a slice of pointers is swap-remove
(`s[i] = s[len(s)-1]; s = s[:len(s)-1]`) or truncate (`s = s[:len(s)-1]`). Both
shrink `len`, but neither clears the now-unused tail slot — which still holds a
live pointer. That dead-but-reachable pointer pins whatever it points to (a
connection, a large struct) for as long as the slice's backing array lives.
`slices.Delete` and `slices.DeleteFunc` zero every element between the new length
and the old length precisely to prevent this; for pointer or pointer-bearing
element types it is not an optimization, it is the correctness fix. If you must
swap-remove by hand, nil the vacated slot before shrinking.

### clear resets a reused pointer-bearing buffer

The builtin `clear` zeroes every element of a slice (length unchanged) or deletes
every key of a map. For a reused buffer of pointers or structs-with-pointers,
`clear(buf)` before returning it to a pool is the correct reset: it drops the
stale references so they cannot survive into the next use and pin what they point
to. Resetting only the length (`buf = buf[:0]`) leaves those pointers live in the
spare capacity.

### Maps grow their buckets but never shrink them

A Go map grows its bucket array as it fills, but `delete` never returns bucket
memory to the allocator, and neither does `clear`. A map that has ever held a
million keys keeps a million keys' worth of bucket spine forever, even if it now
holds ten. For a high-churn, high-cardinality map — a per-IP rate limiter, an
idempotency store, a session table with a rotating key space — this means the
map's footprint is a permanent high-water mark. The only way to give the memory
back is to rebuild: copy the live entries into a fresh, size-hinted map and swap
it in, letting the old spine become garbage. `delete` unlinks entries (freeing
keys and values); it does not shrink the spine.

### sync.Pool buffers accumulate capacity

A `sync.Pool` of reusable byte buffers has a subtler leak. A buffer grows to fit
the largest payload it ever served, and it keeps that capacity when returned. One
64 MiB response, handled once, permanently inflates the buffer it borrowed — and
under load the pool fills with such giants. The fix on `Put` is two-part: reset
the *length* (`b = b[:0]` or `buf.Reset()`) so stale data is not reused, and
*drop* the buffer entirely if its capacity exceeds a guard threshold, so an
outlier does not become the pool's permanent floor. Never try to shrink capacity
in place — you cannot; you can only choose not to keep the oversized array.

### Scanner.Bytes and friends alias an internal buffer

`bufio.Scanner.Bytes()` returns a slice into the scanner's own buffer, which is
overwritten on the next `Scan`. Appending that slice to a retained collection
does two bad things at once: it aliases memory that is about to change (so the
retained data corrupts), and it pins the scanner's buffer. The contract, shared
by many stdlib read paths, is "valid only until the next read." Retaining it
requires a copy (`slices.Clone`, `append([]byte(nil), b...)`) or using the
string-returning variant (`Scanner.Text()`), which allocates a fresh string.

### weak.Pointer references without pinning

`weak.Pointer[T]` (Go 1.24) references memory without keeping it alive. `weak.Make(p)`
records the reference; `Value()` returns the live `*T` or `nil` once the object
has been collected. This is the basis for a cache or canonicalization map that
must not *extend* the lifetime of what it caches: store `weak.Pointer[T]`, treat a
`nil` `Value()` as a miss, and the entry never prevents collection of a value no
live caller still holds. Weak pointers also compare by object identity and keep
comparing equal after collection, which is what makes them usable as stable keys.

### runtime.AddCleanup evicts the dead key

`runtime.AddCleanup(ptr, fn, arg)` (Go 1.24) registers `fn(arg)` to run, in its own
goroutine, some time after `ptr` becomes unreachable. It is the modern,
composable replacement for `runtime.SetFinalizer`: it cannot resurrect the object
(it receives `arg`, not the object), allows many cleanups per object, and runs
even on reference cycles. In a weak cache it does the memory hygiene the weak
pointer alone cannot: when a value is reclaimed, the cleanup deletes the now-dead
map key so the map does not fill with empty slots. The one iron rule is that
neither `fn` nor `arg` may reference `ptr` — if they do, the object stays alive
and the cleanup never runs (and `arg == ptr` panics outright).

### Detecting these leaks

Three tools, from most to least precise. Heap `pprof` with `inuse_space` shows the
retained-by chain — the flame graph names the small object at the top of the pin.
`runtime.ReadMemStats` gives `HeapAlloc` (live heap object bytes) and `HeapInuse`
(bytes in in-use spans, which is where bucket spines show), and a delta around a
forced `runtime.GC()` is a quick, in-test assertion that a large allocation was or
was not reclaimed. `go test -benchmem` and `-memprofile` quantify per-operation
retention. Two disciplines make MemStats assertions honest: `HeapAlloc` changes
smoothly because sweeping is incremental, so call `runtime.GC()` *twice* before
reading (the second GC completes the sweep of the first), and compare *magnitudes*
with generous margins, never exact bytes. And use `runtime.KeepAlive` to hold a
source object reachable across a baseline read so the compiler does not collect it
before you meant to measure it — `KeepAlive` extends liveness for measurement; it
never breaks a pin.

## Common Mistakes

### Returning a producer sub-slice from an exported function

Wrong: `return b.data[low:high]`. The caller now pins the producer's entire
backing array for as long as it keeps the returned slice.

Fix: `return slices.Clone(b.data[low:high])` or `append([]byte(nil), b.data[low:high]...)`.

### Assuming a small window needs no copy

Wrong: "the returned slice is only a few bytes, a copy is wasteful." The window
size is irrelevant; the pin is the whole array. A three-byte window pins a
four-mebibyte array exactly as hard as a four-mebibyte window does.

Fix: copy at the boundary regardless of window size.

### Swap-remove or truncate without zeroing the tail

Wrong: `s[i] = s[len(s)-1]; s = s[:len(s)-1]` on `[]*Conn`. The old last slot still
holds a live `*Conn`, pinning that connection.

Fix: `slices.Delete(s, i, i+1)`, which zeroes the vacated tail, or explicitly nil
the slot before shrinking.

### Believing delete or clear shrinks a map

Wrong: expecting `delete(m, k)` or `clear(m)` to hand bucket memory back. They
remove entries; the bucket spine stays at its high-water mark.

Fix: rebuild into a fresh, size-hinted map and swap it in.

### Returning oversized buffers to a sync.Pool unconditionally

Wrong: `pool.Put(buf)` after one giant payload. That giant capacity now floors the
pool permanently.

Fix: drop buffers whose `cap` exceeds a threshold on `Put`; only reset length for
buffers within the threshold.

### Retaining Scanner.Bytes across the next read

Wrong: `lines = append(lines, scanner.Bytes())`. The stored slice aliases the
scanner's buffer, so it both corrupts on the next `Scan` and pins the buffer.

Fix: `lines = append(lines, slices.Clone(scanner.Bytes()))` or store `scanner.Text()`.

### Using runtime.KeepAlive as if it fixes a leak

Wrong: sprinkling `runtime.KeepAlive` to "stop the leak." It only *extends*
liveness; it never severs a pin. The consumer's retained reference is what keeps
memory alive.

Fix: copy at the boundary. `KeepAlive` is for measurement timing, not for
correctness.

### Writing a MemStats leak test that flakes

Wrong: `make([]byte, n)` with no writes, and reading `HeapAlloc` without forcing
GC first, then asserting an exact byte count. The allocation may not be committed,
sweeping is incremental, and the number is noisy.

Fix: write into the array to commit pages, call `runtime.GC()` twice before
reading, and assert relative magnitudes with a wide margin.

### Reaching for SetFinalizer for cache eviction

Wrong: `runtime.SetFinalizer` to evict a dead cache key. Finalizers can resurrect
the object, allow only one per object, and may not run on cycles.

Fix: `runtime.AddCleanup` (Go 1.24), or a `weak.Pointer` cache, both of which avoid
those hazards.

### Confusing Clip with Clone

Wrong: expecting `slices.Clip(s)` to sever a pin. Clip keeps the same array (until
the next append) and only drops spare capacity; it does not copy.

Fix: `slices.Clone(s)` when you need a fresh array immediately; Clip only bounds
future growth.

Next: [01-bounded-buffer-independent-snapshot.md](01-bounded-buffer-independent-snapshot.md)
