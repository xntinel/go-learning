# Exercise 12: A Bump Allocator's Bounded Slices Are Not an Isolation Boundary

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Zero-copy record-batch formats -- FlatBuffers, Arrow's IPC format, Cap'n
Proto's message builder -- all share one allocation strategy underneath
their generated code: a bump allocator over a single big `[]byte`. Every
record is carved out of the same backing array with a running offset
instead of its own `make` call, so the whole batch ends up in one
contiguous slice that can be written to a socket or an mmap region without
a single extra copy. To keep one record's `append` from spilling into the
next record still living in the same array, the allocator hands back a
three-index slice expression, `buf[off:off+n:off+n]`, so `cap(result) ==
len(result)` and the runtime is forced to reallocate rather than overwrite
a neighbor.

That guarantee is real, and it is also easy to mistake for more than it is.
Capacity bounding stops one specific failure mode -- a careless `append`
writing past where an allocation was supposed to end. It does nothing about
a second failure mode: any code that also holds the arena's own backing
array, not just the bounded handle Alloc returned, can index straight
through the "isolated" region and corrupt it, no `append` involved, no
bounds violated from that code's point of view. This is the concept this
lesson's notes call "reslicing never copies": every slice expression
aliases the same memory, and the only real safety boundary is which
reference a piece of code is holding, not which slice expression produced
it. This module builds the allocator correctly and then proves that second
fact directly, using the arena's own legitimate `Bytes` method -- the same
method a real batch flush would call -- rather than any bug.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
arena/                     module example.com/arena
  go.mod                   go 1.24
  arena.go                 Arena, New, Alloc, Bytes, Size, Remaining
  arena_test.go             sequential allocations, boundary errors, contained
                            append, the Bytes-write corruption, ExampleArena_Alloc
```

- Files: `arena.go`, `arena_test.go`.
- Implement: `New(size int) *Arena` (a negative size is treated as zero); `(*Arena).Alloc(n int) ([]byte, error)` returning `ErrInvalidSize` for a negative `n` and `ErrArenaFull`, wrapped with the requested and remaining sizes, when `n` exceeds `Remaining()`, otherwise bump-allocating and returning `buf[start:end:end]`; `(*Arena).Bytes() []byte` returning the whole allocated region as one slice; `Size` and `Remaining`.
- Test: sequential allocations including a zero-length one; the two error paths; a negative-size `New`; an append past one allocation's own capacity proving it reallocates instead of corrupting its neighbor; a write through `Bytes()` at a prior allocation's offset proving it *does* corrupt that allocation; and `ExampleArena_Alloc` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/arena
cd ~/go-exercises/arena
go mod init example.com/arena
go mod edit -go=1.24
```

### Reslicing never copies -- capacity bounding changes behavior, not ownership

`buf[start:end:end]` sets both `len` and `cap` to `end - start`. The purpose
of the third index is entirely about `append`: with no spare capacity, an
`append` on the returned slice cannot write into `buf[end]` and beyond --
Go's runtime sees `len == cap`, allocates a fresh backing array, copies the
existing bytes into it, and appends there instead. That is a real and
useful guarantee. It is also the *only* thing the three-index expression
guarantees. The slice header it produces -- pointer, length, capacity -- still
points into `buf`, the arena's one shared array. Nothing about `[start:end:end]`
detaches those bytes from `buf`; it only changes what a *future append on
this particular slice value* is allowed to reach.

So a second reference to `buf` -- the arena's own field, or a slice any
caller took before this `Alloc` call -- can index into `buf[start:end]`
directly and overwrite it, with no `append` involved and no capacity check
to violate, because plain indexing (`s[i] = x`) only ever checks against
`len`, not against who else might be viewing the same memory:

```go
rec, _ := a.Alloc(4)      // rec is buf[off:off+4:off+4], cap == len == 4
copy(rec, "AAAA")         // fully within bounds; nothing is unsafe here

whole := a.Bytes()        // the arena's own backing array, by design
whole[off] = 'Z'          // writes straight through rec's "isolated" region
```

Both operations are, individually, perfectly legal Go. `rec` genuinely
cannot be corrupted by *its own* `append` overrunning into the next
allocation -- that is the property `Alloc`'s three-index bound delivers, and
this module's tests confirm it holds. What it does not deliver, and what a
reader has to stop assuming once they see `Bytes` exists, is protection from
a *different* reference to the same array. The arena's `Bytes` method exists
for a legitimate reason -- flushing or transmitting the whole batch in one
zero-copy slice is the entire point of building it this way -- and its own
doc comment says plainly that holding it and writing through it can corrupt
any allocation made so far. There is no bug here to fix; there is a boundary
to understand: safety comes from which reference your code holds, not from
which slice expression produced the value in your hand.

