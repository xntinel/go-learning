# Exercise 18: Decoding Length-Prefixed Frames: len(), Not cap(), Fills a Read

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Length-prefixed framing is the shape most binary wire protocols reduce to
once you strip away everything protocol-specific: a fixed-width header
declaring how many bytes come next, followed by exactly that many bytes,
repeated for as long as the stream lasts. gRPC's own message framing over
HTTP/2 is a 5-byte header (a flag byte plus a 4-byte length) in front of
each protobuf message. Kafka's wire protocol frames every request and
response the same way, a 4-byte big-endian length in front of the payload.
Anyone who has written a decoder for either has written the same 15-line
loop this module builds standalone: read the header, read that many bytes,
hand the payload to the caller, repeat until the connection closes.

That loop has exactly one place to get subtly wrong, and this lesson's own
preallocation idiom sets the trap. Everywhere else in this lesson,
`make([]byte, 0, n)` followed by `append` is the correct way to preallocate
a buffer of known final size -- zero length, full capacity, and `append`
fills it from the front. `io.ReadFull` does not fill a buffer by appending
to it. It fills *exactly* `len(buf)` bytes, because that is the contract of
`io.Reader.Read`: a `Read` call is asked for `len(p)` bytes and is never
entitled to look at `cap(p)`. Hand `io.ReadFull` a `make([]byte, 0, n)`
buffer and it is asked to fill zero bytes -- which it does, immediately,
successfully, with no error at all. The `n` real bytes of payload are left
sitting in the stream, and the next call to read a header reads payload
bytes instead, desynchronizing the decoder for every frame that follows.

This module builds `framedecode`, a command-line tool that reads a stream of
length-prefixed frames from stdin and writes each payload as one
hex-encoded line to stdout, guarding against a corrupt or hostile length
prefix with a configurable maximum before it ever allocates a buffer for
the payload.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
framedecode/                   module example.com/framedecode
  go.mod                       go 1.24
  framedecode.go                package main — FrameReader; NewFrameReader, Next
  framedecode_test.go            package main — frame table, oversized/truncated frames,
                                 the make(0,n)-into-ReadFull contrast, run() end to end
  main.go                       package main — -max flag, exit codes
```

- Files: `framedecode.go`, `framedecode_test.go`, `main.go`.
- Implement: `NewFrameReader(r io.Reader, maxSize int) (*FrameReader, error)` rejecting a non-positive `maxSize`; `(*FrameReader).Next() ([]byte, error)` reading a 4-byte big-endian length header then exactly that many payload bytes with `make([]byte, length)` (not `make([]byte, 0, length)`), returning `io.EOF` at a clean stream boundary, `ErrFrameTooLarge` before allocating a buffer for an over-budget length, and `ErrTruncatedFrame` for a header or payload cut short.
- Tool: `framedecode` reads binary length-prefixed frames from stdin and writes one hex-encoded line per payload to stdout. `-max` bounds the accepted payload size per frame (default 1 MiB). Exit 0 on a clean stream; exit 2 for a bad flag, a frame declaring a length above `-max`, or a truncated frame; exit 1 is reserved for a runtime failure this tool does not otherwise produce.
- Test: the frame table (single frame, multiple frames, a zero-length payload); a length above `maxSize` rejected before the payload is read; a truncated header and a truncated payload both rejected; a non-positive `maxSize` rejected; a `readPayloadBuggy` contrast proving `make([]byte, 0, n)` handed to `io.ReadFull` silently reads nothing; and `run` end to end over a `bytes.Buffer` stdin and stdout.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/framedecode
cd ~/go-exercises/framedecode
go mod init example.com/framedecode
go mod edit -go=1.24
```

### Read fills len(buf), never cap(buf)

