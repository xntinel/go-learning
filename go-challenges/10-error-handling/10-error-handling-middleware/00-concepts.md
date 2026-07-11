# Error Handling Middleware in an HTTP Server — Concepts

Every Go HTTP service needs one place that turns Go errors and panics into HTTP
responses. Scatter that logic across handlers and you get the failures that page
on-call: half-written responses, duplicated log lines, internal SQL strings
leaking to clients, a background goroutine panic that takes the whole process
down, a `superfluous response.WriteHeader call` in the logs. The fix is a
deliberate discipline: handlers *return errors* (and, only for genuine
programmer bugs, panic), and exactly one boundary — the middleware stack —
converts those errors and panics into a stable client contract. This file is the
conceptual foundation. Read it once and you have the model behind all ten
independent exercises that follow: the error-returning handler adapter, sentinel
mapping, panic recovery with the `ErrAbortHandler` carve-out, ordered middleware
composition, RFC 9457 problem+json, a status-capturing `ResponseWriter`,
deadline-to-504 mapping, the goroutine panic trap, request-body bounding, and
log-once-at-the-boundary observability.

## Concepts

### The middleware is the Go/network boundary

`net/http` gives you `http.Handler` — `ServeHTTP(w, r)` — a signature with no
error return. Idiomatic Go says a function that can fail returns an `error`. The
tension between those two facts is the whole subject of this lesson. The
resolution is an adapter: define your own `Handler = func(w, r) error`, let every
handler return an error instead of writing its own failure response, and wrap it
in one function that runs the handler and, on a non-nil error, writes the single
canonical error response. That adapter plus the middleware around it *is* the
boundary between your Go code and the network.

The reason this matters is the *single-writer discipline*. An HTTP response has
exactly one status line and one header block, committed the instant something
calls `WriteHeader` (or the first `Write`, which implies `WriteHeader(200)`).
After that the status is frozen: a second `WriteHeader` is ignored and logged as
`superfluous response.WriteHeader call`, and any attempt to change headers is
silently dropped. If a handler writes a partial success and then hits an error
and the boundary also writes, the client gets a corrupt response. Concentrating
all response-writing in one boundary is what guarantees exactly one writer.

### errors.Is / errors.As traverse the wrap tree

Handlers are usually thin; the real failure happens several calls deep in a
repository or client. `fmt.Errorf("load user %s: %w", id, ErrNotFound)` wraps a
sentinel while adding context, and every layer above can add its own `%w`. The
boundary never sees the sentinel directly — it sees a chain. `errors.Is(err,
ErrNotFound)` walks that chain and reports true if the sentinel is anywhere in
it, so the mapping from sentinel to HTTP status survives arbitrarily deep
wrapping. `errors.As(err, &target)` does the same for a *typed* error, letting
you pull a `*json.SyntaxError` or `*http.MaxBytesError` out of the middle of a
chain and branch on its fields. Go 1.20 added multi-`%w` (`fmt.Errorf("%w and
%w", a, b)`) and `errors.Join`, which build error *trees* rather than chains;
`Is`/`As` traverse trees too. Go 1.26 adds the generic `errors.AsType[E](err)
(E, bool)`, a typed extraction that needs no pre-declared target variable. The
one rule that makes all of this work: classify with `errors.Is`, never `==`. A
bare `err == ErrNotFound` fails the moment anyone wraps the sentinel, and the
request falls through to a default 500.

### Panic recovery belongs at the outermost layer — with a carve-out

A panic is Go's signal for a programmer bug: a nil-map write, an out-of-range
index, an impossible invariant. In an HTTP server an unrecovered panic unwinds
the goroutine and, without a recover, crashes the process — one bad request kills
every in-flight request on that instance. So the stack needs a `Recoverer`
middleware that `defer`s a `recover`, logs the value plus `debug.Stack()`, and
writes a 500. It must sit *outermost*, because a panic thrown by an inner
middleware (a logger, an auth check) must also be caught; recovery placed below
that middleware would miss it.

