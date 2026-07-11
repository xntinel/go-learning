# Test Helpers: Fixtures, Assertions, and Lifecycle — Concepts

On a real backend the test *helper* is the load-bearing layer of a service's test
suite. It is what makes four hundred tests readable, fast, and honest instead of
four hundred copies of the same twelve-line setup. Juniors treat a helper as
"code I extracted to stop repeating myself." A senior treats a helper as a small,
correct, reusable production API with a contract: it takes a `testing.TB`, it
either builds a resource and owns its teardown or asserts one clearly-named
thing, and it attributes its failures to the caller. Get the contract wrong and a
green suite quietly turns flaky under `-shuffle`, a worker-fanout assertion hangs
the run, or a failure message points at the helper instead of the test that
broke. This file is the conceptual foundation for the ten independent exercises
that follow; read it once and you have the model you need for all of them.

## Concepts

### What `t.Helper()` actually changes — and what it does not

`t.Helper()` does exactly one thing: it marks the calling function as a helper so
that when the runner prints the `file:line` prefix for a failure, it *skips this
frame* and reports the caller's location instead. That is the whole mechanism. It
does **not** change control flow, it does not change failure semantics, it does
not make `t.Fatal` behave differently. A helper that calls `t.Fatalf` still
stops the test the same way; `t.Helper()` only fixes *where the failure is
attributed*.

Two consequences follow. First, `t.Helper()` should be the first statement of the
helper — placing it after other statements that might themselves report a failure
means those earlier reports are attributed wrongly. Second, attribution is
per-frame: in a chain where `assertUserSaved` calls `assertJSONEqual` which calls
`equal`, *every* level must call `t.Helper()`. The runner walks up the stack
skipping consecutive helper frames; the first frame that omitted `t.Helper()`
stops the walk, and the failure is attributed there. One missing call in the
middle of the chain and the `file:line` points into your helper library instead
of the test.

### A helper is an API with a contract, not just deduplication

A helper has a signature you design deliberately. Its first parameter is the
testing handle (`testing.TB` or `*testing.T`); it names what it asserts or builds;
and it does exactly one of two jobs. A *fixture* helper builds a dependency and
returns a value the caller uses (a server, a repository, a client). An *assertion*
helper checks one thing and fails the test itself. Mixing the two — a helper that
both builds three things and asserts five branch conditions — hides the behavior
under test and is the classic "test logic buried in a helper" smell.

Inside an assertion helper the choice between `t.Error` and `t.Fatal` is a real
API decision. `t.Fatal` (via `FailNow`) stops the current test immediately, so a
`requireNoError` that fails means the following lines never run — correct when the
rest of the test cannot proceed without the resource. `t.Error` (via `Fail`)
records the failure and lets execution continue, accumulating multiple failures in
one run — correct when you want to report every mismatched field of a struct
rather than dying on the first. Naming encodes the choice: `require*` for fatal,
`assert*`/`expect*` for accumulating, mirrors the convention every Go engineer
already reads.

### Type helpers over `testing.TB`, not `*testing.T`

`testing.TB` is the interface common to `*testing.T`, `*testing.B`, and
`*testing.F`. If you type an assertion helper as `func equal[T comparable](t
testing.TB, got, want T)`, the identical helper works inside a `TestXxx`, a
`BenchmarkXxx`, and a `FuzzXxx`. Type it as `*testing.T` and you have gratuitously
locked it out of benchmarks and fuzz targets. The rule of thumb: an assertion or
fixture helper that does not need `*testing.T`-specific methods (`T.Run`,
`T.Parallel`, `T.Deadline`) should take `testing.TB`.

There is a sharp corner here. `testing.TB` is a *sealed* interface: it contains an
unexported method, so you cannot implement it yourself. That is deliberate — the
Go team reserves the right to add methods. The practical fallout is that when you
want to *unit-test your own helper* (prove `requireNoError` actually fails on a
non-nil error), you cannot hand it a fake `*testing.T`. The idiomatic answer is to
type the helper over a *tiny purpose-built interface* that lists only the methods
the helper uses (`Helper`, `Fatalf`), which both a real `*testing.T` and your
recording fake satisfy. Testify's `TestingT` is exactly this pattern.

