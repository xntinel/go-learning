# Exercise 13: One-Shot Segment Merge with slices.Concat

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Log-structured storage engines -- LevelDB, RocksDB, Badger-style compaction,
or a plain write-ahead log rotating its segments -- periodically fold
several already-complete, immutable segments into one compacted log. The
inputs to that merge are never a live, growing stream: by the time
compaction runs, every segment involved has already been sealed, so its
length is fully known before the merge starts. That is a stronger guarantee
than most code that appends into an accumulator ever gets, and it is worth
exploiting, because the accumulator pattern -- `merged = append(merged,
seg...)` in a loop over segments -- ignores it completely. It grows the
result the same way it would if the segments were arriving one at a time
from an unbounded stream, reallocating over and over as it goes, even though
every one of those reallocations was avoidable: the final size was known
before the first `append` ever ran.

`slices.Concat` is the standard library's answer to exactly this situation.
Given a set of slices whose lengths are all known up front, it sums them in
a single pass, allocates the result once at that exact final size, and
copies each input into place -- one allocation total, not one per segment
plus however many the growth curve needs to reach the final size. The
difference is not a micro-optimization; it is choosing the right tool for a
merge where "how big will this be" is not a question you have to discover
incrementally, because the inputs already answered it.

This module builds `segmerge`, a command that reads `---`-delimited segments
from stdin -- mirroring the footer marker that closes a rotated WAL segment
on disk -- and writes the compacted merge to stdout using `slices.Concat`.
The incremental-append version never ships in the tool's logic; it lives
only in the test file, as the contrast the allocation-count test pins
against.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
segmerge/                      module example.com/segmerge
  go.mod                       go 1.24
  segmerge.go                   package main — ReadSegments, MergeSegments
  segmerge_test.go              package main — segment parsing table, concat correctness,
                                the allocation-count contrast, run() end to end
  main.go                       package main — -stats flag, streaming stdin, exit codes
```

- Files: `segmerge.go`, `segmerge_test.go`, `main.go`.
- Implement: `ReadSegments(r io.Reader) ([][]string, error)`, reading segments terminated by a `---` line (every segment, including the last, must be terminated this way) and returning `ErrUnterminatedSegment` if input ends with pending, unterminated record lines; `MergeSegments(segments ...[]string) []string`, concatenating with `slices.Concat`.
- Tool: `segmerge [-stats]` reads stdin sections separated by `---` lines and writes the concatenated compacted log to stdout, one record per line; `-stats` additionally reports segment and record counts to stderr. Exit 0 on success, exit 2 on a bad flag or an unterminated segment, exit 1 is reserved for a stdin read failure.
- Test: the segment-parsing table (multiple segments, an empty segment between two others, a lone delimiter, two shapes of unterminated input); `MergeSegments` concatenating in order, not aliasing its inputs, and handling zero segments; the heart of the module -- `slices.Concat` needs strictly fewer allocations than an unexported, per-segment-appending `mergeNaiveAppend` merging the identical segments; and `run` end to end, with and without `-stats`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/segmerge
cd ~/go-exercises/segmerge
go mod init example.com/segmerge
go mod edit -go=1.24
```

### Growing an accumulator when the final size was never unknown

The naive merge treats every segment as if its size were a surprise:

```go
var merged []string
for _, seg := range segments {
    merged = append(merged, seg...) // reallocates whenever cap runs out
}
```

Each `append` call may or may not trigger a reallocation depending on how
much spare capacity `merged` currently has, and across a run of several
segments that adds up to several reallocations -- each one copying
everything appended so far into a new, larger array. None of that is
necessary here, because unlike a genuinely unbounded stream, every element
of `segments` is already a complete `[]string` with a known `len` before the
loop starts. The total size of the merge is `sum(len(seg) for seg in
segments)`, computable in one pass over the segment headers alone, with no
need to discover it by growing into it.

`slices.Concat(segments...)` computes exactly that sum, allocates a result
slice of precisely that capacity once, and then copies each segment into its
place. One allocation, not one-per-growth-step. The savings scale with the
number of segments and their combined size, which is exactly the axis a
compaction job varies along.

