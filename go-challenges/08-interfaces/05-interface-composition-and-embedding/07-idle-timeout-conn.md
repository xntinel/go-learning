# Exercise 7: Adding Per-Operation Deadlines by Embedding net.Conn

An infrastructure wrapper: a struct that embeds `net.Conn` and, before every
`Read`/`Write`, sets an idle deadline so a stalled peer cannot pin a goroutine
forever. Every other `net.Conn` method — `LocalAddr`, `Close`, and the rest —
stays promoted. This is delegation-plus-override applied to a real transport
concern.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
idleconn/                   independent module: example.com/idleconn
  go.mod                    go 1.26
  idleconn.go               type Conn (embeds net.Conn); New, Read, Write set idle deadlines
  cmd/
    demo/
      main.go               a timely read succeeds; a silent peer makes the next read time out
  idleconn_test.go          idle timeout, timely read, promoted addr delegates, net.Conn assertion
```

- Files: `idleconn.go`, `cmd/demo/main.go`, `idleconn_test.go`.
- Implement: a `Conn` embedding `net.Conn` whose `Read`/`Write` call `SetReadDeadline`/`SetWriteDeadline(now+idle)` before delegating; everything else promoted.
- Test: with `net.Pipe`, a silent peer makes `Read` return `os.ErrDeadlineExceeded`; a timely write/read succeeds; promoted `LocalAddr` delegates to the embedded conn; a static assertion the wrapper still satisfies `net.Conn`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/05-interface-composition-and-embedding/07-idle-timeout-conn/cmd/demo
cd go-solutions/08-interfaces/05-interface-composition-and-embedding/07-idle-timeout-conn
```

### Why embedding is exactly right here

`net.Conn` has eight methods (`Read`, `Write`, `Close`, `LocalAddr`, `RemoteAddr`,
and three `SetDeadline` variants). You want to change the behavior of two of them
and leave six untouched. Embedding the interface promotes all eight; you then
shadow `Read` and `Write` to install a fresh deadline of `now + idle` before each
operation and delegate the rest to the embedded conn. `LocalAddr`, `Close`, and the
deadline setters remain promoted, so the wrapper is a drop-in `net.Conn` that any
transport code accepts.

The deadline is *per operation*, not a single connection-wide timeout: setting it
before each `Read` means the clock restarts on every successful read, so a
connection that keeps making progress never times out, while one that stalls
mid-stream is unblocked at `idle` after its last byte. That is the "idle timeout"
semantics load balancers and proxies use. A stalled `Read` returns an error that
satisfies `errors.Is(err, os.ErrDeadlineExceeded)` — the deadline error the `net`
package documents — which is how the caller distinguishes an idle timeout from a
real connection failure. `net.Pipe` honors deadlines, so the whole behavior is
testable in memory with no sockets.

Create `idleconn.go`:

```go
package idleconn

import (
	"net"
	"time"
)

// Conn wraps a net.Conn and applies an idle timeout to every Read and Write:
// before each operation it sets a deadline of now+idle, so a stalled peer cannot
// pin the goroutine forever. All other net.Conn methods stay promoted.
type Conn struct {
	net.Conn
	idle time.Duration
}

// New wraps c so each Read and Write must make progress within idle.
func New(c net.Conn, idle time.Duration) *Conn {
	return &Conn{Conn: c, idle: idle}
}

// Read refreshes the read deadline, then delegates.
func (c *Conn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

// Write refreshes the write deadline, then delegates.
func (c *Conn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// Static assertion: the wrapper is still a net.Conn.
var _ net.Conn = (*Conn)(nil)
```

### The runnable demo

`net.Pipe` returns a connected, in-memory, synchronous pair. The demo reads a
message the peer sends, then reads again while the peer stays silent, so the idle
deadline fires instead of blocking forever.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"example.com/idleconn"
)

func main() {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() { c2.Write([]byte("ping")) }()

	conn := idleconn.New(c1, 200*time.Millisecond)
	buf := make([]byte, 8)
	n, _ := conn.Read(buf)
	fmt.Printf("read %q\n", buf[:n])

	// The peer goes silent: the next read times out instead of blocking forever.
	_, err := conn.Read(buf)
	fmt.Println("idle read timed out:", errors.Is(err, os.ErrDeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read "ping"
idle read timed out: true
```

### Tests

Create `idleconn_test.go`:

```go
package idleconn

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestReadTimesOutOnIdlePeer(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	conn := New(c1, 20*time.Millisecond)
	buf := make([]byte, 8)
	_, err := conn.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read err = %v, want os.ErrDeadlineExceeded", err)
	}
}

func TestTimelyReadSucceeds(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() { c2.Write([]byte("ping")) }()

	conn := New(c1, time.Second)
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("read %q, want ping", buf[:n])
	}
}

func TestPromotedAddrDelegates(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	conn := New(c1, time.Second)
	if conn.LocalAddr() != c1.LocalAddr() {
		t.Fatal("LocalAddr did not delegate to the embedded conn")
	}
	if conn.RemoteAddr() != c1.RemoteAddr() {
		t.Fatal("RemoteAddr did not delegate to the embedded conn")
	}
}

func TestSatisfiesNetConn(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	var _ net.Conn = New(c1, time.Second)
}
```

## Review

The wrapper is correct when it changes exactly two behaviors and delegates
everything else: `TestReadTimesOutOnIdlePeer` proves the deadline is installed (a
silent peer produces `os.ErrDeadlineExceeded` rather than a hang),
`TestTimelyReadSucceeds` proves a live exchange still works and the deadline is
refreshed, and `TestPromotedAddrDelegates` proves the un-overridden methods reach
the embedded conn. The common mistake is setting the deadline once in the
constructor instead of before each operation — that yields a whole-connection
timeout, not an idle timeout, and a long-lived healthy connection dies at a fixed
wall-clock time. Because `net.Pipe` honors deadlines, none of this needs a real
socket.

## Resources

- [`net.Conn`](https://pkg.go.dev/net#Conn) — the eight-method interface being embedded, including the deadline setters.
- [`net.Pipe`](https://pkg.go.dev/net#Pipe) — the in-memory connected pair used in the tests.
- [`os.ErrDeadlineExceeded`](https://pkg.go.dev/os#pkg-variables) — the sentinel a timed-out I/O operation wraps.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-segregated-repository-composition.md](06-segregated-repository-composition.md) | Next: [08-audit-fanout-multiwriter.md](08-audit-fanout-multiwriter.md)
