# Exercise 2: The Agentic Tool-Use Loop with the Anthropic Go SDK

The registry from Exercise 1 dispatches a call; this exercise wires it into the
multi-turn loop that a real agent runs against Claude. The loop is the state
machine you own: request, see `stop_reason == tool_use`, execute every tool block,
feed the results back, and repeat until the model returns a final text turn. You
build it so it is bounded, cancellable, and fully testable offline.

This module is self-contained: its own `go mod init`, a bundled minimal
dispatcher, and its own demo and tests. The live client is isolated behind a
build tag so the loop logic and its tests run with no network and no API key.

## What you'll build

```text
anthropictools/             independent module: example.com/anthropictools
  go.mod                    go 1.26; requires github.com/anthropics/anthropic-sdk-go
  loop.go                   MessageCreator interface; RunLoop state machine; ToolBox; WeatherTool
  live.go                   //go:build llm — wires the real anthropic.Client
  cmd/
    demo/
      main.go               drives RunLoop with a scripted stand-in model (no key needed)
  loop_test.go              fake MessageCreator: parallel calls, id matching, ordering, budget, cancel
```

- Files: `loop.go`, `live.go`, `cmd/demo/main.go`, `loop_test.go`.
- Implement: a `RunLoop` that sends messages plus tool definitions, appends the assistant turn, and — while `StopReason` is `tool_use` — executes every `tool_use` block through a `ToolBox`, builds one `tool_result` per id, appends the user results turn, and repeats until a non-tool stop reason or a turn-budget/ context-deadline abort.
- Test: a fake `MessageCreator` returns scripted `*anthropic.Message` values; assert both parallel tools dispatch, each `tool_result` carries the right `ToolUseID`, the assistant turn precedes the results turn, the budget guard fires, and a cancelled context aborts before any call.
- Verify: `go test -count=1 -race ./...`

Set up the module and add the SDK:

```bash
mkdir -p ~/go-exercises/anthropictools/cmd/demo
cd ~/go-exercises/anthropictools
go mod init example.com/anthropictools
go mod edit -go=1.26
go get github.com/anthropics/anthropic-sdk-go@latest
```

### The loop is the whole lesson

Everything the concepts said about the state machine becomes concrete here. One
iteration does four things, in this order, and the order is not negotiable:

1. Call `Messages.New` with the running message list and the tool definitions.
2. Append `msg.ToParam()` — the assistant turn — to the message list immediately.
   This is the invariant: the turn that *requested* the tools must be recorded
   before the turn that *answers* them, or the next request is a 400.
3. If `msg.StopReason` is not `StopReasonToolUse`, the model produced a final
   answer (`end_turn`, `max_tokens`, ...); return it.
4. Otherwise iterate `msg.Content`, execute each `tool_use` block through the
   registry, collect exactly one `tool_result` per block id, append them as a
   single user turn, and loop.

The `for turn := 0; turn < maxTurns; turn++` header is the budget guard: a model
that keeps asking for tools cannot loop forever, because after `maxTurns` the
function returns `ErrTurnBudget`. The `ctx.Err()` check at the top of each
iteration is the deadline/cancellation guard: if the caller's context is already
done, the loop returns before spending another request.

### The opaque bit: how the SDK reconstructs content blocks

One mechanism will bite you if it stays hidden. The response `msg.Content` is a
slice of `anthropic.ContentBlockUnion` — a flattened union that can be a text
block, a tool-use block, a thinking block, and more. You discriminate with
`block.AsAny()`, which returns an `any` you type-switch on. The subtlety is that
`AsAny()` (and the `AsToolUse()` it calls) does not read the union's flat fields;
it *re-unmarshals the block's stored raw JSON* into the concrete type. For a real
response from the API that raw JSON is present, so `AsAny()` returns a fully
populated `ToolUseBlock`. But it means that in tests you cannot fabricate a block
by setting `ContentBlockUnion{Type: "tool_use", ID: "..."}` field-by-field — those
flat fields are ignored by `AsAny()`, and you would get back an empty block. The
correct way to script a fake response is to `json.Unmarshal` wire JSON into an
`anthropic.Message`, which populates the raw metadata the reconstruction reads.
Both the demo and the tests build their fake turns from JSON for exactly this
reason.

