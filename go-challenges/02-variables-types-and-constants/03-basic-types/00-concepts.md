# Basic Types at Service Boundaries — Concepts

At a service boundary the type you choose is your first and cheapest validation
layer. Before a value reaches domain logic, storage, or a downstream call it is a
string from an env var, a number off the wire, a slice of bytes from a socket, a
field in a decoded request. The senior move is to pick a representation so that
whole classes of bad data are either unrepresentable or fail at parse time — a
status that cannot exceed 599, an amount that cannot lose a cent, a name that
cannot contain invalid UTF-8. Everything downstream then gets to assume the value
is already good, because the boundary refused to build it otherwise. This file is
the model behind that discipline; read it once and each of the independent
exercises that follow is an application of it.

## Concepts

### Types are the cheapest validation layer

Validation you can push into a type is validation you never have to remember to
run. If a wire field is declared `uint16`, a value of 70000 cannot be stored in it
at all — the parse that produced it had to fail or wrap first, and if you parse
with the right width it fails. If money is an `int64` count of cents, a fractional
cent is unrepresentable. If a trace id is `[16]byte`, a 15-byte input cannot fill
it. Choosing the representation is choosing which mistakes become impossible. The
representations that do this well share a property: their set of legal values is a
tight superset of the domain's legal values, so the gap the parser must reject is
small and explicit.

### int versus explicit widths

`int` (machine word, 64-bit on modern servers) is the right default for in-process
counters, loop indices, and slice lengths — values whose range is bounded by
memory, not by a protocol. It is the wrong default at a boundary where the field
has a *declared* range or a *binary layout*: an HTTP status fits in `uint16`, a
length prefix on the wire is a fixed 16 or 32 bits, a protobuf field is `int32`, a
correlation id is a 64-bit unsigned counter. Using the width the protocol actually
specifies makes the range part of the type and makes the binary encoding
unambiguous. Then, having validated the narrow boundary value, convert it to a
richer domain type exactly once: latency milliseconds off the wire become a
`time.Duration`, a status becomes an enum, a byte slice becomes a validated string.

### Integer conversions never panic and never report overflow

This is the single most dangerous fact in this lesson. In Go, `int32(x)`,
`uint16(n)`, `int8(v)` — every numeric conversion — silently wraps using two's
complement. `int32(int64(1) << 31)` is a *negative* number. `uint16(70000)` is
4464. `uint32(-1)` is 4294967295. The language will not complain, `go vet` will
not catch a runtime value, and there is no overflow flag to check afterward. So any
narrowing at a boundary — int64 to int32, int to uint16, a signed value into an
unsigned field — must be preceded by an explicit range check against the target
type's `math.Max*`/`math.Min*` constant, and must return an error when the value
does not fit. The plain conversion is a bug generator; the guarded conversion is
the boundary doing its job.

### Signed, unsigned, and two's-complement wrap

Converting a negative signed value to an unsigned type does not clamp to zero — it
reinterprets the bit pattern, yielding a huge positive number. This is the source
of the classic production incidents: a length computed as a difference goes
negative, gets stored in a `uint32` Content-Length, and the server promises to send
four gigabytes; an id arithmetic underflows and a "next id" becomes astronomically
large. Whenever a value that could be negative is about to become unsigned, the
sign check is not optional.

### Strings are immutable byte sequences, not character arrays

A Go `string` is a read-only slice of bytes with no character semantics baked in.
`len(s)` is the byte count. `s[i]` is the i-th *byte*. Ranging with
`for _, r := range s` is the only one of the three that decodes UTF-8 and yields
`rune` values (Unicode code points). The consequence for boundaries: `byte` is for
raw wire data (encode/decode/hash/length), and `rune` is for validated human text
(counting characters, enforcing a display-name limit, checking for control
characters). Content-Length and a database's byte-sized column use *bytes*; a
"max 30 characters" product rule uses *runes*. Conflating them produces off-by-some
column limits and truncation bugs on every non-ASCII input.

### Invalid UTF-8 is real input

