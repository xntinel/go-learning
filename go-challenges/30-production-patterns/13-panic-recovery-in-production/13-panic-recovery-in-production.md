# 13. Panic Recovery in Production

A panic in one HTTP handler should not crash the entire server. In production, panics happen from nil pointer dereferences, out-of-range index accesses, and failed type assertions. The hard parts are: (a) `recover()` only works when called directly inside a deferred function — one indirection level and it returns nil; (b) `http.ErrAbortHandler` is a sentinel panic value the net/http server uses to abort a connection cleanly and must be re-panicked rather than swallowed; (c) once any bytes have been written to the `ResponseWriter`, the 500 header cannot be sent — so the middleware must track whether writing has started.

```text
panicmw/
  go.mod
  middleware.go
  safego.go
  middleware_test.go
  cmd/demo/main.go
```

## Concepts

### How recover Works

`recover()` is a built-in that stops a panicking goroutine's stack unwind and returns the value passed to `panic`. It only has an effect when called directly inside a `defer`-ed function in the same goroutine. Calling recover from a helper function called by the deferred function always returns nil. This is the most common source of broken recovery middleware.

```go
// Wrong: recover is not called directly inside defer
defer func() {
	safeRecover() // recover() inside safeRecover returns nil; panic is NOT stopped
}()

// Correct: recover called directly in the deferred closure
defer func() {
	if v := recover(); v != nil {
		// handle
	}
}()
```

### http.ErrAbortHandler

`net/http` itself uses panic as a signal. From the package docs: "To abort a handler so the client sees an interrupted response but the server doesn't log an error, panic with the value ErrAbortHandler." The server's own recovery logic checks for this value and suppresses stack-trace logging. A custom recovery middleware must re-panic when it sees `http.ErrAbortHandler`, or it breaks this mechanism and causes spurious 500 responses to reach clients.

### Tracking ResponseWriter Writes

HTTP headers can only be sent before the first body byte. If a handler writes body bytes and then panics, `WriteHeader(500)` fails silently — the status was already committed. Wrapping the `ResponseWriter` in a small struct that records whether `Write` or `WriteHeader` has been called lets the middleware skip the error response when it is too late to send one.

### Background Goroutines Are Not Covered by HTTP Middleware

HTTP middleware only covers goroutines spawned by the server to handle requests. Background goroutines launched with `go func()` have no recovery. A `SafeGo` helper wraps `go` with a deferred recovery so that a panicking background worker logs the incident and either exits cleanly or restarts.

### Failure Modes

- Swallowing `http.ErrAbortHandler` causes legitimate connection aborts to return a 500 JSON body to the client.
- Re-panicking after writing headers hangs the connection.
- Calling `recover()` one level too deep means the panic is not stopped and the process crashes.
- Logging the stack only at INFO level buries critical signals in normal traffic.

## Exercises

This is a library package (`package panicmw`), not `package main`. Verify it with `go test`.

### Exercise 1: ResponseWriter Wrapper and Recovery Middleware

Create `middleware.go`:

```go
package panicmw

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
)

// Middleware holds configuration for the HTTP panic-recovery middleware.
type Middleware struct {
	logger     *slog.Logger
	panicCount atomic.Int64
}

// New creates a Middleware with the given structured logger.
func New(logger *slog.Logger) *Middleware {
	return &Middleware{logger: logger}
}

// PanicCount returns the total number of panics recovered by this middleware.
func (m *Middleware) PanicCount() int64 {
	return m.panicCount.Load()
}

// responseWriterSpy wraps http.ResponseWriter and records whether the response
// has been started (WriteHeader or Write called).
type responseWriterSpy struct {
	http.ResponseWriter
	started bool
}

func (s *responseWriterSpy) WriteHeader(code int) {
	s.started = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *responseWriterSpy) Write(b []byte) (int, error) {
	s.started = true
	return s.ResponseWriter.Write(b)
}

// Handler wraps next and recovers from any panic that is not
// http.ErrAbortHandler. On recovery it logs the stack trace with request
// context and, if headers have not yet been sent, writes a 500 JSON response.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy := &responseWriterSpy{ResponseWriter: w}

		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// Re-panic so that net/http can abort the connection cleanly.
			if v == http.ErrAbortHandler {
				panic(v)
			}

			m.panicCount.Add(1)
			stack := debug.Stack()
			requestID := r.Header.Get("X-Request-ID")

			m.logger.Error("panic recovered",
				"error", fmt.Sprintf("%v", v),
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", requestID,
				"stack", string(stack),
			)

			if !spy.started {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":      "internal server error",
					"request_id": requestID,
				})
			}
		}()

		next.ServeHTTP(spy, r)
	})
}
```

