# Choosing Between REST, gRPC, and Connect — Concepts

This is the capstone of the RPC/API chapter, and it is deliberately not "how to
call one framework". By now you have built a Connect service, a grpc-gateway, a
buf schema module, a GraphQL server, and a versioning strategy. The senior skill
this lesson trains is the one you are actually paid for: standing in a design
review and defending an API-style choice against concrete non-functional
requirements rather than tribal preference. The three realistic options a Go
backend team faces in 2026 are REST/JSON over `net/http`, gRPC over HTTP/2, and
Connect (which speaks the Connect protocol, gRPC, and gRPC-Web from one handler).
The through-line of this file is that these are not mutually exclusive religions.
Connect specifically collapses the old "gRPC is fast but not browser/curl
friendly" trade-off; grpc-gateway and Connect's JSON codec collapse the "protobuf
is not human-readable" trade-off. The exercises then force you to *measure* the
trade-offs instead of repeating blog-post folklore.

## Concepts

### The decision axes, made concrete

A framework is downstream of requirements, never upstream. Before you name REST,
gRPC, or Connect, you should be able to write down where your service sits on each
of these axes, because the axes decide the framework, not the other way around.

- **Payload / wire cost.** Self-describing JSON carries field names and typed text
  on every message; protobuf drops the names and uses a compact binary encoding.
  For structured, numeric, repetitive data this is a several-fold size difference
  that shows up on your egress bill and your tail latency. For low-volume,
  human-read traffic it is noise.
- **Streaming shape.** Is the interaction unary request/response, server-streaming
  (one request, many responses), client-streaming, or bidirectional? And if it is
  a stream, does it terminate at a browser, another service, or a human with curl?
  The answer eliminates whole options.
- **Client reach.** A native mobile or service client can speak anything. A
  browser cannot speak raw gRPC. A human debugging at 2am wants curl and a JSON
  body, not `grpcurl` and a hex dump.
- **Schema and codegen tooling.** OpenAPI plus generators, or protobuf plus buf?
  This decides how disciplined your compatibility story is and how much build
  machinery your team carries.
- **Observability and interoperability.** Your existing L7 proxies, load
  balancers, tracing, auth mesh, and API gateway already understand some
  protocols better than others. The "best" protocol on paper loses to the one your
  infrastructure already terminates and inspects.

Everything below is just these axes applied to the three concrete options.

### Why gRPC is fast AND awkward

gRPC's speed and its awkwardness come from the same design. It runs over HTTP/2,
frames messages in a length-prefixed binary envelope, and signals the end of a
call and its status code in HTTP/2 *trailers* (headers sent after the body).
Trailers are the sticking point: browsers have no API to read HTTP/2 trailers, so
a browser cannot speak gRPC natively at all. The industry answer was gRPC-Web, a
variant that moves the trailers into the body so a browser can parse them, plus a
proxy (classically Envoy) to translate gRPC-Web to real gRPC in front of your
service. And because the payload is framed binary, curl cannot poke a gRPC
endpoint; you need `grpcurl`, which must fetch or be given the schema. This single
fact — trailers make gRPC un-browsable and un-curl-able — is what motivated
Connect's design.

### What Connect actually is

Connect is one handler that speaks three protocols with no proxy: the Connect
protocol, gRPC, and gRPC-Web. The Connect protocol itself is deliberately boring:
it is a plain HTTP POST to a `/package.Service/Method` path, with a request body
that is either protobuf or JSON, and it uses ordinary HTTP status semantics rather
than trailers for unary calls. So the same endpoint is gRPC-fast for service
clients that opt into `WithGRPC()`, and at the same time curl-and-browser-friendly
for humans and JavaScript over plain HTTP POST.

The crucial and often-missed detail: the *handler* is protocol-agnostic by
default. You register it once, and it inspects each incoming request to decide
whether it is Connect, gRPC, or gRPC-Web. Only *clients* choose a protocol, via
options like `connect.WithGRPC()` or `connect.WithGRPCWeb()`; the default client
uses the Connect protocol. Exercise 2 proves this concretely by calling one
running handler with a Connect client, a gRPC client, and a raw `http.Post` of a
JSON body, and getting the same answer from all three.

### h2c, and why it matters here

