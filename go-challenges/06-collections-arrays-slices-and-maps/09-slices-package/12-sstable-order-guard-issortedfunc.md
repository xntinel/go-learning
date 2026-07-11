# Exercise 12: Guard An SSTable Merge With slices.IsSortedFunc

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

LSM-tree storage engines -- RocksDB, LevelDB, Cassandra's own SSTables -- write
immutable, sorted segment files and later fold several of them together in a
compaction pass: a k-way merge that walks every segment's keys in lockstep,
relying entirely on each one already being sorted. Nothing in a k-way merge
checks that assumption while it runs; it just reads whichever segment's current
key compares lowest and advances that cursor. Feed it a segment that drifted out
of order -- a buggy compactor, a writer that crashed mid-flush, a manual restore
from an older backup -- and the merge does not crash. It silently emits keys out
of order, and every downstream reader that trusts the merged output, including
one doing a binary search over it, now returns wrong answers with no error at
all.

This module does not build the merge. It builds the guard that has to run before
it: a small tool that reads one segment and answers exactly one question --
are these keys sorted, yes or no -- using `slices.IsSortedFunc` as the explicit
check. That is deliberate. `slices.BinarySearch` and `slices.BinarySearchFunc`
never verify their own precondition; they assume the slice is sorted in the
comparator's order and return a plausible-looking, silently wrong index when it
is not. A preflight guard that actually calls `IsSortedFunc` before anything
downstream trusts the data is the difference between a corrupted segment
failing loudly, right where the corruption is, and it failing quietly three
components downstream in whatever process happened to binary-search the merged
result first.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
sstable-verify/                module example.com/sstable-verify
  go.mod                       go 1.24
  sstable.go                   package main — Record, ParseSegment, VerifySorted (IsSortedFunc)
  sstable_test.go               package main — parse/verify tables, BinarySearch-on-unsorted
                                contrast, run() end to end
  main.go                      package main — reads a file arg or stdin, exit codes
```

- Files: `sstable.go`, `sstable_test.go`, `main.go`.
- Implement: `ParseSegment(r io.Reader) ([]Record, error)` reading tab-separated `key\tvalue` lines, skipping blank lines, rejecting a line with no tab with `ErrMalformedLine`; `VerifySorted(records []Record) error` checking ascending key order with `slices.IsSortedFunc` and, on failure, returning `ErrOutOfOrder` wrapping the first violating line.
- Tool: `sstable-verify [segment.tsv]` reads the named file, or stdin if no argument is given. On success it prints `OK: N keys sorted` to stdout and exits 0. A malformed line or an out-of-order pair is invalid input: the message goes to stderr and the tool exits 2. A file that cannot be opened or read is a runtime failure: stderr, exit 1.
- Test: the parse table (sorted input, blank lines skipped, empty input, a missing tab); the verify table (empty, single record, ascending, equal-consecutive, one drifted pair); the naive `slices.BinarySearchFunc`-without-a-preflight-check contrast; `run` end to end over `strings.Reader` and a `strings.Builder`, covering stdin input, a missing file, a malformed line, and an out-of-order segment.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sstable-verify
cd ~/go-exercises/sstable-verify
go mod init example.com/sstable-verify
go mod edit -go=1.24
```

### IsSortedFunc as a stated precondition, not a hope

`slices.IsSortedFunc(s, cmp)` walks `s` once and reports whether every
consecutive pair satisfies `cmp(s[i-1], s[i]) <= 0`. That is the entire
contract: a yes-or-no answer about the one invariant a sorted-merge algorithm
needs and never checks for itself. The reason this module exists as a standalone
step, ahead of any merge, is that the failure mode it prevents is specifically
silent. `slices.BinarySearchFunc` on an unsorted slice does not panic and does
not return an error -- it returns an index and a `found` boolean that both look
completely ordinary, and on the wrong data they are simply wrong:

```go
records := []Record{{"apple", ...}, {"cherry", ...}, {"banana", ...}} // drifted
i, found := slices.BinarySearchFunc(records, "banana", compareKeys)
// found is false. "banana" is right there in the slice.
```

A compactor that trusts an upstream sort and skips the explicit check is betting
that every writer, every crash-recovery path, and every manual intervention
upstream got the ordering right, forever. `VerifySorted` is the tool's entire
job: call `IsSortedFunc` before anything else touches the segment, and if it
fails, say exactly which line broke the order instead of letting a search three
layers downstream return a wrong answer nobody will trace back here.