Create `arena.go`:

```go
// Package arena implements a bump allocator over a single fixed-size
// []byte, the allocation strategy behind zero-copy record-batch formats
// like FlatBuffers and Arrow: every Alloc call hands out the next unused
// region of one shared buffer instead of allocating its own, so an entire
// batch of records lives in one contiguous, IPC-friendly slice.
//
// The property worth internalizing is in the Alloc and Bytes doc comments:
// bounding an allocation's capacity stops its own append from overrunning
// into the next record, but it is not memory safety. Reslicing never
// copies, so any code that also holds the arena's backing array can still
// write straight through a "bounded" allocation.
package arena

import (
	"errors"
	"fmt"
)

// ErrInvalidSize means Alloc was asked for a negative number of bytes.
var ErrInvalidSize = errors.New("arena: allocation size must not be negative")

// ErrArenaFull means the arena does not have n unallocated bytes left.
var ErrArenaFull = errors.New("arena: not enough remaining capacity")

// Arena is a bump allocator over a single backing array: each Alloc call
// carves the next n bytes off the unused tail and advances an offset. There
// is no Free; the whole arena is reclaimed at once when it becomes
// unreachable.
//
// Not safe for concurrent use by multiple goroutines; the caller must
// synchronize calls to Alloc.
type Arena struct {
	buf []byte
	off int
}

// New returns an Arena backed by a single size-byte buffer. A negative size
// is treated as zero, giving an arena that only ever satisfies zero-length
// allocations.
func New(size int) *Arena {
	if size < 0 {
		size = 0
	}
	return &Arena{buf: make([]byte, size)}
}

// Size reports the arena's total capacity in bytes.
func (a *Arena) Size() int { return len(a.buf) }

// Remaining reports how many bytes are still unallocated.
func (a *Arena) Remaining() int { return len(a.buf) - a.off }

// Alloc bump-allocates n bytes from the arena and returns them as a
// three-index-bounded slice: cap(result) == len(result) == n, so the
// caller's own append into the result can never spill into the next
// allocation -- Go reallocates elsewhere instead of writing past cap.
//
// Alloc returns ErrInvalidSize for a negative n, and ErrArenaFull, wrapped
// with the requested and remaining sizes, if n exceeds the arena's
// remaining capacity. Alloc(0) always succeeds and returns a valid,
// zero-length slice.
//
// The returned slice aliases Arena's single backing array. Capacity
// bounding stops this slice's own append from overrunning into a
// neighboring allocation, but it is not an isolation boundary: see Bytes.
func (a *Arena) Alloc(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidSize, n)
	}
	if n > a.Remaining() {
		return nil, fmt.Errorf("%w: requested %d, remaining %d", ErrArenaFull, n, a.Remaining())
	}
	start := a.off
	a.off += n
	return a.buf[start:a.off:a.off], nil
}

// Bytes returns the arena's entire allocated region as one slice, from
// offset zero through the end of the most recent Alloc. It exists so an
// arena's contents can be flushed or transmitted in a single zero-copy
// slice, the FlatBuffers/Arrow IPC pattern this package models.
//
// The returned slice aliases every allocation Alloc has handed out. A
// caller that retains it and writes through it -- even at an offset
// belonging to an allocation some other code already holds -- corrupts
// that allocation directly, because reslicing never copies: the three-index
// bound on a single Alloc's own return value has no effect on what a
// second reference to the same backing array can reach.
func (a *Arena) Bytes() []byte {
	return a.buf[:a.off]
}
```

### Using it

Construct one `Arena` per batch with `New(size)`, call `Alloc` once per
record as it is produced, and write into the returned slice up to its
length -- that region is genuinely yours to fill and to `append` into
without disturbing a neighbor. When the batch is complete, call `Bytes` to
get the whole thing as one slice for a single `Write` or `mmap` call. The
type is not safe for concurrent use: a single `Arena` bump-allocating from
two goroutines at once would race on `off`, so build one per goroutine or
protect it with your own lock if you need to share it.

`ExampleArena_Alloc` in the test file is the executable demonstration of
this module: `go test` runs it and compares its stdout against the
`// Output:` comment, so the usage shown below cannot drift from the code.
It allocates two records, prints the flushed batch as one contiguous slice,
and shows a third allocation being rejected once the arena is full.

