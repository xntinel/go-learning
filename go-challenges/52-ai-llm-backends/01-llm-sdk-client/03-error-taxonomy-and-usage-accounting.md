# Exercise 3: Mapping provider errors to a common taxonomy and reading token usage

You will build a shared error taxonomy that both adapters map SDK errors into via
`errors.As`, plus token-usage extraction and a cost estimate read on every
response, plus truncation detection from each vendor's stop reason. The tests
drive canned 401/429/500 and success bodies through `httptest`, with retries
disabled so failures surface deterministically.

## What you'll build

```text
llmerr/                           independent module: example.com/llmerr
  go.mod                          go 1.26; requires the Anthropic + OpenAI SDKs
  taxonomy.go                     sentinels + classifyAnthropic / classifyOpenAI (errors.As)
  usage.go                        Usage, Pricing.Cost, Truncated (stop-reason check)
  anthropic.go                    CompleteAnthropic -> (CallResult, classified error)
  openai.go                       CompleteOpenAI -> (CallResult, classified error)
  cmd/
    demo/
      main.go                     success (usage+cost+truncation) and a 429 mapping
  llmerr_test.go                  status->sentinel table; usage/cost; truncation; Example
```

Files: `taxonomy.go`, `usage.go`, `anthropic.go`, `openai.go`, `cmd/demo/main.go`, `llmerr_test.go`.
Implement: a shared taxonomy (auth, rate-limited, bad-request, transient, timeout) mapped from each vendor's error via `errors.As` + `StatusCode`; usage extraction and a `Pricing.Cost`; and `Truncated` detecting the max-tokens cutoff from either vendor's stop reason.
Test: `httptest` handlers returning 401, 429, 500, and a 200 with usage; assert each maps to the right sentinel with `errors.Is`; assert usage/cost on success; assert truncation when `stop_reason`/`finish_reason` signals a cutoff. Use `WithMaxRetries(0)`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/llmerr/cmd/demo
cd ~/go-exercises/llmerr
go mod init example.com/llmerr
go get github.com/anthropics/anthropic-sdk-go@latest
go get github.com/openai/openai-go/v3@latest
```

### The shared taxonomy

The domain should react to *kinds* of failure, not to a vendor's error struct. So
the taxonomy is a small set of sentinels, and each adapter has a classifier that
unwraps its vendor error with `errors.As`, switches on `StatusCode`, and wraps the
matching sentinel with `%w`. The retryable/non-retryable distinction lives in the
status ranges: 401/403 (auth) and 400/404/422 (bad request) are non-retryable and
should surface immediately; 429 (rate-limited) and 5xx (transient) are the ones a
caller may back off and retry. A context deadline is mapped to a timeout sentinel
before the vendor unwrap, because a cancelled request never produces a vendor
`*Error`. The Anthropic classifier also threads through `RequestID`, which is what
you quote when you open a support ticket; the OpenAI error carries `StatusCode`
and `Message`.

Create `taxonomy.go`:

```go
package llmerr

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
)

// Taxonomy sentinels. Wrap with %w; callers classify with errors.Is.
var (
	ErrAuth        = errors.New("authentication failed")    // 401/403 — non-retryable
	ErrRateLimited = errors.New("rate limited")             // 429 — retryable with backoff
	ErrBadRequest  = errors.New("bad request")              // 400/404/422 — non-retryable
	ErrTransient   = errors.New("transient upstream error") // 5xx — retryable
	ErrTimeout     = errors.New("deadline exceeded")        // context cancellation
)

// sentinelForStatus maps an HTTP status to a taxonomy sentinel.
func sentinelForStatus(status int) (error, bool) {
	switch {
	case status == 401 || status == 403:
		return ErrAuth, true
	case status == 429:
		return ErrRateLimited, true
	case status == 400 || status == 404 || status == 422:
		return ErrBadRequest, true
	case status >= 500:
		return ErrTransient, true
	}
	return nil, false
}

func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// classifyAnthropic maps an Anthropic SDK error into the shared taxonomy.
func classifyAnthropic(err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(err) {
		return fmt.Errorf("anthropic: %w", ErrTimeout)
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if sentinel, ok := sentinelForStatus(apiErr.StatusCode); ok {
			return fmt.Errorf("anthropic: status %d request %s: %w",
				apiErr.StatusCode, apiErr.RequestID, sentinel)
		}
	}
	return err // unknown shape: surface as-is rather than mislabel
}

// classifyOpenAI maps an OpenAI SDK error into the shared taxonomy.
func classifyOpenAI(err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(err) {
		return fmt.Errorf("openai: %w", ErrTimeout)
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		if sentinel, ok := sentinelForStatus(apiErr.StatusCode); ok {
			return fmt.Errorf("openai: status %d: %w", apiErr.StatusCode, sentinel)
		}
	}
	return err
}
```

### Usage, cost, and truncation

Usage is read on every response, not as an afterthought, because per-call cost
cannot be reconstructed later. `Usage` is the neutral shape; `Pricing.Cost`
turns it into dollars using per-million-token rates. `Truncated` inspects the raw
stop reason: Anthropic sets `max_tokens` and OpenAI sets `length` when the model
was cut off, and either means the output is incomplete — a signal to raise the cap
or treat the result as an error, never a finished answer.

Create `usage.go`:

```go
package llmerr

