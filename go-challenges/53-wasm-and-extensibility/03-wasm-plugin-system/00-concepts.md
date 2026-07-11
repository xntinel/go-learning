# A Wasm-Based Plugin System — Concepts

Sooner or later a backend service has to run code it did not write. A customer
wants to enrich every event with a field computed their way; another team wants a
routing rule your product team will never prioritize; compliance wants a
redaction filter that changes monthly; a webhook integration needs a per-tenant
transform. The tempting answer — "let them submit a Go plugin" or "let them post
a script we `exec`" — hands untrusted logic your process, your memory, your
network, and your credentials. This lesson is about the alternative that
production systems actually ship: run the extension as a WebAssembly guest inside
a sandbox you control, expose it a deliberately small set of host functions, and
bound the resources any single invocation can consume. The hard part is not
"run wasm" — the earlier lessons did that. The hard part is building the trust
boundary: a capability model that says exactly what each plugin may do, a host
API surface scoped to those grants, and an execution harness that turns a
hostile or buggy plugin into one failed request instead of an OOM or a wedged
goroutine.

## Why Wasm and not the obvious alternatives

Go's own `plugin` package (`plugin.Open`) is a non-starter for untrusted code on
three counts. It requires the plugin to be built with an identical toolchain
version and matching build flags, so it is fragile across releases and
effectively un-distributable to third parties. A loaded plugin cannot be
unloaded, so a bad one lives for the life of the process. And it offers zero
isolation: the plugin runs as native code in your address space with full
ambient authority — every syscall, every file descriptor, every environment
variable your process can reach, it can reach too. A subprocess-over-RPC model
(the `hashicorp/go-plugin` approach in a later lesson) fixes distribution and
unloading, but it still runs untrusted *native* code, now behind an OS process
boundary you must harden yourself, at the cost of serialization and a process
per plugin.

A WebAssembly guest is different in kind. Its linear memory is a bounded `[]byte`
that the host owns; the guest can address nothing outside it. And it has no
*ambient authority* at all: a freshly instantiated module cannot make a syscall,
open a socket, read a clock, or touch the filesystem. It can only call the
functions the host explicitly imported into its namespace at instantiation. That
is the whole game. "Sandboxing" a Wasm plugin is not a firewall bolted on after
the fact; it is the default, and every capability you grant is something you
deliberately add back. Deny-by-default is not a policy you enforce — it is the
starting state.

## The trust boundary lives at the import list

Internalize this one sentence and the rest follows: a guest can only call
functions that exist in its imports, and its imports are exactly what the host
put there when it instantiated the module. There is no other channel. So
"what can this plugin do?" reduces entirely to "which host functions did I export
into this instance?" Controlling that set *is* the enforcement mechanism. The
manifest a plugin author ships — "I need to read the KV store and fetch a URL" —
is a *request*, a statement of intent. It is never, by itself, permission. The
host decides, per plugin, which host functions to wire up, and the guest is
powerless to reach anything else.

This is why the design in this lesson is layered as request, then authorization,
then a scoped surface:

- A **manifest** declares the plugin's name, version, and the capabilities it
  requests. It is data supplied by the (untrusted) plugin author.
- An **operator allow-list** says which capabilities the operator is willing to
  hand out at all. This is your configuration, not the plugin's.
- The **grant** is the intersection: `granted = requested ∩ allowed`. A plugin
  asking for `cap.http.fetch` gets it only if the operator also permits it. This
  two-sided model — the plugin declares intent, the operator authorizes — is how
  real plugin platforms avoid confused-deputy escalation, where a plugin tricks a
  more-privileged host into acting on its behalf.
- The **scoped host module** is built from the grant: the host registers exactly
  the functions the granted capabilities permit, and no others. A grant of
  `{cap.log}` produces an `env` module that exports `log` and does not export
  `kv_get`. The absence is the enforcement.

## Defense in depth: scope at assembly, guard at call

