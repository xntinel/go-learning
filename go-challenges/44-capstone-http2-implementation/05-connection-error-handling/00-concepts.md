# 5. Connection and Error Handling — Concepts

HTTP/2 makes a hard architectural choice: not every error is catastrophic. A single malformed frame on stream 7 must not kill the twenty other requests already in flight. Conversely, a broken HPACK decompression context corrupts shared state and makes all future header decoding impossible, so the entire connection must close. Getting the classification wrong in either direction causes real damage: over-aggressive shutdown kills throughput; under-aggressive shutdown allows corrupted state to propagate. This lesson builds the error-handling and lifecycle layer of an HTTP/2 endpoint — error classification, SETTINGS negotiation, graceful shutdown, keepalive, in-flight draining, and abuse defenses — as independent, self-contained Go modules. Every module is network-agnostic: it operates on decoded frame attributes, not raw bytes, so the same logic works over TLS, plain TCP, or `net.Pipe` in tests. Read this file once and you have the conceptual foundation for all of them.

## Concepts

### Stream Errors vs. Connection Errors

RFC 9113 §5.4 divides protocol violations into two categories based on whether they corrupt shared connection state. A stream error affects only the one stream it is associated with: the correct response is to send `RST_STREAM` with the appropriate error code and then continue using the connection, since all other streams are unaffected. Examples are a DATA frame that exceeds the declared `Content-Length`, or a CANCEL on a stream the client no longer needs. A connection error corrupts state shared across every stream: the correct response is to send `GOAWAY` with the last stream ID that was successfully processed and then close the connection. Examples are an HPACK decompression failure — the compression context is connection-scoped, so once it is corrupted no future HEADERS frame can be decoded correctly — and a `FRAME_SIZE_ERROR` on a SETTINGS, PING, or GOAWAY frame, which are inherently connection-level.

The classification rule for a frame arriving on stream 0 is always "connection error," because stream 0 is the connection control channel and has no per-stream state. The whole policy collapses to a small deterministic predicate: a compression error on any stream, any error on a SETTINGS/PING/GOAWAY frame, or any error on stream 0 is fatal to the connection; everything else on a non-zero stream is a stream error. Encoding this as a single pure function, rather than scattering `if` statements across a frame reader, is what keeps the disposition consistent.

### The Error-Code Taxonomy

RFC 9113 §7 defines a flat space of 32-bit error codes carried identically in both `RST_STREAM` (stream-scoped) and `GOAWAY` (connection-scoped) frames. The same code means the same thing regardless of which frame carries it; the frame is what scopes the disposition. `NO_ERROR` (0x0) means a graceful shutdown with no fault — it is the code a server uses in a clean GOAWAY. `PROTOCOL_ERROR` (0x1) is the catch-all for a malformed exchange. `INTERNAL_ERROR` (0x2), `FLOW_CONTROL_ERROR` (0x3), `SETTINGS_TIMEOUT` (0x4), `STREAM_CLOSED` (0x5), `FRAME_SIZE_ERROR` (0x6), `REFUSED_STREAM` (0x7), `CANCEL` (0x8), `COMPRESSION_ERROR` (0x9), `CONNECT_ERROR` (0xa), `ENHANCE_YOUR_CALM` (0xb), `INADEQUATE_SECURITY` (0xc), and `HTTP_1_1_REQUIRED` (0xd) complete the set. Two of these carry semantics worth memorizing: `REFUSED_STREAM` guarantees the stream was never processed, so the client may safely retry it on a new connection — it is the code a draining server returns for streams above its last-processed ID; and `ENHANCE_YOUR_CALM` is the polite "you are abusing me" signal, the code an endpoint sends before tearing down a connection that is flooding it.

### SETTINGS Negotiation and the Acknowledgment Handshake

