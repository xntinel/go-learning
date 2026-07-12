# Exercise 18: CSV Bulk Import: NULL Marker Versus Empty Cell

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A bulk loader that ingests a Postgres `COPY`-style CSV export before an
upsert has to carry the same nil-vs-empty distinction this lesson has been
building around JSON, but into a text format with no native concept of nil
at all. `COPY ... TO ... CSV` spells SQL `NULL` as a literal `\N` by
convention -- not an empty field, a specific two-character marker chosen
precisely because it cannot occur in ordinary text. A `tags` column holding
a comma-separated list can therefore show up three ways in the export: the
field is `\N` (this row's tags were never set), the field is the empty
string (this row's tags were explicitly cleared to an empty list), or the
field holds `vip,eu,beta` (this row has tags). Those are the CSV equivalents
of absent, `null`, and `[]`, and an importer that does not keep them apart
loses information the source database had and will never get it back.

The failure mode is quiet rather than a panic: an importer that treats both
`\N` and an empty cell as "no tags" and always produces an empty slice
compiles, runs, and imports every row successfully. The bug only shows up
later, in whatever downstream logic asks "were tags ever set on this row" --
a data-quality report, a migration verifying old data survived, a customer
asking why their explicitly-cleared tag list looks identical to a row that
was never tagged. By then the file has already been parsed and the
distinction is gone; there is no way to recover it from the imported records
alone.

This module builds `importtags`, a tool that reads exactly that export
format and prints, per row, which of the three shapes it saw: `nil (NULL)`
for the marker, `[] (empty)` for an explicit empty list, or `[a b c]` for a
real one. The naive "both spellings mean no tags" collapse never appears in
the tool's own parsing path -- it exists only in the test file, as the thing
the tests prove wrong.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
importtags/                    module example.com/importtags
  go.mod                       go 1.24
  importtags.go                package main — Record, Importer; NewImporter, ParseRow, Format
  importtags_test.go           package main — the parse table, malformed-row and constructor
                                validation, the tagsNaive contrast, run() end to end
  main.go                      package main — -null flag, file/stdin source, exit codes
```

- Files: `importtags.go`, `importtags_test.go`, `main.go`.
- Implement: `NewImporter(nullMarker string) (*Importer, error)` rejecting an empty marker with `ErrEmptyNullMarker`; `(*Importer).ParseRow(row []string) (Record, error)` rejecting a row that is not exactly two columns with `ErrMalformedRow`, translating the tags cell into a nil, non-nil-empty, or populated `[]string`; `(Record).Format() string`.
- Tool: `importtags` reads a CSV source -- a named file argument, or stdin when the argument is `-` -- with header `id,tags`, and a `-null` flag naming the SQL-NULL marker (default `\N`). It prints one formatted line per data row to stdout. Exit 0 on success; exit 2 for a missing or extra source argument, an unreadable file, a wrong header, or a malformed row (the stream stops at the first one); exit 1 is reserved for a runtime failure this tool never produces.
- Test: the parse table across the null marker, an empty cell, one tag, and multiple tags; malformed-row and empty-marker validation; a `tagsNaive` contrast proving a length-based "no tags" check collapses the null marker and an empty cell to the same answer; `run` end to end over stdin and over a real temporary file, including a missing file and a bad header.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A text format has no nil, so the importer has to invent one

CSV cannot represent absence the way JSON can with `null`, or a slice header
can with a nil pointer: every cell is a string, and an empty cell is
indistinguishable at the syntax level from a cell that was deliberately left
blank. Postgres's `COPY ... CSV` solves this on the way out by reserving a
literal marker, `\N` by default, that stands for SQL `NULL` specifically so
it can be told apart from an empty string. On the way back in, the importer
has to reverse that encoding by hand -- nothing in `encoding/csv` knows about
it, because it is a Postgres convention, not a CSV one:

```go
func tagsWrong(cell string) []string {
    if cell == "" || cell == `\N` {
        return []string{}   // both spellings become the same []string{}
    }
    return strings.Split(cell, ",")
}
```

This reads as reasonable -- "if there's nothing here, return an empty
slice" -- and it is exactly the mistake: it maps two different facts about
the source row onto one Go value. The correct translation keeps a third
value in play, the nil slice, precisely because Go already has the
nil-vs-empty distinction this lesson is about; the importer's whole job is
routing the marker to nil and the empty cell to a non-nil `[]string{}`
instead of routing both to the same place.

Create `importtags.go`:

```go
// Package main implements importtags, a bulk loader for a Postgres
// COPY-style CSV export with an id column and a comma-separated tags column.
// The export marks SQL NULL with a literal \N, distinct from an empty cell,
// which means an explicit empty list -- CSV has no native nil, so those two
// input shapes must be translated into a []string(nil) versus a non-nil
// []string{} on the Go side, or the distinction the source database made is
// lost the moment the file is parsed.
package main

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyNullMarker is returned by NewImporter when the configured null
// marker is empty.
var ErrEmptyNullMarker = errors.New("importtags: null marker must not be empty")

// ErrMalformedRow is returned by ParseRow when a row does not have exactly
// two columns. It wraps the offending row for context.
var ErrMalformedRow = errors.New("importtags: row must have exactly 2 columns (id,tags)")

// Record is one imported row: an id and its tags, translated from the CSV's
// three possible spellings of the tags column into Go's nil-vs-empty slice
// distinction.
type Record struct {
	ID string
	// Tags is nil if the source cell was the configured null marker
	// (SQL NULL: tags were never set), non-nil and empty if the source cell
	// was the empty string (SQL '{}': tags were explicitly cleared), and
	// non-nil and non-empty otherwise.
	Tags []string
}

// Importer parses CSV rows of the form id,tags into Records, translating the
// tags column's null marker and empty-cell conventions into Go's nil-vs-empty
// slice distinction.
//
// An Importer holds only its configured null marker after construction and
// is safe for concurrent use by multiple goroutines; ParseRow does not
// mutate the Importer.
type Importer struct {
	nullMarker string
}

// NewImporter returns an Importer that treats any tags cell equal to
// nullMarker as SQL NULL. It returns ErrEmptyNullMarker if nullMarker is
// empty.
func NewImporter(nullMarker string) (*Importer, error) {
	if nullMarker == "" {
		return nil, ErrEmptyNullMarker
	}
	return &Importer{nullMarker: nullMarker}, nil
}

// ParseRow converts one CSV data row into a Record. row must have exactly
// two columns, id and tags; ParseRow returns ErrMalformedRow otherwise.
//
// The tags column is classified before it is split: equal to the configured
// null marker produces a nil Tags (SQL NULL, tags were never set); the empty
// string produces a non-nil, empty Tags (SQL '{}', tags were explicitly
// cleared); anything else is split on commas into the tag list. The returned
// Record's Tags slice is freshly allocated and does not alias row.
func (imp *Importer) ParseRow(row []string) (Record, error) {
	if len(row) != 2 {
		return Record{}, fmt.Errorf("%w: got %d columns: %v", ErrMalformedRow, len(row), row)
	}
	id, cell := row[0], row[1]

	switch {
	case cell == imp.nullMarker:
		return Record{ID: id, Tags: nil}, nil
	case cell == "":
		return Record{ID: id, Tags: []string{}}, nil
	default:
		parts := strings.Split(cell, ",")
		tags := make([]string, len(parts))
		copy(tags, parts)
		return Record{ID: id, Tags: tags}, nil
	}
}

// Format renders a Record the way the tool prints it: "id: nil (NULL)" for a
// nil Tags, "id: [] (empty)" for a non-nil empty Tags, and "id: [a b c]"
// otherwise.
func (r Record) Format() string {
	switch {
	case r.Tags == nil:
		return fmt.Sprintf("%s: nil (NULL)", r.ID)
	case len(r.Tags) == 0:
		return fmt.Sprintf("%s: [] (empty)", r.ID)
	default:
		return fmt.Sprintf("%s: [%s]", r.ID, strings.Join(r.Tags, " "))
	}
}
```

### The tool

`importtags` streams the CSV row by row with `encoding/csv.Reader` rather
than loading the whole export into memory, which matters for a bulk loader
meant to run against a real production-sized export. `run` takes the
argument slice plus an `io.Reader`/`io.Writer` pair standing in for stdin and
stdout, so a test drives it against either a `strings.Reader` or a real
temporary file with no process boundary involved. Setting
`r.FieldsPerRecord = 2` makes `encoding/csv` itself reject a row with the
wrong column count before `ParseRow` ever sees it, and every failure `run`
can produce -- a bad flag, a missing or extra source argument, an unreadable
file, a wrong header, a malformed row -- wraps the `errUsage` sentinel, which
`main` maps to exit code 2. The stream stops at the first bad row rather than
skipping it and continuing, because a bulk loader that silently drops rows
is a worse failure mode than one that stops and says exactly where it
stopped.

Create `main.go`:

```go
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input: a bad
// flag, a missing source argument, an unreadable file, a bad header, or a
// malformed row. main maps it to exit code 2; every other error maps to
// exit code 1.
var errUsage = errors.New("usage")

// wantHeader is the exact header row this importer's CSV export format uses.
var wantHeader = []string{"id", "tags"}

// run parses args, reads one CSV source (a named file, or stdin when the
// source argument is "-"), and writes one formatted line per data row to
// stdout. It never touches os.Exit directly, so it can be exercised in a
// test with a strings.Reader standing in for stdin and a bytes.Buffer
// collecting stdout.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("importtags", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	nullMarker := fs.String("null", `\N`, "literal string marking SQL NULL in the tags column")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: expected exactly one source argument (a file path or -), got %d", errUsage, fs.NArg())
	}

	src := stdin
	if source := fs.Arg(0); source != "-" {
		f, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		defer f.Close()
		src = f
	}

	imp, err := NewImporter(*nullMarker)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	r := csv.NewReader(src)
	r.FieldsPerRecord = 2

	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("%w: reading header: %v", errUsage, err)
	}
	if header[0] != wantHeader[0] || header[1] != wantHeader[1] {
		return fmt.Errorf("%w: header = %v, want %v", errUsage, header, wantHeader)
	}

	lineNum := 1
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		lineNum++
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNum, err)
		}
		rec, err := imp.ParseRow(row)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNum, err)
		}
		fmt.Fprintln(stdout, rec.Format())
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: importtags [-null MARKER] (FILE | -)")
		fmt.Fprintln(os.Stderr, "reads a CSV export with columns id,tags and prints one line per")
		fmt.Fprintln(os.Stderr, "record classifying tags as nil (NULL), [] (empty), or [a b c].")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "importtags:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'id,tags\n1,\\N\n2,\n3,"vip,eu,beta"\n' | go run . -