### Exercise 2: SafeGo for Background Goroutines

Create `safego.go`:

```go
package panicmw

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// SafeGoOption configures SafeGo behavior.
type SafeGoOption func(*safeGoConfig)

type safeGoConfig struct {
	name       string
	restart    bool
	maxRetries int
	backoff    time.Duration
}

// WithName sets a descriptive name for log messages.
func WithName(name string) SafeGoOption {
	return func(c *safeGoConfig) { c.name = name }
}

// WithRestart configures the goroutine to restart after a panic up to
// maxRetries times, sleeping backoff between each attempt. If maxRetries
// is 0 the goroutine restarts without limit.
func WithRestart(maxRetries int, backoff time.Duration) SafeGoOption {
	return func(c *safeGoConfig) {
		c.restart = true
		c.maxRetries = maxRetries
		c.backoff = backoff
	}
}

// SafeGo launches fn in a new goroutine with panic recovery. If the goroutine
// panics, the panic is logged and, when WithRestart is active, fn is run again
// up to the configured retry limit.
func SafeGo(ctx context.Context, logger *slog.Logger, fn func(context.Context), opts ...SafeGoOption) {
	cfg := &safeGoConfig{name: "anonymous"}
	for _, o := range opts {
		o(cfg)
	}

	go func() {
		tries := 0
		for {
			panicked := runOnce(ctx, logger, cfg.name, fn)
			if !panicked || !cfg.restart || ctx.Err() != nil {
				return
			}
			tries++
			if cfg.maxRetries > 0 && tries >= cfg.maxRetries {
				logger.Error("goroutine exceeded max retries, stopping",
					"goroutine", cfg.name,
					"tries", tries,
				)
				return
			}
			logger.Info("restarting goroutine",
				"goroutine", cfg.name,
				"attempt", tries+1,
				"backoff", cfg.backoff.String(),
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.backoff):
			}
		}
	}()
}

// runOnce calls fn and returns true if fn panicked, false otherwise.
func runOnce(ctx context.Context, logger *slog.Logger, name string, fn func(context.Context)) (panicked bool) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		panicked = true
		logger.Error("goroutine panic recovered",
			"goroutine", name,
			"error", fmt.Sprintf("%v", v),
			"stack", string(debug.Stack()),
		)
	}()
	fn(ctx)
	return false
}
```

### Exercise 3: Test the Contract

Create `middleware_test.go`:

```go
package panicmw

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandlerReturns500OnPanic(t *testing.T) {
	t.Parallel()

	mw := New(discardLogger())
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went wrong")
	}))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "internal server error" {
		t.Fatalf(`body["error"] = %q, want "internal server error"`, body["error"])
	}
}

func TestHandlerIncreasesPanicCount(t *testing.T) {
	t.Parallel()

	mw := New(discardLogger())
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("count me")
	}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	if got := mw.PanicCount(); got != 3 {
		t.Fatalf("PanicCount() = %d, want 3", got)
	}
}

func TestHandlerPassesThroughOnNoPanic(t *testing.T) {
	t.Parallel()

	mw := New(discardLogger())
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if mw.PanicCount() != 0 {
		t.Fatalf("PanicCount() = %d, want 0", mw.PanicCount())
	}
}

func TestHandlerRepanicsOnErrAbortHandler(t *testing.T) {
	t.Parallel()

	mw := New(discardLogger())
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		v := recover()
		if v != http.ErrAbortHandler {
			t.Errorf("expected ErrAbortHandler to be re-panicked, got %v", v)
		}
		if mw.PanicCount() != 0 {
			t.Errorf("PanicCount() = %d, want 0 (ErrAbortHandler is not counted)", mw.PanicCount())
		}
	}()

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
}

func TestHandlerSkipsBodyWhenHeadersAlreadySent(t *testing.T) {
	t.Parallel()

	mw := New(discardLogger())
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("after write")
	}))

	req := httptest.NewRequest(http.MethodGet, "/partial", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// The middleware must not overwrite the already-committed 200.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already sent)", rec.Code)
	}
}

func TestHandlerSetsRequestIDInBody(t *testing.T) {
	t.Parallel()

	mw := New(discardLogger())
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("with request id")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-abc-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["request_id"] != "req-abc-123" {
		t.Fatalf(`body["request_id"] = %q, want "req-abc-123"`, body["request_id"])
	}
}

func ExampleMiddleware_Handler() {
	mw := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("demo panic")
	})
	handler := mw.Handler(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	fmt.Println(rec.Code)
	// Output:
	// 500
}
```

