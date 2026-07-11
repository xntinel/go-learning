# Compiling Guest Modules with TinyGo and WASI — Concepts

The first three lessons of this chapter put you on the host side: you embedded
`wazero`, crossed the boundary with linear memory, and locked an untrusted module
down with a capability-scoped host API. This lesson flips the seat. Now you are
the author of the guest. On a real backend platform the guest is rarely something
you hand-assemble in `.wat`; it is a customer- or team-supplied extension — a log
redactor, an event transformer, a policy or rules evaluator — shipped to you as a
compiled `.wasm` artifact and run inside your service. The engineering questions
that decide whether this is a toy or a product are all authoring decisions: which
compiler and target you standardize on, whether a guest is a run-once command or a
long-lived reactor, and exactly which capabilities the host wires in. This file is
the conceptual foundation for the three exercises that follow; read it once and
you can reason through each independently.

One honest note up front on how these exercises are validated. Building a WASI
guest requires the `wasip1` target (or TinyGo), and the host runner imports
`wazero` from the network, so this material does not compile on a plain offline
`darwin/amd64` gate the way lessons 1-3 do. The code here is held to the same
standard — real, verified APIs; `gofmt`-clean; every "the code does X" sentence
true of the code — but its build and run are deferred to a machine with the WASI
toolchain and network. Each exercise states the exact `go` and `tinygo` commands
so you can reproduce it end to end.

## Concepts

### WASI Preview 1 is a syscall ABI, not a language feature

WebAssembly on its own has no I/O: a bare module cannot read a file, get the time,
or write to a terminal. WASI (the WebAssembly System Interface), Preview 1
(`wasip1`), fills that gap with a POSIX-flavored set of host functions —
`fd_read`, `fd_write`, `args_get`, `environ_get`, `clock_time_get`, `random_get`,
`path_open`, and friends — grouped under a module named `wasi_snapshot_preview1`.
The consequence for you as a guest author is the pleasant part: a guest compiled
to `wasip1` is *ordinary Go*. You write `os.Stdin`, `os.Stdout`, `os.Args`,
`os.Getenv`, `bufio.Scanner`, `os.ReadFile` — the standard library — and the
compiler lowers those calls onto the WASI imports. There is no special "guest
SDK" to learn for the basics; the syscall shim is the ABI.

The host side is the mirror image. Before you instantiate any WASI guest, you must
supply that `wasi_snapshot_preview1` module, which `wazero` does with a single
call: `wasi_snapshot_preview1.MustInstantiate(ctx, r)`. Skip it and the guest
fails to link at instantiation time with an unresolved-import error for
`fd_write` (or whichever WASI function it first needs), because the module
declares imports the runtime has not provided.

### Two guest shapes: command versus reactor

WASI defines two lifecycles, and choosing between them is really an
invocation-cost decision.

A *command* module has a `_start` entry point — `wazero`'s default start
function. Its `main()` runs to completion during `InstantiateWithConfig`, the
process's world (runtime, package `init`s, GC) is built up, `main` runs, and the
instance is spent afterward. This is the shape of a Unix filter: feed it stdin,
collect stdout, read the exit code, throw the instance away. It is the right model
for a run-once pipeline stage, and it is what Exercise 1 builds.

A *reactor* module is built so the linker emits `_initialize` instead of `_start`.
The host calls `_initialize` once (via `WithStartFunctions("_initialize")`) to run
runtime and package initialization, and then the instance *stays live*. Functions
the guest marks with `//go:wasmexport` can be called on that same instance
repeatedly, amortizing the one-time init cost across many calls. This is the right
model for a hot plugin path — a per-request policy check called thousands of times
a second — and it is what Exercise 2 builds. The mental shortcut: command is a
program you run; reactor is a library you load.

### The toolchain trade-off is a real decision

Two toolchains compile Go to `wasip1`, and the choice is not cosmetic.

The standard toolchain — `GOOS=wasip1 GOARCH=wasm go build` (Go 1.21+) — gives you
the full language, the real runtime and garbage collector, goroutines, and the
entire standard library, including reflection-heavy packages like
`encoding/json`. The cost is size and startup weight: the artifact carries the Go
runtime, so binaries are on the order of megabytes and cold start is heavier.

TinyGo — `tinygo build -target=wasip1` — is an LLVM-based compiler that produces
dramatically smaller, faster-loading modules with a lighter garbage collector.
The cost is coverage: reflection is limited, parts of the standard library are
missing or stubbed, and there are concurrency caveats. TinyGo is not a drop-in for
the standard toolchain; reaching for heavy reflection or the full `net`/`encoding`
surface in a TinyGo guest hits missing-feature or panic walls.

