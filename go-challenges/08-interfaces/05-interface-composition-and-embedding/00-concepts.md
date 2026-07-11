# Interface Composition and Embedding — Concepts

In a Go backend you almost never write the I/O primitive. You wrap it. Every
access-log middleware, every instrumented HTTP client, every audited store, every
byte-counting body is a struct that embeds an interface — `http.ResponseWriter`,
`net.Conn`, `io.ReadCloser` — and adds exactly one concern: metrics, a size
limit, a timeout, buffering, an audit trail. The mechanics look trivial until
they bite in production. A logging middleware that wraps `http.ResponseWriter`
silently kills HTTP/2 flushing and WebSocket hijacking, because the wrapper no
longer satisfies `http.Flusher`/`http.Hijacker`. A body wrapper that forgets to
`Close` the underlying reader leaks connections under load. A `MultiWriter` audit
sink couples request latency to the health of the audit backend. This file is the
conceptual foundation for the nine independent exercises that follow; read it once
and you have the model you need for all of them.

## Concepts

### An interface embedding interfaces is a contract union

`type ReadWriteCloser interface { io.Reader; io.Writer; io.Closer }` embeds three
single-method interfaces. The result is the *union* of their method sets: a type
satisfies `ReadWriteCloser` exactly when it has `Read`, `Write`, and `Close` with
the right signatures. This is composition at the level of the contract. It answers
the question "who is allowed to be passed here". Nothing is delegated, nothing is
overridden — it is purely a name for a set of method sets. Because Go's
satisfaction is structural, any type that happens to have those three methods
satisfies it without declaring so. The degenerate case is the other end of the
scale: an interface with zero methods is the alias `any` (`interface{}`), whose
method-set union is empty, so *every* type satisfies it — composition bottoms out
at the empty contract that constrains nothing.

### A struct embedding an interface is delegation with an override seam

`type wrap struct { http.ResponseWriter }` is a different thing entirely. It is a
concrete struct with an embedded interface *field*. Every method of the embedded
interface is *promoted*: `w.WriteHeader(200)` on the wrapper forwards to the
embedded value's `WriteHeader`. The struct therefore satisfies
`http.ResponseWriter` for free — and you get a seam. You can declare a method with
the same name on the outer struct; yours *shadows* the promoted one, and the
promoted method is still reachable through the field (`w.ResponseWriter.WriteHeader`).
That shadow is the entire point of instrumentation: override `WriteHeader` to
capture the status, override `Write` to count bytes, leave everything else
delegated. The first construct (interface embeds interfaces) is about who
satisfies a contract; the second (struct embeds an interface) is about delegation
plus selective override.

### Method promotion and accidental shadowing

Promotion is what makes embedding ergonomic, and shadowing the *wrong* method is a
classic subtle bug. If you embed `io.Reader` to expose the underlying reader and
then define a `Read` on your struct without meaning to, callers now hit your
override, not the reader you intended to surface. The rule of thumb: only declare a
method that collides with a promoted one when you *intend* to instrument that exact
method; otherwise leave it promoted. When you do override, remember your version is
responsible for calling through to the embedded implementation — a `WriteHeader`
override that captures the code but never calls `w.ResponseWriter.WriteHeader(code)`
sends no header at all.

### The optional-interface problem

This is the single most common composition bug in Go HTTP middleware. Many stdlib
types satisfy *extra* interfaces beyond their nominal one. An `http.ResponseWriter`
handed to a handler frequently *also* implements `http.Flusher` (streaming, SSE),
`http.Hijacker` (WebSocket upgrades), `http.Pusher`, and `io.ReaderFrom`. A
`net.Conn` may implement `syscall.Conn`. When you embed the *interface*, your
struct only promotes the *named* interface's methods. It does not inherit the
underlying value's bonus interfaces. So after you wrap, `w.(http.Flusher)` on your
wrapper fails — the type assertion returns `ok == false` even though the real
writer underneath is a perfectly good `Flusher`. Streaming, Server-Sent Events, and
WebSocket upgrades break, silently, only in production, only on the streaming
endpoints.

### http.NewResponseController is the modern fix

Before Go 1.20 the remedy was to hand-forward every optional interface: type-assert
the underlying writer to `http.Flusher` and re-expose `Flush` on your wrapper, then
do the same for `Hijacker`, `Pusher`, and so on — tedious and easy to get wrong.
Go 1.20 added `http.NewResponseController(w)`, which returns a
`*http.ResponseController` with `Flush()`, `Hijack()`, `SetReadDeadline()`,
`SetWriteDeadline()` methods. It finds the real implementation by *unwrapping*: if
your wrapper exposes an `Unwrap() http.ResponseWriter` method, the controller
traverses it (and any further nested wrappers) until it reaches a writer that
implements the optional interface. The senior discipline is: when you wrap
`http.ResponseWriter`, always add `func (w *wrap) Unwrap() http.ResponseWriter`,
and inside handlers reach optional behavior through
`http.NewResponseController(w)` rather than a raw type assertion on the wrapper.

### Small interfaces compose well; accept the narrowest one

