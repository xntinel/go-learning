# Exercise 1: Document Chunking with Overlap and Source Metadata

The first stage of every RAG pipeline is deterministic backend code: split a
document into bounded, overlapping chunks that still map back to the exact source
span they came from. This exercise builds that chunker with hierarchical
boundaries and reconstructable byte offsets, and proves the offset invariant with
a real test.

This module is fully self-contained. It begins with its own `go mod init`, uses
only the standard library, and ships its own demo and tests. Nothing here imports
any other exercise.

## What you'll build

```text
chunker/                     independent module: example.com/chunker
  go.mod                     go 1.26
  chunker.go                 Config, Chunk, sentinel errors; Split / SplitFile
  cmd/
    demo/
      main.go                runnable demo: chunk a two-paragraph document
  chunker_test.go            offset invariant, overlap, errors, fstest, Unicode
```

- Files: `chunker.go`, `cmd/demo/main.go`, `chunker_test.go`.
- Implement: `Split(docID, text, cfg)` returning `[]Chunk` with `DocID`, `Index`, byte `Start`/`End`, `Text`, and an approximate `Tokens` count, plus `SplitFile` reading from an `fs.FS`.
- Test: every chunk fits `MaxRunes`; adjacent chunks share exactly `Overlap` runes; chunk byte ranges reconstruct the original document; bad config returns `ErrInvalidChunkSize` / `ErrOverlapTooLarge`; empty input returns `ErrEmptyDocument`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/52-ai-llm-backends/05-rag-pipeline/01-chunker/cmd/demo
cd go-solutions/52-ai-llm-backends/05-rag-pipeline/01-chunker
go mod edit -go=1.26
```

### The unit problem, and why we chunk on runes

The chunker's size limits are measured in **runes**, not bytes and not model
tokens. Bytes are wrong because a multi-byte character must never be split in the
middle. Model tokens are unknowable offline — the real tokenizer is model-specific
and lives behind a network call — so we use runes as a deterministic proxy and
carry a coarse `Tokens` estimate (roughly four characters per token) for the
downstream budget. This keeps the whole stage pure and testable while staying
honest that the token count is an approximation.

The core data structure is the `Chunk`. Every chunk records the document it came
from, its ordinal `Index`, its byte range `[Start, End)`, the exact `Text` of that
range, and the token estimate. The byte range is the load-bearing field: it is
what lets a downstream citation say "this claim came from doc-42, bytes 1400-1750"
and what lets a test reconstruct the original document from the chunks alone.

### Rune-safe offsets

Byte offsets that land on rune boundaries are the whole point, so the chunker
converts the text to `[]rune` once and builds a parallel `byteAt` table mapping
each rune index to its byte offset (`byteAt[k]` is where rune `k` starts;
`byteAt[len(runes)]` is `len(text)`). Every chunk boundary is a rune index, and
converting it through `byteAt` yields a byte offset that is always on a valid
UTF-8 boundary. `text[byteAt[i]:byteAt[j]]` is then exactly the runes `[i, j)` —
never a half-character.

### Hierarchical boundaries with a guaranteed stride

The chunk loop walks the rune slice. For the window starting at `i`, the hard
limit is `hardEnd = i + MaxRunes`. If that reaches the end of the document, the
final chunk is `[i, n)`. Otherwise the chunker looks for the best *semantic*
boundary at or before `hardEnd`: it prefers the largest paragraph break (a blank
line), then the largest sentence end (`.`/`!`/`?` followed by space), then the
largest word boundary (a space), and only falls back to a hard cut at `hardEnd` if
nothing better exists. The chosen boundary `j` becomes the chunk end, and the next
window starts at `j - Overlap`, so adjacent chunks share exactly `Overlap` runes.

Two details make this terminate and stay correct. The boundary search is confined
to `(i + Overlap, hardEnd]`, which guarantees `j > i + Overlap` and therefore
`j - Overlap > i` — the loop always makes progress. And because `Overlap` is
validated to be strictly less than `MaxRunes`, `hardEnd` is always a valid
fallback that leaves a healthy stride. The result is contiguous coverage: chunk 0
starts at byte 0, the last chunk ends at `len(text)`, and each next chunk starts
at `prev.End - Overlap`, so you can rebuild the document by walking the ranges and
skipping the already-written overlap region.

Create `chunker.go`:

```go
package chunker

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sentinel errors, wrapped with %w so callers assert them via errors.Is.
var (
	ErrEmptyDocument    = errors.New("empty document")
	ErrInvalidChunkSize = errors.New("invalid chunk size")
	ErrOverlapTooLarge  = errors.New("overlap too large")
)

