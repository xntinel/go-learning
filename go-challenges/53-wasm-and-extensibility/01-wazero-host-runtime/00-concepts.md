# Embedding a Wasm Runtime with wazero — Concepts

There is a recurring need in backend services to run code you did not write at
build time: a customer's pricing rule, a tenant's routing policy, a plugin that
transforms an event. The usual answers are bad. Shelling out to a subprocess
means process management, IPC, and a second deployment artifact. A cgo binding to
`wasmtime` or `wasmer` means a C toolchain, a broken `CGO_ENABLED=0`, dynamic
libc dependencies, and painful cross-compilation. An in-process scripting engine
(Lua, JavaScript) means shipping and sandboxing an interpreter you now own.

`wazero` is the option that keeps everything Go. It is a WebAssembly runtime
written in pure Go with zero cgo and zero platform dependencies, so a service
that embeds it still compiles to a single static binary and still cross-compiles
from your Mac to a Linux/arm64 Lambda image with `GOOS`/`GOARCH` and nothing
else. The value proposition is not "run hello world in Wasm"; it is that Wasm
becomes a first-class, in-process, deterministic, sandboxed execution substrate
you can run untrusted or hot-swappable code on, inside a long-lived server,
without leaving the Go build. This lesson establishes the embedding and lifecycle
discipline. Crossing the boundary with strings and bytes is lesson 2; capability
sandboxing is lesson 3. Here we cross it only with numbers.

## Concepts

### The model: host and guest share nothing by default

Draw the boundary and keep it drawn. The *host* is your Go code — the runtime,
the service around it. The *guest* is the compiled Wasm module. They live in the
same OS process but share nothing implicitly: no variables, no heap, no globals.
The only things that cross the boundary are the functions you explicitly export
from the guest (and, from lesson 2 on, the guest's linear memory and the host
functions you import into it). A guest has no ambient authority. By default it
cannot read a clock, get randomness, touch the filesystem, open a socket, or see
an environment variable unless you wire that capability in. That default-deny
posture is exactly why Wasm is a defensible plugin host: an untrusted module can
compute, but it cannot reach out. In this lesson the boundary carries numeric
arguments only, which sidesteps memory entirely and lets us focus on lifecycle.

### The three-object lifecycle mirrors a connection pool

wazero has three objects, and conflating them is the number-one performance
mistake people make. Treat the relationship the way you treat a database pool.

- `Runtime` (`wazero.NewRuntime` / `NewRuntimeWithConfig`) owns the compilation
  engine and configuration. It is long-lived — one per process, like the pool
  itself. Closing it releases everything it created.
- `CompiledModule` (`Runtime.CompileModule`) is the result of decoding and
  compiling the `.wasm` bytes. This is the expensive step. It is reusable and
  should be produced once per distinct module and cached, like a prepared
  statement.
- `api.Module` instance (`Runtime.InstantiateModule`) is one live, isolated
  execution context with its own linear memory and globals. It is cheap. You
  create one per invocation, per request, or per tenant — like a connection
  checked out of the pool.

The discipline is **compile once, instantiate many**. `Runtime.CompileModule`
does the parsing and code generation; `Runtime.InstantiateModule` just wires up a
fresh isolated instance from an already-compiled module. There is also a
convenience method, `Runtime.InstantiateWithConfig`, that chains compile and
instantiate in one call — fine for a one-shot program, wrong for a server,
because it recompiles the same bytes on every call. In a hot path you compile at
startup and instantiate per unit of work.

### Compiler versus interpreter: think JIT warmup

wazero ships two execution backends and lets you choose explicitly.

- The *compiler* (`NewRuntimeConfigCompiler`, and the default that plain
  `NewRuntime` selects where supported) translates the Wasm into native machine
  code for near-native execution speed. It pays a one-time compile cost per
  module and only runs on supported architectures (amd64, arm64).
- The *interpreter* (`NewRuntimeConfigInterpreter`) executes the Wasm bytecode
  directly. It runs everywhere Go runs, has no code-generation cost, and is
  considerably slower per call.

