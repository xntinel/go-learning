# Designing an Error Hierarchy in a Domain Model — Concepts

In a production service the error hierarchy is a contract. It is the vocabulary
the domain core speaks, and every boundary around that core — the HTTP or gRPC
transport, the SQL repository, the retry worker, the structured logger, the API
client — has to agree on it. Get the hierarchy right and a caller can branch on a
stable *category* (`errors.Is(err, ErrUser)`) without ever naming a concrete type
or importing a driver; the transport maps that category to a status code without a
brittle type switch; the repository turns `sql.ErrNoRows` into `ErrUserNotFound`
so the core never imports `database/sql`; the worker asks "is this retryable?"
instead of guessing; and a client disconnect never becomes a spurious 500. Get it
wrong and you leak SQL text into HTTP bodies, retry permanent failures into a
storm, and break every `errors.Is` the first time a layer wraps with the wrong
verb. This file is the conceptual foundation. Read it once and you have what you
need to reason through each of the nine independent exercises that follow.

## Concepts

### The tree is a matching structure, not an inheritance structure

A hierarchy in Go is not a class tree. It is a set of sentinel values connected by
wrapping, arranged so that `errors.Is` walking a chain finds the category you ask
about. A base sentinel roots the whole tree (`ErrDomain`), a mid-level base roots
one bounded context (`ErrUser`), and the leaf categories (`ErrUserNotFound`,
`ErrUserExists`, `ErrUserInvalid`) are each wrapped with `%w` so that a leaf
matches its parent *and* the root:

```go
var ErrDomain = errors.New("domain error")
var ErrUser = fmt.Errorf("user: %w", ErrDomain)
var ErrUserNotFound = fmt.Errorf("user: not found: %w", ErrUser)
```

`errors.Is(ErrUserNotFound, ErrUser)` and `errors.Is(ErrUserNotFound, ErrDomain)`
are both true, because `%w` makes `Unwrap` return the parent at each step. There is
no "subclass of"; there is only "wraps, so `Is` walks to". A caller that only
cares "is this a user-domain problem?" checks the mid base; a caller that renders a
404 checks the leaf. The tree exists to let those two callers coexist without one
depending on the other's granularity.

### errors.Is semantics, exactly

`errors.Is(err, target)` unwraps `err` repeatedly, and at *each* node it does two
things: it compares the node to `target` with `==`, and, if the node has a method
`Is(target error) bool`, it calls that method. Either returning true short-circuits
to true. That is the whole mechanism, and two design tools fall out of it. First,
`%w` wrapping is what makes a leaf sentinel match its base: the base is literally
one of the nodes on the unwrap path. Second, a *typed* error can opt into matching
a base sentinel by defining its own `Is` method that returns true when
`target == ErrUser` — without wrapping anything. The custom `Is` must be a
*shallow* predicate: it decides whether *this* node matches `target`, nothing more.
It must never call `Unwrap` or recurse, because `errors.Is` already handles the
unwrapping; an `Is` that recurses double-traverses and can loop.

### errors.As and errors.AsType are extraction, not matching

`errors.Is` answers a yes/no question about a category. When you need to read a
field — the offending field name, the resource id, a machine-readable code — you
need the concrete error value, and that is `errors.As`. `errors.As(err, &target)`
walks the same tree and, at the first node *assignable to the target type*, sets
the target and returns true. The critical subtlety: `As` matches by
*assignability to the target's type*, not by chain position, so two different typed
errors that both implement a shared interface can both satisfy the same `As` call —
`As` returns the first one it meets in a depth-first walk.

Go 1.25 added `errors.AsType[E error](err error) (E, bool)`, the generic spelling.
Instead of declaring a zero `var target *UserError` and passing `&target`, you
write `ue, ok := errors.AsType[*UserError](err)`. It is allocation-free and reads
better. The one constraint to internalize: the type parameter `E` must itself
satisfy `error`, so you can `AsType[*UserError]` (a concrete type that implements
`error`) but you cannot `AsType[Coder]` when `Coder` is an interface that lacks an
`Error()` method — for a non-error interface target you still reach for
`errors.As(err, &c)` with a pointer to the interface.

### Sentinel vs typed vs aggregate: the trade-off axis

Three shapes of error, three sweet spots.

A *sentinel* (`var ErrUserNotFound = errors.New(...)`) is a single comparable
value. It is free of data and cheap to compare, which makes it perfect for a pure
category with nothing to say beyond its identity. Its weakness is exactly that: it
cannot carry the id or field that made it happen.

