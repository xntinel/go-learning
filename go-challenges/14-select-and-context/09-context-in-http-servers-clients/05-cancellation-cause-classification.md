# Exercise 5: Classify Why a Request Context Died (Cause)

When a request context is cancelled, `ctx.Err()` tells you *that* it died —
`Canceled` or `DeadlineExceeded` — but on-call needs to know *why*: a slow
upstream (handler-timeout), a bailing client (client-disconnect), or a deploy
(server-shutdown). This exercise builds the middleware and classifier that
attach a specific cause with `context.WithTimeoutCause` / `WithCancelCause` and
read it back with `context.Cause`, emitting one structured reason per request.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
causeclass/                independent module: example.com/causeclass
  go.mod                   go 1.26
  cause.go                 sentinel causes; Reason(ctx); WithCauseLogging middleware
  cmd/
    demo/
      main.go              drives handler-timeout and client-disconnect
  cause_test.go            table test over the three causes + Err() coarseness
```

Files: `cause.go`, `cmd/demo/main.go`, `cause_test.go`.
Implement: sentinel errors `ErrHandlerTimeout`, `ErrClientDisconnect`, `ErrServerShutdown`; `Reason(ctx) string` mapping `context.Cause(ctx)` to a label; `WithCauseLogging(d, logf, next)` middleware deriving `context.WithTimeoutCause`.
Test: a table driving (a) derived-timeout fires -> `handler-timeout`, (b) parent cancelled with a client cause -> `client-disconnect`, (c) parent cancelled with a shutdown cause -> `server-shutdown`; and that `ctx.Err()` stays coarse (`DeadlineExceeded`/`Canceled`) while `Cause` is specific.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/causeclass/cmd/demo
cd ~/go-exercises/causeclass
go mod init example.com/causeclass
```

## The design

`context.WithTimeoutCause(parent, d, cause)` behaves exactly like `WithTimeout`
for control flow — the child is cancelled when `d` elapses and `ctx.Err()`
returns `context.DeadlineExceeded` — but it additionally records `cause` so that
`context.Cause(child)` returns *your* error instead of the generic one. Likewise
`WithCancelCause(parent)` returns a `CancelCauseFunc` you call with a specific
error; that error becomes the child's cause.

The subtlety that makes this useful is how causes propagate up the tree.
`context.Cause` returns the cause of the *first* cancellation in the chain. So if
the middleware derives `ctx, cancel := context.WithTimeoutCause(r.Context(), d,
ErrHandlerTimeout)` and the *parent* (`r.Context()`) is cancelled first — because
the client disconnected — then `context.Cause(ctx)` returns the parent's cause,
not `ErrHandlerTimeout`. That is precisely the distinction on-call needs: the
handler's own timeout only "wins" the cause when it actually fired first. When
the client bailed, the client's cause surfaces even though the handler had its
own deadline armed.

`Reason(ctx)` maps the cause to a stable label using `errors.Is`, so a wrapped
cause still classifies correctly. It keeps `ctx.Err()` in reserve: the concepts
insist that `ctx.Err()` stays coarse (`DeadlineExceeded`/`Canceled`) for control
flow while `Cause` carries the operational detail, and the test pins both.

Create `cause.go`:

```go
package causeclass

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Sentinel causes attached via WithTimeoutCause / WithCancelCause. errors.Is
// against these classifies a cancellation for on-call.
var (
	ErrHandlerTimeout   = errors.New("handler deadline exceeded")
	ErrClientDisconnect = errors.New("client disconnected")
	ErrServerShutdown   = errors.New("server shutting down")
)

// Reason maps context.Cause(ctx) to a stable label. It returns "" when the
// context is not cancelled, so a caller can log a reason only when there is one.
func Reason(ctx context.Context) string {
	err := context.Cause(ctx)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrHandlerTimeout):
		return "handler-timeout"
	case errors.Is(err, ErrClientDisconnect):
		return "client-disconnect"
	case errors.Is(err, ErrServerShutdown):
		return "server-shutdown"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "unknown"
	}
}

// WithCauseLogging derives a per-request timeout context whose cause is
// ErrHandlerTimeout, runs next, and after next returns logs the classified
// reason (if the context was cancelled). Because context.Cause reports the
// first cancellation in the chain, a client-disconnect on r.Context() surfaces
// as client-disconnect here, not handler-timeout.
func WithCauseLogging(d time.Duration, logf func(reason string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeoutCause(r.Context(), d, ErrHandlerTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
		if reason := Reason(ctx); reason != "" {
			logf(reason)
		}
	})
}
```

## The runnable demo

