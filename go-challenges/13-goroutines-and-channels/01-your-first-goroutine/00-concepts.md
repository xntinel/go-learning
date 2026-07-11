# Your First Goroutine: Launching, Waiting, and Surviving Background Work — Concepts

A senior engineer never launches a goroutine without answering three questions
first: who joins it, how does it terminate, and what happens if it panics or the
request is cancelled. "Your first goroutine" is not a syntax lesson; it is the
lesson where those three production contracts become reflexes. The `go` keyword
is one token, but every one of the incidents it causes — logs that vanish on
deploy, one bad task that panics the whole process, an unbounded fan-out that
exhausts a connection pool, a handler that leaks a goroutine per request — comes
from launching a goroutine without settling those three questions. This file is
the conceptual foundation; read it once and you have what you need for the ten
independent exercises that follow, which move you from a hand-rolled
`sync.WaitGroup` (which you must be able to read and debug) to the Go 1.25
`WaitGroup.Go` idiom and finally to `errgroup` with a concurrency cap and context
cancellation — what real batch and fan-out code actually uses against databases,
replicas, and third-party APIs.

## Concepts

### The go statement is fire-and-launch, not a call

`go f(args)` evaluates `f` and its argument expressions in the *current*
goroutine, then schedules `f` to run independently and returns immediately. It is
a statement, not an expression: it yields nothing. This is why "a goroutine has
no return value" is a structural fact, not a style preference — there is no place
for a value to go. Writing `x := go f()` does not compile, and reading a return
from the launched function is impossible. To get data out of a goroutine you must
use a channel, a shared variable guarded by a mutex or an atomic, or an
`errgroup` that carries the error. The arguments are evaluated eagerly at the
`go` statement, in the launching goroutine, which matters: `go f(i)` captures the
value of `i` at launch time, whereas `go func() { f(i) }()` captures the variable
`i` and reads it later, inside the new goroutine.

### A goroutine is a cheap green thread, not a free one

A goroutine is an M:N green thread that the Go runtime scheduler multiplexes onto
a bounded set of OS threads (`GOMAXPROCS` of them). Its initial stack is tiny,
about two kilobytes, and grows and shrinks on demand, so launching thousands is
routine and launching one per short-lived task is normal. But "cheap" is not
"free": every live goroutine costs stack memory, scheduler bookkeeping, and GC
scan surface, and — more importantly — every goroutine you start is a resource
that must terminate. Cheapness is exactly what makes unbounded fan-out dangerous:
the runtime will happily let you launch a million goroutines against an
attacker-controlled input and only fall over once memory or a downstream
connection pool is exhausted.

### Process lifetime is tied to the main goroutine only

When `main` returns — or any goroutine calls `os.Exit` — the process dies and
every other goroutine is terminated instantly, wherever it happens to be: mid
write, mid flush, mid syscall. There is no unwinding, no deferred cleanup, no
chance to finish. This is the mechanism behind the classic "logs vanished on the
last deploy" incident: a fire-and-forget writer goroutine had buffered records it
never got to persist because the process exited first. Correctness therefore
requires an explicit *join* — a `WaitGroup`, a channel, an `errgroup` — for any
goroutine whose completion your program depends on. If a side effect must have
happened before you return or exit, you must have waited for the goroutine that
produces it.

### sync.WaitGroup is a counter with one fragile invariant

`sync.WaitGroup` is a counter. `Add(n)` raises it before you launch, each
finishing goroutine calls `Done` (idiomatically `defer wg.Done()`), and `Wait`
blocks until it returns to zero. The invariant that breaks in production is the
happens-before ordering: `Add` must be observed before `Wait` can see zero. So
you always call `Add` in the launching goroutine, *before* the `go` statement,
never inside the spawned goroutine — if you `Add(1)` inside the new goroutine,
`Wait` can run first, observe a zero counter, and return before the goroutine has
even registered. The other half of the invariant is that every launched path must
reach `Done`: a branch that returns early or a panic without a `defer wg.Done()`
leaves the counter permanently above zero and `Wait` deadlocks forever. Miscount
deadlocks are a real, recurring incident class, which is why you must be able to
read and debug the manual form even as you stop writing it.

