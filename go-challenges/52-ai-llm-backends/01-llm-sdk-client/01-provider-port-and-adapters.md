# Exercise 1: A provider-agnostic Completer port with Anthropic and OpenAI adapters

You will build a domain-level `Completer` interface and two adapters that wrap the
official Anthropic and OpenAI Go SDK clients, translating one neutral request and
response type to and from each vendor's typed params. The test suite drives both
adapters against an `httptest` server with zero network, proving the neutral
mapping is correct in both directions.

## What you'll build

```text
llmclient/                        independent module: example.com/llmclient
  go.mod                          go 1.26; requires the Anthropic + OpenAI SDKs
  completer.go                    Role, Message, Request, Response, Completer (the port)
  anthropic.go                    AnthropicAdapter: neutral <-> anthropic.MessageNewParams
  openai.go                       OpenAIAdapter: neutral <-> openai.ChatCompletionNewParams
  cmd/
    demo/
      main.go                     both adapters pointed at in-process httptest servers
  llmclient_test.go               table-driven httptest tests; sentinel via errors.Is; Example
```

Files: `completer.go`, `anthropic.go`, `openai.go`, `cmd/demo/main.go`, `llmclient_test.go`.
Implement: a neutral `Request`/`Response`, a `Completer` port, and one adapter per vendor that wires the SDK client (API key + base URL options), builds the vendor request from the neutral one, and extracts the neutral response from the vendor union.
Test: stand up an `httptest.NewServer` per vendor, point the adapter at it with `option.WithBaseURL`, and assert both the outbound request path/JSON and the extracted neutral `Response`.
Verify: `go test -count=1 -race ./...`

Set up the module and fetch the SDKs:

```bash
mkdir -p ~/go-exercises/llmclient/cmd/demo
cd ~/go-exercises/llmclient
go mod init example.com/llmclient
go get github.com/anthropics/anthropic-sdk-go@latest
go get github.com/openai/openai-go/v3@latest
```

### The port and the neutral types

The domain owns the vocabulary. `Role`, `Message`, `Request`, and `Response` are
plain structs with no vendor types anywhere in sight, and `Completer` is the
single method the rest of the service depends on. Keeping `System` as its own
field on `Request` (rather than a message with a system role) is deliberate:
Anthropic wants the system prompt top-level, and the OpenAI adapter can trivially
prepend it as a system message, so the neutral shape matches Anthropic's structure
and the OpenAI adapter does the small translation. `Response.Text` is a single
flattened string; each adapter is responsible for producing it from its vendor's
very different response body.

Create `completer.go`:

```go
package llmclient

import "context"

// Role is a neutral message role. System prompts travel in Request.System, so
// the message list only ever carries user and assistant turns.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in a conversation.
type Message struct {
	Role Role
	Text string
}

// Request is the vendor-neutral completion request the domain builds.
type Request struct {
	Model     string // config-driven vendor model ID; never a hardcoded constant
	System    string // optional system prompt (Anthropic top-level; OpenAI prepended)
	Messages  []Message
	MaxTokens int // output cap; required by Anthropic, optional for OpenAI
}

// Response is the vendor-neutral completion result. Text is the concatenation of
// all text output; StopReason is the vendor's raw stop/finish reason.
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
	StopReason   string
}

// Completer is the port. Handlers depend on this, not on any vendor SDK.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
```

### The Anthropic adapter

The adapter wraps an `anthropic.Client` (constructed once, with client options)
and does three jobs on each call: build the vendor request, invoke the SDK, and
narrow the union response. Two vendor-specific details show up here. First,
`MaxTokens` is required, so the adapter always sets it — defaulting a zero value
to a sane cap rather than sending an invalid request. Second, the response
`Content` is a slice of blocks, so the adapter iterates and type-switches with
`AsAny()`, collecting only the text blocks; a `thinking` block would otherwise be
mistaken for the answer if you naively read `Content[0]`.

Create `anthropic.go`:

```go
package llmclient

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicAdapter implements Completer against the Anthropic Messages API.
type AnthropicAdapter struct {
	client       anthropic.Client
	defaultLimit int64
}

// NewAnthropicAdapter constructs the adapter. Pass option.WithAPIKey and
// option.WithBaseURL here; the zero-arg SDK client already reads ANTHROPIC_API_KEY
// from the environment, so a bare NewAnthropicAdapter() works in production.
func NewAnthropicAdapter(opts ...option.RequestOption) *AnthropicAdapter {
	return &AnthropicAdapter{
		client:       anthropic.NewClient(opts...),
		defaultLimit: 1024,
	}
}

func (a *AnthropicAdapter) Complete(ctx context.Context, req Request) (Response, error) {
	limit := int64(req.MaxTokens)
	if limit <= 0 {
		limit = a.defaultLimit // MaxTokens is REQUIRED by Anthropic; never send 0
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: limit,
		Messages:  toAnthropicMessages(req.Messages),
	}
	if req.System != "" {
		// System is a top-level field on Anthropic, not a message.
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}

	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, err
	}

	// Content is a union of blocks (text, thinking, ...). Narrow with AsAny and
	// collect only text; the first block is not guaranteed to be text.
	var sb strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}

	return Response{
		Text:         sb.String(),
		InputTokens:  int(msg.Usage.InputTokens),
		OutputTokens: int(msg.Usage.OutputTokens),
		StopReason:   string(msg.StopReason),
	}, nil
}

func toAnthropicMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Text)
		if m.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(block))
			continue
		}
		out = append(out, anthropic.NewUserMessage(block))
	}
	return out
}
```

### The OpenAI adapter

The OpenAI adapter has the mirror-image shape mismatch. The system prompt is not a
top-level field, so the adapter prepends an `openai.SystemMessage` to the message
slice. The output cap is optional, so the adapter only sets
`MaxCompletionTokens` when the caller asked for one — and it uses the
`param.Opt[T]` constructor `openai.Int` so an unset cap is genuinely omitted
rather than sent as a zero. The response is `Choices[0].Message.Content`, a plain
string, but the adapter length-checks `Choices` first: an empty slice is a real
(if rare) response and indexing it blindly panics.

Create `openai.go`:

```go
package llmclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// ErrNoChoices is returned when the OpenAI response carries no choices.
var ErrNoChoices = errors.New("openai: response contained no choices")

// OpenAIAdapter implements Completer against the OpenAI Chat Completions API.
type OpenAIAdapter struct {
	client openai.Client
}

// NewOpenAIAdapter constructs the adapter. Pass option.WithAPIKey and
// option.WithBaseURL here; the zero-arg SDK client reads OPENAI_API_KEY from the
// environment.
func NewOpenAIAdapter(opts ...option.RequestOption) *OpenAIAdapter {
	return &OpenAIAdapter{client: openai.NewClient(opts...)}
}

func (a *OpenAIAdapter) Complete(ctx context.Context, req Request) (Response, error) {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		// OpenAI folds the system prompt into the messages array.
		msgs = append(msgs, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		if m.Role == RoleAssistant {
			msgs = append(msgs, openai.AssistantMessage(m.Text))
			continue
		}
		msgs = append(msgs, openai.UserMessage(m.Text))
	}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(req.Model),
		Messages: msgs,
	}
	if req.MaxTokens > 0 {
		// param.Opt constructor: sets the field only when the caller asked for a
		// cap, so an unset cap is omitted rather than sent as 0.
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}

	cc, err := a.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, err
	}
	if len(cc.Choices) == 0 {
		return Response{}, fmt.Errorf("openai: %w", ErrNoChoices)
	}

	choice := cc.Choices[0]
	return Response{
		Text:         choice.Message.Content,
		InputTokens:  int(cc.Usage.PromptTokens),
		OutputTokens: int(cc.Usage.CompletionTokens),
		StopReason:   string(choice.FinishReason),
	}, nil
}
```

### The runnable demo

The demo is fully self-contained and offline: it stands up one in-process
`httptest.NewServer` per vendor, each returning a canned vendor JSON body, and
points the adapters at them with `option.WithBaseURL`. This is exactly the
production `WithBaseURL` knob doing double duty — the same code that would target a
gateway in production targets a mock here — so the output is deterministic and the
demo demonstrates the single most valuable production option.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/llmclient"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	openaiopt "github.com/openai/openai-go/v3/option"
)

