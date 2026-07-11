# Constants and Iota for Domain Modeling — Concepts

Constants look like the most boring corner of the language: a `const` block, a
sprinkle of `iota`, done. The senior lens is different. A constant is trivial
only while it stays inside one package. The moment its value crosses a boundary
— a database column, a JSON payload, a config file, an RPC message — that value
stops being an internal counter and becomes an implicit wire contract. A
careless reorder of a `const` block, or an insertion in the middle of it,
silently rewrites the meaning of every row already persisted and every payload
already in flight. Nothing fails to compile. Nothing throws. The `4` you wrote
last quarter as `StateFailed` is now `StateCancelled`, and you find out from an
on-call page, not a test.

This lesson treats constants as the seam between compile-time domain modeling
and runtime persistence and serialization. The recurring discipline is: model
the domain with `iota` for ergonomics, but never let the ordinal escape.
Reserve the zero value for `Unknown` so a forgotten struct field cannot
masquerade as a valid state. Persist and serialize a stable string code — via
`driver.Valuer`/`sql.Scanner` and `encoding.TextMarshaler` — instead of the raw
ordinal. Use powers-of-two bitmasks for permission SETS and plain `iota` for
mutually-exclusive states. Encode size and duration limits with the correct
precision and type. And guard against enum drift with exhaustiveness and
round-trip tests, so the failure surfaces in CI instead of in production.

## The model: what a constant actually is

A constant is an immutable value fixed at compile time. It has no address — you
cannot take `&SomeConst` — because it never occupies a variable's storage; the
compiler substitutes the value at each use site. This is why a constant can be
used where the language demands a compile-time value: array sizes, other const
expressions, `case` labels.

Constants come in two flavors, and the distinction drives half of this lesson.

A *typed* constant binds a value to a named type: `const DefaultPageSize int =
100`, or `StateQueued State = ...`. Once typed, it participates in Go's strict
type system — a `State` cannot be silently added to a `Permission` or an
`http.StatusCode`, even though both are integers underneath. That is the point:
typing a constant makes the domain concept unmixable with unrelated integers,
so the compiler rejects a category error you would otherwise ship.

An *untyped* constant has a default kind (integer, floating-point, rune, string,
boolean) but no fixed type until it is assigned or used in a typed context.
Untyped numeric constants carry *arbitrary precision* at compile time: the
expression `1 << 40` is computed exactly, with no overflow, regardless of any
machine word size, and only narrows to a concrete type when it lands in a typed
destination. This is why the byte-size idiom `1 << (10 * iota)` yields `KiB`,
`MiB`, `GiB` exactly, and why a limit that would overflow `int` on a 32-bit
build stays correct if you assign it to `int64`.

## iota: a per-block line counter, nothing more

`iota` is not magic and not a global. It is a counter scoped to a single `const`
block. It resets to `0` at each `const` keyword and increments by one for every
*ConstSpec* (each line) in that block — whether or not that line mentions
`iota`. The subtle rule that trips people up: when a line omits its expression,
Go repeats the *entire previous expression*, with the current `iota`
substituted. So

```go
const (
	StateUnknown State = iota // 0
	StateQueued               // repeats "State = iota" -> 1
	StateRunning              // 2
)
```

works because the `State = iota` expression is implicitly copied down. Combine
this with the blank identifier `_` to skip a slot, and with shift expressions to
build bitmasks and unit ladders. Once you internalize "the counter ticks per
line, the expression is repeated," every `iota` pattern — sentinels, skipped
slots, `1 << iota` flags, `1 << (10 * iota)` units — reads mechanically.

## Reserve the zero value for Unknown

Go zero-initializes every variable and every struct field you do not explicitly
set. For an enum, that means the value `0` is the one you get *by accident*: a
`Job{ID: "x"}` with no `State` field set, a `var s State` never assigned, a
JSON object missing the `state` key. If you write `StateQueued State = iota`,
then `0` is `queued`, and every one of those accidents silently produces a job
that looks legitimately queued. The bug is invisible because the zero value is
indistinguishable from a real one.

