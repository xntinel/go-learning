# Exercise 3: Cross-Provider Parity — the OpenAI Chat Completions Tool Loop and tool_choice

The state machine from Exercise 2 is provider-independent; only the envelope
changes. This exercise builds the same loop against OpenAI Chat Completions,
which is where the wire differences become concrete: tools are function
definitions, tool calls arrive in `Message.ToolCalls` with `Arguments` as a raw
JSON *string*, results go back as `role=tool` messages keyed by `tool_call_id`,
and `finish_reason == "tool_calls"` drives the loop. Reusing the same dispatcher
shape across both providers is the argument for an internal provider abstraction.

This module is self-contained: its own `go mod init`, the same bundled dispatcher,
and the live client behind a build tag.

## What you'll build

```text
openaitools/                independent module: example.com/openaitools
  go.mod                    go 1.26; requires github.com/openai/openai-go/v3
  loop.go                   CompletionCreator interface; RunLoop; ToolBox; WeatherTool; ForceTool
  live.go                   //go:build llm — wires the real openai.Client
  cmd/
    demo/
      main.go               drives RunLoop with a scripted stand-in model (no key needed)
  loop_test.go              fake CompletionCreator: parallel calls, id matching, ordering, budget, cancel, forced tool
```

- Files: `loop.go`, `live.go`, `cmd/demo/main.go`, `loop_test.go`.
- Implement: a `RunLoop` that appends `choice.Message.ToParam()`, and — while `finish_reason` is `"tool_calls"` — dispatches each `ToolCall` (decoding its `Arguments` JSON string), appends one `openai.ToolMessage(content, tool_call_id)` per call, and repeats until a non-tool finish reason or a budget/deadline abort; plus a `ForceTool` helper for `tool_choice`.
- Test: a fake `CompletionCreator` returns scripted `*openai.ChatCompletion` values; assert `Arguments` is validated before use (no unchecked assertion), each `ToolMessage` echoes the matching `tool_call_id`, the assistant turn precedes the tool messages, the loop stops on a non-tool finish reason, and `ForceTool` selects the named function.
- Verify: `go test -count=1 -race ./...`

Set up the module and add the SDK (note the `/v3` major-version path):

```bash
go mod edit -go=1.26
go get github.com/openai/openai-go/v3@latest
```

### Same machine, different envelope

Line the two providers up and the differences are all in the envelope:

- Definition: a tool is a *function* — `openai.ChatCompletionFunctionTool` wrapping
  a `FunctionDefinitionParam` whose `Parameters` is the JSON Schema. (Anthropic
  used a `ToolParam` with an `InputSchema`.)
- Request to call: the tool calls live in `completion.Choices[0].Message.ToolCalls`,
  and `finish_reason == "tool_calls"` signals them. (Anthropic used `stop_reason ==
  tool_use` and content blocks.)
- Arguments: `toolCall.Function.Arguments` is a **JSON string**, not a decoded
  object — you `json.Unmarshal` it yourself. This is the exact spot the SDK's own
  documentation warns "the model does not always generate valid JSON, and may
  hallucinate parameters ... Validate the arguments in your code," and the exact
  spot the anti-pattern `args["location"].(string)` would panic. The handler
  decodes into a typed struct and returns an error result instead.
- Result: a result is a `role=tool` message built with `openai.ToolMessage(content,
  toolCallID)` — content first, then the id it answers. (Anthropic used a
  `tool_result` block inside a user message.) OpenAI's tool message has no
  `is_error` field, so a tool error is conveyed as content text the model reads.

The loop body is otherwise identical: append the assistant turn first (via
`choice.Message.ToParam()`), then one result per call, bounded by `maxTurns` and a
context check. Because the dispatcher (`ToolBox`) is the same shape as the
Anthropic exercise, one registry could drive both providers behind a thin adapter
— which is the whole case for a provider-agnostic tool layer.

### tool_choice and its trade-off