Nothing guarantees an inbound string is valid UTF-8 — a malformed client, a
mangled proxy, a byte slice reinterpreted as text all deliver invalid sequences.
`utf8.ValidString` is the gate; run it before storing or re-encoding untrusted
text. Truncation is the subtler trap: slicing a string by byte index to fit a
limit can cut through the middle of a multi-byte rune, leaving a dangling partial
code point that renders as the replacement character U+FFFD (`utf8.RuneError`) or
corrupts the field. Truncation that must stay correct counts and cuts on *rune*
boundaries, and its output should itself pass `utf8.ValidString`.

### string([]byte) and []byte(string) copy

Both conversions allocate and copy, because a `string` is immutable and a `[]byte`
is mutable — they cannot share backing memory safely in general. On a hot wire path
that copy is real cost paid per request. Much of what looks like it needs a string
does not: hashing, scanning, length, and many comparisons operate directly on the
`[]byte`, and the standard library is full of paired APIs (`hash.Hash.Write([]byte)`,
`bytes` mirroring `strings`) precisely so you can stay on one side. Convert once, at
the point where the other representation is genuinely required, not reflexively.

### float64 is for measurement, not exact quantities

Binary floating point cannot represent most decimal fractions exactly — `0.1` is
already an approximation, so `0.1 + 0.2` is `0.30000000000000004`, and accumulation
drifts. `float64` is the right type for a sample rate, a ratio, a measured latency,
a physical quantity where a rounding error in the fifteenth digit is noise. It is
the wrong type for money, for counters, for ids, for anything whose exactness is
the point. Money is `int64` minor units (cents), counters are integers, large ids
are integers or byte/string encodings — because beyond 2^53 a `float64` cannot even
represent consecutive integers, so a 64-bit id silently loses its low bits.

### Floating-point hazards at the boundary

Three specific `float64` hazards must be guarded where measurements enter an
aggregate. First, `NaN` (from `0.0/0.0`, `sqrt(-1)`, a bad parse) poisons anything
it touches: `NaN + x` is `NaN`, so one bad sample turns a whole mean into `NaN`, and
because `NaN != NaN` it also breaks sorting and equality. Second, `±Inf` from
overflow behaves similarly and must be detected with `math.IsInf`. Third, `==` on
computed floats is almost always wrong — two mathematically equal computations can
differ in the last bit — so compare within an epsilon tolerance (`math.Abs(a-b) <
eps`) or against an explicit ulp, and reject/skip `NaN`/`Inf` before they enter.

### strconv carries structured errors

The `strconv.Parse*` functions do not just say "bad" — they return a
`*strconv.NumError` wrapping one of two sentinels: `strconv.ErrSyntax` (the text is
not a number) or `strconv.ErrRange` (the number is out of range for the requested
`bitSize`). Two things follow. First, always pass the real `bitSize`
(`ParseUint(s, 10, 16)` for a `uint16` field) so a value that overflows the field is
an `ErrRange` error instead of a silently-accepted wider value. Second, on
`ErrRange` the function *also returns the max-magnitude value for that bitSize* —
so if you ignore the error you accept a clamped `65535` or `MaxInt64` as if it were
the input. Classify with `errors.Is(err, strconv.ErrRange)` versus
`errors.Is(err, strconv.ErrSyntax)` and reject; never read the returned number on a
range error.

### The bool zero value is a trap

A `bool` field's zero value is `false`, which is indistinguishable from "never set".
When "unset" must differ from "explicitly false" — a feature flag override, a PATCH
that omits a field versus sets it false, an optional config knob — a plain `bool`
cannot carry the distinction, and an unset value silently reads as `false` and
clobbers real configuration. The fix is a three-state representation: `*bool` (nil =
unset) or a typed tri-state (`Unset`, `True`, `False`). Precedence resolution
(request override > tenant config > default) is then correct: an unset override
leaves the lower layer intact instead of forcing it false.

### time.Duration is an int64 nanosecond count

`time.Duration` is `int64` nanoseconds, so operational settings parse cleanly with
`time.ParseDuration` (`"250ms"`, `"2s"`, `"1m"`). Two boundary concerns. First, a
zero `Duration` is ambiguous and often dangerous: for many APIs (`http.Server`
timeouts, a context with a zero deadline in some wrappers) zero means "no timeout /
infinite", so treating an unset value as a benign default produces hung requests —
distinguish "unset" from an intended default explicitly. Second, exponential backoff
computed as `base << n` or `base * 2^n` overflows `int64` for large `n` and wraps
*negative*; cap the shift and clamp to a max before it can go negative.

