# Exercise 18: Blocklist Membership Scanner -- Early-Terminating Iteration vs Sort-Then-Scan

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A request-path check in a WAF or a rate limiter tests a batch of candidate
IDs -- API keys presented by a client, IPs on an incoming connection --
against an in-memory blocklist before letting any of them through. The only
question that matters is "does any of these match", answered as early as
possible, because this check sits directly on the hot path and every
microsecond spent on it is added to every request. It has nothing to do with
producing a list, a report, or any kind of ordered output: the caller wants
a boolean and, ideally, which candidate tripped it, and wants it fast.

This is the exact scenario where a lesson well-learned becomes a lesson
misapplied. Earlier in this chapter the correct habit for turning a map into
deterministic output is drilled in hard: never range a map directly into a
result, collect its keys, sort them, then iterate the sorted slice --
`slices.Sorted(maps.Keys(m))`. That idiom is exactly right when the *output
order* is the thing that must be stable. It is exactly wrong here, because a
membership check produces no output order at all -- it produces one boolean.
Applying the sorted-iteration idiom to a membership test means allocating a
slice sized to the entire blocklist and sorting it, on every single call,
to answer a question a single map index already answers in constant time.

The fix is not a smarter sort. It is recognizing that `maps.Keys` returns an
`iter.Seq[K]` -- a Go 1.23 range-over-function iterator -- that can be
ranged directly with an ordinary `break` the moment a match is found,
without ever materializing the full key set into a slice, let alone sorting
it. But for a plain membership question, the correctly reached-for tool is
narrower still: the map's own index expression, `_, ok := m[k]`, already
answers "is k present" without iterating anything. This module builds that
tool, `scanblock`, so the difference between "correct habit, wrong place"
and "the actually right primitive for the job" is something you can measure,
not just take on faith.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
scanblock/                    module example.com/scanblock
  go.mod                      go 1.24
  scanblock.go                package main — Blocklist, NewBlocklist, Contains, FirstBlocked; ErrReadFailure
  scanblock_test.go           package main — blocklist table, stream-order match, read-error
                               propagation, sorted-scan contrast, run() end to end
  main.go                     package main — -blocklist flag, exit codes
```

- Files: `scanblock.go`, `scanblock_test.go`, `main.go`.
- Implement: `NewBlocklist(r io.Reader) (Blocklist, error)` parsing one ID per line, skipping blanks and `#` comments; `(Blocklist).Contains(id string) bool` as a direct map index; `FirstBlocked(r io.Reader, block Blocklist) (id string, found bool, err error)` streaming candidates and stopping at the first match; sentinel `ErrReadFailure`.
- Tool: `scanblock -blocklist <file>` reads candidate IDs from stdin, one per line, and prints the first blocked one (if any) to stdout. Exit 0 if no candidate matches, exit 1 if one does, exit 2 for a missing `-blocklist` flag, an unreadable blocklist file, or an unknown flag.
- Test: parsing table (blanks, comments, trimming, empty blocklist); `FirstBlocked` stopping at the earliest match in stream order rather than sorted order; read-error propagation from both the blocklist source and the candidate stream; a `containsSortedScan` contrast proving the sorted-iteration idiom agrees on every outcome but allocates strictly more; and `run` end to end over `strings.Reader` + `bytes.Buffer`, including the missing-flag, missing-file, and unknown-flag usage errors.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/scanblock
cd ~/go-exercises/scanblock
go mod init example.com/scanblock
go mod edit -go=1.24
```

### A membership check needs no order, so it needs no sort

Here is the version that looks like it is following the chapter's own
advice:

```go
// WRONG TOOL: correct output, applied where no output order is needed.
hit := false
for _, k := range slices.Sorted(maps.Keys(block)) {
    if k == candidate {
        hit = true
        break
    }
}
```

Every line of this compiles, and it does answer the question correctly.
What it does on every single call, though, is: walk the entire blocklist to
collect its keys into a fresh slice, sort that slice, and only then start
comparing against `candidate` -- work that scales with the size of the
*whole blocklist*, paid in full even when `candidate` happens to be the
first key encountered. The sorted-iteration idiom earns its cost when the
thing you need afterward is an *order* -- a report, a golden-tested response
body, a diff. A membership check throws the order away the instant it finds
(or fails to find) a match; sorting first is pure waste on the way to an
answer that was always going to be a single boolean.

The direct fix needs no iteration at all: `_, ok := block[candidate]` is a
single map index, answered in the same constant expected time regardless of
how many entries the blocklist holds. `FirstBlocked` below applies that
index to each candidate in a *streamed* batch instead, so the batch itself
is the thing being iterated with early termination -- an ordinary `for
sc.Scan()` loop that `return`s the moment a match is found, never reading
the rest of stdin. That is the legitimate use of "stop at the first match
without materializing everything": the collection being short-circuited is
the candidate stream, and the blocklist itself is never iterated or sorted
at all.

Create `scanblock.go`:

```go
// Package main implements scanblock, a hot-path membership check: it tests a
// stream of candidate IDs against an in-memory blocklist and reports the
// first one that matches, without ever needing the blocklist's own keys in
// any particular order.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrReadFailure wraps an I/O failure encountered while reading the
// blocklist source or the candidate stream.
var ErrReadFailure = errors.New("scanblock: failed to read input")

