# Exercise 3: Server streaming and testing Connect services with httptest

This exercise adds a server-streaming RPC — one request, many responses — that
honors context cancellation, and shows the two-tier testing strategy a senior
engineer defaults to: pure unit tests of the stream logic against a fake sender,
plus an in-process integration test that drives a real Connect client against an
`httptest` server.

This module is self-contained: its own `go mod init`, its own generated code
(described in Exercise 1), its own demo, and its own tests. It depends on
`connectrpc.com/connect` and `buf`-generated code, so the Go files live behind a
`//go:build connect` tag and the offline gate does not compile them; this is a
bar-mode lesson.

## What you'll build

```text
eventsvc/                    independent module: example.com/eventsvc
  go.mod                     requires connectrpc.com/connect, google.golang.org/protobuf
  proto/event/v1/event.proto the schema: Event, WatchEvents (server streaming)
  gen/event/v1/              GENERATED: event.pb.go, eventv1connect/ (described, not hand-written)
  service.go                 //go:build connect: WatchEvents handler + streamEvents(ctx, req, out)
  cmd/
    demo/
      main.go                //go:build connect: stream N events through a client and print them
  eventsvc_test.go           //go:build connect: fake-sender unit tests + httptest integration
```

- Files: `service.go`, `cmd/demo/main.go`, `eventsvc_test.go` (plus schema and generated code).
- Implement: a `WatchEvents(ctx, *connect.Request[T], *connect.ServerStream[Event]) error` handler that streams a backlog with `stream.Send`, respects `ctx` cancellation, and can follow; factor the loop into a transport-agnostic `streamEvents`.
- Test: unit-test `streamEvents` against a fake sender that captures `Send` calls; integration-test through `httptest.NewServer(mux)` and a real client, asserting ordered messages and that a cancelled context ends the stream with `CodeCanceled`.
- Verify: `go test -tags connect ./...` (needs the modules fetched and `buf generate` run).

### Set up the module and schema

```bash
go mod edit -go=1.26
go get connectrpc.com/connect@latest
go get google.golang.org/protobuf@latest
```

Create `proto/event/v1/event.proto`. The `stream` keyword on the response makes
`WatchEvents` a server-streaming RPC:

```proto
syntax = "proto3";

package event.v1;

option go_package = "example.com/eventsvc/gen/event/v1;eventv1";

message Event {
  string type = 1;
  string user_id = 2;
  int64 seq = 3;
}

message WatchEventsRequest {
  string user_id = 1; // optional filter
  int32 limit = 2;    // 0 = all backlog
  bool follow = 3;    // keep the stream open after the backlog
}

service EventService {
  rpc WatchEvents(WatchEventsRequest) returns (stream Event);
}
```

Generate with the same `buf.gen.yaml` from Exercise 1 (`buf generate proto`). The
generated handler method and client differ from unary in shape:

```go
// generated (illustrative) — the handler receives a ServerStream to Send on:
type EventServiceHandler interface {
	WatchEvents(context.Context, *connect.Request[eventv1.WatchEventsRequest], *connect.ServerStream[eventv1.Event]) error
}

// the client returns a ServerStreamForClient to Receive from:
type EventServiceClient interface {
	WatchEvents(context.Context, *connect.Request[eventv1.WatchEventsRequest]) (*connect.ServerStreamForClient[eventv1.Event], error)
}
```

### Streaming semantics and the cancellation discipline

A server-streaming handler is handed a `*connect.ServerStream[Event]` and calls
`stream.Send(&event)` once per message; returning `nil` ends the stream cleanly,
and returning a `connect.NewError(...)` ends it with that code. The one rule you
cannot skip is respecting `ctx`. When the client disconnects or cancels, the
handler's context is cancelled; a loop that keeps calling `Send` without checking
`ctx.Err()` produces into a dead stream and leaks the goroutine. So the handler
checks `ctx.Err()` before each send and returns `CodeCanceled` when it fires.

Note a Connect-specific nicety: the Connect protocol's streaming works over
HTTP/1.1, so the `httptest.NewServer` below (which is HTTP/1.1) can exercise the
full streaming path. gRPC-protocol streaming would require HTTP/2; the Connect
protocol does not, which is part of why it is `curl`- and proxy-friendly.

### Factor the loop out of the transport

The handler method is tied to the concrete `*connect.ServerStream[Event]`, which
is awkward to fake in a unit test. The fix is to move the business logic into a
plain function that writes through a one-method interface the real stream already
satisfies (`*connect.ServerStream[Event]` has `Send(*Event) error`). Then the
handler is a thin adapter, and `streamEvents` is unit-testable against an
in-memory capturing sender — no server, no client.

Create `service.go`:

```go
//go:build connect

package eventsvc

import (
	"context"

	"connectrpc.com/connect"
	eventv1 "example.com/eventsvc/gen/event/v1"
)

// eventSender is the one method streamEvents needs; *connect.ServerStream[Event]
// satisfies it, and so does a test fake.
type eventSender interface {
	Send(*eventv1.Event) error
}

// Service streams events from an in-memory backlog.
type Service struct {
	events []*eventv1.Event
}

// NewService returns a service seeded with a backlog of events.
func NewService(events ...*eventv1.Event) *Service {
	return &Service{events: events}
}

// WatchEvents streams the backlog and, if follow is set, blocks until the
// client goes away. It is a thin adapter over streamEvents.
func (s *Service) WatchEvents(
	ctx context.Context,
	req *connect.Request[eventv1.WatchEventsRequest],
	stream *connect.ServerStream[eventv1.Event],
) error {
	if err := streamEvents(ctx, req.Msg, s.events, stream); err != nil {
		return err
	}
	if req.Msg.GetFollow() {
		<-ctx.Done() // wait for disconnect/cancel rather than spinning
		return connect.NewError(connect.CodeCanceled, ctx.Err())
	}
	return nil
}

// streamEvents is the transport-agnostic core: it filters by user, honors the
// limit, respects cancellation, and sends through out.
func streamEvents(
	ctx context.Context,
	req *eventv1.WatchEventsRequest,
	events []*eventv1.Event,
	out eventSender,
) error {
	limit := int(req.GetLimit())
	sent := 0
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return connect.NewError(connect.CodeCanceled, err)
		}
		if uid := req.GetUserId(); uid != "" && ev.GetUserId() != uid {
			continue
		}
		if err := out.Send(ev); err != nil {
			return err
		}
		sent++
		if limit > 0 && sent >= limit {
			return nil
		}
	}
	return nil
}
```

### The runnable demo

The demo starts the service on an in-process `httptest` server, opens a stream for
three events, and prints each as it arrives — the exact `Receive()`/`Msg()`/
`Err()` loop a real client uses.

Create `cmd/demo/main.go`:

```go
//go:build connect

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"
	"example.com/eventsvc"
	eventv1 "example.com/eventsvc/gen/event/v1"
	"example.com/eventsvc/gen/event/v1/eventv1connect"
)

func main() {
	svc := eventsvc.NewService(
		&eventv1.Event{Type: "user.created", UserId: "u1", Seq: 1},
		&eventv1.Event{Type: "user.updated", UserId: "u1", Seq: 2},
		&eventv1.Event{Type: "user.deleted", UserId: "u1", Seq: 3},
	)
	mux := http.NewServeMux()
	path, handler := eventv1connect.NewEventServiceHandler(svc)
	mux.Handle(path, handler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := eventv1connect.NewEventServiceClient(http.DefaultClient, ts.URL)
	stream, err := client.WatchEvents(context.Background(),
		connect.NewRequest(&eventv1.WatchEventsRequest{Limit: 3}))
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	n := 0
	for stream.Receive() {
		ev := stream.Msg()
		n++
		fmt.Printf("event %d: %s %s\n", n, ev.GetType(), ev.GetUserId())
	}
	if err := stream.Err(); err != nil { // a false Receive can mean error, not EOF
		log.Fatal(err)
	}
	fmt.Printf("stream complete: %d events\n", n)
}
```

Run it:

```bash
go run -tags connect ./cmd/demo
```

Expected output:

```
event 1: user.created u1
event 2: user.updated u1
event 3: user.deleted u1
stream complete: 3 events
```

### Tests

The unit tier drives `streamEvents` against a `captureSender` that records every
`Send`, so backlog limiting, filtering, and cancellation are asserted with no
network. The integration tier stands up `httptest.NewServer(mux)`, builds a real
`EventServiceClient` against `ts.URL`, and exercises the full serialize/transport/
deserialize path: one test asserts the ordered messages, another opens a `follow`
stream, drains the backlog, cancels the context, and asserts the stream ends with
`CodeCanceled`.

Create `eventsvc_test.go`:

```go
//go:build connect

package eventsvc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"connectrpc.com/connect"
	eventv1 "example.com/eventsvc/gen/event/v1"
	"example.com/eventsvc/gen/event/v1/eventv1connect"
)

// captureSender is a fake eventSender that records Send calls.
type captureSender struct {
	sent []*eventv1.Event
}

func (c *captureSender) Send(ev *eventv1.Event) error {
	c.sent = append(c.sent, ev)
	return nil
}

func sampleEvents() []*eventv1.Event {
	return []*eventv1.Event{
		{Type: "user.created", UserId: "u1", Seq: 1},
		{Type: "user.updated", UserId: "u1", Seq: 2},
		{Type: "user.deleted", UserId: "u1", Seq: 3},
	}
}

func newServer(t *testing.T, svc *Service) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := eventv1connect.NewEventServiceHandler(svc)
	mux.Handle(path, handler)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestStreamEventsLimit(t *testing.T) {
	t.Parallel()
	cap := &captureSender{}
	if err := streamEvents(context.Background(),
		&eventv1.WatchEventsRequest{Limit: 2}, sampleEvents(), cap); err != nil {
		t.Fatalf("streamEvents: %v", err)
	}
	if len(cap.sent) != 2 {
		t.Fatalf("sent %d events, want 2", len(cap.sent))
	}
}

func TestStreamEventsCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first send
	err := streamEvents(ctx, &eventv1.WatchEventsRequest{}, sampleEvents(), &captureSender{})
	if got := connect.CodeOf(err); got != connect.CodeCanceled {
		t.Fatalf("code = %v, want canceled", got)
	}
}

func TestWatchEventsOrdered(t *testing.T) {
	t.Parallel()
	ts := newServer(t, NewService(sampleEvents()...))
	client := eventv1connect.NewEventServiceClient(http.DefaultClient, ts.URL)

	stream, err := client.WatchEvents(context.Background(),
		connect.NewRequest(&eventv1.WatchEventsRequest{Limit: 3}))
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	defer stream.Close()

	var got []string
	for stream.Receive() {
		got = append(got, stream.Msg().GetType())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	want := []string{"user.created", "user.updated", "user.deleted"}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestWatchEventsCancellation(t *testing.T) {
	t.Parallel()
	ts := newServer(t, NewService(sampleEvents()...))
	client := eventv1connect.NewEventServiceClient(http.DefaultClient, ts.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.WatchEvents(ctx,
		connect.NewRequest(&eventv1.WatchEventsRequest{Follow: true}))
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	defer stream.Close()

	for i := range 3 { // drain the backlog before following
		if !stream.Receive() {
			t.Fatalf("expected backlog event %d, got Receive=false: %v", i, stream.Err())
		}
	}
	cancel() // cancelling the follow stream must terminate it
	if stream.Receive() {
		t.Fatal("Receive returned true after cancellation")
	}
	if got := connect.CodeOf(stream.Err()); got != connect.CodeCanceled {
		t.Fatalf("Err code = %v, want canceled", got)
	}
}

func Example() {
	cap := &captureSender{}
	_ = streamEvents(context.Background(),
		&eventv1.WatchEventsRequest{Limit: 2},
		[]*eventv1.Event{
			{Type: "user.created"},
			{Type: "user.updated"},
			{Type: "user.deleted"},
		}, cap)
	for _, ev := range cap.sent {
		fmt.Println(ev.GetType())
	}
	// Output:
	// user.created
	// user.updated
}
```

## Review

The streaming service is correct when the handler is a pure function of its
inputs: `streamEvents` sends the backlog in order, stops at `limit`, filters by
`user_id`, and returns `CodeCanceled` the moment `ctx` is cancelled — proven by
`TestStreamEventsLimit` and `TestStreamEventsCanceled` with no network. The
integration tests then prove the wire path: `TestWatchEventsOrdered` asserts the
ordered messages through a real client, and `TestWatchEventsCancellation` proves a
cancelled context terminates a `follow` stream with `CodeCanceled` rather than
hanging.

The traps are the streaming ones. On the handler side, never loop on `Send`
without checking `ctx.Err()`, or a disconnected client leaks the goroutine — here
`streamEvents` checks it every iteration and the `follow` path waits on
`ctx.Done()` instead of spinning. On the client side, a `false` from `Receive()`
means end-of-stream *or* error, so always check `stream.Err()` after the loop and
`Close()` the stream (the tests and demo both do). Finally, do not reach for
streaming when a paginated unary call would serve a small, bounded result. This is
a bar-mode lesson: the offline gate cannot fetch `connectrpc.com/connect` or run
`buf generate`, so verify by generating the code and running
`go test -tags connect ./...` with network access.

## Resources

- [Connect for Go — Streaming](https://connectrpc.com/docs/go/streaming/) — `ServerStream.Send`, the `ServerStreamForClient` `Receive`/`Msg`/`Err` loop.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest#NewServer) — `NewServer` and `Server.URL` for in-process client/server tests.
- [`connectrpc.com/connect` reference](https://pkg.go.dev/connectrpc.com/connect) — `ServerStream`, `ServerStreamForClient`, and `CodeCanceled`.
- [Connect protocol reference](https://connectrpc.com/docs/protocol/) — why Connect streaming works over HTTP/1.1.

---

Back to [02-errors-interceptors-metadata.md](02-errors-interceptors-metadata.md) | Next: [../02-grpc-gateway-rest-json/00-concepts.md](../02-grpc-gateway-rest-json/00-concepts.md)
