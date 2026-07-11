# Exercise 10: Frame Writer That Sizes Its Buffer By Length, Not Capacity

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every length-prefixed transport -- Cap'n Proto RPC, Thrift's framed transport,
gRPC-over-a-raw-socket -- has a write side that does the same thing: take a
payload, prepend a fixed-width length header, and hand the two-part buffer to
the socket in one `Write` call. The write side looks trivial compared to the
reader, which has to handle partial reads and truncated streams. That is
exactly why it is where a specific `copy` bug survives review: the encoder
allocates its scratch buffer with `make([]byte, 0, total)`, meaning to reserve
room, and then calls `copy` to fill it. `copy`'s bound is the *length* of the
destination, not its capacity, and the destination's length is zero. Every
call copies zero bytes. No panic, no error, no short-write warning -- the
frame that reaches the wire has whatever `make([]byte, 0, total)` and an
untouched header field leave behind, and downstream the reader either hangs
waiting for bytes that were never sent or decodes a frame from stale memory.

This module builds `framepack`, a command that reads one record per line of
stdin and writes one length-prefixed frame per record to stdout: a 4-byte
big-endian length followed by the payload, verbatim. The component underneath
it, `Encoder`, gets the buffer sizing right by construction, and the naive
version that gets it wrong lives only in the test file, isolated as the thing
the tests prove broken.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
framepack/                module example.com/framepack
  go.mod                   go 1.24
  framepack.go             package main — Encoder, NewEncoder, WriteFrame, encodeFrame
  framepack_test.go        package main — encode table, oversized record, reused-source
                            independence, the buggy-buffer contrast, run() end to end
  main.go                  package main — -max-record flag, stdin/stdout, exit codes
```

- Files: `framepack.go`, `framepack_test.go`, `main.go`.
- Implement: `NewEncoder(w io.Writer, maxRecord int) (*Encoder, error)` rejecting a non-positive `maxRecord` with `ErrInvalidMaxRecord`; `(*Encoder).WriteFrame(payload []byte) (int, error)` returning `ErrRecordTooLarge` when `len(payload) > maxRecord` and otherwise writing `encodeFrame(payload)` to the underlying writer in one call; `encodeFrame(payload []byte) []byte` sized with `make([]byte, 4+len(payload))` so `copy` has exactly the room it needs.
- Tool: `framepack` reads one record per stdin line and writes one frame per record to stdout. `-max-record` bounds the accepted payload size (default 65536). Exit 0 on success, exit 2 when a flag is invalid or a record exceeds `-max-record`, exit 1 for any other runtime failure (a write error on stdout, or a read error on stdin).
- Test: the encode table (empty, nil, single byte, typical record); `NewEncoder` rejecting non-positive `maxRecord`; `WriteFrame` accepting a record at the exact limit and rejecting one byte over while leaving the writer untouched; a reused source buffer proving each frame is independent of later mutation; `encodeFrameBuggy` contrast pinning the zero-length-frame defect; and `run` end to end over `strings.Reader` and `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/framepack
cd ~/go-exercises/framepack
go mod init example.com/framepack
go mod edit -go=1.24
```

### `copy`'s bound is `len(dst)`, never `cap(dst)`

`copy(dst, src)` copies `min(len(dst), len(src))` elements. It is not aware
of `cap(dst)` at all -- capacity is a promise about how far a slice's backing
array extends, not about how many elements currently exist to write into.
`make([]byte, 0, total)` honors that promise: the array is `total` bytes long,
but the slice's length is zero, so as far as `copy` is concerned there are
zero addressable destination elements. The naive frame encoder looks like
this:

```go
frame := make([]byte, 0, total)     // len 0, cap total: "room for the frame"
binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
copy(frame, hdr[:])                 // copies min(0, 4)  == 0 bytes
copy(frame[4:], payload)            // frame[4:] still has length total-4... but
return frame                        // frame itself is returned with length 0
```

The two-index expression `frame[4:]` is legal even though `len(frame)` is
zero -- slicing is bounded by *capacity*, not length -- so a version of this
bug can look like it copies the payload correctly while still shipping a
frame with a garbage or zeroed header, depending on exactly how the buffer
is built. The version this module pins in its test file collapses both
copies into one, against the zero-length `frame` itself, which is the
shape this mistake most often takes in a real encoder: build the whole frame
in a correctly-sized scratch buffer, then `copy` it into the "reserved"
destination and return that destination. The fix does not add a check --
it sizes the buffer by length in the first place, so `copy` (or, as here,
direct field writes into the same buffer) has real elements to write into
from the start.

Create `framepack.go`:

```go
// Command framepack encodes each line of stdin as a length-prefixed RPC
// frame: a 4-byte big-endian length followed by the payload, the same
// framing style used by Cap'n Proto RPC and Thrift's framed transport. See
// main.go for the command-line driver and encodeFrame below for the part
// that matters: how the frame's scratch buffer is sized.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidMaxRecord means the configured maximum record size was not
// positive.
var ErrInvalidMaxRecord = errors.New("framepack: max record size must be positive")

