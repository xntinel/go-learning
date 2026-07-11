# Init Functions and Package Initialization — Concepts

Package initialization is where a service quietly decides whether it can even
start, and where hidden global state is born. `init()` runs before `main`, before
any flag is parsed, before a structured logger exists, before there is a
`context.Context` to cancel anything. That timing is the whole story: whatever
`init()` does cannot be parameterized by config, cannot be cancelled, cannot be
observed by the program that is starting, and silently does not run at all in a
test binary that never imports the package. Treat `init()` as a narrow tool for
exactly two production-legitimate jobs — side-effect registration behind a blank
import, and fail-fast invariant checks that abort a structurally broken binary —
and move everything else (opening pools, reading env, dialing services, starting
goroutines) into an explicit constructor or a `sync.Once`-guarded lazy path so it
is testable, cancelable, orderable, and injectable. This file is the conceptual
foundation for the nine independent exercises that follow.

## Concepts

### The initialization order is fully specified and dependency-driven

For a single package, Go initializes in a fixed sequence. First, every imported
package is initialized, each exactly once, transitively, before the importing
package's own initialization begins. Second, the package's own package-level
variables are initialized in *dependency order*, not source order: if
`var a = b + 1` appears above `var b = 2`, Go still initializes `b` first because
`a` depends on it. Variables with no dependencies among each other are
initialized in the order they are declared. Third, after all package-level
variables are set, the `init()` functions run, in the order they are presented to
the compiler — which for `go build` means files are processed in filename order,
and within a file the `init()` functions run top to bottom.

The practical consequence: never write code that assumes package-level variables
are assigned in the textual order you wrote them, and never let two `init()`
functions coordinate across files by relying on which runs first. Dependency
ordering is guaranteed; textual and cross-file ordering is a filename-sort
accident. Relying on the looser rules produces zero-value and nil-pointer bugs
that only surface at runtime, long after the package "compiled fine".

### init() runs in a context-free, config-free vacuum

`init()` executes as part of package initialization, which completes before
`main` is entered. There is no way for `main` to install configuration, parse
flags, construct a logger, or create a context *before* `init()` observes it,
because `init()` already ran. So anything `init()` reads from the environment is
frozen at import time; anything it dials cannot honor a deadline; anything it logs
goes to whatever the default logger is; any goroutine it starts has no cancel
signal. This is precisely why connections, env reads, and background goroutines do
not belong in `init()` — the machinery that would make them correct does not exist
yet.

### A package's init() only runs if the package is imported

`init()` fires when the package is linked into the final binary and initialized —
which only happens if something in the build graph imports it. A unit test binary
that does not import the package silently skips its `init()`. That means
init-based wiring can behave differently in tests than in production: the driver
that "registers itself" is simply absent in a test that forgot the blank import,
and the failure looks like "driver not found" rather than "you forgot to import
it". Explicit registration from `main` makes the active set visible and keeps it
under the test's control.

### Legitimate use one: side-effect registration behind a blank import

The canonical good use of `init()` is self-registration into a shared registry,
activated by a blank import. `database/sql` drivers call `sql.Register(name, drv)`
in their `init()`, so `import _ "github.com/lib/pq"` makes the `postgres` driver
available without the importer taking a direct symbol dependency on it. The
`image` package's PNG/JPEG/GIF decoders and `net/http/pprof`'s handlers work the
same way. The pattern lets a caller opt into a plugin by import path alone, which
is exactly what you want for a set of interchangeable codecs or drivers selected
at build time. The contract that keeps it honest: registering a duplicate name
*panics* (that is what `sql.Register` does), so a mis-wired double import fails
loudly at startup instead of silently shadowing a codec later.

### Legitimate use two: fail-fast invariant checks

The second defensible use is aborting a structurally broken binary before it can
accept traffic. `regexp.MustCompile` and `template.Must` encode this contract
exactly: a static pattern or template that fails to compile is a build-time
mistake, and panicking at package initialization stops the binary from ever
starting. The same reasoning covers verifying that every expected `go:embed`
asset is present and that precompiled templates parse. These checks are cheap,
depend on nothing external, and have a correct answer known at build time; a
failure means the binary itself is wrong, and crashing at load is the right
response.

### sync.Once and its typed helpers are the modern lazy alternative

When a resource is expensive and should be built on first use rather than at
import time, `sync.Once` and its Go 1.21 typed helpers are the tool:
`sync.OnceValue(f)` returns a function that runs `f` at most once and caches its
single result; `sync.OnceValues` does the same for a two-value factory (value plus
error); `sync.OnceFunc` wraps a side-effecting function. All are safe under
concurrent callers — the factory runs exactly once even if a thousand goroutines
race into it — and, importantly, they propagate a panic from the factory to every
caller and to re-invocations. The payoff over an `init()`-built global: the cost
is skippable. A test that never touches the singleton never pays for it, and the
lazy path can be constructed with real dependencies rather than import-time
defaults. Reach for these instead of hand-rolling a `sync.Once` plus a `bool`
plus a value plus a mutex.

### Import cycles are impossible, which is why the registry owns the interface

