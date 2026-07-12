# Exercise 1: A Case-Normalizing Stream Transform That Passes iotest.TestReader

The most common streaming component in a backend is a byte transform that wraps
an underlying reader and rewrites bytes as they flow through: canonicalizing a
header or token stream, normalizing case, redacting secrets. This exercise builds
one — a case-normalizing reader — and proves it obeys the full `io.Reader`
contract with `iotest.TestReader`, not just a single happy-path read.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
casenorm/                   independent module: example.com/casenorm
  go.mod                    module example.com/casenorm
  reader.go                 uppercaseReader transform; stringReader source; NewReader, NewUppercaseReader
  cmd/
    demo/
      main.go               reads a normalized token stream end to end
  reader_test.go            iotest.TestReader contract test + EOF, partial, ReadAll, ReadFrom, empty
```

Files: `reader.go`, `cmd/demo/main.go`, `reader_test.go`.
Implement: an `io.Reader` that ASCII-uppercases bytes as they stream through, wrapping a minimal `stringReader` that returns `io.EOF` when drained.
Test: `iotest.TestReader` against the transformed content, plus EOF propagation, partial read, `io.ReadAll`, `bytes.Buffer.ReadFrom`, and empty input.
Verify: `go test -count=1 -race ./...`

### Why a transform is the right shape to prove the contract

A transforming reader is a passthrough that touches each byte, so it inherits the
underlying reader's chunking exactly. That makes it the cleanest place to see the
contract: whatever `n` the source returns, the transform must uppercase `p[:n]`
and return the same `(n, err)` unchanged. The dangerous temptation is to buffer —
to "collect the whole input, transform it, then serve it" — which breaks
streaming, unbounds memory, and turns a lazy pipe into a blocking one. The correct
transform mutates in place: it reads into the caller's buffer, walks `p[:n]`, and
returns immediately.

The subtle rule that `iotest.TestReader` enforces here is `Read(nil)`. When called
with an empty buffer, a reader with data remaining must return `0, nil` (not EOF,
not an error). Our transform gets this for free because `stringReader.Read(nil)`
copies zero bytes and returns `0, nil` while data remains. `TestReader` also drives
the reader with a `smallByteReader` that hands it one-to-three bytes per call, so a
transform that assumed a large buffer would be caught. Because the transform is
byte-at-a-time, it survives any chunking.

`stringReader` is the minimal correct source: it returns `io.EOF` (never `0, nil`)
once drained, so `io.ReadAll` terminates. Getting that one line wrong — returning
`0, nil` at the end — hangs every caller, which is the third Common Mistake.

Create `reader.go`:

```go
package casenorm

import "io"

// uppercaseReader wraps an io.Reader and ASCII-uppercases each byte as it
// streams through. It never buffers: it transforms the caller's slice in place
// and returns the same (n, err) the source produced.
type uppercaseReader struct {
	r io.Reader
}

// NewUppercaseReader returns a reader that ASCII-uppercases bytes read from r.
func NewUppercaseReader(r io.Reader) io.Reader {
	return &uppercaseReader{r: r}
}

func (u *uppercaseReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	for i := range n {
		if p[i] >= 'a' && p[i] <= 'z' {
			p[i] -= 'a' - 'A'
		}
	}
	return n, err
}

// stringReader is a minimal io.Reader over a string. It returns io.EOF once
// drained, never 0, nil, so io.ReadAll and copy loops terminate.
type stringReader struct {
	rest string
}

func (s *stringReader) Read(p []byte) (int, error) {
	if len(s.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}

// NewReader returns a case-normalizing reader over the string s.
func NewReader(s string) io.Reader {
	return NewUppercaseReader(&stringReader{rest: s})
}
```

### The runnable demo

The demo streams a lowercase token through the transform with `io.ReadAll`,
showing the normalized result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"

	"example.com/casenorm"
)

func main() {
	r := casenorm.NewReader("authorization: bearer abc123")
	out, err := io.ReadAll(r)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("normalized: %s\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
normalized: AUTHORIZATION: BEARER ABC123
```

### Tests

`TestNewReaderPassesIotest` is the anchor: `iotest.TestReader` runs the multi-size
read suite and the `Read(nil)`/EOF checks. The rest pin specific contract points:
EOF propagation on a drained source, a short-buffer partial read, `io.ReadAll` of
the full stream, `bytes.Buffer.ReadFrom` as an alternate consumer, and empty input
producing an empty slice with a nil error.

Create `reader_test.go`:

```go
package casenorm

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

func TestNewReaderPassesIotest(t *testing.T) {
	t.Parallel()
	if err := iotest.TestReader(NewReader("hello world"), []byte("HELLO WORLD")); err != nil {
		t.Fatalf("iotest.TestReader failed: %v", err)
	}
}

func TestReadsAndUppercases(t *testing.T) {
	t.Parallel()
	r := NewUppercaseReader(&stringReader{rest: "hello"})
	buf := make([]byte, 5)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "HELLO" {
		t.Fatalf("buf = %q, want HELLO", got)
	}
}

func TestPropagatesEOF(t *testing.T) {
	t.Parallel()
	r := NewUppercaseReader(&stringReader{rest: ""})
	_, err := r.Read(make([]byte, 5))
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestPartialRead(t *testing.T) {
	t.Parallel()
	r := NewUppercaseReader(&stringReader{rest: "hello"})
	buf := make([]byte, 3)
	n, _ := r.Read(buf)
	if got := string(buf[:n]); got != "HEL" {
		t.Fatalf("buf = %q, want HEL", got)
	}
}

func TestReadAllFullStream(t *testing.T) {
	t.Parallel()
	got, err := io.ReadAll(NewReader("hello world"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "HELLO WORLD" {
		t.Fatalf("got = %q, want HELLO WORLD", string(got))
	}
}

func TestReadFrom(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(NewReader("hi there")); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if buf.String() != "HI THERE" {
		t.Fatalf("buf = %q, want HI THERE", buf.String())
	}
}

func TestEmptyInput(t *testing.T) {
	t.Parallel()
	got, err := io.ReadAll(NewUppercaseReader(&stringReader{rest: ""}))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bytes, want empty", len(got))
	}
}

func ExampleNewReader() {
	out, _ := io.ReadAll(NewReader("mixed Case Header"))
	fmt.Println(string(out))
	// Output: MIXED CASE HEADER
}
```

## Review

The transform is correct when it is a pure passthrough that touches only `p[:n]`
and forwards `(n, err)` untouched: never buffering, never reordering EOF, never
returning `0, nil` from a source that still has data. The proof is
`iotest.TestReader`, which drives the reader with small and empty buffers and the
EOF-at-end check; if it fails, the transform is making an assumption about chunk
size or mishandling the empty-buffer or EOF case. Watch the source too:
`stringReader` returns `io.EOF` when drained, which is what lets `io.ReadAll` and
`ReadFrom` terminate. Run `go test -race` to confirm the whole suite passes.

## Resources

- [`testing/iotest`](https://pkg.go.dev/testing/iotest) — `TestReader` and the adversarial reader wrappers.
- [`io.Reader`](https://pkg.go.dev/io#Reader) — the exact `Read` contract this exercise obeys.
- [`io.ReadAll`](https://pkg.go.dev/io#ReadAll) — why a source must return `io.EOF` to terminate it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-adversarial-chunking-onebyte-halfreader.md](02-adversarial-chunking-onebyte-halfreader.md)