A *typed* error (`type UserError struct { UserID, Field string; ... }`) carries
structured context. Its cost is coupling: a caller that reads `UserID` must name
the concrete type. You mitigate the coupling by giving the type an `Is` method so
callers that only need the *category* can match a base sentinel and stay decoupled,
while callers that need the *data* pay the coupling deliberately with `errors.As`.

An *aggregate* is `errors.Join(e1, e2, ...)`, which builds an error whose
`Unwrap() []error` returns all the branches. `errors.Is` and `errors.As` traverse
every branch, so a joined error can match a category *and* let you enumerate the
individual failures — the right tool for many-at-once validation where you want to
report every bad field in one response, not just the first.

### Multiple %w: one wrap, two categories

Since Go 1.20 `fmt.Errorf` accepts more than one `%w`. `fmt.Errorf("save: %w: %w",
ErrUser, ErrTransient)` produces an error whose `Unwrap` returns `[]error{ErrUser,
ErrTransient}`, so the single value belongs to two categories at once and
`errors.Is` finds both. This is how a cross-cutting axis (retryable, transient)
composes with a domain category without a combinatorial explosion of sentinels: an
error can be "a user error" and "a transient error" simultaneously, and the worker
and the transport each ask their own orthogonal question.

### Boundary translation is the core discipline

The single most important rule is that each layer converts foreign errors into its
own vocabulary exactly once, at the boundary it owns. The repository is the only
place that knows `database/sql`; it turns `sql.ErrNoRows` into `ErrUserNotFound`
and a unique-constraint violation into `ErrUserExists`, so the domain core imports
neither the driver nor `database/sql`. The transport is the only place that knows
HTTP; it turns categories into status codes. The domain core, sitting in the
middle, stays free of `net/http`, `database/sql`, and `grpc`. Translation at the
boundary is what keeps the dependency arrows pointing inward.

### Category-to-status mapping lives at the edge and defaults to 500

Mapping a domain category to an HTTP status belongs at the transport edge, done
with `errors.Is` against stable categories, and it must *default to 500 with the
internal error hidden from the client but preserved for logs*. A 404, 409, or 422
carries a stable, human-safe title; anything unrecognized is a 500 whose body says
nothing more than "internal error" while the real error — which may contain SQL
text, file paths, or a stack — goes only to the log. An unmapped domain error must
never leak its `Error()` string into an HTTP response body.

### Transient vs permanent is an orthogonal axis

Retryability is not a property of a domain category; a "user not found" is
permanent, but a "user store timed out" is transient even though both are user
errors. Model retryability as its own axis: a `Retryable() bool` method, or an
`ErrTransient` base joined onto the domain error with a second `%w`. A backoff
worker then asks "is this retryable?" via `errors.As` on a `Retryable` interface or
`errors.Is(err, ErrTransient)`, and retries only when the answer is yes. It never
infers retryability from the domain category, which is how blind retries of
permanent or non-idempotent operations turn into storms.

### context.Canceled and DeadlineExceeded are control flow, not domain failures

When the client hangs up, the request's context is canceled; when a deadline
passes, it is `DeadlineExceeded`. These are signals about the *call*, not failures
of the *domain*, and they must be detected *before* category mapping. A canceled
context means the client is gone — there is no one to send a 500 to, so the right
outcome is a no-op or a 499, not a server error. A deadline means 504. If you let
these fall through into the default-500 branch you manufacture spurious server
errors and, worse, invite the caller to retry a request the client already
abandoned. When a cancellation was triggered with `context.WithCancelCause(parent,
cause)`, `ctx.Err()` still reports `context.Canceled`, but `context.Cause(ctx)`
recovers the real reason you attached — that is how you distinguish "user pressed
stop" from "circuit breaker opened".

### Machine-readable codes decouple the wire from the wording

Human-facing messages get reworded; a client that switch-matches on
`err.Error() == "user not found"` breaks the day someone edits the string. A small
`Coder` interface — `interface { Code() string }` — implemented across the
hierarchy gives every category a stable identifier (`USER_NOT_FOUND`,
`USER_EXISTS`, `USER_INVALID`) that survives rewording and wrapping. A `codeOf(err)`
helper extracts it via `errors.As` (or `errors.AsType` on the concrete type) and
defaults to `INTERNAL`, and that code is what goes on the JSON envelope and the
structured log field. The wire contract is the code, not the prose.

### Wrap once, log once

