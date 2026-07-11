# Exercise 15: Inspecting Length-Prefixed Frames: EOF Versus ErrUnexpectedEOF

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

gRPC-Web and Cap'n Proto RPC both put messages on the wire the same way: a
fixed-width length prefix, then that many bytes of payload, repeated until the
connection closes. A frame inspector -- the kind of tool you reach for to
debug a capture of that traffic, or to validate a log of frames written to
disk -- has to answer one question per frame it reads: did the stream end
cleanly right here, or did it die in the middle of delivering this frame? Those
are different failures with different remedies. A clean end means the producer
is done and everything downstream can finish normally. A mid-frame death means
the connection dropped, the disk filled up, or a process was killed mid-write,
and whatever partial frame is sitting in the buffer must be discarded, not
processed as if it were real data.

The distinction a length-prefixed reader must make is exactly the one
`io.ReadFull` was built to report: `io.EOF` when zero bytes were read at the
point a new frame was expected, and `io.ErrUnexpectedEOF` when some bytes were
read but not the full amount. A single `r.Read(buf)` call cannot make this
distinction reliably, because `Read` is permitted to return fewer bytes than
requested for reasons that have nothing to do with the frame ending -- a TCP
segment boundary, a pipe's buffer size, a TLS record boundary -- and code that
treats "I got some bytes back" as "I got the whole frame" will silently accept
a truncated read as valid data. That bug does not show up in a unit test built
on `bytes.Reader`, which happily hands back everything you ask for in one
call; it shows up in production, against a real socket, intermittently, which
is exactly why it survives review.

This exercise builds `frameinspect`, a command that reads a stream of these
frames and reports each one's index and size, stopping cleanly at a genuine
end of stream and failing loudly at a truncated one.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
frameinspect/             module example.com/frameinspect
  go.mod                  go 1.24
  frame.go                package main — NextFrame(io.Reader) ([]byte, error)
  frame_test.go           package main — frame table, truncation cases, naive-Read contrast, run() end to end
  main.go                 package main — no flags, stdin or a file argument, exit codes
```

- Files: `frame.go`, `frame_test.go`, `main.go`.
- Implement: `NextFrame(r io.Reader) ([]byte, error)` reading a 4-byte big-endian length prefix and exactly that many payload bytes with `io.ReadFull`, returning `io.EOF` only for a clean boundary and `io.ErrUnexpectedEOF` for any other short read, header or payload.
- Tool: `frameinspect` takes no flags. It reads frames from stdin, or from a single file named as its one argument, and prints `frame <i>: <n> bytes` per frame followed by a summary line. Exit 0 on a clean end of stream, exit 2 for a bad invocation (more than one argument, or a file that cannot be opened), exit 1 when the stream truncates mid-frame.
- Test: a multi-frame round trip ending in a clean `io.EOF`; a header truncated to one, two, or three bytes; a truncated payload; an empty stream; a zero-length payload frame; the naive single-`Read` contrast; `run` end to end over stdin, a file argument, and both usage failures.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/frameinspect
cd ~/go-exercises/frameinspect
go mod init example.com/frameinspect
go mod edit -go=1.24
```

### Why one `Read` call cannot tell a boundary from a truncation

`io.ReadFull(r, buf)` loops on `r.Read` until `buf` is full or reading fails,
and it turns that loop into exactly two distinguishable outcomes: `io.EOF` if
it read zero bytes total (a clean edge -- nothing was owed here), and
`io.ErrUnexpectedEOF` if it read between one and `len(buf)-1` bytes before the
source gave out (something was owed and only part of it arrived). Every other
error, `ReadFull` passes through unchanged. `NextFrame` relies on that
distinction twice: once while reading the 4-byte length prefix, and once while
reading the payload the prefix declared.

A single `Read` call collapses both truncation cases into "I got something
back," because `Read`'s contract only promises `0 <= n <= len(p)`. Code
written against that assumption looks like this:

```go
n, _ := r.Read(payload)   // n may be far less than len(payload)
return payload[:n], nil   // ...and this returns it as if it were complete
```

Against an in-memory `bytes.Reader` this line is invisible, because that
reader always fills the buffer in one call when enough data remains. Against
a socket, a pipe, or anything chunked, it silently truncates frames instead of
reporting the truncation -- the header may even decode to the wrong length
prefix if the short read cut across the 4-byte boundary itself. `NextFrame`
never makes this mistake because it never calls `Read` directly; every read of
a known length goes through `io.ReadFull`.

Create `frame.go`:

```go
// Command frameinspect reports the index and size of every length-prefixed
// frame in a byte stream, and distinguishes a clean end of stream from a
// truncated one.
package main

import (
	"encoding/binary"
	"io"
)

// headerSize is the width of the big-endian length prefix that precedes
// every frame's payload.
const headerSize = 4

// NextFrame reads one length-prefixed frame from r: a 4-byte big-endian
// length, followed by exactly that many payload bytes. It returns io.EOF
// only when the stream ends cleanly at a frame boundary -- zero bytes read
// where a new header was expected. Any other short read, whether inside the
// header or inside the payload, means a frame started but did not finish,
// and NextFrame reports that as io.ErrUnexpectedEOF regardless of which half
// of the frame was cut off.
//
// The returned slice is freshly allocated by make([]byte, n) and owned by
// the caller; it aliases nothing in r.
func NextFrame(r io.Reader) ([]byte, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if err == io.EOF {
			// Zero bytes read while starting a new frame: the stream ended
			// exactly between frames. This is the only clean outcome.
			return nil, io.EOF
		}
		// io.ReadFull returns io.ErrUnexpectedEOF itself when it read some
		// but not all of the header; propagate that same signal so a
		// 1-, 2-, or 3-byte trailing fragment of a length prefix is never
		// mistaken for a valid frame boundary.
		return nil, io.ErrUnexpectedEOF
	}

	n := binary.BigEndian.Uint32(header[:])
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		// The header was read in full, so the stream committed to
		// delivering n more bytes. Whether the payload read got zero bytes
		// (io.EOF) or some but not all (io.ErrUnexpectedEOF), the frame
		// this header promised never arrived: both cases are the same
		// truncation from NextFrame's point of view.
		return nil, io.ErrUnexpectedEOF
	}
	return payload, nil
}
```

### The tool

`frameinspect` needs no flags -- its only input is the stream itself, from
stdin or from one file argument -- so `run` takes the argument slice plus an
`io.Reader` for stdin and an `io.Writer` for stdout, nothing else, and never
touches `os.Args` or `os.Exit` directly. That keeps it driven end to end by a
`strings.Reader` and a `bytes.Buffer` in tests, exactly as `main` drives it
with the real stdin and stdout. Two arguments or an unopenable file are
mistakes the caller fixes by changing the invocation, so both wrap the
`errUsage` sentinel and map to exit code 2. A stream that truncates mid-frame
is not a usage mistake -- the invocation was correct, the *data* was bad -- so
it maps to exit code 1 instead. `run` reads and prints one frame at a time
rather than buffering the whole stream, since the entire point of a
length-prefixed format is that you never need to know the total size up
front.

Create `main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the invocation:
// too many arguments, or a file argument that cannot be opened. main maps it
// to exit code 2. A truncated frame stream is not a usage error -- it is a
// property of the input data -- and maps to exit code 1 instead.
var errUsage = errors.New("usage")

// run reads a length-prefixed frame stream from stdin, or from a file named
// by the single element of args, and writes one line per frame to stdout.
// It never touches os.Exit, so it can be driven end to end in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("%w: expected at most one file argument, got %d", errUsage, len(args))
	}

	r := stdin
	if len(args) == 1 {
		f, err := os.Open(args[0])
		if err != nil {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		defer f.Close()
		r = f
	}

	index := 0
	for {
		frame, err := NextFrame(r)
		if err == io.EOF {
			fmt.Fprintf(stdout, "%d frames, stream ended cleanly\n", index)
			return nil
		}
		if err != nil {
			return fmt.Errorf("frame %d: %w", index, err)
		}
		fmt.Fprintf(stdout, "frame %d: %d bytes\n", index, len(frame))
		index++
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "frameinspect:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '\x00\x00\x00\x05hello\x00\x00\x00\x00\x00\x00\x00\x05world' | go run .
printf '\x00\x00\x00\x05hel' | go run .
printf '' | go run . a b
```

Expected output:

```text
frame 0: 5 bytes
frame 1: 0 bytes
frame 2: 5 bytes
3 frames, stream ended cleanly
frameinspect: frame 0: unexpected EOF
frameinspect: usage: expected at most one file argument, got 2
```

The first command's stream is three frames -- `"hello"`, an empty frame, then
`"world"` -- and every one is reported before the clean-EOF summary line, exit
0. The second command's stream declares a 5-byte payload and delivers only
three bytes of it; `NextFrame` reports `io.ErrUnexpectedEOF` at frame 0, `run`
wraps it with the frame index, and the process exits 1. The third command
passes two positional arguments, which `run` rejects before it ever touches
stdin, wrapping `errUsage` for exit 2.

