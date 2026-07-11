# Frame Parsing — Concepts

HTTP/2 replaces the HTTP/1.1 text parser with a binary framing layer. Every byte on the wire belongs to exactly one frame: a fixed 9-octet header followed by a variable-length payload. Getting the parsing right is unforgiving — a single misread bit desynchronizes the connection and makes every subsequent frame unreadable. This file is the conceptual foundation for the framing layer. Read it once and you will have what you need to work through each exercise, which builds the layer as independent, self-contained Go modules: a complete frame codec for the six connection-critical frame types, a HEADERS + CONTINUATION reassembler, a SETTINGS validator with the ACK handshake, and a frame-size guard that classifies oversized and malformed frames into the right error scope.

## The 9-Octet Frame Header

Every HTTP/2 frame shares the same fixed header (RFC 9113 §4.1):

```text
+-----------------------------------------------+
|                 Length (24)                   |
+---------------+---------------+---------------+
|   Type (8)    |   Flags (8)   |
+-+-------------+---------------+-------------------------------+
|R|                 Stream Identifier (31)                     |
+=+=============================================================+
|                   Frame Payload (0...)                      ...
+---------------------------------------------------------------+
```

Fields in order:

- Length (3 octets, big-endian): the payload size in octets. The receiver reads exactly this many octets before dispatching. The initial maximum is 16384 octets; a SETTINGS frame can raise it up to 16777215.
- Type (1 octet): selects the frame type and, with it, the payload layout.
- Flags (1 octet): type-specific bit fields. Unused bits must be zero on send and ignored on receive.
- R (1 bit) + Stream Identifier (31 bits): the reserved R bit must be zero on send; receivers mask it off before using the stream id.

The length field is 24 bits, not 32. Reading it requires three octets rather than one `binary.BigEndian.Uint32` call:

```go
length := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
```

The stream id occupies a full four-octet slot but only 31 bits are significant; the high bit is always masked:

```go
streamID := binary.BigEndian.Uint32(b[5:]) & 0x7FFFFFFF
```

## Frame Types and Dispatch

Ten frame types are defined in RFC 9113. The six covered by the core codec are the ones every HTTP/2 implementation must handle:

| Type | Value | Stream | Purpose |
|------|-------|--------|---------|
| DATA | 0x0 | non-0 | carries request or response body |
| RST_STREAM | 0x3 | non-0 | terminates a stream immediately |
| SETTINGS | 0x4 | 0 | exchanges configuration parameters |
| PING | 0x6 | 0 | measures round-trip time |
| GOAWAY | 0x7 | 0 | graceful connection shutdown |
| WINDOW_UPDATE | 0x8 | any | adjusts flow-control credit |

The remaining four — HEADERS (0x1), PRIORITY (0x2), PUSH_PROMISE (0x5), and CONTINUATION (0x9) — carry or relate to compressed header blocks; HEADERS and CONTINUATION are the subject of their own module.

The parser reads the 9-octet header and dispatches on the type field. Unknown type values must not cause a connection error: RFC 9113 §4.1 requires that implementations ignore and discard frames of unknown types, which is what preserves forward compatibility with protocol extensions. A dedicated `UnknownFrame` type captures those cases instead of rejecting them.

## Stream Identifiers and Multiplexing

The stream id distinguishes connection-level from stream-level frames and identifies which request/response stream owns the data. Certain frame types carry hard constraints:

- SETTINGS, PING, and GOAWAY must have stream id 0. Receiving them with any other value is a connection error (PROTOCOL_ERROR).
- DATA, HEADERS, and RST_STREAM must have a non-zero stream id. Stream 0 represents the connection itself and cannot carry body data or be reset.
- WINDOW_UPDATE is valid on both stream 0 (connection window) and non-zero streams.

Violating these constraints is a PROTOCOL_ERROR. The parser enforces them with sentinel errors so callers can issue a GOAWAY and close the connection rather than silently mishandling traffic.

## Validation and Error Propagation

RFC 9113 §5.4 distinguishes two error classes. A connection error, signalled by GOAWAY, closes the entire connection. A stream error, signalled by RST_STREAM, terminates one stream and leaves the others open. The framer surfaces parse errors as Go error values wrapped with `%w`; callers use `errors.Is` to decide how to respond on the wire.

Three common sources of parse errors:

1. A frame length exceeding `SETTINGS_MAX_FRAME_SIZE`. The frame is too large; reject with FRAME_SIZE_ERROR.
2. A SETTINGS payload whose length is not a multiple of 6. RFC 9113 §6.5 mandates 6-octet parameter entries; reject with FRAME_SIZE_ERROR.
3. Pad length greater than or equal to the remaining payload in a PADDED DATA or HEADERS frame. The frame is corrupt; reject with PROTOCOL_ERROR.

`io.ReadFull` is mandatory instead of `io.Read`. TCP is a byte stream; a single `Read` call may return fewer octets than requested. `io.ReadFull` retries until the full count arrives or returns an error, which keeps the framer's position in the stream consistent.

