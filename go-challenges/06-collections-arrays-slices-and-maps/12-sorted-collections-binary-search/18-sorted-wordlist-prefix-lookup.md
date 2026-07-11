# Exercise 18: Sorted Word-List Prefix Lookup (a look(1) Clone)

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

The Unix `look(1)` utility answers one question fast: given a sorted
dictionary file and a prefix, which lines start with it? `look /usr/share/dict/words
appl` returns `apple`, `application`, `apply` -- instantly, no matter how big
the dictionary is, because it binary-searches the file instead of reading it.
Every spell-checker and autocomplete index that keeps its term list sorted
answers the same query the same way: this is the on-disk ancestor of the
in-memory prefix index a search-as-you-type feature runs today.

The trick `look` depends on is turning "starts with" into a range query.
Binary search answers exact-match and lower-bound questions natively, but
"every word starting with `cat`" is not a single point -- it is every word
`w` such that `cat <= w < cau`. The lower bound is the prefix itself, used as
an ordinary search target. The upper bound is the smallest string strictly
greater than every possible extension of the prefix, and Go strings compare
byte-wise, so that upper bound is the prefix with its last byte incremented
by one: `cat` becomes `cau`, and any string starting with `cat` -- `cats`,
`catalog`, `cat` itself -- sorts strictly below `cau`. Once both bounds exist,
finding the matching run is the exact same lower/upper-bound pair this lesson
already uses for exact-duplicate runs and range queries; a PREFIX match is
just a range query whose bounds happen to be derived rather than given.

This module builds `look`, a command-line tool that reads a sorted word list
from stdin or a file and prints every line matching a prefix, computing that
derived upper bound instead of falling back to a linear scan.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
look/                     module example.com/look
  go.mod                  go 1.24
  look.go                 package main — WordList, NewWordList, Prefix; ErrNotSorted
  look_test.go             package main — Prefix table, incrementLastByte
                          table, the linear-scan contrast, concurrency,
                          run() end to end
  main.go                 package main — -file flag, stdin/stdout, exit codes
```

- Files: `look.go`, `look_test.go`, `main.go`.
- Implement: `type WordList struct { words []string }`; `func NewWordList(words []string) (*WordList, error)`, rejecting an unsorted input with `ErrNotSorted`; `(*WordList).Prefix(prefix string) []string`, returning every word starting with `prefix` by computing a lower bound at `prefix` and an exclusive upper bound at `prefix` with its last non-0xFF byte incremented, then binary-searching both with `slices.BinarySearch`.
- Tool: `look [-file PATH] PREFIX` reads a sorted, newline-delimited word list from stdin, or from `-file` if given, and prints every matching line to stdout. Exit 0 on success (including zero matches), exit 2 for a missing `PREFIX`, an unknown flag, a file that cannot be opened, or a word list that is not sorted, exit 1 for a runtime read failure unrelated to usage.
- Test: an exact multi-word match, a prefix equal to a whole word that also matches its own extensions, an empty prefix, a miss, `incrementLastByte`'s ordinary case, its 0xFF-carry case, and its no-bound cases, `WordList` safety under concurrent `Prefix` calls, the linear-scan contrast, and `run` end to end over `strings.Reader` and `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/look
cd ~/go-exercises/look
go mod init example.com/look
go mod edit -go=1.24
```

### The upper bound for "starts with" is the prefix, one byte later

Every other equal-range exercise in this lesson derives its upper bound from
an existing element -- "the first element strictly greater than this value
that is already in the data." A prefix match has no such element to point
at: `cat` is not itself a word in the list, so there is nothing to read the
next value from. The upper bound has to be *computed* from the prefix
string alone, and the computation is a byte-level trick: find the rightmost
byte of the prefix that is not `0xFF`, add one to it, and drop everything
after it.

```go
func incrementLastByte(prefix string) (upper string, bounded bool) {
    b := []byte(prefix)
    for i := len(b) - 1; i >= 0; i-- {
        if b[i] != 0xFF {
            b[i]++
            return string(b[:i+1]), true
        }
    }
    return "", false
}
```

`"cat"` becomes `"cau"`: `t` (0x74) becomes `u` (0x75). Every string that
starts with `cat` -- compare byte by byte -- agrees with `cat` through the
third character and then continues, and any continuation sorts below `u` at
that position, so it sorts below `"cau"`. The one case this cannot resolve
is a prefix whose every byte is already `0xFF` (or an empty prefix, which
has no last byte at all): there, no finite string is a valid upper bound,
because a match could in principle extend forever with more `0xFF` bytes, so
`Prefix` searches through the end of the list instead. This is the technique
none of the lesson's other equal-range modules need, because they always
search for a value that is already present in the data; here the boundary
is manufactured from the query itself.

Create `look.go`:

```go
// Command look reimplements the classic Unix look(1) utility: given a
// sorted, newline-delimited word list and a prefix, it prints every line
// that starts with that prefix by binary-searching the list instead of
// scanning it.
package main

