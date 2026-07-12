# Exercise 9: Config-Driven Plugin Loading with Pre-Init Validation

Wire the whole system from configuration: a JSON list of `{name, params}` selects
and orders plugins, each plugin's `Validate(params)` runs before any `Init`, and a
bad config fails fast at startup with every problem reported at once — not one
request at a time in production.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
configload/               independent module: example.com/configload
  go.mod                  go 1.25
  loader.go               Config/PluginConfig; Loader (factories); Build (validate-before-init)
  pipeline.go             Pipeline.Run threads output through plugins in order
  plugins.go              prefix and repeat sample plugins with Validate
  cmd/
    demo/
      main.go             decode JSON config, build pipeline, run input through it
  loader_test.go          aggregated-error, no-init-on-failure, and golden-order tests
```

- Files: `loader.go`, `pipeline.go`, `plugins.go`, `cmd/demo/main.go`, `loader_test.go`.
- Implement: a `Config` decoded from JSON; a `Loader` holding `name -> factory`; `Build` that validates *all* plugins before it `Init`s *any*, aggregating config errors with `errors.Join`; a `Pipeline` that runs plugins in declared order.
- Test: a config with two known and one unknown plugin (plus an invalid param) surfaces both the unknown name and the bad param as a single aggregated startup error, and no plugin's `Init` ran; a valid config builds the pipeline in declared order and `Run` threads output through it.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Validate before Init, and aggregate every error

Config-driven loading is what lets an operator change behavior without touching
code: enable a plugin, reorder the pipeline, tune a parameter, all from a file. The
design property that makes that safe is the order of operations at startup.

`Build` runs in two phases. Phase one *validates* every entry: look up the factory
by name (an unknown name is a config error), build a fresh instance, and call its
`Validate(params)` (a bad parameter is a config error). Crucially, it does *not*
`Init` anything yet — and it does not stop at the first error. Every unknown name
and every bad parameter across the whole config is collected. Only if phase one
found zero errors does phase two run: `Init` each plugin in declared order and
assemble the pipeline.

This ordering delivers two operator-facing guarantees. First, fail fast with the
*complete* picture: `errors.Join` returns a single error listing every
misconfiguration, so the operator fixes them all in one edit instead of
rediscovering them one deploy at a time. Second, no partial initialization: if any
validation fails, *nothing* was `Init`ed, so there are no half-opened connections
or half-registered resources to leak or clean up. Validation is pure and cheap;
`Init` acquires resources — separating them is what makes the failure clean.

Placing `Validate` before `Init` on the interface is the whole idea. A plugin
that only checked its params inside `Init` would already have started acquiring
resources by the time it discovered the config was bad.

Create `loader.go`:

```go
package configload

import (
	"errors"
	"fmt"
)

// Plugin is the config-driven contract: params are validated before Init, and
// Process transforms input in a pipeline.
type Plugin interface {
	Name() string
	Validate(params map[string]any) error
	Init() error
	Process(input string) (string, error)
}

// Factory builds a fresh, unconfigured plugin instance.
type Factory func() Plugin

// PluginConfig is one entry in the config: a plugin name and its parameters.
type PluginConfig struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params"`
}

// Config is the decoded plugin pipeline configuration.
type Config struct {
	Plugins []PluginConfig `json:"plugins"`
}

// Loader maps plugin names to factories and builds pipelines from config.
type Loader struct {
	factories map[string]Factory
}

// NewLoader returns an empty loader.
func NewLoader() *Loader {
	return &Loader{factories: make(map[string]Factory)}
}

// Register associates name with a factory.
func (l *Loader) Register(name string, f Factory) {
	l.factories[name] = f
}

