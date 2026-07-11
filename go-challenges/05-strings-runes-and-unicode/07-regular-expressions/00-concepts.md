# Regular Expressions in Go: Production Text Processing with RE2 — Concepts

In a backend, a regular expression is rarely "match a string." It is a
load-bearing component of log ingestion, request routing, config templating,
alert-rule engines, and PII/secret redaction — and its patterns often arrive
from configuration or from users, i.e. from outside the trust boundary. The
senior question is never "does this pattern match?" but "what does this engine
guarantee under a hostile input, what does it cost to compile and run, and is a
regex even the right tool for this format at all?" Go answers the first question
in a way most languages do not: its `regexp` engine is derived from RE2 and runs
in time linear in the length of the input. Everything else in this lesson — when
to `MustCompile` vs `Compile`, where to compile, how to extract by name, how to
budget an untrusted pattern, when to reach for `net/url` instead — follows from
that one guarantee and its price.

## RE2, not PCRE: the linear-time guarantee

Perl, PCRE, Python's `re`, JavaScript, and Java's `java.util.regex` all use a
backtracking matcher. On a pathological pattern-and-input pair — the classic
`(a+)+$` against a long run of `a` followed by a non-matching byte — a
backtracking engine explores exponentially many ways to partition the input and
can spin for seconds or minutes on a few dozen characters. That is ReDoS
(regular-expression denial of service): one crafted request pins a CPU. Go's
`regexp` cannot do this. It compiles the pattern to an NFA and simulates it in a
single left-to-right pass, so its running time is O(length of input × size of
pattern), with no backtracking and therefore no catastrophic blowup. For a
backend that feeds user- or config-supplied patterns and user-supplied input
into the same matcher, this is the single most important property of the
package. A malicious input cannot make a Go match take super-linear time.

The guarantee is not free, and the price is expressive power. RE2 has **no
backreferences** (`\1`), **no lookahead or lookbehind** (`(?=...)`, `(?<=...)`),
and no `\C`. Those features are precisely what make backtracking necessary, so
an engine that forbids backtracking must forbid them too. This matters the day
you try to port a PCRE or JavaScript pattern: if it uses a backreference or a
lookaround, `regexp.Compile` returns an error, it does not silently
approximate. Knowing this up front tells you when the task is simply outside the
tool — a balanced-delimiter or backreference-driven match needs a real parser,
not a cleverer regex.

## MustCompile vs Compile: a fail-fast decision, not a style choice

`regexp.MustCompile(expr)` returns a `*Regexp` or panics; `regexp.Compile(expr)`
returns `(*Regexp, error)`. The choice between them is a decision about where a
bad pattern should surface.

Use `MustCompile` for a static, literal pattern known valid at build time,
assigned to a package-level `var`. If such a pattern is malformed it is a
programmer bug, and a panic at process startup is the right failure: loud,
immediate, impossible to ignore, and it happens before the process serves a
single request. Use `Compile` for any pattern that comes from configuration, an
HTTP request, a database row, or a rules file — anything not a compile-time
literal. There a bad pattern is *data*, not a bug, and must degrade gracefully:
return an error that names the offending rule, reject the config, answer 400.
`MustCompile` on untrusted input is a latent crash — one malformed alert rule in
a YAML file panics the whole process at boot or, worse, at first use.

## Compile once, match many — and share freely

Compilation parses the pattern and builds the automaton. It allocates and is
comparatively expensive; matching against the compiled `*Regexp` is cheap.
Therefore compile once and reuse. Static patterns become package-level vars;
dynamic patterns are compiled at load time and cached in a map (or behind a
`sync.Once`), never on the hot path. Compiling a regex inside a per-request
handler, a per-row loop, or a per-line scan is a classic latency-and-GC bug: the
automaton is rebuilt on every call and the compile cost dominates the actual
work.

Reuse is safe because a `*Regexp` **is safe for concurrent use by multiple
goroutines**. A single package-level compiled regex is shared across every
handler with no mutex. This is why compile-once is both faster and correct:
there is nothing per-request about a compiled pattern, so nothing needs to be
per-request.

## Submatch mechanics and the number-one production panic

