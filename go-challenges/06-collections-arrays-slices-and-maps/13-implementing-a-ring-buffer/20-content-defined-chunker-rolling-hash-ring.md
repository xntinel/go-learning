# Exercise 20: Content-Defined Chunking with an O(1) Rolling-Hash Window

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every ring buffer earlier in this lesson stores whole values you later
retrieve. A rolling-hash window uses the same fixed-size, evict-the-oldest
shape for a different purpose entirely: it never gives back what it holds,
it only ever answers "what does a cheap summary of my current contents
look like?" -- and it answers that in O(1) time per byte, because a ring
buffer always knows which single element is about to be evicted. That is
the mechanism behind content-defined chunking, the technique restic,
rsync, and most deduplicating backup tools use to split a byte stream:
slide a small window across the stream, maintain a rolling checksum of its
contents, and cut a boundary wherever that checksum matches a mask. Two
files sharing a long common region produce mostly the same chunks even if
bytes were inserted earlier in one of them, because cut points are
determined by local content, not a fixed byte offset -- unlike naive
length-based chunking, where one inserted byte shifts every boundary and
destroys every chunk's dedup match downstream.

The ring buffer's contribution here is easy to skip past and expensive to
skip past: an update to a rolling checksum only needs the one byte leaving
the window and the one byte entering it, `newSum = oldSum - evicted +
incoming`, an O(1) operation regardless of window size. The version most
first attempts write instead re-sums the entire window on every byte,
because summing a slice is the obvious, correct-looking thing to do -- and
it is correct, just quadratic: O(window size) work per byte instead of
O(1), turning a linear scan of the input into work proportional to input
length times window length. A ring buffer already tracks exactly which
byte is about to be evicted, for free, as a side effect of being a
fixed-capacity circular array -- that is the whole reason to reach for one
here.

This exercise builds `cdcchunk`, a command-line tool that reads a byte
stream and prints one line per chunk boundary. The from-scratch resum
never appears in the tool's own code path; it exists only in the test
file, contrasted against the incremental update it replaces.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
cdcchunk/                    module example.com/cdcchunk
  go.mod                     go 1.24
  cdcchunk.go                package main — Chunker, NewChunker, Feed, Pending; rollingUpdate
  cdcchunk_test.go           package main — size-bounds table, incremental-vs-from-scratch
                              contrast, run() end to end
  main.go                    package main — -window/-mask/-min/-max flags, exit codes
```

- Files: `cdcchunk.go`, `cdcchunk_test.go`, `main.go`.
- Implement: `NewChunker(windowSize int, mask uint64, minSize, maxSize int) (*Chunker, error)` rejecting a non-positive window with `ErrInvalidWindow` and an invalid `[minSize,maxSize]` range with `ErrInvalidSizes`; `(*Chunker).Feed(b byte) bool` reporting whether the byte completes a chunk; `(*Chunker).Pending() int` for the trailing partial chunk at EOF.
- Tool: `cdcchunk` reads stdin or a file named as its last argument and streams it byte by byte through a `Chunker`, printing `chunk N: offset=X len=Y` for each boundary and any final partial chunk at EOF. Flags `-window`, `-mask` (a `0x`-prefixed hex string), `-min`, and `-max` configure the chunker. Exit 0 on success, exit 2 for a bad flag, an unparseable `-mask`, or a rejected window/size configuration, exit 1 for a runtime failure such as a missing input file.
- Test: rejected `NewChunker` configurations; the mandatory `maxSize` cut and the `minSize` floor pinned in isolation; the from-scratch resum contrasted against the O(1) incremental update, pinning that their sums always agree while the incremental update performs strictly fewer total operations; `run` end to end over `strings.Reader` and `bytes.Buffer`, including a missing-file runtime failure that must not be reported as a usage error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/20-content-defined-chunker-rolling-hash-ring
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/20-content-defined-chunker-rolling-hash-ring
go mod edit -go=1.24
```

### O(1) eviction versus resumming the window

A ring buffer's defining operation is that pushing a new element and
evicting the oldest happen together, at one known index, in one step. A
rolling checksum exploits that directly: the sum over the window's current
contents changes by exactly the delta between the byte leaving and the byte
arriving, so maintaining it costs one subtraction and one addition no
matter how large the window is:

```go
func rollingUpdate(sum uint64, evicted, incoming byte) (newSum uint64, ops int) {
	return sum - uint64(evicted) + uint64(incoming), 2
}
```

