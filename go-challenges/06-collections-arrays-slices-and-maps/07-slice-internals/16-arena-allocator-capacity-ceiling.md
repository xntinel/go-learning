# Exercise 16: An Arena Allocator That Refuses to Silently Regrow

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

High-throughput storage engines almost never allocate objects one at a time
on the hot path. Pebble and BadgerDB build each memtable inside a bump
allocator: one preallocated backing buffer, and every key or value written
into it is just a growing sub-slice of that same array, handed out with a
pointer bump instead of a call into the general-purpose allocator. FlatBuffers
does the same thing for zero-copy message construction. The technique
inverts everything the rest of this lesson warns about: instead of avoiding
shared backing arrays because aliasing is dangerous, an arena *depends* on
every allocation from one generation sharing exactly one array, because that
sharing is what makes `Reset` -- freeing everything at once by resetting one
cursor -- correct and free.

That invariant has exactly one way to break, and it is the same append
reflex the rest of this lesson trains you to reach for everywhere else.
Implement `Alloc` as `buf = append(buf, make([]byte, n)...)` and it behaves
identically to a real arena right up until a request exceeds the buffer's
capacity -- at which point `append` does what `append` always does when it
runs out of room: it allocates a brand new array, copies everything over,
and returns a header pointing at it. Nothing signals this to the caller.
Every slice handed out before that moment now aliases an orphaned array that
no future allocation will ever share again, silently and permanently
violating the one property the whole design exists to guarantee.

This module builds `arena`, a bump allocator that never takes that silent
path: once its backing buffer is full, `Alloc` returns `ErrArenaFull`
instead of growing. The append-based version that grows anyway is not part
of that API -- it lives only in the test file, as the thing the tests prove
wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
arena/                   module example.com/arena
  go.mod                 go 1.24
  arena.go               Arena; New, Alloc, Reset, Available
  arena_test.go           allocation table, sharing, capacity clip, Reset reuse,
                          the naive-append contrast, Example
```

- Files: `arena.go`, `arena_test.go`.
- Implement: `New(size int) (*Arena, error)` rejecting a non-positive size with `ErrInvalidSize`; `(*Arena).Alloc(n int) ([]byte, error)` cutting `n` bytes from the arena's unused tail, capped to itself via a three-index slice so a caller's `append` cannot spill into the next allocation, returning `ErrInvalidSize` for a negative `n` and `ErrArenaFull` instead of growing when fewer than `n` bytes remain; `(*Arena).Reset()` rewinding the cursor without reallocating; `(*Arena).Available() int`.
- Test: the allocation table (single fit, several small fits, a zero-length allocation, an exact fit followed by overflow, a single request larger than the whole arena); a negative-length request rejected; two allocations proven to share one backing array; `append` on one allocation proven unable to reach the next; `Reset` proven to reuse the same array rather than allocate a new one; a `naiveArena` contrast proving an append-based allocator silently reallocates past capacity while `Arena` refuses; and `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/arena
cd ~/go-exercises/arena
go mod init example.com/arena
go mod edit -go=1.24
```

### One backing array, many allocations, one cursor

An arena is one `make([]byte, size)` call and a running offset. `Alloc(n)`
returns `buf[used:used+n]` and advances `used` by `n` -- no search, no
free list, no per-allocation bookkeeping. `Reset` sets `used` back to zero,
so the entire buffer becomes available again without a single byte being
zeroed or reallocated; the next generation of allocations reuses the same
array the previous generation used, exactly the `s = s[:0]` reuse idiom this
lesson introduces elsewhere, just applied at the scale of an entire request
or compaction pass instead of one buffer.

The property that makes this useful is that two allocations from the same
generation are provably views into the same array: `unsafe.SliceData` on
either one, adjusted by the gap between them, lands on the other. That
matters operationally -- a caller who receives several arena allocations and
wants to bound their combined lifetime knows a single `Reset` call frees all
of them together, with no reference counting. It stops being true the moment
`Alloc` is written to grow past its budget instead of refusing:

```go
func (a *naiveArena) alloc(n int) []byte {
    start := len(a.buf)
    a.buf = append(a.buf, make([]byte, n)...)
    return a.buf[start : start+n]
}
```

Every call here behaves exactly like the real thing while `len(a.buf)+n`
stays under the slice's capacity. The instant it does not, `append`
reallocates: `a.buf` now points at a new array, and everything returned by
earlier calls still points at the old one. Nothing panics, nothing errors,
and the two allocations that used to be provably adjacent now live in
memory that has nothing to do with each other. A caller relying on the
sharing invariant -- to bound a lifetime, to compute an offset, to reason
about a single contiguous write region -- silently gets the wrong answer.

Create `arena.go`:

```go
// Package arena implements a bump allocator over one fixed-size backing
// buffer, the pattern high-throughput storage engines use for a memtable or
// a request-scoped scratch pool: many small allocations that are just
// growing sub-slices of one array, freed all at once with Reset instead of
// one at a time.
//
// The defining invariant of an arena generation is that every allocation it
// hands out shares the same backing array. That invariant is easy to break
// by accident: implement Alloc with append and let it grow past capacity,
// and the runtime silently switches to a brand new array without telling
// the caller, so "these two allocations alias" quietly stops being true.
// This package refuses to grow: Alloc returns ErrArenaFull instead. See the
// package tests for a naive, append-based allocator that breaks the
// invariant, isolated from this package's API.
package arena

