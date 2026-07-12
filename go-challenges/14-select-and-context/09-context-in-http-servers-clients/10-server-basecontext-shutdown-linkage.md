# Exercise 10: Server BaseContext Linked to Graceful Shutdown

`Server.Shutdown` stops accepting new connections and waits for in-flight
requests to finish — but by default it tells running handlers nothing, so a
long-poll handler hangs until the grace period force-closes it. This exercise
wires `http.Server.BaseContext` to a root context you cancel at shutdown, so
every in-flight `r.Context()` observes the shutdown at once and drains cleanly. It
also stamps a per-connection value via `ConnContext`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
shutdownlink/              independent module: example.com/shutdownlink
  go.mod                   go 1.26
  server.go                connKey; NewServer wiring BaseContext + ConnContext; long-poll handler
  cmd/
    demo/
      main.go              long-poll request drains the instant shutdown begins
  server_test.go           shutdown-cancels-handler test, ConnContext-value test
```

Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
Implement: `NewServer` returning an `*http.Server` whose `BaseContext` returns a cancellable root and whose `ConnContext` stamps a per-connection value; a long-poll handler that selects on `r.Context().Done()`.
Test: fire a long-poll in a goroutine, cancel the root and call `Shutdown`, assert the handler observed cancellation (returns promptly with a shutdown-shaped response) and that the `ConnContext` value reached the handler; `Shutdown` returns nil.
Verify: `go test -count=1 -race ./...`

## The design

The full cancellation chain is: shutdown begins → the root context cancels → every
per-connection and per-request context derived from it cancels → each handler's
`r.Context().Done()` fires → the handler bails. `Server.BaseContext func(net.Listener)
context.Context` is what seeds that root: it returns the context that becomes the
ancestor of every request context on the server. Wire it to a cancellable root,
keep the `cancel` alongside the server, and the moment you call `cancel()` every
in-flight handler observes it.

The ordering of a graceful shutdown is therefore two steps: `cancel()` the root to
tell in-flight handlers to wind down, then `Shutdown(ctx)` to stop accepting and
wait for them to return. `Shutdown` alone (without wiring `BaseContext`) stops new
work but leaves a long-poll handler blocked until the grace deadline — which is
the bug this exercise fixes.

`Server.ConnContext func(ctx context.Context, c net.Conn) context.Context` runs
once per accepted connection and lets you stamp a per-connection value (here a tag;
in production a connection id or the peer address) that every request on that
connection can read. It derives from the base context, so the shutdown
cancellation still flows through it.

The long-poll handler selects on `r.Context().Done()` versus a long timer. On
cancellation it reads the `ConnContext` value, records that it observed the
shutdown, and writes a `503`-shaped response so the client sees a clean "server
draining" rather than a dropped connection. Because the handler returns promptly,
`Shutdown` finds the connection idle and returns `nil`.

The exercise uses a manual `net.Listener` + `http.Server` (not `httptest`) so the
`BaseContext`/`ConnContext`/`Shutdown` lifecycle is explicit and fully under the
test's control.

Create `server.go`:

```go
package shutdownlink

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

type connKeyType int

const connKey connKeyType = 0

// ConnTagFrom returns the per-connection tag stamped by ConnContext, or "".
func ConnTagFrom(ctx context.Context) string {
	if v, ok := ctx.Value(connKey).(string); ok {
		return v
	}
	return ""
}

// Server bundles an *http.Server with the root-cancel that drives graceful
// shutdown and a channel the long-poll handler uses to report what it observed.
type Server struct {
	HTTP       *http.Server
	cancelRoot context.CancelFunc
	Observed   chan string
}

// NewServer builds a server whose BaseContext is a cancellable root and whose
// ConnContext stamps connTag on every connection. The long-poll handler waits
// on r.Context().Done(); when the root is cancelled at shutdown, it reports the
// connection tag it saw on Observed and returns a draining response.
func NewServer(connTag string) *Server {
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	observed := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			observed <- ConnTagFrom(r.Context())
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "draining")
		case <-time.After(10 * time.Second):
			_, _ = io.WriteString(w, "poll result")
		}
	})

	return &Server{
		HTTP: &http.Server{
			Handler:     mux,
			BaseContext: func(net.Listener) context.Context { return rootCtx },
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				return context.WithValue(ctx, connKey, connTag)
			},
		},
		cancelRoot: cancelRoot,
		Observed:   observed,
	}
}

