# Exercise 10: Correlation ID Cache Pinning a Whole Frame Buffer

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An ingest pipeline that reads audit records, request frames, or Kafka
messages off the wire usually indexes each one by a short identifier pulled
out of a fixed position within it: a Kubernetes API server correlating audit
events by request ID, a log shipper extracting a trace ID header from a
multi-kilobyte record. The frame itself can be large -- several kilobytes is
routine -- but the identifier the service actually wants to keep around is
sixteen or twenty bytes. The natural way to extract it, `id := frame[a:b]`,
looks like it does exactly that: a short slice, ready to cache.

It does not. A sub-slice of `frame` shares `frame`'s backing array and
inherits its capacity all the way to the end of that array, not just to the
end of the sixteen bytes the caller cares about. Cache that sub-slice in a
map keyed by itself, or in any structure that outlives the read loop, and
every one of those "short" IDs keeps the entire multi-kilobyte frame it came
from reachable through the garbage collector's eyes, for as long as the ID
stays cached. A service that processes a few hundred frames a second and
caches their IDs for an hour is not holding a few hundred kilobytes of
identifiers; it is holding the full run of frames, megabytes deep, disguised
as a small index. This is capacity pinning, and it is invisible in every
test that only checks the ID's *content* -- the bug is entirely in what the
slice header still points at.

This module builds `idindex`, the correct extraction as a package you take
elsewhere: it copies the identifier out with `slices.Clone`-equivalent logic
before it is ever stored, so a cached ID's capacity is its own sixteen
bytes, never the frame's. The pinning version never appears in the package;
it lives only in the test file, as the thing the tests prove wrong.

The reason this bug survives code review so often is that the sub-slice
expression itself is unremarkable -- `frame[10:26]` is the standard, correct
way to read four bytes at an offset, and nothing about reading it in
isolation looks dangerous. The danger is entirely a property of what happens
to the result afterward: a value that is read once and discarded never pins
anything, because the frame it came from becomes unreachable the moment the
read loop moves to the next iteration. It is specifically the act of
*storing* a raw sub-slice past the point where its source would otherwise be
collected -- in a cache, an index, a deduplication set -- that turns an
ordinary slice expression into a long-lived reference to memory nobody
meant to keep.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
idindex/                 module example.com/idindex
  go.mod                 go 1.24
  idindex.go             ID, Index; New, Track, Lookup, Len
  idindex_test.go        extraction table, dedup, buffer-reuse survival,
                         String, the capacity-pinning contrast, ExampleIndex_Track
```

- Files: `idindex.go`, `idindex_test.go`.
- Implement: `New() *Index`; `(*Index).Track(frame []byte) ID`, which extracts the correlation ID at the fixed `[IDOffset:IDOffset+IDLen)` window (clamped to whatever of `frame` exists past `IDOffset`), records it, and returns an owned copy; `(*Index).Lookup(id ID) bool`; `(*Index).Len() int`; `ID.String() string`.
- Test: the extraction table (long frame, frame exactly at the boundary, short frame, empty frame, nil frame); dedup via `Len`; an ID surviving the caller reusing its read buffer for the next frame; `String`'s hex rendering; the capacity-pinning contrast against an unexported `trackNaive`; the copy's independence from later frame mutation; and `ExampleIndex_Track` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/idindex
cd ~/go-exercises/idindex
go mod init example.com/idindex
go mod edit -go=1.24
```

### A sub-slice's capacity reaches to the end of the array it came from

`frame[10:26]` produces a slice with length 16, but its capacity is
`cap(frame) - 10` -- everything from index 10 to the end of `frame`'s
backing array, not just the 16 bytes the expression's own length suggests.
That capacity is not cosmetic: as long as any variable holds that slice
header, the garbage collector must keep the *entire* backing array alive,
because the header's pointer lands inside it and Go's collector works at the
granularity of whole allocations, not the sub-range a length happens to
describe. The naive extraction stores exactly that sub-slice:

```go
func trackNaive(frame []byte) ID {
    return frame[10:26] // len 16, but cap reaches to the end of frame
}
```

