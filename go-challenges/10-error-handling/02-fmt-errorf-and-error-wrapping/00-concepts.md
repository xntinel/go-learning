# fmt.Errorf and Error Wrapping: Adding Context Across Backend Boundaries — Concepts

In a production Go service the error value is not a diagnostic afterthought; it is
a first-class API contract that crosses process and team boundaries. The same
value is what an on-call engineer reads in a log line at 03:00, what a middleware
maps to an HTTP status, and what a caller's `errors.Is` branch keys a
retry-versus-fail decision on. `fmt.Errorf` with the `%w` verb is the primary
tool for the single most common backend decision around errors: how much context
to add as a value travels up through the repository, service, and handler layers,
and whether to keep the underlying cause inspectable or deliberately sever it.

Get this wrong and you produce one of three failures. Opaque messages: a bare
`EOF` in the logs with no operation, no query name, no key, so nobody can tell
what was reading what. Leaky abstractions: a raw `*pq.Error` or a bare
`sql.ErrNoRows` escaping the repository and coupling every HTTP handler to the
database driver. Or broken control flow: a caller that can no longer
`errors.Is(err, sql.ErrNoRows)` because somewhere a layer used `%v` instead of
`%w`, and the 404-mapping branch silently stopped firing. A senior engineer
treats every wrap as an intentional annotate-or-translate boundary decision.

This file is the conceptual foundation. Read it once and you have everything you
need to reason through the eight independent exercises that follow, each of which
drills these decisions on a real handler, repository, config loader, health
aggregator, validator, or transactional operation — not on a syntax demo.

## Concepts

### `%w` versus `%v` is a wrap-or-annotate decision, not a formatting preference

`fmt.Errorf("...: %w", err)` returns an error whose `Unwrap() error` method
returns `err`. That single method is what makes the cause discoverable:
`errors.Is` and `errors.As` walk the chain by calling `Unwrap`. `%v`, by
contrast, flattens the cause into the message string and returns an error with no
`Unwrap` — the chain is severed and the cause is gone as a value, surviving only
as text.

So the choice is semantic. Use `%w` when a caller must branch on the cause: a
transient database error that a retry loop keys on, a `sql.ErrNoRows` a handler
maps to 404. Use `%v` (or translate to a fresh sentinel) when you deliberately
want to hide the underlying type at an abstraction boundary, for example so a
secret value or a driver type never becomes part of your package's observable
surface. Identical-looking log lines can hide opposite semantics; the verb is
the contract.

### Wrapping widens a package's public API surface, permanently

The moment you wrap a concrete error type with `%w` and return it across a package
boundary, every downstream caller can `errors.As` it out and now depends on it.
If a repository wraps a driver's `*pq.Error` with `%w`, a handler three layers up
can reach in and type-assert the driver type — and now the driver is part of the
repository's public contract, forever, whether you meant it or not. Unwrapping is
a compatibility promise. `%v` is the tool that keeps the cause private: it lets
you log the detail without exporting the type.

### Single versus multiple `%w`

`fmt.Errorf` with exactly one `%w` operand yields an error with
`Unwrap() error`. With two or more `%w` operands (Go 1.20+) it yields an error
with `Unwrap() []error` containing the operands in argument order. That second
form lets one call wrap both an operation-context sentinel and an underlying cause
at once — for example `fmt.Errorf("%w: %w", ErrDegraded, cause)`, so a caller can
`errors.Is` the result against both `ErrDegraded` and the specific cause.

### `errors.Join` accumulates independent failures

`errors.Join(errs...)` returns an error whose `Unwrap() []error` yields the
non-nil operands. It discards `nil` operands, returns `nil` if all are `nil`, and
formats as the members' `Error()` strings joined by newlines. It is the idiom for
accumulating independent failures where there is no single cause: every invalid
field of a request at once, or a fan-out to N backends where several fail. A
validator that returns `errors.Join` of one wrapped sentinel per bad field lets an
API report every problem in one response instead of one-at-a-time.

### The critical asymmetry: `errors.Unwrap` versus `errors.Is`/`errors.As`

`errors.Unwrap(err)` only calls the `Unwrap() error` form. For an `errors.Join`
result or a multiple-`%w` error — which implement `Unwrap() []error` — it returns
`nil`. `errors.Is` and `errors.As` traverse *both* forms via a depth-first tree
walk. The practical rule: you inspect a joined or multi-wrapped error with
`errors.Is`/`errors.As`, never with a manual `errors.Unwrap` loop. A hand-rolled
loop that calls `errors.Unwrap` on a `Join` gets `nil` on the first step and
wrongly concludes there is no cause.

### Message hygiene: a breadcrumb trail, not a stutter

A wrapped message should read as a trail from the outermost operation to the root
cause. Do not prefix `error:` or `failed to` at every layer; that stutters into
`failed to run pipeline: failed to run stage: failed to parse: EOF`. Each layer
adds its operation and the one salient identifier (the query name, the key, the
URL, the id) and lets the cause supply the rest. `GET /users/42: get user 42:
query getUserByID id=42: no rows` tells the whole story with no repetition.

