# Exercise 4: A Stream Copy-and-Hash Pipeline That Accepts io.Reader and io.Writer Only

`io.Reader` and `io.Writer` are the canonical proof that a one-method interface
is the most reusable thing you can write. This module builds `CopyAndDigest`, the
core of an object-upload path: it streams bytes from any source to any sink while
computing a SHA-256 digest in one pass, and because it depends only on
`io.Reader` and `io.Writer`, it runs unchanged over a file, an HTTP body, a byte
buffer, or a network socket.

## What you'll build

```text
streamdigest/                  independent module: example.com/streamdigest
  go.mod                       go 1.24
  digest.go                    CopyAndDigest(dst io.Writer, src io.Reader) (int64, [32]byte, error)
  cmd/
    demo/
      main.go                  streams a string into a buffer, prints bytes + hex digest
  digest_test.go               precomputed sha256 match; failingWriter error path with partial count
```

Files: `digest.go`, `cmd/demo/main.go`, `digest_test.go`.
Implement: `CopyAndDigest` using `io.TeeReader` (or `io.MultiWriter`) so the stream is hashed without buffering the whole payload.
Test: feed a `strings.NewReader` into a `bytes.Buffer`; assert byte count, buffer contents, and a precomputed SHA-256; add a `failingWriter` that errors after N bytes and assert the error surfaces with a partial count.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the smallest interfaces are the most reusable

`CopyAndDigest` needs two capabilities: pull bytes and push bytes. It declares
them as `io.Reader` and `io.Writer` — one method each. That is the entire reason
the function is universal. An `*os.File`, an `*http.Request` body (`io.ReadCloser`,
which embeds `io.Reader`), a `bytes.Buffer`, a `net.Conn`, a `strings.Reader`,
and a `gzip.Writer` all satisfy these interfaces, so the same function copies
between any pair of them without knowing what they concretely are. Had the
signature demanded a fat "file" type with `Seek`/`Stat`/`Close`, none of that
would compose.

The one-pass hashing is the composition payoff. `io.TeeReader(src, hasher)`
returns a reader that, every time it is read, also writes what it read into the
hasher. Wrapping `src` in a tee and then `io.Copy(dst, tee)` streams the payload
to `dst` and feeds the hasher simultaneously — no second pass, no buffering the
whole object in memory. This matters for a real upload path where the payload may
be gigabytes: you get the content-hash for free as the bytes flow to storage. An
equivalent construction uses `io.MultiWriter(dst, hasher)` and copies `src` into
it; both are shown below and both are pure interface composition.

`io.Copy` returns the number of bytes written and the first error it hit, so a
sink that fails partway (a full disk, a closed socket) surfaces both the error
and how far it got. `CopyAndDigest` returns that partial count so the caller can
log or resume.

Create `digest.go`:

```go
package streamdigest

import (
	"crypto/sha256"
	"io"
)

// CopyAndDigest streams src into dst and returns the number of bytes copied and
// the SHA-256 of the copied bytes. It depends only on io.Reader and io.Writer,
// so it works over files, HTTP bodies, buffers, and sockets alike. The hash is
// computed in one pass via io.TeeReader; the payload is never fully buffered.
func CopyAndDigest(dst io.Writer, src io.Reader) (int64, [32]byte, error) {
	h := sha256.New()
	tee := io.TeeReader(src, h)
	n, err := io.Copy(dst, tee)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return n, sum, err
}

// CopyAndDigestMulti is the io.MultiWriter formulation of the same operation:
// copy src into a writer that fans every byte to both dst and the hasher.
func CopyAndDigestMulti(dst io.Writer, src io.Reader) (int64, [32]byte, error) {
	h := sha256.New()
	mw := io.MultiWriter(dst, h)
	n, err := io.Copy(mw, src)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return n, sum, err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"

	"example.com/streamdigest"
)

func main() {
	src := strings.NewReader("upload payload: object v1\n")
	var dst bytes.Buffer

	n, sum, err := streamdigest.CopyAndDigest(&dst, src)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("copied %d bytes\n", n)
	fmt.Printf("sha256: %s\n", hex.EncodeToString(sum[:]))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
copied 26 bytes
sha256: b77506a25bd1b038ce16b5f900bd0bb9f6e13037df250493bb8ca985398f0971
```

