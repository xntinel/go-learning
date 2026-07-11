# Stringer, Formatter, and the Human/Machine Representation Boundary — Concepts

`Stringer` looks like a convenience for prettier `fmt.Println` output. It is not.
For a domain type — an enum, a money amount, a credential, a byte count — the
`String()` method is one seam in a set of at least four distinct
representations, and the production incidents come from letting those seams drift
apart. A senior engineer treats a domain enum as having a human representation
(`String()` for logs, errors, and operator UX), a wire representation
(`MarshalText`/`MarshalJSON` for the API), a storage representation
(`driver.Valuer`/`sql.Scanner` for the database column), and a structured-logging
representation (`slog.LogValuer` for redaction). When only one of these exists,
the others fall back to the language default — and the default for an enum whose
underlying type is `uint8` is to emit the raw `iota` integer. That integer is a
time bomb: reorder or insert a constant in a later release and every stored row
and every serialized message silently changes meaning. This file is the
conceptual foundation for the ten independent exercises that follow; read it once
and you have the model you need for all of them.

## Concepts

### What Stringer is, and exactly which verbs it governs

`fmt.Stringer` is `interface { String() string }`. Any type with a
`String() string` method satisfies it, and `fmt` consults it for the *string* and
*default* representations: `%s`, `%v`, `%q` (the result is then quoted), and the
argument-driven `Print`, `Println`, `Sprint` family. It does **not** govern
type-specific verbs. Printing a `Stringer` with `%d`, `%x`, `%o`, `%b`, or `%t`
bypasses `String()` entirely and formats the underlying numeric or boolean value.
This surprises people whose logs mix verbs: `log.Printf("%d", status)` prints `2`
while `log.Printf("%s", status)` prints `running`, and both are "correct" per the
rules. `String()` is the human seam; it is not a universal override.

### The four representations that must stay consistent

A domain enum in a real service is represented four independent ways, and the
common production bug is implementing exactly one of them:

- `String()` — human-facing: logs, error messages, CLI and operator output.
- `MarshalText`/`UnmarshalText` (and/or `MarshalJSON`/`UnmarshalJSON`) — the wire
  format crossing an API or config boundary.
- `driver.Valuer`/`sql.Scanner` — the value stored in and read from a database
  column.
- `slog.LogValuer` — the value a structured-logging handler emits, which is where
  redaction lives.

If you write `String()` and stop, `json.Marshal` still emits the raw `iota`
integer (it does not call `String()`), the database column stores the integer,
and `slog` reflects over the field. The four representations should share a single
source of truth — one canonical `name <-> value` table that `String()`,
`MarshalText`, and `Value()` all consult — so they cannot diverge.

### The method-set rule that makes or breaks satisfaction

Whether a value satisfies `fmt.Stringer` is decided by Go's method-set rules, and
this is the single most common Stringer bug. A *value* method
`func (t T) String() string` is in the method set of both `T` and `*T`, so both a
`T` value and a `*T` pointer satisfy `fmt.Stringer`. A *pointer* method
`func (t *T) String() string` is in the method set of `*T` only. The moment
`String()` is declared on a pointer receiver, a plain `T` value stops satisfying
`fmt.Stringer` — and so does every `T` stored inside a slice, an array, a map
value, or a struct field, because those are addressed as values during formatting.
The result is silent: `fmt` finds no `String()` on the value and prints Go's
default struct/integer layout instead. The rule is therefore blunt: prefer value
receivers for `String()` on small domain types, so the value and everything that
contains it formats correctly.

### String() must be pure

`fmt` may call `String()` many times, from many goroutines, in a log hot path.
It must be a pure function of the receiver: idempotent, side-effect-free, cheap.
A `String()` that logs will emit lines during every format call. One that
increments a counter is non-idempotent. One that acquires a lock can deadlock if
formatting happens while that lock is held elsewhere. One that panics turns a log
line into a crash. Compute from the receiver's fields and return; do nothing else.

### The infinite-recursion trap

