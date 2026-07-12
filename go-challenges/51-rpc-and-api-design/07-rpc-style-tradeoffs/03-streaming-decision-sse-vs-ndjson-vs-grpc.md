# Exercise 3: Streaming Semantics: Server-Streaming RPC vs SSE vs NDJSON

This is the exercise that mirrors a recurring on-the-job argument: a team wants to
tail live order events to a dashboard and reaches for gRPC streaming when SSE would
ship in a day and survive every proxy. You build the same "tail live order events"
feature three ways — a Connect/gRPC server-streaming procedure, a Server-Sent
Events endpoint, and a newline-delimited JSON endpoint — so the trade-off is a
thing you have implemented, not a thing you have read about.

This module is fully self-contained. It begins with its own `go mod init`, and
ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
orderstream/                 independent module: example.com/orderstream
  go.mod                     go 1.26
  events.go                  OrderEvent (the one event type framed three ways)
  sse.go                     StreamSSE/SSEHandler over net/http, flush per frame (pure)
  sseclient.go               DecodeSSE; ErrMalformedFrame (pure)
  ndjson.go                  StreamNDJSON/NDJSONHandler; DecodeNDJSONLine (pure)
  order.proto               server-streaming OrderTailService schema (illustrative)
  stream_online.go           //go:build online — Connect server-streaming handler
  cmd/
    demo/
      main.go                runnable pure demo: tail three events over SSE
  orderstream_test.go        offline SSE flush-order + NDJSON + malformed-frame tests; Example
  stream_online_test.go      //go:build online — Connect server-streaming integration test
```

- Files: `events.go`, `sse.go`, `sseclient.go`, `ndjson.go`, `order.proto`, `stream_online.go`, `cmd/demo/main.go`, `orderstream_test.go`, `stream_online_test.go`.
- Implement: `StreamSSE` and `StreamNDJSON` handlers that flush after every frame via `http.NewResponseController(w).Flush()`, `DecodeSSE`/`DecodeNDJSONLine` clients wrapping `ErrMalformedFrame`, and the online Connect server-streaming handler using `stream.Send`.
- Test: an SSE test that feeds events through an unbuffered channel and reads each framed event back, so a non-flushing handler would deadlock; a table-driven NDJSON test for 0/1/many events in order; a malformed-frame test asserting `errors.Is(err, ErrMalformedFrame)`; and an `Example` printing the first frame.
- Verify: `go test -count=1 -race ./...` (SSE and NDJSON are pure stdlib and fully gate); the Connect streaming path builds and runs with `-tags online` after codegen.

This is a mode=bar lesson only because the Connect server-streaming half needs
generated code and the external module; the SSE and NDJSON halves are pure stdlib
and pass the full race gate. Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/07-rpc-style-tradeoffs/03-streaming-decision-sse-vs-ndjson-vs-grpc/cmd/demo
cd go-solutions/51-rpc-and-api-design/07-rpc-style-tradeoffs/03-streaming-decision-sse-vs-ndjson-vs-grpc
go mod edit -go=1.26
```

### When each streaming shape wins

Before the code, the decision the exercise exists to teach. All three carry the
same `OrderEvent`; they differ only in framing and reach.

- **Connect/gRPC server-streaming** gives a typed, backpressured, multiplexed
  stream with a generated message type end to end. Choose it for internal
  service-to-service feeds where both ends have stubs and you want the compiler to
  enforce the event shape. It is the wrong tool for a browser, which cannot read
  the gRPC trailers without gRPC-Web and a proxy.
- **SSE** (`text/event-stream`) is the browser-native choice: `new EventSource(url)`
  on the client, `data: <json>\n\n` frames on the server, and automatic
  reconnection built into the browser. It rides ordinary HTTP and survives every
  proxy and load balancer. Choose it for dashboards, log tailing, and progress
  feeds that terminate in a browser.
