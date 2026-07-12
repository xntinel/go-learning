# Exercise 3: The Sandboxed Runner — Memory Caps, Deadlines, and Isolation

This is the on-the-job artifact: a host that compiles an untrusted plugin once and
invokes it many times under a resource budget. It bounds memory, bounds wall-clock
time, and instantiates a fresh instance per call so one plugin's fault or state
never touches another's. A runaway plugin becomes one failed request, not an OOM
or a wedged goroutine.

This module is fully self-contained. It begins with its own `go mod init`, embeds
its guest plugins as verified byte slices, and ships its own demo and tests.
Nothing here imports any other exercise.

## What you'll build

```text
runner/                     independent module: example.com/runner
  go.mod                    go 1.26; requires github.com/tetratelabs/wazero
  runner.go                 Host, Plugin, Config; NewHost, Load, Invoke, Close;
                            mapError; sentinels ErrPluginTimeout, ErrPluginTrapped
  cmd/
    demo/
      main.go               runs a looping plugin and a value plugin, prints outcomes
  runner_test.go            timeout, success, isolation, error-mapping, close, memory-cap
```

- Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: a `Host` whose runtime uses `WithMemoryLimitPages` and `WithCloseOnContextDone(true)`; `Load` compiles once; `Plugin.Invoke` instantiates fresh per call under `context.WithTimeout` and maps termination to `ErrPluginTimeout`/`ErrPluginTrapped`.
- Test: a looping guest under a deadline maps to `ErrPluginTimeout` (and wraps a `*sys.ExitError` with `ExitCodeDeadlineExceeded`); a value guest returns its result; isolation across a fault; direct `mapError` cases; an oversized-memory plugin rejected at `Load`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/53-wasm-and-extensibility/03-wasm-plugin-system/03-sandboxed-plugin-runner/cmd/demo
cd go-solutions/53-wasm-and-extensibility/03-wasm-plugin-system/03-sandboxed-plugin-runner
go get github.com/tetratelabs/wazero@latest
```

### The three guarantees, and where each is enforced

**Bounded memory.** The runtime config sets `WithMemoryLimitPages(n)`. A page is
65,536 bytes, so `n` pages is `n * 64 KiB`; passing a byte count here is the
classic bug. wazero enforces the cap at `CompileModule`: a plugin declaring more
linear memory than the limit is rejected at `Load`, before it can ever run. That
is why the memory-cap test asserts `Load` returns an error, not `Invoke`.

**Bounded CPU / wall time.** A guest with no imports can still spin forever — an
empty `loop br 0` never yields and never blocks, so nothing about the sandbox
stops it. Two things together interrupt it: the runtime must be built with
`WithCloseOnContextDone(true)`, and each call must run under a
`context.WithTimeout`. With the option off, the deadline expires and the guest
keeps running; with it on, hitting the deadline closes the instance and returns a
`*sys.ExitError`. Both are required — the option is inert without a deadline, and
the deadline is inert without the option.

**Per-invocation isolation.** `CompileModule` is expensive and safe to reuse;
`InstantiateModule` is cheaper and produces an instance with its own fresh linear
memory. `Load` compiles once; `Invoke` instantiates a fresh instance every call
and closes it after. No state survives between calls, and a fault in one instance
— a trap, a deadline kill — cannot reach a sibling instance or the host. That is
what lets the isolation test run a looping plugin to its death and then run a
well-behaved plugin on the *same* `Host` and still get the right answer.

### Termination: map it, never leak it

`Invoke` collapses every abnormal termination into a package sentinel. The
subtlety is in `mapError`. When the deadline fires with `WithCloseOnContextDone`,
`Call` returns a `*sys.ExitError` whose `ExitCode()` is
`sys.ExitCodeDeadlineExceeded` (and `errors.Is(err, context.DeadlineExceeded)`
also holds); a cancellation gives `sys.ExitCodeContextCanceled`. A trap for any
other reason — `unreachable`, out-of-bounds, divide-by-zero — returns a plain
wazero error that is *not* a `*sys.ExitError`. So `mapError` first checks for a
`*sys.ExitError` and branches on its exit code, then falls back to
`context.DeadlineExceeded`, and treats everything else as a trap.

The wrap uses two `%w` verbs — `fmt.Errorf("%w: %w", ErrPluginTimeout, err)` — so
the returned error satisfies *both* `errors.Is(err, ErrPluginTimeout)` for
callers who only care about the category *and* `errors.As(err, &exit)` for a
caller (or a log line) that wants the raw exit code. The sentinel is the public
contract; the original error stays in the chain for diagnostics but never as the
type a caller must know.

### Why Invoke closes with a detached context

`Invoke` derives a `callCtx` with the deadline and runs the guest under it. The
`defer mod.Close(...)` that cleans up the instance must not use `callCtx`: by the
time the defer runs, `callCtx` may already be past its deadline, and closing under
an expired context is the wrong signal. `context.WithoutCancel(ctx)` gives a
context that carries the parent's values but is never cancelled, so cleanup always
runs cleanly whether the call succeeded or timed out. (On a deadline, the runtime
has already auto-closed the instance; the explicit `Close` is then a harmless,
idempotent no-op — but you never rely on the auto-close for the success path.)

Create `runner.go`:

```go
// Package runner is a sandboxed Wasm plugin host: it compiles a plugin once and
// invokes it many times, each invocation in a fresh instance under a memory cap
// and a wall-clock deadline, mapping every abnormal termination to a sentinel.
package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// Sentinel errors, wrapped with %w so callers match them via errors.Is.
var (
	ErrPluginTimeout = errors.New("plugin exceeded its deadline")
	ErrPluginTrapped = errors.New("plugin trapped")
	ErrNoEntrypoint  = errors.New("plugin has no entrypoint export")
)

