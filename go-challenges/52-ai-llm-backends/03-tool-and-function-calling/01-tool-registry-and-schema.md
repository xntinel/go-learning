# Exercise 1: A Typed Tool Registry with Schema and Argument Validation

Before any model is involved, tool calling needs a dispatcher: something that
maps a tool name and a blob of JSON arguments to a typed handler, validates the
arguments as the hostile input they are, and returns a structured result instead
of panicking. This exercise builds that provider-neutral registry — the piece
both the Anthropic and OpenAI loops in the next two exercises reuse.

This module is fully self-contained. It has its own `go mod init`, uses only the
standard library, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
toolregistry/               independent module: example.com/toolregistry
  go.mod                    go 1.26; standard library only
  registry.go               Registry, RegisterTyped[T], Dispatch -> Result{IsError}; two example tools
  cmd/
    demo/
      main.go               dispatches valid, malformed, missing-field, unknown-tool, and handler-error calls
  registry_test.go          table-driven dispatch, golden schema, errors.Is on sentinels, Example
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a `Registry` with a generic `RegisterTyped[T]` that stores a JSON Schema and a typed handler, and a `Dispatch` that decodes into `T`, validates, runs the handler, and returns a `Result` carrying an `IsError` flag — never a panic.
- Test: valid args dispatch and marshal; malformed JSON, unknown tool, missing required field, a hallucinated extra field, and a handler error each yield `IsError` results; a golden-JSON assertion on a tool's input schema; `errors.Is` on the wrapped sentinels.
- Verify: `go test -count=1 -race ./...`

Set up the module. It needs nothing beyond the standard library:

```bash
mkdir -p ~/go-exercises/toolregistry/cmd/demo
cd ~/go-exercises/toolregistry
go mod init example.com/toolregistry
go mod edit -go=1.26
```

### Why a registry, and why generics

A tool call arriving from either provider reduces to the same two values: a
`name` (which tool) and a `json.RawMessage` (the arguments the model produced).
The registry's job is to turn that untyped pair into a typed, validated function
call and a structured result. Keeping this provider-neutral is the whole point —
the wire envelopes differ between Anthropic and OpenAI, but the dispatch logic is
identical, so it lives once, here, and both loops call into it.

The tension is that `Dispatch` must be untyped (it takes `json.RawMessage`) while
each handler wants to work with a strongly-typed argument struct. Generics
resolve it. `RegisterTyped[T]` is where `T` is known; it closes over a handler
that decodes `json.RawMessage` into `T`, validates, and calls your typed function.
By the time the closure is stored in the map, `T` has been erased back to
`json.RawMessage`, so `Dispatch` stays uniform. Note that `RegisterTyped` is a
free function taking `*Registry`, not a method: Go methods may not declare their
own type parameters, so a generic "register" has to be a function.

### Decoding is a security boundary, not a convenience

The decode step is where the threat model shows up in code. Three deliberate
choices make it a boundary rather than a rubber stamp. First, it decodes with
`json.Decoder.DisallowUnknownFields`, so a hallucinated field the model invented —
a `city` key on a tool that only declares `location` — is rejected rather than
silently dropped; a field you did not declare is a signal something is off.
Second, a decode failure never panics: it returns an error that wraps the
`ErrInvalidArgs` sentinel, which `Dispatch` turns into an `IsError` result the
model can read and correct. Third, semantic validation the schema cannot express —
"location is required", "unit is one of celsius/fahrenheit" — runs through an
optional `Validator` interface: if the decoded struct implements `Validate() error`,
the registry calls it. This is the layer that catches the required-but-omitted
field, because JSON Schema's `required` is advisory to the model, not enforced by
the decoder.

Contrast this with the anti-pattern the concepts named: `args["location"].(string)`
on an undecoded map panics the instant the model omits the key or sends a number.
Here there is no map and no unchecked assertion — only a typed struct and handled
errors.

### The result shape mirrors the wire

`Dispatch` returns a `Result{Name, Content, IsError}`. That shape is not arbitrary:
it is exactly what the provider loops need to build a `tool_result` (Anthropic) or
a `role=tool` message (OpenAI). A recoverable failure — bad arguments, an unknown
tool, a handler error like "exchange rate unavailable" — comes back with
`IsError` true and the error text as the content, so the model self-corrects. A
success comes back with `IsError` false and the marshalled return value. Crucially
`Dispatch` returns no Go `error`: every failure at this layer is one the model is
meant to see and react to, so it is data, not a control-flow exception. The
sentinels wrapped with `%w` still exist so unit tests can assert the *cause* with
`errors.Is`, even though across the wire the model only ever sees text.