printf 'id,tags\n1,vip,extra\n' | go run . -
```

Expected output:

```text
1: nil (NULL)
2: [] (empty)
3: [vip eu beta]
importtags: usage: line 2: record on line 2: wrong number of fields
```

The first three lines are the three shapes an export can carry for the tags
column: the marker, an empty cell, and a quoted CSV field holding a
comma-separated list (quoting is what lets the field's own commas survive
CSV's own comma-as-delimiter syntax). The last line is the second
invocation: row 2 has three columns instead of two, `encoding/csv` itself
rejects it because `FieldsPerRecord` was set to 2, and `run` reports that as
a usage error on stderr with exit code 2 -- the tool never imports a
partial row.

### Tests

`TestParseRow` is the table that matters most: the null marker, an empty
cell, a single tag, and a multi-tag cell, each checked for both the correct
`ID` and the correct nil-vs-non-nil-vs-populated `Tags`. `TestValidation`
covers the two rejection paths -- a malformed row checked directly against
`ParseRow` (independent of `encoding/csv`'s own enforcement, so the domain
logic's own validation is pinned on its own) and an empty null marker at
construction. `TestRecordFormat` pins the exact three renderings a caller
sees on stdout.

`TestTagsNaiveLosesNullVsEmpty` is the antipattern contrast. `tagsNaive` is
unexported and unreachable from the tool's own parsing path; the test shows
it collapses the null marker and an empty cell to the same length-zero
slice, then shows `ParseRow` keeping them apart as nil versus non-nil.
`TestRun` drives the command over stdin: a clean multi-row stream, a missing
source argument, a wrong header, and a malformed row, the last three each
checked against `errUsage`. `TestRunFileSource` proves the real-file path
works identically to `-`, and that a missing file is a usage error rather
than a panic.

Create `importtags_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tagsNaive collapses the null marker and an empty cell to the same
// []string{}. Never exported or reachable from the tool's parsing path: it
// exists only so the tests can pin what it loses.
func tagsNaive(cell string) []string {
	if cell == "" || cell == `\N` {
		return []string{}
	}
	return strings.Split(cell, ",")
}

