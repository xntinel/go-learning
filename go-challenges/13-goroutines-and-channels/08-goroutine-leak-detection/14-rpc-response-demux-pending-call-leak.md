# Exercise 14: A Connection Multiplexer That Wakes Every Pending Call

**Level: Advanced**

An RPC client multiplexes many in-flight requests over a single connection,
matching replies to callers by correlation id while one reader goroutine
dispatches each frame to the waiting caller. The naive version leaks in two
subtle ways: when the connection dies the reader exits and every pending caller
blocks forever on a reply channel no one will ever fill, and a caller whose
context is cancelled leaves its slot in the pending map so the map grows without
bound and the reader eventually sends into an abandoned channel. This exercise
builds the mux so that no pending call and no goroutine ever leaks, even when the
connection dies mid-flight, using exactly one reader goroutine no matter how many
calls are outstanding.

This module is self-contained: its own module, a `rpcmux` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
rpcmux/                      independent module: example.com/rpcmux
  go.mod                     go 1.26
  rpcmux.go                  New, (*Mux).Call, (*Mux).Close, (*Mux).PendingLen, ErrClosed
  cmd/demo/main.go           runnable demo: sequential calls over an echo conn, idempotent close
  rpcmux_test.go             concurrent replies, mid-flight death, ctx-cancel deregister, idempotent close
```

- Files: `rpcmux.go`, `cmd/demo/main.go`, `rpcmux_test.go`.
- Implement: `New(conn Conn) *Mux`, `(*Mux).Call(ctx, payload) ([]byte, error)`, `(*Mux).Close() error`, `(*Mux).PendingLen() int`, and `ErrClosed`.
- Test: 100 concurrent calls each get their own reply under exactly one reader; a dead connection wakes every pending call with `ErrClosed`; a cancelled call deregisters and a late frame for its id is dropped cleanly; `Close` wakes all pending and is idempotent; no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak
go mod tidy
```

### The two leaks hiding in a reply demultiplexer

A multiplexer keeps a map from correlation id to a reply channel. `Call`
allocates an id, registers a channel under that id, sends the request, and waits.
The single reader loops on `Recv`, looks up the channel for each frame's id, and
hands the frame over. It sounds simple, and the happy path is. The leaks live in
the two ways a call can end without a reply.

First, the connection dies. `Recv` returns an error, the reader returns, and now
nobody will ever read the connection again. Every caller still parked on its
reply channel is stranded: the exit path they were counting on (a frame arriving)
has become unreachable. The fix is a `readerDone` channel the reader closes on its
way out, and a `case <-readerDone` in every caller's `select`. Closing a channel
is a broadcast, so one `close` wakes all N waiters at once, each returning
`ErrClosed`. This is the canonical "receive from a channel that will never be sent
to" leak, and closing `readerDone` is the guaranteed exit path that neutralizes
it.

Second, a caller's context is cancelled before its reply arrives. The caller must
return `ctx.Err()`, but if it just returns it leaves its channel registered in the
map. Two bad things follow: the map grows by one entry per cancelled call (a slow
memory leak with an unbounded map), and later, if a frame for that id does arrive,
the reader finds a live channel and sends into it. If that channel is unbuffered
and its owner has gone, the reader blocks forever on the send and the whole
dispatch loop wedges, stranding every other caller too. So the cancel path must
`deregister` the id, and the reply channel must be buffered with capacity one so
that a delivery racing the cancel can always complete without blocking the reader.

The protocol that makes both safe:

1. `Call` atomically allocates a unique id and a `make(chan Frame, 1)` reply
   channel, registers it under the mutex, checks the mux is not closed, and sends.
2. The reader, for each received frame, takes the mutex, looks up the id, and
   delivers only if it is still registered, deleting the entry first so the slot
   is used at most once. Because the channel is buffered(1) and used once, this
   send under the lock can never block, so the reader stays live for the next
   frame.