`io.Reader.Read(p []byte)` is specified in terms of `len(p)`: it reads up to
`len(p)` bytes into `p` and reports how many it actually read. Nothing in
that contract mentions capacity, because capacity is not part of what `Read`
is given -- a `[]byte` argument is passed as a header, and the only field of
that header a `Read` implementation is entitled to act on for sizing the
read is length. `io.ReadFull` layers "keep calling `Read` until `p` is
completely full or an error occurs" on top of that same contract, so it
inherits the same rule: it fills `len(p)` bytes, full stop.

```go
buf := make([]byte, 0, n)   // len 0, cap n: correct for append, wrong here
_, err := io.ReadFull(r, buf)
// err == nil, buf is still length 0: ReadFull was asked for zero bytes
// and delivered exactly zero bytes. The n real bytes are still in r.
```

There is no error here because nothing went wrong by the function's own
contract -- `ReadFull` was asked to fill a zero-length slice and it did.
The bug is entirely upstream, in the caller's choice to preallocate with
`make([]byte, 0, n)` instead of `make([]byte, n)`. The fix is one word: drop
the `0`. `make([]byte, length)` creates `length` addressable, zero-valued
bytes for `ReadFull` to overwrite, which is exactly what a fixed-size read
needs -- the same "create the elements, then fill them by index" form this
lesson treats as the correct alternative to the append-based idiom, just
applied here because `Read` fills by index, not by appending.

Create `framedecode.go`:

```go
// framedecode decodes a length-prefixed binary framing -- the shape gRPC and
// Kafka's own wire protocols both use: a fixed-width length header followed
// by exactly that many payload bytes, repeated until the stream ends.
//
// The detail this file exists to get right is what io.ReadFull actually
// fills: at most len(buf) bytes, never cap(buf). Elsewhere in this lesson
// make([]byte, 0, n) followed by append is the correct way to preallocate a
// known-size buffer. Handed to io.ReadFull instead of append, that same
// idiom is wrong -- a zero-length buffer asks Read for zero bytes and gets
// them, silently, no error, leaving the real payload sitting unread in the
// stream. See the package tests for that exact failure, isolated from the
// FrameReader type below.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// headerSize is the width of the frame length prefix: a big-endian uint32.
const headerSize = 4

// ErrFrameTooLarge is returned by Next when a frame's declared length
// exceeds the FrameReader's configured maximum.
var ErrFrameTooLarge = errors.New("framedecode: length prefix exceeds maximum frame size")

// ErrTruncatedFrame is returned by Next when the stream ends in the middle
// of a length header or a payload.
var ErrTruncatedFrame = errors.New("framedecode: frame truncated before it was complete")

// FrameReader decodes a stream of length-prefixed frames: a 4-byte
// big-endian length header, then that many payload bytes, repeated.
//
// FrameReader is not safe for concurrent use; it holds read position in the
// underlying io.Reader, and Next must not be called from more than one
// goroutine at a time.
type FrameReader struct {
	r       io.Reader
	maxSize uint32
}

// NewFrameReader returns a FrameReader that reads frames from r, rejecting
// any frame whose declared length exceeds maxSize before allocating a
// buffer for it. It returns an error if maxSize is not positive.
func NewFrameReader(r io.Reader, maxSize int) (*FrameReader, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("framedecode: max frame size must be positive, got %d", maxSize)
	}
	return &FrameReader{r: r, maxSize: uint32(maxSize)}, nil
}

// Next reads and returns the next frame's payload.
//
// It returns io.EOF, unwrapped, when the stream ends cleanly between two
// frames -- that is the normal way a stream of frames ends. A length header
// or payload cut short by an early end of stream returns an error wrapping
// both ErrTruncatedFrame and the underlying io.ErrUnexpectedEOF. A declared
// length greater than maxSize returns an error wrapping ErrFrameTooLarge
// without allocating a buffer for the oversized payload, which is what
// keeps a corrupt or hostile length prefix from causing an unbounded
// allocation.
//
// The returned slice is freshly allocated for this call; it does not alias
// the slice from any other call.
func (f *FrameReader) Next() ([]byte, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(f.r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: reading length header: %v", ErrTruncatedFrame, err)
	}

	length := binary.BigEndian.Uint32(header[:])
	if length > f.maxSize {
		return nil, fmt.Errorf("%w: declared %d, max %d", ErrFrameTooLarge, length, f.maxSize)
	}

	// make([]byte, length): io.ReadFull fills exactly len(payload) bytes.
	// make([]byte, 0, length) would give ReadFull a zero-length buffer --
	// it would ask for zero bytes, receive zero bytes, and return a nil
	// error, having read nothing. See readPayloadBuggy in the test file.
	payload := make([]byte, length)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return nil, fmt.Errorf("%w: reading %d-byte payload: %v", ErrTruncatedFrame, length, err)
	}
	return payload, nil
}
```