Create `sstable.go`:

```go
// Command sstable-verify is the preflight guard an LSM-tree compaction pass
// runs before it trusts an SSTable segment's keys are sorted. It never sorts
// or merges anything itself; it only checks the one invariant a merge relies
// on and refuses to be silent about a violation.
//
// See the package tests for why the check is explicit rather than assumed:
// slices.BinarySearch and a k-way merge both behave in silently wrong ways
// on data that drifted out of order, and neither one tells you it happened.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Sentinel errors returned by ParseSegment and VerifySorted. Callers should
// test for them with errors.Is.
var (
	// ErrMalformedLine means a non-blank line had no tab separator.
	ErrMalformedLine = errors.New("sstable-verify: malformed line, want key\\tvalue")
	// ErrOutOfOrder means two consecutive keys violated ascending order.
	ErrOutOfOrder = errors.New("sstable-verify: keys not sorted")
)

// Record is one key/value pair read from a segment, plus the 1-based input
// line it came from, so a violation can be reported against the file the
// operator is looking at rather than a bare slice index.
type Record struct {
	Key, Value string
	Line       int
}

// ParseSegment reads tab-separated key\tvalue lines from r, one Record per
// line, preserving input order. Blank lines are skipped. A non-blank line
// without a tab is rejected with ErrMalformedLine wrapping the line number.
//
// ParseSegment reads r to completion; it does not retain r.
func ParseSegment(r io.Reader) ([]Record, error) {
	var records []Record
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, "\t")
		if !ok {
			return nil, fmt.Errorf("%w: line %d: %q", ErrMalformedLine, line, text)
		}
		records = append(records, Record{Key: key, Value: value, Line: line})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sstable-verify: reading input: %w", err)
	}
	return records, nil
}

// compareKeys orders two Records by Key using plain byte-wise comparison,
// the order every SSTable writer in this module's lineage sorts by.
func compareKeys(a, b Record) int {
	return strings.Compare(a.Key, b.Key)
}

// VerifySorted checks that records is sorted by Key using slices.IsSortedFunc
// -- the explicit invariant check a compaction pass must run before it trusts
// a segment, since a k-way merge or a binary search over unsorted data fails
// silently rather than with an error. If records is not sorted, VerifySorted
// returns ErrOutOfOrder wrapping the first violating line, found with a
// second pass, since IsSortedFunc itself reports only true or false.
func VerifySorted(records []Record) error {
	if slices.IsSortedFunc(records, compareKeys) {
		return nil
	}
	for i := 1; i < len(records); i++ {
		if compareKeys(records[i-1], records[i]) > 0 {
			return fmt.Errorf("%w: line %d: key %q sorts before line %d: key %q",
				ErrOutOfOrder, records[i].Line, records[i].Key, records[i-1].Line, records[i-1].Key)
		}
	}
	return nil // unreachable: IsSortedFunc already reported false above
}
```

### The tool

`sstable-verify` takes at most one positional argument, a file path; with none
it reads stdin, which is what lets it sit in a shell pipeline right after
whatever produced the segment. `run` takes the argument slice, the input
reader, and an `io.Writer` for stdout, and returns a plain `error` -- it never
touches `os.Exit` or a real file handle directly, so a test can drive it with a
`strings.Reader` and a `strings.Builder` without touching the filesystem except
in the one case that specifically tests a missing file. Two sentinels classify
every failure `run` can produce: `errUsage` for a malformed line or an
out-of-order segment, since both are things the caller fixes by fixing the
input, and `errRuntime` for a file that cannot be opened or read, since no
change to the segment's contents fixes that. `main` maps `errRuntime` to exit
code 1 and everything else to exit code 2; a clean pass never returns an error
and exits 0 after printing the count.

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

// errUsage marks a failure the caller can fix by changing the input: a
// malformed line or a segment whose keys are not sorted. main maps it to
// exit code 2. errRuntime marks a failure the caller cannot fix by editing
// the segment: the file could not be opened or read. main maps it to exit
// code 1.
var (
	errUsage   = errors.New("invalid input")
	errRuntime = errors.New("runtime failure")
)

