# Exercise 13: Correlation-ID Response Demultiplexer (Multiplexed RPC Client)

**Level: Advanced**

A client library that multiplexes many in-flight RPCs over a single connection
cannot assume responses come back in request order: a slow query and a fast one
share the same socket, and the fast one's reply often arrives first. The naive
"send, then read the next frame and treat it as my reply" breaks the moment two
calls are in flight. This exercise builds the fix: stamp every request with a
unique correlation ID, register a private reply channel with a router actor, and
have one reader goroutine route each inbound frame to the reply channel for its
ID, so out-of-order responses still reach the right caller.

This module is self-contained: its own module, a `muxclient` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
muxclient/                   independent module: example.com/muxclient
  go.mod                     go 1.26
  muxclient.go               Client over a Transport: New, Run, Call, Close, ErrClosed
  cmd/demo/main.go           runnable demo: concurrent Calls with reversed delivery
  muxclient_test.go          match-by-ID, unique IDs, cancel-drops-late-frame, close semantics
```

- Files: `muxclient.go`, `cmd/demo/main.go`, `muxclient_test.go`.
- Implement: `type Frame struct{ ID uint64; Payload int; Err error }`; `type Transport interface{ Send(Frame) error; Recv() (Frame, error) }`; `type Client`; `func New(t Transport) *Client`; `func (c *Client) Run()`; `func (c *Client) Call(ctx context.Context, payload int) (int, error)`; `func (c *Client) Close(cause error)`; `var ErrClosed error`.
- Test: concurrent Calls each get the response for their payload under reversed delivery; IDs unique; a cancelled Call deregisters its ID so a late frame is dropped; `Close(cause)` fails in-flight Calls with cause and rejects new ones with `ErrClosed`; goroutines exit on Close.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/09-channel-of-channels/13-correlation-id-response-demux/cmd/demo
cd go-solutions/13-goroutines-and-channels/09-channel-of-channels/13-correlation-id-response-demux
go get go.uber.org/goleak
go mod tidy
```

### One router actor owns the correlation table

The whole design turns on a single ownership rule: exactly one goroutine — the
router loop — reads and writes `nextID` and the `pending` map that maps a
correlation ID to its caller's reply channel. Because only that goroutine touches
the state, there is no mutex on the request path, and `go test -race` proves it.
Everything a caller or the reader wants to do becomes a request sent onto one of
the loop's channels:

1. `Call` registers by sending a `registration{reply, idOut}` — a
   channel-of-channels request. The caller creates its own `reply` channel
   buffered to capacity one and hands it to the router; the router allocates the
   next ID, stores `pending[id] = reply`, and sends the ID back on `idOut`. The
   caller never mutates `pending`; it only asks the owner to.
2. `Call` then `Send`s the frame stamped with that ID and blocks in a `select`
   over three outcomes: its reply arrives, its context is cancelled, or the
   client closes.
3. The reader goroutine loops on `Transport.Recv` and forwards every inbound
   frame to the loop. The loop looks up `pending[f.ID]`, deletes the entry, and
   sends the frame on that reply channel. Routing is by ID, so a response that
   arrives out of order still lands in the right caller's channel.

The reply channel is buffered to capacity one for the same reason it always is in
this chapter: if a caller has already walked away (its context fired), the loop's
`ch <- f` must still complete instead of wedging the router forever. One slot is
exactly enough because there is at most one response per ID, and the entry is
deleted the instant it is routed.

The subtle correctness property is the cancellation path. When a `Call`'s context
fires before its frame arrives, the caller must remove its ID from `pending`
before returning — otherwise the entry leaks, and a late frame for that ID would
be delivered to a channel no one reads. `Call` deregisters by sending the ID to
the loop's `deregister` channel; the loop deletes it. A frame that arrives after
deregistration finds no entry and is dropped. IDs are monotonic and never reused,
so a dropped late frame can never be misrouted to a different caller.

