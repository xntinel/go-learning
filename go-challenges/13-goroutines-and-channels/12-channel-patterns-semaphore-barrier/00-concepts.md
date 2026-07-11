# Channel Patterns: Semaphores, Barriers, and Bounded Concurrency — Concepts

Every real backend has a fan-out that must not overwhelm a downstream
dependency: parallel calls to a payment provider, concurrent DB writes during a
batch import, thumbnail generation, S3 uploads, cache warmups. Spawn one
goroutine per work item and you have written a load generator pointed at your
own database. The primitive that fixes this is the *semaphore*: it caps how many
units of work run their critical section at once, so you do not exhaust a
connection pool, blow past a provider's rate limit, or OOM by holding N large
buffers in flight simultaneously. Its sibling, the *barrier*, is a rendezvous:
participants block until the last one arrives, then all proceed together. That
one idea powers readiness gates (do not serve traffic until DB, cache, and
broker are all healthy), phased batch pipelines (every shard finishes extract
before any starts load), and graceful drain (prove zero work is in flight before
shutdown returns). This file is the conceptual foundation for the ten
independent exercises that follow; read it once and you have the model.

## Concepts

### A buffered channel is a counting semaphore

A buffered channel of capacity `n` *is* a counting semaphore. A send acquires a
slot and blocks when the buffer is full; a receive releases a slot. The element
type carries no information — you only care about occupancy — so `struct{}` is
the idiomatic token: it is zero bytes wide, so `chan struct{}` of capacity `n`
costs essentially the ring buffer and nothing per slot.

```go
type Semaphore chan struct{}

func (s Semaphore) Acquire() { s <- struct{}{} } // blocks when full
func (s Semaphore) Release() { <-s }             // frees one slot
```

The blocking `Acquire` is a *bare send*, `s <- struct{}{}`. That is the whole
mechanism; a `select` with only that one send case and no other case is
redundant. `TryAcquire` is a send with a `default` case, which makes it
non-blocking: it grabs a slot if one is free and otherwise reports failure
without waiting. `Release` is a receive. The single invariant you must never
violate is balance: release exactly as many times as you acquired. Releasing
more than you hold either blocks (draining an empty buffer) or, if you get the
count wrong the other way, silently lets too many goroutines run.

### A semaphore does not own goroutines

This is the distinction that trips up most engineers. A semaphore caps
*concurrent critical sections*, not *goroutine count*. If you spawn one
goroutine per item in a million-item batch and gate each with a semaphore of
size 8, you still created a million goroutines — a million stacks — and only
eight run their guarded section at a time. That bounds pressure on the
downstream dependency, but not the memory and scheduler cost of the goroutines
themselves. When the goroutine count is the resource you must bound, you need a
fixed *worker pool*: a small, constant set of goroutines draining a channel of
jobs. The pool also gives natural back-pressure — producers block on a full jobs
channel — whereas a semaphore in front of an unbounded queue lets the queue grow
without limit. Choosing between them is the subject of the final exercise.

### Request-scoped acquisition must be cancellable

The bare-send `Acquire` has a fatal flaw behind an HTTP handler. When the
downstream is slow and the semaphore stays full, a caller's goroutine parks on
the send indefinitely — long after the client has given up and closed the
connection. Under load that is a goroutine leak that ends in OOM. The only
correct form for request-scoped work selects on both the send and the caller's
context:

```go
func (s Semaphore) AcquireCtx(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

When the request's deadline fires or the client disconnects, `ctx.Done()`
unblocks, `AcquireCtx` returns `ctx.Err()` (`context.DeadlineExceeded` or
`context.Canceled`), and the goroutine unwinds instead of pinning a slot it will
never use. Any semaphore used inside a server must be the cancellable one.

### Weighted semaphores bound cost, not count

When jobs are heterogeneous — a 500 MB image transform and a 1 KB JSON
validation should not count the same — a counting semaphore is the wrong tool. A
*weighted* semaphore bounds total in-flight *weight* against a budget.
`golang.org/x/sync/semaphore.Weighted` is the standard implementation:
`Acquire(ctx, n)` blocks until `n` units of weight are free (or the context is
cancelled, in which case it returns `ctx.Err()` and acquires nothing);
`Release(n)` returns exactly `n` units. The balance rule is stricter here: if the
weight you release differs from the weight you acquired, you silently corrupt the
budget — over-release lets too many jobs run, under-release starves the
semaphore. Capture the weight in a variable and `defer sem.Release(w)` with that
same variable.

### errgroup.SetLimit is the idiomatic bounded fan-out

Most hand-rolled `semaphore + WaitGroup + error channel` fan-outs are reinventing
`golang.org/x/sync/errgroup`. `errgroup.WithContext` gives you a `Group` and a
derived context; `Group.SetLimit(n)` turns it into a bounded fan-out where at
most `n` of the `Go`-launched functions run concurrently — `SetLimit(n)` *is* a
semaphore. On top of that, the group captures the first non-nil error returned by
any function and cancels the derived context, so sibling goroutines that honor
`ctx.Done()` stop early instead of hammering a dependency that has already
failed. `TryGo` lets you attempt to add work without blocking when the group is
at its limit. `Wait` blocks for all launched functions and returns the first
error. This is the replacement to reach for before you write your own limiter.

### A barrier is a rendezvous built on close()

A barrier makes N participants wait until all N have arrived, then releases them
simultaneously. The broadcast mechanism is `close(ch)`: every goroutine blocked
on a receive from a closed channel unblocks at once, each receiving the zero
value. The arrival count must be guarded — with a mutex or an atomic — because
participants arrive concurrently and a lost increment means the barrier either
never fires or fires early. The last arrival closes the channel; everyone else is
already parked on the receive, so all proceed together.

```go
func (b *Barrier) Wait() <-chan struct{} {
	b.mu.Lock()
	b.count++
	if b.count == b.n {
		close(b.ch) // broadcast: releases all waiters at once
	}
	b.mu.Unlock()
	return b.ch
}
```

### close() is one-shot, so a simple barrier cannot be reused

A channel can be closed exactly once; a second `close` panics, and a closed
channel can never reopen. So the barrier above is one-shot: good for "all shards
finish phase A once", useless for a loop of phases. A *cyclic* barrier that
resets each round needs a *generation counter* and a *fresh channel per round*.
When the last participant of round K arrives, it closes round K's channel (the
broadcast), swaps in a new channel, and bumps the generation, all under the same
lock — so round K+1 waits on its own channel and round K's close is never
touched again.

### Barrier semantics power readiness and drain

The barrier idea reaches beyond synchronizing worker goroutines. A *readiness
gate* is a barrier over dependency probes: `/readyz` returns 503 until every
probe (DB ping, cache ping, broker ping) has succeeded once, and the last
success closes the "ready" channel — exactly barrier semantics, with `sync.Once`
guarding the close so concurrent probe completions cannot double-close and panic.
*Graceful drain* is the mirror image: `Shutdown` acquires *all* N semaphore slots
(a full-weight `Acquire`), which can only succeed once no task holds a slot, so a
successful acquire is a proof that zero work is in flight before the process
exits — a barrier that fences in-flight work behind a deadline.

### Back-pressure versus buffering

An unbounded work queue plus a semaphore still lets the queue grow without bound:
you are limiting concurrency, not intake, so memory can blow up under a burst. A
worker pool reading from a *bounded* channel applies back-pressure — a producer
that outruns the workers blocks on the send until a worker frees space. Choose
based on whether you may reject or drop work (a rate limiter can) or must
eventually accept all of it (then bound the buffer and let producers block). This
is a capacity-planning decision, not a stylistic one.

### Always pair Acquire with a deferred Release

A slot held forever is a slow-motion deadlock: available concurrency silently
degrades toward zero and the service stalls with no error. The defense is
lexical: `defer sem.Release()` on the line immediately after a successful
`Acquire`, so a panic or an early return cannot leak the slot. The same holds for
weighted release and for worker-pool slots.

### Prove the invariant with -race and a peak tracker

A limiter that "passes its tests" proves nothing unless the tests actually
measure concurrency under the race detector. The trustworthy proof is an atomic
peak-concurrency tracker: on entering the guarded section, increment a live
counter and bump a peak via a `CompareAndSwap` loop; on exit, decrement. Assert
the observed peak is `<= cap`. A functional test that never runs `-race` and
never measures the peak tells you the code returned the right answer once, not
that the cap held or that your counters did not race. Every exercise here runs
under `-race` and asserts a measured peak.

## Common Mistakes

### Forgetting to Release

Wrong: acquire a slot and return (or panic) without releasing. The semaphore
fills permanently and concurrency degrades to zero — a stall with no error to
point at.

Fix: `defer sem.Release()` on the line right after a successful `Acquire`, in the
same lexical scope.

### A blocking Acquire in request-scoped code

Wrong: use the bare-send `Acquire` inside an HTTP handler. When the queue is slow,
one goroutine parks per waiting caller long after the client timed out — a leak
that ends in OOM.

Fix: select on `ctx.Done()` and return `ctx.Err()`; use the cancellable
`AcquireCtx` for anything behind a request.

### Mismatched weighted Release

Wrong: `Release(n)` with `n` different from the acquired weight. Over-release lets
too many jobs run; under-release permanently shrinks the budget.

Fix: capture the exact acquired weight in a variable and `defer sem.Release(w)`
with that variable.

### Reusing a one-shot barrier

Wrong: call `Wait` again after the barrier's channel was closed. The second round
either returns immediately (channel already closed) or panics on a second
`close`.

Fix: use a cyclic barrier — a generation counter plus a fresh channel per round.

### The wrong barrier count

Wrong: construct a barrier for N participants when only N-1 ever arrive. The
barrier never releases and every participant deadlocks.

Fix: derive N from the actual participant set, never a hard-coded guess.

### Closing a semaphore channel

Wrong: `close` a `chan struct{}` used as a semaphore. Every later send panics
("send on closed channel") and every receive returns instantly, so the slot
invariant is gone.

Fix: never close a semaphore. Drop the reference and let the garbage collector
reclaim it.

### Assuming a semaphore bounds goroutine count

Wrong: gate a million-item batch with a size-8 semaphore and expect low memory.
You still spawned a million goroutines; only eight run the critical section at
once.

Fix: use a fixed worker pool when the goroutine count itself is the resource to
bound.

### Testing a limiter without -race or a peak tracker

Wrong: a green functional test with no `-race` and no concurrency measurement. It
says nothing about whether the cap held or whether the counters raced.

Fix: track the peak with an atomic `CompareAndSwap` loop, assert `peak <= cap`,
and always run `go test -race`.

### Ignoring the errgroup context

Wrong: use `errgroup.SetLimit` but write `Go` functions that never check
`ctx.Done()`, so a failed sibling does not stop the others and they keep hitting
a dead dependency.

Fix: derive the context with `errgroup.WithContext` and have every function honor
`ctx.Done()`.

### Double-close on a readiness or barrier broadcast

Wrong: two probes finish concurrently and both call `close(ready)`; the second
panics.

Fix: guard the close with `sync.Once`, or with a mutex-checked generation so
exactly one goroutine ever closes.

Next: [01-buffered-channel-semaphore.md](01-buffered-channel-semaphore.md)