Both peers send a SETTINGS frame immediately after the connection preface (RFC 9113 §6.5). The frame carries zero or more 6-byte parameter records (a 2-byte ID and a 4-byte value), and a peer that receives one MUST respond with a SETTINGS ACK (Flags = 0x1, empty payload) to signal that the parameters have been applied. Six parameters are defined, each with a default that applies before any SETTINGS frame is exchanged: `HEADER_TABLE_SIZE` (0x1, default 4096), `ENABLE_PUSH` (0x2, default 1), `MAX_CONCURRENT_STREAMS` (0x3, no limit), `INITIAL_WINDOW_SIZE` (0x4, default 65535), `MAX_FRAME_SIZE` (0x5, default 16384), and `MAX_HEADER_LIST_SIZE` (0x6, no limit). Out-of-range values are connection errors (RFC 9113 §6.5.2): an `InitialWindowSize` above 2^31-1 is a `FLOW_CONTROL_ERROR`, and a `MaxFrameSize` outside [2^14, 2^24-1] is a `PROTOCOL_ERROR`. If the SETTINGS ACK is not received within a configurable timeout, the connection is treated as dead with `SETTINGS_TIMEOUT` (0x4). The negotiator that models this keeps the read-heavy "remote settings" in an `atomic.Pointer` so request handlers read them without contending with the rare update write, and gates the ACK behind a `sync.Once`-closed channel so a duplicate ACK frame cannot panic by closing the channel twice.

### GOAWAY and Graceful Shutdown

`GOAWAY` carries a `LastStreamID` field: the highest-numbered stream the sender will process. Streams with IDs above that value are guaranteed not to have been processed and should be retried on a new connection; streams at or below it may or may not have been processed. The subtlety is a race: between the moment a server decides to shut down and the moment the GOAWAY arrives at the client, the client may have started new streams. RFC 9113 §6.8 prescribes a two-phase shutdown. Phase 1 sends GOAWAY with `LastStreamID = 2^31-1` (the maximum), signalling intent to close while acknowledging that streams may be in transit; the sender then waits roughly one round-trip for those in-transit streams to arrive. Phase 2 sends GOAWAY again with the actual highest stream ID received so far. After the second GOAWAY the sender waits for in-flight streams to complete, up to a grace period, and then closes the transport. The state machine that tracks "have we sent or received a GOAWAY, and with what last ID" is the half of this that decides whether a new stream may be opened; the *draining* half — actually waiting for the streams at or below the last ID to finish while refusing everything above it — is a separate coordinator built on top of it.

### Draining In-Flight Streams

The advertised last-stream-id is a promise, and a graceful server must keep it: every stream with an ID at or below the last-processed value is allowed to run to completion, while every new stream above it is refused immediately with `REFUSED_STREAM` so the client retries elsewhere. A drain coordinator tracks the set of active streams, flips into a draining mode when shutdown begins, and blocks the shutdown path until the active set empties or a grace deadline expires. The grace deadline is what prevents one stuck stream from wedging the shutdown forever: if the streams have not all finished by the deadline, the server forces the connection closed anyway. The two outcomes — drained cleanly versus forced after timeout — are exactly what the shutdown path must distinguish, because a forced close means some in-flight work was abandoned and may need to be reported. Modeling the grace period with a fake clock (a `testing/synctest` bubble) makes the "waits, then forces" behavior testable without real sleeping and without flakiness.

### PING Keepalive and Liveness Detection

`PING` frames carry an 8-byte opaque payload that the receiver echoes back in a PING ACK (Flags = 0x1). Because the payload is opaque, the sender uses it to match ACKs to outstanding pings and measure round-trip time. A keepalive loop periodically sends PINGs and tracks how many are outstanding; if an ACK does not arrive within a deadline the connection is presumed dead. RFC 9113 §10.5 warns that sending too many PINGs can itself be treated as abuse, so the tracker enforces a concurrent-ping limit on its own outgoing pings. This is the producer side of liveness — what *we* send and wait for — and is distinct from defending against a peer that floods *us*.

