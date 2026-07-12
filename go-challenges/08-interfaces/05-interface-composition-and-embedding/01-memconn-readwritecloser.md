# Exercise 1: A Thread-Safe In-Memory ReadWriteCloser

The foundational artifact of this chapter: a composed `ReadWriteCloser` interface
that unions `io.Reader`, `io.Writer`, and `io.Closer`, and a mutex-guarded
`MemConn` that satisfies it. It is the in-memory stand-in you reach for when a test
needs a `net.Conn`-shaped thing without a socket.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
memconn/                    independent module: example.com/memconn
  go.mod                    go 1.26
  memconn.go                type ReadWriteCloser; type MemConn; New, Read, Write, Close, Buffer
  cmd/
    demo/
      main.go               write, read, EOF, double-close against the ReadWriteCloser contract
  memconn_test.go           satisfaction, round-trip, ErrClosed, race-tested concurrent Read/Write
```

- Files: `memconn.go`, `cmd/demo/main.go`, `memconn_test.go`.
- Implement: a `ReadWriteCloser` interface composing the three `io` interfaces, and a concurrency-safe `MemConn` with `New`, `Read`, `Write`, `Close`, `Buffer`, returning `ErrClosed` on use-after-close and on double-close.
- Test: implicit-satisfaction assignment, round-trip read/write, read-after-close and double-close both `ErrClosed`, three individual `io` static assertions, and a `-race` concurrent Read/Write test.
- Verify: `go test -count=1 -race ./...`

### Why a composed interface, and why MemConn does not embed one

`ReadWriteCloser` embeds three single-method interfaces. That is composition at the
contract level: it is the *union* of `Read`, `Write`, and `Close`, and any type
with those three methods satisfies it structurally, with no `implements`
declaration. `MemConn` is the other side of the coin — a concrete type that
*implements* all three methods directly rather than embedding another value. It
does not embed an interface, because it owns its state (a byte buffer, a read
cursor, a closed flag) and there is nothing to delegate to; the whole point is that
it *is* the implementation. Later exercises embed an interface to wrap an existing
implementation; this one is the primitive being wrapped.

Two design decisions matter. First, `ErrClosed` is a package-level sentinel so
callers can branch on it with `errors.Is`, and both use-after-close and
double-close return it: `Close` is safe to call from multiple `defer`s and reports
the redundant call rather than panicking. Second, the mutex guards every field, so
concurrent `Read` and `Write` from different goroutines is a defined, race-free
operation — the `-race` test pins that contract.

Create `memconn.go`. Note that it implements `Read`/`Write`/`Close` directly and
reads no external clock or resource:

```go
package memconn

import (
	"errors"
	"io"
	"sync"
)

// ErrClosed is returned by Read, Write, and a redundant Close after the
// connection has been closed. It is a sentinel so callers can use errors.Is.
var ErrClosed = errors.New("memconn: closed")

// ReadWriteCloser is the union of the three single-method io interfaces. A type
// satisfies it exactly when it satisfies io.Reader, io.Writer, and io.Closer.
type ReadWriteCloser interface {
	io.Reader
	io.Writer
	io.Closer
}

// MemConn is a concurrency-safe, in-memory ReadWriteCloser. Written bytes are
// appended to an internal buffer; reads consume from a cursor. It is the
// in-memory stand-in used where a test needs a stream without a real socket.
type MemConn struct {
	mu     sync.Mutex
	buffer []byte
	read   int
	closed bool
}

// New returns an open, empty MemConn.
func New() *MemConn {
	return &MemConn{}
}

// Read consumes unread bytes into p. It returns io.EOF once the buffer is drained
// and ErrClosed after Close.
func (m *MemConn) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrClosed
	}
	if m.read >= len(m.buffer) {
		return 0, io.EOF
	}
	n := copy(p, m.buffer[m.read:])
	m.read += n
	return n, nil
}

// Write appends p to the buffer. It returns ErrClosed after Close.
func (m *MemConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrClosed
	}
	m.buffer = append(m.buffer, p...)
	return len(p), nil
}

// Close marks the connection closed. A second Close returns ErrClosed rather than
// panicking, so it is safe to call from multiple defers and error paths.
func (m *MemConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	m.closed = true
	return nil
}