Shutdown is a request too. `Close(cause)` sends the cause into the loop, which
fails every still-pending reply with `Frame{Err: cause}` and returns, closing its
`done` channel. In-flight `Call`s observe either the failure frame or the closed
`done` and return the cause. New `Call`s see `done` already closed at registration
time and return `ErrClosed`. `Close` also closes the transport if it is an
`io.Closer`, which is what unblocks the reader parked in `Recv`.

Create `muxclient.go`:

```go
// Package muxclient multiplexes many in-flight RPCs over a single connection.
// Each outbound request is stamped with a unique correlation ID and registers a
// cap-1 reply channel with a router actor; one reader goroutine consumes inbound
// frames and routes each to the reply channel for its ID, so out-of-order
// responses still reach the right caller with no lock anywhere.
package muxclient

import (
	"context"
	"errors"
	"io"
	"sync"
)

// ErrClosed is returned by Call once the client has been closed.
var ErrClosed = errors.New("muxclient: client closed")

// Frame is one unit on the wire. Responses carry the same ID as their request;
// Err is non-nil when the peer or the client reports a failure for that ID.
type Frame struct {
	ID      uint64
	Payload int
	Err     error
}

// Transport is the single connection the client multiplexes over. Send writes a
// frame; Recv blocks until the next inbound frame or an error. A Transport that
// also implements io.Closer is closed when the client shuts down, which is what
// unblocks a Recv parked on an idle connection.
type Transport interface {
	Send(Frame) error
	Recv() (Frame, error)
}

// registration is a channel-of-channels request: the caller hands the router its
// own cap-1 reply channel, and the router hands back the correlation ID it
// allocated for that reply channel.
type registration struct {
	reply chan Frame
	idOut chan uint64
}

// Client is a multiplexing RPC client. A single router goroutine owns nextID and
// the pending map; a single reader goroutine feeds it inbound frames. No caller
// touches that state directly, so there is no mutex on the request path.
type Client struct {
	t Transport

	register   chan registration
	deregister chan uint64
	inbound    chan Frame
	probe      chan chan int // introspection: current len(pending), for tests

	closeOnce sync.Once
	closeReq  chan error    // carries the shutdown cause into the router loop
	done      chan struct{} // closed when the router loop has exited

	closeCause error // written by the loop before done closes; read after done
}

// New returns a client bound to t. Start its goroutines with Run.
func New(t Transport) *Client {
	return &Client{
		t:          t,
		register:   make(chan registration),
		deregister: make(chan uint64),
		inbound:    make(chan Frame),
		probe:      make(chan chan int),
		closeReq:   make(chan error, 1),
		done:       make(chan struct{}),
	}
}

// Run starts the router actor and the inbound reader. Both exit on Close (or on a
// transport error, which triggers Close). Call it once.
func (c *Client) Run() {
	go c.loop()
	go c.reader()
}

// loop is the router actor: the only goroutine that reads or writes nextID and
// pending, so those need no lock.
func (c *Client) loop() {
	defer close(c.done)
	var nextID uint64
	pending := make(map[uint64]chan Frame)
	for {
		select {
		case reg := <-c.register:
			id := nextID
			nextID++
			pending[id] = reg.reply
			reg.idOut <- id // idOut is cap-1, so this never blocks the loop
		case id := <-c.deregister:
			delete(pending, id)
		case f := <-c.inbound:
			// Route by ID, not arrival order. A frame whose ID is no longer
			// pending (its caller cancelled and deregistered) is dropped.
			if ch, ok := pending[f.ID]; ok {
				delete(pending, f.ID)
				ch <- f // ch is cap-1 and holds at most one frame per ID
			}
		case rc := <-c.probe:
			rc <- len(pending)
		case cause := <-c.closeReq:
			c.closeCause = cause
			for id, ch := range pending {
				ch <- Frame{ID: id, Err: cause}
				delete(pending, id)
			}
			return
		}
	}
}

// reader turns the blocking Recv into a stream of routed frames. A transport
// error becomes a Close so every in-flight Call fails with that cause.
func (c *Client) reader() {
	for {
		f, err := c.t.Recv()
		if err != nil {
			c.Close(err)
			return
		}
		select {
		case c.inbound <- f:
		case <-c.done:
			return
		}
	}
}

// Call performs one RPC: allocate an ID via the router, register the reply
// channel, Send the frame, then await the reply against the caller's context.
// On cancellation it deregisters the ID so a late frame for it is dropped and
// never reaches another caller.
func (c *Client) Call(ctx context.Context, payload int) (int, error) {
	reply := make(chan Frame, 1)
	idOut := make(chan uint64, 1)

	select {
	case c.register <- registration{reply: reply, idOut: idOut}:
	case <-c.done:
		return 0, ErrClosed
	}

	var id uint64
	select {
	case id = <-idOut:
	case <-c.done:
		return 0, ErrClosed
	}

	if err := c.t.Send(Frame{ID: id, Payload: payload}); err != nil {
		c.deregisterID(id)
		return 0, err
	}

	select {
	case f := <-reply:
		if f.Err != nil {
			return 0, f.Err
		}
		return f.Payload, nil
	case <-ctx.Done():
		c.deregisterID(id)
		return 0, context.Cause(ctx)
	case <-c.done:
		return 0, c.closeCause
	}
}

// deregisterID removes id from the pending map, tolerating a closed loop.
func (c *Client) deregisterID(id uint64) {
	select {
	case c.deregister <- id:
	case <-c.done:
	}
}

// Close shuts the client down with cause. In-flight Calls fail with cause;
// later Calls return ErrClosed. It is idempotent and safe from any goroutine.
func (c *Client) Close(cause error) {
	c.closeOnce.Do(func() {
		if cause == nil {
			cause = ErrClosed
		}
		c.closeReq <- cause // buffered cap-1; the loop drains it and exits
		if cl, ok := c.t.(io.Closer); ok {
			cl.Close() // unblock a reader parked in Recv
		}
	})
}

// pendingLen reports how many replies are registered. It rides the same loop as
// every other state access, so its answer is consistent. Used by tests to prove
// a cancelled Call left no entry behind.
func (c *Client) pendingLen() int {
	rc := make(chan int, 1)
	select {
	case c.probe <- rc:
	case <-c.done:
		return 0
	}
	select {
	case n := <-rc:
		return n
	case <-c.done:
		return 0
	}
}
```

