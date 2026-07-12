# Exercise 3: An SSE Re-Streaming HTTP Gateway

This is the on-the-job artifact: the endpoint your frontend actually calls. It
consumes a token source and re-emits it to clients as Server-Sent Events —
setting the right headers, writing one frame per token, flushing after each,
sending heartbeats when idle, propagating client disconnect upstream, and
signaling completion or failure in-band because the HTTP status is already `200`
once streaming has begun.

This module is fully self-contained. It uses only the standard library, has its
own `go mod init`, and ships its own demo and tests. Nothing here imports another
exercise; the token source is an interface you would satisfy with the streaming
client from Exercise 2 in production.

## What you'll build

```text
sseproxy/                   independent module: example.com/sseproxy
  go.mod                    go 1.26
  sseproxy.go               type TokenFunc; type Gateway (http.Handler); flush, heartbeats, cancel, terminal frames
  cmd/
    demo/
      main.go               run the gateway behind httptest and print the reassembled stream
  sseproxy_test.go          httptest-driven: streams incrementally, cancels upstream, error frame, heartbeat, no-flush, write deadline
```

- Files: `sseproxy.go`, `cmd/demo/main.go`, `sseproxy_test.go`.
- Implement: a `Gateway` implementing `http.ServeHTTP` that sets `text/event-stream` headers, flushes each `data:` frame with `http.NewResponseController`, sends `:` heartbeats on a ticker, cancels the source when `r.Context()` fires, emits a terminal `event: done` or `event: error`, and handles a `ResponseWriter` without flush support via `http.ErrNotSupported`.
- Test: drive the handler through `httptest.NewServer` with a fake channel-gated source; assert frames arrive before the source is exhausted, client cancellation reaches the source, an upstream error yields `event: error`, heartbeats appear when idle, and a non-flushing writer returns 500 instead of panicking.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/52-ai-llm-backends/02-streaming-completions/03-sse-stream-proxy/cmd/demo
cd go-solutions/52-ai-llm-backends/02-streaming-completions/03-sse-stream-proxy
go mod edit -go=1.26
```

### The headers, and why each one matters

Before a single byte of body, the handler sets four headers.
`Content-Type: text/event-stream` is what makes it SSE. `Cache-Control: no-cache`
stops caches from holding the response. `Connection: keep-alive` keeps the socket
open. `X-Accel-Buffering: no` is the one people forget: it tells nginx (and
compatible proxies) not to buffer the response, which is the single most common
reason streaming works locally and breaks in staging. None of these help if a
compression middleware sits in front coalescing frames, so the event-stream route
must also be exempt from gzip.

### Flush is not optional, and neither is ResponseController

After each frame the handler flushes. Without a flush, Go's write buffering holds
the bytes until the buffer fills or the handler returns — which is exactly "the
whole response arrives at the end," the failure the gateway exists to prevent. The
tool is `http.NewResponseController(w).Flush()`. The older
`w.(http.Flusher).Flush()` type assertion is a trap: the moment any middleware
wraps the `ResponseWriter`, the assertion fails, and depending on how you wrote it
that is either a silent no-flush or a panic. `ResponseController` unwraps through
middleware to find the real flusher, and when there genuinely is none it returns
`http.ErrNotSupported` — an error you handle, not a panic. The handler probes
flush support up front, before committing to streaming, so it can still return a
clean `500` if the writer cannot stream.

### Cancellation: the money path

`r.Context()` is cancelled when the client disconnects and, in net/http, also when
the handler returns. The handler calls the source with that context, so a client
who walks away cancels upstream generation. This is the most cost-relevant line in
the whole gateway: without it, a closed browser tab keeps the provider generating
billed tokens against a dead socket. The source runs in its own goroutine feeding
a channel; the main loop selects over that channel, a heartbeat ticker, and
`ctx.Done()`, so a cancellation is observed promptly and both goroutines unwind.

### In-band terminal frames

Once the first frame is flushed, the `200` status is committed; you cannot turn it
into a `500`. So success and failure are both signaled inside the stream. A clean
finish emits `event: done` with a `[DONE]` payload; an upstream error emits
`event: error` with the message. Because the error text is untrusted for SSE
framing (a stray newline would split it into bogus frames), the handler collapses
newlines before writing it. A client that sees neither terminal frame — just a
dropped connection — knows the stream was truncated.

### Heartbeats and write deadlines

An idle model can think for seconds before the first token, long enough for a load
balancer to cut an idle connection. A ticker writes a `:` comment frame on the
configured interval; comments are ignored by the SSE parser, so they keep the
connection warm without appearing as data. And because a slow client that stops
reading would otherwise block the handler's `Write` forever — pinning a goroutine
and an upstream connection — an optional per-frame write deadline via
`ResponseController.SetWriteDeadline` turns a stuck write into a failed one, so the
handler sheds the client instead of hanging.

Create `sseproxy.go`:

```go
package sseproxy

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"
)

