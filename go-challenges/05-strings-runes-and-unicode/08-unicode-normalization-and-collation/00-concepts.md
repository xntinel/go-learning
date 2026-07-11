# Unicode Normalization, Case Folding, and Collation — Concepts

A user types "café" into a search box. Your database has a row that was inserted
as "café". The search returns nothing. Both strings render identically on every
screen in the world, yet one is four code points (`c a f é`, the `é` being the
precomposed U+00E9) and the other is five (`c a f e` plus a combining acute
U+0301), so their bytes differ and a byte-level `=` misses. This is not an exotic
corner case; it is the default behavior of every text field you accept from a
browser, a mobile keyboard, or a paste buffer, because different input methods
emit different-but-equivalent encodings of the same visible text.

Unicode text is a boundary-and-invariant problem, not a string trick. A senior
backend engineer owns three surfaces where it bites: the write path (where a
canonical on-disk form lets DB unique constraints, idempotency keys, and cache
keys rely on plain byte equality), identity (where a username or password
normalization bug is an account-takeover or duplicate-account incident), and the
read path (where accent- and case-insensitive search keys and locale-correct
collation decide whether users find their data and whether a listing looks sorted
or broken). The recurring judgment is always the same two questions: which
equivalence am I enforcing — canonical, compatibility, case, or locale — and at
which layer do I enforce it. This file is the model; the ten independent modules
that follow each build one production-shaped artifact against it.

## Concepts

### Byte equality is not user-perceived equality

The Unicode standard defines several distinct notions of "the same string", and a
search, identity, or storage system must consciously pick one. *Canonical
equivalence* says two sequences that are indistinguishable in rendering and
meaning (precomposed `é` vs `e` + combining acute) are the same. *Compatibility
equivalence* is looser: it also equates things that look different but share a
meaning — the ligature `ﬁ` with `fi`, a full-width `Ａ` with `A`, a superscript
`²` with `2`. *Case equivalence* equates letters that differ only in case.
*Locale equivalence* is what a human from a given language community would call
"the same for sorting or matching". These are nested and different, and shipping
the wrong one is a correctness bug that byte-level tools cannot see.

### The four normalization forms

Canonical and compatibility equivalence are each made operational by a pair of
*normalization forms* — a deterministic function that maps every string in an
equivalence class to one representative.

- **NFC** (Canonical Composition) composes base + combining marks into a single
  precomposed code point where one exists: `e` + U+0301 becomes U+00E9. This is
  the web and storage default; browsers overwhelmingly emit NFC, and it is the
  form you should store.
- **NFD** (Canonical Decomposition) does the reverse: it splits precomposed
  characters into a base plus ordered combining marks. It is the intermediate
  form you pass through to *manipulate* accents (strip them, count base letters).
- **NFKC / NFKD** (Compatibility) additionally fold compatibility characters:
  ligatures split, full-width to half-width, superscripts flattened. This is
  *lossy* — it destroys distinctions the user may have intended — so it is for
  matching only, never for round-trip storage.

The core discipline: **store NFC, match with a fold derived from NFD, never store
a compatibility form.**

### Where to normalize: once, at the trust boundary

The single most important architectural decision is *where* normalization
happens. The answer is: canonicalize to NFC exactly once, at the trust boundary —
the HTTP handler that accepts the field or the repository method that writes it —
and nowhere else. If every inbound string is NFC before it reaches the rest of the
system, the rest of the system can use plain `==`, plain map keys, plain database
unique constraints, and be correct. The opposite pattern — normalizing at every
comparison site — is both slower (you pay the transform on the hot path) and
fragile (miss one comparison and you have a phantom cache miss or a duplicate
account). "Normalize on input, compare with bytes" beats "store raw, normalize on
every read".

### The canonical accent-fold recipe

To build a search key that ignores accents you need to *remove* combining marks,
and to remove them you must first make them separate code points. The correct,
script-agnostic recipe is a three-stage transformer chain:

```
transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
```

Decompose to NFD so every accent becomes a standalone combining mark; remove
every rune in general category **Mn** (nonspacing mark); recompose to NFC so the
surviving base letters are back in canonical form. This closes the blind spot of a
manual `U+0300..U+036F` range check, which only removes marks that were *already*
separate and only in the Latin block. A precomposed NFC `é` (U+00E9) has no
combining mark to strip, so the manual path silently leaves the accent on; running
NFD first is what makes both NFC and NFD inputs collapse to the same key, for any
script rather than just Latin.

