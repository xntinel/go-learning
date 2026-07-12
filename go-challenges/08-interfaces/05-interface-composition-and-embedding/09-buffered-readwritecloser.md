# Exercise 9: A Buffered ReadWriteCloser that Flushes on Close

Composing `bufio` buffering over an underlying `io.ReadWriteCloser` (a network
conn, say): expose a `ReadWriteCloser` whose `Read` is buffered by a
`bufio.Reader`, whose `Write` is buffered by a `bufio.Writer`, and whose `Close`
FLUSHES the writer before closing the source, so no buffered bytes are silently
dropped. The core lesson is the order of operations inside a composed `Close`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
bufrwc/                     independent module: example.com/bufrwc
  go.mod                    go 1.26
  bufrwc.go                 type Conn (bufio.Reader + bufio.Writer + Closer); New, Read, Write, Flush, Close
  cmd/
    demo/
      main.go               buffered write is invisible until Close flushes it
  bufrwc_test.go            writes stay buffered until flush, Close flushes-then-closes, joined error, round-trip
```

- Files: `bufrwc.go`, `cmd/demo/main.go`, `bufrwc_test.go`.
- Implement: a `Conn` wrapping an `io.ReadWriteCloser` with a `bufio.Reader` and `bufio.Writer`; `Close` flushes the writer, then closes the source, joining errors with `errors.Join`.
- Test: buffered writes are not visible on the underlying store until `Flush`/`Close`; `Close` flushes then closes; `Close` returns a joined error if flush fails; a buffered read round-trips; a static assertion the wrapper satisfies `io.ReadWriteCloser`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/05-interface-composition-and-embedding/09-buffered-readwritecloser/cmd/demo
cd go-solutions/08-interfaces/05-interface-composition-and-embedding/09-buffered-readwritecloser
```

### Why Close order is a correctness property

`bufio.Writer` accumulates writes in an in-memory buffer and only pushes them to
the underlying writer when the buffer fills or you call `Flush`. That is the whole
point — fewer, larger syscalls — but it means bytes you have "written" may not have
reached the wire yet. If your composed `Close` closes the underlying stream without
flushing first, every byte still sitting in the `bufio.Writer` buffer is lost, with
no error at the `Write` call site. The last partial record of a protocol vanishes,
and the failure is intermittent because it depends on whether the buffer happened to
fill.

So the composed `Close` has a mandatory order: `Flush` the writer, *then* `Close`
the source. Both can fail — the flush can fail because the underlying `Write`
failed, and the close can fail on its own — so you join the two with `errors.Join`
rather than returning only one. Reads are simpler: `bufio.Reader` reads ahead into
its buffer and serves `Read` from it, transparently, so `Read` just delegates. The
type composes three things — a `bufio.Reader`, a `bufio.Writer`, and the underlying
`io.Closer` — into one `io.ReadWriteCloser`, which is interface composition
(`ReadWriter` + `Closer`) realized by a concrete struct.

Create `bufrwc.go`:

```go
package bufrwc

import (
	"bufio"
	"errors"
	"io"
)

// Conn wraps an io.ReadWriteCloser with buffered reads and writes. Close FLUSHES
// the write buffer before closing the underlying stream, so no buffered bytes are
// lost.
type Conn struct {
	r    *bufio.Reader
	w    *bufio.Writer
	base io.Closer
}

// New wraps rwc with a buffered reader and writer over the same stream.
func New(rwc io.ReadWriteCloser) *Conn {
	return &Conn{
		r:    bufio.NewReader(rwc),
		w:    bufio.NewWriter(rwc),
		base: rwc,
	}
}

// Read serves from the read-ahead buffer, delegating to the underlying stream.
func (c *Conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Write buffers p; the bytes reach the underlying stream on Flush or Close.
func (c *Conn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Flush pushes any buffered writes to the underlying stream.
func (c *Conn) Flush() error { return c.w.Flush() }

// Close flushes the write buffer, then closes the underlying stream, joining both
// errors so neither is lost. The order matters: flushing after Close would drop
// buffered bytes.
func (c *Conn) Close() error {
	return errors.Join(c.w.Flush(), c.base.Close())
}

// Static assertion: the wrapper satisfies io.ReadWriteCloser.
var _ io.ReadWriteCloser = (*Conn)(nil)
```

### The runnable demo

The demo's underlying stream is a trivial in-memory sink so you can watch the
buffered write stay invisible until `Close` flushes it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"

	"example.com/bufrwc"
)

