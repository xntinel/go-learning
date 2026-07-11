# Exercise 3: Request Pipelining

A synchronous client pays one full network round trip per request: send, wait, receive, repeat. Pipelining breaks that lock-step by putting many requests on the wire before reading any reply, so a batch of a thousand requests costs roughly one round trip instead of a thousand. This exercise builds a pipelined client over a single connection, where an in-order server lets responses be matched to requests by a FIFO queue rather than a correlation-ID map.

This module is fully self-contained: it bundles its own minimal frame codec and in-order server, starts with its own `go mod init`, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
frame.go             Frame, Encode, Decode, APIKey, ErrConnClosed (bundled minimal codec)
server.go            ServeConn: a strictly in-order, one-frame-at-a-time echo loop
pipeline.go          Pipeline, Call, Send (fire-and-queue), readLoop (FIFO match)
cmd/
  demo/
    main.go          pipeline 5 requests over one conn, then collect ordered replies
mqpipe_test.go       pipelined batch over net.Pipe, FIFO ordering, closed-pipeline error
```

- Files: `frame.go`, `server.go`, `pipeline.go`, `cmd/demo/main.go`, `mqpipe_test.go`.
- Implement: `Pipeline` with `Send(req) *Call` that writes immediately and queues the call, and a `readLoop` that matches replies to calls by FIFO order; `ServeConn`, the in-order server that makes FIFO matching valid.
- Test: send a batch without waiting, assert every reply matches its request in order, and assert a closed pipeline returns `ErrConnClosed`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p mqpipe/cmd/demo && cd mqpipe
go mod init example.com/mqpipe
```

### Why a FIFO queue is enough when responses keep their order

The previous exercise multiplexed with a correlation-ID map because its server dispatched each frame to a goroutine and could answer them out of order. Pipelining makes a different bargain: it requires the server to answer strictly in request order, and in exchange the client's matching collapses from a map to a queue. If the nth response is guaranteed to belong to the nth request, the client does not need to read an ID to know whose reply it holds — it just dequeues the oldest waiting call. This is precisely how HTTP/1.1 pipelining and Redis pipelining match replies, and why both are sensitive to head-of-line blocking: one slow request stalls every request queued behind it.

The mechanism has two halves that must stay in lock-step. `Send` writes its frame to the connection and, under the same write lock, pushes the pending `Call` onto a FIFO channel. Holding the lock across both steps is what guarantees the queue order equals the wire order: if two sends could interleave their writes and their enqueues independently, a frame written first might be queued second, and every later match would be off by one. `readLoop` does the reverse: it decodes one response, dequeues the oldest call, and delivers the reply. Because writes and enqueues are atomic and the server preserves order, the front of the queue is always the call the just-decoded response answers.

The correlation ID is still stamped on every frame, but here it is an assertion rather than a lookup. After dequeuing, `readLoop` checks that the response's ID equals the dequeued call's ID; a mismatch means the ordering contract was violated (a buggy server, or a frame boundary lost) and the call fails loudly instead of silently returning another request's reply. That single check turns "the server promised in-order responses" from an assumption into something the client verifies on every frame.

`Send` returns a `*Call` whose `Done` channel fires when the reply arrives, mirroring the standard library's `net/rpc` asynchronous `Go` call. The caller fires a whole batch of `Send`s, collecting the `*Call` values, then ranges over them waiting on each `Done`. Because the pipeline is single-producer by design — one goroutine doing the sending — `Send` is not safe for concurrent callers; that is the deliberate trade for the simpler FIFO matcher. A multi-producer client is exactly the multiplexed design from the previous exercise.

Create `frame.go`:

```go
package mqpipe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// APIKey identifies the protocol operation carried in a Frame header.
type APIKey uint16

const APIProduce APIKey = 0

// ErrConnClosed is returned by Send once the pipeline's connection is closed.
var (
	ErrBadFrame   = errors.New("malformed frame")
	ErrConnClosed = errors.New("connection closed")
)

// headerLen is the fixed body prefix after the 4-byte length field.
const headerLen = 8

// Frame is one protocol message. The length field counts the bytes after
// itself: length = headerLen + len(Payload).
type Frame struct {
	APIKey        APIKey
	APIVersion    uint16
	CorrelationID int32
	Payload       []byte
}

// Encode writes f to w as a complete, length-prefixed frame.
func (f *Frame) Encode(w io.Writer) error {
	length := headerLen + len(f.Payload)
	hdr := make([]byte, 4+headerLen)
	binary.BigEndian.PutUint32(hdr[0:], uint32(length))
	binary.BigEndian.PutUint16(hdr[4:], uint16(f.APIKey))
	binary.BigEndian.PutUint16(hdr[6:], f.APIVersion)
	binary.BigEndian.PutUint32(hdr[8:], uint32(f.CorrelationID))
	if _, err := w.Write(hdr); err != nil {
		return fmt.Errorf("mqpipe: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("mqpipe: write payload: %w", err)
		}
	}
	return nil
}

// Decode reads exactly one frame from r using io.ReadFull.
func Decode(r io.Reader) (*Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("mqpipe: read length: %w", err)
	}
	length := int(binary.BigEndian.Uint32(lenBuf[:]))
	if length < headerLen {
		return nil, fmt.Errorf("%w: length=%d", ErrBadFrame, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("mqpipe: read frame body: %w", err)
	}
	return &Frame{
		APIKey:        APIKey(binary.BigEndian.Uint16(body[0:])),
		APIVersion:    binary.BigEndian.Uint16(body[2:]),
		CorrelationID: int32(binary.BigEndian.Uint32(body[4:])),
		Payload:       body[headerLen:],
	}, nil
}
```

Create `server.go`:

```go
package mqpipe

import (
	"bufio"
	"net"
)

// ServeConn reads frames from conn and replies in strict request order: it
// fully handles one frame (read, call h, write the response) before reading the
// next. That in-order discipline is exactly what lets a pipelined client match
// replies to requests by FIFO position instead of by correlation ID.
func ServeConn(conn net.Conn, h func(*Frame) *Frame) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	for {
		req, err := Decode(br)
		if err != nil {
			return
		}
		resp := h(req)
		if resp == nil {
			continue
		}
		if err := resp.Encode(bw); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
	}
}
```

Create `pipeline.go`:

```go
package mqpipe

import (
	"bufio"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// maxPipelineDepth bounds how many requests may be queued unanswered. Send
// blocks once this many are outstanding, providing back-pressure.
const maxPipelineDepth = 1024

// Call is one pipelined request. Done fires (with the Call itself) once Reply
// or Err is set. Done is buffered, so the delivery never blocks.
type Call struct {
	Req   *Frame
	Reply *Frame
	Err   error
	Done  chan *Call
}

func (c *Call) finish() { c.Done <- c }

// Pipeline sends requests over one connection without waiting for each reply,
// matching responses to requests by FIFO order. Send is single-producer: call
// it from one goroutine.
type Pipeline struct {
	conn    net.Conn
	bw      *bufio.Writer
	writeMu sync.Mutex
	nextID  atomic.Int32
	pending chan *Call

	once sync.Once
	done chan struct{}
}

// NewPipeline starts a pipeline over conn and launches its reader goroutine.
func NewPipeline(conn net.Conn) *Pipeline {
	p := &Pipeline{
		conn:    conn,
		bw:      bufio.NewWriter(conn),
		pending: make(chan *Call, maxPipelineDepth),
		done:    make(chan struct{}),
	}
	go p.readLoop()
	return p
}

// Close shuts the pipeline down. In-flight and subsequent calls get
// ErrConnClosed.
func (p *Pipeline) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.conn.Close()
}

// Send writes req immediately, queues the pending Call, and returns without
// waiting for the reply. Wait on the returned Call's Done channel.
func (p *Pipeline) Send(req *Frame) *Call {
	call := &Call{Req: req, Done: make(chan *Call, 1)}

	select {
	case <-p.done:
		call.Err = ErrConnClosed
		call.finish()
		return call
	default:
	}

	req.CorrelationID = p.nextID.Add(1)

	p.writeMu.Lock()
	err := req.Encode(p.bw)
	if err == nil {
		err = p.bw.Flush()
	}
	if err != nil {
		p.writeMu.Unlock()
		call.Err = fmt.Errorf("mqpipe: send: %w", err)
		call.finish()
		return call
	}
	// Enqueue under the write lock so queue order equals wire order.
	p.pending <- call
	p.writeMu.Unlock()
	return call
}

// readLoop decodes responses in order, dequeues the oldest pending Call, and
// delivers the reply. It verifies the correlation ID as an ordering assertion.
func (p *Pipeline) readLoop() {
	br := bufio.NewReader(p.conn)
	for {
		f, err := Decode(br)
		if err != nil {
			p.shutdown()
			return
		}
		select {
		case call := <-p.pending:
			if f.CorrelationID != call.Req.CorrelationID {
				call.Err = fmt.Errorf("mqpipe: out-of-order reply: got id %d, want %d",
					f.CorrelationID, call.Req.CorrelationID)
			} else {
				call.Reply = f
			}
			call.finish()
		case <-p.done:
			return
		}
	}
}

// shutdown closes done and fails every still-queued Call.
func (p *Pipeline) shutdown() {
	p.once.Do(func() { close(p.done) })
	for {
		select {
		case call := <-p.pending:
			call.Err = ErrConnClosed
			call.finish()
		default:
			return
		}
	}
}
```

Trace a batch. The producer calls `Send` five times in a row. Each `Send` stamps the next correlation ID, writes the frame, and queues the `Call` under `writeMu`, so five frames hit the wire back-to-back and five calls sit in `pending` in the same order. The server's `ServeConn` reads, echoes, and replies to each in turn, preserving order. `readLoop` decodes reply one, dequeues call one, checks the IDs agree, fires its `Done`; then reply two, call two; and so on. The producer, having collected the five `*Call` values, ranges over them waiting on each `Done` and reads `Reply` in request order. Close the pipeline and `readLoop`'s `Decode` fails, `shutdown` drains any still-queued calls with `ErrConnClosed`, and a later `Send` short-circuits on the closed `done` channel.

### The runnable demo

