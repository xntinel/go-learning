# Type Assertions and Type Switches in Production Backends — Concepts

Type assertions are where Go's static type system meets the dynamic edges of a
real backend. JSON decoded into `any`, an error tree you did not construct, a
`http.ResponseWriter` three middleware wrappers deep, a `driver.Value` row coming
out of a SQL cursor: at every one of these seams you hold a value as a wide
interface and need to recover what it concretely is. The construct `x.(T)` is the
tool, and a senior engineer treats every use of it as a runtime branch that can
panic, mis-fire on a typed-nil interface, or silently swallow a case a missing
`default` never covered. This file is the conceptual foundation for the nine
independent exercises that follow; read it once and you have the model you need to
reason through each one.

## Concepts

### The two forms, and when each is defensible

A type assertion has two forms and they fail very differently. The comma-ok form
`v, ok := x.(T)` never panics: if the dynamic type of `x` is not `T`, then `v` is
the zero value of `T` and `ok` is `false`. This is the only form permitted on any
value whose dynamic type is not statically guaranteed — anything crossing a
boundary you do not control.

The single-return form `v := x.(T)` panics with a `*runtime.TypeAssertionError`
the instant `x` does not hold a `T`. It is defensible only when a prior invariant
makes the type certain: immediately after a successful comma-ok, or on a value you
constructed yourself two lines up. When you do use it deliberately, put it behind
a `Must*` name so the panic is a documented contract, not an accident. Every other
use on external input is a latent crash: a single hostile or merely-unexpected
value takes down the goroutine.

### Asserting to an interface is a capability probe

`x.(io.Writer)` does not test concrete identity. It tests whether the dynamic type
of `x` implements `io.Writer`. This is the "interface upgrade" pattern: you hold a
value as a narrow interface and ask, at runtime, whether it also satisfies a wider
optional one. `f, ok := w.(http.Flusher)` on an `http.ResponseWriter` is the
canonical example — probe for a capability that only some concrete writers have.
The same shape recovers `http.Hijacker`, `io.ReaderFrom`, `fmt.Stringer`, or
`encoding.TextMarshaler` off a value you only hold as `any`.

Under the hood, an interface value is a pair: a type descriptor (the `itab`,
which also carries the method table for that concrete-type/interface pairing) and
a data word. A concrete-type assertion is a pointer comparison on the type
descriptor; an interface assertion is an `itab` lookup (is there a method table
for this concrete type satisfying that interface). Both are cheap individually,
but "cheap" is not "free": in a hot loop over a homogeneous slice, assert once
outside the loop, not once per element.

### The type switch is ordered assertions with a bound variable

`switch v := x.(type) { case A: …; case B: … }` is a sequence of assertions
evaluated top to bottom; the first matching case binds a `v` of that case's type.
Two rules bite in production. First, ordering matters when cases overlap: a
concrete type and an interface it satisfies can both match, so the more specific
case must come first or it is unreachable. Second, in a multi-type case like
`case int, int64:`, `v` is *not* narrowed — it keeps the switch's original static
type (`any`), because the compiler cannot pick one of the listed types for it. You
still need a nested assertion to use it as a concrete number.

`nil` has its own `case nil`, which matches an interface holding no value at all.
And a type switch with no `default` silently does nothing on an unmatched type. In
a dispatcher that is a dropped event with no error and no log — the unknown input
becomes invisible. A production `default` should return a typed error or a defined
fallback so unknown inputs are observable.

### The typed-nil trap

An interface value is nil only when *both* its type word and its data word are
nil. Put a typed nil pointer into an interface and the type word is set, so the
interface is not nil. A function whose signature returns `error` but that returns a
`(*MyError)(nil)` returns a non-nil error: `err != nil` is true, and callers see a
failure that never happened. Worse, a comma-ok extraction `p, ok := err.(*MyError)`
*succeeds* with `p == nil`, so guarding the interface is not enough — you must
guard the pointer too. The fix at the source is to return the untyped `nil`
literal on success, never a nil typed pointer widened to the interface.

### errors.As is the correct extractor, not a raw assertion

A raw `e := err.(*ValidationError)` inspects only the outermost error. The moment
anything wraps it with `fmt.Errorf("...: %w", err)` the assertion misses (and in
the single-return form, panics). `errors.As(err, &target)` walks the `Unwrap`
chain and assigns the first error whose dynamic type is assignable to
`*target`'s element type. It is the tool for typed-error extraction through a wrap
chain, and because its type test is done reflectively it can also match an
interface target — something a plain assertion cannot express generically.
`errors.As` panics if `target` is not a non-nil pointer to a type implementing
`error` or to any interface; that panic is a programmer error, not a runtime input
error, so it is acceptable. Its sibling `errors.Is` walks the same chain comparing
against a sentinel value.

