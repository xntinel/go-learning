# Exercise 3: Grounded Prompt Assembly, Context Budgeting, and Citation Guardrails

This is the stage that separates a demo from something you can put in front of
users. It packs the highest-value chunks under a hard token budget, renders a
grounded prompt with numbered source markers, and — after the model answers —
validates every citation against the retrieved set to catch fabricated references.
Assembly, budgeting, and validation are pure and offline-tested; the single
`Messages.New` call is isolated behind `//go:build online`.

This module is fully self-contained. The default build and test path uses only the
standard library; the Anthropic completion call lives in a `//go:build online`
file compiled only with `-tags online`.

## What you'll build

```text
grounding/                   independent module: example.com/grounding
  go.mod                     go 1.26
  grounding.go               Pack, Assemble, ExtractCitations, ValidateCitations
  complete.go                //go:build online — the single Messages.New call
  online_test.go             //go:build online — a real grounded smoke test
  cmd/
    demo/
      main.go                runnable demo: assemble, then validate two answers
  grounding_test.go          budget, markers, fallback, citation guardrail, golden
```

- Files: `grounding.go`, `complete.go`, `online_test.go`, `cmd/demo/main.go`, `grounding_test.go`.
- Implement: `Pack` (greedy, highest-score-first, under a token budget), `Assemble` (numbered markers plus an "insufficient context" fallback), `ExtractCitations` (parse `[n]` markers, dedupe), and `ValidateCitations` (flag any index outside the retrieved set with `ErrUnsupportedCitation`). The `Messages.New` call behind `//go:build online`.
- Test: assembled context never exceeds the budget; packing is highest-score-first; markers are contiguous and map to included chunks; the fallback path triggers when nothing clears the threshold; citations parse and dedupe; an answer citing `[9]` against 3 sources is flagged; a golden-string test for the rendered prompt; a chunk larger than the whole budget is rejected, not truncated.
- Verify: `go test -count=1 -race ./...` (offline); `go build -tags online ./...` to compile the completion call.

Set up the module:

```bash
mkdir -p go-solutions/52-ai-llm-backends/05-rag-pipeline/03-grounding/cmd/demo
cd go-solutions/52-ai-llm-backends/05-rag-pipeline/03-grounding
go mod edit -go=1.26
```

### Context budgeting: pack, do not concatenate

The model's context window is a hard constraint and every token costs money and
latency, so the assembler never dumps all retrieved chunks into the prompt.
`Pack` filters out anything below the relevance threshold, sorts the survivors by
score descending (ties broken by ID for determinism), and then greedily adds each
chunk whose token estimate still fits the remaining budget, skipping the ones that
do not. Two consequences fall out of this design. A chunk larger than the entire
budget is *rejected*, never truncated mid-sentence — a truncated chunk is a
mangled source that produces mangled citations. And because packing is
highest-score-first, the chunks that do make it in are the most relevant ones that
fit; the budget drops the tail, not the head.

Token counts here are the same coarse proxy as in the chunker (roughly four
characters per token). The real tokenizer differs per model and lives behind a
network call, so `Pack` budgets conservatively — leave headroom in the budget you
pass for the system prompt, the question, and the answer's own `max_tokens`.

### Numbered markers make answers auditable

`Assemble` turns the packed chunks into a grounded request. It renders each chunk
with a contiguous numbered marker `[1]..[n]` and records the mapping from marker
index to source ID in the `Sources` slice, so a later citation `[2]` resolves to a
concrete chunk. The system prompt instructs the model to answer *only* from the
numbered context, to cite with those markers, and to say it does not know
otherwise. When *nothing* clears the threshold, `Assemble` takes the fallback
path: it sets `Insufficient` and emits an "insufficient context" instruction, so
the pipeline never forces the model to hallucinate an answer — and in that case no
model call is needed at all.

### The citation guardrail

Retrieval succeeding does not mean the model used the context faithfully. It can
cite a source that was never retrieved. `ExtractCitations` parses the answer's
`[n]` markers with a regexp, converts them with `strconv.Atoi`, and dedupes into a
sorted slice. `ValidateCitations` then flags any index below 1 or above the number
of retrieved sources, returning a wrapped `ErrUnsupportedCitation`. An answer that
cites `[9]` against three sources is a fabricated reference — caught before it ever
reaches a user. This guardrail is pure string and integer work, which is exactly
why it can be exhaustively unit-tested offline.

### The single network call, isolated

Everything above is deterministic. The one non-deterministic step — the
completion — lives in `complete.go` behind `//go:build online`. It converts the
pure `GroundedPrompt` into `anthropic.MessageNewParams` (System block plus a user
message), makes the `Messages.New` call, concatenates the text blocks of the
response, and runs the same pure `ExtractCitations`/`ValidateCitations` on the
result. The default gate never compiles this file, so the pipeline's logic is
tested for free; the live call is exercised only with `-tags online` and a key.

