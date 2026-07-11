# Goroutine Pools In A Job Service — Concepts

A goroutine is cheap, so the naive reflex under load is to start one per unit of
work: a request arrives, spawn a goroutine; a thousand rows need enrichment,
spawn a thousand goroutines. That reflex is how a Go service knocks over its own
dependencies. Each goroutine that talks to the database borrows a connection from
a pool that has maybe twenty slots; each one that calls an upstream API opens a
socket and consumes part of that service's rate budget; each one holds its stack
and whatever buffers the job allocates. Unbounded fan-out converts a burst of
*work* into a burst of *concurrent operations*, and the concurrent operations are
what exhaust the real, finite resources. A goroutine pool exists to break that
coupling: it puts a fixed ceiling on how many operations run at once, so a spike
in arrivals turns into a queue (or a rejection) rather than a stampede. This file
is the conceptual foundation for the ten exercises that follow; read it once and
you have the model for every bounding primitive the lesson builds.

## Concepts

### A pool bounds fan-out; concurrency is a resource budget, not a speed knob

The purpose of a pool is not to make work faster — it is to keep the number of
simultaneous operations below the point where a downstream resource breaks. That
resource might be the database connection pool, an upstream API's concurrency or
per-second quota, the process's open file descriptors, CPU cores, or memory. The
correct pool size is the size that keeps those resources inside their limits and
no larger. Treating "more workers" as "more throughput" is the central beginner
error: past the bottleneck's capacity, extra workers only add scheduling
overhead, memory, and contention. Concurrency is a budget you spend against a
constraint, and the constraint is almost always downstream of your process.

### Right-sizing: CPU-bound scales to cores, I/O-bound scales to the bottleneck

There is no universal worker count. CPU-bound work — hashing, compression,
parsing, image transforms done in-process — is limited by the number of cores, so
the pool scales to roughly `GOMAXPROCS`; adding workers beyond that just
time-slices the same cores. I/O-bound work — HTTP calls, database queries, file
reads — spends most of its wall-clock time blocked, so a single core can usefully
drive many more such workers, and the right ceiling is whatever the *downstream*
tolerates (its connection limit, its rate budget), not your core count. The only
honest way to pick a number is to measure against the real bottleneck; the pool
is the knob you turn once you know what you are protecting.

### Four bounding primitives, and when each is the right tool

The senior skill is not writing a for-range-over-channel worker — that is the easy
part. It is choosing the right bounding primitive for the situation. There are
four, and they bound different things:

1. A buffered-channel worker pool: a fixed number of goroutines drain a shared
   job queue. Use it when you want a stable, long-lived set of workers and a queue
   with backpressure. This is the classic pool and the artifact Exercise 1 builds.
2. `errgroup.Group` with `SetLimit`: bounded fan-out that also gives you
   first-error cancellation and a `Wait` that aggregates. Use it for a batch where
   the first failure should cancel the rest and you want one error back.
3. `semaphore.Weighted`: bounds total *cost* rather than job *count*, so
   heterogeneous jobs (a cheap thumbnail vs. an expensive render) can each acquire
   a weight and the sum of in-flight weights stays under a budget.
4. `rate.Limiter`: bounds *throughput* — events per second — independent of how
   many workers exist. Use it to respect a third party's per-second quota.

Concurrency capping and rate capping are orthogonal and frequently both needed: a
pool of ten workers each calling an upstream at most fifty times per second needs
a worker pool *and* a rate limiter. Confusing the two is a common and expensive
mistake.

### Channel ownership: the sender closes, exactly once

Channels have strict ownership rules and violating them panics the process. The
sender — the side that submits jobs — owns the channel and is the only side
allowed to close it; a receiver must never close a channel it reads from. Closing
a channel twice panics, and sending on a closed channel panics. This is why a
pool's `Submit` cannot simply do `p.jobs <- job`: after `Close` has closed the
channel, that send would panic. The guard is a mutex plus a `closed` flag (or a
select on a done channel), and `Submit` returns a bool or an error instead of
panicking, so the caller learns the pool is closed rather than crashing. `Close`
itself checks the flag so a double `Close` is idempotent, not a panic.

### WaitGroup discipline and the drain contract

A `sync.WaitGroup` is how the pool knows its workers have finished. The discipline
is exact: call `Add` before you launch the goroutine (never inside it — adding
from within races the `Wait` and can under-count, letting `Wait` return before a
worker has even started), call `Done` via `defer` so it fires on every exit path,
and call `Wait` only after all the `Add`s. A pool encodes its *drain* contract in
this: `Close` closes the job channel and then calls `wg.Wait()`, so `Close` blocks
until every worker has drained the queue and returned. That blocking is the
feature — it guarantees no in-flight job is lost when you shut the pool down.

### Drain versus cancel are different shutdown contracts

"Graceful shutdown" is two distinct contracts and conflating them causes hangs.
Drain (a soft `Close`) means stop accepting new work and let everything already
in flight finish. Cancel (a hard `Shutdown`) means propagate context cancellation
so in-flight jobs abort early. A drain-only pool whose jobs make a hung upstream
call will block forever waiting for that call; a cancel-only shutdown throws away
work that was seconds from completing. Production shutdown usually wants
drain-with-deadline: stop accepting, wait for in-flight work, but give up after a
grace period and return `context.DeadlineExceeded` so an orchestrator receiving
`SIGTERM` is not left hanging past its own kill timeout.

### Context must reach the job, not just the worker

A pool that runs cancelable work has to pass `context.Context` *into* each job —
the job signature becomes `func(ctx context.Context) error` — not merely cancel
the worker loop. Cancelling the loop stops new jobs from starting; it does nothing
for a job already blocked in a network call, because that call never learns it
should stop. `errgroup.WithContext` and `semaphore.Acquire(ctx, ...)` already
thread cancellation for you; a hand-rolled pool must do it explicitly by handing
the context to the job and having the job select on `ctx.Done()`.