The version that looks equally correct and costs far more re-derives the
sum from the window's contents on every byte:

```go
// The version most first drafts write: correct, but it re-walks the
// entire window on every byte instead of using the one byte the ring
// already knows is leaving.
func windowSumFromScratch(window []byte) (sum uint64, ops int) {
	for _, b := range window {
		sum += uint64(b)
		ops++
	}
	return sum, ops
}
```

Both compute the same number. The difference is that `rollingUpdate` does
two additions total per byte fed, while `windowSumFromScratch` does one
addition *per byte currently in the window*, per byte fed -- for a stream
of length N and a window of size W, that is O(N) total work for the
incremental version against O(N·W) for the from-scratch one. Nobody notices
on a short test file; a multi-gigabyte backup source with a 64-byte window
is a different story. A chunk that never hits a matching checksum still
must not grow without bound, so `Feed` forces a cut at `maxSize` regardless
of the rolling sum.

Create `cdcchunk.go`:

```go
// Command cdcchunk splits a byte stream into content-defined chunks the way
// restic and rsync do: a small sliding window of the most recent bytes
// feeds a rolling checksum, and a boundary falls wherever that checksum
// matches a mask -- so an insertion or deletion elsewhere in the stream
// only shifts nearby boundaries, instead of reshuffling every fixed-size
// block the way naive length-based chunking would. That is the basis of
// backup deduplication: two files sharing a long region produce mostly the
// same chunks even if bytes were added earlier in one of them.
//
// The ring buffer's job here is not storage: it turns "recompute the
// checksum over the whole window" into "update it by the one byte that
// just left." See cdcchunk_test.go for the difference that makes.
package main

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by NewChunker.
var (
	ErrInvalidWindow = errors.New("cdcchunk: window size must be positive") // windowSize was not positive
	ErrInvalidSizes  = errors.New("cdcchunk: require 1 <= minSize <= maxSize")
)

// rollingUpdate computes the new window sum after evicted leaves the
// window and incoming enters it. It is O(1) regardless of window size: one
// subtraction, one addition. ops is returned only so tests can pin that
// count against the from-scratch alternative.
func rollingUpdate(sum uint64, evicted, incoming byte) (newSum uint64, ops int) {
	return sum - uint64(evicted) + uint64(incoming), 2
}

// Chunker splits a byte stream into content-defined chunks by feeding each
// byte through a fixed-size sliding window and cutting whenever the
// window's rolling sum matches mask, subject to a minimum and a mandatory
// maximum chunk length.
//
// Concurrency contract: NOT safe for concurrent use. A Chunker holds
// mutable per-stream state and is meant to be driven by a single goroutine
// reading one stream in order.
type Chunker struct {
	window  []byte
	pos     int
	filled  int
	sum     uint64
	mask    uint64
	minSize int
	maxSize int
	curLen  int
}

// NewChunker returns a Chunker with the given window size, cut mask, and
// chunk length bounds, or ErrInvalidWindow / ErrInvalidSizes.
func NewChunker(windowSize int, mask uint64, minSize, maxSize int) (*Chunker, error) {
	if windowSize <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidWindow, windowSize)
	}
	if minSize < 1 || maxSize < minSize {
		return nil, fmt.Errorf("%w: got min=%d max=%d", ErrInvalidSizes, minSize, maxSize)
	}
	return &Chunker{
		window:  make([]byte, windowSize),
		mask:    mask,
		minSize: minSize,
		maxSize: maxSize,
	}, nil
}

// Feed processes one byte of the stream and reports whether it completes a
// chunk: either the mandatory cut at maxSize, or a content-defined cut once
// the window is full, the chunk has reached minSize, and the rolling sum
// matches mask. The window keeps sliding across chunk boundaries; only the
// current chunk's length resets.
func (c *Chunker) Feed(b byte) bool {
	evicted := c.window[c.pos]
	c.window[c.pos] = b
	c.pos = (c.pos + 1) % len(c.window)
	if c.filled < len(c.window) {
		c.filled++
	}
	c.sum, _ = rollingUpdate(c.sum, evicted, b)
	c.curLen++
	if c.curLen >= c.maxSize {
		c.curLen = 0
		return true
	}
	if c.filled == len(c.window) && c.curLen >= c.minSize && c.sum&c.mask == 0 {
		c.curLen = 0
		return true
	}
	return false
}

// Pending reports the length of bytes accumulated since the last boundary,
// i.e. what an EOF must still flush as a final, shorter chunk.
func (c *Chunker) Pending() int { return c.curLen }
```

