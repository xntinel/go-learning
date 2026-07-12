# Exercise 10: Wire the Full Pipeline and Assert End-to-End Invariants

The stages from the previous exercises assemble into one production ordering, and
the value of assembling them is the cross-cutting invariants a reviewer would
demand: output is always valid UTF-8, always NFC-stable, deterministic, and
idempotent. This module wires the full chain through `ProcessJSONLines` and proves
those invariants — including a negative test showing that a mis-ordered chain
leaks an artifact, which is what justifies fixing the order.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise. It uses `golang.org/x/text`; the gate fetches it.

## What you'll build

```text
ingestpipe/               independent module: example.com/ingestpipe
  go.mod                  go 1.26 (requires golang.org/x/text)
  ingestpipe.go           six ordered transforms; DefaultPipeline; ProcessJSONLines
  cmd/
    demo/
      main.go             runnable demo of correct vs mis-ordered output
  ingestpipe_test.go      invariant, idempotence, and ordering-artifact tests
```

- Files: `ingestpipe.go`, `cmd/demo/main.go`, `ingestpipe_test.go`.
- Implement: the ordered chain `RepairUTF8` → `DecodeEntities` → `RemoveControls` → `NormalizeNFC` → `Lowercase` → `CollapseWhitespace`, a `DefaultPipeline`, and `ProcessJSONLines(io.Reader) ([]CleanDocument, error)`.
- Test: over a JSONL fixture mixing invalid UTF-8, entities, decomposed accents, controls, and mixed case, assert every output field is valid UTF-8 and NFC-stable (`norm.NFC.String(out)==out`); assert full idempotence by re-running the pipeline over its own output; a negative test proving collapse-before-control-removal leaks a double space.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/10-composed-ingestion-invariants/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/10-composed-ingestion-invariants
go get golang.org/x/text/unicode/norm
```

### The ordering, and why each stage sits where it does

The production chain is six transforms in a fixed order, and every position has a
reason:

1. `RepairUTF8` first — establish the UTF-8 invariant before anything inspects
   runes. In the JSONL path `encoding/json` already substitutes U+FFFD for invalid
   bytes during decode, so here repair is belt-and-suspenders; it becomes genuinely
   load-bearing the moment a field arrives from a non-JSON source (a raw log line, a
   header value), which is exactly why it lives in the shared chain.
2. `DecodeEntities` — turn `&amp;`/`&Eacute;` into real characters so later stages
   operate on text, not entity syntax.
3. `RemoveControls` — drop invisible C0/C1 bytes (keeping `\n`/`\t`) before
   whitespace handling, so a control byte cannot occupy a "word" slot during
   collapse.
4. `NormalizeNFC` — canonicalize composed/decomposed spellings before any
   case/whitespace work that a downstream index will hash or compare.
5. `Lowercase` — case-normalize for a case-insensitive field.
6. `CollapseWhitespace` last — with controls already gone and entities decoded,
   collapse runs of whitespace to single spaces and trim.

The invariants that fall out are what a reviewer signs off on. After the chain,
every field is valid UTF-8 (stage 1, preserved by every later stage), NFC-stable so
`norm.NFC.String(out) == out` (stage 4, and nothing after it introduces new
decomposable sequences), deterministic (every transform is a pure function), and
idempotent: running the whole pipeline over its own output is a fixed point. That
last property is the one to internalize — a cleaned record re-ingested must clean to
itself, or re-processing drifts.

The negative test is the justification for the order. If `CollapseWhitespace` runs
*before* `RemoveControls`, a non-whitespace control byte like BEL (`\x07`) sitting
between two words survives the collapse as its own token, and removing it afterward
leaves the two spaces that were around it uncollapsed — a `"hello  world"`
double-space artifact instead of `"hello world"`. The invisible byte leaked a
visible defect into the index. Fixing the order removes it.

Create `ingestpipe.go`:

```go
package ingestpipe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const maxLineBytes = 1024 * 1024

// Document is the raw JSONL shape from an untrusted producer.
type Document struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// CleanDocument is the normalized, index-safe record.
type CleanDocument struct {
	ID    string
	Title string
	Body  string
}

// Transform is a pure cleanup stage.
type Transform func(string) string

// The six ordered transforms of the production chain.

func RepairUTF8(s string) string { return strings.ToValidUTF8(s, string(utf8.RuneError)) }

func DecodeEntities(s string) string { return html.UnescapeString(s) }

func RemoveControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func NormalizeNFC(s string) string { return norm.NFC.String(s) }

func Lowercase(s string) string { return strings.ToLower(s) }

func CollapseWhitespace(s string) string { return strings.Join(strings.Fields(s), " ") }

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

// DefaultPipeline is the fixed production ordering.
func DefaultPipeline() Pipeline {
	return New(RepairUTF8, DecodeEntities, RemoveControls, NormalizeNFC, Lowercase, CollapseWhitespace)
}

// ProcessJSONLines streams JSONL from r and cleans each record's Title and Body
// through the pipeline, wrapping decode errors with the physical line number and
// surfacing scanner errors.
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

	"example.com/ingestpipe"
)

