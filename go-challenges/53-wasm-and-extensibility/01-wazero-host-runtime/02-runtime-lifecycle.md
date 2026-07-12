# Exercise 2: Runtime config and instance lifecycle

Running Wasm in a server is a lifecycle-management problem before it is a
performance problem. This exercise builds a `Host` that chooses its execution
backend explicitly, compiles a module exactly once, hands out many isolated
instances from that single compiled module, introspects its exports, and tears
everything down in the correct order.

This module is fully self-contained: its own `go mod init`, its own embedded
guest, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
wasmhost/                    independent module: example.com/wasmhost
  go.mod                     go 1.26; requires github.com/tetratelabs/wazero
  host.go                    type Host, Backend; New, NewAddHost, Instantiate,
                             ExportedFunctionNames, IsClosed, Close; ErrEngineClosed
  cmd/
    demo/
      main.go                compile-once, instantiate-many, then close
  host_test.go              many instances called concurrently; both backends agree;
                             closed-host sentinel; Example
```

- Files: `host.go`, `cmd/demo/main.go`, `host_test.go`.
- Implement: a `Host` built with `NewRuntimeWithConfig` over `NewRuntimeConfigCompiler`/`NewRuntimeConfigInterpreter`, compiling once with `CompileModule` and instantiating many with `InstantiateModule` under distinct `WithName`s; `ExportedFunctionNames` over `CompiledModule.ExportedFunctions`; ordered, idempotent `Close`.
- Test: N instances called concurrently to prove isolation; a backend-parity test producing identical results on compiler and interpreter; a call after `Close` asserted against `ErrEngineClosed` with `errors.Is`; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/53-wasm-and-extensibility/01-wazero-host-runtime/02-runtime-lifecycle/cmd/demo
cd go-solutions/53-wasm-and-extensibility/01-wazero-host-runtime/02-runtime-lifecycle
go mod edit -go=1.26
go get github.com/tetratelabs/wazero@latest
```

### Choosing the backend explicitly

`wazero.NewRuntime` picks the compiler where the architecture supports it and
falls back to the interpreter elsewhere. That default is fine for a program that
does not care, but a server usually does care, so this `Host` selects the backend
by hand through `NewRuntimeWithConfig`. `NewRuntimeConfigCompiler` demands native
code generation and near-native speed; `NewRuntimeConfigInterpreter` runs the
bytecode directly and works in any environment. The two produce identical results
for the same input — the backend-parity test proves exactly that — so the choice
is about the performance envelope, not correctness. Expose it as a `Backend` enum
so the caller states intent at construction time.

### Compile once, instantiate many

`New` calls `CompileModule` a single time and stores the resulting
`CompiledModule`. Every later `Instantiate` calls `InstantiateModule` against that
stored compiled module, so the expensive decode-and-compile work is paid once no
matter how many instances you create. This is the core discipline of embedding
Wasm in a server: compilation is amortized at startup; instantiation is the cheap
per-request or per-tenant step.

Each instance must have a distinct module name, so `Instantiate` threads a `name`
into `NewModuleConfig().WithName(name)`. Distinct names both prevent a
name-collision error when two instances coexist and give you something legible in
logs and traces (`mod.Name()` returns it). The concurrency test creates eight
instances and calls each from its own goroutine with different arguments; because
each instance carries its own linear memory and globals, the calls cannot
interfere, and the results come back independent. That is the isolation guarantee
made testable.

### Introspection without an instance

`ExportedFunctionNames` reads `CompiledModule.ExportedFunctions()`, which returns
a `map[string]api.FunctionDefinition` describing what the module exports —
available from the compiled module alone, before and without instantiating
anything. This is how a plugin host discovers a module's surface at load time
(does it export the entry point I require?) rather than discovering it by calling
into an instance and failing. The helper sorts the names so callers and the
`Example` get a stable order.

### Ordered, idempotent teardown

`Close` releases things in the order the runtime expects when you manage them by
hand: first every instance it handed out, then the `CompiledModule`, then the
`Runtime`. It records the instances as it creates them so it can close them
explicitly, collects any errors with `errors.Join`, and flips a `closed` flag
under a mutex so a second `Close` is a clean no-op — which matters because server
shutdown paths often call `Close` from more than one place. Once closed,
`Instantiate` returns `ErrEngineClosed` rather than touching a torn-down runtime.
`IsClosed` exposes the state for callers that want to check before acting.

Create `host.go`:

