# Exercise 2: Config-driven model selection, generation params, and deadlines

You will build a functional-options config layer that both adapters consume:
model ID selection, generation parameters expressed with the SDKs' optional-field
wrappers, per-request timeouts, a base-URL override for routing through a gateway,
and validation that rejects impossible configs before any network call is made.
The tests prove the config reaches the wire in each vendor's field shape and that
a context deadline surfaces as `context.DeadlineExceeded`.

## What you'll build

```text
llmconfig/                        independent module: example.com/llmconfig
  go.mod                          go 1.26; requires the Anthropic + OpenAI SDKs
  config.go                       Config, functional Options, Validate, sentinel errors
  anthropic.go                    CompleteAnthropic: Config -> anthropic.MessageNewParams
  openai.go                       CompleteOpenAI: Config -> openai.ChatCompletionNewParams
  cmd/
    demo/
      main.go                     validated config driving an httptest-backed call
  llmconfig_test.go               validation table + wire-shape + deadline tests; Example
```

Files: `config.go`, `anthropic.go`, `openai.go`, `cmd/demo/main.go`, `llmconfig_test.go`.
Implement: a `Config` built with functional options and validated by `Validate` (wrapped sentinels), plus one completion function per vendor that turns the config into that vendor's request params and client options.
Test: validation cases asserted with `errors.Is`; `httptest` body captures proving model, max-tokens, and temperature reach the wire in the right field shape (and that an unset temperature is omitted, not sent as 0); a cancellation test proving `context.DeadlineExceeded` propagates.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/llmconfig/cmd/demo
cd ~/go-exercises/llmconfig
go mod init example.com/llmconfig
go get github.com/anthropics/anthropic-sdk-go@latest
go get github.com/openai/openai-go/v3@latest
```

### Config, options, and validate-before-you-call

Functional options give the config a self-documenting, append-only construction
API: `New(WithModel(...), WithMaxTokens(...), ...)`. The important discipline is
that `New` validates before returning, so an impossible config is a caller error
caught at construction time rather than a 400 discovered after a round trip. The
sentinels are wrapped with `%w` so callers can classify the failure with
`errors.Is` — an empty model, a non-positive max-tokens, or an out-of-range
temperature each map to a distinct sentinel.

`Temperature` is a `*float64` on the config, not a plain `float64`, for the exact
reason the SDKs use `param.Opt[T]`: the config must distinguish "no temperature
configured" (`nil`) from "temperature deliberately 0" (a pointer to 0). That
distinction is carried all the way to the wire in the adapters.

Create `config.go`:

```go
package llmconfig

import (
	"errors"
	"fmt"
	"time"
)

// Validation sentinels. Wrap with %w so callers can errors.Is against them.
var (
	ErrNoModel     = errors.New("model is required")
	ErrMaxTokens   = errors.New("max tokens must be positive")
	ErrTemperature = errors.New("temperature must be in [0, 2]")
)

// Config is the vendor-neutral generation config both adapters consume.
type Config struct {
	Model          string
	MaxTokens      int
	Temperature    *float64 // nil = unset (server default); non-nil = explicit
	APIKey         string
	BaseURL        string        // gateway / proxy / httptest override
	RequestTimeout time.Duration // per-attempt bound (0 = SDK default)
	MaxRetries     *int          // nil = SDK default (2); 0 disables retries
}

// Option mutates a Config during New.
type Option func(*Config)

func WithModel(m string) Option        { return func(c *Config) { c.Model = m } }
func WithMaxTokens(n int) Option       { return func(c *Config) { c.MaxTokens = n } }
func WithTemperature(t float64) Option { return func(c *Config) { c.Temperature = &t } }
func WithAPIKey(k string) Option       { return func(c *Config) { c.APIKey = k } }
func WithBaseURL(u string) Option      { return func(c *Config) { c.BaseURL = u } }
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Config) { c.RequestTimeout = d }
}
func WithMaxRetries(n int) Option { return func(c *Config) { c.MaxRetries = &n } }

