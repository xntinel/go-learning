# Exercise 6: Use io.Writer Instead of a Custom One-Method Interface

Before writing a bespoke one-method interface, check whether the standard library
already has it. This module builds an audit-log shipper on `io.Writer` — emitting
newline-delimited JSON — instead of a hand-rolled `LineSink interface { WriteLine(string) error }`.
The payoff is that files, buffers, gzip streams, and failing test spies plug in
for free, because the whole ecosystem already implements `io.Writer`.

## What you'll build

```text
auditsink/                  independent module: example.com/auditsink
  go.mod                    go 1.26
  sink.go                   AuditSink over io.Writer; Event; NewAuditSink; Write
  cmd/
    demo/
      main.go               ships events to a buffer and through gzip, reads back
  sink_test.go              NDJSON into bytes.Buffer; failing io.Writer surfaces error
```

- Files: `sink.go`, `cmd/demo/main.go`, `sink_test.go`.
- Implement: `NewAuditSink(w io.Writer)` that writes one JSON object per line (NDJSON) via `encoding/json`, replacing a bespoke `LineSink` interface.
- Test: write events into a `*bytes.Buffer` and assert the emitted NDJSON; inject a stub `io.Writer` that errors after N bytes and assert the sink surfaces the error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/12-interface-pollution-anti-patterns/06-prefer-stdlib-io-interface/cmd/demo
cd go-solutions/08-interfaces/12-interface-pollution-anti-patterns/06-prefer-stdlib-io-interface
```

### Why io.Writer beats a bespoke LineSink

A bespoke `LineSink interface { WriteLine(string) error }` looks reasonable until
you count what it costs. Nothing in the standard library implements `WriteLine`.
So to ship audit events to a file you write an adapter; to compress them you write
another adapter around `gzip.Writer`; to send them over the network you write a
third; and to test the sink you hand-roll a fake that records lines. Every one of
those already exists for `io.Writer`. `*os.File`, `*bytes.Buffer`, `*gzip.Writer`,
`*bufio.Writer`, `net.Conn`, and `httptest`'s recorder all satisfy `io.Writer`
today. Accept `io.Writer` and you inherit all of them at zero cost, and your sink
composes with any future writer too.

The sink emits NDJSON — one JSON object per line — which is the standard shape for
log shipping (Fluent Bit, Vector, and Loki all speak it). `json.Encoder.Encode`
does exactly this: it writes the JSON encoding of a value followed by a newline,
so a sink built on an `*json.Encoder` over the caller's `io.Writer` produces
NDJSON with no manual delimiter handling. Because the encoder writes straight to
the underlying writer, any write error — a full disk, a closed connection, a test
spy that fails after N bytes — propagates back out of `Encode` and out of the
sink's `Write`. You surface real I/O failures for free precisely because you did
not interpose a custom interface between yourself and the writer.

Create `sink.go`:

```go
package auditsink

import (
	"encoding/json"
	"fmt"
	"io"
)

// Event is one audit record.
type Event struct {
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Target string `json:"target"`
}

// AuditSink writes audit events as newline-delimited JSON to any io.Writer.
// Building on io.Writer (not a bespoke interface) means files, buffers, gzip
// streams, and network connections all work as sinks with no adapter.
type AuditSink struct {
	enc *json.Encoder
}

// NewAuditSink builds a sink over any io.Writer.
func NewAuditSink(w io.Writer) *AuditSink {
	return &AuditSink{enc: json.NewEncoder(w)}
}

// Write emits one event as a JSON line. json.Encoder.Encode appends the newline,
// yielding NDJSON, and surfaces any underlying write error.
func (s *AuditSink) Write(e Event) error {
	if err := s.enc.Encode(e); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}
