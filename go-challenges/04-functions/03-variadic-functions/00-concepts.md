# Variadic Functions in Backend APIs — Concepts

Almost every "open set" API a backend engineer touches every day is variadic:
`slog`'s key-value attribute pairs, `database/sql`'s query arguments, a
middleware chain, `errors.Join`, `fmt.Sprintf`. The syntax is trivial — `...T`
is a slice — so the junior story ends at "you can pass any number of arguments".
The senior story is entirely about the operational consequences: what allocates,
what aliases the caller's memory, what the type checker stops protecting you
from, and when the shape is simply wrong for the job. This file is the model you
carry into all ten exercises that follow; read it once and each exercise becomes
an application of one idea here.

## Concepts

### `...T` is exactly a `[]T` — there is no array and no magic

Inside the function body, a variadic parameter `parts ...string` has the static
type `[]string`. Nothing is special about it there: you range it, index it, pass
it on, take its length. The only special part is the *call site*. Given
`func f(parts ...string)`:

- `f("a", "b", "c")` makes the compiler synthesize a fresh backing array holding
  the three arguments and pass a slice header over it. This is a real
  allocation, every call.
- `f(sl...)` — "splatting" an existing `[]string` — passes `sl`'s slice header
  *directly*. No new backing array, no copy of the elements. The callee's `parts`
  and the caller's `sl` alias the same memory.

Those two facts drive everything below. The homogeneous-list ergonomics you get
for free at the call site are paid for either in an allocation (the first form)
or in aliasing risk (the second).

### Every non-splatted variadic call allocates

Because `f(a, b, c)` builds a backing array, a variadic function invoked in a
tight loop allocates once per iteration. On a request's hot path — a
serialization routine, a repository building millions of keys, a logging
fast-path — that per-call slice is not free. The senior pattern is to expose a
slice-parameter *core* that does the work and keep the variadic form as a thin
public wrapper:

```go
func JoinSegments(parts []string) string { /* the real work */ }
func JoinSegmentsV(parts ...string) string { return JoinSegments(parts) }
```

Callers who already hold a slice call the core and pay nothing extra; callers who
want the sugar call the wrapper. You do not guess which matters — you measure it
with `testing.B` and `b.ReportAllocs()`, and the measured allocation delta is the
decision criterion for whether the slice overload is worth the extra surface.

### Splatting aliases — the classic append-corruption bug

Because `f(sl...)` shares `sl`'s backing array, a callee that appends to its
variadic parameter *within the existing capacity* writes into the caller's array.
The caller never called append and never expected its data to change, yet a
downstream element silently mutates. This is the single most dangerous property
of variadics and the source of a whole class of production bugs:

```go
// defaults has len 2 but cap 4 (it is a sub-slice of a larger array)
func Merge(defaults []string, extra ...string) []string {
	return append(defaults, extra...) // writes into defaults' backing array!
}
```

If `defaults` was `full[:2]` of a four-element `full`, that `append` overwrites
`full[2]`. The fix is to copy first — `slices.Clone(defaults)` (or `make`+`copy`)
— before appending or mutating, so the callee owns its own array. The same rule
applies to mutating elements in place: `parts[i] = strings.ToUpper(parts[i])`
inside a variadic function rewrites the caller's slice when the caller splatted.

### `...any` erases the type checker

`...any` (once spelled `...interface{}`) is the ergonomic-but-unsafe end of the
spectrum, and it is everywhere: `fmt.Printf(format, args...)`,
`slog.Logger.With(kv...)`, `db.QueryContext(ctx, query, args...)`. It lets these
APIs accept a heterogeneous list, but it removes static type safety: the compiler
cannot tell that a `%d` verb got a string, or that a query expects an `int` where
you passed a `time.Time`. Correctness shifts from the type checker to two other
places: your tests, and `go vet`'s analyzers. `go vet` ships a `printf` analyzer
(it catches `fmt.Sprintf("%d", "oops")`) and a `slog` analyzer (it catches
mismatched or odd key-value pairs). Enabling them turns a class of runtime
surprises back into build-time errors, which is why every forwarding wrapper over
`...any` must be guarded by both.

### The key-value `...any` convention requires even arity — and fails quietly

`slog.With(k1, v1, k2, v2, ...)`, `logr`, and `zap`'s sugared logger all encode
attributes as alternating key-value pairs in a single `...any`. The keys are
supposed to be strings and the arity is supposed to be even. When it is not —
you pass a lone trailing argument — `slog` does *not* panic or return an error.
It assigns the orphan to the reserved key `!BADKEY`:

```
l.With("a", 1, "orphan").Info("odd")
// {"level":"INFO","msg":"odd","a":1,"!BADKEY":"orphan"}
```

That silent degradation is the failure mode to internalize: a malformed log line
still emits, just with a garbage key, so a dropped value can hide in production
for weeks. Design and test around it (assert the `!BADKEY` behavior once so you
recognize it), and run the `go vet` slog analyzer to catch the mismatch before it
ships.

