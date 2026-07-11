# Sentinel Errors: The Contract Between Packages — Concepts

A sentinel error looks trivial: `var ErrNotFound = errors.New("not found")`.
One exported variable. But the moment a caller in another package writes
`errors.Is(err, repo.ErrNotFound)`, that variable stops being an implementation
detail and becomes part of your public API — as load-bearing as an exported
struct field or a function signature. You can no longer rename it, delete it, or
quietly change what it means without breaking every importer that branches on
it. A senior engineer treats sentinels with exactly that discipline, and spends
most of the real work not on *defining* them but on three harder questions:
where the boundary sits between the sentinels you own and the leaky
infrastructure sentinels you must translate; where the single seam is that maps
a sentinel to transport semantics (an HTTP status, a gRPC code, a
retryable-vs-terminal decision); and when a bare sentinel is the wrong tool
because the caller needs *data*, not just *identity*, and you owe them a typed
error instead. This file is the conceptual foundation; the nine exercises that
follow each build one production-shaped artifact around it, and each stands
alone.

## The model: identity, not text

A sentinel error is a package-level variable of type `error`, named `ErrXxx`,
exported, and created once with `errors.New`. Its whole value is *identity*.
`errors.New` returns a pointer to an `errorString`, and two separate calls with
the same message produce two different pointers that are not equal. So the
comparison `errors.Is(err, ErrNotFound)` works only because producer and
consumer share the *same* variable — the same pointer. That is precisely why a
sentinel must be an exported package var and not a freshly constructed error:
identity can only be shared through a stable, named, exported value.

This has a direct consequence. If a function returns
`errors.New("user not found")` fresh on every call, the caller has nothing
stable to compare against; the only thing they can do is string-match the
message, which shatters the instant anyone edits the text. The sentinel exists
so that a caller can ask "is this *that specific failure*?" without parsing
prose.

### `%w` is the load-bearing verb

Wrapping is what lets a sentinel survive being passed up through layers.
`fmt.Errorf("getUser %s: %w", id, ErrNotFound)` produces a new error whose
message adds context *and* whose `Unwrap()` chain still contains `ErrNotFound`.
`errors.Is` walks that chain and finds it. Swap the `%w` for `%v` and the
message looks identical, but the chain is now empty — `errors.Is` silently
returns `false`, and the failure is invisible because nothing crashes; a caller
that used to route a 404 now routes a 500. This is the single most common
sentinel bug in production Go, and it is undetectable by reading the log line,
because the log line looks correct. The only defense is discipline (`%w` when
you wrap an error you want callers to match) and a test that asserts
`errors.Is` on the wrapped value.

### `errors.Is` vs `errors.As` / `errors.AsType`

`errors.Is(err, target)` walks the `Unwrap` chain comparing `==` at each link,
and at each link also calls any `Is(target error) bool` method the error
defines. It answers a yes/no identity question. `errors.As(err, &target)` and
its Go 1.26 generic sibling `errors.AsType[E error](err) (E, bool)` search the
chain for a value *assignable to a type* and hand it back so you can read its
fields. The rule of thumb: `Is` when the caller only needs to know *which*
failure it is; `As`/`AsType` when the caller needs to read *data* off the
failure — a retry-after duration, a field name, a conflicting resource id.

## The three hard boundaries

### 1. Domain sentinels you own vs leaky infrastructure sentinels

`sql.ErrNoRows`, `io.EOF`, `context.DeadlineExceeded`, `fs.ErrNotExist` — these
are sentinels too, but they belong to your *infrastructure*, not your domain. If
a repository method returns `sql.ErrNoRows` straight out to its callers, every
one of those callers now imports `database/sql` and is coupled to your storage
engine. Swap Postgres for a gRPC-backed store and the sentinel changes to
something else, and every caller breaks. The boundary rule: infrastructure
sentinels stop at the adapter. The adapter catches them with `errors.Is` and
re-wraps them as your own domain sentinel
(`fmt.Errorf("getUser %s: %w", id, ErrNotFound)`), so the domain sentinel is the
only thing that crosses the seam. Done right, a caller can no longer tell
whether the data came from SQL, a file, or a network — which is the point.

