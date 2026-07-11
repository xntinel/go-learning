# Exercise 4: Stdlib TimeoutHandler vs Slow-Client Write Deadline

Two stdlib mechanisms defend a handler's runtime, and they defend against
opposite failures. `http.TimeoutHandler` bounds how long a handler may take by
cancelling its context and writing a `503` — but it cannot preempt a goroutine
already blocked inside a `Write`. `http.NewResponseController(w).SetWriteDeadline`
sets a real socket deadline, which is the only thing that unblocks a `Write`
stalled by a slow-reader (slowloris) client. This exercise builds a harness that
demonstrates both and pins the boundary between them.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
writedeadline/             independent module: example.com/writedeadline
  go.mod                   go 1.26
  guard.go                 Timeout(h, dt, msg) wrapper; FlushWithDeadline helper
  cmd/
    demo/
      main.go              a 503 from TimeoutHandler, a deadline error from a flush
  guard_test.go            503-body test, write-deadline error-class test
```

Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
Implement: `Timeout(h http.Handler, dt time.Duration, msg string) http.Handler` (a thin, documented wrapper over `http.TimeoutHandler`) and `FlushWithDeadline(w http.ResponseWriter, deadline time.Time, payload string) error` built on `http.NewResponseController`.
Test: a handler behind `Timeout` asserts `503`+message body on deadline; `FlushWithDeadline` with an already-passed deadline asserts the returned error is an `os.ErrDeadlineExceeded`-class (net timeout) error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/writedeadline/cmd/demo
cd ~/go-exercises/writedeadline
go mod init example.com/writedeadline
```

## The design

`http.TimeoutHandler(h, dt, msg)` runs `h` in a goroutine under a context derived
from `r.Context()` with timeout `dt`. If `h` overruns, `TimeoutHandler` writes
`503 Service Unavailable` with `msg` as the body and flips `h`'s response writer
to return `ErrHandlerTimeout` on further writes. The critical limitation, and the
whole reason this exercise pairs it with a write deadline: `TimeoutHandler`
cancels the context but does not — cannot — stop `h`'s goroutine. If `h` is parked
inside `Write` (because the client is reading the response one byte at a time and
the kernel send buffer is full), cancelling the context does nothing; the
goroutine stays blocked in the syscall. A handler that only checks `r.Context()`
between operations, not during a blocking `Write`, is defenseless against a
slow reader.

The defense is a socket deadline. `http.NewResponseController(w)` returns a
controller that can reach the underlying connection;
`rc.SetWriteDeadline(deadline)` sets a real write deadline on it, and `rc.Flush()`
forces buffered bytes to the socket. Once the deadline passes, the next write to
the connection returns an `os.ErrDeadlineExceeded`-class error (a `net.Error`
whose `Timeout()` reports true) instead of blocking forever — so a slow reader
can stall one response but cannot pin the goroutine.

In production you set the deadline in the *future* (`time.Now().Add(writeTimeout)`)
so a healthy client has time to read. To make the mechanism *deterministic* in a
test — without having to fill an OS send buffer, which depends on kernel tuning —
`FlushWithDeadline` accepts the deadline as a parameter and the test passes an
already-passed instant. With a past deadline, the very first flush to the socket
times out immediately, regardless of how fast the client reads, which is exactly
the error class a real slow-reader stall produces.

Create `guard.go`:

```go
package writedeadline

import (
	"io"
	"net/http"
	"time"
)

// Timeout wraps h so a request that runs longer than dt gets a 503 with msg as
// its body. It is a thin, documented pass-through to http.TimeoutHandler: the
// point of the wrapper is the contract in this comment. TimeoutHandler cancels
// the handler's r.Context() when dt elapses, but it cannot preempt a goroutine
// already blocked inside a Write or a syscall; for the write side, use a socket
// deadline (see FlushWithDeadline).
func Timeout(h http.Handler, dt time.Duration, msg string) http.Handler {
	return http.TimeoutHandler(h, dt, msg)
}

// FlushWithDeadline writes payload to w under a real socket write deadline, then
// flushes. If deadline has passed (or the peer is too slow to drain the send
// buffer before it), the flush returns an os.ErrDeadlineExceeded-class error
// (a net.Error reporting Timeout) instead of blocking forever. In production
// pass a future deadline (time.Now().Add(writeTimeout)); the caller may pass a
// past instant to force the timeout deterministically.
func FlushWithDeadline(w http.ResponseWriter, deadline time.Time, payload string) error {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if _, err := io.WriteString(w, payload); err != nil {
		return err
	}
	return rc.Flush()
}
```

