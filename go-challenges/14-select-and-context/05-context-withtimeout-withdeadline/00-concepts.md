# Context WithTimeout and WithDeadline: Time-Budgeting Production Work — Concepts

A service keeps its SLO promise one unit of work at a time. Every request that
enters the door carries an implicit budget — "answer within 300ms" — and that
budget is the contract a slow dependency, a hung query, or a runaway loop is not
allowed to break. `context.WithTimeout` and `context.WithDeadline` are how that
budget becomes a concrete, propagating value: a derived context that closes its
`Done` channel and sets `ctx.Err()` to `context.DeadlineExceeded` the instant the
budget runs out. This file is the conceptual foundation for the whole lesson. Read
it once and you have the model needed to reason through each independent exercise
that follows: the outbound client with a per-call timeout, the repository query
bounded by a budget, the retry loop that respects the total deadline, the fan-out
that splits one budget across branches, the cache read that degrades instead of
blocking, and the observability hooks that attribute a blown deadline to the exact
stage that blew it.

## Concepts

### WithTimeout and WithDeadline are the same mechanism

`context.WithTimeout(parent, d)` is defined as exactly
`context.WithDeadline(parent, time.Now().Add(d))`. Both derive a context with an
absolute deadline; the only difference is the shape of the input. Use
`WithTimeout` for a *relative* budget — "this should take at most N seconds" —
which is what almost every handler, client, and query wants. Reach for
`WithDeadline` only when you genuinely have an *absolute* wall-clock cutoff, which
is rare outside retry loops that compute a total-budget instant and token/lease
expirations. When the deadline arrives, both close `ctx.Done()` and set
`ctx.Err() == context.DeadlineExceeded`. Neither preempts anything; expiry is a
signal, not an interrupt.

### Earliest deadline wins, always

A derived context's *effective* deadline is the minimum of its own requested
deadline and every ancestor's deadline. The godoc is precise: "WithDeadline
returns a derived context ... with the deadline adjusted to be no later than d. If
the parent's deadline is already earlier than d, WithDeadline(parent, d) is
semantically equivalent to parent." The consequence a senior engineer internalizes
is that a child can never outlive its parent. If a request has 40ms of budget left
and you derive a child with a 5-second timeout, the child still dies in 40ms — the
extra 4.96 seconds are dead weight, a comforting number that changes nothing.
`ctx.Deadline()` reports the effective (possibly inherited) deadline, not the one
you asked for.

### Budget introspection: Deadline() and time.Until

`ctx.Deadline()` returns `(time.Time, bool)`; the bool is false when no deadline is
set. Given a deadline, `time.Until(deadline)` is the remaining budget as a
`time.Duration`. This is the lever behind graceful degradation. Senior code reads
the remaining budget and *decides*: skip an expensive enrichment step when too
little time remains and return a timely partial result; split the budget into
per-branch sub-budgets before a fan-out; refuse to start a retry attempt or a
backoff sleep that provably cannot finish in time. A timeout you only *react* to is
half the tool; a budget you *measure and plan against* is the whole tool.

### Cancellation is cooperative, not preemptive

This is the failure mode that dominates incident reviews. A deadline firing does
exactly one thing: it closes a channel. It does not stop a goroutine, interrupt a
syscall, or unwind a loop. The work must *observe* the cancellation. There are two
places that observation has to happen. First, at every blocking I/O call: you must
pass `ctx` into the operation itself — `http.NewRequestWithContext`,
`db.QueryContext`, a `select` on `ctx.Done()` — so the runtime aborts the in-flight
call when the deadline fires. Second, at the top of any long CPU-bound loop: a
non-blocking `select { case <-ctx.Done(): return ctx.Err(); default: }` each
iteration, because a tight loop with no check will run well past the deadline until
it happens to reach an I/O point. A deadline set but never observed is a lie the
dashboards will eventually expose.

### DeadlineExceeded versus Canceled drive different policy