3. `Call` selects over three cases: its reply arrived, its context is done (then
   deregister), or the reader is done (then `ErrClosed`). Every branch is a
   guaranteed exit.
4. `Close` marks the mux closed, unblocks the reader's `Recv` by closing the
   underlying connection if it is an `io.Closer`, and joins on `readerDone`. It is
   wrapped in `sync.Once` so it is idempotent.

The ownership rule is: the reader owns deletion-on-delivery, and the caller owns
deletion-on-abandonment. Deleting a missing key is a no-op, so the two can race
freely. Buffered(1) plus delete-before-send is what lets the reader deliver under
the lock without ever risking a blocking send into a slot whose owner has left.

Create `rpcmux.go`:

```go
// Package rpcmux multiplexes many in-flight RPC calls over a single connection.
// One reader goroutine dispatches each reply frame to the waiting caller by
// correlation id. Every pending call has a guaranteed exit path: its reply, its
// own context cancellation, or the connection dying.
package rpcmux

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned to every caller whose call was still pending when the
// connection died or Close was invoked.
var ErrClosed = errors.New("rpcmux: connection closed")

// Frame is one wire message. ID is the correlation id that pairs a request with
// its reply.
type Frame struct {
	ID      uint64
	Payload []byte
}

// Conn is the single underlying connection. Recv returns a non-nil error when
// the connection closes, which is the reader's exit signal. A Conn that also
// implements io.Closer lets Close unblock a Recv parked on a dead socket.
type Conn interface {
	Send(Frame) error
	Recv() (Frame, error)
}

// Mux fans replies from one Conn out to many concurrent callers.
type Mux struct {
	conn Conn

	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan Frame
	closed  bool

	readerDone chan struct{}
	closeOnce  sync.Once
}

// New starts the single reader goroutine and returns a ready Mux.
func New(conn Conn) *Mux {
	m := &Mux{
		conn:       conn,
		pending:    make(map[uint64]chan Frame),
		readerDone: make(chan struct{}),
	}
	go m.read()
	return m
}

// read is the one and only reader goroutine, regardless of how many calls are
// outstanding. It exits exactly when Recv reports the connection is gone, and
// on exit it closes readerDone so every parked caller is woken with ErrClosed.
func (m *Mux) read() {
	defer close(m.readerDone)
	for {
		frame, err := m.conn.Recv()
		if err != nil {
			m.mu.Lock()
			m.closed = true
			clear(m.pending)
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		reply, ok := m.pending[frame.ID]
		if ok {
			// Deliver only to a still-registered slot, and delete first so the
			// slot is used at most once. reply is buffered(1), so this send
			// under the lock can never block: the reader stays live for the
			// next frame.
			delete(m.pending, frame.ID)
			reply <- frame
		}
		// Unknown or already-deregistered id: drop it. No blocking send into an
		// abandoned channel, no panic.
		m.mu.Unlock()
	}
}

// Call sends payload and waits for the matching reply. It returns the reply
// payload, or ctx.Err() if the caller's context is cancelled first, or
// ErrClosed if the connection dies while the call is pending.
func (m *Mux) Call(ctx context.Context, payload []byte) ([]byte, error) {
	id := m.nextID.Add(1)
	reply := make(chan Frame, 1)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	m.pending[id] = reply
	m.mu.Unlock()

	if err := m.conn.Send(Frame{ID: id, Payload: payload}); err != nil {
		m.deregister(id)
		return nil, err
	}

	select {
	case f := <-reply:
		return f.Payload, nil
	case <-ctx.Done():
		// Deregister so the map does not grow and the reader never sends into
		// this abandoned slot.
		m.deregister(id)
		return nil, ctx.Err()
	case <-m.readerDone:
		m.deregister(id)
		return nil, ErrClosed
	}
}

// deregister removes a pending slot. Deleting a missing key is a no-op, so it is
// safe to race the reader (which may have already delivered and deleted).
func (m *Mux) deregister(id uint64) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
}

// Close stops the reader, causing every pending call to fail with ErrClosed,
// then joins the reader goroutine. It is idempotent.
func (m *Mux) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		// Unblock a Recv parked on the connection so the reader can exit.
		if c, ok := m.conn.(io.Closer); ok {
			_ = c.Close()
		}
	})
	<-m.readerDone
	return nil
}

// PendingLen reports how many calls are currently registered. Test-only
// observability: a correct Mux drains this to zero once every call has returned.
func (m *Mux) PendingLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}
```

