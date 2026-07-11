# Designing a Plugin System: Contracts, Registries, and Lifecycle in Production — Concepts

A plugin system is where interface design stops being an academic exercise and
starts being an operations problem. The moment your host loads code it did not
write — a storage backend selected by a config string, a third-party processor
shipped on its own release cadence, an internal team's transform registered from
an `init()` — the interface you defined becomes a contract you must defend
against concurrency, partial failure, cancellation, and version drift. The
question is never "how do I define an interface." It is "how does this interface
survive third-party code in production." This file is the conceptual foundation
for the nine independent modules that follow; read it once and every module has
its rationale.

## Concepts

### The interface is the contract, and the host owns it

In a plugin system the host defines the interface and the plugin implements it.
That inversion is the entire point: the host says "give me something with these
methods and I will call them at these times," and any type satisfying the method
set — from this binary, a vendored module, or a third party — plugs in without
the host importing it by name. Go's structural (implicit) interface satisfaction
is what makes this cheap: a plugin author writes the methods and is done, no
`implements` keyword, no registration with the type system.

Because every method on the interface is a method every plugin author must write,
the required interface is a tax. Keep it minimal. The canonical lifecycle is four
methods — `Name`, `Init`, `Process`, `Shutdown` — and you should resist growing
it. A fat `Plugin` interface with ten methods forces every author to implement
ten even when the host uses three, and it makes the interface brittle: adding a
method breaks every existing plugin at compile time.

### Capability growth belongs in optional interfaces, not a fatter contract

When some plugins need more than the base contract — a health check, a hot
reload, a human-readable description — the Go idiom is not to widen `Plugin`. It
is to define each extra behavior as its own small interface and discover it at
runtime with a comma-ok type assertion:

```go
if hc, ok := p.(HealthChecker); ok {
	err = hc.Check(ctx)
}
```

The host probes each plugin for the capability and calls it only when present;
plugins that do not implement it are simply skipped. This is exactly how the
standard library works internally: `io.Copy` probes its arguments for
`io.WriterTo` and `io.ReaderFrom` to take a fast path, falling back to the base
`Reader`/`Writer` when they are absent. Optional interfaces let the contract grow
without breaking a single existing plugin.

### Lifecycle ordering is a correctness property

The lifecycle is not a convenience; it is a set of invariants the host must
guarantee. `Init` runs exactly once, before any `Process`. `Process` may run many
times, possibly concurrently. `Shutdown` runs once, after the last `Process`. The
subtle hazard is registration: if the registry stores a plugin in its map *before*
`Init` returns nil, a failed `Init` leaves a half-initialized plugin reachable,
and the first `Process` call panics or misbehaves. The fix is a discipline —
register only after `Init` succeeds, and roll back (do not store) on failure.
Use-before-init is a design bug, not a plugin bug.

### Registration by value versus by constructor

Storing a live plugin instance in the registry is simple and fine when the host
constructs the plugins itself. But it couples "this plugin is known" to "this
plugin instance is alive," and it cannot support a plugin registering itself
before the host has decided to use it. The `database/sql` model solves both:
plugins register a *constructor* — `Register(name string, factory func() (Plugin, error))`
— in a package-level table, usually from an `init()`, and the host calls
`Open(name)` to build a fresh, independently-configured instance on demand. This
decouples the set of known backends from the set of live ones, enables
`init()`-time self-registration without import cycles, and guarantees each `Open`
returns its own instance so two logically separate plugins never share mutable
state. `database/sql.Register` panics on a duplicate name; that fail-fast is
deliberate, because a duplicate driver name is a programming error discoverable at
startup.

### A registry is read from every request goroutine

In a running server the registry is looked up by many request goroutines while an
admin path occasionally mutates it. That is a classic reader-writer workload, so
guard the map with a `sync.RWMutex`: `RLock` for `Run`/lookup, `Lock` for
`Register`/`Deregister`. The second, easy-to-miss rule: any method that returns
internal state must hand back a *copy*, never the live map or a slice backed by
it. A `List()` that returns the internal map lets a caller iterate it while the
mutator writes it — a data race that only `-race` reveals and that no amount of
locking inside `List` prevents, because the race is at the caller. Snapshot with
`maps.Clone` or a copied, sorted slice. `go test -race` is the ground truth here,
not code review.

### Context threading bounds untrusted work

`Process(input string)` gives the host no way to stop a slow or hung plugin.
`Process(ctx context.Context, input string)` does: the host wraps each call in
`context.WithTimeout` so one plugin cannot blow a request's latency budget, and it
can cancel all in-flight work on shutdown. But a context is only as good as the
plugin's cooperation — a plugin that accepts `ctx` and never checks `ctx.Done()`
or `ctx.Err()` gives false safety; the deadline fires and the goroutine keeps
running. Honoring cancellation means selecting on `ctx.Done()` at every blocking
point and checking `ctx.Err()` at loop boundaries. The host bounds; the plugin
must observe.

### Shutdown is reverse-order and failure-tolerant

