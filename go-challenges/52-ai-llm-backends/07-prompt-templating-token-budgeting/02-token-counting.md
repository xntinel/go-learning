# Exercise 2: A pluggable, offline-deterministic token counter

Counting tokens is the load-bearing measurement behind every budget decision, and
the wrong approach — a network call per request, or the wrong encoding, or a
`chars/4` guess — turns into latency, rate-limit pain, and `context_length_exceeded`
storms in production. This exercise builds a `TokenCounter` interface with two
offline implementations: an exact tiktoken counter backed by an embedded vocab
(deterministic in CI, no network) selected per model family, and a dependency-free
heuristic fallback. A network-exact Anthropic counter lives in a separate file
behind a build tag so the default test path stays hermetic.

This module is self-contained: its own `go mod init`, its own demo and tests.
Because it imports the tiktoken tokenizer (an external module) and, behind a build
tag, the Anthropic SDK, the offline gate will not fetch those modules — the build
step is expected to fail without network access. The bar for this exercise is
`gofmt`-clean, real verified APIs, and prose that matches the code; the tiktoken
vocab is embedded, so the counts are deterministic on any machine that has the
module.

## What you'll build

```text
tokens/                     independent module: example.com/tokens
  go.mod                    go 1.26; requires tiktoken-go/tokenizer (+ anthropic-sdk-go, online)
  tokens.go                 TokenCounter interface; TiktokenCounter; HeuristicCounter; Message; ErrUnknownModel
  anthropic_online.go       //go:build online — AnthropicCounter via count_tokens API
  cmd/
    demo/
      main.go               exact vs heuristic on one text; heuristic message total
  tokens_test.go            exact pinned counts, model-to-encoding selection, overhead, heuristic bound
```

- Files: `tokens.go`, `anthropic_online.go`, `cmd/demo/main.go`, `tokens_test.go`.
- Implement: `TokenCounter` with `Count(text) (int, error)` and `CountMessages([]Message) (int, error)`; a `TiktokenCounter` selecting `cl100k_base` or `o200k_base` by model family; a `HeuristicCounter` using a rune-based estimate; sentinel `ErrUnknownModel`; and an `AnthropicCounter` behind `//go:build online`.
- Test (no network in the default path): pinned exact counts under both encodings, empty string is 0, `gpt-4o` selects `o200k` and `gpt-4` selects `cl100k`, `CountMessages` adds the fixed per-message and priming overhead, and the heuristic is monotonic and within a documented bound of the exact counter.
- Verify: `go test -count=1 -race ./...` (add `-tags online` and `ANTHROPIC_API_KEY` to exercise the network counter).

Set up the module and add the dependencies:

```bash
mkdir -p ~/go-exercises/tokens/cmd/demo
cd ~/go-exercises/tokens
go mod init example.com/tokens
go mod edit -go=1.26
go get github.com/tiktoken-go/tokenizer@latest
go get github.com/anthropics/anthropic-sdk-go@latest
```

### The interface and why it exists

The encoding is a property of the model family, and Claude does not use tiktoken at
all, so a single `Count` function cannot be right for every provider. The
`TokenCounter` interface makes the counting strategy swappable: an exact local
tiktoken counter for OpenAI, a network `count_tokens` counter for Anthropic, and a
cheap heuristic when neither is available or worth the cost. `CountMessages` is a
separate method because a chat request is more than the sum of its message strings:
it adds fixed per-message tokens and a few tokens to prime the assistant's reply,
which a naive per-string count misses. The values here mirror the OpenAI cookbook's
`tokens_per_message` and reply-priming constants.

### Selecting the encoding

`tiktoken-go/tokenizer` embeds the BPE vocab in the binary, so `tokenizer.Get`
returns a `Codec` with no network access — this is what keeps counting hermetic and
CI-safe, unlike loaders that download the dictionary at runtime. The one decision
you must get right is which encoding to use: `cl100k_base` for `gpt-4`,
`gpt-3.5-turbo`, and `text-embedding-ada-002`; `o200k_base` for `gpt-4o` and
`gpt-4.1` (and the o-series reasoning models). Using the wrong encoding silently
drifts, so `encodingForModel` selects by prefix and returns `ErrUnknownModel` for
anything it does not recognize, rather than guessing.

Create `tokens.go`:

```go
package tokens

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// ErrUnknownModel is returned when a model name maps to no known encoding.
var ErrUnknownModel = errors.New("unknown model family")

// Chat overhead constants mirror OpenAI's tokens_per_message and reply priming.
// A chat request is more than the sum of its message strings.
const (
	tokensPerMessage = 3
	tokensPerReply   = 3
)

// Message is one chat turn. Role and Content each contribute tokens.
type Message struct {
	Role    string
	Content string
}

// TokenCounter counts tokens for a single string and for a full chat request.
// Implementations may be exact (tiktoken, Anthropic) or approximate (heuristic).
type TokenCounter interface {
	Count(text string) (int, error)
	CountMessages(msgs []Message) (int, error)
}

// encodingForModel picks the BPE encoding by model family. The encoding is a
// property of the model, so the wrong choice produces wrong counts.
func encodingForModel(model string) (tokenizer.Encoding, error) {
	switch {
	case strings.HasPrefix(model, "gpt-4o"),
		strings.HasPrefix(model, "gpt-4.1"),
		strings.HasPrefix(model, "o1"),
		strings.HasPrefix(model, "o3"),
		strings.HasPrefix(model, "o4"):
		return tokenizer.O200kBase, nil
	case strings.HasPrefix(model, "gpt-4"),
		strings.HasPrefix(model, "gpt-3.5"),
		model == "text-embedding-ada-002":
		return tokenizer.Cl100kBase, nil
	default:
		return "", fmt.Errorf("tokens: %q: %w", model, ErrUnknownModel)
	}
}

// TiktokenCounter is an exact, offline OpenAI token counter. The vocab is embedded
// in the binary, so it never touches the network.
type TiktokenCounter struct {
	enc   tokenizer.Encoding
	codec tokenizer.Codec
}

// NewTiktokenCounter builds a counter for the given model's encoding.
func NewTiktokenCounter(model string) (*TiktokenCounter, error) {
	enc, err := encodingForModel(model)
	if err != nil {
		return nil, err
	}
	codec, err := tokenizer.Get(enc)
	if err != nil {
		return nil, fmt.Errorf("tokens: load codec %q: %w", enc, err)
	}
	return &TiktokenCounter{enc: enc, codec: codec}, nil
}

// Encoding reports the encoding this counter selected, for tests and diagnostics.
func (c *TiktokenCounter) Encoding() tokenizer.Encoding { return c.enc }

// Count returns the exact BPE token count of text.
func (c *TiktokenCounter) Count(text string) (int, error) {
	n, err := c.codec.Count(text)
	if err != nil {
		return 0, fmt.Errorf("tokens: count: %w", err)
	}
	return n, nil
}

// CountMessages sums each message's role and content plus the fixed per-message
// overhead, then adds the reply-priming tokens once.
func (c *TiktokenCounter) CountMessages(msgs []Message) (int, error) {
	total := 0
	for _, m := range msgs {
		total += tokensPerMessage
		role, err := c.Count(m.Role)
		if err != nil {
			return 0, err
		}
		content, err := c.Count(m.Content)
		if err != nil {
			return 0, err
		}
		total += role + content
	}
	total += tokensPerReply
	return total, nil
}

// HeuristicCounter is a dependency-free estimate: roughly one token per four runes.
// It drifts on code and non-Latin scripts and should be treated as a rough bound,
// not an exact count.
type HeuristicCounter struct{}

// Count returns the ceiling of runes/4, and 0 for the empty string.
func (HeuristicCounter) Count(text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	runes := utf8.RuneCountInString(text)
	return (runes + 3) / 4, nil
}

// CountMessages applies the same per-message and priming overhead as the exact
// counter so budgets built on either implementation are comparable.
func (h HeuristicCounter) CountMessages(msgs []Message) (int, error) {
	total := 0
	for _, m := range msgs {
		total += tokensPerMessage
		role, _ := h.Count(m.Role)
		content, _ := h.Count(m.Content)
		total += role + content
	}
	total += tokensPerReply
	return total, nil
}
```

### The network-exact Anthropic counter, behind a build tag

Claude is not tiktoken-compatible, and its tokenizer is not public, so exact Claude
counts require the `count_tokens` API — a network round trip. Keeping it behind
`//go:build online` means the default `go test` never hits the network and stays
hermetic; you opt in with `-tags online`. The Go SDK's
`client.Messages.CountTokens` takes a `MessageCountTokensParams` and returns a
`*MessageTokensCount` whose `InputTokens` field is the answer (an `int64`), and it
counts system, tools, and images for you.