// run reads a segment from stdin or, if args names one, from a file, checks
// it with ParseSegment and VerifySorted, and writes the result to stdout. It
// never touches os.Exit, so it can be exercised in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("sstable-verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	r := stdin
	if fs.NArg() > 0 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return fmt.Errorf("%w: %w", errRuntime, err)
		}
		defer f.Close()
		r = f
	}

	records, err := ParseSegment(r)
	if err != nil {
		if errors.Is(err, ErrMalformedLine) {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		return fmt.Errorf("%w: %w", errRuntime, err)
	}

	if err := VerifySorted(records); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	fmt.Fprintf(stdout, "OK: %d keys sorted\n", len(records))
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sstable-verify [segment.tsv]")
		fmt.Fprintln(os.Stderr, "reads tab-separated key\\tvalue lines from the file or stdin")
		fmt.Fprintln(os.Stderr, "and verifies the keys are sorted before a merge trusts them.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sstable-verify:", err)
		if errors.Is(err, errRuntime) {
			os.Exit(1)
		}
		os.Exit(2)
	}
}
```

Run it:

```bash
printf 'apple\t1\nbanana\t2\ncherry\t3\n' > segment.tsv
printf 'apple\t1\ncherry\t2\nbanana\t3\n' > drifted.tsv
go run . segment.tsv
go run . drifted.tsv
printf 'apple\t1\nno-tab-here\n' | go run .
go run . missing.tsv
```

Expected output:

```text
OK: 3 keys sorted
sstable-verify: invalid input: sstable-verify: keys not sorted: line 3: key "banana" sorts before line 2: key "cherry"
sstable-verify: invalid input: sstable-verify: malformed line, want key\tvalue: line 2: "no-tab-here"
sstable-verify: runtime failure: open missing.tsv: no such file or directory
```

The first line is the clean pass, exit 0. The second is `drifted.tsv`, where
`cherry` was written before `banana`: `VerifySorted` names line 3 as the one
that broke ascending order relative to line 2, exit 2. The third pipes a line
with no tab through stdin, caught by `ParseSegment` before `VerifySorted` ever
runs, exit 2. The fourth names a file that does not exist, an `errRuntime`
failure distinct from a bad segment, exit 1. Between them, these four runs
cover every path `run` can take: success, a sortedness violation, a malformed
line, and an I/O failure.

### Tests

`TestParseSegment` is the table for the reader: ordinary sorted input, blank
lines skipped without becoming records, empty input yielding no records and no
error, and a line with no tab producing `ErrMalformedLine` at the right line
number. `TestVerifySorted` tables the sortedness check itself across the edge
cases `slices.IsSortedFunc` has to get right by definition -- empty and
single-record input are trivially sorted, equal consecutive keys pass -- plus
the one case that matters, a pair that drifted out of order.

`TestNaiveBinarySearchSilentlyMissesOnUnsortedData` is the module's reason for
existing. `searchTrustingSortNaive` is unexported and unreachable from the
tool: it runs `slices.BinarySearchFunc` directly against a segment, the way a
compaction pass looks once someone decides the preflight check is redundant
overhead. The test feeds it a segment with one drifted key and shows the search
reports `found == false` for a key that is actually present in the slice -- no
panic, no error, just a wrong answer -- and then shows `VerifySorted` catching
the identical corruption before any search runs, with a specific line number
attached.

Create `sstable_test.go`:

```go
package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    []Record
		wantErr error
	}{
		{"three sorted records", "apple\t1\nbanana\t2\ncherry\t3\n",
			[]Record{{"apple", "1", 1}, {"banana", "2", 2}, {"cherry", "3", 3}}, nil},
		{"blank lines are skipped", "apple\t1\n\nbanana\t2\n",
			[]Record{{"apple", "1", 1}, {"banana", "2", 3}}, nil},
		{"empty input yields no records", "", nil, nil},
		{"missing tab is malformed", "apple\t1\nbanana-only\ncherry\t3\n",
			nil, ErrMalformedLine},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSegment(strings.NewReader(tc.input))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseSegment error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSegment: unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseSegment = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVerifySorted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		wantErr error
	}{
		{"empty input is trivially sorted", nil, nil},
		{"single record is trivially sorted", []Record{{"a", "1", 1}}, nil},
		{"ascending keys pass", []Record{{"a", "1", 1}, {"b", "2", 2}, {"c", "3", 3}}, nil},
		{"equal consecutive keys pass", []Record{{"a", "1", 1}, {"a", "2", 2}}, nil},
		{"a drifted-out-of-order pair fails", []Record{{"a", "1", 1}, {"c", "2", 2}, {"b", "3", 3}}, ErrOutOfOrder},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := VerifySorted(tc.records)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("VerifySorted error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// searchTrustingSortNaive is the merge-path shortcut this module exists to
// prevent: it runs slices.BinarySearchFunc directly against records without
// ever calling VerifySorted first, the way a compaction pass looks once
// someone has "optimized away" the preflight check because it "should
// always be sorted anyway". It is not part of the tool; it exists only to
// show what that shortcut actually returns on data that drifted out of
// order upstream.
func searchTrustingSortNaive(records []Record, key string) (int, bool) {
	return slices.BinarySearchFunc(records, key, func(r Record, key string) int {
		return strings.Compare(r.Key, key)
	})
}

// TestNaiveBinarySearchSilentlyMissesOnUnsortedData is the contrast this
// module is built around. A segment with a single drifted key is exactly
// the corrupted-compactor scenario the concepts file warns about: BinarySearch
// on unsorted data returns a plausible-looking answer, found == false, with
// no error, for a key that is actually present. VerifySorted catches the
// same corruption before any search runs, with a specific line number.
func TestNaiveBinarySearchSilentlyMissesOnUnsortedData(t *testing.T) {
	t.Parallel()

	records := []Record{{"apple", "1", 1}, {"cherry", "2", 2}, {"banana", "3", 3}}

	_, found := searchTrustingSortNaive(records, "banana")
	if found {
		t.Fatal("searchTrustingSortNaive unexpectedly found banana; test no longer exercises the drift")
	}

	if err := VerifySorted(records); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("VerifySorted error = %v, want ErrOutOfOrder before any search runs", err)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
		runtime bool
	}{
		{
			name:  "sorted segment via stdin",
			stdin: "apple\t1\nbanana\t2\ncherry\t3\n",
			want:  "OK: 3 keys sorted\n",
		},
		{
			name:  "empty segment is trivially sorted",
			stdin: "",
			want:  "OK: 0 keys sorted\n",
		},
		{
			name:    "out of order segment is rejected",
			stdin:   "apple\t1\ncherry\t2\nbanana\t3\n",
			wantErr: true,
		},
		{
			name:    "malformed line is rejected",
			stdin:   "apple\t1\nno-tab-here\n",
			wantErr: true,
		},
		{
			name:    "missing file is a runtime failure",
			args:    []string{"/nonexistent/segment.tsv"},
			wantErr: true,
			runtime: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout strings.Builder
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run: want error, got nil")
				}
				wantSentinel := errUsage
				if tc.runtime {
					wantSentinel = errRuntime
				}
				if !errors.Is(err, wantSentinel) {
					t.Fatalf("run error = %v, want it to wrap %v", err, wantSentinel)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: unexpected error: %v", err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`sstable-verify` is correct when it prints `OK: N keys sorted` for exactly the
segments a k-way merge could safely consume, and rejects, by name and line
number, every segment it could not -- one with a malformed line, and one whose
keys are out of order. The trap it guards against is trusting an upstream sort
without checking: `slices.BinarySearchFunc`, and by extension any merge built on
the same sorted-invariant assumption, fails silently on data that drifted out
of order, returning a plausible index or `found == false` with no error at all,
which `TestNaiveBinarySearchSilentlyMissesOnUnsortedData` demonstrates directly
against the naive, unexported search helper. `VerifySorted` closes that gap with
`slices.IsSortedFunc`, an explicit check that reports the first violating line
rather than letting the corruption surface three components downstream. Exit
codes separate what the caller can fix from what they cannot: a malformed line
or an out-of-order segment is invalid input, exit 2; a file that will not open
or read is a runtime failure, exit 1; a clean pass exits 0. Run
`go test -count=1 -race ./...` to confirm the parse and verify tables, the
search contrast, and `run`'s end-to-end behavior across all five cases.

## Resources

- [`slices.IsSortedFunc`](https://pkg.go.dev/slices#IsSortedFunc) — the explicit invariant check this module builds around.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the function that silently trusts the precondition `IsSortedFunc` verifies.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented reader `ParseSegment` streams the segment through.
- [LevelDB: Compaction](https://github.com/google/leveldb/blob/main/doc/impl.md#compactions) — the merge pass an SSTable segment must already be sorted for.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-raft-log-conflict-splice-replace.md](11-raft-log-conflict-splice-replace.md) | Next: [13-dns-negative-cache-nil-vs-empty.md](13-dns-negative-cache-nil-vs-empty.md)