### Variadic-of-functions is the idiomatic pipeline shape

When the "same kind of thing" is itself a function, a variadic expresses a
composable pipeline: validation rules `rules ...func(T) error`, a middleware chain
`mw ...Middleware`, option appliers `opts ...Option`. You iterate the slice and
apply each element; the *iteration order is your API contract* and must be
documented and tested. A middleware chain that applies `mw[0]` outermost is a
different, observable contract from one that applies it innermost — a spy that
records execution order is the honest way to pin it down.

### Aggregating a variable number of failures with errors.Join

`errors.Join(errs ...error)` is the variadic aggregator for multi-error reporting.
Two properties make it compose cleanly with a rule loop: it *discards nil errors*,
and it *returns nil if every argument is nil*. So a validation pipeline can append
each rule's result (nil or not) and hand the whole slice to `errors.Join`; the
result is non-nil exactly when at least one rule failed, and `errors.Is` walks the
joined tree to find each individual sentinel. This is how a handler reports every
invalid field at once instead of bailing on the first — a real UX difference for
an API client.

### The zero-argument case is a real case

`f()` with no variadic arguments yields a *nil* `[]T` inside the function.
Ranging over nil is a no-op, `len` is zero, but indexing `parts[0]` panics. Every
public variadic API must behave sanely with no arguments — a cache-key builder
called with no segments returns the bare namespace, a middleware chain with no
middleware returns an equivalent handler — and that path must be tested, not
assumed away.

### Variadic vs. a struct or functional options

Variadic is right for a *homogeneous open set*: any number of the same kind of
thing (segments, ids, rules, middleware). It is wrong for *distinct-meaning*
parameters. `func NewUser(name, email string, admin bool)` should never become
`func NewUser(args ...any)` — the three arguments mean different things, and a
variadic throws away both the names and the types. Distinct parameters belong in
a regular signature, a config struct, or the functional-options pattern. The test
is semantic: if you would struggle to name the parameter as a plural noun ("the
segments", "the rules"), it is not a list and should not be variadic.

### `database/sql` placeholders exist for injection safety

The reason `db.QueryContext(ctx, query, args...)` takes a `...any` of arguments
separate from the query string is *injection safety*. You build the query with
`?` (or `$1`) placeholders and pass the values as a splatted `...any`; the driver
binds them out-of-band, so a value containing `'; DROP TABLE users; --` is data,
never SQL. Building a dynamic `IN (...)` clause therefore means generating exactly
one `?` per id and forwarding a matching `[]any` — the placeholder count must
always equal `len(args)`, or the driver rejects the statement with a "wrong number
of arguments" error at runtime. Never interpolate values into the query text.

## Common Mistakes

### Using variadic for two or three distinct-meaning parameters

Wrong: `func NewUser(args ...any)` to pass name, email, and an admin flag. The
arguments have different meanings and different types; a variadic discards both.
Fix: a regular signature or a struct/options. Variadic is for homogeneous lists.

### Splatting a slice one element at a time in a loop

Wrong: `for _, seg := range segs { b.String(seg) }` builds N separate keys and
throws away all but the last. Fix: `b.String(segs...)` — splat the whole slice in
one call.

### Appending onto a splatted variadic slice and corrupting the caller

Wrong: `return append(defaults, extra...)` inside a function whose caller passed a
sub-slice with spare capacity — the append overwrites the caller's backing array.
Fix: `slices.Clone(defaults)` (or `make`+`copy`) before appending, so the callee
owns its memory.

### Mutating variadic elements in place

Wrong: `parts[i] = strings.ToUpper(parts[i])` inside a variadic function surprises
a caller who splatted a shared slice. Fix: copy first if the function must mutate.

### Passing an odd number of key-value args to slog

Wrong: `l.With("a", 1, "orphan")` emits a field under `!BADKEY` instead of failing.
Fix: keep pairs balanced and enable `go vet`'s slog analyzer to catch it at build.

### String-concatenating values into a SQL IN clause

Wrong: `"id IN (" + strings.Join(strValues, ",") + ")"` — injection and type bugs.
Fix: emit one `?` per value and forward a `...any` arg slice to `QueryContext`.

### Generating a placeholder count that does not equal len(args)

Wrong: deriving the `?` list and the arg slice from different lengths, so the
driver reports "wrong number of arguments". Fix: derive both from the same length.

### Assuming a variadic on a hot path is free

Wrong: ignoring the per-call slice allocation of a variadic in a tight loop. Fix:
benchmark with `b.ReportAllocs()` and expose a slice-parameter core when it counts.

### Returning on the first validation failure when the caller wanted all

Wrong: `for _, r := range rules { if err := r(v); err != nil { return err } }`
loses every failure after the first. Fix: run all rules and `errors.Join` them.

### Forgetting the empty variadic case

Wrong: assuming at least one argument and indexing `parts[0]`, which panics on
`f()`. Fix: handle and test the zero-argument path (nil slice, `len == 0`).

Next: [01-cache-key-builder-variadic.md](01-cache-key-builder-variadic.md)
