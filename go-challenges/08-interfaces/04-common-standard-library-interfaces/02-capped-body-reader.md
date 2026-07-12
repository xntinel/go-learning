# Exercise 2: A Size-Capped io.Reader That Rejects Oversized Request Bodies

Reading an untrusted request body with `io.ReadAll` and no limit is a
memory-exhaustion DoS: one client streaming gigabytes fills your heap. The fix is
a reader that counts bytes and refuses past a cap — exactly what
`http.MaxBytesReader` does. This module builds that reader from scratch, which is
the cleanest way to learn how to implement `Read(p []byte) (int, error)`
correctly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cappedbody/                 independent module: example.com/cappedbody
  go.mod
  cappedbody.go             CappedReader wrapping io.Reader; ErrBodyTooLarge sentinel
  cmd/
    demo/
      main.go               reads an in-cap body, then an over-cap body
  cappedbody_test.go        under/at/over cap, short-buffer accumulation, random chunks
```

- Files: `cappedbody.go`, `cmd/demo/main.go`, `cappedbody_test.go`.
- Implement: `CappedReader` with `Read(p []byte) (int, error)` that delivers at most `limit` bytes total and returns `ErrBodyTooLarge` once the source has more, plus `NewCappedReader(r io.Reader, limit int64) *CappedReader`.
- Test: body under cap reads fully and ends in `io.EOF`; body exactly at cap succeeds; body over cap returns the sentinel via `errors.Is` at the boundary; short-buffer reads accumulate across calls; no bytes past the cap are delivered.
- Verify: `go test -count=1 -race ./...`

### How MaxBytesReader actually works

The naive cap — count bytes and error once the count passes `limit` — has an
off-by-one that matters: a body of *exactly* `limit` bytes must succeed, and only
byte `limit+1` is a violation. To tell "exactly at the cap" from "one past it",
you have to attempt to read one byte beyond the allowance. That is precisely the
trick `http.MaxBytesReader` uses, and this module reproduces it.

The reader keeps `remaining`, the bytes still allowed (initially `limit`). On each
`Read`, if the caller's buffer `p` is larger than `remaining+1`, it is sliced down
to `remaining+1` — there is no reason to read more than one byte past the cap,
since one extra byte already answers the question. Then it reads from the source.
If the source returned no more than `remaining` bytes, that is legal: decrement
`remaining`, pass through the source's own error (including `io.EOF`), and return.
If the source returned *more* than `remaining` bytes, the cap is exceeded: deliver
only the `remaining` allowed bytes (the extra bytes read into `p` beyond that are
simply not reported, so the caller never sees them), latch the error so every
subsequent `Read` also fails, and return `ErrBodyTooLarge`.

Latching the error in a field is important: `io.ReadAll` calls `Read` in a loop,
and once tripped the reader must stay tripped rather than reading more from the
source. Note how the implementation honours the `Read` contract from the concepts
file — it respects `len(p)`, it can return `n > 0` with a non-nil error in the
same call (the over-cap branch delivers bytes and an error together), it never
retains `p`, and it propagates `io.EOF` unchanged on the under-cap path.

Create `cappedbody.go`:

```go
package cappedbody

import (
	"errors"
	"io"
)

// ErrBodyTooLarge is returned by CappedReader.Read once the underlying source
// yields more than the configured limit.
var ErrBodyTooLarge = errors.New("cappedbody: body exceeds limit")

// CappedReader wraps an io.Reader and delivers at most `remaining` bytes before
// it trips. It mirrors the behaviour of http.MaxBytesReader.
type CappedReader struct {
	r         io.Reader
	remaining int64
	err       error // latched once tripped
}

// NewCappedReader caps r at limit bytes total.
func NewCappedReader(r io.Reader, limit int64) *CappedReader {
	return &CappedReader{r: r, remaining: limit}
}

func (c *CappedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Read at most one byte past the allowance: that single extra byte is
	// enough to distinguish "exactly at the cap" from "over the cap".
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}

	n, err := c.r.Read(p)
	if int64(n) <= c.remaining {
		// Within the allowance: pass the source's own error through.
		c.remaining -= int64(n)
		c.err = err
		return n, err
	}

	// The source produced more than the allowance. Deliver only the allowed
	// prefix (the surplus bytes are dropped, never reported) and latch.
	n = int(c.remaining)
	c.remaining = 0
	c.err = ErrBodyTooLarge
	return n, c.err
}

var _ io.Reader = (*CappedReader)(nil)
```

### The runnable demo

The demo reads a small body that fits under the cap, then a large body that blows
past it, and reports the outcome of each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"example.com/cappedbody"
)

func main() {
	small := cappedbody.NewCappedReader(strings.NewReader("under the cap"), 64)
	data, err := io.ReadAll(small)
	fmt.Printf("read %d bytes, err=%v\n", len(data), err)

	big := cappedbody.NewCappedReader(strings.NewReader(strings.Repeat("x", 5000)), 1024)
	got, err := io.ReadAll(big)
	fmt.Printf("oversized: delivered %d bytes, tooLarge=%v\n", len(got), errors.Is(err, cappedbody.ErrBodyTooLarge))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read 13 bytes, err=<nil>
oversized: delivered 1024 bytes, tooLarge=true
```