Create `segmerge.go`:

```go
// Command segmerge reads several already-complete, immutable log segments
// from stdin and merges them into one compacted log on stdout, computing the
// merged log's exact final size in a single pass instead of growing it
// incrementally.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
)

// segmentDelimiter terminates a segment. Every segment, including the last,
// must end with a line containing exactly this text -- mirroring the footer
// marker that closes a rotated write-ahead-log segment on disk.
const segmentDelimiter = "---"

// ErrUnterminatedSegment is returned by ReadSegments when the input ends
// with record lines that were never closed by a segmentDelimiter line.
var ErrUnterminatedSegment = errors.New("segmerge: input ended with an unterminated segment")

// ReadSegments reads zero or more segments from r. A segment is a run of
// lines up to and including a line that reads exactly "---"; each such line
// closes the segment that precedes it (a segment may be empty). Input that
// is empty, or that ends immediately after a delimiter, is valid and yields
// as many segments as delimiters seen. Input with pending record lines and
// no closing delimiter before EOF returns ErrUnterminatedSegment.
func ReadSegments(r io.Reader) ([][]string, error) {
	var segments [][]string
	var current []string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == segmentDelimiter {
			segments = append(segments, current)
			current = nil
			continue
		}
		current = append(current, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("segmerge: reading input: %w", err)
	}
	if current != nil {
		return nil, ErrUnterminatedSegment
	}
	return segments, nil
}

// MergeSegments concatenates segments, in order, into one compacted log.
//
// It uses slices.Concat, which sums every segment's length up front and
// allocates the result once at its exact final size. Every segment reaching
// MergeSegments is already complete and immutable, so that upfront total is
// always fully knowable -- unlike merging by appending one segment at a time
// into a growing accumulator, which reallocates repeatedly even though the
// final size was never actually unknown.
//
// The returned slice is freshly allocated and aliases none of the input
// segments' backing arrays; the caller may mutate it freely without
// affecting segments.
func MergeSegments(segments ...[]string) []string {
	return slices.Concat(segments...)
}
```

### The tool

