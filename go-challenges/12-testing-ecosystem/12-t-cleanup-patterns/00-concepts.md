# t.Cleanup and Hermetic Test Fixtures for Backend Services — Concepts

Every backend test suite lives or dies on fixture hygiene. A repository test that
leaks a database pool, a worker test that leaks a goroutine, or a config test that
mutates process-global environment or working directory will not just fail once —
it will flake intermittently and poison its neighbors, especially under `-race`
and `-parallel` in CI. A test that is not hermetic is a latent CI outage. The
primitive that makes fixtures hermetic and composable is `t.Cleanup`: it runs at
*test* exit rather than function exit, it runs on failure, it runs last-in
first-out so layered resources tear down in reverse build order, and it integrates
with the Go 1.24 `t.Context()` contract so teardown can wait on a graceful
shutdown. This file is the conceptual foundation; read it once and you have
everything you need to reason through each of the ten independent exercises that
follow.

## Concepts

### defer runs at function return; t.Cleanup runs at test completion

A `defer` executes when the enclosing *function* returns. `t.Cleanup(fn)` executes
when the *test* completes, after the test body and all of its subtests have
finished. For an ordinary serial leaf test these two instants coincide, and either
one works. They diverge — dangerously — the moment a test calls `t.Parallel()` or
spawns parallel subtests.

When a test calls `t.Parallel()`, its function does not run to completion inline.
It signals the test runner and *yields*: control returns from the function so the
parent can launch the rest of its siblings, and the paused body is resumed later,
after the parent function has already returned. Concretely, a parent test that
starts parallel subtests with `t.Run(...)` returns from its own body *before* those
subtests actually execute. Any `defer` in that parent fires at that early return —
tearing down a resource the not-yet-run subtests still depend on. `t.Cleanup`
registered on the parent instead runs after every subtest has completed. For
parallel tests, `t.Cleanup` is the only correct teardown hook.

### Cleanup runs on failure and on panic-recovery paths

`t.Fatal` and `t.FailNow` abort the current test by calling `runtime.Goexit`,
which unwinds the goroutine running its deferred functions — and the test runner's
deferred teardown is what invokes your registered `t.Cleanup` functions. So a
cleanup runs even after `t.Fatal`, whereas a plain statement written at the bottom
of the test body is skipped the instant a `Fatal` fires above it. That is exactly
why transactional rollback and resource release belong in `t.Cleanup`, not in
trailing statements: a database transaction that must roll back when an assertion
fails will not roll back if the rollback is the line after the failing assertion.

### LIFO ordering is a composition primitive

Cleanups run last-added, first-called. This is not an incidental detail; it is the
feature that makes fixtures compose. If you build a harness in dependency order —
a backing store, then an `httptest.Server` over that store, then a configured
`*http.Client` — and register a cleanup at each step, teardown runs client, then
server, then store, automatically. That is the reverse-dependency order you want,
with no manual sequencing: the client drains before the server closes before the
store frees, so nothing tears down a layer another layer is still using. The same
property is what lets a leak guard registered *first* run *last*, after every other
resource has been released, turning "no leaks" into an enforced invariant.

### The Go 1.24 t.Context() contract: canceled just before cleanups run

`t.Context()` returns a context that is canceled *just before* the test's cleanup
functions run. This is a deliberate contract, and it is the idiomatic way to join
background goroutines in a test. Bind a worker to `t.Context()`, register a
`t.Cleanup` that waits on the worker's done signal, and the sequence at test exit
is: the context is canceled, the worker observes `ctx.Done()` and drains and
exits, and the cleanup's `wg.Wait()` (or `<-done`) returns once it has. No leaked
goroutines, no manual cancel plumbing. The corollary is a trap: code *inside* a
cleanup that needs to make a fresh outbound call must use `context.Background()`
(or a fresh timeout), because `t.Context()` is already canceled by the time the
cleanup body runs.

### t.TempDir: a unique, auto-removed directory per call

`t.TempDir()` returns a unique directory, honoring `GOTMPDIR`, and registers its
own cleanup to remove the entire tree at test end. Each call returns a distinct
directory, so parallel tests never collide. It eliminates `os.RemoveAll`
boilerplate and, crucially, removes the tree *unconditionally* — including on
`t.Fatal`, where a manual `defer os.RemoveAll` inside the body would be skipped and
leave `/tmp` litter. Use it for any file-backed fixture: a config loader that
writes a rendered file, a cache that warms a file on disk, a migration runner that
reads a directory.

### Process-global mutators are Cleanup-based and forbidden under parallel

