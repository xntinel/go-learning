# Building a Text-Processing Pipeline for Search Ingestion — Concepts

Text normalization looks like a formatting nicety and is actually a
correctness-and-security concern. In a real backend, untrusted bytes arrive from
an `io.Reader` — a multipart upload, a proxied HTTP body, a Kafka message value,
a tailed log file — and have to become deterministic, index-safe, storage-safe
records before they touch a search engine, a unique index, or a dedup map. Get
the transforms or their order wrong and you do not get a cosmetic glitch: you get
a Bleve or Elasticsearch bulk request rejected by one invalid byte, a `UNIQUE`
constraint that lets two visually identical usernames through, a `varchar`
column truncated into corrupted UTF-8, or a secret indexed in plaintext. This
lesson builds the ingestion path as a chain of small, pure, table-tested
transforms plus a streaming orchestration layer that owns boundaries and error
context. Read this once and each of the ten independent exercises that follow
becomes a single well-scoped stage you can reason about in isolation.

## Concepts

### A pipeline is an ordered contract

The unit of work is deliberately boring: `type Transform func(string) string`. A
transform takes a string and returns a string, with no error, no state, no I/O.
That shape is what makes each stage trivially testable, composable, and
replaceable, and it is what lets a `Pipeline` be nothing more than an ordered
slice of transforms folded left-to-right over the input.

The order is not cosmetic; it encodes real data dependencies. Decode HTML
entities before lowercasing, or `&Eacute;` never becomes `é` and your lowercase
step operates on the literal characters `&`, `E`, `a`, ... Remove control
characters before collapsing whitespace, or an embedded `NUL` or `BEL` survives
into a "collapsed" field and poisons the index with invisible bytes. Normalize
UTF-8 before anything that hashes, compares, or indexes, or two byte-different
spellings of the same word hash to two different keys. Ordering is a contract the
pipeline's assembler owns and a reviewer should be able to justify stage by
stage.

### Streaming beats whole-file assumptions

Binding the ingester to `io.Reader` rather than `*os.File` or a `[]byte` is what
keeps the package independent of where the bytes came from. The same
`ProcessJSONLines(r io.Reader)` reads a `*os.File`, an `http.Request.Body`, a
`strings.NewReader` in a test, or a Kafka value wrapped in `bytes.NewReader`.

The trap is `bufio.Scanner`. Its default maximum token size is 64 KiB
(`bufio.MaxScanTokenSize`). A production record — a fat log line, a document with
a large body — that exceeds that limit makes `Scan` stop and `Err` return
`bufio.ErrTooLong`. Code that ignores `scanner.Err()` silently truncates its
input at the first oversized line and reports success. Two rules follow and are
non-negotiable: call `scanner.Buffer(make([]byte, 0, initial), max)` with a
documented, deliberate maximum, and always return `scanner.Err()` after the scan
loop.

### UTF-8 validity is an ingestion invariant

A Go `string` (and `[]byte`) is an arbitrary byte sequence, not guaranteed valid
UTF-8. Bytes from an upload or a foreign producer can contain a lone continuation
byte (`0x80`), a truncated multi-byte lead (`0xC3` with nothing after it), or any
other malformed sequence. That matters because `encoding/json` will refuse to
re-encode invalid UTF-8 faithfully (it emits U+FFFD), and search-engine bulk
APIs reject documents with invalid byte sequences outright. Validity is therefore
a boundary invariant, established once at ingestion. Two honest options exist:
reject the record with `utf8.ValidString` (strict mode, returns a line-numbered
error) or repair it with `strings.ToValidUTF8`, which replaces each maximal
invalid subsequence with a replacement string (typically the U+FFFD rune). Repair
is lossy but keeps the pipeline flowing; rejection is safe but drops data. Pick
per source and document which.

### Canonical equivalence is a correctness bug, not cosmetics

Unicode lets the same visible text be encoded more than one way. "café" can end
in a single precomposed code point U+00E9 (`é`), or in the base letter `e`
(U+0065) followed by a combining acute accent U+0301. The two strings are
visually identical, byte-different, and therefore unequal under `==`, unequal as
map keys, and unequal to a database `UNIQUE` index. A dedup map or a unique
constraint that does not normalize first lets both spellings through — the
classic production duplicate that "looks the same" in every log line. The fix is
to pick one canonical form and normalize to it at the boundary. NFC (Normalization
Form C, the composed form) is the conventional storage canonical; normalize every
value that becomes a key, a dedup input, or an indexed field to NFC with
`golang.org/x/text/unicode/norm`, and the collision disappears. NFC is
idempotent: `NFC(NFC(s)) == NFC(s)`.

