# The Done Channel Pattern: Cancellation and Shutdown Without Context

Every production Go service eventually has to answer the same question: how does a
goroutine that is busy — reading a channel, forwarding values, running a ticker —
learn that it should stop? The answer, underneath almost everything, is a single
idiom: a `done <-chan struct{}` that the owner closes to broadcast "stop now" to
every goroutine watching it. Fan-out worker pools, fan-in merges, streaming
pipelines, pub/sub broadcasts, and graceful-shutdown paths are all built from this
one primitive. `context.Context` is not an alternative to it — `ctx.Done()`
returns exactly a `<-chan struct{}`, and `Context` is the done-channel pattern with
deadline propagation, value passing, and a cancellation reason bolted on. A senior
backend engineer has to be fluent in the raw idiom, because library-internal
cancellation frequently uses a bare done channel where threading a full `Context`
is overkill, and because the number-one source of goroutine leaks in production Go
is a producer blocked forever on a send to a consumer that walked away. The fix for
that leak is `select { case out <- v: case <-done: return }`, and you must feel it
in your bones.

## Concepts

### A done channel is a signal, not a value carrier

Use `chan struct{}`. The element type `struct{}` is zero-width: it carries no data,
allocates nothing per element, and makes the intent explicit — this channel
communicates *timing*, not *values*. You never send on a done channel. You signal
by **closing** it. Closing and sending are fundamentally different broadcast
mechanisms. A send on a channel is received by exactly one receiver; if you have N
workers all selecting on `done` and you signal with `done <- struct{}{}`, only one
worker wakes and the other N-1 keep running. Closing is a broadcast: every current
receiver and every future receiver observes the close. That difference is the
entire reason the pattern works for fan-out cancellation.

### A closed channel is always ready and yields the zero value

This is the mechanism that makes one `close(done)` wake N goroutines. A receive on
a closed channel never blocks: it returns the element type's zero value
immediately, and it does so repeatedly, forever. So the instant `done` is closed,
`case <-done:` inside a `select` becomes permanently selectable in every goroutine
that reaches that select. There is no queue to drain, no value to deliver once —
the closed state is a latch that every observer sees. That is why a single close
unblocks an unbounded number of receivers, and why you must never rely on a done
channel to carry information beyond the fact of its own closure.

### The receive-only type is a contract about who may cancel

Writing `done <-chan struct{}` in a function signature is not a formality — it is an
enforced contract. A receive-only channel cannot be closed (the compiler rejects
`close` on a `<-chan`), so a function that accepts `done <-chan struct{}` may only
*observe* cancellation, never *trigger* it. The owner — the producer or the
coordinator that created the channel — holds the bidirectional `chan struct{}` and
is the sole closer. This division prevents the two classic bugs of the pattern: a
consumer accidentally closing a channel it does not own, and two goroutines racing
to close the same channel (the second close panics). Give consumers the receive-only
type; keep the bidirectional handle with the one goroutine responsible for closing.

### The leak-avoidance rule: select every send against done

This is the single most important idiom in the lesson. A goroutine that *sends* to
a channel must select that send against `done`:

```go
select {
case out <- v:
case <-done:
	return
}
```

Consider the alternative — a bare `out <- v`. If the downstream consumer stops
reading (it found what it needed, it hit an error, its own context was cancelled),
`out` has no receiver, the send blocks forever, and the sending goroutine is leaked:
it lives until the process dies, holding whatever it had captured. Every fan-in
forwarder, every tee subscriber, every pipeline stage that produces output must
guard its send with a select on done. This is not a stylistic nicety; it is the
difference between a service that reclaims goroutines on cancellation and one that
slowly accumulates them until it exhausts memory.

### select chooses uniformly at random among ready cases

When more than one case of a `select` is ready in the same evaluation, Go picks one
uniformly at random. This has a sharp consequence: if both `done` and `work` are
ready in the same iteration, you cannot assume `done` wins. A loop that must treat
cancellation as strictly higher priority than work needs an explicit re-check —
before doing work, run a non-blocking `select { case <-done: return; default: }` so
a closed done preempts. Most worker loops do not need strict priority (processing a
few extra already-queued items during shutdown is fine), but when correctness
depends on stopping *before* the next unit of work, the random choice is a trap you
must design around.

### Ownership and close order in pipelines

Each pipeline stage owns and closes *its own output* channel, and closes it when its
input is exhausted or `done` fires. A stage never closes its input — that belongs to
the upstream stage. Fan-in is the subtle case: the merged output channel is written
by N forwarder goroutines, so no single forwarder may close it. The merge closes the
output only after a `sync.WaitGroup` confirms every forwarder has returned. Closing
the merged channel while a forwarder is still live races an in-flight send and panics
with "send on closed channel". The rule is mechanical: the last writer, and only the
last writer, closes; when there are many writers, a WaitGroup identifies "last".

### done versus context.Context

`ctx.Done()` returns a `<-chan struct{}` that is closed when the context is cancelled
or its deadline passes. That is the entire connection: `Context` *is* the done-channel
pattern, plus a propagating deadline, a `Value` bag, and an `Err()` that reports *why*
cancellation happened (`context.Canceled` vs `context.DeadlineExceeded`). Reach for a
bare done channel for library-internal, reason-less cancellation — a pipeline stage, a
channel merge, a tee — where a full `Context` is awkward or unnecessary. Reach for
`Context` at API boundaries, across process/RPC calls, and whenever a deadline or a
cancellation *cause* matters. Seeing these as the same primitive is what lets you
bridge old done-based code to a context-threaded codebase (the final exercise), rather
than treating them as rival mechanisms.