### Fixture helpers own their teardown with `t.Cleanup`

The old style returned a cleanup function the caller had to remember to defer:

```go
srv, cleanup := newTestServer(t)
defer cleanup() // one forgotten defer leaks the listener
```

The modern style registers teardown *inside* the helper and returns only the
resource:

```go
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}
```

Now the caller cannot forget to clean up, because there is nothing to remember.
This matters under `-shuffle` and `-count=N`: a single leaked server, goroutine,
or temp file makes the suite flaky in ways that only appear on some runs. Owning
teardown at the point of acquisition is the single most important fixture
discipline.

`t.Cleanup` functions run when the test *and all its subtests* complete, in
**last-added-first-called (LIFO)** order — the mirror image of `defer`. LIFO is
what makes acquisition order the reverse of teardown order: if a test acquires a
database and then a server that talks to it, the server tears down first and the
database second, exactly as you want. Reasoning about that ordering is part of
owning a fixture that composes with others.

### `t.Context()` and the shutdown of background goroutines

Since Go 1.24, `t.Context()` returns a context that "is canceled just before
Cleanup-registered functions are called." That single guarantee is the correct
hook for a fixture that runs a background goroutine. Wire the goroutine's shutdown
to `<-t.Context().Done()`; register a `t.Cleanup` that joins it. Because the
context is canceled *before* cleanups run, by the time your cleanup calls
`wg.Wait()` the goroutine has already been told to stop, so the join returns
promptly instead of blocking forever. A fixture that starts a goroutine but never
gives it a stop signal will block in `Cleanup` and hang the whole test binary — a
failure that looks like "the test suite froze" and is miserable to diagnose.

### The two canonical substrates: `httptest.NewServer` and `t.TempDir`

`httptest.NewServer(handler)` starts a real HTTP server on a random loopback port
and returns a `*httptest.Server` whose `.URL` and `.Client()` give you a wired-up
address and client. It is the canonical way to test an `http.Handler`
end-to-end without binding a fixed port (which would collide under parallelism).
The fixture helper registers `srv.Close` via `t.Cleanup` so the listener and
connections are released deterministically.

`t.TempDir()` returns a unique directory removed automatically on completion. It
is the substrate for any file-backed fixture — a repository, a cache, a config
file. Each call, including one per subtest, yields a *fresh* directory, which is
what gives parallel subtests isolated on-disk state with zero manual cleanup. A
repository fixture rooted in `t.TempDir()` cannot cross-contaminate another
subtest because they physically do not share a directory.

### The concurrency rule that bites in production tests

`FailNow`, `Fatal`, and `SkipNow` "must be called from the goroutine running the
test or benchmark function, not from other goroutines created during the test."
This is a documented, hard rule, not a style preference. `Fatal` works by calling
`runtime.Goexit`, which unwinds *the calling goroutine* — call it from a worker
goroutine and you kill the worker, not the test, leaving the test to continue (or
hang) on broken state. The observable symptoms range from a hung run to a test
that passes despite a real failure.

The consequence for helpers used inside `errgroup` or worker fanout: they must not
call `t.Fatal`. Either they accumulate with `t.Error` (which *is* safe from any
goroutine, because it only records) or they marshal the failure back to the test
goroutine over a channel or a mutex-guarded collector, and the test goroutine
makes the final `t.Fatal`/`t.FailNow` call. "Validate on N workers, drain on the
test goroutine" is the shape.

### `t.Setenv` / `t.Chdir` are process-global and therefore serial

`t.Setenv(k, v)` sets an environment variable and registers a `Cleanup` to restore
the old value — convenient for a config-loader test. But the environment and the
working directory are *process-global* state, so `t.Setenv` and `t.Chdir`
**cannot be used in a parallel test or a test with a parallel ancestor** — doing
so *panics at runtime*. A helper built on `t.Setenv` therefore encodes a hard
constraint: any test using it is inherently serial, and you must not add
`t.Parallel()` to it. A senior documents that contract on the helper rather than
letting a teammate discover it via a mid-run panic.

### Golden-file helpers: the `-update` flag and `testdata/`