gRPC needs HTTP/2. In production you usually get HTTP/2 for free because you serve
TLS and negotiate it in the ALPN handshake. But in a cluster where a mesh
terminates TLS at the sidecar, or in local development, you serve cleartext, and
then you need HTTP/2 without TLS — "h2c", HTTP/2 cleartext. Historically Go
programs wrapped their handler with `golang.org/x/net/http2/h2c`. As of Go 1.24
this is configured directly on the standard library server through the
`http.Server.Protocols` field, which is a `*http.Protocols`:

```go
p := new(http.Protocols)
p.SetHTTP1(true)
p.SetUnencryptedHTTP2(true)
srv := &http.Server{Addr: addr, Handler: mux, Protocols: p}
```

Both switches matter. `SetUnencryptedHTTP2(true)` lets a gRPC client connect over
cleartext h2c. `SetHTTP1(true)` keeps HTTP/1.1 alive so curl and browser JSON
POSTs still work. Forgetting `SetHTTP1` silently breaks every non-gRPC client;
setting neither breaks gRPC. This is a real, easy-to-hit foot-gun and the reason
the field is a pointer you must initialize before you touch it.

### Payload cost is measurable, not mythical

"Protobuf is smaller" is true and quantifiable, and you should quantify it on your
own message rather than quote someone's benchmark. Protobuf omits field names on
the wire and encodes integers as varints (small numbers take one byte) with zigzag
for signed values, so for structured, numeric, repeated data it is typically
several times smaller than the equivalent JSON, which spends bytes on every field
name, every quote, and the decimal text of every number. JSON's wins are real too:
it is human-readable, it needs no codegen, and every tool on earth speaks it. The
honest way to decide is to measure `proto.Size(m)` against `len(jsonBytes)` for
*your* message; Exercise 1 builds exactly that harness. Keep two JSON traps in
mind: JavaScript numbers are IEEE-754 doubles, so an `int64` above 2^53 loses
precision in a browser, and for that reason the protobuf-to-JSON mapping emits
`int64` and `uint64` fields as quoted strings. A number that is an integer in your
Go struct can arrive as a string in a JSON client, and vice versa.

### Streaming is a family, not a checkbox

"Do we need streaming?" is the wrong question; "which streaming shape, to whom?"
is the right one. There are three practical server-to-client options and they are
not interchangeable:

- **gRPC / Connect server-streaming** gives typed, backpressured, multiplexed
  streams. It is the right tool for internal service-to-service feeds where both
  ends have generated stubs and you want one message type end to end.
- **Server-Sent Events (SSE)**, the `text/event-stream` content type, is a
  one-line-to-adopt browser primitive: `new EventSource(url)` on the client, and
  on the server you write `data: <json>\n\n` frames and flush. It rides ordinary
  HTTP/1.1, survives every proxy and load balancer that speaks HTTP, and the
  browser auto-reconnects for you.
- **Newline-delimited JSON (NDJSON)** over a chunked response is the curl-friendly
  firehose: one JSON object per line, readable with `while read line`.

Choosing gRPC streaming for a browser dashboard is the classic over-engineering
mistake. SSE ships in a day, needs zero client tooling, and tolerates the proxies
you already run; reach for bidi gRPC when you genuinely need typed, two-way,
internal streams. Exercise 3 builds all three of the same "tail live order events"
feature so the trade-off is concrete rather than asserted.

### Flushing is the failure mode of HTTP streaming

Any streaming-over-`net/http` handler has one way to be silently broken: it never
flushes. The `http.ResponseWriter` buffers, so if you write frames and return
without flushing, the client sees the whole stream at once when the handler ends,
which defeats the point of streaming. The modern, wrapper-safe way to flush is
`http.NewResponseController(w).Flush()`, which returns an error and correctly
unwraps a `ResponseWriter` that middleware has wrapped. The old approach,
`w.(http.Flusher).Flush()`, both ignores errors and panics the moment any
middleware wraps the writer, because the wrapper does not implement
`http.Flusher`. A subtle related point: you often want to flush the response head
before the first event so a client that connects before any data still receives
the status line and can start reading.

### Schema evolution ties back to versioning

The wrong protocol choice is expensive to live with in proportion to how it
evolves. Protobuf field numbers give you disciplined backward and forward
compatibility almost for free: add a field with a new number, old readers ignore
it, new readers tolerate its absence. JSON/REST relies on additive conventions and
OpenAPI discipline you enforce by hand or with breaking-change tooling. This is the
same versioning discipline from the previous lesson, now as an input to the
style decision: a public API you will maintain for a decade weighs schema
evolution far more heavily than an internal endpoint you can redeploy in lockstep.

