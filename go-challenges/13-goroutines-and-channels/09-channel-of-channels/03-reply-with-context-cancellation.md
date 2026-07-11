# Exercise 3: Context-Aware Call: Deadlines and Cancellation

A request-reply service that ignores context is a latent outage: a slow backend
blocks the HTTP handler that called it right past its deadline budget, and one
stuck dependency stalls every request behind it. This exercise upgrades `Call`
to take a `context.Context` and honor it on both sides of the exchange — while
sending the request and while awaiting the reply — returning the cancellation
reason via `context.Cause`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ctxservice/                independent module: example.com/ctxservice
  go.mod
  ctxservice.go            type Service; Call(ctx, n) selects on ctx.Done both ways
  cmd/
    demo/
      main.go              runnable demo: a fast call and a timed-out call
  ctxservice_test.go       timeout, pre-cancel-with-cause, still-serves tests
```

- Files: `ctxservice.go`, `cmd/demo/main.go`, `ctxservice_test.go`.
- Implement: `Call(ctx context.Context, n int) (int, error)` that selects on `ctx.Done()` while sending and while receiving, returning `context.Cause(ctx)` when the context ends first.
- Test: a short `WithTimeout` returns `context.DeadlineExceeded` before a slow worker finishes; a `WithCancelCause` context returns the injected cause; the service still serves later calls (no leak); base context is `t.Context()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ctxservice/cmd/demo
cd ~/go-exercises/ctxservice
go mod init example.com/ctxservice
```

### Two places context must be honored

A `Call` has two blocking points, and a naive implementation only guards one.
First, the send onto the request channel can block — the worker may be busy and
the inbox unbuffered. Second, the receive on the reply channel can block — the
worker may be slow. If `Call` selects on `ctx.Done()` only around the receive, a
full inbox still hangs the caller; if it selects only around the send, a slow
worker still hangs the caller. Production `Call` selects on `ctx.Done()` in both
places, so whichever way the caller is blocked, an expired deadline or a
cancellation frees it promptly.

When a context branch fires, return `context.Cause(ctx)` rather than `ctx.Err()`.
`context.Cause` is the richer accessor: for a `context.WithTimeout` context it
returns `context.DeadlineExceeded`; for a plain `context.WithCancel` cancellation
it returns `context.Canceled`; and for a `context.WithCancelCause` context
cancelled with `cancel(err)`, it returns exactly `err`. That last case is what
lets a caller distinguish "the client disconnected" from "we shed this load" from
"the deadline passed" — all three arrive as different causes through the same code
path. Because `context.DeadlineExceeded` and `context.Canceled` still satisfy
`errors.Is`, callers that only care about the coarse category keep working.

The worker is deliberately slow — it sleeps to model a backend under load. The
key correctness property is that a caller abandoning the receive on timeout does
not leak the worker: the reply channel is buffered at capacity one, so when the
worker finishes it drops the (now unwanted) response into the buffer and moves on
to the next request. The service keeps serving. That is why the third test can
time a call out and then make a normal call that succeeds.

Create `ctxservice.go`:

```go
package ctxservice

import (
	"context"
	"errors"
	"time"
)

// ErrShuttingDown is returned once the service has been shut down.
var ErrShuttingDown = errors.New("ctxservice: shutting down")

type request struct {
	n     int
	reply chan response
}

type response struct {
	value int
	err   error
}

// Service is a slow request-reply backend used to demonstrate context handling.
type Service struct {
	requests chan request
	work     time.Duration
	quit     chan struct{}
	done     chan struct{}
}