`tool_choice` steers whether and which tool the model calls. The default is `auto`.
Forcing a specific function is done by setting `params.ToolChoice` to the union
returned by `openai.ToolChoiceOptionFunctionToolChoice`, which `ForceTool` wraps.
Forcing is useful for structured extraction where a tool *must* run, but it has a
cost the concepts named: a forced model cannot answer in prose and, if it has
nothing sensible to pass, will fabricate arguments to satisfy the required call.
Reach for it only when the control flow genuinely requires the tool.

Create `loop.go`:

```go
package openaitools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// ErrTurnBudget is returned when the loop exceeds its maximum number of turns.
var ErrTurnBudget = errors.New("tool loop exceeded turn budget")

// CompletionCreator is the single method of the OpenAI client the loop needs.
// The real *openai.ChatCompletionService (client.Chat.Completions) satisfies it,
// and tests pass a scripted fake, so the loop runs with no network and no key.
type CompletionCreator interface {
	New(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error)
}

// ToolBox is the same minimal dispatcher as the Anthropic exercise, bundled here
// so this module is self-contained and so ONE dispatcher shape drives both
// providers. A handler returns result content plus an is_error flag.
type ToolBox struct {
	handlers map[string]func(context.Context, json.RawMessage) (string, bool)
}

// NewToolBox returns an empty dispatcher.
func NewToolBox() *ToolBox {
	return &ToolBox{handlers: make(map[string]func(context.Context, json.RawMessage) (string, bool))}
}

// Register adds a handler under name.
func (b *ToolBox) Register(name string, fn func(context.Context, json.RawMessage) (string, bool)) {
	b.handlers[name] = fn
}

// Dispatch runs the named handler. An unknown tool or a handler failure comes
// back with isError true.
func (b *ToolBox) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	fn, ok := b.handlers[name]
	if !ok {
		return fmt.Sprintf("unknown tool %q", name), true
	}
	return fn(ctx, args)
}

// RunLoop drives the same state machine as the Anthropic loop, in OpenAI's wire
// shape: tool calls arrive in Message.ToolCalls with Arguments as a raw JSON
// string, results go back as role=tool messages keyed by tool_call_id, and
// finish_reason == "tool_calls" continues the loop. It is bounded by maxTurns and
// aborts if ctx is cancelled between turns.
func RunLoop(ctx context.Context, cc CompletionCreator, box *ToolBox, params openai.ChatCompletionNewParams, maxTurns int) (*openai.ChatCompletion, openai.ChatCompletionNewParams, error) {
	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, params, err
		}
		completion, err := cc.New(ctx, params)
		if err != nil {
			return nil, params, fmt.Errorf("chat.completions.new (turn %d): %w", turn, err)
		}
		choice := completion.Choices[0]

		// Invariant: append the assistant turn (which carries tool_calls) before
		// the role=tool messages that answer it.
		params.Messages = append(params.Messages, choice.Message.ToParam())

		if choice.FinishReason != "tool_calls" {
			return completion, params, nil // "stop", "length", ...: a final answer.
		}

		for _, tc := range choice.Message.ToolCalls {
			// Arguments is a JSON string the model produced. Decode and validate it
			// inside the handler; never assert on it unvalidated.
			content, isErr := box.Dispatch(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			_ = isErr // OpenAI role=tool messages have no is_error field; the error text in content is what the model reads.
			// ToolMessage takes content first, then the tool_call_id it answers.
			params.Messages = append(params.Messages, openai.ToolMessage(content, tc.ID))
		}
	}
	return nil, params, fmt.Errorf("after %d turns: %w", maxTurns, ErrTurnBudget)
}

// --- Tool definition, tool_choice helper, and a concrete handler ---

// WeatherArgs is the typed argument surface of get_weather.
type WeatherArgs struct {
	Location string `json:"location"`
	Unit     string `json:"unit"`
}

// WeatherTool is the function-tool definition. Description uses openai.String
// because FunctionDefinitionParam.Description is an optional value.
func WeatherTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
		Name:        "get_weather",
		Description: openai.String("Current weather for a city."),
		Parameters: openai.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"location": map[string]any{"type": "string", "description": "city and country, e.g. 'Paris, FR'"},
				"unit":     map[string]any{"type": "string", "enum": []string{"celsius", "fahrenheit"}},
			},
			"required": []string{"location"},
		},
	})
}

// ForceTool builds a tool_choice that forces the model to call exactly the named
// function. Use sparingly: a forced model cannot answer directly and may
// fabricate arguments to satisfy the required call.
func ForceTool(name string) openai.ChatCompletionToolChoiceOptionUnionParam {
	return openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: name})
}

// NewWeatherBox returns a dispatcher wired with the get_weather handler, which
// decodes the JSON-string arguments into a typed struct and validates them.
func NewWeatherBox() *ToolBox {
	box := NewToolBox()
	box.Register("get_weather", func(_ context.Context, raw json.RawMessage) (string, bool) {
		var a WeatherArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), true
		}
		if a.Location == "" {
			return "location must not be empty", true
		}
		return fmt.Sprintf(`{"location":%q,"temp_c":18,"summary":"overcast"}`, a.Location), false
	})
	return box
}
```