### The tool

`cdcchunk` has one job: turn a byte stream into a list of chunk boundaries.
`run` takes the argument slice plus an `io.Reader` for input and an
`io.Writer` for output, never touching `os.Stdin`, `os.Stdout`, or
`os.Exit` directly, so a test can drive it with a `strings.Reader` and a
`bytes.Buffer`. It reads one byte at a time with `bufio.Reader.ReadByte`
instead of loading the whole input into memory -- streaming is the point
of a bounded-window algorithm, and buffering the input first would defeat
it on exactly the large files this tool is for. Every input mistake -- a
bad flag, an unparseable `-mask`, a rejected size configuration -- wraps
`errUsage`, mapped to exit code 2; a missing input file is a runtime
failure and maps to exit code 1 instead.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

// errUsage marks a failure the caller can fix by changing the command
// line: a bad flag, or window/size bounds NewChunker rejects. main maps it
// to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, streams stdin (or a file named by the last argument)
// through a Chunker one byte at a time, and writes one line per chunk
// boundary to stdout: "chunk N: offset=X len=Y". It never touches
// os.Stdin or os.Exit directly, so it can be driven in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("cdcchunk", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	window := fs.Int("window", 16, "sliding window size in bytes")
	maskStr := fs.String("mask", "0x0f", "cut mask, matched against the window's rolling sum")
	minSize := fs.Int("min", 16, "minimum chunk length in bytes")
	maxSize := fs.Int("max", 64, "maximum (mandatory-cut) chunk length in bytes")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	mask, err := strconv.ParseUint(*maskStr, 0, 64)
	if err != nil {
		return fmt.Errorf("%w: -mask %q: %v", errUsage, *maskStr, err)
	}
	chunker, err := NewChunker(*window, mask, *minSize, *maxSize)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	in := stdin
	if rest := fs.Args(); len(rest) > 0 {
		f, err := os.Open(rest[0])
		if err != nil {
			return fmt.Errorf("open %s: %w", rest[0], err)
		}
		defer f.Close()
		in = f
	}
	r := bufio.NewReader(in)
	var offset, chunkNum, chunkStart int
	for {
		b, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if chunker.Feed(b) {
			chunkNum++
			fmt.Fprintf(stdout, "chunk %d: offset=%d len=%d\n", chunkNum, chunkStart, offset-chunkStart+1)
			chunkStart = offset + 1
		}
		offset++
	}
	if chunker.Pending() > 0 {
		chunkNum++
		fmt.Fprintf(stdout, "chunk %d: offset=%d len=%d\n", chunkNum, chunkStart, chunker.Pending())
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cdcchunk [-window N] [-mask 0xNN] [-min N] [-max N] [file]")
		fmt.Fprintln(os.Stderr, "splits stdin, or file, into content-defined chunks and prints their offsets and lengths.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cdcchunk:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'the quick brown fox jumps over the lazy dog while the wizard quickly vexed jack' | go run . -window 4 -mask 0x07 -min 4 -max 16
printf 'abc' | go run . -mask not-hex
```

Expected output:

```text
chunk 1: offset=0 len=11
chunk 2: offset=11 len=15
chunk 3: offset=26 len=13
chunk 4: offset=39 len=8
chunk 5: offset=47 len=16
chunk 6: offset=63 len=6
chunk 7: offset=69 len=4
chunk 8: offset=73 len=6
```

```text
cdcchunk: usage: -mask "not-hex": strconv.ParseUint: parsing "not-hex": invalid syntax
```

The first run shows content-defined chunking on ordinary English text: an
82-byte pangram splits into eight irregular chunks -- 11, 15, 13, 8, 16, 6,
4, and 6 bytes -- none of them the fixed-size blocks a length-based
splitter would produce, each boundary determined by where the 4-byte
window's rolling sum satisfied `sum&0x07==0`. The second run shows the
exit-2 usage path: an unparseable `-mask` never reaches `NewChunker`,
failing at flag validation with `strconv.ParseUint`'s error wrapped under
`run`'s own `errUsage`.

### Tests

`TestFeedRespectsSizeBounds` isolates the two length bounds from the
content-defined cut condition by choosing masks that can only ever satisfy
one bound: a mask no rolling sum can match leaves only the mandatory
`maxSize` to produce cuts, and a mask of zero -- matching every sum --
leaves only `minSize` standing between it and cutting on every byte.
`TestIncrementalSumMatchesFromScratchWithFewerOps` is this module's
antipattern contrast: it drives 500 pseudo-random bytes through both
`rollingUpdate` and `windowSumFromScratch` in lockstep, asserting the sums
agree at every step -- the O(1) update is not an approximation -- while the
incremental version's running operation count stays strictly below the
from-scratch version's, a property rather than a hand-picked number, since
the exact gap depends on the window size chosen here, not on the algorithm.
`TestRunEndToEnd` drives the command over `strings.Reader`/`bytes.Buffer`: a
below-minimum input flushed whole at EOF, a stream forcing two mandatory
cuts, an empty input, and three usage errors.
`TestRunRejectsMissingFileAsRuntimeNotUsage` confirms a missing input file
wraps a plain error, not `errUsage` -- the exit-1-versus-exit-2 distinction
the tool's contract promises.

Create `cdcchunk_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

func TestNewChunkerRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		window, min, max int
		want             error
	}{
		{name: "zero window", window: 0, min: 4, max: 8, want: ErrInvalidWindow},
		{name: "negative window", window: -1, min: 4, max: 8, want: ErrInvalidWindow},
		{name: "zero min", window: 4, min: 0, max: 8, want: ErrInvalidSizes},
		{name: "max below min", window: 4, min: 8, max: 4, want: ErrInvalidSizes},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewChunker(tc.window, 0xff, tc.min, tc.max); !errors.Is(err, tc.want) {
				t.Errorf("NewChunker(%d,_,%d,%d) err = %v, want %v", tc.window, tc.min, tc.max, err, tc.want)
			}
		})
	}
}