### The runnable demo

The demo fires five concurrent Calls over one connection. The in-memory transport
waits until all five frames are sent, then delivers their responses in reverse
allocation order — the worst case for order-based matching. Every caller still
receives the response for its own payload because routing is by correlation ID.
Output is sorted by request payload so the print order is deterministic regardless
of goroutine scheduling.

Create `cmd/demo/main.go`:

```go
// Command demo shows that many concurrent Calls over one connection each receive
// the response matching their own payload, even when the transport delivers the
// responses in reverse order. Routing is by correlation ID, not arrival order.
package main

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"example.com/muxclient"
)

// reorderTransport is an in-memory connection. It waits until expect frames have
// been sent, then delivers their responses in reverse ID order to prove that the
// client demultiplexes by ID and not by the order frames arrive.
type reorderTransport struct {
	expect int

	mu     sync.Mutex
	sent   []muxclient.Frame
	out    chan muxclient.Frame
	closed chan struct{}
	once   sync.Once
}

func newReorderTransport(expect int) *reorderTransport {
	return &reorderTransport{
		expect: expect,
		out:    make(chan muxclient.Frame, expect),
		closed: make(chan struct{}),
	}
}

func (rt *reorderTransport) Send(f muxclient.Frame) error {
	select {
	case <-rt.closed:
		return fmt.Errorf("transport closed")
	default:
	}
	rt.mu.Lock()
	rt.sent = append(rt.sent, f)
	full := len(rt.sent) == rt.expect
	rt.mu.Unlock()
	if full {
		go rt.release()
	}
	return nil
}

func (rt *reorderTransport) release() {
	rt.mu.Lock()
	frames := slices.Clone(rt.sent)
	rt.mu.Unlock()
	slices.SortFunc(frames, func(a, b muxclient.Frame) int {
		return int(b.ID) - int(a.ID) // descending: reverse of allocation order
	})
	for _, f := range frames {
		select {
		case rt.out <- muxclient.Frame{ID: f.ID, Payload: f.Payload * 10}:
		case <-rt.closed:
			return
		}
	}
}

func (rt *reorderTransport) Recv() (muxclient.Frame, error) {
	select {
	case f := <-rt.out:
		return f, nil
	case <-rt.closed:
		return muxclient.Frame{}, fmt.Errorf("transport closed")
	}
}

func (rt *reorderTransport) Close() error {
	rt.once.Do(func() { close(rt.closed) })
	return nil
}

func main() {
	const n = 5
	t := newReorderTransport(n)
	c := muxclient.New(t)
	c.Run()

	type result struct{ in, out int }
	results := make([]result, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			payload := (i + 1) * 100
			got, err := c.Call(context.Background(), payload)
			if err != nil {
				results[i] = result{payload, -1}
				return
			}
			results[i] = result{payload, got}
		})
	}
	wg.Wait()
	c.Close(muxclient.ErrClosed)

	slices.SortFunc(results, func(a, b result) int { return a.in - b.in })
	for _, r := range results {
		fmt.Printf("request %d -> response %d\n", r.in, r.out)
	}
	fmt.Println("responses arrived reversed; every caller matched by correlation ID")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 100 -> response 1000
request 200 -> response 2000
request 300 -> response 3000
request 400 -> response 4000
request 500 -> response 5000
responses arrived reversed; every caller matched by correlation ID
```