Nothing about this line is wrong in isolation -- `frame[10:26]` is a
perfectly ordinary slice expression. The bug is entirely about what happens
next: the caller stores the result in a long-lived index, and from that
moment the multi-kilobyte `frame` it was read from can never be collected,
because the sixteen-byte-looking `ID` is secretly still pointing into it.
The fix is to copy the window into its own, exactly-sized backing array
before it leaves the function that extracted it -- so the returned value's
capacity equals its length, and the original frame becomes collectable the
moment the caller is done with it.

Create `idindex.go`:

```go
// Package idindex tracks which correlation IDs have been seen among frames
// read off an ingest pipeline, without pinning each frame's full backing
// array in memory once its ID has been cached.
package idindex

import "fmt"

// IDOffset and IDLen describe where a correlation ID lives within a frame:
// bytes [IDOffset : IDOffset+IDLen).
const (
	IDOffset = 10
	IDLen    = 16
)

// ID is a correlation identifier copied out of a frame. It owns its storage:
// unlike a raw sub-slice of the frame it came from, an ID never keeps a
// larger buffer reachable through its capacity.
type ID []byte

// String renders id as a lowercase hex string, suitable for a log line.
func (id ID) String() string {
	return fmt.Sprintf("%x", []byte(id))
}

// Index tracks which correlation IDs have been recorded from ingested
// frames.
//
// Index is not safe for concurrent use; the caller must synchronize calls to
// Track and Lookup across goroutines, for example with a mutex around the
// ingest loop.
type Index struct {
	seen map[string]struct{}
}

// New returns an empty Index.
func New() *Index {
	return &Index{seen: make(map[string]struct{})}
}

// Track extracts the correlation ID at the fixed offset within frame,
// records it, and returns a copy as an ID.
//
// A frame shorter than IDOffset+IDLen is not an error: the extracted window
// clamps to whatever of frame exists past IDOffset, including an empty ID
// for a frame no longer than IDOffset. The returned ID is a freshly
// allocated copy -- it never aliases frame's backing array, so caching it
// does not keep the rest of frame's (typically multi-kilobyte) buffer
// reachable.
func (x *Index) Track(frame []byte) ID {
	start := min(IDOffset, len(frame))
	end := min(start+IDLen, len(frame))

	id := make(ID, end-start)
	copy(id, frame[start:end])
	x.seen[string(id)] = struct{}{}
	return id
}

// Lookup reports whether id was produced by a previous call to Track.
func (x *Index) Lookup(id ID) bool {
	_, ok := x.seen[string(id)]
	return ok
}

// Len reports how many distinct correlation IDs have been recorded.
func (x *Index) Len() int {
	return len(x.seen)
}
```

### Using it

`Index` is built once and shared for the lifetime of the ingest loop: call
`Track` for every frame you read, and `Lookup` whenever you need to check
whether a correlation ID has already been seen. The type carries mutable
state, so its doc comment is explicit that it is not safe for concurrent
use -- a multi-worker ingest pipeline needs its own mutex around a shared
`Index`, or one `Index` per worker.

The contract on `Track` is the whole point of the module: the returned `ID`
never aliases the `frame` it came from. That is what lets a caller cache
thousands of IDs without also, invisibly, caching thousands of full frames.
`ExampleIndex_Track` is this module's runnable demonstration: `go test`
executes it and checks its `// Output:` block, so the behavior shown below
cannot drift from the code that produces it.

```go
func ExampleIndex_Track() {
	frame := make([]byte, 4096)
	copy(frame[IDOffset:IDOffset+IDLen], []byte("correlation-id-1"))

	idx := New()
	id := idx.Track(frame)

	fmt.Println(idx.Lookup(id))
	fmt.Println(len(id), cap(id))
	fmt.Println(cap(id) < len(frame))

	// Output:
	// true
	// 16 16
	// true
}
```

`cap(id)` equals its own length, 16, not `cap(frame) - IDOffset`, which
would be well over four thousand. That single line is the difference
between an index that scales with the number of distinct IDs and one that
secretly scales with the total bytes ever read.

### Tests

`TestTrackTable` sweeps the extraction window across the boundaries that
matter: a frame comfortably longer than the window, one exactly at
`IDOffset+IDLen`, one shorter than that (which must clamp instead of
panicking), one shorter than `IDOffset` itself, an empty frame, and a nil
frame -- all producing a non-nil `ID` of the expected length.
`TestLenCountsDistinctIDs` checks that tracking the same frame twice does
not inflate the index. `TestTrackSurvivesReadBufferReuse` mirrors the real
ingest loop pattern -- reading each frame into one pooled buffer and
overwriting it for the next read -- and confirms an already-cached ID is
unaffected by that reuse, which only holds because `Track` copies.

