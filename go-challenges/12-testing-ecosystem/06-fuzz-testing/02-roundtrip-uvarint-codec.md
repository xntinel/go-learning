# Exercise 2: Round-Trip Fuzz A Length-Prefixed Wire Codec

A message-queue writer that streams frames over a socket needs a framing layer:
each payload is written with a length prefix so the reader knows where it ends.
This module builds that codec with a varint length prefix, then fuzzes the one
property every codec must satisfy — `Decode(Encode(x)) == x` for all `x` — and
pins the malformed-frame errors with a table test.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
wire/                      independent module: example.com/wire
  go.mod                   module path
  frame.go                 EncodeFrame([]byte) []byte; DecodeFrame([]byte) (payload, rest []byte, err error); ErrTruncated
  cmd/
    demo/
      main.go              encode two frames back-to-back, decode them in sequence
  frame_test.go            TestDecodeMalformed, FuzzFrameRoundTrip, Example
```

Files: `frame.go`, `cmd/demo/main.go`, `frame_test.go`.
Implement: `EncodeFrame` (uvarint length + payload) and `DecodeFrame` returning
the payload, the unconsumed rest, and `ErrTruncated` (wrapped) for short input.
Test: a table test for truncated-length and short-payload frames;
`FuzzFrameRoundTrip` asserting the round-trip and trailing-byte survival.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzFrameRoundTrip
-fuzztime=2s`.

Set up the module:

```bash
mkdir -p ~/go-exercises/wire/cmd/demo
cd ~/go-exercises/wire
go mod init example.com/wire
```

### The framing format and the round-trip property

A frame is a `binary.Uvarint`-encoded length followed by exactly that many bytes
of payload. `binary.AppendUvarint(dst, n)` appends the variable-length encoding
of `n` to `dst`; a small length costs one byte, a multi-KB length costs two or
three. `EncodeFrame` is then just "append the length, append the payload".

`DecodeFrame` is the interesting half, because it operates on untrusted bytes off
the wire. `binary.Uvarint(buf)` returns `(value, n)`: `n > 0` is the number of
bytes the length prefix consumed, `n == 0` means the buffer was too short to hold
a complete varint, and `n < 0` means the varint overflowed 64 bits (a hostile or
corrupt length). Both non-positive cases are `ErrTruncated`. After reading the
length you must check that the remaining buffer actually holds that many payload
bytes — a frame claiming a 1 MB payload with 10 bytes left is truncated. Only
then do you slice out the payload and return the unconsumed tail as `rest`, which
lets the caller decode a *stream* of frames back-to-back.

The fuzz property is the codec law: for any `[]byte b`, decoding what you encoded
must return exactly `b`, no error, and an empty `rest`. A single fuzz target
catches every framing off-by-one — a length written one too small, a payload
slice off by one, a varint boundary mishandled. A second, stronger seed encodes
`b`, appends trailing garbage, and asserts the decoder returns `b` and hands the
garbage back as `rest` untouched: the property that framing composes into a
stream.

Create `frame.go`:

```go
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrTruncated is returned (wrapped) when a buffer does not hold a complete
// frame: an incomplete length prefix or fewer payload bytes than the prefix
// promises.
var ErrTruncated = errors.New("truncated frame")

// EncodeFrame returns a self-describing frame: a uvarint length prefix followed
// by payload.
func EncodeFrame(payload []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(len(payload)))
	return append(out, payload...)
}

// DecodeFrame reads one frame from the front of buf. It returns the payload, the
// bytes after the frame (the next frame in a stream), or ErrTruncated.
func DecodeFrame(buf []byte) (payload, rest []byte, err error) {
	n, adv := binary.Uvarint(buf)
	if adv <= 0 {
		return nil, nil, fmt.Errorf("read length: %w", ErrTruncated)
	}
	body := buf[adv:]
	if uint64(len(body)) < n {
		return nil, nil, fmt.Errorf("want %d payload bytes, have %d: %w", n, len(body), ErrTruncated)
	}
	return body[:n], body[n:], nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/wire"
)

func main() {
	stream := append(wire.EncodeFrame([]byte("hello")), wire.EncodeFrame([]byte("queue"))...)
	fmt.Printf("stream is %d bytes\n", len(stream))

	rest := stream
	for len(rest) > 0 {
		var payload []byte
		var err error
		payload, rest, err = wire.DecodeFrame(rest)
		if err != nil {
			fmt.Println("decode error:", err)
			return
		}
		fmt.Printf("frame: %q\n", payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stream is 12 bytes
frame: "hello"
frame: "queue"
```

