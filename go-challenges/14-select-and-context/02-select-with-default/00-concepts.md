# Select with Default: Non-Blocking Channel Operations — Concepts

A `select` without a `default` branch blocks until at least one communication case
can proceed. Adding a `default` branch flips the semantics: if no case is ready at
the instant the `select` is evaluated, the `default` body runs immediately and the
`select` returns. That single keyword turns a blocking channel operation into a
non-blocking probe, and non-blocking probes are the load-shedding primitives of a
Go backend. A telemetry client that drops a sample rather than block the request
path, an admission gate that returns 503 instead of queueing unbounded work, a
counting semaphore that caps concurrent fan-out to a fragile downstream, a
token-bucket rate limiter with no mutex on the hot path, a coalescing config
mailbox that only ever holds the newest snapshot, a SIGTERM handler that flushes
what is already queued inside a deadline — every one of these is `select { case
... ; default: ... }` with a policy attached to the `default`. This file is the
conceptual foundation; read it once and each of the independent exercises that
follow is one production application of the same rule.

## Concepts

### The exact selection rule

The Go specification is precise about what `default` means. When a `select` is
evaluated, the runtime looks at every non-`default` case. If one or more can
proceed (a receive whose channel has a ready value or is closed, a send whose
channel has room or a waiting receiver), the runtime chooses exactly one of the
ready cases *uniformly at random* and `default` is not considered at all. Only
when no communication case can proceed is the `default` body executed. That is the
whole of it, and it is why a `default` never blocks the goroutine: there is always
a branch to take. The randomness among ready cases matters later — see the fairness
note — but the load-bearing fact for non-blocking operations is simply: `default`
runs if and only if nothing else can, right now.

### Three overload policies, and when each is correct

`default` forces you to make explicit a decision that a blocking channel operation
hides: when the channel is not ready, what do you do? There are exactly three
answers, and choosing among them is a capacity-planning decision, not a matter of
style.

WAIT — no `default` at all, or a `time.After`/`ctx.Done()` case. Use this for work
that must not be lost: a durable job, a write that a caller is waiting on. The cost
is that the goroutine parks; the buffer is your only slack, and if the consumer
stalls the producer stalls with it (backpressure propagates upstream).

DROP — a `default` that increments a dropped counter and returns. Use this for
best-effort signals: metrics, access logs, cache-warm hints, a "config changed"
nudge. The point is that losing one is cheaper than blocking the hot path. The
non-negotiable companion is a counter: a silent drop is a silent data-loss bug.

FAIL FAST — a `default` that returns a rejection to the caller. Use this for
request admission: when the queue is full, return 503/`ErrBusy` so the caller can
retry, degrade, or shed elsewhere, instead of letting an unbounded backlog grow
until the process OOMs and takes down in-flight work too. Fail-fast converts a
latent latency cliff into an explicit, observable rejection.

### Non-blocking receive and send are duals

A non-blocking receive is `select { case v, ok := <-ch: ...; default: ... }`. The
two-value form is important: `ok == false` distinguishes a *closed* channel from a
value, but both a closed-and-empty channel and a not-ready channel land you in
different branches — the closed channel makes the receive case ready (it yields the
zero value with `ok == false`), whereas a not-ready open channel takes `default`.
Code that conflates "closed" with "nothing ready, retry later" will spin forever on
a closed channel; handle `ok == false` explicitly. A non-blocking send is the
mirror: `select { case ch <- v: ...; default: ... }`. Both are O(1) and never park
the goroutine.

### Buffer sizing is load-bearing

A non-blocking send into an *unbuffered* channel with no receiver already parked
*always* takes `default` and fails, because an unbuffered send needs a rendezvous —
a receiver has to be waiting at that instant. This surprises people: `make(chan T)`
plus `TrySend` is a `TrySend` that can never succeed unless a goroutine happens to
be blocked in a receive right then. Buffered channels are what make try-send and
drop-when-full meaningful: the buffer absorbs a burst, and its length *is* your
burst tolerance. Size it to the largest burst you are willing to hold in memory;
past that, the `default` fires and your policy (drop or reject) kicks in.

### The busy-loop trap

`for { select { case ...; default: } }` with nothing ready is a busy loop: the
`default` makes each iteration return instantly, so the goroutine spins at the
scheduler's full rate and pegs a CPU core doing no work. `default` belongs to
*single-shot* non-blocking probes, not tight loops. If you find yourself putting a
bare `default` inside a `for`, you almost always wanted one of: remove the
`default` and let the `select` block; drive the loop from a `time.Ticker.C` case so
each iteration waits for a tick; or add explicit backoff. A polling loop is a
`select` over `ticker.C` and `ctx.Done()`, never a `default` spin.

### default does not implement a timeout

"We do not want to wait too long" is not a reason to reach for `default`. A
`default` gives you *zero* wait, not a bounded wait, and pairing it with a manual
`time.Now()` deadline check inside a `for` reconstructs the busy loop. A real
deadline needs a real timer case: `time.After(d)`, a shared `time.Timer`, or
`ctx.Done()`. The timer channel is the idiomatic bound; it fires once, wakes the
`select`, and costs nothing while you wait.

### Coalescing: the single-slot latest-value mailbox

A size-1 channel plus a non-blocking send-with-evict delivers only the *most
recent* value to a slow consumer, with bounded memory. The pattern: try to send;
if the slot is full, do a non-blocking receive to discard the stale value, then
send the new one. A plain non-blocking send into a full size-1 slot drops the
*new* value and leaves the stale one — the exact opposite of what "latest wins"
requires. Config reload, leader/state changes, and "something changed, go
recompute" notifications all want this shape: the consumer never sees a backlog,
only the current snapshot, and the producer never blocks or grows memory.

