# Exercise 3: Preserving Flush and Hijack Through a Wrapper

The fix for the single most common composition bug in Go HTTP middleware. When you
embed `http.ResponseWriter`, your wrapper stops satisfying `http.Flusher` and
`http.Hijacker`, so streaming, Server-Sent Events, and WebSocket upgrades break.
This module shows the failure and the modern remedy: expose
`Unwrap() http.ResponseWriter` and reach optional behavior through
`http.NewResponseController`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
streamwriter/               independent module: example.com/streamwriter
  go.mod                    go 1.26
  streamwriter.go           naiveWriter (broken), metricsWriter with Unwrap, Handler, StreamEvents
  cmd/
    demo/
      main.go               show the wrapper is not a Flusher, then stream+flush via the controller
  streamwriter_test.go      naive wrapper fails http.Flusher; controller.Flush works through Unwrap
```

- Files: `streamwriter.go`, `cmd/demo/main.go`, `streamwriter_test.go`.
- Implement: a `metricsWriter` embedding `http.ResponseWriter` that captures the status AND exposes `Unwrap() http.ResponseWriter`; a `StreamEvents` helper that flushes each event via `http.NewResponseController`; a `Handler` middleware.
- Test: assert a naive wrapper does NOT satisfy `http.Flusher`; assert `http.NewResponseController(w).Flush()` returns nil and sets the recorder's `Flushed` flag through the `Unwrap` seam.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/05-interface-composition-and-embedding/03-responsecontroller-forwarding/cmd/demo
cd go-solutions/08-interfaces/05-interface-composition-and-embedding/03-responsecontroller-forwarding
```

### The bug, stated precisely

An `http.ResponseWriter` handed to a handler by `net/http` almost always *also*
implements `http.Flusher` (needed to push bytes before the handler returns, which
is how SSE and streaming work) and often `http.Hijacker` (needed to take over the
raw TCP connection for a WebSocket upgrade). These are *optional* interfaces: not
part of `http.ResponseWriter`, but discoverable with a type assertion. The moment
you wrap the writer in `type metricsWriter struct { http.ResponseWriter }`, your
struct promotes only the three `ResponseWriter` methods. It does not inherit the
underlying value's `Flush` or `Hijack`. So `w.(http.Flusher)` on your wrapper
returns `ok == false`, the handler's `flusher.Flush()` path is skipped, and
streaming silently degrades to a buffered response that arrives all at once (or
never, for an open-ended SSE stream).

The pre-1.20 fix was to forward each optional interface by hand. The modern fix,
`http.NewResponseController(w)` (Go 1.20+), is cleaner and composes through nested
wrappers: it calls `Flush`/`Hijack`/`SetReadDeadline` by *unwrapping* the writer.
If your wrapper exposes `Unwrap() http.ResponseWriter`, the controller follows it
to the real writer that implements the optional interface. So the rule is: wrap the
writer for your instrumentation, add `Unwrap`, and never type-assert the wrapper
for optional behavior — go through the controller.

Create `streamwriter.go`:

```go
package streamwriter

import (
	"fmt"
	"net/http"
)

// naiveWriter is the WRONG pattern: it wraps http.ResponseWriter without exposing
// Unwrap, so http.NewResponseController cannot reach the underlying http.Flusher
// and streaming silently breaks. It exists only to demonstrate the bug in tests.
type naiveWriter struct {
	http.ResponseWriter
}

// metricsWriter is the correct pattern: it captures the status code AND exposes
// Unwrap, so http.NewResponseController can traverse it to reach optional
// interfaces such as http.Flusher and http.Hijacker.
type metricsWriter struct {
	http.ResponseWriter
	status int
}

func newMetricsWriter(w http.ResponseWriter) *metricsWriter {
	return &metricsWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *metricsWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Status returns the captured status code.
func (w *metricsWriter) Status() int { return w.status }

// Unwrap returns the wrapped writer so http.NewResponseController can traverse
// nested wrappers to find optional interfaces.
func (w *metricsWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// StreamEvents writes each message as a Server-Sent Event and flushes after every
// one through http.NewResponseController, so the client receives events promptly
// even though w is a wrapper that does not itself satisfy http.Flusher.
func StreamEvents(w http.ResponseWriter, messages []string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	for _, m := range messages {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", m); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// Handler wraps h with a metricsWriter and reports the final status to record.
// Because the wrapper exposes Unwrap, handlers that stream still work.
func Handler(record func(status int), h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mw := newMetricsWriter(w)
		h.ServeHTTP(mw, r)
		record(mw.Status())
	})
}
```