`TestTrackDoesNotPinFrameCapacity` is the heart of the module: `trackNaive`
is unexported and unreachable from the package API, and exists only so the
test can state the pinning defect numerically -- the naive extraction's
capacity equals `cap(frame) - IDOffset`, while `Track`'s equals `IDLen`, and
the test asserts the correct one is strictly smaller than the naive one.
`TestTrackCopyIsIndependentOfFrame` confirms that small capacity is a real
copy and not a coincidence, by overwriting `frame` after extraction and
checking the cached `ID` is unchanged.

Create `idindex_test.go`:

```go
package idindex

import (
	"fmt"
	"testing"
)

// trackNaive is the antipattern this module warns about: it stores the raw
// sub-slice of frame directly instead of copying out of it. The returned ID
// shares frame's backing array and inherits its capacity all the way to the
// end of frame, so caching this "short" ID keeps the entire multi-kilobyte
// frame reachable for as long as the ID stays cached.
func trackNaive(frame []byte) ID {
	end := min(IDOffset+IDLen, len(frame))
	start := min(IDOffset, len(frame))
	return frame[start:end]
}

func TestTrackTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		frame   []byte
		wantLen int
	}{
		{name: "long frame", frame: make([]byte, 4096), wantLen: IDLen},
		{name: "frame exactly offset+len", frame: make([]byte, IDOffset+IDLen), wantLen: IDLen},
		{name: "frame shorter than offset+len", frame: make([]byte, IDOffset+4), wantLen: 4},
		{name: "frame shorter than offset", frame: make([]byte, 3), wantLen: 0},
		{name: "empty frame", frame: []byte{}, wantLen: 0},
		{name: "nil frame", frame: nil, wantLen: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx := New()
			id := idx.Track(tc.frame)
			if len(id) != tc.wantLen {
				t.Fatalf("len(id) = %d, want %d", len(id), tc.wantLen)
			}
			if id == nil && tc.wantLen == 0 {
				t.Fatalf("Track returned a nil ID for %q; want non-nil empty", tc.name)
			}
			if !idx.Lookup(id) {
				t.Fatalf("Lookup(id) = false right after Track")
			}
		})
	}
}

func TestLookupUnknownID(t *testing.T) {
	t.Parallel()

	idx := New()
	if idx.Lookup(ID("never-tracked--")) {
		t.Fatal("Lookup reported true for an ID that was never tracked")
	}
}

func TestLenCountsDistinctIDs(t *testing.T) {
	t.Parallel()

	idx := New()
	frameA := make([]byte, 4096)
	copy(frameA[IDOffset:], []byte("id-a------------"))
	frameB := make([]byte, 4096)
	copy(frameB[IDOffset:], []byte("id-b------------"))

	idx.Track(frameA)
	idx.Track(frameA) // same ID again: must not double-count
	idx.Track(frameB)

	if got := idx.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

// TestTrackSurvivesReadBufferReuse mirrors how an ingest loop actually calls
// Track: it reads each frame into the same pooled buffer and reuses it for
// the next read. If Track ever stored a sub-slice of that buffer directly,
// every cached ID would silently change as soon as the buffer was
// overwritten by the next frame -- this pins that it does not.
func TestTrackSurvivesReadBufferReuse(t *testing.T) {
	t.Parallel()

	idx := New()
	buf := make([]byte, 4096)

	copy(buf[IDOffset:], []byte("frame-one-------"))
	firstID := idx.Track(buf)

	for i := range buf {
		buf[i] = 0 // simulate the pipeline reusing the buffer for the next read
	}
	copy(buf[IDOffset:], []byte("frame-two-------"))
	secondID := idx.Track(buf)

	if !idx.Lookup(firstID) {
		t.Error("Lookup(firstID) = false after the buffer was reused for a second frame")
	}
	if !idx.Lookup(secondID) {
		t.Error("Lookup(secondID) = false")
	}
	if string(firstID) == string(secondID) {
		t.Fatalf("firstID and secondID are equal (%q); the buffer reuse corrupted firstID", firstID)
	}
}

func TestIDString(t *testing.T) {
	t.Parallel()

	id := ID{0xde, 0xad, 0xbe, 0xef}
	if got, want := id.String(), "deadbeef"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestTrackDoesNotPinFrameCapacity is the heart of the module. trackNaive
// reproduces the bug that ships when Track stores frame[a:b] directly: the
// returned ID's capacity reaches to the end of frame, not just to its own
// length, so retaining the ID retains the whole frame. Index.Track must not
// exhibit that: its ID's capacity equals its own length.
func TestTrackDoesNotPinFrameCapacity(t *testing.T) {
	t.Parallel()

	frame := make([]byte, 4096)
	copy(frame[IDOffset:IDOffset+IDLen], []byte("correlation-id-1"))

	naive := trackNaive(frame)
	if got, want := cap(naive), cap(frame)-IDOffset; got != want {
		t.Fatalf("cap(naive) = %d, want %d (it must alias frame's backing array)", got, want)
	}

	idx := New()
	id := idx.Track(frame)
	if cap(id) != IDLen {
		t.Fatalf("cap(id) = %d, want %d", cap(id), IDLen)
	}
	if cap(id) >= cap(naive) {
		t.Fatalf("cap(id) = %d, want it far below the naive capacity %d", cap(id), cap(naive))
	}
}

// TestTrackCopyIsIndependentOfFrame proves the returned ID does not merely
// have a small capacity by coincidence, but genuinely owns its bytes:
// mutating frame after Track must not change the cached ID.
func TestTrackCopyIsIndependentOfFrame(t *testing.T) {
	t.Parallel()

	frame := make([]byte, 4096)
	copy(frame[IDOffset:IDOffset+IDLen], []byte("correlation-id-1"))

	idx := New()
	id := idx.Track(frame)
	want := string(id)

	for i := range frame {
		frame[i] = 0xff
	}

	if string(id) != want {
		t.Fatalf("id changed after mutating frame: got %q, want %q", string(id), want)
	}
	if !idx.Lookup(id) {
		t.Fatal("Lookup(id) = false after frame was overwritten")
	}
}

// ExampleIndex_Track demonstrates that a cached ID's capacity collapses to
// its own IDLen bytes -- far below the length of the frame it came from --
// instead of pinning the frame's full backing array.
func ExampleIndex_Track() {
	frame := make([]byte, 4096)
	copy(frame[IDOffset:IDOffset+IDLen], []byte("correlation-id-1"))

	idx := New()
	id := idx.Track(frame)

	fmt.Println(idx.Lookup(id))
	fmt.Println(len(id), cap(id))
	fmt.Println(cap(id) < len(frame))

	// Output:
	// true
	// 16 16
	// true
}
```