- **NDJSON** over a chunked response is the curl-friendly firehose: one JSON object
  per line, consumable with `while read line` or any line reader. Choose it for
  simple internal or CLI consumers that want no client library and no event
  framing to parse.

The recurring mistake is reaching for gRPC streaming for the browser dashboard case.
SSE ships faster, needs zero client tooling, and tolerates the infrastructure you
already run; save typed bidi gRPC for internal streams that need it.

### The one event type

Create `events.go`:

```go
package orderstream

// OrderEvent is one event in a live "tail the order" feed. The same value type is
// framed three ways (SSE, NDJSON, and a Connect server stream) so the wire framing
// is the only thing that differs between transports.
type OrderEvent struct {
	OrderID string `json:"order_id"`
	Type    string `json:"type"`
	Seq     int    `json:"seq"`
}
```

### SSE: flush every frame, and flush the head first

The SSE handler writes each event as a `data: <json>\n\n` frame and calls
`http.NewResponseController(w).Flush()` after each one. Flushing is the whole point:
without it, the `ResponseWriter` buffers and the client sees the entire stream at
once when the handler returns, defeating streaming. Using `NewResponseController`
rather than a `w.(http.Flusher)` type assertion means the flush still works when
middleware wraps the writer, and it returns an error to handle.

There is a second, subtler flush: the handler flushes the 200 status and headers
*before* the first event. A client that connects before any event exists would
otherwise block waiting for the response head; flushing the head immediately lets it
start reading. The handler stops when the source channel is closed or the request
context is cancelled, so a disconnecting client does not leak the goroutine.

Create `sse.go`:

```go
package orderstream

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// StreamSSE writes each event from src as a text/event-stream frame and flushes
// after each one, so a browser EventSource (or curl) sees events as they happen
// rather than all at once when the handler returns. It stops when src is closed or
// the request context is cancelled.
func StreamSSE(w http.ResponseWriter, r *http.Request, src <-chan OrderEvent) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	// Flush the 200 and headers immediately so a client that connects before the
	// first event still receives the response head and can start reading.
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return err
	}

	for {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case ev, ok := <-src:
			if !ok {
				return nil
			}
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return err
			}
			if err := rc.Flush(); err != nil {
				return err
			}
		}
	}
}

// SSEHandler adapts a single event source to an http.Handler.
func SSEHandler(src <-chan OrderEvent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = StreamSSE(w, r, src)
	}
}
```

The SSE client reads one frame at a time: lines up to a blank line, taking the
`data: ` payload and ignoring comment (`:`) heartbeat lines. It returns `io.EOF` at
the end of the stream and wraps `ErrMalformedFrame` for a frame that carries no
`data:` line or whose payload is not valid JSON.

Create `sseclient.go`:

```go
package orderstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrMalformedFrame reports a frame that does not follow the expected framing.
var ErrMalformedFrame = errors.New("malformed frame")

// DecodeSSE reads one text/event-stream frame (lines up to a blank line) and
// decodes its data: payload into an OrderEvent. It returns io.EOF at the end of
// the stream and wraps ErrMalformedFrame for a frame with no data: line or with a
// data: payload that is not valid JSON.
func DecodeSSE(br *bufio.Reader) (OrderEvent, error) {
	var data string
	sawData := false
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF && line == "" {
			if !sawData {
				return OrderEvent{}, io.EOF
			}
			break
		}
		if err != nil && err != io.EOF {
			return OrderEvent{}, err
		}
		line = strings.TrimRight(line, "\n")
		if line == "" { // blank line terminates the frame
			break
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = v
			sawData = true
			continue
		}
		if strings.HasPrefix(line, ":") { // comment / heartbeat
			continue
		}
	}
	if !sawData {
		return OrderEvent{}, fmt.Errorf("sse: %w", ErrMalformedFrame)
	}
	var ev OrderEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return OrderEvent{}, fmt.Errorf("sse decode: %w", ErrMalformedFrame)
	}
	return ev, nil
}
```

