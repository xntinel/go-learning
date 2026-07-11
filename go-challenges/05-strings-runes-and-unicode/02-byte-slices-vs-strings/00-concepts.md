# Byte Slices vs Strings: Zero-Copy, Mutability, and the Wire Boundary — Concepts

The `string` / `[]byte` boundary is where a Go backend spends most of its
allocation budget and where several whole classes of production bugs live. Needless
heap churn on hot request paths (every `string`/`[]byte` conversion copies),
aliasing corruption when unsafe zero-copy is misused, retained-buffer bugs when a
scanner token or an unsafe string outlives its backing array, invalid UTF-8 reaching
a UTF-8-only datastore, and timing side channels when tokens are compared as
strings — all of them trace back to the same handful of facts about how these two
types are laid out and converted. This file is the conceptual foundation; read it
once and the nine independent exercises that follow each become a focused drill on
one operational decision at that boundary.

The senior framing is not "strings are for text and bytes are for binary." It is:
a `string` is an immutable, shareable, read-only handle, and a `[]byte` is a
mutable, poolable, append-friendly buffer. You pick the type that avoids the copy,
keep the mutable buffer at the I/O edge (sockets, files, DB drivers, encoders), and
convert to `string` exactly once at the trust/ownership boundary — while knowing
precisely when an unsafe zero-copy conversion is legitimate and when it is a
foot-gun.

## Concepts

### Two headers, one immutable and one mutable

A `string` is a two-word header: a pointer to backing bytes and a length. A
`[]byte` is a three-word header: pointer, length, and capacity. The extra word on
the slice is the visible sign of the real difference — a slice can grow and be
written through; a string cannot. The Go spec makes string contents immutable: once
a string exists, its bytes never change. That single guarantee is what makes strings
safe to share across goroutines with no lock and no defensive copy, safe to use as
map keys (a map key must not mutate underneath the hash), and safe to pass by value
everywhere. It is also why every mutation-shaped operation — append, in-place
replace, escape-in-buffer — needs `[]byte`. When you catch yourself wanting to
"change a character in a string," that is the type telling you to be on `[]byte`.

### Every conversion copies (and usually allocates)

`[]byte(s)` allocates a fresh slice and copies every byte of `s`; `string(b)`
allocates a fresh string and copies every byte of `b`. The copy is not an
implementation detail you can wish away — it is *required* by the type system,
because the two types disagree about mutability and ownership. If `string(b)` shared
`b`'s storage, a later write to `b` would mutate an "immutable" string. So the
runtime splits ownership by copying. On a cold path this is free enough to ignore.
On a hot request path — a parser that converts every token, a handler that
round-trips a body through both types — that copy is frequently the single dominant
allocation in a CPU profile. The fix is almost never "convert faster"; it is "keep
one type end to end and convert once," or "stay on `[]byte` for the read-only
scan."

### The compiler already elides the copy in specific cases

You do not need `unsafe` to write allocation-free conversion code in the common
shapes, because the compiler is allowed to skip the copy when it can prove the
result cannot outlive or mutate the source. The documented, relied-upon cases:
`string(b)` used *only* as a map key (`m[string(b)]`), used *only* in a comparison
(`string(b) == "GET"`), or used *only* as the range expression
(`for i, r := range string(b)`); and `[]byte(s)` passed straight into an
`io.Writer.Write` or `append`. In each, the temporary string/slice is provably
scoped to that single use, so no heap allocation happens. Knowing this list lets you
write the fast path with plain, safe conversions and reserve `unsafe` for the rare
case none of them cover.

### Unsafe zero-copy: the aliasing contract