Your turn: add `TestSafeGoRecoversPanic` that confirms a panicking background goroutine does not crash the test. Create a channel, call `SafeGo` with a function that closes the channel and then panics, wait on the channel with a timeout, and fail if the timeout fires.

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"example.com/panicmw"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background worker that panics once to demonstrate SafeGo recovery.
	panicmw.SafeGo(ctx, logger, func(ctx context.Context) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
			panic("simulated background panic")
		}
	}, panicmw.WithName("demo-worker"), panicmw.WithRestart(1, 50*time.Millisecond))

	mw := panicmw.New(logger)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /safe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		m["key"] = "x" // nil map write panics
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"total_panics": mw.PanicCount(),
		})
	})

	srv := &http.Server{Addr: ":8080", Handler: mw.Handler(mux)}
	fmt.Fprintln(os.Stdout, "listening on :8080")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
```

Run the demo with `go run ./cmd/demo` from the module root.

## Common Mistakes

### recover Returns nil When Called Outside a Direct defer

Wrong: calling `recover()` inside a helper function that the deferred closure calls.

```go
func handlePanic() {
	if v := recover(); v != nil { // recover() always returns nil here
		// never runs
	}
}

defer handlePanic() // panic still propagates; process crashes
```

Fix: call `recover()` directly inside the deferred closure.

```go
defer func() {
	if v := recover(); v != nil { // stops the panic
		// handle
	}
}()
```

### Swallowing http.ErrAbortHandler

Wrong: treating every non-nil value from `recover()` as an unexpected error.

```go
if v := recover(); v != nil {
	// sends 500 even when the abort was intentional
	w.WriteHeader(http.StatusInternalServerError)
}
```

Fix: re-panic when the value is `http.ErrAbortHandler`.

```go
if v := recover(); v != nil {
	if v == http.ErrAbortHandler {
		panic(v)
	}
	// unexpected panic; write 500
}
```

### Writing a 500 After Headers Are Already Sent

Wrong: calling `w.WriteHeader(500)` after `w.Write` has already been called. The call silently does nothing because the status line is already on the wire.

Fix: wrap the `ResponseWriter` to track whether writing has started, and skip the error body if it has.

### Using panic for Normal Control Flow

Wrong: using `panic` to signal validation errors, not-found conditions, or other expected situations. This forces callers to use `recover` as an alternative to error returns and makes the control flow invisible.

Fix: return errors. Reserve `panic` for situations that should never happen in correct code (programmer bugs, invariant violations) and for the `http.ErrAbortHandler` signal.

## Verification

From `~/go-exercises/panicmw`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `ExampleMiddleware_Handler` function in the test file is verified automatically by `go test`.

## Summary

- `recover()` must be called directly inside a `defer`-ed closure; one level of indirection breaks it.
- Re-panic on `http.ErrAbortHandler` — it is an intentional connection-abort signal, not an error.
- Wrap `ResponseWriter` to detect whether writing has started before attempting to send a 500 body.
- `runtime/debug.Stack()` captures the full goroutine stack trace as a `[]byte`.
- `SafeGo` applies the same recovery pattern to background goroutines and supports optional auto-restart with backoff.
- Holding panic count per middleware instance (not a global variable) makes tests safe to run in parallel.

## What's Next

Next: [14. Blue-Green Deployment Patterns](../14-blue-green-deployment-patterns/14-blue-green-deployment-patterns.md).

## Resources

- [Defer, Panic, and Recover — The Go Blog](https://go.dev/blog/defer-panic-and-recover)
- [Go Wiki: PanicAndRecover](https://go.dev/wiki/PanicAndRecover)
- [runtime/debug — pkg.go.dev](https://pkg.go.dev/runtime/debug)
- [net/http — ErrAbortHandler, Handler.ServeHTTP panic semantics](https://pkg.go.dev/net/http#ErrAbortHandler)
- [builtin — recover](https://pkg.go.dev/builtin#recover)
