# Building a Concurrent Task Scheduler — Concepts

Nearly every backend service grows a background-job subsystem: the thing that
runs webhooks, sends email, processes images, retries failed outbound calls,
sweeps expired rows, and generally does work that must not happen on the request
goroutine. That subsystem is a task scheduler — a worker pool fed by a job queue,
with a result path, backpressure, shutdown semantics, and metrics. It looks
trivial in a diagram and is subtle in production, because the failure modes are
the ones operators actually page on: an unbounded queue that OOMs the process, a
missing backpressure policy that turns a downstream slowdown into a request
pileup, a shutdown path that either drops in-flight work or hangs a rolling
deploy, one poison task that panics and kills the whole pool, per-task deadlines
that leak goroutines because the tasks never cooperate with cancellation, retry
loops without jitter that stampede a recovering dependency, and metrics that lie
under concurrency. This chapter builds that subsystem one concern at a time, and
every module is validated the way production Go is validated — `go test -race
-count=1` plus `go vet` — because a scheduler that passes only without the race
detector is not shippable.

## Concepts

### A scheduler is three parts: a queue, a pool, and a result path

Strip a task scheduler to its essentials and there are exactly three moving
pieces. A *job queue* (a buffered channel) decouples producers — request
goroutines calling `Submit` — from consumers. A *fixed worker pool* (a set of
goroutines ranging over that channel) bounds concurrency: no matter how fast work
arrives, at most `workers` tasks run at once, which is what protects the CPU, the
database connection pool, and any downstream you call. And a *result path* — a
per-task channel the worker writes the outcome to — lets a worker deliver a
result without blocking on a caller that may have walked away. The queue is the
decoupling, the pool is the throttle, and the result channel is the delivery. Get
those three contracts right and the rest of the chapter is variations on them.

The result channel deserves care. It must be buffered with capacity exactly one
(or the worker must `select` on a done signal), so that a worker is *never*
blocked forever delivering a result to a caller that already timed out and
stopped reading. An unbuffered result channel plus an abandoned caller is a
textbook goroutine leak: the worker parks on the send, the pool loses a worker,
and eventually every worker is wedged on a dead caller. Capacity one means the
worker always completes its send and moves to the next job, whether or not anyone
is listening.

### Submit must respect the caller's context

A `Submit` that enqueues with a bare `s.tasks <- t` blocks the caller whenever the
queue is full. If that caller is an HTTP handler with a 200 ms request budget,
blocking on a full queue burns the request's deadline parking a goroutine that
should have returned 503. The correct enqueue is a `select` over the send and
`ctx.Done()`:

```go
select {
case s.tasks <- t:
	return resultCh
case <-ctx.Done():
	return failed(ctx.Err())
}
```

Now the handler's own request context (or a derived deadline) aborts the enqueue
instead of pinning a goroutine past the point the client cares about. Context
propagation is not decoration; it is the mechanism that lets load shed at the
edge instead of collapsing at the core.

### Backpressure is a design choice, not an accident

An unbounded queue does not remove the failure — it moves it. Instead of
rejecting a request now (a `503` the caller can retry), an unbounded queue accepts
everything and crashes later with an out-of-memory kill that takes the whole
process, including all the in-flight work it was holding. A bounded queue with an
explicit admission policy is the production-correct default: when the queue is
full you either fast-fail (`TrySubmit` returns `ErrQueueFull`, a non-blocking
`select` with a `default` clause) or you block up to the caller's deadline and
then fail. Both are honest; the unbounded queue is the one that lies until it
dies. Choosing *which* backpressure policy — shed, block-with-deadline, or a
bounded blocking queue — is a capacity-planning decision, and a senior engineer
is expected to make it explicitly rather than inherit an accidental unbounded
channel.

### Shutdown has two legitimate meanings and a service needs both

`net/http.Server` exposes both `Shutdown(ctx)` and `Close()` for a reason, and a
task scheduler needs the same two verbs. *Graceful drain* stops accepting new
work, lets in-flight tasks finish, and is bounded by a deadline: if the drain does
not complete in time, it returns `ctx.Err()` and the operator decides what to do.
*Hard cancel* signals running tasks to abort immediately. During a rolling deploy
you want drain (finish the webhook you started); when drain overruns its budget
you escalate to cancel (do not hang the deploy forever). Conflating the two causes
one of two paging incidents: a "graceful" shutdown that actually drops in-flight
work, or a "clean" shutdown that hangs indefinitely because one wedged task never
returns. Model both, make them idempotent (a second `Shutdown`/`Close` must not
panic on a double-closed channel), and the deploy story becomes predictable.

### Never hold a mutex across a channel send or any blocking call