Scoping the exported surface is the primary control, but it is not the only one
worth having. A second, independent check belongs *inside* each host function: at
call time, re-verify that the capability is still granted, and if it is not,
return an error code (an "errno") to the guest rather than performing the side
effect. This looks redundant — if `kv_get` is only exported when `cap.kv.read`
was granted, how could it ever run without the grant? The answer is that
assembly-time scoping and call-time guarding protect against *different bugs*.
Assembly-time scoping fails if a refactor mis-wires the builder and registers a
function it should not. Call-time guarding fails if a shared host structure is
accidentally reused across grants. Two independent checks on two different
failure modes is the definition of defense in depth, and it is cheap: one map
lookup per call.

## The boundary is numbers-only, and every read is attacker-controlled

The host/guest boundary carries only numbers — `i32`, `i64`, `f32`, `f64`. There
are no strings, no slices, no structs. A string or a byte buffer crosses as a
`(pointer, length)` pair of `i32`s naming a region of the guest's linear memory
(the ABI the previous lesson built). A host function that "receives a string"
actually receives two integers and must read the bytes itself:
`mod.Memory().Read(ptr, len)` returns `([]byte, bool)`. Both halves of that
signature matter. The `bool` is `false` when the region does not fit in the
current memory, and the pointer and length came from the guest — which is to say,
from untrusted code. Treat every read as attacker-controlled: check the `ok`
return, validate the length before you allocate against it, and never index a
returned slice you have not confirmed is non-empty. A host function that panics
on bad guest input is a denial-of-service vector, because a panic inside a host
function crosses the boundary as a trap and tears down the call.

## Resource exhaustion: the thing Wasm does not solve for free

Memory isolation and capability isolation come for free with the model. Resource
*exhaustion* does not. Two limits you must impose yourself:

**Memory.** A guest can ask its linear memory to grow. Cap it with
`WithMemoryLimitPages(n)` on the runtime config. A page is 65,536 bytes (64 KiB),
not one byte — the single most common configuration bug here is passing a byte
count where a page count is expected. wazero enforces the cap when it compiles a
module: a module whose declared minimum memory exceeds the limit fails at
`CompileModule`, so an over-hungry plugin is rejected at load time, not at some
random allocation later.

**CPU / wall-clock time.** A guest with no syscalls can still loop forever — an
empty `loop br 0` never yields, never blocks, and never returns. Nothing about
the sandbox interrupts it. You must bound it from the outside, and here is the
subtlety that trips everyone: a plain `context.WithTimeout` does **not** stop a
running guest by itself. The runtime has to be told to watch the context. Build
the runtime with `WithCloseOnContextDone(true)`, and *then* a deadline or
cancellation on the context passed to `Call` interrupts execution. Without that
option, the context expires and the guest keeps running.

## Termination semantics: map them, never leak them

When a call is interrupted because its context was cancelled or its deadline
passed (with `WithCloseOnContextDone`), the invocation returns a
`*sys.ExitError` and the module is automatically closed. The exit code is
`sys.ExitCodeDeadlineExceeded` (`0xefffffff`) for a deadline or
`sys.ExitCodeContextCanceled` (`0xffffffff`) for a cancellation, and
`errors.Is(err, context.DeadlineExceeded)` also holds for the deadline case. A
guest that traps for another reason — an `unreachable` instruction, an
out-of-bounds access, an integer divide by zero — returns a plain wazero error
that is *not* a `*sys.ExitError`. Your runner should collapse all of these into
package sentinel errors — `ErrPluginTimeout`, `ErrPluginTrapped` — wrapped with
`%w` so callers match them with `errors.Is`, and log the raw exit code
internally. Never leak `*sys.ExitError` or a raw wazero error up to a caller who
should only care that "the plugin failed."

## Compile once, instantiate per call

