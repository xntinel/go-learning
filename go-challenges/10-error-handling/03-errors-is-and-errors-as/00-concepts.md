# errors.Is and errors.As Across Backend Boundaries — Concepts

In a layered backend service — transport, handler, service, repository, driver —
errors are the primary control-flow signal that crosses every boundary. Each
layer receives errors it did not create and must decide what they mean: is this
a "not found" that becomes a 404, a transient failure worth a retry, a duplicate
that should be acknowledged and dropped, or an unknown fault that becomes a 500
and a page? `errors.Is` and `errors.As` are the two verbs that let a layer
inspect an error without knowing which layer produced it and without coupling to
the layer below. `Is` answers a question of identity — "is there an occurrence of
this specific kind anywhere in the error?" — and `As` answers a question of
payload — "can I recover the typed value some layer attached, so I can read its
fields?". A senior engineer uses these two to translate driver-specific errors
into domain errors at the repository edge, to map domain errors to HTTP or gRPC
status at exactly one boundary, to decide retry/ack/nack/DLQ dispositions in a
worker, and to emit bounded-cardinality observability labels — all without
leaking implementation details upward and without ever comparing errors with `==`
(which breaks the instant any layer wraps). Read this file once and you have the
model for the ten independent exercises that follow.

## Concepts

### Is answers identity, As answers payload

`errors.Is(err, target)` asks: is there an occurrence of this specific kind —
this sentinel, this semantic identity — anywhere in `err`? You use it for control
flow on kind: `if errors.Is(err, ErrNotFound)`. `errors.As(err, &target)` asks: is
there an error in `err` whose concrete type is assignable to `*target`, so I can
read its fields? You use it when you need data off the error — a code, an HTTP
status, a `Retryable()` verdict, a set of structured fields. If all you need is a
yes/no on kind, reach for `Is`. If you need to pull a payload, reach for `As`.

### Both walk a tree, not just a chain

The thing `Is` and `As` traverse is not a linear chain but a tree: it is `err`
plus everything reachable from it via an `Unwrap() error` method (single parent)
*and* via an `Unwrap() []error` method (many children), traversed depth-first.
`errors.Join(e1, e2, e3)` produces the `Unwrap() []error` form, so `Is` and `As`
find sentinels and types nested inside a joined aggregate. This is why validation
that reports every failing field at once still works with `Is`: each field error
is a leaf of the join tree. One trap: the free function `errors.Unwrap(err)` is
*not* the same as this traversal. It follows a single `Unwrap() error` one level
and returns `nil` for a joined error — it does not descend a `Join` tree. Use
`errors.Is`/`errors.As` (or a type switch on `Unwrap() []error`) to inspect
multi-error trees; use `errors.Unwrap` only to peel exactly one wrapping layer.

### Equality for Is is `==` or a custom `Is` method

When `errors.Is` visits a node, it matches if the node `== target`, or if the
node implements `Is(target error) bool` and that method returns true. A sentinel
matches by pointer identity: `var ErrNotFound = errors.New("not found")` is a
unique value, and every occurrence of it in the tree compares equal to the one
package-level variable. A custom error type can override matching by implementing
`Is` — the service error in Exercise 1 matches any target `*ServiceError` with the
same `Op`, so `errors.Is(err, &ServiceError{Op: "Get"})` is true for any wrapped
`Get` failure regardless of the underlying cause. A custom `Is` must return false
(never panic) for target types it does not recognize.

### As matching is assignability or a custom `As` method

When `errors.As` visits a node, it matches if the node's concrete type is
assignable to the type `*target` points at, or if the node implements
`As(target any) bool` and that method returns true. The target must be a non-nil
pointer to a concrete type that implements `error`, or a non-nil pointer to any
interface type; otherwise `As` panics. The interface-target form is the powerful
one: `var r interface{ Retryable() bool }; errors.As(err, &r)` extracts *anything*
in the tree that implements `Retryable()`, which is behavior-based classification
rather than type-based. Exercise 5 uses exactly this to decide retries without
naming a concrete error type.

### A custom `As` method can synthesize a typed view

An error's `As(target any) bool` method lets it hand back a typed value it does
not structurally contain. A driver-level error can implement `As` so that, when
asked for a `**APIError`, it *builds* a fresh public-facing `*APIError` — mapping
its raw driver code to a stable public code, a safe public message, and a
retryability verdict — and assigns it to the target. The caller gets a clean typed
error and never learns the driver type. This is the half of the `errors` API the
original version of this lesson named but never demonstrated; Exercise 6 closes
that gap.

### Translate at boundaries, once

The discipline that keeps a layered service decoupled is: translate errors at the
edges, not everywhere. At the repository edge, convert driver sentinels
(`sql.ErrNoRows`) into domain errors (`ErrUserNotFound`) with `fmt.Errorf(..., %w)`
so that no layer above the repository ever imports `database/sql` to detect
absence. At the handler edge, map domain errors to transport status
(`ErrNotFound` becomes 404) in one place. Scattering `errors.Is` checks through
every layer, or letting a lower layer's sentinel escape to the transport layer,
couples layers that should not know about each other. Inspect where you must
decide; translate where the abstraction changes.