### The tool

`run` takes the flag arguments, an `io.Reader` for stdin, and an `io.Writer`
for stdout -- nothing touches `os.Args`, `os.Stdin`, or `os.Exit`, so a test
can drive it end to end with a `bytes.Buffer` on each side. Streaming is the
point: `run` never buffers the whole input, it calls `Next` once per frame
and writes one line before asking for the next, so a decoder built this way
handles a connection that is still open and still producing frames exactly
as well as a finite file. Every failure `run` can produce -- a bad flag, an
over-budget length, a truncated frame -- is something the caller fixes by
changing the input or the command line, so all three wrap the `errUsage`
sentinel and map to exit code 2; exit code 1 is reserved for a runtime
failure this particular tool has no path to.

Create `main.go`:

```go
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input or the
// command line: a bad flag, an oversized frame, or a truncated stream.
// main maps it to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads length-prefixed frames from stdin and writes each payload as one
// hex-encoded line to stdout. It never touches os.Args or os.Exit, so it can
// be exercised in a test against a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("framedecode", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxSize := fs.Int("max", 1<<20, "maximum accepted payload size in bytes, per frame")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	fr, err := NewFrameReader(stdin, *maxSize)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	for i := 0; ; i++ {
		payload, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: frame %d: %v", errUsage, i, err)
		}
		fmt.Fprintln(stdout, hex.EncodeToString(payload))
	}
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: framedecode [-max N] < frames.bin")
		fmt.Fprintln(os.Stderr, "decodes length-prefixed binary frames from stdin, one hex-encoded payload per output line.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "framedecode:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '\x00\x00\x00\x05hello\x00\x00\x00\x02hi' | go run .
printf '\x00\x00\x00\x64' | go run . -max 4
printf '\x00\x00\x00\x05hel' | go run .
```

Expected output:

```text
68656c6c6f
6869
framedecode: usage: frame 0: framedecode: length prefix exceeds maximum frame size: declared 100, max 4
framedecode: usage: frame 0: framedecode: frame truncated before it was complete: reading 5-byte payload: unexpected EOF
```

The first command decodes two well-formed frames, `"hello"` and `"hi"`, to
their hex encodings, one per line, exit 0. The second sends a header
declaring a 100-byte payload against `-max 4`: `ErrFrameTooLarge` fires
before any payload buffer is allocated, exit 2. The third sends a header
declaring 5 bytes but only 3 arrive: `ErrTruncatedFrame` fires wrapping the
underlying `unexpected EOF`, exit 2.

### Tests

`TestFrameReaderNext` is the table: a single frame, several frames back to
back, and a frame with a zero-length payload, each checked against `io.EOF`
at the stream's end. `TestFrameReaderRejectsOversizedLength` and
`TestFrameReaderRejectsTruncation` pin the two error paths `Next` documents,
the latter covering both a header cut short and a payload cut short.
`TestNewFrameReaderRejectsNonPositiveMax` covers the constructor's
validation.

