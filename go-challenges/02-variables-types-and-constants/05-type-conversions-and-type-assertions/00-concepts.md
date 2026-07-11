# Type Conversions and Type Assertions at Production Boundaries — Concepts

Conversions and assertions are almost never interesting in the middle of a
program. They are decisive at the *edges* — the places where untyped or
externally-typed data crosses into your typed core. A JSON webhook body decoded
into `map[string]any`. A SQL driver value arriving as `any` in `Scan`. An error
value whose concrete type decides retry versus fail. A numeric width that must
fit a downstream `int32` column, a gRPC field, or a protobuf enum. Inside those
boundaries a senior engineer treats every conversion as a *claim about
representability* — does the value fit, is precision preserved — and every
assertion as a *claim about a runtime contract* — does this interface actually
hold that concrete type or capability. The recurring production failures all
come from claims made without checking them: panics from single-value assertions
on untrusted input, silent integer truncation on narrowing, `float64` precision
loss on JSON numbers used as money or IDs, UTF-8 corruption when truncating by
bytes, and unsafe zero-copy tricks that alias mutable memory. This file is the
model; the nine independent exercises that follow each turn one of these into
controlled failure instead of the 3am kind.

## Concepts

### Conversion is a compile-time reshaping of a concrete value

`T(v)` converts a concrete value to type `T` when Go's conversion rules permit
it. It is checked at compile time, it never parses text, and it never inspects
the contents of an interface. `int(x)`, `float64(n)`, `[]byte(s)`, `string(r)`
are all conversions. Crucially, numeric conversions are not lossless: narrowing
(`int64` to `int32`, `int` to `uint16`) silently truncates or wraps around, and
`float64(int64Value)` or `int(floatValue)` loses precision or the fractional
part. So a conversion is really an assertion *you* are responsible for: that the
source value is representable in the target type. Go will not stop you from
turning `math.MaxInt32 + 1` into a negative `int32`; the range check is your job,
not the compiler's.

### Assertion is a runtime inspection of an interface's dynamic type

`v.(T)` asks, at runtime, whether the interface value `v` currently holds a value
of type `T`, and if so extracts it. The single-value form `x := v.(T)` panics on
mismatch. The comma-ok form `x, ok := v.(T)` reports the mismatch as a boolean
and never panics. The rule that keeps services up: any value you did not
construct yourself — a field pulled from decoded JSON, a row from a driver, a
handler from a plugin registry — must be asserted with comma-ok, and a failed
assertion must become a named error, not a panic. The single-value form is only
appropriate when a preceding line guarantees the type (you just put it there).

### You can assert to an interface, not only a concrete type

`v.(io.Closer)`, `v.(http.Flusher)`, `v.(encoding.TextMarshaler)` all succeed
when the dynamic value *implements* that interface, regardless of its concrete
type. This is the optional-capability pattern and it is how the standard library
composes behavior without a combinatorial explosion of interfaces: an
`http.ResponseWriter` might also be an `http.Flusher`; a value handed to a
serializer might also be a `fmt.Stringer`. You probe for the capability, use it
when present, and fall back when absent. Assertion-to-interface is the mechanism
that makes "optional method" work in a statically typed language.

### A type switch is idiomatic multi-way dispatch over dynamic types

`switch x := v.(type) { case string: ...; case int64: ... }` binds `x` to the
matched concrete (or interface) type in each case. This is exactly how
`database/sql` drivers hand values to `Scan`, how `slog.Value` renders an
attribute, and how `encoding/json` walks an `any`. Case order matters: an
interface case (like `fmt.Stringer` or `error`) matches any value implementing
it, so a concrete type that also implements that interface must come *first* or
it will be captured by the broader case. A `time.Time` is a `fmt.Stringer`, so
`case time.Time` must precede `case fmt.Stringer`.

### errors.As is a semantic type assertion for error chains

A plain type switch on an error does not unwrap. `errors.As(err, &target)` walks
the `Unwrap()` chain and assigns the first error that matches `target`'s concrete
or interface type — this is how you make retry/fail decisions without brittle
`strings.Contains(err.Error(), "timeout")` matching that breaks the moment a
library rewords a message or a caller wraps the error one level deeper.
`errors.Is(err, sentinel)` is the sibling for comparing against sentinel values
like `context.DeadlineExceeded`. Between them they replace every fragile
string-based error classification.

### JSON numbers decoded into `any` are `float64` by default

`encoding/json` decodes every JSON number into `float64` when the destination is
`any`. `float64` has 52 bits of mantissa, so it represents integers exactly only
up to `2^53`; a 64-bit ID, a Snowflake, or an exact monetary amount above that
silently rounds. `json.Decoder.UseNumber` changes the policy: numbers arrive as
`json.Number`, which is a `string` you parse yourself with `ParseInt`,
`ParseFloat`, or `big.Rat` under whatever precision rule the field demands. The
default is a convenience; at a money or identity boundary it is a bug waiting for
a large input.

### Strings are immutable UTF-8 bytes; index-slicing can split a rune

