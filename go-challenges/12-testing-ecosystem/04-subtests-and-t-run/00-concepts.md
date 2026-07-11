# Subtests and t.Run — Concepts

`t.Run` looks like syntactic sugar over a `for` loop, and in a beginner's test
suite that is all it is. At scale it is the load-bearing structure of the whole
suite. A senior backend engineer reaches for `t.Run` to get four things that a
bare loop cannot give: selective execution in CI (run only the slow integration
subtree nightly with `-run`/`-skip`), fault isolation (one bad validation case
does not abort its siblings, so a single CI run surfaces every failing endpoint
at once), controlled parallelism with a correct resource lifecycle (a shared DB
or testcontainer built once in the parent and torn down only after all parallel
children finish), and dynamically generated cases from `testdata` so coverage
grows by dropping a file instead of editing code. The hard parts are not the
`t.Run` call; they are the lifecycle and goroutine rules underneath it. This file
is the model. Read it once and each of the nine independent exercises that follow
becomes a variation on rules you already understand.

## Concepts

### What t.Run actually does

`t.Run(name, f)` runs `f` in a *new goroutine* and blocks the calling goroutine
until `f` returns or calls `t.Parallel`. It returns a `bool` reporting whether the
subtest passed, which is occasionally useful for gating dependent work. Two
consequences of the new-goroutine design matter immediately. First, every `Run`
call started inside a test must return before the outer test function returns —
the runtime enforces this and a subtest still running when its parent returns is a
bug in your structure, not a race you can win. Second, because `f` runs on its own
goroutine, the `*testing.T` methods that stop a test by unwinding that goroutine
(`Fatal`, `FailNow`, `SkipNow`, `Parallel`) are meaningful only *on that
goroutine* — a fact that becomes a trap the moment you spawn your own goroutines
inside `f`.

### The subtest name is the addressing scheme

A subtest's full name is the parent test name joined to each `Run` name by a
slash: `TestValidate/invalid/missing_email`. Two transformations are applied.
Spaces in a name become underscores, so `t.Run("missing email", …)` is addressed
as `missing_email`. Duplicate names among siblings get a `#NN` suffix
(`case`, `case#01`, `case#02`) so every subtest is uniquely addressable. This name
is not cosmetic: it is the exact string that appears in every `=== RUN`,
`--- PASS`, and `--- FAIL` line, and it is what `-run` and `-skip` match against.
Non-unique, space-laden, or unstable names are therefore not a style nit — they
silently break your ability to target a case from CI. `t.Name()` returns this full
slash path at runtime, which is how a subtest can log or branch on where it sits
in the tree.

### -run and -skip select a subtree without touching code

`go test -run <regexp>` filters which tests and subtests execute. The regexp is
split on `/`: one pattern is applied per name segment. `-run 'TestValidate/valid'`
runs `TestValidate`, then within it only the `valid` subtree; a segment that is
absent matches everything at that level. `-run 'TestValidate/[^/]+$'` (a segment
that contains no further slash) targets the leaf level. `-skip <regexp>`
(added in Go 1.20) is the inverse: it excludes matching tests and subtests. This
pair is how a real pipeline runs a fast subset on every push
(`-skip 'slow|integration'`) and the expensive named subtree on a nightly cron
(`-run 'TestEndpoints/integration'`) without editing a single line of Go. Because
the match is a regexp, an unanchored pattern like `-run valid` also matches
`invalid`; anchor with `^`/`$` when you mean exactly one name.

### t.Parallel: pause, then resume together

`t.Parallel()` signals that a subtest may run concurrently with other parallel
subtests. The mechanism is specific and worth memorizing: calling `t.Parallel`
*pauses* the current subtest and returns control to the parent; the parent
function runs to completion (starting the remaining siblings, which likewise
pause); and only once the parent test function returns do all the paused parallel
siblings *resume together* and run concurrently. This is why the setup a parent
does before its loop is visible to every parallel child, and why the sequential
code *after* the loop in the parent runs *before* the children actually execute —
a fact that wrecks naive teardown. `t.Parallel` must be called from the subtest's
own goroutine, never from a helper goroutine you spawned.

