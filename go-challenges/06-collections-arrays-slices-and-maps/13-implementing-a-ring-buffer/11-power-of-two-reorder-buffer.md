# Exercise 11: Sequence-Indexed Reorder Buffer With Power-of-Two Index Masking

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A QUIC or TCP receive path does not see bytes arrive in order. Packets take
different routes, some get retransmitted, some arrive twice, and the
reassembly layer's job is to place each incoming segment at the exact offset
its sequence number names, then hand the application only the contiguous
prefix that is actually ready to read. That is a different shape from the
ring buffers earlier in this lesson: instead of `Push` always writing at
`head`, `Insert` writes at whatever slot `seq` selects, and the buffer must
compute that slot correctly for every sequence number it will ever see,
millions of times a second, on the hottest path in the stack.

The technique that makes that computation cheap is the same one 00-concepts.md
names for the ring's own wraparound: replace `seq % capacity` with `seq &
(capacity-1)`. A CPU's integer divider is one of its slower units; a bitwise
AND is one cycle. The trade is that the identity `x & (n-1) == x % n` is only
true when `n` is a power of two — for any other `n` it silently computes a
different, wrong number. A reorder buffer that lets a caller request capacity
100 and then masks with `100-1` is not slightly wrong, it is wrong on
specific sequence numbers that alias onto the same slot, and the corruption
looks exactly like a dropped packet: bytes overwritten before anything read
them.

This module builds `seqbuffer`, a package with no notion of TCP or QUIC
framing — just the sequence-indexed placement and masking discipline
underneath either — so you can see the aliasing bug and its fix in isolation
from checksum and header parsing noise.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
seqbuffer/                module example.com/seqbuffer
  go.mod                  go 1.24
  seqbuffer.go            SeqBuffer[T]; New, Insert, DrainReady, Base, Cap; three sentinel errors
  seqbuffer_test.go       naive-mask aliasing contrast, capacity rounding, insert table,
                           gap-stopping drain, slot-clearing, ExampleSeqBuffer
```

- Files: `seqbuffer.go`, `seqbuffer_test.go`.
- Implement: `New[T any](minCapacity int) *SeqBuffer[T]` clamping a non-positive `minCapacity` to 1 and rounding up to the next power of two; `(*SeqBuffer[T]).Insert(seq uint64, v T) error` returning `ErrSeqTooOld`, `ErrWindowFull`, or `ErrDuplicateSeq`; `(*SeqBuffer[T]).DrainReady() []T` popping the contiguous run starting at the current base sequence; `(*SeqBuffer[T]).Base() uint64`; `(*SeqBuffer[T]).Cap() int`.
- Test: a naive-mask helper that aliases sequence 96 onto the same slot as sequence 100 for a requested capacity of 100, contrasted against the real buffer's rounded capacity of 128; capacity rounding across a table of requests; the insert table (in-window, too old, window-full, duplicate); a gap that blocks draining until it fills; the evicted slot cleared to release a pointer; `ExampleSeqBuffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/11-power-of-two-reorder-buffer
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/11-power-of-two-reorder-buffer
go mod edit -go=1.24
```

### The bitmask is only correct when capacity is a power of two

`seq % capacity` wraps any sequence number into `[0, capacity)` for any
positive `capacity`. `seq & (capacity-1)` is a bitwise trick that produces
the identical result *only* when `capacity` is a power of two, because only
then does `capacity-1` have every bit below the top one set (`128-1` is
`0111_1111`, all seven low bits on). For a non-power-of-two capacity like
100 (`0110_0100`), `100-1` is 99 (`0110_0011`) — bit 2 (value 4) is off —
and the mask silently drops that bit from every sequence number it is
applied to:

```go
// The bug: reuse the fast bitmask against whatever capacity the caller asked
// for, without checking it is a power of two.
func (n *naiveSeqBuffer[T]) slot(seq uint64) int {
    return int(seq & uint64(n.cap-1))    // cap=100: bit 2 of every seq is lost
}
```

With `n.cap = 100`, `slot(96)` and `slot(100)` both come out to 96: two
segments that are four bytes apart in the real stream land on the identical
ring slot, and the second `Insert` silently overwrites the first. Nothing
panics, no error surfaces, no length check fails — the reassembled stream is
simply missing four bytes, with a hole a checksum discovers layers away from
where the bug lives. The fix is not a runtime guard on every `Insert`; it is
refusing to let a non-power-of-two capacity exist in the first place. `New`
rounds every requested capacity up to the next power of two and exposes the
real value through `Cap()`, so `Insert`'s bitmask is correct by construction
for the buffer's entire lifetime, and the caller who asked for 100 finds out
they got 128 the same way they find out anything else about the buffer: by
calling a method.

Create `seqbuffer.go`:

```go
// Package seqbuffer implements a sequence-indexed reorder buffer for
// receive-side packet reassembly: a QUIC- or TCP-style component that
// places each incoming segment directly at the ring slot its sequence
// number selects, instead of pushing arrivals onto a FIFO queue.
//
// The one detail that makes the whole thing correct is that the backing
// ring's capacity must be a power of two. On the per-packet hot path the
// slot for sequence number seq is computed with a bitmask, seq &
// (Cap()-1), instead of a modulo. That identity only computes the correct
// wrap when Cap() is a power of two; for any other capacity it aliases
// distinct sequence numbers onto the same slot and silently corrupts
// reassembly. See the package tests for a worked demonstration of the
// aliasing this package refuses to allow by construction: New always
// rounds the requested capacity up to the next power of two.
package seqbuffer

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by Insert. Callers should test for them with
// errors.Is rather than comparing error strings.
var (
	// ErrSeqTooOld means seq is before the buffer's current base and has
	// already been delivered by a previous DrainReady.
	ErrSeqTooOld = errors.New("seqbuffer: sequence already delivered")
	// ErrWindowFull means seq is at or beyond Base()+Cap(): accepting it
	// would require evicting an unread slot still inside the window.
	ErrWindowFull = errors.New("seqbuffer: sequence exceeds window capacity")
	// ErrDuplicateSeq means seq was already inserted and not yet drained.
	ErrDuplicateSeq = errors.New("seqbuffer: sequence already buffered")
)