import (
	"errors"
	"slices"
)

// ErrNotSorted is returned by NewWordList when the supplied words are not
// sorted ascending. A binary-search prefix lookup over an unsorted list
// does not fail loudly; it silently misses matches, so NewWordList rejects
// the input up front instead.
var ErrNotSorted = errors.New("look: word list is not sorted")

// WordList is a sorted, newline-delimited word list searchable by prefix.
//
// WordList is immutable after construction and is safe for concurrent
// Prefix calls from multiple goroutines.
type WordList struct {
	words []string
}

// NewWordList builds a WordList from words, which must already be sorted
// ascending (byte-wise, the same order Go's < operator uses on strings).
// It returns ErrNotSorted if words violates that invariant.
func NewWordList(words []string) (*WordList, error) {
	if !slices.IsSorted(words) {
		return nil, ErrNotSorted
	}
	return &WordList{words: words}, nil
}

// Prefix returns every word in the list that starts with prefix, in sorted
// order. An empty prefix matches every word. A prefix matching nothing
// returns an empty, non-nil slice.
//
// The returned slice aliases WordList's internal storage and is read-only:
// the caller must not write through it, and must not retain it across a
// point where the WordList could be discarded and garbage collected along
// with its backing array, since the returned slice would keep that array
// alive on its own.
func (wl *WordList) Prefix(prefix string) []string {
	lo, _ := slices.BinarySearch(wl.words, prefix)

	upper, bounded := incrementLastByte(prefix)
	hi := len(wl.words)
	if bounded {
		hi, _ = slices.BinarySearch(wl.words, upper)
	}
	return wl.words[lo:hi]
}