### WaitGroup.Go fuses the bookkeeping (Go 1.25)

Go 1.25 added `(*sync.WaitGroup).Go(f func())`, which does `Add(1)`, `go`, and
`defer Done()` in a single call. It removes the entire class of miscount bugs —
you cannot forget a `Done`, cannot skip it on an early return, and cannot
`Add`-inside-the-goroutine — so prefer it for all new code. You still must
understand the manual `Add`/`Done` form because you will read it in every existing
codebase and because debugging a miscount deadlock requires knowing what
`WaitGroup.Go` is doing underneath.

### Loop-variable capture: fixed for the loop var, not for everything

Before Go 1.22, `for i := ...; ...; i++ { go func() { use(i) }() }` was a notorious
bug: every goroutine captured the *same* variable `i` and observed its final
value. Since Go 1.22 the loop variable is per-iteration, so that specific bug is
gone — each iteration's closure captures a distinct `i`. But the discipline still
matters. Any variable declared *outside* the loop and captured by an escaping
closure is still shared across all goroutines, and mutating it from several
goroutines is still a data race regardless of Go version. Passing the value as an
explicit function argument (`go work(i)` or `wg.Go(func(){ work(v) })` with `v`
range-scoped) remains the clearest, version-independent guarantee that each
goroutine gets its own copy.

### A panic in a goroutine crashes the whole process

A panic unwinds only the stack of the goroutine it occurs in. A `recover` in the
goroutine that *launched* the panicking one will not catch it — recovery is not
inherited across the `go` boundary. An unrecovered panic in *any* goroutine
terminates the entire process, taking down every other goroutine with it.
Therefore every long-lived or fire-and-forget goroutine that must not be able to
kill the server needs its own deferred `recover` boundary at the top of its
function. A worker-supervisor helper that wraps `work()` in `defer func(){ if r
:= recover(); r != nil { onErr(...) } }()` converts a single bad task's panic
into a routed error instead of a process crash.

### Getting data out: channels, shared state, or errgroup

Because the `go` statement yields nothing, results flow out through one of three
mechanisms. A channel is the canonical one: a *buffered* results channel sized to
the number of senders lets each goroutine deposit its result without blocking,
and the collector drains the channel after launching — the classic scatter-gather
shape for querying N replicas or shards and aggregating the responses. A shared
variable guarded by a mutex or written through package `sync/atomic` works when
each goroutine writes a disjoint slot (for example `out[i]` per index) so there is
no contended write at all. An `errgroup` bundles the join and the first-error
propagation together and is what most real batch code reaches for.

### Real fan-out is bounded and cancellable

Launching one goroutine per item over an unbounded or attacker-controlled input
is a footgun: it can exhaust memory, saturate a downstream service, or blow a
database connection pool. Production fan-out is *bounded* — an `errgroup.SetLimit`,
a semaphore channel, or a worker pool caps the number in flight — and
*cancellable* — a `context` so that the first error, or a cancelled request,
tears the rest of the work down instead of letting doomed goroutines run to
completion. `errgroup.WithContext` gives you both: `SetLimit(n)` caps concurrency,
the first non-nil error is returned by `Wait`, and the derived context is
cancelled so in-flight peers observe `ctx.Done()` and stop early.

### Races are dynamic; test with -race and count>1

Any goroutine that reads or writes shared state races unless the access is
synchronized. The `-race` detector is mandatory in CI for concurrent code, but it
is a *dynamic* detector: it only reports races it actually observes at runtime on
the specific interleaving that occurred. A race that only manifests under a rare
schedule can pass green once and ship. Run concurrency tests with `-race` *and*
`-count` greater than one so more interleavings are exercised, and synchronize
tests with real joins (`WaitGroup`, channels, a `NumGoroutine` poll) rather than
`time.Sleep`, which is both slow and a liar about whether the goroutine actually
finished.

