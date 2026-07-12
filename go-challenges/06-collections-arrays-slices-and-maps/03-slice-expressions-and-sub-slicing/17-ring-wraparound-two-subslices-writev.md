# Exercise 17: Expose a Wrapped Ring Buffer as Two Sub-Slices, Not a Copy

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A fixed-capacity ring buffer's readable bytes usually sit in one contiguous
run -- but the moment the write cursor wraps past the end of the backing
array while the read cursor has not caught up, the readable region is split
into two pieces: a run at the tail of the array and a run at its front. The
tempting fix is to `copy` both pieces into one fresh contiguous slice before
handing them to a caller, but that is an allocation and a memcpy on every
single wrapped read, on what is often the hottest path in a proxy or a log
shipper. The alternative this module builds is the one `net.Buffers` and the
POSIX `writev` syscall are built around: expose the two pieces as two
separate sub-slices and let the writer -- or the kernel -- consume them in
order without ever materializing a merged buffer.

This module builds `ringbuf`, a package with one type, `Buffer`, whose
`Readable` method returns the wrapped region as two sub-slices and whose
`WriteTo` hands both straight to `net.Buffers` for a single vectored write.
The version that copies both pieces into one buffer never appears in the
package -- it exists only in the test file, as the thing an allocation test
proves costs strictly more.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ringbuf/                       module example.com/ringbuf
  go.mod                       go 1.24
  ringbuf.go                   ErrFull; Buffer with Write, Readable, Advance, WriteTo
  ringbuf_test.go              table over no-wrap/wrap/empty/full; view-not-copy proof; allocating-baseline contrast; ExampleBuffer_WriteTo
```

- Files: `ringbuf.go`, `ringbuf_test.go`.
- Implement: a fixed-capacity `Buffer` tracking `head` and `size` over a `[]byte` backing array; `Write(p []byte) (int, error)` returning `ErrFull` if `p` does not fit the remaining free space; `Readable() (head, tail []byte)` returning the readable region as one or two sub-slices of the backing array depending on whether it wraps; `Advance(n int)` consuming bytes from the front; `WriteTo(w io.Writer) (int64, error)` implementing `io.WriterTo` by handing both sub-slices to a single `net.Buffers.WriteTo` call.
- Test: a table over no-wrap, an empty buffer, a full buffer that does not wrap, and a buffer forced to wrap after an `Advance` frees room at the front; a test proving `Readable`'s two slices are views (a mutation through `head` is visible on the next call), not copies; a table over `WriteTo`'s no-wrap, wrapped, and empty cases, each checking byte order and that the buffer drains to length zero; a contrast against an unexported copy-into-one-buffer baseline showing it produces the identical bytes at strictly higher allocation cost; `ExampleBuffer_WriteTo` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why two sub-slices beat one copy, and how the wrap actually happens

`Buffer` stores `head` (the read cursor) and `size` (how many readable bytes
are live) over a fixed `[]byte`. The readable region logically runs from
`head` for `size` bytes, wrapping around the end of the array if it has to.
`Readable` computes `end := head + size`: if `end <= len(data)`, the region
fits in one run and `data[head:end]` is returned with a nil second slice. If
`end` overshoots the array, the region is split -- `data[head:]` for the
part that reaches the physical end, `data[:end-len(data)]` for the part that
wrapped to the front -- and both are returned as plain two-index sub-slices
of the *same* backing array `data`. No bytes move. This is the direct
sibling of every other exercise in this lesson: a sub-slice is a view, and
here that view discipline is what makes a ring buffer's O(1) write and O(1)
expose possible in the first place. Copying the wrapped region into one
buffer, by contrast, is an O(n) operation on every wrapped read -- exactly
the cost this design exists to avoid.

The wrap itself is worth tracing through once concretely, because it is easy
to picture wrongly. Start with a 6-byte buffer, write `ABCDEF` (it fills
completely: `head=0`, `size=6`). Call `Advance(4)`, consuming `ABCD`: now
`head=4`, `size=2`, and the two readable bytes are `EF`, sitting at the
*tail* of the backing array. Write `XY`: the next write position is
`(head+size) % len(data) = (4+2) % 6 = 0`, so `X` and `Y` land at the
*front* of the array, in the four slots that `Advance` just freed. The
readable region is now `EF` (indices 4-5) followed by `XY` (indices 0-1) --
two runs, in write order, physically discontiguous. `Readable` returns
exactly `("EF", "XY")`.

`WriteTo` is where this pays off end to end. Rather than reasoning about the
wrap itself, it just asks `Readable` for both pieces, hands whichever ones
are non-empty to a `net.Buffers`, and calls `net.Buffers.WriteTo`. On a
plain `io.Writer`, that issues one `Write` call per piece, in order --
already better than a copy, since it moves the bytes exactly once, straight
from the ring into the writer. On a `*net.TCPConn`, `net.Buffers` goes
further and issues a single vectored `writev(2)` syscall covering both
pieces at once, the same trick this module's name borrows: the kernel
gathers the two discontiguous buffers into one socket write without the
process ever assembling them into one contiguous slice.

Create `ringbuf.go`:

```go
// Package ringbuf implements a fixed-capacity byte ring buffer whose
// readable region, when it wraps past the end of the backing array, is
// exposed as two sub-slices rather than copied into one contiguous buffer --
// the same writev-style pattern net.Buffers and the POSIX writev(2) syscall
// are built around.
package ringbuf