```go
// Package host demonstrates the wazero runtime lifecycle: choose a backend
// explicitly, compile a module once, instantiate it many times as isolated
// instances, introspect its exports, and tear everything down in order.
package host

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ErrEngineClosed is returned when an operation is attempted on a closed Host.
var ErrEngineClosed = errors.New("host: engine is closed")

// Backend selects the wazero execution engine.
type Backend int

const (
	// Compiler emits native machine code (fast, needs amd64/arm64).
	Compiler Backend = iota
	// Interpreter executes bytecode directly (portable, slower, no codegen).
	Interpreter
)

// addWasm is a minimal hand-assembled module exporting add: (i32,i32)->i32.
//
//	(module
//	  (func (export "add") (param i32 i32) (result i32)
//	    local.get 0
//	    local.get 1
//	    i32.add))
var addWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x07, 0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x61, 0x64, 0x64, 0x00, 0x00,
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b,
}

// Host owns one runtime and one compiled module, from which it hands out many
// isolated instances.
type Host struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule

	mu        sync.Mutex
	closed    bool
	instances []api.Module
}

// New builds a runtime on the chosen backend and compiles wasm exactly once.
func New(ctx context.Context, wasm []byte, backend Backend) (*Host, error) {
	var cfg wazero.RuntimeConfig
	switch backend {
	case Interpreter:
		cfg = wazero.NewRuntimeConfigInterpreter()
	default:
		cfg = wazero.NewRuntimeConfigCompiler()
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("host: compile: %w", err)
	}
	return &Host{rt: rt, compiled: compiled}, nil
}

// NewAddHost is a convenience constructor over the embedded add module.
func NewAddHost(ctx context.Context, backend Backend) (*Host, error) {
	return New(ctx, addWasm, backend)
}

// Instantiate produces a fresh isolated instance with a distinct module name.
// The instance is tracked so Close can tear it down in order.
func (h *Host) Instantiate(ctx context.Context, name string) (api.Module, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrEngineClosed
	}
	mod, err := h.rt.InstantiateModule(ctx, h.compiled,
		wazero.NewModuleConfig().WithName(name))
	if err != nil {
		return nil, fmt.Errorf("host: instantiate %q: %w", name, err)
	}
	h.instances = append(h.instances, mod)
	return mod, nil
}

// ExportedFunctionNames lists the module's exported function names, sorted. This
// reads the CompiledModule, so it does not require an instance.
func (h *Host) ExportedFunctionNames() []string {
	defs := h.compiled.ExportedFunctions()
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsClosed reports whether Close has been called.
func (h *Host) IsClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Close tears down in order: instances, then the compiled module, then the
// runtime. It is idempotent: a second call is a no-op.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true

	var errs []error
	for _, m := range h.instances {
		if err := m.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	h.instances = nil
	if err := h.compiled.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.rt.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo lists the module's exports, creates three named instances from the one
compiled module, calls each, then closes the host and shows that a post-close
`Instantiate` yields the sentinel. The `host` package is imported under its
package name even though the module path is `example.com/wasmhost`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tetratelabs/wazero/api"

	host "example.com/wasmhost"
)

func main() {
	ctx := context.Background()

	h, err := host.NewAddHost(ctx, host.Compiler)
	if err != nil {
		log.Fatal(err)
	}
	defer h.Close(ctx)

	fmt.Println("exports:", h.ExportedFunctionNames())

	// Compile-once, instantiate-many: three isolated instances from one module.
	for i := range 3 {
		mod, err := h.Instantiate(ctx, fmt.Sprintf("tenant-%d", i))
		if err != nil {
			log.Fatal(err)
		}
		res, err := mod.ExportedFunction("add").Call(ctx, api.EncodeI32(int32(i)), api.EncodeI32(10))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: add(%d, 10) = %d\n", mod.Name(), i, api.DecodeI32(res[0]))
	}

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println("closed:", h.IsClosed())

	if _, err := h.Instantiate(ctx, "after-close"); err != nil {
		fmt.Println("instantiate after close:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
exports: [add]
tenant-0: add(0, 10) = 10
tenant-1: add(1, 10) = 11
tenant-2: add(2, 10) = 12
closed: true
instantiate after close: host: engine is closed
```

### Tests

`TestInstantiateMany` creates eight instances and calls each from its own
goroutine with distinct arguments, asserting every result. Because the instances
are independent, the concurrent calls are safe and their results do not bleed into
one another; running under `-race` confirms there is no shared mutable state on
the call path. `TestExportedFunctionNames` asserts the compiled module reports the
`add` export. `TestBackendsAgree` runs the same call on both the compiler and the
interpreter and asserts identical results, demonstrating backend determinism.
`TestClosedHost` closes the host twice (the second must be a no-op) and asserts a
subsequent `Instantiate` returns `ErrEngineClosed` through `errors.Is`.

