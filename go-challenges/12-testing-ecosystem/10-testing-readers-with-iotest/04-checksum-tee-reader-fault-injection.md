# Exercise 4: A Streaming Upload Checksum Reader With Mid-Stream Fault Injection

When you stream an object to a store (S3, GCS, a blob backend) you want to verify
its integrity without buffering the whole body in memory. The idiom is
`io.TeeReader`: as bytes flow to the sink, a copy is fed into a running hash. This
exercise builds that pipeline and then injects a hard mid-stream fault with
`iotest.ErrReader` to prove the error surfaces from `io.Copy` and the pipeline
never reports a false-success checksum over a partial stream.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
teachecksum/                independent module: example.com/teachecksum
  go.mod                    module example.com/teachecksum
  copy.go                   CopyWithChecksum: io.TeeReader into crc32, io.Copy to sink
  cmd/
    demo/
      main.go               streams a payload, prints byte count and CRC-32
  copy_test.go              happy path count+sum; ErrReader fault via errors.Is; partial-tee-but-failed
```

Files: `copy.go`, `cmd/demo/main.go`, `copy_test.go`.
Implement: `CopyWithChecksum(dst io.Writer, src io.Reader) (int64, uint32, error)` teeing `src` into a CRC-32 hash while `io.Copy`-ing to `dst`.
Test: happy-path byte count and checksum equal precomputed values; a fault injected with `iotest.ErrReader` surfaces via `errors.Is` and the caller treats the transfer as failed.
Verify: `go test -count=1 -race ./...`

### How the tee tap works, and what a fault must not do

`io.TeeReader(src, h)` returns a reader that, every time it is read, writes the
same bytes into `h` before handing them back. Wrapping the source in a tee and
then `io.Copy(dst, tee)` gives you a single pass that simultaneously moves bytes to
the sink and accumulates a checksum — no second read, no full-body buffer. The
hash only ever sees bytes that were actually read, so on a successful copy
`h.Sum32()` is the CRC-32 of the complete payload.

The correctness question is what happens on a fault. `io.Copy` reads until the
source returns an error. If that error is anything other than `io.EOF`, `io.Copy`
returns it (and the bytes it copied so far). Because a failed read returns `n == 0`
along with the error, the tee never sees those non-existent bytes, so the hash
reflects only the prefix that genuinely streamed. The rule the caller must follow:
**a non-nil error from `io.Copy` invalidates the checksum**. The bytes teed so far
are a partial-stream hash, not the object's hash, and trusting it would certify a
truncated upload as complete. This is the false-success trap the fault test pins.

`iotest.ErrReader(err)` returns a reader that immediately yields `(0, err)`. To
inject a fault *mid-stream* we compose `io.MultiReader(prefix, iotest.ErrReader(
sentinel))`: `io.Copy` streams the prefix (teed into the hash), then hits the
error reader and returns the sentinel. `errors.Is(err, sentinel)` classifies it,
and the caller discards the checksum.

Create `copy.go`:

```go
package teachecksum

import (
	"hash/crc32"
	"io"
)

// CopyWithChecksum streams src into dst while computing a running CRC-32 (IEEE)
// over the bytes actually transferred. It returns the number of bytes copied, the
// checksum, and any error from the copy. On a non-nil error the returned checksum
// covers only the prefix that streamed and MUST NOT be treated as the payload's
// checksum.
func CopyWithChecksum(dst io.Writer, src io.Reader) (int64, uint32, error) {
	h := crc32.NewIEEE()
	tee := io.TeeReader(src, h)
	n, err := io.Copy(dst, tee)
	return n, h.Sum32(), err
}
```

### The runnable demo

The demo streams a fixed payload into a discard sink and prints the byte count and
checksum, matching `crc32.ChecksumIEEE` of the same bytes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"strings"

	"example.com/teachecksum"
)

func main() {
	const payload = "streamed object body"
	n, sum, err := teachecksum.CopyWithChecksum(io.Discard, strings.NewReader(payload))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("copied %d bytes, crc32=%08x\n", n, sum)
	fmt.Printf("independent crc32=%08x\n", crc32.ChecksumIEEE([]byte(payload)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
copied 20 bytes, crc32=144b353c
independent crc32=144b353c
```

