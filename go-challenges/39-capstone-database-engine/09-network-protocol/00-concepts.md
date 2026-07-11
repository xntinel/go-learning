# 9. Network Protocol — Concepts

Exposing a database over the network means speaking a wire protocol precisely enough that unmodified clients connect without special configuration. This lesson follows a simplified version of the PostgreSQL frontend/backend protocol, which is the protocol `psql`, `pgcli`, and the `pgx` driver speak. The hard parts are all in the bytes: the startup message breaks the normal framing rule, every integer is big-endian, two different string encodings coexist on the same wire, the extended query protocol is a multi-message state machine, and a single `Read` is never guaranteed to return a whole message. Read this file once and you will have the conceptual frame for every exercise, each of which builds one slice of the wire layer as an independent, self-contained Go module.

## Concepts

### Length-Prefixed Framing and the "Length Includes Itself" Rule

A TCP connection is a byte stream with no message boundaries. One `Read` may return half a message, two messages, or one and a half; the kernel makes no promise about where its returned bytes fall relative to the sender's logical writes. Length-prefixing is how the protocol re-imposes record boundaries on top of that stream. The reader's contract is fixed and self-describing: read the one type byte, read exactly four length bytes, decode the length, then read exactly that-many-minus-four more bytes. No sentinel scanning, no ambiguity, no guessing where the next message starts.

Every typed backend (server-to-client) message has this layout:

```text
| type byte (1) | length int32 big-endian (4) | payload |
```

The Postgres-specific rule is the part people get wrong: the 32-bit length field counts itself but not the type byte. A message with an empty payload therefore has length `4` (just the four length bytes), and a message with an N-byte payload has length `N + 4`. The type byte sits outside the count. Off-by-four here is the single most common wire bug: set the length to `len(payload)` instead of `len(payload) + 4` and the client reads four fewer bytes than the message contains, misaligns the stream, and garbles every message after it. The connection becomes unrecoverable because there is no resynchronization mechanism in the protocol; once a reader is off by one byte it stays off forever.

The startup message is the only common frame that breaks this layout. It has no type byte: the four-byte length is the very first thing on the wire, followed immediately by an `int32` protocol version and then the parameters. This is exactly why a fresh connection's first read is a dedicated `ReadStartup`, not the general `ReadMessage` — calling `ReadMessage` first would consume the high byte of the length as if it were a type byte and desynchronize immediately. The same typeless shape is shared by `SSLRequest` and `CancelRequest`, which a complete server distinguishes from a real startup by the magic version value (`80877103` and `80877102` respectively) before treating the bytes as a normal handshake.

### Big-Endian on the Wire

Every multi-byte integer in the protocol — the length field, the `int16` column counts, the `int32` parameter lengths, the type OIDs — is big-endian (network byte order), most-significant byte first. This is independent of the host CPU's byte order, so the encoder and decoder must both go through a canonical conversion rather than reinterpreting a Go integer's in-memory representation. In Go that canonical path is `encoding/binary`'s `binary.BigEndian`: `AppendUint16`/`AppendUint32` to write, `Uint16`/`Uint32` to read. Never lay a Go struct directly onto the wire with `unsafe`; that would leak the host's endianness and padding into the protocol and break the moment a client runs on a different architecture.

### Two String Encodings: C-Strings vs Counted Strings

The protocol uses two different string representations and they are not interchangeable. Choosing the wrong one for a field corrupts the frame.

- Null-terminated C-strings are used for identifiers and metadata: the SQL text in a `Query`, statement and portal names, the column names in `RowDescription`, the key/value pairs in the startup message, and every field of an `ErrorResponse`. There is no length prefix; the reader scans forward to the `\0` byte. A correct reader reports whether a terminator was actually found rather than running off the end of the buffer.
- Length-prefixed (counted) byte strings are used for data values: each column value in a `DataRow` and each parameter value in a `Bind`. These are preceded by an `int32` byte count and are not null-terminated, so they may contain embedded zero bytes (arbitrary binary data). The special length `-1` (`0xFFFFFFFF`) means SQL NULL and is followed by no bytes at all; a length of `0` means a present-but-empty value. Conflating those two — encoding NULL as a zero-length string — is a correctness bug that clients detect, because a driver distinguishes the empty string from NULL and will report a nullable column as a non-null empty string.

