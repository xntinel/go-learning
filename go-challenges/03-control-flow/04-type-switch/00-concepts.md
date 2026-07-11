# Type Switches for Runtime Type Dispatch in Backend Systems — Concepts

A type switch is the load-bearing dispatch mechanism at every boundary where a
backend crosses from untyped to typed data. JSON or YAML decoded into `any`;
database driver values arriving as `any`; error values that must be classified
before a retry decision; log attributes rendered by a handler; heterogeneous
commands routed by a worker. In each case the program holds an interface value
whose dynamic type it must discover at run time and branch on. The type switch
is the idiom Go gives you for exactly that: `switch v := x.(type)` inspects the
dynamic type of the interface value and selects a branch, binding `v` to the
matched concrete type inside that branch.

The mechanism is simple; the failure modes are not, and most of them are ones
the compiler will never catch. A type switch does not unwrap wrapped errors, so
classifying a `fmt.Errorf("...: %w", inner)` value with a bare switch silently
misroutes to `default`. Interface cases (`case error`, `case fmt.Stringer`)
match many concrete types and are order-sensitive. Adding a new variant at a
boundary is silently absorbed by `default`, because Go has no exhaustiveness
checking for type switches. And pointer-versus-value receiver method sets change
which interface cases a value satisfies. Read this file once; every exercise
that follows is one real production artifact — a decoder, a redactor, a
`sql.Scanner`, a retry classifier, config coercion, a `slog` handler, a command
router, an error-to-HTTP mapper, a numeric normalizer — and each drills one of
these failure modes at the exact place it appears on the job.

## Concepts

### Value form versus plain form

`switch v := x.(type)` binds `v` to the matched case's concrete type inside each
branch: in `case string:` the variable `v` is a `string`; in `case int64:` it is
an `int64`. That is the form to use when the branch body needs the typed value.

There is one subtlety that trips people up. When a single case lists multiple
types — `case int, int64:` — the body cannot statically know which of them
matched, so `v` keeps the original interface type (`any`) inside that branch, not
`int` or `int64`. If you need the concrete value there you must re-assert it. Use
the plain form `switch x.(type)` (no `v :=`) when the body does not need the
typed value at all, for example when you only want to classify a kind and route.

### No fallthrough

Type switch cases do not fall through, and writing `fallthrough` inside a type
switch is a compile error. Each `case` is an isolated branch. This is different
from an expression `switch`, where `fallthrough` is legal. The consequence is
liberating: a matched branch runs exactly its own body and nothing else, so you
never accidentally leak into the next case's logic.

### A type switch is NOT errors.As

This is the single most common production bug involving type switches, so it
gets its own section. A type switch inspects the dynamic type of the interface
value *directly*; it does not traverse a wrap chain. A value produced by
`fmt.Errorf("dial failed: %w", inner)` has dynamic type `*fmt.wrapError`, not the
dynamic type of `inner`. So this looks right and is wrong:

```go
switch e := err.(type) { // BUG on wrapped errors
case *net.OpError:
	// never reached if err was wrapped by fmt.Errorf
	_ = e
default:
	// wrapped *net.OpError silently lands here
}
```

`case *net.OpError` never matches a wrapped `*net.OpError`; it hits `default` and
the retry logic misroutes. To classify errors across a wrap chain you must use
`errors.As` (which unwraps until it finds a value assignable to the target type)
or `errors.Is` (which unwraps comparing against a sentinel). Reserve the bare
type switch for values you already unwrapped, or for sentinel-level dispatch on
values you know are not wrapped.

### Interface cases match by satisfaction, not identity

`case error`, `case fmt.Stringer`, `case io.Reader` match *any* concrete type that
implements the interface, not one specific type. They are order-sensitive
relative to each other and relative to concrete cases: the first matching case
wins. If `case error` precedes `case *MyError`, then a `*MyError` value (which
implements `error`) is absorbed by `case error` and the specific `*MyError`
handling never runs. List the most specific cases first. A concrete case and an
interface it satisfies may both appear, and ordering decides the winner —
overlapping interface types in cases are *not* a compile error, though duplicate
concrete types are.

### The nil case and the typed-nil trap

`case nil` matches only when the interface value itself is nil — both its type
and its value are nil. A non-nil interface value that happens to hold a nil
pointer, such as `(*T)(nil)` boxed into an `any`, does *not* hit `case nil`. It
hits `case *T` with `v` being a nil `*T`. If that branch then dereferences `v`,
it panics. This is the typed-nil trap: an interface holding a typed nil is not
the nil interface. Guard pointer branches against a nil pointer before
dereferencing.

### No exhaustiveness checking

