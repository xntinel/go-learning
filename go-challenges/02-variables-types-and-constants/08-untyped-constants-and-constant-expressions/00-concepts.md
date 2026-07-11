# Untyped Constants and Constant Expressions in Production Backends — Concepts

Constants are where a backend encodes the numbers it must never get wrong: upload
limits, quotas, timeout budgets, rate-limiter capacities, permission masks, enum
contracts. Go's constant model is unusual and, once understood, is one of the
sharpest tools you have for pushing correctness *left* — from a 3am incident to a
red build. The core distinction the whole lesson turns on is between an *untyped*
constant, which is a pure compile-time number that adapts to whatever numeric
context uses it, and a *typed* constant, where the type itself is part of the
contract and deliberately refuses to mix with raw integers. Read this file once
and you have the model needed for the nine independent exercises that follow; each
one is a real backend artifact — a limit policy, a retry budget, a byte-unit
formatter, an RBAC bitmask, overflow guards, a sampler rate, a rate-limiter
config, a job-status enum, and compile-time invariant assertions.

## Concepts

### A constant has a value and a kind, not (yet) a type

An untyped constant carries two things: a value and a *kind* — one of integer,
floating, rune, complex, string, or boolean. What it does not carry is a type.
`const maxAvatar = 5 * 1024 * 1024` is an untyped integer constant with value
5242880 and kind integer. It has no `int` or `int64` glued to it. The type is
decided only when the constant is *used* in a typed context: assigned to a typed
variable, returned from a typed function, passed to a typed parameter. This is
precisely what lets the single declaration flow into an `int` field here, an
`int64` field there, and a `float32` return value somewhere else, with no
conversion noise at any call site. The constant is a number that takes the shape
of the hole it is poured into.

### Precision during compilation is effectively unbounded

Constant expressions are evaluated by the compiler with exact arithmetic. The
language spec guarantees at least 256 bits of mantissa and a large exponent
range, so intermediate results do not overflow the way runtime `int64` arithmetic
does. `const huge = 1 << 100` compiles fine — the compiler holds the value
exactly. The catch, and it is the whole discipline, comes at *materialization*:
the moment that constant lands in a typed variable, it is range-checked against
that type. `var n int64 = 1 << 100` fails to compile because 2^100 does not fit
`int64`. Huge intermediate values are legal; the final value must fit the target.

### Default types: what a bare constant becomes

When an untyped constant is used somewhere that needs *a* type but does not
dictate *which* — the classic case is `x := someConst` or passing the constant to
an `any`/`interface{}` parameter — it falls back to the default type for its kind:
integer defaults to `int`, floating to `float64`, rune to `rune` (which is
`int32`), complex to `complex128`, boolean to `bool`, string to `string`. This is
a real trap at API boundaries. If a downstream API wants a `float32` and you write
`rate := defaultRate`, `rate` is a `float64` (the default), not the `float32` you
assumed; you must convert explicitly, `float32(defaultRate)`, or assign to a typed
variable, `var rate float32 = defaultRate`. The `:=` gives you the default type,
never the type you happened to want.

### The kind of the operands decides the operation, not the target

`1.0 / 10` is `0.1`; `1 / 10` is `0`. The difference is not the variable you
assign the result to — it is the kind of the operands. `1 / 10` is integer
division because both operands are integer-kind constants, so the result is the
integer constant 0, and `var r float64 = 1 / 10` stores `0.0`, not `0.1`. Writing
`1.0 / 10` makes the first operand floating-kind, which promotes the whole
expression to floating and yields `0.1`. This silent integer-division bug bites
sampler rates, percentages, and any ratio computed from integer literals. The
target type being `float64` does not rescue you; the operation already happened at
compile time using the operand kinds.

### math package constants are untyped — and that is the point

`math.MaxInt64`, `math.MinInt64`, `math.MaxInt`, `math.MaxUint64`,
`math.MaxFloat64` are all *untyped* constants. This is exactly why
`var x int64 = math.MaxUint64` fails to compile: `math.MaxUint64` is the untyped
constant 2^64 − 1, and when you try to materialize it into an `int64` the compiler
range-checks it and rejects the overflow. You must use `uint64`. The untypedness
is a feature: the same `math.MaxInt64` can bound-check a value in an `int` context
and an `int64` context without any per-type variants, and the compiler still
catches the case where the constant cannot fit the type you assign it to.

### time.Duration constants are typed — and that is also the point

`time.Second`, `time.Millisecond`, and friends are *typed* constants of type
`time.Duration`. Here the type is deliberately part of the contract. A function
that takes a `time.Duration` will not accept a bare `5` — you must write
`5 * time.Second`, because a raw integer is not a `Duration`. That refusal is a
safety feature: it stops the "was that 5 seconds or 5 nanoseconds or 5
milliseconds?" class of bug. And because the constants are typed, constant
`Duration` arithmetic like `time.Second / requestsPerSecond` is folded at compile
time into a typed `Duration` (10ms for 100 rps), giving you a self-documenting,
unit-safe value with zero runtime cost.

### The decision rule: typed or untyped

