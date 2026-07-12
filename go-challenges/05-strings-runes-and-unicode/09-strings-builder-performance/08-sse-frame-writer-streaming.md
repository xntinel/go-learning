# Exercise 8: Write Server-Sent Events Frames Streaming into http.ResponseWriter

A Server-Sent Events endpoint pushes a stream of events to a client, and each event
is a small framed block: an optional `event:` line, an optional `id:` line, one or
more `data:` lines, and a blank line terminator. This exercise writes those frames
directly into an `io.Writer` (the `http.ResponseWriter`), flushing after each frame,
rather than building the whole stream in memory — and handles multi-line payloads by
splitting on newlines.

This module is self-contained.

## What you'll build

```text
sseframe/                    independent module: example.com/sseframe
  go.mod
  sseframe.go                Event; WriteEvent (streams to io.Writer + flush); FormatEvent (Builder)
  cmd/
    demo/
      main.go                writes two frames to stdout, including a multi-line one
  sseframe_test.go           exact framing via httptest recorder; multi-line data; flush-per-frame
```

Files: `sseframe.go`, `cmd/demo/main.go`, `sseframe_test.go`.
Implement: `WriteEvent(w io.Writer, ev Event) error` streaming the frame and calling `Flush` if `w` is an `http.Flusher`, plus `FormatEvent(ev Event) string` for the in-memory case.
Test: exact wire framing through `httptest.ResponseRecorder`; a multi-line payload becomes multiple `data:` lines; a fake `Flusher` confirms one flush per frame.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/08-sse-frame-writer-streaming/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/08-sse-frame-writer-streaming
```

### Framing and flushing

The SSE wire format is simple but exact. Each frame is a sequence of `field: value`
lines terminated by a single blank line, and the client treats the blank line as the
signal that the event is complete. The `data` field is special: a payload that spans
multiple lines is not sent as one `data:` line with embedded newlines — each physical
line gets its own `data:` prefix, and the client rejoins them with newlines. So a
message `"line1\nline2"` becomes two lines, `data: line1` and `data: line2`. We get
that by `strings.Split(ev.Data, "\n")` and prefixing each piece.

The streaming discipline matters because the sink is a socket. `WriteEvent` writes the
frame straight into the `io.Writer` with `fmt.Fprintf` per field rather than assembling
the entire multi-event stream in a buffer first — an SSE connection may be open for
hours, and buffering it all would be a memory leak. After each frame it calls `Flush`
so the bytes actually leave the server's write buffer and reach the client promptly;
without the flush, a buffered `ResponseWriter` might hold several events before sending,
defeating the point of server-push. `Flush` is only available on writers that implement
`http.Flusher`, so we type-assert and flush only when the sink supports it — a plain
`bytes.Buffer` sink (in a test) simply is not flushed, which is fine.

`FormatEvent` builds the same frame as a single string with `strings.Builder`. It is
the right tool when you genuinely need the frame in memory — assembling one bounded
frame is fine; what you must avoid is buffering the whole unbounded stream. The tests
use it to assert `WriteEvent` and `FormatEvent` agree.

Create `sseframe.go`:

```go
package sseframe

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Event is one SSE event. Empty Event/ID fields are omitted from the frame.
type Event struct {
	ID    string
	Event string
	Data  string
}

// WriteEvent streams one SSE frame into w and flushes if w is an http.Flusher.
// It writes field by field rather than buffering the whole stream in memory.
func WriteEvent(w io.Writer, ev Event) error {
	if ev.Event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", ev.Event); err != nil {
			return err
		}
	}
	if ev.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", ev.ID); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// FormatEvent builds one SSE frame as a string using strings.Builder. Prefer
// WriteEvent for streaming; use this when you need the frame in memory.
func FormatEvent(ev Event) string {
	var b strings.Builder
	if ev.Event != "" {
		b.WriteString("event: ")
		b.WriteString(ev.Event)
		b.WriteByte('\n')
	}
	if ev.ID != "" {
		b.WriteString("id: ")
		b.WriteString(ev.ID)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"os"

	"example.com/sseframe"
)

func main() {
	events := []sseframe.Event{
		{Event: "message", ID: "1", Data: "hello"},
		{ID: "2", Data: "line1\nline2"},
	}
	for _, ev := range events {
		if err := sseframe.WriteEvent(os.Stdout, ev); err != nil {
			log.Fatal(err)
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
event: message
id: 1
data: hello

id: 2
data: line1
data: line2

```

### Tests

The framing test uses `httptest.ResponseRecorder` (which implements `http.Flusher`)
and asserts the exact bytes, including the doubled `data:` for a multi-line payload.
A separate test uses a fake writer that counts `Flush` calls to prove one flush per
frame. A consistency test checks `WriteEvent` and `FormatEvent` produce the same bytes.

Create `sseframe_test.go`:

```go
package sseframe

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestWriteEventFraming(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if err := WriteEvent(rec, Event{Event: "tick", ID: "7", Data: "a\nb"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	const want = "event: tick\nid: 7\ndata: a\ndata: b\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Fatal("expected the recorder to have been flushed")
	}
}

func TestOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if err := WriteEvent(rec, Event{Data: "only-data"}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	const want = "data: only-data\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

// countingWriter is an io.Writer + http.Flusher that records flush calls.
type countingWriter struct {
	buf     bytes.Buffer
	flushes int
}

func (c *countingWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *countingWriter) Flush()                      { c.flushes++ }

func TestFlushPerFrame(t *testing.T) {
	t.Parallel()

	cw := &countingWriter{}
	for range 3 {
		if err := WriteEvent(cw, Event{Data: "x"}); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	if cw.flushes != 3 {
		t.Fatalf("flushes = %d, want 3", cw.flushes)
	}
}

func TestWriteAndFormatAgree(t *testing.T) {
	t.Parallel()

	ev := Event{Event: "e", ID: "9", Data: "multi\nline"}
	var buf bytes.Buffer
	if err := WriteEvent(&buf, ev); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if buf.String() != FormatEvent(ev) {
		t.Fatalf("WriteEvent %q != FormatEvent %q", buf.String(), FormatEvent(ev))
	}
}

func ExampleFormatEvent() {
	fmt.Print(FormatEvent(Event{ID: "1", Data: "hi"}))
	// Output:
	// id: 1
	// data: hi
}
```

## Review

The writer is correct when the frame matches the SSE grammar exactly: present fields
in order, each `data:` line prefixed separately (so a multi-line payload becomes
several `data:` lines), and a single blank line terminating the frame. Streaming into
the `io.Writer` and flushing per frame is what makes server-push actually push — a
buffered response would hold events until the buffer fills. The `http.Flusher`
type-assert means the same code works against a real `ResponseWriter` (flushed) and a
test `bytes.Buffer` (not flushed) without change. `FormatEvent` and `WriteEvent` must
agree, which the consistency test pins.

## Resources

- [Server-Sent Events (MDN)](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events) — the frame grammar and `data:` line rules.
- [http.Flusher](https://pkg.go.dev/net/http#Flusher) — flushing buffered response data to the client.
- [httptest.ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder) — recording handler output, including `Flushed`.

---

Prev: [07-append-based-zero-intermediate-serializer.md](07-append-based-zero-intermediate-serializer.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-canonical-querystring-signing-builder.md](09-canonical-querystring-signing-builder.md)