Create `registry.go`:

```go
package toolregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

// Sentinel errors classify failures so callers and tests can branch with
// errors.Is instead of matching strings.
var (
	// ErrUnknownTool is returned when a dispatch names a tool that was never registered.
	ErrUnknownTool = errors.New("unknown tool")
	// ErrInvalidArgs is returned when the model's JSON arguments fail to decode or validate.
	ErrInvalidArgs = errors.New("invalid arguments")
	// ErrRateUnavailable is returned by convert_currency for an unknown currency pair.
	ErrRateUnavailable = errors.New("exchange rate unavailable")
)

// Validator is implemented by argument structs that need semantic checks the JSON
// Schema cannot express (bounds, enums, cross-field rules). RegisterTyped calls
// Validate after a successful decode.
type Validator interface {
	Validate() error
}

// Tool couples a handler with the JSON Schema advertised to the model. The schema
// is the wire contract that both provider SDKs serialize into a tool definition.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	handler     func(ctx context.Context, raw json.RawMessage) (any, error)
}

// Result mirrors a provider tool_result: structured content plus an is_error flag
// that lets the model self-correct instead of the loop crashing.
type Result struct {
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// Registry is a provider-neutral dispatcher mapping tool names to typed handlers.
type Registry struct {
	tools map[string]Tool
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// RegisterTyped adds a handler whose arguments decode into T. It is a free
// function, not a method, because Go methods may not introduce their own type
// parameters. The closure it stores erases T back to json.RawMessage so Dispatch
// stays untyped, while the handler body works with a strongly-typed struct.
func RegisterTyped[T any](r *Registry, name, description string, schema map[string]any, fn func(context.Context, T) (any, error)) {
	r.tools[name] = Tool{
		Name:        name,
		Description: description,
		Schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			if len(bytes.TrimSpace(raw)) == 0 {
				raw = json.RawMessage("{}")
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var args T
			if err := dec.Decode(&args); err != nil {
				return nil, fmt.Errorf("%s: decode arguments: %v: %w", name, err, ErrInvalidArgs)
			}
			if v, ok := any(&args).(Validator); ok {
				if err := v.Validate(); err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
			}
			return fn(ctx, args)
		},
	}
}

// Names returns the registered tool names, sorted, for deterministic listing.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Schema returns the JSON Schema registered for a tool, or false if absent.
func (r *Registry) Schema(name string) (map[string]any, bool) {
	t, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	return t.Schema, true
}

// Dispatch decodes and runs the named tool, always returning a Result. A missing
// tool, malformed JSON, failed validation, or a handler error each become a
// Result with IsError set true; Dispatch never panics and never returns a Go
// error, because every failure here is one the model can read and recover from.
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) Result {
	t, ok := r.tools[name]
	if !ok {
		return errorResult(name, fmt.Errorf("%q: %w", name, ErrUnknownTool))
	}
	out, err := t.handler(ctx, args)
	if err != nil {
		return errorResult(name, err)
	}
	content, err := json.Marshal(out)
	if err != nil {
		return errorResult(name, fmt.Errorf("%s: marshal result: %w", name, err))
	}
	return Result{Name: name, Content: content, IsError: false}
}

func errorResult(name string, err error) Result {
	content, _ := json.Marshal(map[string]string{"error": err.Error()})
	return Result{Name: name, Content: content, IsError: true}
}

// --- Example tools wired on top of the registry ---

// WeatherArgs is the typed argument surface of the get_weather tool.
type WeatherArgs struct {
	Location string `json:"location"`
	Unit     string `json:"unit"`
}

// Validate enforces the semantics the JSON Schema cannot: a non-empty location
// and a unit drawn from a fixed enumeration. Model-supplied arguments are
// untrusted, so this runs on every call.
func (a WeatherArgs) Validate() error {
	if a.Location == "" {
		return fmt.Errorf("location must not be empty: %w", ErrInvalidArgs)
	}
	switch a.Unit {
	case "", "celsius", "fahrenheit":
		return nil
	default:
		return fmt.Errorf("unit %q not in [celsius fahrenheit]: %w", a.Unit, ErrInvalidArgs)
	}
}

// WeatherReport is the get_weather result.
type WeatherReport struct {
	Location string `json:"location"`
	TempC    int    `json:"temp_c"`
	Summary  string `json:"summary"`
}

func getWeather(_ context.Context, a WeatherArgs) (any, error) {
	// A real handler would call a weather provider here, propagating ctx so a
	// cancelled request cancels the downstream call.
	return WeatherReport{Location: a.Location, TempC: 18, Summary: "overcast"}, nil
}

// ConvertArgs is the typed argument surface of the convert_currency tool.
type ConvertArgs struct {
	Amount float64 `json:"amount"`
	From   string  `json:"from"`
	To     string  `json:"to"`
}

// Validate rejects a negative amount. The currency pair is checked in the handler
// so a missing rate surfaces as a distinct sentinel.
func (a ConvertArgs) Validate() error {
	if a.Amount < 0 {
		return fmt.Errorf("amount must be non-negative: %w", ErrInvalidArgs)
	}
	return nil
}

func convertCurrency(_ context.Context, a ConvertArgs) (any, error) {
	rates := map[string]float64{"USD:EUR": 0.92, "EUR:USD": 1.09}
	rate, ok := rates[a.From+":"+a.To]
	if !ok {
		return nil, fmt.Errorf("%s to %s: %w", a.From, a.To, ErrRateUnavailable)
	}
	converted := math.Round(a.Amount*rate*100) / 100
	return map[string]any{"amount": converted, "currency": a.To}, nil
}

// Default returns a registry populated with the two example tools and their
// hand-written JSON Schemas.
func Default() *Registry {
	r := New()
	RegisterTyped(r, "get_weather", "Current weather for a location.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"location": map[string]any{"type": "string", "description": "city and country, e.g. 'Paris, FR'"},
				"unit":     map[string]any{"type": "string", "enum": []string{"celsius", "fahrenheit"}},
			},
			"required": []string{"location"},
		}, getWeather)
	RegisterTyped(r, "convert_currency", "Convert an amount between two ISO-4217 currencies.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"amount": map[string]any{"type": "number"},
				"from":   map[string]any{"type": "string"},
				"to":     map[string]any{"type": "string"},
			},
			"required": []string{"amount", "from", "to"},
		}, convertCurrency)
	return r
}
```

