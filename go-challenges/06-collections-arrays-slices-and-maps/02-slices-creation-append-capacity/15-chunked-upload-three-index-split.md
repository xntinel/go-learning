# Exercise 15: Splitting an Upload Into Chunks With a Three-Index Slice

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A multipart upload client breaks a large payload into fixed-size chunks and
hands each one to a separate upload goroutine or a separate HTTP part. The
naive way to produce those chunks is `payload[i:j]` for each window, but a
two-index slice's capacity runs all the way to the end of `payload`, not to
`j`. If any chunk consumer appends to its chunk — to add a trailing
checksum, a padding byte, or a part boundary — and that chunk still has
spare capacity because it shares `payload`'s backing array, the append
silently overwrites the first bytes of the *next* chunk, which some other
goroutine may already be reading or uploading. This is the same failure
mode object stores and multipart encoders guard against by copying, except
here the fix costs nothing: a three-index slice.

This module builds the chunker as a package: a `Chunker` constructed once
with a validated chunk size, whose `Split` method never hands out a chunk a
consumer's `append` can corrupt. The naive two-index form never appears in
that method — it lives only in the test file, as an unexported function the
tests use to pin the exact corruption it causes, contrasted against `Split`
on the same input.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
uploadchunk/               module example.com/uploadchunk
  go.mod                   go 1.24
  uploadchunk.go            Chunker; New, Split; one sentinel error
  uploadchunk_test.go       boundary table, cap==len property, append-isolation,
                           concurrency, the splitNaive contrast, ExampleChunker_Split
```

- Files: `uploadchunk.go`, `uploadchunk_test.go`.
- Implement: `New(size int) (*Chunker, error)` rejecting a non-positive size with `ErrInvalidChunkSize`; `(*Chunker).Split(payload []byte) [][]byte`, which walks `payload` in `c.size`-byte windows and returns each window as `payload[i:j:j]` so `cap(chunk) == len(chunk)` for every chunk, including a shorter final chunk when `len(payload)` is not a multiple of `c.size`, and returns `nil` for an empty payload.
- Test: a non-positive size rejected with `errors.Is`; a boundary table (exact multiple, ragged final chunk, payload shorter than one chunk, empty payload, chunk size of one); every chunk having `cap == len`; a consumer `append` onto one chunk unable to change the first byte of the next; `Split` called concurrently from many goroutines on one shared `*Chunker`; a `splitNaive` contrast proving the two-index form lets exactly that corruption happen; and `ExampleChunker_Split` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/uploadchunk
cd ~/go-exercises/uploadchunk
go mod init example.com/uploadchunk
go mod edit -go=1.24
```

### Why a two-index chunk is not a safe hand-off

`payload[i:j]` has `len == j-i`, but its `cap` is `cap(payload) - i` — the
distance from `i` to the *end of payload's backing array*, not to `j`. That
means every chunk except the last one is handed out with hidden spare
capacity: the bytes of the chunks after it. A consumer that does
`chunk = append(chunk, checksumByte)` finds that spare room, writes the
checksum byte into what it believes is its own storage, and has actually
overwritten byte zero of the next chunk. Nothing about the call site looks
wrong; the corruption only shows up later, when the next chunk's data
arrives at its destination with its first byte silently changed, and by
then the goroutine that caused it is long gone. Written out, the naive
version looks entirely reasonable:

```go
func splitNaive(payload []byte, size int) [][]byte {
    var chunks [][]byte
    for i := 0; i < len(payload); i += size {
        j := min(i+size, len(payload))
        chunks = append(chunks, payload[i:j])   // two-index: cap runs past j
    }
    return chunks
}
```

`payload[i:j:j]` fixes this by setting `cap` explicitly to `j-i`, the same
as `len`. Now `len(chunk) == cap(chunk)` for every chunk `Split` returns, so
the very first `append` beyond a chunk's own length finds zero spare room
and is forced to allocate a brand-new backing array before it writes
anything. The chunk's `append` still succeeds — the consumer's code does
not need to know anything changed — but it can no longer reach back into
`payload`. This is the general pattern for handing out any sub-slice of a
buffer you still own to code that might append: cap it at its own length.

Create `uploadchunk.go`:

```go
// Package uploadchunk splits a payload into fixed-size chunks safe to hand
// out to independent consumers -- one per upload goroutine, one per
// multipart part -- without any consumer's append reaching into the bytes
// of the chunk that follows it.
package uploadchunk

import (
	"errors"
	"fmt"
)

// ErrInvalidChunkSize means the configured chunk size was not positive.
var ErrInvalidChunkSize = errors.New("uploadchunk: chunk size must be positive")

// Chunker splits a payload into chunks of a fixed maximum size.
//
// A Chunker is immutable after construction and is safe for concurrent use:
// multiple goroutines may call Split on the same *Chunker at once, each
// call operating only on the payload it was given.
type Chunker struct {
	size int
}

// New returns a Chunker that splits payloads into chunks of at most size
// bytes. It returns ErrInvalidChunkSize if size is not positive.
func New(size int) (*Chunker, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidChunkSize, size)
	}
	return &Chunker{size: size}, nil
}

// Split divides payload into consecutive chunks of at most c.size bytes
// each, using the three-index expression payload[i:j:j] for every chunk.
// Capping capacity at j means a chunk's capacity always equals its length,
// so a consumer that appends to a chunk (say, to add a trailing checksum
// before forwarding it to the next multipart stage) is forced to allocate
// its own backing array instead of writing into the bytes of the chunk
// that follows it in payload. The final chunk may be shorter than c.size
// ("ragged") if len(payload) is not a multiple of c.size. Split returns nil
// for an empty payload and never allocates a backing array itself: every
// chunk slices the same array payload points at.
//
// Aliasing contract: every chunk shares payload's backing array for its
// own bytes -- mutating payload after Split is visible through the chunks,
// and vice versa -- but no chunk's capacity reaches into another chunk's
// bytes, so an append on one chunk can never corrupt another.
func (c *Chunker) Split(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}

	n := (len(payload) + c.size - 1) / c.size
	chunks := make([][]byte, 0, n)
	for i := 0; i < len(payload); i += c.size {
		j := i + c.size
		if j > len(payload) {
			j = len(payload)
		}
		chunks = append(chunks, payload[i:j:j])
	}
	return chunks
}
```

### Using it

Construct one `Chunker` with `New`, sized to whatever bound your upload
protocol imposes — a part-size limit, a network MTU-driven batch size — and
call `Split` per payload. Because a `Chunker` holds no mutable state after
construction, one value can be shared across every caller goroutine without
a mutex, which `TestChunkerIsSafeForConcurrentUse` holds to real concurrency
under `-race`, not just to the type's doc comment. Every chunk `Split`
returns still aliases `payload`'s bytes — mutating `payload` afterward is
visible through the chunks — but no chunk's capacity reaches into another,
so handing different chunks to different consumers is always safe from
cross-chunk corruption, regardless of what each consumer appends.

`ExampleChunker_Split` is the runnable demonstration of this module: `go
test` executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift away from the code.

```go
func ExampleChunker_Split() {
	c, err := New(10)
	if err != nil {
		panic(err)
	}

	payload := make([]byte, 25)
	for i := range payload {
		payload[i] = byte('A' + i%26)
	}

	chunks := c.Split(payload)
	for i, ch := range chunks {
		fmt.Printf("chunk %d: len=%d cap=%d data=%q\n", i, len(ch), cap(ch), ch)
	}

	before := chunks[1][0]
	chunks[0] = append(chunks[0], '!')
	after := chunks[1][0]
	fmt.Printf("chunk 1 first byte before=%q after=%q unchanged=%v\n", before, after, before == after)

	// Output:
	// chunk 0: len=10 cap=10 data="ABCDEFGHIJ"
	// chunk 1: len=10 cap=10 data="KLMNOPQRST"
	// chunk 2: len=5 cap=5 data="UVWXY"
	// chunk 1 first byte before='K' after='K' unchanged=true
}
```

### Tests

`TestSplitBoundaries` is a table over the shapes a chunker has to get
right: an exact multiple, a ragged final chunk, a payload shorter than one
chunk size, an empty payload, and a chunk size of one.
`TestSplitCapsAtChunkLength` asserts every returned chunk has `cap == len`,
the property the three-index expression exists to guarantee.
`TestConsumerAppendDoesNotClobberNextChunk` is the sharpest correctness
test: it splits a 20-byte payload into two 10-byte chunks, appends onto
chunk 0, and asserts chunk 1 is byte-for-byte unchanged.
`TestChunkerIsSafeForConcurrentUse` calls `Split` from 20 goroutines on one
shared `*Chunker`, each with its own payload, holding the type's
concurrency-safety doc comment to a real, race-detected test.