// Blocklist is a set of blocked IDs. Membership is answered by a direct map
// index, never by iterating, sorting, or materializing the blocklist's keys
// -- a map already answers "is this key present" in O(1), so there is
// nothing an iteration order could improve.
type Blocklist map[string]struct{}

// NewBlocklist reads one ID per line from r, skipping blank lines and lines
// beginning with '#'. Leading and trailing whitespace on each line is
// trimmed before the ID is stored. An empty blocklist (zero entries) is
// valid: it simply matches nothing. NewBlocklist returns ErrReadFailure,
// wrapped with the underlying error, if r fails while being read.
func NewBlocklist(r io.Reader) (Blocklist, error) {
	b := make(Blocklist)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b[line] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: blocklist: %v", ErrReadFailure, err)
	}
	return b, nil
}

// Contains reports whether id is present in the blocklist. It is a single
// map index: Blocklist never needs to iterate, sort, or copy its members to
// answer a membership question, regardless of how large it is.
func (b Blocklist) Contains(id string) bool {
	_, ok := b[id]
	return ok
}

// FirstBlocked reads candidate IDs from r, one per line, and returns the
// first one present in block. It stops reading as soon as a match is found:
// candidates after the match are never read, which matters when r is a
// large or unbounded stream -- a batch of API keys or IPs checked on a hot
// request path, where only "does any of them match" is needed, not a full
// scored list. Blank lines are skipped. found is false, with id the zero
// value, if r is exhausted with no match. FirstBlocked returns
// ErrReadFailure only if reading r itself fails.
func FirstBlocked(r io.Reader, block Blocklist) (id string, found bool, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		candidate := strings.TrimSpace(sc.Text())
		if candidate == "" {
			continue
		}
		if block.Contains(candidate) {
			return candidate, true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("%w: candidates: %v", ErrReadFailure, err)
	}
	return "", false, nil
}
```

### The tool

`scanblock` reads its blocklist from a file named by `-blocklist` and its
candidates from stdin, so `run` takes the argument slice plus explicit
readers and writers for stdin, stdout, and stderr -- nothing touches
`os.Stdin`, `os.Stdout`, or `os.Exit` directly, which is what lets a test
drive the whole tool over a `strings.Reader` and a `bytes.Buffer`. The exit
code carries the actual answer, not just success or failure: 0 means no
candidate matched, 1 means one did (and its ID was printed to stdout), and 2
covers everything that keeps the check from running at all -- a missing
`-blocklist` flag, a blocklist file that cannot be opened or read, or an
unrecognized flag. `FirstBlocked` streams stdin through `bufio.Scanner`
rather than reading it all into memory first, so a very large candidate
batch with an early match never pays for reading past that match.

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

// run scans candidates on stdin against the blocklist file named by the
// -blocklist flag, writing the first blocked ID (if any) to stdout followed
// by a newline. matched is true if a blocked ID was found. err is non-nil
// only for a usage problem or an unreadable input source -- never for "no
// match", which is a normal, successful outcome. run never touches
// os.Stdin, os.Stdout, or os.Exit directly, so it can be driven end to end
// from a test with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) (matched bool, err error) {
	fs := flag.NewFlagSet("scanblock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	blocklistPath := fs.String("blocklist", "", "path to the blocklist file, one ID per line (required)")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if *blocklistPath == "" {
		return false, errors.New("scanblock: -blocklist is required")
	}

	f, err := os.Open(*blocklistPath)
	if err != nil {
		return false, fmt.Errorf("scanblock: %w", err)
	}
	defer f.Close()

	block, err := NewBlocklist(f)
	if err != nil {
		return false, err
	}

	id, found, err := FirstBlocked(stdin, block)
	if err != nil {
		return false, err
	}
	if found {
		fmt.Fprintln(stdout, id)
	}
	return found, nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: scanblock -blocklist FILE")
		fmt.Fprintln(os.Stderr, "reads candidate IDs from stdin, one per line, and prints the first one found in the blocklist.")
	}
	matched, err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scanblock:", err)
		os.Exit(2)
	}
	if matched {
		os.Exit(1)
	}
	os.Exit(0)
}
```

