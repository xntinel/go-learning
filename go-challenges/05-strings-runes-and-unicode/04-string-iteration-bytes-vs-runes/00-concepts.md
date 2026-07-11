# String Iteration: Bytes vs Runes in Production Text Handling — Concepts

Every backend that accepts user-generated text answers one recurring question at
each boundary: am I counting and slicing in bytes, or in characters? A display
name, a comment body, a filename, a JSON field, a log line — each is a Go string,
and a Go string makes no promise about which unit is correct. Get it wrong and the
failure is not a compile error; it is a truncated multi-byte rune that decodes to
U+FFFD garbage downstream, a `varchar(255)` insert that overflows because 255
"characters" of CJK is 765 bytes, a "50 character" validation that silently
accepts a 200-byte name, a redaction mask that leaks the tail of a token, or a
JSON encoder that errors deep in the stack on invalid UTF-8 you never validated at
ingest. This file is the model behind request validation, safe truncation for
storage and queues, log scrubbing, streaming decode, PII masking, and parser error
reporting. Read it once; the nine exercises that follow each stand alone and apply
one facet of it to a real backend task.

## A string is bytes, and UTF-8 is only a convention

A Go string is an immutable, read-only slice of bytes with *no enforced encoding*.
UTF-8 is the convention the standard library and most of the ecosystem assume, but
it is a convention, not an invariant the `string` type guarantees. You can put any
byte sequence into a string — including bytes that are not valid UTF-8 — and the
compiler will not stop you. `string([]byte{0xff})` is a perfectly legal one-byte
string. This is the single most consequential fact in the whole topic: because
invalid UTF-8 is *representable*, it must be *validated* at the boundaries where it
matters, or it will surface as corruption far from where it entered.

Two operations act on bytes and are O(1): `len(s)` is the byte length (a single
field read, not a scan), and `s[i]` is the `i`-th byte, of type `byte` (an alias
for `uint8`). Neither decodes anything. `s[i]` for a multi-byte character gives you
one fragment of a UTF-8 sequence, not a character.

## Range decodes; the index is a byte offset

`for i, r := range s` is different in kind from indexing. It *decodes* the string
as UTF-8, yielding successive runes. `r` is a `rune` (an alias for `int32`, a
Unicode code point), and its cost is proportional to the rune count. The subtle,
bug-breeding detail is `i`: it is the **byte offset** of the start of each rune,
not a rune index. For a string of multi-byte runes those offsets are
non-contiguous — `0, 3, 6, ...` for three CJK characters, not `0, 1, 2`. If you
want a rune index, you maintain a separate counter:

```go
runeIndex := 0
for byteOffset, r := range s {
	_ = byteOffset // start-of-rune byte position, NOT a rune index
	_ = r
	runeIndex++
}
```

Choosing between byte indexing and range is choosing your unit of correctness, not
a matter of style. Bytes are the unit for storage limits, wire formats, and binary
protocols. Runes are the unit for most "character" limits and for anything a human
counts.

## Three different "lengths", routinely conflated

There are three lengths of a piece of text, and production bugs come from using one
where another is meant:

- **Byte length** — `len(s)` and `utf8.RuneLen(r)`. This is what storage and wire
  limits are measured in: a Postgres `varchar(n)` byte cap, a DynamoDB 400 KB item
  limit, a fixed-width binary field, a Kafka message size.
- **Code-point count** — `utf8.RuneCountInString(s)`, or equivalently the number
  of iterations of `for range s`. This is what most "character" limits mean: a
  50-character display name, a 280-character post.
- **Grapheme-cluster / display-cell count** — what a *human* perceives as one
  "character" or how many terminal columns it occupies. Go's standard library does
  not compute this; you reach for `golang.org/x/text` or a segmentation library.

These three diverge whenever text leaves ASCII. `café` is 5 bytes but 4 runes.
`中文` is 6 bytes but 2 runes. And even the rune count is not the human count: `é`
written as `e` followed by the combining acute accent U+0301 is two runes for one
visible character; a family emoji built from several code points joined by
zero-width joiners is many runes for one glyph; an East Asian wide character is one
rune but two terminal cells. Runes are code points, and code points are the ceiling
of what the byte-vs-rune model can express. Knowing where that ceiling is — and
naming `golang.org/x/text` as the tool past it — is part of doing this correctly.

## ASCII is the fast path

A byte below `utf8.RuneSelf` (0x80) is a single-byte rune that is its own value:
for the ASCII range, the byte and the rune coincide, byte indexing is safe, and the
byte count equals the rune count. This is why `strings` functions special-case
ASCII and why a validator can shortcut when it knows a field is ASCII-only.
`utf8.UTFMax` (4) bounds the encoded size of any single rune, which is why a decode
buffer never needs more than four bytes of lookahead.

## RuneError is ambiguous — check the size

When `range` or `utf8.DecodeRuneInString` meets a byte that is not valid UTF-8, it
yields `utf8.RuneError` (U+FFFD, the replacement character) and advances by one
byte — `DecodeRuneInString` returns `(RuneError, 1)`. The trap: U+FFFD is *also a
perfectly legitimate character* that can appear in valid input, and there it
decodes as `(RuneError, 3)` (its own three-byte UTF-8 encoding). Therefore
`r == utf8.RuneError` alone does **not** mean "decoding failed" — it false-positives
on real replacement characters. The correct failure test is
`r == utf8.RuneError && size == 1`, or an explicit `utf8.Valid` / `utf8.ValidString`
check. Answering "did decode fail?" with `r == RuneError` and no size check is one
of the most common latent bugs in text code.

## Slicing by byte offset can split a rune

