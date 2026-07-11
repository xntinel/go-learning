# Struct Memory Layout, Alignment, and Padding — Concepts

Struct layout looks like academic trivia until it is the root cause of a
production incident. The same layout facts explain, in turn: a metrics struct
that panics the moment your image runs on a 32-bit ARM edge device; a hot
in-memory index that either fits or does not fit in L2 cache across ten million
rows; a sharded counter that silently loses a third of its throughput to false
sharing without a single mutex in sight; and a binary-protocol header you cannot
blindly `memcpy` onto a socket because Go's in-memory padding is not the wire
format. A senior backend engineer is expected to reason about offsets,
alignment, cache lines, and the allocation consequences of field order, and to
defend those choices in code review. This file is the model; each exercise that
follows is a concrete production artifact built on it — a layout auditor, a CI
size gate, a cache-line-padded counter, a 32-bit-safe atomic metrics struct, a
compact hot-path record, a wire header, and columnar batch storage.

## Concepts

### The alignment invariant and why padding exists

Every Go type `T` has an alignment `A = unsafe.Alignof(T)`, and the language
guarantees that a value of type `T` is only ever stored at a memory address that
is a multiple of `A`. The hardware reason is that a CPU loads an aligned 8-byte
value in one memory access; a misaligned one may take two accesses or, on some
architectures, fault outright. Primitive alignments on a 64-bit platform are:
`bool` and `byte` align 1, `int16` align 2, `int32`/`float32` align 4,
`int64`/`float64`/`uint64` and every pointer/`string`/slice header align 8.

To keep each field aligned, the compiler inserts *interior padding* bytes
between fields, and *trailing padding* at the very end so that the struct's own
size is a multiple of the struct's alignment (which is the maximum alignment of
any field). This is why the size of a struct is not the sum of its field sizes.
The canonical example:

```
struct{ a bool; b int64 }   // a@0, 7 bytes padding, b@8  -> size 16, not 9
```

`a` occupies offset 0. `b` must land on a multiple of 8, so it goes at offset 8,
leaving bytes 1..7 as dead padding. The struct's alignment is 8, its size is
already a multiple of 8, so the total is 16. Nine bytes of data, sixteen bytes
of footprint.

### Field order is memory order — Go does not reorder

This is the single most consequential fact of the lesson, and the original
version of this material stated it wrong: the Go compiler does **not** reorder
struct fields. Declaration order is memory order, full stop. (Rust, by contrast,
is free to reorder fields; Go deliberately is not, so that layout is predictable
for `unsafe`, cgo, and reflection.) The practical consequence is that minimizing
padding is *your* job: you order fields from largest alignment to smallest.
Interleaving a `bool` between two `int64`s forces the compiler to pad around the
`bool`; grouping the `int64`s and trailing the `bool`s packs them into the same
word. The same six fields declared in a bad order versus a good order differ by
a documented and reproducible number of bytes per value, and at a million-plus
copies that difference is real RAM and real cache-miss traffic.

### Sizeof, Alignof, Offsetof are compile-time constants

`unsafe.Sizeof`, `unsafe.Alignof`, and `unsafe.Offsetof` are not function calls
that run at runtime and inspect a value. They are evaluated by the compiler and
have type `uintptr`; each is a constant expression. They do not read the value
you pass — `unsafe.Sizeof(x)` only looks at the *type* of `x` — so they are safe
even for the pointee of a nil pointer. Two things follow. First, you can use
them in constant contexts, including as an array length (`[64 -
unsafe.Sizeof(atomic.Int64{})]byte` is a valid padding field). Second, and this
is a real bug that lived in the original lesson: because `Offsetof` returns an
*unsigned* `uintptr`, testing `unsafe.Offsetof(s.f) >= 0` is a tautology (always
true) and `< 0` is always false — the assertion checks nothing. The real
invariants worth asserting are `offset % Alignof(field) == 0` (every field is
aligned) and that offsets increase monotonically in declaration order (because
Go does not reorder).