// Build validates every config entry before initializing any plugin. It returns
// the joined errors of all unknown names and invalid params (calling no Init if
// validation fails), and otherwise Inits each plugin in declared order and
// returns the assembled Pipeline.
func (l *Loader) Build(cfg Config) (*Pipeline, error) {
	type staged struct {
		plugin Plugin
		params map[string]any
	}

	// Phase 1: validate all, Init none.
	var errs []error
	var pending []staged
	for _, pc := range cfg.Plugins {
		f, ok := l.factories[pc.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown plugin %q", pc.Name))
			continue
		}
		p := f()
		if err := p.Validate(pc.Params); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", pc.Name, err))
			continue
		}
		pending = append(pending, staged{plugin: p, params: pc.Params})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Phase 2: Init in declared order and assemble the pipeline.
	plugins := make([]Plugin, 0, len(pending))
	for _, s := range pending {
		if err := s.plugin.Init(); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q init: %w", s.plugin.Name(), err))
			continue
		}
		plugins = append(plugins, s.plugin)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &Pipeline{plugins: plugins}, nil
}
```

### The pipeline threads output in order

The `Pipeline` runs its plugins in the order they were declared, feeding each
plugin's output into the next. A failure in any stage stops the pipeline and
reports which plugin failed, wrapped with `%w` so the caller can `errors.Is` the
underlying error.

Create `pipeline.go`:

```go
package configload

import "fmt"

// Pipeline runs plugins in declared order, threading each output into the next.
type Pipeline struct {
	plugins []Plugin
}

// Run passes input through each plugin in order and returns the final output.
func (p *Pipeline) Run(input string) (string, error) {
	out := input
	for _, pl := range p.plugins {
		next, err := pl.Process(out)
		if err != nil {
			return "", fmt.Errorf("plugin %q: %w", pl.Name(), err)
		}
		out = next
	}
	return out, nil
}

// Names returns the plugin names in pipeline order.
func (p *Pipeline) Names() []string {
	names := make([]string, len(p.plugins))
	for i, pl := range p.plugins {
		names[i] = pl.Name()
	}
	return names
}
```

### Two sample plugins with real validation

`prefix` prepends a configured string; its `Validate` requires a `prefix` param of
type string. `repeat` repeats its input a configured number of times; its
`Validate` requires a `times` param that is a positive whole number — and because
JSON decodes every number as `float64`, it must accept a `float64` and check it is
a positive integer.

Create `plugins.go`:

```go
package configload

import (
	"fmt"
	"strings"
)

// prefix prepends a configured string to its input.
type prefix struct {
	value string
}

func (prefix) Name() string { return "prefix" }

func (p *prefix) Validate(params map[string]any) error {
	v, ok := params["prefix"]
	if !ok {
		return fmt.Errorf("missing required param %q", "prefix")
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("param %q must be a string, got %T", "prefix", v)
	}
	p.value = s
	return nil
}

func (prefix) Init() error { return nil }

func (p *prefix) Process(input string) (string, error) { return p.value + input, nil }

// repeat repeats its input a configured number of times.
type repeat struct {
	times int
}

func (repeat) Name() string { return "repeat" }

func (r *repeat) Validate(params map[string]any) error {
	v, ok := params["times"]
	if !ok {
		return fmt.Errorf("missing required param %q", "times")
	}
	// JSON numbers decode as float64.
	f, ok := v.(float64)
	if !ok {
		return fmt.Errorf("param %q must be a number, got %T", "times", v)
	}
	n := int(f)
	if float64(n) != f || n < 1 {
		return fmt.Errorf("param %q must be a positive whole number, got %v", "times", v)
	}
	r.times = n
	return nil
}

func (repeat) Init() error { return nil }

func (r *repeat) Process(input string) (string, error) {
	return strings.Repeat(input, r.times), nil
}

// NewPrefix and NewRepeat are the factories the loader registers.
func NewPrefix() Plugin { return &prefix{} }
func NewRepeat() Plugin { return &repeat{} }
```

### The runnable demo

The demo decodes a JSON config that declares `prefix` then `repeat`, builds the
pipeline, and runs an input through it. `prefix` turns `"hi"` into `">> hi"`, then
`repeat` (times 2) doubles it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"example.com/configload"
)

const raw = `{
  "plugins": [
    {"name": "prefix", "params": {"prefix": ">> "}},
    {"name": "repeat", "params": {"times": 2}}
  ]
}`

func main() {
	var cfg configload.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		log.Fatal(err)
	}

	l := configload.NewLoader()
	l.Register("prefix", configload.NewPrefix)
	l.Register("repeat", configload.NewRepeat)

	pipe, err := l.Build(cfg)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("pipeline:", pipe.Names())
	out, err := pipe.Run("hi")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("output: %q\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
pipeline: [prefix repeat]
output: ">> hi>> hi"
```

