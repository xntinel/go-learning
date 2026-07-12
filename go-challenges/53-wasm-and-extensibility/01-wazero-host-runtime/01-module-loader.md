# Exercise 1: A compile-once module loader

The first thing you build when you embed Wasm is a small facade that turns raw
`.wasm` bytes into typed, callable Go functions. This exercise builds an `Engine`
type that compiles a module once, instantiates it, and exposes numeric call
helpers that encode and decode arguments correctly over the uint64 stack ABI.

This module is fully self-contained. It begins with its own `go mod init`, embeds
its guest as a verified `[]byte`, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
engine/                      independent module: example.com/engine
  go.mod                     go 1.26; requires github.com/tetratelabs/wazero
  engine.go                  type Engine; New, NewAddEngine, CallI32/I64/F64, Close
                             sentinels ErrExportNotFound, ErrNoResult; embedded addWasm
  cmd/
    demo/
      main.go                loads the embedded add module and calls it
  engine_test.go             table-driven add calls; missing-export sentinel; Example
```

- Files: `engine.go`, `cmd/demo/main.go`, `engine_test.go`.
- Implement: an `Engine` that wraps a `wazero.Runtime`, compiles bytes with `CompileModule`, instantiates with `InstantiateModule`, and exposes `CallI32`/`CallI64`/`CallF64` plus `Close`.
- Test: table-driven `add` calls (including negatives) decoded with `api.DecodeI32`; a missing export asserted against `ErrExportNotFound` with `errors.Is`; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...`

Set up the module. wazero requires a recent toolchain, so pin the language
version and add the dependency:

```bash
mkdir -p go-solutions/53-wasm-and-extensibility/01-wazero-host-runtime/01-module-loader/cmd/demo
cd go-solutions/53-wasm-and-extensibility/01-wazero-host-runtime/01-module-loader
go mod edit -go=1.26
go get github.com/tetratelabs/wazero@latest
```

### Why the guest is embedded bytes

A lesson that depends on a Wasm toolchain (TinyGo, `wat2wasm`) to produce its
guest would not build offline, so the guest here is a minimal `add` module
assembled by hand and embedded as a `[]byte`. The bytes are the WebAssembly
binary encoding of this text format:

```wat
(module
  (func (export "add") (param i32 i32) (result i32)
    local.get 0
    local.get 1
    i32.add))
```

Every byte is accounted for in a comment next to the slice, so you can see the
magic number, the type section describing `(i32,i32)->i32`, the export naming
`add`, and the code section. You never have to trust an opaque blob: it is a
transparent, minimal, valid module that compiles and runs with no external
dependency.

### The three-object flow, made concrete

`New` performs the full lifecycle once. `wazero.NewRuntime(ctx)` creates the
long-lived runtime that owns the compilation engine. `Runtime.CompileModule`
decodes and compiles the bytes into a `CompiledModule` — the expensive step.
`Runtime.InstantiateModule` then wires up one isolated `api.Module` instance from
that compiled module, using a default `wazero.NewModuleConfig()`. The `Engine`
holds the runtime and the instance; `Close` delegates to `Runtime.Close`, which
releases the instance, the compiled code, and the runtime together. On any error
during construction, `New` closes the runtime before returning so a half-built
engine never leaks.

### The encode/decode discipline in the call helpers

The helpers are where the uint64 stack ABI is respected. `api.Function.Call`
takes `...uint64` and returns `[]uint64`; the numbers are raw 64-bit stack words,
not Go integers. `CallI32` therefore maps each `int32` argument through
`api.EncodeI32` before the call and decodes `results[0]` through `api.DecodeI32`
after. This is exactly what makes the negative-number row in the test pass: a
naive `uint64(int32(-5))` would sign-extend to a value the guest reads as a huge
positive i32, whereas `api.EncodeI32(-5)` places the correct 32-bit two's
complement in the low word. `CallF64` uses `api.EncodeF64`/`DecodeF64`, which
move the IEEE-754 bit pattern rather than performing a lossy numeric cast.

Two failure modes are turned into sentinel errors. If the requested export does
not exist, `ExportedFunction` returns `nil`, and the helper returns
`ErrExportNotFound` wrapped with `%w` so callers can match it with `errors.Is`.
If the function runs but returns no value, indexing `results[0]` would panic, so
the helper checks `len(res)` and returns `ErrNoResult` instead. Respecting the
results-slice contract is the difference between a robust loader and a panic on
the first zero-result export.