`ctx.Err()` returns `context.DeadlineExceeded` when a timeout fires and
`context.Canceled` when someone called `cancel()`. These are not interchangeable.
"We ran out of time" often warrants a retry with a fresh budget or an alert about a
slow dependency; "the caller cancelled" (the client hung up, a sibling branch
failed) warrants stopping quietly and retrying nothing. Always return `ctx.Err()`
or wrap it with `%w`, and discriminate at the boundary with `errors.Is`. Relabeling
one as the other — or swallowing the error entirely — makes callers apply the wrong
retry policy and makes dashboards misattribute the failure.

### The timer resource and defer cancel()

`WithTimeout` and `WithDeadline` allocate an internal timer. It is released when you
call the returned `cancel` *or* when the deadline fires, whichever comes first.
Calling `cancel` promptly — the moment the work finishes — returns the timer early
instead of letting it linger until expiry. Under high request volume, a missed
`cancel()` on work that finished fast leaves thousands of live timer entries queued
until their deadlines elapse. `go vet`'s `lostcancel` check flags the omission. The
rule has no exceptions: always `defer cancel()`, even when the deadline seems
certain to fire on its own. It costs one line and it is never wrong.

### Causes: attributing which stage blew the budget (Go 1.21)

When a request threads through several timed stages — authenticate, then fetch,
then render — and one of them times out, `ctx.Err()` is `DeadlineExceeded` for all
of them; it cannot tell you *which* stage was too slow. `context.WithTimeoutCause`
and `context.WithDeadlineCause` (Go 1.21) attach a semantic error to the deadline.
After expiry, `ctx.Err()` still returns `DeadlineExceeded` — that contract is
unchanged, so existing `errors.Is(err, context.DeadlineExceeded)` checks keep
working — but `context.Cause(ctx)` returns the labeled cause you supplied. Log and
increment metrics against `Cause`, and a dashboard can attribute a blown budget to
the exact stage and SLO. `context.WithCancelCause` (Go 1.20) does the same for
manual cancels: `cancel(myReason)` makes `Cause` return `myReason` while `Err`
stays `Canceled`.

### AfterFunc: fire compensation exactly at expiry (Go 1.21)

`context.AfterFunc(ctx, f)` runs `f` in its own goroutine when `ctx` becomes done
(cancelled or deadline-exceeded) and returns a `stop func() bool` that deregisters
`f`. It is the clean way to fire compensation precisely at expiry — release a
lease, emit a timeout metric, signal a downstream cancellation — without a manual
`go func() { <-ctx.Done(); ... }()` and its lifetime bugs. It carries an inherent
race: the work may finish at almost the same instant the deadline fires. `stop`
returns true if it deregistered `f` before it started (so `f` will not run) and
false if `f` has already started or finished. Making the compensation and the
normal cleanup mutually exclusive — a mutex-guarded "released" flag so exactly one
of them wins — is the contract you must enforce.

### Propagating deadlines across process boundaries

A request's budget should flow into every downstream call it makes. Within a
process, that is automatic: pass the same `ctx` down and each derived deadline
inherits the earliest. Across process boundaries it is not free. gRPC propagates
deadlines on the wire, so a server's remaining budget becomes the client stub's
deadline automatically. Plain HTTP does not: you translate the remaining budget
(`time.Until(deadline)`) into the outbound request's own timeout, or into a header
the downstream service reads. And the server's own `ReadTimeout`/`WriteTimeout` and
handler timeout cap what any handler-derived deadline can honestly promise — a
handler cannot grant a downstream call more time than the server will hold the
connection open.

### Monotonic clocks make deadline timing trustworthy

`time.Now()` carries a monotonic reading alongside the wall-clock time. Deadline
arithmetic and elapsed-time measurement use the monotonic component, so they are
immune to wall-clock adjustments — an NTP step or a manual clock change cannot make
a deadline fire early or an elapsed measurement go negative. This is why a timeout
set for "300ms from now" fires after 300ms of real elapsed time regardless of what
the wall clock does, and why tests can assert timing within a slack window at all.