### NDJSON: one object per line

The NDJSON handler is the same pattern with simpler framing: a `json.Encoder`
writes one object per line (`Encode` appends the newline), flushed after each. The
client reads with a `bufio.Scanner` line by line and decodes each; a bad line wraps
`ErrMalformedFrame`.

Create `ndjson.go`:

```go
package orderstream

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// StreamNDJSON writes each event as one JSON object followed by a newline
// (newline-delimited JSON) and flushes after each, producing a curl-friendly
// firehose. It stops when src is closed or the request context is cancelled.
func StreamNDJSON(w http.ResponseWriter, r *http.Request, src <-chan OrderEvent) error {
	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)

	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return err
	}

	for {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case ev, ok := <-src:
			if !ok {
				return nil
			}
			if err := enc.Encode(ev); err != nil { // Encode appends '\n'
				return err
			}
			if err := rc.Flush(); err != nil {
				return err
			}
		}
	}
}

// NDJSONHandler adapts a single event source to an http.Handler.
func NDJSONHandler(src <-chan OrderEvent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = StreamNDJSON(w, r, src)
	}
}

// DecodeNDJSONLine decodes one NDJSON line into an OrderEvent, wrapping
// ErrMalformedFrame on invalid JSON.
func DecodeNDJSONLine(line []byte) (OrderEvent, error) {
	var ev OrderEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return OrderEvent{}, fmt.Errorf("ndjson decode: %w", ErrMalformedFrame)
	}
	return ev, nil
}
```

### The Connect server-streaming twin (online)

The typed alternative. The schema declares a `stream OrderEvent` return, and the
handler calls `stream.Send` per event; the client iterates with `Receive`/`Msg`.
The contrast with SSE/NDJSON is exactly the point: the frame type is generated and
the stream is typed and backpressured, at the cost of needing codegen and, for a
browser, gRPC-Web.

This is the illustrative schema; it is a `proto` block, not assembled Go:

```proto
syntax = "proto3";
package order.v1;
option go_package = "example.com/orderstream/gen/order/v1;orderv1";

message OrderEvent {
  string order_id = 1;
  string type = 2;
  int64 seq = 3;
}

message TailOrderEventsRequest {
  string order_id = 1;
}

service OrderTailService {
  rpc TailOrderEvents(TailOrderEventsRequest) returns (stream OrderEvent);
}
```

Create `stream_online.go`:

```go
//go:build online

// This file holds the Connect server-streaming handler. It is excluded from the
// default build because it imports connectrpc.com/connect and the generated
// order/v1 packages. Build and test it with -tags online after codegen. The SSE
// and NDJSON handlers are pure stdlib and pass the full offline race gate.
package orderstream

import (
	"context"

	"connectrpc.com/connect"
	orderv1 "example.com/orderstream/gen/order/v1"
)

// TailServer implements the generated server-streaming OrderTailService handler.
type TailServer struct {
	events []OrderEvent
}

func NewTailServer(events []OrderEvent) *TailServer {
	return &TailServer{events: events}
}

// TailOrderEvents streams each event to the client with stream.Send. Unlike the
// SSE and NDJSON handlers, the frame type is the generated orderv1.OrderEvent and
// the transport handles framing and backpressure. It honors context cancellation.
func (s *TailServer) TailOrderEvents(
	ctx context.Context,
	req *connect.Request[orderv1.TailOrderEventsRequest],
	stream *connect.ServerStream[orderv1.OrderEvent],
) error {
	for _, ev := range s.events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.Send(&orderv1.OrderEvent{
			OrderId: ev.OrderID,
			Type:    ev.Type,
			Seq:     int64(ev.Seq),
		}); err != nil {
			return err
		}
	}
	return nil
}
```

### The runnable demo

The demo tails three events over SSE against an in-process `httptest` server and
prints each as it decodes it, so it exercises the real flush-per-frame path with no
codegen.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/orderstream"
)

