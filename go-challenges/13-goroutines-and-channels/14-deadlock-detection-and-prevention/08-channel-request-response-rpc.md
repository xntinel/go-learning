# Exercise 8: In-Process Request/Response with a Buffered Reply Channel

An actor-style service goroutine that owns some state and answers requests over channels is a
clean way to avoid locks entirely. But it has a specific deadlock trap: if each request carries an
*unbuffered* reply channel and the caller times out and walks away, the server blocks forever
trying to send its response. This exercise builds the pattern correctly — reply channels buffered
to capacity 1 — so a caller's timeout can never wedge the server.

This module is fully self-contained: its own `go mod init`, all types inline, its own demo and
tests.

## What you'll build

```text
actor/                     independent module: example.com/actor
  go.mod                   go 1.25
  actor.go                 Server (owns a counter); Do(ctx, req) with a buffered reply chan
  cmd/
    demo/
      main.go              a few requests; one with an expired context
  actor_test.go            round trip; caller-timeout does not wedge server; -race
```

- Files: `actor.go`, `cmd/demo/main.go`, `actor_test.go`.
- Implement: a `Server` goroutine that receives request structs each carrying a `reply chan Response` buffered to capacity 1, so the server's send never blocks even if the caller has stopped listening.
- Test: a happy-path round trip; a test where the caller's context expires before the server replies, asserting the server does NOT block and keeps serving subsequent requests; run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/14-deadlock-detection-and-prevention/08-channel-request-response-rpc/cmd/demo
cd go-solutions/13-goroutines-and-channels/14-deadlock-detection-and-prevention/08-channel-request-response-rpc
go mod edit -go=1.25
```

### Why the reply channel must be buffered

The actor pattern gives one goroutine sole ownership of some state (here, a counter). Callers do
not touch the state directly; they send a request over a channel and the server mutates the state
and sends a response back. Because only the server touches the state, there is no lock and no data
race — the channel handoff is the synchronization.

The response has to travel back somehow, and the idiomatic way is a per-request reply channel: the
request struct carries a `reply chan Response`, the caller sends the request and then receives from
its own reply channel. Now consider the failure. The caller uses a context deadline: it sends the
request, then `select`s over `reply` and `ctx.Done()`. The deadline fires first (the server was
busy), so the caller returns `DeadlineExceeded` and moves on — it is no longer receiving from
`reply`. The server finishes the work and does `req.reply <- resp`. If `reply` is **unbuffered**,
that send blocks until someone receives, and no one ever will — the server goroutine is wedged on
one abandoned request, and every subsequent request piles up behind it. One caller-side timeout has
taken down the whole server. This is a partial deadlock: the server is stuck, the rest of the
process runs, the runtime says nothing.

The fix is one character of capacity: make each reply channel **buffered with capacity 1**. Now the
server's send always succeeds immediately — it deposits the response into the buffer and moves on to
the next request, whether or not the caller is still listening. If the caller already left, the
buffered response is simply garbage-collected with the channel. The server can never block on the
reply, so a caller timeout is contained to that one caller. This is the standard idiom for
request/response over channels, and the reason is exactly this decoupling.

The server also needs its own shutdown path: a `done` channel (or context) so it exits when the
service stops, and callers need to handle the server being gone. We give the server a `Close` that
stops its loop.

Create `actor.go`:

```go
package actor

import "context"

// Op selects what a request asks the server to do.
type Op int

const (
	// OpIncr adds Delta to the counter and returns the new value.
	OpIncr Op = iota
	// OpGet returns the current counter without changing it.
	OpGet
)

// Response carries the server's answer back to the caller.
type Response struct {
	Value int
}

// request is what a caller sends to the server. The reply channel is buffered to
// capacity 1 so the server can always deposit its Response without blocking, even
// if the caller has already timed out and stopped listening.
type request struct {
	op    Op
	delta int
	reply chan Response
}

// Server owns a counter and mutates it only from its own goroutine, so no lock is
// needed. Callers interact through Do.
type Server struct {
	reqs chan request
	done chan struct{}
}