### reflect is the runtime analog

`unsafe.*` works only on statically-named types and fields. When you need to
inspect an *arbitrary* type at runtime — to build a linter, a serializer, or a
layout auditor that works on any struct — `reflect` is the tool.
`reflect.TypeFor[T]()` gives the `reflect.Type`; `Type.Size()`, `Type.Align()`,
and `StructField.Offset` mirror the `unsafe` trio, and `Type.FieldAlign()`
reports a type's alignment when it is used as a struct field. This is exactly how
`golang.org/x/tools/.../fieldalignment` — the real analyzer senior engineers run
in CI — computes wasted padding. The cost is runtime reflection, so you do it in
a diagnostic or a test, not on a hot path.

### Cache lines and false sharing

A CPU does not move memory one byte at a time; it moves *cache lines*, typically
64 bytes, and cache coherency is maintained at cache-line granularity. When two
different cores write to two different variables that happen to sit in the same
cache line, each write invalidates the other core's copy of the whole line, so
the two writes ping-pong the line back and forth and serialize even though the
variables are logically independent. This is *false sharing*, and it is a
throughput bug that no unit test detects and no mutex is involved in. The classic
trigger is an array of per-goroutine counters packed adjacent: shard 0 and shard
1 share a line, cores fight over it. The fix is to pad each hot field so it
occupies its own 64-byte line. You trade memory for the elimination of coherency
traffic — worth it precisely on the hottest counters.

### 64-bit atomics require 8-byte alignment

Atomic operations on a 64-bit value require the value to be 8-byte aligned. On
64-bit platforms every `int64` field is already 8-aligned, so this is invisible.
On 32-bit platforms (`386`, 32-bit `arm`, 32-bit `mips`) the natural word is 4
bytes, and the Go runtime cannot guarantee that an arbitrary `int64` *field*
lands on an 8-byte boundary. The legacy functions `atomic.AddInt64` /
`atomic.LoadInt64` operating on a bare misaligned `int64` field therefore
*panic at runtime* on those platforms — a crash that never appears on your amd64
laptop or CI, only on the edge/IoT target in the field. The modern fix is the
`atomic.Int64` / `atomic.Uint64` wrapper types (Go 1.19+), which the runtime
guarantees are 8-byte aligned on every platform; prefer them. The older manual
fix is to place the 64-bit field first in the struct (offset 0 is always
aligned) and never let it move.

### Size drives heap and cache footprint at scale

A 16-byte reduction per record is nothing for a singleton config struct and
everything for a struct you store by the millions. Ten million entries at 16
bytes saved is 160 MB of RAM you did not spend, and — often more important — far
fewer cache lines touched when a background scan walks the whole structure, which
is a direct latency win. This is why a *size budget* belongs in CI as a
regression gate: `unsafe.Sizeof` is a compile-time constant, so a test that pins
`Sizeof(CacheEntry{}) <= 48` fails the build the day someone adds a field that
blows the cache footprint, before it ships.

### Zero-size types and the trailing-field trap

`struct{}` and `[0]byte` occupy zero bytes. This powers the idiomatic
allocation-free set, `map[string]struct{}`, where the value carries no
information and costs no per-entry value storage — perfect for dedup sets and
idempotency-key sets. But there is a trap: Go guarantees that taking the address
of a field yields a pointer *within* the allocation, and a pointer must not point
past the end of an object. So if the *last* field of a struct has size zero, the
compiler pads the struct by one alignment unit so that `&lastField` does not
point past the allocation. Moving that same zero-size field earlier removes the
padding. A struct ending in a `[0]byte` sentinel is therefore strictly larger
than the same struct with the sentinel moved up — a surprising size increase
from a "free" field.

### In-memory layout is not the wire format