That digest is the real SHA-256 of the 26-byte payload
(`printf 'upload payload: object v1\n' | shasum -a 256`). The test below also
asserts against a precomputed digest so correctness never depends on the demo.

### Tests

The test asserts three things at once against a known input: the byte count, the
copied contents, and the SHA-256. The digest is precomputed inside the test from
`sha256.Sum256` of the same bytes, so the assertion is self-checking and does not
rely on a magic constant. The `failingWriter` proves the error path: it accepts
`limit` bytes then returns an error, and the test confirms `CopyAndDigest`
surfaces that error and reports the partial count.

Create `digest_test.go`:

```go
package streamdigest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCopyAndDigestMatchesPrecomputed(t *testing.T) {
	t.Parallel()

	payload := "the quick brown fox jumps over the lazy dog"
	want := sha256.Sum256([]byte(payload))

	var dst bytes.Buffer
	n, sum, err := CopyAndDigest(&dst, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("n = %d, want %d", n, len(payload))
	}
	if dst.String() != payload {
		t.Fatalf("dst = %q, want %q", dst.String(), payload)
	}
	if sum != want {
		t.Fatalf("sum = %x, want %x", sum, want)
	}
}

func TestCopyAndDigestMultiAgrees(t *testing.T) {
	t.Parallel()

	payload := "identical bytes, two constructions"
	var a, b bytes.Buffer

	_, sumA, err := CopyAndDigest(&a, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, sumB, err := CopyAndDigestMulti(&b, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if sumA != sumB {
		t.Fatalf("tee sum %x != multiwriter sum %x", sumA, sumB)
	}
}

// errShortWrite is returned by failingWriter once its limit is reached.
var errShortWrite = errors.New("writer full")

// failingWriter accepts up to limit bytes then fails, modeling a full disk or a
// dropped socket mid-stream.
type failingWriter struct {
	limit   int
	written int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, errShortWrite
	}
	if len(p) > remaining {
		w.written += remaining
		return remaining, errShortWrite
	}
	w.written += len(p)
	return len(p), nil
}

func TestCopyAndDigestSurfacesWriterError(t *testing.T) {
	t.Parallel()

	src := strings.NewReader("this payload is longer than the writer accepts")
	dst := &failingWriter{limit: 10}

	n, _, err := CopyAndDigest(dst, src)
	if !errors.Is(err, errShortWrite) {
		t.Fatalf("err = %v, want errShortWrite", err)
	}
	if n != 10 {
		t.Fatalf("partial count = %d, want 10", n)
	}
}

func ExampleCopyAndDigest() {
	var dst bytes.Buffer
	n, sum, _ := CopyAndDigest(&dst, strings.NewReader("hi"))
	fmt.Printf("%d %x\n", n, sum[:4])
	// Output: 2 8f434346
}

var _ io.Writer = (*failingWriter)(nil)
```

The `ExampleCopyAndDigest` digest prefix `8f434346` is the real first four bytes
of `sha256("hi")`; `go test` verifies it against a live run.

## Review

The pipeline is correct when it copies exactly the source bytes to the sink and
the returned digest equals `sha256.Sum256` of those same bytes — the test pins
both against a precomputed value so a regression cannot hide. The reusability is
the lesson: because the signature is `io.Writer, io.Reader` and nothing wider, the
same function serves a file upload, an HTTP proxy, and a buffer test with no
change. The failure-path test matters in production — a sink that dies mid-stream
must surface its error and its partial count so the caller can retry or resume,
which is exactly what `io.Copy`'s two return values give you. Avoid the temptation
to read the whole payload into memory to hash it; `io.TeeReader` hashes in one
streaming pass. Run `go test -race` to confirm the streaming path is clean.

## Resources

- [io package (Reader, Writer, Copy, TeeReader, MultiWriter)](https://pkg.go.dev/io)
- [crypto/sha256 package](https://pkg.go.dev/crypto/sha256)
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-consumer-defined-handler-dep.md](03-consumer-defined-handler-dep.md) | Next: [05-read-only-cache-view.md](05-read-only-cache-view.md)
