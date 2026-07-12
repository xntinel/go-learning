# Exercise 9: Streaming NFC Normalization of a Large Upload Without Buffering

The boundary-NFC discipline from Exercise 3 assumed a string you can hold in
memory. An ingestion pipeline — a multi-megabyte CSV upload, a log tail, a document
import — cannot afford `io.ReadAll` then `norm.NFC.String`. This module normalizes
an arbitrarily large stream to NFC on the fly, in constant memory, by wrapping the
source `io.Reader`, and shows the write-side variant too.

This module is fully self-contained: its own `go mod init`, its own demo and tests.
It uses `golang.org/x/text/unicode/norm`.

## What you'll build

```text
nfcstream/                  independent module: example.com/nfcstream
  go.mod                    requires golang.org/x/text
  stream.go                 NormalizeReader (norm.NFC.Reader) and NormalizeWriter (norm.NFC.Writer)
  cmd/demo/main.go          normalize a stream, show it shrinks to NFC
  stream_test.go            boundary-split input; reader==writer==NFC.String; 1-byte reader
```

Files: `stream.go`, `cmd/demo/main.go`, `stream_test.go`.
Implement: `NormalizeReader(dst io.Writer, src io.Reader)` via `io.Copy(dst, norm.NFC.Reader(src))` and `NormalizeWriter(dst io.Writer, src io.Reader)` via `norm.NFC.Writer`, closing the writer to flush.
Test: a multi-KB stream with NFD sequences split across the internal buffer boundary streams to exactly `norm.NFC.String(whole)`; the writer path produces identical bytes; a reader returning one byte per `Read` still normalizes correctly.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get golang.org/x/text/unicode/norm
```

### Why streaming, and the boundary problem it solves

`norm.NFC` is a `transform.Transformer`, so it composes with the `io` streaming
adapters. `norm.NFC.Reader(src)` returns an `io.Reader` that reads from `src` and
yields NFC-normalized bytes; wrap the upload's reader in it and `io.Copy` to the
sink, and you never hold more than a small buffer of the payload. `norm.NFC.Writer(dst)`
is the write-side mirror: bytes written to it are normalized and forwarded to `dst`.
(`transform.NewReader(src, norm.NFC)` and `transform.NewWriter(dst, norm.NFC)` are
the general forms; the `norm.Form` methods are thin conveniences over them.)

The reason this is not trivial is the *boundary problem*. A canonical decomposition
like `e` + U+0301 can straddle two `Read` calls: the base `e` arrives in one chunk,
the combining acute in the next. A naive "normalize each chunk independently"
approach would compose nothing at the split and emit the wrong bytes. The transform
machinery handles this correctly: it buffers an incomplete trailing sequence and
carries it into the next chunk, so the streamed output is byte-for-byte identical to
normalizing the whole input at once — which is exactly what the test asserts, and
what makes streaming safe.

One operational detail: `norm.NFC.Writer` returns an `io.WriteCloser`, and you must
`Close` it. The transformer may be holding a final buffered sequence; `Close`
flushes it. Forgetting the `Close` silently truncates the last few bytes.

Create `stream.go`:

```go
package nfcstream

import (
	"io"

	"golang.org/x/text/unicode/norm"
)

// NormalizeReader copies src to dst, normalizing to NFC on the read side. Memory
// stays constant regardless of stream size: norm.NFC.Reader buffers only a partial
// trailing sequence across Read boundaries.
func NormalizeReader(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, norm.NFC.Reader(src))
}

// NormalizeWriter copies src to dst, normalizing to NFC on the write side. The
// NFC writer is an io.WriteCloser holding a possible trailing sequence, so it must
// be closed to flush; Close errors are surfaced.
func NormalizeWriter(dst io.Writer, src io.Reader) (int64, error) {
	w := norm.NFC.Writer(dst)
	n, err := io.Copy(w, src)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return n, err
}
```

### The runnable demo

The demo pushes an NFD stream (decomposed accents, so more bytes than NFC) through
the reader path and reports how many bytes went in versus out, plus that the result
is genuinely NFC.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"strings"

	"example.com/nfcstream"
	"golang.org/x/text/unicode/norm"
)

func main() {
	// NFD unit: "café naïve " decomposed. Repeat it into a sizable stream.
	unit := "cafe" + string(rune(0x0301)) + " nai" + string(rune(0x0308)) + "ve "
	input := strings.Repeat(unit, 1000)

	var out bytes.Buffer
	n, err := nfcstream.NormalizeReader(&out, strings.NewReader(input))
	if err != nil {
		panic(err)
	}

	fmt.Printf("in bytes:  %d\n", len(input))
	fmt.Printf("out bytes: %d\n", n)
	fmt.Printf("output is NFC: %v\n", norm.NFC.IsNormalString(out.String()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in bytes:  15000
out bytes: 13000
output is NFC: true
```

