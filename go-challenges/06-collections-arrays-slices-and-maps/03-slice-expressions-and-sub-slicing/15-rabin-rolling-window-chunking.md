# Exercise 15: Content-Defined Chunking with a Rolling Window Hash

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A backup tool that splits every file into fixed-size 4KB blocks before
deduplicating them has a fragile property: insert a single byte anywhere near
the start of a large file and every block boundary downstream shifts by one
byte, so a file that changed by one byte now looks 100% different to the
dedup index. Content-defined chunking (CDC) fixes this by choosing boundaries
based on the file's *content* instead of its byte offset: slide a fixed-width
window over the data, compute a cheap rolling hash of that window at every
position, and cut a chunk whenever the low bits of the hash match a target
pattern. Because the decision depends only on the trailing window's bytes, an
edit only disturbs the one or two chunks that overlap it -- everything
downstream of that realigns and matches again. This is exactly what restic,
borgbackup, and rsync's rolling checksum all lean on to make deduplication and
delta transfer survive edits in the middle of a file.

This exercise builds `cdc`, a command-line tool that reads an arbitrary byte
stream from stdin and prints each chunk's offset and size as it is found,
streaming the whole way -- it never buffers the input or a chunk's content in
memory, only a fixed-width window's worth of state. The engine underneath is
a Rabin-style polynomial rolling hash maintained incrementally, one byte at a
time, in O(1) time regardless of window size.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
cdc/                           module example.com/cdc
  go.mod                       go 1.24
  cdc.go                       package main — ErrInvalidWindowSize; Params; Chunker with Write/Flush
  cdc_test.go                  package main — rolling-vs-from-scratch arithmetic, complexity contrast, edge cases, run() end to end
  main.go                      package main — -window/-mask flags, streaming stdin, exit codes
```

- Files: `cdc.go`, `cdc_test.go`, `main.go`.
- Implement: `Chunker`, which maintains a polynomial rolling hash over the last `Params.WindowSize` bytes written to it and reports a chunk's `(offset, size)` to a caller-supplied `onChunk` callback the instant its boundary is found, via `NewChunker(p Params, onChunk func(offset, size int64) error) (*Chunker, error)`, `(*Chunker).Write(p []byte) (int, error)` implementing `io.Writer`, and `(*Chunker).Flush() error` for the final chunk.
- Tool: `cdc` reads the entire input from stdin as a stream (`io.Copy` into the `Chunker`, never loading it whole into a buffer) and writes one `chunk N: offset=X size=Y` line to stdout per chunk found. `-window` sets the rolling-hash window size in bytes (default 48); `-mask` sets the boundary mask, where a boundary falls wherever `hash&mask == 0` (default 4095, target average chunk size 4096 bytes). Exit 0 on success, exit 2 for a bad flag or a non-positive window size, exit 1 for a stdin read failure.
- Test: the rolling hash's arithmetic checked against a from-scratch oracle at every full-window offset; a naive from-scratch chunker shown to agree with `Chunker` on every boundary while doing strictly more per-byte work (O(n*w) vs O(n)); empty input and input shorter than one window; `NewChunker` rejecting a non-positive window size and a nil callback; `run` end to end over a `strings.Reader`/`bytes.Reader` and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the window has to roll, not restart, and why that beats hashing from scratch

The window this tool hashes at every offset is conceptually `data[i-w+1:i+1]`
-- a plain two-index sub-slice, the exact construct this lesson has been
building around. The ground-truth definition of that window's hash folds it
with Horner's method: `h = h*base + b` for each byte, so the first byte of
the window carries the highest power of `base`. Computing that from scratch
at every one of thousands of offsets is O(n*w) and is exactly what a rolling
hash exists to avoid: by keeping a ring buffer of the current window's bytes,
`rollingHash.roll` cancels the outgoing byte's contribution
(`hash - outgoing*base^(w-1)`), shifts the remaining terms up one power
(`* base`), and adds the incoming byte -- an O(1) update that is provably
equal to the from-scratch hash of the new window at every step.
`TestRollingHashMatchesFromScratch` is the proof: it runs both computations
side by side over two thousand bytes and requires bit-for-bit equality at
every one of the 1,985 full-window offsets.

`Chunker.Write` never resets the rolling hash at a chunk boundary -- only the
"bytes since the last boundary" counter resets. Because of that, a boundary
decision at offset `i` is a pure function of `data[i-w+1:i+1]` and nothing
else, unaffected by where the previous cut happened; an edit near the start
of the stream can only disturb the chunk it lands in, not every chunk after
it, which is the entire value proposition of CDC over fixed-size blocks. A
minimum chunk size -- the current chunk must already hold at least
`WindowSize` bytes -- keeps a boundary from landing one byte after the last
one.

`Chunker` never buffers a chunk's bytes: it only tracks an offset and a
running length, handing both to `onChunk` the moment a boundary is found.
That is what lets `main.go` stream arbitrarily large input through
`io.Copy` without ever holding the whole file, or even one whole chunk, in
memory -- consistent with how a backup tool processes files far larger than
available RAM.

Create `cdc.go`:

```go
// Package main implements cdc, a content-defined chunking tool: it splits
// stdin into variable-length chunks with a Rabin-style rolling hash over a
// fixed-width trailing window, printing each chunk's offset and size. This
// file holds the chunking logic; main.go wires it to flags and stdio.
package main

