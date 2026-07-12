# Exercise 4: Extract an Auth Token Without Pinning the Whole Request Buffer

A request handler reads a full header block into a large `[]byte`, finds the
bearer token as a short run of bytes inside it, and stores that token in a
long-lived `Session`. If it stores the raw sub-slice, the tiny token pins the
entire read buffer for the session's whole lifetime — one 32-byte token holding a
64 KiB buffer alive, times every logged-in user. This module builds the parser and
fixes the pin with `slices.Clone`, and shows why `slices.Clip` is *not* the fix.

## What you'll build

```text
token/                       independent module: example.com/token
  go.mod                     go 1.24
  token.go                   type Session; Parse (Clone), ParseRaw (sub-slice), ParseClip
  cmd/
    demo/
      main.go                parse a token, mutate the source, show Clone stayed independent
  token_test.go              clone-independent, raw-aliases, clone-releases-buffer, raw-pins; -race
```

Files: `token.go`, `cmd/demo/main.go`, `token_test.go`.
Implement: `Parse` (stores `slices.Clone(token)`), `ParseRaw` (stores the sub-slice), `ParseClip` (stores `slices.Clip(token)`).
Test: mutate the source after parse and assert only the raw/clip paths alias; use a `HeapAlloc` delta to prove `Parse` releases the buffer and `ParseRaw` pins it.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Clone severs the pin; Clip does not

The parser locates the token with `bytes.Index` for the `Bearer ` prefix and
`bytes.IndexByte` for the trailing CR, then `bytes.TrimSpace` to strip stray
spaces. Everything up to here returns a *sub-slice* of the read buffer — the right
bytes, but a header that points into the buffer's array. What you do with that
sub-slice at the storage boundary is the whole lesson.

`Parse` calls `slices.Clone(tok)`. Clone allocates a fresh array holding only the
token's bytes, so the stored `Session.Token` does not point into the read buffer:
once the handler returns and drops the buffer, the buffer is reclaimable. This is
the correct fix — a defensive copy at the ownership boundary between the
short-lived buffer and the long-lived session.

`ParseRaw` stores the sub-slice directly. It is the bug: the session now pins the
whole read buffer until the session dies.

`ParseClip` stores `slices.Clip(tok)`, and it is a deliberate trap. Clip returns
`tok[:len(tok):len(tok)]` — it trims spare capacity so a later `append` reallocates,
but it does **not** copy: the result still points into the read buffer's array and
pins it exactly as hard as `ParseRaw` does. Clip is the right tool when *you own*
the array and only need to bound future appends or drop a trailing spare tail; it
is the wrong tool when your goal is to stop pinning someone else's buffer. The
test proves this by showing a `ParseClip` token still aliases the source. The
rule to carry away: Clone makes a new array immediately; Clip keeps the same array
until the next append. Only Clone severs an existing pin.

Create `token.go`:

```go
// Package token extracts a bearer token from a request buffer without pinning it.
package token

import (
	"bytes"
	"errors"
	"slices"
)

// ErrNoToken is returned when the buffer holds no bearer token.
var ErrNoToken = errors.New("token: no bearer token in buffer")

var bearer = []byte("Bearer ")

// Session is long-lived. Whatever it stores in Token is retained for the session.
type Session struct {
	Token []byte
}

// locate returns the token as a SUB-SLICE of buf (it points into buf's array).
func locate(buf []byte) ([]byte, error) {
	i := bytes.Index(buf, bearer)
	if i < 0 {
		return nil, ErrNoToken
	}
	rest := buf[i+len(bearer):]
	end := bytes.IndexByte(rest, '\r')
	if end < 0 {
		end = len(rest)
	}
	tok := bytes.TrimSpace(rest[:end])
	if len(tok) == 0 {
		return nil, ErrNoToken
	}
	return tok, nil
}

// Parse stores an independent clone of the token. Dropping buf frees it: the
// session does not pin the read buffer. This is the correct implementation.
func Parse(buf []byte) (*Session, error) {
	tok, err := locate(buf)
	if err != nil {
		return nil, err
	}
	return &Session{Token: slices.Clone(tok)}, nil
}

// ParseRaw stores the located sub-slice directly, pinning the whole read buffer
// for the session's lifetime. Kept only to contrast with Parse.
func ParseRaw(buf []byte) (*Session, error) {
	tok, err := locate(buf)
	if err != nil {
		return nil, err
	}
	return &Session{Token: tok}, nil
}

// ParseClip stores slices.Clip(tok). Clip drops spare capacity but does not copy,
// so the result still points into buf and still pins it. This is NOT a fix for
// the pin; it only bounds future appends. Kept to show Clip is not Clone.
func ParseClip(buf []byte) (*Session, error) {
	tok, err := locate(buf)
	if err != nil {
		return nil, err
	}
	return &Session{Token: slices.Clip(tok)}, nil
}
```

## The runnable demo

The demo parses a token from a header block, then overwrites the source buffer and
shows the cloned token is unaffected — the observable sign it does not alias the
buffer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/token"
)