### Tests

`TestConcurrentCallsMatchByID` fires fifty concurrent Calls, waits for all fifty
sends via the transport's buffered `recorded` channel (no sleeps), delivers every
response in reverse ID order, and asserts each caller received the response for
its own payload and that all correlation IDs are distinct.
`TestCancelledCallDropsLateFrame` pins the deregistration invariant: it cancels a
Call before its frame arrives, asserts `pendingLen` is zero, then uses the
transport's FIFO delivery to route a late frame for the cancelled ID *before* a
second caller's response, proving the late frame is dropped rather than misrouted.
`TestCloseFailsInflightAndRejectsNew` asserts an in-flight Call fails with the
close cause and a later Call returns `ErrClosed`. `TestCloseWithoutTransportError`
closes a quiescent client and relies on goleak to prove the reader parked in
`Recv` exits. `TestTransportErrorFailsInflight` pins the reader path: a `Recv`
error is promoted to a Close whose cause reaches the in-flight caller. `TestMain`
wraps every test in `goleak.VerifyTestMain`.

Create `muxclient_test.go`:

```go
package muxclient

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// errTransportClosed is what fakeTransport reports once it is closed.
var errTransportClosed = errors.New("fake transport closed")

// fakeTransport is a controllable in-memory connection. Every Send is recorded on
// a buffered channel so a test can wait for exact send counts without polling;
// deliver pushes a response the reader will pick up. Delivery order is the FIFO
// order of the out channel, which the test uses to sequence frames precisely.
type fakeTransport struct {
	recorded chan Frame
	out      chan Frame
	closed   chan struct{}
	once     sync.Once
}

func newFakeTransport(capacity int) *fakeTransport {
	return &fakeTransport{
		recorded: make(chan Frame, capacity),
		out:      make(chan Frame, capacity),
		closed:   make(chan struct{}),
	}
}

func (f *fakeTransport) Send(fr Frame) error {
	select {
	case <-f.closed:
		return errTransportClosed
	default:
	}
	f.recorded <- fr
	return nil
}

func (f *fakeTransport) Recv() (Frame, error) {
	select {
	case fr := <-f.out:
		return fr, nil
	case <-f.closed:
		return Frame{}, errTransportClosed
	}
}

func (f *fakeTransport) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTransport) deliver(fr Frame) {
	select {
	case f.out <- fr:
	case <-f.closed:
	}
}

// TestConcurrentCallsMatchByID fires many concurrent Calls over one connection,
// delivers every response in reverse allocation order, and asserts each caller
// gets the response for its own payload. It also asserts the correlation IDs are
// all distinct.
func TestConcurrentCallsMatchByID(t *testing.T) {
	const n = 50
	ft := newFakeTransport(n)
	c := New(ft)
	c.Run()
	defer c.Close(ErrClosed)

	got := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			got[i], errs[i] = c.Call(context.Background(), i)
		})
	}

	// Wait for all n sends, then reply in reverse ID order.
	sent := make([]Frame, 0, n)
	for range n {
		sent = append(sent, <-ft.recorded)
	}
	slices.SortFunc(sent, func(a, b Frame) int { return int(b.ID) - int(a.ID) })
	for _, fr := range sent {
		ft.deliver(Frame{ID: fr.ID, Payload: fr.Payload + 1000})
	}

	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Call(%d) error = %v", i, errs[i])
		}
		if want := i + 1000; got[i] != want {
			t.Fatalf("Call(%d) = %d, want %d (frame routed to wrong caller)", i, got[i], want)
		}
	}

	ids := make(map[uint64]bool, n)
	for _, fr := range sent {
		if ids[fr.ID] {
			t.Fatalf("duplicate correlation ID %d", fr.ID)
		}
		ids[fr.ID] = true
	}
	if len(ids) != n {
		t.Fatalf("got %d distinct IDs, want %d", len(ids), n)
	}
}

// TestCancelledCallDropsLateFrame pins the deregistration invariant: a Call
// cancelled before its frame arrives removes its ID from pending, so a later
// frame for that ID is dropped rather than delivered to a different caller, and
// no entry leaks. The fake transport's FIFO out channel guarantees the late
// frame is routed before the second caller's response, making the drop
// observable and deterministic.
func TestCancelledCallDropsLateFrame(t *testing.T) {
	ft := newFakeTransport(4)
	c := New(ft)
	c.Run()
	defer c.Close(ErrClosed)

	ctx, cancel := context.WithCancel(context.Background())
	var callErr error
	done := make(chan struct{})
	go func() {
		_, callErr = c.Call(ctx, 7)
		close(done)
	}()

	first := <-ft.recorded // the cancelled call has now registered ID and sent
	cancel()
	<-done
	if !errors.Is(callErr, context.Canceled) {
		t.Fatalf("cancelled Call error = %v, want context.Canceled", callErr)
	}
	if pl := c.pendingLen(); pl != 0 {
		t.Fatalf("pending = %d after cancel, want 0 (ID not deregistered)", pl)
	}

	// A fresh call gets a new, distinct ID.
	var got int
	var freshErr error
	done2 := make(chan struct{})
	go func() {
		got, freshErr = c.Call(context.Background(), 42)
		close(done2)
	}()
	second := <-ft.recorded
	if second.ID == first.ID {
		t.Fatalf("fresh call reused correlation ID %d", first.ID)
	}

	// Deliver the late frame for the cancelled ID first, then the fresh response.
	// FIFO delivery means the late frame is routed (and dropped) before the fresh
	// response reaches its caller.
	ft.deliver(Frame{ID: first.ID, Payload: 999})
	ft.deliver(Frame{ID: second.ID, Payload: second.Payload + 1000})
	<-done2

	if freshErr != nil {
		t.Fatalf("fresh Call error = %v", freshErr)
	}
	if got != 1042 {
		t.Fatalf("fresh Call = %d, want 1042 (late frame leaked to wrong caller)", got)
	}
	if pl := c.pendingLen(); pl != 0 {
		t.Fatalf("pending = %d after delivery, want 0", pl)
	}
}

// TestCloseFailsInflightAndRejectsNew pins shutdown: Close(cause) fails every
// in-flight Call with that cause, and a Call started afterward returns ErrClosed.
func TestCloseFailsInflightAndRejectsNew(t *testing.T) {
	ft := newFakeTransport(4)
	c := New(ft)
	c.Run()

	cause := errors.New("connection reset by peer")
	var callErr error
	done := make(chan struct{})
	go func() {
		_, callErr = c.Call(context.Background(), 5)
		close(done)
	}()
	<-ft.recorded // in-flight: registered and sent, now awaiting a reply

	c.Close(cause)
	<-done
	if !errors.Is(callErr, cause) {
		t.Fatalf("in-flight Call error = %v, want %v", callErr, cause)
	}

	if _, err := c.Call(context.Background(), 6); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close Call error = %v, want ErrClosed", err)
	}
}

// TestCloseWithoutTransportError exercises Close on a quiescent client: the
// reader is parked in Recv, and Close must unblock it (via io.Closer) so both
// goroutines exit. goleak in TestMain verifies no goroutine survives.
func TestCloseWithoutTransportError(t *testing.T) {
	ft := newFakeTransport(1)
	c := New(ft)
	c.Run()
	c.Close(ErrClosed)

	if _, err := c.Call(context.Background(), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close = %v, want ErrClosed", err)
	}
}

// TestTransportErrorFailsInflight pins the reader path: a transport Recv error is
// promoted to a Close, so the in-flight Call fails with the transport's error.
func TestTransportErrorFailsInflight(t *testing.T) {
	ft := newFakeTransport(4)
	c := New(ft)
	c.Run()
	defer c.Close(ErrClosed)

	var callErr error
	done := make(chan struct{})
	go func() {
		_, callErr = c.Call(context.Background(), 9)
		close(done)
	}()
	<-ft.recorded

	ft.Close() // makes Recv return errTransportClosed; reader turns it into Close
	<-done
	if !errors.Is(callErr, errTransportClosed) {
		t.Fatalf("in-flight Call error = %v, want errTransportClosed", callErr)
	}
}
```

