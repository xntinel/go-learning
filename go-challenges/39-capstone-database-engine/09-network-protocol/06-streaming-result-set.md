# Exercise 6: Streaming a Large Result Set in Chunks

A result set can be far larger than memory, so a server must stream `DataRow` messages rather than buffer the whole thing. This exercise frames rows through a chunked `Writer` that flushes every N rows and a `Reader` that decodes them one frame at a time. Because `net.Pipe` is unbuffered, the test exercises real backpressure: the writer cannot get ahead of the reader, neither side ever holds the entire result set, and a disconnected reader surfaces to the writer as a write error instead of an infinite block.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
stream.go            Writer (chunked DataRow framing + Finish), Reader (ReadFrame), DecodeRow
cmd/
  demo/
    main.go          stream a result set from a Writer to a Reader over net.Pipe
stream_test.go       a large streamed result with backpressure, decode table, reader-close detection
```

- Files: `stream.go`, `cmd/demo/main.go`, `stream_test.go`.
- Implement: `Writer`, `NewWriter`, `(*Writer).WriteRow`, `(*Writer).Finish`, `Reader`, `NewReader`, `(*Reader).ReadFrame`, `DecodeRow`, and the `ErrShort` sentinel.
- Test: `stream_test.go` streams 5000 rows through an unbuffered pipe and reads them all back, decodes assorted rows including NULLs, and asserts a closed reader makes `WriteRow` fail rather than hang.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/09-network-protocol/06-streaming-result-set/cmd/demo && cd go-solutions/39-capstone-database-engine/09-network-protocol/06-streaming-result-set
```

### Why chunked framing and what backpressure buys

Streaming means the server emits each `DataRow` as it produces it and never materializes the full result. The `Writer` wraps the connection in a `bufio.Writer` and flushes every `chunk` rows: small chunks bound memory tightly and yield to the reader often; large chunks amortize the flush over more rows. Either way the invariant holds — at most one chunk's worth of rows sits in the buffer at a time, never the whole set. `Finish` writes `CommandComplete` and flushes whatever partial chunk remains, so the last few rows are never stranded in the buffer.

Backpressure is the property that makes this safe under a slow consumer, and `net.Pipe` makes it visible because it is unbuffered: a write blocks until the reader reads. So a writer streaming into a reader that drains slowly is forced to slow down to the reader's pace — it physically cannot run ahead and exhaust memory. The flip side is failure detection: if the reader disconnects, the next flush has nowhere to go and `WriteRow` returns a write error (`io.ErrClosedPipe` on a `net.Pipe`) instead of blocking forever. That error is how a streaming server learns a client hung up mid-result and stops producing rows.

The `Reader` mirrors the framing exactly, and every fixed-size field — the type byte, the four length bytes, the payload — goes through `io.ReadFull`, because a single `Read` on a real connection may return a partial frame. `ReadFrame` returns `io.EOF` from the first byte as a clean end of stream; any short read mid-frame is a truncation error. `DecodeRow` turns a `DataRow` payload into text-format column values, decoding the `-1` length as SQL NULL and bounds-checking every counted value so a malformed payload is `ErrShort`, not a panic.

Create `stream.go`:

```go
// Package streaming frames a large result set as a stream of DataRow messages
// written in chunks. net.Pipe is unbuffered, so a slow reader applies natural
// backpressure to the writer: this package never buffers the whole result set,
// only one bufio chunk at a time.
package streaming

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Backend message type bytes used by a result stream.
const (
	MsgDataRow     byte = 'D'
	MsgCmdComplete byte = 'C'
)

// ErrShort is the sentinel for a payload that ends before the format requires.
var ErrShort = errors.New("streaming: payload truncated")

// Writer streams DataRow messages, flushing the bufio buffer every chunk rows.
type Writer struct {
	w       *bufio.Writer
	chunk   int
	pending int
}

// NewWriter wraps c. chunk is the number of rows buffered before a Flush; values
// below 1 are clamped to 1.
func NewWriter(c net.Conn, chunk int) *Writer {
	if chunk < 1 {
		chunk = 1
	}
	return &Writer{w: bufio.NewWriter(c), chunk: chunk}
}

// WriteRow frames one DataRow of text-format column values (nil = SQL NULL) and
// flushes once chunk rows have accumulated, bounding memory and yielding to the
// reader.
func (w *Writer) WriteRow(cols []*string) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(cols)))
	for _, c := range cols {
		if c == nil {
			payload = binary.BigEndian.AppendUint32(payload, 0xFFFFFFFF)
			continue
		}
		payload = binary.BigEndian.AppendUint32(payload, uint32(len(*c)))
		payload = append(payload, *c...)
	}
	if err := writeFrame(w.w, MsgDataRow, payload); err != nil {
		return err
	}
	w.pending++
	if w.pending >= w.chunk {
		w.pending = 0
		return w.w.Flush()
	}
	return nil
}

// Finish writes CommandComplete and flushes any buffered rows.
func (w *Writer) Finish(tag string) error {
	if err := writeFrame(w.w, MsgCmdComplete, append([]byte(tag), 0)); err != nil {
		return err
	}
	return w.w.Flush()
}

// writeFrame writes type + int32(len+4) + payload (no flush).
func writeFrame(w *bufio.Writer, t byte, payload []byte) error {
	if err := w.WriteByte(t); err != nil {
		return fmt.Errorf("streaming: write type: %w", err)
	}
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)+4))
	if _, err := w.Write(lb[:]); err != nil {
		return fmt.Errorf("streaming: write len: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("streaming: write payload: %w", err)
	}
	return nil
}

// Reader reads framed messages from a stream.
type Reader struct {
	r *bufio.Reader
}

// NewReader wraps c.
func NewReader(c net.Conn) *Reader {
	return &Reader{r: bufio.NewReader(c)}
}

// ReadFrame reads one message. It uses io.ReadFull for the length and payload so
// a partial Read never truncates a frame. io.EOF from the first byte signals a
// clean end of stream.
func (r *Reader) ReadFrame() (byte, []byte, error) {
	t, err := r.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lb [4]byte
	if _, err := io.ReadFull(r.r, lb[:]); err != nil {
		return 0, nil, fmt.Errorf("streaming: read len: %w", err)
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n < 4 {
		return 0, nil, fmt.Errorf("streaming: len %d < 4: %w", n, ErrShort)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return 0, nil, fmt.Errorf("streaming: read payload: %w", err)
	}
	return t, payload, nil
}

// DecodeRow parses a DataRow payload into text-format column values; a nil
// element is SQL NULL.
func DecodeRow(payload []byte) ([]*string, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("streaming: row count: %w", ErrShort)
	}
	n := int(binary.BigEndian.Uint16(payload[:2]))
	rest := payload[2:]
	cols := make([]*string, n)
	for i := range cols {
		if len(rest) < 4 {
			return nil, fmt.Errorf("streaming: col %d len: %w", i, ErrShort)
		}
		l := int32(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if l == -1 {
			cols[i] = nil
			continue
		}
		if int(l) > len(rest) {
			return nil, fmt.Errorf("streaming: col %d data: %w", i, ErrShort)
		}
		s := string(rest[:l])
		cols[i] = &s
		rest = rest[l:]
	}
	return cols, nil
}
```

### The runnable demo

The demo streams a small result from a writer to a reader over `net.Pipe`, flushing every two rows. The reader prints each decoded row and the final `CommandComplete` tag. The values are deterministic, so the output is fixed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"example.com/streaming"
)