Run it:

```bash
printf 'alice\nbob\ncarol\n' > blocklist.txt
printf 'dave\nbob\neve\n' | go run . -blocklist blocklist.txt
printf 'dave\neve\n' | go run . -blocklist blocklist.txt
go run . -blocklist missing.txt < /dev/null
```

Expected output:

```text
bob
scanblock: scanblock: open missing.txt: no such file or directory
```

The first `go run` line matches `bob` from the blocklist and prints it,
exiting 1. The second finds no match among `dave` and `eve`, exits 0, and
prints nothing at all -- silence is the expected, successful outcome for
"nothing blocked". The third names a blocklist file that does not exist:
`os.Open` fails, `run` wraps it in a plain `scanblock: ...` error (there is
no dedicated sentinel for this case, since any `os.Open` failure is
equally a configuration problem), and `main` writes it to stderr and exits
2.

### Tests

`TestNewBlocklistSkipsBlankAndCommentLines` and
`TestNewBlocklistEmptySourceIsValid` pin the parsing rules, including the
edge case of a blocklist with zero real entries, which must behave like a
blocklist that matches nothing rather than an error.
`TestFirstBlockedStopsAtFirstMatchInStreamOrder` is the test that
distinguishes this module from a sorted-scan implementation on the
*outside*: it puts the alphabetically later ID earlier in the candidate
stream and asserts `FirstBlocked` returns that one, proving the function
walks candidates in the order they arrive, not in any sorted order.
`TestReadErrorsWrapErrReadFailure` checks both places the tool reads from an
`io.Reader` -- the blocklist file and the candidate stream -- wrap a failing
`Read` in the same sentinel.

`TestContainsAgreesWithSortedScanButAllocatesLess` is the heart of the
module. `containsSortedScan` is unexported and unreachable from the package
API; it is the sorted-iteration idiom applied to a plain membership check.
The test builds a 2000-entry blocklist, confirms `Contains` and
`containsSortedScan` agree on both a present and an absent candidate, and
then uses `testing.AllocsPerRun` to assert `Contains` allocates strictly
less than `containsSortedScan` -- never an exact count, since the runtime's
own allocation behavior for `sort` is not a documented contract.
`TestRun` drives the whole tool end to end: a match, a non-match, and the
three usage failures (missing flag, missing file, unknown flag), all
without touching a real file descriptor for stdin or stdout.

