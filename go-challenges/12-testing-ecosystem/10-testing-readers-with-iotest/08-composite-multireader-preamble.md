# Exercise 8: A Protocol Composer: Prepend a Preamble to a Payload Stream

When you send a message on the wire you often need to prepend a fixed header — a
length prefix, a content-type framing, a magic preamble — before a payload body.
Concatenating them into a new buffer defeats streaming and doubles memory.
`io.MultiReader` composes readers into one without copying, and the composite must
obey the full `io.Reader` contract across the header/body seam. This exercise
proves it with `iotest.TestReader` and `iotest.HalfReader`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
composereader/              independent module: example.com/composereader
  go.mod                    module example.com/composereader
  compose.go                Compose: io.MultiReader(header, body) as a single io.Reader
  cmd/
    demo/
      main.go               prepends a length header to a body, reads it back whole
  compose_test.go           iotest.TestReader on the composite; HalfReader seam test; empty-header/empty-body
```

Files: `compose.go`, `cmd/demo/main.go`, `compose_test.go`.
Implement: `Compose(header, body []byte) io.Reader` returning `io.MultiReader` over the two.
Test: `iotest.TestReader` against the concatenated bytes; `iotest.HalfReader` seam test asserting no bytes lost or duplicated at the boundary; empty-header and empty-body edge cases read cleanly to `io.EOF`.
Verify: `go test -count=1 -race ./...`

### Why MultiReader, and the seam that must be seamless

`io.MultiReader(r1, r2, ...)` returns a reader that reads `r1` to EOF, then `r2`,
and so on, returning its own `io.EOF` only after the last is drained. It never
copies the underlying data into a combined buffer, so composing a 16-byte header
with a 4 GB body allocates 16 bytes plus the readers, not 4 GB. This is the correct
way to frame an outbound message: build a `bytes.Reader` over the header, hand the
body reader as-is, and stream the pair.

The correctness risk lives at the **seam**. A single `Read` on the composite must
not straddle two underlying readers in a way that loses or duplicates a byte, and
the transition from header to body must be invisible to the caller. `io.MultiReader`
handles this: when the header reader returns EOF, `MultiReader` advances to the body
reader within the machinery of the read, presenting one continuous stream. The
`HalfReader` seam test wraps the composite so every read is chopped in half,
forcing many small reads right across the boundary, and asserts the assembled output
equals `header + body` exactly.

`iotest.TestReader` is the full contract certificate. It calls `Read(nil)`
(expecting `0, nil` while data remains — `MultiReader` delegates to the first
non-exhausted reader, and `bytes.Reader.Read(nil)` returns `0, nil`), drives
multi-size reads, and checks the EOF-at-end behavior. The edge cases matter for a
protocol composer: an empty header (a zero-length preamble) or an empty body (a
header-only control frame) must still read cleanly to `io.EOF`, which `MultiReader`
gives because an exhausted reader is simply skipped.

Create `compose.go`:

```go
package composereader

import (
	"bytes"
	"io"
)

// Compose returns a single io.Reader that yields header followed by body, without
// allocating a combined buffer. It is the streaming way to prepend a protocol
// preamble to a payload.
func Compose(header, body []byte) io.Reader {
	return io.MultiReader(bytes.NewReader(header), bytes.NewReader(body))
}
```

### The runnable demo

The demo prepends a 4-byte big-endian length header to a body and reads the whole
composite back, printing the header bytes and the body.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"

	"example.com/composereader"
)

func main() {
	body := []byte("payload-body")
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(body)))

	out, err := io.ReadAll(composereader.Compose(header, body))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("length header: %d\n", binary.BigEndian.Uint32(out[:4]))
	fmt.Printf("body: %s\n", out[4:])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
length header: 12
body: payload-body
```

### Tests

`TestCompositePassesIotest` runs `iotest.TestReader` on `Compose(header, body)`
against the concatenated bytes — the full contract. `TestSeamUnderHalfReader` wraps
the composite in `iotest.HalfReader` and asserts `io.ReadAll` equals `header+body`
with no bytes lost or duplicated at the boundary. `TestEmptyHeader` and
`TestEmptyBody` pin the header-only and body-only edge cases.

Create `compose_test.go`:

```go
package composereader

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
)

func concat(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

func TestCompositePassesIotest(t *testing.T) {
	t.Parallel()
	header := []byte("HDR:")
	body := []byte("the streamed body")
	if err := iotest.TestReader(Compose(header, body), concat(header, body)); err != nil {
		t.Fatalf("iotest.TestReader failed: %v", err)
	}
}

func TestSeamUnderHalfReader(t *testing.T) {
	t.Parallel()
	header := []byte("preamble-")
	body := []byte("body-content")
	want := concat(header, body)

	got, err := io.ReadAll(iotest.HalfReader(Compose(header, body)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEmptyHeader(t *testing.T) {
	t.Parallel()
	body := []byte("just the body")
	got, err := io.ReadAll(Compose(nil, body))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q, want %q", got, body)
	}
}

func TestEmptyBody(t *testing.T) {
	t.Parallel()
	header := []byte("header-only")
	got, err := io.ReadAll(Compose(header, nil))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, header) {
		t.Fatalf("got %q, want %q", got, header)
	}
}
```

## Review

The composer is correct when it streams header-then-body as one continuous reader
with no combined allocation and no seam artifact. `iotest.TestReader` certifies the
full contract including `Read(nil)` and EOF-at-end; the `HalfReader` test is the one
that would expose a boundary bug by forcing many small reads across the header/body
transition. The empty-header and empty-body cases matter because real protocols send
zero-length preambles and header-only control frames, and `io.MultiReader` handles
both by skipping an exhausted reader. Run `go test -race` to confirm all four.

## Resources

- [`io.MultiReader`](https://pkg.go.dev/io#MultiReader) — concatenates readers without copying.
- [`testing/iotest#TestReader`](https://pkg.go.dev/testing/iotest#TestReader) — the full reader-contract suite.
- [`bytes.NewReader`](https://pkg.go.dev/bytes#NewReader) — a reader over a byte slice for the header.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-line-framing-scanner-across-boundaries.md](07-line-framing-scanner-across-boundaries.md) | Next: [09-truncating-writer-short-write.md](09-truncating-writer-short-write.md)