The classic crash: a `String()` that formats its own receiver with a
default-dispatching verb.

```
func (s Status) String() string {
    return fmt.Sprintf("%v", s) // %v re-enters String() -> stack overflow
}
```

Because `%v` (and `Sprint(s)`) dispatch back into `String()`, the method calls
itself forever and the goroutine's stack overflows — an unrecoverable crash, not
a `panic` you can catch. The fix is to format something that is *not* the named
receiver: a specific field, the underlying type via a conversion, or a locally
defined type that lacks the method. A defined type `type raw Status` does not
inherit `Status`'s methods, so `fmt.Sprintf("%v", raw(s))` prints the underlying
layout without re-entering `String()`. Never pass the whole named receiver to a
default-dispatching verb inside its own `String()`.

### fmt.Formatter: the escalation from Stringer

`Stringer` gives you one representation. When you need per-verb behavior, width,
precision, and flags, you implement `fmt.Formatter`:
`Format(f fmt.State, verb rune)`. `fmt.State` is an `io.Writer` (`Write`) plus
`Width() (int, bool)`, `Precision() (int, bool)`, and `Flag(c int) bool`. Now a
single money type can print `$12.34` under `%s`, its raw minor units under `%d`,
include the currency code under `%+v`, honor `%8s` padding, and respond to the
`'+'` and `'#'` flags — all decided inside `Format`. Reach for `Formatter` when a
type genuinely needs verb- and flag-sensitive output (money, durations, masked
identifiers); do not stuff width/precision logic into `String()`, where `fmt`
ignores it, and do not reach for `Formatter` when a single `String()` suffices.

### GoStringer is a different seam

`fmt.GoStringer` is `interface { GoString() string }` and drives `%#v`. It is
meant for Go-syntax debug output — a representation you could paste back into
source — and is distinct from the human `String()`. A type can have both: `%v`
gives the operator-friendly form, `%#v` gives the developer-friendly Go-literal
form.

### Never persist or transmit the raw iota

An enum's `iota` integer is an implementation detail of the current source order.
Storing `2` in a database column or emitting `2` on the wire couples every
historical record to the exact declaration order at the time it was written.
Insert a new constant in the middle, or reorder for readability, and `2` now means
a different state — every old row and every replayed message is silently
corrupted, with no error to alert you. The rule: persist and transmit the stable
string *name*, via `MarshalText`/`Valuer`, and parse it back on the way in. Treat
an unrecognized name as an explicit, typed error — never a silent zero value,
which hides both bad input and your own migration bugs.

### slog.LogValuer: leak-proof redaction

`slog.LogValuer` is `interface { LogValue() slog.Value }`. It lets a type control
its own structured-log representation independently of `String()`. For a secret,
returning `slog.StringValue("REDACTED")` is the idiomatic, leak-proof redaction:
no `slog` code path can print the cleartext, because the handler asks the value
what to log and the value answers "REDACTED". This matters because a struct logged
with `slog` is reflected over field by field; a `Token` type with a `String()`
that redacts but no `LogValue()` can still leak if a handler reaches the underlying
string, and it always leaks under a plain `%v` on the containing struct if the
field is exported. `slog.GroupValue` is the complement: it expands one value into
several safe sub-attributes (a user id, a key prefix) so observability keeps the
useful shape without the secret. `slog` resolves nested `LogValuer`s with loop
protection, so a `LogValue()` that returns another `LogValuer` is safe.

### go:generate stringer for large enums

A fifteen-value enum with a hand-written `switch` is a maintenance liability: add
a constant, forget the `switch` arm, and `String()` silently returns the wrong
name or the numeric fallback. `//go:generate stringer -type=ErrorCode` (from
`golang.org/x/tools/cmd/stringer`) emits a `String()` backed by a `_name` string
and an `_index` array — fast and allocation-light, and it special-cases
contiguous runs. Two things to understand: the generated file includes a
compile-time guard function that fails to build if the constant *values* change
without regeneration; but if you *insert* a constant (shifting later values)
without rerunning `stringer`, the names silently misalign, so a CI guard that
fails when a declared constant is missing from the generated table is worth
keeping. The generated out-of-range fallback is exactly the `TypeName(N)` form.