### The closed sets you must switch over exactly

Two stdlib boundaries hand you a value as `any` drawn from a fixed, closed set,
and knowing the set exactly is what keeps the switch total.

`encoding/json` unmarshals into `any` as exactly six shapes: `map[string]any` for
an object, `[]any` for an array, `string`, `float64` for *every* number (integer
or not), `bool`, and `nil`. There is no `int` case; an `int` arm in a JSON walker
is dead code and integers silently fall through to `default`. Match `float64` and
narrow. (If you truly need integer fidelity, `json.Decoder.UseNumber` gives you
`json.Number` instead — a distinct opt-in shape.)

`database/sql/driver.Value` is `any` constrained to exactly `nil`, `int64`,
`float64`, `bool`, `[]byte`, `string`, and `time.Time`. Scanning a dynamic row
means a type switch over that closed set; anything else is a driver bug and should
hit a `default` that returns a typed error. The reverse direction is a
`driver.Valuer`: a domain type implements `Value() (driver.Value, error)` to
convert itself into one of those seven shapes.

### net.Error, and why Temporary() is the wrong retry signal

`net.Error` is `interface{ error; Timeout() bool; Temporary() bool }`. Its
`Temporary()` method is deprecated because "temporary" was never well defined —
different implementations disagreed on what it meant, so it is not a reliable retry
signal. Classify retryability instead from `Timeout()`, from
`errors.Is(err, context.DeadlineExceeded)` and `os.ErrDeadlineExceeded`, and from
explicit status codes. Recover the `net.Error` with `errors.As`, not a raw
assertion, so a wrapped timeout deep in the chain is still detected.

### http.ResponseController superseded hand-rolled writer assertions

Before Go 1.20 the way to flush a streaming response or hijack a connection was to
assert `w.(http.Flusher)` or `w.(http.Hijacker)` directly on the
`ResponseWriter`. That pattern is fragile: any middleware that wraps the writer to
capture the status code, count bytes, or buffer output breaks the assertion unless
it manually re-forwards every optional interface. Go 1.20 introduced
`http.ResponseController` (via `http.NewResponseController(w)`), which reaches the
underlying capability by walking the `Unwrap() http.ResponseWriter` convention that
well-behaved wrappers implement. `rc.Flush()` and `rc.Hijack()` succeed through a
wrapper that a raw `w.(http.Flusher)` would miss. Knowing both the old assertion
pattern and precisely why it broke is the senior point — you will still read the
old form in existing middleware.

## Common Mistakes

### Using the panic form on a boundary value

Wrong: `s := x.(string)` on a value from JSON, a `map[string]any`, a
`ResponseWriter`, or an error. Any unexpected input crashes the goroutine. Use
comma-ok and return a typed error.

### Treating a typed-nil interface as nil

Wrong: returning a nil `*MyError` from a function typed to return `error` and
expecting callers to see success. The interface is non-nil; callers see a failure
that never happened. Return the untyped `nil` literal on success, and when you must
inspect, guard the pointer with an explicit `p == nil` after the comma-ok.

### Extracting a typed error with a raw assertion instead of errors.As

Wrong: `e := err.(*ValidationError)`. It inspects only the top of the chain, so any
`%w` wrapping makes it miss, and the single-return form panics on a type mismatch.
Use `errors.As(err, &target)`, which traverses the whole `Unwrap` chain.

### Adding an int case to a JSON type switch

Wrong: a `case int:` arm over a value from `encoding/json`. Every JSON number
decodes to `float64`, so the arm is dead code and integers fall to `default`. Match
`float64` and convert.

### Relying on net.Error.Temporary() to decide retries

Wrong: `if ne.Temporary() { retry() }`. It is deprecated and unreliable. Base the
decision on `Timeout()`, context-deadline errors, and explicit status codes.

### Hand-asserting http.Flusher inside a wrapping middleware

Wrong: `w.(http.Flusher)` where `w` is a wrapper that captured the status code and
did not re-forward `Flusher`. The assertion fails even though the base writer can
flush. Use `http.NewResponseController(w)` and the `Unwrap` convention.

### Omitting the default in a dispatch switch

Wrong: a dispatcher type switch with no `default`, so an unhandled event type is
dropped with no error and no log. Return a typed `ErrUnhandled` so the miss is
observable.

### A type assertion per element in a hot loop

Wrong: asserting `x.(T)` on every element of a slice known to be homogeneous.
Assert or convert once outside the loop instead.

### Assuming a multi-type case narrows the variable

Wrong: `case int, int64, float64:` and then using `v` as a number. In a multi-type
case `v` keeps the interface type, so you still need a nested assertion or a further
switch to reach a concrete numeric value.

Next: [01-plugin-loader-type-switch.md](01-plugin-loader-type-switch.md)
