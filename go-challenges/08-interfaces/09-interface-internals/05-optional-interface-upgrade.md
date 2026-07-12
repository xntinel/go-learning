# Exercise 5: Preserve http.Flusher And io.ReaderFrom When Wrapping A ResponseWriter

Almost every HTTP middleware wraps `http.ResponseWriter` to capture the status
code. Do it naively and you silently break streaming: the wrapper no longer
satisfies `http.Flusher`, so Server-Sent Events and chunked responses stop
flushing in production. This module builds the wrapper the right way, forwarding
the optional interfaces via runtime assertions — the standard library's "interface
upgrade" idiom.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
statusmw/                   independent module: example.com/statusmw
  go.mod                    go 1.26
  statusmw.go               Recorder (forwards Flush/ReadFrom) vs BrokenRecorder; Capture
  cmd/
    demo/
      main.go               runs a flushing handler through the middleware
  statusmw_test.go          Flusher forwarding, broken wrapper hides Flush, httptest flush
```

- Files: `statusmw.go`, `cmd/demo/main.go`, `statusmw_test.go`.
- Implement: a status-capturing `Recorder` that forwards `http.Flusher` and `io.ReaderFrom` via assertions on the inner writer, a `BrokenRecorder` that does not, and a `Capture` middleware.
- Test: `Recorder` satisfies `http.Flusher` and forwards `Flush` to the inner writer; `BrokenRecorder` does not satisfy `http.Flusher`; an `httptest` handler that flushes still flushes through the middleware.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/09-interface-internals/05-optional-interface-upgrade/cmd/demo
cd go-solutions/08-interfaces/09-interface-internals/05-optional-interface-upgrade
go mod edit -go=1.26
```

### Why the naive wrapper breaks streaming

`http.ResponseWriter` is a small interface: `Header`, `Write`, `WriteHeader`. But
the concrete value the server hands your handler *also* implements optional
interfaces — `http.Flusher` (flush buffered bytes to the client, essential for
SSE and streaming), `http.Hijacker` (take over the TCP connection, for WebSockets),
and `io.ReaderFrom` (efficient `sendfile`-style copies). The stdlib discovers
these at runtime by asking: `if f, ok := w.(http.Flusher); ok { f.Flush() }`. That
is an *optional interface upgrade* — a runtime type assertion that asks "does this
value also satisfy that richer interface?".

A status-capturing wrapper typically embeds `http.ResponseWriter`:

```go
type BrokenRecorder struct {
	http.ResponseWriter
	Status int
}
```

Embedding an interface promotes only the methods *in that interface's method set* —
`Header`, `Write`, `WriteHeader`. It does *not* promote `Flush`, because `Flush`
is not part of `http.ResponseWriter`. So even though the inner value is a
`http.Flusher`, `*BrokenRecorder` is not: the `w.(http.Flusher)` assertion in the
stdlib (or in `http.NewResponseController`) now fails, and flushing silently turns
into buffering. The user's live event stream stalls, and nothing errors — the
worst kind of production bug.

The fix is to forward each optional interface explicitly. `Recorder` adds a
`Flush` method that asserts the inner writer is a `http.Flusher` and delegates,
and a `ReadFrom` that does the same for `io.ReaderFrom` (falling back to `io.Copy`
when the inner writer lacks it). Now `*Recorder` satisfies `http.Flusher`, the
stdlib's upgrade assertion succeeds, and streaming works. (The modern alternative
is `http.NewResponseController(w)`, which centralizes these upgrades; the manual
forwarding here is what it does under the hood and what you still write when you
need a custom wrapper.)

Create `statusmw.go`:

```go
package statusmw

import (
	"io"
	"net/http"
)

// Recorder wraps an http.ResponseWriter to capture the status code while
// preserving the optional interfaces (http.Flusher, io.ReaderFrom) that
// streaming depends on.
type Recorder struct {
	http.ResponseWriter
	Status int
}

// NewRecorder wraps w, defaulting the status to 200 (which the server assumes
// when WriteHeader is never called).
func NewRecorder(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader records the status before forwarding it.
func (r *Recorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the inner writer's Flusher if it has one. Implementing this
// method is what keeps *Recorder satisfying http.Flusher.
func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom forwards to the inner writer's io.ReaderFrom if it has one, else
// falls back to a plain copy. This preserves sendfile-style efficiency.
func (r *Recorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(r.ResponseWriter, src)
}

// BrokenRecorder is the naive wrapper: it embeds http.ResponseWriter and adds no
// Flush method, so it does NOT satisfy http.Flusher even when the inner writer
// does. Kept here to prove the bug in a test.
type BrokenRecorder struct {
	http.ResponseWriter
	Status int
}

// WriteHeader records the status before forwarding it.
func (r *BrokenRecorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}

// Capture is a middleware that wraps the response writer in a Recorder so the
// captured status is available after next returns.
func Capture(next http.Handler, onDone func(status int)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := NewRecorder(w)
		next.ServeHTTP(rec, r)
		if onDone != nil {
			onDone(rec.Status)
		}
	})
}
```

### The runnable demo