### Buffering a result channel with cap 1

When a goroutine computes a single result and delivers it on a channel, buffering that
channel with capacity 1 — `make(chan T, 1)` — lets the goroutine send its result and
exit even if the caller never reads it. With an unbuffered result channel, the
goroutine's send blocks until the caller receives, so the goroutine's lifetime is
coupled to the caller's attention; if the caller abandons the result, the goroutine
leaks. The cap-1 buffer decouples them: the send always succeeds into the buffer, the
goroutine returns, and the buffered value is either read later or garbage-collected
with the channel. This is a deliberate, well-known leak-avoidance trade-off for the
"spawn a goroutine, get one result" shape.

### Graceful shutdown is three ordered steps

The correct SIGTERM path is: (1) close `done` to stop intake and signal every worker,
(2) `Wait` on a `sync.WaitGroup` for in-flight work to drain, (3) *bound* that wait
with a timeout so one stuck worker cannot hang the process forever. Step three is a
`select` between a channel that closes when `wg.Wait()` returns and a `time.After`
budget. Guard the `close(done)` with `sync.Once` so calling `Shutdown` twice — which
happens when both a signal handler and a defer trigger it — does not panic on a double
close. Skipping the timeout is the classic mistake: `wg.Wait()` with no bound turns one
misbehaving worker into a hung deployment.

### time.Ticker must be stopped when its loop exits

A `time.Ticker` runs on a runtime timer. When the loop consuming `ticker.C` exits — on
`done` — you must call `ticker.Stop()`. `Stop` does not close `ticker.C`; it halts
delivery and lets the runtime reclaim the timer. Forgetting `Stop` leaves the timer
firing into a channel nobody reads until garbage collection eventually notices, which
can keep resources alive longer than intended. `defer ticker.Stop()` right after
`time.NewTicker` is the habit; the ticker exercise makes this concrete.

### Non-blocking send implements backpressure

Adding a `default` case turns a send into load-shedding:

```go
select {
case ch <- v:
	// delivered
case <-done:
	return
default:
	// dropped: consumer is not keeping up
}
```

A hot-path producer — a metrics emitter, a telemetry hook — that cannot afford to
block on a slow consumer uses this to *drop* rather than stall, while still honoring
cancellation. The three outcomes (delivered, cancelled, dropped) are exactly what a
lossy-but-live pipeline needs. The contrast is the naive `ch <- v`, which blocks the
hot path the moment the consumer falls behind.

## Common Mistakes

### Signaling by sending instead of closing

Wrong: `done <- struct{}{}` to stop workers. A send wakes exactly one receiver, so in
a fan-out with N workers only one stops and the rest run on.

Fix: `close(done)`. Closing is a broadcast — every current and future receiver of
`done` observes it, so one close stops all N.

### Closing done from a receiver, or closing it twice

Wrong: a consumer closes the `done` it was handed, or two goroutines both close it. A
second close panics with "close of closed channel".

Fix: only the owner closes, exactly once. Give consumers a `<-chan struct{}` parameter
(which cannot be closed) and guard the owner's close with `sync.Once`.

### The classic producer leak: an unguarded send

Wrong: `out <- v` in a forwarder or stage. When the downstream consumer abandons the
stream, the send blocks forever and the goroutine leaks.

Fix: `select { case out <- v: case <-done: return }`. Every send in a fan-in, tee, or
pipeline stage needs this guard.

### Closing a merged output before all forwarders finish

Wrong: closing the fan-in output channel as soon as the inputs are drained, while a
forwarder may still be mid-send. This races the send and panics with "send on closed
channel".

Fix: close the merged channel only after a `sync.WaitGroup` confirms every forwarder
has returned.

### Assuming done wins a ready select

Wrong: relying on `case <-done:` to be chosen when both it and the work case are ready.
`select` picks uniformly at random, so work may be processed after cancellation.

Fix: if cancellation must preempt, re-check done non-blockingly before processing:
`select { case <-done: return; default: }`.

### Using chan bool or chan int for a pure signal

Wrong: `chan bool` with `done <- true`, which forces a meaningless value and invites
send-versus-close confusion.

Fix: `chan struct{}` closed to signal. Zero width, unambiguous intent, no value to get
wrong.

### Forgetting ticker.Stop when the poller exits

Wrong: returning from the ticker loop on `done` without calling `ticker.Stop()`. The
underlying timer keeps firing until GC reclaims it.

Fix: `defer ticker.Stop()` after `time.NewTicker`, or `ticker.Stop()` on the done
branch.

### Handling only one of the two exit paths

Wrong: a worker loop that selects on `done` but ignores the work-channel-closed case
(or vice versa). It either never finishes normal work or never cancels.

Fix: handle both — `v, ok := <-work; if !ok { return }` for normal completion and
`case <-done:` for cancellation.

### An unbounded Wait in Shutdown

Wrong: `wg.Wait()` in `Shutdown` with no timeout. One stuck worker hangs the entire
process on SIGTERM.

Fix: bound the drain — `select` between a channel closed after `wg.Wait()` and a
`time.After(budget)`, returning a timeout error if the budget is exceeded.

### Treating done and context.Done() as different mechanisms

Wrong: building parallel cancellation plumbing because context and done channels feel
unrelated. They are the same primitive; `ctx.Done()` returns a `<-chan struct{}`.

Fix: bridge them explicitly (return `ctx.Done()` to done-based code; cancel a context
from a bare done) instead of duplicating the machinery.

Next: [01-cancellable-worker.md](01-cancellable-worker.md)