// New returns a Service whose worker takes work time to answer each request.
func New(work time.Duration) *Service {
	return &Service{
		requests: make(chan request),
		work:     work,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Run is the actor loop. Start it with: go s.Run().
func (s *Service) Run() {
	defer close(s.done)
	for {
		select {
		case req := <-s.requests:
			time.Sleep(s.work) // model a slow backend
			req.reply <- response{value: req.n * 2}
		case <-s.quit:
			return
		}
	}
}

// Shutdown signals the loop to stop and waits for it to exit.
func (s *Service) Shutdown() {
	close(s.quit)
	<-s.done
}

// Call sends n and returns the doubled value. It honors ctx on both the send and
// the receive, returning context.Cause(ctx) if the context ends first.
func (s *Service) Call(ctx context.Context, n int) (int, error) {
	reply := make(chan response, 1)
	select {
	case s.requests <- request{n: n, reply: reply}:
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, ErrShuttingDown
	}
	select {
	case resp := <-reply:
		return resp.value, resp.err
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, ErrShuttingDown
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/ctxservice"
)

func main() {
	s := ctxservice.New(30 * time.Millisecond)
	go s.Run()
	defer s.Shutdown()

	// A generous deadline: the call completes.
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	defer cancel1()
	if v, err := s.Call(ctx1, 21); err == nil {
		fmt.Printf("fast call: %d\n", v)
	}

	// A tight deadline: the call times out before the worker finishes.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel2()
	if _, err := s.Call(ctx2, 21); errors.Is(err, context.DeadlineExceeded) {
		fmt.Println("slow call: deadline exceeded")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast call: 42
slow call: deadline exceeded
```

### Tests

`TestCallTimesOut` gives the service a 50 ms worker and calls with a 5 ms
deadline, asserting `context.DeadlineExceeded` comes back before the worker could
possibly have finished. `TestCallPreCanceledWithCause` cancels a
`WithCancelCause` context with a custom sentinel *before* calling, and asserts the
call returns that exact cause. `TestServiceStillServesAfterTimeout` times a call
out, then makes a normal call with a generous deadline and asserts it succeeds —
proving the abandoned call did not wedge the worker. All use `t.Context()` as the
base context.

Create `ctxservice_test.go`:

```go
package ctxservice

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCallTimesOut(t *testing.T) {
	t.Parallel()
	s := New(50 * time.Millisecond)
	go s.Run()
	defer s.Shutdown()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()

	_, err := s.Call(ctx, 7)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v, want context.DeadlineExceeded", err)
	}
}

func TestCallPreCanceledWithCause(t *testing.T) {
	t.Parallel()
	errShed := errors.New("load shed")

	s := New(time.Millisecond)
	go s.Run()
	defer s.Shutdown()

	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errShed) // cancel before calling

	_, err := s.Call(ctx, 7)
	if !errors.Is(err, errShed) {
		t.Fatalf("Call error = %v, want %v", err, errShed)
	}
	// ctx.Err() is still context.Canceled here, but Call returns the richer
	// cause so the caller can tell *why* it was cancelled.
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

func TestServiceStillServesAfterTimeout(t *testing.T) {
	t.Parallel()
	s := New(30 * time.Millisecond)
	go s.Run()
	defer s.Shutdown()

	// This call abandons its receive on timeout.
	ctx1, cancel1 := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel1()
	if _, err := s.Call(ctx1, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want DeadlineExceeded", err)
	}

	// The worker must still be able to answer a later call.
	ctx2, cancel2 := context.WithTimeout(t.Context(), time.Second)
	defer cancel2()
	got, err := s.Call(ctx2, 21)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if got != 42 {
		t.Fatalf("second call = %d, want 42", got)
	}
}
```

Note the split the second test makes explicit: `Call` returns the injected cause
(`errShed`), which is what the caller acts on, while `ctx.Err()` on the same
context is still the coarse `context.Canceled`. `context.Cause` is strictly richer
than `ctx.Err()` — cancelling with `cancel(nil)` would make the cause fall back to
`context.Canceled`, so returning the cause never loses information.

## Review

`Call` is correct when a caller's deadline always wins over a slow backend. The
two `select` statements — one around the send, one around the receive — are the
whole mechanism; drop the `ctx.Done()` case from either and one of the tests hangs
until its own safety timeout. Returning `context.Cause(ctx)` rather than
`ctx.Err()` is what makes `TestCallPreCanceledWithCause` able to recover the
injected reason; with `ctx.Err()` the caller would only ever see the generic
`context.Canceled`.

The subtle correctness point is the no-leak property proven by
`TestServiceStillServesAfterTimeout`. Because the reply channel is buffered at
capacity one, a worker that finishes after its caller has given up still completes
its send and returns to the loop, so the next call is served normally. Remove the
buffer and the worker blocks forever on the abandoned reply — the exact leak the
next exercise isolates. Run `go test -race` to confirm the send/receive handoff is
race-free.

## Resources

- [`context` package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancelCause`, `Cause`, `DeadlineExceeded`, `Canceled`.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — recovering the cancellation reason.
- [`testing.T.Context`](https://pkg.go.dev/testing#T.Context) — the per-test context cancelled at cleanup (Go 1.24+).
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — cancellation propagation through channels.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-reply-buffer-prevents-worker-leak.md](04-reply-buffer-prevents-worker-leak.md)