`FindStringSubmatch(s)` returns a `[]string` where index 0 is the whole match
and indices 1..n are the capture groups in left-parenthesis order. On **no
match it returns nil.** The most common regex panic in production Go is indexing
that result without checking: `m := re.FindStringSubmatch(s); id := m[1]` panics
with an index-out-of-range the first time `s` does not match, because `m` is
nil. Always guard — `if m == nil { return ErrNoMatch }` or `if len(m) > 1` —
before touching `m[1]`. The same holds for `FindStringSubmatchIndex`, which
returns a `[]int` of byte offsets (2 per group: start and end), and returns nil
on no match.

Those byte offsets are worth dwelling on: they are **byte** positions, not rune
positions. `FindStringSubmatchIndex` gives you offsets you can slice directly
(`s[loc[2]:loc[3]]`), zero-copy, and precise error locations — but if you treat
them as rune indices on multibyte UTF-8 text you get off-by-many bugs.
`FindReaderSubmatchIndex(r io.RuneReader)` runs the same match over a reader too
large to hold in memory, which is how you grep a gigabyte log without slurping
it.

## Named groups decouple extraction from position

`(?P<name>...)` names a capture group, and `re.SubexpIndex("name")` returns its
index (or -1 if there is no such group). This decouples field extraction from
positional counting: if you later insert a new group in the middle of the
pattern, positional code (`m[3]`) silently reads the wrong field, while
`m[re.SubexpIndex("level")]` keeps working. `SubexpNames()` returns the ordered
names (index 0 is the empty name of the whole match) and is how you turn a match
into a `map[string]string`. Guarding with `if idx := re.SubexpIndex("x"); idx >=
0` also protects you against a group that was renamed out from under the code.

## Match vs Find vs Replace, and the Replace footguns

`MatchString` answers a boolean; `Find*` extracts substrings or indices;
`Replace*` rewrites. Do not answer "does it match, and if so what did it
capture?" with a `MatchString` followed by a second `FindStringSubmatch` — that
scans twice. `FindStringSubmatch` already does both: nil means no match,
non-nil carries the groups.

The replacement family has a sharp edge. `ReplaceAllString(src, repl)` performs
`$1` / `${name}` expansion inside `repl` (the same mechanism as
`Expand`/`ExpandString`), so a literal `$` in replacement *data* must be written
`$$` or it will be misread as a group reference. When the replacement is
dynamic, contextual, or stateful, use `ReplaceAllStringFunc(src, func(match
string) string)`, which hands you each match and lets you compute its
replacement — including recording side effects in a closure (e.g. accumulating
which variables were unresolved). When you are substituting untrusted literal
text and want no `$` interpretation at all, `ReplaceAllLiteralString` disables
expansion entirely.

## QuoteMeta is the regex analog of SQL parameterization

When you interpolate user-provided literal text into a larger pattern —
"match paths beginning with this configured prefix," "route these literal
segments" — wrap the literal in `regexp.QuoteMeta` first. It escapes every
metacharacter so the text matches itself and cannot alter the pattern's
structure. Skipping it is a pattern-injection bug: a user whose "literal" prefix
is `.*` suddenly matches everything, and a user whose prefix is `(a+)+` smuggles
an expensive pattern into your engine. QuoteMeta is to a regex what a bound
parameter is to SQL.

## Anchoring turns search into validation

`^` and `$` (or `\A` and `\z`) are the difference between "contains a match" and
"is entirely this shape." A validator regex *without* anchors matches any
embedded substring: a semver check written as `\d+\.\d+\.\d+` happily accepts
`garbage-1.2.3-more` because the middle of the string matches. This is a real
validation-bypass class of bug. Anchor every validator with `^...$`. Note `$`
matches at end-of-text and, in multiline mode (the `(?m)` flag), also at
end-of-line; `\z` matches only at absolute end-of-text.

## Inspecting a pattern before you trust it: regexp/syntax

RE2 bounds *match* time in the length of the input, but it does not bound the
size or nesting of the *pattern* you hand it, nor is there any per-match timeout.
An attacker who supplies both the pattern and a large input can still burn CPU
and memory proportional to what they sent. `regexp/syntax.Parse(expr, flags)`
lets you parse a pattern into a tree of `*syntax.Regexp` and inspect it —
walking `.Sub` to measure nesting depth, for instance — *before* you compile it,
so you can reject an over-complex or over-long pattern under a budget. Combined
with an input-size cap and `context` cancellation at the call site, this is how
you accept a user-supplied pattern responsibly. The linear-time guarantee
removes ReDoS; it does not remove your responsibility to bound resources.

## Longest vs leftmost-first semantics