Go does not verify that a type switch covers every variant a boundary can
produce. If a new concrete type starts arriving — a new command struct, a new
decoded shape — it silently routes to `default`. Unlike a sealed union in some
other languages, the compiler gives you no completeness guarantee. The
discipline is: always write an explicit `default` that returns a typed error or
logs, so a new variant fails loudly instead of being swallowed. Never rely on
"default can't happen."

### Method sets and receivers decide interface matching

Whether a value hits an interface case depends on pointer-versus-value
receivers. A value of type `T` satisfies an interface only if every method in
that interface is declared on a value receiver; `*T` satisfies interfaces whose
methods are on value *or* pointer receivers. So a `T` value passed where the
interface needs a pointer-receiver method does not match the interface case and
falls through — silently. When your interface cases are not matching a type you
expected, check whether you handed the switch a value where a pointer was
required.

### Cost model

A type switch compiles to a chain of type comparisons and interface-table (itab)
lookups, roughly linear in the number of concrete cases (large switches may be
hashed by the compiler). It is not free, but it is far cheaper than reflection
via `reflect.TypeOf`. When the set of types is closed and known at compile time,
prefer a type switch over reflection; reach for reflection only when the type set
is genuinely open or you must operate structurally on arbitrary types.

### Boundary discipline

Decode or scan *once*, at the edge, into typed Go values, and keep the
`any`-typed zone as thin as possible. Two fixed vocabularies anchor the edges. On
the JSON side, `json.Number` with `Decoder.UseNumber` preserves integer
precision that `float64` would silently destroy for values beyond 2^53. On the
database side, `database/sql/driver.Value` is exactly one of a fixed set — `nil`,
`int64`, `float64`, `bool`, `[]byte`, `string`, `time.Time` — and a `sql.Scanner`
must type-switch over precisely that vocabulary and reject anything else with a
loss-of-information error.

### Type switch versus polymorphism

A type switch centralizes dispatch: it is open to new call sites but closed to
new types — adding a type means editing every switch. An interface method (the
visitor pattern) distributes dispatch: it is open to new types but closed to new
operations — adding an operation means editing every type. Choose per the axis
along which you expect change. If new types arrive often but the operation set is
stable, prefer a method on an interface. If the type set is fixed but you keep
adding operations over it, prefer type switches. Senior code documents which
trade-off it took and why.

## Common Mistakes

### Type-switching on a wrapped error

Wrong: `switch e := err.(type) { case *net.OpError: ... }` on an error that was
wrapped by `fmt.Errorf("...: %w", ...)`. The dynamic type is `*fmt.wrapError`, so
the concrete case is skipped and the value lands in `default`.

Fix: use `errors.As(err, &target)` for concrete types and `errors.Is(err,
sentinel)` for sentinels; both unwrap the chain. Reserve the bare type switch for
already-unwrapped or sentinel-level values.

### A broad case before specific ones

Wrong: putting `case any` (or an interface case that everything satisfies) before
the specific cases. The switch becomes a no-op that always takes the broad
branch.

Fix: order cases most-specific first; put `default` last.

### Assuming case nil catches a typed nil

Wrong: expecting `case nil` to catch a non-nil interface wrapping a nil `*T`. It
hits `case *T` with a nil pointer, and the handler then dereferences and panics.

Fix: guard the pointer branch (`if v == nil { ... }`) before dereferencing, or
normalize typed nils at the boundary.

### Decoding JSON without UseNumber

Wrong: decoding JSON into `any` without `Decoder.UseNumber`, so a large integer
arrives as `float64` and is truncated before the `case json.Number` branch ever
runs.

Fix: call `dec.UseNumber()` so integers arrive as `json.Number` and dispatch on
that; convert with range checks.

### Relying on default never firing

Wrong: assuming `default` is dead code after you added a new variant elsewhere.
There is no exhaustiveness check, so new types are silently swallowed.

Fix: make `default` return a typed error naming the unexpected `%T`, and cover it
with a test.

### Using a multi-type case variable as one of its types

Wrong: in `case T1, T2:` treating the bound variable as `T1`. It is still the
interface type there and will not compile if used as `T1`.

Fix: split into separate cases when the body needs the concrete value, or
re-assert inside the shared branch.

### Retaining a []byte handed to sql.Scanner.Scan

Wrong: storing the `[]byte` you received in `Scan(src any)` directly. The driver
owns that memory and reuses it on the next `Scan`, so your value mutates
underneath you.

Fix: copy the bytes (`append([]byte(nil), b...)` or `bytes.Clone`) before
retaining them.

### Ordering an error interface case before a concrete case

Wrong: `case error:` before `case *MyError:`. Since `*MyError` implements
`error`, it is absorbed by `case error` and the specific handling never runs.

Fix: list the concrete `case *MyError` before the broad `case error`.

Next: [01-polymorphic-json-event-decoder.md](01-polymorphic-json-event-decoder.md)