const (
	defaultMemoryPages uint32        = 4 // 256 KiB
	defaultTimeout     time.Duration = 100 * time.Millisecond
	defaultEntrypoint  string        = "run"
)

// Config tunes the resource budget and entrypoint. Zero fields take defaults.
type Config struct {
	MemoryLimitPages uint32
	CallTimeout      time.Duration
	Entrypoint       string
}

// Host owns a wazero runtime configured to cap guest memory and to interrupt a
// guest when its call context is done. Plugins compiled on it share the runtime
// but run as independent instances.
type Host struct {
	rt      wazero.Runtime
	timeout time.Duration
	entry   string
}

// NewHost builds a Host with the given budget. It caps memory with
// WithMemoryLimitPages and enables WithCloseOnContextDone so a deadline actually
// interrupts a running guest.
func NewHost(ctx context.Context, cfg Config) *Host {
	pages := cmp.Or(cfg.MemoryLimitPages, defaultMemoryPages)
	rc := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(pages).
		WithCloseOnContextDone(true)
	return &Host{
		rt:      wazero.NewRuntimeWithConfig(ctx, rc),
		timeout: cmp.Or(cfg.CallTimeout, defaultTimeout),
		entry:   cmp.Or(cfg.Entrypoint, defaultEntrypoint),
	}
}

// Close releases the runtime and every plugin compiled on it.
func (h *Host) Close(ctx context.Context) error {
	return h.rt.Close(ctx)
}

// Plugin is a compiled-once module. Invoke instantiates a fresh instance per call.
type Plugin struct {
	host     *Host
	compiled wazero.CompiledModule
}

// Load compiles plugin bytes once. A plugin whose declared memory exceeds the
// host's page limit is rejected here, at load time.
func (h *Host) Load(ctx context.Context, wasm []byte) (*Plugin, error) {
	compiled, err := h.rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compile plugin: %w", err)
	}
	return &Plugin{host: h, compiled: compiled}, nil
}