`s[:n]` truncates at a byte boundary. If byte `n` lands in the middle of a
multi-byte rune, the result is invalid UTF-8 — a lone lead byte or a dangling
continuation byte. That invalid string then travels: a JSON encoder mangles it to
U+FFFD or errors, a Postgres UTF8 column rejects the insert, a downstream decoder
reports garbage. Safe byte-budget truncation must back off to a rune boundary:
`utf8.RuneStart(b)` reports whether a byte can begin a rune, and
`utf8.DecodeLastRuneInString` peels the final rune cleanly. The rule is: never cut
a string at an arbitrary byte offset for storage or display without landing on a
rune boundary.

## Reject vs repair is a policy decision per boundary

Given possibly-invalid UTF-8 you have exactly three options, and which one is right
depends on the boundary:

- **Reject** — `utf8.Valid` / `utf8.ValidString` return false; you refuse the input
  with an error. Correct at an ingest boundary that must not persist garbage (an
  HTTP write path feeding a text column).
- **Repair** — `strings.ToValidUTF8(s, replacement)` replaces each maximal run of
  invalid bytes with a replacement (typically U+FFFD). Correct for a pipeline that
  must never drop a record (log/telemetry ingestion): make it safe to encode and
  index rather than lose the line.
- **Pass through** — you know the source is trusted or already validated upstream.

`utf8.ValidRune` is the single-code-point cousin: it reports whether one rune can be
legally encoded (in range, not a surrogate half). Reject and repair are not
interchangeable defaults; each boundary picks one deliberately.

## Streaming: decode across read-buffer seams

When text is too large to hold in memory — a large request body, an uploaded file —
you still may need to operate per character. The wrong way is to call
`utf8.DecodeRune` on each `Read` chunk independently: a rune whose bytes straddle
two reads is corrupted at the seam, because the first chunk ends with an incomplete
sequence. The right tool is `bufio.Reader.ReadRune`, which buffers and reassembles
UTF-8 sequences that span read boundaries and hands you one rune at a time. It
surfaces the same `(RuneError, size 1)` contract for invalid bytes, and a clean
`io.EOF` at the end.

## Common Mistakes

### Treating the range index as a rune index

Wrong: using `i` from `for i, r := range s` to index a parallel `[]rune` or to
compute a column position. `i` is the byte offset of the rune's start; for
multi-byte input it skips ahead by the rune's width, so it is not `0, 1, 2, ...`.

Fix: keep a separate `runeIndex++` counter for a rune index, and treat `i` only as
a byte position (which is exactly what you want when the value it feeds — a
`json.SyntaxError.Offset`, a slice bound — is itself in bytes).

### Enforcing a "character" limit with len

Wrong: `if len(name) > 50 { reject }`. Correct only for ASCII; for any multi-byte
input it under-counts characters and lets over-long names through. The mirror bug is
using a rune limit (`utf8.RuneCountInString(name) > 50`) to guard a *byte*-capped
store and overflowing the column when 50 CJK runes are 150 bytes.

Fix: count in the store's unit. Character limits use `utf8.RuneCountInString`;
byte-capped stores validate `len` (and truncate on a rune boundary, below).

### Byte-slicing to truncate

Wrong: `s[:n]` to shorten for display or storage, which splits a multi-byte rune and
emits invalid UTF-8.

Fix: back off to a rune boundary with `utf8.RuneStart` / `utf8.DecodeLastRuneInString`
(the byte-budget truncation exercise), or convert to `[]rune` for a character cap.

### Detecting decode failure with RuneError and ignoring size

Wrong: `if r == utf8.RuneError { corrupt = true }`. This flags legitimate U+FFFD
characters that were already present in valid input.

Fix: test `r == utf8.RuneError && size == 1`, or call `utf8.Valid`/`ValidString`.

### Indexing bytes where a rune is meant

Wrong: `for i := 0; i < len(s); i++ { use(s[i]) }` when `use` wants a character. For
multi-byte input each `s[i]` is a fragment of a sequence, not a rune.

Fix: `for _, r := range s` for per-character work, or `utf8.DecodeRuneInString` when
you must also track byte widths.

### Assuming rune count equals what a human sees

Wrong: reporting `utf8.RuneCountInString` as "the number of characters". Combining
marks (`e` + U+0301) and emoji ZWJ sequences count as several runes for one visible
grapheme; East Asian wide runes are one rune but two display cells.

Fix: know the ceiling. For true grapheme or display-width counting, reach for
`golang.org/x/text` (normalization / width) or a segmentation library; document the
limitation where you rely on rune count.

### Padding fixed-width output by byte width

Wrong: `fmt.Sprintf("%-10s", cell)` to align a column — `%-*s` pads to a byte width,
so any non-ASCII cell shears the table.

Fix: pad by rune count; and know that even rune count misaligns East Asian wide
cells, which is where `golang.org/x/text/width` or a runewidth library is required.

### Decoding a stream chunk-by-chunk

Wrong: `utf8.DecodeRune` on each independent `Read` result, corrupting runes whose
bytes straddle two reads.

Fix: `bufio.Reader.ReadRune`, which reassembles across the seam.

### Converting to []rune in a hot loop just to count

Wrong: `len([]rune(s))` inside a hot path — it allocates an O(n) slice only to count.

Fix: `utf8.RuneCountInString(s)` or a `for range` loop counts allocation-free.

### Skipping validation at ingest

Wrong: assuming a Go string is always valid UTF-8 and never validating, then
discovering the corruption only when a JSON re-encode or a Postgres UTF8 insert
fails deep in the stack.

Fix: validate (`utf8.ValidString`) or repair (`strings.ToValidUTF8`) at the ingest
boundary, as a deliberate reject-vs-repair policy.

Next: [01-formcount-bytes-runes-stats.md](01-formcount-bytes-runes-stats.md)