`TestReadPayloadBuggySilentlyReadsNothing` is the heart of the module.
`readPayloadBuggy` is unexported and unreachable from the package API; it
reproduces the exact preallocation mistake described above. The test calls
it against a reader holding five real bytes, asserts the call succeeds with
a nil error and a zero-length result, and then reads the same reader again
to show the five bytes are still sitting there, completely untouched --
proving the bug is silent, not merely wrong. `TestRun` drives the whole tool
end to end: valid streams against their expected hex output, an oversized
frame, a truncated frame, and an unknown flag, all three failures asserted
to wrap `errUsage`.

Create `framedecode_test.go`:

```go
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// encodeFrame builds one length-prefixed frame: a 4-byte big-endian length
// header followed by payload.
func encodeFrame(payload []byte) []byte {
	var header [headerSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	return append(header[:], payload...)
}

// readPayloadBuggy is the mistake this module is about, isolated. It looks
// like the preallocation idiom used correctly elsewhere in this lesson --
// make with zero length and known capacity -- but handed to io.ReadFull
// instead of append, it asks Read for zero bytes and gets them: no error,
// no payload, and the real bytes are left sitting unread in r for whatever
// reads next. It is never exported and never reachable from the package
// API; it exists only so the tests can pin what it gets wrong.
func readPayloadBuggy(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, 0, n) // BUG: len 0, so ReadFull reads nothing
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func TestFrameReaderNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		frames [][]byte
		max    int
	}{
		{name: "single frame", frames: [][]byte{{0xDE, 0xAD, 0xBE, 0xEF}}, max: 1024},
		{name: "multiple frames", frames: [][]byte{{0x01}, {0x02, 0x03}, {0x04, 0x05, 0x06}}, max: 1024},
		{name: "empty payload", frames: [][]byte{{}}, max: 1024},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stream bytes.Buffer
			for _, f := range tc.frames {
				stream.Write(encodeFrame(f))
			}

			fr, err := NewFrameReader(&stream, tc.max)
			if err != nil {
				t.Fatalf("NewFrameReader: %v", err)
			}
			for i, want := range tc.frames {
				got, err := fr.Next()
				if err != nil {
					t.Fatalf("Next() frame %d: %v", i, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("Next() frame %d = %x, want %x", i, got, want)
				}
			}
			if _, err := fr.Next(); !errors.Is(err, io.EOF) {
				t.Fatalf("Next() after last frame: err = %v, want io.EOF", err)
			}
		})
	}
}

func TestFrameReaderRejectsOversizedLength(t *testing.T) {
	t.Parallel()

	stream := bytes.NewReader(encodeFrame(make([]byte, 100)))
	fr, err := NewFrameReader(stream, 10)
	if err != nil {
		t.Fatalf("NewFrameReader: %v", err)
	}
	if _, err := fr.Next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Next() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameReaderRejectsTruncation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "truncated header", raw: []byte{0x00, 0x00}},
		{name: "truncated payload", raw: func() []byte {
			full := encodeFrame([]byte{1, 2, 3, 4, 5})
			return full[:len(full)-2]
		}()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fr, err := NewFrameReader(bytes.NewReader(tc.raw), 1024)
			if err != nil {
				t.Fatalf("NewFrameReader: %v", err)
			}
			if _, err := fr.Next(); !errors.Is(err, ErrTruncatedFrame) {
				t.Fatalf("Next() error = %v, want ErrTruncatedFrame", err)
			}
		})
	}
}

func TestNewFrameReaderRejectsNonPositiveMax(t *testing.T) {
	t.Parallel()

	for _, max := range []int{0, -1} {
		if _, err := NewFrameReader(strings.NewReader(""), max); err == nil {
			t.Errorf("NewFrameReader(max=%d): want error, got nil", max)
		}
	}
}

// TestReadPayloadBuggySilentlyReadsNothing is the whole point of the
// module: it pins the exact failure make([]byte, 0, n) ships when it is
// handed to io.ReadFull instead of append. The call succeeds -- err is nil
// -- and returns a zero-length slice, even though n real bytes are still
// sitting unread in the reader.
func TestReadPayloadBuggySilentlyReadsNothing(t *testing.T) {
	t.Parallel()

	payload := []byte{1, 2, 3, 4, 5}
	r := bytes.NewReader(payload)

	got, err := readPayloadBuggy(r, len(payload))
	if err != nil {
		t.Fatalf("readPayloadBuggy: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(readPayloadBuggy(...)) = %d, want 0 (the bug is that it reads nothing)", len(got))
	}

	// The real payload is still sitting in r, unread -- proving the bytes
	// were never consumed, not merely discarded.
	remaining := make([]byte, len(payload))
	if _, err := io.ReadFull(r, remaining); err != nil {
		t.Fatalf("reading what should be the untouched payload: %v", err)
	}
	if !bytes.Equal(remaining, payload) {
		t.Fatalf("remaining bytes = %x, want %x", remaining, payload)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		frames    [][]byte
		rawStdin  []byte
		wantLines []string
		wantErr   bool
	}{
		{
			name:      "two frames decode to two hex lines",
			frames:    [][]byte{{0xDE, 0xAD}, {0x01, 0x02, 0x03}},
			wantLines: []string{"dead", "010203"},
		},
		{
			name:      "empty stream produces no lines",
			frames:    nil,
			wantLines: nil,
		},
		{
			name:    "frame exceeds -max",
			args:    []string{"-max", "2"},
			frames:  [][]byte{{0x01, 0x02, 0x03, 0x04}},
			wantErr: true,
		},
		{
			name:     "truncated frame",
			rawStdin: []byte{0x00, 0x00, 0x00, 0x05, 0x01, 0x02},
			wantErr:  true,
		},
		{
			name:    "unknown flag",
			args:    []string{"-bogus"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdin bytes.Buffer
			if tc.rawStdin != nil {
				stdin.Write(tc.rawStdin)
			} else {
				for _, f := range tc.frames {
					stdin.Write(encodeFrame(f))
				}
			}

			var stdout bytes.Buffer
			err := run(tc.args, &stdin, &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}

			var wantOut string
			for _, line := range tc.wantLines {
				wantOut += line + "\n"
			}
			if stdout.String() != wantOut {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), wantOut)
			}
		})
	}
}
```

