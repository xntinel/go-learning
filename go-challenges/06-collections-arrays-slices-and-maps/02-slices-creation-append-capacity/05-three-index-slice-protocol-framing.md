# Exercise 5: Bounded Sub-Slices for Wire-Protocol Framing (Three-Index Expression)

A framing parser carves a single read buffer into per-frame payload views without
copying — the fast path in any binary protocol codec. The danger is that a
downstream handler which `append`s into a payload view can, with an ordinary
two-index slice, spill past the frame's end and clobber the next frame's bytes in
the shared buffer. The three-index expression `s[start:end:end]` bounds each
view's capacity to its length so that first over-length append is forced to copy
instead of overwriting a neighbor.

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
framing/                   independent module: example.com/framing
  go.mod                   go 1.26
  framing.go               Parse (bounded), ParseUnbounded (buggy), ErrTruncated
  cmd/
    demo/
      main.go              parse frames, append to one payload, show neighbor safety
  framing_test.go          bounded-vs-unbounded corruption, cap==len assertion, Example
```

Files: `framing.go`, `cmd/demo/main.go`, `framing_test.go`.
Implement: `Parse(buf) ([][]byte, error)` handing each payload as `buf[start:end:end]`, and a buggy `ParseUnbounded` using `buf[start:end]`.
Test: append to a returned payload and assert adjacent frames are untouched with the bounded parser but corrupted with the unbounded one; assert `cap(payload) == len(payload)`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/05-three-index-slice-protocol-framing/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/05-three-index-slice-protocol-framing
go mod edit -go=1.26
```

### Why the third index is the safe hand-off

The wire format here is the simplest length-prefixed framing: one byte of length
`n`, then `n` payload bytes, repeated. The parser walks the buffer and, for each
frame, produces a slice over the payload region. The tempting way to write that is
`buf[start:end]` — a two-index slice. Its length is `end-start` (correct), but its
*capacity* runs from `start` all the way to the end of `buf`. So the spare
capacity of frame 0's payload overlaps every following frame's bytes. If a handler
does `payload = append(payload, x)`, and `len < cap` (which it is, because there
are more frames after it), the append writes `x` in place — directly onto the next
frame's bytes in the shared buffer. The neighbor is silently corrupted.

`buf[start:end:end]` — the three-index form — sets capacity to `end-start`, equal
to the length. Now `cap == len`, there is zero spare capacity, and the handler's
first append that grows the payload finds no room and is *forced* to allocate a
fresh backing array and copy the payload into it. The append lands in the
handler's own new array; the shared read buffer is never touched. This is the
correct discipline for handing out any view into shared storage that a consumer
might append to: cap it to its length so an append cannot reach past the window.

The cost is nothing on the read path (it is the same three words), and the copy
only happens if a consumer actually appends — readers that just read pay nothing.
The two-index version is not "faster"; it is a latent corruption that happens to
not fire until someone appends.

Create `framing.go`:

```go
package framing

import "errors"

// ErrTruncated indicates the buffer ended mid-frame.
var ErrTruncated = errors.New("framing: truncated frame")

// Parse splits a length-prefixed buffer (one length byte, then that many payload
// bytes, repeated) into payload views. Each payload is buf[i:j:j] — capacity is
// bounded to length, so a downstream append is forced to copy instead of
// clobbering the next frame's bytes in the shared buffer.
func Parse(buf []byte) ([][]byte, error) {
	var frames [][]byte
	for i := 0; i < len(buf); {
		n := int(buf[i])
		i++
		if i+n > len(buf) {
			return nil, ErrTruncated
		}
		frames = append(frames, buf[i:i+n:i+n])
		i += n
	}
	return frames, nil
}

// ParseUnbounded is the BUGGY version, kept to document the trap: it hands out
// two-index views whose capacity runs to the end of buf, so a consumer's append
// into one payload overwrites the following frames.
func ParseUnbounded(buf []byte) ([][]byte, error) {
	var frames [][]byte
	for i := 0; i < len(buf); {
		n := int(buf[i])
		i++
		if i+n > len(buf) {
			return nil, ErrTruncated
		}
		frames = append(frames, buf[i:i+n])
		i += n
	}
	return frames, nil
}
```

### The runnable demo