### Caseless matching has tiers, and they are not interchangeable

There are three tools and they are genuinely different:

- `strings.EqualFold(a, b)` performs Unicode *simple* case folding and answers a
  single pairwise question: are these two strings equal ignoring case? It is
  convenient for a one-off compare, but it is a comparison, not a value — you
  cannot store it, index it, or use it as a map key. And simple folding does not
  handle one-to-many foldings: `EqualFold("straße", "STRASSE")` is `false`.
- `strings.ToLower(s)` is Unicode-aware, locale-independent lowercasing. It
  produces a value, but lowercasing is not the same as caseless matching; it does
  not fold `ß` to `ss` and it is not the canonical form the Unicode standard
  defines for caseless comparison.
- `cases.Fold()` from `golang.org/x/text/cases` produces a full Unicode
  case-folded string — a stable canonical key you can store in a column and index.
  `cases.Fold().String("straße")` and `cases.Fold().String("STRASSE")` both yield
  `"strasse"`, so the pair collides in a uniqueness set where `EqualFold` said they
  differed.

Storing a folded canonical column beats comparing at query time: the database can
enforce uniqueness and use an index, instead of table-scanning with a fold
function. The recommended canonicalization for a username or email-local key is
NFC first, then fold, and that composition is idempotent. One honest caveat: full
case folding does not solve every script. `İ` (U+0130, Turkish dotted capital I)
folds to `i` + combining dot above (U+0307), so `İstanbul` does *not* fold-equal
`ISTANBUL`. Caseless matching is defined by Unicode, not by intuition.

### Diacritic stripping is decompose, drop marks, recompose

