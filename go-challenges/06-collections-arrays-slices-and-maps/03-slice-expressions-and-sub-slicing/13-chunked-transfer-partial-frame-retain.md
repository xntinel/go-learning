# Exercise 13: Decode Length-Prefixed Frames Without Letting the Read Buffer Drift

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A chunked TCP ingest — a custom event-bus consumer, a replicated log
follower — reads bytes off a socket into a fixed-capacity buffer and has to
decode as many complete length-prefixed frames as it currently holds, then
carry forward whatever partial frame is left for the next read. The obvious
implementation reslices the buffer's low bound forward past whatever was
consumed, `buf = buf[consumed:]`, and it is subtly wrong: on a long-running
connection that reads in small chunks, that low bound only ever advances,
never resets, so the buffer eventually runs out of room before `cap` even
though most of its capacity is logically free. This exercise builds the
version that survives — retaining the partial tail by copying it to the
front of the buffer instead of reslicing past it.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
framedecoder/               module example.com/framedecoder
  go.mod                    go 1.24
  framedecoder.go            ErrBufferFull; Decoder with Feed/Decode/Buffered/Available; New; EncodeFrame
  framedecoder_test.go       exact frames, split header, split body, capacity-drift test,
                             the naive-reslice contrast, ExampleDecoder
```

- Files: `framedecoder.go`, `framedecoder_test.go`.
- Implement: `New(capacity int) *Decoder` building a `Decoder` holding
  `buf []byte` at a fixed capacity; `(*Decoder).Feed(data []byte) error`
  appending or returning `ErrBufferFull`; `(*Decoder).Decode() [][]byte`
  extracting every complete `[4-byte length][payload]` frame currently
  buffered and then compacting any partial tail to the front of `buf` with
  `copy(buf, buf[consumed:])` followed by `buf = buf[:n]`, never with a plain
  reslice of `buf` itself.
- Test: a read that lands exactly on frame boundaries; a length prefix split
  across two reads; a payload split across two reads; two hundred rounds of
  split delivery on a small buffer proving `Available()` returns to full
  capacity every round; an unexported `naiveDecoder` that retains its tail by
  reslicing, contrasted against `Decoder` on the same split-delivery
  scenario; and `ExampleDecoder` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/framedecoder
cd ~/go-exercises/framedecoder
go mod init example.com/framedecoder
go mod edit -go=1.24
```

### Why retaining the tail is a copy, not a reslice

`Decode` walks the buffer from the front, decoding every frame whose 4-byte
length prefix and full payload are both present, and stops the instant it
hits a frame that is not yet complete — a split header (fewer than 4 bytes
left) or a split body (header decoded, but not enough payload bytes buffered
yet). Whatever bytes are left at that point are a real, partial frame that
must survive until the next `Feed` completes it.

The tempting way to keep those bytes is the same reslice this lesson has used
everywhere else: `d.buf = d.buf[consumed:]`. It is correct in the sense that
the partial frame's bytes are still there — but it is exactly the "head/tail
trimming does not free memory" trap from this lesson's concepts file, applied
to a buffer that gets refilled instead of one that gets read once and
discarded:

```go
func (d *naiveDecoder) decode() [][]byte {
	// ... decode every complete frame, tracking consumed ...
	d.buf = d.buf[consumed:] // the bug: the low bound only ever moves forward
	return frames
}
```

Every call's reslice moves the buffer's low bound forward by `consumed` and
never moves it back. The next `Feed` appends *after* the current (shrunken)
length, which is still measured against the *original* backing array's
capacity — so the space *before* the current low bound is gone forever, even
though it holds no live data. A connection that reads in small chunks calls
`Decode` constantly, and the low bound creeps forward every time; eventually
there is no room left before `cap(buf)` for `Feed` to append into, and
`ErrBufferFull` starts firing on a buffer that is, logically, mostly empty.

The fix is to physically move the surviving bytes back to offset zero instead
of just changing where the slice header starts counting from:

```
n := copy(d.buf, d.buf[consumed:])
d.buf = d.buf[:n]
```

`copy` moves the partial tail's bytes to the front of the *same* backing
array and returns how many bytes it moved; reslicing to `d.buf[:n]` then just
updates the length, with the full original capacity still available after it.
Every `Decode` call ends with the buffer's low bound back at zero, so the
usable capacity for the next `Feed` never shrinks — the buffer cannot drift no
matter how many small, split reads arrive over the connection's lifetime.

Create `framedecoder.go`:

```go
// Package framedecoder decodes length-prefixed frames out of a fixed-
// capacity read buffer, the way a chunked TCP ingest (a custom event-bus
// consumer, a replicated log follower) accumulates bytes from the wire and
// must hand back complete frames while carrying an incomplete tail forward
// to the next read.
package framedecoder

import (
	"encoding/binary"
	"errors"
)

// ErrBufferFull means Feed would grow the buffer past its fixed capacity.
// A real connection would treat this as backpressure: stop reading from the
// socket until Decode has freed room.
var ErrBufferFull = errors.New("framedecoder: buffer full, no room for read")

// Decoder accumulates bytes from repeated reads into a fixed-capacity
// buffer and decodes length-prefixed frames out of it. Each frame on the
// wire is a 4-byte big-endian length prefix followed by that many bytes of
// payload.
//
// A Decoder is not safe for concurrent use: Feed and Decode both mutate the
// internal buffer. A connection with one reader goroutine should own one
// Decoder.
type Decoder struct {
	// buf's length is the number of valid, not-yet-fully-decoded bytes
	// currently held; its capacity is fixed for the Decoder's lifetime.
	buf []byte
}

// New builds a Decoder backed by a scratch buffer of the given capacity.
func New(capacity int) *Decoder {
	return &Decoder{buf: make([]byte, 0, capacity)}
}

// Feed appends newly read bytes to the internal buffer. It returns
// ErrBufferFull, without buffering any of data, if data would not fit in
// the remaining capacity.
func (d *Decoder) Feed(data []byte) error {
	if len(data) > cap(d.buf)-len(d.buf) {
		return ErrBufferFull
	}
	d.buf = append(d.buf, data...)
	return nil
}

// Decode extracts every complete frame currently held in the buffer, in
// order, and returns them as independent copies. Any trailing bytes that do
// not yet form a complete frame -- a split header or a split body -- are
// retained for the next call.
//
// Retention is done by copying the partial tail to the front of the buffer
// (copy(buf, buf[consumed:])) and truncating the length to match, not by
// reslicing (buf = buf[consumed:]). Reslicing would advance the buffer's
// low bound every call without ever recovering the space before it -- the
// same non-freeing trim behavior that lets a small view pin a large array --
// so a long-running connection that reads in small chunks would eventually
// find no room left before cap, even though most of the buffer's capacity
// is logically free. Copying to offset zero after every Decode is what
// keeps the buffer's usable capacity constant for the life of the
// connection. See the package tests for a side-by-side demonstration.
func (d *Decoder) Decode() [][]byte {
	var frames [][]byte
	consumed := 0

	for {
		remaining := d.buf[consumed:]
		if len(remaining) < 4 {
			break // split header: fewer than 4 length-prefix bytes buffered
		}
		frameLen := int(binary.BigEndian.Uint32(remaining[:4]))
		if len(remaining) < 4+frameLen {
			break // split body: header decoded, payload not fully buffered yet
		}

		frame := make([]byte, frameLen)
		copy(frame, remaining[4:4+frameLen])
		frames = append(frames, frame)
		consumed += 4 + frameLen
	}

	n := copy(d.buf, d.buf[consumed:])
	d.buf = d.buf[:n]
	return frames
}

// Buffered reports how many bytes are currently held, complete or partial.
func (d *Decoder) Buffered() int {
	return len(d.buf)
}

// Available reports how many more bytes Feed can accept before returning
// ErrBufferFull.
func (d *Decoder) Available() int {
	return cap(d.buf) - len(d.buf)
}

// EncodeFrame builds one length-prefixed wire frame from payload: a 4-byte
// big-endian length followed by the payload bytes. It is the encode side of
// the protocol Decode reads, used here to build test and example input.
func EncodeFrame(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(payload)))
	copy(out[4:], payload)
	return out
}
```

### Using it

Construct one `Decoder` per connection with `New(capacity)`, sized to the
largest frame (or small batch of frames) you expect between reads. Feed it
bytes as they arrive from the socket; call `Decode` after every `Feed` to
drain whatever complete frames are now available. `Buffered()` and
`Available()` are read-only introspection, useful for logging or metrics —
`Available() == 0` after a `Decode` that returned no frames is the signal to
apply backpressure upstream rather than call `Feed` again.

Every frame `Decode` returns is an independent copy (`make` plus `copy`, not
a sub-slice), so callers may retain it indefinitely without pinning the
internal buffer alive. `Decoder` is not safe for concurrent use: `Feed` and
`Decode` both mutate the same internal buffer, so a connection with one
reader goroutine should own one `Decoder`.

