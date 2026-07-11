# Exercise 5: A Decompressing ReadCloser that Closes Both Layers

The canonical stacked-`Close` failure mode. Given an `io.ReadCloser` holding
gzip-compressed bytes — an upstream HTTP response body — you return an
`io.ReadCloser` that transparently decompresses, and whose `Close` closes the gzip
reader AND the underlying source. Forget the source and you leak the connection.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
gzipbody/                   independent module: example.com/gzipbody
  go.mod                    go 1.26
  gzipbody.go               type gzipReadCloser; New closes gzip.Reader then source on Close
  cmd/
    demo/
      main.go               gzip some bytes, decode them back through the composed reader
  gzipbody_test.go          round-trip, single Close closes both layers, joined error, corrupt header
```

- Files: `gzipbody.go`, `cmd/demo/main.go`, `gzipbody_test.go`.
- Implement: `New(src io.ReadCloser) (io.ReadCloser, error)` that wraps `src` in a `gzip.Reader`; `Read` decompresses; `Close` closes the gzip reader then the source, joining their errors with `errors.Join`.
- Test: round-trip known bytes; a single `Close` closes the source spy exactly once; `Close` surfaces the source's error via `errors.Join`; a corrupt header makes `New` return an error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gzipbody/cmd/demo
cd ~/go-exercises/gzipbody
go mod init example.com/gzipbody
```

### Why gzip.Reader.Close is not enough

`gzip.NewReader(src)` reads the gzip header from `src` and returns a `*gzip.Reader`
that decompresses on `Read`. Its `Close` finishes and validates the gzip stream —
but it does **not** close `src`. That is by design: the decoder does not own the
source. In an HTTP client, `src` is `resp.Body`; if you decompress and then close
only the gzip layer, `resp.Body` is never closed, the underlying TCP connection is
never returned to the transport's pool, and under load the pool exhausts and every
request starts blocking on a free connection. The bug is invisible in a unit test
and catastrophic in production.

The composition fix is a single `io.ReadCloser` that owns both layers. `Read`
delegates to the gzip reader. `Close` closes the gzip reader *and then* the source,
and joins the two errors with `errors.Join` so a failure in either is reported
rather than swallowed — you want to know if the gzip stream was truncated *and* if
the connection close failed. The order (decoder first, then source) mirrors the
data-flow order and lets the decoder finish reading any trailing bytes before the
source is torn down.

Create `gzipbody.go`:

```go
package gzipbody

import (
	"compress/gzip"
	"errors"
	"io"
)

// gzipReadCloser decodes gzip data from src and, on Close, closes BOTH the gzip
// reader and the underlying source, so an upstream connection is not leaked.
type gzipReadCloser struct {
	gz  *gzip.Reader
	src io.ReadCloser
}

// New wraps a gzip-compressed source. The returned ReadCloser yields the
// decompressed bytes; closing it closes the decoder and then the source. If the
// gzip header is invalid, New returns the error and closes nothing new.
func New(src io.ReadCloser) (io.ReadCloser, error) {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return nil, err
	}
	return &gzipReadCloser{gz: gz, src: src}, nil
}

func (g *gzipReadCloser) Read(p []byte) (int, error) {
	return g.gz.Read(p)
}

// Close closes the decoder first, then the source, joining both errors so neither
// is lost.
func (g *gzipReadCloser) Close() error {
	return errors.Join(g.gz.Close(), g.src.Close())
}

// Static assertion: the composed value is an io.ReadCloser.
var _ io.ReadCloser = (*gzipReadCloser)(nil)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"example.com/gzipbody"
)

func main() {
	// Produce gzip-compressed bytes, as an upstream server would.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte("compressed payload from upstream"))
	gw.Close()

	src := io.NopCloser(bytes.NewReader(buf.Bytes()))
	r, err := gzipbody.New(src)
	if err != nil {
		panic(err)
	}

	data, _ := io.ReadAll(r)
	fmt.Printf("decoded: %s\n", data)
	fmt.Println("close:", r.Close())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
decoded: compressed payload from upstream
close: <nil>
```

### Tests

Create `gzipbody_test.go`:

```go
package gzipbody

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
)

// spyCloser records how many times Close was called and can inject a Close error.
type spyCloser struct {
	r      io.Reader
	closes int
	err    error
}

func (s *spyCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *spyCloser) Close() error {
	s.closes++
	return s.err
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTripAndClosesSource(t *testing.T) {
	t.Parallel()
	spy := &spyCloser{r: bytes.NewReader(gzipBytes(t, []byte("hello world")))}
	r, err := New(spy)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("decoded = %q, want hello world", got)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if spy.closes != 1 {
		t.Fatalf("source closed %d times, want 1", spy.closes)
	}
}

func TestCloseJoinsSourceError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("source close failed")
	spy := &spyCloser{r: bytes.NewReader(gzipBytes(t, []byte("x"))), err: sentinel}
	r, err := New(spy)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	io.ReadAll(r)

	if err := r.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("Close err = %v, want to wrap source sentinel", err)
	}
	if spy.closes != 1 {
		t.Fatalf("source closed %d times, want 1", spy.closes)
	}
}

func TestCorruptHeaderRejected(t *testing.T) {
	t.Parallel()
	src := io.NopCloser(strings.NewReader("this is not gzip data"))
	if _, err := New(src); err == nil {
		t.Fatal("expected an error on a corrupt gzip header")
	}
}
```

## Review

The wrapper is correct when a single `Close` on the composed reader closes the
source exactly once — `spy.closes == 1` in both close tests is the guard against
the leak — and when a source-close failure surfaces through `errors.Join` rather
than being dropped in favor of the (nil) gzip-close result. `TestCorruptHeaderRejected`
confirms `New` reports a malformed stream at construction rather than deferring the
failure to the first `Read`. The one-line lesson: when you stack a decoder over an
owned resource, the decoder's `Close` is never sufficient — compose a `ReadCloser`
whose `Close` reaches every layer that owns something.

## Resources

- [`compress/gzip`](https://pkg.go.dev/compress/gzip) — `NewReader`, `Reader.Close` (and its explicit non-closing of the source), `NewWriter`.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining the decoder and source close errors.
- [`io.NopCloser`](https://pkg.go.dev/io#NopCloser) — adapting a reader into a ReadCloser for the source.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-metered-request-body.md](04-metered-request-body.md) | Next: [06-segregated-repository-composition.md](06-segregated-repository-composition.md)
