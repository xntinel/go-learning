# Exercise 10: Inverted Postings Index That Keeps Every Match

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A search engine's inverted index is the data structure Elasticsearch and Lucene
build during indexing: for every term, the list of every document that contains
it. It is called "inverted" because a document naturally lists its own terms,
and the index flips that relationship around -- term to documents, not document
to terms -- so that answering "which documents contain go" is a single map
lookup instead of a scan over every document. Every full-text search feature a
backend ships, from a support-ticket search box to a log grep endpoint, sits on
top of exactly this structure.

The trap is almost invisible in a first draft. A term-to-document mapping looks
like a one-to-one relationship if you only ever test it with one document per
term: `map[string]docID` compiles, passes a smoke test, and silently discards
every collision. A term that appears in ten documents does not error, does not
panic, it just ends up pointing at whichever document was indexed last -- the
other nine matches vanish from search results with no signal that anything went
wrong. This is the same "last write wins" collapse the `maps` package documents
for `Invert` and `Collect` on duplicate values, and it is exactly why a real
postings list is `map[string][]string`, built by appending, never by
overwriting.

This exercise builds `postingsidx`, a command-line tool that reads a tiny
document corpus from stdin, one document per line, and prints the inverted
index: every term next to the sorted, deduplicated list of documents that
contain it. A `-min-df` flag filters out terms below a minimum document
frequency, the same knob a real search engine uses to drop stopwords that
appear in nearly every document and add no discriminating power to a query.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
postingsidx/                   module example.com/postingsidx
  go.mod                       go 1.24
  postingsidx.go                package main — Index, NewIndex, Add, Postings, DocFrequency, Terms, BuildIndex
  postingsidx_test.go           package main — accumulation table, malformed-line table, lastWriteWins contrast, run() end to end
  main.go                       package main — -min-df flag, exit codes
```

- Files: `postingsidx.go`, `postingsidx_test.go`, `main.go`.
- Implement: `type Index struct{...}`; `NewIndex() *Index`; `(*Index) Add(docID string, terms []string)` appending postings; `(*Index) Postings(term string) []string` returning the sorted, deduplicated list; `(*Index) DocFrequency(term string) int`; `(*Index) Terms() []string` sorted; `BuildIndex(r io.Reader) (*Index, error)` streaming one line at a time, rejecting a malformed line with `ErrMalformedLine` and the 1-based line number.
- Tool: `postingsidx` reads `docID: term term term` lines from stdin, takes `-min-df N` (default 1), and prints each qualifying term with its posting list to stdout. Exit 0 on success, exit 2 for a malformed line or a bad flag, exit 1 is reserved for a runtime failure reading stdin.
- Test: accumulation and deduplication across repeated terms and repeated documents; every malformed-line shape (no colon, empty docID, no terms); blank lines skipped; the `lastWriteWins` contrast proving a one-to-one map collapses three documents into one; `run` end to end for the default and filtered output and every rejected input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One term, many documents: why the value has to be a slice

The instinct that produces the bug is reasonable on its face: "for each term,
remember its document" reads naturally as `map[string]string`. It even works,
right up until the same term shows up in a second document:

```go
index := make(map[string]string)
for docID, terms := range corpus {
    for _, t := range terms {
        index[t] = docID   // the second document with "go" erases the first
    }
}
```

There is no error here, no panic, nothing `go vet` or the race detector can
flag. The map assignment is a perfectly ordinary overwrite, and Go's map
iteration order is randomized per range, so which document survives for a
given term is not even deterministic across runs. A term present in ten
documents silently becomes a term present in exactly one, chosen by whichever
line the runtime happened to visit last. Search results for that term are
wrong in the one direction nobody's smoke test catches: too few matches,
not zero.

The fix is the shape of the value, not a smarter assignment: `map[string][]string`,
built with `append` instead of `=`. Appending never discards a prior entry, so
every document that contains a term stays reachable through it. The postings
list still needs a pass of `slices.Sort` plus `slices.Compact` before it is fit
to print, both because Go's map iteration order is randomized -- two runs over
the same corpus must not print documents in a different order -- and because a
single document can mention the same term twice within one line, which would
otherwise leave a duplicate docID sitting in its own postings list.

Create `postingsidx.go`:

```go
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// ErrMalformedLine is returned by BuildIndex when an input line does not
// match the "docID: term term term" format.
var ErrMalformedLine = errors.New("postingsidx: malformed line")

