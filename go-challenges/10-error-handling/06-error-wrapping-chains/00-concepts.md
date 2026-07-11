# Error Wrapping Chains: Building, Inspecting, and Operating Wrapped Errors in Production — Concepts

Error wrapping is where a service's failure semantics actually live. When a query
fails three layers down in a repository, what the HTTP handler at the top does
about it — return 404 or 500, retry or give up, log the SQL or hide it — is
decided entirely by whether the error that bubbled up still carries the
information needed to classify it. A wrapping chain is the data structure that
carries that information. This file is the conceptual foundation for the nine
independent exercises that follow; each builds one real artifact (a migrator, a
repository, an HTTP classifier, a validator, a transactional cleanup path, a
retry loop, a semantic-matching API error, a context/timeout boundary, and an
observability boundary) where wrapping is the load-bearing mechanism.

## Concepts

### A chain is a linked list; a joined error is a tree

You build a chain by wrapping: `fmt.Errorf("loading config: %w", err)` returns a
new error whose `Unwrap() error` method exposes the error you passed. Do it again
one layer up and you have a three-link list: the outermost error is the entry
point ("migration failed in phase load"), the innermost is the root cause
(`os.ErrNotExist`). `errors.Is` and `errors.As` walk this list so you never have
to call `Unwrap` by hand.

As of Go 1.20 an error can wrap *many* children. `errors.Join(e1, e2, e3)` and a
single `fmt.Errorf` with *multiple* `%w` verbs both produce an error that
implements `Unwrap() []error` instead of `Unwrap() error`. That turns the chain
into a tree, and `errors.Is`/`errors.As` traverse it pre-order, depth-first. The
practical consequence: `errors.Unwrap` (the single-child function) returns `nil`
for a joined error, so you must inspect joined errors with `errors.Is`/`As` or a
`Unwrap() []error` type assertion — never with `errors.Unwrap`.

### Is is for identity; As is for data

`errors.Is(err, target)` reports whether any error in the tree either equals
`target` (via `==`) or has an `Is(target error) bool` method that returns true.
Use it to answer "is this failure ultimately a not-found?" — a question about
*identity* against a sentinel.

`errors.As(err, &target)` finds the first error in the tree whose concrete type
is assignable to `*target` (or that has an `As(any) bool` method) and assigns it,
returning true. Use it to *extract typed data* — the `*ValidationError` with its
`Field`, the `*MigrationError` with its `Phase`, the `net.Error` you want to ask
`Timeout()`. The target must be a pointer to an error-implementing type;
`errors.As(err, target)` without the `&`, or a target that is not such a pointer,
panics.

### Chain versus join: causal versus peer

These are different tools and the choice is semantic, not stylistic. `%w`
expresses a *single causal chain*: this failed *because of* that, which failed
*because of* that. `errors.Join` (or multiple `%w`) expresses *independent
siblings*: several things failed and none caused the others. A validator that
finds three bad fields joins three peer errors. A transactional operation whose
primary action failed *and* whose rollback then also failed wraps two causes in
one `fmt.Errorf` — both are real, neither caused the other, and dropping either
loses information an operator needs. Choose by asking whether the causes are
causal or peer.

### Boundary translation: each layer speaks its own vocabulary

The most important production discipline here is that each architectural layer
wraps-and-translates foreign errors into its own vocabulary. A repository catches
`sql.ErrNoRows` and returns a domain `ErrUserNotFound` — wrapping the original
with `%w` so it is still there for debugging, but exposing a sentinel the service
layer can depend on without importing `database/sql`. Without this, an
`errors.Is(err, sql.ErrNoRows)` check leaks into the HTTP handler and couples the
whole application to the driver; swap the driver and every layer breaks. Translate
at the boundary; the domain sentinel drives control flow, the wrapped original
serves the debugger.

### Sentinel design and why == is wrong at call sites

A sentinel is a package-level `var ErrNotFound = errors.New("not found")`. It has
a stable identity, which is exactly what `errors.Is` compares against. The trap is
that once you wrap a sentinel — `fmt.Errorf("load: %w", ErrNotFound)` — the value
you hold is the *wrapper*, not the sentinel, so `err == ErrNotFound` is false.
That is not a bug in wrapping; it is the entire reason `errors.Is` exists: it
walks the chain and compares each link, so it finds the sentinel no matter how
deeply wrapped. Never compare a possibly-wrapped error with `==`; use
`errors.Is`.

### %w is an API contract

When you export a function that wraps a sentinel, callers *will* write
`errors.Is(err, YourSentinel)` and `errors.As(err, &YourType)` against what you
wrapped. That makes the set of things you wrap part of your public API. Changing a
`%w` to a `%v`, or swapping the sentinel underneath, silently breaks every caller
that classified on it — with no compile error. Treat the wrapped cause the way you
treat a function signature.

