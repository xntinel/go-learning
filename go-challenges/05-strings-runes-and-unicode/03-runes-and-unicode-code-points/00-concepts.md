# Runes and Unicode Code Points at the Backend Boundary — Concepts

A `rune` is where a Go backend meets the messy reality of user-supplied text:
usernames, titles, log lines, HTTP headers, JSON bodies, `varchar` columns. The
senior job is never "iterate a string". It is deciding, at each I/O boundary,
whether the unit of correctness is a byte (storage and wire), a code point
(validation and truncation), or a grapheme (display and cursor movement), and
then defending that boundary against invalid UTF-8, control and bidi characters,
and multi-byte splits that silently corrupt data or open spoofing attacks. Read
this once and you have the model for all ten independent exercises that follow;
each is a boundary an on-call engineer actually owns.

## Concepts

### Three units, three boundaries

A `rune` is an alias for `int32` and denotes a Unicode *code point*: not a byte,
and not a user-perceived *grapheme cluster*. The distinction is the root cause of
most text bugs, so pin it with a concrete string. `café` is 4 runes, 5 bytes
(`é` is the two bytes `0xC3 0xA9`), and 4 graphemes. But `café` written with a
combining accent — `e` followed by `U+0301 COMBINING ACUTE ACCENT` — is 5 runes,
6 bytes, and still 4 graphemes. Same visible text, three different counts.

That gives you three units and three boundaries:

- Bytes are the unit of storage and the wire: `len(s)`, a `varchar(255)` budget,
  everything you hand to `io.Writer` or a socket.
- Code points are the unit of validation and truncation:
  `utf8.RuneCountInString(s)`, a username length limit, a safe cut.
- Graphemes are the unit of display and cursor movement, and they are *not* in
  the standard library — grapheme-cluster segmentation lives in a third-party
  library such as `github.com/rivo/uniseg` (out of scope here). Counting code
  points for a display width is a real bug: `é` as `e` plus
  a combining mark is two code points but one column.

Pick the unit that matches the boundary you own. Choosing wrong is not a style
nit; it is a data-corruption or spoofing bug.

### range decodes UTF-8 and yields (byteIndex, rune)

`for i, r := range s` decodes UTF-8 as it goes and yields the byte index of each
rune together with the rune itself. The index advances by the rune's encoded
width (1 to 4 bytes), not by 1, so on `café` the index sequence is `0, 1, 2, 3`
and then the loop ends after decoding the two-byte `é` at index 3. Indexing
`s[i]` instead yields a single `byte` (a `uint8`), never a rune — `"café"[3]` is
`0xC3`, a fragment.

The subtle part: invalid bytes do not panic. A byte sequence that is not valid
UTF-8 decodes to `utf8.RuneError` (`U+FFFD`, the replacement character) with a
width of 1, and the loop keeps going. So a `range` loop over untrusted bytes
*silently substitutes* rather than failing. If silent substitution is
unacceptable — and at a trust boundary it usually is — you must gate the input
with `utf8.ValidString` or repair it with `strings.ToValidUTF8` first, rather
than discovering the mojibake three services downstream.

### string(r) allocates; strings.Builder.WriteRune does not

`string(r)` allocates a fresh heap string holding `r`'s UTF-8 encoding. Doing it
once is fine; doing it per iteration in a hot loop — `out += string(r)` — is a
classic quadratic-allocation leak, because each `+=` also copies the whole
accumulator. `strings.Builder.WriteRune` encodes the rune directly into one
growing buffer with no intermediate strings, and `Builder.Grow(n)` pre-sizes
that buffer so a known-length transform allocates once. Every rune-rewriting
function in this lesson is built on a `Builder`.

### UTF-8 self-synchronizes, which makes safe truncation possible

UTF-8 was designed so that continuation bytes are always `10xxxxxx` and lead
bytes never are. That property — self-synchronization — is what lets
`utf8.DecodeLastRuneInString` walk *backward* from the end of a string to find a
rune boundary, and it is why truncating to a byte budget can always back up to a
safe cut instead of emitting a lone `0xC3`. `utf8.RuneLen(r)` returns the encoded
width of a rune (or `-1` if it is not a valid code point), letting you spend a
byte budget rune by rune without ever splitting one.

`utf8.RuneSelf` (`0x80`) is the ASCII fast path: any byte below it is a
standalone code point, which is why an ASCII-only validator can skip decoding
entirely and why `range` over pure ASCII is essentially a byte loop. It is the
invariant behind every fast UTF-8 validator.

### Case folding is not byte-lowering

`strings.ToLower` and `strings.ToUpper` operate correctly per rune, but they are
locale-independent. For *equality* the right tool is `strings.EqualFold`, which
applies Unicode simple case-folding: it matches `K` (`U+212A KELVIN SIGN`) to
`k`, and lower-case sigma to final sigma. `unicode.SimpleFold(r)` enumerates a
rune's fold orbit — the set of code points equivalent under simple folding —
cycling back to where it started. None of these do *locale-specific* rules such
as the Turkish dotless `i`, so they are correct for protocol tokens (HTTP header
names, email local parts, identifiers) but wrong for human-locale UI text, which
needs `golang.org/x/text/collate`. The one thing you must never do is lower raw
bytes: byte-lowering a multi-byte rune corrupts its continuation bytes and yields
invalid UTF-8.

### Normalization is orthogonal to case and to control-char stripping