### Decide representation once, at the edge

The unifying rule: the boundary is the single place that converts wire
representation to domain representation and the single place that surfaces every
format, range, and encoding failure as an *error* — not as a truncated, clamped,
wrapped, or NaN-poisoned value that propagates ambiguity inward. A value that has
passed the boundary is, by construction, a valid domain value. Everything above
depends on getting that one conversion right and loud.

## Common Mistakes

### Using int for every numeric field at a boundary

Wrong: parse every wire number into `int` because it is convenient. You lose the
range and binary-layout guarantees the protocol actually has, and you push the
range check downstream where it is usually forgotten.

Fix: use the width the protocol specifies (`uint16` status, `int32` field, 32-bit
length prefix), validate against it at parse time, then convert to a richer domain
type once.

### Narrowing with a plain conversion

Wrong: `int32(x)` or `uint16(n)` on a boundary value, assuming the compiler will
complain on overflow. It silently wraps — large positives go negative, negatives go
huge-unsigned.

Fix: range-check against `math.MaxInt32`/`math.MinInt32`/`math.MaxUint16` first and
return an error when the value does not fit; only then convert.

### Treating len(s) as a character count

Wrong: enforce a "30 characters" limit with `len(s) <= 30`. For any multi-byte
input the byte count and rune count differ, so the limit is wrong in both directions.

Fix: count runes with `utf8.RuneCountInString` for human-facing limits; reserve
`len` for byte-sized concerns (Content-Length, byte columns).

### Truncating a string by byte index

Wrong: `s[:limit]` to fit a length cap. If `limit` lands inside a multi-byte rune
you emit a corrupt partial code point that renders as U+FFFD.

Fix: truncate on rune boundaries (count runes, cut at the byte offset of the Nth
rune) and confirm the result still passes `utf8.ValidString`.

### Trusting inbound strings are valid UTF-8

Wrong: store or re-encode an untrusted string without checking. Invalid bytes then
propagate into logs, databases, and downstream services.

Fix: gate untrusted text with `utf8.ValidString` at the boundary and reject invalid
input.

### Storing money, counters, or ids in float64

Wrong: dollars as `float64`, a large id as `float64`. Sums drift
(`0.1 + 0.2 != 0.3`) and ids beyond 2^53 lose their low bits.

Fix: `int64` minor units for money, integers for counters, integer or byte/string
encodings for ids.

### Comparing computed floats with == and ignoring NaN

Wrong: `ratio == threshold` on computed values, and feeding a `NaN`/`Inf` sample
straight into a running mean.

Fix: compare within an epsilon (`math.Abs(a-b) < eps`), and reject or skip
`math.IsNaN`/`math.IsInf` samples before they poison the aggregate.

### Ignoring strconv range errors

Wrong: read a `*strconv.NumError` as merely "invalid", or omit `bitSize` so a
32-bit field accepts a 64-bit number, or accept the clamped max-magnitude value the
function returns on `ErrRange`.

Fix: pass the field's real `bitSize` and classify with
`errors.Is(err, strconv.ErrRange)` versus `strconv.ErrSyntax`; never use the
returned number on a range error.

### Modeling optional booleans as a plain bool

Wrong: an override flag as `bool`, so "unset" and "false" are the same value and an
absent override clobbers a tenant's real setting.

Fix: use `*bool` or a typed tri-state so unset, true, and false are three distinct
states, and resolve precedence so unset defers to the lower layer.

### Copying at hot wire boundaries

Wrong: `string(body)` or `[]byte(s)` on every request when the operation (hash,
length, scan) works on the bytes directly.

Fix: stay on `[]byte` through hashing/scanning and convert only at the point a
`string` is genuinely required.

### Treating a zero Duration as a safe default

Wrong: accept an unset `time.Duration` as a sensible default when the API in
question reads zero as "no timeout / infinite", producing hung requests.

Fix: distinguish "unset" from an intended default explicitly, and map an empty
config value to a documented positive default rather than to zero.

Next: [01-telemetry-event-boundary-types.md](01-telemetry-event-boundary-types.md)