### Simple vs Extended Query Protocol

The protocol offers two ways to run a statement and a real server supports both. The simple protocol is one round trip: a single `Query` message (type `'Q'`) carries the SQL text and the server streams back the whole result. It always uses text format, cannot carry out-of-line parameters (any value must be inlined into the SQL string), and treats the message as one implicit transaction. The server's reply per statement is a fixed sequence:

```text
RowDescription -> DataRow* -> CommandComplete -> ReadyForQuery
```

The `CommandComplete` tag encodes the command and a count: `"SELECT 3"`, `"INSERT 0 1"`, `"UPDATE 5"`, `"DELETE 2"`. INSERT always puts `0` in the second field, a legacy slot once used for the OID of the inserted row.

The extended protocol splits the same work into separate `Parse`, `Bind`, `Describe`, `Execute`, and `Sync` messages. It names prepared statements, binds typed parameters out of band (which defeats SQL injection and enables statement reuse), can request binary result formats, and lets a client pipeline many statements before a single `Sync` boundary. The reply sequence maps message to acknowledgment:

```text
Parse    'P' -> ParseComplete '1'
Bind     'B' -> BindComplete  '2'
Describe 'D' -> RowDescription 'T' or NoData 'n'
Execute  'E' -> rows + CommandComplete 'C'
Sync     'S' -> ReadyForQuery 'Z'
```

Drivers like `pgx` use the extended protocol for parameterized queries; `psql` uses the simple protocol for interactive input. The two share the same result messages (`RowDescription`, `DataRow`, `CommandComplete`) and the same `ReadyForQuery` terminator, so the result-encoding code is written once and reused by both paths.

### The Connection State Machine and the ReadyForQuery Status Byte

A connection is a small state machine. After a valid startup the server sends the handshake sequence — `AuthenticationOk` (type `'R'`, payload `int32(0)`), one `ParameterStatus` per server setting, `BackendKeyData` (process ID and cancel key), and finally `ReadyForQuery` (type `'Z'`) — and then sits in the ready state. The client, which blocks on that first `ReadyForQuery` before sending anything, then drives the connection into the query-processing state with a `Query` (simple) or a `Parse`/`Bind`/`Execute` run (extended). The server streams results and returns to ready by sending `ReadyForQuery` again.

`ReadyForQuery` is therefore the synchronization point of the whole protocol. A well-behaved client sends nothing on a fresh connection until it has seen the first one, and a server must send exactly one per simple `Query` and exactly one per `Sync`. Send too few and the client hangs waiting; send too many and its accounting desynchronizes. Because the client blocks on it, the server must also flush all buffered output before waiting for the next frontend message — a buffered writer that never flushes deadlocks both sides.

