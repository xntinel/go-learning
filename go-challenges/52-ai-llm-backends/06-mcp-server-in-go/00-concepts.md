# Building MCP Servers in Go — Concepts

An MCP server is not a chatbot feature. It is a typed RPC service that an LLM
host — Claude Desktop, an agent runtime, an IDE — discovers and drives on the
model's behalf. The Model Context Protocol (MCP) standardizes the wire so any
compliant host can enumerate your capabilities and call them without bespoke
glue. For a senior backend engineer the mental model is deliberately boring: a
self-describing JSON-RPC 2.0 service with three kinds of endpoint, fronted by one
of two transports, whose argument surface is driven by an autonomous, sometimes
hallucinating client. Get that framing right and the SDK is small; get it wrong
and you have published your production internals to whatever can reach the port.
This file is the conceptual foundation for the three independent exercises that
follow: a stdio tool server, a resource-and-prompt server, and the same server
exposed over authenticated Streamable HTTP.

## Concepts

### What MCP actually is

MCP is a JSON-RPC 2.0 protocol that standardizes how an LLM host discovers and
invokes a server's capabilities. The host connects, negotiates capabilities, and
then issues typed requests — `tools/list`, `tools/call`, `resources/read`,
`prompts/get`, and so on. You almost never touch the raw JSON: the official Go
SDK (`github.com/modelcontextprotocol/go-sdk/mcp`, v1.x, maintained with Google)
turns a Go function into a spec-conformant RPC, and reflects your Go structs into
the JSON Schema the model reads. The value of the standard is uniformity — the
same server works with every host — and the cost is that your tool surface is now
a public contract that clients depend on, versioned like any API.

The protocol has three primitives, and choosing the wrong one is the single most
common design error:

- Tools are model-invoked, effectful RPCs. The model decides to call them, with
  arguments it constructs. `search`, `create_ticket`, `run_query` are tools.
- Resources are application-controlled, read-only data addressable by URI, with a
  MIME type. They are side-effect-free and cacheable: a config document, a
  runbook, a schema dump. The host reads them; the model does not "call" them.
- Prompts are user-invoked, reusable message templates the client can fetch and
  parameterize — a "summarize this incident" template, say. They are surfaced to
  the user (often as slash-commands), not auto-invoked by the model.

The dividing line is semantic, not syntactic. If an operation has a side effect
or takes free-form arguments the model must reason about, it is a tool. If it is
a stable document you can name with a URI and hand back byte-for-byte, it is a
resource — modeling it as a tool robs the host of caching and addressability and
tempts the model to treat a safe read as an effectful action.

### The two transports and when each applies

The same `*mcp.Server` runs behind either transport; you choose based on where
the host lives.

Stdio (`&mcp.StdioTransport{}`, driven by `server.Run(ctx, transport)`) runs your
server as a subprocess of exactly one host. Messages are newline-delimited JSON
over the process's stdin and stdout. There is a single client, and no
authentication layer is needed because the server inherits the trust of the
process that launched it — the host already decided to run your binary. This is
the shape for a local tool a desktop app or IDE spawns.

Streamable HTTP (`mcp.NewStreamableHTTPHandler`) is a remote, multi-session
transport over a single HTTP endpoint. One handler serves many concurrent
sessions; responses can stream as `text/event-stream` (SSE) or come back as a
single `application/json` body. This is a public network surface. It gets exactly
the treatment any internal API exposed to the outside gets: authentication,
authorization, rate limiting, request logging, and a health check — enforced
before a request ever reaches a tool. Treating the HTTP transport as "just
another transport" and skipping auth is how internal capabilities leak.

### Schema-driven contracts

The generic `mcp.AddTool[In, Out]` reflects your Go input and output structs into
the JSON Schema advertised for the tool. Field names come from `json` tags;
descriptions and constraints come from `jsonschema` tags. That schema is the
model's only documentation of your tool — the model never sees your Go code or
your comments, only the schema. So the description on a field is functional, not
cosmetic: "the ISO-8601 start date, inclusive" versus "start" is the difference
between the model filling the argument correctly and the model guessing. Vague or
missing `jsonschema` descriptions are the usual root cause of "the model keeps
calling my tool wrong." The SDK also validates incoming arguments against that
schema before your handler runs, so structurally invalid input is rejected for
you — but the schema only encodes what you expressed in it.

### Tool errors versus protocol errors

This distinction is the crux of a robust tool, and the Go SDK draws it precisely.
A validation failure or a business error ("record not found", "quota exceeded")
is a tool error: it belongs inside the `CallToolResult` with `IsError` set to
true, so the model sees the failure as content and can recover — retry with
different arguments, apologize, choose another path. A protocol error, by
contrast, means the RPC machinery itself failed: no such tool, the server does
not support tools, a malformed request. That surfaces as a JSON-RPC error and the
model cannot meaningfully recover from it.

With the generic handler (`ToolHandlerFor`), the SDK makes the safe choice the
default: if your handler returns an ordinary Go `error`, the SDK packs it into
the result's `Content` and sets `IsError` — it becomes a tool error, not a
protocol error. So returning `fmt.Errorf("title must not be empty")` from a tool
handler is exactly right; the model receives it as a recoverable condition. You
can also build the result explicitly and call `result.SetError(err)` when you
want to control the content text. Genuine protocol errors are raised by the
framework (unknown tool) or, if you drop down to the low-level `ToolHandler`,
by returning an error there. The mistake to avoid is assuming a returned error
"crashes the RPC"; under the generic handler it does the opposite, and that is
what you want.

