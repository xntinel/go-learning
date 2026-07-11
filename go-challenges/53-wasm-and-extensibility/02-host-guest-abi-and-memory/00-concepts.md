# Host/Guest ABI and Linear Memory — Concepts

A WebAssembly module is a pure computation sandbox. Its only shared surface with
the host is a single contiguous byte array — its linear memory — and a set of
exported functions whose parameters and results are limited to four numeric
types: `i32`, `i64`, `f32`, `f64`. There is no way to "pass a string" or "pass a
struct" across that boundary. Every rich value has to be serialized into the byte
array and referenced by integers. Designing exactly how that happens — which
integers mean what, who allocates the space, and who frees it — is your job. It is
the same class of problem as designing a wire protocol or a cgo/FFI boundary: you
own the calling convention, the memory-ownership rules, and the bounds checking,
and a mistake is a memory-safety or correctness bug (out-of-bounds read, use of a
freed pointer, aliasing corruption) rather than a compile error. This file is the
model you need before writing any host that reaches into guest memory. The three
exercises that follow build the pieces in isolation: the pointer/length codec, raw
linear-memory I/O, and a full round-trip that lets the guest own its own
allocations.

## Concepts

### Why an ABI is needed at all

An Application Binary Interface is a contract about how bytes are laid out and how
functions are called at the machine level. On a normal platform the compiler and
the operating system agree on one for you. Across the Wasm host/guest boundary
there is no such prior agreement, and the medium is deliberately minimal: a Wasm
function signature admits only the four numeric types, and the only mutable state
both sides can see is one linear-memory byte array per module. A Go `string` or
`[]byte` or `struct` simply has no representation the boundary understands. So you
invent one: you decide that "a string" is encoded as its UTF-8 bytes written into
linear memory at some offset, and that a function which needs a string takes two
`i32` parameters — the offset (a "pointer" into the byte array) and the byte
length. This hand-designed calling convention is the ABI. It is not a language
feature; it is a discipline both sides must follow identically, and nothing checks
that they agree except your tests.

### The (pointer, length) pair and its encoding

