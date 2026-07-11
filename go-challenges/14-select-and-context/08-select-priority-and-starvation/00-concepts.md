# Select Priority and Starvation — Concepts

Go's `select` is deliberately *fair*: when more than one case is ready, the
runtime picks one uniformly at random ([Go spec, Select statements](https://go.dev/ref/spec#Select_statements)).
That is a feature — it stops any single channel from monopolizing a long-running
`select` — but it has a consequence senior engineers keep colliding with in
production: **priority is never free.** There is no case-ordering, no "put the
important channel first" trick, no runtime knob that makes one channel win. If
some signal must preempt another — cancellation must beat work, an error must
beat a result, a shutdown must beat a backlog, tenant A must not drown tenant B —
you *engineer* that priority out of primitives. This lesson is the toolbox for
doing it without busy-spinning the CPU and without leaking goroutines.

Read this file once and you have the model behind all nine independent exercises
that follow: a cancellation-first consumer, an anti-starvation drain, a
drain-with-deadline shutdown, a fail-fast fan-in, a weighted fair scheduler
across QoS classes, a rate-limited tiered dispatcher, send-side load shedding,
dynamic-N priority via `reflect.Select`, and an SLO-based starvation detector.

## The runtime gives you fairness, not priority

The single most important sentence: **case order in a `select` has zero effect on
which case runs.** The spec says the runtime chooses uniformly at random among
the cases whose communication can proceed. Writing the high-priority channel as
the first case buys nothing. If you want priority you construct it, and the
construction is always the same shape: a *non-blocking peek* over the
high-priority signals, followed by a *blocking fall-through* over everything.

### The canonical priority idiom: peek, then fall through

The peek is a `select` with a `default` case, so it returns immediately whether
or not a high-priority channel is ready:

```go
// Peek: does hi have something right now? If not, default fires instantly.
select {
case v := <-hi:
	return v, true
default:
}
// Fall-through: nothing high was ready, so block on either channel.
select {
case v := <-hi:
	return v, true
case v := <-lo:
	return v, true
}
```

If `hi` is ready, the peek takes it and the low channel never gets a look. If
`hi` is empty, the peek's `default` fires and control drops to the blocking
`select`, which parks the goroutine until *any* channel is ready. The peek adds
the cost of one extra `select` per iteration — negligible next to the channel
operations themselves — and in exchange you get strict preference for `hi`.

### Cancellation belongs in its own strict check, not merged into the peek

A subtle trap: it is tempting to write the peek as `select { case <-ctx.Done():
...; case v := <-hi: ...; default: }`. But if the context is *already* cancelled
**and** `hi` has a buffered item, both cases are ready, and `select` picks one at
random — so a cancelled request can still be handed a high-priority item roughly
half the time. If cancellation must *strictly* win, give it its own non-blocking
`select` **before** the high peek:

```go
select {
case <-ctx.Done():
	return zero, false // cancellation strictly first
default:
}
// ... then the hi peek, then the blocking fall-through
```

And `ctx.Done()` must *also* appear in the blocking fall-through — otherwise, on
an idle consumer parked at the fall-through, a later cancellation would never
wake it. The rule is: cancellation is checked strictly first for preemption, and
again in the fall-through for wakeup. Omit it from the fall-through and an idle
consumer hangs after cancel; omit the strict pre-check and a saturated `hi`
channel means the loop never even reaches a place where cancellation can win —
the consumer keeps draining a backlog for a request that is already gone.

## Strict priority *is* starvation, by construction

This is the property to internalize, not a bug to fix. A consumer that always
prefers `hi` and only reads `lo` when `hi` is momentarily empty will, under a
continuously-fed `hi` channel, *never* read `lo`. That is not a defect in the
code — it is exactly what "strict priority" means. Whether your workload
tolerates it is a design decision you must make explicitly. If `lo` carries
best-effort telemetry that can wait indefinitely, strict priority is correct. If
`lo` carries a second tenant's requests, strict priority is an outage for that
tenant, and you need bounded fairness.

### Bounded fairness: a counter, or deficit round-robin

The cheapest fairness mechanism is a **consecutive-high counter**: after serving
N high items in a row, force one low item if any is available, then reset the
counter. This trades a little high-priority latency (at most one low item every
N) for a hard guarantee that no more than N high items are served before a low
one gets a turn. It is a single integer of state that the loop consults.

For more than two classes, the generalization is **weighted (deficit)
round-robin**: each class carries a budget proportional to its weight; a class
with weight 3 is served roughly three times as often as a class with weight 1
over a frame, but every class with a positive weight is served at least once per
frame, so none starves. The share is proportional; the floor is non-zero.

A knob that is *exposed but never wired* is worse than no knob: it silently does
nothing and the low class still starves while the operator believes fairness is
on. Fairness needs real state (a counter or per-class deficit) that the dispatch
loop actually reads on every iteration.

## The busy-loop trap

`default` makes the peek non-blocking. That is the point — but it means the peek
is *not* a place the goroutine can park. In the peek-then-fall-through structure,
the **blocking fall-through is the only point where the loop sleeps.** Any
mistake that skips the fall-through, or that adds a `default` to it, turns the
consumer into a CPU-burning spin: with all channels empty, the loop cycles as
fast as the scheduler allows, pinning a core to 100% for no work.

So: `default` belongs only in the peek, never in the blocking `select`. And if
your control flow can reach the top of the loop without passing through a
blocking wait — for example a fairness pre-check that `continue`s — make sure the
common empty-channel path still lands on the blocking fall-through. When there is
genuinely no channel to block on (a poll over a set that may all be empty), park
on a `time.Ticker` or add a small backoff between peeks instead of spinning.

## Priority applies to the send direction too

Everything above is receive-side, but `select` with `default` is equally the
primitive for **backpressure and load shedding on the send side**. A non-blocking
send tries to enqueue and, if the downstream buffer is full, does something other
than block:

```go
select {
case downstream <- item:
	// enqueued
default:
	// downstream full: shed (drop + count) or divert to an overflow path
}
```

Under sustained overload this is how an ingress stays responsive instead of
building an unbounded queue: high-priority items that cannot be enqueued are shed
with a counter (so the loss is *observable*), and low-priority items are diverted
to an overflow or dead-letter channel. A silent non-blocking send — one that
drops with no metric — loses data with no signal, which is how a "mysterious"
gap in downstream traffic becomes an all-night incident.

## Dynamic channel sets need reflect.Select

The static peek-then-fall-through works when the channels are known at compile
time. When the *set* of channels is only known at runtime — dynamic
subscriptions, a config-reload that adds and removes sources — you cannot write a
fixed `select`. `reflect.Select(cases []reflect.SelectCase) (chosen int, recv
reflect.Value, recvOK bool)` selects over a slice of cases built at runtime. Use
a `reflect.SelectDefault` case to build a non-blocking peek pass (iterate the
channels in priority order, each as a one-shot recv-or-default), then a blocking
`reflect.Select` over the full case set plus `ctx.Done()`. The `recvOK` return is
`false` when the chosen channel is closed, which is your signal to prune it from
the set. `reflect` is slower and loses static type safety, so reach for it *only*
when N or membership is genuinely dynamic — for a fixed set, static selects win.

## Graceful shutdown is priority in time

Shutdown is a priority problem on the time axis: the shutdown signal must preempt
buffered work, but slamming to a halt drops in-flight items. The production shape
is: on `ctx.Done()` (from `signal.NotifyContext` catching SIGTERM), stop
accepting *new* work, then drain what is already in flight under a **bounded
deadline**, and abort whatever remains when the deadline fires. Bounding the
drain matters: an unbounded drain lets a slow producer keep the process alive
past its shutdown budget, and Kubernetes escalates SIGTERM to SIGKILL anyway.
`context.WithTimeoutCause(parent, d, cause)` plus `context.Cause(ctx)` make the
*reason* the drain ended recoverable — you can tell "drained cleanly" from
"hit the drain deadline and dropped 4 items" after the fact.

## Fail-fast fan-in is priority over an error channel

A scatter-gather that fans work out to N goroutines and collects results should
not keep aggregating after the first failure. Model it as priority: peek the
error channel (and `ctx.Done()`) *before* consuming the next result, so the first
error aborts immediately and cancels the shared context, which unblocks the
remaining workers so they exit instead of leaking. The two failure modes to
avoid are (a) returning on the first error without cancelling — the other workers
block forever on channels no one will read, a classic goroutine leak — and (b)
using unbuffered error/result channels such that a worker trying to report after
the aggregator has moved on blocks forever. Cancel the context and give the
losing sends somewhere to go.

## Rate limiting and priority compose

High priority must not become a way to *bypass* a rate limit. The composition is:
gate emission on a token — a `time.Ticker` tick, or a `golang.org/x/time/rate`
limiter token — and, *per token*, apply the priority peek to choose which tier to
emit. Priority then reorders *within* the cap (high tier drained first when both
are ready at a token) instead of escaping it. Emitting high-priority items
outside the token cadence blows the global rate cap, which is exactly the
outbound-API abuse the limiter existed to prevent.

## Starvation is observable — make it an alert, not a surprise

Because strict priority silently starves the low class, starvation is invisible
until a customer complains. Turn it into a signal: record the last-served time
per class and, against a per-class max-wait SLO, emit a starvation event when a
class exceeds its budget. Fire the event on the *transition* into breach (once
per episode, gated by a "already breached" flag), not on every loop iteration, or
the alert becomes noise. Now "tenant B is being starved" is a metric an operator
sees before it becomes an incident.

## Common Mistakes

### Assuming select evaluates cases top to bottom

Wrong: ordering the high-priority case first and expecting it to win. `select`
chooses uniformly at random among ready cases; order confers nothing. Fix: build
an explicit non-blocking peek over the high channel, then a blocking
fall-through.

### Putting default in the blocking fall-through

Wrong: `select { case <-hi: ...; case <-lo: ...; default: }`. With both channels
empty the loop spins at 100% CPU. Fix: the `default` lives only in the peek; the
fall-through must be a real blocking `select` so the goroutine parks.

### Omitting ctx.Done() from the peek (or merging it wrongly)

Wrong: checking cancellation only in the fall-through — a saturated `hi` channel
means the loop never reaches the fall-through, so the consumer keeps working
after the request is cancelled. Also wrong: merging `ctx.Done()` and `hi` into
one peek `select` and expecting cancellation to strictly win — when both are
ready, `select` is random. Fix: a strict non-blocking `ctx.Done()` check first,
and `ctx.Done()` again in the fall-through for wakeup.

### Exposing a fairness knob that does nothing

Wrong: a `fairness int` parameter the loop never reads (the original
`PriorityFair` bug) — the low class still starves and the knob is a lie. Fix:
fairness must be backed by real state, a consecutive-high counter or per-class
deficit, that the loop consults every iteration.

### Calling the priority function in a tight empty loop

Wrong: `for { Priority(ctx, hi, lo) }` when both channels are usually empty and
the function itself does not block — it burns CPU peeking. Fix: ensure the common
path parks on the blocking fall-through, or in a poll-style scheduler park on a
`time.Ticker`/backoff between peeks.

### Early-returning from a fan-in on first error without cancelling

Wrong: returning the first error and leaving the shared context live — the
remaining workers block forever on unread result channels and leak. Fix: cancel
the shared context on the first error (and on ctx done), then `wg.Wait()` so the
workers observe cancellation and exit before you return.

### Draining on shutdown with no deadline

Wrong: looping over the work channel until it is empty on shutdown — a slow
producer keeps the drain (and the process) alive past its shutdown budget. Fix:
bound the drain with `context.WithTimeoutCause`, count what is dropped when the
deadline fires, and expose the reason via `context.Cause`.

### Reaching for reflect.Select on a static set

Wrong: using `reflect.Select` when the channels are known at compile time — it is
slower and discards static type safety for no benefit. Fix: use `reflect.Select`
only when the set is genuinely dynamic at runtime; otherwise write static selects.

### Letting high priority bypass the rate limiter

Wrong: emitting high-tier items the moment they arrive, outside the token
cadence — the global rate cap is blown. Fix: gate every emission on a token and
apply the priority peek per token, so priority reorders within the cap.

### Non-blocking send that drops silently

Wrong: `select { case ch <- v: default: }` that discards on a full buffer with no
counter — data vanishes with no signal. Fix: increment a shed counter (and, where
it matters, route to an overflow/dead-letter path) so every drop is observable.

Next: [01-priority-peek-consumer.md](01-priority-peek-consumer.md)
