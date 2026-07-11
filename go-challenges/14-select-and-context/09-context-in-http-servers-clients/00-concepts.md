# Context in HTTP Servers and Clients — Concepts

This is the lesson where context stops being an abstraction and becomes the
load-bearing member of every production HTTP path. In a real service, a single
inbound request fans out into a database call, two upstream fetches, an audit
write, and a response flush — and any of those can be aborted for three
different reasons: the client hung up, the handler blew its deadline, or the
process is shutting down for a deploy. A senior backend engineer must treat
`r.Context()` as the one cancellation spine that ties all three together, know
exactly where that signal must be honored (DB calls, upstream fetches, response
writes) versus where it must be deliberately dropped (audit logs, async
telemetry, cache warms), and be able to tell on-call *why* a request died from a
single log line. This file is the conceptual foundation; read it once and every
one of the ten independent exercises that follow will make sense.

## The request context is the server's cancellation spine

Every `*http.Request` already carries a context, reachable through
`r.Context()`. It is not something you create — the server owns it and cancels
it for you in three situations: the client closes the connection, `Server.Shutdown`
begins, or a handler-level timeout middleware fires. A handler that respects
this context stops working the instant nobody is listening; a handler that
ignores it runs to completion, burns CPU on a result that will be discarded, and
may try to write into a dead socket.

The single most common and most damaging mistake in this whole topic is starting
from `context.Background()` inside a handler. That severs the spine: the derived
work no longer observes client-disconnect or shutdown, so it leaks. The rule is
absolute — inside a handler, every derived context descends from `r.Context()`,
never from `Background()`, unless you are *deliberately* detaching background
work (which has its own tool, covered below).

## The context tree: base, conn, request, derivation

The request context is the leaf of a tree the server builds per connection:

```
Server.BaseContext (server root, one per listener)
  -> ConnContext        (per TCP connection)
    -> r.Context()      (per request)
      -> your WithTimeout / WithValue derivations
```

Cancellation flows strictly downward: cancel any node and everything below it
cancels. This is not trivia — it is the mechanism behind graceful shutdown. If
you wire `Server.BaseContext` to a root context that you cancel when shutdown
begins, then *every in-flight request context in the whole server* observes the
cancellation at once, and long-poll handlers that select on `r.Context().Done()`
bail immediately instead of hanging until the grace period force-closes them.
Leave `BaseContext` at its default and `Shutdown` will stop accepting new
connections but will not tell running handlers anything.

## Outbound calls: `NewRequestWithContext`, never `NewRequest`

`http.NewRequest` hardcodes `context.Background()` as the outbound request's
context. That means the caller's deadline and cancellation never reach the round
trip: the client's connection dial, TLS handshake, header wait, and body read
are all uncancellable. `http.NewRequestWithContext` is the only correct
constructor for an outbound call, because every internal timeout and
cancellation inside `net/http`'s transport is keyed on the request's context. If
you take a `ctx` argument and then build the outbound request with `NewRequest`,
the `ctx` is decorative — a reviewer should reject it on sight.

## Middleware derives, handlers consume

The canonical timeout pattern has two halves. Middleware *derives*: it computes
`ctx, cancel := context.WithTimeout(r.Context(), d)`, defers `cancel()`, and
replaces the request via `next.ServeHTTP(w, r.WithContext(ctx))`. The downstream
handler *consumes*: it selects on `ctx.Done()` before and around every blocking
operation. Deriving a deadline the handler never checks makes the deadline
decorative — the handler runs to completion regardless. The `defer cancel()` is
not optional bookkeeping: skip it and you leak the timer and its goroutine until
the deadline fires, even when the handler returned early.

## `TimeoutHandler` cancels context; it cannot preempt a blocked write