func main() {
	src := make(chan orderstream.OrderEvent, 3)
	for i := 1; i <= 3; i++ {
		src <- orderstream.OrderEvent{OrderID: "ord-7", Type: "status", Seq: i}
	}
	close(src)

	srv := httptest.NewServer(orderstream.SSEHandler(src))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	for {
		ev, err := orderstream.DecodeSSE(br)
		if err != nil {
			break
		}
		fmt.Printf("event seq=%d type=%s order=%s\n", ev.Seq, ev.Type, ev.OrderID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event seq=1 type=status order=ord-7
event seq=2 type=status order=ord-7
event seq=3 type=status order=ord-7
```

### Tests

The SSE test proves flushing without mocking it: the source is an *unbuffered*
channel, so after the test sends one event it must be able to read that event's
frame back before it sends the next. If the handler did not flush, the frame would
sit in the buffer and the read would block, which the per-read timeout turns into a
failure. The NDJSON test is table-driven over 0, 1, and many events and checks
order. The malformed-frame test asserts both decoders wrap `ErrMalformedFrame`. The
`Example` prints the first frame.

Create `orderstream_test.go`:

```go
package orderstream

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readSSEWithin decodes one frame in a goroutine and fails if it does not arrive
// within d, so a non-flushing handler is caught as a timeout rather than a hang.
func readSSEWithin(t *testing.T, br *bufio.Reader, d time.Duration) OrderEvent {
	t.Helper()
	type res struct {
		ev  OrderEvent
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ev, err := DecodeSSE(br)
		ch <- res{ev, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("DecodeSSE: %v", r.err)
		}
		return r.ev
	case <-time.After(d):
		t.Fatal("no frame within timeout: handler did not flush")
		return OrderEvent{}
	}
}

func TestSSEFlushesEachFrameInOrder(t *testing.T) {
	t.Parallel()
	src := make(chan OrderEvent) // unbuffered: a frame arrives only if flushed
	srv := httptest.NewServer(SSEHandler(src))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	for i := 1; i <= 3; i++ {
		src <- OrderEvent{OrderID: "ord-1", Type: "status", Seq: i}
		ev := readSSEWithin(t, br, 2*time.Second)
		if ev.Seq != i {
			t.Fatalf("frame %d out of order: got seq %d", i, ev.Seq)
		}
	}
	close(src)
}

func TestNDJSONFramesDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int
	}{
		{"zero", 0},
		{"one", 1},
		{"many", 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := make(chan OrderEvent, tc.n)
			for i := 1; i <= tc.n; i++ {
				src <- OrderEvent{OrderID: "ord-2", Type: "status", Seq: i}
			}
			close(src)

			srv := httptest.NewServer(NDJSONHandler(src))
			defer srv.Close()

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()

			sc := bufio.NewScanner(resp.Body)
			got := 0
			for sc.Scan() {
				ev, err := DecodeNDJSONLine(sc.Bytes())
				if err != nil {
					t.Fatalf("DecodeNDJSONLine: %v", err)
				}
				got++
				if ev.Seq != got {
					t.Fatalf("line %d out of order: got seq %d", got, ev.Seq)
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got != tc.n {
				t.Fatalf("got %d events, want %d", got, tc.n)
			}
		})
	}
}

func TestDecodeMalformedFrame(t *testing.T) {
	t.Parallel()
	if _, err := DecodeNDJSONLine([]byte("{not json")); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("DecodeNDJSONLine err = %v, want errors.Is ErrMalformedFrame", err)
	}
	br := bufio.NewReader(strings.NewReader("event: ping\n\n"))
	if _, err := DecodeSSE(br); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("DecodeSSE err = %v, want errors.Is ErrMalformedFrame", err)
	}
}