// instantiate creates a fresh instance under the given (deadline-bearing) context.
// It is factored out so tests can observe the instance directly.
func (p *Plugin) instantiate(ctx context.Context) (api.Module, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions()
	return p.host.rt.InstantiateModule(ctx, p.compiled, cfg)
}

// Invoke runs the plugin's entrypoint in a fresh instance under a per-call
// deadline, then closes the instance. A deadline hit becomes ErrPluginTimeout; a
// trap becomes ErrPluginTrapped; a clean return yields the raw result stack.
func (p *Plugin) Invoke(ctx context.Context) ([]uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, p.host.timeout)
	defer cancel()

	mod, err := p.instantiate(callCtx)
	if err != nil {
		return nil, mapError(err)
	}
	// Close under a context detached from the deadline so cleanup always runs.
	defer mod.Close(context.WithoutCancel(ctx))

	fn := mod.ExportedFunction(p.host.entry)
	if fn == nil {
		return nil, fmt.Errorf("%q: %w", p.host.entry, ErrNoEntrypoint)
	}
	res, err := fn.Call(callCtx)
	if err != nil {
		return nil, mapError(err)
	}
	return res, nil
}

// mapError collapses wazero and sys termination errors into package sentinels,
// keeping the original error in the chain (%w) so a caller can still inspect the
// sys.ExitError and its exit code.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var exit *sys.ExitError
	switch {
	case errors.As(err, &exit):
		switch exit.ExitCode() {
		case sys.ExitCodeDeadlineExceeded, sys.ExitCodeContextCanceled:
			return fmt.Errorf("%w: %w", ErrPluginTimeout, err)
		default:
			return fmt.Errorf("%w (exit code %#x): %w", ErrPluginTrapped, exit.ExitCode(), err)
		}
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrPluginTimeout, err)
	default:
		return fmt.Errorf("%w: %w", ErrPluginTrapped, err)
	}
}
```

### The runnable demo

The demo loads two hand-assembled plugins — one that loops forever, one that
returns 42 — and runs both against a 50 ms budget. The loop is killed by the
deadline; the value plugin, on the same host, still returns correctly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/tetratelabs/wazero/api"

	"example.com/runner"
)

// loopWasm is (module (func (export "run") (loop br 0))): an infinite loop.
var loopWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b,
}

// valueWasm is (module (func (export "run") (result i32) i32.const 42)).
var valueWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x2a, 0x0b,
}

func main() {
	ctx := context.Background()
	host := runner.NewHost(ctx, runner.Config{CallTimeout: 50 * time.Millisecond})
	defer host.Close(ctx)

	loop, err := host.Load(ctx, loopWasm)
	if err != nil {
		log.Fatal(err)
	}
	good, err := host.Load(ctx, valueWasm)
	if err != nil {
		log.Fatal(err)
	}

	if _, err := loop.Invoke(ctx); errors.Is(err, runner.ErrPluginTimeout) {
		fmt.Println("loop plugin: killed by deadline")
	}

	res, err := good.Invoke(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("value plugin: run() = %d\n", api.DecodeU32(res[0]))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loop plugin: killed by deadline
value plugin: run() = 42
```

### Tests

The guests are hand-assembled, valid Wasm binaries embedded as byte slices, so the
tests need no toolchain. `TestInvokeTimeout` runs the loop under a 50 ms budget and
asserts the mapped error is `ErrPluginTimeout`, still satisfies
`context.DeadlineExceeded`, and wraps a `*sys.ExitError` whose code is
`ExitCodeDeadlineExceeded`. `TestInvokeSuccess` decodes 42. `TestIsolation` proves
a fault on one plugin leaves a sibling working on the same host.
`TestMapError` covers the mapping directly with synthetic `*sys.ExitError` values,
so it is precise and never flaky. `TestModuleClosedAfterTimeout` observes the
instance directly and asserts `IsClosed()` after a deadline.
`TestLoadRejectsOversizedMemory` proves the memory cap bites at `Load`.