import (
	"errors"
	"io"
	"net"
)

// ErrFull is returned by Write when p does not fit in the buffer's
// remaining free space. Buffer has fixed capacity; it never grows.
var ErrFull = errors.New("ringbuf: buffer full")

// Buffer is a fixed-capacity byte ring buffer. Bytes are appended at the
// tail by Write and consumed from the head by Advance. Its defining
// property is what Readable returns when the readable region wraps past the
// end of the backing array: two sub-slices, never a copy into one
// contiguous buffer.
//
// A Buffer is not safe for concurrent use: Write, Readable, Advance, and
// WriteTo all read or mutate head and size without synchronization, so the
// caller must serialize access (a mutex, or confining a Buffer to one
// goroutine) if it is shared.
type Buffer struct {
	data []byte
	head int // index of the first readable byte
	size int // number of readable bytes currently stored
}

// New allocates a Buffer with the given fixed capacity.
func New(capacity int) *Buffer {
	return &Buffer{data: make([]byte, capacity)}
}

// Len reports how many readable bytes are currently stored.
func (b *Buffer) Len() int { return b.size }

// Cap reports the buffer's fixed capacity.
func (b *Buffer) Cap() int { return len(b.data) }

// Write appends p to the buffer. It fails with ErrFull, writing nothing, if
// p does not fit in the remaining free space -- this ring never reallocates
// to grow.
func (b *Buffer) Write(p []byte) (int, error) {
	free := len(b.data) - b.size
	if len(p) > free {
		return 0, ErrFull
	}
	writePos := (b.head + b.size) % len(b.data)
	n := copy(b.data[writePos:], p)
	if n < len(p) {
		copy(b.data, p[n:])
	}
	b.size += len(p)
	return len(p), nil
}

// Readable returns the current readable region as two sub-slices: head is
// the contiguous run starting at the read cursor, and tail is the
// wrapped-around remainder starting at index 0 of the backing array. When
// the readable region does not wrap past the end of the backing array, tail
// is nil. Neither slice copies data -- both are views into Buffer's own
// backing array, valid only until the next Write or Advance call, the same
// short-lived-view discipline as a pooled scratch buffer.
func (b *Buffer) Readable() (head, tail []byte) {
	if b.size == 0 {
		return nil, nil
	}
	end := b.head + b.size
	if end <= len(b.data) {
		return b.data[b.head:end], nil
	}
	return b.data[b.head:], b.data[:end-len(b.data)]
}

// Advance consumes up to n bytes from the front of the readable region. If
// n exceeds the number of readable bytes, it consumes all of them.
func (b *Buffer) Advance(n int) {
	if n > b.size {
		n = b.size
	}
	b.head = (b.head + n) % len(b.data)
	b.size -= n
}