// Shutdown performs the graceful sequence: cancel the root so in-flight handlers
// observe the shutdown, then wait for them to drain.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancelRoot()
	return s.HTTP.Shutdown(ctx)
}
```

## The runnable demo

The demo starts the server on a real loopback listener, fires a long-poll in a
goroutine, then shuts down — and prints what the handler observed plus the drain
status the client received.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"example.com/shutdownlink"
)

func main() {
	srv := shutdownlink.NewServer("conn-tag")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	go func() { _ = srv.HTTP.Serve(ln) }()
	url := "http://" + ln.Addr().String() + "/poll"

	statusCh := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			statusCh <- -1
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		statusCh <- resp.StatusCode
	}()

	time.Sleep(50 * time.Millisecond) // let the poll reach the handler

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = srv.Shutdown(ctx)

	fmt.Printf("observed_tag=%s client_status=%d shutdown_err=%v\n", <-srv.Observed, <-statusCh, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
observed_tag=conn-tag client_status=503 shutdown_err=<nil>
```

## Tests

`TestShutdownCancelsInflightHandler` is the end-to-end contract: fire a long-poll,
let it reach the handler, call `Shutdown`, and assert the handler observed the
cancellation (the `ConnContext` tag arrives on `Observed`), the client got the
`503` draining response, and `Shutdown` returned `nil`. That single test proves
the whole chain — base-ctx cancel → request-ctx cancel → handler bails — and that
`ConnContext` values reach the handler.

Create `server_test.go`:

```go
package shutdownlink

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownCancelsInflightHandler(t *testing.T) {
	t.Parallel()

	srv := NewServer("conn-tag")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.HTTP.Serve(ln) }()
	url := "http://" + ln.Addr().String() + "/poll"

	type clientResult struct {
		status int
		err    error
	}
	clientCh := make(chan clientResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			clientCh <- clientResult{err: err}
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			clientCh <- clientResult{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		clientCh <- clientResult{status: resp.StatusCode}
	}()

	// Give the poll time to reach the handler and park on r.Context().Done().
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case tag := <-srv.Observed:
		if tag != "conn-tag" {
			t.Fatalf("handler saw ConnContext tag %q, want conn-tag", tag)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed shutdown cancellation")
	}

	select {
	case res := <-clientCh:
		if res.err != nil {
			t.Fatalf("client error: %v", res.err)
		}
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("client status = %d, want 503 (draining)", res.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client never received the draining response")
	}
}
```

## Review

The wiring is correct when `BaseContext` returns a root you cancel at shutdown, so
`Shutdown`'s "stop accepting and wait" is preceded by an explicit signal that
in-flight handlers actually observe. The test proves the full chain end to end:
the handler parked on `r.Context().Done()` wakes the instant the root is
cancelled, reads its `ConnContext` tag, and drains with a `503` — and `Shutdown`
returns `nil` because the handler returned promptly rather than hanging to the
grace deadline. The mistake this fixes is the common one of calling `Shutdown`
without wiring `BaseContext`: new connections are refused, but a long-poll handler
blocks until it is force-closed, turning a clean deploy into dropped connections.
Run with `-race`; the observed and client results travel over buffered channels.

## Resources

- [`http.Server.BaseContext`](https://pkg.go.dev/net/http#Server) — the root context field seeding every request context.
- [`http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown semantics and its return contract.
- [`net.Listen`](https://pkg.go.dev/net#Listen) — the explicit loopback listener the server runs on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-bounded-body-read-guard.md](09-bounded-body-read-guard.md) | Next: [../10-context-aware-database-queries/00-concepts.md](../10-context-aware-database-queries/00-concepts.md)