For a platform running a handful of large guests, the standard toolchain's
convenience usually wins. For a platform instantiating thousands of small guests,
artifact size and cold-start count push you toward TinyGo. Pick the toolchain
against the guest's actual needs, not out of habit.

### go:wasmexport and the c-shared reactor build

`//go:wasmexport name` (added in Go 1.24, TinyGo 0.34+) marks a Go function as a
Wasm export named `name`. It only compiles under `GOOS=wasip1`, and it belongs in
`package main`. Marking a function is not enough on its own: a normal build still
emits `_start` and runs `main()`, spending the instance. To get a reactor you
build with `-buildmode=c-shared`, which tells the linker to suppress `_start`,
emit `_initialize` instead, and export your `//go:wasmexport` functions along with
the init code they depend on:

```
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reactor.wasm .
```

TinyGo mirrors this with `tinygo build -buildmode=c-shared -target=wasip1`. A
`package main` still needs a `func main()` present to compile, but under the
reactor build it is *not* invoked automatically — the host calls `_initialize`,
then the exported functions.

### The exported-function ABI is numeric

Go 1.24 relaxed the *type* constraints on `//go:wasmexport` and
`//go:wasmimport` (bool, int32/int64, float32/float64, `unsafe.Pointer`, and
pointers to certain types are permitted at the language level), but the seam a
`wazero` host actually drives is numeric. A host calls
`api.Function.Call(ctx, ...uint64)` and gets back `[]uint64`; scalars are packed
with `api.EncodeI32`/`api.EncodeI64`/`api.EncodeF64` and recovered with the
matching `Decode`. So a reactor export whose Go signature is
`func(int64, int32) int32` is invoked from the host as
`fn.Call(ctx, api.EncodeI64(id), api.EncodeI32(pct))` and read back with
`api.DecodeI32(res[0])`.

The sharp edge: there is no rich RPC here. Passing a Go `string` or `[]byte`
*through* an exported function to the host means going back to the linear-memory
mechanism from lesson 2 — write the bytes into the module's memory with a
guest-exported allocator and pass a `(ptr, len)` scalar pair. Reactor exports are
the low-level seam; keep their signatures scalar and layer any structured
protocol on top through memory.

### wazero enforces the sandbox host-side, and it is default-deny

The guest is ordinary Go asking for stdin, args, env, and files — but it gets
*nothing* unless the host grants it, and the grant lives entirely in
`wazero.ModuleConfig`. The defaults are the whole point:

- `WithArgs` defaults to none, so `os.Args` is empty in the guest.
- `WithStdin`/`WithStdout`/`WithStderr` default to `io.Discard`; an unwired stdin
  yields EOF immediately.
- `WithEnv` sets nothing, so `os.Getenv` returns `""`.
- Filesystem access is denied until you attach an `FSConfig`.

Crucially, none of these fall back to the host process's `os.Args`, `os.Environ`,
or `os.Stdin`. That is deliberate: a guest that inherited the host's ambient
authority would not be sandboxed. You build the least-privilege surface *up* by
granting capabilities one at a time, rather than starting permissive and trying to
lock it down. This default-deny posture is exactly the property a platform team
wants.

### Filesystem is capability-based through preopens

WASI has no global filesystem namespace. A guest sees only directories the host
*preopens* for it, mapped to guest paths through `FSConfig`:

- `NewFSConfig().WithReadOnlyDirMount(hostDir, "/in")` grants read-only access; the
  guest does `os.Open("/in/data.txt")`.
- `WithDirMount(hostDir, "/out")` grants read-write access.
- `WithFSMount(fsys fs.FS, "/in")` mounts an in-memory or embedded `fs.FS`
  (read-only, since `fs.FS` has no write path) — handy for hermetic tests.

Two rules keep this safe. Mount the narrowest directory the guest needs, never the
host root (`WithDirMount("/", "/")`), which hands over everything. And know the
sharp edge: even a narrow mount can be traversed with `../` relative lookups, so
prefer a purpose-built directory and prefer read-only whenever the guest only
reads.

### Determinism is a deliberate default

Under `wazero`'s defaults the guest's view of time and randomness is *fake and
reproducible*: the wall and monotonic clocks advance by a fixed 1ms per read
rather than tracking real time, and `random_get` returns a deterministic
sequence. For replayable pipelines and reproducible plugin tests this is a
feature — the same input yields byte-identical output every run. You opt into real
behavior explicitly: `WithSysWalltime()`, `WithSysNanotime()`, and
`WithSysNanosleep()` wire the host clock; `WithRandSource(crypto/rand.Reader)`
wires real entropy. The trap runs both ways: anything that needs real entropy
(tokens, nonces, session IDs) is broken until you wire a real source, and anything
that needs reproducibility is broken the moment you wire one in without meaning to.