The demo drives two cancellation paths directly (no network needed) and prints
the classified reason plus the coarse `ctx.Err()` for each, showing the two-level
distinction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/causeclass"
)

func main() {
	// Handler-timeout: the derived deadline fires first.
	ctx, cancel := context.WithTimeoutCause(context.Background(), 10*time.Millisecond, causeclass.ErrHandlerTimeout)
	defer cancel()
	<-ctx.Done()
	fmt.Printf("timeout:     reason=%s err=%v\n", causeclass.Reason(ctx), ctx.Err())

	// Client-disconnect: the parent is cancelled first, so its cause wins even
	// though the child had its own deadline armed.
	parent, cancelParent := context.WithCancelCause(context.Background())
	child, cancelChild := context.WithTimeoutCause(parent, time.Hour, causeclass.ErrHandlerTimeout)
	defer cancelChild()
	cancelParent(causeclass.ErrClientDisconnect)
	<-child.Done()
	fmt.Printf("disconnect:  reason=%s err=%v\n", causeclass.Reason(child), child.Err())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeout:     reason=handler-timeout err=context deadline exceeded
disconnect:  reason=client-disconnect err=context canceled
```

## Tests

The table drives all three causes and, for each, asserts both the classified
`Reason` and the coarse `ctx.Err()` so the two-level distinction is pinned:
handler-timeout carries `DeadlineExceeded` as its coarse error, while the two
parent-cancellation cases carry `Canceled`. A separate test exercises the
`WithCauseLogging` middleware end to end through an `httptest` server, capturing
the logged reason via a buffered channel.

Create `cause_test.go`:

```go
package causeclass

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReasonClassifiesCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		build      func() context.Context
		wantReason string
		wantErr    error
	}{
		{
			name: "handler-timeout",
			build: func() context.Context {
				ctx, cancel := context.WithTimeoutCause(context.Background(), 5*time.Millisecond, ErrHandlerTimeout)
				t.Cleanup(cancel)
				<-ctx.Done()
				return ctx
			},
			wantReason: "handler-timeout",
			wantErr:    context.DeadlineExceeded,
		},
		{
			name: "client-disconnect",
			build: func() context.Context {
				parent, cancelParent := context.WithCancelCause(context.Background())
				ctx, cancel := context.WithTimeoutCause(parent, time.Hour, ErrHandlerTimeout)
				t.Cleanup(cancel)
				cancelParent(ErrClientDisconnect)
				<-ctx.Done()
				return ctx
			},
			wantReason: "client-disconnect",
			wantErr:    context.Canceled,
		},
		{
			name: "server-shutdown",
			build: func() context.Context {
				parent, cancelParent := context.WithCancelCause(context.Background())
				ctx, cancel := context.WithTimeoutCause(parent, time.Hour, ErrHandlerTimeout)
				t.Cleanup(cancel)
				cancelParent(ErrServerShutdown)
				<-ctx.Done()
				return ctx
			},
			wantReason: "server-shutdown",
			wantErr:    context.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := tc.build()
			if got := Reason(ctx); got != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got, tc.wantReason)
			}
			if !errors.Is(ctx.Err(), tc.wantErr) {
				t.Fatalf("ctx.Err() = %v, want (coarse) %v", ctx.Err(), tc.wantErr)
			}
		})
	}
}

func TestMiddlewareLogsHandlerTimeout(t *testing.T) {
	t.Parallel()

	logged := make(chan string, 1)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		http.Error(w, "timeout", http.StatusGatewayTimeout)
	})
	srv := httptest.NewServer(WithCauseLogging(10*time.Millisecond, func(reason string) {
		logged <- reason
	}, slow))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	select {
	case reason := <-logged:
		if reason != "handler-timeout" {
			t.Fatalf("logged reason = %q, want handler-timeout", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("middleware did not log a reason")
	}
}
```

## Review

The middleware is correct when the cause travels intact: `Reason` reads
`context.Cause(ctx)` and classifies it with `errors.Is`, and the cause of the
first cancellation in the chain wins — so a client-disconnect on `r.Context()`
correctly outranks the handler's own armed deadline. The point the table pins is
that `ctx.Err()` remains the coarse signal for control flow while `Cause` carries
the operational reason; conflating the two (logging only `ctx.Err()`) throws away
the one thing on-call needs. Run with `-race`; the logging channel is buffered so
the middleware never blocks after the client goes away.

## Resources

- [`context.WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) — attach a cause to a deadline.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — and the `CancelCauseFunc` that records the cause.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — read the first-cancellation cause; contrast with `ctx.Err()`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-timeouthandler-and-write-deadline.md](04-timeouthandler-and-write-deadline.md) | Next: [06-detached-background-work.md](06-detached-background-work.md)