// New builds and validates a Config. It returns a wrapped sentinel on any
// impossible field, so the caller never issues a request that is certain to 400.
func New(opts ...Option) (Config, error) {
	var c Config
	for _, o := range opts {
		o(&c)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate rejects impossible configs before any network call.
func (c Config) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("config: %w", ErrNoModel)
	}
	if c.MaxTokens <= 0 {
		return fmt.Errorf("config: %w", ErrMaxTokens)
	}
	if c.Temperature != nil && (*c.Temperature < 0 || *c.Temperature > 2) {
		return fmt.Errorf("config: %w (%.2f)", ErrTemperature, *c.Temperature)
	}
	return nil
}
```

### Client options versus request params, made concrete

The two configuration surfaces from the concepts file appear here as two distinct
translation steps. `Config.BaseURL`, `APIKey`, `RequestTimeout`, and `MaxRetries`
become *client options* (`option.With*`) applied at client construction.
`Config.Model`, `MaxTokens`, and `Temperature` become *request params* on the
params struct. `WithBaseURL` is the gateway knob: set it to an httptest URL in CI
or to a LiteLLM / egress-proxy URL in production, and the same completion code
routes accordingly.

Create `anthropic.go`:

```go
package llmconfig

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func (c Config) anthropicOptions() []option.RequestOption {
	opts := []option.RequestOption{}
	if c.APIKey != "" {
		opts = append(opts, option.WithAPIKey(c.APIKey))
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	if c.RequestTimeout > 0 {
		opts = append(opts, option.WithRequestTimeout(c.RequestTimeout))
	}
	if c.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*c.MaxRetries))
	}
	return opts
}

// CompleteAnthropic runs one completion using the config's params and options.
func CompleteAnthropic(ctx context.Context, c Config, prompt string) (string, error) {
	client := anthropic.NewClient(c.anthropicOptions()...)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.Model),
		MaxTokens: int64(c.MaxTokens), // required by Anthropic; Validate guarantees > 0
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	}
	if c.Temperature != nil {
		// Only sent when configured. Note: recent Anthropic models reject
		// temperature with a 400 — a production adapter would gate this per model.
		params.Temperature = anthropic.Float(*c.Temperature)
	}

	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String(), nil
}
```

Create `openai.go`. The key contrast: OpenAI's output cap is `MaxCompletionTokens`,
set through the `openai.Int` `param.Opt` constructor, and temperature is likewise
sent only when configured so an unset value is omitted rather than sent as 0.

```go
package llmconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// ErrNoChoices is returned when the OpenAI response carries no choices.
var ErrNoChoices = errors.New("openai: response contained no choices")

func (c Config) openaiOptions() []option.RequestOption {
	opts := []option.RequestOption{}
	if c.APIKey != "" {
		opts = append(opts, option.WithAPIKey(c.APIKey))
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	if c.RequestTimeout > 0 {
		opts = append(opts, option.WithRequestTimeout(c.RequestTimeout))
	}
	if c.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*c.MaxRetries))
	}
	return opts
}

// CompleteOpenAI runs one completion using the config's params and options.
func CompleteOpenAI(ctx context.Context, c Config, prompt string) (string, error) {
	client := openai.NewClient(c.openaiOptions()...)

	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(c.Model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		// Optional for OpenAI, but Validate guarantees a positive cap, so send it.
		MaxCompletionTokens: openai.Int(int64(c.MaxTokens)),
	}
	if c.Temperature != nil {
		params.Temperature = openai.Float(*c.Temperature)
	}

	cc, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", err
	}
	if len(cc.Choices) == 0 {
		return "", fmt.Errorf("openai: %w", ErrNoChoices)
	}
	return cc.Choices[0].Message.Content, nil
}
```

### The runnable demo

The demo builds a validated config, runs one completion against an in-process
httptest server (via `WithBaseURL`), and then shows validation rejecting an
impossible config before any call. The output is deterministic because the stub
returns a fixed body.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/llmconfig"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5-2",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	}))
	defer srv.Close()

	cfg, err := llmconfig.New(
		llmconfig.WithModel("gpt-5-2"),
		llmconfig.WithMaxTokens(128),
		llmconfig.WithTemperature(0.2),
		llmconfig.WithAPIKey("test-key"),
		llmconfig.WithBaseURL(srv.URL),
	)
	if err != nil {
		fmt.Println("unexpected config error:", err)
		return
	}
	fmt.Printf("config valid: model=%s maxTokens=%d\n", cfg.Model, cfg.MaxTokens)

	text, err := llmconfig.CompleteOpenAI(context.Background(), cfg, "hi")
	if err != nil {
		fmt.Println("call error:", err)
		return
	}
	fmt.Printf("response: %q\n", text)

	_, err = llmconfig.New(llmconfig.WithModel("gpt-5-2"), llmconfig.WithMaxTokens(64),
		llmconfig.WithTemperature(9))
	fmt.Printf("rejected bad config: %v (is-temp-error=%t)\n",
		err, errors.Is(err, llmconfig.ErrTemperature))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
config valid: model=gpt-5-2 maxTokens=128
response: "ok"
rejected bad config: config: temperature must be in [0, 2] (9.00) (is-temp-error=true)
```

