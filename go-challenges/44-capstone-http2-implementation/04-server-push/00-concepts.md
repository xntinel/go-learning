# 4. Server Push — Concepts

HTTP/2 server push lets a server send a response the client never asked for, anticipating a request the client is about to make. When a browser fetches `/index.html`, a pushing server can deliver `/style.css` and `/app.js` on the same connection before the browser has even parsed the HTML that references them, collapsing the round trips the client would otherwise spend discovering and requesting those assets. This file is the conceptual foundation for the whole lesson. Read it once and you will have the model needed to reason through each exercise, which builds the push machinery as independent, self-contained Go modules: PUSH_PROMISE frame encoding, a path-based push policy, a per-connection tracker that deduplicates and bounds concurrency, SETTINGS_ENABLE_PUSH negotiation, RST_STREAM cancellation handling, and promised-stream-ID sequencing validation.

A blunt caveat first, because honesty about the subject matters more than enthusiasm for it: every major browser has removed support for HTTP/2 server push. Chrome disabled it by default in Chrome 106 (October 2022) and Firefox followed in the same month; both now send `SETTINGS_ENABLE_PUSH=0` at the start of every connection, which forbids the server from pushing anything. Push is studied here because it exercises the most subtle parts of the HTTP/2 state machine — server-initiated streams, reserved stream states, settings negotiation, and stream-ID sequencing — not because you should ship it. The closing section explains exactly why it failed and what replaced it.

## Concepts

### The PUSH_PROMISE Frame (RFC 9113 §6.6)

PUSH_PROMISE (type byte `0x05`) is the frame that announces an intended push. It does not carry the pushed response; it reserves the stream that will. The frame travels on an existing client-initiated stream — the request stream whose response the push is associated with — and its payload begins with a 4-byte field holding one reserved bit followed by a 31-bit promised stream ID, then an HPACK-compressed header block. That header block is not a response; it is the synthetic *request* the pushed response answers, and it must contain the four pseudo-header fields `:method`, `:path`, `:scheme`, and `:authority`. Because the client must be able to treat the pushed response as the answer to a request it could have made itself, the method is restricted to safe, cacheable methods — only `GET` and `HEAD` are valid (RFC 9113 §8.4), and a pushed request must not carry a body.

The `END_HEADERS` flag (`0x04`) signals that the header block is complete within this frame. When the compressed headers do not fit in one frame, CONTINUATION frames follow and only the last one sets `END_HEADERS`. The exercises encode the single-frame case, where the caller supplies an already-compressed header block and the framing layer prepends the 9-byte frame header and the 4-byte promised-stream-ID prefix. Keeping HPACK compression in the caller and framing in the encoder is what makes each independently testable.

### Server-Initiated Streams And The Push Sequence

HTTP/2 partitions the stream-ID space by initiator: clients open odd-numbered streams (1, 3, 5, …) and servers open even-numbered streams (2, 4, 6, …), with stream 0 reserved for connection control (RFC 9113 §5.1.1). A pushed response lives on a server-initiated stream, so its ID is always even. The server allocates the next even ID before sending the PUSH_PROMISE, and because several handler goroutines may push concurrently, that allocation must be atomic so two pushes never collide on an ID or hand them out in the wrong order.

