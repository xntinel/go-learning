# Exercise 8: Test streaming responses and Server-Sent Events with Flusher

Streaming endpoints — Server-Sent Events, long-poll, log tailing — write a piece,
flush it, and keep the connection open. A `ResponseRecorder` buffers everything, so
it can prove the *flush contract* but never *incremental delivery*; for that you
need a real server and an incremental reader. This module builds an SSE handler and
tests both halves, including that the producer stops when the client disconnects.

## What you'll build

```text
sse/                            independent module: example.com/sse-streaming-flush
  go.mod                        go 1.26
  sse.go                        Handler(src) streaming SSE events, flushing each, watching ctx
  cmd/
    demo/
      main.go                   streams three events and reads them with a bufio.Scanner
  sse_test.go                   unit flush assertion + e2e incremental read + disconnect stops producer
```

- Files: `sse.go`, `cmd/demo/main.go`, `sse_test.go`.
- Implement: `Handler(src <-chan string)` that sets `text/event-stream`, writes `data: <event>\n\n` per item, calls `Flush` after each, and returns on `r.Context().Done()` or a closed source.
- Test: unit — run against a recorder with a pre-filled closed channel, assert `rec.Flushed` and the buffered event lines; e2e — start a server, read events incrementally with a `bufio.Scanner`, then cancel the context and assert the producer goroutine observes `Done()` and stops (guarded by a timeout so a leak fails).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sse/cmd/demo
cd ~/go-exercises/sse
go mod init example.com/sse-streaming-flush
```

### Why a recorder cannot prove streaming, and Flusher

An `http.ResponseWriter` buffers writes; the bytes do not necessarily leave the
process until the handler returns — unless the handler calls `Flush()` on the
`http.Flusher` the writer implements. Flushing is what makes SSE work: each
`data: ...\n\n` frame reaches the client immediately, not at the end. To test this
you must separate two claims. The *flush contract* — "the handler flushed after
each event" — is unit-testable: a `ResponseRecorder` implements `http.Flusher`,
records `Flushed = true` when `Flush` is called, and buffers the frames so you can
read them back. But *incremental delivery* — "the client receives event 1 before
the handler produces event 4" — cannot be shown against a recorder, which buffers
everything and hands it all back at once. That claim needs a real
`httptest.Server` and a client that reads the body progressively with a
`bufio.Scanner`.

The other production hazard is the producer goroutine. A streaming handler runs a
loop that could run forever; when the client disconnects, `r.Context()` is
canceled, and the loop must `select` on `r.Context().Done()` and return — otherwise
it leaks for the life of the process. The e2e test proves this directly: it cancels
the client context and asserts the handler exits (signaled by a channel the test
closes on the handler's return), failing with a timeout if the producer leaks.

`Handler` takes its events from a channel so both tests can drive it: the unit test
pre-fills and closes the channel (the handler drains and returns); the e2e test
feeds it from a goroutine and relies on context cancellation to stop the handler.

Create `sse.go`:

```go
package sse

import (
	"fmt"
	"net/http"
)

// Handler streams items from src as Server-Sent Events, flushing after each so
// the client receives them incrementally. It returns when src is closed or the
// request context is canceled (client disconnect).
func Handler(src <-chan string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush() // commit and flush the headers

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", ev)
				flusher.Flush()
			}
		}
	}
}
```

### The demo

The demo streams three events (pre-filled, then the channel is closed) and reads
them back with a scanner.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"

	"example.com/sse-streaming-flush"
)

func main() {
	src := make(chan string, 3)
	src <- "tick-0"
	src <- "tick-1"
	src <- "tick-2"
	close(src)

	srv := httptest.NewServer(sse.Handler(src))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			fmt.Println(line)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
data: tick-0
data: tick-1
data: tick-2
```

### Tests

The unit test runs the handler against a recorder with a pre-filled, closed
channel: the handler drains three events and returns, so we assert `rec.Flushed`
and that the buffer holds three `data:` frames. The e2e test starts a server whose
handler is wrapped to close a `stopped` channel on return; a feeder goroutine
supplies events, the test reads the first three incrementally, then cancels the
context and asserts the handler exits (via `stopped`) within a timeout — proving
the producer honors client disconnect.

Create `sse_test.go`:

```go
package sse

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFlushContractUnit(t *testing.T) {
	t.Parallel()

	src := make(chan string, 3)
	src <- "a"
	src <- "b"
	src <- "c"
	close(src)

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()
	Handler(src)(rec, req)

	if !rec.Flushed {
		t.Fatal("Flushed = false, want true (handler must flush each event)")
	}
	if got := strings.Count(rec.Body.String(), "data: "); got != 3 {
		t.Fatalf("event frames = %d, want 3 (body %q)", got, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestIncrementalDeliveryAndDisconnect(t *testing.T) {
	t.Parallel()

	src := make(chan string)
	stopped := make(chan struct{})
	core := Handler(src)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		core(w, r)
	}))
	t.Cleanup(srv.Close)

	// Feed events until the test tears down.
	feedDone := make(chan struct{})
	t.Cleanup(func() { close(feedDone) })
	go func() {
		for i := 0; ; i++ {
			select {
			case <-feedDone:
				return
			case src <- eventName(i):
			}
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the first three events incrementally.
	scanner := bufio.NewScanner(resp.Body)
	var got []string
	for scanner.Scan() && len(got) < 3 {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			got = append(got, strings.TrimPrefix(line, "data: "))
		}
	}
	want := []string{"tick-0", "tick-1", "tick-2"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("events = %v, want prefix %v", got, want)
		}
	}

	// Client disconnects; the handler must observe ctx.Done and stop.
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("producer goroutine leaked: handler did not return after client disconnect")
	}
}

func eventName(i int) string {
	return "tick-" + string(rune('0'+i))
}
```

The `eventName` helper keeps the events small and printable (`tick-0`..`tick-9`);
the test only reads the first three. As a "your turn" addition, assert the handler
returns a 500 when the `ResponseWriter` does not implement `http.Flusher` (wrap a
recorder in a type that hides `Flush`).

## Review

The split is the lesson: unit-assert the flush *contract* with a recorder
(`rec.Flushed` plus the buffered frames), but prove incremental *delivery* only
with a real server and a `bufio.Scanner` reading as bytes arrive. A recorder can
never show that event 1 reaches the client before event 4 is produced, because it
buffers. The disconnect half is the production-critical one: a streaming handler
that ignores `r.Context().Done()` leaks a goroutine on every dropped client, and
the timeout-guarded `stopped` channel turns that leak into a failing test rather
than a slow memory leak in production. Running under `-race` confirms the channel
hand-off between feeder, handler, and reader is clean.

## Resources

- [net/http `Flusher`](https://pkg.go.dev/net/http#Flusher) — the flush interface behind streaming.
- [httptest `ResponseRecorder.Flushed`](https://pkg.go.dev/net/http/httptest#ResponseRecorder) — the unit-level flush signal.
- [bufio `Scanner`](https://pkg.go.dev/bufio#Scanner) — reading a streamed body line by line.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-middleware-chain.md](07-middleware-chain.md) | Next: [09-context-deadline-handler.md](09-context-deadline-handler.md)