`TestNaiveTwoIndexChunkLetsAppendClobberNextChunk` is the antipattern
contrast. `splitNaive` is unexported and unreachable from the package API;
the test uses it to pin the corruption numerically — `cap(naive[0])`
reaches all the way to `20`, the end of `payload`, not `10`, the end of the
chunk — and then shows a single append actually rewriting `naive[1][0]`,
the exact byte a `payload[i:j:j]` split makes unreachable to that append.

Create `uploadchunk_test.go`:

```go
package uploadchunk

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNewRejectsNonPositiveSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1} {
		if _, err := New(size); !errors.Is(err, ErrInvalidChunkSize) {
			t.Errorf("New(%d) error = %v, want ErrInvalidChunkSize", size, err)
		}
	}
}

func TestSplitBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		size    int
		want    [][]byte
	}{
		{
			name:    "exact multiple splits evenly",
			payload: []byte("0123456789ABCDEFGHIJ"), // 20 bytes
			size:    5,
			want:    [][]byte{[]byte("01234"), []byte("56789"), []byte("ABCDE"), []byte("FGHIJ")},
		},
		{
			name:    "ragged final chunk",
			payload: []byte("0123456789ABC"), // 13 bytes, size 5 -> 5,5,3
			size:    5,
			want:    [][]byte{[]byte("01234"), []byte("56789"), []byte("ABC")},
		},
		{
			name:    "payload shorter than one chunk",
			payload: []byte("ab"),
			size:    10,
			want:    [][]byte{[]byte("ab")},
		},
		{
			name:    "empty payload yields no chunks",
			payload: []byte{},
			size:    5,
			want:    nil,
		},
		{
			name:    "chunk size of one",
			payload: []byte("abc"),
			size:    1,
			want:    [][]byte{[]byte("a"), []byte("b"), []byte("c")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := New(tc.size)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := c.Split(tc.payload)
			if len(got) != len(tc.want) {
				t.Fatalf("Split() returned %d chunks, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if !bytes.Equal(got[i], tc.want[i]) {
					t.Errorf("chunk %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitCapsAtChunkLength(t *testing.T) {
	t.Parallel()

	c, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	chunks := c.Split([]byte("0123456789ABCDEFGHIJ")) // 20 bytes

	for i, ch := range chunks {
		if cap(ch) != len(ch) {
			t.Errorf("chunk %d: cap=%d len=%d, want cap == len (three-index split)", i, cap(ch), len(ch))
		}
	}
}

func TestConsumerAppendDoesNotClobberNextChunk(t *testing.T) {
	t.Parallel()

	c, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := []byte("0123456789ABCDEFGHIJ") // 20 bytes, two chunks of 10
	chunks := c.Split(payload)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	wantChunk1 := append([]byte(nil), chunks[1]...)

	// Because chunk 0 was bounded with the three-index expression,
	// cap(chunk0) == len(chunk0), so this append must allocate a new array
	// rather than writing into payload[10], which is chunk 1's first byte.
	grown := append(chunks[0], '!')

	if !bytes.Equal(chunks[1], wantChunk1) {
		t.Fatalf("chunk 1 was clobbered by an append to chunk 0: got %q, want %q", chunks[1], wantChunk1)
	}
	if string(grown) != "0123456789!" {
		t.Fatalf("grown chunk 0 has unexpected content: %q", grown)
	}
}

// TestChunkerIsSafeForConcurrentUse calls Split on one shared *Chunker from
// many goroutines at once, each with its own payload. Because Split only
// reads c.size and never writes any Chunker field, there is nothing here
// for the race detector to flag.
func TestChunkerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	c, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('a' + i%26)}, 10) // 10 bytes -> 3 chunks of size 4
			if chunks := c.Split(payload); len(chunks) != 3 {
				t.Errorf("goroutine %d: got %d chunks, want 3", i, len(chunks))
			}
		}(i)
	}
	wg.Wait()
}

// splitNaive is the version everyone reaches for first, and the one that
// ships: payload[i:j] instead of the three-index payload[i:j:j]. It is
// never exported and never reachable from Chunker -- Split does not
// contain it.
func splitNaive(payload []byte, size int) [][]byte {
	var chunks [][]byte
	for i := 0; i < len(payload); i += size {
		j := i + size
		if j > len(payload) {
			j = len(payload)
		}
		chunks = append(chunks, payload[i:j])
	}
	return chunks
}

// TestNaiveTwoIndexChunkLetsAppendClobberNextChunk pins the exact defect a
// two-index split ships: chunk 0's capacity runs to the end of payload, not
// to its own length, so an append onto chunk 0 lands inside chunk 1's
// bytes. This is the corruption the three-index form in Split exists to
// prevent -- Split does not contain this code path at all.
func TestNaiveTwoIndexChunkLetsAppendClobberNextChunk(t *testing.T) {
	t.Parallel()

	payload := []byte("0123456789ABCDEFGHIJ") // 20 bytes, two chunks of 10
	naive := splitNaive(payload, 10)
	if len(naive) != 2 {
		t.Fatalf("splitNaive returned %d chunks, want 2", len(naive))
	}
	if cap(naive[0]) != 20 {
		t.Fatalf("cap(naive[0]) = %d, want 20 (runs to the end of payload)", cap(naive[0]))
	}

	_ = append(naive[0], '!') // writes in place: index 10 of the shared array

	if naive[1][0] != '!' {
		t.Fatalf("naive[1][0] = %q, want '!': the append on chunk 0 should have clobbered it", naive[1][0])
	}
}

// ExampleChunker_Split is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below. It splits a 25-byte payload into 10-byte chunks (10, 10, 5 -- a
// ragged last chunk), then appends onto chunk 0 and shows chunk 1's first
// byte is unchanged.
func ExampleChunker_Split() {
	c, err := New(10)
	if err != nil {
		panic(err)
	}

	payload := make([]byte, 25)
	for i := range payload {
		payload[i] = byte('A' + i%26)
	}

	chunks := c.Split(payload)
	for i, ch := range chunks {
		fmt.Printf("chunk %d: len=%d cap=%d data=%q\n", i, len(ch), cap(ch), ch)
	}

	before := chunks[1][0]
	chunks[0] = append(chunks[0], '!')
	after := chunks[1][0]
	fmt.Printf("chunk 1 first byte before=%q after=%q unchanged=%v\n", before, after, before == after)

	// Output:
	// chunk 0: len=10 cap=10 data="ABCDEFGHIJ"
	// chunk 1: len=10 cap=10 data="KLMNOPQRST"
	// chunk 2: len=5 cap=5 data="UVWXY"
	// chunk 1 first byte before='K' after='K' unchanged=true
}
```

