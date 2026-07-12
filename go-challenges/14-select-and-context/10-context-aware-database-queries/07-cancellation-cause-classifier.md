# Exercise 7: Classifying Why a Query Was Aborted: Timeout vs Client-Disconnect vs Shutdown

`ctx.Err()` tells you a query was cancelled but not *why*, and the why decides the
response: a server-side budget timeout is retryable (503), a client disconnect is
not (499, nobody is listening), and a shutdown drain is a 503 that should stop
accepting work. This exercise wires the three real cancellation sources through
layered contexts with causes and classifies them into an operational decision.

## What you'll build

```text
cancelcause/                 independent module: example.com/cancelcause
  go.mod                     go 1.25
  cancelcause.go             Server with layered ctx (shutdown -> req -> query); Classify(ctx) -> Decision
  cmd/
    demo/
      main.go                fire each of the three sources, print the decision
  cancelcause_test.go        cause-per-source, Err-insufficient, nil-cause fallback, -race
```

Files: `cancelcause.go`, `cmd/demo/main.go`, `cancelcause_test.go`.
Implement: sentinel causes `ErrServerBudget`/`ErrClientGone`/`ErrShutdown`; a `Server.Handle` that layers a shutdown context, a client-cancellable request context (bridged from the client connection via `context.AfterFunc`), and a per-query `context.WithTimeoutCause`; and `Classify(ctx)` reading `context.Cause(ctx)`.
Test: each source makes `context.Cause(ctx)` return its exact sentinel and `Classify` map it to the right status/retry decision; `ctx.Err()` is only `DeadlineExceeded`/`Canceled` in every case; `WithCancelCause(nil)` falls back to `context.Canceled`.
Verify: `go test -count=1 -race ./...`

### Why `ctx.Err()` is not enough, and how causes fix it

`context.Cause(ctx)` (Go 1.21) returns a specific error explaining a cancellation,
where `ctx.Err()` returns only the coarse `Canceled` or `DeadlineExceeded`. You
supply the specific error at the point you set up each cancellation:
`context.WithTimeoutCause(parent, d, cause)` attaches a cause to a timeout, and
`context.WithCancelCause(parent)` hands back a `CancelCauseFunc` you call with a
cause. The crucial propagation rule: `context.Cause` walks up the chain and
returns the cause of the *first* context that was cancelled. So a single
`Classify(queryCtx)` correctly reports a server budget timeout on the query
context, a client disconnect that cancelled the parent request context, or a
shutdown that cancelled the grandparent — all through one lookup.

The `Handle` method builds exactly the layering a real server has:

```
shutdownCtx (WithCancelCause, server lifetime)
  -> reqCtx (WithCancelCause, per request; cancelled with ErrClientGone)
    -> queryCtx (WithTimeoutCause, per query; cause ErrServerBudget)
```

The client-disconnect bridge is the production-shaped part. A real server learns
of a disconnect through the request's context; we model that with
`context.AfterFunc(clientCtx, func() { reqCancel(ErrClientGone) })`, which fires a
cause-bearing cancellation the moment the client's context is done, and returns a
`stop` we defer so the callback is removed on the normal path. When any of the
three fire, `work(queryCtx)` returns, and `Classify(queryCtx)` reads the cause:

| cause | Err() | Decision |
| --- | --- | --- |
| `ErrServerBudget` | `DeadlineExceeded` | 503, retryable |
| `ErrClientGone` | `Canceled` | 499, no retry |
| `ErrShutdown` | `Canceled` | 503, drain, no retry |

Notice `Err()` is identical (`Canceled`) for both the client and shutdown cases —
which is exactly why classifying on `Err()` alone would send the wrong response,
and why the cause is load-bearing. `Classify` also has fallbacks: a bare
`DeadlineExceeded` cause maps to a retryable 503, a bare `Canceled` to 499, and a
`nil` cause (not cancelled) to 200. `WithCancelCause` called with a `nil` cause
falls back to `context.Canceled`, which the fallback branch handles.

Set up the module:

```bash
go mod edit -go=1.25
```

Create `cancelcause.go`:

```go
package cancelcause

import (
	"context"
	"errors"
	"time"
)

// The three cancellation causes a request-serving path must distinguish.
var (
	ErrServerBudget = errors.New("cancelcause: server query budget exceeded")
	ErrClientGone   = errors.New("cancelcause: client disconnected")
	ErrShutdown     = errors.New("cancelcause: server shutting down")
)

// Decision is the operational response derived from a cancellation cause.
type Decision struct {
	Status    int
	Retryable bool
	Reason    string
}

// Classify maps context.Cause(ctx) to a Decision. It reads the cause, not
// ctx.Err(), because Err() cannot tell a client disconnect from a shutdown.
func Classify(ctx context.Context) Decision {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, ErrServerBudget):
		return Decision{Status: 503, Retryable: true, Reason: "server query budget exceeded"}
	case errors.Is(cause, ErrClientGone):
		return Decision{Status: 499, Retryable: false, Reason: "client disconnected"}
	case errors.Is(cause, ErrShutdown):
		return Decision{Status: 503, Retryable: false, Reason: "server draining"}
	case errors.Is(cause, context.DeadlineExceeded):
		return Decision{Status: 503, Retryable: true, Reason: "deadline exceeded"}
	case errors.Is(cause, context.Canceled):
		return Decision{Status: 499, Retryable: false, Reason: "canceled"}
	default:
		return Decision{Status: 200, Retryable: false, Reason: "ok"}
	}
}

// Server owns a shutdown context that cancels every in-flight request when the
// process is draining.
type Server struct {
	shutdownCtx    context.Context
	shutdownCancel context.CancelCauseFunc
}

// NewServer returns a Server whose shutdown context is live until Shutdown.
func NewServer() *Server {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Server{shutdownCtx: ctx, shutdownCancel: cancel}
}

// Shutdown cancels the shutdown context with ErrShutdown, draining all requests.
func (s *Server) Shutdown() { s.shutdownCancel(ErrShutdown) }

// Handle runs work under a layered context and, if work is aborted, returns the
// Decision classified from the cancellation cause plus the work error. The
// Decision comes first and the error last, matching Go's convention of error as
// the final return value. clientCtx models the client connection: when it is
// done, the request is cancelled with ErrClientGone. budget is the per-query
// timeout carrying ErrServerBudget.
func (s *Server) Handle(clientCtx context.Context, budget time.Duration, work func(context.Context) error) (Decision, error) {
	reqCtx, reqCancel := context.WithCancelCause(s.shutdownCtx)
	defer reqCancel(nil)

	// Bridge a client disconnect into a cause-bearing cancellation.
	stop := context.AfterFunc(clientCtx, func() { reqCancel(ErrClientGone) })
	defer stop()

	queryCtx, cancelQuery := context.WithTimeoutCause(reqCtx, budget, ErrServerBudget)
	defer cancelQuery()

	if err := work(queryCtx); err != nil {
		return Classify(queryCtx), err
	}
	return Decision{Status: 200, Retryable: false, Reason: "ok"}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/cancelcause"
)

func block(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func main() {
	s := cancelcause.NewServer()

	d, _ := s.Handle(context.Background(), 5*time.Millisecond, block)
	fmt.Printf("budget:   status=%d retry=%v reason=%s\n", d.Status, d.Retryable, d.Reason)

	clientCtx, disconnect := context.WithCancel(context.Background())
	go func() { time.Sleep(3 * time.Millisecond); disconnect() }()
	d, _ = s.Handle(clientCtx, time.Hour, block)
	fmt.Printf("client:   status=%d retry=%v reason=%s\n", d.Status, d.Retryable, d.Reason)

	go func() { time.Sleep(3 * time.Millisecond); s.Shutdown() }()
	d, _ = s.Handle(context.Background(), time.Hour, block)
	fmt.Printf("shutdown: status=%d retry=%v reason=%s\n", d.Status, d.Retryable, d.Reason)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
budget:   status=503 retry=true reason=server query budget exceeded
client:   status=499 retry=false reason=client disconnected
shutdown: status=503 retry=false reason=server draining
```

### Tests

Each subtest fires one source, asserts the exact cause, the mapped decision, and
that `ctx.Err()` alone is insufficient (identical between the client and shutdown
cases).

Create `cancelcause_test.go`:

