# Exercise 7: Compose Small Ports Into a ReadWriteCloser-Style Interface When a Consumer Needs Several

Segregation is not "always one method." Sometimes a consumer legitimately needs
several roles at once — a bidirectional protocol handler must read, write, and
close. The right move is to *compose* small interfaces by embedding, exactly as
the standard library builds `io.ReadWriteCloser` from `Reader`, `Writer`, and
`Closer`, rather than hand-writing a new fat interface. This module builds a
`Session` port by composition and a `drainAndClose` consumer that needs only a
subset.

## What you'll build

```text
session/                       independent module: example.com/session
  go.mod                       go 1.24
  session.go                   Session = io.ReadWriteCloser (embedded); ReadCloser subset; drainAndClose
  conn.go                      fakeConn implements Read/Write/Close; tracks closed
  cmd/
    demo/
      main.go                  writes, drains, closes a fakeConn through composed ports
  session_test.go              satisfies ReadWriteCloser + ReadCloser; drainAndClose reads to EOF, closes once
```

Files: `session.go`, `conn.go`, `cmd/demo/main.go`, `session_test.go`.
Implement: a `Session` interface composed by embedding `io.Reader`, `io.Writer`, `io.Closer`; a smaller `ReadCloser`; and `drainAndClose(ReadCloser)`.
Test: `fakeConn` satisfies `io.ReadWriteCloser` and the local `ReadCloser`; `drainAndClose` reads to EOF then closes exactly once; double-close and post-close read are handled.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/06-interface-segregation/07-compose-role-interfaces/cmd/demo
cd go-solutions/08-interfaces/06-interface-segregation/07-compose-role-interfaces
go mod edit -go=1.24
```

### Compose, do not re-declare

When a consumer needs read, write, and close, the wrong instinct is to write a
fresh three-method interface listing `Read`, `Write`, and `Close` again. That
duplicates signatures that already exist as `io.Reader`, `io.Writer`, and
`io.Closer`, and it means a value satisfying the standard `io.ReadWriteCloser`
would not automatically satisfy your look-alike unless the method sets happen to
match exactly. Composition by embedding avoids all of that: `Session` embeds the
three standard interfaces, so it *is* `io.ReadWriteCloser` structurally, and any
`net.Conn`, `os.File`, or fake that satisfies those satisfies `Session` for free.

The counterpart is the subset. `drainAndClose` reads a connection to EOF and
closes it — it never writes. So it declares `ReadCloser` (embedding only
`io.Reader` and `io.Closer`), which is `io.ReadCloser` in the standard library.
The consumer that needs two roles composes two; the consumer that needs three
composes three. Segregate into atoms, compose at the call site to the exact set
each site uses.

`drainAndClose` is a real graceful-shutdown shape: drain any remaining bytes off
the connection (so the peer's writes are consumed and the socket can close
cleanly), then close exactly once. Closing twice is a common bug; the fake tracks
a `closed` flag and returns a sentinel on the second close so tests can assert
idempotent-close discipline.

Create `session.go`:

```go
package session

import (
	"fmt"
	"io"
)

// Session is composed by embedding the three standard one-method interfaces,
// exactly like io.ReadWriteCloser. It is NOT a hand-written fat interface.
type Session interface {
	io.Reader
	io.Writer
	io.Closer
}

// ReadCloser is the subset a drain-and-close consumer needs: read + close, no
// write. Same shape as io.ReadCloser.
type ReadCloser interface {
	io.Reader
	io.Closer
}

// drainAndClose reads rc to EOF, discarding the bytes, then closes it once. It
// depends on ReadCloser, so it structurally cannot write to the connection.
func drainAndClose(rc ReadCloser) (int64, error) {
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		// Still attempt to close, but report the drain error.
		_ = rc.Close()
		return n, fmt.Errorf("drain: %w", err)
	}
	if err := rc.Close(); err != nil {
		return n, fmt.Errorf("close: %w", err)
	}
	return n, nil
}
```

Create `conn.go`. The fake implements all three methods and tracks close state:

```go
package session

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// ErrAlreadyClosed is returned by a second Close on the same connection.
var ErrAlreadyClosed = errors.New("connection already closed")

// fakeConn is an in-memory bidirectional connection: a read buffer, a write
// buffer, and a close flag. It satisfies io.ReadWriteCloser.
type fakeConn struct {
	mu       sync.Mutex
	readBuf  *bytes.Buffer
	writeBuf bytes.Buffer
	closed   bool
}

// newFakeConn seeds the read side with the bytes the peer will "send".
func newFakeConn(incoming string) *fakeConn {
	return &fakeConn{readBuf: bytes.NewBufferString(incoming)}
}

func (c *fakeConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.EOF
	}
	return c.readBuf.Read(p)
}

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, ErrAlreadyClosed
	}
	return c.writeBuf.Write(p)
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrAlreadyClosed
	}
	c.closed = true
	return nil
}

