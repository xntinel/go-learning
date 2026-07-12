# Exercise 8: Hand a Framing Consumer a Bounded Window It Cannot Grow Into

A protocol framer parses length-prefixed messages out of one shared read buffer
and hands each payload to a consumer as a slice. If that slice carries spare
capacity — because it is a plain two-index sub-slice — a consumer that `append`s to
its frame silently overwrites the *next* frame still living in the buffer. The
three-index (full) slice expression bounds the capacity to the length, so the
append reallocates instead. This module builds both and reproduces the corruption.

## What you'll build

```text
framing/                     independent module: example.com/framing
  go.mod                     go 1.24
  framing.go                 type Framer; NewFramer, Next (three-index), NextUnsafe (two-index)
  cmd/
    demo/
      main.go                parse two frames, append to the first, show the second survives
  framing_test.go            three-index-prevents, two-index-corrupts, still-aliases; -race
```

Files: `framing.go`, `cmd/demo/main.go`, `framing_test.go`.
Implement: `Next` returning `buf[start:end:end]` (cap == len) and `NextUnsafe` returning `buf[start:end]`.
Test: parse two adjacent frames, `append` to frame 1, and assert frame 2 is intact with `Next` but corrupted with `NextUnsafe`; assert `cap == len` for the safe path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## The full slice expression bounds write-through

Each frame is a 4-byte big-endian length prefix followed by that many payload
bytes, packed back to back in one buffer. The framer walks the buffer, and for
each frame it returns the payload as a slice into the buffer — no copy, because the
whole point of framing is to avoid copying every message.

The hazard is capacity. A plain `buf[start:end]` inherits the buffer's capacity
from `start` all the way to the end of the buffer. So the payload slice's `cap` is
far larger than its `len`, and the spare capacity *is the rest of the buffer* —
including every following frame. When the consumer does `frame = append(frame, b)`,
`append` sees spare capacity and writes in place, straight over the next frame's
bytes. Nothing errors; the next frame is just quietly corrupted. This is one of
the nastiest slice bugs precisely because the producer and consumer look correct in
isolation.

`Next` returns `buf[start:end:end]`. The third index sets the capacity to
`end - start`, so `cap == len`: there is no spare capacity, and the consumer's
first `append` must allocate a new array and copy. The consumer can grow its frame
freely without touching the buffer. `NextUnsafe` returns the two-index form and
exists only to reproduce the corruption.

Be precise about what three-index does and does not do. It bounds *write-through* —
the consumer cannot grow into your data. It does **not** break the *read pin*: the
returned frame still points into the buffer and keeps it alive until the consumer
copies. Three-index prevents corruption; only a copy (`slices.Clone`) prevents the
pin. The `still-aliases` test makes this explicit by mutating the buffer and
watching the frame change.

Create `framing.go`:

```go
// Package framing parses length-prefixed frames from a shared buffer, handing each
// payload out as a capacity-bounded window the consumer cannot grow into.
package framing

import "encoding/binary"

// Framer walks length-prefixed frames in a single shared buffer.
type Framer struct {
	buf []byte
	pos int
}

// NewFramer returns a Framer over buf.
func NewFramer(buf []byte) *Framer { return &Framer{buf: buf} }

// Next returns the next frame payload as buf[start:end:end]. Because cap == len,
// a consumer's append reallocates instead of overwriting the following frame.
func (f *Framer) Next() ([]byte, bool) {
	start, end, ok := f.advance()
	if !ok {
		return nil, false
	}
	return f.buf[start:end:end], true
}

// NextUnsafe returns buf[start:end]. Its capacity runs to the end of the buffer,
// so a consumer's append writes over later frames. Kept only to contrast.
func (f *Framer) NextUnsafe() ([]byte, bool) {
	start, end, ok := f.advance()
	if !ok {
		return nil, false
	}
	return f.buf[start:end], true
}

// advance decodes the next length prefix and returns the payload bounds.
func (f *Framer) advance() (start, end int, ok bool) {
	if f.pos+4 > len(f.buf) {
		return 0, 0, false
	}
	n := int(binary.BigEndian.Uint32(f.buf[f.pos:]))
	start = f.pos + 4
	end = start + n
	if n < 0 || end > len(f.buf) {
		return 0, 0, false
	}
	f.pos = end
	return start, end, true
}

// Frame encodes a length-prefixed frame for payload, appended to dst.
func Frame(dst, payload []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}
```

## The runnable demo