Create `runner_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// loopWasm is (module (func (export "run") (loop br 0))): an infinite loop.
var loopWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b,
}

// valueWasm is (module (func (export "run") (result i32) i32.const 42)).
var valueWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x2a, 0x0b,
}

// trapWasm is (module (func (export "run") unreachable)): always traps.
var trapWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x05, 0x01, 0x03, 0x00, 0x00, 0x0b,
}

// bigMemWasm is (module (memory 3) (func (export "run"))): declares 3 pages.
var bigMemWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x03,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
}

func TestInvokeSuccess(t *testing.T) {
	t.Parallel()
	host := NewHost(t.Context(), Config{})
	t.Cleanup(func() { host.Close(context.Background()) })

	p, err := host.Load(t.Context(), valueWasm)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := p.Invoke(t.Context())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("no result")
	}
	if got := api.DecodeU32(res[0]); got != 42 {
		t.Errorf("run() = %d, want 42", got)
	}
}

func TestInvokeTimeout(t *testing.T) {
	t.Parallel()
	host := NewHost(t.Context(), Config{CallTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { host.Close(context.Background()) })

	p, err := host.Load(t.Context(), loopWasm)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = p.Invoke(t.Context())
	if !errors.Is(err, ErrPluginTimeout) {
		t.Fatalf("Invoke error = %v, want ErrPluginTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout should also satisfy context.DeadlineExceeded: %v", err)
	}
	var exit *sys.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("timeout should wrap *sys.ExitError: %v", err)
	}
	if exit.ExitCode() != sys.ExitCodeDeadlineExceeded {
		t.Errorf("exit code = %#x, want %#x", exit.ExitCode(), sys.ExitCodeDeadlineExceeded)
	}
}

func TestIsolation(t *testing.T) {
	t.Parallel()
	host := NewHost(t.Context(), Config{CallTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { host.Close(context.Background()) })

	loop, err := host.Load(t.Context(), loopWasm)
	if err != nil {
		t.Fatalf("Load loop: %v", err)
	}
	good, err := host.Load(t.Context(), valueWasm)
	if err != nil {
		t.Fatalf("Load value: %v", err)
	}

	if _, err := loop.Invoke(t.Context()); !errors.Is(err, ErrPluginTimeout) {
		t.Fatalf("loop should time out, got %v", err)
	}
	res, err := good.Invoke(t.Context())
	if err != nil {
		t.Fatalf("good plugin failed after loop fault: %v", err)
	}
	if got := api.DecodeU32(res[0]); got != 42 {
		t.Errorf("run() = %d, want 42", got)
	}
}

func TestMapError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"deadline exit", sys.NewExitError(sys.ExitCodeDeadlineExceeded), ErrPluginTimeout},
		{"canceled exit", sys.NewExitError(sys.ExitCodeContextCanceled), ErrPluginTimeout},
		{"abnormal exit", sys.NewExitError(7), ErrPluginTrapped},
		{"plain trap", errors.New("wasm error: unreachable"), ErrPluginTrapped},
		{"deadline error", context.DeadlineExceeded, ErrPluginTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapError(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("mapError(%v) = %v, want nil", tc.in, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("mapError(%v) is not %v: %v", tc.in, tc.want, got)
			}
		})
	}
}

func TestModuleClosedAfterTimeout(t *testing.T) {
	t.Parallel()
	host := NewHost(t.Context(), Config{CallTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { host.Close(context.Background()) })

	p, err := host.Load(t.Context(), loopWasm)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	callCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	mod, err := p.instantiate(callCtx)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer mod.Close(context.Background())

	_, err = mod.ExportedFunction("run").Call(callCtx)
	if !errors.Is(mapError(err), ErrPluginTimeout) {
		t.Fatalf("expected timeout mapping, got %v", err)
	}
	if !mod.IsClosed() {
		t.Error("module should be auto-closed after the context deadline")
	}
}

func TestLoadRejectsOversizedMemory(t *testing.T) {
	t.Parallel()
	host := NewHost(t.Context(), Config{MemoryLimitPages: 2})
	t.Cleanup(func() { host.Close(context.Background()) })

	if _, err := host.Load(t.Context(), bigMemWasm); err == nil {
		t.Fatal("expected Load to reject a plugin whose memory exceeds the page limit")
	}
}

func ExamplePlugin_Invoke() {
	ctx := context.Background()
	host := NewHost(ctx, Config{})
	defer host.Close(ctx)

	p, err := host.Load(ctx, valueWasm)
	if err != nil {
		panic(err)
	}
	res, err := p.Invoke(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(api.DecodeU32(res[0]))
	// Output: 42
}

// Your turn: a plugin can trap for reasons other than a deadline — an unreachable
// instruction, an out-of-bounds access, a bad division. Prove that a trapping
// plugin maps to ErrPluginTrapped and is not reported as a timeout.
func TestTrapMapsToTrapped(t *testing.T) {
	t.Parallel()
	host := NewHost(t.Context(), Config{})
	t.Cleanup(func() { host.Close(context.Background()) })

	p, err := host.Load(t.Context(), trapWasm)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = p.Invoke(t.Context())
	if !errors.Is(err, ErrPluginTrapped) {
		t.Fatalf("Invoke error = %v, want ErrPluginTrapped", err)
	}
	if errors.Is(err, ErrPluginTimeout) {
		t.Error("a trap must not be reported as a timeout")
	}
}
```