### Polling with a ticker and cancellation

The correct replacement for a `time.Sleep` loop is a `select` over `time.NewTicker`
`.C` and `ctx.Done()`. The ticker bounds the rate; cancellation wakes the loop
*immediately* instead of waiting out a sleep. Always `defer ticker.Stop()` — a
`Ticker` holds a runtime timer, and dropping the reference without `Stop` leaks
that timer until it is garbage-collected; across many short-lived poll calls the
leak accumulates. This is the shape of a startup readiness gate (poll the DB until
it answers, bounded by a boot deadline) and of any "do X every N until told to
stop" loop.

### Draining under shutdown

On SIGTERM you want to flush work that is *already queued* but not accept new work,
and you want to do it inside a bounded deadline. A non-blocking `Drain` — a
try-receive loop that stops the instant the buffer is empty — empties the queue and
then exits. Contrast that with `for job := range ch`, which blocks forever if the
producer stopped without closing the channel: your shutdown path hangs past its
deadline waiting on a producer that will never send again. Non-blocking drain, or a
`select` over the jobs channel and `ctx.Done()`, is what distinguishes "graceful
drain" from "hang".

### Observability of drops

Every DROP-policy `default` must feed a counter — `atomic.Int64`, an `expvar`, a
metrics hook. Shed load you do not count is shed load you cannot alert on, and the
first sign of a silent drop is a user complaint, not a graph. The counter is not
optional decoration; it is the difference between "we are shedding 2% under peak,
as designed" and "data is silently disappearing and nobody knows".

### Concurrency limiting with a counting semaphore

A buffered channel of capacity N is a non-blocking counting semaphore. `TryAcquire`
is a non-blocking send of a token into the buffer (success = a slot was free,
failure = at capacity); `Release` is a receive. This gives admission control for
downstream fan-out — cap the number of concurrent outbound calls to a fragile
dependency or connection pool — with no `sync.Mutex` on the hot path, and with the
option to reject immediately (`TryAcquire`) or wait with cancellation (`select`
over the send and `ctx.Done()`). The same channel-as-semaphore backs the
token-bucket limiter: tokens are the buffered elements, `Allow` is a non-blocking
receive, and a ticker refills with a non-blocking send that drops when full to cap
the bucket.

### The fairness nuance

Because `default` short-circuits the pseudo-random choice among ready cases, a
`default` that is "always takeable" can starve a case that is only *sometimes*
ready. In a loop that both does real work on a channel and has a `default`, if you
structure the branches so the `default` path dominates, the ready case that would
have been picked when both were ready never gets its fair random turn — the
`default` fires first. When you need the hot path to win, do not give it a
`default`: use a separate blocking `select` for the case that must be serviced, and
reserve non-blocking probes for the branches where dropping is the intended policy.

## Common Mistakes

### Busy-looping with a bare default

Wrong: `for { select { case v := <-ch: process(v); default: } }`. With nothing on
`ch`, the loop spins at scheduler speed and pegs a core.

Fix: block with `for v := range ch` (or a `default`-free `select`), or gate the
loop on a `time.Ticker.C` case, or add backoff. Reserve `default` for one-shot
probes.

### Using default to fake a timeout

Wrong: adding a `default` because "we do not want to wait too long", then checking
a clock — a busy loop that never actually bounds the wait.

Fix: use a `time.After(d)` or `ctx.Done()` case. That is a real, cheap deadline; a
`default` is a zero-wait, not a bounded wait.

### TrySend into an unbuffered channel

Wrong: `make(chan T)` then `select { case ch <- v: ; default: }`, and being
surprised it always takes `default`. An unbuffered send needs a receiver parked at
that instant; with none, it can never proceed non-blockingly.

Fix: buffer the channel (`make(chan T, n)`) if try-send/drop-when-full is the
intent; the buffer length is your burst tolerance.

### Silent drops with no counter

Wrong: a drop-when-full `default` with no metric, so shed load is invisible until
users complain.

Fix: increment an `atomic.Int64` (or a metrics hook) on the `default` branch, and
export it. You cannot alert on what you do not count.

### Producer blocked on a channel nobody drains

Wrong: a producer doing a *blocking* send into a bounded channel that a shutdown
path stopped draining — it hangs forever, leaking the goroutine.

Fix: make the producer's send non-blocking (drop/reject) or `ctx`-aware (`select`
over the send and `ctx.Done()`), so it exits when the consumer is gone.

### Forgetting ticker.Stop

Wrong: creating a `time.Ticker` per poll/loop invocation and never calling `Stop`,
leaking a runtime timer each time.

Fix: `defer t.Stop()` immediately after `time.NewTicker`.

### Treating closed-and-empty as not-ready

Wrong: in a try-receive, treating `ok == false` as "still open, retry later". A
closed channel makes the receive case *ready* every time, yielding `(zero, false)`;
a loop that retries on `ok == false` spins forever on a closed channel.

Fix: check the two-value receive and handle closed explicitly (stop, or switch to a
different source).

### Coalescing without evicting

Wrong: a plain non-blocking send into a full size-1 notify channel, which drops the
*new* value and keeps the stale one, so the consumer keeps seeing old state.

Fix: on a full slot, non-blocking receive to discard the stale value first, then
send the newest. Latest-wins requires evicting the old, not dropping the new.

### Draining with a blocking receive during shutdown

Wrong: `for v := range ch` on a channel whose producer already stopped but never
closed — the shutdown path blocks past its deadline waiting on a send that will
never come.

Fix: use a non-blocking `Drain` (a try-receive loop) or a `select` over the jobs
channel and `ctx.Done()`, bounded by the shutdown context.

Next: [01-trydefault-nonblocking-primitives.md](01-trydefault-nonblocking-primitives.md)