The demo packs two frames into one buffer, reads the first, appends to it, and
shows the second frame is untouched — because the three-index window forced the
append to reallocate.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/framing"
)

func main() {
	var buf []byte
	buf = framing.Frame(buf, []byte("one"))
	buf = framing.Frame(buf, []byte("two"))

	fr := framing.NewFramer(buf)
	f1, _ := fr.Next()
	fmt.Printf("frame 1: %q (len %d, cap %d)\n", f1, len(f1), cap(f1))

	// Append to frame 1: with cap == len this reallocates and cannot touch buf.
	grown := append(f1, '!')
	fmt.Printf("grown:   %q\n", grown)

	f2, _ := fr.Next()
	fmt.Printf("frame 2: %q\n", f2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frame 1: "one" (len 3, cap 3)
grown:   "one!"
frame 2: "two"
```

## Tests

The corruption test packs two frames, appends to frame 1, and reads frame 2. With
`Next` (three-index) the append reallocates, so frame 2 reads back intact; with
`NextUnsafe` (two-index) the append overwrites frame 2's length prefix, so the
second read fails or returns the wrong bytes. The aliasing test confirms the
three-index frame still views the buffer (write-through is bounded, the read pin is
not).

Create `framing_test.go`:

```go
package framing

import (
	"testing"
)

func twoFrames() []byte {
	var buf []byte
	buf = Frame(buf, []byte("one"))
	buf = Frame(buf, []byte("two"))
	return buf
}

func TestThreeIndexPreventsCorruption(t *testing.T) {
	t.Parallel()

	fr := NewFramer(twoFrames())

	f1, ok := fr.Next()
	if !ok || string(f1) != "one" {
		t.Fatalf("frame 1 = %q, %v; want one, true", f1, ok)
	}
	if cap(f1) != len(f1) {
		t.Fatalf("three-index frame cap %d != len %d", cap(f1), len(f1))
	}

	_ = append(f1, '!') // reallocates; must not touch the buffer

	f2, ok := fr.Next()
	if !ok || string(f2) != "two" {
		t.Fatalf("frame 2 = %q, %v; want two, true (append corrupted it)", f2, ok)
	}
}

func TestTwoIndexCorrupts(t *testing.T) {
	t.Parallel()

	fr := NewFramer(twoFrames())

	f1, ok := fr.NextUnsafe()
	if !ok || string(f1) != "one" {
		t.Fatalf("frame 1 = %q, %v; want one, true", f1, ok)
	}
	if cap(f1) == len(f1) {
		t.Fatal("two-index frame should carry spare capacity into the next frame")
	}

	_ = append(f1, '!') // writes over frame 2's length prefix in the shared buffer

	f2, ok := fr.NextUnsafe()
	if ok && string(f2) == "two" {
		t.Fatal("frame 2 survived the append; two-index should have corrupted it")
	}
}

func TestThreeIndexStillAliasesBuffer(t *testing.T) {
	t.Parallel()

	buf := twoFrames()
	fr := NewFramer(buf)
	f1, _ := fr.Next()

	buf[4] = 'O' // mutate the buffer under frame 1's payload

	if string(f1) != "One" {
		t.Fatalf("frame = %q; three-index still aliases the buffer, so it should reflect the write", f1)
	}
}
```

## Review

The framer is correct when a three-index frame reports `cap == len`, survives a
consumer's `append` without corrupting the following frame, and still reflects
writes to the underlying buffer (the read pin is intact). `TestTwoIndexCorrupts`
proves the failure the full slice expression prevents: a two-index frame carries
the rest of the buffer as spare capacity, so appending to it overwrites the next
frame. The mistake this module exists to prevent is handing a framing consumer a
plain `buf[start:end]` window; use `buf[start:end:end]` so its appends reallocate.
Remember three-index bounds only write-through — to also drop the pin on the
buffer, the consumer must `slices.Clone`. Run `go test -race` to confirm framing is
safe under concurrent consumers of independent buffers.

## Resources

- [Go spec: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the full (three-index) slice expression `a[low:high:max]`.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary#ByteOrder) — `BigEndian.Uint32` / `PutUint32` for length prefixes.
- [Go blog: Arrays, slices (and strings): the mechanics of 'append'](https://go.dev/blog/slices) — how `append` uses spare capacity in place.

---

Back to [07-rate-limiter-map-rebuild-reclaim.md](07-rate-limiter-map-rebuild-reclaim.md) | Next: [09-weak-canonicalization-cache.md](09-weak-canonicalization-cache.md)