The demo starts an in-order echo server on a loopback port, opens a pipeline, sends five requests without waiting between them, then collects the replies. The correlation IDs in the output confirm the FIFO match held: reply 0 carries ID 1, reply 4 carries ID 5, in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net"

	"example.com/mqpipe"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		mqpipe.ServeConn(conn, func(req *mqpipe.Frame) *mqpipe.Frame {
			return &mqpipe.Frame{
				APIKey:        req.APIKey,
				CorrelationID: req.CorrelationID,
				Payload:       req.Payload,
			}
		})
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		log.Fatal(err)
	}
	p := mqpipe.NewPipeline(conn)
	defer p.Close()

	// Fire the whole batch before reading any reply.
	const n = 5
	calls := make([]*mqpipe.Call, n)
	for i := 0; i < n; i++ {
		payload := fmt.Appendf(nil, "message-%d", i)
		calls[i] = p.Send(&mqpipe.Frame{APIKey: mqpipe.APIProduce, Payload: payload})
	}

	fmt.Printf("pipelined %d requests before reading any reply\n", n)
	for i, call := range calls {
		<-call.Done
		if call.Err != nil {
			log.Fatalf("call %d: %v", i, call.Err)
		}
		fmt.Printf("  reply[%d] correlationID=%d payload=%q\n",
			i, call.Reply.CorrelationID, call.Reply.Payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pipelined 5 requests before reading any reply
  reply[0] correlationID=1 payload="message-0"
  reply[1] correlationID=2 payload="message-1"
  reply[2] correlationID=3 payload="message-2"
  reply[3] correlationID=4 payload="message-3"
  reply[4] correlationID=5 payload="message-4"
```

### Tests

`startEchoPipe` wires a `Pipeline` to a `ServeConn` echo over `net.Pipe`, with no real socket. `TestPipelineFIFOOrder` sends a batch without waiting and asserts every reply matches its request payload and correlation ID in order — the property the FIFO matcher exists to provide. `TestPipelineClosed` closes the pipeline and asserts a later `Send` returns `ErrConnClosed`.

Create `mqpipe_test.go`:

```go
package mqpipe

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"testing"
)

func startEchoPipe(t *testing.T) *Pipeline {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go ServeConn(serverConn, func(req *Frame) *Frame {
		return &Frame{
			APIKey:        req.APIKey,
			CorrelationID: req.CorrelationID,
			Payload:       append([]byte(nil), req.Payload...),
		}
	})
	p := NewPipeline(clientConn)
	t.Cleanup(func() { p.Close() })
	return p
}

func TestPipelineFIFOOrder(t *testing.T) {
	t.Parallel()
	p := startEchoPipe(t)

	const n = 50
	calls := make([]*Call, n)
	for i := 0; i < n; i++ {
		payload := fmt.Appendf(nil, "msg-%d", i)
		calls[i] = p.Send(&Frame{APIKey: APIProduce, Payload: payload})
	}

	for i, call := range calls {
		<-call.Done
		if call.Err != nil {
			t.Fatalf("call %d: %v", i, call.Err)
		}
		want := fmt.Appendf(nil, "msg-%d", i)
		if !bytes.Equal(call.Reply.Payload, want) {
			t.Errorf("reply %d: payload = %q, want %q", i, call.Reply.Payload, want)
		}
		if call.Reply.CorrelationID != int32(i+1) {
			t.Errorf("reply %d: correlationID = %d, want %d", i, call.Reply.CorrelationID, i+1)
		}
	}
}

func TestPipelineClosed(t *testing.T) {
	t.Parallel()
	p := startEchoPipe(t)
	p.Close()

	call := p.Send(&Frame{APIKey: APIProduce, Payload: []byte("x")})
	<-call.Done
	if !errors.Is(call.Err, ErrConnClosed) {
		t.Fatalf("err = %v, want ErrConnClosed", call.Err)
	}
}
```

## Review

The pipeline is correct when queue order equals wire order and the server preserves order. Confirm that `Send` holds `writeMu` across both the frame write and the `pending` enqueue, so two frames can never be queued in a different order than they were written, and that `readLoop` dequeues exactly one call per decoded response and verifies the correlation ID as an ordering check. The FIFO test passing under `-race` with a 50-request batch is the proof the match never slips.

Common mistakes for this feature. The first is enqueuing the call outside the write lock, which lets the queue order drift from the wire order and shifts every later reply onto the wrong call. The second is using a pipelined client against an out-of-order server: the moment the server answers request 3 before request 2, the FIFO matcher hands request 2's reply to request 3's caller, which is why the correlation-ID assertion matters and why true concurrent dispatch needs the map-based multiplexer instead. The third is treating `Send` as concurrent-safe; it is single-producer here, and sharing one `Pipeline` across goroutines reintroduces exactly the ordering races the design avoids.

## Resources

- [`net/rpc` Client.Go](https://pkg.go.dev/net/rpc#Client.Go) — the standard library's asynchronous call returning a `*Call` with a `Done` channel, the model this `Send` follows.
- [Redis pipelining](https://redis.io/docs/latest/develop/use/pipelining/) — why sending a batch before reading replies collapses N round trips into one, and the throughput it buys.
- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the exact-length read that lets the in-order server and the pipeline reader frame the stream without short-read bugs.

---

Back to [02-multiplexed-client-server.md](02-multiplexed-client-server.md) | Next: [04-connection-pool-reconnect.md](04-connection-pool-reconnect.md)