Go forbids import cycles at compile time. A registry cannot import its drivers to
register them while those drivers import the registry to satisfy its interface —
the compiler rejects the cycle outright. The structural resolution is the
base-package + subpackage split: the registry package defines the `Driver` and
`Conn` interfaces and the `Register`/`Open` machinery; each driver is a
subpackage that imports the registry to implement the interface; and a wiring
layer (`main`, or a thin `all` package) imports both and calls `Register`
explicitly. This is not merely a workaround for the cycle rule — it is what keeps
the registry testable, because the test constructs its own `Registry`, registers
fakes, and exercises the contract with no dependency on any real driver.

### Package-level variable initializers are init-time side effects too

`var x = compute()` at package scope is not free: `compute()` runs during package
initialization, on the same context-free, config-free path as `init()`. If
`compute()` reads external state or can fail, that failure has nowhere to go
except a panic during load, and its inputs cannot be injected by a test. A
package-level `var db = openDB()` is exactly as problematic as opening the pool
inside `init()`; both must move into a constructor or a lazy `OnceValue`.

### TestMain is the correct hook for ordered, teardown-capable test setup

A test-file `init()` cannot tear anything down (there is no teardown hook), cannot
see the run result, and cannot control ordering relative to other setup.
`func TestMain(m *testing.M)` is the correct place for shared setup that needs a
teardown or an exit code: it sets up the fixture, calls `m.Run()`, tears down, and
calls `os.Exit(code)` with the run's result. Per-test teardown belongs in
`t.Cleanup`. Putting shared fixtures in a test `init()` is the mistake `TestMain`
exists to prevent.

### Functional-options constructors kill hidden import-time configuration

The refactor that removes hidden global state converts `init()`-driven
configuration into an explicit `Load(...Option) (*Config, error)` constructor.
Inject the environment lookup as a `getenv func(string) string` so tests supply a
fake map instead of mutating the process environment; use functional options for
overrides; and aggregate every validation failure at once with `errors.Join` so a
single `Load` reports all missing keys rather than the first. The result is
deterministic, injectable, and reset-per-test — none of which an `init()`-built
global can offer.

## Common Mistakes

### Using init() to wire production dependencies

Wrong: a package's `init()` opens a database pool, reads env or a config file,
dials a remote service, or starts a goroutine. It runs before config and logging
exist, cannot be cancelled, and silently no-ops in any test that does not import
the package. Fix: expose an explicit constructor (`New`/`Load`) called from
`main`, or a lazy `sync.OnceValue` path.

### Assuming init() functions run in a chosen cross-package order

Wrong: relying on package A's `init()` running before package B's. The order is
fixed by the import graph and filename sort, not by developer intent. Fix: never
let two `init()` functions coordinate; register explicitly from `main`.

### Assuming package-level vars initialize in source order

Wrong: writing code that reads one package var from another assuming textual
position determines order. Go initializes in dependency order, so a variable can
be ready before an earlier-declared one, and code that assumes otherwise reads a
zero value or nil pointer. Fix: rely on the documented dependency ordering, never
on where a declaration sits in the file.

### Storing state in a package-level global set by init()

Wrong: `var globalConn = openDB()` or a global filled in `init()`. Tests cannot
reset it, it lives for the whole process, and its failure can only panic at load.
Fix: build it in `New()`/`Load()` and inject it, or make it lazy with
`sync.OnceValue`.

### Creating a registry/driver import cycle

Wrong: the registry imports the drivers to register them in `init()` while the
drivers import the registry for the interface. The compiler rejects the cycle.
Fix: the registry owns the interface, drivers import the registry, and wiring code
imports both and registers explicitly.

### Ignoring a duplicate self-registration

Wrong: letting a second `Register` of the same name silently overwrite or no-op.
The `database/sql` contract panics on duplicate `Register` precisely so a
mis-wired blank import fails loudly at startup rather than shadowing a codec
later. Fix: panic (or hard-fail) on duplicate registration.

### Putting shared test setup in a test-file init()

Wrong: a test `init()` that seeds a shared fixture, then discovering there is no
teardown hook and no access to the exit code. Fix: use `TestMain(m *testing.M)`
with `m.Run()` and `os.Exit`, plus `t.Cleanup` for per-test teardown.

### Hand-rolling sync.Once for a lazy singleton

Wrong: a bespoke `sync.Once` + `bool` + value + mutex for a lazy value.
`sync.OnceValue`/`OnceValues` already encode it correctly, including panic
propagation on re-call. Fix: use the `Once*` helpers.

### Doing heavy work at import time

Wrong: network calls or large-file parsing at package scope, so that merely
importing the package — in a linter, a tool, or a fast unit test — pays the cost.
Fix: make it lazy or explicit; reserve `init()` for cheap validation and
registration.

### Compiling a regexp per request instead of once at package scope

Wrong: `regexp.Compile` per request in a hot path, or `regexp.Compile` at init
with the error dropped. `regexp.MustCompile` at package scope is the intended
pattern for a static pattern: it fails fast at load and compiles exactly once.
Per-request compilation is a real latency and allocation bug. Fix: precompile once
with `MustCompile`.

Next: [01-driver-registry-contract.md](01-driver-registry-contract.md)
