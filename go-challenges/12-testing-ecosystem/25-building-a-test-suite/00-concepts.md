# Building A Production Test Suite For A Cache Library — Concepts

A cache is the most test-sensitive component many backend engineers own. It has
concurrency (a `sync.RWMutex` guarding a map), time-dependent behavior (TTL
expiry), a serialization surface (a stats report), and it sits on the hot path
behind an HTTP handler where an allocation regression silently doubles GC
pressure under load. That single realistic artifact is enough to teach how a
senior engineer actually structures a test suite for a repository that will be
maintained for years: feature-organized top-level tests, table-driven cases,
shared helpers with correct failure attribution, suite-level fixtures via
`TestMain`, golden files with an `-update` workflow, `httptest` for the handler
that fronts the cache, deterministic time and concurrency with `testing/synctest`
instead of sleeps, a benchmark suite that guards allocations, and a fuzz target
for the key/value round-trip.

The through-line is that the suite is a living document and an operational asset.
It must run clean under `-race`, be fast, be deterministic — no flakes from
wall-clock timing or goroutine scheduling — and fail with messages that point at
the real defect. Each technique maps to a concrete production reason: flaky TTL
tests page people at 3am; an un-attributed helper failure sends the on-call to
the wrong file during an incident; an unguarded allocation regression in a cache
doubles GC pressure under load without anyone noticing until latency spikes.

## Concepts

### Feature-organized suites: one top-level Test per behavior

The first structural decision is how to lay out the top-level `Test` functions.
The pattern that survives years of maintenance is feature-per-function: one
`TestSet`, one `TestGet`, one `TestDelete`, one `TestLen`, with individual cases
as named subtests via `t.Run`. This is not about aesthetics. The payoff is
operational: `go test -run 'TestGet/expired'` runs exactly one case; a failure in
one behavior does not bury the output of every other; and a new engineer reading
the test file learns the API's surface from the function names alone. The
opposite — a single giant `TestCache` with every assertion inline — produces
unreadable output, offers no selective execution, and lets one early failure hide
the rest.

### Subtests versus table-driven cases

Subtests and table-driven tests are two tools for two shapes of problem. Use
named subtests (`t.Run("not_found", ...)`) when the cases are *heterogeneous* —
each needs its own setup, its own assertions, its own shape. Use a table when the
cases are *homogeneous* — they differ only in data, not in logic — and collapse
them into rows of a named struct iterated by one loop. The rule is to let the
shape of the data decide: a row struct with `name`, inputs, and expected outputs
beats ten near-identical hand-written subtests, but forcing a heterogeneous case
into a table row full of half-used fields is worse than a plain subtest. When a
row carries an expected error, compare it with `errors.Is` and handle the
nil-wanted branch explicitly, because `errors.Is(nil, nil)` is `true` and a naive
comparison hides the case where you expected success but got an error.

### t.Helper is about failure attribution

`t.Helper()` marks the calling function as a test helper so the failure reporter
skips its stack frame; the reported `file:line` then points at the line in the
test that *called* the helper, not at the `t.Fatalf` inside it. This is a
correctness property, not a nicety. A shared `assertValue` without `t.Helper()`
reports every failure at the same line inside the helper, so during an incident
the on-call opens the helper and learns nothing about which of forty call sites
actually broke. Any helper that can fail the test must call `t.Helper()` as its
first statement.

### t.Parallel semantics and their pitfalls

`t.Parallel()` marks a test to run concurrently with other parallel tests. The
subtle part is ordering: when a subtest calls `t.Parallel()`, it *pauses* and
signals the parent to continue; the parent function runs to the end of its body,
and only then do the paused parallel children run together. So any code you write
in the parent *after* the `t.Run` block executes *before* the parallel children,
not after — a classic source of "why did my teardown run too early" bugs. Put
post-children logic in `t.Cleanup`, not inline after `t.Run`. The second pitfall
is shared mutable fixtures: parallel subtests that share one `*Cache` are safe
only for operations the cache itself synchronizes (`Set`/`Get`/`Delete` under the
`RWMutex`). Reassigning an unsynchronized field such as the injected clock from
inside a parallel subtest is a data race the `-race` detector will catch; give
that subtest its own cache instance instead.

### Deterministic time is non-negotiable

