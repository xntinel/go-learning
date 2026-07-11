# REST/JSON Gateways with grpc-gateway — Concepts

You already own a gRPC service. It is strongly typed, contract-first, and internal:
other services dial it, the wire format is protobuf, and the `.proto` is the single
source of truth. Now product wants a plain REST/JSON surface for browsers, webhooks,
mobile clients, and people running `curl`. The wrong move is to hand-write a second
HTTP service that re-implements the same business logic — two code paths that drift,
two validation layers, two places to fix a bug. `grpc-gateway` is the move that keeps
one `.proto` authoritative: it is a `protoc`/`buf` plugin that reads `google.api.http`
annotations on your service and generates a *reverse proxy* — an `http.Handler` built
around `runtime.ServeMux` — that transcodes an incoming HTTP request (verb, path,
query, body) into a gRPC call and marshals the reply back to JSON. The gRPC server and
the REST facade are generated from the same definition, so they cannot drift.

The senior work here is not "turn on the plugin". It is the operational judgment around
it: whether to register the concrete server in-process or dial it over a socket; how the
proto field names leak into your public JSON contract through `protojson`; how gRPC
status codes map deterministically to HTTP status and a stable error envelope; and how
auth and tracing headers cross the HTTP/gRPC boundary. Get the marshaler or the error
handler wrong and you ship an inconsistent, unversionable public API. Get registration
wrong and you add a needless localhost hop and a second thing that can fail. This file is
the conceptual foundation for the three exercises that follow; read it once and you can
reason through each.

## Concepts

### What transcoding actually is

Transcoding is a mechanical mapping from an HTTP request to a unary (or server-streaming)
gRPC call and back. The generated proxy owns a `runtime.ServeMux` — a router that is
*not* a `net/http` mux. Each annotated RPC becomes a route: the annotation's verb keyword
sets the HTTP method, its URL template sets the path (with capture segments), the request
message is populated from path parameters, the JSON body, and query parameters, the RPC is
invoked, and the response message is marshaled to JSON. Nothing about your handler changes;
you write it once as a gRPC method and the gateway exposes it twice. The `.proto` stays the
one place that defines both surfaces, and code generation keeps them in lockstep.

### The google.api.http mapping model

The annotation lives on the method as `option (google.api.http)`. Its shape is a small,
precise grammar (defined by `google/api/http.proto`, the `HttpRule` message):

- The verb keyword — `get`, `post`, `put`, `patch`, `delete` — sets both the HTTP method
  and the URL template in one line: `get: "/v1/orders/{id}"`.
- `{name}` captures a single path segment into the request field `name`; `{name=segments/*}`
  captures a multi-segment pattern. A nested field is addressed with a dotted path, e.g.
  `{order.id}` binds the `id` field of the `order` sub-message.
- `body: "*"` binds the entire JSON request body onto the top-level request message;
  `body: "field"` binds the body onto exactly one sub-message field and leaves the rest of
  the request to path/query. `response_body: "field"` narrows what is returned.
- Any request field not consumed by the path or the body becomes a *query parameter*. This
  is how `GET /v1/orders?page_size=50&state=OPEN` populates a list request. Unknown query
  parameters are ignored by default, so old clients sending stale params do not 400.
- `additional_bindings { ... }` attaches extra routes to the same RPC. This is how you keep
  a legacy `/orders/{id}` path alive next to a new `/v1/orders/{id}` — both resolve to one
  handler, so versioning and migration cost you no duplicated logic.

### Registration topology: in-process vs dialed

The generated code gives you three ways to wire the service into a mux, and the choice is a
deployment decision, not a detail.

- `RegisterXHandlerServer(ctx, mux, srv)` wires the *concrete server object* directly into
  the mux. There is no socket, no marshaling to the wire and back — the gateway calls your
  Go methods in-process. This is the right default for a single binary that serves both gRPC
  and REST, and it is what tests use: zero network hops, one failure domain, one process.
- `RegisterXHandlerFromEndpoint(ctx, mux, endpoint, opts)` dials a real gRPC endpoint and
  registers a client. `RegisterXHandler(ctx, mux, conn)` does the same given a `*grpc.ClientConn`
  you built. Use these when the gateway is a *standalone* proxy or sidecar in front of a gRPC
  service that lives in another process or on another host. They add a network hop, its own
  timeouts and retries, and a second failure domain.

The trap is dialing `localhost` to reach a server that lives in the same binary: you pay for
serialization, a loopback round trip, and connection management to call a function you could
have called directly. Reach for `FromEndpoint` only when there really is a separate process.
The current constructor is `grpc.NewClient(target, opts...)`; `grpc.DialContext`/`grpc.Dial`
with `WithBlock` are deprecated. For a plaintext dev endpoint you must pass
`grpc.WithTransportCredentials(insecure.NewCredentials())` or the dial fails on transport
security.

### protojson is your public JSON contract

The gateway marshals messages with `protojson`, and its options are part of your versioned
API — not an implementation detail you can flip later.

- Default `protojson` emits **lowerCamelCase** field names (`pageSize`). Setting
  `UseProtoNames: true` keeps the proto **snake_case** names (`page_size`). Whichever you
  pick, clients depend on it; changing it is a breaking change.
- `EmitUnpopulated` controls whether zero-valued fields appear in the output. With it off
  (the default), a `0`/`""`/`false` field is omitted; with it on, it is present. Clients that
  distinguish "absent" from "zero" care deeply. Flipping this later silently breaks them.
- On the way in, `UnmarshalOptions{DiscardUnknown: true}` makes the gateway tolerate unknown
  incoming fields instead of rejecting them, which is what you want for forward-compatible
  clients that send fields your current schema does not know yet.