## Review

`Split` is correct when every returned chunk has `cap == len` and the
ragged final chunk is exactly the leftover bytes — both are asserted
directly by `TestSplitCapsAtChunkLength` and `TestSplitBoundaries`. The
property that actually matters in production is the one
`TestConsumerAppendDoesNotClobberNextChunk` proves: no `append` a consumer
performs on one chunk can reach into another chunk's bytes, because the
three-index expression `payload[i:j:j]` leaves each chunk with zero spare
capacity. `TestNaiveTwoIndexChunkLetsAppendClobberNextChunk` pins the
alternative: swap that for the more natural-looking `payload[i:j]` and the
whole boundary table above would still pass — the bug is only observable
once a consumer appends, which is exactly why it survives code review so
often. `New` rejects a non-positive chunk size with `ErrInvalidChunkSize`,
checkable with `errors.Is`, and `Chunker` is immutable after construction,
so `TestChunkerIsSafeForConcurrentUse` can share one instance across
goroutines and still pass under `-race`. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [Slice expressions — the Go spec](https://go.dev/ref/spec#Slice_expressions) — defines the three-index form and its capacity rule.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — how `append` decides whether to reallocate.
- [`bytes` package](https://pkg.go.dev/bytes) — used by the tests to compare chunk contents.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-zero-alloc-scratch-field-splitter.md](14-zero-alloc-scratch-field-splitter.md) | Next: [16-bounded-queue-drop-oldest-vs-reject.md](16-bounded-queue-drop-oldest-vs-reject.md)