### Testability: the client behind an interface

`RunLoop` never mentions the concrete client. It takes a `MessageCreator`, an
interface with the single method the loop uses, whose signature matches
`*anthropic.MessageService.New` exactly — including the variadic
`option.RequestOption`. The real client satisfies it (`&client.Messages`), and a
test passes a fake that returns scripted messages. That is what lets the entire
state machine — parallel calls, id matching, ordering, the budget, cancellation —
be verified deterministically with no network and no key. The live wiring lives
in `live.go` behind `//go:build llm`, so it is compiled only when you ask for it.

Create `loop.go`:

```go
package anthropictools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ErrTurnBudget is returned when the loop exceeds its maximum number of turns,
// which is the guard against a model that calls tools forever.
var ErrTurnBudget = errors.New("tool loop exceeded turn budget")

// MessageCreator is the single method of the Anthropic client the loop needs.
// The real *anthropic.MessageService (client.Messages) satisfies it, and tests
// pass a scripted fake, so the loop is exercised with no network and no API key.
type MessageCreator interface {
	New(ctx context.Context, params anthropic.MessageNewParams, opts ...option.RequestOption) (*anthropic.Message, error)
}

// ToolBox is a minimal provider-neutral dispatcher, bundled so this module is
// self-contained. A handler returns the result content and an is_error flag,
// which is exactly what a tool_result block carries.
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
// back with isError true so the model can read it as a recoverable tool_result.
func (b *ToolBox) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	fn, ok := b.handlers[name]
	if !ok {
		return fmt.Sprintf("unknown tool %q", name), true
	}
	return fn(ctx, args)
}

// RunLoop drives the agentic tool-use state machine: request, inspect the stop
// reason, execute every tool_use block, feed the results back, repeat. It returns
// the final assistant message and the full transcript. The loop is bounded by
// maxTurns and aborts if ctx is cancelled between turns.
func RunLoop(ctx context.Context, mc MessageCreator, tools []anthropic.ToolUnionParam, box *ToolBox, messages []anthropic.MessageParam, maxTurns int) (*anthropic.Message, []anthropic.MessageParam, error) {
	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, messages, err
		}
		msg, err := mc.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeSonnet5,
			MaxTokens: 1024,
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			return nil, messages, fmt.Errorf("messages.new (turn %d): %w", turn, err)
		}

		// Invariant: append the assistant turn that requested the tools BEFORE the
		// tool_result turn that answers it, or the next request is a 400.
		messages = append(messages, msg.ToParam())

		if msg.StopReason != anthropic.StopReasonToolUse {
			return msg, messages, nil // end_turn, max_tokens, ...: a final answer.
		}

		// Collect exactly one result per tool_use block. Text or thinking blocks in
		// the same turn are skipped here; only tool_use blocks need an answer.
		var results []anthropic.ContentBlockParamUnion
		for _, block := range msg.Content {
			tu, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			content, isErr := box.Dispatch(ctx, tu.Name, tu.Input)
			results = append(results, anthropic.NewToolResultBlock(tu.ID, content, isErr))
		}
		if len(results) == 0 {
			return msg, messages, nil // stop_reason said tool_use but no blocks: nothing to answer.
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}
	return nil, messages, fmt.Errorf("after %d turns: %w", maxTurns, ErrTurnBudget)
}

// --- A concrete tool, so the module has something to dispatch ---

// WeatherArgs is the typed argument surface of get_weather.
type WeatherArgs struct {
	Location string `json:"location"`
	Unit     string `json:"unit"`
}

// WeatherTool is the tool definition sent to the model. Description uses the
// anthropic.String helper because ToolParam.Description is an optional value.
func WeatherTool() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        "get_weather",
		Description: anthropic.String("Current weather for a city."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"location": map[string]any{"type": "string", "description": "city and country, e.g. 'Paris, FR'"},
				"unit":     map[string]any{"type": "string", "enum": []string{"celsius", "fahrenheit"}},
			},
			Required: []string{"location"},
		},
	}}
}

// NewWeatherBox returns a dispatcher wired with the get_weather handler. The
// handler decodes into a typed struct and validates; a bad argument becomes an
// is_error result the model can correct, never a panic.
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

The live client is a few lines, kept out of the offline build. `anthropic.NewClient`
returns a `Client` value; `&client.Messages` is the `*MessageService` that
satisfies `MessageCreator`.

Create `live.go`:

```go
//go:build llm

