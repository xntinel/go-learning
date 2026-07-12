# Exercise 6: A gzip Middleware That Reuses Writers via Reset

A `gzip.Writer` is expensive to construct — it allocates compression tables and
window buffers — so a compression middleware that builds a fresh one per request
wastes exactly the kind of allocation `sync.Pool` exists to eliminate. This
module builds a pooled gzip middleware, and in the process exercises the two
subtleties that make `gzip.Writer` poolable: `Reset(w)` to rebind it to the
current response, and `Close` to flush the footer before returning it.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
gzipmw/                     independent module: example.com/gzipmw
  go.mod                    go 1.26
  gzipmw/
    middleware.go           Gzip(next) http.Handler; pooled *gzip.Writer, Reset+Close
  cmd/
    demo/
      main.go               drives the middleware and decodes the compressed body
  gzipmw/middleware_test.go Content-Encoding, round-trip decode, concurrent streams
```

Files: `gzipmw/middleware.go`, `cmd/demo/main.go`, `gzipmw/middleware_test.go`.
Implement: `Gzip(next http.Handler) http.Handler` that, for clients advertising `Accept-Encoding: gzip`, gets a pooled `*gzip.Writer`, `Reset`s it to the `ResponseWriter`, wraps the writer, and `Close`s + returns it after the handler runs.
Test: `httptest` asserts `Content-Encoding: gzip` and that `gzip.NewReader` decodes the body to the original payload; a concurrent `-race` test proves pooled writers are Reset per request with no stream corruption.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/05-sync-pool/06-gzip-response-compression-middleware/go-solutions/15-sync-primitives/05-sync-pool/06-gzip-response-compression-middleware go-solutions/15-sync-primitives/05-sync-pool/06-gzip-response-compression-middleware/cmd/demo
cd go-solutions/15-sync-primitives/05-sync-pool/06-gzip-response-compression-middleware
```

### Reset to rebind, Close to flush — the poolable lifecycle

A `gzip.Writer` is bound to a destination `io.Writer` at construction. That is a
problem for pooling: the writer you pull from the pool was last pointed at some
previous request's `ResponseWriter`, which is long gone. `Reset(w)` is the answer
— it clears the compressor's internal state *and* rebinds it to a new
destination. This is the whole reason `compress/gzip` ships a `Reset` method: it
is what lets one recycled writer serve every connection in turn. `Get` from the
pool, `Reset(w)` to the current response, and the writer is ready.

The other half is `Close`. GZIP frames a stream with a trailing footer — a
CRC-32 checksum and the uncompressed length — that is only emitted when you call
`Close`. Skip it and the client receives a truncated stream that fails to
decode. So the lifecycle is strict: `Reset(w)`, write through the writer, then
`Close` to flush the footer *before* the handler returns, and only then `Put` the
writer back. Crucially, `gzip.Writer.Close` closes the gzip stream but does *not*
close the underlying `ResponseWriter`, so calling it on a pooled writer is safe —
the socket stays open for the server to finish the response.

Two header details make the middleware correct HTTP. You must remove any
`Content-Length` the inner handler set, because the compressed length differs
from the uncompressed one and a stale length corrupts the response framing. And
you set `Vary: Accept-Encoding` so caches key on whether the client asked for
compression. The wrapping `gzipResponseWriter` overrides only `Write`, routing
the handler's bytes through the gzip writer while leaving header and status
handling to the embedded `ResponseWriter`.

Create `gzipmw/middleware.go`:

```go
package gzipmw

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// writerPool recycles gzip writers across requests. New returns a writer bound
// to io.Discard; every Get rebinds it to the real destination via Reset. New
// returns a pointer, so putting it into the pool's interface value is free.
var writerPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipResponseWriter routes body writes through the gzip writer while leaving
// header and status handling to the embedded ResponseWriter.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.gz.Write(b)
}

// Gzip wraps next so that responses to clients advertising gzip support are
// compressed through a pooled, per-request gzip.Writer.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz := writerPool.Get().(*gzip.Writer)
		gz.Reset(w) // rebind this recycled writer to the current response
		defer func() {
			gz.Close() // flush the GZIP footer before reuse; leaves w open
			writerPool.Put(gz)
		}()

		h := w.Header()
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length") // compressed length differs from what next set

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}
```

