# String Formatting with fmt — Concepts

Formatting looks like a beginner topic and is not. It is the exact surface where a
backend service leaks a credential into a log line, corrupts a structured-logging
stream so the parser downstream drops half the fields, silently breaks
`errors.Is`/`errors.As` matching across a repository boundary, or burns measurable
CPU boxing values into `interface{}` on every request. `fmt` is not a printing
convenience; it is a dispatch machine that decides — per verb — which method on
your type to call, how to escape untrusted bytes, and whether an error stays
matchable or collapses to text. This file is the mental model behind ten
independent exercises. Read it once and you have the model for all of them: which
interface each verb dispatches to, where the safety boundary is (never trust input
as a format, never let a type stringify a secret), and what formatting costs.

## Concepts

### Verbs are method dispatch, not string templates

The single most useful thing to internalize is that a verb is a request to call a
particular method, resolved at runtime against the value's dynamic type:

- `%v` is the default rendering. For a type with a `String() string` method it
  calls that; for an `error` it calls `Error()`; otherwise it uses reflection.
- `%+v` is `%v` plus struct field names (`{Name:alice Age:30}` instead of
  `{alice 30}`).
- `%#v` is Go-syntax: `main.Config{Name:"alice", Age:30}`. It invokes
  `GoString()` if the type implements `fmt.GoStringer`.
- `%s` renders as a string: it calls `String()` (Stringer) or `Error()` (error),
  else the raw bytes for `[]byte`/`string`.
- `%q` is a double-quoted, safely-escaped string — quotes, backslashes, and
  control characters are escaped for you. Never hand-roll this.
- `%d` decimal integer, `%x`/`%X` hex (for integers and for `[]byte`), `%f`/`%g`
  floats, `%t` bool, `%p` pointer, `%T` the dynamic type, `%c` a rune.
- `%w` is special: it is only valid in `fmt.Errorf`, and it records an *unwrap
  link* rather than merely formatting.

Knowing which method a verb dispatches to is the whole game. `%v` on a type that
implements `String()` is redaction-safe if that method redacts; `%d` on the same
type ignores `String()` entirely and may leak the raw value. A senior engineer
reads a format string and knows which method fires.

### The three formatting interfaces and their precedence

Three interfaces let a type control its own rendering. Their precedence, highest
first:

1. `fmt.Formatter` — `Format(f fmt.State, verb rune)`. This overrides *everything*.
   The type receives the `fmt.State` (so it can read width, precision, and flags
   and write raw bytes) and the verb rune, and is fully responsible for the
   output. This is how a `Secret` type can guarantee that `%v`, `%s`, and `%q`
   all render `[REDACTED]` no matter what.
2. `fmt.Stringer` — `String() string`. Used for `%v` and `%s` when there is no
   `Formatter`. This is the everyday one: a `Money` or `Duration` renders itself.
3. `fmt.GoStringer` — `GoString() string`. Used only for `%#v`.
4. `error` — `Error() string`. Honored like `Stringer` for `%v`/`%s`.

If a type implements `Formatter`, the other three are bypassed for the verbs it
handles. If it implements both `Stringer` and `error`, `Error()` wins for
`%v`/`%s`. This precedence is why redaction belongs in `Formatter` (total
control) while a domain value like money belongs in `Stringer` (simple, and still
participates in `%v`).

### The Stringer infinite-recursion trap

The most common self-inflicted crash with `fmt`: calling `fmt.Sprintf("%v", x)`
inside `x.String()`. `%v` sees that `x` has a `String()` method and calls it,
which calls `Sprintf("%v", x)` again, forever, until the stack overflows. The fix
is to never format the receiver with a value-dispatching verb inside its own
method: format the underlying fields (`fmt.Sprintf("%d.%02d %s", whole, frac,
cur)`), or convert the receiver to a defined type that does *not* have the method
(`type plain Money; fmt.Sprintf("%v", plain(x))`). The same trap exists for
`Error()` on an error type and for `GoString()`.

### %w records an unwrap link; %v flattens it

