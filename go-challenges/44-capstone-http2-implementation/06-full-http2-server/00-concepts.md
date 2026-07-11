# 6. Full HTTP/2 Server — Concepts

The five preceding lessons each built one layer of HTTP/2 in isolation: the frame
codec (lesson 01), the HPACK header compressor (lesson 02), stream multiplexing
(lesson 03), server push (lesson 04), and connection-level error handling (lesson
05). A complete server is not a sixth component — it is the composition of those
five into a single connection that reads frames, decompresses headers, routes
requests to an `http.Handler`, and writes responses back, all while several
goroutines touch the same connection at once. The hard part is no longer any one
algorithm; it is the concurrency boundaries, because a server runs a read loop,
many handler goroutines, and a write serializer that all race to touch shared
state, and a single misordered lock corrupts every stream on the connection.

This file is the conceptual foundation for two self-contained modules. The first
assembles the wired server behind a TLS listener that negotiates "h2" with ALPN
and drives it end to end with Go's own `net/http` client. The second serves the
same requests over cleartext TCP using HTTP/2 prior knowledge (h2c), the
TLS-less entry path defined by RFC 9113 §3.3.

## Concepts

### The integration problem: five layers composing

In isolation, a bug in layer N is local. In composition, a bug in layer N
surfaces as a symptom in layer M two round-trips later — a flow-control timeout
caused by a missing WINDOW_UPDATE that was never sent because a RST_STREAM
handler deleted the wrong stream ID. The integration layer's whole job is to
define ownership boundaries so that every component has exactly one goroutine
writing to it at a time. Concretely: the framer's `ReadFrame` is called only by
the connection's read loop; its `WriteFrame` is called only through one write
mutex; the HPACK decoder is touched only by the read loop; the HPACK encoder is
touched only under that same write mutex. Once those four rules hold, the five
layers compose without data races.

### Decoupling via interfaces

The server does not hard-code the framer or the HPACK codec. It depends on two
small interfaces — a `FrameLayer` (`ReadFrame`/`WriteFrame` over `RawFrame`
values) and a `HeadersCodec` (`Decode`/`Encode` over name/value pairs) — and a
`Config` carries factory functions that build them per connection. This is the
seam that lets the same server run on the framer and HPACK codec from lessons 01
and 02, on the standard library's `golang.org/x/net/http2/hpack`, or on a stub
in a unit test. A `RawFrame` is the wire frame laid bare: a 24-bit length, an
8-bit type, 8-bit flags, a 31-bit stream ID, and the raw payload. Every layer
above speaks in these structs.

### TLS, ALPN, and the cleartext alternative

HTTP/2 over TLS selects the protocol with ALPN (Application-Layer Protocol
Negotiation, RFC 7301) during the handshake: the client offers a list in its
ClientHello, the server picks one in its ServerHello, and only then is the first
byte of HTTP exchanged. In Go you set `tls.Config.NextProtos = []string{"h2",
"http/1.1"}`, call `Conn.Handshake()`, and read the result from
`ConnectionState().NegotiatedProtocol`. The field is empty until the handshake
completes, so the order matters; if the client never offered "h2", the server
falls back to HTTP/1.1 or closes.

Cleartext HTTP/2 (h2c) skips TLS entirely. RFC 9113 §3.3 defines the
prior-knowledge form: the client simply opens a TCP connection and sends the
HTTP/2 connection preface immediately, with no negotiation. (RFC 9113 removed the
RFC 7540 `Upgrade: h2c` handshake; prior knowledge is the cleartext path that
remains in the current spec.) Because the server already treats any non-TLS
connection as h2c, the read loop is identical — only the listener and the absence
of a handshake differ. Cleartext offers no confidentiality and must be confined
to trusted networks or used behind a TLS-terminating proxy.

### The client connection preface and the SETTINGS handshake

After the TLS handshake (or immediately, for h2c), the client sends a fixed
24-byte octet sequence (RFC 9113 §3.4):

```
PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n
```

The server reads and validates these exact bytes before sending anything; a
mismatch is a connection error. Only then does the server send its own SETTINGS
frame — advertising MAX_CONCURRENT_STREAMS, INITIAL_WINDOW_SIZE, and
MAX_FRAME_SIZE — and begin its read loop. The client sends its SETTINGS too; each
side acknowledges the other's with a SETTINGS frame carrying the ACK flag and an
empty payload. A SETTINGS frame with ACK set must never be acknowledged again, or
the two peers ping-pong ACKs forever.

### The three-goroutine model and the write mutex

A connection has exactly three kinds of goroutine. One read loop owns
`ReadFrame` and the HPACK decoder; it never writes a response body. One handler
goroutine per active stream runs the `http.Handler` and writes through the
`ResponseWriter`. One logical write path — guarded by a single mutex, `wmu` —
serializes every outbound frame. The read loop also writes (PING ACKs, SETTINGS
ACKs, WINDOW_UPDATEs), so it shares `wmu` with the handlers. The rule that makes
this safe is that the HPACK *encode* and the HEADERS *write* happen under the
same `wmu` acquisition: if two handlers interleaved `Encode` calls, the encoder's
dynamic table would advance for one stream while the frame for the other had not
yet been written, and the client's decoder would desynchronize on the next
header block. Encode-then-write is one atomic critical section.