Real cache code depends on `time.Now`, so tests must not depend on real time.
There are two techniques and they are not interchangeable. The first is
dependency injection: the cache reads the clock through an unexported
`now func() time.Time` field that a test overrides with a frozen instant. This is
simple and works on any toolchain, and it is the right tool for a plain unit
check of expiry logic. The second is `testing/synctest` (stable in Go 1.25),
which virtualizes the `time` package underneath the code so you test the real
`time.Now`/`time.Sleep` calls with no abstraction at all, and — crucially — also
makes goroutine scheduling deterministic. Prefer `synctest` for concurrent or
timer-driven code (a background reaper, a ticker); prefer injection for a simple
synchronous unit check. Never sleep real wall-clock time to wait for a TTL to
expire: it is slow, and under CI load it is flaky.

### The testing/synctest bubble model

`synctest.Test(t, func(t *testing.T) { ... })` runs the function in a bubble where
`time.Now` starts at 2000-01-01 00:00:00 UTC and the clock advances only when
every goroutine in the bubble is *durably blocked* — blocked on something that can
only be woken from inside the bubble, such as `time.Sleep`, a channel, or a
`WaitGroup`. `synctest.Wait()` blocks the caller until that quiescent point, which
removes the race between "I advanced the clock" and "the background goroutine
reacted". This eliminates an entire class of flakiness: no real-time slack, and
no dependence on which runnable goroutine the scheduler happens to pick. The
constraints are real: no real I/O inside the bubble (a syscall never becomes
durably blocked and hangs the clock), a goroutine you start must be able to exit
(the bubble waits for all of them), and lock acquisition does not count as durably
blocked.

### The golden-file pattern

For any serialized output — a stats report, a JSON API response, a rendered
template — hand-writing the expected bytes in a string literal rots the moment the
format changes. The golden-file pattern instead compares the rendered output
against a checked-in `testdata/*.golden` fixture, regenerated on demand by a
custom `-update` flag (`go test -run TestStats -update`). The non-negotiable
prerequisite is that the output is deterministic: sort map keys, freeze the clock,
and only then compare bytes. The `testdata` directory is ignored by the `go`
tool, so it ships safely as a fixture and is never mistaken for a package. When
the test fails, it must print a diff-friendly message and remind the reader to run
`-update` — never hand-edit a golden file.

### TestMain for suite-level fixtures

`TestMain(m *testing.M)` is the single per-package hook for setup and teardown
that must happen once around the whole test binary — warming a shared cache,
creating a temp working directory, opening a fixture. The contract has one sharp
edge: if `TestMain` reads flags it must call `flag.Parse()` first, it runs the
tests with `m.Run()`, and it must propagate the exit code with `os.Exit(code)`.
Because `os.Exit` does *not* run deferred functions, teardown cannot be a
`defer` — it must be called explicitly *before* `os.Exit`. There is exactly one
`TestMain` per package. Pair it with `testing.Short()`: a slow stress or property
test calls `t.Skip` when `testing.Short()` is true, so `go test -short` skips it
in a fast CI tier while the full suite still exercises it.

### httptest for the handler that fronts the cache

`httptest.NewRecorder` plus `httptest.NewRequest` exercise an `http.Handler`
in-process with no sockets and no ports. You build a request, serve it against the
handler, and assert on `rec.Code` and `rec.Body`. For a cache-aside handler the
decisive test injects a *counting* loader function and proves the cache-aside
contract: the first request misses and calls the loader, the second is served from
the cache and does not. Go 1.22's `ServeMux` gives method-and-wildcard routing
(`GET /items/{key}`) and `r.PathValue("key")`, so the routing itself is testable
without a third-party router. Tie the request context to the test lifecycle with
`t.Context()` so cancellation is scoped to the test.

### Benchmarks in modern Go

`for b.Loop() { ... }` (Go 1.24) replaces the legacy `for i := 0; i < b.N; i++`
loop. It resets the timer on the first iteration and stops it on the last, so
setup written *before* the loop is automatically excluded from the measurement
without a manual `b.ResetTimer`, and it keeps the loop's arguments and results
alive so the compiler cannot dead-code-eliminate the work you are trying to
measure. Pair it with `b.ReportAllocs()` and `-benchmem` to surface `allocs/op`
and catch an allocation regression — the specific failure mode that quietly
doubles GC pressure in a hot cache. `b.RunParallel` with `pb.Next()` measures the
contended-lock throughput that a real cache actually experiences under concurrent
load.