// Index is an inverted postings index: it maps each term to the sorted,
// deduplicated list of document IDs that contain it.
//
// Index is not safe for concurrent use while it is being built; call Add
// only from a single goroutine. Once building is finished, Terms and
// Postings are safe for concurrent use by multiple goroutines: they sort and
// read the already-populated map and perform no further writes to it beyond
// caching that sort, which is idempotent.
type Index struct {
	postings map[string][]string
}

// NewIndex returns an empty Index ready to accept postings via Add.
func NewIndex() *Index {
	return &Index{postings: make(map[string][]string)}
}

// Add records that docID contains every term in terms. It accumulates: a
// term already indexed keeps every prior docID and gains this one. That is
// the fix for the one-to-one "last write wins" mapping described in the
// package tests, where a plain map[string]string overwrites on every
// collision instead of appending.
func (idx *Index) Add(docID string, terms []string) {
	for _, t := range terms {
		idx.postings[t] = append(idx.postings[t], docID)
	}
}

// Postings returns the sorted, deduplicated list of document IDs that
// contain term, or nil if the term was never indexed.
//
// The returned slice aliases Index's internal storage. Callers must not
// mutate it; call slices.Clone to retain a private copy across further calls
// to Add.
func (idx *Index) Postings(term string) []string {
	ids, ok := idx.postings[term]
	if !ok || len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	idx.postings[term] = ids
	return ids
}

// DocFrequency reports the number of distinct documents that contain term:
// len(idx.Postings(term)).
func (idx *Index) DocFrequency(term string) int {
	return len(idx.Postings(term))
}

// Terms returns every indexed term in sorted order. Ranging idx's internal
// map directly would visit terms in the runtime's randomized order; Terms
// sorts so that two runs over the same input always print in the same
// order.
func (idx *Index) Terms() []string {
	return slices.Sorted(maps.Keys(idx.postings))
}

