# 4. Connection Timeouts and Deadlines

TCP reads and writes block until data arrives or the peer closes the connection.
Without explicit deadlines a goroutine servicing a slow or silent client waits
forever, holding memory and a file descriptor. This lesson teaches the three
control points Go exposes — dial timeout, per-direction operation deadline, and
the sliding-window idle-timeout pattern — and shows how to detect timeout errors
correctly in the presence of error wrapping.

```text
deadline/
  go.mod
  deadline.go
  deadline_test.go
  cmd/demo/main.go
```

## Concepts

### The Deadline Model

Every `net.Conn` carries three independent deadlines: a combined one set by
`SetDeadline`, a read-only one set by `SetReadDeadline`, and a write-only one set
by `SetWriteDeadline`. A deadline is an absolute `time.Time`, not a duration.
Passing a zero `time.Time{}` clears the deadline and disables the timeout for
that direction.

When a deadline is exceeded, the next (or current) read or write returns an
error that wraps `os.ErrDeadlineExceeded`. The deadline does not automatically
renew; to implement a rolling idle timeout the code must call `SetReadDeadline`
again before each read.

### Dial Timeouts

`net.DialTimeout("tcp", addr, d)` is a one-liner: it dials and returns an error
if the TCP handshake is not complete within `d`. The lower-level
`net.Dialer{Timeout: d}` exposes the same limit plus optional keep-alive and
local address binding. For context-based cancellation use
`net.Dialer{}.DialContext(ctx, "tcp", addr)` — cancelling the context is
equivalent to the timeout firing, but the caller gets to decide when.

Dial timeout and operation deadlines are independent. A successful dial with a
3-second timeout starts a connection with no deadlines; subsequent reads and
writes block forever unless the caller sets deadlines on the returned `net.Conn`.

### Detecting Timeout Errors

The `net.Error` interface exposes `Timeout() bool`. Use `errors.As` to unwrap,
not a bare type assertion, because `net` wraps errors in `*net.OpError`:

```go
func IsTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
```

Equivalent (Go 1.20+): `errors.Is(err, os.ErrDeadlineExceeded)`. Both work;
`net.Error.Timeout()` also catches OS-level timeouts that pre-date the sentinel.

EOF (`io.EOF`) is not a timeout: it means the peer closed the connection
normally. Connection reset is not a timeout either. Only deadline expiry sets
`Timeout() == true`.

### The Sliding-Window Idle Timeout

Setting a single deadline at connection open and never updating it means the
connection times out from the moment it was established, not from the last
activity. The correct pattern:

1. Before each `Read`, call `SetReadDeadline(time.Now().Add(idleTimeout))`.
2. If the read succeeds, the next call to `Read` resets the deadline again.
3. If the read times out, `idleTimeout` has elapsed since the last successful
   read — the client is genuinely idle.

A wrapper type that overrides `Read` (and `Write`) is idiomatic: the handler
code sees a plain `net.Conn` and the timeout logic is in one place.

### Failure Modes

`SetDeadline` takes `time.Time`, not `time.Duration`. Passing a duration
directly is a compile error, but a common mistake is computing the wrong
absolute time (e.g. `time.Unix(0, timeout.Nanoseconds())` instead of
`time.Now().Add(timeout)`).

Calling `SetDeadline` when only the read direction needs a timeout is wasteful:
a slow write will also be killed. Use `SetReadDeadline` and `SetWriteDeadline`
independently.

Not checking `IsTimeout` before deciding to close a connection means a transient
OS error (e.g. `EINTR`) is treated as a timeout. Always branch on the error kind.

## Exercises

This is a library with a `cmd/demo` program. Verify with `go test`, not `go run`.

### Exercise 1: Core Types and IsTimeout

Create `deadline.go`:

