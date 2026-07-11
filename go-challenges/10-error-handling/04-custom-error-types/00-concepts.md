# Custom Error Types — Concepts

Strings are where error information goes to die. `fmt.Errorf("user %d not found",
id)` throws away every fact a caller might act on — which entity, which
operation, whether a retry could help, what HTTP status the transport should
emit — and forces the next layer to reconstruct that lost structure by matching
substrings. A senior backend engineer instead designs error *types* as
first-class domain values: structs that carry the field, entity, operation,
category, retry hint, and redaction rules, so that handlers, middleware, retry
loops, and log pipelines make decisions programmatically. This file is the
conceptual foundation for the nine independent modules that follow; each builds a
real production artifact (a validator, a repository translation layer, an HTTP
responder, a retry classifier, a rate-limit signal, a slog integration) so every
type-design decision stays anchored to a concrete on-the-job failure mode.

## Concepts

### An error type is a domain value, not a formatted string

The design order is: first model the struct fields around the *decisions
downstream code must make*, then write `Error()` to format them. Never the
reverse. If a retry loop must decide whether to retry, the type carries a
`Retryable() bool` or a typed category; if the transport must pick a status, the
type carries a `Status int` and a machine-readable `Code`; if a form UI must show
which fields failed, the type carries `Field` and `Rule`. `Error() string` is a
*view* of that data for humans and logs — it is the last thing you write, and it
is never the interface callers program against. A type whose only accessor is its
message is no better than the string you replaced.

### The receiver choice is load-bearing

`error` is satisfied by any type with `Error() string`, but *which* type gets that
method is a real decision. Give `Error()` a pointer receiver — `func (e
*ValidationError) Error() string` — and only `*ValidationError` satisfies `error`;
the value `ValidationError` does not. That is almost always what you want for a
struct error, because it makes identity unambiguous and makes `errors.As(err,
&ve)` (where `ve` is `*ValidationError`) target the exact type that lives in the
chain. Give `Error()` a value receiver and *both* `T` and `*T` satisfy `error`,
which quietly changes comparability and, worse, makes `errors.As[T]` against a
wrapped `*T` a silent no-match. Pick the pointer receiver by default for struct
errors; reach for a value receiver only for tiny comparable sentinel-like errors
where value identity is the point.

### Unwrap has two shapes, and both are traversed

`Unwrap() error` exposes a single parent — the linear chain a repository builds
when it wraps a driver error. `Unwrap() []error` (Go 1.20+) exposes multiple
children — the *tree* that `errors.Join` and an aggregate validation error build.
`errors.Is` and `errors.As` traverse *both* forms depth-first, which is exactly
what makes a joined error searchable: `errors.Is(joined, ErrRequired)` finds the
sentinel no matter which branch holds it. A type implements one or the other, not
both. Choose `Unwrap() []error` when the error genuinely aggregates several
independent failures (collect-all validation); choose `Unwrap() error` for the
common wrap-one-cause case.

### %w wraps, %v severs

`fmt.Errorf("get user: %w", cause)` preserves the chain: the returned error's
`Unwrap()` yields `cause`, so `errors.Is`/`errors.As` can still find anything
inside it. `fmt.Errorf("get user: %v", cause)` flattens `cause` into a string and
returns an error with *no* `Unwrap` — the chain is severed and the underlying
sentinel or typed error is unreachable. Wrapping with `%w` is the mechanism that
lets a repository return a typed `RepositoryError` while still letting a caller
`errors.Is(err, sql.ErrNoRows)`. Using `%v` where you meant `%w` is a silent,
common, and untested-for bug.

### Custom Is and As define semantic equality — a sharp tool

A type may implement `Is(target error) bool` to define what "matches" means
beyond pointer identity: a `RepositoryError` whose `Is` returns true when the
target is a `RepositoryError` of the same `Kind` lets `errors.Is(err, ErrNotFound)`
succeed for *any* not-found error, not just one specific instance. A type may
implement `As(target any) bool` to project itself onto a different target type.
Both are powerful and both are easily abused: an `Is` that matches too broadly
(any instance of the type regardless of field) silently swallows unrelated errors
in every `errors.Is` check downstream. Default to a plain struct plus wrapping,
and add a custom `Is`/`As` only when a *category* match is genuinely the semantic
you want. When you do write `Is`, make its match predicate as narrow as the
intended equality.

### Translate at boundaries; do not leak infrastructure sentinels

`sql.ErrNoRows`, a driver's constraint code, an `fs.PathError` — these are
infrastructure facts. They must not travel past the architectural boundary that
owns them. The repository edge translates `sql.ErrNoRows` into a domain
`Kind==NotFound` (while still wrapping the original so `errors.Is` keeps working);
the transport edge translates a domain error into an HTTP or gRPC status. A
`sql.ErrNoRows` check in an HTTP handler couples your transport layer to your
database driver — change the store and the handler breaks. The custom error type
is the *vehicle* for that translation: it carries the domain category up, and it
carries the wire representation down.

### Separate the wire representation from the internal cause

An `APIError` sends a stable, machine-readable `Code` and a safe `Message` to the
client while keeping the wrapped internal error for the logs. Clients must never
receive leaked internal strings — no SQL fragments, file paths, stack details, or
"connection refused to 10.0.3.14:5432". The responder writes `Code`/`Message` to
the body and logs the full chain server-side. This split is not cosmetic: it is a
security boundary. A type that puts the internal cause in the JSON body is an
information-disclosure bug wearing the costume of a helpful error message.