### Tests

Create `frame_test.go`:

```go
package wire

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestDecodeMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"length-says-5-payload-has-2", append(EncodeFrame([]byte("hello"))[:1], 'h', 'i')},
		{"overflow-varint", bytes.Repeat([]byte{0xff}, 11)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := DecodeFrame(tc.in)
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("DecodeFrame(%v) err = %v, want ErrTruncated", tc.in, err)
			}
		})
	}
}

func FuzzFrameRoundTrip(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("x"))
	f.Add([]byte("hello, queue"))
	f.Add(bytes.Repeat([]byte("A"), 4096))
	f.Fuzz(func(t *testing.T, b []byte) {
		payload, rest, err := DecodeFrame(EncodeFrame(b))
		if err != nil {
			t.Fatalf("DecodeFrame(EncodeFrame(%d bytes)) error: %v", len(b), err)
		}
		if !bytes.Equal(payload, b) {
			t.Fatalf("round-trip payload = %q, want %q", payload, b)
		}
		if len(rest) != 0 {
			t.Fatalf("round-trip left %d trailing bytes, want 0", len(rest))
		}

		// Trailing bytes after a frame must survive as rest, untouched.
		trailer := []byte{0xDE, 0xAD}
		framed := append(EncodeFrame(b), trailer...)
		payload, rest, err = DecodeFrame(framed)
		if err != nil {
			t.Fatalf("decode with trailer: %v", err)
		}
		if !bytes.Equal(payload, b) || !bytes.Equal(rest, trailer) {
			t.Fatalf("with trailer: payload=%q rest=%v, want %q and %v", payload, rest, b, trailer)
		}
	})
}

func Example() {
	frame := EncodeFrame([]byte("ok"))
	payload, rest, err := DecodeFrame(frame)
	fmt.Printf("%q %d %v\n", payload, len(rest), err)
	// Output: "ok" 0 <nil>
}
```

## Review

The codec is correct when `EncodeFrame` and `DecodeFrame` are true inverses:
every byte slice survives the round trip unchanged with no error and no leftover,
and any buffer too short for its own declared length is rejected with
`ErrTruncated` rather than panicking on an out-of-range slice. The trap the fuzz
target guards is the payload-length check — omit `uint64(len(body)) < n` and a
hostile length turns `body[:n]` into a slice-bounds panic on the first malformed
frame from the network. Note the codec uses `binary.Uvarint`'s three-way return
(`adv > 0`, `adv == 0`, `adv < 0`) rather than assuming success. Run
`go test -race ./...`, then `go test -fuzz=FuzzFrameRoundTrip -fuzztime=2s`.

## Resources

- [`encoding/binary` varints](https://pkg.go.dev/encoding/binary#AppendUvarint) — `AppendUvarint`, `PutUvarint`, and `Uvarint`'s `(value, n)` contract.
- [Go Fuzzing reference](https://go.dev/doc/security/fuzz/) — corpus, minimization, and the `-fuzz`/`-fuzztime` flags.
- [`bytes.Equal`](https://pkg.go.dev/bytes#Equal) — the byte-exact comparison the round-trip property uses.

---

Back to [01-fuzz-parseint-invariant.md](01-fuzz-parseint-invariant.md) | Next: [03-differential-ipv4-parser.md](03-differential-ipv4-parser.md)