Create `scanblock_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestNewBlocklistSkipsBlankAndCommentLines(t *testing.T) {
	t.Parallel()

	src := "alice\n\n# comment\n  bob  \n"
	b, err := NewBlocklist(strings.NewReader(src))
	if err != nil {
		t.Fatalf("NewBlocklist: %v", err)
	}
	if len(b) != 2 {
		t.Fatalf("len(b) = %d, want 2", len(b))
	}
	if !b.Contains("alice") || !b.Contains("bob") {
		t.Fatalf("b = %v, want alice and bob present (bob trimmed)", b)
	}
}

func TestNewBlocklistEmptySourceIsValid(t *testing.T) {
	t.Parallel()

	b, err := NewBlocklist(strings.NewReader(""))
	if err != nil {
		t.Fatalf("NewBlocklist: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("len(b) = %d, want 0", len(b))
	}
	if b.Contains("anything") {
		t.Fatal("empty blocklist must match nothing")
	}
}

// errReader always fails, standing in for a broken blocklist source or a
// stdin stream that errors mid-read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestFirstBlockedStopsAtFirstMatchInStreamOrder(t *testing.T) {
	t.Parallel()

	block, err := NewBlocklist(strings.NewReader("blocked-a\nblocked-b\n"))
	if err != nil {
		t.Fatalf("NewBlocklist: %v", err)
	}
	// blocked-b appears earlier in the candidate stream than blocked-a even
	// though it sorts after it -- FirstBlocked must return the one it
	// encounters first, not the lexicographically first one.
	candidates := "safe1\nblocked-b\nsafe2\nblocked-a\n"

	id, found, err := FirstBlocked(strings.NewReader(candidates), block)
	if err != nil {
		t.Fatalf("FirstBlocked: %v", err)
	}
	if !found || id != "blocked-b" {
		t.Fatalf("FirstBlocked = (%q, %v), want (\"blocked-b\", true)", id, found)
	}
}

func TestFirstBlockedNoMatch(t *testing.T) {
	t.Parallel()

	block, err := NewBlocklist(strings.NewReader("blocked\n"))
	if err != nil {
		t.Fatalf("NewBlocklist: %v", err)
	}
	id, found, err := FirstBlocked(strings.NewReader("safe1\nsafe2\n"), block)
	if err != nil {
		t.Fatalf("FirstBlocked: %v", err)
	}
	if found || id != "" {
		t.Fatalf("FirstBlocked = (%q, %v), want (\"\", false)", id, found)
	}
}

// TestReadErrorsWrapErrReadFailure checks both call sites that read from an
// io.Reader -- the blocklist source and the candidate stream -- wrap a
// failing Read in the same sentinel, using an empty candidate stream in the
// blocklist case (never reached, since NewBlocklist fails first) and an
// otherwise-valid one-entry blocklist in the candidate case.
func TestReadErrorsWrapErrReadFailure(t *testing.T) {
	t.Parallel()

	if _, err := NewBlocklist(errReader{}); !errors.Is(err, ErrReadFailure) {
		t.Fatalf("NewBlocklist error = %v, want ErrReadFailure", err)
	}

	block, err := NewBlocklist(strings.NewReader("blocked\n"))
	if err != nil {
		t.Fatalf("NewBlocklist: %v", err)
	}
	if _, found, err := FirstBlocked(errReader{}, block); found || !errors.Is(err, ErrReadFailure) {
		t.Fatalf("FirstBlocked = (found=%v, err=%v), want (false, ErrReadFailure)", found, err)
	}
}

// containsSortedScan is the check almost everyone reaches for once they've
// internalized "sort map keys for determinism": collect and sort the whole
// blocklist, then scan for candidate. It is never exported and never
// reached from the package API. Sorting is the right tool when the output
// order itself must be deterministic; a membership check emits no order at
// all, so this buys nothing and pays for a full copy plus a sort per call.
func containsSortedScan(block Blocklist, candidate string) bool {
	hit := false
	for _, k := range slices.Sorted(maps.Keys(block)) {
		if k == candidate {
			hit = true
			break
		}
	}
	return hit
}

// TestContainsAgreesWithSortedScanButAllocatesLess is the whole point of the
// module: both forms must agree on every outcome, and the sorted scan must
// need strictly more allocations than the direct map index -- never an
// exact count, since sort's allocation curve is not a documented contract.
//
// No t.Parallel here: testing.AllocsPerRun panics if run from a parallel
// test, since a concurrent goroutine allocating would skew the measurement.
func TestContainsAgreesWithSortedScanButAllocatesLess(t *testing.T) {
	block := make(Blocklist, 2000)
	for i := range 2000 {
		block["id-"+strconv.Itoa(i)] = struct{}{}
	}

	for _, tc := range []struct {
		name      string
		candidate string
		want      bool
	}{
		{"present", "id-1999", true},
		{"absent", "id-nowhere", false},
	} {
		if got := block.Contains(tc.candidate); got != tc.want {
			t.Fatalf("%s: Contains = %v, want %v", tc.name, got, tc.want)
		}
		if got := containsSortedScan(block, tc.candidate); got != tc.want {
			t.Fatalf("%s: containsSortedScan = %v, want %v", tc.name, got, tc.want)
		}
	}

	exact := testing.AllocsPerRun(50, func() {
		block.Contains("id-1999")
	})
	sorted := testing.AllocsPerRun(50, func() {
		containsSortedScan(block, "id-1999")
	})
	if !(exact < sorted) {
		t.Fatalf("allocations: Contains = %v, containsSortedScan = %v; want Contains < containsSortedScan", exact, sorted)
	}
}

func writeBlocklist(t *testing.T, entries ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(path, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("no match exits clean with empty stdout", func(t *testing.T) {
		t.Parallel()
		path := writeBlocklist(t, "blocked-id")
		var stdout, stderr bytes.Buffer
		matched, err := run([]string{"-blocklist", path}, strings.NewReader("safe1\nsafe2\n"), &stdout, &stderr)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if matched {
			t.Fatal("matched = true, want false")
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
	})

	t.Run("match prints the blocked id", func(t *testing.T) {
		t.Parallel()
		path := writeBlocklist(t, "blocked-id")
		var stdout, stderr bytes.Buffer
		matched, err := run([]string{"-blocklist", path}, strings.NewReader("safe1\nblocked-id\nsafe2\n"), &stdout, &stderr)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !matched {
			t.Fatal("matched = false, want true")
		}
		if stdout.String() != "blocked-id\n" {
			t.Fatalf("stdout = %q, want %q", stdout.String(), "blocked-id\n")
		}
	})

	t.Run("missing -blocklist flag is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		_, err := run(nil, strings.NewReader(""), &stdout, &stderr)
		if err == nil {
			t.Fatal("run: want error for missing -blocklist")
		}
	})

	t.Run("nonexistent blocklist file is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		_, err := run([]string{"-blocklist", "/no/such/file"}, strings.NewReader(""), &stdout, &stderr)
		if err == nil {
			t.Fatal("run: want error for a missing blocklist file")
		}
	})

	t.Run("unknown flag is a usage error", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		_, err := run([]string{"-bogus"}, strings.NewReader(""), &stdout, &stderr)
		if err == nil {
			t.Fatal("run: want error for an unknown flag")
		}
	})
}
```

