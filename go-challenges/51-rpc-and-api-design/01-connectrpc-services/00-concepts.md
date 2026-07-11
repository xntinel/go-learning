# Building Services with ConnectRPC — Concepts

A senior backend engineer rarely gets to pick a single wire protocol. Internal
service-to-service traffic wants gRPC's efficiency and streaming; dashboards,
`curl`-based debugging, webhooks, and browsers want plain HTTP/1.1 with JSON. The
usual answer is to run a gRPC core and bolt a hand-written REST/JSON gateway in
front of it, and then maintain two representations of every method forever.
ConnectRPC removes that duplication: one schema-defined, code-generated handler
speaks the Connect protocol, gRPC, and gRPC-Web simultaneously off the same
`net/http` mux. The same method, on the same port, answers a gRPC client over
HTTP/2 and a `curl` POST of JSON over HTTP/1.1. This file is the conceptual
foundation for the three exercises that follow; read it once and you have the
model you need for all of them.

This is a bar-mode lesson. The exercises depend on `buf`/`protoc` code generation
and the `connectrpc.com/connect` module, so the code is written to be
`gofmt`-clean and API-honest, external and generated code sits behind a
`//go:build connect` tag, and the generated `*.pb.go` / `*connect` glue is
described rather than gate-compiled. Correctness here is proven by clean
formatting, real verified APIs, and prose that matches the code, not by an
offline build.

## Concepts

### Why Connect exists

Connect is a single set of generated handlers that simultaneously speaks the
Connect protocol (HTTP/1.1 or HTTP/2, JSON or binary Protobuf), gRPC, and
gRPC-Web. One `.proto` service definition produces one handler interface and one
client, and that handler answers all three protocols from the same route. The
operational win is that you stop maintaining a gRPC core plus a separately
written REST/JSON gateway that has to be kept in lockstep by hand: the JSON
surface is the same generated code, so it cannot drift from the gRPC surface.

The trade-off against `grpc-go` is scope. Connect is a smaller surface built on
plain `net/http` and middleware (interceptors) that compose like `net/http`
handlers, at the cost of `grpc-go`'s richer ecosystem — xDS-based service
discovery, the full set of load-balancing policies, and some of the more exotic
call options. If you need those, `grpc-go` is still the tool. For the common case
of "a service that other services and a browser both call," Connect is less code
to own.

### The net/http integration model

There is no separate server type to learn. A generated
`NewUserServiceHandler(svc, opts...)` returns a `(string, http.Handler)` pair —
the RPC route prefix and the handler that serves it. You register it on an
ordinary `http.ServeMux`:

```go
mux := http.NewServeMux()
path, handler := userv1connect.NewUserServiceHandler(svc)
mux.Handle(path, handler)
```

Because it is just an `http.Handler` on a standard mux, Connect services compose
with every piece of `net/http` machinery you already run: TLS config, read and
write timeouts, `http.TimeoutHandler`, panic-recovery middleware, tracing
wrappers, and additional routes for health checks or a metrics endpoint on the
same server. The RPC route prefix is derived from the fully-qualified service
name, e.g. `/user.v1.UserService/`, and each method is a POST to
`/user.v1.UserService/GetUser`.

### One port, two transports

HTTP/1.1 gives you the `curl`- and browser-reachable JSON surface. HTTP/2 is
required for gRPC clients and for full-duplex and server streaming, and it must
be available on the *same* listener so a single port serves both. Modern Go
(1.24+) exposes this through `http.Protocols`: build one, call `SetHTTP1(true)`
and `SetUnencryptedHTTP2(true)`, and assign it to `http.Server.Protocols`.

```go
proto := new(http.Protocols)
proto.SetHTTP1(true)
proto.SetUnencryptedHTTP2(true)
srv := &http.Server{Addr: "127.0.0.1:8080", Handler: mux, Protocols: proto}
```

Before Go 1.24 the same effect required wrapping the handler with
`golang.org/x/net/http2/h2c.NewHandler(mux, &http2.Server{})` so that unencrypted
("cleartext") HTTP/2 could be negotiated without TLS. The failure mode of
forgetting unencrypted HTTP/2 is quietly nasty: `curl` over HTTP/1.1 keeps
working, so the service looks healthy, while gRPC clients and every streaming RPC
break because they cannot get an HTTP/2 connection. Test the gRPC and streaming
paths explicitly, not just the JSON one, or this stays hidden until a consumer
complains.