import (
	"errors"
	"fmt"
)

// ErrInvalidWindowSize is returned by NewChunker when WindowSize is not positive.
var ErrInvalidWindowSize = errors.New("cdc: window size must be positive")

// base is the rolling hash's multiplier. It only needs to be odd so
// consecutive powers stay well distributed under uint64 wraparound; the
// hash is not cryptographic, only cheap and well-mixed.
const base = uint64(257)

// rollingHash computes a polynomial (Rabin-style) hash over the most recent
// windowSize bytes fed to it through roll, in O(1) time per byte.
type rollingHash struct {
	windowSize int
	pow        uint64 // base^(windowSize-1), cancels the outgoing byte
	ring       []byte
	pos        int
	seen       int
	hash       uint64
}

func newRollingHash(windowSize int) *rollingHash {
	pow := uint64(1)
	for i := 0; i < windowSize-1; i++ {
		pow *= base
	}
	return &rollingHash{windowSize: windowSize, pow: pow, ring: make([]byte, windowSize)}
}

// roll feeds the next byte in; full reports whether the window is populated
// yet. A boundary decision made before full is true is not meaningful.
func (r *rollingHash) roll(b byte) (hash uint64, full bool) {
	if r.seen < r.windowSize {
		r.hash = r.hash*base + uint64(b)
		r.ring[r.pos] = b
		r.pos = (r.pos + 1) % r.windowSize
		r.seen++
		return r.hash, r.seen == r.windowSize
	}
	outgoing := r.ring[r.pos]
	r.hash = (r.hash-uint64(outgoing)*r.pow)*base + uint64(b)
	r.ring[r.pos] = b
	r.pos = (r.pos + 1) % r.windowSize
	return r.hash, true
}

// Params configures content-defined chunking. WindowSize is the number of
// trailing bytes the rolling hash covers; Mask selects the target average
// chunk size -- a boundary falls where hash&Mask == 0, so k set low bits in
// Mask yield chunks of roughly 2^k bytes, the technique restic and
// borgbackup use so an edit anywhere in a file only disturbs nearby chunks.
type Params struct {
	WindowSize int
	Mask       uint64
}

// Chunker splits a byte stream into content-defined chunks, reporting each
// chunk's offset and size to onChunk as soon as its boundary is found. Feed
// bytes through Write; call Flush once, after the last Write, to report the
// final chunk. Not safe for concurrent use: Write and Flush mutate its
// rolling-hash and offset state without synchronization.
type Chunker struct {
	p       Params
	roll    *rollingHash
	offset  int64
	current int64
	onChunk func(offset, size int64) error
}

// NewChunker returns a Chunker that reports each chunk boundary to onChunk.
// It returns ErrInvalidWindowSize if p.WindowSize is not positive, and an
// error if onChunk is nil.
func NewChunker(p Params, onChunk func(offset, size int64) error) (*Chunker, error) {
	if p.WindowSize <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidWindowSize, p.WindowSize)
	}
	if onChunk == nil {
		return nil, errors.New("cdc: onChunk must not be nil")
	}
	return &Chunker{p: p, roll: newRollingHash(p.WindowSize), onChunk: onChunk}, nil
}

// Write implements io.Writer: it calls onChunk immediately after a byte
// whose trailing WindowSize-byte window hashes to hash&p.Mask == 0, once
// the chunk in progress holds at least WindowSize bytes. The rolling window
// is never reset at a boundary, only the chunk-length counter is, so a
// decision is a pure function of the bytes trailing it. On an onChunk
// error, Write stops and returns bytes consumed so far, with that error.
func (c *Chunker) Write(p []byte) (int, error) {
	for i, b := range p {
		hash, full := c.roll.roll(b)
		c.current++
		if full && c.current >= int64(c.p.WindowSize) && hash&c.p.Mask == 0 {
			if err := c.onChunk(c.offset, c.current); err != nil {
				return i + 1, err
			}
			c.offset += c.current
			c.current = 0
		}
	}
	return len(p), nil
}

