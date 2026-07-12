# Exercise 1: A Reusable Copy/Close Pipeline Over io.Reader, io.Writer, io.Closer

Every reverse proxy, upload handler, and streaming endpoint has the same core: it
takes bytes from one place (`io.Reader`) and moves them to another
(`io.Writer`), and it closes the resources it opened (`io.Closer`) on the way
out. This module builds that core as a tiny, reusable `pipe` package and proves
its behaviour with tests, framed as the body-forwarding heart of an HTTP proxy
handler.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pipe/                       independent module: example.com/pipe
  go.mod
  pipe.go                   Copy(dst,src); Close(c); ReadCloser (Reader+Closer); NopCloser
  cmd/
    demo/
      main.go               forwards an upstream body into a buffer, then closes it
  pipe_test.go              copy-all-bytes, empty-input contract, NopCloser interop
```

- Files: `pipe.go`, `cmd/demo/main.go`, `pipe_test.go`.
- Implement: `Copy(dst io.Writer, src io.Reader) (int64, error)` wrapping `io.Copy`; `Close(c io.Closer)` that swallows the error for deferred use; a `ReadCloser` embedding `io.Reader`+`io.Closer`; `NopCloser(r io.Reader) ReadCloser`.
- Test: copy-all-bytes with a byte-count assertion, `Close` ignoring an error, `NopCloser` reading then closing without effect, interop with `io.NopCloser`, and the empty-input `(0, nil)` contract.
- Verify: `go test -count=1 -race ./...`

### Why these four pieces

A proxy handler reads `r.Body` (an `io.ReadCloser`) and forwards it to an upstream
connection (an `io.Writer`). `Copy` is the forwarding step; it delegates to
`io.Copy`, which loops `Read`/`Write` with an internal buffer and returns the
total byte count and the first error that stopped it. Wrapping `io.Copy` rather
than hand-rolling a loop is the correct instinct: `io.Copy` already honours the
`Read` contract (process `n` before `err`) and the `Write` contract (a short
write is an error), and it opportunistically uses `io.WriterTo`/`io.ReaderFrom`
fast paths when the concrete types offer them.

`Close` exists so a caller can write `defer pipe.Close(body)` without a
lambda. It calls `Close()` and discards the error, which is the accepted
convention on a *read* path — a drained request body rarely fails to close, and
even if it does there is nothing to recover. (On a write path you would check the
error instead; that distinction is in the concepts file.)

`ReadCloser` is interface composition by embedding: a struct with an embedded
`io.Reader` and an embedded `io.Closer` promotes both methods, so the struct
itself satisfies `io.ReadCloser`. `NopCloser` uses it to bolt a no-op `Close`
onto a plain reader — the exact move you make when you have a `*bytes.Reader` of
a synthetic body and a handler that expects an `io.ReadCloser`. The standard
library ships `io.NopCloser` for this; building your own once shows precisely how
it works, and the test proves the two are interchangeable.

Create `pipe.go`:

```go
package pipe

import "io"

// Copy forwards every byte from src to dst, returning the number of bytes
// copied. It wraps io.Copy, which honours the Reader and Writer contracts and
// uses ReaderFrom/WriterTo fast paths when the concrete types provide them.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}

// Close closes c and swallows the error, so it can be deferred on a read path
// where a failed close carries no recoverable information: defer pipe.Close(c).
func Close(c io.Closer) {
	_ = c.Close()
}