func main() {
	correct := ingestpipe.DefaultPipeline()
	misordered := ingestpipe.New(
		ingestpipe.RepairUTF8,
		ingestpipe.DecodeEntities,
		ingestpipe.NormalizeNFC,
		ingestpipe.Lowercase,
		ingestpipe.CollapseWhitespace, // collapse BEFORE removing controls: the bug
		ingestpipe.RemoveControls,
	)

	in := "HELLO \x07 World"
	fmt.Printf("correct:    %q\n", correct.Clean(in))
	fmt.Printf("misordered: %q\n", misordered.Clean(in))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct:    "hello world"
misordered: "hello  world"
```

### Tests

`TestInvariants` runs a JSONL fixture (built with `json.Marshal` so the bytes are
well-formed) whose fields carry an invalid byte, an HTML entity, a decomposed
accent, a control byte, and mixed case, then asserts every output field is valid
UTF-8 and NFC-stable. `TestIdempotent` feeds each cleaned field back through the
pipeline and asserts a fixed point. `TestOrderingArtifact` proves the mis-ordered
chain leaks a double space where the correct chain does not.

Create `ingestpipe_test.go`:

```go
package ingestpipe

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// fixture builds a JSONL body whose fields exercise every stage.
func fixture(t *testing.T) string {
	t.Helper()
	docs := []Document{
		{
			ID: "1",
			// Title mixes an HTML entity and a NUL control byte.
			Title: "CAF&Eacute; &amp; \x00 Search",
			// Body has decomposed accents (e + U+0301), whitespace runs, and mixed case.
			Body: "Re\u0301sume\u0301   TEXT",
		},
		{
			ID: "2",
			// Title carries an invalid UTF-8 byte from an untrusted producer.
			Title: "Hello " + string([]byte{0x80}) + " WORLD",
			Body:  "Go&#39;s\tPipeline",
		},
	}
	var lines []string
	for _, d := range docs {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	return strings.Join(lines, "\n")
}

func TestInvariants(t *testing.T) {
	t.Parallel()

	docs, err := DefaultPipeline().ProcessJSONLines(strings.NewReader(fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2", len(docs))
	}
	for _, d := range docs {
		for _, f := range []string{d.Title, d.Body} {
			if !utf8.ValidString(f) {
				t.Fatalf("field %q is not valid UTF-8", f)
			}
			if norm.NFC.String(f) != f {
				t.Fatalf("field %q is not NFC-stable", f)
			}
		}
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()

	p := DefaultPipeline()
	docs, err := p.ProcessJSONLines(strings.NewReader(fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		for _, f := range []string{d.Title, d.Body} {
			if again := p.Clean(f); again != f {
				t.Fatalf("pipeline not idempotent: Clean(%q) = %q", f, again)
			}
		}
	}
}

func TestOrderingArtifact(t *testing.T) {
	t.Parallel()

	correct := DefaultPipeline()
	misordered := New(
		RepairUTF8, DecodeEntities, NormalizeNFC, Lowercase, CollapseWhitespace, RemoveControls,
	)

	in := "hello \x07 world"
	if got := correct.Clean(in); got != "hello world" {
		t.Fatalf("correct order = %q, want %q", got, "hello world")
	}
	got := misordered.Clean(in)
	if got != "hello  world" {
		t.Fatalf("misordered = %q, want the double-space artifact %q", got, "hello  world")
	}
	if got == correct.Clean(in) {
		t.Fatal("misordered output should differ from correct output")
	}
}

func ExampleDefaultPipeline() {
	docs, _ := DefaultPipeline().ProcessJSONLines(
		strings.NewReader(`{"id":"1","title":"CAF&Eacute; &amp; SEARCH","body":"go   TEXT"}`),
	)
	fmt.Println(docs[0].Title)
	// Output: café & search
}
```

## Review

The composed pipeline is correct when the four invariants hold together over a
realistic fixture: every output field is valid UTF-8 and NFC-stable, the transforms
are deterministic, and the whole chain is idempotent so a re-ingested clean record
maps to itself. The ordering test is the one that earns the fixed order — it shows
concretely that collapsing whitespace before removing controls leaks a double-space
artifact, so the sequence is a contract, not a preference. When you extend the chain
(adding redaction, say), re-run the idempotence and NFC-stability assertions: a new
stage that breaks either is a stage in the wrong place. Run `go test -race` to
confirm the streaming path stays clean.

## Resources

- [unicode/utf8.ValidString](https://pkg.go.dev/unicode/utf8#ValidString) — the UTF-8 output invariant.
- [golang.org/x/text/unicode/norm](https://pkg.go.dev/golang.org/x/text/unicode/norm) — NFC and NFC-stability checks.
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — the streaming boundary the pipeline runs over.
- [The Go Blog: text normalization in Go](https://go.dev/blog/normalization) — normalization as an ingestion invariant.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-pii-redaction-transform.md](09-pii-redaction-transform.md) | Next: [../../06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/00-concepts.md](../../06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/00-concepts.md)