Reason about it like JIT warmup. For a hot, long-lived workload where the module
is compiled once and called millions of times, the compiler's upfront cost
amortizes to nothing and you want the native speed. For a portability-first
target, a restricted environment where emitting executable memory is not allowed,
or a cold-start-sensitive path where you call the module a handful of times and
never again, the interpreter's zero warmup can win. Both backends are
deterministic and produce identical results for the same input — the choice is
purely about the performance envelope, not correctness.

### The uint64 stack ABI and the encode/decode trap

Every value that crosses `api.Function.Call` does so as a `uint64`, regardless of
its real Wasm type. `Call(ctx, params ...uint64) ([]uint64, error)` takes and
returns raw 64-bit stack words. An `i32`, an `i64`, an `f32`, an `f64` are all
transported as one `uint64` each; the type lives in the function signature, not
in the value. This means you must convert with the `api` package's helpers on the
way in and out: `api.EncodeI32`/`DecodeI32`, `EncodeU32`/`DecodeU32`,
`EncodeI64`/`DecodeI64`, `EncodeF64`/`DecodeF64`, and so on.

The subtle correctness trap of the whole chapter lives here. A raw Go cast
`uint64(myInt32)` is *not* the correct encoding for a negative number: for
`int32(-5)`, `uint64(-5)` sign-extends to `0xFFFFFFFFFFFFFFFF`, whereas the Wasm
i32 stack word must be `0x00000000FFFFFFFB`. `api.EncodeI32` does the correct
32-bit two's-complement placement; the naive cast silently corrupts every
negative integer. Floats are worse: an `f64` on the stack is the IEEE-754 *bit
pattern* of the value, so `EncodeF64` is `math.Float64bits` and a numeric cast
like `uint64(3.14)` throws the fraction away entirely. Never hand-cast; always
encode and decode.

### Isolation and determinism are guarantees, not accidents

Each instance produced by `InstantiateModule` has its own linear memory and its
own globals. Nothing leaks from one instance to another — instantiate two copies
of the same compiled module and mutating one cannot be observed through the other.
Combined with the default-deny posture (no clock, no randomness, no I/O unless
wired), this makes each instance a clean, reproducible sandbox. It also dictates
your concurrency model: a single `api.Module` instance has exactly one linear
memory, so concurrent goroutines calling into the same instance race on that
memory. The correct pattern is one instance per concurrent unit of work — they
cheaply share the single `CompiledModule` — or serialized access to a shared
instance. Never fan out goroutines onto one instance and expect isolation.

### Failure modes: a guest can loop forever or trap

Untrusted code misbehaves. A guest can enter an infinite loop, or it can trap
(an out-of-bounds access, an integer divide by zero, an `unreachable`). A trap
surfaces as an error from `Call` and unwinds cleanly, so traps are not the
dangerous case. The dangerous case is the infinite loop: by default, a spinning
guest holds the calling goroutine forever, and passing a `context.WithTimeout`
does **not** help on its own. Cancellation of a running guest is opt-in. You must
build the runtime with `RuntimeConfig.WithCloseOnContextDone(true)` *and* pass a
cancellable or deadline-bearing context into `Call`; only then does wazero check
for cancellation and interrupt the guest when the deadline fires. With that
enabled, the runaway `Call` returns an error that wraps `context.DeadlineExceeded`
(or `context.Canceled`), and you are back in control. Treat every invocation as
untrusted work carrying a deadline.

### Memory must be bounded

A Wasm module has a *linear memory* measured in pages of 64 KiB each, and it can
ask to grow it at runtime with the `memory.grow` instruction, up to the Wasm
maximum of 65536 pages — 4 GiB. A buggy or hostile guest that grows without bound
will pressure the host. Cap it with `RuntimeConfig.WithMemoryLimitPages(n)`: once
set, a `memory.grow` past the cap fails (the instruction returns `-1` to the
guest; the host-side `Memory().Grow` returns `ok == false`) instead of
succeeding, so an over-allocating guest fails its own allocation rather than
exhausting the host. Remember the unit: pages, not bytes. `WithMemoryLimitPages(2)`
is a 128 KiB ceiling. Every plugin invocation should carry both a time budget and
a memory budget.