// Buffer returns a copy of the currently unread bytes, for inspection in tests.
func (m *MemConn) Buffer() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.buffer)-m.read)
	copy(out, m.buffer[m.read:])
	return out
}

// Static assertions: *MemConn satisfies each io interface and the composed one
// implicitly. These fail at compile time if a method signature drifts.
var _ io.Reader = (*MemConn)(nil)
var _ io.Writer = (*MemConn)(nil)
var _ io.Closer = (*MemConn)(nil)
var _ ReadWriteCloser = (*MemConn)(nil)
```

### The runnable demo

The demo drives `MemConn` through the `ReadWriteCloser` interface variable, to make
the point that the concrete type is never named after construction:

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"

	"example.com/memconn"
)

func main() {
	var conn memconn.ReadWriteCloser = memconn.New()

	n, _ := conn.Write([]byte("session=alice"))
	fmt.Printf("wrote %d bytes\n", n)

	buf := make([]byte, 32)
	n, _ = conn.Read(buf)
	fmt.Printf("read %d bytes: %s\n", n, buf[:n])

	if _, err := conn.Read(buf); err == io.EOF {
		fmt.Println("second read: EOF")
	}

	fmt.Println("first close:", conn.Close())
	fmt.Println("second close:", conn.Close())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote 13 bytes
read 13 bytes: session=alice
second read: EOF
first close: <nil>
second close: memconn: closed
```

### Tests

The suite keeps every original assertion — implicit satisfaction, round-trip,
read-after-close, double-close, and the three individual `io` static assertions —
and adds `TestConcurrentReadWrite`, which drives `Read` and `Write` from many
goroutines so `go test -race` proves the mutex actually serializes access.

Create `memconn_test.go`:

```go
package memconn

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

func TestSatisfiesReadWriteCloser(t *testing.T) {
	t.Parallel()
	var rwc ReadWriteCloser = New()
	if err := rwc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c := New()
	n, err := c.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d,%v; want 5,nil", n, err)
	}

	buf := make([]byte, 5)
	n, err = c.Read(buf)
	if err != nil || n != 5 {
		t.Fatalf("Read = %d,%v; want 5,nil", n, err)
	}
	if !bytes.Equal(buf, []byte("hello")) {
		t.Fatalf("buf = %q, want hello", buf)
	}
}

func TestRejectsReadAfterClose(t *testing.T) {
	t.Parallel()
	c := New()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := c.Read(buf); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read after close = %v, want ErrClosed", err)
	}
}

func TestRejectsDoubleClose(t *testing.T) {
	t.Parallel()
	c := New()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
}

func TestImplementsIOInterfaces(t *testing.T) {
	t.Parallel()
	c := New()
	var _ io.Reader = c
	var _ io.Writer = c
	var _ io.Closer = c
}

func TestConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	c := New()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = c.Write([]byte{byte(i)})
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, 8)
			_, _ = c.Read(buf)
		}()
	}
	wg.Wait()
}

func Example() {
	c := New()
	c.Write([]byte("payload"))
	buf := make([]byte, 7)
	n, _ := c.Read(buf)
	fmt.Printf("%d %s\n", n, buf[:n])
	// Output: 7 payload
}
```

## Review

`MemConn` is correct when every field access happens under the mutex and the state
machine is exact: `Read` returns `ErrClosed` after close, `io.EOF` when drained,
and consumed bytes otherwise; `Write` returns `ErrClosed` after close and appends
otherwise; `Close` flips the flag once and reports the redundant call. The most
common way to get this wrong is to make `Close` panic or succeed silently on the
second call — both hide the double-close bug that idempotent-with-sentinel makes
visible. The static `var _ ... = (*MemConn)(nil)` lines are not decoration: they
turn a signature drift (say, `Read(p []byte) int`) into a compile error instead of
a runtime surprise at a call site. Run `go test -race` to confirm the concurrent
Read/Write test finds no data race.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types) — interface embedding and method-set unions.
- [`io` package](https://pkg.go.dev/io) — `Reader`, `Writer`, `Closer`, and the composed `ReadWriteCloser`.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock guarding the buffer.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-instrumented-responsewriter.md](02-instrumented-responsewriter.md)