func TestParseRow(t *testing.T) {
	t.Parallel()

	imp, err := NewImporter(`\N`)
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}

	tests := []struct {
		name     string
		row      []string
		wantID   string
		wantTags []string
		wantNil  bool
	}{
		{name: "null marker", row: []string{"1", `\N`}, wantID: "1", wantNil: true},
		{name: "empty cell", row: []string{"2", ""}, wantID: "2", wantTags: []string{}},
		{name: "single tag", row: []string{"3", "vip"}, wantID: "3", wantTags: []string{"vip"}},
		{name: "multiple tags", row: []string{"4", "vip,eu,beta"}, wantID: "4", wantTags: []string{"vip", "eu", "beta"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec, err := imp.ParseRow(tc.row)
			if err != nil {
				t.Fatalf("ParseRow(%v): %v", tc.row, err)
			}
			if rec.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", rec.ID, tc.wantID)
			}
			if tc.wantNil {
				if rec.Tags != nil {
					t.Errorf("Tags = %v, want nil", rec.Tags)
				}
				return
			}
			if rec.Tags == nil || len(rec.Tags) != len(tc.wantTags) {
				t.Fatalf("Tags = %v, want %v", rec.Tags, tc.wantTags)
			}
			for i, tag := range tc.wantTags {
				if rec.Tags[i] != tag {
					t.Errorf("Tags[%d] = %q, want %q", i, rec.Tags[i], tag)
				}
			}
		})
	}
}