The live client mirrors the Anthropic one, kept out of the offline build.
`openai.NewClient` returns a `Client` value; `&client.Chat.Completions` is the
`*ChatCompletionService` that satisfies `CompletionCreator`.

Create `live.go`:

```go
//go:build llm

package openaitools

import "github.com/openai/openai-go/v3"

// LiveCreator wires the real OpenAI client. It is compiled only with -tags llm
// because it needs the network and an OPENAI_API_KEY. NewClient returns a value;
// &client.Chat.Completions is the *ChatCompletionService that satisfies
// CompletionCreator.
func LiveCreator() (CompletionCreator, error) {
	client := openai.NewClient()
	return &client.Chat.Completions, nil
}
```

### The runnable demo

The demo drives the real `RunLoop` with a scripted stand-in: turn one is a
`tool_calls` completion, turn two a final `stop`. As in the Anthropic exercise the
fake completions are built from wire JSON so the SDK's union reconstruction
behaves like a real response.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"example.com/openaitools"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// scripted stands in for the live client so the demo runs deterministically with
// no API key: turn one returns a tool call, turn two the final text. Swap in
// &client.Chat.Completions (see live.go, built with -tags llm) for real calls.
type scripted struct {
	turns []*openai.ChatCompletion
	i     int
}

func (s *scripted) New(_ context.Context, _ openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
	c := s.turns[s.i]
	s.i++
	return c, nil
}

func mustCompletion(raw string) *openai.ChatCompletion {
	var c openai.ChatCompletion
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		panic(err)
	}
	return &c
}

func main() {
	ctx := context.Background()

	model := &scripted{turns: []*openai.ChatCompletion{
		mustCompletion(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"call_01","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris, FR\"}"}}]}}]}`),
		mustCompletion(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"It is 18C and overcast in Paris."}}]}`),
	}}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModelGPT4o,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What is the weather in Paris?")},
		Tools:    []openai.ChatCompletionToolUnionParam{openaitools.WeatherTool()},
		Seed:     openai.Int(0),
	}

	completion, transcript, err := openaitools.RunLoop(ctx, model, openaitools.NewWeatherBox(), params, 8)
	if err != nil {
		fmt.Println("loop error:", err)
		return
	}

	fmt.Printf("messages in transcript: %d\n", len(transcript.Messages))
	fmt.Println("final answer:", completion.Choices[0].Message.Content)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