### The runnable demo

The demo drives three sequential calls over an in-memory echo connection so the
output order is deterministic, shows the pending map draining to zero, calls
`Close` twice to prove idempotency, and shows a post-close call failing fast.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/rpcmux"
)

// echoConn is an in-memory Conn that replies to every request by prefixing
// "reply-" to its payload, keeping the correlation id.
type echoConn struct {
	ch     chan rpcmux.Frame
	closed chan struct{}
	once   sync.Once
}

func newEchoConn() *echoConn {
	return &echoConn{ch: make(chan rpcmux.Frame, 64), closed: make(chan struct{})}
}

func (c *echoConn) Send(f rpcmux.Frame) error {
	select {
	case <-c.closed:
		return fmt.Errorf("send on closed conn")
	case c.ch <- f:
		return nil
	}
}

func (c *echoConn) Recv() (rpcmux.Frame, error) {
	select {
	case <-c.closed:
		return rpcmux.Frame{}, fmt.Errorf("conn closed")
	case f := <-c.ch:
		return rpcmux.Frame{ID: f.ID, Payload: append([]byte("reply-"), f.Payload...)}, nil
	}
}

func (c *echoConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func main() {
	m := rpcmux.New(newEchoConn())

	// Three sequential calls: deterministic order and replies.
	for i := range 3 {
		req := fmt.Sprintf("ping-%d", i+1)
		reply, err := m.Call(context.Background(), []byte(req))
		if err != nil {
			fmt.Println("call error:", err)
			return
		}
		fmt.Printf("call %s -> %s\n", req, reply)
	}
	fmt.Println("pending after calls:", m.PendingLen())

	// Close is idempotent: two calls, one clean join.
	_ = m.Close()
	_ = m.Close()
	fmt.Println("closed twice: ok")

	// A call after Close fails fast with ErrClosed.
	_, err := m.Call(context.Background(), []byte("late"))
	fmt.Println("call after close:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call ping-1 -> reply-ping-1
call ping-2 -> reply-ping-2
call ping-3 -> reply-ping-3
pending after calls: 0
closed twice: ok
call after close: rpcmux: connection closed
```

### Tests

`TestMain` installs `goleak.VerifyTestMain(m)`, which fails the package if any
goroutine outlives the tests, so every mux's reader must have exited.
`TestConcurrentCallsGetCorrectReplies` fires 100 concurrent `Call`s over one echo
connection, checks each caller got the reply keyed to its own id, asserts exactly
one goroutine is parked in `(*Mux).read` via a pprof dump, and confirms the
pending map drained to zero. `TestConnectionDiesMidFlight` parks 50 calls on a
connection whose `Recv` blocks until `kill()`, severs it, and asserts every caller
returns `ErrClosed` and the map drains. `TestContextCancelDeregisters` cancels one
call, verifies it returns `context.Canceled` and its slot is gone, then delivers a
late frame for that id and confirms the reader drops it without blocking or
panicking. `TestCloseWakesPendingAndIsIdempotent` parks 20 calls on a silent
connection, calls `Close` twice, and asserts all pending calls and a subsequent
call return `ErrClosed`.

Create `rpcmux_test.go`:

```go
package rpcmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain runs every test, then verifies no goroutine leaked out of the whole
// package. The single reader goroutine of every Mux must have exited via Close
// or a dead connection.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var errConnDead = errors.New("conn dead")

// echoConn replies to each request by prefixing "reply-" to its payload.
type echoConn struct {
	ch     chan Frame
	closed chan struct{}
	once   sync.Once
}

func newEchoConn() *echoConn {
	return &echoConn{ch: make(chan Frame, 256), closed: make(chan struct{})}
}

func (c *echoConn) Send(f Frame) error {
	select {
	case <-c.closed:
		return errConnDead
	case c.ch <- f:
		return nil
	}
}

func (c *echoConn) Recv() (Frame, error) {
	select {
	case <-c.closed:
		return Frame{}, errConnDead
	case f := <-c.ch:
		return Frame{ID: f.ID, Payload: append([]byte("reply-"), f.Payload...)}, nil
	}
}

func (c *echoConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// dyingConn accepts sends but never replies until kill() is called, after which
// every Recv returns an error. It models a connection that dies mid-flight.
type dyingConn struct {
	die  chan struct{}
	once sync.Once
}

func newDyingConn() *dyingConn { return &dyingConn{die: make(chan struct{})} }

func (c *dyingConn) Send(Frame) error { return nil }

func (c *dyingConn) Recv() (Frame, error) {
	<-c.die
	return Frame{}, errConnDead
}

func (c *dyingConn) kill()        { c.once.Do(func() { close(c.die) }) }
func (c *dyingConn) Close() error { c.kill(); return nil }

// manualConn swallows sends and only delivers frames the test hands it via
// deliver(). deliver blocks until the reader consumes the frame, so a test can
// synchronize on a "late" frame being processed.
type manualConn struct {
	in   chan Frame
	die  chan struct{}
	once sync.Once
}

func newManualConn() *manualConn {
	return &manualConn{in: make(chan Frame), die: make(chan struct{})}
}

func (c *manualConn) Send(Frame) error { return nil }

func (c *manualConn) Recv() (Frame, error) {
	select {
	case f := <-c.in:
		return f, nil
	case <-c.die:
		return Frame{}, errConnDead
	}
}

func (c *manualConn) deliver(f Frame) { c.in <- f }
func (c *manualConn) Close() error    { c.once.Do(func() { close(c.die) }); return nil }

// countReaders returns how many goroutines are parked inside the Mux reader.
func countReaders() int {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	return strings.Count(buf.String(), "rpcmux.(*Mux).read")
}

// waitPending polls PendingLen down to want within a generous deadline.
func waitPending(m *Mux, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.PendingLen() == want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestConcurrentCallsGetCorrectReplies fires 100 concurrent calls over one
// connection and checks each caller received the reply for its own id, that a
// single reader served them all, and the pending map drained to zero.
func TestConcurrentCallsGetCorrectReplies(t *testing.T) {
	m := New(newEchoConn())
	defer m.Close()

	const n = 100
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req := fmt.Sprintf("req-%d", i)
			reply, err := m.Call(context.Background(), []byte(req))
			if err != nil {
				t.Errorf("call %d: unexpected error %v", i, err)
				return
			}
			got[i] = string(reply)
		})
	}
	wg.Wait()

	for i := range n {
		want := fmt.Sprintf("reply-req-%d", i)
		if got[i] != want {
			t.Fatalf("call %d: got %q, want %q", i, got[i], want)
		}
	}
	if r := countReaders(); r != 1 {
		t.Fatalf("expected exactly one reader goroutine, found %d", r)
	}
	if !waitPending(m, 0) {
		t.Fatalf("pending did not drain to 0, still %d", m.PendingLen())
	}
}

// TestConnectionDiesMidFlight registers many pending calls, kills the
// connection, and asserts every caller is woken with ErrClosed and the pending
// map drains.
func TestConnectionDiesMidFlight(t *testing.T) {
	conn := newDyingConn()
	m := New(conn)
	defer m.Close()

	const n = 50
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = m.Call(context.Background(), []byte("x"))
		})
	}
	// Let the calls register, then sever the connection.
	if !waitPending(m, n) {
		t.Fatalf("expected %d pending calls, saw %d", n, m.PendingLen())
	}
	conn.kill()
	wg.Wait()

	for i := range n {
		if !errors.Is(errs[i], ErrClosed) {
			t.Fatalf("call %d: got %v, want ErrClosed", i, errs[i])
		}
	}
	if !waitPending(m, 0) {
		t.Fatalf("pending did not drain to 0, still %d", m.PendingLen())
	}
}

