# Type Inference At Boundaries — Concepts

Type inference bugs do not announce themselves. The compiler accepts the code, the
tests pass, and then in production an `int32` size field overflows, a per-second
rate truncates to `0`, a 19-digit order id loses its low bits, or a
`strconv.ParseFloat(raw, 32)` that "returns a float32" turns out to have been a
`float64` the whole time. None of these is a syntax error. Each is a place where
what the compiler *inferred* differs from what the engineer *intended*, and the gap
is only visible under a specific input at runtime.

This lesson treats inference as an operational hazard at the exact boundaries where
senior backend engineers live: config loaders, port and id parsers, byte-count
constants, retry and backoff math, metrics rate computation, JSON decoding, and
generic helpers. In every module the difference between correct and quietly wrong
is whether you can predict the inferred concrete type before it crosses the
boundary, and every module pins its contract with a compile-time type assertion so
that a later refactor which widens or narrows a type breaks the *build*, not a
customer.

## Inference follows the expression, not your intent

`:=` does not adopt the type you had in mind; it adopts the type the right-hand
expression produces. An integer literal defaults to `int`, a floating literal to
`float64`, a function call to that function's declared return type. So
`workers := 4` is an `int`, not the `int64` your `BIGINT` column wants, and
`rate := strconv.ParseFloat(raw, 32)` — had it compiled that way — would have been
`float64`. The mental model to internalize: read the expression, name its type,
and only then decide whether a boundary needs you to be explicit. When the storage
type matters, write it down: `var workers int64 = 4`, or let a typed struct field
drive an untyped constant, or convert after a validated range check.

## Untyped constants have arbitrary precision and a default type

A literal like `10 << 20`, `2 * time.Second`, or `0.10` is an *untyped constant*.
Untyped constants are evaluated at arbitrary precision and carry only a *default
type by kind* (`int`, `float64`, `string`, `bool`, `rune`). They adapt to whatever
target is in scope: assign `10 << 20` to an `int64` field and it becomes `int64`;
assign `0.10` to a `float32` field and it becomes `float32`. Only when there is no
target — a bare `:=`, a variadic parameter, a return with no declared type — does
the constant fall back to its default type. This is why letting a struct field's
type "reach back" and type a constant is the safest pattern: the field is the
target, the constant conforms.

Arbitrary precision has a sharp, useful edge: `const maxUpload = 5 << 30` is a
perfectly legal constant even though the expression is large, but the moment you
try to store a constant that does not fit its fixed-width target — `1 << 40` into an
`int32`, or `5 << 30` into an `int32` on any platform — it is a *compile-time
overflow error at the assignment site*. That is not a nuisance; it is a guardrail
you can lean on for size limits. By contrast, `x := 1 << 40` infers `int`, which is
64-bit on every platform Go currently targets for servers — safe on amd64/arm64,
but an operational trap for anyone who still assumes `int` is 32-bit.

## Parse-family functions return wide, fixed types

Every `strconv` and `time` parser returns one concrete type regardless of any
`bitSize` argument. `strconv.ParseInt` returns `int64`. `strconv.ParseUint` returns
`uint64`. `strconv.ParseFloat` returns `float64` — even `ParseFloat(raw, 32)`.
`strconv.Atoi` returns `int`. `strconv.ParseBool` returns `bool`.
`time.ParseDuration` returns `time.Duration`. The `bitSize` argument bounds the
*accepted range* (so `ParseUint(raw, 10, 16)` rejects `65536` with
`strconv.ErrRange`), but it does not change the *return type*. The correct pattern
is: parse into the wide type, validate the value fits and satisfies your business
range, then narrow with an explicit conversion — `uint16(n)`, `float32(f)` — on the
far side of the check. Narrowing before validating is how you silently wrap a value.

## Defined types are distinct even over the same underlying type

`time.Duration` is defined as `int64`; a `Level` you declare as `type Level int8`
has underlying type `int8`. But a defined type is a *distinct* type: you cannot add
a `time.Duration` to a plain `int`, compare a `Level` to an `int8`, or multiply a
`Duration` by an `int` variable, without an explicit conversion. The one exception
is untyped constants, which are the only operands that cross the boundary freely —
which is exactly why `2 * time.Second` compiles (`2` is untyped) while
`attempt * time.Second` (with `attempt` an `int` variable) does not. To scale a
duration by a runtime integer you must write `time.Duration(attempt) * time.Second`
or shift the duration's bits. This distinction is what makes typed enums and
`time.Duration` safe: the compiler refuses to let unrelated quantities mix.

## Integer division truncates toward zero

`a / b` where both operands are integers yields an integer, truncated toward zero.
`5 / 10` is `0`, not `0.5`; `3 / 1000` is `0`. This is the single most common
source of silent metrics bugs: `requestCount / windowSeconds` and `errors / total`
collapse to `0` (or `1`) and a dashboard reports a flat line. The fix is to convert
one operand to `float64` *before* the division — `float64(count) / windowSeconds` —
and only round for reporting. Guard the denominator against zero separately;
integer division by zero panics, and float division by zero yields `Inf`/`NaN`.

## Generics: type-parameter inference and constraints