`http.TimeoutHandler(h, dt, msg)` runs `h` with a derived timeout context, and if
`h` overruns it writes `503 Service Unavailable` with `msg` as the body and
switches `h`'s `ResponseWriter` to return `ErrHandlerTimeout` on further writes.
What it does *not* do is kill `h`'s goroutine. `TimeoutHandler` cancels
`r.Context()` and unblocks any code that checks the context, but a goroutine
already parked inside a `Write`, a `read(2)` syscall, or a non-context-aware C
call keeps running until it unblocks on its own. This matters for the write
side: a slow-reader client (a slowloris that accepts response bytes one per
second) can pin a handler goroutine inside `Write` forever, and `TimeoutHandler`
is powerless against it. The defense is a real socket deadline:
`http.NewResponseController(w).SetWriteDeadline(t)` sets a deadline on the
underlying connection, so a stalled `Write` (or `Flush`) returns an
`os.ErrDeadlineExceeded`-class error instead of blocking indefinitely.

## Knowing *why* a context died: `Cause`

`ctx.Err()` answers the coarse question — `Canceled` or `DeadlineExceeded` — and
that is the right thing for control flow. But in production the coarse answer is
useless for on-call: was it a slow upstream (handler-timeout), a bailing client
(client-disconnect), or a deploy (server-shutdown)? All three surface as
`Canceled` or `DeadlineExceeded` through `ctx.Err()`. The distinguishing tool is
the cause: `context.WithTimeoutCause(parent, d, cause)` and
`context.WithCancelCause(parent)` attach a specific error, and `context.Cause(ctx)`
retrieves it. A single log line — `reason=handler-timeout request_id=...` — lets
the on-call engineer classify an incident in seconds. The subtlety worth
internalizing: after a `WithTimeoutCause` timer fires, `ctx.Err()` is still the
coarse `DeadlineExceeded` while `context.Cause(ctx)` is your specific cause; and
when a parent is cancelled first, `Cause` on the child returns the *parent's*
cause, because the first cancellation in the chain wins.

## A request deadline is a budget, not a per-hop reset

When a resilient client retries an idempotent upstream call, the naive
implementation resets the timeout on each attempt — three attempts of "up to two
seconds each" quietly becomes a six-second request against a two-second SLA, and
under load those over-budget requests pile up and pin server resources. The
correct model treats the inbound `ctx` deadline as a single shared budget that
all attempts spend from. The retry loop never resets the deadline; it checks the
*remaining* budget before each backoff and aborts the moment the remaining
budget is smaller than the next sleep, returning `context.Cause(ctx)` when the
budget is exhausted mid-flight. Clone the request per attempt with
`Request.Clone(ctx)` and rewind the body with `GetBody` so each attempt sends a
fresh, complete request.

## Some work must outlive the request: `WithoutCancel`

Audit logs, cache warms, and async telemetry must survive the client
disconnecting — an audit record that is dropped because the user closed the tab
is a compliance hole. But you still want the request-scoped values (request id,
trace id) attached to that work. `context.WithoutCancel(parent)` returns a
context that keeps the parent's *values* but is immune to the parent's
*cancellation*. Launch the background write from a `WithoutCancel` copy of
`r.Context()`, then bound it with its own `context.WithTimeout` so it cannot run
forever, and join it (a `WaitGroup`) so a test — and a graceful shutdown — can
wait for it. Passing `r.Context()` directly into fire-and-forget work is the bug
this prevents: the write aborts the instant the client disconnects.

## Request-scoped values belong in the context, keyed by an unexported type