### The generic request and response envelope

Handlers do not take and return the bare Protobuf message. They take
`*connect.Request[T]` and return `*connect.Response[T]`. The actual message is
`req.Msg` (a `*T`); the envelope around it carries the request and response
headers, trailers, peer information, and the RPC `Spec` (which includes the
procedure name). That envelope is how metadata travels alongside the payload:
auth tokens, request IDs, idempotency keys, and trace context ride in
`req.Header()`, and the handler sets response metadata via
`connect.NewResponse(msg).Header()`. Returning the bare message would compile for
the value but would throw away your only channel for headers and trailers, so the
envelope is not ceremony — it is the metadata contract.

### The error model is a contract, not cosmetics

`connect.NewError(code, err)` produces an error carrying a gRPC-compatible code:
`CodeNotFound`, `CodeInvalidArgument`, `CodeUnauthenticated`,
`CodePermissionDenied`, `CodeUnavailable`, `CodeInternal`, and the rest of the
canonical set. The code is the machine-readable part of your API that clients
build retry and alerting logic on. `CodeInvalidArgument` says "do not retry, the
request is malformed"; `CodeUnavailable` says "retry with backoff, I am
temporarily down." Collapsing every failure to `CodeUnknown` (which is what a
plain `errors.New` or `fmt.Errorf` returned from a handler becomes on the wire)
destroys the client's ability to tell those apart. Choosing the code is a design
decision on every failure path.

For remediation detail beyond the code, `connect.NewErrorDetail(msg)` attaches a
typed Protobuf message — the well-known `errdetails.BadRequest` with per-field
`FieldViolation`s, `errdetails.ResourceInfo`, `errdetails.RetryInfo`, and so on.
The client reads these back as structured data instead of string-parsing an error
message. Clients unwrap the whole thing with `connect.CodeOf(err)` for the code
and `errors.As` into a `*connect.Error` (`var cerr *connect.Error;
errors.As(err, &cerr)`) to reach `Message()` and `Details()`; each detail's
`Value()` returns the decoded `proto.Message`.

### Interceptors are the middleware layer

Cross-cutting concerns — authentication, structured logging, metrics, timeouts,
and panic recovery — live in interceptors, applied once and uniformly rather than
copy-pasted into every method. The `connect.Interceptor` interface has three
methods: `WrapUnary`, `WrapStreamingClient`, and `WrapStreamingHandler`, each
transforming one function into a wrapped one. When you only care about unary RPCs,
`connect.UnaryInterceptorFunc` — a `func(connect.UnaryFunc) connect.UnaryFunc` —
is the shortcut; it wraps unary calls and passes streaming calls through
untouched. Interceptors attach with `connect.WithInterceptors(...)` on both
handlers and clients, and `connect.WithRecover` gives you panic recovery that
turns a handler panic into a coded error instead of a dropped connection.

Ordering matters and is easy to get wrong. Interceptors wrap outermost-first: the
first interceptor in the `WithInterceptors` list is the outermost wrapper, so it
runs first on the way in and last on the way out. Put auth *before* logging and a
rejected request never reaches the logger as if it were accepted; put logging
first and you log unauthenticated requests as though they succeeded. The order in
the slice is the order of the middleware chain.

### Streaming semantics and their cost

Server streaming — one request, many responses — uses
`(*connect.ServerStream[T]).Send` in the handler and a `Receive()`/`Msg()`/
`Err()` loop on the client, where the client method returns a
`*connect.ServerStreamForClient[T]`. Streaming requires HTTP/2 and a more careful
client, and it complicates load balancing (a long-lived stream pins a client to
one backend), timeouts, and back-pressure. Reserve it for genuinely long-lived or
large-result responses — a change feed, a log tail, a large export — and reach
for a paginated unary call for small, bounded result sets. A streaming RPC in
front of a result that fits in one response buys nothing and costs operational
complexity.

