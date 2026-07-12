# Exercise 20: Decoding Variable-Length Frames with a Frame-Count Cap

**Nivel: Intermedio** — validacion rapida (un test corto).

A length-prefixed wire protocol delivers frames split arbitrarily across
network reads: a single chunk might hold three complete frames, or half of
one. Decoding it needs two loops working together — one over the incoming
chunks, one over however many complete frames the accumulated buffer
currently holds — and a hard cap on total frames so a chatty or malicious
sender cannot force the decoder to buffer forever. This module builds that
decoder and the labeled `break` that makes the cap actually stop both loops.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
framedecoder/                  module example.com/framedecoder
  go.mod                       go 1.24
  framedecoder.go               Frame; Decode(chunks, maxFrames) ([]Frame, error); ErrIncompleteStream
  framedecoder_test.go            multi-frame chunk, split frame, cap stops both loops, incomplete tail, zero-length frame
  cmd/demo/
    main.go                     three split frames decoded, a fourth frame skipped by the cap
```

- Files: `framedecoder.go`, `framedecoder_test.go`, `cmd/demo/main.go`.
- Implement: `Decode(chunks [][]byte, maxFrames int) ([]Frame, error)` — an outer labeled `for _, chunk := range chunks` over the stream, an inner `for len(buf) > 0` that drains complete frames from the accumulated buffer, and a `break stream` the instant `maxFrames` is reached.
- Test: multiple frames in one chunk; a frame split across a chunk boundary; the cap stopping mid-stream and never touching a later chunk; an incomplete final frame reported as an error; a zero-length frame.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the cap needs a labeled break, not a plain one

`Decode` is two nested loops for a reason that has nothing to do with style:
the outer loop's unit of work is "a chunk arrived," the inner loop's unit of
work is "a complete frame is available in the buffer," and those are
different units that advance at different rates — one chunk can yield zero,
one, or several frames. The inner loop's own exit (`break` on `len(buf) <
1+n`, meaning the current frame is still incomplete) is a *plain* `break`
because it only ever means "wait for the next chunk," which is exactly what
letting the outer loop continue accomplishes. The cap is different: once
`maxFrames` is reached there must be *no* further chunks read at all, not
even to check whether they contain more frames. A plain `break` inside the
inner loop at that point would only stop parsing the current chunk — the
outer `range` would immediately move on to the next chunk and start
accumulating more bytes into `buf`, which is precisely the unbounded
buffering the cap exists to prevent. The labeled `break stream` is what makes
the cap airtight: it exits both loops in one statement, the same instant the
limit is hit.

Create `framedecoder.go`:

```go
package framedecoder

import "errors"

// ErrIncompleteStream means the input ended in the middle of a frame.
var ErrIncompleteStream = errors.New("framedecoder: incomplete frame at end of stream")

// Frame is one decoded, length-prefixed frame.
type Frame struct {
	ID      int
	Payload []byte
}

// Decode parses length-prefixed frames (a 1-byte length followed by that many
// payload bytes) out of a stream delivered as successive chunks -- exactly
// how a network read arrives in practice, where a frame can be split across
// chunk boundaries. It stops after maxFrames frames even if more chunks
// remain, so a misbehaving or malicious sender cannot force unbounded
// buffering.
//
// The outer loop ranges over the stream's chunks; the inner loop drains as
// many complete frames as the accumulated buffer currently holds. The
// labeled break on the cap is what makes this safe: a plain break there would
// only exit the inner per-chunk loop, and the outer loop would go on
// accumulating and parsing further chunks it was never supposed to touch
// once the cap was reached.
func Decode(chunks [][]byte, maxFrames int) ([]Frame, error) {
	var frames []Frame
	var buf []byte

stream:
	for _, chunk := range chunks {
		buf = append(buf, chunk...)
		for len(buf) > 0 {
			n := int(buf[0])
			if len(buf) < 1+n {
				break // this frame is split across a chunk boundary; wait for more
			}
			payload := append([]byte(nil), buf[1:1+n]...)
			frames = append(frames, Frame{ID: len(frames), Payload: payload})
			buf = buf[1+n:]
			if len(frames) >= maxFrames {
				break stream
			}
		}
	}

	if len(buf) > 0 && len(frames) < maxFrames {
		return frames, ErrIncompleteStream
	}
	return frames, nil
}
```

### The runnable demo

The demo splits three frames ("alpha", "beta", "gamma") across three network
reads at awkward boundaries, and adds a fourth chunk holding a fourth frame
("skip-me") that the cap of 3 ensures is never even looked at.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/framedecoder"
)

func frame(payload string) []byte {
	return append([]byte{byte(len(payload))}, []byte(payload)...)
}

func main() {
	// Three frames arrive split awkwardly across three network reads, and a
	// fourth frame ("skip-me") shows up in a later chunk that is never
	// reached because maxFrames caps the decode at 3.
	full := append(append(frame("alpha"), frame("beta")...), frame("gamma")...)
	chunks := [][]byte{
		full[:4],
		full[4:12],
		full[12:],
		frame("skip-me"),
	}

	frames, err := framedecoder.Decode(chunks, 3)
	if err != nil {
		fmt.Println("decode error:", err)
		return
	}
	for _, f := range frames {
		fmt.Printf("frame %d: %q\n", f.ID, f.Payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
frame 0: "alpha"
frame 1: "beta"
frame 2: "gamma"
```

