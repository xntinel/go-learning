# TestMain: Package-Level Setup, Teardown, and Suite Lifecycle — Concepts

Every backend test package eventually needs something that must exist exactly
once for the whole suite: a database pool with migrations applied, a running HTTP
server, a message broker connection, a seeded set of fixtures, a normalized
timezone, a silenced logger. Doing that work inside each test is slow, flaky, and
duplicated; doing it in an `init()` gives you no place to tear it down and no
control over ordering. `TestMain` is the one hook the `testing` package gives you
for this: a function that replaces the default test runner for a single package,
runs once in the main goroutine before any `Test`/`Benchmark`/`Example`, and owns
the process exit code. The senior-level material is not "call `m.Run()`". It is
the failure modes around that call — the ones that leak containers in CI, turn a
red suite green, and leak global state into sibling packages — and the adjacent
techniques (subprocess re-exec, goroutine-leak gating) that live in the same
lifecycle slot. This file is the conceptual foundation for the nine independent
exercises that follow; each one is a production-shaped harness you can lift into a
real service.

## Concepts

### What TestMain actually is, and the contract it must honor

When a test package declares `func TestMain(m *testing.M)`, the generated test
binary calls *your* function instead of the default runner. That means the whole
responsibility for running the tests and setting the process exit status is yours.
The contract is exact and unforgiving:

- You **must** call `m.Run()`. It runs all the tests/benchmarks/examples selected
  by the usual flags and returns an `int` exit code (0 = all passed, non-zero =
  something failed).
- You **must** propagate that code with `os.Exit(m.Run())` (or
  `os.Exit(code)` after capturing `code := m.Run()`). Returning from `TestMain`
  without calling `os.Exit`, or calling `os.Exit(0)` unconditionally, decouples
  the process exit status from the test result. The tests still print their
  failures, but the binary exits 0, and CI reads exit 0 as "green". This is the
  single most dangerous TestMain bug: a suite that is silently always green.

`TestMain` is optional. A package with no `TestMain` gets the default runner,
which is equivalent to `func TestMain(m *testing.M) { os.Exit(m.Run()) }`. You add
one only when you have package-scoped setup/teardown or custom flags to parse.

### It runs once, in the main goroutine, before everything

`TestMain` runs exactly once per package — not per test, not per subtest — and it
runs on the main goroutine before any test function starts, including before any
`t.Parallel()` test is unblocked. That timing is precisely why it is the correct
place to provision shared, expensive resources: it is the natural serialization
point. Nothing races your setup because nothing else has started yet. When
`m.Run()` returns, every test (including parallel ones) has finished, so it is
equally the right place to tear those resources down. Anything that needs to be
*fresh per test* does not belong here — that is what the test body, `t.Cleanup`,
and `t.TempDir` are for. `TestMain` is for the once-per-package surface.

### os.Exit does not run deferred functions — the run() int wrapper

Here is the trap that leaks resources in CI. `os.Exit` terminates the process
immediately; it does **not** run deferred functions. So this is broken:

```go
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "fixtures")
	defer os.RemoveAll(dir) // NEVER RUNS
	os.Exit(m.Run())        // exits here, skipping the defer above
}
```

The temp dir (or the Docker container, or the DB connection, or the open file)
leaks on every run. The idiom that fixes it is to push all the work — and all the
defers — into a helper that *returns* a code, and reduce `TestMain` to a single
line whose only job is the `os.Exit`:

```go
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "fixtures")
	if err != nil {
		return 1
	}
	defer os.RemoveAll(dir) // runs, because run() returns normally
	return m.Run()
}
```

Because `run` returns normally (it does not call `os.Exit`), its deferred cleanup
executes before control returns to `TestMain`, and only then does `os.Exit` fire.
This `run() int` wrapper is the backbone pattern of the whole lesson: every
serious setup/teardown harness uses it.

### Global state must be saved and restored, or it leaks across packages