By default Go uses leftmost-first (Perl) semantics: for `a|ab` against `"ab"`,
the first alternative that matches at the earliest position wins, so it matches
`"a"`. `re.Longest()` (or compiling with `MustCompilePOSIX`) switches to
leftmost-longest (POSIX) semantics, where the longest overall match wins, so
`a|ab` matches `"ab"`. This changes which alternation branch a tokenizer picks
and is occasionally exactly the knob you need — and occasionally a surprise if
you did not ask for it.

## Regex is the wrong default for structured formats

The recurring senior judgment: reach for a real parser first, and use regex only
for the genuinely irregular slice a parser does not cover. `net/url` for URLs
(percent-encoding, IPv6 brackets, ports, userinfo), `encoding/json` for JSON,
`time.Parse` for timestamps (timezones, layouts), `net/mail` for addresses
(quoted local parts, comments), `strconv` for numbers. A regex "validator" for
any of these is almost always subtly wrong: it misses the cases the format's
grammar allows and the format's real parser already handles. The first exercise
makes this concrete by parsing URLs with `net/url` and confining the regex to a
piece `net/url` does not split.

## Common Mistakes

### Indexing a submatch without checking for nil

Wrong: `m := re.FindStringSubmatch(s); use(m[1])`. On no match `m` is nil and
`m[1]` panics with index-out-of-range — the number-one regex panic in
production. Fix: `if m == nil { return ..., ErrNoMatch }` (or `len(m) > n`)
before any `m[i]`.

### Compiling on the hot path

Wrong: `re := regexp.MustCompile(pat)` inside a handler, a row loop, or a line
scan — the automaton is rebuilt every call and the compile dominates latency.
Fix: compile once into a package-level `var`, or into a cache at load time; the
compiled `*Regexp` is safe to share across goroutines with no mutex.

### MustCompile on untrusted input

Wrong: `regexp.MustCompile(rule.Pattern)` where `rule.Pattern` came from config
or a request — one bad pattern panics the whole process. Fix: `regexp.Compile`
and surface the error naming the offending rule so the config is rejected, not
the process crashed.

### Interpolating user text without QuoteMeta

Wrong: `regexp.Compile("^" + userPrefix + "/.*")`. A `userPrefix` of `.*` or
`(a+)+` changes the pattern's meaning or cost — pattern injection. Fix: wrap the
literal in `regexp.QuoteMeta` so its metacharacters are inert.

### A regex where a real parser belongs

Wrong: hand-rolling a regex to validate or parse URLs, emails, timestamps, or
JSON. It misses percent-encoding, IPv6, quoted local parts, timezones, and
escaping. Fix: `net/url`, `net/mail`, `time.Parse`, `encoding/json`; regex only
for the irregular remainder.

### Forgetting anchors on a validator

Wrong: `regexp.MustCompile(`\d+\.\d+\.\d+`)` to *validate* a version — it
matches an embedded substring, so `x1.2.3y` passes. Fix: anchor with `^...$`
(or `\A...\z`) so the whole string must be the shape.

### Assuming backreferences or lookaround exist

Wrong: porting a PCRE/JS pattern with `\1` or `(?=...)` and swallowing the
`Compile` error. RE2 does not support them, by design. Fix: detect the
unsupported feature, and if the task genuinely needs it, use a parser instead of
a regex.

### Confusing byte offsets with rune positions

Wrong: treating the `[]int` from `FindStringSubmatchIndex` as rune indices and
slicing a `[]rune` or counting characters with them. On multibyte UTF-8 this is
off by many. Fix: they are byte offsets into the original string; slice the
string directly with them.

### Relying on positional submatch indices

Wrong: reading `m[3]` for "the level," then inserting a group earlier and
shifting every field. Fix: `(?P<level>...)` plus `m[re.SubexpIndex("level")]`.

### Treating linear time as total safety

Wrong: matching a package-level regex against unbounded user input and assuming
RE2 makes it safe. Linear in input length still means a multi-megabyte input
costs proportional CPU, and there is no built-in per-match timeout. Fix: cap
input size and cancel via `context` at the call site.

### Unescaped `$` in a replacement string

Wrong: `re.ReplaceAllString(s, userText)` where `userText` may contain `$` — it
is misread as `$1`/`${name}` expansion. Fix: `ReplaceAllLiteralString`, or
escape as `$$`.

Next: [01-url-parts-extractor.md](01-url-parts-extractor.md)
