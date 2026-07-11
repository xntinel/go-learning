# The strings Package in Production — Concepts

Nearly every backend request touches untrusted text before any typed value
exists. An `Authorization` header arrives as a raw string; a `Content-Type`
decides whether you decode the body at all; a structured-log line has to be
parsed back into fields; a `.env` file becomes config; a request path becomes a
route; a batch of secret values must be scrubbed from every log line; a header
or method name has to match an allowlist regardless of case; and a variadic
`WHERE id IN (...)` needs a placeholder skeleton that never interpolates user
data. All of this runs on the hot path, and all of it is `strings` work.

That is exactly where naive use of the package turns into a bug that ships. A
case-sensitive scheme check (`== "Bearer"`) rejects RFC-compliant clients that
send `bearer`. `strings.ToLower` used as an identity canonicalizer opens a
spoofing hole across Unicode scripts. Secret redaction built by chaining
`ReplaceAll` lets one replacement clobber another. `strings.Fields` silently
shreds a quoted logfmt value into three tokens. SQL assembled by concatenation
is an injection vector. And building a regexp or `Replacer` inside the function
allocates on every call. This file is the model behind those decisions; each of
the nine exercises that follow is one concrete on-the-job artifact with a real
`*_test.go` pinning its contract.

## Concepts

### Cut is the split you almost always want

`strings.Cut(s, sep)` returns `(before, after string, found bool)` in a single
allocation-free scan: it finds the first `sep`, returns the two halves, and its
`found` bool distinguishes "separator absent" from "separator present but the
second half is empty". That distinction is the whole point. `key=` (present, empty
value) and `key` (absent) are different states, and only the bool tells them
apart. Compare the alternatives: `strings.SplitN(s, sep, 2)` allocates a slice
and forces you to check `len(parts)`; `strings.Index` returns an offset and makes
you slice by hand with the off-by-one risk that implies. For the ubiquitous
"split on the first separator" — scheme from token, `key=value`, mediatype from
parameters, first `=` in a URL that itself contains `=` — `Cut` is the right
primitive.

`CutPrefix` and `CutSuffix` (Go 1.20) are the same idea for affixes:
`after, found := strings.CutPrefix(s, "export ")` replaces the
`HasPrefix`-then-`TrimPrefix` double scan and makes "was the prefix actually
there" an explicit bool instead of an implicit "did the string change".

### EqualFold is for ASCII protocol tokens, not for identity

Protocol keywords defined case-insensitively — the HTTP auth-scheme, the request
method, a header field name — should be compared with `strings.EqualFold`, which
performs Unicode simple case folding in one pass without allocating.
`strings.ToLower(a) == strings.ToLower(b)` allocates two strings and is subtly
different from folding, and a case-sensitive `==` is an interoperability bug.

But `EqualFold` is *simple* (not full) folding, and it is not an identity
oracle. It is not locale-aware: it treats ASCII `I` and `i` as equal
unconditionally, which is wrong under Turkish casing rules where dotted and
dotless i are distinct letters. It folds compatibility characters you did not
expect: the Kelvin sign `K` (U+212A) folds to ASCII `k`. And it never handles
the German eszett as `ss`: `EqualFold("straße", "strasse")` is `false`. So use it
to decide "is this the Bearer scheme" — an ASCII protocol comparison — and never
to decide "are these two usernames the same person". Identity canonicalization
across arbitrary scripts needs `golang.org/x/text/cases` or the PRECIS profiles,
not string folding.

### ToLower is Unicode-aware but not locale-aware

`strings.ToLower` is not byte-based; it maps each code point through the default
Unicode lowercase mapping. That is enough for ASCII and for most single code
points, but it cannot express context or locale rules: Greek final sigma, the
Turkish dotless i, and the German eszett all need rules that a per-rune default
mapping does not carry. `strings.ToLowerSpecial(unicode.TurkishCase, s)` exists
for locale-aware casing, and `golang.org/x/text/cases` is the correct tool when
casing must be right. The operational rule: lowercasing is a display and
loose-matching convenience, never a security-grade canonicalization.

### NewReplacer is a single left-to-right, argument-order pass

`strings.NewReplacer(pairs...)` performs one left-to-right scan that, at each
position, applies the first matching key *in the order the pairs were given* and
then continues *past* the emitted replacement — it never re-scans output it just
produced. Two facts follow. First, when two keys can match at the same position
the earlier-listed one wins, so `NewReplacer` does not automatically prefer the
longest match; if a longer secret must beat a shorter prefix of it, list (or
sort) the longer one first. Second, because output is never revisited, it is
correct for multi-value substitution (redacting a set of secrets, escaping a set
of characters). Chained `strings.ReplaceAll` does the opposite: each call
re-scans the whole string including text an earlier call produced, so mapping
`foo -> bar` and then `bar -> baz` turns your fresh `bar` into `baz`. A
`*Replacer` is also immutable and safe for concurrent use by multiple goroutines:
build it once at constructor scope and share the pointer across every request.

### Fields splits on whitespace and does not understand quotes