### The runnable demo

`httptest.NewRecorder` implements `Flush` and records a `Flushed` flag, so it
stands in for a real streaming-capable writer. The demo confirms the wrapper is
*not* a `Flusher` directly, yet flushing still works through the controller.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/streamwriter"
)

func main() {
	rec := httptest.NewRecorder()

	handler := streamwriter.Handler(
		func(status int) { fmt.Printf("logged status=%d\n", status) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := w.(http.Flusher); ok {
				fmt.Println("wrapper is a Flusher: unexpected")
			} else {
				fmt.Println("wrapper is not a Flusher (as expected)")
			}
			if err := streamwriter.StreamEvents(w, []string{"one", "two"}); err != nil {
				fmt.Println("stream error:", err)
			}
		}),
	)

	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	fmt.Printf("flushed=%v\n", rec.Flushed)
	fmt.Printf("body=%q\n", rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrapper is not a Flusher (as expected)
logged status=200
flushed=true
body="data: one\n\ndata: two\n\n"
```

### Tests

Create `streamwriter_test.go`:

```go
package streamwriter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNaiveWrapperIsNotAFlusher(t *testing.T) {
	t.Parallel()
	// The recorder IS a Flusher, but wrapping it hides that.
	var w http.ResponseWriter = &naiveWriter{httptest.NewRecorder()}
	if _, ok := w.(http.Flusher); ok {
		t.Fatal("naiveWriter unexpectedly satisfies http.Flusher")
	}
}

func TestControllerFlushThroughUnwrap(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	mw := newMetricsWriter(rec)

	// The wrapper is not a Flusher directly...
	if _, ok := http.ResponseWriter(mw).(http.Flusher); ok {
		t.Fatal("metricsWriter should not satisfy http.Flusher directly")
	}

	// ...but the controller reaches the recorder's Flush via Unwrap.
	if err := http.NewResponseController(mw).Flush(); err != nil {
		t.Fatalf("Flush through wrapper: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("recorder was not flushed through the wrapper")
	}
}

func TestStreamEventsFlushesAndCapturesStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	var logged int
	h := Handler(
		func(status int) { logged = status },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := StreamEvents(w, []string{"a", "b"}); err != nil {
				t.Errorf("StreamEvents: %v", err)
			}
		}),
	)
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))

	if !rec.Flushed {
		t.Fatal("stream did not flush")
	}
	if got, want := rec.Body.String(), "data: a\n\ndata: b\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if logged != http.StatusOK {
		t.Fatalf("logged status = %d, want 200", logged)
	}
}
```

## Review

The lesson lands when both assertions in `TestControllerFlushThroughUnwrap` hold:
the wrapper is *not* a `Flusher` (proving the bug is real, not hypothetical), and
`http.NewResponseController(mw).Flush()` still succeeds and flushes the recorder
(proving `Unwrap` is the bridge). Delete the `Unwrap` method and the second half
fails — the controller reaches the `metricsWriter`, finds no `Flush` and no
`Unwrap`, and returns `http.ErrNotSupported`. The production takeaway: any wrapper
you put in front of `http.ResponseWriter` must expose `Unwrap`, and any streaming
code must flush through `http.NewResponseController`, never through a type
assertion on whatever writer it was handed.

## Resources

- [`http.NewResponseController`](https://pkg.go.dev/net/http#NewResponseController) — the controller, `Flush`, `Hijack`, and the `Unwrap` traversal.
- [`http.Flusher`](https://pkg.go.dev/net/http#Flusher) and [`http.Hijacker`](https://pkg.go.dev/net/http#Hijacker) — the optional interfaces a naive wrapper drops.
- [Handle Flush/Hijack via ResponseController](https://go.dev/doc/go1.20#net_http) — the Go 1.20 release note introducing the pattern.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-instrumented-responsewriter.md](02-instrumented-responsewriter.md) | Next: [04-metered-request-body.md](04-metered-request-body.md)