// BuildIndex reads "docID: term term term" lines from r, one document per
// line, and returns the resulting Index. It streams: each line is parsed
// and its tokens appended to the index as it is read, so memory use is
// proportional to the number of distinct terms and postings, not to the
// size of r.
//
// A blank line (after trimming whitespace) is skipped. Any other line that
// has no ':' separator, an empty docID, or no terms after the separator
// fails with an error wrapping ErrMalformedLine and the 1-based line
// number.
func BuildIndex(r io.Reader) (*Index, error) {
	idx := NewIndex()
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		docID, rest, ok := strings.Cut(line, ":")
		docID = strings.TrimSpace(docID)
		if !ok || docID == "" {
			return nil, fmt.Errorf("line %d: %w: %q", lineNo, ErrMalformedLine, line)
		}
		terms := strings.Fields(rest)
		if len(terms) == 0 {
			return nil, fmt.Errorf("line %d: %w: %q", lineNo, ErrMalformedLine, line)
		}
		idx.Add(docID, terms)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("postingsidx: reading input: %w", err)
	}
	return idx, nil
}
```

### The tool

`run` takes the flag arguments, an `io.Reader` for stdin, and an `io.Writer`
for stdout, so a test drives it with a `strings.Reader` and a `bytes.Buffer`
and never touches `os.Args` or a real terminal. `BuildIndex` streams: it reads
one line at a time with `bufio.Scanner` and appends into the index as it
goes, rather than reading all of stdin into memory first, which matters once
the corpus is larger than what comfortably fits in RAM. Both a bad flag and a
malformed input line are the caller's mistake to fix -- wrong command line or
wrong input file -- so both wrap `errUsage` and `main` maps that to exit code
2; exit code 1 is reserved for a genuine I/O failure reading stdin, which
`BuildIndex` reports separately, without wrapping `ErrMalformedLine`.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input: a bad flag, or a malformed line BuildIndex rejects. main
// maps it, and any error wrapping ErrMalformedLine, to exit code 2; any
// other error is a runtime failure and maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, builds the postings index from stdin, and writes one
// line per qualifying term to stdout: "term: docID docID docID", terms in
// sorted order, each posting list sorted and deduplicated. Only terms whose
// document frequency is at least minDF are printed.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("postingsidx", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	minDF := fs.Int("min-df", 1, "minimum document frequency required to print a term")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *minDF < 1 {
		return fmt.Errorf("%w: -min-df must be at least 1, got %d", errUsage, *minDF)
	}

	idx, err := BuildIndex(stdin)
	if err != nil {
		if errors.Is(err, ErrMalformedLine) {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}

	for _, term := range idx.Terms() {
		postings := idx.Postings(term)
		if len(postings) < *minDF {
			continue
		}
		fmt.Fprintf(stdout, "%s: %s\n", term, strings.Join(postings, " "))
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: postingsidx [-min-df N] < input")
		fmt.Fprintln(os.Stderr, `reads "docID: term term term" lines from stdin and prints each term's posting list.`)
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "postingsidx:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'doc1: go rust python\ndoc2: go rust\ndoc3: go\n' | go run .
printf 'doc1: go rust python\ndoc2: go rust\ndoc3: go\n' | go run . -min-df 2
printf 'doc1 go rust\n' | go run .
```

Expected output:

```text
go: doc1 doc2 doc3
python: doc1
rust: doc1 doc2
go: doc1 doc2 doc3
rust: doc1 doc2
postingsidx: usage: line 1: postingsidx: malformed line: "doc1 go rust"
```

The first run shows every term at its full document frequency: `go` reaches
all three documents, `rust` reaches two, `python` only one. The second run,
`-min-df 2`, drops `python` -- exactly the filter a real index uses to hide
terms too rare to be useful, or (inverted) too common to discriminate a query.
The third run feeds a line with no `:` separator and shows the exit-2 usage
error, wrapping the line number `BuildIndex` attached to `ErrMalformedLine`.

### Tests

`TestLastWriteWinsCollapsesDuplicates` is the module's center of gravity:
`lastWriteWins` is unexported, unreachable from `Index`'s API, and exists only
so the test can state the defect precisely -- three documents that all contain
`go` collapse to exactly one entry in a `map[string]string` -- and then show
the same three documents through `Index.Add` and `Postings` producing all
three, sorted. `TestBuildIndexAccumulatesAndDeduplicates` covers a term
repeated within one line, a term shared across lines, and a term unique to
one document, checking `Postings`, `DocFrequency`, and `Terms` together.
`TestBuildIndexRejectsMalformedLines` sweeps every shape of bad input:
no separator, an empty docID, no terms after the separator, and a docID that
is only whitespace. `TestRun` drives the whole tool end to end, including the
`-min-df` filter and every usage-error path.

Create `postingsidx_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// lastWriteWins is the naive one-to-one index that ships when a term-to-doc
// mapping is written as map[string]string instead of map[string][]string.
// Every collision overwrites the previous entry, so a term that appears in
// several documents ends up mapped to only the most recently indexed one.
// It is never exported and never reachable from the index API; it exists so
// the tests can pin what it gets wrong.
func lastWriteWins(docs map[string][]string) map[string]string {
	out := make(map[string]string)
	for docID, terms := range docs {
		for _, t := range terms {
			out[t] = docID // overwrites any earlier document for this term
		}
	}
	return out
}

func TestLastWriteWinsCollapsesDuplicates(t *testing.T) {
	t.Parallel()

	docs := map[string][]string{"doc1": {"go"}, "doc2": {"go"}, "doc3": {"go"}}
	buggy := lastWriteWins(docs)
	if len(buggy) != 1 {
		t.Fatalf("lastWriteWins produced %d entries for one term, want 1", len(buggy))
	}
	if !slices.Contains([]string{"doc1", "doc2", "doc3"}, buggy["go"]) {
		t.Fatalf("lastWriteWins[go] = %q, want one of doc1/doc2/doc3", buggy["go"])
	}

	idx := NewIndex()
	for docID, terms := range docs {
		idx.Add(docID, terms)
	}
	want := []string{"doc1", "doc2", "doc3"}
	if got := idx.Postings("go"); !slices.Equal(got, want) {
		t.Fatalf("Postings(go) = %v, want %v", got, want)
	}
}