messages in transcript: 4
final answer: It is 18C and overcast in Paris.
```

The transcript holds four messages: the user question, the assistant `tool_calls`
turn, the `role=tool` result, and the assistant final text.

### Tests

The tests script the model with a fake `CompletionCreator`.
`TestRunLoopParallelToolCalls` returns two tool calls in one turn and asserts both
are dispatched, that each produced a `role=tool` message echoing the matching
`tool_call_id` (`call_a`, `call_b`), and that the assistant turn precedes them.
`TestRunLoopStopsOnNonToolFinish` confirms a `"stop"` finish reason ends the loop
after one call. `TestForceToolSelectsNamedFunction` checks that `ForceTool` builds
the correct forced-function `tool_choice`. `TestArgumentsValidatedNotAsserted`
feeds a hostile arguments string (`{"location":42}`) and asserts it becomes an
error result rather than a panic — the anti-pattern made a passing test.

Create `loop_test.go`:

```go
package openaitools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// fakeCreator returns scripted completions, one per call.
type fakeCreator struct {
	responses []*openai.ChatCompletion
	calls     int
}

func (f *fakeCreator) New(ctx context.Context, _ openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.calls >= len(f.responses) {
		return nil, errors.New("fake: no scripted response left")
	}
	c := f.responses[f.calls]
	f.calls++
	return c, nil
}

// completionFromJSON builds a fake completion from wire JSON. As with the
// Anthropic SDK, the OpenAI union types reconstruct from raw JSON, so scripting a
// fake by JSON (rather than setting flat fields) is what makes ToParam and the
// tool-call type switch behave like a real response.
func completionFromJSON(t *testing.T, raw string) *openai.ChatCompletion {
	t.Helper()
	var c openai.ChatCompletion
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("build fake completion: %v", err)
	}
	return &c
}

func toolCallsCompletion(t *testing.T, calls string) *openai.ChatCompletion {
	return completionFromJSON(t, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":`+
		`{"role":"assistant","content":"","tool_calls":[`+calls+`]}}]}`)
}

func stopCompletion(t *testing.T, text string) *openai.ChatCompletion {
	return completionFromJSON(t, `{"choices":[{"index":0,"finish_reason":"stop","message":`+
		`{"role":"assistant","content":"`+text+`"}}]}`)
}

func baseParams() openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model:    openai.ChatModelGPT4o,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("weather in Paris and Berlin?")},
		Tools:    []openai.ChatCompletionToolUnionParam{WeatherTool()},
		Seed:     openai.Int(0),
	}
}

func TestRunLoopParallelToolCalls(t *testing.T) {
	t.Parallel()
	turn1 := toolCallsCompletion(t,
		`{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris, FR\"}"}},`+
			`{"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Berlin, DE\"}"}}`)
	fake := &fakeCreator{responses: []*openai.ChatCompletion{turn1, stopCompletion(t, "done")}}

	completion, params, err := RunLoop(t.Context(), fake, NewWeatherBox(), baseParams(), 8)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("final finish_reason = %q, want stop", completion.Choices[0].FinishReason)
	}
	if fake.calls != 2 {
		t.Fatalf("model called %d times, want 2", fake.calls)
	}
	// Messages: user, assistant(tool_calls), tool(call_a), tool(call_b), assistant(text).
	if len(params.Messages) != 5 {
		t.Fatalf("transcript has %d messages, want 5", len(params.Messages))
	}
	if params.Messages[1].OfAssistant == nil {
		t.Error("message 1 should be the assistant tool_calls turn")
	}
	ids := toolCallIDs(params.Messages)
	if len(ids) != 2 || !ids["call_a"] || !ids["call_b"] {
		t.Fatalf("tool_call ids echoed = %v, want call_a and call_b", ids)
	}
}

func toolCallIDs(msgs []openai.ChatCompletionMessageParamUnion) map[string]bool {
	ids := make(map[string]bool)
	for _, m := range msgs {
		if tm := m.OfTool; tm != nil {
			ids[tm.ToolCallID] = true
		}
	}
	return ids
}

func TestRunLoopStopsOnNonToolFinish(t *testing.T) {
	t.Parallel()
	fake := &fakeCreator{responses: []*openai.ChatCompletion{stopCompletion(t, "hello")}}
	completion, _, err := RunLoop(t.Context(), fake, NewWeatherBox(), baseParams(), 8)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if completion.Choices[0].FinishReason != "stop" || fake.calls != 1 {
		t.Fatalf("finish=%q calls=%d, want stop and 1 call", completion.Choices[0].FinishReason, fake.calls)
	}
}

