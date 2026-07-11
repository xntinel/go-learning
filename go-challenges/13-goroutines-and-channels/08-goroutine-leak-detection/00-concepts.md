# Goroutine Leak Detection In Production Services — Concepts

A goroutine leak is the quintessential slow-burn production incident. The service
passes every functional test, ships green, and then over hours or days RSS climbs,
GC pauses lengthen, scheduler contention grows, and eventually the pod OOM-kills or
the p99 latency collapses — with no single request to blame. There is no stack
trace pointing at the culprit, because the culprit is not a crash: it is a
goroutine that started, did its job, and then never returned. This file is the
conceptual foundation for the ten exercises that follow. Read it once and you have
the mental model to reason through cancellation-correct concurrency, to prove the
absence of leaks in CI, and to diagnose a live leak in a running process.

## Concepts

### A leak is a correctness bug, not a performance nuisance

The instinct is to file a goroutine leak under "performance": the program is a
little slower, uses a little more memory, we will optimize it later. That framing
is wrong and it is why leaks ship. A leaked goroutine means a goroutine's *exit
contract is unsatisfiable* for some input or state — the code path that was
supposed to let it return can never be taken. That is a logic bug in exactly the
same category as a resource you acquire but never release, or a lock you take but
never unlock. A senior backend engineer treats "every goroutine must have a
guaranteed exit path" as a hard invariant of concurrent code, enforced by a test,
not as a nice-to-have checked by a human watching a memory graph.

The reframing is what makes leaks *testable*. "The service uses too much memory" is
not something you can assert in a unit test. "This function returns with the same
number of goroutines it started with" is. Once you state the bug as "the exit path
is missing or unreachable", you can write a test that fails the PR.

### The exit-path invariant

Every goroutine you start must have a guaranteed path to `return` for ALL
executions, not just the happy one. The legitimate exit mechanisms are few:

- a receive on a stop channel that is guaranteed to be closed (`<-stop`);
- a `select` that includes `case <-ctx.Done():`;
- a `for range ch` over a channel that is guaranteed to be closed by its producer;
- a plain `return` at the end of bounded work.

The invariant is violated the moment ANY code path inside the goroutine can block
forever. A goroutine parked on a channel send that no one will ever receive, or a
receive from a channel no one will ever send to or close, is leaked — permanently,
because nothing schedules it again. Context does not save you here: cancelling a
context does *nothing* to a goroutine that is blocked on an unbuffered channel send
and is not also selecting on `ctx.Done()`. Context is a signal the goroutine must
actively watch, not a force that reaches in and stops it.

### The canonical leak shapes, memorized

Leaks are not infinitely varied. A handful of shapes account for almost all of them
in real backend code, and knowing them by name lets you spot one in review:

1. Send on a channel whose only receiver already returned — the fan-out where the
   caller returns on timeout or first result, and the losing workers block forever
   trying to send their result. This is the single most common production leak.
2. Receive from a channel that is never closed and never sent to — the consumer
   waiting on a producer that was abandoned.
3. A worker pool whose producer forgot to close the jobs channel, or whose consumer
   stopped reading results.
4. A pub/sub subscriber that never unsubscribes: its delivery goroutine and channel
   stay alive as long as the broker holds a reference.
5. Per-iteration `time.After` in a `select` loop — each iteration allocates a
   `Timer` (and its runtime bookkeeping) that lives until it fires, so a hot loop
   accumulates them.
6. Background loops that a `Shutdown` forgets to join — signalling stop is not the
   same as waiting for the goroutine to actually finish.

### runtime.NumGoroutine is a coarse, racy count

`runtime.NumGoroutine()` returns how many goroutines exist right now. It gives you a
number, never an identity, and it is inherently racy: a goroutine that has decided
to return but has not yet been descheduled is still counted. That is why the
homegrown leak test cannot assert `NumGoroutine() == baseline` once and be done. It
must `runtime.GC()` (which helps finalize exiting goroutines) and then *poll* the
count back down to the baseline within a deadline, retrying with a small sleep. A
single-shot equality assertion on `NumGoroutine` is the classic flaky test that
fails one CI run in fifty and gets marked "known flaky" instead of fixed.