// isClosed reports whether Close has run (test helper).
func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Compile-time proof the fake satisfies the composed and subset interfaces.
var (
	_ io.ReadWriteCloser = (*fakeConn)(nil)
	_ Session            = (*fakeConn)(nil)
	_ ReadCloser         = (*fakeConn)(nil)
)
```

### The runnable demo

Create `cmd/demo/main.go`. Expose an exported drain entry point.

Add to `session.go`:

```go
// DrainAndClose is the exported wrapper over drainAndClose for demos and
// external consumers that hold only a ReadCloser.
func DrainAndClose(rc ReadCloser) (int64, error) {
	return drainAndClose(rc)
}

// NewConn returns a fakeConn seeded with incoming bytes, as a Session.
func NewConn(incoming string) Session {
	return newFakeConn(incoming)
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"

	"example.com/session"
)

func main() {
	// A full Session: read, write, close.
	conn := session.NewConn("hello from peer")

	// Write through the Writer role.
	_, _ = io.WriteString(conn, "request payload")
	fmt.Println("wrote request")

	// Drain and close through the narrower ReadCloser role.
	n, err := session.DrainAndClose(conn)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("drained %d bytes and closed\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote request
drained 15 bytes and closed
```

### Tests

Create `session_test.go`:

```go
package session

import (
	"errors"
	"io"
	"testing"
)

func TestFakeConnSatisfiesComposedPorts(t *testing.T) {
	t.Parallel()

	c := newFakeConn("data")
	// Usable as the full composed Session and as the ReadCloser subset.
	var s Session = c
	var rc ReadCloser = c
	if s == nil || rc == nil {
		t.Fatal("fakeConn should satisfy both composed interfaces")
	}
}

func TestDrainAndCloseReadsToEOFAndClosesOnce(t *testing.T) {
	t.Parallel()

	c := newFakeConn("twelve bytes")
	n, err := drainAndClose(c)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("twelve bytes")) {
		t.Fatalf("drained %d, want %d", n, len("twelve bytes"))
	}
	if !c.isClosed() {
		t.Fatal("connection should be closed")
	}
}

func TestDoubleCloseReportsSentinel(t *testing.T) {
	t.Parallel()

	c := newFakeConn("x")
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("second close err = %v, want ErrAlreadyClosed", err)
	}
}

func TestPostCloseReadReturnsEOF(t *testing.T) {
	t.Parallel()

	c := newFakeConn("unread")
	_ = c.Close()

	buf := make([]byte, 8)
	n, err := c.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("post-close read = %d,%v; want 0,EOF", n, err)
	}
}

func TestDrainAndCloseNeedsOnlyReadClose(t *testing.T) {
	t.Parallel()

	// A type with ONLY Read and Close (no Write) still works with drainAndClose,
	// proving the consumer's dependency is the subset, not the full Session.
	var rc ReadCloser = readOnlyClose{data: "abc"}
	n, err := drainAndClose(rc)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("drained %d, want 3", n)
	}
}

// readOnlyClose has no Write method at all; it satisfies ReadCloser but not
// Session, demonstrating the subset dependency.
type readOnlyClose struct {
	data string
	pos  int
}

func (r readOnlyClose) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	if r.pos >= len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

func (r readOnlyClose) Close() error { return nil }

var _ ReadCloser = readOnlyClose{}
```

Note `readOnlyClose.Read` takes a value receiver, so `r.pos += n` does not
persist across calls; the method returns `io.EOF` alongside the final bytes in a
single read, which `io.Copy` handles correctly for this small input. That keeps
the type minimal while still draining fully.

## Review

The composition is correct when `Session` is built by embedding `io.Reader`,
`io.Writer`, and `io.Closer` — not by re-listing `Read`/`Write`/`Close` — so any
`io.ReadWriteCloser` satisfies it automatically and the standard-library ecosystem
composes with your port. The subset `ReadCloser` proves the counter-point: a
consumer that only drains and closes depends on two roles, not three, and
`readOnlyClose` (which has no `Write` at all) still satisfies it. The failure
modes the tests pin are the real ones: closing twice must be idempotent-safe
(sentinel on the second call), and a post-close read must not read stale data.
Run `go test -race` because the fake's buffers and close flag are guarded by a
mutex against concurrent use.

## Resources

- [io package (Reader, Writer, Closer, ReadWriteCloser, ReadCloser, Copy, Discard)](https://pkg.go.dev/io)
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding)
- [Go Specification: Interface types (embedding)](https://go.dev/ref/spec#Interface_types)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-narrow-port-for-test-doubles.md](06-narrow-port-for-test-doubles.md) | Next: [08-health-check-aggregator.md](08-health-check-aggregator.md)