### Wrapping a nil error is a real trap

`fmt.Errorf("op: %w", nil)` does not return `nil`. It returns a non-nil error
whose message contains the literal `%!w(<nil>)` and whose `Unwrap` returns `nil`.
So a success path that wraps unconditionally becomes a fabricated failure. Always
guard `if err != nil` before wrapping — and in the defer idiom below, the guard is
what keeps a clean exit clean.

### The named-return + defer wrap idiom

For a resource-owning function with many early returns, repeating the same wrap
prefix at each `return` is error-prone and easy to forget on one path. The idiom
that guarantees uniform context on every exit:

```go
func Do() (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("do: %w", err)
		}
	}()
	// ... many early returns of a bare err ...
}
```

The named return `err` is visible to the deferred closure, which wraps it once, on
whichever path set it. The `if err != nil` guard is not optional: without it the
success path (where `err` is `nil`) would be turned into a non-nil error
containing `%!w(<nil>)`. This is essential for transaction commit/rollback and
file-close paths.

### Boundary translation versus propagation

A repository should translate infrastructure sentinels into domain sentinels
rather than leak `database/sql` upward. Return a domain `ErrNotFound`, not the
raw `sql.ErrNoRows`, so callers depend on your domain vocabulary and not on your
storage choice. Translation means wrapping the *domain* sentinel with `%w` (so
`errors.Is(err, ErrNotFound)` matches) while deliberately not wrapping the driver
sentinel (so `errors.Is(err, sql.ErrNoRows)` is false at the boundary). You can
still keep the driver's message for debugging by rendering it with `%v` — the
string survives for the log, the type does not escape as a value.

### Custom error struct versus opaque `fmt.Errorf`

`fmt.Errorf` produces an unexported `*fmt.wrapError` you can only inspect by its
cause and message. A struct type with exported fields plus an `Unwrap() error`
method gives callers both a typed handle (`errors.As` yields the struct, so a
middleware can read `Code`) *and* participation in the chain (`errors.Is` still
finds the wrapped domain sentinel). Choose the struct when callers need
structured data — an HTTP status code, a retryable flag — and `fmt.Errorf` when
they only need the message and the cause.

## Common Mistakes

### Using `%v` where `%w` was intended

Wrong: `fmt.Errorf("query user: %v", err)` when a caller later does
`errors.Is(err, sql.ErrNoRows)`. The message looks identical in the log, but the
chain is severed and the `Is` branch silently returns false. The bug only
surfaces when a retry or a 404 mapping quietly stops firing.

Fix: use `%w` at any layer whose cause a caller must branch on.

### Wrapping a nil error through the defer idiom without a guard

Wrong: `defer func() { err = fmt.Errorf("do: %w", err) }()` with no
`if err != nil`. On the success path this fabricates a non-nil error containing
`%!w(<nil>)`.

Fix: `defer func() { if err != nil { err = fmt.Errorf("do: %w", err) } }()`.

### Inspecting a Join or multi-`%w` error with a manual `errors.Unwrap` loop

Wrong: looping `for e := err; e != nil; e = errors.Unwrap(e)` over an
`errors.Join` result and concluding there is no cause when the first step returns
`nil`.

Fix: use `errors.Is`/`errors.As`, which traverse the `Unwrap() []error` tree.

### Leaking an infrastructure error type across a package boundary

Wrong: a repository returning `fmt.Errorf("find user: %w", pqErr)` so every caller
can `errors.As` the driver type. Now the driver is part of the repository's public
contract.

Fix: translate to a domain sentinel (`%w` the sentinel), render the driver cause
with `%v` if you want it in the log.

### Stuttering context at every layer

Wrong: prefixing `failed to`/`error:` at each wrap so the final line reads
`failed to run pipeline: failed to run stage: failed to parse: EOF`.

Fix: add the operation and the one salient identifier; let the cause supply the
rest.

### Comparing `err.Error()` strings instead of using `errors.Is`

Wrong: `if err.Error() == "user not found"`. Both the test and the branch break
the moment someone edits the message.

Fix: `errors.Is(err, ErrUserNotFound)`.

### Confusing single and multiple `%w`

Wrong: using two `%w` operands when a caller expects a single `Unwrap() error`
chain (its type assertion on the unwrap now fails), or one `%w` when you meant to
carry both a context sentinel and a cause.

Fix: pick the form the caller's inspection needs; document which.

### Putting secrets or full payloads into a wrap message

Wrong: `fmt.Errorf("bad DB_PASSWORD=%s: %w", pw, ErrConfig)`. The annotated error
leaks the credential into every log that records it as it propagates up.

Fix: substitute a redacted placeholder and wrap the sentinel, so `errors.Is` still
matches but the secret never enters the string.

Next: [01-errwrap-pipeline.md](01-errwrap-pipeline.md)
