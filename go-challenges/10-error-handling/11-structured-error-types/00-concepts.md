# Structured Error Types For A Validation And API Boundary — Concepts

A string error is fine for a log line and useless for a client. The moment an
error has to cross a wire — from your validation layer to a browser form, from
your repository layer to your HTTP layer, from your domain to a status code — it
stops being prose and becomes data. This lesson builds that data type and the
machinery around it: a `FieldError` with a stable machine `Code`, a dotted
`Field` path, and typed `Params`; a `ValidationError` that is a real
`errors.Is`/`errors.As` tree; a versioned JSON wire contract aligned with RFC
9457; a single error-to-status mapper; a repository translator that turns a
database unique-constraint violation into a per-field 409; and a bulk importer
that bounds its own error output under adversarial input. This file is the
conceptual foundation. Read it once and you have what you need for the nine
independent modules that follow.

## Concepts

### A structured error is data, not a string

A structured error carries four separable things: a machine `Code` (a typed
constant like `required`, `max_len`, `conflict`), a `Field`/path that locates
the failure, a human `Message`, and typed `Params` for interpolation
(`{"max": 50}`). The contract is that callers switch on `Code` and read
`Params`; they never parse `Message`. The instant a caller does
`strings.Contains(err.Error(), "too long")` you have coupled two services to an
English sentence, and the coupling breaks the day someone rewords the message or
adds a locale. The `Code` is the API; the `Message` is a rendering.

```go
type FieldError struct {
	Code    Code           // stable machine constant, never reworded
	Field   string         // dotted path: "items.2.sku"
	Message string         // human text, may change or be localized
	Params  map[string]any // {"max": 50} for interpolation
}
```

### Collect all failures, except when you must not

A form validator should return *every* failure at once. Returning only the
first error forces the client into a fix-one, submit, fix-the-next loop — a
round trip per mistake. So `Validate` accumulates a `[]*FieldError` and returns
one `ValidationError` wrapping all of them. The exception is a streaming or
batch path: validating a million-row import and returning a million errors
blows up response size and latency, so a batch needs a bounded, fail-fast
variant with a `MaxErrors` cap and a `Truncated` flag. Same error type, two
collection strategies chosen by the shape of the input.

### A ValidationError is an error tree

Go 1.20 gave the `errors` package multi-error support: a type that implements
`Unwrap() []error` (not the single-error `Unwrap() error`) participates in
`errors.Is`/`errors.As` tree walking. The walk is depth-first, pre-order over
the slice. If each `FieldError` in turn wraps a category sentinel via its own
`Unwrap() error`, then `errors.Is(validationErr, ErrRequired)` is true when *any*
field failed for that reason, and `errors.As` can pull a specific `*FieldError`
out of a deeply wrapped request. The subtle part: `errors.Unwrap` (the function)
only follows the single-error `Unwrap() error` method. It is deliberately blind
to the `Unwrap() []error` form, so `errors.Unwrap(validationErr)` returns `nil`
even though the tree is full of children. That is by design, not a bug — the
multi-error form is reached by `Is`/`As`, never by a linear `Unwrap` chain.

### errors.As / errors.AsType extract; errors.Is identifies

`errors.Is(err, sentinel)` answers "is this error, anywhere in its tree, that
specific sentinel value?" — identity. `errors.As(err, &target)` and its Go 1.26
generic sibling `errors.AsType[E](err) (E, bool)` answer "is there an error of
this concrete type in the tree, and if so give it to me" — extraction. Use `As`
to reach for `*ValidationError` or `*FieldError` across any wrap boundary; use
`Is` for sentinels. Never type-assert `err.(*ValidationError)` directly: an
assertion looks only at the top of the chain, so a wrapped error is silently
missed. `errors.AsType[*FieldError](err)` is the modern, type-safe form:

```go
if fe, ok := errors.AsType[*FieldError](err); ok {
	// fe is the first *FieldError in err's tree
}
```

### Field paths are a contract with the frontend

A bare `Field: "email"` cannot tell `customer.email` from `contact.email`, and a
frontend cannot map it to a form input. The fix is compositional dotted paths
with indices for repeated elements: `customer.email`, `items.0.qty`,
`items.2.sku`, `shipping.zip`. You build the path as you recurse — pass a base
prefix down, append `.field` for a struct member and `.N` for a slice index —
so each error maps to exactly one input on the page. Storing bare field names
and hoping to reconstruct the path later does not work; build it on the way down.

### The wire schema is a versioned API, not your struct layout