// TestFeedRespectsSizeBounds pins both length bounds in isolation: a mask
// that can never match leaves only maxSize to force cuts, and a mask of 0
// (matches every sum) leaves only minSize to block cutting on every byte.
func TestFeedRespectsSizeBounds(t *testing.T) {
	t.Parallel()

	t.Run("mandatory max forces a cut", func(t *testing.T) {
		t.Parallel()
		c, err := NewChunker(4, 0xffffffff, 6, 10)
		if err != nil {
			t.Fatalf("NewChunker: %v", err)
		}
		var cuts []int
		for i := 0; i < 25; i++ {
			if c.Feed(byte(i)) {
				cuts = append(cuts, i+1)
			}
		}
		want := []int{10, 20}
		if len(cuts) != len(want) || cuts[0] != want[0] || cuts[1] != want[1] {
			t.Fatalf("cuts = %v, want %v", cuts, want)
		}
	})

	t.Run("min blocks an always-matching mask", func(t *testing.T) {
		t.Parallel()
		const minSize = 8
		c, err := NewChunker(4, 0, minSize, 1000)
		if err != nil {
			t.Fatalf("NewChunker: %v", err)
		}
		first := -1
		for i := 0; i < 30; i++ {
			if c.Feed(byte(i)) {
				first = i + 1
				break
			}
		}
		if first < minSize {
			t.Fatalf("first cut at length %d, want >= minSize %d", first, minSize)
		}
	})
}

// windowSumFromScratch re-sums every byte in the window: the version most
// first drafts write, correct but redoing work the ring already avoids,
// since it knows exactly which byte is leaving each step. Never reachable
// from Chunker's API.
func windowSumFromScratch(window []byte) (sum uint64, ops int) {
	for _, b := range window {
		sum += uint64(b)
		ops++
	}
	return sum, ops
}

// TestIncrementalSumMatchesFromScratchWithFewerOps is the heart of the
// module: drive a byte stream through both the O(1) rollingUpdate and a
// from-scratch resum after every byte, and pin that the sums always agree
// while the incremental update needed strictly fewer total operations --
// a property, not a hand-picked count that would depend on window size.
func TestIncrementalSumMatchesFromScratchWithFewerOps(t *testing.T) {
	t.Parallel()
	const windowSize = 32
	rng := rand.New(rand.NewSource(1))
	stream := make([]byte, 500)
	rng.Read(stream)

	window := make([]byte, windowSize)
	pos := 0
	var incrementalSum uint64
	var incrementalOps, fromScratchOps int

	for _, b := range stream {
		evicted := window[pos]
		window[pos] = b
		pos = (pos + 1) % windowSize

		var ops int
		incrementalSum, ops = rollingUpdate(incrementalSum, evicted, b)
		incrementalOps += ops

		scratchSum, sOps := windowSumFromScratch(window)
		fromScratchOps += sOps

		if incrementalSum != scratchSum {
			t.Fatalf("incremental sum %d != from-scratch sum %d after feeding %v", incrementalSum, scratchSum, b)
		}
	}
	if !(incrementalOps < fromScratchOps) {
		t.Fatalf("ops: incremental=%d fromScratch=%d, want incremental < fromScratch", incrementalOps, fromScratchOps)
	}
}

