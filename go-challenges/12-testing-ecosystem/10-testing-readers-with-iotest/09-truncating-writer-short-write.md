# Exercise 9: A Copy Loop That Detects Short Writes to a Truncating Sink

The reader contract has a mirror on the writer side: a `Write` may accept fewer
bytes than offered, and `io.ErrShortWrite` is the signal that not all bytes landed.
A copy loop that assumes `w.Write(p)` always wrote `len(p)` bytes silently drops
data when a sink — a rate-capped `http.ResponseWriter`, a bounded network buffer —
stops accepting. This exercise builds a short-write-detecting copy, proves it, and
exposes the trap in `iotest.TruncateWriter`: it truncates but *reports success*, so
only an independent byte check catches the loss.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
shortwrite/                 independent module: example.com/shortwrite
  go.mod                    module example.com/shortwrite
  copy.go                   boundedWriter (honest short write); CopyChecked (manual short-write detection)
  cmd/
    demo/
      main.go               over-cap payload to honest sink vs iotest.TruncateWriter
  copy_test.go              io.Copy -> ErrShortWrite; manual loop -> ErrShortWrite; TruncateWriter masks; under-limit
```

Files: `copy.go`, `cmd/demo/main.go`, `copy_test.go`.
Implement: `NewBoundedWriter(w io.Writer, cap int) io.Writer` that reports the real short count when full, and `CopyChecked(dst io.Writer, src io.Reader) (int64, error)` that returns `io.ErrShortWrite` when a write is short.
Test: `io.Copy` to the honest bounded sink returns `io.ErrShortWrite` via `errors.Is`; the manual loop does too; `iotest.TruncateWriter` masks the short write (no error) so only the delivered-byte count reveals the loss; an under-limit payload writes fully.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shortwrite/cmd/demo
cd ~/go-exercises/shortwrite
go mod init example.com/shortwrite
```

### An honest short write vs. a lying one

There are two kinds of truncating sink and the difference is the whole lesson. An
**honest** bounded writer accepts what it can and returns the real count: given a
16-byte write with 8 bytes of room, it writes 8 and returns `(8, nil)`. That short
count is the signal. `io.Copy` (and `strings.Reader.WriteTo`, and any correct
manual loop) compares the returned `nw` to the offered `len(p)`; on `nw < len(p)`
with a nil error it returns `io.ErrShortWrite`. So detection is automatic *if the
sink is honest about how much it took*.

`iotest.TruncateWriter(w, n)` is the **lying** sink, and deliberately so: it writes
the first `n` bytes to `w` and then reports `len(p)` written — it claims a full
write while silently dropping the rest. This models the nastiest real bug, a writer
that swallows bytes and returns success, and it demonstrates why trusting the
returned count is not enough. Against `TruncateWriter`, both `io.Copy` and a manual
`nw < len(p)` check see a "full" write and report no error, yet the destination
holds only `n` bytes. The only way to catch it is to independently compare the bytes
that actually reached the sink against what you sent.

`CopyChecked` implements the correct manual discipline: read a chunk, write it,
check `nw < nr`, and surface `io.ErrShortWrite`. It catches the honest bounded
writer. Against `TruncateWriter` it cannot — nothing that trusts the return value
can — which is exactly the point the test makes by asserting the delivered-byte
count, not the error.

Create `copy.go`:

```go
package shortwrite

import (
	"bytes"
	"io"
)

// boundedWriter accepts at most cap total bytes into an underlying buffer and
// HONESTLY returns the short count once full, so a copy loop can detect the short
// write. This is how a correct rate-capped sink behaves.
type boundedWriter struct {
	buf *bytes.Buffer
	cap int
}

// NewBoundedWriter returns a writer that accepts at most cap bytes into buf,
// reporting the real (possibly short) count on each Write.
func NewBoundedWriter(buf *bytes.Buffer, cap int) io.Writer {
	return &boundedWriter{buf: buf, cap: cap}
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	room := b.cap - b.buf.Len()
	if room <= 0 {
		return 0, nil // full: took none of len(p) -> short write
	}
	if len(p) > room {
		return b.buf.Write(p[:room]) // took room < len(p) -> short write
	}
	return b.buf.Write(p)
}

// CopyChecked streams src into dst and returns io.ErrShortWrite if any write
// accepted fewer bytes than offered. It cannot detect a sink that LIES about the
// count (see iotest.TruncateWriter); only an independent byte check can.
func CopyChecked(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			total += int64(nw)
			if ew != nil {
				return total, ew
			}
			if nw < nr {
				return total, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return total, nil
			}
			return total, er
		}
	}
}
```

### The runnable demo