`encoding/binary` walks a struct field by field and reads/writes each field's
bytes; it does **not** `memcpy` the struct, and it deliberately ignores Go's
padding. Consequently `binary.Size(header)` (the wire size, no padding) differs
from `unsafe.Sizeof(header)` (the in-memory size, with padding). You must never
`memcpy` or `unsafe`-cast a Go struct onto a socket: the padding bytes leak into
the stream and, worse, the padding differs across `GOARCH`, so a message written
on amd64 would not parse on a 32-bit peer. Serialize field by field with
`encoding/binary` and pin the exact bytes with a golden test. The one case where
you genuinely want host C ABI layout — mapping a C struct over cgo/FFI or
`mmap`-ing a kernel structure — is served by a leading `_ structs.HostLayout`
marker field (Go 1.23+), which tells the compiler to match the host C layout and
opts the type out of any future Go-specific layout freedom.

### Array-of-structs vs struct-of-arrays

The natural representation of a batch of `N` records is an array of structs
(AoS): `[]Event`. But when a hot loop touches only one field per element — say,
summing `Amount` over a million events — AoS drags the whole struct, padding and
all, through cache to reach one field, wasting most of every cache line. The
columnar alternative, struct-of-arrays (SoA) — `Ids []uint64; Amounts []int64;
Flags []uint8` — stores each field contiguously, so the hot scan reads a dense
run of exactly the field it needs, improving locality and eliminating
per-element padding (the `Flags` column packs one byte per element instead of
padding each to the struct's alignment). SoA is the standard shape for
analytics-style scans and for code you want the compiler to vectorize. AoS stays
better when you touch most fields of one element at a time (per-request work).

## Common Mistakes

### Assuming struct size is the sum of field sizes

Wrong: computing `sizeof(struct{a bool; b int64})` as `1 + 8 = 9`. The real size
is 16 (interior + trailing padding). Fix: always query `unsafe.Sizeof`; never add
field sizes by hand.

### Asserting Offsetof(x.f) >= 0 or < 0

Wrong: `if unsafe.Offsetof(s.f) < 0 { fail }` as a layout test. `Offsetof`
returns unsigned `uintptr`, so `>= 0` is always true and `< 0` is always false;
the assertion verifies nothing. This exact bug shipped in the original lesson.
Fix: assert `offset % unsafe.Alignof(field) == 0` and that offsets increase in
declaration order.

### Expecting the compiler to reorder fields

Wrong: leaving fields in a convenient logical order and assuming Go will pack
them. It will not; declaration order is authoritative. Fix: order fields
largest-alignment-to-smallest yourself, or run `fieldalignment` and take its
suggestion.

### Legacy atomic on a bare int64 field, shipped to 32-bit

Wrong: `atomic.AddInt64(&m.counter, 1)` on a `counter int64` field that is not
first, then deploying to a 32-bit target where the field is misaligned and the
call panics. Fix: use `atomic.Int64` (auto-aligned) or place the 64-bit field
first.

### Ignoring false sharing

Wrong: packing hot per-goroutine counters adjacent in an array so they share a
cache line, then blaming the mutex or the scheduler for lost throughput. Fix: pad
each hot counter to its own cache line.

### memcpy-ing a Go struct onto the wire

Wrong: casting a struct to `[]byte` and writing it to a socket, assuming its
layout matches the protocol. Go padding leaks into the bytes and differs across
`GOARCH`. Fix: use `encoding/binary` field by field and pin the encoding with a
golden test.

### Reaching for unsafe.Pointer + Offsetof to read a field

Wrong: `*(*int)(unsafe.Add(unsafe.Pointer(&s), unsafe.Offsetof(s.f)))` to read
`s.f`. It is fragile to layout changes and pointless. Fix: name the field, `s.f`.

### Optimizing layout before measuring

Wrong: hand-packing every struct in the codebase for "efficiency". Reordering a
singleton config struct saves nothing and hurts readability. Fix: reorder and
pack only the structs that are actually hot — large slices, high-frequency
allocations, per-request objects — and let the rest read naturally.

Next: [01-field-ordering-record-types.md](01-field-ordering-record-types.md)