A test binary is compiled per package, but a lot of the state a suite mutates is
*process* global: `slog.Default()`, `time.Local`, process environment variables,
feature-flag singletons. If `TestMain` mutates one of these and does not put it
back, the mutation leaks. Within the same package it produces order-dependent
failures (a test that assumed the default logger now sees a discarding one). Worse,
because `go test ./...` can compile several packages into test binaries that share
process-level defaults in some layouts, an unrestored `time.Local` or `TZ` becomes
a heisenbug that only shows up when packages run in a particular order. The rule
is mechanical: capture the prior value before `m.Run()`, restore it after, and do
the restore inside the `run()` wrapper so it actually executes.

Note also which APIs you do *not* have here. `t.Setenv`, `t.TempDir`,
`t.Cleanup`, and `t.Context` are methods on `*testing.T`, and inside `TestMain`
there is no `T` yet — the tests have not started. Package-level env and temp-dir
setup in `TestMain` must be done manually with `os.Setenv`/`os.MkdirTemp` and
explicit save/restore. And `t.Setenv` is doubly unavailable: it is per-test *and*
it panics if the test has called `t.Parallel()`, so it cannot express a
once-per-package env change even from within a test.

### Custom flags: declare at package scope, flag.Parse in TestMain

`go test` and your test binary share one flag set. If you want a custom flag —
`-integration`, `-update`, a DSN override — you declare it at package scope with
`flag.Bool`/`flag.String`, and you call `flag.Parse()` inside `TestMain` before
`m.Run()`. By the time `TestMain` is invoked, the testing package has already
registered all the `-test.*` flags, so a single `flag.Parse()` parses both the
framework's flags and yours together. Skip the `flag.Parse()` and your flag stays
at its zero value forever — the code compiles and the flag silently never turns
on. This is how the real golden-file and integration-gate workflows are wired.

### Slow vs. fast separation, and skipping a whole suite gracefully

Backends split their tests into a fast default run (units, always in CI) and a
slow opt-in run (integration against real infrastructure). Two levers do this.
`testing.Short()` reads the built-in `-short` flag; a slow test starts with
`if testing.Short() { t.Skip(...) }`. A custom gate flag like `-integration`
(default false) does the opt-in the other way: `if !*integration { t.Skip(...) }`.
`TestMain` is where you read the environment to decide whether the integration
surface is even *available* — is `TEST_DATABASE_DSN` set? — and skip or degrade
the entire suite gracefully when it is not, so a developer with no database can
still run the unit tests and see green.

### A shared mutable resource trades startup cost for isolation risk

Provisioning one database, one server, or one in-memory store for the whole
package is the point of `TestMain` — you pay the startup cost once. But it means
your tests now run against *common, mutable* state, often concurrently under
`t.Parallel()`. That is a real correctness hazard, not a detail. Tests must
isolate themselves: use unique keys per test, wrap each test in a transaction that
rolls back, or make the handler stateless so concurrency is safe. A stateless
`/healthz` handler is fine to hammer from twenty parallel tests; a shared
in-memory map is not, unless each test writes disjoint keys. The harness gives you
the resource; isolation is still your job.

### Testing code that calls os.Exit or log.Fatal: subprocess re-exec

You cannot test a function that calls `os.Exit(2)` (or `log.Fatal`, which calls
`os.Exit(1)`) in-process — it would terminate the test runner itself. The
documented technique is to re-exec the test binary as a child process guarded by
an environment variable. The test checks the guard: if it is set, it calls the
exiting function (and the *child* actually exits with the real code); if it is
not, it runs `exec.Command(os.Args[0], "-test.run=TheSameTest")` with the guard
set in the child's environment, waits for the child, and asserts the child's exit
status via `*exec.ExitError` and `ProcessState.ExitCode()`. This is the same
pattern the standard library uses for its own `os.Exit` tests, and it complements
`TestMain`: it is how you cover the very shutdown paths a lifecycle harness sets up.

### Enforcing package-scope invariants after m.Run()

