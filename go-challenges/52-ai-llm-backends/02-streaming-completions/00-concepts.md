# Streaming Completions over SSE — Concepts

Almost no browser talks to Anthropic or OpenAI directly. A browser-to-provider
call would ship your API key to every client, and it would bypass everything a
backend exists to enforce: authentication, authorization, rate limiting, request
logging, billing attribution, and content redaction. So the real job behind
"streaming completions" is not calling an SDK — it is building a token-streaming
gateway. Your service consumes the provider's Server-Sent Events (SSE) stream,
optionally transforms or redacts it, and re-emits SSE to your own clients with
correct flushing, context cancellation, heartbeats, and in-band error signaling.

The happy path is easy: the SDK hands you `stream.Next()` and you print deltas.
The hard parts are the ones that only bite in production. An HTTP 200 does not
mean success, because provider errors arrive mid-stream after the headers are
already on the wire. You must flush after every frame or the client sees nothing
until the very end. You must propagate client disconnect upstream or you keep
paying for tokens nobody will read and leak connections. And a partial stream is
not safely retriable once you have emitted bytes. Getting flush, cancellation,
and mid-stream error handling right is what separates a demo from a gateway that
survives slow clients and buffering reverse proxies under load. This chapter
splits the wire format, the provider client, and the re-streaming server so each
concern can be tested offline and reasoned about on its own.

## Concepts

### Why SSE, and not WebSocket or chunked JSON

Token generation is unidirectional: the server produces, the client consumes.
Server-Sent Events fit that shape exactly. SSE is plain HTTP with a
`Content-Type: text/event-stream` body; it needs no protocol upgrade the way a
WebSocket does, it works through most proxies and CDNs, and the browser
`EventSource` API gives you automatic reconnection keyed on `Last-Event-ID` for
free. A full-duplex WebSocket is overkill when tokens only flow one way, and it
adds an upgrade handshake and a second failure mode. Raw chunked JSON is the
other tempting shortcut, but it lacks event framing (no named event types), lacks
the id/retry reconnection semantics, and forces every client to invent its own
message boundaries. SSE gives you all of that as a tiny, well-specified text
format.

### The wire format, exactly

An event stream is UTF-8 text, processed line by line. Each line is either a
comment or a `field: value` pair. The defined fields are `data`, `event`, `id`,
and `retry`; any other field name is ignored. A single leading space after the
colon is stripped, so `data: hi` and `data:hi` both carry `hi`. The rules that
trip people up:

- Multiple consecutive `data:` lines are concatenated with a newline between
  them. Two `data:` lines carrying `a` and `b` produce the single data value
  `a\nb`, not two events.
- A line that starts with a colon is a comment. It carries no field and is
  ignored by the parser. This is exactly what a heartbeat uses: a bare `:` line
  keeps the connection warm without ever appearing as data.
- A blank line dispatches the accumulated event. Everything buffered since the
  last blank line becomes one event; the buffers then reset.
- Lines may end in LF, CRLF, or a lone CR; a robust decoder handles all three.
- A UTF-8 byte-order mark at the very start of the stream is stripped once.

OpenAI adds one convention on top of the spec: the stream terminates with a
literal `data: [DONE]` line. That sentinel is a terminator, not JSON — trying to
`json.Unmarshal` it is a bug; you recognize it and stop.

### The Anthropic event model

Anthropic streams a structured sequence of named events over SSE:
`message_start`, then for each content block a `content_block_start`, a run of
`content_block_delta` events, and a `content_block_stop`; then a `message_delta`
that carries the final `stop_reason` and cumulative token `usage`; then
`message_stop`. The deltas come in kinds — `text_delta` for prose,
`input_json_delta` for streamed tool-call arguments, `thinking_delta` for
extended thinking. Interleaved with all of that are periodic `ping` keepalives
and, critically, in-band `error` events such as `overloaded_error`. That last
point is the failure mode to burn into memory: a `200 OK` response can still
deliver an error event after the headers are sent. The status line lies; the
stream tells the truth.

The Go SDK models each frame as a `MessageStreamEventUnion`. You call
`stream.Next()` / `stream.Current()`, switch on `event.AsAny()`, and inside a
`ContentBlockDeltaEvent` switch again on `event.Delta.AsAny()` to reach a
`TextDelta` whose `.Text` is the increment. In parallel you call
`message.Accumulate(event)` on an `anthropic.Message` to rebuild the whole
message as you go.