// incrementLastByte computes the exclusive upper bound for a PREFIX match:
// the lexicographically smallest string that is strictly greater than every
// string starting with prefix. It finds the rightmost byte of prefix that
// is not 0xFF, increments it, and drops everything after it -- the same
// trick that turns "cat" into "cau", an upper bound that includes "catalog"
// and "cats" but excludes "cau" itself and everything after it.
//
// It reports bounded=false when prefix is empty or every byte is already
// 0xFF, meaning there is no finite upper bound short of the end of the
// list; the caller should then search through the last word instead.
func incrementLastByte(prefix string) (upper string, bounded bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xFF {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}
```

### The tool

`look` has one required input, a prefix, and one optional source, a file
path defaulting to stdin, so `run` takes the argument slice plus an
`io.Reader` for the word list and an `io.Writer` for stdout -- nothing tied
to `os.Args` or a real file handle, which makes every case in the test table
driveable with a `strings.Reader`. Reading proceeds line by line through
`bufio.Scanner` rather than slurping the whole file with `io.ReadAll` and
splitting it by hand, which keeps the tool's memory use proportional to the
word list it actually holds rather than to a second, redundant copy of it.
Every failure short of a genuine I/O error while scanning -- a bad flag, a
missing `PREFIX`, a file that cannot be opened, an unsorted word list -- is
something the caller fixes by changing the command line or the input, so
each wraps the `errUsage` sentinel and `main` maps that to exit code 2. A
scanner error is the one failure this tool can hit that is not the caller's
fault to fix by re-invoking the command differently, and it maps to exit
code 1.

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

// errUsage marks a failure the caller can fix by changing the command
// line or the input file: a bad flag, a missing PREFIX argument, a file
// that cannot be opened, or a word list that is not sorted. main maps it to
// exit code 2. A scanner error while reading the word list is a runtime
// failure unrelated to usage and maps to exit code 1 instead.
var errUsage = errors.New("usage")

// run parses args, loads a sorted word list from -file or from stdin,
// prints every line starting with the requested prefix to stdout, and
// returns an error wrapping errUsage or a plain error depending on which
// kind of failure occurred. It never touches os.Exit, so it can be
// exercised in a test against a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("look", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "path to a sorted word list (default: read stdin)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: expected exactly one PREFIX argument, got %d", errUsage, fs.NArg())
	}
	prefix := fs.Arg(0)

	source := stdin
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		defer f.Close()
		source = f
	}

	var words []string
	scanner := bufio.NewScanner(source)
	for scanner.Scan() {
		words = append(words, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading word list: %w", err)
	}

	wl, err := NewWordList(words)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	for _, w := range wl.Prefix(prefix) {
		fmt.Fprintln(stdout, w)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: look [-file PATH] PREFIX")
		fmt.Fprintln(os.Stderr, "prints every line of a sorted word list starting with PREFIX.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "look:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'apple\napplication\napply\nbanana\nband\nbandana\ncherry\n' > words.txt
go run . -file words.txt appl
go run . band < words.txt
go run . < words.txt
```

Expected output:

```text
apple
application
apply
band
bandana
look: usage: expected exactly one PREFIX argument, got 0
```

The first command reads from `-file` and matches `appl` against three words.
The second reads the same list from stdin and matches `band` against two.
The third omits the required `PREFIX` argument entirely: `run` returns an
error wrapping `errUsage`, `main` prints it to stderr prefixed with `look:`,
and exits with code 2 -- the "usage or invalid input" bucket this tool
reserves for every mistake the caller can fix by re-running the command
differently.

### Tests

`TestWordListPrefix` is the table: several words sharing a prefix, a prefix
that is itself a whole word and also matches its own extensions, an empty
prefix matching everything, and two kinds of miss. `TestNewWordList` checks
both sides of construction: an unsorted input is rejected with
`ErrNotSorted`, and a nil input is accepted as a valid, empty list.
`TestIncrementLastByte` is the derived-boundary logic in isolation: the
ordinary case, the 0xFF-carry case, and the two cases with no finite bound.

`TestLinearScanInspectsEveryWordBinarySearchDoesNot` is the heart of the
module. `linearPrefixScan` is unexported and unreachable from the package
API: it is the naive `strings.HasPrefix` walk over every word, correct but
blind to the fact that the list is sorted. The test counts comparisons for
both strategies over the same five-thousand-word list -- the linear scan
through an instrumented loop, and the same lower/upper-bound search
`Prefix` performs, run through `slices.BinarySearchFunc` with an
instrumented comparator rather than a reimplementation -- and asserts a
property, binary strictly fewer comparisons than linear, never an exact
count tied to this one input size.

Create `look_test.go`:

```go
package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestWordListPrefix(t *testing.T) {
	t.Parallel()

	words := []string{
		"cat", "catalog", "cats", "dog", "doghouse", "dogma", "zebra",
	}
	wl, err := NewWordList(words)
	if err != nil {
		t.Fatalf("NewWordList: %v", err)
	}

	tests := []struct {
		name   string
		prefix string
		want   []string
	}{
		{name: "matches several words", prefix: "cat", want: []string{"cat", "catalog", "cats"}},
		{name: "prefix equal to whole word matches its extensions too", prefix: "dog", want: []string{"dog", "doghouse", "dogma"}},
		{name: "empty prefix matches everything", prefix: "", want: words},
		{name: "no match falls between entries", prefix: "elephant", want: []string{}},
		{name: "no match past every entry", prefix: "zzz", want: []string{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := wl.Prefix(tc.prefix); !slices.Equal(got, tc.want) {
				t.Fatalf("Prefix(%q) = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

func TestNewWordList(t *testing.T) {
	t.Parallel()

	if _, err := NewWordList([]string{"b", "a"}); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("NewWordList(unsorted) error = %v, want ErrNotSorted", err)
	}

	wl, err := NewWordList(nil)
	if err != nil {
		t.Fatalf("NewWordList(nil): %v", err)
	}
	if got := wl.Prefix("anything"); len(got) != 0 {
		t.Fatalf("Prefix on empty list = %v, want empty", got)
	}
}

func TestIncrementLastByte(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		prefix      string
		wantUpper   string
		wantBounded bool
	}{
		{name: "ordinary ascii prefix", prefix: "cat", wantUpper: "cau", wantBounded: true},
		{name: "trailing 0xff carries into the previous byte", prefix: "ab\xff", wantUpper: "ac", wantBounded: true},
		{name: "every byte is 0xff has no bound", prefix: "\xff\xff", wantBounded: false},
		{name: "empty prefix has no bound", prefix: "", wantBounded: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upper, bounded := incrementLastByte(tc.prefix)
			if bounded != tc.wantBounded || (bounded && upper != tc.wantUpper) {
				t.Fatalf("incrementLastByte(%q) = (%q, %v), want (%q, %v)",
					tc.prefix, upper, bounded, tc.wantUpper, tc.wantBounded)
			}
		})
	}
}

func TestWordListPrefixSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	words := make([]string, 200)
	for i := range words {
		words[i] = fmt.Sprintf("word%03d", i)
	}
	wl, err := NewWordList(words)
	if err != nil {
		t.Fatalf("NewWordList: %v", err)
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := wl.Prefix("word01"); len(got) != 10 {
				t.Errorf("Prefix(\"word01\") len = %d, want 10", len(got))
			}
		}()
	}
	wg.Wait()
}

// linearPrefixScan is the antipattern this module contrasts, kept
// unexported and unreachable from the package API: it checks every word
// with strings.HasPrefix, one at a time, ignoring that the list is sorted.
// It agrees with WordList.Prefix on every input but inspects the entire
// list on every call.
func linearPrefixScan(words []string, prefix string, comparisons *int) []string {
	out := make([]string, 0)
	for _, w := range words {
		*comparisons++
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}

// TestLinearScanInspectsEveryWordBinarySearchDoesNot is the heart of the
// module: it counts comparisons for linearPrefixScan versus the same
// lower/upper-bound search Prefix runs (via slices.BinarySearchFunc with an
// instrumented comparator, not a reimplementation) and asserts a property,
// never an exact count tied to this input size.
func TestLinearScanInspectsEveryWordBinarySearchDoesNot(t *testing.T) {
	t.Parallel()

	words := make([]string, 5000)
	for i := range words {
		words[i] = fmt.Sprintf("word%05d", i)
	}
	const prefix = "word04999"

	var linearComparisons int
	linearPrefixScan(words, prefix, &linearComparisons)

	var binaryComparisons int
	countedCompare := func(w, target string) int {
		binaryComparisons++
		return cmp.Compare(w, target)
	}
	_, _ = slices.BinarySearchFunc(words, prefix, countedCompare)
	if upper, bounded := incrementLastByte(prefix); bounded {
		_, _ = slices.BinarySearchFunc(words, upper, countedCompare)
	}

	if !(binaryComparisons < linearComparisons) {
		t.Fatalf("comparisons: binary = %d, linear = %d; want binary strictly fewer", binaryComparisons, linearComparisons)
	}
	if linearComparisons != len(words) {
		t.Fatalf("linear scan performed %d comparisons, want %d (a full pass)", linearComparisons, len(words))
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "words.txt")
	if err := os.WriteFile(filePath, []byte("alpha\nbeta\nbetamax\ngamma\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name      string
		args      []string
		stdin     io.Reader
		want      string
		wantErr   bool
		wantUsage bool
	}{
		{name: "matches from stdin", args: []string{"band"}, stdin: strings.NewReader("apple\nbanana\nband\nbandana\ncherry\n"), want: "band\nbandana\n"},
		{name: "missing prefix argument is a usage error", args: []string{}, stdin: strings.NewReader(""), wantErr: true, wantUsage: true},
		{name: "unsorted input is a usage error", args: []string{"a"}, stdin: strings.NewReader("banana\napple\n"), wantErr: true, wantUsage: true},
		{name: "-file reads the word list instead of stdin", args: []string{"-file", filePath, "beta"}, stdin: strings.NewReader(""), want: "beta\nbetamax\n"},
		{name: "a read failure is a runtime error, not usage", args: []string{"band"}, stdin: errReader{}, wantErr: true, wantUsage: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, tc.stdin, &stdout)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if tc.wantUsage != errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want wraps errUsage = %v", tc.args, err, tc.wantUsage)
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

// errReader is an io.Reader that always fails, used to exercise the
// runtime-failure branch of run (a scanner error unrelated to usage).
type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated read failure")
}
```

## Review

`look` is correct when `Prefix` returns exactly the words that start with
the given prefix -- no more, no less -- for a prefix that matches several
words, one word, none, or every word. The mechanism worth internalizing is
`incrementLastByte`: it turns a PREFIX query, which has no natural upper
bound the way an exact-duplicate run does, into the same half-open
`[lo, hi)` lower/upper-bound pair every other range query in this lesson
uses, by manufacturing `hi` from the prefix itself rather than reading it
off an existing element. `NewWordList` rejects an unsorted input with
`ErrNotSorted` instead of returning silently wrong answers, and `WordList`
is immutable after construction, so it is safe to share across every
`Prefix` call a concurrent handler might make. `run` keeps that logic
separate from `os.Args` and `os.Exit`, streaming the input through
`bufio.Scanner`, mapping every input mistake to exit code 2 and a genuine
read failure to exit code 1. Run `go test -count=1 -race ./...` to confirm
the prefix table, the boundary-derivation table, the linear-scan contrast,
and `run`'s end-to-end behavior.

## Resources

- [`slices.BinarySearch`](https://pkg.go.dev/slices#BinarySearch) — the lower/upper-bound search this module runs twice per query.
- [`look(1)`](https://man.openbsd.org/look.1) — the original Unix utility this module reimplements.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented streaming reader `run` uses instead of loading the file with `io.ReadAll`.
- [`strings.HasPrefix`](https://pkg.go.dev/strings#HasPrefix) — the linear-scan primitive the antipattern uses, correct but blind to sort order.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-paginated-log-exponential-seek.md](17-paginated-log-exponential-seek.md) | Next: [19-order-book-price-level-insert.md](19-order-book-price-level-insert.md)