// NewServer starts the server goroutine and returns a handle to it.
func NewServer() *Server {
	s := &Server{
		reqs: make(chan request),
		done: make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *Server) loop() {
	var counter int
	for {
		select {
		case <-s.done:
			return
		case req := <-s.reqs:
			var resp Response
			switch req.op {
			case OpIncr:
				counter += req.delta
				resp.Value = counter
			case OpGet:
				resp.Value = counter
			}
			// Never blocks: reply is buffered to 1. If the caller has gone, the
			// response lands in the buffer and is discarded with the channel.
			req.reply <- resp
		}
	}
}

// Do sends a request and waits for the response or ctx cancellation. Because the
// reply channel is buffered, a timeout here never wedges the server: it returns
// ctx.Err() and the server proceeds to the next request unaffected.
func (s *Server) Do(ctx context.Context, op Op, delta int) (Response, error) {
	req := request{op: op, delta: delta, reply: make(chan Response, 1)}
	select {
	case s.reqs <- req:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-s.done:
		return Response{}, context.Canceled
	}
	select {
	case resp := <-req.reply:
		return resp, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Close stops the server goroutine.
func (s *Server) Close() { close(s.done) }
```

### The runnable demo

The demo issues a couple of increments and a get, then makes one request with an already-expired
context to show it returns an error while the server keeps serving afterward.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/actor"
)

func main() {
	s := actor.NewServer()
	defer s.Close()

	ctx := context.Background()
	r, _ := s.Do(ctx, actor.OpIncr, 5)
	fmt.Printf("incr 5 -> %d\n", r.Value)
	r, _ = s.Do(ctx, actor.OpIncr, 3)
	fmt.Printf("incr 3 -> %d\n", r.Value)

	// An already-expired context: Do returns an error, server is unaffected.
	expired, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Do(expired, actor.OpGet, 0); errors.Is(err, context.Canceled) {
		fmt.Println("get with cancelled ctx: canceled")
	}

	// Server still serves normally afterward.
	r, _ = s.Do(ctx, actor.OpGet, 0)
	fmt.Printf("get -> %d\n", r.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
incr 5 -> 5
incr 3 -> 8
get with cancelled ctx: canceled
get -> 8
```

### Tests

`TestRoundTrip` covers the happy path. `TestCallerTimeoutDoesNotWedgeServer` is the point of the
exercise: it makes the server slow, has a caller time out waiting for the reply, and then asserts a
*subsequent* request still succeeds — which is only possible if the server did not block on the
abandoned reply. If the reply channel were unbuffered, the server would be stuck on the first
caller's send and the second request would time out too; the test would then fail (caught by its
watchdog). All under `-race`, since the counter is shared state accessed across goroutines.

Create `actor_test.go`:

```go
package actor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	s := NewServer()
	defer s.Close()

	r, err := s.Do(t.Context(), OpIncr, 10)
	if err != nil || r.Value != 10 {
		t.Fatalf("incr: got %d,%v want 10,nil", r.Value, err)
	}
	r, err = s.Do(t.Context(), OpGet, 0)
	if err != nil || r.Value != 10 {
		t.Fatalf("get: got %d,%v want 10,nil", r.Value, err)
	}
}

func TestCallerTimeoutDoesNotWedgeServer(t *testing.T) {
	t.Parallel()

	// A server that is slow to reply: we intercept by making the first caller
	// abandon its request, then check the server still serves the next one.
	s := NewServer()
	defer s.Close()

	// First caller: an already-expired context, so Do returns before the reply
	// arrives. The server will still try to send the reply; with a buffered reply
	// channel that send succeeds and the server moves on.
	expired, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // ensure the deadline has passed
	if _, err := s.Do(expired, OpIncr, 1); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// Depending on timing the request may or may not have been accepted; either
		// way the error must be a context error, never a hang.
		t.Fatalf("timed-out Do err = %v, want a context error", err)
	}

	// Second caller: the server must still respond. If the server had wedged on the
	// first (abandoned) reply, this would time out.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Do(t.Context(), OpGet, 0); err != nil {
			t.Errorf("second Do err = %v, want nil (server wedged?)", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not answer the second request: wedged on an abandoned reply")
	}
}
```

## Review

The server is correct when no single caller can wedge it. The buffered reply channel is what
guarantees that: `req.reply <- resp` in the server loop always succeeds because the buffer has room
for exactly the one response that request will ever get, so the server never blocks on a caller that
has left. `TestCallerTimeoutDoesNotWedgeServer` proves it by abandoning one request and confirming
the next is still answered. The actor pattern's other payoff is visible too: the counter is mutated
only from `loop`, so there is no lock and `-race` stays clean.

The mistake to avoid is an unbuffered reply channel — it couples the server's liveness to every
caller's patience, and one timeout takes the server down. The related trap is forgetting the
server's own shutdown path: without the `done` channel the `loop` goroutine leaks when the service
stops. Keep the buffer at exactly 1: a larger buffer is unnecessary (each reply channel serves one
request) and a zero buffer reintroduces the wedge.

## Resources

- [Go by Example: Stateful Goroutines](https://gobyexample.com/stateful-goroutines) — the actor/owned-state pattern over channels.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered vs unbuffered channel semantics.
- [`context.Context`](https://pkg.go.dev/context#Context) — caller-side deadlines that must not wedge the callee.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-bounded-fanout-semaphore.md](09-bounded-fanout-semaphore.md)