Create `engine.go`:

```go
// Package engine embeds a wazero WebAssembly runtime and exposes an add module
// compiled once and instantiated for use, with typed numeric call helpers.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ErrExportNotFound is returned when a requested exported function is absent.
var ErrExportNotFound = errors.New("engine: exported function not found")

// ErrNoResult is returned when an exported function yields no result value but
// the caller expected one.
var ErrNoResult = errors.New("engine: function returned no result")

// addWasm is a minimal, hand-assembled WebAssembly module with a single export.
// The WAT it corresponds to is:
//
//	(module
//	  (func (export "add") (param i32 i32) (result i32)
//	    local.get 0
//	    local.get 1
//	    i32.add))
//
// Embedding the bytes keeps the exercise fully offline: no toolchain, no files.
var addWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
	0x01, 0x07, 0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f, // type: (i32,i32)->i32
	0x03, 0x02, 0x01, 0x00, // function: one func of type 0
	0x07, 0x07, 0x01, 0x03, 0x61, 0x64, 0x64, 0x00, 0x00, // export "add" -> func 0
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b, // code
}

// Engine wraps a long-lived wazero runtime and one instantiated module.
type Engine struct {
	rt  wazero.Runtime
	mod api.Module
}

// New compiles wasm once and instantiates a single module from it. The returned
// Engine owns the runtime; call Close to release it.
func New(ctx context.Context, wasm []byte) (*Engine, error) {
	rt := wazero.NewRuntime(ctx)
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("engine: compile: %w", err)
	}
	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("engine: instantiate: %w", err)
	}
	return &Engine{rt: rt, mod: mod}, nil
}

// NewAddEngine is a convenience constructor over the embedded add module.
func NewAddEngine(ctx context.Context) (*Engine, error) {
	return New(ctx, addWasm)
}

// call looks up an export and invokes it with pre-encoded stack words.
func (e *Engine) call(ctx context.Context, name string, params ...uint64) ([]uint64, error) {
	fn := e.mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("%q: %w", name, ErrExportNotFound)
	}
	res, err := fn.Call(ctx, params...)
	if err != nil {
		return nil, fmt.Errorf("engine: call %q: %w", name, err)
	}
	return res, nil
}

// CallI32 invokes an i32-returning export, encoding every argument as an i32.
func (e *Engine) CallI32(ctx context.Context, name string, args ...int32) (int32, error) {
	params := make([]uint64, len(args))
	for i, a := range args {
		params[i] = api.EncodeI32(a)
	}
	res, err := e.call(ctx, name, params...)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("%q: %w", name, ErrNoResult)
	}
	return api.DecodeI32(res[0]), nil
}

// CallI64 invokes an i64-returning export, encoding every argument as an i64.
func (e *Engine) CallI64(ctx context.Context, name string, args ...int64) (int64, error) {
	params := make([]uint64, len(args))
	for i, a := range args {
		params[i] = api.EncodeI64(a)
	}
	res, err := e.call(ctx, name, params...)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("%q: %w", name, ErrNoResult)
	}
	return int64(res[0]), nil
}

// CallF64 invokes an f64-returning export, encoding every argument as an f64.
func (e *Engine) CallF64(ctx context.Context, name string, args ...float64) (float64, error) {
	params := make([]uint64, len(args))
	for i, a := range args {
		params[i] = api.EncodeF64(a)
	}
	res, err := e.call(ctx, name, params...)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("%q: %w", name, ErrNoResult)
	}
	return api.DecodeF64(res[0]), nil
}

// Close releases the runtime and everything it created. It is safe to call once.
func (e *Engine) Close(ctx context.Context) error {
	return e.rt.Close(ctx)
}
```

Note that `CallI64` decodes its result with a plain `int64(res[0])`: an i64 value
occupies the full 64-bit stack word already, so the conversion is exact and no
`api.DecodeI64` is needed. The encode helper is still used on the way in for
symmetry and to keep the ABI discipline explicit.

### The runnable demo

The demo builds the add engine, calls it over a few argument pairs — including a
negative — and then deliberately requests a function the module does not export
to show the sentinel error surfacing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/engine"
)