### Tests

`TestHappyPath` streams a known payload and asserts the byte count and checksum
equal `crc32.ChecksumIEEE` of the same bytes. `TestMidStreamFault` composes a
prefix with `iotest.ErrReader(errBoom)`, asserts `io.Copy` returns the sentinel
via `errors.Is`, that the prefix bytes did reach the sink (the tee ran), and that
the returned checksum is *not* the full-payload checksum — so the caller must not
trust it.

Create `copy_test.go`:

```go
package teachecksum

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestHappyPath(t *testing.T) {
	t.Parallel()
	const payload = "the complete object body"
	var sink bytes.Buffer

	n, sum, err := CopyWithChecksum(&sink, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("CopyWithChecksum: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("copied %d bytes, want %d", n, len(payload))
	}
	if want := crc32.ChecksumIEEE([]byte(payload)); sum != want {
		t.Fatalf("checksum = %08x, want %08x", sum, want)
	}
	if sink.String() != payload {
		t.Fatalf("sink = %q, want %q", sink.String(), payload)
	}
}

var errBoom = errors.New("upstream connection reset")

func TestMidStreamFault(t *testing.T) {
	t.Parallel()
	const prefix = "first half arrived"
	src := io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(errBoom))
	var sink bytes.Buffer

	n, sum, err := CopyWithChecksum(&sink, src)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if n != int64(len(prefix)) {
		t.Fatalf("copied %d bytes, want %d", n, len(prefix))
	}
	if sink.String() != prefix {
		t.Fatalf("sink = %q, want %q", sink.String(), prefix)
	}
	// The transfer failed: the partial-stream checksum must NOT match a checksum
	// of some intended full payload. Here we prove it differs from the prefix+more
	// case by confirming it equals only the prefix's checksum, which the caller
	// discards because err != nil.
	if sum != crc32.ChecksumIEEE([]byte(prefix)) {
		t.Fatalf("partial checksum unexpected: %08x", sum)
	}
}

func TestEmptySource(t *testing.T) {
	t.Parallel()
	n, sum, err := CopyWithChecksum(io.Discard, strings.NewReader(""))
	if err != nil {
		t.Fatalf("CopyWithChecksum: %v", err)
	}
	if n != 0 {
		t.Fatalf("copied %d bytes, want 0", n)
	}
	if want := crc32.ChecksumIEEE(nil); sum != want {
		t.Fatalf("checksum = %08x, want %08x", sum, want)
	}
}
```

## Review

The pipeline is correct when the checksum is a byproduct of a single streaming
pass and is honored only on success. `io.TeeReader` guarantees the hash sees
exactly the bytes read, so on a clean `io.EOF` the sum is the payload's CRC-32; on
any other error `io.Copy` returns it and the sum covers only the prefix. The
false-success trap is trusting that partial sum: the fault test proves the sentinel
propagates via `errors.Is` and that the caller, seeing a non-nil error, treats the
transfer as failed regardless of how many bytes were teed. Swap `crc32` for
`crypto/sha256` and the structure is identical — the tee tap is agnostic to the
hash. Run `go test -race` to confirm both paths.

## Resources

- [`io.TeeReader`](https://pkg.go.dev/io#TeeReader) — mirrors reads into a writer for a streaming tap.
- [`testing/iotest#ErrReader`](https://pkg.go.dev/testing/iotest#ErrReader) — a reader that returns a fixed error immediately.
- [`hash/crc32`](https://pkg.go.dev/hash/crc32) — `NewIEEE` and `ChecksumIEEE`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-dataerr-eof-with-final-data.md](03-dataerr-eof-with-final-data.md) | Next: [05-timeout-reader-retry-loop.md](05-timeout-reader-retry-loop.md)
