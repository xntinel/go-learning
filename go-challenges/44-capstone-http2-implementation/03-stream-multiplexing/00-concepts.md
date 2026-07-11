# 3. Stream Multiplexing — Concepts

HTTP/2's defining feature over HTTP/1.1 is stream multiplexing: many independent request-response exchanges run concurrently on a single TCP connection, so one slow response no longer stalls all the others. The price is machinery HTTP/1.1 never needed: a per-stream lifecycle state machine, two independent levels of credit-based flow control (per-stream and connection-wide), accounting on both the send and the receive side of every window, a cap on concurrent streams in each direction, and a scheduler that decides which ready stream gets the next slice of connection bandwidth. This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which builds the machinery piece by piece as independent, self-contained Go modules: the full stream multiplexer (state machine plus send-side flow control), a receive-side flow-control window with WINDOW_UPDATE replenishment, an inbound concurrent-stream limiter that refuses excess streams, and a weighted byte-budget scheduler.

## Concepts

### Why A Single TCP Connection Is Both The Problem And The Solution

HTTP/1.1 keeps several TCP connections open to parallelize requests. Each connection has its own flow-control and congestion state, so the browser ends up fighting itself: connections compete for bandwidth, and TCP slow-start is paid on each one. HTTP/2 multiplexes all streams on one connection. That connection has a single congestion window reflecting the true state of the network path, and streams inside it share bandwidth without competing. The trade-off is that a single packet loss now blocks every stream (TCP head-of-line blocking), which is why HTTP/3 replaces TCP with QUIC. Everything in this lesson exists to make many logical streams coexist correctly on one byte pipe.

### The Stream State Machine (RFC 9113 §5.1)

Every HTTP/2 stream has a 32-bit identity. Client-initiated streams use odd IDs (1, 3, 5, ...); server-initiated push streams use even IDs. IDs are strictly increasing and never reused — an endpoint that opens stream 7 can never afterward open stream 5, and once a stream closes its number is spent forever. A stream moves through five states driven by frame events: it starts `idle`, becomes `open` when HEADERS is sent or received, becomes `half-closed(local)` once this side sends END_STREAM (it can still receive) or `half-closed(remote)` once the peer sends END_STREAM (it can still send), and reaches the terminal `closed` state when the second END_STREAM or any RST_STREAM lands. The exact transitions form a small table: each `(state, event)` pair maps to a next state, and any event not in the table is a protocol error that the receiver answers with RST_STREAM carrying PROTOCOL_ERROR (or STREAM_CLOSED if the stream was already closed). Encoding the machine as an explicit table rather than scattered `if` statements is what makes "is this transition legal?" a single map lookup and makes the illegal transitions enumerable in a test.

### Two Levels Of Flow Control (RFC 9113 §5.2 and §6.9)

HTTP/2 flow control is credit-based and exists at two independent levels. Each stream begins with `SETTINGS_INITIAL_WINDOW_SIZE` bytes of send credit (default 65535); a sender decrements that window for every DATA byte and must stop when it reaches zero until the receiver grants more via a WINDOW_UPDATE targeting that stream ID. A second, connection-wide window covers all DATA frames across every stream and is addressed by stream ID 0. A sender needs credit in *both* windows before it may send a DATA frame, and the two are genuinely independent: a stream can have a large per-stream window yet be blocked by an exhausted connection window, or the reverse. Only DATA frames are flow-controlled; HEADERS, SETTINGS, WINDOW_UPDATE and the rest flow freely so that control traffic is never starved by data.

### Send Side Versus Receive Side

A flow-control window is a contract between two endpoints, so it has two halves that must be implemented separately. The *send* window is the credit this endpoint has been granted by the peer; it is decremented as this side emits DATA and replenished when a WINDOW_UPDATE arrives. The *receive* window is the credit this endpoint has granted to the peer; it is decremented as the peer's DATA arrives and replenished when this endpoint chooses to emit its own WINDOW_UPDATE. The receive side carries two responsibilities the send side does not: it must detect a peer that sends more than the granted window (a FLOW_CONTROL_ERROR — the peer broke the contract), and it must decide *when* to hand back credit. Sending a WINDOW_UPDATE for every byte consumed wastes frames; the standard tactic is to accumulate consumed bytes and emit a single update once they cross a threshold (commonly half the window), which is the right balance between frame overhead and keeping the peer un-stalled.

### SETTINGS_INITIAL_WINDOW_SIZE Retroactive Adjustment (RFC 9113 §6.9.2)

When a SETTINGS frame changes `SETTINGS_INITIAL_WINDOW_SIZE`, every existing stream's send window must be adjusted immediately by the signed delta (new − old), not just streams opened afterward. A window pushed beyond 2^31−1 is a FLOW_CONTROL_ERROR; a window driven negative simply means the sender must stop and wait for WINDOW_UPDATE frames to bring it positive again — a negative window is legal, an overflowed one is not. The connection-level window is a separate quantity and is *not* touched by this setting. Forgetting the retroactive adjustment is one of the most common HTTP/2 implementation bugs, because it only manifests on connections where the setting changes after streams already exist.

### Concurrent Stream Limits In Both Directions (RFC 9113 §5.1.2)