### Tests

`TestAlloc` walks a sequence of allocations against one arena, including a
zero-length one that must not advance the offset, checking `len`, `cap`,
and `Remaining()` at each step. `TestBytesReflectsContiguousAllocations`
confirms `Bytes` concatenates every allocation in order with no gaps --
the property that makes it usable as a single flush. `TestAllocRejectsNegativeSize`
and `TestAllocRejectsExceedingRemaining` pin the two sentinel errors, the
second also checking the wrapped message text carries the requested and
remaining sizes. `TestNewWithNegativeSizeIsZeroCapacity` is the boundary
where `New` receives an invalid size and clamps rather than panicking.

The last two tests are the module's point. `TestAllocAppendStaysContained`
appends past one allocation's own capacity and confirms its neighbor is
untouched -- the guarantee the three-index bound actually provides.
`TestBytesWriteCorruptsIsolatedAllocation` then takes the arena's own
`Bytes()` slice and writes through it at the first allocation's offset,
and confirms that allocation's previously-returned handle now reads the
corrupted bytes -- proving that guarantee stops there. Neither test needs a
buggy version of `Alloc` to make its point: both call the real, correct API
exactly as documented, because the lesson here is about which reference a
caller holds, not a defect in the allocator.

Create `arena_test.go`:

```go
package arena

import (
	"errors"
	"fmt"
	"testing"
)

func TestAlloc(t *testing.T) {
	t.Parallel()

	a := New(16)

	first, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	if len(first) != 4 || cap(first) != 4 {
		t.Fatalf("first: len=%d cap=%d, want 4/4", len(first), cap(first))
	}
	if a.Remaining() != 12 {
		t.Fatalf("Remaining() = %d, want 12", a.Remaining())
	}

	second, err := a.Alloc(0)
	if err != nil {
		t.Fatalf("Alloc(0): %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("Alloc(0): len = %d, want 0", len(second))
	}
	if a.Remaining() != 12 {
		t.Fatalf("Remaining() after zero-length alloc = %d, want 12", a.Remaining())
	}

	third, err := a.Alloc(12)
	if err != nil {
		t.Fatalf("Alloc(12): %v", err)
	}
	if len(third) != 12 || a.Remaining() != 0 {
		t.Fatalf("third: len=%d remaining=%d, want 12/0", len(third), a.Remaining())
	}
}

// TestBytesReflectsContiguousAllocations checks that Bytes concatenates
// every allocation made so far, in order and without gaps -- the property
// that makes it usable as a single zero-copy view of a whole record batch.
func TestBytesReflectsContiguousAllocations(t *testing.T) {
	t.Parallel()

	a := New(10)
	first, err := a.Alloc(3)
	if err != nil {
		t.Fatalf("Alloc(3): %v", err)
	}
	copy(first, "abc")

	second, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	copy(second, "wxyz")

	if got, want := string(a.Bytes()), "abcwxyz"; got != want {
		t.Fatalf("Bytes() = %q, want %q", got, want)
	}
}

func TestAllocRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	a := New(16)
	if _, err := a.Alloc(-1); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(-1) error = %v, want ErrInvalidSize", err)
	}
}

func TestAllocRejectsExceedingRemaining(t *testing.T) {
	t.Parallel()

	a := New(8)
	if _, err := a.Alloc(4); err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	_, err := a.Alloc(5)
	if !errors.Is(err, ErrArenaFull) {
		t.Fatalf("Alloc(5) error = %v, want ErrArenaFull", err)
	}
	want := "arena: not enough remaining capacity: requested 5, remaining 4"
	if err.Error() != want {
		t.Fatalf("error text = %q, want %q", err.Error(), want)
	}
}

func TestNewWithNegativeSizeIsZeroCapacity(t *testing.T) {
	t.Parallel()

	a := New(-5)
	if a.Size() != 0 {
		t.Fatalf("Size() = %d, want 0", a.Size())
	}
	if _, err := a.Alloc(1); !errors.Is(err, ErrArenaFull) {
		t.Fatalf("Alloc(1) on zero-size arena error = %v, want ErrArenaFull", err)
	}
	if _, err := a.Alloc(0); err != nil {
		t.Fatalf("Alloc(0) on zero-size arena: %v", err)
	}
}

// TestAllocAppendStaysContained proves the safety property Alloc's
// three-index bound actually provides: appending past a returned
// allocation's own capacity reallocates elsewhere instead of overrunning
// into whatever was allocated right after it.
func TestAllocAppendStaysContained(t *testing.T) {
	t.Parallel()

	a := New(8)
	first, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	second, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	copy(second, "BBBB")

	// first has zero spare capacity (cap == len == 4), so appending past it
	// must allocate a new backing array rather than writing into second.
	grown := append(first, 'X')
	if len(grown) != 5 {
		t.Fatalf("len(grown) = %d, want 5", len(grown))
	}
	if string(second) != "BBBB" {
		t.Fatalf("second = %q after appending to first, want unchanged %q", second, "BBBB")
	}
}

// TestBytesWriteCorruptsIsolatedAllocation is the heart of the module: it
// proves that Alloc's capacity bound is not an isolation boundary. Bytes
// hands out the arena's whole backing array by design (for flushing a
// batch in one zero-copy slice), and writing through that reference at an
// offset another allocation already owns corrupts that allocation exactly
// as documented -- because reslicing never copies, and Bytes and the
// earlier Alloc call still point at the same array.
func TestBytesWriteCorruptsIsolatedAllocation(t *testing.T) {
	t.Parallel()

	a := New(8)
	first, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	copy(first, "AAAA")
	if _, err := a.Alloc(4); err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}

	whole := a.Bytes()
	if len(whole) != 8 {
		t.Fatalf("Bytes(): len = %d, want 8", len(whole))
	}

	// Write through the arena's own retained backing slice, at the exact
	// offset "first" occupies. first was never appended to and never left
	// its own bounds; nothing about its own capacity was violated.
	copy(whole[0:4], "ZZZZ")

	if string(first) != "ZZZZ" {
		t.Fatalf("first = %q after writing through Bytes(), want %q: capacity bounding did not isolate it", first, "ZZZZ")
	}
}

// ExampleArena_Alloc is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleArena_Alloc() {
	a := New(16)

	rec1, err := a.Alloc(6)
	if err != nil {
		panic(err)
	}
	copy(rec1, "alice0")

	rec2, err := a.Alloc(6)
	if err != nil {
		panic(err)
	}
	copy(rec2, "bob000")

	fmt.Printf("rec1=%q rec2=%q remaining=%d\n", rec1, rec2, a.Remaining())
	fmt.Printf("flushed batch: %q\n", a.Bytes())

	if _, err := a.Alloc(5); errors.Is(err, ErrArenaFull) {
		fmt.Println("third record rejected:", err)
	}

	// Output:
	// rec1="alice0" rec2="bob000" remaining=4
	// flushed batch: "alice0bob000"
	// third record rejected: arena: not enough remaining capacity: requested 5, remaining 4
}
```