// TokenFunc produces a token stream bound to ctx. The gateway calls it once per
// request and passes r.Context(), so client disconnect propagates to the source.
type TokenFunc func(ctx context.Context) iter.Seq2[string, error]

// Gateway is an http.Handler that re-streams a TokenFunc to clients as SSE.
type Gateway struct {
	// Source produces the tokens for one request.
	Source TokenFunc
	// Heartbeat is the interval between ":" keepalive comments; zero disables them.
	Heartbeat time.Duration
	// WriteTimeout bounds each frame write so a stalled client is dropped rather
	// than pinning a goroutine; zero means no deadline.
	WriteTimeout time.Duration
}

// ServeHTTP streams Source to the client. It flushes after every frame, sends
// heartbeats when idle, cancels Source when the client disconnects, and ends with
// a terminal event: done or event: error frame.
func (g Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// Probe flush support before committing to streaming. If the writer cannot
	// flush we can still return a clean error status; once we stream we cannot.
	if err := rc.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "streaming unsupported by server", http.StatusInternalServerError)
		}
		return
	}

	ctx := r.Context()

	writeFrame := func(s string) bool {
		if g.WriteTimeout > 0 {
			if err := rc.SetWriteDeadline(time.Now().Add(g.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return false
			}
		}
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	type item struct {
		tok string
		err error
	}
	items := make(chan item)
	go func() {
		defer close(items)
		for tok, err := range g.Source(ctx) {
			select {
			case items <- item{tok: tok, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var tick <-chan time.Time
	if g.Heartbeat > 0 {
		ticker := time.NewTicker(g.Heartbeat)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			// Client disconnected; the source goroutine observes ctx too.
			return
		case <-tick:
			if !writeFrame(": heartbeat\n\n") {
				return
			}
		case it, ok := <-items:
			if !ok {
				// Source finished cleanly: signal an unambiguous end.
				writeFrame("event: done\ndata: [DONE]\n\n")
				return
			}
			if it.err != nil {
				// Status is already 200; the only channel left is an in-band frame.
				writeFrame("event: error\ndata: " + oneLine(it.err.Error()) + "\n\n")
				return
			}
			if !writeFrame("data: " + it.tok + "\n\n") {
				return
			}
		}
	}
}

// oneLine collapses newlines so an error message cannot break SSE framing.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}
```

### The runnable demo

The demo runs the gateway behind `httptest.NewServer`, requests it as a client,
reassembles the `data:` frames, and prints the result. The source emits four
tokens with small delays so the frames genuinely stream rather than arriving at
once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"iter"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"example.com/sseproxy"
)

func source(tokens []string) sseproxy.TokenFunc {
	return func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			for _, tok := range tokens {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
				if !yield(tok, nil) {
					return
				}
			}
		}
	}
}

func main() {
	gw := sseproxy.Gateway{
		Source:    source([]string{"The", "quick", "brown", "fox"}),
		Heartbeat: time.Second,
	}
	srv := httptest.NewServer(gw)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var toks []string
	var terminal string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			if p := strings.TrimPrefix(line, "data: "); p != "[DONE]" {
				toks = append(toks, p)
			}
		case line == "event: done":
			terminal = "done"
		case line == "event: error":
			terminal = "error"
		}
	}
	fmt.Printf("tokens: %s\n", strings.Join(toks, " "))
	fmt.Printf("terminal: %s\n", terminal)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tokens: The quick brown fox
terminal: done
```

### Tests

The tests drive the handler through a real `httptest.NewServer` and read the body
incrementally, because the properties that matter are about timing and lifecycle,
not just output. `TestStreamsBeforeSourceExhausted` gates the source on a channel
and confirms the first frame arrives before the source is released — that is the
proof the handler flushes rather than buffers. `TestClientCancelStopsSource`
cancels the request and asserts the source observes `ctx.Done()`, proving
cancellation propagates upstream. `TestUpstreamErrorFrame` confirms a source error
becomes a terminal `event: error` frame rather than silent truncation.
`TestHeartbeatWhenIdle` confirms `:` comments appear while the source is idle.
`TestFlushUnsupported` calls the handler with a `ResponseWriter` that cannot flush
and asserts a clean `500` rather than a panic. Finally the two write-deadline
tests exercise the `WriteTimeout` path: `TestWriteDeadlineConfigured` streams with
a deadline set on every frame and confirms the frames still land, and
`TestWriteDeadlineShedsStalledClient` makes a frame write fail — the way a
stalled client's expired deadline would — and asserts the handler unwinds instead
of blocking forever.

Create `sseproxy_test.go`:

```go
package sseproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// readData scans until the next data frame and returns its payload, skipping
// comments, blank lines, and the [DONE] terminator.
func readData(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	for sc.Scan() {
		line := sc.Text()
		if p, ok := strings.CutPrefix(line, "data: "); ok {
			if p == "[DONE]" {
				continue
			}
			return p
		}
	}
	t.Fatal("no data frame before stream ended")
	return ""
}

func TestStreamsBeforeSourceExhausted(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			if !yield("first", nil) {
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			yield("second", nil)
		}
	}
	srv := httptest.NewServer(Gateway{Source: src})
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	if got := readData(t, sc); got != "first" {
		t.Fatalf("first frame = %q, want first", got)
	}
	// The first frame arrived while the source is still blocked, so the handler
	// flushed rather than buffering the whole response.
	close(release)
	if got := readData(t, sc); got != "second" {
		t.Fatalf("second frame = %q, want second", got)
	}
}

func TestClientCancelStopsSource(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			if !yield("hello", nil) {
				return
			}
			<-ctx.Done()   // block until the client goes away
			close(stopped) // record that cancellation propagated upstream
		}
	}
	srv := httptest.NewServer(Gateway{Source: src})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(resp.Body)
	if got := readData(t, sc); got != "hello" {
		t.Fatalf("first frame = %q, want hello", got)
	}
	cancel()
	resp.Body.Close()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not observe client cancellation")
	}
}

func TestUpstreamErrorFrame(t *testing.T) {
	t.Parallel()
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			if !yield("partial", nil) {
				return
			}
			yield("", errors.New("upstream exploded"))
		}
	}
	srv := httptest.NewServer(Gateway{Source: src})
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"data: partial", "event: error", "upstream exploded"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q:\n%s", want, text)
		}
	}
}

func TestHeartbeatWhenIdle(t *testing.T) {
	t.Parallel()
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			<-ctx.Done() // stay idle; never emit a token
		}
	}
	srv := httptest.NewServer(Gateway{Source: src, Heartbeat: 20 * time.Millisecond})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":") {
			return // observed a heartbeat comment
		}
	}
	t.Fatal("no heartbeat comment observed before stream ended")
}

// noFlushRecorder is an http.ResponseWriter with no Flush method and no Unwrap,
// so http.NewResponseController(w).Flush() reports http.ErrNotSupported.
type noFlushRecorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *noFlushRecorder) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *noFlushRecorder) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *noFlushRecorder) WriteHeader(code int)        { w.code = code }

func TestFlushUnsupported(t *testing.T) {
	t.Parallel()
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) { yield("x", nil) }
	}
	w := &noFlushRecorder{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	Gateway{Source: src}.ServeHTTP(w, req) // must not panic

	if w.code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.code, http.StatusInternalServerError)
	}
	if !strings.Contains(w.body.String(), "streaming unsupported") {
		t.Fatalf("body = %q, want the unsupported message", w.body.String())
	}
}

// deadlineWriter is an http.ResponseWriter that supports Flush and
// SetWriteDeadline directly, so http.NewResponseController finds both without a
// real socket. It records that a deadline was set and can be told to fail Write
// after a given number of frames, simulating a stalled client whose write
// deadline has expired.
type deadlineWriter struct {
	header      http.Header
	body        bytes.Buffer
	deadlineSet bool
	failAfter   int // once writes exceeds this count, Write returns a timeout
	writes      int
}

func (w *deadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineWriter) WriteHeader(int) {}

func (w *deadlineWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.failAfter > 0 && w.writes > w.failAfter {
		return 0, os.ErrDeadlineExceeded
	}
	return w.body.Write(b)
}

func (w *deadlineWriter) Flush() {}

func (w *deadlineWriter) SetWriteDeadline(time.Time) error {
	w.deadlineSet = true
	return nil
}

func TestWriteDeadlineConfigured(t *testing.T) {
	t.Parallel()
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			for _, tok := range []string{"alpha", "beta"} {
				if !yield(tok, nil) {
					return
				}
			}
		}
	}
	w := &deadlineWriter{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	Gateway{Source: src, WriteTimeout: 50 * time.Millisecond}.ServeHTTP(w, req)

	if !w.deadlineSet {
		t.Fatal("SetWriteDeadline was never called with WriteTimeout configured")
	}
	body := w.body.String()
	for _, want := range []string{"data: alpha", "data: beta", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestWriteDeadlineShedsStalledClient(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if !yield(fmt.Sprintf("tok%d", i), nil) {
					return
				}
			}
		}
	}
	// Fail the write of the second frame, standing in for a stalled client whose
	// per-frame write deadline expired.
	w := &deadlineWriter{failAfter: 1}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		Gateway{Source: src, WriteTimeout: 10 * time.Millisecond}.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not unwind after a failed frame write")
	}
	if !w.deadlineSet {
		t.Fatal("SetWriteDeadline was never called")
	}
	if !strings.Contains(w.body.String(), "tok0") {
		t.Fatalf("body should carry the first frame before the write failed:\n%s", w.body.String())
	}
}

func Example() {
	src := func(ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			for _, tok := range []string{"hello", "world"} {
				if !yield(tok, nil) {
					return
				}
			}
		}
	}
	srv := httptest.NewServer(Gateway{Source: src})
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "data: "); ok && p != "[DONE]" {
			fmt.Println(p)
		}
	}
	// Output:
	// hello
	// world
}
```

## Review

The gateway is correct when three lifecycle properties hold, and the tests target
each directly. It streams rather than buffers: `TestStreamsBeforeSourceExhausted`
reads a frame while the source is still blocked, which only works if the handler
flushed. It propagates cancellation: `TestClientCancelStopsSource` fails if
`r.Context()` never reaches the source, which is the difference between a closed
tab stopping the bill and running it up. And it never lies about completion:
`TestUpstreamErrorFrame` proves a failure becomes an `event: error` frame rather
than a silently short response, which the client would otherwise mistake for a
real answer.

The mistakes to avoid are the ones that pass a demo and fail production. Do not
reach for `w.(http.Flusher)` — `TestFlushUnsupported` shows the graceful path is
`ResponseController` returning `http.ErrNotSupported`, and the type assertion
would panic once middleware wraps the writer. Do not forget `X-Accel-Buffering: no`
and keeping compression off this route; the handler can be perfect and a proxy
will still coalesce your frames. And do not try to write a `500` after the first
frame — the status is committed, so failures go in-band. Run `go test -race`; the
source goroutine, the ticker, and the request handler share a context and a
channel, and the detector confirms the handoff is clean.

## Resources

- [`net/http.ResponseController`](https://pkg.go.dev/net/http#ResponseController) — `Flush`, `SetWriteDeadline`, and the `http.ErrNotSupported` contract.
- [MDN — Using server-sent events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events) — the frame format, comments as keepalives, and client-side `EventSource`.
- [Anthropic — Streaming Messages](https://platform.claude.com/docs/en/docs/build-with-claude/streaming) — the upstream event and `ping` model this gateway re-emits, including in-band error events.

---

Back to [02-streaming-llm-client.md](02-streaming-llm-client.md) | Next: [../03-tool-and-function-calling/00-concepts.md](../03-tool-and-function-calling/00-concepts.md)
