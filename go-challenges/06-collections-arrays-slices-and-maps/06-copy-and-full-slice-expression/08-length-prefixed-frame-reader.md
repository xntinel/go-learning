# Exercise 8: Read Length-Prefixed Frames With `io.ReadFull`

A wire-protocol frame reader reads a 4-byte big-endian length, allocates exactly
that many bytes with `make`, and fills it with `io.ReadFull`, returning each frame
as an owned slice. This is the case where `make` + `io.ReadFull` is the right tool:
the length is known up front, so there is no over-allocation, no aliasing with any
other buffer, and short reads are reported precisely. This exercise builds the
reader and pins the `io.EOF` vs `io.ErrUnexpectedEOF` boundary.

Self-contained module: own `go mod init`, own demo, own tests.

## What you'll build

```text
frames/                    independent module: example.com/frames
  go.mod                   go 1.26
  frames.go                ReadFrame, ReadAll, WriteFrame (4-byte BE length prefix)
  cmd/
    demo/
      main.go              write three frames to a buffer, read them all back
  frames_test.go           round-trip, truncated-body ErrUnexpectedEOF, clean EOF, ownership
```

Files: `frames.go`, `cmd/demo/main.go`, `frames_test.go`.
Implement: `ReadFrame` (read length, `make`, `io.ReadFull`), `ReadAll`, `WriteFrame`.
Test: concatenated frames round-trip with correct bytes and lengths; a truncated body yields `io.ErrUnexpectedEOF`; a clean boundary yields `io.EOF`; each returned frame is independent of the source stream.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/08-length-prefixed-frame-reader/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/08-length-prefixed-frame-reader
```

### Why `make` + `io.ReadFull`, not `append` growth

When the length is known before you read the body — here from a 4-byte prefix —
the precise tool is `make([]byte, n)` followed by `io.ReadFull`. `make([]byte, n)`
allocates exactly `n` bytes: no over-allocation from `append`'s growth doubling,
and no capacity spilling past `n` that a later `append` could reach. The frame is
sized by length, so `io.ReadFull` fills it completely and the returned slice has
`cap == len == n` — a self-contained, owned frame the caller can keep or mutate
freely. Using `append` growth here would be both wasteful (rounded-up capacity)
and looser (the frame would carry spare capacity aliasing nothing, but signalling
"you may append", which is the wrong contract for a decoded frame).

`io.ReadFull` is what gives correct short-read semantics, which a single `r.Read`
does not. `Read` may return fewer bytes than requested for any reason; treating its
first return as a complete frame silently accepts truncated input. `io.ReadFull`
loops until the buffer is full and distinguishes the two ways a read can end: it
returns `io.EOF` only if it read *zero* bytes (a clean frame boundary — the stream
ended exactly between frames), and `io.ErrUnexpectedEOF` if it read some but not
all (a truncated frame). `ReadFrame` propagates the clean `io.EOF` from the length
prefix so `ReadAll` can stop, and maps a mid-body `io.EOF` to `io.ErrUnexpectedEOF`
because a body that started but did not finish is a protocol error, not a clean
end.

Create `frames.go`:

```go
package frames

import (
	"encoding/binary"
	"errors"
	"io"
)

// ReadFrame reads one length-prefixed frame: a 4-byte big-endian length, then
// exactly that many bytes. It returns io.EOF at a clean frame boundary and
// io.ErrUnexpectedEOF if the stream ends mid-frame. The returned slice is owned
// by the caller (fresh backing array, cap == len).
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		// io.EOF here means a clean boundary; a partial header is unexpected.
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// ReadAll reads frames until a clean EOF, returning them in order.
func ReadAll(r io.Reader) ([][]byte, error) {
	var out [][]byte
	for {
		f, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

// WriteFrame writes payload as a length-prefixed frame.
func WriteFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
```

### The runnable demo

The demo writes three frames — including a zero-length one — into a buffer, then
reads them all back and prints each with its length.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/frames"
)

func main() {
	var buf bytes.Buffer
	for _, p := range [][]byte{[]byte("hello"), {}, []byte("world")} {
		if err := frames.WriteFrame(&buf, p); err != nil {
			panic(err)
		}
	}

	all, err := frames.ReadAll(&buf)
	if err != nil {
		panic(err)
	}
	for i, f := range all {
		fmt.Printf("frame %d: len=%d %q\n", i, len(f), f)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frame 0: len=5 "hello"
frame 1: len=0 ""
frame 2: len=5 "world"
```

### Tests

`TestRoundTrip` writes several frames and reads them back, asserting bytes and
lengths. `TestTruncatedBodyIsUnexpectedEOF` feeds a header claiming more bytes than
the stream delivers and asserts `io.ErrUnexpectedEOF`. `TestCleanEOFAtBoundary`
asserts `io.EOF` after the last full frame. `TestFrameIsIndependent` mutates a
returned frame and asserts the source bytes are untouched.

Create `frames_test.go`:

```go
package frames

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func encode(payloads ...[]byte) []byte {
	var buf bytes.Buffer
	for _, p := range payloads {
		if err := WriteFrame(&buf, p); err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	want := [][]byte{[]byte("hello"), {}, []byte("world"), []byte("42")}
	got, err := ReadAll(bytes.NewReader(encode(want...)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d = %q, want %q", i, got[i], want[i])
		}
		if len(got[i]) != cap(got[i]) {
			t.Fatalf("frame %d cap=%d len=%d, want cap==len", i, cap(got[i]), len(got[i]))
		}
	}
}

func TestTruncatedBodyIsUnexpectedEOF(t *testing.T) {
	t.Parallel()

	// Header claims 5 bytes, but only 2 follow.
	stream := append(encode([]byte("hello"))[:4], []byte("he")...)
	_, err := ReadFrame(bytes.NewReader(stream))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestCleanEOFAtBoundary(t *testing.T) {
	t.Parallel()

	r := bytes.NewReader(encode([]byte("only")))
	if _, err := ReadFrame(r); err != nil {
		t.Fatalf("first frame err = %v, want nil", err)
	}
	if _, err := ReadFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("second read err = %v, want io.EOF", err)
	}
}

func TestFrameIsIndependent(t *testing.T) {
	t.Parallel()

	src := encode([]byte("abc"))
	srcCopy := bytes.Clone(src)

	f, err := ReadFrame(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	f[0] = 'Z' // mutate the owned frame

	if !bytes.Equal(src, srcCopy) {
		t.Fatalf("source stream mutated via returned frame: %q", src)
	}
}
```

## Review

The reader is correct when frames round-trip exactly and the two end-of-stream
cases are distinguished: `TestRoundTrip` pins the bytes, lengths, and `cap == len`
ownership; `TestCleanEOFAtBoundary` pins the clean `io.EOF`; and
`TestTruncatedBodyIsUnexpectedEOF` pins the truncated-frame error, all asserted
with `errors.Is` so wrapping stays safe. `TestFrameIsIndependent` confirms each
frame is on its own backing array, so a caller mutating it cannot reach the source.
The design lesson: when you know the length, `make([]byte, n)` + `io.ReadFull` is
the exact, aliasing-free tool; reach for `append` growth only when the size is
genuinely unknown up front.

## Resources

- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull)
- [`encoding/binary` (`BigEndian`)](https://pkg.go.dev/encoding/binary#ByteOrder)
- [`io.ErrUnexpectedEOF`](https://pkg.go.dev/io#pkg-variables)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-metrics-batcher-flush-and-reuse.md](07-metrics-batcher-flush-and-reuse.md) | Next: [09-header-snapshot-deep-clone.md](09-header-snapshot-deep-clone.md)