### Every goroutine needs a defined termination

Each goroutine you start must have a path to stop: it finishes its work and
returns, or it observes `ctx.Done()` or a close signal and returns. A goroutine
with no exit path is a leak. Leaks accumulate silently — a handler that spawns a
helper per request and never joins it slowly grows the goroutine count and the
memory behind it until the process degrades. `runtime.NumGoroutine()` sampled
before and after an operation is a cheap first-line assertion that the goroutines
you started actually stopped; a dedicated tool like `goleak` comes later, but the
before/after delta catches the obvious leaks today.

## Common Mistakes

### Reading a return value from a goroutine

Wrong: `x := go f()`, or expecting the launched function's return to be visible
to the caller. The `go` statement is not an expression and yields nothing.

Fix: route the result through a channel, a shared variable guarded by a
mutex/atomic, or an `errgroup` that carries the error.

### Launching and returning without joining

Wrong: start a goroutine for a side effect (persist a log, warm a cache) and
return or exit without waiting for it. `main` returns, the process dies, and the
goroutine is killed before it completes — the vanishing-logs, lost-writes
incident.

Fix: join every goroutine whose effect you depend on, with a `WaitGroup`, a
channel, or an `errgroup`, before you return or exit.

### Calling wg.Add inside the spawned goroutine

Wrong: `go func() { wg.Add(1); ...; wg.Done() }()`. `Wait` can run before the
goroutine registers, observe a zero counter, and return early, nondeterministically
missing the work.

Fix: call `Add` in the launching goroutine before `go`, or use `WaitGroup.Go`,
which does the `Add` at the correct moment for you.

### Mismatched Add/Done counts

Wrong: `Add(n)` and then some path skips `Done` — an early `return`, or a panic
with no `defer wg.Done()`. The counter never reaches zero and `Wait` deadlocks
forever.

Fix: always `defer wg.Done()` as the first statement of the goroutine, or use
`WaitGroup.Go`, which removes this whole class of bug.

### Expecting the parent to recover a child's panic

Wrong: wrapping the `go` statement in a `defer recover()` and assuming it catches
a panic inside the launched goroutine. It cannot; recovery does not cross the `go`
boundary, and the process crashes.

Fix: give each independent goroutine its own deferred `recover` at the top of its
function if it must not be able to take the process down.

### Sending on an under-buffered results channel

Wrong: N sender goroutines sending on an unbuffered channel (or one buffered
smaller than N) with no concurrent receiver. The senders block forever and the
fan-out deadlocks.

Fix: buffer the results channel to the number of senders so each can deposit its
result without blocking, then drain after launching; or run a concurrent
receiver.

### Unbounded fan-out over untrusted input

Wrong: one goroutine per element of an unbounded or attacker-controlled slice
with no cap. Memory blow-up and downstream overload.

Fix: bound the concurrency (`errgroup.SetLimit`, a semaphore channel, a worker
pool) and make it cancellable with a context.

### Mutating a shared outer variable from goroutines

Wrong: a variable declared outside the loop, captured and mutated by every
goroutine. Still a data race even under Go 1.22 loop-var semantics.

Fix: pass the value as an argument, or give each goroutine its own disjoint slot
(`out[i]`), so there is no shared mutable write.

### Using time.Sleep to wait for goroutines in tests

Wrong: `go work(); time.Sleep(50 * time.Millisecond)` and then asserting. Flaky
under load and it hides the very bug you are testing for.

Fix: synchronize with a real join — `WaitGroup.Wait`, a channel receive, or a
bounded `NumGoroutine` poll.

### Testing concurrency without -race and with -count=1

Wrong: a single green run with the race detector off. A data race or leak that
only appears under a particular interleaving ships undetected.

Fix: run `go test -race -count=10` (or more) on concurrent code so multiple
schedules are exercised.

Next: [01-fanout-single-background-task.md](01-fanout-single-background-task.md)
