# 8. Exercise 29.8: Plugin System

Go's `plugin` package loads `.so` shared objects at runtime on Linux and macOS. This makes it possible to extend an application without recompiling the host. The hard part is not the loading call itself but the constraints that surround it: host and plugin must share the same module path, the same Go toolchain version, and an agreed interface type whose identity is identical in both binaries. This lesson splits the work into a gate-testable `pipeline` library and an offline plugin-host binary so the core logic is fully tested even when `-buildmode=plugin` is unavailable.

## Concepts

### The plugin package model

`go build -buildmode=plugin` compiles a Go package into a `.so` shared object. The resulting file is loaded at runtime with `plugin.Open(path)`, which returns a `*plugin.Plugin`. Exported symbols are retrieved by name with `p.Lookup("SymbolName")`, which returns an `interface{}` that holds a pointer to the exported variable or function.

The host and the plugin execute in the same process after loading. The Go runtime merges their type information, which is why version and module-path alignment is mandatory. If either differs, `plugin.Open` returns an error or panics.

`plugin` only works on Linux and macOS (`darwin`). On Windows or in `-race` mode the package exists but `Open` always returns an error, so always provide a build-constrained fallback or document the limitation.

### The shared interface contract

The `Transformer` interface must be defined in a package that both the host and the plugin import from the same module path. If the host defines the interface inline and the plugin defines it separately, the runtime sees two distinct types even if they have identical method sets. Type assertions will fail silently.

The idiomatic pattern is:

1. Define the interface in a shared internal package (e.g. `pipeline`).
2. Each plugin imports that package and returns a concrete type that satisfies it.
3. Plugin symbols are exported as variables of function type: `var NewTransformer = func() interface{} { return &myImpl{} }`.
4. The host looks up the symbol and asserts `*func() interface{}` (pointer to function), dereferences, calls the function, and asserts the result to the interface.

The pointer indirection is the source of the most common bug in plugin code. `plugin.Lookup` always returns a pointer to the exported symbol, never the symbol value directly.

### Failure modes

- Version mismatch: "plugin was built with a different version of package X" -- rebuild both with `go build` from the same `GOROOT`.
- Missing symbol: `p.Lookup("NewTransformer")` returns `(nil, error)` -- always check the error.
- Wrong type assertion: asserting `func() interface{}` instead of `*func() interface{}` panics -- dereference before calling.
- Module path mismatch: if the plugin's import path for the shared package does not match the host's, the interface types differ and type assertions silently return `ok=false`.
- Windows / unsupported OS: `plugin.Open` returns an error on every call -- document this or use a build tag.
- Unloading: Go plugins cannot be unloaded once opened. Memory and goroutines from a plugin live for the process lifetime.

## Exercises

### Exercise 1: The pipeline library (gate-testable)

The `pipeline` package contains the `Transformer` interface and the `Run` / `RunWithTrace` functions. This is the only code that runs in the automated gate; it requires no `.so` files.

```bash
mkdir -p pipeline cmd/pluginhost plugins/uppercase plugins/reverse
```

Create `pipeline/pipeline.go`:

```go
package pipeline

// Transformer is the contract every plugin must satisfy.
type Transformer interface {
	Name() string
	Transform(input string) string
}

// StepResult records the output after one transformation step.
type StepResult struct {
	Name   string
	Output string
}

// Run applies each transformer in order to input and returns the final result.
func Run(transformers []Transformer, input string) string {
	result := input
	for _, t := range transformers {
		result = t.Transform(result)
	}
	return result
}

// RunWithTrace applies each transformer in order and records intermediate results.
func RunWithTrace(transformers []Transformer, input string) (string, []StepResult) {
	result := input
	steps := make([]StepResult, 0, len(transformers))
	for _, t := range transformers {
		result = t.Transform(result)
		steps = append(steps, StepResult{Name: t.Name(), Output: result})
	}
	return result, steps
}
```

Create `pipeline/pipeline_test.go`:

```go
package pipeline_test

import (
	"strings"
	"testing"

	"plugin-system/pipeline"
)

type upperTransformer struct{}

func (u *upperTransformer) Name() string              { return "uppercase" }
func (u *upperTransformer) Transform(s string) string { return strings.ToUpper(s) }

type reverseTransformer struct{}

func (r *reverseTransformer) Name() string { return "reverse" }
func (r *reverseTransformer) Transform(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

type prefixTransformer struct{ prefix string }

func (p *prefixTransformer) Name() string              { return "prefix" }
func (p *prefixTransformer) Transform(s string) string { return p.prefix + s }

func TestRunEmpty(t *testing.T) {
	result := pipeline.Run(nil, "hello")
	if result != "hello" {
		t.Errorf("empty pipeline: got %q, want %q", result, "hello")
	}
}

func TestRunSingle(t *testing.T) {
	ts := []pipeline.Transformer{&upperTransformer{}}
	if got := pipeline.Run(ts, "hello"); got != "HELLO" {
		t.Errorf("got %q, want %q", got, "HELLO")
	}
}

func TestRunChain(t *testing.T) {
	ts := []pipeline.Transformer{
		&upperTransformer{},
		&reverseTransformer{},
	}
	if got := pipeline.Run(ts, "hello"); got != "OLLEH" {
		t.Errorf("got %q, want %q", got, "OLLEH")
	}
}

func TestRunWithTrace(t *testing.T) {
	ts := []pipeline.Transformer{
		&upperTransformer{},
		&prefixTransformer{prefix: ">>"},
	}
	final, steps := pipeline.RunWithTrace(ts, "go")
	if final != ">>GO" {
		t.Errorf("final: got %q, want %q", final, ">>GO")
	}
	if len(steps) != 2 {
		t.Fatalf("steps: got %d, want 2", len(steps))
	}
	if steps[0].Name != "uppercase" || steps[0].Output != "GO" {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1].Name != "prefix" || steps[1].Output != ">>GO" {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestTransformerIdentity(t *testing.T) {
	noop := &prefixTransformer{prefix: ""}
	ts := []pipeline.Transformer{noop}
	if got := pipeline.Run(ts, "abc"); got != "abc" {
		t.Errorf("got %q, want %q", got, "abc")
	}
}
```

Run the library tests:

```bash
go test -count=1 -race ./pipeline/...
```

### Exercise 2: Plugin implementations (offline)

The sources below are illustrative. Build them with the commands in the Verification section. They are not extracted by the automated gate because they carry no `Create` marker and require `-buildmode=plugin` to compile.

`plugins/uppercase/main.go` -- implements `pipeline.Transformer` as a plugin:

```go
package main

import (
	"strings"

	"plugin-system/pipeline"
)

type uppercaseTransformer struct{}

func (t *uppercaseTransformer) Name() string              { return "uppercase" }
func (t *uppercaseTransformer) Transform(s string) string { return strings.ToUpper(s) }

// NewTransformer is the symbol the host looks up.
// It must be a var of function type, not a func declaration.
var NewTransformer = func() interface{} {
	return pipeline.Transformer(&uppercaseTransformer{})
}
```

`plugins/reverse/main.go`:

```go
package main

import "plugin-system/pipeline"

type reverseTransformer struct{}

func (t *reverseTransformer) Name() string { return "reverse" }
func (t *reverseTransformer) Transform(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

var NewTransformer = func() interface{} {
	return pipeline.Transformer(&reverseTransformer{})
}
```

### Exercise 3: Plugin host (offline)

The host binary loads `.so` files from a directory and passes the resulting `[]pipeline.Transformer` to `pipeline.Run`. This code is illustrative and requires `-buildmode=plugin` output to test end-to-end.

`cmd/pluginhost/loader.go` -- plugin loading logic:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"plugin"

	"plugin-system/pipeline"
)