import (
	"errors"
	"fmt"
)

// ErrInvalidSize is returned by New for a non-positive size and by Alloc
// for a negative length.
var ErrInvalidSize = errors.New("arena: size must not be negative")

// ErrArenaFull is returned by Alloc when the arena's backing buffer does not
// have room for the request. The arena never grows to satisfy an
// over-budget allocation; the caller must Reset or use a larger Arena.
var ErrArenaFull = errors.New("arena: allocation exceeds remaining capacity")

// Arena is a bump allocator over a single fixed-size []byte buffer. Alloc
// cuts a sub-slice from the unused tail of that buffer and advances a
// cursor; it never reallocates. Reset rewinds the cursor to zero so the
// same backing array can be reused for a new generation of allocations.
//
// Arena is not safe for concurrent use. Alloc and Reset both mutate the
// arena's cursor without synchronization; a caller sharing one Arena across
// goroutines must guard it with its own lock.
//
// Every slice Alloc returns aliases the arena's single backing array.
// Retaining an allocation past the next Reset is a use-after-free in
// spirit: Reset does not clear the buffer, so a later Alloc can hand out
// the same bytes to a new, unrelated caller, and both will observe writes
// meant for the other. Slices from one generation (the span between two
// Resets) are safe to alias each other; nothing is safe to alias across a
// Reset.
type Arena struct {
	buf  []byte
	used int
}

// New returns an Arena backed by a buffer of exactly size bytes. It returns
// ErrInvalidSize if size is not positive.
func New(size int) (*Arena, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidSize, size)
	}
	return &Arena{buf: make([]byte, size)}, nil
}

// Alloc returns a slice of exactly n bytes cut from the arena's unused
// tail, and advances the arena's cursor past it. It returns ErrInvalidSize
// for a negative n and ErrArenaFull if fewer than n bytes remain -- Alloc
// never reallocates to satisfy a request that does not fit.
//
// The returned slice is capped to its own length via a three-index slice
// expression, so appending to it can never spill into the arena's next
// allocation: append is forced to allocate a fresh array instead of
// scribbling past the end of what was granted. The slice still aliases the
// arena's backing array for reads, writes within bounds, and reslicing.
func (a *Arena) Alloc(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: negative length %d", ErrInvalidSize, n)
	}
	if n > len(a.buf)-a.used {
		return nil, fmt.Errorf("%w: requested %d, %d available", ErrArenaFull, n, len(a.buf)-a.used)
	}
	start := a.used
	a.used += n
	return a.buf[start:a.used:a.used], nil
}

