# Error Handling in Goroutines — Concepts

A function returns an error; a goroutine cannot. `go doWork()` throws the return
value on the floor, and with it any error, any panic, any signal that the work
failed. That single fact reshapes every concurrent workload a backend service
runs: a fan-out to N upstream replicas, a bounded pool churning through a batch of
DB upserts, a streaming ingestion pipeline, a background audit write kicked off
from an HTTP handler. Each one has to make a deliberate decision about how an
error leaves the goroutine and reaches a place where someone can act on it. The
senior skill is not "use a channel". It is choosing the *shape* of error flow —
fail-fast versus collect-all, bounded versus unbounded, why-aware cancellation
versus a bare `context.Canceled` — and then proving with tests that a failure
early in the run leaks no goroutines. This file is the conceptual spine; read it
once and the ten independent exercises that follow each drill one production
pattern.

## Concepts

### A goroutine has no return value

There are exactly three ways an error can leave a goroutine: send it on a
**channel**, write it to a **shared variable guarded by a mutex**, or hand it to a
**callback**. There is no fourth option. `errgroup` and every worker-pool library
is built on one of these three underneath; picking among them is a design
decision, not an implementation detail. A channel gives you fan-in and natural
back-pressure. A mutex-protected slice gives you a complete per-item report. A
callback gives you a place to log or push the failure to a sink. Choose by what
the caller needs to do with the outcome.

### An unhandled error in a goroutine is silently lost

`go func() { _ = repo.Save(rec) }()` compiles, runs, and — when `Save` fails —
tells no one. Nobody is receiving on a channel, nobody is reading a shared slice,
so the failure evaporates. This is the silent-drop bug that hides data loss in
production: the write never happened, no error surfaced, no alert fired, and the
first symptom is a customer noticing missing data days later. Every goroutine you
spawn must have an answer to "where does its error go", and that answer must be a
place someone actually reads.

### Two aggregation strategies, chosen by intent

Concurrent error handling splits into two fundamentally different shapes, and
using the wrong one is a correctness bug, not a style nit.

**Fail-fast**: the first error cancels the rest and becomes the result. This is
right for "all must succeed" fan-out — querying three replicas where you need all
three, validating a request against several services where any rejection dooms
the request. There is no point letting the other goroutines finish; cancel them,
return the first error, move on. `errgroup.WithContext` gives you this shape for
free: the first `Go` func to return non-nil cancels a derived context and `Wait`
returns that error.

**Collect-all**: every outcome is recorded, successes and failures alike. This is
right for batch and reconciliation work — a nightly job upserting ten thousand
rows where you must report exactly which forty failed, a migration that has to
list every partial result. Cancelling on the first error would throw away the
information the report exists to produce. `errgroup` does not do this; you build
it yourself with a buffered channel plus `errors.Join`, or a mutex-protected
`[]Result`.

The mistake is reaching for `errgroup` (fail-fast) when the caller needed a
report, or hand-rolling a collector when a single first-error was all anyone
wanted.

### recover is local to its own goroutine

An unrecovered panic in *any* goroutine terminates the *entire process* — not the
goroutine, the process. And a `recover` in the parent goroutine cannot catch a
panic in a child: `recover` only sees a panic unwinding its own goroutine's
stack. So the reassuring-looking `defer func(){ recover() }()` at the top of your
request handler does nothing for the `go backgroundWork()` you launched inside it.
Every long-lived or fire-and-forget goroutine needs its *own* deferred `recover`,
placed inside the goroutine, that converts the panic into an error carrying the
task name and — for on-call — a captured `debug.Stack()`. This is not optional
hardening; it is the difference between one bad job logging an error and one bad
job crashing the whole server.

### errgroup coordinates, but only if workers honor the context

