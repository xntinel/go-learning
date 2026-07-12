# Exercise 2: A Provider-Agnostic Streaming Client with Accumulation and Cancellation

The provider SDKs each give you a stream of deltas, but a gateway wants one shape
it can render, fold, and cancel regardless of which provider produced it. This
exercise builds that abstraction: a `Streamer` that yields incremental text deltas
as an `iter.Seq2[string, error]`, an accumulator that folds them into a final
result, and — behind a build tag — two concrete adapters for the Anthropic and
OpenAI Go SDKs.

This module is fully self-contained. The offline-testable core (the interface, the
accumulator, and an in-memory scripted streamer) has its own `go mod init`, code,
demo, and tests. The two real SDK adapters live behind a `//go:build llm` tag so
the package compiles and its tests run with no network and no API keys; you
compile-check the adapters with `go build -tags llm ./...` where the SDKs and
credentials are available.

## What you'll build

```text
streamclient/               independent module: example.com/streamclient
  go.mod                    go 1.26
  streamclient.go           Streamer, Summary, Usage, Result; Accumulate; ScriptedStreamer; sentinel ErrOverloaded
  anthropic.go              //go:build llm — AnthropicStreamer over client.Messages.NewStreaming
  openai.go                 //go:build llm — OpenAIStreamer over client.Chat.Completions.NewStreaming
  cmd/
    demo/
      main.go               fold a scripted stream into a Result and print it
  streamclient_test.go      fake-streamer tests: fold, mid-stream error, cancellation, Close-always
```

- Files: `streamclient.go`, `anthropic.go`, `openai.go`, `cmd/demo/main.go`, `streamclient_test.go`.
- Implement: a `Streamer` interface yielding `iter.Seq2[string, error]` plus a `Summary()`; an `Accumulate` that folds deltas into a `Result{Text, StopReason, Usage}` and surfaces errors; an in-memory `ScriptedStreamer`; and two SDK adapters behind `//go:build llm`.
- Test: drive `Accumulate` against the scripted fake and assert deltas fold to the correct text and summary, an injected mid-stream error surfaces through the iterator (wrapped sentinel, `errors.Is`), a cancelled context returns `context.Canceled`, and the stream's cleanup always runs.
- Verify: `go test -count=1 -race ./...` (offline core); `go build -tags llm ./...` where the SDKs are available.

Set up the module. The core needs no dependencies; add the SDKs only when you
compile the tagged adapters:

```bash
mkdir -p go-solutions/52-ai-llm-backends/02-streaming-completions/02-streaming-llm-client/cmd/demo
cd go-solutions/52-ai-llm-backends/02-streaming-completions/02-streaming-llm-client
go mod edit -go=1.26
# only needed to build the //go:build llm adapters:
go get github.com/anthropics/anthropic-sdk-go@latest
go get github.com/openai/openai-go@latest
```

### The abstraction: deltas as an iterator, metadata as a summary

The unit both providers agree on is a text delta: a small increment of the
assistant's message. So the core of the interface is an
`iter.Seq2[string, error]` — a range-over-func that yields each delta and, if the
stream fails, a terminal error. The error is a first-class value in the iterator,
not something swallowed: the concepts lesson's rule that an HTTP 200 is not
success shows up here as "the loop ending is not success; the error value is."

Deltas alone are not enough, though. You still need the final `stop_reason` and
the token `usage` for persistence and billing, and those arrive out of band — in
Anthropic's `message_delta`, in OpenAI's trailing usage chunk. So the `Streamer`
interface has two methods: `Stream` for the deltas and `Summary` for the folded
metadata, valid once iteration has completed cleanly. `Accumulate` ties them
together: it ranges over the deltas building the full text, and on a clean finish
reads the summary into a `Result`.

Modeling the delta stream and the summary separately is what keeps the abstraction
provider-agnostic and offline-testable. The fake implements the same two methods
with canned data; the real adapters implement them over the SDK. `Accumulate`
never knows which it is talking to.

Create `streamclient.go`:

```go
package streamclient

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
)

// ErrOverloaded models a recoverable, in-band provider error delivered mid-stream
// (for example Anthropic's overloaded_error). Handlers wrap it with %w so callers
// can assert it with errors.Is even though across a real wire it becomes text.
var ErrOverloaded = errors.New("provider overloaded")

// Usage is the token accounting for a completion, needed for billing attribution.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Summary is the out-of-band metadata a stream reports once it finishes: why it
// stopped and how many tokens it cost.
type Summary struct {
	StopReason string
	Usage      Usage
}

// Result is the folded outcome of a stream: the full text plus its summary.
type Result struct {
	Text       string
	StopReason string
	Usage      Usage
}

// Streamer is a provider-agnostic token source. Stream yields text deltas until
// the stream ends or errors; Summary returns the stop reason and usage and is
// valid only after Stream has completed without an error.
type Streamer interface {
	Stream(ctx context.Context) iter.Seq2[string, error]
	Summary() Summary
}

// Accumulate folds a Streamer's deltas into a Result. It concatenates the deltas
// while iterating (a gateway would also render each one live), and on a clean
// finish reads the summary. A delta error is returned immediately, carrying
// whatever text was accumulated before the failure.
func Accumulate(ctx context.Context, s Streamer) (Result, error) {
	var b strings.Builder
	for delta, err := range s.Stream(ctx) {
		if err != nil {
			return Result{Text: b.String()}, err
		}
		b.WriteString(delta)
	}
	sum := s.Summary()
	return Result{Text: b.String(), StopReason: sum.StopReason, Usage: sum.Usage}, nil
}

// ScriptedStreamer is an in-memory Streamer that replays canned deltas. It exists
// so the fold logic (and the re-streaming server in the next exercise) can be
// tested and demoed without a provider, a network, or an API key.
type ScriptedStreamer struct {
	deltas []string
	sum    Summary
	failAt int  // index at which to yield ErrOverloaded; negative disables
	closed bool // set when Stream's cleanup runs, mirroring stream.Close()
}

// Script builds a ScriptedStreamer that replays deltas with no injected failure.
func Script(deltas ...string) *ScriptedStreamer {
	return &ScriptedStreamer{deltas: deltas, failAt: -1}
}

// WithSummary sets the stop reason and usage the streamer reports on a clean finish.
func (s *ScriptedStreamer) WithSummary(sum Summary) *ScriptedStreamer {
	s.sum = sum
	return s
}

// FailAt injects a wrapped ErrOverloaded before the delta at index i, modeling a
// mid-stream provider error.
func (s *ScriptedStreamer) FailAt(i int) *ScriptedStreamer {
	s.failAt = i
	return s
}

// Closed reports whether Stream's cleanup ran, standing in for the deferred
// stream.Close() the real adapters must always call.
func (s *ScriptedStreamer) Closed() bool { return s.closed }

// Stream yields the canned deltas, honoring context cancellation between deltas
// and injecting the configured failure. The deferred close mirrors the real
// adapters' defer stream.Close().
func (s *ScriptedStreamer) Stream(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		defer func() { s.closed = true }()
		for i, d := range s.deltas {
			if ctx.Err() != nil {
				yield("", ctx.Err())
				return
			}
			if i == s.failAt {
				yield("", fmt.Errorf("scripted stream failed at %d: %w", i, ErrOverloaded))
				return
			}
			if !yield(d, nil) {
				return
			}
		}
	}
}

// Summary returns the configured stop reason and usage.
func (s *ScriptedStreamer) Summary() Summary { return s.sum }
```

### The real adapters, behind a build tag

The two SDK adapters implement the same interface. They are gated by
`//go:build llm` so the default build — and the gate — never tries to resolve the
SDK imports; you build them explicitly with `-tags llm` in an environment that has
the modules and, at runtime, the API keys.

The Anthropic adapter is the canonical streaming loop from the SDK. It calls
`client.Messages.NewStreaming(ctx, params)`, `defer`s `stream.Close()`, and for
each event calls `message.Accumulate(event)` to rebuild the whole message while
also switching on `event.AsAny()` to reach a `ContentBlockDeltaEvent`, then on
`event.Delta.AsAny()` to reach a `TextDelta` whose `.Text` is the increment it
yields. After the loop it checks `stream.Err()` — the mid-stream error path — and
only on success reads the folded `StopReason` and `Usage` off the accumulated
message. The `ctx` handed in is the request context, so a disconnected client
cancels upstream generation.

Create `anthropic.go`:

```go
//go:build llm

package streamclient

import (
	"context"
	"iter"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicStreamer adapts the Anthropic Go SDK to the Streamer interface.
type AnthropicStreamer struct {
	client anthropic.Client
	params anthropic.MessageNewParams
	sum    Summary
}

// NewAnthropicStreamer builds a streamer for the given API key and request params.
func NewAnthropicStreamer(apiKey string, params anthropic.MessageNewParams) *AnthropicStreamer {
	return &AnthropicStreamer{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
		params: params,
	}
}

// Stream yields text deltas from a streamed Messages call, accumulating the full
// message so Summary can report the stop reason and usage afterward.
func (a *AnthropicStreamer) Stream(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		stream := a.client.Messages.NewStreaming(ctx, a.params)
		defer stream.Close()

		msg := anthropic.Message{}
		for stream.Next() {
			event := stream.Current()
			if err := msg.Accumulate(event); err != nil {
				yield("", err)
				return
			}
			switch ev := event.AsAny().(type) {
			case anthropic.ContentBlockDeltaEvent:
				switch delta := ev.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					if !yield(delta.Text, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield("", err)
			return
		}
		a.sum = Summary{
			StopReason: string(msg.StopReason),
			Usage: Usage{
				InputTokens:  int(msg.Usage.InputTokens),
				OutputTokens: int(msg.Usage.OutputTokens),
			},
		}
	}
}

// Summary returns the folded stop reason and usage from the accumulated message.
func (a *AnthropicStreamer) Summary() Summary { return a.sum }
```

The OpenAI adapter follows the same shape with that provider's model. It requests
usage explicitly — `stream_options.include_usage` via
`ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}` — because
OpenAI omits usage otherwise and delivers it only in a trailing chunk with an
empty `choices` array. It folds chunks with a `ChatCompletionAccumulator` and
yields `chunk.Choices[0].Delta.Content` when present.

Create `openai.go`:

```go
//go:build llm

package streamclient

import (
	"context"
	"iter"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// OpenAIStreamer adapts the OpenAI Go SDK to the Streamer interface.
type OpenAIStreamer struct {
	client openai.Client
	params openai.ChatCompletionNewParams
	sum    Summary
}

// NewOpenAIStreamer builds a streamer for the given API key and request params,
// forcing include_usage so the trailing chunk carries token counts.
func NewOpenAIStreamer(apiKey string, params openai.ChatCompletionNewParams) *OpenAIStreamer {
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	return &OpenAIStreamer{
		client: openai.NewClient(option.WithAPIKey(apiKey)),
		params: params,
	}
}

// Stream yields content deltas from a streamed Chat Completions call, folding
// chunks with an accumulator so Summary can report the finish reason and usage.
func (o *OpenAIStreamer) Stream(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		stream := o.client.Chat.Completions.NewStreaming(ctx, o.params)
		defer stream.Close()

		acc := openai.ChatCompletionAccumulator{}
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				if !yield(chunk.Choices[0].Delta.Content, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield("", err)
			return
		}
		sum := Summary{Usage: Usage{
			InputTokens:  int(acc.Usage.PromptTokens),
			OutputTokens: int(acc.Usage.CompletionTokens),
		}}
		if len(acc.Choices) > 0 {
			sum.StopReason = string(acc.Choices[0].FinishReason)
		}
		o.sum = sum
	}
}

// Summary returns the folded finish reason and usage from the accumulator.
func (o *OpenAIStreamer) Summary() Summary { return o.sum }
```

### The runnable demo

The demo folds a scripted stream, which runs offline. It prints the reconstructed
text and the summary the streamer reported.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/streamclient"
)

