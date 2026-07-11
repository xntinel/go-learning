# Exercise 6: Byte Ring Buffer Implementing io.Reader and io.Writer

Between a `net.Conn` read loop and a protocol parser sits a bounded byte buffer.
This module builds a `ByteRing` that implements `io.Writer` (with backpressure —
short write plus `io.ErrShortWrite` when full, never a silent overwrite) and
`io.Reader` (draining from the tail). It is the one place where the `io` contracts
and the ring's eviction policy collide, and the resolution is the senior lesson:
a byte ring behind `io.Writer` must reject-newest, not overwrite-oldest.

Self-contained: its own module, the `ByteRing`, a demo, and tests including
`io.Copy` interop.

## What you'll build

```text
byteringio/                independent module: example.com/byteringio
  go.mod                   go 1.24
  bytering.go              ByteRing: Write (io.Writer), Read (io.Reader), Len, Free
  cmd/
    demo/
      main.go              fill past capacity, observe short write, drain
  bytering_test.go         short-write on overflow, FIFO across wrap, io.Copy interop
```

Files: `bytering.go`, `cmd/demo/main.go`, `bytering_test.go`.
Implement: `ByteRing` with `Write(p) (int, error)`, `Read(p) (int, error)`, `Len`, `Free`.
Test: writing more than free space returns `n < len(p)` with `io.ErrShortWrite`; interleaved Write/Read is byte-for-byte FIFO across wraparound; `io.Copy` through the ring in bounded chunks preserves bytes.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/byteringio/cmd/demo
cd ~/go-exercises/byteringio
go mod init example.com/byteringio
go mod edit -go=1.24
```

### Why Write rejects instead of overwriting

Overwrite-oldest is correct for telemetry, where the newest sample is the one you
want. It is catastrophic for a byte stream, because the bytes in the buffer are
*committed data* a consumer has not read yet. Overwriting them silently corrupts
the stream. The `io.Writer` contract encodes this: `Write` must return
`n == len(p), nil` only if it accepted every byte, and otherwise return `n < len(p)`
with a non-nil error. The canonical error for "I could not take all of it" is
`io.ErrShortWrite`. So `ByteRing.Write` copies as many bytes as fit in the free
space, and if that is fewer than `len(p)`, it returns the partial count and
`io.ErrShortWrite`. The caller — an `io.Copy`, a framer — sees the short write and
knows to retry after the consumer drains some bytes. This is backpressure expressed
through the return values.

`Read` is the mirror: it copies up to `len(p)` bytes from the tail, returns how many
it moved, and a nil error. When the buffer is empty it returns `0, nil`. It does not
return `io.EOF`, because an open ring is not at end of stream — EOF would falsely
tell the reader no more bytes will ever come. (A higher layer that knows the stream
is closed can translate an empty ring into EOF; the buffer itself must not.)

### Wraparound with two copies

Because the free (or filled) region can straddle the end of the backing array, both
`Write` and `Read` may need two `copy` calls: one from the current index to the end
of the array, and one from the start of the array for the remainder. The byte count
`n` is computed first (the min of the request and the available space), then the
index math splits `n` into the tail-of-array part and the wrap part. Getting the
split right is the whole implementation; `copy` returns the number of bytes moved,
which the code uses to advance the indices.

Create `bytering.go`:

```go
package bytering

import "io"

// ByteRing is a fixed-capacity byte buffer implementing io.Reader and io.Writer.
// Write applies backpressure (short write + io.ErrShortWrite) when full rather
// than overwriting unread bytes, as the io.Writer contract requires. It is not
// safe for concurrent use.
type ByteRing struct {
	data []byte
	head int // next write index
	tail int // next read index
	size int // bytes currently buffered
}

// NewByteRing returns a ByteRing holding at most capacity bytes (clamped to >= 1).
func NewByteRing(capacity int) *ByteRing {
	if capacity <= 0 {
		capacity = 1
	}
	return &ByteRing{data: make([]byte, capacity)}
}

// Len reports the number of buffered bytes.
func (b *ByteRing) Len() int { return b.size }

// Free reports how many more bytes Write can accept right now.
func (b *ByteRing) Free() int { return len(b.data) - b.size }

// Write copies bytes from p into the ring. If p does not fit in the free space it
// writes what it can and returns io.ErrShortWrite with the partial count.
func (b *ByteRing) Write(p []byte) (int, error) {
	n := min(len(p), b.Free())
	written := 0
	for written < n {
		// Copy from head to the end of the array, then wrap.
		chunk := copy(b.data[b.head:], p[written:n])
		b.head = (b.head + chunk) % len(b.data)
		written += chunk
	}
	b.size += written
	if written < len(p) {
		return written, io.ErrShortWrite
	}
	return written, nil
}

// Read copies up to len(p) buffered bytes into p, draining from the tail. It
// returns 0, nil when the buffer is empty; it never returns io.EOF, since an open
// ring is not at end of stream.
func (b *ByteRing) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := min(len(p), b.size)
	read := 0
	for read < n {
		chunk := copy(p[read:n], b.data[b.tail:])
		b.tail = (b.tail + chunk) % len(b.data)
		read += chunk
	}
	b.size -= read
	return read, nil
}
```

Note the `copy(b.data[b.head:], ...)` slices from `head` to the end of the array, so
each iteration copies at most one contiguous run; the loop runs at most twice
(once before the wrap, once after). `min` is the built-in added in Go 1.21.

### The runnable demo

The demo makes a capacity-8 ring, writes "hello world" (11 bytes) so only the first
8 are accepted and it reports the short write, drains 5 bytes, then writes the
leftover, showing backpressure and recovery.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"

	"example.com/byteringio"
)

func main() {
	b := bytering.NewByteRing(8)

	msg := []byte("hello world")
	n, err := b.Write(msg)
	fmt.Printf("wrote %d of %d bytes, err=%v\n", n, len(msg), errors.Is(err, io.ErrShortWrite))

	buf := make([]byte, 5)
	rn, _ := b.Read(buf)
	fmt.Printf("read %d bytes: %q\n", rn, string(buf[:rn]))

	// Now there is room; write the rest of the message.
	rest := msg[n:]
	n2, err2 := b.Write(rest)
	fmt.Printf("wrote %d of %d leftover bytes, err=%v\n", n2, len(rest), err2)

	out := make([]byte, 16)
	rn2, _ := b.Read(out)
	fmt.Printf("drained: %q\n", string(out[:rn2]))
}
```