// ReadCloser composes io.Reader and io.Closer by embedding. The promoted Read
// and Close methods make it satisfy io.ReadCloser.
type ReadCloser struct {
	io.Reader
	io.Closer
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// NopCloser wraps r with a no-op Close, yielding an io.ReadCloser for callers
// (like an http.Handler forwarding a synthetic body) that require one.
func NopCloser(r io.Reader) ReadCloser {
	return ReadCloser{Reader: r, Closer: nopCloser{}}
}

// Compile-time proof that the composition satisfies the composed interface.
var _ io.ReadCloser = ReadCloser{}
```

### The runnable demo

The demo plays the proxy: it wraps an upstream payload as an `io.ReadCloser`,
defers its close, forwards it into a buffer with `Copy`, and reports the byte
count and the forwarded body.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"example.com/pipe"
)

func main() {
	// An upstream response body arrives as an io.ReadCloser.
	body := io.NopCloser(strings.NewReader("upstream payload"))
	defer pipe.Close(body)

	var dst bytes.Buffer
	n, err := pipe.Copy(&dst, body)
	if err != nil {
		fmt.Println("copy error:", err)
		return
	}

	fmt.Printf("forwarded %d bytes\n", n)
	fmt.Printf("body: %s\n", dst.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
forwarded 16 bytes
body: upstream payload
```

### Tests

The tests preserve every case the original lesson pinned and add the empty-input
contract test. `TestCopyCopiesAllBytes` asserts the byte count and the copied
content. `TestCopyHandlesEmptyInput` pins that copying an empty reader is
`(0, nil)`, not an error — the "end of stream is not a failure" rule.
`TestCloseIgnoresError` calls `Close` on a closer and asserts nothing panics.
`TestNopCloserReadsAndClosesWithoutEffect` proves the composed `ReadCloser` reads
through to the underlying reader and closes harmlessly.
`TestNopCloserWithIONopCloser` proves our `NopCloser` and `io.NopCloser` are
interchangeable.

Create `pipe_test.go`:

```go
package pipe

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCopyCopiesAllBytes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	n, err := Copy(&buf, strings.NewReader("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Fatalf("n = %d, want 11", n)
	}
	if buf.String() != "hello world" {
		t.Fatalf("buf = %q, want %q", buf.String(), "hello world")
	}
}

func TestCopyHandlesEmptyInput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	n, err := Copy(&buf, strings.NewReader(""))
	if err != nil {
		t.Fatalf("Copy of empty reader returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes for empty input, want 0", buf.Len())
	}
}

func TestCloseIgnoresError(t *testing.T) {
	t.Parallel()

	// Close must not panic and must discard the closer's error.
	Close(io.NopCloser(strings.NewReader("x")))
}

func TestNopCloserReadsAndClosesWithoutEffect(t *testing.T) {
	t.Parallel()

	r := NopCloser(strings.NewReader("hello"))
	defer Close(r)

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want %q", data, "hello")
	}
}

func TestNopCloserWithIONopCloser(t *testing.T) {
	t.Parallel()

	// Our NopCloser and the stdlib io.NopCloser are interchangeable.
	var ours io.ReadCloser = NopCloser(strings.NewReader("x"))
	var std io.ReadCloser = io.NopCloser(strings.NewReader("x"))

	for _, rc := range []io.ReadCloser{ours, std} {
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "x" {
			t.Fatalf("data = %q, want %q", data, "x")
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close returned %v, want nil", err)
		}
	}
}

func ExampleCopy() {
	var buf bytes.Buffer
	n, _ := Copy(&buf, strings.NewReader("proxy"))
	fmt.Println(n, buf.String())
	// Output: 5 proxy
}
```

## Review

The pipeline is correct when `Copy` reports exactly the bytes moved and reproduces
the source verbatim, when copying an empty reader is `(0, nil)` rather than an
error, and when the composed `ReadCloser` both reads through and closes without
side effects. The two mistakes this module guards against are ignoring a resource
until the caller forgets to close it — `Close` plus `defer` is the fix — and
implementing `Read` with the wrong signature so a custom type silently fails to
satisfy `io.Reader`; here you lean on the stdlib's `Read`/`Write` and add
`var _ io.ReadCloser = ReadCloser{}` so any drift fails to compile. Run
`go test -race` to confirm the whole thing holds under the race detector.

## Resources

- [io package](https://pkg.go.dev/io) — `Reader`, `Writer`, `Closer`, `Copy`, `NopCloser`, `ReadAll`.
- [io.Copy](https://pkg.go.dev/io#Copy) — the byte-count/error contract and the `ReaderFrom`/`WriterTo` fast paths.
- [Effective Go: interface composition](https://go.dev/doc/effective_go#interfaces_and_types) — embedding single-method interfaces into larger ones.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-capped-body-reader.md](02-capped-body-reader.md)