### Using it

`frameinspect` has no importable API -- it is a tool you run, not a package
you import -- so there is nothing to configure beyond the stream itself. The
one exported piece of logic worth knowing if you build a similar frame
consumer of your own is the shape of `NextFrame`: read the length with
`io.ReadFull`, size the payload buffer with `make([]byte, n)`, read the
payload with `io.ReadFull`, and let `io.EOF` mean "nothing more was expected
here" while every other short read becomes `io.ErrUnexpectedEOF`. That is the
whole contract, and it is what lets `run` stop a loop correctly instead of
guessing from a byte count.

### Tests

`TestNextFrame` is the table: a normal multi-frame stream read to a clean
`io.EOF`, a header truncated to one, two, or three bytes, a truncated
payload, an empty stream, and a zero-length payload frame -- the last one
matters because a length of zero is a valid frame, not an error, and the code
must not confuse "declared zero bytes" with "declared nothing." The next test
is the module's reason for existing: `nextFrameNaive` is the one-`Read`
version of frame reading, unexported and unreachable from the tool, kept
alive only so `TestNaiveReadSilentlyAcceptsTruncatedPayload` can feed the same
truncated stream to both functions and show `NextFrame` reporting
`io.ErrUnexpectedEOF` while the naive version returns a three-byte frame with
a nil error -- silent data loss instead of a reported failure.
`TestRun` drives the command end to end: a clean stream on stdin, the same
stream truncated, an empty stream, a file argument, too many arguments, and a
missing file, checking both the printed output and which sentinel each
failure wraps.

Create `frame_test.go`:

```go
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// encode concatenates payloads into a length-prefixed byte stream, the exact
// wire format NextFrame reads.
func encode(payloads ...[]byte) []byte {
	var buf bytes.Buffer
	for _, p := range payloads {
		var header [headerSize]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(p)))
		buf.Write(header[:])
		buf.Write(p)
	}
	return buf.Bytes()
}

func TestNextFrame(t *testing.T) {
	t.Parallel()

	t.Run("reads frames in order and then a clean EOF", func(t *testing.T) {
		t.Parallel()

		r := bytes.NewReader(encode([]byte("hello"), []byte{}, []byte("world")))
		want := [][]byte{[]byte("hello"), {}, []byte("world")}
		for i, w := range want {
			got, err := NextFrame(r)
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			if !bytes.Equal(got, w) {
				t.Fatalf("frame %d = %q, want %q", i, got, w)
			}
		}
		if _, err := NextFrame(r); !errors.Is(err, io.EOF) {
			t.Fatalf("final read err = %v, want io.EOF", err)
		}
	})

	t.Run("truncated header of one two or three bytes is unexpected EOF", func(t *testing.T) {
		t.Parallel()

		full := encode([]byte("x"))
		for n := 1; n < headerSize; n++ {
			r := bytes.NewReader(full[:n])
			if _, err := NextFrame(r); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("header truncated to %d bytes: err = %v, want io.ErrUnexpectedEOF", n, err)
			}
		}
	})

	t.Run("truncated payload is unexpected EOF", func(t *testing.T) {
		t.Parallel()

		// Header declares 5 bytes, but only 3 follow.
		stream := append(encode([]byte("hello"))[:headerSize], []byte("hel")...)
		if _, err := NextFrame(bytes.NewReader(stream)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("empty stream is a clean EOF, not an error frame", func(t *testing.T) {
		t.Parallel()

		if _, err := NextFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	})

	t.Run("zero-length payload frame", func(t *testing.T) {
		t.Parallel()

		got, err := NextFrame(bytes.NewReader(encode(nil)))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(got) != 0 {
			t.Fatalf("len(got) = %d, want 0", len(got))
		}
	})
}

// nextFrameNaive is the version of frame reading that a first draft of this
// tool tends to ship: one Read call for the header, one Read call for the
// payload, and any non-zero count is treated as a complete read. It is
// unexported, unreachable from the tool, and exists only so the tests can
// pin what it silently gets wrong: rather than reporting the truncation, it
// returns a short frame dressed up as a successful one.
func nextFrameNaive(r io.Reader) ([]byte, error) {
	header := make([]byte, headerSize)
	n, err := r.Read(header)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, io.EOF
	}
	length := binary.BigEndian.Uint32(header)
	payload := make([]byte, length)
	pn, err := r.Read(payload)
	if err != nil && pn == 0 {
		return nil, err
	}
	// BUG: pn may be less than length -- the stream may have closed early --
	// but any non-zero read is accepted as if it were the whole frame.
	return payload[:pn], nil
}

// TestNaiveReadSilentlyAcceptsTruncatedPayload is the heart of this module.
// Fed the same truncated stream, NextFrame reports the truncation and
// nextFrameNaive does not: it returns a short frame with a nil error, which
// is exactly the class of bug a single r.Read call ships into production.
func TestNaiveReadSilentlyAcceptsTruncatedPayload(t *testing.T) {
	t.Parallel()

	// Header declares 5 bytes ("hello"), but only 3 follow.
	stream := append(encode([]byte("hello"))[:headerSize], []byte("hel")...)

	if _, err := NextFrame(bytes.NewReader(stream)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("NextFrame err = %v, want io.ErrUnexpectedEOF", err)
	}

	got, err := nextFrameNaive(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("nextFrameNaive returned an error %v; the bug is that it should have returned nil with a short frame", err)
	}
	if len(got) != 3 {
		t.Fatalf("nextFrameNaive len(got) = %d, want 3 (the truncated payload, silently accepted as complete)", len(got))
	}
	if string(got) != "hel" {
		t.Fatalf("nextFrameNaive got = %q, want %q", got, "hel")
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("clean stream from stdin", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		stdin := bytes.NewReader(encode([]byte("hello"), []byte{}, []byte("world")))
		if err := run(nil, stdin, &stdout); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "frame 0: 5 bytes\nframe 1: 0 bytes\nframe 2: 5 bytes\n3 frames, stream ended cleanly\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("truncated stream is a runtime failure not a usage error", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		stream := append(encode([]byte("hello"))[:headerSize], []byte("hel")...)
		err := run(nil, bytes.NewReader(stream), &stdout)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("run err = %v, want io.ErrUnexpectedEOF", err)
		}
		if errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, must not wrap errUsage", err)
		}
	})

	t.Run("empty stream", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		if err := run(nil, bytes.NewReader(nil), &stdout); err != nil {
			t.Fatalf("run: %v", err)
		}
		if stdout.String() != "0 frames, stream ended cleanly\n" {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})

	t.Run("too many arguments is a usage error", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		err := run([]string{"a", "b"}, bytes.NewReader(nil), &stdout)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want it to wrap errUsage", err)
		}
	})

	t.Run("missing file is a usage error", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		err := run([]string{"/no/such/file-frameinspect-test"}, bytes.NewReader(nil), &stdout)
		if !errors.Is(err, errUsage) {
			t.Fatalf("run err = %v, want it to wrap errUsage", err)
		}
	})

	t.Run("reads from a file argument instead of stdin", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "frames.bin")
		if err := os.WriteFile(path, encode([]byte("abc")), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		var stdout bytes.Buffer
		if err := run([]string{path}, strings.NewReader(""), &stdout); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "frame 0: 3 bytes\n1 frames, stream ended cleanly\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})
}
```