`segmerge` reads its input one line at a time with `bufio.Scanner`, in
keeping with the fact that compaction inputs on disk are typically streamed
rather than loaded whole. `run` takes the argument slice, an `io.Reader` for
stdin, and separate `io.Writer`s for stdout and stderr, and returns an error
instead of calling `os.Exit`, so it can be driven from a test with a
`strings.Reader` and two `bytes.Buffer`s. A bad flag or an unterminated
segment are both things the caller fixes by changing the command line or the
input, so both wrap `errUsage` and map to exit code 2; a genuine I/O failure
reading stdin maps to exit code 1.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input stream: a bad flag or an unterminated segment. main maps it
// to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads segments from stdin, merges them, and writes the compacted log
// to stdout, one record per line. With -stats it also writes the segment and
// record counts to stderr. It never touches os.Args or os.Exit, so it can be
// exercised in a test against plain io.Reader and io.Writer values.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("segmerge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stats := fs.Bool("stats", false, "report segment and record counts to stderr")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	segments, err := ReadSegments(stdin)
	if err != nil {
		if errors.Is(err, ErrUnterminatedSegment) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}

	merged := MergeSegments(segments...)
	for _, rec := range merged {
		fmt.Fprintln(stdout, rec)
	}

	if *stats {
		total := 0
		for _, seg := range segments {
			total += len(seg)
		}
		fmt.Fprintf(stderr, "segments=%d records=%d\n", len(segments), total)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: segmerge [-stats]")
		fmt.Fprintln(os.Stderr, "merges --- delimited segments from stdin into one compacted log.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "segmerge:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'a1\na2\n---\nb1\n---\nc1\nc2\nc3\n---\n' | go run . -stats
printf 'a1\na2\n' | go run .
```

Expected output:

```text
a1
a2
b1
c1
c2
c3
segments=3 records=6
segmerge: usage: segmerge: input ended with an unterminated segment
```

The first run merges three segments -- lengths 2, 1, and 3 -- into the six
lines on stdout, in order, and `-stats` reports the segment and record
counts to stderr after the merge. The second run has no closing `---` at
all: two record lines arrive and then EOF, which is exactly the pending,
unterminated segment `ReadSegments` rejects, reported with exit code 2.

### Tests

`TestReadSegmentsTable` is the parsing table: multiple segments, an empty
segment sandwiched between two others, a lone delimiter with nothing before
it, and two shapes of unterminated input -- record lines with no delimiter
at all, and a properly closed segment followed by more record lines that
never get their own closing `---`. `TestMergeSegmentsConcatenatesInOrder`,
`TestMergeSegmentsDoesNotAliasInputs`, and `TestMergeSegmentsNoSegments`
cover `MergeSegments` directly: ordering, that mutating the result never
touches a source segment, and the zero-segment edge case.

`TestConcatAllocatesFewerTimesThanNaiveAppend` is the heart of the module.
`mergeNaiveAppend` is unexported and never reachable from the package's
logic; the test merges the identical fifty segments through both it and
`MergeSegments`, measuring allocations with `testing.AllocsPerRun` and
asserting only `concatAllocs < naiveAllocs` -- never an exact count, since
the runtime's growth curve is not part of this module's contract. That test
deliberately omits `t.Parallel`, because `AllocsPerRun` panics if it runs
from a parallel test. `TestRun` drives the command end to end: the exact
stdout and stderr shown above, and every usage-error path.

Create `segmerge_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// mergeNaiveAppend is the antipattern this module warns about: it builds the
// compacted log by appending one segment at a time into a growing
// accumulator, even though every segment's length -- and therefore the
// final total -- is already known before the loop starts. Each time the
// accumulator's capacity runs out it must reallocate, despite the final
// size never actually being unknown.
func mergeNaiveAppend(segments [][]string) []string {
	var merged []string
	for _, seg := range segments {
		merged = append(merged, seg...)
	}
	return merged
}

func TestReadSegmentsTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    [][]string
		wantErr bool
	}{
		{name: "empty input", input: "", want: nil},
		{
			name:  "three segments",
			input: "a1\na2\n---\nb1\n---\nc1\nc2\nc3\n---\n",
			want: [][]string{
				{"a1", "a2"},
				{"b1"},
				{"c1", "c2", "c3"},
			},
		},
		{
			name:  "empty segment between two others",
			input: "a1\n---\n---\nc1\n---\n",
			want:  [][]string{{"a1"}, nil, {"c1"}},
		},
		{name: "single delimiter only", input: "---\n", want: [][]string{nil}},
		{name: "unterminated segment", input: "a1\na2\n", wantErr: true},
		{name: "unterminated segment after a closed one", input: "a1\n---\nb1\n", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ReadSegments(strings.NewReader(tc.input))
			if tc.wantErr {
				if !errors.Is(err, ErrUnterminatedSegment) {
					t.Fatalf("ReadSegments(%q) error = %v, want ErrUnterminatedSegment", tc.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadSegments(%q): %v", tc.input, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ReadSegments(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for i := range got {
				if !slices.Equal(got[i], tc.want[i]) {
					t.Fatalf("segment %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMergeSegmentsConcatenatesInOrder(t *testing.T) {
	t.Parallel()

	segments := [][]string{
		{"a1", "a2"},
		{"b1"},
		nil,
		{"c1", "c2", "c3"},
	}
	got := MergeSegments(segments...)
	want := []string{"a1", "a2", "b1", "c1", "c2", "c3"}
	if !slices.Equal(got, want) {
		t.Fatalf("MergeSegments(...) = %v, want %v", got, want)
	}
}

func TestMergeSegmentsDoesNotAliasInputs(t *testing.T) {
	t.Parallel()

	seg := []string{"a1", "a2"}
	merged := MergeSegments(seg)
	merged[0] = "mutated"
	if seg[0] != "a1" {
		t.Fatalf("mutating the merged log changed the source segment: %q", seg[0])
	}
}

func TestMergeSegmentsNoSegments(t *testing.T) {
	t.Parallel()

	if got := MergeSegments(); len(got) != 0 {
		t.Fatalf("MergeSegments() = %v, want empty", got)
	}
}

// TestConcatAllocatesFewerTimesThanNaiveAppend is the heart of the module:
// merging the same segments, slices.Concat's single upfront allocation must
// need strictly fewer allocations than repeatedly appending one segment at a
// time. The exact counts are a runtime growth-curve detail and are
// deliberately not asserted, only the inequality is.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test.
func TestConcatAllocatesFewerTimesThanNaiveAppend(t *testing.T) {
	segments := make([][]string, 50)
	for i := range segments {
		segments[i] = []string{
			fmt.Sprintf("rec-%d-a", i),
			fmt.Sprintf("rec-%d-b", i),
			fmt.Sprintf("rec-%d-c", i),
		}
	}

	concatAllocs := testing.AllocsPerRun(50, func() {
		_ = MergeSegments(segments...)
	})
	naiveAllocs := testing.AllocsPerRun(50, func() {
		_ = mergeNaiveAppend(segments)
	})
	if !(concatAllocs < naiveAllocs) {
		t.Fatalf("allocations: concat = %v, naive = %v; want concat < naive", concatAllocs, naiveAllocs)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		args       []string
		stdin      string
		wantStdout string
		wantStderr string
		wantErr    bool
	}{
		{
			name:       "merge without stats",
			args:       nil,
			stdin:      "a1\na2\n---\nb1\n---\nc1\nc2\nc3\n---\n",
			wantStdout: "a1\na2\nb1\nc1\nc2\nc3\n",
		},
		{
			name:       "merge with stats",
			args:       []string{"-stats"},
			stdin:      "a1\na2\n---\nb1\n---\nc1\nc2\nc3\n---\n",
			wantStdout: "a1\na2\nb1\nc1\nc2\nc3\n",
			wantStderr: "segments=3 records=6\n",
		},
		{
			name:       "empty input produces empty output",
			args:       nil,
			stdin:      "",
			wantStdout: "",
		},
		{
			name:    "unterminated segment is a usage error",
			args:    nil,
			stdin:   "a1\na2\n",
			wantErr: true,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-bogus"},
			stdin:   "a1\n---\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.wantStdout {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.wantStdout)
			}
			if stderr.String() != tc.wantStderr {
				t.Fatalf("run(%v) stderr = %q, want %q", tc.args, stderr.String(), tc.wantStderr)
			}
		})
	}
}
```

## Review

`MergeSegments` is correct when it needs fewer allocations than the naive
accumulator to merge the identical input --
`TestConcatAllocatesFewerTimesThanNaiveAppend` pins exactly that inequality,
never an exact count, against the unexported `mergeNaiveAppend`, which never
appears in the package's exported surface. The mechanism is `slices.Concat`
computing the merge's total size in one pass over already-known segment
lengths and allocating once, instead of discovering the size incrementally
through repeated `append` growth -- a discovery that was never necessary,
because every segment reaching a compaction job is already complete and
immutable. Around that core, `ReadSegments` requires every segment,
including the last, to be closed by a `---` delimiter, rejecting a stream
that ends mid-segment with `ErrUnterminatedSegment`; `MergeSegments` never
aliases any input segment's backing array. The tool streams its input line
by line, maps a bad flag or an unterminated segment to exit code 2, and
reserves exit code 1 for a stdin read failure. Run
`go test -count=1 -race ./...` to confirm the parsing table, merge
correctness, the allocation-count contrast, and `run`'s end-to-end behavior.

## Resources

- [`slices.Concat`](https://pkg.go.dev/slices#Concat) — the single-allocation concatenation this module builds around.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.
- [LevelDB: Compaction](https://github.com/google/leveldb/blob/main/doc/impl.md#compactions) — a production log-structured store whose compaction merges already-sealed, immutable segments, the same shape this module models.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented streaming reader this tool uses instead of loading all of stdin at once.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-sliding-window-front-truncation-compaction.md](12-sliding-window-front-truncation-compaction.md) | Next: [14-slices-chunk-batch-aliasing.md](14-slices-chunk-batch-aliasing.md)