Graceful shutdown on SIGTERM is where sloppy plugin hosts leak connections and
hang. Two properties matter. First, tear down in reverse registration order:
plugins registered later may depend on earlier ones, so last-in-first-out is the
safe teardown, mirroring how deferred cleanup and dependency graphs unwind.
Second, do not stop at the first error. If plugin B's `Shutdown` fails, plugins A
and C must still be torn down; collect every error with `errors.Join` and return
the aggregate so the operator sees all failures, not just the first. Bound each
`Shutdown` with its own context so one hung plugin cannot block the process from
exiting.

### Decorators compose because they satisfy the same interface

Cross-cutting concerns — logging, metrics, bounded retries — should be added
uniformly without editing plugin code. Because a decorator *is* a `Plugin` (it
embeds one and satisfies the same interface), it wraps any plugin transparently
and can be stacked in any order: `WithLogging(WithMetrics(WithRetry(p)))`. The one
invariant a decorator must preserve is `Name()` — the registry identifies plugins
by name, so a decorator that changed the name would break lookup. Delegate
`Name()` straight through and only wrap `Process`.

### Version negotiation makes ABI drift explicit

A long-lived host and independently-shipped plugins drift. A plugin built against
an older contract, loaded blind, crashes deep inside `Process` at the worst
possible time. Add an explicit `APIVersion() int` (or a semver struct) that the
host checks *at registration*, rejecting anything outside its supported range with
a typed error carrying the got-and-wanted numbers. That turns a latent runtime
panic into a clean, actionable startup rejection. Use a typed error with `Unwrap`
so callers can both `errors.Is` it against a sentinel and `errors.As` it to
recover the version detail.

### Fail fast at wiring time

Config-driven loading — a list of `{name, params}` decoded from JSON that selects
and orders plugins — is what lets an operator change behavior without recompiling.
The design rule is validate-before-init: each plugin exposes `Validate(params)
error` that the host runs on *all* plugins before it `Init`s *any*, aggregating
every config error with `errors.Join`. An operator then sees every misconfiguration
at startup — an unknown plugin name and a bad parameter reported together — rather
than discovering them one request at a time in production. Fail fast, fail with
the complete picture, and never half-initialize a pipeline.

## Common Mistakes

### A fat required interface

Wrong: a `Plugin` interface with ten methods so every author must implement all
ten even when the host calls three. Fix: a tiny required interface
(`Name`/`Init`/`Process`/`Shutdown`) plus optional interfaces discovered by a
comma-ok type assertion for the extras.

### Storing the plugin before Init succeeds

Wrong: `r.plugins[p.Name()] = p` and then `p.Init()`, leaving a half-initialized
plugin reachable when `Init` fails, so the first `Process` panics. Fix: call
`Init` first and store only on success; roll back (do not store) on failure.

### Forgetting Shutdown on unload

Wrong: unloading a plugin without calling `Shutdown`, leaking file handles,
connections, or goroutines. Fix: the registry calls `Shutdown` on every plugin,
in reverse registration order.

### Stopping shutdown at the first error

Wrong: returning as soon as one `Shutdown` fails, so later plugins are never torn
down. Fix: continue through all of them and aggregate with `errors.Join`.

### Racing the plugin map

Wrong: reading and mutating the map from multiple goroutines without a lock, or
returning the live map from `List()` so a caller iterates it while a mutator
writes. Fix: a `sync.RWMutex` around every access and a defensive snapshot
(`maps.Clone` or a copied slice) from any accessor. `-race` is the arbiter.

### Passing a context that Process ignores

Wrong: `Process(ctx, input)` that never checks `ctx.Done()`/`ctx.Err()`, so
deadlines and cancellation are silently dropped. Fix: select on `ctx.Done()` at
blocking points and check `ctx.Err()` at loop boundaries.

### Comparing wrapped errors with ==

Wrong: `err == ErrNotFound` against an error that has been wrapped with `%w`, or
reaching for a concrete type without `errors.As`, so sentinel and typed errors
from plugins are missed. Fix: `errors.Is` for sentinels, `errors.As` for typed
errors.

### Sharing one mutable instance across registrations

Wrong: registering the same live instance under two names (or reusing it across
`Open` calls), so mutating one path corrupts the other. Fix: construct a fresh
instance per registration from a factory function.

### Assuming every plugin matches the current contract

Wrong: skipping the version check and letting an incompatible plugin fail deep in
`Process`. Fix: validate `APIVersion()` at registration and reject out-of-range
plugins with a typed error.

### Loop-variable aliasing on a pre-1.22 mental model

Wrong: `for _, p := range plugins { go func() { use(p) }() }` written defensively
with `p := p`, or worse without the fix on an old toolchain. Fix: on go 1.22+ the
loop variable is per-iteration, so `for _, p := range plugins` is safe to capture,
and `for i := range n` is the idiom for counted loops.

Next: [01-plugin-contract-and-registry.md](01-plugin-contract-and-registry.md)