`CompileModule` is expensive: it decodes and compiles the whole module. It is
also concurrency-safe to share and reuse. `InstantiateModule` is comparatively
cheap and produces a single `api.Module` instance with its own fresh linear
memory. The rule that makes a multi-tenant plugin host both fast and safe is:
compile each plugin once at load, and instantiate a fresh instance for every
invocation. Compiling per request wastes the expensive step on the hot path;
sharing one instance across requests lets one request's state — or one tenant's
data — bleed into the next. A fresh instance per call guarantees no state
survives between invocations, which is exactly what lets a service treat the same
compiled plugin as safe to run for many tenants concurrently. The mental model is
prepared statements: you prepare (compile) once and reuse the prepared form, but
you never share a single execution (instance/transaction) across concurrent
callers.

## Isolation is per-instance

Closing or trapping one module instance never affects sibling instances or the
host. A plugin that loops until its deadline is killed, its instance is closed,
and the very next invocation — of the same plugin or a different one on the same
runtime — starts clean. This per-instance isolation is what lets a multi-tenant
service treat a plugin fault as a single failed request rather than an outage.
The host owns the lifecycle for every call: instantiate, run under a deadline,
close (with a `defer`), whether the call succeeded or failed.

## What Wasm still does not give you

Be honest about the edges of the sandbox. It does not give you side-channel
resistance (timing, cache) between plugins. It does not give you wall-clock
fairness across many plugins competing for CPU — that is a scheduling problem you
solve above the runtime. And it does not protect you from a host function *you*
wrote badly: every function you export is attack surface, and the sandbox is only
as tight as the smallest-privilege host API you are willing to expose. The wins
here are memory isolation, capability isolation, and bounded resources per call;
everything else is still your engineering.

## Common Mistakes

### Sharing one host module or one instance across plugins or calls

Wrong: build the `env` host module once with the full function set and reuse it
for every plugin, or instantiate a guest once and call it for every request.
Either one destroys the boundary — the shared module grants every capability to
everyone, and the shared instance lets one call's memory leak into the next.

Fix: scope the host module to each plugin's granted capabilities, and instantiate
a fresh guest instance for every invocation from the shared *compiled* module.

### Assuming a context deadline stops a running guest

Wrong: `wazero.NewRuntime(ctx)` plus a `context.WithTimeout` and expecting an
infinite loop to be killed when the deadline passes. It will not be; the guest
runs on.

Fix: `wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))`.
Only with that option does a deadline or cancellation interrupt the guest and
return a `*sys.ExitError`.

### Panicking inside a host function on bad guest input

Wrong: call `mod.Memory().Read(ptr, len)` and index the result while ignoring the
`ok` bool, or trust a guest-supplied length in an allocation. A host panic
crosses the boundary as a trap and is a DoS the guest can trigger at will.

Fix: check the `ok` return, validate the length, and signal problems to the guest
through an errno return value instead of panicking.

### Confusing pages with bytes, or not capping memory at all

Wrong: leaving the default (unbounded) memory, or calling
`WithMemoryLimitPages(65536)` thinking it means 64 KiB. It means 65,536 *pages* —
4 GiB.

Fix: a page is 64 KiB. Size the limit in pages for the workload, and treat a
plugin that exceeds it as a plugin fault (rejected at compile), not a host fault.

### Leaking the runtime error type to callers

Wrong: returning the raw `*sys.ExitError` or a bare wazero error up the stack, so
callers must know wazero's types to react.

Fix: map to package sentinels (`ErrPluginTimeout`, `ErrPluginTrapped`) wrapped
with `%w`, so callers use `errors.Is`; keep the exit code in your logs.

### Trusting the manifest as authorization

Wrong: granting every capability the manifest requests, because the plugin
"asked nicely."

Fix: intersect the requested capabilities with an operator allow-list. A request
is intent; only the operator grants permission.

### Recompiling on every request, or forgetting to close instances

Wrong: `Runtime.Instantiate(ctx, wasmBytes)` (which compiles) inside the request
path, or instantiating per call and relying on the garbage collector to reclaim
instances.

Fix: `CompileModule` once at load, `InstantiateModule` per call from the compiled
form, and `defer mod.Close(ctx)` on every invocation. The runtime auto-closes on
context-done, but never assume that on the success path.

Next: [01-plugin-manifest-and-capabilities.md](01-plugin-manifest-and-capabilities.md)