### The runnable demo

The demo wraps a tiny handler, drives it with an in-process recorder that
advertises gzip, and decodes the compressed body back to prove the round trip.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/gzipmw/gzipmw"
)

func main() {
	const payload = "the quick brown fox jumps over the lazy dog, repeatedly and compressibly"

	handler := gzipmw.Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, payload)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	fmt.Printf("content-encoding=%s\n", rec.Header().Get("Content-Encoding"))
	fmt.Printf("compressed-bytes=%d original-bytes=%d\n", rec.Body.Len(), len(payload))

	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		panic(err)
	}
	decoded, _ := io.ReadAll(zr)
	fmt.Printf("decoded=%s\n", decoded)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (compressed byte count may vary slightly by zlib version):

```
content-encoding=gzip
compressed-bytes=76 original-bytes=72
decoded=the quick brown fox jumps over the lazy dog, repeatedly and compressibly
```

### Tests

The tests assert the header and the decoded round trip, then fire many
concurrent requests with *distinct* payloads under `-race` — distinct payloads
are what expose a stream that got corrupted by a writer that was not properly
Reset between requests.

Create `gzipmw/middleware_test.go`:

```go
package gzipmw

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func echoHandler(payload string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, payload)
	})
}

func decodeGzip(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading gzip stream: %v", err)
	}
	return string(out)
}

func TestGzipRoundTrip(t *testing.T) {
	t.Parallel()

	const payload = "hello gzip world, this string is long enough to compress"
	handler := Gzip(echoHandler(payload))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if got := decodeGzip(t, rec.Body.Bytes()); got != payload {
		t.Fatalf("decoded = %q, want %q", got, payload)
	}
}

func TestNoGzipWhenNotAccepted(t *testing.T) {
	t.Parallel()

	const payload = "plain response"
	handler := Gzip(echoHandler(payload))

	req := httptest.NewRequest(http.MethodGet, "/", nil) // no Accept-Encoding
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want empty", enc)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body = %q, want %q (uncompressed)", rec.Body.String(), payload)
	}
}

func TestConcurrentStreamsNoCorruption(t *testing.T) {
	t.Parallel()

	const n = 300
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			payload := fmt.Sprintf("request-%d-payload-with-enough-bytes-to-compress", i)
			handler := Gzip(echoHandler(payload))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if got := decodeGzip(t, rec.Body.Bytes()); got != payload {
				t.Errorf("stream corruption: got %q, want %q", got, payload)
			}
		}()
	}
	wg.Wait()
}
```

## Review

The middleware is correct when a gzip-accepting client gets a `Content-Encoding:
gzip` response that decodes exactly back to the handler's bytes, and a
non-accepting client gets the plain body untouched. The two failure modes to
respect are both in the lifecycle: forgetting `Reset(w)` reuses a writer still
pointed at a dead response (or carrying prior state), and forgetting `Close`
ships a footerless, undecodable stream. `TestConcurrentStreamsNoCorruption`
gives each of 300 goroutines a unique payload, so any writer that was not cleanly
Reset between requests produces a decode mismatch under `-race`. Run
`go test -race`.

## Resources

- [`gzip.Writer.Reset`](https://pkg.go.dev/compress/gzip#Writer.Reset) — clears state and rebinds to a new destination, the key to pooling.
- [`gzip.Writer.Close`](https://pkg.go.dev/compress/gzip#Writer.Close) — flushes the footer; does not close the underlying writer.
- [`compress/gzip`](https://pkg.go.dev/compress/gzip) — the writer/reader pair and the GZIP framing.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-json-response-encoder-handler.md](05-json-response-encoder-handler.md) | Next: [07-byte-slice-pool-pointer.md](07-byte-slice-pool-pointer.md)