The subtlety that separates a toy recoverer from a correct one is the carve-out.
`net/http` deliberately uses two sentinel panics as control signals, and a
blanket recover that converts them to a 500 corrupts the server's semantics.
`http.ErrAbortHandler` is panicked to abort a handler and close the connection
*without* logging — used, for example, by reverse proxies when the upstream dies.
`http.ErrHandlerTimeout` is the value `http.TimeoutHandler`'s writer returns after
the deadline. A correct `Recoverer` re-panics `http.ErrAbortHandler` (and treats
`http.ErrHandlerTimeout` the same) instead of turning it into a normal 500, so
`net/http`'s intentional connection-abort behavior is preserved.

### recover only catches panics on its own goroutine

This is the production failure mode that surprises people. `recover` is
goroutine-local. It catches a panic that unwinds *the same goroutine* it is
deferred on. If a handler does `go doWork()` and `doWork` panics, that panic
unwinds a *different* goroutine — the `Recoverer`'s deferred recover never runs,
and the panic propagates to the top of that goroutine and crashes the whole
process. The `Recoverer` protecting the request goroutine gives zero protection
to goroutines the handler spawns. Background work needs its own `recover` at the
top of the goroutine's function — a `safeGo` helper that wraps the work, recovers,
and reports the panic through a logger or error channel. Fire-and-forget
goroutines without this are a latent process-killer.

### A status-capturing ResponseWriter must preserve Unwrap

Access logging and metrics need the response status and byte count, but
`http.ResponseWriter` does not expose them after the fact. The standard move is to
wrap the writer in a small recorder that intercepts `WriteHeader` (to record the
code) and `Write` (to record the code as 200 if not set, and count bytes). Two
things make this dangerous if done naively. First, the recorder must guard against
a second `WriteHeader` itself — if it forwards two `WriteHeader` calls it
reproduces the `superfluous` bug and records the wrong status. Second, wrapping
hides the concrete type: `net/http`'s `http.Flusher`, `http.Hijacker`, and the
newer `http.NewResponseController` (Flush, `SetWriteDeadline`,
`SetReadDeadline`) reach the real writer by type-asserting or by calling an
`Unwrap() http.ResponseWriter` method. A wrapper without `Unwrap` silently
disables streaming and deadlines — SSE and long-poll handlers stop flushing with
no error. Since Go 1.20 the contract is: give your wrapper an `Unwrap()
http.ResponseWriter` method and let `http.NewResponseController` find the
capabilities through it.

### The error body is a contract: RFC 9457 problem+json

An ad-hoc `{"error": "..."}` body is not a contract; clients cannot branch on it
reliably and it invites leaking internals. RFC 9457 (Problem Details for HTTP
APIs, which obsoletes RFC 7807) defines a standard `application/problem+json`
document with `type` (a URI identifying the problem class), `title` (a stable
human summary), `status` (the HTTP code), `detail` (a human explanation of *this*
occurrence), and `instance` (a URI/id for this specific occurrence, a natural
home for a correlation id). The discipline that goes with it: 4xx bodies may carry
a specific `detail` (a validation message is safe and useful to the client), but
5xx bodies must carry a *generic* detail — never the raw error string, which can
contain SQL, file paths, or stack fragments. The full error goes to the server
log, keyed by the same correlation id that appears in the response, so an operator
can join the client's id to the server-side detail without the client ever seeing
it.

### Deadlines are errors too

A request that runs too long is a failure with its own status. Derive a
per-request `context.WithTimeout` in a middleware, have handlers honor
`r.Context()` (select on `ctx.Done()` around slow work), and map
`context.DeadlineExceeded` to 504 Gateway Timeout in the boundary. This is
distinct from `http.TimeoutHandler`, which substitutes its *own* buffered
`ResponseWriter` and, on timeout, discards the buffer and writes its 503 (or a
custom body). The trade-off is real: `TimeoutHandler` cannot rescue a handler that
has already *started streaming*, because once the real writer is committed there
is nothing to buffer — its whole mechanism depends on buffering the response.
Context-based timeouts push the responsibility onto the handler to check
`ctx.Done()`, but they compose with streaming and with downstream calls that also
take the context.

### Bound the input

An ingestion endpoint that decodes `r.Body` without a limit lets a single request
allocate unbounded memory — a trivial denial of service. `http.MaxBytesReader(w,
r.Body, n)` caps the body at `n` bytes and, on overflow, returns a
`*http.MaxBytesError` (which `errors.As` extracts) that the boundary maps to 413
Request Entity Too Large — not 400. Pair it with `json.Decoder` and
`DisallowUnknownFields()` so unexpected fields are rejected, and map the typed
JSON errors (`*json.SyntaxError`, `*json.UnmarshalTypeError`, `io.EOF` for an
empty body) to 400. Decode *into* a struct, check for a second JSON value after
the first to reject trailing garbage. Never decode an unbounded request body.