### Tests

`TestStreamMatchesWholeString` builds a large NFD input and asserts the streamed
output equals `norm.NFC.String` of the whole thing — proving boundary sequences are
carried correctly. `TestOneBytePerRead` runs the same through a reader that returns
a single byte per `Read` (`iotest.OneByteReader`), the worst case for boundary
handling. `TestWriterMatchesReader` asserts the write-side path produces identical
bytes.

Create `stream_test.go`:

```go
package nfcstream

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"testing/iotest"

	"golang.org/x/text/unicode/norm"
)

// bigNFD builds a multi-KB NFD string whose combining marks will straddle the
// transformer's internal buffer boundaries.
func bigNFD() string {
	unit := "cafe" + string(rune(0x0301)) + " nai" + string(rune(0x0308)) + "ve "
	return strings.Repeat(unit, 1000)
}

func TestStreamMatchesWholeString(t *testing.T) {
	t.Parallel()

	input := bigNFD()
	want := norm.NFC.String(input)

	var out bytes.Buffer
	if _, err := NormalizeReader(&out, strings.NewReader(input)); err != nil {
		t.Fatalf("NormalizeReader: %v", err)
	}
	if out.String() != want {
		t.Fatalf("streamed output != NFC.String of whole input (len %d vs %d)", out.Len(), len(want))
	}
}

func TestOneBytePerRead(t *testing.T) {
	t.Parallel()

	input := bigNFD()
	want := norm.NFC.String(input)

	// One byte per Read maximally stresses cross-boundary sequence handling.
	src := iotest.OneByteReader(strings.NewReader(input))
	var out bytes.Buffer
	if _, err := NormalizeReader(&out, src); err != nil {
		t.Fatalf("NormalizeReader(1-byte): %v", err)
	}
	if out.String() != want {
		t.Fatal("1-byte-per-read stream did not normalize identically to the whole string")
	}
}

func TestWriterMatchesReader(t *testing.T) {
	t.Parallel()

	input := bigNFD()

	var viaReader bytes.Buffer
	if _, err := NormalizeReader(&viaReader, strings.NewReader(input)); err != nil {
		t.Fatalf("NormalizeReader: %v", err)
	}
	var viaWriter bytes.Buffer
	if _, err := NormalizeWriter(&viaWriter, strings.NewReader(input)); err != nil {
		t.Fatalf("NormalizeWriter: %v", err)
	}
	if !bytes.Equal(viaReader.Bytes(), viaWriter.Bytes()) {
		t.Fatal("reader-path and writer-path output differ")
	}
}

func ExampleNormalizeReader() {
	in := "cafe" + string(rune(0x0301)) // NFD café
	var out bytes.Buffer
	NormalizeReader(&out, strings.NewReader(in))
	fmt.Println(norm.NFC.IsNormalString(out.String()))
	fmt.Println(out.Len() < len(in))
	// Output:
	// true
	// true
}
```

## Review

The stream normalizer is correct when its output is byte-identical to normalizing
the whole payload at once, no matter how the bytes are chunked: `NormalizeReader`
wraps the source in `norm.NFC.Reader` and `io.Copy`s it, and the transformer carries
an incomplete trailing sequence across `Read` boundaries, which the 1-byte-per-read
test proves in the worst case. The write-side `NormalizeWriter` must `Close` the NFC
writer to flush its final buffered sequence — the mistake that silently truncates
output. The overarching mistake this module exists to prevent is `io.ReadAll` +
`norm.NFC.String` on a large upload: it works on a unit test and falls over in
production when the payload does not fit in memory. Stream it.

## Resources

- [`golang.org/x/text/unicode/norm`](https://pkg.go.dev/golang.org/x/text/unicode/norm) — `Form.Reader`, `Form.Writer`, `Form.String`, `IsNormalString`.
- [`golang.org/x/text/transform`](https://pkg.go.dev/golang.org/x/text/transform) — `NewReader`, `NewWriter`, the general streaming adapters.
- [`testing/iotest`](https://pkg.go.dev/testing/iotest#OneByteReader) — `OneByteReader`, for stressing boundary handling.

---

Back to [08-collation-sort-keys-for-a-db-index.md](08-collation-sort-keys-for-a-db-index.md) | Next: [10-normalization-fast-path-and-fuzz-guard.md](10-normalization-fast-path-and-fuzz-guard.md)
