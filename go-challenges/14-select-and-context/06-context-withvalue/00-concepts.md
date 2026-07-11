# Context WithValue: Request-Scoped Data Across API and Process Boundaries — Concepts

A `context.Context` carries two entirely different payloads that happen to share
one type. The first is cancellation: a tree of `done` channels wired by
`WithCancel`/`WithTimeout`/`WithDeadline`, closed when a request is aborted or
times out. The second is values: an immutable linked list of key-value pairs
built by `WithValue`. They ride the same parent chain but solve different
problems, and conflating them is the source of most context abuse. This lesson
is about the second payload used well: the small set of genuinely cross-cutting,
request-scoped facts — trace/correlation ID, authenticated principal, tenant,
locale, idempotency key — that must be reachable from every layer of a request
without being threaded through every function signature, and about the precise
line between that and the domain arguments that must never live there.

The godoc states the rule the whole lesson orbits: "Use context Values only for
request-scoped data that transits processes and APIs, not for passing optional
parameters to functions." Every exercise is one layer where that data actually
lives — the `net/http` middleware that mints it at the edge, the `slog.Handler`
that reads it so every log line is correlated for free, the `RoundTripper` and
server middleware that serialize it across a process boundary, the repository and
authz guards that consume it — and every Common Mistake below is a way senior
engineers still get it wrong in production.

## Concepts

### Two payloads, one parent chain

`WithValue(parent, key, val)` returns a new context node whose only job is to
answer one more key. It does not touch cancellation; a `WithValue` node created
above a later `WithCancel` node still resolves in the cancellable child, because
`Value` walks *up* the parent chain and finds the value node regardless of what
kind of nodes sit between them. This is why the idiomatic order — set request-id
and principal at the edge, then derive a `WithTimeout` per downstream call — works:
the values set before the timeout are still visible inside it. The value list and
the cancellation tree are independent concerns sharing one spine.

### ctx.Value is an O(depth) walk, not a map

Every `ctx.Value(k)` is a linear walk up the parent chain: each node compares its
own key to `k` with `==` and, on a miss, delegates to its parent. There is no map
and no caching. A context with a dozen scalar values stacked by a dozen
`WithValue` calls costs a dozen comparisons per lookup, on every lookup, on a hot
path. The fix is structural: store one composite struct (a `Meta` with
`TraceID`, `UserID`, `Locale`) under one key, not twelve scalars under twelve
keys. One `WithValue`, one node, one comparison — and one place to evolve the
shape of request metadata.

### Keys must be an unexported named type

`Value` compares keys with `==`. If two packages both stored a value under the
string `"user"`, the second would silently read (or shadow) the first — a
cross-package collision with no compile error and no runtime panic, just wrong
data. The defense is not a "unique enough" string; it is making collision
*structurally impossible*. Define an unexported named type in your package —
`type key struct{}` is the canonical choice: zero width, zero comparison cost, and
unnameable and unconstructable from any other package. No other package can pass
your `key{}` to `WithValue` or `Value`, so no other package can read or overwrite
your slot. A string or an `int` key cannot offer that guarantee.

### The accessor pair hides the key and never panics

The idiomatic shape is a pair of functions per value: `WithX(ctx, v) ctx` to
attach and `xFrom(ctx) (X, bool)` to read. The package owns the key; callers never
see it, never write a naked `ctx.Value(k).(T)`, and never risk a panic. The
accessor uses the comma-ok type assertion — `v, ok := ctx.Value(key{}).(X)` — so
absence returns the zero value and `false` instead of panicking. This one
discipline eliminates the most common context crash: a bare `.(T)` assertion that
panics when the key is missing, or when a colliding string key holds a value of
another type.

### The boundary rule as an operational test

There is a mechanical way to decide whether a value belongs in the context or in
the signature: imagine deleting it. If deleting the value would change the
program's *correctness* — a different order gets created, a query returns the
wrong rows — it was a required function parameter in disguise and must be passed
explicitly. If deleting it would only degrade *observability* — logs lose a trace
ID, an audit line loses the actor — it is legitimately request-scoped and belongs
in the context. `productID` fails the test and must be a parameter. `traceID`
passes and belongs in context. This is the single most useful heuristic in the
lesson, and the refactor exercise makes a codebase obey it.

### Values must be effectively immutable and goroutine-safe