Create `anthropic_online.go`:

```go
//go:build online

package tokens

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicCounter counts Claude tokens exactly via the count_tokens API. It makes
// a network call per count, so cache results for immutable segments.
type AnthropicCounter struct {
	client anthropic.Client
	model  anthropic.Model
}

// NewAnthropicCounter builds a counter for a Claude model.
func NewAnthropicCounter(apiKey string, model anthropic.Model) *AnthropicCounter {
	return &AnthropicCounter{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
		model:  model,
	}
}

// Count counts a single string as one user message.
func (a *AnthropicCounter) Count(text string) (int, error) {
	return a.CountMessages([]Message{{Role: "user", Content: text}})
}

// CountMessages asks the API for the exact input-token count of the chat request.
func (a *AnthropicCounter) CountMessages(msgs []Message) (int, error) {
	params := anthropic.MessageCountTokensParams{
		Model:    a.model,
		Messages: toAnthropic(msgs),
	}
	resp, err := a.client.Messages.CountTokens(context.Background(), params)
	if err != nil {
		return 0, err
	}
	return int(resp.InputTokens), nil
}

func toAnthropic(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Content)
		if m.Role == "assistant" {
			out = append(out, anthropic.NewAssistantMessage(block))
			continue
		}
		out = append(out, anthropic.NewUserMessage(block))
	}
	return out
}
```

### The runnable demo

The demo compares the exact tiktoken count against the heuristic for one string,
then budgets a two-message chat with the heuristic — the message total is fully
deterministic from the rune-based estimate.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/tokens"
)

func main() {
	counter, err := tokens.NewTiktokenCounter("gpt-4")
	if err != nil {
		log.Fatal(err)
	}
	var heuristic tokens.HeuristicCounter

	text := "Hello, world!"
	exact, err := counter.Count(text)
	if err != nil {
		log.Fatal(err)
	}
	approx, _ := heuristic.Count(text)
	fmt.Printf("text: %q\n", text)
	fmt.Printf("exact (cl100k): %d\n", exact)
	fmt.Printf("heuristic:      %d\n", approx)

	msgs := []tokens.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Summarize the incident report."},
	}
	total, _ := heuristic.CountMessages(msgs)
	fmt.Printf("messages (heuristic total): %d\n", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
text: "Hello, world!"
exact (cl100k): 4
heuristic:      4
messages (heuristic total): 27
```

### Tests

No network in the default path. The exact-count cases pin values from the embedded
codec that do not drift: `"hello world"` is two tokens under both encodings, the
empty string is zero, and `"a"` is one. The selection test confirms `gpt-4o` picks
`o200k` and `gpt-4` picks `cl100k`. The overhead test asserts `CountMessages`
equals the manual sum plus the fixed constants without pinning any magic number,
and the heuristic test asserts monotonicity plus a documented factor-of-two bound
against the exact counter.

Create `tokens_test.go`:

```go
package tokens

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

func TestExactCounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		text  string
		want  int
	}{
		{"gpt-4", "", 0},
		{"gpt-4", "a", 1},
		{"gpt-4", "hello world", 2},
		{"gpt-4o", "", 0},
		{"gpt-4o", "hello world", 2},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%q", tt.model, tt.text), func(t *testing.T) {
			t.Parallel()
			c, err := NewTiktokenCounter(tt.model)
			if err != nil {
				t.Fatalf("NewTiktokenCounter: %v", err)
			}
			got, err := c.Count(tt.text)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Count(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestEncodingSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  tokenizer.Encoding
	}{
		{"gpt-4o", tokenizer.O200kBase},
		{"gpt-4.1", tokenizer.O200kBase},
		{"gpt-4", tokenizer.Cl100kBase},
		{"gpt-3.5-turbo", tokenizer.Cl100kBase},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			c, err := NewTiktokenCounter(tt.model)
			if err != nil {
				t.Fatalf("NewTiktokenCounter: %v", err)
			}
			if c.Encoding() != tt.want {
				t.Fatalf("Encoding() = %q, want %q", c.Encoding(), tt.want)
			}
		})
	}
}

