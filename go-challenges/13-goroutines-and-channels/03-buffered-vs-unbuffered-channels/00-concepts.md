# Buffered vs Unbuffered Channels: Capacity, Backpressure, and Bounded Concurrency

Choosing `make(chan T)` versus `make(chan T, n)` looks like a micro-decision and
is actually a flow-control decision — the kind senior backend work is made of. It
is the difference between an ingest endpoint that sheds load and returns 503 under
a spike and one that unbounded-queues itself into an OOM and a latency cliff. This
file is the model you need to reason through the nine exercises that follow: what a
buffer physically is, why an unbuffered channel is a synchronization barrier rather
than a zero-length queue, how capacity behaves as a backpressure knob with a real
cost, and the close/drain protocol that makes graceful shutdown possible. Read it
once; each exercise then stands alone.

## Concepts

### What a buffer physically is

`make(chan T, n)` allocates an internal ring buffer of capacity `n` alongside the
channel's send/receive wait queues. A send blocks *only* when the buffer is full
and a receiver is not immediately ready; a receive blocks *only* when the buffer is
empty and a sender is not immediately ready. `cap(ch)` is fixed at creation and
never changes. `len(ch)` is the current number of elements sitting in the buffer —
a live gauge that rises as producers outrun consumers and falls as consumers catch
up. For an unbuffered channel `cap` is 0 and `len` is always 0; there is nowhere to
store a value.

### The unbuffered channel is a barrier, not a queue

This is the point people miss. An unbuffered channel (`cap` 0) is a
*synchronization point*: a send and a receive must rendezvous — both goroutines
meet, the value is handed across, and both proceed. Neither can make progress
without the other. The Go memory model states the send on an unbuffered channel
happens-before the corresponding receive *completes*, and — the direction people
forget — the receive happens-before the send *completes*. That two-way ordering is
a barrier: after the handoff, the sender knows the receiver has taken the value.
You reach for unbuffered channels precisely when you want that guarantee — a signal
that the other side has reached a known point — not merely to move data. Treating
an unbuffered channel as "a buffered one with cap 0" throws away the only reason it
exists.

### A buffered channel trades the handoff for throughput

A buffered channel decouples sender and receiver up to `cap`. It converts
synchronization into queuing: the sender deposits a value and walks away while the
receiver is still busy, as long as the buffer is not full. The memory model
guarantee weakens accordingly — the send of a *particular* value still
happens-before the receive of *that* value, but there is no lockstep between the
two goroutines and no guarantee the receiver has caught up. You gain burst
absorption and throughput; you lose the barrier. Choose a buffer when you want to
smooth a transient burst, not when you need to know the other side has arrived.

### Capacity is a backpressure knob with a cost

Capacity is not a performance dial you turn up for free. Three regimes matter.
Capacity 0 gives strict flow control: the producer moves in lockstep with the
consumer, which is the strongest backpressure there is. Capacity equal to the
expected burst absorbs a spike so a momentarily-slow consumer does not stall the
producer, then returns to empty at steady state. Oversized capacity
(`make(chan T, 1_000_000)`) pins large memory, lets work pile up invisibly, and
*adds* latency because items sit in the queue longer before they are served — with
no gain in correctness or steady-state throughput. At steady state throughput is
bounded by the consumer; the buffer only smooths transients. A permanently full
buffer means the producer is blocking again — the buffer has become invisible, and
all you bought was a memory bill and a latency tax.

### Buffering never fixes a slow consumer

Repeat it because it is the most expensive mistake: buffering does not make a slow
consumer faster. If the consumer's steady-state rate is below the producer's, the
buffer fills, the producer blocks, and you are back to lockstep — except now with
`cap` items of latency baked in. The only real fixes are a faster consumer, more
consumers (a wider pool), or deliberately shedding load. A bigger buffer just
delays the moment you notice.

### Non-blocking operations turn a block into a decision

`select { case ch <- v: default: }` turns a would-be block into a branch: if the
send cannot proceed immediately, take the `default` and *decide* — shed the load,
return 503, try again later. This is how a backend rejects under overload instead
of unbounded-queuing itself into memory exhaustion. The same shape gives you a
try-acquire on a semaphore (`select { case sem <- token: default: }`) and a poll on
a result channel. Non-blocking ops are the mechanism behind every load-shedding and
try-lock pattern in this lesson.

### chan struct{} as a counting semaphore

A buffered `chan struct{}` of size N is the idiomatic concurrency limiter: send a
token to acquire (the send blocks once N tokens are outstanding, i.e. the buffer is
full), receive a token to release. `struct{}` is zero-width so the buffer costs
almost nothing; the capacity *is* the concurrency limit. This bounds a fan-out of
outbound HTTP or DB calls to at most N in flight. When you need context
cancellation, weighted acquisition, or a library-grade API, the equivalents are
`golang.org/x/sync/semaphore.Weighted` (`Acquire(ctx, n)`, `TryAcquire`, `Release`)
and `errgroup.Group.SetLimit(n)`.