`SETTINGS_MAX_CONCURRENT_STREAMS` caps how many streams may be simultaneously active, and it applies independently to each endpoint. The limit this endpoint *advertises* governs the streams the peer may open toward it; the limit the peer advertises governs the streams this endpoint may open. Both are real and they are not the same number. On the outbound side, an endpoint that would exceed the peer's limit simply does not open the stream and waits. On the inbound side, an endpoint that receives a HEADERS opening one stream too many may refuse it with RST_STREAM carrying REFUSED_STREAM — a deliberately distinct code from PROTOCOL_ERROR, because REFUSED_STREAM tells the peer the stream was definitively not processed and is therefore safe to retry on a fresh connection. Only streams in `open` or either `half-closed` state count toward the limit; `idle` and `closed` streams do not.

### Scheduling Ready Streams: From Priority To A Weighted Budget

When several streams are simultaneously ready to send and the connection window allows only so many bytes this round, something must decide who sends. RFC 7540 specified an elaborate priority tree (streams depend on parents, siblings split bandwidth by weight, dependencies can be exclusive). RFC 9113 deprecated that scheme because almost no implementation built it correctly and the complexity bought little, but the underlying need — distribute a fixed byte budget across ready streams in proportion to some weight, and skip streams that are blocked or closed — remains. A weighted scheduler captures the durable idea without the deprecated tree: given a connection budget and a set of ready streams with weights, allocate the budget proportionally, hand any rounding remainder to the streams with the largest fractional shortfall (the largest-remainder method, so the allocation always sums to exactly the budget), and never allocate to a stream that is not ready. This is the part of the design where fairness is decided.

### Goroutine Topology On A Single Connection

A correct endpoint needs at minimum one read goroutine that demultiplexes each incoming frame by stream ID to the right stream, one write goroutine that drains a shared outbound channel and serializes every stream's output onto the single socket, and a goroutine per stream that processes its frames. Flow-control blocking lives in the per-stream goroutines: they wait for per-stream and connection credit before enqueuing DATA, using a `sync.Cond` to sleep until a WINDOW_UPDATE is processed. The one inviolable rule is that the read goroutine must never block, because WINDOW_UPDATE and RST_STREAM arrive on it; if it stalls, every sender waiting for credit deadlocks. Demultiplexing therefore pushes frames into buffered per-stream channels and returns immediately rather than doing per-stream work inline.

## Common Mistakes

### Blocking The Read Goroutine While Demultiplexing

Wrong: the frame demultiplexer does per-stream work inline, or sends to an unbuffered per-stream channel that the stream goroutine is not currently reading. WINDOW_UPDATE and RST_STREAM frames then queue behind the stall, every goroutine waiting on send credit stays blocked forever because the credit-granting frame can never be processed, and the connection deadlocks. Fix: the demultiplexer writes to a buffered per-stream channel via a non-blocking path and returns at once; the stream goroutine drains the channel at its own pace.

### Forgetting The Retroactive INITIAL_WINDOW_SIZE Delta

Wrong: when a SETTINGS frame changes `INITIAL_WINDOW_SIZE`, applying the new value only to streams opened afterward. Existing streams keep the stale window, so they either under-send and waste bandwidth or over-send and trigger a FLOW_CONTROL_ERROR. Fix: apply the signed delta (new − old) to every existing stream's send window at once, checking each for overflow past 2^31−1, and leave the connection-level window untouched.

### Confusing The Send Window With The Receive Window

Wrong: maintaining a single window for a stream and decrementing it both when sending DATA and when receiving DATA. The two directions are governed by different endpoints and replenished by different WINDOW_UPDATE frames; conflating them double-counts and corrupts the accounting. Fix: keep the send window (credit granted to us, replenished by the peer's WINDOW_UPDATE) entirely separate from the receive window (credit we granted, replenished by the WINDOW_UPDATE we choose to send).

### Not Detecting A Peer That Overruns The Receive Window

Wrong: on the receive side, blindly buffering whatever DATA arrives. A peer that ignores flow control can exhaust memory, and the protocol violation goes unreported. Fix: before accepting a DATA frame, check its length against the remaining receive window; if it exceeds the window the peer has broken the contract and the stream (or connection) must be terminated with FLOW_CONTROL_ERROR.

### Treating Stream IDs As Reusable

Wrong: removing a closed stream from the table and handing its ID to the next stream. RFC 9113 §5.1.1 forbids reuse; the peer may still hold the old stream in a half-closed state and will mis-route frames. Fix: stream IDs increase monotonically and are never reused; when the 31-bit space is exhausted the connection is retired with GOAWAY and a new one is established.

### Refusing An Excess Inbound Stream With The Wrong Error Code

Wrong: rejecting a stream that exceeds the advertised `MAX_CONCURRENT_STREAMS` with PROTOCOL_ERROR, or silently dropping it. PROTOCOL_ERROR implies the peer is malformed and may tear down the whole connection; a silent drop leaves the peer waiting forever. Fix: refuse the one excess stream with RST_STREAM carrying REFUSED_STREAM, which tells the peer the stream was not processed and is safe to retry on a new connection, and leaves the existing streams undisturbed.

---

Next: [01-stream-multiplexer.md](01-stream-multiplexer.md)