### errors.AsType[T] is the Go 1.26 successor to errors.As

`errors.AsType[E error](err error) (E, bool)` returns the typed value directly:
`if ae, ok := errors.AsType[*APIError](err); ok { ... }`. No pointer target, no
`**T` dance, no reflection, compile-time-checked, and zero-allocation and several
times faster on typical chains. Prefer it in new code. `errors.As` is retained for
three real cases it still serves: a target that is a *non-error* interface
(`interface{ Temporary() bool }`), reusing one preallocated target across a
measured hot loop, and a target type known only at run time. `AsType` also
eliminates a whole class of bug — passing the wrong pointer level to `errors.As`
is a runtime panic that simply cannot be written with the generic form. Because it
is Go 1.26, guard the version and keep `errors.As` as the fallback for older
toolchains.

### Error types double as observability contracts

Implementing `slog.LogValuer` lets the error decide its own structured, redacted
representation. `LogValue() slog.Value` returning a `slog.GroupValue` of `op`,
`entity`, `kind`, `request_id` means every log line gets typed fields the pipeline
can index, and — critically — the type can *omit* a sensitive value (a token, a
password) from both `LogValue()` and `Error()` so the secret never reaches the
sink. The error type decides what is safe to say about itself. Redaction is not
the logger's job; it is the error's.

### Error construction is not free

Allocating a rich struct, plus any wrapping, costs. On a hot path — an inner loop,
a per-request validation that runs millions of times — that allocation shows up.
Sentinels (`var ErrNotFound = errors.New(...)`) are allocated once, cheap, and
comparable; typed structs are informative but allocate on every construction.
Choose per call-site frequency: sentinel for the ubiquitous, expected,
carries-no-data case; typed struct where the carried data earns its allocation;
and preallocate or pool where profiling says it matters. "Always return a rich
typed error" is as wrong as "always return a string".

### The typed-nil trap

A non-nil `error` interface holding a nil concrete pointer is still non-nil. A
constructor written as `func Validate() error { var errs *ValidationErrors; /* no
failures */ return errs }` returns an interface whose *type* is `*ValidationErrors`
and whose *value* is nil — and `err != nil` at the call site is unexpectedly true,
because an interface is nil only when both its type and value are nil. The fix is
to return an untyped `nil` explicitly when there are no failures, never a typed-nil
pointer. This is the single most common way a "collect all errors" validator ships
a bug that reports success as failure.

## Common Mistakes

### Value receiver, then expecting errors.As into *T to succeed

Wrong: `func (e ValidationError) Error() string`, then `var ve *ValidationError;
errors.As(err, &ve)`. The concrete type wrapped in the chain is whatever you
constructed; if you constructed `ValidationError` (a value) the `*T` target never
matches, and if you constructed `*ValidationError` the value receiver still makes
`AsType[ValidationError]` a silent miss. Fix: use a pointer receiver and construct
`&ValidationError{...}` so the type in the chain and the `errors.As` target agree.

### Formatting the data into a string and discarding it

Wrong: `return fmt.Errorf("field %s is required", name)`. The caller can only
string-match. Fix: `return &ValidationError{Field: name, Rule: "required", Err:
ErrRequired}` so the field and rule are programmatically readable and the sentinel
is `errors.Is`-checkable.

### %v instead of %w when wrapping

Wrong: `fmt.Errorf("query: %v", err)`. The chain is severed; `errors.Is(result,
sql.ErrNoRows)` is false even though the cause was `sql.ErrNoRows`. Fix: use `%w`.

### Comparing custom errors with ==

Wrong: `if err == &ValidationError{Field: "Host"}`. The error is wrapped and/or a
different pointer, so `==` is always false. Fix: `errors.Is(err, target)`, backed
by a narrow custom `Is` when you want a category match.

### Returning a typed nil from a collect-all validator

Wrong: `var errs *ValidationErrors; ...; return errs` with zero failures returns a
non-nil interface. Fix: `if len(errs.Errs) == 0 { return nil }` — return an untyped
nil.

### A custom Is that matches too broadly

Wrong: an `Is` that returns true for any instance of the type regardless of field
or kind. Every `errors.Is(err, someTargetOfThatType)` then succeeds and swallows
unrelated errors. Fix: match on the discriminating field (`Kind`, `Field`) only.

### Leaking the internal cause into the HTTP body

Wrong: `json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})` where
`err` wraps a SQL error. Fix: write a safe `Code`/`Message`, keep the internal
error for the server log only.

### sql.ErrNoRows checked in the handler

Wrong: `if errors.Is(err, sql.ErrNoRows) { w.WriteHeader(404) }` in the transport
layer. Fix: translate to a domain `Kind==NotFound` at the repository boundary and
have the handler switch on the domain kind.

### Wrong pointer level to errors.As

Wrong: `errors.As(err, ve)` (not a pointer) or `errors.As(err, &&ve)` — a runtime
panic. Fix: pass `&ve` where `ve` is a `*T`, or use `errors.AsType[*T](err)`, which
removes the pointer-level decision entirely.

### Reflecting a secret into logs or responses

Wrong: `Error()` or `LogValue()` that includes a token or password field. Fix:
omit the sensitive field from both surfaces; the error type is responsible for its
own redaction.

Next: [01-validationerror-structured-type.md](01-validationerror-structured-type.md)
