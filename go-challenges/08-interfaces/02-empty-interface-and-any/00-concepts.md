# The Empty Interface and `any`: Untyped Values in Production Backends — Concepts

`any` is the seam where a statically typed Go backend meets untyped reality: JSON
of an unknown shape arriving on a webhook, config read from YAML or environment
variables, request-scoped values carried in a `context.Context`, values handed
back by a database driver, and the arbitrary attributes attached to a structured
log line. At every one of those boundaries the concrete type is genuinely not
known at compile time, and Go's answer is to box the value into an empty
interface. A senior engineer has to know exactly what the runtime does with that
box, how to recover the static type safely on the way out, which operations panic,
and — the part juniors miss — when `any` is the wrong tool and a typed interface
or a generic eliminates a whole class of runtime bugs. This file is the conceptual
spine; read it once and each of the independent modules that follow is a real
boundary artifact you can build without re-deriving the theory.

## Concepts

### `any` is exactly `interface{}`

`any` is a compiler alias introduced in Go 1.18. It is not a new type, not a
supertype, not a wildcard — `any` and `interface{}` are the identical type, and
you can assign between them freely. The alias exists purely for readability:
`map[string]any` reads better than `map[string]interface{}`, and a generic
constraint `[T any]` reads better than `[T interface{}]`. Everything true of the
empty interface is true of `any`. The interesting rule is not the syntax; it is
the discipline: reach for `any` only where the type is genuinely opaque at a
boundary, never as a lazy stand-in for a type you could have named.

### An interface value is two words

An interface value is a pair of machine words. The first is a type descriptor: for
a non-empty interface it is an `itab` (interface table) that pairs the concrete
type with the method-set slots the interface needs; for the empty interface it is
a plain `*_type` (the runtime calls this shape an `eface`). The second word is a
data pointer to the stored value. The consequence you must internalize: storing a
non-pointer value in an `any` generally forces that value to escape to the heap,
because the interface needs a pointer to point at. Assigning an `int` to an `any`
costs an allocation and adds a pointer for the garbage collector to trace. That is
cheap once and ruinous a million times — never box in a hot loop.

