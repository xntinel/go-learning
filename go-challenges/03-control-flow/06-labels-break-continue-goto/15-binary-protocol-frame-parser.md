# Exercise 15: Skip malformed binary frames in a message stream

**Nivel: Intermedio** — validacion rapida (un test corto).

A network protocol parser reads variable-length frames prefixed with a magic
sequence and a length header off a raw byte stream — a TCP socket buffer, a
message queue payload, a replicated log segment. Real streams get corrupted:
a dropped byte, a bit flip, a partial write. When a candidate frame fails
validation, the parser cannot abort the whole stream; it has to skip past the
bad bytes and keep hunting for the next valid frame boundary. This module is
fully self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
frameparser/                independent module: example.com/frameparser
  go.mod                     go 1.24
  frameparser.go             Frame, ParseFrames
  cmd/
    demo/
      main.go                runnable demo: one clean stream, one corrupted checksum
  frameparser_test.go        table test: empty, valid, leading noise, corrupted, truncated
```

- Files: `frameparser.go`, `cmd/demo/main.go`, `frameparser_test.go`.
- Implement: `ParseFrames(data []byte) (frames []Frame, skipped int)`, scanning for `[magic][length][payload][checksum]` frames and resuming the scan one byte later whenever a candidate fails validation.
- Test: empty input, a single valid frame, junk bytes before a valid frame, a corrupted checksum with a valid frame on either side, and a frame truncated at the end of the buffer.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/frameparser/cmd/demo
cd ~/go-exercises/frameparser
go mod init example.com/frameparser
go mod edit -go=1.24
```

### Why the resync needs a labeled continue

The frame format is `[4-byte magic][2-byte big-endian length][length bytes of
payload][1-byte checksum]`. The scanner tries every byte position as a
possible frame start. Checking the magic itself is a small loop over its four
bytes — and the moment any one of those bytes fails to match, the position is
not a frame start at all, so the scan must move to the very next byte and try
again. A bare `continue` there would only advance to the next magic byte
inside the same failed match; it is the OUTER byte-scan loop that needs to
resume, one byte later. The same reasoning applies once the magic matches but
the length or checksum turns out wrong: those checks happen after the magic
loop already exited normally, so the code that detects them sits directly in
the outer loop's body and can reach it with a bare `continue` — but the magic
loop itself can only reach the outer scan with a labeled one. Resuming at
`i+1` rather than jumping past the whole declared frame length is deliberate:
a corrupted length field cannot be trusted to tell you where the *next* real
frame begins, so the only safe move is to advance one byte and keep scanning.

Create `frameparser.go`:

```go
package frameparser

// frameMagic identifies the start of a frame: 4 fixed bytes.
var frameMagic = [4]byte{0xDE, 0xAD, 0xBE, 0xEF}

// Frame is one successfully parsed frame.
type Frame struct {
	Offset  int
	Payload []byte
}

// ParseFrames scans a byte stream for magic-prefixed, length-delimited
// frames: [4-byte magic][2-byte big-endian length][length bytes of
// payload][1-byte checksum = sum of payload bytes mod 256]. A frame is
// malformed when the magic only partially matches at a position, the
// declared length runs past the end of the buffer, or the checksum byte
// does not match. Rather than aborting the whole scan, a malformed frame is
// skipped: the scanner resumes searching for the next magic sequence one
// byte after the current position, using a labeled continue that jumps out
// of the frame-specific validation loop straight back to the outer
// byte-scan loop.
func ParseFrames(data []byte) (frames []Frame, skipped int) {
	i := 0
scan:
	for i < len(data) {
		if i+4 > len(data) {
			break scan
		}
		for k := 0; k < 4; k++ {
			if data[i+k] != frameMagic[k] {
				i++
				continue scan
			}
		}
		// Magic matched at i; validate the length-prefixed body.
		if i+6 > len(data) {
			skipped++
			i++
			continue scan
		}
		length := int(data[i+4])<<8 | int(data[i+5])
		bodyStart := i + 6
		bodyEnd := bodyStart + length
		if bodyEnd+1 > len(data) {
			skipped++
			i++
			continue scan
		}
		var sum byte
		for _, b := range data[bodyStart:bodyEnd] {
			sum += b
		}
		if sum != data[bodyEnd] {
			skipped++
			i++
			continue scan
		}
		payload := append([]byte(nil), data[bodyStart:bodyEnd]...)
		frames = append(frames, Frame{Offset: i, Payload: payload})
		i = bodyEnd + 1
	}
	return frames, skipped
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/frameparser"
)

// buildFrame encodes one well-formed frame: magic, big-endian length,
// payload, and a checksum equal to the sum of the payload bytes mod 256.
func buildFrame(payload []byte) []byte {
	var sum byte
	for _, b := range payload {
		sum += b
	}
	frame := []byte{0xDE, 0xAD, 0xBE, 0xEF, byte(len(payload) >> 8), byte(len(payload))}
	frame = append(frame, payload...)
	frame = append(frame, sum)
	return frame
}

func main() {
	var stream []byte
	stream = append(stream, buildFrame([]byte("hello"))...)

	// A corrupted frame: valid magic and length, but a tampered checksum
	// byte, simulating bit flips on the wire.
	corrupt := buildFrame([]byte("world"))
	corrupt[len(corrupt)-1] ^= 0xFF
	stream = append(stream, corrupt...)

	stream = append(stream, buildFrame([]byte("ok"))...)

	frames, skipped := frameparser.ParseFrames(stream)
	fmt.Println("frames parsed:", len(frames))
	for _, f := range frames {
		fmt.Printf("  offset=%d payload=%q\n", f.Offset, f.Payload)
	}
	fmt.Println("frames skipped:", skipped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frames parsed: 2
  offset=0 payload="hello"
  offset=24 payload="ok"
frames skipped: 1
```