### Wrapping is why Is/As exist and what `==` breaks

The instant any layer does `fmt.Errorf("adding context: %w", err)`, the returned
error is no longer `==` the sentinel it wraps — but `errors.Is` still finds the
sentinel because `%w` records it as the wrapped child. That is the whole point:
`Is`/`As` exist so that adding context does not destroy the ability to inspect
kind and payload. The corollary is that every layer must preserve the chain: use
`%w`, not `%v`. `fmt.Errorf("getUser: %v", err)` flattens the error to a plain
string and severs the tree; `Is` and `As` above that point find nothing.

### Join aggregates without a wrapping message

`errors.Join(errs...)` returns a single error whose `Error()` is the inputs joined
by newlines, whose `Unwrap() []error` exposes the inputs, and which is `nil` when
every input is `nil`. That nil-when-all-nil property makes it the natural return
of a validator that must report every failing field instead of stopping at the
first: collect one error per failure, `return errors.Join(errs...)`, and the
caller gets `nil` on success or an aggregate that `Is`/`As` can walk. A single
`fmt.Errorf` with multiple `%w` verbs likewise produces a multi-error tree.

### Context cancellation vs deadline are distinct sentinels

`context.Canceled` and `context.DeadlineExceeded` are two different sentinels with
two different operational meanings: the first is the client going away
(disconnected, gave up) and warrants no alert; the second is the server exceeding
its own time budget and usually does. They are frequently wrapped by `net/http`,
`database/sql`, and your own layers, so you must classify them with `errors.Is`,
never with `==` on `ctx.Err()` and never by matching on the error string (which
changes across Go versions and wrappers).

### errors.AsType is the generic form of As (Go 1.26)

`errors.AsType[E error](err error) (E, bool)` is the return-value form of `As`
added in Go 1.26. Instead of declaring a pointer temporary and passing its
address, you write `ve, ok := errors.AsType[*ValidationError](err)` and get the
matched error back directly. It obeys exactly the same tree traversal and the same
custom-`As` rules; it just reads cleaner at extraction call sites, especially when
a function extracts more than one typed error. The docs recommend preferring
`AsType` for most uses. Exercise 10 refactors a classic `As` call site to it.

## Common Mistakes

### Comparing wrapped errors with `==`

Wrong: `if err == ErrNotFound`. This fails the moment any layer wraps the sentinel
with `%w`, and in production something always wraps. Fix: `if errors.Is(err, ErrNotFound)`.

### Passing a wrong-shaped target to As

Wrong: `errors.As(err, se)` where `se` is a `*ServiceError` value (not its
address), or a pointer to a type that does not implement `error`. Both panic. Fix:
pass `&se` where `se` is `*ServiceError`, or a pointer to an interface type.

### Forgetting Unwrap on a custom wrapper

Wrong: a custom error type that stores a wrapped error but has no `Unwrap() error`
(or `Unwrap() []error`) method. The tree stops at that node, and `Is`/`As` cannot
see anything it wraps. Fix: implement `Unwrap`.

### Leaking lower-layer sentinels upward

Wrong: returning `sql.ErrNoRows` from a repository straight to an HTTP handler.
Now the transport layer imports `database/sql` and is coupled to the storage
engine. Fix: translate to a domain error at the repository boundary and return
that.

### Using %v instead of %w when adding context

Wrong: `fmt.Errorf("getUser: %v", err)`. `%v` renders the error as a string and
severs the chain; `Is`/`As` above find nothing. Fix: use `%w` on the error you
want to remain inspectable.

### First-failure-wins validation

Wrong: returning on the first invalid field, forcing the user through one
round-trip per mistake. Fix: collect one error per failing field and
`errors.Join` them so the response lists every problem at once.

### Assuming errors.Unwrap traverses a Join

Wrong: calling `errors.Unwrap` on a joined error and expecting to reach its
children. `errors.Unwrap` follows only a single `Unwrap() error` and returns `nil`
for a `Join`. Fix: use `errors.Is`/`errors.As`, or type-switch on `Unwrap() []error`.

### Matching ctx errors by `==` or by string

Wrong: `if ctx.Err() == context.DeadlineExceeded`, or matching on
`strings.Contains(err.Error(), "deadline")`. Both break under wrapping and across
versions. Fix: `errors.Is(err, context.DeadlineExceeded)` and
`errors.Is(err, context.Canceled)`.

### Using the raw error message as a metric label

Wrong: labeling a metric or keying a map by `err.Error()`. Error strings are
unbounded, so this is a cardinality explosion. Fix: extract a bounded `Code` via
`errors.As` and label on that, with an `internal` fallback when `As` fails.

### A custom Is/As method that panics or ignores the chain

Wrong: an `Is`/`As` method that panics on an unexpected target type, or that only
checks the receiver and forgets that traversal continues into the wrapped tree.
Fix: type-assert the target, return false when it does not match, and let the
receiver's `Unwrap` expose the rest of the tree so traversal continues.

Next: [01-service-layer-is-as.md](01-service-layer-is-as.md)