### Tests

`TestBuildAggregatesConfigErrors` builds a config with one unknown plugin and one
known plugin given an invalid param, and asserts the returned error mentions both
problems — a single aggregated startup error. It also proves *no* `Init` ran by
registering factories whose `Init` increments a shared counter that must stay zero.
`TestBuildGoldenOrder` builds a valid two-stage config and asserts both the
declared order (`Names()`) and the threaded output of `Run`.

Create `loader_test.go`:

```go
package configload

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// spy is a plugin whose Init increments a shared counter, so a test can prove
// Init did or did not run. Its Validate can be made to fail.
type spy struct {
	name      string
	initCount *atomic.Int64
	failValid bool
}

func (s *spy) Name() string { return s.name }

func (s *spy) Validate(map[string]any) error {
	if s.failValid {
		return errors.New("bad param")
	}
	return nil
}

func (s *spy) Init() error { s.initCount.Add(1); return nil }

func (s *spy) Process(input string) (string, error) { return input, nil }

func TestBuildAggregatesConfigErrors(t *testing.T) {
	t.Parallel()

	var inits atomic.Int64
	l := NewLoader()
	l.Register("good", func() Plugin { return &spy{name: "good", initCount: &inits} })
	l.Register("bad", func() Plugin {
		return &spy{name: "bad", initCount: &inits, failValid: true}
	})

	cfg := Config{Plugins: []PluginConfig{
		{Name: "good"},
		{Name: "bad"},     // fails Validate
		{Name: "missing"}, // unknown factory
	}}

	_, err := l.Build(cfg)
	if err == nil {
		t.Fatal("expected aggregated config error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "missing") {
		t.Fatalf("error %q does not report the unknown plugin", msg)
	}
	if !strings.Contains(msg, "bad param") {
		t.Fatalf("error %q does not report the invalid param", msg)
	}
	if got := inits.Load(); got != 0 {
		t.Fatalf("Init ran %d times despite validation failure, want 0", got)
	}
}

func TestBuildGoldenOrder(t *testing.T) {
	t.Parallel()

	l := NewLoader()
	l.Register("prefix", NewPrefix)
	l.Register("repeat", NewRepeat)

	cfg := Config{Plugins: []PluginConfig{
		{Name: "prefix", Params: map[string]any{"prefix": ">> "}},
		{Name: "repeat", Params: map[string]any{"times": float64(2)}},
	}}

	pipe, err := l.Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := []string{"prefix", "repeat"}; !slices.Equal(pipe.Names(), want) {
		t.Fatalf("pipeline order = %v, want %v", pipe.Names(), want)
	}
	out, err := pipe.Run("hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := ">> hi>> hi"; out != want {
		t.Fatalf("Run output = %q, want %q", out, want)
	}
}

func TestValidateRejectsWrongParamType(t *testing.T) {
	t.Parallel()

	l := NewLoader()
	l.Register("repeat", NewRepeat)

	cfg := Config{Plugins: []PluginConfig{
		{Name: "repeat", Params: map[string]any{"times": "two"}}, // string, not number
	}}
	if _, err := l.Build(cfg); err == nil {
		t.Fatal("expected validation error for non-numeric times")
	}
}
```

## Review

The loader is correct when validation is complete before initialization begins:
`Build` reports every unknown name and every bad parameter in one aggregated error,
and — the property the counter test pins — `Init` runs zero times when any
validation fails, so a bad config never leaves a half-initialized pipeline behind.
That is why `Validate` is a separate method placed before `Init` on the interface;
folding the checks into `Init` would acquire resources before discovering the
config was wrong. The golden test pins the other half: a valid config builds the
pipeline in declared order and `Run` threads output through each stage. Remember
that JSON decodes numbers as `float64`, so a `times` param arrives as `float64(2)`
and the plugin must convert and range-check it — a real trap when validating
config-supplied numbers.

## Resources

- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — decoding config, including numbers as `float64` into `any`.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating every config error into one startup failure.
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — the `Validate`/`Init`/`Process` contract that drives config-based wiring.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-versioned-contract-negotiation.md](08-versioned-contract-negotiation.md) | Next: [../14-interface-based-middleware-chain/00-concepts.md](../14-interface-based-middleware-chain/00-concepts.md)