A snapshot (golden-file) helper compares a produced artifact — a rendered
template, a serialized API response — against committed bytes in
`testdata/<name>.golden`. The standard shape is a package-level flag
`var update = flag.Bool("update", false, ...)`: a normal `go test` run *compares*
`got` against the golden and prints a readable diff on mismatch, while
`go test -update` *rewrites* the golden. The directory name `testdata/` is
special: the `go` tool ignores it when building, so it is the standard home for
fixtures. Two failure modes to avoid: a helper that *always* writes the file (no
`-update` guard) can never fail, defeating the point; and a helper that reads or
writes `testdata` without accounting for concurrent subtests can race under
`-race`.

### The reusable case-runner

Table-driven tests are the Go idiom, but the `for _, c := range cases { t.Run(...)
}` scaffolding is itself repeated in every file. A generic case-runner —
`runCases[C any](t, cases, name, run)` — collapses that scaffolding into one line
per test while preserving `t.Run` subtests, per-case `t.Parallel`, and correct
`t.Helper` attribution. It is the shared harness a service package imports across
dozens of `*_test.go` files, and it is the natural capstone that ties the
generics, subtest, and attribution ideas together.

## Common Mistakes

### Omitting `t.Helper()`, or breaking the chain

Wrong: a helper without `t.Helper()`, or one that calls it after other statements,
so a failure points at the helper's line. Equally wrong: a helper that calls
another helper where only the outer one calls `t.Helper()` — attribution stops at
the inner frame.

Fix: `t.Helper()` as the first statement of *every* helper in the call chain.

### Typing an assertion helper as `*testing.T`

Wrong: `func equal(t *testing.T, got, want int)` when it needs nothing
`T`-specific — now it cannot be reused from a benchmark or fuzz target.

Fix: type it over `testing.TB`. And do not try to hand-implement `testing.TB` for
a fake — it has an unexported method; use a minimal purpose-built interface
instead.

### Returning a cleanup func the caller must defer

Wrong: `srv, cleanup := newServer(t); defer cleanup()`. One forgotten defer leaks
a server, goroutine, or temp file and makes the suite flaky under `-shuffle` or
`-count=N`.

Fix: register `t.Cleanup` *inside* the fixture helper and return only the
resource.

### Calling `t.Fatal` from a spawned goroutine

Wrong: a worker goroutine (in an `errgroup` or `go func()`) that calls
`t.Fatal`/`t.FailNow`. That is undefined behavior — `Goexit` unwinds the worker,
not the test, and can hang the run or silently pass a broken test.

Fix: accumulate failures with `t.Error` (safe from any goroutine) or channel them
back to the test goroutine, which makes the final fatal call.

### Combining a `t.Setenv` helper with `t.Parallel`

Wrong: a config helper that calls `t.Setenv` used in a test that also calls
`t.Parallel()` — this panics at runtime because the environment is process-global.

Fix: document that the helper is inherently serial; never add `t.Parallel()` to a
test that uses it.

### Burying real test logic inside a helper

Wrong: a helper with branching assertions and hidden behavior under test, so the
reader cannot see what the test actually checks.

Fix: a helper asserts or builds *one* clearly-named thing; keep the behavior under
test visible in the test body.

### A golden helper with no `-update` guard, or one that races

Wrong: a helper that always writes the golden file (tests can never fail), or one
that reads and writes a shared `testdata` path from concurrent subtests under
`-race`.

Fix: guard writing behind a `-update` flag; give each concurrent case a distinct
golden name or serialize access.

### Sharing mutable fixture state across parallel subtests

Wrong: building one `httptest.Server` or temp DB and mutating it from parallel
subtests — cross-contamination. Or the reverse: rebuilding an expensive read-only
fixture per subtest for no reason.

Fix: share read-only fixtures; give each parallel subtest its own `t.TempDir()`
and its own mutable state.

### Ignoring `t.Context()` cancellation in a background-goroutine fixture

Wrong: a fixture that starts a goroutine but never wires it to `t.Context()`
cancellation, then blocks forever in `Cleanup` joining a goroutine that was never
told to stop.

Fix: watch `<-t.Context().Done()` in the goroutine; the context is canceled just
before cleanups run, so the join in `Cleanup` returns promptly.

Next: [01-validator-assert-helpers.md](01-validator-assert-helpers.md)