func TestRunLoopBudgetGuard(t *testing.T) {
	t.Parallel()
	always := func() *openai.ChatCompletion {
		return toolCallsCompletion(t, `{"id":"call_x","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"X\"}"}}`)
	}
	fake := &fakeCreator{responses: []*openai.ChatCompletion{always(), always(), always(), always()}}
	_, _, err := RunLoop(t.Context(), fake, NewWeatherBox(), baseParams(), 3)
	if !errors.Is(err, ErrTurnBudget) {
		t.Fatalf("err = %v, want wrapped ErrTurnBudget", err)
	}
	if fake.calls != 3 {
		t.Fatalf("model called %d times, want the 3-turn budget", fake.calls)
	}
}

func TestRunLoopContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fake := &fakeCreator{responses: []*openai.ChatCompletion{stopCompletion(t, "unreached")}}
	_, _, err := RunLoop(ctx, fake, NewWeatherBox(), baseParams(), 8)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.calls != 0 {
		t.Fatalf("model called %d times after cancel, want 0", fake.calls)
	}
}

func TestForceToolSelectsNamedFunction(t *testing.T) {
	t.Parallel()
	choice := ForceTool("get_weather")
	if choice.OfFunctionToolChoice == nil {
		t.Fatal("ForceTool did not set OfFunctionToolChoice")
	}
	if got := choice.OfFunctionToolChoice.Function.Name; got != "get_weather" {
		t.Fatalf("forced function = %q, want get_weather", got)
	}
}

func TestArgumentsValidatedNotAsserted(t *testing.T) {
	t.Parallel()
	// A hostile/malformed arguments string must not panic; it becomes an error result.
	content, isErr := NewWeatherBox().Dispatch(t.Context(), "get_weather", json.RawMessage(`{"location":42}`))
	if !isErr {
		t.Fatalf("numeric location: isErr = false, want true (content=%s)", content)
	}
}
```

## Review

The loop is correct when its transcript obeys the same invariant as the Anthropic
one in OpenAI's shape: the assistant `tool_calls` turn is appended before the
`role=tool` messages, and every `tool_call_id` is echoed by exactly one tool
message. `TestRunLoopParallelToolCalls` proves the id echo and the ordering;
`TestRunLoopStopsOnNonToolFinish` proves the loop ends on a non-tool
`finish_reason`. That the two providers are driven by the same `ToolBox` and the
same four-step body is the point — the differences are envelope, not logic.

The mistakes to avoid are OpenAI-specific traps on top of the shared ones. Do not
treat `Arguments` as decoded data: it is a JSON string, and
`TestArgumentsValidatedNotAsserted` exists to prove the handler decodes and
validates it rather than asserting on it and panicking. Do not swap the argument
order of `openai.ToolMessage` — it is content first, then the id. Do not forget
that `finish_reason`, not the presence of text, decides whether to loop. And use
`ForceTool` deliberately: forcing a tool removes the model's ability to answer
directly and can make it fabricate arguments. Run `go test -race` to keep the
loop and dispatcher race-free, because a production version runs the parallel
calls concurrently.

## Resources

- [`openai-go` package reference](https://pkg.go.dev/github.com/openai/openai-go/v3) — `ChatCompletionNewParams`, `ChatCompletionFunctionTool`, `ToolMessage`, `ChatCompletionMessage.ToParam`, and the tool-choice helpers.
- [OpenAI Go SDK — chat completion tool-calling example](https://github.com/openai/openai-go/tree/main/examples/chat-completion-tool-calling) — the compile-tested source this loop generalizes.
- [OpenAI docs — Function calling guide](https://platform.openai.com/docs/guides/function-calling) — `tool_calls`, `tool_call_id`, `finish_reason`, and `tool_choice` on the wire.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-anthropic-tool-loop.md](02-anthropic-tool-loop.md) | Next: [../04-embeddings-and-pgvector/00-concepts.md](../04-embeddings-and-pgvector/00-concepts.md)