### Backpressure and load-shedding beat unbounded queueing

An unbounded queue looks like it is "accepting" all the work, but it is really
trading a memory-and-latency blowup for the appearance of acceptance. Under
sustained overload the backlog grows without limit, latency climbs as jobs wait
behind an ever-longer line, and eventually the process runs out of memory. The
correct behavior under overload is to *reject*: a non-blocking send onto a bounded
queue with a `default` case that returns `ErrQueueFull` (the moral equivalent of
an HTTP 503) so callers fail fast and can retry elsewhere or shed the request.
Load-shedding keeps the service responsive; unbounded queueing hides the overload
until it becomes an outage.

### Panic isolation: one bad job must not shrink the pool

An unrecovered panic in a worker goroutine is not caught by whoever submitted the
job — it crashes the entire process. The submitter's stack is long gone; there is
no `try` around `Submit` that can catch it. So each job must run under a deferred
`recover` inside the worker loop, converting a panic into a per-job error (and
ideally a metric and a stack log) instead of a process crash. The subtler failure
is a *bare* recover with no reporting: it stops the crash but silently kills
nothing visible while the real damage — if you got the structure wrong and the
worker goroutine still exits — is that the pool permanently loses a worker and its
effective concurrency shrinks with no signal. Recover per job, keep the worker
alive, and surface the recovered value.

### Fan-out/fan-in with index correlation

When a pool processes a slice and you need to map each output back to its input,
carry the input index in the result: a `Result{Index, Value, Err}` collected on a
channel, or each goroutine writing a distinct preallocated slot. Writing to a
distinct index needs no lock and has no data race, because no two goroutines touch
the same element. The race trap is the opposite: sharing one slice and appending
concurrently, or having several goroutines write overlapping regions — that is a
data race the `-race` detector will catch. Give each goroutine its own slot, or
serialize through a results channel.

### Observability: pools are latency amplifiers when saturated

A saturated pool does not fail loudly; it just gets slow, because work waits in
the queue before a worker picks it up. To detect that in production you need three
signals: queue depth (how much backpressure is building), active workers (are you
using all of `Size`, i.e. saturated, or idling), and submit-to-start wait time
(the queueing delay each job actually experienced). Read these through atomics so
a snapshot is race-free, and you can tell the difference between "the pool is fine
and the downstream is slow" and "the pool itself is the bottleneck and needs more
workers." Without these signals, pool saturation looks identical to a slow
dependency, and you tune the wrong thing.

## Common Mistakes

### Never closing the pool

Wrong: create a pool, submit work, and never call `Close`. The worker goroutines
block forever on the job channel and leak; over a long-running service these
accumulate. `Close` — channel close plus `wg.Wait()` — is mandatory, and pairing
it with `defer` at the call site is the habit.

### Submitting to a closed pool without a guard

Wrong: `Submit` does `p.jobs <- job` directly. After `Close` closes the channel,
that send panics and takes down the process. Guard with a mutex and a `closed`
flag (or a select on a done channel) and return `false`/`ErrClosed` so the caller
learns the pool is gone instead of crashing.

### Closing the channel from a receiver, or closing it twice

Wrong: a worker closes the job channel, or `Close` closes it every time it is
called. Both panic. Only the owner/sender closes, and exactly once — the `closed`
flag makes `Close` idempotent.

### Calling wg.Add inside the goroutine

Wrong: the worker calls `p.wg.Add(1)` as its first line. That races `Wait`: the
main goroutine may call `Close`/`Wait` before the worker has run its `Add`, and
`Wait` returns while workers are still live. Always `Add` before `go`.

### Oversizing a CPU-bound pool

Wrong: a thousand workers for in-process hashing "to go faster." CPU-bound
throughput is capped by cores; the extra workers only add scheduling overhead and
memory. Size CPU-bound pools to roughly `GOMAXPROCS`.

### Using an unbounded queue as the pool

Wrong: an ever-growing slice or an unbuffered fan-out with no ceiling standing in
for a pool. It hides overload as latency and memory growth until the process is
OOM-killed. Bound the queue and shed load past capacity.

### Ignoring context in the workers

Wrong: workers that cannot observe cancellation. After `Shutdown` a job stuck in a
hung upstream call keeps running and blocks the drain indefinitely. Pass `ctx`
into the job and select on `ctx.Done()`.

### Letting a job panic without recover

Wrong: no `recover` in the worker loop. One panicking input takes down the whole
process. Recover per job and convert the panic to an error.

### Confusing concurrency limiting with rate limiting

Wrong: assuming `SetLimit` or a semaphore also caps requests per second. They cap
*simultaneous* work, not the rate. To protect a per-second quota you additionally
need a `rate.Limiter`.

### errgroup without SetLimit, or ignoring first-error cancellation

Wrong: using `errgroup` but forgetting `SetLimit`, so it still spawns an unbounded
number of goroutines; or forgetting that the first error cancels the shared
context, so the remaining functions should check `ctx.Err()` and stop early rather
than plowing ahead.

### Racing on a shared slice

Wrong: several goroutines append to one slice or write overlapping regions of it.
That is a data race. Give each goroutine a distinct preallocated index, or collect
through a results channel.

### Blocking Submit under load with no escape

Wrong: `Submit` does a blocking send with no select-default and no timeout, so
under load the caller stalls inside the pool instead of getting fast backpressure.
Offer a `TrySubmit` that does a non-blocking send and returns `ErrQueueFull`.

Next: [01-bounded-worker-pool.md](01-bounded-worker-pool.md)