### Tests

The validation tests are a table asserting each sentinel with `errors.Is`. The
wire-shape tests capture the outbound body and prove the configured model and cap
land on the wire in each vendor's field name (`max_tokens` for Anthropic,
`max_completion_tokens` for OpenAI). The optional-field pair
(`TestTemperatureOmittedWhenUnset` and `TestTemperatureZeroIsSent`) is the crux:
it proves the `param.Opt` wrappers distinguish unset from a deliberate 0 — the
first asserts no `temperature` key appears, the second asserts an explicit 0 does.
`TestDeadlineExceeded` uses a slow handler and `WithMaxRetries(0)` so a short
context deadline surfaces as `context.DeadlineExceeded` rather than being retried.

Create `llmconfig_test.go`:

```go
package llmconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want error
	}{
		{"no model", []Option{WithMaxTokens(16)}, ErrNoModel},
		{"zero max tokens", []Option{WithModel("m")}, ErrMaxTokens},
		{"negative max tokens", []Option{WithModel("m"), WithMaxTokens(-1)}, ErrMaxTokens},
		{"temp too high", []Option{WithModel("m"), WithMaxTokens(16), WithTemperature(2.5)}, ErrTemperature},
		{"temp negative", []Option{WithModel("m"), WithMaxTokens(16), WithTemperature(-0.1)}, ErrTemperature},
		{"valid", []Option{WithModel("m"), WithMaxTokens(16), WithTemperature(0)}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.opts...)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("New() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("New() = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// bodyCapture returns a stub server that decodes the request body into dst.
func bodyCapture(t *testing.T, dst *map[string]any, respJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, dst)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respJSON)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const openaiOK = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5-2",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`

const anthropicOK = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8",` +
	`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":5,"output_tokens":1}}`