## Review

The runner is correct when the three guarantees hold and the error mapping is
exact. The memory cap must bite at `Load` (compile time), which is why the
oversized-memory test checks `Load`, not `Invoke`. The deadline must actually
interrupt a running guest, which only happens with `WithCloseOnContextDone(true)`
*and* a `context.WithTimeout`; drop either and `TestInvokeTimeout` hangs instead
of failing, the tell that the interruption is not wired up. Isolation must survive
a fault: `TestIsolation` runs a plugin to its death and then a good one on the
same host, so if the good call fails you are sharing an instance or the runtime
did not recover.

The mistakes to avoid are the leaks. Do not return the raw `*sys.ExitError` or a
bare wazero error to callers — map to `ErrPluginTimeout`/`ErrPluginTrapped` with
two `%w` verbs so callers use `errors.Is` while diagnostics can still reach the
exit code. Do not recompile on the hot path: `Load` compiles once and `Invoke`
instantiates per call. And always `defer mod.Close` on every invocation with a
detached context, so cleanup runs even when the call has already blown its
deadline. Run `go test -race` to confirm concurrent invocations stay isolated.

## Resources

- [wazero package reference](https://pkg.go.dev/github.com/tetratelabs/wazero) — `NewRuntimeWithConfig`, `RuntimeConfig.WithMemoryLimitPages`, `WithCloseOnContextDone`, `CompileModule`, `InstantiateModule`, `ModuleConfig`.
- [wazero/sys package](https://pkg.go.dev/github.com/tetratelabs/wazero/sys) — `ExitError`, `ExitCodeDeadlineExceeded`, `ExitCodeContextCanceled`, `NewExitError`.
- [wazero/api package](https://pkg.go.dev/github.com/tetratelabs/wazero/api) — `Module.ExportedFunction`, `Module.Close`, `Module.IsClosed`, `DecodeU32`.
- [wazero.io: runtime configuration and context-based interruption](https://wazero.io/docs/) — how memory limits and `WithCloseOnContextDone` bound a guest.

---

Back to [02-capability-scoped-host-module.md](02-capability-scoped-host-module.md) | Next: [../04-tinygo-wasi-guest-modules/00-concepts.md](../04-tinygo-wasi-guest-modules/00-concepts.md)