### Flood and Abuse Defenses

A correct HTTP/2 endpoint is cheap to talk to, which is also what makes it cheap to attack. Two abuse vectors are worth defending explicitly. The first is the HTTP/2 Rapid Reset attack (CVE-2023-44487): a client opens a stream and immediately sends `RST_STREAM`, over and over. Because each reset frees the concurrency slot instantly, the attacker never trips `MAX_CONCURRENT_STREAMS`, yet the server still does the per-stream setup work for every request — an amplification that took down large services in 2023. The defense is rate-based: count stream cancellations that happen within a short window of the stream opening (the "rapid" in rapid reset — a stream that ran for a while and was then cancelled is legitimate), and if that rate exceeds a budget within a sliding window, tear the connection down with `ENHANCE_YOUR_CALM`. The second vector is a flood of cheap control frames — PINGs, SETTINGS, empty frames — that make the connection do work without ever making a request progress. The defense there is progress-based rather than rate-based: count control frames received since the last time a stream actually advanced, and trip `ENHANCE_YOUR_CALM` when that count exceeds a budget with no intervening useful work. Both defenses converge on the same code, `ENHANCE_YOUR_CALM`, but they measure different things — a rate over time versus a count between progress events — and a robust server runs both.

## Common Mistakes

### Treating HPACK Errors as Stream Errors

Sending `RST_STREAM 7` for a decompression failure on stream 7 keeps the connection alive, but the HPACK context is now out of sync, so future HEADERS frames on every stream will fail to decode. HPACK errors are connection errors: send `GOAWAY` with `COMPRESSION_ERROR` and close the transport. A correct classifier returns a connection error for any compression code regardless of the stream ID.

### Skipping the SETTINGS Acknowledgment Timer

Sending a SETTINGS frame at startup and then forgetting about it means that a buggy or slow peer leaves the connection silently using the wrong parameters forever. Start a timer when you send SETTINGS, wait for the ACK with a deadline, and close the connection on `SETTINGS_TIMEOUT`. The RFC does not mandate a minimum, but 10-30 seconds is typical.

### Sending GOAWAY Only Once

Sending a single GOAWAY with the actual last stream ID and immediately stopping reads silently drops the streams a client started just before the GOAWAY arrived; the client sees success followed by a broken connection. Use the two-phase shutdown: phase 1 advertises `LastStreamID = 2^31-1` to stop new streams, then after a round-trip phase 2 sends the real last ID so the client can safely retry the refused streams.

### Closing the ACK Channel Twice

Calling `close(ackCh)` directly when a SETTINGS ACK arrives panics if the peer sends two ACK frames (a protocol violation, but possible over the wire). Wrap the close in `sync.Once` so subsequent ACKs are no-ops.

### Racing on Lifecycle State Without Synchronization

Reading a `sentGoaway` boolean or an active-stream set from multiple goroutines without a mutex is a data race with undefined behavior. Protect every read and write — a mutex is clearest when several fields move together, and an `atomic.Pointer` is right for a read-heavy single value like remote settings. Run every module under `go test -race`.

### Forcing the Drain Without a Grace Deadline, or Wedging on It Forever

Two opposite mistakes share one fix. Closing the transport the instant draining begins abandons in-flight streams that would have finished in milliseconds; waiting unconditionally for the active set to empty lets one stuck stream hang the shutdown forever. Bound the wait with a grace deadline and distinguish the two outcomes — drained cleanly versus forced after timeout — so the caller knows whether work was abandoned.

### Counting Every Reset as an Attack

Rate-limiting all `RST_STREAM` frames breaks legitimate clients that cancel long-running requests (a user navigating away, a cancelled `fetch`). The Rapid Reset signal is specifically a reset that lands within a short window of the stream opening; a stream that ran for a while before being cancelled must not count toward the abuse budget, or the defense becomes a denial of service against well-behaved clients.

---

Next: [01-error-classification.md](01-error-classification.md)