// SeqBuffer is a fixed-capacity reorder buffer indexed by sequence number.
// Insert places a value directly at the ring slot its sequence number
// selects; DrainReady returns the contiguous run of values starting at the
// buffer's current base sequence, in order, once earlier gaps fill in.
//
// SeqBuffer is not safe for concurrent use. A receive-side reassembly
// buffer is normally owned by exactly one reader goroutine per connection;
// a caller that shares one across goroutines must synchronize externally.
type SeqBuffer[T any] struct {
	data     []T
	occupied []bool
	mask     uint64
	base     uint64
}

// New returns a SeqBuffer whose capacity is the smallest power of two at
// least minCapacity. A non-positive minCapacity is clamped to 1. Rounding
// up, rather than accepting minCapacity verbatim, is what keeps the
// bitmask Insert uses on its hot path correct: seq & (Cap()-1) only
// computes the right wrap when Cap() is a power of two. Call Cap() to
// learn the real capacity New chose.
func New[T any](minCapacity int) *SeqBuffer[T] {
	if minCapacity < 1 {
		minCapacity = 1
	}
	c := nextPowerOfTwo(minCapacity)
	return &SeqBuffer[T]{
		data:     make([]T, c),
		occupied: make([]bool, c),
		mask:     uint64(c - 1),
	}
}

// Cap reports the buffer's real capacity: the smallest power of two at
// least as large as the value requested at construction.
func (b *SeqBuffer[T]) Cap() int { return len(b.data) }

// Base reports the lowest sequence number not yet returned by DrainReady.
// Any seq below Base has already been delivered; any seq at or beyond
// Base()+Cap() falls outside the current window.
func (b *SeqBuffer[T]) Base() uint64 { return b.base }

// slot computes the ring index for seq using a bitmask rather than a
// modulo. This is only correct because Cap() is always a power of two:
// b.mask is Cap()-1, so seq & b.mask wraps exactly like seq % Cap() would,
// at a fraction of the cost on the per-packet path.
func (b *SeqBuffer[T]) slot(seq uint64) int {
	return int(seq & b.mask)
}