### Case folding is not lowercasing

`strings.ToLower` and `cases.Lower(tag)` do *lowercasing*, which is
locale-sensitive by design: Turkish maps capital dotless `I` to dotless `ı`,
German expands `ß`, Greek lowercases a word-final sigma differently. That
sensitivity is correct for display but wrong for an identifier key, where you want
one answer regardless of the server's or user's locale. **Case folding**
(`cases.Fold`) is the locale-independent operation built for caseless matching: it
maps `STRAßE` and `strasse` to the same thing (`ß` folds to `ss`) so they collide
as the same identifier. Use `Fold` for identifiers and dedup keys; use
`Lower(tag)`/`Upper(tag)` only for locale-correct display.

### PRECIS: the modern framework for internationalized identifiers

Rolling your own username and password validation misses bidi-override spoofing,
zero-width joiners, and confusable control characters. **PRECIS** (RFC 8264, with
the username/password profiles in RFC 8265 and nicknames in RFC 8266) is the
versioned framework that replaces the older ad-hoc Nameprep/Stringprep. It defines
an `IdentifierClass` (restrictive: letters, digits, a small safe set) and a
`FreeformClass` (permissive, for passwords and display names), disallows dangerous
code points by construction, and specifies enforcement — width mapping, case
mapping, normalization to NFC, and validation — as a single ordered pipeline. In
Go, `precis.UsernameCaseMapped` canonicalizes and case-folds a username,
`precis.OpaqueString` validates a password while preserving it, and
`Profile.CompareKey`/`Profile.Compare` give you a storable key and a direct
equality check. Because it is versioned, its behavior is auditable and stable.

### Collation is ordering, a separate problem

Normalization and folding answer "are these the same?". *Collation* answers "which
comes first?", and it is a genuinely different, inherently locale-dependent
algorithm. Swedish sorts `å ä ö` *after* `z`; German phonebook order differs from
dictionary order; Spanish once treated `ch` as one letter. Collation is
*multi-level*: the primary level compares base letters, the secondary level breaks
ties on accents, the tertiary level breaks ties on case. `collate.New(tag,
opts...)` builds a locale `Collator`; options select which levels matter —
`IgnoreCase` drops the tertiary level, `IgnoreDiacritics` drops the secondary,
`Numeric` makes embedded numbers sort as values (so `item2` < `item12`), `Loose`
is a convenience for case- and accent-insensitive matching. Sorting
internationalized strings with `sort.Strings` (byte order) mis-sorts every
accented and non-ASCII name and every mixed-case list.

### Collation sort keys, and why they drift

A `Collator` can turn a string into an opaque *sort key* — a byte slice whose
plain `bytes.Compare` order equals the collation order — via
`Collator.Key`/`KeyFromString`. This lets you precompute one `bytea` column and
have the database `ORDER BY` it directly, getting locale-correct order without
re-running the collator per query. The catch: a sort key encodes the CLDR/locale
version baked into the `x/text` release that produced it. A library upgrade can
change the key bytes, so a precomputed key column must be regenerated on upgrade or
ordering silently drifts out of sync with freshly computed keys.

### The transform.Transformer model and streaming

Normalization, case folding, and mark removal are all `transform.Transformer`
values, which is why they compose with `transform.Chain` and why they can run
*streaming*: `norm.NFC.Reader(r)` / `transform.NewReader(r, t)` wrap a source
`io.Reader` and normalize on the fly, and the `Writer` variants do the same on the
write side, all in constant memory regardless of input size. Transformers are
*stateful* (they hold partial multi-byte sequences across buffer boundaries), so
they are not safe for concurrent use; `transform.String`/`Bytes` call `Reset`
before running, which is why a single package-level chain can be reused
sequentially across calls but must not be shared across goroutines.

### The fast path: skip work when the input is already normal

The vast majority of production traffic is already NFC, so paying a full transform
(and its allocation) on every request is waste. `norm.Form.IsNormalString(s)`
reports whether `s` is already in the target form, and `QuickSpanString(s)` returns
the length of the already-normal prefix — either lets a hot path short-circuit and
return the input unchanged with zero allocation when there is nothing to do.

