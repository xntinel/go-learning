# Testing io.Reader and io.Writer with testing/iotest — Concepts

In a Go backend, `io.Reader` and `io.Writer` are the universal seam for every
stream that crosses a boundary: HTTP request and response bodies, gRPC frame
decoders, object-store uploads and downloads, Kafka and NATS payloads, log and
NDJSON ingestion, gzip and zstd decompression, checksum and encryption
pipelines, and raw `net.Conn` traffic. The single hardest and most
under-tested contract in all of that code is the `io.Reader` contract itself.
Most streaming bugs are not exotic; they are one read loop that assumed a single
`Read` returns one logical chunk. This file is the conceptual foundation for the
nine independent exercises that follow; read it once and you can reason through
each of them.

## Concepts

### The io.Reader contract in full

`Read(p []byte) (n int, err error)` is deceptively small and almost always
misread. Its guarantees are narrow on purpose:

- `Read` may return any `0 <= n <= len(p)`. It is allowed to use less than the
  whole buffer even when more data exists. A **short read is not an error**.
- The caller **must process the `n` returned bytes before inspecting `err`**.
  Bytes and errors can arrive together.
- `n > 0` with a non-nil `err` (including `io.EOF`) is legal and common.
- Returning `0, nil` is discouraged: a caller in a `for` loop can spin forever
  on it. An implementation at end-of-stream should return `io.EOF`, not `0, nil`.

Almost every truncation and corruption incident traces to code that violated one
of these: a decoder that read a 4-byte header with a single `Read` and got 3
bytes; a loop that checked `err` first and dropped the final chunk; a custom
reader that returned `0, nil` and hung `io.ReadAll`.

### Two legal EOF conventions, both of which you must handle

`io.EOF` can arrive two ways, and a correct reader survives both:

1. On the call *after* the last data: one `Read` returns `(n>0, nil)`, the next
   returns `(0, io.EOF)`.
2. *Together with* the last data: one `Read` returns `(n>0, io.EOF)`.

`bytes.Reader`, `strings.Reader`, and most stdlib readers use form (1); a network
socket or a `bufio`-buffered source may use either. A decoder that checks `err`
before consuming `n` silently loses the final chunk under form (2). This is
exactly what `iotest.DataErrReader` converts a reader into, so you can flush that
bug out in a unit test.

### testing/iotest is an adversarial contract fuzzer

`testing/iotest` is not a convenience wrapper; it is a deterministic reproduction
of the pathological chunking real networks, TLS records, and kernel buffers
produce. A reader tested only with one large buffer passes CI and then corrupts
data in production. The wrappers:

- `OneByteReader(r)` forces exactly one byte per `Read`.
- `HalfReader(r)` returns half the requested bytes each `Read` (rounded down).
- `DataErrReader(r)` moves EOF onto the final data read (form 1 -> form 2).
- `ErrReader(err)` returns `(0, err)` on every read: a permanent injected fault.
- `TimeoutReader(r)` returns `(0, ErrTimeout)` on the *second* read and then
  reads normally: a transient stall.
- `TruncateWriter(w, n)` mirrors the trick on the writer side.
- `TestReader(r, content)` runs a full multi-size read suite and asserts the
  reader yields exactly `content`, that `Read(nil)` returns `0, nil`, and that
  reading at EOF returns `0, io.EOF`.

`TestReader` additionally validates `io.ReaderAt` and `io.Seeker` behavior *when
the reader implements them*, so passing it certifies random-access and seek
semantics, not just sequential `Read`. Run it as the baseline contract test for
any reader you write; hand-rolled edge-case tests always miss a case it covers.

### io.ReadFull is the primitive for fixed-size reads

A single `Read` cannot be trusted to fill a buffer, so it is the wrong tool for a
fixed-size header or frame. `io.ReadFull(r, buf)` loops until `buf` is full or an
error occurs. Its error discipline is precise and worth memorizing:

- filled the buffer: returns `(len(buf), nil)`.
- zero bytes read then EOF: returns `(0, io.EOF)` — a clean end on a boundary.
- *partial* fill then EOF: returns `(k, io.ErrUnexpectedEOF)` — a truncated,
  corrupt frame.

Confusing `io.EOF` with `io.ErrUnexpectedEOF` is a classic framing bug: a partial
frame is corruption, not clean termination. `io.ReadFull` draws that line for you.

### Composition primitives and their contracts

- `io.MultiReader(r1, r2, ...)` concatenates readers, returning EOF only after
  the last is drained. It lets you prepend a header to a body without allocating
  a combined buffer.
- `io.TeeReader(r, w)` returns a reader that mirrors everything it reads into `w`
  — the way to compute a running checksum or audit tap while streaming, without
  buffering the whole payload.
- `io.LimitReader(r, n)` silently truncates at `n` bytes and returns `io.EOF`
  with **no error**. That is correct for "read at most a prefix" and dangerously
  wrong as a body-size guard: an oversized request looks like a valid short body.
- `net/http.MaxBytesReader(w, r, n)` is the security-correct body cap: it returns
  a typed `*http.MaxBytesError` once the limit is exceeded, so you can reject the
  request instead of silently accepting a truncated one.

### The writer-side mirror of the contract