// Insert places v at the slot seq selects. It returns ErrSeqTooOld if seq
// precedes Base, ErrWindowFull if seq is at or beyond Base()+Cap(), and
// ErrDuplicateSeq if seq is already buffered and not yet drained.
func (b *SeqBuffer[T]) Insert(seq uint64, v T) error {
	if seq < b.base {
		return fmt.Errorf("%w: seq=%d base=%d", ErrSeqTooOld, seq, b.base)
	}
	if seq-b.base >= uint64(len(b.data)) {
		return fmt.Errorf("%w: seq=%d base=%d cap=%d", ErrWindowFull, seq, b.base, len(b.data))
	}
	idx := b.slot(seq)
	if b.occupied[idx] {
		return fmt.Errorf("%w: seq=%d", ErrDuplicateSeq, seq)
	}
	b.data[idx] = v
	b.occupied[idx] = true
	return nil
}

// DrainReady pops and returns the contiguous run of values starting at
// Base, advancing Base past each one returned. It stops at the first gap,
// so a single missing segment blocks every later one from draining, which
// is the point: a reassembly buffer must deliver bytes in order. It
// returns nil, not an error, if the slot at Base is not yet occupied.
//
// The returned slice is freshly allocated and never aliases internal
// storage; the caller may retain and mutate it freely.
func (b *SeqBuffer[T]) DrainReady() []T {
	var out []T
	for {
		idx := b.slot(b.base)
		if !b.occupied[idx] {
			break
		}
		out = append(out, b.data[idx])
		var zero T
		b.data[idx] = zero
		b.occupied[idx] = false
		b.base++
	}
	return out
}

// nextPowerOfTwo returns the smallest power of two greater than or equal
// to n. n must be at least 1.
func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
```

### Using it

A connection-level reassembler constructs one `SeqBuffer` with `New` sized
to the largest reorder window it is willing to tolerate, then calls `Insert`
once per arriving segment and `DrainReady` whenever it wants to hand
contiguous bytes to the application — after every insert, or on a timer, or
whenever the application asks to read. `Cap()` tells the caller what window
it actually got, since `New` may have rounded a requested size up; a caller
that cares about the exact byte budget should size its requests to already
be a power of two, or read `Cap()` back and act on it rather than assuming
its own request stuck.

Three contracts matter at the boundary. `Insert`'s errors are all sentinel
values checkable with `errors.Is`, wrapped with the offending sequence
number and the buffer's current `Base`/`Cap` for a useful log line.
`DrainReady`'s returned slice is a fresh allocation, never a view into
`SeqBuffer`'s internal array, so the caller can hold onto it, sort it, or
hand it to another goroutine without synchronizing with further inserts.
And `SeqBuffer` itself is not safe for concurrent use — a reassembly buffer
belongs to one connection's one reader goroutine, and the type's doc comment
says so rather than leaving it to be discovered under load.

`ExampleSeqBuffer`, in `seqbuffer_test.go`, is the runnable demonstration of
this module: `go test` executes it and checks its stdout against the
`// Output:` comment. It inserts out of order, shows a gap blocking the
drain, fills the gap, and shows a sequence number far outside the window
rejected with `ErrWindowFull`.

### Tests

`TestNaiveBitmaskAliasesNonPowerOfTwoCapacity` is the module's center of
gravity: `newNaive` and its unexported `slot` method live only in the test
file and are never reachable from the package API. It pins the exact
failure — `naive.slot(96) == naive.slot(100)` for a requested capacity of
100 — and then shows the real `SeqBuffer`, which rounds that same request up
to 128 in `New`, computing distinct slots for the same two sequence numbers.
`TestCapRoundsUpToPowerOfTwo` and `TestNewClampsNonPositiveCapacityToOne`
cover the rounding table and the non-positive-capacity edge case.
`TestInsert` is the request table: a normal insert, a sequence number
already delivered, one beyond the window, and a duplicate within it, each
checked against its sentinel with `errors.Is`. `TestDrainReadyStopsAtTheFirstGap`
and `TestDrainReadyOnEmptyBufferReturnsNothing` pin the ordering guarantee at
its two edges. `TestDrainReadyClearsSlotToAvoidRetainingReferences` uses a
`SeqBuffer[*int]` and reads the unexported `data` slice directly — legal
because the test lives in the same package — to confirm the evicted slot no
longer holds the pointer, the zeroing discipline 00-concepts.md calls a
correctness requirement for any `T` that can leak.

Create `seqbuffer_test.go`:

```go
package seqbuffer

import (
	"errors"
	"fmt"
	"testing"
)

// naiveSeqBuffer is the reorder buffer as it is usually written the first
// time: a constructor that accepts any requested capacity and reuses the
// same bitmask trick regardless. It is never exported and never reachable
// from the package API; it exists only so the tests can pin the aliasing
// it causes when capacity is not a power of two.
type naiveSeqBuffer[T any] struct {
	data []T
	cap  int
}

func newNaive[T any](capacity int) *naiveSeqBuffer[T] {
	return &naiveSeqBuffer[T]{data: make([]T, capacity), cap: capacity}
}

// slot applies the same seq & (cap-1) bitmask SeqBuffer.slot uses, but
// without ever rounding cap up to a power of two first. For a capacity
// like 100 (0b1100100), cap-1 is 99 (0b1100011), and that mask does not
// have every low bit set, so distinct sequence numbers collide.
func (n *naiveSeqBuffer[T]) slot(seq uint64) int {
	return int(seq & uint64(n.cap-1))
}

// TestNaiveBitmaskAliasesNonPowerOfTwoCapacity is the heart of the module:
// with a requested capacity of 100, the naive bitmask maps sequence
// numbers 96 and 100 to the same slot, so the second Insert would silently
// overwrite the first and corrupt reassembly. The real SeqBuffer rounds
// 100 up to 128 in New, and under that mask the same two sequence numbers
// land in different slots.
func TestNaiveBitmaskAliasesNonPowerOfTwoCapacity(t *testing.T) {
	t.Parallel()

	naive := newNaive[string](100)
	if naive.slot(96) != naive.slot(100) {
		t.Fatalf("naive slot(96)=%d slot(100)=%d, want equal: expected the naive mask to alias them",
			naive.slot(96), naive.slot(100))
	}

	real := New[string](100)
	if got := real.Cap(); got != 128 {
		t.Fatalf("real.Cap() = %d, want 128 (100 rounded up to a power of two)", got)
	}
	if real.slot(96) == real.slot(100) {
		t.Fatalf("real slot(96) == slot(100) == %d, want distinct slots", real.slot(96))
	}
}

func TestNewClampsNonPositiveCapacityToOne(t *testing.T) {
	t.Parallel()

	for _, requested := range []int{0, -1, -100} {
		b := New[int](requested)
		if got := b.Cap(); got != 1 {
			t.Errorf("New(%d).Cap() = %d, want 1", requested, got)
		}
	}
}

func TestCapRoundsUpToPowerOfTwo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		requested int
		wantCap   int
	}{
		{1, 1},
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
		{100, 128},
		{128, 128},
		{129, 256},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("requested=%d", tc.requested), func(t *testing.T) {
			t.Parallel()
			if got := New[int](tc.requested).Cap(); got != tc.wantCap {
				t.Fatalf("New(%d).Cap() = %d, want %d", tc.requested, got, tc.wantCap)
			}
		})
	}
}

func TestInsert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		build   func() *SeqBuffer[string]
		seq     uint64
		value   string
		wantErr error
	}{
		{
			name:  "within window succeeds",
			build: func() *SeqBuffer[string] { return New[string](4) },
			seq:   0, value: "a", wantErr: nil,
		},
		{
			name: "seq before base is too old",
			build: func() *SeqBuffer[string] {
				b := New[string](4)
				_ = b.Insert(0, "a")
				b.DrainReady() // advances base to 1
				return b
			},
			seq: 0, value: "x", wantErr: ErrSeqTooOld,
		},
		{
			name:  "seq at base plus cap exceeds the window",
			build: func() *SeqBuffer[string] { return New[string](4) },
			seq:   4, value: "z", wantErr: ErrWindowFull,
		},
		{
			name: "re-inserting a buffered seq is a duplicate",
			build: func() *SeqBuffer[string] {
				b := New[string](4)
				_ = b.Insert(2, "c")
				return b
			},
			seq: 2, value: "c2", wantErr: ErrDuplicateSeq,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.build()
			err := b.Insert(tc.seq, tc.value)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Insert(%d): unexpected error: %v", tc.seq, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Insert(%d) error = %v, want %v", tc.seq, err, tc.wantErr)
			}
		})
	}
}

func TestDrainReadyStopsAtTheFirstGap(t *testing.T) {
	t.Parallel()

	b := New[string](4)
	_ = b.Insert(0, "a")
	_ = b.Insert(2, "c") // seq 1 is missing

	got := b.DrainReady()
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("DrainReady() = %v, want [a]", got)
	}
	if b.Base() != 1 {
		t.Fatalf("Base() = %d, want 1", b.Base())
	}

	_ = b.Insert(1, "b")
	got = b.DrainReady()
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("DrainReady() = %v, want [b c]", got)
	}
	if b.Base() != 3 {
		t.Fatalf("Base() = %d, want 3", b.Base())
	}
}

func TestDrainReadyOnEmptyBufferReturnsNothing(t *testing.T) {
	t.Parallel()

	b := New[string](4)
	if got := b.DrainReady(); len(got) != 0 {
		t.Fatalf("DrainReady() on empty buffer = %v, want empty", got)
	}
}

// TestDrainReadyClearsSlotToAvoidRetainingReferences checks the zeroing
// discipline that matters for a SeqBuffer[T] holding pointers: once
// DrainReady hands a slot's value to the caller, the internal slot must
// not keep its own reference, or a long-lived buffer retains every
// pointer it has ever stored.
func TestDrainReadyClearsSlotToAvoidRetainingReferences(t *testing.T) {
	t.Parallel()

	b := New[*int](2)
	v := 42
	if err := b.Insert(0, &v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	drained := b.DrainReady()
	if len(drained) != 1 || drained[0] != &v {
		t.Fatalf("DrainReady() = %v, want [%p]", drained, &v)
	}
	if idx := b.slot(0); b.data[idx] != nil {
		t.Fatalf("evicted slot %d still holds a pointer: %v", idx, b.data[idx])
	}
}

// ExampleSeqBuffer demonstrates out-of-order insertion, a gap that blocks
// draining, the gap filling in, and a sequence number rejected for
// falling outside the window.
func ExampleSeqBuffer() {
	buf := New[string](4)
	fmt.Println("cap:", buf.Cap())

	_ = buf.Insert(2, "c")
	_ = buf.Insert(0, "a")
	fmt.Println(buf.DrainReady()) // seq 1 missing: only "a" is ready

	_ = buf.Insert(1, "b")
	fmt.Println(buf.DrainReady()) // gap filled: "b" then "c" drain together
	fmt.Println("base:", buf.Base())

	if err := buf.Insert(10, "z"); err != nil {
		fmt.Println("insert seq 10:", errors.Is(err, ErrWindowFull))
	}

	// Output:
	// cap: 4
	// [a]
	// [b c]
	// base: 3
	// insert seq 10: true
}
```