A Go string is an immutable sequence of bytes that is conventionally UTF-8.
`s[i]` and `s[:n]` index *bytes*, not runes, so `s[:max]` to fit a fixed-width
column can cut a multi-byte code point in half and produce invalid UTF-8 that a
database may reject or store corrupted. Length-bounding for storage or logging
must be rune-aware, backing off to a code-point boundary with `unicode/utf8`.
Conversions between the forms cost real work: `[]rune(s)` and `string([]rune)`
allocate and re-encode; `[]byte(s)` and `string([]byte)` copy the bytes.

### unsafe.String / unsafe.Slice give zero-copy conversion — and an alias

`string(b)` copies the bytes so the resulting string is independent of the slice.
`unsafe.String(unsafe.SliceData(b), len(b))` instead reinterprets the same
backing array as a string with no allocation — and therefore creates an *alias*.
Mutating `b` afterward mutates the string, which the language defines as
undefined behavior because strings are supposed to be immutable. The trick is
legitimate only when the source bytes are provably immutable for the string's
whole lifetime: a hot-path cache key built from a buffer you never write again, a
read-only mapped region. Everywhere else it is a heisenbug generator.

### database/sql hands you a small, fixed set of concrete types

A driver delivers column values to `Scan(src any)` as one of `int64`, `float64`,
`bool`, `[]byte`, `string`, `time.Time`, or `nil`. Implementing `sql.Scanner`
and `driver.Valuer` for a custom column type is therefore a mandatory type-switch
boundary: your `Scan` must handle each concrete form the driver may send
(including `nil` for a NULL column) and convert it into your type, and your
`Value` must return one of the canonical `driver.Value` types.

### int and uint are platform-dependent widths

`int` and `uint` are 32-bit on some build targets and 64-bit on others. Code that
narrows `int64` to `int` without a range check compiles and works on `amd64` /
`arm64`, then silently overflows when someone builds it for a 32-bit target. The
correct guard compares against the platform's real bound — `int(^uint(0) >> 1)`
is the maximum `int` on whatever platform you are compiling for — or against the
explicit `math.MaxInt32` / `math.MinInt32` bounds of the concrete target width.

## Common Mistakes

### Single-value assertion on untrusted input

Wrong: `id := payload["id"].(string)`. If the field is missing or a number, this
panics and takes down the request (or the process). Fix: `id, ok :=
payload["id"].(string)`; on `!ok` return an error that names the field, so the
boundary rejects the input instead of crashing.

### Asserting a JSON number as `int`

Wrong: `payload["attempts"].(int)`. A number decoded into `any` is `float64` by
default or `json.Number` under `UseNumber` — never `int` — so the assertion
always fails. Fix: assert `json.Number` (with `UseNumber`) and `ParseInt`, or
accept `float64` and range-check before converting.

### Narrowing without a range check

Wrong: `int32(counter)` where `counter` is an aggregated `int64`. `MaxInt32 + 1`
becomes a negative number and corrupts the value written downstream. Fix: compare
against `math.MinInt32`/`math.MaxInt32` and return an error when the value does
not fit, before the conversion.

### float64 for money or large IDs from JSON

Wrong: decoding a `price` or a 64-bit `id` into `any` and reading it as
`float64`. Above `2^53` the integer is wrong and decimals round. Fix:
`UseNumber` plus `ParseInt`, or a decimal / `big.Rat` policy for money.

### Truncating a string with `s[:max]`

Wrong: `s[:64]` to fit a `varchar(64)`. It can split a multi-byte rune and emit
invalid UTF-8. Fix: back off to a rune boundary with `unicode/utf8` so the result
is always valid.

### Mutating the backing bytes after unsafe.String

Wrong: `s := unsafe.String(unsafe.SliceData(b), len(b))` and then writing to `b`.
The string observes the write — undefined behavior. Fix: use `unsafe.String` only
when `b` is never mutated again, and use the copying `string(b)` otherwise.

### Classifying errors by string matching

Wrong: `strings.Contains(err.Error(), "timeout")`. It breaks on wrapping and on
any message change. Fix: `errors.As` for concrete/interface error types and
`errors.Is` for sentinels.

### Forgetting the nil case in Scan

Wrong: a `Scan(src any)` type switch with no `case nil`. A NULL column arrives as
`nil` and either panics on the default or writes a garbage zero. Fix: handle
`nil` explicitly with a defined policy (zero value, or an error if the column is
non-nullable).

### Assuming int is 64-bit

Wrong: skipping the range check because "int is 64 bits". It is not on every
target. Fix: guard `int64`→`int` narrowing with `int(^uint(0) >> 1)` or the
concrete-width math bounds.

### Treating conversion and assertion as interchangeable

`T(v)` will not extract a value from an interface, and `v.(T)` will not change a
concrete numeric type. Confusing them is a compile error at best and a wrong
mental model at worst: conversion reshapes a concrete value, assertion inspects
an interface.

Next: [01-webhook-json-decoder.md](01-webhook-json-decoder.md)