`errgroup.WithContext(ctx)` returns a group and a derived context that is
cancelled the moment any `Go` func returns non-nil, or when `Wait` returns. That
cancellation is what turns N independent goroutines into a coordinated group. But
`errgroup` does not stop your work — it only cancels the context. A worker that
never checks `ctx.Done()` runs to completion regardless; the cancellation buys you
nothing. Fail-fast only works if every blocking call in the worker is
context-aware: `ctx.Done()` in the `select`, `http.NewRequestWithContext`, a
`QueryContext` instead of `Query`. "errgroup cancels the work" is a myth;
errgroup cancels the *context*, and disciplined workers cancel themselves.

### Unbounded fan-out is a production hazard

`for _, item := range tenThousandItems { g.Go(func() error { return process(item) }) }`
spawns ten thousand goroutines at once. Each may open a DB connection, a file
descriptor, a socket. Under load this exhausts the connection pool, hits the
file-descriptor ceiling, and balloons memory — a self-inflicted outage the moment
the batch grows. Bound it. `errgroup`'s `SetLimit(n)` makes `Go` block once `n`
are in flight, turning the group into a worker pool with no extra machinery.
`TryGo` gives a non-blocking admission path: it returns false instead of blocking
when the group is saturated, useful when you want to shed load rather than queue
it. The rule: any fan-out whose width is driven by input size must be bounded.

### Cancellation causes carry *why*, plain Canceled throws it away

`context.Canceled` tells you the context was cancelled. It does not tell you
*why* — first-failure, deadline, or graceful shutdown all collapse into the same
opaque value, and a caller trying to decide "retry or give up?" is left guessing.
`context.WithCancelCause` returns a `CancelCauseFunc` you call with an explaining
error; every observer reads `context.Cause(ctx)` to learn the specific reason
(which job failed, wrapped with `%w` so `errors.Is` still works).
`context.WithTimeoutCause` attaches a cause to a deadline. Because a cancelled
child inherits its parent's cause, one `context.Cause` call distinguishes
"a worker failed" from "we ran out of time" — the classification your retry and
alerting logic needs. Plain `context.Canceled` makes that logic guess.

### Goroutine leaks are the quiet failure mode

The subtle bug in concurrent error handling is not a crash; it is a goroutine that
never returns. On an early error, a worker left blocked on an unbuffered channel
that no one will read again, or on a context that is never cancelled, lives
forever — holding its stack, its captured variables, maybe a connection. It does
not fail a test; the test passes and the leak accumulates in production until the
process is fat with parked goroutines. Two disciplines prevent it: cancel the
context on every exit path so blocked workers wake and return, and **buffer error
channels to `len(jobs)`** so a worker can always send-and-exit even after the
parent has stopped receiving. `go.uber.org/goleak` proves the absence of leaks in
a test: `goleak.VerifyTestMain(m)` for the package, `defer goleak.VerifyNone(t)`
around a runner call whose jobs fail early. If the runner leaks, the test fails
with the leaked stack.

### Buffered vs unbuffered error channels

A channel buffered to `len(jobs)` lets every worker send its result and exit
without a receiver present. An unbuffered channel forces a rendezvous: if the
parent has already returned after the first error and stopped receiving, a late
worker blocks forever on the send — a leak. For the collect-all pattern where you
drain after `Wait`, buffer to the job count. For fail-fast, cancel the context so
workers stop trying to send. Never leave a worker's only exit path blocked on a
send nobody will receive.

### errors.Join stays inspectable — if you put context in first

`errors.Join(errs...)` builds a multi-error whose `Is` and `As` traverse every
joined error, so an aggregate remains classifiable: `errors.Is(joined, ErrBoom)`
and `errors.As(joined, &typed)` both work across the whole tree. But a joined
error is only as debuggable as the context you baked into each part before
joining. `errors.Join(err1, err2, err3)` of three bare `"connection refused"`
errors is an opaque wall of identical text; the same three wrapped as
`fmt.Errorf("job %q: %w", name, err)` and logged as structured records — job name,
error type via `errors.As`, level — is triageable at 3am. Aggregate for the return
value; log structured records for the human.

### Background tasks must detach from the request context