The corrupted "world" frame never appears: its checksum byte was flipped, so
`ParseFrames` counts it as skipped and resumes scanning right after it,
picking the "ok" frame back up at offset 24.

### Tests

`TestParseFrames` covers five shapes of input: nothing at all, one clean
frame, junk bytes ahead of a clean frame, a corrupted frame sandwiched
between two good ones, and a frame whose declared length runs past the end
of the buffer.

Create `frameparser_test.go`:

```go
package frameparser

import "testing"

func buildFrame(payload []byte, corruptChecksum bool) []byte {
	var sum byte
	for _, b := range payload {
		sum += b
	}
	if corruptChecksum {
		sum ^= 0xFF
	}
	frame := []byte{0xDE, 0xAD, 0xBE, 0xEF, byte(len(payload) >> 8), byte(len(payload))}
	frame = append(frame, payload...)
	frame = append(frame, sum)
	return frame
}

func TestParseFrames(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		data         []byte
		wantPayloads []string
		wantSkipped  int
	}{
		"empty input": {
			data:         nil,
			wantPayloads: nil,
			wantSkipped:  0,
		},
		"single valid frame": {
			data:         buildFrame([]byte("hello"), false),
			wantPayloads: []string{"hello"},
			wantSkipped:  0,
		},
		"leading noise before a valid frame is skipped byte by byte": {
			data:         append([]byte{0x00, 0xDE, 0xAD}, buildFrame([]byte("hi"), false)...),
			wantPayloads: []string{"hi"},
			wantSkipped:  0,
		},
		"a corrupted checksum is skipped, later frames still parse": {
			data: func() []byte {
				var s []byte
				s = append(s, buildFrame([]byte("one"), false)...)
				s = append(s, buildFrame([]byte("bad"), true)...)
				s = append(s, buildFrame([]byte("two"), false)...)
				return s
			}(),
			wantPayloads: []string{"one", "two"},
			wantSkipped:  1,
		},
		"a truncated frame at the end of the buffer is skipped": {
			data: func() []byte {
				full := buildFrame([]byte("truncated"), false)
				return full[:len(full)-3]
			}(),
			wantPayloads: nil,
			wantSkipped:  1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			frames, skipped := ParseFrames(tc.data)
			if len(frames) != len(tc.wantPayloads) {
				t.Fatalf("got %d frames, want %d (%v)", len(frames), len(tc.wantPayloads), frames)
			}
			for i, f := range frames {
				if string(f.Payload) != tc.wantPayloads[i] {
					t.Fatalf("frame[%d].Payload = %q, want %q", i, f.Payload, tc.wantPayloads[i])
				}
			}
			if skipped != tc.wantSkipped {
				t.Fatalf("skipped = %d, want %d", skipped, tc.wantSkipped)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The parser is correct when a malformed frame never derails the scan: the
"corrupted checksum" case is the one to study, since the stream has a valid
frame on both sides of the bad one and both are still recovered. The bug this
exercise guards against is a bare `continue` inside the magic-matching loop —
it would only retry the remaining magic bytes at the same position instead of
advancing the outer scan, hanging the parser in an infinite loop the moment a
non-magic byte appears. Resuming at `i+1` instead of trusting the (possibly
corrupt) declared length is what lets the scanner recover mid-stream instead
of losing synchronization for the rest of the buffer.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [encoding/binary](https://pkg.go.dev/encoding/binary) — the standard approach to fixed-width length prefixes in real wire formats.
- [RFC 9293, TCP framing considerations](https://www.rfc-editor.org/rfc/rfc9293) — background on why stream protocols need explicit resynchronization.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-permission-audit-triple-nested-label.md](14-permission-audit-triple-nested-label.md) | Next: [16-replica-consistency-quorum-check.md](16-replica-consistency-quorum-check.md)