// memStream is a trivial in-memory io.ReadWriteCloser for the demo.
type memStream struct {
	out []byte
}

func (m *memStream) Read(p []byte) (int, error)  { return 0, io.EOF }
func (m *memStream) Write(p []byte) (int, error) { m.out = append(m.out, p...); return len(p), nil }
func (m *memStream) Close() error                { return nil }

func main() {
	base := &memStream{}
	conn := bufrwc.New(base)

	fmt.Fprint(conn, "buffered write")
	fmt.Printf("underlying before flush: %d bytes\n", len(base.out))

	conn.Close()
	fmt.Printf("underlying after close: %q\n", base.out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
underlying before flush: 0 bytes
underlying after close: "buffered write"
```

### Tests

Create `bufrwc_test.go`:

```go
package bufrwc

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

// memRWC is a spy io.ReadWriteCloser recording what actually reached the stream.
type memRWC struct {
	mu     sync.Mutex
	buf    []byte
	read   int
	closed bool
}

func (m *memRWC) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.read >= len(m.buf) {
		return 0, io.EOF
	}
	n := copy(p, m.buf[m.read:])
	m.read += n
	return n, nil
}

func (m *memRWC) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = append(m.buf, p...)
	return len(p), nil
}

func (m *memRWC) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memRWC) written() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.buf)
}

// failRWC fails every write, so Flush fails.
type failRWC struct{}

func (failRWC) Read(p []byte) (int, error)  { return 0, io.EOF }
func (failRWC) Write(p []byte) (int, error) { return 0, errors.New("write failed") }
func (failRWC) Close() error                { return nil }

func TestWritesStayBufferedUntilFlush(t *testing.T) {
	t.Parallel()
	base := &memRWC{}
	c := New(base)

	if _, err := c.Write([]byte("buffered")); err != nil {
		t.Fatal(err)
	}
	if base.written() != "" {
		t.Fatalf("bytes reached underlying before flush: %q", base.written())
	}

	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if base.written() != "buffered" {
		t.Fatalf("after flush = %q, want buffered", base.written())
	}
}

func TestCloseFlushesThenCloses(t *testing.T) {
	t.Parallel()
	base := &memRWC{}
	c := New(base)

	if _, err := c.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if base.written() != "payload" {
		t.Fatalf("Close did not flush: %q", base.written())
	}
	if !base.closed {
		t.Fatal("underlying stream not closed")
	}
}

func TestCloseJoinsFlushError(t *testing.T) {
	t.Parallel()
	c := New(failRWC{})
	if _, err := c.Write([]byte("data that will fail to flush")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("Close returned nil despite a flush failure")
	}
}

func TestBufferedReadRoundTrips(t *testing.T) {
	t.Parallel()
	base := &memRWC{buf: []byte("hello world")}
	c := New(base)

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("read = %q, want hello world", got)
	}
}

func Example() {
	base := &memRWC{}
	c := New(base)
	fmt.Fprint(c, "queued")
	fmt.Printf("before close: %d bytes\n", len(base.written()))
	c.Close()
	fmt.Printf("after close: %s\n", base.written())
	// Output:
	// before close: 0 bytes
	// after close: queued
}
```

## Review

The wrapper is correct when a write smaller than the `bufio` buffer is invisible on
the underlying stream until `Flush` or `Close`
(`TestWritesStayBufferedUntilFlush`), when `Close` flushes *and then* closes so the
buffered bytes survive and the source is actually closed
(`TestCloseFlushesThenCloses`), and when a flush failure is not swallowed by a
successful close (`TestCloseJoinsFlushError`). The mistake this exists to prevent is
a `Close` that closes the source without flushing — it passes a naive round-trip
test and silently drops the last buffer in production. The `Example` makes the
buffering visible: `before close` reports zero bytes on the underlying stream
because they are still in the `bufio.Writer`; `after close` shows them flushed.
Order of operations in a composed
`Close` is not a style choice; it is the correctness property.

## Resources

- [`bufio.NewReader`](https://pkg.go.dev/bufio#NewReader) and [`bufio.NewWriter`](https://pkg.go.dev/bufio#NewWriter) — the buffering layers being composed.
- [`bufio.Writer.Flush`](https://pkg.go.dev/bufio#Writer.Flush) — the call `Close` must make before closing the source.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining the flush and close errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-audit-fanout-multiwriter.md](08-audit-fanout-multiwriter.md) | Next: [../06-interface-segregation/00-concepts.md](../06-interface-segregation/00-concepts.md)