## Review

`Track` is correct when a cached `ID`'s capacity equals its own length,
never the frame's -- that is the property `TestTrackDoesNotPinFrameCapacity`
pins, contrasted against the unexported `trackNaive`, which is never part of
the package's exported surface. The mechanism is copying the extraction
window into a freshly allocated array before it ever leaves `Track`, rather
than returning a sub-slice of the caller's own frame. That single decision
is what lets an ingest pipeline cache millions of correlation IDs over its
lifetime while keeping memory proportional to the number of *distinct* IDs,
not the total bytes it has ever read. Around that core, `Index` handles a
frame shorter than the ID window by clamping instead of panicking, survives
the caller reusing its read buffer for the next frame, and documents plainly
that it is not safe for concurrent use. Run `go test -count=1 -race ./...`
to confirm the extraction table, the buffer-reuse survival, the
capacity-pinning contrast, and `ExampleIndex_Track`.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the standard-library operation this module's manual copy performs, useful when the source's type is already generic.
- [Go Spec: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — how a sub-slice's capacity is derived from the array it was sliced from, not from the slice expression's own length.
- [`unsafe.SliceData`](https://pkg.go.dev/unsafe#SliceData) — the diagnostic for confirming two slices do or do not share a backing array.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — canonical patterns for copying out of, rather than aliasing, a larger buffer.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-two-dim-shared-row.md](09-two-dim-shared-row.md) | Next: [11-scatter-gather-fixed-index-results.md](11-scatter-gather-fixed-index-results.md)