A handler that launches `go auditWrite(r.Context(), event)` has created a task
that dies the instant the client disconnects, because the request context is
cancelled on disconnect. If the task must outlive the request — an audit write, a
cache warm, a webhook delivery — detach it: `context.WithoutCancel(parent)` keeps
the parent's values (trace IDs, auth) but drops its cancellation, then wrap it in
a fresh timeout so the task is still bounded. And that detached task still needs
its own `recover` and its own error sink, because now more than ever there is no
caller waiting to notice it failed.

### Modern idioms (Go 1.24-1.26)

`sync.WaitGroup.Go(f)` (Go 1.25) runs `f` in a new goroutine and does the
`Add(1)`/`defer Done()` for you, eliminating the classic "forgot Add" and
"Add inside the goroutine" races. `for i := range n` gives a clean fixed-count
worker loop. And since Go 1.22 the loop variable is per-iteration, so the old
`j := j` capture workaround is dead — a closure over the range variable in a
`for _, j := range jobs { go func(){ use(j) }() }` is correct without it. These
exercises use `wg.Go` and the modern loop scoping throughout.

## Common Mistakes

### Letting an error disappear

Wrong: `go func() { _ = do() }()`. The caller can never learn it failed; a
failure vanishes with no trace.

Fix: send it on a channel, record it under a mutex, or use `errgroup` — give the
error a destination someone reads.

### Sharing a plain variable across goroutines

Wrong: `var err error; go func() { err = do() }()`. Concurrent write with the
read after the goroutine is a data race; the value is undefined and `-race` flags
it.

Fix: a channel, a mutex, or `errgroup`.

### Unbuffered error channel with a parent that stops receiving

Wrong: an unbuffered `errCh` where the parent returns after the first error. Late
workers block forever on the send — a goroutine leak.

Fix: buffer to `len(jobs)` so every worker can send-and-exit, or cancel the
context so workers stop before sending, and drain fully.

### Expecting a parent recover to catch a child panic

Wrong: a `defer recover()` in the parent goroutine meant to save a panic in a
`go worker()`. It cannot; the process crashes.

Fix: `defer recover()` *inside* each goroutine.

### Assuming errgroup cancels the work

Wrong: relying on `errgroup.WithContext` to stop a worker that never checks
`ctx.Done()`. It only cancels the context; the worker runs to completion.

Fix: honor `ctx` in every blocking call inside the worker.

### Unbounded g.Go over a huge slice

Wrong: one `g.Go` per item across a ten-thousand-element batch, exhausting
connections and file descriptors under load.

Fix: `g.SetLimit(n)`, or a worker pool.

### Relying on plain context.Canceled to know why

Wrong: reading `ctx.Err() == context.Canceled` and trying to tell deadline from
first-failure from shutdown apart. They are indistinguishable.

Fix: `context.WithCancelCause` / `context.WithTimeoutCause`, read
`context.Cause(ctx)`.

### Calling wg.Add inside the goroutine

Wrong: `go func(){ wg.Add(1); ...; wg.Done() }()`. `Wait` can return before the
goroutine is counted — a race.

Fix: `wg.Add(1)` before `go`, or use `wg.Go` (Go 1.25), which does it correctly.

### Spawning a background task with the request context

Wrong: `go audit(r.Context(), ev)` — cancelled the instant the client
disconnects.

Fix: `context.WithoutCancel(r.Context())` (or a fresh root) plus its own timeout.

### Returning a bare errors.Join wall of text

Wrong: `return errors.Join(errs...)` of unwrapped errors, an opaque blob no one
can triage.

Fix: wrap each with job name and type before joining, and emit one structured log
record per failure.

### Forgetting to call the CancelFunc

Wrong: dropping the `cancel` returned by `context.With*`, leaking the context's
resources. `go vet`'s lostcancel check flags it.

Fix: `defer cancel()` (or `defer cancel(nil)` for a `CancelCauseFunc`) on all
paths.

Next: [01-concurrent-runner-channel-join.md](01-concurrent-runner-channel-join.md)