## Review

`scanblock` is correct when `FirstBlocked` returns the earliest match in the
candidate stream's own order and stops reading the instant it does, and when
`Contains` answers every membership question with a single map index rather
than any iteration at all. The trap this module isolates is subtle because
the wrong version is not a bug in the usual sense -- `containsSortedScan`
returns exactly the right boolean every time. Its defect is cost: it
performs a full copy-and-sort of the blocklist on every call to answer a
question that carries no order requirement, applying the "sort map keys for
deterministic output" habit to a place where no output, ordered or
otherwise, was ever produced. `ErrReadFailure` covers both places the tool
reads a stream, so a broken blocklist file and a broken stdin pipe surface
the same way. The exit code doubles as the answer: 0 for no match, 1 for a
match (with the matched ID on stdout), 2 for anything that kept the check
from running -- a missing flag, an unreadable blocklist, or a bad flag. Run
`go test -count=1 -race ./...` to confirm the parsing table, the
stream-order behavior, the read-error propagation, the allocation contrast,
and `run`'s end-to-end behavior.

## Resources

- [`maps.Keys`](https://pkg.go.dev/maps#Keys) — returns an `iter.Seq[K]`, usable directly with `range` and `break`.
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — the right tool when output order matters; the wrong one here.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — line-oriented streaming without loading the whole input into memory.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe used to pin the cost difference as a property, not a count.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-bounded-cardinality-label-counter.md](17-bounded-cardinality-label-counter.md) | Next: [19-composite-key-quota-tracker.md](19-composite-key-quota-tracker.md)