// ErrRecordTooLarge means a payload exceeded the configured maximum record
// size.
var ErrRecordTooLarge = errors.New("framepack: record exceeds max size")

// Encoder writes length-prefixed frames to an underlying io.Writer.
//
// Not safe for concurrent use by multiple goroutines; the caller must
// synchronize calls to WriteFrame.
type Encoder struct {
	w         io.Writer
	maxRecord int
}

// NewEncoder returns an Encoder that writes frames to w, rejecting any
// payload longer than maxRecord bytes. It returns ErrInvalidMaxRecord if
// maxRecord is not positive.
func NewEncoder(w io.Writer, maxRecord int) (*Encoder, error) {
	if maxRecord <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxRecord, maxRecord)
	}
	return &Encoder{w: w, maxRecord: maxRecord}, nil
}

// WriteFrame encodes payload as a length-prefixed frame and writes it to the
// underlying writer in a single Write call. It returns ErrRecordTooLarge,
// wrapped with the offending size, if len(payload) exceeds the configured
// maximum, and leaves the underlying writer untouched in that case.
//
// WriteFrame does not retain payload: the returned byte count and error
// aside, the caller may reuse or overwrite payload immediately after
// WriteFrame returns.
func (e *Encoder) WriteFrame(payload []byte) (int, error) {
	if len(payload) > e.maxRecord {
		return 0, fmt.Errorf("%w: %d bytes exceeds max %d", ErrRecordTooLarge, len(payload), e.maxRecord)
	}
	return e.w.Write(encodeFrame(payload))
}

// encodeFrame builds one frame: a 4-byte big-endian length header followed
// by payload. The destination is sized by length, make([]byte, 4+len(payload)),
// so copy has exactly the room it needs. Sizing it by capacity instead --
// make([]byte, 0, 4+len(payload)) -- would leave len(dst) == 0, and copy's
// bound is min(len(dst), len(src)), never cap(dst): every copy would copy
// zero bytes, and the frame would ship with its length header correct but
// its payload silently empty. No panic, no error, just a wrong frame on the
// wire. See encodeFrameBuggy in framepack_test.go for that version, isolated
// so it never reaches this file's API.
func encodeFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}
```

### The tool

`framepack` has no interesting flags beyond `-max-record`, so `run` takes
the argument slice, an `io.Reader` for stdin, and an `io.Writer` for stdout
-- nothing tied to `os.Stdin`/`os.Stdout` directly -- which makes it
trivial to drive end to end from a table test with a `strings.Reader` and a
`bytes.Buffer`. It streams: `bufio.Scanner` reads one line at a time and
`WriteFrame` writes one frame at a time, so the tool's memory use does not
grow with the number of records, only with the longest single line. Every
failure `run` can produce before a write actually fails -- a bad flag, an
invalid `-max-record`, a record over the limit -- is a usage error the
caller fixes by changing the input, so all three wrap the `errUsage`
sentinel and `main` maps that to exit code 2. A failure writing to stdout or
reading from stdin is a runtime failure with no fix on the caller's command
line, so it is left unwrapped and maps to exit code 1.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input: a bad flag, an invalid -max-record, or a record that
// exceeds it. main maps it to exit code 2; every other error maps to 1.
var errUsage = errors.New("usage")

// run parses args, reads one record per line of stdin, and writes one
// length-prefixed frame per record to stdout. It never touches os.Stdin,
// os.Stdout, or os.Exit directly, so it can be driven end to end in a test
// with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("framepack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxRecord := fs.Int("max-record", 1<<16, "maximum record size in bytes")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	enc, err := NewEncoder(stdout, *maxRecord)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		if _, err := enc.WriteFrame(scanner.Bytes()); err != nil {
			if errors.Is(err, ErrRecordTooLarge) {
				return fmt.Errorf("%w: line %d: %v", errUsage, line, err)
			}
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: framepack -max-record N")
		fmt.Fprintln(os.Stderr, "reads one record per stdin line, writes a length-prefixed frame per record to stdout.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "framepack:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'ping\npong\n' | go run . -max-record 64 | xxd
printf 'toolong\n' | go run . -max-record 4
```

Expected output:

```text
00000000: 0000 0004 7069 6e67 0000 0004 706f 6e67  ....ping....pong
framepack: usage: line 1: framepack: record exceeds max size: 7 bytes exceeds max 4
```

