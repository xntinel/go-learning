# Exercise 2: Prove a Prefix-Stripping Reader Survives Pathological Chunk Boundaries

Many wire protocols begin with a fixed magic prefix — a format signature, a
version tag, a framing preamble — that a reader must consume before passing the
remainder through. The classic bug is assuming the first `Read` delivers the
whole prefix. This exercise builds a prefix stripper the correct way with
`io.ReadFull` and proves it against `iotest.OneByteReader` and
`iotest.HalfReader`, which chop every read to the smallest possible size.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
prefixstrip/                independent module: example.com/prefixstrip
  go.mod                    module example.com/prefixstrip
  reader.go                 stripReader consuming a fixed prefix via io.ReadFull; NewStripReader; ErrBadPrefix
  cmd/
    demo/
      main.go               strips a magic prefix and prints the payload
  reader_test.go            identical output across plain/OneByte/Half; short-stream ErrUnexpectedEOF; iotest.TestReader
```

Files: `reader.go`, `cmd/demo/main.go`, `reader_test.go`.
Implement: an `io.Reader` that consumes a fixed-length prefix once (via `io.ReadFull`), rejects a wrong prefix with a sentinel, then streams the remainder unchanged.
Test: `io.ReadAll` output identical across `{plain, OneByteReader, HalfReader}`; a stream shorter than the prefix yields `io.ErrUnexpectedEOF`; `iotest.TestReader` on the stripped output.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/10-testing-readers-with-iotest/02-adversarial-chunking-onebyte-halfreader/cmd/demo
cd go-solutions/12-testing-ecosystem/10-testing-readers-with-iotest/02-adversarial-chunking-onebyte-halfreader
```

### Why io.ReadFull, and why lazily

The naive stripper reads once, checks that it got `len(prefix)` bytes, and passes
the rest through. Under a single large buffer in CI it works; under
`OneByteReader` the first `Read` returns one byte and the "prefix" check fails or,
worse, silently treats one byte as the whole prefix and corrupts the stream. The
fix is `io.ReadFull(s.r, buf)`: it loops until `buf` (exactly `len(prefix)` bytes)
is filled or the stream ends. A short read is invisible to the caller because
`ReadFull` absorbs it.

The prefix is consumed **lazily**, on the first `Read`, guarded by a `stripped`
flag — not in the constructor. Doing I/O in a constructor is a smell: it blocks,
it can fail where the caller cannot handle it, and it breaks the lazy-pipe
expectation. On the first `Read` the reader fills the prefix buffer, validates it,
sets `stripped`, and falls through to serving the body. Every subsequent `Read` is
a direct passthrough.

The error discipline is the teaching point. `io.ReadFull` on a stream shorter than
the prefix returns `io.ErrUnexpectedEOF` (partial fill) or `io.EOF` (zero bytes).
Both mean the same thing here — the stream is too short to even contain the
protocol preamble — so we normalize `io.EOF` to `io.ErrUnexpectedEOF` and surface
it. A truncated preamble is corruption, not a clean empty stream.

One detail that makes it pass `iotest.TestReader`: `TestReader` first calls
`Read(nil)`. On that call we still strip the prefix, then return
`s.r.Read(nil)` which is `0, nil` while the body has data — the exact `Read(nil)`
contract. Because stripping happens once and the body passthrough is byte-exact,
the composed reader satisfies the full contract.

Create `reader.go`:

```go
package prefixstrip

import (
	"bytes"
	"errors"
	"io"
)

// ErrBadPrefix is returned when the stream does not begin with the expected
// protocol preamble.
var ErrBadPrefix = errors.New("prefixstrip: bad prefix")

// stripReader consumes a fixed prefix from r on the first Read, then streams the
// remainder unchanged. It uses io.ReadFull so an arbitrarily short first Read
// cannot fool the prefix check.
type stripReader struct {
	r        io.Reader
	prefix   []byte
	stripped bool
}

// NewStripReader returns a reader that removes prefix from the front of r.
func NewStripReader(r io.Reader, prefix []byte) io.Reader {
	return &stripReader{r: r, prefix: prefix}
}

func (s *stripReader) Read(p []byte) (int, error) {
	if !s.stripped {
		got := make([]byte, len(s.prefix))
		if _, err := io.ReadFull(s.r, got); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if !bytes.Equal(got, s.prefix) {
			return 0, ErrBadPrefix
		}
		s.stripped = true
	}
	return s.r.Read(p)
}
```