Reserving `iota == 0` for `StateUnknown` (or `Invalid`, or `Unset`) converts
that whole class of bug into a detectable state. A forgotten field is now
`Unknown`, which your validation, your `ParseState`, and your persistence layer
can all reject explicitly. The zero value should always be either a safe,
correct default or an obviously-invalid sentinel — never a real operational
state you would be surprised to land in by omission.

## Enums versus bitmasks: two different shapes

Plain `iota` and `1 << iota` look similar but model opposite things, and mixing
them is a category error.

Plain `iota` models a set of *mutually-exclusive* values: a job is queued OR
running OR succeeded, exactly one at a time. The values `0,1,2,3,4` are just
distinct labels; their numeric relationships are meaningless. Combining them
with bitwise OR (`StateQueued | StateRunning`) produces a number that is neither
state and means nothing.

`1 << iota` models a *set* of independent flags — a bitmask — where each value
is a distinct power of two occupying its own bit: `Read = 1`, `Write = 2`,
`Admin = 4`. Because the bits do not overlap, you can hold any combination at
once, and the set operations are bitwise: union with OR (`|`), test membership
with AND (`all & required == required`), and — the one people get wrong — clear
a flag with AND-NOT (`all &^ required`). Do not clear a bit by subtraction (it
underflows if the bit was not set) or by `all & ^required` on a too-narrow
unsigned type (the complement's high bits matter). `&^` is the idiom precisely
because it cannot underflow and touches only the named bits. `math/bits`
provides `OnesCount16` and friends to count how many flags are set.

The decision rule: if the values are alternatives, use plain `iota`; if they
are independently-combinable flags, use `1 << iota`. A value can never be both.

## The ordinal is private; the external contract is not

This is the thesis of the lesson. An `iota` ordinal is an *internal* encoding
tied to declaration order. It is perfectly fine to reorder a `const` block, or
insert a new value in the middle, as long as the ordinal never leaves the
process. The instant it crosses a boundary, its number is frozen into a
contract you did not intend to sign:

- A database column storing `2` for `running`: reorder the block and every
  existing row now means something else.
- A JSON field that marshals to `2` by default int encoding: every client that
  cached or persisted that payload is now wrong.
- A config file or an RPC message: same failure, different medium.

The fix is uniform: never let the ordinal cross the boundary. Serialize a stable
string code by implementing `encoding.TextMarshaler`/`TextUnmarshaler` (so
`encoding/json` emits `"running"`, not `2`) and persist one by implementing
`database/sql/driver.Valuer` and `sql.Scanner` (so the column stores
`'running'`). Now the string is the contract, the ordinal is free to change,
and a round-trip test locks the mapping so a rename is caught in CI. Two
practical notes on the persistence seam: `Valuer.Value` returns a
`driver.Value` (one of a small set of types — `string` is fine), and
`sql.Scanner.Scan` must handle whatever the *driver* hands back, which for a
text column is frequently `[]byte`, not `string` (and sometimes `nil`). A
`Scan` that type-switches on `string` only will panic or error at runtime under
a driver that returns bytes.

## Sizes, durations, and compile-time precision

Two recurring numeric-limit patterns exploit untyped-constant precision.

Byte sizes use `1 << (10 * iota)` with a blank first slot:

```go
const (
	_          = iota
	KiB int64 = 1 << (10 * iota) // 1024
	MiB                          // 1048576
	GiB                          // 1073741824
)
```

The arithmetic is exact and unbounded at compile time; you then pin the result
to `int64` so a multi-gigabyte limit survives a 32-bit target instead of
overflowing an `int`. Use binary `1024`, not decimal `1000`, when you mean KiB.

Durations exploit that `time.Duration` is an `int64` count of nanoseconds. An
untyped numeric constant multiplies cleanly with `time.Second` (`5 *
time.Second` is a `time.Duration`), so a backoff table reads naturally. The trap
is exponential backoff by shifting: `BaseBackoff << attempt` overflows `int64`
at high attempt counts and wraps to a negative or absurd duration. Cap it with
a `MaxBackoff` (or `min`) and guard the overflow explicitly.

## Weaponize the compiler, then guard drift

Two defensive techniques close the lesson.

A *compile-time assertion* turns "this limit must fit its type" into a build
failure instead of a runtime surprise. `const _ uint16 = MaxPageSize` fails to
compile if `MaxPageSize` exceeds `65535`. `math.MaxInt64`, `math.MaxUint16`, and
friends give exact boundaries for these guards. For a limit that depends on
runtime inputs (page × size), there is no compile-time check, so pair the
constant boundary with a runtime overflow guard that returns an error rather
than a wrapped-around negative offset.

*Exhaustiveness discipline* catches the most common enum bug: someone adds a new
value and forgets to update `String`, `ParseState`, or an error/status registry,
so it serializes as `"unknown"` or panics on lookup. A trailing sentinel
constant (`maxState` as the final line of the block, capturing the count via
`iota`) bounds an iteration over every real value, and a round-trip test asserts
`ParseState(s.String()) == s` for each. When a teammate adds a constant but
forgets the mapping, the count no longer matches the sentinel and the test
fails — drift caught in CI, not on call.

Finally, prefer named stdlib constants (`http.StatusServiceUnavailable`, not
`503`) over magic numbers, and model derived categories — a status class, an
error code — as their own typed enums backed by an explicit registry for the
external contract. The registry is the single place the public string and HTTP
status live, decoupled from declaration order.

## Common Mistakes

### Starting the enum at the zero value

Wrong: `StateQueued State = iota`, so the zero value is a real, operational
state and a forgotten struct field silently becomes `queued`. Fix: reserve
`iota == 0` for `Unknown`/`Invalid`, an explicitly non-operational sentinel.

### Persisting or serializing the raw ordinal

Wrong: storing `2` for `running` in a column or letting default int JSON
encoding emit `2`, then reordering or inserting a constant and silently
corrupting every existing row and payload. Fix: persist a stable string code via
`Valuer`/`Scanner` and `TextMarshaler`/`TextUnmarshaler`, and lock it with a
round-trip test.

### Combining sequential iota values as a set

Wrong: `StateQueued | StateRunning`, expecting a meaningful combined state. Fix:
`1 << iota` for genuine flag sets only; keep mutually-exclusive states as plain
`iota` and never OR them.

### Clearing a bit the wrong way

Wrong: `all & ^required` on an unsigned type of the wrong width, or `all -
required` (which underflows if the bit was unset). Fix: the idiomatic clear is
`all &^ required` (AND-NOT), which cannot underflow and touches only the named
bits.

### Declaring byte sizes as int or in decimal

Wrong: `KiB = 1 << 10` as an `int` that overflows a 32-bit build at a
multi-gigabyte limit, or using `1000` where you mean `1024`. Fix: `int64` typed
constants with `1 << (10 * iota)`.

### Shifting a duration without a cap

Wrong: `BaseBackoff << attempt` with no ceiling, overflowing to a negative or
huge duration at high attempt counts. Fix: saturate at `MaxBackoff` (or `min`)
and guard the overflow.

### Adding a value and forgetting the mapping

Wrong: a new enum value with no `String`/`ParseState`/registry entry, so it
serializes as `"unknown"` or panics on lookup. Fix: an exhaustiveness test bound
by a sentinel count constant plus a round-trip assertion.

### Assuming Scan only receives string

Wrong: a `sql.Scanner` that type-switches on `string` only; many drivers return
a text column as `[]byte`, so it fails at runtime. Fix: handle `string`,
`[]byte`, and `nil`, and error on any other source type.

### Magic numbers in HTTP retry logic

Wrong: `503` and `429` scattered through retry code, easy to typo (`503` vs
`530`). Fix: reference `net/http` named constants and model the status class as
a typed enum.

Next: [01-workflow-states-and-permissions.md](01-workflow-states-and-permissions.md)