func TestRollingUpdateIsConstantTwoOps(t *testing.T) {
	t.Parallel()
	if _, ops := rollingUpdate(100, 7, 42); ops != 2 {
		t.Fatalf("rollingUpdate ops = %d, want 2 regardless of window size", ops)
	}
}

func TestRunEndToEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "below min never cuts, flushed at EOF",
			args:  []string{"-window", "4", "-min", "8", "-max", "100", "-mask", "0xffffffff"},
			input: "abc",
			want:  "chunk 1: offset=0 len=3\n",
		},
		{
			name:  "mandatory max forces two cuts",
			args:  []string{"-window", "2", "-min", "1", "-max", "4", "-mask", "0xffffffff"},
			input: "abcdefgh",
			want:  "chunk 1: offset=0 len=4\nchunk 2: offset=4 len=4\n",
		},
		{
			name:  "empty input produces no chunks",
			args:  []string{"-window", "4", "-min", "1", "-max", "8"},
			input: "",
			want:  "",
		},
		{name: "invalid mask is a usage error", args: []string{"-mask", "not-hex"}, input: "abc", wantErr: true},
		{name: "max below min is a usage error", args: []string{"-min", "10", "-max", "5"}, input: "abc", wantErr: true},
		{name: "unknown flag is a usage error", args: []string{"-bogus"}, input: "abc", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}

func TestRunRejectsMissingFileAsRuntimeNotUsage(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	err := run([]string{"does-not-exist.bin"}, strings.NewReader(""), &stdout)
	if err == nil {
		t.Fatal("run with a missing file: want error, got nil")
	}
	if errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want a runtime failure (exit 1), not errUsage (exit 2)", err)
	}
}
```

## Review

The chunker is correct when `rollingUpdate`'s incrementally-maintained sum
matches a from-scratch resum of the window at every byte fed --
`TestIncrementalSumMatchesFromScratchWithFewerOps` pins that directly --
while doing it in O(1) instead of O(window size) per byte, the property
that makes content-defined chunking practical on large inputs. Around that
core, `NewChunker` rejects a non-positive window with `ErrInvalidWindow`
and an invalid `[minSize,maxSize]` range with `ErrInvalidSizes`; `Feed`
enforces a mandatory cut at `maxSize` regardless of the rolling sum, so a
chunk can never grow unbounded against adversarial input that never
satisfies the mask; and a chunk can never cut below `minSize`, even against
a mask (like zero) that would otherwise match every byte. The tool wraps
every input mistake in `errUsage` for exit code 2, and reserves exit code 1
for a genuine runtime failure like a missing input file, verified directly
by `TestRunRejectsMissingFileAsRuntimeNotUsage`. Run
`go test -count=1 -race ./...` to confirm the size-bounds table, the
incremental-versus-from-scratch contrast, and `run`'s end-to-end behavior.

## Resources

- [restic design: content-defined chunking](https://restic.readthedocs.io/en/stable/100_references.html) — restic's own explanation of why chunk boundaries are content-defined rather than fixed-offset, and what that buys deduplication.
- [rsync algorithm paper (Tridgell and Mackerras, 1996)](https://rsync.samba.org/tech_report/) — the original rolling-checksum technique this module's `rollingUpdate` is a minimal instance of.
- [`bufio.Reader.ReadByte`](https://pkg.go.dev/bufio#Reader.ReadByte) — the streaming byte-at-a-time read this tool uses instead of loading the whole input into memory.
- [`flag.FlagSet` with `ContinueOnError`](https://pkg.go.dev/flag#ContinueOnError) — how `run` gets a returnable parse error instead of `flag.CommandLine`'s default `os.Exit`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-multi-reader-fanout-ring-watermark.md](19-multi-reader-fanout-ring-watermark.md) | Next: [../14-custom-map-based-data-structure/00-concepts.md](../14-custom-map-based-data-structure/00-concepts.md)