```go
// deadline.go
package deadline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrBind is returned when the listener cannot bind to the given address.
var ErrBind = errors.New("deadline: cannot bind listener")

// Server is a TCP server that enforces a per-connection idle timeout.
// When no data arrives for IdleTimeout the connection is closed.
type Server struct {
	ln          net.Listener
	idleTimeout time.Duration
}

// New creates a Server listening on addr with the given per-connection idle
// timeout. Pass idleTimeout = 0 to disable idle timeouts.
func New(addr string, idleTimeout time.Duration) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBind, err)
	}
	return &Server{ln: ln, idleTimeout: idleTimeout}, nil
}

// Addr returns the listener address assigned by the OS.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// Close stops the server by closing the listener.
func (s *Server) Close() error {
	return s.ln.Close()
}

// Serve accepts connections and calls handler in a new goroutine for each one.
// When an idle timeout is configured the connection is wrapped in an idleConn
// that resets the read and write deadlines on every successful I/O call.
// Serve returns when the listener is closed.
func (s *Server) Serve(handler func(conn net.Conn)) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			if s.idleTimeout > 0 {
				c = &idleConn{Conn: c, timeout: s.idleTimeout}
			}
			handler(c)
		}(conn)
	}
}

// idleConn wraps net.Conn and enforces a sliding-window idle timeout.
// The read (and write) deadline is refreshed before every call so that the
// timeout window starts from the last activity, not from connection open.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// IsTimeout reports whether err is a network timeout error.
// It uses errors.As so it works correctly with wrapped errors from net.
func IsTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Dial connects to addr within dialTimeout using net.DialTimeout.
// Use DialContext when cancellation is also required.
func Dial(addr string, dialTimeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, dialTimeout)
}

// DialContext connects to addr using a context-aware dialer.
// The returned connection carries no deadlines; the caller sets them.
func DialContext(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
```

The `idleConn` struct embeds `net.Conn` and overrides only `Read` and `Write`.
All other methods (`Close`, `LocalAddr`, `RemoteAddr`, `SetDeadline`) are
forwarded to the underlying connection automatically by embedding.

### Exercise 2: Tests

Create `deadline_test.go`:

```go
// deadline_test.go
package deadline

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestIsTimeoutTrueForExpiredDeadline(t *testing.T) {
	t.Parallel()

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	// A deadline in the past causes the next Read to return immediately.
	if err := a.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := a.Read(make([]byte, 1))
	if !IsTimeout(err) {
		t.Fatalf("IsTimeout = false, want true; err = %v", err)
	}
}

func TestIsTimeoutFalseForClosedPipe(t *testing.T) {
	t.Parallel()

	a, b := net.Pipe()
	b.Close() // EOF from the peer is not a timeout.
	_, err := a.Read(make([]byte, 1))
	a.Close()
	if IsTimeout(err) {
		t.Fatalf("IsTimeout = true for closed-pipe error %v, want false", err)
	}
}

func TestServerIdleTimeout(t *testing.T) {
	t.Parallel()

	const idleTimeout = 100 * time.Millisecond

	srv, err := New("127.0.0.1:0", idleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	go srv.Serve(func(conn net.Conn) {
		defer conn.Close()
		io.Copy(conn, conn) // echo: each Read resets the deadline via idleConn
	})

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Verify the echo path is alive.
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want %q", buf, "ping")
	}

	// Go silent. The server must close the connection after idleTimeout.
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected server to close connection after idle timeout; got nil error")
	}
}

func TestDialContextCancellation(t *testing.T) {
	t.Parallel()

	// Bind a listener but never call Accept so the port is open but unresponsive.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before dialling

	_, err = DialContext(ctx, ln.Addr().String())
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

// ExampleIsTimeout demonstrates that a deadline set in the past triggers an
// immediate timeout error on the next Read.
func ExampleIsTimeout() {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	_ = a.SetReadDeadline(time.Now().Add(-time.Second))
	_, err := a.Read(make([]byte, 1))
	fmt.Println(IsTimeout(err))
	// Output: true
}
```

Your turn: add `TestWriteDeadlineExpires` that uses `net.Pipe()`, fills the
pipe's kernel buffer by writing without a reader, and confirms that
`IsTimeout(err)` returns true once the write deadline expires.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"example.com/deadline"
)