func main() {
	c, s := net.Pipe()

	const rows = 5
	go func() {
		defer s.Close()
		w := streaming.NewWriter(s, 2) // flush every 2 rows
		for i := 0; i < rows; i++ {
			v := strconv.Itoa(i)
			if err := w.WriteRow([]*string{&v}); err != nil {
				fmt.Fprintln(os.Stderr, "writer:", err)
				return
			}
		}
		if err := w.Finish("SELECT 5"); err != nil {
			fmt.Fprintln(os.Stderr, "writer:", err)
		}
	}()

	r := streaming.NewReader(c)
	defer c.Close()
	for {
		typ, payload, err := r.ReadFrame()
		if err != nil {
			fmt.Fprintln(os.Stderr, "reader:", err)
			os.Exit(1)
		}
		switch typ {
		case streaming.MsgDataRow:
			cols, err := streaming.DecodeRow(payload)
			if err != nil {
				fmt.Fprintln(os.Stderr, "reader:", err)
				os.Exit(1)
			}
			fmt.Printf("[reader] DataRow: %s\n", *cols[0])
		case streaming.MsgCmdComplete:
			tag := payload
			if n := len(tag); n > 0 && tag[n-1] == 0 {
				tag = tag[:n-1]
			}
			fmt.Printf("[reader] CommandComplete: %s\n", tag)
			return
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
[reader] DataRow: 0
[reader] DataRow: 1
[reader] DataRow: 2
[reader] DataRow: 3
[reader] DataRow: 4
[reader] CommandComplete: SELECT 5
```

### Tests

The headline test streams 5000 rows through an unbuffered pipe and reads them all back in order, which only succeeds if backpressure works and no frame is truncated. A decode table covers two-value rows, NULLs, and the empty row. A short-payload table pins the `ErrShort` boundaries. The reader-close test asserts the writer observes a write error after the reader disconnects, instead of blocking forever.

Create `stream_test.go`:

```go
package streaming

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
)

func TestStreamLargeResultSetWithBackpressure(t *testing.T) {
	t.Parallel()
	const rows = 5000
	c, s := net.Pipe() // unbuffered: writer blocks until reader drains
	t.Cleanup(func() { c.Close(); s.Close() })

	writeErr := make(chan error, 1)
	go func() {
		w := NewWriter(s, 64) // flush every 64 rows
		for i := 0; i < rows; i++ {
			v := strconv.Itoa(i)
			if err := w.WriteRow([]*string{&v, nil}); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- w.Finish("SELECT 5000")
	}()

	r := NewReader(c)
	got := 0
	for {
		typ, payload, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame after %d rows: %v", got, err)
		}
		if typ == MsgCmdComplete {
			break
		}
		if typ != MsgDataRow {
			t.Fatalf("type = %q, want DataRow", typ)
		}
		cols, err := DecodeRow(payload)
		if err != nil {
			t.Fatalf("DecodeRow row %d: %v", got, err)
		}
		if len(cols) != 2 || cols[0] == nil || cols[1] != nil {
			t.Fatalf("row %d shape wrong: %v", got, cols)
		}
		if want := strconv.Itoa(got); *cols[0] != want {
			t.Fatalf("row %d value = %q, want %q", got, *cols[0], want)
		}
		got++
	}
	if got != rows {
		t.Errorf("received %d rows, want %d", got, rows)
	}
	if err := <-writeErr; err != nil {
		t.Errorf("writer: %v", err)
	}
}

func TestDecodeRowTable(t *testing.T) {
	t.Parallel()
	mk := func(vals ...*string) []byte {
		var p []byte
		p = appendU16(p, uint16(len(vals)))
		for _, v := range vals {
			if v == nil {
				p = appendU32(p, 0xFFFFFFFF)
				continue
			}
			p = appendU32(p, uint32(len(*v)))
			p = append(p, *v...)
		}
		return p
	}
	a, b := "x", ""
	cases := []struct {
		name    string
		payload []byte
		wantLen int
	}{
		{"two-values", mk(&a, &b), 2},
		{"with-null", mk(&a, nil), 2},
		{"empty-row", mk(), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cols, err := DecodeRow(tc.payload)
			if err != nil {
				t.Fatalf("DecodeRow: %v", err)
			}
			if len(cols) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(cols), tc.wantLen)
			}
		})
	}
}

func TestDecodeRowShort(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		{0x00},                         // count truncated
		{0x00, 0x01},                   // count says 1 col, no length
		{0x00, 0x01, 0x00, 0x00, 0x00}, // length truncated
	}
	for i, payload := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeRow(payload); !errors.Is(err, ErrShort) {
				t.Errorf("err = %v, want ErrShort", err)
			}
		})
	}
}

func TestWriterObservesReaderClose(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer s.Close()
	w := NewWriter(s, 1) // flush every row so the close is seen immediately
	r := NewReader(c)

	readErr := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if _, _, err := r.ReadFrame(); err != nil {
				readErr <- err
				return
			}
		}
		readErr <- c.Close() // disconnect after 100 rows
	}()

	var writeErr error
	for i := 0; i < 1_000_000; i++ {
		v := strconv.Itoa(i)
		if writeErr = w.WriteRow([]*string{&v}); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		t.Fatal("writer never observed reader close")
	}
	if !errors.Is(writeErr, io.ErrClosedPipe) && !errors.Is(writeErr, net.ErrClosed) {
		t.Errorf("writeErr = %v, want a closed-pipe error", writeErr)
	}
	if err := <-readErr; err != nil {
		t.Errorf("reader close: %v", err)
	}
}

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
```

## Review

The stream is correct when 5000 rows flow through an unbuffered pipe in order with bounded memory: the writer holds at most one chunk, the reader decodes one frame at a time, and backpressure keeps the writer at the reader's pace. NULL columns decode as `nil` distinct from the empty string, and every counted value is bounds-checked so a malformed payload is `ErrShort` rather than a panic. When the reader disconnects, the writer's next flush fails with a closed-pipe error, which is how a streaming server detects a hung-up client and stops producing.

The mistakes to avoid: never flushing (or flushing only in `Finish`), which buffers the entire result and defeats streaming; assuming a single `Read` returns a whole frame, which truncates rows under load; and ignoring the write error, which leaks a goroutine that streams forever into a dead connection.

## Resources

- [PostgreSQL: Message Formats — DataRow](https://www.postgresql.org/docs/current/protocol-message-formats.html) — the counted-value layout each streamed row uses.
- [`bufio.Writer`](https://pkg.go.dev/bufio#Writer) — buffering and the explicit `Flush` that chunking controls.
- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the exact-count read that makes partial network reads safe.
- [`net.Pipe`](https://pkg.go.dev/net#Pipe) — the unbuffered, synchronous connection that exposes real backpressure.

---

Back to [05-extended-query-flow.md](05-extended-query-flow.md) | Next: [07-startup-auth-handshake.md](07-startup-auth-handshake.md)