## Review

"Correct" here means every response reaches exactly the caller that sent its
request, no matter the arrival order, and that no goroutine, map entry, or
blocked send survives a cancel or a close. The guarantee comes from one
invariant: a single router goroutine owns `nextID` and `pending`, so ID
allocation, registration, routing, deregistration, and shutdown are all
serialized through one `select` with no lock — which is why `-race` stays clean.
`TestConcurrentCallsMatchByID` proves the routing under reversed delivery and the
uniqueness of IDs; `TestCancelledCallDropsLateFrame` proves that a cancelled Call
removes its entry so a late frame is dropped, using FIFO delivery to make the drop
deterministically observable; the close tests prove in-flight Calls get the cause
while new ones get `ErrClosed`, and goleak proves both goroutines exit. The
production bug this pattern prevents is the response mix-up that plagues naive
multiplexed clients: without correlation IDs and a deregister-on-cancel step, a
timed-out request's late reply gets handed to the next caller in line, who then
acts on another request's data — a silent, intermittent correctness failure that
is nearly impossible to reproduce after the fact.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the channel-of-channels used to register each caller's reply channel with the router.
- [`context.Cause`](https://pkg.go.dev/context#Cause) -- surfacing why a Call was abandoned (cancellation, deadline, or a Close cause) instead of a bare `context.Canceled`.
- [Go Memory Model](https://go.dev/ref/mem) -- why single-goroutine ownership of the pending map is race-free without a mutex, and why the closed `done` channel safely publishes the close cause.
- [Effective Go: Share by communicating](https://go.dev/doc/effective_go#sharing) -- the actor discipline behind routing state changes through one goroutine.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-singleflight-cache-coordinator.md](12-singleflight-cache-coordinator.md) | Next: [../10-signaling-with-closed-channels/00-concepts.md](../10-signaling-with-closed-channels/00-concepts.md)