On the wire a single push is three steps: the server sends PUSH_PROMISE on stream N (the client's request stream), naming a promised even stream M; then it sends HEADERS on stream M carrying the pushed response's status and headers; then DATA on stream M ending with END_STREAM. The promised stream is special: from the moment the client sees the PUSH_PROMISE it is in the "reserved (remote)" state, holding the ID until the HEADERS arrive. The client matches the frames on stream M to the promise it received and presents the result as a completed response to a request it never issued.

### SETTINGS And ENABLE_PUSH Negotiation

A SETTINGS frame (type `0x04`) carries connection-level parameters as a sequence of 6-byte entries, each a 2-byte identifier and a 4-byte value (RFC 9113 §6.5). SETTINGS is always sent on stream 0; a SETTINGS frame on any other stream is a PROTOCOL_ERROR, and a payload whose length is not a multiple of 6 is a FRAME_SIZE_ERROR. Both are connection errors: the offending endpoint must close the whole connection with a GOAWAY rather than resetting a single stream.

`SETTINGS_ENABLE_PUSH` (identifier `0x02`) is the parameter that governs push. Its initial value is 1, so push is permitted until the client says otherwise. A client disables push by sending `SETTINGS_ENABLE_PUSH=0`; from the moment the server processes that frame it MUST NOT send any further PUSH_PROMISE on that connection, and doing so is a PROTOCOL_ERROR. The value is a boolean dressed as a 32-bit integer: any value other than 0 or 1 is itself a PROTOCOL_ERROR. The setting can change at any point in the connection's life, so a correct server consults the current value before every push rather than caching a decision made at connection start. This is exactly the negotiation today's browsers use to switch push off — they send `ENABLE_PUSH=0` in their very first SETTINGS frame.

### Push Cancellation Via RST_STREAM

A client that receives a PUSH_PROMISE for something it does not want — most often a resource it already holds in cache — refuses it by sending RST_STREAM on the promised stream. RST_STREAM (type `0x03`) is a fixed-size frame: a 9-byte header plus exactly a 4-byte error code, so a payload of any other length is a FRAME_SIZE_ERROR, and RST_STREAM on stream 0 is a PROTOCOL_ERROR. The error code for an unwanted push is usually `CANCEL` (`0x08`) or `REFUSED_STREAM` (`0x07`). The economics are why cancellation matters: a refused push costs one 13-byte RST_STREAM frame, whereas a push the client did not want but cannot refuse costs the entire response body plus a stream slot for its lifetime.

On the server, an arriving RST_STREAM on a pushed stream must immediately stop any further DATA on that stream and free its slot. A pushed stream counts against the client's `SETTINGS_MAX_CONCURRENT_STREAMS` limit, so the active-push count has to drop the instant a cancellation lands; otherwise the count drifts upward and the server eventually believes it is at the concurrency ceiling when nothing is actually in flight.

### Promised-Stream-ID Sequencing

The stream-ID rules are not a formality; a violation is a connection error that tears down every stream. A PUSH_PROMISE must satisfy two structural constraints at once. The associated stream — the one the promise is delivered on — must be a valid client-initiated stream: odd and non-zero. The promised stream must be a valid server-initiated stream: even and non-zero. Beyond parity, IDs must move only forward. RFC 9113 §5.1.1 requires that a newly opened stream ID be numerically greater than every stream the initiating endpoint has already opened, so each promised ID must strictly exceed the previous one. Reusing or rewinding an ID is a PROTOCOL_ERROR. A validator that enforces parity, non-zero, and strict monotonicity in one place is the cheapest insurance against the most common server-push framing bug.

### Policy And Deduplication

What to push is a policy question, and policy is the only application-specific part of the whole mechanism. A simple, effective policy is a map from request paths to the resources their responses reference: a request for `/index.html` pushes `/style.css` and `/app.js`. Exact matches should win over prefix matches so a specific rule can override a general one. Everything else — frame encoding, stream allocation, settings, cancellation — is connection-level mechanism that does not care what the resources are.

Deduplication is the discipline that keeps policy honest. Without it, a server that pushes `/style.css` for every HTML page would push the same stylesheet again and again on one connection, forcing the client to cancel each redundant copy. A per-connection set of already-pushed resources means each resource is promised at most once per connection. Deduplication plus a concurrency bound plus the ENABLE_PUSH check together form the gate every push reservation passes through before a PUSH_PROMISE is ever framed.

### Why Push Was Deprecated

Server push failed in production for three structural reasons, and they are worth internalizing because they are really lessons about speculative work in distributed systems. The first is cache blindness: the server cannot see the client's cache, so it inevitably pushes resources the client already has, spending bandwidth and stream slots to deliver bytes that land in the trash. The second is bandwidth interference: a large pushed asset can occupy the connection ahead of a higher-priority response the client actually requested, making the page the user is waiting for measurably slower — push could and did cause regressions. The third is intermediary inconsistency: CDNs and reverse proxies handled PUSH_PROMISE unevenly, and in practice many silently dropped push frames, so the feature could not be relied on end to end.

The replacement is `103 Early Hints` (RFC 8297). Instead of pushing bytes, the server sends an interim HTTP 103 response listing resources the client should consider prefetching, and the client — which can read its own cache — decides which hints to act on. The hint is advisory and cheap; the push was speculative and expensive. Early Hints keeps the latency win of telling the client about resources early while discarding the part of push that made it harmful, namely the server deciding unilaterally to spend the connection on bytes the client may not need.

## Common Mistakes

### Sending PUSH_PROMISE After END_STREAM On The Associated Stream

A PUSH_PROMISE must be sent before the response on the associated stream closes. Once the handler writes its response body with END_STREAM, that stream is half-closed (local), and a PUSH_PROMISE delivered afterward reaches the client on a stream that can no longer carry new push headers — a PROTOCOL_ERROR that tears down the connection. Reserve the stream and send every PUSH_PROMISE before writing the END_STREAM-bearing response.

### Caching The ENABLE_PUSH Decision Instead Of Re-Checking It

Reading `SETTINGS_ENABLE_PUSH` once at connection start and storing it in a plain field is wrong because the client can disable push at any time with a later SETTINGS frame. A server that misses that update and pushes anyway commits a PROTOCOL_ERROR. Apply each SETTINGS frame as it arrives and consult the current value before every push; the check is a single atomic read and is never the bottleneck.

### Using Odd IDs Or Non-Monotonic IDs For Pushed Streams

Pushed streams are server-initiated and must use even IDs that strictly increase. Borrowing the client's odd-ID space, reusing an ID, or letting a lower ID follow a higher one is a PROTOCOL_ERROR in each case. Allocate from a dedicated even-ID counter that starts at 2 and only ever advances, and never share it with the client-side allocator.

### Pushing The Same Resource Twice On One Connection

Pushing on every request without a deduplication set means a shared asset like a stylesheet is promised repeatedly, and the client must cancel each redundant copy with RST_STREAM, wasting bandwidth and stream slots. This was one of the concrete failure modes that doomed push in practice. Keep a per-connection set of already-pushed resources and skip any resource it already contains.

### Forgetting To Free The Slot On RST_STREAM CANCEL

Incrementing the active-push count when a stream is promised but never decrementing it when the client cancels makes the count grow without bound. Once it reaches `SETTINGS_MAX_CONCURRENT_STREAMS`, every subsequent reservation is refused for the rest of the connection even though no push is actually in flight. Decrement the count both when the server finishes a pushed stream with END_STREAM and when it receives RST_STREAM on a promised stream, and make the decrement idempotent so a duplicate cancel cannot drive the count negative.

---

Next: [01-push-promise-frame.md](01-push-promise-frame.md)