The module path is `example.com/byteringio` but the package is `bytering`, so the
import path is `example.com/byteringio` while the qualifier is `bytering`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote 8 of 11 bytes, err=true
read 5 bytes: "hello"
wrote 3 of 3 leftover bytes, err=<nil>
drained: " world"
```

The first write took only "hello wo" (8 bytes) and reported the short write; after
draining "hello", the leftover "rld" fit; the final drain yields " world" (a space
plus "world"), reconstructing the original stream in order across the wrap.

### Tests

The tests pin the three `io` contracts. Overflow: writing more than `Free` returns a
short count with `io.ErrShortWrite`. FIFO fidelity across wrap: interleave writes and
reads so the indices wrap, and assert the bytes come out exactly as they went in.
Streaming interop: pump a source through the ring into a sink in bounded chunks and
assert byte-for-byte equality — this exercises the ring as a real `io.ReadWriter`
under repeated backpressure. (A bare `io.Copy(sink, ring)` would loop forever,
since the ring deliberately never returns `io.EOF`; the bounded pump is the honest
way to drive an open buffer.)

Create `bytering_test.go`:

```go
package bytering

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestWriteShortWhenFull(t *testing.T) {
	t.Parallel()
	b := NewByteRing(4)
	n, err := b.Write([]byte("abcdef")) // 6 into cap 4
	if n != 4 {
		t.Fatalf("Write n = %d, want 4", n)
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write err = %v, want io.ErrShortWrite", err)
	}
	// The buffered bytes must be the FIRST four, not the last four (no overwrite).
	out := make([]byte, 4)
	rn, _ := b.Read(out)
	if got := string(out[:rn]); got != "abcd" {
		t.Fatalf("buffered = %q, want %q (Write overwrote committed bytes)", got, "abcd")
	}
}

func TestFIFOAcrossWraparound(t *testing.T) {
	t.Parallel()
	b := NewByteRing(4)
	var got []byte
	// Write 2, read 2, repeatedly, so head/tail sweep past the array end.
	src := []byte("abcdefghij")
	for i := 0; i < len(src); i += 2 {
		if _, err := b.Write(src[i : i+2]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		tmp := make([]byte, 2)
		rn, _ := b.Read(tmp)
		got = append(got, tmp[:rn]...)
	}
	if string(got) != string(src) {
		t.Fatalf("round-tripped %q, want %q", got, src)
	}
}

func TestReadEmptyReturnsZeroNil(t *testing.T) {
	t.Parallel()
	b := NewByteRing(4)
	n, err := b.Read(make([]byte, 4))
	if n != 0 || err != nil {
		t.Fatalf("Read on empty = (%d, %v), want (0, nil); must not return io.EOF", n, err)
	}
}

func TestStreamThroughRingPreservesBytes(t *testing.T) {
	t.Parallel()
	b := NewByteRing(8)
	src := bytes.Repeat([]byte("0123456789"), 5) // 50 bytes through an 8-byte ring
	var sink bytes.Buffer

	remaining := src
	tmp := make([]byte, 8)
	for len(remaining) > 0 || b.Len() > 0 {
		if len(remaining) > 0 {
			n, err := b.Write(remaining)
			remaining = remaining[n:]
			if err != nil && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("Write: %v", err)
			}
		}
		rn, _ := b.Read(tmp)
		sink.Write(tmp[:rn])
	}
	if !bytes.Equal(sink.Bytes(), src) {
		t.Fatalf("copy through ring corrupted %d bytes", len(src))
	}
}
```

## Review

The buffer is correct when it never loses a committed byte and reconstructs the
stream in order across the wrap. `TestWriteShortWhenFull` is the contract test: it
proves `Write` reports `io.ErrShortWrite` and keeps the *earliest* bytes, not the
latest — the opposite of the telemetry ring's overwrite-oldest. `TestFIFOAcrossWraparound`
and `TestStreamThroughRingPreservesBytes` prove byte fidelity when head and tail sweep past the
array boundary. The two traps: overwriting unread bytes on a full `Write` (silent
data loss, a broken `io.Writer`), and returning `io.EOF` from an empty `Read` (a
false end-of-stream that makes a reader give up on a still-open buffer). Return the
short write for the first and `0, nil` for the second.

## Resources

- [`io` package](https://pkg.go.dev/io) — `io.Reader`, `io.Writer`, `io.ErrShortWrite`, and the `Read`/`Write` contracts.
- [`bytes` package](https://pkg.go.dev/bytes) — `bytes.Buffer` as a reference growable buffer and `bytes.Equal`.
- [`builtin` min/max](https://pkg.go.dev/builtin#min) — the Go 1.21 built-ins used for the copy-size math.

---

Back to [05-sliding-window-latency-percentiles.md](05-sliding-window-latency-percentiles.md) | Next: [07-blocking-bounded-queue-cond.md](07-blocking-bounded-queue-cond.md)
