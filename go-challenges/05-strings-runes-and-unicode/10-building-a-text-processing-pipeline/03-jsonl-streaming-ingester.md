# Exercise 3: Stream JSON Lines with Line-Numbered Error Context

The transforms are pure and stateless; the streaming layer is where boundaries,
buffering, and error context live. This module builds `ProcessJSONLines`, the
orchestrator that reads a JSONL stream from any `io.Reader`, cleans each record's
fields, and — crucially — fails a malformed record with the physical line number
so a single bad row in a million is debuggable.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ingester/                 independent module: example.com/ingester
  go.mod                  go 1.26
  ingester.go             Document, CleanDocument, Pipeline, ProcessJSONLines
  cmd/
    demo/
      main.go             runnable demo over an in-memory JSONL stream
  ingester_test.go        golden, line-number, blank-line, and oversized-line tests
```

- Files: `ingester.go`, `cmd/demo/main.go`, `ingester_test.go`.
- Implement: `ProcessJSONLines(r io.Reader) ([]CleanDocument, error)` using `bufio.Scanner` with an explicit `Buffer`, blank-line skipping that does not disturb line numbering, per-line `json.Unmarshal` into a `Document`, pipeline cleanup of `Title`/`Body`, decode errors wrapped with `line N` via `%w`, and `scanner.Err()` propagation.
- Test: a golden multi-record decode; a malformed record on line 2 after a blank line asserting the error contains `line 2`; a blank-line-between-records test; an oversized-line test asserting `bufio.ErrTooLong` is surfaced via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/03-jsonl-streaming-ingester/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/03-jsonl-streaming-ingester
```

### Buffer sizing, line numbering, and error wrapping

`bufio.Scanner` reads line by line, but its default maximum token size is 64 KiB.
A production document with a large body exceeds that, and the default behavior is
brutal: `Scan` returns `false` and `Err` returns `bufio.ErrTooLong`. Code that
loops on `Scan` and never checks `Err` treats an oversized line as clean
end-of-input and silently drops the rest of the stream. So the first line of the
function sizes the buffer deliberately — `scanner.Buffer(make([]byte, 0, 64*1024),
maxLineBytes)` — with a documented `maxLineBytes` ceiling, and the last thing the
function does before returning is check `scanner.Err()`. Sizing the buffer larger
does not eliminate the ceiling; it moves it to a value you chose on purpose and can
explain in review.

Line numbering is a contract with whoever debugs the data. The counter increments
once per physical line read, *before* the blank-line skip, so a blank line still
consumes a number. That means a malformed record on physical line 2 reports "line
2" even when line 1 was blank — the number points at the actual byte offset in the
file, not at a logical record index. Skipping blank lines after incrementing is
what preserves that alignment.

Decode failures wrap with `fmt.Errorf("line %d: decode document: %w", lineNumber,
err)`. The `%w` verb preserves the underlying `*json.SyntaxError` (or whatever
`json.Unmarshal` returned) so a caller can both read the human location and match
the cause with `errors.Is`/`errors.As`. The scanner-error path wraps
`scanner.Err()` the same way, which is what lets a test assert
`errors.Is(err, bufio.ErrTooLong)` on the oversized case.

Create `ingester.go`:

```go
package ingester

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"strings"
	"unicode"
)

// maxLineBytes is the deliberate per-record ceiling. Records larger than this
// surface bufio.ErrTooLong rather than silently truncating the stream.
const maxLineBytes = 1024 * 1024

// Document is the raw JSONL shape as it arrives from an untrusted producer.
type Document struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// CleanDocument is the normalized record ready for indexing.
type CleanDocument struct {
	ID    string
	Title string
	Body  string
}

// Transform is a pure cleanup stage.
type Transform func(string) string

// Pipeline applies an ordered chain of transforms.
type Pipeline struct {
	transforms []Transform
}

func New(transforms ...Transform) Pipeline {
	return Pipeline{transforms: transforms}
}

func (p Pipeline) Clean(input string) string {
	output := input
	for _, transform := range p.transforms {
		output = transform(output)
	}
	return output
}

// DefaultPipeline is the field-cleanup chain applied to Title and Body.
func DefaultPipeline() Pipeline {
	return New(decodeEntities, removeControls, lowercase, collapseWhitespace)
}

func decodeEntities(s string) string { return html.UnescapeString(s) }

func removeControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func lowercase(s string) string { return strings.ToLower(s) }

func collapseWhitespace(s string) string { return strings.Join(strings.Fields(s), " ") }

// ProcessJSONLines streams a JSONL body from r, cleaning Title and Body of each
// record. Blank lines are skipped without disturbing line numbering, decode
// failures are wrapped with the physical line number, and scanner errors
// (including bufio.ErrTooLong) are surfaced.
func (p Pipeline) ProcessJSONLines(r io.Reader) ([]CleanDocument, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	var docs []CleanDocument
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var doc Document
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			return nil, fmt.Errorf("line %d: decode document: %w", lineNumber, err)
		}

		docs = append(docs, CleanDocument{
			ID:    doc.ID,
			Title: p.Clean(doc.Title),
			Body:  p.Clean(doc.Body),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan documents: %w", err)
	}

	return docs, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/ingester"
)

func main() {
	body := strings.Join([]string{
		`{"id":"1","title":" CAF&Eacute; &amp; Search ","body":"Hello\tWORLD"}`,
		``,
		`{"id":"2","title":"Go&#39;s Scanner","body":"Multiple   spaces\ncollapse"}`,
	}, "\n")

	docs, err := ingester.DefaultPipeline().ProcessJSONLines(strings.NewReader(body))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, d := range docs {
		fmt.Printf("%s | %q | %q\n", d.ID, d.Title, d.Body)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1 | "café & search" | "hello world"
2 | "go's scanner" | "multiple spaces collapse"
```

