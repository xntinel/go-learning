# Embedding for Composition — Concepts

Embedding is the single mechanism Go gives you for reuse-by-composition, and it
is the one senior engineers reach for constantly in production infrastructure:
an HTTP server that extends `http.Server`, access-log middleware that wraps
`http.ResponseWriter` while preserving `Flusher` and `Hijacker`, an
observability handler that extends `slog.Handler`, a repository decorator that
wraps an interface to add metrics or retry, a family of domain models that share
a `BaseModel`. Embedding looks like inheritance and is emphatically not
inheritance. Treating it as inheritance is where the sharp edges cut: nil
embedded pointers panic, a `sync.Mutex` embedded by value makes your struct
non-copyable and `go vet` flags every copy, diamond embedding produces
ambiguous-selector compile errors, JSON silently flattens embedded fields, and —
the one that surprises people most — promoted methods do not participate in
virtual dispatch, so a base method never calls your override. This file is the
model. Read it once and you have what you need for the ten independent artifacts
that follow.

## Concepts

### Promotion: what an embedded field contributes

An embedded field is a field declared with a type but no name: `struct{ *http.Server }`
rather than `struct{ srv *http.Server }`. The field is still there — its name is
the unqualified type name (`Server`), so you can always reach it explicitly as
`s.Server`. What embedding adds on top is promotion: every exported and
unexported field of the inner type, and every method in the inner type's method
set, becomes reachable directly on the outer type as if declared there. The outer
type's method set is the union of its own methods and the promoted ones. Promotion
is defined by depth: a field or method of the outer type sits at depth 0, a member
of a directly-embedded type at depth 1, a member of a type embedded in that at
depth 2, and so on. When the same name exists at two different depths, the
shallower one wins and hides the deeper one; the deeper one is still reachable by
qualifying through the embedded field. When the same name exists at the same depth
through two different embedded fields, the selector is ambiguous and does not
promote at all.

### Value vs pointer embedding

Embedding `T` copies the inner value into the outer struct and gives the outer the
value-receiver method set of `T`. Embedding `*T` stores a pointer, shares the inner
value with whoever else holds it, and promotes both the value-receiver and the
pointer-receiver methods of `T`. The choice is not cosmetic. For a component with
shared mutable state that must be a single instance — `*http.Server`, a
`*sql.DB`, an API `*Client` — embed the pointer, because embedding the value would
copy the server and your outer methods would mutate a copy the real server never
sees. For a small immutable value component embed the value, and you get a usable
zero value for free. The trade-off with pointer embedding is that the zero value
of the outer type has a nil inner pointer, and any promoted call then panics with
a nil dereference — which is why pointer-embedding types need a constructor that
guarantees the pointer is set.

### Embedding is composition, not inheritance — no virtual dispatch

This is the concept that catches experienced engineers coming from Java or C++.
If a base type's method calls another method by name, it calls the base type's
method, always, even when the base is embedded in an outer type that redefines
that method. There is no dynamic dispatch through the embedded field. Concretely:
if `BaseValidator.Validate` internally called `b.check()`, embedding
`BaseValidator` in `CreateUserRequest` and defining `CreateUserRequest.check`
would not cause `Validate` to call your `check` — `Validate` is a method on
`BaseValidator` and only ever sees `BaseValidator`. To get "super"-like layering
you invert it: the outer type shadows the method, does its own work, and
explicitly calls the embedded field's version (`r.BaseValidator.Validate()`). The
outer is in control; the base is a component it delegates to, not a parent that
calls back down into it.

### Shadowing: extend by overriding, then delegate

When the outer type declares a method or field with the same name as a promoted
one, the outer declaration hides (shadows) the inner. The inner is still reachable
through the embedded field selector. This is the correct and idiomatic way to
extend a component's behavior: define `func (s *Server) Shutdown(ctx) error` that
logs and then calls `s.Server.Shutdown(ctx)`. The danger is shadowing without
delegating — writing a `Shutdown` that logs and returns nil silently disables the
real shutdown. Shadowing is a scalpel; forgetting the delegate turns it into a
deletion.