func main() {
	ctx := context.Background()

	eng, err := engine.NewAddEngine(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer eng.Close(ctx)

	for _, p := range [][2]int32{{2, 3}, {-5, 8}, {1000, 337}} {
		sum, err := eng.CallI32(ctx, "add", p[0], p[1])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("add(%d, %d) = %d\n", p[0], p[1], sum)
	}

	if _, err := eng.CallI32(ctx, "multiply", 2, 3); err != nil {
		fmt.Println("expected miss:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add(2, 3) = 5
add(-5, 8) = 3
add(1000, 337) = 1337
expected miss: "multiply": engine: exported function not found
```

### Tests

`TestCallI32` is table-driven and includes a negative-argument row precisely to
prove the encoding is correct — a loader that hand-casts arguments fails
`with negative` and `both negative` while passing the positive rows, which is the
signature of the ABI bug. `TestUnknownExport` asserts that requesting an absent
function returns `ErrExportNotFound` through `errors.Is`, confirming the wrap with
`%w`. The `Example` calls `add(-5, 8)` and prints `3`, auto-verified by `go test`.
Each test derives its context from `t.Context()`.

Create `engine_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCallI32(t *testing.T) {
	t.Parallel()
	eng, err := NewAddEngine(t.Context())
	if err != nil {
		t.Fatalf("NewAddEngine: %v", err)
	}
	t.Cleanup(func() { eng.Close(t.Context()) })

	tests := []struct {
		name string
		a, b int32
		want int32
	}{
		{"positive", 2, 3, 5},
		{"with negative", -5, 8, 3},
		{"both negative", -4, -6, -10},
		{"zero", 0, 0, 0},
		{"large", 1000, 337, 1337},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := eng.CallI32(t.Context(), "add", tc.a, tc.b)
			if err != nil {
				t.Fatalf("CallI32(add, %d, %d): %v", tc.a, tc.b, err)
			}
			if got != tc.want {
				t.Errorf("add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestUnknownExport(t *testing.T) {
	t.Parallel()
	eng, err := NewAddEngine(t.Context())
	if err != nil {
		t.Fatalf("NewAddEngine: %v", err)
	}
	t.Cleanup(func() { eng.Close(t.Context()) })

	_, err = eng.CallI32(t.Context(), "subtract", 1, 2)
	if !errors.Is(err, ErrExportNotFound) {
		t.Fatalf("CallI32(subtract) error = %v, want ErrExportNotFound", err)
	}
}

func ExampleEngine_call() {
	ctx := context.Background()
	eng, err := NewAddEngine(ctx)
	if err != nil {
		panic(err)
	}
	defer eng.Close(ctx)

	sum, _ := eng.CallI32(ctx, "add", -5, 8)
	fmt.Println(sum)
	// Output: 3
}
```

## Review

The loader is correct when the encode/decode path is honored end to end: the
negative-argument rows only pass because `api.EncodeI32` and `api.DecodeI32` place
and read the 32-bit two's complement rather than sign-extending a raw cast. If you
see `with negative` fail while `positive` passes, the ABI is being violated
somewhere — that is the tell.

The mistakes to avoid are the ones the sentinels guard. Do not index `results[0]`
without checking `len(res)`; a zero-result export would panic, which is why
`ErrNoResult` exists. Do not swallow a missing export as a generic error; wrapping
`ErrExportNotFound` with `%w` is what lets a caller distinguish "you asked for a
function that is not there" from "the function trapped". And do not call
`InstantiateWithConfig` per invocation to save a line — this engine compiles once
in `New` and reuses the instance, which is the whole point of separating compile
from instantiate. Run `go test -race` to confirm the helpers are clean under the
race detector.

## Resources

- [wazero package reference](https://pkg.go.dev/github.com/tetratelabs/wazero) — `NewRuntime`, `CompileModule`, `InstantiateModule`, `NewModuleConfig`, `Runtime.Close`.
- [wazero/api package](https://pkg.go.dev/github.com/tetratelabs/wazero/api) — `Module`, `Function.Call`, and the `EncodeI32`/`DecodeI32`/`EncodeF64`/`DecodeF64` helpers.
- [wazero.io documentation](https://wazero.io/docs/) — how the runtime, compiled modules, and instances relate.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-runtime-lifecycle.md](02-runtime-lifecycle.md)