### Exit semantics: a clean run is still an ExitError

A WASI command terminates by calling `proc_exit`, and Go's `wasip1` runtime does
this at the end of `_start` even on a normal return. `wazero` surfaces that as
`*sys.ExitError` returned from `InstantiateWithConfig` — *including exit code 0*.
So a successful command run does not return a `nil` error; it returns a
`*sys.ExitError` whose `ExitCode()` is 0. The correct host pattern is not
`if err != nil { fail }`; it is: type-assert (with `errors.As`) to `*sys.ExitError`
and branch on `ExitCode()`, treating any *other* error as a genuine
instantiation or validation failure. Getting this wrong turns every successful run
into a reported failure.

## Common Mistakes

### Treating a command module like a service

Wrong: calling `InstantiateWithConfig` once on a command module and then trying to
invoke its exported functions repeatedly. A command's `_start` already ran `main()`
to completion; the instance is spent, and there are no persistent exports to call.

Fix: build a reactor (`-buildmode=c-shared` plus `//go:wasmexport`) and instantiate
it with `WithStartFunctions("_initialize")`, keeping the instance live for repeated
calls.

### Forgetting to instantiate the WASI host module

Wrong: instantiating the guest without first calling
`wasi_snapshot_preview1.MustInstantiate(ctx, r)`, then hitting an
unresolved-import error at instantiation for `fd_write`, `args_get`, or similar. A
WASI guest declares imports the runtime must satisfy; it cannot link without them.

Fix: instantiate `wasi_snapshot_preview1` on the runtime once, before you
instantiate any guest.

### Expecting ambient authority inside the guest

Wrong: assuming `os.Args`, `os.Getenv`, `os.Stdin`, or file access "just work"
because the guest is Go. `wazero` grants nothing by default; without `WithArgs`,
`WithEnv`, `WithStdin`, and an `FSConfig`, the guest sees empty argv, no env, EOF
on stdin, and no filesystem.

Fix: grant each capability explicitly through `ModuleConfig`, adding only what the
guest needs.

### Treating a clean run as a nil error (or exit 0 as failure)

Wrong: `_, err := rt.InstantiateWithConfig(...); if err != nil { return err }`.
A successful command run returns a `*sys.ExitError` with code 0, so this reports
every success as a failure.

Fix: `errors.As(err, &exit)` and branch on `exit.ExitCode()`; only a non-ExitError
is a real instantiation failure.

### Passing a Go string through a reactor export

Wrong: giving a `//go:wasmexport` function a `string`/`[]byte` parameter and
expecting the `wazero` host to receive text. The call convention across the raw
boundary is `[]uint64` in, `[]uint64` out.

Fix: keep exports scalar; move bytes through linear memory with a guest-exported
allocator and a `(ptr, len)` pair, as in lesson 2.

### Building the reactor wrong

Wrong: building a `//go:wasmexport` guest without `-buildmode=c-shared`, or on the
wrong `GOOS`. Without `wasip1` the directive does not compile; without `c-shared`
the linker emits `_start` and there is no `_initialize`, so the host's
`WithStartFunctions("_initialize")` fails.

Fix: `GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared` (or the TinyGo
equivalent).

### Assuming time and randomness are real

Wrong: relying on `time.Now()` or `crypto/rand` inside the guest to be real under
`wazero` defaults — the clock is a 1ms-stepping counter and randomness is a fixed
sequence — or, conversely, wiring `WithSysWalltime`/`WithRandSource` into a
pipeline you expected to be reproducible.

Fix: choose deliberately. Keep the defaults for reproducibility; opt in with
`WithSysWalltime()` and `WithRandSource(crypto/rand.Reader)` when the guest needs
real time or entropy.

### Mounting too broadly

Wrong: `WithDirMount("/", "/")` (or any broad host directory) to make file I/O
convenient, which hands the guest the whole filesystem — and forgetting that even
a narrow mount can be escaped with `../`.

Fix: mount the minimal, purpose-built directory, prefer `WithReadOnlyDirMount`
whenever the guest only reads, and never mount the host root.

### Assuming TinyGo is a drop-in

Wrong: porting a guest that uses heavy reflection, the full `net`/`encoding`
standard library, or unrestricted goroutines to TinyGo and hitting missing-feature
or panic walls.

Fix: pick the toolchain against the guest's real requirements — the standard
`wasip1` toolchain for full-language guests, TinyGo for small, fast-cold-start
ones that stay within its supported surface.

Next: [01-wasi-command-filter.md](01-wasi-command-filter.md)