### Tests

`TestDecodeFrameSplitAcrossChunkBoundary` is the base correctness case for
the two-loop shape; `TestDecodeStopsAtMaxFramesAndSkipsRemainingChunks` is the
one that actually proves the labeled break works, by putting a frame in a
chunk after the cap and asserting it never shows up in the result.
`TestDecodeIncompleteFrameAtEndOfStream` and `TestDecodeZeroLengthFrame`
round out the boundary conditions of the length-prefix format itself.

Create `framedecoder_test.go`:

```go
package framedecoder

import (
	"bytes"
	"errors"
	"testing"
)

// frame builds a length-prefixed encoding of payload.
func frame(payload string) []byte {
	return append([]byte{byte(len(payload))}, []byte(payload)...)
}

func TestDecodeSingleChunkMultipleFrames(t *testing.T) {
	t.Parallel()

	chunks := [][]byte{
		append(append(frame("ab"), frame("cde")...), frame("f")...),
	}

	got, err := Decode(chunks, 10)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []string{"ab", "cde", "f"}
	if len(got) != len(want) {
		t.Fatalf("len(frames) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if !bytes.Equal(got[i].Payload, []byte(w)) {
			t.Errorf("frame %d = %q, want %q", i, got[i].Payload, w)
		}
	}
}

func TestDecodeFrameSplitAcrossChunkBoundary(t *testing.T) {
	t.Parallel()

	full := frame("hello")
	chunks := [][]byte{
		full[:2], // length byte + first char
		full[2:], // rest of the payload
	}

	got, err := Decode(chunks, 10)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Payload, []byte("hello")) {
		t.Fatalf("got %+v, want one frame %q", got, "hello")
	}
}

func TestDecodeStopsAtMaxFramesAndSkipsRemainingChunks(t *testing.T) {
	t.Parallel()

	// The cap is reached inside the first chunk; a second chunk holding
	// frame "c" must never be consumed once the cap trips.
	chunks := [][]byte{
		append(frame("a"), frame("b")...),
		frame("c"),
	}

	got, err := Decode(chunks, 2)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(frames) = %d, want 2 (capped)", len(got))
	}
	if !bytes.Equal(got[0].Payload, []byte("a")) || !bytes.Equal(got[1].Payload, []byte("b")) {
		t.Fatalf("got %+v, want frames a, b -- chunk 2 (\"c\") must never be reached", got)
	}
}

func TestDecodeIncompleteFrameAtEndOfStream(t *testing.T) {
	t.Parallel()

	full := frame("hello")
	chunks := [][]byte{full[:3]} // length byte says 5, only 2 payload bytes arrive

	got, err := Decode(chunks, 10)
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(frames) = %d, want 0", len(got))
	}
}

func TestDecodeEmptyStream(t *testing.T) {
	t.Parallel()

	got, err := Decode(nil, 10)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(frames) = %d, want 0", len(got))
	}
}

func TestDecodeZeroLengthFrame(t *testing.T) {
	t.Parallel()

	chunks := [][]byte{append(frame(""), frame("x")...)}

	got, err := Decode(chunks, 10)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(frames) = %d, want 2", len(got))
	}
	if len(got[0].Payload) != 0 {
		t.Errorf("frame 0 payload = %q, want empty", got[0].Payload)
	}
	if !bytes.Equal(got[1].Payload, []byte("x")) {
		t.Errorf("frame 1 payload = %q, want %q", got[1].Payload, "x")
	}
}
```

## Review

`Decode` is correct when a frame split across any chunk boundary decodes
identically to the same frame arriving whole, and when reaching `maxFrames`
stops the decoder from reading a single further byte from any later chunk.
The common mistake this design avoids is using a plain `break` at the cap
check — it compiles, the tests for the simple cases still pass, and it is
only `TestDecodeStopsAtMaxFramesAndSkipsRemainingChunks` that catches the bug,
because that is the only test with meaningful content in a chunk after the
cap. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — the labeled form used to exit both loops at once.
- [gRPC over HTTP/2: Length-Prefixed-Message](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md#length-prefixed-message) — a real length-prefixed framing format shaped like this exercise's.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-checkpoint-recovery-retries.md](19-checkpoint-recovery-retries.md) | Next: [21-metrics-time-bucket-roundrobin.md](21-metrics-time-bucket-roundrobin.md)