## Review

`SeqBuffer` is correct when its capacity is always a power of two, because
that is the one condition under which `seq & (Cap()-1)` computes the same
wrap as `seq % Cap()` — for any other capacity the mask silently drops bits
and aliases distinct sequence numbers onto the same slot, corrupting
reassembly without a panic or an error anywhere near the bug. `New` closes
that off structurally by rounding every requested capacity up and exposing
the real value through `Cap()`, rather than trusting every call site to pass
a power of two. Around that core, `Insert` rejects a sequence number that
already shipped (`ErrSeqTooOld`), one that would overrun the window
(`ErrWindowFull`), and a re-delivery still pending drain (`ErrDuplicateSeq`),
all checkable with `errors.Is`; `DrainReady` returns only the contiguous
prefix starting at `Base`, zeroing each evicted slot so a `SeqBuffer[*T]`
never retains a pointer past the point the caller received it. The type is
explicitly not safe for concurrent use, matching its real deployment as one
reassembly buffer per connection owned by one goroutine. Run
`go test -count=1 -race ./...` to confirm the aliasing contrast, the
capacity table, the insert table, the gap-stopping drain, and the slot
zeroing.

## Resources

- [Go Specification: Arithmetic operators](https://go.dev/ref/spec#Arithmetic_operators) — the bitwise AND used for the power-of-two mask.
- [RFC 9000, section 2.2](https://www.rfc-editor.org/rfc/rfc9000#section-2.2) — QUIC's description of out-of-order stream data and the receiver's reassembly obligation.
- [Wikipedia: Circular buffer](https://en.wikipedia.org/wiki/Circular_buffer) — the power-of-two masking optimization in general form.
- [errors.Is](https://pkg.go.dev/errors#Is) — how callers should test the sentinel errors this package returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-range-over-func-iterator-and-benchmark.md](10-range-over-func-iterator-and-benchmark.md) | Next: [12-sliding-window-rate-limiter-count-vs-time.md](12-sliding-window-rate-limiter-count-vs-time.md)