### go.uber.org/goleak is the production standard

The homegrown count-and-poll works, but the industry-standard detector is
`go.uber.org/goleak`. It snapshots the set of goroutine stacks and reports any that
should not be there, naming the leaking function — an identity, not just a number.
Two entry points matter:

- `goleak.VerifyTestMain(m)` inside a package's `TestMain` checks for leaks *after
  all tests in the package have run*. It is parallel-safe because it runs once, at
  the end, when every test has finished.
- `goleak.VerifyNone(t)`, typically `defer`-ed at the top of a single test, checks
  that one test leaked nothing. It is documented as **incompatible with
  `t.Parallel()`** — a parallel test's goroutines overlap with its siblings, so a
  per-test snapshot cannot attribute them. Use `VerifyTestMain` for parallel
  packages and reserve `VerifyNone` for serial tests.

goleak already ignores the runtime's own permanent goroutines. For a goroutine your
own code keeps alive for the whole process lifetime *on purpose* (a metrics
reporter, a signal handler), whitelist it precisely: `IgnoreTopFunction("pkg.fn")`
names the one function, and `IgnoreCurrent()` snapshots whatever is running at
startup so it is subtracted from later checks. The discipline is to whitelist the
one specific top function with a documented reason, never to add a broad ignore that
silences real leaks along with the intended one.

### Buffered channels are a legitimate fan-out leak fix

For the send-after-receiver-left shape, a buffered result channel sized to the
number of producers is a correct, idiomatic fix: if every loser can always complete
its send into the buffer, it exits even after the receiver has gone. The trade-off
is a bounded, known memory cost (N slots) versus the alternative of a
context-aware send, `select { case ch <- v: case <-ctx.Done(): }`, which uses no
extra buffer but requires every worker to hold and watch the context. Both are
correct; the buffered channel is simpler when N is small and known, the ctx-aware
send scales when N is large or unbounded.

### errgroup makes fan-out leak-proof by construction

`golang.org/x/sync/errgroup` is the idiomatic replacement for hand-rolled
`WaitGroup` + error-channel plumbing. `errgroup.WithContext(ctx)` returns a group
and a derived context that is cancelled when the first `Go`-launched function
returns a non-nil error; `Wait()` blocks until *every* launched goroutine has
returned and yields that first error. Because `Wait()` joins everything, a
correctly-used errgroup cannot leak — provided the launched functions actually
watch the derived context and exit when it is cancelled. `SetLimit(n)` caps how many
run concurrently, which is how you protect a downstream (a database, a rate-limited
API) from an unbounded fan-out.

### sync.WaitGroup.Go removes the classic Add/Done races

Go 1.25 added `(*sync.WaitGroup).Go(f)`, which does the `Add(1)`, runs `f` in a new
goroutine, and calls `Done()` when it returns — all in one call. It eliminates the
`Add(1)`/`defer Done()` boilerplate and, more importantly, the classic bug of
calling `Add` *after* the `go` statement (a data race between `Add` and `Wait`). A
supervisor that starts N background loops should launch each with `wg.Go`, then on
shutdown cancel the root context and `Wait()` with a deadline, reporting any loop
that missed it.

### Detection belongs in CI, run under -race

A leak that only shows up as slow RSS growth over days will ship, because no test
and no reviewer sees it. The same leak, asserted by a `goleak` or `NumGoroutine`
check in a unit test, fails the PR the moment it is introduced. Leak tests must run
with `-race -count=1`: `-race` because ad-hoc goroutine coordination is where data
races breed, and `-count=1` to defeat the test cache so the assertion actually runs.
The whole point is to move detection from a human watching Grafana at 3am to a
deterministic gate on every commit.

### Shutdown correctness is a superset of leak-freedom

Being leak-free at steady state is necessary but not sufficient. A correct
`Shutdown` must *join* — wait for each background goroutine's done signal — within a
deadline, and it must honestly surface the case where a loop refuses to stop in
time, rather than pretending shutdown succeeded. Signalling stop and returning
immediately is a bug: the goroutine is still running, its resources are not yet
released, and a leak test run right after would still see it. The honest shutdown
returns an error that names the loop that did not stop, so an operator knows exactly
what is stuck.