// Reset rewinds the arena to empty, making its entire backing buffer
// available to the next Alloc call. It does not zero the buffer and does
// not allocate a new one: the same array is reused, which is the point of
// an arena. Every slice returned by Alloc before this call must not be used
// after it.
func (a *Arena) Reset() {
	a.used = 0
}

// Available reports how many bytes the next Alloc call can still satisfy
// without returning ErrArenaFull.
func (a *Arena) Available() int {
	return len(a.buf) - a.used
}
```

### Using it

Construct one `Arena` per generation -- a memtable's lifetime, a request's
lifetime, one pass of a batch job -- sized to the largest total allocation
that generation will ever need, and call `Alloc` for each piece of scratch
space instead of `make`. When the generation ends, `Reset` and hand the same
`Arena` to the next one; nothing is freed piece by piece, and nothing is
garbage-collected until the whole arena itself is. A size that turns out too
small is a configuration bug caught immediately as `ErrArenaFull`, not a
silent switch to unbounded, ungoverned allocation.

`Arena` is not safe for concurrent use, and the returned allocations alias
the arena's own backing array -- both contracts are stated on the type and
enforced by the tests: `TestAllocationsShareOneBackingArray` pins the
sharing, and `TestAllocClipsCapacitySoAppendCannotSpill` pins that appending
to one allocation cannot reach into its neighbor. `Example` is the runnable
demonstration of this module: `go test` executes it and compares its stdout
against the `// Output:` comment below, so the usage shown here cannot drift
away from the code.

```go
func Example() {
	a, err := New(16)
	if err != nil {
		panic(err)
	}

	x, err := a.Alloc(8)
	if err != nil {
		panic(err)
	}
	y, err := a.Alloc(4)
	if err != nil {
		panic(err)
	}

	adjacent := uintptr(basePointer(y))-uintptr(basePointer(x)) == uintptr(len(x))
	fmt.Println("second allocation immediately follows the first:", adjacent)

	_, err = a.Alloc(8) // only 4 bytes remain
	fmt.Println("over-budget allocation rejected:", errors.Is(err, ErrArenaFull))

	// Output:
	// second allocation immediately follows the first: true
	// over-budget allocation rejected: true
}
```

### Tests

`TestAlloc` is the table: single allocations that fit exactly, several small
ones that fit together, a zero-length allocation, an exact fit followed by
one byte too many, and a single request bigger than the whole arena.
`TestAllocationsShareOneBackingArray` and `TestAllocClipsCapacitySoAppendCannotSpill`
pin the two aliasing contracts stated on `Alloc`'s doc comment directly,
using `unsafe.SliceData` to compare addresses rather than trusting the
comment. `TestResetReusesTheSameArray` pins that `Reset` never reallocates.

`TestNaiveArenaSilentlyOrphansEarlierAllocations` is the heart of the
module. `naiveArena` is unexported and unreachable from the package API; it
implements `Alloc` with `append` the way most people do on a first attempt.
The test shows it behaving identically to the real `Arena` while requests
stay under capacity, then shows the exact moment it diverges: the instant a
request exceeds the buffer's capacity, the naive version's backing array
address changes with no error returned, while the real `Arena`, given the
identical request pattern, returns `ErrArenaFull` instead. If a future edit
reintroduces `append` into `Alloc`, this test fails here instead of in a
memtable silently losing the sharing invariant a compaction routine depends
on.

Create `arena_test.go`:

```go
package arena

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"unsafe"
)

// naiveArena mimics the bump allocator most people write on the first
// attempt: Alloc just appends onto a slice. It is never exported and is not
// part of this package's API; it exists only so the tests can pin exactly
// what it gets wrong. Below its initial capacity it behaves like Arena. The
// moment a request exceeds that capacity, append silently switches n.buf to
// a brand new backing array -- every allocation handed out before that
// point now aliases an orphaned array that nothing new will ever share.
type naiveArena struct {
	buf []byte
}

func (n *naiveArena) alloc(size int) []byte {
	start := len(n.buf)
	n.buf = append(n.buf, make([]byte, size)...)
	return n.buf[start : start+size]
}

// basePointer returns the address of a slice's first byte, or nil for an
// empty slice, so tests can compare whether two slices share one array.
func basePointer(b []byte) unsafe.Pointer {
	return unsafe.Pointer(unsafe.SliceData(b))
}

func TestNewRejectsNonPositiveSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1, -16} {
		if _, err := New(size); !errors.Is(err, ErrInvalidSize) {
			t.Errorf("New(%d) error = %v, want ErrInvalidSize", size, err)
		}
	}
}

func TestAlloc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		size    int
		allocs  []int
		wantErr error
	}{
		{name: "single allocation fits", size: 16, allocs: []int{16}},
		{name: "several small allocations fit", size: 16, allocs: []int{4, 4, 4, 4}},
		{name: "zero-length allocation", size: 8, allocs: []int{0, 8}},
		{name: "exact fit then one more byte overflows", size: 8, allocs: []int{8, 1}, wantErr: ErrArenaFull},
		{name: "single allocation larger than arena", size: 4, allocs: []int{5}, wantErr: ErrArenaFull},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, err := New(tc.size)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			var lastErr error
			for _, n := range tc.allocs {
				_, lastErr = a.Alloc(n)
				if lastErr != nil {
					break
				}
			}
			if tc.wantErr == nil && lastErr != nil {
				t.Fatalf("Alloc: unexpected error: %v", lastErr)
			}
			if tc.wantErr != nil && !errors.Is(lastErr, tc.wantErr) {
				t.Fatalf("Alloc error = %v, want %v", lastErr, tc.wantErr)
			}
		})
	}
}

func TestAllocRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	a, err := New(16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Alloc(-1); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(-1) error = %v, want ErrInvalidSize", err)
	}
}

func TestAllocationsShareOneBackingArray(t *testing.T) {
	t.Parallel()

	a, err := New(16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	x, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	y, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}

	// y must begin exactly len(x) bytes after x begins: both are views into
	// the same array, laid out back to back, not two independent buffers.
	gap := uintptr(basePointer(y)) - uintptr(basePointer(x))
	if gap != uintptr(len(x)) {
		t.Fatalf("gap between allocations = %d, want %d (they must share one backing array)", gap, len(x))
	}
}

func TestAllocClipsCapacitySoAppendCannotSpill(t *testing.T) {
	t.Parallel()

	a, err := New(16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	x, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	if cap(x) != len(x) {
		t.Fatalf("cap(x) = %d, want %d (cap must equal len so append reallocates)", cap(x), len(x))
	}

	y, err := a.Alloc(4)
	if err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	before := append([]byte(nil), y...)

	x = append(x, 0xFF, 0xFF, 0xFF, 0xFF) // would spill into y if x's cap exceeded its len
	if !bytes.Equal(y, before) {
		t.Fatalf("appending to x corrupted y: y = %v, want %v", y, before)
	}
	_ = x
}

func TestResetReusesTheSameArray(t *testing.T) {
	t.Parallel()

	a, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := a.Alloc(8)
	if err != nil {
		t.Fatalf("Alloc(8): %v", err)
	}
	if a.Available() != 0 {
		t.Fatalf("Available() = %d, want 0", a.Available())
	}

	a.Reset()
	if a.Available() != 8 {
		t.Fatalf("Available() after Reset = %d, want 8", a.Available())
	}

	second, err := a.Alloc(8)
	if err != nil {
		t.Fatalf("Alloc(8) after Reset: %v", err)
	}
	if basePointer(first) != basePointer(second) {
		t.Fatal("Reset caused Alloc to hand out a different backing array; an arena must reuse its buffer")
	}
}

// TestNaiveArenaSilentlyOrphansEarlierAllocations is the whole point of the
// module: it pins the exact defect an append-based bump allocator ships.
// Two allocations taken while there is still room share one array, exactly
// like the real Arena. The moment a request exceeds the naive arena's
// current capacity, append reallocates onto a new array with no error and
// no signal to the caller -- the invariant "every allocation from this
// generation shares one backing array" silently stops holding, and nothing
// in the naive API tells you it happened.
func TestNaiveArenaSilentlyOrphansEarlierAllocations(t *testing.T) {
	t.Parallel()

	n := &naiveArena{buf: make([]byte, 0, 8)}
	initial := basePointer(n.buf)

	n.alloc(4)
	n.alloc(4) // fills the 8-byte capacity; still within it, so still the same array
	if basePointer(n.buf) != initial {
		t.Fatal("naive arena reallocated within its own capacity; test setup is wrong")
	}

	n.alloc(4) // exceeds capacity: append silently reallocates onto a new array
	if basePointer(n.buf) == initial {
		t.Fatal("naive arena did not reallocate on overflow; test no longer demonstrates the defect")
	}

	// The real Arena, given the same request pattern, refuses instead of
	// silently moving: the caller finds out immediately.
	a, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Alloc(4); err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	if _, err := a.Alloc(4); err != nil {
		t.Fatalf("Alloc(4): %v", err)
	}
	if _, err := a.Alloc(4); !errors.Is(err, ErrArenaFull) {
		t.Fatalf("Alloc(4) over budget: err = %v, want ErrArenaFull", err)
	}
}

// Example demonstrates that two allocations from the same Arena generation
// share one backing array, and that a request past the remaining capacity
// is rejected rather than silently satisfied from a new one.
func Example() {
	a, err := New(16)
	if err != nil {
		panic(err)
	}

	x, err := a.Alloc(8)
	if err != nil {
		panic(err)
	}
	y, err := a.Alloc(4)
	if err != nil {
		panic(err)
	}

	adjacent := uintptr(basePointer(y))-uintptr(basePointer(x)) == uintptr(len(x))
	fmt.Println("second allocation immediately follows the first:", adjacent)

	_, err = a.Alloc(8) // only 4 bytes remain
	fmt.Println("over-budget allocation rejected:", errors.Is(err, ErrArenaFull))

	// Output:
	// second allocation immediately follows the first: true
	// over-budget allocation rejected: true
}
```