Request ids, the authenticated principal, and trace ids are request-scoped
metadata; they belong in `context.Value`, not in a long-lived struct that
outlives the request (which leaks one request's id into another). The key must
be an unexported named type — `type ctxKey int` with a private constant — so no
other package can collide with it, and never a bare `string` key (which collides
across packages that happen to pick the same string). Expose a typed accessor,
`RequestIDFrom(ctx) string`, that returns the empty string when no id is present:
callers get a clean no-metadata contract and never touch the raw key. Reserve
`context.Value` for this kind of metadata — never smuggle required function
parameters (a database handle, a logger the function cannot work without)
through it.

## Outbound observability rides on the request context: `httptrace`

`httptrace.WithClientTrace(ctx, trace)` installs hooks — DNS start/done, connect
start/done, TLS handshake, first response byte — on the outbound request's
context without changing call semantics or the response. You tag the measured
DNS/connect/TTFB durations with the request id pulled from the same context, and
now upstream latency is attributable per inbound request in your observability
backend. Because the trace rides the request context, cancellation still applies
normally; the instrumentation is transparent.

## Reading a request body is a trust boundary

An unbounded `io.ReadAll(r.Body)` is a memory-exhaustion vector: an attacker (or
a buggy client) sends a multi-gigabyte or slow-drip body and the server buffers
it all. Wrap `r.Body` in `http.MaxBytesReader(w, r.Body, limit)`, which caps the
read and returns a `*http.MaxBytesError` once the limit is exceeded — translate
that to `413 Request Entity Too Large`, and be careful to distinguish it (with
`errors.As`) from a genuine context cancellation, which is a different failure
with a different response. Read under `r.Context()` so an abandoned upload stops
buffering instead of holding the goroutine. (There is no `Decoder.DecodeContext`
method — JSON decoding is the ordinary `json.Decoder`/`json.Unmarshal` over the
size-capped, context-honored read.)

## Writing after the context is done

Once `ctx.Done()` has fired because the client is gone, writing to the
`http.ResponseWriter` may fail silently or race with the server tearing the
connection down. Gate expensive work and writes on a `ctx.Done()` check, and
prefer returning an error status the middleware can translate over writing into
a connection nobody is reading.

## Common Mistakes

### Starting from `context.Background()` inside a handler

Wrong: `ctx := context.Background()` (or a fresh `WithTimeout` off it) inside
`ServeHTTP`. This severs the request from client-disconnect and shutdown, so the
server keeps doing work nobody will read. Fix: derive from `r.Context()`.

### Using `http.NewRequest` for outbound calls

Wrong: `http.NewRequest(method, url, body)` inside a function that took a `ctx`.
The deadline never reaches the round trip. Fix:
`http.NewRequestWithContext(ctx, method, url, body)` — the only constructor the
transport actually honors.

### Storing the request id in a struct instead of the context

Wrong: hanging the request id off a request-scoped struct that outlives the
request, leaking ids across requests. Fix: store it under an unexported `ctxKey`
type and read it back with `RequestIDFrom(r.Context())`.

### Writing to the ResponseWriter after `ctx.Done()`

Wrong: doing the expensive work and writing unconditionally. Fix: check the
context before each write and bail before any blocking operation rather than
writing into a closed connection.

### Assuming `TimeoutHandler` kills the handler goroutine

Wrong: relying on `TimeoutHandler` to stop a handler blocked in a `Write` or a
syscall. It only cancels the context and writes `503`; the goroutine runs until
it unblocks. Fix: for the write side, set a real socket deadline with
`http.NewResponseController(w).SetWriteDeadline`.

### Resetting the deadline on every retry

Wrong: giving each retry attempt a fresh `WithTimeout`, so one logical request
runs far past its SLA. Fix: spend one shared `ctx` budget; abort when the
remaining budget is under the next backoff.

### Passing `r.Context()` into fire-and-forget work

Wrong: launching an audit or cache-warm goroutine with `r.Context()`, so the
write is aborted the instant the client disconnects. Fix:
`context.WithoutCancel(r.Context())` to keep values while dropping cancellation,
then bound it with its own `WithTimeout`.

### `io.ReadAll(r.Body)` without `MaxBytesReader`

Wrong: an unbounded read, exposing the server to memory exhaustion, and not
distinguishing `*http.MaxBytesError` (413) from a genuine context cancellation.
Fix: `http.MaxBytesReader(w, r.Body, limit)`, read under `r.Context()`, and
branch on the error type.

### Forgetting `defer cancel()`

Wrong: deriving a `WithTimeout`/`WithCancel` context and never cancelling it,
leaking the timer and its goroutine until the deadline fires. Fix: `defer cancel()`
on the line after the derivation.

### Not wiring `BaseContext` to a shutdown-cancelled root

Wrong: leaving `Server.BaseContext` at its default, so in-flight handlers never
observe `Server.Shutdown` and long-poll requests hang until the grace period
force-closes them. Fix: `BaseContext` returns a cancellable root you cancel when
shutdown begins.

Next: [01-per-request-timeout-middleware.md](01-per-request-timeout-middleware.md)
