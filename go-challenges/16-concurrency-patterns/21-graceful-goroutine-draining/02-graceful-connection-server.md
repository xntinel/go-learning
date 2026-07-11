# Exercise 2: The Graceful Connection Server

A network server is where draining earns its keep: a deploy or a scale-down sends the process a stop signal while requests are mid-flight, and a graceful server must stop accepting new connections, let the in-flight handlers finish replying, and only force the stragglers closed once a deadline has passed. This exercise builds that server over raw TCP — one accept loop, one goroutine per connection, a `WaitGroup` join, and an interruptible handler — so every goroutine is visible and the drain can be asserted leak-free.

This module is fully self-contained: its own `go mod init`, its own demo, and its own tests, importing nothing from the other exercises.

## What you'll build

```text
gracefulsrv/
  go.mod
  server.go            Server, New, Addr, Shutdown; accept loop, per-conn
                       handler, WaitGroup join, deadline + force, ErrForced
  cmd/
    demo/
      main.go          serve a few requests, then drain cleanly
  server_test.go       in-flight request drains within the deadline, a too-short
                       deadline forces, new connections refused, no leak
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `New() (*Server, error)`, `(*Server).Addr() string`, `(*Server).Shutdown(context.Context) error`.
- Test: an in-flight handler completes and replies when the deadline is generous; a too-short deadline returns `ErrForced` and drops the connection; new connections are refused after `Shutdown`; the goroutine count returns to baseline.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p gracefulsrv/cmd/demo && cd gracefulsrv
go mod init example.com/gracefulsrv
```

## The protocol and the shape of the drain

To keep the focus on draining rather than parsing, the wire protocol is one line per connection: the client sends `"<milliseconds>\n"` naming how long its request should take, and the handler replies `"ok\n"` once that much simulated work has elapsed. This is the smallest protocol that still has a property real servers have and toy examples usually lack: the work is *interruptible*. The handler does not `time.Sleep`; it `select`s on `time.After(d)` against a force channel, so the server can actually abort it. A handler built on a bare `Sleep` would ignore every attempt to force it and leak past shutdown — the force would be cosmetic.

The server has exactly three kinds of goroutine, and the `WaitGroup` counts all of them. One accept-loop goroutine pulls connections off the listener; one handler goroutine per live connection; and, during `Shutdown`, one short-lived goroutine that waits on the `WaitGroup` and closes a `done` channel so the wait can be raced against a deadline. The phases map cleanly onto these. Stop-accepting is `s.ln.Close()`: a closed listener makes `Accept` return an error, the accept loop returns, and the kernel refuses further connections to that port — new work is turned away instantly. Drain-in-flight is `wg.Wait()` selected against `ctx.Done()`: if every handler finishes before the deadline, `Shutdown` returns nil. Force-exit is closing the `force` channel: every handler's `select` immediately takes the force branch, returns, closes its connection (which signals the client), and the `WaitGroup` reaches zero — so even the forced path leaves no goroutine behind.

Two closes must each happen exactly once even though `Shutdown` may be called more than once or from more than one goroutine, so each is wrapped in its own `sync.Once`: one for the listener close, one for the force-channel close. Closing either twice would panic.

Create `server.go`:

```go
// server.go
package gracefulsrv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrForced is returned by Shutdown when the drain deadline expired and the
// in-flight handlers had to be aborted instead of finishing on their own.
var ErrForced = errors.New("gracefulsrv: drain deadline exceeded, in-flight work forced to stop")

// Server is a small TCP server with graceful draining. Each connection carries
// one request, the line "<milliseconds>\n", naming how long the handler should
// work; the handler replies "ok\n" when the work completes, or drops the
// connection without replying if the server is forced to stop first.
type Server struct {
	ln       net.Listener
	wg       sync.WaitGroup
	force    chan struct{}
	closeLn  sync.Once
	forceОne sync.Once
}

// New starts a Server on an automatically chosen 127.0.0.1 port and begins
// accepting connections. Use Addr to discover the bound address.
func New() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gracefulsrv: listen: %w", err)
	}
	s := &Server{ln: ln, force: make(chan struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr returns the address the server is listening on, e.g. "127.0.0.1:54321".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// acceptLoop accepts connections until the listener is closed by Shutdown.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed: stop accepting new work
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

// handle serves one request on conn. The simulated work is interruptible: it
// waits on a timer for the requested duration but also on the force channel, so
// Shutdown can abort it once the drain deadline passes.
func (s *Server) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	ms, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		fmt.Fprintln(conn, "error: want an integer millisecond count")
		return
	}

	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
		fmt.Fprintln(conn, "ok")
	case <-s.force:
		return // forced: drop the connection; the deferred Close signals the client
	}
}

// Shutdown stops accepting new connections and waits for in-flight handlers to
// finish. If ctx expires before they do, Shutdown forces every in-flight handler
// to abort and returns ErrForced. It is safe to call multiple times.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeLn.Do(func() { _ = s.ln.Close() })

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.forceОne.Do(func() { close(s.force) })
		<-done // handlers abort promptly now that force is closed
		return ErrForced
	}
}
```

Notice that `Shutdown` does not return the instant the deadline fires. It closes the `force` channel and then waits on `done` again — now a fast wait, because every handler unblocks immediately — before returning `ErrForced`. That second wait is what makes "forced" honest: the call returns only once the forced goroutines are actually gone, so a leak check right after `Shutdown` sees a clean slate on both the graceful and the forced path.

### The runnable demo