package anthropictools

import "github.com/anthropics/anthropic-sdk-go"

// LiveCreator wires the real Anthropic client. It is compiled only with
// -tags llm because it needs the network and an ANTHROPIC_API_KEY. NewClient
// returns a value; &client.Messages is the *MessageService that satisfies
// MessageCreator.
func LiveCreator() (MessageCreator, error) {
	client := anthropic.NewClient()
	return &client.Messages, nil
}
```

### The runnable demo

The demo drives the real `RunLoop` with a scripted stand-in for the model, so it
runs deterministically with no API key: turn one returns a `tool_use` for the
weather, turn two returns the final text. Swapping in `LiveCreator()` (built with
`-tags llm`) is the only change needed to talk to Claude. Note the fake turns are
built from wire JSON, per the reconstruction mechanism above.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"example.com/anthropictools"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// scripted stands in for the live client so the demo runs deterministically with
// no API key. It returns a tool_use turn, then a final text turn. Swap in
// &client.Messages (see live.go, built with -tags llm) for real calls. The turns
// are built from wire JSON because the SDK reconstructs typed content blocks from
// each block's raw JSON.
type scripted struct {
	turns []*anthropic.Message
	i     int
}

func (s *scripted) New(_ context.Context, _ anthropic.MessageNewParams, _ ...option.RequestOption) (*anthropic.Message, error) {
	m := s.turns[s.i]
	s.i++
	return m, nil
}

func mustMessage(raw string) *anthropic.Message {
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return &m
}

func main() {
	ctx := context.Background()

	model := &scripted{turns: []*anthropic.Message{
		mustMessage(`{"role":"assistant","stop_reason":"tool_use","content":[` +
			`{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"location":"Paris, FR","unit":"celsius"}}]}`),
		mustMessage(`{"role":"assistant","stop_reason":"end_turn","content":[` +
			`{"type":"text","text":"It is 18C and overcast in Paris."}]}`),
	}}

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("What is the weather in Paris?")),
	}

	final, transcript, err := anthropictools.RunLoop(ctx, model,
		[]anthropic.ToolUnionParam{anthropictools.WeatherTool()},
		anthropictools.NewWeatherBox(), messages, 8)
	if err != nil {
		fmt.Println("loop error:", err)
		return
	}

	fmt.Printf("turns in transcript: %d\n", len(transcript))
	for _, block := range final.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			fmt.Println("final answer:", tb.Text)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
turns in transcript: 4
final answer: It is 18C and overcast in Paris.
```

The transcript holds four turns: the initial user question, the assistant
`tool_use`, the user `tool_result`, and the assistant final text — the exact
shape the API requires.

### Tests

The tests script the model with a fake `MessageCreator` and assert the state
machine's invariants. `TestRunLoopParallelToolCalls` returns two `tool_use` blocks
in one turn and checks that both are dispatched, that the resulting user turn
carries a `tool_result` for each id (`toolu_a` and `toolu_b`), and that the
assistant turn is appended before the results turn. `TestRunLoopBudgetGuard`
scripts a model that always asks for a tool and asserts `RunLoop` stops after
exactly the turn budget with `ErrTurnBudget`. `TestRunLoopContextCancelled` cancels
the context up front and asserts the loop returns `context.Canceled` without ever
calling the model. Every fake message is built from JSON so `AsAny()` reconstructs
real blocks.