### t.Run does not return until its parallel children finish

There is one guarantee that makes post-parallel teardown deterministic:
`t.Run(name, f)` does not return until every parallel subtest started *inside* `f`
has completed. So if you wrap a batch of parallel children in a
`t.Run("group", func(t *testing.T) { … })`, the code that follows that outer `Run`
call runs only after all the children in the group are done. This wrapper-group
idiom is the canonical place to tear down a fixture that several parallel subtests
shared: build it before the group, run the parallel children inside the group,
and clean up on the line after the group returns.

### t.Cleanup runs after all subtests, LIFO

`t.Cleanup(f)` registers `f` to run when the test *and all its subtests* have
finished. Cleanups run last-registered-first (LIFO), which is exactly the order
you want for nested resources: register "open DB" then "open writer", and cleanup
closes the writer before the DB. Crucially, a *parent's* cleanup runs after all of
its children, including parallel ones — so `t.Cleanup` on the parent is the second
canonical way (alongside the wrapper group) to tear down a shared fixture safely
after parallel children. Getting the LIFO order wrong — closing a DB before
flushing a dependent writer — is a real shutdown-ordering bug that `t.Cleanup`
prevents only if you register in dependency order.

### t.TempDir, t.Setenv, and t.Context

`t.TempDir()` returns a fresh, uniquely-named directory that is automatically
removed when the test and its subtests complete; calling it per subtest gives each
case its own isolated working directory with no manual `os.RemoveAll`. `t.Setenv`
sets an environment variable and restores the previous value on cleanup, but it
*forbids* use in a parallel test or a test with any parallel ancestor and will
panic if you try — because an env var is process-global state and mutating it
while sibling tests run concurrently is a data race across the whole process. The
same reasoning bans `os.Chdir` in parallel tests. `t.Context()` (Go 1.24) returns
a context that is cancelled *just before* the test's cleanup functions run,
letting a cleanup that shuts a resource down via context cancellation observe the
cancellation at the right moment.

### Error vs Fatal, and sibling isolation

Two failure verbs with different control flow. `t.Error`/`t.Errorf` mark the test
failed and *continue*, so a single subtest can accumulate several independent
complaints ("field name empty" and "field age negative") in one run — the right
choice when you want every problem reported at once. `t.Fatal`/`t.FailNow` mark
the test failed and *stop it immediately* by calling `runtime.Goexit` on the test
goroutine — the right choice when continuing is pointless or unsafe (a nil pointer
you are about to dereference). The subtest boundary contains the stop: a
`t.Fatal` in one subtest ends *that* subtest only; its siblings still run. This is
fault isolation — a batch of table cases run as subtests will each report its own
pass/fail, so one CI run surfaces every failing case rather than dying on the
first.

### The goroutine rule: Fatal/FailNow/SkipNow/Parallel are goroutine-local

This is the rule that silently corrupts concurrent handler tests.
`t.Fatal`, `t.FailNow`, `t.SkipNow`, and `t.Parallel` MUST be called only from the
test's own goroutine — the one running the test or subtest function. Called from a
goroutine you spawned, `Fatal`/`FailNow` run `runtime.Goexit` on the *wrong*
goroutine: they terminate your worker goroutine, not the test, so the test keeps
running and may report PASS while the assertion that "failed" did nothing. The
data race in accessing `t` from another goroutine is real but secondary; the
primary hazard is that your failure is silently swallowed. The fix is structural:
workers must marshal their outcomes back to the test goroutine (over a channel, a
`sync.WaitGroup` plus a shared slice, or an `errgroup.Group`), and the test
goroutine ranges over those outcomes and calls `t.Fatalf` itself. `t.Log`,
`t.Error`, and `t.Errorf` *are* safe to call from other goroutines (they only
record), but they do not stop anything.