// WriteTo implements io.WriterTo: it writes the entire readable region to w
// and advances past whatever was written. When the region wraps, it hands
// both sub-slices to w in one net.Buffers.WriteTo call -- the writev-style
// pattern that lets the kernel gather two discontiguous buffers into a
// single write on a *net.TCPConn, instead of first copying the wrapped
// region into one contiguous buffer just to satisfy a plain io.Writer.
func (b *Buffer) WriteTo(w io.Writer) (int64, error) {
	head, tail := b.Readable()
	var bufs net.Buffers
	if len(head) > 0 {
		bufs = append(bufs, head)
	}
	if len(tail) > 0 {
		bufs = append(bufs, tail)
	}
	if len(bufs) == 0 {
		return 0, nil
	}
	n, err := bufs.WriteTo(w)
	b.Advance(int(n))
	return n, err
}
```

### Using it

`Buffer` is a self-contained value: `New(capacity)` allocates it once, and
`Write`/`Readable`/`Advance`/`WriteTo` are the whole surface a caller needs
for the lifetime of a connection or a log stream. The aliasing contract
lives on `Readable`'s doc comment and matters more here than almost anywhere
else in this lesson: the two slices it returns are valid only until the next
`Write` or `Advance`, because both of those can move `head` or overwrite the
very bytes `head` and `tail` point at. A caller that needs to hold onto a
chunk of readable data past its next call into the buffer must copy it first
(`bytes.Clone`), the same short-lived-view discipline `bufio.Scanner.Bytes()`
documents for its own result. Because `Buffer` mutates unsynchronized state
on every method, it is not safe for concurrent use, and a caller sharing one
across goroutines -- a single connection buffer read by one goroutine and
written by another -- must add its own mutex or otherwise serialize access.

The module has no `main.go`, because a ring buffer is a library component,
not a tool. Its executable demonstration is `ExampleBuffer_WriteTo`: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift away from the code.

```go
func ExampleBuffer_WriteTo() {
	b := New(6)

	mustWriteExample(b, "ABCDEF") // fill it completely
	head, tail := b.Readable()
	fmt.Printf("after filling: head=%q tail=%q\n", head, tail)

	b.Advance(4) // consume ABCD, freeing 4 slots at the front
	head, tail = b.Readable()
	fmt.Printf("after advancing 4: head=%q tail=%q\n", head, tail)

	mustWriteExample(b, "XY") // wraps: lands at the freed front of the array
	head, tail = b.Readable()
	fmt.Printf("after writing XY (wraps): head=%q tail=%q\n", head, tail)

	var out bytes.Buffer
	n, err := b.WriteTo(&out)
	if err != nil {
		panic(err)
	}
	fmt.Printf("WriteTo wrote %d bytes in order: %q\n", n, out.String())
	fmt.Printf("buffer len after WriteTo: %d\n", b.Len())

	// Output:
	// after filling: head="ABCDEF" tail=""
	// after advancing 4: head="EF" tail=""
	// after writing XY (wraps): head="EF" tail="XY"
	// WriteTo wrote 4 bytes in order: "EFXY"
	// buffer len after WriteTo: 0
}
```

`head` and `tail` land exactly where the arithmetic in the previous section
predicts, and `WriteTo` writes them out in the right order -- `EF` before
`XY`, even though `XY` physically sits earlier in the backing array -- and
drains the buffer to empty in a single call.

### Tests

`TestReadable` is the required table: an ordinary no-wrap region, an empty
buffer (both slices nil), a full buffer that happens not to wrap, and the
wrap case built with the exact write/advance/write sequence traced through
above. `TestReadableIsAView` is the test that catches a well-intentioned but
wrong rewrite of `Readable` that copies its result instead of slicing --
mutating through the returned `head` slice must be visible on the next call.
`TestWriteRejectsOverflow` pins the fixed-capacity contract. `TestWriteTo` is
a table over the no-wrap, wrapped, and empty cases, each checking both the
exact byte order written and that the buffer's length reaches zero
afterward. `readableCopied` is the antipattern this module warns against: it
merges the wrapped region into one scratch buffer before returning it,
which looks like a reasonable simplification and returns byte-identical
content -- `TestCopiedMatchesViewsButAllocates` checks that agreement
explicitly -- but allocates and copies on every call, where `Readable`
allocates none, which the same test proves with `testing.AllocsPerRun`
rather than by argument. It does not call `t.Parallel`, because
`AllocsPerRun` panics if run from a parallel subtest.

Create `ringbuf_test.go`:

```go
package ringbuf

import (
	"bytes"
	"fmt"
	"testing"
)