`fmt.Errorf("get user: %w", err)` produces an error whose `Unwrap()` returns
`err`, so `errors.Is(result, sentinel)` and `errors.As(result, &target)` walk the
chain and find it through as many layers as you wrap. `fmt.Errorf("get user: %v",
err)` produces an error with the *same text* but no unwrap link: the chain is
flattened into a string, `Unwrap()` returns nil, and `errors.Is` can no longer
match. Choosing `%v` where you meant `%w` is the classic silent bug that breaks
control flow two layers away. Since Go 1.20 a single `Errorf` may carry multiple
`%w` verbs, producing a multi-error that `errors.Is` matches against each wrapped
error, mirroring `errors.Join`.

### Flags, width, and precision grammar

A verb is `%[flags][width][.precision]verb`:

- Flags: `-` left-justify, `+` always show sign (and, for structs under `%v`,
  it is `%+v` that adds field names), `0` zero-pad, space for a leading space on
  positives, `#` alternate form (`0x` prefix for `%#x`, Go syntax for `%#v`).
- Width is the *minimum* field width; the value is padded (with spaces, or zeros
  under `0`) to reach it. Width never truncates.
- Precision (`.N`) limits: digits after the decimal for floats (`%.2f`), maximum
  characters for strings (`%.5s`), minimum digits for integers.
- `%%` emits a literal percent sign — the only way to get a `%` next to a number
  like `%6.2f%%`.

Width and precision compose: `%08.2f` is "at least 8 wide, zero-padded, 2
decimals". Float rounding is IEEE-754 round-to-nearest on the actual stored
value, so `%.2f` of a literal like `2.675` can surprise you (the nearest float is
just below 2.675, so it renders `2.67`). Pin golden tests on values whose float
representation is exact when you can.

### fmt has an allocation cost

Every argument to a `fmt` function is passed as `any`, which boxes it into an
`interface{}`. For a value that does not already live behind a pointer, boxing
forces a heap allocation, and the reflection-driven formatting path is much slower
than a direct `strconv` call. On a cold path this is irrelevant. On a request-hot
logging path it is real CPU and GC pressure. The fix is not to abandon `fmt` but
to reach, in the hot path only, for `strconv.AppendInt`/`AppendQuote` and
`fmt.Appendf` writing into a *reused* `[]byte` (or a pooled `strings.Builder`), so
the line is built with zero per-call allocation. Measure it: `testing.AllocsPerRun`
gives an allocation count and a benchmark gives ns/op. Optimize only what a
measurement proves is hot.

### Format-string injection

If untrusted data reaches the *format* argument — `fmt.Errorf(userInput)` or
`fmt.Sprintf(externalMsg)` — attacker-controlled `%` sequences are interpreted as
verbs. At best this corrupts the output with `%!s(MISSING)` artifacts; a stray
`%` in user text becomes `%!(NOVERB)`; verbs like `%#v` or `%p` can expose
internal representation. The rule is absolute: the format string is always a
constant literal, and the data is always an *argument*: `fmt.Errorf("%s",
userInput)`. `go vet`'s printf analyzer flags non-constant format strings and
verb/argument mismatches; in modern Go (1.24+) `go vet` reports "non-constant
format string in call to fmt.Sprintf", and it runs as part of the gate — which is
exactly why the vulnerable pattern cannot even be committed.

### Alignment for humans needs text/tabwriter

Manually padding columns with `%-20s` breaks the moment a value is wider than your
guess. `text/tabwriter` solves it properly: you write tab-separated cells with
`fmt.Fprintf(w, "...\t...\t...\n", ...)`, and the writer measures the widest cell
in each column across all rows and expands the tabs so columns line up. Two rules
are non-negotiable: you must call `Flush()` (the writer buffers until then, so
forgetting it yields empty output), and text after the final tab on a line is not
a padded cell — to right-align the last column you must end the line with a
trailing tab. Flags like `tabwriter.AlignRight` apply to the whole writer, not one
column.

### Scanning is the fragile inverse of formatting