The module has no `main.go`, because a frame decoder is a library, not a
tool. Its executable demonstration is `ExampleDecoder`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

```go
func ExampleDecoder() {
	const capacity = 32
	d := New(capacity)

	f1 := EncodeFrame([]byte("order-42"))
	f2 := EncodeFrame([]byte("order-43"))

	// Round 1: one read delivers a split header -- only 2 of 4 length bytes.
	chunk1 := f1[:2]
	if err := d.Feed(chunk1); err != nil {
		panic(err)
	}
	frames := d.Decode()
	fmt.Printf("round 1: fed %d bytes, decoded %d frames, buffered=%d available=%d\n",
		len(chunk1), len(frames), d.Buffered(), d.Available())

	// Round 2: the rest of frame 1's header and body, plus all of frame 2 --
	// two frames worth of data land in the same read. available=32 again
	// afterward is the point: the buffer fully reclaims its capacity.
	chunk2 := concatFrames(f1[2:], f2)
	if err := d.Feed(chunk2); err != nil {
		panic(err)
	}
	frames = d.Decode()
	fmt.Printf("round 2: fed %d bytes, decoded %d frames, buffered=%d available=%d\n",
		len(chunk2), len(frames), d.Buffered(), d.Available())
	for i, f := range frames {
		fmt.Printf("  frame %d: %q\n", i, f)
	}

	// Output:
	// round 1: fed 2 bytes, decoded 0 frames, buffered=2 available=30
	// round 2: fed 22 bytes, decoded 2 frames, buffered=0 available=32
	//   frame 0: "order-42"
	//   frame 1: "order-43"
}
```

`available=32` reappearing after round 2, even after two rounds of split
delivery, is the point of the exercise: every complete decode returns the
buffer to a fully reclaimed state.

### Tests

The first three tests are the table the exercise is built around: frames
that land exactly on read boundaries, a length prefix split across two
reads, and a payload split across two reads. `TestDecodeNeverDriftsCapacity`
is the sharpest one: it drives two hundred rounds of split delivery through a
16-byte buffer — far more total bytes than the buffer's capacity — and
asserts `Available()` returns to the full 16 bytes after every round, which
is only possible if `Decode` is compacting instead of reslicing.

`naiveDecoder` is `Decoder` with a single change — `Decode` retaining its
partial tail with `d.buf = d.buf[consumed:]` instead of the copy-and-truncate
above — and it is unexported and unreachable from the package API.
`TestNaiveDecoderDriftsCapacity` reproduces the exact split-delivery scenario
`TestDecodeNeverDriftsCapacity` proves the real `Decoder` survives, and
pins that the naive version instead hits `ErrBufferFull` within the first
few rounds, on a buffer that is logically empty every time it fails.

Create `framedecoder_test.go`:

```go
package framedecoder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// concatFrames joins pre-encoded wire frames into one read-sized chunk, the
// way several frames often arrive back to back in a single socket read.
func concatFrames(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func TestDecodeExactFrames(t *testing.T) {
	t.Parallel()

	d := New(64)
	f1 := EncodeFrame([]byte("alpha"))
	f2 := EncodeFrame([]byte("beta"))

	if err := d.Feed(concatFrames(f1, f2)); err != nil {
		t.Fatalf("Feed() unexpected error: %v", err)
	}

	frames := d.Decode()
	if len(frames) != 2 {
		t.Fatalf("Decode() returned %d frames, want 2", len(frames))
	}
	if string(frames[0]) != "alpha" || string(frames[1]) != "beta" {
		t.Fatalf("Decode() = %q, %q; want %q, %q", frames[0], frames[1], "alpha", "beta")
	}
	if got := d.Buffered(); got != 0 {
		t.Fatalf("Buffered() after exact frames = %d, want 0", got)
	}
}

func TestDecodeSplitHeader(t *testing.T) {
	t.Parallel()

	d := New(64)
	frame := EncodeFrame([]byte("payload"))

	// Deliver only the first 2 of the 4 length-prefix bytes.
	if err := d.Feed(frame[:2]); err != nil {
		t.Fatalf("Feed() unexpected error: %v", err)
	}
	if frames := d.Decode(); len(frames) != 0 {
		t.Fatalf("Decode() with split header = %d frames, want 0", len(frames))
	}
	if got := d.Buffered(); got != 2 {
		t.Fatalf("Buffered() after split header = %d, want 2", got)
	}

	// Deliver the rest: the remaining 2 header bytes plus the full body.
	if err := d.Feed(frame[2:]); err != nil {
		t.Fatalf("Feed() unexpected error: %v", err)
	}
	frames := d.Decode()
	if len(frames) != 1 || string(frames[0]) != "payload" {
		t.Fatalf("Decode() after completing header = %v, want [\"payload\"]", frames)
	}
	if got := d.Buffered(); got != 0 {
		t.Fatalf("Buffered() after completing frame = %d, want 0", got)
	}
}

func TestDecodeSplitBody(t *testing.T) {
	t.Parallel()

	d := New(64)
	frame := EncodeFrame([]byte("longer-payload"))

	// Deliver the full 4-byte header plus only 3 bytes of the body.
	if err := d.Feed(frame[:4+3]); err != nil {
		t.Fatalf("Feed() unexpected error: %v", err)
	}
	if frames := d.Decode(); len(frames) != 0 {
		t.Fatalf("Decode() with split body = %d frames, want 0", len(frames))
	}
	if got := d.Buffered(); got != 4+3 {
		t.Fatalf("Buffered() after split body = %d, want %d", got, 4+3)
	}

	// Deliver the remaining body bytes.
	if err := d.Feed(frame[4+3:]); err != nil {
		t.Fatalf("Feed() unexpected error: %v", err)
	}
	frames := d.Decode()
	if len(frames) != 1 || string(frames[0]) != "longer-payload" {
		t.Fatalf("Decode() after completing body = %v, want [\"longer-payload\"]", frames)
	}
}

// TestDecodeNeverDriftsCapacity is the sharpest test: it drives the decoder
// through many rounds of split delivery on a small, fixed-capacity buffer,
// processing far more total bytes than the buffer's capacity. Because
// Decode compacts its partial tail instead of reslicing past it,
// Available() returns to full capacity after every fully consumed round,
// and Feed never fails.
func TestDecodeNeverDriftsCapacity(t *testing.T) {
	t.Parallel()

	const capacity = 16 // room for exactly one small frame's header + body
	d := New(capacity)

	for round := 0; round < 200; round++ {
		payload := []byte(fmt.Sprintf("p%d", round%10))
		frame := EncodeFrame(payload)
		if len(frame) > capacity {
			t.Fatalf("test frame %d exceeds decoder capacity %d", len(frame), capacity)
		}

		// Split delivery: header arrives first, body arrives after.
		if err := d.Feed(frame[:4]); err != nil {
			t.Fatalf("round %d: Feed(header) unexpected error: %v", round, err)
		}
		if frames := d.Decode(); len(frames) != 0 {
			t.Fatalf("round %d: Decode() with split header = %d frames, want 0", round, len(frames))
		}

		if err := d.Feed(frame[4:]); err != nil {
			t.Fatalf("round %d: Feed(body) unexpected error: %v", round, err)
		}
		frames := d.Decode()
		if len(frames) != 1 || string(frames[0]) != string(payload) {
			t.Fatalf("round %d: Decode() = %v, want [%q]", round, frames, payload)
		}

		if got := d.Available(); got != capacity {
			t.Fatalf("round %d: Available() = %d, want %d (capacity did not fully recover)", round, got, capacity)
		}
	}
}

func TestFeedRejectsOverCapacity(t *testing.T) {
	t.Parallel()

	d := New(4)
	if err := d.Feed(make([]byte, 5)); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("Feed() err = %v, want ErrBufferFull", err)
	}
	if got := d.Buffered(); got != 0 {
		t.Fatalf("Buffered() after rejected Feed = %d, want 0 (nothing partially buffered)", got)
	}
}

// naiveDecoder is Decoder with a single change: Decode retains its partial
// tail with a plain reslice, d.buf = d.buf[consumed:], instead of copying
// the tail to the front of the buffer. It is never exported and never
// reachable from the package API; it exists only so the test below can pin
// the drift bug numerically instead of just asserting it in prose.
type naiveDecoder struct {
	buf []byte
}

func newNaiveDecoder(capacity int) *naiveDecoder {
	return &naiveDecoder{buf: make([]byte, 0, capacity)}
}

func (d *naiveDecoder) feed(data []byte) error {
	if len(data) > cap(d.buf)-len(d.buf) {
		return ErrBufferFull
	}
	d.buf = append(d.buf, data...)
	return nil
}

func (d *naiveDecoder) decode() [][]byte {
	var frames [][]byte
	consumed := 0
	for {
		remaining := d.buf[consumed:]
		if len(remaining) < 4 {
			break
		}
		frameLen := int(binary.BigEndian.Uint32(remaining[:4]))
		if len(remaining) < 4+frameLen {
			break
		}
		frame := make([]byte, frameLen)
		copy(frame, remaining[4:4+frameLen])
		frames = append(frames, frame)
		consumed += 4 + frameLen
	}
	d.buf = d.buf[consumed:] // the bug: the low bound only ever moves forward
	return frames
}

// TestNaiveDecoderDriftsCapacity reproduces the exact scenario
// TestDecodeNeverDriftsCapacity proves the real Decoder survives: repeated
// split delivery on a small, fixed-capacity buffer. The naive decoder fails
// within a handful of rounds with ErrBufferFull even though every byte it
// is holding at that point is a fully-decoded frame it should have
// forgotten -- the buffer is logically empty and reports full anyway.
func TestNaiveDecoderDriftsCapacity(t *testing.T) {
	t.Parallel()

	const capacity = 16
	d := newNaiveDecoder(capacity)

	failedAt := -1
	for round := 0; round < 50 && failedAt == -1; round++ {
		payload := []byte(fmt.Sprintf("p%d", round%10))
		frame := EncodeFrame(payload)

		if err := d.feed(frame[:4]); err != nil {
			failedAt = round
			break
		}
		d.decode()
		if err := d.feed(frame[4:]); err != nil {
			failedAt = round
			break
		}
		d.decode()
	}

	if failedAt == -1 {
		t.Fatal("naive decoder never hit ErrBufferFull; want the drift bug to reproduce within 50 rounds")
	}
	if failedAt > 5 {
		t.Fatalf("naive decoder drifted into ErrBufferFull at round %d, want within the first few rounds", failedAt)
	}
}

// ExampleDecoder is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below. It
// drives a 32-byte buffer through a split header, then a read that lands
// two frames' worth of bytes at once, printing Buffered() and Available()
// at each step to make the tail-compaction visible.
func ExampleDecoder() {
	const capacity = 32
	d := New(capacity)

	f1 := EncodeFrame([]byte("order-42"))
	f2 := EncodeFrame([]byte("order-43"))

	// Round 1: one read delivers a split header -- only 2 of 4 length bytes.
	chunk1 := f1[:2]
	if err := d.Feed(chunk1); err != nil {
		panic(err)
	}
	frames := d.Decode()
	fmt.Printf("round 1: fed %d bytes, decoded %d frames, buffered=%d available=%d\n",
		len(chunk1), len(frames), d.Buffered(), d.Available())

	// Round 2: the rest of frame 1's header and body, plus all of frame 2 --
	// two frames worth of data land in the same read. available=32 again
	// afterward is the point: the buffer fully reclaims its capacity.
	chunk2 := concatFrames(f1[2:], f2)
	if err := d.Feed(chunk2); err != nil {
		panic(err)
	}
	frames = d.Decode()
	fmt.Printf("round 2: fed %d bytes, decoded %d frames, buffered=%d available=%d\n",
		len(chunk2), len(frames), d.Buffered(), d.Available())
	for i, f := range frames {
		fmt.Printf("  frame %d: %q\n", i, f)
	}

	// Output:
	// round 1: fed 2 bytes, decoded 0 frames, buffered=2 available=30
	// round 2: fed 22 bytes, decoded 2 frames, buffered=0 available=32
	//   frame 0: "order-42"
	//   frame 1: "order-43"
}
```

## Review

The decoder is correct when every complete frame in a read is decoded, any
partial trailing bytes survive intact to the next `Feed`, and — the property
the other tests alone would not catch — `Available()` returns to full
capacity after every round that fully drains, no matter how many split reads
it took to get there. `naiveDecoder` pins the wrong turn precisely:
`d.buf = d.buf[consumed:]` in place of the copy-and-truncate at the end of
`Decode` passes every one of the boundary tests, because none of them run
long enough to exhaust capacity, and only a test that processes far more
bytes than the buffer holds forces the drift to show up as spurious
`ErrBufferFull` failures. `Decoder` is not safe for concurrent use, since
`Feed` and `Decode` share one mutable buffer, and every frame `Decode`
returns is an independent copy the caller may retain freely. `ExampleDecoder`
is the executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [`copy` (builtin)](https://pkg.go.dev/builtin#copy)
- [`encoding/binary`](https://pkg.go.dev/encoding/binary)
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-cursor-advance-buf-reslice.md](12-cursor-advance-buf-reslice.md) | Next: [14-multipart-boundary-zero-copy-views.md](14-multipart-boundary-zero-copy-views.md)