### The unknown-value convention is TypeName(N)

When `String()` meets a value outside its known set, the convention — the one
`stringer` itself emits — is `TypeName(N)`: `unknown(99)`, `ErrorCode(42)`,
`Status(7)`. Never return an empty string for an unrecognized value: a blank log
line is undiagnosable, and it hides the data-corruption bug (a persisted integer
that no longer maps to a constant) that produced the out-of-range value in the
first place. `TypeName(N)` keeps the bad value visible and traceable.

## Common Mistakes

### Pointer-receiver String() on a type used by value

Wrong: `func (t *T) String() string`, then formatting a `T` value or a `T` inside
a slice or struct field. The value does not satisfy `fmt.Stringer`, so `fmt`
prints Go's default layout and the intended string never appears.

Fix: use a value receiver, `func (t T) String() string`. A value method is in
both `T`'s and `*T`'s method sets, so the value, the pointer, and every composite
containing a `T` all format correctly.

### Recursing through the receiver

Wrong: `return fmt.Sprintf("%v", t)` (or `fmt.Sprint(t)`) inside `t.String()`.
`%v` dispatches back into `String()` and the stack overflows.

Fix: format a field or convert to an unnamed underlying type first
(`type raw T; fmt.Sprintf("%v", raw(t))`), so the format call does not re-enter
`String()`.

### Side effects in String()

Wrong: a `String()` that logs, increments a counter, or takes a lock. `fmt` calls
it often, from many goroutines, so the side effect leaks into every log line and
can deadlock or race.

Fix: compute purely from the receiver and return. Nothing else.

### Assuming String() covers serialization

Wrong: implementing `String()` and expecting JSON and the database to use it.
`json.Marshal` and `driver.Valuer` do not call `String()`; the raw `iota` integer
goes over the wire and into the column.

Fix: implement `MarshalText`/`MarshalJSON` and `driver.Valuer`/`sql.Scanner`
explicitly, sharing the same name table as `String()`.

### Persisting the integer, then reordering constants

Wrong: storing the enum's `iota` value, then inserting or reordering constants in
a later release. Every historical row silently changes meaning.

Fix: store the stable string name and parse it back; treat unknown names as a
typed error.

### A secret with String() but no LogValue()

Wrong: a credential type that redacts in `String()` but has no `slog.LogValuer`.
Structured logging can still surface the cleartext, and any `%v` on the containing
struct leaks the exported field.

Fix: implement `LogValue()` returning `slog.StringValue("REDACTED")` (and redact
in `String()` too), so no path prints the secret.

### Empty string for the unknown case

Wrong: `return ""` for an unrecognized enum value. The log line is blank and the
corruption is invisible.

Fix: return `TypeName(N)` so the bad value is diagnosable.

### Expecting String() to change %d or %x

Wrong: assuming a `Stringer` prints its name under every verb. `%d`, `%x`, `%t`
ignore `String()` and show the underlying value.

Fix: know which verbs dispatch to `String()` (`%s`, `%v`, `%q`) and keep verbs
consistent in log formats.

### Hand-writing a huge switch that drifts

Wrong: a fifteen-arm `switch` maintained by hand, which falls out of sync when a
constant is added.

Fix: `//go:generate stringer` plus a CI guard test that fails when a declared
constant is missing from the generated table.

### Formatter where Stringer suffices, or width logic in String()

Wrong: implementing `fmt.Formatter` for a type that needs only one
representation, or putting width/precision handling in `String()` where `fmt`
ignores it.

Fix: use `String()` for the single human form; escalate to `fmt.Formatter` only
when verbs, width, precision, and flags genuinely matter.

Next: [01-status-enum-stringer.md](01-status-enum-stringer.md)
