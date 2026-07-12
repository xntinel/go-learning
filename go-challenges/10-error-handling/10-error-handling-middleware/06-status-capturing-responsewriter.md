# Exercise 6: Wrap ResponseWriter to capture status without breaking Flush

Access logs and metrics need the response status and byte count, which
`http.ResponseWriter` does not expose after the fact. This exercise builds a
`statusRecorder` that captures both, guards against a second `WriteHeader` (the
`superfluous WriteHeader` bug), and — critically — exposes `Unwrap()
http.ResponseWriter` so `http.NewResponseController` (Flush, deadlines) still
reaches the real writer.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
statusrec/                   independent module: example.com/statusrec
  go.mod                     go 1.26
  recorder.go                statusRecorder: WriteHeader guard, byte count, Unwrap; access-log middleware
  cmd/
    demo/
      main.go                runnable demo: logs method, status, bytes for two requests
  recorder_test.go           double-WriteHeader guard; Flush via NewResponseController; single status
```

Files: `recorder.go`, `cmd/demo/main.go`, `recorder_test.go`.
Implement: a `statusRecorder` wrapping `http.ResponseWriter` that records status
and bytes, ignores a second `WriteHeader`, and has `Unwrap() http.ResponseWriter`;
an `AccessLog` middleware that uses it.
Test: a handler calling `WriteHeader` twice records only the first status and does
not panic; `http.NewResponseController(recorder).Flush()` succeeds, proving
`Unwrap` works.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/10-error-handling-middleware/06-status-capturing-responsewriter/cmd/demo
cd go-solutions/10-error-handling/10-error-handling-middleware/06-status-capturing-responsewriter
```

### Why the wrapper is subtle

An `http.ResponseWriter` tells you nothing about what was written after the fact —
no status accessor, no byte count. Access-log and metrics middleware need both, so
they wrap the writer, intercepting `WriteHeader` and `Write` to record the status
code and accumulate bytes. The naive version is three lines and has two bugs.

First bug: the double `WriteHeader`. A handler that calls `w.WriteHeader(200)` and
later, on a late error, `w.WriteHeader(500)` triggers `net/http`'s `superfluous
response.WriteHeader call` — the real status stays 200 but a careless recorder
records 500, so your logs disagree with what the client received. The recorder must
mirror `net/http`'s own rule: record and forward the *first* `WriteHeader` only,
ignore the rest. A `wroteHeader bool` flag does it. And because a bare `Write`
implies `WriteHeader(200)`, `Write` must set the status to 200 if the header was
not written yet — otherwise a handler that only calls `Write` logs a status of 0.

Second bug, the dangerous one: wrapping *hides the concrete type*. `net/http`
delivers a `*http.response` that also implements `http.Flusher` (for streaming),
`http.Hijacker` (for websockets), and works with `http.NewResponseController`
(Flush, `SetWriteDeadline`, `SetReadDeadline`). Handlers reach those by type
asserting on the writer or by calling `http.NewResponseController(w)`. Once you
wrap, `w` is your `*statusRecorder`, which does *not* implement `Flusher` — so an
SSE handler that does `w.(http.Flusher).Flush()` panics, or `NewResponseController`
returns `ErrNotSupported`, and streaming silently stops. The fix, standardized in
Go 1.20, is an `Unwrap() http.ResponseWriter` method: `NewResponseController` and
the `http.rwUnwrapper` interface follow the `Unwrap` chain to find a writer that
supports Flush/deadlines. Give your recorder `func (rec *statusRecorder) Unwrap()
http.ResponseWriter { return rec.ResponseWriter }` and streaming works through it.

Embedding `http.ResponseWriter` in the struct means the recorder inherits `Header`
and any methods it does not override, and only `WriteHeader`/`Write` are
customized. Note that embedding alone does *not* solve the Flusher problem — an
embedded `ResponseWriter` promotes `Flush` only if the underlying type has it as a
method in the interface, which `http.ResponseWriter` does not declare; the
capability is discovered dynamically, which is exactly why `Unwrap` is required.

Create `recorder.go`:

```go
package statusrec

import (
	"log/slog"
	"net/http"
)