## Header Blocks Span HEADERS and CONTINUATION

A compressed header block (an HPACK-encoded set of header fields) is logically one unit but may not fit one frame. RFC 9113 §4.3 lets a sender split it: a HEADERS frame carrying the first fragment, followed by zero or more CONTINUATION frames carrying the rest, with END_HEADERS set on the last fragment. The receiver must concatenate every fragment in order before handing the result to the HPACK decoder.

The sequencing rule is strict and is a connection-level PROTOCOL_ERROR when broken: once a HEADERS (or PUSH_PROMISE) frame arrives without END_HEADERS, the very next frame on the connection must be a CONTINUATION on the same stream. Any other frame type, or a CONTINUATION on a different stream, desynchronizes header decompression for the whole connection, which is why it cannot be a mere stream error. The HEADERS payload itself may also carry an optional Pad Length octet (PADDED flag) and an optional 5-octet priority block (PRIORITY flag), both of which must be stripped before the bare header block fragment is exposed.

## SETTINGS Parameter Values and the ACK Handshake

SETTINGS does more than carry six identifier/value pairs. Each parameter has a defined value range (RFC 9113 §6.5.2), and an out-of-range value is a connection error with a *specific* code:

- SETTINGS_ENABLE_PUSH must be 0 or 1; otherwise PROTOCOL_ERROR.
- SETTINGS_INITIAL_WINDOW_SIZE must not exceed 2^31-1; otherwise FLOW_CONTROL_ERROR.
- SETTINGS_MAX_FRAME_SIZE must lie in [16384, 16777215]; otherwise PROTOCOL_ERROR.

Unknown setting identifiers must be ignored, not rejected. SETTINGS is also acknowledged: a non-ACK SETTINGS frame requires the peer to reply with an empty SETTINGS frame that has the ACK flag set. Tracking how many of your own SETTINGS frames remain unacknowledged is how an endpoint knows when its advertised parameters have taken effect, and an ACK that carries any payload is itself a FRAME_SIZE_ERROR.

## Frame-Size Errors Have a Scope

A frame-size violation is not always fatal. RFC 9113 §4.2 says a size error in a frame that could alter the state of the *entire connection* must be a connection error: that means any frame carrying a field block (HEADERS, PUSH_PROMISE, CONTINUATION), a SETTINGS frame, or *any* frame on stream 0. Every other size error — an oversized DATA frame, a wrong-length RST_STREAM on a live stream — is a stream error: the endpoint sends RST_STREAM, discards the offending frame's payload to stay byte-aligned, and keeps serving other streams. Choosing the wrong scope either tears down a connection that only needed one stream reset, or leaves a corrupt connection running. A correct framer therefore reports both the FRAME_SIZE_ERROR and whether it is connection-scoped.

## Common Mistakes

### Using `Read` Instead of `io.ReadFull`

Wrong: `n, err := r.Read(buf)` — on a real TCP connection `n` may be less than `len(buf)` because the kernel delivers data in segments. The framer's byte offset is now off by `len(buf) - n` octets and every subsequent parse is garbage. Fix: `_, err := io.ReadFull(r, buf)` — it retries internally until the full count is received or the connection is closed.

### Forgetting to Mask the Reserved Bit in Stream ID

Wrong: `streamID := binary.BigEndian.Uint32(b[5:])` — without masking, a peer that sets the high bit makes stream 0 appear as `0x80000000`, causing every connection-level frame to fail stream-id validation. Fix: always mask with `& 0x7FFFFFFF`.

### Reading the Length Field as a 32-bit Integer

Wrong: `length := binary.BigEndian.Uint32(b[0:4])` — this reads the first octet of the type field into the low octet of length and shifts the type and flags fields off by one, making the whole header unreadable. Fix: the length field is 24 bits spanning `b[0:3]`: `uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])`.

### Treating Unknown Frame Types as Errors

Wrong: returning an error when the type is not one of the ten defined values. This breaks forward compatibility; a peer using an HTTP/2 extension sees a spurious connection error. Fix: return an `UnknownFrame` (or silently discard) and continue. Only connection-level semantic violations warrant an error.

### Treating Every Frame-Size Error as a Connection Error

Wrong: sending GOAWAY for an oversized DATA frame. That tears down a connection over a single misbehaving stream. Fix: classify by RFC 9113 §4.2 — only field-block frames, SETTINGS, and stream-0 frames escalate to a connection error; other size errors are stream errors, recovered by discarding the payload and sending RST_STREAM.

### Handing a Header Block to HPACK Before END_HEADERS

Wrong: decoding the fragment from a single HEADERS frame that did not set END_HEADERS. HPACK state is stateful and order-sensitive; decoding a partial block corrupts the dynamic table for the rest of the connection. Fix: buffer every fragment and only decode once the END_HEADERS-terminated CONTINUATION sequence is complete, rejecting any interleaving frame as a connection error.

Next: [01-frame-codec.md](01-frame-codec.md)