func TestReadable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		capacity           int
		setup              func(t *testing.T, b *Buffer)
		wantHead, wantTail string
	}{
		{
			name:     "no wrap: one contiguous region, capacity to spare",
			capacity: 8,
			setup:    func(t *testing.T, b *Buffer) { mustWrite(t, b, "abcd") },
			wantHead: "abcd",
		},
		{
			name:     "empty buffer reports two nil regions",
			capacity: 8,
			setup:    func(t *testing.T, b *Buffer) {},
		},
		{
			name:     "full, no wrap: readable region spans the whole capacity",
			capacity: 4,
			setup:    func(t *testing.T, b *Buffer) { mustWrite(t, b, "abcd") },
			wantHead: "abcd",
		},
		{
			name:     "wrap: readable region splits across the end of the backing array",
			capacity: 6,
			setup: func(t *testing.T, b *Buffer) {
				mustWrite(t, b, "ABCDEF") // fills the ring: head=0 size=6
				b.Advance(4)              // consumes ABCD: head=4 size=2, readable "EF"
				mustWrite(t, b, "XY")     // writePos=(4+2)%6=0: X,Y land at data[0:2]
			},
			wantHead: "EF",
			wantTail: "XY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := New(tc.capacity)
			tc.setup(t, b)

			head, tail := b.Readable()
			if got := string(head); got != tc.wantHead {
				t.Fatalf("head = %q, want %q", got, tc.wantHead)
			}
			if got := string(tail); got != tc.wantTail {
				t.Fatalf("tail = %q, want %q", got, tc.wantTail)
			}
		})
	}
}

// TestReadableIsAView proves Readable hands out sub-slices of Buffer's own
// backing array, not copies: mutating through the returned head slice must
// be visible on the next call to Readable.
func TestReadableIsAView(t *testing.T) {
	t.Parallel()

	b := New(6)
	mustWrite(t, b, "ABCDEF")
	b.Advance(4)
	mustWrite(t, b, "XY")

	head, _ := b.Readable()
	head[0] = 'e'

	head2, _ := b.Readable()
	if head2[0] != 'e' {
		t.Fatalf("mutation through head was not visible: Readable() head = %q", head2)
	}
}

func TestWriteRejectsOverflow(t *testing.T) {
	t.Parallel()

	b := New(4)
	if _, err := b.Write([]byte("abcd")); err != nil {
		t.Fatalf("Write(4 bytes into cap 4) = %v, want nil", err)
	}
	if _, err := b.Write([]byte("x")); err != ErrFull {
		t.Fatalf("Write into full buffer = %v, want ErrFull", err)
	}
}

func TestWriteTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		setup    func(t *testing.T, b *Buffer)
		wantN    int64
		wantOut  string
	}{
		{
			name:     "no wrap",
			capacity: 8,
			setup:    func(t *testing.T, b *Buffer) { mustWrite(t, b, "hello") },
			wantN:    5,
			wantOut:  "hello",
		},
		{
			name:     "wrapped",
			capacity: 6,
			setup: func(t *testing.T, b *Buffer) {
				mustWrite(t, b, "ABCDEF")
				b.Advance(4)
				mustWrite(t, b, "XY") // readable is now "EF" then wrapped "XY"
			},
			wantN:   4,
			wantOut: "EFXY",
		},
		{
			name:     "empty",
			capacity: 4,
			setup:    func(t *testing.T, b *Buffer) {},
			wantN:    0,
			wantOut:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := New(tc.capacity)
			tc.setup(t, b)

			var out bytes.Buffer
			n, err := b.WriteTo(&out)
			if err != nil {
				t.Fatalf("WriteTo() error = %v", err)
			}
			if n != tc.wantN {
				t.Fatalf("WriteTo() n = %d, want %d", n, tc.wantN)
			}
			if out.String() != tc.wantOut {
				t.Fatalf("WriteTo() wrote %q, want %q", out.String(), tc.wantOut)
			}
			if b.Len() != 0 {
				t.Fatalf("Len() after WriteTo = %d, want 0", b.Len())
			}
		})
	}
}

// readableCopied is the antipattern this module warns against: a tempting
// rewrite of Readable that merges the wrapped region into one contiguous
// scratch buffer before returning it, so callers only ever see a single
// []byte. It returns the identical bytes in the identical order -- every
// content-based assertion in TestReadable would still pass against it -- but
// it allocates and copies on every call, exactly the O(n) cost per wrapped
// read this module's two-sub-slice design exists to avoid. Never exported,
// never reachable from Buffer; it exists only so a test can measure that
// cost directly.
func readableCopied(b *Buffer) []byte {
	head, tail := b.Readable()
	out := make([]byte, 0, len(head)+len(tail))
	out = append(out, head...)
	out = append(out, tail...)
	return out
}