func main() {
	// Start an echo server with a 300ms idle timeout.
	srv, err := deadline.New("127.0.0.1:0", 300*time.Millisecond)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	go srv.Serve(func(conn net.Conn) {
		defer conn.Close()
		io.Copy(conn, conn)
	})

	addr := srv.Addr().String()
	fmt.Printf("server on %s with 300ms idle timeout\n", addr)

	conn, err := deadline.Dial(addr, 2*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("echoed: %s\n", buf)

	// Wait past the idle timeout.
	time.Sleep(500 * time.Millisecond)

	_, err = conn.Read(buf)
	if err != nil {
		if deadline.IsTimeout(err) {
			fmt.Println("read timed out (client-side deadline)")
		} else {
			fmt.Println("server closed connection after idle timeout")
		}
	}
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Passing a Duration to SetDeadline

Wrong:

```go
conn.SetReadDeadline(5 * time.Second)
```

This is a compile error — `SetReadDeadline` takes `time.Time`, not
`time.Duration`. What the programmer intended is the fix:

```go
conn.SetReadDeadline(time.Now().Add(5 * time.Second))
```

### Setting a Single Deadline and Never Updating It

Wrong:

```go
conn.SetReadDeadline(time.Now().Add(30 * time.Second))
// ... handle connection for minutes ...
```

The deadline fires 30 seconds after the connection opens, not 30 seconds after
the last message. An active client that sends every 20 seconds is killed at 30
seconds.

Fix: reset the deadline before each read (the sliding-window pattern shown in
`idleConn.Read`).

### Type-Asserting Instead of errors.As

Wrong:

```go
if netErr, ok := err.(net.Error); ok && netErr.Timeout() { ... }
```

This misses errors wrapped with `fmt.Errorf("... %w", err)`.

Fix:

```go
var netErr net.Error
if errors.As(err, &netErr) && netErr.Timeout() { ... }
```

### Treating EOF as a Timeout

Wrong:

```go
_, err := conn.Read(buf)
if err != nil {
    log.Printf("idle timeout: %v", err)
    return
}
```

`io.EOF` (normal peer close) is logged as "idle timeout".

Fix: branch on error kind before deciding the cause.

```go
_, err := conn.Read(buf)
if errors.Is(err, io.EOF) {
    return // clean close
}
if IsTimeout(err) {
    log.Printf("idle timeout: %v", conn.RemoteAddr())
    return
}
if err != nil {
    log.Printf("read error: %v", err)
    return
}
```

### Using SetDeadline When Only One Direction Needs a Limit

Wrong: `conn.SetDeadline(time.Now().Add(d))` kills both reads and writes. A
large response payload in flight will be cut off because the write deadline also
fires.

Fix: use `SetReadDeadline` and `SetWriteDeadline` independently so a slow write
does not cancel a fast read and vice versa.

## Verification

From `~/go-exercises/deadline`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification — there is no program to
eyeball. `gofmt -l .` must produce no output (empty means formatted correctly).

## Summary

- `SetReadDeadline` / `SetWriteDeadline` take an absolute `time.Time`; compute
  it with `time.Now().Add(d)`.
- A zero `time.Time{}` clears the deadline; non-zero past times trigger an
  immediate timeout on the next I/O.
- The sliding-window idle timeout resets the deadline before every `Read`,
  giving the client `idleTimeout` from the last activity.
- Wrap `net.Conn` in a struct that embeds the interface and overrides only
  `Read` and `Write`; all other methods are forwarded by embedding.
- Detect timeouts with `errors.As` + `net.Error.Timeout()`, not bare type
  assertions; this handles wrapped errors correctly.
- Dial timeout and operation deadlines are independent: a successful dial has
  no operation deadlines unless the code sets them explicitly.

## What's Next

Next: [TCP Keep-Alive](../05-tcp-keep-alive/05-tcp-keep-alive.md).

## Resources

- [net.Conn — SetDeadline, SetReadDeadline, SetWriteDeadline](https://pkg.go.dev/net#Conn)
- [net.Dialer — Timeout, DialContext](https://pkg.go.dev/net#Dialer)
- [os.ErrDeadlineExceeded](https://pkg.go.dev/os#ErrDeadlineExceeded)
- [Go Blog: Timeouts and Deadlines](https://go.dev/blog/deadlines)
- [net.Error — Timeout method](https://pkg.go.dev/net#Error)