func TestBuildIndexAccumulatesAndDeduplicates(t *testing.T) {
	t.Parallel()

	idx, err := BuildIndex(strings.NewReader(
		"doc1: go rust go\n" + "doc2: rust\n" + "doc3: go python\n" + "\n   \n"))
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	postings := []struct {
		term string
		want []string
		df   int
	}{
		{"go", []string{"doc1", "doc3"}, 2},
		{"rust", []string{"doc1", "doc2"}, 2},
		{"python", []string{"doc3"}, 1},
		{"missing", nil, 0},
	}
	for _, tc := range postings {
		if got := idx.Postings(tc.term); !slices.Equal(got, tc.want) {
			t.Errorf("Postings(%q) = %v, want %v", tc.term, got, tc.want)
		}
		if got := idx.DocFrequency(tc.term); got != tc.df {
			t.Errorf("DocFrequency(%q) = %d, want %d", tc.term, got, tc.df)
		}
	}

	wantTerms := []string{"go", "python", "rust"}
	if got := idx.Terms(); !slices.Equal(got, wantTerms) {
		t.Fatalf("Terms() = %v, want %v", got, wantTerms)
	}
}

func TestBuildIndexRejectsMalformedLines(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, input string }{
		{"no colon", "doc1 go rust\n"},
		{"empty docID", ": go rust\n"},
		{"no terms after colon", "doc1:\n"},
		{"blank docID with spaces", "   : go\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := BuildIndex(strings.NewReader(tc.input)); !errors.Is(err, ErrMalformedLine) {
				t.Fatalf("BuildIndex(%q) error = %v, want ErrMalformedLine", tc.input, err)
			}
		})
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
	}{
		{
			name:  "default min-df prints every term",
			stdin: "doc1: go rust\ndoc2: go\n",
			want:  "go: doc1 doc2\nrust: doc1\n",
		},
		{
			name:  "min-df filters rare terms",
			args:  []string{"-min-df", "2"},
			stdin: "doc1: go rust\ndoc2: go\n",
			want:  "go: doc1 doc2\n",
		},
		{name: "malformed line is a usage error", stdin: "doc1 go rust\n", wantErr: true},
		{name: "min-df below one is a usage error", args: []string{"-min-df", "0"}, stdin: "doc1: go\n", wantErr: true},
		{name: "unknown flag is a usage error", args: []string{"-bogus"}, stdin: "doc1: go\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
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
```

## Review

The index is correct when every document that mentions a term is still
reachable through it -- that is the entire point of an inverted index, and
the entire way to break one is a value type that can only hold one document
at a time. `map[string][]string` built with `append` fixes that; a plain
`map[string]string` built with `=` is the bug, silently dropping every
document but the last for any term with more than one match, which
`TestLastWriteWinsCollapsesDuplicates` pins directly against `lastWriteWins`.
Around that core, `BuildIndex` streams its input, rejects a malformed line
with the 1-based line number attached, and `Postings` always returns a
sorted, deduplicated list so two runs over the same corpus print identically
regardless of the runtime's randomized map iteration order. The tool exits 2
for a malformed line or a bad flag and reserves exit 1 for a genuine failure
reading stdin. Run `go test -count=1 -race ./...`.

## Resources

- [`maps` package: Common Mistakes, Invert/Collect on duplicates](00-concepts.md) — why a one-to-one map collapses colliding values.
- [`slices.Sort`](https://pkg.go.dev/slices#Sort) and [`slices.Compact`](https://pkg.go.dev/slices#Compact) — sorting and deduplicating a postings list before printing it.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the streaming line reader `BuildIndex` uses instead of buffering all of stdin.
- [Wikipedia: Inverted index](https://en.wikipedia.org/wiki/Inverted_index) — the data structure this exercise builds, and how Lucene/Elasticsearch use it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-grouped-index-pagination.md](09-grouped-index-pagination.md) | Next: [11-quorum-capacity-lazy-iterator-scan.md](11-quorum-capacity-lazy-iterator-scan.md)
