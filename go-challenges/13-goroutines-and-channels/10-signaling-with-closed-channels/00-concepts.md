# Signaling With Closed Channels: Broadcast, Shutdown, and Cancellation

Closing a channel is the only zero-cost, race-free fan-out broadcast Go gives
you. One `close(ch)` unblocks an unbounded number of receivers at once, with no
value to drain, no re-arming, and no per-receiver bookkeeping. Every production
shutdown and cancellation path a backend engineer owns is built on this one
primitive: draining an in-flight queue, broadcasting stop to a worker pool,
gating request handlers behind a readiness barrier, aborting the siblings of a
parallel fan-out on the first error, and bridging `context.Context` cancellation
into hand-rolled stop channels. The through-line is *ownership and lifecycle
correctness*: exactly one goroutine owns a channel's close, it closes exactly
once, and every consumer provably exits so nothing leaks under `-race`. This
file is the model behind all nine exercises that follow; read it once and the
rest are variations on the same mechanism.

## The broadcast asymmetry

A value send on a channel wakes *exactly one* receiver. If ten goroutines are
parked on `<-ch` and you do `ch <- v`, one of them gets `v` and the other nine
stay blocked. This is the defining property of a channel used as a queue, and it
is exactly the wrong property for a shutdown signal: you would have to send `N`
times to stop `N` workers, and you would have to know `N`.

Closing the channel inverts that. `close(ch)` transitions the channel into a
permanently-ready state that *every* current and future receiver observes. Ten
parked receivers all wake. A goroutine that starts receiving an hour later still
sees the channel closed and returns immediately. There is no value to hand out,
so there is no "one winner" — the readiness itself is the message. That
asymmetry is why `close` is *the* fan-out signal primitive and a value send is
not. When you catch yourself writing `stop <- true` to shut down more than one
goroutine, you have reached for the wrong tool.

## Receiving from a closed channel

A receive from a closed channel never blocks. In the single-value form,
`<-ch` returns the element type's zero value immediately, forever. In the
two-value form, `v, ok := <-ch` returns `(zero, false)` — the `ok == false` is
how you distinguish "channel closed" from "received a real zero value". Because
a receive on a closed channel is always runnable, a `select` case that receives
from a closed channel is *always* selectable. That is the entire trick behind
the always-ready "done" arm:

```go
select {
case <-stop: // once stop is closed, this case is permanently ready
	return
case v := <-work:
	handle(v)
}
```

Before `stop` is closed, that case blocks and the `select` waits on `work`.
After `close(stop)`, the case is always ready, so the next iteration returns.

## Use `chan struct{}`, not `chan bool`

A signaling channel should be `chan struct{}`. `struct{}` is a zero-width type:
it occupies no memory and carries no data. It says, precisely, "this channel
transmits an event, not a value". The receiver neither needs nor should inspect
what came across — the *close* is the whole signal. Using `chan bool` invites a
category error: someone writes `stop <- false` thinking it means "keep going",
but a `false` is still a real value that wakes exactly one receiver and must be
drained, so it neither broadcasts nor communicates what the sender intended.
`chan struct{}` closed exactly once removes the temptation.

## The ownership rule: one owner, one close

The single hardest rule to internalize, because every violation is a panic:

- Sending on a closed channel panics.
- Closing an already-closed channel panics.
- Closing a `nil` channel blocks forever.

From those three facts follows the discipline. Exactly one goroutine owns a
channel's close, and it closes exactly once. Closing a channel you *receive*
from is a bug: the owner will panic on its next send. When multiple shutdown
paths (a `defer`, a signal handler, an error branch) might all race to close the
same channel, guard the close with `sync.Once`, or gate it behind a single
`stopped` flag under a mutex. `sync.Once.Do(func(){ close(ch) })` is the
canonical idempotent close: the first caller closes, every later caller is a
no-op, and it is safe under concurrency.

## A closed channel is a one-shot latch

Closing is irreversible. There is no "reopen". A closed channel is a one-shot
latch: it flips from "blocking" to "permanently ready" and stays there. That is
exactly what you want for shutdown and readiness — a thing that happens once and
stays happened. It is exactly wrong for a *reusable* event. If you need to signal
"go" repeatedly across generations, you cannot close-and-recreate a channel that
other goroutines still hold a reference to (they keep reading the old, closed
one and spin). Reusable, edge-triggered notification needs a different tool: a
fresh channel per generation, or `sync.Cond`. Reach for `close` when the event
is terminal; reach for `sync.Cond` when it repeats.

## The done/quit pattern: request versus acknowledge

Robust shutdown needs *two* channels with *two* owners, because "shutdown
requested" and "shutdown completed" are different facts. The caller owns and
closes `stop` to *request* shutdown. The worker owns and closes `done` (or calls
`wg.Done()`) to *acknowledge* it has actually exited. A `Stop()` method that
merely closes `stop` and returns is lying: the caller proceeds while work is
still in flight, which loses writes, leaks goroutines, and flakes tests. A
correct `Stop()` closes `stop` and then blocks on `<-done` (or `wg.Wait()`),
turning "requested" into "completed". That block is what makes shutdown
deterministic and leak-free.

## `context.Context` is the same mechanism