```go
package cancelcause

import (
	"context"
	"errors"
	"testing"
	"time"
)

func block(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestServerBudget(t *testing.T) {
	t.Parallel()
	s := NewServer()

	d, err := s.Handle(context.Background(), 5*time.Millisecond, block)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("work err = %v, want DeadlineExceeded", err)
	}
	if d.Status != 503 || !d.Retryable {
		t.Fatalf("Decision = %+v, want {503 retryable}", d)
	}
}

func TestClientDisconnect(t *testing.T) {
	t.Parallel()
	s := NewServer()

	clientCtx, disconnect := context.WithCancel(context.Background())
	go func() { time.Sleep(3 * time.Millisecond); disconnect() }()

	d, err := s.Handle(clientCtx, time.Hour, block)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("work err = %v, want Canceled", err)
	}
	if d.Status != 499 || d.Retryable {
		t.Fatalf("Decision = %+v, want {499 not-retryable}", d)
	}
}

func TestShutdownDrain(t *testing.T) {
	t.Parallel()
	s := NewServer()

	go func() { time.Sleep(3 * time.Millisecond); s.Shutdown() }()

	d, err := s.Handle(context.Background(), time.Hour, block)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("work err = %v, want Canceled", err)
	}
	if d.Status != 503 || d.Retryable {
		t.Fatalf("Decision = %+v, want {503 drain not-retryable}", d)
	}
}

func TestErrIsInsufficientToDistinguish(t *testing.T) {
	t.Parallel()
	// The client and shutdown cases produce the SAME ctx.Err() (Canceled) but
	// DIFFERENT decisions; only the cause distinguishes them.
	s1 := NewServer()
	clientCtx, disconnect := context.WithCancel(context.Background())
	go func() { time.Sleep(3 * time.Millisecond); disconnect() }()
	dClient, errClient := s1.Handle(clientCtx, time.Hour, block)

	s2 := NewServer()
	go func() { time.Sleep(3 * time.Millisecond); s2.Shutdown() }()
	dShutdown, errShutdown := s2.Handle(context.Background(), time.Hour, block)

	if !errors.Is(errClient, context.Canceled) || !errors.Is(errShutdown, context.Canceled) {
		t.Fatalf("both should be Canceled: client=%v shutdown=%v", errClient, errShutdown)
	}
	if dClient.Status == dShutdown.Status {
		t.Fatalf("decisions must differ despite identical Err(): client=%d shutdown=%d", dClient.Status, dShutdown.Status)
	}
}

func TestNilCauseFallsBackToCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(nil) // nil cause -> context.Canceled
	if got := context.Cause(ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("Cause after nil cancel = %v, want context.Canceled", got)
	}
	if d := Classify(ctx); d.Status != 499 {
		t.Fatalf("Classify(nil-cause) = %+v, want status 499", d)
	}
}

func TestNoCancellationIsOK(t *testing.T) {
	t.Parallel()
	if d := Classify(context.Background()); d.Status != 200 {
		t.Fatalf("Classify(live ctx) = %+v, want status 200", d)
	}
}
```

## Review

The classifier is correct when identical `ctx.Err()` values map to different
decisions via the cause. `TestErrIsInsufficientToDistinguish` is the heart of the
lesson: the client-disconnect and shutdown cases both yield `context.Canceled`,
yet they must produce a 499 and a 503 — so any implementation that switched on
`ctx.Err()` would be wrong, and only `context.Cause` carries enough information.
The three source tests confirm the cause propagates from wherever it was set
(query timeout, request cancel, or server shutdown) to a single `Classify` at the
query context. The `nil`-cause test pins the documented fallback to
`context.Canceled`. Run `-race`: the disconnect/shutdown are fired from separate
goroutines while `work` blocks on the query context.

## Resources

- [context: WithCancelCause and Cause](https://pkg.go.dev/context#WithCancelCause) — attaching and reading a specific cancellation cause.
- [context: WithTimeoutCause](https://pkg.go.dev/context#WithTimeoutCause) and [WithDeadlineCause](https://pkg.go.dev/context#WithDeadlineCause) — causes for timeouts.
- [context: AfterFunc](https://pkg.go.dev/context#AfterFunc) — bridging a client disconnect into a cause-bearing cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-context-aware-retry-backoff.md](08-context-aware-retry-backoff.md)