### The close protocol and why it enables graceful drain

Closing is a *broadcast*: `close(ch)` unblocks every receiver at once. A `range`
over the channel ends; `v, ok := <-ch` yields `ok == false` after the last value.
The rules that keep this safe: only the *sole sender* closes (a receiver must never
close, or the next send panics with "send on closed channel"), and a channel is
closed exactly once (a double close panics). For fan-in from N workers, no single
worker owns the channel, so a dedicated closer goroutine does `wg.Wait()` then
`close(results)`. The crucial property for shutdown: a *buffered* channel can be
closed with elements still in the buffer, and receivers drain those remaining
elements before `ok` becomes false. That is exactly what lets a graceful-shutdown
path stop accepting new work, close the input, and let workers drain the already-
buffered jobs — nothing accepted is dropped. An unbuffered channel has nothing
queued, so there is nothing to drain; the choice of buffered-vs-unbuffered decides
whether in-flight work survives a shutdown.

### len/cap are metrics, never control flow

`len(ch)` and `cap(ch)` are legitimate, cheap observability signals: `len(ch)/cap(ch)`
is a saturation gauge an SRE scrapes to see how close a queue is to full, and an
autoscaler can act on. They are racy snapshots, though — the value is stale by the
next instruction. Feeding them to a dashboard is fine (a slightly-old number is
still informative). Using them as a control-flow predicate is a bug: `if len(ch)==0`
or `if len(ch)<cap(ch)` before a send/receive races the very interleaving it is
trying to guard, causing lost sends or spurious blocks. The rule: `len`/`cap` for
metrics only; use the blocking op or `select`+`default` for control flow.

### The production pipeline is the errgroup version

A raw channel pipeline — `Generate` → `Square` → `Filter` — is a fine teaching
artifact and silently drops two things a production service cannot drop: errors and
cancellation. A stage that fails has no way to report it; a consumer that gives up
leaves upstream producers blocked forever on an unread channel. The production
evolution is `errgroup.WithContext` plus `SetLimit(N)`: every stage returns an
error, the first failure cancels the derived context so downstream stages stop
early, `SetLimit` bounds the fan-out, and buffered stage channels still bound
memory. That is the version a senior actually ships. The naive raw-channel pipeline
is where you start; the errgroup pipeline is where you land.

## Common Mistakes

### Using a buffer to hide a synchronization bug

Wrong: a buffered send that "works" only because the buffer is not yet full; the
race resurfaces the instant traffic fills the buffer and the send starts blocking.

Fix: choose unbuffered when you actually need the happens-before handoff, and size
buffers to a justified burst, not to silence a symptom you have not diagnosed.

### Closing from the receiver, or closing twice

Wrong: a receiver closing the channel (the next send panics "send on closed
channel"), or two goroutines both closing it (double close panics).

Fix: the single sender owns the close. For fan-in, one closer goroutine does
`wg.Wait()` then `close(results)` — exactly once, from one place.

### Oversizing the buffer to "be safe"

Wrong: `make(chan T, 1_000_000)` on the theory that bigger is safer. It pins large
memory, adds latency by letting work pile up unseen, and hides that the consumer
cannot keep up.

Fix: size to the expected burst and expose `len`/`cap` as a saturation metric so you
*see* the queue depth instead of padding it.

### Treating a buffered channel as unbounded

Wrong: a huge or effectively-unbounded buffer plus a slow consumer — a memory leak
and OOM waiting under load.

Fix: bound the buffer and, when it is full, either block deliberately or shed with
`select`+`default`. A bounded queue that rejects is healthier than an unbounded one
that dies.

### Using len(ch) as a guard before a send or receive

Wrong: `if len(ch) < cap(ch) { ch <- v }` — the value is stale by the next
instruction, so the send can still block or a concurrent sender can fill the slot.

Fix: use the blocking send/receive, or `select`+`default` for a non-blocking try.
Reserve `len`/`cap` for metrics.

### Leaking the producer when the consumer exits early

Wrong: the consumer stops ranging (an error, a cancel) while the producer keeps
sending into a now-unread channel — the producer blocks forever.

Fix: pass a `context` and `select` on `ctx.Done()` in the producer, or use errgroup
so the first error cancels the whole pipeline and unblocks everyone.

### Forgetting to close results in a worker pool

Wrong: workers write to `results` but nobody closes it, so the fan-in `range results`
never terminates and the program deadlocks.

Fix: exactly one closer goroutine that does `wg.Wait()` then `close(results)`. A
`go test` timeout catches the hang in CI so it never reaches production.

### Assuming a buffered channel is a barrier

Wrong: relying on a buffered channel for lockstep progress or a cross-goroutine
barrier; it only orders each value's own send-before-receive.

Fix: when you need a barrier or lockstep, use an unbuffered channel or explicit
synchronization (`sync.WaitGroup`, a `sync.Cond`), not buffer capacity.

Next: [01-bounded-stage-pipeline.md](01-bounded-stage-pipeline.md)