The demo serves three quick sequential requests against its own address, then drains with a generous deadline so the path is clean and the output deterministic.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"example.com/gracefulsrv"
)

func roundTrip(addr, body string) (string, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", body); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func main() {
	s, err := gracefulsrv.New()
	if err != nil {
		fmt.Println("start error:", err)
		return
	}

	served := 0
	for range 3 {
		out, err := roundTrip(s.Addr(), "10")
		if err != nil || out != "ok" {
			fmt.Printf("request failed: out=%q err=%v\n", out, err)
			continue
		}
		served++
	}
	fmt.Printf("served %d requests\n", served)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		fmt.Println("drain forced:", err)
		return
	}
	fmt.Println("drained cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
served 3 requests
drained cleanly
```

### Tests

The tests drive the server over a real loopback socket. The drain test starts a slow request, waits until it is on the wire and the handler has entered its work `select`, then shuts down with a generous deadline and asserts the in-flight request still received its `"ok"` — and that a fresh connection is refused afterward. The force test starts a five-second request and shuts down with a fifty-millisecond deadline, asserting `ErrForced` and that the client's read fails because the handler dropped the connection. The leak test runs a clean round trip, shuts down, and asserts the goroutine count returns to baseline; it is not parallel, so the count is stable.

Create `server_test.go`:

```go
// server_test.go
package gracefulsrv_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"example.com/gracefulsrv"
)

func mustNew(t *testing.T) *gracefulsrv.Server {
	t.Helper()
	s, err := gracefulsrv.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func roundTrip(addr, body string) (string, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", body); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func TestDrainsInFlightWithinDeadline(t *testing.T) {
	t.Parallel()
	s := mustNew(t)

	type result struct {
		out string
		err error
	}
	resCh := make(chan result, 1)
	onWire := make(chan struct{})
	go func() {
		conn, err := net.Dial("tcp", s.Addr())
		if err != nil {
			close(onWire)
			resCh <- result{"", err}
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%d\n", 150)
		close(onWire) // request is sent; the handler is about to start working
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		resCh <- result{strings.TrimSpace(line), err}
	}()

	<-onWire
	time.Sleep(20 * time.Millisecond) // let the handler enter its work select

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	r := <-resCh
	if r.err != nil {
		t.Fatalf("in-flight request failed: %v", r.err)
	}
	if r.out != "ok" {
		t.Errorf("in-flight response = %q, want %q", r.out, "ok")
	}
	if _, err := net.Dial("tcp", s.Addr()); err == nil {
		t.Error("expected a new connection to be refused after Shutdown")
	}
}

func TestForcesWhenDeadlineExceeded(t *testing.T) {
	t.Parallel()
	s := mustNew(t)

	errCh := make(chan error, 1)
	onWire := make(chan struct{})
	go func() {
		conn, err := net.Dial("tcp", s.Addr())
		if err != nil {
			close(onWire)
			errCh <- err
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%d\n", 5000) // five seconds of work
		close(onWire)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = bufio.NewReader(conn).ReadString('\n')
		errCh <- err // expect a non-nil error: the handler dropped the connection
	}()

	<-onWire
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, gracefulsrv.ErrForced) {
		t.Fatalf("Shutdown = %v, want ErrForced", err)
	}
	if err := <-errCh; err == nil {
		t.Error("expected the forced handler to drop the connection without replying")
	}
}

func TestNoGoroutineLeakAfterShutdown(t *testing.T) {
	// Not parallel: NumGoroutine is only stable when no parallel test runs.
	base := runtime.NumGoroutine()

	s := mustNew(t)
	out, err := roundTrip(s.Addr(), "10")
	if err != nil || out != "ok" {
		t.Fatalf("roundTrip: out=%q err=%v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	assertNoLeak(t, base)
}

// assertNoLeak polls until the goroutine count returns to base, allowing for the
// asynchronous teardown of goroutines that have already returned.
func assertNoLeak(t *testing.T, base int) {
	t.Helper()
	for range 100 {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline %d, now %d", base, runtime.NumGoroutine())
}
```

## Review

The server is correct when, after `Shutdown` returns, no connection is being accepted and no handler goroutine survives — on both the graceful and the forced path. Closing the listener is the stop-accepting phase, and it is load-bearing that this is the *only* thing that turns away new work: the kernel refuses connections to a closed listening socket, which the "new connection refused" assertion checks. The drain is `wg.Wait()` raced against the context, and the force is closing the `force` channel; the handler must wait on that channel rather than sleeping, or the force does nothing and the goroutine leaks — the force test, which asserts the client's read fails, is what keeps the handler interruptible. The subtlety most easily gotten wrong is returning `ErrForced` the instant the deadline fires without waiting for the forced handlers to actually exit; the second `<-done` after closing `force` is what makes the leak assertion valid immediately after `Shutdown`. Run under `go test -race`: the detector proves the `WaitGroup`, the two `sync.Once` closes, and the `force` broadcast are synchronized, and the non-parallel leak test proves the accept loop and every handler are gone once the drain completes.

## Resources

- [`net.Listener`](https://pkg.go.dev/net#Listener) — `Accept` returns an error once the listener is closed, which is how the accept loop terminates.
- [`net/http` Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the standard library's production take on this exact pattern: stop accepting, drain idle connections, honour a context deadline.
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) — explicit cancellation channels for fanned-out goroutines, the basis of the `force` broadcast.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — the drain budget that decides graceful versus forced.

---

Back to [01-draining-worker-pool.md](01-draining-worker-pool.md) | Next: [03-signal-driven-worker.md](03-signal-driven-worker.md)
</content>