// TestCopiedMatchesViewsButAllocates is the heart of the module: the
// antipattern produces the same bytes as the two-sub-slice Readable, while
// allocating on every call where Readable allocates none. It does not call
// t.Parallel: testing.AllocsPerRun panics if run from a parallel subtest.
func TestCopiedMatchesViewsButAllocates(t *testing.T) {
	b := New(6)
	mustWrite(t, b, "ABCDEF")
	b.Advance(4)
	mustWrite(t, b, "XY") // readable wraps: "EF" then "XY"

	head, tail := b.Readable()
	want := append(append([]byte{}, head...), tail...)
	if got := readableCopied(b); !bytes.Equal(got, want) {
		t.Fatalf("readableCopied(b) = %q, want %q", got, want)
	}

	var sinkHead, sinkTail, sinkCopy []byte
	viewAllocs := testing.AllocsPerRun(100, func() {
		sinkHead, sinkTail = b.Readable()
	})
	copiedAllocs := testing.AllocsPerRun(100, func() {
		sinkCopy = readableCopied(b)
	})
	_, _, _ = sinkHead, sinkTail, sinkCopy
	if viewAllocs != 0 {
		t.Fatalf("Readable: got %v allocations per run, want 0", viewAllocs)
	}
	if copiedAllocs == 0 {
		t.Fatalf("readableCopied: got 0 allocations per run, want at least 1")
	}
}

func mustWrite(t *testing.T, b *Buffer, s string) {
	t.Helper()
	if _, err := b.Write([]byte(s)); err != nil {
		t.Fatalf("Write(%q) = %v, want nil", s, err)
	}
}

// ExampleBuffer_WriteTo is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below. It fills a 6-byte buffer, drains 4 bytes, writes 2 more (forcing a
// wrap into the freed front slots), and drains the wrapped region through
// WriteTo in a single call.
func ExampleBuffer_WriteTo() {
	b := New(6)

	mustWriteExample(b, "ABCDEF") // fill it completely
	head, tail := b.Readable()
	fmt.Printf("after filling: head=%q tail=%q\n", head, tail)

	b.Advance(4) // consume ABCD, freeing 4 slots at the front
	head, tail = b.Readable()
	fmt.Printf("after advancing 4: head=%q tail=%q\n", head, tail)

	mustWriteExample(b, "XY") // wraps: lands at the freed front of the array
	head, tail = b.Readable()
	fmt.Printf("after writing XY (wraps): head=%q tail=%q\n", head, tail)

	var out bytes.Buffer
	n, err := b.WriteTo(&out)
	if err != nil {
		panic(err)
	}
	fmt.Printf("WriteTo wrote %d bytes in order: %q\n", n, out.String())
	fmt.Printf("buffer len after WriteTo: %d\n", b.Len())

	// Output:
	// after filling: head="ABCDEF" tail=""
	// after advancing 4: head="EF" tail=""
	// after writing XY (wraps): head="EF" tail="XY"
	// WriteTo wrote 4 bytes in order: "EFXY"
	// buffer len after WriteTo: 0
}

func mustWriteExample(b *Buffer, s string) {
	if _, err := b.Write([]byte(s)); err != nil {
		panic(err)
	}
}
```

## Review

`Buffer` is correct when `Readable` reports exactly the bytes written and
not yet advanced past, in the right order, whether or not they wrap -- and
when that reporting never costs a copy. The wrap test and the view test are
two different claims: the wrap test says the *content* is right, the view
test says the *mechanism* is right (a real sub-slice, not a materialized
copy that happens to contain the right bytes). The antipattern this module
is built to catch is the tempting shortcut of writing `Readable` so it
internally does `copy(scratch, head); copy(scratch[len(head):], tail);
return scratch, nil` -- it would pass every content-based test in this file
while quietly reintroducing the O(n) copy and allocation this whole design
exists to avoid, and only `TestCopiedMatchesViewsButAllocates`'s direct
`AllocsPerRun` measurement catches it; that helper is never exported and
never reachable from `Buffer`, exactly because production code should never
be able to reach for it by accident. `Buffer` is not safe for concurrent
use, so a caller sharing one across goroutines owns the synchronization. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the two-index sub-slices `Readable` returns for each half of the wrapped region.
- [`net.Buffers`](https://pkg.go.dev/net#Buffers) — the writev-style vectored write this module's `WriteTo` is built on.
- [`io.WriterTo`](https://pkg.go.dev/io#WriterTo) — the interface `Buffer.WriteTo` implements.
- [`writev(2)` man page](https://man7.org/linux/man-pages/man2/writev.2.html) — the underlying syscall `net.Buffers` uses on a `*net.TCPConn` to write discontiguous buffers in one kernel call.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-rotate-segments-three-reversals.md](16-rotate-segments-three-reversals.md) | Next: [18-three-index-plugin-api-guard.md](18-three-index-plugin-api-guard.md)