The demo runs a streaming handler through `Capture`. The handler writes a header,
writes a chunk, and flushes. It serves into an `httptest.ResponseRecorder`, whose
`Flush` sets a `Flushed` flag, so the demo can prove the flush reached the inner
writer through the wrapper — and print the captured status.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/statusmw"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "event: ping\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	var captured int
	mw := statusmw.Capture(handler, func(status int) { captured = status })

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))

	fmt.Printf("captured status: %d\n", captured)
	fmt.Printf("flushed through wrapper: %v\n", rec.Flushed)
	fmt.Printf("body: %q\n", rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
captured status: 202
flushed through wrapper: true
body: "event: ping\n\n"
```

If `Recorder` did not forward `Flush`, the handler's `w.(http.Flusher)` assertion
would fail, `rec.Flushed` would be `false`, and a real client would never see the
event until the buffer filled.

### Tests

`TestRecorderForwardsFlush` uses a custom inner writer that counts `Flush` calls,
wraps it in a `Recorder`, asserts `*Recorder` satisfies `http.Flusher`, calls
`Flush` through that interface, and checks the inner counter incremented.
`TestBrokenRecorderHidesFlush` is the regression that fails a wrapper which forgot
to forward: it asserts `*BrokenRecorder` does *not* satisfy `http.Flusher` even
though its inner writer does. `TestOptionalInterfaceTable` tabulates the presence
of `Flusher`/`ReaderFrom` across both wrappers. `TestFlushThroughHTTPTest` is the
end-to-end check with `httptest`.

Create `statusmw_test.go`:

```go
package statusmw

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// flushCounter is an http.ResponseWriter that also implements http.Flusher and
// io.ReaderFrom, counting how often each is invoked.
type flushCounter struct {
	header    http.Header
	status    int
	flushes   int
	readsFrom int
	body      strings.Builder
}

func newFlushCounter() *flushCounter { return &flushCounter{header: make(http.Header)} }

func (w *flushCounter) Header() http.Header         { return w.header }
func (w *flushCounter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *flushCounter) WriteHeader(code int)        { w.status = code }
func (w *flushCounter) Flush()                      { w.flushes++ }
func (w *flushCounter) ReadFrom(src io.Reader) (int64, error) {
	w.readsFrom++
	return io.Copy(&w.body, src)
}

func TestRecorderForwardsFlush(t *testing.T) {
	t.Parallel()

	inner := newFlushCounter()
	rec := NewRecorder(inner)

	f, ok := any(rec).(http.Flusher)
	if !ok {
		t.Fatal("*Recorder must satisfy http.Flusher")
	}
	f.Flush()
	f.Flush()
	if inner.flushes != 2 {
		t.Fatalf("inner flushes = %d, want 2", inner.flushes)
	}
}

func TestRecorderForwardsReadFrom(t *testing.T) {
	t.Parallel()

	inner := newFlushCounter()
	rec := NewRecorder(inner)

	rf, ok := any(rec).(io.ReaderFrom)
	if !ok {
		t.Fatal("*Recorder must satisfy io.ReaderFrom")
	}
	n, err := rf.ReadFrom(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("ReadFrom error = %v", err)
	}
	if n != 5 || inner.readsFrom != 1 {
		t.Fatalf("ReadFrom n=%d readsFrom=%d, want 5 and 1", n, inner.readsFrom)
	}
}

func TestBrokenRecorderHidesFlush(t *testing.T) {
	t.Parallel()

	inner := newFlushCounter() // inner IS a Flusher
	broken := &BrokenRecorder{ResponseWriter: inner}

	if _, ok := any(broken).(http.Flusher); ok {
		t.Fatal("*BrokenRecorder must NOT satisfy http.Flusher (the bug this guards)")
	}
}

func TestOptionalInterfaceTable(t *testing.T) {
	t.Parallel()

	inner := newFlushCounter()
	tests := []struct {
		name        string
		w           http.ResponseWriter
		wantFlusher bool
	}{
		{"Recorder", NewRecorder(inner), true},
		{"BrokenRecorder", &BrokenRecorder{ResponseWriter: inner}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ok := tc.w.(http.Flusher)
			if ok != tc.wantFlusher {
				t.Fatalf("%s satisfies http.Flusher = %v, want %v", tc.name, ok, tc.wantFlusher)
			}
		})
	}
}

func TestFlushThroughHTTPTest(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	var captured int
	mw := Capture(handler, func(status int) { captured = status })

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if captured != http.StatusAccepted {
		t.Fatalf("captured status = %d, want %d", captured, http.StatusAccepted)
	}
	if !rec.Flushed {
		t.Fatal("flush did not reach the inner writer through the wrapper")
	}
}
```

## Review

The wrapper is correct when it is transparent to the optional-interface assertions
the stdlib performs. The proof is the pair of tests: `*Recorder` satisfies
`http.Flusher` and forwards to the inner counter, while `*BrokenRecorder` — the
naive embed-and-capture wrapper — does not satisfy it at all. That second test is
the one worth keeping in a real codebase, because the failure mode is silent:
nothing errors, streaming just stops. The mechanism to remember is that embedding
`http.ResponseWriter` promotes only its three methods; every richer capability
(`Flush`, `Hijack`, `ReadFrom`, `Push`) must be forwarded by hand or delegated to
`http.NewResponseController`. Run `go test -race` since middleware runs under
concurrent requests.

## Resources

- [http.ResponseController](https://pkg.go.dev/net/http#ResponseController) — the modern API that centralizes these optional-interface upgrades.
- [http.Flusher](https://pkg.go.dev/net/http#Flusher) — the optional interface a streaming handler asserts for.
- [io.ReaderFrom](https://pkg.go.dev/io#ReaderFrom) — the copy-optimization interface `io.Copy` upgrades to at runtime.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-boxing-allocation-hotpath.md](04-boxing-allocation-hotpath.md) | Next: [06-errors-as-itab-taxonomy.md](06-errors-as-itab-taxonomy.md)