// Usage is neutral token accounting, read on every response for chargeback.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Pricing holds per-million-token rates for a model.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Cost estimates the dollar cost of a single call.
func (p Pricing) Cost(u Usage) float64 {
	return float64(u.InputTokens)/1e6*p.InputPerMTok +
		float64(u.OutputTokens)/1e6*p.OutputPerMTok
}

// Truncated reports whether a raw stop/finish reason indicates the model hit the
// output cap. Anthropic uses "max_tokens"; OpenAI uses "length".
func Truncated(stopReason string) bool {
	return stopReason == "max_tokens" || stopReason == "length"
}
```

### The adapters return usage and a classified error

Each completion returns a `CallResult` carrying the text, usage, and raw stop
reason, plus an error already mapped through the taxonomy. `WithMaxRetries` is
plumbed explicitly so tests can disable retries (a stubbed 500 surfaces
immediately instead of being retried into a longer wait) — and so the concepts
file's timeout math is visible: with retries enabled, the wall-clock ceiling is
roughly the request timeout times `retries + 1`.

Create `anthropic.go`:

```go
package llmerr

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// CallResult is the neutral outcome of one completion.
type CallResult struct {
	Text       string
	Usage      Usage
	StopReason string
}

// CompleteAnthropic runs one completion, returning usage and a classified error.
func CompleteAnthropic(ctx context.Context, opts []option.RequestOption, model, prompt string) (CallResult, error) {
	client := anthropic.NewClient(opts...)

	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 256,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return CallResult{}, classifyAnthropic(err)
	}

	var sb strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return CallResult{
		Text: sb.String(),
		Usage: Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
		StopReason: string(msg.StopReason),
	}, nil
}
```

Create `openai.go`:

```go
package llmerr

import (
	"context"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// ErrNoChoices is returned when the OpenAI response carries no choices.
var ErrNoChoices = errors.New("openai: response contained no choices")

// CompleteOpenAI runs one completion, returning usage and a classified error.
func CompleteOpenAI(ctx context.Context, opts []option.RequestOption, model, prompt string) (CallResult, error) {
	client := openai.NewClient(opts...)

	cc, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:               openai.ChatModel(model),
		Messages:            []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
		MaxCompletionTokens: openai.Int(256),
	})
	if err != nil {
		return CallResult{}, classifyOpenAI(err)
	}
	if len(cc.Choices) == 0 {
		return CallResult{}, fmt.Errorf("openai: %w", ErrNoChoices)
	}

	choice := cc.Choices[0]
	return CallResult{
		Text: choice.Message.Content,
		Usage: Usage{
			InputTokens:  int(cc.Usage.PromptTokens),
			OutputTokens: int(cc.Usage.CompletionTokens),
		},
		StopReason: string(choice.FinishReason),
	}, nil
}
```

### The runnable demo

The demo runs a truncated success (reading usage, computing cost, flagging the cut
output) and a rate-limited call (showing the classified sentinel), both against
in-process httptest servers with retries disabled. Output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/llmerr"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func main() {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8",`+
			`"content":[{"type":"text","text":"partial answer"}],"stop_reason":"max_tokens",`+
			`"usage":{"input_tokens":1000,"output_tokens":500}}`)
	}))
	defer okSrv.Close()

	rlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer rlSrv.Close()

	ctx := context.Background()
	pricing := llmerr.Pricing{InputPerMTok: 5, OutputPerMTok: 25}

	res, err := llmerr.CompleteAnthropic(ctx,
		[]option.RequestOption{option.WithAPIKey("k"), option.WithBaseURL(okSrv.URL)},
		"claude-opus-4-8", "hi")
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("success: %q in=%d out=%d cost=$%.6f truncated=%t\n",
		res.Text, res.Usage.InputTokens, res.Usage.OutputTokens,
		pricing.Cost(res.Usage), llmerr.Truncated(res.StopReason))

	_, err = llmerr.CompleteAnthropic(ctx,
		[]option.RequestOption{option.WithAPIKey("k"), option.WithBaseURL(rlSrv.URL), option.WithMaxRetries(0)},
		"claude-opus-4-8", "hi")
	fmt.Printf("rate-limited classified: is-ErrRateLimited=%t\n",
		errors.Is(err, llmerr.ErrRateLimited))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
success: "partial answer" in=1000 out=500 cost=$0.017500 truncated=true
rate-limited classified: is-ErrRateLimited=true
```

### Tests

`TestClassification` is a table of stubbed status codes, one row per taxonomy
sentinel, asserted with `errors.Is` and run against both adapters. Every case sets
`WithMaxRetries(0)` so a 429 or 500 surfaces on the first attempt rather than
being retried. `TestUsageAndCost` proves usage extraction and the cost formula on
a success body. `TestTruncationDetected` proves `Truncated` fires for both
Anthropic's `max_tokens` and OpenAI's `length` and stays false for a clean stop.

Create `llmerr_test.go`:

```go
package llmerr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	openaiopt "github.com/openai/openai-go/v3/option"
)