The power of `io` is that `Reader`, `Writer`, and `Closer` are one method each, so
any subset composes: `ReadWriter`, `ReadCloser`, `WriteCloser`, `ReadWriteCloser`.
Every method you add to a composed interface *shrinks* the set of types that can
satisfy it. A `ReadWriteCloserSeeker` is so specific that almost nothing implements
it. The corollary for callers is "accept the narrowest interface a function
actually needs": a handler that only reads a request body should take an
`io.Reader`, not an `*http.Request`; a query handler should depend on a two-method
`ReaderRepo`, not a fat `Store`. Narrow parameters widen the set of things you can
pass — including fakes in tests.

### Closing composed readers: propagate Close to every owner

When you stack decoders — `gzip.Reader` over an HTTP body, a cipher stream over a
file — `Close` must reach *every* layer that owns a resource. `gzip.Reader.Close()`
flushes and validates the gzip stream but does *not* close the underlying reader.
If you close only the gzip layer and drop the `http.Response.Body`, the connection
is never returned to the pool and the pool exhausts under load. The fix is to
compose a single `io.ReadCloser` whose `Close` closes the decoder *and then* the
source, typically joining their errors with `errors.Join` so neither is lost.

### NopCloser and MultiWriter are the canonical adapters

`io.NopCloser(r)` turns an `io.Reader` into an `io.ReadCloser` by adding a no-op
`Close`, adapting a reader to an API that demands the closer contract.
`io.MultiWriter(a, b, ...)` fans a single `Write` out to N writers. Both are
composition primitives, and `MultiWriter` carries a trap: it writes to the sinks in
order and *stops at the first error*, and it blocks on the slowest sink. That
couples the caller's latency and failure to the most fragile writer — fine for
"write to stdout and a buffer", dangerous for "write to the response and a remote
audit backend on the hot path". For best-effort audit fan-out you want a wrapper
that isolates the audit sink's errors from the primary write, or an async/buffered
sink, not a synchronous `MultiWriter`.

### Idempotent, safe, concurrency-aware Close

Composed closers are called from `defer` and from error paths, often more than
once. `Close` must be safe to call twice — return a sentinel like `ErrClosed`, or
`nil`, but never panic and never double-decrement a metric. A `sync.Once` or a
`closed bool` flag is the usual guard. Two more hazards: a wrapper that embeds an
*interface* (not a concrete type) and leaves it nil panics on the first promoted
call, so require the dependency in a constructor and validate it; and a wrapper
that adds a mutex must guard the *same* invariants the wrapped type assumes, not a
different set.

### Deadlines by embedding, buffering by composition

Two production wrappers show delegation-plus-override cleanly. A per-operation
idle timeout embeds `net.Conn` and overrides `Read`/`Write` to call
`SetReadDeadline`/`SetWriteDeadline(now+idle)` before each operation, so a stalled
peer cannot pin a goroutine forever — every other `net.Conn` method
(`LocalAddr`, `Close`, ...) stays promoted. A buffered `ReadWriteCloser` composes a
`bufio.Reader` and a `bufio.Writer` over the underlying stream; its `Close` must
`Flush` the writer *before* closing the source, or the last partial buffer is
silently dropped. Order of operations in a composed `Close` is a correctness
property, not a detail.

## Common Mistakes

### Wrapping http.ResponseWriter and losing Flusher/Hijacker/Pusher

Wrong: `type lw struct { http.ResponseWriter }` with no `Unwrap`, so
`w.(http.Flusher)` fails and SSE, streaming, and WebSocket upgrades break.

Fix: implement `Unwrap() http.ResponseWriter` and reach optional behavior through
`http.NewResponseController(w)`, or explicitly forward the optional interfaces.

### A composed interface with too many methods

Wrong: `ReadWriteCloserSeeker` — four methods, so almost no type satisfies it and
callers cannot pass the values they have.

Fix: keep composed interfaces to the smallest union callers actually need; split
distinct concerns into separate interfaces.

### Closing the decoder but leaking the source

Wrong: closing `gzip.Reader` but dropping the `http.Response.Body`, exhausting the
connection pool under load.

Fix: compose a `ReadCloser` whose `Close` closes the decoder *and then* the source,
joining the errors.

### Accidentally shadowing a promoted method

Wrong: defining `Read` on a struct that embeds an `io.Reader` you meant to expose,
so callers silently hit your override.

Fix: only override methods you intend to instrument; leave the rest promoted.

### Assuming io.MultiWriter is fault-tolerant

Wrong: fanning the hot-path write to a response and a remote audit sink with
`io.MultiWriter`; it returns on the first error and blocks on the slowest sink, so
a slow or failing audit backend degrades every request.

Fix: for best-effort audit, use a wrapper that isolates the audit error, or a
buffered/async sink off the hot path.

### Non-idempotent Close

Wrong: a wrapper whose `Close` is called from several defers/error paths, causing a
double-close panic or a double metric decrement.

Fix: guard with `sync.Once` or a closed flag that returns a sentinel error.

### A nil embedded interface

Wrong: embedding an *interface* in a struct and leaving it nil, then panicking on
the first promoted call.

Fix: require the dependency in a constructor and validate it is non-nil.

### A buffered Close that does not Flush first

Wrong: `Close` closes the underlying writer without flushing, silently dropping the
last partial buffer.

Fix: `Close` must `Flush` and then close the source, returning the first (or joined)
error.

Next: [01-memconn-readwritecloser.md](01-memconn-readwritecloser.md)