This is the single most common concurrency bug in a hand-rolled scheduler. If
`Submit` takes a lock to mutate a counter and *keeps holding it* across
`s.tasks <- t`, then whenever the queue is full the send blocks with the lock
held — which serializes every other `Submit` behind it and can deadlock outright
if a worker needs the same lock to make progress. The discipline is mechanical:
take the lock only to mutate small shared state (a `closed` flag, a counter),
release it, then do the blocking send. A lock guards data, not I/O.

### Single ownership beats locking for shared data structures

A `container/heap` (used for priority dispatch and for delayed tasks) is not
safe for concurrent use, and wrapping every heap operation in a mutex is both
slow and error-prone. The idiomatic fix is *single ownership*: one dispatcher
goroutine owns the heap outright and is the only code that ever touches it.
Producers hand it work over a channel; it hands the highest-priority ready task
to an idle worker over another channel. Because exactly one goroutine reads and
writes the heap, `container/heap` needs no locking at all, and the data race is
structurally impossible rather than merely guarded. Channels move the ownership;
they do not merely move the data.

### Delayed scheduling is a min-heap plus one re-armable timer

Running a task "after 30 s" or "at 09:00" is a min-heap keyed by run-at, drained
by a single `time.Timer` armed for the earliest entry. The subtle bug is the
timer re-arm: when a *nearer* task is inserted while the timer is already armed
for a later one, you must `Stop` and `Reset` the timer to the new earliest time,
or the nearer task fires late (it waits behind the previously-earliest deadline).
`time.Timer.Reset` has a documented discipline — you may only call it on a stopped
or expired timer, and you must be careful about draining the channel — and getting
it wrong produces either a late task or a spurious fire. A single dispatcher
goroutine owning both the heap and the timer keeps this correct and race-free.

### Per-task deadlines only work if the task cooperates

`context.WithTimeout` does *not* preempt a running goroutine — Go has no
preemptive cancellation of arbitrary code. It signals: it closes `ctx.Done()` and
sets `ctx.Err()`. A task that never selects on `ctx.Done()` (or never passes the
context to the DB/HTTP call that does) runs to completion regardless of the
deadline. So per-task timeouts have two distinct effects that are easy to
conflate: the *accounting* is freed at the deadline (the scheduler records a
`DeadlineExceeded` result and considers the slot available), but the *goroutine*
is only freed if the task actually observes cancellation. Design tasks as
`func(ctx context.Context) (any, error)` and make cooperation the contract; a task
that ignores its context is a latent worker-pinning bug. And every derived context
needs `defer cancel()`, or you leak the timer goroutine behind every
`WithTimeout`.

### recover runs only in a deferred function on the panicking goroutine

A task that panics will, by default, take down the entire process — a single
poison payload kills every worker and every in-flight job. The fix is a
`defer`/`recover` wrapper around *each* task invocation, on the worker goroutine
that runs it, because `recover` only works when called directly from a deferred
function on the goroutine that panicked. A recovered panic must become an *error
result*, not a silent success: convert the recovered value into an `error` (with
`fmt.Errorf`), record it, and let the worker move to the next task. The pool
survives one bad task; the bad task's caller learns it failed.

### Fixed-count pools vs. cost-weighted admission

A fixed-count worker pool bounds concurrency by *goroutine count* — fine when
tasks are roughly uniform. When tasks are heterogeneous (a few jobs each need
2 GB, many need 20 MB), a count-based limit over-commits on the heavy ones: eight
"workers" running eight 2 GB jobs blows the memory envelope. The right tool is a
*weighted semaphore* (`golang.org/x/sync/semaphore.Weighted`), where each task
declares a cost and admission is against a total-capacity budget. A few heavy
jobs and many light jobs then share a fixed resource envelope without
over-committing. `Weighted.Acquire` honors context cancellation, and every
`Acquire(n)` must be paired with exactly-matching `Release(n)` on every path
(including the error path), or capacity leaks and the effective envelope shrinks
until the scheduler stalls.

### Retry needs capped exponential backoff with jitter and a bound

A job runner retries transient failures, but naive retry is a footgun. Retrying
immediately, or on a fixed interval, or in lockstep across many workers, produces
a synchronized retry storm — a thundering herd that hammers a dependency exactly
when it is trying to recover. The production pattern is capped exponential backoff
(delay doubles each attempt, up to a ceiling) with jitter (randomize the delay so
retries de-synchronize) and a hard bound (max attempts or an overall deadline).
Terminal failures — the ones that exhaust retries — go to a *dead-letter* channel
for inspection rather than vanishing. Backoff sleeps must themselves respect the
context deadline, or a "bounded" retry blocks past shutdown.

### Observability counters must be atomic and semantically coherent

