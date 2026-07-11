# Parallel Tests: Isolation, Shared Fixtures, and Race Safety — Concepts

Turning on `t.Parallel()` across a backend test suite is not a language trick;
it is a CI economics decision. You are trading wall-clock time for isolation
discipline and resource contention, and a senior engineer owns that trade-off.
A suite that opens database connections, binds ephemeral ports, and spins up
`httptest` backends will run dramatically faster in parallel — or it will flake
intermittently in ways that look like logic bugs but are really contention and
shared-state bugs. The difference between the two outcomes is entirely a matter
of how well you understand one mechanism: the two-phase execution model of
`t.Parallel`. Internalize that, and almost every rule in this chapter stops being
a rule you memorize and becomes a consequence you can derive.

## The two-phase execution model

`t.Parallel()` does something surprising the first time you see it: it *pauses*
the calling test. When a test calls `t.Parallel()`, control returns to the
test's parent. The parent then runs the rest of its serial code and any other
serial subtests to completion. Only after the parent's serial work is finished
does the test runner *resume* all of the paused siblings together and run them
concurrently.

That single fact explains the shape of every parallel test you will ever write:

- Code that runs *before* `t.Parallel()` runs serially, in the normal one-at-a-
  time order, at the moment the test is first entered.
- Code that runs *after* `t.Parallel()` runs concurrently with every other
  parallel sibling at the same level, at some later point when the runner
  resumes the paused group.

So "do setup before `t.Parallel`, do the work and assertions after" is not a
style convention — it is the only arrangement that is coherent. If you mutate
shared state before `t.Parallel()`, that mutation is serial and looks safe, but
the assertion that reads it runs concurrently against state other tests are also
touching. The bug hides in the gap between the two phases.

## Scope of concurrency, and the two "-parallel"-shaped knobs

A parallel test runs concurrently only with *other parallel tests at the same
level*. Serial tests and parallel tests never overlap: the runner drains the
serial work first, then releases the parallel group. This is why a serial test
that calls `t.Setenv` can coexist in a package with parallel tests — they are
temporally separated.

Two flags are easy to confuse:

- `-parallel N` bounds how many parallel tests run concurrently *within a single
  package's test binary*. Default is `GOMAXPROCS`.
- `-p N` bounds how many *packages' test binaries* run at once. Also default
  `GOMAXPROCS`.

They compose multiplicatively: with `-p 4 -parallel 8` you can have up to 32
tests in flight. When you are sizing parallelism against a scarce shared
resource — a database with a fixed connection pool, a rate-limited upstream —
you must reason about both, or an in-package `-parallel` cap will be silently
multiplied by cross-package `-p`.

## Independence is a hard requirement, not a preference

Parallel tests must not share mutable state. Package-level globals, a shared map
or slice, the process working directory, and environment variables are all
process-wide and unsynchronized across parallel tests. Each parallel test must
construct its own fixture *inside* the test body (after `t.Parallel()`), or
share only immutable, read-only state. "These tests happen to not collide today"
is not independence; a data race that the runtime does not observe on this run is
still a bug, and `-shuffle` or a busier CI machine will find it.

## os-global operations are forbidden under parallelism

`t.Setenv` and `t.Chdir` mutate process-wide state. Because that state is shared
by every goroutine, the standard library makes both *panic* if called in a test
that is itself parallel or has any parallel ancestor. This is not an arbitrary
restriction; it is the runtime refusing to let you introduce a race it cannot
protect you from.

The senior response is not to give up parallelism — it is design-for-
testability. Instead of reading `os.Getenv` or the current directory *inside* the
code under test, inject a `Config` struct, an `fs.FS`, or a clock function. Code
that takes its configuration as a parameter has no process-global dependency, so
its tests need no `t.Setenv` and can run in parallel. The env-reading adapter at
the edge is tested once, serially; the pure logic underneath is tested many times,
in parallel. Exercise 4 builds exactly this split.

## Shared read-only fixtures and the Run-group pattern

Some fixtures are expensive: a migrated test database, a seeded set of rows, one
`httptest.Server`. You want to build such a fixture *once* and let many parallel
subtests share it read-only. The correct structure is to build the fixture in a
parent test, then run the parallel subtests inside a `t.Run("group", ...)`
wrapper:

```
func TestAPI(t *testing.T) {
	srv := newExpensiveServer()      // built once, serially
	t.Cleanup(srv.Close)             // torn down after all children
	t.Run("group", func(t *testing.T) {
		t.Run("a", func(t *testing.T) { t.Parallel(); /* hit srv */ })
		t.Run("b", func(t *testing.T) { t.Parallel(); /* hit srv */ })
	})
	// control reaches here only after a and b both finish
}
```