func main() {
	buf := []byte("Host: api.example.com\r\nAuthorization: Bearer s3cr3t-abc\r\n\r\n")

	sess, err := token.Parse(buf)
	if err != nil {
		fmt.Println("parse:", err)
		return
	}
	fmt.Printf("token: %s\n", sess.Token)

	// Overwrite the source buffer; the cloned token must not change.
	for i := range buf {
		buf[i] = '.'
	}
	fmt.Printf("token after source overwrite: %s\n", sess.Token)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
token: s3cr3t-abc
token after source overwrite: s3cr3t-abc
```

## Tests

The correctness tests overwrite the source buffer after parsing: `Parse`'s token is
unchanged (independent), while `ParseRaw` and `ParseClip` tokens change (they alias
the buffer). The memory tests use a `HeapAlloc` delta around a forced GC — a large
buffer, a short token, drop the buffer — to prove `Parse` releases the buffer and
`ParseRaw` pins it. The memory tests are non-parallel so their global `HeapAlloc`
readings are not perturbed by other tests.

Create `token_test.go`:

```go
package token

import (
	"bytes"
	"errors"
	"runtime"
	"testing"
)

const bufSize = 8 << 20 // 8 MiB read buffer; half = 4 MiB threshold.

// buildBuf returns a large buffer whose header contains the given token.
func buildBuf(tok string) []byte {
	b := make([]byte, bufSize)
	for i := range b {
		b[i] = 'x'
	}
	header := []byte("Authorization: Bearer " + tok + "\r\n")
	copy(b, header)
	return b
}

func readHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestParseClonesToken(t *testing.T) {
	t.Parallel()

	buf := []byte("Authorization: Bearer abc123\r\n")
	sess, err := Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sess.Token, []byte("abc123")) {
		t.Fatalf("token = %q, want abc123", sess.Token)
	}

	for i := range buf { // clobber the source
		buf[i] = 0
	}
	if !bytes.Equal(sess.Token, []byte("abc123")) {
		t.Fatalf("clone changed with source: %q", sess.Token)
	}
}

func TestParseRawAliasesSource(t *testing.T) {
	t.Parallel()

	buf := []byte("Authorization: Bearer abc123\r\n")
	sess, err := ParseRaw(buf)
	if err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] = 0
	}
	if bytes.Equal(sess.Token, []byte("abc123")) {
		t.Fatal("ParseRaw token did not alias source; it should have changed")
	}
}

func TestParseClipStillAliases(t *testing.T) {
	t.Parallel()

	buf := []byte("Authorization: Bearer abc123\r\n")
	sess, err := ParseClip(buf)
	if err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] = 0
	}
	if bytes.Equal(sess.Token, []byte("abc123")) {
		t.Fatal("Clip token did not alias source; Clip does not copy, so it should have changed")
	}
}

func TestParseNoToken(t *testing.T) {
	t.Parallel()

	if _, err := Parse([]byte("Host: example.com\r\n")); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

func TestCloneReleasesBuffer(t *testing.T) {
	base := readHeap()

	buf := buildBuf("short-token")
	sess, err := Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(buf)
	buf = nil
	after := readHeap()

	half := int64(bufSize) / 2
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("Parse pinned the buffer: delta %d bytes, want < %d", delta, half)
	}
	runtime.KeepAlive(sess)
}

func TestRawPinsBuffer(t *testing.T) {
	base := readHeap()

	buf := buildBuf("short-token")
	sess, err := ParseRaw(buf)
	if err != nil {
		t.Fatal(err)
	}
	buf = nil
	after := readHeap()

	half := int64(bufSize) / 2
	if delta := int64(after) - int64(base); delta < half {
		t.Fatalf("ParseRaw did not pin the buffer: delta %d bytes, want >= %d", delta, half)
	}
	runtime.KeepAlive(sess)
}
```

## Review

The parser is correct when `Parse` returns a token that neither aliases nor pins
the read buffer: overwriting the source leaves the cloned token intact, and
dropping the buffer lets its 8 MiB be reclaimed. `ParseRaw` and `ParseClip` are the
two traps — the first stores a sub-slice, the second stores a Clip that people
mistake for a copy; both alias and both pin, and the tests document each. The
mistake this module exists to prevent is retaining a sub-slice of a large read
buffer in a long-lived struct, and its close cousin, reaching for `slices.Clip`
believing it copies. Clone when you must not pin the source; Clip only when you own
the array and just need to bound the next append. Run `go test -race` to confirm
the parse path is safe under concurrent requests.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the copy that severs the pin.
- [`slices.Clip`](https://pkg.go.dev/slices#Clip) — trims spare capacity without copying; note it does not sever an existing pin.
- [`bytes.Index` and `bytes.TrimSpace`](https://pkg.go.dev/bytes#Index) — locating and trimming the token inside the buffer.

---

Back to [03-subscriber-registry-delete-zeroing.md](03-subscriber-registry-delete-zeroing.md) | Next: [05-scanner-line-copy-on-retain.md](05-scanner-line-copy-on-retain.md)
