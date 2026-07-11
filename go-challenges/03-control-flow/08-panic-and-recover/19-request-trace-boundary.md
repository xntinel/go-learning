# Exercise 19: Request Handler Panic Recovery with Trace Context Capture

**Nivel: Intermedio** — validacion rapida (un test corto).

A generic recovery middleware that writes a 500 and logs a stack trace is
only half the job in a service that does distributed tracing: the log line
is useless to an on-call engineer unless it carries the trace ID that
correlates it with the rest of that request's spans, and it must not turn a
deliberate stream abort into a false alarm. This module builds `Recover`, an
`http.Handler` wrapper that pulls the trace ID out of the request's
`context.Context`, records it alongside the panic and its stack through an
injectable `Logger`, and — matching the concepts file's guidance on
`http.ErrAbortHandler` — re-panics a deliberate abort instead of writing a
500 over it. It is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
tracehandler/                independent module: example.com/tracehandler
  go.mod                     go 1.24
  tracehandler.go             WithTraceID, TraceID, LogRecord, Logger, Recover
  cmd/
    demo/
      main.go                runnable demo: a panicking handler through Recover
  tracehandler_test.go         500 + logged trace ID, ErrAbortHandler re-panics, no-panic passthrough
```

Files: `tracehandler.go`, `cmd/demo/main.go`, `tracehandler_test.go`.
Implement: `Recover(logger Logger, next http.Handler) http.Handler` that captures `debug.Stack()` first, reads the trace ID via `TraceID(r.Context())`, and either re-panics an `http.ErrAbortHandler` after logging it or writes a 500 for anything else.
Test: a handler panicking with an ordinary error asserts a 500 response and exactly one `LogRecord` carrying the injected trace ID and a non-empty stack; a handler panicking with `http.ErrAbortHandler` asserts the panic re-propagates past `ServeHTTP` (caught by the test's own recover), no 500 is written, and the log record is marked `Reraised`; a handler that does not panic is a pure passthrough.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/request-trace-boundary/cmd/demo
cd ~/go-exercises/request-trace-boundary
go mod init example.com/tracehandler
go mod edit -go=1.24
```

### Why the trace ID is read inside the deferred function, and why the abort case re-panics after logging

`TraceID` is read from `r.Context()` *inside* the deferred recover, not
before it — because the whole point of this wrapper is to attach
observability to whatever panicked, and by the time the deferred function
runs, the request's context (and whatever middleware upstream attached to
it, including the trace ID) is still fully available; nothing about a panic
invalidates the context the request arrived with. `debug.Stack()` is called
as the very first thing the deferred function does, per the concepts file's
rule: any other call running first would overwrite the stack context and
the recorded trace would no longer point at the panic site.

The `http.ErrAbortHandler` branch is what makes this exercise about more
than "recover and log." `net/http` treats `panic(http.ErrAbortHandler)`
specially: it is how a streaming handler says "the client disconnected, stop
writing to this response, and do not log this as a server error." A generic
recovery wrapper that writes a 500 for every panic would try to write
headers/status onto a connection that is being deliberately abandoned, and
would pollute error-rate dashboards with what was an expected client
disconnect, not a bug. Detecting it with `errors.Is(e, http.ErrAbortHandler)`
— never `==`, so a wrapped abort still matches — lets this middleware log
the abort for visibility (`Reraised: true` in the `LogRecord`) while still
re-panicking so `net/http`'s own abort machinery runs and no 500 is ever
written over the half-finished response.

Create `tracehandler.go`:

```go
package tracehandler

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
)

type traceIDKey struct{}

// WithTraceID attaches a trace ID to ctx, the way inbound middleware
// (a load balancer header, a tracing library) would before the handler
// chain runs.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID retrieves the trace ID attached by WithTraceID, if any.
func TraceID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(traceIDKey{}).(string)
	return id, ok
}

// LogRecord is exactly what an observability pipeline needs to correlate a
// panic back to the request that caused it.
type LogRecord struct {
	TraceID  string
	Panic    any
	Stack    []byte
	Reraised bool
}

// Logger receives one LogRecord per recovered panic.
type Logger interface {
	Log(LogRecord)
}

// LoggerFunc adapts a plain function to Logger.
type LoggerFunc func(LogRecord)

func (f LoggerFunc) Log(r LogRecord) { f(r) }

// Recover wraps next with a panic boundary that captures the request's
// trace ID from context, records the panic and its stack via logger, and
// then decides between two outcomes: an http.ErrAbortHandler panic (a
// deliberate stream abort — a client disconnect, a giving-up mid-response)
// is logged as such and re-panicked so net/http's own abort machinery still
// runs and no 500 is written over a response that may be partially sent;
// any other panic gets a 500 and stays contained here, with full
// observability context preserved in the LogRecord regardless of which path
// was taken.
func Recover(logger Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			stack := debug.Stack() // capture first, before any other call
			traceID, _ := TraceID(r.Context())

			if e, ok := rec.(error); ok && errors.Is(e, http.ErrAbortHandler) {
				logger.Log(LogRecord{TraceID: traceID, Panic: rec, Stack: stack, Reraised: true})
				panic(rec)
			}

			logger.Log(LogRecord{TraceID: traceID, Panic: rec, Stack: stack})
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

A handler panics with an ordinary error; `Recover` logs it with the request's
trace ID and writes a 500.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/tracehandler"
)

func main() {
	logger := tracehandler.LoggerFunc(func(r tracehandler.LogRecord) {
		fmt.Printf("logged: trace=%s reraised=%v panic=%v\n", r.TraceID, r.Reraised, r.Panic)
	})

	boom := tracehandler.Recover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("failed to load account"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req = req.WithContext(tracehandler.WithTraceID(req.Context(), "trace-demo-1"))
	rec := httptest.NewRecorder()
	boom.ServeHTTP(rec, req)
	fmt.Printf("response status: %d\n", rec.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
logged: trace=trace-demo-1 reraised=false panic=failed to load account
response status: 500
```

