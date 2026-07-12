# Exercise 3: A Length-Prefixed Frame Decoder That Handles EOF-With-Data

Length-prefixed framing — a fixed-width length header followed by that many
payload bytes — is the backbone of most binary wire protocols (gRPC's message
framing, Redis bulk strings, custom TCP protocols). The subtle correctness
requirement is that the decoder must process the bytes a `Read` returns *before*
checking the error, because a well-behaved source may deliver the final frame's
bytes together with `io.EOF`. This exercise builds the decoder and proves it with
`iotest.DataErrReader`, which forces exactly that timing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
frameproto/                 independent module: example.com/frameproto
  go.mod                    module example.com/frameproto
  decode.go                 DecodeFrames: uint32 big-endian length + payload, via io.ReadFull
  cmd/
    demo/
      main.go               encodes three frames, decodes them back
  decode_test.go            identical frames plain vs DataErrReader; truncation -> ErrUnexpectedEOF; clean boundary -> EOF
```

Files: `decode.go`, `cmd/demo/main.go`, `decode_test.go`.
Implement: `DecodeFrames(r io.Reader) ([][]byte, error)` reading `uint32` big-endian length then that many payload bytes with `io.ReadFull`, looping until a clean `io.EOF` on a frame boundary.
Test: identical decoded frames from a plain reader and from `iotest.DataErrReader`; a truncated final frame yields `io.ErrUnexpectedEOF`; a clean end yields no error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/10-testing-readers-with-iotest/03-dataerr-eof-with-final-data/cmd/demo
cd go-solutions/12-testing-ecosystem/10-testing-readers-with-iotest/03-dataerr-eof-with-final-data
```

### The EOF-with-data trap, and why io.ReadFull defuses it

Consider the naive read loop that reads the length header, then does
`n, err := r.Read(payload); if err != nil { return }`. When the source returns the
last frame's payload bytes together with `io.EOF` (the second legal EOF form), the
loop sees a non-nil `err` and returns *before recording the payload it already
received* — the final frame is silently dropped. This is one of the most common
and hardest-to-reproduce streaming bugs, because most stdlib readers use the other
EOF form and the bug never fires in a naive test.

`io.ReadFull` is the correct primitive and it handles this for you: internally it
accumulates bytes across reads, and if it collects exactly `len(buf)` bytes it
returns `err == nil` *even when the underlying read delivered them alongside EOF*.
So `DecodeFrames` reads the length with `io.ReadFull`, then the payload with
`io.ReadFull`, and never has to reason about which EOF form the source uses.

The error mapping is where framing correctness lives:

- Header read returns `io.EOF` with zero bytes: a clean end on a frame boundary.
  Stop and return the frames collected so far, no error.
- Header read returns `io.ErrUnexpectedEOF`: a partial length header — the stream
  was cut mid-header. Corruption.
- Payload read returns `io.EOF` (zero bytes read) or `io.ErrUnexpectedEOF`: the
  header promised N bytes the stream cannot supply. We normalize the `io.EOF`
  case to `io.ErrUnexpectedEOF`, because a truncated payload is corruption, not a
  clean boundary.

`iotest.DataErrReader(r)` rewrites `r` so its final data read carries `io.EOF`
instead of deferring it to a follow-up call. Decoding the identical byte stream
through a plain reader and through `DataErrReader` must produce identical frames;
if it does not, the decoder is checking `err` before consuming `n`.

Create `decode.go`:

```go
package frameproto

import (
	"encoding/binary"
	"io"
)

// DecodeFrames reads length-prefixed frames from r: a uint32 big-endian length
// followed by that many payload bytes, repeated until end of stream. A clean EOF
// on a frame boundary ends decoding with no error; a truncated header or payload
// returns io.ErrUnexpectedEOF.
func DecodeFrames(r io.Reader) ([][]byte, error) {
	var frames [][]byte
	var header [4]byte
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return frames, nil // clean end on a boundary
			}
			return frames, err // io.ErrUnexpectedEOF: truncated header
		}
		length := binary.BigEndian.Uint32(header[:])
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF // header promised bytes that never came
			}
			return frames, err
		}
		frames = append(frames, payload)
	}
}

// EncodeFrame writes a single length-prefixed frame into dst, returning the
// bytes. It is the inverse of one DecodeFrames iteration, used by the demo/tests.
func EncodeFrame(dst []byte, payload []byte) []byte {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	dst = append(dst, header[:]...)
	dst = append(dst, payload...)
	return dst
}
```