Create `loop_test.go`:

```go
package anthropictools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// fakeCreator returns a scripted sequence of messages, one per call, and records
// the params it was called with so tests can assert on the transcript.
type fakeCreator struct {
	responses []*anthropic.Message
	calls     int
	lastMsgs  []anthropic.MessageParam
}

func (f *fakeCreator) New(ctx context.Context, params anthropic.MessageNewParams, _ ...option.RequestOption) (*anthropic.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.lastMsgs = params.Messages
	if f.calls >= len(f.responses) {
		return nil, errors.New("fake: no scripted response left")
	}
	m := f.responses[f.calls]
	f.calls++
	return m, nil
}

// msgFromJSON builds a fake response by unmarshaling wire JSON into a Message.
// This is the correct way to script a fake: the SDK reconstructs typed content
// blocks (via AsAny) from each block's raw JSON, so hand-setting only the flat
// struct fields would produce empty blocks. Building from JSON populates that raw.
func msgFromJSON(t *testing.T, raw string) *anthropic.Message {
	t.Helper()
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("build fake message: %v", err)
	}
	return &m
}

func toolUseMsg(t *testing.T, id, name, args string) *anthropic.Message {
	return msgFromJSON(t, `{"role":"assistant","stop_reason":"tool_use","content":[`+
		`{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":`+args+`}]}`)
}

func textMsg(t *testing.T, text string) *anthropic.Message {
	return msgFromJSON(t, `{"role":"assistant","stop_reason":"end_turn","content":[`+
		`{"type":"text","text":"`+text+`"}]}`)
}

func TestRunLoopParallelToolCalls(t *testing.T) {
	t.Parallel()
	// Turn 1: two tool_use blocks in one assistant turn. Turn 2: a text answer.
	turn1 := msgFromJSON(t, `{"role":"assistant","stop_reason":"tool_use","content":[`+
		`{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{"location":"Paris, FR"}},`+
		`{"type":"tool_use","id":"toolu_b","name":"get_weather","input":{"location":"Berlin, DE"}}]}`)
	fake := &fakeCreator{responses: []*anthropic.Message{turn1, textMsg(t, "done")}}

	final, msgs, err := RunLoop(t.Context(), fake, []anthropic.ToolUnionParam{WeatherTool()}, NewWeatherBox(), nil, 8)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if final.StopReason != anthropic.StopReasonEndTurn {
		t.Fatalf("final StopReason = %v, want end_turn", final.StopReason)
	}
	if fake.calls != 2 {
		t.Fatalf("model called %d times, want 2", fake.calls)
	}
	// Transcript: assistant(tool_use) -> user(2 tool_result) -> assistant(text).
	if len(msgs) != 3 {
		t.Fatalf("transcript has %d turns, want 3", len(msgs))
	}
	if msgs[0].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("turn 0 role = %v, want assistant", msgs[0].Role)
	}
	if msgs[1].Role != anthropic.MessageParamRoleUser {
		t.Errorf("turn 1 role = %v, want user (the tool_result turn)", msgs[1].Role)
	}
	// The user turn must carry exactly one tool_result per tool_use id.
	ids := toolResultIDs(t, msgs[1])
	if len(ids) != 2 || !ids["toolu_a"] || !ids["toolu_b"] {
		t.Fatalf("tool_result ids = %v, want toolu_a and toolu_b", ids)
	}
}

func toolResultIDs(t *testing.T, m anthropic.MessageParam) map[string]bool {
	t.Helper()
	ids := make(map[string]bool)
	for _, block := range m.Content {
		if tr := block.OfToolResult; tr != nil {
			ids[tr.ToolUseID] = true
		}
	}
	return ids
}

func TestRunLoopBudgetGuard(t *testing.T) {
	t.Parallel()
	// A model that always asks for a tool must be stopped by the turn budget.
	always := func() *anthropic.Message { return toolUseMsg(t, "toolu_x", "get_weather", `{"location":"X"}`) }
	fake := &fakeCreator{responses: []*anthropic.Message{always(), always(), always(), always()}}

	_, _, err := RunLoop(t.Context(), fake, []anthropic.ToolUnionParam{WeatherTool()}, NewWeatherBox(), nil, 3)
	if !errors.Is(err, ErrTurnBudget) {
		t.Fatalf("err = %v, want wrapped ErrTurnBudget", err)
	}
	if fake.calls != 3 {
		t.Fatalf("model called %d times, want exactly the 3-turn budget", fake.calls)
	}
}

func TestRunLoopContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fake := &fakeCreator{responses: []*anthropic.Message{textMsg(t, "unreached")}}

	_, _, err := RunLoop(ctx, fake, []anthropic.ToolUnionParam{WeatherTool()}, NewWeatherBox(), nil, 8)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.calls != 0 {
		t.Fatalf("model called %d times after cancel, want 0", fake.calls)
	}
}