### Dynamically generated subtests

Nothing requires the set of subtests to be a fixed literal table. You can discover
inputs at runtime — read every file under `testdata/`, and call
`t.Run(filepath.Base(f), …)` once per file — so the suite grows a case every time
someone drops a fixture, with no code change. Paired with the golden-file pattern
(compare rendered output against a checked-in `.golden` sibling, regenerate them
with a package-level `-update` flag), this is how renderers, formatters, and
serializers get exhaustive, self-locating coverage: a mismatch names the exact
file, and adding coverage is a `git add testdata/new-case.json`.

## Common Mistakes

### Calling t.Fatal from a spawned goroutine

Wrong: inside a subtest you `go func(){ if err != nil { t.Fatal(err) } }()`. The
`Fatal` runs `runtime.Goexit` on that goroutine, ending it, not the test; the test
proceeds and can report PASS. Fix: send the outcome back to the test goroutine (a
channel or `errgroup`) and call `t.Fatalf` there. Reserve `t.Error`/`t.Log` for
anything you must report from inside a goroutine, and remember they do not stop
the test.

### Expecting post-loop teardown to run before parallel children

Wrong: a parent that loops calling `t.Run` with `t.Parallel` children and then
runs teardown code on the next line, assuming the children finished. They have
not — `t.Parallel` only pauses them; they resume after the parent returns. The
teardown runs against a fixture the children have not touched yet. Fix: wrap the
parallel children in a `t.Run("group", …)` (which does not return until they
finish) and tear down after it, or register the teardown with `t.Cleanup`, which
runs after all subtests.

### Using t.Setenv (or os.Chdir) in a parallel subtest

Wrong: a parallel subtest calls `t.Setenv("REGION", "eu")`. It panics, because the
env var is process-global and mutating it while siblings run concurrently races
across the whole process. Fix: keep env/`Chdir`-mutating cases serial (no
`t.Parallel` on them or their ancestors), or pass configuration explicitly rather
than through the environment.

### Sharing a mutable fixture across parallel subtests unguarded

Wrong: parallel children write to a shared map or counter with no synchronization
and the suite passes on your laptop. Under `-race` (and under load in CI) it is a
data race. Fix: make the shared fixture read-only for the duration of the parallel
group, or guard every access with a mutex; run `go test -race` and treat any
report as a failure.

### Non-unique or space-laden subtest names

Wrong: two cases named `"basic"`, or a name like `"missing email"`. The duplicates
become `basic` and `basic#01`, and the space becomes an underscore, so your
`-run 'TestX/basic$'` targets only one of them and `-run 'TestX/missing email'`
never matches. Fix: pick stable, unique, filter-friendly names with no spaces.

### Hoisting nothing: expensive setup inside the t.Run callback

Wrong: building the fixture (opening a store, compiling a template) *inside* the
`t.Run` callback, so it re-runs for every case. Fix: build shared, read-only setup
once in the parent before the loop, keep the callback to the per-case assertion,
and put teardown in `t.Cleanup` or a wrapper group.

### Expecting the first failure to abort the whole table

Wrong: assuming that when case 3 fails, cases 4-10 are skipped, so you "fixed one
bug" and re-run. Subtests are isolated: a `t.Fatal` stops only its own subtest, and
`t.Error` does not even do that. Prefer `t.Error` inside a batch to keep collecting
every failure, and use `t.Fatal` deliberately where continuing is unsafe.

### Registering cleanups in the wrong order

Wrong: `t.Cleanup(closeDB)` then `t.Cleanup(closeWriter)` when the writer flushes
to the DB — cleanups run LIFO, so `closeWriter` runs first, which is correct here,
but reverse the two registrations and you close the DB before the writer flushes.
Fix: register cleanups in dependency order (acquire order), trusting LIFO to tear
down in the reverse.

Next: [01-table-driven-subtests.md](01-table-driven-subtests.md)