### 2. One translation seam to transport semantics

A domain sentinel means nothing to HTTP or gRPC on its own. Somewhere it has to
become a `404`, a `409`, a `codes.NotFound`, or a "retry this / give up"
decision. The failure mode is scattering that mapping across every handler, so
that `ErrNotFound` maps to 404 in one place and 400 in another. The fix is a
single `classify(err)` function — the one seam where a `switch` over
`errors.Is` turns each sentinel into its status, and *only* there. Callers above
it deal in sentinels; the wire format is decided once. That seam is also where
you decide what leaks: an internal error (a disk failure, a nil deref) maps to
500 with a *generic* body, and the raw error is logged server-side, never echoed
to the client.

### 3. Sentinel vs typed error

A sentinel carries identity and nothing else. When the caller needs to *act on
data* — build a `409` with a `Location` header pointing at the conflicting
resource, respect a `Retry-After`, report which field failed validation — a
bare sentinel forces them to re-parse the error string, which is the same
fragility the sentinel was supposed to eliminate. That is the signal to reach
for a typed error matched with `errors.As`/`errors.AsType`. The two patterns are
not exclusive: a typed error can implement `Is(target error) bool` so that
`errors.Is(err, ErrDuplicate)` *and* `errors.As(err, &conflict)` both succeed on
the same value — callers that only need identity use `Is`, callers that need the
id use `As`, and neither is forced to know about the other.

## Categories via a custom `Is` method

Sometimes one sentinel should represent a *class* of failures — "any 4xx", "any
5xx" — without the caller enumerating every member. An error type can implement
`Is(target error) bool` to say "I match this category sentinel": an
`APIError{Code: 404}` returns `true` for `ErrClientError` and `false` for
`ErrServerError`. The documented contract for that method matters: it must
compare *shallowly*, a single level, and must **not** call `Unwrap` or walk the
chain itself. `errors.Is` already does the walking; a custom `Is` that also
recurses double-traverses and can report wrong matches. The method answers only
"does *this* error, ignoring what it wraps, match the target?".

## `errors.Join` and multi-error trees

`errors.Join(errs...)` combines several errors into one value that implements
`Unwrap() []error` (the slice form, distinct from the single `Unwrap() error`).
`errors.Is` and `errors.As` traverse *both* forms, so a caller can still match
any single cause inside a joined value. Two behaviors make it ideal for
accumulating validation failures: `Join` discards `nil` inputs, and `Join` of
all-`nil` (or of nothing) returns `nil`. So a validator can append one wrapped
sentinel per failing field and `return errors.Join(errs...)` unconditionally: a
clean payload yields `nil`, a dirty one yields a value that reports every
violation and still answers `errors.Is` for each. The trap: a hand-rolled
aggregate that only implements `Unwrap() error` hides all but one cause from
`errors.Is`; use `errors.Join` (or implement `Unwrap() []error`) so the whole
tree is visible.

## Cancellation vs deadline

`context.Canceled` and `context.DeadlineExceeded` are distinct sentinels with
distinct *operational* meaning, and conflating them is an incident waiting to
happen. Cancellation usually means the caller gave up cleanly (the client hung
up, a parent shut down) — no alert, no SLA breach. A deadline means time ran
out — that *is* an SLA event you count and possibly page on. Classify them with
`errors.Is`, never `==`: once any downstream library wraps `ctx.Err()`, the raw
`==` comparison fails while `errors.Is` still works. Where a cancellation reason
was attached with `context.WithCancelCause`, `context.Cause(ctx)` returns the
underlying reason even though `ctx.Err()` still reports `context.Canceled`. In a
retry loop the same distinction is safety-critical: treating `context.Canceled`
or `context.DeadlineExceeded` (or a terminal `ErrPermission`) as retryable makes
the loop hammer a dependency that is dead, forbidden, or already abandoned by
its caller.