// Config controls chunking. Sizes are measured in runes, a deterministic proxy
// for model tokens.
type Config struct {
	MaxRunes int // maximum runes per chunk (> 0)
	Overlap  int // runes shared between adjacent chunks (>= 0, < MaxRunes)
}

// Chunk is a bounded slice of a document with reconstructable source metadata.
type Chunk struct {
	DocID  string
	Index  int    // ordinal position, starting at 0
	Start  int    // byte offset into the source, inclusive
	End    int    // byte offset into the source, exclusive
	Text   string // == source[Start:End]
	Tokens int    // approximate model tokens (~4 chars/token)
}

func (c Config) validate() error {
	if c.MaxRunes <= 0 {
		return fmt.Errorf("chunker: max runes %d: %w", c.MaxRunes, ErrInvalidChunkSize)
	}
	if c.Overlap < 0 {
		return fmt.Errorf("chunker: overlap %d: %w", c.Overlap, ErrInvalidChunkSize)
	}
	if c.Overlap >= c.MaxRunes {
		return fmt.Errorf("chunker: overlap %d >= max %d: %w", c.Overlap, c.MaxRunes, ErrOverlapTooLarge)
	}
	return nil
}

func approxTokens(runes int) int {
	return (runes + 3) / 4
}

// Split breaks text into bounded, overlapping chunks with reconstructable byte
// offsets. It returns ErrEmptyDocument for blank input and ErrInvalidChunkSize /
// ErrOverlapTooLarge for bad config.
func Split(docID, text string, cfg Config) ([]Chunk, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("chunker: %q: %w", docID, ErrEmptyDocument)
	}

	runes := []rune(text)
	n := len(runes)

	// byteAt[k] is the byte offset where rune k starts; byteAt[n] == len(text).
	byteAt := make([]int, n+1)
	off := 0
	for k, r := range runes {
		byteAt[k] = off
		off += utf8.RuneLen(r)
	}
	byteAt[n] = len(text)

	var chunks []Chunk
	for i := 0; i < n; {
		hardEnd := i + cfg.MaxRunes
		if hardEnd >= n {
			chunks = append(chunks, makeChunk(docID, len(chunks), text, byteAt, i, n))
			break
		}
		j := boundary(runes, i+cfg.Overlap, hardEnd)
		chunks = append(chunks, makeChunk(docID, len(chunks), text, byteAt, i, j))
		i = j - cfg.Overlap
	}
	return chunks, nil
}

func makeChunk(docID string, index int, text string, byteAt []int, i, j int) Chunk {
	start, end := byteAt[i], byteAt[j]
	return Chunk{
		DocID:  docID,
		Index:  index,
		Start:  start,
		End:    end,
		Text:   text[start:end],
		Tokens: approxTokens(j - i),
	}
}

// boundary returns the largest preferred break in (lo, hi]: paragraph, then
// sentence, then word, else the hard cut hi.
func boundary(runes []rune, lo, hi int) int {
	for b := hi; b > lo; b-- {
		if b >= 2 && runes[b-1] == '\n' && runes[b-2] == '\n' {
			return b
		}
	}
	for b := hi; b > lo; b-- {
		if isSentenceEnd(runes, b) {
			return b
		}
	}
	for b := hi; b > lo; b-- {
		if b < len(runes) && unicode.IsSpace(runes[b]) {
			return b
		}
	}
	return hi
}

func isSentenceEnd(runes []rune, b int) bool {
	if b == 0 {
		return false
	}
	switch runes[b-1] {
	case '.', '!', '?':
		return b == len(runes) || unicode.IsSpace(runes[b])
	}
	return false
}