## Review

`NextFrame` is correct when every short read is classified the same way
`io.ReadFull` classifies it: zero bytes at a frame boundary is `io.EOF`, and
anything else short is `io.ErrUnexpectedEOF`, whether the cut lands inside the
4-byte length prefix or inside the payload it declared. `TestNextFrame` pins
the boundary and every truncation shape; `TestNaiveReadSilentlyAcceptsTruncatedPayload`
is the module's core lesson, showing that a single `Read` call does not just
report truncation less precisely -- it fails to report it at all, returning a
short frame as if it were complete. `run` keeps that distinction visible at
the process boundary: a clean end of stream exits 0, a bad invocation wraps
`errUsage` and exits 2, and a truncated stream exits 1 without ever being
confused for a usage mistake. Run `go test -count=1 -race ./...` to confirm
the frame table, the naive-Read contrast, and `run`'s behavior across stdin, a
file argument, and both failure paths.

## Resources

- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the function that turns a short read into `io.EOF` or `io.ErrUnexpectedEOF` depending on how many bytes arrived.
- [`io.ErrUnexpectedEOF`](https://pkg.go.dev/io#pkg-variables) — documented alongside `io.EOF` as the signal for a read that ended before it should have.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary#ByteOrder) — `BigEndian.Uint32`, used to decode the length prefix.
- [gRPC-Web protocol spec](https://github.com/grpc/grpc-web/blob/master/doc/wire-format-mode-browser.md) — a real length-prefixed framing format shaped exactly like this exercise's.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-rate-limiter-registry-pointer-map-clone.md](14-rate-limiter-registry-pointer-map-clone.md) | Next: [16-fanout-log-shipper-clone-per-sink.md](16-fanout-log-shipper-clone-per-sink.md)