The demo parses two frames, appends to the first payload, and shows that with the
bounded parser the second frame is intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/framing"
)

func main() {
	// Two frames: length 2 "hi", length 2 "yo".
	buf := []byte{2, 'h', 'i', 2, 'y', 'o'}

	frames, err := framing.Parse(buf)
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("frame0=%q cap=%d\n", frames[0], cap(frames[0]))
	fmt.Printf("frame1=%q\n", frames[1])

	// A handler appends to frame0's payload. Bounded cap forces a copy.
	p := append(frames[0], 'X', 'Y')
	fmt.Printf("appended=%q\n", p)
	fmt.Printf("frame1 after append=%q\n", frames[1])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frame0="hi" cap=2
frame1="yo"
appended="hiXY"
frame1 after append="yo"
```

### Tests

`TestBoundedCapEqualsLen` asserts every payload from `Parse` has `cap == len`.
`TestBoundedAppendDoesNotCorruptNeighbor` appends to frame 0 and asserts frame 1 is
still `yo`. `TestUnboundedAppendCorruptsNeighbor` runs the same append through the
buggy parser and asserts the neighbor *is* corrupted, documenting the bug the
three-index form prevents. `TestTruncated` asserts the sentinel via `errors.Is`.

Create `framing_test.go`:

```go
package framing

import (
	"errors"
	"fmt"
	"testing"
)

func frameBuf() []byte {
	// length 2 "hi", length 2 "yo"
	return []byte{2, 'h', 'i', 2, 'y', 'o'}
}

func TestBoundedCapEqualsLen(t *testing.T) {
	t.Parallel()
	frames, err := Parse(frameBuf())
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	for i, f := range frames {
		if cap(f) != len(f) {
			t.Errorf("frame %d: cap=%d len=%d, want cap==len", i, cap(f), len(f))
		}
	}
}

func TestBoundedAppendDoesNotCorruptNeighbor(t *testing.T) {
	t.Parallel()
	frames, err := Parse(frameBuf())
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	_ = append(frames[0], 'X', 'Y') // forced to copy: cap==len
	if string(frames[1]) != "yo" {
		t.Fatalf("neighbor corrupted: frame1=%q, want %q", frames[1], "yo")
	}
}

func TestUnboundedAppendCorruptsNeighbor(t *testing.T) {
	t.Parallel()
	frames, err := ParseUnbounded(frameBuf())
	if err != nil {
		t.Fatalf("ParseUnbounded error: %v", err)
	}
	_ = append(frames[0], 'X', 'Y') // spills into the shared buffer
	if string(frames[1]) == "yo" {
		t.Fatal("expected the unbounded append to corrupt frame1, but it was intact")
	}
}

func TestTruncated(t *testing.T) {
	t.Parallel()
	buf := []byte{4, 'a', 'b'} // claims 4 bytes, only 2 present
	if _, err := Parse(buf); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func ExampleParse() {
	frames, _ := Parse([]byte{3, 'a', 'b', 'c'})
	fmt.Printf("%q cap=%d\n", frames[0], cap(frames[0]))
	// Output: "abc" cap=3
}
```

## Review

`Parse` is correct when every payload has `cap == len` and a downstream append into
any payload cannot reach the shared buffer. The paired tests make the point by
contrast: `TestBoundedAppendDoesNotCorruptNeighbor` passes because the bounded cap
forces the append to copy, and `TestUnboundedAppendCorruptsNeighbor` *requires* the
two-index version to corrupt its neighbor — if that test ever stopped observing
corruption, the buffer layout would no longer be shared and the lesson would be
demonstrating nothing. The habit to carry out of this: when you hand out a view
into memory you do not want a consumer's append to overwrite, use `s[i:j:j]`.
Distinguish it from Exercise 4's clone-on-store: three-index caps defend against a
consumer *appending* past a view; a clone defends against the producer *reusing or
overwriting* the bytes the view covers. Run `-race` to confirm.

## Resources

- [Go Specification: Slice expressions (full slice expression)](https://go.dev/ref/spec#Slice_expressions)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-cache-aliasing-append-bug.md](04-cache-aliasing-append-bug.md) | Next: [06-filter-expired-sessions-in-place.md](06-filter-expired-sessions-in-place.md)