The schemas are written by hand as `map[string]any` rather than reflected from the
structs. That duplicates the shape, but it makes the contract explicit and lets
each field carry a precise description — and since the schema doubles as prompt
material for the model, that control is worth the duplication here.

### The runnable demo

The demo drives one call of each kind through `Dispatch` and prints the result,
so you can see that every failure path produces a structured `IsError` result and
not a crash. Note the second call sends deliberately malformed JSON and the fourth
sends a field the schema never declared.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"example.com/toolregistry"
)

func main() {
	ctx := context.Background()
	reg := toolregistry.Default()

	calls := []struct {
		name string
		args string
	}{
		{"get_weather", `{"location":"Paris, FR","unit":"celsius"}`},
		{"get_weather", `{"location":`},                               // malformed JSON
		{"get_weather", `{"unit":"celsius"}`},                         // missing required location
		{"get_weather", `{"location":"Paris, FR","city":"Paris"}`},    // hallucinated field
		{"convert_currency", `{"amount":10,"from":"USD","to":"JPY"}`}, // handler error
		{"teleport", `{}`}, // unknown tool
	}

	for _, c := range calls {
		res := reg.Dispatch(ctx, c.name, json.RawMessage(c.args))
		fmt.Printf("%-16s isError=%-5v %s\n", res.Name, res.IsError, res.Content)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
get_weather      isError=false {"location":"Paris, FR","temp_c":18,"summary":"overcast"}
get_weather      isError=true  {"error":"get_weather: decode arguments: unexpected EOF: invalid arguments"}
get_weather      isError=true  {"error":"get_weather: location must not be empty: invalid arguments"}
get_weather      isError=true  {"error":"get_weather: decode arguments: json: unknown field \"city\": invalid arguments"}
convert_currency isError=true  {"error":"USD to JPY: exchange rate unavailable"}
teleport         isError=true  {"error":"\"teleport\": unknown tool"}
```

### Tests

The tests are pure and need no network. The table drives every dispatch path and
asserts only the `IsError` flag, which is the contract that matters: a valid call
is a success, and each of the five failure modes is a recoverable error result,
never a Go error out of `Dispatch`. `TestSchemaGolden` marshals a registered tool's
input schema and compares it byte-for-byte to a golden string — `json.Marshal`
sorts map keys, so the output is deterministic — which is how you catch an
accidental change to the model-facing contract. Two `errors.Is` tests assert that
the validation and handler sentinels are wrapped with `%w`, so the *cause* is
recoverable in code even though the model only sees text.

Create `registry_test.go`:

```go
package toolregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestDispatch(t *testing.T) {
	t.Parallel()
	reg := Default()
	tests := []struct {
		name        string
		tool        string
		args        string
		wantIsError bool
	}{
		{"valid weather", "get_weather", `{"location":"Paris, FR","unit":"celsius"}`, false},
		{"malformed json", "get_weather", `{"location":`, true},
		{"missing required", "get_weather", `{"unit":"celsius"}`, true},
		{"hallucinated field", "get_weather", `{"location":"Paris, FR","nope":1}`, true},
		{"bad enum", "get_weather", `{"location":"Paris, FR","unit":"kelvin"}`, true},
		{"unknown tool", "teleport", `{}`, true},
		{"handler error", "convert_currency", `{"amount":1,"from":"USD","to":"JPY"}`, true},
		{"valid convert", "convert_currency", `{"amount":10,"from":"USD","to":"EUR"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := reg.Dispatch(t.Context(), tt.tool, json.RawMessage(tt.args))
			if res.IsError != tt.wantIsError {
				t.Fatalf("Dispatch(%s) IsError = %v, want %v (content=%s)", tt.tool, res.IsError, tt.wantIsError, res.Content)
			}
			if res.Name != tt.tool {
				t.Errorf("Result.Name = %q, want %q", res.Name, tt.tool)
			}
			if !json.Valid(res.Content) {
				t.Errorf("Result.Content is not valid JSON: %s", res.Content)
			}
		})
	}
}

func TestSchemaGolden(t *testing.T) {
	t.Parallel()
	reg := Default()
	schema, ok := reg.Schema("get_weather")
	if !ok {
		t.Fatal("get_weather not registered")
	}
	got, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	const want = `{"properties":{"location":{"description":"city and country, e.g. 'Paris, FR'","type":"string"},"unit":{"enum":["celsius","fahrenheit"],"type":"string"}},"required":["location"],"type":"object"}`
	if string(got) != want {
		t.Errorf("schema mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestNames(t *testing.T) {
	t.Parallel()
	got := Default().Names()
	want := []string{"convert_currency", "get_weather"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestValidateSentinel(t *testing.T) {
	t.Parallel()
	if err := (WeatherArgs{Location: ""}).Validate(); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("Validate empty location err = %v, want wrapped ErrInvalidArgs", err)
	}
}

func TestHandlerSentinel(t *testing.T) {
	t.Parallel()
	_, err := convertCurrency(t.Context(), ConvertArgs{Amount: 1, From: "USD", To: "JPY"})
	if !errors.Is(err, ErrRateUnavailable) {
		t.Fatalf("convertCurrency err = %v, want wrapped ErrRateUnavailable", err)
	}
}

func Example() {
	reg := Default()
	res := reg.Dispatch(context.Background(), "convert_currency",
		json.RawMessage(`{"amount":10,"from":"USD","to":"EUR"}`))
	fmt.Printf("isError=%v content=%s\n", res.IsError, res.Content)
	// Output: isError=false content={"amount":9.2,"currency":"EUR"}
}
```

## Review

The registry is correct when a well-formed call returns `IsError` false with the
marshalled handler result, and every one of the five failure modes — malformed
JSON, an unknown tool, a missing required field, a hallucinated extra field, and a
handler error — returns `IsError` true with a JSON error body, all without a panic
and without a Go error escaping `Dispatch`. That last part is the whole design:
`Dispatch` returns no `error` because at this layer there is no failure the model
should not be allowed to see and retry. The golden-schema test guards the other
half of the contract, the model-facing schema, against silent drift.

The mistakes to avoid are the ones the concepts warn about, made concrete. Do not
reach into decoded arguments with an unchecked type assertion; here there is a
typed struct and a handled `err`, which is why a malformed or hostile payload
produces a result instead of a crash. Do not skip `DisallowUnknownFields` — a
model that invents a field is a signal, and dropping the field silently hides it.
And keep the `Validator` layer: JSON Schema's `required` is advice to the model,
so the missing-`location` case is caught by `Validate`, not by the decoder. Run
`go test -race` even though the registry has no goroutines of its own, because the
loops in the next exercises will call `Dispatch` concurrently across parallel tool
calls, and the handlers must stay race-free.

## Resources

- [`encoding/json`](https://pkg.go.dev/encoding/json) — `json.RawMessage`, `Decoder.DisallowUnknownFields`, and `Marshal`'s deterministic key ordering.
- [Type parameters (Go generics)](https://go.dev/ref/spec#Type_parameters) — why `RegisterTyped` must be a function, not a method, to introduce `T`.
- [`errors`](https://pkg.go.dev/errors) — `errors.Is` and the `%w` wrapping the sentinels rely on.
- [OpenAI docs — Function calling](https://platform.openai.com/docs/guides/function-calling) — the JSON Schema shape a registry's `map[string]any` mirrors on the wire.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-anthropic-tool-loop.md](02-anthropic-tool-loop.md)