func TestCountMessagesOverhead(t *testing.T) {
	t.Parallel()
	c, err := NewTiktokenCounter("gpt-4")
	if err != nil {
		t.Fatalf("NewTiktokenCounter: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "hello world"}}
	got, err := c.CountMessages(msgs)
	if err != nil {
		t.Fatalf("CountMessages: %v", err)
	}
	role, _ := c.Count("user")
	content, _ := c.Count("hello world")
	want := tokensPerMessage + role + content + tokensPerReply
	if got != want {
		t.Fatalf("CountMessages = %d, want %d (per-message + role + content + reply)", got, want)
	}
}

func TestHeuristicMonotonic(t *testing.T) {
	t.Parallel()
	var h HeuristicCounter
	prev := -1
	for _, s := range []string{"", "a", "ab", "abcd", "abcdefgh", "abcdefghijklmnop"} {
		n, _ := h.Count(s)
		if n < prev {
			t.Fatalf("heuristic not monotonic: Count(%q)=%d < prev %d", s, n, prev)
		}
		prev = n
	}
}

func TestHeuristicBound(t *testing.T) {
	t.Parallel()
	exact, err := NewTiktokenCounter("gpt-4")
	if err != nil {
		t.Fatalf("NewTiktokenCounter: %v", err)
	}
	var h HeuristicCounter
	corpus := []string{
		"The quick brown fox jumps over the lazy dog.",
		"Budgeting is allocation under a hard, priced ceiling.",
		"func main() { fmt.Println(\"hello\") }",
	}
	for _, s := range corpus {
		e, err := exact.Count(s)
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		a, _ := h.Count(s)
		// The heuristic is documented to stay within a factor of two of exact on
		// natural-language and short-code inputs.
		if a > 2*e || e > 2*a {
			t.Fatalf("heuristic %d drifts beyond 2x of exact %d for %q", a, e, s)
		}
	}
}

// Your turn: gpt-5 uses o200k_base. Extend encodingForModel to select it, then
// change this test to assert a working counter instead of ErrUnknownModel.
func TestNewModelFamily(t *testing.T) {
	t.Parallel()
	_, err := NewTiktokenCounter("gpt-5")
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel until you add gpt-5", err)
	}
}

func ExampleHeuristicCounter() {
	var c HeuristicCounter
	n, _ := c.Count("the quick brown fox") // 19 runes -> ceil(19/4) = 5
	fmt.Println(n)
	// Output: 5
}
```

## Review

The counter is correct when the encoding matches the model and the overhead is
accounted for. `TestExactCounts` pins values that do not drift because the vocab is
embedded, so a regression in encoding selection or codec wiring shows up
immediately; `TestEncodingSelection` proves `gpt-4o` and `gpt-4` diverge as they
must. `TestCountMessagesOverhead` deliberately computes its expectation from the
counter itself plus the constants, so it verifies the per-message and priming
overhead without hard-coding a fragile magic number.

The mistakes to avoid are the concepts made concrete. Do not count Claude tokens
with a tiktoken encoding — that is why the Anthropic counter lives in its own
build-tagged file and uses `CountTokens`. Do not depend on a runtime BPE download;
`tiktoken-go/tokenizer` embeds the vocab so tests stay hermetic and offline builds
work. And do not treat the heuristic as exact — `TestHeuristicBound` documents that
it is only within a factor of two, which is fine for a rough guardrail and wrong for
a hard budget. When exact counts matter, use the tiktoken or Anthropic counter and
cache the results for immutable segments.

## Resources

- [`tiktoken-go/tokenizer`](https://pkg.go.dev/github.com/tiktoken-go/tokenizer) — embedded, offline BPE tokenizer: `Get`, `ForModel`, and `Codec.Count`/`Encode`/`Decode`.
- [OpenAI Cookbook: How to count tokens with tiktoken](https://cookbook.openai.com/examples/how_to_count_tokens_with_tiktoken) — the per-message and reply-priming overhead this counter mirrors.
- [Anthropic Messages count_tokens API](https://docs.claude.com/en/api/messages-count-tokens) — the network-exact Claude counter; returns `input_tokens` and counts system, tools, and images.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-safe-prompt-templating.md](01-safe-prompt-templating.md) | Next: [03-context-budget-assembler.md](03-context-budget-assembler.md)
