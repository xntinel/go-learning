# Arrays: Fixed Size and Value Semantics in Backend Hot Paths — Concepts

Arrays are the boring-but-load-bearing primitive at the crypto, wire-protocol,
and networking boundaries of a Go backend. They rarely appear in application
glue code, but they are everywhere the type must state its size as part of its
contract: `sha256.Sum256` returns `[32]byte`, an AES-GCM nonce is `[12]byte`, a
MAC address is `[6]byte`, a protocol magic is `[4]byte`, and `netip.Addr` is a
comparable value wrapping fixed byte arrays. A senior engineer has to know cold
why a `[32]byte` digest is a safe, cheap, comparable map key while a `[]byte`
field silently shares backing memory, why mutating a hot-path buffer needs
`*[N]T`, and why converting a `[]byte` off the wire into a `[N]byte` panics if
the slice is too short. This file is the conceptual foundation; read it once and
you have everything you need to reason through each of the independent exercises
that follow.

## Concepts

### Size is part of the type

`[16]byte` and `[32]byte` are distinct, incompatible types. You cannot assign one
to the other, pass one where the other is expected, or compare them with `==`;
the compiler rejects it. The length is not metadata carried at runtime — it is
baked into the type at compile time and is the specification. There is no
runtime-sized array in Go: the length in `[N]T` must be a constant expression.
This is precisely why cryptographic and wire APIs return arrays rather than
slices. `sha256.Sum256(data) [32]byte` cannot return the wrong number of bytes,
and a caller cannot accidentally truncate it, because the size is welded to the
type. When the size is a fixed, non-negotiable part of the contract — a block
size, a digest, a nonce, a MAC, a protocol magic — the array makes the compiler
enforce it for free.

### Arrays are values, not references

This is the single most consequential fact about arrays, and the one that trips
up engineers who reach for Go with a slices-are-arrays mental model from other
languages. Assignment copies the whole array. Passing an array to a function
copies it. Returning an array copies it. Comparing two arrays with `==` compares
element by element. An array is the data, not a handle to the data.

The sharp edge is the contrast with slices. A `[]byte` is a three-word header
(pointer, length, capacity) over a backing store. Copying a slice header copies
those three words and leaves both headers pointing at the *same* backing array.
So when a `[N]byte` lives inside a struct, copying the struct deep-copies the
array; when a `[]byte` field lives inside a struct, copying the struct copies
only the header and both structs now alias the same bytes. That difference is the
entire basis for the defensive-copy decision at a secret-handling boundary: a
struct with a `Key [32]byte` field hands out a real independent copy by value,
while a struct with a `Key []byte` field hands out an aliased view that a caller
can mutate out from under you.

### Comparability and map keys

A fixed-size array of comparable elements is itself comparable, so `[32]byte`,
`[6]byte`, `[16]byte`, and `netip.Addr` (which wraps fixed arrays) can be used
directly as map keys and set members. This is not a minor convenience. It means a
content-addressed store keys `map[[32]byte][]byte` on the raw digest with no
`hex.EncodeToString` allocation per write; an L2 forwarding table keys
`map[[6]byte]Port` on the raw MAC with no string interning; an IP allowlist keys
`map[netip.Addr]struct{}` on the address value directly. The array's value
semantics are what make this sound: two independently-computed equal arrays are
`==` and therefore hash to the same bucket, so equal content deduplicates
automatically. This underpins content-addressed storage, deduplication,
interning, and idempotency guards — patterns where "the same bytes must mean the
same key" is the correctness requirement.

### Value copy as immutability

Because the whole array is copied on assignment and return, returning a
struct-with-array-field by value is a cheap, safe, immutable snapshot. Nothing
the caller does to the returned value can reach back and mutate the source, and
no lock is needed to read a snapshot that is a private copy. This is the
deliberate mechanism behind defensive copies of key material and lock-free config
snapshots: you are not copying for paranoia, you are copying because value
semantics make the copy an isolated, immutable view by construction. A `[60]int`
rate-limiter state array copied out as a `Snapshot()` cannot be corrupted by the
observer; the limiter keeps mutating its own array, the observer holds an
independent frozen picture.

### Mutation requires a pointer

The flip side of value semantics: a value receiver or value parameter operates on
a *copy*, so a method `func (b Block) Mix()` that writes `b[0] = 0xff` mutates a
throwaway and the caller sees nothing. To mutate a hot-path buffer in place you
need `*[N]T`. Two bridges make this ergonomic without allocating: `&arr` yields a
`*[N]T`, and `arr[:]` (or `(&arr)[:]`) produces a slice header over the array's
storage so you can hand a fixed array to any slice-consuming API — `io.Reader`,
`crypto/rand.Read`, `hash.Hash.Write` — with zero heap allocation, because the
slice just points at the array's existing stack storage. In-place XOR whitening,
filling a nonce from `rand.Read(n[:])`, and reading a block through `copy(b[:],
src)` all rely on this.

### Slice to array conversions (Go 1.20+)

Since Go 1.20 you can convert a slice to an array value with `[N]T(s)` and to an
array pointer with `(*[N]T)(s)`. The value form copies the first `N` elements; the
pointer form aliases them (no copy — the returned `*[N]T` points into the slice's
backing store). Both **panic** if `len(s) < N`. This is the correct, idiomatic way
to read a fixed frame off a `[]byte` at a wire boundary — but the panic makes the
length check mandatory. You always guard `if len(buf) < headerLen { return err }`
*before* converting, so a truncated read returns an error instead of taking down
the goroutine. Getting this wrong is a classic remote-crash: an attacker sends a
short frame and `(*[4]byte)(buf)` panics.

