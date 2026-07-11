# Channel Basics: Send, Receive, Close, and Unbuffered Synchronization — Concepts

Channels are the load-bearing wall of almost every Go backend service. A
request/reply command loop that serializes access to shared state without a
mutex, a fan-out/fan-in worker pool sitting behind an HTTP handler, a streaming
database cursor, an in-process pipeline, a graceful-shutdown handoff — all of
them are channels underneath. The senior skill is not "how do I send on a
channel"; it is reasoning about the *guarantees* a channel gives you and the
*failure modes* that take services down. An unbuffered channel gives you a
happens-before edge from the Go memory model — a send completes only after the
receiver has taken the value — which is simultaneously synchronization and
natural backpressure. Getting that wrong produces the three classic production
incidents: a send-on-closed panic during shutdown, a receive-with-no-sender
deadlock that hangs a handler until the request times out, and lost work when a
channel is closed before it is fully drained. This file is the conceptual
foundation. Read it once and you have what you need for every one of the
independent exercises that follow.

## Concepts

### A channel is a typed, reference-typed conduit

A channel is created with `make(chan T)`; its element type `T` is fixed at
creation and enforced by the compiler, so a `chan int` can never carry a string.
A channel is a *reference type*: copying a `chan T` value copies a handle to the
same underlying conduit, which is why passing a channel to a goroutine shares one
conduit rather than duplicating it. A channel variable that is declared but never
`make`d is `nil`. A nil channel is a legal value, but every send and receive on
it blocks forever — sometimes used deliberately inside a `select` to disable a
case, but in straight-line code it is almost always a bug (a struct field someone
forgot to initialize with `make`).

### Unbuffered channels rendezvous

An unbuffered channel has no capacity. A send `ch <- v` does not complete until
some other goroutine has executed the matching receive `<-ch`, and a receive does
not complete until some goroutine sends. The two goroutines meet at a single
point in time — a rendezvous. This is two things at once. It is *synchronization*:
after the send returns you know the receiver has the value. And it is
*backpressure*: a slow consumer throttles a fast producer directly, with no
unbounded buffer growing in memory to absorb the mismatch. When you need a
producer to be paced by its consumer — a durable enqueue that must not outrun the
persister, a handoff that must not lose work — unbuffered is the correct default.

### The memory model: send happens-before receive

The Go memory model states that the completion of a send on a channel
happens-before the completion of the corresponding receive completes. This is the
formal reason you can use a channel instead of a lock. Anything a goroutine wrote
to memory *before* it sent a value is guaranteed visible to the goroutine that
receives that value, without any additional synchronization. A worker that fills
in a struct and then sends it over a channel has published that struct safely; the
receiver reads a fully-constructed value. This edge is what makes "share memory by
communicating" a correctness guarantee and not just a slogan.

### close signals "no more values", as a broadcast

`close(ch)` records that no further values will ever be sent. It is a broadcast:
every current and future receiver observes it. Receiving from a closed channel
returns immediately with the element's zero value and a second boolean result of
`false` — the comma-ok form `v, ok := <-ch`. A `for v := range ch` loop reads
values until the channel is closed and drained, then terminates cleanly. Close is
how a producer tells a ranging consumer "you can stop now"; without it, the
consumer's range blocks forever waiting for a value that will never arrive, and
that goroutine leaks.

### Closing does not discard already-sent values

This is the property graceful shutdown depends on. Closing a channel does not
throw away values that were already sent into it (buffered, or in flight to a
receiver). Receivers keep getting every already-sent value, each with `ok == true`,
until the channel is empty; only *then* does a receive observe the closed state
with `ok == false`. So the correct drain-on-shutdown is: close the input channel,
then keep receiving until the channel reports closed-and-empty. The tail of queued
work is delivered, not dropped.

### Channel ownership discipline

Exactly one goroutine should own a channel's send side and be the one — the only
one, exactly once — that closes it. Two rules follow and they eliminate an entire
class of panics: do not close a channel from the receiver side, and do not close a
channel that any producer might still write to. When there are multiple producers,
they coordinate with a `sync.WaitGroup`, and a single separate goroutine closes the
channel after `wg.Wait()` returns. Ownership is the design principle that makes
close-related panics impossible rather than merely unlikely.

### Request/reply: serialize state without a mutex

The command-channel (or actor) pattern puts a piece of shared state inside a single
goroutine and lets nobody else touch it. Callers send a command struct that carries
its own private reply channel; the owning goroutine processes commands one at a
time and answers on each reply channel. Because only the owner ever reads or writes
the state, there is no data race and no mutex — the serialization comes from the
loop processing one command at a time. This trades lock contention for a serialized
loop, and it is the idiomatic realization of "do not communicate by sharing memory;
share memory by communicating". A monotonic ID generator, an in-memory sequence for
idempotency keys, a connection registry — all fit this shape.