Keep a constant *untyped* when it is a pure compile-time number that must adapt to
several numeric contexts — a byte limit that flows into `int`, `int64`, and
`float32`; a `math`-style bound. Make a constant *typed* — a defined type plus,
usually, a `Stringer` and maybe `TextMarshaler` — when the type itself is an
invariant of the API: an enum (`Status`), a bitmask flag set (`Perm`), a unit
(`time.Duration`). The typed version costs you explicit conversions but buys a
compile-time wall against passing a raw `int` where a domain value is required,
and against illegal state transitions. Untyped is for flexibility; typed is for
contract.

### iota builds typed flag sets from untyped arithmetic

`iota` is an untyped integer constant that increments once per `const`
specification line within a block. Combined with `1 << iota` it produces
power-of-two flag values — 1, 2, 4, 8 — folded at compile time. Wrap the block in
a defined type (`type Perm uint32`) and those flags become a contract: callers can
`|` and `&` them, but cannot pass an arbitrary `uint32` where a `Perm` is
expected. The same iota mechanism, without the shift, numbers the members of an
enum (`StatusQueued`, `StatusRunning`, ...), and the final iota value doubles as
the count of members — handy for compile-time table-length checks.

### Compile-time invariant assertions

Because constants are checked at compile time, you can make a constant that guards
*itself* and fails the build when it drifts. The workhorse idiom is
`const _ = uint(expr)`: converting a *negative* untyped constant to `uint` is a
compile error ("constant −1 overflows uint"), so `const _ = uint(limitA - limitB)`
fails the build the moment `limitA` drops below `limitB`. Pairing both directions
(`uint(a-b)` and `uint(b-a)`) asserts exact equality. `const _ = uint(0 - (n & (n-1)))`
fails unless `n` is a power of two. An array-length or indexing trick asserts that
a lookup table's length matches an enum's count. These guards have zero runtime
cost — their success *is* the test — and they catch a drifted constant before it
ever deploys.

### Untyped string and boolean constants exist too

The model is not only numeric. An untyped string constant can seed a defined
string type — the canonical text of an enum member, for instance — at compile
time, and an untyped boolean constant participates in constant expressions the
same way. When you write `const statusDoneText = "done"` and later use it to
build a `type Status`'s text form, you are relying on the same untyped-adapts-to-
typed-context rule, just in the string kind.

## Common Mistakes

### Declaring every numeric constant as int by habit

Wrong: `const maxBytes int = 5 * 1024 * 1024`, then every `int64` call site needs
`int64(maxBytes)`. Locking the type early forces noisy conversions everywhere.

Fix: leave it untyped — `const maxBytes = 5 * 1024 * 1024` — and let each context
adopt it. Only pin a type when the type is genuinely part of the contract.

### Expecting a fraction from integer division

Wrong: `const rate = 1 / 10` (or `requestsPerSecond / 2` with integer operands),
expecting `0.1` and silently getting `0` because both operands are integer-kind.

Fix: make an operand floating-kind — `1.0 / 10` — so the whole expression is
computed in the floating kind. The target being `float64` does not change the
operation; the operand kinds do.

### Assuming a huge constant becomes a usable runtime value

Wrong: `const huge = 1 << 100; var n int64 = huge`. The constant is fine; the
assignment is not — 2^100 does not fit `int64`.

Fix: keep intermediate constant math as large as you like, but ensure the *final*
materialized value fits the type where it lands.

### Assigning math.MaxUint64 into int64

Wrong: `var x int64 = math.MaxUint64`, surprised it fails. `math` constants are
untyped and range-checked against `int64`, which cannot hold 2^64 − 1.

Fix: use `uint64` for `math.MaxUint64`; reserve `int64` for values within its
range and bound-check with `math.MaxInt64`.

### Trusting := to pick a float32 (or int32) at a boundary

Wrong: `rate := defaultRate` and assuming `rate` is `float32` because a downstream
API wants `float32`. `:=` yields the default type, `float64`.

Fix: convert explicitly, `float32(defaultRate)`, or declare the target type,
`var rate float32 = defaultRate`.

### Mixing SI (1000) and binary (1024) byte ladders

Wrong: defining `KB = 1024` in one place and treating `1000` as a KB elsewhere, so
quota and billing math silently disagree by 2.4% and grows with each unit step.

Fix: define both ladders once, as clearly-named constant expressions
(`KiB = 1 << 10`, `KB = 1000`), and never let a value cross ladders implicitly.

### Exposing a bitmask or enum as a bare int

Wrong: `const PermRead = 1`, a plain `int`, so any function taking that "permission"
accepts `42`, and the compiler is powerless.

Fix: define `type Perm uint32` (or `type Status int`) so the type is the contract;
raw integers can no longer be passed where a domain value is required.

### Passing a bare int where a Duration is expected

Wrong: `Retry(5)` intending five seconds, or defeating the `Duration` contract with
`time.Duration(5)` (which is five nanoseconds).

Fix: write `5 * time.Second`. The typed `Duration` constant makes the bare-int
mistake a compile error on purpose; do not paper over it with a conversion.

### Overflowing an int64 counter at runtime

Wrong: accumulating an `int64` metrics counter with `+=` and discovering the wrap
to negative only in production.

Fix: bound-check with `math.MaxInt64` before adding (a `SafeAdd` helper), so an
overflow is a returned error on the ingest path, not a silent corruption.

Next: [01-upload-limit-policy.md](01-upload-limit-policy.md)