The context package performs *zero* synchronization on stored values. A context is
routinely shared across the goroutines that a single request fans out into, and
they all read the same stored value concurrently. So the value must be safe for
simultaneous use: store value types (a struct of strings) or a deeply-immutable
snapshot, set once on the way in and only read thereafter. Storing a shared
`*mutable` pointer and writing to it from several goroutines is a data race the
context API will not save you from — it does no locking on your behalf. "Set once
at the edge, read everywhere downstream" is not just a style; it is what makes the
absence of locking safe.

### Layering and shadowing never mutate a parent

`WithValue` is purely additive: it never mutates its parent, and a second
`WithValue` with the *same* key does not overwrite the first — it shadows it for
descendants only. The parent, and any sibling branch derived from the parent
before the second call, keep the original value. This is precisely what makes
middleware layering safe: request-id middleware wraps the context, then auth wraps
that, then locale wraps that, and each layer's addition is visible downstream
without any layer being able to corrupt what an earlier layer or a sibling request
observes.

### Values die at the process boundary

A context does not serialize. It is an in-memory linked list of Go values; it
cannot travel over a socket. The instant a request crosses an HTTP or gRPC call,
every context value is gone on the other side. To carry a trace ID across the
boundary you must copy it out of the context into a request *header* on the client,
and re-hydrate it from the header into a fresh context on the server. This copy-out
/ copy-in is the entire reason distributed-tracing propagators (W3C
`traceparent`, B3) exist: they are the serialization format for the context values
that cannot cross a process edge on their own. Forgetting this is why a trace ID
silently vanishes at the first network hop.

### slog.Handler.Handle receives the context on purpose

`slog.Handler.Handle(ctx, record)` takes a context as its first argument for one
reason: so a handler can enrich every log record from request-scoped values. A
custom handler that wraps a JSON handler and, in `Handle`, pulls the trace and user
out of `ctx` and calls `record.AddAttrs` before delegating, makes every log line in
a request carry its correlation attributes automatically — no `logger.With(...)`
threaded through call sites, no fields passed by hand. This is the sanctioned,
high-leverage use of `WithValue` in modern Go observability, and it is the
production payoff that justifies putting a trace ID in the context at all.

## Common Mistakes

### A built-in-typed key

Wrong: `context.WithValue(ctx, "user", u)` or `WithValue(ctx, 0, u)`. Any other
package using the same string or int collides silently — no error, just one
package reading another's data. Fix: an unexported named type per package
(`type key struct{}`), so no other package can name or construct the key.

### Storing domain arguments in the context

Wrong: stashing `productID`, `amount`, or a query filter in the context and
fishing it out with `ctx.Value("productID").(string)` deep in a handler. This
creates a hidden dependency (the function's real inputs are invisible in its
signature) and panics the day a caller forgets to set it. Fix: apply the boundary
rule — if removing it breaks correctness it is a parameter; pass it explicitly.

### Storing a shared mutable pointer

Wrong: `WithValue(ctx, k, &State{})` and then mutating `*State` from several
request goroutines. The context does no locking, so this is a textbook data race
that `-race` will flag. Fix: store an immutable value snapshot, or if the state
must mutate, store a type that guards itself with its own mutex — and question
whether it belongs in the context at all.

### A naked type assertion

Wrong: `id := ctx.Value(k).(string)`. It panics when the key is absent, or when a
colliding key holds another type. Fix: always comma-ok — `id, ok :=
ctx.Value(k).(string)` — and hide it behind an accessor that returns
`(value, bool)`.

### Assuming a value crosses the network

Wrong: seeding a trace ID into a context, making an HTTP call, and expecting the
server to see it. Context values do not serialize; the server sees nothing. Fix:
copy the value into a request header on the client and extract it back into a
context on the server — that is what a propagator does.

### Piling scalars under many keys

Wrong: a separate `WithValue` for trace, user, tenant, locale, and idempotency
key. Each adds a node to the O(depth) walk and another key that could collide.
Fix: one struct under one key; evolve its fields instead of adding keys.

### Expecting a child WithValue to update a parent

Wrong: assuming that a downstream `WithValue` changes what the parent or a sibling
branch sees. Contexts are immutable; only descendants of the new node observe the
shadowed value. Fix: treat every `WithValue` as producing a strictly more-specific
child, and pass that child down explicitly.

### Using context as a dependency-injection channel

Wrong: putting a `*sql.DB`, a logger you intend to swap, or a `*http.Client` into a
context value to avoid wiring them through constructors. Context is for
request-scoped *data*, not for application wiring; dependencies belong in structs
passed at construction. Fix: inject dependencies through fields and constructors;
reserve the context for per-request facts.

Next: [01-type-safe-meta-carrier.md](01-type-safe-meta-carrier.md)