### The OpenAI chunk model

OpenAI's Chat Completions stream is a sequence of `ChatCompletionChunk` values.
Each chunk carries `choices[].delta.content` (the text increment) and, on the
final content chunk, `choices[].finish_reason`. Token usage is not in every
chunk — it appears only when you set `stream_options.include_usage`, and it
arrives in a trailing chunk whose `choices` array is empty. The SDK's
`ChatCompletionAccumulator` folds chunks back into a full completion via
`AddChunk`, and its `JustFinishedContent` / `JustFinishedToolCall` /
`JustFinishedRefusal` helpers tell you when a piece has completed so you can act
on it. Assuming usage appears in every chunk, or forgetting to request it, is a
common source of "why is my token count always zero".

### Accumulation is folding

Streaming gives you fragments, but you almost always still need the whole message
anyway: to persist it, to attribute billing and usage, and to assemble tool-call
arguments that arrive as fragments across many deltas. So the mental model is a
fold — you render deltas live for the user while simultaneously folding them into
a final object. Both SDKs give you the fold for free (`Message.Accumulate`,
`ChatCompletionAccumulator.AddChunk`), which is why you rarely hand-concatenate.
The one place hand-joining bites is tool calls: OpenAI streams tool-call arguments
as fragments that must be joined by tool-call index before you can unmarshal the
JSON — concatenating them blindly across indices produces garbage.

### Lifecycle: close the stream or leak the connection

A stream owns an underlying HTTP response body. You must `defer stream.Close()`
and check `stream.Err()` after the loop. An unread or unclosed stream leaks the
connection and keeps the upstream generating — and billing — tokens you will
never read. The two-part discipline is non-negotiable: close on the way out,
check `Err()` on the way out. A loop that ends because `Next()` returned false
has told you nothing about whether it ended cleanly; only `Err()` does.

### Cancellation propagation is the money detail