func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"e","message":"m"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", 401, ErrAuth},
		{"forbidden", 403, ErrAuth},
		{"rate limited", 429, ErrRateLimited},
		{"bad request", 400, ErrBadRequest},
		{"not found", 404, ErrBadRequest},
		{"server error", 500, ErrTransient},
		{"bad gateway", 502, ErrTransient},
	}
	for _, tc := range tests {
		t.Run("anthropic/"+tc.name, func(t *testing.T) {
			t.Parallel()
			srv := statusServer(t, tc.status)
			_, err := CompleteAnthropic(context.Background(),
				[]anthropicopt.RequestOption{
					anthropicopt.WithAPIKey("k"),
					anthropicopt.WithBaseURL(srv.URL),
					anthropicopt.WithMaxRetries(0),
				}, "m", "hi")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want errors.Is %v", tc.status, err, tc.want)
			}
		})
		t.Run("openai/"+tc.name, func(t *testing.T) {
			t.Parallel()
			srv := statusServer(t, tc.status)
			_, err := CompleteOpenAI(context.Background(),
				[]openaiopt.RequestOption{
					openaiopt.WithAPIKey("k"),
					openaiopt.WithBaseURL(srv.URL),
					openaiopt.WithMaxRetries(0),
				}, "m", "hi")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want errors.Is %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestUsageAndCost(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":1000,"output_tokens":500}}`)
	}))
	t.Cleanup(srv.Close)

	res, err := CompleteAnthropic(context.Background(),
		[]anthropicopt.RequestOption{anthropicopt.WithAPIKey("k"), anthropicopt.WithBaseURL(srv.URL)},
		"m", "hi")
	if err != nil {
		t.Fatalf("CompleteAnthropic: %v", err)
	}
	if res.Usage != (Usage{InputTokens: 1000, OutputTokens: 500}) {
		t.Errorf("usage = %+v, want {1000 500}", res.Usage)
	}
	got := Pricing{InputPerMTok: 5, OutputPerMTok: 25}.Cost(res.Usage)
	if want := 0.0175; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestTruncationDetected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		want   bool
	}{
		{"max_tokens", true}, // Anthropic cutoff
		{"length", true},     // OpenAI cutoff
		{"end_turn", false},  // Anthropic clean stop
		{"stop", false},      // OpenAI clean stop
	}
	for _, tc := range tests {
		if got := Truncated(tc.reason); got != tc.want {
			t.Errorf("Truncated(%q) = %t, want %t", tc.reason, got, tc.want)
		}
	}
}

func ExampleTruncated() {
	fmt.Println(Truncated("max_tokens"), Truncated("length"), Truncated("end_turn"))
	// Output: true true false
}
```

## Review

Classification is correct when each status maps to exactly one sentinel and the
retryable/non-retryable split is preserved: 401/403 to `ErrAuth`, 429 to
`ErrRateLimited`, 400/404/422 to `ErrBadRequest`, 5xx to `ErrTransient`, and a
cancelled context to `ErrTimeout`. The `TestClassification` table proves this for
both vendors under `errors.Is`, and it sets `WithMaxRetries(0)` so a 429 or 5xx
is not silently retried before the assertion — the single most common reason such
a test flakes or hangs. Classify with `errors.As` into the vendor `*Error` and
switch on `StatusCode`; string-matching the message would break the moment a
vendor rewords it, and would lose the `RequestID` you need for support.

Usage and truncation are the accounting half. Read `Usage` on every response, not
only on the ones you happen to inspect — `Pricing.Cost` is only trustworthy if the
counts were captured at the source. And treat `Truncated` as a real error signal:
a `max_tokens`/`length` stop means the model was cut off, so the text is a
fragment. The one subtlety to keep honest is that an unknown error shape falls
through the classifier unchanged rather than being forced into a sentinel —
mislabeling a transport error as `ErrBadRequest` would be worse than surfacing it
raw. Run `go test -race` to confirm concurrent classification and usage reads are
safe.

## Resources

- [Anthropic Go SDK — error handling](https://github.com/anthropics/anthropic-sdk-go#errors) — `*anthropic.Error`, `StatusCode`, `RequestID`, and `WithMaxRetries`.
- [OpenAI Go SDK — errors and retries](https://github.com/openai/openai-go#errors) — `*openai.Error` and the default retry behavior.
- [errors package](https://pkg.go.dev/errors) — `errors.As` and `errors.Is` for unwrapping and classification.
- [Anthropic API errors](https://platform.claude.com/docs/en/api/errors) — the status-code semantics behind the taxonomy.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-config-models-and-generation-params.md](02-config-models-and-generation-params.md) | Next: [../02-streaming-completions/00-concepts.md](../02-streaming-completions/00-concepts.md)