### Preventing the flow-control deadlock

The read loop runs in one goroutine; a handler that writes a large body may need
send-window credit that only arrives as WINDOW_UPDATE frames the read loop must
deliver. If the handler ever blocks while *holding* `wmu`, the read loop cannot
acquire `wmu` to send anything, but the read loop is also the only path that can
grant the credit the handler is waiting for: deadlock. The discipline is that a
handler acquires and releases `wmu` independently for each DATA frame and never
holds it while waiting for window credit. The read loop and the write path share
only `wmu`, and each holds it for the duration of a single frame, never across a
blocking wait.

### Implementing http.ResponseWriter

`http.ResponseWriter` has three methods, and HTTP/2 imposes ordering rules on
each. `Header()` returns a buffered map. `WriteHeader(code)` encodes every
buffered header with HPACK and sends a HEADERS frame whose first field is the
`:status` pseudo-header (RFC 9113 §8.3.2); it fires at most once. `Write(p)`
calls `WriteHeader(200)` implicitly on first use, then emits `p` as DATA frames
bounded by MAX_FRAME_SIZE. Two ordering rules bite here: pseudo-headers precede
regular headers, and all header field names must be lowercase (RFC 9113 §8.2.1) —
a conforming client rejects `Content-Type` and accepts only `content-type`. The
writer keeps a small mutex over its `wroteHeader`/`sentEndStream` flags, and that
mutex is released before any call that takes `wmu`, so the two locks have a
strict order and never deadlock.

### Graceful shutdown with GOAWAY

GOAWAY (RFC 9113 §6.8) is how a server announces it will accept no new streams.
Its payload carries the highest stream ID the server has processed; a client may
safely retry any stream with a larger ID on a fresh connection. Graceful shutdown
sends GOAWAY to every active connection, waits for in-flight streams to finish,
and closes once they drain — or, if a deadline expires first, force-closes the
remaining connections and reports the deadline error. The two outcomes are both
correct: a clean drain returns nil; a client that holds an idle connection open
past the deadline is force-closed.

### CONTINUATION: no frame may interleave a header block

A header block too large for one frame spills into CONTINUATION frames, and RFC
9113 §6.10 forbids *any* other frame — even on another stream — between a HEADERS
frame and its terminating CONTINUATION. The read loop tracks this with a single
boolean: while a header block is open (END_HEADERS not yet seen), only a
CONTINUATION for the same stream is legal, and anything else is a connection
error. The block is dispatched to a handler only once it is complete, so the
server never starts writing a stream's response in the middle of receiving its
request headers.

## Common Mistakes

### Holding two locks in the wrong order

Wrong: `WriteHeader` holds the response writer's `mu`, then calls `sendHeaders`,
which takes `conn.wmu`; meanwhile another path takes `wmu` first and then reaches
for the writer's `mu`. Two goroutines deadlock on the lock-order cycle. Fix:
establish one global order — the writer's `mu` is never held while calling
anything that acquires `wmu`. Release `mu` before `sendHeaders` or `writeFrame`.

### Interleaving the HPACK encode and the frame write

Wrong: holding `wmu` only around `WriteFrame` and calling `Encode` outside it.
Two streams then interleave their encodes, the dynamic table advances for one
while the other's frame is unsent, and the peer's decoder desynchronizes on the
next request. Fix: hold `wmu` across the whole `Encode`-then-`WriteFrame`
sequence so the table mutation and the byte emission are one atomic step.

### Blocking the read loop on a write

Wrong: a handler holds `wmu` while waiting for send-window credit, so the read
loop — the only goroutine that can deliver the WINDOW_UPDATE that would unblock
the handler — stalls trying to acquire `wmu`. Fix: never hold `wmu` across a wait
for window credit; acquire and release it per frame, as the DATA write path does.

### Sending response headers without lowercasing field names

Wrong: writing `Content-Type` straight from the buffered header map into HPACK.
RFC 9113 §8.2.1 requires lowercase field names, and a conforming client rejects
the connection. Fix: lowercase every non-pseudo header name before encoding, and
emit `:status` first.

### Acknowledging a SETTINGS ACK

Wrong: replying to every SETTINGS frame with an ACK, including ones that already
carry the ACK flag. The two peers then ACK each other's ACKs without end. Fix:
treat a SETTINGS frame with the ACK flag set as terminal — apply nothing, send
nothing.

### Dispatching a header block before it is complete

Wrong: building the request and starting the handler from the first HEADERS frame
without waiting for its CONTINUATION frames, so the server writes a response while
the request's header block is still arriving and interleaves frames illegally.
Fix: accumulate fragments until END_HEADERS, and only then decode and dispatch.

---

Next: [01-wired-http2-server.md](01-wired-http2-server.md)