## The runnable demo

The demo shows both mechanisms end to end: a handler that sleeps past the
`Timeout` limit yields a `503`, and a handler that flushes under an already-passed
deadline reports a timeout error class.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"example.com/writedeadline"
)

func main() {
	// 1) TimeoutHandler: a slow handler yields a 503 with the configured body.
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(writedeadline.Timeout(slow, 30*time.Millisecond, "handler timed out"))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("timeouthandler: status=%d body=%q\n", resp.StatusCode, string(body))

	// 2) Write deadline: a flush under a past deadline reports a timeout error.
	errCh := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errCh <- writedeadline.FlushWithDeadline(w, time.Now().Add(-time.Second), "payload")
	})
	srv2 := httptest.NewServer(h)
	defer srv2.Close()

	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv2.URL, nil)
	if resp2, err := http.DefaultClient.Do(req2); err == nil {
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
	}
	err := <-errCh
	var ne net.Error
	timeout := errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	fmt.Printf("writedeadline: err!=nil=%v timeout-class=%v\n", err != nil, timeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeouthandler: status=503 body="handler timed out"
writedeadline: err!=nil=true timeout-class=true
```

## Tests

`TestTimeoutHandlerReturns503` asserts the `503` and the exact message body on a
handler that overruns the limit (the handler also selects on `r.Context().Done()`
so it exits once `TimeoutHandler` cancels it, proving the cancel-context claim and
avoiding a leaked goroutine). `TestWriteDeadlineErrorsOnPastDeadline` captures the
error returned by `FlushWithDeadline` under a past deadline and asserts it is a
timeout-class error — the same class a real slow-reader stall produces.

Create `guard_test.go`:

```go
package writedeadline

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestTimeoutHandlerReturns503(t *testing.T) {
	t.Parallel()

	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
			_, _ = io.WriteString(w, "late")
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(Timeout(slow, 30*time.Millisecond, "handler timed out"))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if string(body) != "handler timed out" {
		t.Fatalf("body = %q, want %q", body, "handler timed out")
	}
}

func TestWriteDeadlineErrorsOnPastDeadline(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errCh <- FlushWithDeadline(w, time.Now().Add(-time.Second), "payload")
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	flushErr := <-errCh
	if flushErr == nil {
		t.Fatal("FlushWithDeadline returned nil, want a timeout-class error")
	}
	var ne net.Error
	if !errors.Is(flushErr, os.ErrDeadlineExceeded) && !(errors.As(flushErr, &ne) && ne.Timeout()) {
		t.Fatalf("err = %v, want an os.ErrDeadlineExceeded-class (net timeout) error", flushErr)
	}
}
```

## Review

The pairing is the lesson: `TimeoutHandler` is a *runtime* bound that works by
cancelling the context and writing a canned `503`, so it only stops code that
checks the context; `SetWriteDeadline` is a *socket* bound that returns a real
error from a stalled `Write`. The bug to avoid is assuming `TimeoutHandler` kills
the handler goroutine — it does not, and a handler blocked in a `Write` behind a
slow reader keeps running until the write deadline (or the client) unblocks it.
The test uses a past deadline to make the timeout deterministic without depending
on kernel send-buffer sizes; in production the deadline is a future instant sized
to your slowest legitimate client. Run with `-race`: the error channel is
buffered so the handler goroutine never blocks sending after the client goes away.

## Resources

- [`http.TimeoutHandler`](https://pkg.go.dev/net/http#TimeoutHandler) — the 503 wrapper and its `ErrHandlerTimeout` write behavior.
- [`http.NewResponseController`](https://pkg.go.dev/net/http#NewResponseController) — `SetWriteDeadline` and `Flush` on the underlying connection.
- [`os.ErrDeadlineExceeded`](https://pkg.go.dev/os#pkg-variables) — the sentinel a timed-out socket write matches under `errors.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-client-fetch-with-context.md](03-client-fetch-with-context.md) | Next: [05-cancellation-cause-classification.md](05-cancellation-cause-classification.md)