func Example() {
	src := make(chan OrderEvent, 1)
	src <- OrderEvent{OrderID: "ord-9", Type: "created", Seq: 1}
	close(src)

	srv := httptest.NewServer(SSEHandler(src))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	ev, err := DecodeSSE(br)
	if err != nil {
		fmt.Println("decode:", err)
		return
	}
	fmt.Printf("%s %s seq=%d\n", ev.OrderID, ev.Type, ev.Seq)
	// Output: ord-9 created seq=1
}
```

The online test proves the typed alternative against a real Connect server: it
mounts the streaming handler, streams three events, and asserts the client receives
them in order. It is behind `//go:build online` and runs with `-tags online` after
codegen.

Create `stream_online_test.go`:

```go
//go:build online

package orderstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	orderv1 "example.com/orderstream/gen/order/v1"
	"example.com/orderstream/gen/order/v1/orderv1connect"
)

func TestConnectServerStreaming(t *testing.T) {
	events := []OrderEvent{
		{OrderID: "ord-1", Type: "created", Seq: 1},
		{OrderID: "ord-1", Type: "confirmed", Seq: 2},
		{OrderID: "ord-1", Type: "shipped", Seq: 3},
	}
	mux := http.NewServeMux()
	path, handler := orderv1connect.NewOrderTailServiceHandler(NewTailServer(events))
	mux.Handle(path, handler)

	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := orderv1connect.NewOrderTailServiceClient(srv.Client(), srv.URL)
	stream, err := client.TailOrderEvents(context.Background(),
		connect.NewRequest(&orderv1.TailOrderEventsRequest{OrderId: "ord-1"}))
	if err != nil {
		t.Fatalf("TailOrderEvents: %v", err)
	}
	got := 0
	for stream.Receive() {
		got++
		if int(stream.Msg().GetSeq()) != got {
			t.Fatalf("out of order: got seq %d, want %d", stream.Msg().GetSeq(), got)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if got != len(events) {
		t.Fatalf("got %d events, want %d", got, len(events))
	}
}
```

## Review

The correctness of a streaming handler is entirely about flushing, and the SSE test
is built so that a non-flushing handler cannot pass: because the source channel is
unbuffered, a frame only reaches the client if the handler flushed it, and the
per-read timeout converts a missing flush into a failure instead of a hang. The
mistakes to avoid are the ones from the concepts file: never flush with
`w.(http.Flusher).Flush()`, which panics under wrapping middleware — use
`http.NewResponseController(w).Flush()` and check its error; and do not forget to
flush the response head before the first event, or a client that connects early
blocks. The larger review point is the decision itself: if this feed were for a
browser dashboard, SSE is the answer and standing up gRPC streaming plus a gRPC-Web
proxy is over-engineering; reserve the typed Connect stream for internal consumers.

Confirm the SSE and NDJSON core with `go test -race ./...`: the flush-order test,
the 0/1/many table, and the malformed-frame assertions must all pass and the
`Example` must print the first frame. To exercise the typed alternative, generate
the Connect code, add the module requirements, and `go test -tags online ./...`; a
passing `TestConnectServerStreaming` shows the three events arriving in order over a
real server stream.

## Resources

- [`net/http#ResponseController`](https://pkg.go.dev/net/http#ResponseController) — `Flush() error`, the wrapper-safe successor to `http.Flusher`.
- [Server-Sent Events (WHATWG HTML)](https://html.spec.whatwg.org/multipage/server-sent-events.html) — the `text/event-stream` framing and the `EventSource` reconnection model.
- [Connect for Go — streaming](https://connectrpc.com/docs/go/streaming/) — server-streaming handlers with `stream.Send` and the client `Receive`/`Msg` loop.
- [`encoding/json#Encoder`](https://pkg.go.dev/encoding/json#Encoder) — `Encode` and its trailing newline, the basis of NDJSON framing.

---

Back to [02-one-endpoint-three-protocols.md](02-one-endpoint-three-protocols.md) | Next: [../../52-ai-llm-backends/01-llm-sdk-client/00-concepts.md](../../52-ai-llm-backends/01-llm-sdk-client/00-concepts.md)
