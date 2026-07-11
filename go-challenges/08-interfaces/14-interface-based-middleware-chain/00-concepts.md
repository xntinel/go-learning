# Interface-Based Middleware Chains for HTTP Servers — Concepts

Middleware is where an HTTP service enforces the cross-cutting contracts it must
uphold in production: authentication and authorization, observability, rate
limiting, timeouts, panic isolation, and protocol correctness. The abstraction
is trivially small — a middleware is just `func(http.Handler) http.Handler` —
but the senior skill is not writing that signature. It is composing an ordered,
correct chain where ordering is load-bearing, where every layer respects the
write-once contract of `http.ResponseWriter`, and where wrapping the writer does
not silently break the optional interfaces that streaming, SSE, and websocket
handlers depend on. This file is the conceptual foundation for the ten
independent exercises that follow; each builds one real layer of the toolbox a
backend engineer actually ships.

## Concepts

### A middleware is a function from handler to handler

The whole abstraction is `func(http.Handler) http.Handler`: take the next
handler, return a new handler that does something before or after calling it.
Composition is repeated application. What makes an anonymous closure usable as a
handler at all is interface satisfaction: `http.HandlerFunc` is a named function
type with a `ServeHTTP` method, so a bare `func(w, r)` converted to
`http.HandlerFunc` implements `http.Handler`. Every middleware in this lesson
returns `http.HandlerFunc(func(w http.ResponseWriter, r *http.Request){ ... })`
and slots into the chain because of that one method.

### The type alias is not decoration — it is identity

Declaring `type Handler = http.Handler` (with the `=`) makes `Handler` an *alias*:
it is the exact same type, sharing identity, so any existing `http.Handler`
already satisfies a `Middleware func(Handler) Handler` parameter with no
conversion. Drop the `=` and `type Handler http.Handler` becomes a *distinct
named type* with the same underlying interface but a different identity; an
ordinary `http.Handler` value no longer satisfies the middleware signature, and
every call site needs an explicit conversion. The alias keeps the signature
readable ("a function from handler to handler") without introducing a new type
into the type system. This is one of the few places where the `=` in a type
declaration changes program semantics, not just naming.

### Ordering is load-bearing and counter-intuitive

`Chain.Then` folds the middleware slice in reverse: `h = mw[i](h)` from the last
index down to the first. The consequence is that the *first* middleware declared
becomes the *outermost* wrapper — it runs first on the way in and last on the way
out. This is the opposite of the naive reading of the list, and getting it
backwards is a real production incident, not a style nit. Recover must be
outermost so it catches panics from every inner layer, including the logger and
the auth check. Request-ID must run before logging so every log line already
carries the trace id. Auth must run before per-subject rate limiting so the
limiter can key on the authenticated subject, but after request-id so the 401
itself is traceable. The order you declare the chain *is* the order these
contracts execute; treat the slice as the outer-to-inner nesting it produces.

### The ResponseWriter contract is write-once

`http.ResponseWriter` has a strict protocol. `WriteHeader(code)` may be called at
most once. The first call to `Write` implicitly commits a `200 OK` if
`WriteHeader` has not been called yet. Once any bytes are flushed to the client,
the status and headers are fixed — you cannot change the code or add a header.
Violating this prints the familiar `http: superfluous response.WriteHeader call`
to the server log and produces a corrupt response. Middlewares that touch the
response are exactly the ones that break this: a recover boundary that writes a
500 after the handler already sent a 200, an error mapper that re-calls
`WriteHeader`. The defenses are a `wroteHeader` flag or a status-capturing
wrapper that makes `WriteHeader` idempotent, and never writing a status once the
inner handler has started responding.

### Wrapping the writer drops optional interfaces

`http.ResponseWriter` is a minimal interface, but the concrete value the server
passes also satisfies optional interfaces: `http.Flusher` (SSE, chunked
streaming), `http.Hijacker` (websocket upgrades), `http.Pusher`, `io.ReaderFrom`.
The instant you wrap it in your own struct to capture the status code, those
optional interfaces disappear — your wrapper only has the methods you wrote, so a
type assertion to `http.Flusher` inside a streaming handler now fails and
flushing silently stops working. Before Go 1.20 the fix was to hand-forward every
optional method, which is brittle. Since Go 1.20 the correct tool is
`http.NewResponseController(w)`, which reaches *through* wrappers (via an
`Unwrap() http.ResponseWriter` method your wrapper provides) to find the real
`Flush`, `Hijack`, or `SetWriteDeadline`. A capturing wrapper that implements
`Unwrap` keeps streaming alive; one that does not breaks it.

### Short-circuiting is returning without calling next

A middleware controls the request by choosing whether to call
`next.ServeHTTP(w, r)`. On the happy path it must call `next` *exactly once*.
Calling it zero times on a request that should proceed hangs the client or sends
an empty body; calling it twice double-serves and triggers the superfluous-header
warning. Deliberately *not* calling `next` is how a middleware short-circuits:
auth writes 401 and returns, rate limiting writes 429 and returns, a CORS
preflight writes 204 and returns. The invariant to hold in your head for every
layer: on success, call next once; on rejection, write the response and return
without calling next.

### Context is the request-scoped propagation channel

Request-scoped values (a request id) and deadlines travel on the request's
`context.Context`. You attach them with `r = r.WithContext(ctx)` and read them
with `r.Context().Value(key)`. The one rule that prevents cross-package
corruption: the key must be an unexported named type, never a bare `string`. Two
packages both using `"request_id"` as a string key would collide in the same
context; an unexported `type ctxKey struct{}` (or `type ctxKey int`) is unique to
your package and cannot clash. Always pair the writer (`WithRequestID`) with a
typed accessor (`FromContext(ctx) (string, bool)`) so callers never touch the raw
key.

