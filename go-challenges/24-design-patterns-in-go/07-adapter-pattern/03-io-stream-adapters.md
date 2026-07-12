# Exercise 3: Adapting To The io.Reader And io.Writer Seams

The previous two exercises adapted toward vendor SDKs. This one adapts toward the standard library, and that changes the economics entirely: when the far side of your adapter is `io.Reader` or `io.Writer`, you do not gain one integration, you gain hundreds. Anything that reads bytes — `io.Copy`, `bufio.Scanner`, `compress/gzip`, `encoding/json`, an `http.Request` body — instantly accepts your domain producer once it is an `io.Reader`, and anything that writes bytes accepts your domain consumer once it is an `io.Writer`. This exercise builds both directions: a `SourceReader` that turns a pull-based domain `LineSource` into an `io.Reader`, and a `LineWriter` that turns an `io.Writer` into calls on a push-based domain `LineSink`.

This module is fully self-contained. It begins with its own `go mod init`, depends only on the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bridge.go                         LineSource, LineSink ports; SourceReader (io.Reader), LineWriter (io.Writer)
cmd/
  demo/main.go                    pipe a domain source through io.Copy into a domain sink
bridge_test.go                    ReadAll round-trip, 1-byte reads, bufio.Scanner, split/flush, sink error
```

- Files: `bridge.go`, `cmd/demo/main.go`, `bridge_test.go`.
- Implement: `SourceReader.Read` (fills `p`, buffers the unread tail, returns `io.EOF` only when drained) and `LineWriter.Write` (splits on `'\n'`, buffers a partial line) plus `LineWriter.Flush`; the `SliceSource` and `SliceSink` convenience types.
- Test: `io.ReadAll` round-trip, reassembly under 1-byte reads, integration with `bufio.Scanner`, line splitting across `Write` boundaries, `Flush` of a trailing partial line, and propagation of a sink error.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/07-adapter-pattern/03-io-stream-adapters/cmd/demo
cd go-solutions/24-design-patterns-in-go/07-adapter-pattern/03-io-stream-adapters
```

### Reading: the io.Reader contract is the hard part

A domain `LineSource` is a pull API: call `Next()` and it hands back the next line and a boolean that goes `false` at the end. The standard library, by contrast, wants to *pull bytes* on its own schedule, through `Read(p []byte) (int, error)`, into a buffer it owns and sizes. The `SourceReader` bridges the two, and the entire difficulty is honoring the `io.Reader` contract, which is more subtle than it looks.

Three rules govern a correct `Read`. It must fill as much of `p` as it can and return the count; callers tolerate a short read (fewer than `len(p)` bytes) with a `nil` error, so you do not have to fill `p` completely, but you should not return `0, nil` while data is still available, because that spins the caller. It must return `io.EOF` only when the stream is genuinely exhausted — and the clean way is to return `n, nil` while bytes remain and `0, io.EOF` on the following call, once the source is drained and the buffer is empty. And it must survive a `p` smaller than a line: if a line does not fit, the leftover bytes have to be remembered, because the next `Read` arrives with a fresh `p` and must continue exactly where the previous one stopped.

The implementation meets all three with a single `buf` field holding the unread tail of the current line. The loop fills `p` from `buf`, and whenever `buf` empties it pulls the next line from the source, appends a `'\n'`, and continues. When `Next()` returns `false`, it sets `done` and stops; if it has already copied some bytes this call it returns them with `nil`, otherwise it returns `io.EOF`. The 1-byte-read test exists precisely to prove the buffering is correct: reading one byte at a time is the most hostile schedule a caller can impose, and the reassembled output must still be byte-for-byte identical.

### Writing: split, buffer, and flush

The write direction is the mirror. A domain `LineSink` is a push API: hand it a complete line with `WriteLine(string)`. The standard library, though, writes *arbitrary byte chunks* through `Write(p []byte) (int, error)`, and those chunks do not respect line boundaries — a single `Write` may carry several lines and half of another, and the next `Write` may carry the rest of that half. `LineWriter` accumulates bytes in a buffer, emits a `WriteLine` for every complete `'\n'`-terminated line it finds, and holds whatever trailing bytes are left over until either the next newline arrives or `Flush` is called.

