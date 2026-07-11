# 7. TCP Protocol and Client Library — Concepts

A message queue that applications reach over the network needs three cooperating pieces: a binary frame format whose header makes request and response matching unambiguous even when replies arrive out of order, a server that accepts concurrent connections and dispatches frames to a handler, and a client library that hides framing and connection management behind a small call surface. This file is the conceptual foundation. Read it once and you will have the vocabulary to reason through each exercise, which builds the protocol piece by piece as independent, self-contained Go modules: the wire codec, a multiplexed client-and-server pair, a pipelined client, and a connection pool with reconnect and exponential backoff.

## Concepts

### The Binary Frame: Length Prefix and Correlation IDs

Every message on the wire is a frame: a length-prefixed byte sequence whose header carries a correlation ID. The length prefix (4 bytes, big-endian) tells the receiver exactly how many bytes form one message, so it can issue a single `io.ReadFull` and be done. Without a length prefix the receiver would have to re-scan the byte stream after every read to find a message boundary, because TCP is a stream protocol with no notion of message edges: one `Write` of 100 bytes can arrive as three `Read` calls of 40, 40, and 20 bytes, or as one of 100, and the receiver cannot tell which without framing.

The correlation ID (4 bytes in the header) is what enables connection multiplexing. The client assigns a unique ID to every request. The server copies that ID into the matching response. A dedicated reader goroutine on the client looks up the waiting caller by ID and delivers the response, so a hundred goroutines can have a hundred in-flight requests on one TCP connection with no serialization.

The frame format used here follows Kafka's convention: the 4-byte length field counts the bytes that follow it, not the total frame size. This keeps the arithmetic uniform: `length = headerLen + len(Payload)`. The wire layout:

```text
offset  size  field
0       4     length (= headerLen + len(Payload), big-endian uint32)
4       2     API key (uint16)
6       2     API version (uint16)
8       4     correlation ID (int32)
12      N     payload
```

`Decode` validates the length before it trusts any field: a length smaller than `headerLen` is a malformed frame and is rejected with a sentinel error, never used to size a slice. This is the safe failure mode for a reader handed bytes from a half-open or hostile connection.

### BinaryWriter and BinaryReader: Centralizing Endianness

Calling `encoding/binary.BigEndian.PutUint32` at every field site is tedious and error-prone. A thin `BinaryWriter` wraps a `[]byte` and exposes `WriteInt8 / WriteInt16 / WriteInt32 / WriteInt64 / WriteString / WriteBytes`, all big-endian, so the endianness decision lives in one place rather than scattered across every payload builder. The matching `BinaryReader` wraps an `io.Reader` and uses the error-accumulation pattern: every read stores the first error in a field, and subsequent reads become no-ops. The caller decodes a whole sequence of fields and checks `Err()` once at the end instead of after every field, which keeps payload decoders flat and readable.

Length-prefixed strings use a 2-byte signed length (matching Kafka). A length of -1 represents a null string; non-null strings use `int16(len(s))` followed by the UTF-8 bytes. Length-prefixed byte slices use a 4-byte signed length, because payloads can exceed the 32 KB that a signed 16-bit length could express.

### Connection Multiplexing: One Connection, Many In-Flight Requests

The multiplexed client maintains a `map[int32]chan *Frame` called `pending`. When a goroutine calls `Roundtrip`, it:

1. Acquires a semaphore slot, which limits the in-flight count to `maxInFlight` and provides back-pressure.
2. Atomically increments the correlation ID counter and stores its reply channel in `pending` under that ID.
3. Locks a write mutex and sends the encoded frame.
4. Blocks on its reply channel, or on the `done` channel if the client is closing.

A single reader goroutine (`readLoop`) reads frames sequentially from the connection — TCP delivers them in order — and dispatches each response to the correct channel by correlation ID. Reads are inherently sequential, so the read side needs no mutex. Writes need a mutex because multiple goroutines share one TCP connection and a partial write from one goroutine interleaved with another would corrupt the frame stream. The semaphore is a buffered channel of size `maxInFlight`: acquiring it before sending and releasing it (via defer) after receiving means that when `maxInFlight` requests are in flight, the next caller blocks until one completes.

### Server Design: Per-Connection Goroutines and Concurrent Dispatch

The server spawns one goroutine per accepted connection. Each connection goroutine reads frames in a loop and, in the multiplexed design, spawns a goroutine per frame to invoke the `Handler`. This two-level model means a slow handler does not block other in-flight requests on the same connection, and responses may legitimately leave in a different order than requests arrived — which is fine, because the client matches by correlation ID.