// SplitFile reads name from fsys and chunks it, using name as the DocID.
func SplitFile(fsys fs.FS, name string, cfg Config) ([]Chunk, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("chunker: read %q: %w", name, err)
	}
	return Split(name, string(data), cfg)
}
```

### The runnable demo

The demo chunks a two-paragraph document and prints each chunk's index, rune
length, byte range, token estimate, and a short preview, so you can watch the
overlap and the boundary choices directly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/chunker"
)

func main() {
	doc := "Retrieval-augmented generation grounds a model in your own data. " +
		"The retrieval subsystem is deterministic backend code you can test.\n\n" +
		"Chunking is the first stage. Good chunks respect sentence boundaries " +
		"and carry byte offsets so every answer can cite its exact source span."

	chunks, err := chunker.Split("doc-1", doc, chunker.Config{MaxRunes: 100, Overlap: 20})
	if err != nil {
		panic(err)
	}

	fmt.Printf("document doc-1: %d chunks\n", len(chunks))
	for _, c := range chunks {
		preview := strings.ReplaceAll(c.Text, "\n", " ")
		if len(preview) > 32 {
			preview = preview[:32] + "..."
		}
		fmt.Printf("[%d] bytes=[%d,%d) tokens=%d | %q\n", c.Index, c.Start, c.End, c.Tokens, preview)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
document doc-1: 5 chunks
[0] bytes=[0,64) tokens=16 | "Retrieval-augmented generation g..."
[1] bytes=[44,134) tokens=23 | "el in your own data. The retriev..."
[2] bytes=[114,162) tokens=12 | "code you can test.  Chunking is ..."
[3] bytes=[142,241) tokens=25 | " is the first stage. Good chunks..."
[4] bytes=[221,273) tokens=13 | "sets so every answer can cite it..."
```

### Tests

The tests encode the invariants that make the chunker trustworthy.
`TestOffsetInvariant` reconstructs the original document from the chunk byte
ranges alone — walking the chunks and skipping each already-written overlap region
— and asserts it equals the input, which only holds if every chunk maps to a real
span and the coverage is contiguous. `TestOverlap` asserts adjacent chunks share
exactly `Overlap` runes. `TestMaxRunes` asserts no chunk exceeds the limit. The
error tests assert the sentinels via `errors.Is`. `TestSplitFile` drives the
`fs.FS` path with `fstest.MapFS`. `TestUnicodeOffsets` is the learner extension:
it chunks a multi-byte document and confirms every offset lands on a rune
boundary so `Text` is always valid UTF-8.

Create `chunker_test.go`:

```go
package chunker

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

const sample = "Retrieval-augmented generation grounds a model in your own data. " +
	"The retrieval subsystem is deterministic backend code you can test.\n\n" +
	"Chunking is the first stage. Good chunks respect sentence boundaries " +
	"and carry byte offsets so every answer can cite its exact source span."

func reconstruct(text string, chunks []Chunk) string {
	var b strings.Builder
	prevEnd := 0
	for _, c := range chunks {
		if c.Start < prevEnd {
			b.WriteString(text[prevEnd:c.End]) // skip the overlap already written
		} else {
			b.WriteString(text[c.Start:c.End])
		}
		if c.End > prevEnd {
			prevEnd = c.End
		}
	}
	return b.String()
}

func TestOffsetInvariant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no overlap", Config{MaxRunes: 80, Overlap: 0}},
		{"small overlap", Config{MaxRunes: 100, Overlap: 20}},
		{"large window", Config{MaxRunes: 500, Overlap: 50}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chunks, err := Split("doc", sample, tc.cfg)
			if err != nil {
				t.Fatalf("Chunk: %v", err)
			}
			for _, c := range chunks {
				if got := sample[c.Start:c.End]; got != c.Text {
					t.Fatalf("chunk %d: text != source[%d:%d]", c.Index, c.Start, c.End)
				}
			}
			if got := reconstruct(sample, chunks); got != sample {
				t.Fatalf("reconstruct mismatch:\n got %q\nwant %q", got, sample)
			}
		})
	}
}

func TestOverlap(t *testing.T) {
	t.Parallel()
	cfg := Config{MaxRunes: 100, Overlap: 20}
	chunks, err := Split("doc", sample, cfg)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	for k := 0; k+1 < len(chunks); k++ {
		prevEndRunes := utf8.RuneCountInString(sample[:chunks[k].End])
		nextStartRunes := utf8.RuneCountInString(sample[:chunks[k+1].Start])
		if shared := prevEndRunes - nextStartRunes; shared != cfg.Overlap {
			t.Fatalf("chunks %d/%d share %d runes, want %d", k, k+1, shared, cfg.Overlap)
		}
	}
}

func TestMaxRunes(t *testing.T) {
	t.Parallel()
	cfg := Config{MaxRunes: 100, Overlap: 20}
	chunks, err := Split("doc", sample, cfg)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	for _, c := range chunks {
		if n := utf8.RuneCountInString(c.Text); n > cfg.MaxRunes {
			t.Fatalf("chunk %d has %d runes, exceeds max %d", c.Index, n, cfg.MaxRunes)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"zero max", Config{MaxRunes: 0, Overlap: 0}, ErrInvalidChunkSize},
		{"negative overlap", Config{MaxRunes: 100, Overlap: -1}, ErrInvalidChunkSize},
		{"overlap too large", Config{MaxRunes: 50, Overlap: 50}, ErrOverlapTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Split("doc", sample, tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEmptyDocument(t *testing.T) {
	t.Parallel()
	_, err := Split("doc", "   \n\t ", Config{MaxRunes: 100, Overlap: 10})
	if !errors.Is(err, ErrEmptyDocument) {
		t.Fatalf("err = %v, want ErrEmptyDocument", err)
	}
}

func TestSplitFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"notes/rag.txt": {Data: []byte(sample)},
	}
	chunks, err := SplitFile(fsys, "notes/rag.txt", Config{MaxRunes: 120, Overlap: 20})
	if err != nil {
		t.Fatalf("SplitFile: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	if chunks[0].DocID != "notes/rag.txt" {
		t.Fatalf("DocID = %q, want notes/rag.txt", chunks[0].DocID)
	}
}

// TestUnicodeOffsets is the learner extension: multi-byte input must never split
// a rune, so every offset stays on a UTF-8 boundary and every chunk is valid.
func TestUnicodeOffsets(t *testing.T) {
	t.Parallel()
	doc := "Los sistemas de recuperacion son deterministas. " +
		"Cada fragmento conserva sus limites y acentos: cafe, nino, corazon.\n\n" +
		"El modelo unicamente responde con el contexto proporcionado."
	chunks, err := Split("es", doc, Config{MaxRunes: 60, Overlap: 15})
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk %d is not valid UTF-8", c.Index)
		}
		if got := doc[c.Start:c.End]; got != c.Text {
			t.Fatalf("chunk %d offset mismatch", c.Index)
		}
	}
}

func ExampleSplit() {
	chunks, _ := Split("doc-1", "A short document that fits in one chunk.", Config{MaxRunes: 100, Overlap: 10})
	fmt.Println(len(chunks), chunks[0].Text)
	// Output: 1 A short document that fits in one chunk.
}
```

## Review

The chunker is correct when three invariants hold together. Every chunk's byte
range yields exactly its text (`source[Start:End] == Text`), so a citation can
point at a real span. Adjacent chunks share exactly `Overlap` runes, so boundary
context is never lost. And the chunk ranges reconstruct the whole document, which
is only possible if coverage is contiguous — the property `TestOffsetInvariant`
checks directly. If reconstruction fails, the boundary loop is either dropping a
region or double-counting one outside the overlap.

The mistakes to avoid are subtle. Do not measure sizes in bytes — a multi-byte
character split across a byte boundary corrupts the chunk; runes plus the `byteAt`
table keep every offset UTF-8-safe, which `TestUnicodeOffsets` verifies. Do not
let the boundary search range down to `i` — confining it to `(i + Overlap,
hardEnd]` is what guarantees forward progress and the exact overlap count.
Remember the `Tokens` field is an approximation (roughly four characters per
token); the downstream budget must leave headroom rather than trust it. Run
`go test -race` to confirm the invariants hold across the table of configs.

## Resources

- [`unicode/utf8`](https://pkg.go.dev/unicode/utf8) — `RuneLen`, `RuneCountInString`, and `ValidString` for rune-safe offsets.
- [`testing/fstest`](https://pkg.go.dev/testing/fstest) — `MapFS` for driving the `fs.FS` path without touching disk.
- [Anthropic: Introducing Contextual Retrieval](https://www.anthropic.com/news/contextual-retrieval) — why chunking and boundary context dominate retrieval quality.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retriever.md](02-retriever.md)