### Deterministic teardown and why cgo-free is operational

`Runtime.Close` (and `CloseWithExitCode`) releases the compiled native code,
every instance the runtime created, and their mapped memory. Order matters when
you close things by hand — instances, then the `CompiledModule`, then the
`Runtime` — though `Runtime.Close` alone will tear down everything it owns, and
closing twice is safe. In a long-lived server this is not optional bookkeeping:
leaking a `Runtime` leaks native machine-code pages that never come back, so own
the `Close` in the same scope that created the runtime and make shutdown paths
idempotent. `context.Context` threads through instantiate and call, carrying
cancellation and tracing.

Finally, the reason to reach for wazero over a cgo binding is operational, not
aesthetic. Pure Go means `CGO_ENABLED=0` static binaries, trivial
cross-compilation (build the Linux/arm64 artifact on a Mac with two environment
variables), no libc or version skew to debug in production, and reproducible
builds. For embedded backend use that is usually the deciding factor; reach for a
cgo-based runtime only when a specific feature forces it.

## Common Mistakes

### Recompiling on every request

Wrong: calling `Runtime.InstantiateWithConfig` (which decodes and compiles) per
invocation, so you pay the compile cost on every request.

Fix: call `Runtime.CompileModule` once at startup, cache the `CompiledModule`,
and call `Runtime.InstantiateModule` per request or per tenant. Instantiation is
the cheap step.

### Passing arguments as raw uint64 casts

Wrong: `fn.Call(ctx, uint64(a), uint64(b))` with a negative or floating-point
`a`/`b`. `uint64(int32(-5))` sign-extends to a 64-bit value the guest reads as a
huge positive i32, and `uint64(3.14)` discards the fraction.

Fix: encode on the way in and decode on the way out —
`fn.Call(ctx, api.EncodeI32(a), api.EncodeI32(b))` and
`api.DecodeI32(results[0])`. Use `EncodeF64`/`DecodeF64` for floats, which move
the IEEE-754 bit pattern.

### Assuming Call cancellation works out of the box

Wrong: wrapping a call in `context.WithTimeout` but leaving the runtime at its
default config. A spinning guest ignores the deadline and hangs the goroutine.

Fix: build the runtime with `NewRuntimeConfig().WithCloseOnContextDone(true)`
*and* pass the deadline context into `Call`. Both are required.

### Not bounding guest memory

Wrong: instantiating an untrusted guest with no memory limit, then being
surprised when it grows toward the 4 GiB Wasm maximum and pressures the host.

Fix: set `RuntimeConfig.WithMemoryLimitPages(n)`, and remember a page is 64 KiB,
so size `n` accordingly.

### Leaking runtimes and instances

Wrong: forgetting `defer rt.Close(ctx)`, or closing objects in a scope that does
not own them. Over a server's lifetime this leaks native code and mapped memory.

Fix: own `Close` in the same scope that created the object, close instances
before the compiled module before the runtime when you close by hand, and make
shutdown idempotent (a second `Close` is a no-op).

### Sharing one instance across goroutines

Wrong: fanning out goroutines that all call into one `api.Module` instance and
expecting isolation. They share one linear memory and race.

Fix: instantiate one module per concurrent unit — they share the cheap
`CompiledModule` — or serialize access to a single instance.

### Indexing results without checking arity

Wrong: assuming `results[0]` exists. A function declared with no results returns
an empty slice, and `results[0]` panics.

Fix: check `len(results)` (or the function's `FunctionDefinition.ResultTypes`)
before indexing, and return a sentinel error when the arity is not what the
caller expects.

### Reaching for a cgo runtime out of habit

Wrong: defaulting to `wasmtime`/`wasmer` Go bindings, which pull in cgo and break
`CGO_ENABLED=0` static builds and easy cross-compilation.

Fix: default to wazero for embedded backend use; choose a cgo runtime only when a
specific feature it has and wazero lacks forces the trade.

Next: [01-module-loader.md](01-module-loader.md)