Create `host_test.go`:

```go
package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

func addOn(ctx context.Context, mod api.Module, a, b int32) (int32, error) {
	res, err := mod.ExportedFunction("add").Call(ctx, api.EncodeI32(a), api.EncodeI32(b))
	if err != nil {
		return 0, err
	}
	return api.DecodeI32(res[0]), nil
}

func TestInstantiateMany(t *testing.T) {
	t.Parallel()
	h, err := NewAddHost(t.Context(), Compiler)
	if err != nil {
		t.Fatalf("NewAddHost: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) })

	const n = 8
	mods := make([]api.Module, n)
	for i := range n {
		m, err := h.Instantiate(t.Context(), fmt.Sprintf("inst-%d", i))
		if err != nil {
			t.Fatalf("Instantiate %d: %v", i, err)
		}
		mods[i] = m
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := addOn(t.Context(), mods[i], int32(i), 100)
			if err != nil {
				t.Errorf("inst-%d call: %v", i, err)
				return
			}
			if want := int32(i) + 100; got != want {
				t.Errorf("inst-%d add = %d, want %d", i, got, want)
			}
		}()
	}
	wg.Wait()
}

func TestExportedFunctionNames(t *testing.T) {
	t.Parallel()
	h, err := NewAddHost(t.Context(), Interpreter)
	if err != nil {
		t.Fatalf("NewAddHost: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) })

	if names := h.ExportedFunctionNames(); !slices.Contains(names, "add") {
		t.Fatalf("ExportedFunctionNames() = %v, want to contain %q", names, "add")
	}
}

func TestBackendsAgree(t *testing.T) {
	t.Parallel()
	for _, backend := range []Backend{Compiler, Interpreter} {
		h, err := NewAddHost(t.Context(), backend)
		if err != nil {
			t.Fatalf("NewAddHost(%v): %v", backend, err)
		}
		mod, err := h.Instantiate(t.Context(), "x")
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}
		got, err := addOn(t.Context(), mod, 20, 22)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if got != 42 {
			t.Errorf("backend %v add(20,22) = %d, want 42", backend, got)
		}
		h.Close(context.Background())
	}
}

func TestClosedHost(t *testing.T) {
	t.Parallel()
	h, err := NewAddHost(t.Context(), Compiler)
	if err != nil {
		t.Fatalf("NewAddHost: %v", err)
	}
	if err := h.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(t.Context()); err != nil {
		t.Fatalf("second Close should be a no-op, got: %v", err)
	}
	if !h.IsClosed() {
		t.Fatal("IsClosed() = false after Close")
	}
	if _, err := h.Instantiate(t.Context(), "late"); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("Instantiate after Close = %v, want ErrEngineClosed", err)
	}
}

func ExampleHost_ExportedFunctionNames() {
	ctx := context.Background()
	h, _ := NewAddHost(ctx, Interpreter)
	defer h.Close(ctx)
	fmt.Println(h.ExportedFunctionNames())
	// Output: [add]
}
```

## Review

The lifecycle is correct when compilation happens once and instantiation happens
per unit of work. If you find yourself calling `CompileModule` (or the
`InstantiateWithConfig` convenience that hides it) inside the per-request path,
you have collapsed the two steps and reintroduced the compile cost you were trying
to amortize; keep `CompileModule` in `New` and only `InstantiateModule` in
`Instantiate`.

The teardown is the other place bugs hide. Closing the runtime alone will release
everything it owns, but when you track and close instances explicitly, do it
before the compiled module and the compiled module before the runtime, and make
the whole thing idempotent — a server that calls `Close` from a `defer` and again
from a signal handler must not error on the second call. Finally, give every
instance a distinct `WithName`: two live instances sharing a name collide at
instantiation. Run `go test -race` to confirm the concurrent per-instance calls
carry no shared mutable state.

## Resources

- [wazero package reference](https://pkg.go.dev/github.com/tetratelabs/wazero) — `NewRuntimeWithConfig`, `NewRuntimeConfigCompiler`, `NewRuntimeConfigInterpreter`, `CompiledModule.ExportedFunctions`, `CompiledModule.Close`.
- [wazero/api package](https://pkg.go.dev/github.com/tetratelabs/wazero/api) — `Module`, `FunctionDefinition`, `Module.Name`, `Module.Close`.
- [wazero.io documentation](https://wazero.io/docs/) — the compiler versus interpreter engines and when each applies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-module-loader.md](01-module-loader.md) | Next: [03-execution-limits.md](03-execution-limits.md)