Because `TestMain` sees the exit code from `m.Run()` before the process ends, it
can enforce invariants that no individual test can: no leaked goroutines, no open
file descriptors, a coverage floor. The mechanism is: run the suite, and if it
otherwise passed, inspect process state (e.g. `runtime.NumGoroutine()` against a
baseline captured before `m.Run()`), give any stragglers a brief settle window,
and override the exit code to non-zero if the invariant is violated. This is
exactly the mechanism behind tools like `go.uber.org/goleak`. It is inherently a
little flaky — goroutines exit asynchronously — so it must be written with
settle-and-retry polling, and it should only tighten a passing run (never mask a
failing one). The stdlib version you build here teaches the mechanism; a real
project would reach for goleak's hardened implementation.

## Common Mistakes

### Calling m.Run() but not os.Exit(m.Run())

Wrong: `func TestMain(m *testing.M) { setup(); m.Run() }`. The process exits 0
regardless of failures, and CI reports the suite green while tests are red.

Fix: `os.Exit(m.Run())`, or `code := m.Run(); os.Exit(code)`. The exit status must
be the test result.

### Putting teardown as a defer directly in TestMain

Wrong: `defer os.RemoveAll(dir)` (or `defer db.Close()`) written in `TestMain`
right before `os.Exit(m.Run())`. `os.Exit` skips defers, so the cleanup never
runs and CI accumulates leaked temp dirs, containers, and connections.

Fix: move all defers into a `func run(m *testing.M) int` wrapper that returns
`m.Run()`, and make `TestMain` just `os.Exit(run(m))`.

### Unconditional os.Exit(0) or ignoring m.Run()'s return

Wrong: `m.Run(); os.Exit(0)`. Same green-washing as forgetting `os.Exit`, but more
insidious because it looks deliberate.

Fix: never discard the code `m.Run()` returns; it is the whole point.

### Mutating global state without saving and restoring it

Wrong: `slog.SetDefault(quiet)` / `os.Setenv("TZ", "UTC")` / `time.Local = loc` in
`TestMain` with no restore. The change leaks into other packages and produces
order-dependent flakiness.

Fix: capture the prior value first (`prev := slog.Default()`,
`old, ok := os.LookupEnv("TZ")`), and restore it in a defer inside `run()`.

### Declaring a custom flag but forgetting flag.Parse()

Wrong: `var integration = flag.Bool("integration", false, "...")` with no
`flag.Parse()` in `TestMain`. The flag is stuck at `false`; `-integration` on the
command line does nothing.

Fix: call `flag.Parse()` in `TestMain` before `m.Run()`.

### Reaching for t.Setenv or t.TempDir inside TestMain

Wrong: trying to call `t.Setenv`/`t.TempDir`/`t.Cleanup` in `TestMain`. There is
no `*testing.T` there — the tests have not started. And `t.Setenv` additionally
panics in any test that called `t.Parallel()`.

Fix: use `os.Setenv`/`os.MkdirTemp` with manual save/restore in `TestMain`; keep
`t.Setenv`/`t.TempDir` for per-test needs in the test body.

### Sharing one mutable resource across parallel tests without isolation

Wrong: a single `*sql.DB` or in-memory store written by many `t.Parallel()` tests
using overlapping keys. Tests contaminate each other intermittently.

Fix: isolate — unique keys per test, per-test transactions with rollback, or a
stateless handler. The shared resource is fine; the shared *mutation* is not.

### Testing an os.Exit / log.Fatal path in-process

Wrong: calling the function that does `os.Exit(2)` directly from a test. It kills
the test runner, not the code under test.

Fix: re-exec the test binary as a subprocess guarded by an env var and assert the
child's exit code with `*exec.ExitError` / `ProcessState.ExitCode()`.

### Doing the once-per-package setup in every test body

Wrong: starting the server, opening the DB, and running migrations at the top of
each test. The suite is slow and the repeated setup is a flake source.

Fix: do it once in `TestMain`/`run()` and share it through a package var; let each
test isolate its own data.

### Assuming TestMain runs per test

Wrong: expecting `TestMain` to give each test a fresh resource. It runs exactly
once for the whole package.

Fix: per-test freshness belongs in the test or in `t.Cleanup`; `TestMain` is for
the shared, once-per-package surface only.

Next: [01-silence-default-logger.md](01-silence-default-logger.md)