### Stack allocation and escape analysis

Small fixed-size arrays typically live on the stack. Their size is known at
compile time, so the compiler can allocate them in the stack frame with no heap
traffic, no garbage-collector pressure, and cache-friendly locality. This is why
arrays are the right choice for lookup tables, digests, nonces, and small
transform state in hot paths: a `[256]byte` translation table built once at
package init, a `[8]uint64` mixing state, a `[16]byte` block — all allocation-free
and cache-resident. The caveat is size: a *large* array is copied in full on every
assignment and call, so a big `[N]T` should be passed as `*[N]T` to avoid the copy
while still keeping the compile-time size guarantee. "Arrays are cheap" is true for
small arrays and false for large ones.

### Arrays vs slices: the decision rule

Use an array when the size is a fixed, compile-time invariant that is part of the
contract: block size, digest, nonce, MAC, protocol magic, a lookup table indexed
by a byte. Use a slice for anything that grows, or whose length is data-dependent,
or that you want to share by reference. The test is simple: if the length can ever
differ between two valid values, it is a slice; if every valid value has exactly
the same length and that length is the spec, it is an array.

### Constant-time comparison for secrets

Arrays support `==`, and for most fixed identifiers — a MAC, a magic, an IP — that
is exactly what you want. But `==` (like `bytes.Equal`) short-circuits on the
first differing byte, so its running time leaks how many leading bytes matched.
For secret-dependent comparisons — a MAC tag, an auth token, an HMAC — that timing
is a side channel an attacker can exploit to forge a valid tag byte by byte. Secret
comparisons must use `crypto/subtle.ConstantTimeCompare` (returns 1 for equal, 0
otherwise, and 0 immediately for a length mismatch) or `crypto/hmac.Equal`, both
of which run in time independent of the content. The rule: `==` for identity,
constant-time compare for secrets.

## Common Mistakes

### Using a slice with a comment "always N bytes"

Wrong: `type Nonce []byte // always 12 bytes`. The slice does not enforce the
length; the comment is the only contract, and comments do not compile. Any code
path can hand you an 11-byte or 13-byte "nonce".

Fix: `type Nonce [12]byte`. The compiler is the spec, and every value is exactly
twelve bytes by construction.

### Expecting a value receiver to mutate the caller's array

Wrong: `func (b Block) Whiten(key Block)` that writes into `b` and expects the
caller's block to change. The receiver is a copy; the caller sees nothing.

Fix: `func (b *Block) Whiten(key *Block)`. A pointer receiver mutates the
caller's array in place.

### Treating a by-value struct copy as a defensive copy of a slice field

Wrong: storing key material as a `Key []byte` field and returning the struct by
value as a "snapshot". The slice backing store is shared, so the snapshot still
aliases the original and either side can corrupt the other.

Fix: store the key as a `Key [32]byte` array field. Now the by-value struct copy
deep-copies the key and the snapshot is truly independent.

### Comparing arrays of different sizes

Wrong: `if [16]byte(a) == [32]byte(b)`. The compiler rejects it because the two
are different types. Forcing a conversion to make it compile hides a real bug.

Fix: compare like-typed values. If you genuinely need to relate a 16-byte and a
32-byte quantity, you are almost always modeling something wrong.

### Converting a slice to an array without a length check

Wrong: `magic := [4]byte(buf[:4])` or `(*[4]byte)(buf)` on a buffer that might be
shorter than four bytes. Both panic on a short slice, which turns a truncated
network read into a crash.

Fix: guard first — `if len(buf) < 4 { return Header{}, ErrShortBuffer }` — then
convert. The length check is not optional at a wire boundary.

### Using == or bytes.Equal to compare secret tags

Wrong: `if mac == want` or `bytes.Equal(mac, want)` for an auth tag, HMAC, or
token. Neither is constant-time; both leak timing that enables a byte-by-byte
forgery attack.

Fix: `crypto/subtle.ConstantTimeCompare(mac, want) == 1` or
`crypto/hmac.Equal(mac, want)`.

### Rebuilding a lookup table on every call

Wrong: constructing a `[256]byte` translation table inside the function that uses
it, on every call. That throws away the entire point of a fixed-size table — the
one-time, allocation-free build.

Fix: build the table once as a package-level `var` (or in `init`), and index it
in the hot path.

### Calling netip.Addr.As4 on an IPv6 or zero address

Wrong: `addr.As4()` without checking the address family. `As4` panics if the
address is not a four-byte IPv4 address (including the zero `netip.Addr`).

Fix: use `As16()` for a universal 16-byte key, or check `addr.Is4()` before
calling `As4()`.

### Assuming a large array is cheap to pass

Wrong: passing a large `[N]T` by value in a hot path and paying a full copy on
every call. Value semantics do not become free just because the array is big.

Fix: pass `*[N]T` for large fixed buffers to avoid the copy, while still keeping
the compile-time size guarantee. Keep value passing for small arrays where the
copy is negligible and the immutability is a feature.

Next: [01-block-checksum-fixed-arrays.md](01-block-checksum-fixed-arrays.md)