Create `grounding.go`:

```go
package grounding

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrUnsupportedCitation is returned when an answer cites a source index that
// was not in the retrieved set. Wrapped with %w; assert via errors.Is.
var ErrUnsupportedCitation = errors.New("unsupported citation")

const groundedSystem = "You are a retrieval-grounded assistant. Answer the " +
	"question using ONLY the numbered context passages. Cite every claim with " +
	"its source marker in square brackets, e.g. [1]. If the context does not " +
	"contain the answer, reply that you do not know. Do not use outside knowledge."

const insufficientSystem = "You are a retrieval-grounded assistant. No context " +
	"passages cleared the relevance threshold for this question. Reply that you " +
	"do not know based on the provided context. Do not answer from outside knowledge."

// InsufficientAnswer is the deterministic reply used when nothing clears the
// threshold, so no model call is required.
const InsufficientAnswer = "I don't know based on the provided context."

// Match is a retrieved chunk with its similarity score.
type Match struct {
	ID    string
	Score float32
	Text  string
}

// Source records the mapping from a rendered marker index to a chunk ID.
type Source struct {
	Index int
	ID    string
}

// GroundedPrompt is the assembled, model-ready request. It carries plain strings
// so the pure pipeline has no dependency on any LLM SDK.
type GroundedPrompt struct {
	System       string
	User         string
	Sources      []Source
	Insufficient bool
}

func approxTokens(text string) int {
	return (utf8.RuneCountInString(text) + 3) / 4
}

// Pack selects, highest-score-first, the matches at or above threshold whose
// token estimates fit within budget. A match larger than the whole budget is
// rejected, never truncated.
func Pack(matches []Match, threshold float32, budget int) []Match {
	filtered := make([]Match, 0, len(matches))
	for _, m := range matches {
		if m.Score >= threshold {
			filtered = append(filtered, m)
		}
	}
	slices.SortFunc(filtered, func(a, b Match) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID))
	})
	var packed []Match
	used := 0
	for _, m := range filtered {
		t := approxTokens(m.Text)
		if used+t > budget {
			continue // does not fit; try the next, smaller candidate
		}
		used += t
		packed = append(packed, m)
	}
	return packed
}

// Assemble builds a grounded prompt from matches under a token budget. When no
// match clears the threshold and fits, it returns an Insufficient prompt that
// needs no model call.
func Assemble(question string, matches []Match, threshold float32, budget int) GroundedPrompt {
	packed := Pack(matches, threshold, budget)
	if len(packed) == 0 {
		return GroundedPrompt{
			System:       insufficientSystem,
			User:         "Question: " + question,
			Insufficient: true,
		}
	}
	var sb strings.Builder
	sources := make([]Source, len(packed))
	for i, m := range packed {
		n := i + 1
		fmt.Fprintf(&sb, "[%d] %s\n\n", n, m.Text)
		sources[i] = Source{Index: n, ID: m.ID}
	}
	return GroundedPrompt{
		System:  groundedSystem,
		User:    fmt.Sprintf("Context:\n\n%sQuestion: %s", sb.String(), question),
		Sources: sources,
	}
}

var citationRe = regexp.MustCompile(`\[(\d+)\]`)

// ExtractCitations parses [n] markers from an answer, deduped and sorted.
func ExtractCitations(answer string) []int {
	seen := map[int]bool{}
	var out []int
	for _, m := range citationRe.FindAllStringSubmatch(answer, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// ValidateCitations flags any cited index outside 1..sourceCount.
func ValidateCitations(cited []int, sourceCount int) error {
	for _, n := range cited {
		if n < 1 || n > sourceCount {
			return fmt.Errorf("grounding: citation [%d] outside 1..%d: %w", n, sourceCount, ErrUnsupportedCitation)
		}
	}
	return nil
}

// CheckAnswer extracts and validates the citations in answer against p's sources.
func CheckAnswer(answer string, p GroundedPrompt) ([]int, error) {
	cited := ExtractCitations(answer)
	return cited, ValidateCitations(cited, len(p.Sources))
}
```

Create `complete.go`. The `//go:build online` constraint keeps the SDK dependency
out of the default build:

```go
//go:build online

package grounding

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// Completion is a grounded answer with its validated citation indices.
type Completion struct {
	Answer    string
	Citations []int
}

// Complete grounds p with a single Messages.New call, then validates the
// answer's citations against the retrieved sources. This is the one
// non-deterministic step in the pipeline, isolated behind the online build tag.
func Complete(ctx context.Context, client anthropic.Client, model anthropic.Model, p GroundedPrompt) (Completion, error) {
	if p.Insufficient {
		return Completion{Answer: InsufficientAnswer}, nil
	}
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: p.System}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(p.User)),
		},
	})
	if err != nil {
		return Completion{}, fmt.Errorf("grounding: messages.new: %w", err)
	}
	var answer string
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			answer += tb.Text
		}
	}
	cited := ExtractCitations(answer)
	if err := ValidateCitations(cited, len(p.Sources)); err != nil {
		return Completion{Answer: answer, Citations: cited}, err
	}
	return Completion{Answer: answer, Citations: cited}, nil
}
```

Create `online_test.go`, a real grounded call that skips without a key:

```go
//go:build online

package grounding

import (
	"os"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestComplete_Online(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	p := Assemble("What is the capital of France?", []Match{
		{ID: "geo-1", Score: 0.9, Text: "Paris is the capital of France."},
	}, 0.2, 2000)
	got, err := Complete(t.Context(), anthropic.NewClient(), anthropic.ModelClaudeOpus4_8, p)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Answer == "" {
		t.Fatal("empty answer")
	}
}
```

### The runnable demo

The demo assembles a prompt from three matches (one below threshold), then runs
the citation guardrail against a faithful answer and a fabricated one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/grounding"
)