The single payload byte of `ReadyForQuery` is the transaction status, and it is not decoration: clients display it (psql's transaction-aware prompt) and drivers branch on it.

- `'I'` (idle): no transaction block is open; each statement autocommits.
- `'T'` (in transaction): a `BEGIN` is open; statements accumulate until `COMMIT`.
- `'E'` (failed transaction): a statement inside the block errored. The server now rejects every statement except `COMMIT`/`ROLLBACK` (both of which roll back) until the block is closed. A new engine must actually enter this state on an error inside a transaction, or clients will send statements that real Postgres would have refused.

Each connection keeps this status, plus its prepared statements and portals, in its own per-connection state. The status byte on the wire is just a projection of that state. State must never be shared between connections: even a mutex only prevents data races, not the logical corruption of one session seeing another's portals. A fresh state object is allocated per connection.

A `Sync` in the extended protocol has a second role beyond ending a cycle: it forces `ReadyForQuery` to be emitted even if a prior step in the batch failed. That is the recovery mechanism — clients pipeline multiple Parse/Bind/Execute sequences and use the `ReadyForQuery` after `Sync` to know exactly when the server is ready for the next batch.

### Partial Reads and io.ReadFull

A single `net.Conn.Read` is only guaranteed to return at least one byte; it may return fewer than requested even when more bytes are on the way. Code that writes `conn.Read(buf)` and then assumes `buf` is full is wrong on any real network and will intermittently truncate messages under load or across packet (MTU) boundaries. The correct primitive for a fixed-size read is `io.ReadFull(r, buf)`, which loops until exactly `len(buf)` bytes have arrived, or returns `io.ErrUnexpectedEOF` for a partial read and `io.EOF` when nothing was read at all. Every fixed-size read in this lesson — the four length bytes, the exact payload — goes through `io.ReadFull`.

Wrapping the connection in a `bufio.Reader` coalesces the many small reads (a one-byte type, a four-byte length) so they do not each become a syscall, but buffering does not change the contract: the buffer can be partially filled too, so you still must read the exact count. Decoders that work on an in-memory payload slice face the same hazard one level up — the slice may be shorter than the format requires. Each one checks the remaining length before every field and returns a wrapped sentinel error on a short buffer, so a caller can classify the failure with `errors.Is` and respond with a protocol error instead of panicking on a slice bounds violation.

## Common Mistakes

### Getting the Message Length Field Wrong

Wrong: setting the length field to `len(payload)` instead of `len(payload) + 4`.

What happens: the client reads four fewer bytes than the message contains, misaligns the stream, and every subsequent message is garbled. There is no resynchronization in the protocol, so the connection is permanently unrecoverable.

Fix: the length is the number of bytes in the message including the four length bytes themselves but not the preceding type byte. Always add 4 to the payload length when writing, and read exactly `length - 4` payload bytes when reading.

### Calling ReadMessage for the First Message on a Connection

Wrong: using the general typed-message reader for the very first frame on a new connection.

What happens: the typed reader consumes one type byte first, but the startup message has no type byte, so it eats the high byte of the four-byte length. The length is then decoded from the wrong bytes, producing a nonsense value, and the stream is corrupt from the first read.

Fix: a fresh connection's first read is the dedicated startup reader, which reads the four-byte length directly with no leading type byte.

### Encoding NULL as an Empty String

Wrong: sending a zero-length counted string for a SQL NULL column or parameter.

What happens: the client driver distinguishes `""` (the empty string) from NULL. Encoding NULL as a zero-length value makes a nullable column report as a non-null empty string, silently changing query results.

Fix: encode NULL with the special length `-1` (`0xFFFFFFFF`) and no following bytes. A length of `0` is reserved for the genuinely empty value.

### Forgetting to Flush After a Response Sequence

Wrong: writing `RowDescription`, `DataRow`, and `CommandComplete` into a buffered writer and then waiting for the next query without flushing.

What happens: both sides deadlock. The client blocks waiting for data still sitting in the server's `bufio.Writer`; the server blocks waiting for the next message the client will not send until it sees the result.

Fix: flush after every complete response sequence — startup, query result, error response. Helpers that own a sequence (the startup response, the error response) flush internally; a handler that streams a query result must flush after its final `ReadyForQuery`.

### Sharing Connection State Between Sessions

Wrong: allocating one connection-state object at server startup and passing it to every handler.

What happens: concurrent connections corrupt each other's prepared-statement and portal maps. A `sync.Map` would remove the data race but not the logical bug — connection A's portal would still be visible to connection B.

Fix: allocate a fresh per-connection state object inside the per-connection handler. Protocol state is never shared; a mutex is only for cross-connection bookkeeping such as the active-connection count.

---

Next: [01-message-framing-and-startup.md](01-message-framing-and-startup.md)