func main() {
	s := streamclient.Script("The ", "quick ", "brown ", "fox").
		WithSummary(streamclient.Summary{
			StopReason: "end_turn",
			Usage:      streamclient.Usage{InputTokens: 9, OutputTokens: 4},
		})

	res, err := streamclient.Accumulate(context.Background(), s)
	if err != nil {
		log.Fatalf("accumulate: %v", err)
	}
	fmt.Printf("text: %q\n", res.Text)
	fmt.Printf("stop: %s\n", res.StopReason)
	fmt.Printf("usage: in=%d out=%d\n", res.Usage.InputTokens, res.Usage.OutputTokens)
	fmt.Printf("closed: %v\n", s.Closed())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
text: "The quick brown fox"
stop: end_turn
usage: in=9 out=4
closed: true
```

### Tests

The tests drive `Accumulate` against the scripted fake and never touch the
network. The happy path asserts the deltas fold to the full text and the summary
comes through. The error path injects a wrapped `ErrOverloaded` mid-stream and
asserts it surfaces through the iterator — `errors.Is` finds the sentinel and the
partial text stops at the failure point, proving the error is not swallowed. The
cancellation path passes an already-cancelled context and asserts `Accumulate`
returns `context.Canceled`. Every case also asserts `Closed()` — the fake's stand-in
for the deferred `stream.Close()` that a real adapter must always run.

Create `streamclient_test.go`:

```go
package streamclient

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAccumulateFolds(t *testing.T) {
	t.Parallel()
	s := Script("Hello", ", ", "world").WithSummary(Summary{
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 5, OutputTokens: 3},
	})
	res, err := Accumulate(t.Context(), s)
	if err != nil {
		t.Fatalf("Accumulate: %v", err)
	}
	if res.Text != "Hello, world" {
		t.Errorf("Text = %q, want %q", res.Text, "Hello, world")
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
	if res.Usage != (Usage{InputTokens: 5, OutputTokens: 3}) {
		t.Errorf("Usage = %+v, want in=5 out=3", res.Usage)
	}
	if !s.Closed() {
		t.Error("stream was not closed after a clean finish")
	}
}

func TestAccumulateMidStreamError(t *testing.T) {
	t.Parallel()
	s := Script("part-one", "part-two", "never").FailAt(2)
	res, err := Accumulate(t.Context(), s)
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("err = %v, want wrapped ErrOverloaded", err)
	}
	if res.Text != "part-onepart-two" {
		t.Errorf("Text = %q, want the two deltas before the failure", res.Text)
	}
	if !s.Closed() {
		t.Error("stream was not closed after a mid-stream error")
	}
}

func TestAccumulateCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the first delta

	s := Script("a", "b", "c")
	res, err := Accumulate(ctx, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty on immediate cancellation", res.Text)
	}
	if !s.Closed() {
		t.Error("stream was not closed after cancellation")
	}
}

func Example() {
	s := Script("stream", "ed").WithSummary(Summary{StopReason: "end_turn"})
	res, _ := Accumulate(context.Background(), s)
	fmt.Printf("%q %s\n", res.Text, res.StopReason)
	// Output: "streamed" end_turn
}
```

## Review

The client is correct when `Accumulate` is a faithful fold: text is the exact
concatenation of the deltas up to the point iteration stopped, and the summary is
read only on a clean finish. `TestAccumulateFolds` pins the happy path;
`TestAccumulateMidStreamError` is the important one, because it proves the error
is a value that flows out through the iterator rather than being dropped — if you
ever `continue` past a delta error, this test fails, and in production an
`overloaded_error` would masquerade as a short answer.
`TestAccumulateCancellation` proves the context reaches the source; a fake that
ignored `ctx` would fold all three deltas and fail the empty-text assertion.

The mistakes to avoid are the lifecycle ones. Always `defer stream.Close()` in the
real adapters — `Closed()` is the fake's proxy for that, asserted on every path,
because an unclosed stream leaks the connection and keeps the provider billing.
Always check `stream.Err()` after the loop rather than trusting that `Next()`
returning false means success. And keep the SDK adapters behind `//go:build llm`
so the offline core stays buildable and testable; the CI step that matters for
them is `go build -tags llm ./...`, not a unit test that would need a live key.

## Resources

- [anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) — `Messages.NewStreaming`, `Message.Accumulate`, `event.AsAny()`, and the `ContentBlockDeltaEvent`/`TextDelta` union shape.
- [openai-go](https://pkg.go.dev/github.com/openai/openai-go) — `Chat.Completions.NewStreaming`, `ChatCompletionAccumulator`, `ChatCompletionChunk`, and `StreamOptions`/`IncludeUsage`.
- [`iter` package](https://pkg.go.dev/iter) — `Seq2` and the range-over-func contract the `Streamer` returns.
- [Go blog — Range Over Function Types](https://go.dev/blog/range-functions) — how push iterators like `iter.Seq2` compose with `for range`.

---

Back to [01-sse-wire-decoder.md](01-sse-wire-decoder.md) | Next: [03-sse-stream-proxy.md](03-sse-stream-proxy.md)