// TestContextCancelDeregisters cancels one call's context, then delivers a late
// frame for that same id. The call must return ctx.Err(), the pending slot must
// be gone, and the late frame must neither block the reader nor panic.
func TestContextCancelDeregisters(t *testing.T) {
	conn := newManualConn()
	m := New(conn)
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var (
		callErr error
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		_, callErr = m.Call(ctx, []byte("q"))
	}()

	if !waitPending(m, 1) {
		t.Fatalf("call did not register, pending=%d", m.PendingLen())
	}
	cancel()
	<-done

	if !errors.Is(callErr, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", callErr)
	}
	if !waitPending(m, 0) {
		t.Fatalf("cancelled call left its slot registered, pending=%d", m.PendingLen())
	}

	// The first (and only) call used id 1. A late reply for it must be dropped
	// cleanly. deliver blocks until the reader has consumed the frame.
	conn.deliver(Frame{ID: 1, Payload: []byte("late")})
	if m.PendingLen() != 0 {
		t.Fatalf("late frame changed pending to %d", m.PendingLen())
	}
}

// TestCloseWakesPendingAndIsIdempotent parks calls on a silent connection,
// closes the mux, and asserts every pending call returns ErrClosed. Close is
// safe to call repeatedly.
func TestCloseWakesPendingAndIsIdempotent(t *testing.T) {
	m := New(newManualConn())

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = m.Call(context.Background(), []byte("y"))
		})
	}
	if !waitPending(m, n) {
		t.Fatalf("expected %d pending, saw %d", n, m.PendingLen())
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	wg.Wait()

	for i := range n {
		if !errors.Is(errs[i], ErrClosed) {
			t.Fatalf("call %d: got %v, want ErrClosed", i, errs[i])
		}
	}
	if _, err := m.Call(context.Background(), []byte("z")); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: got %v, want ErrClosed", err)
	}
}
```

## Review

Correct here means every `Call` returns exactly once, through its reply, its
context, or `ErrClosed`, and the reader goroutine exits whenever the connection
does. Three invariants guarantee it: `readerDone` is closed on the reader's only
exit, so a dead connection is a broadcast that wakes all N parked callers at once;
the reply channel is buffered(1) and the reader deletes the entry before
delivering, so a delivery that races a context cancellation can always complete
without the reader ever blocking on a send; and every non-reply exit path
`deregister`s the id, so the pending map never grows past the live-call count. The
tests prove each edge: `goleak.VerifyTestMain` fails on any surviving reader, the
mid-flight-death and idempotent-close tests assert `ErrClosed` for every waiter,
and the cancel test delivers a late frame for a deregistered id to show the reader
drops it rather than blocking. The production bug this pattern prevents is the
classic multiplexer stall: one cancelled or timed-out caller leaves a live
channel in the map, a delayed reply for that id makes the sole reader block on an
unbuffered send, and the entire client wedges with every future call hanging and
its goroutines leaking until the process is restarted.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector; `VerifyTestMain` is the parallel-safe package-level check used here.
- [Go Concurrency Patterns: Context](https://go.dev/blog/context) -- why a cancelled context obliges the callee to stop and clean up, not just return.
- [`sync.Once`](https://pkg.go.dev/sync#Once) -- the primitive that makes `Close` idempotent and joins the reader exactly once.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- `atomic.Uint64` for allocating collision-free correlation ids without holding the mutex.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-request-coalescing-cancel-safe-no-leak.md](13-request-coalescing-cancel-safe-no-leak.md) | Next: [../09-channel-of-channels/00-concepts.md](../09-channel-of-channels/00-concepts.md)