// Flush reports the final chunk, if any bytes remain since the last
// boundary. Call it exactly once, after the last Write.
func (c *Chunker) Flush() error {
	if c.current == 0 {
		return nil
	}
	if err := c.onChunk(c.offset, c.current); err != nil {
		return err
	}
	c.offset += c.current
	c.current = 0
	return nil
}
```

### The tool

`cdc` has no configuration beyond two numeric flags, so `run` takes the
argument slice, an `io.Reader` for stdin, and an `io.Writer` for stdout --
nothing tied to the real terminal, which makes it trivial to drive from a
test with a `strings.Reader` and a `bytes.Buffer`. `flag.NewFlagSet` with
`flag.ContinueOnError` lets `run` return a parse error instead of the
package-level `flag.CommandLine` calling `os.Exit` out from under the test.
A bad flag or a rejected window size is a usage mistake the caller fixes by
changing the command line, so both wrap the `errUsage` sentinel and `main`
maps that to exit code 2; a stdin read failure is a runtime problem and maps
to exit code 1. The whole input flows through `io.Copy(chunker, stdin)`,
which reads it in bounded pieces rather than loading it all at once, keeping
memory use independent of input size -- the same streaming discipline a real
backup tool needs when chunking files far larger than RAM.

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
)

// errUsage marks a failure fixable by changing the command line: a bad flag
// or an invalid window size. main maps it to exit code 2; anything else
// (a stdin read failure) maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, chunks stdin, and writes one line per chunk to stdout. It
// never touches os.Stdin/os.Stdout/os.Exit, so it is testable against a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("cdc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	window := fs.Int("window", 48, "rolling hash window size in bytes")
	mask := fs.Uint64("mask", 4095, "boundary mask; a boundary falls where hash&mask == 0 (target average chunk size is mask+1 bytes)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	w := bufio.NewWriter(stdout)
	index := 0
	onChunk := func(offset, size int64) error {
		index++
		_, err := fmt.Fprintf(w, "chunk %d: offset=%d size=%d\n", index, offset, size)
		return err
	}

	c, err := NewChunker(Params{WindowSize: *window, Mask: *mask}, onChunk)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	if _, err := io.Copy(c, stdin); err != nil {
		return fmt.Errorf("cdc: reading input: %w", err)
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("cdc: writing output: %w", err)
	}
	return w.Flush()
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cdc [-window N] [-mask N] < input")
		fmt.Fprintln(os.Stderr, "splits stdin into content-defined chunks and prints each chunk's offset and size.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cdc:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'the quick brown fox jumps over the lazy dog. the quick brown fox jumps over the lazy dog again and again, testing content defined chunking with a rolling hash over a small window so the boundaries actually show up in a short demo string used here.' | go run . -window 8 -mask 31
printf 'x' | go run . -window 0
```

Expected output:

```text
chunk 1: offset=0 size=37
chunk 2: offset=37 size=45
chunk 3: offset=82 size=25
chunk 4: offset=107 size=26
chunk 5: offset=133 size=37
chunk 6: offset=170 size=13
chunk 7: offset=183 size=60
chunk 8: offset=243 size=4
cdc: usage: cdc: window size must be positive: got 0
```

The eight-chunk run shows the mask picking boundaries at content-dependent
points -- the sizes vary between 4 and 60 bytes even though every byte of the
input was fed through the same rolling hash uniformly, because the boundary
condition depends on the actual bytes in each trailing 8-byte window, not on
a fixed stride. The second command shows the exit-2 usage path: `cdc:`
followed by the error `run` returns, wrapping `ErrInvalidWindowSize`.

### Tests