// TestValidation covers a malformed row (checked directly against ParseRow,
// independent of encoding/csv's own field-count enforcement) and an empty
// null marker at construction time.
func TestValidation(t *testing.T) {
	t.Parallel()
	imp, err := NewImporter(`\N`)
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	for _, row := range [][]string{{"1"}, {"1", "vip", "extra"}, {}} {
		if _, err := imp.ParseRow(row); !errors.Is(err, ErrMalformedRow) {
			t.Errorf("ParseRow(%v) error = %v, want ErrMalformedRow", row, err)
		}
	}
	if _, err := NewImporter(""); !errors.Is(err, ErrEmptyNullMarker) {
		t.Errorf("NewImporter(\"\") error = %v, want ErrEmptyNullMarker", err)
	}
}

func TestRecordFormat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rec  Record
		want string
	}{
		{Record{ID: "1", Tags: nil}, "1: nil (NULL)"},
		{Record{ID: "2", Tags: []string{}}, "2: [] (empty)"},
		{Record{ID: "3", Tags: []string{"a", "b"}}, "3: [a b]"},
	} {
		if got := tc.rec.Format(); got != tc.want {
			t.Errorf("Format() = %q, want %q", got, tc.want)
		}
	}
}

// TestTagsNaiveLosesNullVsEmpty is the antipattern contrast: tagsNaive maps
// the null marker and an empty cell to the same []string{}, losing "never
// tagged" versus "tags explicitly cleared". ParseRow keeps them apart.
func TestTagsNaiveLosesNullVsEmpty(t *testing.T) {
	t.Parallel()
	if len(tagsNaive(`\N`)) != 0 || len(tagsNaive("")) != 0 {
		t.Fatal("tagsNaive did not collapse both spellings to length 0")
	}

	imp, err := NewImporter(`\N`)
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	nullRec, _ := imp.ParseRow([]string{"1", `\N`})
	emptyRec, _ := imp.ParseRow([]string{"2", ""})
	if (nullRec.Tags == nil) == (emptyRec.Tags == nil) {
		t.Fatal("ParseRow lost the null-vs-empty distinction tagsNaive already lost")
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
			name:  "clean stream via stdin",
			args:  []string{"-"},
			stdin: "id,tags\n1,\\N\n2,\n3,\"vip,eu,beta\"\n",
			want:  "1: nil (NULL)\n2: [] (empty)\n3: [vip eu beta]\n",
		},
		{name: "missing source argument", args: []string{}, wantErr: true},
		{name: "wrong header", args: []string{"-"}, stdin: "name,tags\n1,vip\n", wantErr: true},
		{name: "malformed row wrong column count", args: []string{"-"}, stdin: "id,tags\n1,vip,extra\n", wantErr: true},
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

// TestRunFileSource proves the non-stdin path, and that a missing file is a
// usage error.
func TestRunFileSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "export.csv")
	if err := os.WriteFile(path, []byte("id,tags\n1,\\N\n2,vip\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout bytes.Buffer
	if err := run([]string{path}, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "1: nil (NULL)\n2: [vip]\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	missing := filepath.Join(dir, "does-not-exist.csv")
	if err := run([]string{missing}, strings.NewReader(""), &stdout); !errors.Is(err, errUsage) {
		t.Fatalf("run(%q) error = %v, want it to wrap errUsage", missing, err)
	}
}
```

## Review

`ParseRow` is correct when the three shapes a tags cell can take -- the null
marker, an empty cell, a populated cell -- land on three distinct Go values:
nil, non-nil-empty, and non-nil-populated. Treating the marker and an empty
cell as the same "no tags" case is the mistake this module isolates: it
compiles, imports every row without error, and only shows its cost later,
when nothing downstream can tell a row that was never tagged from one whose
tags were explicitly cleared. `NewImporter` rejects an empty null marker with
`ErrEmptyNullMarker`; `ParseRow` rejects a wrong column count with
`ErrMalformedRow`, both checkable with `errors.Is`. `run` streams the CSV row
by row via `encoding/csv.Reader`, stops at the first row it cannot import
rather than silently skipping it, and maps every input problem -- a bad flag,
a missing source, an unreadable file, a wrong header, a malformed row -- to
exit code 2, reserving 1 for a runtime failure this tool never produces. Run
`go test -count=1 -race ./...` to confirm the parse table, the validation
paths, the naive-collapse contrast, and `run`'s behavior over both stdin and
a real file.

## Resources

- [PostgreSQL: COPY](https://www.postgresql.org/docs/current/sql-copy.html) — the `NULL` option and its default `\N` marker for CSV-adjacent text output.
- [encoding/csv](https://pkg.go.dev/encoding/csv) — `Reader.FieldsPerRecord` and the quoting rules used to embed a comma inside one field.
- [RFC 4180](https://www.rfc-editor.org/rfc/rfc4180) — the closest thing CSV has to a specification, including the quoting this tool relies on.
- [Go Wiki: CodeReviewComments](https://go.dev/wiki/CodeReviewComments#declaring-empty-slices) — the same nil-vs-empty guidance this exercise applies to a text wire format instead of JSON.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-json-field-state-classifier.md](17-json-field-state-classifier.md) | Next: [19-multiheader-deep-clone.md](19-multiheader-deep-clone.md)