The load-bearing detail: `t.Run` does not return until its subtests complete,
*including* paused parallel subtests. So any code after the `t.Run("group", ...)`
call — and any `t.Cleanup` registered before it — runs exactly once, after all
children have finished. That is what makes shared teardown correct.

## defer versus t.Cleanup under parallelism

The most common shared-fixture bug is tearing the fixture down with a parent-
level `defer`. A `defer` fires when the *function it is in* returns. In a
parallel subtest, that is fine: the deferred call runs when the subtest resumes
and finishes. But a `defer` at the *parent* level fires when the parent function
returns — and the parent function returns to the runner at the moment it calls
`t.Parallel()`-driven children into their paused state, *before* those children
resume. So a parent-level `defer srv.Close()` closes the server while the paused
subtests are still queued to run against it. They then execute against a
destroyed resource and flake.

`t.Cleanup` fixes this by definition: cleanup functions run when the test *and
all its subtests* have completed, not when the parent function body returns. Use
`t.Cleanup` (or the `t.Run("group", ...)` wrapper, which has the same effect) for
any teardown of state shared with parallel children. Reserve `defer` for cleanup
that is local to a single, non-parent test.

## Cleanup semantics: LIFO and context cancellation

`t.Cleanup` runs its functions last-added-first-called (LIFO) once the test and
its subtests complete. Stacked fixtures unwind in reverse: if you build a temp
dir, then open a store rooted in it, then start a background worker, registering
a cleanup after each layer means teardown runs worker, then store, then temp dir
— exactly the safe order.

`t.Context()` (Go 1.24) returns a context that is cancelled *just before* the
test's cleanup functions run. That timing is the point: a worker or server the
test started can watch `ctx.Done()`, and because cancellation fires before
cleanup, the cleanup can wait for the worker to drain gracefully. You get
"signal shutdown, then await it" ordering for free, without threading a manual
cancel func through the test.

## The loop-variable change (Go 1.22) and false-green suites

Before Go 1.22, a `for`/`range` loop reused one variable across iterations. A
table-driven test that launched a parallel subtest per case captured that shared
variable; by the time the paused subtests resumed, the loop had finished and the
variable held the *last* case. Every parallel subtest then silently tested only
the last row — a green suite that verified almost nothing. The classic fix was
`tc := tc` to shadow the variable per iteration.

Go 1.22 changed the semantics: each iteration now gets its own copy of the loop
variable. The `tc := tc` shadow is no longer needed and is pure noise on modern
toolchains. But the history matters: on an older toolchain, omitting it produces
the silent last-case bug, and cargo-culting it on 1.22+ signals a stale mental
model. Write the modern form, and know why the old form existed.

## The race detector is probabilistic evidence, not proof

`-race` instruments memory accesses and reports data races it *actually observes*
during a run. It cannot report a race that did not happen to occur on that
execution. Parallelism plus enough iterations raises the probability of
observation, but nothing guarantees detection on any single run. A green `-race`
run is evidence, not a proof of concurrency safety.

The way a senior hardens a CI gate is to stack the probabilistic tools:

- `-race` to instrument memory accesses,
- `-count=N` to repeat the run and give races more chances to surface,
- `-shuffle=on` to randomize test order and expose inter-test coupling — one
  test leaking state that another depends on.

`-shuffle` prints the seed it used; a failure is reproduced with
`-shuffle=<seed>`. If a suite passes in declaration order but fails under
`-shuffle`, you have an ordering dependency, which is a design bug regardless of
whether it involves a data race.

## Resource contention is the hidden cost

Turning on `t.Parallel()` suite-wide can exhaust resources that have nothing to
do with CPU: a database connection pool with a fixed cap, the supply of
ephemeral ports or file descriptors, a rate-limited third-party dependency. The
resulting failures look exactly like logic bugs — a query that "randomly" errors,
a dial that "randomly" refuses — but they are contention. The fix is to bound
parallelism against the *scarce resource*, not against CPU count: a package-level
weighted semaphore (a buffered channel, or `golang.org/x/sync/semaphore`) sized
to the pool, each test acquiring before it touches the resource and releasing via
`t.Cleanup`. `-parallel N` is a coarser version of the same idea; the semaphore
is precise because it counts the acquisitions that actually matter.

## t.Parallel parallelizes tests, not the work inside a test

A frequent misconception: `t.Parallel()` does not make the goroutines you spawn
*inside* one test run concurrently — they already do, regardless. `t.Parallel`
only lets that whole test overlap with sibling parallel *tests*. So a parallel-
safe data structure still needs atomics or a mutex to be exercised concurrently
within a single test (Exercise 1 fans out goroutines inside one test to do
exactly that), and marking that test `t.Parallel()` changes nothing about the
concurrency of its internal goroutines — only whether it overlaps its siblings.