### The runnable demo

The demo encodes three frames into one buffer, then decodes them back, showing
the round trip.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/frameproto"
)

func main() {
	var buf []byte
	for _, msg := range []string{"login", "ping", "logout"} {
		buf = frameproto.EncodeFrame(buf, []byte(msg))
	}

	frames, err := frameproto.DecodeFrames(bytes.NewReader(buf))
	if err != nil {
		log.Fatal(err)
	}
	for i, f := range frames {
		fmt.Printf("frame %d: %s\n", i, f)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frame 0: login
frame 1: ping
frame 2: logout
```

### Tests

`TestPlainAndDataErrAgree` is the anchor: it decodes the same encoded stream
through a plain `bytes.Reader` and through `iotest.DataErrReader`, and asserts the
frame slices are identical. `TestTruncatedPayload` cuts the last frame short and
asserts `io.ErrUnexpectedEOF`. `TestCleanBoundary` ends exactly on a frame
boundary and asserts no error with all frames intact. `TestTruncatedHeader` cuts
mid-header and also expects `io.ErrUnexpectedEOF`.

Create `decode_test.go`:

```go
package frameproto

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"testing/iotest"
)

func encodeAll(msgs ...string) []byte {
	var buf []byte
	for _, m := range msgs {
		buf = EncodeFrame(buf, []byte(m))
	}
	return buf
}

func TestPlainAndDataErrAgree(t *testing.T) {
	t.Parallel()
	stream := encodeAll("alpha", "beta", "gamma")
	want := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}

	plain, err := DecodeFrames(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("plain decode: %v", err)
	}
	if !reflect.DeepEqual(plain, want) {
		t.Fatalf("plain frames = %v, want %v", plain, want)
	}

	dataErr, err := DecodeFrames(iotest.DataErrReader(bytes.NewReader(stream)))
	if err != nil {
		t.Fatalf("DataErrReader decode: %v", err)
	}
	if !reflect.DeepEqual(dataErr, plain) {
		t.Fatalf("DataErrReader frames = %v, want %v", dataErr, plain)
	}
}

func TestTruncatedPayload(t *testing.T) {
	t.Parallel()
	stream := encodeAll("complete", "cut")
	stream = stream[:len(stream)-2] // drop 2 bytes of the last payload

	_, err := DecodeFrames(bytes.NewReader(stream))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestTruncatedHeader(t *testing.T) {
	t.Parallel()
	stream := encodeAll("frame")
	stream = append(stream, 0x00, 0x00) // 2 stray header bytes, no length/payload

	_, err := DecodeFrames(bytes.NewReader(stream))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestCleanBoundary(t *testing.T) {
	t.Parallel()
	stream := encodeAll("one", "two")
	frames, err := DecodeFrames(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
}

func TestEmptyStream(t *testing.T) {
	t.Parallel()
	frames, err := DecodeFrames(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("got %d frames, want 0", len(frames))
	}
}
```

## Review

The decoder is correct when every fixed-size read goes through `io.ReadFull` and
the two EOF outcomes are classified precisely: `io.EOF` with zero bytes on a
header read is a clean boundary; anything partial is `io.ErrUnexpectedEOF`. The
`DataErrReader` test is the one that catches the real bug — if the decoder checked
`err` before consuming payload bytes, decoding through `DataErrReader` would drop
the final frame and diverge from the plain decode. `io.ReadFull` prevents that by
construction, which is exactly why you reach for it instead of a bare `Read` for
framed protocols. Run `go test -race` to confirm all cases pass.

## Resources

- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the fixed-size read primitive and its EOF semantics.
- [`testing/iotest#DataErrReader`](https://pkg.go.dev/testing/iotest#DataErrReader) — moves EOF onto the final data read.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.Uint32` / `PutUint32` for the length header.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-adversarial-chunking-onebyte-halfreader.md](02-adversarial-chunking-onebyte-halfreader.md) | Next: [04-checksum-tee-reader-fault-injection.md](04-checksum-tee-reader-fault-injection.md)