func main() {
	matches := []grounding.Match{
		{ID: "kb-1", Score: 0.82, Text: "Go 1.21 introduced the built-in min and max functions."},
		{ID: "kb-2", Score: 0.41, Text: "The slices package provides generic slice helpers."},
		{ID: "kb-3", Score: 0.09, Text: "Unrelated: the cafeteria menu rotates every week."},
	}

	p := grounding.Assemble("What did Go 1.21 add?", matches, 0.2, 60)
	fmt.Printf("sources packed: %d (kb-3 below threshold)\n", len(p.Sources))
	fmt.Println("---")
	fmt.Println(p.User)
	fmt.Println("---")

	good := "Go 1.21 added the built-in min and max functions [1]."
	cited, err := grounding.CheckAnswer(good, p)
	fmt.Printf("faithful answer cites %v: err=%v\n", cited, err)

	bad := "Go 1.21 added min and max [1], plus goroutine pools [3]."
	_, err = grounding.CheckAnswer(bad, p)
	fmt.Printf("fabricated citation flagged: %t\n", errors.Is(err, grounding.ErrUnsupportedCitation))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sources packed: 2 (kb-3 below threshold)
---
Context:

[1] Go 1.21 introduced the built-in min and max functions.

[2] The slices package provides generic slice helpers.

Question: What did Go 1.21 add?
---
faithful answer cites [1]: err=<nil>
fabricated citation flagged: true
```

### Tests

The tests cover the budget, the marker mapping, the fallback, and the guardrail.
`TestPackBudget` asserts the packed context never exceeds the budget and that a
chunk larger than the whole budget is rejected rather than truncated.
`TestPackHighestScoreFirst` asserts packing order. `TestMarkersContiguous` asserts
the rendered markers are `1..n` and map to the included chunks.
`TestInsufficientFallback` asserts the fallback triggers with no sources.
`TestExtractCitations` covers dedupe. `TestFabricatedCitation` asserts an out-of-
range citation is flagged via `errors.Is`. `TestGoldenPrompt` pins the exact
rendered prompt.

Create `grounding_test.go`:

```go
package grounding

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestPackBudget(t *testing.T) {
	t.Parallel()
	matches := []Match{
		{ID: "a", Score: 0.9, Text: "aaaaaaaaaaaaaaaaaaaa"}, // 20 runes -> 5 tokens
		{ID: "b", Score: 0.8, Text: "bbbbbbbbbbbbbbbbbbbb"}, // 5 tokens
		{ID: "c", Score: 0.7, Text: "cccccccccccccccccccc"}, // 5 tokens
	}
	packed := Pack(matches, 0.0, 10) // budget fits exactly two chunks
	if len(packed) != 2 {
		t.Fatalf("packed %d chunks, want 2", len(packed))
	}
	total := 0
	for _, m := range packed {
		total += approxTokens(m.Text)
	}
	if total > 10 {
		t.Fatalf("packed %d tokens, exceeds budget 10", total)
	}
}

func TestPackRejectsOversizeChunk(t *testing.T) {
	t.Parallel()
	big := Match{ID: "big", Score: 0.9, Text: "this single chunk is far larger than the whole budget"}
	packed := Pack([]Match{big}, 0.0, 3) // ~13 tokens vs budget 3
	if len(packed) != 0 {
		t.Fatalf("packed %d chunks, want 0 (oversize rejected, not truncated)", len(packed))
	}
}

func TestPackHighestScoreFirst(t *testing.T) {
	t.Parallel()
	matches := []Match{
		{ID: "low", Score: 0.3, Text: "aaaa"},
		{ID: "high", Score: 0.9, Text: "bbbb"},
		{ID: "mid", Score: 0.6, Text: "cccc"},
	}
	packed := Pack(matches, 0.0, 100)
	got := []string{packed[0].ID, packed[1].ID, packed[2].ID}
	want := []string{"high", "mid", "low"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestMarkersContiguous(t *testing.T) {
	t.Parallel()
	p := Assemble("Q?", []Match{
		{ID: "s1", Score: 0.9, Text: "alpha"},
		{ID: "s2", Score: 0.8, Text: "beta"},
	}, 0.1, 100)
	if len(p.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(p.Sources))
	}
	for i, s := range p.Sources {
		if s.Index != i+1 {
			t.Fatalf("source %d has index %d, want %d", i, s.Index, i+1)
		}
	}
	markers := ExtractCitations(p.User) // the prompt itself carries [1] [2]
	if !slices.Equal(markers, []int{1, 2}) {
		t.Fatalf("markers = %v, want [1 2]", markers)
	}
}

func TestInsufficientFallback(t *testing.T) {
	t.Parallel()
	p := Assemble("Q?", []Match{
		{ID: "x", Score: 0.05, Text: "irrelevant"},
	}, 0.5, 100) // nothing clears the threshold
	if !p.Insufficient {
		t.Fatal("want Insufficient prompt")
	}
	if len(p.Sources) != 0 {
		t.Fatalf("sources = %d, want 0", len(p.Sources))
	}
}

func TestExtractCitations(t *testing.T) {
	t.Parallel()
	got := ExtractCitations("First [2], then [1], and again [2]. Also [10].")
	want := []int{1, 2, 10}
	if !slices.Equal(got, want) {
		t.Fatalf("citations = %v, want %v", got, want)
	}
}

func TestFabricatedCitation(t *testing.T) {
	t.Parallel()
	p := Assemble("Q?", []Match{
		{ID: "s1", Score: 0.9, Text: "alpha"},
		{ID: "s2", Score: 0.8, Text: "beta"},
		{ID: "s3", Score: 0.7, Text: "gamma"},
	}, 0.1, 100)
	_, err := CheckAnswer("A claim [1] and a fabricated one [9].", p)
	if !errors.Is(err, ErrUnsupportedCitation) {
		t.Fatalf("err = %v, want ErrUnsupportedCitation", err)
	}
}

func TestGoldenPrompt(t *testing.T) {
	t.Parallel()
	p := Assemble("Q?", []Match{
		{ID: "s1", Score: 0.9, Text: "Alpha."},
		{ID: "s2", Score: 0.8, Text: "Beta."},
	}, 0.1, 100)
	want := "Context:\n\n[1] Alpha.\n\n[2] Beta.\n\nQuestion: Q?"
	if p.User != want {
		t.Fatalf("prompt mismatch:\n got %q\nwant %q", p.User, want)
	}
}

func ExampleExtractCitations() {
	fmt.Println(ExtractCitations("grounded in [2] and [1], per [2]"))
	// Output: [1 2]
}
```

## Review

The grounding stage is correct when three properties hold. The packed context
never exceeds the budget and drops from the tail — `TestPackBudget` and
`TestPackHighestScoreFirst` pin both. An oversize chunk is rejected, never
truncated, so citations always point at whole passages
(`TestPackRejectsOversizeChunk`). And the citation guardrail flags any index
outside the retrieved set (`TestFabricatedCitation`), which is the mechanism that
turns "the model cited a source" into "the model cited a source that actually
exists."

The mistakes to avoid are the ones that quietly ship hallucinations. Do not force
an answer when nothing clears the threshold — `Assemble` returns an `Insufficient`
prompt precisely so the pipeline can short-circuit to `InsufficientAnswer` with no
model call. Do not trust that retrieval success implies a faithful answer; run
`ValidateCitations` on every response. And do not treat the token estimate as
exact — it is a rune-based proxy, so budget with headroom for the system prompt,
question, and answer. Run `go test -race` for the pure pipeline; run `go build
-tags online ./...` (with the SDK fetched) to compile the completion call.

## Resources

- [`regexp`](https://pkg.go.dev/regexp) — `MustCompile` and `FindAllStringSubmatch` for extracting `[n]` markers.
- [anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) — `Messages.New`, `MessageNewParams`, `NewUserMessage`, `NewTextBlock`, and System `TextBlockParam` blocks.
- [Anthropic: Introducing Contextual Retrieval](https://www.anthropic.com/news/contextual-retrieval) — grounding, reranking, and reducing retrieval failures.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-retriever.md](02-retriever.md) | Next: [../06-mcp-server-in-go/00-concepts.md](../06-mcp-server-in-go/00-concepts.md)
