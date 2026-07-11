# String Basics for Backend Services — Concepts

Strings are the substrate of every backend boundary: HTTP request lines and
headers, log lines, config values, SQL and CSV rows, bearer tokens, and raw user
input. At a junior level "what is a string" is a trivia question. At senior level
the interesting questions are operational: what does Go's string model *cost*,
where does it *leak*, and where does a naive one-liner silently corrupt Unicode
or blow up latency under load. This file is the conceptual foundation for the
independent exercises that follow; read it once and each exercise becomes a
concrete application of one of these ideas to a real artifact — a parser, a
header decoder, a query builder, a validator, a sanitizer, a router.

## The model: an immutable two-word view over bytes

A Go `string` is a read-only slice of bytes described by a two-word header: a
pointer to the backing bytes and a length. The header is copied by value cheaply
(sixteen bytes on a 64-bit machine); the bytes it points at are never mutated in
place. Every operation that looks like a mutation — `s + "x"`, `strings.ToUpper`,
`strings.TrimSpace` — returns a *new* string over new bytes; the original is
untouched. Immutability is what makes a string safe to share across goroutines
without a lock and safe to use as a map key.

`len(s)` returns the length word: the number of *bytes*, not characters. For
ASCII the two coincide, which is exactly why the bug hides in tests and surfaces
in production the first time a customer types an accent or an emoji. `len("café")`
is 5, not 4, because `é` is two UTF-8 bytes. Any rule expressed in characters — a
"max 50 characters" display-name limit, a database `varchar` semantics check, a
truncation for a UI — must be computed with `utf8.RuneCountInString(s)`, never
`len(s)`.

## Slicing is free, and that is the trap

`s[i:j]` produces a new string header that points *into the same backing array*.
It is O(1) and allocates nothing — no copy of the bytes. This is why slicing is
fast, and it is also the sharpest edge in the whole model. The sub-string keeps
the entire original backing array alive for the garbage collector. Extract a
20-byte request ID out of a 2 MB request body with `body[start:end]`, stash it in
a cache or a long-lived struct field, and you have pinned two megabytes that can
never be reclaimed while that 20-byte view exists. It is a textbook memory
retention bug: invisible in a unit test, fatal under sustained load.

The fix is `strings.Clone(s)`, which copies the bytes into a fresh minimal
allocation and severs the link to the big buffer. (`string([]byte(s))` does the
same by round-tripping through a byte slice.) Clone is the right tool *only* when
a small piece of a large buffer must outlive the buffer; cloning everything just
wastes memory, so reach for it deliberately, guided by the retention it prevents.

## Equality and ordering are byte operations

`a == b` compares the byte sequences directly; the compiler lowers it to an
efficient runtime routine that first checks the lengths and then the bytes. There
is no need for `bytes.Equal([]byte(a), []byte(b))` — those conversions allocate
and copy for nothing. `map[string]V` uses this same equality for key lookup.
Ordering with `<`/`cmp.Compare` is lexicographic over the raw UTF-8 bytes, which
for ASCII matches dictionary order and gives deterministic, stable output for log
dedup and canonical serialization.

Case-insensitive comparison is the common variation, and the naive form is a
per-request allocation trap: `strings.ToLower(a) == strings.ToLower(b)` builds
two brand-new lowercased strings on every call. On a hot path — matching HTTP
methods, header names, hostnames, enum tokens — that is pure garbage-collector
pressure. `strings.EqualFold(a, b)` does Unicode simple case-folding *in place*
with zero allocation and is the correct tool.

## A string value is bytes, not guaranteed UTF-8

Go *source* is UTF-8 and string *literals* in your source are therefore valid
UTF-8. But the `string` *type* is just bytes: a value assembled from network
input, a file, or a database column can hold arbitrary, invalid byte sequences.
`utf8.ValidString(s)` reports whether the bytes are well-formed UTF-8. Ranging
over invalid bytes does not error — it silently yields `utf8.RuneError` (U+FFFD,
the replacement rune) for each bad sequence, so a corrupt byte flows downstream
unnoticed. Writing such bytes straight into logs, a JSON response, or a database
is a real hazard: encoding corruption, poison records that break a later reader,
and — when control characters like `\n` are involved — log injection, where an
attacker's `\n INFO fake-entry` forges a log line. Gate untrusted text with
`utf8.ValidString` and normalize it with `strings.ToValidUTF8(s, "�")` (and
strip control characters) before it is persisted or logged.

## The modern splitting and building toolkit