## Review

`Next` is correct when it fills exactly the number of bytes a frame's own
header declares, no more and no fewer, which is what `make([]byte, length)`
guarantees and `make([]byte, 0, length)` does not: `io.ReadFull` fills
`len(buf)`, never `cap(buf)`, so a zero-length buffer reads zero bytes and
reports success while the real payload sits unread in the stream. `Next`
checks a frame's declared length against the configured maximum before
allocating anything for it, which is what turns a corrupt or hostile length
prefix into `ErrFrameTooLarge` instead of an unbounded allocation. `run`
streams frame by frame rather than buffering the whole input, maps every
input mistake -- a bad flag, an over-budget length, a truncated frame -- to
exit code 2 through the shared `errUsage` sentinel, and reserves exit code 1
for a runtime failure this tool has no path to. Run
`go test -count=1 -race ./...` to confirm the frame table, the two rejection
paths, the `make(0, n)`-into-`ReadFull` contrast, and `run`'s end-to-end
behavior.

## Resources

- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the exact semantics this module hinges on: it fills `len(buf)`, never `cap(buf)`.
- [`io.Reader`](https://pkg.go.dev/io#Reader) — the `Read(p []byte)` contract, specified entirely in terms of `len(p)`.
- [gRPC over HTTP/2: Length-Prefixed-Message](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#length-prefixed-message) — the production framing this exercise's format mirrors.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.PutUint32`/`Uint32`, used to encode and decode the length header.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-struct-shallow-copy-slice-field.md](17-struct-shallow-copy-slice-field.md) | Next: [19-flat-backing-array-2d-grid.md](19-flat-backing-array-2d-grid.md)