### Custom Is/As: matching by meaning, and the shallow-compare rule

A value-typed error can implement `Is(target error) bool` to match on a field
instead of identity. An `APIError{Code: "rate_limited"}` whose `Is` compares
`Code` lets callers write `errors.Is(err, &APIError{Code: "rate_limited"})` and
match *any* wrapped API error with that code, not one specific pointer. The
non-negotiable convention: a custom `Is` (or `As`) must do a *shallow, single-level
comparison only* and must **not** call `Unwrap` itself. The `errors` package
already walks the whole tree and calls your `Is` at each node; if your `Is`
recurses, the tree is walked twice and, with a cyclic or self-referential chain,
can loop forever.

### Context and network: three distinct "gave up" signals

`context.Canceled` and `context.DeadlineExceeded` are distinct sentinels, both
reachable through wrapping, and conflating them loses a real distinction:
*canceled* means the caller aborted (map to a 499-style client-closed status, do
not retry), *deadline exceeded* means time ran out (map to 504, a retry may or may
not help). The socket-level analog is `net.Error`: extract it with `errors.As` and
ask `Timeout()`. A boundary that checks only one of these, or treats all three as
"timeout", makes wrong retry and status decisions. Wrapping preserves all three as
distinguishable; your classifier must actually distinguish them.

### Error text is a debugging surface and a security surface at once

The message you wrap with is simultaneously the thing an operator reads at 3am and
a string that can end up in an HTTP response body. Wrap with enough context to
debug — the operation, the ids — but never hand the raw chain to a client: it can
carry table names, file paths, internal hostnames, or a wrapped secret. The
production pattern is two views of one failure: the full chain logged internally
with `log/slog`, and a sanitized public error (a safe code plus a user-facing
message) extracted with `errors.As` and returned to the caller. Wrap to preserve
internal context; sanitize at the boundary.

## Common Mistakes

### Wrapping with %v instead of %w

Wrong: `fmt.Errorf("load failed: %v", err)`. The formatted string keeps the *text*
of the cause but severs the chain — the result has no `Unwrap`, so
`errors.Is`/`As` can no longer reach the cause. Use `%w` whenever the wrapped
error is meant to remain inspectable.

### Replacing the error instead of wrapping it

Wrong: `return errors.New("load failed")`. The original cause is gone; the caller
cannot classify or debug it. Wrap the cause with `%w`, do not discard it.

### Comparing a wrapped sentinel with ==

Wrong: `if err == ErrNotFound`. Once wrapped, `err` is the wrapper, not the
sentinel, so this is false. Use `errors.Is(err, ErrNotFound)`, which walks the
chain.

### Passing the wrong thing to errors.As

Wrong: `errors.As(err, myErr)` (missing `&`) or passing a target that is not a
pointer to an error-implementing type. `errors.As` panics or never matches. Pass a
pointer to the concrete type or interface: `errors.As(err, &myErr)`.

### Using errors.Unwrap on a joined error

Wrong: expecting `errors.Unwrap(joined)` to return one of the joined children. A
`errors.Join` result implements `Unwrap() []error`, and `errors.Unwrap` (which
looks for `Unwrap() error`) returns `nil`. Inspect joined errors with
`errors.Is`/`As` or a `interface{ Unwrap() []error }` type assertion.

### Leaking a lower-layer sentinel upward

Wrong: letting `sql.ErrNoRows` or an `os` error reach the HTTP layer so the
handler does `errors.Is(err, sql.ErrNoRows)`. That couples the whole app to the
driver. Translate at the boundary into a domain sentinel and classify on that.

### Over-wrapping with redundant prefixes

Wrong: `"error: failed to: could not: load config: %w"`. Each layer piling on a
vague prefix produces noisy nested messages. Add one concise context clause per
layer (the operation and its key ids) and let the chain, not the adjectives, tell
the story.

### Calling Unwrap inside a custom Is or As

Wrong: a custom `Is` that walks the chain itself. The `errors` package already
walks the tree and invokes your `Is` at each node; recursing double-walks and can
infinite-loop. Keep `Is`/`As` a shallow, single-level comparison.

### Returning the raw internal chain to the client

Wrong: `http.Error(w, err.Error(), 500)` with `err` the full wrapped chain. It can
leak table names, paths, or secrets. Sanitize at the boundary — return a public
error — and log the full chain separately with `slog`.

### Checking context.DeadlineExceeded but not context.Canceled

Wrong: treating every context error as a timeout, or checking only one sentinel.
Canceled (caller aborted) and DeadlineExceeded (deadline hit) drive different
status codes and different retry behavior. Distinguish them explicitly.

Next: [01-migration-tool-wrapping-chain.md](01-migration-tool-wrapping-chain.md)