## Review

`Alloc` is correct when a record's own `append` can never reach a
neighboring record -- `TestAllocAppendStaysContained` pins that by
appending past one allocation's capacity and confirming the next one is
untouched, and it holds because `buf[start:end:end]` forces that specific
`append` to reallocate. What that guarantee does not cover is the module's
real point: `TestBytesWriteCorruptsIsolatedAllocation` writes through the
arena's own `Bytes()` slice, no `append` involved, and shows a previously
"bounded" allocation change value underneath its holder. Both behaviors are
documented, both are correct Go, and neither is a bug -- `Bytes` exists
because flushing a whole batch in one zero-copy slice is the entire reason
to build an arena this way. The general rule this module exists to teach:
capacity bounding governs what one slice value's own `append` can reach; it
says nothing about a second reference to the same backing array, so the
real safety boundary is which reference a piece of code holds, not which
slice expression produced the value in its hand. Around that core, `New`
clamps a negative size to zero instead of panicking, `Alloc` rejects a
negative request with `ErrInvalidSize` and an over-budget one with
`ErrArenaFull`, and `Arena` is explicitly not safe for concurrent use since
`Alloc` mutates its offset with no synchronization. Run
`go test -count=1 -race ./...` to confirm all of it.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the three-index form and its `cap = max - low` rule.
- [`append`](https://go.dev/ref/spec#Appending_and_copying_slices) — why an `append` at `len == cap` must allocate a new backing array.
- [Arrow's IPC format](https://arrow.apache.org/docs/format/Columnar.html#serialization-and-interprocess-communication-ipc) — the real-world zero-copy batch layout this exercise models.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — background on why reslicing never copies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-trace-id-extraction-must-clone.md](11-trace-id-extraction-must-clone.md) | Next: [13-circuit-breaker-window-insert-shift.md](13-circuit-breaker-window-insert-shift.md)