The hex dump is two frames back to back: `0000 0004` (the big-endian length
4) followed by `ping`'s four bytes, then the same shape for `pong`. There is
no daylight between them and no phantom zero bytes -- exactly what
`make([]byte, 4+len(payload))` guarantees and `make([]byte, 0, total)` would
not. The second command feeds a 7-byte record through a 4-byte limit: the
tool refuses it before writing anything, reports the line number, and exits
2 -- a usage error, because the fix is a smaller record or a larger
`-max-record`, not a retry.

### Tests

`TestEncodeFrame` is the table: empty, nil, one byte, and a typical record,
each checked by decoding the frame back and comparing the length header and
payload. `TestNewEncoderRejectsNonPositiveMaxRecord` pins the constructor's
sentinel. `TestWriteFrameLimit` is the boundary table: a payload at exactly
`maxRecord` succeeds and decodes back correctly, one byte over is rejected
with `ErrRecordTooLarge` and leaves the underlying writer untouched.
`TestWriteFrameIndependentOfReusedSource` writes a frame, mutates the source
slice in place -- exactly what a caller looping over `scanner.Bytes()` does
on every iteration -- and confirms the first frame did not change, which
only holds because `encodeFrame` copies into a fresh buffer rather than
aliasing the caller's slice.

`TestBuggyEncoderShipsEmptyFrames` is the heart of the module.
`encodeFrameBuggy` is unexported and unreachable from `Encoder`; it builds
the frame correctly in a temporary buffer and then `copy`s it into a
`make([]byte, 0, total)` destination, so the copy always writes zero bytes
and the function returns a slice of length zero regardless of payload size.
The test asserts exactly that -- `len(encodeFrameBuggy(p)) == 0` for every
payload tried -- against `encodeFrame` producing the correct `4+len(p)`. If
a future edit reintroduces a capacity-only scratch buffer into `encodeFrame`,
this test fails here instead of a peer service hanging on a socket read.
`TestRun` drives the command end to end: multi-record input including a
blank line (a valid zero-length record), an oversized record, and a
non-positive `-max-record`, all checked against `errUsage`.

Create `framepack_test.go`:

```go
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// encodeFrameBuggy sizes its scratch buffer by capacity, not length -- the
// mistake encodeFrame's doc comment warns about. Never exported, never
// reachable from Encoder; exists only so the tests can pin what it gets wrong.
func encodeFrameBuggy(payload []byte) []byte {
	total := 4 + len(payload)
	frame := make([]byte, 0, total) // BUG: length 0, only capacity reserved

	src := make([]byte, total)
	binary.BigEndian.PutUint32(src[:4], uint32(len(payload)))
	copy(src[4:], payload)

	copy(frame, src) // dst has length 0, so this copies zero bytes, always
	return frame
}

func decodeFrame(t *testing.T, buf []byte) (payload, rest []byte) {
	t.Helper()
	if len(buf) < 4 {
		t.Fatalf("frame too short to hold a length header: %d bytes", len(buf))
	}
	n := binary.BigEndian.Uint32(buf[:4])
	if 4+int(n) > len(buf) {
		t.Fatalf("declared length %d exceeds remaining buffer %d", n, len(buf)-4)
	}
	return buf[4 : 4+n], buf[4+n:]
}

func TestEncodeFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty payload", payload: []byte{}},
		{name: "nil payload", payload: nil},
		{name: "single byte", payload: []byte{0x2a}},
		{name: "typical record", payload: []byte("hello, framepack")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			frame := encodeFrame(tc.payload)
			if len(frame) != 4+len(tc.payload) {
				t.Fatalf("len(frame) = %d, want %d", len(frame), 4+len(tc.payload))
			}
			got, rest := decodeFrame(t, frame)
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("decoded payload = %q, want %q", got, tc.payload)
			}
			if len(rest) != 0 {
				t.Fatalf("trailing bytes after payload: %d", len(rest))
			}
		})
	}
}

func TestNewEncoderRejectsNonPositiveMaxRecord(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1, -100} {
		if _, err := NewEncoder(&bytes.Buffer{}, size); !errors.Is(err, ErrInvalidMaxRecord) {
			t.Errorf("NewEncoder(%d) error = %v, want ErrInvalidMaxRecord", size, err)
		}
	}
}

// TestWriteFrameLimit checks both sides of the configured maximum: at the
// limit succeeds, one byte over is rejected before anything is written.
func TestWriteFrameLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		wantErr bool
	}{
		{name: "at exact limit", payload: []byte("four")},
		{name: "one byte over", payload: []byte("four+"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			enc, err := NewEncoder(&buf, 4)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			_, err = enc.WriteFrame(tc.payload)
			if tc.wantErr {
				if !errors.Is(err, ErrRecordTooLarge) {
					t.Fatalf("WriteFrame error = %v, want ErrRecordTooLarge", err)
				}
				if buf.Len() != 0 {
					t.Fatalf("writer received %d bytes for a rejected record, want 0", buf.Len())
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, _ := decodeFrame(t, buf.Bytes())
			if string(got) != string(tc.payload) {
				t.Fatalf("decoded payload = %q, want %q", got, tc.payload)
			}
		})
	}
}

// TestWriteFrameIndependentOfReusedSource proves each written frame is
// independent of the caller's buffer: mutating the source slice after
// WriteFrame returns must not change what was already written.
func TestWriteFrameIndependentOfReusedSource(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, 64)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}

	scratch := []byte("first!")
	if _, err := enc.WriteFrame(scratch); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	copy(scratch, "SECOND")
	if _, err := enc.WriteFrame(scratch); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	first, rest := decodeFrame(t, buf.Bytes())
	if string(first) != "first!" {
		t.Fatalf("first frame = %q, want %q (must not see the later mutation)", first, "first!")
	}
	second, rest := decodeFrame(t, rest)
	if string(second) != "SECOND" {
		t.Fatalf("second frame = %q, want %q", second, "SECOND")
	}
	if len(rest) != 0 {
		t.Fatalf("trailing bytes: %d", len(rest))
	}
}

// TestBuggyEncoderShipsEmptyFrames pins the exact defect a capacity-only
// scratch buffer produces, so a regression fails here, not on the wire.
func TestBuggyEncoderShipsEmptyFrames(t *testing.T) {
	t.Parallel()

	for _, payload := range [][]byte{[]byte("x"), []byte("hello, framepack"), []byte("")} {
		buggy := encodeFrameBuggy(payload)
		if len(buggy) != 0 {
			t.Fatalf("encodeFrameBuggy(%q) len = %d, want 0", payload, len(buggy))
		}

		good := encodeFrame(payload)
		if len(good) != 4+len(payload) {
			t.Fatalf("encodeFrame(%q) len = %d, want %d", payload, len(good), 4+len(payload))
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		stdin     string
		want      []string
		wantErr   bool
		wantUsage bool
	}{
		{
			name:  "two records",
			args:  []string{"-max-record", "64"},
			stdin: "ping\npong\n",
			want:  []string{"ping", "pong"},
		},
		{
			name:  "empty line is a zero-length record",
			args:  []string{"-max-record", "64"},
			stdin: "one\n\nthree\n",
			want:  []string{"one", "", "three"},
		},
		{
			name:      "record over max-record is a usage error",
			args:      []string{"-max-record", "2"},
			stdin:     "toolong\n",
			wantErr:   true,
			wantUsage: true,
		},
		{
			name:      "non-positive max-record is a usage error",
			args:      []string{"-max-record", "0"},
			stdin:     "x\n",
			wantErr:   true,
			wantUsage: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if tc.wantUsage && !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}

			rest := stdout.Bytes()
			for _, want := range tc.want {
				var got []byte
				got, rest = decodeFrame(t, rest)
				if string(got) != want {
					t.Fatalf("decoded record = %q, want %q", got, want)
				}
			}
			if len(rest) != 0 {
				t.Fatalf("trailing bytes after all expected records: %d", len(rest))
			}
		})
	}
}
```

## Review

`WriteFrame` is correct when the frame it emits has length `4+len(payload)`
every time, header and payload both present -- that is what
`TestEncodeFrame` and `TestWriteFrameLimit` pin, and what
`TestBuggyEncoderShipsEmptyFrames` shows failing catastrophically, not
subtly, once the scratch buffer is sized by capacity instead of length. The
rule underneath: `copy`'s destination bound is `len(dst)`, and `make([]T, 0,
n)` sets that length to zero no matter how large `n` is, so a `copy` into it
is a silent no-op, never a panic. `encodeFrame` avoids the whole question by
sizing the buffer with `make([]byte, 4+len(payload))` up front, so every
byte position it writes into already exists. Around that core, `NewEncoder`
rejects a non-positive `maxRecord` with `ErrInvalidMaxRecord`, `WriteFrame`
rejects an oversized payload with `ErrRecordTooLarge` before touching the
writer, and each frame is independent of whatever the caller does to its
source slice afterward. `run` maps every input mistake to exit code 2 and
reserves 1 for a genuine write or read failure. Run
`go test -count=1 -race ./...` to confirm all of it.

## Resources

- [`copy`](https://go.dev/ref/spec#Appending_and_copying_slices) — the spec paragraph defining the copy count as `min(len(dst), len(src))`.
- [`make`](https://go.dev/ref/spec#Making_slices_maps_and_channels) — why `make([]T, 0, n)` has zero addressable elements despite reserving `n` slots of capacity.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary#ByteOrder) — `BigEndian.PutUint32`, used for the frame's length header.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the streaming line reader `run` uses instead of loading all of stdin into memory.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-header-snapshot-deep-clone.md](09-header-snapshot-deep-clone.md) | Next: [11-trace-id-extraction-must-clone.md](11-trace-id-extraction-must-clone.md)