The inbound request context, `r.Context()`, is cancelled the moment the browser
disconnects (and, in Go's net/http, also when your handler returns). You must
thread that same context into the provider's `NewStreaming` call. If you do not,
a user who closes the tab leaves you generating tokens against a dead connection,
holding a goroutine and paying for output nobody will see. This is the single
most cost-relevant line of code in an LLM gateway: the context that flows in from
the client must flow out to the provider, unbroken.

### Flush after every frame — and use ResponseController

Re-streaming means you cannot buffer-then-send. After writing each SSE frame you
must flush, or the operating system and Go's `bufio` layers will hold your bytes
until the buffer fills or the handler returns, defeating the entire point. The
correct tool is `http.NewResponseController(w).Flush()`, not the old
`w.(http.Flusher)` type assertion. The type assertion silently fails the moment
any middleware wraps the `ResponseWriter` (compression, logging, metrics), and a
`panic` on a failed assertion is worse than a missed flush. `ResponseController`
unwraps through middleware to find the real flusher and returns
`http.ErrNotSupported` instead of panicking when there genuinely is none.

### Infrastructure that silently defeats streaming

Streaming can be perfect in code and still broken in production because something
in front of it buffers. Gzip or brotli compression middleware coalesces frames
to compress them. Reverse proxies buffer responses by default — nginx's
`proxy_buffering on` is the classic culprit; sending the `X-Accel-Buffering: no`
response header disables it. HTTP/1.1 intermediaries may buffer too. The symptom
is always the same and always confusing: everything works in local testing and
then the whole response arrives at once at the end in staging. Set
`Cache-Control: no-cache`, `X-Accel-Buffering: no`, and keep compression off the
event-stream route.

### Mid-stream errors must be signaled in-band

Once the first byte is flushed, the status line and headers are committed. You
cannot change a `200` into a `500` afterward — the client already saw `200`. So
when the upstream fails mid-stream, or your redaction step rejects a token, the
only channel left is the stream itself. The gateway emits a terminal frame, by
convention `event: error`, so the client can distinguish a genuine completion
from a truncated one. A matching `event: done` on success gives the client an
unambiguous end marker. Silence — just closing the connection — is
indistinguishable from a network drop, and the client cannot tell a short answer
from a failed one.

### Slow consumers and backpressure

A slow client blocks your `Write` calls. In an SSE handler that means a pinned
goroutine and a held upstream connection, one per stuck client — a cheap way to
exhaust a server. You need an explicit policy. `ResponseController.SetWriteDeadline`
lets a write fail rather than block forever, so a stalled client is dropped
instead of leaking. A bounded internal buffer with a drop-or-close policy is the
other lever. The point is to decide, on purpose, whether you block, shed, or
drop — never to block unboundedly by default.

### Heartbeats keep idle connections alive

Idle proxies and load balancers close connections that carry no traffic, and an
LLM can think for many seconds before the first token. A periodic comment line —
a bare `:` frame — keeps the connection warm without polluting the data stream,
since comments are ignored by the SSE parser. The provider's own `ping` events
serve the same purpose upstream; your gateway sends its own downstream on a
ticker.

### Retry semantics for streams

A non-streaming call that fails before producing output is trivially retriable.
A streaming call that has already emitted tokens is not: you cannot un-send bytes,
and replaying would duplicate the prefix the client already rendered. So
resilience for streams means fail-fast-before-first-token retry — if the stream
errors before you have flushed anything, you may transparently retry; once output
is on the wire, you surface the error in-band and stop. This is the opposite of
the blind-replay retry that works for idempotent unary calls.

## Common Mistakes

### Treating HTTP 200 as success

Wrong: ending the loop when `Next()` returns false and assuming a clean short
answer, so an `overloaded_error` delivered mid-stream looks like a terse reply.

Fix: always check `stream.Err()` after the loop and handle in-band `error`
events. The status code was decided before the model produced a single token.

### Forgetting to close the stream

Wrong: reading deltas and returning without `stream.Close()`, leaking the HTTP
connection and letting the upstream keep generating billed tokens.

Fix: `defer stream.Close()` immediately after obtaining the stream, every time.

### Buffering the whole response, or never flushing

Wrong: appending deltas to a builder and writing once at the end, or writing each
frame but never flushing — the client sees nothing until completion.

Fix: write one frame per token and flush after each with
`http.NewResponseController(w).Flush()`.

### Using an http.Flusher type assertion

Wrong: `w.(http.Flusher).Flush()`, which silently fails or panics once the
`ResponseWriter` is wrapped by middleware.

Fix: `http.NewResponseController(w).Flush()`, which unwraps through middleware
and returns `http.ErrNotSupported` instead of panicking.

### Not passing r.Context() upstream

Wrong: calling `NewStreaming(context.Background(), ...)` inside a handler, so a
disconnected browser keeps costing money and holding a goroutine.

Fix: thread `r.Context()` into the provider call so client disconnect cancels
upstream generation.

### Concatenating tool-call fragments blindly

Wrong: joining every OpenAI `delta.content` and ignoring that tool-call arguments
stream as fragments that must be grouped by tool-call index before unmarshalling.

Fix: accumulate tool-call arguments per index (or let the accumulator do it) and
only unmarshal once the call is complete.

### Assuming usage appears in every chunk

Wrong: reading token usage off an arbitrary OpenAI chunk and getting zero.

Fix: set `stream_options.include_usage` and read usage from the trailing chunk
whose `choices` array is empty.

### Parsing SSE with a default Scanner

Wrong: a bare `bufio.Scanner` that hits `bufio.ErrTooLong` on a large `data`
line, or that mishandles multi-line `data` and CRLF.

Fix: raise the scanner buffer with `Scanner.Buffer`, join consecutive `data:`
lines with newlines, and handle CRLF/LF/CR line endings.

### Unmarshalling the [DONE] sentinel

Wrong: feeding `[DONE]` to `json.Unmarshal` and failing.

Fix: recognize `[DONE]` as a stream terminator and stop iterating.

### Leaving proxy buffering or compression enabled

Wrong: fronting the endpoint with gzip middleware or an nginx with
`proxy_buffering on`, which coalesces frames so streaming breaks only in
production.

Fix: disable compression on the event-stream route and send
`X-Accel-Buffering: no` and `Cache-Control: no-cache`.

### Blocking forever on a slow client

Wrong: writing frames with no deadline, so one stuck client pins a goroutine and
an upstream connection indefinitely.

Fix: set `ResponseController.SetWriteDeadline` (or bound an internal buffer) and
adopt an explicit drop-or-shed policy.

### Trying to write an error status after the first byte

Wrong: attempting `w.WriteHeader(500)` after streaming has begun, when the `200`
is already committed.

Fix: signal failure in-band with a terminal `event: error` frame; the status
line is no longer yours to change.

Next: [01-sse-wire-decoder.md](01-sse-wire-decoder.md)