### Ambiguous selectors and diamond embedding

Embed two types that each expose a member of the same name at the same depth —
two mixins that both declare an `ID` field, or both implement `String()` — and the
bare selector `x.ID` or `x.String()` is a compile error: "ambiguous selector".
Neither promotes, because Go refuses to guess which one you meant. You resolve it
by qualifying through the specific embedded field (`x.AuditMixin.ID`) or, better
for consumers, by declaring an explicit member on the outer type that picks one
source and thereby shadows both promoted candidates at depth 0. This is a real
problem the moment you compose mixins from two different packages that happen to
share a field name.

### Wrapping interfaces to build decorators

Embedding an interface value (not a concrete type) is the standard way to build a
decorator: metrics, retry, logging, caching wrappers that override one method and
forward all the rest. You declare `struct{ UserRepository }`, store a real
implementation in that embedded field, override only `FindByID` to add your
behavior, and every other method of the interface is promoted straight through to
the wrapped value. You did not have to reimplement `Save`, `Delete`, and the other
twelve methods just to instrument one. The trap is that a nil embedded interface
makes every promoted call panic, so a decorator needs a constructor that rejects a
nil delegate.

### Wrapping http.ResponseWriter without breaking streaming

Access logging, metrics, and compression middleware all need to observe or modify
the response, so they wrap `http.ResponseWriter` by embedding it and overriding
`WriteHeader`/`Write` to capture the status code and byte count. Two things bite
here. First, the default-200 case: if a handler writes a body without ever calling
`WriteHeader`, the status on the wire is 200, so your recorder must initialize its
captured status to 200, not 0. Second, `http.ResponseWriter` is often more than it
looks — the concrete value also implements `http.Flusher` (for streaming),
`http.Hijacker` (for websocket upgrades), and `io.ReaderFrom`. A naive embedding
wrapper still satisfies `http.ResponseWriter` but no longer satisfies `Flusher` or
`Hijacker`, so a streaming handler that type-asserts `w.(http.Flusher)` suddenly
fails and buffering breaks. The modern fix is to give your wrapper an
`Unwrap() http.ResponseWriter` method and let handlers reach the optional
interfaces through `http.NewResponseController`, which walks the `Unwrap` chain to
find `Flush`/`Hijack` on the underlying writer.

### Extending slog.Handler for request-scoped context

Structured-logging plumbing that enriches every record with a trace id or request
id is built by embedding `slog.Handler` and overriding `Handle(ctx, record)` to
pull the id out of the context and `AddAttrs` before delegating to the embedded
handler. The subtlety is `WithAttrs` and `WithGroup`: `slog` calls those to derive
child handlers (that is what `logger.With(...)` does under the hood), and the
embedded handler's versions return the *base* handler type, discarding your
override. You must re-wrap: override `WithAttrs`/`WithGroup` so they call the inner
handler and box the result back into your type, preserving your `Handle` override
down the chain. `Enabled` can be left promoted because it has no such re-wrapping
concern.

### Embedding sync primitives widens your API and forbids copying

Embedding `sync.Mutex` or `sync.RWMutex` promotes `Lock`/`Unlock` onto the outer
type, which means callers can lock and unlock your struct's internal invariants
from outside — usually a mistake, because they can deadlock or corrupt state you
meant to encapsulate. It also makes the containing struct non-copyable: a
`sync.Mutex` must not be copied after first use, so `go vet`'s copylocks analyzer
flags any pass-by-value, slice append, map assignment, or value return of the
outer struct. A copied lock silently breaks mutual exclusion because the copy and
the original are now different locks. The default choice is therefore a named,
unexported `mu sync.Mutex` field, not an embedded one; embed the lock only when
exporting `Lock`/`Unlock` is a deliberate part of the type's contract.

### JSON and embedding: fields flatten, names can collide

`encoding/json` treats an embedded struct's exported fields as if they were fields
of the outer struct: they flatten into the same JSON object rather than nesting.
Embed `BaseModel{ID, CreatedAt}` into `User` and the JSON is
`{"id":..., "created_at":..., "email":...}` at one level, not a nested `base`
object. Struct tags on the embedded type apply. Name collisions follow Go's
shadowing rules — if the outer type declares a field that serializes to the same
JSON name at a shallower depth, the outer field wins and the embedded one is
dropped from the output. When you actually want nesting, do not embed: use a named
field with a `json` tag, which serializes as a sub-object.