`%w` adds call-site context while *preserving identity*: the wrapped error still
`errors.Is` its original leaf. `%v` adds context but *severs identity*: the result
is an opaque string that no longer matches anything. Use `%w` to annotate as an
error crosses a boundary, and keep the annotation short — a `%w` at every single
frame produces a paragraph-long message. Then log the error at *exactly one*
boundary, the top. Logging the same error at the repository, the service, and the
handler produces three log lines for one failure and makes the real signal hard to
find. Wrap on the way up; log at the top; do neither more than necessary.

### When not to build a hierarchy

A hierarchy earns its keep only when a caller genuinely branches on a *category*.
If your service has a single kind of error, or if every caller handles each error
specifically anyway, a base sentinel is pure noise: it adds a layer of wrapping and
a `%w` for no one's benefit. Build the tree when there is a real generic handler —
a transport that maps categories to statuses, a worker that retries a class of
failures — and not before. Over-engineering an error hierarchy is as real a failure
mode as under-designing one.

## Common Mistakes

### Wrapping the base with %v instead of %w

Wrong: `fmt.Errorf("user: %v", ErrUser)`. This renders the base into a string and
severs the chain, so `errors.Is(err, ErrUser)` returns false. It is the classic
hierarchy bug and it is silent — the code compiles and the message even looks
right. Fix: wrap categories with `%w` so `Unwrap` returns the base and `errors.Is`
walks to it.

### A custom Is that unwraps or compares deeply

Wrong: an `Is(target error) bool` that calls `Unwrap` or does a recursive
comparison. `errors.Is` already unwraps; a recursive `Is` double-traverses and can
loop. Fix: make `Is` a shallow predicate that only decides whether *this* node
matches `target` (typically `return target == ErrUser`).

### Comparing to a sentinel with ==

Wrong: `if err == sql.ErrNoRows`. The comparison holds only while the error is
unwrapped; it breaks the instant any layer wraps it. Fix: `errors.Is(err,
sql.ErrNoRows)`, which walks the chain.

### Leaking driver or transport errors upward

Wrong: returning `sql.ErrNoRows`, a `*pq.Error`, or an `*http.Response` from a
domain method. That forces the core to import the driver and every caller to know
the storage engine. Fix: translate at the boundary — the repository returns
`ErrUserNotFound`, and the driver error stays inside the repository package.

### A giant type switch over concrete error types in the handler

Wrong: `switch e := err.(type) { case *NotFoundError: ...; case *ConflictError: ...}`
in the transport. It is brittle, and a new error type that no case names silently
maps to nothing (the default). Fix: `errors.Is` against stable categories, which
match through wrapping and degrade to the 500 default explicitly.

### Mapping everything to 500, or leaking the message into the body

Wrong: returning 500 for every error, or writing `err.Error()` into the response
body and shipping SQL text or file paths to the client. Fix: map known categories
to 4xx with a stable title, default the rest to 500, and put the real error only in
the log — never in the body.

### Misclassifying context errors as a domain 500

Wrong: letting `context.Canceled` / `context.DeadlineExceeded` fall into the
default-500 branch. A client disconnect becomes a fake server error and, if the
caller retries, a storm. Fix: check the context errors *first* and map them to a
client-gone / timeout outcome before any domain mapping.

### Retrying by category instead of by an explicit transient axis

Wrong: "retry all user errors" or "retry everything that isn't a validation error".
Retryability inferred from the domain category retries permanent and
non-idempotent failures. Fix: model a transient axis (`Retryable()` or
`ErrTransient`) and retry only when that axis says yes.

### Over-wrapping and double-logging

Wrong: a `%w` at every frame so the final message is a paragraph, plus a log call
at each layer so one failure emits three lines. Fix: wrap where context genuinely
helps, and log at exactly one boundary.

### errors.Join surprises with nils

Wrong: assuming validation aggregation always returns non-nil. `errors.Join(nil,
x)` drops the nil, and `errors.Join` of all-nil returns nil. Off-by-one here means
your "there were errors" check misfires when some checks pass. Fix: build the slice
of non-nil violations, `errors.Join` it, and treat a nil result as "valid".

### Assuming errors.As matches by position

Wrong: expecting `errors.As` to return the outermost or a specific error. It
matches by *assignability to the target type*, first in a depth-first walk, so two
typed errors sharing an interface can surprise you. Fix: target the most specific
type you actually need, and remember `As` returns the first assignable node.

Next: [01-sentinel-error-hierarchy.md](01-sentinel-error-hierarchy.md)