// statusRecorder wraps an http.ResponseWriter to capture the status code and the
// number of bytes written, for access logging. It guards against a second
// WriteHeader and exposes Unwrap so http.NewResponseController can reach the real
// writer for Flush and deadlines.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	// Default to 200: a handler that only calls Write implies WriteHeader(200).
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records and forwards only the first status; later calls are
// ignored, exactly as net/http does, so the log matches what the client saw.
func (rec *statusRecorder) WriteHeader(status int) {
	if rec.wroteHeader {
		return
	}
	rec.wroteHeader = true
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

// Unwrap lets http.NewResponseController find the underlying writer for
// Flush/SetWriteDeadline. Without it, wrapping disables streaming and deadlines.
func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// AccessLog wraps next, recording the final status and byte count for each
// request.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := newStatusRecorder(w)
		next.ServeHTTP(rec, r)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status, "bytes", rec.bytes)
	})
}
```

### The runnable demo

The demo installs a text `slog` handler on stdout, wraps two handlers (one plain,
one that double-writes the header), and prints the access-log lines. To keep the
output deterministic the demo strips the timestamp by using a `ReplaceAttr` that
drops the `time` key.

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/statusrec"
)

func main() {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	})
	mux.HandleFunc("/double", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError) // ignored by the recorder
		w.Write([]byte("body"))
	})

	srv := httptest.NewServer(statusrec.AccessLog(mux))
	defer srv.Close()

	for _, path := range []string{"/ok", "/double"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			panic(err)
		}
		resp.Body.Close()
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg=request method=GET path=/ok status=201 bytes=7
level=INFO msg=request method=GET path=/double status=200 bytes=4
```

The `/double` line records status 200 — the *first* `WriteHeader` — not the
ignored 500, matching what the client received.

### Tests

`TestSingleStatusOnDoubleWriteHeader` calls `WriteHeader` twice and asserts the
recorder kept the first status and did not panic. `TestFlushReachesRealWriter` is
the `Unwrap` proof: it wraps an `httptest.ResponseRecorder` (which implements
`http.Flusher`), then calls `http.NewResponseController(rec).Flush()` and asserts
no error — which only works if `NewResponseController` followed `Unwrap` to the
flushable underlying writer. A `WriteOnly` case proves a bare `Write` records 200
and the byte count.

Create `recorder_test.go`:

```go
package statusrec

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSingleStatusOnDoubleWriteHeader(t *testing.T) {
	t.Parallel()

	base := httptest.NewRecorder()
	rec := newStatusRecorder(base)

	rec.WriteHeader(http.StatusTeapot)
	rec.WriteHeader(http.StatusInternalServerError) // must be ignored

	if rec.status != http.StatusTeapot {
		t.Fatalf("recorded status = %d, want %d (first wins)", rec.status, http.StatusTeapot)
	}
	if base.Code != http.StatusTeapot {
		t.Fatalf("forwarded status = %d, want %d", base.Code, http.StatusTeapot)
	}
}

func TestWriteOnlyRecords200AndBytes(t *testing.T) {
	t.Parallel()

	base := httptest.NewRecorder()
	rec := newStatusRecorder(base)

	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 || rec.bytes != 5 {
		t.Fatalf("bytes: write returned %d, recorded %d, want 5", n, rec.bytes)
	}
	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (implied by Write)", rec.status)
	}
}

func TestFlushReachesRealWriter(t *testing.T) {
	t.Parallel()

	// httptest.ResponseRecorder implements http.Flusher.
	base := httptest.NewRecorder()
	rec := newStatusRecorder(base)

	ctrl := http.NewResponseController(rec)
	if err := ctrl.Flush(); err != nil {
		t.Fatalf("Flush through Unwrap failed: %v", err)
	}
	if !base.Flushed {
		t.Fatal("underlying recorder was not flushed; Unwrap not honored")
	}
}

func ExampleAccessLog() {
	base := httptest.NewRecorder()
	rec := newStatusRecorder(base)
	rec.WriteHeader(http.StatusAccepted)
	rec.WriteHeader(http.StatusOK) // ignored
	fmt.Println(rec.status)
	// Output: 202
}
```

## Review

The recorder is correct on three counts. It records the status a handler actually
committed — the first `WriteHeader`, with later calls ignored — so
`TestSingleStatusOnDoubleWriteHeader` catches the log-disagrees-with-client bug. It
treats a bare `Write` as an implied 200 and counts bytes, which
`TestWriteOnlyRecords200AndBytes` pins. And it preserves the writer's capabilities
through `Unwrap`, which `TestFlushReachesRealWriter` proves by flushing via
`http.NewResponseController` — delete the `Unwrap` method and that test fails with
`ErrNotSupported`, which in production means a streaming endpoint silently stops
flushing. The whole point is that adding observability to the response must not
degrade the response: same status semantics, same streaming, same deadlines.

## Resources

- [`net/http#NewResponseController`](https://pkg.go.dev/net/http#NewResponseController) — Flush/SetWriteDeadline that follows the `Unwrap` chain.
- [`net/http#ResponseController.Flush`](https://pkg.go.dev/net/http#ResponseController.Flush) — the flush call the wrapper must not break.
- [`net/http#Flusher`](https://pkg.go.dev/net/http#Flusher) — the streaming capability hidden by a naive wrapper.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-request-timeout-mapping.md](07-request-timeout-mapping.md)