### Log once, at the boundary, with a request-scoped logger

The final discipline is observability. A `RequestID` middleware mints a
correlation id (or adopts an inbound one), stores a `slog.Logger` pre-populated
with that id via `slog.With` into the request context, and echoes the id in a
response header. The boundary logs the full error *exactly once* — with the id,
method, path, and mapped status — and handlers *never log errors*; they return
them. This eliminates the classic double-log where a handler logs an error and
the boundary logs it again with a different shape, and it links the client-facing
id to the server log line. Middleware order is a deliberate, testable stack:
`Recoverer` outermost, then `RequestID`, then `Logger`, then the handler, so the
logger and recoverer both see the id and the recoverer catches panics from
everything inside it.

## Common Mistakes

### Panicking on ordinary bad input

Wrong: a handler that `panic`s when a query parameter is missing or malformed.
That treats a routine, expected condition as a programmer bug and leans on the
`Recoverer` as a control-flow mechanism.

Fix: return `fmt.Errorf("...: %w", ErrInvalidInput)` and let the boundary map it
to 400. Reserve panic for genuine invariant violations.

### Recovering deep in the call stack

Wrong: a `recover` inside a helper several calls below the handler. It swallows
the panic before the boundary `Recoverer` sees it, often after the response is
already half-written, leaving a corrupt response and no stack in the logs.

Fix: let panics propagate to the outermost `Recoverer`. Recover deep only when
you deliberately convert a panic to an error at a well-defined seam and document
it.

### A blanket recover that swallows ErrAbortHandler

Wrong: `if rec := recover(); rec != nil { write500() }` with no carve-out. It
converts `http.ErrAbortHandler` and `http.ErrHandlerTimeout` into a normal 500,
corrupting `net/http`'s intentional quiet-abort and timeout semantics.

Fix: re-panic those two sentinels; only non-abort panics become a 500.

### Assuming Recoverer protects spawned goroutines

Wrong: `go sendEmail(user)` inside a handler, trusting the `Recoverer` to catch a
panic in `sendEmail`. The panic is on a different goroutine; it escapes recovery
and crashes the process.

Fix: launch background work through a `safeGo` helper that recovers *inside* the
goroutine and reports the panic.

### Writing the response twice

Wrong: a handler writes a partial body, then returns an error the boundary also
writes. Result: `superfluous response.WriteHeader call` and a mangled response.

Fix: a handler either fully writes the success response *or* returns an error and
writes nothing. The boundary is the only writer on the error path.

### Wrapping ResponseWriter without Unwrap

Wrong: a status-recording wrapper with no `Unwrap()` method. `http.Flusher` and
`http.NewResponseController` can no longer reach the real writer, silently
breaking streaming and deadlines.

Fix: implement `Unwrap() http.ResponseWriter` on the wrapper and use
`http.NewResponseController` for Flush/deadlines.

### Leaking internal detail in 5xx bodies

Wrong: `json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})` on a
500, shipping the raw error — SQL text, file paths, stack fragments — to the
client.

Fix: 5xx bodies carry a generic message plus a correlation id; the full error
goes only to the server log under that id.

### Decoding an unbounded body, or mapping MaxBytesError to 400

Wrong: `json.NewDecoder(r.Body).Decode(&v)` with no `MaxBytesReader`, or catching
the overflow and returning 400.

Fix: wrap with `http.MaxBytesReader`, and map `*http.MaxBytesError` to 413, not
400.

### Double-logging the same error

Wrong: the handler logs the error and returns it, and the boundary logs it again.
Two differently-shaped lines for one failure.

Fix: handlers return errors and never log them; the boundary logs exactly once
with the request-scoped logger.

### Comparing errors with == instead of errors.Is

Wrong: `switch err { case ErrNotFound: ... }`. Any wrapped sentinel falls through
to the default 500.

Fix: `switch { case errors.Is(err, ErrNotFound): ... }`.

Next: [01-handler-error-adapter.md](01-handler-error-adapter.md)
