# 2. HPACK Header Compression — Concepts

HPACK (RFC 7541) compresses HTTP/2 header fields by exploiting three redundancies on a single connection. Pseudo-headers that repeat on every request (`:method: GET`, `:scheme: https`) live in a fixed static table and are sent as a single index byte. Headers first seen on this connection are appended to a connection-scoped dynamic table and later referenced by index. Raw string values are Huffman-coded with a canonical tree tuned for HTTP. Underneath those three ideas sit two wire primitives every representation is built from: an integer encoding with an N-bit prefix, and a length-prefixed string literal with an optional Huffman flag.

This file is the conceptual foundation. Read it once and you have everything needed to reason through the exercises, which build HPACK from both ends: a connection-scoped context around the canonical `golang.org/x/net/http2/hpack` that ties the pieces together, then three independent modules that reimplement the primitives the library hides — the N-bit-prefix integer codec with its overflow guard, the RFC 7541 Appendix B Huffman codec, and the dynamic table with its eviction and size-update rules.

## Concepts

### The Static Table And The Dynamic Table

HPACK maintains two indexed tables that share one address space. The static table (RFC 7541 Appendix A) is a fixed list of 61 common header fields at indices 1-61: entry 2 is `:method: GET`, entry 4 is `:path: /`, entry 7 is `:scheme: https`. When the encoder finds a header there it emits one byte with the high bit set and the 7-bit index, so `:method: GET` becomes `0x82` with no name or value bytes at all.

The dynamic table occupies indices 62 and up. It is a connection-scoped FIFO: a new entry is prepended so the newest is always the lowest dynamic index, and every existing entry's index shifts up by one. Both peers maintain their own copy and the copies must stay byte-for-byte identical, because an index in one header block is resolved against whatever the table holds at that moment. If the encoder inserts entry A then entry B but the decoder processes only one block, every subsequent index resolves to the wrong header, and nothing in the stream says which block diverged. This is why any encode or decode error is connection-fatal under RFC 7540 section 5.4.1: a desynchronized table cannot be repaired, only torn down.

### Integer Encoding With An N-Bit Prefix

Every numeric quantity in HPACK — an index, a string length, a table-size update — uses the same integer encoding (RFC 7541 section 5.1), and it begins mid-byte. The first byte carries N usable bits, where N is 4, 5, 6, or 7 depending on the representation, because the high 8-N bits already hold representation flags. If the value fits in `2^N - 1` it sits directly in the prefix and costs nothing extra. Otherwise the prefix bits are all set to 1 and the remainder is emitted in continuation bytes, 7 value bits per byte, the top bit a continuation flag (1 = more follow, 0 = last). Small indices, the common case, cost one byte; magnitude grows the encoding by one byte per additional 7 bits.

The decoder of this loop is a documented denial-of-service surface. A hostile peer can send an unbounded run of continuation bytes, each with the high bit set, and a naive decoder either loops forever or overflows its accumulator into a negative or wrapped value that is then used as a length or an allocation size. RFC 7541 section 5.1 explicitly requires a bound: stop and reject once the value would exceed the implementation's integer limit. The guard must fire *before* the shift overflows, not after, because by the time the accumulator has wrapped the damage is done.

### String Literals And Huffman Coding

A string not covered by an index is a length-prefixed octet sequence. The first bit of the length field is the Huffman flag: 0 means raw octets, 1 means Huffman-coded with the static canonical tree of RFC 7541 Appendix B. That tree assigns 5-bit codes to the most common HTTP characters (lowercase letters, digits, common punctuation) and progressively longer codes — up to 30 bits — to rare bytes, so typical header values shrink 20-30%.

Two details make the Huffman codec subtle. First, the final byte is padded to a byte boundary using the most-significant bits of the EOS symbol, which are all ones; a decoder must accept up to 7 such padding bits and reject anything longer, because a full byte of "padding" hides a dropped symbol. Second, the 30-bit EOS symbol must never appear as a decoded value: its presence in the body, or padding that is not a strict prefix of it, is a decoding error. The code is prefix-free, so decoding is a bit-at-a-time walk that emits a symbol the instant the accumulated bits match a code.

### Entry Size, Eviction, And The Size-Update Directive

Each dynamic table entry is accounted at `len(name) + len(value) + 32` bytes (RFC 7541 section 4.1); the 32-byte constant models per-entry bookkeeping so a table of many tiny headers cannot blow up memory. The table has a maximum size, 4096 bytes by default. Adding an entry first evicts the oldest entries — highest indices — until the newcomer fits. An entry whose own accounted size exceeds the maximum cannot be stored at all; per RFC 7541 section 4.4 the attempt empties the table and adds nothing.

The maximum is not fixed. When a peer advertises a new `SETTINGS_HEADER_TABLE_SIZE`, the encoder must emit a dynamic table size update — itself an integer with a 5-bit prefix — at the very start of the next header block, before any field, and then evict down to the new bound. The decoder applies that update in-stream. A size update that arrives after any field representation in a block is illegal and must be rejected. Reducing the bound to 0 and raising it again is the canonical way to flush the table without tearing down the connection.

### Never-Indexed (Sensitive) Header Fields

RFC 7541 section 7.1 defines a never-indexed literal representation (first byte `0001xxxx`). A field encoded this way must never enter any table, and a forwarding proxy must re-emit it never-indexed. This is the defence against CRIME-style compression oracles: because the value of `authorization`, `cookie`, or `set-cookie` never enters the dynamic table, an attacker who can watch compressed sizes cannot binary-search a secret by injecting guesses and observing when the compressed length drops. Enforcing this per header name in one place, rather than trusting each caller to set a flag, is what makes it reliable.

## Common Mistakes

### Treating An Encode Or Decode Error As Recoverable

The dynamic tables of the two peers are a shared, ordered state machine. Once a block fails to encode or decode, the local table may have advanced while the peer's did not, and every later index resolves against a table the peer no longer agrees on. Continuing produces silent header corruption far from the root cause. The fix is mechanical: any HPACK error is a connection error (RFC 7540 section 5.4.1); tear the connection down rather than skip the block.

### Decoding A Variable-Length Integer Without A Bound

A decoder that trusts the continuation flag will follow an attacker's endless run of `0x80`-or-higher bytes until it overflows or hangs. The overflowed accumulator is worse than the hang because it becomes a small or negative "length" that the caller then trusts. Bound the value and reject before the shift can wrap, treating both overflow and a continuation byte with no successor as hard errors.

### Mishandling Huffman Padding

Two opposite errors live here. Accepting more than 7 bits of trailing padding lets a corrupted stream hide a missing symbol; rejecting valid 1-7 bit EOS-prefix padding breaks legitimate input. The rule is exact: 1 to 7 trailing bits, all of which must be the leading bits of EOS (all ones). Anything else, including the EOS symbol appearing in the body, is invalid.

### Forgetting Never-Indexed Enforcement For Sensitive Headers

Encoding `authorization` as an ordinary indexed literal puts the token in the dynamic table, where a shared cache or a compression oracle can reach it. Relying on callers to mark each sensitive field is fragile. Enforce the never-indexed representation by header name in one chokepoint so a forgotten flag cannot leak a credential.

### Changing The Table Size In The Middle Of A Block

A size update is only legal before any field representation in a block. Emitting one mid-block — for example by adjusting the encoder's table size after the first field has been written — produces a stream the decoder must reject. Change the table size only between blocks, then let the eviction run before the first field of the next block.

---

Next: [01-hpack-context.md](01-hpack-context.md)