`strings.Cut(s, sep)` returns `(before, after, found)` — the clean, allocation-
lean idiom for "split on the first separator". It replaces `SplitN(s, sep, 2)`
plus manual length and index guards, and it removes a whole family of off-by-one
slice bugs. `strings.CutPrefix(s, prefix)` and `CutSuffix` fold `HasPrefix` and
`TrimPrefix` into one call that returns a `found` bool — exactly what prefix
routing and token schemes like `"Bearer "` want. Trimming a prefix with
`s[len(prefix):]` without first confirming the prefix is present is a panic or a
mis-slice waiting to happen; `CutPrefix` is prefix-safe by construction.

```go
token, ok := strings.CutPrefix(auth, "Bearer ")
// ok == false leaves token == auth, no slice-out-of-range risk
```

`strings.SplitN(s, sep, n)` caps the number of pieces at `n`, which preserves
separators inside the *last* field — the log parser splits with `n=3` precisely
so the message keeps its internal spaces. `strings.Fields` splits on runs of
Unicode whitespace and drops empties; `strings.FieldsFunc(s, pred)` splits on any
rune predicate you supply, the tool for tokenizing a value that may use spaces,
commas, or semicolons interchangeably (OAuth scopes, tag lists).

For assembling output, `strings.Builder` accumulates into one growable buffer and
hands back the final string with no copy via `String()`; `Grow(n)` pre-sizes it so
there are no intermediate reallocations. This turns the classic O(n^2) `+=` loop
— building a bulk `INSERT`, a CSV export, a multi-field body — into a single O(n)
pass, and it is the correct pattern on any hot path. One rule: a `Builder` must
not be copied after its first write; pass `*strings.Builder`, never a `Builder`
by value, or you trigger the runtime's "illegal use of non-zero Builder copied by
value" panic.

## Common Mistakes

### Using len(s) as a character or quota limit

Wrong: enforcing "max 50 characters" or a `varchar(50)` guard with `len(name)`.
`len("café") == 5` but it is four characters, and a CJK or emoji name over-counts
far more, so legitimate input is rejected or truncated mid-rune.

Fix: `utf8.RuneCountInString(name)` when the limit is logically in characters.

### Comparing strings through []byte conversions

Wrong: `bytes.Equal([]byte(a), []byte(b))` — each `[]byte(...)` conversion
allocates and copies the whole string just to compare it.

Fix: `a == b` directly; the compiler emits the optimal length-then-bytes compare.

### Slicing a large string and keeping the small piece

Wrong: `id := body[start:end]` cached or stored long-term. The 20-byte view pins
the entire multi-megabyte backing array; the GC can never reclaim `body`.

Fix: `strings.Clone(body[start:end])` (or `string([]byte(...))`) to copy the
piece into a fresh minimal allocation and drop the reference to the big buffer.

### Quadratic concatenation in a hot path

Wrong: building a bulk `VALUES` clause, a CSV, or a response body with `out += ...`
in a loop — each `+=` allocates a new string and copies everything so far, giving
O(n^2) allocations and heavy GC pressure.

Fix: a `strings.Builder` with `Grow`, written once and read out with `String()`.

### Case-insensitive compare via ToLower

Wrong: `strings.ToLower(a) == strings.ToLower(b)` on a per-request path — two
allocations per call, and subtly wrong for some Unicode case pairs.

Fix: `strings.EqualFold(a, b)` — zero allocation and Unicode-aware.

### Assuming a string value is valid UTF-8

Wrong: trusting that a `string` from the network or a client is well-formed UTF-8
because Go source is UTF-8. Ranging over the bad bytes yields `utf8.RuneError`
silently, and persisting them corrupts records or injects forged log lines.

Fix: gate with `utf8.ValidString` and normalize with `strings.ToValidUTF8`
(plus control-character stripping) before logging or storing.

### SplitN(s, sep, 2) for a first-separator split

Wrong: `parts := strings.SplitN(s, "=", 2)` followed by `len(parts)` and index
checks to find "before" and "after" the first separator.

Fix: `before, after, found := strings.Cut(s, "=")` — one call, no index math.

### Trimming a prefix without checking it is there

Wrong: `token := auth[len("Bearer "):]` after only a `Contains` check, or with no
check at all — panics or mis-slices when the input lacks the scheme.

Fix: `strings.CutPrefix(auth, "Bearer ")` (or `TrimPrefix`), which is prefix-safe
and reports whether the prefix was present.

### Copying a strings.Builder after writing

Wrong: passing a `strings.Builder` by value or storing written builders in a
slice — the runtime panics with "illegal use of non-zero Builder copied by value".

Fix: keep the `Builder` local, or pass a `*strings.Builder`.

Next: [01-log-line-parser.md](01-log-line-parser.md)
