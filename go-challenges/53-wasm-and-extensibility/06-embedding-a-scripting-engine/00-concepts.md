# Embedding a Scripting Engine — Concepts

Sooner or later an operator or a customer needs to change behavior without waiting
for your next deploy: the pricing rule, the eligibility check, the routing
decision, the alert threshold, the authorization policy, a config transform. The
heavyweight answer is a WebAssembly plugin (the rest of this chapter): compiled
code from an arbitrary language, memory-isolated from a potentially hostile
author. The lightweight answer — and usually the right first reach — is an
embedded expression or scripting engine that runs user-authored logic *in the
same process*, at request rate, safely. This file is the conceptual foundation
for that reach: what "safely" actually requires, how the engines differ, and why
the sandbox is your job and not the library's default. Read it once and the three
independent exercises that follow — a rules engine on `expr`, an ABAC policy
checker on CEL, and a config-as-code pipeline on Starlark — become variations on
a single theme.

## Concepts

### The capability-versus-containment spectrum

The first decision is not "which library" but "how much power does this
requirement actually need". Arrange the options on a spectrum from least to most
capable, because capability trades directly against how hard the thing is to
contain.

At the low-capability end are pure *expression* evaluators: `expr` and Google's
CEL. They are non-Turing-complete *by design*. There are no user-defined loops,
no recursion, no persistent state between evaluations, no way to define a
function that calls itself. An expression is a single value computed from inputs:
`amount > 1000 && country in ["US", "CA"]`. This constraint is exactly what makes
them safe to run on untrusted-ish input on the hot path — a bounded AST evaluates
in microseconds and cannot spin forever. This is the correct tool for rules,
filters, and policy.

At the higher-capability end are real *scripting* languages. Starlark (the
configuration dialect behind Bazel and Tilt) has control flow, functions, and
comprehensions. That expressiveness is sometimes genuinely required — a config
generator that computes a fleet of service definitions from a few parameters
cannot be a single expression. You pay for the power with explicit budgets:
because the language *can* loop, you must bound how long it runs. The rule of
thumb is to choose the *least powerful* engine that still expresses the
requirement. Reaching for a scripting language when an expression would do buys
you a DoS surface you then have to defend.

### Scripting engine versus WASM plugin: which and when

Both let outsiders change in-process behavior, but they answer different threat
models. Embedding a scripting engine wins on latency (microseconds, no
serialization, no module instantiation), on operational simplicity (no separate
toolchain, one process, typed access to your host data structures), and on the
common case where what changes is *logic authored by a semi-trusted operator* —
your own SREs, a customer's admin editing a rule in a web form. WASM wins when
the extension is *code from a third party*: it can be compiled from any language,
distributed as an opaque binary, and — crucially — sandboxed with hard memory
isolation from an author you do not trust at all. A slogan that holds up:
scripting is for untrusted *data and logic* from semi-trusted authors; WASM is
for untrusted *code* from untrusted authors. If a rule fits in a boolean
expression, do not stand up a WASM runtime for it.

### The sandbox is your responsibility, not the library's default

This is the single most important operational fact and it surprises people. These
engines ship *permissive* by default. `expr` enables a large set of builtins.
Give an evaluator a host function and it will happily call it. The library gives
you *primitives* — an allowlist mechanism, a typed environment, resource limits,
a cancellation hook — but it does not compose them into a policy for you. You
must build a deny-by-default sandbox: disable all builtins and re-enable a
vetted allowlist, expose a typed environment that contains *only* the fields you
intend to reveal, and set hard resource limits. Never hand an untrusted
expression `fmt.Sprintf`, anything from `os` or `net`, or a reflection-backed
helper that can reach arbitrary methods. The default is convenient; convenient is
not safe.

### A taxonomy of resource limits, and the DoS vector each one stops

There is no single "safe" switch. Different limits stop different attacks, and a
production embedding usually needs several:

- Complexity or size limits at *compile* time. `expr`'s `MaxNodes` caps the
  number of AST nodes, so a pathologically nested expression is rejected before
  it is ever run. This bounds parse and compile blowups.
- Memory limits at *runtime*. `expr`'s `vm.VM.MemoryBudget` caps allocation
  during a single evaluation, which stops allocation bombs — a huge range like
  `1..1000000000` or a large string repetition — from exhausting the heap.
- Cost limits. CEL's `CostLimit` bounds total *work units* using a combined
  static-plus-dynamic cost model; a comprehension over a large list accrues cost
  per iteration and trips the limit.
- Step limits. Starlark's `SetMaxExecutionSteps` bounds the instruction count, so
  a heavy loop is cancelled after N steps.
- Wall-clock limits. None of the above is a *time* bound. Wall-clock enforcement
  needs a cancellation mechanism — for CEL, `ContextEval` plus
  `InterruptCheckFrequency`; for Starlark, a watcher goroutine that calls
  `Thread.Cancel`.

The trap is assuming one limit covers another. A cost limit is not a timeout. A
node limit does not stop a runtime allocation bomb. Layer them.

### Compile once, evaluate many

Parsing and type-checking are the expensive phases; evaluation of an
already-compiled program is cheap. Both engines that matter here make the split
explicit: `expr.Compile` returns a `*vm.Program`, and CEL's two-phase model turns
source into a type-checked AST (`Env.Compile`) and then into an executable
(`Env.Program`). A compiled program is safe to cache and to reuse concurrently
for evaluation, each call supplying its own environment. Treat compilation as a
deploy-time or config-load step and evaluation as the hot path. Recompiling on
every request is the most common performance mistake, and it also re-runs the
type check on the request path, converting a config-time error into a latent
runtime failure.