### Tests

`TestRecoverWritesFiveHundredAndLogsTraceID` asserts the 500 response and
the log record's trace ID and non-empty stack. `TestRecoverRepanicsAbortHandler`
wraps the call to `ServeHTTP` in its own recover to catch the re-raised
`http.ErrAbortHandler`, and asserts the response never got a 500 written and
the log record is marked `Reraised`. `TestRecoverNoPanicIsANoOp` confirms a
clean handler passes through untouched, and the logger is never called.

Create `tracehandler_test.go`:

```go
package tracehandler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverWritesFiveHundredAndLogsTraceID(t *testing.T) {
	var records []LogRecord
	logger := LoggerFunc(func(r LogRecord) { records = append(records, r) })

	handler := Recover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("nil pointer somewhere deep"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = req.WithContext(WithTraceID(req.Context(), "trace-123"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	got := records[0]
	if got.TraceID != "trace-123" {
		t.Fatalf("TraceID = %q, want trace-123", got.TraceID)
	}
	if got.Reraised {
		t.Fatal("an ordinary panic must not be marked Reraised")
	}
	if len(got.Stack) == 0 {
		t.Fatal("Stack must be captured")
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	var records []LogRecord
	logger := LoggerFunc(func(r LogRecord) { records = append(records, r) })

	handler := Recover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req = req.WithContext(WithTraceID(req.Context(), "trace-abort"))
	rec := httptest.NewRecorder()

	func() {
		defer func() {
			r := recover()
			if r != http.ErrAbortHandler {
				t.Fatalf("recovered value = %v, want http.ErrAbortHandler re-raised", r)
			}
		}()
		handler.ServeHTTP(rec, req)
		t.Fatal("expected the abort panic to propagate past ServeHTTP")
	}()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (no 500 should be written for a deliberate abort)", rec.Code, http.StatusOK)
	}
	if len(records) != 1 || !records[0].Reraised {
		t.Fatalf("records = %+v, want exactly one Reraised record", records)
	}
	if records[0].TraceID != "trace-abort" {
		t.Fatalf("TraceID = %q, want trace-abort", records[0].TraceID)
	}
}

func TestRecoverNoPanicIsANoOp(t *testing.T) {
	logger := LoggerFunc(func(LogRecord) { t.Fatal("logger should not be called") })
	handler := Recover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
}
```

## Review

`Recover` is correct when every recovered panic carries the request's trace
ID into the log record, regardless of which of the two outcomes fires, and
when an `http.ErrAbortHandler` never gets a 500 written over it. Capturing
`debug.Stack()` as the very first statement in the deferred function — before
reading the trace ID, before anything else — is what keeps the stack
trace pointing at the actual panic site; any call ahead of it in the defer
would overwrite that context. The `errors.Is` check against
`http.ErrAbortHandler`, rather than a direct `==` comparison, is what keeps
this correct even if some layer wraps the sentinel before it reaches this
boundary — a common outcome once middleware starts composing `fmt.Errorf("%w", ...)`
around recovered values.

## Resources

- [net/http: ErrAbortHandler](https://pkg.go.dev/net/http#ErrAbortHandler) — the sentinel panic value that deliberately aborts a response without logging it as a server error.
- [context.Context](https://pkg.go.dev/context) — carrying request-scoped values like a trace ID across a call chain.
- [runtime/debug: Stack](https://pkg.go.dev/runtime/debug#Stack) — capturing the goroutine's stack at the moment of recovery.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-message-batch-consumer.md](18-message-batch-consumer.md) | Next: [20-event-sourcing-replay.md](20-event-sourcing-replay.md)