### Nonspacing marks: unicode.Mn, not a Latin range

The correct predicate for "this rune is a combining accent" is Unicode general
category **Mn** (Mark, nonspacing), exposed as the `*unicode.RangeTable`
`unicode.Mn` and used via `unicode.Is(unicode.Mn, r)` or `runes.In(unicode.Mn)`.
It covers combining marks in every script; the literal range `U+0300..U+036F` is
only the Latin combining block and misses Greek, Cyrillic, Hebrew, Arabic,
Devanagari, and more. Use the category, not the range.

### stdlib versus golang.org/x/text

`golang.org/x/text` is not the standard library, but it is the Go team's canonical
implementation of the Unicode algorithms (norm, cases, collate, precis, transform),
tracking the CLDR. The standard senior trade-off: ship the zero-dependency stdlib
approximation only when you *know* the input is Latin/ASCII and already NFD, and
reach for `x/text` the moment correctness beyond that matters — with streaming
transforms and fast-path checks so correctness does not cost throughput. Modules 01
and 02 build and then diagnose the stdlib-only baseline precisely so that modules
03 onward can show what adopting `x/text` buys.

## Common Mistakes

### Using strings.ToLower as a search or identity key

`strings.ToLower` is locale-blind: `STRAßE` stays `straße` (not `strasse`) and a
Turkish user's `ı`/`I` distinction is lost, so users either collide who should not
or fail to match who should. Use `cases.Fold` for identifiers and dedup keys.

### Assuming a manual U+0300..U+036F strip decomposes accents

That range check only removes marks that are *already* separate (NFD input). A
precomposed NFC `café` (U+00E9) has no combining mark in that range, so the accent
silently survives and NFC and NFD inputs produce different keys. Run `norm.NFD`
first (the NFD to remove-Mn to NFC chain), and use the `unicode.Mn` category
rather than the Latin-only literal range.

### Storing NFD (or mixed) and querying NFC, or the reverse

DB unique constraints, primary keys, and equality joins compare bytes. Without a
single canonical form imposed at the write boundary you get duplicate accounts,
phantom cache misses, and joins that fail on visually identical keys.

### Using NFKC/NFKD for storage

Compatibility normalization is lossy — `ﬁ` becomes `fi`, full-width becomes
half-width, superscripts collapse. It is a matching aid; storing the folded form
destroys the user's original text. Store NFC; use compatibility folds only to
build a throwaway match key.

### Rolling your own username/password validation

Ad-hoc allow/deny rules miss bidi-override spoofing, zero-width joiners, and
confusable control characters that RFC 8265 profiles reject by construction. Use
`precis.UsernameCaseMapped` / `precis.OpaqueString`.

### Sorting internationalized strings with sort.Strings

Byte order mis-sorts every accented and non-ASCII name, and interleaves upper- and
lowercase wrongly. Use a `collate.Collator` for the target locale.

### Sharing a Collator or its Buffer across goroutines

A `*Collator` and its `Buffer` are stateful and not safe for concurrent use. Build
one Collator per goroutine, or guard a shared one with a mutex.

### Treating collation sort keys as stable forever

Sort keys encode the CLDR/locale version; a Go or `x/text` upgrade can change them,
so a precomputed key column must be regenerated on upgrade or the stored order
drifts away from freshly computed order.

### Buffering an entire upload to normalize it

`io.ReadAll` then `norm.NFC.String` defeats streaming and blows up memory on large
inputs. Use `norm.NFC.Reader` / `transform.NewReader` so memory stays constant and
combining sequences split across buffer boundaries are handled correctly.

### Normalizing on every comparison instead of once at the boundary

You pay the allocation on the hot path when `IsNormalString`/`QuickSpanString`
could short-circuit the common already-NFC case. Normalize at the boundary; check
the fast path where you cannot.

### Passing language.Und to cases.Lower and expecting Turkish or German behavior

Locale-specific folding needs the specific `language.Tag` (`language.Turkish`,
`language.German`). `Und` (or the zero `Tag`) gives the default root behavior, not
the locale rule you wanted.

Next: [01-stdlib-only-normalizer.md](01-stdlib-only-normalizer.md)