### Static typing as an operability feature

`expr` and CEL type-check an expression against the *declared* environment at
compile time. A rule that compares an integer age to the string `"old"`, or
references a field that does not exist, fails at config load with a clear message
— not at 2am in production with a panic. This is a decisive argument for choosing
a typed engine when the shape of your data is known: it turns a whole class of
runtime failures into deploy-time validation you can gate a config change on. A
dynamically-typed scripting language gives you that error only when the bad line
happens to execute.

### Cancellation is not automatic, and its granularity differs per engine

This is subtle and it defeats naive timeouts. `expr`'s VM does *not* check a
context between opcodes; wrapping `expr.Run` in a `context.WithTimeout` does
nothing for a CPU-bound evaluation. Because the language has no unbounded loops,
its real DoS surface is memory and complexity, which is why you guard it with
`MemoryBudget` and `MaxNodes` rather than a timer. CEL enforces cancellation only
through `ContextEval` combined with `InterruptCheckFrequency`, which polls the
context every N comprehension iterations. Starlark has no built-in wall-clock
bound at all — you enforce it with a separate goroutine that calls
`Thread.Cancel`. Knowing the granularity is what prevents a false sense of a
working timeout.

### Determinism and side-effect freedom

For config-as-code and for policy decisions you want to audit, evaluation should
be a pure function of its inputs. The same rule against the same fact must always
produce the same answer, or caching, reproducibility, and auditability all break.
That means: do not expose wall-clock time, randomness, or IO to the sandbox; keep
host functions pure; and freeze any shared or predeclared value so a script
cannot mutate host state and leak an effect into the next evaluation. Starlark's
`Freeze` makes a value deeply immutable for exactly this reason. A non-pure
sandbox is a sandbox that lies to your audit log.

### Error surfaces and failing closed

The host must distinguish two error classes and handle them differently.
*Compile* errors (type mismatches, parse failures) are the config author's
mistake and belong at config-load time, surfaced as a clear operator-facing
message — never reported as a 500 on the request path. *Runtime* errors (a limit
exceeded, a cancellation, a host-function failure) happen during evaluation. For
anything security-relevant — an authorization or admission decision — a runtime
error must *fail closed*: deny, never allow. Mapping "errored or cancelled" to
"allow" is a security bug. And a limit breach must never crash the host: `expr`'s
memory-budget breach *panics inside the VM*, and while `expr.Run` recovers it
into a returned error at the `Run` boundary, any host function you write must
return errors rather than panic.

## Common Mistakes

### Treating the engine as safe by default

Wrong: compiling operator expressions with every builtin enabled, or dropping
`fmt.Sprintf` and `os` helpers into the environment for "convenience", handing
untrusted logic formatting, IO, or reflection reach.

Fix: `DisableAllBuiltins` and re-enable only a vetted allowlist; expose a typed
environment struct that contains exactly the fields the rule may read and nothing
else.

### Assuming a context deadline aborts a running expression

Wrong: wrapping `expr.Run` in `context.WithTimeout` and believing a CPU-bound
evaluation will be cancelled. `expr`'s VM does not poll the context between
opcodes.

Fix: rely on `MaxNodes` plus `MemoryBudget` for `expr`; use
`ContextEval` plus `InterruptCheckFrequency` for CEL and a watcher goroutine
calling `Thread.Cancel` for Starlark.

### Recompiling on every request

Wrong: calling `expr.Compile` or CEL's `Env.Compile`/`Env.Program` inside the
request handler, paying parse and type-check cost on the hot path and
re-introducing the type check as a latent runtime failure.

Fix: compile at config-load time, cache the `*vm.Program` / `cel.Program`, and
reuse it for evaluation with per-call environments.

### Setting CostLimit but expecting it to be a timeout

Wrong: configuring CEL's `CostLimit` and assuming it bounds wall-clock time, or
setting it without `EvalOptions(OptTrackCost)` so cost is never tracked.

Fix: cost is a work-units bound, not time. Track cost with `OptTrackCost`, and
enforce wall-clock separately via `ContextEval` and `InterruptCheckFrequency`.

### Forgetting that Starlark disables recursion and while by default

Wrong: writing (or testing) Starlark that assumes Python-like recursion and being
surprised by the dynamic "called recursively" error; or the opposite — enabling
`FileOptions.Recursion`/`While` for untrusted input without a step budget,
reopening the infinite-loop DoS.

Fix: keep the safe defaults for untrusted input, express iteration with bounded
`for`-over-`range`, and if you must enable recursion or `while`, always pair it
with `SetMaxExecutionSteps`.

### Not freezing predeclared or shared values in Starlark

Wrong: exposing a mutable host dict to scripts, letting one evaluation mutate it
and leak state into the next, breaking determinism and thread-safety.

Fix: `Freeze` the predeclared `StringDict` so scripts cannot mutate host state.

### Letting a host function panic

Wrong: a host function that does a bare type assertion or nil deref and panics,
tearing down the request goroutine.

Fix: host functions return errors, never panic. Recover the memory-budget panic
at the `expr.Run` boundary (the library already does this) and return CEL/host
errors as values.

### Failing open on evaluation error for policy

Wrong: `allowed, err := policy.Eval(...); if err != nil { allow() }`. A
limit-exceeded, cancelled, or errored policy evaluation that grants access is a
security hole.

Fix: fail closed. Any non-true-or-errored policy result denies.

Next: [01-rules-engine-with-expr.md](01-rules-engine-with-expr.md)