## Common Mistakes

### Setting a child timeout longer than the parent's remaining budget

Wrong: deriving `child, _ := context.WithTimeout(parent, 5*time.Second)` when the
parent has 1 second of budget left, and reasoning as though the child now has 5
seconds. It does not. The parent's earlier deadline still wins; the child dies with
the parent, and the extra 4 seconds are meaningless.

Fix: treat the parent's deadline as a hard ceiling. If you need to allocate a
sub-budget, compute it from `time.Until(parentDeadline)`, never from a fixed
duration that might exceed it.

### Forgetting defer cancel() because "the deadline fires anyway"

Wrong: `ctx, _ := context.WithTimeout(parent, d); work(ctx)`. When `work` returns
before `d` elapses, the timer entry lingers until the deadline, leaking timers
under load.

Fix: `ctx, cancel := context.WithTimeout(parent, d); defer cancel()`. `go vet`'s
`lostcancel` catches the omission; there is no case where dropping `cancel` is
correct.

### Checking ctx.Done() only after an I/O call

Wrong: a CPU-bound `for { compute(item) }` loop with no cancellation check runs
well past the deadline until it happens to hit an operation that respects `ctx`.

Fix: a non-blocking `select { case <-ctx.Done(): return ctx.Err(); default: }` at
the top of every iteration, so the loop honors the deadline between I/O points.

### Relabeling a timeout as Canceled (or swallowing ctx.Err())

Wrong: returning `context.Canceled` from a path that actually hit a deadline, or
dropping `ctx.Err()` and returning a generic error. Callers then apply the wrong
retry policy and dashboards misattribute the failure.

Fix: return `ctx.Err()` or wrap it with `%w`, and discriminate with
`errors.Is(err, context.DeadlineExceeded)` versus
`errors.Is(err, context.Canceled)`.

### Not passing ctx into the actual I/O

Wrong: `http.NewRequest` instead of `http.NewRequestWithContext`, or `db.Query`
instead of `db.QueryContext`. The deadline fires and `ctx.Done()` closes, but the
in-flight syscall keeps running to completion, so the timeout frees no resource and
the slow dependency still consumes a connection for its full duration.

Fix: thread `ctx` into the operation that does the blocking, every time. The
context is only as effective as the deepest call that honors it.

### Assuming context.Cause(ctx) equals ctx.Err()

Wrong: after a `WithTimeoutCause` expiry, reading `ctx.Err()` when you wanted the
attribution. `Err()` is `DeadlineExceeded`; only `context.Cause(ctx)` returns the
labeled cause. Reading the wrong one silently discards the observability you added.

Fix: check `ctx.Err()` for policy (timeout vs cancel) and `context.Cause(ctx)` for
attribution (which stage/SLO). They answer different questions.

### Retrying with backoff that ignores the total budget

Wrong: a retry loop that sleeps its full exponential backoff regardless of the
caller's deadline, overrunning the budget, or a `time.Sleep` in the backoff that
cannot be cancelled so a mid-backoff cancel waits out the whole sleep.

Fix: before each attempt and each sleep, check `time.Until(deadline)` and refuse a
step that cannot finish in time; sleep with a `ctx`-aware timer
(`select { case <-timer.C: case <-ctx.Done(): }`) so a cancel returns immediately.

### Blocking a request on a slow cache under the full budget

Wrong: reading a cache under the whole request budget, so a stalled cache stalls
the request. A cache is an optimization, not a dependency you block on.

Fix: read the cache under a deliberately short timeout derived from the parent, and
on `DeadlineExceeded` fall back to the source of truth (or a safe default) using the
remaining parent budget. The short cache timeout must not cancel the parent.

Next: [01-request-budget-toolkit.md](01-request-budget-toolkit.md)