func TestToolBoxErrorResult(t *testing.T) {
	t.Parallel()
	box := NewWeatherBox()
	content, isErr := box.Dispatch(t.Context(), "get_weather", json.RawMessage(`{"location":""}`))
	if !isErr {
		t.Fatalf("empty location: isErr = false, want true (content=%s)", content)
	}
	if _, isErr := box.Dispatch(t.Context(), "nope", json.RawMessage(`{}`)); !isErr {
		t.Fatal("unknown tool: isErr = false, want true")
	}
}
```

## Review

The loop is correct when the transcript it produces obeys the conversation-shape
invariant: an assistant `tool_use` turn is always immediately followed by a user
turn carrying one `tool_result` per requested id, and the loop terminates the
instant `StopReason` is anything other than `tool_use`. `TestRunLoopParallelToolCalls`
proves both halves — the id-to-result mapping and the assistant-before-results
ordering — and would fail if you appended the results before the assistant turn or
dropped one of the parallel calls. `TestRunLoopBudgetGuard` proves the loop is
bounded, which is the difference between a request and a runaway bill.

The mistakes to avoid are the ones baked into the tests. Do not build fake
messages by setting `ContentBlockUnion` fields directly — `AsAny()` reconstructs
from raw JSON, so a hand-built union comes back empty; build from JSON. Do not
skip appending `msg.ToParam()`, and never append it *after* the results. Do not
let a bad tool argument panic the loop: the `ToolBox` handler decodes into a typed
struct and returns an `is_error` result, exactly the recoverable path the model
needs. And keep the `ctx.Err()` check and the `maxTurns` bound — without them a
stuck model runs until something else times out. Run `go test -race`; the loop and
its dispatcher must stay race-free because a production version executes parallel
tool calls concurrently.

## Resources

- [`anthropic-sdk-go` package reference](https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go) — `MessageNewParams`, `ToolParam`, `ToolUnionParam`, `NewToolResultBlock`, `NewUserMessage`, and `Message.ToParam`.
- [Anthropic Go SDK — tool use example](https://github.com/anthropics/anthropic-sdk-go/tree/main/examples/tools) — the compile-tested source this loop generalizes.
- [Anthropic docs — Tool use overview](https://docs.claude.com/en/docs/agents-and-tools/tool-use/overview) — `stop_reason`, `tool_use`/`tool_result` blocks, and parallel tool calls on the wire.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-tool-registry-and-schema.md](01-tool-registry-and-schema.md) | Next: [03-openai-tool-loop-and-tool-choice.md](03-openai-tool-loop-and-tool-choice.md)