`golang.org/x/text/unicode/norm` changes *composition*: NFC composes, NFD
decomposes, and NFKC/NFKD additionally apply compatibility mappings. ASCII-
folding a name for a slug is the pipeline "NFD, then drop `unicode.Mn` (nonspacing
marks), then NFC": decompose `é` into `e` plus a combining accent, drop the
accent, recompose. This is best-effort, not a bijection — `ß`, `ø`, and every
non-Latin script have no Latin decomposition, so a transliteration map is still
needed and the policy (drop, keep, or map) must be an explicit, tested decision.
Normalization, case-folding, and control-char stripping solve three different
problems; NFD-folding for a slug does not remove a zero-width space, and stripping
a bidi override does not normalize composition. You often need all three, in a
deliberate order.

### Security: zero-width and bidi runes are valid UTF-8 and dangerous

Zero-width characters (`U+200B` through `U+200D`, and `U+FEFF`) and bidirectional
control characters (`U+202A`..`U+202E`, `U+2066`..`U+2069`) are perfectly valid
UTF-8, yet they enable homoglyph spoofing and Trojan-Source attacks: an
identifier that renders identically to `admin` but is a different byte string, or
a source line that displays in an order different from how the compiler reads it.
The Unicode category `unicode.Cf` (format) covers all of these, `unicode.Cc`
covers C0/C1 control characters, and `unicode.Bidi_Control` is the bidi subset.
Stripping `Cf`/`Cc` at the trust boundary — for usernames and audit-log fields —
is the defense, and it is independent of normalization.

### Streaming: bufio.Reader.ReadRune reassembles a straddling rune

`bufio.Reader.ReadRune` returns `(rune, size, error)` and, crucially, correctly
reassembles a multi-byte rune that straddles the internal buffer boundary: it
fills more bytes until it has a full rune. A hand-rolled loop of `Read` plus
`utf8.DecodeRune` does *not* handle a rune split across two `Read` calls unless
you accumulate the short read yourself — and short reads are the norm under TCP
fragmentation or `iotest.OneByteReader`. That is why a log or token scanner that
must not miscount uses `ReadRune` (or accumulates before decoding). Invalid
encoding surfaces as `RuneError` with `size == 1`, distinct from a real `U+FFFD`
in the input (which decodes with `size == 3`), and a clean end is `io.EOF`.

## Common Mistakes

### Using strings.ToLower as a substitute for per-rune folding

Wrong: `strings.ToLower` on a whole string for case-insensitive comparison, or
worse, lowering raw bytes. Byte-lowering a multi-byte rune corrupts its trailing
continuation byte and yields invalid UTF-8, and even correct `ToLower` misses
`K` (`U+212A`). Fix: fold per rune, or use `strings.EqualFold` for comparison.

### Using len(s) as a character count for a validation limit

Wrong: `if len(s) > 30` for a username or title limit. `len` is bytes: a 30-byte
limit rejects a 15-character accented name and admits a 30-emoji name that is far
longer visually. Fix: `utf8.RuneCountInString(s)` for a code-point limit, and
keep `len` only for a byte/storage budget.

### Truncating with s[:n] on an arbitrary byte offset

Wrong: `s[:n]` where `n` lands inside a multi-byte rune, emitting a broken tail
(a lone `0xC3`) that breaks JSON marshaling, the DB write, or the next consumer.
Fix: walk back to a rune boundary with `utf8.DecodeLastRuneInString`.

### Assuming range never substitutes

Wrong: trusting that `for _, r := range s` preserves every byte. A slice built
from an untrusted source may hold invalid UTF-8, and `range` yields `U+FFFD`
width-1 silently. Fix: if fidelity matters, gate with `utf8.ValidString` or
repair with `strings.ToValidUTF8` before iterating.

### Building output with concatenation inside a rune loop

Wrong: `out += string(r)` inside a `for range`, which is quadratic. Fix:
`strings.Builder` with `WriteRune` (and `Grow`) so the encoding happens once into
a single buffer.

### Confusing a code point with a grapheme

Wrong: counting `é` written as `e` plus a combining acute as 2 for a display
width or cursor step. Code-point count is correct for storage and validation but
wrong for what the user sees; grapheme segmentation needs a third-party library
such as `github.com/rivo/uniseg`.

### Per-Read DecodeRune without handling a rune split across reads

Wrong: reading a stream with `utf8.DecodeRune` on each `Read`'s bytes. Under
short reads (`iotest.OneByteReader`, TCP fragmentation) a rune spanning two reads
miscounts or errors. Fix: `bufio.Reader.ReadRune`, which fills until it has a full
rune.

### Treating normalization, folding, and stripping as one step

Wrong: assuming an NFD-fold for a slug also removes a zero-width space or a bidi
override. They solve different problems. Fix: an explicit `Cf`/`Cc` strip for the
security boundary, separate from normalization and from case-folding.

### Comparing headers or emails with strings.ToLower

Wrong: lowering both sides and expecting Unicode correctness (it misses `U+212A`)
or applying it to human-locale text and getting the Turkish `i` wrong. Fix:
`EqualFold` for protocol tokens; locale-aware collation (`x/text/collate`) for
human text.

### Assuming ASCII-folding is lossless or reversible

Wrong: expecting NFD-folding to handle `ß`, `ø`, or non-Latin scripts. They have
no Latin decomposition, so folding silently leaves or drops them. Fix: make the
policy (drop, keep, map) an explicit, tested decision, not an accident of the
algorithm.

Next: [01-slug-generator-code-point-loop.md](01-slug-generator-code-point-loop.md)