`t.Setenv` and `t.Chdir` are both implemented on top of `Cleanup`: they mutate a
process-global (an environment variable, the working directory), record the prior
value, and register a cleanup to restore it. Because that state is shared across
the whole process, they are forbidden under `t.Parallel()` or any parallel
ancestor: the runtime *panics* to prevent one test's mutation from corrupting a
concurrently running test. A test that touches env or cwd must stay serial, or you
must inject the dependency (pass config or a base path as a parameter) instead of
reaching for process globals. This is a hard constraint, not a style preference.

### testing.TB lets one fixture serve tests, benchmarks, and fuzz targets

`Cleanup`, `TempDir`, `Setenv`, `Helper`, and `Context` all live on the `testing.TB`
interface, which `*testing.T`, `*testing.B`, and `*testing.F` all satisfy. A seed
or fixture helper typed `func(tb testing.TB)` is therefore reusable identically
from a `Test`, a `Benchmark`, and a fuzz target — the same helper that hydrates a
repository for correctness tests also warms it for a throughput benchmark. On the
benchmark side, `B.Cleanup` pairs with the Go 1.24 `b.Loop()` form: setup and
teardown registered outside the `for b.Loop()` loop are excluded from the measured
window, so the seeded state is reused across iterations without re-registering
cleanups.

### A Cleanup inside a t.Helper fixture is the unit of hermeticity

The discipline that ties this together: each fixture owns *both* setup and
teardown. A `t.Helper()`-marked constructor that opens a resource and registers its
`t.Cleanup` in the same breath means the caller cannot forget to release it — the
release is not the caller's responsibility at all. Register the leak guard first
(so LIFO runs it last) and "no leaks" stops being a hope and becomes an invariant
the fixture enforces. Cleanups can even register further cleanups and can
themselves call `t.Fatal`/`t.Error`; a panic in one cleanup still lets the
remaining ones run, so teardown of independent resources is resilient — but shared
mutable teardown state must be guarded, because parallel subtests' cleanups can
interleave with the parent's.

## Common Mistakes

### Using defer for teardown in a parallel test

Wrong: `defer conn.Release()` in a test whose parallel subtests use `conn`. The
enclosing function yields at `t.Parallel()` (or returns after launching the
subtests) before the test has logically finished, so the resource is released while
the test is still running. Fix: register the release with `t.Cleanup`, which runs
at true test completion.

### Creating a resource and registering no teardown

Wrong: a test that opens a store or starts a goroutine and never releases it. The
resource leaks, the test is not hermetic, and later tests inherit the mess. Fix:
own setup and teardown together in a `t.Helper()` fixture that registers the
`t.Cleanup` — the caller literally cannot forget.

### Closing a resource twice

Wrong: closing a resource explicitly in the test body and again in a cleanup, so
the second close errors or panics. Fix: make `Close` idempotent (return `nil` on a
second call) and let only the fixture own the close.

### Adding t.Parallel to a test that uses t.Setenv or t.Chdir

Wrong: adding `t.Parallel()` to a test that calls `t.Setenv` or `t.Chdir`. The
runtime panics because these mutate process-global state shared across all tests.
Fix: keep such tests serial, or inject the config value or base path as a
dependency rather than touching env or cwd.

### Assuming t.Context() is still alive during cleanup

Wrong: calling an outbound API with `t.Context()` from inside a cleanup. The
context is canceled just *before* cleanups run, so the call fails immediately with
a canceled context. Fix: use `context.Background()` or a fresh timeout for work
that must happen during teardown.

### Relying on defer/RemoveAll for temp files instead of t.TempDir

Wrong: `dir, _ := os.MkdirTemp(...); defer os.RemoveAll(dir)` inside the test body.
The `defer` is skipped on `t.Fatal`, littering `/tmp` on every failure. Fix:
`t.TempDir()` removes the tree unconditionally at test end.

### Registering the leak guard last, expecting it to run first

Wrong: registering the global leak assertion after acquiring resources, expecting
it to run before they are torn down. LIFO means last-registered runs first, so the
guard fires *before* the resources are released and false-fails. Fix: register the
guard first so it runs last, after everything else has been released.

### Polling runtime.NumGoroutine for leak detection

Wrong: snapshotting `runtime.NumGoroutine()` and asserting it did not grow. It is
racy against the scheduler and against other tests, so it flakes. Fix: track your
fixture's own goroutines and connections with an explicit `atomic.Int64` counter
and assert *that* in the cleanup; keep `NumGoroutine` as an observational log only.

Next: [01-repo-fixture-cleanup.md](01-repo-fixture-cleanup.md)
