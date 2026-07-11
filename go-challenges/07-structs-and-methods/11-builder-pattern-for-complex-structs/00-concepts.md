# Builder Pattern and Fluent Construction for Complex Backend Structs — Concepts

Complex construction is a production reliability concern, not a syntax curiosity.
When a struct has many optional fields, when defaults must be applied
deterministically, when construction can fail (an unparseable URL, a missing
credential, a contradictory pair of flags), and above all when a
partially-constructed value must never escape into the rest of the program, a
plain constructor with a growing parameter list stops being safe. A senior
backend engineer reaches for a builder or for functional options precisely to
turn construction into a *boundary*: a Request, a Client, a Config that leaves
`Build()` is guaranteed valid and fully owned by its caller. This file is the
conceptual foundation. Read it once and you have everything you need to reason
through the eight independent construction paths that follow — each one a thing
you actually ship: HTTP request and client construction, layered config loading,
SQL query assembly, and typed batch accumulation.

## Concepts

### A builder is a staging value with exactly one validation point

A builder is a mutable value that accumulates fields and enforces an invariant at
a single terminal method, conventionally `Build`. The contract is sharp:
everything that leaves `Build` is valid and fully owned; everything before `Build`
is provisional and must not be trusted. Chainable setters (`Method`, `URL`,
`Header`) are *pure mutators* — they record a field and return the builder so the
next call can chain. They never validate. Validation lives only at the terminal,
because a setter that returned an error would force the caller to check on every
link of the chain, which defeats the entire point of a fluent API. The mental
model is a two-phase object: an open, permissive accumulation phase, then one
irreversible transition through `Build` into a closed, guaranteed-valid product.

### Pointer-receiver versus value-receiver is an aliasing decision

There are two shapes of chainable setter and the choice between them is not a
style preference — it decides who shares memory with whom. A pointer-receiver
setter (`func (b *Builder) URL(u string) *Builder`) mutates one shared instance
and returns the same pointer; the whole chain operates on a single builder. A
value-receiver setter (`func (b Builder) URL(u string) Builder`) operates on a
copy and returns the copy; each call yields a new, independent builder. Pointer
builders are the default for a linear, single-use chain. Value builders exist for
one specific, valuable scenario: *forking a shared base*. You configure a base
builder once (say, with an auth header and a base URL) and every call site derives
its own request from it without mutating the shared base. That only works if each
setter returns a copy.

Either way, one rule is non-negotiable: reference-typed fields (maps, slices) must
be deep-copied, because copying a struct copies the map *header*, not the map. Two
"independent" builders that share the same underlying map are not independent at
all — a write through one is visible through the other. `maps.Clone` and
`slices.Clone` exist for exactly this.

### Copy-on-build ownership

The invariant "fully owned" means `Build` must copy every reference-typed field
into the produced value, so the product cannot be mutated through the builder's
internals and the builder cannot see later mutations of the product. Returning the
builder's own map from `Build` is a classic aliasing defect: the consumer mutates
what they think is *their* map, and a second `Build` (or the builder's next setter)
sees the change. The fix is mechanical and cheap — `maps.Clone(b.headers)` on the
way out. A cheap copy at the boundary buys a strong guarantee everywhere downstream.

### Aggregated validation with errors.Join

Failing on the first missing field forces the caller through a fix-rebuild-fix
cycle: they supply the method, rebuild, learn the URL is also missing, supply it,
rebuild again. Aggregating every problem with `errors.Join` reports them all at
once, which is strictly better developer experience, and — crucially — each joined
cause remains individually matchable with `errors.Is` against a package-level
sentinel. `errors.Join` skips nil errors and returns nil if every argument is nil,
so the idiom is: append a wrapped sentinel per failed check into a slice, then
`errors.Join(errs...)` at the end; a nil result means "valid".

### Functional options are Go's idiomatic alternative

For *constructors*, Go's community leans on functional options rather than a
mutable builder: `func NewClient(base string, opts ...Option) (*Client, error)`
where `type Option func(*config)`. Each option is a closure that sets one field.
The advantages are concrete: options are composable, they travel as first-class
values (you can store a `[]Option` and reuse it to build two independent clients),
they are order-independent for independent fields, and the zero-option call
(`NewClient(base)`) already yields a valid client because defaults are applied
before the options run. You can extend the API with a new `WithX` without breaking
any existing call site — the signature never changes. Validation still happens
once, after all options apply. The trade-off versus a builder: a long fluent chain
reads more naturally as a builder, and only a staged builder can enforce required
ordering; options shine for the "one required arg plus a bag of optionals"
constructor, which is most of them.

### Type-state builders move required fields into the type system

When a field is genuinely required and forgetting it is a real hazard, a
type-state (staged) builder encodes the requirement in the types. `New()` returns
a `needsMethod` whose only method is `Method(...)`, which returns a `needsURL`
whose only method is `URL(...)`, which finally returns a `readyBuilder` that
exposes the optional setters and `Build`. Calling `Build` before `URL` is not a
runtime error — it does not compile, because that method does not exist on that
stage's type. This trades API surface and ceremony (several interface types, more
methods) for a compile-time guarantee. It is justified for a hard ordering
invariant and is over-engineering for a struct that is all optional fields.