func loadTransformers(dir string) ([]pipeline.Transformer, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.so"))
	if err != nil {
		return nil, err
	}

	var transformers []pipeline.Transformer
	for _, path := range matches {
		p, err := plugin.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: load %s: %v\n", path, err)
			continue
		}

		sym, err := p.Lookup("NewTransformer")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s missing NewTransformer: %v\n", path, err)
			continue
		}

		// plugin.Lookup returns a pointer to the exported var.
		factory, ok := sym.(*func() interface{})
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: %s NewTransformer has wrong type\n", path)
			continue
		}

		t, ok := (*factory)().(pipeline.Transformer)
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: %s does not implement pipeline.Transformer\n", path)
			continue
		}

		transformers = append(transformers, t)
	}

	return transformers, nil
}
```

`cmd/pluginhost/main.go` -- orchestrator:

```go
package main

import (
	"fmt"
	"os"
	"strings"

	"plugin-system/pipeline"
)

func main() {
	input := "Hello World from Go Plugins"
	if len(os.Args) > 1 {
		input = strings.Join(os.Args[1:], " ")
	}

	transformers, err := loadTransformers("./build/plugins")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading plugins: %v\n", err)
		os.Exit(1)
	}

	if len(transformers) == 0 {
		fmt.Fprintln(os.Stderr, "no plugins found in ./build/plugins/")
		os.Exit(1)
	}

	final, steps := pipeline.RunWithTrace(transformers, input)
	fmt.Printf("Input: %s\n\n", input)
	for _, s := range steps {
		fmt.Printf("[%s] => %s\n", s.Name, s.Output)
	}
	fmt.Printf("\nFinal: %s\n", final)
}
```

## Common Mistakes

Wrong: Asserting `sym.(func() interface{})` instead of `sym.(*func() interface{})`.
What happens: the type assertion panics at runtime because `plugin.Lookup` always returns a pointer to the exported variable.
Fix: assert `*func() interface{}`, then call `(*factory)()` to invoke the factory.

Wrong: Defining the `Transformer` interface separately in both host and plugin instead of importing a shared package.
What happens: even if the method sets are identical, Go's type system treats them as different types. The assertion `result.(pipeline.Transformer)` returns `ok=false` silently.
Fix: put the interface in one package (`plugin-system/pipeline`) and import it in both places.

Wrong: Building plugins with a different Go toolchain version than the host.
What happens: `plugin.Open` returns an error: "plugin was built with a different version of package runtime".
Fix: compile host and all plugins with the exact same `go build` invocation from the same `GOROOT`. Pin the toolchain version in `go.mod` with `toolchain go1.xx.x`.

## Verification

Gate-testable part (runs in CI without `.so` files):

```bash
gofmt -l ./pipeline/
go vet ./pipeline/...
go build ./pipeline/...
go test -count=1 -race ./pipeline/...
```

Offline part (Linux/macOS only, requires `-buildmode=plugin`):

```bash
mkdir -p build/plugins
go build -buildmode=plugin -o build/plugins/uppercase.so ./plugins/uppercase/
go build -buildmode=plugin -o build/plugins/reverse.so   ./plugins/reverse/
go build -o build/host ./cmd/pluginhost/
./build/host "Go plugins are powerful"
```

The pipeline library tests pass in the automated gate. Plugin loading is validated offline because `-buildmode=plugin` is not supported in the standard `go test` flow.

## Summary

- `go build -buildmode=plugin` produces `.so` shared objects loadable at runtime with `plugin.Open`.
- `plugin.Lookup` returns a pointer to the exported symbol; always assert `*T`, not `T`.
- Define shared interfaces in one package imported by both host and plugins; never duplicate them.
- Host and plugins must be built with the same Go toolchain version and share the same module root.
- The `plugin` package is only supported on Linux and macOS; plan for a no-op fallback on other platforms.
- Separating the core pipeline logic into a testable library lets the gate verify behavior without `.so` files.

## What's Next

Next: [Exercise 29.9: Building a CLI Code Generator](../09-building-a-cli-code-generator/09-building-a-cli-code-generator.md).

## Resources

- [plugin package](https://pkg.go.dev/plugin)
- [Go build modes](https://pkg.go.dev/cmd/go#hdr-Build_modes)
- [plugin package warnings](https://pkg.go.dev/plugin#hdr-Warnings)
- [Go toolchain directive](https://go.dev/doc/toolchain)
