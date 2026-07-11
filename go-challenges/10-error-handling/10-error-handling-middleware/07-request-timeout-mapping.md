# Exercise 7: Enforce per-request timeouts and map deadline errors

A request that runs too long is a failure with its own status. This exercise
builds a middleware that derives a per-request `context.WithTimeout`, a handler
that honors `r.Context()`, and an error boundary that maps
`context.DeadlineExceeded` to 504 Gateway Timeout — then contrasts it with
`http.TimeoutHandler` and explains why `TimeoutHandler` cannot rescue a handler
that already began streaming.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
reqtimeout/                  independent module: example.com/reqtimeout
  go.mod                     go 1.26
  timeout.go                 Timeout middleware; deadline-aware handler; WriteError maps 504
  cmd/
    demo/
      main.go                runnable demo: a slow handler times out -> 504
  timeout_test.go            ctx-timeout -> 504 (DeadlineExceeded); TimeoutHandler 503 body
```

Files: `timeout.go`, `cmd/demo/main.go`, `timeout_test.go`.
Implement: `Timeout(d)` middleware wrapping `r` in `context.WithTimeout`; a handler
that selects on `ctx.Done()`; a boundary mapping `context.DeadlineExceeded` to 504.
Test: a handler sleeping past a short timeout -> 504 and the mapped error is
`context.DeadlineExceeded`; a slow handler under `http.TimeoutHandler` -> its 503
body. Run with `-race` and `t.Context()`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqtimeout/cmd/demo
cd ~/go-exercises/reqtimeout
go mod init example.com/reqtimeout
```

### Two ways to time out, and why they differ

There are two mechanisms in the stdlib for bounding a request, and they are not
interchangeable.

The **context-based** approach: a middleware calls `ctx, cancel :=
context.WithTimeout(r.Context(), d)`, replaces the request context with
`r.WithContext(ctx)`, and `defer cancel()`. The handler is now responsible for
honoring the deadline — it selects on `ctx.Done()` around slow work (a DB call, an
upstream request, a sleep). When the deadline fires, `ctx.Err()` becomes
`context.DeadlineExceeded`, the handler returns that error, and the boundary maps
it to 504 Gateway Timeout. The strength: the same context flows into every
downstream call, so a slow database query is cancelled too, not just abandoned.
The cost: a handler that ignores `ctx.Done()` runs to completion regardless — the
context does not forcibly stop it. Timeouts are cooperative.

Why 504 and not 503? 504 Gateway Timeout means "I, acting as a gateway to some
work, did not get a timely response." A per-request deadline on downstream work is
exactly that. (`http.TimeoutHandler` uses 503 Service Unavailable by default,
which is also defensible; the point is to choose deliberately and map
consistently.)

The **`http.TimeoutHandler`** approach: `http.TimeoutHandler(next, d, msg)` wraps
a handler, runs it on a separate goroutine, and substitutes its *own* buffered
`ResponseWriter`. If the handler finishes first, the buffer is flushed to the real
writer. If the deadline fires first, `TimeoutHandler` discards the buffer and
writes a 503 with `msg`. This is convenient and requires nothing of the handler —
but it has a hard limitation rooted in *how* it works. Because it buffers the whole
response to be able to throw it away on timeout, it **cannot** handle a handler
that has already *started streaming* to the real connection: once bytes are
committed to the client there is nothing to buffer and nothing to discard, and the
stdlib documents that `TimeoutHandler` does not support the `http.Flusher` /
`http.Hijacker` a streaming handler needs. So `TimeoutHandler` fits
buffered-response endpoints; context timeouts fit streaming and downstream-cancel
scenarios.

This module builds the context approach as the primary artifact and demonstrates
`http.TimeoutHandler` in a test for contrast.

Create `timeout.go`:

```go
package reqtimeout

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidInput = errors.New("invalid input")
)

// Handler returns an error instead of writing its own failure response.
type Handler func(w http.ResponseWriter, r *http.Request) error

// WithError adapts a Handler and routes non-nil errors to WriteError.
func WithError(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, err)
		}
	})
}

// WriteError maps errors to statuses, including context.DeadlineExceeded -> 504.
func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalidInput):
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status)})
}

// Timeout derives a per-request context deadline. Handlers must honor
// r.Context().Done() for the deadline to take effect.
func Timeout(d time.Duration) func(Handler) Handler {
	return func(next Handler) Handler {
		return func(w http.ResponseWriter, r *http.Request) error {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			return next(w, r.WithContext(ctx))
		}
	}
}

// SlowWork simulates a handler doing bounded work that respects cancellation.
// It returns ctx.Err() (DeadlineExceeded) if the deadline fires first.
func SlowWork(work time.Duration) Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		select {
		case <-time.After(work):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"result": "done"})
			return nil
		case <-r.Context().Done():
			return r.Context().Err() // context.DeadlineExceeded on timeout
		}
	}
}
```

### The runnable demo

The demo wraps a 200ms unit of work in a 50ms timeout, so the deadline fires
first and the boundary writes a 504.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"example.com/reqtimeout"
)

func main() {
	slow := reqtimeout.Timeout(50 * time.Millisecond)(reqtimeout.SlowWork(200 * time.Millisecond))
	srv := httptest.NewServer(reqtimeout.WithError(slow))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=504 body={"error":"Gateway Timeout"}
```

### Tests

`TestContextTimeoutMapsTo504` uses `t.Context()` as the base, wraps 200ms of work
in a 20ms timeout, and asserts both the 504 status and that the handler's returned
error `errors.Is` `context.DeadlineExceeded`. `TestTimeoutHandlerWritesServiceUnavailable`
wraps a slow plain handler in `http.TimeoutHandler` and asserts the 503 and its
custom body, showing the alternative mechanism. Both run under `-race`.

Create `timeout_test.go`:

```go
package reqtimeout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestContextTimeoutMapsTo504(t *testing.T) {
	t.Parallel()

	var handlerErr error
	inner := SlowWork(200 * time.Millisecond)
	wrapped := Timeout(20 * time.Millisecond)(func(w http.ResponseWriter, r *http.Request) error {
		handlerErr = inner(w, r)
		return handlerErr
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(t.Context())
	WithError(wrapped).ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if !errors.Is(handlerErr, context.DeadlineExceeded) {
		t.Fatalf("handler error = %v, want context.DeadlineExceeded", handlerErr)
	}
}

func TestTimeoutHandlerWritesServiceUnavailable(t *testing.T) {
	t.Parallel()

	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			w.Write([]byte("done"))
		case <-r.Context().Done():
		}
	})
	h := http.TimeoutHandler(slow, 20*time.Millisecond, "request timed out")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(t.Context())
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "request timed out") {
		t.Fatalf("body = %q, want it to contain the timeout message", string(body))
	}
}

func ExampleWriteError() {
	rec := httptest.NewRecorder()
	WriteError(rec, context.DeadlineExceeded)
	fmt.Println(rec.Code)
	// Output: 504
}
```

## Review

The context timeout is correct when the deadline both stops the handler (it selects
on `ctx.Done()`) and surfaces as `context.DeadlineExceeded`, which the boundary
maps to 504 — `TestContextTimeoutMapsTo504` asserts both halves, and if the
mapping ever falls through to 500 someone compared with `==` instead of
`errors.Is`. The `http.TimeoutHandler` test pins the alternative: a 503 with a
custom body, produced without the handler cooperating. The trade-off is the lesson:
`TimeoutHandler` is turnkey but buffers the whole response and cannot rescue a
streaming handler; context timeouts require the handler to honor cancellation but
compose with streaming and propagate to downstream calls. Use `t.Context()` so the
test's own deadline governs the harness, and run `-race` because the timeout fires
on the timer goroutine while the handler runs on another.

## Resources

- [`context#WithTimeout`](https://pkg.go.dev/context#WithTimeout) — deriving a per-request deadline and `context.DeadlineExceeded`.
- [`net/http#TimeoutHandler`](https://pkg.go.dev/net/http#TimeoutHandler) — the buffered-writer alternative and its streaming limitation.
- [`net/http#StatusGatewayTimeout`](https://pkg.go.dev/net/http#StatusGatewayTimeout) — the 504 the boundary maps deadlines to.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-goroutine-panic-trap.md](08-goroutine-panic-trap.md)