### The tool surface is an untrusted-input boundary

The client driving your tools is an autonomous model. It may hallucinate
arguments, retry aggressively, or — via prompt injection in the data it is
reading — be steered adversarially. Every argument is therefore untrusted input,
exactly like a request body from the open internet. The generated schema handles
shape and type; you still enforce the semantics: bounds on a limit, an
enumeration on a status, a length cap on a free-text field, and above all no
unsanitized interpolation of model-supplied strings into SQL, shell, or
filesystem paths. A tool that concatenates a model argument into a query is an
injection vector with an LLM as the attacker's proxy. Effectful tools also
authorize inside the handler and run with least privilege: the fact that the
model asked to delete something is not authorization to delete it.

### Sessions, context, and cancellation

Each connection is a session, and every handler receives a `context.Context` tied
to it. When the client disconnects or cancels the request, that context is
cancelled — so a long-running tool must propagate `ctx` into its downstream calls
and honor `ctx.Done()`, or a cancelled request leaks a goroutine and whatever
resources it holds. This is ordinary Go server discipline; MCP just makes it
non-optional because agents cancel and re-plan constantly. Stateless HTTP mode
(`StreamableHTTPOptions{Stateless: true}`) trades per-session state and
server-initiated messages for horizontal scalability: any process behind a load
balancer can serve any request, because there is no session affinity to preserve.
Choose it when you scale out and do not need the server to push notifications to
the client.

### Capability negotiation and versioning

Server and client exchange capabilities when the session initializes. The SDK
advertises a capability when you register the corresponding feature: add a tool
before connecting and the server announces tool support; add a resource and it
announces resources. The protocol itself is versioned and evolving — some
features carry deprecation windows — so pin the SDK version and treat your tool
schemas as a versioned API contract. Renaming a tool or tightening its schema is
a breaking change to every client that learned the old shape.

### Idempotency and side effects

An agent does not call your tool exactly once. It may retry after a timeout,
repeat a step while re-planning, or issue the same call twice because two
branches of its reasoning both needed it. A mutating tool must therefore be safe
under repetition: accept an idempotency key so a retried "create" returns the
original result instead of a duplicate, or design the operation to converge (set
state to a value rather than increment it). Assuming exactly-once invocation is a
correctness bug that only shows up in production, under load, when an agent
retries.

## Common Mistakes

### Modeling read-only data as a tool (or a mutation as a resource)

Wrong: exposing a static config document as a `get_config` tool, so the host
cannot cache it or address it by URI and the model treats a safe read as an
effectful call. Equally wrong: hiding a mutation behind a resource read.

Fix: if it is stable, addressable, and side-effect-free, register it as a
resource with a URI and MIME type; if it takes free-form arguments or has an
effect, make it a tool. Let the semantics pick the primitive.

### Returning business failures as protocol errors

Wrong: under the low-level handler, letting a "not found" bubble up as a protocol
error, so the model sees an opaque transport failure it cannot recover from.

Fix: return the condition as a tool error — with the generic `ToolHandlerFor`,
just `return nil, out, fmt.Errorf(...)` (or `result.SetError(err)`), which sets
`IsError` and packs the message into `Content` for the model to read and react to.

### Trusting model-supplied arguments

Wrong: relying only on the generated schema, then interpolating a model string
straight into SQL, a shell command, or a file path.

Fix: validate semantics beyond the schema (bounds, enums, lengths), and use
parameterized queries / safe path joins / argument arrays. Treat every argument
as hostile input, because the model can be steered by injected data.

### Thin or missing schema descriptions

Wrong: `jsonschema:""` or no tag, then blaming the model for calling the tool
incorrectly.

Fix: write a precise `jsonschema` description and constraints for every field.
The schema is the only spec the model has; make it say what you mean.

### Exposing the HTTP transport without auth

Wrong: mounting `NewStreamableHTTPHandler` on a public mux with no
authentication, rate limiting, or logging — publishing internal capabilities to
anyone who can reach the endpoint.

Fix: wrap the handler in middleware that authenticates and authorizes before the
request reaches a tool, add rate limiting and request logging, and keep an
unauthenticated health endpoint separate.

### Ignoring the handler context

Wrong: a tool that calls a database or HTTP dependency with
`context.Background()` instead of the handler's `ctx`, so a client disconnect
cannot cancel the work and goroutines leak.

Fix: thread the handler `ctx` into every downstream call and select on
`ctx.Done()` in long loops.

### Assuming a tool is called at most once

Wrong: a `create` tool that inserts unconditionally, producing duplicates when
the agent retries the step.

Fix: accept an idempotency key (or design for convergence) so a repeated call
returns the original outcome instead of a second side effect.

### Reaching for a stale or third-party API surface

Wrong: writing against old SDK shapes (`NewToolFeature`, `AddFeatures`,
`NewStreamTransport`) or copying `mark3labs/mcp-go` signatures, which do not exist
in the official SDK.

Fix: use the current official surface — `mcp.NewServer`, the generic
`mcp.AddTool`, `server.Run(ctx, transport)`, `AddResource`, `AddPrompt`,
`NewStreamableHTTPHandler` — and verify each signature on pkg.go.dev.

Next: [01-stdio-tools-server.md](01-stdio-tools-server.md)