The `io.Writer` contract has its own rule: if `Write` returns `n < len(p)` it must return a non-nil error, and it must not retain `p` after returning. `LineWriter` copies every byte of `p` into its own buffer immediately (via `append`, so `p` is never retained), which means it can always report `n == len(p)`. If a downstream `WriteLine` then fails, `Write` returns `len(p)` together with the sink's error — the bytes were accepted into the buffer, and the error reports that a line could not be delivered. That is a deliberate, documented choice; the contract forbids only the reverse (a short count without an error), so reporting a full count alongside an error is legitimate and is the honest description of what happened. `Flush` exists because the last line of a stream is often not newline-terminated, and without an explicit flush those final bytes would sit in the buffer forever.

Create `bridge.go`:

```go
// Package iobridge adapts domain stream types to the standard io.Reader and
// io.Writer seams, so domain producers and consumers can plug into io.Copy,
// bufio.Scanner, compress/gzip, net/http, and the rest of the io ecosystem.
package iobridge

import (
	"bytes"
	"io"
)

// LineSource is a domain pull-based producer of text lines. It knows nothing
// about bytes, buffers, or io.Reader.
type LineSource interface {
	Next() (line string, ok bool)
}

// LineSink is a domain push-based consumer of text lines. It knows nothing
// about bytes, buffers, or io.Writer.
type LineSink interface {
	WriteLine(line string) error
}

// SourceReader adapts a LineSource to io.Reader. Each line the source yields is
// emitted followed by a single '\n'. The reader keeps the unread tail of the
// current line in buf so a small p across calls is handled correctly.
type SourceReader struct {
	src  LineSource
	buf  []byte
	done bool
}

// NewSourceReader wraps src so it can be read as a byte stream.
func NewSourceReader(src LineSource) *SourceReader {
	return &SourceReader{src: src}
}

// Read implements io.Reader. It fills p from the buffered tail and from freshly
// pulled lines, returning io.EOF only once the source is exhausted and no
// buffered bytes remain.
func (r *SourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for total < len(p) {
		if len(r.buf) == 0 {
			if r.done {
				break
			}
			line, ok := r.src.Next()
			if !ok {
				r.done = true
				break
			}
			r.buf = append([]byte(line), '\n')
		}
		n := copy(p[total:], r.buf)
		r.buf = r.buf[n:]
		total += n
	}
	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

// LineWriter adapts a LineSink to io.Writer. Bytes written are split on '\n';
// each complete line is handed to the sink. A trailing partial line stays in
// buf until the next newline arrives or Flush is called.
type LineWriter struct {
	sink LineSink
	buf  []byte
}

// NewLineWriter wraps sink so bytes can be written into it.
func NewLineWriter(sink LineSink) *LineWriter {
	return &LineWriter{sink: sink}
}

// Write implements io.Writer. Every byte of p is copied into the internal
// buffer, so Write always reports n == len(p); a sink failure surfaces through
// the returned error while the unflushed bytes remain buffered.
func (w *LineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if err := w.sink.WriteLine(string(w.buf[:i])); err != nil {
			return len(p), err
		}
		w.buf = w.buf[i+1:]
	}
}

// Flush emits any buffered bytes that were not terminated by a newline as a
// final line. It is a no-op when the buffer is empty.
func (w *LineWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	line := string(w.buf)
	w.buf = w.buf[:0]
	return w.sink.WriteLine(line)
}

// SliceSource is a LineSource backed by an in-memory slice of lines.
type SliceSource struct {
	Lines []string
	pos   int
}

// Next yields the next line, or ("", false) once the slice is exhausted.
func (s *SliceSource) Next() (string, bool) {
	if s.pos >= len(s.Lines) {
		return "", false
	}
	line := s.Lines[s.pos]
	s.pos++
	return line, true
}

// SliceSink is a LineSink that appends every received line to Lines.
type SliceSink struct {
	Lines []string
}

// WriteLine records line.
func (s *SliceSink) WriteLine(line string) error {
	s.Lines = append(s.Lines, line)
	return nil
}

var (
	_ io.Reader = (*SourceReader)(nil)
	_ io.Writer = (*LineWriter)(nil)
)
```

The two `var _ io.Reader`/`io.Writer` assertions are the point of the whole exercise stated as compile-time facts: `*SourceReader` *is* an `io.Reader` and `*LineWriter` *is* an `io.Writer`, so both drop into any standard-library function that names those interfaces.