## Exported sentinels are mutable global state

There is no `const` error in Go — an `error` is an interface value, and
interface values cannot be constants. So `pkg.ErrNotFound` is a plain package
variable, and *any* importer can execute `pkg.ErrNotFound = errors.New("oops")`
and corrupt every comparison against it, process-wide, for every other package.
There is no language-level protection; the mitigation is convention (never
reassign a sentinel) plus, for a closed set where you want real immutability, an
unexported concrete type with exported *instances* (the values are exported, the
type's mutability is not exposed). Knowing the guarantee is only a convention is
part of treating sentinels as the API contract they are.

## Common Mistakes

### Using `%v` instead of `%w` when wrapping

Wrong: `fmt.Errorf("getUser: %v", ErrNotFound)`. The message reads fine but the
sentinel is not in the chain, so `errors.Is(err, ErrNotFound)` returns `false`
and every caller that branched on it silently takes the wrong path.

Fix: `fmt.Errorf("getUser: %w", ErrNotFound)`. `%w` records the sentinel so
`errors.Is` can walk to it. Assert it with a test on the wrapped value.

### Comparing with `==` across a wrapping boundary

Wrong: `if err == sql.ErrNoRows`. This is true only for the bare, unwrapped
value and becomes `false` the instant any layer wraps it with `%w`.

Fix: `if errors.Is(err, sql.ErrNoRows)`. `Is` walks the chain; `==` sees only
the top.

### Returning a fresh `errors.New` per call

Wrong: `return errors.New("not found")` inside a method. Every call yields a new
pointer with no shared identity, so callers cannot match it and must string-scan
the message.

Fix: define one exported package var `ErrNotFound` and wrap *it* with `%w`.

### Hiding the sentinel in an unexported var

Wrong: `var errNotFound = errors.New("not found")`. Consumers in other packages
cannot name it, so cross-package `errors.Is` is impossible.

Fix: export it — `var ErrNotFound = ...` — if callers are meant to match it.

### Leaking driver/stdlib sentinels out of the domain

Wrong: a repository returning `sql.ErrNoRows`/`io.EOF` unchanged, coupling every
caller to the storage engine.

Fix: translate at the adapter — catch with `errors.Is`, re-wrap as your own
domain sentinel so the infrastructure sentinel never crosses the seam.

### A custom `Is` method that calls `Unwrap` or walks deep

Wrong: an `Is` method that recurses into wrapped errors. `errors.Is` already
walks the chain, so this double-traverses and can produce wrong matches.

Fix: make `Is` a shallow, single-level comparison against the category
sentinels only.

### Treating cancellation/deadline (or a terminal error) as retryable

Wrong: a backoff loop that retries on `context.Canceled`,
`context.DeadlineExceeded`, or `ErrPermission`, hammering a dead or forbidden
dependency.

Fix: classify with `errors.Is` and return fast on terminal sentinels and on
context cancellation; retry only the transient set.

### Expecting `errors.Is` to see through `Join` via single `Unwrap`

Wrong: hand-rolling an aggregate error with only `Unwrap() error`, then
wondering why `errors.Is` finds only one of the joined causes.

Fix: use `errors.Join` (or implement `Unwrap() []error`); `errors.Is`/`As`
traverse the slice form.

### Reaching for a sentinel when the caller needs data

Wrong: returning a bare `ErrConflict` when the caller must know *which* resource
conflicts, forcing them to parse the id out of the message string.

Fix: return a typed error (`*ConflictError{ExistingID}`) matched with
`errors.As`/`errors.AsType`; optionally give it an `Is` method so the sentinel
still matches too.

### Reassigning or mis-typing an exported sentinel

Wrong: assuming `pkg.ErrX` is immutable, or defining a sentinel as a value type
whose zero value can accidentally compare equal to an unrelated one.

Fix: never reassign a sentinel; for a closed set, use an unexported type with
exported instances so identity is stable and the type stays non-mutable.

Next: [01-sentinel-repository.md](01-sentinel-repository.md)