## testing/synctest: deterministic time for concurrent code

The worst tests to write are for timer-driven concurrency: a retry with backoff,
a rate limiter, a debounced flush worker. Testing them honestly with real
`time.Sleep` is slow and still racy. Go 1.25 stabilized `testing/synctest`
(experimental behind `GOEXPERIMENT=synctest` in 1.24) to fix this. `synctest.Test(t, f)`
runs `f` in a "bubble" with a virtualized clock that only advances when every
bubble goroutine is durably blocked, and it waits for all bubble goroutines to
exit before returning. `synctest.Wait()` blocks until every *other* goroutine in
the bubble is durably blocked, removing the race between "I advanced the clock"
and "the background goroutine reacted". A two-second backoff is asserted in
microseconds, deterministically. `synctest.Run` is deprecated in favor of
`synctest.Test`, which wires the bubble to the test's `T`. Exercise 9 uses it on
a retry/backoff caller.

## Common Mistakes

### Sharing a package-level global across parallel tests

Wrong: parallel tests read and write a shared map, slice, or package var. The
failures are nondeterministic and pass on rerun, which is the signature of a
data race the runtime did not happen to observe.

Fix: construct each test's state inside the test body after `t.Parallel()`, or
share only immutable data.

### Mutating shared state before t.Parallel and expecting isolation

Wrong: setup that mutates process-wide state runs before `t.Parallel()`, so it
looks serial and safe; the assertion after `t.Parallel()` then races against
other tests touching the same state. Serial setup does not make a concurrent
assertion safe.

Fix: keep the mutable state per-test; if it truly must be shared, make it
read-only after construction.

### Calling t.Setenv or t.Chdir under a parallel ancestor

Wrong: `t.Setenv`/`t.Chdir` in a parallel test panics, because they mutate
process-global state.

Fix: inject a `Config`, an `fs.FS`, or a clock so the code under test has no
process-global dependency; then the test can be parallel. Test the thin env-
reading adapter once, serially.

### Tearing down a shared fixture with a parent-level defer

Wrong: `defer srv.Close()` at the parent level fires when the parent function
returns — before the paused parallel subtests resume — so they run against a
closed server.

Fix: `t.Cleanup(srv.Close)`, or wrap the subtests in `t.Run("group", ...)` and
close after it. Cleanup runs after all subtests complete.

### Cargo-culting tc := tc, or omitting it on old toolchains

Wrong (1.22+): `tc := tc` is dead noise. Wrong (pre-1.22): omitting it makes
every parallel subtest test only the last table row — a silent false green.

Fix: on 1.22+ write the loop without the shadow; know it was mandatory before.

### Treating a green suite as proof without -race

Wrong: parallel tests run without `-race` and a pass is read as concurrency
safety. `-race` observing nothing is not the same as `-race` not running.

Fix: run `-race`, and stack `-count` and `-shuffle=on` in the CI gate to raise
detection probability and surface ordering coupling.

### Using the outer test's T inside a parallel subtest

Wrong: a subtest closure references the enclosing test's `*testing.T` instead of
its own `t`. Failures and logs attribute to the wrong test and cleanup scoping is
wrong.

Fix: always use the subtest's own `t` parameter.

### Enabling t.Parallel suite-wide without bounding contention

Wrong: every test goes parallel and the shared DB pool, ephemeral ports, or file
descriptors are exhausted, producing flakes that look like logic bugs.

Fix: bound with a package-level semaphore sized to the scarce resource (and/or
`-parallel`), acquiring before use and releasing via `t.Cleanup`.

### Assuming t.Parallel parallelizes work inside one test

Wrong: expecting `t.Parallel()` to make in-test goroutines concurrent. It does
not; it only lets whole tests overlap. Goroutines inside a test are concurrent on
their own and still need synchronization.

Fix: use atomics/mutexes for in-test concurrency; use `t.Parallel` for cross-test
overlap.

### Depending on test order or output ordering

Wrong: asserting on the order of log lines or execution across parallel tests.
Order is unspecified and `-shuffle` will break the assumption.

Fix: make each test order-independent; assert on aggregate invariants, not
sequence.

### Sleeping to wait for background goroutines

Wrong: `time.Sleep` to let a background goroutine catch up in a concurrency test
— slow and flaky.

Fix: synchronize explicitly (channels, `sync.WaitGroup`), or use
`synctest.Wait` inside a bubble for timer-driven code.

Next: [01-parallel-safe-counter.md](01-parallel-safe-counter.md)