The non-negotiable discipline in a streaming handler is respecting context
cancellation. When a client disconnects, its context is cancelled; a handler that
loops on `Send` without checking `ctx.Err()` (or without selecting on
`ctx.Done()`) keeps producing into a dead stream and leaks a goroutine. On the
client side, a `false` from `Receive()` is ambiguous — it means end-of-stream
*or* an error — so you must check `stream.Err()` after the loop and `Close()` the
stream when done.

### The schema-first workflow

Services are defined in `.proto`, and `buf` (driving `protoc-gen-go` and
`protoc-gen-connect-go`) generates the message types, the handler interface, and
the client. The generated `*connect` package is the boundary you code against; you
implement the handler interface and call the client, and never hand-write wire
encoding. In this lesson the hand-written code lives behind a `//go:build connect`
tag precisely because that generation step, and the `connectrpc.com/connect`
dependency, are external to the offline gate.

### Testing in-process

The senior default is two tiers. Unit-test handler logic directly by constructing
a `*connect.Request[T]` with `connect.NewRequest` and calling the method — no
network, no server. Then integration-test the full serialize/transport/
deserialize path with `net/http/httptest.NewServer(mux)` and a real Connect
client pointed at `ts.URL`. That exercises the actual protocol in CI without
binding a well-known port or standing up a real server, and it catches the
protocol-level mistakes (wrong codes on the wire, missing HTTP/2 for streaming)
that a direct method call cannot. Streaming handlers can additionally be unit
-tested by passing a fake stream that captures `Send` calls.

## Common Mistakes

### Returning the bare message instead of the envelope

Reading the request payload directly instead of through `req.Msg`, or returning a
`*T` instead of `connect.NewResponse(msg)`, throws away headers and trailers even
when it compiles. The envelope is how response metadata (a request ID echo, a
cache directive, a trailer) leaves the handler; skip it and you have no way to
set that metadata.

### Serving only HTTP/1.1

Calling plain `http.ListenAndServe` (or setting only `SetHTTP1(true)`) without
unencrypted HTTP/2 or `h2c` leaves gRPC clients and every streaming RPC broken
while `curl` still works, so the gap hides until a real consumer hits it. Enable
`SetUnencryptedHTTP2(true)` (or wrap with `h2c` on pre-1.24 Go) and test a gRPC
call, not just a JSON one.

### Losing the code with errors.New / fmt.Errorf

Returning a plain error from a handler collapses to `CodeUnknown`/`CodeInternal`
on the wire, so the client cannot distinguish a validation error (do not retry)
from unavailability (retry with backoff). Always wrap with `connect.NewError` and
a deliberately chosen code.

### Putting remediation only in the message string

Stuffing the actionable detail (which field was invalid, how long to wait) into
the human-readable message forces clients to parse strings. Attach a typed
`connect.NewErrorDetail` instead, so clients get structured, machine-readable
remediation.

### Getting interceptor order wrong

Assuming `WithInterceptors` order is irrelevant. Interceptors wrap
outermost-first, so a logging interceptor placed before an auth interceptor logs
unauthenticated requests as if they were accepted. Order the slice as the
middleware chain: auth before logging when you do not want to log rejected calls.

### Ignoring context cancellation in a stream

A server-streaming loop that never checks `ctx.Err()` keeps calling `Send` after
the client disconnects, leaking the goroutine forever. Select on `ctx.Done()` or
check `ctx.Err()` each iteration and return.

### Mishandling the client streaming loop

Forgetting to check `stream.Err()` after the `Receive()` loop treats an error as
a clean end-of-stream, and forgetting to `Close()` the stream leaks the
connection. A `false` from `Receive()` is only clean if `Err()` is `nil`.

### Trying to gate/compile bar-mode code offline

The generated `.pb.go` and the `connectrpc.com/connect` module are not present in
the offline gate, so a build will fail with "no required module provides
package." That is expected: this is a bar-mode lesson. Correctness is
`gofmt`-clean, API-honest code plus a described `buf` codegen step, and the
external/network code stays behind the `//go:build connect` tag.

### Reaching for streaming when pagination would do

Adding HTTP/2-only streaming for a small, bounded result complicates load
balancing and timeouts for no benefit. Use a paginated unary call unless the
result is genuinely long-lived or too large for one response.

Next: [01-unary-service-multi-protocol.md](01-unary-service-multi-protocol.md)