`fmt.Sscanf(line, format, &a, &b, ...)` parses a fixed-format line back into typed
fields and returns `(n int, err error)` — `n` is how many items were successfully
assigned. It is far more brittle than formatting: `%s` is greedy up to the next
whitespace (so a message field with spaces is truncated), a space in the format
matches a *run* of whitespace, and a missing field yields a short `n` and an
`EOF`/parse error with the earlier fields already assigned. For robust backend
parsing, `strings.Cut` (split on the first separator, keep the remainder intact)
and `strings.Fields` + `strconv` are usually clearer and safer. Reach for `Sscanf`
only for a genuinely fixed, well-controlled format, and always check `n` and
`err`.

### logfmt and structured logging

Structured logging emits machine-parseable `key=value` lines. The discipline:
quote any value that may contain spaces or special characters with `%q` (so the
parser sees one token), render domain types through their `String()`/`Stringer`
so downstream sees stable tokens (a `Duration` as `750ms`, money as `12.34 USD`),
and validate at the boundary — reject an empty level, an empty message, an odd
number of key-value arguments, or a non-string key — so a malformed line never
reaches the consumer. A logfmt encoder is a real production artifact, and it is
the first exercise here.

## Common Mistakes

### Using %v on a []byte and getting a decimal slice

Wrong: `fmt.Sprintf("id=%v", []byte{1, 2, 3})` prints `id=[1 2 3]` — the slice of
integers, useless for a trace ID. Fix: `%x` for hex (`010203`), or hex/base64
encoding for transport. `%s` on `[]byte` gives the raw text, which is right for
UTF-8 payloads but wrong for binary.

### Calling Sprintf("%v", receiver) inside String()/Error()

Wrong: a `String()` that does `return fmt.Sprintf("%v", m)` — infinite recursion,
stack overflow. Fix: format the fields, or convert to a defined type without the
method before formatting.

### Passing user input as the format string

Wrong: `fmt.Errorf(err.Error())` or `fmt.Sprintf(userMsg)` — a `%` in the data
corrupts the output (`%!d(MISSING)`) or exposes internals, and you are ignoring
the `go vet` warning. Fix: `fmt.Errorf("%s", userMsg)`, always a constant format.

### Using %v instead of %w when wrapping

Wrong: `fmt.Errorf("db: %v", err)` — the wrapped error is now unreachable by
`errors.Is`/`errors.As`, silently breaking matching downstream. Fix: `%w` when the
caller may need to match; `%v` only when you deliberately want to seal the chain.

### Reaching for Sprintf on every request in a hot path

Wrong: building each access-log line with `fmt.Sprintf`, paying interface-boxing
and reflection allocations per request. Fix: `strconv.Append*`/`fmt.Appendf` into
a reused buffer; prove the win with `testing.AllocsPerRun` and a benchmark.

### Forgetting Flush on a tabwriter

Wrong: writing rows to a `tabwriter.Writer` and never calling `Flush()` — the
buffer is never emitted, so the output is empty or truncated. Fix: `defer
w.Flush()` (or an explicit `Flush()` before you read the buffer), and end the line
with a trailing tab if the last column must be padded.

### Manually quoting strings

Wrong: `"name=\"" + name + "\""` — misses escaping of embedded quotes,
backslashes, and control characters, producing an unparseable line. Fix: `%q`.

### Confusing %+v with %#v

Wrong: assuming `%+v` prints Go syntax, or that `%#v` shows plain field values.
`%+v` adds field *names* to the default rendering; `%#v` prints Go-syntax and
invokes `GoStringer`. They are different tools for different audiences (an operator
vs. a debugger).

### Trusting Sscanf to parse variable input

Wrong: `fmt.Sscanf` on a line with an optional field or a space-containing message
— it returns a short `n` and a partially filled result, and the missing fields
keep their zero values. Fix: check `n` and `err`, or use `strings.Cut`/`Fields`.

### Logging a secret type with no redaction

Wrong: a credential or PAN stored in a plain `string`/struct that has no
`Formatter`/`Stringer`, so `%v` in any log line or wrapped error prints it in
plaintext. Fix: a dedicated type whose `Format` method always redacts, with an
explicit `Reveal()` escape hatch used only where intended.

Next: [01-logfmt-encoder.md](01-logfmt-encoder.md)