### Panic recovery belongs at the very edge

An unrecovered panic in a handler goroutine does not just fail that request — by
default it crashes the entire server process. A nil-map write in one rarely-hit
handler must not take down every other in-flight request. The defense is a
deferred `recover()` in the *outermost* middleware, which converts the panic into
a 500 and logs the value plus `runtime/debug.Stack()`. Two subtleties make this a
senior task rather than a one-liner. First, the recover must respect the
write-once contract: if the handler already committed a status before panicking,
do not write a second header. Second, it must re-panic `http.ErrAbortHandler` —
the runtime uses that specific sentinel to abort a response intentionally (a
handler signalling "stop, do not send anything"), and swallowing it hides real
aborts and can leave a hijacked connection in a bad state.

### Per-client rate limiting is a keyed set of token buckets

A single global `rate.Limiter` throttles all clients together, which is almost
never the requirement — one hot client would starve everyone. Per-client limiting
needs a `map[string]*rate.Limiter` keyed by client identity (the IP from
`RemoteAddr`, or the authenticated subject), guarded by a mutex, with a bucket
lazily created per key. Each bucket is a `rate.NewLimiter(rate.Every(interval),
burst)`; `Allow()` reports whether a token was available. Two operational truths:
the limiter must *persist across requests* (a per-request limiter that resets
every call never limits anything), and the map *grows unbounded* without
eviction, so in production the store itself is a resource to manage with an LRU or
TTL. On rejection, write 429 with a `Retry-After` header.

### Timeouts have two layers with different guarantees

`http.TimeoutHandler(next, dt, msg)` gives a hard cutoff: it runs the handler
against a buffered `ResponseWriter` and, if the handler is not done by `dt`,
discards the buffer and writes a `503 Service Unavailable` with `msg`. Simple, but
because it buffers the whole response it is wrong for streaming — it defeats SSE
and chunked output. The other layer is cooperative: derive
`ctx, cancel := context.WithTimeout(r.Context(), d)`, rewrap the request, and let
the handler watch `ctx.Done()` so it can abort a slow downstream call (a DB query,
an upstream HTTP request) itself. Cooperative cancellation does not force a
response, but it lets well-written handlers stop wasting work; `ctx.Err()` returns
`context.DeadlineExceeded` once the deadline fires. Real services use both:
`TimeoutHandler` as a backstop for non-streaming routes, `WithTimeout` for
cooperative propagation everywhere.

## Common Mistakes

### Composing the chain in the wrong order

Wrong: assuming the first middleware in the list is the innermost, wrapping
closest to the handler. `Chain.Then` walks the slice in reverse precisely so the
first declared is the *outermost*. Getting it backwards puts recover *inside* the
handlers it is supposed to protect (so a panic in an outer layer still crashes the
server) or logs the request before the request-id middleware has generated an id.

### Using a distinct type instead of an alias

Wrong: `type Handler http.Handler`. That is a new named type; an ordinary
`http.Handler` no longer satisfies `func(Handler) Handler`, and every call site
needs a conversion. Fix: `type Handler = http.Handler` — the alias shares identity
so `http.Handler` values pass directly.

### Forgetting next, or calling it twice

Wrong: doing work but never calling `next.ServeHTTP` on the happy path (the
request hangs or returns an empty body), or calling it more than once (a double
response and a `superfluous response.WriteHeader call`). Fix: call `next` exactly
once on the success path, and return without calling it when short-circuiting.

### Losing Flusher/Hijacker by plain embedding

Wrong: wrapping `http.ResponseWriter` in a struct to capture the status, which
drops `http.Flusher` and `http.Hijacker`; SSE, chunked streaming, and websocket
upgrades silently stop working. Fix: give the wrapper an `Unwrap() http.ResponseWriter`
method and reach optional behavior through `http.NewResponseController`.

### Writing the header twice on the error path

Wrong: in a recover or error-mapping middleware, calling `WriteHeader(500)` after
the inner handler already committed a `200`. Fix: track whether the header was
written (a `wroteHeader` flag or a status-capturing wrapper) and skip the 500 once
the response has started.

### A plain string context key

Wrong: storing the request id under `ctx.Value("request_id")` with a bare string,
which collides with any other package doing the same. Fix: an unexported key type
(`type ctxKey struct{}`) and a typed accessor.

### One shared limiter, or a per-request limiter

Wrong: building a single `rate.Limiter` for all clients when the requirement is
per-client (a global throttle), or constructing a fresh limiter inside the handler
on every request (it resets each call and never limits). Fix: a keyed, mutex-
guarded map of limiters that persists across requests.

### Swallowing http.ErrAbortHandler in recover

Wrong: a recover middleware that catches every panic value, including
`http.ErrAbortHandler`. The runtime uses that sentinel to abort a response on
purpose; recovering it hides real aborts. Fix: re-panic when the recovered value
is `http.ErrAbortHandler`.

### Using TimeoutHandler on a streaming endpoint

Wrong: wrapping an SSE or chunked route in `http.TimeoutHandler`. It buffers the
entire response, so it defeats streaming and turns a long-lived stream into a 503.
Fix: use cooperative `context.WithTimeout` for streaming and reserve
`TimeoutHandler` for bounded, non-streaming responses.

### Not testing under -race

Wrong: shipping a keyed limiter map, a shared counter, or a mutated closure
without `go test -race`. Keyed maps and shared state behind middleware are the
classic data races that only surface under concurrency. Fix: exercise every
stateful middleware with concurrent requests under `-race`.

Next: [01-chain-composition-and-order.md](01-chain-composition-and-order.md)