### Fuzzing asserts invariants, not fixed outputs

A fuzz target seeds a corpus with `f.Add`, explores mutations with `f.Fuzz`, and —
this is the mental shift — checks a *property* rather than a fixed expected value.
For a cache the natural invariant is the Set-then-Get round-trip: whatever bytes
you store come back byte-for-byte, and `Len`/`Delete` stay consistent regardless
of the key. This surfaces bugs in key normalization or copy-on-store logic that a
hand-picked example would miss. Seed failures are persisted under `testdata/fuzz`
and become permanent regression seeds; the target also runs its seed corpus under
a plain `go test`, so it guards against regressions even when nobody is actively
fuzzing.

### The suite as an operational asset

All of the above serves one goal: a suite that engineers trust and therefore run.
That requires four properties simultaneously. It must be fast, with slow tiers
gated behind `-short`. It must be deterministic, with no wall-clock or scheduler
flakes. It must be race-clean, because a cache is concurrent by definition and a
data race that passes locally corrupts under production load — so `-race` runs in
CI, always. And it must make coverage visible with `-cover` so gaps are known. A
flaky or slow suite gets ignored, and an ignored suite is worse than no suite at
all, because it gives false confidence.

## Common Mistakes

### Putting every case in one giant Test function

Wrong: a single `TestCache` with all cases inline. The output is unreadable, there
is no selective `-run`, and one early failure obscures the rest.

Fix: feature-per-function, case-per-subtest — `TestSet`, `TestGet`, `TestDelete`,
`TestLen`, each with named `t.Run` cases.

### Omitting t.Helper in assertion helpers

Wrong: an `assertValue` helper that calls `t.Fatalf` without `t.Helper()`. Every
failure reports a line inside the helper, so the on-call opens the wrong file.

Fix: call `t.Helper()` as the first statement of any helper that can fail the
test, so `file:line` points at the calling test.

### Assuming parallel subtests run before the parent's post-Run code

Wrong: writing teardown or assertions inline after the `t.Run` block and expecting
them to run after the parallel children. The parent body runs to completion first;
the paused parallel children run afterward.

Fix: put post-children logic in the parent's `t.Cleanup`, or synchronize
explicitly — never inline after `t.Run`.

### Reassigning shared unsynchronized state from a parallel subtest

Wrong: a parallel subtest doing `c.now = func() ...` on a `*Cache` shared with
other parallel subtests. The clock field is not guarded by the mutex, so it is a
data race that `-race` flags.

Fix: give the subtest its own cache instance, or synchronize the field. Share a
fixture across parallel subtests only for operations the fixture itself
synchronizes.

### Testing TTL with real time.Sleep

Wrong: `time.Sleep(2 * time.Second)` to wait for an entry to expire. It is slow
and flaky under CI load.

Fix: inject a frozen clock for a unit check, or use `testing/synctest`'s fake
clock for timer/concurrent code. Never sleep real time to wait for expiry.

### Running teardown via defer in TestMain

Wrong: `defer teardown()` in `TestMain`, then `os.Exit(m.Run())`. `os.Exit` does
not run deferred functions, so teardown silently never happens.

Fix: capture the code, run teardown explicitly, then exit:
`code := m.Run(); teardown(); os.Exit(code)`.

### Non-deterministic golden output

Wrong: rendering a report with unsorted map iteration or a real timestamp, then
golden-comparing it. The golden flaps run to run.

Fix: sort keys, freeze the clock, and only then compare bytes. Regenerate with
`-update`, never by hand.

### Benchmarks with setup inside the loop or eliminable work

Wrong: `for i := 0; i < b.N; i++` with expensive setup inside the loop, or a body
whose result is discarded so the compiler deletes it.

Fix: use `for b.Loop()` (Go 1.24), which excludes pre-loop setup and keeps values
alive, and add `b.ReportAllocs()` to guard allocations.

### Skipping -race in CI

Wrong: running the suite without `-race` because it is faster. A cache is
concurrent; a race that passes locally corrupts under load.

Fix: run `go test -race` in CI, always. A cache with no race test is untested.

### Fuzz targets that assert a hardcoded expected value

Wrong: a fuzz function comparing the output to a fixed constant. It cannot
generalize over random input.

Fix: assert an invariant — round-trip equality, no panic, internal consistency —
so the property holds for every generated input.

Next: [01-implement-the-cache.md](01-implement-the-cache.md)