```

### The runnable demo

The demo ships events to a plain buffer and, without changing the sink, to a gzip
stream — then decompresses and prints them, proving `gzip.Writer` is a drop-in
`io.Writer` sink.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"example.com/auditsink"
)

func main() {
	events := []auditsink.Event{
		{Action: "login", Actor: "alice", Target: "console"},
		{Action: "delete", Actor: "bob", Target: "record-42"},
	}

	// Sink 1: a plain buffer.
	var buf bytes.Buffer
	sink := auditsink.NewAuditSink(&buf)
	for _, e := range events {
		if err := sink.Write(e); err != nil {
			panic(err)
		}
	}
	fmt.Print(buf.String())

	// Sink 2: the SAME sink type over a gzip stream, no adapter needed.
	var gzbuf bytes.Buffer
	gz := gzip.NewWriter(&gzbuf)
	gzsink := auditsink.NewAuditSink(gz)
	for _, e := range events {
		if err := gzsink.Write(e); err != nil {
			panic(err)
		}
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}

	// Read the compressed audit log back.
	zr, err := gzip.NewReader(&gzbuf)
	if err != nil {
		panic(err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		panic(err)
	}
	fmt.Printf("gzip round-trip bytes: %d\n", len(decompressed))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"action":"login","actor":"alice","target":"console"}
{"action":"delete","actor":"bob","target":"record-42"}
gzip round-trip bytes: 109
```

### Tests

`TestWritesNDJSON` uses a `*bytes.Buffer` sink and asserts the exact two-line
NDJSON output. `TestSurfacesWriteError` injects a `failingWriter` that returns an
error after N bytes and asserts the sink returns a wrapped error — the failure a
bespoke interface would have hidden behind a custom fake. `ExampleAuditSink_Write`
shows the single-line output.

Create `sink_test.go`:

```go
package auditsink

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestWritesNDJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sink := NewAuditSink(&buf)

	events := []Event{
		{Action: "login", Actor: "alice", Target: "console"},
		{Action: "delete", Actor: "bob", Target: "record-42"},
	}
	for _, e := range events {
		if err := sink.Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	want := `{"action":"login","actor":"alice","target":"console"}` + "\n" +
		`{"action":"delete","actor":"bob","target":"record-42"}` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("NDJSON =\n%q\nwant\n%q", got, want)
	}
}

// failingWriter is a stub io.Writer that succeeds for the first limit bytes and
// then returns an error. Because the sink is built on io.Writer, this ten-line
// spy is the whole fake we need.
type failingWriter struct {
	limit   int
	written int
}

var errWriterFull = errors.New("writer full")

func (w *failingWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, errWriterFull
	}
	if len(p) > remaining {
		w.written = w.limit
		return remaining, errWriterFull
	}
	w.written += len(p)
	return len(p), nil
}

func TestSurfacesWriteError(t *testing.T) {
	t.Parallel()

	// Allow a few bytes, then fail partway through the first event.
	fw := &failingWriter{limit: 10}
	sink := NewAuditSink(fw)

	err := sink.Write(Event{Action: "login", Actor: "alice", Target: "console"})
	if !errors.Is(err, errWriterFull) {
		t.Fatalf("Write err = %v, want errors.Is(_, errWriterFull)", err)
	}
}

func ExampleAuditSink_Write() {
	var buf bytes.Buffer
	sink := NewAuditSink(&buf)
	_ = sink.Write(Event{Action: "login", Actor: "alice", Target: "console"})
	fmt.Print(buf.String())
	// Output: {"action":"login","actor":"alice","target":"console"}
}
```

## Review

The design lesson is subtraction: by NOT inventing `LineSink`, the sink gained
every `io.Writer` in existence as a valid destination and every `io.Writer` spy
as a test fake. The `TestSurfacesWriteError` case is the concrete proof that
reusing the stdlib interface does not cost you error handling — a partial write
propagates out through `Encode` and the sink wraps it with `%w`, so callers can
match it. A bespoke interface would have forced you to define, implement, and fake
`WriteLine` yourself, and to re-plumb error propagation by hand, for a type nobody
else in the program can use. Reach for a custom interface only when no stdlib
interface fits the shape of the data; for "write bytes somewhere," `io.Writer`
always fits.

## Resources

- [io.Writer](https://pkg.go.dev/io#Writer) — the one-method interface the whole ecosystem implements.
- [json.Encoder.Encode](https://pkg.go.dev/encoding/json#Encoder.Encode) — writes the JSON encoding followed by a newline (NDJSON).
- [compress/gzip](https://pkg.go.dev/compress/gzip) — `gzip.Writer` is an `io.Writer`, so it drops into the sink unchanged.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-interface-segregation-role-split.md](05-interface-segregation-role-split.md) | Next: [07-typed-nil-interface-pitfall.md](07-typed-nil-interface-pitfall.md)