Operators cannot run a scheduler they cannot see. The minimum surface is:
submitted, completed, failed, and retried counts; an in-flight gauge; queue
depth; and per-task latency. Two rules make these trustworthy under concurrency.
First, use atomics (`sync/atomic.Int64`) so a concurrent `Stats()` read never sees
a torn value. Second, keep the numbers *coherent*: the in-flight gauge must be
derived as `started − finished` from two atomics, not tracked as a single counter
incremented at start and decremented "somewhere", which drifts under races and
early returns. A `Stats()` snapshot built from atomic loads gives an observer a
consistent view without a global lock, and exporting it via `expvar` makes it
scrape-able by a metrics agent. A gauge that can go negative or a counter that
disagrees with itself is worse than no metric: it sends the operator chasing a
phantom.

## Common Mistakes

### A Stop that signals but never joins the workers

Wrong: `Stop` closes the stop channel (or the task channel) and returns
immediately. The workers are still draining and may still write to channels the
test has torn down; the goroutines leak.

Fix: `Stop` must `s.wg.Wait()` on a `sync.WaitGroup` the workers `Done` on exit,
so `Stop` returns only after every worker has actually stopped.

### Submitting to a scheduler whose task channel is already closed

Wrong: `Submit` sends to `s.tasks` with no guard; after `Stop` has closed it, the
next `Submit` panics with "send on closed channel".

Fix: guard with a `closed` flag under a mutex and return `ErrShuttingDown` instead
of sending. Only the owner closes the channel, and only after fencing out
producers via the flag.

### Holding the mutex across the enqueue send

Wrong: `s.mu.Lock(); s.count++; s.tasks <- t; s.mu.Unlock()`. On a full queue the
send blocks with the lock held, serializing every `Submit` and inviting deadlock.

Fix: mutate under the lock, unlock, then do the `select` send outside the lock.

### Unbuffered result channels

Wrong: `done := make(chan Result)` and a worker that sends the result to it. A
caller that timed out and stopped reading leaves the worker blocked forever on the
send — a permanent goroutine leak.

Fix: `done := make(chan Result, 1)`. The buffer of one lets the worker complete
the send and move on regardless of whether the caller is still listening.

### Closing the task channel from a producer while other producers may still Submit

Wrong: any producer calls `close(s.tasks)`. A second producer mid-`Submit` then
panics on the closed channel.

Fix: only the owner (`Stop`) closes, and only after the `closed` flag has fenced
all producers out. Multiple producers, single closer.

### Ignoring ctx in Submit

Wrong: enqueue with a bare `s.tasks <- t`, so a full queue parks the caller past
its request deadline.

Fix: always `select` over the enqueue and `ctx.Done()`, so the caller's deadline
aborts the enqueue.

### Tracking the in-flight gauge as one drifting counter

Wrong: a single `running` counter incremented on start and decremented on finish,
which drifts under races and early returns and can read negative.

Fix: derive `Running = started − finished` from two monotonic atomics; it can
never disagree with itself.

### Re-arming the delay timer incorrectly

Wrong: inserting a nearer task without `Stop`/`Reset`-ing the timer, so the nearer
task fires only after the previously-earliest deadline; or draining the timer
channel wrong on `Reset`, producing a spurious fire.

Fix: follow the documented `Timer.Reset` discipline — reset to the new earliest
run-at whenever the heap's minimum changes.

### Assuming WithTimeout preempts a running task

Wrong: expecting a `context.WithTimeout` to stop a task that never checks
`ctx.Done()`. It does not; the task runs to completion and the "freed" worker is
only freed on the books.

Fix: write tasks that select on `ctx.Done()` (or pass `ctx` into the blocking
call), and understand that worker-level timeout accounting frees the slot, not the
goroutine.

### Forgetting defer cancel() on a derived context

Wrong: `ctx, _ := context.WithTimeout(...)` with the `CancelFunc` discarded. Every
submitted task leaks the timer goroutine behind its context.

Fix: `ctx, cancel := context.WithTimeout(...)` and `defer cancel()` (or cancel on
every exit path).

### Calling recover outside a deferred function, or not wrapping tasks at all

Wrong: no per-task `recover`, so one panicking task crashes the whole pool; or
calling `recover()` outside a `defer`, where it returns nil and does nothing.

Fix: wrap each task invocation in a deferred `recover` on the worker goroutine and
convert the recovered value into an error result.

### Releasing a weighted semaphore with the wrong weight

Wrong: `Acquire(n)` then `Release(m)` with `m != n`, or skipping `Release` on the
error path. Capacity permanently shrinks until the scheduler stalls.

Fix: `defer sem.Release(n)` with the exact `n` that was acquired, on every path.

Next: [01-worker-pool-scheduler.md](01-worker-pool-scheduler.md)