### The runnable demo

The demo builds a stream of a magic prefix followed by a JSON body and prints the
stripped remainder.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"strings"

	"example.com/prefixstrip"
)

func main() {
	const magic = "GOX1"
	stream := strings.NewReader(magic + `{"event":"login","user":"alice"}`)

	r := prefixstrip.NewStripReader(stream, []byte(magic))
	out, err := io.ReadAll(r)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("payload: %s\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
payload: {"event":"login","user":"alice"}
```

### Tests

The core test drives the same prefixed payload through three wrappers — plain,
`iotest.OneByteReader`, and `iotest.HalfReader` — and asserts the stripped output
is byte-identical every time. If the reader assumed one `Read` equals the whole
prefix, the one-byte and half wrappers would break it. A separate test proves a
stream shorter than the prefix returns `io.ErrUnexpectedEOF`, and a wrong prefix
returns `ErrBadPrefix`. Finally `iotest.TestReader` certifies the full contract on
the stripped body.

Create `reader_test.go`:

```go
package prefixstrip

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const magic = "GOX1"

func source(payload string) io.Reader {
	return strings.NewReader(magic + payload)
}

func TestStripsAcrossChunkBoundaries(t *testing.T) {
	t.Parallel()
	const payload = "the quick brown fox jumps over the lazy dog"

	wrappers := map[string]func(io.Reader) io.Reader{
		"plain":   func(r io.Reader) io.Reader { return r },
		"oneByte": iotest.OneByteReader,
		"half":    iotest.HalfReader,
	}

	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := NewStripReader(wrap(source(payload)), []byte(magic))
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != payload {
				t.Fatalf("got %q, want %q", got, payload)
			}
		})
	}
}

func TestShortStreamIsUnexpectedEOF(t *testing.T) {
	t.Parallel()
	// Stream shorter than the 4-byte prefix.
	r := NewStripReader(strings.NewReader("GO"), []byte(magic))
	_, err := io.ReadAll(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestWrongPrefixRejected(t *testing.T) {
	t.Parallel()
	r := NewStripReader(strings.NewReader("BADXpayload"), []byte(magic))
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrBadPrefix) {
		t.Fatalf("err = %v, want ErrBadPrefix", err)
	}
}

func TestStrippedOutputPassesIotest(t *testing.T) {
	t.Parallel()
	const payload = "HELLO WORLD"
	r := NewStripReader(source(payload), []byte(magic))
	if err := iotest.TestReader(r, []byte(payload)); err != nil {
		t.Fatalf("iotest.TestReader failed: %v", err)
	}
}
```

## Review

The stripper is correct when the prefix check is done with `io.ReadFull`, so no
chunking can smuggle a partial prefix past it, and when a too-short stream is
classified as `io.ErrUnexpectedEOF` rather than a clean empty read. The three-way
wrapper test is the proof that matters: identical output under plain,
`OneByteReader`, and `HalfReader` means the reader has no hidden dependence on
read size. If the one-byte case diverges, the bug is a single-`Read` assumption
somewhere in the strip logic. Keep the prefix consumption lazy and one-shot; doing
it in the constructor, or re-checking on every `Read`, is both wrong and slower.

## Resources

- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — fills the buffer or returns `io.ErrUnexpectedEOF`.
- [`testing/iotest`](https://pkg.go.dev/testing/iotest) — `OneByteReader` and `HalfReader`.
- [`io.ErrUnexpectedEOF`](https://pkg.go.dev/io#pkg-variables) — the truncated-frame sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-transforming-reader-passes-iotest.md](01-transforming-reader-passes-iotest.md) | Next: [03-dataerr-eof-with-final-data.md](03-dataerr-eof-with-final-data.md)