### Interop and org gravity

Finally, respect the gravity of what already exists. Public and partner APIs skew
REST/OpenAPI because that is what external consumers expect and what their tools
generate against. Internal high-fanout microservices skew gRPC/Connect for the
payload and streaming wins. Anything browser-facing needs a JSON or gRPC-Web path.
The best protocol on paper loses to the one your gateways, meshes, and client teams
already support.

### The synthesis: a repeatable rubric

Compress all of the above into a decision you can defend out loud:

- Browser-facing with no proxy budget: Connect or REST.
- Polyglot internal, high-throughput, service-to-service: gRPC or Connect.
- Public, human-facing: REST/OpenAPI, or Connect with its JSON protocol.
- Streaming to browsers: SSE first; reach for bidi gRPC only for internal typed
  streams.

For a Go shop, Connect is frequently the low-regret default precisely because it
does not force the browser-versus-performance trade-off: one handler is gRPC-fast
for services and curl/browser-friendly for humans. That is not a claim to memorize;
the exercises make you prove it.

## Common Mistakes

### Treating REST, gRPC, and Connect as mutually exclusive religions

Wrong: picking a favorite framework and arguing for it everywhere. Fix: they
compose. Connect serves gRPC and JSON from one handler; grpc-gateway puts a REST
face on a gRPC service. Decide per requirement, not per tribe, and mix them in one
system where the requirements differ.

### Enabling h2c incorrectly in Go 1.24+

Wrong: assigning to `http.Server.Protocols` without initializing the pointer, or
setting only `SetUnencryptedHTTP2(true)` and forgetting `SetHTTP1(true)`. The first
panics on a nil pointer; the second silently breaks curl and browser clients while
gRPC keeps working, so the failure looks mysterious. Fix:
`p := new(http.Protocols); p.SetHTTP1(true); p.SetUnencryptedHTTP2(true);
srv.Protocols = p` so both the gRPC (h2c) path and the HTTP/1.1 path work.

### Streaming without flushing

Wrong: writing SSE or NDJSON frames and trusting the server to send them; they
buffer until the handler returns, and the client sees one burst. Fix: call
`http.NewResponseController(w).Flush()` after each frame and check its error.

### Reaching for http.Flusher via a type assertion

Wrong: `w.(http.Flusher).Flush()`. This panics the instant middleware wraps the
`ResponseWriter`, because the wrapper does not satisfy `http.Flusher`. Fix: use
`http.NewResponseController(w).Flush()`, which unwraps to find the real flusher and
returns an error you can handle.

### Assuming gRPC just works in the browser or with curl

Wrong: expecting a browser or curl to hit a raw gRPC endpoint. Browsers need
gRPC-Web (which Connect serves natively) and humans debug via the Connect JSON
protocol or `grpcurl`, not curl against binary-framed gRPC. Fix: if you need
browser or human reach, serve Connect or REST, not bare gRPC.

### Quoting "protobuf is 5x smaller" without measuring

Wrong: citing a size ratio from a blog post. The ratio depends heavily on how
numeric versus string your data is and how repetitive it is; for short,
string-heavy messages the gap narrows and JSON's readability may win. Fix: measure
`proto.Size(m)` against `len(json.Marshal(...))` on your actual message, as
Exercise 1 does.

### Mixing proto v1 and v2 APIs

Wrong: passing a `github.com/golang/protobuf` (v1) message to
`google.golang.org/protobuf/proto.Marshal` (v2). Fix: use the v2 module
throughout; `proto.Message` is `protoreflect.ProtoMessage`, and legacy messages are
bridged with `protoadapt` when you must interoperate.

### Comparing decoded protobuf messages with reflect.DeepEqual or ==

Wrong: asserting message equality in tests with `==` or `reflect.DeepEqual`.
Generated messages carry unexported bookkeeping state, so naive equality is both
wrong and fragile. Fix: use `proto.Equal`, or `protocmp.Transform()` with
`cmp.Diff` for a readable diff.

### Picking gRPC streaming for a browser dashboard

Wrong: standing up a bidi gRPC stream and a gRPC-Web proxy so a dashboard can show
live updates. Fix: SSE ships faster, survives every HTTP proxy, and auto-reconnects
in the browser with no client library. Save typed bidi gRPC for internal
service-to-service streams that actually need it.

Next: [01-wire-format-cost-model.md](01-wire-format-cost-model.md)