A Go slice is a triple: a pointer, a length, and a capacity. Across the boundary
capacity is irrelevant (the guest, not the host, manages the backing store), so a
region of guest memory is fully described by two 32-bit integers: the offset where
it starts and how many bytes it spans. When a function needs to *accept* such a
region you pass those as two separate `i32` parameters. When a function needs to
*return* one, you hit a constraint: a Wasm function returns values on a small
stack, and the classic, maximally portable convention (used by the TinyGo
toolchain and wazero's own examples) is to return a single `i64` with the pointer
in the high 32 bits and the length in the low 32 bits:

```
packed = uint64(ptr)<<32 | uint64(length)
ptr    = uint32(packed >> 32)
length = uint32(packed)
```

Getting the halves straight is the whole game: unpacking with the shift on the
wrong field silently swaps pointer and length and every subsequent read is
garbage. wazero exposes `api.EncodeU32(v)` / `api.DecodeU32(v)`, which zero-extend
a `uint32` into a `uint64` and truncate back, so the packed form can also be
written `api.EncodeU32(ptr)<<32 | api.EncodeU32(length)`; the two spellings are
identical bit-for-bit. Those helpers exist because a Wasm `i32` argument is
carried in a `uint64` slot on wazero's call stack, so any single `i32` you pass to
`Function.Call` must be widened with `api.EncodeU32` first.

### Memory ownership: the guest owns its heap

The single most important rule, and the one that trips up everyone coming from a
"just write to a buffer" mindset: the host must not pick an offset and write
there. The guest module has its own allocator managing its own linear memory —
stack, globals, heap, and whatever the guest language runtime keeps. If you write
your input at an address you invented, you are very likely stomping on the guest's
live data, and the corruption surfaces later as a mysterious wrong answer, not as
an error at the write. The correct protocol is: the guest *exports an allocator*
(conventionally `alloc`, `malloc`, or similar) and a *deallocator* (`free`,
`dealloc`). To hand data to the guest you call `alloc(size)`, which returns an
offset the guest promises is free; you write your bytes there; you pass that
offset and length to the real function; and when you no longer need the region you
call `dealloc(ptr, size)`. The guest chose the address, so the guest's bookkeeping
stays consistent.

### Two directions of transfer are not symmetric

Data flows both ways and each direction has its own ownership bookkeeping.
Host→guest: the host calls the guest allocator, writes the input, and passes
`(ptr, len)`; after the call the host frees that input region. Guest→host: the
called function itself allocates a result region inside guest memory, fills it, and
returns the packed `(ptr, len)`; the host reads those bytes out and then must free
*that* region too. The subtle leak is forgetting the second one — the host
faithfully frees its input but never frees the value the guest returned, so the
guest heap grows on every call until the module is torn down. In a plugin host
that services many requests against one long-lived module instance, that is a slow
memory leak with the guest as the culprit and the host as the cause.

### `Memory.Read` returns a write-through view, not a copy

`api.Memory.Read(offset, byteCount)` returns a `[]byte` that *aliases the live
linear-memory buffer*. It is not a copy. Two consequences follow, both of them
sharp. First, it is write-through: mutating the returned slice mutates guest
memory in place. If you use that slice as a scratch buffer and change a byte, you
have silently edited the guest's state. Second, and worse, the view is invalidated
by any capacity change. When the guest (or the host) grows linear memory with
`memory.grow`, wazero may reallocate the backing array, and your previously
returned slice now points at the *old*, abandoned buffer: reads through it see
stale data and writes through it are lost. This is a dangling-pointer bug dressed
in Go clothing — the slice is still valid Go memory, so nothing panics; it just
stops reflecting reality. The rule that keeps you safe is absolute: any bytes you
intend to keep across another guest call (anything that might grow memory) must be
copied out immediately with `append([]byte(nil), view...)` or `bytes.Clone`.

### Bounds checking is the host's job

`Read` and `Write` return an `ok bool` that is `false` when the requested region
does not fit in the current memory size. That boolean is not advisory. If you
discard it, an out-of-bounds `Read` gives you a short or empty slice and an
out-of-bounds `Write` silently does nothing, and either way a plugin can feed your
host a `(ptr, len)` that points outside its memory to probe or corrupt. Converting
`ok == false` into a real error is the security boundary. Separately, you must
guard the arithmetic itself: `ptr + length` can overflow a `uint32` (a guest
returning `ptr = math.MaxUint32, len = 2`), so compute the end offset in `uint64`
— `end := uint64(ptr) + uint64(length)` — and compare against the memory size
before trusting it. A `uint32` addition here wraps to a tiny number and passes a
naive check while pointing far out of range.

### Units and the page model

Linear memory is measured in two different units and confusing them is a common
bug. It grows in *pages* of 65536 bytes (64 KiB). `Memory.Size()` returns the
current size in *bytes*. `Memory.Grow(deltaPages)` takes a number of *pages*, not
bytes, and returns the *previous* page count and an `ok` bool (`false` if growth
would exceed the module's declared maximum). To grow by N bytes you request
`ceil(N / 65536)` pages; to turn a page count into bytes you multiply by 65536. A
module's memory maxes out at 65536 pages (4 GiB), at which point `Size()`, being a
`uint32` byte count, would need 2^32 and overflows to 0 — an edge worth knowing
about even though you will rarely reach it.

### The trade-off of stable views

You can make a `Read` view stable by declaring the memory with its minimum equal
to its maximum (or configuring a fixed capacity), so wazero preallocates the full
backing array and `grow` never reallocates. That buys you a view that survives
across calls — at the cost of reserving the entire address range up front and
giving up the ability to grow on demand. For a general-purpose host library that
is the wrong default: it wastes address space and couples your host to a memory
shape the guest may not want. The safer discipline is the opposite one — never
hold a `Read` view across a `Call` or a `Grow`, always defensive-copy the bytes
you need to keep — which works regardless of how the guest declared its memory.

### The failure modes to name

Because there is no compiler enforcing any of this, it helps to keep an explicit
catalogue of what can go wrong, so a bug's symptom maps to a cause: use-after-free
(reading a region after `dealloc`), double-free (calling `dealloc` twice on the
same region), leaking the guest-returned allocation (freeing the input but not the
output), aliasing corruption (holding a `Read` view across a `grow`), integer
overflow in `ptr + length`, and byte-order assumptions when reading structured
fields — Wasm linear memory is little-endian, and wazero gives you
`Memory.ReadUint32Le` / `WriteUint32Le` so you never have to hand-assemble bytes,
but you must be deliberate about endianness the moment you read anything wider than
a byte.

## Common Mistakes

### Writing to an offset you picked yourself

Wrong: choosing a "free-looking" address such as `mem.Write(1024, data)` and
passing `1024` to the guest. You are almost certainly overwriting the guest
allocator's own state or another live object, and the corruption shows up later as
a wrong result.

Fix: ask the guest for the address. `res, _ := alloc.Call(ctx, uint64(len(data)));
ptr := api.DecodeU32(res[0])`, then `mem.Write(ptr, data)`. The guest chose it, so
its bookkeeping stays valid.

### Ignoring the `ok` bool from `Read`/`Write`

Wrong: `mem.Write(ptr, data)` with the return discarded. An out-of-range write
silently does nothing and you never learn the region was invalid.

Fix: `if !mem.Write(ptr, data) { return fmt.Errorf("write %d bytes at %d: %w",
len(data), ptr, ErrOutOfBounds) }`. Turn `false` into a wrapped sentinel error.

### Holding a `Read` view across another call or a grow

Wrong: `buf, _ := mem.Read(p, n); other.Call(ctx, ...); use(buf)`. If the call
grew memory, `buf` now aliases the old, reallocated buffer and is stale.

Fix: copy it out before doing anything else — `out := append([]byte(nil), buf...)`
(or `bytes.Clone(buf)`) — then use the copy.

### Unpacking the packed `i64` with the halves swapped

Wrong: `ptr := uint32(r); n := uint32(r >> 32)`. That reads the length as the
pointer and vice versa.

Fix, matching the `ptr<<32 | len` convention: `ptr := uint32(r >> 32); n :=
uint32(r)`.

### Forgetting to free the guest-returned allocation

Wrong: freeing only the input region and returning, leaking the region the guest
allocated for its result on every call.

Fix: as soon as you have decoded the returned `(outPtr, outLen)`, schedule its
release — `defer dealloc.Call(ctx, api.EncodeU32(outPtr), api.EncodeU32(outLen))`
— on the same path that frees the input.

### Confusing bytes with pages

Wrong: treating `Size()` as a page count, or passing a byte count to `Grow`.

Fix: `Size()` is bytes; `Grow` takes and returns pages of 65536 bytes. Convert
explicitly (`pages := (n + 65535) / 65536`, `bytes := pages * 65536`).

### Mutating a `Read` view as scratch space

Wrong: taking a `Read` view and editing it in place because it is a convenient
buffer — because the view is write-through, you have just corrupted guest memory.

Fix: copy first if you need a mutable buffer that is not meant to change the guest.

### Passing a Go pointer as an `i32`

Wrong: assuming a host `[]byte` address is meaningful to the guest, or handing a
Go pointer across as an integer. Only integer offsets into the guest's own linear
memory are valid; a host address means nothing inside the sandbox.

Fix: allocate in the guest, write the bytes, and pass the guest offset.

### Double-free and use-after-free

Wrong: calling `dealloc` twice on one region, or reading a region after freeing
it. With a real allocator the first corrupts the free list and the second reads
recycled bytes.

Fix: free exactly once on every path, and never read a region after it is freed.

Next: [01-ptr-len-abi-codec.md](01-ptr-len-abi-codec.md)