`TestRollingHashMatchesFromScratch` pins the arithmetic underneath
everything else: the incremental `roll` update must equal a from-scratch
Horner's-method hash at every one of nearly two thousand full-window offsets.
`TestFromScratchHashCostsMoreThanRolling` is the antipattern contrast this
module is built around: `chunkFromScratchHash`, unexported and unreachable
from `Chunker`, recomputes each window's hash directly from its raw bytes
instead of rolling it -- a plausible first attempt at CDC that a reviewer
could easily wave through, since it finds the *same* boundaries as `Chunker`.
What differs is cost, measured directly by counting per-byte work rather than
by timing: the from-scratch approach performs strictly more work
(`O(n*w)` multiply-add steps against `Chunker`'s `O(n)`), which the test
asserts as an inequality, never an exact count, since the precise multiplier
is an implementation detail. `TestChunkerEdgeCasesAndValidation` covers empty
input, input shorter than one window, and `NewChunker` rejecting a
non-positive window size or a nil callback. `TestRun` drives the command end
to end: a realistic input whose printed chunks must exactly partition it
(contiguous offsets, sizes summing to the input length), a non-positive
window and an unknown flag both producing an error that wraps `errUsage`, and
empty input producing no chunk lines at all.

Create `cdc_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// pseudoRandomBytes generates deterministic filler bytes from a fixed LCG
// seed, standing in for arbitrary file content, no math/rand needed.
func pseudoRandomBytes(n int, seed uint64) []byte {
	data := make([]byte, n)
	state := seed
	for i := range data {
		state = state*6364136223846793005 + 1442695040888963407
		data[i] = byte(state >> 33)
	}
	return data
}

// hashWindowFromScratch is the ground-truth oracle: the rolling
// implementation's output must match this at every full-window offset.
func hashWindowFromScratch(w []byte) uint64 {
	var h uint64
	for _, b := range w {
		h = h*base + uint64(b)
	}
	return h
}

// TestRollingHashMatchesFromScratch pins the arithmetic against the oracle
// at every full-window offset.
func TestRollingHashMatchesFromScratch(t *testing.T) {
	t.Parallel()

	data := pseudoRandomBytes(2000, 1)
	const w = 16
	r, checked := newRollingHash(w), 0
	for i, b := range data {
		if hash, full := r.roll(b); full {
			if want := hashWindowFromScratch(data[i-w+1 : i+1]); hash != want {
				t.Fatalf("offset %d: rolling hash = %d, want %d", i, hash, want)
			}
			checked++
		}
	}
	if checked != len(data)-w+1 {
		t.Fatalf("checked %d offsets, want %d", checked, len(data)-w+1)
	}
}

// chunkCorrect runs data through the real Chunker and collects each
// (offset, size) pair it reports.
func chunkCorrect(t *testing.T, data []byte, p Params) [][2]int64 {
	t.Helper()
	var got [][2]int64
	c, err := NewChunker(p, func(offset, size int64) error {
		got = append(got, [2]int64{offset, size})
		return nil
	})
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	if _, err := c.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return got
}

// chunkFromScratchHash is the antipattern: a plausible first CDC attempt
// that hashes each window directly instead of rolling it. Same boundaries
// as Chunker, O(n*w) cost instead of O(n); never exported.
func chunkFromScratchHash(data []byte, p Params, ops *int64) [][2]int64 {
	var chunks [][2]int64
	start := 0
	for i := p.WindowSize - 1; i < len(data); i++ {
		w := data[i-p.WindowSize+1 : i+1]
		*ops += int64(len(w))
		if i+1-start < p.WindowSize {
			continue
		}
		if hashWindowFromScratch(w)&p.Mask == 0 {
			chunks = append(chunks, [2]int64{int64(start), int64(i + 1 - start)})
			start = i + 1
		}
	}
	if start < len(data) {
		chunks = append(chunks, [2]int64{int64(start), int64(len(data) - start)})
	}
	return chunks
}

// TestFromScratchHashCostsMoreThanRolling: the naive approach must agree
// with Chunker on every boundary, while doing strictly more per-byte work.
func TestFromScratchHashCostsMoreThanRolling(t *testing.T) {
	t.Parallel()

	data := pseudoRandomBytes(2000, 11)
	p := Params{WindowSize: 32, Mask: 0xFF}

	rollingSpans := chunkCorrect(t, data, p)
	var naiveOps int64
	naiveSpans := chunkFromScratchHash(data, p, &naiveOps)
	rollingOps := int64(len(data)) // one O(1) roll call per byte

	if len(rollingSpans) != len(naiveSpans) {
		t.Fatalf("boundary counts differ: rolling=%d from-scratch=%d", len(rollingSpans), len(naiveSpans))
	}
	for i := range rollingSpans {
		if rollingSpans[i] != naiveSpans[i] {
			t.Fatalf("chunk %d differs: rolling=%v from-scratch=%v", i, rollingSpans[i], naiveSpans[i])
		}
	}
	if !(naiveOps > rollingOps) {
		t.Fatalf("work units: from-scratch=%d rolling=%d; want from-scratch > rolling", naiveOps, rollingOps)
	}
}

func TestChunkerEdgeCasesAndValidation(t *testing.T) {
	t.Parallel()

	if got := chunkCorrect(t, nil, Params{WindowSize: 8, Mask: 0x3}); len(got) != 0 {
		t.Errorf("chunkCorrect(nil) = %v, want no chunks", got)
	}
	short := pseudoRandomBytes(5, 3)
	if got := chunkCorrect(t, short, Params{WindowSize: 8, Mask: 0x3}); len(got) != 1 || got[0] != ([2]int64{0, 5}) {
		t.Errorf("chunkCorrect(short) = %v, want one chunk spanning the input", got)
	}

	noop := func(int64, int64) error { return nil }
	for _, ws := range []int{0, -1} {
		if _, err := NewChunker(Params{WindowSize: ws, Mask: 1}, noop); !errors.Is(err, ErrInvalidWindowSize) {
			t.Errorf("NewChunker(WindowSize=%d) error = %v, want ErrInvalidWindowSize", ws, err)
		}
	}
	if _, err := NewChunker(Params{WindowSize: 8, Mask: 1}, nil); err == nil {
		t.Error("NewChunker(onChunk=nil) error = nil, want non-nil")
	}
}

// TestRun exercises the command end to end without os.Args/os.Stdin/os.Exit.
func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("chunks partition the input exactly", func(t *testing.T) {
		t.Parallel()
		data := pseudoRandomBytes(2048, 7)
		var stdout bytes.Buffer
		if err := run([]string{"-window", "16", "-mask", "63"}, bytes.NewReader(data), &stdout); err != nil {
			t.Fatalf("run: %v", err)
		}
		var total int
		for i, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
			var idx int
			var offset, size int64
			if _, err := fmt.Sscanf(line, "chunk %d: offset=%d size=%d", &idx, &offset, &size); err != nil {
				t.Fatalf("line %d = %q: %v", i, line, err)
			}
			if offset != int64(total) {
				t.Fatalf("line %d: offset = %d, want %d", i, offset, total)
			}
			total += int(size)
		}
		if total != len(data) {
			t.Fatalf("chunks cover %d bytes, want %d", total, len(data))
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"non-positive window is a usage error", []string{"-window", "0"}},
		{"unknown flag is a usage error", []string{"-bogus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			if err := run(tc.args, strings.NewReader("data"), &stdout); !errors.Is(err, errUsage) {
				t.Fatalf("run error = %v, want it to wrap errUsage", err)
			}
		})
	}

	t.Run("empty input produces no chunk lines", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		if err := run(nil, strings.NewReader(""), &stdout); err != nil || stdout.Len() != 0 {
			t.Fatalf("run(empty) = (err=%v, stdout=%q), want (nil, \"\")", err, stdout.String())
		}
	})
}
```

## Review

`Chunker` is correct when it finds boundaries by exactly the same arithmetic
a from-scratch hash of each window would produce, and it earns its added
complexity over that from-scratch approach only by doing so at O(n) instead
of O(n*w) -- a claim `TestFromScratchHashCostsMoreThanRolling` measures by
counting per-byte work, never by timing it or asserting an exact multiplier.
The trap this module is built around is a chunker that resets its rolling
state at each boundary "to keep things simple": the moment that happens, a
boundary decision stops being a pure function of the trailing window and
starts depending on where the previous cut fell, which is exactly the
offset-dependence CDC exists to eliminate. `cdc` streams its input end to
end -- `io.Copy` into `Chunker.Write`, no buffering of the file or of a
chunk's bytes -- and maps a bad flag or window size to exit code 2, a stdin
read failure to exit code 1. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the two-index sub-slice `data[i-w+1:i+1]` this module hashes at every offset.
- [restic chunker package](https://github.com/restic/chunker) — restic's production content-defined chunking implementation, the same rolling-hash-plus-mask technique this module builds a simplified version of.
- [Rabin fingerprint (Wikipedia)](https://en.wikipedia.org/wiki/Rabin_fingerprint) — the polynomial rolling hash family this module implements a simplified variant of.
- [`io.Copy`](https://pkg.go.dev/io#Copy) — the streaming primitive `main.go` uses to feed stdin into `Chunker` without buffering the whole input.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-multipart-boundary-zero-copy-views.md](14-multipart-boundary-zero-copy-views.md) | Next: [16-rotate-segments-three-reversals.md](16-rotate-segments-three-reversals.md)