`context.Context.Done()` returns a `<-chan struct{}` that is *exactly* this
closed-channel broadcast. Cancelling a context closes its done channel, and that
close propagates to every derived context by closing theirs. Hand-rolled stop
channels and context cancellation are not two different mechanisms — they are the
same one, and you can bridge freely between them. Two helpers make the bridge
clean: `context.AfterFunc(ctx, f)` (Go 1.21) runs `f` in its own goroutine when
`ctx` is cancelled and returns a `stop func() bool` to unregister it, which lets
you close a derived stop channel exactly when a context is cancelled;
`context.WithCancelCause(parent)` (Go 1.20) gives you a `cancel(err)` that
records *why* it was cancelled, retrievable with `context.Cause(ctx)`, so a
first-error fan-out can attribute the abort.

## Drain versus cancel are different closes

Two closes with the same syntax mean opposite things, and conflating them is a
classic bug. Closing the *work* channel means "no more items — finish what
remains": the consumer ranges to completion and processes every buffered item.
Closing a *stop* channel means "abandon in-flight — return now": the consumer
discards what is queued and exits immediately. A graceful shutdown drains; a
forced shutdown cancels. The failure mode of confusing them is a deadlock: a
consumer that received "stop" but then tries to drain an unbuffered `work`
channel whose producer has already exited will block forever on a receive that
never completes. On stop, return; never block on a producer that may be gone.

## Fair selection and observing the stop arm

When more than one case of a `select` is ready, Go picks one *pseudo-randomly* —
it does not prefer the first. That fairness is usually what you want, but it has
a consequence: if a `work` channel is red-hot and `stop` is closed, each
iteration has a coin-flip chance of taking `work` again, so observation of the
stop arm can be delayed across iterations. It is never *starved* forever (the
close is permanent, so a future iteration eventually picks it), but if you need
prompt shutdown, structure the loop so the stop arm is re-evaluated every
iteration and does not depend on `work` going idle. Never write a loop whose only
exit is a stop case nested inside a branch that only runs when work is absent.

## Proof of termination is not optional

Every close-to-signal design must be paired with a *proof* that receivers
actually exit. The reliable contract is `WaitGroup` completion: each goroutine
`defer wg.Done()`, and `wg.Wait()` returning is a hard guarantee that all of
them ran to the end. `runtime.NumGoroutine()` is a *flaky secondary* check, not
a contract: the scheduler reclaims exited goroutines asynchronously, so the count
lags and a naive `NumGoroutine() == baseline` assertion right after `Shutdown()`
races the runtime. Use `WaitGroup` as the primary signal and, if you must poll
`NumGoroutine`, do it in a bounded retry/settle loop. Run all of it under
`-race`, which is what catches an unsynchronized close and a send-on-closed that
a lucky scheduler would otherwise hide.

## Common Mistakes

### Sending a sentinel value to signal shutdown

Wrong: `stop <- true` to stop a pool. Only one receiver wakes, so a pool of `N`
workers has `N-1` goroutines that never see the signal and leak.

Fix: `close(stop)`. The close broadcasts to every receiver at once, and you do
not need to know `N`.

### Closing the channel from a receiver

Wrong: a consumer calls `close(stop)` on a channel the owner still sends on. The
owner panics on its next send-on-closed.

Fix: only the owner closes; receivers only read. Split into `stop` (owned by the
caller) and `done` (owned by the worker) so each side closes only what it owns.

### Double-close panic from multiple shutdown paths

Wrong: a `defer close(ch)`, a signal handler, and an error branch all call
`close(ch)`. The second one panics.

Fix: guard the close with `sync.Once`, or a single `stopped` flag under a mutex.
The first path closes; the rest are no-ops.

### Reusing a closed channel as a repeatable event

Wrong: `close(ch)` to signal, then expecting to signal again later — or
close-and-recreate a channel other goroutines still hold a pointer to.

Fix: a closed channel is one-shot. Use a fresh channel per generation, or
`sync.Cond` for repeated edge-triggered notification.

### `Stop()` that does not wait for the goroutine

Wrong: `Stop()` closes `stop` and returns immediately. The caller proceeds while
work is still in flight — lost writes, leaked goroutines, flaky tests.

Fix: block on `<-done` (or `wg.Wait()`) before returning, so `Stop()` means
"has actually stopped", not "has been asked to".

### `chan bool` that forces the receiver to interpret a value

Wrong: `stop chan bool`, and a `stop <- false` that a receiver must read and
branch on. A `false` is still a value that wakes exactly one receiver.

Fix: `chan struct{}` where the close *is* the signal and there is nothing to
interpret.

### A stopped consumer that deadlocks draining a dead producer

Wrong: on receiving stop, the consumer loops back to `<-work`, but the producer
has already exited, so the unbuffered receive blocks forever.

Fix: on stop, return immediately. Never block on a producer that may be gone.

### `runtime.NumGoroutine` as the sole leak assertion

Wrong: `if runtime.NumGoroutine() != baseline { t.Fatal("leak") }` right after
`Shutdown()`. Goroutine teardown is asynchronous, so this races and flakes.

Fix: assert via `WaitGroup` completion as the contract; use `NumGoroutine` only
inside a bounded retry loop as a secondary guard.

### Forgetting to `Stop()` a timer after an early flush

Wrong: a batcher flushes on a size threshold but leaves its `time.AfterFunc`
timer armed, so the deadline later fires a spurious second flush.

Fix: honor the timer's `Stop()` return value and gate the flush path so a
size-triggered flush cancels the pending deadline flush.

### A long-lived goroutine whose only exit never closes

Wrong: a goroutine whose sole exit is a `select` arm on a channel nobody ever
closes (the context is never cancelled, no stop is wired). It runs forever.

Fix: ensure every long-lived goroutine has a guaranteed-closed termination arm —
`ctx.Done()`, a `stop` channel someone owns, or both.

Next: [01-graceful-shutdown-service.md](01-graceful-shutdown-service.md)