Generating a slug or an ASCII-ish canonical key ("Résumé Cafétéria" → "resume
cafeteria") is a three-step normalization, not a lookup table: decompose to NFD so
each accented letter splits into a base letter plus its combining marks, remove
the runes in Unicode category `Mn` (Mark, nonspacing — the combining accents),
then recompose to NFC. `golang.org/x/text/transform.Chain` wires the three
transformers into one, and `runes.Remove(runes.In(unicode.Mn))` is the mark
filter. This canonicalizes Latin-script text; it does *not* romanize other
scripts — Greek, Cyrillic, Han, and Arabic pass through essentially unchanged,
because their letters are not Latin-base-plus-accent. Keep that in the input
contract: a slug generator strips diacritics, it does not transliterate.

### Length limits are measured in runes, never bytes

A `varchar(64)` limit or a search-field cap is a limit on characters, and slicing
a Go string at a byte offset (`s[:64]`) can cut through the middle of a multi-byte
rune, producing invalid UTF-8 and a corrupted stored value. Count with
`utf8.RuneCountInString` and cut on a rune boundary — iterate with `for i := range
s` (the index is always a rune boundary) or step with `utf8.DecodeRuneInString`.
Append an ellipsis only when truncation actually removed something. And be honest
about the limit of "rune": a user-perceived character (a grapheme cluster) can be
several runes — an emoji with a skin-tone modifier or a ZWJ sequence, a base
letter plus multiple combining marks. Rune-count truncation never corrupts UTF-8,
but if you need "at most N *perceived* characters" you need a grapheme
segmentation library. State which guarantee you are giving.

### Allocation control matters at ingestion scale

Building output by repeatedly concatenating (`out += string(r)`) inside a loop is
quadratic: each `+=` allocates a fresh backing array and copies everything so far.
At ingestion scale that is the difference between linear and quadratic time. Build
rune-by-rune output with `strings.Builder`, and call `Grow(len(s))` up front so
the builder allocates its backing array once. `WriteRune`/`WriteByte`/`WriteString`
append without reallocating until the reserved capacity is exceeded. This is the
standard shape for any transform that filters or rewrites characters.

### Entity decoding is not HTML parsing; regex is not an HTML parser

`html.UnescapeString` decodes HTML entities — `&amp;` → `&`, `&#39;` → `'`,
`&Eacute;` → `É`, `&#x20AC;` → `€` — in text that has *already been extracted*
from HTML. It does not parse markup, strip tags, or defend against malformed HTML.
If your source is raw HTML, extract the text with a real parser
(`golang.org/x/net/html`) upstream, then feed the extracted fields to this
pipeline. Never ship a regular expression that strips `<...>` tags and call the
result safe: HTML is not a regular language, and a regex tag-stripper is a
security bug waiting for a crafted input. Keep the input contract honest —
"already-extracted text, entity-decoded here" — rather than pretending to parse.

Regex *is* the right tool for well-defined token grammars: an email address, a
`Bearer` token, a long hex or base64 blob. Redacting those before indexing is a
legitimate, tightly-scoped regex job. The distinction is not "regex bad" but
"regex for regular grammars, a parser for recursive ones."

### Error context is a debugging feature

A malformed record in a million-line file is only debuggable if the error says
*which* line. Wrap decode failures with `fmt.Errorf("line %d: ...: %w", n, err)`
so the position is in the message and the underlying error is preserved for
`errors.Is`/`errors.As`. Blank lines must be skipped without disturbing the line
counter, so a "line 2" error still points at physical line 2 even if line 1 was
blank. The wrapped `%w` chain is not decoration; it is what lets a caller both
print a human-readable location and programmatically match the underlying cause.

## Common Mistakes

### Trusting the default Scanner buffer

Wrong: `bufio.NewScanner(r)` and assuming every record fits under 64 KiB, then
ignoring the return of `scanner.Err()`. An oversized production line silently
truncates the stream and the function reports success.

Fix: `scanner.Buffer(make([]byte, 0, initial), maxBytes)` with a documented
maximum, and always return `scanner.Err()` after the loop so `bufio.ErrTooLong`
surfaces instead of being swallowed.

### Treating a regex as an HTML parser

Wrong: stripping tags with a regular expression and calling the output safe.

Fix: parse HTML upstream with a real parser, or constrain the contract to
already-extracted text and only decode entities with `html.UnescapeString`.

### Assuming Go strings are valid UTF-8

Wrong: passing bytes from an upload straight into JSON re-encoding or a search
bulk request. Invalid sequences poison the encode and get the document rejected.

Fix: guard at the boundary — `utf8.ValidString` to reject, or `strings.ToValidUTF8`
to repair to U+FFFD — before the record travels further.

### Comparing or deduping without normalization

Wrong: using raw user text as a map key or relying on a `UNIQUE` index, so NFC vs
NFD spellings of the same word slip through as two rows.

Fix: normalize to NFC (and for caseless keys, fold) before the value becomes a key
or hits a unique constraint.

### Confusing EqualFold/ToLower with a canonical form

Wrong: reaching for `strings.EqualFold` or `strings.ToLower` as if either were a
storable caseless canonical. `EqualFold` is a pairwise comparison, not a value;
`ToLower` does not fold `ß`.

Fix: use `cases.Fold()` to produce the canonical key you store and index; fold
after NFC.

### Truncating on a byte offset

Wrong: `s[:n]` to enforce a length budget, splitting a multi-byte rune into
invalid UTF-8 and corrupting the stored column.

Fix: count with `utf8.RuneCountInString` and cut on a rune boundary; document
whether the budget is in runes or graphemes.

### Quadratic string building

Wrong: `out += string(r)` in a per-rune loop, reallocating and copying on every
iteration.

Fix: `strings.Builder` with `Grow(len(s))`, then `WriteRune`.

### Getting the transform order wrong

Wrong: lowercasing before entity decode, or collapsing whitespace before removing
controls, so half-decoded entities or invisible bytes leak into the index.

Fix: fix and document the order — repair UTF-8, decode entities, remove controls,
normalize, fold/lower, collapse whitespace — and assert it with a test.

### Discarding the error from transform.String or json.Unmarshal

Wrong: `out, _, _ := transform.String(t, s)` or ignoring the `json.Unmarshal`
error, hiding malformed input.

Fix: check every error and surface it with line/field context.

### Mixing parsing, cleanup, and formatting in one function

Wrong: one function reads JSON, mutates strings, and formats output, so nothing is
independently testable and ordering is implicit.

Fix: keep parsing in the ingester, cleanup in pure transforms, and presentation
outside the package.

Next: [01-ordered-transform-pipeline.md](01-ordered-transform-pipeline.md)