The over-cap read delivers exactly `1024` bytes (the cap) and no more — `io.ReadAll`
never sees byte 1025.

### Tests

`TestUnderCap` reads a body smaller than the cap and asserts it comes through
whole with no error. `TestExactlyAtCap` pins the boundary: a body of exactly
`limit` bytes must succeed. `TestOverCap` asserts the sentinel via `errors.Is`
and that exactly `limit` bytes were delivered, never one more.
`TestShortBufferAccumulates` reads through a tiny buffer to prove the byte
accounting is correct across many `Read` calls, not just one. `TestRandomChunks`
feeds the reader through a source that returns awkward, varying chunk sizes and
asserts the cap still holds — a lightweight fuzz over the accumulation logic.

Create `cappedbody_test.go`:

```go
package cappedbody

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestUnderCap(t *testing.T) {
	t.Parallel()

	r := NewCappedReader(strings.NewReader("small body"), 1024)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll under cap: %v", err)
	}
	if string(data) != "small body" {
		t.Fatalf("data = %q, want %q", data, "small body")
	}
}

func TestExactlyAtCap(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("a", 100)
	r := NewCappedReader(strings.NewReader(body), 100)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("body of exactly the cap must succeed, got: %v", err)
	}
	if len(data) != 100 {
		t.Fatalf("delivered %d bytes, want 100", len(data))
	}
}

func TestOverCap(t *testing.T) {
	t.Parallel()

	r := NewCappedReader(strings.NewReader(strings.Repeat("b", 101)), 100)
	data, err := io.ReadAll(r)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if len(data) != 100 {
		t.Fatalf("delivered %d bytes past the cap; want exactly 100", len(data))
	}
}

func TestShortBufferAccumulates(t *testing.T) {
	t.Parallel()

	r := NewCappedReader(strings.NewReader("0123456789"), 8)
	var got []byte
	buf := make([]byte, 3) // deliberately tiny
	var readErr error
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			readErr = err
			break
		}
	}
	if !errors.Is(readErr, ErrBodyTooLarge) {
		t.Fatalf("final err = %v, want ErrBodyTooLarge", readErr)
	}
	if string(got) != "01234567" {
		t.Fatalf("accumulated %q, want %q", got, "01234567")
	}
}

func TestRandomChunks(t *testing.T) {
	t.Parallel()

	for range 200 {
		size := rand.IntN(2000)
		cap64 := int64(rand.IntN(2000))
		src := &choppyReader{data: bytes.Repeat([]byte{'z'}, size)}
		r := NewCappedReader(src, cap64)
		data, err := io.ReadAll(r)

		if int64(size) > cap64 {
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("size=%d cap=%d: err=%v, want ErrBodyTooLarge", size, cap64, err)
			}
			if int64(len(data)) != cap64 {
				t.Fatalf("size=%d cap=%d: delivered %d, want %d", size, cap64, len(data), cap64)
			}
		} else {
			if err != nil {
				t.Fatalf("size=%d cap=%d: unexpected err %v", size, cap64, err)
			}
			if len(data) != size {
				t.Fatalf("size=%d cap=%d: delivered %d, want %d", size, cap64, len(data), size)
			}
		}
	}
}

// choppyReader returns the data in randomly-sized chunks to stress the
// accumulation logic across many Read calls.
type choppyReader struct {
	data []byte
	pos  int
}

func (c *choppyReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	max := len(c.data) - c.pos
	if len(p) < max {
		max = len(p)
	}
	if max > 1 {
		max = 1 + rand.IntN(max)
	}
	n := copy(p, c.data[c.pos:c.pos+max])
	c.pos += n
	return n, nil
}

func ExampleCappedReader() {
	r := NewCappedReader(strings.NewReader("hi"), 100)
	data, _ := io.ReadAll(r)
	fmt.Println(string(data))
	// Output: hi
}
```

## Review

The reader is correct when a body of exactly the cap succeeds, a body one byte
larger fails with `ErrBodyTooLarge`, and the number of bytes ever delivered never
exceeds the cap regardless of how the source chunks its output. The subtle part
is reading one byte past the allowance to distinguish "at" from "over", and
latching the error so `io.ReadAll`'s loop stops. The mistake this prevents is the
real-world one: `io.ReadAll(r.Body)` with no cap. In a handler you would write
`r.Body = cappedbody.NewCappedReader(r.Body, maxBytes)` (or reach for
`http.MaxBytesReader`, which additionally signals the connection to close). Run
`go test -race` — the random-chunk test doubles as a light fuzz over the
accounting.

## Resources

- [io package](https://pkg.go.dev/io) — the `Read` contract, `io.LimitReader`, `io.ReadAll`.
- [http.MaxBytesReader](https://pkg.go.dev/net/http#MaxBytesReader) — the production reader this module reconstructs.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-io-copy-close-pipeline.md](01-io-copy-close-pipeline.md) | Next: [03-redacting-writer.md](03-redacting-writer.md)