### Fan-out and fan-in

Fan-out sends work to several worker goroutines that all receive from the same
channel; the runtime hands each sent value to one ready receiver, distributing the
work. Fan-in funnels several producer goroutines' output onto one shared channel.
The fan-in owner problem is always the same: who closes the shared output, and
when? The answer is the ownership discipline — a `WaitGroup` counts the producers,
and one goroutine calls `close(out)` after `wg.Wait()`, so the output is closed
exactly once, only after every producer has finished sending.

### Pipelines compose because each stage closes its output

A pipeline stage reads from an input channel until that input is closed, then
closes its own output channel — which propagates the end-of-stream signal to the
next stage down the line. That single contract (range the input, close the output
when the input closes) is what lets you snap stages together like pipe segments.
Directional channel types make the contract explicit and compiler-checked: a
`<-chan T` parameter may only be received from, a `chan<- T` may only be sent to,
so a stage cannot accidentally close its input or send upstream.

### Fan-out loses input order

Independent workers finish in nondeterministic order, so results arriving on a
fan-in channel are not in the order the inputs were submitted. A batch API contract
usually forbids that — response `i` must correspond to request `i`. To preserve
order you carry an index alongside each value through the pipeline and reassemble
into a pre-sized slice at `out[result.Index]`, rather than appending in arrival
order. This is a deliberate, explicit step; fan-out gives you throughput, and the
index gives back the ordering it took away.

## Common Mistakes

### Forgetting to close a channel a consumer is ranging over

Wrong: a producer finishes sending but never calls `close`. The consumer's
`for v := range ch` never terminates; that goroutine blocks forever waiting for a
value that will never come, and leaks. Fix: the goroutine that owns the send side
closes the channel when it is done producing.

### Sending on a closed channel

Wrong: a publisher writes `ch <- v` after another path has already called
`close(ch)`. This panics with "send on closed channel", and it is a common
shutdown race. Fix: apply the single-owner-closes discipline so no producer can
outlive the close; where a service genuinely must accept sends racing a shutdown,
guard them behind a `closed` flag under a mutex and return an error instead.

### Closing twice, or closing a nil channel

Wrong: two shutdown paths both call `close(ch)`, or code closes a channel that was
never `make`d. Both panic. Fix: close exactly once from the owner; if multiple
paths can trigger shutdown, funnel the close through a `sync.Once` or a guarded
`closed` flag.

### Closing from the receiver side or a non-owner producer

Wrong: a consumer closes the channel it reads from, or one producer among several
closes the shared channel. Another producer then sends after close and panics. Fix:
only the send-side owner closes; with multiple producers, a `WaitGroup` plus one
closing goroutine after `Wait()` is the correct topology.

### Receiving from a nil channel

Wrong: a struct field of type `chan T` is used without ever being initialized with
`make`, so it is nil; a receive on it blocks forever, and if it is the only
runnable path the whole program deadlocks. Fix: initialize every channel with
`make` before use. (Blocking on a nil channel is a real technique inside `select`
to disable a case, but that is a deliberate choice, not this bug.)

### Returning before draining on shutdown

Wrong: a shutdown path closes the input and immediately returns, or a consumer
`select`s on a quit signal and exits while values are still queued. The tail of
already-sent work is silently discarded. Fix: close the input, then receive until
the channel reports closed-and-empty (`for range` to completion, or comma-ok until
`ok` is false) before declaring shutdown done.

### Treating an unbuffered send as fire-and-forget

Wrong: assuming `ch <- v` on an unbuffered channel returns immediately. It blocks
until a receiver takes the value; if no goroutine is receiving, the sender
deadlocks. Fix: make sure a receiver exists — start the workers or the consumer
*before* flooding sends — or choose the right channel topology for the work.

### Appending fan-out results and expecting input order

Wrong: collecting worker results by appending them to a slice and assuming
`out[i]` corresponds to `input[i]`. Arrival order is nondeterministic. Fix: carry
an index with each value and write to a pre-sized slice at that index, or sort the
results by a stable key afterward.

### Misusing a WaitGroup around channels

Wrong: calling `wg.Add` inside the worker goroutine (it races the `wg.Wait` in the
closer) or forgetting to close the results channel after `Wait()`, so the
collector's range never ends. Fix: call `wg.Add` before launching each worker, and
close the output in a separate goroutine after `wg.Wait()`.

Next: [01-worker-pool-fan-out-fan-in.md](01-worker-pool-fan-out-fan-in.md)