An inner `sync.WaitGroup` inside `handleConn` ensures graceful shutdown: when the read loop exits (EOF or a deadline), `defer wg.Wait()` drains all in-flight handler goroutines before the connection closes. A `sync.Mutex` serializes writes within one connection, and the `bufio.Writer.Flush` call happens inside that mutex so the flush is atomic with the preceding `Encode`. Read and write deadlines prevent goroutine leaks from stale connections: a read deadline causes `Decode` to return an error if no frame arrives, exiting the connection goroutine cleanly.

### Pipelining: Filling the Bandwidth-Delay Product

Multiplexing hides latency by running many goroutines. Pipelining hides the same latency from a single goroutine: it sends a batch of requests back-to-back without waiting for each reply, then collects the replies. The win is the round-trip time. A synchronous client that sends one request and waits for its reply before sending the next pays one full RTT per request, so on a 1 ms link it caps at roughly 1000 requests per second no matter how fast the server is. A pipelined client puts all the requests on the wire first, so the total time is approximately one RTT plus the server's processing time, independent of batch size. This is exactly the technique behind HTTP/1.1 pipelining, Redis pipelining, and the `net/rpc` asynchronous `Go` call.

The matching question is the interesting part. When responses are guaranteed to come back in the same order they were sent — true for a single connection served by a strictly in-order handler — the client does not even need correlation IDs: a FIFO queue of waiting calls suffices, because the nth response belongs to the nth request. Correlation IDs become necessary only when the server may reorder responses, as the concurrent-dispatch server above does. The pipelining exercise builds the FIFO form and keeps the correlation ID purely as an assertion that the ordering held.

### Connection Pools, Reconnect, and Exponential Backoff

Opening a TCP connection costs a round trip for the handshake, so a client that dials a fresh connection per request throws away that cost on every call. A connection pool amortizes it: a bounded set of established connections is kept idle between uses, `Get` hands one out, and `Put` returns it. The bound matters as much as the reuse — an unbounded pool under load opens connections until the server's file-descriptor limit or accept backlog is exhausted, so the pool caps concurrency the same way the in-flight semaphore does.

Reconnect is the other half. Connections break: the server restarts, a load balancer recycles an idle socket, a network blip resets the link. A pooled connection that has gone bad must be discarded rather than handed out, and a fresh one dialed. The danger is the reconnect storm: when a server restarts, every client retries at once, and a naive tight retry loop hammers the recovering server with a synchronized flood that keeps it down. Exponential backoff is the fix. After a failed dial the client waits `base`, then `base*2`, then `base*4`, and so on up to a `maxDelay` ceiling, doubling the gap between attempts so the load on the failing server falls geometrically. Adding jitter — a random fraction of the delay — desynchronizes a fleet of clients so they do not all retry on the same tick; the AWS Architecture Blog's "exponential backoff and jitter" analysis shows full jitter minimizes both contention and completion time. A backoff loop must also respect a `context.Context` deadline so a caller can bound how long it is willing to wait for a connection rather than blocking forever on an unreachable server.

## Common Mistakes

### Forgetting the Write Mutex

Wrong: two goroutines call `conn.Write` (or `Encode` into a shared `bufio.Writer`) concurrently inside `handleConn` or in the client. TCP does not guarantee that the bytes of one write appear contiguous on the wire, so the frame stream interleaves and the peer decodes garbage.

Fix: hold a `sync.Mutex` around the `Encode` plus `Flush` pair. Reads are safe without a lock because each connection is read by a single goroutine; only writes are concurrent.

### Flushing Outside the Write Mutex

Wrong: `Encode` runs inside the write mutex but `bw.Flush()` runs after releasing it. A second goroutine acquires the mutex and starts encoding before the first goroutine's bytes are flushed, and the two frames interleave in the buffered writer.

Fix: call `bw.Flush()` inside the same mutex that protects `Encode`, so the encode-and-flush is one atomic unit.

### Forgetting to Delete the Pending Entry on Send Error

Wrong: after `Encode` or `Flush` fails, the correlation ID's entry stays in the `pending` map. The reader goroutine never sees a matching response, so the channel and the map entry leak until the connection closes.

Fix: on any send error, lock the pending mutex and `delete(pending, id)` before returning the error.

### Not Propagating the Correlation ID from Request to Response

Wrong: a `Handler` returns a fresh `Frame` without copying `req.CorrelationID`. The client's reader looks up correlation ID 0 in `pending`, finds nothing, and the caller blocks until its deadline fires.

Fix: always copy `req.CorrelationID` into the response frame the handler returns.

### A Reconnect Loop With No Backoff and No Context

Wrong: `for { conn, err := dial(); if err == nil { break } }`. When the server is down, this spins as fast as the OS rejects connections, turning one client into a denial-of-service source against a server that is trying to recover, and it never gives up.

Fix: sleep an exponentially growing, jittered delay between attempts up to a ceiling, and select on `ctx.Done()` each iteration so the caller's deadline can cancel the wait.

---

Next: [01-binary-frame-codec.md](01-binary-frame-codec.md)