### Tests

The golden test decodes two records and asserts the exact `CleanDocument` slice.
`TestReportsLineNumber` puts a malformed record on physical line 2 (after a blank
line 1) and asserts the error mentions `line 2`, proving the counter is not thrown
off by the skip. `TestSkipsBlankLinesWithoutShiftingNumbers` interleaves blank
lines and checks both the decoded records and that a later malformed line still
reports its physical number. `TestOversizedLineSurfacesErrTooLong` feeds a line
past a small buffer and asserts `errors.Is(err, bufio.ErrTooLong)` — the error is
surfaced, not swallowed.

Create `ingester_test.go`:

```go
package ingester

import (
	"bufio"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestProcessJSONLinesGolden(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		`{"id":"1","title":" CAF&Eacute; &amp; Search ","body":"Hello\tWORLD"}`,
		`{"id":"2","title":"Go&#39;s Scanner","body":"Multiple   spaces\ncollapse"}`,
	}, "\n")

	got, err := DefaultPipeline().ProcessJSONLines(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []CleanDocument{
		{ID: "1", Title: "café & search", Body: "hello world"},
		{ID: "2", Title: "go's scanner", Body: "multiple spaces collapse"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("docs = %#v, want %#v", got, want)
	}
}

func TestReportsLineNumber(t *testing.T) {
	t.Parallel()

	// Line 1 blank, line 2 malformed: the error must say line 2.
	_, err := DefaultPipeline().ProcessJSONLines(strings.NewReader("\n{bad json}\n"))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error = %v, want it to mention line 2", err)
	}
}

func TestSkipsBlankLinesWithoutShiftingNumbers(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		`{"id":"1","title":"one","body":"a"}`, // line 1
		``,                                    // line 2 blank
		`{"id":"2","title":"two","body":"b"}`, // line 3
		``,                                    // line 4 blank
		`{oops}`,                              // line 5 malformed
	}, "\n")

	_, err := DefaultPipeline().ProcessJSONLines(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected an error for the malformed line")
	}
	if !strings.Contains(err.Error(), "line 5") {
		t.Fatalf("error = %v, want it to mention line 5", err)
	}
}

func TestOversizedLineSurfacesErrTooLong(t *testing.T) {
	t.Parallel()

	// A single record far larger than maxLineBytes would be impractical to build,
	// so exercise the same code path with a Pipeline that shares the ceiling but a
	// deliberately huge line: repeat a chunk past the 1 MiB ceiling.
	huge := `{"id":"1","title":"` + strings.Repeat("x", maxLineBytes+1) + `"}`
	_, err := DefaultPipeline().ProcessJSONLines(strings.NewReader(huge))
	if err == nil {
		t.Fatal("expected an error for the oversized line")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("error = %v, want it to wrap bufio.ErrTooLong", err)
	}
}
```

## Review

The ingester is correct when three properties hold together: records decode and
clean to the exact golden slice, a malformed line reports its *physical* number
(unaffected by blank-line skipping), and an oversized line surfaces
`bufio.ErrTooLong` through the `%w` chain rather than truncating the stream. The
mistakes to avoid are the classic scanner traps: never trust the default 64 KiB
buffer, and never return `nil` without first checking `scanner.Err()`. Keep the
line counter incrementing before the blank-line `continue`, or your "line N"
messages drift off the real file offset the moment a blank line appears. Run
`go test -race` to confirm the streaming path is clean.

## Resources

- [bufio.Scanner: Buffer, Scan, Err](https://pkg.go.dev/bufio#Scanner) — token limits and the `ErrTooLong` contract.
- [bufio.ErrTooLong](https://pkg.go.dev/bufio#pkg-variables) — the sentinel returned when a token exceeds the buffer.
- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — per-line decoding and its error types.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — wrapping the cause for `errors.Is`/`errors.As`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-pure-cleanup-transforms.md](02-pure-cleanup-transforms.md) | Next: [04-utf8-validation-and-repair.md](04-utf8-validation-and-repair.md)