### A runnable demo

The demo is the payoff. It builds a domain `SliceSource` on the left and a domain `SliceSink` on the right and connects them with `io.Copy` — the standard library moves the bytes, and the two adapters translate at each end, with no glue code in between. It then reuses a `SourceReader` as the input to a `bufio.Scanner`, and finally shows a trailing partial line staying buffered until `Flush`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"io"

	"example.com/iobridge"
)

func main() {
	// A domain source on the left, a domain sink on the right, and the standard
	// library's io.Copy moving bytes between them through two adapters.
	src := &iobridge.SliceSource{Lines: []string{"alpha", "beta", "gamma"}}
	sink := &iobridge.SliceSink{}
	w := iobridge.NewLineWriter(sink)

	n, err := io.Copy(w, iobridge.NewSourceReader(src))
	if err != nil {
		fmt.Println("copy error:", err)
		return
	}
	fmt.Printf("copied %d bytes through io.Copy\n", n)
	fmt.Printf("sink received: %v\n", sink.Lines)

	// The same SourceReader plugs straight into bufio.Scanner.
	src2 := &iobridge.SliceSource{Lines: []string{"one", "two", "three"}}
	sc := bufio.NewScanner(iobridge.NewSourceReader(src2))
	count := 0
	for sc.Scan() {
		count++
	}
	fmt.Printf("scanner saw %d lines\n", count)

	// A trailing partial line stays buffered until Flush.
	sink2 := &iobridge.SliceSink{}
	w2 := iobridge.NewLineWriter(sink2)
	io.WriteString(w2, "partial without newline")
	fmt.Printf("before flush: %v\n", sink2.Lines)
	w2.Flush()
	fmt.Printf("after flush: %v\n", sink2.Lines)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
copied 17 bytes through io.Copy
sink received: [alpha beta gamma]
scanner saw 3 lines
before flush: []
after flush: [partial without newline]
```

### Tests

The tests verify both seams and their contracts. `TestSourceReader_ReadAllRoundTrip` confirms `io.ReadAll` drains the reader to the exact expected bytes. `TestSourceReader_TinyBufferReassembles` forces 1-byte reads to prove the unread-tail buffering survives the worst-case schedule. `TestSourceReader_PlugsIntoBufioScanner` proves real standard-library code consumes the adapter. `TestSourceReader_EmptySourceIsEOF` pins the drained-stream behavior. On the write side, `TestLineWriter_SplitsOnNewline` proves a line split across two `Write` calls is reassembled, `TestLineWriter_FlushEmitsTrailingPartial` pins the flush semantics, and `TestLineWriter_SurfacesSinkError` proves a sink failure reaches the caller with the full byte count. `TestPipe_SourceThroughCopyIntoSink` exercises the end-to-end `io.Copy` pipeline, and `TestLineWriter_IsAnIOWriter` shows the same call site accepting both a stdlib writer and ours.

Create `bridge_test.go`:

```go
package iobridge

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSourceReader_ReadAllRoundTrip(t *testing.T) {
	t.Parallel()

	src := &SliceSource{Lines: []string{"alpha", "beta", "gamma"}}
	got, err := io.ReadAll(NewSourceReader(src))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "alpha\nbeta\ngamma\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSourceReader_TinyBufferReassembles(t *testing.T) {
	t.Parallel()

	src := &SliceSource{Lines: []string{"hello", "world"}}
	r := NewSourceReader(src)

	var out []byte
	p := make([]byte, 1) // force many short reads across line boundaries
	for {
		n, err := r.Read(p)
		out = append(out, p[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if string(out) != "hello\nworld\n" {
		t.Fatalf("got %q", out)
	}
}

func TestSourceReader_PlugsIntoBufioScanner(t *testing.T) {
	t.Parallel()

	src := &SliceSource{Lines: []string{"one", "two", "three"}}
	sc := bufio.NewScanner(NewSourceReader(src))

	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if strings.Join(lines, ",") != "one,two,three" {
		t.Fatalf("got %v", lines)
	}
}

func TestSourceReader_EmptySourceIsEOF(t *testing.T) {
	t.Parallel()

	r := NewSourceReader(&SliceSource{})
	n, err := r.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("n=%d err=%v, want 0, io.EOF", n, err)
	}
}

func TestLineWriter_SplitsOnNewline(t *testing.T) {
	t.Parallel()

	sink := &SliceSink{}
	w := NewLineWriter(sink)

	if _, err := io.WriteString(w, "first\nsec"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.WriteString(w, "ond\nthird\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := []string{"first", "second", "third"}
	if strings.Join(sink.Lines, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", sink.Lines, want)
	}
}

func TestLineWriter_FlushEmitsTrailingPartial(t *testing.T) {
	t.Parallel()

	sink := &SliceSink{}
	w := NewLineWriter(sink)
	if _, err := io.WriteString(w, "no newline here"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(sink.Lines) != 0 {
		t.Fatalf("expected nothing flushed yet, got %v", sink.Lines)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(sink.Lines) != 1 || sink.Lines[0] != "no newline here" {
		t.Fatalf("got %v", sink.Lines)
	}
}

var errSinkFull = errors.New("sink full")

type failingSink struct{}

func (failingSink) WriteLine(string) error { return errSinkFull }

func TestLineWriter_SurfacesSinkError(t *testing.T) {
	t.Parallel()

	w := NewLineWriter(failingSink{})
	n, err := io.WriteString(w, "boom\n")
	if !errors.Is(err, errSinkFull) {
		t.Fatalf("err = %v, want errSinkFull", err)
	}
	if n != len("boom\n") {
		t.Fatalf("n = %d, want %d", n, len("boom\n"))
	}
}

func TestPipe_SourceThroughCopyIntoSink(t *testing.T) {
	t.Parallel()

	src := &SliceSource{Lines: []string{"red", "green", "blue"}}
	sink := &SliceSink{}
	w := NewLineWriter(sink)

	n, err := io.Copy(w, NewSourceReader(src))
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != int64(len("red\ngreen\nblue\n")) {
		t.Fatalf("copied %d bytes", n)
	}
	if strings.Join(sink.Lines, ",") != "red,green,blue" {
		t.Fatalf("got %v", sink.Lines)
	}
}

func TestLineWriter_IsAnIOWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	// Confirm the same call site accepts both the stdlib writer and ours.
	sinks := []io.Writer{&buf, NewLineWriter(&SliceSink{})}
	for _, s := range sinks {
		if _, err := io.WriteString(s, "line\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}
```

## Review

The adapters are correct when they satisfy the standard interfaces' contracts, not merely their signatures. For `SourceReader.Read`: it returns `0, io.EOF` only when the source is drained and the buffer is empty, it returns `n, nil` while bytes remain, and it never loses the tail of a line that did not fit in `p` — the 1-byte-read test is the proof, because any off-by-one in the buffering corrupts the reassembled output. For `LineWriter.Write`: it copies `p` rather than retaining it, it reports `n == len(p)`, and it buffers a partial line across calls so a line split between two `Write`s is still delivered whole; `Flush` releases the final unterminated line.

The mistakes the tests are shaped to catch are the classic `io` contract violations. A `Read` that returns the last bytes together with `io.EOF` and then forgets them loses a record; returning `0, nil` when momentarily empty spins `io.Copy` forever; dropping the unread tail garbles output under small buffers. On the write side, retaining `p` and reading it after returning is a data race waiting to happen, and forgetting `Flush` silently swallows the final line. Because both adapters hold per-instance buffers and no shared state, `go test -race ./...` over the parallel tests confirms there is nothing to race on, and the `bufio.Scanner` and `io.Copy` tests confirm real standard-library consumers are satisfied.

## Resources

- [`io.Reader` and `io.Writer`](https://pkg.go.dev/io#Reader) — the exact documented contracts, including the `io.EOF` and short-read rules and the "must not retain p" requirement.
- [The Go Programming Language: io interfaces](https://pkg.go.dev/io) — the package overview showing how many helpers (`Copy`, `ReadAll`, `MultiReader`, `TeeReader`) accept any `Reader`/`Writer`, which is why adapting to them is high-leverage.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented consumer the `SourceReader` is tested against, and a model for how line buffering is done in the standard library.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-payment-gateway-acl.md](02-payment-gateway-acl.md) | Next: [04-messaging-failover-adapter.md](04-messaging-failover-adapter.md)