There are optimizations. A value that is already pointer-shaped (a pointer, a
slice header's backing, a `*T`) boxes without a fresh allocation because the data
word can point at the existing memory. The runtime also keeps a small table of
boxed integers for values below 256, so `any(1)` does not allocate but `any(1000)`
does. These are implementation details, not a contract: do not reason about
allocation from memory. Measure with `testing.AllocsPerRun` before you assume
boxing is free, and the capstone module does exactly that.

### A type assertion recovers the static type

Once a value is inside an `any`, the compiler has forgotten its type; a type
assertion is the runtime check that gets it back. The production default is the
comma-ok form: `v, ok := x.(T)` succeeds with `ok == true` and `v` the value when
`x`'s dynamic type is exactly `T` (or, for an interface target, implements it),
and otherwise returns the zero value with `ok == false` and no panic. The
single-return form `v := x.(T)` panics on a mismatch. The panic form is only
defensible when the type is statically guaranteed by construction a line or two
earlier; anywhere a value crossed a boundary — a decoded payload, a driver value —
the comma-ok form and a typed error is the only safe choice. A single unexpected
payload type must return a 400, not take down the handler.

### A type switch is the tool for many concrete types

When a value could be one of several concrete types, a chain of assertions is
noise; `switch v := x.(type) { case string: ...; case int: ... }` gives each case
body a `v` already typed to that case. It is the natural shape for a JSON value
walker or a `Stringer` that formats whatever it holds. The rule that separates a
correct type switch from a lurking bug is the `default` case: it is where an
unknown dynamic type must be handled explicitly — formatted defensively, or turned
into an error — never silently ignored. A type switch with no `default` that falls
through to a zero value is how malformed input becomes a silent data-corruption
incident.

### JSON into `any` has a fixed, lossy mapping

`json.Unmarshal(data, &v)` where `v` is `any` produces a fixed mapping: a JSON
object becomes `map[string]any`, an array becomes `[]any`, a string becomes
`string`, a boolean becomes `bool`, `null` becomes `nil`, and — the trap — every
number becomes `float64`. A `float64` has 52 bits of mantissa, so any integer
above 2^53 (about 9.0e15) cannot be represented exactly. A 64-bit database ID or a
Stripe-style order number silently loses its low bits the moment it round-trips
through `float64`. The fix at a 64-bit boundary is `json.Decoder` with
`UseNumber()`, which decodes numbers as `json.Number` (a string under the hood)
whose `Int64()` recovers the exact integer. This is not a micro-optimization; it
is the difference between charging the right customer and the wrong one.

### context values are `any`, so keys must be typed

`context.WithValue(parent, key, val)` stores an `any` value under an `any` key, and
`ctx.Value(key)` returns `any`. Correctness rests entirely on the key. If two
unrelated packages both use the string `"user"` as a key, the second silently
shadows the first — a collision the compiler cannot see. The discipline is a
package-private key type: `type ctxKey struct{}` (or a named unexported type) so
that a key from package A can never equal a key from package B even if their
underlying representations match, because their types differ. Pair each key with
typed accessors — `WithRequestID(ctx, id)` and `RequestID(ctx) (string, bool)` —
so no caller ever writes a raw assertion, and a missing value returns `("", false)`
instead of panicking.

### The database/sql boundary is a closed set

`database/sql/driver` narrows `any` to a closed set. `driver.Value` is documented
to be one of exactly six dynamic types: `nil`, `int64`, `float64`, `bool`,
`[]byte`, `string`, and `time.Time`. A type that implements `driver.Valuer` must
have its `Value()` return one of those; a type that implements `sql.Scanner` must
have its `Scan(src any)` accept any of them, because different drivers hand back
different representations of the same column — one driver returns a `TEXT` column
as `[]byte`, another as `string`, and you must handle both. Two rules are easy to
miss: `nil` src means SQL `NULL` and must be handled as a zero value, not an error;
and you must not retain the `[]byte` you were handed past the return of `Scan`,
because the driver owns and reuses that memory — copy it (or unmarshal out of it,
which copies) before you return.

### Comparability is a dynamic property

`==` on two `any` values first compares their dynamic types and then their values —
and it panics at runtime if the dynamic type is not comparable. Slices, maps, and
functions are not comparable, so `any([]byte{1}) == any([]byte{1})` does not return
`false`, it panics. The same landmine sits under map keys: using an `any` whose
dynamic type is a slice as a map key panics on insert. When you must compare values
of unknown dynamic type — deduplicating repeated webhook deliveries, detecting
config drift — guard first with `reflect.TypeOf(x).Comparable()` and use fast `==`
only when it is true, falling back to `reflect.DeepEqual` for the uncomparable
cases. Do not wrap every comparison in `recover`; check comparability up front.

### Generics and typed interfaces are the antidote

Most `any` in a codebase is not a true untyped boundary; it is a missing type
parameter. An in-memory cache typed as `map[string]any` forces every caller to
assert on the way out and boxes every value on the way in. Rewritten as
`Store[K comparable, V any]`, the value type is enforced by the compiler, callers
get a typed `V` back with no assertion, and the boxing disappears. A type mismatch
that compiled fine against the `any` store — storing a `User` and reading it back
as an `Order` — becomes a compile error. The lesson of the capstone is exactly
this: reach for `any` only at a genuine untyped boundary (JSON, config, context,
driver, log attrs), and everywhere else let a small method-bearing interface or a
type parameter move the check to compile time.

## Common Mistakes

### Using `any` where a typed interface or generic belongs

Wrong: a function that takes `any` and immediately type-switches over three known
types. The contract is invisible and the check is deferred to runtime.

Fix: define a small method-bearing interface that names those types' shared
behavior, or make the function generic. The compiler now documents and enforces
the contract, and the type switch disappears.

### Panic-form assertions on boundary values

Wrong: `id := payload["order_id"].(int64)` on a decoded webhook. One malformed
delivery panics the handler.

Fix: the comma-ok form and a typed error — `id, ok := ...; if !ok { return ErrBadPayload }`.
The panic form is only for a type guaranteed by construction a line earlier.

### Comparing `any` of uncomparable dynamic type

Wrong: `if a == b` where `a` and `b` are `any` that might hold slices or maps; or
using such an `any` as a map key. Both panic at runtime for slices/maps/funcs.

Fix: check `reflect.TypeOf(a).Comparable()` before `==`, and fall back to
`reflect.DeepEqual`. Never key a map with an `any` of unknown dynamic type.

### Assuming JSON numbers are `int`

Wrong: expecting `payload["amount"].(int)` after `json.Unmarshal` into `any`. JSON
numbers decode as `float64`, so the assertion fails, and even as `float64` a large
int64 ID is already corrupted.

Fix: use `json.Decoder.UseNumber()` and `json.Number.Int64()` at any 64-bit
boundary; coerce `float64` deliberately and reject non-whole values.

### Bare or built-in context keys

Wrong: `context.WithValue(ctx, "request_id", id)`. Any other package using the
same string collides silently.

Fix: an unexported key type (`type reqIDKey struct{}`) and typed accessors, so keys
from different packages can never be equal and callers never assert.

### A one-sided sql.Scanner

Wrong: `Scan` that only handles `[]byte` and panics on `string` or `nil`; or one
that stores the `[]byte` src in a field and reads it later.

Fix: handle `[]byte`, `string`, and `nil` (as zero), return a typed error for
anything else, and copy or unmarshal out of the `[]byte` before returning — the
driver reuses that buffer.

### Boxing in hot loops without measuring

Wrong: pushing millions of values through an `any`-typed channel or slice and
assuming it is free. Each non-pointer element is a heap allocation and GC work.

Fix: keep a concrete or generic type on the hot path; reserve `any` for the true
boundary. Confirm with `testing.AllocsPerRun` rather than guessing.

### Lossy or panic-prone log fields

Wrong: a variadic log helper that panics on an odd number of key-value args, or
that logs a raw error struct with `%v` instead of its `Error()`.

Fix: mirror `slog`'s own `!BADKEY` handling for a dangling arg (never panic), and
render error values through `Error()`. Use `slog.LogValuer` for expensive fields so
they are only computed when the record is actually emitted.

### reflect for everything

Wrong: `reflect.DeepEqual` on every comparison for safety. It is correct but slow,
and it allocates.

Fix: check `Comparable()` and use `==` on the fast path; reserve `DeepEqual` for
the genuinely uncomparable dynamic types.

Next: [01-json-value-any-type.md](01-json-value-any-type.md)
