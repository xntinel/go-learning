# Exercise 6: Wire Boundary — []byte vs string, Content-Length and ETag

At the HTTP wire boundary the difference between `[]byte` and `string` is not
stylistic — it decides whether `Content-Length` is correct, whether an `ETag` is a
stable hash of the real bytes, and whether every response pays an unnecessary copy.
This module builds a small response-metadata helper that computes length in bytes,
hashes the raw body, and measures the allocation cost of `string([]byte)`.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
wire/                      independent module: example.com/wire
  go.mod                   go 1.26
  wire.go                  ContentLength, ETag, WriteHeaders
  cmd/
    demo/
      main.go              Content-Length vs rune count, and the ETag, for a UTF-8 body
  wire_test.go             byte-length, golden/deterministic ETag, allocation test
```

- Files: `wire.go`, `cmd/demo/main.go`, `wire_test.go`.
- Implement: `ContentLength(body []byte) int` (bytes), `ETag(body []byte) string` (strong ETag from a SHA-256 of the raw bytes), `WriteHeaders(w, body)` using `fmt.Fprintf`.
- Test: `Content-Length` equals `len(body)` in bytes even for a multi-byte body (differs from rune count); the `ETag` is deterministic and changes on a one-byte edit, with a golden value; `testing.AllocsPerRun` shows `string([]byte)` copies while `len` does not.
- Verify: `go test -count=1 -race ./...`

### Why bytes, and why the copy matters

`Content-Length` is the number of *bytes* on the wire, full stop — it has nothing to
do with characters. For a body of `"Hello, 世界"` the correct header value is 13, not
the 9 runes a `RuneCountInString` would report; a server that set `Content-Length` to
the rune count would under-declare the body and desync the connection. So the helper
computes `len(body)` directly on the `[]byte`, never converting to a string first.

The `ETag` is a strong validator: it must be a stable function of the exact bytes, so
it is a SHA-256 of the raw body, hex-encoded and quoted. The important part is that
the hash consumes `[]byte` directly — `sha256.Sum256` takes a `[]byte`, so there is no
reason to build a `string` first. That matters because `string(body)` and
`[]byte(s)` both *copy*: a `string` is immutable and a `[]byte` is mutable, so they
cannot safely share backing memory, and the conversion allocates a fresh copy every
time. On a hot response path that is a per-request allocation for nothing. The
allocation test makes the cost concrete with `testing.AllocsPerRun`: converting to a
string that escapes allocates, while taking the length of the slice allocates nothing.
The rule that falls out: stay on `[]byte` for length, hashing, and scanning, and
convert to `string` only where an API genuinely requires one.

Create `wire.go`:

```go
package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// ContentLength returns the body size in bytes, the value the Content-Length
// header must carry. It is len(body), not a rune count.
func ContentLength(body []byte) int {
	return len(body)
}

// ETag returns a strong ETag: a quoted hex SHA-256 of the raw body bytes. The
// hash consumes []byte directly, with no string conversion.
func ETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// WriteHeaders writes the Content-Length and ETag headers for body in CRLF wire
// form to w.
func WriteHeaders(w io.Writer, body []byte) {
	fmt.Fprintf(w, "Content-Length: %d\r\n", ContentLength(body))
	fmt.Fprintf(w, "ETag: %s\r\n", ETag(body))
}
```

### The runnable demo

The demo takes a multi-byte body and prints its `Content-Length` (bytes) beside its
rune count so the two are visibly different, then prints the `ETag`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/wire"
)

func main() {
	body := []byte("Hello, 世界")
	fmt.Println("Content-Length:", wire.ContentLength(body))
	fmt.Println("rune count:", utf8.RuneCountInString(string(body)))
	fmt.Println("ETag:", wire.ETag(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Content-Length: 13
rune count: 9
ETag: "a281e84c7f61393db702630c2a6807e871cd3b6896c9e56e22982d125696575c"
```

### Tests

`TestContentLengthIsBytes` asserts the length is the byte count (13) and not the rune
count (9) for a multi-byte body — the header must be bytes. `TestETag` checks the hash
is deterministic for identical bytes, differs for a one-byte change, and matches a
golden value pinned to a fixed body. `TestWriteHeaders` checks the CRLF wire form.
`TestStringConversionAllocates` uses `testing.AllocsPerRun` to show that an escaping
`string(body)` allocates while `len(body)` does not — it is not marked parallel because
it writes package-level sinks and depends on allocation counts.

Create `wire_test.go`:

```go
package wire

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func TestContentLengthIsBytes(t *testing.T) {
	t.Parallel()

	body := []byte("Hello, 世界")
	if got := ContentLength(body); got != 13 {
		t.Fatalf("ContentLength = %d, want 13 bytes", got)
	}
	if runes := utf8.RuneCountInString(string(body)); runes != 9 {
		t.Fatalf("rune count = %d, want 9 (must differ from byte length)", runes)
	}
}

func TestETag(t *testing.T) {
	t.Parallel()

	a := ETag([]byte("Hello, world"))
	b := ETag([]byte("Hello, world"))
	if a != b {
		t.Fatal("ETag not deterministic for identical bytes")
	}
	if c := ETag([]byte("Hello, worlds")); a == c {
		t.Fatal("ETag did not change on a one-byte edit")
	}
	const golden = `"4ae7c3b6ac0beff671efa8cf57386151c06e58ca53a78d83f36107316cec125f"`
	if a != golden {
		t.Fatalf("ETag = %s, want golden %s", a, golden)
	}
}

func TestWriteHeaders(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	WriteHeaders(&buf, []byte("Hello, 世界"))
	got := buf.String()
	if want := "Content-Length: 13\r\n"; !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Fatalf("headers %q missing %q", got, want)
	}
}

var (
	strSink string
	lenSink int
)

func TestStringConversionAllocates(t *testing.T) {
	body := []byte("some response body bytes")

	strAllocs := testing.AllocsPerRun(100, func() {
		strSink = string(body)
	})
	if strAllocs < 1 {
		t.Fatalf("string(body) allocs = %v, want >= 1 (it copies)", strAllocs)
	}

	lenAllocs := testing.AllocsPerRun(100, func() {
		lenSink = len(body)
	})
	if lenAllocs != 0 {
		t.Fatalf("len(body) allocs = %v, want 0", lenAllocs)
	}
	_ = strSink
	_ = lenSink
}
```

## Review

The helpers are correct when `Content-Length` is bytes and the `ETag` is a stable
function of those bytes: `TestContentLengthIsBytes` pins the byte-versus-rune
distinction, and `TestETag`'s golden value catches any accidental change to the hashing
or encoding. The allocation test makes the `string([]byte)` copy visible and is the
reason the production helpers take and hash `[]byte` directly — converting to a string
to hash or to measure length would allocate on every response for no benefit. Keep
`WriteHeaders` on CRLF because that is the wire format; the demo prints fields
individually only to keep its output readable.

## Resources

- [crypto/sha256: Sum256](https://pkg.go.dev/crypto/sha256#Sum256) — hashing `[]byte` directly.
- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations of a closure.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — why `string`/`[]byte` conversions copy.
- [MDN: ETag](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/ETag) — strong vs weak validators.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-float-metrics-slo.md](07-float-metrics-slo.md)