A `Write(p)` may accept fewer bytes than offered; `io.ErrShortWrite` is the
signal that a write path could not place all the bytes. `io.Copy` and any correct
manual write loop must detect `nn < len(p)` rather than assuming a full write. A
network sink or a rate-capped `http.ResponseWriter` can stop accepting bytes
mid-stream. `iotest.TruncateWriter(w, n)` simulates a sink that writes only the
first `n` bytes — but note it *lies*: it reports `len(p)` written while silently
dropping the rest, which is precisely why a copy loop that trusts the returned
count loses data. To catch truncation you need either a sink that reports the
real short count (so `io.Copy` yields `io.ErrShortWrite`) or an independent check
of how many bytes actually reached the destination.

### bufio.Scanner reframes chunks into logical tokens

`bufio.Scanner` turns an arbitrarily-chunked byte stream into logical tokens
(lines, records) independent of `Read` boundaries — the right tool for NDJSON and
log ingestion where a record can straddle any read. Its constraints:

- a bounded maximum token size (`bufio.MaxScanTokenSize`, tunable via
  `Scanner.Buffer`); an oversized token yields `bufio.ErrTooLong`.
- errors surface through `Scanner.Err()` *after* the `Scan` loop returns false,
  not from `Scan` itself. Forgetting to call `Err()` swallows `ErrTooLong` and an
  underlying read error, so truncated input looks complete.
- `Scanner.Bytes()` returns a slice valid only until the next `Scan`; it is
  overwritten in place. Copy it (or use `Scanner.Text()`) to keep a token.

### Error classification decides retry vs. fail vs. reject

Streams terminate and fail in distinguishable ways, and `errors.Is`/`errors.As`
is how you tell them apart: `io.EOF` is normal termination; `io.ErrUnexpectedEOF`
is a truncated frame (corruption); `iotest.ErrTimeout` is a retryable transient;
`*http.MaxBytesError` is a policy limit breach (reject). Retrying every error
turns a permanent fault into an infinite loop; retrying none turns a transient
stall into a failed request. Classify, then decide, and bound the retry count.

### Why chunk-boundary robustness matters in production

Real networks deliver bytes in unpredictable sizes: a 64 KB HTTP body can arrive
as one `Read` in a loopback test and as forty ragged reads over a congested WAN.
A reader that only ever saw the friendly single-buffer read in CI is untested
against the one thing production guarantees. The adversarial `iotest` wrappers
reproduce that variability inside a deterministic unit test, which is the whole
point: prove the component survives before an incident does the proving for you.

## Common Mistakes

### Assuming one Read returns a whole logical unit

Wrong: reading a 4-byte header with a single `Read` and using whatever `n` comes
back. A short read leaves you with a partial header. Fix: use `io.ReadFull` for
fixed-size reads, or `bufio.Scanner` for framing, and test with
`iotest.OneByteReader` and `iotest.HalfReader`.

### Checking err before consuming the n returned bytes

Wrong: `if err != nil { return }` before processing `p[:n]`. When EOF arrives
with the last data (form 2), the final chunk is silently dropped. Fix: always
process `p[:n]` first, then handle `err`; verify with `iotest.DataErrReader`.

### Returning 0, nil at end-of-stream

Wrong: a custom reader that returns `0, nil` when drained. `io.ReadAll` and copy
loops spin or hang forever. Fix: return `io.EOF` (or `n>0`) so callers terminate.

### Treating io.LimitReader as a body-size guard

Wrong: `io.LimitReader(r.Body, maxBytes)` to cap a request. It truncates silently
with no error, so an oversized request reads as a valid short body. Fix: use
`net/http.MaxBytesReader` and check for `*http.MaxBytesError` with `errors.As`.

### Confusing io.EOF with io.ErrUnexpectedEOF

Wrong: treating a partial fixed-size read as clean termination. A partial frame
is corruption. Fix: use `io.ReadFull` and treat `io.ErrUnexpectedEOF` as an
error, `io.EOF`-on-a-boundary as success.

### Hand-rolling edge-case reader tests

Wrong: writing your own ad-hoc partial-read and EOF tests and missing the
multi-size-read and `ReaderAt`/`Seeker` coverage the stdlib provides. Fix: run
`iotest.TestReader` as the baseline contract test, then add domain-specific ones.

### Ignoring short writes

Wrong: assuming `w.Write(p)` always wrote `len(p)` bytes and dropping the
remainder. Fix: check `nn < len(p)` and surface `io.ErrShortWrite`; exercise the
path with `iotest.TruncateWriter` and an independent byte-count check.

### Retrying every read error indistinctly

Wrong: a read wrapper that retries on any error, or on none. Fix: classify with
`errors.Is` against `iotest.ErrTimeout` for the retryable case and bound the
retry count so a permanent fault cannot loop forever.

### Forgetting Scanner.Err() after the loop

Wrong: `for sc.Scan() { ... }` and returning without checking `sc.Err()`.
`bufio.ErrTooLong` or an underlying read error is swallowed and truncated input
looks complete. Fix: always check `sc.Err()` once the loop ends.

### Retaining Scanner.Bytes() past the next Scan

Wrong: appending `sc.Bytes()` to a slice and reading it later; the backing array
is reused and the value mutates. Fix: copy the bytes, or use `sc.Text()` which
allocates a fresh string.

Next: [01-transforming-reader-passes-iotest.md](01-transforming-reader-passes-iotest.md)