### Deterministic default layering with cmp.Or

`cmp.Or` (Go 1.22+) returns its first non-zero argument, which makes "environment
else file else built-in default" a one-liner: `cmp.Or(envVal, fileVal, defaultVal)`.
It is the clean way to layer sources by precedence. But it has one sharp edge: it
treats the *zero value* as "absent", so for a field where zero is a legitimate,
intended value (port 0 meaning "pick a free port", an empty string meaning
"cleared"), `cmp.Or` will silently skip an explicit setting and fall through to the
default. The disciplined answer is to distinguish "unset" from "zero" using a
pointer field or, for environment variables, `os.LookupEnv`'s `ok` boolean, which
tells you the variable was set even when its value is empty.

### Generics let one builder accumulate any element type

A batch accumulator for bulk inserts or bulk publishes is the same shape for every
element type, so it is naturally generic: `Batch[T any]` with `Add(T)`,
`AddAll(...T)`, a capacity hint via `slices.Grow`, a size guard, and a `Build()`
returning `[]T`. Ownership still matters — `Build` returns `slices.Clone` of the
accumulated slice so mutating the result does not corrupt a later `Build` or the
builder's internal buffer.

### Parameterized construction keeps untrusted data out of the text

A SQL query builder is a construction path with a security invariant. The generated
SQL *text* is assembled with `strings.Builder`, and user values never touch that
text — they go into a parallel `[]any` args slice, addressed by placeholders
(`$1`, `$2`, ... for PostgreSQL, `?` for MySQL). The output `(sql, args)` is then
safe to hand to `database/sql`'s `QueryContext`, which sends the args separately
from the statement. Concatenating even a single "trusted" column value into the
text reintroduces injection. The invariant is structural: if a value influences
the query, it does so as a placeholder plus an args entry, never as text.

### Builders are single-use and not concurrency-safe by default

A mutable builder carries accumulated state; reusing one after `Build` and being
surprised the state carried over is a self-inflicted bug. Treat a builder as
single-use unless you explicitly design a reusable/forkable value builder, and
never share one mutable builder across goroutines. A fresh `New()` per construction
is the safe default and costs nothing.

## Common Mistakes

### Validating inside chainable setters

Wrong: a `Method` setter that returns an error when the method is empty, forcing an
error check on every link and destroying the fluent chain. Fix: setters are pure
mutators; all validation happens once, in `Build`.

### Returning a value from a pointer-style setter

Wrong: a setter declared `func (b *Builder) URL(u string) Builder` (value return)
on a builder meant to be mutable — each call mutates a throwaway copy, and the
terminal builds an empty struct. Fix: return `*Builder` for mutable builders; only
return `Builder` (value) when you deliberately want copy/fork semantics, and then
deep-copy the reference fields.

### Leaking internal reference state from Build

Wrong: returning the builder's own map or slice from `Build`, so the consumer can
mutate the builder's internals (and a later `Build` sees it). Fix: copy every
reference-typed field with `maps.Clone` / `slices.Clone` at the boundary.

### Reusing a mutable builder after Build

Wrong: calling `Build`, then calling a setter again and reusing the same builder,
surprised the earlier state persists. Fix: treat a mutable builder as single-use,
or design a value/fork builder when reuse from a base is the goal.

### Using zero-value checks where zero is valid

Wrong: `cmp.Or` (or a plain `if x == 0`) for a field where zero is an intended
value (port 0, empty string), silently overriding an explicit setting with a
default. Fix: use a pointer field or `os.LookupEnv`'s `ok` bool to distinguish
"unset" from "zero".

### Failing on the first error instead of aggregating

Wrong: returning the first missing-field error, forcing multiple fix-rebuild
cycles. Fix: accumulate wrapped sentinels and `errors.Join` them, so the caller
sees every problem at once and can still match each with `errors.Is`.

### Concatenating user input into SQL text

Wrong: writing a value directly into the query string, "just this one column".
Fix: append a placeholder to the text and the value to the args slice; the value
never enters the text.

### Building an http.Client with a zero Timeout

Wrong: constructing an `http.Client` whose `Timeout` was never set — a zero
`Timeout` means *no timeout*, an unbounded hang in production. Fix: always apply a
finite default and reject a non-positive override.

### Sharing one mutable Transport across clients

Wrong: reusing a single `*http.Transport` for every built client, so tuning or
closing one affects the others. Fix: give each build its own transport instance.

### Over-engineering a type-state builder

Wrong: a staged type-state builder for a struct that has only optional fields,
inflating the API surface for no invariant gain. Fix: reserve type-state for a
genuine required-ordering invariant; use plain options or a simple builder
otherwise.

Next: [01-request-builder-with-validation.md](01-request-builder-with-validation.md)