### The clogged-API problem: embed only a genuine component

Every method the inner type has becomes a method on the outer type. Embed a type
with a wide API and you widen your own type's surface with methods that may make no
sense on it, confusing consumers about what the real contract is. Embed only when
the inner is genuinely a component of the outer and its API belongs there — an
"is-composed-of" relationship where the promoted methods are ones you want callers
to use. Otherwise, use a named field and expose exactly the operations you mean to.

## Common Mistakes

### Embedding a value where a pointer is required

Wrong: `struct{ http.Server }`. The outer methods mutate a copy of the server;
the original never sees the change, and the two `http.Server` values drift apart.

Fix: embed `*http.Server`. Any component with shared mutable state — a server, a
`Client`, a `sql.DB` — is pointer-embedded.

### Shadowing a promoted method without delegating

Wrong: overriding `Shutdown` with a body that logs and returns nil. The outer
method hides the real one, so the server never actually shuts down.

Fix: the outer method does its extra work and then calls the embedded field's
method: `return s.Server.Shutdown(ctx)`.

### Expecting virtual dispatch through the base

Wrong: assuming a base method will call your outer override because you "overrode"
it. Promotion has no dynamic dispatch; the base method only ever calls the base's
methods.

Fix: shadow in the outer type and call the embedded base explicitly to layer
behavior. Do not expect the base to call up into you.

### Wrapping ResponseWriter and losing Flusher/Hijacker

Wrong: a status-capturing wrapper that only embeds `http.ResponseWriter`. It no
longer satisfies `http.Flusher` or `http.Hijacker`, so streaming responses buffer
and websocket upgrades fail.

Fix: give the wrapper `Unwrap() http.ResponseWriter` and have handlers reach
`Flush`/`Hijack` through `http.NewResponseController`.

### Forgetting the default-200 in a status recorder

Wrong: initializing the captured status to 0. A handler that writes a body without
calling `WriteHeader` sends 200 on the wire but your log records 0.

Fix: initialize the recorder's status field to `http.StatusOK` and only overwrite
it when `WriteHeader` is actually called.

### Embedding sync.Mutex by value and then copying the struct

Wrong: embedding `sync.Mutex` and passing the struct by value, appending it to a
slice, or storing it as a map value. `go vet` copylocks flags it and mutual
exclusion silently breaks.

Fix: use a named unexported `mu` field, always take pointer receivers, and never
copy the struct after first use — or embed `*sync.Mutex` when sharing is intended.

### Leaving an embedded pointer or interface nil

Wrong: constructing the zero value of a pointer- or interface-embedding type and
calling a promoted method. It panics with a nil dereference in production.

Fix: a constructor that initializes (and validates) the embedded dependency, so a
correctly-built value can never have a nil embedded pointer.

### Hitting an ambiguous selector from two mixins

Wrong: embedding two types that expose the same field or method name at equal
depth and reaching for the bare selector — a compile error — or leaning on it
through reflection or JSON at runtime.

Fix: qualify through the specific embedded field, or declare an explicit outer
member that selects one source and shadows both promoted candidates.

### Overriding slog Handle but not re-wrapping WithAttrs/WithGroup

Wrong: overriding `Handle` to inject context attributes but leaving `WithAttrs`
promoted. `logger.With(...)` returns the base handler and your enrichment silently
vanishes for any logger that carries attributes.

Fix: override `WithAttrs` and `WithGroup` to re-box the inner handler's result
back into your type so the `Handle` override survives down the chain.

### Embedding just to save typing

Wrong: embedding a type only to avoid writing a few delegating methods, cluttering
the outer type's API with unrelated inner methods.

Fix: embed only a genuine component whose API belongs on the outer type; otherwise
use a named field and expose the operations you actually mean to.

Next: [01-embedded-http-server.md](01-embedded-http-server.md)
