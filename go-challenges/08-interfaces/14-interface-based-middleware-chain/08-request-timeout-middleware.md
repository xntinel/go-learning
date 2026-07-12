# Exercise 8: Per-request deadline enforcement

Slow requests must be bounded so a stuck handler cannot hold a connection forever.
This module contrasts the two layers Go gives you: `http.TimeoutHandler` for a hard
503 cutoff, and `context.WithTimeout` for cooperative cancellation a handler can
observe to abort its own downstream work.

Fully self-contained: its own `go mod init`, demo, and tests. Nothing here imports
another exercise.

## What you'll build

```text
timeoutmw/                   independent module: example.com/timeoutmw
  go.mod                     go 1.26
  middleware.go              Timeout (wraps http.TimeoutHandler) + Deadline (context variant)
  cmd/demo/main.go           runnable demo: a slow handler yields 503
  middleware_test.go         hard 503 cutoff, fast passthrough, cooperative cancellation
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Timeout(d, msg)` wrapping `http.TimeoutHandler`, and `Deadline(d)` that derives `context.WithTimeout(r.Context(), d)`, rewraps the request, and lets handlers observe `ctx.Done()`/`ctx.Err()`.
- Test: a slow handler behind `Timeout` yields 503 and the message body; a fast handler passes through 200; behind `Deadline` a handler selecting on `ctx.Done()` returns promptly with `ctx.Err() == context.DeadlineExceeded`, and a fast handler sees no cancellation.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/14-interface-based-middleware-chain/08-request-timeout-middleware/cmd/demo
cd go-solutions/08-interfaces/14-interface-based-middleware-chain/08-request-timeout-middleware
go mod edit -go=1.26
```

### Hard cutoff vs cooperative cancellation

`http.TimeoutHandler(next, dt, msg)` is the blunt instrument. It runs `next`
against an *in-memory buffered* `ResponseWriter`; if the handler is not done by
`dt`, it discards the buffer and writes `503 Service Unavailable` with `msg`. The
guarantee is strong — the client always gets a bounded response — but the buffering
has a cost: because the whole response is held until the handler returns, it
defeats streaming. An SSE or chunked endpoint wrapped in `TimeoutHandler` cannot
flush incrementally, so you must not use it there. For bounded, non-streaming
routes it is exactly right and needs no per-handler cooperation.

`context.WithTimeout(r.Context(), d)` is the cooperative layer. It attaches a
deadline to the request context and returns a `cancel` you must always call
(`defer cancel()`) to release the timer. The middleware rewraps the request with
the new context and calls `next`; the handler itself watches `ctx.Done()` and
aborts its slow work — a database query, an upstream call — when the deadline
fires, reading `ctx.Err() == context.DeadlineExceeded` to know why. This does not
*force* a response the way `TimeoutHandler` does; a handler that ignores the
context keeps running. Its value is letting well-written handlers stop wasting work
and propagate the cancellation to the libraries they call (`database/sql`,
`net/http` client requests all honor a cancelled context). Real services layer
both: `TimeoutHandler` as an outer backstop, `Deadline` for cooperative propagation.

Create `middleware.go`:

```go
package timeoutmw

import (
	"context"
	"net/http"
	"time"
)

type Handler = http.Handler

type Middleware func(Handler) Handler

// Timeout wraps http.TimeoutHandler: if next is not done within d, the client
// gets a 503 with msg. It buffers the response, so it is unsuitable for streaming.
func Timeout(d time.Duration, msg string) Middleware {
	return func(next Handler) Handler {
		return http.TimeoutHandler(next, d, msg)
	}
}

// Deadline attaches a d-bounded context to the request so handlers can observe
// ctx.Done() and abort their own downstream work cooperatively.
func Deadline(d time.Duration) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
```

### The runnable demo

The demo puts a handler that sleeps 200 ms behind a 20 ms `Timeout`, so the client
gets a 503 with the configured message.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"example.com/timeoutmw"
)

func main() {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "done")
	})
	handler := timeoutmw.Timeout(20*time.Millisecond, "request timed out")(slow)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	fmt.Printf("status: %d\n", rec.Code)
	fmt.Printf("body contains timeout message: %t\n",
		strings.Contains(rec.Body.String(), "request timed out"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 503
body contains timeout message: true
```

### Tests

`TestTimeoutHandlerCutsOffSlow` proves a slow handler yields 503 with the message.
`TestTimeoutHandlerPassesFast` proves a fast handler returns 200 untouched.
`TestDeadlineCancelsSlowWork` proves the cooperative variant: a handler selecting
on `ctx.Done()` returns promptly and reports `context.DeadlineExceeded`.
`TestDeadlineFastHandlerNotCancelled` proves a handler finishing before the
deadline sees no cancellation. Durations are tiny to keep the test fast.

Create `middleware_test.go`:

```go
package timeoutmw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTimeoutHandlerCutsOffSlow(t *testing.T) {
	t.Parallel()

	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := Timeout(20*time.Millisecond, "timed out")(slow)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "timed out") {
		t.Fatalf("body = %q, want it to contain the timeout message", rec.Body.String())
	}
}

func TestTimeoutHandlerPassesFast(t *testing.T) {
	t.Parallel()

	fast := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Timeout(time.Second, "timed out")(fast)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestDeadlineCancelsSlowWork(t *testing.T) {
	t.Parallel()

	var errSeen error
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			errSeen = r.Context().Err()
		case <-time.After(time.Second):
		}
	})
	h := Deadline(10 * time.Millisecond)(slow)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(errSeen, context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded", errSeen)
	}
}

func TestDeadlineFastHandlerNotCancelled(t *testing.T) {
	t.Parallel()

	var errSeen error
	fast := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errSeen = r.Context().Err() // returns before any deadline fires
	})
	h := Deadline(time.Second)(fast)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if errSeen != nil {
		t.Fatalf("ctx.Err() = %v, want nil for a fast handler", errSeen)
	}
}
```

## Review

The two layers are correct when each does its own job: `Timeout` guarantees a
bounded 503 even for a handler that never cooperates (`TestTimeoutHandlerCutsOffSlow`),
while `Deadline` propagates a cancellable context that a cooperating handler
observes (`TestDeadlineCancelsSlowWork` asserting `context.DeadlineExceeded`). The
trap to remember is that `TimeoutHandler` buffers the whole response, so it is
wrong for SSE/streaming — reach for `Deadline` there. Always `defer cancel()` after
`context.WithTimeout`; skipping it leaks the timer until the deadline fires. Note
the fast-path test asserts `ctx.Err() == nil`, proving `Deadline` never cancels a
handler that finishes in time.

## Resources

- [net/http#TimeoutHandler](https://pkg.go.dev/net/http#TimeoutHandler) — the buffered hard-503 cutoff.
- [context#WithTimeout](https://pkg.go.dev/context#WithTimeout) — the cooperative deadline and its `cancel`.
- [context#Context](https://pkg.go.dev/context#Context) — `Done` and `Err`, including `DeadlineExceeded`.
- [net/http#StatusServiceUnavailable](https://pkg.go.dev/net/http#StatusServiceUnavailable) — the 503 `TimeoutHandler` returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-per-client-rate-limit-middleware.md](07-per-client-rate-limit-middleware.md) | Next: [09-cors-and-security-headers-middleware.md](09-cors-and-security-headers-middleware.md)