Since Go 1.18 you can omit explicit type arguments when the compiler can deduce
them from the call. `Clamp(v, 1, n)` infers `T` from `v`, `1`, and `n`; you do not
write `Clamp[int](...)`. Constraints define what operations are legal on `T`:
`cmp.Ordered` (Go 1.21) admits every type that supports `<` — ints, floats,
strings, and defined types over them — so `min`, `max`, and `<` work on a
`cmp.Ordered` type parameter. `comparable` admits every type usable with `==`.
`cmp.Or[T comparable](vals ...T) T` (Go 1.22) returns the first non-zero argument
and infers `T` from its variadic args; mixing two differently-typed arguments in a
single call fails inference, because all variadic elements must share one `T`. An
untyped constant argument is fine (it conforms to whatever `T` the other args fix),
but two *typed* arguments of different types is an error.

`cmp.Or` has a semantic trap distinct from its inference: it is a *first-non-zero*
selector, not a *presence* check. An explicit `0`, `""`, or `false` override is
indistinguishable from "unset", so if zero is a legal configured value you must
carry presence separately (a pointer or an `ok` bool). Do not use `cmp.Or` where a
zero override must win.

## The any/interface boundary erases static type

The moment a value passes through `any` (or `interface{}`), its static type is gone
and only its dynamic type remains, recoverable with a type switch or assertion. The
canonical production hazard is JSON: `encoding/json` decodes *every* JSON number
into `float64` when the target is `any` or `map[string]any`. A `float64` has a
53-bit mantissa, so any integer larger than 2^53 (about 9.0e15) — a Snowflake id, a
large order id, a 19-digit external id — loses its low bits silently on decode. The
fix is `json.Decoder.UseNumber()`, which delivers numbers as `json.Number` (a
string underneath); you then call `Number.Int64()` to recover the exact integer, or
`Number.Float64()` when a float really is what you want.

## Compile-time type assertions turn expectations into build contracts

The line `var _ T = expr` compiles only if `expr` is assignable to `T`. Put such a
line in a `_test.go` file — or anywhere in the package — and you have converted an
inferred-type *expectation* into a build-time *contract*. If a later refactor
changes a function's return from `int` to `int64`, or widens a struct field, the
assertion stops compiling before any test runs. This is the cheapest, earliest
guard against silent widening or narrowing, and it costs one line per boundary you
care about. The discipline across this lesson: be explicit about the target type at
each boundary (config, network, JSON, DB column, public signature), keep the
validated range next to the conversion, and pin the result type with `var _ T`.

## Common Mistakes

### Assuming a literal infers to the storage type

Wrong: `workers := 4` and expecting `int64` because the column is `BIGINT`. `4` is
an untyped constant with default type `int`, so `workers` is `int`.

Fix: give it a target — `var workers int64 = 4`, let a typed struct field drive the
constant, or convert after a range check. The storage type must appear somewhere
the compiler can see it.

### Believing a `bitSize` argument changes the return type

Wrong: treating `strconv.ParseFloat(raw, 32)` as returning `float32`, or
`strconv.ParseUint(raw, 10, 16)` as returning `uint16`.

Fix: both return the wide type (`float64`, `uint64`); `bitSize` only bounds the
accepted range. Validate, then convert with `float32(f)` / `uint16(n)`.

### Multiplying a duration by an int variable

Wrong: `attempt * time.Second` where `attempt` is an `int`. A `time.Duration` and a
plain `int` cannot mix; only the untyped-constant form `2 * time.Second` compiles.

Fix: `time.Duration(attempt) * time.Second`, or shift the duration:
`base << attempt`.

### Integer-division truncation in rates and ratios

Wrong: `count / windowSeconds` or `errors / total` with both operands integers —
the result truncates to `0` or `1`.

Fix: convert one operand to `float64` before dividing, then `math.Round` for
reporting. Guard a zero denominator explicitly.

### Mixing a defined type with its underlying type

Wrong: comparing a `Level` (underlying `int8`) to a plain `int`, or adding a
`time.Duration` to an `int`, expecting the underlying types to make them
interchangeable.

Fix: convert explicitly — `int8(level)`, `time.Duration(n)`. Defined types are
distinct; only untyped constants cross freely.

### Trusting JSON to preserve integers

Wrong: decoding into `any`/`map[string]any` and assuming a large integer id
survives. Every JSON number becomes `float64`, corrupting integers beyond 2^53.

Fix: `json.Decoder.UseNumber()`, then `json.Number.Int64()` for exact integers.

### Using `cmp.Or` as a presence check

Wrong: relying on `cmp.Or(override, def)` when `0`/`""`/`false` is a legal override
value — `cmp.Or` returns the first *non-zero* value, so an explicit zero is
indistinguishable from unset.

Fix: carry presence separately with a pointer or an `ok` bool when zero is a
meaningful value.

### Returning inferred defaults through `any` or a map

Wrong: computing defaults with `:=` in a helper and returning them via `any` or
`map[string]interface{}`, which discards the target types and reintroduces the
ambiguity you were avoiding.

Fix: keep defaults typed and close to the constructor, so struct field types drive
the constants.

Next: [01-runtimecfg-config-loader.md](01-runtimecfg-config-loader.md)
