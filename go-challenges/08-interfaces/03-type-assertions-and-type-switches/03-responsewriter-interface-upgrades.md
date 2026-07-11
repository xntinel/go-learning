# Exercise 3: Probe Optional Capabilities On http.ResponseWriter (SSE Flush)

A server-sent-events endpoint must flush after every event or the client sees
nothing until the connection closes. `http.ResponseWriter` does not declare
`Flush` — only some concrete writers implement `http.Flusher` — so you must probe
for the capability at runtime. This exercise shows the classic assertion, why a
middleware wrapper silently breaks it, and the modern `http.ResponseController`
fix.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ssestream/                  independent module: example.com/ssestream
  go.mod                    module path
  ssestream.go              StreamEvents (controller flush), rawFlush, statusRecorder wrapper
  cmd/
    demo/
      main.go               runnable demo streaming events into a recorder
  ssestream_test.go         Flushed assertions: direct recorder vs capability-hiding wrapper
```

Files: `ssestream.go`, `cmd/demo/main.go`, `ssestream_test.go`.
Implement: `StreamEvents(w, events)` flushing via `http.NewResponseController`; `rawFlush(w)` showing the old `w.(http.Flusher)` probe; a `statusRecorder` wrapper that hides the capability but implements `Unwrap`.
Test: drive a `httptest.ResponseRecorder` and assert `rec.Flushed`; wrap it and show the raw assertion fails while the controller still flushes via `Unwrap`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ssestream/cmd/demo
cd ~/go-exercises/ssestream
go mod init example.com/ssestream
```

### The capability probe and why it breaks

`http.ResponseWriter` guarantees only `Header`, `Write`, and `WriteHeader`.
Flushing is optional: the concrete writer the net/http server passes you does
implement `http.Flusher`, but the interface does not say so. The historical way to
reach it is `f, ok := w.(http.Flusher)` — an assertion to an *interface* type,
which succeeds when the dynamic type happens to implement `Flusher`. That is the
`rawFlush` function below.

The fragility is real middleware. Almost every non-trivial handler chain wraps the
`ResponseWriter` to capture the status code, count bytes, or add a request ID. A
wrapper that embeds the `http.ResponseWriter` interface promotes only that
interface's three methods — it does *not* promote `Flush`, because the embedded
static type has no `Flush`. So `wrappedWriter.(http.Flusher)` fails even though the
base recorder can flush. The event stream silently stops flushing behind such
middleware.

`http.ResponseController` (Go 1.20) is the fix. `http.NewResponseController(w)`
returns a controller whose `Flush()` walks the writer chain: if `w` implements
`Flusher` it flushes, otherwise it looks for `Unwrap() http.ResponseWriter`, steps
to the inner writer, and tries again. A wrapper that implements that one `Unwrap`
method stays transparent to flushing (and hijacking) without re-declaring every
optional interface. `StreamEvents` uses the controller, so it works whether it is
handed a bare writer or one wrapped several layers deep.

`statusRecorder` below is the exact capability-hiding wrapper: it embeds
`http.ResponseWriter`, captures the status, and implements `Unwrap`. A raw
`Flusher` assertion on it fails; the controller reaches through it.

Create `ssestream.go`:

```go
package ssestream

import (
	"fmt"
	"net/http"
)

// StreamEvents writes each event as an SSE frame and flushes after each one via
// http.ResponseController, so it works through middleware wrappers that hide the
// underlying http.Flusher.
func StreamEvents(w http.ResponseWriter, events []string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	for _, e := range events {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", e); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// rawFlush is the pre-1.20 pattern: assert to http.Flusher directly. It returns
// whether the writer advertised the capability. Kept to show the failure mode.
func rawFlush(w http.ResponseWriter) bool {
	f, ok := w.(http.Flusher)
	if ok {
		f.Flush()
	}
	return ok
}

// statusRecorder is a typical middleware wrapper: it captures the status code and
// embeds the ResponseWriter interface, which hides Flusher from a raw assertion.
// It implements Unwrap so http.ResponseController can still reach the base writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
```

### The runnable demo

The demo drives the handler against an `httptest.ResponseRecorder` (which
implements `Flusher`) and reports the streamed body and whether a flush happened.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"strings"

	"example.com/ssestream"
)

func main() {
	rec := httptest.NewRecorder()
	if err := ssestream.StreamEvents(rec, []string{"open", "tick", "close"}); err != nil {
		fmt.Println("stream error:", err)
		return
	}
	fmt.Println("flushed:", rec.Flushed)
	fmt.Print(strings.ReplaceAll(rec.Body.String(), "\n\n", "|"))
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed: true
data: open|data: tick|data: close|
```

### Tests

`TestStreamsAndFlushes` drives the handler through a bare recorder and asserts
`rec.Flushed`. `TestWrapperHidesFlusherButControllerReaches` is the senior point:
the raw assertion on the wrapper fails, yet `StreamEvents` (via the controller and
`Unwrap`) still flushes the base recorder.

Create `ssestream_test.go`:

```go
package ssestream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamsAndFlushes(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	if err := StreamEvents(rec, []string{"a", "b"}); err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("recorder was not flushed")
	}
	if got := rec.Body.String(); !strings.Contains(got, "data: a\n\n") {
		t.Fatalf("body = %q, missing first event", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestWrapperHidesFlusherButControllerReaches(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = &statusRecorder{ResponseWriter: rec}

	// The old pattern fails: the wrapper does not advertise http.Flusher.
	if _, ok := w.(http.Flusher); ok {
		t.Fatal("wrapper unexpectedly satisfied http.Flusher directly")
	}
	if rawFlush(w) {
		t.Fatal("rawFlush should report false through the wrapper")
	}

	// The controller reaches the base recorder via Unwrap and flushes it.
	if err := StreamEvents(w, []string{"x"}); err != nil {
		t.Fatalf("StreamEvents through wrapper: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("base recorder was not flushed through the wrapper")
	}
}

func TestRawFlushWorksOnBareRecorder(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if !rawFlush(rec) {
		t.Fatal("bare recorder should satisfy http.Flusher")
	}
	if !rec.Flushed {
		t.Fatal("rawFlush did not flush the bare recorder")
	}
}
```

## Review

The handler is correct when a flush actually reaches the underlying writer after
every event, and the two tests bracket the point: a bare recorder is flushed by
either mechanism, but the wrapped recorder is flushed only through the controller.
The mistake this guards against is hand-asserting `w.(http.Flusher)` inside a chain
where a wrapper does not re-forward the interface — a bug that shows up not as a
crash but as an SSE stream that buffers silently. The `Unwrap() http.ResponseWriter`
method on the wrapper is the one line that keeps it transparent. Note there is no
panic-form assertion anywhere here: every probe is comma-ok, because the writer is
a boundary value. Run `go test -race` to confirm the flush reaches through the
wrapper.

## Resources

- [http.ResponseController (modern replacement for Flusher/Hijacker assertions)](https://pkg.go.dev/net/http#ResponseController)
- [http.Flusher](https://pkg.go.dev/net/http#Flusher)
- [httptest.ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-retry-classifier-net-error.md](02-retry-classifier-net-error.md) | Next: [04-dynamic-json-walker.md](04-dynamic-json-walker.md)