func main() {
	anthropicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant",`+
			`"model":"claude-opus-4-8","content":[{"type":"text","text":"Hello from Claude"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":5}}`)
	}))
	defer anthropicSrv.Close()

	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5-2",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello from GPT"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`)
	}))
	defer openaiSrv.Close()

	claude := llmclient.NewAnthropicAdapter(
		anthropicopt.WithAPIKey("test-key"),
		anthropicopt.WithBaseURL(anthropicSrv.URL),
	)
	gpt := llmclient.NewOpenAIAdapter(
		openaiopt.WithAPIKey("test-key"),
		openaiopt.WithBaseURL(openaiSrv.URL),
	)

	adapters := []struct {
		name string
		c    llmclient.Completer
		mdl  string
	}{
		{"anthropic", claude, "claude-opus-4-8"},
		{"openai", gpt, "gpt-5-2"},
	}

	ctx := context.Background()
	for _, a := range adapters {
		resp, err := a.c.Complete(ctx, llmclient.Request{
			Model:     a.mdl,
			System:    "You are terse.",
			Messages:  []llmclient.Message{{Role: llmclient.RoleUser, Text: "hi"}},
			MaxTokens: 64,
		})
		if err != nil {
			fmt.Printf("%s: error: %v\n", a.name, err)
			continue
		}
		fmt.Printf("%s: %q in=%d out=%d stop=%s\n",
			a.name, resp.Text, resp.InputTokens, resp.OutputTokens, resp.StopReason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
anthropic: "Hello from Claude" in=12 out=5 stop=end_turn
openai: "Hello from GPT" in=9 out=4 stop=stop
```

### Tests

The tests are the point of the whole exercise: they prove the adapter maps
neutral to vendor correctly *and* extracts vendor to neutral correctly, with no
network. Each case stands up an `httptest` server whose handler both records the
outbound request (path and decoded JSON body) for assertion and returns a canned
vendor response. `TestAnthropicRequestMapping` asserts the system prompt landed in
the top-level `system` field and the max-tokens cap reached the wire.
`TestOpenAIRequestMapping` asserts the system prompt was prepended as a
`role: "system"` message. `TestResponseExtraction` proves the neutral `Response`
is pulled correctly from each vendor's union. `TestOpenAINoChoices` proves the
empty-`Choices` guard returns the wrapped sentinel, asserted with `errors.Is`.

Create `llmclient_test.go`:

```go
package llmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	openaiopt "github.com/openai/openai-go/v3/option"
)

// capture records the last request a stub server received.
type capture struct {
	path string
	body map[string]any
}

func stubServer(t *testing.T, cap *capture, responseJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.path = r.URL.Path
		_ = json.Unmarshal(raw, &cap.body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responseJSON)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const anthropicOK = `{"id":"msg_1","type":"message","role":"assistant",` +
	`"model":"claude-opus-4-8","content":[{"type":"text","text":"answer"}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`

const openaiOK = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5-2",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

func TestAnthropicRequestMapping(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := stubServer(t, &cap, anthropicOK)
	a := NewAnthropicAdapter(anthropicopt.WithAPIKey("k"), anthropicopt.WithBaseURL(srv.URL))

	_, err := a.Complete(context.Background(), Request{
		Model:     "claude-opus-4-8",
		System:    "be terse",
		Messages:  []Message{{Role: RoleUser, Text: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if cap.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", cap.path)
	}
	if got := cap.body["max_tokens"]; got != float64(64) {
		t.Errorf("max_tokens on wire = %v, want 64", got)
	}
	// Anthropic carries the system prompt top-level, not in the messages array.
	if _, ok := cap.body["system"]; !ok {
		t.Error("system prompt missing from top-level request field")
	}
}

func TestOpenAIRequestMapping(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := stubServer(t, &cap, openaiOK)
	a := NewOpenAIAdapter(openaiopt.WithAPIKey("k"), openaiopt.WithBaseURL(srv.URL))

	_, err := a.Complete(context.Background(), Request{
		Model:    "gpt-5-2",
		System:   "be terse",
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if cap.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", cap.path)
	}
	// OpenAI folds the system prompt into messages[0] as role "system".
	msgs, ok := cap.body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages missing or empty: %v", cap.body["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("messages[0].role = %v, want system", first["role"])
	}
}

func TestResponseExtraction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		newFn   func() Completer
		respGot Response
	}{
		{
			name: "anthropic",
			newFn: func() Completer {
				srv := stubServer(t, &capture{}, anthropicOK)
				return NewAnthropicAdapter(anthropicopt.WithAPIKey("k"), anthropicopt.WithBaseURL(srv.URL))
			},
			respGot: Response{Text: "answer", InputTokens: 7, OutputTokens: 3, StopReason: "end_turn"},
		},
		{
			name: "openai",
			newFn: func() Completer {
				srv := stubServer(t, &capture{}, openaiOK)
				return NewOpenAIAdapter(openaiopt.WithAPIKey("k"), openaiopt.WithBaseURL(srv.URL))
			},
			respGot: Response{Text: "answer", InputTokens: 7, OutputTokens: 3, StopReason: "stop"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := tc.newFn()
			got, err := c.Complete(context.Background(), Request{
				Model:     "m",
				Messages:  []Message{{Role: RoleUser, Text: "hi"}},
				MaxTokens: 16,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got != tc.respGot {
				t.Errorf("Response = %+v, want %+v", got, tc.respGot)
			}
		})
	}
}

func TestOpenAINoChoices(t *testing.T) {
	t.Parallel()
	var cap capture
	srv := stubServer(t, &cap, `{"id":"c","object":"chat.completion","model":"m","choices":[],`+
		`"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	a := NewOpenAIAdapter(openaiopt.WithAPIKey("k"), openaiopt.WithBaseURL(srv.URL))

	_, err := a.Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	if !errors.Is(err, ErrNoChoices) {
		t.Fatalf("err = %v, want wrapped ErrNoChoices", err)
	}
}

func ExampleAnthropicAdapter() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, anthropicOK)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(anthropicopt.WithAPIKey("k"), anthropicopt.WithBaseURL(srv.URL))
	resp, _ := a.Complete(context.Background(), Request{
		Model:     "claude-opus-4-8",
		Messages:  []Message{{Role: RoleUser, Text: "hi"}},
		MaxTokens: 16,
	})
	fmt.Printf("%s (%d/%d)\n", resp.Text, resp.InputTokens, resp.OutputTokens)
	// Output: answer (7/3)
}
```

## Review

The adapter is correct when a round trip preserves meaning in both directions.
Outbound: the neutral `System` reaches Anthropic's top-level `system` field and
OpenAI's `messages[0]` with role `system`; the neutral `MaxTokens` reaches the
wire; and the model ID is whatever config supplied. Inbound: `Response.Text` is
the concatenation of Anthropic's text blocks (skipping any non-text block) or
OpenAI's `Choices[0].Message.Content`, and usage/stop-reason are normalized. The
tests pin all of this without a network because `option.WithBaseURL` redirects the
SDK at an `httptest` server; if a test ever needs the live API, that belongs in a
separate file behind `//go:build online`.

The mistakes to avoid are the ones the code was written to prevent. Do not read
`msg.Content[0].Text` directly — the first Anthropic block may be a `thinking`
block, which is why the adapter iterates and narrows with `AsAny()`. Do not index
`cc.Choices[0]` without the length check — `TestOpenAINoChoices` exists precisely
to prove the guard returns the wrapped `ErrNoChoices` under `errors.Is`. Do not
send a zero `MaxTokens` to Anthropic; the adapter defaults it because Anthropic
requires it. And keep the vendor SDK types on the adapter side of the boundary:
the `Completer` interface speaks only in the neutral `Request`/`Response`, which
is what lets a handler swap vendors with a config change. Run `go test -race` to
confirm the adapters are safe under concurrent calls, since one adapter instance
is shared across goroutines in a real service.

## Resources

- [Anthropic Go SDK — pkg.go.dev reference](https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go) — `MessageNewParams`, `NewUserMessage`, and the `Content` union.
- [OpenAI Go SDK v3 — pkg.go.dev reference](https://pkg.go.dev/github.com/openai/openai-go/v3) — `ChatCompletionNewParams`, the message constructors, and `Choices`.
- [OpenAI Go SDK — GitHub](https://github.com/openai/openai-go) — client construction, chat completions, and the `option` package in the README.
- [Anthropic Go SDK — GitHub](https://github.com/anthropics/anthropic-sdk-go) — the `examples/` directory and the `option` package.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-config-models-and-generation-params.md](02-config-models-and-generation-params.md)