Marshaling your Go struct straight to the client leaks internal fields (the `Err`
cause, a stack, database details) and welds your public JSON to your Go field
names — rename a field in a refactor and you have silently broken every client.
Decouple them with a hand-written `json.Marshaler`: emit `{code, path, detail}`,
never the internal cause, and render `detail` from `Code`+`Params` through a
catalog so the text is stable and localizable. Wrap the aggregate in the RFC 9457
`application/problem+json` shape — `{type, title, status, errors: [...]}` — so
status, type, title, and a per-field array are standard across your services.
The public error schema is something you *version*, not an accident of struct
layout.

### Status mapping belongs in exactly one place

Handlers must not each hardcode `http.StatusUnprocessableEntity`. Put the
translation in one `StatusFromError`: `errors.As` for `*ValidationError` -> 422,
a `StatusCoder` interface (`HTTPStatus() int`) for domain errors that declare
their own status, `errors.Is` against sentinels (`ErrNotFound` -> 404,
`ErrConflict` -> 409, `ErrUnauthorized` -> 401), and an unknown error defaults to
500 *and is flagged for logging*. The default-plus-log matters: a mapping gap
must surface as a logged 500, never as a silent 200 or an unlogged 500.

### Errors cross layers by translation, not leakage

A repository must not return the raw driver error to the HTTP layer. A unique
constraint violation (PostgreSQL SQLSTATE `23505` plus a constraint name like
`users_email_key`) is a user-visible 409 conflict on a specific field, not a
500. The repository recognizes the specific SQLSTATE and constraint via
`errors.As`, looks the constraint up in a `constraint -> field` table, and
returns a `FieldError{Field: "email", Code: conflict}`. Everything it does *not*
recognize must pass through untouched, so a genuine fault (a dropped connection,
a deadlock) is never masked as a validation error. Translating too aggressively —
mapping every driver error to 409 — hides real faults just as badly as never
translating at all.

### Separate the Code from the Message

Keep the machine `Code` invariant across releases and locales; render the human
text at the boundary from `Code`+`Params` through a small catalog. This is what
makes the same error localizable (swap the catalog per `Accept-Language`) and
stable (the `Code` a client codes against never moves when marketing rewords the
copy). The `Message` stored on the struct is at most a default; the authoritative
text is produced at serialization time.

## Common Mistakes

### Returning only the first error

Wrong: `Validate` returns on the first bad field, so the client fixes one thing,
submits, and discovers the next. Fix: accumulate every `*FieldError` into one
`ValidationError` and return them all (module 01's core).

### Storing a bare field name

Wrong: `Field: "email"`, so the caller cannot tell `user.email` from
`contact.email` and the frontend cannot bind it. Fix: compositional dotted paths
with indices (`items.2.sku`), built as you recurse (module 03).

### Switching on Message text

Wrong: `if strings.Contains(err.Error(), "required")`. It breaks the moment the
wording or locale changes. Fix: always carry a typed `Code` constant and switch
on that.

### Type-asserting across a wrap boundary

Wrong: `ve, ok := err.(*ValidationError)`. A wrapped error is missed. Fix:
`errors.As(err, &ve)` or `errors.AsType[*ValidationError](err)`.

### Unwrap() error on a multi-error aggregate

Wrong: implementing `Unwrap() error` on a type that holds many errors, silently
dropping all but one from the tree. Fix: implement `Unwrap() []error` so
`errors.Is`/`errors.As` see the whole set (module 02).

### Marshaling the raw struct to the client

Wrong: `json.Marshal(validationErr)` leaks the internal `Err` cause and welds
the public JSON to Go field names. Fix: hand-write `MarshalJSON` emitting a
versioned `{code, path, detail}` shape (module 05).

### 500 for a unique-constraint violation

Wrong: letting a duplicate-email insert become a 500 (it is a user-visible 409),
or the opposite error of mapping every driver error to 409 and masking a real
fault. Fix: recognize the specific SQLSTATE/constraint and pass everything else
through (module 08).

### Unbounded error output on a huge bad batch

Wrong: returning one error per row for a million-row bad file, blowing up
response size and latency. Fix: cap with `MaxErrors` plus a `Truncated` flag and
honor context cancellation (module 09).

### A cause with no way to reach it

Wrong: a `FieldError` with an unexported cause but no `Unwrap`, so `errors.Is`
against category sentinels never matches. Fix: wire the cause into the tree via
`Unwrap` (module 02).

Next: [01-structured-field-error-validator.md](01-structured-field-error-validator.md)