## Review

`Alloc` is correct when every allocation it hands out from one generation
provably shares the arena's single backing array, and when a request that
does not fit fails loudly with `ErrArenaFull` instead of quietly growing.
The three-index slice expression `buf[start:used:used]` caps each
allocation's capacity to its own length, so a caller's `append` cannot spill
into the neighbor; `Reset` rewinds the cursor without touching the array, so
the same buffer serves every generation. The trap is reaching for `append`
inside `Alloc` the way you would anywhere else in this lesson: it works
until the buffer is full, and then it reallocates, silently detaching every
already-issued allocation from whatever comes next. `New` validates its size
with `ErrInvalidSize`, checkable with `errors.Is`; `Arena` is explicitly not
safe for concurrent use, and every returned slice aliases the arena's array
until the next `Reset`. `Example` is the executable documentation: `go test`
verifies its output. Run `go test -count=1 -race ./...`.

## Resources

- [`unsafe.SliceData`](https://pkg.go.dev/unsafe#SliceData) — the diagnostic used throughout this module to prove or disprove that two slices share a backing array.
- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the three-index form `a[low:high:max]` used to cap an allocation's capacity to its length.
- [`append`](https://pkg.go.dev/builtin#append) — why it reallocates once `len == cap`, the exact moment a naive arena breaks its own invariant.
- [Pebble: `arenaskl`](https://github.com/cockroachdb/pebble/tree/master/internal/arenaskl) — a production Go bump allocator backing a memtable's skiplist, the real system this exercise's design mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-header-pass-by-value-filter-return.md](15-header-pass-by-value-filter-return.md) | Next: [17-struct-shallow-copy-slice-field.md](17-struct-shallow-copy-slice-field.md)