The demo copies a 16-byte payload to an honest 8-byte sink (surfacing
`io.ErrShortWrite`) and to an 8-byte `iotest.TruncateWriter` (which masks the loss:
no error, but only 8 bytes delivered).

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing/iotest"

	"example.com/shortwrite"
)

func main() {
	const capacity = 8
	payload := "0123456789ABCDEF" // 16 bytes

	var sink bytes.Buffer
	bw := shortwrite.NewBoundedWriter(&sink, capacity)
	n, err := io.Copy(bw, strings.NewReader(payload))
	fmt.Printf("bounded: wrote %d, sink has %d, err=%v\n", n, sink.Len(), err)

	var masked bytes.Buffer
	tw := iotest.TruncateWriter(&masked, capacity)
	n, err = io.Copy(tw, strings.NewReader(payload))
	fmt.Printf("truncate: wrote %d, masked has %d, err=%v\n", n, masked.Len(), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bounded: wrote 8, sink has 8, err=short write
truncate: wrote 16, masked has 8, err=<nil>
```

### Tests

`TestIoCopyDetectsShortWrite` copies an over-cap payload to the honest bounded sink
and asserts `io.Copy` returns `io.ErrShortWrite` via `errors.Is` with exactly `cap`
bytes delivered. `TestCopyCheckedDetectsShortWrite` proves the manual loop catches
the same. `TestTruncateWriterMasksShortWrite` shows the trap: `CopyChecked` returns
no error against `iotest.TruncateWriter`, yet only `cap` bytes reached the sink — so
the delivered count, not the error, is what exposes the loss.
`TestUnderLimitWritesFully` pins that an under-cap payload writes completely.

Create `copy_test.go`:

```go
package shortwrite

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const capacity = 8

func TestIoCopyDetectsShortWrite(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	bw := NewBoundedWriter(&sink, capacity)

	_, err := io.Copy(bw, strings.NewReader("payload-that-is-too-long"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
	if sink.Len() != capacity {
		t.Fatalf("sink has %d bytes, want %d", sink.Len(), capacity)
	}
}

func TestCopyCheckedDetectsShortWrite(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	bw := NewBoundedWriter(&sink, capacity)

	_, err := CopyChecked(bw, bytes.NewReader([]byte("payload-that-is-too-long")))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
	if sink.Len() != capacity {
		t.Fatalf("sink has %d bytes, want %d", sink.Len(), capacity)
	}
}

func TestTruncateWriterMasksShortWrite(t *testing.T) {
	t.Parallel()
	var masked bytes.Buffer
	tw := iotest.TruncateWriter(&masked, capacity)

	// TruncateWriter reports a full write, so a count-trusting loop sees no error.
	_, err := CopyChecked(tw, bytes.NewReader([]byte("payload-that-is-too-long")))
	if err != nil {
		t.Fatalf("err = %v, want nil (TruncateWriter masks the short write)", err)
	}
	// Only the delivered-byte count reveals the silent loss.
	if masked.Len() != capacity {
		t.Fatalf("masked has %d bytes, want %d (silently dropped the rest)", masked.Len(), capacity)
	}
}

func TestUnderLimitWritesFully(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	bw := NewBoundedWriter(&sink, capacity)

	const payload = "short" // 5 < cap
	n, err := io.Copy(bw, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != int64(len(payload)) || sink.String() != payload {
		t.Fatalf("wrote %d %q, want %d %q", n, sink.String(), len(payload), payload)
	}
}
```

## Review

The copy is correct when it treats a short return count as `io.ErrShortWrite` rather
than assuming a full write — the mirror of processing `n` bytes before `err` on the
read side. The honest bounded sink proves the automatic detection path: `io.Copy`
and the manual loop both surface `io.ErrShortWrite`. The `iotest.TruncateWriter` case
is the sharp edge: it reports success while dropping bytes, so no count-trusting
logic can catch it, and the test asserts the delivered-byte count instead. The
production lesson is that a sink you do not control can lie, and integrity-critical
transfers need an independent check (a byte count, a checksum, a content-length
compare), not just the writer's word. Run `go test -race` to confirm.

## Resources

- [`io.ErrShortWrite`](https://pkg.go.dev/io#pkg-variables) — the short-write signal.
- [`io.Copy`](https://pkg.go.dev/io#Copy) — how the stdlib copy detects a short write.
- [`testing/iotest#TruncateWriter`](https://pkg.go.dev/testing/iotest#TruncateWriter) — a sink that truncates but reports a full write.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-composite-multireader-preamble.md](08-composite-multireader-preamble.md) | Next: [../11-testing-filesystems-with-fstest/00-concepts.md](../11-testing-filesystems-with-fstest/00-concepts.md)