`strings.Fields` splits on runs of `unicode.IsSpace`. That is exactly wrong for
any format with quoted whitespace: logfmt's `msg="user logged in"` becomes three
tokens (`msg="user`, `logged`, `in"`), and the same happens to shell-like input.
Knowing *when* `Fields` is insufficient is half the lesson: a real tokenizer for
these formats needs `strings.FieldsFunc` with quote state, or a manual rune scan
that tracks whether it is inside quotes. Reach for `Fields` only when the format
truly has no quoting.

### Builder assembles once, owned by one goroutine

`strings.Builder` amortizes appends into a single growing buffer and, on
`String()`, hands back that buffer with no final copy. `Grow(n)` pre-sizes it so a
known-length assembly does no reallocation at all. It is the correct primitive
for building placeholder skeletons, log lines, and any string assembled from
pieces. Two constraints define its safe use: it is not safe for concurrent use
(one builder per goroutine or request), and it must not be copied after the first
write (the `noCopy` guard). The buffer belongs to one owner.

### Canonicalization is an ordered pipeline pinned by idempotency

A canonical form is the output of a fixed sequence of transformations, and the
order is part of the contract: trim before you lower-case, collapse runs before
you strip disallowed characters, strip before the final trim. Get the order wrong
and you leave stray separators or preserve trailing space. The property that
proves the pipeline is coherent is idempotency: `Normalize(Normalize(x))` must
equal `Normalize(x)` — the canonical form is a fixed point. A fixed-point test is
the cheapest way to catch an ordering bug that a single-pass example test misses.

### Build regexes and Replacers once, at package or constructor scope

A compiled `*regexp.Regexp`, a `*strings.Replacer`, and a `cases.Caser` are all
immutable and reusable. Building them inside the function means allocating and
compiling on every call — on the hot path. Declare them at package scope
(`var re = regexp.MustCompile(...)`) or build them once in a constructor and store
the pointer. Build-once, reuse is the same discipline for all three.

### strings builds skeletons; data flows through the driver

The deepest rule in this lesson: `strings` assembles the *fixed* part of a
statement — SQL placeholders, HTML tags, a query template — while untrusted
*values* travel through the database driver's parameter binding, the HTML
encoder, or the query encoder, never through string concatenation. Conflating
"assemble a string" with "inject a value into a query" is the root cause of SQL
injection and XSS. `InClause` emits `($1,$2,$3)`; the three values are passed as
driver args. The `strings` package must never see the user's value.

## Common Mistakes

### Comparing the auth-scheme case-sensitively

Wrong: `if scheme == "Bearer"`. RFC 9110 defines the auth-scheme as
case-insensitive, so a client sending `bearer abc` is rejected. Fix: compare the
scheme with `strings.EqualFold` while leaving the token bytes untouched.

### Lower-casing the token or the parameter value

Wrong: lower-casing the whole header, which mangles an opaque, case-significant
Bearer token or a case-sensitive parameter value. Fix: only the protocol keyword
is case-insensitive; the credential and most parameter values are returned
verbatim.

### Tokenizing quoted formats with strings.Fields

Wrong: `strings.Fields(logfmtLine)`, which splits inside `msg="user logged in"`.
Fix: use a quote-aware scanner (`FieldsFunc` with state or a manual scan), then
`Cut` each token on `=`.

### Chaining ReplaceAll for multi-value substitution

Wrong: a sequence of `strings.ReplaceAll` calls, where each re-scans the previous
call's output and one replacement clobbers another. Fix: one `strings.NewReplacer`
built once, applied in a single left-to-right pass (order the pairs so a longer
key beats a shorter prefix).

### Treating ToLower or EqualFold as identity canonicalization

Wrong: deduplicating usernames or emails across Unicode scripts with `ToLower` or
`EqualFold` (Turkish i, German ss, Kelvin sign), which enables collisions and
spoofing. Fix: use `golang.org/x/text` / PRECIS for identifiers; keep folding for
ASCII protocol tokens only.

### Splitting key=value with Split instead of Cut

Wrong: `strings.Split(s, "=")`, which shatters a value that itself contains `=`
(`DATABASE_URL=postgres://u:p@h/db?x=1`). Fix: `strings.Cut(s, "=")` splits on the
first `=` and returns the rest intact.

### Building a Replacer, regexp, or Caser per call

Wrong: `regexp.MustCompile(...)` or `strings.NewReplacer(...)` inside the function.
Fix: build once at package or constructor scope and reuse the immutable value.

### Building SQL by concatenating user values

Wrong: assembling `WHERE id IN (` + user values + `)`. Fix: emit placeholders
(`($1,$2,$3)` or `(?,?,?)`) and pass the values as driver args; `strings` builds
only the skeleton.

### Sharing or copying a strings.Builder

Wrong: one `strings.Builder` written from several goroutines, or a `Builder`
copied after its first write. Both are undefined. Fix: one builder per goroutine
or request.

### Relying on Split's zero-value behavior or ignoring Cut's bool

Wrong: assuming `strings.Split("", sep)` returns an empty slice (it returns
`[""]`), or ignoring `Cut`'s `found` bool so `key` and `key=` look identical. Fix:
check the bool and handle the empty-input case explicitly.

Next: [01-username-normalizer.md](01-username-normalizer.md)