You install these by building a `runtime.JSONPb` and registering it with
`runtime.WithMarshalerOption(runtime.MIMEWildcard, ...)`. Leave them at defaults and you have
made a contract decision by accident.

### The HTTP/gRPC boundary for headers and metadata

HTTP headers and gRPC metadata are different namespaces, and the gateway does not blindly
copy one into the other. By default only a small whitelist of headers crosses (the behavior
of `runtime.DefaultHeaderMatcher`). To forward `Authorization` and `X-Request-Id` into gRPC
metadata you install `runtime.WithIncomingHeaderMatcher` with a function that maps the header
names you trust and falls back to the default for the rest. Forwarding *everything* is the
opposite mistake: it leaks internal headers into the backend. `runtime.WithMetadata` injects
derived values (the matched path pattern, the RPC method, a tenant id) into the outgoing
metadata regardless of the incoming request. In the other direction,
`runtime.WithForwardResponseOption` plus `runtime.ServerMetadataFromContext` let a handler's
server-set metadata surface on the HTTP response — the canonical use is promoting an
`x-http-code` to the real HTTP status, or a `location` value to a `Location` header on a
`201 Created`. Auth and tracing correctness live entirely in these four hooks.

### Status and error mapping is a contract, not an afterthought

gRPC has its own error model: a `codes.Code` plus a message plus optional typed details. HTTP
clients need an HTTP status *and* a stable, machine-readable body. The canonical code→status
mapping is `runtime.HTTPStatusFromCode`: `NotFound → 404`, `InvalidArgument → 400`,
`PermissionDenied → 403`, `Unauthenticated → 401`, `AlreadyExists → 409`,
`Unavailable → 503`, and so on. Never hand-roll this table; you will drift from the canonical
one. HTTP status alone is lossy — `400` covers many distinct failures — so machine clients
depend on a JSON envelope carrying the numeric code, the string code, the message, and the
details. You produce that by installing `runtime.WithErrorHandler` with an
`ErrorHandlerFunc` that pulls the status out with `status.FromError`, maps the code, and
writes your envelope instead of the default shape. Gateway-level failures that never reach an
RPC — an unknown path, a wrong method — are handled separately by
`runtime.WithRoutingErrorHandler`; install one so those return `404`/`405` in the *same*
envelope, not a second error shape your clients have to special-case.

### Failure modes, limits, and where the gateway sits

grpc-gateway transcodes unary and server-streaming RPCs; server streaming becomes
newline-delimited (chunked) JSON. It does **not** transcode client-streaming or bidirectional
RPCs over plain REST — do not design a public REST surface around those. The gateway is an
extra component: in the dialed topology it can fail or add latency independently of the
backend. Code generation depends on the googleapis annotation protos
(`google/api/annotations.proto`, `google/api/http.proto`) being on your include path or in
your `buf` dependencies; forget them and codegen fails or silently emits no gateway. Finally,
the gateway is transcoding, not a general web framework: auth, rate limiting, CORS, and
logging belong in ordinary `http.Handler` middleware wrapping the mux, and OpenAPI/Swagger can
be generated from the same annotations via `protoc-gen-openapiv2` so your docs never drift
from the contract.

## Common Mistakes

### Shipping the wrong JSON casing

Leaving `protojson` at defaults ships lowerCamelCase JSON when your API contract (or existing
REST consumers) expect snake_case. Once clients read `page_size`, adding `UseProtoNames: true`
later — or removing it — is a breaking change. Decide the casing deliberately and pin it.

### Letting zero-valued fields vanish

Relying on the `EmitUnpopulated: false` default means a `0`, `""`, or `false` silently drops
out of responses, which breaks clients that distinguish absent from zero. Flipping it later is
an accidental breaking change. Choose it on purpose per your contract.

### A pointless localhost hop

Using `RegisterXHandlerFromEndpoint` to dial `localhost` when the gRPC service is in the same
binary adds serialization, a loopback round trip, extra timeouts, and a second failure mode.
Use `RegisterXHandlerServer` in-process unless the backend is genuinely a separate process.

### No custom error handler

Without `runtime.WithErrorHandler`, clients get grpc-gateway's default error shape and cannot
rely on a stable `code`/`message`/`details` envelope. And hand-writing an HTTP status table
instead of `runtime.HTTPStatusFromCode` drifts from the canonical mapping the moment a new
code matters.

### Forgetting the routing error handler

Skipping `runtime.WithRoutingErrorHandler` means unknown paths and wrong methods return a
different error shape (and sometimes a different status) than RPC-level errors, so clients face
two envelopes instead of one.

### Assuming all headers pass through

The default matcher whitelists only a few headers, so `Authorization` and custom headers never
reach the handler unless `WithIncomingHeaderMatcher` forwards them. Forwarding everything
blindly is the opposite failure — it leaks internal headers to the backend.

### Missing the annotation dependencies

Omitting `google/api/annotations.proto` and `google/api/http.proto` from your `buf` deps or
`protoc` include paths makes codegen fail or emit no gateway at all. The annotations must
resolve at generation time.

### Expecting streaming REST that does not exist

Client-streaming and bidirectional RPCs do not transcode to REST. Only unary and
server-streaming do. Do not promise a browser a bidi endpoint through the gateway.

### Using the deprecated dial API

`grpc.DialContext`/`grpc.Dial` with `WithBlock` is deprecated; use `grpc.NewClient`. And a
plaintext dev dial without `insecure.NewCredentials()` fails on transport security.

Next: [01-annotated-gateway-transcoding.md](01-annotated-gateway-transcoding.md)