func TestAnthropicConfigOnWire(t *testing.T) {
	t.Parallel()
	var body map[string]any
	srv := bodyCapture(t, &body, anthropicOK)
	cfg, err := New(WithModel("claude-opus-4-8"), WithMaxTokens(200), WithAPIKey("k"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := CompleteAnthropic(context.Background(), cfg, "hi"); err != nil {
		t.Fatalf("CompleteAnthropic: %v", err)
	}
	if body["model"] != "claude-opus-4-8" {
		t.Errorf("model = %v, want claude-opus-4-8", body["model"])
	}
	if body["max_tokens"] != float64(200) {
		t.Errorf("max_tokens = %v, want 200", body["max_tokens"])
	}
}

func TestOpenAIConfigOnWire(t *testing.T) {
	t.Parallel()
	var body map[string]any
	srv := bodyCapture(t, &body, openaiOK)
	cfg, err := New(WithModel("gpt-5-2"), WithMaxTokens(200), WithAPIKey("k"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := CompleteOpenAI(context.Background(), cfg, "hi"); err != nil {
		t.Fatalf("CompleteOpenAI: %v", err)
	}
	if body["model"] != "gpt-5-2" {
		t.Errorf("model = %v, want gpt-5-2", body["model"])
	}
	// OpenAI names the cap max_completion_tokens, not max_tokens.
	if body["max_completion_tokens"] != float64(200) {
		t.Errorf("max_completion_tokens = %v, want 200", body["max_completion_tokens"])
	}
}

func TestTemperatureOmittedWhenUnset(t *testing.T) {
	t.Parallel()
	var body map[string]any
	srv := bodyCapture(t, &body, openaiOK)
	cfg, _ := New(WithModel("gpt-5-2"), WithMaxTokens(16), WithAPIKey("k"), WithBaseURL(srv.URL))
	if _, err := CompleteOpenAI(context.Background(), cfg, "hi"); err != nil {
		t.Fatalf("CompleteOpenAI: %v", err)
	}
	if _, present := body["temperature"]; present {
		t.Errorf("temperature present on wire when unset: %v", body["temperature"])
	}
}

func TestTemperatureZeroIsSent(t *testing.T) {
	t.Parallel()
	var body map[string]any
	srv := bodyCapture(t, &body, openaiOK)
	cfg, _ := New(WithModel("gpt-5-2"), WithMaxTokens(16), WithTemperature(0), WithAPIKey("k"), WithBaseURL(srv.URL))
	if _, err := CompleteOpenAI(context.Background(), cfg, "hi"); err != nil {
		t.Fatalf("CompleteOpenAI: %v", err)
	}
	got, present := body["temperature"]
	if !present || got != float64(0) {
		t.Errorf("temperature = %v (present=%t), want explicit 0", got, present)
	}
}

func TestDeadlineExceeded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // slower than the context deadline
		fmt.Fprint(w, openaiOK)
	}))
	t.Cleanup(srv.Close)

	cfg, _ := New(WithModel("gpt-5-2"), WithMaxTokens(16), WithAPIKey("k"),
		WithBaseURL(srv.URL), WithMaxRetries(0)) // no retries: fail deterministically

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := CompleteOpenAI(ctx, cfg, "hi")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func ExampleConfig_Validate() {
	_, err := New(WithModel(""), WithMaxTokens(16))
	fmt.Println(errors.Is(err, ErrNoModel))
	// Output: true
}
```

## Review

The config is correct when an invalid one cannot escape `New`: an empty model, a
non-positive cap, and an out-of-range temperature each produce a wrapped sentinel,
proved by the `TestValidate` table under `errors.Is`. The adapters are correct
when the configured model and cap arrive on the wire in the right vendor field —
`max_tokens` for Anthropic, `max_completion_tokens` for OpenAI — and when the
optional-field distinction holds: an unset temperature must be absent from the
body and a configured 0 must be an explicit 0. That pair of tests is the one that
catches a subtle regression where someone "simplifies" `*float64` to a plain
`float64` and silently starts overriding the server default on every request.

The traps to remember: a request timeout is per-attempt, so a hard end-to-end
bound comes from the `context.WithTimeout` deadline threaded into the call — which
is what `TestDeadlineExceeded` verifies, using `WithMaxRetries(0)` so the failure
is not retried into a longer wait. Keep the model ID config-driven; the tests pass
it as a plain string precisely to model the real deployment where the ID lives in
configuration. Run `go test -race` to confirm the config value is safe to share
across goroutines (it is: a `Config` is read-only after `New`).

## Resources

- [OpenAI Go SDK — request options](https://pkg.go.dev/github.com/openai/openai-go/v3/option) — `WithBaseURL`, `WithRequestTimeout`, `WithMaxRetries`.
- [OpenAI Go SDK — param package](https://pkg.go.dev/github.com/openai/openai-go/v3/packages/param) — the `Opt[T]` optional-field model behind `openai.Int` / `openai.Float`.
- [Anthropic Go SDK — pkg.go.dev](https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go) — `MessageNewParams.MaxTokens` (required) and `Temperature`.
- [context package](https://pkg.go.dev/context) — `WithTimeout` and `DeadlineExceeded` for the operation-wide ceiling.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-provider-port-and-adapters.md](01-provider-port-and-adapters.md) | Next: [03-error-taxonomy-and-usage-accounting.md](03-error-taxonomy-and-usage-accounting.md)