When you genuinely must reinterpret one type as the other with no copy,
`unsafe.String(unsafe.SliceData(b), len(b))` produces a string that shares `b`'s
backing array, and `unsafe.Slice(unsafe.StringData(s), len(s))` produces a slice
that shares `s`'s. `unsafe.SliceData(b)` returns the `*byte` at the slice's data
pointer; `unsafe.StringData(s)` returns the `*byte` at the string's. These are zero
allocation because they build a header that points at memory you already have. The
contract is strict and non-negotiable: never mutate the `[]byte` while the aliased
string is alive (you would be mutating an "immutable" string, and any code holding
it — including the runtime's string internals — may misbehave), and never retain
either handle past the lifetime of the backing array. Violate it and you get silent
corruption or a data race that the race detector may not even flag, because from the
runtime's view the string is immutable and no write to it was expected. Use it only
where you can see both ends of the buffer's life in one function.

### The bytes package mirrors the strings package

For read-only parsing of wire data you do not need to convert to `string` at all,
because `bytes` mirrors `strings` function for function: `bytes.Cut`,
`bytes.TrimSpace`, `bytes.EqualFold`, `bytes.ToLower`, `bytes.Index`, `bytes.Split`
have the exact semantics of their `strings` twins but take and return `[]byte`. A
header parser, a protocol scanner, a field splitter can operate directly on the
buffer the socket or file handed you, converting to `string` only for the few
tokens you actually keep. Every conversion you *don't* do is an allocation you don't
pay.

### Retained-slice hazards: Scanner.Bytes and Reader.Peek alias

`bufio.Scanner.Bytes()` and `bufio.Reader.Peek(n)` return slices that alias the
reader's internal buffer, and that memory is only valid until the next `Scan()` or
read. This is a deliberate performance decision — it lets you scan gigabytes without
allocating per line — but it makes retaining a token a bug unless you copy first.
If you store `sc.Bytes()` in a map or a struct field and keep scanning, the next
line overwrites the same array and your stored "token" silently changes to whatever
came later. The copy idiom is `append([]byte(nil), tok...)` (or `bytes.Clone(tok)`),
or convert to `string(tok)` which copies as a side effect. The rule: a token you
keep past the next read must be copied; a token you consume before the next read can
alias for free.

### Builders: strings.Builder vs bytes.Buffer

`strings.Builder` accumulates via `Write`/`WriteString`/`WriteByte` and hands out
the finished string with a final zero-copy `String()` — it can do this safely
because it forbids copying the Builder (a copied Builder panics on next use), so
nothing else can alias its buffer at the moment it "freezes" into a string.
`bytes.Buffer` is the byte-slice twin, but its `Bytes()` method still *aliases* its
internal storage: the returned slice is invalidated by the next write or `Reset()`,
so treat it as a view, not an owned copy. Because a `bytes.Buffer` can be reset and
reused, it pairs naturally with `sync.Pool` to amortize its allocation across many
requests — the pattern behind most high-throughput response builders.

### The Append pattern: formatting into a reused buffer

The `Append` family — `strconv.AppendInt`, `strconv.AppendFloat`,
`strconv.AppendQuote`, `utf8.AppendRune`, and plain `append` — formats a value into
a caller-owned `[]byte` and returns the extended slice, with no intermediate string.
This is the idiom behind zero-allocation encoders: keep one buffer, `buf = buf[:0]`
to reset its length while retaining capacity, append each field, write the whole
line, repeat. Contrast `fmt.Sprintf`, which reflects over its arguments and
allocates a fresh string every call, and string concatenation with `+=`, which
allocates a new string per step. On a high-frequency emit loop (a metrics
line-protocol encoder, a log formatter) the Append pattern is the difference
between zero and thousands of allocations per second.

### UTF-8 correctness is a boundary concern

Go strings are conventionally UTF-8 but the type does not enforce it — a `string`
can hold arbitrary bytes, including ill-formed UTF-8. Many datastores are stricter:
a `utf8mb4` MySQL column, a Postgres `text` column, a Mongo string field will reject
or mangle invalid sequences, and the failure surfaces as an opaque driver error at
write time rather than a clean rejection at ingress. `utf8.Valid(b)` /
`utf8.ValidString(s)` check well-formedness; `bytes.ToValidUTF8(b, repl)` /
`strings.ToValidUTF8(s, repl)` repair by replacing each ill-formed run with a
replacement (commonly `U+FFFD`). The senior move is to validate or repair at the
API boundary so an invalid sequence becomes a 400 you control, never a datastore
error you have to reverse-engineer from a stack trace.

### Comparing secrets must be constant time

Comparing a presented API token or a recomputed HMAC against the expected value
with `==` or `bytes.Equal` short-circuits on the first differing byte, which leaks,
through timing, how many leading bytes were correct — enough for a patient attacker
to recover a secret byte by byte. Security-sensitive comparisons must be
length-independent and non-short-circuiting: `crypto/subtle.ConstantTimeCompare(x,
y)` returns 1 only if the byte slices are equal, in time that does not depend on
where they differ (and returns 0 immediately only for a length mismatch, which is
generally safe to reveal), and `crypto/hmac.Equal` wraps it for MAC tags. These
operate on `[]byte`, which is one more reason the auth layer keeps tokens as bytes.

### Pooling requires Reset and a cap guard

A `sync.Pool` of `*bytes.Buffer` cuts allocation on hot handlers, but two
disciplines are mandatory. First, `Reset()` on borrow or return so a buffer never
leaks one request's bytes into another's response. Second, a capacity guard on
return: a buffer that grew to hold one unusually large payload will, if returned,
pin that peak-sized array in the pool for the life of the process, and under load
the pool fills with such giants and your steady-state memory tracks the worst case,
not the common case. Drop oversized buffers (`if buf.Cap() > limit { return }`
instead of `Put`) so the pool holds only right-sized ones.

### Cost model, one glance

Byte-level scanning and `strconv.AppendX` are cheap. `fmt.Sprintf` reflects and
allocates. `regexp` on a hot path is expensive (compile once, and even a compiled
`Regexp` walks a state machine per call). `string`/`[]byte` conversion is a copy.
The discipline that falls out of this: choose the byte-oriented tool at the I/O
edge where the loop is hot, and reserve the convenient string tool (`fmt`, `regexp`,
free conversion) for cold paths where clarity beats a few nanoseconds.

## Common Mistakes

### Re-implementing CSV/JSON escaping in production

Wrong: hand-rolling CSV or JSON escaping in shipping code. `encoding/csv` and
`encoding/json` already encode the escape rules correctly, including the corner
cases you will forget. Fix: hand-roll only to *learn* the rules (as Exercise 1
does), and ship the standard library.

### Round-tripping through both conversions to mutate

Wrong: `b := []byte(s); b = append(b, x...); s = string(b)` allocates twice — once
per conversion — to accomplish one append. Fix: build with `strings.Builder` when
the result is a string, or `bytes.Buffer` when it is bytes; neither pays the
double conversion.

### Mutating or retaining an unsafe-aliased buffer

Wrong: taking `unsafe.String`/`unsafe.Slice` for zero-copy and then writing to the
`[]byte` or storing either handle past the backing array's life. You corrupt the
"immutable" string or race on shared memory. Fix: use the unsafe conversion only
where the buffer is provably read-only and outlives the derived handle, all visible
in one function.

### Retaining Scanner.Bytes past the next Scan

Wrong: storing `sc.Bytes()` (or `Reader.Peek` results) and continuing to scan;
later reads overwrite the buffer and your stored tokens silently change. Fix: copy
before you keep — `append([]byte(nil), tok...)`, `bytes.Clone(tok)`, or
`string(tok)`.

### Forgetting to Flush a buffered writer

Wrong: buffering the last record in a `bufio.Writer` or a custom CSV writer and
returning without flushing, so the final line never reaches the sink. Fix: `Flush`
at the end of every write batch (and `defer` it where the writer's lifetime is
the function's).

### Comparing tokens with == or bytes.Equal

Wrong: `presented == expected` or `bytes.Equal(tag, want)` for a secret; both
return early on the first mismatch and leak timing. Fix:
`subtle.ConstantTimeCompare` or `hmac.Equal`.

### Letting unvalidated bytes reach a UTF-8-only column

Wrong: forwarding request bytes straight to the datastore and getting a driver
error at write time when the bytes are ill-formed UTF-8. Fix: `utf8.Valid` at
ingress for strict endpoints, `ToValidUTF8` for lossy ingestion, so the boundary
returns a clean 400.

### Indexing a string by byte and assuming one index is one character

Wrong: treating `s[i]` as "the i-th character" — it is the i-th *byte*, and any
multibyte UTF-8 rune spans several bytes, so the assumption corrupts non-ASCII text.
Fix: iterate with `for _, r := range s` for runes, and remember `len(s)` is a byte
count, not a rune count (`utf8.RuneCountInString` gives runes).

### Putting an unbounded buffer back into a pool

Wrong: `Put`-ing a `bytes.Buffer` that grew to megabytes, pinning that peak size
in the pool for the process lifetime. Fix: cap-guard before `Put` and drop
oversized buffers so the pool holds only right-sized ones.

### Building hot-path output with Sprintf or +=

Wrong: `fmt.Sprintf` or `+=` concatenation inside a high-frequency emit loop,
paying reflection and a fresh allocation per record. Fix: the Append pattern into a
reused buffer (`buf = buf[:0]`, then `strconv.AppendInt` and friends).

### Treating bytes.Buffer.Bytes() as an owned copy

Wrong: holding the slice from `buf.Bytes()` and reading it after the next write or
`Reset` — it aliases the buffer's storage and is invalidated. Fix: copy it
(`bytes.Clone`) if you need it to survive the buffer's next mutation.

Next: [01-csv-writer-byte-buffer.md](01-csv-writer-byte-buffer.md)