### Diagnosing a live leak with pprof

When a leak is already loose in a running process, you cannot rerun a unit test
against it. `runtime/pprof.Lookup("goroutine").WriteTo(w, 2)` writes every
goroutine's full stack in human-readable form; the count at the top and the repeated
stacks below tell you which function is accumulating. `Profile.Count()` and
`runtime.NumGoroutine()` are the cheap numeric signals to expose on an internal
endpoint and alert on — a goroutine count that climbs monotonically with RSS is a
leak until proven otherwise. Exposing a `/debug/goroutines` handler (guarded to an
internal network) turns "the pod is OOMing" into "here is the stack that is
leaking".

## Common Mistakes

### An unconditional loop with no exit path

Wrong: `go func() { for { doWork() } }()`. The goroutine runs until the process
exits; every call that starts one leaks. Every goroutine needs a stop channel, a
`case <-ctx.Done()`, or a bounded `range` over a channel guaranteed to close.

### Passing a context but never selecting on ctx.Done

Wrong: handing a goroutine a `ctx` and then blocking it on a bare channel send, a
receive, or `time.Sleep`. Cancelling the context does nothing, because the goroutine
is not watching `ctx.Done()`. Context is a signal to `select` on, not a remote kill.

### A Shutdown that signals stop but does not wait

Wrong: `close(s.stop); return nil`. The goroutine is still running when Shutdown
returns; the leak test still fails and the resources are not released. Join on a
`done` channel with a deadline before returning.

### Asserting NumGoroutine once without GC or polling

Wrong: `if runtime.NumGoroutine() != before { t.Fatal(...) }`. The count still
includes goroutines mid-exit, so the test flakes intermittently in CI. Call
`runtime.GC()` and poll back to the baseline within a deadline.

### VerifyNone together with t.Parallel

Wrong: `func TestX(t *testing.T) { t.Parallel(); defer goleak.VerifyNone(t); ... }`.
They are documented as incompatible; a parallel test's goroutines overlap its
siblings. Use `goleak.VerifyTestMain(m)` for parallel packages.

### Unbuffered fan-out with an early-returning caller

Wrong: workers send results on an unbuffered channel while the caller can return on
timeout or first result. The losers block forever on the send. Fix with a buffered
channel sized to N, or a `select { case ch <- v: case <-ctx.Done(): }`.

### time.After inside a select loop

Wrong: `for { select { case <-work: ...; case <-time.After(d): ... } }`. Each
iteration allocates a `Timer` that lives until `d` elapses, so a hot loop
accumulates timers and their heap. Reuse a single `time.NewTicker`, or a
`Stop`-and-`Reset` `Timer`.

### Forgetting Ticker.Stop / Timer.Stop, or never Unsubscribing

Wrong: a poller that never calls `(*Ticker).Stop()`, or a subscriber that reads a
subscription channel with no `Unsubscribe` path. The broadcaster keeps the goroutine
and channel alive.

### A broker doing a blocking send to each subscriber

Wrong: the broadcast loop does `sub.ch <- msg` to every subscriber. One slow or
departed subscriber blocks the entire broadcast. Use a non-blocking
`select { case sub.ch <- msg: default: /* drop */ }` plus an `Unsubscribe`.

### Assuming process exit cleans up the leak

Wrong: "the goroutine dies when the program ends, so it does not matter." Long-lived
servers never end; the leak compounds across every request until OOM.

### Not running leak tests under -race

Ad-hoc goroutine coordination is exactly where data races hide. `go test` without
`-race` compiles and passes while a race silently corrupts state.

### Treating a goleak failure as flaky noise

Wrong: a goleak failure appears, so you add a broad `IgnoreTopFunction` or
`IgnoreAnyFunction` to make CI green. That silences the next real leak too. Fix the
leak, or whitelist the one specific top function with a written reason.

Next: [01-leak-detection-service.md](01-leak-detection-service.md)
